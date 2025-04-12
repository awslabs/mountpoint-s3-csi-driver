package mounter

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"k8s.io/mount-utils"

	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/envprovider"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/targetpath"
	"github.com/awslabs/aws-s3-csi-driver/pkg/mountpoint"
	"github.com/awslabs/aws-s3-csi-driver/pkg/podmounter/mountoptions"
	"github.com/awslabs/aws-s3-csi-driver/pkg/podmounter/mppod"
	"github.com/awslabs/aws-s3-csi-driver/pkg/podmounter/mppod/watcher"
	"github.com/awslabs/aws-s3-csi-driver/pkg/util"
)

const mountpointPodReadinessTimeout = 10 * time.Second
const mountpointPodReadinessCheckInterval = 100 * time.Millisecond

// targetDirPerm is the permission to use while creating target directory if its not exists.
const targetDirPerm = fs.FileMode(0755)

// mountSyscall is the function that performs `mount` operation for given `target` with given Mountpoint `args`.
// It returns mounted FUSE file descriptor as a result.
// This is mainly exposed for testing, in production platform-native function (`mountSyscallDefault`) will be used.
type mountSyscall func(target string, args mountpoint.Args) (fd int, err error)

// A PodMounter is a [Mounter] that mounts Mountpoint on pre-created Kubernetes Pod running in the same node.
type PodMounter struct {
	podWatcher        *watcher.Watcher
	mount             mount.Interface
	kubeletPath       string
	mountSyscall      mountSyscall
	kubernetesVersion string
	credProvider      *credentialprovider.Provider
}

// NewPodMounter creates a new [PodMounter] with given Kubernetes client.
func NewPodMounter(podWatcher *watcher.Watcher, credProvider *credentialprovider.Provider, mount mount.Interface, mountSyscall mountSyscall, kubernetesVersion string) (*PodMounter, error) {
	return &PodMounter{
		podWatcher:        podWatcher,
		credProvider:      credProvider,
		mount:             mount,
		kubeletPath:       util.KubeletPath(),
		mountSyscall:      mountSyscall,
		kubernetesVersion: kubernetesVersion,
	}, nil
}

