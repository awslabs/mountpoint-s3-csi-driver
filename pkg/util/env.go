package util

import (
	"os"
	"strconv"
)

func SupportLegacySystemdMounts() bool {
	return os.Getenv("SUPPORT_LEGACY_SYSTEMD_MOUNTS") == "true"
}

// GetEnvAsIntOrFallback returns the env variable (parsed as integer) for
// the given key and falls back to the given defaultValue if not set.
// Copied from https://github.com/kubernetes/kubernetes/blob/release-1.36/pkg/util/env/env.go
func GetEnvAsIntOrFallback(key string, defaultValue int) (int, error) {
	if v := os.Getenv(key); v != "" {
		value, err := strconv.Atoi(v)
		if err != nil {
			return defaultValue, err
		}
		return value, nil
	}
	return defaultValue, nil
}
