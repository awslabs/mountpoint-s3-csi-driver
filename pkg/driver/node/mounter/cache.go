package mounter

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
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

// deprecatedCacheVolumeAttributes are v2's per-PV cache volume settings.
var deprecatedCacheVolumeAttributes = []string{
	volumecontext.CacheEmptyDirMedium,
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

const (
	bytesPerMiB            = 1024 * 1024
	cacheSizeMarginPercent = 95 // Mountpoint evicts cache only after cache write, so we leave some headroom
)

// resolveCacheTypeAndDir returns the cache type the mounter pod's spec declares and where that
// cache lives on the node, or (CacheNone, "") if it has no cache volume.
// With mounterDir at <kubelet>/pods/<mounterUID>, the two cache types land in:
//
//	emptyDir   <mounterDir>/volumes/kubernetes.io~empty-dir/cache       (can be constructed)
//	ephemeral  <mounterDir>/volumes/kubernetes.io~csi/pvc-<uid>/mount   (read from the mounter's cache PVC)
func resolveCacheTypeAndDir(ctx context.Context, clientset kubernetes.Interface, pod *corev1.Pod, mounterDir string) (CacheType, string, error) {
	for _, v := range pod.Spec.Volumes {
		// Skip other volumes on mounter pod (e.g. commDir)
		if v.Name != CacheVolumeName {
			continue
		}
		// Note we could return early as "Names must be unique across all API versions of the same resource."
		switch {
		case v.EmptyDir != nil:
			// Both ""/"Memory" mediums share one path on the node, so identify from spec.
			dir := filepath.Join(mounterDir, volumesSubdir, emptyDirVolumesSubdir, CacheVolumeName)
			if v.EmptyDir.Medium == corev1.StorageMediumMemory {
				return CacheEmptyDirMemory, dir, nil
			}
			return CacheEmptyDirDisk, dir, nil

		case v.Ephemeral != nil:
			// A generic ephemeral volume's PVC is always named <pod>-<volume>, so the bound PV name can
			// be read off it and the path constructed. For mounter pod s3-csi-daemonset-mounter-abcde:
			//
			//	PVC  				kube-system/s3-csi-daemonset-mounter-abcde-cache
			//	.spec.volumeName  	pvc-9f3c1a2b
			//	path  				/var/lib/kubelet/pods/<mounterUID>/volumes/kubernetes.io~csi/pvc-9f3c1a2b/mount
			pvcName := pod.Name + "-" + CacheVolumeName
			pvc, err := clientset.CoreV1().PersistentVolumeClaims(pod.Namespace).Get(ctx, pvcName, metav1.GetOptions{})
			if err != nil {
				return CacheNone, "", fmt.Errorf("failed to get cache volume PVC %s/%s: %w", pod.Namespace, pvcName, err)
			}
			if pvc.Spec.VolumeName == "" {
				return CacheNone, "", fmt.Errorf("cache volume PVC %s/%s is not bound", pod.Namespace, pvcName)
			}
			return CacheEphemeral, filepath.Join(mounterDir, volumesSubdir, csiVolumesSubdir, pvc.Spec.VolumeName, "mount"), nil
		}
	}
	return CacheNone, "", nil
}

// configureCache decides whether this mount caches, and rejects a PV whose requested cache settings the mounter pod cannot
// serve, and sets mount option Args for the cache directory. It warns for every deprecated v1 or v2 cache setting the PV uses.
func configureCache(args *mountpoint.Args, volumeCtx map[string]string, volumeID string, mounterCacheType CacheType) error {
	// Deferred so it prints after the other warnings.
	defer warnDeprecatedCacheVolumeAttributes(volumeCtx, volumeID)

	pvCache := volumeCtx[volumecontext.Cache]
	cacheViaMountOptions := args.Has(mountpoint.ArgCache)

	// Reject a cache in both mountOptions and volumeAttributes, even `cache: false`, to match v2.
	if cacheViaMountOptions && pvCache != "" {
		return status.Error(codes.InvalidArgument,
			"Cache configured with both `mountOptions` and `volumeAttributes`, please remove the deprecated cache configuration in `mountOptions`")
	}

	v2PvCacheType, err := cacheTypeFromPV(pvCache, volumeCtx[volumecontext.CacheEmptyDirMedium])
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "Volume %s has an invalid cache setting: %v", volumeID, err)
	}
	cacheViaV3 := pvCache == volumecontext.CacheEnabled
	cacheViaV2 := v2PvCacheType != CacheNone

	// Cache not enabled, exit early.
	if !cacheViaMountOptions && !cacheViaV3 && !cacheViaV2 {
		// Mountpoint refuses `--max-cache-size` without `--cache`, so reject to match v2.
		if args.Has(mountpoint.ArgMaxCacheSize) {
			return status.Errorf(codes.InvalidArgument,
				"Volume %s sets %s in its mountOptions but does not enable the cache. Set the %q volume"+
					" attribute to %q, or remove %s.",
				volumeID, mountpoint.ArgMaxCacheSize, volumecontext.Cache, volumecontext.CacheEnabled, mountpoint.ArgMaxCacheSize)
		}
		return nil
	}

	// Now we know cache is requested by PV - reject if the mounter pod cannot serve it.
	if mounterCacheType == CacheNone {
		return status.Errorf(codes.InvalidArgument,
			"Volume %s requests a local cache, but s3-csi-daemonset-mounter has no cache volume."+
				" Add a daemonsetMounters[0].cache block to the Helm values and restart its pods.", volumeID)
	}
	// TODO: Also reject a cacheEphemeralStorageClassName that differs from the mounter's cache PVC storage class.
	// (low probability of user encountering this user case but not 0; we might not want to incur this complexity)
	if cacheViaV2 && v2PvCacheType != mounterCacheType {
		return status.Errorf(codes.InvalidArgument,
			"Volume %s requests an %s cache, but s3-csi-daemonset-mounter on this node provides %s."+
				" Set %q to %q to use this node's cache, or schedule the pod onto a node whose"+
				" cache type matches.",
			volumeID, v2PvCacheType, mounterCacheType, volumecontext.Cache, volumecontext.CacheEnabled)
	}

	if cacheViaMountOptions {
		klog.Warningf("NodePublishVolume: volume %s enables the cache via the deprecated `cache` mount option,"+
			" so its path is ignored and the volume uses this node's %s cache. Remove it from mountOptions and"+
			" set the %q volume attribute to %q instead.",
			volumeID, mounterCacheType, volumecontext.Cache, volumecontext.CacheEnabled)
	}
	if cacheViaV2 {
		klog.Warningf("NodePublishVolume: volume %s names its cache type with the deprecated %q value %q,"+
			" which must match this node's cache. Set %q to %q to use whichever cache this node has.",
			volumeID, volumecontext.Cache, pvCache, volumecontext.Cache, volumecontext.CacheEnabled)
	}

	// Discard PV supplied path and cache dir path.
	args.Set(mountpoint.ArgCache, MountOptionCacheDir(volumeID))

	setMaxCacheSizeFromDeprecatedPVSizes(args, volumeCtx, volumeID, mounterCacheType)
	return nil
}

