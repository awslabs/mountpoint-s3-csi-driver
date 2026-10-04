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

// cacheVolumePerm lets a Mountpoint reach its own cache directory but not create or list entries beside it.
const cacheVolumePerm = fs.FileMode(0711)

// mountCacheDirPerm closes a mount's cache directory to every UID but the one its Mountpoint runs as.
const mountCacheDirPerm = fs.FileMode(0700)

// emptyDirArg makes this binary the removal helper, which empties a cache directory as its owner, since the mounter cannot read inside one.
const emptyDirArg = "empty-cache-dir"

// ProcessManager tracks and manages Mountpoint child processes.
type ProcessManager struct {
	commDir         string
	cacheDir        string        // the cache volume's mount path, or "" when this container has none
	runner          ProcessRunner // interface for spawning processes; substituted in tests
	memory          memoryLimit
	cache           cacheLimit
	chownForTesting func(path string, uid, gid int) error // unprivileged tests record ownership instead of chown

	mu        sync.Mutex
	processes map[uint32]mountpointProcess // the UID a Mountpoint runs as -> that Mountpoint; one per UID
	wg        sync.WaitGroup               // tracks waiter goroutines
}

// mountpointProcess is a running Mountpoint and the mount it serves.
type mountpointProcess struct {
	mountId string
	handle  ProcessHandle
}

func NewProcessManager(commDir, cacheDir string, runner ProcessRunner, memory memoryLimit, cache cacheLimit) *ProcessManager {
	return &ProcessManager{
		commDir:   commDir,
		cacheDir:  cacheDir,
		runner:    runner,
		memory:    memory,
		cache:     cache,
		processes: make(map[uint32]mountpointProcess),
	}
}

// secureCacheVolume gives the cache volume to root, so a Mountpoint can reach its own directory but not list or create beside it.
func (pm *ProcessManager) secureCacheVolume() error {
	if pm.cacheDir == "" {
		return nil
	}
	if err := pm.chownWithDefault(pm.cacheDir, 0, 0); err != nil {
		// NFS with root_squash / EFS access point doesn't allow root to change ownership
		return fmt.Errorf("cannot chown %s: the cache volume must allow root to change ownership: %w", pm.cacheDir, err)
	}
	if err := os.Chmod(pm.cacheDir, cacheVolumePerm); err != nil {
		return fmt.Errorf("cannot chmod %s to %o: %w", pm.cacheDir, cacheVolumePerm, err)
	}
	return nil
}

// emptyCacheVolume empties the /cache volume, and returns an error for anything it could not remove.
func (pm *ProcessManager) emptyCacheVolume() error {
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
		if err := pm.removeCacheVolumeEntry(entry.Name()); err != nil {
			errs = append(errs, err)
		}
	}

	// List again: anything left, or created during the deletion process, is reported.
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

