// DaemonsetMounter is the primary side of the two-daemonset architecture: where the primary daemonset
// (s3-csi-node, privileged) performs FUSE mounts and passes file descriptors (fds) to the secondary
// daemonset (s3-csi-daemonset-mounter, unprivileged) which runs mount-s3 to serve S3 I/O.
//
// The two daemonsets communicate through the secondary daemonset's emptyDir volume (commDir). The
// primary daemonset discovers and maintains the commDir path, re-discovering it when the secondary
// daemonset restarts.
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

	k8sstrings "k8s.io/utils/strings"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/envprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/targetpath"
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

	mountReadyTimeout      = 2 * time.Minute
	mountReadyPollInterval = 500 * time.Millisecond

	commDirCheckInterval      = 5 * time.Second
	commDirStaleCheckInterval = 1 * time.Second
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

// DaemonsetMounter is a [Mounter] that delegates Mountpoint process management
// to a secondary daemonset running on the same node. It communicates via the
// secondary pod's emptyDir volume, accessed through the kubelet pod directory.
type DaemonsetMounter struct {
	clientset    kubernetes.Interface
	nodeID       string
	kubeletPath  string
	mount        *mpmounter.Mounter
	credProvider credentialprovider.ProviderInterface

	// Comm dir discovery: commDir caches the path (nil = stale),
	// rediscoverCh wakes the background watcher to re-discover immediately.
	commDir      atomic.Pointer[string]
	rediscoverCh chan struct{}

	// Injectable for testing. nil = use default.
	mountSyscall mountSyscallFunc
}

// NewDaemonsetMounter creates a new [DaemonsetMounter].
// mountSyscall may be nil, in which case the default FUSE mount implementation is used.
func NewDaemonsetMounter(clientset kubernetes.Interface, nodeID string, mount *mpmounter.Mounter,
	credProvider credentialprovider.ProviderInterface, mountSyscall mountSyscallFunc) *DaemonsetMounter {
	return &DaemonsetMounter{
		clientset:    clientset,
		nodeID:       nodeID,
		kubeletPath:  util.ContainerKubeletPath(),
		mount:        mount,
		credProvider: credProvider,
		rediscoverCh: make(chan struct{}, 1),
		mountSyscall: mountSyscall,
	}
}

// Mount mounts the given S3 bucket at the target path.
//
// It performs the following steps:
//  1. Provides credentials for the mount
//  2. Opens /dev/fuse and performs the kernel FUSE mount on target
//  3. Sends mount options (including FUSE FD) to the secondary daemonset via UDS
//  4. Waits for Mountpoint to start serving (or an error)
func (dm *DaemonsetMounter) Mount(ctx context.Context, bucketName string, target string,
	credentialCtx credentialprovider.ProvideContext, args mountpoint.Args, fsGroup string, userEnv envprovider.Environment) error {

	mountId, err := GetMountId(target)
	if err != nil {
		return err
	}
	volumeId := credentialCtx.VolumeID
	mountSuccess := false

	commDir, err := dm.GetCommDir()
	if err != nil {
		return fmt.Errorf("failed to find s3-csi-daemonset-mounter pod: %w. %s", err, helpMessageForCheckingMounterPodStatus())
	}

	// Provision credentials before the isMounted early return so republish refreshes them
	credsEnv, err := dm.provideCredentials(ctx, commDir, mountId, &credentialCtx)
	if err != nil {
		return err
	}
	defer func() {
		if mountSuccess {
			return
		}
		if err := dm.cleanupCredentials(commDir, mountId, credentialCtx.ToCleanupCtx()); err != nil {
			klog.Errorf("DaemonsetMounter: failed to clean up credential directory for mount %s: %v", mountId, err)
			// todo: once we have UID allocation, we don't return UID to the pool here, we need to cleanup creds first
		}
	}()

	// Idempotency: if target is already a healthy Mountpoint mount, return early
	isMounted, err := dm.IsMountPoint(target)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("failed to check if target %q is a mount point (mount target is possibly"+
			" corrupted, manual pod re-creation %s might be required for mount recovery): %w",
			target, credentialCtx.WorkloadPodID, err)
	}
	if isMounted {
		klog.V(4).Infof("DaemonsetMounter: target %s is already mounted, credentials refreshed", target)
		mountSuccess = true
		return nil
	}

	// Ensure target directory exists (kubelet may not have created it yet)
	if err := os.MkdirAll(target, targetDirPerm); err != nil {
		return fmt.Errorf("failed to create target directory %q: %w", target, err)
	}

	// Remove old error file if exists
	errFilePath := filepath.Join(commDir, GetErrorFileName(mountId))
	if err = removeIfExists(errFilePath); err != nil {
		return err
	}

	// Perform FUSE mount and send options to secondary daemonset
	if err := dm.mountS3AtTarget(ctx, target, bucketName, args, mountId, volumeId, commDir, userEnv, credsEnv); err != nil {
		return err
	}

	// Clean up the FUSE mount if waitForMount fails
	defer func() {
		if !mountSuccess {
			// TODO(vlaad): might leak a mount, implement periodic stale mount cleanup
			if umErr := dm.mount.Unmount(target); umErr != nil {
				klog.Errorf("Failed to unmount %q during cleanup: %v", target, umErr)
			}
		}
	}()

	// Wait for mount readiness or error
	err = dm.waitForMount(ctx, target, mountId, errFilePath)
	if err != nil {
		return err
	}

	mountSuccess = true
	klog.V(4).Infof("DaemonsetMounter: volume %s (mount %s) mounted at %s", volumeId, mountId, target)
	return nil
}

