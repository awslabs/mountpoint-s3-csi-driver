package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/mounter"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/mounter/mountertest"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mountoptions"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

// --- Fake ProcessRunner ---

type fakeProcessHandle struct {
	pid      int
	cmd      *exec.Cmd
	extraFds []uintptr // captured at Start time before close
	exitCode int
	stderr   []byte
	done     chan struct{}
	sigCh    chan os.Signal
}

func (h *fakeProcessHandle) Pid() int { return h.pid }

func (h *fakeProcessHandle) Wait() (int, []byte) {
	<-h.done
	return h.exitCode, h.stderr
}

func (h *fakeProcessHandle) Signal(sig os.Signal) error {
	h.sigCh <- sig
	return nil
}

// Exit makes Wait() return with the given code and stderr.
func (h *fakeProcessHandle) Exit(code int, stderr string) {
	h.exitCode = code
	h.stderr = []byte(stderr)
	close(h.done)
}

type fakeProcessRunner struct {
	mu         sync.Mutex
	nextPid    int
	handles    []*fakeProcessHandle
	startErr   error       // when set, Start fails instead of spawning
	helperCmds []*exec.Cmd // the removal helpers started, which run in-process as the test user
	helperErr  error       // when set, every removal helper fails instead of emptying
}

func (r *fakeProcessRunner) Start(cmd *exec.Cmd) (ProcessHandle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cmd.Args[1] == emptyDirArg {
		return r.runRemovalHelper(cmd), nil
	}
	if r.startErr != nil {
		return nil, r.startErr
	}
	r.nextPid++
	var extraFds []uintptr
	for _, f := range cmd.ExtraFiles {
		extraFds = append(extraFds, f.Fd())
	}
	h := &fakeProcessHandle{
		pid:      r.nextPid,
		cmd:      cmd,
		extraFds: extraFds,
		done:     make(chan struct{}),
		sigCh:    make(chan os.Signal, 1),
	}
	r.handles = append(r.handles, h)
	return h, nil
}

func (r *fakeProcessRunner) runRemovalHelper(cmd *exec.Cmd) ProcessHandle {
	r.helperCmds = append(r.helperCmds, cmd)
	h := &fakeProcessHandle{done: make(chan struct{})}
	if r.helperErr != nil {
		h.Exit(1, r.helperErr.Error())
	} else if err := removalHelperEmptyDir(cmd.Args[2]); err != nil {
		h.Exit(1, err.Error())
	} else {
		h.Exit(0, "")
	}
	return h
}

// newProcessManagerWithCache returns a manager whose container has a cache volume, and that volume.
// Tests are not root, so it does not chown; a test that checks ownership replaces pm.chownForTesting.
func newProcessManagerWithCache(t *testing.T, fr *fakeProcessRunner, cache cacheLimit) (*ProcessManager, string) {
	t.Helper()
	cacheDir := t.TempDir()
	pm := NewProcessManager(t.TempDir(), cacheDir, fr, memoryLimit{strategy: memoryLimitNone}, cache)
	pm.chownForTesting = func(string, int, int) error { return nil }
	return pm, cacheDir
}

// recordChowns makes pm record what it chowns, keyed by path, as [uid, gid]; only the test's goroutine chowns, so no lock.
func recordChowns(pm *ProcessManager) map[string][2]int {
	owners := map[string][2]int{}
	pm.chownForTesting = func(path string, uid, gid int) error {
		owners[path] = [2]int{uid, gid}
		return nil
	}
	return owners
}

func assertNotExist(t *testing.T, path string) {
	t.Helper()
	_, err := os.Lstat(path)
	assert.Equals(t, true, errors.Is(err, fs.ErrNotExist))
}

// --- Tests ---

func TestHandleConnection_PropagatesOptionsToRunner(t *testing.T) {
	commDir := t.TempDir()
	fr := &fakeProcessRunner{}
	pm := NewProcessManager(commDir, "", fr, memoryLimit{strategy: memoryLimitNone}, cacheLimit{strategy: cacheLimitNone})

	sockPath := filepath.Join(commDir, "test.sock")
	listener, err := net.Listen("unix", sockPath)
	assert.NoError(t, err)
	defer listener.Close()

	dev := mountertest.OpenDevNull(t)

	// Send options via UDS in background
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		mountoptions.Send(ctx, sockPath, mountoptions.Options{
			Fd:         int(dev.Fd()),
			BucketName: "test-bucket",
			Args:       []string{"--region", "us-west-2"},
			Env:        []string{"AWS_REGION=us-west-2"},
			VolumeId:   "pod123-vol456",
			Uid:        65536,
			Gid:        65536,
		})
	}()

	conn, err := listener.Accept()
	assert.NoError(t, err)

	handleConnection(conn.(*net.UnixConn), "/opt/mount-s3", pm, 5*time.Second)

	// Verify runner received the options
	fr.mu.Lock()
	assert.Equals(t, 1, len(fr.handles))
	fr.mu.Unlock()

	cmd := fr.handles[0].cmd
	assert.Equals(t, "/opt/mount-s3", cmd.Path)
	assert.Equals(t, "test-bucket", cmd.Args[1])
	assert.Equals(t, []string{"AWS_REGION=us-west-2"}, cmd.Env)
	assert.Equals(t, 1, len(fr.handles[0].extraFds)) // FD was passed

	// The credentials must reach the child, or it would run as the mounter's root with no isolation.
	cred := cmd.SysProcAttr.Credential
	assert.Equals(t, uint32(65536), cred.Uid)
	assert.Equals(t, uint32(65536), cred.Gid)
	assert.Equals(t, 0, len(cred.Groups)) // supplementary groups cleared

	// Cleanup
	fr.handles[0].Exit(0, "")
	pm.Shutdown()
}

