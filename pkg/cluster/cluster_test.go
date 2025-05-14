package cluster_test

import (
	"testing"

	"github.com/awslabs/aws-s3-csi-driver/pkg/cluster"
	"github.com/awslabs/aws-s3-csi-driver/pkg/util/testutil/assert"
	"k8s.io/utils/ptr"
)

func TestMountpointPodUserID(t *testing.T) {
	testCases := []struct {
		name     string
		variant  cluster.Variant
		expected *int64
	}{
		{
			name:     "Default Kubernetes should return 1000",
			variant:  cluster.DefaultKubernetes,
			expected: ptr.To(int64(1000)),
		},
		{
			name:     "OpenShift should return nil",
			variant:  cluster.OpenShift,
			expected: nil,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			gotValue := testCase.variant.MountpointPodUserID()
			assert.Equals(t, testCase.expected, gotValue)
		})
	}
}

func TestIsSelectableFieldsSupported(t *testing.T) {
	tests := []struct {
		name          string
		serverVersion string
		want          bool
		wantErr       bool
	}{
		{
			name:          "version greater than minimum supported version",
			serverVersion: "v1.33.0",
			want:          true,
			wantErr:       false,
		},
		{
			name:          "version equal to minimum supported version",
			serverVersion: "v1.32.0",
			want:          true,
			wantErr:       false,
		},
		{
			name:          "version less than minimum supported version",
			serverVersion: "v1.31.0",
			want:          false,
			wantErr:       false,
		},
		{
			name:          "version with patch number",
			serverVersion: "v1.32.2",
			want:          true,
			wantErr:       false,
		},
		{
			name:          "version with release candidate",
			serverVersion: "v1.32.0-rc.1",
			want:          true,
			wantErr:       false,
		},
		{
			name:          "invalid version format",
			serverVersion: "invalid.version",
			want:          false,
			wantErr:       true,
		},
		{
			name:          "empty version string",
			serverVersion: "",
			want:          false,
			wantErr:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cluster.IsSelectableFieldsSupported(tt.serverVersion)

			if tt.wantErr {
				// If we expect an error, we don't check the boolean result
				if err == nil {
					t.Error("expected error but got none")
				}
				return
			}

			// If we don't expect an error, verify there isn't one
			assert.NoError(t, err)

			// Compare the actual result with expected
			assert.Equals(t, tt.want, got)
		})
	}
}
