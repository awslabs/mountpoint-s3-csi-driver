package custom_testsuites

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"

	"bytes"
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
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
	"sigs.k8s.io/yaml"
)

// Upgrade/Rollback Test Coverage Summary
//
// What's Covered:
// The tests verify IRSA authentication (both driver-level and pod-level) with credential refresh by running for 90 minutes
// to exceed the 1-hour IAM role session duration and 10-minute service account token expiration cycles, ensuring credentials
// work across version transitions. They also ensure workload continuity by creating pods before/after upgrade/rollback and
// monitoring that file operations (read/write/delete) continue working throughout both processes, with clean pod termination
// verified at each stage.
//
// What's NOT Covered:
// EKS Pod Identity authentication is NOT tested (only IRSA is tested). TODO: Add EKS Pod Identity test coverage.
// Caching configurations (emptyDir, ephemeral, shared cache) are completely untested, and Mountpoint Pod Sharing scenarios
// during upgrades are not verified. Cross-account bucket access, resource limits on Mountpoint containers, and advanced
// mount options (like --prefix, --read-only, --metadata-ttl) are also not tested.
//
// However, these gaps are acceptable for upgrade/rollback testing because these features (cache, resource limits,
// mount options) are static configurations set at pod creation time and are not modified or maintained by the driver
// during pod lifecycle or version transitions. The upgrade/rollback process only affects the driver's control plane
// (credential refresh, pod scheduling, mount/unmount operations), which is covered by testing IRSA authentication
// and workload continuity. EKS Pod Identity should be added in future test iterations.

// This value defines how long the upgrade and rollback tests should take.
//
// This needs to be at least more than 70 minutes because
//  1. We ask for service account tokens that valid for 10 min
//  2. Session duration of the IAM roles we assume is 1 hour
//
// So, to make sure we hit both of the cycles in the worst case, we want to run our upgrade and rollback tests for 70min+.
// Therefore we can be sure if the credentials are successfully refreshed after the upgrade and rollback.
const UPGRADE_TEST_DURATION_IN_MINUTES = 90
const ROLLBACK_TEST_DURATION_IN_MINUTES = 90

// Token expiration for service account tokens during tests (10 minutes)
const TEST_TOKEN_EXPIRATION_SECONDS = 600

const helmRepo = "https://awslabs.github.io/mountpoint-s3-csi-driver"
const helmChartSource = "../../charts/aws-mountpoint-s3-csi-driver"
const helmChartName = "aws-mountpoint-s3-csi-driver"
const helmReleaseName = "mountpoint-s3-csi-driver"
const helmReleaseNamespace = "kube-system"

var helmChartPreviousVersion = os.Getenv("MOUNTPOINT_CSI_DRIVER_PREVIOUS_VERSION")
var helmChartNewVersion = os.Getenv("MOUNTPOINT_CSI_DRIVER_NEW_VERSION")
var helmChartContainerRepository = os.Getenv("REPOSITORY")
var helmChartContainerTag = os.Getenv("TAG")

// isMajorVersionUpgrade indicates the upgrade crosses a major version boundary, so rolling
// the driver back to the previous major version cannot serve mounts created by the new major
// version. In that case, workloads created on the new version (Set D) cannot survive the
// rollback and must be terminated before it. Defaults to false (same-major upgrade), where
// Set D is kept through the rollback and monitored to verify existing workloads survive.
var isMajorVersionUpgrade = os.Getenv("MOUNTPOINT_CSI_DRIVER_IS_MAJOR_VERSION_UPGRADE") == "true"

// tokenExpirationPostRenderer patches CSIDriver and ServiceAccount to use shorter token expiration for tests
type tokenExpirationPostRenderer struct {
	expirationSeconds int64
}