func TestProcessManager_SecureCacheVolume(t *testing.T) {
	t.Run("gives the cache volume to root as 0711, so a Mountpoint can reach its own directory but not list or create beside it", func(t *testing.T) {
		pm, cacheDir := newProcessManagerWithCache(t, &fakeProcessRunner{}, cacheLimit{strategy: cacheLimitNone})
		chowns := recordChowns(pm)

		assert.NoError(t, pm.secureCacheVolume())

		// Note: This assert chownForTesting called on cacheDir with UID: 0, GID: 0; details in recordChowns
		assert.Equals(t, [2]int{0, 0}, chowns[cacheDir])
		fi, err := os.Stat(cacheDir)
		assert.NoError(t, err)
		assert.Equals(t, fs.FileMode(0711), fi.Mode().Perm())
	})

	t.Run("fails saying the cache volume must let root change ownership, when root cannot chown it", func(t *testing.T) {
		pm, cacheDir := newProcessManagerWithCache(t, &fakeProcessRunner{}, cacheLimit{strategy: cacheLimitNone})
		pm.chownForTesting = func(string, int, int) error { return syscall.EPERM }

		err := pm.secureCacheVolume()
		if err == nil {
			t.Fatal("expected secureCacheVolume to fail when the chown fails")
		}
		assert.Contains(t, err.Error(), "cannot chown "+cacheDir+": the cache volume must allow root to change ownership")
	})

	t.Run("does nothing without a cache volume", func(t *testing.T) {
		pm := NewProcessManager(t.TempDir(), "", &fakeProcessRunner{}, memoryLimit{strategy: memoryLimitNone}, cacheLimit{strategy: cacheLimitNone})
		pm.chownForTesting = func(string, int, int) error { return syscall.EPERM }

		assert.NoError(t, pm.secureCacheVolume())
	})
}

func TestProcessManager_EmptyCacheVolume(t *testing.T) {
	t.Run("removes every entry in the cache volume, with its contents, but not a symlink's target", func(t *testing.T) {
		pm, cacheDir := newProcessManagerWithCache(t, &fakeProcessRunner{}, cacheLimit{strategy: cacheLimitNone})
		blockDir := filepath.Join(cacheDir, "pv-a", "mountpoint-cache", "V2", "ab")
		assert.NoError(t, os.MkdirAll(blockDir, 0700))
		assert.NoError(t, os.WriteFile(filepath.Join(blockDir, "block"), []byte("x"), 0600))
		assert.NoError(t, os.Mkdir(filepath.Join(cacheDir, "pv-b"), 0700))
		assert.NoError(t, os.Mkdir(filepath.Join(cacheDir, "lost+found"), 0700))
		strayFile := filepath.Join(cacheDir, "stray-file")
		assert.NoError(t, os.WriteFile(strayFile, []byte("x"), 0600))
		outside := t.TempDir()
		target := filepath.Join(outside, "keep")
		assert.NoError(t, os.WriteFile(target, []byte("x"), 0600))
		assert.NoError(t, os.Symlink(outside, filepath.Join(cacheDir, "pv-link")))

		assert.NoError(t, pm.emptyCacheVolume())

		entries, err := os.ReadDir(cacheDir)
		assert.NoError(t, err)
		assert.Equals(t, 0, len(entries))
		_, err = os.Lstat(target)
		assert.NoError(t, err)
	})

	t.Run("names an entry it cannot remove, with the reason, and still removes the others", func(t *testing.T) {
		// The helper fails as it would on a subtree another UID owns, which an unprivileged test cannot create.
		pm, cacheDir := newProcessManagerWithCache(t, &fakeProcessRunner{helperErr: errors.New("permission denied")}, cacheLimit{strategy: cacheLimitNone})
		stuck := filepath.Join(cacheDir, "pv-stuck", "mountpoint-cache")
		assert.NoError(t, os.MkdirAll(stuck, 0700))
		assert.NoError(t, os.WriteFile(filepath.Join(stuck, "block"), []byte("x"), 0600))
		// Listed after pv-stuck, so the cleanup is shown to carry on past a failure.
		assert.NoError(t, os.Mkdir(filepath.Join(cacheDir, "pv-z"), 0700))

		err := pm.emptyCacheVolume()
		if err == nil {
			t.Fatal("expected an error for the entry it could not remove")
		}
		assert.Contains(t, err.Error(), "permission denied")
		assert.Contains(t, err.Error(), "still holds [pv-stuck] after cleanup")
		assertNotExist(t, filepath.Join(cacheDir, "pv-z"))
	})

	t.Run("fails when the cache volume cannot be listed", func(t *testing.T) {
		pm := NewProcessManager(t.TempDir(), filepath.Join(t.TempDir(), "missing"), &fakeProcessRunner{},
			memoryLimit{strategy: memoryLimitNone}, cacheLimit{strategy: cacheLimitNone})

		err := pm.emptyCacheVolume()
		if err == nil {
			t.Fatal("expected an error for a cache volume that cannot be listed")
		}
		assert.Contains(t, err.Error(), "failed to list cache volume")
	})

	t.Run("does nothing without a cache volume", func(t *testing.T) {
		pm := NewProcessManager(t.TempDir(), "", &fakeProcessRunner{}, memoryLimit{strategy: memoryLimitNone}, cacheLimit{strategy: cacheLimitNone})

		// Note: without the "" check this would fail listing the working directory's empty path.
		assert.NoError(t, pm.emptyCacheVolume())
	})
}

func TestProcessManager_Launch_HappyPath(t *testing.T) {
	commDir := t.TempDir()
	fr := &fakeProcessRunner{}
	pm := NewProcessManager(commDir, "", fr, memoryLimit{strategy: memoryLimitNone}, cacheLimit{strategy: cacheLimitNone})
	dev := mountertest.OpenDevNull(t)

	err := pm.Launch("mount-123", "/usr/bin/mount-s3", mountoptions.Options{
		Uid:        65536,
		Gid:        65536,
		Fd:         int(dev.Fd()),
		BucketName: "my-bucket",
		Env:        []string{"AWS_REGION=us-east-1"},
	})
	assert.NoError(t, err)

	// Verify cmd args
	cmd := fr.handles[0].cmd
	assert.Equals(t, []string{"/usr/bin/mount-s3", "my-bucket", "/dev/fd/3", "--foreground"}, cmd.Args)
	assert.Equals(t, []uintptr{dev.Fd()}, fr.handles[0].extraFds)
	assert.Equals(t, []string{"AWS_REGION=us-east-1"}, cmd.Env)

	// Verify tracked
	pm.mu.Lock()
	assert.Equals(t, 1, len(pm.processes))
	assert.Equals(t, fr.handles[0].Pid(), pm.processes[65536].handle.Pid())
	pm.mu.Unlock()

	// Clean exit
	fr.handles[0].Exit(0, "")
	pm.Shutdown()

	// Verify untracked, no error file
	pm.mu.Lock()
	assert.Equals(t, 0, len(pm.processes))
	pm.mu.Unlock()

	_, err = os.ReadFile(filepath.Join(commDir, "mount-123.error"))
	assert.Equals(t, true, os.IsNotExist(err))
}

