package mounter

import (
	"fmt"
	"slices"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/volumecontext"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
)

const (
	// CacheVolumeName is the name of the mounter pod's cache volume, which the chart mounts at /cache.
	CacheVolumeName = "cache"
)

// deprecatedVolumeAttributes are v2 PV settings for the cache volume in Mountpoint pods.
var deprecatedVolumeAttributes = []string{
	volumecontext.CacheEmptyDirMedium,
	volumecontext.CacheEmptyDirSizeLimit,
	volumecontext.CacheEphemeralStorageClassName,
	volumecontext.CacheEphemeralStorageResourceRequest,
}

// mounterPodCacheVolume returns the volume a mounter pod mounts for its Mountpoints to cache in, or nil if it has none.
func mounterPodCacheVolume(pod corev1.Pod) *corev1.Volume {
	if i := slices.IndexFunc(pod.Spec.Volumes, func(v corev1.Volume) bool { return v.Name == CacheVolumeName }); i >= 0 {
		return &pod.Spec.Volumes[i]
	}
	return nil
}

// cacheVolumeSummary describes a mounter pod's cache volume for the discovery log.
func cacheVolumeSummary(cacheVolume *corev1.Volume) string {
	switch {
	case cacheVolume == nil:
		return "cache type: none"
	case cacheVolume.EmptyDir != nil:
		return fmt.Sprintf("cache type: emptyDir, emptyDir medium: %q, emptyDir sizeLimit: %q",
			cacheVolume.EmptyDir.Medium, cacheVolume.EmptyDir.SizeLimit.String())
	case cacheVolume.Ephemeral != nil:
		spec := cacheVolume.Ephemeral.VolumeClaimTemplate.Spec
		return fmt.Sprintf("cache type: ephemeral, ephemeral StorageClass: %q, ephemeral request: %q",
			ptr.Deref(spec.StorageClassName, ""), spec.Resources.Requests.Storage().String())
	}
	return "cache type: other"
}

// validateCacheRequest returns whether this mount caches, or an InvalidArgument error for a cache request it cannot serve.
func validateCacheRequest(args mountpoint.Args, volumeCtx map[string]string, volumeID string, mounterHasCache, newTarget bool) (caches bool, err error) {
	pvCache := volumeCtx[volumecontext.Cache]
	cacheViaMountOptions := args.Has(mountpoint.ArgCache)

	// Reject a cache in both mountOptions and volumeAttributes, even `cache: disabled`, to match v2.
	if cacheViaMountOptions && pvCache != "" {
		return false, status.Error(codes.InvalidArgument,
			"Cache configured with both `mountOptions` and `volumeAttributes`, please remove the deprecated cache configuration in `mountOptions`")
	}

	switch {
	case cacheViaMountOptions:
		if newTarget {
			klog.Warningf("NodePublishVolume: volume %s enables the cache via the deprecated `cache` mount option,"+
				" so its path is ignored. Remove it from mountOptions and set the %q volume attribute to %q instead.",
				volumeID, volumecontext.Cache, volumecontext.CacheEnabled)
		}
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

// warnDeprecatedCacheSettings warns about a PV's v2 settings
func warnDeprecatedCacheSettings(volumeCtx map[string]string, volumeID string, nodeCache *corev1.Volume) {
	pvCache := volumeCtx[volumecontext.Cache]
	var mismatch string

	// cache type / emptyDir medium / ephemeral StorageClass mismatch
	switch pvCache {
	case volumecontext.CacheTypeEmptyDir:
		if nodeCache.EmptyDir == nil {
			mismatch = "cache type"
		} else if string(nodeCache.EmptyDir.Medium) != volumeCtx[volumecontext.CacheEmptyDirMedium] {
			mismatch = "emptyDir medium"
		}
	case volumecontext.CacheTypeEphemeral:
		if nodeCache.Ephemeral == nil {
			mismatch = "cache type"
		} else if ptr.Deref(nodeCache.Ephemeral.VolumeClaimTemplate.Spec.StorageClassName, "") != volumeCtx[volumecontext.CacheEphemeralStorageClassName] {
			mismatch = "ephemeral StorageClass"
		}
	}
	if mismatch != "" {
		klog.Warningf("NodePublishVolume: volume %s requests cache %q, but its %s differs from this node's cache volume, which"+
			" s3-csi-daemonset-mounter uses instead. Set %q to %q to use this node's cache without this warning.",
			volumeID, pvCache, mismatch, volumecontext.Cache, volumecontext.CacheEnabled)
	}

	// No cache type mismatch: warn if v2 cache sizes are set (CacheEmptyDirSizeLimit / CacheEphemeralStorageResourceRequest)
	var deprecated []string
	for _, attr := range deprecatedVolumeAttributes {
		if volumeCtx[attr] != "" {
			deprecated = append(deprecated, attr)
		}
	}
	if len(deprecated) > 0 {
		klog.Warningf("NodePublishVolume: volume %s sets the deprecated volume attributes %q. Remove them:"+
			" every mount runs in the mounter pod the daemonsetMounters[0] Helm values configure, and a PV opts in with %q set to %q.",
			volumeID, deprecated, volumecontext.Cache, volumecontext.CacheEnabled)
	} else if mismatch == "" && (pvCache == volumecontext.CacheTypeEmptyDir || pvCache == volumecontext.CacheTypeEphemeral) {
		klog.Warningf("NodePublishVolume: volume %s sets the v2 %q value %q. Set it to %q to use this node's cache without this warning.",
			volumeID, volumecontext.Cache, pvCache, volumecontext.CacheEnabled)
	}
}
