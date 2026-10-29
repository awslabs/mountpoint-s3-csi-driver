package node

import (
	"fmt"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/volumecontext"
)

func TestLogSafeNodePublishVolumeRequest(t *testing.T) {
	const (
		proxyPassword = "proxy-password-value"
		saToken       = "service-account-token-value"
		secret        = "secret-value"
	)
	req := &csi.NodePublishVolumeRequest{
		VolumeId:   "vol-id",
		TargetPath: "/var/lib/kubelet/pods/pod-uid/volumes/kubernetes.io~csi/pv/mount",
		Secrets:    map[string]string{"key": secret},
		VolumeContext: map[string]string{
			volumecontext.BucketName:              "amzn-s3-demo-bucket",
			"mountpointEnv.HTTPS_PROXY":           "http://user:" + proxyPassword + "@proxy:3128",
			volumecontext.CSIServiceAccountTokens: `{"sts.amazonaws.com":{"token":"` + saToken + `"}}`,
		},
	}

	// Format it the way NodePublishVolume logs it.
	logged := fmt.Sprintf("%+v", logSafeNodePublishVolumeRequest(req))

	for _, sensitive := range []string{proxyPassword, saToken, secret} {
		if strings.Contains(logged, sensitive) {
			t.Errorf("Log output contains sensitive value %q: %s", sensitive, logged)
		}
	}
	for _, kept := range []string{"vol-id", "amzn-s3-demo-bucket", "proxy:3128"} {
		if !strings.Contains(logged, kept) {
			t.Errorf("Log output is missing %q: %s", kept, logged)
		}
	}

	// The request itself must stay intact, as NodePublishVolume uses it after logging.
	if got := req.VolumeContext["mountpointEnv.HTTPS_PROXY"]; got != "http://user:"+proxyPassword+"@proxy:3128" {
		t.Errorf("Original request's volume context was modified: %q", got)
	}
	if _, ok := req.VolumeContext[volumecontext.CSIServiceAccountTokens]; !ok {
		t.Errorf("Original request's service account tokens were removed")
	}
}
