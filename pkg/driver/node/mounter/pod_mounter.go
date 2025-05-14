package mounter

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crdv1beta "github.com/awslabs/aws-s3-csi-driver/pkg/api/v1beta"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/envprovider"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/targetpath"
	"github.com/awslabs/aws-s3-csi-driver/pkg/mountpoint"
	mpmounter "github.com/awslabs/aws-s3-csi-driver/pkg/mountpoint/mounter"
	"github.com/awslabs/aws-s3-csi-driver/pkg/mountpoint/mountoptions"
	"github.com/awslabs/aws-s3-csi-driver/pkg/podmounter/mppod"
	"github.com/awslabs/aws-s3-csi-driver/pkg/podmounter/mppod/watcher"
	"github.com/awslabs/aws-s3-csi-driver/pkg/util"
)

// targetDirPerm is the permission to use while creating target directory if its not exists.
const targetDirPerm = fs.FileMode(0755)

// mountSyscall is the function that performs `mount` operation for given `target` with given Mountpoint `args`.
// It returns mounted FUSE file descriptor as a result.
// This is mainly exposed for testing, in production platform-native function (`mpmounter.Mount`) will be used.
type mountSyscall func(target string, args mountpoint.Args) (fd int, err error)
type bindMountSyscall func(source, target string) (err error)

// A PodMounter is a [Mounter] that mounts Mountpoint on pre-created Kubernetes Pod running in the same node.
type PodMounter struct {
	podWatcher        *watcher.Watcher
	s3paCache         cache.Cache
	mount             *mpmounter.Mounter
	kubeletPath       string
	mountSyscall      mountSyscall
	bindMountSyscall  bindMountSyscall
	kubernetesVersion string
	credProvider      *credentialprovider.Provider
	nodeID            string
}

// NewPodMounter creates a new [PodMounter] with given Kubernetes client.
func NewPodMounter(
	podWatcher *watcher.Watcher,
	s3paCache cache.Cache,
	credProvider *credentialprovider.Provider,
	mount *mpmounter.Mounter,
	mountSyscall mountSyscall,
	bindMountSyscall bindMountSyscall,
	kubernetesVersion,
	nodeID string,
) (*PodMounter, error) {
	return &PodMounter{
		podWatcher:        podWatcher,
		s3paCache:         s3paCache,
		credProvider:      credProvider,
		mount:             mount,
		kubeletPath:       util.KubeletPath(),
		mountSyscall:      mountSyscall,
		bindMountSyscall:  bindMountSyscall,
		kubernetesVersion: kubernetesVersion,
		nodeID:            nodeID,
	}, nil
}

