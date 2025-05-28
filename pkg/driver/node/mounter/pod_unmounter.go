package mounter

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	mpmounter "github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mounter"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/podmounter/mppod"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/podmounter/mppod/watcher"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
)

const (
	danglingMountpointCleanupInterval = 2 * time.Minute

	waitUntilMountpointIsUnmountedTimeout  = 30 * time.Second
	waitUntilMountpointIsUnmountedInterval = 5 * time.Second

	waitUntilMountpointIsUnusedTimeout  = 30 * time.Second
	waitUntilMountpointIsUnusedInterval = 5 * time.Second
)

// PodUnmounter handles unmounting of Mountpoint Pods and cleanup of associated resources
type PodUnmounter struct {
	nodeID       string
	mount        *mpmounter.Mounter
	kubeletPath  string
	podWatcher   *watcher.Watcher
	credProvider *credentialprovider.Provider
}

// NewPodUnmounter creates a new PodUnmounter instance with the given parameters
func NewPodUnmounter(
	nodeID string,
	mount *mpmounter.Mounter,
	podWatcher *watcher.Watcher,
	credProvider *credentialprovider.Provider,
) *PodUnmounter {
	return &PodUnmounter{
		nodeID:       nodeID,
		mount:        mount,
		kubeletPath:  util.KubeletPath(),
		podWatcher:   podWatcher,
		credProvider: credProvider,
	}
}

// HandleMountpointPodUpdate is a Pod Update handler that triggers unmounting
// if the Mountpoint Pod is marked for unmounting via annotations
func (u *PodUnmounter) HandleMountpointPodUpdate(old, new any) {
	mpPod := new.(*corev1.Pod)
	if mpPod.Spec.NodeName != u.nodeID {
		return
	}

	u.unmountMountpointPodIfNeeded(mpPod)
}

// StartPeriodicCleanup begins periodic cleanup of dangling mounts
// This is needed in case when `HandleMountpointPodUpdate()` missed an update event to trigger cleanup.
// stopCh: Channel to signal stopping of the cleanup routine
func (u *PodUnmounter) StartPeriodicCleanup(stopCh <-chan struct{}) {
	ticker := time.NewTicker(danglingMountpointCleanupInterval)
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
	sourceMountDir := SourceMountDir(u.kubeletPath)
	entries, err := os.ReadDir(sourceMountDir)
	if err != nil {
		// Source mount dir does not exists, meaning there aren't any mounts
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("failed to read source mount directory %q: %w", sourceMountDir, err)
	}

	for _, file := range entries {
		if !file.IsDir() {
			continue
		}

		mpPodName := file.Name()
		source := filepath.Join(sourceMountDir, mpPodName)
		mpPod, err := u.podWatcher.Get(mpPodName)
		if err != nil {
			if apierrors.IsNotFound(err) {
				klog.Infof("Found a dangling Mountpoint mount %q, cleaning up", mpPodName)
				if _, err := u.unmountAndRemoveMountpointSource(source); err != nil {
					klog.Errorf("Failed to unmount and remove Mountpoint %q: %v", source, err)
				} else {
					klog.Infof("Successfully cleaned dangling Mountpoint mount %q", mpPodName)
				}
				continue
			}

			return fmt.Errorf("failed to check existence of Mountpoint Pod %q: %w", mpPodName, err)
		}

		u.unmountMountpointPodIfNeeded(mpPod)
	}

	return nil
}

// unmountMountpointPodIfNeeded unmounts `mpPod` if and only if annotated with "needs-unmount".
func (u *PodUnmounter) unmountMountpointPodIfNeeded(mpPod *corev1.Pod) {
	if value, _ := mpPod.Annotations[mppod.AnnotationNeedsUnmount]; value != "true" {
		// Not marked for unmount, skip it
		return
	}

	unlockMountpointPod := lockMountpointPod(mpPod.Name)
	defer unlockMountpointPod()

	u.cleanUnmount(mpPod)
}

// cleanUnmount performs a clean unmount for `mpPod`.
func (u *PodUnmounter) cleanUnmount(mpPod *corev1.Pod) {
	klog.V(5).Infof("Starting unmount procedure for Mountpoint Pod %q", mpPod.Name)

	source := u.mountpointPodSourcePath(mpPod.Name)
	podPath := u.podPath(string(mpPod.UID))

	// First, write `mount.exit` file to signal a clean exit to Mountpoint Pod, so it exists with zero code.
	if err := u.writeExitFile(podPath); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			klog.Errorf("Failed to write exit file for Mountpoint Pod %q: %v", mpPod.Name, err)
		}
		return
	}

	// Now unmount and remove `source`
	wasMountpoint, err := u.unmountAndRemoveMountpointSource(source)
	if err != nil {
		if errors.Is(err, errMountpointIsStillInUse) {
			klog.Infof("Mountpoint Pod %q is still in use, will retry later", mpPod.Name)
		} else {
			klog.Errorf("Failed to unmount and remove Mountpoint Pod %q: %v", mpPod.Name, err)
		}
		return
	}

	if err := u.cleanupCredentials(mpPod); err != nil {
		klog.Errorf("Failed to cleanup credentials of Mountpoint Pod %q: %v", mpPod.Name, err)
		return
	}

	if wasMountpoint {
		klog.Infof("Mountpoint Pod %q successfully unmounted", mpPod.Name)
	}
}

