package mppod_test

import (
	"path/filepath"
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/google/go-cmp/cmp/cmpopts"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/cluster"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/volumecontext"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/podmounter/mppod"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

const (
	namespace                   = "mount-s3"
	mountpointVersion           = "1.10.0"
	image                       = "mp-image:latest"
	headRoomImage               = "pause:latest"
	imagePullPolicy             = corev1.PullAlways
	command                     = "/bin/aws-s3-csi-mounter"
	priorityClassName           = "mount-s3-critical"
	preemptingPriorityClassName = "mount-s3-preempting-critical"
	headroomPriorityClassName   = "mount-s3-headroom"
	testNode                    = "test-node"
	testPodUID                  = "test-pod-uid"
	testVolName                 = "test-vol"
	testVolID                   = "test-vol-id"
	csiDriverVersion            = "1.12.0"
)

func TestCreatingMountpointPods(t *testing.T) {
	createAndVerifyPod(t, cluster.DefaultKubernetes, new(int64(1000)))
}

func TestCreatingMountpointPodsInOpenShift(t *testing.T) {
	createAndVerifyPod(t, cluster.OpenShift, (*int64)(nil))
}

func TestCreatingHeadroomPod(t *testing.T) {
	creator := mppod.NewCreator(createTestConfig(cluster.DefaultKubernetes), testr.New(t))

	workloadPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "workload-pod",
			Namespace: "workload-namespace",
			UID:       "2a1d7271-dc3a-416f-8b22-4eccba5c1373",
		},
	}

	t.Run("Basic HeadroomPod creation", func(t *testing.T) {
		pv := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{
				Name: testVolName,
			},
			Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{
						VolumeHandle: testVolID,
					},
				},
			},
		}

		hrPod, err := creator.HeadroomPod(workloadPod, pv)
		assert.NoError(t, err)

		assert.Equals(t, "hr-f050b11ab3ce10843f3404c1f46407320ed07ab7b35f9ba40c3792e2", hrPod.Name)
		assert.Equals(t, namespace, hrPod.Namespace)
		assert.Equals(t, map[string]string{
			mppod.LabelHeadroomForPod:    string(workloadPod.UID),
			mppod.LabelHeadroomForVolume: pv.Name,
		}, hrPod.Labels)
		assert.Equals(t, headroomPriorityClassName, hrPod.Spec.PriorityClassName)
		assert.Equals(t, []corev1.PodAffinityTerm{
			{
				LabelSelector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{
						{
							Key:      mppod.LabelHeadroomForWorkload,
							Operator: metav1.LabelSelectorOpIn,
							Values:   []string{string(workloadPod.UID)},
						},
					},
				},
				Namespaces:  []string{workloadPod.Namespace},
				TopologyKey: "kubernetes.io/hostname",
			},
		}, hrPod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution)
		assert.Equals(t, []corev1.Toleration{
			{Operator: corev1.TolerationOpExists},
		}, hrPod.Spec.Tolerations)

		assert.Equals(t, 1, len(hrPod.Spec.Containers))
		assert.Equals(t, "pause", hrPod.Spec.Containers[0].Name)
		assert.Equals(t, headRoomImage, hrPod.Spec.Containers[0].Image)

		assert.Equals(t, new(false), hrPod.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation)
		assert.Equals(t, &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		}, hrPod.Spec.Containers[0].SecurityContext.Capabilities)
		assert.Equals(t, new(true), hrPod.Spec.Containers[0].SecurityContext.RunAsNonRoot)
		assert.Equals(t, &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		}, hrPod.Spec.Containers[0].SecurityContext.SeccompProfile)

		// Verify no resources are set by default
		hrContainer := hrPod.Spec.Containers[0]
		assert.Equals(t, true, hrContainer.Resources.Requests.Cpu().IsZero())
		assert.Equals(t, true, hrContainer.Resources.Requests.Memory().IsZero())
		assert.Equals(t, true, hrContainer.Resources.Limits.Cpu().IsZero())
		assert.Equals(t, true, hrContainer.Resources.Limits.Memory().IsZero())
	})

	t.Run("With Container Resources specified in PV", func(t *testing.T) {
		t.Run("With valid requests and limits", func(t *testing.T) {
			pv := &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: testVolName,
				},
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						CSI: &corev1.CSIPersistentVolumeSource{
							VolumeHandle: testVolID,
							VolumeAttributes: map[string]string{
								volumecontext.MountpointContainerResourcesRequestsCpu:    "500m",
								volumecontext.MountpointContainerResourcesRequestsMemory: "128Mi",
								volumecontext.MountpointContainerResourcesLimitsCpu:      "1",
								volumecontext.MountpointContainerResourcesLimitsMemory:   "256Mi",
							},
						},
					},
				},
			}

			hrPod, err := creator.HeadroomPod(workloadPod, pv)
			assert.NoError(t, err)

			hrContainer := hrPod.Spec.Containers[0]
			assert.Equals(t, corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			}, hrContainer.Resources.Requests)
			assert.Equals(t, corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			}, hrContainer.Resources.Limits)
		})

		t.Run("With only requests", func(t *testing.T) {
			pv := &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: testVolName,
				},
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						CSI: &corev1.CSIPersistentVolumeSource{
							VolumeHandle: testVolID,
							VolumeAttributes: map[string]string{
								volumecontext.MountpointContainerResourcesRequestsCpu:    "250m",
								volumecontext.MountpointContainerResourcesRequestsMemory: "64Mi",
							},
						},
					},
				},
			}

			hrPod, err := creator.HeadroomPod(workloadPod, pv)
			assert.NoError(t, err)

			hrContainer := hrPod.Spec.Containers[0]
			assert.Equals(t, corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("250m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			}, hrContainer.Resources.Requests)
			assert.Equals(t, true, hrContainer.Resources.Limits.Cpu().IsZero())
			assert.Equals(t, true, hrContainer.Resources.Limits.Memory().IsZero())
		})

		t.Run("With only limits", func(t *testing.T) {
			pv := &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: testVolName,
				},
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						CSI: &corev1.CSIPersistentVolumeSource{
							VolumeHandle: testVolID,
							VolumeAttributes: map[string]string{
								volumecontext.MountpointContainerResourcesLimitsCpu:    "2",
								volumecontext.MountpointContainerResourcesLimitsMemory: "512Mi",
							},
						},
					},
				},
			}

			hrPod, err := creator.HeadroomPod(workloadPod, pv)
			assert.NoError(t, err)

			hrContainer := hrPod.Spec.Containers[0]
			assert.Equals(t, true, hrContainer.Resources.Requests.Cpu().IsZero())
			assert.Equals(t, true, hrContainer.Resources.Requests.Memory().IsZero())
			assert.Equals(t, corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			}, hrContainer.Resources.Limits)
		})

		t.Run("With invalid resource values", func(t *testing.T) {
			for name, volumeAttributes := range map[string]map[string]string{
				"invalid CPU request": {
					volumecontext.MountpointContainerResourcesRequestsCpu:    "invalid",
					volumecontext.MountpointContainerResourcesRequestsMemory: "128Mi",
				},
				"invalid memory request": {
					volumecontext.MountpointContainerResourcesRequestsCpu:    "500m",
					volumecontext.MountpointContainerResourcesRequestsMemory: "invalid",
				},
				"invalid CPU limit": {
					volumecontext.MountpointContainerResourcesLimitsCpu:    "invalid",
					volumecontext.MountpointContainerResourcesLimitsMemory: "256Mi",
				},
				"invalid memory limit": {
					volumecontext.MountpointContainerResourcesLimitsCpu:    "1",
					volumecontext.MountpointContainerResourcesLimitsMemory: "invalid",
				},
			} {
				t.Run(name, func(t *testing.T) {
					pv := &corev1.PersistentVolume{
						ObjectMeta: metav1.ObjectMeta{
							Name: testVolName,
						},
						Spec: corev1.PersistentVolumeSpec{
							PersistentVolumeSource: corev1.PersistentVolumeSource{
								CSI: &corev1.CSIPersistentVolumeSource{
									VolumeHandle:     testVolID,
									VolumeAttributes: volumeAttributes,
								},
							},
						},
					}

					_, err := creator.HeadroomPod(workloadPod, pv)
					assert.Equals(t, true, err != nil)
				})
			}
		})
	})
}

