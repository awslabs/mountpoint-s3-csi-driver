package helmchart

import (
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

const entry = `maxVolumesPerNode: 4, memoryLimitStrategy: equalSplit, resources: {requests: {memory: 4Gi}}, logLevel: 4`

func TestMounterEntriesRejectInvalidValues(t *testing.T) {
	for _, testCase := range []struct {
		name                string
		values              string
		expectedErrContains string
	}{
		{"no entries", `daemonsetMounters: []`, "daemonsetMounters must have at least one entry"},
		{"negative limit on a later entry", `daemonsetMounters: [{nodeSelector: {x: "1"}, ` + entry + `}, {name: b, nodeSelector: {x: "2"}, maxVolumesPerNode: -1, memoryLimitStrategy: none, logLevel: 4}]`, "maxVolumesPerNode must be a whole number >= 0"},
		{"limit that isn't a number", `daemonsetMounters: [{maxVolumesPerNode: "abc", memoryLimitStrategy: none, logLevel: 4}]`, "maxVolumesPerNode must be a whole number >= 0"},
		{"decimal limit", `daemonsetMounters: [{maxVolumesPerNode: 4.5, memoryLimitStrategy: none, logLevel: 4}]`, "maxVolumesPerNode must be a whole number >= 0"},
		{"invalid name", `daemonsetMounters: [{name: Big_Mem, nodeSelector: {x: "1"}, ` + entry + `}]`, `name "Big_Mem" must be lowercase letters`},
		{"repeated name", `daemonsetMounters: [{name: a, nodeSelector: {x: "1"}, ` + entry + `}, {name: a, nodeSelector: {x: "2"}, ` + entry + `}]`, `two entries are named "a"`},
		{"two unnamed", `daemonsetMounters: [{nodeSelector: {x: "1"}, ` + entry + `}, {nodeSelector: {x: "2"}, ` + entry + `}]`, "at most one entry may leave out name"},
		{"no selector with two entries", `daemonsetMounters: [{nodeSelector: {x: "1"}, ` + entry + `}, {name: b, ` + entry + `}]`, "nodeSelector or matchExpressions is required"},
		{"same selection", `daemonsetMounters: [{nodeSelector: {x: "1"}, ` + entry + `}, {name: b, nodeSelector: {x: "1"}, ` + entry + `}]`, "two entries select the same nodes"},
		{"key in node.nodeSelector and an entry", `{node: {nodeSelector: {x: "1"}}, daemonsetMounters: [{nodeSelector: {x: "2"}, ` + entry + `}]}`, `node.nodeSelector both set "x"`},
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

func TestMounterEntriesPlacement(t *testing.T) {
	daemonSets, err := renderDaemonSets(t, `node: {nodeSelector: {team: data}}
daemonsetMounters:
  - {name: general, nodeSelector: {pool: general}, matchExpressions: [{key: mem, operator: Lt, values: ["65536"]}], `+entry+`}
  - {name: bigmem, matchExpressions: [{key: mem, operator: Gt, values: ["65535"]}], maxVolumesPerNode: 32, memoryLimitStrategy: none, logLevel: 4}
  - {name: small, nodeSelector: {pool: small}, `+entry+`}`)
	assert.NoError(t, err)

	// The node pod runs wherever any entry's mounter runs: each default rule with each entry.
	node := daemonSets["s3-csi-node"]
	assert.Equals(t, []string{
		notSpecial + " AND pool In general AND mem Lt 65536",
		notSpecial + " AND mem Gt 65535",
		notSpecial + " AND pool In small",
		autoNotSystem + " AND pool In general AND mem Lt 65536",
		autoNotSystem + " AND mem Gt 65535",
		autoNotSystem + " AND pool In small",
	}, terms(node))
	assert.Equals(t, map[string]string{"kubernetes.io/os": "linux", "team": "data"}, node.Spec.Template.Spec.NodeSelector)

	general := daemonSets["s3-csi-daemonset-mounter-general"]
	assert.Equals(t, map[string]string{"kubernetes.io/os": "linux", "team": "data", "pool": "general"}, general.Spec.Template.Spec.NodeSelector)
	assert.Equals(t, []string{notSpecial + " AND mem Lt 65536", autoNotSystem + " AND mem Lt 65536"}, terms(general))
	assert.Equals(t, "4", general.Spec.Template.Labels["s3.csi.aws.com/max-volumes-per-node"])

	bigmem := daemonSets["s3-csi-daemonset-mounter-bigmem"]
	assert.Equals(t, []string{notSpecial + " AND mem Gt 65535", autoNotSystem + " AND mem Gt 65535"}, terms(bigmem))
	assert.Equals(t, "32", bigmem.Spec.Template.Labels["s3.csi.aws.com/max-volumes-per-node"])

	// With only a nodeSelector, the mounter keeps the default rules as they are.
	small := daemonSets["s3-csi-daemonset-mounter-small"]
	assert.Equals(t, map[string]string{"kubernetes.io/os": "linux", "team": "data", "pool": "small"}, small.Spec.Template.Spec.NodeSelector)
	assert.Equals(t, []string{notSpecial, autoNotSystem}, terms(small))
}

func TestMounterEntriesKeepOtherAffinityRules(t *testing.T) {
	const entries = `daemonsetMounters:
  - {name: small, nodeSelector: {pool: small}, ` + entry + `}
  - {name: bigmem, matchExpressions: [{key: mem, operator: Gt, values: ["65535"]}], maxVolumesPerNode: 32, memoryLimitStrategy: none, logLevel: 4}`

	t.Run("keeps matchFields, preferred rules and pod affinity", func(t *testing.T) {
		const affinity = `{nodeAffinity: {requiredDuringSchedulingIgnoredDuringExecution: {nodeSelectorTerms: [{matchExpressions: [{key: a, operator: In, values: ["1"]}], matchFields: [{key: metadata.name, operator: NotIn, values: [n1]}]}]},
  preferredDuringSchedulingIgnoredDuringExecution: [{weight: 1, preference: {matchExpressions: [{key: p, operator: Exists}]}}]},
  podAntiAffinity: {requiredDuringSchedulingIgnoredDuringExecution: [{labelSelector: {matchLabels: {app: x}}, topologyKey: kubernetes.io/hostname}]}}`
		var want corev1.Affinity
		assert.NoError(t, yaml.Unmarshal([]byte(affinity), &want))
		daemonSets, err := renderDaemonSets(t, "node: {affinity: "+affinity+"}\n"+entries)
		assert.NoError(t, err)

		node := daemonSets["s3-csi-node"].Spec.Template.Spec.Affinity
		assert.Equals(t, []string{"a In 1 AND pool In small", "a In 1 AND mem Gt 65535"}, terms(daemonSets["s3-csi-node"]))
		for _, term := range node.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
			assert.Equals(t, want.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchFields, term.MatchFields)
		}
		assert.Equals(t, want.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution, node.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution)
		assert.Equals(t, want.PodAntiAffinity, node.PodAntiAffinity)
		assert.Equals(t, &want, daemonSets["s3-csi-daemonset-mounter-small"].Spec.Template.Spec.Affinity)
	})

	// null, not {}, since Helm merges {} into the default rules.
	t.Run("adds a rule per entry with no node.affinity", func(t *testing.T) {
		daemonSets, err := renderDaemonSets(t, "node: {affinity: null}\n"+entries)
		assert.NoError(t, err)
		assert.Equals(t, []string{"pool In small", "mem Gt 65535"}, terms(daemonSets["s3-csi-node"]))
		assert.Equals(t, (*corev1.Affinity)(nil), daemonSets["s3-csi-daemonset-mounter-small"].Spec.Template.Spec.Affinity)
		assert.Equals(t, []string{"mem Gt 65535"}, terms(daemonSets["s3-csi-daemonset-mounter-bigmem"]))
	})
}

func TestMounterEntriesDefaultInstall(t *testing.T) {
	daemonSets, err := renderDaemonSets(t, `{}`)
	assert.NoError(t, err)
	assert.Equals(t, 2, len(daemonSets))
	for _, name := range []string{"s3-csi-node", "s3-csi-daemonset-mounter"} {
		assert.Equals(t, map[string]string{"kubernetes.io/os": "linux"}, daemonSets[name].Spec.Template.Spec.NodeSelector)
		assert.Equals(t, []string{notSpecial, autoNotSystem}, terms(daemonSets[name]))
	}
	assert.Equals(t, "4", daemonSets["s3-csi-daemonset-mounter"].Spec.Template.Labels["s3.csi.aws.com/max-volumes-per-node"])
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
	ch, err := loader.Load("../../../charts/aws-mountpoint-s3-csi-driver")
	assert.NoError(t, err)
	userValues, err := chartutil.ReadValues([]byte(values))
	assert.NoError(t, err)
	userValues["experimental"] = map[string]any{"mounterMode": "daemonset"}
	userValues["unsupportedDevInstall"] = true
	renderValues, err := chartutil.ToRenderValues(ch, userValues, chartutil.ReleaseOptions{Name: "s3-csi", Namespace: "kube-system"}, chartutil.DefaultCapabilities)
	assert.NoError(t, err)
	manifests, err := engine.Render(ch, renderValues)
	if err != nil {
		return nil, err
	}
	daemonSets := map[string]appsv1.DaemonSet{}
	for _, file := range []string{"node.yaml", "mounter-daemonset.yaml"} {
		for _, doc := range strings.Split(manifests[ch.Name()+"/templates/"+file], "\n---") {
			var ds appsv1.DaemonSet
			assert.NoError(t, yaml.Unmarshal([]byte(doc), &ds))
			if ds.Kind == "DaemonSet" {
				daemonSets[ds.Name] = ds
			}
		}
	}
	return daemonSets, nil
}
