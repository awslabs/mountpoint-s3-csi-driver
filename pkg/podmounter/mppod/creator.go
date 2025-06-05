package mppod

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/cluster"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/volumecontext"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
)

// Labels populated on spawned Mountpoint Pods.
const (
	LabelMountpointVersion = "s3.csi.aws.com/mountpoint-version"
	LabelVolumeName        = "s3.csi.aws.com/volume-name"
	LabelVolumeId          = "s3.csi.aws.com/volume-id"
	// LabelCSIDriverVersion specifies the CSI Driver's version used during creation of the Mountpoint Pod.
	// The controller checks this label against the current CSI Driver version before assigning a new workload to the Mountpoint Pod,
	// if they differ, the controller won't send new workload to the Mountpoint Pod and instead creates a new one.
	LabelCSIDriverVersion = "s3.csi.aws.com/mounted-by-csi-driver-version"
)

// Known list of annotations on Mountpoint Pods.
const (
	// AnnotationNeedsUnmount means the Mountpoint Pod scheduled for unmount.
	// Its the controller's responsibility to annotate a Mountpoint Pod as "needs-unmount" once
	// it has no workloads assigned to it. The controller ensures to not send new workload after the Mountpoint Pod
	// annotated with this annotation.
	// Its the node's responsibility to observe this annotation and perform unmount procedure for the Mountpoint Pod.
	AnnotationNeedsUnmount = "s3.csi.aws.com/needs-unmount"
	// AnnotationNoNewWorkload means the Mountpoint Pod shouldn't get a new workload assigned to it.
	// The existing workloads won't affected with this annotation, and would keep running until termination as per their regular lifecycle.
	// The controller ensures to not send new workload after the Mountpoint Pod annotated with this annotation.
	AnnotationNoNewWorkload = "s3.csi.aws.com/no-new-workload"
)

const (
	// cacheTypeEmptyDir creates a cache unique to the Mountpoint Pod by using `emptyDir` volume type.
	// See https://kubernetes.io/docs/concepts/storage/volumes/#emptydir.
	cacheTypeEmptyDir = "emptyDir"
	// cacheTypeEphemeral creates a generic ephemeral volume unique to the Mountpoint Pod by using `ephemeral` volume type.
	// See https://kubernetes.io/docs/concepts/storage/ephemeral-volumes/#generic-ephemeral-volumes.
	cacheTypeEphemeral = "ephemeral"
)

const CommunicationDirSizeLimit = 10 * 1024 * 1024 // 10MB

// A ContainerConfig represents configuration for containers in the spawned Mountpoint Pods.
type ContainerConfig struct {
	Command         string
	Image           string
	ImagePullPolicy corev1.PullPolicy
}

// A Config represents configuration for spawned Mountpoint Pods.
type Config struct {
	Namespace         string
	MountpointVersion string
	PriorityClassName string
	Container         ContainerConfig
	CSIDriverVersion  string
	ClusterVariant    cluster.Variant
}

// A Creator allows creating specification for Mountpoint Pods to schedule.
type Creator struct {
	config Config
	log    logr.Logger
}

// NewCreator creates a new creator with the given `config`.
func NewCreator(config Config, log logr.Logger) *Creator {
	return &Creator{config: config, log: log}
}

