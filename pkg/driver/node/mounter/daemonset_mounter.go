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
//	IsMountPoint -> GetCommDir -> ProvideCredentials -> Mount (FUSE) -> Send -> waitForMount
//	Stale commDir path? -> store nil, signal rediscoverCh, return error
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
	"strings"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	mountutils "k8s.io/mount-utils"

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

	// cleanupInterval is how often the periodic cleanup job runs.
	cleanupInterval = 2 * time.Minute

	// cleanupHealthCheckTimeout caps the source health probe so a frozen FUSE daemon can't stall cleanup.
	cleanupHealthCheckTimeout = 10 * time.Second
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

// bindMountSyscallFunc performs a bind mount. Injectable for testing.
type bindMountSyscallFunc func(source, target string) error

// mountInfoProviderFunc reads the kernel mount table. Injectable for testing.
type mountInfoProviderFunc func() ([]mountutils.MountInfo, error)

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
	mountSyscall      mountSyscallFunc
	bindMountSyscall  bindMountSyscallFunc
	mountInfoProvider mountInfoProviderFunc

	// mountMap tracks shared source mounts for pod-sharing.
	// Mount/Unmount use reference-counted sharing via this map.
	mountMap *MountMap
}

// NewDaemonsetMounter creates a new [DaemonsetMounter].
// mountSyscall, bindMountSyscall, and mountInfoProvider may be nil,
// in which case the default implementations are used.
func NewDaemonsetMounter(clientset kubernetes.Interface, nodeID string, mount *mpmounter.Mounter,
	credProvider credentialprovider.ProviderInterface, mountSyscall mountSyscallFunc, bindMountSyscall bindMountSyscallFunc, mountInfoProvider mountInfoProviderFunc) *DaemonsetMounter {
	return &DaemonsetMounter{
		clientset:         clientset,
		nodeID:            nodeID,
		kubeletPath:       util.ContainerKubeletPath(),
		mount:             mount,
		credProvider:      credProvider,
		rediscoverCh:      make(chan struct{}, 1),
		mountSyscall:      mountSyscall,
		bindMountSyscall:  bindMountSyscall,
		mountInfoProvider: mountInfoProvider,
		mountMap:          NewMountMap(),
	}
}

// Mount mounts the given S3 bucket at the target path with pod-sharing support.
//
// Flow:
//  1. Acquire per-volume lock via MountMap (serializes concurrent NodePublishVolume for same PV)
//  2. If source is mounted, check source health:
//     - Dead source → mark sourceMounted=false (fall through to step 3)
//     - Healthy source → validate compatibility (reject incompatible params before any cred writes)
//  3. If source not mounted (fresh, dead-source recovery, or prior failed attempt):
//     - Clean up any stale resources via cleanupMount (idempotent); fail mount if cleanup fails
//     - Write meta file, set SourcePath/CommDir on the entry
//  4. Provision credentials (under lock to avoid race with cleanup on failure)
//  5. If target is already mounted (republish/retry): creds refreshed above, return early
//  6. If source is mounted (healthy) → bind mount to new target, bump refcount
//  7. If source not mounted → FUSE mount at source, bind mount source → target,
//     set sourceMounted=true and refcount=1
//
// Error handling:
//   - If fuseMount or bindMount fails, cleanupMount is called. If cleanup succeeds,
//     the map entry and meta file are removed (clean slate for next retry). If cleanup
//     fails, the entry and meta are preserved so the next retry enters step 3 and
//     retries cleanup before proceeding.
func (dm *DaemonsetMounter) Mount(ctx context.Context, bucketName string, target string,
	credentialCtx credentialprovider.ProvideContext, args mountpoint.Args, fsGroup string, userEnv envprovider.Environment) error {

	// Check target health
	//   - (TargetAbsent, nil):  target is absent/fresh — proceed with mount.
	//   - (TargetHealthy, nil): target is healthy and mounted (republish/legacy).
	//   - (TargetDead, nil):    target mount is dead/corrupted — return nil.
	//   - (_, err):             UNKNOWN — let kubelet retry.
	targetState, targetErr := dm.CheckTargetState(ctx, target)
	if targetErr != nil {
		return fmt.Errorf("cannot determine health of target %q for volume %s, will retry: %w", target, credentialCtx.VolumeID, targetErr)
	}
	if targetState == TargetDead {
		klog.V(2).Infof("DaemonsetMounter: target %s for volume %s is a corrupted mount; not proceeding, pod re-creation %s required for recovery", target, credentialCtx.VolumeID, credentialCtx.WorkloadPodID)
		return nil
	}

	// Handle legacy mounts — only for healthy mounted targets.
	if targetState == TargetHealthy {
		// V1 (systemd) check: target is mounted with zero bind-mount references.
		if dm.isSystemDMountpoint(target) {
			klog.Infof("DaemonsetMounter: target %s is a legacy V1 (systemd) mount for volume %s, will only refresh credentials", target, credentialCtx.VolumeID)
			credentialCtx.SetAsSystemDMountpoint()
			credentialsPath := hostPluginDirWithDefault()
			credentialCtx.SetWriteAndEnvPath(credentialsPath, credentialsPath)

			if _, _, err := dm.credProvider.Provide(ctx, credentialCtx); err != nil {
				klog.Errorf("DaemonsetMounter: failed to refresh V1 (systemd) credentials for %q: %v", target, err)
				return fmt.Errorf("Failed to provide systemd credentials: %w", err)
			}
			return nil
		}
	}

	// Extract PV name from target path to use as the volume identifier for sharing and filesystem paths.
	// PV names are Kubernetes resource names — guaranteed DNS-safe (no '/', no '..', alphanumeric + '-' + '.').
	// Target path format: /var/lib/kubelet/pods/<podUID>/volumes/kubernetes.io~csi/<pv-name>/mount
	parsedTarget, err := targetpath.Parse(target)
	if err != nil {
		return fmt.Errorf("failed to parse target path %q: %w", target, err)
	}
	volumeID := parsedTarget.VolumeID // This is the PV name

	// Resolve commDir once per NodePublishVolume to ensure credentials and mount options
	// are sent to the same mounter instance. Prevents the race where mounter pod restarts
	// between provideCredentials and fuseMount, causing mount-s3 to start without creds.
	commDir, err := dm.GetCommDir()
	if err != nil {
		return fmt.Errorf("connection to s3-csi-daemonset-mounter not yet established, allowing kubelet to retry NodePublishVolume: %w. %s", err, helpMessageForCheckingMounterPodStatus())
	}

	// All paths (republish, share, new mount) go through mountOrShareSource
	// which holds the per-volume lock and validates compatibility before any
	// credential writes.
	return dm.mountOrShareSource(ctx, bucketName, target, volumeID, commDir, credentialCtx, args, fsGroup, userEnv, targetState == TargetHealthy)
}

