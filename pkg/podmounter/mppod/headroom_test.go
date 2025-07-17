package mppod_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/podmounter/mppod"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

func TestShouldReserveHeadroomForMountpointPod(t *testing.T) {
	tests := []struct {
		name     string
		pod      *corev1.Pod
		expected bool
	}{
		{
			name: "pod with headroom scheduling gate",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					SchedulingGates: []corev1.PodSchedulingGate{
						{Name: mppod.SchedulingGateReserveHeadroomForMountpointPod},
					},
				},
			},
			expected: true,
		},
		{
			name: "pod with multiple scheduling gates including headroom gate",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					SchedulingGates: []corev1.PodSchedulingGate{
						{Name: "other-gate"},
						{Name: mppod.SchedulingGateReserveHeadroomForMountpointPod},
						{Name: "another-gate"},
					},
				},
			},
			expected: true,
		},
		{
			name: "pod without headroom scheduling gate",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					SchedulingGates: []corev1.PodSchedulingGate{
						{Name: "other-gate"},
						{Name: "another-gate"},
					},
				},
			},
			expected: false,
		},
		{
			name: "pod with no scheduling gates",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					SchedulingGates: []corev1.PodSchedulingGate{},
				},
			},
			expected: false,
		},
		{
			name: "pod with nil scheduling gates",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					SchedulingGates: nil,
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mppod.ShouldReserveHeadroomForMountpointPod(tt.pod)
			assert.Equals(t, tt.expected, result)
		})
	}
}

func TestUngateHeadroomSchedulingGateForWorkloadPod(t *testing.T) {
	tests := []struct {
		name               string
		pod                *corev1.Pod
		expectedGatesAfter []corev1.PodSchedulingGate
	}{
		{
			name: "remove headroom gate from single gate",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					SchedulingGates: []corev1.PodSchedulingGate{
						{Name: mppod.SchedulingGateReserveHeadroomForMountpointPod},
					},
				},
			},
			expectedGatesAfter: []corev1.PodSchedulingGate{},
		},
		{
			name: "remove headroom gate from multiple gates",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					SchedulingGates: []corev1.PodSchedulingGate{
						{Name: "first-gate"},
						{Name: mppod.SchedulingGateReserveHeadroomForMountpointPod},
						{Name: "last-gate"},
					},
				},
			},
			expectedGatesAfter: []corev1.PodSchedulingGate{
				{Name: "first-gate"},
				{Name: "last-gate"},
			},
		},
		{
			name: "no headroom gate to remove",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					SchedulingGates: []corev1.PodSchedulingGate{
						{Name: "first-gate"},
						{Name: "second-gate"},
					},
				},
			},
			expectedGatesAfter: []corev1.PodSchedulingGate{
				{Name: "first-gate"},
				{Name: "second-gate"},
			},
		},
		{
			name: "empty scheduling gates",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					SchedulingGates: []corev1.PodSchedulingGate{},
				},
			},
			expectedGatesAfter: []corev1.PodSchedulingGate{},
		},
		{
			name: "nil scheduling gates",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					SchedulingGates: nil,
				},
			},
			expectedGatesAfter: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mppod.UngateHeadroomSchedulingGateForWorkloadPod(tt.pod)
			assert.Equals(t, tt.expectedGatesAfter, tt.pod.Spec.SchedulingGates)
		})
	}
}

func TestLabelWorkloadPodForHeadroomPod(t *testing.T) {
	tests := []struct {
		name           string
		pod            *corev1.Pod
		expectedResult bool
		expectedLabel  string
	}{
		{
			name: "pod without labels",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					UID: "test-uid-123",
				},
			},
			expectedResult: true,
			expectedLabel:  "test-uid-123",
		},
		{
			name: "pod with existing labels but no headroom label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					UID: "test-uid-456",
					Labels: map[string]string{
						"app": "test-app",
					},
				},
			},
			expectedResult: true,
			expectedLabel:  "test-uid-456",
		},
		{
			name: "pod already has headroom label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					UID: "test-uid-789",
					Labels: map[string]string{
						mppod.LabelHeadroomForWorkload: "existing-value",
					},
				},
			},
			expectedResult: false,
			expectedLabel:  "existing-value",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mppod.LabelWorkloadPodForHeadroomPod(tt.pod)
			assert.Equals(t, tt.expectedResult, result)
			assert.Equals(t, tt.expectedLabel, tt.pod.Labels[mppod.LabelHeadroomForWorkload])
		})
	}
}

