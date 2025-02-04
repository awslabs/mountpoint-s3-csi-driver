package csimounter

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"

	"k8s.io/klog/v2"

	"github.com/awslabs/aws-s3-csi-driver/pkg/mountpoint"
	"github.com/awslabs/aws-s3-csi-driver/pkg/podmounter/mountoptions"
	"github.com/awslabs/aws-s3-csi-driver/pkg/podmounter/mppod"
)

// mountErrorPath is the path to write error logs if Mountpoint fails to mount.
var mountErrorPath = mppod.PathInsideMountpointPod(mppod.KnownPathMountError)
var mountErrorFileperm = fs.FileMode(0600) // only owner readable and writeable

// successExitCode is the exit code returned from `aws-s3-csi-mounter` to indicate a clean exit,
// so Kubernetes doesn't have to restart it and transition the Pod into `Succeeded` state.
const successExitCode = 0

// A CmdRunner is responsible for running given `cmd` until completion and returning its exit code and its error (if any).
// This is mainly exposed for mocking in tests, `DefaultCmdRunner` is always used in non-test environments.
type CmdRunner func(cmd *exec.Cmd) (int, error)

// DefaultCmdRunner is a real CmdRunner implementation that runs given `cmd`.
func DefaultCmdRunner(cmd *exec.Cmd) (int, error) {
	err := cmd.Run()
	if err != nil {
		return 0, err
	}
	return cmd.ProcessState.ExitCode(), nil
}

// An Options represents options to use while mounting Mountpoint.
type Options struct {
	MountpointPath string
	MountExitPath  string
	MountOptions   mountoptions.Options
	CmdRunner      CmdRunner
}

// Run runs Mountpoint with given options until completion and returns its exit code and its error (if any).
func Run(options Options) (int, error) {
	if options.CmdRunner == nil {
		options.CmdRunner = DefaultCmdRunner
	}

	mountOptions := options.MountOptions

	fuseDev := os.NewFile(uintptr(mountOptions.Fd), "/dev/fuse")
	if fuseDev == nil {
		return 0, fmt.Errorf("passed file descriptor %d is invalid", mountOptions.Fd)
	}

	mountpointArgs := mountpoint.ParseArgs(mountOptions.Args)

	// By default Mountpoint runs in a detached mode. Here we want to monitor it by relaying its output,
	// and also we want to wait until it terminates. We're passing `--foreground` to achieve this.
	mountpointArgs.Set(mountpoint.ArgForeground, mountpoint.ArgNoValue)

	args := append([]string{
		mountOptions.BucketName,
		// We pass FUSE fd using `ExtraFiles`, and each entry becomes as file descriptor 3+i.
		"/dev/fd/3",
	}, mountpointArgs.SortedList()...)

	cmd := exec.Command(options.MountpointPath, args...)
	cmd.ExtraFiles = []*os.File{fuseDev}
	cmd.Env = options.MountOptions.Env

	var stderrBuf bytes.Buffer

	// Connect Mountpoint's stdout/stderr to this commands stdout/stderr,
	// so Mountpoint logs can be viewable with `kubectl logs`.
	cmd.Stdout = os.Stdout
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderrBuf)

	exitCode, err := options.CmdRunner(cmd)
	if err != nil {
		// If Mountpoint fails, write it to `mountErrorPath` to let `PodMounter` running in the same node know.
		if writeErr := os.WriteFile(mountErrorPath, stderrBuf.Bytes(), mountErrorFileperm); writeErr != nil {
			klog.Infof("Failed to write mount error logs to %s: %v\n", mountErrorPath, err)
		}
		return 0, err
	}

	if checkIfFileExists(options.MountExitPath) {
		// If `mount.exit` is exists, that means the CSI Driver Node Pod unmounted the filesystem
		// and we should cleanly exit regardless of Mountpoint's exit-code.
		return successExitCode, nil
	}

	return exitCode, nil
}

// checkIfFileExists checks whether given `path` exists.
func checkIfFileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
