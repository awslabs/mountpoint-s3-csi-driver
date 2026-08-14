package util

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

const (
	// DefaultMountPropagationDelay is the default time to wait for host mounts
	// to propagate into the container's mount namespace after a pod restart.
	DefaultMountPropagationDelay = 5 * time.Second
)

func SupportLegacySystemdMounts() bool {
	return os.Getenv("SUPPORT_LEGACY_SYSTEMD_MOUNTS") == "true"
}

// MountPropagationDelay returns the configured mount propagation delay.
// It reads from the MOUNT_PROPAGATION_DELAY_SECONDS environment variable.
// If unset or empty, returns DefaultMountPropagationDelay.
// If set to "0", returns 0 (disabling the delay).
func MountPropagationDelay() time.Duration {
	val := os.Getenv("MOUNT_PROPAGATION_DELAY_SECONDS")
	if val == "" {
		return DefaultMountPropagationDelay
	}
	seconds, err := strconv.Atoi(val)
	if err != nil || seconds < 0 {
		return DefaultMountPropagationDelay
	}
	return time.Duration(seconds) * time.Second
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
