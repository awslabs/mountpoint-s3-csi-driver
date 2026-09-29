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

// resolveCacheDir returns where the mounter pod's cache volume lives on the node, or "" if it has none.
// With mounterDir at <kubelet>/pods/<mounterUID>, the two volume kinds land in:
//
//	emptyDir   <mounterDir>/volumes/kubernetes.io~empty-dir/cache       (can be constructed)
//	ephemeral  <mounterDir>/volumes/kubernetes.io~csi/pvc-<uid>/mount   (read from the mounter's cache PVC)
func resolveCacheDir(ctx context.Context, clientset kubernetes.Interface, pod *corev1.Pod, mounterDir string) (string, error) {
	for _, v := range pod.Spec.Volumes {
		// Skip other volumes on mounter pod (e.g. commDir)
		if v.Name != CacheVolumeName {
			continue
		}
		// Note we could return early as "Names must be unique across all API versions of the same resource."
		switch {
		case v.EmptyDir != nil:
			// Disk and "Memory" (tmpfs) mediums share one path on the node.
			return filepath.Join(mounterDir, volumesSubdir, emptyDirVolumesSubdir, CacheVolumeName), nil

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
				return "", fmt.Errorf("failed to get cache volume PVC %s/%s: %w", pod.Namespace, pvcName, err)
			}
			if pvc.Spec.VolumeName == "" {
				return "", fmt.Errorf("cache volume PVC %s/%s is not bound", pod.Namespace, pvcName)
			}
			return filepath.Join(mounterDir, volumesSubdir, csiVolumesSubdir, pvc.Spec.VolumeName, "mount"), nil
		}
	}
	return "", nil
}

// configureCache decides whether this mount caches, rejects a cache request the mounter pod cannot
// serve, and points `--cache` at the mount's directory.
func configureCache(args *mountpoint.Args, volumeCtx map[string]string, volumeID string, mounterCacheDir string) error {
	pvCache := volumeCtx[volumecontext.Cache]
	cacheViaMountOptions := args.Has(mountpoint.ArgCache)

	// Reject a cache in both mountOptions and volumeAttributes, even `cache: false`, to match v2.
	if cacheViaMountOptions && pvCache != "" {
		return status.Error(codes.InvalidArgument,
			"Cache configured with both `mountOptions` and `volumeAttributes`, please remove the deprecated cache configuration in `mountOptions`")
	}

	switch {
	case cacheViaMountOptions:
		klog.Warningf("NodePublishVolume: volume %s enables the cache via the deprecated `cache` mount option,"+
			" so its path is ignored. Remove it from mountOptions and set the %q volume attribute to %q instead.",
			volumeID, volumecontext.Cache, volumecontext.CacheEnabled)
	case pvCache == "", strings.EqualFold(pvCache, volumecontext.CacheDisabled):
		return nil
	case strings.EqualFold(pvCache, volumecontext.CacheEnabled),
		pvCache == volumecontext.CacheTypeEmptyDir, pvCache == volumecontext.CacheTypeEphemeral:
		// The 3 valid cache values all enable caching.
	default:
		return status.Errorf(codes.InvalidArgument,
			"Volume %s sets the %q volume attribute to %q. Set it to %q to use this node's cache, or %q for none.",
			volumeID, volumecontext.Cache, pvCache, volumecontext.CacheEnabled, volumecontext.CacheDisabled)
	}

	if mounterCacheDir == "" {
		return status.Errorf(codes.InvalidArgument,
			"Volume %s requests a local cache, but s3-csi-daemonset-mounter has no cache volume."+
				" Add a daemonsetMounters[0].cache block to the Helm values and restart its pods.", volumeID)
	}

	// Discard PV supplied path and use PV subdirectory in the mounter's cache directory.
	args.Set(mountpoint.ArgCache, MountOptionCacheDir(volumeID))
	return nil
}

// MountOptionCacheDir returns a mount's cache directory as Mountpoint sees it, passed to `--cache`.
func MountOptionCacheDir(volumeID string) string {
	return filepath.Join("/", CacheVolumeName, volumeID)
}

// createCacheDir creates a mount's cache directory on the node. Mountpoint requires it to exist
// before it starts, as it only creates its own `mountpoint-cache` directory inside it.
func createCacheDir(cacheDir, volumeID string) error {
	// Defensive only: resolveCacheDir returns "" only when the mounter has no cache volume, which configureCache rejects.
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
