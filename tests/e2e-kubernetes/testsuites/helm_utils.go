package custom_testsuites

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
)

// Helm helpers for Serial e2e suites that upgrade the driver's release to other values and restore the installed ones.

// TODO: the cache suite and this file borrow these from other files. The ones outside util.go could
// move into util.go, or here for the Helm ones, in a future refactor.
//   - pod_sharing_daemonset.go: assertPodFailsToMount, isPodReady
//   - process_isolation_daemonset.go: statPath, commDirHostPath
//   - cache.go: createEBSCacheSC, ebsCSIDriverDaemonSet, deleteObjectFromS3
//   - credentials.go: contextWithVolumeAttributes
//   - upgrade_and_rollback.go: initHelmClient, helmChartSource, helmChartName, helmReleaseNamespace
//   - taint_removal.go: mounterDaemonSetName
//   - util.go: createVolumeResourceWithMountOptions, bucketNameFromVolumeResource, createPod, createPodWithoutWaiting,
//     checkWriteToPathSucceed, checkReadFromPathSucceed, isDaemonsetMounterMode, awsConfig,
//     execInMounterPod

const (
	// e2eUpgradedKey marks a release an upgrade changed (the chart ignores unknown keys). It alone tells "the install
	// chose these values" from "a previous run's restore never ran", otherwise identical and both healthy.
	e2eUpgradedKey = "e2eUpgraded"

	// mounterRestartTimeout covers provisioning, attaching and mounting an ephemeral cache; an unbound one blocks 2m3s per attempt.
	mounterRestartTimeout = 6 * time.Minute
	// csiNodeRestartTimeout only covers pod start: the mounter it waits for is already Ready.
	csiNodeRestartTimeout = 2 * time.Minute
)

// driverRelease is the driver's installed Helm release, which a Serial suite upgrades in place and then restores.
type driverRelease struct {
	helm  *action.Configuration
	name  string
	chart *chart.Chart
	// installed is the release's user-supplied values: what every upgrade must resend (CI's image overrides).
	installed map[string]any
	// revision is the release's revision at setup, which the restore rolls back to, chart and values alike.
	revision int
}

// setUpDriverRelease reads the driver's release and registers its restore. Call it from a BeforeAll in an Ordered, Serial container.
func setUpDriverRelease(ctx context.Context, f *framework.Framework) *driverRelease {
	GinkgoHelper()
	_, helmCfg := initHelmClient()
	ch, err := loader.Load(helmChartSource)
	framework.ExpectNoError(err, "loading the working-tree chart at %s", helmChartSource)
	r := &driverRelease{helm: helmCfg, name: csiDriverReleaseName(helmCfg), chart: ch}
	// User-supplied only, so chart defaults are not written back as user overrides.
	r.installed, err = action.NewGetValues(helmCfg).Run(r.name)
	framework.ExpectNoError(err, "reading the user-supplied values of release %q", r.name)
	installedRelease, err := action.NewGet(helmCfg).Run(r.name)
	framework.ExpectNoError(err, "reading release %q", r.name)
	r.revision = installedRelease.Version

	// go test's -timeout, or a killed run, exits without the restore; restoring the values read here would then report success.
	if upgraded, _ := r.installed[e2eUpgradedKey].(bool); upgraded {
		// A plain `helm rollback` goes to the previous revision, which an earlier upgrade of the same run may also have marked.
		Fail(fmt.Sprintf("release %q still carries a previous run's values, so its restore did not run. Recover with:"+
			" helm rollback %s <the newest revision whose `helm get values --revision` has no %s> -n %s,"+
			" then kubectl delete pod -n %s -l %s", r.name, r.name, e2eUpgradedKey, helmReleaseNamespace,
			csiDriverDaemonSetNamespace, mounterPodLabel))
	}

	cleanupF := teardownSafeFramework(f)
	DeferCleanup(func(ctx context.Context) {
		By(fmt.Sprintf("Rolling the driver's release back to revision %d", r.revision))
		rollback := action.NewRollback(r.helm)
		rollback.Version = r.revision
		framework.ExpectNoError(rollback.Run(r.name), "rolling release %q back to revision %d", r.name, r.revision)
		restartDriverPods(ctx, cleanupF)
	})
	return r
}

