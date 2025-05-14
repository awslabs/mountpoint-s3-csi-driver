package controller_test

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "github.com/onsi/gomega/gstruct"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/awslabs/aws-s3-csi-driver/cmd/aws-s3-csi-controller/csicontroller"
	crdv1beta "github.com/awslabs/aws-s3-csi-driver/pkg/api/v1beta"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/version"
	"github.com/awslabs/aws-s3-csi-driver/pkg/podmounter/mppod"
)

var _ = Describe("Mountpoint Controller", func() {
	var testNode string

	BeforeEach(func() {
		testNode = generateRandomNodeName()
	})

	Context("Static Provisioning", func() {
		Context("Scheduled Pod with pre-bound PV and PVC", func() {
			It("should schedule a Mountpoint Pod", func() {
				vol := createVolume()
				vol.bind()

				pod := createPod(withPVC(vol.pvc))
				pod.schedule(testNode)

				waitAndVerifyS3PodAttachmentAndMountpointPod(testNode, vol, pod)
			})

			It("should schedule a Mountpoint Pod per PV", func() {
				vol1 := createVolume()
				vol1.bind()
				vol2 := createVolume()
				vol2.bind()

				pod := createPod(withPVC(vol1.pvc), withPVC(vol2.pvc))
				pod.schedule(testNode)

				waitAndVerifyS3PodAttachmentAndMountpointPod(testNode, vol1, pod)
				waitAndVerifyS3PodAttachmentAndMountpointPod(testNode, vol2, pod)
			})

			It("should not schedule a Mountpoint Pod if the volume is backed by a different CSI driver", func() {
				vol := createVolume(withCSIDriver(ebsCSIDriver))
				vol.bind()

				pod := createPod(withPVC(vol.pvc))
				pod.schedule(testNode)

				expectNoS3PodAttachmentWithFields(defaultExpectedFields(testNode, vol.pv))
			})

			It("should only schedule Mountpoint Pods for volumes backed by S3 CSI Driver", func() {
				vol1 := createVolume()
				vol1.bind()
				vol2 := createVolume(withCSIDriver(ebsCSIDriver))
				vol2.bind()

				pod := createPod(withPVC(vol1.pvc), withPVC(vol2.pvc))
				pod.schedule(testNode)

				waitAndVerifyS3PodAttachmentAndMountpointPod(testNode, vol1, pod)
				expectNoS3PodAttachmentWithFields(defaultExpectedFields(testNode, vol2.pv))
			})
		})

		Context("Scheduled Pod with late PV and PVC binding", func() {
			It("should schedule a Mountpoint Pod", func() {
				vol := createVolume()

				pod := createPod(withPVC(vol.pvc))
				pod.schedule(testNode)

				expectNoS3PodAttachmentWithFields(defaultExpectedFields(testNode, vol.pv))

				vol.bind()

				waitAndVerifyS3PodAttachmentAndMountpointPod(testNode, vol, pod)
			})

			It("should schedule a Mountpoint Pod per PV", func() {
				vol1 := createVolume()
				vol2 := createVolume()

				pod := createPod(withPVC(vol1.pvc), withPVC(vol2.pvc))
				pod.schedule(testNode)

				expectNoS3PodAttachmentWithFields(defaultExpectedFields(testNode, vol1.pv))
				expectNoS3PodAttachmentWithFields(defaultExpectedFields(testNode, vol2.pv))

				vol1.bind()

				waitAndVerifyS3PodAttachmentAndMountpointPod(testNode, vol1, pod)
				expectNoS3PodAttachmentWithFields(defaultExpectedFields(testNode, vol2.pv))

				vol2.bind()

				waitAndVerifyS3PodAttachmentAndMountpointPod(testNode, vol2, pod)
			})

			It("should not schedule a Mountpoint Pod if the volume is backed by a different CSI driver", func() {
				vol := createVolume(withCSIDriver(ebsCSIDriver))

				pod := createPod(withPVC(vol.pvc))
				pod.schedule(testNode)

				expectNoS3PodAttachmentWithFields(defaultExpectedFields(testNode, vol.pv))

				vol.bind()

				expectNoS3PodAttachmentWithFields(defaultExpectedFields(testNode, vol.pv))
			})

			It("should only schedule Mountpoint Pods for volumes backed by S3 CSI Driver", func() {
				vol1 := createVolume()
				vol2 := createVolume(withCSIDriver(ebsCSIDriver))

				pod := createPod(withPVC(vol1.pvc), withPVC(vol2.pvc))
				pod.schedule(testNode)

				expectNoS3PodAttachmentWithFields(defaultExpectedFields(testNode, vol1.pv))
				expectNoS3PodAttachmentWithFields(defaultExpectedFields(testNode, vol2.pv))

				vol2.bind()

				expectNoS3PodAttachmentWithFields(defaultExpectedFields(testNode, vol1.pv))
				expectNoS3PodAttachmentWithFields(defaultExpectedFields(testNode, vol2.pv))

				vol1.bind()

				waitAndVerifyS3PodAttachmentAndMountpointPod(testNode, vol1, pod)
				expectNoS3PodAttachmentWithFields(defaultExpectedFields(testNode, vol2.pv))
			})
		})

		Context("Late scheduled Pod with pre-bound PV and PVC", func() {
			It("should schedule a Mountpoint Pod", func() {
				vol := createVolume()
				vol.bind()

				pod := createPod(withPVC(vol.pvc))

				expectNoS3PodAttachmentWithFields(defaultExpectedFields(testNode, vol.pv))

				pod.schedule(testNode)

				waitAndVerifyS3PodAttachmentAndMountpointPod(testNode, vol, pod)
			})

			It("should schedule a Mountpoint Pod per PV", func() {
				vol1 := createVolume()
				vol1.bind()
				vol2 := createVolume()
				vol2.bind()

				pod := createPod(withPVC(vol1.pvc), withPVC(vol2.pvc))

				expectNoS3PodAttachmentWithFields(map[string]string{"PersistentVolumeName": vol1.pv.Name})
				expectNoS3PodAttachmentWithFields(map[string]string{"PersistentVolumeName": vol2.pv.Name})

				pod.schedule(testNode)

				waitAndVerifyS3PodAttachmentAndMountpointPod(testNode, vol1, pod)
				waitAndVerifyS3PodAttachmentAndMountpointPod(testNode, vol2, pod)
			})

			It("should not schedule a Mountpoint Pod if the volume is backed by a different CSI driver", func() {
				vol := createVolume(withCSIDriver(ebsCSIDriver))
				vol.bind()

				pod := createPod(withPVC(vol.pvc))

				expectNoS3PodAttachmentWithFields(map[string]string{"PersistentVolumeName": vol.pv.Name})

				pod.schedule(testNode)

				expectNoS3PodAttachmentWithFields(defaultExpectedFields(testNode, vol.pv))
			})

			It("should only schedule Mountpoint Pods for volumes backed by S3 CSI Driver", func() {
				vol1 := createVolume()
				vol1.bind()
				vol2 := createVolume(withCSIDriver(ebsCSIDriver))
				vol2.bind()

				pod := createPod(withPVC(vol1.pvc), withPVC(vol2.pvc))

				expectNoS3PodAttachmentWithFields(map[string]string{"PersistentVolumeName": vol1.pv.Name})
				expectNoS3PodAttachmentWithFields(map[string]string{"PersistentVolumeName": vol2.pv.Name})

				pod.schedule(testNode)

				waitAndVerifyS3PodAttachmentAndMountpointPod(testNode, vol1, pod)
				expectNoS3PodAttachmentWithFields(defaultExpectedFields(testNode, vol2.pv))
			})
		})

		Context("Late scheduled Pod with late PV and PVC binding", func() {
			It("should schedule a Mountpoint Pod", func() {
				vol := createVolume()

				pod := createPod(withPVC(vol.pvc))

				expectNoS3PodAttachmentWithFields(map[string]string{"PersistentVolumeName": vol.pv.Name})

				pod.schedule(testNode)

				expectNoS3PodAttachmentWithFields(defaultExpectedFields(testNode, vol.pv))

				vol.bind()

				waitAndVerifyS3PodAttachmentAndMountpointPod(testNode, vol, pod)
			})

			It("should schedule a Mountpoint Pod per PV", func() {
				vol1 := createVolume()
				vol2 := createVolume()

				pod := createPod(withPVC(vol1.pvc), withPVC(vol2.pvc))

				expectNoS3PodAttachmentWithFields(map[string]string{"PersistentVolumeName": vol1.pv.Name})
				expectNoS3PodAttachmentWithFields(map[string]string{"PersistentVolumeName": vol2.pv.Name})

				pod.schedule(testNode)
				vol2.bind()

				expectNoS3PodAttachmentWithFields(defaultExpectedFields(testNode, vol1.pv))
				waitAndVerifyS3PodAttachmentAndMountpointPod(testNode, vol2, pod)

				vol1.bind()

				waitAndVerifyS3PodAttachmentAndMountpointPod(testNode, vol1, pod)
			})

			It("should not schedule a Mountpoint Pod if the volume is backed by a different CSI driver", func() {
				vol := createVolume(withCSIDriver(ebsCSIDriver))

				pod := createPod(withPVC(vol.pvc))

				expectNoS3PodAttachmentWithFields(map[string]string{"PersistentVolumeName": vol.pv.Name})

				vol.bind()

				expectNoS3PodAttachmentWithFields(map[string]string{"PersistentVolumeName": vol.pv.Name})

				pod.schedule(testNode)

				expectNoS3PodAttachmentWithFields(defaultExpectedFields(testNode, vol.pv))
			})

			It("should only schedule Mountpoint Pods for volumes backed by S3 CSI Driver", func() {
				vol1 := createVolume()
				vol2 := createVolume(withCSIDriver(ebsCSIDriver))

				pod := createPod(withPVC(vol1.pvc), withPVC(vol2.pvc))

				expectNoS3PodAttachmentWithFields(map[string]string{"PersistentVolumeName": vol1.pv.Name})
				expectNoS3PodAttachmentWithFields(map[string]string{"PersistentVolumeName": vol2.pv.Name})

				vol1.bind()
				vol2.bind()
				pod.schedule(testNode)

				waitAndVerifyS3PodAttachmentAndMountpointPod(testNode, vol1, pod)
				expectNoS3PodAttachmentWithFields(defaultExpectedFields(testNode, vol2.pv))
			})
		})

		Context("Multiple Pods using the same PV and PVC", func() {
			Context("Same Node", func() {
				Context("Pre-bound PV and PVC", func() {
					It("should schedule single Mountpoint Pod", func() {
						vol := createVolume()
						vol.bind()

						pod1 := createPod(withPVC(vol.pvc))
						pod2 := createPod(withPVC(vol.pvc))

						expectNoS3PodAttachmentWithFields(map[string]string{"PersistentVolumeName": vol.pv.Name})

						pod1.schedule(testNode)

						s3pa, _ := waitAndVerifyS3PodAttachmentAndMountpointPod(testNode, vol, pod1)
						expectNoPodUIDInS3PodAttachment(s3pa, string(pod2.UID))

						pod2.schedule(testNode)

						s3pa1, mpPod1 := waitAndVerifyS3PodAttachmentAndMountpointPodWithMinVersion(testNode, vol, pod1, s3pa.ResourceVersion)
						s3pa2, mpPod2 := waitAndVerifyS3PodAttachmentAndMountpointPodWithMinVersion(testNode, vol, pod2, s3pa.ResourceVersion)

						Expect(s3pa1.Name).To(Equal(s3pa2.Name), "S3PodAttachment should have the same name")
						Expect(mpPod1.Name).To(Equal(mpPod2.Name), "Mountpoint Pods should have the same name")
					})
				})

				Context("Late PV and PVC binding", func() {
					It("should schedule single Mountpoint Pod", func() {
						vol := createVolume()

						pod1 := createPod(withPVC(vol.pvc))
						pod2 := createPod(withPVC(vol.pvc))
						pod1.schedule(testNode)

						expectNoS3PodAttachmentWithFields(defaultExpectedFields(testNode, vol.pv))

						vol.bind()

						s3pa, _ := waitAndVerifyS3PodAttachmentAndMountpointPod(testNode, vol, pod1)
						expectNoPodUIDInS3PodAttachment(s3pa, string(pod2.UID))

						pod2.schedule(testNode)

						waitAndVerifyS3PodAttachmentAndMountpointPodWithMinVersion(testNode, vol, pod1, s3pa.ResourceVersion)
						waitAndVerifyS3PodAttachmentAndMountpointPodWithMinVersion(testNode, vol, pod2, s3pa.ResourceVersion)
					})
				})

				Context("MountOptions", func() {
					It("should schedule different Mountpoint Pods if mountOptions were modified", func() {
						vol := createVolume()
						vol.bind()
						pv := vol.pv

						pod1 := createPod(withPVC(vol.pvc))
						pod1.schedule(testNode)

						s3pa1, mpPod1 := waitAndVerifyS3PodAttachmentAndMountpointPod(testNode, vol, pod1)

						// Adding some sleep time before updating PV because reconciler requeues pod1 event to clear expectation
						// and it can cause transient test failure if we update PV MountOptions too quickly
						time.Sleep(5 * time.Second)

						pv.Spec.MountOptions = []string{"--allow-delete"}
						Expect(k8sClient.Update(ctx, pv)).To(Succeed())

						pod2 := createPod(withPVC(vol.pvc))
						pod2.schedule(testNode)

						s3pa2, mpPod2 := waitAndVerifyS3PodAttachmentAndMountpointPod(testNode, vol, pod2)

						Expect(s3pa1.Name).NotTo(Equal(s3pa2.Name), "S3PodAttachment should not have the same name")
						Expect(mpPod1.Name).NotTo(Equal(mpPod2.Name), "Mountpoint Pods should not have the same name")
					})
				})

				Context("FSGroup", func() {
					It("should schedule single Mountpoint Pod if workload pods have the same FSGroup", func() {
						vol := createVolume()
						vol.bind()

						pod1 := createPod(withPVC(vol.pvc), withFSGroup(1111))
						pod2 := createPod(withPVC(vol.pvc), withFSGroup(1111))
						pod1.schedule(testNode)

						expectedFields := defaultExpectedFields(testNode, vol.pv)
						expectedFields["WorkloadFSGroup"] = "1111"
						s3pa, _ := waitAndVerifyS3PodAttachmentAndMountpointPodWithExpectedFields(testNode, vol, pod1, expectedFields)
						expectNoPodUIDInS3PodAttachment(s3pa, string(pod2.UID))

						pod2.schedule(testNode)

						s3pa1, mpPod1 := waitAndVerifyS3PodAttachmentAndMountpointPodWithMinVersionAndExpectedField(testNode, vol, pod1, s3pa.ResourceVersion, expectedFields)
						s3pa2, mpPod2 := waitAndVerifyS3PodAttachmentAndMountpointPodWithMinVersionAndExpectedField(testNode, vol, pod2, s3pa.ResourceVersion, expectedFields)

						Expect(s3pa1.Name).To(Equal(s3pa2.Name), "S3PodAttachment should have the same name")
						Expect(mpPod1.Name).To(Equal(mpPod2.Name), "Mountpoint Pods should have the same name")
					})

					It("should schedule different Mountpoint Pods if workload pods have different FSGroup", func() {
						vol := createVolume()
						vol.bind()

						pod1 := createPod(withPVC(vol.pvc)) // no fsGroup
						pod2 := createPod(withPVC(vol.pvc), withFSGroup(1111))
						pod3 := createPod(withPVC(vol.pvc), withFSGroup(2222))
						pod1.schedule(testNode)
						pod2.schedule(testNode)
						pod3.schedule(testNode)

						expectedFields := defaultExpectedFields(testNode, vol.pv)
						s3pa1 := waitForS3PodAttachmentWithFields(expectedFields, "")
						expectedFields["WorkloadFSGroup"] = "1111"
						s3pa2 := waitForS3PodAttachmentWithFields(expectedFields, "")
						expectedFields["WorkloadFSGroup"] = "2222"
						s3pa3 := waitForS3PodAttachmentWithFields(expectedFields, "")

						Expect(len(s3pa1.Spec.MountpointS3PodAttachments)).To(Equal(1))
						Expect(len(s3pa2.Spec.MountpointS3PodAttachments)).To(Equal(1))
						Expect(len(s3pa3.Spec.MountpointS3PodAttachments)).To(Equal(1))
						mpPod1 := waitAndVerifyMountpointPodFromPodAttachment(s3pa1, pod1, vol)
						mpPod2 := waitAndVerifyMountpointPodFromPodAttachment(s3pa2, pod2, vol)
						mpPod3 := waitAndVerifyMountpointPodFromPodAttachment(s3pa3, pod3, vol)

						Expect(s3pa1.Name).NotTo(Equal(s3pa2.Name), "S3PodAttachment should not have the same name")
						Expect(s3pa1.Name).NotTo(Equal(s3pa3.Name), "S3PodAttachment should not have the same name")
						Expect(s3pa2.Name).NotTo(Equal(s3pa3.Name), "S3PodAttachment should not have the same name")

						Expect(mpPod1.Name).NotTo(Equal(mpPod2.Name), "Mountpoint Pods should not have the same name")
						Expect(mpPod1.Name).NotTo(Equal(mpPod3.Name), "Mountpoint Pods should not have the same name")
						Expect(mpPod2.Name).NotTo(Equal(mpPod3.Name), "Mountpoint Pods should not have the same name")
					})
				})

				Context("authenticationSource=pod", func() {
					It("should schedule single Mountpoint Pod if workload pods have the same namespace and service account", func() {
						vol := createVolume(withVolumeAttributes(map[string]string{
							"authenticationSource": "pod",
						}))
						vol.bind()

						sa := &corev1.ServiceAccount{
							ObjectMeta: metav1.ObjectMeta{
								GenerateName: "sa-",
								Namespace:    defaultNamespace,
							},
						}
						Expect(k8sClient.Create(ctx, sa)).To(Succeed())

						pod1 := createPod(withPVC(vol.pvc), withServiceAccount(sa.Name))
						pod2 := createPod(withPVC(vol.pvc), withServiceAccount(sa.Name))
						pod1.schedule(testNode)

						expectedFields := defaultExpectedFields(testNode, vol.pv)
						expectedFields["AuthenticationSource"] = "pod"
						expectedFields["WorkloadServiceAccountName"] = sa.Name
						expectedFields["WorkloadNamespace"] = defaultNamespace
						s3pa, _ := waitAndVerifyS3PodAttachmentAndMountpointPodWithExpectedFields(testNode, vol, pod1, expectedFields)
						expectNoPodUIDInS3PodAttachment(s3pa, string(pod2.UID))

						pod2.schedule(testNode)

						s3pa1, mpPod1 := waitAndVerifyS3PodAttachmentAndMountpointPodWithMinVersionAndExpectedField(testNode, vol, pod1, s3pa.ResourceVersion, expectedFields)
						s3pa2, mpPod2 := waitAndVerifyS3PodAttachmentAndMountpointPodWithMinVersionAndExpectedField(testNode, vol, pod2, s3pa.ResourceVersion, expectedFields)

						Expect(s3pa1.Name).To(Equal(s3pa2.Name), "S3PodAttachment should have the same name")
						Expect(mpPod1.Name).To(Equal(mpPod2.Name), "Mountpoint Pods should have the same name")
					})

					It("should schedule single Mountpoint Pod if workload pods have the same namespace, service account and IRSA role annotation", func() {
						vol := createVolume(withVolumeAttributes(map[string]string{
							"authenticationSource": "pod",
						}))
						vol.bind()

						sa := &corev1.ServiceAccount{
							ObjectMeta: metav1.ObjectMeta{
								GenerateName: "sa-",
								Namespace:    defaultNamespace,
								Annotations:  map[string]string{csicontroller.AnnotationServiceAccountRole: "test-role"},
							},
						}
						Expect(k8sClient.Create(ctx, sa)).To(Succeed())

						pod1 := createPod(withPVC(vol.pvc), withServiceAccount(sa.Name))
						pod2 := createPod(withPVC(vol.pvc), withServiceAccount(sa.Name))
						pod1.schedule(testNode)

						expectedFields := defaultExpectedFields(testNode, vol.pv)
						expectedFields["AuthenticationSource"] = "pod"
						expectedFields["WorkloadServiceAccountName"] = sa.Name
						expectedFields["WorkloadNamespace"] = defaultNamespace
						expectedFields["WorkloadServiceAccountIAMRoleARN"] = "test-role"
						s3pa, _ := waitAndVerifyS3PodAttachmentAndMountpointPodWithExpectedFields(testNode, vol, pod1, expectedFields)
						expectNoPodUIDInS3PodAttachment(s3pa, string(pod2.UID))

						pod2.schedule(testNode)

						s3pa1, mpPod1 := waitAndVerifyS3PodAttachmentAndMountpointPodWithMinVersionAndExpectedField(testNode, vol, pod1, s3pa.ResourceVersion, expectedFields)
						s3pa2, mpPod2 := waitAndVerifyS3PodAttachmentAndMountpointPodWithMinVersionAndExpectedField(testNode, vol, pod2, s3pa.ResourceVersion, expectedFields)

						Expect(s3pa1.Name).To(Equal(s3pa2.Name), "S3PodAttachment should have the same name")
						Expect(mpPod1.Name).To(Equal(mpPod2.Name), "Mountpoint Pods should have the same name")
					})

					It("should schedule different Mountpoint Pods if workload pods have different IRSA role annotations for the same service account", func() {
						vol := createVolume(withVolumeAttributes(map[string]string{
							"authenticationSource": "pod",
						}))
						vol.bind()

						sa := &corev1.ServiceAccount{
							ObjectMeta: metav1.ObjectMeta{
								GenerateName: "sa-",
								Namespace:    defaultNamespace,
							},
						}
						Expect(k8sClient.Create(ctx, sa)).To(Succeed())

						pod1 := createPod(withPVC(vol.pvc), withServiceAccount(sa.Name)) // no IRSA annotation
						pod1.schedule(testNode)
						expectedFields := defaultExpectedFields(testNode, vol.pv)
						expectedFields["AuthenticationSource"] = "pod"
						expectedFields["WorkloadNamespace"] = defaultNamespace
						expectedFields["WorkloadServiceAccountName"] = sa.Name
						expectedFields["WorkloadServiceAccountIAMRoleARN"] = ""
						s3pa1 := waitForS3PodAttachmentWithFields(expectedFields, "")

						// Adding some sleep time before updating SA because reconciler requeues pod1 event to clear expectation
						// and it can cause transient test failure if we update SA annotation too quickly
						time.Sleep(5 * time.Second)

						sa.Annotations = map[string]string{csicontroller.AnnotationServiceAccountRole: "test-role-1"}
						Expect(k8sClient.Update(ctx, sa)).To(Succeed())
						pod2 := createPod(withPVC(vol.pvc), withServiceAccount(sa.Name))
						pod2.schedule(testNode)
						expectedFields["WorkloadServiceAccountIAMRoleARN"] = "test-role-1"
						s3pa2 := waitForS3PodAttachmentWithFields(expectedFields, "")

						time.Sleep(5 * time.Second)

						sa.Annotations = map[string]string{csicontroller.AnnotationServiceAccountRole: "test-role-2"}
						Expect(k8sClient.Update(ctx, sa)).To(Succeed())
						pod3 := createPod(withPVC(vol.pvc), withServiceAccount(sa.Name))
						pod3.schedule(testNode)
						expectedFields["WorkloadServiceAccountIAMRoleARN"] = "test-role-2"
						s3pa3 := waitForS3PodAttachmentWithFields(expectedFields, "")

						Expect(len(s3pa1.Spec.MountpointS3PodAttachments)).To(Equal(1))
						Expect(len(s3pa2.Spec.MountpointS3PodAttachments)).To(Equal(1))
						Expect(len(s3pa3.Spec.MountpointS3PodAttachments)).To(Equal(1))
						mpPod1 := waitAndVerifyMountpointPodFromPodAttachment(s3pa1, pod1, vol)
						mpPod2 := waitAndVerifyMountpointPodFromPodAttachment(s3pa2, pod2, vol)
						mpPod3 := waitAndVerifyMountpointPodFromPodAttachment(s3pa3, pod3, vol)

						Expect(s3pa1.Name).NotTo(Equal(s3pa2.Name), "S3PodAttachment should not have the same name")
						Expect(s3pa1.Name).NotTo(Equal(s3pa3.Name), "S3PodAttachment should not have the same name")
						Expect(s3pa2.Name).NotTo(Equal(s3pa3.Name), "S3PodAttachment should not have the same name")

						Expect(mpPod1.Name).NotTo(Equal(mpPod2.Name), "Mountpoint Pods should not have the same name")
						Expect(mpPod1.Name).NotTo(Equal(mpPod3.Name), "Mountpoint Pods should not have the same name")
						Expect(mpPod2.Name).NotTo(Equal(mpPod3.Name), "Mountpoint Pods should not have the same name")
					})

					It("should schedule different Mountpoint Pods if workload pods have different service account names in the same namespace", func() {
						vol := createVolume(withVolumeAttributes(map[string]string{
							"authenticationSource": "pod",
						}))
						vol.bind()

						sa1 := &corev1.ServiceAccount{
							ObjectMeta: metav1.ObjectMeta{
								GenerateName: "sa-",
								Namespace:    defaultNamespace,
							},
						}
						sa2 := &corev1.ServiceAccount{
							ObjectMeta: metav1.ObjectMeta{
								GenerateName: "sa-",
								Namespace:    defaultNamespace,
							},
						}
						Expect(k8sClient.Create(ctx, sa1)).To(Succeed())
						Expect(k8sClient.Create(ctx, sa2)).To(Succeed())

						pod1 := createPod(withPVC(vol.pvc)) // default service account
						pod2 := createPod(withPVC(vol.pvc), withServiceAccount(sa1.Name))
						pod3 := createPod(withPVC(vol.pvc), withServiceAccount(sa2.Name))
						pod1.schedule(testNode)
						pod2.schedule(testNode)
						pod3.schedule(testNode)

						expectedFields := defaultExpectedFields(testNode, vol.pv)
						expectedFields["AuthenticationSource"] = "pod"
						expectedFields["WorkloadNamespace"] = defaultNamespace
						expectedFields["WorkloadServiceAccountName"] = "default"
						s3pa1 := waitForS3PodAttachmentWithFields(expectedFields, "")
						expectedFields["WorkloadServiceAccountName"] = sa1.Name
						s3pa2 := waitForS3PodAttachmentWithFields(expectedFields, "")
						expectedFields["WorkloadServiceAccountName"] = sa2.Name
						s3pa3 := waitForS3PodAttachmentWithFields(expectedFields, "")

						Expect(len(s3pa1.Spec.MountpointS3PodAttachments)).To(Equal(1))
						Expect(len(s3pa2.Spec.MountpointS3PodAttachments)).To(Equal(1))
						Expect(len(s3pa3.Spec.MountpointS3PodAttachments)).To(Equal(1))
						mpPod1 := waitAndVerifyMountpointPodFromPodAttachment(s3pa1, pod1, vol)
						mpPod2 := waitAndVerifyMountpointPodFromPodAttachment(s3pa2, pod2, vol)
						mpPod3 := waitAndVerifyMountpointPodFromPodAttachment(s3pa3, pod3, vol)

						Expect(s3pa1.Name).NotTo(Equal(s3pa2.Name), "S3PodAttachment should not have the same name")
						Expect(s3pa1.Name).NotTo(Equal(s3pa3.Name), "S3PodAttachment should not have the same name")
						Expect(s3pa2.Name).NotTo(Equal(s3pa3.Name), "S3PodAttachment should not have the same name")

						Expect(mpPod1.Name).NotTo(Equal(mpPod2.Name), "Mountpoint Pods should not have the same name")
						Expect(mpPod1.Name).NotTo(Equal(mpPod3.Name), "Mountpoint Pods should not have the same name")
						Expect(mpPod2.Name).NotTo(Equal(mpPod3.Name), "Mountpoint Pods should not have the same name")
					})

					It("should schedule different Mountpoint Pods if workload pods have same service account names in the different namespace", func() {
						vol := createVolume(withVolumeAttributes(map[string]string{
							"authenticationSource": "pod",
						}))
						vol.bind()

						ns := &corev1.Namespace{
							ObjectMeta: metav1.ObjectMeta{
								GenerateName: "ns-",
							},
						}
						Expect(k8sClient.Create(ctx, ns)).To(Succeed())
						sa1 := &corev1.ServiceAccount{
							ObjectMeta: metav1.ObjectMeta{
								GenerateName: "sa-",
								Namespace:    defaultNamespace,
							},
						}
						Expect(k8sClient.Create(ctx, sa1)).To(Succeed())
						sa2 := &corev1.ServiceAccount{
							ObjectMeta: metav1.ObjectMeta{
								Name:      sa1.Name,
								Namespace: ns.Name,
							},
						}
						Expect(k8sClient.Create(ctx, sa2)).To(Succeed())
						_, err := controllerutil.CreateOrUpdate(ctx, k8sClient, sa2, func() error { return nil })
						Expect(err).To(Succeed())
						pvc2 := &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{
								GenerateName: "test-pvc",
								Namespace:    ns.Name,
							},
							Spec: corev1.PersistentVolumeClaimSpec{
								StorageClassName: vol.pvc.Spec.StorageClassName,
								AccessModes:      vol.pvc.Spec.AccessModes,
								Resources:        vol.pvc.Spec.Resources,
							},
						}
						Expect(k8sClient.Create(ctx, pvc2)).To(Succeed())
						vol2 := testVolume{pv: vol.pv, pvc: pvc2}

						pod1 := createPod(withPVC(vol.pvc), withServiceAccount(sa1.Name))
						pod1.schedule(testNode)

						expectedFields := defaultExpectedFields(testNode, vol.pv)
						expectedFields["AuthenticationSource"] = "pod"
						expectedFields["WorkloadNamespace"] = defaultNamespace
						expectedFields["WorkloadServiceAccountName"] = sa1.Name
						s3pa1 := waitForS3PodAttachmentWithFields(expectedFields, "")
						Expect(len(s3pa1.Spec.MountpointS3PodAttachments)).To(Equal(1))
						mpPod1 := waitAndVerifyMountpointPodFromPodAttachment(s3pa1, pod1, vol)

						pod2 := createPod(withPVC(pvc2), withServiceAccount(sa1.Name), withNamespace(ns.Name))
						vol2.bind()
						pod2.schedule(testNode)

						expectedFields["WorkloadNamespace"] = ns.Name
						s3pa2 := waitForS3PodAttachmentWithFields(expectedFields, "")
						Expect(len(s3pa2.Spec.MountpointS3PodAttachments)).To(Equal(1))
						mpPod2 := waitAndVerifyMountpointPodFromPodAttachment(s3pa2, pod2, vol)

						Expect(s3pa1.Name).NotTo(Equal(s3pa2.Name), "S3PodAttachment should not have the same name")
						Expect(mpPod1.Name).NotTo(Equal(mpPod2.Name), "Mountpoint Pods should not have the same name")
					})
				})

				Context("Mountpoint Pod Annotations", func() {
					It("should schedule a new Mountpoint Pod if existing Mountpoint Pod annotated as 'needs-unmount'", func() {
						vol := createVolume()
						vol.bind()

						pod1 := createPod(withPVC(vol.pvc))
						pod2 := createPod(withPVC(vol.pvc))
						pod3 := createPod(withPVC(vol.pvc))
						pod1.schedule(testNode)
						pod2.schedule(testNode)

						expectedFields := defaultExpectedFields(testNode, vol.pv)
						s3pa := waitForS3PodAttachmentWithFields(expectedFields, "")

						Expect(len(s3pa.Spec.MountpointS3PodAttachments)).To(Equal(1))
						mpPod1 := waitAndVerifyMountpointPodFromPodAttachment(s3pa, pod1, vol)
						mpPod2 := waitAndVerifyMountpointPodFromPodAttachment(s3pa, pod2, vol)
						Expect(mpPod1.Name).To(Equal(mpPod2.Name))

						// Now terminate the workloads for `mpPod1` (which is the same as `mpPod2`)
						pod1.terminate()
						pod2.terminate()

						// Wait until Mountpoint Pod annotated with "needs-unmount"
						waitForObject(mpPod1.Pod, func(g Gomega, pod *corev1.Pod) {
							g.Expect(pod.Annotations).To(HaveKeyWithValue(mppod.AnnotationNeedsUnmount, "true"))
						})

						// Schedule `pod3` to the same node
						pod3.schedule(testNode)

						// Wait until `pod3` is assigned
						s3pa = waitForS3PodAttachmentWithFields(expectedFields, s3pa.ResourceVersion, func(g Gomega, s3pa *crdv1beta.MountpointS3PodAttachment) {
							Expect(findMountpointPodNameForWorkload(s3pa, string(pod3.UID))).ToNot(BeEmpty())
						})

						// Verify `pod3` has been assigned to a new Mountpoint Pod
						mpPod3 := waitAndVerifyMountpointPodFromPodAttachment(s3pa, pod3, vol)
						Expect(mpPod1.Name).NotTo(Equal(mpPod3.Name))
					})

					It("should schedule a new Mountpoint Pod if existing Mountpoint Pod has been created by a different CSI Driver version", func() {
						vol := createVolume()
						vol.bind()

						pod1 := createPod(withPVC(vol.pvc))
						pod2 := createPod(withPVC(vol.pvc))
						pod3 := createPod(withPVC(vol.pvc))
						pod1.schedule(testNode)
						pod2.schedule(testNode)

						expectedFields := defaultExpectedFields(testNode, vol.pv)
						s3pa := waitForS3PodAttachmentWithFields(expectedFields, "")

						Expect(len(s3pa.Spec.MountpointS3PodAttachments)).To(Equal(1))
						mpPod1 := waitAndVerifyMountpointPodFromPodAttachment(s3pa, pod1, vol)
						mpPod2 := waitAndVerifyMountpointPodFromPodAttachment(s3pa, pod2, vol)
						Expect(mpPod1.Name).To(Equal(mpPod2.Name))

						// Now patch `mpPod1` as it was created with a different CSI Driver version
						differentCSIDriverVersion := "1.0.0.different"
						mpPod1.label(mppod.LabelCSIDriverVersion, differentCSIDriverVersion)

						// Schedule `pod3` to the same node
						pod3.schedule(testNode)

						// Wait until `pod3` is assigned
						s3pa = waitForS3PodAttachmentWithFields(expectedFields, s3pa.ResourceVersion, func(g Gomega, s3pa *crdv1beta.MountpointS3PodAttachment) {
							Expect(findMountpointPodNameForWorkload(s3pa, string(pod3.UID))).ToNot(BeEmpty())
						})
						Expect(len(s3pa.Spec.MountpointS3PodAttachments)).To(Equal(2))
						Expect(findMountpointPodNameForWorkload(s3pa, string(pod1.UID))).NotTo(BeEmpty())
						Expect(findMountpointPodNameForWorkload(s3pa, string(pod2.UID))).NotTo(BeEmpty())
						Expect(findMountpointPodNameForWorkload(s3pa, string(pod3.UID))).NotTo(BeEmpty())

						// Verify `pod3` has been assigned to a new Mountpoint Pod
						mpPod3 := waitAndVerifyMountpointPodFromPodAttachment(s3pa, pod3, vol)
						Expect(mpPod1.Name).NotTo(Equal(mpPod3.Name))
					})

					It("should schedule a new Mountpoint Pod if existing Mountpoint Pod annotated as 'no-new-workload'", func() {
						vol := createVolume()
						vol.bind()

						pod1 := createPod(withPVC(vol.pvc))
						pod2 := createPod(withPVC(vol.pvc))
						pod3 := createPod(withPVC(vol.pvc))
						pod1.schedule(testNode)
						pod2.schedule(testNode)

						expectedFields := defaultExpectedFields(testNode, vol.pv)
						s3pa := waitForS3PodAttachmentWithFields(expectedFields, "")

						Expect(len(s3pa.Spec.MountpointS3PodAttachments)).To(Equal(1))
						mpPod1 := waitAndVerifyMountpointPodFromPodAttachment(s3pa, pod1, vol)
						mpPod2 := waitAndVerifyMountpointPodFromPodAttachment(s3pa, pod2, vol)
						Expect(mpPod1.Name).To(Equal(mpPod2.Name))

						// Now annotate `mpPod1` with "no-new-workload"
						mpPod1.annotate(mppod.AnnotationNoNewWorkload, "true")

						// Schedule `pod3` to the same node
						pod3.schedule(testNode)

						// Wait until `pod3` is assigned
						s3pa = waitForS3PodAttachmentWithFields(expectedFields, s3pa.ResourceVersion, func(g Gomega, s3pa *crdv1beta.MountpointS3PodAttachment) {
							Expect(findMountpointPodNameForWorkload(s3pa, string(pod3.UID))).ToNot(BeEmpty())
						})
						Expect(len(s3pa.Spec.MountpointS3PodAttachments)).To(Equal(2))
						Expect(findMountpointPodNameForWorkload(s3pa, string(pod1.UID))).NotTo(BeEmpty())
						Expect(findMountpointPodNameForWorkload(s3pa, string(pod2.UID))).NotTo(BeEmpty())
						Expect(findMountpointPodNameForWorkload(s3pa, string(pod3.UID))).NotTo(BeEmpty())

						// Verify `pod3` has been assigned to a new Mountpoint Pod
						mpPod3 := waitAndVerifyMountpointPodFromPodAttachment(s3pa, pod3, vol)
						Expect(mpPod1.Name).NotTo(Equal(mpPod3.Name))
					})
				})
			})

			Context("Different Node", func() {
				Context("Pre-bound PV and PVC", func() {
					It("should schedule a Mountpoint Pod per Workload Pod", func() {
						vol := createVolume()
						vol.bind()

						pod1 := createPod(withPVC(vol.pvc))
						pod2 := createPod(withPVC(vol.pvc))

						expectNoS3PodAttachmentWithFields(map[string]string{"PersistentVolumeName": vol.pv.Name})

						pod1.schedule("test-node1")

						waitAndVerifyS3PodAttachmentAndMountpointPod("test-node1", vol, pod1)
						expectNoS3PodAttachmentWithFields(defaultExpectedFields("test-node2", vol.pv))

						pod2.schedule("test-node2")

						waitAndVerifyS3PodAttachmentAndMountpointPod("test-node2", vol, pod2)
					})
				})

				Context("Late PV and PVC binding", func() {
					It("should schedule a Mountpoint Pod per Workload Pod", func() {
						vol := createVolume()

						pod1 := createPod(withPVC(vol.pvc))
						pod2 := createPod(withPVC(vol.pvc))
						pod1.schedule("test-node1")

						expectNoS3PodAttachmentWithFields(map[string]string{"PersistentVolumeName": vol.pv.Name})

						vol.bind()

						waitAndVerifyS3PodAttachmentAndMountpointPod("test-node1", vol, pod1)
						expectNoS3PodAttachmentWithFields(defaultExpectedFields("test-node2", vol.pv))

						pod2.schedule("test-node2")

						waitAndVerifyS3PodAttachmentAndMountpointPod("test-node2", vol, pod2)
					})
				})
			})
		})
	})

	Context("Different Volume Types", func() {
		It("should not schedule a Mountpoint Pod if the Pod only uses an emptyDir volume", func() {
			pod := createPod(withVolume("empty-dir", corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			}))
			pod.schedule(testNode)

			expectNoS3PodAttachmentWithFields(map[string]string{"NodeName": testNode})
		})

		It("should not schedule a Mountpoint Pod if the Pod only uses a hostPath volume", func() {
			pod := createPod(withVolume("host-path", corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/tmp/mountpoint-test",
					Type: ptr.To(corev1.HostPathDirectoryOrCreate),
				},
			}))
			pod.schedule(testNode)

			expectNoS3PodAttachmentWithFields(map[string]string{"NodeName": testNode})
		})

		It("should not schedule a Mountpoint Pod if the Pod only different volume-types/CSI-drivers", func() {
			vol := createVolume(withCSIDriver(ebsCSIDriver))
			vol.bind()

			pod := createPod(
				withPVC(vol.pvc),
				withVolume("host-path", corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{
						Path: "/tmp/mountpoint-test",
						Type: ptr.To(corev1.HostPathDirectoryOrCreate),
					},
				}),
				withVolume("empty-dir", corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				}),
			)
			pod.schedule(testNode)

			expectNoS3PodAttachmentWithFields(map[string]string{"NodeName": testNode})
		})
	})

	Context("Mountpoint Pod Management", func() {
		It("should delete completed Mountpoint Pods", func() {
			vol := createVolume()
			vol.bind()

			pod := createPod(withPVC(vol.pvc))
			pod.schedule(testNode)

			_, mountpointPod := waitAndVerifyS3PodAttachmentAndMountpointPod(testNode, vol, pod)

			mountpointPod.succeed()

			waitForObjectToDisappear(mountpointPod.Pod)
		})

		It("should not schedule a Mountpoint Pod if the Workload Pod is terminating", func() {
			vol := createVolume()
			vol.bind()

			pod := createPod(withPVC(vol.pvc))
			pod.schedule(testNode)

			// `pod` got a `mountpointPod`
			s3pa, mountpointPod := waitAndVerifyS3PodAttachmentAndMountpointPod(testNode, vol, pod)

			// `mountpointPod` got terminated
			mountpointPod.succeed()
			waitForObjectToDisappear(mountpointPod.Pod)

			// `pod` got terminated
			pod.terminate()
			waitForObjectToDisappear(s3pa)

			// Since `pod` was in `Pending` state, termination of Pod will still keep that in
			// `Pending` state but will populate `DeletionTimestamp` to indicate this Pod is terminating.
			// In this case, there shouldn't be a new Mountpoint Pod spawned for it.
			expectNoS3PodAttachmentWithFields(defaultExpectedFields(testNode, vol.pv))
		})

		It("should delete S3 Pod Attachment if the Workload Pod is terminated", func() {
			vol := createVolume()
			vol.bind()

			pod := createPod(withPVC(vol.pvc))
			pod.schedule(testNode)

			// `pod` got a `mountpointPod`
			s3pa, _ := waitAndVerifyS3PodAttachmentAndMountpointPod(testNode, vol, pod)

			// `pod` got terminated
			pod.terminate()
			waitForObjectToDisappear(pod.Pod)
			waitForObjectToDisappear(s3pa)
		})
	})

	Context("Mountpoint Pod Customization", func() {
		It("should use configured service account name in PV", func() {
			sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
				Name:      "mount-s3-sa",
				Namespace: mountpointNamespace,
			}}
			Expect(k8sClient.Create(ctx, sa)).To(Succeed())
			waitForObject(sa)

			vol := createVolume(withVolumeAttributes(map[string]string{
				"mountpointPodServiceAccountName": sa.Name,
			}))
			vol.bind()

			pod := createPod(withPVC(vol.pvc))
			pod.schedule(testNode)

			_, mountpointPod := waitAndVerifyS3PodAttachmentAndMountpointPod(testNode, vol, pod)

			Expect(mountpointPod.Spec.ServiceAccountName).To(Equal(sa.Name))

			mountpointPod.succeed()
			waitForObjectToDisappear(mountpointPod.Pod)
		})

		It("should use configured resource requests and limits in PV", func() {
			vol := createVolume(withVolumeAttributes(map[string]string{
				"mountpointContainerResourcesRequestsCpu":    "1",
				"mountpointContainerResourcesRequestsMemory": "100Mi",
				"mountpointContainerResourcesLimitsCpu":      "2",
				"mountpointContainerResourcesLimitsMemory":   "200Mi",
			}))
			vol.bind()

			pod := createPod(withPVC(vol.pvc))
			pod.schedule(testNode)

			_, mountpointPod := waitAndVerifyS3PodAttachmentAndMountpointPod(testNode, vol, pod)

			mpContainer := mountpointPod.Spec.Containers[0]
			Expect(mpContainer.Resources.Requests).To(Equal(corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("100Mi"),
			}))
			Expect(mpContainer.Resources.Limits).To(Equal(corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("200Mi"),
			}))

			mountpointPod.succeed()
			waitForObjectToDisappear(mountpointPod.Pod)
		})
	})
})

