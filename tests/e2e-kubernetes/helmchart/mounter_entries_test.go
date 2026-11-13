package helmchart

import (
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

func TestAdditionalDaemonSetsRejectInvalidValues(t *testing.T) {
	for _, testCase := range []struct {
		name                string
		values              string
		expectedErrContains string
	}{
		{"limit that isn't a number", `mounter: {maxVolumesPerNode: "abc"}`, "mounter.maxVolumesPerNode must be a whole number >= 0"},
		{"decimal limit", `mounter: {maxVolumesPerNode: 4.5}`, "mounter.maxVolumesPerNode must be a whole number >= 0"},
		{"negative limit on an entry", `additionalDaemonSets: {b: {node: {nodeSelector: {x: "1"}}, mounter: {maxVolumesPerNode: -1}}}`, "additionalDaemonSets.b.mounter.maxVolumesPerNode must be a whole number >= 0"},
		{"invalid name", `additionalDaemonSets: {Big_Mem: {node: {nodeSelector: {x: "1"}}}}`, `name "Big_Mem" must be lowercase letters`},
		{"a key outside node and mounter", `additionalDaemonSets: {b: {maxVolumesPerNode: 4, node: {nodeSelector: {x: "1"}}}}`, "additionalDaemonSets.b.maxVolumesPerNode: an entry only takes node and mounter"},
		{"no placement", `additionalDaemonSets: {b: {mounter: {maxVolumesPerNode: 8}}}`, "additionalDaemonSets.b.node: nodeSelector or affinity is required"},
		{"same placement as another entry", `additionalDaemonSets: {a: {node: {nodeSelector: {x: "1"}}}, b: {node: {nodeSelector: {x: "1"}}}}`, "additionalDaemonSets.b.node selects the same nodes as another DaemonSet"},
		{"same placement as the default", `{node: {nodeSelector: {x: "1"}, affinity: null}, additionalDaemonSets: {b: {node: {nodeSelector: {x: "1"}}}}}`, "additionalDaemonSets.b.node selects the same nodes as another DaemonSet"},
		{"invalid entry mounter", `additionalDaemonSets: {b: {node: {nodeSelector: {x: "1"}}, mounter: {memoryLimitStrategy: halves}}}`, "additionalDaemonSets.b.mounter: memoryLimitStrategy must be one of"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := renderDaemonSets(t, testCase.values)
			if err == nil {
				t.Fatal("expected the chart to fail to render")
			}
			assert.Contains(t, err.Error(), testCase.expectedErrContains)
		})
	}
}

func TestAdditionalDaemonSetsPlacementAndOverrides(t *testing.T) {
	daemonSets, err := renderDaemonSets(t, `node: {nodeSelector: {team: data}, logLevel: 5, podAnnotations: {a: node}}
mounter: {maxVolumesPerNode: 4, podAnnotations: {a: mounter}}
additionalDaemonSets:
  bigmem:
    node: {nodeSelector: {pool: bigmem}, podLabels: {cost: ml}, podAnnotations: {b: bigmem}}
    mounter: {maxVolumesPerNode: 32, resources: {requests: {memory: 32Gi}}, podAnnotations: {a: bigmem}}
  cached:
    node: {affinity: {nodeAffinity: {requiredDuringSchedulingIgnoredDuringExecution: {nodeSelectorTerms: [{matchExpressions: [{key: mem, operator: Gt, values: ["65535"]}]}]}}}}`)
	assert.NoError(t, err)
	assert.Equals(t, 6, len(daemonSets))

	// The default pair keeps node's placement, on both pods.
	for _, name := range []string{"s3-csi-node", "s3-csi-mounter"} {
		assert.Equals(t, map[string]string{"kubernetes.io/os": "linux", "team": "data"}, daemonSets[name].Spec.Template.Spec.NodeSelector)
		assert.Equals(t, []string{notSpecial, autoNotSystem}, terms(daemonSets[name]))
	}

	// An entry's pair uses only its own placement, on both pods: node's nodeSelector and affinity aren't copied.
	for _, name := range []string{"s3-csi-node-bigmem", "s3-csi-mounter-bigmem"} {
		assert.Equals(t, map[string]string{"kubernetes.io/os": "linux", "pool": "bigmem"}, daemonSets[name].Spec.Template.Spec.NodeSelector)
		assert.Equals(t, (*corev1.Affinity)(nil), daemonSets[name].Spec.Template.Spec.Affinity)
	}
	for _, name := range []string{"s3-csi-node-cached", "s3-csi-mounter-cached"} {
		assert.Equals(t, map[string]string{"kubernetes.io/os": "linux"}, daemonSets[name].Spec.Template.Spec.NodeSelector)
		assert.Equals(t, []string{"mem Gt 65535"}, terms(daemonSets[name]))
	}

	// Everything else is copied from node and mounter, and the entry overrides what it sets.
	assert.Equals(t, "ml", daemonSets["s3-csi-node-bigmem"].Spec.Template.Labels["cost"])
	assert.Equals(t, true, slices.Contains(daemonSets["s3-csi-node-bigmem"].Spec.Template.Spec.Containers[0].Args, "--v=5"))
	assert.Equals(t, "32", daemonSets["s3-csi-mounter-bigmem"].Spec.Template.Labels["s3.csi.aws.com/max-volumes-per-node"])
	assert.Equals(t, "32Gi", daemonSets["s3-csi-mounter-bigmem"].Spec.Template.Spec.Containers[0].Resources.Requests.Memory().String())
	assert.Equals(t, "4", daemonSets["s3-csi-mounter-cached"].Spec.Template.Labels["s3.csi.aws.com/max-volumes-per-node"])
	assert.Equals(t, "4Gi", daemonSets["s3-csi-mounter-cached"].Spec.Template.Spec.Containers[0].Resources.Requests.Memory().String())

	// Annotations follow the same copy and override, each on its own kind of pod.
	for name, want := range map[string]map[string]string{
		"s3-csi-node":           {"a": "node"},
		"s3-csi-mounter":        {"a": "mounter"},
		"s3-csi-node-bigmem":    {"a": "node", "b": "bigmem"},
		"s3-csi-mounter-bigmem": {"a": "bigmem"},
		"s3-csi-node-cached":    {"a": "node"},
		"s3-csi-mounter-cached": {"a": "mounter"},
	} {
		assert.Equals(t, want, daemonSets[name].Spec.Template.Annotations)
	}
}

