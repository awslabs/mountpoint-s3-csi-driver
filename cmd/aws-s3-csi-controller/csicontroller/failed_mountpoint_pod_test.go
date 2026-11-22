package csicontroller

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	crdv2 "github.com/awslabs/mountpoint-s3-csi-driver/pkg/api/v2"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/podmounter/mppod"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

const testMPNamespace = "mount-s3"

func TestHandleWorkloadOfFailedMountpointPod(t *testing.T) {
	attachedTo := func(mpPodName string, uids ...string) map[string][]crdv2.WorkloadAttachment {
		var attachments []crdv2.WorkloadAttachment
		for _, uid := range uids {
			attachments = append(attachments, crdv2.WorkloadAttachment{WorkloadPodUID: uid, AttachmentTime: metav1.Now()})
		}
		return map[string][]crdv2.WorkloadAttachment{mpPodName: attachments}
	}

	t.Run("moves a pending workload to a new Mountpoint Pod when its Mountpoint Pod was evicted", func(t *testing.T) {
		failed := newMPPod("mp-failed", corev1.PodFailed, "Evicted")
		s3pa := newS3PA("s3pa-1", attachedTo("mp-failed", "workload-1"))
		c, r := newReconcilerWithObjects(t, failed, s3pa)

		handled, requeue, err := r.handleWorkloadOfFailedMountpointPod(context.Background(), newWorkloadPod("workload-1", corev1.PodPending), newPV(), s3pa, mppod.DefaultPriorityClass, logr.Discard())
		assert.NoError(t, err)
		assert.Equals(t, true, handled)
		assert.Equals(t, false, requeue)

		got := getS3PA(t, c, "s3pa-1")
		if _, ok := got.Spec.MountpointS3PodAttachments["mp-failed"]; ok {
			t.Errorf("expected failed Mountpoint Pod to be removed from MountpointS3PodAttachment, got %v", got.Spec.MountpointS3PodAttachments)
		}
		assert.Equals(t, 1, len(got.Spec.MountpointS3PodAttachments))
		for name, attachments := range got.Spec.MountpointS3PodAttachments {
			assert.Equals(t, 1, len(attachments))
			assert.Equals(t, "workload-1", attachments[0].WorkloadPodUID)
			newPod := &corev1.Pod{}
			assert.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: testMPNamespace, Name: name}, newPod))
			assert.Equals(t, "mp-", newPod.GenerateName)
		}
	})

	t.Run("deletes the failed Mountpoint Pod once the last workload has moved away", func(t *testing.T) {
		failed := newMPPod("mp-failed", corev1.PodFailed, "Evicted")
		s3pa := newS3PA("s3pa-1", attachedTo("mp-failed", "workload-1"))
		c, r := newReconcilerWithObjects(t, failed, s3pa)

		_, _, err := r.handleWorkloadOfFailedMountpointPod(context.Background(), newWorkloadPod("workload-1", corev1.PodPending), newPV(), s3pa, mppod.DefaultPriorityClass, logr.Discard())
		assert.NoError(t, err)

		err = c.Get(context.Background(), client.ObjectKeyFromObject(failed), &corev1.Pod{})
		if !apierrors.IsNotFound(err) {
			t.Errorf("expected failed Mountpoint Pod to be deleted, got %v", err)
		}
		assert.Equals(t, 1, countMountpointPods(t, c)) // only the new one is left
	})

	t.Run("keeps other workloads attached to the failed Mountpoint Pod for their own reconciliation", func(t *testing.T) {
		failed := newMPPod("mp-failed", corev1.PodFailed, "Evicted")
		s3pa := newS3PA("s3pa-1", attachedTo("mp-failed", "workload-1", "workload-2"))
		c, r := newReconcilerWithObjects(t, failed, s3pa)

		handled, _, err := r.handleWorkloadOfFailedMountpointPod(context.Background(), newWorkloadPod("workload-1", corev1.PodPending), newPV(), s3pa, mppod.DefaultPriorityClass, logr.Discard())
		assert.NoError(t, err)
		assert.Equals(t, true, handled)

		got := getS3PA(t, c, "s3pa-1")
		assert.Equals(t, 2, len(got.Spec.MountpointS3PodAttachments))
		assert.Equals(t, 1, len(got.Spec.MountpointS3PodAttachments["mp-failed"]))
		assert.Equals(t, "workload-2", got.Spec.MountpointS3PodAttachments["mp-failed"][0].WorkloadPodUID)
		assert.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(failed), &corev1.Pod{})) // still referenced

		// The second workload joins the Mountpoint Pod spawned for the first one rather than spawning another.
		handled, _, err = r.handleWorkloadOfFailedMountpointPod(context.Background(), newWorkloadPod("workload-2", corev1.PodPending), newPV(), got, mppod.DefaultPriorityClass, logr.Discard())
		assert.NoError(t, err)
		assert.Equals(t, true, handled)

		got = getS3PA(t, c, "s3pa-1")
		assert.Equals(t, 1, len(got.Spec.MountpointS3PodAttachments))
		for _, attachments := range got.Spec.MountpointS3PodAttachments {
			assert.Equals(t, 2, len(attachments))
		}
		// The failed Pod is deleted with the last workload that moves away from it
		err = c.Get(context.Background(), client.ObjectKeyFromObject(failed), &corev1.Pod{})
		if !apierrors.IsNotFound(err) {
			t.Errorf("expected failed Mountpoint Pod to be deleted, got %v", err)
		}
		assert.Equals(t, 1, countMountpointPods(t, c))
	})

	t.Run("assigns the workload to another healthy Mountpoint Pod of the S3PA instead of spawning one", func(t *testing.T) {
		failed := newMPPod("mp-failed", corev1.PodFailed, "Evicted")
		healthy := newMPPod("mp-healthy", corev1.PodRunning, "")
		attachments := attachedTo("mp-failed", "workload-1")
		attachments["mp-healthy"] = []crdv2.WorkloadAttachment{{WorkloadPodUID: "workload-9", AttachmentTime: metav1.Now()}}
		s3pa := newS3PA("s3pa-1", attachments)
		c, r := newReconcilerWithObjects(t, failed, healthy, s3pa)

		handled, _, err := r.handleWorkloadOfFailedMountpointPod(context.Background(), newWorkloadPod("workload-1", corev1.PodPending), newPV(), s3pa, mppod.DefaultPriorityClass, logr.Discard())
		assert.NoError(t, err)
		assert.Equals(t, true, handled)

		got := getS3PA(t, c, "s3pa-1")
		assert.Equals(t, 1, len(got.Spec.MountpointS3PodAttachments))
		assert.Equals(t, 2, len(got.Spec.MountpointS3PodAttachments["mp-healthy"]))
		assert.Equals(t, 1, countMountpointPods(t, c)) // no new Pod, and the failed one is gone
	})

	t.Run("leaves a running workload attached, as its mount can only be fixed by restarting it", func(t *testing.T) {
		failed := newMPPod("mp-failed", corev1.PodFailed, "Evicted")
		s3pa := newS3PA("s3pa-1", attachedTo("mp-failed", "workload-1"))
		c, r := newReconcilerWithObjects(t, failed, s3pa)

		handled, requeue, err := r.handleWorkloadOfFailedMountpointPod(context.Background(), newWorkloadPod("workload-1", corev1.PodRunning), newPV(), s3pa, mppod.DefaultPriorityClass, logr.Discard())
		assert.NoError(t, err)
		assert.Equals(t, false, handled)
		assert.Equals(t, false, requeue)

		assert.Equals(t, 1, len(getS3PA(t, c, "s3pa-1").Spec.MountpointS3PodAttachments["mp-failed"]))
		assert.Equals(t, 1, countMountpointPods(t, c)) // nothing spawned, nothing deleted
	})

	t.Run("does nothing if the Mountpoint Pod is not failed", func(t *testing.T) {
		for _, phase := range []corev1.PodPhase{corev1.PodPending, corev1.PodRunning} {
			mpPod := newMPPod("mp-1", phase, "")
			s3pa := newS3PA("s3pa-1", attachedTo("mp-1", "workload-1"))
			c, r := newReconcilerWithObjects(t, mpPod, s3pa)

			handled, _, err := r.handleWorkloadOfFailedMountpointPod(context.Background(), newWorkloadPod("workload-1", corev1.PodPending), newPV(), s3pa, mppod.DefaultPriorityClass, logr.Discard())
			assert.NoError(t, err)
			assert.Equals(t, false, handled)
			assert.Equals(t, 1, len(getS3PA(t, c, "s3pa-1").Spec.MountpointS3PodAttachments["mp-1"]))
			assert.Equals(t, 1, countMountpointPods(t, c))
		}
	})

	t.Run("does nothing if the Mountpoint Pod is not found", func(t *testing.T) {
		s3pa := newS3PA("s3pa-1", attachedTo("mp-gone", "workload-1"))
		c, r := newReconcilerWithObjects(t, s3pa)

		handled, _, err := r.handleWorkloadOfFailedMountpointPod(context.Background(), newWorkloadPod("workload-1", corev1.PodPending), newPV(), s3pa, mppod.DefaultPriorityClass, logr.Discard())
		assert.NoError(t, err)
		assert.Equals(t, false, handled)
		assert.Equals(t, 1, len(getS3PA(t, c, "s3pa-1").Spec.MountpointS3PodAttachments["mp-gone"]))
		assert.Equals(t, 0, countMountpointPods(t, c))
	})

	t.Run("backs off when the replacement Mountpoint Pod fails again right away", func(t *testing.T) {
		failed := newMPPod("mp-failed", corev1.PodFailed, "Evicted")
		s3pa := newS3PA("s3pa-1", attachedTo("mp-failed", "workload-1"))
		c, r := newReconcilerWithObjects(t, failed, s3pa)
		now := time.Now()
		r.replacementBackoff.now = func() time.Time { return now }
		workload := newWorkloadPod("workload-1", corev1.PodPending)

		// First replacement is immediate
		handled, _, err := r.handleWorkloadOfFailedMountpointPod(context.Background(), workload, newPV(), s3pa, mppod.DefaultPriorityClass, logr.Discard())
		assert.NoError(t, err)
		assert.Equals(t, true, handled)
		got := getS3PA(t, c, "s3pa-1")
		assert.Equals(t, 1, len(got.Spec.MountpointS3PodAttachments))
		var replacement string
		for name := range got.Spec.MountpointS3PodAttachments {
			replacement = name
		}

		// The replacement fails too
		assert.NoError(t, c.Delete(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: testMPNamespace, Name: replacement}}))
		assert.NoError(t, c.Create(context.Background(), newMPPod(replacement, corev1.PodFailed, "Evicted")))

		// Asking again right away is rate-limited and nothing changes
		handled, requeue, err := r.handleWorkloadOfFailedMountpointPod(context.Background(), workload, newPV(), got, mppod.DefaultPriorityClass, logr.Discard())
		assert.NoError(t, err)
		assert.Equals(t, true, handled)
		assert.Equals(t, true, requeue)
		got = getS3PA(t, c, "s3pa-1")
		if _, ok := got.Spec.MountpointS3PodAttachments[replacement]; !ok {
			t.Fatalf("expected workload to stay attached to %q while backing off, got %v", replacement, got.Spec.MountpointS3PodAttachments)
		}

		// After the backoff it is replaced again
		now = now.Add(minReplacementBackoff + time.Second)
		handled, requeue, err = r.handleWorkloadOfFailedMountpointPod(context.Background(), workload, newPV(), got, mppod.DefaultPriorityClass, logr.Discard())
		assert.NoError(t, err)
		assert.Equals(t, true, handled)
		assert.Equals(t, false, requeue)
		got = getS3PA(t, c, "s3pa-1")
		if _, ok := got.Spec.MountpointS3PodAttachments[replacement]; ok {
			t.Fatalf("expected %q to be replaced after the backoff, got %v", replacement, got.Spec.MountpointS3PodAttachments)
		}
	})
}