// warnDeprecatedCacheVolumeAttributes warns once, naming every v2 cache volume attribute the PV sets.
func warnDeprecatedCacheVolumeAttributes(volumeCtx map[string]string, volumeID string) {
	var deprecated []string
	for _, attr := range deprecatedCacheVolumeAttributes {
		if volumeCtx[attr] != "" {
			deprecated = append(deprecated, attr)
		}
	}
	if len(deprecated) > 0 {
		klog.Warningf("NodePublishVolume: volume %s sets the deprecated volume attributes %q. Remove them:"+
			" the daemonsetMounters[0].cache Helm value now configures the cache, and a PV opts in with %q set to %q.",
			volumeID, deprecated, volumecontext.Cache, volumecontext.CacheEnabled)
	}
}

// MountOptionCacheDir returns a mount's cache directory as Mountpoint sees it, passed to `--cache`.
func MountOptionCacheDir(volumeID string) string {
	return filepath.Join("/", CacheVolumeName, volumeID)
}

// setMaxCacheSizeFromDeprecatedPVSizes bounds the cache if max-cache-size mountOptions is not
// provided, as a final fallback to match v2 behaviour.
func setMaxCacheSizeFromDeprecatedPVSizes(args *mountpoint.Args, volumeCtx map[string]string, volumeID string, mounterCacheType CacheType) {
	var sizeAttr string

	// We inject ONLY the attribute for the mounter pod's cache type (emptyDir vs ephemeral, and its medium), the other is ignored.
	// TODO: see both todo comments below; we might also want to simplify by NOT injecting for safety for any mountpoint-pod volume sizes,
	// and instead ask user to bound max-cache-size before v2 to v3 upgrade or use equalSplit strategy instead.
	switch mounterCacheType {
	case CacheEmptyDirDisk, CacheEmptyDirMemory:
		// TODO: Evaluate whether a size limit written for the other medium should still bound the cache.
		if pvType, err := cacheTypeFromPV(volumecontext.CacheTypeEmptyDir, volumeCtx[volumecontext.CacheEmptyDirMedium]); err != nil || pvType != mounterCacheType {
			return
		}
		sizeAttr = volumecontext.CacheEmptyDirSizeLimit
	case CacheEphemeral:
		// TODO: Evaluate if we want to inject for cacheEphemeralStorageClassName that differs from the mounter's cache PVC storage class.
		sizeAttr = volumecontext.CacheEphemeralStorageResourceRequest
	default: // CacheNone only, which configureCache rejected before calling.
		return
	}

	pvSize := volumeCtx[sizeAttr]
	// mountOptions max-cache-size flag has priority, and if unset we also skip.
	if pvSize == "" || args.Has(mountpoint.ArgMaxCacheSize) {
		return
	}

	quantity, err := resource.ParseQuantity(pvSize)
	if err != nil {
		klog.Warningf("NodePublishVolume: volume %s sets the %q volume attribute to %q, which is not a"+
			" Kubernetes quantity, so the driver injects no %s from it. Under cacheLimitStrategy=none, set %s"+
			" in its mountOptions instead.",
			volumeID, sizeAttr, pvSize, mountpoint.ArgMaxCacheSize, mountpoint.ArgMaxCacheSize)
		return
	}

	// Multiply BEFORE dividing as every integer division floors
	boundMiB := quantity.Value() * cacheSizeMarginPercent / 100 / bytesPerMiB

	args.Set(mountpoint.ArgMaxCacheSize, strconv.FormatInt(boundMiB, 10))
	// This node does not know the mounter's cacheLimitStrategy, so the advice names both.
	klog.Warningf("NodePublishVolume: volume %s sets no %s in its mountOptions, so the driver injects %d MiB,"+
		" %d%% of its deprecated %q volume attribute (%q). Under cacheLimitStrategy=none, set %s in its"+
		" mountOptions instead; under equalSplit, s3-csi-daemonset-mounter replaces it with an equal share.",
		volumeID, mountpoint.ArgMaxCacheSize, boundMiB, cacheSizeMarginPercent,
		sizeAttr, pvSize, mountpoint.ArgMaxCacheSize)
}

