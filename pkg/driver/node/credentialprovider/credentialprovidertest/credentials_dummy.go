package credentialprovidertest

import (
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/envprovider"
)

type DummyCredentials struct {
	AuthenticationSource credentialprovider.AuthenticationSource
	DumpFn               func(string, string) (envprovider.Environment, error)
}

func (c *DummyCredentials) Source() credentialprovider.AuthenticationSource {
	return c.AuthenticationSource
}

func (c *DummyCredentials) Dump(writePath string, envPath string) (envprovider.Environment, error) {
	if c.DumpFn != nil {
		return c.DumpFn(writePath, envPath)
	}
	return nil, nil
}