// Mount mounts the given `bucketName` at the `target` path using provided credential context and Mountpoint arguments.
//
// At high level, this method will:
//  1. Find corresponding MountpointS3PodAttachment custom resource and Mountpoint Pod
//  2. Wait for Mountpoint Pod to be `Running`
//  3. Write credentials to Mountpoint Pod's credentials directory
//  4. Obtain a FUSE file descriptor
//  5. Call `mount` syscall with `source` and obtained FUSE file descriptor
//  6. Send mount options (including FUSE file descriptor) to Mountpoint Pod
//  7. Wait until Mountpoint successfully mounts at `source`
//  8. Bind mounts from `source` to `target`
//
// If Mountpoint is already mounted at `target`, it will return early at step 3 to ensure credentials are up-to-date.
// If Mountpoint is already mounted at `source`, it will skip steps 4-7 and only perform bind mount to `target`.
func (pm *PodMounter) Mount(ctx context.Context, bucketName string, target string, credentialCtx credentialprovider.ProvideContext, args mountpoint.Args, fsGroup string, pvMountOptions string) error {
	volumeName, err := pm.volumeNameFromTargetPath(target)
	if err != nil {
		return fmt.Errorf("Failed to extract volume name from %q: %w", target, err)
	}

	s3PodAttachment, mpPodName, err := pm.getS3PodAttachmentWithRetry(ctx, volumeName, credentialCtx, fsGroup, pvMountOptions)
	if err != nil {
		klog.Errorf("Failed to find corresponding MountpointS3PodAttachment custom resource %q: %v", target, err)
		return fmt.Errorf("Failed to find corresponding MountpointS3PodAttachment custom resource %q: %w", target, err)
	}

	// TODO: If `target` is a `systemd`-mounted Mountpoint, this would return an error,
	// but we should still update the credentials for it by calling `credProvider.Provide`.
	pod, podPath, err := pm.waitForMountpointPod(ctx, mpPodName)
	if err != nil {
		klog.Errorf("Failed to wait for Mountpoint Pod to be ready for %q: %v", target, err)
		return fmt.Errorf("Failed to wait for Mountpoint Pod to be ready for %q: %w", target, err)
	}
	source := filepath.Join(SourceMountDir(pm.kubeletPath), mpPodName)
	unlockMountpointPod := lockMountpointPod(mpPodName)
	defer unlockMountpointPod()

	isTargetMountPoint, err := pm.IsMountPoint(target)
	if err != nil {
		err = pm.verifyOrSetupMountTarget(target, err)
		if err != nil {
			return fmt.Errorf("Failed to verify target path can be used as a mount point %q: %w", target, err)
		}
	}
	isSourceMountPoint, err := pm.IsMountPoint(source)
	if err != nil {
		err = pm.verifyOrSetupMountTarget(source, err)
		if err != nil {
			return fmt.Errorf("Failed to verify source path can be used as a mount point %q: %w", source, err)
		}
	}

	// Note that this part happens before `isMountPoint` check, as we want to update credentials even though
	// there is an existing mount point at `target`.
	credEnv, authenticationSource, err := pm.provideCredentials(ctx, podPath, string(pod.UID), s3PodAttachment.Spec.WorkloadServiceAccountIAMRoleARN, credentialCtx)
	if err != nil {
		klog.Errorf("Failed to provide credentials for %s: %v\n%s", source, err, pm.helpMessageForGettingMountpointLogs(pod))
		return fmt.Errorf("Failed to provide credentials for %q: %w\n%s", source, err, pm.helpMessageForGettingMountpointLogs(pod))
	}

	if isTargetMountPoint {
		klog.V(4).Infof("Target path %q is already mounted. Only refreshed credentials.", target)
		return nil
	}

	if !isSourceMountPoint {
		err = pm.mountS3AtSource(ctx, source, pod, podPath, bucketName, credEnv, authenticationSource, args)
		if err != nil {
			return fmt.Errorf("Failed to mount at source %s: %v", source, err)
		}
	}

	err = pm.bindMountSyscallWithDefault(source, target)
	if err != nil {
		klog.Errorf("Failed to bind mount %s to target %s: %v", source, target, err)
		return fmt.Errorf("Failed to bind mount %s to target %s: %w", source, target, err)
	}

	return nil
}