//-- Utilities for tests.

// A testPod represents a Kubernetes Pod created for tests.
type testPod struct {
	*corev1.Pod
}

// schedule simulates `testPod` to be scheduled in given node.
func (p *testPod) schedule(name string) {
	binding := &corev1.Binding{Target: corev1.ObjectReference{Name: name}}
	Expect(k8sClient.SubResource("binding").Create(ctx, p.Pod, binding)).To(Succeed())

	waitForObject(p.Pod, func(g Gomega, pod *corev1.Pod) {
		g.Expect(pod.Spec.NodeName).To(Equal(name))
	})
}

// succeed simulates `testPod` to be succeeded running.
func (p *testPod) succeed() {
	p.Status.Phase = corev1.PodSucceeded
	Expect(k8sClient.Status().Update(ctx, p.Pod)).To(Succeed())

	waitForObject(p.Pod, func(g Gomega, pod *corev1.Pod) {
		g.Expect(pod.Status.Phase).To(Equal(corev1.PodSucceeded))
	})
}

// annotate adds given `key` and `value` as a annotation to `testPod`.
func (p *testPod) annotate(key, value string) {
	patch := client.MergeFrom(p.DeepCopy())
	if p.Annotations == nil {
		p.Annotations = make(map[string]string)
	}
	p.Annotations[key] = value

	Expect(k8sClient.Patch(ctx, p.Pod, patch)).To(Succeed())
	waitForObject(p.Pod, func(g Gomega, pod *corev1.Pod) {
		g.Expect(pod.Annotations).To(HaveKeyWithValue(key, value))
	})
}