// Create returns a new Mountpoint Pod spec to schedule for given `node` and `pv`.
func (c *Creator) Create(node string, pv *corev1.PersistentVolume) (*corev1.Pod, error) {
	uid := c.config.ClusterVariant.MountpointPodUserID()
	mpPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "mp-",
			Namespace:    c.config.Namespace,
			Labels: map[string]string{
				LabelMountpointVersion: c.config.MountpointVersion,
				LabelVolumeName:        pv.Name,
				LabelVolumeId:          pv.Spec.CSI.VolumeHandle,
				LabelCSIDriverVersion:  c.config.CSIDriverVersion,
			},
		},
		Spec: corev1.PodSpec{
			// Mountpoint terminates with zero exit code on a successful termination,
			// and in turn `/bin/aws-s3-csi-mounter` also exits with Mountpoint process' exit code,
			// here `restartPolicy: OnFailure` allows Pod to only restart on non-zero exit codes (i.e. some failures)
			// and not successful exists (i.e. zero exit code).
			RestartPolicy: corev1.RestartPolicyOnFailure,
			SecurityContext: &corev1.PodSecurityContext{
				FSGroup: uid,
			},
			Containers: []corev1.Container{{
				Name:            "mountpoint",
				Image:           c.config.Container.Image,
				ImagePullPolicy: c.config.Container.ImagePullPolicy,
				Command:         []string{c.config.Container.Command},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr.To(false),
					Capabilities: &corev1.Capabilities{
						Drop: []corev1.Capability{"ALL"},
					},
					RunAsUser:    uid,
					RunAsNonRoot: ptr.To(true),
					SeccompProfile: &corev1.SeccompProfile{
						Type: corev1.SeccompProfileTypeRuntimeDefault,
					},
				},
				VolumeMounts: []corev1.VolumeMount{
					{
						Name:      CommunicationDirName,
						MountPath: filepath.Join("/", CommunicationDirName),
					},
				},
			}},
			PriorityClassName: c.config.PriorityClassName,
			Affinity: &corev1.Affinity{
				NodeAffinity: &corev1.NodeAffinity{
					// This is to making sure Mountpoint Pod gets scheduled into same node as the Workload Pod
					RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
						NodeSelectorTerms: []corev1.NodeSelectorTerm{
							{
								MatchFields: []corev1.NodeSelectorRequirement{{
									Key:      metav1.ObjectNameField,
									Operator: corev1.NodeSelectorOpIn,
									Values:   []string{node},
								}},
							},
						},
					},
				},
			},
			Tolerations: []corev1.Toleration{
				// Tolerate all taints.
				// - "NoScheduled" – If the Workload Pod gets scheduled to a node, Mountpoint Pod should also get
				//   scheduled into the same node to provide the volume.
				// - "NoExecute" – If the Workload Pod tolerates a "NoExecute" taint, Mountpoint Pod should also
				//   tolerate it to keep running and provide volume for the Workload Pod.
				//   If the Workload Pod would get descheduled and then the corresponding Mountpoint Pod
				//   would also get descheduled naturally due to CSI volume lifecycle.
				{Operator: corev1.TolerationOpExists},
			},
			Volumes: []corev1.Volume{
				// This emptyDir volume is used for communication between Mountpoint Pod and the CSI Driver Node Pod
				{
					Name: CommunicationDirName,
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{
							Medium:    corev1.StorageMediumMemory,
							SizeLimit: resource.NewQuantity(CommunicationDirSizeLimit, resource.BinarySI),
						},
					},
				},
			},
		},
	}

	mpContainer := &mpPod.Spec.Containers[0]
	volumeAttributes := ExtractVolumeAttributes(pv)
	mountpointArgs := mountpoint.ParseArgs(pv.Spec.MountOptions)

	if err := c.configureLocalCache(mpPod, mpContainer, mountpointArgs, volumeAttributes); err != nil {
		return nil, err
	}
	c.configureServiceAccount(mpPod, volumeAttributes)
	if err := c.configureResourceRequests(mpContainer, volumeAttributes); err != nil {
		return nil, err
	}
	if err := c.configureResourceLimits(mpContainer, volumeAttributes); err != nil {
		return nil, err
	}

	return mpPod, nil
}