// mountOrShareSource implements the pod-sharing Mount flow using MountMap.
func (dm *DaemonsetMounter) mountOrShareSource(ctx context.Context, bucketName string, target string,
	volumeID string, commDir string, credentialCtx credentialprovider.ProvideContext, args mountpoint.Args, fsGroup string, userEnv envprovider.Environment, targetIsMounted bool) error {

	// Get or create the per-volume entry, then lock it.
	// Retry loop ensures we hold the canonical entry — not one orphaned by a concurrent unmount/delete.
	var entry *MountEntry
	for {
		entry, _ = dm.mountMap.GetOrCreate(volumeID)
		entry.mu.Lock()
		if dm.mountMap.Get(volumeID) == entry {
			break // we hold the canonical entry
		}
		entry.mu.Unlock() // orphaned entry (deleted by concurrent unmount), retry
	}
	defer entry.mu.Unlock()

	// Build mount params for this request — used for validation and stored on first mount.
	incomingParams := MountParams{
		MountOptions:             args.SortedList(),
		AuthenticationSource:     credentialCtx.AuthenticationSource,
		ServiceAccountName:       credentialCtx.ServiceAccountName,
		ServiceAccountEKSRoleARN: credentialCtx.ServiceAccountEKSRoleARN,
		PodNamespace:             credentialCtx.PodNamespace,
		FSGroup:                  fsGroup,
	}

	// If source is mounted, check health first. Dead source = mark not mounted so we go
	// through the fresh-mount path below. Only enforce compatibility on a healthy (living) source.
	if entry.sourceMounted {
		healthy, healthErr := dm.IsSourceHealthy(ctx, entry.SourcePath)
		switch {
		case healthErr != nil:
			// Health could not be determined (transient error or timeout). Fail closed:
			// do NOT tear down a possibly-live source out from under active consumers.
			// Return an error so kubelet retries NodePublishVolume.
			return fmt.Errorf("cannot determine health of existing source mount for volume %s, will retry: %w", volumeID, healthErr)
		case !healthy:
			klog.V(2).Infof("DaemonsetMounter: source %s is dead for volume %s, will clean up and re-mount", entry.SourcePath, volumeID)
			entry.sourceMounted = false
		default:
			// Healthy source — enforce compatibility before any credential writes.
			if err := entry.Params.ValidateCompatibility(&incomingParams); err != nil {
				return fmt.Errorf("cannot share mount for volume %s: %w", volumeID, err)
			}
		}
	}

	if !entry.sourceMounted {
		// Fresh mount: ensure no associated resources (credentials, error file, FUSE mount,
		// source directory) are left behind from a previous attempt or dead source before
		// creating new ones. cleanupMount is idempotent — safe when resources don't exist.
		if cleanErr := dm.cleanupMount(entry, credentialCtx.ToCleanupCtx()); cleanErr != nil {
			return fmt.Errorf("failed to clean up stale resources for volume %s, cannot proceed with fresh mount: %w", volumeID, cleanErr)
		}

		entry.Params = incomingParams
		entry.SourcePath = SourceMountPath(dm.kubeletPath, volumeID)
		entry.CommDir = commDir
		if err := WriteMeta(dm.kubeletPath, entry); err != nil {
			return fmt.Errorf("failed to write meta for volume %s, cannot proceed with mount: %w", volumeID, err)
		}
	}

	// Provision credentials under the lock. We always use entry.CommDir which is set above
	// (either from an existing healthy entry, or freshly assigned from commDir on new mount).
	// This ensures credentials are written to the same location that cleanup will look at.
	credsEnv, err := dm.provideCredentials(ctx, entry.CommDir, volumeID, &credentialCtx)
	if err != nil {
		return err
	}

	// Idempotency: if target is already mounted (republish/retry), creds are refreshed above, done.
	if targetIsMounted {
		klog.V(4).Infof("DaemonsetMounter: target %s is already mounted, credentials refreshed", target)
		return nil
	}

	if entry.sourceMounted {
		// Source was confirmed healthy above. Bind mount to new target.
		if err := dm.BindMount(entry.SourcePath, target); err != nil {
			return err
		}
		entry.RefCount++
		entry.Targets = append(entry.Targets, target)
		klog.V(4).Infof("DaemonsetMounter: shared existing mount for volume %s → %s (refcount=%d)",
			volumeID, target, entry.RefCount)
		return nil
	}

	// New mount: FUSE mount at source, then bind to target.

	if err := dm.fuseMount(ctx, bucketName, entry.SourcePath, volumeID, commDir, args, userEnv, credsEnv); err != nil {
		if cleanErr := dm.cleanupMount(entry, credentialCtx.ToCleanupCtx()); cleanErr != nil {
			klog.Errorf("DaemonsetMounter: cleanup after fuseMount failure for volume %s: %v", volumeID, cleanErr)
			return err
		}
		dm.mountMap.Delete(volumeID)
		RemoveMeta(dm.kubeletPath, volumeID)
		return err
	}

	// Bind mount source → target.
	if err := dm.BindMount(entry.SourcePath, target); err != nil {
		if cleanErr := dm.cleanupMount(entry, credentialCtx.ToCleanupCtx()); cleanErr != nil {
			klog.Errorf("DaemonsetMounter: cleanup after BindMount failure for volume %s: %v", volumeID, cleanErr)
			return err
		}
		dm.mountMap.Delete(volumeID)
		RemoveMeta(dm.kubeletPath, volumeID)
		return err
	}

	// Populate entry — SourcePath and CommDir already set above.
	entry.RefCount = 1
	entry.Targets = []string{target}
	entry.sourceMounted = true

	klog.V(4).Infof("DaemonsetMounter: new shared mount for volume %s at source %s → %s", volumeID, entry.SourcePath, target)
	return nil
}

