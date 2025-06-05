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
	namespace         = "mount-s3"
	mountpointVersion = "1.10.0"
	image             = "mp-image:latest"
	imagePullPolicy   = corev1.PullAlways
	command           = "/bin/aws-s3-csi-mounter"
	priorityClassName = "mount-s3-critical"
	testNode          = "test-node"
	testPodUID        = "test-pod-uid"
	testVolName       = "test-vol"
	testVolID         = "test-vol-id"
	csiDriverVersion  = "1.12.0"
)

func createTestConfig(clusterVariant cluster.Variant) mppod.Config {
	return mppod.Config{
		Namespace:         namespace,
		MountpointVersion: mountpointVersion,
		PriorityClassName: priorityClassName,
		Container: mppod.ContainerConfig{
			Image:           image,
			ImagePullPolicy: imagePullPolicy,
			Command:         command,
		},
		CSIDriverVersion: csiDriverVersion,
		ClusterVariant:   clusterVariant,
	}
}

func createAndVerifyPod(t *testing.T, clusterVariant cluster.Variant, expectedRunAsUser *int64) {
	creator := mppod.NewCreator(createTestConfig(clusterVariant), testr.New(t))

	verifyDefaultValues := func(mpPod *corev1.Pod) {
		assert.Equals(t, "mp-", mpPod.GenerateName)
		assert.Equals(t, "", mpPod.Name)
		assert.Equals(t, namespace, mpPod.Namespace)
		assert.Equals(t, map[string]string{
			mppod.LabelMountpointVersion: mountpointVersion,
			mppod.LabelCSIDriverVersion:  csiDriverVersion,
			mppod.LabelVolumeName:        testVolName,
			mppod.LabelVolumeId:          testVolID,
		}, mpPod.Labels)

		assert.Equals(t, priorityClassName, mpPod.Spec.PriorityClassName)
		assert.Equals(t, corev1.RestartPolicyOnFailure, mpPod.Spec.RestartPolicy)
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
		assert.Equals(t, ptr.To(false), mpPod.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation)
		assert.Equals(t, &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		}, mpPod.Spec.Containers[0].SecurityContext.Capabilities)
		assert.Equals(t, expectedRunAsUser, mpPod.Spec.Containers[0].SecurityContext.RunAsUser)
		assert.Equals(t, ptr.To(true), mpPod.Spec.Containers[0].SecurityContext.RunAsNonRoot)
		assert.Equals(t, &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		}, mpPod.Spec.Containers[0].SecurityContext.SeccompProfile)
		assert.Equals(t, &corev1.VolumeMount{
			Name:      mppod.CommunicationDirName,
			MountPath: "/" + mppod.CommunicationDirName,
		}, findVolumeMountFromContainer(mpPod.Spec.Containers[0], mppod.CommunicationDirName))
	}

	t.Run("Empty PV", func(t *testing.T) {
		mpPod, err := creator.Create(testNode, &corev1.PersistentVolume{
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
		})

		assert.NoError(t, err)
		verifyDefaultValues(mpPod)
	})

	t.Run("Mount Options", func(t *testing.T) {
		t.Run("With cache", func(t *testing.T) {
			mpPod, err := creator.Create(testNode, &corev1.PersistentVolume{
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
			})

			assert.NoError(t, err)
			verifyDefaultValues(mpPod)
			verifyLocalCacheVolume(t, mpPod, corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			})
		})
	})

	t.Run("Cache Configuration", func(t *testing.T) {
		t.Run("With emptyDir cache", func(t *testing.T) {
			mpPod, err := creator.Create(testNode, &corev1.PersistentVolume{
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
			})

			assert.NoError(t, err)
			verifyDefaultValues(mpPod)
			verifyLocalCacheVolume(t, mpPod, corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			})
		})

		t.Run("With emptyDir cache and size limit", func(t *testing.T) {
			sizeLimit := "1Gi"
			mpPod, err := creator.Create(testNode, &corev1.PersistentVolume{
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
			})

			assert.NoError(t, err)
			verifyDefaultValues(mpPod)
			verifyLocalCacheVolume(t, mpPod, corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					SizeLimit: ptr.To(resource.MustParse(sizeLimit)),
				},
			})
		})

		t.Run("With emptyDir cache and memory medium", func(t *testing.T) {
			mpPod, err := creator.Create(testNode, &corev1.PersistentVolume{
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
			})

			assert.NoError(t, err)
			verifyDefaultValues(mpPod)
			verifyLocalCacheVolume(t, mpPod, corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					Medium: corev1.StorageMediumMemory,
				},
			})
		})

		t.Run("With emptyDir cache, size limit and memory medium", func(t *testing.T) {
			sizeLimit := "1Gi"
			mpPod, err := creator.Create(testNode, &corev1.PersistentVolume{
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
			})

			assert.NoError(t, err)
			verifyDefaultValues(mpPod)
			verifyLocalCacheVolume(t, mpPod, corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					SizeLimit: ptr.To(resource.MustParse(sizeLimit)),
					Medium:    corev1.StorageMediumMemory,
				},
			})
		})

		t.Run("With ephemeral cache", func(t *testing.T) {
			scName := "test-cache-sc"
			storageRequest := "1Gi"
			mpPod, err := creator.Create(testNode, &corev1.PersistentVolume{
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
			})

			assert.NoError(t, err)
			verifyDefaultValues(mpPod)
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
			_, err := creator.Create(testNode, &corev1.PersistentVolume{
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
			})
			assert.Equals(t, cmpopts.AnyError, err)
		})

		t.Run("With ephemeral cache but missing resource request", func(t *testing.T) {
			_, err := creator.Create(testNode, &corev1.PersistentVolume{
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
			})
			assert.Equals(t, cmpopts.AnyError, err)
		})

		t.Run("With ephemeral cache but invalid resource request", func(t *testing.T) {
			_, err := creator.Create(testNode, &corev1.PersistentVolume{
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
			})
			assert.Equals(t, cmpopts.AnyError, err)
		})

		t.Run("With invalid cache type", func(t *testing.T) {
			_, err := creator.Create(testNode, &corev1.PersistentVolume{
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
			})
			assert.Equals(t, cmpopts.AnyError, err)
		})

		t.Run("With invalid emptyDir size limit", func(t *testing.T) {
			_, err := creator.Create(testNode, &corev1.PersistentVolume{
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
			})
			assert.Equals(t, cmpopts.AnyError, err)
		})

		t.Run("With invalid emptyDir medium", func(t *testing.T) {
			_, err := creator.Create(testNode, &corev1.PersistentVolume{
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
			})
			assert.Equals(t, cmpopts.AnyError, err)
		})

		t.Run("With both mount options cache and volume attributes cache", func(t *testing.T) {
			_, err := creator.Create(testNode, &corev1.PersistentVolume{
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
			})
			assert.Equals(t, cmpopts.AnyError, err)
		})
	})

	t.Run("With ServiceAccountName specified in PV", func(t *testing.T) {
		mpPod, err := creator.Create(testNode, &corev1.PersistentVolume{
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
		})

		assert.NoError(t, err)
		verifyDefaultValues(mpPod)
		assert.Equals(t, "mount-s3-sa", mpPod.Spec.ServiceAccountName)
	})

	t.Run("With Container Resources specified in PV", func(t *testing.T) {
		t.Run("With valid requests and limits", func(t *testing.T) {
			mpPod, err := creator.Create(testNode, &corev1.PersistentVolume{
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
			})

			assert.NoError(t, err)
			verifyDefaultValues(mpPod)
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
			mpPod, err := creator.Create(testNode, &corev1.PersistentVolume{
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
			})

			assert.NoError(t, err)
			verifyDefaultValues(mpPod)
			mpContainer := mpPod.Spec.Containers[0]
			assert.Equals(t, corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("100Mi"),
			}, mpContainer.Resources.Requests)
			assert.Equals(t, true, mpContainer.Resources.Limits.Cpu().IsZero())
			assert.Equals(t, true, mpContainer.Resources.Limits.Memory().IsZero())
		})

		t.Run("With valid limits only", func(t *testing.T) {
			mpPod, err := creator.Create(testNode, &corev1.PersistentVolume{
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
			})

			assert.NoError(t, err)
			verifyDefaultValues(mpPod)
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
					_, err := creator.Create(testNode, &corev1.PersistentVolume{
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
					})

					assert.Equals(t, cmpopts.AnyError, err)
				})
			}

		})
	})
}

func TestCreatingMountpointPods(t *testing.T) {
	createAndVerifyPod(t, cluster.DefaultKubernetes, ptr.To(int64(1000)))
}

func TestCreatingMountpointPodsInOpenShift(t *testing.T) {
	createAndVerifyPod(t, cluster.OpenShift, (*int64)(nil))
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
