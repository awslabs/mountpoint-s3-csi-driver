package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

// startServing starts serve and returns once it listens, which is after its startup cleanup.
func startServing(t *testing.T, pm *ProcessManager) (stop chan struct{}, done chan error) {
	sock := filepath.Join(t.TempDir(), mountSockName)
	stop, done = make(chan struct{}), make(chan error, 1)
	go func() { done <- serve(pm, sock, "/usr/bin/mount-s3", stop) }()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Lstat(sock); err == nil {
			return stop, done
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve did not listen on %s", sock)
		}
	}
}

func TestServe(t *testing.T) {
	t.Run("empties the cache volume before listening", func(t *testing.T) {
		pm, cacheDir := newProcessManagerWithCache(t, &fakeProcessRunner{})
		assert.NoError(t, os.Mkdir(filepath.Join(cacheDir, "pv-old"), 0700))
		stop, done := startServing(t, pm)
		assertNotExist(t, filepath.Join(cacheDir, "pv-old"))
		close(stop)
		assert.NoError(t, <-done)
	})

	t.Run("still listens when the startup cleanup cannot remove an entry", func(t *testing.T) {
		pm, cacheDir := newProcessManagerWithCache(t, &fakeProcessRunner{})
		createUnremovableCacheEntry(t, cacheDir, "pv-stuck")
		stop, done := startServing(t, pm)
		close(stop)
		<-done
	})

	t.Run("empties the cache volume after stopping", func(t *testing.T) {
		pm, cacheDir := newProcessManagerWithCache(t, &fakeProcessRunner{})
		stop, done := startServing(t, pm)
		assert.NoError(t, os.Mkdir(filepath.Join(cacheDir, "pv-new"), 0700))
		close(stop)
		assert.NoError(t, <-done)
		assertNotExist(t, filepath.Join(cacheDir, "pv-new"))
	})

	t.Run("fails at exit naming what it could not remove, so it shows in the pod's status", func(t *testing.T) {
		pm, cacheDir := newProcessManagerWithCache(t, &fakeProcessRunner{})
		stop, done := startServing(t, pm)
		createUnremovableCacheEntry(t, cacheDir, "pv-stuck")
		close(stop)
		err := <-done
		if err == nil {
			t.Fatal("expected serve to fail at exit on what it could not remove")
		}
		assert.Contains(t, err.Error(), "still holds [pv-stuck]")
	})
}