// fuseMount performs the FUSE mount + FD send + wait cycle at the given path.
// Credentials are already provisioned by the caller (Mount).
// commDir is passed from the caller to ensure a single GetCommDir() per NodePublishVolume.
func (dm *DaemonsetMounter) fuseMount(ctx context.Context, bucketName string, mountPath string,
	volumeID string, commDir string, args mountpoint.Args, userEnv envprovider.Environment, credsEnv envprovider.Environment) error {

	if err := os.MkdirAll(mountPath, targetDirPerm); err != nil {
		return fmt.Errorf("failed to create mount directory %q: %w", mountPath, err)
	}

	mountOpts := mpmounter.MountOptions{
		ReadOnly:   args.Has(mountpoint.ArgReadOnly),
		AllowOther: args.Has(mountpoint.ArgAllowOther) || args.Has(mountpoint.ArgAllowRoot),
	}
	fd, err := dm.mountSyscallWithDefault(mountPath, mountOpts)
	if err != nil {
		return fmt.Errorf("failed to mount FUSE at %q: %w", mountPath, err)
	}

	fdClosed := false
	defer func() {
		if !fdClosed {
			dm.closeFUSEDevFD(fd)
		}
	}()

	args.Remove(mountpoint.ArgReadOnly)

	env := envprovider.Environment{}
	env.Merge(userEnv)
	env.Merge(envprovider.Default())
	env.Merge(credsEnv)

	if maxAttempts, ok := args.Remove(mountpoint.ArgAWSMaxAttempts); ok {
		env.Set(envprovider.EnvMaxAttempts, maxAttempts)
	}

	sockPath := filepath.Join(commDir, MountSockName)
	errFilePath := filepath.Join(commDir, GetErrorFileName(volumeID))
	os.Remove(errFilePath)

	klog.V(4).Infof("DaemonsetMounter: sending mount options (mount %s) to %s", volumeID, sockPath)

	sendCtx, sendCancel := context.WithTimeout(ctx, sendOptionsTimeout)
	defer sendCancel()

	err = mountoptions.Send(sendCtx, sockPath, mountoptions.Options{
		Fd:         fd,
		BucketName: bucketName,
		Args:       args.SortedList(),
		Env:        env.List(),
		VolumeId:   volumeID,
	})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || os.IsPermission(err) || errors.Is(err, context.DeadlineExceeded) {
			klog.V(4).Infof("DaemonsetMounter: comm dir may be stale, signaling re-discovery")
			dm.commDir.Store(nil)
			select {
			case dm.rediscoverCh <- struct{}{}:
			default:
			}
		}
		return fmt.Errorf("failed to send mount options (mount %s): %w. %s", volumeID, err, helpMessageForGettingMounterLogs())
	}

	dm.closeFUSEDevFD(fd)
	fdClosed = true

	err = dm.waitForMount(ctx, mountPath, volumeID, errFilePath)
	if err != nil {
		return err
	}

	return nil
}

