package mountoptions_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/awslabs/aws-s3-csi-driver/pkg/podmounter/mountoptions"
	"github.com/awslabs/aws-s3-csi-driver/pkg/util"
	"github.com/awslabs/aws-s3-csi-driver/pkg/util/testutil/assert"
)

func TestMountOptions(t *testing.T) {
	// Go returns `invalid argument` errors if you try to do `net.Listen()` or `net.Dial()` with a Unix socket
	// path thats longer than 108 characters by default. We're creating symlinks automatically and use those Unix
	// socket paths in this case. Here we add test cases for both short and long Unix socket paths.
	// See https://github.com/golang/go/issues/6895 for more details.

	t.Run("Short Path", func(t *testing.T) {
		mountSock := filepath.Join(t.TempDir(), "m")
		if len(mountSock) >= 108 {
			t.Fatalf("test Unix socket path %q must be shorter than 108 characters", mountSock)
		}
		testRoundtrip(t, mountSock)
	})

	t.Run("Long Path", func(t *testing.T) {
		basepath := filepath.Join(t.TempDir(), "long"+strings.Repeat("g", 108))
		assert.NoError(t, os.Mkdir(basepath, 0700))

		mountSock := filepath.Join(basepath, "mount.sock")
		if len(mountSock) <= 108 {
			t.Fatalf("test Unix socket path %q must be longer than 108 characters", mountSock)
		}
		testRoundtrip(t, mountSock)
	})
}

func testRoundtrip(t *testing.T, mountSock string) {
	file, err := os.Open(os.DevNull)
	assert.NoError(t, err)
	defer file.Close()

	var wantStat = &syscall.Stat_t{}
	err = syscall.Fstat(int(file.Fd()), wantStat)
	assert.NoError(t, err)

	c := make(chan mountoptions.Options)
	go func() {
		mountOptions, err := mountoptions.Recv(defaultContext(t), mountSock)
		assert.NoError(t, err)
		c <- mountOptions
	}()

	err = util.WaitForUnixSocket(defaultTimeout, 500*time.Millisecond, mountSock)
	assert.NoError(t, err)

	want := mountoptions.Options{
		Fd:         int(file.Fd()),
		BucketName: "test-bucket",
		Args:       []string{"--bucket=testing"},
		Env:        []string{"TEST_ENV=testing"},
	}
	err = mountoptions.Send(defaultContext(t), mountSock, want)
	assert.NoError(t, err)

	got := <-c

	gotFile := os.NewFile(uintptr(got.Fd), "fd")
	if gotFile == nil {
		t.Fatalf("received file descriptor %d is invalid\n", got.Fd)
	}

	var gotStat = &syscall.Stat_t{}
	err = syscall.Fstat(got.Fd, gotStat)
	assert.NoError(t, err)

	// Reset fds as they might be different in different ends.
	// To verify underlying objects are the same, we need to compare "dev" and "ino" from "fstat" syscall.
	got.Fd = 0
	want.Fd = 0
	assert.Equals(t, wantStat.Dev, gotStat.Dev)
	assert.Equals(t, wantStat.Ino, gotStat.Ino)
	assert.Equals(t, want, got)
}

const defaultTimeout = 10 * time.Second

func defaultContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	t.Cleanup(cancel)
	return ctx
}
