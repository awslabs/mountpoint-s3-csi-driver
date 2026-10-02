package custom_testsuites

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
)

// Helm helpers for e2e suites: reconfigure the mounter DaemonSet, wait for its replacement pods, and restore the release.

// TODO: the cache suite and this file borrow these from other files. The ones outside util.go could
// move into util.go, or here for the Helm ones, in a future refactor.
//   - pod_sharing_daemonset.go: createPodsOnSameNode, createPodOnNode, deletePodsInOrder, isPodReady,
//     execInPodWithNamespace, assertPodFailsToMount, assertPodIOFails, killMounterPodOnNode,
//     killCSINodePodOnNode, waitForCSINodePodReady, waitForCSINodePodStable
//   - cache.go: createEBSCacheSC, ebsCSIDriverDaemonSet, deleteObjectFromS3
//   - credentials.go: contextWithVolumeAttributes
//   - upgrade_and_rollback.go: initHelmClient, waitForCSIDriverDaemonSetRollout
//   - util.go: createVolumeResourceWithMountOptions, bucketNameFromVolumeResource, createPodWithoutWaiting,
//     checkExecInPodSucceed, checkWriteToPathSucceed, checkReadFromPathSucceed,
//     csiDriverDaemonSet, isDaemonsetMounterMode

// The two mounter values these suites swap, as the chart spells them; cacheLimitStrategy lives inside the cache block.
const (
	cacheValuesKey        = "cache"
	cacheLimitStrategyKey = "cacheLimitStrategy"

	// mounterE2EAppliedKey marks the release as holding this suite's configuration (the chart ignores unknown mounter keys). It alone
	// tells "the install chose a tmpfs cache" from "a previous run's restore never ran", otherwise identical and both healthy.
	mounterE2EAppliedKey = "e2eApplied"

	// strategyFromCacheBlock sets no strategy of its own, so the one inside the cache block passed to apply is used.
	strategyFromCacheBlock = ""
)

const (
	// mounterInstalledCleanlyTimeout only absorbs a pod the installer's own restart is still replacing.
	mounterInstalledCleanlyTimeout = 30 * time.Second

	// csiNodeSettleAfterMounterRestart is how long s3-csi-node gets to notice the new mounter itself: longer than its readiness
	// probe period, short enough that the deletion path below costs less than the kubelet's restart backoff would.
	csiNodeSettleAfterMounterRestart = 30 * time.Second
	csiNodeRecoveryTimeout           = 2 * time.Minute

	// mounterRestartTimeout covers provisioning, attaching and mounting an ephemeral cache; an unbound one blocks 2m3s per attempt.
	mounterRestartTimeout = 6 * time.Minute
)

// setUpMounterReconfiguration prepares a suite to change the mounter's values and registers the restore.
// Call it from a BeforeAll in an Ordered, Serial container.
func setUpMounterReconfiguration(ctx context.Context, f *framework.Framework) *mounterValuesConfigurator {
	GinkgoHelper()
	if !isDaemonsetMounterMode(ctx, f) {
		Skip("the cache volume is configured on the mounter DaemonSet, which only exists in daemonset mode")
	}

	// A precondition, not a wait: it catches a mounter left mid-rollout, one of the shapes a lost restore leaves behind.
	waitForMounterDaemonSetReady(ctx, f, mounterInstalledCleanlyTimeout)

	mounters := newMounterValuesConfigurator(ctx, f)

	// Ginkgo drops this suite's DeferCleanup when the last spec in the Ordered tree is skipped by a nested BeforeAll (group.run
	// never reaches attemptSpec, the only place cleanup runs). The values just read would then be the restore payload, so later
	// runs would restore a configuration nobody installed and report success; readiness cannot tell, as it usually works.
	if applied, _ := mounters.baseMounter[mounterE2EAppliedKey].(bool); applied {
		Fail(fmt.Sprintf("release %q still carries this suite's own mounter configuration, so a previous"+
			" run's restore did not run and the values read here are not the install's."+
			" Recover with: helm rollback %s -n %s", mounters.release, mounters.release, helmReleaseNamespace))
	}

	cleanupF := teardownSafeFramework(f)
	DeferCleanup(func(ctx context.Context) { mounters.restore(ctx, cleanupF) })
	return mounters
}

