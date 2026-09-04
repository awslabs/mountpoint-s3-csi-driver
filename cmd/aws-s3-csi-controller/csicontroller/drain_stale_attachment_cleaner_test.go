package csicontroller_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/awslabs/mountpoint-s3-csi-driver/cmd/aws-s3-csi-controller/csicontroller"
	crdv2 "github.com/awslabs/mountpoint-s3-csi-driver/pkg/api/v2"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/podmounter/mppod"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

// newDrainCleaner builds a DrainStaleAttachmentCleaner backed by a fake client seeded with objs.
func newDrainCleaner(t *testing.T, objs ...client.Object) (client.Client, *csicontroller.DrainStaleAttachmentCleaner) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).Build()
	return c, csicontroller.NewDrainStaleAttachmentCleaner(c, mountpointNamespace)
}

// newMountpointPod returns a Mountpoint Pod (in the Mountpoint namespace, not a Headroom Pod) with
// the given phase.
func newMountpointPod(name string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: mountpointNamespace,
			UID:       types.UID(uuid.New().String()),
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

// s3paWith builds an S3PA whose single Mountpoint Pod holds the given workload UIDs, each attached
// `age` ago.
func s3paWith(name, mpPodName string, age time.Duration, workloadUIDs ...string) *crdv2.MountpointS3PodAttachment {
	attachTime := metav1.NewTime(time.Now().UTC().Add(-age))
	var atts []crdv2.WorkloadAttachment
	for _, uid := range workloadUIDs {
		atts = append(atts, crdv2.WorkloadAttachment{WorkloadPodUID: uid, AttachmentTime: attachTime})
	}
	return &crdv2.MountpointS3PodAttachment{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: crdv2.MountpointS3PodAttachmentSpec{
			NodeName:                   testNode,
			PersistentVolumeName:       "test-pv",
			VolumeID:                   "test-vol-id",
			MountpointS3PodAttachments: map[string][]crdv2.WorkloadAttachment{mpPodName: atts},
		},
	}
}

func TestDrainStaleAttachmentCleaner(t *testing.T) {
	ctx := context.Background()

	t.Run("Stale workload cleanup", func(t *testing.T) {
		t.Run("removes stale workload, annotates emptied Mountpoint Pod, deletes emptied S3PA", func(t *testing.T) {
			staleUID := uuid.New().String()
			mpPod := newMountpointPod("mp-1", corev1.PodRunning)
			// Attachment is old and its workload pod does NOT exist -> stale.
			s3pa := s3paWith("s3pa-1", mpPod.Name, 5*time.Minute, staleUID)

			c, cleaner := newDrainCleaner(t, mpPod, s3pa)
			assert.NoError(t, cleaner.RunCleanup(ctx))

			// Mountpoint Pod annotated needs-unmount.
			got := &corev1.Pod{}
			assert.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: mountpointNamespace, Name: mpPod.Name}, got))
			if got.Annotations[mppod.AnnotationNeedsUnmount] != "true" {
				t.Fatalf("expected needs-unmount annotation, got %v", got.Annotations)
			}

			// S3PA deleted (no Mountpoint Pods left).
			err := c.Get(ctx, client.ObjectKey{Name: s3pa.Name}, &crdv2.MountpointS3PodAttachment{})
			if !apierrors.IsNotFound(err) {
				t.Fatalf("expected S3PA to be deleted, get err=%v", err)
			}
		})

		t.Run("keeps attachment whose workload pod still exists", func(t *testing.T) {
			workload := newWorkloadPod()
			mpPod := newMountpointPod("mp-1", corev1.PodRunning)
			s3pa := s3paWith("s3pa-1", mpPod.Name, 5*time.Minute, string(workload.UID))

			c, cleaner := newDrainCleaner(t, workload, mpPod, s3pa)
			assert.NoError(t, cleaner.RunCleanup(ctx))

			got := &crdv2.MountpointS3PodAttachment{}
			assert.NoError(t, c.Get(ctx, client.ObjectKey{Name: s3pa.Name}, got))
			if len(got.Spec.MountpointS3PodAttachments[mpPod.Name]) != 1 {
				t.Fatalf("expected attachment to be kept, got %v", got.Spec.MountpointS3PodAttachments)
			}
		})

		t.Run("keeps attachment that is too new even if workload pod is missing", func(t *testing.T) {
			staleUID := uuid.New().String()
			mpPod := newMountpointPod("mp-1", corev1.PodRunning)
			// Missing workload, but attachment is younger than the staleness threshold.
			s3pa := s3paWith("s3pa-1", mpPod.Name, 30*time.Second, staleUID)

			c, cleaner := newDrainCleaner(t, mpPod, s3pa)
			assert.NoError(t, cleaner.RunCleanup(ctx))

			got := &crdv2.MountpointS3PodAttachment{}
			assert.NoError(t, c.Get(ctx, client.ObjectKey{Name: s3pa.Name}, got))
			if len(got.Spec.MountpointS3PodAttachments[mpPod.Name]) != 1 {
				t.Fatalf("expected new attachment to be kept, got %v", got.Spec.MountpointS3PodAttachments)
			}
		})
	})

	t.Run("Succeeded Mountpoint Pod cleanup", func(t *testing.T) {
		t.Run("deletes Succeeded Mountpoint Pod", func(t *testing.T) {
			mpPod := newMountpointPod("mp-done", corev1.PodSucceeded)
			c, cleaner := newDrainCleaner(t, mpPod)
			assert.NoError(t, cleaner.RunCleanup(ctx))

			err := c.Get(ctx, client.ObjectKey{Namespace: mountpointNamespace, Name: mpPod.Name}, &corev1.Pod{})
			if !apierrors.IsNotFound(err) {
				t.Fatalf("expected succeeded Mountpoint Pod to be deleted, get err=%v", err)
			}
		})

		t.Run("does not delete Running Mountpoint Pod", func(t *testing.T) {
			mpPod := newMountpointPod("mp-running", corev1.PodRunning)
			c, cleaner := newDrainCleaner(t, mpPod)
			assert.NoError(t, cleaner.RunCleanup(ctx))

			assert.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: mountpointNamespace, Name: mpPod.Name}, &corev1.Pod{}))
		})
	})

	t.Run("Headroom Pod cleanup", func(t *testing.T) {
		t.Run("deletes leftover Headroom Pod when workload is gone", func(t *testing.T) {
			hrPod := newHeadroomPod(uuid.New().String())
			c, cleaner := newDrainCleaner(t, hrPod)
			assert.NoError(t, cleaner.RunCleanup(ctx))

			err := c.Get(ctx, client.ObjectKey{Namespace: mountpointNamespace, Name: hrPod.Name}, &corev1.Pod{})
			if !apierrors.IsNotFound(err) {
				t.Fatalf("expected leftover Headroom Pod to be deleted, get err=%v", err)
			}
		})

		t.Run("keeps Headroom Pod when workload exists and is unscheduled", func(t *testing.T) {
			workload := newWorkloadPod()
			hrPod := newHeadroomPod(string(workload.UID))
			c, cleaner := newDrainCleaner(t, workload, hrPod)
			assert.NoError(t, cleaner.RunCleanup(ctx))

			assert.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: mountpointNamespace, Name: hrPod.Name}, &corev1.Pod{}))
		})
	})
}