// mountS3AtSource mounts an S3 bucket at the specified source path using the Mountpoint Pod.
//
// Parameters:
//   - ctx: Context for cancellation and timeout control
//   - source: The path where the S3 bucket should be mounted
//   - mpPod: Mountpoint Pod that will serve this mount point
//   - podPath: Base path for Pod-specific files
//   - bucketName: Name of the S3 bucket to mount
//   - credEnv: Environment variables related to AWS credentials
//   - authenticationSource: Authentication source from PV volume attribute
//   - args: Mountpoint arguments
//
// Returns:
//   - error: nil if successful, otherwise an error describing what went wrong
//
// The function performs the following steps:
//  1. Prepares environment and mount arguments
//  2. Performs the initial mount syscall to obtain FUSE file descriptor
//  3. Sends mount options to the Mountpoint Pod
//  4. Waits for the mount to be ready
//
// If any step fails, it ensures cleanup by unmounting the source path.
func (pm *PodMounter) mountS3AtSource(ctx context.Context, source string, mpPod *corev1.Pod, podPath string,
	bucketName string, credEnv envprovider.Environment, authenticationSource credentialprovider.AuthenticationSource,
	args mountpoint.Args) error {
	env := envprovider.Default()
	env.Merge(credEnv)

	// Move `--aws-max-attempts` to env if provided
	if maxAttempts, ok := args.Remove(mountpoint.ArgAWSMaxAttempts); ok {
		env.Set(envprovider.EnvMaxAttempts, maxAttempts)
	}

	args.Set(mountpoint.ArgUserAgentPrefix, UserAgent(authenticationSource, pm.kubernetesVersion))

	podMountSockPath := mppod.PathOnHost(podPath, mppod.KnownPathMountSock)
	podMountErrorPath := mppod.PathOnHost(podPath, mppod.KnownPathMountError)

	klog.V(4).Infof("Mounting %s for %s", source, mpPod.Name)

	fuseDeviceFD, err := pm.mountSyscallWithDefault(source, args)
	if err != nil {
		klog.Errorf("Failed to mount %s: %v", source, err)
		return fmt.Errorf("Failed to mount %s: %w", source, err)
	}

	// Remove the read-only argument from the list as mount-s3 does not support it when using FUSE
	// file descriptor (we already pass MS_RDONLY flag during mount syscall in `pod_mounter_linux.go`)
	if args.Has(mountpoint.ArgReadOnly) {
		args.Remove(mountpoint.ArgReadOnly)
	}

	// This will set to false in the success condition. This is set to `true` by default to
	// ensure we don't leave `source` mounted if Mountpoint is not started to serve requests for it.
	unmount := true
	defer func() {
		if unmount {
			if err := pm.unmountTarget(source); err != nil {
				klog.V(4).ErrorS(err, "Failed to unmount mounted source %s\n", source)
			} else {
				klog.V(4).Infof("Source %s unmounted successfully\n", source)
			}
		}
	}()

	// This function can either fail or successfully send mount options to Mountpoint Pod - in which
	// Mountpoint Pod will get its own fd referencing the same underlying file description.
	// In both case we need to close the fd in this process.
	defer pm.closeFUSEDevFD(fuseDeviceFD)

	// Remove old mount error file if exists
	_ = os.Remove(podMountErrorPath)

	klog.V(4).Infof("Sending mount options to Mountpoint Pod %s on %s", mpPod.Name, podMountSockPath)

	err = mountoptions.Send(ctx, podMountSockPath, mountoptions.Options{
		Fd:         fuseDeviceFD,
		BucketName: bucketName,
		Args:       args.SortedList(),
		Env:        env.List(),
	})
	if err != nil {
		klog.Errorf("Failed to send mount option to Mountpoint Pod %s for %s: %v\n%s", mpPod.Name, source, err, pm.helpMessageForGettingMountpointLogs(mpPod))
		return fmt.Errorf("Failed to send mount options to Mountpoint Pod %s for %s: %w\n%s", mpPod.Name, source, err, pm.helpMessageForGettingMountpointLogs(mpPod))
	}

	err = pm.waitForMount(ctx, source, mpPod.Name, podMountErrorPath)
	if err != nil {
		klog.Errorf("Failed to wait for Mountpoint Pod %s to be ready for %s: %v\n%s", mpPod.Name, source, err, pm.helpMessageForGettingMountpointLogs(mpPod))
		return fmt.Errorf("Failed to wait for Mountpoint Pod %s to be ready for %s: %w\n%s", mpPod.Name, source, err, pm.helpMessageForGettingMountpointLogs(mpPod))
	}

	// Mountpoint successfully started, so don't unmount the filesystem
	unmount = false
	return nil
}

// Unmount unmounts only the bind mount point at `target`.
func (pm *PodMounter) Unmount(_ context.Context, target string, _ credentialprovider.CleanupContext) error {
	err := pm.unmountTarget(target)
	if err != nil {
		klog.Errorf("Failed to unmount %q: %v", target, err)
		return fmt.Errorf("Failed to unmount %q: %w", target, err)
	}
	return nil
}

// IsMountPoint returns whether given `target` is a `mount-s3` mount.
func (pm *PodMounter) IsMountPoint(target string) (bool, error) {
	return pm.mount.CheckMountpoint(target)
}

// waitForMountpointPod waits until Mountpoint Pod for given `podName` is in `Running` state.
// It returns found Mountpoint Pod and it's base directory.
func (pm *PodMounter) waitForMountpointPod(ctx context.Context, podName string) (*corev1.Pod, string, error) {
	pod, err := pm.podWatcher.Wait(ctx, podName)
	if err != nil {
		return nil, "", err
	}

	klog.V(4).Infof("Mountpoint Pod %s/%s is running with id %s", pod.Namespace, podName, pod.UID)

	return pod, pm.podPath(string(pod.UID)), nil
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
	err := mpmounter.CloseFD(fd)
	if err != nil {
		klog.V(4).Infof("Mount: Failed to close /dev/fuse file descriptor %d: %v\n", fd, err)
	}
}

// verifyOrSetupMountTarget checks target path for existence and corrupted mount error.
// If the target dir does not exists it tries to create it.
// If the target dir is corrupted (decided with `mount.IsCorruptedMnt`) it tries to unmount it to have a clean mount.
func (pm *PodMounter) verifyOrSetupMountTarget(target string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		klog.V(5).Infof("Target path does not exists %s, trying to create", target)
		if err := os.MkdirAll(target, targetDirPerm); err != nil {
			return fmt.Errorf("Failed to create target directory: %w", err)
		}

		return nil
	} else if pm.mount.IsMountpointCorrupted(err) {
		klog.V(4).Infof("Target path %q is a corrupted mount. Trying to unmount", target)
		if unmountErr := pm.unmountTarget(target); unmountErr != nil {
			klog.V(4).Infof("Failed to unmount target path %q: %v, original failure of stat: %v", target, unmountErr, err)
			return fmt.Errorf("Failed to unmount target path %q: %w, original failure of stat: %v", target, unmountErr, err)
		}

		return nil
	}

	// Some other error that we cannot recover from, just propagate it.
	return err
}