func TestProcessManager_Launch_MemoryTarget(t *testing.T) {
	testCases := []struct {
		name     string
		limit    memoryLimit
		args     []string
		wantArgs []string
	}{
		{
			name:     "equalSplit injects the node's share",
			limit:    mustMemoryLimit(t, memoryLimitEqualSplit, 4*gib, 4), // (4096 - 64) / 4 = 1008
			wantArgs: []string{"--foreground", "--memory-target=1008"},
		},
		{
			name:     "equalSplit overrides --memory-target from PV mountOptions",
			limit:    mustMemoryLimit(t, memoryLimitEqualSplit, 4*gib, 4),
			args:     []string{"--memory-target=2048"},
			wantArgs: []string{"--foreground", "--memory-target=1008"},
		},
		{
			name:     "equalSplit leaves other mount options alone",
			limit:    mustMemoryLimit(t, memoryLimitEqualSplit, 4*gib, 4),
			args:     []string{"--prefix=data/"},
			wantArgs: []string{"--foreground", "--memory-target=1008", "--prefix=data/"},
		},
		{
			name:     "none leaves --memory-target unset",
			limit:    mustMemoryLimit(t, memoryLimitNone, 4*gib, 4),
			wantArgs: []string{"--foreground"},
		},
		{
			name:     "none passes --memory-target from PV mountOptions through",
			limit:    mustMemoryLimit(t, memoryLimitNone, 4*gib, 4),
			args:     []string{"--memory-target=2048"},
			wantArgs: []string{"--foreground", "--memory-target=2048"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fr := &fakeProcessRunner{}
			pm := NewProcessManager(t.TempDir(), "", fr, tc.limit, cacheLimit{strategy: cacheLimitNone})
			dev := mountertest.OpenDevNull(t)

			options := mountoptions.Options{
				Fd:         int(dev.Fd()),
				BucketName: "my-bucket",
				Args:       tc.args,
				Uid:        mounter.UIDRangeStart,
				Gid:        mounter.UIDRangeStart,
			}
			assert.NoError(t, pm.Launch("mount-123", "/usr/bin/mount-s3", options))

			// cmd.Args is [binary, bucket, /dev/fd/3, ...sorted args].
			assert.Equals(t, tc.wantArgs, fr.handles[0].cmd.Args[3:])

			// The wire slice is the driver's mount-sharing key and is persisted to meta, so a target
			// leaking into it would make running mounts look incompatible with themselves after a resize.
			assert.Equals(t, tc.args, options.Args)

			fr.handles[0].Exit(0, "")
			pm.Shutdown()
		})
	}
}

func TestProcessManager_Launch_TracksNothingWhenStartFails(t *testing.T) {
	fr := &fakeProcessRunner{startErr: errors.New("fork/exec: no such file")}
	pm := NewProcessManager(t.TempDir(), "", fr, mustMemoryLimit(t, memoryLimitEqualSplit, 4*gib, 4), cacheLimit{strategy: cacheLimitNone})
	dev := mountertest.OpenDevNull(t)

	err := pm.Launch("mount-doomed", "/usr/bin/mount-s3", mountoptions.Options{
		Fd:         int(dev.Fd()),
		BucketName: "bucket",
		Uid:        mounter.UIDRangeStart,
		Gid:        mounter.UIDRangeStart,
	})
	if err == nil {
		t.Fatal("expected Launch to fail when the process cannot start")
	}

	pm.mu.Lock()
	assert.Equals(t, 0, len(pm.processes))
	pm.mu.Unlock()
}

func TestProcessManager_Launch_MaxCacheSize(t *testing.T) {
	// What the driver sends; the mounter replaces it with its own directory for the mount.
	const cacheArg = "--cache=/cache/mount-123"

	testCases := []struct {
		name     string
		cache    cacheLimit
		args     []string
		wantArgs []string
	}{
		{
			name:     "equalSplit injects this node's share",
			cache:    mustCacheLimit(t, cacheLimitEqualSplit, 4*gib, 4),
			args:     []string{cacheArg},
			wantArgs: []string{"--foreground", "--max-cache-size=972"},
		},
		{
			name:     "equalSplit with no cache volume size injects 0, which serves the mount uncached",
			cache:    mustCacheLimit(t, cacheLimitEqualSplit, 0, 4),
			args:     []string{cacheArg},
			wantArgs: []string{"--foreground", "--max-cache-size=0"},
		},
		{
			name:     "none leaves the PV's size alone",
			cache:    cacheLimit{strategy: cacheLimitNone},
			args:     []string{cacheArg, "--max-cache-size=3000"},
			wantArgs: []string{"--foreground", "--max-cache-size=3000"},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			fr := &fakeProcessRunner{}
			pm, cacheDir := newProcessManagerWithCache(t, fr, testCase.cache)
			dev := mountertest.OpenDevNull(t)

			options := mountoptions.Options{
				Fd:         int(dev.Fd()),
				BucketName: "my-bucket",
				Args:       testCase.args,
				Uid:        mounter.UIDRangeStart,
				Gid:        mounter.UIDRangeStart,
			}
			assert.NoError(t, pm.Launch("mount-123", "/usr/bin/mount-s3", options))

			// cmd.Args is [binary, bucket, /dev/fd/3, ...sorted args].
			wantArgs := append([]string{"--cache=" + filepath.Join(cacheDir, "uid-65536")}, testCase.wantArgs...)
			assert.Equals(t, wantArgs, fr.handles[0].cmd.Args[3:])
			// The wire slice is the driver's mount-sharing key and is persisted to its meta file, so a
			// share leaking into it would make a running mount look incompatible with itself on restart.
			assert.Equals(t, testCase.args, options.Args)

			fr.handles[0].Exit(0, "")
			pm.Shutdown()
		})
	}
}

