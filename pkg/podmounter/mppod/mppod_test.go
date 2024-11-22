package mppod_test

import (
	"testing"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/awslabs/aws-s3-csi-driver/pkg/podmounter/mppod"
	"github.com/awslabs/aws-s3-csi-driver/pkg/util/testutil/assert"
)

func TestGeneratingMountpointPodName(t *testing.T) {
	t.Run("Consistency", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{UID: types.UID(uuid.New().String())},
		}
		pvc := &corev1.PersistentVolumeClaim{
			Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "test-vol"},
		}

		assert.Equals(t,
			mppod.MountpointPodNameFor(string(pod.UID), pvc.Spec.VolumeName),
			mppod.MountpointPodNameFor(string(pod.UID), pvc.Spec.VolumeName))
	})

	t.Run("Uniqueness", func(t *testing.T) {
		pod1 := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{UID: types.UID(uuid.New().String())},
		}
		pvc1 := &corev1.PersistentVolumeClaim{
			Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "test-vol-1"},
		}
		pod2 := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{UID: types.UID(uuid.New().String())},
		}
		pvc2 := &corev1.PersistentVolumeClaim{
			Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "test-vol-2"},
		}

		if mppod.MountpointPodNameFor(string(pod1.UID), pvc1.Spec.VolumeName) == mppod.MountpointPodNameFor(string(pod1.UID), pvc2.Spec.VolumeName) {
			t.Error("Different PVCs with same Pod should return a different Mountpoint Pod name")
		}
		if mppod.MountpointPodNameFor(string(pod1.UID), pvc1.Spec.VolumeName) == mppod.MountpointPodNameFor(string(pod2.UID), pvc1.Spec.VolumeName) {
			t.Error("Different Pods with same PVC should return a different Mountpoint Pod name")
		}
	})

	t.Run("Snapshot", func(t *testing.T) {
		mountpointPodName := mppod.MountpointPodNameFor("a4509011-bd2a-4f37-b1b0-05d715087852", "test-vol")
		assert.Equals(t, "mp-55f7d2331f3149f00d62d7af839d4cee895e1c68a2f0d96ffd359f79", mountpointPodName)
	})
}
