package custom_testsuites

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	crdv1beta "github.com/awslabs/aws-s3-csi-driver/pkg/api/v1beta"
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

var s3paGVR = schema.GroupVersionResource{Group: "s3.csi.aws.com", Version: "v1beta", Resource: "mountpoints3podattachments"}

const mountpointNamespace = "mount-s3"

const defaultTimeout = 10 * time.Second
const defaultInterval = 1 * time.Second

const podCleanupTimeout = 5 * time.Minute

var IsPodMounter bool

type s3CSIPodSharingTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

func InitS3CSIPodSharingTestSuite() storageframework.TestSuite {
	return &s3CSIPodSharingTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name: "podsharing",
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
	var l local

	f := framework.NewFrameworkWithCustomTimeouts(NamespacePrefix+"podsharing", storageframework.GetDriverTimeouts(driver))
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

	cleanup := func(ctx context.Context) {
		var errs []error
		for _, resource := range l.resources {
			errs = append(errs, resource.CleanupResource(ctx))
		}
		framework.ExpectNoError(errors.NewAggregate(errs), "while cleanup resource")
	}

	ginkgo.Describe("Pod Sharing", ginkgo.Ordered, func() {
		var oidcProvider string

		ginkgo.BeforeAll(func(ctx context.Context) {
			oidcProvider = oidcProviderForCluster(ctx, f)
		})

		ginkgo.BeforeEach(func(ctx context.Context) {
			if !IsPodMounter {
				ginkgo.Skip("Pod Mounter is not enabled, skipping pod sharing tests")
			}

			l = local{}
			l.config = driver.PrepareTest(ctx, f)
			ginkgo.DeferCleanup(cleanup)
		})

		ginkgo.It("should share Mountpoint Pod (authenticationSource=driver)", func(ctx context.Context) {
			resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
			l.resources = append(l.resources, resource)

			targetNode, pods := createPodsInTheSameNode(ctx, f, 2, resource, func(index int, pod *v1.Pod) {})

			s3paNames, mountpointPodNames := verifyPodsShareMountpointPod(ctx, f, pods, defaultExpectedFields(targetNode, resource.Pv))
			defer deleteWorkloadPodsAndEnsureMountpointResourcesCleaned(ctx, f, pods, s3paNames, mountpointPodNames)

			checkCrossReadWrite(f, pods[0], pods[1])
		})

		ginkgo.It("should share Mountpoint Pod if pods have the same fsGroup", func(ctx context.Context) {
			resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
			l.resources = append(l.resources, resource)

			targetNode, pods := createPodsInTheSameNode(ctx, f, 2, resource, func(index int, pod *v1.Pod) {
				if pod.Spec.SecurityContext == nil {
					pod.Spec.SecurityContext = &v1.PodSecurityContext{}
				}
				pod.Spec.SecurityContext.FSGroup = ptr.To(int64(1000))
			})

			expectedFields := defaultExpectedFields(targetNode, resource.Pv)
			expectedFields["WorkloadFSGroup"] = "1000"

			s3paNames, mountpointPodNames := verifyPodsShareMountpointPod(ctx, f, pods, expectedFields)
			defer deleteWorkloadPodsAndEnsureMountpointResourcesCleaned(ctx, f, pods, s3paNames, mountpointPodNames)

			checkCrossReadWrite(f, pods[0], pods[1])
		})

		ginkgo.It("should not share Mountpoint Pod if pods have different fsGroup", func(ctx context.Context) {
			resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
			l.resources = append(l.resources, resource)

			_, pods := createPodsInTheSameNode(ctx, f, 2, resource, func(index int, pod *v1.Pod) {
				if pod.Spec.SecurityContext == nil {
					pod.Spec.SecurityContext = &v1.PodSecurityContext{}
				}
				pod.Spec.SecurityContext.FSGroup = ptr.To(int64(1000 + index))
			})

			s3paNames, mountpointPodNames := verifyPodsHaveDifferentMountpointPods(ctx, f, pods, func(pod *v1.Pod) map[string]string {
				expectedFields := defaultExpectedFields(pod.Spec.NodeName, resource.Pv)
				expectedFields["WorkloadFSGroup"] = strconv.FormatInt(*pod.Spec.SecurityContext.FSGroup, 10)
				return expectedFields
			})
			defer deleteWorkloadPodsAndEnsureMountpointResourcesCleaned(ctx, f, pods, s3paNames, mountpointPodNames)

			checkCrossReadWrite(f, pods[0], pods[1])
		})

		ginkgo.It("should not share Mountpoint Pod if mountOptions are different", func(ctx context.Context) {
			resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
			l.resources = append(l.resources, resource)

			var pods []*v1.Pod

			// First Pod
			ginkgo.By("Creating pod1 with a volume")
			pod1, err := e2epod.CreatePod(ctx, f.ClientSet, f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelBaseline, "")
			framework.ExpectNoError(err)
			targetNode := pod1.Spec.NodeName

			resource.Pv, err = f.ClientSet.CoreV1().PersistentVolumes().Get(ctx, resource.Pv.Name, metav1.GetOptions{})
			framework.ExpectNoError(err)
			firstMountOptions := strings.Join(resource.Pv.Spec.MountOptions, ",")
			resource.Pv.Spec.MountOptions = []string{"--allow-delete"}
			resource.Pv, err = f.ClientSet.CoreV1().PersistentVolumes().Update(ctx, resource.Pv, metav1.UpdateOptions{})
			framework.ExpectNoError(err)

			// Second Pod
			pods = append(pods, pod1)
			ginkgo.By("Creating pod2 with a volume")
			pod2, err := e2epod.CreatePod(ctx, f.ClientSet, f.Namespace.Name, map[string]string{"kubernetes.io/hostname": targetNode}, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelBaseline, "")
			framework.ExpectNoError(err)
			pods = append(pods, pod2)

			s3paNames, mountpointPodNames := verifyPodsHaveDifferentMountpointPods(ctx, f, pods, func(pod *v1.Pod) map[string]string {
				expectedFields := defaultExpectedFields(pod.Spec.NodeName, resource.Pv)
				if pod.Name == pod1.Name {
					expectedFields["MountOptions"] = firstMountOptions
				} else {
					expectedFields["MountOptions"] = "--allow-delete"
				}
				return expectedFields
			})
			defer deleteWorkloadPodsAndEnsureMountpointResourcesCleaned(ctx, f, pods, s3paNames, mountpointPodNames)

			checkCrossReadWrite(f, pods[0], pods[1])
		})

		ginkgo.It("should share Mountpoint Pod if pod namespaces and service accounts are the same (authenticationSource=pod)", func(ctx context.Context) {
			if oidcProvider == "" {
				ginkgo.Skip("OIDC provider is not configured, skipping PLI - IRSA tests")
			}

			idConfig, err := setupPodLevelIdentity(ctx, f, oidcProvider)
			framework.ExpectNoError(err)
			ginkgo.DeferCleanup(idConfig.Cleanup)

			resource := createVolumeResourceWithMountOptions(contextWithAuthenticationSource(ctx, "pod"), l.config, pattern, nil)
			l.resources = append(l.resources, resource)

			targetNode, pods := createPodsInTheSameNode(ctx, f, 2, resource, func(index int, pod *v1.Pod) {
				pod.Spec.ServiceAccountName = idConfig.ServiceAccount.Name
			})

			expectedFields := defaultExpectedFields(targetNode, resource.Pv)
			expectedFields["AuthenticationSource"] = "pod"
			expectedFields["WorkloadNamespace"] = f.Namespace.Name
			expectedFields["WorkloadServiceAccountName"] = idConfig.ServiceAccount.Name
			s3paNames, mountpointPodNames := verifyPodsShareMountpointPod(ctx, f, pods, expectedFields)
			defer deleteWorkloadPodsAndEnsureMountpointResourcesCleaned(ctx, f, pods, s3paNames, mountpointPodNames)

			checkCrossReadWrite(f, pods[0], pods[1])
		})

		ginkgo.It("should not share Mountpoint Pod if pod service accounts are the different (authenticationSource=pod)", func(ctx context.Context) {
			if oidcProvider == "" {
				ginkgo.Skip("OIDC provider is not configured, skipping PLI - IRSA tests")
			}

			idConfig1, err := setupPodLevelIdentity(ctx, f, oidcProvider)
			framework.ExpectNoError(err)
			ginkgo.DeferCleanup(idConfig1.Cleanup)

			idConfig2, err := setupPodLevelIdentity(ctx, f, oidcProvider)
			framework.ExpectNoError(err)
			ginkgo.DeferCleanup(idConfig2.Cleanup)

			saNames := []string{idConfig1.ServiceAccount.Name, idConfig2.ServiceAccount.Name}
			resource := createVolumeResourceWithMountOptions(contextWithAuthenticationSource(ctx, "pod"), l.config, pattern, nil)
			l.resources = append(l.resources, resource)

			_, pods := createPodsInTheSameNode(ctx, f, 2, resource, func(index int, pod *v1.Pod) {
				pod.Spec.ServiceAccountName = saNames[index-1]
			})

			s3paNames, mountpointPodNames := verifyPodsHaveDifferentMountpointPods(ctx, f, pods, func(pod *v1.Pod) map[string]string {
				expectedFields := defaultExpectedFields(pod.Spec.NodeName, resource.Pv)
				expectedFields["AuthenticationSource"] = "pod"
				expectedFields["WorkloadNamespace"] = f.Namespace.Name
				expectedFields["WorkloadServiceAccountName"] = pod.Spec.ServiceAccountName
				return expectedFields
			})
			defer deleteWorkloadPodsAndEnsureMountpointResourcesCleaned(ctx, f, pods, s3paNames, mountpointPodNames)

			checkCrossReadWrite(f, pods[0], pods[1])
		})

		ginkgo.It("should allow read-only mount from a shared read-write Mountpoint Pod", func(ctx context.Context) {
			resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
			l.resources = append(l.resources, resource)

			targetNode, pods := createPodsInTheSameNode(ctx, f, 2, resource, func(index int, pod *v1.Pod) {
				if index > 1 {
					container := &pod.Spec.Containers[0]
					for i := range container.VolumeMounts {
						container.VolumeMounts[i].ReadOnly = true
					}
				}
			})

			s3paNames, mountpointPodNames := verifyPodsShareMountpointPod(ctx, f, pods, defaultExpectedFields(targetNode, resource.Pv))
			defer deleteWorkloadPodsAndEnsureMountpointResourcesCleaned(ctx, f, pods, s3paNames, mountpointPodNames)

			toWrite := 1024 // 1KB
			firstFile := "/mnt/volume1/file1.txt"
			secondFile := "/mnt/volume1/file2.txt"
			seed := time.Now().UTC().UnixNano()
			// pods[0] should get a read-write mount
			checkWriteToPath(f, pods[0], firstFile, toWrite, seed)

			// pods[1] should get a read-only mount
			checkReadFromPath(f, pods[1], firstFile, toWrite, seed)
			checkWriteToPathFails(f, pods[1], secondFile, toWrite, seed)
		})

		ginkgo.It("should allow read-only PVC mount from a shared read-write Mountpoint Pod", func(ctx context.Context) {
			resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
			l.resources = append(l.resources, resource)

			targetNode, pods := createPodsInTheSameNode(ctx, f, 2, resource, func(index int, pod *v1.Pod) {
				if index > 1 {
					for i := range pod.Spec.Volumes {
						pod.Spec.Volumes[i].PersistentVolumeClaim.ReadOnly = true
					}
				}
			})

			s3paNames, mountpointPodNames := verifyPodsShareMountpointPod(ctx, f, pods, defaultExpectedFields(targetNode, resource.Pv))
			defer deleteWorkloadPodsAndEnsureMountpointResourcesCleaned(ctx, f, pods, s3paNames, mountpointPodNames)

			toWrite := 1024 // 1KB
			firstFile := "/mnt/volume1/file1.txt"
			secondFile := "/mnt/volume1/file2.txt"
			seed := time.Now().UTC().UnixNano()
			// pods[0] should get a read-write mount
			checkWriteToPath(f, pods[0], firstFile, toWrite, seed)

			// pods[1] should get a read-only mount
			checkReadFromPath(f, pods[1], firstFile, toWrite, seed)
			checkWriteToPathFails(f, pods[1], secondFile, toWrite, seed)
		})
	})
}

