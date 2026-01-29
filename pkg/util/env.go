package util

import "os"

func SupportLegacySystemdMounts() bool {
	return os.Getenv("SUPPORT_LEGACY_SYSTEMD_MOUNTS") == "true"
}
