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

// offEntries keeps the default pair off the entries' nodes, which the chart requires with any entry.
const offEntries = "node: {nodeSelector: {pool: default}}\n"

func TestAdditionalDaemonSetsRejectInvalidValues(t *testing.T) {
	for _, testCase := range []struct {
		name                string
		values              string
		expectedErrContains string
	}{
		{"limit that isn't a number", `mounter: {maxVolumesPerNode: "abc"}`, "mounter.maxVolumesPerNode must be a whole number >= 0"},
		{"decimal limit", `mounter: {maxVolumesPerNode: 4.5}`, "mounter.maxVolumesPerNode must be a whole number >= 0"},
		{"negative limit on an entry", offEntries + `additionalDaemonSets: {b: {node: {nodeSelector: {x: "1"}}, mounter: {maxVolumesPerNode: -1}}}`, "additionalDaemonSets.b.mounter.maxVolumesPerNode must be a whole number >= 0"},
		{"a name with an uppercase letter", `additionalDaemonSets: {BigMem: {node: {nodeSelector: {x: "1"}}}}`, `name "BigMem" must be lowercase letters`},
		{"a name with an underscore", `additionalDaemonSets: {big_mem: {node: {nodeSelector: {x: "1"}}}}`, `name "big_mem" must be lowercase letters`},
		{"a name ending in '-'", `additionalDaemonSets: {big-: {node: {nodeSelector: {x: "1"}}}}`, `name "big-" must be lowercase letters`},
		{"a key outside node and mounter", `additionalDaemonSets: {b: {maxVolumesPerNode: 4, node: {nodeSelector: {x: "1"}}}}`, "additionalDaemonSets.b.maxVolumesPerNode: an entry only takes node and mounter"},
		{"no placement", `additionalDaemonSets: {b: {mounter: {maxVolumesPerNode: 8}}}`, "additionalDaemonSets.b.node: nodeSelector or affinity is required"},
		{"same placement as another entry", `additionalDaemonSets: {a: {node: {nodeSelector: {x: "1"}}}, b: {node: {nodeSelector: {x: "1"}}}}`, "additionalDaemonSets.b.node selects the same nodes as the top-level node or another entry"},
		{"same placement, as a number and as a string", `additionalDaemonSets: {a: {node: {nodeSelector: {x: 1}}}, b: {node: {nodeSelector: {x: "1"}}}}`, "additionalDaemonSets.b.node selects the same nodes as the top-level node or another entry"},
		{"same placement as the default", `{node: {affinity: {nodeAffinity: {requiredDuringSchedulingIgnoredDuringExecution: {nodeSelectorTerms: [{matchExpressions: [{key: x, operator: In, values: ["1"]}]}]}}}}, additionalDaemonSets: {b: {node: {affinity: {nodeAffinity: {requiredDuringSchedulingIgnoredDuringExecution: {nodeSelectorTerms: [{matchExpressions: [{key: x, operator: In, values: ["1"]}]}]}}}}}}}`, "additionalDaemonSets.b.node selects the same nodes as the top-level node or another entry"},
		{"an entry with no default placement", `{node: {affinity: null}, additionalDaemonSets: {b: {node: {nodeSelector: {x: "1"}}}}}`, "additionalDaemonSets needs the default s3-csi-node and s3-csi-mounter kept off the entries' nodes"},
		// Also checks the chart's copy of the default node.affinity still matches values.yaml.
		{"an entry with the default placement unchanged", `additionalDaemonSets: {b: {node: {nodeSelector: {x: "1"}}}}`, "additionalDaemonSets needs the default s3-csi-node and s3-csi-mounter kept off the entries' nodes"},
		{"an entry service account", `additionalDaemonSets: {b: {node: {nodeSelector: {x: "1"}, serviceAccount: {name: b}}}}`, "additionalDaemonSets.b.node.serviceAccount: set it on node instead"},
		{"an entry preventPodSchedulingIfMissing", `additionalDaemonSets: {b: {node: {nodeSelector: {x: "1"}, preventPodSchedulingIfMissing: true}}}`, "additionalDaemonSets.b.node.preventPodSchedulingIfMissing: set it on node instead"},
		{"invalid entry mounter", offEntries + `additionalDaemonSets: {b: {node: {nodeSelector: {x: "1"}}, mounter: {memoryLimitStrategy: halves}}}`, "additionalDaemonSets.b.mounter: memoryLimitStrategy must be one of"},
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
mounter: {maxVolumesPerNode: 6, resources: {requests: {memory: 6Gi}}, podAnnotations: {a: mounter}, logLevel: 6}
additionalDaemonSets:
  bigmem:
    node: {nodeSelector: {pool: bigmem}, podLabels: {cost: ml}, podAnnotations: {b: bigmem}, logLevel: 7, tolerateAllTaints: false, tolerations: [{key: gpu, operator: Exists}],
      kubeletPath: /var/lib/k0s/kubelet, resources: {requests: {cpu: "1"}}}
    mounter: {maxVolumesPerNode: 32, resources: {requests: {memory: 32Gi}}, podAnnotations: {a: bigmem}, podLabels: {x: bigmem}, imagePullSecrets: [bigmem], logLevel: 2}
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
	assert.Equals(t, true, slices.Contains(daemonSets["s3-csi-node-cached"].Spec.Template.Spec.Containers[0].Args, "--v=5"))
	assert.Equals(t, true, slices.Contains(daemonSets["s3-csi-node-bigmem"].Spec.Template.Spec.Containers[0].Args, "--v=7"))
	assert.Equals(t, "/var/lib/k0s/kubelet", envValue(daemonSets["s3-csi-node-bigmem"], "s3-plugin", "HOST_KUBELET_PATH"))
	assert.Equals(t, "1", daemonSets["s3-csi-node-bigmem"].Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().String())
	assert.Equals(t, true, slices.Contains(daemonSets["s3-csi-mounter-cached"].Spec.Template.Spec.Containers[0].Args, "--v=6"))
	assert.Equals(t, true, slices.Contains(daemonSets["s3-csi-mounter-bigmem"].Spec.Template.Spec.Containers[0].Args, "--v=2"))
	assert.Equals(t, "bigmem", daemonSets["s3-csi-mounter-bigmem"].Spec.Template.Labels["x"])
	assert.Equals(t, []corev1.LocalObjectReference{{Name: "bigmem"}}, daemonSets["s3-csi-mounter-bigmem"].Spec.Template.Spec.ImagePullSecrets)
	// Both bigmem pods get its gpu toleration, and not the tolerate-all one, since it sets tolerateAllTaints: false.
	for _, name := range []string{"s3-csi-node-bigmem", "s3-csi-mounter-bigmem"} {
		tolerations := daemonSets[name].Spec.Template.Spec.Tolerations
		assert.Equals(t, true, slices.ContainsFunc(tolerations, func(t corev1.Toleration) bool { return t.Key == "gpu" }))
		assert.Equals(t, false, slices.ContainsFunc(tolerations, func(t corev1.Toleration) bool {
			return t.Key == "" && t.Operator == corev1.TolerationOpExists && t.Effect == ""
		}))
	}
	assert.Equals(t, "32", daemonSets["s3-csi-mounter-bigmem"].Spec.Template.Labels["s3.csi.aws.com/max-volumes-per-node"])
	assert.Equals(t, "32Gi", daemonSets["s3-csi-mounter-bigmem"].Spec.Template.Spec.Containers[0].Resources.Requests.Memory().String())
	assert.Equals(t, "6", daemonSets["s3-csi-mounter-cached"].Spec.Template.Labels["s3.csi.aws.com/max-volumes-per-node"])
	assert.Equals(t, "6Gi", daemonSets["s3-csi-mounter-cached"].Spec.Template.Spec.Containers[0].Resources.Requests.Memory().String())

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

func TestAdditionalDaemonSetsCache(t *testing.T) {
	daemonSets, err := renderDaemonSets(t, offEntries+`mounter: {cache: {limitStrategy: equalSplit, emptyDir: {sizeLimit: 8Gi}}}
additionalDaemonSets:
  copied: {node: {nodeSelector: {pool: copied}}}
  ebs: {node: {nodeSelector: {pool: ebs}}, mounter: {cache: {limitStrategy: none, ephemeral: {storageClassName: gp3, resourceRequests: 100Gi}}}}
  none: {node: {nodeSelector: {pool: none}}, mounter: {cache: null}}`)
	assert.NoError(t, err)

	// An entry copies the default cache, unless it sets its own cache or cache: null.
	for name, want := range map[string]string{"s3-csi-mounter-copied": "emptyDir", "s3-csi-mounter-ebs": "ephemeral", "s3-csi-mounter-none": ""} {
		ds, ok := daemonSets[name]
		assert.Equals(t, true, ok)
		got := ""
		for _, volume := range ds.Spec.Template.Spec.Volumes {
			if volume.Name == "cache" && volume.EmptyDir != nil {
				got = "emptyDir"
			} else if volume.Name == "cache" && volume.Ephemeral != nil {
				got = "ephemeral"
			}
		}
		assert.Equals(t, want, got)
	}
}

func TestAdditionalDaemonSetsNulls(t *testing.T) {
	// An entry set to null renders no pair, and mounter.cache: null renders no cache.
	daemonSets, err := renderDaemonSets(t, offEntries+`mounter: {cache: null}
additionalDaemonSets: {big: {node: {nodeSelector: {p: big}}}, gone: null}`)
	assert.NoError(t, err)
	assert.Equals(t, 4, len(daemonSets))
	for _, volume := range daemonSets["s3-csi-mounter"].Spec.Template.Spec.Volumes {
		assert.Equals(t, false, volume.Name == "cache")
	}
}

func TestMounterLogLevel(t *testing.T) {
	// logLevel: null falls back to 4, not an empty --v=, and 0 stays 0.
	for values, want := range map[string]string{
		`mounter: {logLevel: null}`: "--v=4",
		`mounter: {logLevel: 0}`:    "--v=0",
		offEntries + `additionalDaemonSets: {b: {node: {nodeSelector: {x: "1"}}, mounter: {logLevel: null}}}`: "--v=4",
	} {
		daemonSets, err := renderDaemonSets(t, values)
		assert.NoError(t, err)
		for name, ds := range daemonSets {
			if strings.HasPrefix(name, "s3-csi-mounter") {
				assert.Equals(t, true, slices.Contains(ds.Spec.Template.Spec.Containers[0].Args, want))
			}
		}
	}
}

func TestPodModeIgnoresAdditionalDaemonSets(t *testing.T) {
	manifests, err := renderChart(t, `additionalDaemonSets: {b: {node: {nodeSelector: {x: "1"}}}}`, map[string]any{"experimental": map[string]any{"mounterMode": "pod"}})
	assert.NoError(t, err)
	assert.Equals(t, 1, strings.Count(manifests["aws-mountpoint-s3-csi-driver/templates/node.yaml"], "kind: DaemonSet"))
	assert.Equals(t, "", strings.TrimSpace(manifests["aws-mountpoint-s3-csi-driver/templates/mounter-daemonset.yaml"]))
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

func TestAdditionalDaemonSetsFallbackVolumeLimit(t *testing.T) {
	daemonSets, err := renderDaemonSets(t, offEntries+`additionalDaemonSets:
  big: {node: {nodeSelector: {pool: big}}, mounter: {maxVolumesPerNode: 32, memoryLimitStrategy: none}}
  unbounded: {node: {nodeSelector: {pool: free}}, mounter: {maxVolumesPerNode: 0, memoryLimitStrategy: none}}`)
	assert.NoError(t, err)
	// Until its mounter runs, each pair's CSI node advertises its own pair's limit, where 0 means none.
	for name, want := range map[string]string{"s3-csi-node": "4", "s3-csi-node-big": "32", "s3-csi-node-unbounded": "0"} {
		assert.Equals(t, want, envValue(daemonSets[name], "s3-plugin", "FALLBACK_MAX_VOLUMES_PER_NODE"))
	}
}

func TestAdditionalDaemonSetsQuoteNodeSelectorValues(t *testing.T) {
	// Each value must render as a quoted string: true, numbers, the key on, which YAML reads as a bool, and null,
	// which becomes "".
	values := offEntries + `additionalDaemonSets: {gpu: {node: {nodeSelector: {gpu: true, zone: 1, memory: 10000000, "on": "x", empty: null}}}}`
	daemonSets, err := renderDaemonSets(t, values)
	assert.NoError(t, err)
	for _, name := range []string{"s3-csi-node-gpu", "s3-csi-mounter-gpu"} {
		assert.Equals(t, map[string]string{"kubernetes.io/os": "linux", "gpu": "true", "zone": "1", "memory": "10000000", "on": "x", "empty": ""}, daemonSets[name].Spec.Template.Spec.NodeSelector)
	}
	// The parse above turns an unquoted true into "true" too, so also check the rendered text.
	manifests, err := renderChart(t, values, map[string]any{"experimental": map[string]any{"mounterMode": "daemonset"}})
	assert.NoError(t, err)
	for _, file := range []string{"node.yaml", "mounter-daemonset.yaml"} {
		manifest := manifests["aws-mountpoint-s3-csi-driver/templates/"+file]
		for _, want := range []string{`"gpu": "true"`, `"zone": "1"`, `"memory": "10000000"`, `"on": "x"`, `"empty": ""`} {
			assert.Contains(t, manifest, want)
		}
	}
}

func TestAdditionalDaemonSetsAffinityOnlyEntry(t *testing.T) {
	// Neither pair has a nodeSelector, so only the affinity makes the two placements different.
	daemonSets, err := renderDaemonSets(t, `node: {affinity: {nodeAffinity: {requiredDuringSchedulingIgnoredDuringExecution: {nodeSelectorTerms: [{matchExpressions: [{key: pool, operator: NotIn, values: [big]}]}]}}}}
additionalDaemonSets:
  big: {node: {affinity: {nodeAffinity: {requiredDuringSchedulingIgnoredDuringExecution: {nodeSelectorTerms: [{matchExpressions: [{key: pool, operator: In, values: [big]}]}]}}}}}`)
	assert.NoError(t, err)
	for _, name := range []string{"s3-csi-node-big", "s3-csi-mounter-big"} {
		assert.Equals(t, []string{"pool In big"}, terms(daemonSets[name]))
	}
}

func TestAdditionalDaemonSetsPairPodsShareScheduling(t *testing.T) {
	daemonSets, err := renderDaemonSets(t, offEntries+`additionalDaemonSets:
  defaults: {node: {nodeSelector: {p: defaults}, tolerateAllTaints: false}}
  gpu: {node: {nodeSelector: {p: gpu}, tolerateAllTaints: false, defaultTolerations: false, tolerations: [{key: gpu, operator: Exists}]}}`)
	assert.NoError(t, err)
	// Both pods of a pair run on the same nodes, so they need the same placement and tolerations.
	for _, pair := range []string{"", "-defaults", "-gpu"} {
		node, mounter := daemonSets["s3-csi-node"+pair].Spec.Template.Spec, daemonSets["s3-csi-mounter"+pair].Spec.Template.Spec
		assert.Equals(t, node.NodeSelector, mounter.NodeSelector)
		assert.Equals(t, node.Affinity, mounter.Affinity)
		assert.Equals(t, node.Tolerations, mounter.Tolerations)
		assert.Equals(t, node.PriorityClassName, mounter.PriorityClassName)
	}
	catchAll := corev1.Toleration{Operator: corev1.TolerationOpExists}
	assert.Equals(t, []corev1.Toleration{catchAll}, daemonSets["s3-csi-node"].Spec.Template.Spec.Tolerations)
	assert.Equals(t, false, slices.Contains(daemonSets["s3-csi-node-defaults"].Spec.Template.Spec.Tolerations, catchAll))
	assert.Equals(t, []corev1.Toleration{{Key: "gpu", Operator: corev1.TolerationOpExists}}, daemonSets["s3-csi-node-gpu"].Spec.Template.Spec.Tolerations)
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

// envValue returns the value of env var name in ds's container, or "" if it has none.
func envValue(ds appsv1.DaemonSet, container, name string) string {
	for _, c := range ds.Spec.Template.Spec.Containers {
		if c.Name != container {
			continue
		}
		for _, env := range c.Env {
			if env.Name == name {
				return env.Value
			}
		}
	}
	return ""
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