// createPodsInTheSameNode creates `n` nodes within a single node that uses given volume resource.
// It allows modifying Pod specs via the provided callback.
func createPodsInTheSameNode(ctx context.Context, f *framework.Framework, n int, resource *storageframework.VolumeResource, modifier func(int, *v1.Pod)) (string, []*v1.Pod) {
	var pods []*v1.Pod
	var targetNode string
	var nodeSelector map[string]string
	for i := range n {
		index := i + 1

		if i > 0 && targetNode != "" {
			nodeSelector = map[string]string{"kubernetes.io/hostname": targetNode}
		}

		ginkgo.By(fmt.Sprintf("Creating pod%d with a volume", index))
		pod := e2epod.MakePod(f.Namespace.Name, nodeSelector, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelBaseline, "")
		modifier(index, pod)

		pod, err := createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
		framework.ExpectNoError(err)
		ginkgo.DeferCleanup(e2epod.DeletePodWithWait, f.ClientSet, pod)

		if i == 0 {
			targetNode = pod.Spec.NodeName
		} else {
			gomega.Expect(pod.Spec.NodeName).To(gomega.Equal(targetNode))
		}
		pods = append(pods, pod)
	}
	return targetNode, pods
}

func deleteWorkloadPodsAndEnsureMountpointResourcesCleaned(ctx context.Context, f *framework.Framework, pods []*v1.Pod, s3paNames, mountpointPodNames []string) {
	for _, pod := range pods {
		framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
	}
	verifyMountpointResourcesCleanup(ctx, f, s3paNames, mountpointPodNames)
}