// Mount mounts the given `bucketName` at the `target` path using provided credential context and Mountpoint arguments.
//
// At high level, this method will:
//  1. Wait for Mountpoint Pod to be `Running`
//  2. Write credentials to Mountpoint Pod's credentials directory
//  3. Obtain a FUSE file descriptor
//  4. Call `mount` syscall with `target` and obtained FUSE file descriptor
//  5. Send mount options (including FUSE file descriptor) to Mountpoint Pod
//  6. Wait until Mountpoint successfully mounts at `target`
//
// If Mountpoint is already mounted at `target`, it will return early at step 2 to ensure credentials are up-to-date.
func (pm *PodMounter) Mount(ctx context.Context, bucketName string, target string, credentialCtx credentialprovider.ProvideContext, args mountpoint.Args) error {
	volumeName, err := pm.volumeNameFromTargetPath(target)
	if err != nil {
		return fmt.Errorf("Failed to extract volume name from %q: %w", target, err)
	}

	podID := credentialCtx.PodID

	err = pm.verifyOrSetupMountTarget(target)
	if err != nil {
		return fmt.Errorf("Failed to verify target path can be used as a mount point %q: %w", target, err)
	}

	isMountPoint, err := pm.IsMountPoint(target)
	if err != nil {
		return fmt.Errorf("Could not check if %q is already a mount point: %w", target, err)
	}

	// TODO: If `target` is a `systemd`-mounted Mountpoint, this would return an error,
	// but we should still update the credentials for it by calling `credProvider.Provide`.
	pod, podPath, err := pm.waitForMountpointPod(ctx, podID, volumeName)
	if err != nil {
		klog.Errorf("Failed to wait for Mountpoint Pod to be ready for %q: %v", target, err)
		return fmt.Errorf("Failed to wait for Mountpoint Pod to be ready for %q: %w", target, err)
	}

	podCredentialsPath, err := pm.ensureCredentialsDirExists(podPath)
	if err != nil {
		klog.Errorf("Failed to create credentials directory for %q: %v", target, err)
		return fmt.Errorf("Failed to create credentials directory for %q: %w", target, err)
	}

	credentialCtx.SetWriteAndEnvPath(podCredentialsPath, mppod.PathInsideMountpointPod(mppod.KnownPathCredentials))

	// Note that this part happens before `isMountPoint` check, as we want to update credentials even though
	// there is an existing mount point at `target`.
	credEnv, authenticationSource, err := pm.credProvider.Provide(ctx, credentialCtx)
	if err != nil {
		klog.Errorf("Failed to provide credentials for %s: %v\n%s", target, err, pm.helpMessageForGettingMountpointLogs(pod))
		return fmt.Errorf("Failed to provide credentials for %q: %w\n%s", target, err, pm.helpMessageForGettingMountpointLogs(pod))
	}

	if isMountPoint {
		klog.V(4).Infof("Target path %q is already mounted", target)
		return nil
	}

	env := envprovider.Default()
	env.Merge(credEnv)

	// Move `--aws-max-attempts` to env if provided
	if maxAttempts, ok := args.Remove(mountpoint.ArgAWSMaxAttempts); ok {
		env.Set(envprovider.EnvMaxAttempts, maxAttempts)
	}

	args.Set(mountpoint.ArgUserAgentPrefix, UserAgent(authenticationSource, pm.kubernetesVersion))

	podMountSockPath := mppod.PathOnHost(podPath, mppod.KnownPathMountSock)
	podMountErrorPath := mppod.PathOnHost(podPath, mppod.KnownPathMountError)

	klog.V(4).Infof("Mounting %s for %s", target, pod.Name)

	fuseDeviceFD, err := pm.mountSyscallWithDefault(target, args)
	if err != nil {
		klog.Errorf("Failed to mount %s: %v", target, err)
		return fmt.Errorf("Failed to mount %s: %w", target, err)
	}

	// Remove the read-only argument from the list as mount-s3 does not support it when using FUSE
	// file descriptor (we already pass MS_RDONLY flag during mount syscall in `pod_mounter_linux.go`)
	if args.Has(mountpoint.ArgReadOnly) {
		args.Remove(mountpoint.ArgReadOnly)
	}

	// This will set to false in the success condition. This is set to `true` by default to
	// ensure we don't leave `target` mounted if Mountpoint is not started to serve requests for it.
	unmount := true
	defer func() {
		if unmount {
			if err := pm.unmountTarget(target); err != nil {
				klog.V(4).ErrorS(err, "Failed to unmount mounted target %s\n", target)
			} else {
				klog.V(4).Infof("Target %s unmounted successfully\n", target)
			}
		}
	}()

	// This function can either fail or successfully send mount options to Mountpoint Pod - in which
	// Mountpoint Pod will get its own fd referencing the same underlying file description.
	// In both case we need to close the fd in this process.
	defer pm.closeFUSEDevFD(fuseDeviceFD)

	// Remove old mount error file if exists
	_ = os.Remove(podMountErrorPath)

	klog.V(4).Infof("Sending mount options to Mountpoint Pod %s on %s", pod.Name, podMountSockPath)

	err = mountoptions.Send(ctx, podMountSockPath, mountoptions.Options{
		Fd:         fuseDeviceFD,
		BucketName: bucketName,
		Args:       args.SortedList(),
		Env:        env.List(),
	})
	if err != nil {
		klog.Errorf("Failed to send mount option to Mountpoint Pod %s for %s: %v\n%s", pod.Name, target, err, pm.helpMessageForGettingMountpointLogs(pod))
		return fmt.Errorf("Failed to send mount options to Mountpoint Pod %s for %s: %w\n%s", pod.Name, target, err, pm.helpMessageForGettingMountpointLogs(pod))
	}

	err = pm.waitForMount(ctx, target, pod.Name, podMountErrorPath)
	if err != nil {
		klog.Errorf("Failed to wait for Mountpoint Pod %s to be ready for %s: %v\n%s", pod.Name, target, err, pm.helpMessageForGettingMountpointLogs(pod))
		return fmt.Errorf("Failed to wait for Mountpoint Pod %s to be ready for %s: %w\n%s", pod.Name, target, err, pm.helpMessageForGettingMountpointLogs(pod))
	}

	// Mountpoint successfully started, so don't unmount the filesystem
	unmount = false
	return nil
}

