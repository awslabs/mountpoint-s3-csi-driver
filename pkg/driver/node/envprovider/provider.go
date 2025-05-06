// Package envprovider provides utilities for accessing environment variables to pass Mountpoint.
package envprovider

import (
	"fmt"
	"maps"
	"os"
	"slices"
)

const (
	EnvRegion                          = "AWS_REGION"
	EnvDefaultRegion                   = "AWS_DEFAULT_REGION"
	EnvSTSRegionalEndpoints            = "AWS_STS_REGIONAL_ENDPOINTS"
	EnvMaxAttempts                     = "AWS_MAX_ATTEMPTS"
	EnvProfile                         = "AWS_PROFILE"
	EnvConfigFile                      = "AWS_CONFIG_FILE"
	EnvSharedCredentialsFile           = "AWS_SHARED_CREDENTIALS_FILE"
	EnvRoleARN                         = "AWS_ROLE_ARN"
	EnvWebIdentityTokenFile            = "AWS_WEB_IDENTITY_TOKEN_FILE"
	EnvContainerAuthorizationTokenFile = "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"
	EnvContainerCredentialsFullURI     = "AWS_CONTAINER_CREDENTIALS_FULL_URI"
	EnvEC2MetadataDisabled             = "AWS_EC2_METADATA_DISABLED"
	EnvAccessKeyID                     = "AWS_ACCESS_KEY_ID"
	EnvSecretAccessKey                 = "AWS_SECRET_ACCESS_KEY"
	EnvSessionToken                    = "AWS_SESSION_TOKEN"
	EnvMountpointCacheKey              = "UNSTABLE_MOUNTPOINT_CACHE_KEY"
)

// Key represents an environment variable name.
type Key = string

// Value represents an environment variable value.
type Value = string

// Environment represents a list of environment variables as key-value pairs.
type Environment map[Key]Value

// envAllowlist is the list of environment variables to pass-by by default.
// If any of these set, it will be returned as-is in [Default].
var envAllowlist = []Key{
	EnvRegion,
	EnvDefaultRegion,
	EnvSTSRegionalEndpoints,
}

// Region returns detected region from environment variables `AWS_REGION` or `AWS_DEFAULT_REGION`.
// It returns an empty string if both is unset.
func Region() Value {
	region := os.Getenv(EnvRegion)
	if region != "" {
		return region
	}
	return os.Getenv(EnvDefaultRegion)
}

// Default returns list of environment variables to pass Mountpoint.
func Default() Environment {
	environment := make(Environment)
	for _, key := range envAllowlist {
		val := os.Getenv(key)
		if val != "" {
			environment[key] = val
		}
	}
	return environment
}

// List returns a sorted slice of environment variables in "KEY=VALUE" format.
func (env Environment) List() []string {
	list := []string{}
	for key, val := range env {
		list = append(list, format(key, val))
	}
	slices.Sort(list)
	return list
}

// Delete deletes the environment variable with the specified key.
func (env Environment) Delete(key Key) {
	delete(env, key)
}

// Set adds or updates the environment variable with the specified key and value.
func (env Environment) Set(key Key, value Value) {
	env[key] = value
}

// Merge adds all key-value pairs from the given environment to the current environment.
// If a key exists in both environments, the value from the given environment takes precedence.
func (env Environment) Merge(other Environment) {
	maps.Copy(env, other)
}

// format formats given key and value to be used as an environment variable.
func format(key Key, value Value) string {
	return fmt.Sprintf("%s=%s", key, value)
}
