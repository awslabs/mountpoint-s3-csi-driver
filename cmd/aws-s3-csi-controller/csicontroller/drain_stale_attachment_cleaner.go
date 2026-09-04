package csicontroller

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	crdv2 "github.com/awslabs/mountpoint-s3-csi-driver/pkg/api/v2"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/podmounter/mppod"
)

// Drain-only cleanup cadence. The threshold guards against racing a freshly-created attachment: an
// attachment younger than this is left alone even if its workload pod isn't found yet.
const (
	drainCleanupInterval          = 2 * time.Minute
	drainStaleAttachmentThreshold = 2 * time.Minute
)

// DrainStaleAttachmentCleaner is the controller used in daemonset (V3) mounter mode.
//
// During a V2 -> V3 upgrade, existing V2 Mountpoint Pods must still be drained cleanly when their
// workloads terminate — otherwise they would hang indefinitely. Rather than an event-driven
// reconciler, drain-only mode runs a single periodic sweep that:
//
//   - Removes stale workload UIDs (whose pods no longer exist) from each MountpointS3PodAttachment,
//     annotates emptied Mountpoint Pods with `needs-unmount`, and deletes emptied S3PAs.
//   - Deletes completed (Succeeded) V2 Mountpoint Pods.
//   - Deletes leftover V2 Headroom Pods once their workload is gone or past scheduling.
//
// It never spawns new Mountpoint Pods, creates/updates S3PAs for active workloads, or creates
// Headroom Pods — those responsibilities belong to V3's DaemonsetMounter, not the controller.
//
// This type is intentionally SELF-CONTAINED: it depends only on the API client, so the V2
// spawn/create controller (reconciler.go) and its cleaner (stale_attachment_cleaner.go) can both be
// deleted without touching drain-only mode.
type DrainStaleAttachmentCleaner struct {
	client.Client
	mountpointNamespace string
}

// NewDrainStaleAttachmentCleaner creates a DrainStaleAttachmentCleaner. It needs the API client and
// the Mountpoint namespace (used to identify Mountpoint/Headroom Pods); drain-only mode never spawns
// pods so it needs no image/priority-class config.
func NewDrainStaleAttachmentCleaner(c client.Client, mountpointNamespace string) *DrainStaleAttachmentCleaner {
	return &DrainStaleAttachmentCleaner{Client: c, mountpointNamespace: mountpointNamespace}
}

// Start runs the periodic cleanup loop until the context is cancelled. Satisfies manager.Runnable.
func (cm *DrainStaleAttachmentCleaner) Start(ctx context.Context) error {
	log := logf.FromContext(ctx)
	log.Info("Starting drain-only stale attachment cleaner")

	ticker := time.NewTicker(drainCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("Completed drain-only stale attachment cleaner")
			return nil
		case <-ticker.C:
			if err := cm.RunCleanup(ctx); err != nil {
				log.Error(err, "Failed to run drain-only cleanup")
			}
		}
	}
}

// RunCleanup performs a single drain sweep: prune stale S3PA workload references, delete completed
// Mountpoint Pods, and delete leftover Headroom Pods.
func (cm *DrainStaleAttachmentCleaner) RunCleanup(ctx context.Context) error {
	log := logf.FromContext(ctx)

	podList := &corev1.PodList{}
	if err := cm.List(ctx, podList); err != nil {
		return err
	}
	// Create a map of existing pod UIDs for quick lookup
	existingPods := make(map[string]*corev1.Pod)
	for _, pod := range podList.Items {
		existingPods[string(pod.UID)] = &pod
	}

	// Always attempt Headroom Pod cleanup — in drain-only mode there is no feature-flag gate; any
	// leftover V2 Headroom Pods must be removed once their workload is gone or past scheduling.
	if err := cm.cleanupStaleHeadroomPods(ctx, existingPods); err != nil {
		log.Error(err, "Error cleaning up stale Headroom Pods")
	}

	// Delete completed (Succeeded) V2 Mountpoint Pods. These never get a new workload in drain-only
	// mode, so once they've cleanly unmounted and exited there is nothing left to keep them around.
	if err := cm.cleanupSucceededMountpointPods(ctx, existingPods); err != nil {
		log.Error(err, "Error cleaning up succeeded Mountpoint Pods")
	}

	s3paList := &crdv2.MountpointS3PodAttachmentList{}
	if err := cm.List(ctx, s3paList); err != nil {
		return err
	}
	for i := range s3paList.Items {
		if err := cm.cleanupStaleWorkloads(ctx, &s3paList.Items[i], existingPods); err != nil {
			log.Error(err, "Error cleaning up S3PodAttachment", "s3pa", s3paList.Items[i].Name)
			continue
		}
	}

	return nil
}

// cleanupStaleWorkloads removes workload references whose pod no longer exists (and whose attachment
// is older than drainStaleAttachmentThreshold, to avoid racing a fresh mount). Emptied Mountpoint
// Pods are annotated with needs-unmount and dropped; an emptied S3PA is deleted.
func (cm *DrainStaleAttachmentCleaner) cleanupStaleWorkloads(ctx context.Context, s3pa *crdv2.MountpointS3PodAttachment, existingPods map[string]*corev1.Pod) error {
	log := logf.FromContext(ctx).WithValues("s3pa", s3pa.Name)
	modified := false
	now := time.Now().UTC()

	for mpPodName, attachments := range s3pa.Spec.MountpointS3PodAttachments {
		var validAttachments []crdv2.WorkloadAttachment
		for _, attachment := range attachments {
			_, exists := existingPods[attachment.WorkloadPodUID]
			isStale := now.Sub(attachment.AttachmentTime.Time) > drainStaleAttachmentThreshold
			if exists || !isStale {
				validAttachments = append(validAttachments, attachment)
			} else {
				modified = true
				log.Info("Removing stale workload reference",
					"workloadUID", attachment.WorkloadPodUID, "mountpointPod", mpPodName,
					"attachmentAge", now.Sub(attachment.AttachmentTime.Time))
			}
		}

		if len(validAttachments) == 0 {
			if err := cm.addNeedsUnmountAnnotation(ctx, mpPodName, log); err != nil {
				return err
			}
			delete(s3pa.Spec.MountpointS3PodAttachments, mpPodName)
		} else {
			s3pa.Spec.MountpointS3PodAttachments[mpPodName] = validAttachments
		}
	}

	if modified {
		if len(s3pa.Spec.MountpointS3PodAttachments) == 0 {
			return cm.deleteS3PodAttachment(ctx, s3pa)
		}
		return cm.Update(ctx, s3pa)
	}
	return nil
}