// label adds given `key` and `value` as a label to `testPod`.
func (p *testPod) label(key, value string) {
	patch := client.MergeFrom(p.DeepCopy())
	if p.Labels == nil {
		p.Labels = make(map[string]string)
	}
	p.Labels[key] = value

	Expect(k8sClient.Patch(ctx, p.Pod, patch)).To(Succeed())
	waitForObject(p.Pod, func(g Gomega, pod *corev1.Pod) {
		g.Expect(pod.Labels).To(HaveKeyWithValue(key, value))
	})
}

// terminate simulates `testPod` to be terminating.
func (p *testPod) terminate() {
	Expect(k8sClient.Delete(ctx, p.Pod)).To(Succeed())
}

// withPVC returns a `podModifier` that adds given `pvc` to the Pods volumes.
func withPVC(pvc *corev1.PersistentVolumeClaim) podModifier {
	return func(pod *corev1.Pod) {
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: pvc.Name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvc.Name,
				},
			},
		})
	}
}

// withVolume returns a `podModifier` that adds given volume to the Pods volumes.
func withVolume(name string, vol corev1.VolumeSource) podModifier {
	return func(pod *corev1.Pod) {
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name:         name,
			VolumeSource: vol,
		})
	}
}

// withFSGroup returns a `podModifier` that sets fsGroup in the Pod's security context.
func withFSGroup(fsGroup int64) podModifier {
	return func(pod *corev1.Pod) {
		if pod.Spec.SecurityContext == nil {
			pod.Spec.SecurityContext = &corev1.PodSecurityContext{}
		}
		pod.Spec.SecurityContext.FSGroup = &fsGroup
	}
}

