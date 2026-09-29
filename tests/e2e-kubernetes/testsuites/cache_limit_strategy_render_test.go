package custom_testsuites

import (
	"path/filepath"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
)

// renderChart renders the working-tree chart with the given values, the way `helm template` does, and
// returns the rendered templates keyed by path.
func renderChart(t *testing.T, values map[string]any) (map[string]string, error) {
	t.Helper()
	// helmChartSource is relative to the suite root, where ginkgo runs; `go test` runs one level deeper.
	path := filepath.Join("..", helmChartSource)
	chart, err := loader.Load(path)
	if err != nil {
		t.Fatalf("loading the chart at %s: %v", path, err)
	}

	rendered, err := chartutil.ToRenderValues(chart, values, chartutil.ReleaseOptions{
		Name:      "release-name",
		Namespace: "kube-system",
	}, nil)
	if err != nil {
		return nil, err
	}

	return engine.Render(chart, rendered)
}

// mounterValues returns render values whose daemonsetMounters[0] is complete. Helm replaces a list
// element rather than merging it, so a partial one drops logLevel and the mounter renders `--v=` and
// exits 2 on it. unsupportedDevInstall keeps templates/node.yaml from failing the whole render first.
func mounterValues(mounter map[string]any) map[string]any {
	element := map[string]any{
		"maxVolumesPerNode":   4,
		"memoryLimitStrategy": "none",
		"resources":           map[string]any{"requests": map[string]any{"memory": "2Gi", "cpu": "500m"}},
		"logLevel":            4,
		"podLabels":           map[string]any{},
		"tolerateAllTaints":   true,
		"defaultTolerations":  true,
		"tolerations":         []any{},
		"affinity":            map[string]any{},
		"imagePullSecrets":    []any{},
	}
	for key, value := range mounter {
		element[key] = value
	}
	return map[string]any{
		"unsupportedDevInstall": true,
		"experimental":          map[string]any{"mounterMode": "daemonset"},
		"daemonsetMounters":     []any{element},
	}
}

// omitStrategy leaves cacheLimitStrategy out of the cache block, which the chart requires.
const omitStrategy = ""

func emptyDirCache(strategy string, emptyDir map[string]any) map[string]any {
	cache := map[string]any{"emptyDir": emptyDir}
	if strategy != omitStrategy {
		cache["cacheLimitStrategy"] = strategy
	}
	return cache
}

func ephemeralCache(strategy, resourceRequests string) map[string]any {
	cache := map[string]any{"ephemeral": map[string]any{"storageClassName": "gp3", "resourceRequests": resourceRequests}}
	if strategy != omitStrategy {
		cache["cacheLimitStrategy"] = strategy
	}
	return cache
}

