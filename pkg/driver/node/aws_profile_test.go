package node_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node"
)

const testAccessKeyId = "test-access-key-id"
const testSecretAccessKey = "test-secret-access-key"
const testSessionToken = "test-session-token"

func TestCreatingAWSProfile(t *testing.T) {
	t.Run("create config and credentials files", func(t *testing.T) {
		profile, err := node.CreateAWSProfile(t.TempDir(), testAccessKeyId, testSecretAccessKey, testSessionToken)
		assertNoError(t, err)
		assertCredentialsFromAWSProfile(t, profile, testAccessKeyId, testSecretAccessKey, testSessionToken)
	})

	t.Run("create config and credentials files with empty session token", func(t *testing.T) {
		profile, err := node.CreateAWSProfile(t.TempDir(), testAccessKeyId, testSecretAccessKey, "")
		assertNoError(t, err)
		assertCredentialsFromAWSProfile(t, profile, testAccessKeyId, testSecretAccessKey, "")
	})

	t.Run("ensure config and credentials files are owner readable only", func(t *testing.T) {
		profile, err := node.CreateAWSProfile(t.TempDir(), testAccessKeyId, testSecretAccessKey, testSessionToken)
		assertNoError(t, err)
		assertCredentialsFromAWSProfile(t, profile, testAccessKeyId, testSecretAccessKey, testSessionToken)

		configStat, err := os.Stat(profile.ConfigPath)
		assertNoError(t, err)
		assertEquals(t, 0400, configStat.Mode())

		credentialsStat, err := os.Stat(profile.CredentialsPath)
		assertNoError(t, err)
		assertEquals(t, 0400, credentialsStat.Mode())
	})

	t.Run("fail if credentials contains non-ascii characters", func(t *testing.T) {
		t.Run("access key ID", func(t *testing.T) {
			_, err := node.CreateAWSProfile(t.TempDir(), testAccessKeyId+"\n\t\r credential_process=exit", testSecretAccessKey, testSessionToken)
			assertEquals(t, true, errors.Is(err, node.ErrInvalidCredentials))
		})
		t.Run("secret access key", func(t *testing.T) {
			_, err := node.CreateAWSProfile(t.TempDir(), testAccessKeyId, testSecretAccessKey+"\n", testSessionToken)
			assertEquals(t, true, errors.Is(err, node.ErrInvalidCredentials))
		})
		t.Run("session token", func(t *testing.T) {
			_, err := node.CreateAWSProfile(t.TempDir(), testAccessKeyId, testSecretAccessKey, testSessionToken+"\n\r")
			assertEquals(t, true, errors.Is(err, node.ErrInvalidCredentials))
		})
	})
}

func TestCleaningUpAWSProfile(t *testing.T) {
	t.Run("clean config and credentials files", func(t *testing.T) {
		basepath := t.TempDir()

		profile, err := node.CreateAWSProfile(basepath, testAccessKeyId, testSecretAccessKey, testSessionToken)
		assertNoError(t, err)
		assertCredentialsFromAWSProfile(t, profile, testAccessKeyId, testSecretAccessKey, testSessionToken)

		err = node.CleanupAWSProfile(basepath)
		assertNoError(t, err)

		_, err = os.Stat(profile.ConfigPath)
		assertEquals(t, true, errors.Is(err, fs.ErrNotExist))

		_, err = os.Stat(profile.CredentialsPath)
		assertEquals(t, true, errors.Is(err, fs.ErrNotExist))
	})

	t.Run("cleaning non-existent config and credentials files should not be an error", func(t *testing.T) {
		err := node.CleanupAWSProfile(t.TempDir())
		assertNoError(t, err)
	})
}

func assertCredentialsFromAWSProfile(t *testing.T, profile node.AWSProfile, accessKeyID string, secretAccessKey string, sessionToken string) {
	credentials := parseAWSProfile(t, profile)
	assertEquals(t, accessKeyID, credentials.AccessKeyID)
	assertEquals(t, secretAccessKey, credentials.SecretAccessKey)
	assertEquals(t, sessionToken, credentials.SessionToken)
}

func parseAWSProfile(t *testing.T, profile node.AWSProfile) aws.Credentials {
	sharedConfig, err := config.LoadSharedConfigProfile(context.Background(), profile.Name, func(c *config.LoadSharedConfigOptions) {
		c.ConfigFiles = []string{profile.ConfigPath}
		c.CredentialsFiles = []string{profile.CredentialsPath}
	})
	assertNoError(t, err)
	return sharedConfig.Credentials
}

func assertEquals[T comparable](t *testing.T, expected T, got T) {
	if expected != got {
		t.Errorf("Expected %#v, Got %#v", expected, got)
	}
}

func assertNoError(t *testing.T, err error) {
	if err != nil {
		t.Errorf("Expected no error, but got: %s", err)
	}
}
