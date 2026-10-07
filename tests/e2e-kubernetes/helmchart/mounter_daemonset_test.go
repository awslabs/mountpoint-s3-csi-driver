package helmchart

import (
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

func TestMounterDaemonSetRejectsInvalidMemory(t *testing.T) {
	for _, testCase := range []struct {
		name                string
		mounter             string
		expectedErrContains string
	}{
		{
			name:                "memoryLimitStrategy unknown",
			mounter:             `{maxVolumesPerNode: 4, memoryLimitStrategy: halves}`,
			expectedErrContains: "memoryLimitStrategy must be one of",
		},
		{
			name:                "equalSplit, no memory request",
			mounter:             `{maxVolumesPerNode: 4, memoryLimitStrategy: equalSplit}`,
			expectedErrContains: "resources.requests.memory is required when memoryLimitStrategy=equalSplit",
		},
		{
			name:                "equalSplit + maxVolumesPerNode 0",
			mounter:             `{maxVolumesPerNode: 0, memoryLimitStrategy: equalSplit, resources: {requests: {memory: 4Gi}}}`,
			expectedErrContains: "maxVolumesPerNode is required and must be greater than 0 when memoryLimitStrategy=equalSplit",
		},
		{
			name:                "memory request at the 64 MiB reserve",
			mounter:             `{maxVolumesPerNode: 4, memoryLimitStrategy: equalSplit, resources: {requests: {memory: 64Mi}}}`,
			expectedErrContains: "is at or below the 64 MiB the mounter reserves for its own process",
		},
		{
			name:                "memory share under 512 MiB",
			mounter:             `{maxVolumesPerNode: 4, memoryLimitStrategy: equalSplit, resources: {requests: {memory: 1Gi}}}`,
			expectedErrContains: "gives 240 MiB per Mountpoint process, below Mountpoint's minimum --memory-target of 512 MiB",
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
			mounter:             `{maxVolumesPerNode: 4, memoryLimitStrategy: none, cache: {limitStrategy: none}}`,
			expectedErrContains: "cache must set exactly one of emptyDir or ephemeral",
		},
		{
			name:                "cache + emptyDir + ephemeral",
			mounter:             `{maxVolumesPerNode: 4, memoryLimitStrategy: none, cache: {emptyDir: {sizeLimit: 2Gi}, ephemeral: {storageClassName: gp3, resourceRequests: 10Gi}, limitStrategy: none}}`,
			expectedErrContains: "cache must set exactly one of emptyDir or ephemeral",
		},
		{
			name:                "emptyDir, unknown medium",
			mounter:             `{maxVolumesPerNode: 4, memoryLimitStrategy: none, cache: {emptyDir: {medium: HugePages-2Mi}, limitStrategy: none}}`,
			expectedErrContains: `cache.emptyDir.medium must be "" (node disk) or "Memory" (tmpfs), got "HugePages-2Mi"`,
		},
		{
			name:                "equalSplit + emptyDir, no sizeLimit",
			mounter:             `{maxVolumesPerNode: 4, memoryLimitStrategy: none, cache: {emptyDir: {}, limitStrategy: equalSplit}}`,
			expectedErrContains: `cache.emptyDir.sizeLimit is required when cache.limitStrategy is "equalSplit"`,
		},
		{
			name:                "ephemeral, no storageClassName",
			mounter:             `{maxVolumesPerNode: 4, memoryLimitStrategy: none, cache: {ephemeral: {resourceRequests: 10Gi}, limitStrategy: none}}`,
			expectedErrContains: "cache.ephemeral.storageClassName is required",
		},
		{
			name:                "ephemeral, no resourceRequests",
			mounter:             `{maxVolumesPerNode: 4, memoryLimitStrategy: none, cache: {ephemeral: {storageClassName: gp3}, limitStrategy: none}}`,
			expectedErrContains: "cache.ephemeral.resourceRequests is required",
		},
		// Cache limit
		{
			name:                "cache, no limitStrategy",
			mounter:             `{maxVolumesPerNode: 4, memoryLimitStrategy: none, cache: {emptyDir: {sizeLimit: 2Gi}}}`,
			expectedErrContains: `cache.limitStrategy must be one of [equalSplit none], got ""`,
		},
		{
			name:                "equalSplit + maxVolumesPerNode 0",
			mounter:             `{maxVolumesPerNode: 0, memoryLimitStrategy: none, cache: {emptyDir: {sizeLimit: 2Gi}, limitStrategy: equalSplit}}`,
			expectedErrContains: "maxVolumesPerNode is required and must be greater than 0 when cache.limitStrategy=equalSplit",
		},
		{
			// 95% of 3Mi split 4 ways is 0.71 MiB.
			name:                "equalSplit, cache share under 1 MiB",
			mounter:             `{maxVolumesPerNode: 4, memoryLimitStrategy: none, cache: {emptyDir: {sizeLimit: 3Mi}, limitStrategy: equalSplit}}`,
			expectedErrContains: "no whole MiB of --max-cache-size. Raise cache.emptyDir.sizeLimit",
		},
		// Memory target Integration for tmpfs
		{
			// (2112Mi - 64Mi) / 4 is exactly 512 MiB, so only counting the 1Gi tmpfs against the memory request rejects this.
			name:                "tmpfs, memory share under 512 MiB",
			mounter:             `{maxVolumesPerNode: 4, memoryLimitStrategy: equalSplit, resources: {requests: {memory: 2112Mi}}, cache: {emptyDir: {sizeLimit: 1Gi, medium: Memory}, limitStrategy: equalSplit}}`,
			expectedErrContains: "the 1024 MiB tmpfs cache volume, which split across maxVolumesPerNode=4 gives 256 MiB per Mountpoint process, below Mountpoint's minimum --memory-target of 512 MiB. Raise resources.requests.memory or lower maxVolumesPerNode, or lower cache.emptyDir.sizeLimit.",
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

func TestMounterDaemonSetGrantsCacheCapabilitiesOnlyWithACache(t *testing.T) {
	t.Run("grants CHOWN, DAC_OVERRIDE and FOWNER with a cache", func(t *testing.T) {
		manifest, err := renderMounterDaemonSet(t, `daemonsetMounters: [{maxVolumesPerNode: 4, memoryLimitStrategy: none, cache: {emptyDir: {sizeLimit: 2Gi}, limitStrategy: equalSplit}}]`)
		assert.NoError(t, err)
		assert.Contains(t, manifest, "- CHOWN")
		assert.Contains(t, manifest, "- DAC_OVERRIDE")
		assert.Contains(t, manifest, "- FOWNER")
	})

	t.Run("grants none of CHOWN, DAC_OVERRIDE and FOWNER without a cache", func(t *testing.T) {
		manifest, err := renderMounterDaemonSet(t, `daemonsetMounters: [{maxVolumesPerNode: 4, memoryLimitStrategy: none}]`)
		assert.NoError(t, err)
		assert.Equals(t, false, strings.Contains(manifest, "- CHOWN"))
		assert.Equals(t, false, strings.Contains(manifest, "- DAC_OVERRIDE"))
		assert.Equals(t, false, strings.Contains(manifest, "- FOWNER"))
	})
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