// Unmount unmounts the target path with pod-sharing awareness.
//
// Flow:
//  1. Unmounts the bind mount at target
//  2. Decrements refcount in MountMap
//  3. If refcount reaches 0, unmounts the FUSE source and removes the entry
func (dm *DaemonsetMounter) Unmount(ctx context.Context, target string, credentialCtx credentialprovider.CleanupContext) error {
	// Handle V1 (systemd) legacy mounts
	// Check IsMountPoint first — if not mounted, isSystemDMountpoint would falsely match
	// (zero refs on an unmounted path).
	isMounted, mountErr := dm.IsMountPoint(target)
	if mountErr == nil && isMounted && dm.isSystemDMountpoint(target) {
		klog.Infof("DaemonsetMounter: unmounting legacy V1 (systemd) mount at %s for volume %s", target, credentialCtx.VolumeID)
		if err := dm.unmountIfMounted(target); err != nil {
			return fmt.Errorf("failed to unmount V1 (systemd) target %q: %w", target, err)
		}
		credentialCtx.SetAsSystemDMountpoint()
		credentialCtx.WritePath = hostPluginDirWithDefault()
		// Best-effort credential cleanup: the unmount above already succeeded, so we
		// don't fail the RPC over leftover cred files. A retry would skip this branch
		// (target no longer mounted) anyway.
		if err := dm.credProvider.Cleanup(credentialCtx); err != nil {
			klog.Errorf("DaemonsetMounter: failed to clean up V1 (systemd) credentials for %s: %v", target, err)
		}
		return nil
	}

	// Extract PV name from target path for V3 unmount flow.
	parsedTarget, err := targetpath.Parse(target)
	if err != nil {
		return fmt.Errorf("failed to parse target path %q: %w", target, err)
	}
	volumeID := parsedTarget.VolumeID // This is the PV name
	return dm.releaseTarget(target, volumeID, credentialCtx)
}

// releaseTarget handles the Unmount flow with MountMap refcounting.
func (dm *DaemonsetMounter) releaseTarget(target string, volumeID string, credentialCtx credentialprovider.CleanupContext) error {
	// The periodic cleanup job also deletes entries from the map, so between our
	// Get and our Lock the entry could be deleted/replaced. Re-check after locking
	// that we still hold the map's canonical entry; retry if not.
	var entry *MountEntry
	for {
		entry = dm.mountMap.Get(volumeID)
		if entry == nil {
			// No entry means this volume is not tracked (e.g., after CSI node restart without
			// successful RebuildMountMap recovery). Best-effort unmount the target in case a
			// stale bind mount still exists in the kernel mount table.
			klog.V(4).Infof("DaemonsetMounter: no mount map entry for volume %s, best-effort unmount of %s", volumeID, target)
			if err := dm.unmountIfMounted(target); err != nil {
				klog.Errorf("DaemonsetMounter: best-effort unmount of untracked target %s failed: %v", target, err)
			}
			return nil
		}
		entry.mu.Lock()
		if dm.mountMap.Get(volumeID) == entry {
			break
		}
		entry.mu.Unlock() // orphaned entry (deleted by concurrent unmount/cleanup), retry
	}
	defer entry.mu.Unlock()

	// Unmount the bind mount at target.
	if err := dm.unmountIfMounted(target); err != nil {
		return fmt.Errorf("failed to unmount bind mount at %q: %w", target, err)
	}

	// Remove target from entry.
	for i, t := range entry.Targets {
		if t == target {
			entry.Targets = append(entry.Targets[:i], entry.Targets[i+1:]...)
			entry.RefCount--
			break
		}
	}

	if entry.RefCount == 0 {
		// Last consumer: clean up all mount resources.
		klog.V(4).Infof("DaemonsetMounter: last consumer for volume %s, cleaning up mount", volumeID)

		if err := dm.cleanupMount(entry, credentialCtx); err != nil {
			klog.Errorf("DaemonsetMounter: %v (will retry on next unmount attempt)", err)
			// Don't remove meta or map entry — leave for retry/background cleanup.
		} else {
			// All resources confirmed cleaned — safe to remove bookkeeping.
			dm.mountMap.Delete(volumeID)
			RemoveMeta(dm.kubeletPath, volumeID)
		}
	}

	klog.V(4).Infof("DaemonsetMounter: volume %s unmounted from %s", volumeID, target)
	return nil
}