// Launch spawns a Mountpoint process for the given mount and waits for it asynchronously.
// Takes ownership of options.Fd, caller must not close it after calling this function.
// Returns an error if a process with the same mountId or UID is already running.
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
	mountCacheDir := mountCacheDirName(options.Uid)
	if cached {
		if pm.cacheDir == "" {
			fuseDev.Close()
			return fmt.Errorf("refusing to launch mount %s: it requests a cache, but this container has no cache volume", mountId)
		}
		args.Set(mountpoint.ArgCache, filepath.Join(pm.cacheDir, mountCacheDir))
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

	// Reject a used mount ID. processes holds at most maxVolumesPerNode entries, so an O(n) scan is acceptable.
	for _, p := range pm.processes {
		if p.mountId == mountId {
			pm.mu.Unlock()
			fuseDev.Close()
			return fmt.Errorf("mount %s already has a running process", mountId)
		}
	}

	// Reject used UID, or else two mounts would be able to access cache / credentials of each other.
	// We rely on csi-node's UID allocator moving its cursor on, so the NodePublishVolume retry gets another UID.
	if other, taken := pm.processes[options.Uid]; taken {
		pm.mu.Unlock()
		fuseDev.Close()
		// Don't expose PV name / mountID for other mounts in error which reaches pod events, and only log it
		klog.Errorf("Refusing to launch mount %s with UID %d already in use by mount %s", mountId, options.Uid, other.mountId)
		return fmt.Errorf("refusing to launch mount %s: UID %d is already in use by another mount", mountId, options.Uid)
	}
	// Delete any error files that earlier Mountpoint of this PV wrote after node deletes error files.
	// TODO if we add process to ensure Mountpoint exited, this should not be needed.
	os.Remove(filepath.Join(pm.commDir, mountId+errorFileExt))

	// Now that we're certain no running Mountpoint has this UID, the remaining cache directory for this UID is a leftover: remove it.
	if pm.cacheDir != "" {
		if err := pm.removeCacheVolumeEntry(mountCacheDir); err != nil {
			pm.mu.Unlock()
			fuseDev.Close()
			klog.Errorf("Failed to remove leftover cache directory %s: %v", mountCacheDir, err)
			return fmt.Errorf("failed to remove the leftover cache directory of UID %d, see the mounter log", options.Uid)
		}
	}
	if cached {
		if err := pm.createMountCacheDir(mountCacheDir, options.Uid); err != nil {
			pm.mu.Unlock()
			fuseDev.Close()
			return err
		}
	}

	handle, err := pm.runner.Start(cmd)
	if err != nil {
		if cached {
			if rmErr := pm.removeCacheVolumeEntry(mountCacheDir); rmErr != nil {
				klog.Errorf("Failed to remove cache directory of mount %s after a failed start: %v", mountId, rmErr)
			}
		}
		pm.mu.Unlock()
		fuseDev.Close()
		return fmt.Errorf("failed to start Mountpoint: %w", err)
	}

	// Child has its own copy of the FD (kernel dup'd it during fork/exec).
	fuseDev.Close()

	pm.processes[options.Uid] = mountpointProcess{mountId: mountId, handle: handle}
	pm.mu.Unlock()

	klog.Infof("Launched Mountpoint for mount %s as UID %d (pid %d)", mountId, options.Uid, handle.Pid())

	pm.wg.Add(1)
	go func() {
		defer pm.wg.Done()
		exitCode, stderr := handle.Wait()

		// Before freeing mountId and the UID, so a relaunch cannot create the directory this then removes.
		if cached {
			if err := pm.removeCacheVolumeEntry(mountCacheDir); err != nil {
				klog.Errorf("Failed to remove cache directory of mount %s: %v", mountId, err)
			}
		}

		pm.mu.Lock()
		// Before freeing mountId, so a relaunch's removal of a stale error file always comes after this write.
		if exitCode != 0 {
			pm.writeErrorFile(mountId, stderr)
		}
		delete(pm.processes, options.Uid)
		pm.mu.Unlock()

		if exitCode != 0 {
			klog.Errorf("Mountpoint for mount %s (UID %d) exited with code %d", mountId, options.Uid, exitCode)
		} else {
			klog.Infof("Mountpoint for mount %s (UID %d) exited cleanly", mountId, options.Uid)
		}
	}()

	return nil
}

// mountCacheDirName names the cache directory of the Mountpoint running as uid.
func mountCacheDirName(uid uint32) string {
	return fmt.Sprintf("uid-%d", uid)
}

// removeCacheVolumeEntry removes an entry of the cache volume, whoever owns it.
// Note Mountpoint also removes its own cache when it exits cleanly.
func (pm *ProcessManager) removeCacheVolumeEntry(entryName string) error {
	// We guard escapes and partial cache delete attempts by checking the cacheDir and entryName, and returning error if something is wrong.
	// Defensive: Names come from mountCacheDirName or from listing the cache volume; so only a new caller could lead to these problems.
	if pm.cacheDir == "" || entryName == "" || entryName == "." || entryName == ".." || strings.ContainsRune(entryName, filepath.Separator) {
		return fmt.Errorf("refusing to remove cache directory %q in %q: not a cache volume and a plain directory name", entryName, pm.cacheDir)
	}
	path := filepath.Join(pm.cacheDir, entryName)
	// Root owns the cache volume, so it can remove an empty directory or a file, but cannot read inside another UID's directory.
	err := os.Remove(path)
	// Emptying a non-empty dir needs its owner.
	if errors.Is(err, syscall.ENOTEMPTY) {
		info, statErr := os.Lstat(path)
		if statErr != nil {
			return fmt.Errorf("failed to find the owner of cache directory %q: %w", path, statErr)
		}
		owner := info.Sys().(*syscall.Stat_t).Uid
		// Log error if UID is not the one in the name, but do not fail as we're deleting it now anyways.
		if strings.HasPrefix(entryName, "uid-") && entryName != mountCacheDirName(owner) {
			klog.Errorf("Cache directory %q is owned by UID %d, not the UID its name gives; removing it anyway", path, owner)
		}
		if owner == 0 {
			// Root's own tree (should not normally happen): remove it here, so the helper only ever runs as a non-root UID.
			err = os.RemoveAll(path)
		} else {
			// Use the helper (this binary with emptyDirArg flag) to empty it as that UID.
			if err := pm.spawnRemovalHelper(path, owner); err != nil {
				return err
			}
			// Remove the directory itself now that it is empty.
			err = os.Remove(path)
		}
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("failed to remove cache directory %q: %w", path, err)
	}
	return nil
}