func verifyPodsShareMountpointPod(ctx context.Context, f *framework.Framework, pods []*v1.Pod, expectedFields map[string]string) ([]string, []string) {
	var s3paNames []string
	var mountpointPodNames []string
	var s3paList *crdv1beta.MountpointS3PodAttachmentList
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
			if matchesSpec(s3pa.Spec, expectedFields) {
				s3paNames = append(s3paNames, s3pa.Name)
				allUIDs := make(map[string]bool)
				for mpPodName, attachments := range s3pa.Spec.MountpointS3PodAttachments {
					mountpointPodNames = append(mountpointPodNames, mpPodName)
					for _, attachment := range attachments {
						allUIDs[attachment.WorkloadPodUID] = true
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
	})).WithTimeout(defaultTimeout).WithPolling(defaultInterval).Should(gomega.BeTrue())

	return s3paNames, mountpointPodNames
}

func verifyPodsHaveDifferentMountpointPods(ctx context.Context, f *framework.Framework, pods []*v1.Pod, expectedFieldsFunc func(pod *v1.Pod) map[string]string) ([]string, []string) {
	var s3paNames []string
	var mountpointPodNames []string
	var s3paList *crdv1beta.MountpointS3PodAttachmentList
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
					s3paNames = append(s3paNames, s3pa.Name)
					matchCount++
					break
				}
			}
		}

		return matchCount == len(pods), nil
	})).WithTimeout(defaultTimeout).WithPolling(defaultInterval).Should(gomega.BeTrue())

	podToMountpointPod := make(map[string]string)
	for _, s3pa := range s3paList.Items {
		for mpPodName, attachments := range s3pa.Spec.MountpointS3PodAttachments {
			for _, attachment := range attachments {
				podToMountpointPod[attachment.WorkloadPodUID] = mpPodName
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
		mountpointPodNames = append(mountpointPodNames, mpPodName)
	}

	framework.Gomega().Expect(len(seenMountpointPods)).To(gomega.Equal(len(pods)))

	return s3paNames, mountpointPodNames
}

func verifyMountpointResourcesCleanup(ctx context.Context, f *framework.Framework, s3paNames []string, mountpointPodNames []string) {
	framework.Logf("Verifying MountpointS3PodAttachments are deleted: %v", s3paNames)
	for _, s3paName := range s3paNames {
		err := waitForKubernetesObjectToDisappear(ctx, func(ctx context.Context) (*unstructured.Unstructured, error) {
			return f.DynamicClient.Resource(s3paGVR).Get(ctx, s3paName, metav1.GetOptions{})
		}, podCleanupTimeout, defaultInterval)
		gomega.Expect(err).To(gomega.BeNil())
	}

	framework.Logf("Verifying Mountpoint Pods are deleted: %v", mountpointPodNames)
	for _, mpPodName := range mountpointPodNames {
		err := waitForKubernetesObjectToDisappear(ctx, func(ctx context.Context) (*v1.Pod, error) {
			return f.ClientSet.CoreV1().Pods(mountpointNamespace).Get(ctx, mpPodName, metav1.GetOptions{})
		}, podCleanupTimeout, defaultInterval)
		gomega.Expect(err).To(gomega.BeNil())
	}
}

// Convert UnstructuredList to MountpointS3PodAttachmentList
func convertToCustomResourceList(list *unstructured.UnstructuredList) (*crdv1beta.MountpointS3PodAttachmentList, error) {
	crList := &crdv1beta.MountpointS3PodAttachmentList{
		Items: make([]crdv1beta.MountpointS3PodAttachment, 0, len(list.Items)),
	}

	for _, item := range list.Items {
		cr := &crdv1beta.MountpointS3PodAttachment{}
		err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, cr)
		if err != nil {
			return nil, fmt.Errorf("failed to convert item to MountpointS3PodAttachment: %v", err)
		}
		crList.Items = append(crList.Items, *cr)
	}

	return crList, nil
}

