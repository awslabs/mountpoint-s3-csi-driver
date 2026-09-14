package main

import (
	"fmt"
	"os"
	"strconv"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
)

const bytesPerMiB = 1024 * 1024

const (
	memoryLimitEnvName   = "MOUNTER_MEMORY_LIMIT_BYTES"
	memoryRequestEnvName = "MOUNTER_MEMORY_REQUEST_BYTES"
)

// containerMemoryBudgetBytes returns the memory budget to divide between hosted Mountpoints.
func containerMemoryBudgetBytes() (budgetBytes int64, budgetField string) {
	if bytes := memoryEnvBytes(memoryLimitEnvName); bytes > 0 {
		return bytes, "limits.memory"
	}
	if bytes := memoryEnvBytes(memoryRequestEnvName); bytes > 0 {
		return bytes, "requests.memory"
	}
	return 0, ""
}

// memoryEnvBytes reads a memory quantity from the named env variable.
func memoryEnvBytes(name string) int64 {
	value := os.Getenv(name)
	if value == "" {
		return 0
	}

	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		klog.Errorf("Ignoring unparseable %s=%q, expected a memory quantity: %v", name, value, err)
		return 0
	}

	return quantity.Value()
}

type memoryTarget struct {
	miB int64
	err error
}

// arg returns the value to pass Mountpoint's `--memory-target`, "" to leave the flag unset. A non-nil
// error means this container cannot provide the target the mount needs.
func (t memoryTarget) arg() (string, error) {
	if t.err != nil {
		return "", t.err
	}
	if t.miB <= 0 {
		return "", nil
	}
	return strconv.FormatInt(t.miB, 10), nil
}

// resolveMemoryTarget divides this container's memory budget between the maxVolumesPerNode Mountpoints
// it hosts, logging the inputs it used so `kubectl logs` can answer why a mount was sized the way it was.
// budgetField names the resources field budgetBytes came from ("" when there is no budget), from
// [containerMemoryBudgetBytes].
func resolveMemoryTarget(budgetBytes int64, budgetField string, maxVolumesPerNode int64) memoryTarget {
	if budgetBytes <= 0 || maxVolumesPerNode <= 0 {
		// Report which input is missing rather than a byte count: a zero budget means "none declared",
		// which is the opposite of a small one — Mountpoint then sizes itself against the whole node.
		var reason string
		switch {
		case budgetBytes <= 0 && maxVolumesPerNode <= 0:
			reason = "this container declares no memory budget (neither resources.limits.memory nor " +
				"resources.requests.memory is set), and maxVolumesPerNode is 0"
		case budgetBytes <= 0:
			reason = "this container declares no memory budget (neither resources.limits.memory nor " +
				"resources.requests.memory is set)"
		default:
			reason = fmt.Sprintf("maxVolumesPerNode is 0, so this container's memory budget "+
				"(resources.%s, %d bytes) has nothing to divide by", budgetField, budgetBytes)
		}

		klog.Warningf("Not setting %s on hosted Mountpoints: %s. Each Mountpoint that does not set "+
			"%s in its PV mountOptions will instead target 95%% of the memory it detects, so together "+
			"they can exhaust the node. Set daemonsetMounters[].resources.limits.memory (or "+
			"requests.memory) and a non-zero daemonsetMounters[].maxVolumesPerNode.",
			mountpoint.ArgMemoryTarget, reason, mountpoint.ArgMemoryTarget)
		return memoryTarget{}
	}

	// Floor the share, so the combined target of all volumes never exceeds this container's budget.
	targetMiB := budgetBytes / maxVolumesPerNode / bytesPerMiB
	if targetMiB < mountpoint.MinMemoryTargetMiB {
		err := fmt.Errorf("the mounter container's memory budget (resources.%s, %d bytes) divided by "+
			"maxVolumesPerNode=%d gives %d MiB per volume, below Mountpoint's minimum %s of %d MiB, so "+
			"mounts that do not set %s in their PV mountOptions are refused. Raise "+
			"daemonsetMounters[].resources.%s or lower daemonsetMounters[].maxVolumesPerNode",
			budgetField, budgetBytes, maxVolumesPerNode, targetMiB, mountpoint.ArgMemoryTarget,
			mountpoint.MinMemoryTargetMiB, mountpoint.ArgMemoryTarget, budgetField)
		klog.Error(err)
		return memoryTarget{err: err}
	}

	klog.Infof("Each Mountpoint gets %s=%d (this container's resources.%s of %d bytes divided by "+
		"maxVolumesPerNode=%d), unless its PV mountOptions set %s themselves.",
		mountpoint.ArgMemoryTarget, targetMiB, budgetField, budgetBytes, maxVolumesPerNode,
		mountpoint.ArgMemoryTarget)
	return memoryTarget{miB: targetMiB}
}