// provideCredentials provides credentials
func (pm *PodMounter) provideCredentials(ctx context.Context, podPath, mpPodUID, serviceAccountEKSRoleARN string,
	credentialCtx credentialprovider.ProvideContext) (envprovider.Environment, credentialprovider.AuthenticationSource, error) {
	podCredentialsPath, err := pm.ensureCredentialsDirExists(podPath)
	if err != nil {
		return nil, "", fmt.Errorf("Failed to create credentials directory: %w", err)
	}

	credentialCtx.SetWriteAndEnvPath(podCredentialsPath, mppod.PathInsideMountpointPod(mppod.KnownPathCredentials))
	credentialCtx.SetServiceAccountEKSRoleARN(serviceAccountEKSRoleARN)
	credentialCtx.SetMountpointPodID(mpPodUID)

	return pm.credProvider.Provide(ctx, credentialCtx)
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
func (pm *PodMounter) podPath(podUID string) string {
	return filepath.Join(pm.kubeletPath, "pods", podUID)
}

// mountSyscallWithDefault delegates to `mountSyscall` if set, or fallbacks to platform-native `mpmounter.Mount`.
func (pm *PodMounter) mountSyscallWithDefault(target string, args mountpoint.Args) (int, error) {
	if pm.mountSyscall != nil {
		return pm.mountSyscall(target, args)
	}

	opts := mpmounter.MountOptions{
		ReadOnly:   args.Has(mountpoint.ArgReadOnly),
		AllowOther: args.Has(mountpoint.ArgAllowOther) || args.Has(mountpoint.ArgAllowRoot),
	}
	return pm.mount.Mount(target, opts)
}

// bindMountWithDefault delegates to `bindMountSyscall` if set, or fallbacks to platform-native `mpmounter.BindMount`.
func (pm *PodMounter) bindMountSyscallWithDefault(source, target string) error {
	if pm.bindMountSyscall != nil {
		return pm.bindMountSyscall(source, target)
	}

	return pm.mount.BindMount(source, target)
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

// getS3PodAttachmentWithRetry retrieves a MountpointS3PodAttachment resource that matches the given volume and credential context.
// It continuously retries the operation until either a matching attachment is found or the context is canceled.
func (pm *PodMounter) getS3PodAttachmentWithRetry(ctx context.Context, volumeName string, credentialCtx credentialprovider.ProvideContext, fsGroup, pvMountOptions string) (*crdv1beta.MountpointS3PodAttachment, string, error) {
	fieldFilters := client.MatchingFields{
		crdv1beta.FieldNodeName:             pm.nodeID,
		crdv1beta.FieldPersistentVolumeName: volumeName,
		crdv1beta.FieldVolumeID:             credentialCtx.VolumeID,
		crdv1beta.FieldMountOptions:         pvMountOptions,
		crdv1beta.FieldWorkloadFSGroup:      fsGroup,
		crdv1beta.FieldAuthenticationSource: credentialCtx.AuthenticationSource,
	}
	if credentialCtx.AuthenticationSource == credentialprovider.AuthenticationSourcePod {
		fieldFilters[crdv1beta.FieldWorkloadNamespace] = credentialCtx.PodNamespace
		fieldFilters[crdv1beta.FieldWorkloadServiceAccountName] = credentialCtx.ServiceAccountName
		// Note that we intentionally do not include `FieldWorkloadServiceAccountIAMRoleARN` to list filters because
		// CSI Driver Node does not know which role ARN to use (if any).
		// Role ARN is determined by reconciler and passed to node via MountpointS3PodAttachment.
	}

	for {
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		default:
		}

		s3paList := &crdv1beta.MountpointS3PodAttachmentList{}
		err := pm.s3paCache.List(ctx, s3paList, fieldFilters)
		if err != nil {
			klog.Errorf("Failed to list MountpointS3PodAttachments: %v", err)
			return nil, "", err
		}
		for _, s3pa := range s3paList.Items {
			for mpPodName, attachments := range s3pa.Spec.MountpointS3PodAttachments {
				for _, attachment := range attachments {
					if attachment.WorkloadPodUID == credentialCtx.WorkloadPodID {
						return &s3pa, mpPodName, nil
					}
				}
			}
		}

		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