func TestProcessManager_Launch_CacheDir(t *testing.T) {
	const mountId = "mount-123"
	// The directory a mount as UID 65536 gets.
	const dirName = "uid-65536"

	cachedOptions := func(t *testing.T, args ...string) mountoptions.Options {
		dev := mountertest.OpenDevNull(t)
		return mountoptions.Options{
			Fd:         int(dev.Fd()),
			BucketName: "my-bucket",
			Args:       append([]string{"--cache=/cache/" + mountId}, args...),
			Uid:        mounter.UIDRangeStart,
			Gid:        mounter.UIDRangeStart}
	}

	t.Run("creates the mount's directory, owned by its UID and closed to every other, and points Mountpoint at it whatever --cache the request carried", func(t *testing.T) {
		fr := &fakeProcessRunner{}
		pm, cacheDir := newProcessManagerWithCache(t, fr, cacheLimit{strategy: cacheLimitNone})
		chowns := recordChowns(pm)
		// Repeated `cache` mount options on a PV reach the mounter; three in all, as Args.Set alone drops a second.
		options := cachedOptions(t, "--cache=/elsewhere", "--cache=/other")
		// Not the fixtures' 65536, so the name is shown to follow the launch's UID.
		options.Uid, options.Gid = 65537, 65537
		mountCacheDir := filepath.Join(cacheDir, "uid-65537")

		assert.NoError(t, pm.Launch(mountId, "/usr/bin/mount-s3", options))

		fi, err := os.Lstat(mountCacheDir)
		assert.NoError(t, err)
		assert.Equals(t, true, fi.IsDir())
		assert.Equals(t, fs.FileMode(0700), fi.Mode().Perm())
		assert.Equals(t, [2]int{65537, 65537}, chowns[mountCacheDir])
		assert.Equals(t, []string{"--cache=" + mountCacheDir, "--foreground"}, fr.handles[0].cmd.Args[3:])
		assert.Equals(t, []string{"--cache=/cache/" + mountId, "--cache=/elsewhere", "--cache=/other"}, options.Args)

		fr.handles[0].Exit(0, "")
		pm.Shutdown()
	})

	t.Run("removes what its UID left behind before an uncached launch too", func(t *testing.T) {
		fr := &fakeProcessRunner{}
		pm, cacheDir := newProcessManagerWithCache(t, fr, cacheLimit{strategy: cacheLimitNone})
		// What a killed Mountpoint of an earlier mount as this UID left behind, and another UID's.
		leftoverBlock := filepath.Join(cacheDir, dirName, "mountpoint-cache", "block")
		assert.NoError(t, os.MkdirAll(filepath.Dir(leftoverBlock), 0700))
		assert.NoError(t, os.WriteFile(leftoverBlock, []byte("x"), 0600))
		assert.NoError(t, os.Mkdir(filepath.Join(cacheDir, "uid-65537"), 0700))
		options := cachedOptions(t)
		options.Args = nil

		assert.NoError(t, pm.Launch(mountId, "/usr/bin/mount-s3", options))

		assertNotExist(t, filepath.Join(cacheDir, dirName))
		_, err := os.Stat(filepath.Join(cacheDir, "uid-65537"))
		assert.NoError(t, err)

		fr.handles[0].Exit(0, "")
		pm.Shutdown()
	})

	t.Run("fails without starting Mountpoint when what its UID left behind cannot be removed, and releases the mount", func(t *testing.T) {
		fr := &fakeProcessRunner{helperErr: errors.New("permission denied")}
		pm, cacheDir := newProcessManagerWithCache(t, fr, cacheLimit{strategy: cacheLimitNone})
		assert.NoError(t, os.MkdirAll(filepath.Join(cacheDir, dirName, "mountpoint-cache"), 0700))

		err := pm.Launch(mountId, "/usr/bin/mount-s3", cachedOptions(t))
		if err == nil {
			t.Fatal("expected Launch to refuse while a leftover of its UID remains")
		}
		assert.Contains(t, err.Error(), "failed to remove the leftover cache directory of UID 65536")
		// The error reaches this mount's pod events, so the removal's own error goes to the log only.
		assert.Equals(t, false, strings.Contains(err.Error(), "permission denied"))
		assert.Equals(t, 0, len(fr.handles))
		assertMountReleased(t, pm)
	})

	t.Run("fails without starting Mountpoint when the cache volume cannot be written to, and releases the mount", func(t *testing.T) {
		// Assert, not skip: CI is unprivileged, so a root run must fail loudly rather than lose this case.
		assert.Equals(t, false, os.Geteuid() == 0)
		fr := &fakeProcessRunner{}
		pm, cacheDir := newProcessManagerWithCache(t, fr, cacheLimit{strategy: cacheLimitNone})
		// A read-only cache volume: the kubelet has mounted it, but nothing can be created inside.
		assert.NoError(t, os.Chmod(cacheDir, 0500))
		t.Cleanup(func() { os.Chmod(cacheDir, 0700) })

		err := pm.Launch(mountId, "/usr/bin/mount-s3", cachedOptions(t))
		if err == nil {
			t.Fatal("expected Launch to fail on a read-only cache volume")
		}
		assert.Contains(t, err.Error(), "failed to create cache directory")
		assert.Equals(t, 0, len(fr.handles))
		assertMountReleased(t, pm)
	})

	t.Run("removes the directory when Mountpoint fails to start", func(t *testing.T) {
		fr := &fakeProcessRunner{startErr: errors.New("fork/exec: no such file")}
		pm, cacheDir := newProcessManagerWithCache(t, fr, cacheLimit{strategy: cacheLimitNone})

		if err := pm.Launch(mountId, "/usr/bin/mount-s3", cachedOptions(t)); err == nil {
			t.Fatal("expected Launch to fail when the process cannot start")
		}
		assertNotExist(t, filepath.Join(cacheDir, dirName))
	})

	t.Run("removes the directory and its contents once Mountpoint exits, before freeing the mount", func(t *testing.T) {
		fr := &fakeProcessRunner{}
		pm, cacheDir := newProcessManagerWithCache(t, fr, cacheLimit{strategy: cacheLimitNone})
		mountCacheDir := filepath.Join(cacheDir, dirName)
		assert.NoError(t, pm.Launch(mountId, "/usr/bin/mount-s3", cachedOptions(t)))
		// A killed Mountpoint leaves its blocks behind; a clean exit would have removed mountpoint-cache itself.
		blockDir := filepath.Join(mountCacheDir, "mountpoint-cache", "V2", "ab")
		assert.NoError(t, os.MkdirAll(blockDir, 0700))
		assert.NoError(t, os.WriteFile(filepath.Join(blockDir, "block"), []byte("x"), 0600))

		// While we hold the lock, the waiter cannot free the mount ID, so the directory must be removed before that.
		pm.mu.Lock()
		fr.handles[0].Exit(137, "")
		dirGone := false
		// 2s only bounds the failing case.
		for deadline := time.Now().Add(2 * time.Second); !dirGone && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			_, err := os.Lstat(mountCacheDir)
			dirGone = errors.Is(err, fs.ErrNotExist)
		}
		pm.mu.Unlock()
		assert.Equals(t, true, dirGone)

		pm.Shutdown()
	})
}

