package cluster_test

import (
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/cluster"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
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

func TestDistributionUserAgent(t *testing.T) {
	testCases := []struct {
		name         string
		distribution cluster.Distribution
		expected     cluster.Distribution
	}{
		{
			name:         "EKS addon remains eks-addon",
			distribution: cluster.DistributionEKSAddon,
			expected:     cluster.Distribution("eks-addon"),
		},
		{
			name:         "EKS self-managed maps to eks-self-managed",
			distribution: cluster.DistributionEKSSelfManaged,
			expected:     cluster.Distribution("eks-self-managed"),
		},
		{
			name:         "ROSA remains rosa",
			distribution: cluster.DistributionROSA,
			expected:     cluster.Distribution("rosa"),
		},
		{
			name:         "OpenShift maps to other",
			distribution: cluster.DistributionOpenShift,
			expected:     cluster.Distribution("other"),
		},
		{
			name:         "Other remains other",
			distribution: cluster.DistributionOther,
			expected:     cluster.Distribution("other"),
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			gotValue := testCase.distribution.UserAgent()
			assert.Equals(t, testCase.expected, gotValue)
		})
	}
}
