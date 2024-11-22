package mppod

import (
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// Labels populated on spawned Mountpoint Pods.
const (
	LabelVersion    = "s3.csi.aws.com/mountpoint-version"
	LabelPodUID     = "s3.csi.aws.com/pod-uid"
	LabelVolumeName = "s3.csi.aws.com/volume-name"
)

// A ContainerConfig represents configuration for containers in the spawned Mountpoint Pods.
type ContainerConfig struct {
	Command         string
	Image           string
	ImagePullPolicy corev1.PullPolicy
}

// A Config represents configuration for spawned Mountpoint Pods.
type Config struct {
	Namespace string
	Version   string
	Container ContainerConfig
}

// A Creator allows creating specification for Mountpoint Pods to schedule.
type Creator struct {
	config Config
}

// NewCreator creates a new creator with the given `config`.
func NewCreator(config Config) *Creator {
	return &Creator{config: config}
}

// Create returns a new Mountpoint Pod spec to schedule for given `pod` and `pvc`.
//
// It automatically assigns Mountpoint Pod to `pod`'s node.
// The name of the Mountpoint Pod is consistently generated from `pod` and `pvc` using `MountpointPodNameFor` function.
func (c *Creator) Create(pod *corev1.Pod, pvc *corev1.PersistentVolumeClaim) *corev1.Pod {
	node := pod.Spec.NodeName
	name := MountpointPodNameFor(string(pod.UID), pvc.Spec.VolumeName)

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: c.config.Namespace,
			Labels: map[string]string{
				LabelVersion:    c.config.Version,
				LabelPodUID:     string(pod.UID),
				LabelVolumeName: pvc.Spec.VolumeName,
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
				},
				VolumeMounts: []corev1.VolumeMount{
					{
						Name:      CommunicationDirName,
						MountPath: filepath.Join("/", CommunicationDirName),
					},
				},
			}},
			Affinity: &corev1.Affinity{
				NodeAffinity: &corev1.NodeAffinity{
					// This is to making sure Mountpoint Pod gets scheduled into same node as the Pod
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
			Volumes: []corev1.Volume{
				// This emptyDir volume is used for communication between Mountpoint Pod and the CSI Driver Node Pod
				{
					Name: CommunicationDirName,
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				},
			},
		},
	}
}
