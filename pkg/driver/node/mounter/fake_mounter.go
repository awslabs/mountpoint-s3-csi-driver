package mounter

import (
	"context"

	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/aws-s3-csi-driver/pkg/mountpoint"
)

type FakeMounter struct{}

func (m *FakeMounter) Mount(ctx context.Context, bucketName string, target string,
	credentialCtx credentialprovider.ProvideContext, args mountpoint.Args, fsGroup, pvMountOptions string) error {
	return nil
}

func (m *FakeMounter) Unmount(ctx context.Context, target string, credentialCtx credentialprovider.CleanupContext) error {
	return nil
}

func (m *FakeMounter) IsMountPoint(target string) (bool, error) {
	return false, nil
}
