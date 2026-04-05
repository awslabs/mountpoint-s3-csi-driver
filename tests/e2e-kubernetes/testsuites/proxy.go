package custom_testsuites

import (
	"context"
	"fmt"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/test/e2e/framework"
	e2eevents "k8s.io/kubernetes/test/e2e/framework/events"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eservice "k8s.io/kubernetes/test/e2e/framework/service"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
)

type s3CSIProxyTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

func InitS3ProxyTestSuite() storageframework.TestSuite {
	return &s3CSIProxyTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name: "proxy",
			TestPatterns: []storageframework.TestPattern{
				storageframework.DefaultFsPreprovisionedPV,
			},
		},
	}
}

func (t *s3CSIProxyTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

func (t *s3CSIProxyTestSuite) SkipUnsupportedTests(_ storageframework.TestDriver, pattern storageframework.TestPattern) {
	if pattern.VolType != storageframework.PreprovisionedPV {
		e2eskipper.Skipf("Suite %q does not support %v", t.tsInfo.Name, pattern.VolType)
	}
}

func (t *s3CSIProxyTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	type local struct {
		resources []*storageframework.VolumeResource
		config    *storageframework.PerTestConfig
	}
	var (
		l local
	)

	f := framework.NewFrameworkWithCustomTimeouts("proxy", storageframework.GetDriverTimeouts(driver))
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

	expectFailToMount := func(ctx context.Context) {
		resource := createVolumeResourceWithAccessMode(ctx, l.config, pattern, v1.ReadWriteMany)
		l.resources = append(l.resources, resource)

		client := f.ClientSet.CoreV1().Pods(f.Namespace.Name)

		pod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelRestricted, "")

		pod, err := client.Create(ctx, pod, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		eventSelector := fields.Set{
			"involvedObject.kind":      "Pod",
			"involvedObject.name":      pod.Name,
			"involvedObject.namespace": f.Namespace.Name,
			"reason":                   events.FailedMountVolume,
		}.AsSelector().String()
		framework.Logf("Waiting for FailedMount event: %s", eventSelector)

		err = e2eevents.WaitTimeoutForEvent(ctx, f.ClientSet, f.Namespace.Name, eventSelector, "MountVolume.SetUp failed", 5*time.Minute)
		if err == nil {
			framework.Logf("Got FailedMount event: %s", eventSelector)
		} else {
			framework.Logf("Didn't get FailedMount event: %s", eventSelector)
		}

		pod, err = client.Get(ctx, pod.Name, metav1.GetOptions{})
		framework.ExpectNoError(err)
		gomega.Expect(pod.Status.Phase).To(gomega.Equal(v1.PodPending))
	}

	ginkgo.It("should be able to operate behind proxy", func(ctx context.Context) {
		proxyUrl := "proxy.default.svc.cluster.local"
		var proxyPort int32 = 3128
		proxy := contextWithVolumeAttributes(ctx, map[string]string{
			"mountpointEnv.HTTPS_PROXY": fmt.Sprintf("http://%s:%d", proxyUrl, proxyPort),
		})

		resource := createVolumeResource(proxy, l.config, pattern, v1.ReadWriteMany, []string{
			fmt.Sprintf("uid=%d", defaultNonRootUser),
			fmt.Sprintf("gid=%d", defaultNonRootGroup),
			"allow-other",
			"debug",
			"debug-crt",
		})
		l.resources = append(l.resources, resource)

		ginkgo.By("Creating proxy pod")
		client := f.ClientSet.CoreV1().Pods(v1.NamespaceDefault)

		proxyLabels := map[string]string{
			"app": "proxy",
		}

		proxyPod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "proxy",
				Namespace: v1.NamespaceDefault,
				Labels:    proxyLabels,
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Name:  "proxy",
						Image: "ubuntu/squid:latest",
						Ports: []v1.ContainerPort{{ContainerPort: proxyPort}},
					},
				},
			},
		}

		proxyPod, err := client.Create(ctx, proxyPod, metav1.CreateOptions{})
		framework.ExpectNoError(err)
		defer func() {
			_ = e2epod.DeletePodWithWait(ctx, f.ClientSet, proxyPod)
		}()

		ginkgo.By("Creating proxy service")
		serviceClient := f.ClientSet.CoreV1().Services(v1.NamespaceDefault)

		proxyService := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "proxy",
				Namespace: v1.NamespaceDefault,
			},
			Spec: v1.ServiceSpec{
				Selector: proxyLabels,
				Ports:    []v1.ServicePort{{Port: proxyPort}},
			},
		}

		proxyService, err = serviceClient.Create(ctx, proxyService, metav1.CreateOptions{})
		framework.ExpectNoError(err)
		defer func() {
			e2eservice.WaitForServiceDeletedWithFinalizer(ctx, f.ClientSet, v1.NamespaceDefault, proxyService.Name)
		}()

		ginkgo.By("Creating workload pod with a volume")
		pod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelRestricted, "")
		podModifierNonRoot(pod)
		pod, err = createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
		framework.ExpectNoError(err)
		defer func() {
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
		}()
		volPath := "/mnt/volume1"
		fileInVol := fmt.Sprintf("%s/file.txt", volPath)
		seed := time.Now().UTC().UnixNano()
		toWrite := 1024 // 1KB
		ginkgo.By("Checking write to a volume")
		checkWriteToPath(ctx, f, pod, fileInVol, toWrite, seed)
		ginkgo.By("Checking read from a volume")
		checkReadFromPath(ctx, f, pod, fileInVol, toWrite, seed)
		ginkgo.By("Checking file group owner")
		e2epod.VerifyExecInPodSucceed(ctx, f, pod, fmt.Sprintf("stat -L -c '%%a %%g %%u' %s | grep '644 %d %d'", fileInVol, defaultNonRootGroup, defaultNonRootUser))
		ginkgo.By("Checking dir group owner")
		e2epod.VerifyExecInPodSucceed(ctx, f, pod, fmt.Sprintf("stat -L -c '%%a %%g %%u' %s | grep '755 %d %d'", volPath, defaultNonRootGroup, defaultNonRootUser))
		ginkgo.By("Checking pod identity")
		e2epod.VerifyExecInPodSucceed(ctx, f, pod, fmt.Sprintf("id | grep 'uid=%d gid=%d groups=%d'", defaultNonRootUser, defaultNonRootGroup, defaultNonRootGroup))

		ginkgo.By("Checking mountpoint actually operate behind proxy")
		// Find the Mountpoint pods associated with our volume
		mpPods, err := findMountpointPods(ctx, f.ClientSet, resource.Pv.Name)
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "Failed to find Mountpoint pods")

		logs, err := e2epod.GetPodLogs(ctx, f.ClientSet, mpPods[0].Namespace, mpPods[0].Name, mpPods[0].Spec.Containers[0].Name)
		framework.ExpectNoError(err)
		gomega.Expect(logs).To(gomega.ContainSubstring(fmt.Sprintf("through a tunnel via proxy \"%s\"", proxyUrl)))
	})

	ginkgo.It("should fail when mountpointEnv.HTTPS_PROXY set to a non-existent proxy", func(ctx context.Context) {
		expectFailToMount(contextWithVolumeAttributes(ctx, map[string]string{
			"mountpointEnv.HTTPS_PROXY": "nonexistentproxy",
		}))
	})

	ginkgo.It("should reject unallowed env var", func(ctx context.Context) {
		expectFailToMount(contextWithVolumeAttributes(ctx, map[string]string{
			"mountpointEnv.FOO": "BAR",
		}))
	})
}
