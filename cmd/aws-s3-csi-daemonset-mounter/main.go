// `aws-s3-csi-daemonset-mounter` is the entrypoint binary running on the secondary (mounter) DaemonSet.
// It listens on a Unix domain socket for mount requests from the CSI Driver Node Pod,
// and spawns a Mountpoint instance for each request.
//
// Unlike the pod-per-mount architecture (V2), this binary manages multiple Mountpoint processes
// within a single pod. Each mount request produces exactly one Mountpoint child process.
//
// # Protocol
//
// Communication happens over a single Unix domain socket (mount.sock) in the shared comm directory.
// Each mount request is a separate connection to this socket:
//
//  1. The driver connects and sends a JSON-encoded [mountoptions.Options] message along with
//     the FUSE file descriptor via SCM_RIGHTS (Unix domain socket ancillary data).
//  2. The mounter receives the options, spawns a Mountpoint child process with the FUSE fd,
//     and closes the connection.
//  3. If the Mountpoint process exits with a non-zero code, its stderr is written to
//     <comm-dir>/<mount-id>.error. Nothing is written on clean (zero) exit.
//     The driver is responsible for removing this file during Unmount.
//
// The mount-id (Options.VolumeId) must be unique per active mount (e.g. <WorkloadPodId>-<VolumeId>
// or just <VolumeId> with pod sharing). Duplicate mount-ids are rejected.
//
// Note: if Mountpoint crashes with non-zero exit after the driver has already completed Unmount,
// a small .error file may be left behind. This is bounded by the number of such rare race
// occurrences and each file is only a few KB of stderr.
package main

import (
	"flag"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"k8s.io/klog/v2"
)

var (
	commDir          = flag.String("comm-dir", "/comm", "Directory for communication socket and error files")
	mountpointBinDir = flag.String("mountpoint-bin-dir", os.Getenv("MOUNTPOINT_BIN_DIR"), "Directory of mount-s3 binary")
	recvTimeout      = flag.Duration("recv-timeout", 30*time.Second, "Timeout for receiving mount options from a connection")
)

const (
	mountSockName = "mount.sock"
	mountpointBin = "mount-s3"
)

func main() {
	klog.InitFlags(nil)
	flag.Parse()

	sockPath := filepath.Join(*commDir, mountSockName)
	mountpointPath := filepath.Join(*mountpointBinDir, mountpointBin)

	// Remove stale socket file if it exists
	os.Remove(sockPath)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		klog.Fatalf("Failed to listen on %s: %v", sockPath, err)
	}
	defer listener.Close()

	klog.Infof("Listening on %s, mountpoint binary: %s", sockPath, mountpointPath)

	pm := NewProcessManager(*commDir, &defaultProcessRunner{})

	// Handle shutdown signals: terminate all MP processes gracefully
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		klog.Infof("Received signal %s, closing listener", sig)
		listener.Close()
	}()

	// Periodic observability: log number of tracked and actual child processes
	go pm.LogStatusPeriodically(30 * time.Second)

	// Accept loop — sequential, kernel backlog queues concurrent requests
	for {
		conn, err := listener.Accept()
		if err != nil {
			// Check if listener was closed (shutdown)
			if opErr, ok := err.(*net.OpError); ok && opErr.Err.Error() == "use of closed network connection" {
				klog.Info("Listener closed, exiting accept loop")
				break
			}
			klog.Errorf("Failed to accept connection: %v", err)
			continue
		}

		handleConnection(conn.(*net.UnixConn), mountpointPath, pm, *recvTimeout)
	}

	pm.Shutdown()
}