// csiDriverReleaseName finds the installed release of this chart. Not helmReleaseName: CI installs that (scripts/run.sh) but
// dev/mp-dev.sh installs `aws-mountpoint-s3-csi-driver`, and hardcoding either fails in the other.
func csiDriverReleaseName(cfg *action.Configuration) string {
	GinkgoHelper()
	list := action.NewList(cfg)
	// All states: a run killed mid-upgrade leaves the release pending-upgrade, which the default list hides.
	list.StateMask = action.ListAll
	releases, err := list.Run()
	framework.ExpectNoError(err, "listing Helm releases")
	var names []string
	for _, r := range releases {
		if r.Chart != nil && r.Chart.Metadata != nil && r.Chart.Metadata.Name == helmChartName {
			names = append(names, r.Name)
		}
	}
	if len(names) != 1 {
		Fail(fmt.Sprintf("expected exactly one installed %q release, found %v", helmChartName, names))
	}
	return names[0]
}

// teardownSafeFramework returns a framework a BeforeAll DeferCleanup can still use: those run *after* the framework's AfterEach
// nils f.ClientSet, so closing over f panics. Every helper these cleanups call uses only ClientSet.
func teardownSafeFramework(f *framework.Framework) *framework.Framework {
	return &framework.Framework{ClientSet: f.ClientSet}
}

// upgrade upgrades the release to values, the complete user-supplied set, and waits until the driver runs them.
func (r *driverRelease) upgrade(ctx context.Context, f *framework.Framework, values map[string]any) {
	GinkgoHelper()
	values = maps.Clone(values)
	values[e2eUpgradedKey] = true
	r.upgradeAndRestartDriverPods(ctx, f, values)
}

// upgradeAndRestartDriverPods upgrades the release to values and replaces the driver pods so they run them.
func (r *driverRelease) upgradeAndRestartDriverPods(ctx context.Context, f *framework.Framework, values map[string]any) {
	GinkgoHelper()
	up := action.NewUpgrade(r.helm)
	up.Namespace = helmReleaseNamespace
	// values is complete; with an empty set Helm would otherwise reuse the release's current values.
	up.ResetValues = true
	// Not Wait: Helm would wait for every mounter pod to be updated, which an OnDelete DaemonSet never does by itself.
	_, err := up.RunWithContext(ctx, r.name, r.chart, values)
	framework.ExpectNoError(err, "upgrading release %q", r.name)
	restartDriverPods(ctx, f)
}

// restartDriverPods replaces the driver pods so they run the release as it is now.
func restartDriverPods(ctx context.Context, f *framework.Framework) {
	GinkgoHelper()
	// The mounter is OnDelete, so a release change alone changes none of its pods.
	By("Restarting the mounter pods, then the s3-csi-node pods")
	deletePods(ctx, f, mounterPodLabel)
	waitForDaemonSetReady(ctx, f, mounterDaemonSetName, mounterPodLabel, mounterRestartTimeout)
	// s3-csi-node exits when it finds no mounter, and the kubelet backs off its restarts for up to five minutes.
	deletePods(ctx, f, csiNodePodLabel)
	waitForDaemonSetReady(ctx, f, csiDriverDaemonSetName, csiNodePodLabel, csiNodeRestartTimeout)
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
		if err != nil || int32(len(pods.Items)) != desired {
			return false, err
		}
		for i := range pods.Items {
			if pods.Items[i].Status.Phase != v1.PodRunning || !isPodReady(&pods.Items[i]) {
				return false, nil
			}
		}
		return true, nil
	}).WithTimeout(timeout).WithPolling(5*time.Second).Should(BeTrue(), func() string { return describePods(ctx, f, label) })
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