func TestAdditionalDaemonSetsDefaultInstall(t *testing.T) {
	daemonSets, err := renderDaemonSets(t, `{}`)
	assert.NoError(t, err)
	assert.Equals(t, 2, len(daemonSets))
	for _, name := range []string{"s3-csi-node", "s3-csi-mounter"} {
		assert.Equals(t, map[string]string{"kubernetes.io/os": "linux"}, daemonSets[name].Spec.Template.Spec.NodeSelector)
		assert.Equals(t, []string{notSpecial, autoNotSystem}, terms(daemonSets[name]))
	}
	assert.Equals(t, "4", daemonSets["s3-csi-mounter"].Spec.Template.Labels["s3.csi.aws.com/max-volumes-per-node"])
}

func TestAdditionalDaemonSetsQuoteNodeSelectorValues(t *testing.T) {
	// Unquoted, these render as a bool and a number, which a nodeSelector can't hold.
	daemonSets, err := renderDaemonSets(t, `additionalDaemonSets: {gpu: {node: {nodeSelector: {gpu: true, zone: 1}}}}`)
	assert.NoError(t, err)
	for _, name := range []string{"s3-csi-node-gpu", "s3-csi-mounter-gpu"} {
		assert.Equals(t, map[string]string{"kubernetes.io/os": "linux", "gpu": "true", "zone": "1"}, daemonSets[name].Spec.Template.Spec.NodeSelector)
	}
}

// The two default node.affinity rules: not Fargate, hybrid or Auto Mode, or Auto Mode outside the system NodePool.
const (
	notSpecial    = "eks.amazonaws.com/compute-type NotIn fargate,hybrid,auto"
	autoNotSystem = "eks.amazonaws.com/compute-type In auto AND karpenter.sh/nodepool NotIn system"
)

// terms returns each required node term of ds as its matchExpressions joined by AND.
func terms(ds appsv1.DaemonSet) []string {
	var out []string
	for _, term := range ds.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		var exprs []string
		for _, e := range term.MatchExpressions {
			exprs = append(exprs, e.Key+" "+string(e.Operator)+" "+strings.Join(e.Values, ","))
		}
		out = append(out, strings.Join(exprs, " AND "))
	}
	return out
}

// renderDaemonSets renders the chart in daemonset mode with values, a values file's YAML, and returns its DaemonSets by name.
func renderDaemonSets(t *testing.T, values string) (map[string]appsv1.DaemonSet, error) {
	t.Helper()
	manifests, err := renderChart(t, values, daemonsetMode)
	if err != nil {
		return nil, err
	}
	daemonSets := map[string]appsv1.DaemonSet{}
	for _, file := range []string{"node.yaml", "mounter-daemonset.yaml"} {
		for _, doc := range strings.Split(manifests[templatePath(file)], "\n---") {
			var ds appsv1.DaemonSet
			assert.NoError(t, yaml.Unmarshal([]byte(doc), &ds))
			if ds.Kind == "DaemonSet" {
				daemonSets[ds.Name] = ds
			}
		}
	}
	return daemonSets, nil
}
