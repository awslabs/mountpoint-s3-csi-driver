package mounter

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/envprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
	mpmounter "github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mounter"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mountoptions"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util"
)

const (
	mounterPodLabel  = "app=aws-s3-csi-daemonset-mounter"
	commVolumeName   = "comm"
	mountSockName    = "mount.sock"
	mountErrorSuffix = ".error"

	daemonsetMountReadyTimeout = 15 * time.Second
	daemonsetMountPollInterval = 500 * time.Millisecond
)

// DaemonsetMounter is a [Mounter] that delegates Mountpoint process management
// to a secondary daemonset running on the same node. It communicates via the
// secondary pod's emptyDir volume, accessed through the kubelet pod directory.
type DaemonsetMounter struct {
	clientset   kubernetes.Interface
	nodeID      string
	kubeletPath string
	mount       *mpmounter.Mounter

	// Cached comm dir path (discovered from secondary pod UID).
	mu      sync.Mutex
	commDir string
}

// NewDaemonsetMounter creates a new [DaemonsetMounter].
func NewDaemonsetMounter(clientset kubernetes.Interface, nodeID string) *DaemonsetMounter {
	return &DaemonsetMounter{
		clientset:   clientset,
		nodeID:      nodeID,
		kubeletPath: util.ContainerKubeletPath(),
		mount:       mpmounter.New(),
	}
}

// Mount mounts the given S3 bucket at the target path.
//
// It performs the following steps:
//  1. Opens /dev/fuse and performs the kernel FUSE mount on target
//  2. Sends mount options (including FUSE FD) to the secondary daemonset via UDS
//  3. Waits for Mountpoint to start serving (or an error)
func (dm *DaemonsetMounter) Mount(ctx context.Context, bucketName string, target string,
	credentialCtx credentialprovider.ProvideContext, args mountpoint.Args, fsGroup string, userEnv envprovider.Environment) error {

	// Mount identifier must be unique per mount (not per volume), since multiple pods
	// can mount the same volume. Use workload pod UID to distinguish.
	// TODO: change for pod/volume sharing
	podUID := credentialCtx.WorkloadPodID
	volumeId := credentialCtx.VolumeID
	mountId := podUID + "-" + volumeId

	// Idempotency: if target is already a healthy Mountpoint mount, return early.
	// Kubelet may call NodePublishVolume repeatedly (requiresRepublish, retries).
	isMounted, err := dm.IsMountPoint(target)
	if err != nil {
		return fmt.Errorf("failed to check if target %q is a mount point: %w", target, err)
	}
	if isMounted {
		klog.V(4).Infof("DaemonsetMounter: target %s is already mounted, nothing to do", target)
		return nil
	}

	commDir, err := dm.getCommDir(ctx)
	if err != nil {
		return fmt.Errorf("failed to discover mounter pod comm dir: %w", err)
	}

	// Ensure target directory exists (kubelet may not have created it yet)
	if err := os.MkdirAll(target, targetDirPerm); err != nil {
		return fmt.Errorf("failed to create target directory %q: %w", target, err)
	}

	// Step 1: Perform kernel FUSE mount on target
	mountOpts := mpmounter.MountOptions{
		ReadOnly:   args.Has(mountpoint.ArgReadOnly),
		AllowOther: args.Has(mountpoint.ArgAllowOther) || args.Has(mountpoint.ArgAllowRoot),
	}
	fd, err := dm.mount.Mount(target, mountOpts)
	if err != nil {
		return fmt.Errorf("failed to mount FUSE at %q: %w", target, err)
	}

	// Ensure FD is closed and mount is cleaned up on failure
	fdClosed := false
	unmount := true
	defer func() {
		if !fdClosed {
			dm.closeFUSEDevFD(fd)
		}
		if unmount {
			if umErr := dm.mount.Unmount(target); umErr != nil {
				klog.Errorf("Failed to unmount %q during cleanup: %v", target, umErr)
			}
		}
	}()

	// Remove read-only from args since we already passed MS_RDONLY in mount syscall
	args.Remove(mountpoint.ArgReadOnly)

	// TODO: add --user-agent-prefix for S3 request telemetry (needs kubernetesVersion/variant fields)

	// Build environment
	// TODO: (pod-level creds) set envs and write tokens
	// TODO: should we inherit envs from the driver process? or from the mounter? should they overwrite userEnv?
	env := envprovider.Environment{}
	env.Merge(userEnv)
	env.Merge(envprovider.Default())

	// Move --aws-max-attempts to env if provided
	if maxAttempts, ok := args.Remove(mountpoint.ArgAWSMaxAttempts); ok {
		env.Set(envprovider.EnvMaxAttempts, maxAttempts)
	}

	// Step 2: Send mount options to secondary daemonset
	sockPath := filepath.Join(commDir, mountSockName)
	errFilePath := filepath.Join(commDir, mountId+mountErrorSuffix)

	// Remove old error file if exists
	os.Remove(errFilePath)

	klog.V(4).Infof("DaemonsetMounter: sending mount options for volume %s (mount %s) to %s", volumeId, mountId, sockPath)

	sendCtx, sendCancel := context.WithTimeout(ctx, daemonsetMountReadyTimeout)
	defer sendCancel()

	err = mountoptions.Send(sendCtx, sockPath, mountoptions.Options{
		Fd:         fd,
		BucketName: bucketName,
		Args:       args.SortedList(),
		Env:        env.List(),
		VolumeId:   mountId,
	})
	if err != nil {
		// If send failed due to stale path, invalidate cache, and let Kubelet
		// retry NodePublishVolume.
		if errors.Is(err, fs.ErrNotExist) || os.IsPermission(err) {
			klog.V(4).Infof("DaemonsetMounter: comm dir may be stale, invalidating cache")
			dm.invalidateCommDir()
		}
		return fmt.Errorf("failed to send mount options for volume %s (mount %s): %w", volumeId, mountId, err)
	}

	// Close the FD in this process — the secondary daemonset now holds it
	dm.closeFUSEDevFD(fd)
	fdClosed = true

	// Step 3: Wait for mount readiness or error
	err = dm.waitForMount(ctx, target, mountId, errFilePath)
	if err != nil {
		return err
	}

	unmount = false
	klog.V(4).Infof("DaemonsetMounter: volume %s (mount %s) mounted at %s", volumeId, mountId, target)
	return nil
}