// StartPeriodicCleanup runs CleanupOrphans on a ticker until stopCh is closed.
//
// It's a safety net for when inline cleanup and startup cleanup miss resources orphaned while the driver keeps running with no operation touching a volume
// e.g. a cleanup that failed at refcount 0, or a source mount left dead after a mounter-pod crash.
func (dm *DaemonsetMounter) StartPeriodicCleanup(stopCh <-chan struct{}) {
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			dm.CleanupOrphans()
		}
	}
}

// For each entry, under the entry's lock:
//   - if the source mount is unhealthy, tear it down;
//   - otherwise reconcile the refcount against the live bind mounts, and if it drops
//     to zero, tear it down.
func (dm *DaemonsetMounter) CleanupOrphans() {
	dm.mountMap.Range(func(volumeID string, entry *MountEntry) bool {
		dm.cleanupEntry(volumeID, entry)
		return true
	})
}

// cleanupEntry reconciles and, if needed, tears down a single mount entry.
func (dm *DaemonsetMounter) cleanupEntry(volumeID string, entry *MountEntry) {
	entry.mu.Lock()
	defer entry.mu.Unlock()

	// A concurrent unmount may have deleted this entry between the Range read and
	// the lock. Confirm the map still points to this exact entry; if not, skip it.
	if dm.mountMap.Get(volumeID) != entry {
		return
	}

	// An empty SourcePath means GetOrCreate inserted a blank entry that has not been
	// given a mount yet (mid-creation). Nothing to clean, so leave it.
	if entry.SourcePath == "" {
		return
	}

	mountInfos, err := dm.mountInfoProviderWithDefault()
	if err != nil {
		klog.Errorf("DaemonsetMounter: cleanup: failed to read mount table for volume %s: %v", volumeID, err)
		return
	}

	sourceMI := findMountByPath(mountInfos, entry.SourcePath)
	if sourceMI == nil {
		// Source is gone from the mount table — no bind mounts can reference it, so
		// it's a true orphan (e.g. a crash between writing meta and creating the mount).
		klog.V(2).Infof("DaemonsetMounter: cleanup: source %s for volume %s not in mount table, cleaning up", entry.SourcePath, volumeID)
		dm.teardownEntry(volumeID, entry)
		return
	}

	// Being in the mount table doesn't mean the source still works — a crashed mounter leaves
	// a dead FUSE mount that lingers but errors on every I/O. So we probe it (with a timeout so
	// a frozen daemon can't stall the pass). If it's dead, tear it down and reclaim its resources;
	// if we can't tell, leave it alone rather than risk destroying a source still serving a workload.
	ctx, cancel := context.WithTimeout(context.Background(), cleanupHealthCheckTimeout)
	healthy, healthErr := dm.IsSourceHealthy(ctx, entry.SourcePath)
	cancel()
	switch {
	case healthErr != nil:
		klog.Errorf("DaemonsetMounter: cleanup: source health unknown for volume %s (%v), leaving intact", volumeID, healthErr)
		return
	case !healthy:
		klog.V(2).Infof("DaemonsetMounter: cleanup: source %s for volume %s is dead, cleaning up", entry.SourcePath, volumeID)
		dm.teardownEntry(volumeID, entry)
		return
	}

	liveTargets := findBindMountTargets(mountInfos, deviceID(sourceMI), entry.SourcePath)

	// Sync targets and refcount to the kernel mount table (the source of truth for who is actually bind-mounted).
	entry.Targets = liveTargets
	entry.RefCount = len(liveTargets)

	// If the kernel shows zero bind mounts on this source, nobody's using it — tear it down.
	if len(liveTargets) == 0 {
		klog.V(2).Infof("DaemonsetMounter: cleanup: volume %s has no remaining consumers, cleaning up", volumeID)
		dm.teardownEntry(volumeID, entry)
		return
	}

	// Source still in use — leave it. Logged (V4) so a healthy-volume pass is observable.
	klog.V(4).Infof("DaemonsetMounter: cleanup: volume %s healthy with %d live consumer(s), leaving intact", volumeID, len(liveTargets))
}

// teardownEntry runs cleanupMount and, only on success, removes the in-memory
// entry and then its meta file (entry before meta, matching the other teardown
// paths). On failure both are kept so a later pass retries. Caller must hold entry.mu.
func (dm *DaemonsetMounter) teardownEntry(volumeID string, entry *MountEntry) {
	cleanupCtx := credentialprovider.CleanupContext{VolumeID: volumeID}
	if err := dm.cleanupMount(entry, cleanupCtx); err != nil {
		klog.Errorf("DaemonsetMounter: cleanup: %v (will retry next tick)", err)
		return
	}
	dm.mountMap.Delete(volumeID)
	RemoveMeta(dm.kubeletPath, volumeID)
}

