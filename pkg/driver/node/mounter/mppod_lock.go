package mounter

import (
	"sync"

	"k8s.io/klog/v2"
)

// MPPodLock represents a reference-counted mutex lock for Mountpoint Pod.
// It ensures synchronized access to pod-specific resources.
type MPPodLock struct {
	mutex    sync.Mutex
	refCount int
}

var (
	// mpPodLocks maps pod name to their corresponding locks.
	mpPodLocks = make(map[string]*MPPodLock)

	// mpPodLocksMutex guards access to the mpPodLocks map.
	mpPodLocksMutex sync.Mutex
)

// lockMountpointPod acquires a lock for the specified pod name and returns an unlock function.
// The returned function must be called to release the lock and cleanup resources.
//
// Parameters:
//   - mpPodName: The name of the Mountpoint Pod to lock
//
// Returns:
//   - func(): A function that when called will unlock the pod and release associated resources
//
// Usage:
//
//	unlock := lockMountpointPod(mpPodName)
//	defer unlock()
func lockMountpointPod(mpPodName string) func() {
	mpPodLock := getMPPodLock(mpPodName)
	mpPodLock.mutex.Lock()
	return func() {
		mpPodLock.mutex.Unlock()
		releaseMPPodLock(mpPodName)
	}
}

// getMPPodLock retrieves or creates a lock for the specified pod name.
// It increments the reference count for existing locks.
// The caller is responsible for calling releaseMPPodLock when the lock is no longer needed.
func getMPPodLock(mpPodName string) *MPPodLock {
	mpPodLocksMutex.Lock()
	defer mpPodLocksMutex.Unlock()

	lock, exists := mpPodLocks[mpPodName]
	if !exists {
		lock = &MPPodLock{refCount: 1}
		mpPodLocks[mpPodName] = lock
	} else {
		lock.refCount++
	}
	return lock
}

// releaseMPPodLock decrements the reference count for a pod's lock.
// When the reference count reaches zero, the lock is removed from the map.
// If the lock doesn't exist, the function returns silently.
func releaseMPPodLock(mpPodName string) {
	mpPodLocksMutex.Lock()
	defer mpPodLocksMutex.Unlock()

	lock, exists := mpPodLocks[mpPodName]
	if !exists {
		// Should never happen
		klog.Errorf("Attempted to release non-existent lock for Mountpoint Pod %s", mpPodName)
		return
	}

	lock.refCount--

	if lock.refCount <= 0 {
		delete(mpPodLocks, mpPodName)
	}
}
