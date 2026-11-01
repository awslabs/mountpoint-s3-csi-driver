package mounter_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/golang/mock/gomock"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/cluster"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	mock_credentialprovider "github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider/mocks"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/mounter"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
	mpmounter "github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mounter"
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

	pods := testCtx.client.CoreV1().Pods("kube-system")
	list, err := pods.List(testCtx.ctx, metav1.ListOptions{})
	assert.NoError(testCtx.t, err)
	assert.Equals(testCtx.t, 1, len(list.Items))

	pod := &list.Items[0]
	pod.Spec.Containers = []corev1.Container{{Name: "mounter", Image: image}}
	_, err = pods.Update(testCtx.ctx, pod, metav1.UpdateOptions{})
	assert.NoError(testCtx.t, err)
	assert.NoError(testCtx.t, testCtx.dm.DiscoverMounter(testCtx.ctx))
}

// useStrictCredProvider replaces the DaemonsetMounter with one whose credential provider fails the
// test on any call, so a test can prove nothing was written.
func (testCtx *dmTestCtx) useStrictCredProvider() {
	testCtx.t.Helper()

	strict := mock_credentialprovider.NewMockProviderInterface(gomock.NewController(testCtx.t))
	testCtx.dm = mounter.NewDaemonsetMounter(testCtx.client, testCtx.nodeName,
		mpmounter.NewWithMount(testCtx.mount), strict,
		func(string, mpmounter.MountOptions) (int, error) {
			testCtx.t.Fatal("FUSE mount attempted despite an image mismatch")
			return 0, nil
		}, func(string, string) error {
			testCtx.t.Fatal("bind mount attempted despite an image mismatch")
			return nil
		}, nil, "", cluster.DefaultKubernetes)
	testCtx.installOwnershipRecorders()
}

func assertImageMismatch(t *testing.T, err error) {
	t.Helper()
	assert.Equals(t, codes.FailedPrecondition, status.Code(err))
	if !strings.Contains(err.Error(), imageA) || !strings.Contains(err.Error(), imageB) {
		t.Fatalf("error should name both images, got: %v", err)
	}
	// The fix, in order, for this node.
	for _, step := range []string{
		"upgrade or rollback is incomplete",
		"kubectl cordon test-node",
		"kubectl drain test-node --ignore-daemonsets",
		"kubectl delete pod",
		"--field-selector spec.nodeName=test-node",
		"kubectl uncordon test-node",
	} {
		if !strings.Contains(err.Error(), step) {
			t.Fatalf("error should contain %q, got: %v", step, err)
		}
	}
}

func TestDaemonsetMounterImageCheck(t *testing.T) {
	t.Run("Mounts when the CSI node and mounter images match", func(t *testing.T) {
		t.Setenv("CSI_NODE_IMAGE", imageA)
		testCtx := setupDM(t)
		testCtx.setMounterPodImage(imageA)

		testCtx.mountAndServe(testCtx.podUID, nil)
	})

	t.Run("Refuses a new mount on mismatch before writing anything", func(t *testing.T) {
		t.Setenv("CSI_NODE_IMAGE", imageB)
		testCtx := setupDM(t)
		testCtx.useStrictCredProvider()
		testCtx.setMounterPodImage(imageA)

		err := testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath(testCtx.podUID),
			testCtx.driverProvideCtx(testCtx.podUID), nil, mountpoint.ParseArgs(nil), "", nil)
		assertImageMismatch(t, err)

		// No meta file, so nothing was recorded for this mount.
		metaDir := filepath.Join(testCtx.kubeletPath, "plugins", "s3.csi.aws.com", "meta")
		entries, _ := os.ReadDir(metaDir)
		assert.Equals(t, 0, len(entries))
	})

	t.Run("Refuses credential refresh of a mounted target on mismatch", func(t *testing.T) {
		t.Setenv("CSI_NODE_IMAGE", imageB)
		testCtx := setupDM(t)
		testCtx.useStrictCredProvider()
		testCtx.setMounterPodImage(imageA)

		// A healthy, already-mounted target makes this a republish (credential refresh).
		target := testCtx.targetPath(testCtx.podUID)
		assert.NoError(t, os.MkdirAll(target, 0755))
		assert.NoError(t, testCtx.mount.Mount("mountpoint-s3", target, "fuse", nil))

		err := testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, target,
			testCtx.driverProvideCtx(testCtx.podUID), nil, mountpoint.ParseArgs(nil), "", nil)
		assertImageMismatch(t, err)
	})

	t.Run("Refuses when the mounter pod has no mounter container", func(t *testing.T) {
		t.Setenv("CSI_NODE_IMAGE", imageA)
		testCtx := setupDM(t)
		testCtx.useStrictCredProvider()
		assert.NoError(t, testCtx.dm.DiscoverMounter(testCtx.ctx))

		err := testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath(testCtx.podUID),
			credentialprovider.ProvideContext{WorkloadPodID: testCtx.podUID, VolumeID: testCtx.volumeID},
			nil, mountpoint.ParseArgs(nil), "", nil)
		assert.Equals(t, codes.FailedPrecondition, status.Code(err))
	})

	t.Run("Check is off when CSI_NODE_IMAGE is unset", func(t *testing.T) {
		t.Setenv("CSI_NODE_IMAGE", "")
		testCtx := setupDM(t)
		testCtx.setMounterPodImage(imageA)

		testCtx.mountAndServe(testCtx.podUID, nil)
	})
}
