package util_test

import (
	"testing"

	"github.com/awslabs/aws-s3-csi-driver/pkg/util"
	"github.com/awslabs/aws-s3-csi-driver/pkg/util/testutil/assert"
	"k8s.io/utils/ptr"
)

func TestMountpointPodUserID(t *testing.T) {
	testCases := []struct {
		name     string
		variant  util.ClusterVariant
		expected *int64
	}{
		{
			name:     "Default Kubernetes should return 1000",
			variant:  util.DefaultKubernetes,
			expected: ptr.To(int64(1000)),
		},
		{
			name:     "OpenShift should return nil",
			variant:  util.OpenShift,
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
