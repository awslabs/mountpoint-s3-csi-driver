package mppod_test

import (
	"path/filepath"
	"testing"

	"github.com/awslabs/aws-s3-csi-driver/pkg/podmounter/mppod"
	"github.com/awslabs/aws-s3-csi-driver/pkg/util/testutil/assert"
)

func TestGeneratingPathsInsideMountpointPod(t *testing.T) {
	assert.Equals(t, "/comm/mount.sock", mppod.PathInsideMountpointPod("mount.sock"))
	assert.Equals(t, "/comm/mount.sock", mppod.PathInsideMountpointPod("/mount.sock"))
	assert.Equals(t, "/comm/sa-token/web-identity.token", mppod.PathInsideMountpointPod("./sa-token/web-identity.token"))
}

func TestGeneratingPathsForMountpointPodOnHost(t *testing.T) {
	podPath := "/var/lib/kubelet/pods/46efe8aa-75d9-4b12-8fdd-0ce0c2cabd99"
	assert.Equals(t, filepath.Join(podPath, "/volumes/kubernetes.io~empty-dir/comm/mount.sock"), mppod.PathOnHost(podPath, "mount.sock"))
	assert.Equals(t, filepath.Join(podPath, "/volumes/kubernetes.io~empty-dir/comm/mount.sock"), mppod.PathOnHost(podPath, "/mount.sock"))
	assert.Equals(t, filepath.Join(podPath, "/volumes/kubernetes.io~empty-dir/comm/sa-token/web-identity.token"), mppod.PathOnHost(podPath, "./sa-token/web-identity.token"))
}