// Unmount unmounts the mount point at `target` and cleans all credentials.
func (pm *PodMounter) Unmount(ctx context.Context, target string, credentialCtx credentialprovider.CleanupContext) error {
	volumeName, err := pm.volumeNameFromTargetPath(target)
	if err != nil {
		return fmt.Errorf("Failed to extract volume name from %q: %w", target, err)
	}

	podID := credentialCtx.PodID

	// TODO: If `target` is a `systemd`-mounted Mountpoint, this would return an error,
	// but we should still unmount it and clean the credentials.
	pod, podPath, err := pm.waitForMountpointPod(ctx, podID, volumeName)
	if err != nil {
		klog.Errorf("Failed to wait for Mountpoint Pod to be ready for %q: %v", target, err)
		return fmt.Errorf("Failed to wait for Mountpoint Pod for %q: %w", target, err)
	}

	credentialCtx.WritePath = pm.credentialsDir(podPath)

	// Write `mount.exit` file to indicate Mountpoint Pod to cleanly exit.
	podMountExitPath := mppod.PathOnHost(podPath, mppod.KnownPathMountExit)
	_, err = os.OpenFile(podMountExitPath, os.O_RDONLY|os.O_CREATE, credentialprovider.CredentialFilePerm)
	if err != nil {
		klog.Errorf("Failed to send a exit message to Mountpoint Pod for %q: %s\n%s", target, err, pm.helpMessageForGettingMountpointLogs(pod))
		return fmt.Errorf("Failed to send a exit message to Mountpoint Pod for %q: %w\n%s", target, err, pm.helpMessageForGettingMountpointLogs(pod))
	}

	err = pm.unmountTarget(target)
	if err != nil {
		klog.Errorf("Failed to unmount %q: %v", target, err)
		return fmt.Errorf("Failed to unmount %q: %w", target, err)
	}

	err = pm.credProvider.Cleanup(credentialCtx)
	if err != nil {
		klog.Errorf("Failed to clean up credentials for %s: %v\n%s", target, err, pm.helpMessageForGettingMountpointLogs(pod))
		return fmt.Errorf("Failed to clean up credentials for %q: %w\n%s", target, err, pm.helpMessageForGettingMountpointLogs(pod))
	}

	return nil
}

// IsMountPoint returns whether given `target` is a `mount-s3` mount.
func (pm *PodMounter) IsMountPoint(target string) (bool, error) {
	// TODO: Can we just use regular `IsMountPoint` check from `mounter` with containerization?
	return isMountPoint(pm.mount, target)
}

// waitForMountpointPod waints until Mountpoint Pod for given `podID` and `volumeName` is in `Running` state.
// It returns found Mountpoint Pod and it's base directory.
func (pm *PodMounter) waitForMountpointPod(ctx context.Context, podID, volumeName string) (*corev1.Pod, string, error) {
	podName := mppod.MountpointPodNameFor(podID, volumeName)

	pod, err := pm.podWatcher.Wait(ctx, podName)
	if err != nil {
		return nil, "", err
	}

	klog.V(4).Infof("Mountpoint Pod %s/%s is running with id %s", pod.Namespace, podName, pod.UID)

	return pod, pm.podPath(pod), nil
}

// waitForMount waits until Mountpoint is successfully mounted at `target`.
// It returns an error if Mountpoint fails to mount.
func (pm *PodMounter) waitForMount(parentCtx context.Context, target, podName, podMountErrorPath string) error {
	ctx, cancel := context.WithCancel(parentCtx)
	// Cancel at the end to ensure we cancel polling from goroutines.
	defer cancel()

	mountResultCh := make(chan error)

	klog.V(4).Infof("Waiting until Mountpoint Pod %s mounts on %s", podName, target)

	// Poll for mount error file
	go func() {
		wait.PollUntilContextCancel(ctx, 1*time.Second, true, func(ctx context.Context) (done bool, err error) {
			res, err := os.ReadFile(podMountErrorPath)
			if err != nil {
				return false, nil
			}

			mountResultCh <- fmt.Errorf("Mountpoint Pod %s failed: %s", podName, res)
			return true, nil
		})
	}()

	// Poll for `IsMountPoint` check
	go func() {
		err := wait.PollUntilContextCancel(ctx, 1*time.Second, true, func(ctx context.Context) (done bool, err error) {
			return pm.IsMountPoint(target)
		})

		if err != nil {
			mountResultCh <- fmt.Errorf("Failed to check if Mountpoint Pod %s mounted: %w", podName, err)
		} else {
			mountResultCh <- nil
		}
	}()

	err := <-mountResultCh
	if err == nil {
		klog.V(4).Infof("Mountpoint Pod %s mounted on %s", podName, target)
	} else {
		klog.V(4).Infof("Mountpoint Pod %s failed to mount on %s: %v", podName, target, err)
	}

	return err
}

