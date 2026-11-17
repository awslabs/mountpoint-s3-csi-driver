package mounter

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

// The volume limit this node pod advertises to the scheduler, through NodeGetInfo.
//
// It is the MounterMaxVolumesLabel of this node's running mounter. A node pod with no mounter still serves the
// volumes already on its node, which need unmounting (and those V1 and V2 mounted, credential refresh), so it
// registers right away, advertising the fallback limit: the smallest maxVolumesPerNode of the chart's
// daemonsetMounters entries, so no more than the mounter that comes to run here takes. The startup taint, not
// registration, keeps workloads off a new node until its mounter runs.
const (
	// defaultStaleVolumeLimitWarnAfter is how long kubelet can keep a limit that no longer holds, without
	// re-reading it, before this node pod warns that kubelet isn't refreshing it.
	defaultStaleVolumeLimitWarnAfter = 60 * time.Second
)

// volumeLimitState tracks this node's mounter over time for VolumeLimit. Its zero value is ready to use.
type volumeLimitState struct {
	mu sync.Mutex

	// found is closed once a mounter is discovered for the first time.
	found       chan struct{}
	foundClosed bool

	// advertised is the limit NodeGetInfo last returned, and advertisedReason why, at advertisedAt. A zero
	// advertisedAt means kubelet hasn't asked yet.
	advertised       int64
	advertisedReason string
	advertisedAt     time.Time

	// staleWarnedFor is the limit the last stale-limit warning was logged for, if staleWarned.
	staleWarnedFor int64
	staleWarned    bool
}

// foundCh returns the channel closed once a mounter is first discovered. Caller must hold s.mu.
func (s *volumeLimitState) foundCh() chan struct{} {
	if s.found == nil {
		s.found = make(chan struct{})
	}
	return s.found
}

// MounterFound returns a channel that is closed once a mounter is discovered on this node for the first time.
func (dm *DaemonsetMounter) MounterFound() <-chan struct{} {
	dm.limit.mu.Lock()
	defer dm.limit.mu.Unlock()
	return dm.limit.foundCh()
}

// SetFallbackVolumeLimit sets the volume limit to advertise while no mounter runs on this node, 0 for none.
// Call it before serving NodeGetInfo.
func (dm *DaemonsetMounter) SetFallbackVolumeLimit(limit int64) {
	dm.fallbackVolumeLimit = limit
}

// setMounter records m as this node's mounter, or nil once it is gone or stale.
func (dm *DaemonsetMounter) setMounter(m *mounterPod) {
	s := &dm.limit
	s.mu.Lock()
	defer s.mu.Unlock()

	dm.mounter.Store(m)
	if m != nil && !s.foundClosed {
		close(s.foundCh())
		s.foundClosed = true
	}
}

// VolumeLimit returns the volume limit to advertise for this node (0 for none): the running mounter's
// MounterMaxVolumesLabel, or the fallback limit while none runs. It fails only when it can't tell whether a
// mounter runs, as the fallback can be more than a running mounter takes.
func (dm *DaemonsetMounter) VolumeLimit(ctx context.Context) (int64, error) {
	limit, reason, warn, err := dm.currentVolumeLimit(ctx)
	if err != nil {
		return 0, err
	}

	s := &dm.limit
	s.mu.Lock()
	// Several reasons share a limit, such as none, so a new reason alone is a change to log.
	changed := s.advertisedAt.IsZero() || s.advertised != limit || s.advertisedReason != reason
	s.advertised, s.advertisedReason, s.advertisedAt = limit, reason, time.Now()
	// Kubelet now has the current limit, so a later stale one is a new incident to warn about.
	s.staleWarned = false
	s.mu.Unlock()

	if changed {
		msg := fmt.Sprintf("DaemonsetMounter: advertising %s for node %s: %s", describeVolumeLimit(limit), dm.nodeID, reason)
		if warn {
			klog.Warning(msg)
		} else {
			klog.Info(msg)
		}
	}
	return limit, nil
}

