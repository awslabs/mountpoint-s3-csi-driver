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
	mu       sync.Mutex
	nextPid  int
	handles  []*fakeProcessHandle
	startErr error // when set, Start fails instead of spawning
}

func (r *fakeProcessRunner) Start(cmd *exec.Cmd) (ProcessHandle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
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

// createUnremovableCacheEntry creates an entry in cacheDir that the test user cannot remove, so its removal fails.
func createUnremovableCacheEntry(t *testing.T, cacheDir, entryName string) {
	t.Helper()
	// Assert, not skip: CI is unprivileged, so a root run must fail loudly rather than lose this case.
	assert.Equals(t, false, os.Geteuid() == 0)
	// Without CAP_DAC_OVERRIDE, which the test user lacks and the mounter holds, nothing can be unlinked from a read-only directory.
	lockedDir := filepath.Join(cacheDir, entryName, "mountpoint-cache")
	assert.NoError(t, os.MkdirAll(lockedDir, 0700))
	assert.NoError(t, os.WriteFile(filepath.Join(lockedDir, "block"), []byte("x"), 0600))
	assert.NoError(t, os.Chmod(lockedDir, 0500))
	t.Cleanup(func() { os.Chmod(lockedDir, 0700) })
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
		assert.Equals(t, map[string][2]int{cacheDir: {0, 0}}, chowns)
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
		pm, cacheDir := newProcessManagerWithCache(t, &fakeProcessRunner{}, cacheLimit{strategy: cacheLimitNone})
		createUnremovableCacheEntry(t, cacheDir, "pv-stuck")
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
				Uid:        mountoptions.UIDRangeStart,
				Gid:        mountoptions.UIDRangeStart,
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
		Uid:        mountoptions.UIDRangeStart,
		Gid:        mountoptions.UIDRangeStart,
	})
	if err == nil {
		t.Fatal("expected Launch to fail when the process cannot start")
	}

	assertMountReleased(t, pm, int(dev.Fd()), 0)
}

