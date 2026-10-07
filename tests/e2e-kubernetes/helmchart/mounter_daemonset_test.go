package helmchart

import (
	"testing"

	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

func TestMounterDaemonSetRejectsInvalidCache(t *testing.T) {
	// Helm replaces a list value whole, so each row spells out its full daemonsetMounters entry.
	for _, testCase := range []struct {
		name                string
		mounter             string
		expectedErrContains string
	}{
		// Cache volume
		{
			name:                "cache, no emptyDir nor ephemeral",
			mounter:             `{maxVolumesPerNode: 4, memoryLimitStrategy: none, cache: {}}`,
			expectedErrContains: "cache must set exactly one of emptyDir or ephemeral",
		},
		{
			name:                "cache + emptyDir + ephemeral",
			mounter:             `{maxVolumesPerNode: 4, memoryLimitStrategy: none, cache: {emptyDir: {sizeLimit: 2Gi}, ephemeral: {storageClassName: gp3, resourceRequests: 10Gi}}}`,
			expectedErrContains: "cache must set exactly one of emptyDir or ephemeral",
		},
		{
			name:                "emptyDir, unknown medium",
			mounter:             `{maxVolumesPerNode: 4, memoryLimitStrategy: none, cache: {emptyDir: {medium: HugePages-2Mi}}}`,
			expectedErrContains: `cache.emptyDir.medium must be "" (node disk) or "Memory" (tmpfs), got "HugePages-2Mi"`,
		},
		{
			name:                "ephemeral, no storageClassName",
			mounter:             `{maxVolumesPerNode: 4, memoryLimitStrategy: none, cache: {ephemeral: {resourceRequests: 10Gi}}}`,
			expectedErrContains: "cache.ephemeral.storageClassName is required",
		},
		{
			name:                "ephemeral, no resourceRequests",
			mounter:             `{maxVolumesPerNode: 4, memoryLimitStrategy: none, cache: {ephemeral: {storageClassName: gp3}}}`,
			expectedErrContains: "cache.ephemeral.resourceRequests is required",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := renderMounterDaemonSet(t, "daemonsetMounters: ["+testCase.mounter+"]")
			if err == nil {
				t.Fatal("expected the chart to fail to render")
			}
			assert.Contains(t, err.Error(), testCase.expectedErrContains)
		})
	}
}

// renderMounterDaemonSet renders the chart with values, a values file's YAML, and returns the mounter DaemonSet's manifest.
func renderMounterDaemonSet(t *testing.T, values string) (string, error) {
	t.Helper()
	ch, err := loader.Load("../../../charts/aws-mountpoint-s3-csi-driver")
	assert.NoError(t, err)
	userValues, err := chartutil.ReadValues([]byte(values))
	assert.NoError(t, err)
	// The mounter renders only in daemonset mode, and the chart refuses to render from a source checkout without unsupportedDevInstall.
	userValues["experimental"] = map[string]any{"mounterMode": "daemonset"}
	userValues["unsupportedDevInstall"] = true
	renderValues, err := chartutil.ToRenderValues(ch, userValues, chartutil.ReleaseOptions{Name: "s3-csi", Namespace: "kube-system"}, chartutil.DefaultCapabilities)
	assert.NoError(t, err)
	manifests, err := engine.Render(ch, renderValues)
	return manifests[ch.Name()+"/templates/mounter-daemonset.yaml"], err
}
