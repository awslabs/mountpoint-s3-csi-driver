//go:generate mockgen -source=mounter.go -destination=./mocks/mock_mount.go -package=mock_driver
package mounter

import (
	"context"
	"path/filepath"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
)

// Mounter is an interface for mount operations
type Mounter interface {
	Mount(ctx context.Context, bucketName string, target string, credentialCtx credentialprovider.ProvideContext, args mountpoint.Args, fsGroup string) error
	Unmount(ctx context.Context, target string, credentialCtx credentialprovider.CleanupContext) error
	IsMountPoint(target string) (bool, error)
}

// Internal S3 CSI Driver directory for source mount points
func SourceMountDir(kubeletPath string) string {
	return filepath.Join(kubeletPath, "plugins", "s3.csi.aws.com", "mnt")
}
