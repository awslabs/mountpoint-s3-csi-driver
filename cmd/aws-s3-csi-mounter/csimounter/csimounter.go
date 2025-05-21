package csimounter

import (
	"fmt"
	"io/fs"
	"os"

	"k8s.io/klog/v2"

	"github.com/awslabs/aws-s3-csi-driver/pkg/mountpoint"
	"github.com/awslabs/aws-s3-csi-driver/pkg/mountpoint/mountoptions"
	"github.com/awslabs/aws-s3-csi-driver/pkg/mountpoint/runner"
)

var mountErrorFileperm = fs.FileMode(0600) // only owner readable and writeable

// SuccessExitCode is the exit code returned from `aws-s3-csi-mounter` to indicate a clean exit,
// so Kubernetes doesn't have to restart it and transition the Pod into `Succeeded` state.
const SuccessExitCode = 0

// restartExitCode is the exit code returned from `aws-s3-csi-mounter` to indicate an error exit,
// so Kubernetes would restart it. This is the default exit code if `mount.exit` is not present,
// meaning the CSI Driver Node Pod didn't requested a clean exit.
const restartExitCode = 1

// An Options represents options to use while mounting Mountpoint.
type Options struct {
	MountpointPath string
	MountExitPath  string
	MountErrPath   string
	MountOptions   mountoptions.Options
	CmdRunner      runner.CmdRunner
}

// Run runs Mountpoint with given options until completion and returns its exit code and its error (if any).
func Run(options Options) (int, error) {
	mountOptions := options.MountOptions
	mountpointArgs := mountpoint.ParseArgs(mountOptions.Args)

	// TODO: This is a temporary workaround to create a cache folder if caching is enabled,
	// ideally we should create a volume (`emptyDir` by default) in the Mountpoint Pod and use that.
	mountpointArgs, err := createCacheDir(mountpointArgs)
	if err != nil {
		return 0, fmt.Errorf("failed to create cache dir: %w", err)
	}

	exitCode, stdErr, err := runner.RunInForeground(runner.ForegroundOptions{
		BinaryPath: options.MountpointPath,
		BucketName: mountOptions.BucketName,
		Fd:         mountOptions.Fd,
		Args:       mountpointArgs,
		Env:        mountOptions.Env,
		CmdRunner:  options.CmdRunner,
	})
	if err != nil {
		// If Mountpoint fails, write it to `options.MountErrPath` to let `PodMounter` running in the same node know.
		if writeErr := os.WriteFile(options.MountErrPath, stdErr, mountErrorFileperm); writeErr != nil {
			klog.Errorf("Failed to write mount error logs to %s: %v\n", options.MountErrPath, err)
		}
		return exitCode, err
	}

	if ShouldExitWithSuccessCode(options.MountExitPath) {
		return SuccessExitCode, nil
	}

	return restartExitCode, nil
}

// ShouldExitWithSuccessCode returns whether the container should exit with zero code.
// If `mount.exit` is exists, that means the CSI Driver Node Pod unmounted the filesystem
// and we should cleanly exit regardless of Mountpoint's exit-code.
func ShouldExitWithSuccessCode(mountExitPath string) bool {
	return checkIfFileExists(mountExitPath)
}

// checkIfFileExists checks whether given `path` exists.
func checkIfFileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// createCacheDir creates a temporary directory to use as a cache directory if caching is enabled in given `args`.
// It will replace the value of `--cache` with the created random directory.
func createCacheDir(args mountpoint.Args) (mountpoint.Args, error) {
	_, ok := args.Remove(mountpoint.ArgCache)
	if !ok {
		// Caching is not enabled
		return args, nil
	}

	// Caching is enabled, so create a temporary directory and pass it to `args`
	cacheDir, err := os.MkdirTemp(os.TempDir(), "mountpoint-s3-cache")
	if err != nil {
		return args, fmt.Errorf("failed to create cache directory: %w", err)
	}
	args.Set(mountpoint.ArgCache, cacheDir)
	return args, nil
}