// Unmount unmounts the FUSE filesystem at target.
// This causes the Mountpoint process in the secondary daemonset to exit.
func (dm *DaemonsetMounter) Unmount(ctx context.Context, target string, cleanupCtx credentialprovider.CleanupContext) error {
	// cleanup the error file and credentials
	commDir, err := dm.GetCommDir()
	if err != nil {
		return fmt.Errorf("failed to find s3-csi-daemonset-mounter pod: %w. %s", err, helpMessageForCheckingMounterPodStatus())
	}
	mountId, err := GetMountId(target)
	if err != nil {
		return err
	}

	if err = removeIfExists(filepath.Join(commDir, GetErrorFileName(mountId))); err != nil {
		return fmt.Errorf("failed to remove the error file: %w", err)
	}

	err = dm.cleanupCredentials(commDir, mountId, cleanupCtx)
	if err != nil {
		return fmt.Errorf("failed to cleanup credentials: %w", err)
	}

	// finally unmount (this order ensures retrying cleanup)
	err = dm.mount.Unmount(target)
	if err != nil {
		return fmt.Errorf("failed to unmount %q: %w", target, err)
	}

	klog.V(4).Infof("DaemonsetMounter: volume %s unmounted from %s", cleanupCtx.VolumeID, target)
	return nil
}

// IsMountPoint returns whether the given target is a Mountpoint mount.
func (dm *DaemonsetMounter) IsMountPoint(target string) (bool, error) {
	return dm.mount.CheckMountpoint(target)
}

// mountS3AtTarget performs the kernel FUSE mount at target and sends mount options
// (including the FUSE fd) to the secondary daemonset. On success the mount remains
// at target; on failure any partial mount is cleaned up.
func (dm *DaemonsetMounter) mountS3AtTarget(ctx context.Context, target string, bucketName string,
	args mountpoint.Args, mountId string, volumeId string, commDir string,
	userEnv envprovider.Environment, credsEnv envprovider.Environment) error {

	mountOpts := mpmounter.MountOptions{
		ReadOnly:   args.Has(mountpoint.ArgReadOnly),
		AllowOther: args.Has(mountpoint.ArgAllowOther) || args.Has(mountpoint.ArgAllowRoot),
	}
	fd, err := dm.mountSyscallWithDefault(target, mountOpts)
	if err != nil {
		return fmt.Errorf("failed to mount FUSE at %q: %w", target, err)
	}
	defer func() {
		if err := mpmounter.CloseFD(fd); err != nil {
			klog.V(4).Infof("DaemonsetMounter: failed to close /dev/fuse fd %d: %v", fd, err)
		}
	}()

	// TODO: add --user-agent-prefix for S3 request telemetry

	// Build environment
	env := envprovider.Environment{}
	env.Merge(userEnv)
	env.Merge(envprovider.Default())
	env.Merge(credsEnv)

	// Move --aws-max-attempts to env if provided
	if maxAttempts, ok := args.Remove(mountpoint.ArgAWSMaxAttempts); ok {
		env.Set(envprovider.EnvMaxAttempts, maxAttempts)
	}
	// Remove read-only from args since we already passed MS_RDONLY in mount syscall
	args.Remove(mountpoint.ArgReadOnly)

	// Send mount options to secondary daemonset
	sockPath := filepath.Join(commDir, MountSockName)
	klog.V(4).Infof("DaemonsetMounter: sending mount options for volume %s (mount %s) to %s", volumeId, mountId, sockPath)
	sendCtx, sendCancel := context.WithTimeout(ctx, sendOptionsTimeout)
	defer sendCancel()
	err = mountoptions.Send(sendCtx, sockPath, mountoptions.Options{
		Fd:         fd,
		BucketName: bucketName,
		Args:       args.SortedList(),
		Env:        env.List(),
		VolumeId:   mountId,
	})
	if err != nil {
		// If send failed due to stale path, signal re-discovery and let Kubelet retry NodePublishVolume.
		// Send -> dialWithRetry retries ENOENT/ECONNREFUSED, so we only check these errors
		if errors.Is(err, fs.ErrNotExist) || os.IsPermission(err) || errors.Is(err, context.DeadlineExceeded) {
			klog.V(4).Infof("DaemonsetMounter: comm dir may be stale, signaling re-discovery")
			dm.commDir.Store(nil)
			select {
			case dm.rediscoverCh <- struct{}{}:
			default:
			}
		}
		dm.mount.Unmount(target)
		return fmt.Errorf("failed to send mount options for volume %s (mount %s): %w. %s", volumeId, mountId, err, helpMessageForGettingMounterLogs())
	}

	return nil
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
			mountResultCh <- fmt.Errorf("timed out waiting for Mountpoint to serve mount %s at %s. %s", mountId, target, helpMessageForGettingMounterLogs())
		} else {
			mountResultCh <- nil
		}
	}()

	return <-mountResultCh
}

