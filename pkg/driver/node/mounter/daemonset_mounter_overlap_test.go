package mounter

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

// Tests for a node with two running mounter pods, which happens when two mounter entries match it.
// They capture klog's process-global output, so none may call t.Parallel.

func TestCheckForMultipleMounterPodsLogsAndKeepsMounter(t *testing.T) {
	first, second := runningMounterPod("mounter-first"), runningMounterPod("mounter-second")
	dm := overlapDM(t, first, second)
	dm.mounter.Store(&mounterPod{commDir: dm.commDirOf(*first)})
	lockMount(t, dm)
	logs := testutil.CaptureKlog(t)

	returnsPromptly(t, dm.checkForMultipleMounterPods)

	assert.Contains(t, logs.String(), "multiple running mounter pods found on node test-node (expected exactly 1, got 2: mounter-first, mounter-second)")
	assert.Equals(t, dm.commDirOf(*first), dm.mounter.Load().commDir)
}

func TestDiscoverMounterPicksMounterInUse(t *testing.T) {
	// Listed in name order, so only the in-use rule can pick inUse over the first pod.
	first, inUse := runningMounterPod("mounter-a"), runningMounterPod("mounter-b")
	dm := overlapDM(t, first, inUse)
	entry, _ := dm.mountMap.GetOrCreate("pv-1")
	entry.CommDir = dm.commDirOf(*inUse)
	logs := testutil.CaptureKlog(t)

	assert.NoError(t, dm.DiscoverMounter(context.Background()))

	assert.Equals(t, dm.commDirOf(*inUse), dm.mounter.Load().commDir)
	assert.Equals(t, dm.commDirOf(*inUse), dm.lastCommDir)
	assert.Contains(t, logs.String(), "Using mounter-b")
}

func TestDiscoverMounterFailsWithNoMounterInUse(t *testing.T) {
	dm := overlapDM(t, runningMounterPod("mounter-a"), runningMounterPod("mounter-b"))
	entry, _ := dm.mountMap.GetOrCreate("pv-1")
	entry.CommDir = dm.commDirOf(*runningMounterPod("mounter-gone"))
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	assert.ErrorIs(t, dm.DiscoverMounter(ctx), ErrMultipleMounterPods)
	assert.Equals(t, (*mounterPod)(nil), dm.mounter.Load())
}

func TestDiscoverMounterSkipsDeletingPod(t *testing.T) {
	deleting, inUse := runningMounterPod("mounter-a"), runningMounterPod("mounter-b")
	deleting.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	dm := overlapDM(t, deleting, inUse, runningMounterPod("mounter-c"))
	for volumeID, pod := range map[string]*corev1.Pod{"pv-1": deleting, "pv-2": inUse} {
		entry, _ := dm.mountMap.GetOrCreate(volumeID)
		entry.CommDir = dm.commDirOf(*pod)
	}

	assert.NoError(t, dm.DiscoverMounter(context.Background()))

	assert.Equals(t, dm.commDirOf(*inUse), dm.mounter.Load().commDir)
}

func TestRediscoveryKeepsItsMounterWithoutMountLocks(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		hasMounter bool
	}{
		{"after its socket is gone", true},
		{"after a failed mount request cleared it", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// Listed in name order, so own is picked only by matching the pod it had.
			first, own := runningMounterPod("mounter-a"), runningMounterPod("mounter-b")
			dm := overlapDM(t, first, own)
			dm.lastCommDir = dm.commDirOf(*own)
			if testCase.hasMounter {
				dm.mounter.Store(&mounterPod{commDir: dm.lastCommDir})
			}
			lockMount(t, dm)

			var healthy bool
			returnsPromptly(t, func() { healthy = dm.checkCommDir() })

			assert.Equals(t, true, healthy)
			assert.Equals(t, dm.commDirOf(*own), dm.mounter.Load().commDir)
		})
	}
}

func TestRediscoveryPicksNoMounterWhenItsOwnIsGone(t *testing.T) {
	dm := overlapDM(t, runningMounterPod("mounter-a"), runningMounterPod("mounter-b"))
	dm.lastCommDir = dm.commDirOf(*runningMounterPod("mounter-gone"))
	dm.mounter.Store(&mounterPod{commDir: dm.lastCommDir})

	assert.Equals(t, false, dm.checkCommDir())
	assert.Equals(t, (*mounterPod)(nil), dm.mounter.Load())
}

func overlapDM(t *testing.T, pods ...*corev1.Pod) *DaemonsetMounter {
	t.Helper()
	var objects []runtime.Object
	for _, pod := range pods {
		objects = append(objects, pod)
	}
	return &DaemonsetMounter{clientset: fake.NewSimpleClientset(objects...), nodeID: "test-node", kubeletPath: t.TempDir(), mountMap: NewMountMap()}
}

// runningMounterPod is a running mounter pod on test-node.
func runningMounterPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: mounterNamespace, UID: types.UID(name),
			Labels: map[string]string{"app": "s3-csi-daemonset-mounter"},
		},
		Spec:   corev1.PodSpec{NodeName: "test-node"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// lockMount holds a mount's lock for the rest of the test, as a mount in progress does.
func lockMount(t *testing.T, dm *DaemonsetMounter) {
	entry, _ := dm.mountMap.GetOrCreate("pv-locked")
	entry.mu.Lock()
	t.Cleanup(entry.mu.Unlock)
}

// returnsPromptly fails the test if f doesn't return within a second.
func returnsPromptly(t *testing.T, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		f()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("did not return within a second, so it is likely waiting on a mount's lock")
	}
}