func createTestConfig(clusterVariant cluster.Variant) mppod.Config {
	return mppod.Config{
		Namespace:                   namespace,
		MountpointVersion:           mountpointVersion,
		PriorityClassName:           priorityClassName,
		PreemptingPriorityClassName: preemptingPriorityClassName,
		HeadroomPriorityClassName:   headroomPriorityClassName,
		Container: mppod.ContainerConfig{
			Image:           image,
			HeadroomImage:   headRoomImage,
			ImagePullPolicy: imagePullPolicy,
			Command:         command,
		},
		CSIDriverVersion: csiDriverVersion,
		ClusterVariant:   clusterVariant,
		CustomLabels:     map[string]string{},
		PodLabels:        map[string]string{},
	}
}

func createAndVerifyPod(t *testing.T, clusterVariant cluster.Variant, expectedRunAsUser *int64) {
	creator := mppod.NewCreator(createTestConfig(clusterVariant), testr.New(t))

	verifyDefaultValues := func(mpPod *corev1.Pod, expectedPriorityClassName string) {
		assert.Equals(t, "mp-", mpPod.GenerateName)
		assert.Equals(t, "", mpPod.Name)
		assert.Equals(t, namespace, mpPod.Namespace)
		assert.Equals(t, map[string]string{
			mppod.LabelMountpointVersion: mountpointVersion,
			mppod.LabelCSIDriverVersion:  csiDriverVersion,
		}, mpPod.Labels)
		assert.Equals(t, map[string]string{
			mppod.AnnotationVolumeName: testVolName,
			mppod.AnnotationVolumeId:   testVolID,
		}, mpPod.Annotations)

		assert.Equals(t, expectedPriorityClassName, mpPod.Spec.PriorityClassName)
		assert.Equals(t, corev1.RestartPolicyOnFailure, mpPod.Spec.RestartPolicy)
		assert.Equals(t, ptr.To(int64(mppod.TerminationGracePeriodSeconds)), mpPod.Spec.TerminationGracePeriodSeconds)
		assert.Equals(t, expectedRunAsUser, mpPod.Spec.SecurityContext.FSGroup)
		assert.Equals(t, &corev1.Volume{
			Name: mppod.CommunicationDirName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					Medium:    corev1.StorageMediumMemory,
					SizeLimit: resource.NewQuantity(mppod.CommunicationDirSizeLimit, resource.BinarySI),
				},
			},
		}, findVolumeFromPod(mpPod, mppod.CommunicationDirName))
		assert.Equals(t, &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{
						{
							MatchFields: []corev1.NodeSelectorRequirement{{
								Key:      metav1.ObjectNameField,
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{testNode},
							}},
						},
					},
				},
			},
		}, mpPod.Spec.Affinity)
		assert.Equals(t, []corev1.Toleration{
			{Operator: corev1.TolerationOpExists},
		}, mpPod.Spec.Tolerations)

		assert.Equals(t, 1, len(mpPod.Spec.Containers))
		assert.Equals(t, image, mpPod.Spec.Containers[0].Image)
		assert.Equals(t, imagePullPolicy, mpPod.Spec.Containers[0].ImagePullPolicy)
		assert.Equals(t, []string{command}, mpPod.Spec.Containers[0].Command)
		assert.Equals(t, new(false), mpPod.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation)
		assert.Equals(t, &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		}, mpPod.Spec.Containers[0].SecurityContext.Capabilities)
		assert.Equals(t, expectedRunAsUser, mpPod.Spec.Containers[0].SecurityContext.RunAsUser)
		assert.Equals(t, new(true), mpPod.Spec.Containers[0].SecurityContext.RunAsNonRoot)
		assert.Equals(t, &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		}, mpPod.Spec.Containers[0].SecurityContext.SeccompProfile)
		assert.Equals(t, &corev1.VolumeMount{
			Name:      mppod.CommunicationDirName,
			MountPath: "/" + mppod.CommunicationDirName,
		}, findVolumeMountFromContainer(mpPod.Spec.Containers[0], mppod.CommunicationDirName))
	}

	t.Run("Empty PV", func(t *testing.T) {
		mpPod, err := creator.MountpointPod(testNode, &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{
				Name: testVolName,
			},
			Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{
						VolumeHandle: testVolID,
					},
				},
			},
		}, mppod.DefaultPriorityClass)

		assert.NoError(t, err)
		verifyDefaultValues(mpPod, priorityClassName)
	})

	t.Run("Mount Options", func(t *testing.T) {
		t.Run("With cache", func(t *testing.T) {
			mpPod, err := creator.MountpointPod(testNode, &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: testVolName,
				},
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						CSI: &corev1.CSIPersistentVolumeSource{
							VolumeHandle: testVolID,
						},
					},
					MountOptions: []string{
						"cache /mnt/mp-cache",
					},
				},
			}, mppod.DefaultPriorityClass)

			assert.NoError(t, err)
			verifyDefaultValues(mpPod, priorityClassName)
			verifyLocalCacheVolume(t, mpPod, corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			})
		})
	})

	t.Run("Cache Configuration", func(t *testing.T) {
		t.Run("With emptyDir cache", func(t *testing.T) {
			mpPod, err := creator.MountpointPod(testNode, &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: testVolName,
				},
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						CSI: &corev1.CSIPersistentVolumeSource{
							VolumeHandle: testVolID,
							VolumeAttributes: map[string]string{
								volumecontext.Cache: "emptyDir",
							},
						},
					},
				},
			}, mppod.DefaultPriorityClass)

			assert.NoError(t, err)
			verifyDefaultValues(mpPod, priorityClassName)
			verifyLocalCacheVolume(t, mpPod, corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			})
		})

		t.Run("With emptyDir cache and size limit", func(t *testing.T) {
			sizeLimit := "1Gi"
			mpPod, err := creator.MountpointPod(testNode, &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: testVolName,
				},
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						CSI: &corev1.CSIPersistentVolumeSource{
							VolumeHandle: testVolID,
							VolumeAttributes: map[string]string{
								volumecontext.Cache:                  "emptyDir",
								volumecontext.CacheEmptyDirSizeLimit: sizeLimit,
							},
						},
					},
				},
			}, mppod.DefaultPriorityClass)

			assert.NoError(t, err)
			verifyDefaultValues(mpPod, priorityClassName)
			verifyLocalCacheVolume(t, mpPod, corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					SizeLimit: new(resource.MustParse(sizeLimit)),
				},
			})
		})

		t.Run("With emptyDir cache and memory medium", func(t *testing.T) {
			mpPod, err := creator.MountpointPod(testNode, &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: testVolName,
				},
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						CSI: &corev1.CSIPersistentVolumeSource{
							VolumeHandle: testVolID,
							VolumeAttributes: map[string]string{
								volumecontext.Cache:               "emptyDir",
								volumecontext.CacheEmptyDirMedium: "Memory",
							},
						},
					},
				},
			}, mppod.DefaultPriorityClass)

			assert.NoError(t, err)
			verifyDefaultValues(mpPod, priorityClassName)
			verifyLocalCacheVolume(t, mpPod, corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					Medium: corev1.StorageMediumMemory,
				},
			})
		})

		t.Run("With emptyDir cache, size limit and memory medium", func(t *testing.T) {
			sizeLimit := "1Gi"
			mpPod, err := creator.MountpointPod(testNode, &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: testVolName,
				},
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						CSI: &corev1.CSIPersistentVolumeSource{
							VolumeHandle: testVolID,
							VolumeAttributes: map[string]string{
								volumecontext.Cache:                  "emptyDir",
								volumecontext.CacheEmptyDirSizeLimit: sizeLimit,
								volumecontext.CacheEmptyDirMedium:    "Memory",
							},
						},
					},
				},
			}, mppod.DefaultPriorityClass)

			assert.NoError(t, err)
			verifyDefaultValues(mpPod, priorityClassName)
			verifyLocalCacheVolume(t, mpPod, corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					SizeLimit: new(resource.MustParse(sizeLimit)),
					Medium:    corev1.StorageMediumMemory,
				},
			})
		})

		t.Run("With ephemeral cache", func(t *testing.T) {
			scName := "test-cache-sc"
			storageRequest := "1Gi"
			mpPod, err := creator.MountpointPod(testNode, &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: testVolName,
				},
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						CSI: &corev1.CSIPersistentVolumeSource{
							VolumeHandle: testVolID,
							VolumeAttributes: map[string]string{
								volumecontext.Cache:                                "ephemeral",
								volumecontext.CacheEphemeralStorageClassName:       scName,
								volumecontext.CacheEphemeralStorageResourceRequest: storageRequest,
							},
						},
					},
				},
			}, mppod.DefaultPriorityClass)

			assert.NoError(t, err)
			verifyDefaultValues(mpPod, priorityClassName)
			verifyLocalCacheVolume(t, mpPod, corev1.VolumeSource{
				Ephemeral: &corev1.EphemeralVolumeSource{
					VolumeClaimTemplate: &corev1.PersistentVolumeClaimTemplate{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"s3.csi.aws.com/type": "local-ephemeral-cache",
							},
						},
						Spec: corev1.PersistentVolumeClaimSpec{
							AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
							StorageClassName: &scName,
							VolumeMode:       ptr.To(corev1.PersistentVolumeFilesystem),
							Resources: corev1.VolumeResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceStorage: resource.MustParse(storageRequest),
								},
							},
						},
					},
				},
			})
		})

		t.Run("With ephemeral cache but missing storage class name", func(t *testing.T) {
			_, err := creator.MountpointPod(testNode, &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: testVolName,
				},
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						CSI: &corev1.CSIPersistentVolumeSource{
							VolumeHandle: testVolID,
							VolumeAttributes: map[string]string{
								volumecontext.Cache: "ephemeral",
								volumecontext.CacheEphemeralStorageResourceRequest: "1Gi",
							},
						},
					},
				},
			}, mppod.DefaultPriorityClass)
			assert.Equals(t, cmpopts.AnyError, err)
		})

		t.Run("With ephemeral cache but missing resource request", func(t *testing.T) {
			_, err := creator.MountpointPod(testNode, &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: testVolName,
				},
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						CSI: &corev1.CSIPersistentVolumeSource{
							VolumeHandle: testVolID,
							VolumeAttributes: map[string]string{
								volumecontext.Cache:                          "ephemeral",
								volumecontext.CacheEphemeralStorageClassName: "test-sc",
							},
						},
					},
				},
			}, mppod.DefaultPriorityClass)
			assert.Equals(t, cmpopts.AnyError, err)
		})

		t.Run("With ephemeral cache but invalid resource request", func(t *testing.T) {
			_, err := creator.MountpointPod(testNode, &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: testVolName,
				},
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						CSI: &corev1.CSIPersistentVolumeSource{
							VolumeHandle: testVolID,
							VolumeAttributes: map[string]string{
								volumecontext.Cache:                                "ephemeral",
								volumecontext.CacheEphemeralStorageClassName:       "test-sc",
								volumecontext.CacheEphemeralStorageResourceRequest: "invalid",
							},
						},
					},
				},
			}, mppod.DefaultPriorityClass)
			assert.Equals(t, cmpopts.AnyError, err)
		})

		t.Run("With invalid cache type", func(t *testing.T) {
			_, err := creator.MountpointPod(testNode, &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: testVolName,
				},
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						CSI: &corev1.CSIPersistentVolumeSource{
							VolumeHandle: testVolID,
							VolumeAttributes: map[string]string{
								volumecontext.Cache: "invalid",
							},
						},
					},
				},
			}, mppod.DefaultPriorityClass)
			assert.Equals(t, cmpopts.AnyError, err)
		})

		t.Run("With invalid emptyDir size limit", func(t *testing.T) {
			_, err := creator.MountpointPod(testNode, &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: testVolName,
				},
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						CSI: &corev1.CSIPersistentVolumeSource{
							VolumeHandle: testVolID,
							VolumeAttributes: map[string]string{
								volumecontext.Cache:                  "emptyDir",
								volumecontext.CacheEmptyDirSizeLimit: "invalid",
							},
						},
					},
				},
			}, mppod.DefaultPriorityClass)
			assert.Equals(t, cmpopts.AnyError, err)
		})

		t.Run("With invalid emptyDir medium", func(t *testing.T) {
			_, err := creator.MountpointPod(testNode, &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: testVolName,
				},
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						CSI: &corev1.CSIPersistentVolumeSource{
							VolumeHandle: testVolID,
							VolumeAttributes: map[string]string{
								volumecontext.Cache:               "emptyDir",
								volumecontext.CacheEmptyDirMedium: "invalid",
							},
						},
					},
				},
			}, mppod.DefaultPriorityClass)
			assert.Equals(t, cmpopts.AnyError, err)
		})

		t.Run("With both mount options cache and volume attributes cache", func(t *testing.T) {
			_, err := creator.MountpointPod(testNode, &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: testVolName,
				},
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						CSI: &corev1.CSIPersistentVolumeSource{
							VolumeHandle: testVolID,
							VolumeAttributes: map[string]string{
								volumecontext.Cache: "emptyDir",
							},
						},
					},
					MountOptions: []string{
						"cache /mnt/mp-cache",
					},
				},
			}, mppod.DefaultPriorityClass)
			assert.Equals(t, cmpopts.AnyError, err)
		})
	})

	t.Run("With ServiceAccountName specified in PV", func(t *testing.T) {
		mpPod, err := creator.MountpointPod(testNode, &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{
				Name: testVolName,
			},
			Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{
						VolumeHandle: testVolID,
						VolumeAttributes: map[string]string{
							"mountpointPodServiceAccountName": "mount-s3-sa",
						},
					},
				},
			},
		}, mppod.DefaultPriorityClass)

		assert.NoError(t, err)
		verifyDefaultValues(mpPod, priorityClassName)
		assert.Equals(t, "mount-s3-sa", mpPod.Spec.ServiceAccountName)
	})

	t.Run("With Container Resources specified in PV", func(t *testing.T) {
		t.Run("With valid requests and limits", func(t *testing.T) {
			mpPod, err := creator.MountpointPod(testNode, &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: testVolName,
				},
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						CSI: &corev1.CSIPersistentVolumeSource{
							VolumeHandle: testVolID,
							VolumeAttributes: map[string]string{
								"mountpointContainerResourcesRequestsCpu":    "1",
								"mountpointContainerResourcesRequestsMemory": "100Mi",
								"mountpointContainerResourcesLimitsCpu":      "2",
								"mountpointContainerResourcesLimitsMemory":   "200Mi",
							},
						},
					},
				},
			}, mppod.DefaultPriorityClass)

			assert.NoError(t, err)
			verifyDefaultValues(mpPod, priorityClassName)
			mpContainer := mpPod.Spec.Containers[0]
			assert.Equals(t, corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("100Mi"),
			}, mpContainer.Resources.Requests)
			assert.Equals(t, corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("200Mi"),
			}, mpContainer.Resources.Limits)
		})

		t.Run("With valid requests only", func(t *testing.T) {
			mpPod, err := creator.MountpointPod(testNode, &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: testVolName,
				},
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						CSI: &corev1.CSIPersistentVolumeSource{
							VolumeHandle: testVolID,
							VolumeAttributes: map[string]string{
								"mountpointContainerResourcesRequestsCpu":    "1",
								"mountpointContainerResourcesRequestsMemory": "100Mi",
							},
						},
					},
				},
			}, mppod.DefaultPriorityClass)

			assert.NoError(t, err)
			verifyDefaultValues(mpPod, priorityClassName)
			mpContainer := mpPod.Spec.Containers[0]
			assert.Equals(t, corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("100Mi"),
			}, mpContainer.Resources.Requests)
			assert.Equals(t, true, mpContainer.Resources.Limits.Cpu().IsZero())
			assert.Equals(t, true, mpContainer.Resources.Limits.Memory().IsZero())
		})

		t.Run("With valid limits only", func(t *testing.T) {
			mpPod, err := creator.MountpointPod(testNode, &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: testVolName,
				},
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						CSI: &corev1.CSIPersistentVolumeSource{
							VolumeHandle: testVolID,
							VolumeAttributes: map[string]string{
								"mountpointContainerResourcesLimitsCpu":    "2",
								"mountpointContainerResourcesLimitsMemory": "200Mi",
							},
						},
					},
				},
			}, mppod.DefaultPriorityClass)

			assert.NoError(t, err)
			verifyDefaultValues(mpPod, priorityClassName)
			mpContainer := mpPod.Spec.Containers[0]
			assert.Equals(t, true, mpContainer.Resources.Requests.Cpu().IsZero())
			assert.Equals(t, true, mpContainer.Resources.Requests.Memory().IsZero())
			assert.Equals(t, corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("200Mi"),
			}, mpContainer.Resources.Limits)
		})

		t.Run("With invalid values", func(t *testing.T) {
			for name, volumeAttributes := range map[string]map[string]string{
				"mountpointContainerResourcesRequestsCpu": {
					"mountpointContainerResourcesRequestsCpu":    "invalid",
					"mountpointContainerResourcesRequestsMemory": "100Mi",
					"mountpointContainerResourcesLimitsCpu":      "2",
					"mountpointContainerResourcesLimitsMemory":   "200Mi",
				},
				"mountpointContainerResourcesRequestsMemory": {
					"mountpointContainerResourcesRequestsCpu":    "1",
					"mountpointContainerResourcesRequestsMemory": "invalid",
					"mountpointContainerResourcesLimitsCpu":      "2",
					"mountpointContainerResourcesLimitsMemory":   "200Mi",
				},
				"mountpointContainerResourcesLimitsCpu": {
					"mountpointContainerResourcesRequestsCpu":    "1",
					"mountpointContainerResourcesRequestsMemory": "100Mi",
					"mountpointContainerResourcesLimitsCpu":      "invalid",
					"mountpointContainerResourcesLimitsMemory":   "200Mi",
				},
				"mountpointContainerResourcesLimitsMemory": {
					"mountpointContainerResourcesRequestsCpu":    "1",
					"mountpointContainerResourcesRequestsMemory": "100Mi",
					"mountpointContainerResourcesLimitsCpu":      "2",
					"mountpointContainerResourcesLimitsMemory":   "invalid",
				},
			} {
				t.Run(name, func(t *testing.T) {
					_, err := creator.MountpointPod(testNode, &corev1.PersistentVolume{
						ObjectMeta: metav1.ObjectMeta{
							Name: testVolName,
						},
						Spec: corev1.PersistentVolumeSpec{
							PersistentVolumeSource: corev1.PersistentVolumeSource{
								CSI: &corev1.CSIPersistentVolumeSource{
									VolumeAttributes: volumeAttributes,
								},
							},
						},
					}, mppod.DefaultPriorityClass)

					assert.Equals(t, cmpopts.AnyError, err)
				})
			}

		})
	})

	t.Run("With Preempting Priority Class", func(t *testing.T) {
		mpPod, err := creator.MountpointPod(testNode, &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{
				Name: testVolName,
			},
			Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{
						VolumeHandle: testVolID,
					},
				},
			},
		}, mppod.PreemptingPriorityClass)

		assert.NoError(t, err)
		verifyDefaultValues(mpPod, preemptingPriorityClassName)
	})

}

