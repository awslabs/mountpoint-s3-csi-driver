package main

import (
	"fmt"
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

	// Reject a version-skewed CSI Driver Node before spawning anything: the mounter uses an OnDelete
	// update strategy, so after a Helm upgrade a new driver may talk to this (still-old) mounter until
	// the node is recycled. On mismatch we do NOT mount — we close the FUSE fd and surface a clear
	// error via the .error file (which the driver's waitForMount polls), so NodePublishVolume fails
	// fast instead of producing a silent/partial mount.
	if options.ProtocolVersion != mountoptions.ProtocolVersion {
		syscall.Close(options.Fd)
		msg := fmt.Sprintf("protocol version mismatch: mounter=%q driver=%q. "+
			"The s3-csi-daemonset-mounter pod is running a version incompatible with the CSI Driver Node; "+
			"recycle the node or delete the mounter pod so it is recreated at the matching version.",
			mountoptions.ProtocolVersion, options.ProtocolVersion)
		klog.Error(msg)
		pm.WriteErrorFile(mountId, []byte(msg))
		return
	}

	klog.Infof("Received mount request for mount %s, bucket %s", mountId, options.BucketName)

	err = pm.Launch(mountId, mountpointPath, options) // ownership of options.Fd is transferred here
	if err != nil {
		klog.Errorf("Failed to launch Mountpoint for mount %s: %v", mountId, err)
	}
}