// teardownSafeFramework returns a framework a BeforeAll DeferCleanup can still use: those run *after* the framework's AfterEach
// nils f.ClientSet, so closing over f panics. Every helper these cleanups call uses only ClientSet.
func teardownSafeFramework(f *framework.Framework) *framework.Framework {
	return &framework.Framework{ClientSet: f.ClientSet}
}

// mounterValuesConfigurator changes the mounter DaemonSet's Helm values and puts them back. It holds the release, the
// working-tree chart, and the install's values, which every upgrade must resend complete.
type mounterValuesConfigurator struct {
	helm        *action.Configuration
	release     string
	chart       *chart.Chart
	baseValues  map[string]any
	baseMounter map[string]any
	// nodeImage guards the values round-trip: an incomplete values set would silently drop the CI install's image overrides.
	nodeImage string
}

func newMounterValuesConfigurator(ctx context.Context, f *framework.Framework) *mounterValuesConfigurator {
	GinkgoHelper()
	_, helmCfg := initHelmClient()
	ch, err := loader.Load(helmChartSource)
	framework.ExpectNoError(err, "loading the working-tree chart at %s", helmChartSource)

	release := csiDriverReleaseName(helmCfg)
	baseValues := currentReleaseValues(helmCfg, release)
	return &mounterValuesConfigurator{
		helm:        helmCfg,
		release:     release,
		chart:       ch,
		baseValues:  baseValues,
		baseMounter: mounterElement(ch, baseValues),
		nodeImage:   csiDriverNodeImage(ctx, f),
	}
}

// apply swaps the mounter's cache block for cache (nil removes it), sets its strategy unless strategyFromCacheBlock and its
// maxVolumesPerNode unless 0, runs the driver on nodeSelector's nodes (nil: every node), and waits
// until the running pods carry them. The upgrade alone changes no pod: the DaemonSet is OnDelete, so it reports Ready with the
// old pods, and Helm's readiness check short-circuits for a non-RollingUpdate DaemonSet without ever observing them.
func (c *mounterValuesConfigurator) apply(ctx context.Context, f *framework.Framework, cache map[string]any, strategy string,
	maxVolumesPerNode int, nodeSelector map[string]string) {
	GinkgoHelper()
	vals := withMounterCache(c.baseValues, c.baseMounter, cache, strategy, maxVolumesPerNode)
	withDriverPlacement(vals, c.chart.Values, nodeSelector)
	// Stamped here, not in withMounterCache, so restore (rebuilt from baseMounter) clears the marker by construction.
	vals["daemonsetMounters"].([]any)[0].(map[string]any)[mounterE2EAppliedKey] = true
	upgradeMounterValues(ctx, c.helm, c.release, c.chart, vals)
	framework.ExpectNoError(waitForCSIDriverDaemonSetRollout(ctx, f), "waiting for the node DaemonSet")
	restartMounterPodsAndWait(ctx, f)

	Expect(csiDriverNodeImage(ctx, f)).To(Equal(c.nodeImage),
		"the Helm upgrade changed the driver image, so the values round-trip lost an override")
	assertMounterCacheVolume(ctx, f, cache)
}