// GetErrorFileName returns the error file name for a given volume ID.
func GetErrorFileName(volumeID string) string {
	return volumeID + MountErrorSuffix
}

// cleanupMount removes all resources associated with a mount entry.
// Returns nil only when ALL resources are confirmed absent (deleted or already gone).
// Safe to call multiple times (idempotent). Caller must hold entry.mu if entry is in the map.
func (dm *DaemonsetMounter) cleanupMount(entry *MountEntry, credentialCtx credentialprovider.CleanupContext) error {
	var errs []error

	// Clean credential files and error file (both live in commDir)
	if entry.CommDir != "" {
		if err := dm.cleanupCredentials(entry.CommDir, entry.VolumeID, credentialCtx); err != nil {
			klog.Errorf("DaemonsetMounter: cleanup credentials for volume %s: %v", entry.VolumeID, err)
			errs = append(errs, err)
		}
		errFile := filepath.Join(entry.CommDir, GetErrorFileName(entry.VolumeID))
		if err := os.Remove(errFile); err != nil && !os.IsNotExist(err) {
			klog.Errorf("DaemonsetMounter: cleanup error file %s: %v", errFile, err)
			errs = append(errs, err)
		}
	}

	// Unmount FUSE source (causes mount-s3 to exit via kernel FUSE teardown)
	if entry.SourcePath != "" {
		if err := dm.unmountIfMounted(entry.SourcePath); err != nil {
			klog.Errorf("DaemonsetMounter: unmount source %s: %v", entry.SourcePath, err)
			errs = append(errs, err)
		}
		if err := os.Remove(entry.SourcePath); err != nil && !os.IsNotExist(err) {
			klog.Errorf("DaemonsetMounter: remove source dir %s: %v", entry.SourcePath, err)
			errs = append(errs, err)
		}
	}

	//[TODO] Clean cache dir
	//[TODO] Verify no MP process running

	if len(errs) > 0 {
		return fmt.Errorf("incomplete cleanup for volume %s (%d errors)", entry.VolumeID, len(errs))
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

// IsMountPoint returns whether the given target is a Mountpoint mount.
func (dm *DaemonsetMounter) IsMountPoint(target string) (bool, error) {
	return dm.mount.CheckMountpoint(target)
}

// IsSourceHealthy checks if the FUSE mount at sourcePath is alive and serving.
// Runs the check in a goroutine bounded by ctx to avoid blocking forever if
// the FUSE daemon is alive but not reading from the fd (frozen).
//
// Uses [mpmounter.Mounter.IsHealthyMountpoint] which returns:
//   - (true, nil):             healthy — live Mountpoint daemon confirmed via IO.
//   - (false, ErrMountAbsent): path absent or not a Mountpoint mount.
//   - (false, nil):            dead/corrupted daemon (ENOTCONN, ESTALE, EIO).
//   - (false, err):            UNKNOWN — transient error; callers MUST NOT treat as dead.
//
// For the source, "absent" means the mount is dead — so ErrMountAbsent is folded into
// (false, nil). Callers get the original three-way contract: healthy / dead / unknown.
func (dm *DaemonsetMounter) IsSourceHealthy(ctx context.Context, sourcePath string) (bool, error) {
	type result struct {
		healthy bool
		err     error
	}
	ch := make(chan result, 1)
	go func() {
		healthy, err := dm.mount.IsHealthyMountpoint(sourcePath)
		if errors.Is(err, mpmounter.ErrMountAbsent) {
			ch <- result{false, nil} // absent source = dead
			return
		}
		ch <- result{healthy, err}
	}()
	select {
	case r := <-ch:
		return r.healthy, r.err
	case <-ctx.Done():
		klog.V(2).Infof("DaemonsetMounter: IsSourceHealthy timed out for %s, treating as unknown", sourcePath)
		return false, ctx.Err()
	}
}

// TargetState represents the health/mount state of a workload target path.
type TargetState int

const (
	// TargetAbsent means the target is not mounted (fresh mount, proceed).
	TargetAbsent TargetState = iota
	// TargetHealthy means the target is mounted and the FUSE daemon is alive.
	TargetHealthy
	// TargetDead means the target is mounted but the FUSE daemon is dead/corrupted.
	TargetDead
)

// CheckTargetState checks if the workload's bind-mount target is a live Mountpoint mount.
// Callers interpret:
//   - (TargetAbsent, nil):  target is absent/fresh — proceed with mount.
//   - (TargetHealthy, nil): target is healthy and mounted (republish/legacy).
//   - (TargetDead, nil):    target mount is dead/corrupted — do NOT proceed (return nil).
//   - (_, err):             UNKNOWN — let kubelet retry.
func (dm *DaemonsetMounter) CheckTargetState(ctx context.Context, target string) (TargetState, error) {
	type result struct {
		state TargetState
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		healthy, err := dm.mount.IsHealthyMountpoint(target)
		if errors.Is(err, mpmounter.ErrMountAbsent) {
			ch <- result{TargetAbsent, nil}
			return
		}
		if err != nil {
			ch <- result{TargetDead, err} // transient/unknown error
			return
		}
		if healthy {
			ch <- result{TargetHealthy, nil}
		} else {
			ch <- result{TargetDead, nil}
		}
	}()
	select {
	case r := <-ch:
		return r.state, r.err
	case <-ctx.Done():
		klog.V(2).Infof("DaemonsetMounter: CheckTargetState timed out for %s, treating as unknown", target)
		return TargetDead, ctx.Err()
	}
}

// BindMount performs a bind mount from source to target, creating the target directory if needed.
func (dm *DaemonsetMounter) BindMount(source, target string) error {
	if err := os.MkdirAll(target, targetDirPerm); err != nil {
		return fmt.Errorf("failed to create bind mount target directory %q: %w", target, err)
	}
	if err := dm.bindMountSyscallWithDefault(source, target); err != nil {
		return fmt.Errorf("failed to bind mount %q → %q: %w", source, target, err)
	}
	klog.V(4).Infof("DaemonsetMounter: bind mounted %s → %s", source, target)
	return nil
}

// bindMountSyscallWithDefault delegates to bindMountSyscall if set, or falls back to dm.mount.BindMount.
func (dm *DaemonsetMounter) bindMountSyscallWithDefault(source, target string) error {
	if dm.bindMountSyscall != nil {
		return dm.bindMountSyscall(source, target)
	}
	return dm.mount.BindMount(source, target)
}
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

// unmountIfMounted checks whether the given path is a mountpoint and only issues an unmount
// if it is currently mounted. This avoids errors from old util-linux versions that return
// EINVAL when unmounting a path that is not a mount point.
// Returns nil if the path is not mounted (intent already satisfied).
func (dm *DaemonsetMounter) unmountIfMounted(target string) error {
	isMounted, err := dm.mount.IsMountPoint(target)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // path doesn't exist — nothing to unmount
		}
		// IsMountPoint can return corrupted mount errors; attempt unmount anyway.
		klog.V(4).Infof("DaemonsetMounter: mount check for %s returned error, attempting unmount: %v", target, err)
	} else if !isMounted {
		return nil // not in mount table — intent already satisfied
	}
	return dm.mount.Unmount(target)
}

