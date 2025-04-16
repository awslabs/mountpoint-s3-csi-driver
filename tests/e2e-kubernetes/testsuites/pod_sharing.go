package custom_testsuites

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	crdv1 "github.com/awslabs/aws-s3-csi-driver/pkg/api/v1"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
	"k8s.io/utils/ptr"
)

var s3paGVR = schema.GroupVersionResource{Group: "s3.csi.aws.com", Version: "v1", Resource: "mountpoints3podattachments"}

type s3CSIPodSharingTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

func InitS3CSIPodSharingTestSuite() storageframework.TestSuite {
	return &s3CSIPodSharingTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name: "multivolume",
			TestPatterns: []storageframework.TestPattern{
				storageframework.DefaultFsPreprovisionedPV,
			},
		},
	}
}

func (t *s3CSIPodSharingTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

func (t *s3CSIPodSharingTestSuite) SkipUnsupportedTests(_ storageframework.TestDriver, _ storageframework.TestPattern) {
}

func (t *s3CSIPodSharingTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	type local struct {
		resources []*storageframework.VolumeResource
		config    *storageframework.PerTestConfig
	}
	var (
		l local
	)

	f := framework.NewFrameworkWithCustomTimeouts(NamespacePrefix+"multivolume", storageframework.GetDriverTimeouts(driver))
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

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

	ginkgo.It("should concurrently access the single volume from pods on the same node using the same Mountpoint Pod", func(ctx context.Context) {
		resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
		l.resources = append(l.resources, resource)

		var pods []*v1.Pod
		node := l.config.ClientNodeSelection
		// Create each pod with pvc
		for i := 0; i < 2; i++ {
			index := i + 1
			ginkgo.By(fmt.Sprintf("Creating pod%d with a volume on %+v", index, node))
			pod, err := e2epod.CreatePod(ctx, f.ClientSet, f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelBaseline, "")
			framework.ExpectNoError(err)
			// The pod must get deleted before this function returns because the caller may try to
			// delete volumes as part of the tests. Keeping the pod running would block that.
			// If the test times out, then the namespace deletion will take care of it.
			defer func() {
				framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
			}()
			pods = append(pods, pod)
			e2epod.SetAffinity(&node, pod.Spec.NodeName)
		}

		verifyPodsShareMountpointPod(ctx, f, pods, resource.Pv)
		checkCrossReadWrite(f, pods[0], pods[1])
	})

	ginkgo.It("should concurrently access the single volume from pods on the same node using different Mountpoint Pods if fsGroup is different", func(ctx context.Context) {
		resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
		l.resources = append(l.resources, resource)

		var pods []*v1.Pod
		var targetNode string
		for i := 0; i < 2; i++ {
			index := i + 1
			podConfig := &e2epod.Config{
				NS:            f.Namespace.Name,
				PVCs:          []*v1.PersistentVolumeClaim{resource.Pvc},
				SecurityLevel: admissionapi.LevelBaseline,
				FsGroup:       ptr.To(int64(1000 + i)),
			}

			// For the first pod, let it schedule anywhere
			// For subsequent pods, force them to the same node as the first pod
			if i > 0 && targetNode != "" {
				podConfig.NodeSelection = e2epod.NodeSelection{
					Name: targetNode,
				}
			}

			ginkgo.By(fmt.Sprintf("Creating pod%d", index))
			pod, err := e2epod.CreateSecPod(ctx, f.ClientSet, podConfig, 10*time.Second)
			framework.ExpectNoError(err)

			// Store the node name from the first pod
			if i == 0 {
				targetNode = pod.Spec.NodeName
			}

			defer func() {
				framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
			}()
			pods = append(pods, pod)
		}

		verifyPodsHaveDifferentMountpointPods(ctx, f, pods, resource.Pv, func(pod *v1.Pod) map[string]string {
			expectedFields := defaultExpectedFields(pod.Spec.NodeName, resource.Pv)
			expectedFields["WorkloadFSGroup"] = fmt.Sprintf("%d", pod.Spec.SecurityContext.FSGroup)
			return expectedFields
		})
		checkCrossReadWrite(f, pods[0], pods[1])
	})

	// TODO: Add more test cases
}

func verifyPodsShareMountpointPod(ctx context.Context, f *framework.Framework, pods []*v1.Pod, pv *v1.PersistentVolume) {
	var s3paList *crdv1.MountpointS3PodAttachmentList
	framework.Gomega().Eventually(ctx, framework.HandleRetry(func(ctx context.Context) (bool, error) {
		list, err := f.DynamicClient.Resource(s3paGVR).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, err
		}
		s3paList, err = convertToCustomResourceList(list)
		if err != nil {
			return false, err
		}
		for _, s3pa := range s3paList.Items {
			if matchesSpec(s3pa.Spec, defaultExpectedFields(pods[0].Spec.NodeName, pv)) {
				allUIDs := make(map[string]bool)
				for _, uids := range s3paList.Items[0].Spec.MountpointS3PodToWorkloadPodUIDs {
					for _, uid := range uids {
						allUIDs[uid] = true
					}
				}
				for _, pod := range pods {
					podUID := string(pod.UID)
					if _, exists := allUIDs[podUID]; !exists {
						return false, fmt.Errorf("pod UID %s not found in MountpointS3PodAttachment", podUID)
					}
				}

				return true, nil
			}
		}

		return false, err
	})).WithTimeout(10 * time.Second).WithPolling(1 * time.Second).Should(gomega.BeTrue())

}

