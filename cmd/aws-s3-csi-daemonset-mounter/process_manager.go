package main

import (
	"bytes"
	"errors"
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

	"github.com/shirou/gopsutil/v4/process"
	"k8s.io/klog/v2"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/mounter"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mountoptions"
)

const errorFilePerm = fs.FileMode(0600)
const errorFileExt = ".error"

// TODO: Remove to use per-mount UID with process isolation lands, with its defined permissions.
const cacheDirPerm = fs.FileMode(0770)

// ProcessManager tracks and manages Mountpoint child processes.
type ProcessManager struct {
	commDir  string
	cacheDir string        // the cache volume's mount path, or "" when this container has none
	runner   ProcessRunner // interface for spawning processes; substituted in tests
	memory   memoryLimit
	cache    cacheLimit

	mu        sync.Mutex
	processes map[string]ProcessHandle // mountId -> process handle
	wg        sync.WaitGroup           // tracks waiter goroutines
}

func NewProcessManager(commDir, cacheDir string, runner ProcessRunner, memory memoryLimit, cache cacheLimit) *ProcessManager {
	return &ProcessManager{
		commDir:   commDir,
		cacheDir:  cacheDir,
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

	// We point --cache at a directory derived here, so the one created and removed is the one Mountpoint uses.
	cached := args.Has(mountpoint.ArgCache)
	for args.Has(mountpoint.ArgCache) {
		args.Remove(mountpoint.ArgCache)
	}
	if cached {
		if pm.cacheDir == "" {
			fuseDev.Close()
			return fmt.Errorf("refusing to launch mount %s: it requests a cache, but this container has no cache volume", mountId)
		}
		args.Set(mountpoint.ArgCache, filepath.Join(pm.cacheDir, mountId))
	}

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
	// Delete any error files that earlier Mountpoint of this PV wrote after node deletes error files.
	// TODO if we add process to ensure Mountpoint exited, this should not be needed.
	os.Remove(filepath.Join(pm.commDir, mountId+errorFileExt))

	if cached {
		if err := pm.createCacheDir(mountId); err != nil {
			pm.mu.Unlock()
			fuseDev.Close()
			return err
		}
	}

	handle, err := pm.runner.Start(cmd)
	if err != nil {
		if cached {
			if rmErr := removeCacheDir(pm.cacheDir, mountId); rmErr != nil {
				klog.Errorf("Failed to remove cache directory of mount %s after a failed start: %v", mountId, rmErr)
			}
		}
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

		// Before freeing mountId, so a relaunch cannot create the directory this then removes.
		if cached {
			if err := removeCacheDir(pm.cacheDir, mountId); err != nil {
				klog.Errorf("Failed to remove cache directory of mount %s: %v", mountId, err)
			}
		}

		pm.mu.Lock()
		// Before freeing mountId, so a relaunch's removal of a stale error file always comes after this write.
		if exitCode != 0 {
			pm.writeErrorFile(mountId, stderr)
		}
		delete(pm.processes, mountId)
		pm.mu.Unlock()

		if exitCode != 0 {
			klog.Errorf("Mountpoint for mount %s exited with code %d", mountId, exitCode)
		} else {
			klog.Infof("Mountpoint for mount %s exited cleanly", mountId)
		}
	}()

	return nil
}

// createCacheDir creates a cache PV-specific directory for a mount. Mountpoint writes ./mountpoint-cache inside it.
func (pm *ProcessManager) createCacheDir(mountId string) error {
	// Start empty rather than trusting leftover subdirectories whose contents might belong to another UID.
	// Note RemoveAll does not follow symlinks, so we won't need to Lstat to check.
	if err := removeCacheDir(pm.cacheDir, mountId); err != nil {
		return fmt.Errorf("failed to clear cache directory for mount %s: %w", mountId, err)
	}
	mountCacheDir := filepath.Join(pm.cacheDir, mountId)
	if err := os.Mkdir(mountCacheDir, cacheDirPerm); err != nil {
		return fmt.Errorf("failed to create cache directory %q for mount %s: %w", mountCacheDir, mountId, err)
	}
	// Chmod through a handle opened without following symlinks, so swapping the name cannot redirect it.
	dir, err := os.OpenFile(mountCacheDir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("failed to open cache directory %q for mount %s: %w", mountCacheDir, mountId, err)
	}
	defer dir.Close()
	// Mkdir subtracts the umask, which typically clears the group write bit, so Chmod again.
	if err := dir.Chmod(cacheDirPerm); err != nil {
		return fmt.Errorf("failed to set permissions on cache directory %q for mount %s: %w", mountCacheDir, mountId, err)
	}

	klog.V(4).Infof("Created cache directory %s for mount %s", mountCacheDir, mountId)
	return nil
}

// removeCacheDir removes a mount's cache directory.
// Note Mountpoint also removes its own cache when it exits cleanly.
func removeCacheDir(cacheDir, mountId string) error {
	// We guard escapes and partial cache delete attempts by checking the cacheDir and mountId, and returning error if something is wrong.
	// handleConnection already validates mountId; so only a new caller of Launch could lead to these problems.
	if cacheDir == "" || mountId == "" || mountId == "." || mountId == ".." || strings.ContainsRune(mountId, filepath.Separator) {
		return fmt.Errorf("refusing to remove cache directory %q for mount %q: not a cache volume and a plain directory name", cacheDir, mountId)
	}
	return os.RemoveAll(filepath.Join(cacheDir, mountId))
}

// removeLeftoverCacheDirs empties the cache volume, and returns an error for anything it could not remove.
func (pm *ProcessManager) removeLeftoverCacheDirs() error {
	if pm.cacheDir == "" {
		return nil
	}
	entries, err := os.ReadDir(pm.cacheDir)
	if err != nil {
		return fmt.Errorf("failed to list cache volume %q: %w", pm.cacheDir, err)
	}
	var errs []error
	leftovers := make(map[string]bool, len(entries))
	for _, entry := range entries {
		leftovers[entry.Name()] = true
		if err := os.RemoveAll(filepath.Join(pm.cacheDir, entry.Name())); err != nil {
			errs = append(errs, err)
		}
	}

	// List again rather than trust the removals: anything left, or created meanwhile, is reported.
	remaining, err := os.ReadDir(pm.cacheDir)
	if err != nil {
		return fmt.Errorf("failed to list cache volume %q: %w", pm.cacheDir, err)
	}
	var notRemoved, appeared []string
	for _, entry := range remaining {
		if leftovers[entry.Name()] {
			notRemoved = append(notRemoved, entry.Name())
		} else {
			appeared = append(appeared, entry.Name())
		}
	}
	if len(notRemoved) > 0 {
		errs = append(errs, fmt.Errorf("cache volume %q still holds %v after cleanup", pm.cacheDir, notRemoved))
	}
	if len(appeared) > 0 {
		errs = append(errs, fmt.Errorf("cache volume %q gained %v during cleanup", pm.cacheDir, appeared))
	}
	return errors.Join(errs...)
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
