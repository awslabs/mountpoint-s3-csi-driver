package credentialprovider_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/scality/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/scality/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider/awsprofile/awsprofiletest"
	"github.com/scality/mountpoint-s3-csi-driver/pkg/driver/node/envprovider"
	"github.com/scality/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

const testAccessKeyID = "test-access-key-id"
const testSecretAccessKey = "test-secret-access-key"
const testSessionToken = "test-session-token"

const testPodID = "2a17db00-0bf3-4052-9b3f-6c89dcee5d79"
const testVolumeID = "test-vol"
const testProfilePrefix = testPodID + "-" + testVolumeID + "-"

const testPodLevelServiceAccountToken = testPodID + "-" + testVolumeID + ".token"
const testDriverLevelServiceAccountToken = "token"

const testPodServiceAccount = "test-sa"
const testPodNamespace = "test-ns"

const testEnvPath = "/test-env"

func TestProvidingDriverLevelCredentials(t *testing.T) {
	provider := credentialprovider.New(nil)

	authenticationSourceVariants := []string{
		credentialprovider.AuthenticationSourceDriver,
		// It should fallback to Driver-level if authentication source is unspecified.
		credentialprovider.AuthenticationSourceUnspecified,
	}

	t.Run("only long-term credentials", func(t *testing.T) {
		for _, authSource := range authenticationSourceVariants {
			setEnvForLongTermCredentials(t)

			writePath := t.TempDir()
			provideCtx := credentialprovider.ProvideContext{
				AuthenticationSource: authSource,
				WritePath:            writePath,
				EnvPath:              testEnvPath,
				PodID:                testPodID,
				VolumeID:             testVolumeID,
			}

			env, source, err := provider.Provide(context.Background(), provideCtx)
			assert.NoError(t, err)
			assert.Equals(t, credentialprovider.AuthenticationSourceDriver, source)
			assert.Equals(t, envprovider.Environment{
				"AWS_PROFILE":                 testProfilePrefix + "s3-csi",
				"AWS_CONFIG_FILE":             "/test-env/" + testProfilePrefix + "s3-csi-config",
				"AWS_SHARED_CREDENTIALS_FILE": "/test-env/" + testProfilePrefix + "s3-csi-credentials",
			}, env)
			assertLongTermCredentials(t, writePath)
		}
	})

	t.Run("missing credentials", func(t *testing.T) {
		// Clear environment variables to test credential validation
		t.Setenv("AWS_ACCESS_KEY_ID", "")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "")
		t.Setenv("AWS_SESSION_TOKEN", "")

		writePath := t.TempDir()
		provideCtx := credentialprovider.ProvideContext{
			AuthenticationSource: credentialprovider.AuthenticationSourceDriver,
			WritePath:            writePath,
			EnvPath:              testEnvPath,
			PodID:                testPodID,
			VolumeID:             testVolumeID,
		}

		_, _, err := provider.Provide(context.Background(), provideCtx)
		assert.Equals(t, "credentialprovider: static IAM credentials not provided via environment variables", err.Error())
	})

	t.Run("missing access key", func(t *testing.T) {
		// Only set secret access key without access key
		t.Setenv("AWS_ACCESS_KEY_ID", "")
		t.Setenv("AWS_SECRET_ACCESS_KEY", testSecretAccessKey)

		writePath := t.TempDir()
		provideCtx := credentialprovider.ProvideContext{
			AuthenticationSource: credentialprovider.AuthenticationSourceDriver,
			WritePath:            writePath,
			EnvPath:              testEnvPath,
			PodID:                testPodID,
			VolumeID:             testVolumeID,
		}

		_, _, err := provider.Provide(context.Background(), provideCtx)
		assert.Equals(t, "credentialprovider: static IAM credentials not provided via environment variables", err.Error())
	})

	t.Run("missing secret key", func(t *testing.T) {
		// Only set access key without secret
		t.Setenv("AWS_ACCESS_KEY_ID", testAccessKeyID)
		t.Setenv("AWS_SECRET_ACCESS_KEY", "")

		provider := credentialprovider.New(nil)
		writePath := t.TempDir()
		provideCtx := credentialprovider.ProvideContext{
			AuthenticationSource: credentialprovider.AuthenticationSourceDriver,
			WritePath:            writePath,
			EnvPath:              testEnvPath,
			PodID:                testPodID,
			VolumeID:             testVolumeID,
		}

		_, _, err := provider.Provide(context.Background(), provideCtx)
		assert.Equals(t, "credentialprovider: static IAM credentials not provided via environment variables", err.Error())
	})
}

