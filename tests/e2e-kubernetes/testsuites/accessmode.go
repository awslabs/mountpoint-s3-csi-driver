package custom_testsuites

import (
	"context"
	"fmt"
	"time"

	"github.com/onsi/ginkgo/v2"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
)

type s3CSIAccessModeTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

func InitS3AccessModeTestSuite() storageframework.TestSuite {
	return &s3CSIAccessModeTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name: "accessmode",
			TestPatterns: []storageframework.TestPattern{
				storageframework.DefaultFsPreprovisionedPV,
			},
		},
	}
}

func (t *s3CSIAccessModeTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

func (t *s3CSIAccessModeTestSuite) SkipUnsupportedTests(_ storageframework.TestDriver, _ storageframework.TestPattern) {
}

func (t *s3CSIAccessModeTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	type local struct {
		resources []*storageframework.VolumeResource
		config    *storageframework.PerTestConfig
	}
	var (
		l local
	)

	f := framework.NewFrameworkWithCustomTimeouts("accessmode", storageframework.GetDriverTimeouts(driver))
	f.NamespacePodSecurityLevel = admissionapi.LevelRestricted

	cleanup := func(ctx context.Context) {
		var errs []error
		for _, resource := range l.resources {
			errs = append(errs, resource.CleanupResource(ctx))
		}
		framework.ExpectNoError(errors.NewAggregate(errs), "while cleanup resource")
	}
	ginkgo.BeforeEach(func(ctx context.Context) {
		l = local{}
		l.config = driver.PrepareTest(ctx, f)
		ginkgo.DeferCleanup(cleanup)
	})

	validateWriteToVolumeFails := func(ctx context.Context) {
		resource := createVolumeResourceWithAccessMode(ctx, l.config, pattern, v1.ReadOnlyMany)
		l.resources = append(l.resources, resource)
		ginkgo.By("Creating pod with a volume")
		pod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelRestricted, "")
		var err error
		pod, err = createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
		framework.ExpectNoError(err)
		defer func() {
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
		}()
		volPath := "/mnt/volume1"
		fileInVol := fmt.Sprintf("%s/file.txt", volPath)
		seed := time.Now().UTC().UnixNano()
		toWrite := 1024 // 1KB
		ginkgo.By("Checking that write to a volume fails")
		checkWriteToPathFails(f, pod, fileInVol, toWrite, seed)
	}
	ginkgo.It("should fail to write to ReadOnlyMany volume", func(ctx context.Context) {
		validateWriteToVolumeFails(ctx)
	})
	ginkgo.It("S3 express -- should fail to write to ReadOnlyMany volume", func(ctx context.Context) {
		l.config.Prefix = S3ExpressTestIdentifier
		validateWriteToVolumeFails(ctx)
	})
}
