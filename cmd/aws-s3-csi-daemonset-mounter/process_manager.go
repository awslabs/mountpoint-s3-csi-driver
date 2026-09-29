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
	"sync"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v4/process"
	"k8s.io/klog/v2"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/mounter"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mountoptions"
)

const errorFilePerm = fs.FileMode(0600)
const errorFileExt = ".error"

// ProcessManager tracks and manages Mountpoint child processes.
type ProcessManager struct {
	commDir string
	runner  ProcessRunner // interface for spawning processes; substituted in tests
	memory  memoryLimit
	cache   cacheLimit

	mu        sync.Mutex
	processes map[string]ProcessHandle // mountId -> process handle
	wg        sync.WaitGroup           // tracks waiter goroutines
}

func NewProcessManager(commDir string, runner ProcessRunner, memory memoryLimit, cache cacheLimit) *ProcessManager {
	return &ProcessManager{
		commDir:   commDir,
		runner:    runner,
		memory:    memory,
		cache:     cache,
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

	if options.Uid < mounter.UIDRangeStart || options.Uid > mounter.UIDRangeEnd {
		fuseDev.Close()
		return fmt.Errorf("refusing to launch mount %s with out-of-range UID %d", mountId, options.Uid)
	}
	if options.Gid != options.Uid {
		fuseDev.Close()
		return fmt.Errorf("refusing to launch mount %s with GID %d not matching UID %d", mountId, options.Gid, options.Uid)
	}

	args := mountpoint.ParseArgs(options.Args)
	args.Set(mountpoint.ArgForeground, mountpoint.ArgNoValue)

	if targetMiB := pm.memory.targetFor(mountId, args); targetMiB > 0 {
		args.Set(mountpoint.ArgMemoryTarget, strconv.FormatInt(targetMiB, 10))
	}
	if sizeMiB, ok := pm.cache.maxCacheSizeFor(mountId, args); ok {
		args.Set(mountpoint.ArgMaxCacheSize, strconv.FormatInt(sizeMiB, 10))
	}

	cmdArgs := append([]string{
		options.BucketName,
		"/dev/fd/3", // ExtraFiles[0] becomes fd 3
	}, args.SortedList()...)

	cmd := exec.Command(mountpointPath, cmdArgs...)
	cmd.ExtraFiles = []*os.File{fuseDev}

	cmd.Env = options.Env
	cmd.Stdout = newPrefixWriter(os.Stdout, mountId)
	cmd.Stderr = newPrefixWriter(os.Stderr, mountId)

	// Give the child the per-mount credentials csi-node determined, so the kernel isolates it from
	// every other Mountpoint on this node. Supplementary groups are cleared: the mounter runs as
	// root, and inheriting its groups would hand the child access to every other mount's files.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid:    options.Uid,
			Gid:    options.Gid,
			Groups: []uint32{},
		},
	}

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
			pm.writeErrorFile(mountId, stderr)
			klog.Errorf("Mountpoint for mount %s exited with code %d", mountId, exitCode)
		} else {
			klog.Infof("Mountpoint for mount %s exited cleanly", mountId)
		}
	}()

	return nil
}

// writeErrorFile reports a mount failure to the driver, whose waitForMount polls for this file — the
// only reply channel on the otherwise one-way mount socket.
func (pm *ProcessManager) writeErrorFile(mountId string, content []byte) {
	errPath := filepath.Join(pm.commDir, mountId+errorFileExt)
	// TODO(vlaad): write error file atomically (open,write,rename)
	if err := os.WriteFile(errPath, content, errorFilePerm); err != nil {
		klog.Errorf("Failed to write error file for mount %s: %v", mountId, err)
	}
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
		klog.Infof("Status: tracked=%d actual_children=%d open_fds=%d goroutines=%d memory_limit_strategy=%s share_mib=%d cache_limit_strategy=%s cache_share_mib=%d mounts=%v",
			tracked, actual, openFDs, goroutines, pm.memory.strategy, pm.memory.shareMiB,
			pm.cache.strategy, pm.cache.shareMiB, mountIds)
	}
}

func countOpenFDs() int {
	p, err := process.NewProcess(int32(os.Getpid()))
	if err != nil {
		return -1
	}
	n, err := p.NumFDs()
	if err != nil {
		return -1
	}
	return int(n)
}

func countChildProcesses() int {
	p, err := process.NewProcess(int32(os.Getpid()))
	if err != nil {
		return -1
	}
	children, err := p.Children()
	if err != nil {
		return -1
	}
	return len(children)
}