func TestCleanup(t *testing.T) {
	t.Run("cleanup driver level", func(t *testing.T) {
		// Provide/create long-term AWS credentials first
		setEnvForLongTermCredentials(t)
		provider := credentialprovider.New(nil)

		writePath := t.TempDir()
		provideCtx := credentialprovider.ProvideContext{
			AuthenticationSource: credentialprovider.AuthenticationSourceDriver,
			WritePath:            writePath,
			EnvPath:              testEnvPath,
			PodID:                testPodID,
			VolumeID:             testVolumeID,
		}

		env, source, err := provider.Provide(context.Background(), provideCtx)
		assert.NoError(t, err)
		assert.Equals(t, credentialprovider.AuthenticationSourceDriver, source)
		assert.Equals(t, testProfilePrefix+"s3-csi", env["AWS_PROFILE"])
		assert.Equals(t, "/test-env/"+testProfilePrefix+"s3-csi-config", env["AWS_CONFIG_FILE"])
		assert.Equals(t, "/test-env/"+testProfilePrefix+"s3-csi-credentials", env["AWS_SHARED_CREDENTIALS_FILE"])
		assertLongTermCredentials(t, writePath)

		// Perform cleanup
		err = provider.Cleanup(credentialprovider.CleanupContext{
			WritePath: writePath,
			PodID:     testPodID,
			VolumeID:  testVolumeID,
		})
		assert.NoError(t, err)

		// Verify files were removed
		_, err = os.Stat(filepath.Join(writePath, testProfilePrefix+"s3-csi-config"))
		if err == nil {
			t.Fatalf("AWS Config should be cleaned up")
		}
		assert.Equals(t, fs.ErrNotExist, err)

		_, err = os.Stat(filepath.Join(writePath, testProfilePrefix+"s3-csi-credentials"))
		if err == nil {
			t.Fatalf("AWS Credentials should be cleaned up")
		}
		assert.Equals(t, fs.ErrNotExist, err)
	})
}

func setEnvForLongTermCredentials(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", testAccessKeyID)
	t.Setenv("AWS_SECRET_ACCESS_KEY", testSecretAccessKey)
	t.Setenv("AWS_SESSION_TOKEN", testSessionToken)
}

func assertLongTermCredentials(t *testing.T, basepath string) {
	config, err := awsprofiletest.ReadConfig(filepath.Join(basepath, testProfilePrefix+"s3-csi-config"))
	assert.NoError(t, err)
	assert.Equals(t, map[string]map[string]string{
		"profile " + testProfilePrefix + "s3-csi": {},
	}, config)

	credentials, err := awsprofiletest.ReadCredentials(filepath.Join(basepath, testProfilePrefix+"s3-csi-credentials"))
	assert.NoError(t, err)
	assert.Equals(t, map[string]map[string]string{
		testProfilePrefix + "s3-csi": {
			"aws_access_key_id":     testAccessKeyID,
			"aws_secret_access_key": testSecretAccessKey,
			"aws_session_token":     testSessionToken,
		},
	}, credentials)
}

func TestProvideWithSecretAuthSource(t *testing.T) {
	tests := []struct {
		name         string
		secretData   map[string]string
		expectError  bool
		expectedAuth credentialprovider.AuthenticationSource
	}{
		{
			name: "valid credentials",
			secretData: map[string]string{
				"key_id":     "ACCESS123",
				"access_key": "SECRET456",
			},
			expectError:  false,
			expectedAuth: credentialprovider.AuthenticationSourceSecret,
		},
		{
			name: "missing key_id",
			secretData: map[string]string{
				"access_key": "SECRET456",
			},
			expectError: true,
		},
		{
			name: "missing access_key",
			secretData: map[string]string{
				"key_id": "ACCESS123",
			},
			expectError: true,
		},
		{
			name:        "empty secret",
			secretData:  map[string]string{},
			expectError: true,
		},
		{
			name: "invalid key_id format",
			secretData: map[string]string{
				"key_id":     "Invalid@Key", // Contains non-alphanumeric character
				"access_key": "SECRET456",
			},
			expectError: true,
		},
		{
			name: "invalid access_key format",
			secretData: map[string]string{
				"key_id":     "ACCESS123",
				"access_key": "Invalid@Secret", // Contains invalid character
			},
			expectError: true,
		},
		{
			name: "unexpected keys",
			secretData: map[string]string{
				"key_id":     "ACCESS123",
				"access_key": "SECRET456",
				"extra_key":  "ignored",
			},
			expectError:  false, // Should ignore the extra key
			expectedAuth: credentialprovider.AuthenticationSourceSecret,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := credentialprovider.New(nil)

			provideCtx := credentialprovider.ProvideContext{
				VolumeID:             "test-volume-id",
				AuthenticationSource: credentialprovider.AuthenticationSourceSecret,
				SecretData:           tt.secretData,
			}

			env, authSource, err := provider.Provide(context.Background(), provideCtx)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got nil")
				}
			} else {
				assert.NoError(t, err)
				assert.Equals(t, tt.expectedAuth, authSource)
				if env == nil {
					t.Errorf("Expected environment to be not nil")
				}
			}
		})
	}
}

func TestProvideWithUnknownAuthSource(t *testing.T) {
	provider := credentialprovider.New(nil)

	writePath := t.TempDir()
	provideCtx := credentialprovider.ProvideContext{
		AuthenticationSource: "unknown-source", // Using an unknown authentication source
		WritePath:            writePath,
		EnvPath:              testEnvPath,
		PodID:                testPodID,
		VolumeID:             testVolumeID,
	}

	env, source, err := provider.Provide(context.Background(), provideCtx)

	// Verify error was returned
	if err == nil {
		t.Errorf("Expected error for unknown authentication source, got nil")
	}

	// Verify error message contains all supported auth sources
	expectedErrMsg := "unknown `authenticationSource`: unknown-source, only `driver` (default option if not specified) and `secret` supported"
	if err.Error() != expectedErrMsg {
		t.Errorf("Expected error message %q, got %q", expectedErrMsg, err.Error())
	}

	// Verify returned values
	assert.Equals(t, credentialprovider.AuthenticationSourceUnspecified, source)
	assert.Equals(t, envprovider.Environment(nil), env)
}