// withServiceAccount returns a `podModifier` that sets ServiceAccountName.
func withServiceAccount(saName string) podModifier {
	return func(pod *corev1.Pod) {
		pod.Spec.ServiceAccountName = saName
	}
}

// withNamespace returns a `podModifier` that sets Namespace.
func withNamespace(namespace string) podModifier {
	return func(pod *corev1.Pod) {
		pod.Namespace = namespace
	}
}

// A podModifier is a function for modifying Pod to be created.
type podModifier func(*corev1.Pod)

// createPod creates a new Kubernetes Pod in the control plane with given `modifiers`.
func createPod(modifiers ...podModifier) *testPod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:    defaultNamespace,
			GenerateName: "test-pod-",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "test-container",
					Image: defaultContainerImage,
				},
			},
		},
	}
	for _, m := range modifiers {
		m(pod)
	}

	Expect(k8sClient.Create(ctx, pod)).To(Succeed())
	waitForObject(pod)
	return &testPod{Pod: pod}
}

// A testVolume represents a volume (only PV and PVC as of today) created for tests.
type testVolume struct {
	pv  *corev1.PersistentVolume
	pvc *corev1.PersistentVolumeClaim
}

// bind bounds PV and PVC to each other.
func (v *testVolume) bind() {
	v.pv.Spec.ClaimRef = &corev1.ObjectReference{Name: v.pvc.Name, Namespace: v.pvc.Namespace}
	Expect(k8sClient.Update(ctx, v.pv)).To(Succeed())
	v.pv.Status.Phase = corev1.VolumeBound
	Expect(k8sClient.Status().Update(ctx, v.pv)).To(Succeed())

	v.pvc.Spec.VolumeName = v.pv.Name
	Expect(k8sClient.Update(ctx, v.pvc)).To(Succeed())
	v.pvc.Status.Phase = corev1.ClaimBound
	Expect(k8sClient.Status().Update(ctx, v.pvc)).To(Succeed())

	waitForObject(v.pv, func(g Gomega, pv *corev1.PersistentVolume) {
		g.Expect(pv.Status.Phase).To(Equal(corev1.VolumeBound))
	})

	waitForObject(v.pvc, func(g Gomega, pvc *corev1.PersistentVolumeClaim) {
		g.Expect(pvc.Status.Phase).To(Equal(corev1.ClaimBound))
	})
}

