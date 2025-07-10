package csicontroller

import (
	"context"
	"time"

	crdv2beta "github.com/awslabs/mountpoint-s3-csi-driver/pkg/api/v2beta"
	corev1 "k8s.io/api/core/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	cleanupInterval          = 2 * time.Minute
	staleAttachmentThreshold = 2 * time.Minute
)

// StaleAttachmentCleaner handles periodic cleanup of stale workload attachments in case reconciler missed pod deletion event.
type StaleAttachmentCleaner struct {
	reconciler *Reconciler
}

// NewStaleAttachmentCleaner creates a new StaleAttachmentCleaner
func NewStaleAttachmentCleaner(reconciler *Reconciler) *StaleAttachmentCleaner {
	return &StaleAttachmentCleaner{
		reconciler: reconciler,
	}
}

// Start begins the periodic cleanup process
func (cm *StaleAttachmentCleaner) Start(ctx context.Context) error {
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := cm.runCleanup(ctx); err != nil {
				log := logf.FromContext(ctx)
				log.Error(err, "Failed to run cleanup")
			}
		}
	}
}

// runCleanup performs cleanup operation
func (cm *StaleAttachmentCleaner) runCleanup(ctx context.Context) error {
	log := logf.FromContext(ctx)

	// Get all pods in the cluster
	podList := &corev1.PodList{}
	if err := cm.reconciler.List(ctx, podList); err != nil {
		return err
	}

	// Create a map of existing pod UIDs for quick lookup
	existingPods := make(map[string]struct{})
	for _, pod := range podList.Items {
		existingPods[string(pod.UID)] = struct{}{}
	}

	// Get all MountpointS3PodAttachments
	s3paList := &crdv2beta.MountpointS3PodAttachmentList{}
	if err := cm.reconciler.List(ctx, s3paList); err != nil {
		return err
	}

	// Check each S3PodAttachment for stale workload references
	for _, s3pa := range s3paList.Items {
		if err := cm.cleanupStaleWorkloads(ctx, &s3pa, existingPods); err != nil {
			log.Error(err, "Error cleaning up S3PodAttachment", "s3pa", s3pa.Name)
			continue
		}
	}

	return nil
}

// cleanupStaleWorkloads removes stale workload references from a single S3PodAttachment.
// A workload reference is considered stale if the referenced Pod no longer exists in the cluster
// and the attachment is older than staleAttachmentThreshold (this is to avoid race condition with reconciler).
// If a Mountpoint Pod has zero attachments after cleanup, "s3.csi.aws.com/needs-unmount" annotation is added and its entry in S3PodAttachment is deleted.
// If S3PodAttachment has no remaining Mountpoint Pods, the entire S3PodAttachment is deleted.
func (cm *StaleAttachmentCleaner) cleanupStaleWorkloads(ctx context.Context, s3pa *crdv2beta.MountpointS3PodAttachment, existingPods map[string]struct{}) error {
	log := logf.FromContext(ctx).WithValues("s3pa", s3pa.Name)
	modified := false

	now := time.Now().UTC()

	// Check each mountpoint pod's attachments
	for mpPodName, attachments := range s3pa.Spec.MountpointS3PodAttachments {
		var validAttachments []crdv2beta.WorkloadAttachment

		for _, attachment := range attachments {
			// Check if pod exists and attachment is not too new
			_, exists := existingPods[attachment.WorkloadPodUID]
			isStale := now.Sub(attachment.AttachmentTime.Time) > staleAttachmentThreshold

			if exists || !isStale {
				validAttachments = append(validAttachments, attachment)
			} else {
				modified = true
				log.Info("Removing stale workload reference",
					"workloadUID", attachment.WorkloadPodUID,
					"mountpointPod", mpPodName,
					"attachmentAge", now.Sub(attachment.AttachmentTime.Time))
			}
		}

		if len(validAttachments) == 0 {
			if err := cm.reconciler.addNeedsUnmountAnnotation(ctx, mpPodName, log); err != nil {
				return err
			}
			delete(s3pa.Spec.MountpointS3PodAttachments, mpPodName)
		} else {
			s3pa.Spec.MountpointS3PodAttachments[mpPodName] = validAttachments
		}
	}

	// Update the S3PodAttachment if modified
	if modified {
		if len(s3pa.Spec.MountpointS3PodAttachments) == 0 {
			return cm.reconciler.Delete(ctx, s3pa)
		}
		return cm.reconciler.Update(ctx, s3pa)
	}

	return nil
}