// The messages are asserted in full because they are the only place an operator is told what the
// arithmetic was and which of the values to change.
func TestRenderCacheLimitStrategy(t *testing.T) {
	testCases := []struct {
		name     string
		mounter  map[string]any
		wantFail string
	}{
		// Values that must render
		{
			name: "none allows maxVolumesPerNode 0, which it does not divide by",
			mounter: map[string]any{
				"maxVolumesPerNode": 0,
				"cache":             emptyDirCache("none", map[string]any{"medium": "", "sizeLimit": "4Gi"}),
			},
		},
		{
			name:    "none allows a tmpfs with no sizeLimit",
			mounter: map[string]any{"cache": emptyDirCache("none", map[string]any{"medium": "Memory"})},
		},
		{
			name: "a tmpfs that leaves Mountpoint its minimum memory target is allowed",
			mounter: map[string]any{
				"memoryLimitStrategy": "equalSplit",
				"resources":           map[string]any{"requests": map[string]any{"memory": "4Gi", "cpu": "500m"}},
				"cache":               emptyDirCache("equalSplit", map[string]any{"medium": "Memory", "sizeLimit": "1Gi"}),
			},
		},
		{
			name: "a disk emptyDir is not charged to memory",
			mounter: map[string]any{
				"memoryLimitStrategy": "equalSplit",
				"resources":           map[string]any{"requests": map[string]any{"memory": "4Gi", "cpu": "500m"}},
				"cache":               emptyDirCache("equalSplit", map[string]any{"medium": "", "sizeLimit": "8Gi"}),
			},
		},
		// Values that must be refused
		{
			name:     "a cache block with no strategy is refused, as memoryLimitStrategy is",
			mounter:  map[string]any{"cache": emptyDirCache(omitStrategy, map[string]any{"medium": "", "sizeLimit": "4Gi"})},
			wantFail: `daemonsetMounters[]: cache.cacheLimitStrategy must be one of [equalSplit none], got "".`,
		},
		{
			name:     "a misspelled strategy inside the cache block is refused",
			mounter:  map[string]any{"cache": emptyDirCache("equalsplit", map[string]any{"medium": "", "sizeLimit": "4Gi"})},
			wantFail: `daemonsetMounters[]: cache.cacheLimitStrategy must be one of [equalSplit none], got "equalsplit". "equalSplit" gives every Mountpoint process on the node an equal share of the cache volume as its --max-cache-size; "none" leaves --max-cache-size to each PV's mountOptions.`,
		},
		{
			name:     "a cache block with both emptyDir and ephemeral is refused",
			mounter:  map[string]any{"cache": map[string]any{"emptyDir": map[string]any{"sizeLimit": "4Gi"}, "ephemeral": map[string]any{"storageClassName": "gp3", "resourceRequests": "10Gi"}}},
			wantFail: "daemonsetMounters[]: cache must set exactly one of emptyDir or ephemeral.",
		},
		{
			// A bare `cache:` reaches the chart as null, which is falsy, so this row is the one that pins
			// hasKey over truthiness: read for truth it would mean "no cache" silently.
			name:     "a bare cache block is refused rather than read as no cache",
			mounter:  map[string]any{"cache": nil},
			wantFail: "daemonsetMounters[]: cache must set exactly one of emptyDir or ephemeral.",
		},
		{
			name:     "a HugePages medium is refused",
			mounter:  map[string]any{"cache": emptyDirCache("none", map[string]any{"medium": "HugePages"})},
			wantFail: `daemonsetMounters[0].cache.emptyDir.medium must be "" (node disk) or "Memory" (tmpfs), got "HugePages"`,
		},
		{
			name:     "an ephemeral cache with no storageClassName is refused",
			mounter:  map[string]any{"cache": map[string]any{"cacheLimitStrategy": "equalSplit", "ephemeral": map[string]any{"resourceRequests": "10Gi"}}},
			wantFail: "daemonsetMounters[0].cache.ephemeral.storageClassName is required when the cache block sets ephemeral",
		},
		{
			// Two rows and not one: storageClassName renders first, so one set of values reaches one required.
			name:     "an ephemeral cache with no resourceRequests is refused",
			mounter:  map[string]any{"cache": map[string]any{"cacheLimitStrategy": "equalSplit", "ephemeral": map[string]any{"storageClassName": "gp3"}}},
			wantFail: "daemonsetMounters[0].cache.ephemeral.resourceRequests is required when the cache block sets ephemeral",
		},
		{
			name: "equalSplit has no divisor when maxVolumesPerNode is 0",
			mounter: map[string]any{
				"maxVolumesPerNode": 0,
				"cache":             emptyDirCache("equalSplit", map[string]any{"medium": "", "sizeLimit": "4Gi"}),
			},
			wantFail: "daemonsetMounters[]: maxVolumesPerNode is required and must be greater than 0 when cache.cacheLimitStrategy=equalSplit, got 0: it is the divisor for the cache volume's size. Set cache.cacheLimitStrategy=none to leave --max-cache-size to each PV's mountOptions.",
		},
		{
			name:     "equalSplit has no size to divide when the emptyDir has no sizeLimit",
			mounter:  map[string]any{"cache": emptyDirCache("equalSplit", map[string]any{"medium": ""})},
			wantFail: `daemonsetMounters[0].cache.emptyDir.sizeLimit is required when cache.cacheLimitStrategy is "equalSplit"`,
		},
		{
			// A tmpfs is charged to the mounter's memory, so memory's equalSplit divides only what it leaves.
			name: "a tmpfs that leaves Mountpoint less than its minimum memory target is refused",
			mounter: map[string]any{
				"memoryLimitStrategy": "equalSplit",
				"resources":           map[string]any{"requests": map[string]any{"memory": "4Gi", "cpu": "500m"}},
				"cache":               emptyDirCache("equalSplit", map[string]any{"medium": "Memory", "sizeLimit": "3Gi"}),
			},
			wantFail: "daemonsetMounters[]: resources.requests.memory (4Gi) leaves 960 MiB after the 64 MiB reserved for the mounter process and the 3072 MiB tmpfs cache volume, which split across maxVolumesPerNode=4 gives 240 MiB per Mountpoint process, below Mountpoint's minimum --memory-target of 512 MiB.",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := renderChart(t, mounterValues(testCase.mounter))

			if testCase.wantFail == "" {
				if err != nil {
					t.Fatalf("expected the chart to render, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected the chart to refuse these values")
			}
			if !strings.Contains(err.Error(), testCase.wantFail) {
				t.Errorf("render error\n got: %v\nwant it to contain: %s", err, testCase.wantFail)
			}
		})
	}
}

