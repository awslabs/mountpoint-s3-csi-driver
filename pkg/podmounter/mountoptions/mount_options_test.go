package mountoptions_test

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/awslabs/aws-s3-csi-driver/pkg/podmounter/mountoptions"
	"github.com/awslabs/aws-s3-csi-driver/pkg/util"
	"github.com/awslabs/aws-s3-csi-driver/pkg/util/testutil/assert"
)

func TestSendingAndReceivingMountOptions(t *testing.T) {
	file, err := os.Open(os.DevNull)
	assert.NoError(t, err)
	defer file.Close()

	var wantStat = &syscall.Stat_t{}
	err = syscall.Fstat(int(file.Fd()), wantStat)
	assert.NoError(t, err)

	mountSock := filepath.Join(t.TempDir(), "m")

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
