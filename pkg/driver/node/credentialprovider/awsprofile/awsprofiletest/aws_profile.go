// Package awsprofiletest provides testing utilities for AWS Profiles.
package awsprofiletest

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"

	"github.com/awslabs/aws-s3-csi-driver/pkg/util/testutil/assert"
)

func AssertCredentialsFromAWSProfile(t *testing.T, profileName, configFile, credentialsFile, accessKeyID, secretAccessKey, sessionToken string) {
	t.Helper()

	credentials := parseAWSProfile(t, profileName, configFile, credentialsFile)
	assert.Equals(t, accessKeyID, credentials.AccessKeyID)
	assert.Equals(t, secretAccessKey, credentials.SecretAccessKey)
	assert.Equals(t, sessionToken, credentials.SessionToken)
}

func parseAWSProfile(t *testing.T, profileName, configFile, credentialsFile string) aws.Credentials {
	sharedConfig, err := config.LoadSharedConfigProfile(context.Background(), profileName, func(c *config.LoadSharedConfigOptions) {
		c.ConfigFiles = []string{configFile}
		c.CredentialsFiles = []string{credentialsFile}
	})
	assert.NoError(t, err)
	return sharedConfig.Credentials
}
