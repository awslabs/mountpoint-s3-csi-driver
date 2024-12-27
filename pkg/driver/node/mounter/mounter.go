//go:generate mockgen -source=mounter.go -destination=./mocks/mock_mount.go -package=mock_driver
package mounter

import (
	"context"
	"os"

	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/envprovider"
	"github.com/awslabs/aws-s3-csi-driver/pkg/mountpoint"
	"github.com/awslabs/aws-s3-csi-driver/pkg/system"
)

type ServiceRunner interface {
	StartService(ctx context.Context, config *system.ExecConfig) (string, error)
	RunOneshot(ctx context.Context, config *system.ExecConfig) (string, error)
}

// Mounter is an interface for mount operations
type Mounter interface {
	Mount(bucketName string, target string, credentials credentialprovider.Credentials, env envprovider.Environment, args mountpoint.Args) error
	Unmount(target string) error
	IsMountPoint(target string) (bool, error)
}

const MountS3PathEnv = "MOUNT_S3_PATH"

func MountS3Path() string {
	mountS3Path := os.Getenv(MountS3PathEnv)
	if mountS3Path == "" {
		mountS3Path = defaultMountS3Path
	}
	return mountS3Path
}
