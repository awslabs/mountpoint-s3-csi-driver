package mounter

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/volumecontext"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
)

const (
	// CacheVolumeName is the /cache directory mounted inside the mounter pod.
	CacheVolumeName = "cache"

	//	emptyDir   <mounterDir>/volumes/kubernetes.io~empty-dir/cache
	//	ephemeral  <mounterDir>/volumes/kubernetes.io~csi/<bound PV>/mount
	volumesSubdir         = "volumes"
	emptyDirVolumesSubdir = "kubernetes.io~empty-dir"
	csiVolumesSubdir      = "kubernetes.io~csi"

	// TODO: Remove to use process isolation PR defined permissions.
	cacheDirPerm = fs.FileMode(0770)
)

// V2 Per-mount cache attributes that have NO EFFECT in daemonset mode (only warning).
var otherCacheVolumeAttributes = []string{
	volumecontext.CacheEmptyDirSizeLimit,
	volumecontext.CacheEphemeralStorageClassName,
	volumecontext.CacheEphemeralStorageResourceRequest,
}

// CacheType is the mounter pod's cache type, also shown in errors and logs. It distinguishes between:
// Type "emptyDir" Medium "" (disk), Type "emptyDir" Medium "Memory" (tmpfs), and Type "ephemeral".
type CacheType string

const (
	CacheNone           CacheType = "no cache"
	CacheEmptyDirDisk   CacheType = "emptyDir"
	CacheEmptyDirMemory CacheType = "emptyDir (medium Memory)"
	CacheEphemeral      CacheType = "ephemeral"
)

// cacheTypeFromPod returns the cache type the mounter pod's spec declares, or CacheNone if it has
// no cache volume.
func cacheTypeFromPod(pod *corev1.Pod) CacheType {
	for _, v := range pod.Spec.Volumes {
		// Skip other volumes on mounter pod (e.g. commDir)
		if v.Name != CacheVolumeName {
			continue
		}
		switch {
		case v.EmptyDir != nil:
			// Both ""/"Memory" mediums share one path on the node, so identify from spec.
			if v.EmptyDir.Medium == corev1.StorageMediumMemory {
				return CacheEmptyDirMemory
			}
			return CacheEmptyDirDisk
		case v.Ephemeral != nil:
			return CacheEphemeral
		}
	}
	return CacheNone
}

// MountOptionCacheDir returns a mount's cache directory as Mountpoint sees it, passed to `--cache`.
func MountOptionCacheDir(volumeID string) string {
	return filepath.Join("/", CacheVolumeName, volumeID)
}

// resolveCacheDir returns the mounter pod's cache volume on the node, or "" when it has none.
// With mounterDir at <kubelet>/pods/<mounterUID>, the two cache types land in:
//
//	emptyDir   <mounterDir>/volumes/kubernetes.io~empty-dir/cache       (can be constructed)
//	ephemeral  <mounterDir>/volumes/kubernetes.io~csi/<bound PV>/mount  (read from the mounter's cache claim)
func (dm *DaemonsetMounter) resolveCacheDir(ctx context.Context, pod *corev1.Pod, mounterDir string, cacheType CacheType) (string, error) {
	switch cacheType {
	case CacheNone:
		return "", nil

	case CacheEmptyDirDisk, CacheEmptyDirMemory:
		// Both mediums share one path, which is why only the pod spec can tell them apart.
		return filepath.Join(mounterDir, volumesSubdir, emptyDirVolumesSubdir, CacheVolumeName), nil

	case CacheEphemeral:
		// A generic ephemeral volume's claim is always named <pod>-<volume>, so the bound PV name can
		// be read off it and the path constructed. For mounter pod s3-csi-daemonset-mounter-abcde:
		//
		//	claim  kube-system/s3-csi-daemonset-mounter-abcde-cache
		//	  .spec.volumeName  pvc-9f3c1a2b
		//	  path  /var/lib/kubelet/pods/<mounterUID>/volumes/kubernetes.io~csi/pvc-9f3c1a2b/mount
		claimName := pod.Name + "-" + CacheVolumeName
		claim, err := dm.clientset.CoreV1().PersistentVolumeClaims(pod.Namespace).Get(ctx, claimName, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("failed to get cache volume claim %s/%s: %w", pod.Namespace, claimName, err)
		}
		if claim.Spec.VolumeName == "" {
			return "", fmt.Errorf("cache volume claim %s/%s is not bound", pod.Namespace, claimName)
		}
		return filepath.Join(mounterDir, volumesSubdir, csiVolumesSubdir, claim.Spec.VolumeName, "mount"), nil
	}

	// A CacheType this build does not know
	return "", fmt.Errorf("unknown cache type %q on s3-csi-daemonset-mounter", cacheType)
}

