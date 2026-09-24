package main

import (
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
)

const bytesPerMiB = 1024 * 1024

const memoryRequestEnvName = "MOUNTER_MEMORY_REQUEST_BYTES"

const requestField = "resources.requests.memory"

// mounterOverheadMiB is held back from the memory request for this Go process, which lives in the same
// cgroup as the Mountpoints it supervises but is invisible to their memory accounting. It measures
// ~20-26 MiB in practice; the rest is slack for future growth.
const mounterOverheadMiB = 64

type memoryLimitStrategy string

const (
	memoryLimitEqualSplit memoryLimitStrategy = "equalSplit"
	memoryLimitNone       memoryLimitStrategy = "none"
)

// parseMemoryLimitStrategy maps the `--memory-limit-strategy` flag onto a strategy.
func parseMemoryLimitStrategy(value string) (memoryLimitStrategy, error) {
	switch strategy := memoryLimitStrategy(value); strategy {
	case memoryLimitEqualSplit, memoryLimitNone:
		return strategy, nil
	default:
		return "", fmt.Errorf("unknown memory limit strategy %q, expected %q or %q",
			value, memoryLimitEqualSplit, memoryLimitNone)
	}
}

// containerMemoryRequestBytes returns this container's memory request, or 0 when none is declared.
func containerMemoryRequestBytes() int64 {
	value := os.Getenv(memoryRequestEnvName)
	if value == "" {
		return 0
	}

	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		klog.Errorf("Ignoring unparseable %s=%q, expected a memory quantity: %v", memoryRequestEnvName, value, err)
		return 0
	}

	return quantity.Value()
}

// memoryLimit is the resolved `--memory-target` policy for every Mountpoint this container hosts.
type memoryLimit struct {
	strategy memoryLimitStrategy
	shareMiB int64 // under equalSplit, what each Mountpoint gets; 0 otherwise
}

// newMemoryLimit resolves the strategy once at startup, logging the inputs it used so `kubectl logs`
// can answer why a mount was sized the way it was. An error means no mount on this node can be served,
// so the caller is expected to exit rather than accept a config it cannot honour.
func newMemoryLimit(strategy memoryLimitStrategy, requestBytes int64, maxVolumesPerNode int64) (memoryLimit, error) {
	if strategy == memoryLimitNone {
		klog.Infof("memoryLimitStrategy=%s: %s is left to each PV's mountOptions.",
			strategy, mountpoint.ArgMemoryTarget)
		return memoryLimit{strategy: strategy}, nil
	}

	if requestBytes <= 0 {
		return memoryLimit{}, fmt.Errorf("memoryLimitStrategy=%s divides this container's %s between "+
			"the Mountpoints it hosts, but none is declared. Set daemonsetMounters[].%s, or set "+
			"daemonsetMounters[].memoryLimitStrategy=%s to leave %s to each PV's mountOptions",
			memoryLimitEqualSplit, requestField, requestField, memoryLimitNone, mountpoint.ArgMemoryTarget)
	}

	if maxVolumesPerNode <= 0 {
		return memoryLimit{}, fmt.Errorf("memoryLimitStrategy=%s divides this container's %s by "+
			"maxVolumesPerNode, which is %d. Set a positive daemonsetMounters[].maxVolumesPerNode, or "+
			"set daemonsetMounters[].memoryLimitStrategy=%s to leave %s to each PV's mountOptions",
			memoryLimitEqualSplit, requestField, maxVolumesPerNode, memoryLimitNone,
			mountpoint.ArgMemoryTarget)
	}

	budgetMiB := requestBytes/bytesPerMiB - mounterOverheadMiB
	if budgetMiB <= 0 {
		return memoryLimit{}, fmt.Errorf("this container's %s of %d bytes is at or below the %d MiB "+
			"reserved for the mounter process itself, leaving nothing for Mountpoint. Raise "+
			"daemonsetMounters[].%s", requestField, requestBytes, mounterOverheadMiB, requestField)
	}

	shareMiB := budgetMiB / maxVolumesPerNode
	if shareMiB < mountpoint.MinMemoryTargetMiB {
		return memoryLimit{}, fmt.Errorf("this container's %s (%d bytes, %d MiB after reserving %d MiB "+
			"for the mounter process) divided by maxVolumesPerNode=%d gives %d MiB per Mountpoint, below "+
			"Mountpoint's minimum %s of %d MiB. Raise daemonsetMounters[].%s or lower "+
			"daemonsetMounters[].maxVolumesPerNode",
			requestField, requestBytes, budgetMiB, mounterOverheadMiB, maxVolumesPerNode, shareMiB,
			mountpoint.ArgMemoryTarget, mountpoint.MinMemoryTargetMiB, requestField)
	}

	klog.Infof("memoryLimitStrategy=%s: each Mountpoint gets %s=%d (this container's %s of %d bytes, "+
		"minus %d MiB for the mounter process, divided by maxVolumesPerNode=%d).",
		memoryLimitEqualSplit, mountpoint.ArgMemoryTarget, shareMiB, requestField, requestBytes,
		mounterOverheadMiB, maxVolumesPerNode)
	return memoryLimit{strategy: strategy, shareMiB: shareMiB}, nil
}

// targetFor returns the `--memory-target` to pass to a mount's Mountpoint, reading what the PV's
// mountOptions asked for out of args. A zero target means "pass args through as they are".
func (l memoryLimit) targetFor(volumeId string, args mountpoint.Args) int64 {
	declared, hasDeclared := args.Value(mountpoint.ArgMemoryTarget)

	if l.strategy == memoryLimitNone {
		if !hasDeclared {
			klog.Warningf("Volume %s does not set %s in its PV mountOptions and "+
				"memoryLimitStrategy=%s, so its Mountpoint targets 95%% of the memory it detects. "+
				"Together the mounts on this node can exhaust it. Set %s in the PV mountOptions, or set "+
				"daemonsetMounters[].memoryLimitStrategy=%s to divide this container's %s evenly.",
				volumeId, mountpoint.ArgMemoryTarget, memoryLimitNone, mountpoint.ArgMemoryTarget,
				memoryLimitEqualSplit, requestField)
		}
		return 0
	}

	if hasDeclared {
		klog.Warningf("Ignoring %s=%s from the PV mountOptions of volume %s: memoryLimitStrategy=%s "+
			"gives every Mountpoint on this node an equal %d MiB share of this container's %s. Set "+
			"daemonsetMounters[].memoryLimitStrategy=%s to honour per-volume targets.",
			mountpoint.ArgMemoryTarget, declared, volumeId, memoryLimitEqualSplit, l.shareMiB,
			requestField, memoryLimitNone)
	}

	return l.shareMiB
}
