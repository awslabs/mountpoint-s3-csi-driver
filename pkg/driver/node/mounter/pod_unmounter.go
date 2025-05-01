package mounter

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/aws-s3-csi-driver/pkg/podmounter/mppod"
	"github.com/awslabs/aws-s3-csi-driver/pkg/podmounter/mppod/watcher"
	"github.com/awslabs/aws-s3-csi-driver/pkg/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/mount-utils"
)

const cleanupInterval = 10 * time.Second

// PodUnmounter handles unmounting of Mountpoint Pods and cleanup of associated resources
type PodUnmounter struct {
	nodeID         string
	mountUtil      mount.Interface
	kubeletPath    string
	sourceMountDir string
	podWatcher     *watcher.Watcher
	credProvider   *credentialprovider.Provider
	mutex          sync.Mutex
}

// NewPodUnmounter creates a new PodUnmounter instance with the given parameters
func NewPodUnmounter(
	nodeID string,
	mountUtil mount.Interface,
	podWatcher *watcher.Watcher,
	credProvider *credentialprovider.Provider,
	sourceMountDir string,
) *PodUnmounter {
	return &PodUnmounter{
		nodeID:         nodeID,
		mountUtil:      mountUtil,
		kubeletPath:    util.KubeletPath(),
		sourceMountDir: sourceMountDir,
		podWatcher:     podWatcher,
		credProvider:   credProvider,
	}
}

// HandleMountpointPodUpdate is a Pod Update handler that triggers unmounting
// if the Mountpoint Pod is marked for unmounting via annotations
func (u *PodUnmounter) HandleMountpointPodUpdate(old, new any) {
	mpPod := new.(*corev1.Pod)
	if mpPod.Spec.NodeName != u.nodeID {
		return
	}

	if value, exists := mpPod.Annotations[mppod.AnnotationNeedsUnmount]; exists && value == "true" {
		u.unmountSourceForPod(mpPod)
	}
}

// unmountSourceForPod performs the unmounting process for a specific Mountpoint Pod
// including cleanup of associated resources
// mpPod: The Mountpoint Pod to unmount
func (u *PodUnmounter) unmountSourceForPod(mpPod *corev1.Pod) {
	mpPodUID := string(mpPod.UID)
	mpPodLock := getMPPodLock(mpPodUID)
	mpPodLock.mutex.Lock()
	defer func() {
		mpPodLock.mutex.Unlock()
		releaseMPPodLock(mpPodUID)
	}()

	klog.Infof("Found Mountpoint Pod %s (UID: %s) with %s annotation, unmounting it", mpPod.Name, mpPodUID, mppod.AnnotationNeedsUnmount)

	podPath := filepath.Join(u.kubeletPath, "pods", mpPodUID)
	source := filepath.Join(u.sourceMountDir, mpPodUID)
	volumeId := mpPod.Labels[mppod.LabelVolumeId]

	if err := u.writeExitFile(podPath); err != nil {
		return
	}

	if err := u.unmountAndCleanup(source); err != nil {
		return
	}
	klog.Infof("Successfully unmounted Mountpoint Pod - %s", mpPod.Name)

	if err := u.cleanupCredentials(volumeId, mpPodUID, podPath, source, mpPod); err != nil {
		return
	}
}

// writeExitFile creates an exit file in the pod's directory to signal Mountpoint Pod termination
// podPath: Path to the pod's directory
// Returns error if file creation fails
func (u *PodUnmounter) writeExitFile(podPath string) error {
	podMountExitPath := mppod.PathOnHost(podPath, mppod.KnownPathMountExit)
	_, err := os.OpenFile(podMountExitPath, os.O_RDONLY|os.O_CREATE, credentialprovider.CredentialFilePerm)
	if err != nil {
		klog.Errorf("Failed to send a exit message to Mountpoint Pod: %s", err)
		return err
	}
	return nil
}

// unmountAndCleanup unmounts the source directory and removes it
// source: Path to the source directory to unmount
// Returns error if unmounting or cleanup fails
func (u *PodUnmounter) unmountAndCleanup(source string) error {
	if err := u.mountUtil.Unmount(source); err != nil {
		klog.Errorf("Failed to unmount source %q: %v", source, err)
		return err
	}

	if err := os.Remove(source); err != nil {
		klog.Errorf("Failed to remove source directory %q: %v", source, err)
		return err
	}
	return nil
}

// cleanupCredentials removes credentials associated with the Mountpoint Pod
func (u *PodUnmounter) cleanupCredentials(volumeId, mpPodUID, podPath, source string, mpPod *corev1.Pod) error {
	err := u.credProvider.Cleanup(credentialprovider.CleanupContext{
		VolumeID:  volumeId,
		PodID:     mpPodUID,
		WritePath: filepath.Join(u.kubeletPath, "pods", mpPodUID),
	})
	if err != nil {
		klog.Errorf("Failed to clean up credentials for %s: %v", source, err)
		return err
	}
	return nil
}

// StartPeriodicCleanup begins periodic cleanup of dangling mounts
// This is needed in case when `HandleMountpointPodUpdate()` missed an update event to trigger cleanup.
// stopCh: Channel to signal stopping of the cleanup routine
func (u *PodUnmounter) StartPeriodicCleanup(stopCh <-chan struct{}) {
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			if err := u.CleanupDanglingMounts(); err != nil {
				klog.Errorf("Failed to run clean up of dangling mounts: %v", err)
			}
		}
	}
}

// CleanupDanglingMounts scans the source mount directory for potential dangling mounts
// and cleans them up. It also unmounts any Mountpoint Pods marked for unmounting.
func (u *PodUnmounter) CleanupDanglingMounts() error {
	// Ensure only one cleanup runs at a time
	if !u.mutex.TryLock() {
		return nil
	}
	defer u.mutex.Unlock()

	entries, err := os.ReadDir(u.sourceMountDir)
	if err != nil {
		klog.Errorf("Failed to read source mount directory (`%s`): %v", u.sourceMountDir, err)
		return err
	}

	for _, file := range entries {
		if !file.IsDir() {
			continue
		}

		mpPodUID := file.Name()
		source := filepath.Join(u.sourceMountDir, mpPodUID)
		// Try to find corresponding pod
		mpPod, err := u.findPodByUID(mpPodUID)
		if err != nil {
			klog.Errorf("Failed to check Mountpoint Pod (UID: %s) existence: %v", mpPodUID, err)
			return err
		}

		if mpPod == nil {
			klog.V(4).Infof("Mountpoint Pod not found for UID %s, will unmount and delete folder", mpPodUID)
			if err := u.unmountAndCleanup(source); err != nil {
				klog.Errorf("Failed to cleanup dangling mount for UID %s: %v", mpPodUID, err)
			}
			continue
		}

		// Unmount only if Mountpoint Pod is marked for unmounting
		if value, exists := mpPod.Annotations[mppod.AnnotationNeedsUnmount]; exists && value == "true" {
			u.unmountSourceForPod(mpPod)
		}
	}

	return nil
}

// findPodByUID finds Mountpoint Pod by UID in podWatcher's cache
func (u *PodUnmounter) findPodByUID(mpPodUID string) (*corev1.Pod, error) {
	pods, err := u.podWatcher.List()
	if err != nil {
		return nil, err
	}

	for _, pod := range pods {
		if string(pod.UID) == mpPodUID {
			return pod, nil
		}
	}
	return nil, nil
}
