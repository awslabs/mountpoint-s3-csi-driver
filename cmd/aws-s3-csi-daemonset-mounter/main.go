// `aws-s3-csi-daemonset-mounter` is the entrypoint binary running on the secondary (mounter) DaemonSet.
// It listens on a Unix domain socket for mount requests from the CSI Driver Node Pod,
// and spawns a Mountpoint process for each request.
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
//  3. If the mounter refuses a request with a valid mount-id, or the Mountpoint process exits with a non-zero
//     code, the reason or its stderr is written to <comm-dir>/<mount-id>.error. Nothing is written on clean (zero) exit.
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
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"k8s.io/klog/v2"
)

var (
	commDir                 = flag.String("comm-dir", "/comm", "Directory for communication socket and error files")
	mountpointBinDir        = flag.String("mountpoint-bin-dir", os.Getenv("MOUNTPOINT_BIN_DIR"), "Directory of mount-s3 binary")
	recvTimeout             = flag.Duration("recv-timeout", 30*time.Second, "Timeout for receiving mount options from a connection")
	stderrCapacity          = flag.Uint("stderr-capacity", 1024*1024, "Maximum bytes of stderr to retain per Mountpoint process (tail)")
	maxVolumesPerNode       = flag.Int64("max-volumes-per-node", 0, "Maximum number of Mountpoint processes this container hosts")
	memoryLimitStrategyFlag = flag.String("memory-limit-strategy", string(memoryLimitEqualSplit),
		"How to size each Mountpoint's --memory-target: \"equalSplit\" to divide this container's memory "+
			"request between max-volumes-per-node Mountpoints, or \"none\" to leave it to the PV mountOptions")
	cacheLimitStrategyFlag = flag.String("cache-limit-strategy", "",
		"How to size each Mountpoint's --max-cache-size when this container has a cache volume: \"equalSplit\" to divide it between "+
			"max-volumes-per-node Mountpoints, or \"none\" to leave it to the PV mountOptions. Unset when it has none")
	cacheMediumFlag = flag.String("cache-medium", "",
		"The cache volume's emptyDir medium; \"Memory\" makes it a tmpfs charged to this container's memory")
	cacheDir = flag.String("cache-dir", "", "The cache volume's mount path, or \"\" when this container has none")
)

const (
	mountSockName = "mount.sock"
	mountpointBin = "mount-s3"
)

func main() {
	klog.InitFlags(nil)
	flag.Parse()

	memoryLimitStrategy, err := parseMemoryLimitStrategy(*memoryLimitStrategyFlag)
	if err != nil {
		klog.Fatalf("Invalid --memory-limit-strategy: %v", err)
	}

	memoryRequestBytes, err := containerMemoryRequestBytes()
	if err != nil {
		klog.Fatalf("Invalid memory request: %v", err)
	}
	cacheVolumeBytes, err := cacheCapacityBytes()
	if err != nil {
		klog.Fatalf("Invalid cache volume size: %v", err)
	}

	memoryLimit, err := newMemoryLimit(memoryLimitStrategy, memoryRequestBytes,
		tmpfsCacheBytes(*cacheMediumFlag, cacheVolumeBytes), *maxVolumesPerNode)
	if err != nil {
		klog.Fatalf("Invalid Mountpoint memory configuration: %v", err)
	}

	// The chart passes --cache-limit-strategy only with a cache volume; without one no mount here caches, so there is nothing to limit.
	cacheLimit := cacheLimit{strategy: cacheLimitNone}
	if *cacheLimitStrategyFlag == "" && (*cacheDir != "" || cacheVolumeBytes > 0 || *cacheMediumFlag != "") {
		klog.Fatalf("This container has a cache volume (--cache-dir, %s or --cache-medium is set) but no --cache-limit-strategy", cacheCapacityEnvName)
	}
	if *cacheLimitStrategyFlag != "" {
		cacheLimitStrategy, err := parseCacheLimitStrategy(*cacheLimitStrategyFlag)
		if err != nil {
			klog.Fatalf("Invalid --cache-limit-strategy: %v", err)
		}
		cacheLimit, err = newCacheLimit(cacheLimitStrategy, cacheVolumeBytes, *maxVolumesPerNode)
		if err != nil {
			klog.Fatalf("Invalid Mountpoint cache configuration: %v", err)
		}
	}

	sockPath := filepath.Join(*commDir, mountSockName)
	mountpointPath := filepath.Join(*mountpointBinDir, mountpointBin)

	pm := NewProcessManager(*commDir, *cacheDir, &defaultProcessRunner{stderrCapacity: *stderrCapacity}, memoryLimit, cacheLimit)

	// Handle shutdown signals: terminate all MP processes gracefully
	stop := make(chan struct{})
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		klog.Infof("Received signal %s, closing listener", sig)
		close(stop)
	}()

	// Periodic observability: log number of tracked and actual child processes
	go pm.LogStatusPeriodically(30 * time.Second)

	if err := serve(pm, sockPath, mountpointPath, stop); err != nil {
		klog.Fatalf("%v", err)
	}
}

// serve empties the cache volume, handles mount requests on sockPath until stop closes, then stops every Mountpoint
// and empties the cache volume again.
func serve(pm *ProcessManager, sockPath, mountpointPath string, stop <-chan struct{}) error {
	// Clean up cache directories after startup, to remove any leftover cache directories from previous mounter pod crash.
	// A directory that cannot be removed fails only when its PV's next cached mount starts and removal is retried, to limit
	// blast radius of a failed cleanup.
	if err := pm.removeLeftoverCacheDirs(); err != nil {
		klog.Errorf("Some leftover cache directories remain: %v", err)
	}

	// Remove stale socket file if it exists
	os.Remove(sockPath)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", sockPath, err)
	}
	defer listener.Close()

	klog.Infof("Listening on %s, mountpoint binary: %s", sockPath, mountpointPath)

	go func() {
		<-stop
		listener.Close()
	}()

	// Accept loop — sequential, kernel backlog queues concurrent requests
	for {
		conn, err := listener.Accept()
		if err != nil {
			// Check if listener was closed (shutdown)
			if errors.Is(err, net.ErrClosed) {
				klog.Info("Listener closed, exiting accept loop")
				break
			}
			klog.Errorf("Failed to accept connection: %v", err)
			continue
		}

		handleConnection(conn.(*net.UnixConn), mountpointPath, pm, *recvTimeout)
	}

	pm.Shutdown()

	// Exit non-zero, so a cache volume the mounter cannot clean shows in the pod's status, not only in a log.
	if err := pm.removeLeftoverCacheDirs(); err != nil {
		return fmt.Errorf("some cache directories could not be removed: %w", err)
	}
	return nil
}