// restore puts back the release's installed cache block and strategy. It repeats apply's steps so helmRestored is set between
// the upgrade and the restart, and so a failed image or volume assertion cannot mask a successful restore.
func (c *mounterValuesConfigurator) restore(ctx context.Context, f *framework.Framework) {
	By("Restoring the cache configuration the release was installed with")

	// Only the Helm state matters to later specs; if only the pod restart fails, a plain pod delete recovers the cluster.
	helmRestored := false
	defer func() {
		if r := recover(); r != nil {
			recovery := fmt.Sprintf("kubectl delete pod -n %s -l %s", csiDriverDaemonSetNamespace, mounterDaemonSetLabel)
			if !helmRestored {
				recovery = fmt.Sprintf("helm upgrade %s -n %s %s --reuse-values && %s",
					c.release, helmReleaseNamespace, helmChartSource, recovery)
			}
			fmt.Printf("::error file=helm_utils.go::Failed to restore the mounter DaemonSet's"+
				" cache configuration. Later Serial specs may fail with \"no cache volume\". Recover with: %s\n", recovery)
			panic(r)
		}
	}()

	// Sent complete with the install's own cache: baseValues verbatim would write back a partial element a previous run left.
	// The strategy lives inside the cache block, so restoring the block restores it too.
	original, _ := c.baseMounter[cacheValuesKey].(map[string]any)
	upgradeMounterValues(ctx, c.helm, c.release, c.chart,
		withMounterCache(c.baseValues, c.baseMounter, original, strategyFromCacheBlock, 0))
	helmRestored = true
	framework.ExpectNoError(waitForCSIDriverDaemonSetRollout(ctx, f), "waiting for the node DaemonSet")
	restartMounterPodsAndWait(ctx, f)
}

// csiDriverReleaseName finds the installed release of this chart. Not helmReleaseName: CI installs that (scripts/run.sh) but
// dev/mp-dev.sh installs `aws-mountpoint-s3-csi-driver`, and hardcoding either fails in the other.
func csiDriverReleaseName(cfg *action.Configuration) string {
	GinkgoHelper()
	list := action.NewList(cfg)
	list.All = true
	list.SetStateMask()
	releases, err := list.Run()
	framework.ExpectNoError(err, "listing Helm releases")

	var names []string
	for _, r := range releases {
		if r.Chart != nil && r.Chart.Metadata != nil && r.Chart.Metadata.Name == helmChartName {
			names = append(names, r.Name)
		}
	}
	if len(names) != 1 {
		Fail(fmt.Sprintf("expected exactly one installed %q release, found %v."+
			" This suite upgrades the release in place and cannot guess which one to touch.", helmChartName, names))
	}
	framework.Logf("Found the driver's Helm release: %s", names[0])
	return names[0]
}

// currentReleaseValues returns the release's user-supplied values: the overrides an in-place upgrade must resend (CI's image
// and service-account ones) and the restore payload. AllValues is off so chart defaults are not written back as user overrides.
func currentReleaseValues(cfg *action.Configuration, release string) map[string]any {
	GinkgoHelper()
	get := action.NewGetValues(cfg)
	get.AllValues = false
	vals, err := get.Run(release)
	framework.ExpectNoError(err, "reading the user-supplied values of release %q", release)
	return vals
}

// mounterElement returns a complete daemonsetMounters[0]: the chart's default with any user-supplied element overlaid. It starts
// from the chart, not the coalesced values, because Helm *replaces* lists: once a partial element is in the release, the default
// is unreachable and each run strips it further (an absent logLevel renders `--v=` and the container exits 2). This self-heals.
func mounterElement(ch *chart.Chart, userVals map[string]any) map[string]any {
	element := deepCopyMap(singleMounterElement(ch.Values, "the chart's values.yaml"))
	if user, ok := userVals["daemonsetMounters"]; ok {
		for k, v := range singleMounterElement(map[string]any{"daemonsetMounters": user}, "the release's user-supplied values") {
			element[k] = deepCopyValue(v)
		}
	}
	return element
}

func singleMounterElement(vals map[string]any, source string) map[string]any {
	mounters, ok := vals["daemonsetMounters"].([]any)
	if !ok || len(mounters) != 1 {
		Fail(fmt.Sprintf("expected exactly one daemonsetMounters entry in %s, got %#v."+
			" The install shape changed, so this suite's values assumptions are void.", source, vals["daemonsetMounters"]))
	}
	element, ok := mounters[0].(map[string]any)
	if !ok {
		Fail(fmt.Sprintf("daemonsetMounters[0] in %s is not a map: %#v", source, mounters[0]))
	}
	return element
}