// mountSyscallWithDefault delegates to mountSyscall if set, or falls back to dm.mount.Mount.
func (dm *DaemonsetMounter) mountSyscallWithDefault(target string, opts mpmounter.MountOptions) (int, error) {
	if dm.mountSyscall != nil {
		return dm.mountSyscall(target, opts)
	}
	return dm.mount.Mount(target, opts)
}

// provideCredentials creates a per-mount credential directory and provisions credentials into it.
func (dm *DaemonsetMounter) provideCredentials(ctx context.Context, commDir, mountId string, credentialCtx *credentialprovider.ProvideContext) (envprovider.Environment, error) {
	mountCredDir := filepath.Join(commDir, mountId)
	if err := os.MkdirAll(mountCredDir, credentialprovider.CredentialDirPerm); err != nil {
		return nil, fmt.Errorf("failed to create credential directory %q: %w", mountCredDir, err)
	}
	credentialCtx.WritePath = mountCredDir
	credentialCtx.EnvPath = filepath.Join("/comm", mountId)
	credentialCtx.MountKind = credentialprovider.MountKindDaemonset

	env, _, err := dm.credProvider.Provide(ctx, *credentialCtx)
	if err != nil {
		return nil, fmt.Errorf("failed to provide credentials for mount %s: %w", mountId, err)
	}
	return env, nil
}

// cleanupCredentials removes the per-mount credential directory.
func (dm *DaemonsetMounter) cleanupCredentials(commDir, mountId string, cleanupCtx credentialprovider.CleanupContext) error {
	mountCredDir := filepath.Join(commDir, mountId)
	cleanupCtx.WritePath = mountCredDir
	cleanupCtx.MountKind = credentialprovider.MountKindDaemonset
	if err := dm.credProvider.Cleanup(cleanupCtx); err != nil {
		return err
	}
	if err := os.RemoveAll(mountCredDir); err != nil {
		return err
	}
	return nil
}

// GetMountId returns a filesystem-safe mount ID derived from the target path.
// It uses the pod UID and PV name from the kubelet target path structure:
//
//	.../pods/<pod-uid>/volumes/kubernetes.io~csi/<pv-name>/mount
//
// Both components are escaped as defense in depth against unexpected characters.
func GetMountId(target string) (string, error) {
	tp, err := targetpath.Parse(target)
	if err != nil {
		return "", fmt.Errorf("failed to parse target path %q: %w", target, err)
	}
	return k8sstrings.EscapeQualifiedName(tp.PodID) + "-" + k8sstrings.EscapeQualifiedName(tp.VolumeID), nil
}

// GetErrorFileName returns the error file name for a given mount ID.
func GetErrorFileName(mountId string) string {
	return mountId + MountErrorSuffix
}

// removeIfExists removes a file, ignoring "not exist" errors.
func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// helpMessageForGettingMounterLogs returns a help message with a command to get mounter logs.
func helpMessageForGettingMounterLogs() string {
	return fmt.Sprintf("You can see mounter logs by running: `kubectl logs -n %s -l app=s3-csi-daemonset-mounter`", mounterNamespace)
}

// helpMessageForCheckingMounterPodStatus returns a help message with a command to check mounter pod status.
func helpMessageForCheckingMounterPodStatus() string {
	return fmt.Sprintf("You can check mounter pod status by running: `kubectl get pods -n %s -l app=s3-csi-daemonset-mounter`", mounterNamespace)
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
func (dm *DaemonsetMounter) StartCommDirWatch(stopCh <-chan struct{}) {
	ticker := time.NewTicker(commDirCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
		case <-dm.rediscoverCh:
		}
		// Polls faster when comm dir is stale
		if dm.checkCommDir() {
			ticker.Reset(commDirCheckInterval)
		} else {
			ticker.Reset(commDirStaleCheckInterval)
		}
	}
}

// checkCommDir verifies the socket exists and re-discovers if stale.
// Returns true if comm dir is healthy after the check.
func (dm *DaemonsetMounter) checkCommDir() bool {
	dir := dm.commDir.Load()
	if dir != nil {
		sockPath := filepath.Join(*dir, MountSockName)
		if _, err := os.Stat(sockPath); err == nil {
			return true
		}
		klog.V(2).Infof("DaemonsetMounter: socket gone, re-discovering")
		dm.commDir.Store(nil)
	}

	ctx, cancel := context.WithTimeout(context.Background(), commDirRediscoveryTimeout)
	defer cancel()
	newDir, err := dm.tryDiscoverCommDir(ctx)
	if err != nil {
		klog.V(4).Infof("DaemonsetMounter: rediscovery failed: %v", err)
		return false
	}
	dm.commDir.Store(&newDir)
	klog.V(2).Infof("DaemonsetMounter: re-discovered comm dir: %s", newDir)
	return true
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
