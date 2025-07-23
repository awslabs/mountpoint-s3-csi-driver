package custom_testsuites

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
)

// This value defines how long the upgrade test should take.
//
// This needs to be at least more than 2 hours because
//  1. We ask for service account tokens that valid for 1 hour (see `CSIDriver` object)
//  2. Session duration of the IAM roles we assume is 1 hour
//
// So, to make sure we hit both of the cycles in the worst case, we want to run our upgrade tests for 2 hours+.
// Therefore we can be sure if the credentials are successfully refreshed after the upgrade.
const UPGRADE_TEST_DURATION_IN_MINUTES = 150

const helmRepo = "https://awslabs.github.io/mountpoint-s3-csi-driver"
const helmChartSource = "../../charts/aws-mountpoint-s3-csi-driver"
const helmChartName = "aws-mountpoint-s3-csi-driver"
const helmReleaseName = "mountpoint-s3-csi-driver"
const helmReleaseNamespace = "kube-system"

var helmChartPreviousVersion = os.Getenv("MOUNTPOINT_CSI_DRIVER_PREVIOUS_VERSION")
var helmChartNewVersion = os.Getenv("MOUNTPOINT_CSI_DRIVER_NEW_VERSION")
var helmChartContainerRepository = os.Getenv("REPOSITORY")
var helmChartContainerTag = os.Getenv("TAG")

type s3CSIUpgradeTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

func InitS3CSIUpgradeTestSuite() storageframework.TestSuite {
	return &s3CSIUpgradeTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name: "upgrade",
			TestPatterns: []storageframework.TestPattern{
				storageframework.DefaultFsPreprovisionedPV,
			},
		},
	}
}

