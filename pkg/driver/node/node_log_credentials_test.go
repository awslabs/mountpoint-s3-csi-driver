package node_test

import (
	"context"
	"fmt"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/golang/mock/gomock"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/volumecontext"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

// Fake credentials shaped like real ones, so both the verbatim and the shape checks have something to find.
// The JWT is three base64url parts: {"alg":"RS256"}, {"sub":"test"} and "signature".
const (
	fakeServiceAccountJWT = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ0ZXN0In0.c2lnbmF0dXJl"
	fakeAccessKeyID       = "AKIAIOSFODNN7EXAMPLE"
	fakeSecretAccessKey   = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
)

// The kubelet hands csi-node the workload's service account tokens on every publish, in the volume context
// and (Kubernetes v1.35+) in the request's secrets. At the most verbose level csi-node logs every publish and
// unpublish, so neither may carry a token or key into the logs.
func TestNodePublishAndUnpublishLogNoCredentials(t *testing.T) {
	logs := testutil.CaptureKlog(t, 10)

	nodeTestEnv := initNodeServerTestEnv(t)
	nodeTestEnv.mockMounter.EXPECT().Mount(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
	nodeTestEnv.mockMounter.EXPECT().IsMountPoint(gomock.Any()).Return(true, nil)
	nodeTestEnv.mockMounter.EXPECT().Unmount(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)

	tokens := fmt.Sprintf(`{"sts.amazonaws.com":{"token":%q,"expirationTimestamp":"2026-10-30T01:00:00Z"}}`, fakeServiceAccountJWT)
	const targetPath = "/var/lib/kubelet/pods/d8c872d7-a29c-4362-81b1-9912370d0813/volumes/kubernetes.io~csi/s3-pv/mount"
	ctx := context.Background()

	_, err := nodeTestEnv.server.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId: "s3-pv",
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
		},
		TargetPath: targetPath,
		VolumeContext: map[string]string{
			volumecontext.BucketName:              "test-bucket",
			volumecontext.AuthenticationSource:    "pod",
			volumecontext.CSIServiceAccountTokens: tokens,
		},
		Secrets: map[string]string{
			volumecontext.CSIServiceAccountTokens: tokens,
			// Not something the kubelet sends today, but node-publish secrets are free-form: none may be logged.
			"accessKeyId":     fakeAccessKeyID,
			"secretAccessKey": fakeSecretAccessKey,
		},
	})
	assert.NoError(t, err)

	_, err = nodeTestEnv.server.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "s3-pv",
		TargetPath: targetPath,
	})
	assert.NoError(t, err)

	testutil.AssertNoCredentialsLogged(t, logs(), fakeServiceAccountJWT, fakeAccessKeyID, fakeSecretAccessKey)
}
