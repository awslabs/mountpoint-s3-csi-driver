// DaemonsetMounter implements Mounter for the two-daemonset architecture.
// The primary performs FUSE mounts and hands FDs to the secondary via Unix socket.
//
// Startup (driver.go):
//
//	DiscoverCommDir -> retries tryDiscoverCommDir until secondary pod found
//	StartCommDirWatch -> background goroutine calling checkCommDir every 5s
//
// Mount:
//
//	IsMountPoint -> GetCommDir -> Mount (FUSE) -> Send -> waitForMount
//	Stale path? -> store nil, signal rediscoverCh, return error
//
// Background (StartCommDirWatch -> checkCommDir):
//
//	stat(socket) -> healthy? return : tryDiscoverCommDir
//
// Comm dir: <kubeletPath>/pods/<uid>/volumes/kubernetes.io~empty-dir/comm/
package mounter

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
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
	mounterPodLabel  = "app=s3-csi-daemonset-mounter"
	CommVolumeName   = "comm"
	MountSockName    = "mount.sock"
	MountErrorSuffix = ".error"

	// TODO: lower sendOptionsTimeout once secondary has concurrent accept to reduce blocks on Mount -> Send -> dialWithRetry
	sendOptionsTimeout = 15 * time.Second

	mountReadyTimeout      = 15 * time.Second
	mountReadyPollInterval = 500 * time.Millisecond

	commDirCheckInterval      = 5 * time.Second
	commDirDiscoveryTimeout   = 60 * time.Second
	commDirRediscoveryTimeout = 15 * time.Second
)

var mounterNamespace = os.Getenv("MOUNTER_NAMESPACE")

// Exported for error matching in tests and NodePublishVolume callers.
var (
	ErrCommDirNotReady        = errors.New("comm dir not yet discovered or stale")
	ErrCommDirDiscoveryFailed = errors.New("comm dir discovery failed")
	ErrMultipleMounterPods    = errors.New("multiple running mounter pods found")
	ErrNoRunningMounterPod    = errors.New("no running mounter pod found")
)

// mountSyscallFunc performs the FUSE mount and returns the fd. Injectable for testing.
type mountSyscallFunc func(target string, opts mpmounter.MountOptions) (int, error)

// sendOptionsFunc sends mount options to the mounter via Unix socket. Injectable for testing.
type sendOptionsFunc func(ctx context.Context, sockPath string, options mountoptions.Options) error

// DaemonsetMounter is a [Mounter] that delegates Mountpoint process management
// to a secondary daemonset running on the same node. It communicates via the
// secondary pod's emptyDir volume, accessed through the kubelet pod directory.
type DaemonsetMounter struct {
	clientset   kubernetes.Interface
	nodeID      string
	kubeletPath string
	mount       *mpmounter.Mounter

	// Comm dir discovery: commDir caches the path (nil = stale),
	// rediscoverCh wakes the background watcher to re-discover immediately.
	commDir      atomic.Pointer[string]
	rediscoverCh chan struct{}

	// Injectable for testing. nil = use default.
	mountSyscall mountSyscallFunc
	sendOptions  sendOptionsFunc
}

