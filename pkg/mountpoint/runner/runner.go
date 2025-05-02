// Package runner provides utilities for running Mountpoint instances.
package runner

import "os/exec"

// An ExitCode represents exit code of a Mountpoint process.
type ExitCode = int

// A CmdRunner is responsible for running given `cmd` until completion and returning its exit code and its error (if any).
// This is mainly exposed for mocking in tests, [DefaultCmdRunner] is always used in non-test environments.
type CmdRunner func(cmd *exec.Cmd) (ExitCode, error)

// DefaultCmdRunner is a real CmdRunner implementation that runs given `cmd`.
func DefaultCmdRunner(cmd *exec.Cmd) (ExitCode, error) {
	err := cmd.Run()
	if err != nil {
		return 0, err
	}
	return cmd.ProcessState.ExitCode(), nil
}