func TestWorkloadHasLabelPodForHeadroomPod(t *testing.T) {
	tests := []struct {
		name     string
		pod      *corev1.Pod
		expected bool
	}{
		{
			name: "pod with headroom label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						mppod.LabelHeadroomForWorkload: "test-uid",
					},
				},
			},
			expected: true,
		},
		{
			name: "pod without labels",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: nil,
				},
			},
			expected: false,
		},
		{
			name: "pod with other labels but no headroom label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "test-app",
					},
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mppod.WorkloadHasLabelPodForHeadroomPod(tt.pod)
			assert.Equals(t, tt.expected, result)
		})
	}
}

func TestUnlabelWorkloadPodForHeadroomPod(t *testing.T) {
	tests := []struct {
		name           string
		pod            *corev1.Pod
		expectedResult bool
		expectedLabels map[string]string
	}{
		{
			name: "pod with headroom label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						mppod.LabelHeadroomForWorkload: "test-uid",
						"app":                          "test-app",
					},
				},
			},
			expectedResult: true,
			expectedLabels: map[string]string{
				"app": "test-app",
			},
		},
		{
			name: "pod with only headroom label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						mppod.LabelHeadroomForWorkload: "test-uid",
					},
				},
			},
			expectedResult: true,
			expectedLabels: map[string]string{},
		},
		{
			name: "pod without headroom label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "test-app",
					},
				},
			},
			expectedResult: false,
			expectedLabels: map[string]string{
				"app": "test-app",
			},
		},
		{
			name: "pod without labels",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: nil,
				},
			},
			expectedResult: false,
			expectedLabels: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mppod.UnlabelWorkloadPodForHeadroomPod(tt.pod)
			assert.Equals(t, tt.expectedResult, result)
			assert.Equals(t, tt.expectedLabels, tt.pod.Labels)
		})
	}
}

func TestHeadroomLabelingFunctions(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID: "test-uid",
			Labels: map[string]string{
				"app": "test-app",
			},
		},
	}

	assert.Equals(t, false, mppod.WorkloadHasLabelPodForHeadroomPod(pod))

	labeled := mppod.LabelWorkloadPodForHeadroomPod(pod)
	assert.Equals(t, true, labeled)
	assert.Equals(t, true, mppod.WorkloadHasLabelPodForHeadroomPod(pod))
	assert.Equals(t, map[string]string{
		mppod.LabelHeadroomForWorkload: "test-uid",
		"app":                          "test-app",
	}, pod.Labels)

	labeledAgain := mppod.LabelWorkloadPodForHeadroomPod(pod)
	assert.Equals(t, false, labeledAgain)
	assert.Equals(t, true, mppod.WorkloadHasLabelPodForHeadroomPod(pod))

	unlabeled := mppod.UnlabelWorkloadPodForHeadroomPod(pod)
	assert.Equals(t, true, unlabeled)
	assert.Equals(t, false, mppod.WorkloadHasLabelPodForHeadroomPod(pod))
	assert.Equals(t, map[string]string{
		"app": "test-app",
	}, pod.Labels)

	unlabeledAgain := mppod.UnlabelWorkloadPodForHeadroomPod(pod)
	assert.Equals(t, false, unlabeledAgain)
	assert.Equals(t, false, mppod.WorkloadHasLabelPodForHeadroomPod(pod))
}

func TestIsHeadroomPod(t *testing.T) {
	tests := []struct {
		name     string
		pod      *corev1.Pod
		expected bool
	}{
		{
			name: "headroom pod",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "hr-abc123",
				},
			},
			expected: true,
		},
		{
			name: "regular pod",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "regular-pod-name",
				},
			},
			expected: false,
		},
		{
			name: "mountpoint pod",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "mp-abc123",
				},
			},
			expected: false,
		},
		{
			name: "pod with headroom pod prefix in middle",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "pod-hr-name",
				},
			},
			expected: false,
		},
		{
			name: "pod with empty name",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "",
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mppod.IsHeadroomPod(tt.pod)
			assert.Equals(t, tt.expected, result)
		})
	}
}
