package main

import (
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
)

const cacheCapacityEnvName = "MOUNTER_CACHE_CAPACITY"

// cacheMarginPercent of the cache volume is divided between the Mountpoints, because Mountpoint evicts
// only after a write has already crossed --max-cache-size.
const cacheMarginPercent = 95

type cacheLimitStrategy string

const (
	cacheLimitEqualSplit cacheLimitStrategy = "equalSplit"
	cacheLimitNone       cacheLimitStrategy = "none"
)

// parseCacheLimitStrategy maps the `--cache-limit-strategy` flag onto a strategy.
func parseCacheLimitStrategy(value string) (cacheLimitStrategy, error) {
	switch strategy := cacheLimitStrategy(value); strategy {
	case cacheLimitEqualSplit, cacheLimitNone:
		return strategy, nil
	default:
		return "", fmt.Errorf("unknown cache limit strategy %q, expected %q or %q",
			value, cacheLimitEqualSplit, cacheLimitNone)
	}
}

// cacheCapacityBytes returns the size of this container's cache volume, or 0 when none is declared.
func cacheCapacityBytes() (int64, error) {
	value := os.Getenv(cacheCapacityEnvName)
	if value == "" {
		return 0, nil
	}

	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a parseable Kubernetes quantity: %w", cacheCapacityEnvName, value, err)
	}

	return quantity.Value(), nil
}

// tmpfsCacheBytes returns how much of this container's memory the cache volume takes: all of it for a
// Memory-medium emptyDir, which is a tmpfs, and none otherwise.
func tmpfsCacheBytes(medium string, capacityBytes int64) int64 {
	if medium != string(corev1.StorageMediumMemory) {
		return 0
	}
	return capacityBytes
}

// cacheLimit is the resolved `--max-cache-size` policy for every Mountpoint this container hosts.
type cacheLimit struct {
	strategy cacheLimitStrategy
	shareMiB int64 // under equalSplit, what each Mountpoint gets; 0 otherwise
}

// newCacheLimit resolves the strategy once at startup, logging the inputs it used so `kubectl logs`
// can answer why a mount was sized the way it was. An error means no mount on this node can be served,
// so the caller is expected to exit rather than accept a config it cannot honour.
func newCacheLimit(strategy cacheLimitStrategy, capacityBytes int64, maxVolumesPerNode int64) (cacheLimit, error) {
	if strategy == cacheLimitNone {
		klog.Infof("cacheLimitStrategy=%s: %s is left to each PV's mountOptions.",
			strategy, mountpoint.ArgMaxCacheSize)
		return cacheLimit{strategy: strategy}, nil
	}

	if capacityBytes < 0 {
		return cacheLimit{}, fmt.Errorf("cacheLimitStrategy=%s divides this container's cache volume, whose size "+
			"is %d bytes. Set a positive daemonsetMounters[].cache.emptyDir.sizeLimit or .ephemeral.resourceRequests",
			cacheLimitEqualSplit, capacityBytes)
	}

	if maxVolumesPerNode <= 0 {
		return cacheLimit{}, fmt.Errorf("cacheLimitStrategy=%s divides this container's cache volume by "+
			"maxVolumesPerNode, which is %d. Set a positive daemonsetMounters[].maxVolumesPerNode, or "+
			"set daemonsetMounters[].cache.cacheLimitStrategy=%s to leave %s to each PV's mountOptions",
			cacheLimitEqualSplit, maxVolumesPerNode, cacheLimitNone, mountpoint.ArgMaxCacheSize)
	}

	shareMiB := capacityBytes * cacheMarginPercent / 100 / maxVolumesPerNode / bytesPerMiB
	if shareMiB == 0 {
		klog.Warningf("cacheLimitStrategy=%s gives each Mountpoint no whole MiB of this container's cache "+
			"volume of %d bytes divided by maxVolumesPerNode=%d, so every mount is served uncached with %s=0. "+
			"Set or raise daemonsetMounters[].cache.emptyDir.sizeLimit or .ephemeral.resourceRequests, or "+
			"lower daemonsetMounters[].maxVolumesPerNode.",
			cacheLimitEqualSplit, capacityBytes, maxVolumesPerNode, mountpoint.ArgMaxCacheSize)
		return cacheLimit{strategy: strategy, shareMiB: 0}, nil
	}

	klog.Infof("cacheLimitStrategy=%s: each Mountpoint gets %s=%d (%d%% of this container's cache volume "+
		"of %d bytes, divided by maxVolumesPerNode=%d).",
		cacheLimitEqualSplit, mountpoint.ArgMaxCacheSize, shareMiB, cacheMarginPercent, capacityBytes,
		maxVolumesPerNode)
	return cacheLimit{strategy: strategy, shareMiB: shareMiB}, nil
}

// maxCacheSizeFor returns the `--max-cache-size` to pass to a mount's Mountpoint, reading what the PV's
// mountOptions asked for out of args. Unlike a memory target, 0 is a real size ("no cache"), so ok is
// false when args should pass through as they are.
func (l cacheLimit) maxCacheSizeFor(volumeId string, args mountpoint.Args) (sizeMiB int64, ok bool) {
	// Mountpoint refuses --max-cache-size without --cache, which only a mount that opted in has.
	// Leave stray --max-cache-size for Mountpoint to reject.
	if !args.Has(mountpoint.ArgCache) {
		return 0, false
	}
	declared, hasDeclared := args.Value(mountpoint.ArgMaxCacheSize)

	if l.strategy == cacheLimitNone {
		if !hasDeclared {
			klog.Warningf("Volume %s does not set %s in its PV mountOptions and cacheLimitStrategy=%s, "+
				"so nothing bounds its share of the cache volume. Together the mounts on this node can fill it. "+
				"Set %s in the PV mountOptions, or set daemonsetMounters[].cache.cacheLimitStrategy=%s to "+
				"divide the cache volume evenly.",
				volumeId, mountpoint.ArgMaxCacheSize, cacheLimitNone, mountpoint.ArgMaxCacheSize,
				cacheLimitEqualSplit)
		}
		return 0, false
	}

	if hasDeclared {
		klog.Warningf("Ignoring %s=%s for volume %s: cacheLimitStrategy=%s gives every Mountpoint on this "+
			"node an equal %d MiB share of this container's cache volume. Remove %s from the PV mountOptions.",
			mountpoint.ArgMaxCacheSize, declared, volumeId, cacheLimitEqualSplit, l.shareMiB, mountpoint.ArgMaxCacheSize)
	}

	return l.shareMiB, true
}
