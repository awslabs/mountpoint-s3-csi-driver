package mppod

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"

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

// A scheduling gate can be used on Workload Pods using a volume backed by the CSI Driver to signal the CSI Driver
// to reserve headroom for the Mountpoint Pod to serve volumes to workload.
//
// If this scheduling gate is used on a Workload Pod, the CSI Driver:
//  1. Will label the Workload Pod with [LabelHeadroomForWorkload] equals to Workload Pod's UID
//  2. Will create a Headroom Pod using a pause container with inter-pod affinity to the Workload Pod
//  3. Will ungate this scheduling gate from the Workload Pod to let it scheduled (alongside the Headroom Pod)
//  4. Will schedule Mountpoint Pod if necessary (i.e., the CSI Driver cannot share an existing Mountpoint Pod)
//     into the same node as the Workload and Headroom Pods
//  5. Mountpoint Pod will replace the Headroom Pod if there is no space in the node
//  6. Once the Workload Pod is no longer in `Pending` state (i.e., either scheduled or terminated),
//     the Headroom Pod will be deleted by the CSI Driver
const SchedulingGateReserveHeadroomForMountpointPod = "experimental.s3.csi.aws.com/reserve-headroom-for-mppod"

const headroomPodNamePrefix = "hr-"

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
	return fmt.Sprintf("%s%x", headroomPodNamePrefix, sha256.Sum224(fmt.Appendf(nil, "%s%s", workloadPod.UID, pv.Name)))
}

// IsHeadroomPod returns whether given pod is a Headroom Pod.
//
// Note that, this function doesn't check the namespace, it's caller's responsibility to ensure
// the pod queried is in the correct namespace for Headroom Pods.
func IsHeadroomPod(pod *corev1.Pod) bool {
	return strings.HasPrefix(pod.Name, headroomPodNamePrefix)
}

// LabelWorkloadPodForHeadroomPod adds [LabelHeadroomForWorkload] label to the `workloadPod`
// in order to use in inter-pod affinity rules in the Headroom Pod.
//
// It returns whether label added.
func LabelWorkloadPodForHeadroomPod(workloadPod *corev1.Pod) bool {
	if WorkloadHasLabelPodForHeadroomPod(workloadPod) {
		return false
	}

	if workloadPod.Labels == nil {
		workloadPod.Labels = make(map[string]string)
	}
	workloadPod.Labels[LabelHeadroomForWorkload] = string(workloadPod.UID)
	return true
}

// WorkloadHasLabelPodForHeadroomPod returns whether the `workloadPod` is labelled with [LabelHeadroomForWorkload].
func WorkloadHasLabelPodForHeadroomPod(workloadPod *corev1.Pod) bool {
	return workloadPod.Labels != nil &&
		workloadPod.Labels[LabelHeadroomForWorkload] != ""
}

// UnlabelWorkloadPodForHeadroomPod removes [LabelHeadroomForWorkload] label from the `workloadPod`.
//
// It returns whether label removed.
func UnlabelWorkloadPodForHeadroomPod(workloadPod *corev1.Pod) bool {
	if !WorkloadHasLabelPodForHeadroomPod(workloadPod) {
		return false
	}

	delete(workloadPod.Labels, LabelHeadroomForWorkload)
	return true
}

// ShouldReserveHeadroomForMountpointPod returns whether the `workloadPod` wants to reserve headroom for a Mountpoint Pod.
func ShouldReserveHeadroomForMountpointPod(workloadPod *corev1.Pod) bool {
	return slices.ContainsFunc(workloadPod.Spec.SchedulingGates, isHeadroomScheduligGate)
}

// UngateHeadroomSchedulingGateForWorkloadPod removes the [SchedulingGateReserveHeadroomForMountpointPod] scheduling gate from the `workloadPod`.
func UngateHeadroomSchedulingGateForWorkloadPod(workloadPod *corev1.Pod) {
	workloadPod.Spec.SchedulingGates = slices.DeleteFunc(workloadPod.Spec.SchedulingGates, isHeadroomScheduligGate)
}

// isHeadroomScheduligGate returns whether the `sg` equals to [SchedulingGateReserveHeadroomForMountpointPod].
func isHeadroomScheduligGate(sg corev1.PodSchedulingGate) bool {
	return sg.Name == SchedulingGateReserveHeadroomForMountpointPod
}
