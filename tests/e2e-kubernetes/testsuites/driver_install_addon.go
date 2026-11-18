package custom_testsuites

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	"sigs.k8s.io/yaml"
)

const (
	// driverAddonName is the EKS add-on that installs the chart, which then has no Helm release in the cluster.
	driverAddonName = "aws-mountpoint-s3-csi-driver"
	// e2eUpgradedTag marks an add-on an upgrade changed, as e2eUpgradedKey does a release: the add-on's schema rejects unknown keys.
	e2eUpgradedTag = "e2e-upgraded"
	// addonApplyTimeout covers EKS applying an update, which took under 10s, or finishing one that changes no DaemonSet.
	addonApplyTimeout = 5 * time.Minute
	// addonSettleTimeout covers EKS seeing the pods of an update healthy, which took up to 3 minutes after they were.
	addonSettleTimeout = 10 * time.Minute
)

// addonInstall is the driver installed as the EKS add-on, which renders the chart with the add-on's configuration values.
type addonInstall struct {
	eks *eks.Client
	// f lists the driver's DaemonSets, in apply too, which upgradeOnly gives no framework.
	f       *framework.Framework
	arn     string
	version string
	// configured is the add-on's configuration values at setup as EKS returns them, "" for none, which the restore sends back.
	configured string
	installed  map[string]any
	// tagged is whether apply has tagged the add-on, which it does before the first update.
	tagged bool
	// update is the ID of the last update.
	update string
}

// newAddonInstall reads the driver's EKS add-on, failing if a previous run's restore did not run.
func newAddonInstall(ctx context.Context, f *framework.Framework) *addonInstall {
	GinkgoHelper()
	client := eks.NewFromConfig(awsConfig(ctx))
	out, err := client.DescribeAddon(ctx, &eks.DescribeAddonInput{ClusterName: &ClusterName, AddonName: aws.String(driverAddonName)})
	framework.ExpectNoError(err, "found no %q Helm release, so describing EKS add-on %q on cluster %q (--cluster-name)",
		helmChartName, driverAddonName, ClusterName)
	a := &addonInstall{eks: client, f: f, arn: aws.ToString(out.Addon.AddonArn), version: aws.ToString(out.Addon.AddonVersion),
		configured: aws.ToString(out.Addon.ConfigurationValues), installed: map[string]any{}}
	// JSON or YAML, as EKS takes either.
	framework.ExpectNoError(yaml.Unmarshal([]byte(a.configured), &a.installed),
		"parsing the add-on's configuration values %q", a.configured)

	if _, ok := out.Addon.Tags[e2eUpgradedTag]; ok {
		Fail(fmt.Sprintf("add-on %q is still tagged %s, so a previous run's restore did not run. Recover with:"+
			" aws eks update-addon --cluster-name %s --addon-name %s --configuration-values <its values before that run, '{}' for none>"+
			" (refused while UPDATING: first kubectl delete pod -n %s -l %s), then kubectl delete pod -n %[5]s -l %[6]s,"+
			" then once describe-addon reports ACTIVE, aws eks untag-resource --resource-arn %[7]s --tag-keys %[2]s",
			driverAddonName, e2eUpgradedTag, ClusterName, driverAddonName, csiDriverDaemonSetNamespace, mounterPodLabel, a.arn))
	}
	return a
}

func (a *addonInstall) read() map[string]any { return a.installed }

func (a *addonInstall) apply(ctx context.Context, values map[string]any) {
	GinkgoHelper()
	// Before the first update, so a run that exits without the restore leaves the tag.
	if !a.tagged {
		_, err := a.eks.TagResource(ctx, &eks.TagResourceInput{ResourceArn: &a.arn, Tags: map[string]string{e2eUpgradedTag: "true"}})
		framework.ExpectNoError(err, "tagging add-on %q %s=true", driverAddonName, e2eUpgradedTag)
		a.tagged = true
	}
	configuration, err := json.Marshal(values)
	framework.ExpectNoError(err, "encoding configuration values %v", values)
	a.updateTo(ctx, string(configuration))
}

// updateTo updates the add-on's configuration values and waits until EKS has applied them, seconds after accepting them:
// a mounter pod replaced before then restarts from its old template, which its OnDelete DaemonSet then never rolls out.
func (a *addonInstall) updateTo(ctx context.Context, configuration string) {
	GinkgoHelper()
	By("Updating the driver's add-on to configuration values " + configuration)
	before, err := driverDaemonSets(ctx, a.f)
	framework.ExpectNoError(err, "listing the driver's DaemonSets")
	out, err := a.eks.UpdateAddon(ctx, &eks.UpdateAddonInput{
		ClusterName: &ClusterName,
		AddonName:   aws.String(driverAddonName),
		// The version read at setup, so only the values change.
		AddonVersion:        &a.version,
		ConfigurationValues: &configuration,
	})
	framework.ExpectNoError(err, "updating add-on %q to configuration values %s", driverAddonName, configuration)
	a.update = aws.ToString(out.Update.Id)
	Eventually(ctx, func(ctx context.Context) (bool, error) {
		// Done already, as an update that needs no pod replaced can be.
		if done, err := a.settled(ctx); done || err != nil {
			return done, err
		}
		// Or a DaemonSet changed, appeared or went: EKS has applied the update, and waits for its pods.
		after, err := driverDaemonSets(ctx, a.f)
		return !maps.Equal(generations(after), generations(before)), err
	}).WithTimeout(addonApplyTimeout).WithPolling(2*time.Second).Should(BeTrue(), func() string { return a.describe(ctx) })
}