func TestReplacementBackoff(t *testing.T) {
	now := time.Now()
	b := &replacementBackoff{now: func() time.Time { return now }}

	assert.Equals(t, true, b.allowed("a"))
	b.record("a")
	assert.Equals(t, false, b.allowed("a"))
	assert.Equals(t, true, b.allowed("b"))

	now = now.Add(minReplacementBackoff)
	assert.Equals(t, true, b.allowed("a"))
	b.record("a")

	// Delay doubles
	now = now.Add(minReplacementBackoff)
	assert.Equals(t, false, b.allowed("a"))
	now = now.Add(minReplacementBackoff)
	assert.Equals(t, true, b.allowed("a"))

	// Many attempts never exceed the cap, and never overflow
	for range 100 {
		b.record("a")
	}
	now = now.Add(maxReplacementBackoff)
	assert.Equals(t, true, b.allowed("a"))

	// Starts from scratch after a quiet period
	b.record("a")
	now = now.Add(replacementBackoffResetAfter + time.Second)
	assert.Equals(t, true, b.allowed("a"))
	b.record("c")
	assert.Equals(t, 1, len(b.entries)) // "a" and "b"... were forgotten
}

func TestReconcileFailedMountpointPod(t *testing.T) {
	t.Run("deletes the failed Pod when nothing references it", func(t *testing.T) {
		failed := newMPPod("mp-failed", corev1.PodFailed, "Evicted")
		other := newS3PA("s3pa-other", map[string][]crdv2.WorkloadAttachment{"mp-other": {{WorkloadPodUID: "w", AttachmentTime: metav1.Now()}}})
		c, r := newReconcilerWithObjects(t, failed, other)

		_, err := r.reconcileMountpointPod(context.Background(), failed)
		assert.NoError(t, err)

		err = c.Get(context.Background(), client.ObjectKeyFromObject(failed), &corev1.Pod{})
		if !apierrors.IsNotFound(err) {
			t.Errorf("expected failed Mountpoint Pod to be deleted, got %v", err)
		}
	})

	t.Run("deletes a failed Pod that was never scheduled to a node when nothing references it", func(t *testing.T) {
		failed := newMPPod("mp-failed", corev1.PodFailed, "")
		failed.Spec.NodeName = ""
		c, r := newReconcilerWithObjects(t, failed)

		_, err := r.reconcileMountpointPod(context.Background(), failed)
		assert.NoError(t, err)
		assert.Equals(t, 0, countMountpointPods(t, c))
	})

	t.Run("keeps the failed Pod while it is referenced, even if it has no node", func(t *testing.T) {
		failed := newMPPod("mp-failed", corev1.PodFailed, "Evicted")
		failed.Spec.NodeName = ""
		s3pa := newS3PA("s3pa-1", map[string][]crdv2.WorkloadAttachment{"mp-failed": {{WorkloadPodUID: "w", AttachmentTime: metav1.Now()}}})
		c, r := newReconcilerWithObjects(t, failed, s3pa)

		res, err := r.reconcileMountpointPod(context.Background(), failed)
		assert.NoError(t, err)
		assert.Equals(t, reconcile.Result{}, res)
		assert.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(failed), &corev1.Pod{}))
	})

	t.Run("does not delete running or pending Pods", func(t *testing.T) {
		for _, phase := range []corev1.PodPhase{corev1.PodPending, corev1.PodRunning} {
			mpPod := newMPPod("mp-1", phase, "")
			c, r := newReconcilerWithObjects(t, mpPod)

			_, err := r.reconcileMountpointPod(context.Background(), mpPod)
			assert.NoError(t, err)
			assert.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(mpPod), &corev1.Pod{}))
		}
	})
}