func (t *s3CSIUpgradeTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

func (t *s3CSIUpgradeTestSuite) SkipUnsupportedTests(_ storageframework.TestDriver, pattern storageframework.TestPattern) {
	if pattern.VolType != storageframework.PreprovisionedPV {
		e2eskipper.Skipf("Suite %q does not support %v", t.tsInfo.Name, pattern.VolType)
	}
}

func (t *s3CSIUpgradeTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	f := framework.NewFrameworkWithCustomTimeouts(NamespacePrefix+"upgrade", storageframework.GetDriverTimeouts(driver))
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

	type local struct {
		config *storageframework.PerTestConfig

		// A list of cleanup functions to be called after each test to clean resources created during the test.
		cleanup []func(context.Context) error
	}

	var l local
	var oidcProvider string

	deferCleanup := func(f func(context.Context) error) {
		l.cleanup = append(l.cleanup, f)
	}

	cleanup := func(ctx context.Context) {
		var errs []error
		slices.Reverse(l.cleanup) // clean items in reverse order similar to how `defer` works
		for _, f := range l.cleanup {
			errs = append(errs, f(ctx))
		}
		framework.ExpectNoError(errors.NewAggregate(errs), "while cleanup resource")
	}

	createPod := func(ctx context.Context, serviceAccount string) *v1.Pod {
		vol := createVolumeResourceWithMountOptions(ctx, l.config, pattern, []string{"debug", "debug-crt", "allow-delete"})
		deferCleanup(vol.CleanupResource)

		pod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{vol.Pvc}, admissionapi.LevelBaseline, "")
		pod.Spec.ServiceAccountName = serviceAccount

		pod, err := createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
		framework.ExpectNoError(err)
		deferCleanup(func(ctx context.Context) error { return e2epod.DeletePodWithWait(ctx, f.ClientSet, pod) })

		return pod
	}

	checkBasicFileOperations := func(ctx context.Context, pod *v1.Pod) {
		seed := time.Now().UTC().UnixNano()
		filename := fmt.Sprintf("test-%d.txt", seed)
		path := filepath.Join(e2epod.VolumeMountPath1, filename)
		testWriteSize := 1024 // 1KB

		checkWriteToPath(ctx, f, pod, path, testWriteSize, seed)
		checkReadFromPath(ctx, f, pod, path, testWriteSize, seed)
		checkListingPathWithEntries(ctx, f, pod, e2epod.VolumeMountPath1, []string{filename, "test.txt"})
		checkDeletingPath(ctx, f, pod, path)
		checkListingPathWithEntries(ctx, f, pod, e2epod.VolumeMountPath1, []string{"test.txt"})
	}

	updateCSIDriversServiceAccountRole := func(ctx context.Context, oidcProvider, policyName string) {
		By("Updating CSI Driver's Service Account Role for IRSA")
		sa := csiDriverServiceAccount(ctx, f)

		role, removeRole := createRole(ctx, f, assumeRoleWithWebIdentityPolicyDocument(ctx, oidcProvider, sa), policyName)
		deferCleanup(removeRole)

		sa, restoreServiceAccountRole := overrideServiceAccountRole(ctx, f, sa, *role.Arn)
		deferCleanup(restoreServiceAccountRole)

		waitUntilRoleIsAssumableWithWebIdentity(ctx, f, sa)

		// Trigger recreation of our pods to use the new IAM role
		killCSIDriverPods(ctx, f)
	}

	assignPolicyToServiceAccount := func(ctx context.Context, sa *v1.ServiceAccount, policyName string) *v1.ServiceAccount {
		role, removeRole := createRole(ctx, f, assumeRoleWithWebIdentityPolicyDocument(ctx, oidcProvider, sa), policyName)
		deferCleanup(removeRole)

		sa, _ = overrideServiceAccountRole(ctx, f, sa, *role.Arn)
		waitUntilRoleIsAssumableWithWebIdentity(ctx, f, sa)
		return sa
	}

	createServiceAccountWithPolicy := func(ctx context.Context, policyName string) *v1.ServiceAccount {
		sa, removeSA := createServiceAccount(ctx, f)
		deferCleanup(removeSA)

		return assignPolicyToServiceAccount(ctx, sa, policyName)
	}

	enablePLI := func(ctx context.Context) context.Context {
		return contextWithVolumeAttributes(ctx, map[string]string{"authenticationSource": "pod"})
	}

	BeforeEach(func(ctx context.Context) {
		l = local{}
		l.config = driver.PrepareTest(ctx, f)
		DeferCleanup(cleanup)
	})

	It("Upgrade to a new version from a previous version without interrupting the workloads", func(ctx context.Context) {
		if helmChartPreviousVersion == "" || helmChartNewVersion == "" {
			Fail("Please set the previous and new versions to test using `MOUNTPOINT_CSI_DRIVER_PREVIOUS_VERSION` and `MOUNTPOINT_CSI_DRIVER_NEW_VERSION` environment variables")
		}

		oidcProvider = oidcProviderForCluster(ctx, f)
		if oidcProvider == "" {
			Fail("Please configure OIDC provider for the testing cluster")
		}

		framework.Logf("Testing upgrade from %q to %q...", helmChartPreviousVersion, helmChartNewVersion)

		settings, cfg := initHelmClient()

		// Make sure to start from a clean state
		uninstallCSIDriver(cfg)

		// Install the previously released version from our Helm repo
		chartPath := pullCSIDriver(settings, cfg, helmChartPreviousVersion)
		installCSIDriver(cfg, helmChartPreviousVersion, chartPath)

		// Configure driver-level IRSA with "S3ReadOnlyAccess" policy
		updateCSIDriversServiceAccountRole(ctx, oidcProvider, iamPolicyS3ReadOnlyAccess)
		// Create two SAs for pod-level IRSA with "S3FullAccess" and "S3ReadOnlyAccess" policies
		pliFullAccessSA, pliReadOnlyAccessSA := createServiceAccountWithPolicy(ctx, iamPolicyS3FullAccess), createServiceAccountWithPolicy(ctx, iamPolicyS3ReadOnlyAccess)

		// Create three workloads with different SAs
		dliReadOnlyAccessPod := createPod(ctx, "default")
		pliFullAccessPod := createPod(enablePLI(ctx), pliFullAccessSA.Name)
		pliReadOnlyAccessPod := createPod(enablePLI(ctx), pliReadOnlyAccessSA.Name)

		fullAccessPods, readOnlyAccessPods := []*v1.Pod{pliFullAccessPod}, []*v1.Pod{dliReadOnlyAccessPod, pliReadOnlyAccessPod}

		// Write a sample files to writeable pods
		seed := time.Now().UTC().UnixNano()
		testWriteSize := 1024 // 1KB
		testFile := filepath.Join(e2epod.VolumeMountPath1, "test.txt")
		for _, pod := range fullAccessPods {
			checkWriteToPath(ctx, f, pod, testFile, testWriteSize, seed)
			checkReadFromPath(ctx, f, pod, testFile, testWriteSize, seed)
		}

		// Ensure read-only pods can do listing but fails to write
		for _, pod := range readOnlyAccessPods {
			checkListingPath(ctx, f, pod, e2epod.VolumeMountPath1)
			checkWriteToPathFails(ctx, f, pod, testFile, testWriteSize, seed)
		}

		// Now upgrade it to the new version
		if strings.HasSuffix(helmChartNewVersion, "-source") {
			// If the version ends with `-source`, do a source build
			chartPath = packageHelmChartFromSource(helmChartNewVersion)
		} else {
			// Otherwise just install a released version
			chartPath = pullCSIDriver(settings, cfg, helmChartNewVersion)
		}
		upgradeCSIDriver(cfg, helmChartNewVersion, chartPath)

		// Create new workloads after the upgrade
		dliReadOnlyAccessPodNewVersion := createPod(ctx, "default")
		pliFullAccessPodNewVersion := createPod(enablePLI(ctx), pliFullAccessSA.Name)
		pliReadOnlyAccessPodNewVersion := createPod(enablePLI(ctx), pliReadOnlyAccessSA.Name)
		fullAccessPods = append(fullAccessPods, pliFullAccessPodNewVersion)
		readOnlyAccessPods = append(readOnlyAccessPods, dliReadOnlyAccessPodNewVersion, pliReadOnlyAccessPodNewVersion)
		for _, pod := range []*v1.Pod{pliFullAccessPodNewVersion} {
			checkWriteToPath(ctx, f, pod, testFile, testWriteSize, seed)
			checkReadFromPath(ctx, f, pod, testFile, testWriteSize, seed)
		}
		for _, pod := range readOnlyAccessPods {
			checkListingPath(ctx, f, pod, e2epod.VolumeMountPath1)
			checkWriteToPathFails(ctx, f, pod, testFile, testWriteSize, seed)
		}

		// Ensure the workloads are still healthy
		for range UPGRADE_TEST_DURATION_IN_MINUTES {
			framework.Logf("Checking if workloads are still healthy after the upgrade...")

			// Check full-access pods can still write and read originally created file
			for _, pod := range fullAccessPods {
				// Test reading the existing file
				checkReadFromPath(ctx, f, pod, testFile, testWriteSize, seed)
				// Test basic file operations
				checkBasicFileOperations(ctx, pod)
			}

			// Check read-only pods can still list but fails to write
			for _, pod := range readOnlyAccessPods {
				checkListingPath(ctx, f, pod, e2epod.VolumeMountPath1)
				checkWriteToPathFails(ctx, f, pod, testFile, testWriteSize, seed)
			}

			// Sleep for a minute for the next cycle
			framework.Logf("Sleeping for a minute for the next cycle...")
			time.Sleep(1 * time.Minute)
		}

		// Ensure the workloads can be terminated without any problem
		for _, pod := range slices.Concat(fullAccessPods, readOnlyAccessPods) {
			e2epod.DeletePodWithWait(ctx, f.ClientSet, pod)
		}
	})
}

