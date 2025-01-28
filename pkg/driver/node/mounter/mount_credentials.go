package mounter

import (
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/awsprofile"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/envprovider"
)

type MountCredentials struct {
	// Identifies how these credentials are obtained.
	AuthenticationSource AuthenticationSource

	// -- Env variable provider
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string

	// -- Profile provider
	ConfigFilePath            string
	SharedCredentialsFilePath string

	// -- STS provider
	WebTokenPath string
	AwsRoleArn   string

	// -- IMDS provider
	DisableIMDSProvider bool

	// -- Generic
	Region        string
	DefaultRegion string
	StsEndpoints  string

	// -- TODO - Move somewhere better
	MountpointCacheKey string
}

// Get environment variables to pass to mount-s3 for authentication.
func (mc *MountCredentials) Env(awsProfile awsprofile.AWSProfile) envprovider.Environment {
	env := envprovider.Environment{}

	// For profile provider from long-term credentials
	if awsProfile.Name != "" {
		env.Set(envprovider.EnvProfile, awsProfile.Name)
		env.Set(envprovider.EnvConfigFile, awsProfile.ConfigPath)
		env.Set(envprovider.EnvSharedCredentialsFile, awsProfile.CredentialsPath)
	} else {
		// For profile provider
		if mc.ConfigFilePath != "" {
			env.Set(envprovider.EnvConfigFile, mc.ConfigFilePath)
		}
		if mc.SharedCredentialsFilePath != "" {
			env.Set(envprovider.EnvSharedCredentialsFile, mc.SharedCredentialsFilePath)
		}
	}

	// For STS Web Identity provider
	if mc.WebTokenPath != "" {
		env.Set(envprovider.EnvWebIdentityTokenFile, mc.WebTokenPath)
		env.Set(envprovider.EnvRoleARN, mc.AwsRoleArn)
	}

	// For disabling IMDS provider
	if mc.DisableIMDSProvider {
		env.Set(envprovider.EnvEC2MetadataDisabled, "true")
	}

	// Generic variables
	if mc.Region != "" {
		env.Set(envprovider.EnvRegion, mc.Region)
	}
	if mc.DefaultRegion != "" {
		env.Set(envprovider.EnvDefaultRegion, mc.DefaultRegion)
	}
	if mc.StsEndpoints != "" {
		env.Set(envprovider.EnvSTSRegionalEndpoints, mc.StsEndpoints)
	}

	if mc.MountpointCacheKey != "" {
		env.Set(envprovider.EnvMountpointCacheKey, mc.MountpointCacheKey)
	}

	return env
}