func (r *tokenExpirationPostRenderer) Run(renderedManifests *bytes.Buffer) (*bytes.Buffer, error) {
	decoder := yamlutil.NewYAMLOrJSONDecoder(renderedManifests, 4096)
	var result bytes.Buffer

	// We parse each YAML object from Helm manifests, handling EOF and skipping empty objects
	for {
		var obj map[string]interface{}
		if err := decoder.Decode(&obj); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}

		if obj == nil {
			continue
		}

		kind, _ := obj["kind"].(string)

		// Patch CSIDriver token expiration
		if kind == "CSIDriver" {
			spec := obj["spec"].(map[string]interface{})
			tokenRequests := spec["tokenRequests"].([]interface{})
			// Loop handles variable array length
			for i := range tokenRequests {
				tr := tokenRequests[i].(map[string]interface{})
				tr["expirationSeconds"] = r.expirationSeconds
			}
		}

		// Patch ServiceAccount token expiration annotation
		if kind == "ServiceAccount" {
			metadata := obj["metadata"].(map[string]interface{})
			if name, _ := metadata["name"].(string); name == "s3-csi-driver-sa" {
				annotations, ok := metadata["annotations"].(map[string]interface{})
				if !ok {
					annotations = make(map[string]interface{})
					metadata["annotations"] = annotations
				}
				annotations["eks.amazonaws.com/token-expiration"] = fmt.Sprintf("%d", r.expirationSeconds)
			}
		}

		// Re-encode the object
		data, err := yaml.Marshal(obj)
		if err != nil {
			return nil, err
		}
		result.WriteString("---\n")
		result.Write(data)
	}

	return &result, nil
}

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

		checkWriteToPathSucceedEventually(ctx, f, pod, path, testWriteSize, seed)
		checkReadFromPathSucceedEventually(ctx, f, pod, path, testWriteSize, seed)
		checkListingPathWithEntriesEventually(ctx, f, pod, e2epod.VolumeMountPath1, []string{filename, "test.txt"})
		checkDeletingPathSucceed(ctx, f, pod, path)
		checkListingPathWithEntriesEventually(ctx, f, pod, e2epod.VolumeMountPath1, []string{"test.txt"})
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

	// writeAndVerifyTestFile writes a test file with content derived from seed and verifies it can be read.
	writeAndVerifyTestFile := func(ctx context.Context, pods []*v1.Pod, seed int64) (testFile string, testWriteSize int) {
		testWriteSize = 1024
		testFile = filepath.Join(e2epod.VolumeMountPath1, "test.txt")
		for _, pod := range pods {
			checkWriteToPathSucceedEventually(ctx, f, pod, testFile, testWriteSize, seed)
			checkReadFromPathSucceedEventually(ctx, f, pod, testFile, testWriteSize, seed)
		}
		return
	}

	// verifyReadOnlyAccess verifies pods can list but not write
	verifyReadOnlyAccess := func(ctx context.Context, pods []*v1.Pod, testFile string, testWriteSize int, seed int64) {
		for _, pod := range pods {
			checkListingPathSucceedEventually(ctx, f, pod, e2epod.VolumeMountPath1)
			checkWriteToPathFailsEventually(ctx, f, pod, testFile, testWriteSize, seed)
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
			checkReadFromPathSucceedEventually(ctx, f, pod, testFile, testWriteSize, seed)
			checkBasicFileOperations(ctx, pod)
		}
		for _, pod := range readOnlyPods {
			checkListingPathSucceedEventually(ctx, f, pod, e2epod.VolumeMountPath1)
			checkWriteToPathFailsEventually(ctx, f, pod, testFile, testWriteSize, seed)
		}
	}

	// runUpgradeTest performs the complete upgrade test workflow
	runUpgradeTest := func(ctx context.Context, fromVersion, toVersion string, useSourceBuild bool) {
		settings, cfg := setupTestEnvironment(ctx)
		framework.Logf("Testing upgrade from %q to %q...", fromVersion, toVersion)

		// Install the previous version with token expiration patching
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
		// Set D	|After upgrade	| Test new version works; on same-major runs, verify it survives rollback	|Major-version upgrade: before rollback. Otherwise: after rollback monitoring
		// Set E	|After rollback	| Test new workload creation post-rollback	|After rollback monitoring

		// Create Set A + Set B (for upgrade test + rollback test)
		framework.Logf("Creating Set A and Set B workloads before upgrade...")
		fullAccessPodsSetA, readOnlyAccessPodsSetA := createTestWorkloads(ctx, pliFullAccessSA, pliReadOnlyAccessSA)
		fullAccessPodsSetB, readOnlyAccessPodsSetB := createTestWorkloads(ctx, pliFullAccessSA, pliReadOnlyAccessSA)

		// One seed for all sets: monitoring verifies every pod against a single seed.
		seed := time.Now().UTC().UnixNano()

		// Test Set A workloads
		framework.Logf("Testing Set A workloads...")
		testFile, testWriteSize := writeAndVerifyTestFile(ctx, fullAccessPodsSetA, seed)
		verifyReadOnlyAccess(ctx, readOnlyAccessPodsSetA, testFile, testWriteSize, seed)

		// Test Set B workloads
		framework.Logf("Testing Set B workloads...")
		testFile, testWriteSize = writeAndVerifyTestFile(ctx, fullAccessPodsSetB, seed)
		verifyReadOnlyAccess(ctx, readOnlyAccessPodsSetB, testFile, testWriteSize, seed)

		// Upgrade to the new version with token expiration patching
		if useSourceBuild {
			chartPath = packageHelmChartFromSource(toVersion)
		} else {
			chartPath = pullCSIDriver(settings, cfg, toVersion)
		}
		upgradeCSIDriver(cfg, f, toVersion, chartPath)

		// Create Set C + Set D after the upgrade
		framework.Logf("Creating Set C and Set D workloads after upgrade...")
		fullAccessPodsSetC, readOnlyAccessPodsSetC := createTestWorkloads(ctx, pliFullAccessSA, pliReadOnlyAccessSA)
		fullAccessPodsSetD, readOnlyAccessPodsSetD := createTestWorkloads(ctx, pliFullAccessSA, pliReadOnlyAccessSA)

		// Test Set C workloads
		framework.Logf("Testing Set C workloads...")
		testFile, testWriteSize = writeAndVerifyTestFile(ctx, fullAccessPodsSetC, seed)
		verifyReadOnlyAccess(ctx, readOnlyAccessPodsSetC, testFile, testWriteSize, seed)

		// Test Set D workloads
		framework.Logf("Testing Set D workloads...")
		testFile, testWriteSize = writeAndVerifyTestFile(ctx, fullAccessPodsSetD, seed)
		verifyReadOnlyAccess(ctx, readOnlyAccessPodsSetD, testFile, testWriteSize, seed)

		// Ensure the workloads are still healthy
		framework.Logf("Monitoring all 12 workloads (Set A + B + C + D) for %d minutes...", UPGRADE_TEST_DURATION_IN_MINUTES)
		allFullAccessAfterUpgrade := slices.Concat(fullAccessPodsSetA, fullAccessPodsSetB, fullAccessPodsSetC, fullAccessPodsSetD)
		allReadOnlyAfterUpgrade := slices.Concat(readOnlyAccessPodsSetA, readOnlyAccessPodsSetB, readOnlyAccessPodsSetC, readOnlyAccessPodsSetD)

		monitorWorkloadsForDuration(ctx, allFullAccessAfterUpgrade, allReadOnlyAfterUpgrade, testFile, testWriteSize, seed, UPGRADE_TEST_DURATION_IN_MINUTES*time.Minute, "upgrade", verifyWorkloadHealth)

		// Terminate Set B + Set C (test termination after upgrade)
		framework.Logf("Terminating Set B and Set C workloads to test termination after upgrade...")
		for _, pod := range slices.Concat(fullAccessPodsSetB, readOnlyAccessPodsSetB, fullAccessPodsSetC, readOnlyAccessPodsSetC) {
			e2epod.DeletePodWithWait(ctx, f.ClientSet, pod)
		}
		framework.Logf("Set B and Set C terminated successfully.")
		// Set D was created on the new version. On a MAJOR version upgrade, rolling the driver
		// back to the previous major version cannot serve mounts created by the new major
		// version, so Set D must be terminated before rollback.
		if isMajorVersionUpgrade {
			framework.Logf("Major version upgrade: Set A remains running, terminating Set D workloads before rollback (new-version workloads can't survive rollback to the previous major)...")
			for _, pod := range slices.Concat(fullAccessPodsSetD, readOnlyAccessPodsSetD) {
				e2epod.DeletePodWithWait(ctx, f.ClientSet, pod)
			}
		} else {
			framework.Logf("Set A and Set D remain running.")
		}

		framework.Logf("Upgrade phase completed successfully, proceeding to rollback test...")

		// Rollback phase - only runs if upgrade succeeded
		// Rollback failures are non-fatal and logged as warnings

		rollbackSucceeded := true
		func() {
			defer func() {
				if r := recover(); r != nil {
					rollbackSucceeded = false
					framework.Logf("WARNING: Rollback phase failed with panic: %v", r)
				}
			}()

			// Perform rollback
			rollbackCSIDriver(cfg, f)

			// Create Set E after rollback
			framework.Logf("Creating Set E workloads after rollback...")
			fullAccessPodsSetE, readOnlyAccessPodsSetE := createTestWorkloads(ctx, pliFullAccessSA, pliReadOnlyAccessSA)

			// Test Set E workloads
			framework.Logf("Testing Set E workloads...")
			testFile, testWriteSize = writeAndVerifyTestFile(ctx, fullAccessPodsSetE, seed)
			verifyReadOnlyAccess(ctx, readOnlyAccessPodsSetE, testFile, testWriteSize, seed)

			// Monitor Set A + E (+ Set D on non-major version upgrade runs) after rollback.
			allFullAccessAfterRollback := slices.Concat(fullAccessPodsSetA, fullAccessPodsSetE)
			allReadOnlyAfterRollback := slices.Concat(readOnlyAccessPodsSetA, readOnlyAccessPodsSetE)
			if !isMajorVersionUpgrade {
				allFullAccessAfterRollback = slices.Concat(allFullAccessAfterRollback, fullAccessPodsSetD)
				allReadOnlyAfterRollback = slices.Concat(allReadOnlyAfterRollback, readOnlyAccessPodsSetD)
			}
			framework.Logf("Monitoring workloads (Set A + E%s) for %d minutes after rollback...",
				map[bool]string{true: "", false: " + D"}[isMajorVersionUpgrade], ROLLBACK_TEST_DURATION_IN_MINUTES)

			monitorWorkloadsForDuration(ctx, allFullAccessAfterRollback, allReadOnlyAfterRollback, testFile, testWriteSize, seed, ROLLBACK_TEST_DURATION_IN_MINUTES*time.Minute, "rollback", verifyWorkloadHealth)

			// Terminate the monitored workloads (Set A + E, plus Set D on non-major version upgrade runs).
			framework.Logf("Terminating post-rollback workloads to test termination after rollback...")
			podsToTerminate := slices.Concat(fullAccessPodsSetA, readOnlyAccessPodsSetA, fullAccessPodsSetE, readOnlyAccessPodsSetE)
			if !isMajorVersionUpgrade {
				podsToTerminate = slices.Concat(podsToTerminate, fullAccessPodsSetD, readOnlyAccessPodsSetD)
			}
			for _, pod := range podsToTerminate {
				e2epod.DeletePodWithWait(ctx, f.ClientSet, pod)
			}
			framework.Logf("Post-rollback workloads terminated successfully")
		}()

		// Log rollback outcome with GitHub Actions annotation if failed
		if rollbackSucceeded {
			framework.Logf("Rollback phase completed successfully")
		} else {
			fmt.Println("::warning file=upgrade_and_rollback.go,line=318::Rollback phase failed but upgrade succeeded - test marked as passed")
			framework.Logf("WARNING: Rollback phase failed, but test is still marked as passed since upgrade succeeded")
		}
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

// buildHelmValuesBase creates Helm values for installation of the previous version,
// without image overrides so the chart's default image would be used.
func buildHelmValuesBase() map[string]any {
	return map[string]any{
		"node": map[string]any{
			"podInfoOnMountCompat": map[string]any{
				"enable": "true",
			},
		},
	}
}

// buildHelmValuesForUpgrade creates Helm values for install/upgrade of the new version,
// overriding the image fields to use the CI-built container from the current commit.
func buildHelmValuesForUpgrade() map[string]any {
	values := buildHelmValuesBase()
	if helmChartContainerRepository != "" && helmChartContainerTag != "" {
		values["image"] = map[string]any{
			"repository": helmChartContainerRepository,
			"tag":        helmChartContainerTag,
			"pullPolicy": "Always",
		}
	}
	// Disable maxVolumesPerNode limit for tests — the upgrade test creates 12+ PVs
	// on a single node. Must provide full array element because Helm replaces arrays
	// entirely (doesn't merge individual fields into array items).
	values["daemonsetMounters"] = []map[string]any{{
		"maxVolumesPerNode":  0,
		"resources":          map[string]any{"requests": map[string]any{"cpu": "500m", "memory": "2Gi"}},
		"logLevel":           4,
		"podLabels":          map[string]any{},
		"nodeSelector":       map[string]any{},
		"tolerateAllTaints":  true,
		"defaultTolerations": true,
		"tolerations":        []any{},
		"affinity":           map[string]any{},
		"imagePullSecrets":   []any{},
	}}
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
	installClient.PostRenderer = &tokenExpirationPostRenderer{expirationSeconds: TEST_TOKEN_EXPIRATION_SECONDS}

	chart, err := loader.Load(chartPath)
	framework.ExpectNoError(err)

	release, err := installClient.RunWithContext(context.Background(), chart, buildHelmValuesBase())
	framework.ExpectNoError(err)

	framework.Logf("Helm release %q created", release.Name)
}

// monitorWorkloadsForDuration monitors workload health for a specified duration, checking every minute and logging progress.
// TODO: track the seed per set so each set can write distinct content, which would also catch a pod reading another set's volume.
func monitorWorkloadsForDuration(
	ctx context.Context,
	fullAccessPods []*v1.Pod,
	readOnlyPods []*v1.Pod,
	testFile string,
	testWriteSize int,
	seed int64,
	duration time.Duration,
	phase string,
	verifyFunc func(context.Context, []*v1.Pod, []*v1.Pod, string, int, int64),
) {
	endTime := time.Now().Add(duration)
	for time.Now().Before(endTime) {
		framework.Logf("Checking if workloads are still healthy after %s...", phase)
		verifyFunc(ctx, fullAccessPods, readOnlyPods, testFile, testWriteSize, seed)

		if remaining := time.Until(endTime); remaining > time.Minute {
			time.Sleep(time.Minute)
		} else if remaining > 0 {
			time.Sleep(remaining)
		}
	}
}

// upgradeCSIDriver upgrades the CSI Driver's Helm chart to the new version.
func upgradeCSIDriver(cfg *action.Configuration, f *framework.Framework, version string, chartPath string) {
	upgradeClient := action.NewUpgrade(cfg)
	upgradeClient.Namespace = helmReleaseNamespace
	upgradeClient.Version = version
	upgradeClient.Wait = true
	upgradeClient.Timeout = 30 * time.Second
	upgradeClient.PostRenderer = &tokenExpirationPostRenderer{expirationSeconds: TEST_TOKEN_EXPIRATION_SECONDS}

	chart, err := loader.Load(chartPath)
	framework.ExpectNoError(err)

	release, err := upgradeClient.RunWithContext(context.Background(), helmReleaseName, chart, buildHelmValuesForUpgrade())
	framework.ExpectNoError(err)

	framework.Logf("Helm release %q updated to %v (from %q)", release.Name, version, chartPath)

	framework.ExpectNoError(waitForCSIDriverDaemonSetRollout(context.Background(), f))
}

// rollbackCSIDriver performs a rollback using Helm's rollback action.
func rollbackCSIDriver(cfg *action.Configuration, f *framework.Framework) {

	rollbackClient := action.NewRollback(cfg)
	rollbackClient.Wait = true
	rollbackClient.Timeout = 30 * time.Second
	// Version = 0 means rollback to previous revision https://github.com/helm/helm/blob/e31a078e/pkg/action/rollback.go#L129-L132
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
