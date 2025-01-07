package csimounter_test

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/awslabs/aws-s3-csi-driver/cmd/aws-s3-csi-mounter/csimounter"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/mounter/mountertest"
	"github.com/awslabs/aws-s3-csi-driver/pkg/podmounter/mountoptions"
	"github.com/awslabs/aws-s3-csi-driver/pkg/util/testutil/assert"
)

func TestRunningMountpoint(t *testing.T) {
	mountpointPath := filepath.Join(t.TempDir(), "mount-s3")

	t.Run("Passes bucket name and FUSE device as mount point", func(t *testing.T) {
		dev := mountertest.OpenDevNull(t)

		runner := func(c *exec.Cmd) (int, error) {
			mountertest.AssertSameFile(t, dev, c.ExtraFiles[0])
			assert.Equals(t, mountpointPath, c.Path)
			assert.Equals(t, []string{mountpointPath, "test-bucket", "/dev/fd/3"}, c.Args[:3])
			return 0, nil
		}

		exitCode, err := csimounter.Run(csimounter.Options{
			MountpointPath: mountpointPath,
			MountOptions: mountoptions.Options{
				Fd:         int(dev.Fd()),
				BucketName: "test-bucket",
			},
			CmdRunner: runner,
		})
		assert.NoError(t, err)
		assert.Equals(t, 0, exitCode)
	})

	t.Run("Passes bucket name", func(t *testing.T) {
		runner := func(c *exec.Cmd) (int, error) {
			assert.Equals(t, mountpointPath, c.Path)
			assert.Equals(t, []string{mountpointPath, "test-bucket"}, c.Args[:2])
			return 0, nil
		}

		exitCode, err := csimounter.Run(csimounter.Options{
			MountpointPath: mountpointPath,
			MountOptions: mountoptions.Options{
				Fd:         int(mountertest.OpenDevNull(t).Fd()),
				BucketName: "test-bucket",
			},
			CmdRunner: runner,
		})
		assert.NoError(t, err)
		assert.Equals(t, 0, exitCode)
	})

	t.Run("Passes environment variables", func(t *testing.T) {
		env := []string{"FOO=bar", "BAZ=qux"}

		runner := func(c *exec.Cmd) (int, error) {
			assert.Equals(t, env, c.Env)
			return 0, nil
		}

		exitCode, err := csimounter.Run(csimounter.Options{
			MountpointPath: mountpointPath,
			MountOptions: mountoptions.Options{
				Fd:  int(mountertest.OpenDevNull(t).Fd()),
				Env: env,
			},
			CmdRunner: runner,
		})
		assert.NoError(t, err)
		assert.Equals(t, 0, exitCode)
	})

	t.Run("Adds `--foreground` argument if not passed", func(t *testing.T) {
		runner := func(c *exec.Cmd) (int, error) {
			assert.Equals(t, []string{
				mountpointPath,
				"test-bucket", "/dev/fd/3",
				"--foreground",
			}, c.Args)
			return 0, nil
		}

		exitCode, err := csimounter.Run(csimounter.Options{
			MountpointPath: mountpointPath,
			MountOptions: mountoptions.Options{
				Fd:         int(mountertest.OpenDevNull(t).Fd()),
				BucketName: "test-bucket",
			},
			CmdRunner: runner,
		})
		assert.NoError(t, err)
		assert.Equals(t, 0, exitCode)

		exitCode, err = csimounter.Run(csimounter.Options{
			MountpointPath: mountpointPath,
			MountOptions: mountoptions.Options{
				Fd:         int(mountertest.OpenDevNull(t).Fd()),
				BucketName: "test-bucket",
				Args:       []string{"--foreground"},
			},
			CmdRunner: runner,
		})
		assert.NoError(t, err)
		assert.Equals(t, 0, exitCode)
	})

	t.Run("Fails if file descriptor is invalid", func(t *testing.T) {
		_, err := csimounter.Run(csimounter.Options{
			MountpointPath: mountpointPath,
			MountOptions: mountoptions.Options{
				Fd:         -1,
				BucketName: "test-bucket",
			},
		})
		assert.Equals(t, cmpopts.AnyError, err)
	})
}
