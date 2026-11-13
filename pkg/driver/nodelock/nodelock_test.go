package nodelock

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

// The other CSI node in these tests is a separate process, since a flock only excludes other open files, and what the
// lock protects against is another process.

func TestAcquireCreatesTheFileAndItsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plugins", "s3.csi.aws.com", FileName)

	lock, err := Acquire(context.Background(), path)
	assert.NoError(t, err)
	defer lock.file.Close()

	held, err := lock.isAtPath()
	assert.NoError(t, err)
	assert.Equals(t, true, held)
	assert.Equals(t, "busy", tryInOtherProcess(t, path))
}

func TestAcquireWaitsUntilTheHolderExits(t *testing.T) {
	shortenIntervals(t)
	path := filepath.Join(t.TempDir(), FileName)
	other := holdInOtherProcess(t, path)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := Acquire(ctx, path)
	assert.Equals(t, true, errors.Is(err, context.DeadlineExceeded))

	// The kernel releases the lock when its holder exits, however it exits.
	assert.NoError(t, other.Process.Kill())
	_ = other.Wait()

	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lock, err := Acquire(ctx, path)
	assert.NoError(t, err)
	defer lock.file.Close()
}

func TestWatchRelocksAFileDeletedWhileHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	lock, err := Acquire(context.Background(), path)
	assert.NoError(t, err)
	defer func() { lock.file.Close() }()

	assert.NoError(t, os.Remove(path))
	// Without Watch, another process could now lock a new file at path.
	assert.Equals(t, "locked", tryInOtherProcess(t, path))
	assert.NoError(t, os.Remove(path))

	stop := make(chan struct{})
	watching := make(chan struct{})
	go func() {
		defer close(watching)
		lock.Watch(stop, 10*time.Millisecond, func(err error) { t.Errorf("unexpected onLost: %v", err) })
	}()
	eventually(t, func() bool {
		_, err := os.Stat(path)
		return err == nil
	})
	close(stop)
	<-watching

	held, err := lock.isAtPath()
	assert.NoError(t, err)
	assert.Equals(t, true, held)
	assert.Equals(t, "busy", tryInOtherProcess(t, path))
}

func TestWatchReportsTheLockLostWhenAnotherProcessLockedTheReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	lock, err := Acquire(context.Background(), path)
	assert.NoError(t, err)
	defer lock.file.Close()

	assert.NoError(t, os.Remove(path))
	holdInOtherProcess(t, path)

	lost := make(chan error, 1)
	stop := make(chan struct{})
	defer close(stop)
	go lock.Watch(stop, 10*time.Millisecond, func(err error) { lost <- err })
	select {
	case err := <-lost:
		assert.Contains(t, err.Error(), "another CSI node locked the new file")
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not report the lock lost")
	}
}

func TestWatchKeepsAnUnchangedLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	lock, err := Acquire(context.Background(), path)
	assert.NoError(t, err)
	defer lock.file.Close()
	original := lock.file

	stop := make(chan struct{})
	watching := make(chan struct{})
	go func() {
		defer close(watching)
		lock.Watch(stop, 10*time.Millisecond, func(err error) { t.Errorf("unexpected onLost: %v", err) })
	}()
	time.Sleep(100 * time.Millisecond)
	close(stop)
	<-watching
	assert.Equals(t, true, original == lock.file)
}

const (
	helperPathEnv = "NODELOCK_TEST_PATH"
	helperModeEnv = "NODELOCK_TEST_MODE"
)

// TestHelperOtherProcess is the other CSI node in these tests. In "hold" mode it takes the lock, prints "locked", and
// holds it until killed or its stdin closes. In "try" mode it prints whether it could take the lock at once.
func TestHelperOtherProcess(t *testing.T) {
	path := os.Getenv(helperPathEnv)
	if path == "" {
		t.Skip("only runs as the other process in this package's tests")
	}
	switch os.Getenv(helperModeEnv) {
	case "hold":
		lock, err := Acquire(context.Background(), path)
		if err != nil {
			fmt.Println("error:", err)
			os.Exit(1)
		}
		fmt.Println("locked")
		_, _ = io.Copy(io.Discard, os.Stdin)
		lock.file.Close()
	case "try":
		lock, err := tryAcquire(path)
		switch {
		case errors.Is(err, errHeldElsewhere):
			fmt.Println("busy")
		case err != nil || lock == nil:
			fmt.Println("error:", err)
		default:
			fmt.Println("locked")
			lock.file.Close()
		}
	}
	os.Exit(0)
}

// holdInOtherProcess starts a process that holds the lock at path, and returns once it does. It is killed when the
// test ends.
func holdInOtherProcess(t *testing.T, path string) *exec.Cmd {
	t.Helper()
	cmd := helperCommand(path, "hold")
	stdin, err := cmd.StdinPipe()
	assert.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	assert.NoError(t, err)
	assert.NoError(t, cmd.Start())
	t.Cleanup(func() {
		stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	assert.NoError(t, err)
	assert.Equals(t, "locked", strings.TrimSpace(line))
	return cmd
}

// tryInOtherProcess reports whether another process can take the lock at path at once: "locked" or "busy".
func tryInOtherProcess(t *testing.T, path string) string {
	t.Helper()
	out, err := helperCommand(path, "try").Output()
	assert.NoError(t, err)
	return strings.TrimSpace(string(out))
}

func helperCommand(path, mode string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperOtherProcess$")
	cmd.Env = append(os.Environ(), helperPathEnv+"="+path, helperModeEnv+"="+mode)
	return cmd
}

func shortenIntervals(t *testing.T) {
	poll, log := pollInterval, waitLogInterval
	pollInterval, waitLogInterval = 10*time.Millisecond, time.Hour
	t.Cleanup(func() { pollInterval, waitLogInterval = poll, log })
}

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
