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
	mounterDaemonSet    = "s3-csi-daemonset-mounter"
)

// renderedWorkload is a workload the chart renders, with the pod template it runs.
type renderedWorkload struct {
	kind, name string
	pod        corev1.PodSpec
}

// renderWorkloads renders the whole chart with values, a values file's YAML, and returns every DaemonSet and
// Deployment in it.
func renderWorkloads(t *testing.T, values string) []renderedWorkload {
	t.Helper()
	ch, err := loader.Load("../../../charts/aws-mountpoint-s3-csi-driver")
	assert.NoError(t, err)
	userValues, err := chartutil.ReadValues([]byte(values))
	assert.NoError(t, err)
	// The chart refuses to render from a source checkout without unsupportedDevInstall.
	userValues["unsupportedDevInstall"] = true
	renderValues, err := chartutil.ToRenderValues(ch, userValues, chartutil.ReleaseOptions{Name: "s3-csi", Namespace: "kube-system"}, chartutil.DefaultCapabilities)
	assert.NoError(t, err)
	manifests, err := engine.Render(ch, renderValues)
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
				workloads = append(workloads, renderedWorkload{"DaemonSet", ds.Name, ds.Spec.Template.Spec})
			case "Deployment":
				var d appsv1.Deployment
				assert.NoError(t, yaml.Unmarshal([]byte(doc), &d))
				workloads = append(workloads, renderedWorkload{"Deployment", d.Name, d.Spec.Template.Spec})
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

// Values rendering the daemonset architecture, without and with a cache volume.
var daemonsetModeValues = map[string]string{
	"without a cache": `
experimental: {mounterMode: daemonset}
daemonsetMounters: [{maxVolumesPerNode: 4, memoryLimitStrategy: none}]`,
	"with a cache": `
experimental: {mounterMode: daemonset}
daemonsetMounters: [{maxVolumesPerNode: 4, memoryLimitStrategy: none, cache: {emptyDir: {sizeLimit: 2Gi}, limitStrategy: equalSplit}}]`,
}

func TestOnlyTheNodePluginIsPrivileged(t *testing.T) {
	cases := map[string]string{"default values": ``}
	for name, values := range daemonsetModeValues {
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

func TestMounterRunsWithMinimalPrivileges(t *testing.T) {
	baseCapabilities := []corev1.Capability{"SETUID", "SETGID", "KILL"}
	wantCapabilities := map[string][]corev1.Capability{
		"without a cache": baseCapabilities,
		"with a cache":    append(slices.Clone(baseCapabilities), "CHOWN", "DAC_OVERRIDE", "FOWNER"),
	}
	for name, values := range daemonsetModeValues {
		t.Run(name, func(t *testing.T) {
			mounter := findWorkload(t, renderWorkloads(t, values), "DaemonSet", mounterDaemonSet)

			// Pod: no node network (IMDS unreachable at hop limit 1), no host PID namespace, no Kubernetes API token.
			assert.Equals(t, false, mounter.pod.HostNetwork)
			assert.Equals(t, false, mounter.pod.HostPID)
			assert.Equals(t, false, mounter.pod.HostIPC)
			if mounter.pod.AutomountServiceAccountToken == nil || *mounter.pod.AutomountServiceAccountToken {
				t.Error("the mounter pod must set automountServiceAccountToken: false")
			}

			containers := allContainers(mounter.pod)
			assert.Equals(t, 1, len(containers))
			sc := containers[0].SecurityContext
			if sc == nil {
				t.Fatal("the mounter container has no securityContext")
			}
			if sc.Privileged != nil && *sc.Privileged {
				t.Error("the mounter container must not be privileged")
			}
			if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
				t.Error("the mounter container must set allowPrivilegeEscalation: false")
			}
			if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
				t.Error("the mounter container must set readOnlyRootFilesystem: true")
			}
			if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
				t.Error("the mounter container must use the RuntimeDefault seccomp profile")
			}
			if sc.Capabilities == nil {
				t.Fatal("the mounter container sets no capabilities")
			}
			assert.Equals(t, []corev1.Capability{"ALL"}, sc.Capabilities.Drop)
			assert.Equals(t, wantCapabilities[name], sc.Capabilities.Add)
		})
	}
}

func TestImagesComeFromTheConfiguredRegistry(t *testing.T) {
	for _, tc := range []struct {
		name, values, wantPrefix string
	}{
		{
			name:       "public ECR by default",
			values:     daemonsetModeValues["with a cache"],
			wantPrefix: "public.ecr.aws/",
		},
		{
			name: "the regional EKS registry for the EKS add-on",
			values: daemonsetModeValues["with a cache"] + `
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
