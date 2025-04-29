package mounter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	crdv1beta "github.com/awslabs/aws-s3-csi-driver/pkg/api/v1beta"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/aws-s3-csi-driver/pkg/podmounter/mppod"
	"github.com/awslabs/aws-s3-csi-driver/pkg/podmounter/mppod/watcher"
	"github.com/awslabs/aws-s3-csi-driver/pkg/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/mount-utils"
	"sigs.k8s.io/controller-runtime/pkg/cache"
)

type PodUnmounter struct {
	nodeID         string
	mountUtil      mount.Interface
	kubeletPath    string
	sourceMountDir string
	podWatcher     *watcher.Watcher
	s3paCache      cache.Cache
	credProvider   *credentialprovider.Provider
}

func NewPodUnmounter(
	nodeID string,
	mountUtil mount.Interface,
	podWatcher *watcher.Watcher,
	s3paCache cache.Cache,
	credProvider *credentialprovider.Provider,
	sourceMountDir string,
) *PodUnmounter {
	return &PodUnmounter{
		nodeID:         nodeID,
		mountUtil:      mountUtil,
		kubeletPath:    util.KubeletPath(),
		sourceMountDir: sourceMountDir,
		podWatcher:     podWatcher,
		s3paCache:      s3paCache,
		credProvider:   credProvider,
	}
}

func (u *PodUnmounter) HandleS3PodAttachmentUpdate(old, new any) {
	s3pa := new.(*crdv1beta.MountpointS3PodAttachment)
	if s3pa.Spec.NodeName != u.nodeID {
		return
	}

	for mpPodName, uids := range s3pa.Spec.MountpointS3PodToWorkloadPodUIDs {
		if len(uids) == 0 {
			u.unmountSourceForPod(s3pa, mpPodName)
		}
	}
}

func (u *PodUnmounter) unmountSourceForPod(s3pa *crdv1beta.MountpointS3PodAttachment, mpPodName string) {
	klog.Infof("Found Mountpoint Pod with zero workload pods, unmounting it - %s", mpPodName)
	mpPod, err := u.podWatcher.Get(mpPodName)
	if err != nil {
		klog.Infof("failed to find Mountpoint Pod %s during update event", mpPodName)
		return
	}

	mpPodUID := string(mpPod.UID)
	podPath := filepath.Join(u.kubeletPath, "pods", mpPodUID)
	source := filepath.Join(u.sourceMountDir, mpPodUID)

	if err := u.writeExitFile(podPath, mpPod); err != nil {
		return
	}

	if err := u.unmountAndCleanup(source); err != nil {
		return
	}
	klog.Infof("Successfully unmounted Mountpoint Pod - %s", mpPodName)

	if err := u.cleanupCredentials(s3pa, mpPodUID, podPath, source, mpPod); err != nil {
		return
	}
}

func (u *PodUnmounter) writeExitFile(podPath string, mpPod *corev1.Pod) error {
	podMountExitPath := mppod.PathOnHost(podPath, mppod.KnownPathMountExit)
	_, err := os.OpenFile(podMountExitPath, os.O_RDONLY|os.O_CREATE, credentialprovider.CredentialFilePerm)
	if err != nil {
		klog.Errorf("Failed to send a exit message to Mountpoint Pod: %s", err)
		return err
	}
	return nil
}

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

func (u *PodUnmounter) cleanupCredentials(s3pa *crdv1beta.MountpointS3PodAttachment, mpPodUID, podPath, source string, mpPod *corev1.Pod) error {
	err := u.credProvider.Cleanup(credentialprovider.CleanupContext{
		VolumeID:  s3pa.Spec.VolumeID,
		PodID:     mpPodUID,
		WritePath: filepath.Join(u.kubeletPath, "pods", mpPodUID),
	})
	if err != nil {
		klog.Errorf("Failed to clean up credentials for %s: %v", source, err)
		return err
	}
	return nil
}

func (u *PodUnmounter) CleanupDanglingMounts() {
	entries, err := os.ReadDir(u.sourceMountDir)
	if err != nil {
		klog.Errorf("Failed to read source mount directory (`%s`): %v", u.sourceMountDir, err)
		return
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
			klog.V(4).Infof("Mountpoint Pod not found for UID %s, will only unmount and delete folder: %v", mpPodUID, err)
			if err := u.unmountAndCleanup(source); err != nil {
				klog.Errorf("Failed to cleanup dangling mount for Mountpoint Pod %s: %v", mpPod.Name, err)
			}
			continue
		}

		// Check if pod has an S3PodAttachment
		hasWorkloads, err := u.checkForWorkloads(mpPod)
		if err != nil {
			klog.Errorf("Failed to check workloads for Mountpoint Pod %s: %v", mpPod.Name, err)
			continue
		}

		if !hasWorkloads {
			klog.Infof("Found dangling mount for Mountpoint Pod %s (UID: %s), cleaning up", mpPod.Name, mpPodUID)
			podPath := filepath.Join(u.kubeletPath, "pods", mpPodUID)
			if err := u.writeExitFile(podPath, mpPod); err != nil {
				return
			}

			if err := u.unmountAndCleanup(source); err != nil {
				klog.Errorf("Failed to cleanup dangling mount for Mountpoint Pod %s: %v", mpPod.Name, err)
				continue
			}

			// TODO: Skip credential clean up as we do not know volumeID OR delete all files in credential folder?
		}
	}
}

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
	return nil, fmt.Errorf("Mountpoint Pod not found for UID %s", mpPodUID)
}

func (u *PodUnmounter) checkForWorkloads(mpPod *corev1.Pod) (bool, error) {
	s3paList := &crdv1beta.MountpointS3PodAttachmentList{}
	err := u.s3paCache.List(context.Background(), s3paList)
	if err != nil {
		return false, err
	}

	// Find attachment for this pod and check if it has workloads
	for _, s3pa := range s3paList.Items {
		for mpPodName, workloadUIDs := range s3pa.Spec.MountpointS3PodToWorkloadPodUIDs {
			if mpPodName == mpPod.Name {
				return len(workloadUIDs) > 0, nil
			}
		}
	}
	return false, nil
}