// assertMountReleased asserts that a failed Launch gave up the lock and tracks nothing, so a retry can proceed.
func assertMountReleased(t *testing.T, pm *ProcessManager) {
	t.Helper()
	if !pm.mu.TryLock() {
		t.Fatal("the failed launch kept the lock")
	}
	defer pm.mu.Unlock()
	assert.Equals(t, 0, len(pm.processes))
}

func TestProcessManager_Launch_MultipleProcesses(t *testing.T) {
	commDir := t.TempDir()
	fr := &fakeProcessRunner{}
	pm := NewProcessManager(commDir, "", fr, memoryLimit{strategy: memoryLimitNone}, cacheLimit{strategy: cacheLimitNone})

	for i, id := range []string{"mount-a", "mount-b", "mount-c"} {
		dev := mountertest.OpenDevNull(t)
		// Distinct UIDs, as the mounter refuses a UID a running mount holds.
		uid := uint32(65536 + i)
		err := pm.Launch(id, "/usr/bin/mount-s3", mountoptions.Options{
			Uid:        uid,
			Gid:        uid,
			Fd:         int(dev.Fd()),
			BucketName: fmt.Sprintf("bucket-%d", i),
		})
		assert.NoError(t, err)
	}

	// All 3 tracked
	pm.mu.Lock()
	assert.Equals(t, 3, len(pm.processes))
	pm.mu.Unlock()

	// Verify each got correct bucket and received exactly one FD
	assert.Equals(t, "bucket-0", fr.handles[0].cmd.Args[1])
	assert.Equals(t, "bucket-1", fr.handles[1].cmd.Args[1])
	assert.Equals(t, "bucket-2", fr.handles[2].cmd.Args[1])

	// One exits with error, one clean, one still running
	fr.handles[0].Exit(1, "oom killed")
	fr.handles[1].Exit(0, "")

	// Give goroutines time to process
	time.Sleep(50 * time.Millisecond)

	// Only mount-c still tracked
	pm.mu.Lock()
	assert.Equals(t, 1, len(pm.processes))
	assert.Equals(t, "mount-c", pm.processes[65538].mountId)
	pm.mu.Unlock()

	// Error file written for mount-a, not for mount-b
	errBytes, err := os.ReadFile(filepath.Join(commDir, "mount-a.error"))
	assert.NoError(t, err)
	assert.Equals(t, "oom killed", string(errBytes))

	_, err = os.ReadFile(filepath.Join(commDir, "mount-b.error"))
	assert.Equals(t, true, os.IsNotExist(err))

	// Shutdown signals remaining and waits
	fr.handles[2].Exit(0, "")
	pm.Shutdown()

	pm.mu.Lock()
	assert.Equals(t, 0, len(pm.processes))
	pm.mu.Unlock()
}

func TestProcessManager_Launch_DuplicateMountId_Rejected(t *testing.T) {
	commDir := t.TempDir()
	fr := &fakeProcessRunner{}
	pm := NewProcessManager(commDir, "", fr, memoryLimit{strategy: memoryLimitNone}, cacheLimit{strategy: cacheLimitNone})

	dev1 := mountertest.OpenDevNull(t)
	err := pm.Launch("same-mount", "/usr/bin/mount-s3", mountoptions.Options{
		Uid:        65536,
		Gid:        65536,
		Fd:         int(dev1.Fd()),
		BucketName: "bucket",
	})
	assert.NoError(t, err)

	// Second launch with same mountId should fail, even with another UID, as the node's retry for a PV gets one.
	dev2 := mountertest.OpenDevNull(t)
	err = pm.Launch("same-mount", "/usr/bin/mount-s3", mountoptions.Options{
		Uid:        65537,
		Gid:        65537,
		Fd:         int(dev2.Fd()),
		BucketName: "bucket",
	})
	if err == nil {
		t.Fatal("Expected error for duplicate mountId, got nil")
	}

	// Only one process tracked
	pm.mu.Lock()
	assert.Equals(t, 1, len(pm.processes))
	pm.mu.Unlock()

	// After first exits, same mountId can be reused
	fr.handles[0].Exit(0, "")
	time.Sleep(10 * time.Millisecond)

	dev3 := mountertest.OpenDevNull(t)
	err = pm.Launch("same-mount", "/usr/bin/mount-s3", mountoptions.Options{
		Uid:        65536,
		Gid:        65536,
		Fd:         int(dev3.Fd()),
		BucketName: "bucket",
	})
	assert.NoError(t, err)

	fr.handles[1].Exit(0, "")
	pm.Shutdown()
}

