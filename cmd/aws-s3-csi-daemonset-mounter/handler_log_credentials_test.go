package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/mounter/mountertest"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mountoptions"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

// The mounter receives a mount's credentials in the options csi-node sends, and passes them to Mountpoint
// in its environment. Across the whole lifecycle (receive, launch, Mountpoint failing, shutdown) it logs
// the mount ID and bucket, never the environment.
//
// Mountpoint's own stderr is copied to the logs as is, so this does not cover what Mountpoint prints.
func TestHandleConnection_LogsNoCredentials(t *testing.T) {
	const (
		accessKeyID     = "AKIAIOSFODNN7EXAMPLE"
		secretAccessKey = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
		sessionToken    = "IQoJb3JpZ2luX2VjEXAMPLESESSIONTOKEN"
		// Shaped like a service account token, three base64url parts.
		webIdentityToken = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ0ZXN0In0.c2lnbmF0dXJl"
	)
	logs := testutil.CaptureKlog(t, 10)

	commDir := t.TempDir()
	fr := &fakeProcessRunner{}
	pm := NewProcessManager(commDir, "", fr, memoryLimit{strategy: memoryLimitNone}, cacheLimit{strategy: cacheLimitNone})

	sockPath := filepath.Join(commDir, "test.sock")
	listener, err := net.Listen("unix", sockPath)
	assert.NoError(t, err)
	defer listener.Close()

	dev := mountertest.OpenDevNull(t)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		mountoptions.Send(ctx, sockPath, mountoptions.Options{
			Fd:         int(dev.Fd()),
			BucketName: "test-bucket",
			Args:       []string{"--region=us-west-2"},
			Env: []string{
				"AWS_ACCESS_KEY_ID=" + accessKeyID,
				"AWS_SECRET_ACCESS_KEY=" + secretAccessKey,
				"AWS_SESSION_TOKEN=" + sessionToken,
				"AWS_WEB_IDENTITY_TOKEN=" + webIdentityToken,
			},
			VolumeId: "s3-pv",
			Uid:      65536,
			Gid:      65536,
		})
	}()

	conn, err := listener.Accept()
	assert.NoError(t, err)
	handleConnection(conn.(*net.UnixConn), "/opt/mount-s3", pm, 5*time.Second)

	fr.mu.Lock()
	assert.Equals(t, 1, len(fr.handles))
	handle := fr.handles[0]
	fr.mu.Unlock()
	// Guards the check below: had the credentials not reached Mountpoint, there would be nothing to leak.
	assert.Equals(t, 4, len(handle.cmd.Env))

	// Mountpoint fails, so the error path logs too. Its stderr carries no credentials here.
	handle.Exit(1, "Error: Failed to create S3 client\n")
	pm.Shutdown()
	// Shutdown returns once the exit is handled; the error file proves the failure path ran.
	_, err = os.Stat(filepath.Join(commDir, "s3-pv"+errorFileExt))
	assert.NoError(t, err)

	testutil.AssertNoCredentialsLogged(t, logs(), accessKeyID, secretAccessKey, sessionToken, webIdentityToken)
}