// configureLocalCache configures necessary cache volumes for the pod and the container if its enabled.
func (c *Creator) configureLocalCache(mpPod *corev1.Pod, mpContainer *corev1.Container, args mountpoint.Args, volumeAttributes map[string]string) error {
	cacheEnabledViaOptions := args.Has(mountpoint.ArgCache)
	cacheType := volumeAttributes[volumecontext.Cache]
	if !cacheEnabledViaOptions && cacheType == "" {
		// Cache is not enabled
		return nil
	}

	if cacheEnabledViaOptions {
		if cacheType != "" {
			return errors.New("Cache configured with both `mountOptions` and `volumeAttributes`, please remove the deprecated cache configuration in `mountOptions`")
		}

		// TODO: Create and link `CACHING.md`.
		cacheType = cacheTypeEmptyDir
		c.log.Info("Configuring cache via `mountOptions` is deprecated, will fallback using `emptyDir`. We recommend setting `sizeLimit` on cache folders, please see CACHING.md for more details.")
	}

	var volumeSource corev1.VolumeSource
	var err error
	switch cacheType {
	case cacheTypeEmptyDir:
		volumeSource, err = c.createCacheVolumeSourceForEmptyDir(volumeAttributes)
	case cacheTypeEphemeral:
		volumeSource, err = c.createCacheVolumeSourceForEphemeral(volumeAttributes)
	default:
		return fmt.Errorf("unsupported local-cache type: %q, only %q and %q are supported", cacheType, cacheTypeEmptyDir, cacheTypeEphemeral)
	}
	if err != nil {
		return fmt.Errorf("failed to configure %q local-cache: %w", cacheType, err)
	}

	mpContainer.VolumeMounts = append(mpContainer.VolumeMounts, corev1.VolumeMount{
		Name:      LocalCacheDirName,
		MountPath: filepath.Join("/", LocalCacheDirName),
	})
	mpPod.Spec.Volumes = append(mpPod.Spec.Volumes, corev1.Volume{
		Name:         LocalCacheDirName,
		VolumeSource: volumeSource,
	})

	return nil
}

// createCacheVolumeSourceForEmptyDir creates an `emptyDir` volume source to use as local-cache.
func (c *Creator) createCacheVolumeSourceForEmptyDir(volumeAttributes map[string]string) (corev1.VolumeSource, error) {
	emptyDir := &corev1.EmptyDirVolumeSource{}

	if sizeLimit := volumeAttributes[volumecontext.CacheEmptyDirSizeLimit]; sizeLimit != "" {
		quantity, err := resource.ParseQuantity(sizeLimit)
		if err != nil {
			return corev1.VolumeSource{}, failedToParseQuantityError(err, volumecontext.CacheEmptyDirSizeLimit, sizeLimit)
		}
		emptyDir.SizeLimit = &quantity
	}

	if medium := volumeAttributes[volumecontext.CacheEmptyDirMedium]; medium != "" {
		switch medium {
		case string(corev1.StorageMediumMemory):
			emptyDir.Medium = corev1.StorageMediumMemory
		default:
			return corev1.VolumeSource{}, fmt.Errorf("unknown value for %q: %q. Only %q supported", volumecontext.CacheEmptyDirMedium, medium, corev1.StorageMediumMemory)
		}
	}

	return corev1.VolumeSource{EmptyDir: emptyDir}, nil
}

// createCacheVolumeSourceForEphemeral creates an `ephemeral` volume source to use as local-cache.
func (c *Creator) createCacheVolumeSourceForEphemeral(volumeAttributes map[string]string) (corev1.VolumeSource, error) {
	storageClassName := volumeAttributes[volumecontext.CacheEphemeralStorageClassName]
	if storageClassName == "" {
		return corev1.VolumeSource{}, fmt.Errorf("%q must be provided with %q cache type", volumecontext.CacheEphemeralStorageClassName, cacheTypeEphemeral)
	}

	storageResourceRequestStr := volumeAttributes[volumecontext.CacheEphemeralStorageResourceRequest]
	if storageResourceRequestStr == "" {
		return corev1.VolumeSource{}, fmt.Errorf("%q must be provided with %q cache type", volumecontext.CacheEphemeralStorageResourceRequest, cacheTypeEphemeral)
	}

	storageRequestSize, err := resource.ParseQuantity(storageResourceRequestStr)
	if err != nil {
		return corev1.VolumeSource{}, failedToParseQuantityError(err, volumecontext.CacheEphemeralStorageResourceRequest, storageResourceRequestStr)
	}

	return corev1.VolumeSource{
		Ephemeral: &corev1.EphemeralVolumeSource{
			VolumeClaimTemplate: &corev1.PersistentVolumeClaimTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"s3.csi.aws.com/type": "local-ephemeral-cache",
					},
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					StorageClassName: &storageClassName,
					VolumeMode:       ptr.To(corev1.PersistentVolumeFilesystem),
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: storageRequestSize,
						},
					},
				},
			},
		},
	}, nil
}

