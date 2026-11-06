package mounter

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

// The volume limit this node pod advertises to the scheduler, through NodeGetInfo.
//
// It is the MounterMaxVolumesLabel of this node's mounter, kept after the mounter goes away until another
// one runs. A node pod with no mounter still serves the volumes already on its node, which need unmounting
// (and those V1 and V2 mounted, credential refresh), so it stays registered, and advertises
// noMounterVolumeLimit until a mounter runs.
const (
	// noMounterVolumeLimit is the limit advertised until a mounter runs on this node: none. The startup
	// taint keeps workloads off new nodes until their mounter runs.
	noMounterVolumeLimit int64 = 0

	// defaultLimitWaitForMounter is how long after startup VolumeLimit waits for a mounter before advertising
	// noMounterVolumeLimit, so a node pod starting alongside its mounter registers the mounter's limit, the
	// only one some kubelets read. Kubelet allows NodeGetInfo 2 minutes at registration.
	defaultLimitWaitForMounter = 60 * time.Second

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

	// lastLimit is the limit of the last mounter discovered, if it had a valid MounterMaxVolumesLabel (hasLastLimit).
	lastLimit    int64
	hasLastLimit bool

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

// SetVolumeLimitTimingForTesting replaces how long VolumeLimit waits for a mounter after startup, and how
// long a stale limit lasts before a warning.
func (dm *DaemonsetMounter) SetVolumeLimitTimingForTesting(limitWaitForMounter, staleWarnAfter time.Duration) {
	dm.limitWaitForMounter = limitWaitForMounter
	dm.staleVolumeLimitWarnAfter = staleWarnAfter
}

// setMounter records m as this node's mounter, or nil once it is gone or stale.
func (dm *DaemonsetMounter) setMounter(m *mounterPod) {
	s := &dm.limit
	s.mu.Lock()
	defer s.mu.Unlock()

	dm.mounter.Store(m)
	if m == nil {
		return
	}
	// A mounter with no valid label has no limit to keep once it goes away, so drop any earlier mounter's.
	s.lastLimit, s.hasLastLimit = m.maxVolumes, m.maxVolumes >= 0
	if !s.foundClosed {
		close(s.foundCh())
		s.foundClosed = true
	}
}

// VolumeLimit returns the volume limit to advertise for this node (0 for none): the mounter's
// MounterMaxVolumesLabel once one runs here, or noMounterVolumeLimit until then. A mounter that goes away
// keeps its limit until another one runs. Shortly after startup, it first waits for a mounter.
func (dm *DaemonsetMounter) VolumeLimit(ctx context.Context) int64 {
	dm.waitForMounterAfterStartup(ctx)

	limit, reason, warn := dm.currentVolumeLimit()
	if ctx.Err() != nil {
		// Kubelet gave up on this call, so it never gets this limit: don't record it as the one kubelet has.
		return limit
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
	return limit
}

// waitForMounterAfterStartup waits until a mounter is discovered, ctx is done, or limitWaitForMounter
// has passed since this node pod started, whichever is first.
func (dm *DaemonsetMounter) waitForMounterAfterStartup(ctx context.Context) {
	wait := time.Until(dm.startedAt.Add(dm.limitWaitForMounter))
	if wait <= 0 || dm.mounter.Load() != nil {
		return
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-dm.MounterFound():
	case <-timer.C:
	case <-ctx.Done():
	}
}

// currentVolumeLimit returns the volume limit that holds now, why, and whether that deserves a warning.
func (dm *DaemonsetMounter) currentVolumeLimit() (int64, string, bool) {
	if m := dm.mounter.Load(); m != nil {
		switch {
		case m.maxVolumes < 0:
			return noMounterVolumeLimit, "its s3-csi-daemonset-mounter has no valid " + MounterMaxVolumesLabel + " label", true
		case m.maxVolumes == 0:
			return 0, "its s3-csi-daemonset-mounter's " + MounterMaxVolumesLabel + " label is 0. The scheduler will not " +
				"limit the number of S3 volumes on this node, which may lead to resource exhaustion (memory OOM, or disk " +
				"exhaustion when cache is enabled) on s3-csi-daemonset-mounter", true
		default:
			return m.maxVolumes, "its s3-csi-daemonset-mounter's " + MounterMaxVolumesLabel + " label", false
		}
	}

	s := &dm.limit
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hasLastLimit {
		return s.lastLimit, "kept from its s3-csi-daemonset-mounter, which went away; new S3 mounts on this node fail until another one runs", false
	}
	return noMounterVolumeLimit, "no s3-csi-daemonset-mounter runs on this node, so new S3 mounts on it fail until one does", false
}

// warnIfVolumeLimitStale logs a warning when kubelet still has a limit other than the one of this node's
// running mounter, and hasn't re-read it for staleVolumeLimitWarnAfter, which happens where kubelet reads it
// only at registration. It warns once per limit until kubelet's limit holds again. A limit kubelet kept
// from a mounter that went away needs no warning.
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
