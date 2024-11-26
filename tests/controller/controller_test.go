package controller_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/awslabs/aws-s3-csi-driver/pkg/podmounter/mppod"
)

var _ = Describe("Mountpoint Controller", func() {
	Context("Static Provisioning", func() {
		Context("Scheduled Pod with pre-bound PV and PVC", func() {
			It("should schedule a Mountpoint Pod", func() {
				vol := createVolume()
				vol.bind()

				pod := createPod(withPVC(vol.pvc))
				pod.schedule("test-node")

				waitAndVerifyMountpointPodFor(pod, vol)
			})

			It("should schedule a Mountpoint Pod per PV", func() {
				vol1 := createVolume()
				vol1.bind()
				vol2 := createVolume()
				vol2.bind()

				pod := createPod(withPVC(vol1.pvc), withPVC(vol2.pvc))
				pod.schedule("test-node")

				waitAndVerifyMountpointPodFor(pod, vol1)
				waitAndVerifyMountpointPodFor(pod, vol2)
			})

			It("should not schedule a Mountpoint Pod if the volume is backed by a different CSI driver", func() {
				vol := createVolume(withCSIDriver(ebsCSIDriver))
				vol.bind()

				pod := createPod(withPVC(vol.pvc))
				pod.schedule("test-node")

				expectNoMountpointPodFor(pod, vol)
			})

			It("should only schedule Mountpoint Pods for volumes backed by S3 CSI Driver", func() {
				vol1 := createVolume()
				vol1.bind()
				vol2 := createVolume(withCSIDriver(ebsCSIDriver))
				vol2.bind()

				pod := createPod(withPVC(vol1.pvc), withPVC(vol2.pvc))
				pod.schedule("test-node")

				waitAndVerifyMountpointPodFor(pod, vol1)
				expectNoMountpointPodFor(pod, vol2)
			})
		})

		Context("Scheduled Pod with late PV and PVC binding", func() {
			It("should schedule a Mountpoint Pod", func() {
				vol := createVolume()

				pod := createPod(withPVC(vol.pvc))
				pod.schedule("test-node")

				expectNoMountpointPodFor(pod, vol)

				vol.bind()

				waitAndVerifyMountpointPodFor(pod, vol)
			})

			It("should schedule a Mountpoint Pod per PV", func() {
				vol1 := createVolume()
				vol2 := createVolume()

				pod := createPod(withPVC(vol1.pvc), withPVC(vol2.pvc))
				pod.schedule("test-node")

				expectNoMountpointPodFor(pod, vol1)
				expectNoMountpointPodFor(pod, vol2)

				vol1.bind()

				waitAndVerifyMountpointPodFor(pod, vol1)
				expectNoMountpointPodFor(pod, vol2)

				vol2.bind()

				waitAndVerifyMountpointPodFor(pod, vol2)
			})

			It("should not schedule a Mountpoint Pod if the volume is backed by a different CSI driver", func() {
				vol := createVolume(withCSIDriver(ebsCSIDriver))

				pod := createPod(withPVC(vol.pvc))
				pod.schedule("test-node")

				expectNoMountpointPodFor(pod, vol)

				vol.bind()

				expectNoMountpointPodFor(pod, vol)
			})

			It("should only schedule Mountpoint Pods for volumes backed by S3 CSI Driver", func() {
				vol1 := createVolume()
				vol2 := createVolume(withCSIDriver(ebsCSIDriver))

				pod := createPod(withPVC(vol1.pvc), withPVC(vol2.pvc))
				pod.schedule("test-node")

				expectNoMountpointPodFor(pod, vol1)
				expectNoMountpointPodFor(pod, vol2)

				vol2.bind()

				expectNoMountpointPodFor(pod, vol1)
				expectNoMountpointPodFor(pod, vol2)

				vol1.bind()

				waitAndVerifyMountpointPodFor(pod, vol1)
				expectNoMountpointPodFor(pod, vol2)
			})
		})

		Context("Late scheduled Pod with pre-bound PV and PVC", func() {
			It("should schedule a Mountpoint Pod", func() {
				vol := createVolume()
				vol.bind()

				pod := createPod(withPVC(vol.pvc))

				expectNoMountpointPodFor(pod, vol)

				pod.schedule("test-node")

				waitAndVerifyMountpointPodFor(pod, vol)
			})

			It("should schedule a Mountpoint Pod per PV", func() {
				vol1 := createVolume()
				vol1.bind()
				vol2 := createVolume()
				vol2.bind()

				pod := createPod(withPVC(vol1.pvc), withPVC(vol2.pvc))

				expectNoMountpointPodFor(pod, vol1)
				expectNoMountpointPodFor(pod, vol2)

				pod.schedule("test-node")

				waitAndVerifyMountpointPodFor(pod, vol1)
				waitAndVerifyMountpointPodFor(pod, vol2)
			})

			It("should not schedule a Mountpoint Pod if the volume is backed by a different CSI driver", func() {
				vol := createVolume(withCSIDriver(ebsCSIDriver))
				vol.bind()

				pod := createPod(withPVC(vol.pvc))

				expectNoMountpointPodFor(pod, vol)

				pod.schedule("test-node")

				expectNoMountpointPodFor(pod, vol)
			})

			It("should only schedule Mountpoint Pods for volumes backed by S3 CSI Driver", func() {
				vol1 := createVolume()
				vol1.bind()
				vol2 := createVolume(withCSIDriver(ebsCSIDriver))
				vol2.bind()

				pod := createPod(withPVC(vol1.pvc), withPVC(vol2.pvc))

				expectNoMountpointPodFor(pod, vol1)
				expectNoMountpointPodFor(pod, vol2)

				pod.schedule("test-node")

				waitAndVerifyMountpointPodFor(pod, vol1)
				expectNoMountpointPodFor(pod, vol2)
			})
		})

		Context("Late scheduled Pod with late PV and PVC binding", func() {
			It("should schedule a Mountpoint Pod", func() {
				vol := createVolume()

				pod := createPod(withPVC(vol.pvc))

				expectNoMountpointPodFor(pod, vol)

				pod.schedule("test-node")

				expectNoMountpointPodFor(pod, vol)

				vol.bind()

				waitAndVerifyMountpointPodFor(pod, vol)
			})

			It("should schedule a Mountpoint Pod per PV", func() {
				vol1 := createVolume()
				vol2 := createVolume()

				pod := createPod(withPVC(vol1.pvc), withPVC(vol2.pvc))

				expectNoMountpointPodFor(pod, vol1)
				expectNoMountpointPodFor(pod, vol2)

				pod.schedule("test-node")
				vol2.bind()

				expectNoMountpointPodFor(pod, vol1)
				waitAndVerifyMountpointPodFor(pod, vol2)

				vol1.bind()

				waitAndVerifyMountpointPodFor(pod, vol1)
			})

			It("should not schedule a Mountpoint Pod if the volume is backed by a different CSI driver", func() {
				vol := createVolume(withCSIDriver(ebsCSIDriver))

				pod := createPod(withPVC(vol.pvc))

				expectNoMountpointPodFor(pod, vol)

				vol.bind()

				expectNoMountpointPodFor(pod, vol)

				pod.schedule("test-node")

				expectNoMountpointPodFor(pod, vol)
			})

			It("should only schedule Mountpoint Pods for volumes backed by S3 CSI Driver", func() {
				vol1 := createVolume()
				vol2 := createVolume(withCSIDriver(ebsCSIDriver))

				pod := createPod(withPVC(vol1.pvc), withPVC(vol2.pvc))

				expectNoMountpointPodFor(pod, vol1)
				expectNoMountpointPodFor(pod, vol2)

				vol1.bind()
				vol2.bind()
				pod.schedule("test-node")

				waitAndVerifyMountpointPodFor(pod, vol1)
				expectNoMountpointPodFor(pod, vol2)
			})
		})

		Context("Multiple Pods using the same PV and PVC", func() {
			Context("Same Node", func() {
				Context("Pre-bound PV and PVC", func() {
					It("should schedule a Mountpoint Pod per Workload Pod", func() {
						vol := createVolume()
						vol.bind()

						pod1 := createPod(withPVC(vol.pvc))
						pod2 := createPod(withPVC(vol.pvc))

						expectNoMountpointPodFor(pod1, vol)
						expectNoMountpointPodFor(pod2, vol)

						pod1.schedule("test-node")

						waitAndVerifyMountpointPodFor(pod1, vol)
						expectNoMountpointPodFor(pod2, vol)

						pod2.schedule("test-node")

						waitAndVerifyMountpointPodFor(pod2, vol)
					})
				})

				Context("Late PV and PVC binding", func() {
					It("should schedule a Mountpoint Pod per Workload Pod", func() {
						vol := createVolume()

						pod1 := createPod(withPVC(vol.pvc))
						pod2 := createPod(withPVC(vol.pvc))
						pod1.schedule("test-node")

						expectNoMountpointPodFor(pod1, vol)
						expectNoMountpointPodFor(pod2, vol)

						vol.bind()

						waitAndVerifyMountpointPodFor(pod1, vol)
						expectNoMountpointPodFor(pod2, vol)

						pod2.schedule("test-node")

						waitAndVerifyMountpointPodFor(pod2, vol)
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

						expectNoMountpointPodFor(pod1, vol)
						expectNoMountpointPodFor(pod2, vol)

						pod1.schedule("test-node1")

						waitAndVerifyMountpointPodFor(pod1, vol)
						expectNoMountpointPodFor(pod2, vol)

						pod2.schedule("test-node2")

						waitAndVerifyMountpointPodFor(pod2, vol)
					})
				})

				Context("Late PV and PVC binding", func() {
					It("should schedule a Mountpoint Pod per Workload Pod", func() {
						vol := createVolume()

						pod1 := createPod(withPVC(vol.pvc))
						pod2 := createPod(withPVC(vol.pvc))
						pod1.schedule("test-node1")

						expectNoMountpointPodFor(pod1, vol)
						expectNoMountpointPodFor(pod2, vol)

						vol.bind()

						waitAndVerifyMountpointPodFor(pod1, vol)
						expectNoMountpointPodFor(pod2, vol)

						pod2.schedule("test-node2")

						waitAndVerifyMountpointPodFor(pod2, vol)
					})
				})
			})
		})
	})

	Context("Mountpoint Pod Management", func() {
		It("should delete completed Mountpoint Pods", func() {
			vol := createVolume()
			vol.bind()

			pod := createPod(withPVC(vol.pvc))
			pod.schedule("test-node")

			mountpointPod := waitForMountpointPodFor(pod, vol)
			verifyMountpointPodFor(pod, vol, mountpointPod)

			mountpointPod.succeed()

			waitForObjectToDisappear(mountpointPod.Pod)
		})

		It("should not schedule a Mountpoint Pod if the Workload Pod is terminating", func() {
			vol := createVolume()
			vol.bind()

			pod := createPod(withPVC(vol.pvc))
			pod.schedule("test-node")

			// `pod` got a `mountpointPod`
			mountpointPod := waitForMountpointPodFor(pod, vol)
			verifyMountpointPodFor(pod, vol, mountpointPod)

			// `mountpointPod` got terminated
			mountpointPod.succeed()
			waitForObjectToDisappear(mountpointPod.Pod)

			// `pod` got terminated
			pod.terminate()

			// Since `pod` was in `Pending` state, termination of Pod will still keep that in
			// `Pending` state but will populate `DeletionTimestamp` to indicate this Pod is terminating.
			// In this case, there shouldn't be a new Mountpoint Pod spawned for it.
			expectNoMountpointPodFor(pod, vol)
		})

		It("should delete Mountpoint Pod if the Workload Pod is terminated", func() {
			vol := createVolume()
			vol.bind()

			pod := createPod(withPVC(vol.pvc))
			pod.schedule("test-node")

			// `pod` got a `mountpointPod`
			mountpointPod := waitForMountpointPodFor(pod, vol)
			verifyMountpointPodFor(pod, vol, mountpointPod)

			// `pod` got terminated
			pod.terminate()
			waitForObjectToDisappear(pod.Pod)

			// `mountpointPod` scheduled for `pod` should also get terminated
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

// waitForMountpointPodFor waits and returns the Mountpoint Pod scheduled for given `pod` and `vol`.
func waitForMountpointPodFor(pod *testPod, vol *testVolume) *testPod {
	mountpointPodKey := mountpointPodNameFor(pod, vol)
	mountpointPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      mountpointPodKey.Name,
			Namespace: mountpointPodKey.Namespace,
		},
	}
	waitForObject(mountpointPod)
	return &testPod{Pod: mountpointPod}
}

// expectNoMountpointPodFor verifies that there is no Mountpoint Pod scheduled for given `pod` and `vol`.
func expectNoMountpointPodFor(pod *testPod, vol *testVolume) {
	mountpointPodKey := mountpointPodNameFor(pod, vol)
	expectNoObject(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      mountpointPodKey.Name,
		Namespace: mountpointPodKey.Namespace,
	}})
}

// waitAndVerifyMountpointPodFor waits and verifies Mountpoint Pod scheduled for given `pod` and `vol.`
func waitAndVerifyMountpointPodFor(pod *testPod, vol *testVolume) {
	mountpointPod := waitForMountpointPodFor(pod, vol)
	verifyMountpointPodFor(pod, vol, mountpointPod)
}

// verifyMountpointPodFor verifies given `mountpointPod` for given `pod` and `vol`.
func verifyMountpointPodFor(pod *testPod, vol *testVolume, mountpointPod *testPod) {
	Expect(mountpointPod.ObjectMeta.Labels).To(HaveKeyWithValue(mppod.LabelVersion, mountpointVersion))
	Expect(mountpointPod.ObjectMeta.Labels).To(HaveKeyWithValue(mppod.LabelPodUID, string(pod.UID)))
	Expect(mountpointPod.ObjectMeta.Labels).To(HaveKeyWithValue(mppod.LabelVolumeName, vol.pvc.Spec.VolumeName))

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

	Expect(mountpointPod.Spec.Containers[0].Image).To(Equal(mountpointImage))
	Expect(mountpointPod.Spec.Containers[0].ImagePullPolicy).To(Equal(mountpointImagePullPolicy))
	Expect(mountpointPod.Spec.Containers[0].Command).To(Equal([]string{mountpointContainerCommand}))
}

// waitForObject waits until `obj` appears in the control plane.
func waitForObject[Obj client.Object](obj Obj, verifiers ...func(Gomega, Obj)) {
	key := types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
	Eventually(func(g Gomega) {
		g.Expect(k8sClient.Get(ctx, key, obj)).To(Succeed())
		for _, verifier := range verifiers {
			verifier(g, obj)
		}
	}, defaultWaitTimeout, defaultWaitRetryPeriod).Should(Succeed())
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

// expectNoObject verifies object with given key does not exists within a time period.
func expectNoObject(obj client.Object) {
	key := types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
	Consistently(func(g Gomega) {
		err := k8sClient.Get(ctx, key, obj)
		g.Expect(err).ToNot(BeNil(), "The object expected not to exists but its found: %#v", obj)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "Expected not found error but fond: %v", err)
	}, defaultWaitTimeout/2, defaultWaitTimeout/4).Should(Succeed())
}

// mountpointPodNameFor returns namespaced name of Mountpoint Pod for given `pod` and `vol`.
func mountpointPodNameFor(pod *testPod, vol *testVolume) types.NamespacedName {
	return types.NamespacedName{
		Name:      mppod.MountpointPodNameFor(string(pod.Pod.UID), vol.pvc.Spec.VolumeName),
		Namespace: mountpointNamespace,
	}
}
