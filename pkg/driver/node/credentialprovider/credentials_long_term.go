package credentialprovider

import (
	"fmt"
	"path/filepath"

	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/credentialprovider/awsprofile"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/envprovider"
)

type longTermCredentials struct {
	source AuthenticationSource

	accessKeyID     string
	secretAccessKey string
	sessionToken    string
}

func (c *longTermCredentials) Source() AuthenticationSource {
	return c.source
}

func (c *longTermCredentials) Dump(writePath string, envPath string) (envprovider.Environment, error) {
	awsProfile, err := awsprofile.CreateAWSProfile(writePath, c.accessKeyID, c.secretAccessKey, c.sessionToken, CredentialFilePerm)
	if err != nil {
		return nil, fmt.Errorf("credentialprovider: long-term: failed to create aws profile: %w", err)
	}

	profile := awsProfile.Name
	configFile := filepath.Join(envPath, awsProfile.ConfigFilename)
	credentialsFile := filepath.Join(envPath, awsProfile.CredentialsFilename)

	return envprovider.Environment{
		envprovider.Format(envprovider.EnvProfile, profile),
		envprovider.Format(envprovider.EnvConfigFile, configFile),
		envprovider.Format(envprovider.EnvSharedCredentialsFile, credentialsFile),
	}, nil
}
