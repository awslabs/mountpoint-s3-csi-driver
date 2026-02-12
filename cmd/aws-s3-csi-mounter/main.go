// `aws-s3-csi-mounter` is the entrypoint binary running on Mountpoint Pods.
// It is responsible for receiving mount options from the CSI Driver Node Pod,
// and spawning a Mountpoint instance in turn.
// It will then wait until Mountpoint process terminates (which normally happens as a result of `unmount`).
//
// See /docs/ARCHITECTURE.md for more details.
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"k8s.io/klog/v2"

	"github.com/awslabs/mountpoint-s3-csi-driver/cmd/aws-s3-csi-mounter/csimounter"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint/mountoptions"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/podmounter/mppod"
)

var mountSockRecvTimeout = flag.Duration("mount-sock-recv-timeout", 2*time.Minute, "Timeout for receiving mount options from passed Unix socket.")
var mountpointBinDir = flag.String("mountpoint-bin-dir", os.Getenv("MOUNTPOINT_BIN_DIR"), "Directory of mount-s3 binary.")

var mountSockPath = mppod.PathInsideMountpointPod(mppod.KnownPathMountSock)
var mountExitPath = mppod.PathInsideMountpointPod(mppod.KnownPathMountExit)
var mountErrorPath = mppod.PathInsideMountpointPod(mppod.KnownPathMountError)

const mountpointBin = "mount-s3"

func main() {
	klog.InitFlags(nil)
	flag.Parse()

	setupSignalHandler()

	mountpointBinFullPath := filepath.Join(*mountpointBinDir, mountpointBin)
	mountOptions, err := recvMountOptions()
	if err != nil {
		if csimounter.ShouldExitWithSuccessCode(mountExitPath) {
			klog.Info("Failed to receive mount options and detected `mount.exit` file, exiting with zero code")
			os.Exit(csimounter.SuccessExitCode)
			return
		}

		klog.Fatalf("Failed to receive mount options from %s: %v. "+
			"This error is often caused by invalid config, "+
			"see the troubleshooting doc: "+
			"https://github.com/awslabs/mountpoint-s3-csi-driver/blob/main/docs/TROUBLESHOOTING.md#mountpoint-pods-are-failing-with-failed-to-receive-mount-options-from-commmountsock\n",
			mountSockPath, err)
	}

	exitCode, err := csimounter.Run(csimounter.Options{
		MountpointPath: mountpointBinFullPath,
		MountExitPath:  mountExitPath,
		MountErrPath:   mountErrorPath,
		MountOptions:   mountOptions,
	})
	if err != nil {
		klog.Fatalf("Failed to run Mountpoint: %v\n", err)
	}
	klog.Infof("Mountpoint exited with %d exit code\n", exitCode)
	os.Exit(exitCode)
}

func recvMountOptions() (mountoptions.Options, error) {
	ctx, cancel := context.WithTimeout(context.Background(), *mountSockRecvTimeout)
	defer cancel()
	klog.Infof("Trying to receive mount options from %s", mountSockPath)
	options, err := mountoptions.Recv(ctx, mountSockPath)
	if err != nil {
		return mountoptions.Options{}, err
	}
	klog.Infof("Mount options has been received from %s", mountSockPath)
	return options, nil
}

// setupSignalHandler captures and ignores SIGTERM signals to prevent default
// termination.
//
// This, combined with TerminationGracePeriodSeconds=10m, prevents Mountpoint
// from terminating before the workload pod. This achieves the desired
// termination order in typical cases where the API respects the grace period
// and the workload terminates within 10 minutes.
//
// Note: The desired order may be violated if the grace period is overridden
// (e.g., via Karpenter NodePool settings) or if the workload takes longer
// than 10 minutes to terminate.
func setupSignalHandler() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM)
	go func() {
		for range sigChan {
			klog.Info("Received SIGTERM, ignoring")
		}
	}()
}
