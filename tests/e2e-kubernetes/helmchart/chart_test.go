package helmchart

import (
	"slices"
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

const (
	nodePluginContainer = "s3-plugin"
	mounterDaemonSet    = "s3-csi-mounter"
)

// renderedWorkload is a workload the chart renders, with the pod template it runs.
type renderedWorkload struct {
	kind, name  string
	pod         corev1.PodSpec
	annotations map[string]string
}

// renderChart renders the whole chart with values, a values file's YAML, and returns its manifests by template
// path. The chart refuses to render from a source checkout without unsupportedDevInstall, so that is always set.
func renderChart(t *testing.T, values string) (map[string]string, error) {
	t.Helper()
	ch, err := loader.Load("../../../charts/aws-mountpoint-s3-csi-driver")
	assert.NoError(t, err)
	userValues, err := chartutil.ReadValues([]byte(values))
	assert.NoError(t, err)
	userValues["unsupportedDevInstall"] = true
	renderValues, err := chartutil.ToRenderValues(ch, userValues, chartutil.ReleaseOptions{Name: "s3-csi", Namespace: "kube-system"}, chartutil.DefaultCapabilities)
	assert.NoError(t, err)
	return engine.Render(ch, renderValues)
}

// templatePath is the key renderChart returns a template's manifest under.
func templatePath(file string) string {
	return "aws-mountpoint-s3-csi-driver/templates/" + file
}

// renderWorkloads renders the whole chart with values, a values file's YAML, and returns every DaemonSet and
// Deployment in it.
func renderWorkloads(t *testing.T, values string) []renderedWorkload {
	t.Helper()
	manifests, err := renderChart(t, values)
	assert.NoError(t, err)

	var workloads []renderedWorkload
	for path, manifest := range manifests {
		// Skip NOTES.txt and helpers: only templates are Kubernetes objects.
		if !strings.HasSuffix(path, ".yaml") {
			continue
		}
		for _, doc := range strings.Split(manifest, "\n---") {
			var meta struct {
				Kind string `json:"kind"`
			}
			if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
				t.Fatalf("rendered a document that is not YAML: %v\n%s", err, doc)
			}
			switch meta.Kind {
			case "DaemonSet":
				var ds appsv1.DaemonSet
				assert.NoError(t, yaml.Unmarshal([]byte(doc), &ds))
				workloads = append(workloads, renderedWorkload{"DaemonSet", ds.Name, ds.Spec.Template.Spec, ds.Spec.Template.Annotations})
			case "Deployment":
				var d appsv1.Deployment
				assert.NoError(t, yaml.Unmarshal([]byte(doc), &d))
				workloads = append(workloads, renderedWorkload{"Deployment", d.Name, d.Spec.Template.Spec, d.Spec.Template.Annotations})
			}
		}
	}
	if len(workloads) == 0 {
		t.Fatal("the chart rendered no DaemonSet or Deployment")
	}
	return workloads
}

func allContainers(pod corev1.PodSpec) []corev1.Container {
	return append(slices.Clone(pod.InitContainers), pod.Containers...)
}

func findWorkload(t *testing.T, workloads []renderedWorkload, kind, name string) renderedWorkload {
	t.Helper()
	for _, w := range workloads {
		if w.kind == kind && w.name == name {
			return w
		}
	}
	t.Fatalf("the chart rendered no %s %q", kind, name)
	return renderedWorkload{}
}

// Values rendering the mounter without and with a cache volume.
var cacheValues = map[string]string{
	"without a cache": `
mounter: {maxVolumesPerNode: 4, memoryLimitStrategy: none}`,
	"with a cache": `
mounter: {maxVolumesPerNode: 4, memoryLimitStrategy: none, cache: {emptyDir: {sizeLimit: 2Gi}, limitStrategy: equalSplit}}`,
}

func TestOnlyTheNodePluginIsPrivileged(t *testing.T) {
	cases := map[string]string{"with the default values": ``}
	for name, values := range cacheValues {
		cases[name] = values
	}
	for name, values := range cases {
		t.Run(name, func(t *testing.T) {
			var privileged []string
			for _, w := range renderWorkloads(t, values) {
				for _, c := range allContainers(w.pod) {
					if c.SecurityContext != nil && c.SecurityContext.Privileged != nil && *c.SecurityContext.Privileged {
						privileged = append(privileged, w.kind+"/"+w.name+"/"+c.Name)
					}
				}
			}
			assert.Equals(t, []string{"DaemonSet/s3-csi-node/" + nodePluginContainer}, privileged)
		})
	}
}

func TestImagesComeFromTheConfiguredRegistry(t *testing.T) {
	for _, tc := range []struct {
		name, values, wantPrefix string
	}{
		{
			name:       "public ECR by default",
			values:     cacheValues["with a cache"],
			wantPrefix: "public.ecr.aws/",
		},
		{
			name: "the regional EKS registry for the EKS add-on",
			values: cacheValues["with a cache"] + `
isEKSAddon: true
image: {containerRegistry: 602401143452.dkr.ecr.us-west-2.amazonaws.com}
sidecars:
  nodeDriverRegistrar: {image: {containerRegistry: 602401143452.dkr.ecr.us-west-2.amazonaws.com}}
  livenessProbe: {image: {containerRegistry: 602401143452.dkr.ecr.us-west-2.amazonaws.com}}`,
			wantPrefix: "602401143452.dkr.ecr.us-west-2.amazonaws.com/eks/",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, w := range renderWorkloads(t, tc.values) {
				for _, c := range allContainers(w.pod) {
					if !strings.HasPrefix(c.Image, tc.wantPrefix) {
						t.Errorf("%s/%s/%s uses image %q, not from %q", w.kind, w.name, c.Name, c.Image, tc.wantPrefix)
					}
				}
			}
		})
	}
}

func TestControllerValues(t *testing.T) {
	affinity := &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
		NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{
			{Key: "role", Operator: corev1.NodeSelectorOpIn, Values: []string{"controller"}}}}}}}}
	controller := findWorkload(t, renderWorkloads(t, `controller:
  nodeSelector: {pool: system}
  podAnnotations: {a: b}
  affinity: {nodeAffinity: {requiredDuringSchedulingIgnoredDuringExecution: {nodeSelectorTerms: [{matchExpressions: [{key: role, operator: In, values: [controller]}]}]}}}`),
		"Deployment", "s3-csi-controller")
	assert.Equals(t, map[string]string{"kubernetes.io/os": "linux", "pool": "system"}, controller.pod.NodeSelector)
	assert.Equals(t, affinity, controller.pod.Affinity)
	assert.Equals(t, map[string]string{"a": "b"}, controller.annotations)
}

func TestRootPodAnnotations(t *testing.T) {
	// Every driver pod gets the root annotations, and a pod's own podAnnotations win on the same key.
	workloads := renderWorkloads(t, `podAnnotations: {a: root, b: root}
controller: {podAnnotations: {b: controller}}`)
	for _, w := range workloads {
		want := map[string]string{"a": "root", "b": "root"}
		if w.name == "s3-csi-controller" {
			want["b"] = "controller"
		}
		assert.Equals(t, want, w.annotations)
	}
}
