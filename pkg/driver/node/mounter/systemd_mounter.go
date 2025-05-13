package mounter

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"k8s.io/klog/v2"
	"k8s.io/mount-utils"

	"github.com/scality/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/scality/mountpoint-s3-csi-driver/pkg/driver/node/envprovider"
	"github.com/scality/mountpoint-s3-csi-driver/pkg/mountpoint"
	"github.com/scality/mountpoint-s3-csi-driver/pkg/system"
)

type SystemdMounter struct {
	Runner            ServiceRunner
	Mounter           mount.Interface
	MpVersion         string
	MountS3Path       string
	kubernetesVersion string
	credProvider      *credentialprovider.Provider
}

func NewSystemdMounter(credProvider *credentialprovider.Provider, mpVersion string, kubernetesVersion string) (*SystemdMounter, error) {
	runner, err := system.StartOsSystemdSupervisor()
	if err != nil {
		return nil, fmt.Errorf("failed to start systemd supervisor: %w", err)
	}
	return &SystemdMounter{
		Runner:            runner,
		Mounter:           mount.New(""),
		MpVersion:         mpVersion,
		MountS3Path:       MountS3Path(),
		kubernetesVersion: kubernetesVersion,
		credProvider:      credProvider,
	}, nil
}

// IsMountPoint returns whether given `target` is a `mount-s3` mount.
func (m *SystemdMounter) IsMountPoint(target string) (bool, error) {
	return isMountPoint(m.Mounter, target)
}

// Mount mounts the given bucket at the target path using provided credentials.
//
// Options will be passed through mostly unchanged, with the exception of
// the user agent prefix which will be added to the Mountpoint headers.
//
// Long-term credentials will be passed via credentials file and
// the rest will be passed through environment variables.
//
// This method will create the target path if it does not exist and if there is an existing corrupt
// mount, it will attempt an unmount before attempting the mount.
func (m *SystemdMounter) Mount(ctx context.Context, bucketName string, target string, credentialCtx credentialprovider.ProvideContext, args mountpoint.Args) error {
	if bucketName == "" {
		return fmt.Errorf("bucket name is empty")
	}
	if target == "" {
		return fmt.Errorf("target is empty")
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	credentialCtx.SetWriteAndEnvPath(m.credentialWriteAndEnvPath())

	cleanupDir := false

	// check if the target path exists and is a directory
	mountpointErr := verifyMountPointStatx(target)
	if mountpointErr != nil {
		// does not exist, create the directory
		if os.IsNotExist(mountpointErr) {
			if err := os.MkdirAll(target, 0755); err != nil {
				return fmt.Errorf("Failed to create target directory: %w", err)
			}
			cleanupDir = true
			defer func() {
				if cleanupDir {
					if err := os.Remove(target); err != nil {
						klog.V(4).Infof("Mount: Failed to delete target dir: %s.", target)
					}
				}
			}()
		}
		// Corrupted mount, try unmounting
		if mount.IsCorruptedMnt(mountpointErr) {
			klog.V(4).Infof("Mount: Target path %q is a corrupted mount. Trying to unmount.", target)
			if mntErr := m.Unmount(ctx, target, credentialprovider.CleanupContext{
				WritePath: credentialCtx.WritePath,
				PodID:     credentialCtx.PodID,
				VolumeID:  credentialCtx.VolumeID,
			}); mntErr != nil {
				return fmt.Errorf("Unable to unmount the target %q : %v, %v", target, mountpointErr, mntErr)
			}
		}
	}

	isMountPoint, err := m.IsMountPoint(target)
	if err != nil {
		return fmt.Errorf("Could not check if %q is a mount point: %v, %v", target, mountpointErr, err)
	}

	credEnv, authenticationSource, err := m.credProvider.Provide(ctx, credentialCtx)
	if err != nil {
		klog.V(4).Infof("NodePublishVolume: Failed to provide credentials for %s: %v", target, err)
		return err
	}

	if isMountPoint {
		klog.V(4).Infof("NodePublishVolume: Target path %q is already mounted", target)
		return nil
	}

	env := envprovider.Default()
	env.Merge(credEnv)

	// Move `--aws-max-attempts` to env if provided
	if maxAttempts, ok := args.Remove(mountpoint.ArgAWSMaxAttempts); ok {
		env.Set(envprovider.EnvMaxAttempts, maxAttempts)
	}

	enforceCSIDriverMountArgPolicy(&args)

	args.Set(mountpoint.ArgUserAgentPrefix, UserAgent(authenticationSource, m.kubernetesVersion))

	output, err := m.Runner.StartService(timeoutCtx, &system.ExecConfig{
		Name:        "mount-s3-" + m.MpVersion + "-" + uuid.New().String() + ".service",
		Description: "Mountpoint for Amazon S3 CSI driver FUSE daemon",
		ExecPath:    m.MountS3Path,
		Args:        append(args.SortedList(), bucketName, target),
		Env:         env.List(),
	})

	if err != nil {
		return fmt.Errorf("Mount failed: %w output: %s", err, output)
	}
	if output != "" {
		klog.V(5).Infof("mount-s3 output: %s", output)
	}
	cleanupDir = false
	return nil
}

func (m *SystemdMounter) Unmount(ctx context.Context, target string, credentialCtx credentialprovider.CleanupContext) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	credentialCtx.WritePath, _ = m.credentialWriteAndEnvPath()

	output, err := m.Runner.RunOneshot(timeoutCtx, &system.ExecConfig{
		Name:        "mount-s3-umount-" + uuid.New().String() + ".service",
		Description: "Mountpoint for Amazon S3 CSI driver unmount",
		ExecPath:    "/usr/bin/umount",
		Args:        []string{target},
	})
	if err != nil {
		return fmt.Errorf("Unmount failed: %w unmount output: %s", err, output)
	}
	if output != "" {
		klog.V(5).Infof("umount output: %s", output)
	}

	err = m.credProvider.Cleanup(credentialCtx)
	if err != nil {
		klog.V(4).Infof("Unmount: Failed to clean up credentials for %s: %v", target, err)
	}

	return nil
}

func (m *SystemdMounter) credentialWriteAndEnvPath() (writePath string, envPath string) {
	// This is the plugin directory for CSI driver mounted in the container.
	writePath = "/csi"
	// This is the plugin directory for CSI driver in the host.
	envPath = hostPluginDirWithDefault()
	return writePath, envPath
}

func hostPluginDirWithDefault() string {
	hostPluginDir := os.Getenv("HOST_PLUGIN_DIR")
	if hostPluginDir == "" {
		hostPluginDir = "/var/lib/kubelet/plugins/s3.csi.scality.com/"
	}
	return hostPluginDir
}