// withMounterCache returns a deep copy of userVals with daemonsetMounters set to one complete element, its cache block replaced
// by cache (nil removes it) and its strategy set unless strategyFromCacheBlock. Always complete, restore included: Helm replaces
// a whole list element, and a partial one poisons later runs.
func withMounterCache(userVals, element, cache map[string]any, strategy string, maxVolumesPerNode int) map[string]any {
	// A nil element sends only `cache` and the chart renders the rest empty: an absent logLevel becomes `--v=` and the mounter exits 2.
	if len(element) == 0 {
		Fail("withMounterCache was given an empty mounter element; mounterElement must run first")
	}
	if cache == nil && strategy != strategyFromCacheBlock {
		Fail("withMounterCache was given a strategy with no cache block; the chart reads the strategy from inside it")
	}
	out := deepCopyMap(userVals)
	mounter := deepCopyMap(element)
	if cache == nil {
		delete(mounter, cacheValuesKey)
	} else {
		// Copied before the strategy goes in, because the callers share one cache block between specs.
		cache = deepCopyMap(cache)
		if strategy != strategyFromCacheBlock {
			cache[cacheLimitStrategyKey] = strategy
		}
		mounter[cacheValuesKey] = cache
	}
	if maxVolumesPerNode > 0 {
		mounter["maxVolumesPerNode"] = maxVolumesPerNode
	}
	out["daemonsetMounters"] = []any{mounter}
	return out
}

// withDriverPlacement sets both driver DaemonSets' placement in vals to the chart's defaults narrowed to nodeSelector, so no row
// inherits an install's pinning: unpinned workloads would land where s3-csi-node is absent, or exits for want of a mounter.
func withDriverPlacement(vals, chartVals map[string]any, nodeSelector map[string]string) {
	node, _ := vals["node"].(map[string]any)
	if node == nil {
		node = map[string]any{}
		vals["node"] = node
	}
	chartNode, _ := chartVals["node"].(map[string]any)
	node["affinity"] = deepCopyValue(chartNode["affinity"])
	// Both DaemonSets read node.nodeSelector, so this one key keeps s3-csi-node and the mounter on the same nodes.
	selector := map[string]any{}
	for k, v := range nodeSelector {
		selector[k] = v
	}
	node["nodeSelector"] = selector

	mounter := vals["daemonsetMounters"].([]any)[0].(map[string]any)
	mounter["affinity"] = deepCopyValue(singleMounterElement(chartVals, "the chart's values.yaml")["affinity"])
}

func deepCopyMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = deepCopyValue(v)
	}
	return out
}

func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return deepCopyMap(t)
	case []any:
		s := make([]any, len(t))
		for i := range t {
			s[i] = deepCopyValue(t[i])
		}
		return s
	default:
		return v
	}
}

// upgradeMounterValues upgrades the release in place. No PostRenderer: unlike the upgrade suite, this must not alter token expiry.
//
// Wait is off because Helm's readiness checks reports mounter DaemonSet as `InProgress, Updated: 0/2` and waits
// for every pod to be updated, which doesn't happen until the mounter pods are deleted (OnDelete).
func upgradeMounterValues(ctx context.Context, cfg *action.Configuration, release string, ch *chart.Chart, vals map[string]any) {
	GinkgoHelper()
	up := action.NewUpgrade(cfg)
	up.Namespace = helmReleaseNamespace
	up.Wait = false
	up.Timeout = 2 * time.Minute // the API calls only, since nothing is waited for
	up.ReuseValues = false       // vals is already the complete user-supplied set

	_, err := up.RunWithContext(ctx, release, ch, vals)
	framework.ExpectNoError(err, "upgrading release %q to change the mounter's values", release)
}

