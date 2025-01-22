package credentialprovider

import (
	"path/filepath"

	"github.com/google/renameio"

	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/envprovider"
	"github.com/awslabs/aws-s3-csi-driver/pkg/util"
)

const (
	serviceAccountFilename = "serviceaccount.token"
)

type stsWebIdentityCredentials struct {
	source AuthenticationSource

	cacheKey string

	roleARN string

	// These are mutually exclusive, if both set, `webIdentityToken` will be used.
	webIdentityToken     string
	webIdentityTokenFile string
}

func (c *stsWebIdentityCredentials) Source() AuthenticationSource {
	return c.source
}

func (c *stsWebIdentityCredentials) Dump(writePath string, envPath string) (envprovider.Environment, error) {
	env := envprovider.Environment{
		envprovider.Format(envprovider.EnvRoleARN, c.roleARN),
		envprovider.Format(envprovider.EnvWebIdentityTokenFile, filepath.Join(envPath, serviceAccountFilename)),
	}

	tokenPath := filepath.Join(writePath, serviceAccountFilename)

	var err error
	if c.webIdentityToken != "" {
		err = renameio.WriteFile(tokenPath, []byte(c.webIdentityToken), CredentialFilePerm)
	} else {
		err = util.ReplaceFile(tokenPath, c.webIdentityTokenFile, CredentialFilePerm)
	}
	if err != nil {
		return nil, err
	}

	// TODO: These were needed with `systemd` but probably won't be necessary with containerization - except disabling IMDS provider probably.
	if c.source == AuthenticationSourcePod {
		env = append(env,
			envprovider.Format(envprovider.EnvMountpointCacheKey, c.cacheKey),
			envprovider.Format(envprovider.EnvConfigFile, filepath.Join(envPath, "disable-config")),
			envprovider.Format(envprovider.EnvSharedCredentialsFile, filepath.Join(envPath, "disable-credentials")),
			envprovider.Format(envprovider.EnvEC2MetadataDisabled, "true"))
	}

	return env, nil
}
