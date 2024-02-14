package driver

type FakeMounter struct{}

func (m *FakeMounter) Mount(bucketName string, target string,
	credentials *MountCredentials, options []string) error {
	return nil
}

func (m *FakeMounter) Unmount(target string) error {
	return nil
}

func (m *FakeMounter) IsMountPoint(target string) (bool, error) {
	return false, nil
}
