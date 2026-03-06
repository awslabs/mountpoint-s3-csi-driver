package custom_testsuites

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"

	"path/filepath"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/repo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
	"sigs.k8s.io/yaml"
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
const ROLLBACK_TEST_DURATION_IN_MINUTES = 150

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

	// createTestWorkloads creates workloads with different access levels
	createTestWorkloads := func(ctx context.Context, pliFullAccessSA, pliReadOnlyAccessSA *v1.ServiceAccount) (fullAccessPods, readOnlyAccessPods []*v1.Pod) {
		dliReadOnlyAccessPod := createPod(ctx, "default")
		pliFullAccessPod := createPod(enablePLI(ctx), pliFullAccessSA.Name)
		pliReadOnlyAccessPod := createPod(enablePLI(ctx), pliReadOnlyAccessSA.Name)
		return []*v1.Pod{pliFullAccessPod}, []*v1.Pod{dliReadOnlyAccessPod, pliReadOnlyAccessPod}
	}

	// writeAndVerifyTestFile writes a test file and verifies it can be read
	writeAndVerifyTestFile := func(ctx context.Context, pods []*v1.Pod) (testFile string, testWriteSize int, seed int64) {
		seed = time.Now().UTC().UnixNano()
		testWriteSize = 1024
		testFile = filepath.Join(e2epod.VolumeMountPath1, "test.txt")
		for _, pod := range pods {
			checkWriteToPath(ctx, f, pod, testFile, testWriteSize, seed)
			checkReadFromPath(ctx, f, pod, testFile, testWriteSize, seed)
		}
		return
	}

	// verifyReadOnlyAccess verifies pods can list but not write
	verifyReadOnlyAccess := func(ctx context.Context, pods []*v1.Pod, testFile string, testWriteSize int, seed int64) {
		for _, pod := range pods {
			checkListingPath(ctx, f, pod, e2epod.VolumeMountPath1)
			checkWriteToPathFails(ctx, f, pod, testFile, testWriteSize, seed)
		}
	}

	// setupTestEnvironment prepares the test environment with OIDC and Helm
	setupTestEnvironment := func(ctx context.Context) (*cli.EnvSettings, *action.Configuration) {
		oidcProvider = oidcProviderForCluster(ctx, f)
		if oidcProvider == "" {
			Fail("Please configure OIDC provider for the testing cluster")
		}
		settings, cfg := initHelmClient()
		uninstallCSIDriver(cfg)
		return settings, cfg
	}

	// verifyWorkloadHealth checks if pods can perform expected operations
	verifyWorkloadHealth := func(ctx context.Context, fullAccessPods, readOnlyPods []*v1.Pod, testFile string, testWriteSize int, seed int64) {
		for _, pod := range fullAccessPods {
			checkReadFromPath(ctx, f, pod, testFile, testWriteSize, seed)
			checkBasicFileOperations(ctx, pod)
		}
		for _, pod := range readOnlyPods {
			checkListingPath(ctx, f, pod, e2epod.VolumeMountPath1)
			checkWriteToPathFails(ctx, f, pod, testFile, testWriteSize, seed)
		}
	}

	// runUpgradeTest performs the complete upgrade test workflow
	runUpgradeTest := func(ctx context.Context, fromVersion, toVersion string, useSourceBuild bool) {
		settings, cfg := setupTestEnvironment(ctx)
		framework.Logf("Testing upgrade from %q to %q...", fromVersion, toVersion)

		// Install the previous version
		chartPath := pullCSIDriver(settings, cfg, fromVersion)
		installCSIDriver(cfg, fromVersion, chartPath)

		// Configure driver-level IRSA with "S3ReadOnlyAccess" policy
		updateCSIDriversServiceAccountRole(ctx, oidcProvider, iamPolicyS3ReadOnlyAccess)
		// Create two SAs for pod-level IRSA with "S3FullAccess" and "S3ReadOnlyAccess" policies
		pliFullAccessSA, pliReadOnlyAccessSA := createServiceAccountWithPolicy(ctx, iamPolicyS3FullAccess), createServiceAccountWithPolicy(ctx, iamPolicyS3ReadOnlyAccess)

		// To test both upgrade termination and rollback scenarios, we create 5 sets of workloads:

		// Set		|Created When	| Purpose									|Terminated When
		//__________|_______________|___________________________________________|_________________________
		// Set A	|Before upgrade	| Test pre-upgrade workloads on rollback	|After rollback monitoring
		// Set B	|Before upgrade	| Test upgrade + termination after upgrade	|After upgrade monitoring
		// Set C	|After upgrade	| Test upgrade + termination after upgrade	|After upgrade monitoring
		// Set D	|After upgrade	| Test post-upgrade workloads on rollback	|After rollback monitoring
		// Set E	|After rollback	| Test new workload creation post-rollback	|After rollback monitoring

		// Create Set A + Set B (for upgrade test + rollback test)
		framework.Logf("Creating Set A and Set B workloads before upgrade...")
		fullAccessPodsSetA, readOnlyAccessPodsSetA := createTestWorkloads(ctx, pliFullAccessSA, pliReadOnlyAccessSA)
		fullAccessPodsSetB, readOnlyAccessPodsSetB := createTestWorkloads(ctx, pliFullAccessSA, pliReadOnlyAccessSA)

		// Test Set A workloads
		framework.Logf("Testing Set A workloads...")
		testFile, testWriteSize, seed := writeAndVerifyTestFile(ctx, fullAccessPodsSetA)
		verifyReadOnlyAccess(ctx, readOnlyAccessPodsSetA, testFile, testWriteSize, seed)

		// Test Set B workloads
		framework.Logf("Testing Set B workloads...")
		_, _, _ = writeAndVerifyTestFile(ctx, fullAccessPodsSetB)
		verifyReadOnlyAccess(ctx, readOnlyAccessPodsSetB, testFile, testWriteSize, seed)

		// Declare Set C, D variables
		var fullAccessPodsSetC, fullAccessPodsSetD []*v1.Pod
		var readOnlyAccessPodsSetC, readOnlyAccessPodsSetD []*v1.Pod

		// Run upgrade phase and capture success/failure
		upgradeSucceeded := true
		func() {
			defer func() {
				if r := recover(); r != nil {
					upgradeSucceeded = false
					framework.Logf("Upgrade phase failed with panic: %v", r)
				}
			}()

			// Upgrade to the new version
			if useSourceBuild {
				chartPath = packageHelmChartFromSource(toVersion)
			} else {
				chartPath = pullCSIDriver(settings, cfg, toVersion)
			}
			upgradeCSIDriver(cfg, f, toVersion, chartPath)

			// Create Set C + Set D after the upgrade
			framework.Logf("Creating Set C and Set D workloads after upgrade...")
			fullAccessPodsSetC, readOnlyAccessPodsSetC = createTestWorkloads(ctx, pliFullAccessSA, pliReadOnlyAccessSA)
			fullAccessPodsSetD, readOnlyAccessPodsSetD = createTestWorkloads(ctx, pliFullAccessSA, pliReadOnlyAccessSA)

			// Test Set C workloads
			framework.Logf("Testing Set C workloads...")
			_, _, _ = writeAndVerifyTestFile(ctx, fullAccessPodsSetC)
			verifyReadOnlyAccess(ctx, readOnlyAccessPodsSetC, testFile, testWriteSize, seed)

			// Test Set D workloads
			framework.Logf("Testing Set D workloads...")
			_, _, _ = writeAndVerifyTestFile(ctx, fullAccessPodsSetD)
			verifyReadOnlyAccess(ctx, readOnlyAccessPodsSetD, testFile, testWriteSize, seed)

			// Ensure all workloads are still healthy for 150 minutes
			framework.Logf("Monitoring all 12 workloads (Set A + B + C + D) for %d minutes...", UPGRADE_TEST_DURATION_IN_MINUTES)
			allFullAccessAfterUpgrade := slices.Concat(fullAccessPodsSetA, fullAccessPodsSetB, fullAccessPodsSetC, fullAccessPodsSetD)
			allReadOnlyAfterUpgrade := slices.Concat(readOnlyAccessPodsSetA, readOnlyAccessPodsSetB, readOnlyAccessPodsSetC, readOnlyAccessPodsSetD)
			for range UPGRADE_TEST_DURATION_IN_MINUTES {
				framework.Logf("Checking if workloads are still healthy after the upgrade...")
				verifyWorkloadHealth(ctx, allFullAccessAfterUpgrade, allReadOnlyAfterUpgrade, testFile, testWriteSize, seed)
				framework.Logf("Sleeping for a minute for the next cycle...")
				time.Sleep(1 * time.Minute)
			}

			// Terminate Set B + Set C (test termination after upgrade)
			framework.Logf("Terminating Set B and Set C workloads to test termination after upgrade...")
			for _, pod := range slices.Concat(fullAccessPodsSetB, readOnlyAccessPodsSetB, fullAccessPodsSetC, readOnlyAccessPodsSetC) {
				e2epod.DeletePodWithWait(ctx, f.ClientSet, pod)
			}
			framework.Logf("Set B and Set C terminated successfully. Set A and Set D remain running.")
		}()

		// Log upgrade outcome
		if upgradeSucceeded {
			framework.Logf("Upgrade phase completed successfully")
		} else {
			framework.Logf("Upgrade phase failed, but continuing to rollback phase...")
		}

		// Rollback phase - ALWAYS runs after upgrade

		// Perform rollback
		rollbackCSIDriver(cfg, f)

		// Create Set E after rollback
		framework.Logf("Creating Set E workloads after rollback...")
		fullAccessPodsSetE, readOnlyAccessPodsSetE := createTestWorkloads(ctx, pliFullAccessSA, pliReadOnlyAccessSA)

		// Test Set E workloads
		framework.Logf("Testing Set E workloads...")
		_, _, _ = writeAndVerifyTestFile(ctx, fullAccessPodsSetE)
		verifyReadOnlyAccess(ctx, readOnlyAccessPodsSetE, testFile, testWriteSize, seed)

		// Monitor Set A + D + E for 150 minutes after rollback
		framework.Logf("Monitoring workloads (Set A + D + E) for %d minutes after rollback...", ROLLBACK_TEST_DURATION_IN_MINUTES)

		allFullAccessAfterRollback := slices.Concat(fullAccessPodsSetA, fullAccessPodsSetD, fullAccessPodsSetE)
		allReadOnlyAfterRollback := slices.Concat(readOnlyAccessPodsSetA, readOnlyAccessPodsSetD, readOnlyAccessPodsSetE)

		for range ROLLBACK_TEST_DURATION_IN_MINUTES {
			framework.Logf("Checking if workloads are still healthy after rollback...")
			verifyWorkloadHealth(ctx, allFullAccessAfterRollback, allReadOnlyAfterRollback, testFile, testWriteSize, seed)
			framework.Logf("Sleeping for a minute for the next cycle...")
			time.Sleep(1 * time.Minute)
		}

		// Terminate Set A + D + E (test termination after rollback)
		framework.Logf("Terminating Set A, Set D, and Set E workloads to test termination after rollback...")
		for _, pod := range slices.Concat(fullAccessPodsSetA, readOnlyAccessPodsSetA, fullAccessPodsSetD, readOnlyAccessPodsSetD, fullAccessPodsSetE, readOnlyAccessPodsSetE) {
			e2epod.DeletePodWithWait(ctx, f.ClientSet, pod)
		}
		framework.Logf("Set A, Set D, and Set E terminated successfully")

		framework.Logf("Upgrade phase completed successfully")
	}

	BeforeEach(func(ctx context.Context) {
		l = local{}
		l.config = driver.PrepareTest(ctx, f)
		DeferCleanup(cleanup)
	})

	if helmChartPreviousVersion == "" && helmChartNewVersion == "" {
		// Run upgrade to current commit test
		It("Upgrade to current commit from latest release without interrupting workloads", func(ctx context.Context) {
			if helmChartContainerRepository == "" || helmChartContainerTag == "" {
				Fail("Please set container repository and tag using `REPOSITORY` and `TAG` environment variables")
			}

			settings, cfg := setupTestEnvironment(ctx)
			latestVersion := getLatestReleasedVersion(settings, cfg)
			// Using "0.0.0" as a placeholder version for the new commit being tested
			runUpgradeTest(ctx, latestVersion, "0.0.0", true)
		})
	} else {
		// Run version-to-version upgrade test
		It("Upgrade between specified versions without interrupting the workloads", func(ctx context.Context) {
			if helmChartPreviousVersion == "" || helmChartNewVersion == "" {
				Fail("Please set the previous and new versions to test using `MOUNTPOINT_CSI_DRIVER_PREVIOUS_VERSION` and `MOUNTPOINT_CSI_DRIVER_NEW_VERSION` environment variables")
			}

			_, _ = setupTestEnvironment(ctx)
			framework.Logf("Testing upgrade from %q to %q...", helmChartPreviousVersion, helmChartNewVersion)

			useSourceBuild := strings.HasSuffix(helmChartNewVersion, "-source")
			runUpgradeTest(ctx, helmChartPreviousVersion, helmChartNewVersion, useSourceBuild)
		})
	}
}