// unmountAndRemoveMountpointSource unmounts Mountpoint at `source`, and then removes the (empty) directory.
// It returns whether `source` was a Mountpoint and any error encountered.
func (u *PodUnmounter) unmountAndRemoveMountpointSource(source string) (bool, error) {
	isMountpoint, err := u.mount.CheckMountpoint(source)
	isCorruptedMountpoint := err != nil && u.mount.IsMountpointCorrupted(err)
	if err != nil && errors.Is(err, fs.ErrNotExist) {
		// Target does not exists, nothing to do
		return isMountpoint, nil
	} else if err != nil && !isCorruptedMountpoint {
		return isMountpoint, fmt.Errorf("failed to check orphan Mountpoint %q: %w", source, err)
	}

	if isMountpoint {
		// If `source` is still a Mountpoint mount, let's wait until all references (i.e., bind mounts) are gone
		// to ensure to not interrupt any (potentially terminating) workloads.
		if err := u.waitUntilMountpointIsUnused(source); err != nil {
			return isMountpoint, fmt.Errorf("failed to wait until orphan Mountpoint %q is unused: %w", source, err)
		}
	}

	if isMountpoint || isCorruptedMountpoint {
		if err := u.mount.Unmount(source); err != nil {
			return isMountpoint, fmt.Errorf("failed to unmount orphan Mountpoint %q: %w", source, err)
		}
	}

	if err := u.waitUntilMountpointIsUnmounted(source); err != nil {
		return isMountpoint, fmt.Errorf("failed to wait until orphan Mountpoint %q is unmounted: %w", source, err)
	}

	// Now we know there is no Mountpoint at `source`, and it should be a regular directory.
	// Let's remove it
	if err := os.Remove(source); err != nil {
		return isMountpoint, fmt.Errorf("failed to remove source directory of orphan Mountpoint %q: %w", source, err)
	}

	return isMountpoint, nil
}

// writeExitFile creates an exit file in the pod's directory to signal Mountpoint Pod termination
// podPath: Path to the pod's directory
// Returns error if file creation fails
func (u *PodUnmounter) writeExitFile(podPath string) error {
	podMountExitPath := mppod.PathOnHost(podPath, mppod.KnownPathMountExit)
	f, err := os.OpenFile(podMountExitPath, os.O_RDONLY|os.O_CREATE, credentialprovider.CredentialFilePerm)
	_ = f.Close()
	return err
}

// cleanupCredentials removes credentials associated with the Mountpoint Pod
func (u *PodUnmounter) cleanupCredentials(mpPod *corev1.Pod) error {
	return u.credProvider.Cleanup(credentialprovider.CleanupContext{
		VolumeID:  mpPod.Labels[mppod.LabelVolumeId],
		PodID:     string(mpPod.UID),
		WritePath: mppod.PathOnHost(u.podPath(string(mpPod.UID)), mppod.KnownPathCredentials),
		MountKind: credentialprovider.MountKindPod,
	})
}

// errMountpointIsStillInUse is returned when [waitUntilMountpointIsUnused] fails with timeout.
var errMountpointIsStillInUse = errors.New("podunmounter: mountpoint is still in use")

// waitUntilMountpointIsUnused waits until all references to Mountpoint at `source` is gone.
// Returns an error if condition is not met within `waitUntilMountpointIsUnusedTimeout`.
func (u *PodUnmounter) waitUntilMountpointIsUnused(source string) error {
	ctx, cancel := context.WithTimeout(context.Background(), waitUntilMountpointIsUnusedTimeout)
	defer cancel()

	err := wait.PollUntilContextCancel(ctx, waitUntilMountpointIsUnusedInterval, true, func(ctx context.Context) (done bool, err error) {
		references, err := u.mount.FindReferencesToMountpoint(source)
		if err != nil {
			return false, err
		}

		if len(references) > 0 {
			return false, nil
		}

		return true, nil
	})
	if err != nil && errors.Is(err, context.DeadlineExceeded) {
		return errMountpointIsStillInUse
	}

	return err
}

// waitUntilMountpointIsUnmounted waits until Mountpoint at `source` is unmounted.
// Returns an error if condition is not met within `waitUntilMountpointIsUnmountedTimeout`.
func (u *PodUnmounter) waitUntilMountpointIsUnmounted(source string) error {
	ctx, cancel := context.WithTimeout(context.Background(), waitUntilMountpointIsUnmountedTimeout)
	defer cancel()

	return wait.PollUntilContextCancel(ctx, waitUntilMountpointIsUnmountedInterval, true, func(ctx context.Context) (done bool, err error) {
		isMountpoint, err := u.mount.CheckMountpoint(source)
		if err != nil {
			return false, err
		}

		return !isMountpoint, nil
	})
}

// mountpointPodSourcePath returns source path for `mpPodName`.
// This is the path where `mpPodName` is mounted.
func (u *PodUnmounter) mountpointPodSourcePath(mpPodName string) string {
	return filepath.Join(SourceMountDir(u.kubeletPath), mpPodName)
}

// podPath returns `pod`'s basepath inside kubelet's path.
func (u *PodUnmounter) podPath(podUID string) string {
	return filepath.Join(u.kubeletPath, "pods", podUID)
}
