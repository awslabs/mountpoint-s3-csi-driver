// Package envprovider provides utilities for accessing environment variables to pass Mountpoint.
package envprovider

import (
	"fmt"
	"os"
	"slices"
	"strings"
)

const (
	EnvRegion                = "AWS_REGION"
	EnvDefaultRegion         = "AWS_DEFAULT_REGION"
	EnvSTSRegionalEndpoints  = "AWS_STS_REGIONAL_ENDPOINTS"
	EnvMaxAttempts           = "AWS_MAX_ATTEMPTS"
	EnvProfile               = "AWS_PROFILE"
	EnvConfigFile            = "AWS_CONFIG_FILE"
	EnvSharedCredentialsFile = "AWS_SHARED_CREDENTIALS_FILE"
	EnvRoleARN               = "AWS_ROLE_ARN"
	EnvWebIdentityTokenFile  = "AWS_WEB_IDENTITY_TOKEN_FILE"
	EnvEC2MetadataDisabled   = "AWS_EC2_METADATA_DISABLED"

	EnvMountpointCacheKey = "UNSTABLE_MOUNTPOINT_CACHE_KEY"
)

// An Environment represents a list of environment variables.
type Environment = []string

// envAllowlist is the list of environment variables to pass-by by default.
// If any of these set, it will be returned as-is in [Provide].
var envAllowlist = []string{
	EnvRegion,
	EnvDefaultRegion,
	EnvSTSRegionalEndpoints,
}

// Region returns detected region from environment variables `AWS_REGION` or `AWS_DEFAULT_REGION`.
// It returns an empty string if both is unset.
func Region() string {
	region := os.Getenv(EnvRegion)
	if region != "" {
		return region
	}
	return os.Getenv(EnvDefaultRegion)
}

// Provide returns list of environment variables to pass Mountpoint.
func Provide() Environment {
	environment := Environment{}
	for _, key := range envAllowlist {
		val := os.Getenv(key)
		if val != "" {
			environment = append(environment, Format(key, val))
		}
	}
	return environment
}

// Format formats given key and value to be used as an environment variable.
func Format(key, value string) string {
	return fmt.Sprintf("%s=%s", key, value)
}

// Remove removes environment variable with given `key` from given environment variables `env`.
// It returns updated environment variables.
func Remove(env Environment, key string) Environment {
	prefix := key
	if !strings.HasSuffix(key, "=") {
		prefix = prefix + "="
	}
	return slices.DeleteFunc(env, func(k string) bool {
		return strings.HasPrefix(k, prefix)
	})
}
