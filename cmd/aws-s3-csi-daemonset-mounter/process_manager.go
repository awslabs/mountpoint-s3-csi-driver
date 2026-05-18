package main

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"k8s.io/klog/v2"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mountoptions"
)

const errorFilePerm = fs.FileMode(0600)
const errorFileExt = ".error"

// ProcessManager tracks and manages Mountpoint child processes.
type ProcessManager struct {
	commDir   string
	runner    ProcessRunner // interface for spawning processes; substituted in tests
	mu        sync.Mutex
	processes map[string]ProcessHandle // mountId -> process handle
	wg        sync.WaitGroup           // tracks waiter goroutines
}

func NewProcessManager(commDir string, runner ProcessRunner) *ProcessManager {
	return &ProcessManager{
		commDir:   commDir,
		runner:    runner,
		processes: make(map[string]ProcessHandle),
	}
}

// Launch spawns a Mountpoint process for the given mount and waits for it asynchronously.
// Takes ownership of options.Fd, caller must not close it after calling this function.
// Returns an error if a process with the same mountId is already running.
func (pm *ProcessManager) Launch(mountId string, mountpointPath string, options mountoptions.Options) error {
	fuseDev := os.NewFile(uintptr(options.Fd), "/dev/fuse")
	if fuseDev == nil {
		return fmt.Errorf("invalid FUSE file descriptor %d", options.Fd)
	}

	args := mountpoint.ParseArgs(options.Args)
	args.Set(mountpoint.ArgForeground, mountpoint.ArgNoValue)

	cmdArgs := append([]string{
		options.BucketName,
		"/dev/fd/3", // ExtraFiles[0] becomes fd 3
	}, args.SortedList()...)

	cmd := exec.Command(mountpointPath, cmdArgs...)
	cmd.ExtraFiles = []*os.File{fuseDev}

	// TODO: we might need to make the child to inherit credentials ENV from this process (for driver-level creds)
	// e.g. AWS_ROLE_ARN, AWS_WEB_IDENTITY_TOKEN_FILE,
	//      AWS_CONTAINER_CREDENTIALS_FULL_URI, AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE

	cmd.Env = options.Env
	cmd.Stdout = newPrefixWriter(os.Stdout, mountId)
	cmd.Stderr = newPrefixWriter(os.Stderr, mountId)

	// Hold lock across duplicate check and process start to prevent races.
	pm.mu.Lock()
	if _, exists := pm.processes[mountId]; exists {
		pm.mu.Unlock()
		fuseDev.Close()
		return fmt.Errorf("mount %s already has a running process", mountId)
	}

	handle, err := pm.runner.Start(cmd)
	if err != nil {
		pm.mu.Unlock()
		fuseDev.Close()
		return fmt.Errorf("failed to start Mountpoint: %w", err)
	}

	// Child has its own copy of the FD (kernel dup'd it during fork/exec).
	fuseDev.Close()

	pm.processes[mountId] = handle
	pm.mu.Unlock()

	klog.Infof("Launched Mountpoint for mount %s (pid %d)", mountId, handle.Pid())

	pm.wg.Add(1)
	go func() {
		defer pm.wg.Done()
		exitCode, stderr := handle.Wait()

		pm.mu.Lock()
		delete(pm.processes, mountId)
		pm.mu.Unlock()

		if exitCode != 0 {
			errPath := filepath.Join(pm.commDir, mountId+errorFileExt)
			if writeErr := os.WriteFile(errPath, stderr, errorFilePerm); writeErr != nil {
				klog.Errorf("Failed to write error file for mount %s: %v", mountId, writeErr)
			}
			klog.Errorf("Mountpoint for mount %s exited with code %d", mountId, exitCode)
		} else {
			klog.Infof("Mountpoint for mount %s exited cleanly", mountId)
		}
	}()

	return nil
}

// Shutdown sends SIGTERM to all processes and waits for them to exit.
func (pm *ProcessManager) Shutdown() {
	pm.mu.Lock()
	for mountId, handle := range pm.processes {
		klog.Infof("Sending SIGTERM to Mountpoint for mount %s (pid %d)", mountId, handle.Pid())
		handle.Signal(syscall.SIGTERM)
	}
	pm.mu.Unlock()

	pm.wg.Wait()
}

// prefixWriter wraps an io.Writer and prefixes each line with a mount ID.
type prefixWriter struct {
	w      io.Writer
	prefix string
}

func newPrefixWriter(w io.Writer, mountId string) *prefixWriter {
	return &prefixWriter{w: w, prefix: fmt.Sprintf("[%s] ", mountId)}
}

func (pw *prefixWriter) Write(p []byte) (int, error) {
	lines := bytes.Split(p, []byte("\n"))
	for i, line := range lines {
		if len(line) == 0 && i == len(lines)-1 {
			break
		}
		if _, err := pw.w.Write([]byte(pw.prefix)); err != nil {
			return 0, err
		}
		if _, err := pw.w.Write(line); err != nil {
			return 0, err
		}
		if _, err := pw.w.Write([]byte("\n")); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// LogStatusPeriodically logs the number of tracked and actual child processes at the given interval.
func (pm *ProcessManager) LogStatusPeriodically(interval time.Duration) {
	for {
		time.Sleep(interval)

		pm.mu.Lock()
		tracked := len(pm.processes)
		var mountIds []string
		for id := range pm.processes {
			mountIds = append(mountIds, id)
		}
		pm.mu.Unlock()

		actual := countChildProcesses()
		openFDs := countOpenFDs()
		goroutines := runtime.NumGoroutine()
		klog.Infof("Status: tracked=%d actual_children=%d open_fds=%d goroutines=%d mounts=%v", tracked, actual, openFDs, goroutines, mountIds)
	}
}

// countOpenFDs counts open file descriptors of this process by reading /proc/self/fd.
func countOpenFDs() int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	return len(entries)
}

// countChildProcesses counts child processes of this process by reading /proc.
func countChildProcesses() int {
	myPid := os.Getpid()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return -1
	}

	count := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if err != nil {
			continue
		}
		fields := strings.SplitN(string(stat[strings.LastIndex(string(stat), ")")+2:]), " ", 3)
		if len(fields) >= 2 {
			ppid, _ := strconv.Atoi(fields[1])
			if ppid == myPid && pid != myPid {
				count++
			}
		}
	}
	return count
}
