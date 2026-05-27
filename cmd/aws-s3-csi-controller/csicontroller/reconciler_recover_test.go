package csicontroller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	crdv2 "github.com/awslabs/mountpoint-s3-csi-driver/pkg/api/v2"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/podmounter/mppod"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

func TestRecoverFromDuplicateS3PodAttachments(t *testing.T) {
	t.Run("returns the S3PA that references Mountpoint Pods and deletes the duplicate that references none", func(t *testing.T) {
		withMountpointPods := newS3PA("s3pa-live", map[string][]crdv2.WorkloadAttachment{
			"mp-1": {{WorkloadPodUID: "uid-1", AttachmentTime: metav1.NewTime(metav1.Now().Time)}},
		})
		noMountpointPods := newS3PA("s3pa-orphan", map[string][]crdv2.WorkloadAttachment{})
		c, r := newReconcilerWithObjects(t, withMountpointPods, noMountpointPods)

		got, err := r.recoverFromDuplicateS3PodAttachments(context.Background(), []crdv2.MountpointS3PodAttachment{*withMountpointPods, *noMountpointPods}, testFilters(), logr.Discard())
		assert.NoError(t, err)
		if got == nil || got.Name != withMountpointPods.Name {
			t.Fatalf("expected to return %q, got %v", withMountpointPods.Name, got)
		}
		assertS3PAExists(t, c, withMountpointPods.Name)
		assertS3PADeleted(t, c, noMountpointPods.Name)
	})

	t.Run("returns nil and deletes all duplicates when none reference Mountpoint Pods", func(t *testing.T) {
		noMountpointPodsA := newS3PA("s3pa-empty-a", map[string][]crdv2.WorkloadAttachment{})
		noMountpointPodsB := newS3PA("s3pa-empty-b", map[string][]crdv2.WorkloadAttachment{})
		c, r := newReconcilerWithObjects(t, noMountpointPodsA, noMountpointPodsB)

		got, err := r.recoverFromDuplicateS3PodAttachments(context.Background(), []crdv2.MountpointS3PodAttachment{*noMountpointPodsA, *noMountpointPodsB}, testFilters(), logr.Discard())
		assert.NoError(t, err)
		if got != nil {
			t.Fatalf("expected nil S3PA when no duplicate references Mountpoint Pods, got %q", got.Name)
		}
		assertS3PADeleted(t, c, noMountpointPodsA.Name)
		assertS3PADeleted(t, c, noMountpointPodsB.Name)
	})

	t.Run("clears stale pending creation expectation when all duplicates referenced no Mountpoint Pods are deleted", func(t *testing.T) {
		noMountpointPodsA := newS3PA("s3pa-stuck-a", map[string][]crdv2.WorkloadAttachment{})
		noMountpointPodsB := newS3PA("s3pa-stuck-b", map[string][]crdv2.WorkloadAttachment{})
		c, r := newReconcilerWithObjects(t, noMountpointPodsA, noMountpointPodsB)
		filters := testFilters()
		r.s3paExpectations.setPending(filters)

		got, err := r.recoverFromDuplicateS3PodAttachments(context.Background(), []crdv2.MountpointS3PodAttachment{*noMountpointPodsA, *noMountpointPodsB}, filters, logr.Discard())
		assert.NoError(t, err)
		if got != nil {
			t.Fatalf("expected nil S3PA when no duplicate references Mountpoint Pods, got %q", got.Name)
		}
		assertS3PADeleted(t, c, noMountpointPodsA.Name)
		assertS3PADeleted(t, c, noMountpointPodsB.Name)
		if r.s3paExpectations.isPending(filters) {
			t.Errorf("expected pending creation expectation to be cleared after deleting all duplicates that referenced no Mountpoint Pods")
		}
	})

	t.Run("does not clear pending expectation when a duplicate that references Mountpoint Pods survives", func(t *testing.T) {
		// When recovery returns a surviving S3PA, the same reconcile continues into
		// handleExistingS3PodAttachment (active-pod path), which clears pending.
		withMountpointPods := newS3PA("s3pa-live", map[string][]crdv2.WorkloadAttachment{
			"mp-1": {{WorkloadPodUID: "uid-1", AttachmentTime: metav1.NewTime(metav1.Now().Time)}},
		})
		noMountpointPods := newS3PA("s3pa-orphan", map[string][]crdv2.WorkloadAttachment{})
		c, r := newReconcilerWithObjects(t, withMountpointPods, noMountpointPods)
		filters := testFilters()
		r.s3paExpectations.setPending(filters)

		got, err := r.recoverFromDuplicateS3PodAttachments(context.Background(), []crdv2.MountpointS3PodAttachment{*withMountpointPods, *noMountpointPods}, filters, logr.Discard())
		assert.NoError(t, err)
		if got == nil || got.Name != withMountpointPods.Name {
			t.Fatalf("expected to return %q, got %v", withMountpointPods.Name, got)
		}
		assertS3PADeleted(t, c, noMountpointPods.Name)
		if !r.s3paExpectations.isPending(filters) {
			t.Errorf("expected pending creation expectation to remain when a surviving S3PA references Mountpoint Pods")
		}
	})

	t.Run("returns error when more than one duplicate references Mountpoint Pods", func(t *testing.T) {
		withMountpointPodsA := newS3PA("s3pa-live-a", map[string][]crdv2.WorkloadAttachment{
			"mp-a": {{WorkloadPodUID: "uid-a", AttachmentTime: metav1.NewTime(metav1.Now().Time)}},
		})
		withMountpointPodsB := newS3PA("s3pa-live-b", map[string][]crdv2.WorkloadAttachment{
			"mp-b": {{WorkloadPodUID: "uid-b", AttachmentTime: metav1.NewTime(metav1.Now().Time)}},
		})
		noMountpointPods := newS3PA("s3pa-orphan", map[string][]crdv2.WorkloadAttachment{})
		c, r := newReconcilerWithObjects(t, withMountpointPodsA, withMountpointPodsB, noMountpointPods)

		got, err := r.recoverFromDuplicateS3PodAttachments(context.Background(), []crdv2.MountpointS3PodAttachment{*withMountpointPodsA, *withMountpointPodsB, *noMountpointPods}, testFilters(), logr.Discard())
		if err == nil {
			t.Fatalf("expected an error when multiple S3PAs reference Mountpoint Pods, got nil")
		}
		if !strings.Contains(err.Error(), "referencing Mountpoint Pods") {
			t.Errorf("expected error to mention S3PAs referencing Mountpoint Pods, got: %v", err)
		}
		if got != nil {
			t.Errorf("expected nil S3PA on error, got %q", got.Name)
		}
		assertS3PAExists(t, c, withMountpointPodsA.Name)
		assertS3PAExists(t, c, withMountpointPodsB.Name)
		assertS3PAExists(t, c, noMountpointPods.Name)
	})

	t.Run("returns error and preserves pending when deleting a duplicate that references no Mountpoint Pods fails with conflict", func(t *testing.T) {
		noMountpointPodsA := newS3PA("s3pa-empty-a", map[string][]crdv2.WorkloadAttachment{})
		noMountpointPodsB := newS3PA("s3pa-empty-b", map[string][]crdv2.WorkloadAttachment{})

		c := fake.NewClientBuilder().
			WithScheme(testScheme()).
			WithObjects(noMountpointPodsA, noMountpointPodsB).
			WithInterceptorFuncs(interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if s3pa, ok := obj.(*crdv2.MountpointS3PodAttachment); ok && s3pa.Name == noMountpointPodsA.Name {
						current := &crdv2.MountpointS3PodAttachment{}
						if err := c.Get(ctx, client.ObjectKey{Name: noMountpointPodsA.Name}, current); err != nil {
							return err
						}
						current.Spec.MountpointS3PodAttachments = map[string][]crdv2.WorkloadAttachment{
							"mp-x": {{WorkloadPodUID: "uid-x", AttachmentTime: metav1.NewTime(metav1.Now().Time)}},
						}
						if err := c.Update(ctx, current); err != nil {
							return err
						}
					}
					return c.Delete(ctx, obj, opts...)
				},
			}).
			Build()
		r := &Reconciler{Client: c, mountpointPodConfig: testPodConfig(), s3paExpectations: newExpectations()}
		filters := testFilters()
		r.s3paExpectations.setPending(filters)

		got, err := r.recoverFromDuplicateS3PodAttachments(context.Background(), []crdv2.MountpointS3PodAttachment{*noMountpointPodsA, *noMountpointPodsB}, filters, logr.Discard())
		if err == nil {
			t.Fatalf("expected error when delete fails, got nil (got=%v)", got)
		}
		if !apierrors.IsConflict(errors.Unwrap(err)) {
			t.Errorf("expected error to wrap an IsConflict, got: %v", err)
		}
		if got != nil {
			t.Errorf("expected nil S3PA on error, got %q", got.Name)
		}
		if !r.s3paExpectations.isPending(filters) {
			t.Errorf("expected pending creation expectation to remain after a failed delete")
		}
		assertS3PAExists(t, c, noMountpointPodsA.Name)
	})
}

