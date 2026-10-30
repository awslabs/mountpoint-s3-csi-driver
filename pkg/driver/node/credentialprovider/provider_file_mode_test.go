package credentialprovider_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

// Every credential file the driver writes for a mount carries the mode for its owner, whichever driver-level
// source provides it: owner-only (0400) and owned by the mount's UID in daemonset mode, so no other mount's
// Mountpoint can read it.
func TestProvideWritesDriverLevelCredentialFilesWithTheModeForTheirOwner(t *testing.T) {
	testutil.CleanRegionEnv(t)
	sources := []struct {
		name  string
		setup func(t *testing.T)
	}{
		{
			name: "long-term credentials from a Kubernetes Secret",
			setup: func(t *testing.T) {
				t.Setenv("AWS_ACCESS_KEY_ID", testAccessKeyID)
				t.Setenv("AWS_SECRET_ACCESS_KEY", testSecretAccessKey)
				t.Setenv("AWS_SESSION_TOKEN", testSessionToken)
			},
		},
		{
			name: "the driver's IRSA web identity token",
			setup: func(t *testing.T) {
				tokenFile := filepath.Join(t.TempDir(), "token")
				assert.NoError(t, os.WriteFile(tokenFile, []byte(testWebIdentityToken), 0600))
				t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", tokenFile)
				t.Setenv("AWS_ROLE_ARN", testRoleARN)
			},
		},
	}
	owners := []struct {
		name     string
		uid      uint32
		wantPerm fs.FileMode
	}{
		{name: "owned by a mount", uid: 65536, wantPerm: credentialprovider.IsolatedCredentialFilePerm},
		{name: "owned by csi-node", uid: 0, wantPerm: credentialprovider.CredentialFilePerm},
	}
	for _, source := range sources {
		for _, owner := range owners {
			t.Run(source.name+", "+owner.name, func(t *testing.T) {
				if owner.uid != 0 && os.Getuid() != 0 {
					t.Skip("handing a file to another UID needs root")
				}
				source.setup(t)
				writePath := t.TempDir()
				provider := credentialprovider.New(nil, dummyRegionProvider)

				_, _, err := provider.Provide(context.Background(), credentialprovider.ProvideContext{
					AuthenticationSource: credentialprovider.AuthenticationSourceDriver,
					WritePath:            writePath,
					EnvPath:              testEnvPath,
					WorkloadPodID:        testPodID,
					VolumeID:             testVolumeID,
					MountKind:            credentialprovider.MountKindDaemonset,
					Uid:                  owner.uid,
				})
				assert.NoError(t, err)

				entries, err := os.ReadDir(writePath)
				assert.NoError(t, err)
				if len(entries) == 0 {
					t.Fatal("expected Provide to write credential files")
				}
				for _, entry := range entries {
					stat, err := os.Stat(filepath.Join(writePath, entry.Name()))
					assert.NoError(t, err)
					if stat.Mode().Perm() != owner.wantPerm {
						t.Errorf("%s has mode %o, want %o", entry.Name(), stat.Mode().Perm(), owner.wantPerm)
					}
					if owner.uid != 0 {
						sys, ok := stat.Sys().(*syscall.Stat_t)
						assert.Equals(t, true, ok)
						assert.Equals(t, owner.uid, sys.Uid)
						assert.Equals(t, owner.uid, sys.Gid)
					}
				}
			})
		}
	}
}
