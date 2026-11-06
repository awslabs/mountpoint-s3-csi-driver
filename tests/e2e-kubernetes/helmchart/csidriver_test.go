package helmchart

import (
	"testing"

	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

// Kubelet re-reads each node's volume limit, which follows the node's mounter, on Kubernetes versions with the field.
func TestCSIDriverNodeAllocatableUpdatePeriod(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		kubeVersion chartutil.KubeVersion
		want        *int64
	}{
		{"1.37", chartutil.KubeVersion{Version: "v1.37.0", Major: "1", Minor: "37"}, ptr.To[int64](10)},
		{"EKS 1.34", chartutil.KubeVersion{Version: "v1.34.2-eks-3025e55", Major: "1", Minor: "34"}, ptr.To[int64](10)},
		{"1.33, the field's first version", chartutil.KubeVersion{Version: "v1.33.0", Major: "1", Minor: "33"}, ptr.To[int64](10)},
		{"before 1.33", chartutil.KubeVersion{Version: "v1.32.9", Major: "1", Minor: "32"}, nil},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			csiDriver := renderCSIDriver(t, testCase.kubeVersion)
			assert.Equals(t, testCase.want, csiDriver.Spec.NodeAllocatableUpdatePeriodSeconds)
		})
	}
}

// renderCSIDriver renders the chart's CSIDriver for a cluster running kubeVersion.
func renderCSIDriver(t *testing.T, kubeVersion chartutil.KubeVersion) storagev1.CSIDriver {
	t.Helper()
	ch, err := loader.Load("../../../charts/aws-mountpoint-s3-csi-driver")
	assert.NoError(t, err)
	// The chart refuses to render from a source checkout without unsupportedDevInstall.
	userValues := map[string]any{"unsupportedDevInstall": true}
	capabilities := chartutil.DefaultCapabilities.Copy()
	capabilities.KubeVersion = kubeVersion
	renderValues, err := chartutil.ToRenderValues(ch, userValues, chartutil.ReleaseOptions{Name: "s3-csi", Namespace: "kube-system"}, capabilities)
	assert.NoError(t, err)
	manifests, err := engine.Render(ch, renderValues)
	assert.NoError(t, err)

	var csiDriver storagev1.CSIDriver
	assert.NoError(t, yaml.Unmarshal([]byte(manifests[ch.Name()+"/templates/csidriver.yaml"]), &csiDriver))
	assert.Equals(t, "s3.csi.aws.com", csiDriver.Name)
	return csiDriver
}
