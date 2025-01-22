package mounter

import "github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/awsprofile"

const (
	awsProfileEnv               = "AWS_PROFILE"
	awsConfigFileEnv            = "AWS_CONFIG_FILE"
	awsSharedCredentialsFileEnv = "AWS_SHARED_CREDENTIALS_FILE"
	keyIdEnv                    = "AWS_ACCESS_KEY_ID"
	accessKeyEnv                = "AWS_SECRET_ACCESS_KEY"
	sessionTokenEnv             = "AWS_SESSION_TOKEN"
	disableIMDSProviderEnv      = "AWS_EC2_METADATA_DISABLED"
	regionEnv                   = "AWS_REGION"
	defaultRegionEnv            = "AWS_DEFAULT_REGION"
	stsEndpointsEnv             = "AWS_STS_REGIONAL_ENDPOINTS"
	roleArnEnv                  = "AWS_ROLE_ARN"
	webIdentityTokenEnv         = "AWS_WEB_IDENTITY_TOKEN_FILE"
	MountS3PathEnv              = "MOUNT_S3_PATH"
	awsMaxAttemptsEnv           = "AWS_MAX_ATTEMPTS"
	MountpointCacheKey          = "UNSTABLE_MOUNTPOINT_CACHE_KEY"
	defaultMountS3Path          = "/usr/bin/mount-s3"
	userAgentPrefix             = "--user-agent-prefix"
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
func (mc *MountCredentials) Env(awsProfile awsprofile.AWSProfile) []string {
	env := []string{}

	// For profile provider from long-term credentials
	if awsProfile.Name != "" {
		env = append(env, awsProfileEnv+"="+awsProfile.Name)
		env = append(env, awsConfigFileEnv+"="+awsProfile.ConfigPath)
		env = append(env, awsSharedCredentialsFileEnv+"="+awsProfile.CredentialsPath)
	} else {
		// For profile provider
		if mc.ConfigFilePath != "" {
			env = append(env, awsConfigFileEnv+"="+mc.ConfigFilePath)
		}
		if mc.SharedCredentialsFilePath != "" {
			env = append(env, awsSharedCredentialsFileEnv+"="+mc.SharedCredentialsFilePath)
		}
	}

	// For STS Web Identity provider
	if mc.WebTokenPath != "" {
		env = append(env, webIdentityTokenEnv+"="+mc.WebTokenPath)
		env = append(env, roleArnEnv+"="+mc.AwsRoleArn)
	}

	// For disabling IMDS provider
	if mc.DisableIMDSProvider {
		env = append(env, disableIMDSProviderEnv+"=true")
	}

	// Generic variables
	if mc.Region != "" {
		env = append(env, regionEnv+"="+mc.Region)
	}
	if mc.DefaultRegion != "" {
		env = append(env, defaultRegionEnv+"="+mc.DefaultRegion)
	}
	if mc.StsEndpoints != "" {
		env = append(env, stsEndpointsEnv+"="+mc.StsEndpoints)
	}

	if mc.MountpointCacheKey != "" {
		env = append(env, MountpointCacheKey+"="+mc.MountpointCacheKey)
	}

	return env
}
