package mounter

import "github.com/awslabs/aws-s3-csi-driver/pkg/mountpoint"

type FakeMounter struct{}

func (m *FakeMounter) Mount(bucketName string, target string,
	credentials *MountCredentials, args mountpoint.Args) error {
	return nil
}

func (m *FakeMounter) Unmount(target string) error {
	return nil
}

func (m *FakeMounter) IsMountPoint(target string) (bool, error) {
	return false, nil
}
