package node_test

import (
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/scality/mountpoint-s3-csi-driver/pkg/util/testutil/assert"

	"github.com/scality/mountpoint-s3-csi-driver/pkg/driver/node"
	"github.com/scality/mountpoint-s3-csi-driver/pkg/driver/node/volumecontext"
)

func TestLogSafeNodePublishVolumeRequestCoverage(t *testing.T) {
	tests := []struct {
		name           string
		secrets        map[string]string
		expectedOutput map[string]string
	}{
		{
			name: "redact secret_access_key only",
			secrets: map[string]string{
				"key_id":            "AKIAXXXXXXXXXXXXXXXX",
				"secret_access_key": "secret-that-should-be-redacted",
			},
			expectedOutput: map[string]string{
				"key_id":            "AKIAXXXXXXXXXXXXXXXX",
				"secret_access_key": "[REDACTED]",
			},
		},
		{
			name: "keep other values",
			secrets: map[string]string{
				"key_id":            "AKIAXXXXXXXXXXXXXXXX",
				"secret_access_key": "secret-that-should-be-redacted",
				"other_key":         "other-value",
			},
			expectedOutput: map[string]string{
				"key_id":            "AKIAXXXXXXXXXXXXXXXX",
				"secret_access_key": "[REDACTED]",
				"other_key":         "other-value",
			},
		},
		{
			name:           "empty secrets",
			secrets:        map[string]string{},
			expectedOutput: map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &csi.NodePublishVolumeRequest{
				VolumeId: "test-volume",
				VolumeContext: map[string]string{
					"bucketName":                          "test-bucket",
					volumecontext.CSIServiceAccountTokens: "some-token-value",
				},
				Secrets: tt.secrets,
			}

			// Call the function through the exported accessor
			safeReq := node.LogSafeNodePublishVolumeRequestForTest(req)

			// Verify secrets are properly redacted
			assert.Equals(t, tt.expectedOutput, safeReq.Secrets)

			// Verify sensitive service account tokens are removed
			_, hasTokens := safeReq.VolumeContext[volumecontext.CSIServiceAccountTokens]
			if hasTokens {
				t.Errorf("Expected tokens to be removed from VolumeContext")
			}

			// Verify other fields are preserved
			assert.Equals(t, req.VolumeId, safeReq.VolumeId)
			if safeReq.VolumeContext == nil {
				t.Errorf("Expected VolumeContext to be not nil")
			}
			assert.Equals(t, "test-bucket", safeReq.VolumeContext["bucketName"])
		})
	}
}
