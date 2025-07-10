package awsprofile_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider/awsprofile"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider/awsprofile/awsprofiletest"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

const testAccessKeyId = "test-access-key-id"
const testSecretAccessKey = "test-secret-access-key"
const testSessionToken = "test-session-token"
const testFilePerm = fs.FileMode(0600)

func TestCreatingAWSProfile(t *testing.T) {
	defaultSettings := awsprofile.Settings{
		Basepath: t.TempDir(),
		Prefix:   "test-",
		FilePerm: testFilePerm,
	}

	t.Run("create config and credentials files", func(t *testing.T) {
		creds := awsprofile.Credentials{
			AccessKeyID:     testAccessKeyId,
			SecretAccessKey: testSecretAccessKey,
			SessionToken:    testSessionToken,
		}
		profile, err := awsprofile.Create(defaultSettings, creds)
		assert.NoError(t, err)
		assertCredentialsFromAWSProfile(t, defaultSettings, profile, testAccessKeyId, testSecretAccessKey, testSessionToken)
	})

	t.Run("create config and credentials files with empty session token", func(t *testing.T) {
		creds := awsprofile.Credentials{
			AccessKeyID:     testAccessKeyId,
			SecretAccessKey: testSecretAccessKey,
		}
		profile, err := awsprofile.Create(defaultSettings, creds)
		assert.NoError(t, err)
		assertCredentialsFromAWSProfile(t, defaultSettings, profile, testAccessKeyId, testSecretAccessKey, "")
	})

	t.Run("ensure config and credentials files are created with correct permissions", func(t *testing.T) {
		creds := awsprofile.Credentials{
			AccessKeyID:     testAccessKeyId,
			SecretAccessKey: testSecretAccessKey,
			SessionToken:    testSessionToken,
		}
		profile, err := awsprofile.Create(defaultSettings, creds)
		assert.NoError(t, err)
		assertCredentialsFromAWSProfile(t, defaultSettings, profile, testAccessKeyId, testSecretAccessKey, testSessionToken)

		configStat, err := os.Stat(filepath.Join(defaultSettings.Basepath, profile.ConfigFilename))
		assert.NoError(t, err)
		assert.Equals(t, testFilePerm, configStat.Mode())

		credentialsStat, err := os.Stat(filepath.Join(defaultSettings.Basepath, profile.CredentialsFilename))
		assert.NoError(t, err)
		assert.Equals(t, testFilePerm, credentialsStat.Mode())
	})

	t.Run("fail if credentials contains non-ascii characters", func(t *testing.T) {
		t.Run("access key ID", func(t *testing.T) {
			creds := awsprofile.Credentials{
				AccessKeyID:     testAccessKeyId + "\n\t\r credential_process=exit",
				SecretAccessKey: testSecretAccessKey,
				SessionToken:    testSessionToken,
			}
			_, err := awsprofile.Create(defaultSettings, creds)
			assert.Equals(t, true, errors.Is(err, awsprofile.ErrInvalidCredentials))
		})
		t.Run("secret access key", func(t *testing.T) {
			creds := awsprofile.Credentials{
				AccessKeyID:     testAccessKeyId,
				SecretAccessKey: testSecretAccessKey + "\n",
				SessionToken:    testSessionToken,
			}
			_, err := awsprofile.Create(defaultSettings, creds)
			assert.Equals(t, true, errors.Is(err, awsprofile.ErrInvalidCredentials))
		})
		t.Run("session token", func(t *testing.T) {
			creds := awsprofile.Credentials{
				AccessKeyID:     testAccessKeyId,
				SecretAccessKey: testSecretAccessKey,
				SessionToken:    testSessionToken + "\n\r",
			}
			_, err := awsprofile.Create(defaultSettings, creds)
			assert.Equals(t, true, errors.Is(err, awsprofile.ErrInvalidCredentials))
		})
	})
}

func TestCleaningUpAWSProfile(t *testing.T) {
	settings := awsprofile.Settings{
		Basepath: t.TempDir(),
		Prefix:   "test-",
		FilePerm: testFilePerm,
	}

	t.Run("clean config and credentials files", func(t *testing.T) {
		creds := awsprofile.Credentials{
			AccessKeyID:     testAccessKeyId,
			SecretAccessKey: testSecretAccessKey,
			SessionToken:    testSessionToken,
		}

		profile, err := awsprofile.Create(settings, creds)
		assert.NoError(t, err)
		assertCredentialsFromAWSProfile(t, settings, profile, testAccessKeyId, testSecretAccessKey, testSessionToken)

		err = awsprofile.Cleanup(settings)
		assert.NoError(t, err)

		_, err = os.Stat(filepath.Join(settings.Basepath, profile.ConfigFilename))
		assert.Equals(t, true, errors.Is(err, fs.ErrNotExist))

		_, err = os.Stat(filepath.Join(settings.Basepath, profile.CredentialsFilename))
		assert.Equals(t, true, errors.Is(err, fs.ErrNotExist))
	})

	t.Run("cleaning non-existent config and credentials files should not be an error", func(t *testing.T) {
		err := awsprofile.Cleanup(settings)
		assert.NoError(t, err)
	})
}

func assertCredentialsFromAWSProfile(t *testing.T, settings awsprofile.Settings, profile awsprofile.Profile, accessKeyID string, secretAccessKey string, sessionToken string) {
	awsprofiletest.AssertCredentialsFromAWSProfile(
		t,
		profile.Name,
		settings.FilePerm,
		filepath.Join(settings.Basepath, profile.ConfigFilename),
		filepath.Join(settings.Basepath, profile.CredentialsFilename),
		accessKeyID,
		secretAccessKey,
		sessionToken,
	)
}
