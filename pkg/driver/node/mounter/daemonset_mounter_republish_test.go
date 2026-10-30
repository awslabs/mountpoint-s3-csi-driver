package mounter_test

import (
	"errors"
	"slices"
	"testing"

	mountutils "k8s.io/mount-utils"

	mpmounter "github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mounter"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/mounter"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

// With `requiresRepublish: true`, the kubelet calls NodePublishVolume on a running pod's target
// periodically. A healthy mount must keep serving across those calls: never remounted, never unmounted.
func TestDaemonsetMounter_RepublishKeepsAHealthyMount(t *testing.T) {
	testCtx := setupDM(t)
	target := testCtx.targetPath(testCtx.podUID)
	source := mounter.SourceMountPath(testCtx.kubeletPath, testCtx.volumeID)
	testCtx.mountAndServe(testCtx.podUID, nil)

	// From here a FUSE mount would mean the healthy mount was replaced.
	newFUSEMounts := 0
	testCtx.mountSyscall = func(string, mpmounter.MountOptions) (int, error) {
		newFUSEMounts++
		return 0, errors.New("a republish of a healthy target must not mount again")
	}

	const republishes = 5
	for range republishes {
		err := testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, target, testCtx.driverProvideCtx(testCtx.podUID),
			nil, mountpoint.ParseArgs(nil), "", nil)
		assert.NoError(t, err)
	}

	assert.Equals(t, 0, newFUSEMounts)
	for _, action := range testCtx.mount.GetLog() {
		if action.Action == mountutils.FakeActionUnmount {
			t.Errorf("a republish of a healthy target unmounted %s", action.Target)
		}
	}
	mounted := func(path string) bool {
		return slices.ContainsFunc(testCtx.mount.MountPoints, func(mp mountutils.MountPoint) bool { return mp.Path == path })
	}
	assert.Equals(t, true, mounted(source))
	assert.Equals(t, true, mounted(target))

	// Still one consumer: a republish is not a new pod, so the one unpublish tears the source down.
	// Had the republishes counted as consumers, the source would outlive its only pod.
	err := testCtx.dm.Unmount(testCtx.ctx, target, credentialprovider.CleanupContext{
		VolumeID: testCtx.volumeID,
		PodID:    testCtx.podUID,
	})
	assert.NoError(t, err)
	testCtx.assertUnmounted(target)
	testCtx.assertUnmounted(source)
}
