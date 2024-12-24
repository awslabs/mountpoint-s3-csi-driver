package awsprofile_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/credentialprovider/awsprofile"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/credentialprovider/awsprofile/awsprofiletest"
	"github.com/awslabs/aws-s3-csi-driver/pkg/util/testutil/assert"
)

const testAccessKeyId = "test-access-key-id"
const testSecretAccessKey = "test-secret-access-key"
const testSessionToken = "test-session-token"
const testFilePerm = fs.FileMode(0600)

func TestCreatingAWSProfile(t *testing.T) {
	t.Run("create config and credentials files", func(t *testing.T) {
		basepath := t.TempDir()
		profile, err := awsprofile.CreateAWSProfile(basepath, testAccessKeyId, testSecretAccessKey, testSessionToken, testFilePerm)
		assert.NoError(t, err)
		assertCredentialsFromAWSProfile(t, basepath, profile, testAccessKeyId, testSecretAccessKey, testSessionToken)
	})

	t.Run("create config and credentials files with empty session token", func(t *testing.T) {
		basepath := t.TempDir()
		profile, err := awsprofile.CreateAWSProfile(basepath, testAccessKeyId, testSecretAccessKey, "", testFilePerm)
		assert.NoError(t, err)
		assertCredentialsFromAWSProfile(t, basepath, profile, testAccessKeyId, testSecretAccessKey, "")
	})

	t.Run("ensure config and credentials files are created with correct permissions", func(t *testing.T) {
		basepath := t.TempDir()
		profile, err := awsprofile.CreateAWSProfile(basepath, testAccessKeyId, testSecretAccessKey, testSessionToken, testFilePerm)
		assert.NoError(t, err)
		assertCredentialsFromAWSProfile(t, basepath, profile, testAccessKeyId, testSecretAccessKey, testSessionToken)

		configStat, err := os.Stat(filepath.Join(basepath, profile.ConfigFilename))
		assert.NoError(t, err)
		assert.Equals(t, testFilePerm, configStat.Mode())

		credentialsStat, err := os.Stat(filepath.Join(basepath, profile.CredentialsFilename))
		assert.NoError(t, err)
		assert.Equals(t, testFilePerm, credentialsStat.Mode())
	})

	t.Run("fail if credentials contains non-ascii characters", func(t *testing.T) {
		t.Run("access key ID", func(t *testing.T) {
			_, err := awsprofile.CreateAWSProfile(t.TempDir(), testAccessKeyId+"\n\t\r credential_process=exit", testSecretAccessKey, testSessionToken, testFilePerm)
			assert.Equals(t, true, errors.Is(err, awsprofile.ErrInvalidCredentials))
		})
		t.Run("secret access key", func(t *testing.T) {
			_, err := awsprofile.CreateAWSProfile(t.TempDir(), testAccessKeyId, testSecretAccessKey+"\n", testSessionToken, testFilePerm)
			assert.Equals(t, true, errors.Is(err, awsprofile.ErrInvalidCredentials))
		})
		t.Run("session token", func(t *testing.T) {
			_, err := awsprofile.CreateAWSProfile(t.TempDir(), testAccessKeyId, testSecretAccessKey, testSessionToken+"\n\r", testFilePerm)
			assert.Equals(t, true, errors.Is(err, awsprofile.ErrInvalidCredentials))
		})
	})
}

func TestCleaningUpAWSProfile(t *testing.T) {
	t.Run("clean config and credentials files", func(t *testing.T) {
		basepath := t.TempDir()

		profile, err := awsprofile.CreateAWSProfile(basepath, testAccessKeyId, testSecretAccessKey, testSessionToken, testFilePerm)
		assert.NoError(t, err)
		assertCredentialsFromAWSProfile(t, basepath, profile, testAccessKeyId, testSecretAccessKey, testSessionToken)

		err = awsprofile.CleanupAWSProfile(basepath)
		assert.NoError(t, err)

		_, err = os.Stat(filepath.Join(basepath, profile.ConfigFilename))
		assert.Equals(t, true, errors.Is(err, fs.ErrNotExist))

		_, err = os.Stat(filepath.Join(basepath, profile.CredentialsFilename))
		assert.Equals(t, true, errors.Is(err, fs.ErrNotExist))
	})

	t.Run("cleaning non-existent config and credentials files should not be an error", func(t *testing.T) {
		err := awsprofile.CleanupAWSProfile(t.TempDir())
		assert.NoError(t, err)
	})
}

func assertCredentialsFromAWSProfile(t *testing.T, basepath string, profile awsprofile.AWSProfile, accessKeyID string, secretAccessKey string, sessionToken string) {
	awsprofiletest.AssertCredentialsFromAWSProfile(
		t,
		profile.Name,
		filepath.Join(basepath, profile.ConfigFilename),
		filepath.Join(basepath, profile.CredentialsFilename),
		accessKeyID,
		secretAccessKey,
		sessionToken,
	)
}