// isSystemDMountpoint determines whether the specified target path is a V1 systemd-managed mountpoint.
// A systemd mountpoint has zero bind-mount references to it — systemd mounts directly to the target
// (unlike V2/V3 which bind-mount from a source path, creating references).
func (dm *DaemonsetMounter) isSystemDMountpoint(target string) bool {
	if !util.SupportLegacySystemdMounts() {
		return false
	}

	references, err := dm.mount.FindReferencesToMountpoint(target)
	if err != nil {
		klog.Warningf("DaemonsetMounter: failed to find references to %s for systemd detection: %v", target, err)
		return false
	}

	return len(references) == 0
}

// provideCredentials creates a per-mount credential directory and provisions credentials into it.
func (dm *DaemonsetMounter) provideCredentials(ctx context.Context, commDir, volumeID string, credentialCtx *credentialprovider.ProvideContext) (envprovider.Environment, error) {
	mountCredDir := filepath.Join(commDir, volumeID)
	if err := os.MkdirAll(mountCredDir, credentialprovider.CredentialDirPerm); err != nil {
		return nil, fmt.Errorf("failed to create credential directory %q: %w", mountCredDir, err)
	}
	credentialCtx.WritePath = mountCredDir
	credentialCtx.EnvPath = filepath.Join("/comm", volumeID)
	credentialCtx.MountKind = credentialprovider.MountKindDaemonset

	env, _, err := dm.credProvider.Provide(ctx, *credentialCtx)
	if err != nil {
		return nil, fmt.Errorf("failed to provide credentials for mount %s: %w", volumeID, err)
	}
	return env, nil
}

