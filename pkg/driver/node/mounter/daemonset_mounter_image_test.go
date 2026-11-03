package mounter_test

import (
	"context"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/envprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

const (
	imageA = "registry.example/aws-mountpoint-s3-csi-driver:v3.0.0"
	imageB = "registry.example/aws-mountpoint-s3-csi-driver:v3.1.0"
)

// setMounterPodImage sets the mounter pod's `mounter` container image, then re-runs discovery, the
// only thing that reads it.
func (testCtx *dmTestCtx) setMounterPodImage(image string) {
	testCtx.t.Helper()
	testCtx.setMounterPodContainers([]corev1.Container{{Name: "mounter", Image: image}})
}

// setMounterPodContainers replaces the mounter pod's containers, then re-runs discovery.
func (testCtx *dmTestCtx) setMounterPodContainers(containers []corev1.Container) {
	testCtx.t.Helper()
	pods := testCtx.client.CoreV1().Pods("kube-system")
	list, err := pods.List(testCtx.ctx, metav1.ListOptions{})
	assert.NoError(testCtx.t, err)
	assert.Equals(testCtx.t, 1, len(list.Items))

	pod := &list.Items[0]
	pod.Spec.Containers = containers
	_, err = pods.Update(testCtx.ctx, pod, metav1.UpdateOptions{})
	assert.NoError(testCtx.t, err)
	assert.NoError(testCtx.t, testCtx.dm.DiscoverMounter(testCtx.ctx))
}

// countingProvider counts the credentials it provides, so a test can tell a refresh happened.
type countingProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *countingProvider) Provide(context.Context, credentialprovider.ProvideContext) (envprovider.Environment, credentialprovider.AuthenticationSource, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return envprovider.Environment{}, credentialprovider.AuthenticationSourceDriver, nil
}

func (p *countingProvider) Cleanup(credentialprovider.CleanupContext) error { return nil }

func (p *countingProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// After an upgrade the CSI node rolls automatically while the mounter waits for the node to be recycled, so the
// two run different images on every node until then. That is supported: the CSI node only warns.
func TestDaemonsetMounterImageCheck(t *testing.T) {
	t.Run("Mounts when the CSI node and mounter images match", func(t *testing.T) {
		t.Setenv("CSI_NODE_IMAGE", imageA)
		testCtx := setupDM(t)
		testCtx.setMounterPodImage(imageA)
		testCtx.mountAndServe(testCtx.podUID, nil)
		assert.Equals(t, true, testCtx.isMountedAt(testCtx.targetPath(testCtx.podUID)))
	})

	t.Run("Mounts when the CSI node is newer than the mounter", func(t *testing.T) {
		t.Setenv("CSI_NODE_IMAGE", imageB)
		testCtx := setupDM(t)
		testCtx.setMounterPodImage(imageA)
		testCtx.mountAndServe(testCtx.podUID, nil)
		assert.Equals(t, true, testCtx.isMountedAt(testCtx.targetPath(testCtx.podUID)))
	})

	t.Run("Refreshes credentials of a running mount when the images differ", func(t *testing.T) {
		t.Setenv("CSI_NODE_IMAGE", imageB)
		testCtx := setupDM(t)
		provider := &countingProvider{}
		testCtx.useCredProvider(provider)
		testCtx.setMounterPodImage(imageA)
		testCtx.mountAndServe(testCtx.podUID, nil)
		assert.Equals(t, 1, provider.count())

		// The kubelet's republish of the mounted target refreshes its credentials.
		err := testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath(testCtx.podUID),
			testCtx.driverProvideCtx(testCtx.podUID), nil, mountpoint.ParseArgs(nil), "", nil)
		assert.NoError(t, err)
		assert.Equals(t, 2, provider.count())
	})

	t.Run("Mounts when the mounter is newer than the CSI node", func(t *testing.T) {
		t.Setenv("CSI_NODE_IMAGE", imageA)
		testCtx := setupDM(t)
		testCtx.setMounterPodImage(imageB)
		testCtx.mountAndServe(testCtx.podUID, nil)
		assert.Equals(t, true, testCtx.isMountedAt(testCtx.targetPath(testCtx.podUID)))
	})

	t.Run("Mounts when the mounter pod has no mounter container", func(t *testing.T) {
		t.Setenv("CSI_NODE_IMAGE", imageA)
		testCtx := setupDM(t)
		testCtx.setMounterPodContainers([]corev1.Container{{Name: "something-else", Image: imageA}})
		testCtx.mountAndServe(testCtx.podUID, nil)
		assert.Equals(t, true, testCtx.isMountedAt(testCtx.targetPath(testCtx.podUID)))
	})

	t.Run("Mounts when CSI_NODE_IMAGE is unset", func(t *testing.T) {
		t.Setenv("CSI_NODE_IMAGE", "")
		testCtx := setupDM(t)
		testCtx.setMounterPodImage(imageA)
		testCtx.mountAndServe(testCtx.podUID, nil)
		assert.Equals(t, true, testCtx.isMountedAt(testCtx.targetPath(testCtx.podUID)))
	})
}

// isMountedAt reports whether the fake mounter has a mount at path.
func (testCtx *dmTestCtx) isMountedAt(path string) bool {
	for _, mp := range testCtx.mount.MountPoints {
		if mp.Path == path {
			return true
		}
	}
	return false
}
