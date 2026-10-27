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
	first, second := runningMounterPod("mounter-first", 1), runningMounterPod("mounter-second", 2)
	dm := overlapDM(t, first, second)
	dm.mounter.Store(&mounterPod{commDir: dm.commDirOf(*first)})
	logs := testutil.CaptureKlog(t)

	dm.checkForMultipleMounterPods()

	assert.Contains(t, logs.String(), "multiple running mounter pods found on node test-node (expected exactly 1, got 2: mounter-first, mounter-second)")
	assert.Equals(t, dm.commDirOf(*first), dm.mounter.Load().commDir)
}

func TestDiscoverMounterPicksMounterInUse(t *testing.T) {
	// Listed in name order, so the oldest pod also comes first and only the in-use rule can pick inUse.
	older, inUse := runningMounterPod("mounter-a", 1), runningMounterPod("mounter-b", 2)
	dm := overlapDM(t, older, inUse)
	entry, _ := dm.mountMap.GetOrCreate("pv-1")
	entry.CommDir = dm.commDirOf(*inUse)
	logs := testutil.CaptureKlog(t)

	assert.NoError(t, dm.DiscoverMounter(context.Background()))

	assert.Equals(t, dm.commDirOf(*inUse), dm.mounter.Load().commDir)
	assert.Contains(t, logs.String(), "Using mounter-b")
}

func TestDiscoverMounterFailsWithNoMounterInUse(t *testing.T) {
	dm := overlapDM(t, runningMounterPod("mounter-a", 1), runningMounterPod("mounter-b", 2))
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	assert.ErrorIs(t, dm.DiscoverMounter(ctx), ErrMultipleMounterPods)
	assert.Equals(t, (*mounterPod)(nil), dm.mounter.Load())
}

func TestRediscoveryPicksNoMounterWhenItsOwnIsGone(t *testing.T) {
	dm := overlapDM(t, runningMounterPod("mounter-a", 1), runningMounterPod("mounter-b", 2))

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

// runningMounterPod is a running mounter pod on test-node, created createdMinute minutes into the hour.
func runningMounterPod(name string, createdMinute int) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: mounterNamespace, UID: types.UID(name),
			Labels:            map[string]string{"app": "s3-csi-daemonset-mounter"},
			CreationTimestamp: metav1.NewTime(time.Date(2026, 1, 1, 0, createdMinute, 0, 0, time.UTC)),
		},
		Spec:   corev1.PodSpec{NodeName: "test-node"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}