func TestProcessManager_Launch_MaxCacheSize(t *testing.T) {
	// What the driver sends; the mounter replaces it with its own directory for the mount.
	const cacheArg = "--cache"

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
				Uid:        mountoptions.UIDRangeStart,
				Gid:        mountoptions.UIDRangeStart,
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
			Args:       append([]string{"--cache"}, args...),
			Uid:        mountoptions.UIDRangeStart,
			Gid:        mountoptions.UIDRangeStart}
	}

	t.Run("creates the PV's directory, owned by its UID and closed to every other, and points Mountpoint at it whatever --cache the request carried", func(t *testing.T) {
		fr := &fakeProcessRunner{}
		pm, cacheDir := newProcessManagerWithCache(t, fr, cacheLimit{strategy: cacheLimitNone})
		chowns := recordChowns(pm)
		// A request may repeat --cache; three in all, as Args.Set alone drops a second.
		options := cachedOptions(t, "--cache=/elsewhere", "--cache=/other")
		// Not the fixtures' 65536, so the name is shown to follow the launch's UID.
		options.Uid, options.Gid = 65537, 65537
		perPvCacheDir := filepath.Join(cacheDir, "uid-65537")

		assert.NoError(t, pm.Launch(mountId, "/usr/bin/mount-s3", options))

		fi, err := os.Lstat(perPvCacheDir)
		assert.NoError(t, err)
		assert.Equals(t, true, fi.IsDir())
		assert.Equals(t, fs.FileMode(0700), fi.Mode().Perm())
		assert.Equals(t, [2]int{65537, 65537}, chowns[perPvCacheDir])
		assert.Equals(t, []string{"--cache=" + perPvCacheDir, "--foreground"}, fr.handles[0].cmd.Args[3:])
		assert.Equals(t, []string{"--cache", "--cache=/elsewhere", "--cache=/other"}, options.Args)

		fr.handles[0].Exit(0, "")
		pm.Shutdown()
	})

	t.Run("a UID with a leftover cache is refused, cached or not, until the cleaner removes it", func(t *testing.T) {
		for _, cached := range []bool{true, false} {
			t.Run(fmt.Sprintf("cached=%t", cached), func(t *testing.T) {
				fr := &fakeProcessRunner{}
				pm, cacheDir := newProcessManagerWithCache(t, fr, cacheLimit{strategy: cacheLimitNone})
				// What a killed Mountpoint of an earlier mounter pod left as this UID, and as another, marked as at startup.
				leftoverBlock := filepath.Join(cacheDir, dirName, "mountpoint-cache", "block")
				assert.NoError(t, os.MkdirAll(filepath.Dir(leftoverBlock), 0700))
				assert.NoError(t, os.WriteFile(leftoverBlock, []byte("x"), 0600))
				assert.NoError(t, os.Mkdir(filepath.Join(cacheDir, "uid-65537"), 0700))
				assert.NoError(t, pm.markLeftoverCacheDirs())
				options := cachedOptions(t)
				if !cached {
					options.Args = nil
				}

				err := pm.Launch(mountId, "/usr/bin/mount-s3", options)
				if err == nil {
					t.Fatal("expected Launch to refuse while a leftover of its UID remains")
				}
				assert.Contains(t, err.Error(), "refusing to launch mount mount-123: the previous cache of UID 65536 is still being removed")
				assert.Equals(t, 0, len(fr.handles))
				// Left for the cleaner: Launch did not remove it while holding the lock, which would block every other mount.
				_, err = os.Stat(leftoverBlock)
				assert.NoError(t, err)
				// Both UIDs stay taken; neither leftover is a mount's, so no mount ID is held.
				assertMountReleased(t, pm, options.Fd, 2)

				pm.cleanPendingCacheDirs()

				assertNotExist(t, filepath.Join(cacheDir, dirName))
				assertNotExist(t, filepath.Join(cacheDir, "uid-65537"))
				assertMountReleased(t, pm, -1, 0)
				assert.NoError(t, pm.Launch(mountId, "/usr/bin/mount-s3", cachedOptions(t)))
				fr.handles[0].Exit(0, "")
				pm.Shutdown()
			})
		}
	})

	t.Run("keeps refusing the UID while what it left behind cannot be removed", func(t *testing.T) {
		fr := &fakeProcessRunner{}
		pm, cacheDir := newProcessManagerWithCache(t, fr, cacheLimit{strategy: cacheLimitNone})
		createUnremovableCacheEntry(t, cacheDir, dirName)
		assert.NoError(t, pm.markLeftoverCacheDirs())

		pm.cleanPendingCacheDirs()

		assertCleanupPending(t, pm, mountoptions.UIDRangeStart, "")
		err := pm.Launch(mountId, "/usr/bin/mount-s3", cachedOptions(t))
		if err == nil {
			t.Fatal("expected Launch to refuse the UID while its cache is still being removed")
		}
		assert.Contains(t, err.Error(), "the previous cache of UID 65536 is still being removed")
		// The error reaches this mount's pod events, so the removal's own error goes to the log only.
		assert.Equals(t, false, strings.Contains(err.Error(), "permission denied"))
		assert.Equals(t, 0, len(fr.handles))
		pm.Shutdown()
	})

	t.Run("fails without starting Mountpoint when the cache volume cannot be written to, and releases the mount", func(t *testing.T) {
		// Assert, not skip: CI is unprivileged, so a root run must fail loudly rather than lose this case.
		assert.Equals(t, false, os.Geteuid() == 0)
		fr := &fakeProcessRunner{}
		pm, cacheDir := newProcessManagerWithCache(t, fr, cacheLimit{strategy: cacheLimitNone})
		// A read-only cache volume: the kubelet has mounted it, but nothing can be created inside.
		assert.NoError(t, os.Chmod(cacheDir, 0500))
		t.Cleanup(func() { os.Chmod(cacheDir, 0700) })
		options := cachedOptions(t)

		err := pm.Launch(mountId, "/usr/bin/mount-s3", options)
		if err == nil {
			t.Fatal("expected Launch to fail on a read-only cache volume")
		}
		assert.Contains(t, err.Error(), "failed to create cache directory")
		assert.Equals(t, 0, len(fr.handles))
		assertMountReleased(t, pm, options.Fd, 0)
	})

	t.Run("removes the directory when Mountpoint fails to start", func(t *testing.T) {
		fr := &fakeProcessRunner{startErr: errors.New("fork/exec: no such file")}
		pm, cacheDir := newProcessManagerWithCache(t, fr, cacheLimit{strategy: cacheLimitNone})

		if err := pm.Launch(mountId, "/usr/bin/mount-s3", cachedOptions(t)); err == nil {
			t.Fatal("expected Launch to fail when the process cannot start")
		}
		assertNotExist(t, filepath.Join(cacheDir, dirName))
		assertMountReleased(t, pm, -1, 0)
	})

	t.Run("marks the UID, but frees the mount, when the directory of a Mountpoint that failed to start cannot be removed", func(t *testing.T) {
		fr := &fakeProcessRunner{startErr: errors.New("fork/exec: no such file")}
		pm, cacheDir := newProcessManagerWithCache(t, fr, cacheLimit{strategy: cacheLimitNone})
		// Make the freshly created directory unremovable, as the chown is the last step of creating it.
		pm.chownForTesting = func(path string, _, _ int) error {
			createUnremovableCacheEntry(t, cacheDir, filepath.Base(path))
			return nil
		}
		options := cachedOptions(t)

		if err := pm.Launch(mountId, "/usr/bin/mount-s3", options); err == nil {
			t.Fatal("expected Launch to fail when the process cannot start")
		}
		// The UID and its slot are held, the mount ID is not.
		assertMountReleased(t, pm, options.Fd, 1)
		assertCleanupPending(t, pm, mountoptions.UIDRangeStart, "")

		// Another PV on that UID is refused; the same PV's retry on another UID goes ahead.
		err := pm.Launch("mount-456", "/usr/bin/mount-s3", cachedOptions(t))
		if err == nil {
			t.Fatal("expected Launch to refuse the UID while its leftover remains")
		}
		assert.Contains(t, err.Error(), "the previous cache of UID 65536 is still being removed")
		fr.startErr = nil
		pm.chownForTesting = func(string, int, int) error { return nil }
		retry := cachedOptions(t)
		retry.Uid, retry.Gid = 65537, 65537
		assert.NoError(t, pm.Launch(mountId, "/usr/bin/mount-s3", retry))

		fr.handles[0].Exit(0, "")
		pm.Shutdown()
	})

	t.Run("removes the directory and its contents once Mountpoint exits, before freeing the mount", func(t *testing.T) {
		fr := &fakeProcessRunner{}
		pm, cacheDir := newProcessManagerWithCache(t, fr, cacheLimit{strategy: cacheLimitNone})
		perPvCacheDir := filepath.Join(cacheDir, dirName)
		assert.NoError(t, pm.Launch(mountId, "/usr/bin/mount-s3", cachedOptions(t)))
		// A killed Mountpoint leaves its blocks behind; a clean exit would have removed mountpoint-cache itself.
		blockDir := filepath.Join(perPvCacheDir, "mountpoint-cache", "V2", "ab")
		assert.NoError(t, os.MkdirAll(blockDir, 0700))
		assert.NoError(t, os.WriteFile(filepath.Join(blockDir, "block"), []byte("x"), 0600))

		// While we hold the lock, the waiter cannot free the mount ID, so the directory must be removed before that.
		pm.mu.Lock()
		fr.handles[0].Exit(137, "")
		dirGone := false
		// 2s only bounds the failing case.
		for deadline := time.Now().Add(2 * time.Second); !dirGone && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			_, err := os.Lstat(perPvCacheDir)
			dirGone = errors.Is(err, fs.ErrNotExist)
		}
		pm.mu.Unlock()
		assert.Equals(t, true, dirGone)

		pm.Shutdown()
	})
}

