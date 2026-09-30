package mounter

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

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

// cacheDirForMount returns the cache volume root this mount caches under, "" when it does not cache,
// or an error for a cache request the mounter pod cannot serve.
func cacheDirForMount(args mountpoint.Args, volumeCtx map[string]string, volumeID string, mounterCacheDir string) (string, error) {
	pvCache := volumeCtx[volumecontext.Cache]
	cacheViaMountOptions := args.Has(mountpoint.ArgCache)

	// Reject a cache in both mountOptions and volumeAttributes, even `cache: disabled`, to match v2.
	if cacheViaMountOptions && pvCache != "" {
		return "", status.Error(codes.InvalidArgument,
			"Cache configured with both `mountOptions` and `volumeAttributes`, please remove the deprecated cache configuration in `mountOptions`")
	}

	switch {
	case cacheViaMountOptions:
		klog.Warningf("NodePublishVolume: volume %s enables the cache via the deprecated `cache` mount option,"+
			" so its path is ignored. Remove it from mountOptions and set the %q volume attribute to %q instead.",
			volumeID, volumecontext.Cache, volumecontext.CacheEnabled)
	case pvCache == "", strings.EqualFold(pvCache, volumecontext.CacheDisabled):
		return "", nil
	case strings.EqualFold(pvCache, volumecontext.CacheEnabled),
		pvCache == volumecontext.CacheTypeEmptyDir, pvCache == volumecontext.CacheTypeEphemeral:
		// The valid cache values all enable caching.
	default:
		return "", status.Errorf(codes.InvalidArgument,
			"Volume %s sets the %q volume attribute to %q. Set it to %q to use this node's cache, or %q for none.",
			volumeID, volumecontext.Cache, pvCache, volumecontext.CacheEnabled, volumecontext.CacheDisabled)
	}

	if mounterCacheDir == "" {
		return "", status.Errorf(codes.InvalidArgument,
			"Volume %s requests a local cache, but s3-csi-daemonset-mounter has no cache volume."+
				" Add a daemonsetMounters[0].cache block to the Helm values and restart its pods.", volumeID)
	}

	return mounterCacheDir, nil
}

// MountOptionCacheDir returns a mount's cache directory as Mountpoint sees it, passed to `--cache`.
func MountOptionCacheDir(volumeID string) string {
	return filepath.Join("/", CacheVolumeName, volumeID)
}

// createCacheDir creates a fresh cache directory for a mount on the node. Mountpoint requires it to exist
// before it starts, as it only creates its own `mountpoint-cache` directory inside it.
func createCacheDir(cacheDir, volumeID string) error {
	// Defensive only: resolveCacheDir returns "" only when the mounter has no cache volume, which cacheDirForMount rejects.
	if cacheDir == "" {
		return fmt.Errorf("s3-csi-daemonset-mounter has no cache volume for volume %s", volumeID)
	}

	// Start empty rather than trust a leftover, whose contents may belong to another UID, or anything planted in its place.
	// RemoveAll does not follow symlinks.
	if err := removeCacheDir(cacheDir, volumeID); err != nil {
		return fmt.Errorf("failed to clear cache directory for volume %s: %w", volumeID, err)
	}
	mountCacheDir := filepath.Join(cacheDir, volumeID)
	// Locking /cache to rot:root 0711 will prevvent other users
	if err := os.Mkdir(mountCacheDir, cacheDirPerm); err != nil {
		return fmt.Errorf("failed to create cache directory %q for volume %s: %w", mountCacheDir, volumeID, err)
	}
	// Chmod through a handle opened without following symlinks, so swapping the name cannot redirect it.
	dir, err := os.OpenFile(mountCacheDir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("failed to open cache directory %q for volume %s: %w", mountCacheDir, volumeID, err)
	}
	defer dir.Close()
	// Mkdir subtracts the umask, which typically clears the group write bit Mountpoint needs.
	if err := dir.Chmod(cacheDirPerm); err != nil {
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