// packageHelmChartFromSource creates a Helm package from the CSI Driver's source.
func packageHelmChartFromSource(version string) string {
	if helmChartContainerRepository == "" || helmChartContainerTag == "" {
		Fail("Please set container repository and tag using `REPOSITORY` and `TAG` environment variables if you want to test a source build")
	}

	out := GinkgoT().TempDir()

	packageClient := action.NewPackage()
	packageClient.Destination = out
	packageClient.Version = version

	chartPath, err := packageClient.Run(helmChartSource, map[string]any{
		"image": map[string]any{
			"repository": helmChartContainerRepository,
			"tag":        helmChartContainerTag,
		},
	})
	framework.ExpectNoError(err)
	framework.Logf("Packaged Helm chart to %q", chartPath)
	return chartPath
}

// pullCSIDriver pulls a CSI Driver version from the CSI Driver's Helm repository.
func pullCSIDriver(settings *cli.EnvSettings, cfg *action.Configuration, version string) string {
	out := GinkgoT().TempDir()

	pullClient := action.NewPullWithOpts(
		action.WithConfig(cfg))
	pullClient.RepoURL = helmRepo
	pullClient.DestDir = out
	pullClient.Settings = settings
	pullClient.Version = version

	_, err := pullClient.Run(helmChartName)
	framework.ExpectNoError(err)

	chartPath := filepath.Join(out, fmt.Sprintf("%s-%s.tgz", helmChartName, version))
	framework.Logf("Downloaded Helm chart to %q", chartPath)
	return chartPath
}

