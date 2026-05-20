package main

import (
	"net"
	"regexp"
	"syscall"
	"time"

	"k8s.io/klog/v2"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mountoptions"
)

var validMountId = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// handleConnection receives mount options from a single connection and spawns a Mountpoint child process.
func handleConnection(conn *net.UnixConn, mountpointPath string, pm *ProcessManager, recvTimeout time.Duration) {
	defer conn.Close()

	var deadline time.Time
	if recvTimeout > 0 {
		deadline = time.Now().Add(recvTimeout)
	}

	options, err := mountoptions.RecvOnConn(conn, deadline)
	if err != nil {
		klog.Errorf("Failed to receive mount options: %v", err)
		return
	}

	mountId := options.VolumeId
	if mountId == "" || !validMountId.MatchString(mountId) {
		syscall.Close(options.Fd)
		klog.Errorf("Received mount request with invalid mountId: %q", mountId)
		return
	}

	klog.Infof("Received mount request for mount %s, bucket %s", mountId, options.BucketName)

	err = pm.Launch(mountId, mountpointPath, options) // ownership of options.Fd is transferred here
	if err != nil {
		klog.Errorf("Failed to launch Mountpoint for mount %s: %v", mountId, err)
	}
}
