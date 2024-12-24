package credentialprovider

import (
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/envprovider"
)

type sharedProfileCredentials struct {
	source AuthenticationSource

	configFile            string
	sharedCredentialsFile string
}

func (c *sharedProfileCredentials) Source() AuthenticationSource {
	return c.source
}

func (c *sharedProfileCredentials) Dump(writePath string, envPath string) (envprovider.Environment, error) {
	return envprovider.Environment{
		envprovider.Format(envprovider.EnvConfigFile, c.configFile),
		envprovider.Format(envprovider.EnvSharedCredentialsFile, c.sharedCredentialsFile),
	}, nil
}