// buildHelmValues creates common Helm values for install/upgrade
func buildHelmValues() map[string]any {
	values := map[string]any{
		"node": map[string]any{
			"podInfoOnMountCompat": map[string]any{
				"enable": "true",
			},
		},
	}
	if helmChartContainerRepository != "" && helmChartContainerTag != "" {
		values["image"] = map[string]any{
			"repository": helmChartContainerRepository,
			"tag":        helmChartContainerTag,
		}
	}
	return values
}

// getLatestReleasedVersion retrieves the latest published release version to upgrade from.
// If the current chart version is published, it returns that version.
// Otherwise, it returns the latest published release less than the current version.
func getLatestReleasedVersion(settings *cli.EnvSettings, cfg *action.Configuration) string {
	// Load current chart version
	chart, err := loader.Load(helmChartSource)
	framework.ExpectNoError(err)
	chartVersion := chart.Metadata.Version
	framework.Logf("Current chart version: %s", chartVersion)

	allVersions := getAllPublishedVersions()

	// If chart version is published, use it
	if slices.Contains(allVersions, chartVersion) {
		framework.Logf("Chart version %s is published, using it for upgrade test", chartVersion)
		return chartVersion
	}

	// Chart version not published, find latest version less than current
	var olderVersions []string
	for _, v := range allVersions {
		if v < chartVersion {
			olderVersions = append(olderVersions, v)
		}
	}

	if len(olderVersions) == 0 {
		Fail(fmt.Sprintf("No published releases found older than %s", chartVersion))
	}

	slices.SortFunc(olderVersions, func(a, b string) int { return strings.Compare(b, a) })
	framework.Logf("Using latest published release older than %s: v%s", chartVersion, olderVersions[0])
	return olderVersions[0]
}