// restartMounterPodsAndWait deletes every mounter pod and waits until both DaemonSets are Ready, as the OnDelete mounter needs after
// any change. Cluster-wide, because the upgrade changed the spec on every node and the restore must be complete.
// TODO can better integrate with utils killCSIDriverPods and the 2 kill functions in pod sharing daemonset test.
func restartMounterPodsAndWait(ctx context.Context, f *framework.Framework) {
	GinkgoHelper()
	pods, err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: mounterDaemonSetLabel,
	})
	framework.ExpectNoError(err, "listing mounter pods to restart them")
	for i := range pods.Items {
		framework.Logf("Deleting mounter pod %s on node %s", pods.Items[i].Name, pods.Items[i].Spec.NodeName)
		framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, &pods.Items[i]))
	}

	waitForMounterDaemonSetReady(ctx, f, mounterRestartTimeout)
	waitForCSINodeDaemonSetReady(ctx, f)
}

// waitForCSINodeDaemonSetReady brings every s3-csi-node pod back to Ready after the mounter pods were replaced, deleting the ones
// that gave up: s3-csi-node exits when the mounter's comm directory does not appear in time, and the kubelet's up-to-five-minute
// restart backoff would outlast a spec's budget. Deleting the pod clears the backoff at once.
//
// Note: waitForCSIDriverDaemonSetRollout is not enough on its own, and the callers' use of it before
// this point is not what makes the node driver ready: only deleting a wedged pod clears the backoff.
func waitForCSINodeDaemonSetReady(ctx context.Context, f *framework.Framework) {
	GinkgoHelper()
	notReady := func(ctx context.Context) ([]string, error) { return notReadyCSINodePods(ctx, f) }

	err := framework.Gomega().Eventually(ctx, notReady).
		WithTimeout(csiNodeSettleAfterMounterRestart).WithPolling(5 * time.Second).Should(BeEmpty())
	if err == nil {
		return
	}

	stuck, err := notReadyCSINodePods(ctx, f)
	framework.ExpectNoError(err, "listing s3-csi-node pods")
	for _, name := range stuck {
		framework.Logf("Deleting s3-csi-node pod %s to clear its restart backoff", name)
		if err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).
			Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			framework.ExpectNoError(err, "deleting s3-csi-node pod %s", name)
		}
	}

	Eventually(ctx, notReady).WithTimeout(csiNodeRecoveryTimeout).WithPolling(5*time.Second).
		Should(BeEmpty(), "s3-csi-node did not come back after its pods were deleted")
}

// notReadyCSINodePods names the s3-csi-node pods not Running and Ready, so a failure lists them rather than a bare timeout.
func notReadyCSINodePods(ctx context.Context, f *framework.Framework) ([]string, error) {
	pods, err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=" + csiDriverDaemonSetName,
	})
	if err != nil {
		return nil, err
	}
	var names []string
	for i := range pods.Items {
		if pods.Items[i].Status.Phase != v1.PodRunning || !isPodReady(&pods.Items[i]) {
			names = append(names, pods.Items[i].Name)
		}
	}
	return names, nil
}

// waitForMounterDaemonSetReady waits until every mounter pod is Running and Ready at the current generation; NumberReady alone
// can be satisfied by a stale-generation read before the deletions are observed.
func waitForMounterDaemonSetReady(ctx context.Context, f *framework.Framework, timeout time.Duration) {
	GinkgoHelper()
	Eventually(ctx, func(ctx context.Context) (bool, error) {
		ds, err := f.ClientSet.AppsV1().DaemonSets(csiDriverDaemonSetNamespace).
			Get(ctx, mounterDaemonSetName, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		if ds.Status.ObservedGeneration != ds.Generation || ds.Status.DesiredNumberScheduled == 0 ||
			ds.Status.NumberReady != ds.Status.DesiredNumberScheduled {
			return false, nil
		}

		pods, err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: mounterDaemonSetLabel,
		})
		if err != nil || int32(len(pods.Items)) != ds.Status.DesiredNumberScheduled {
			return false, nil
		}
		for i := range pods.Items {
			if pods.Items[i].Status.Phase != v1.PodRunning || !isPodReady(&pods.Items[i]) {
				return false, nil
			}
		}
		return true, nil
	}).WithTimeout(timeout).WithPolling(5*time.Second).Should(BeTrue(), func() string { return dumpMounterPods(ctx, f) })
}

