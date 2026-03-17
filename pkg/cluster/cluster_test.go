package cluster_test

import (
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/cluster"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

func TestVariantString(t *testing.T) {
	testCases := []struct {
		name     string
		variant  cluster.Variant
		expected string
	}{
		{
			name:     "Default Kubernetes should return kubernetes",
			variant:  cluster.DefaultKubernetes,
			expected: "kubernetes",
		},
		{
			name:     "OpenShift should return openshift",
			variant:  cluster.OpenShift,
			expected: "openshift",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			assert.Equals(t, testCase.expected, testCase.variant.String())
		})
	}
}

func TestInstallationMethod(t *testing.T) {
	t.Setenv("INSTALLATION_TYPE", "")

	testCases := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "empty value should return unknown",
			input:    "",
			expected: "unknown",
		},
		{
			name:     "kustomize should be recognized",
			input:    "kustomize",
			expected: "kustomize",
		},
		{
			name:     "eks-addon should be recognized",
			input:    "eks-addon",
			expected: "eks-addon",
		},
		{
			name:     "value should be sanitized and lowercased",
			input:    "  HELM  ",
			expected: "helm",
		},
		{
			name:     "unknown value should return unknown",
			input:    "operator",
			expected: "unknown",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("INSTALLATION_TYPE", testCase.input)
			assert.Equals(t, testCase.expected, cluster.InstallationMethod())
		})
	}
}

func TestMountpointPodUserID(t *testing.T) {
	testCases := []struct {
		name     string
		variant  cluster.Variant
		expected *int64
	}{
		{
			name:     "Default Kubernetes should return 1000",
			variant:  cluster.DefaultKubernetes,
			expected: new(int64(1000)),
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
