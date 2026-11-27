package csicontroller

import (
	"context"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	crdv2 "github.com/awslabs/mountpoint-s3-csi-driver/pkg/api/v2"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/podmounter/mppod"
)

const (
	// Bounds for the delay between two replacements of failed Mountpoint Pods for the same workload.
	// The first replacement is immediate.
	minReplacementBackoff = 10 * time.Second
	maxReplacementBackoff = 5 * time.Minute
	// A workload that did not need a replacement for this long starts from scratch.
	replacementBackoffResetAfter = 15 * time.Minute
)

// replacementBackoff rate-limits replacing failed Mountpoint Pods per workload, so a node that keeps failing
// Mountpoint Pods (e.g., continuous DiskPressure evictions) does not result in a tight create/fail loop.
// The zero value is ready to use.
type replacementBackoff struct {
	mu      sync.Mutex
	now     func() time.Time // for tests, defaults to time.Now
	entries map[string]replacementBackoffEntry
}

type replacementBackoffEntry struct {
	attempts int
	last     time.Time
}

func (b *replacementBackoff) currentTime() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

// allowed returns whether a replacement for `workloadUID` is allowed now.
func (b *replacementBackoff) allowed(workloadUID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	e, found := b.entries[workloadUID]
	if !found {
		return true
	}
	t := b.currentTime()
	if t.Sub(e.last) > replacementBackoffResetAfter {
		return true
	}
	delay := minReplacementBackoff << (e.attempts - 1)
	if delay > maxReplacementBackoff || delay <= 0 {
		delay = maxReplacementBackoff
	}
	return t.Sub(e.last) >= delay
}

// record records that a replacement for `workloadUID` was done just now.
func (b *replacementBackoff) record(workloadUID string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	t := b.currentTime()
	if b.entries == nil {
		b.entries = map[string]replacementBackoffEntry{}
	}
	// Forget old entries so the map does not grow forever.
	for uid, e := range b.entries {
		if t.Sub(e.last) > replacementBackoffResetAfter {
			delete(b.entries, uid)
		}
	}
	e := b.entries[workloadUID]
	b.entries[workloadUID] = replacementBackoffEntry{attempts: e.attempts + 1, last: t}
}

// mountpointPodNameForWorkload returns name of the Mountpoint Pod `workloadUID` is attached to in `s3pa`.
func mountpointPodNameForWorkload(s3pa *crdv2.MountpointS3PodAttachment, workloadUID string) (string, bool) {
	for mpPodName, attachments := range s3pa.Spec.MountpointS3PodAttachments {
		for _, attachment := range attachments {
			if attachment.WorkloadPodUID == workloadUID {
				return mpPodName, true
			}
		}
	}
	return "", false
}

// detachWorkloadFromMountpointPod removes `workloadUID` from `mpPodName`'s attachments in `s3pa` (in-memory only),
// and drops the Mountpoint Pod from `s3pa` if there are no workloads left.
func detachWorkloadFromMountpointPod(s3pa *crdv2.MountpointS3PodAttachment, mpPodName, workloadUID string) {
	remaining := make([]crdv2.WorkloadAttachment, 0, len(s3pa.Spec.MountpointS3PodAttachments[mpPodName]))
	for _, attachment := range s3pa.Spec.MountpointS3PodAttachments[mpPodName] {
		if attachment.WorkloadPodUID != workloadUID {
			remaining = append(remaining, attachment)
		}
	}
	if len(remaining) == 0 {
		delete(s3pa.Spec.MountpointS3PodAttachments, mpPodName)
		return
	}
	s3pa.Spec.MountpointS3PodAttachments[mpPodName] = remaining
}

// handleWorkloadOfFailedMountpointPod moves `workloadPod` away from its Mountpoint Pod if that Pod is in `Failed` phase
// (e.g., it was evicted by kubelet because of node pressure). A Failed Pod never recovers, so without this
// the workload would be stuck on a Mountpoint Pod that can never serve its volume.
//
// The workload is attached to another suitable Mountpoint Pod, or a new one is spawned for it. The next
// `NodePublishVolume` retry from kubelet finds the new Mountpoint Pod through the MountpointS3PodAttachment.
// Once no workload is left on the failed Pod, it is deleted so CSI Driver Node can clean up its stale mount.
//
// Workloads that are already running are left alone: their mount is broken and can only be fixed by restarting the workload
// (kubelet does not call `NodePublishVolume` again for a running Pod), so a new Mountpoint Pod would just sit idle.
// The failed Pod is deleted when such a workload terminates (see [Reconciler.addNeedsUnmountAnnotation]).
//
// It returns whether the workload was handled. If not (i.e., Mountpoint Pod is not failed), caller should continue as usual.
func (r *Reconciler) handleWorkloadOfFailedMountpointPod(
	ctx context.Context,
	workloadPod *corev1.Pod,
	pv *corev1.PersistentVolume,
	s3pa *crdv2.MountpointS3PodAttachment,
	priorityClassKind mppod.PriorityClassKind,
	log logr.Logger,
) (handled bool, requeue bool, err error) {
	workloadUID := string(workloadPod.UID)
	mpPodName, found := mountpointPodNameForWorkload(s3pa, workloadUID)
	if !found {
		return false, DontRequeue, nil
	}

	mpPod, err := r.getMountpointPod(ctx, mpPodName)
	if err != nil {
		// A missing Pod is intentionally not handled here: the lookup goes through the informer cache,
		// so it can't be told apart from a Pod that was just created and not yet observed.
		if apierrors.IsNotFound(err) {
			return false, DontRequeue, nil
		}
		return true, Requeue, err
	}
	if mpPod.Status.Phase != corev1.PodFailed {
		return false, DontRequeue, nil
	}

	log = log.WithValues("failedMountpointPod", mpPodName, "failedMountpointPodReason", mpPod.Status.Reason)

	if isPodRunning(workloadPod) {
		log.Info("Mountpoint Pod failed while the workload is running. The workload needs to be restarted to get a working mount")
		return false, DontRequeue, nil
	}

	if !r.replacementBackoff.allowed(workloadUID) {
		log.Info("Mountpoint Pod failed again shortly after a previous replacement, backing off before replacing it")
		return true, Requeue, nil
	}

	log.Info("Mountpoint Pod failed, moving the workload to a new Mountpoint Pod")
	detachWorkloadFromMountpointPod(s3pa, mpPodName, workloadUID)
	// This either assigns the workload to another suitable Mountpoint Pod in `s3pa`, or spawns a new one,
	// and persists the (already modified) `s3pa` in a single update.
	requeue, err = r.addWorkloadToS3PodAttachment(ctx, workloadPod, pv, s3pa, priorityClassKind, log)
	if err != nil || requeue {
		return true, requeue, err
	}
	r.replacementBackoff.record(workloadUID)

	if _, stillReferenced := s3pa.Spec.MountpointS3PodAttachments[mpPodName]; !stillReferenced {
		if err := r.deleteMountpointPod(ctx, mpPod); err != nil {
			// Not worth failing the workload's reconciliation, it is already moved. A periodic resync of the Pod retries this.
			log.Error(err, "Failed to delete failed Mountpoint Pod")
		}
	}
	return true, DontRequeue, nil
}