// assertMountReleased asserts that a failed Launch gave up the lock and the FUSE device fd (skipped when fd < 0), and tracks
// only `taken` UIDs, running or with a cache still to remove, so a retry can proceed. Every mount ID it tracks belongs to one.
func assertMountReleased(t *testing.T, pm *ProcessManager, fd int, taken int) {
	t.Helper()
	if !pm.mu.TryLock() {
		t.Fatal("the failed launch kept the lock")
	}
	defer pm.mu.Unlock()
	assert.Equals(t, taken, len(pm.processes))
	withMount := 0
	for uid, p := range pm.processes {
		if p.mountId != "" {
			withMount++
			assert.Equals(t, uid, pm.uids[p.mountId])
		}
	}
	assert.Equals(t, withMount, len(pm.uids))
	if fd >= 0 {
		assert.ErrorIs(t, syscall.Fstat(fd, &syscall.Stat_t{}), syscall.EBADF)
	}
}

// assertCleanupPending asserts uid carries the deletion marker, holding mountId ("" for none).
func assertCleanupPending(t *testing.T, pm *ProcessManager, uid uint32, mountId string) {
	t.Helper()
	pm.mu.Lock()
	defer pm.mu.Unlock()
	p, ok := pm.processes[uid]
	assert.Equals(t, true, ok)
	assert.Equals(t, true, p.cleanupPending)
	assert.Equals(t, nil, p.handle)
	assert.Equals(t, mountId, p.mountId)
	if mountId != "" {
		assert.Equals(t, uid, pm.uids[mountId])
	}
}