// A volumeModifier is a function for modifying a volume to be created.
type volumeModifier func(*testVolume)

// withCSIDriver returns a `volumeModifier` that updates volume to use given CSI Driver.
func withCSIDriver(name string) volumeModifier {
	return func(v *testVolume) {
		v.pv.Spec.PersistentVolumeSource.CSI.Driver = name
	}
}

// withVolumeAttributes returns a `volumeModifier` that updates volume to have given volume attributes.
func withVolumeAttributes(volAttributes map[string]string) volumeModifier {
	return func(v *testVolume) {
		v.pv.Spec.PersistentVolumeSource.CSI.VolumeAttributes = volAttributes
	}
}

// createVolume creates a new pair of unbounded PV and PVC.
func createVolume(modifiers ...volumeModifier) *testVolume {
	accessModes := []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}
	resources := corev1.ResourceList{
		corev1.ResourceStorage: resource.MustParse("10Gi"),
	}

	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-pv",
		},
		Spec: corev1.PersistentVolumeSpec{
			// Needs to be empty for static provisioning
			StorageClassName: "",
			AccessModes:      accessModes,
			Capacity:         resources,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       s3CSIDriver,
					VolumeHandle: "test-csi-volume",
				},
			},
		},
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-pvc",
			Namespace:    defaultNamespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: ptr.To(""),
			AccessModes:      accessModes,
			Resources:        corev1.VolumeResourceRequirements{Requests: resources},
		},
	}

	testVolume := &testVolume{pv: pv, pvc: pvc}

	for _, modifier := range modifiers {
		modifier(testVolume)
	}

	Expect(k8sClient.Create(ctx, pv)).To(Succeed(), "Failed to create PV")
	Expect(k8sClient.Create(ctx, pvc)).To(Succeed(), "Failed to create PVC")

	waitForObject(pv)
	waitForObject(pvc)

	return testVolume
}

