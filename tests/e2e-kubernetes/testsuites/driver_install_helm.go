package custom_testsuites

import (
	"context"
	"fmt"
	"maps"

	. "github.com/onsi/ginkgo/v2"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"k8s.io/kubernetes/test/e2e/framework"
)

// e2eUpgradedKey marks a release an upgrade changed (the chart ignores unknown keys). It alone tells "the install
// chose these values" from "a previous run's restore never ran", otherwise identical and both healthy.
const e2eUpgradedKey = "e2eUpgraded"

// helmInstall is the driver installed as a Helm release of the chart.
type helmInstall struct {
	helm      *action.Configuration
	name      string
	chart     *chart.Chart
	installed map[string]any
	// revision is the release's revision at setup, which the restore rolls back to, chart and values alike.
	revision int
}

// newHelmInstall reads the release, failing if a previous run's restore did not run.
func newHelmInstall(cfg *action.Configuration, name string, ch *chart.Chart) *helmInstall {
	GinkgoHelper()
	installedRelease, err := action.NewGet(cfg).Run(name)
	framework.ExpectNoError(err, "reading release %q", name)
	// User-supplied only, so chart defaults are not written back as user overrides.
	h := &helmInstall{helm: cfg, name: name, chart: ch, installed: installedRelease.Config, revision: installedRelease.Version}

	// go test's -timeout, or a killed run, exits without the restore; restoring the values read here would then report success.
	if upgraded, _ := h.installed[e2eUpgradedKey].(bool); upgraded {
		// A plain `helm rollback` goes to the previous revision, which an earlier upgrade of the same run may also have marked.
		Fail(fmt.Sprintf("release %q still carries a previous run's values, so its restore did not run. Recover with:"+
			" helm rollback %s <the newest revision whose `helm get values --revision` has no %s> -n %s,"+
			" then kubectl delete pod -n %s -l %s", h.name, h.name, e2eUpgradedKey, helmReleaseNamespace,
			csiDriverDaemonSetNamespace, mounterPodLabel))
	}
	return h
}

func (h *helmInstall) read() map[string]any { return h.installed }

func (h *helmInstall) apply(ctx context.Context, values map[string]any) {
	GinkgoHelper()
	values = maps.Clone(values)
	values[e2eUpgradedKey] = true
	up := action.NewUpgrade(h.helm)
	up.Namespace = helmReleaseNamespace
	// values is complete; with an empty set Helm would otherwise reuse the release's current values.
	up.ResetValues = true
	// Not Wait: Helm would wait for every mounter pod to be updated, which an OnDelete DaemonSet never does by itself.
	_, err := up.RunWithContext(ctx, h.name, h.chart, values)
	framework.ExpectNoError(err, "upgrading release %q", h.name)
}

// settle has nothing to wait for: the upgrade does not Wait, and the release is deployed once it returns.
func (h *helmInstall) settle(context.Context) {}

func (h *helmInstall) restore(ctx context.Context, f *framework.Framework) {
	GinkgoHelper()
	By(fmt.Sprintf("Rolling the driver's release back to revision %d", h.revision))
	rollback := action.NewRollback(h.helm)
	rollback.Version = h.revision
	framework.ExpectNoError(rollback.Run(h.name), "rolling release %q back to revision %d", h.name, h.revision)
	restartDriverPods(ctx, f)
}

// csiDriverReleaseNames finds the installed releases of this chart. Not helmReleaseName: CI installs that (scripts/run.sh) but
// dev/mp-dev.sh installs `aws-mountpoint-s3-csi-driver`, and hardcoding either fails in the other.
func csiDriverReleaseNames(cfg *action.Configuration) []string {
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
	return names
}