// cleanupFailedMountpointPod deletes a failed Mountpoint Pod if no MountpointS3PodAttachment references it.
//
// Deleting the Pod is what lets CSI Driver Node on the Pod's node clean up the stale FUSE mount it left behind:
// an evicted Pod's directory is gone, so the "needs-unmount" flow cannot be used, but the node removes
// mounts of Mountpoint Pods that do not exist anymore.
//
// If the Pod is still referenced, nothing is done here: its workloads are moved away by their own reconciliation
// (see [Reconciler.handleWorkloadOfFailedMountpointPod]), which deletes the Pod afterwards, or the Pod is deleted
// when the last (running) workload using it terminates (see [Reconciler.addNeedsUnmountAnnotation]).
func (r *Reconciler) cleanupFailedMountpointPod(ctx context.Context, mpPod *corev1.Pod) error {
	log := logf.FromContext(ctx).WithValues("mountpointPod", mpPod.Name)

	referenced, err := r.isMountpointPodReferenced(ctx, mpPod)
	if err != nil {
		return err
	}
	if referenced {
		log.V(debugLevel).Info("Failed Mountpoint Pod is still referenced by a MountpointS3PodAttachment")
		return nil
	}

	return r.deleteMountpointPod(ctx, mpPod)
}

// isMountpointPodReferenced returns whether any MountpointS3PodAttachment still has an entry for `mpPod`.
func (r *Reconciler) isMountpointPodReferenced(ctx context.Context, mpPod *corev1.Pod) (bool, error) {
	s3paList := &crdv2.MountpointS3PodAttachmentList{}
	if err := r.List(ctx, s3paList); err != nil {
		return false, err
	}
	for _, s3pa := range s3paList.Items {
		if _, ok := s3pa.Spec.MountpointS3PodAttachments[mpPod.Name]; ok {
			return true, nil
		}
	}
	return false, nil
}

// workloadPodsOfFailedMountpointPod maps a failed Mountpoint Pod to reconcile requests for the workload Pods attached to it,
// so they are moved to a new Mountpoint Pod right away rather than whenever their next unrelated update happens.
// It returns nothing for any other kind of Pod.
func (r *Reconciler) workloadPodsOfFailedMountpointPod(ctx context.Context, obj client.Object) []reconcile.Request {
	mpPod, ok := obj.(*corev1.Pod)
	if !ok || mpPod.Status.Phase != corev1.PodFailed || !r.isInMountpointNamespace(mpPod) || mppod.IsHeadroomPod(mpPod) {
		return nil
	}

	log := logf.FromContext(ctx).WithValues("mountpointPod", mpPod.Name)

	// Not filtering by node: a failed Pod might not have been scheduled to a node, and the number of
	// MountpointS3PodAttachments is small compared to the number of Pods (the stale attachment cleaner lists them all too).
	s3paList := &crdv2.MountpointS3PodAttachmentList{}
	if err := r.List(ctx, s3paList); err != nil {
		log.Error(err, "Failed to list MountpointS3PodAttachments for a failed Mountpoint Pod")
		return nil
	}
	workloadUIDs := map[string]struct{}{}
	for _, s3pa := range s3paList.Items {
		for _, attachment := range s3pa.Spec.MountpointS3PodAttachments[mpPod.Name] {
			workloadUIDs[attachment.WorkloadPodUID] = struct{}{}
		}
	}
	if len(workloadUIDs) == 0 {
		return nil
	}

	podList := &corev1.PodList{}
	if err := r.List(ctx, podList); err != nil {
		log.Error(err, "Failed to list Pods for a failed Mountpoint Pod")
		return nil
	}
	var requests []reconcile.Request
	for _, pod := range podList.Items {
		if _, ok := workloadUIDs[string(pod.UID)]; ok {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&pod)})
		}
	}
	return requests
}
