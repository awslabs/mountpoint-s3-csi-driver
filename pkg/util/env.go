package util

import "os"

func SupportLegacySystemdMounts() bool {
	return os.Getenv("SUPPORT_LEGACY_SYSTEMD_MOUNTS") == "true"
}

func SupportLegacyPodMounts() bool {
	return os.Getenv("SUPPORT_LEGACY_POD_MOUNTS") == "true"
}
