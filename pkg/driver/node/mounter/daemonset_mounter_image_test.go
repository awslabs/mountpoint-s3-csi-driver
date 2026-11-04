package mounter_test

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

const (
	imageA = "registry.example/aws-mountpoint-s3-csi-driver:v3.0.0"
	imageB = "registry.example/aws-mountpoint-s3-csi-driver:v3.1.0"
)

// setMounterPodImage sets the mounter pod's image, then re-runs discovery, which reads it.
func (testCtx *dmTestCtx) setMounterPodImage(image string) {
	testCtx.t.Helper()
	pods := testCtx.client.CoreV1().Pods("kube-system")
	list, err := pods.List(testCtx.ctx, metav1.ListOptions{})
	assert.NoError(testCtx.t, err)
	pod := &list.Items[0]
	pod.Spec.Containers = []corev1.Container{{Name: "mounter", Image: image}}
	_, err = pods.Update(testCtx.ctx, pod, metav1.UpdateOptions{})
	assert.NoError(testCtx.t, err)
	assert.NoError(testCtx.t, testCtx.dm.DiscoverMounter(testCtx.ctx))
}

func TestDaemonsetMounterImageMismatch(t *testing.T) {
	t.Run("Mounts and warns how to recycle the node when the images differ", func(t *testing.T) {
		t.Setenv("CSI_NODE_IMAGE", imageB)
		testCtx := setupDM(t)
		testCtx.setMounterPodImage(imageA)
		logs := testutil.CaptureKlog(t)

		testCtx.mountAndServe(testCtx.podUID, nil) // fails the test if the mount is refused
		// The kubelet's republish, which refreshes credentials, is served too.
		assert.NoError(t, testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath(testCtx.podUID),
			testCtx.driverProvideCtx(testCtx.podUID), nil, mountpoint.ParseArgs(nil), "", nil))

		assert.Contains(t, logs.String(), "image mismatch on node "+testCtx.nodeName)
		assert.Contains(t, logs.String(), "kubectl cordon "+testCtx.nodeName)
	})

	t.Run("Mounts without a warning when the images match", func(t *testing.T) {
		t.Setenv("CSI_NODE_IMAGE", imageA)
		testCtx := setupDM(t)
		testCtx.setMounterPodImage(imageA)
		logs := testutil.CaptureKlog(t)

		testCtx.mountAndServe(testCtx.podUID, nil)

		assert.Equals(t, false, strings.Contains(logs.String(), "image mismatch"))
	})
}