// cleanupSucceededMountpointPods deletes V2 Mountpoint Pods that have completed (Succeeded).
func (cm *DrainStaleAttachmentCleaner) cleanupSucceededMountpointPods(ctx context.Context, existingPods map[string]*corev1.Pod) error {
	log := logf.FromContext(ctx)

	for _, pod := range existingPods {
		if !cm.isInMountpointNamespace(pod) || mppod.IsHeadroomPod(pod) {
			continue
		}
		if pod.Status.Phase != corev1.PodSucceeded {
			continue
		}
		if err := cm.Delete(ctx, pod); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			log.Error(err, "Failed to delete succeeded Mountpoint Pod", "mountpointPod", pod.Name)
			continue
		}
		log.Info("Deleted succeeded Mountpoint Pod", "mountpointPod", pod.Name)
	}
	return nil
}

// cleanupStaleHeadroomPods deletes leftover V2 Headroom Pods whose referenced workload no longer
// exists or is past scheduling.
func (cm *DrainStaleAttachmentCleaner) cleanupStaleHeadroomPods(ctx context.Context, existingPods map[string]*corev1.Pod) error {
	log := logf.FromContext(ctx)

	for _, pod := range existingPods {
		if !cm.isInMountpointNamespace(pod) || !mppod.IsHeadroomPod(pod) {
			continue
		}

		workloadPodUID := pod.Labels[mppod.LabelHeadroomForPod]
		if workloadPodUID == "" {
			log.Info("Headroom Pod missing workload pod UID label, skipping", "headroomPod", pod.Name)
			continue
		}

		workloadPod, exists := existingPods[workloadPodUID]
		if !exists || shouldDeleteHeadroomPodForTheWorkloadPod(workloadPod) {
			log.Info("Deleting stale Headroom Pod", "headroomPod", pod.Name, "workloadPodUID", workloadPodUID)
			if err := cm.Delete(ctx, pod); err != nil {
				log.Error(err, "Failed to delete stale Headroom Pod", "headroomPod", pod.Name, "workloadPodUID", workloadPodUID)
				continue
			}
		}
	}
	return nil
}

// addNeedsUnmountAnnotation annotates the Mountpoint Pod with `needs-unmount`, triggering the CSI
// node to cleanly unmount so the pod exits Succeeded.
func (cm *DrainStaleAttachmentCleaner) addNeedsUnmountAnnotation(ctx context.Context, mpPodName string, log logr.Logger) error {
	// Get the pod
	mpPod := &corev1.Pod{}
	err := cm.Get(ctx, types.NamespacedName{Namespace: cm.mountpointNamespace, Name: mpPodName}, mpPod)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Failed to find Mountpoint Pod - ignoring")
			return nil
		}
		log.Error(err, "Failed to get Pod")
		return err
	}

	if mpPod.Annotations == nil {
		mpPod.Annotations = make(map[string]string)
	}
	mpPod.Annotations[mppod.AnnotationNeedsUnmount] = "true"

	// Update the pod
	err = cm.Update(ctx, mpPod) // TODO: This probably needs to be a patch as we might've get a stale Mountpoint Pod.
	if err != nil {
		log.Error(err, "Failed to update Mountpoint Pod")
		return err
	}

	return nil
}

// deleteS3PodAttachment deletes the S3PA with a resourceVersion precondition so a concurrent
// modification causes a 409 Conflict (retried next sweep) rather than a lost update.
func (cm *DrainStaleAttachmentCleaner) deleteS3PodAttachment(ctx context.Context, s3pa *crdv2.MountpointS3PodAttachment) error {
	return cm.Delete(ctx, s3pa, client.Preconditions{ResourceVersion: &s3pa.ResourceVersion})
}

// isInMountpointNamespace reports whether `pod` is in the Mountpoint namespace.
func (cm *DrainStaleAttachmentCleaner) isInMountpointNamespace(pod *corev1.Pod) bool {
	return pod.Namespace == cm.mountpointNamespace
}

// SetupWithManager registers the cleaner as a manager Runnable.
func (cm *DrainStaleAttachmentCleaner) SetupWithManager(mgr ctrl.Manager) error {
	return mgr.Add(cm)
}

// shouldDeleteHeadroomPodForTheWorkloadPod returns whether Headroom Pods for `workloadPod` should be
// deleted: once the workload is terminating, or has moved past its Pending phase.
func shouldDeleteHeadroomPodForTheWorkloadPod(workloadPod *corev1.Pod) bool {
	if workloadPod.DeletionTimestamp != nil {
		// Workload Pod is terminating, we no longer need any Headroom Pods.
		return true
	}
	if workloadPod.Spec.NodeName != "" {
		// Workload Pod is scheduled; drop Headroom Pods once it starts running (past "Pending").
		return workloadPod.Status.Phase != corev1.PodPending
	}
	return false
}
