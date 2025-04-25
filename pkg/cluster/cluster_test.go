package cluster_test

import (
	"testing"

	"github.com/scality/mountpoint-s3-csi-driver/pkg/cluster"
	"github.com/scality/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
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