// getAllPublishedVersions fetches and parses all published versions from the Helm repository.
func getAllPublishedVersions() []string {
	client := &http.Client{Timeout: 30 * time.Second}

	resp, err := client.Get(helmRepo + "/index.yaml")
	framework.ExpectNoError(err)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		framework.Failf("Failed to fetch index.yaml: HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	framework.ExpectNoError(err)

	var index repo.IndexFile
	err = yaml.Unmarshal(body, &index)
	framework.ExpectNoError(err)

	var allVersions []string
	if chartVersions, ok := index.Entries[helmChartName]; ok {
		for _, cv := range chartVersions {
			if !strings.Contains(cv.Version, "-") {
				allVersions = append(allVersions, cv.Version)
			}
		}
	}

	if len(allVersions) == 0 {
		Fail("No published releases found in Helm repository")
	}

	return allVersions
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

	release, err := installClient.RunWithContext(context.Background(), chart, buildHelmValues())
	framework.ExpectNoError(err)

	framework.Logf("Helm release %q created", release.Name)
}

// upgradeCSIDriver upgrades the CSI Driver's Helm chart to the new version.
func upgradeCSIDriver(cfg *action.Configuration, f *framework.Framework, version string, chartPath string) {
	upgradeClient := action.NewUpgrade(cfg)
	upgradeClient.Namespace = helmReleaseNamespace
	upgradeClient.Version = version
	upgradeClient.Wait = true
	upgradeClient.Timeout = 30 * time.Second

	chart, err := loader.Load(chartPath)
	framework.ExpectNoError(err)

	release, err := upgradeClient.RunWithContext(context.Background(), helmReleaseName, chart, buildHelmValues())
	framework.ExpectNoError(err)

	framework.Logf("Helm release %q updated to %v (from %q)", release.Name, version, chartPath)

	framework.ExpectNoError(waitForCSIDriverDaemonSetRollout(context.Background(), f))
}

// rollbackCSIDriver performs a rollback using Helm's rollback action.
func rollbackCSIDriver(cfg *action.Configuration, f *framework.Framework) {

	rollbackClient := action.NewRollback(cfg)
	rollbackClient.Wait = true
	rollbackClient.Timeout = 30 * time.Second
	// Version = 0 means rollback to previous revision
	rollbackClient.Version = 0

	err := rollbackClient.Run(helmReleaseName)
	framework.ExpectNoError(err, "Failed to rollback CSI Driver")

	framework.Logf("Helm release %q rolled back successfully", helmReleaseName)

	// Wait for DaemonSet to rollout with old version
	framework.ExpectNoError(waitForCSIDriverDaemonSetRollout(context.Background(), f))
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

// waitForCSIDriverDaemonSetRollout waits until the CSI Driver's DaemonSet is ready after an upgrade.
func waitForCSIDriverDaemonSetRollout(ctx context.Context, f *framework.Framework) error {
	framework.Logf("Waiting for %q to ready", csiDriverDaemonSetName)

	err := framework.Gomega().
		Eventually(ctx, (func(ctx context.Context) error {
			ds := csiDriverDaemonSet(ctx, f)

			desired, scheduled, ready := ds.Status.DesiredNumberScheduled, ds.Status.CurrentNumberScheduled, ds.Status.NumberReady
			if desired != scheduled && desired != ready {
				return fmt.Errorf("DaemonSet is not ready. DesiredScheduled: %d, CurrentScheduled: %d, Ready: %d", desired, scheduled, ready)
			}

			return nil
		})).
		WithTimeout(1 * time.Minute).
		WithPolling(10 * time.Second).
		Should(gomega.BeNil())
	if err != nil {
		return err
	}
	framework.Logf("%q is ready", csiDriverDaemonSetName)
	return nil
}