// waitFor polls cond until it holds; 2s only bounds the failing case.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func TestProcessManager_CacheCleanupFailure(t *testing.T) {
	const mountId = "mount-123"
	const uid = mountoptions.UIDRangeStart
	const dirName = "uid-65536"
	cachedOptions := func(t *testing.T, uid uint32) mountoptions.Options {
		return mountoptions.Options{Fd: int(mountertest.OpenDevNull(t).Fd()), BucketName: "my-bucket", Args: []string{"--cache"}, Uid: uid, Gid: uid}
	}
	// exitWithUnremovableCache launches mountId as uid, leaves a cache it cannot remove, and returns once it has exited.
	exitWithUnremovableCache := func(t *testing.T, pm *ProcessManager, fr *fakeProcessRunner, cacheDir string) {
		t.Helper()
		assert.NoError(t, pm.Launch(mountId, "/usr/bin/mount-s3", cachedOptions(t, uid)))
		createUnremovableCacheEntry(t, cacheDir, dirName)
		fr.handles[len(fr.handles)-1].Exit(137, "killed")
		waitFor(t, "the deletion marker", func() bool {
			pm.mu.Lock()
			defer pm.mu.Unlock()
			return pm.processes[uid].cleanupPending
		})
	}

	t.Run("releases nothing: the mount ID, the UID and the slot stay taken while its cache cannot be removed", func(t *testing.T) {
		fr := &fakeProcessRunner{}
		pm, cacheDir := newProcessManagerWithCache(t, fr, cacheLimit{strategy: cacheLimitNone})

		exitWithUnremovableCache(t, pm, fr, cacheDir)

		assertCleanupPending(t, pm, uid, mountId)
		// The error file is still written, so csi-node sees the failure at once.
		_, err := os.Stat(filepath.Join(pm.commDir, mountId+errorFileExt))
		assert.NoError(t, err)
		// The same PV, even on another UID, is refused until the cache is gone.
		err = pm.Launch(mountId, "/usr/bin/mount-s3", cachedOptions(t, uid+1))
		if err == nil {
			t.Fatal("expected the mount ID to stay taken")
		}
		assert.Contains(t, err.Error(), "still has a Mountpoint process, which may be exiting or removing its cache")
		// Another PV on that UID is refused, without naming the mount.
		err = pm.Launch("mount-456", "/usr/bin/mount-s3", cachedOptions(t, uid))
		if err == nil {
			t.Fatal("expected the UID to stay taken")
		}
		assert.Contains(t, err.Error(), "the previous cache of UID 65536 is still being removed")
		assert.Equals(t, false, strings.Contains(err.Error(), mountId))
		assert.Equals(t, 1, len(fr.handles))
		pm.Shutdown()
	})

	t.Run("the cache cleaner retries in the background and frees the mount ID and the UID once the cache is gone", func(t *testing.T) {
		fr := &fakeProcessRunner{}
		pm, cacheDir := newProcessManagerWithCache(t, fr, cacheLimit{strategy: cacheLimitNone})
		exitWithUnremovableCache(t, pm, fr, cacheDir)
		stop := make(chan struct{})
		pm.StartCacheCleaner(stop, 10*time.Millisecond)

		// Still unremovable: a few retries leave it taken.
		time.Sleep(50 * time.Millisecond)
		assertCleanupPending(t, pm, uid, mountId)

		assert.NoError(t, os.Chmod(filepath.Join(cacheDir, dirName, "mountpoint-cache"), 0700))
		waitFor(t, "the cleaner to free the UID", func() bool {
			pm.mu.Lock()
			defer pm.mu.Unlock()
			return len(pm.processes) == 0
		})
		assertNotExist(t, filepath.Join(cacheDir, dirName))
		assertMountReleased(t, pm, -1, 0)
		assert.NoError(t, pm.Launch(mountId, "/usr/bin/mount-s3", cachedOptions(t, uid)))

		fr.handles[1].Exit(0, "")
		close(stop)
		pm.Shutdown()
	})

	t.Run("Shutdown signals only running Mountpoints, and waits for the cleaner once stopped", func(t *testing.T) {
		fr := &fakeProcessRunner{}
		pm, cacheDir := newProcessManagerWithCache(t, fr, cacheLimit{strategy: cacheLimitNone})
		exitWithUnremovableCache(t, pm, fr, cacheDir)
		assert.NoError(t, pm.Launch("mount-456", "/usr/bin/mount-s3", cachedOptions(t, uid+1)))
		stop := make(chan struct{})
		pm.StartCacheCleaner(stop, time.Hour)
		go func() {
			<-fr.handles[1].sigCh
			fr.handles[1].Exit(0, "")
		}()

		close(stop)
		pm.Shutdown() // would panic on the marker's nil handle, or hang on the cleaner
		assertCleanupPending(t, pm, uid, mountId)
	})
}