// closeFUSEDevFD closes given FUSE file descriptor.
func (pm *PodMounter) closeFUSEDevFD(fd int) {
	err := syscall.Close(fd)
	if err != nil {
		klog.V(4).Infof("Mount: Failed to close /dev/fuse file descriptor %d: %v\n", fd, err)
	}
}

// verifyOrSetupMountTarget checks target path for existence and corrupted mount error.
// If the target dir does not exists it tries to create it.
// If the target dir is corrupted (decided with `mount.IsCorruptedMnt`) it tries to unmount it to have a clean mount.
func (pm *PodMounter) verifyOrSetupMountTarget(target string) error {
	err := verifyMountPointStatx(target)
	if err == nil {
		return nil
	}

	if errors.Is(err, fs.ErrNotExist) {
		klog.V(5).Infof("Target path does not exists %s, trying to create", target)
		if err := os.MkdirAll(target, targetDirPerm); err != nil {
			return fmt.Errorf("Failed to create target directory: %w", err)
		}

		return nil
	} else if mount.IsCorruptedMnt(err) {
		klog.V(4).Infof("Target path %q is a corrupted mount. Trying to unmount", target)
		if unmountErr := pm.unmountTarget(target); unmountErr != nil {
			klog.V(4).Infof("Failed to unmount target path %q: %v, original failure of stat: %v", target, unmountErr, err)
			return fmt.Errorf("Failed to unmount target path %q: %w, original failure of stat: %v", target, unmountErr, err)
		}

		return nil
	}

	return err
}

// ensureCredentialsDirExists ensures credentials dir for `podPath` is exists.
// It returns credentials dir and any error.
func (pm *PodMounter) ensureCredentialsDirExists(podPath string) (string, error) {
	credentialsBasepath := pm.credentialsDir(podPath)
	err := os.Mkdir(credentialsBasepath, credentialprovider.CredentialDirPerm)
	if err != nil && !errors.Is(err, fs.ErrExist) {
		klog.V(4).Infof("Failed to create credentials directory for pod %s: %v", podPath, err)
		return "", err
	}

	return credentialsBasepath, nil
}

// credentialsDir returns credentials dir for `podPath`.
func (pm *PodMounter) credentialsDir(podPath string) string {
	return mppod.PathOnHost(podPath, mppod.KnownPathCredentials)
}

// podPath returns `pod`'s basepath inside kubelet's path.
func (pm *PodMounter) podPath(pod *corev1.Pod) string {
	return filepath.Join(pm.kubeletPath, "pods", string(pod.UID))
}

// mountSyscallWithDefault delegates to `mountSyscall` if set, or fallbacks to platform-native `mountSyscallDefault`.
func (pm *PodMounter) mountSyscallWithDefault(target string, args mountpoint.Args) (int, error) {
	if pm.mountSyscall != nil {
		return pm.mountSyscall(target, args)
	}

	return pm.mountSyscallDefault(target, args)
}

// unmountTarget calls `unmount` syscall on `target`.
func (pm *PodMounter) unmountTarget(target string) error {
	return pm.mount.Unmount(target)
}

// volumeNameFromTargetPath tries to extract PersistentVolume's name from `target` path.
func (pm *PodMounter) volumeNameFromTargetPath(target string) (string, error) {
	tp, err := targetpath.Parse(target)
	if err != nil {
		return "", err
	}
	return tp.VolumeID, nil
}

func (pm *PodMounter) helpMessageForGettingMountpointLogs(pod *corev1.Pod) string {
	return fmt.Sprintf("You can see Mountpoint logs by running: `kubectl logs -n %s %s`. If the Mountpoint Pod already restarted, you can also pass `--previous` to get logs from the previous run.", pod.Namespace, pod.Name)
}