// waitForMountpointPodWithName waits and returns the Mountpoint Pod scheduled for given `mpPodName`
func waitForMountpointPodWithName(mpPodName string) *testPod {
	mountpointPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      mpPodName,
			Namespace: mountpointNamespace,
		},
	}
	waitForObject(mountpointPod)
	return &testPod{Pod: mountpointPod}
}

// expectNoS3PodAttachmentWithFields verifies that no MountpointS3PodAttachment matching specified fields exists within a time period
func expectNoS3PodAttachmentWithFields(expectedFields map[string]string) {
	Consistently(func(g Gomega) {
		list := &crdv1beta.MountpointS3PodAttachmentList{}
		g.Expect(k8sClient.List(ctx, list)).To(Succeed())

		for i := range list.Items {
			cr := &list.Items[i]
			if matchesSpec(cr.Spec, expectedFields) {
				g.Expect(false).To(BeTrue(), "Found matching MountpointS3PodAttachment when none was expected: %#v", cr)
			}
		}
	}, defaultWaitTimeout/2, defaultWaitTimeout/4).Should(Succeed())
}

// expectNoPodUIDInS3PodAttachment validates that pod UID does not exist in MountpointS3PodAttachments map
func expectNoPodUIDInS3PodAttachment(s3pa *crdv1beta.MountpointS3PodAttachment, podUID string) {
	mpPodName := findMountpointPodNameForWorkload(s3pa, podUID)
	Expect(mpPodName).To(BeEmpty(), "Found pod UID %s in S3PodAttachment when none was expected: %#v", podUID, s3pa)
}

