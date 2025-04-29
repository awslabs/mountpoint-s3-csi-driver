package csimounter_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/awslabs/aws-s3-csi-driver/cmd/aws-s3-csi-mounter/csimounter"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/mounter/mountertest"
	"github.com/awslabs/aws-s3-csi-driver/pkg/mountpoint/mountoptions"
	"github.com/awslabs/aws-s3-csi-driver/pkg/util/testutil/assert"
)

const successExitCode = 0
const restartExitCode = 1

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
		assert.Equals(t, restartExitCode, exitCode)
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
		assert.Equals(t, restartExitCode, exitCode)
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
				Fd:         int(mountertest.OpenDevNull(t).Fd()),
				BucketName: "test-bucket",
				Env:        env,
			},
			CmdRunner: runner,
		})
		assert.NoError(t, err)
		assert.Equals(t, restartExitCode, exitCode)
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
		assert.Equals(t, restartExitCode, exitCode)

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
		assert.Equals(t, restartExitCode, exitCode)
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

	t.Run("Writes `mount.err` file if Mountpoint fails", func(t *testing.T) {
		basepath := t.TempDir()
		mountErrPath := filepath.Join(basepath, "mount.err")
		mountpointErr := errors.New("Mountpoint failed due to missing credentials")

		dev := mountertest.OpenDevNull(t)
		runner := func(c *exec.Cmd) (int, error) {
			mountertest.AssertSameFile(t, dev, c.ExtraFiles[0])
			assert.Equals(t, mountpointPath, c.Path)
			assert.Equals(t, []string{mountpointPath, "test-bucket", "/dev/fd/3"}, c.Args[:3])

			// Emulate Mountpoint writing errors to `stderr`.
			_, err := c.Stderr.Write([]byte(mountpointErr.Error()))
			assert.NoError(t, err)

			return restartExitCode, mountpointErr
		}

		exitCode, err := csimounter.Run(csimounter.Options{
			MountpointPath: mountpointPath,
			MountErrPath:   mountErrPath,
			MountOptions: mountoptions.Options{
				Fd:         int(dev.Fd()),
				BucketName: "test-bucket",
			},
			CmdRunner: runner,
		})
		assert.Equals(t, mountpointErr, err)
		assert.Equals(t, restartExitCode, exitCode)

		errMsg, err := os.ReadFile(mountErrPath)
		assert.NoError(t, err)
		assert.Equals(t, mountpointErr.Error(), string(errMsg))
	})

	t.Run("Exists with zero code if `mount.exit` file exist", func(t *testing.T) {
		basepath := t.TempDir()
		mountExitPath := filepath.Join(basepath, "mount.exit")

		dev := mountertest.OpenDevNull(t)
		runner := func(c *exec.Cmd) (int, error) {
			mountertest.AssertSameFile(t, dev, c.ExtraFiles[0])
			assert.Equals(t, mountpointPath, c.Path)
			assert.Equals(t, []string{mountpointPath, "test-bucket", "/dev/fd/3"}, c.Args[:3])
			return 1, nil
		}

		// Create `mount.exit` file
		_, err := os.OpenFile(mountExitPath, os.O_RDONLY|os.O_CREATE, 0666)
		assert.NoError(t, err)

		exitCode, err := csimounter.Run(csimounter.Options{
			MountpointPath: mountpointPath,
			MountExitPath:  mountExitPath,
			MountOptions: mountoptions.Options{
				Fd:         int(dev.Fd()),
				BucketName: "test-bucket",
			},
			CmdRunner: runner,
		})
		assert.NoError(t, err)
		// Should be `successExitCode` even though Mountpoint exited with a different exit code
		assert.Equals(t, successExitCode, exitCode)
	})
}
