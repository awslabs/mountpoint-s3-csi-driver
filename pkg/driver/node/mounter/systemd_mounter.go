package mounter

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"k8s.io/klog/v2"
	"k8s.io/mount-utils"

	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/envprovider"
	"github.com/awslabs/aws-s3-csi-driver/pkg/mountpoint"
	"github.com/awslabs/aws-s3-csi-driver/pkg/system"
)

var defaultMountS3Path = "/usr/bin/mount-s3"

// https://github.com/awslabs/mountpoint-s3/blob/9ed8b6243f4511e2013b2f4303a9197c3ddd4071/mountpoint-s3/src/cli.rs#L421
const mountpointDeviceName = "mountpoint-s3"

type SystemdMounter struct {
	Ctx         context.Context
	Runner      ServiceRunner
	Mounter     mount.Interface
	MpVersion   string
	MountS3Path string
}

func NewSystemdMounter(mpVersion string) (*SystemdMounter, error) {
	ctx := context.Background()
	runner, err := system.StartOsSystemdSupervisor()
	if err != nil {
		return nil, fmt.Errorf("failed to start systemd supervisor: %w", err)
	}
	return &SystemdMounter{
		Ctx:         ctx,
		Runner:      runner,
		Mounter:     mount.New(""),
		MpVersion:   mpVersion,
		MountS3Path: MountS3Path(),
	}, nil
}

// IsMountPoint returns whether given `target` is a `mount-s3` mount.
// We implement the IsMountPoint interface instead of using Kubernetes' implementation
// because we need to verify not only that the target is a mount point but also that it is specifically a mount-s3 mount point.
// This is achieved by calling the mounter.List() method to enumerate all mount points.
func (m *SystemdMounter) IsMountPoint(target string) (bool, error) {
	if _, err := os.Stat(target); os.IsNotExist(err) {
		return false, err
	}

	mountPoints, err := m.Mounter.List()
	if err != nil {
		return false, fmt.Errorf("Failed to list mounts: %w", err)
	}
	for _, mp := range mountPoints {
		if mp.Path == target {
			if mp.Device != mountpointDeviceName {
				klog.V(4).Infof("IsMountPoint: %s is not a `mount-s3` mount. Expected device type to be %s but got %s, skipping unmount", target, mountpointDeviceName, mp.Device)
				continue
			}

			return true, nil
		}
	}
	return false, nil
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
func (m *SystemdMounter) Mount(bucketName string, target string, credentials credentialprovider.Credentials, env envprovider.Environment, args mountpoint.Args) error {
	if bucketName == "" {
		return fmt.Errorf("bucket name is empty")
	}
	if target == "" {
		return fmt.Errorf("target is empty")
	}
	timeoutCtx, cancel := context.WithTimeout(m.Ctx, 30*time.Second)
	defer cancel()

	cleanupDir := false

	// check if the target path exists
	_, statErr := os.Stat(target)
	if statErr != nil {
		// does not exist, create the directory
		if os.IsNotExist(statErr) {
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
		if mount.IsCorruptedMnt(statErr) {
			klog.V(4).Infof("Mount: Target path %q is a corrupted mount. Trying to unmount.", target)
			if mntErr := m.Unmount(target); mntErr != nil {
				return fmt.Errorf("Unable to unmount the target %q : %v, %v", target, statErr, mntErr)
			}
		}
	}

	isMountPoint, err := m.IsMountPoint(target)
	if err != nil {
		return fmt.Errorf("Could not check if %q is a mount point: %v, %v", target, statErr, err)
	}

	credentialsBasepath, err := m.ensureCredentialsDirExists(target)
	if err != nil {
		return err
	}

	// Note that this part happens before `isMountPoint` check, as we want to update credentials even though
	// there is an existing mount point at `target`.
	credentialsEnv, err := credentials.Dump(credentialsBasepath, credentialsBasepath)
	if err != nil {
		klog.V(4).Infof("NodePublishVolume: Failed to dump credentials for %s: %v", target, err)
		return err
	}

	env = append(env, credentialsEnv...)

	if isMountPoint {
		klog.V(4).Infof("NodePublishVolume: Target path %q is already mounted", target)
		return nil
	}

	output, err := m.Runner.StartService(timeoutCtx, &system.ExecConfig{
		Name:        "mount-s3-" + m.MpVersion + "-" + uuid.New().String() + ".service",
		Description: "Mountpoint for Amazon S3 CSI driver FUSE daemon",
		ExecPath:    m.MountS3Path,
		Args:        append(args.SortedList(), bucketName, target),
		Env:         env,
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

func (m *SystemdMounter) Unmount(target string) error {
	timeoutCtx, cancel := context.WithTimeout(m.Ctx, 30*time.Second)
	defer cancel()

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

	credentialsBasepath := m.credentialsDir(target)
	err = os.RemoveAll(credentialsBasepath)
	if err != nil {
		klog.V(5).Infof("NodePublishVolume: Failed to clean up credentials for %s: %v", target, err)
		return nil
	}

	return nil
}

// ensureCredentialsDirExists ensures credentials dir for `target` is exists.
// It returns credentials dir and any error.
func (m *SystemdMounter) ensureCredentialsDirExists(target string) (string, error) {
	credentialsBasepath := m.credentialsDir(target)
	if err := os.Mkdir(credentialsBasepath, credentialprovider.CredentialDirPerm); !errors.Is(err, fs.ErrExist) {
		klog.V(4).Infof("NodePublishVolume: Failed to create credentials directory for %s: %v", target, err)
		return "", err
	}

	return credentialsBasepath, nil
}

// credentialsDir returns a directory to write credentials to for given `target`.
//
// Kubernetes creates target path in the form of "/var/lib/kubelet/pods/<pod-uuid>/volumes/kubernetes.io~csi/<volume-id>/mount".
// So, the directory of the target path is unique for this mount, and we're creating a new folder in this path for credentials.
// The credentials folder and all its contents will be cleaned up in `Unmount`.
func (m *SystemdMounter) credentialsDir(target string) string {
	return filepath.Join(filepath.Dir(target), "credentials")
}
