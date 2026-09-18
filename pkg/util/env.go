package util

import (
	"fmt"
	"os"
	"strconv"
)

func SupportLegacySystemdMounts() bool {
	return os.Getenv("SUPPORT_LEGACY_SYSTEMD_MOUNTS") == "true"
}

func SupportLegacyPodMounts() bool {
	return os.Getenv("SUPPORT_LEGACY_POD_MOUNTS") == "true"
}

// GetEnvAsInt returns the env variable parsed as an integer.
// Returns an error if the variable is not set or not a valid integer.
func GetEnvAsInt(key string) (int, error) {
	val, ok := os.LookupEnv(key)
	if !ok {
		return 0, fmt.Errorf("%s environment variable is required", key)
	}
	value, err := strconv.Atoi(val)
	if err != nil {
		return 0, fmt.Errorf("%s must be a valid integer, got %q: %w", key, val, err)
	}
	return value, nil
}
