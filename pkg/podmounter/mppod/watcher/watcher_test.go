package watcher_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/awslabs/aws-s3-csi-driver/pkg/podmounter/mppod/watcher"
	"github.com/awslabs/aws-s3-csi-driver/pkg/util/testutil/assert"
)

func TestGettingAlreadyScheduledAndReadyPod(t *testing.T) {
	client := fake.NewClientset()

	mpPod := createMountpointPod(t, client, testMountpointPodName)
	mpPod.run()

	mpPodWatcher := createAndStartWatcher(t, client)

	pod, err := mpPodWatcher.Wait(context.Background(), mpPod.pod.Name)
	assert.NoError(t, err)
	assert.Equals(t, mpPod.pod, pod)
}

func TestGettingScheduledButNotYetReadyPod(t *testing.T) {
	client := fake.NewClientset()

	mpPod := createMountpointPod(t, client, testMountpointPodName)

	mpPodWatcher := createAndStartWatcher(t, client)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	pod, err := mpPodWatcher.Wait(ctx, testMountpointPodName)
	assert.Equals(t, watcher.ErrPodNotReady, err)
	if pod != nil {
		t.Fatalf("Pod should be nil if `watcher.ErrPodNotReady` error returned, but got %#v", pod)
	}

	mpPod.run()

	pod, err = mpPodWatcher.Wait(context.Background(), testMountpointPodName)
	assert.NoError(t, err)
	assert.Equals(t, mpPod.pod, pod)
}

func TestGettingNotYetScheduledPod(t *testing.T) {
	client := fake.NewClientset()

	mpPodWatcher := createAndStartWatcher(t, client)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	pod, err := mpPodWatcher.Wait(ctx, testMountpointPodName)
	assert.Equals(t, watcher.ErrPodNotFound, err)
	if pod != nil {
		t.Fatalf("Pod should be nil if `watcher.ErrPodNotFound` error returned, but got %#v", pod)
	}

	mpPod := createMountpointPod(t, client, testMountpointPodName)

	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	pod, err = mpPodWatcher.Wait(ctx, testMountpointPodName)
	assert.Equals(t, watcher.ErrPodNotReady, err)
	if pod != nil {
		t.Fatalf("Pod should be nil if `watcher.ErrPodNotReady` error returned, but got %#v", pod)
	}

	mpPod.run()

	pod, err = mpPodWatcher.Wait(context.Background(), testMountpointPodName)
	assert.NoError(t, err)
	assert.Equals(t, mpPod.pod, pod)
}

func TestGettingPodsConcurrently(t *testing.T) {
	client := fake.NewClientset()

	mpPodWatcher := createAndStartWatcher(t, client)

	foundPods := make(chan *corev1.Pod)
	for range 5 {
		go func() {
			pod, err := mpPodWatcher.Wait(context.Background(), testMountpointPodName)
			assert.NoError(t, err)
			foundPods <- pod
		}()
	}

	mpPod := createMountpointPod(t, client, testMountpointPodName)
	mpPod.run()

	for range 5 {
		foundPod := <-foundPods
		assert.Equals(t, mpPod.pod, foundPod)
	}
}

func createAndStartWatcher(t *testing.T, client kubernetes.Interface) *watcher.Watcher {
	mpPodWatcher := watcher.New(client, testMountpointPodNamespace, "test-node", 10*time.Second)

	stopCh := make(chan struct{})
	t.Cleanup(func() {
		close(stopCh)
	})

	err := mpPodWatcher.Start(stopCh)
	assert.NoError(t, err)

	return mpPodWatcher
}

const testMountpointPodName = "mp-pod"
const testMountpointPodNamespace = "mount-s3-test"

type mountpointPod struct {
	t      *testing.T
	client kubernetes.Interface
	pod    *corev1.Pod
}

func createMountpointPod(t *testing.T, client kubernetes.Interface, name string) *mountpointPod {
	t.Helper()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:  types.UID(uuid.New().String()),
			Name: name,
		},
	}
	pod, err := client.CoreV1().Pods(testMountpointPodNamespace).Create(context.Background(), pod, metav1.CreateOptions{})
	assert.NoError(t, err)

	return &mountpointPod{t, client, pod}
}

func (mp *mountpointPod) run() {
	mp.t.Helper()
	mp.pod.Status.Phase = corev1.PodRunning
	var err error
	mp.pod, err = mp.client.CoreV1().Pods(testMountpointPodNamespace).UpdateStatus(context.Background(), mp.pod, metav1.UpdateOptions{})
	assert.NoError(mp.t, err)
}