func TestAddNeedsUnmountAnnotation(t *testing.T) {
	t.Run("annotates a healthy Mountpoint Pod", func(t *testing.T) {
		mpPod := newMPPod("mp-1", corev1.PodRunning, "")
		c, r := newReconcilerWithObjects(t, mpPod)

		assert.NoError(t, r.addNeedsUnmountAnnotation(context.Background(), "mp-1", logr.Discard()))

		got := &corev1.Pod{}
		assert.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(mpPod), got))
		assert.Equals(t, "true", got.Annotations[mppod.AnnotationNeedsUnmount])
	})

	t.Run("deletes a failed Mountpoint Pod instead, as there is nothing left to unmount cleanly", func(t *testing.T) {
		mpPod := newMPPod("mp-1", corev1.PodFailed, "Evicted")
		c, r := newReconcilerWithObjects(t, mpPod)

		assert.NoError(t, r.addNeedsUnmountAnnotation(context.Background(), "mp-1", logr.Discard()))

		err := c.Get(context.Background(), client.ObjectKeyFromObject(mpPod), &corev1.Pod{})
		if !apierrors.IsNotFound(err) {
			t.Errorf("expected failed Mountpoint Pod to be deleted, got %v", err)
		}
	})
}

func TestWorkloadPodsOfFailedMountpointPod(t *testing.T) {
	workload1 := newWorkloadPod("workload-1", corev1.PodPending)
	workload2 := newWorkloadPod("workload-2", corev1.PodPending)
	unrelated := newWorkloadPod("workload-3", corev1.PodPending)
	s3pa := newS3PA("s3pa-1", map[string][]crdv2.WorkloadAttachment{
		"mp-failed": {{WorkloadPodUID: "workload-1"}, {WorkloadPodUID: "workload-2"}},
		"mp-other":  {{WorkloadPodUID: "workload-3"}},
	})

	t.Run("returns the workloads attached to a failed Mountpoint Pod", func(t *testing.T) {
		failed := newMPPod("mp-failed", corev1.PodFailed, "Evicted")
		_, r := newReconcilerWithObjects(t, failed, s3pa, workload1, workload2, unrelated)

		got := r.workloadPodsOfFailedMountpointPod(context.Background(), failed)
		assert.Equals(t, 2, len(got))
		names := map[string]bool{}
		for _, req := range got {
			names[req.Name] = true
		}
		assert.Equals(t, true, names[workload1.Name])
		assert.Equals(t, true, names[workload2.Name])
	})

	t.Run("returns nothing for Mountpoint Pods that are not failed", func(t *testing.T) {
		running := newMPPod("mp-failed", corev1.PodRunning, "")
		_, r := newReconcilerWithObjects(t, running, s3pa, workload1)
		assert.Equals(t, 0, len(r.workloadPodsOfFailedMountpointPod(context.Background(), running)))
	})

	t.Run("returns nothing for failed Pods outside of the Mountpoint namespace", func(t *testing.T) {
		failedWorkload := newWorkloadPod("workload-1", corev1.PodFailed)
		_, r := newReconcilerWithObjects(t, s3pa, failedWorkload)
		assert.Equals(t, 0, len(r.workloadPodsOfFailedMountpointPod(context.Background(), failedWorkload)))
	})
}