// TestRenderCacheLimitStrategyPlumbing pins what reaches the mounter DaemonSet, because a flag or env
// var that silently stops being rendered leaves every mount unbounded rather than failing anything.
// s3-csi-node is not checked: it is strategy-blind, so nothing about the strategy reaches it.
func TestRenderCacheLimitStrategyPlumbing(t *testing.T) {
	testCases := []struct {
		name        string
		mounter     map[string]any
		wantMounter []string
		// wantAbsent must not render, since a phantom flag or capacity would reach the mounter silently.
		wantAbsent []string
	}{
		{
			name:    "equalSplit reaches the mounter with the cache volume's size",
			mounter: map[string]any{"cache": emptyDirCache("equalSplit", map[string]any{"medium": "", "sizeLimit": "4Gi"})},
			wantMounter: []string{
				"- --cache-limit-strategy=equalSplit",
				"- --max-volumes-per-node=4",
				`- name: MOUNTER_CACHE_CAPACITY`,
				`value: "4Gi"`,
				`medium: ""`,
				"sizeLimit: 4Gi",
				"mountPath: /cache",
				"fsGroup: 1000",
			},
		},
		{
			name:        "an ephemeral volume's resourceRequests is the capacity the mounter divides",
			mounter:     map[string]any{"cache": ephemeralCache("equalSplit", "10Gi")},
			wantMounter: []string{`value: "10Gi"`, `storageClassName: "gp3"`, "storage: 10Gi"},
		},
		{
			name:        "none reaches the mounter and sends no capacity when the emptyDir declares none",
			mounter:     map[string]any{"cache": emptyDirCache("none", map[string]any{"medium": ""})},
			wantMounter: []string{"- --cache-limit-strategy=none"},
			wantAbsent:  []string{"MOUNTER_CACHE_CAPACITY"},
		},
		{
			name:        "a tmpfs cache tells the mounter its medium, so memory can account for it",
			mounter:     map[string]any{"cache": emptyDirCache("equalSplit", map[string]any{"medium": "Memory", "sizeLimit": "1Gi"})},
			wantMounter: []string{"- --cache-medium=Memory", `medium: "Memory"`, "sizeLimit: 1Gi"},
		},
		{
			// Allowed under none, so it must still reach the pod spec, where the kubelet sizes the volume from it.
			name:        "none emits a sizeLimit it does not divide",
			mounter:     map[string]any{"cache": emptyDirCache("none", map[string]any{"medium": "", "sizeLimit": "4Gi"})},
			wantMounter: []string{"- --cache-limit-strategy=none", "sizeLimit: 4Gi", `value: "4Gi"`},
		},
		{
			// Without a cache volume there is nothing to divide, so neither flag nor capacity may render.
			name:       "a node with no cache volume gets no cache strategy",
			mounter:    map[string]any{},
			wantAbsent: []string{"--cache-limit-strategy", "MOUNTER_CACHE_CAPACITY", "mountPath: /cache", "fsGroup"},
		},
		{
			name: "the cache volume's size reaches the mounter without a memory request",
			mounter: map[string]any{
				"resources": map[string]any{"requests": map[string]any{"cpu": "500m"}},
				"cache":     emptyDirCache("equalSplit", map[string]any{"medium": "", "sizeLimit": "4Gi"}),
			},
			wantMounter: []string{`- name: MOUNTER_CACHE_CAPACITY`, `value: "4Gi"`},
			wantAbsent:  []string{"MOUNTER_MEMORY_REQUEST_BYTES"},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			rendered, err := renderChart(t, mounterValues(testCase.mounter))
			if err != nil {
				t.Fatalf("expected the chart to render, got: %v", err)
			}

			mounterDaemonSet := rendered["aws-mountpoint-s3-csi-driver/templates/mounter-daemonset.yaml"]
			for _, want := range testCase.wantMounter {
				if !strings.Contains(mounterDaemonSet, want) {
					t.Errorf("mounter-daemonset.yaml does not contain %q", want)
				}
			}
			for _, absent := range testCase.wantAbsent {
				if strings.Contains(mounterDaemonSet, absent) {
					t.Errorf("mounter-daemonset.yaml contains %q", absent)
				}
			}
		})
	}
}
