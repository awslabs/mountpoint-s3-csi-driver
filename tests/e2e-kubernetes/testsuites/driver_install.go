package custom_testsuites

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"helm.sh/helm/v3/pkg/chart/loader"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
)

// Helpers for Serial e2e suites that reconfigure the installed driver, a Helm release or the EKS add-on, with other values
// and restore the installed ones.

// TODO: the cache suite and the driver_install files borrow these from other files. The ones outside util.go could
// move into util.go, or into driver_install_helm.go for the Helm ones, in a future refactor.
//   - pod_sharing_daemonset.go: assertPodFailsToMount, isPodReady
//   - process_isolation_daemonset.go: statPath, commDirHostPath, mountpointProcessRunningAs
//   - cache.go: createEBSCacheSC, ebsCSIDriverDaemonSet, deleteObjectFromS3
//   - credentials.go: contextWithVolumeAttributes, ClusterName
//   - upgrade_and_rollback.go: initHelmClient, helmChartSource, helmChartName, helmReleaseNamespace
//   - taint_removal.go: mounterDaemonSetName
//   - util.go: createVolumeResourceWithMountOptions, bucketNameFromVolumeResource, createPod, createPodWithoutWaiting,
//     checkWriteToPathSucceed, checkReadFromPathSucceed, isDaemonsetMounterMode, awsConfig,
//     execInMounterPod, podOnNode

const (
	// mounterRestartTimeout covers provisioning, attaching and mounting an ephemeral cache; an unbound one blocks 2m3s per attempt.
	mounterRestartTimeout = 6 * time.Minute
	// csiNodeRestartTimeout only covers pod start: the mounter it waits for is already Ready.
	csiNodeRestartTimeout = 2 * time.Minute
)

// driverRelease is the installed driver, which a Serial suite reconfigures in place and then restores.
type driverRelease struct {
	install driverInstall
	// affinity is s3-csi-node's at setup, the default pair's placement, which the value builders send or narrow.
	affinity map[string]any
	// installed is the install's user-supplied values: what every upgrade must resend (CI's image overrides).
	installed map[string]any
}

// driverInstall is how the driver is installed, which decides how its values are changed and restored.
type driverInstall interface {
	// read returns the install's user-supplied values at setup.
	read() map[string]any
	// apply makes values, the complete user-supplied set, the install's, without replacing any driver pod.
	apply(ctx context.Context, values map[string]any)
	// settle waits until the install reports apply's values in effect, once any pods they need replaced have been.
	settle(ctx context.Context)
	// restore returns the install to its values at setup and waits until the driver runs them.
	restore(ctx context.Context, f *framework.Framework)
}

// setUpDriverRelease reads the driver's install and registers its restore. Call it from a BeforeAll in an Ordered, Serial container.
func setUpDriverRelease(ctx context.Context, f *framework.Framework) *driverRelease {
	GinkgoHelper()
	cleanupF := teardownSafeFramework(f)
	r := &driverRelease{install: findDriverInstall(ctx, cleanupF), affinity: csiNodeAffinity(ctx, f)}
	r.installed = r.install.read()

	DeferCleanup(func(ctx context.Context) { r.install.restore(ctx, cleanupF) })
	return r
}

// csiNodeAffinity returns s3-csi-node's affinity in the shape of values, as the chart renders node.affinity there as is.
func csiNodeAffinity(ctx context.Context, f *framework.Framework) map[string]any {
	GinkgoHelper()
	ds, err := f.ClientSet.AppsV1().DaemonSets(csiDriverDaemonSetNamespace).Get(ctx, csiDriverDaemonSetName, metav1.GetOptions{})
	framework.ExpectNoError(err, "reading DaemonSet %s", csiDriverDaemonSetName)
	encoded, err := json.Marshal(ds.Spec.Template.Spec.Affinity)
	framework.ExpectNoError(err, "encoding the affinity of DaemonSet %s", csiDriverDaemonSetName)
	affinity := map[string]any{}
	framework.ExpectNoError(json.Unmarshal(encoded, &affinity), "decoding the affinity of DaemonSet %s", csiDriverDaemonSetName)
	return affinity
}

// findDriverInstall returns how the driver is installed: as the chart's one Helm release or, with none, as the EKS add-on.
func findDriverInstall(ctx context.Context, f *framework.Framework) driverInstall {
	GinkgoHelper()
	_, helmCfg := initHelmClient()
	names := csiDriverReleaseNames(helmCfg)
	switch len(names) {
	case 0:
		return newAddonInstall(ctx, f)
	case 1:
		// Only a Helm install needs the chart, which helm upgrade renders. EKS renders the add-on's own.
		ch, err := loader.Load(helmChartSource)
		framework.ExpectNoError(err, "loading the working-tree chart at %s", helmChartSource)
		return newHelmInstall(helmCfg, names[0], ch)
	}
	Fail(fmt.Sprintf("expected at most one installed %q release, found %v", helmChartName, names))
	return nil
}

// teardownSafeFramework returns a framework a BeforeAll DeferCleanup can still use: those run *after* the framework's AfterEach
// nils f.ClientSet, so closing over f panics. Every helper these cleanups call uses only ClientSet.
func teardownSafeFramework(f *framework.Framework) *framework.Framework {
	return &framework.Framework{ClientSet: f.ClientSet}
}