func verifyPodsHaveDifferentMountpointPods(ctx context.Context, f *framework.Framework, pods []*v1.Pod, pv *v1.PersistentVolume, expectedFieldsFunc func(pod *v1.Pod) map[string]string) {
	var s3paList *crdv1.MountpointS3PodAttachmentList
	framework.Gomega().Eventually(ctx, framework.HandleRetry(func(ctx context.Context) (bool, error) {
		list, err := f.DynamicClient.Resource(s3paGVR).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, fmt.Errorf("failed to list S3PodAttachments: %w", err)
		}
		s3paList, err = convertToCustomResourceList(list)
		if err != nil {
			return false, fmt.Errorf("failed to convert to custom resource list: %w", err)
		}

		matchCount := 0
		for _, s3pa := range s3paList.Items {
			for _, pod := range pods {
				if matchesSpec(s3pa.Spec, expectedFieldsFunc(pod)) {
					matchCount++
					break
				}
			}
		}

		return matchCount == len(pods), nil
	})).WithTimeout(10 * time.Second).WithPolling(1 * time.Second).Should(gomega.BeTrue())

	podToMountpointPod := make(map[string]string)
	for _, s3pa := range s3paList.Items {
		for mpPodName, workloadPodUIDs := range s3pa.Spec.MountpointS3PodToWorkloadPodUIDs {
			for _, uid := range workloadPodUIDs {
				podToMountpointPod[uid] = mpPodName
			}
		}
	}

	seenMountpointPods := make(map[string]bool)
	for _, pod := range pods {
		podUID := string(pod.UID)
		mpPodName, exists := podToMountpointPod[podUID]

		framework.Gomega().Expect(exists).To(gomega.BeTrue())

		_, alreadySeen := seenMountpointPods[mpPodName]
		framework.Gomega().Expect(alreadySeen).To(gomega.BeFalse())

		seenMountpointPods[mpPodName] = true
	}

	framework.Gomega().Expect(len(seenMountpointPods)).To(gomega.Equal(len(pods)))
}

// Convert UnstructuredList to MountpointS3PodAttachmentList
func convertToCustomResourceList(list *unstructured.UnstructuredList) (*crdv1.MountpointS3PodAttachmentList, error) {
	crList := &crdv1.MountpointS3PodAttachmentList{
		Items: make([]crdv1.MountpointS3PodAttachment, 0, len(list.Items)),
	}

	for _, item := range list.Items {
		cr := &crdv1.MountpointS3PodAttachment{}
		err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, cr)
		if err != nil {
			return nil, fmt.Errorf("failed to convert item to MountpointS3PodAttachment: %v", err)
		}
		crList.Items = append(crList.Items, *cr)
	}

	return crList, nil
}

// matchesSpec checks whether MountpointS3PodAttachmentSpec matches `expected` fields
func matchesSpec(spec crdv1.MountpointS3PodAttachmentSpec, expected map[string]string) bool {
	specValues := map[string]string{
		"NodeName":                         spec.NodeName,
		"PersistentVolumeName":             spec.PersistentVolumeName,
		"VolumeID":                         spec.VolumeID,
		"MountOptions":                     spec.MountOptions,
		"AuthenticationSource":             spec.AuthenticationSource,
		"WorkloadFSGroup":                  spec.WorkloadFSGroup,
		"WorkloadServiceAccountName":       spec.WorkloadServiceAccountName,
		"WorkloadNamespace":                spec.WorkloadNamespace,
		"WorkloadServiceAccountIAMRoleARN": spec.WorkloadServiceAccountIAMRoleARN,
	}

	for k, v := range expected {
		if specValues[k] != v {
			return false
		}
	}
	return true
}

// defaultExpectedFields return default test expected fields for MountpointS3PodAttachmentSpec matching
func defaultExpectedFields(nodeName string, pv *v1.PersistentVolume) map[string]string {
	return map[string]string{
		"NodeName":             nodeName,
		"PersistentVolumeName": pv.Name,
		"VolumeID":             pv.Spec.CSI.VolumeHandle,
		"MountOptions":         strings.Join(pv.Spec.MountOptions, ","),
		"AuthenticationSource": "driver",
		"WorkloadFSGroup":      "",
	}
}

func checkCrossReadWrite(f *framework.Framework, pod1, pod2 *v1.Pod) {
	toWrite := 1024 // 1KB
	path := "/mnt/volume1"

	// Check write from pod1 and read from pod2
	checkPodWriteAndOtherPodRead(f, pod1, pod2, path, "file1.txt", toWrite)

	// Check write from pod2 and read from pod1
	checkPodWriteAndOtherPodRead(f, pod2, pod1, path, "file2.txt", toWrite)
}

func checkPodWriteAndOtherPodRead(f *framework.Framework, writerPod, readerPod *v1.Pod, basePath, filename string, size int) {
	filePath := filepath.Join(basePath, filename)
	seed := time.Now().UTC().UnixNano()

	checkWriteToPath(f, writerPod, filePath, size, seed)
	checkReadFromPath(f, readerPod, filePath, size, seed)
}