// cacheTypeFromPV returns the cache type a PV's `cache` and `cacheEmptyDirMedium` attributes name.
// cache set to `true` / `false`, or not set, all name CacheNone (caller checks CacheEnabled separately).
func cacheTypeFromPV(pvCache, pvCacheEmptyDirMedium string) (CacheType, error) {
	switch pvCache {
	case "", volumecontext.CacheDisabled, volumecontext.CacheEnabled: // caller should check CacheEnabled separately
		return CacheNone, nil
	case volumecontext.CacheTypeEphemeral:
		return CacheEphemeral, nil // note: ignore medium which is for emptyDir only
	case volumecontext.CacheTypeEmptyDir:
		switch pvCacheEmptyDirMedium {
		case "":
			return CacheEmptyDirDisk, nil
		case string(corev1.StorageMediumMemory):
			return CacheEmptyDirMemory, nil
		default:
			// "HugePages" mediums are not supported - they're backed by hugetlbfs, which Mountpoint cannot write cache blocks into.
			return CacheNone, fmt.Errorf("%q %q is not supported. Leave it unset, or set it to %q",
				volumecontext.CacheEmptyDirMedium, pvCacheEmptyDirMedium, corev1.StorageMediumMemory)
		}
	default:
		// Refused rather than warned: a near miss like "ture" would otherwise serve the mount uncached silently.
		return CacheNone, fmt.Errorf("%q is not a supported cache value. Use %q for this node's cache,"+
			" %q for no cache, or the deprecated %q or %q",
			pvCache, volumecontext.CacheEnabled, volumecontext.CacheDisabled,
			volumecontext.CacheTypeEmptyDir, volumecontext.CacheTypeEphemeral)
	}
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
	// by a Mountpoint process would redirect the Chmod below. So we use os.Lstat instead of os.stat
	// to also check fi.Mode() is not ModeSymlink (not mode Lrwxrwxrwx).
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
	// We guard escapes and partial cache delete attempts by checking the cacheDir and volumeID, and returning error if something is wrong.
	// volumeID is a PV name from the kubelet's target path; so only corrupted meta path could lead to these problems.
	if cacheDir == "" || volumeID == "" || volumeID == "." || volumeID == ".." || strings.ContainsRune(volumeID, filepath.Separator) {
		return fmt.Errorf("refusing to remove cache directory %q for volume %q: not a cache volume and a plain directory name", cacheDir, volumeID)
	}
	return os.RemoveAll(filepath.Join(cacheDir, volumeID))
}