// helpers

const (
	testNodeName = "test-node"
	testPVName   = "test-pv"
	testVolumeID = "test-vol-id"
)

func newS3PA(name string, attachments map[string][]crdv2.WorkloadAttachment) *crdv2.MountpointS3PodAttachment {
	return &crdv2.MountpointS3PodAttachment{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: crdv2.MountpointS3PodAttachmentSpec{
			NodeName:                   testNodeName,
			PersistentVolumeName:       testPVName,
			VolumeID:                   testVolumeID,
			MountpointS3PodAttachments: attachments,
		},
	}
}

func newReconcilerWithObjects(t *testing.T, objs ...client.Object) (client.Client, *Reconciler) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(objs...).
		Build()
	return c, &Reconciler{Client: c, mountpointPodConfig: testPodConfig(), s3paExpectations: newExpectations()}
}

func testPodConfig() mppod.Config {
	return mppod.Config{
		Namespace:         "mount-s3",
		PodLabels:         map[string]string{},
		HeadroomPodLabels: map[string]string{},
	}
}

func testFilters() client.MatchingFields {
	return client.MatchingFields{
		crdv2.FieldNodeName:             testNodeName,
		crdv2.FieldPersistentVolumeName: testPVName,
		crdv2.FieldVolumeID:             testVolumeID,
	}
}

func testScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(crdv2.AddToScheme(scheme))
	return scheme
}

func assertS3PAExists(t *testing.T, c client.Client, name string) {
	t.Helper()
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, &crdv2.MountpointS3PodAttachment{}); err != nil {
		t.Errorf("expected S3PodAttachment %q to exist, got error: %v", name, err)
	}
}

func assertS3PADeleted(t *testing.T, c client.Client, name string) {
	t.Helper()
	err := c.Get(context.Background(), client.ObjectKey{Name: name}, &crdv2.MountpointS3PodAttachment{})
	if err == nil {
		t.Errorf("expected S3PodAttachment %q to be deleted, but it still exists", name)
	} else if !apierrors.IsNotFound(err) {
		t.Errorf("unexpected error checking S3PodAttachment %q: %v", name, err)
	}
}