// upgrade makes values, the complete user-supplied set, the install's, and waits until the driver runs them.
func (r *driverRelease) upgrade(ctx context.Context, f *framework.Framework, values map[string]any) {
	GinkgoHelper()
	r.install.apply(ctx, values)
	restartDriverPods(ctx, f)
	r.install.settle(ctx)
}

// upgradeOnly makes values the install's without replacing any driver pod, as a customer's helm upgrade or update-addon does.
func (r *driverRelease) upgradeOnly(ctx context.Context, values map[string]any) {
	GinkgoHelper()
	r.install.apply(ctx, values)
	r.install.settle(ctx)
}

// restartDriverPods replaces the driver pods so they run the install as it is now.
func restartDriverPods(ctx context.Context, f *framework.Framework) {
	GinkgoHelper()
	// The mounter is OnDelete, so a change to the install alone changes none of its pods.
	By("Restarting the mounter pods, then the s3-csi-node pods")
	deletePods(ctx, f, mounterPodLabel)
	for _, name := range daemonSetNames(ctx, f, mounterDaemonSetName) {
		waitForDaemonSetReady(ctx, f, name, mounterPodLabel, mounterRestartTimeout)
	}
	// After the mounters, so each s3-csi-node finds its mounter at startup and registers its limit right away.
	deletePods(ctx, f, csiNodePodLabel)
	for _, name := range daemonSetNames(ctx, f, csiDriverDaemonSetName) {
		waitForDaemonSetReady(ctx, f, name, csiNodePodLabel, csiNodeRestartTimeout)
	}
}

// deletePods deletes the driver pods matching label, waiting for each to go.
func deletePods(ctx context.Context, f *framework.Framework, label string) {
	GinkgoHelper()
	pods, err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{LabelSelector: label})
	framework.ExpectNoError(err, "listing %s pods", label)
	for i := range pods.Items {
		framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, &pods.Items[i]))
	}
}

// waitForDaemonSetReady waits until every pod of the DaemonSet runs its current spec and is Ready. NumberReady alone can be
// satisfied by a stale-generation read before the deletions are observed.
func waitForDaemonSetReady(ctx context.Context, f *framework.Framework, name, label string, timeout time.Duration) {
	GinkgoHelper()
	Eventually(ctx, func(ctx context.Context) (bool, error) {
		ds, err := f.ClientSet.AppsV1().DaemonSets(csiDriverDaemonSetNamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		desired := ds.Status.DesiredNumberScheduled
		if ds.Status.ObservedGeneration != ds.Generation || desired == 0 ||
			ds.Status.UpdatedNumberScheduled != desired || ds.Status.NumberReady != desired {
			return false, nil
		}
		pods, err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{LabelSelector: label})
		if err != nil {
			return false, err
		}
		// Every mounter DaemonSet's pods carry the same label, so count only this one's.
		var own int32
		for i := range pods.Items {
			if !metav1.IsControlledBy(&pods.Items[i], ds) {
				continue
			}
			own++
			if pods.Items[i].Status.Phase != v1.PodRunning || !isPodReady(&pods.Items[i]) {
				return false, nil
			}
		}
		return own == desired, nil
	}).WithTimeout(timeout).WithPolling(5*time.Second).Should(BeTrue(), func() string { return describePods(ctx, f, label) })
}

// daemonSetNames returns the release's DaemonSets of one kind, named prefix: the default pair's, and one per
// additionalDaemonSets entry.
func daemonSetNames(ctx context.Context, f *framework.Framework, prefix string) []string {
	GinkgoHelper()
	list, err := f.ClientSet.AppsV1().DaemonSets(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{})
	framework.ExpectNoError(err, "listing DaemonSets")
	var names []string
	for _, ds := range list.Items {
		if strings.HasPrefix(ds.Name, prefix) {
			names = append(names, ds.Name)
		}
	}
	return names
}

// describePods says why pods are not ready, so a rejected rendered flag (exit code, args) is told apart from a claim that
// never bound (PVC phase) rather than both being a bare timeout.
func describePods(ctx context.Context, f *framework.Framework, label string) string {
	pods, err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{LabelSelector: label})
	if err != nil {
		return fmt.Sprintf("listing %s pods: %v", label, err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s pods:\n", label)
	for _, pod := range pods.Items {
		fmt.Fprintf(&b, "  pod %s on %s: phase=%s args=%v\n", pod.Name, pod.Spec.NodeName, pod.Status.Phase, pod.Spec.Containers[0].Args)
		for _, cs := range pod.Status.ContainerStatuses {
			fmt.Fprintf(&b, "    container %s: ready=%v restarts=%d state=%+v lastState=%+v\n",
				cs.Name, cs.Ready, cs.RestartCount, cs.State, cs.LastTerminationState)
		}
		for _, vol := range pod.Spec.Volumes {
			if vol.Ephemeral == nil {
				continue
			}
			// The kubelet names an ephemeral volume's claim <pod>-<volume>.
			if pvc, err := f.ClientSet.CoreV1().PersistentVolumeClaims(pod.Namespace).
				Get(ctx, pod.Name+"-"+vol.Name, metav1.GetOptions{}); err == nil {
				fmt.Fprintf(&b, "    PVC %s: phase=%s\n", pvc.Name, pvc.Status.Phase)
			}
		}
	}
	return b.String()
}
