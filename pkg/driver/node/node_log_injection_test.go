package node_test

import (
	"context"
	"strings"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A bucket name, mount option or other value passed on to Mountpoint with control characters, such as the CR/LF of a log injection attempt, can never
// be valid. NodePublishVolume rejects it before Mountpoint gets it, since Mountpoint echoes invalid arguments as is.
func TestNodePublishVolumeRejectsControlCharacters(t *testing.T) {
	const injected = "log_injection_before\r\nI9999 00:00:00.000000 1 FORGED_RECORD log_injection_after"
	mountVolCap := func(flags ...string) *csi.VolumeCapability {
		return &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{MountFlags: flags}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
		}
	}
	for name, req := range map[string]*csi.NodePublishVolumeRequest{
		"bucketName": {
			VolumeId:         "test-volume-id",
			VolumeCapability: mountVolCap(),
			TargetPath:       "/var/lib/kubelet/target/path",
			VolumeContext:    map[string]string{"bucketName": "bucket_" + injected},
		},
		// stsRegion becomes Mountpoint's AWS_REGION, which Mountpoint logs as is at debug level.
		"stsRegion": {
			VolumeId:         "test-volume-id",
			VolumeCapability: mountVolCap(),
			TargetPath:       "/var/lib/kubelet/target/path",
			VolumeContext:    map[string]string{"bucketName": "test-bucket-name", "stsRegion": "us-east-1" + injected},
		},
		// A mountpointEnv.* attribute becomes an environment variable of Mountpoint.
		"mountpointEnv": {
			VolumeId:         "test-volume-id",
			VolumeCapability: mountVolCap(),
			TargetPath:       "/var/lib/kubelet/target/path",
			VolumeContext:    map[string]string{"bucketName": "test-bucket-name", "mountpointEnv.HTTPS_PROXY": "http://proxy" + injected},
		},
		"mount option": {
			VolumeId:         "test-volume-id",
			VolumeCapability: mountVolCap("--prefix=" + injected + "/"),
			TargetPath:       "/var/lib/kubelet/target/path",
			VolumeContext:    map[string]string{"bucketName": "test-bucket-name"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			env := initNodeServerTestEnv(t) // no Mount expected: the mock fails the test if Mount is called
			_, err := env.server.NodePublishVolume(context.Background(), req)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("want InvalidArgument, got %v", err)
			}
			msg := status.Convert(err).Message()
			if strings.ContainsAny(msg, "\r\n") {
				t.Errorf("error message has a raw CR/LF: %q", msg)
			}
			if !strings.Contains(msg, `log_injection_before\r\nI9999`) {
				t.Errorf("error message should show the value escaped: %q", msg)
			}
		})
	}
}