// installCSIDriver installs the CSI Driver's Helm chart to the testing cluster.
func installCSIDriver(cfg *action.Configuration, version string, chartPath string) {
	installClient := action.NewInstall(cfg)
	installClient.ReleaseName = helmReleaseName
	installClient.Namespace = helmReleaseNamespace
	installClient.Version = version
	installClient.Wait = true
	installClient.Timeout = 30 * time.Second

	chart, err := loader.Load(chartPath)
	framework.ExpectNoError(err)

	release, err := installClient.RunWithContext(context.Background(), chart, map[string]any{})
	framework.ExpectNoError(err)

	framework.Logf("Helm release %q created", release.Name)
}

// upgradeCSIDriver upgrades the CSI Driver's Helm chart to the new version.
func upgradeCSIDriver(cfg *action.Configuration, version string, chartPath string) {
	upgradeClient := action.NewUpgrade(cfg)
	upgradeClient.Namespace = helmReleaseNamespace
	upgradeClient.Version = version
	upgradeClient.Wait = true
	upgradeClient.Timeout = 30 * time.Second

	chart, err := loader.Load(chartPath)
	framework.ExpectNoError(err)

	release, err := upgradeClient.RunWithContext(context.Background(), helmReleaseName, chart, map[string]any{})
	framework.ExpectNoError(err)

	framework.Logf("Helm release %q updated to %v (from %q)", release.Name, version, chartPath)
}

// uninstallCSIDriver uninstalls the CSI Driver's Helm chart from the testing cluster.
func uninstallCSIDriver(cfg *action.Configuration) {
	uninstallClient := action.NewUninstall(cfg)
	uninstallClient.DeletionPropagation = "foreground"
	uninstallClient.Wait = true
	uninstallClient.IgnoreNotFound = true
	uninstallClient.Timeout = 30 * time.Second

	framework.Logf("Uninstalling Helm release %q", helmReleaseName)

	_, err := uninstallClient.Run(helmReleaseName)
	framework.ExpectNoError(err)
}

// initHelmClient initialises Helm client and returns the configuration to use in further operations.
func initHelmClient() (*cli.EnvSettings, *action.Configuration) {
	logger := log.Default()
	settings := cli.New()

	actionConfig := new(action.Configuration)
	err := actionConfig.Init(
		settings.RESTClientGetter(),
		helmReleaseNamespace,
		os.Getenv("HELM_DRIVER"),
		logger.Printf)
	framework.ExpectNoError(err)

	return settings, actionConfig
}
