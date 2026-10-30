package node_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/golang/mock/gomock"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/envprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util"
)

// NodePublishVolume parses a request built from the kubelet and the PV, so a malformed one must be refused
// with InvalidArgument and never panic. A request it accepts must reach the mounter with no `-o` or
// `--ca-bundle`, a target under the kubelet path, and only user environment variables on the allowlist.
//
// `go test` runs the seeds below; `go test -fuzz=FuzzNodePublishVolume ./pkg/driver/node/` explores further.
func FuzzNodePublishVolume(f *testing.F) {
	const target = "/var/lib/kubelet/pods/d8c872d7-a29c-4362-81b1-9912370d0813/volumes/kubernetes.io~csi/s3-pv/mount"
	type seed struct {
		volumeID, bucket   string
		hasBucket          bool
		targetPath         string
		mountFlags         string // one flag per line
		ctxKey, ctxValue   string
		readOnly, hasMount bool
	}
	for _, s := range []seed{
		{"s3-pv", "bucket", true, target, "", "", "", false, true},
		{"s3-pv", "bucket", true, target, "--allow-other\n--uid=1000\n--region us-west-2", "authenticationSource", "pod", true, true},
		{"s3-pv", "bucket", true, target, "-o allow_other", "", "", false, true},
		{"s3-pv", "bucket", true, target, " --ca-bundle=/etc/ssl/evil.pem ", "", "", false, true},
		{"s3-pv", "bucket", true, target, "--cache /tmp\n--max-cache-size=abc", "cache", "emptyDir", false, true},
		{"s3-pv", "bucket", true, target, "--max-cache-size=99999999999999999999", "cacheEmptyDirSizeLimit", "1Gi", false, true},
		{"s3-pv", "bucket", true, target, "", "mountpointEnv.AWS_ENDPOINT_URL", "http://attacker", false, true},
		{"s3-pv", "bucket", true, target, "", "mountpointEnv.HTTPS_PROXY", "http://proxy:3128", false, true},
		{"s3-pv", "bucket", true, target, "", "csi.storage.k8s.io/serviceAccount.tokens", "{not json", false, true},
		{"s3-pv", "bucket", true, "/etc/passwd", "", "", "", false, true},
		{"s3-pv", "bucket", true, "/var/lib/kubelet/../../etc", "", "", "", false, true},
		{"s3-pv", "", false, target, "", "", "", false, true},
		{"", "bucket", true, target, "", "", "", false, true},
		{"s3-pv", "bucket", true, "", "", "", "", false, true},
		{"s3-pv", "bucket", true, target, "", "", "", false, false},
		{"s3-pv\x00", "bucket\n", true, target + "\x00", "\x00", "\x00", "\x00", true, true},
	} {
		f.Add(s.volumeID, s.bucket, s.hasBucket, s.targetPath, s.mountFlags, s.ctxKey, s.ctxValue, s.readOnly, s.hasMount)
	}

	f.Fuzz(func(t *testing.T, volumeID, bucket string, hasBucket bool, targetPath, mountFlags, ctxKey, ctxValue string, readOnly, hasMount bool) {
		nodeTestEnv := initNodeServerTestEnv(t)
		nodeTestEnv.mockMounter.EXPECT().
			Mount(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
			DoAndReturn(func(_ context.Context, gotBucket, gotTarget string, _ credentialprovider.ProvideContext,
				_ map[string]string, args mountpoint.Args, _ string, userEnv envprovider.Environment) error {
				if args.Has(mountpoint.ArgFsTab) || args.Has(mountpoint.ArgCABundle) {
					t.Errorf("the mounter got a refused flag: %v", args.SortedList())
				}
				if !strings.HasPrefix(gotTarget, util.ContainerKubeletPath()) {
					t.Errorf("the mounter got target %q, outside the kubelet path %q", gotTarget, util.ContainerKubeletPath())
				}
				if hasBucket && gotBucket != bucket {
					t.Errorf("the mounter got bucket %q, want %q", gotBucket, bucket)
				}
				for key := range userEnv {
					if !slices.Contains([]string{envprovider.EnvHTTPSProxy, envprovider.EnvNoProxy}, key) {
						t.Errorf("the mounter got user environment variable %q, which is not on the allowlist", key)
					}
				}
				return nil
			}).AnyTimes()

		volCtx := map[string]string{}
		if ctxKey != "" {
			volCtx[ctxKey] = ctxValue
		}
		if hasBucket {
			volCtx["bucketName"] = bucket
		}
		volCap := &csi.VolumeCapability{
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
		}
		if hasMount {
			var flags []string
			if mountFlags != "" {
				flags = strings.Split(mountFlags, "\n")
			}
			volCap.AccessType = &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{MountFlags: flags}}
		}

		_, err := nodeTestEnv.server.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
			VolumeId:         volumeID,
			TargetPath:       targetPath,
			VolumeCapability: volCap,
			VolumeContext:    volCtx,
			Readonly:         readOnly,
		})
		// The mounter accepts everything here, so only the request itself can be refused, and only as invalid.
		if err != nil && status.Code(err) != codes.InvalidArgument {
			t.Errorf("NodePublishVolume refused a request with %v, want InvalidArgument: %v", status.Code(err), err)
		}
	})
}
