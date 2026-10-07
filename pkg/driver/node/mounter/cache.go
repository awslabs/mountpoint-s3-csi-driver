package mounter

import (
	"slices"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/volumecontext"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
)

const (
	// CacheVolumeName is the /cache directory mounted inside the mounter pod.
	CacheVolumeName = "cache"
)

// mounterPodHasCacheVolume reports whether a mounter pod mounts the cache volume its Mountpoints cache in.
// Note: we may need to expand this function to get exact cache type for v2 style PV compatibility validations.
func mounterPodHasCacheVolume(pod corev1.Pod) bool {
	return slices.ContainsFunc(pod.Spec.Volumes, func(v corev1.Volume) bool { return v.Name == CacheVolumeName })
}

// validateCacheRequest returns whether this mount caches, or an InvalidArgument error for a cache request it cannot serve.
func validateCacheRequest(args mountpoint.Args, volumeCtx map[string]string, volumeID string, mounterHasCache bool) (caches bool, err error) {
	pvCache := volumeCtx[volumecontext.Cache]
	cacheViaMountOptions := args.Has(mountpoint.ArgCache)

	// Reject a cache in both mountOptions and volumeAttributes, even `cache: disabled`, to match v2.
	if cacheViaMountOptions && pvCache != "" {
		return false, status.Error(codes.InvalidArgument,
			"Cache configured with both `mountOptions` and `volumeAttributes`, please remove the deprecated cache configuration in `mountOptions`")
	}

	switch {
	case cacheViaMountOptions:
		klog.Warningf("NodePublishVolume: volume %s enables the cache via the deprecated `cache` mount option,"+
			" so its path is ignored. Remove it from mountOptions and set the %q volume attribute to %q instead.",
			volumeID, volumecontext.Cache, volumecontext.CacheEnabled)
	case pvCache == "", strings.EqualFold(pvCache, volumecontext.CacheDisabled):
		return false, nil
	case strings.EqualFold(pvCache, volumecontext.CacheEnabled),
		pvCache == volumecontext.CacheTypeEmptyDir, pvCache == volumecontext.CacheTypeEphemeral:
		// The valid cache values all enable caching.
	default:
		return false, status.Errorf(codes.InvalidArgument,
			"Volume %s sets the %q volume attribute to %q. Set it to %q to use this node's cache, or %q for none.",
			volumeID, volumecontext.Cache, pvCache, volumecontext.CacheEnabled, volumecontext.CacheDisabled)
	}

	if !mounterHasCache {
		return false, status.Errorf(codes.InvalidArgument,
			"Volume %s requests a local cache, but s3-csi-daemonset-mounter has no cache volume."+
				" Add a daemonsetMounters[0].cache block to the Helm values and restart its pods.", volumeID)
	}

	return true, nil
}