// createCacheDir creates a mount's cache directory on the node. Mountpoint requires it to exist
// before it starts, as it only creates its own `mountpoint-cache` directory inside it.
func createCacheDir(cacheDir, volumeID string) error {
	// Defensive only: resolveCacheDir returns "" only for CacheNone, which configureCache already rejected.
	if cacheDir == "" {
		return fmt.Errorf("s3-csi-daemonset-mounter has no cache volume for volume %s", volumeID)
	}

	mountCacheDir := filepath.Join(cacheDir, volumeID)
	if err := os.Mkdir(mountCacheDir, cacheDirPerm); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("failed to create cache directory %q for volume %s: %w", mountCacheDir, volumeID, err)
	}
	// Mkdir tolerates EEXIST and Chmod follows symlinks, so without Lstat a symlink planted here
	// by a Mountpoint process (the volume root is group-writable) would redirect the Chmod below.
	if fi, err := os.Lstat(mountCacheDir); err != nil {
		return fmt.Errorf("failed to stat cache directory %q for volume %s: %w", mountCacheDir, volumeID, err)
	} else if !fi.Mode().IsDir() {
		return fmt.Errorf("cache directory %q for volume %s is not a directory (mode %s)", mountCacheDir, volumeID, fi.Mode())
	}
	// Mkdir subtracts the umask, which typically clears the group write bit Mountpoint needs.
	if err := os.Chmod(mountCacheDir, cacheDirPerm); err != nil {
		return fmt.Errorf("failed to set permissions on cache directory %q for volume %s: %w", mountCacheDir, volumeID, err)
	}

	klog.V(4).Infof("DaemonsetMounter: created cache directory %s for volume %s", mountCacheDir, volumeID)
	return nil
}

// removeCacheDir removes a mount's cache directory. Mountpoint removes its own cache when it exits
// cleanly, so this covers the cases it misses, such as being killed.
func removeCacheDir(cacheDir, volumeID string) error {
	// volumeID is a PV name from the kubelet's target path, so these cannot occur in practice.
	// Guarded anyway because this is a recursive delete and the volume root is shared by every
	// mount on the node: `..` would escape it, and a separator would leave the volume entirely.
	if cacheDir == "" || volumeID == "" || volumeID == "." || volumeID == ".." || strings.ContainsRune(volumeID, filepath.Separator) {
		return nil
	}
	return os.RemoveAll(filepath.Join(cacheDir, volumeID))
}

