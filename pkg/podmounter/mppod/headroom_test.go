package mppod_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

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
