package custom_testsuites

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
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

const helmRepo = "https://awslabs.github.io/mountpoint-s3-csi-driver"
const helmChartSource = "../../charts/aws-mountpoint-s3-csi-driver"
const helmChartSourceVersion = "0.0.0-source"
const helmChartName = "aws-mountpoint-s3-csi-driver"
const helmReleaseName = "aws-mountpoint-s3-csi-driver"
const helmReleaseNamespace = "kube-system"

var helmChartPreviousVersion = os.Getenv("MOUNTPOINT_CSI_DRIVER_PREVIOUS_VERSION")

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

	createPod := func(ctx context.Context) *v1.Pod {
		vol := createVolumeResourceWithMountOptions(ctx, l.config, pattern, []string{"debug", "debug-crt"})
		deferCleanup(vol.CleanupResource)

		pod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{vol.Pvc}, admissionapi.LevelBaseline, "")

		pod, err := createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
		framework.ExpectNoError(err)
		deferCleanup(func(ctx context.Context) error { return e2epod.DeletePodWithWait(ctx, f.ClientSet, pod) })

		return pod
	}

	BeforeEach(func(ctx context.Context) {
		l = local{}
		l.config = driver.PrepareTest(ctx, f)
		DeferCleanup(cleanup)
	})

	Describe("Upgrade", Serial, Ordered, func() {
		It("Upgrades to latest from previous version", func(ctx context.Context) {
			if helmChartPreviousVersion == "" {
				Fail("Please set previous version to test against using `MOUNTPOINT_CSI_DRIVER_PREVIOUS_VERSION` environment variable")
			}

			settings, cfg := initHelmClient()

			// Make sure to start from a clean state
			uninstallCSIDriver(cfg)

			// Install previously released version from our Helm repo
			chartPath := pullCSIDriver(settings, cfg, helmChartPreviousVersion)
			installCSIDriver(cfg, helmChartPreviousVersion, chartPath)

			// Create a new workload
			pod := createPod(ctx)

			// Write a sample file
			seed := time.Now().UTC().UnixNano()
			testWriteSize := 1024 // 1KB
			testFile := filepath.Join(e2epod.VolumeMountPath1, "test.txt")
			checkWriteToPath(f, pod, testFile, testWriteSize, seed)
			checkReadFromPath(f, pod, testFile, testWriteSize, seed)

			// Now upgrade it to latest version by packaging our Helm chart from source
			chartPath = packageHelmChartFromSource(helmChartSourceVersion)
			upgradeCSIDriver(cfg, helmChartSourceVersion, chartPath)

			// Ensure the workload is still healthy and the testing file is still readable
			for range 15 {
				checkReadFromPath(f, pod, testFile, testWriteSize, seed)
				time.Sleep(1 * time.Minute)
			}

			// Ensure the workload can be terminated without any problem
			e2epod.DeletePodWithWait(ctx, f.ClientSet, pod)
		})

		// TODO: Add test cases for:
		// 	- Using pod-level identity with IRSA
		//  - Using driver-level identity with IRSA
		//  - Creating a new workload after upgrade
	})
}

// packageHelmChartFromSource creates a Helm package from the CSI Driver's source.
func packageHelmChartFromSource(version string) string {
	out := GinkgoT().TempDir()

	packageClient := action.NewPackage()
	packageClient.Destination = out
	packageClient.Version = version

	chartPath, err := packageClient.Run(helmChartSource, map[string]any{})
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

	release, err := upgradeClient.RunWithContext(context.Background(), helmChartName, chart, map[string]any{})
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