// currentVolumeLimit returns the volume limit that holds now, why, and whether that deserves a warning.
func (dm *DaemonsetMounter) currentVolumeLimit(ctx context.Context) (int64, string, bool, error) {
	maxVolumes, found := int64(0), false
	if m := dm.mounter.Load(); m != nil {
		maxVolumes, found = m.maxVolumes, true
	} else {
		// Discovery may not have found a mounter that runs yet, so look. A running mounter's label wins over the
		// fallback: one left from before a helm upgrade that raised maxVolumesPerNode takes fewer volumes.
		running, err := dm.runningMounterPods(ctx)
		switch {
		case errors.Is(err, ErrMultipleMounterPods):
			// New mounts fail until only one runs, and the fallback is no more than either takes.
		case err != nil:
			return 0, "", false, fmt.Errorf("can't tell whether an s3-csi-daemonset-mounter runs on node %s, so it can't "+
				"tell its volume limit: %w", dm.nodeID, err)
		case len(running) == 1:
			maxVolumes, found = mounterPodMaxVolumes(running[0]), true
			// Discovery takes it on now, rather than on its backed-off schedule.
			dm.requestRediscovery()
		}
	}

	const fallback = "the smallest maxVolumesPerNode of the daemonsetMounters entries"
	switch {
	case !found:
		return dm.fallbackVolumeLimit, "no s3-csi-daemonset-mounter runs on this node yet, so new S3 mounts on it fail " +
			"until one does. Until then it advertises " + fallback, false, nil
	case maxVolumes < 0:
		return dm.fallbackVolumeLimit, "its s3-csi-daemonset-mounter has no valid " + MounterMaxVolumesLabel +
			" label, so it advertises " + fallback, true, nil
	case maxVolumes == 0:
		return 0, "its s3-csi-daemonset-mounter's " + MounterMaxVolumesLabel + " label is 0. The scheduler will not " +
			"limit the number of S3 volumes on this node, which may lead to resource exhaustion (memory OOM, or disk " +
			"exhaustion when cache is enabled) on s3-csi-daemonset-mounter", true, nil
	default:
		return maxVolumes, "its s3-csi-daemonset-mounter's " + MounterMaxVolumesLabel + " label", false, nil
	}
}

// warnIfVolumeLimitStale logs a warning when kubelet still has a limit other than the one of this node's
// running mounter, and hasn't re-read it for staleVolumeLimitWarnAfter, which happens where kubelet reads it
// only at registration. It warns once per limit until kubelet's limit holds again, and not while no mounter
// runs.
func (dm *DaemonsetMounter) warnIfVolumeLimitStale() {
	m := dm.mounter.Load()

	s := &dm.limit
	s.mu.Lock()
	defer s.mu.Unlock()
	if m == nil || m.maxVolumes < 0 || s.advertisedAt.IsZero() || s.advertised == m.maxVolumes {
		// Kubelet's limit holds, so a later stale one is a new incident to warn about.
		s.staleWarned = false
		return
	}
	limit := m.maxVolumes
	if time.Since(s.advertisedAt) < dm.staleVolumeLimitWarnAfter {
		return
	}
	if s.staleWarned && s.staleWarnedFor == limit {
		return
	}
	s.staleWarned, s.staleWarnedFor = true, limit
	klog.Warningf("DaemonsetMounter: the volume limit kubelet has for node %s is %s, but its s3-csi-daemonset-mounter's is %s, "+
		"and kubelet hasn't re-read it for %v. Kubelet re-reads it only where the MutableCSINodeAllocatableCount feature is on "+
		"(EKS 1.34 and later, Kubernetes 1.35 and later); elsewhere, restart this node's s3-csi-node pod so it registers again: "+
		"`kubectl delete pod -n %s -l app=s3-csi-node --field-selector spec.nodeName=%s`",
		dm.nodeID, formatVolumeLimit(s.advertised), formatVolumeLimit(limit), time.Since(s.advertisedAt).Round(time.Second),
		mounterNamespace, dm.nodeID)
}

// describeVolumeLimit returns limit for a log line: "no volume limit" for 0, else "a volume limit of <limit>".
func describeVolumeLimit(limit int64) string {
	if limit == 0 {
		return "no volume limit"
	}
	return "a volume limit of " + strconv.FormatInt(limit, 10)
}

// formatVolumeLimit returns limit as a value: "none" for 0, else the number.
func formatVolumeLimit(limit int64) string {
	if limit == 0 {
		return "none"
	}
	return strconv.FormatInt(limit, 10)
}