// Unmount unmounts the FUSE filesystem at target.
// This causes the Mountpoint process in the secondary daemonset to exit.
func (dm *DaemonsetMounter) Unmount(ctx context.Context, target string, credentialCtx credentialprovider.CleanupContext) error {
	err := dm.mount.Unmount(target)
	if err != nil {
		return fmt.Errorf("failed to unmount %q: %w", target, err)
	}

	// Attempt to clean up stale error files (if Mountpoint crashed after a successful mount)
	dm.mu.Lock()
	commDir := dm.commDir
	dm.mu.Unlock()
	if commDir != "" {
		mountId := credentialCtx.PodID + "-" + credentialCtx.VolumeID
		os.Remove(filepath.Join(commDir, mountId+mountErrorSuffix))
	}

	klog.V(4).Infof("DaemonsetMounter: volume %s unmounted from %s", credentialCtx.VolumeID, target)
	return nil
}

// IsMountPoint returns whether the given target is a Mountpoint mount.
func (dm *DaemonsetMounter) IsMountPoint(target string) (bool, error) {
	return dm.mount.CheckMountpoint(target)
}

// TODO: refactor closeFUSEDevFD into a shared helper (duplicated in pod_mounter.go)
func (dm *DaemonsetMounter) closeFUSEDevFD(fd int) {
	if err := mpmounter.CloseFD(fd); err != nil {
		klog.V(4).Infof("DaemonsetMounter: failed to close /dev/fuse fd %d: %v", fd, err)
	}
}

// WarmCommDir attempts to discover and cache the comm dir path at startup.
// It is non-blocking and intended for pre-warming so the first mount is fast.
func (dm *DaemonsetMounter) WarmCommDir(ctx context.Context) {
	if _, err := dm.getCommDir(ctx); err != nil {
		klog.V(2).Infof("DaemonsetMounter: mounter pod not yet available at startup: %v", err)
	}
}