// configureServiceAccount configures service account of the pod if its specified in the volume attributes.
func (c *Creator) configureServiceAccount(mpPod *corev1.Pod, volumeAttributes map[string]string) {
	if saName := volumeAttributes[volumecontext.MountpointPodServiceAccountName]; saName != "" {
		mpPod.Spec.ServiceAccountName = saName
	}
}

// configureResourceRequests configures resource requests of the container if its specified in the volume attributes.
func (c *Creator) configureResourceRequests(mpContainer *corev1.Container, volumeAttributes map[string]string) error {
	resourceRequestsCpu := volumeAttributes[volumecontext.MountpointContainerResourcesRequestsCpu]
	resourceRequestsMemory := volumeAttributes[volumecontext.MountpointContainerResourcesRequestsMemory]

	if resourceRequestsCpu != "" || resourceRequestsMemory != "" {
		mpContainer.Resources.Requests = make(corev1.ResourceList)

		if resourceRequestsCpu != "" {
			quantity, err := resource.ParseQuantity(resourceRequestsCpu)
			if err != nil {
				return failedToParseQuantityError(err, volumecontext.MountpointContainerResourcesRequestsCpu, resourceRequestsCpu)
			}
			mpContainer.Resources.Requests[corev1.ResourceCPU] = quantity
		}

		if resourceRequestsMemory != "" {
			quantity, err := resource.ParseQuantity(resourceRequestsMemory)
			if err != nil {
				return failedToParseQuantityError(err, volumecontext.MountpointContainerResourcesRequestsMemory, resourceRequestsMemory)
			}
			mpContainer.Resources.Requests[corev1.ResourceMemory] = quantity
		}
	}

	return nil
}

// configureResourceLimits configures resource limits of the container if its specified in the volume attributes.
func (c *Creator) configureResourceLimits(mpContainer *corev1.Container, volumeAttributes map[string]string) error {
	resourceLimitsCpu := volumeAttributes[volumecontext.MountpointContainerResourcesLimitsCpu]
	resourceLimitsMemory := volumeAttributes[volumecontext.MountpointContainerResourcesLimitsMemory]

	if resourceLimitsCpu != "" || resourceLimitsMemory != "" {
		mpContainer.Resources.Limits = make(corev1.ResourceList)

		if resourceLimitsCpu != "" {
			quantity, err := resource.ParseQuantity(resourceLimitsCpu)
			if err != nil {
				return failedToParseQuantityError(err, volumecontext.MountpointContainerResourcesLimitsCpu, resourceLimitsCpu)
			}
			mpContainer.Resources.Limits[corev1.ResourceCPU] = quantity
		}

		if resourceLimitsMemory != "" {
			quantity, err := resource.ParseQuantity(resourceLimitsMemory)
			if err != nil {
				return failedToParseQuantityError(err, volumecontext.MountpointContainerResourcesLimitsMemory, resourceLimitsMemory)
			}
			mpContainer.Resources.Limits[corev1.ResourceMemory] = quantity
		}
	}

	return nil
}

// ExtractVolumeAttributes extracts volume attributes from given `pv`.
// It always returns a non-nil map, and it's safe to use even though `pv` doesn't contain any volume attributes.
func ExtractVolumeAttributes(pv *corev1.PersistentVolume) map[string]string {
	csiSpec := pv.Spec.CSI
	if csiSpec == nil {
		return map[string]string{}
	}

	volumeAttributes := csiSpec.VolumeAttributes
	if volumeAttributes == nil {
		return map[string]string{}
	}

	return volumeAttributes
}

// failedToParseQuantityError creates an error if provided quantity is not parsable.
func failedToParseQuantityError(err error, field, value string) error {
	return fmt.Errorf("failed to parse quantity %q for %q: %w", value, field, err)
}