func TestProcessManager_Launch_DuplicateUID_Rejected(t *testing.T) {
	fr := &fakeProcessRunner{}
	pm, cacheDir := newProcessManagerWithCache(t, fr, cacheLimit{strategy: cacheLimitNone})

	dev1 := mountertest.OpenDevNull(t)
	err := pm.Launch("mount-a", "/usr/bin/mount-s3", mountoptions.Options{
		Uid:        65536,
		Gid:        65536,
		Fd:         int(dev1.Fd()),
		BucketName: "bucket",
		Args:       []string{"--cache=/cache/mount-a"},
	})
	assert.NoError(t, err)
	cachedBlock := filepath.Join(cacheDir, "uid-65536", "block")
	assert.NoError(t, os.WriteFile(cachedBlock, []byte("x"), 0600))

	// A different mountId carrying the same UID must be rejected: two Mountpoints under one UID
	// would defeat the per-mount kernel isolation.
	dev2 := mountertest.OpenDevNull(t)
	err = pm.Launch("mount-b", "/usr/bin/mount-s3", mountoptions.Options{
		Uid:        65536,
		Gid:        65536,
		Fd:         int(dev2.Fd()),
		BucketName: "bucket",
	})
	if err == nil {
		t.Fatal("Expected error for duplicate UID, got nil")
	}
	// The error reaches mount-b's pod events, so it must not name the other mount.
	assert.Equals(t, false, strings.Contains(err.Error(), "mount-a"))
	// The UID check must come before the removal of that UID's directory.
	_, err = os.Stat(cachedBlock)
	assert.NoError(t, err)

	// Only the first process is tracked.
	pm.mu.Lock()
	assert.Equals(t, 1, len(pm.processes))
	assert.Equals(t, "mount-a", pm.processes[65536].mountId)
	pm.mu.Unlock()

	fr.handles[0].Exit(0, "")
	pm.Shutdown()
}

func TestProcessManager_RemoveCacheVolumeEntry(t *testing.T) {
	t.Run("empties a directory as its owner, with no groups, then removes it", func(t *testing.T) {
		fr := &fakeProcessRunner{}
		pm, cacheDir := newProcessManagerWithCache(t, fr, cacheLimit{strategy: cacheLimitNone})
		logs := captureKlog(t)
		// A leftover a killed Mountpoint would leave: not empty, so root alone cannot remove it.
		owner := uint32(os.Getuid())
		// Named for another UID, so the name always disagrees with the owner.
		dir := filepath.Join(cacheDir, mountCacheDirName(owner+1))
		assert.NoError(t, os.MkdirAll(filepath.Join(dir, "mountpoint-cache"), 0700))
		assert.NoError(t, os.WriteFile(filepath.Join(dir, "mountpoint-cache", "block"), []byte("x"), 0600))

		assert.NoError(t, pm.removeCacheVolumeEntry(filepath.Base(dir)))

		assertNotExist(t, dir)
		// The fake runner records the helper's command and empties the directory in-process instead of starting it.
		assert.Equals(t, 1, len(fr.helperCmds)) // helper started only once
		helper := fr.helperCmds[0]
		// i.e. `aws-s3-csi-daemonset-mounter empty-cache-dir <dir>`, with no environment.
		assert.Equals(t, []string{emptyDirArg, dir}, helper.Args[1:])
		assert.Equals(t, []string{}, helper.Env)
		// The test user created the directory, so it owns it; the helper must run as the owner, not as root.
		assert.Equals(t, &syscall.Credential{Uid: owner, Gid: owner, Groups: []uint32{}}, helper.SysProcAttr.Credential)
		assert.Contains(t, logs.String(), fmt.Sprintf("Emptying cache directory %s as UID %d", dir, owner))
		// Since owner is now different from uid-<uid> dir name (mountCacheDirName(owner+1)) - we check for this log line too
		assert.Contains(t, logs.String(), fmt.Sprintf("%q is owned by UID %d, not the UID its name gives; removing it anyway", dir, owner))

		// A directory named for its owner is not a mismatch.
		assert.NoError(t, os.MkdirAll(filepath.Join(cacheDir, mountCacheDirName(owner), "mountpoint-cache"), 0700))
		logs.Reset()
		assert.NoError(t, pm.removeCacheVolumeEntry(mountCacheDirName(owner)))
		assert.Equals(t, false, strings.Contains(logs.String(), "not the UID its name gives"))
	})

	t.Run("never runs the removal helper as root", func(t *testing.T) {
		fr := &fakeProcessRunner{}
		pm, cacheDir := newProcessManagerWithCache(t, fr, cacheLimit{strategy: cacheLimitNone})

		if err := pm.spawnRemovalHelper(cacheDir, 0); err == nil {
			t.Fatal("expected spawnRemovalHelper to refuse UID 0")
		}
		assert.Equals(t, 0, len(fr.helperCmds))
	})

	t.Run("fails when the mounter has no cache volume", func(t *testing.T) {
		pm := NewProcessManager(t.TempDir(), "", &fakeProcessRunner{}, memoryLimit{strategy: memoryLimitNone}, cacheLimit{strategy: cacheLimitNone})
		if err := pm.removeCacheVolumeEntry("uid-65536"); err == nil {
			t.Fatal("expected removeCacheVolumeEntry to refuse an empty cache volume")
		}
	})

	t.Run("refuses a name that is not a plain entry of the cache volume", func(t *testing.T) {
		testCases := []struct {
			name  string
			entry string
		}{
			{name: "an unnamed entry", entry: ""},
			{name: "the volume root itself", entry: "."},
			{name: "the parent of the volume root", entry: ".."},
			{name: "a path escaping the volume root", entry: "../sibling"},
			{name: "a subdirectory of a mount's cache", entry: "uid-65536/mountpoint-cache/V2"},
		}

		for _, testCase := range testCases {
			t.Run(testCase.name, func(t *testing.T) {
				pm, cacheDir := newProcessManagerWithCache(t, &fakeProcessRunner{}, cacheLimit{strategy: cacheLimitNone})
				sibling := filepath.Join(filepath.Dir(cacheDir), "sibling")
				assert.NoError(t, os.Mkdir(sibling, 0770))
				cachedBlocks := filepath.Join(cacheDir, "uid-65536", "mountpoint-cache", "V2")
				assert.NoError(t, os.MkdirAll(cachedBlocks, 0770))

				if err := pm.removeCacheVolumeEntry(testCase.entry); err == nil {
					t.Fatal("expected removeCacheVolumeEntry to refuse a name that is not a plain directory name")
				}

				// Check that it removed nothing after returning error, and that
				// the cache volume root and its sibling are still there.
				_, err := os.Stat(cacheDir)
				assert.NoError(t, err)
				_, err = os.Stat(filepath.Dir(cacheDir))
				assert.NoError(t, err)
				_, err = os.Stat(sibling)
				assert.NoError(t, err)
				_, err = os.Stat(cachedBlocks)
				assert.NoError(t, err)
			})
		}
	})
}