// cleanupCredentials removes the per-mount credential directory.
func (dm *DaemonsetMounter) cleanupCredentials(commDir, volumeID string, cleanupCtx credentialprovider.CleanupContext) error {
	mountCredDir := filepath.Join(commDir, volumeID)
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

// mountInfoProviderWithDefault delegates to mountInfoProvider if set, or falls back to parseMountInfoFromProc.
func (dm *DaemonsetMounter) mountInfoProviderWithDefault() ([]mountutils.MountInfo, error) {
	if dm.mountInfoProvider != nil {
		return dm.mountInfoProvider()
	}
	return parseMountInfoFromProc()
}

// populateEntryFromMeta creates or retrieves the MountMap entry for the given volume
// and fills it from persisted metadata and mount state.
func (dm *DaemonsetMounter) populateEntryFromMeta(meta *MountMeta, sourcePath string, sourceMounted bool, targets []string) {
	entry, _ := dm.mountMap.GetOrCreate(meta.VolumeID)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	entry.SourcePath = sourcePath
	entry.CommDir = meta.CommDir
	entry.Params = MountParams{
		MountOptions:             meta.MountOptions,
		AuthenticationSource:     meta.AuthenticationSource,
		ServiceAccountName:       meta.ServiceAccountName,
		ServiceAccountEKSRoleARN: meta.ServiceAccountEKSRoleARN,
		PodNamespace:             meta.PodNamespace,
		FSGroup:                  meta.FSGroup,
	}
	entry.RefCount = len(targets)
	entry.Targets = targets
	entry.sourceMounted = sourceMounted
}

// RebuildMountMap reconstructs the MountMap from disk on driver startup.
// It scans the meta directory for .meta.json files, verifies each source mount
// is still alive via /proc/self/mountinfo, and counts bind mounts (targets) by
// matching device IDs.
//
// Algorithm:
//  1. List all .meta.json files in the plugins/s3.csi.aws.com/meta/ directory
//  2. For each meta file, parse the JSON to get MountMeta
//  3. Scan /proc/self/mountinfo to find the source mount and its device ID
//  4. Count bind mounts sharing that device ID (these are the targets)
//  5. Populate MountMap entries with refcount = number of bind mounts
//
// Entries with dead source mounts are skipped (meta file cleaned up).
func (dm *DaemonsetMounter) RebuildMountMap() error {
	metaDir := filepath.Join(dm.kubeletPath, "plugins", "s3.csi.aws.com", "meta")
	entries, err := os.ReadDir(metaDir)
	if err != nil {
		if os.IsNotExist(err) {
			klog.V(4).Info("MountMap: no meta directory found, starting fresh")
			return nil
		}
		return fmt.Errorf("failed to read meta directory %s: %w", metaDir, err)
	}

	// Parse mount table once
	mountInfos, err := dm.mountInfoProviderWithDefault()
	if err != nil {
		return fmt.Errorf("failed to parse /proc/self/mountinfo: %w", err)
	}

	for _, dirEntry := range entries {
		if !strings.HasSuffix(dirEntry.Name(), ".meta.json") {
			continue
		}

		metaPath := filepath.Join(metaDir, dirEntry.Name())
		meta, err := readMeta(metaPath)
		if err != nil {
			klog.Warningf("MountMap: failed to read meta file %s, skipping: %v", metaPath, err)
			continue
		}

		// Derive SourcePath from VolumeID (not persisted, always computable)
		sourcePath := SourceMountPath(dm.kubeletPath, meta.VolumeID)

		// Find the source mount in mountinfo
		sourceMI := findMountByPath(mountInfos, sourcePath)
		if sourceMI == nil {
			// Source mount is gone — full cleanup of all resources before removing meta.
			klog.V(2).Infof("MountMap: source mount %s for volume %s not found in mount table, cleaning up", sourcePath, meta.VolumeID)
			entry := &MountEntry{
				VolumeID:   meta.VolumeID,
				SourcePath: sourcePath,
				CommDir:    meta.CommDir,
			}
			cleanupCtx := credentialprovider.CleanupContext{VolumeID: meta.VolumeID}
			if cleanErr := dm.cleanupMount(entry, cleanupCtx); cleanErr != nil {
				klog.Errorf("MountMap: cleanup for dead volume %s failed: %v (keeping meta and map entry for retry)", meta.VolumeID, cleanErr)
				// Store entry in the map so future NodePublish or periodic cleanup can
				// retry cleanup using the original commDir where credentials were written.
				dm.populateEntryFromMeta(meta, sourcePath, false, nil)
				continue
			}
			os.Remove(metaPath)
			continue
		}

		// Count bind mounts sharing the same device ID (major:minor)
		targets := findBindMountTargets(mountInfos, deviceID(sourceMI), sourcePath)

		dm.populateEntryFromMeta(meta, sourcePath, true, targets)

		klog.V(2).Infof("MountMap: recovered volume %s with %d targets from mount table", meta.VolumeID, len(targets))
	}

	return nil
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
func (dm *DaemonsetMounter) waitForMount(parentCtx context.Context, target, volumeID, errFilePath string) error {
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
			mountResultCh <- fmt.Errorf("Mountpoint for mount %s failed: %s", volumeID, string(content))
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
			mountResultCh <- fmt.Errorf("timed out waiting for Mountpoint to serve mount %s at %s. %s", volumeID, target, helpMessageForGettingMounterLogs())
		} else {
			mountResultCh <- nil
		}
	}()

	return <-mountResultCh
}