// matchesSpec checks whether MountpointS3PodAttachmentSpec matches `expected` fields
func matchesSpec(spec crdv1beta.MountpointS3PodAttachmentSpec, expected map[string]string) bool {
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

type podLevelIdentityConfig struct {
	ServiceAccount *v1.ServiceAccount
	IAMRole        string
	Cleanup        func(context.Context) error
}

// setupPodLevelIdentity creates necessary resources for pod-level identity tests
func setupPodLevelIdentity(ctx context.Context, f *framework.Framework, oidcProvider string) (*podLevelIdentityConfig, error) {
	config := &podLevelIdentityConfig{}
	var cleanupFuncs []func(context.Context) error

	// Create Service Account
	sa, removeSA := createServiceAccount(ctx, f)
	config.ServiceAccount = sa
	cleanupFuncs = append(cleanupFuncs, removeSA)

	// Create IAM Role with full access policy
	role, removeRole := createRole(ctx, f,
		assumeRoleWithWebIdentityPolicyDocument(ctx, oidcProvider, sa),
		iamPolicyS3FullAccess)
	config.IAMRole = *role.Arn
	cleanupFuncs = append(cleanupFuncs, removeRole)

	// Annotate Service Account with Role ARN
	sa, restoreServiceAccountRole := overrideServiceAccountRole(ctx, f, sa, config.IAMRole)
	config.ServiceAccount = sa
	cleanupFuncs = append(cleanupFuncs, restoreServiceAccountRole)

	// Wait for role to be assumable
	waitUntilRoleIsAssumableWithWebIdentity(ctx, f, sa)

	// Combine cleanup functions
	config.Cleanup = func(ctx context.Context) error {
		var errs []error
		// Execute cleanup functions in reverse order
		for i := len(cleanupFuncs) - 1; i >= 0; i-- {
			if err := cleanupFuncs[i](ctx); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.NewAggregate(errs)
	}

	return config, nil
}
