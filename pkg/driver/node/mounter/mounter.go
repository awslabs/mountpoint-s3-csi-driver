//go:generate mockgen -source=mounter.go -destination=./mocks/mock_mount.go -package=mock_driver
package mounter

import (
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"

	"k8s.io/klog/v2"
	"k8s.io/mount-utils"

	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/aws-s3-csi-driver/pkg/mountpoint"
	"github.com/awslabs/aws-s3-csi-driver/pkg/system"
)

// https://github.com/awslabs/mountpoint-s3/blob/9ed8b6243f4511e2013b2f4303a9197c3ddd4071/mountpoint-s3/src/cli.rs#L421
const mountpointDeviceName = "mountpoint-s3"

type ServiceRunner interface {
	StartService(ctx context.Context, config *system.ExecConfig) (string, error)
	RunOneshot(ctx context.Context, config *system.ExecConfig) (string, error)
}

// Mounter is an interface for mount operations
type Mounter interface {
	Mount(ctx context.Context, bucketName string, target string, credentialCtx credentialprovider.ProvideContext, args mountpoint.Args, fsGroup, pvMountOptions string) error
	Unmount(ctx context.Context, target string, credentialCtx credentialprovider.CleanupContext) error
	IsMountPoint(target string) (bool, error)
}

// Internal S3 CSI Driver directory for source mount points
const SourceMountDir = "/var/lib/kubelet/plugins/s3.csi.aws.com/mnt/"
const MountS3PathEnv = "MOUNT_S3_PATH"
const defaultMountS3Path = "/usr/bin/mount-s3"

func MountS3Path() string {
	mountS3Path := os.Getenv(MountS3PathEnv)
	if mountS3Path == "" {
		mountS3Path = defaultMountS3Path
	}
	return mountS3Path
}

// isMountPoint returns whether given `target` is a `mount-s3` mount.
// We implement additional check on top of `mounter.IsMountPoint` because we need
// to verify not only that the target is a mount point but also that it is specifically a mount-s3 mount point.
// This is achieved by calling the `mounter.List()` method to enumerate all mount points.
func isMountPoint(mounter mount.Interface, target string) (bool, error) {
	if _, err := os.Stat(target); os.IsNotExist(err) {
		return false, err
	}

	mountPoints, err := mounter.List()
	if err != nil {
		return false, fmt.Errorf("Failed to list mounts: %w", err)
	}
	for _, mp := range mountPoints {
		if mp.Path == target {
			if mp.Device != mountpointDeviceName {
				klog.V(4).Infof("IsMountPoint: %s is not a `mount-s3` mount. Expected device type to be %s but got %s, skipping unmount", target, mountpointDeviceName, mp.Device)
				continue
			}

			return true, nil
		}
	}
	return false, nil
}

// findSourceMountPoint locates the source S3 mount point for a given target path by comparing
// device IDs and inodes with all S3 mount points at driver source directory `SourceMountDir`.
//
// Parameters:
//   - mounter: Interface providing mounting operations and mount point listing capabilities
//   - target: The target path whose source mount point needs to be found
//
// Returns:
//   - string: The path of the source mount point if found
//   - error: An error if the operation fails
//
// The function works by:
// 1. Getting the device ID and inode of the target path
// 2. Listing all mount points in the system that has "mountpoint-s3" as device name and prefix `SourceMountDir`
// 3. Finding a mount point that matches both the device ID and inode of the target
func findSourceMountPoint(mounter mount.Interface, target string) (string, error) {
	if mounter == nil {
		return "", fmt.Errorf("mounter interface cannot be nil")
	}

	targetFileInfo, err := os.Stat(target)
	if err != nil {
		return "", fmt.Errorf("failed to stat %q: %w", target, err)
	}

	targetSysInfo, ok := targetFileInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("failed to get system info for target %q", target)
	}

	targetDevID := targetSysInfo.Dev
	targetInodeID := targetSysInfo.Ino

	mountPoints, err := mounter.List()
	if err != nil {
		return "", fmt.Errorf("failed to list mount points: %w", err)
	}

	for _, mountPoint := range mountPoints {
		if mountPoint.Device != mountpointDeviceName || !strings.HasPrefix(mountPoint.Path, SourceMountDir) {
			continue
		}

		mountPathInfo, err := os.Stat(mountPoint.Path)
		if err != nil {
			klog.V(4).Infof("Skipping mount point %q: unable to stat %v", mountPoint.Path, err)
			continue
		}

		mountSysInfo, ok := mountPathInfo.Sys().(*syscall.Stat_t)
		if !ok {
			klog.V(4).Infof("Skipping mount point %q: unable to get system info", mountPoint.Path)
			continue
		}

		if targetDevID == mountSysInfo.Dev && targetInodeID == mountSysInfo.Ino {
			return mountPoint.Path, nil
		}
	}

	return "", fmt.Errorf("no source mount point found for path %q (device: %d, inode: %d)",
		target, targetDevID, targetInodeID)
}