// NewDaemonsetMounter creates a new [DaemonsetMounter].
// mountSyscall may be nil, in which case the default FUSE mount implementation is used.
func NewDaemonsetMounter(clientset kubernetes.Interface, nodeID string, mount *mpmounter.Mounter,
	mountSyscall mountSyscallFunc) *DaemonsetMounter {
	return &DaemonsetMounter{
		clientset:    clientset,
		nodeID:       nodeID,
		kubeletPath:  util.ContainerKubeletPath(),
		mount:        mount,
		rediscoverCh: make(chan struct{}, 1),
		mountSyscall: mountSyscall,
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
	// If target doesn't exist (ErrNotExist), defer directory creation to MkdirAll step.
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		// Note: Skipped handling dm.mount.IsMountpointCorrupted(err) case - we accept that for old
		// pods they won't recover after daemonset-mounter restart, unlike PodMounter:verifyOrSetupMountTarget
		// TODO: daemonset mounter pod recovery
		if dm.mount.IsMountpointCorrupted(err) {
			klog.Errorf("DaemonsetMounter: mount point %q is corrupted for pod %s: %v", target, credentialCtx.WorkloadPodID, err)
		}
		return fmt.Errorf("failed to check if target %q is a mount point (mount target is possibly"+
			" corrupted, manual pod re-creation %s might be required for mount recovery): %w",
			target, credentialCtx.WorkloadPodID, err)
	}
	if isMounted {
		klog.V(4).Infof("DaemonsetMounter: target %s is already mounted, nothing to do", target)
		return nil
	}

	commDir, err := dm.GetCommDir()
	if err != nil {
		return fmt.Errorf("connection to s3-csi-daemonset-mounter not yet established, allowing kubelet to retry NodePublishVolume: %w", err)
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
	fd, err := dm.mountSyscallWithDefault(target, mountOpts)
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
	sockPath := filepath.Join(commDir, MountSockName)
	errFilePath := filepath.Join(commDir, mountId+MountErrorSuffix)

	// Remove old error file if exists
	os.Remove(errFilePath)

	klog.V(4).Infof("DaemonsetMounter: sending mount options for volume %s (mount %s) to %s", volumeId, mountId, sockPath)

	sendCtx, sendCancel := context.WithTimeout(ctx, sendOptionsTimeout)
	defer sendCancel()

	err = dm.sendOptionsWithDefault(sendCtx, sockPath, mountoptions.Options{
		Fd:         fd,
		BucketName: bucketName,
		Args:       args.SortedList(),
		Env:        env.List(),
		VolumeId:   mountId,
	})
	if err != nil {
		// If send failed due to stale path, signal re-discovery and let Kubelet retry NodePublishVolume.
		// TODO: add helpMessageForGettingMountpointLogs to help users on mount failures
		// TODO: add tests for these errors to make sure they cover all required error cases
		if errors.Is(err, fs.ErrNotExist) || os.IsPermission(err) || errors.Is(err, context.DeadlineExceeded) {
			klog.V(4).Infof("DaemonsetMounter: comm dir may be stale, signaling re-discovery")
			dm.commDir.Store(nil)
			select {
			case dm.rediscoverCh <- struct{}{}:
			default:
			}
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
	if dir := dm.commDir.Load(); dir != nil {
		mountId := credentialCtx.PodID + "-" + credentialCtx.VolumeID
		os.Remove(filepath.Join(*dir, mountId+MountErrorSuffix))
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

// mountSyscallWithDefault delegates to mountSyscall if set, or falls back to dm.mount.Mount.
func (dm *DaemonsetMounter) mountSyscallWithDefault(target string, opts mpmounter.MountOptions) (int, error) {
	if dm.mountSyscall != nil {
		return dm.mountSyscall(target, opts)
	}
	return dm.mount.Mount(target, opts)
}

// sendOptionsWithDefault delegates to sendOptions if set, or falls back to mountoptions.Send.
func (dm *DaemonsetMounter) sendOptionsWithDefault(ctx context.Context, sockPath string, opts mountoptions.Options) error {
	if dm.sendOptions != nil {
		return dm.sendOptions(ctx, sockPath, opts)
	}
	return mountoptions.Send(ctx, sockPath, opts)
}

// DiscoverCommDir discovers the comm dir path synchronously with retries.
// It blocks until the secondary mounter pod is found or the timeout expires.
func (dm *DaemonsetMounter) DiscoverCommDir(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, commDirDiscoveryTimeout)
	defer cancel()

	// 82.5s max (0.5 + 1 + 2 + 4 + 5*15), bounded by commDirDiscoveryTimeout (60s) context.
	backoff := wait.Backoff{
		Duration: 500 * time.Millisecond,
		Factor:   2.0,
		Steps:    20, // i.e. 19 sleeps
		Cap:      5 * time.Second,
	}

	var lastErr error
	err := wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		dir, err := dm.tryDiscoverCommDir(ctx)
		if err == nil {
			dm.commDir.Store(&dir)
			return true, nil
		}
		lastErr = err
		klog.V(4).Infof("DaemonsetMounter: discovery failed: %v", err)
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("%w, check that s3-csi-daemonset-mounter is running on this node (last: %w): %w", ErrCommDirDiscoveryFailed, lastErr, err)
	}
	return nil
}

// StartCommDirWatch runs a background health-check loop that periodically verifies
// the comm dir socket is healthy and re-discovers it on staleness (e.g. secondary pod
// restart). Also wakes immediately when Mount signals staleness via rediscoverCh.
// TODO: decrease interval of checks when stale?
func (dm *DaemonsetMounter) StartCommDirWatch(stopCh <-chan struct{}) {
	ticker := time.NewTicker(commDirCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			dm.checkCommDir()
		case <-dm.rediscoverCh:
			dm.checkCommDir()
		}
	}
}

// checkCommDir verifies the socket exists and re-discovers if stale.
func (dm *DaemonsetMounter) checkCommDir() {
	dir := dm.commDir.Load()
	if dir != nil {
		sockPath := filepath.Join(*dir, MountSockName)
		if _, err := os.Stat(sockPath); err == nil {
			return // healthy, nothing to do
		}
		klog.V(2).Infof("DaemonsetMounter: socket gone, re-discovering")
		dm.commDir.Store(nil)
	}

	ctx, cancel := context.WithTimeout(context.Background(), commDirRediscoveryTimeout)
	defer cancel()
	newDir, err := dm.tryDiscoverCommDir(ctx)
	if err != nil {
		klog.V(4).Infof("DaemonsetMounter: rediscovery failed: %v", err)
		return
	}
	dm.commDir.Store(&newDir)
	klog.V(2).Infof("DaemonsetMounter: re-discovered comm dir: %s", newDir)
}

// GetCommDir returns the cached comm dir path without blocking, exported for testing
// Returns an error if the path is not yet discovered or has been marked stale.
func (dm *DaemonsetMounter) GetCommDir() (string, error) {
	dir := dm.commDir.Load()
	if dir == nil {
		return "", ErrCommDirNotReady
	}
	return *dir, nil
}

// tryDiscoverCommDir performs a single attempt to find the secondary mounter pod on
// this node and returns the path to its emptyDir comm volume as seen from the
// primary daemonset (via kubelet pod dir).
func (dm *DaemonsetMounter) tryDiscoverCommDir(ctx context.Context) (string, error) {
	pods, err := dm.clientset.CoreV1().Pods(mounterNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: mounterPodLabel,
		FieldSelector: "spec.nodeName=" + dm.nodeID,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list mounter pods on node %s: %w", dm.nodeID, err)
	}

	var running []corev1.Pod
	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodRunning {
			running = append(running, pod)
		}
	}

	if len(running) > 1 {
		return "", fmt.Errorf("%w on node %s (expected exactly 1, got %d)", ErrMultipleMounterPods, dm.nodeID, len(running))
	}
	if len(running) == 0 {
		return "", fmt.Errorf("%w on node %s", ErrNoRunningMounterPod, dm.nodeID)
	}

	podUID := string(running[0].UID)
	commDir := filepath.Join(dm.kubeletPath, "pods", podUID, "volumes", "kubernetes.io~empty-dir", CommVolumeName)
	klog.V(4).Infof("DaemonsetMounter: discovered mounter pod %s (uid=%s), comm dir: %s", running[0].Name, podUID, commDir)
	return commDir, nil
}

// waitForMount waits until Mountpoint is serving at target or an error occurs.
func (dm *DaemonsetMounter) waitForMount(parentCtx context.Context, target, mountId, errFilePath string) error {
	ctx, cancel := context.WithTimeout(parentCtx, mountReadyTimeout)
	defer cancel()

	mountResultCh := make(chan error, 2)

	// Poll for error file
	go func() {
		wait.PollUntilContextCancel(ctx, mountReadyPollInterval, true, func(ctx context.Context) (bool, error) {
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
		err := wait.PollUntilContextCancel(ctx, mountReadyPollInterval, true, func(ctx context.Context) (bool, error) {
			isMounted, _ := dm.mount.CheckMountpoint(target)
			return isMounted, nil
		})
		if err != nil {
			mountResultCh <- fmt.Errorf("timed out waiting for Mountpoint to serve mount %s at %s, check s3-csi-daemonset-mounter pod logs for mount-s3 startup errors", mountId, target)
		} else {
			mountResultCh <- nil
		}
	}()

	return <-mountResultCh
}