func TestProcessManager_Launch_MaxVolumesPerNode(t *testing.T) {
	options := func(t *testing.T, uid uint32) mountoptions.Options {
		return mountoptions.Options{Fd: int(mountertest.OpenDevNull(t).Fd()), BucketName: "bucket", Uid: uid, Gid: uid}
	}

	t.Run("refuses a launch beyond maxVolumesPerNode, and launches again once a Mountpoint exits", func(t *testing.T) {
		fr := &fakeProcessRunner{}
		pm := NewProcessManager(t.TempDir(), "", fr, memoryLimit{strategy: memoryLimitNone}, cacheLimit{strategy: cacheLimitNone})
		pm.maxVolumesPerNode = 2
		assert.NoError(t, pm.Launch("mount-a", "/usr/bin/mount-s3", options(t, 65536)))
		assert.NoError(t, pm.Launch("mount-b", "/usr/bin/mount-s3", options(t, 65537)))

		third := options(t, 65538)
		err := pm.Launch("mount-c", "/usr/bin/mount-s3", third)
		if err == nil {
			t.Fatal("expected Launch to refuse beyond maxVolumesPerNode")
		}
		assert.Contains(t, err.Error(), "refusing to launch mount mount-c: this node already hosts maxVolumesPerNode=2 volumes")
		assert.Equals(t, 2, len(fr.handles))
		assertMountReleased(t, pm, third.Fd, 2)

		fr.handles[0].Exit(0, "")
		waitFor(t, "mount-a to exit", func() bool {
			pm.mu.Lock()
			defer pm.mu.Unlock()
			return len(pm.processes) == 1
		})
		assert.NoError(t, pm.Launch("mount-c", "/usr/bin/mount-s3", options(t, 65538)))

		fr.handles[1].Exit(0, "")
		fr.handles[2].Exit(0, "")
		pm.Shutdown()
	})

	t.Run("counts a cache still being removed against maxVolumesPerNode", func(t *testing.T) {
		fr := &fakeProcessRunner{}
		pm, cacheDir := newProcessManagerWithCache(t, fr, cacheLimit{strategy: cacheLimitNone})
		pm.maxVolumesPerNode = 1
		createUnremovableCacheEntry(t, cacheDir, "uid-65540")
		assert.NoError(t, pm.markLeftoverCacheDirs())

		err := pm.Launch("mount-a", "/usr/bin/mount-s3", options(t, 65536))
		if err == nil {
			t.Fatal("expected Launch to refuse while a leftover takes the only slot")
		}
		assert.Contains(t, err.Error(), "this node already hosts maxVolumesPerNode=1 volumes")

		assert.NoError(t, os.Chmod(filepath.Join(cacheDir, "uid-65540", "mountpoint-cache"), 0700))
		pm.cleanPendingCacheDirs()
		assert.NoError(t, pm.Launch("mount-a", "/usr/bin/mount-s3", options(t, 65536)))

		fr.handles[0].Exit(0, "")
		pm.Shutdown()
	})

	t.Run("does not cap with maxVolumesPerNode 0", func(t *testing.T) {
		fr := &fakeProcessRunner{}
		pm := NewProcessManager(t.TempDir(), "", fr, memoryLimit{strategy: memoryLimitNone}, cacheLimit{strategy: cacheLimitNone})
		for i := range 5 {
			assert.NoError(t, pm.Launch(fmt.Sprintf("mount-%d", i), "/usr/bin/mount-s3", options(t, uint32(65536+i))))
		}
		for _, h := range fr.handles {
			h.Exit(0, "")
		}
		pm.Shutdown()
	})
}