// waitAndVerifyS3PodAttachmentAndMountpointPodWithExpectedFields waits and verifies that MountpointS3PodAttachment and Mountpoint Pod
// are created for given `node`, `vol`, `pod` and `expectedFields`
func waitAndVerifyS3PodAttachmentAndMountpointPodWithExpectedFields(
	node string,
	vol *testVolume,
	pod *testPod,
	expectedFields map[string]string,
) (*crdv1beta.MountpointS3PodAttachment, *testPod) {
	s3pa := waitForS3PodAttachmentWithFields(expectedFields, "")
	Expect(len(s3pa.Spec.MountpointS3PodAttachments)).To(Equal(1))
	mpPod := waitAndVerifyMountpointPodFromPodAttachment(s3pa, pod, vol)
	return s3pa, mpPod
}

// waitAndVerifyS3PodAttachmentAndMountpointPod waits and verifies that MountpointS3PodAttachment and Mountpoint Pod
// are created for given `node`, `vol` and `pod`
func waitAndVerifyS3PodAttachmentAndMountpointPod(
	node string,
	vol *testVolume,
	pod *testPod,
) (*crdv1beta.MountpointS3PodAttachment, *testPod) {
	return waitAndVerifyS3PodAttachmentAndMountpointPodWithExpectedFields(node, vol, pod, defaultExpectedFields(node, vol.pv))
}

// waitAndVerifyS3PodAttachmentAndMountpointPodWithMinVersionAndExpectedField waits and verifies that MountpointS3PodAttachment with `minVersion` and Mountpoint Pod
// are created for given `node`, `vol`, `pod` and `expectedFields`
func waitAndVerifyS3PodAttachmentAndMountpointPodWithMinVersionAndExpectedField(
	testNode string,
	vol *testVolume,
	pod *testPod,
	minVersion string,
	expectedFields map[string]string,
) (*crdv1beta.MountpointS3PodAttachment, *testPod) {
	s3pa := waitForS3PodAttachmentWithFields(expectedFields, minVersion)
	Expect(len(s3pa.Spec.MountpointS3PodAttachments)).To(Equal(1))
	mpPod := waitAndVerifyMountpointPodFromPodAttachment(s3pa, pod, vol)
	return s3pa, mpPod
}