// settle waits until the last update is done, which EKS refuses another update until.
func (a *addonInstall) settle(ctx context.Context) {
	GinkgoHelper()
	By("Waiting for the driver's add-on to be ACTIVE")
	Eventually(ctx, a.settled).WithTimeout(addonSettleTimeout).WithPolling(10*time.Second).
		Should(BeTrue(), func() string { return a.describe(ctx) })
}

// settled reports whether the last update is done: Successful, which EKS reports once every pod it changed is Ready, and the
// add-on ACTIVE. It stops an Eventually on an update EKS failed or cancelled.
func (a *addonInstall) settled(ctx context.Context) (bool, error) {
	status, err := a.updateStatus(ctx)
	switch {
	case err != nil || status == ekstypes.UpdateStatusInProgress:
		return false, err
	case status != ekstypes.UpdateStatusSuccessful:
		return false, StopTrying(fmt.Sprintf("update %s of add-on %q is %s", a.update, driverAddonName, status))
	}
	out, err := a.eks.DescribeAddon(ctx, &eks.DescribeAddonInput{ClusterName: &ClusterName, AddonName: aws.String(driverAddonName)})
	if err != nil {
		return false, err
	}
	return out.Addon.Status == ekstypes.AddonStatusActive, nil
}

func (a *addonInstall) updateStatus(ctx context.Context) (ekstypes.UpdateStatus, error) {
	out, err := a.eks.DescribeUpdate(ctx, &eks.DescribeUpdateInput{Name: &ClusterName, AddonName: aws.String(driverAddonName),
		UpdateId: &a.update})
	if err != nil {
		return "", err
	}
	return out.Update.Status, nil
}

// describe says where the add-on and its last update are, with EKS's health issues and update errors, and which DaemonSets
// have pods not running their template, as EKS waits for those.
func (a *addonInstall) describe(ctx context.Context) string {
	var b strings.Builder
	addon, err := a.eks.DescribeAddon(ctx, &eks.DescribeAddonInput{ClusterName: &ClusterName, AddonName: aws.String(driverAddonName)})
	if err != nil {
		fmt.Fprintf(&b, "describing add-on %q: %v\n", driverAddonName, err)
	} else {
		fmt.Fprintf(&b, "add-on %q is %s\n", driverAddonName, addon.Addon.Status)
		if addon.Addon.Health != nil {
			for _, issue := range addon.Addon.Health.Issues {
				fmt.Fprintf(&b, "  health issue %s: %s, resources %v\n", issue.Code, aws.ToString(issue.Message), issue.ResourceIds)
			}
		}
	}
	update, err := a.eks.DescribeUpdate(ctx, &eks.DescribeUpdateInput{Name: &ClusterName, AddonName: aws.String(driverAddonName),
		UpdateId: &a.update})
	if err != nil {
		fmt.Fprintf(&b, "describing update %s: %v\n", a.update, err)
	} else {
		fmt.Fprintf(&b, "update %s is %s\n", a.update, update.Update.Status)
		for _, e := range update.Update.Errors {
			fmt.Fprintf(&b, "  error %s: %s, resources %v\n", e.ErrorCode, aws.ToString(e.ErrorMessage), e.ResourceIds)
		}
	}
	daemonSets, err := driverDaemonSets(ctx, a.f)
	if err != nil {
		fmt.Fprintf(&b, "listing the driver's DaemonSets: %v\n", err)
	}
	for _, ds := range daemonSets {
		if ds.Status.UpdatedNumberScheduled != ds.Status.DesiredNumberScheduled {
			fmt.Fprintf(&b, "DaemonSet %s: %d of %d pods run its template\n", ds.Name, ds.Status.UpdatedNumberScheduled,
				ds.Status.DesiredNumberScheduled)
		}
	}
	return b.String()
}

// restore updates the add-on back to its configuration values at setup, and untags it once the driver runs them.
func (a *addonInstall) restore(ctx context.Context, f *framework.Framework) {
	GinkgoHelper()
	// Untagged, it was never updated.
	if !a.tagged {
		return
	}
	// A failed upgrade can leave its update in progress, and EKS refuses another until the pods it changed are replaced.
	if a.update != "" {
		status, err := a.updateStatus(ctx)
		framework.ExpectNoError(err, "describing update %s of add-on %q", a.update, driverAddonName)
		if status == ekstypes.UpdateStatusInProgress {
			restartDriverPods(ctx, f)
			a.settle(ctx)
		}
	}
	// With none at setup, `{}`: the chart's defaults all the same, though describe-addon then reports `{}`.
	a.updateTo(ctx, cmp.Or(a.configured, "{}"))
	restartDriverPods(ctx, f)
	a.settle(ctx)
	_, err := a.eks.UntagResource(ctx, &eks.UntagResourceInput{ResourceArn: &a.arn, TagKeys: []string{e2eUpgradedTag}})
	framework.ExpectNoError(err, "untagging add-on %q %s", driverAddonName, e2eUpgradedTag)
}

// driverDaemonSets returns the driver's s3-csi-node and mounter DaemonSets: the default pair's, and one of each per
// additionalDaemonSets entry.
func driverDaemonSets(ctx context.Context, f *framework.Framework) ([]appsv1.DaemonSet, error) {
	list, err := f.ClientSet.AppsV1().DaemonSets(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(list.Items, func(ds appsv1.DaemonSet) bool {
		return !strings.HasPrefix(ds.Name, csiDriverDaemonSetName) && !strings.HasPrefix(ds.Name, mounterDaemonSetName)
	}), nil
}

// generations returns each DaemonSet's generation, which every change to its spec increments.
func generations(daemonSets []appsv1.DaemonSet) map[string]int64 {
	out := map[string]int64{}
	for _, ds := range daemonSets {
		out[ds.Name] = ds.Generation
	}
	return out
}