func TestRemovalHelperEmptyDir(t *testing.T) {
	t.Run("removes everything inside, including what its owner made unreadable, but not the directory", func(t *testing.T) {
		dir := t.TempDir()
		for _, sub := range []string{"read-only", "no-access"} {
			assert.NoError(t, os.MkdirAll(filepath.Join(dir, "mountpoint-cache", sub), 0700))
			assert.NoError(t, os.WriteFile(filepath.Join(dir, "mountpoint-cache", sub, "block"), []byte("x"), 0600))
		}
		assert.NoError(t, os.Chmod(filepath.Join(dir, "mountpoint-cache", "read-only"), 0500))
		assert.NoError(t, os.Chmod(filepath.Join(dir, "mountpoint-cache", "no-access"), 0000))

		assert.NoError(t, removalHelperEmptyDir(dir))

		entries, err := os.ReadDir(dir)
		assert.NoError(t, err)
		assert.Equals(t, 0, len(entries))
	})

	t.Run("removes a planted symlink without following it", func(t *testing.T) {
		dir := t.TempDir()
		outside := t.TempDir()
		assert.NoError(t, os.WriteFile(filepath.Join(outside, "keep"), []byte("x"), 0600))
		assert.NoError(t, os.Chmod(outside, 0500))
		t.Cleanup(func() { os.Chmod(outside, 0700) })
		assert.NoError(t, os.Symlink(outside, filepath.Join(dir, "link")))
		// Listed before "link" and unreadable, so the first removal fails and the chmod walk meets the link.
		assert.NoError(t, os.MkdirAll(filepath.Join(dir, "a-locked", "sub"), 0700))
		assert.NoError(t, os.Chmod(filepath.Join(dir, "a-locked"), 0))

		assert.NoError(t, removalHelperEmptyDir(dir))

		assertNotExist(t, filepath.Join(dir, "link"))
		fi, err := os.Stat(outside)
		assert.NoError(t, err)
		assert.Equals(t, fs.FileMode(0500), fi.Mode().Perm())
		_, err = os.Stat(filepath.Join(outside, "keep"))
		assert.NoError(t, err)
	})
}

func TestProcessManager_ChownWithDefault(t *testing.T) {
	t.Run("chowns a directory, but not through a symlink planted at its path", func(t *testing.T) {
		pm := NewProcessManager(t.TempDir(), t.TempDir(), &fakeProcessRunner{}, memoryLimit{strategy: memoryLimitNone}, cacheLimit{strategy: cacheLimitNone})
		dir := t.TempDir()
		link := filepath.Join(t.TempDir(), "link")
		assert.NoError(t, os.Symlink(dir, link))

		// Chowning to the test user's own IDs needs no privilege, so we can run the real chown not the test override one.
		assert.NoError(t, pm.chownWithDefault(dir, os.Getuid(), os.Getgid()))
		if err := pm.chownWithDefault(link, os.Getuid(), os.Getgid()); err == nil {
			t.Fatal("expected chownWithDefault to refuse a symlink")
		}
	})
}

func TestProcessManager_Shutdown_SendsSIGTERM(t *testing.T) {
	commDir := t.TempDir()
	fr := &fakeProcessRunner{}
	pm := NewProcessManager(commDir, "", fr, memoryLimit{strategy: memoryLimitNone}, cacheLimit{strategy: cacheLimitNone})

	dev := mountertest.OpenDevNull(t)
	err := pm.Launch("m1", "/usr/bin/mount-s3", mountoptions.Options{
		Uid:        65536,
		Gid:        65536,
		Fd:         int(dev.Fd()),
		BucketName: "b",
	})
	assert.NoError(t, err)

	go func() {
		select {
		case sig := <-fr.handles[0].sigCh:
			assert.Equals(t, os.Signal(syscall.SIGTERM), sig)
			fr.handles[0].Exit(0, "")
		case <-time.After(5 * time.Second):
			t.Error("Timed out waiting for SIGTERM")
			fr.handles[0].Exit(1, "")
		}
	}()

	pm.Shutdown()
}

// TestHandleConnection_NoFdLeak verifies that handleConnection does not leak file descriptors
// across multiple iterations, including the error path (empty VolumeId).
// NOTE: Do not use t.Parallel() here — fd counting via /proc/self/fd is process-global
// and would be unreliable if other tests open/close fds concurrently.
func TestHandleConnection_NoFdLeak(t *testing.T) {
	commDir := t.TempDir()
	fr := &fakeProcessRunner{}
	pm := NewProcessManager(commDir, "", fr, memoryLimit{strategy: memoryLimitNone}, cacheLimit{strategy: cacheLimitNone})

	sockPath := filepath.Join(commDir, "test.sock")
	listener, err := net.Listen("unix", sockPath)
	assert.NoError(t, err)
	defer listener.Close()

	fdsBefore := countOpenFds(t)

	const iterations = 5
	for i := range iterations {
		dev := mountertest.OpenDevNull(t)
		sendDone := make(chan struct{})
		volumeId := fmt.Sprintf("vol-%d", i)
		if i == iterations-1 {
			volumeId = "" // no VolumeId — handleConnection should close fd without launching
		}
		uid := uint32(65536 + i)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			mountoptions.Send(ctx, sockPath, mountoptions.Options{
				Uid:        uid,
				Gid:        uid,
				Fd:         int(dev.Fd()),
				BucketName: "bucket",
				VolumeId:   volumeId,
			})
			dev.Close()
			close(sendDone)
		}()

		conn, err := listener.Accept()
		assert.NoError(t, err)
		handleConnection(conn.(*net.UnixConn), "/opt/mount-s3", pm, 5*time.Second)
		<-sendDone
		if i < iterations-1 {
			fr.handles[i].Exit(0, "")
		}
	}

	time.Sleep(50 * time.Millisecond)
	pm.Shutdown()

	fdsAfter := countOpenFds(t)
	if fdsAfter > fdsBefore {
		t.Errorf("fd leak: %d before, %d after", fdsBefore, fdsAfter)
	}
}

func countOpenFds(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	assert.NoError(t, err)
	return len(entries)
}

