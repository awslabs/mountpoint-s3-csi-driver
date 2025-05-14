package mounter

import (
	"testing"

	"github.com/awslabs/aws-s3-csi-driver/pkg/util/testutil/assert"
)

func TestGetMPPodLock(t *testing.T) {
	// Clear the map before testing
	mpPodLocks = make(map[string]*MPPodLock)

	t.Run("New lock creation", func(t *testing.T) {
		podUID := "pod1"
		lock := getMPPodLock(podUID)

		assert.Equals(t, 1, lock.refCount)
		assert.Equals(t, 1, len(mpPodLocks))
	})

	t.Run("Existing lock retrieval", func(t *testing.T) {
		podUID := "pod2"
		firstLock := getMPPodLock(podUID)
		secondLock := getMPPodLock(podUID)

		if firstLock != secondLock {
			t.Fatal("Expected to get the same lock instance")
		}
		assert.Equals(t, 2, firstLock.refCount)
	})
}

func TestReleaseMPPodLock(t *testing.T) {
	// Clear the map before testing
	mpPodLocks = make(map[string]*MPPodLock)

	t.Run("Release existing lock", func(t *testing.T) {
		podUID := "pod3"
		getMPPodLock(podUID)
		getMPPodLock(podUID)

		releaseMPPodLock(podUID)

		lock, exists := mpPodLocks[podUID]
		assert.Equals(t, true, exists)
		assert.Equals(t, 1, lock.refCount)

		releaseMPPodLock(podUID)

		_, exists = mpPodLocks[podUID]
		assert.Equals(t, false, exists)
	})

	t.Run("Release non-existent lock", func(t *testing.T) {
		podUID := "non-existent-pod"
		releaseMPPodLock(podUID)
		// This test passes if no panic occurs
	})
}