// configureCache decides whether this mount caches, and rejects a PV whose requested
// cache type is not the one the mounter pod has.
func configureCache(args *mountpoint.Args, volumeCtx map[string]string, volumeID string, mounterCacheType CacheType) error {
	cacheType := volumeCtx[volumecontext.Cache]
	cacheEnabledViaOptions := args.Has(mountpoint.ArgCache)

	// Reject if cache configured with both mountOptions / volumeAttributes (match v2).
	if cacheEnabledViaOptions && cacheType != "" {
		return status.Error(codes.InvalidArgument,
			"Cache configured with both `mountOptions` and `volumeAttributes`, please remove the deprecated cache configuration in `mountOptions`")
	}

	// Mountpoint rejects `--max-cache-size` without `--cache` - strip max-cache-size with warning.
	if !cacheEnabledViaOptions && cacheType == "" {
		if _, ok := args.Remove(mountpoint.ArgMaxCacheSize); ok {
			klog.Warningf("NodePublishVolume: volume %s sets %s but does not enable the cache, ignoring it."+
				" Set the %q volume attribute to enable the cache.",
				volumeID, mountpoint.ArgMaxCacheSize, volumecontext.Cache)
		}
		return nil
	}

	// After 'not both' and 'not neither' are eliminated, cache is set by either mountOptions / volume attributes.
	// Return error if mounter pod has no cache volumes.
	if mounterCacheType == CacheNone {
		return status.Errorf(codes.InvalidArgument,
			"Volume %s requests a local cache, but s3-csi-daemonset-mounter has no cache volume."+
				" Add a daemonsetMounters[0].cache block and restart its pods", volumeID)
	}

	// If enabled via mountOptions, warn + use mounter pod's cache directory
	if cacheEnabledViaOptions {
		klog.Warningf("NodePublishVolume: volume %s configures the cache via the deprecated `cache`"+
			" mount option, so it accepts the node's %s cache. Use the %q volume attribute instead,"+
			" set to the mounter's enabled cache type.",
			volumeID, mounterCacheType, volumecontext.Cache)
	} else if err := checkCacheTypeMatches(volumeCtx, volumeID, mounterCacheType); err != nil {
		// if enabled via volumeAttributes, reject if cache type doesn't match mounter pod's cache type.
		return err
	}

	// Discard PV supplied path and cache dir path.
	args.Set(mountpoint.ArgCache, MountOptionCacheDir(volumeID))

	// For unused volumeAttributes (v2 only) - warn user and prompt updating to v3 PVs.
	for _, attr := range otherCacheVolumeAttributes {
		if volumeCtx[attr] != "" {
			klog.Warningf("NodePublishVolume: volume %s sets %q, which has no effect in v3."+
				" The cache volume is shared by every mount on the node and is configured with the"+
				" daemonsetMounters[0].cache Helm value.",
				volumeID, attr)
		}
	}

	// TODO: For emptyDir disk case, find approach to limit max cache size with/without sizeLimit to prevent
	// resource exhaustion (e.g. --max-cache-size). Currently Mountpoint stops writing cache once cache filesystem
	// has less than 5% free, which works correctly for emptyDir:Memory (tmpfs) and ephemeral. But emptyDir disk case
	// Mountpoint's statvfs reads the node's root filesystem stats instead, so we cannot rely on this check.
	// Temp: warn user if cache is enabled but no max cache size is set (remove when TODO addressed).
	if !args.Has(mountpoint.ArgMaxCacheSize) {
		klog.Warningf("NodePublishVolume: volume %s enables the cache without %s, so it may use the"+
			" whole cache volume of s3-csi-daemonset-mounter.", volumeID, mountpoint.ArgMaxCacheSize)
	}

	return nil
}

// checkCacheTypeMatches rejects a PV whose requested cache type is not the one this node has.
func checkCacheTypeMatches(volumeCtx map[string]string, volumeID string, mounterCacheType CacheType) error {
	requested, err := cacheTypeFromPV(volumeCtx[volumecontext.Cache], volumeCtx[volumecontext.CacheEmptyDirMedium])
	if err != nil {
		return status.Errorf(codes.InvalidArgument,
			"Volume %s sets %q: %v. It must also match the mounter pod's cache, which is %s",
			volumeID, volumecontext.Cache, err, mounterCacheType)
	}

	if requested != mounterCacheType {
		// Both remediations, since only the operator knows which of the two applies.
		return status.Errorf(codes.InvalidArgument,
			"Volume %s requests a %s cache, but s3-csi-daemonset-mounter on this node provides %s."+
				" Set the volume's %q attribute to match this node, or schedule it onto a node whose"+
				" cache type matches.",
			volumeID, requested, mounterCacheType, volumecontext.Cache)
	}
	return nil
}

// cacheTypeFromPV returns a PV's cache type and cacheEmptyDirMedium attributes name.
func cacheTypeFromPV(cacheType, emptyDirMedium string) (CacheType, error) {
	switch cacheType {
	case "":
		return CacheNone, nil
	case volumecontext.CacheTypeEphemeral:
		return CacheEphemeral, nil // note: ignore medium which is for emptyDir only
	case volumecontext.CacheTypeEmptyDir:
		switch emptyDirMedium {
		case "":
			return CacheEmptyDirDisk, nil
		case string(corev1.StorageMediumMemory):
			return CacheEmptyDirMemory, nil
		default:
			// "HugePages" mediums are not supported - they're backed by hugetlbfs, which Mountpoint cannot write cache blocks into.
			return CacheNone, fmt.Errorf("unsupported emptyDir medium %q, must be %q or %q",
				emptyDirMedium, "", corev1.StorageMediumMemory)
		}
	default:
		return CacheNone, fmt.Errorf("%q is not a cache type, must be %q or %q",
			cacheType, volumecontext.CacheTypeEmptyDir, volumecontext.CacheTypeEphemeral)
	}
}
