package util

import "os"

func UsePodMounter() bool {
	return os.Getenv("MOUNTER_KIND") == "pod"
}

func SupportLegacySystemdMounts() bool {
	return os.Getenv("SUPPORT_LEGACY_SYSTEMD_MOUNTS") == "true"
}
