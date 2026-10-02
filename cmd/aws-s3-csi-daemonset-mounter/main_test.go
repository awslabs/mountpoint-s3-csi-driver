package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

func TestServe_EmptiesTheCacheVolumeBeforeListeningAndAfterStopping(t *testing.T) {
	pm, cacheDir := newProcessManagerWithCache(t, &fakeProcessRunner{}, cacheLimit{strategy: cacheLimitNone})
	assert.NoError(t, os.Mkdir(filepath.Join(cacheDir, "pv-old"), 0700))
	sock := filepath.Join(t.TempDir(), mountSockName)
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- serve(pm, sock, "/usr/bin/mount-s3", stop) }()

	// The socket exists only once the startup cleanup has run.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Lstat(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve did not listen on %s", sock)
		}
	}
	assertNotExist(t, filepath.Join(cacheDir, "pv-old"))

	assert.NoError(t, os.Mkdir(filepath.Join(cacheDir, "pv-new"), 0700))
	close(stop)
	assert.NoError(t, <-done)
	assertNotExist(t, filepath.Join(cacheDir, "pv-new"))
}