func findVolumeMountFromContainer(container corev1.Container, name string) *corev1.VolumeMount {
	for _, vm := range container.VolumeMounts {
		if vm.Name == name {
			return &vm
		}
	}
	return nil
}

func findVolumeFromPod(pod *corev1.Pod, name string) *corev1.Volume {
	for _, v := range pod.Spec.Volumes {
		if v.Name == name {
			return &v
		}
	}
	return nil
}

func verifyLocalCacheVolume(t *testing.T, mpPod *corev1.Pod, expected corev1.VolumeSource) {
	cacheVol := findVolumeFromPod(mpPod, mppod.LocalCacheDirName)
	if cacheVol == nil {
		t.Fatalf("pod should have a cache volume with name %q", mppod.LocalCacheDirName)
	}

	assert.Equals(t, mppod.LocalCacheDirName, cacheVol.Name)
	assert.Equals(t, expected, cacheVol.VolumeSource)

	mpContainer := mpPod.Spec.Containers[0]
	cacheMount := findVolumeMountFromContainer(mpContainer, mppod.LocalCacheDirName)
	if cacheMount == nil {
		t.Fatalf("container should have a cache mount with name %q", mppod.LocalCacheDirName)
	}

	assert.Equals(t, &corev1.VolumeMount{
		Name:      mppod.LocalCacheDirName,
		MountPath: filepath.Join("/", mppod.LocalCacheDirName),
	}, cacheMount)
}