func TestProcessManager_MarkLeftoverCacheDirs(t *testing.T) {
	t.Run("marks the UID of each per-PV cache directory left, and only those", func(t *testing.T) {
		pm, cacheDir := newProcessManagerWithCache(t, &fakeProcessRunner{}, cacheLimit{strategy: cacheLimitNone})
		for _, name := range []string{"uid-65540", "uid-70000", "pv-other", "uid-x", "uid-065541"} {
			assert.NoError(t, os.Mkdir(filepath.Join(cacheDir, name), 0700))
		}

		assert.NoError(t, pm.markLeftoverCacheDirs())

		assertMountReleased(t, pm, -1, 2)
		assertCleanupPending(t, pm, 65540, "")
		assertCleanupPending(t, pm, 70000, "")
	})

	t.Run("does nothing without a cache volume", func(t *testing.T) {
		pm := NewProcessManager(t.TempDir(), "", &fakeProcessRunner{}, memoryLimit{strategy: memoryLimitNone}, cacheLimit{strategy: cacheLimitNone})
		assert.NoError(t, pm.markLeftoverCacheDirs())
		assertMountReleased(t, pm, -1, 0)
	})
}

func TestParsePerPvCacheDirName(t *testing.T) {
	for _, testCase := range []struct {
		name string
		uid  uint32
		ok   bool
	}{
		{"uid-65536", 65536, true},
		{"uid-0", 0, true},
		{"uid-4294967295", 4294967295, true},
		{"uid-4294967296", 0, false}, // overflows uint32
		{"uid-065536", 0, false},     // not what perPvCacheDirName makes
		{"uid-+1", 0, false},
		{"uid-", 0, false},
		{"uid-x", 0, false},
		{"pv-1", 0, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			uid, ok := parsePerPvCacheDirName(testCase.name)
			assert.Equals(t, testCase.ok, ok)
			assert.Equals(t, testCase.uid, uid)
		})
	}
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
	assertMountReleased(t, pm, int(dev2.Fd()), 1)

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
		Args:       []string{"--cache"},
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
	assertMountReleased(t, pm, int(dev2.Fd()), 1)
	pm.mu.Lock()
	assert.Equals(t, "mount-a", pm.processes[65536].mountId)
	pm.mu.Unlock()

	fr.handles[0].Exit(0, "")
	pm.Shutdown()
}

func TestProcessManager_RemoveCacheVolumeEntry(t *testing.T) {
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
			{name: "a subdirectory of a PV's cache", entry: "uid-65536/mountpoint-cache/V2"},
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
			Args:       []string{"--cache"},
			Uid:        mountoptions.UIDRangeStart,
			Gid:        mountoptions.UIDRangeStart,
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
		{"below the range", mountoptions.UIDRangeStart - 1, mountoptions.UIDRangeStart - 1},
		{"above the range", mountoptions.UIDRangeEnd + 1, mountoptions.UIDRangeEnd + 1},
		{"GID not matching UID", mountoptions.UIDRangeStart, mountoptions.UIDRangeStart + 1},
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
