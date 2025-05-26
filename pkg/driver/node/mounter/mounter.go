//go:generate mockgen -source=mounter.go -destination=./mocks/mock_mount.go -package=mock_driver
package mounter

import (
	"context"
	"fmt"
	"os"

	"k8s.io/klog/v2"
	"k8s.io/mount-utils"

	"github.com/scality/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/scality/mountpoint-s3-csi-driver/pkg/mountpoint"
	"github.com/scality/mountpoint-s3-csi-driver/pkg/system"
)

// https://github.com/awslabs/mountpoint-s3/blob/9ed8b6243f4511e2013b2f4303a9197c3ddd4071/mountpoint-s3/src/cli.rs#L421
const mountpointDeviceName = "mountpoint-s3"

type ServiceRunner interface {
	StartService(ctx context.Context, config *system.ExecConfig) (string, error)
	RunOneshot(ctx context.Context, config *system.ExecConfig) (string, error)
}

// Mounter is an interface for mount operations
type Mounter interface {
	Mount(ctx context.Context, bucketName string, target string, credentialCtx credentialprovider.ProvideContext, args mountpoint.Args) error
	Unmount(ctx context.Context, target string, credentialCtx credentialprovider.CleanupContext) error
	IsMountPoint(target string) (bool, error)
}

const (
	MountS3PathEnv     = "MOUNT_S3_PATH"
	defaultMountS3Path = "/usr/bin/mount-s3"
)

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
		return false, fmt.Errorf("failed to list mounts: %w", err)
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