func TestShouldAssignNewWorkloadToMountpointPodRejectsFailed(t *testing.T) {
	_, r := newReconcilerWithObjects(t)
	assert.Equals(t, false, r.shouldAssignNewWorkloadToMountpointPod(newMPPod("mp-1", corev1.PodFailed, "Evicted"), logr.Discard()))
	assert.Equals(t, true, r.shouldAssignNewWorkloadToMountpointPod(newMPPod("mp-1", corev1.PodRunning, ""), logr.Discard()))
}

// helpers

func newMPPod(name string, phase corev1.PodPhase, reason string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testMPNamespace, UID: types.UID("uid-" + name)},
		Spec:       corev1.PodSpec{NodeName: testNodeName},
		Status:     corev1.PodStatus{Phase: phase, Reason: reason},
	}
}

func newWorkloadPod(uid string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-" + uid, Namespace: "default", UID: types.UID(uid)},
		Spec:       corev1.PodSpec{NodeName: testNodeName},
		Status:     corev1.PodStatus{Phase: phase},
	}
}

func newPV() *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: testPVName},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: mountpointCSIDriverName, VolumeHandle: testVolumeID},
			},
		},
	}
}

func getS3PA(t *testing.T, c client.Client, name string) *crdv2.MountpointS3PodAttachment {
	t.Helper()
	s3pa := &crdv2.MountpointS3PodAttachment{}
	assert.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: name}, s3pa))
	return s3pa
}

func countMountpointPods(t *testing.T, c client.Client) int {
	t.Helper()
	podList := &corev1.PodList{}
	assert.NoError(t, c.List(context.Background(), podList, client.InNamespace(testMPNamespace)))
	return len(podList.Items)
}
