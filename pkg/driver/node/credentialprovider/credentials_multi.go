package credentialprovider

import "github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/envprovider"

type multiCredentials struct {
	source      AuthenticationSource
	credentials []Credentials
}

func (c *multiCredentials) Source() AuthenticationSource {
	return c.source
}

func (c *multiCredentials) Dump(writePath string, envPath string) (envprovider.Environment, error) {
	environment := envprovider.Environment{}
	for _, c := range c.credentials {
		env, err := c.Dump(writePath, envPath)
		if err != nil {
			return nil, err
		}
		environment = append(environment, env...)
	}
	return environment, nil
}
