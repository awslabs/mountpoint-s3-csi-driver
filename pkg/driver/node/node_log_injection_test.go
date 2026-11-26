package node_test

import (
	"context"
	"strings"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/golang/mock/gomock"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/envprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil"
)

// A bucket name, mount option or other value passed on to Mountpoint with a line break, such as the CR/LF of a log
// injection attempt, is rejected before Mountpoint gets it, since Mountpoint can print what it is given as is.
func TestNodePublishVolumeRejectsLineBreaks(t *testing.T) {
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
			if name == "mountpointEnv" {
				// An environment variable can hold a secret, such as a proxy password: name it, never echo it.
				if strings.Contains(msg, "log_injection") {
					t.Errorf("error message should not echo a mountpointEnv value: %q", msg)
				}
			} else if !strings.Contains(msg, `log_injection_before\r\nI9999`) {
				t.Errorf("error message should show the value escaped: %q", msg)
			}
		})
	}
}

// The rejection is for line breaks only. Other control characters, such as a tab in a prefix (legal in an S3 key
// and accepted before V3), still mount: everything that relays Mountpoint's output escapes them.
func TestNodePublishVolumeAcceptsTabInMountOption(t *testing.T) {
	env := initNodeServerTestEnv(t)
	var gotPrefix string
	env.mockMounter.EXPECT().Mount(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, _ credentialprovider.ProvideContext, _ map[string]string, args mountpoint.Args, _ string, _ envprovider.Environment) error {
			gotPrefix, _ = args.Value("--prefix")
			return nil
		})
	_, err := env.server.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId: "test-volume-id",
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{MountFlags: []string{"prefix data\tset/"}}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
		},
		TargetPath:    "/var/lib/kubelet/target/path",
		VolumeContext: map[string]string{"bucketName": "test-bucket-name"},
	})
	if err != nil {
		t.Fatalf("a tab in a mount option should mount, got %v", err)
	}
	if gotPrefix != "data\tset/" {
		t.Errorf("Mountpoint should get the prefix with its tab as is, got %q", gotPrefix)
	}
}

// A proxy URL written as a YAML block scalar ends in a newline and can carry a password. The error, which kubelet
// puts in the Pod's FailedMount event, names the attribute but not its value.
func TestNodePublishVolumeDoesNotEchoMountpointEnvValue(t *testing.T) {
	env := initNodeServerTestEnv(t) // no Mount expected
	_, err := env.server.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId: "test-volume-id",
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
		},
		TargetPath:    "/var/lib/kubelet/target/path",
		VolumeContext: map[string]string{"bucketName": "test-bucket-name", "mountpointEnv.HTTPS_PROXY": "http://user:secret@proxy.example.com\n"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
	msg := status.Convert(err).Message()
	if strings.Contains(msg, "secret") || strings.Contains(msg, "proxy.example.com") {
		t.Errorf("error message echoes the mountpointEnv value: %q", msg)
	}
	if !strings.Contains(msg, "mountpointEnv.HTTPS_PROXY") {
		t.Errorf("error message should name the attribute: %q", msg)
	}
}

// NEL, U+2028 and U+2029 count as line breaks too: some log viewers end a record on them.
func TestNodePublishVolumeRejectsUnicodeLineBreaks(t *testing.T) {
	for _, lineBreak := range []string{"\u0085", "\u2028", "\u2029"} {
		env := initNodeServerTestEnv(t) // no Mount expected
		_, err := env.server.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
			VolumeId: "test-volume-id",
			VolumeCapability: &csi.VolumeCapability{
				AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{MountFlags: []string{"prefix a" + lineBreak + "b/"}}},
				AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
			},
			TargetPath:    "/var/lib/kubelet/target/path",
			VolumeContext: map[string]string{"bucketName": "test-bucket-name"},
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%q: want InvalidArgument, got %v", lineBreak, err)
		}
	}
}

// The HTTPS_PROXY URL can hold a proxy password, so NodePublishVolume's request log at -v=4 (the chart default) shows
// the key but never its value, whatever its form: a URL, a scheme-less user:password@host, or a value no URL parser
// accepts. Other mountpointEnv.* values, such as NO_PROXY, stay in the log for debugging.
func TestNodePublishVolumeDoesNotLogHTTPSProxyValue(t *testing.T) {
	for name, proxy := range map[string]string{
		"URL":         "http://user:s3cr3tpw@proxy.example.com:3128",
		"scheme-less": "user:s3cr3tpw@proxy.example.com:3128",
		"malformed":   "http://user:s3cr3tpw%zz@proxy:3128",
	} {
		t.Run(name, func(t *testing.T) {
			logs := testutil.CaptureKlogAtVerbosity(t, 4)
			env := initNodeServerTestEnv(t)
			env.mockMounter.EXPECT().Mount(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
			env.server.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
				VolumeId: "test-volume-id",
				VolumeCapability: &csi.VolumeCapability{
					AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
					AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
				},
				TargetPath: "/var/lib/kubelet/target/path",
				VolumeContext: map[string]string{
					"bucketName":                "test-bucket-name",
					"mountpointEnv.HTTPS_PROXY": proxy,
					"mountpointEnv.NO_PROXY":    ".internal.example.com,10.0.0.0/8",
				},
			})
			got := logs()
			if !strings.Contains(got, "NodePublishVolume: new request") {
				t.Fatalf("the request log line is missing, so this test checks nothing: %q", got)
			}
			if strings.Contains(got, "s3cr3tpw") {
				t.Errorf("the HTTPS_PROXY value was logged: %q", got)
			}
			if !strings.Contains(got, `key:"mountpointEnv.HTTPS_PROXY" value:"REDACTED"`) {
				t.Errorf("the log should still show mountpointEnv.HTTPS_PROXY, with its value REDACTED: %q", got)
			}
			if !strings.Contains(got, `key:"mountpointEnv.NO_PROXY" value:".internal.example.com,10.0.0.0/8"`) {
				t.Errorf("the log should show the NO_PROXY value: %q", got)
			}
		})
	}
}
