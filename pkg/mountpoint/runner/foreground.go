package runner

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
)

// ErrMissingBinaryPath is returned when Mountpoint binary path is empty.
var ErrMissingBinaryPath = errors.New("runner: missing Mountpoint binary path")

// ErrMissingBucketName is returned when S3 Bucket name is empty.
var ErrMissingBucketName = errors.New("runner: missing S3 Bucket name")

// A ForegroundOptions represents options for running Mountpoint in the foreground.
type ForegroundOptions struct {
	// Path to the Mountpoint binary `mount-s3`.
	BinaryPath string
	// Name of the S3 Bucket to mount.
	BucketName string
	// FUSE file descriptor for Mountpoint process to communicate with the kernel.
	// Can be obtained using `github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mounter` package.
	Fd int
	// Mountpoint arguments.
	Args mountpoint.Args
	// Mountpoint processes's environment variables.
	Env []string
	// Command runner to use, if nil, [DefaultCmdRunner] will be used.
	CmdRunner CmdRunner
}

// RunInForeground runs Mountpoint in the foreground until completion.
// It returns Mountpoint processes's exit code, standard error output (if Mountpunt ran and fail), and any error occurred.
// It redirects Mountpoint processes's stdout and stderr to calling process.
func RunInForeground(opts ForegroundOptions) (ExitCode, []byte, error) {
	if opts.BinaryPath == "" {
		return 0, nil, ErrMissingBinaryPath
	}
	if opts.BucketName == "" {
		return 0, nil, ErrMissingBucketName
	}
	if opts.CmdRunner == nil {
		opts.CmdRunner = DefaultCmdRunner
	}

	fuseDev := os.NewFile(uintptr(opts.Fd), "/dev/fuse")
	if fuseDev == nil {
		return 0, nil, fmt.Errorf("runner: passed file descriptor %d is not a valid FUSE file descriptor", opts.Fd)
	}
	defer fuseDev.Close()

	mountpointArgs := opts.Args

	// By default Mountpoint runs in a detached mode. Here we want to monitor it by relaying its output,
	// and also we want to wait until it terminates. We're passing `--foreground` to achieve this.
	mountpointArgs.Set(mountpoint.ArgForeground, mountpoint.ArgNoValue)

	args := append([]string{
		opts.BucketName,
		// We pass FUSE fd using `ExtraFiles`, and each entry becomes as file descriptor 3+i.
		"/dev/fd/3",
	}, mountpointArgs.SortedList()...)

	cmd := exec.Command(opts.BinaryPath, args...)
	cmd.ExtraFiles = []*os.File{fuseDev}
	cmd.Env = opts.Env

	var stderrBuf bytes.Buffer
	// Connect Mountpoint's stdout/stderr to this commands stdout/stderr,
	// as we're running Mountpoint in the foreground.
	cmd.Stdout = os.Stdout
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderrBuf)

	exitCode, err := opts.CmdRunner(cmd)
	if err != nil {
		return exitCode, stderrBuf.Bytes(), err
	}

	return exitCode, nil, nil
}
