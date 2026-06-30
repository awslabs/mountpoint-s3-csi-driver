package csicontroller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

func TestIsPodActive(t *testing.T) {
	now := metav1.Now()
	isDeleting := &now
	var noDeleting *metav1.Time

	tests := []struct {
		name              string
		phase             corev1.PodPhase
		deletionTimestamp *metav1.Time
		expect            bool
	}{
		{"Pending", corev1.PodPending, noDeleting, true},
		{"Pending + terminating", corev1.PodPending, isDeleting, false},
		{"Running", corev1.PodRunning, noDeleting, true},
		{"Running + terminating", corev1.PodRunning, isDeleting, true},
		{"Succeeded", corev1.PodSucceeded, noDeleting, false},
		{"Succeeded + terminating", corev1.PodSucceeded, isDeleting, false},
		{"Failed", corev1.PodFailed, noDeleting, false},
		{"Failed + terminating", corev1.PodFailed, isDeleting, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: tt.deletionTimestamp},
				Status:     corev1.PodStatus{Phase: tt.phase},
			}
			assert.Equals(t, tt.expect, isPodActive(pod))
		})
	}
}