// waitAndVerifyS3PodAttachmentAndMountpointPod waits and verifies that MountpointS3PodAttachment with `minVersion` and Mountpoint Pod
// are created for given `node`, `vol` and `pod`
func waitAndVerifyS3PodAttachmentAndMountpointPodWithMinVersion(
	testNode string,
	vol *testVolume,
	pod *testPod,
	minVersion string,
) (*crdv1beta.MountpointS3PodAttachment, *testPod) {
	return waitAndVerifyS3PodAttachmentAndMountpointPodWithMinVersionAndExpectedField(testNode, vol, pod, minVersion, defaultExpectedFields(testNode, vol.pv))
}

// waitAndVerifyMountpointPodFromPodAttachment waits and verifies Mountpoint Pod scheduled for given `s3pa`, `pod` and `vol.`
func waitAndVerifyMountpointPodFromPodAttachment(s3pa *crdv1beta.MountpointS3PodAttachment, pod *testPod, vol *testVolume) *testPod {
	GinkgoHelper()

	podUID := string(pod.UID)
	// Wait until workload is assigned to `s3pa`
	waitForObject(s3pa, func(g Gomega, s3pa *crdv1beta.MountpointS3PodAttachment) {
		g.Expect(findMountpointPodNameForWorkload(s3pa, podUID)).ToNot(BeEmpty())
	})

	mpPodName := findMountpointPodNameForWorkload(s3pa, podUID)
	Expect(mpPodName).NotTo(BeEmpty(), "No Mountpoint Pod found for pod UID %s in MountpointS3PodAttachment: %#v", podUID, s3pa)
	Expect(s3pa.Spec.MountpointS3PodAttachments[mpPodName]).To(ContainElement(
		MatchFields(IgnoreExtras, Fields{
			"WorkloadPodUID": Equal(podUID),
			"AttachmentTime": Not(BeZero()),
		}),
	))

	mountpointPod := waitForMountpointPodWithName(mpPodName)
	verifyMountpointPodFor(pod, vol, mountpointPod)

	return mountpointPod
}

// findMountpointPodNameForWorkload tries to found Mountpoint Pod name that `workloadUID` is assigned to in given `s3pa`,
// it returns an empty string if not.
func findMountpointPodNameForWorkload(s3pa *crdv1beta.MountpointS3PodAttachment, workloadUID string) string {
	for mpPodName, attachments := range s3pa.Spec.MountpointS3PodAttachments {
		for _, attachment := range attachments {
			if attachment.WorkloadPodUID == workloadUID {
				return mpPodName
			}
		}
	}
	return ""
}

// verifyMountpointPodFor verifies given `mountpointPod` for given `pod` and `vol`.
func verifyMountpointPodFor(pod *testPod, vol *testVolume, mountpointPod *testPod) {
	GinkgoHelper()

	Expect(mountpointPod.ObjectMeta.Labels).To(HaveKeyWithValue(mppod.LabelMountpointVersion, mountpointVersion))
	Expect(mountpointPod.ObjectMeta.Labels).To(HaveKeyWithValue(mppod.LabelVolumeName, vol.pvc.Spec.VolumeName))
	Expect(mountpointPod.ObjectMeta.Labels).To(HaveKeyWithValue(mppod.LabelCSIDriverVersion, version.GetVersion().DriverVersion))

	Expect(mountpointPod.Spec.RestartPolicy).To(Equal(corev1.RestartPolicyOnFailure))

	Expect(mountpointPod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms).To(Equal([]corev1.NodeSelectorTerm{
		{
			MatchFields: []corev1.NodeSelectorRequirement{{
				Key:      metav1.ObjectNameField,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{pod.Spec.NodeName},
			}},
		},
	}))
	Expect(mountpointPod.Spec.Tolerations).To(Equal([]corev1.Toleration{
		{
			Operator: corev1.TolerationOpExists,
		},
	}))
	Expect(mountpointPod.Spec.PriorityClassName).To(Equal(mountpointPriorityClassName))

	Expect(mountpointPod.Spec.Containers[0].Image).To(Equal(mountpointImage))
	Expect(mountpointPod.Spec.Containers[0].ImagePullPolicy).To(Equal(mountpointImagePullPolicy))
	Expect(mountpointPod.Spec.Containers[0].Command).To(Equal([]string{mountpointContainerCommand}))
}

// waitForObject waits until `obj` appears in the control plane.
func waitForObject[Obj client.Object](obj Obj, verifiers ...func(Gomega, Obj)) {
	GinkgoHelper()

	key := types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
	Eventually(func(g Gomega) {
		g.Expect(k8sClient.Get(ctx, key, obj)).To(Succeed())
		for _, verifier := range verifiers {
			verifier(g, obj)
		}
	}, defaultWaitTimeout, defaultWaitRetryPeriod).Should(Succeed())
}

// waitForS3PodAttachmentWithFields waits until a MountpointS3PodAttachment matching specified node and pv appears in the cluster
func waitForS3PodAttachmentWithFields(
	expectedFields map[string]string,
	minResourceVersion string,
	verifiers ...func(Gomega, *crdv1beta.MountpointS3PodAttachment),
) *crdv1beta.MountpointS3PodAttachment {
	var matchedCR *crdv1beta.MountpointS3PodAttachment

	Eventually(func(g Gomega) {
		list := &crdv1beta.MountpointS3PodAttachmentList{}
		g.Expect(k8sClient.List(ctx, list)).To(Succeed())

		for i := range list.Items {
			cr := &list.Items[i]
			if matchesSpec(cr.Spec, expectedFields) {
				// Skip if the resource version isn't newer than the minimum
				if minResourceVersion != "" {
					minVersion, err := strconv.ParseInt(minResourceVersion, 10, 64)
					g.Expect(err).NotTo(HaveOccurred())

					currentVersion, err := strconv.ParseInt(cr.ResourceVersion, 10, 64)
					g.Expect(err).NotTo(HaveOccurred())

					if currentVersion <= minVersion {
						continue
					}
				}

				for _, verifier := range verifiers {
					verifier(g, cr)
				}
				matchedCR = cr
				return
			}
		}

		g.Expect(false).To(BeTrue(), "No matching MountpointS3PodAttachment found")
	}, defaultWaitTimeout, defaultWaitRetryPeriod).Should(Succeed())

	return matchedCR
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
func defaultExpectedFields(nodeName string, pv *corev1.PersistentVolume) map[string]string {
	return map[string]string{
		"NodeName":             nodeName,
		"PersistentVolumeName": pv.Name,
		"VolumeID":             pv.Spec.CSI.VolumeHandle,
		"MountOptions":         strings.Join(pv.Spec.MountOptions, ","),
		"AuthenticationSource": "driver",
		"WorkloadFSGroup":      "",
	}
}

// waitForObjectToDisappear waits until `obj` disappears in the control plane.
func waitForObjectToDisappear(obj client.Object) {
	key := types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
	Eventually(func(g Gomega) {
		err := k8sClient.Get(ctx, key, obj)
		if err == nil {
			g.Expect(obj.GetDeletionTimestamp()).ToNot(BeNil(), "Expected deletion timestamp to be non-nil: %#v", obj)
		} else {
			g.Expect(err).ToNot(BeNil(), "The object expected not to exists but its found: %#v", obj)
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "Expected not found error but fond: %v", err)
		}
	}, defaultWaitTimeout, defaultWaitRetryPeriod).Should(Succeed())
}

// generateRandomNodeName generates random node name
func generateRandomNodeName() string {
	return fmt.Sprintf("test-node-%s", uuid.New().String()[:8])
}