// getCommDir returns the cached comm dir path, discovering it if needed.
// If the cached path is stale (socket no longer exists), it re-discovers.
func (dm *DaemonsetMounter) getCommDir(ctx context.Context) (string, error) {
	dm.mu.Lock()
	defer dm.mu.Unlock()

	if dm.commDir != "" {
		sockPath := filepath.Join(dm.commDir, mountSockName)
		if _, err := os.Stat(sockPath); err == nil {
			return dm.commDir, nil
		}
		klog.V(4).Infof("DaemonsetMounter: cached comm dir is stale, re-discovering")
		dm.commDir = ""
	}

	commDir, err := dm.discoverCommDir(ctx)
	if err != nil {
		return "", err
	}
	dm.commDir = commDir
	return commDir, nil
}

// invalidateCommDir clears the cached comm dir so the next call re-discovers it.
func (dm *DaemonsetMounter) invalidateCommDir() {
	dm.mu.Lock()
	defer dm.mu.Unlock()
	dm.commDir = ""
}

// discoverCommDir finds the secondary mounter pod on this node and returns the path
// to its emptyDir comm volume as seen from the primary daemonset (via kubelet pod dir).
// It retries until secondary daemonset mounter appears in case primary starts before secondary.
func (dm *DaemonsetMounter) discoverCommDir(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, daemonsetMountReadyTimeout)
	defer cancel()

	for {
		pods, err := dm.clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{
			LabelSelector: mounterPodLabel,
			FieldSelector: "spec.nodeName=" + dm.nodeID,
		})
		if err != nil {
			return "", fmt.Errorf("failed to list mounter pods on node %s: %w", dm.nodeID, err)
		}

		// Filter to running pods
		var running []corev1.Pod
		for _, pod := range pods.Items {
			if pod.Status.Phase == corev1.PodRunning {
				running = append(running, pod)
			}
		}

		if len(running) > 1 {
			return "", fmt.Errorf("multiple running mounter pods found on node %s (expected exactly 1, got %d)", dm.nodeID, len(running))
		}

		if len(running) == 1 {
			podUID := string(running[0].UID)
			// Path: <kubeletPath>/pods/<pod-uid>/volumes/kubernetes.io~empty-dir/comm/
			commDir := filepath.Join(dm.kubeletPath, "pods", podUID, "volumes", "kubernetes.io~empty-dir", commVolumeName)
			klog.V(4).Infof("DaemonsetMounter: discovered mounter pod %s (uid=%s), comm dir: %s", running[0].Name, podUID, commDir)
			return commDir, nil
		}

		// No running mounter pod yet — wait and retry
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("timed out waiting for running mounter pod on node %s: %w", dm.nodeID, ctx.Err())
		case <-time.After(daemonsetMountPollInterval):
		}
	}
}

// waitForMount waits until Mountpoint is serving at target or an error occurs.
func (dm *DaemonsetMounter) waitForMount(parentCtx context.Context, target, mountId, errFilePath string) error {
	ctx, cancel := context.WithTimeout(parentCtx, daemonsetMountReadyTimeout)
	defer cancel()

	mountResultCh := make(chan error, 2)

	// Poll for error file
	go func() {
		wait.PollUntilContextCancel(ctx, daemonsetMountPollInterval, true, func(ctx context.Context) (bool, error) {
			content, err := os.ReadFile(errFilePath)
			if err != nil {
				return false, nil
			}
			os.Remove(errFilePath)
			mountResultCh <- fmt.Errorf("Mountpoint for mount %s failed: %s", mountId, string(content))
			return true, nil
		})
	}()

	// Poll for mount readiness
	go func() {
		err := wait.PollUntilContextCancel(ctx, daemonsetMountPollInterval, true, func(ctx context.Context) (bool, error) {
			isMounted, _ := dm.mount.CheckMountpoint(target)
			return isMounted, nil
		})
		if err != nil {
			mountResultCh <- fmt.Errorf("timed out waiting for Mountpoint to serve mount %s at %s", mountId, target)
		} else {
			mountResultCh <- nil
		}
	}()

	return <-mountResultCh
}