// spawnRemovalHelper empties dir by running this binary as uid, with no groups and no capabilities.
func (pm *ProcessManager) spawnRemovalHelper(dir string, uid uint32) error {
	// As root the helper would keep the mounter's capabilities; root's own trees are removed in the main binary instead.
	if uid == 0 {
		return fmt.Errorf("refusing to run the removal helper as root for %q", dir)
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to find this binary to empty %q: %w", dir, err)
	}
	klog.Infof("Emptying cache directory %s as UID %d with removal helper binary", dir, uid)
	cmd := exec.Command(self, emptyDirArg, dir)
	cmd.Stderr = newPrefixWriter(os.Stderr, "removal helper "+filepath.Base(dir))
	// No environment: a process of the same UID could read the helper's environment from /proc.
	cmd.Env = []string{}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: uid, Groups: []uint32{}}}
	handle, err := pm.runner.Start(cmd)
	if err != nil {
		return fmt.Errorf("failed to empty cache directory %q as UID %d: %w", dir, uid, err)
	}
	if exitCode, stderr := handle.Wait(); exitCode != 0 {
		return fmt.Errorf("failed to empty cache directory %q as UID %d: exit status %d: %s", dir, uid, exitCode, bytes.TrimSpace(stderr))
	}
	return nil
}

// runAsRemovalHelper empties a directory and exits, when spawnRemovalHelper started this binary as the removal helper.
func runAsRemovalHelper(args []string) {
	if len(args) != 3 || args[1] != emptyDirArg {
		return
	}
	if err := removalHelperEmptyDir(args[2]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// removalHelperEmptyDir is the removal helper's work: it removes everything inside dir but not dir, as only root can write the cache volume.
func removalHelperEmptyDir(dir string) error {
	err := removalHelperRemoveContents(dir)
	if !errors.Is(err, fs.ErrPermission) {
		return err
	}
	// A Mountpoint can make its own subdirectories unreadable; as their owner we can open them up again.
	fmt.Fprintf(os.Stderr, "Permission denied emptying %s, so making its subdirectories accessible to their owner and retrying: %v\n", dir, err)
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		// Directories only: Chmod follows symlinks, and WalkDir reports a symlink as not a directory.
		if err == nil && d.IsDir() {
			os.Chmod(path, mountCacheDirPerm)
		}
		return nil
	})
	return removalHelperRemoveContents(dir)
}

// removalHelperRemoveContents removes everything inside dir but not dir.
func removalHelperRemoveContents(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// createMountCacheDir creates a mount's cache directory, owned by the UID its Mountpoint runs as. Mountpoint writes ./mountpoint-cache inside it.
func (pm *ProcessManager) createMountCacheDir(mountCacheDir string, uid uint32) error {
	path := filepath.Join(pm.cacheDir, mountCacheDir)
	if err := os.Mkdir(path, mountCacheDirPerm); err != nil {
		return fmt.Errorf("failed to create cache directory %q: %w", path, err)
	}
	if err := pm.chownWithDefault(path, int(uid), int(uid)); err != nil {
		return fmt.Errorf("failed to hand cache directory %q to UID %d: %w", path, uid, err)
	}

	klog.V(4).Infof("Created cache directory %s", path)
	return nil
}

func (pm *ProcessManager) chownWithDefault(path string, uid, gid int) error {
	if pm.chownForTesting != nil {
		return pm.chownForTesting(path, uid, gid)
	}
	// Chown through a handle opened without following symlinks, so a link planted at path cannot redirect it.
	dir, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Chown(uid, gid)
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
	for _, p := range pm.processes {
		klog.Infof("Sending SIGTERM to Mountpoint for mount %s (pid %d)", p.mountId, p.handle.Pid())
		p.handle.Signal(syscall.SIGTERM)
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
		var mounts []string
		for uid, p := range pm.processes {
			mounts = append(mounts, fmt.Sprintf("%s:%d", p.mountId, uid))
		}
		pm.mu.Unlock()

		actual := countChildProcesses()
		openFDs := countOpenFDs()
		goroutines := runtime.NumGoroutine()
		klog.Infof("Status: tracked=%d actual_children=%d open_fds=%d goroutines=%d memory_limit_strategy=%s share_mib=%d cache_limit_strategy=%s cache_share_mib=%d mounts=%v",
			tracked, actual, openFDs, goroutines, pm.memory.strategy, pm.memory.shareMiB,
			pm.cache.strategy, pm.cache.shareMiB, mounts)
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
