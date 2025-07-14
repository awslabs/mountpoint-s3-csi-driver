package mppod

import (
	"crypto/sha256"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// Labels populated on spawned Headroom Pods.
const (
	LabelHeadroomForPod    = "experimental.s3.csi.aws.com/headroom-for-pod"
	LabelHeadroomForVolume = "experimental.s3.csi.aws.com/headroom-for-volume"
)

// Labels populated on Workload Pods requesting Headroom Pods.
const (
	LabelHeadroomForWorkload = "experimental.s3.csi.aws.com/headroom-for-workload"
)

// HeadroomPod returns a new Headroom Pod spec for the given `workloadPod` and `pv`.
// This Headroom Pod serves as a capacity headroom to allow scheduling of the Mountpoint Pod alongside `workloadPod` to provide volume for `pv`.
func (c *Creator) HeadroomPod(workloadPod *corev1.Pod, pv *corev1.PersistentVolume) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      HeadroomPodNameFor(workloadPod, pv),
			Namespace: c.config.Namespace,
			Labels: map[string]string{
				LabelHeadroomForPod:    string(workloadPod.UID),
				LabelHeadroomForVolume: pv.Name,
			},
		},
		Spec: corev1.PodSpec{
			PriorityClassName: c.config.HeadroomPriorityClassName,
			Affinity: &corev1.Affinity{
				// Specify inter-pod affinity rule to Workload Pod to
				// ensure they're co-scheduled into the same node.
				PodAffinity: &corev1.PodAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
						{
							LabelSelector: &metav1.LabelSelector{
								MatchExpressions: []metav1.LabelSelectorRequirement{
									{
										Key:      LabelHeadroomForWorkload,
										Operator: metav1.LabelSelectorOpIn,
										Values:   []string{string(workloadPod.UID)},
									},
								},
							},
							Namespaces:  []string{workloadPod.Namespace},
							TopologyKey: "kubernetes.io/hostname",
						},
					},
				},
			},
			Containers: []corev1.Container{
				{
					Name:  "pause",
					Image: c.config.Container.HeadroomImage,
					// TODO: Populate resources if PV specifies Mountpoint Pod resources.
					// Resources: corev1.ResourceRequirements{},
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: ptr.To(false),
						RunAsNonRoot:             ptr.To(true),
						Capabilities: &corev1.Capabilities{
							Drop: []corev1.Capability{"ALL"},
						},
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
				},
			},
			Tolerations: []corev1.Toleration{
				// Tolerate all taints, so this Headroom Pod would be scheduled to any node alongside the Workload Pod.
				{Operator: corev1.TolerationOpExists},
			},
		},
	}
}

// HeadroomPodNameFor returns a consistent name for the Headroom Pod for given `workloadPod` and `pv`.
func HeadroomPodNameFor(workloadPod *corev1.Pod, pv *corev1.PersistentVolume) string {
	return fmt.Sprintf("hr-%x", sha256.Sum224(fmt.Appendf(nil, "%s%s", workloadPod.UID, pv.Name)))
}