func TestHandleConnection_MountIdValidation(t *testing.T) {
	validIds := []string{
		"abcdef12-3456-7890-abcd-ef1234567890-vol456",
		"pvc-abcdef12-3456-7890-abcd-ef1234567890",
		"s3-pv",
	}
	invalidIds := []string{
		"../../etc/passwd",
		"../parent",
		"slash/inside",
		"/absolute",
		"-starts-with-dash",
		".starts-with-dot",
		"has spaces",
		"has\nnewline",
	}

	commDir := t.TempDir()
	fr := &fakeProcessRunner{}
	pm := NewProcessManager(commDir, "", fr, memoryLimit{strategy: memoryLimitNone}, cacheLimit{strategy: cacheLimitNone})

	sockPath := filepath.Join(commDir, "test.sock")
	listener, err := net.Listen("unix", sockPath)
	assert.NoError(t, err)
	defer listener.Close()

	dev := mountertest.OpenDevNull(t)
	defer dev.Close()

	sendMount := func(id string, uid uint32) {
		sendDone := make(chan struct{})
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			mountoptions.Send(ctx, sockPath, mountoptions.Options{
				Uid:        uid,
				Gid:        uid,
				Fd:         int(dev.Fd()),
				BucketName: "bucket",
				VolumeId:   id,
			})
			close(sendDone)
		}()

		conn, err := listener.Accept()
		assert.NoError(t, err)
		handleConnection(conn.(*net.UnixConn), "/opt/mount-s3", pm, 5*time.Second)
		<-sendDone
	}

	for _, id := range invalidIds {
		sendMount(id, 65536)
	}

	fr.mu.Lock()
	assert.Equals(t, 0, len(fr.handles))
	fr.mu.Unlock()

	for i, id := range validIds {
		sendMount(id, uint32(65536+i))
	}

	fr.mu.Lock()
	assert.Equals(t, len(validIds), len(fr.handles))
	fr.mu.Unlock()

	for _, h := range fr.handles {
		h.Exit(0, "")
	}
	pm.Shutdown()
}

func TestHandleConnection_RefusedLaunchWritesErrorFile(t *testing.T) {
	commDir := t.TempDir()
	fr := &fakeProcessRunner{}
	pm := NewProcessManager(commDir, "", fr, memoryLimit{strategy: memoryLimitNone}, cacheLimit{strategy: cacheLimitNone})

	sockPath := filepath.Join(commDir, "test.sock")
	listener, err := net.Listen("unix", sockPath)
	assert.NoError(t, err)
	defer listener.Close()

	dev := mountertest.OpenDevNull(t)
	// Waited on below, so the sender is done with dev before its cleanup closes it.
	sendDone := make(chan struct{})
	go func() {
		defer close(sendDone)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		mountoptions.Send(ctx, sockPath, mountoptions.Options{
			Fd:         int(dev.Fd()),
			BucketName: "bucket",
			VolumeId:   "s3-pv",
			Args:       []string{"--cache=/cache/s3-pv"},
			Uid:        mounter.UIDRangeStart,
			Gid:        mounter.UIDRangeStart,
		})
	}()

	conn, err := listener.Accept()
	assert.NoError(t, err)
	handleConnection(conn.(*net.UnixConn), "/opt/mount-s3", pm, 5*time.Second)
	<-sendDone

	errBytes, err := os.ReadFile(filepath.Join(commDir, "s3-pv.error"))
	assert.NoError(t, err)
	assert.Contains(t, string(errBytes), "has no cache volume")
	assert.Equals(t, 0, len(fr.handles))
}

func TestProcessManager_Launch_ErrorExit_WritesErrorFile(t *testing.T) {
	commDir := t.TempDir()
	fr := &fakeProcessRunner{}
	pm := NewProcessManager(commDir, "", fr, memoryLimit{strategy: memoryLimitNone}, cacheLimit{strategy: cacheLimitNone})

	dev := mountertest.OpenDevNull(t)
	err := pm.Launch("mount-abc", "/usr/bin/mount-s3", mountoptions.Options{
		Uid:        65536,
		Gid:        65536,
		Fd:         int(dev.Fd()),
		BucketName: "bucket",
	})
	assert.NoError(t, err)

	fr.handles[0].Exit(1, "credential error")
	pm.Shutdown()

	errBytes, err := os.ReadFile(filepath.Join(commDir, "mount-abc.error"))
	assert.NoError(t, err)
	assert.Equals(t, "credential error", string(errBytes))
}

func TestProcessManager_Launch_RemovesAnEarlierErrorFile(t *testing.T) {
	commDir := t.TempDir()
	fr := &fakeProcessRunner{}
	pm := NewProcessManager(commDir, "", fr, memoryLimit{strategy: memoryLimitNone}, cacheLimit{strategy: cacheLimitNone})
	errFile := filepath.Join(commDir, "mount-abc.error")
	assert.NoError(t, os.WriteFile(errFile, []byte("credential error"), 0600))

	dev := mountertest.OpenDevNull(t)
	assert.NoError(t, pm.Launch("mount-abc", "/usr/bin/mount-s3",
		mountoptions.Options{Fd: int(dev.Fd()), BucketName: "bucket", Uid: 65536, Gid: 65536}))
	assertNotExist(t, errFile)

	fr.handles[0].Exit(0, "")
	pm.Shutdown()
}

func TestProcessManager_Launch_RejectsCredentialsOutsideTheAllocatorRange(t *testing.T) {
	for _, tc := range []struct {
		name     string
		uid, gid uint32
	}{
		{"zero, the unset value", 0, 0},
		{"below the range", mounter.UIDRangeStart - 1, mounter.UIDRangeStart - 1},
		{"above the range", mounter.UIDRangeEnd + 1, mounter.UIDRangeEnd + 1},
		{"GID not matching UID", mounter.UIDRangeStart, mounter.UIDRangeStart + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fr := &fakeProcessRunner{}
			pm := NewProcessManager(t.TempDir(), "", fr, memoryLimit{strategy: memoryLimitNone}, cacheLimit{strategy: cacheLimitNone})
			dev := mountertest.OpenDevNull(t)

			err := pm.Launch("vol-bad-creds", "/usr/bin/mount-s3", mountoptions.Options{
				Uid:        tc.uid,
				Gid:        tc.gid,
				Fd:         int(dev.Fd()),
				BucketName: "bucket",
			})
			if err == nil {
				t.Fatalf("expected Launch to refuse uid %d gid %d", tc.uid, tc.gid)
			}

			fr.mu.Lock()
			defer fr.mu.Unlock()
			assert.Equals(t, 0, len(fr.handles))
		})
	}
}
