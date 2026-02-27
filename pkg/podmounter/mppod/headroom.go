package mppod

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Labels populated on spawned Headroom Pods.
const (
	LabelHeadroomForPod    = "s3.csi.aws.com/headroom-for-pod"
	LabelHeadroomForVolume = "s3.csi.aws.com/headroom-for-volume"
)

// Labels populated on Workload Pods requesting Headroom Pods.
const (
	LabelHeadroomForWorkload = "s3.csi.aws.com/headroom-for-workload"
)

// A scheduling gate can be used on Workload Pods using a volume backed by the CSI Driver to signal the CSI Driver
// to reserve headroom for the Mountpoint Pod to serve volumes to workload.
//
// If this scheduling gate is used on a Workload Pod, the CSI Driver:
//  1. Labels the Workload Pod to use inter-pod affinity rules in the Headroom Pods
//  2. Creates Headroom Pods using a pause container with inter-pod affinity rule to the Workload Pod
//  3. Ungates the scheduling gate from the Workload Pod to let it scheduled - alongside the Headroom Pods if possible
//  4. Schedules Mountpoint Pod if necessary (i.e., the CSI Driver cannot share an existing Mountpoint Pod) into the same node as the Workload and Headroom Pods using a preempting priority class
//  5. Mountpoint Pod most likely preempts the Headroom Pods if there is no space in the node - as the Headroom Pods uses a negative priority -, or just gets scheduled if there is enough space for all pods
//  6. Deletes the Headroom Pods as soon as the Workload Pod is running or terminated - as Mountpoint Pods are already scheduled or no longer needed
const SchedulingGateReserveHeadroomForMountpointPod = "s3.csi.aws.com/reserve-headroom-for-mppod"

const headroomPodNamePrefix = "hr-"

// HeadroomPod returns a new Headroom Pod spec for the given `workloadPod` and `pv`.
// This Headroom Pod serves as a capacity headroom to allow scheduling of the Mountpoint Pod alongside `workloadPod` to provide volume for `pv`.
func (c *Creator) HeadroomPod(workloadPod *corev1.Pod, pv *corev1.PersistentVolume) (*corev1.Pod, error) {
	labels := maps.Clone(c.config.CustomLabels)
	maps.Copy(labels, c.config.PodLabels)
	labels[LabelHeadroomForPod] = string(workloadPod.UID)
	labels[LabelHeadroomForVolume] = pv.Name

	hrPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      HeadroomPodNameFor(workloadPod, pv),
			Namespace: c.config.Namespace,
			Labels:    labels,
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
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: new(false),
						RunAsNonRoot:             new(true),
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

	hrContainer := &hrPod.Spec.Containers[0]
	volumeAttributes := ExtractVolumeAttributes(pv)

	if err := c.configureResourceRequests(hrContainer, volumeAttributes); err != nil {
		return nil, err
	}
	if err := c.configureResourceLimits(hrContainer, volumeAttributes); err != nil {
		return nil, err
	}

	return hrPod, nil
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
	return slices.ContainsFunc(workloadPod.Spec.SchedulingGates, isHeadroomSchedulingGate)
}

// UngateHeadroomSchedulingGateForWorkloadPod removes the [SchedulingGateReserveHeadroomForMountpointPod] scheduling gate from the `workloadPod`.
func UngateHeadroomSchedulingGateForWorkloadPod(workloadPod *corev1.Pod) {
	workloadPod.Spec.SchedulingGates = slices.DeleteFunc(workloadPod.Spec.SchedulingGates, isHeadroomSchedulingGate)
}

// isHeadroomSchedulingGate returns whether the `sg` equals to [SchedulingGateReserveHeadroomForMountpointPod].
func isHeadroomSchedulingGate(sg corev1.PodSchedulingGate) bool {
	return sg.Name == SchedulingGateReserveHeadroomForMountpointPod
}
