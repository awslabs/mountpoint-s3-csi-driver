package mountoptions_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mountoptions"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

func TestMountOptions(t *testing.T) {
	// Go returns `invalid argument` errors if you try to do `net.Listen()` or `net.Dial()` with a Unix socket
	// path thats longer than 108 characters by default. We're trying to use relative paths if that's the case
	// to make the socket paths shorter. Here we add test cases for both short and long Unix socket paths.
	// See https://github.com/golang/go/issues/6895 for more details.

	t.Run("Short Path", func(t *testing.T) {
		basePath := t.TempDir()
		t.Chdir(basePath)

		mountSock := filepath.Join(basePath, "m")
		if len(mountSock) >= 108 {
			t.Fatalf("test Unix socket path %q must be shorter than 108 characters", mountSock)
		}
		testRoundtrip(t, mountSock)
	})

	t.Run("Long Path", func(t *testing.T) {
		basePath := filepath.Join(t.TempDir(), "long"+strings.Repeat("g", 108))
		sockBasepath := filepath.Join(basePath, "mount")
		assert.NoError(t, os.MkdirAll(sockBasepath, 0700))

		t.Chdir(basePath)

		mountSock := filepath.Join(sockBasepath, "mount.sock")
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

// TestRecvCloseFdsOnError verifies that RecvOnConn closes received file descriptors on error.
// NOTE: Do not use t.Parallel() here — fd counting via /proc/self/fd is process-global
// and would be unreliable if other tests open/close fds concurrently.
func TestRecvCloseFdsOnError(t *testing.T) {
	t.Run("Bad JSON", func(t *testing.T) {
		testRecvClosesFdsOnError(t, []byte("not json"), 1)
	})
	t.Run("More Than One Fd", func(t *testing.T) {
		testRecvClosesFdsOnError(t, mustMarshal(t, mountoptions.Options{BucketName: "b"}), 2)
	})
}

func testRecvClosesFdsOnError(t *testing.T, message []byte, fdCount int) {
	// Create files to send as fds.
	files := make([]*os.File, fdCount)
	for i := range files {
		file, err := os.Open(os.DevNull)
		assert.NoError(t, err)
		defer file.Close()
		files[i] = file
	}

	// Set up a unix socket pair.
	server, client, err := unixSocketPair(t)
	assert.NoError(t, err)
	defer server.Close()

	// Send message with fds from client side.
	fds := make([]int, len(files))
	for i, f := range files {
		fds[i] = int(f.Fd())
	}
	unixRights := syscall.UnixRights(fds...)
	_, _, err = client.WriteMsgUnix(message, unixRights, nil)
	assert.NoError(t, err)
	client.Close()

	// Count open fds before Recv.
	fdsBefore := countOpenFds(t)

	// RecvOnConn should return an error and not leak fds.
	_, recvErr := mountoptions.RecvOnConn(server, time.Time{})
	if recvErr == nil {
		t.Fatal("expected RecvOnConn to return an error")
	}

	// Count open fds after Recv - should not have increased.
	fdsAfter := countOpenFds(t)
	if fdsAfter > fdsBefore {
		t.Errorf("file descriptor leak: had %d open fds before RecvOnConn, have %d after", fdsBefore, fdsAfter)
	}
}

func TestRecvOnConnRespectsDeadline(t *testing.T) {
	server, client, err := unixSocketPair(t)
	assert.NoError(t, err)
	defer server.Close()
	defer client.Close()

	_, err = mountoptions.RecvOnConn(server, time.Now().Add(50*time.Millisecond))
	if err == nil {
		t.Fatal("expected RecvOnConn to return a timeout error")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("expected timeout error, got: %v", err)
	}
}

func unixSocketPair(t *testing.T) (*net.UnixConn, *net.UnixConn, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, err
	}
	serverFile := os.NewFile(uintptr(fds[0]), "server")
	clientFile := os.NewFile(uintptr(fds[1]), "client")

	serverConn, err := net.FileConn(serverFile)
	serverFile.Close() // FileConn dups the fd, close the original.
	if err != nil {
		clientFile.Close()
		return nil, nil, err
	}
	clientConn, err := net.FileConn(clientFile)
	clientFile.Close() // FileConn dups the fd, close the original.
	if err != nil {
		serverConn.Close()
		return nil, nil, err
	}
	return serverConn.(*net.UnixConn), clientConn.(*net.UnixConn), nil
}

func countOpenFds(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	assert.NoError(t, err)
	return len(entries)
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	assert.NoError(t, err)
	return data
}

const defaultTimeout = 10 * time.Second

func defaultContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	t.Cleanup(cancel)
	return ctx
}
