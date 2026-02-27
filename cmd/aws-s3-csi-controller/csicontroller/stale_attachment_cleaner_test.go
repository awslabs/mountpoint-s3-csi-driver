package csicontroller_test

import (
	"context"
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/awslabs/mountpoint-s3-csi-driver/cmd/aws-s3-csi-controller/csicontroller"
	crdv2 "github.com/awslabs/mountpoint-s3-csi-driver/pkg/api/v2"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/podmounter/mppod"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

const (
	mountpointNamespace = "mount-s3"

	testNode = "test-node"
)

func TestStaleAttachmentCleaner(t *testing.T) {
	t.Run("Headroom Pod Cleanup", func(t *testing.T) {
		testCases := []struct {
			name  string
			setup func() ([]client.Object, []client.Object)
		}{
			{
				name: "should delete orphaned Headroom Pod when Workload Pod doesn't exist",
				setup: func() ([]client.Object, []client.Object) {
					hrPod := newHeadroomPod(uuid.New().String())
					return []client.Object{hrPod}, []client.Object{hrPod}
				},
			},
			{
				name: "should not delete Headroom Pod when Workload Pod exists and is unscheduled",
				setup: func() ([]client.Object, []client.Object) {
					workloadPod := newWorkloadPod()
					hrPod := newHeadroomPod(string(workloadPod.GetUID()))
					return []client.Object{workloadPod, hrPod}, []client.Object{}
				},
			},
			{
				name: "should not delete Headroom Pod when Workload Pod is scheduled but pending",
				setup: func() ([]client.Object, []client.Object) {
					workloadPod := newWorkloadPod()
					workloadPod.Spec.NodeName = testNode
					workloadPod.Status.Phase = corev1.PodPending
					hrPod := newHeadroomPod(string(workloadPod.GetUID()))
					return []client.Object{workloadPod, hrPod}, []client.Object{}
				},
			},
			{
				name: "should delete Headroom Pod when Workload Pod is scheduled and running",
				setup: func() ([]client.Object, []client.Object) {
					workloadPod := newWorkloadPod()
					workloadPod.Spec.NodeName = testNode
					workloadPod.Status.Phase = corev1.PodRunning
					hrPod := newHeadroomPod(string(workloadPod.GetUID()))
					return []client.Object{workloadPod, hrPod}, []client.Object{hrPod}
				},
			},
			{
				name: "should delete Headroom Pod when Workload Pod is scheduled and succeeded",
				setup: func() ([]client.Object, []client.Object) {
					workloadPod := newWorkloadPod()
					workloadPod.Spec.NodeName = testNode
					workloadPod.Status.Phase = corev1.PodSucceeded
					hrPod := newHeadroomPod(string(workloadPod.GetUID()))
					return []client.Object{workloadPod, hrPod}, []client.Object{hrPod}
				},
			},
			{
				name: "should delete Headroom Pod when Workload Pod is scheduled and failed",
				setup: func() ([]client.Object, []client.Object) {
					workloadPod := newWorkloadPod()
					workloadPod.Spec.NodeName = testNode
					workloadPod.Status.Phase = corev1.PodFailed
					hrPod := newHeadroomPod(string(workloadPod.GetUID()))
					return []client.Object{workloadPod, hrPod}, []client.Object{hrPod}
				},
			},
			{
				name: "should delete Headroom Pod when Workload Pod is terminating",
				setup: func() ([]client.Object, []client.Object) {
					workloadPod := newWorkloadPod()
					workloadPod.DeletionTimestamp = ptr.To(metav1.Now())
					workloadPod.Finalizers = []string{"dummy-finalizer"}
					hrPod := newHeadroomPod(string(workloadPod.GetUID()))
					return []client.Object{workloadPod, hrPod}, []client.Object{hrPod}
				},
			},
			{
				name: "should handle mixed scenarios correctly",
				setup: func() ([]client.Object, []client.Object) {
					// Active Workload Pod (unscheduled) - Headroom Pod should stay
					activeWorkload := newWorkloadPod()
					activeHrPod := newHeadroomPod(string(activeWorkload.GetUID()))

					// Running Workload Pod - Headroom Pod should be deleted
					runningWorkload := newWorkloadPod()
					runningWorkload.Spec.NodeName = testNode
					runningWorkload.Status.Phase = corev1.PodRunning
					runningHrPod := newHeadroomPod(string(runningWorkload.GetUID()))

					// Orphaned Headroom Pod - should be deleted
					orphanedHrPod := newHeadroomPod(uuid.New().String())

					return []client.Object{
						activeWorkload, activeHrPod,
						runningWorkload, runningHrPod,
						orphanedHrPod,
					}, []client.Object{runningHrPod, orphanedHrPod}
				},
			},
			{
				name: "should handle multiple Headroom Pods for the same Workload Pod",
				setup: func() ([]client.Object, []client.Object) {
					workloadPod := newWorkloadPod()
					workloadPod.Spec.NodeName = testNode
					workloadPod.Status.Phase = corev1.PodRunning

					hrPod1 := newHeadroomPod(string(workloadPod.GetUID()))
					hrPod2 := newHeadroomPod(string(workloadPod.GetUID()))

					return []client.Object{workloadPod, hrPod1, hrPod2}, []client.Object{hrPod1, hrPod2}
				},
			},
			{
				name: "should ignore Headroom-named Pods in different namespaces",
				setup: func() ([]client.Object, []client.Object) {
					orphanedUID := uuid.New().String()

					// Headroom Pod in correct namespace - should be deleted
					hrPodCorrectNs := newHeadroomPod(orphanedUID)

					// Headroom-named Pod in different namespace - should be ignored
					hrPodDiffNs := newHeadroomPod(orphanedUID)
					hrPodDiffNs.Namespace = "some-other-namespace"

					return []client.Object{hrPodCorrectNs, hrPodDiffNs}, []client.Object{hrPodCorrectNs}
				},
			},
			{
				name: "should handle empty cluster gracefully",
				setup: func() ([]client.Object, []client.Object) {
					return nil, nil
				},
			},
		}

		for _, testCase := range testCases {
			t.Run(testCase.name, func(t *testing.T) {
				allPods, shouldBeDeleted := testCase.setup()
				client, cleaner := createStaleAttachmentCleaner(t, allPods, true)

				err := cleaner.RunCleanup(context.Background())
				assert.NoError(t, err)

				verifyHeadroomPodDeletions(t, client, shouldBeDeleted)
			})
		}

		t.Run("should not delete any Headroom Pods if feature is disabled", func(t *testing.T) {
			// Active Workload Pod
			activeWorkload := newWorkloadPod()
			activeHrPod := newHeadroomPod(string(activeWorkload.GetUID()))

			// Running Workload Pod
			runningWorkload := newWorkloadPod()
			runningWorkload.Spec.NodeName = testNode
			runningWorkload.Status.Phase = corev1.PodRunning
			runningHrPod := newHeadroomPod(string(runningWorkload.GetUID()))

			// Orphaned Headroom Pod
			orphanedHrPod := newHeadroomPod(uuid.New().String())

			allPods := []client.Object{
				activeWorkload, activeHrPod,
				runningWorkload, runningHrPod,
				orphanedHrPod,
			}

			c, cleaner := createStaleAttachmentCleaner(t, allPods, false)

			err := cleaner.RunCleanup(context.Background())
			assert.NoError(t, err)

			for _, pod := range allPods {
				err := c.Get(t.Context(), client.ObjectKeyFromObject(pod), pod)
				// Pod's shouldn't be deleted and `Get` should work without any errors
				assert.NoError(t, err)
			}
		})
	})

}

func verifyHeadroomPodDeletions(t *testing.T, c client.Client, expectedDeleted []client.Object) {
	for _, obj := range expectedDeleted {
		err := c.Get(t.Context(), client.ObjectKeyFromObject(obj), &corev1.Pod{})
		if err == nil {
			t.Errorf("Expected Headroom Pod %q to be deleted, but it still exists", obj.GetName())
		} else if !errors.IsNotFound(err) {
			t.Errorf("Unexpected error checking Headroom Pod %q: %v", obj.GetName(), err)
		}
	}
}

func createStaleAttachmentCleaner(t *testing.T, existingPods []client.Object, headroomPodsEnabled bool) (client.Client, *csicontroller.StaleAttachmentCleaner) {
	client := fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(existingPods...).
		Build()

	reconciler := csicontroller.NewReconciler(client, mppodConfig(headroomPodsEnabled), testr.New(t))
	cleaner := csicontroller.NewStaleAttachmentCleaner(reconciler)
	return client, cleaner
}

func mppodConfig(headroomPodsEnabled bool) mppod.Config {
	config := mppod.Config{
		Namespace:    mountpointNamespace,
		CustomLabels: map[string]string{},
		PodLabels:    map[string]string{},
	}

	if headroomPodsEnabled {
		config.HeadroomPriorityClassName = "headroom-priority"
		config.PreemptingPriorityClassName = "preempting-priority"
	}

	return config
}

func newWorkloadPod() *corev1.Pod {
	uid := uuid.New().String()
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "workload-" + uid[:8],
			Namespace: "default",
			UID:       types.UID(uid),
		},
	}
}

func newHeadroomPod(workloadPodUID string) *corev1.Pod {
	uid := uuid.New().String()
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "hr-pod-" + uid[:8],
			Namespace: mountpointNamespace,
			Labels: map[string]string{
				mppod.LabelHeadroomForPod: workloadPodUID,
			},
			UID: types.UID(uid),
		},
	}
}

func testScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(crdv2.AddToScheme(scheme))
	return scheme
}
