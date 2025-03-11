package mppod

import (
	"fmt"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/awslabs/aws-s3-csi-driver/pkg/cluster"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/volumecontext"
)

// Labels populated on spawned Mountpoint Pods.
const (
	LabelMountpointVersion = "s3.csi.aws.com/mountpoint-version"
	LabelPodUID            = "s3.csi.aws.com/pod-uid"
	LabelVolumeName        = "s3.csi.aws.com/volume-name"
	LabelCSIDriverVersion  = "s3.csi.aws.com/mounted-by-csi-driver-version"
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
}

// NewCreator creates a new creator with the given `config`.
func NewCreator(config Config) *Creator {
	return &Creator{config: config}
}

// Create returns a new Mountpoint Pod spec to schedule for given `pod` and `pv`.
//
// It automatically assigns Mountpoint Pod to `pod`'s node.
// The name of the Mountpoint Pod is consistently generated from `pod` and `pv` using `MountpointPodNameFor` function.
func (c *Creator) Create(pod *corev1.Pod, pv *corev1.PersistentVolume) (*corev1.Pod, error) {
	node := pod.Spec.NodeName
	name := MountpointPodNameFor(string(pod.UID), pv.Name)

	mpPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: c.config.Namespace,
			Labels: map[string]string{
				LabelMountpointVersion: c.config.MountpointVersion,
				LabelPodUID:            string(pod.UID),
				LabelVolumeName:        pv.Name,
				LabelCSIDriverVersion:  c.config.CSIDriverVersion,
			},
		},
		Spec: corev1.PodSpec{
			// Mountpoint terminates with zero exit code on a successful termination,
			// and in turn `/bin/aws-s3-csi-mounter` also exits with Mountpoint process' exit code,
			// here `restartPolicy: OnFailure` allows Pod to only restart on non-zero exit codes (i.e. some failures)
			// and not successful exists (i.e. zero exit code).
			RestartPolicy: corev1.RestartPolicyOnFailure,
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
					RunAsUser:    c.config.ClusterVariant.MountpointPodUserID(),
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

	volumeAttributes := extractVolumeAttributes(pv)

	if saName := volumeAttributes[volumecontext.MountpointPodServiceAccountName]; saName != "" {
		mpPod.Spec.ServiceAccountName = saName
	}

	mpContainer := &mpPod.Spec.Containers[0]

	{
		resourceRequestsCpu := volumeAttributes[volumecontext.MountpointContainerResourcesRequestsCpu]
		resourceRequestsMemory := volumeAttributes[volumecontext.MountpointContainerResourcesRequestsMemory]

		if resourceRequestsCpu != "" || resourceRequestsMemory != "" {
			mpContainer.Resources.Requests = make(corev1.ResourceList)

			if resourceRequestsCpu != "" {
				quantity, err := resource.ParseQuantity(resourceRequestsCpu)
				if err != nil {
					return nil, failedToParseQuantityError(err, volumecontext.MountpointContainerResourcesRequestsCpu, resourceRequestsCpu)
				}
				mpContainer.Resources.Requests[corev1.ResourceCPU] = quantity
			}

			if resourceRequestsMemory != "" {
				quantity, err := resource.ParseQuantity(resourceRequestsMemory)
				if err != nil {
					return nil, failedToParseQuantityError(err, volumecontext.MountpointContainerResourcesRequestsMemory, resourceRequestsMemory)
				}
				mpContainer.Resources.Requests[corev1.ResourceMemory] = quantity
			}
		}
	}

	{
		resourceLimitsCpu := volumeAttributes[volumecontext.MountpointContainerResourcesLimitsCpu]
		resourceLimitsMemory := volumeAttributes[volumecontext.MountpointContainerResourcesLimitsMemory]

		if resourceLimitsCpu != "" || resourceLimitsMemory != "" {
			mpContainer.Resources.Limits = make(corev1.ResourceList)

			if resourceLimitsCpu != "" {
				quantity, err := resource.ParseQuantity(resourceLimitsCpu)
				if err != nil {
					return nil, failedToParseQuantityError(err, volumecontext.MountpointContainerResourcesLimitsCpu, resourceLimitsCpu)
				}
				mpContainer.Resources.Limits[corev1.ResourceCPU] = quantity
			}

			if resourceLimitsMemory != "" {
				quantity, err := resource.ParseQuantity(resourceLimitsMemory)
				if err != nil {
					return nil, failedToParseQuantityError(err, volumecontext.MountpointContainerResourcesLimitsMemory, resourceLimitsMemory)
				}
				mpContainer.Resources.Limits[corev1.ResourceMemory] = quantity
			}
		}
	}

	return mpPod, nil
}

// extractVolumeAttributes extracts volume attributes from given `pv`.
// It always returns a non-nil map, and it's safe to use even though `pv` doesn't contain any volume attributes.
func extractVolumeAttributes(pv *corev1.PersistentVolume) map[string]string {
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
