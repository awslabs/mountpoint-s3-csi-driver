package awsprofile_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/scality/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider/awsprofile"
	"github.com/scality/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider/awsprofile/awsprofiletest"
	"github.com/scality/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

const (
	testAccessKeyID     = "AKIAEXAMPLE"
	testSecretAccessKey = "secretkey123"
	testSessionToken    = "sessiontoken456"
	testFilePerm        = fs.FileMode(0600)
)

// ------------------------------------------------------------------
// Create() happy paths
// ------------------------------------------------------------------

func TestCreateProfile_WithAndWithoutSessionToken(t *testing.T) {
	dir := t.TempDir()
	settings := awsprofile.Settings{Basepath: dir, Prefix: "my-", FilePerm: testFilePerm}

	cases := []struct {
		name        string
		creds       awsprofile.Credentials
		expectToken string
	}{
		{
			name: "with session token",
			creds: awsprofile.Credentials{
				AccessKeyID:     testAccessKeyID,
				SecretAccessKey: testSecretAccessKey,
				SessionToken:    testSessionToken,
			},
			expectToken: testSessionToken,
		},
		{
			name: "without session token",
			creds: awsprofile.Credentials{
				AccessKeyID:     testAccessKeyID,
				SecretAccessKey: testSecretAccessKey,
			},
			expectToken: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			profile, err := awsprofile.Create(settings, tc.creds)
			assert.NoError(t, err)

			awsprofiletest.AssertCredentialsFromAWSProfile(
				t,
				profile.Name,
				testFilePerm,
				filepath.Join(dir, profile.ConfigFilename),
				filepath.Join(dir, profile.CredentialsFilename),
				testAccessKeyID,
				testSecretAccessKey,
				tc.expectToken,
			)
		})
	}
}

// ------------------------------------------------------------------
// Invalid credentials
// ------------------------------------------------------------------

func TestCreateProfile_InvalidCredentials(t *testing.T) {
	dir := t.TempDir()
	settings := awsprofile.Settings{Basepath: dir, Prefix: "bad-", FilePerm: testFilePerm}

	invalids := []awsprofile.Credentials{
		{AccessKeyID: "bad\nid", SecretAccessKey: testSecretAccessKey},
		{AccessKeyID: testAccessKeyID, SecretAccessKey: "bad\tsecret"},
		{AccessKeyID: testAccessKeyID, SecretAccessKey: testSecretAccessKey, SessionToken: "bad\rtoken"},
	}

	for i, creds := range invalids {
		if _, err := awsprofile.Create(settings, creds); !errors.Is(err, awsprofile.ErrInvalidCredentials) {
			t.Fatalf("case %d: expected ErrInvalidCredentials, got %v", i, err)
		}
	}
}

// ------------------------------------------------------------------
// Cleanup
// ------------------------------------------------------------------

func TestCleanupProfile(t *testing.T) {
	dir := t.TempDir()
	settings := awsprofile.Settings{Basepath: dir, Prefix: "cleanup-", FilePerm: testFilePerm}

	creds := awsprofile.Credentials{
		AccessKeyID:     testAccessKeyID,
		SecretAccessKey: testSecretAccessKey,
	}

	profile, err := awsprofile.Create(settings, creds)
	assert.NoError(t, err)

	// First cleanup should delete files.
	assert.NoError(t, awsprofile.Cleanup(settings))
	_, err = os.Stat(filepath.Join(dir, profile.ConfigFilename))
	assert.Equals(t, true, errors.Is(err, fs.ErrNotExist))
	_, err = os.Stat(filepath.Join(dir, profile.CredentialsFilename))
	assert.Equals(t, true, errors.Is(err, fs.ErrNotExist))

	// Second cleanup should be a no‑op.
	assert.NoError(t, awsprofile.Cleanup(settings))
}
