package custom_testsuites

import (
	"path/filepath"
	"reflect"
	"testing"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
)

// chartForTest loads the working-tree chart; helmChartSource is relative to the suite root, one level above where `go test` runs.
func chartForTest(t *testing.T) *chart.Chart {
	t.Helper()
	path := filepath.Join("..", helmChartSource)
	ch, err := loader.Load(path)
	if err != nil {
		t.Fatalf("loading the chart at %s: %v", path, err)
	}
	return ch
}

// A cache reconfiguration must always send a *complete* daemonsetMounters element: Helm replaces list elements, so a partial one
// renders an absent logLevel as `--v=` and the container exits 2, with nothing naming the cause.
func TestMounterElementIsAlwaysComplete(t *testing.T) {
	ch := chartForTest(t)

	// Keys the chart's own values.yaml defines, and therefore the ones the rendered mounter needs.
	chartElement := ch.Values["daemonsetMounters"].([]any)[0].(map[string]any)
	if _, ok := chartElement["logLevel"]; !ok {
		t.Fatal("the chart's daemonsetMounters[0] has no logLevel; this test's premise is wrong")
	}

	tmpfs := map[string]any{"emptyDir": map[string]any{"medium": "Memory"}}

	for _, tc := range []struct {
		name     string
		userVals map[string]any
	}{
		{
			// A dev-cluster install: only --set overrides, no daemonsetMounters at all.
			name:     "no user-supplied element",
			userVals: map[string]any{"image": map[string]any{"tag": "latest"}},
		},
		{
			// A CI install: the whole element supplied via --values.
			name:     "complete user-supplied element",
			userVals: map[string]any{"daemonsetMounters": []any{deepCopyMap(chartElement)}},
		},
		{
			// What a previous run of this suite used to leave behind. The overlay must repair it rather than propagate it, or each run strips the element further.
			name: "partial user-supplied element from an earlier run",
			userVals: map[string]any{
				"daemonsetMounters": []any{map[string]any{"cache": tmpfs}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			element := mounterElement(ch, tc.userVals)

			// Both inputs are reused: baseValues is the restore payload, and baseMounter is what every later reconfiguration builds on.
			userValsBefore := deepCopyMap(tc.userVals)
			elementBefore := deepCopyMap(element)

			out := withMounterCache(tc.userVals, element, tmpfs, strategyFromCacheBlock, 0)

			sent := out["daemonsetMounters"].([]any)[0].(map[string]any)
			for k := range chartElement {
				if _, ok := sent[k]; !ok {
					t.Errorf("sent element is missing %q, which the chart's values.yaml defines", k)
				}
			}
			if got := sent["logLevel"]; got == nil || got == "" {
				t.Errorf("logLevel is %v; the mounter renders --v= and exits 2 on it", got)
			}
			if sent["cache"] == nil {
				t.Error("sent element has no cache block")
			}

			if !reflect.DeepEqual(userValsBefore, tc.userVals) {
				t.Errorf("withMounterCache mutated the caller's values:\n before %#v\n after  %#v", userValsBefore, tc.userVals)
			}
			if !reflect.DeepEqual(elementBefore, element) {
				t.Errorf("withMounterCache mutated the mounter element:\n before %#v\n after  %#v", elementBefore, element)
			}
		})
	}
}

// Removing the cache must still send everything else, which is what the no-cache spec relies on.
func TestMounterElementWithoutCacheKeepsTheRest(t *testing.T) {
	ch := chartForTest(t)
	userVals := map[string]any{
		"daemonsetMounters": []any{map[string]any{
			"cache": map[string]any{"ephemeral": map[string]any{"storageClassName": "gp3", "resourceRequests": "10Gi"}},
		}},
	}

	out := withMounterCache(userVals, mounterElement(ch, userVals), nil, strategyFromCacheBlock, 0)
	sent := out["daemonsetMounters"].([]any)[0].(map[string]any)

	if _, ok := sent["cache"]; ok {
		t.Error("cache should be absent when nil is passed")
	}
	if got := sent["logLevel"]; got == nil || got == "" {
		t.Errorf("logLevel is %v; removing the cache must not strip the rest of the element", got)
	}
}

// The chart reads cacheLimitStrategy from inside the cache block and refuses one at element level outright, so the wrong level
// fails `helm upgrade`. Only the `none` Context passes a real strategy, and only on a cluster, so this is the one nesting check.
func TestMounterCacheCarriesTheStrategyInsideTheBlock(t *testing.T) {
	ch := chartForTest(t)
	// Shared between Contexts by the cache suite, so it must come back unmodified.
	shared := map[string]any{"emptyDir": map[string]any{"medium": "Memory", "sizeLimit": "128Mi"}}

	out := withMounterCache(map[string]any{}, mounterElement(ch, nil), shared, "none", 8)
	sent := out["daemonsetMounters"].([]any)[0].(map[string]any)

	cache, ok := sent["cache"].(map[string]any)
	if !ok {
		t.Fatalf("sent element has no cache block: %#v", sent)
	}
	if got := cache[cacheLimitStrategyKey]; got != "none" {
		t.Errorf("the cache block's %s is %v, want \"none\"", cacheLimitStrategyKey, got)
	}
	if _, ok := sent[cacheLimitStrategyKey]; ok {
		t.Error("the strategy was written at element level, which the chart refuses outright")
	}
	if _, ok := shared[cacheLimitStrategyKey]; ok {
		t.Error("the caller's shared cache block was mutated")
	}
	if got := sent["maxVolumesPerNode"]; got != 8 {
		t.Errorf("the element's maxVolumesPerNode is %v, want 8", got)
	}
}

// An install pinned to the NVMe nodes, both ways dev/ has done it, must not carry over into a row: its unpinned workloads would
// land on nodes with no working s3-csi-node.
func TestDriverPlacementIgnoresTheInstallsPinning(t *testing.T) {
	ch := chartForTest(t)
	nvmeOnly := map[string]any{"nodeAffinity": map[string]any{"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{
		"nodeSelectorTerms": []any{map[string]any{"matchExpressions": []any{map[string]any{
			"key": "s3.csi.aws.com/local-nvme", "operator": "In", "values": []any{"true"}}}}}}}}
	userVals := map[string]any{
		"node": map[string]any{
			"nodeSelector":   map[string]any{"s3.csi.aws.com/local-nvme": "true"},
			"affinity":       nvmeOnly,
			"serviceAccount": map[string]any{"annotations": map[string]any{"eks.amazonaws.com/role-arn": "ci-role"}},
		},
		"daemonsetMounters": []any{map[string]any{"affinity": nvmeOnly}},
	}
	chartNodeAffinity := ch.Values["node"].(map[string]any)["affinity"]

	for _, tc := range []struct {
		name         string
		nodeSelector map[string]string
		want         map[string]any
	}{
		{name: "an unpinned row runs on every node", nodeSelector: nil, want: map[string]any{}},
		{name: "a pinned row runs on its own nodes", nodeSelector: map[string]string{"pinned": "yes"}, want: map[string]any{"pinned": "yes"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := withMounterCache(userVals, mounterElement(ch, userVals), nil, strategyFromCacheBlock, 0)
			withDriverPlacement(out, ch.Values, tc.nodeSelector)

			node := out["node"].(map[string]any)
			if !reflect.DeepEqual(node["nodeSelector"], tc.want) {
				t.Errorf("node.nodeSelector is %#v, want %#v", node["nodeSelector"], tc.want)
			}
			if !reflect.DeepEqual(node["affinity"], chartNodeAffinity) {
				t.Errorf("node.affinity is %#v, want the chart's %#v", node["affinity"], chartNodeAffinity)
			}
			if got := out["daemonsetMounters"].([]any)[0].(map[string]any)["affinity"]; !reflect.DeepEqual(got, map[string]any{}) {
				t.Errorf("daemonsetMounters[0].affinity is %#v, want the chart's {}", got)
			}
			// CI's IRSA role lives beside the placement keys, so replacing node wholesale would drop it.
			if node["serviceAccount"] == nil {
				t.Error("node.serviceAccount was dropped with the placement")
			}
			if _, ok := userVals["node"].(map[string]any)["nodeSelector"].(map[string]any)["s3.csi.aws.com/local-nvme"]; !ok {
				t.Error("withDriverPlacement mutated the install's values, which are the restore payload")
			}
		})
	}
}