// dumpMounterPods reports why the mounter pods are not ready, so a rejected rendered flag (exit code, args) is distinguishable
// from a claim that never bound (PVC phase) rather than both being a bare timeout.
func dumpMounterPods(ctx context.Context, f *framework.Framework) string {
	var b strings.Builder
	b.WriteString("the mounter DaemonSet did not become ready:\n")

	pods, err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: mounterDaemonSetLabel,
	})
	if err != nil {
		fmt.Fprintf(&b, "  could not list mounter pods: %v\n", err)
		return b.String()
	}

	for i := range pods.Items {
		pod := &pods.Items[i]
		fmt.Fprintf(&b, "  pod %s on %s: phase=%s\n", pod.Name, pod.Spec.NodeName, pod.Status.Phase)
		for _, cs := range pod.Status.ContainerStatuses {
			fmt.Fprintf(&b, "    container %s: ready=%v restarts=%d state=%+v lastState=%+v\n",
				cs.Name, cs.Ready, cs.RestartCount, cs.State, cs.LastTerminationState)
		}
		if len(pod.Spec.Containers) > 0 {
			fmt.Fprintf(&b, "    args: %v\n", pod.Spec.Containers[0].Args)
		}
		// An ephemeral cache blocks the pod until its claim binds, so its phase names the cause.
		if pvc, err := f.ClientSet.CoreV1().PersistentVolumeClaims(pod.Namespace).
			Get(ctx, pod.Name+"-"+cacheVolumeNameInPodSpec, metav1.GetOptions{}); err == nil {
			fmt.Fprintf(&b, "    cache PVC %s: phase=%s\n", pvc.Name, pvc.Status.Phase)
		}
	}
	return b.String()
}

func csiDriverNodeImage(ctx context.Context, f *framework.Framework) string {
	return csiDriverDaemonSet(ctx, f).Spec.Template.Spec.Containers[0].Image
}

// assertMounterCacheVolume checks the mounter DaemonSet renders the cache volume asked for, so a bad reconfiguration fails here.
// It reads the pod template, not a live pod: just after a restart a terminating pod can still carry the previous spec.
func assertMounterCacheVolume(ctx context.Context, f *framework.Framework, cache map[string]any) {
	GinkgoHelper()
	ds, err := f.ClientSet.AppsV1().DaemonSets(csiDriverDaemonSetNamespace).
		Get(ctx, mounterDaemonSetName, metav1.GetOptions{})
	framework.ExpectNoError(err, "reading the mounter DaemonSet")

	var found *v1.Volume
	for i := range ds.Spec.Template.Spec.Volumes {
		if ds.Spec.Template.Spec.Volumes[i].Name == cacheVolumeNameInPodSpec {
			found = &ds.Spec.Template.Spec.Volumes[i]
			break
		}
	}

	if cache == nil {
		Expect(found).To(BeNil(), "expected no cache volume on the mounter DaemonSet")
		return
	}
	Expect(found).NotTo(BeNil(), "expected a cache volume on the mounter DaemonSet")

	// The values block names its cache type by which sub-block it sets, as the chart reads it.
	if ed, ok := cache["emptyDir"].(map[string]any); ok {
		Expect(found.EmptyDir).NotTo(BeNil(), "expected an emptyDir cache volume")
		Expect(string(found.EmptyDir.Medium)).To(Equal(fmt.Sprint(ed["medium"])))
	} else {
		Expect(found.Ephemeral).NotTo(BeNil(), "expected an ephemeral cache volume")
	}
}
