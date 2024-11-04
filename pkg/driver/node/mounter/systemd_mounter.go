package mounter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/awsprofile"
	"github.com/awslabs/aws-s3-csi-driver/pkg/system"
	"github.com/google/uuid"
	"k8s.io/klog/v2"
	"k8s.io/mount-utils"
)

// https://github.com/awslabs/mountpoint-s3/blob/9ed8b6243f4511e2013b2f4303a9197c3ddd4071/mountpoint-s3/src/cli.rs#L421
const mountpointDeviceName = "mountpoint-s3"

type SystemdMounter struct {
	Ctx               context.Context
	Runner            ServiceRunner
	MountLister       MountLister
	MpVersion         string
	MountS3Path       string
	kubernetesVersion string
}

func NewSystemdMounter(mpVersion string, kubernetesVersion string) (*SystemdMounter, error) {
	ctx := context.Background()
	runner, err := system.StartOsSystemdSupervisor()
	if err != nil {
		return nil, fmt.Errorf("failed to start systemd supervisor: %w", err)
	}
	return &SystemdMounter{
		Ctx:               ctx,
		Runner:            runner,
		MountLister:       &ProcMountLister{ProcMountPath: procMounts},
		MpVersion:         mpVersion,
		MountS3Path:       MountS3Path(),
		kubernetesVersion: kubernetesVersion,
	}, nil
}

// IsMountPoint returns whether given `target` is a `mount-s3` mount.
func (m *SystemdMounter) IsMountPoint(target string) (bool, error) {
	if _, err := os.Stat(target); os.IsNotExist(err) {
		return false, err
	}

	mountPoints, err := m.MountLister.ListMounts()
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
func (m *SystemdMounter) Mount(bucketName string, target string, credentials *MountCredentials, options []string) error {
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

	mounts, err := m.MountLister.ListMounts()
	if err != nil {
		return fmt.Errorf("Could not check if %q is a mount point: %v, %v", target, statErr, err)
	}
	for _, m := range mounts {
		if m.Path == target {
			klog.V(4).Infof("NodePublishVolume: Target path %q is already mounted", target)
			return nil
		}
	}

	env := []string{}
	var authenticationSource AuthenticationSource
	if credentials != nil {
		var awsProfile awsprofile.AWSProfile
		if credentials.AccessKeyID != "" && credentials.SecretAccessKey != "" {
			// Kubernetes creates target path in the form of "/var/lib/kubelet/pods/<pod-uuid>/volumes/kubernetes.io~csi/<volume-id>/mount".
			// So the directory of the target path is unique for this mount, and we can use it to write credentials and config files.
			// These files will be cleaned up in `Unmount`.
			basepath := filepath.Dir(target)
			awsProfile, err = awsprofile.CreateAWSProfile(basepath, credentials.AccessKeyID, credentials.SecretAccessKey, credentials.SessionToken)
			if err != nil {
				klog.V(4).Infof("Mount: Failed to create AWS Profile in %s: %v", basepath, err)
				return fmt.Errorf("Mount: Failed to create AWS Profile in %s: %v", basepath, err)
			}
		}

		authenticationSource = credentials.AuthenticationSource

		env = credentials.Env(awsProfile)
	}
	options, env = moveOptionToEnvironmentVariables(awsMaxAttemptsOption, awsMaxAttemptsEnv, options, env)
	options = addUserAgentToOptions(options, UserAgent(authenticationSource, m.kubernetesVersion))

	output, err := m.Runner.StartService(timeoutCtx, &system.ExecConfig{
		Name:        "mount-s3-" + m.MpVersion + "-" + uuid.New().String() + ".service",
		Description: "Mountpoint for Amazon S3 CSI driver FUSE daemon",
		ExecPath:    m.MountS3Path,
		Args:        append(options, bucketName, target),
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

// Moves a parameter optionName from the options list to MP's environment variable list. We need this as options are
// passed to the driver in a single field, but MP sometimes only supports config from environment variables.
// Returns an updated options and environment.
func moveOptionToEnvironmentVariables(optionName string, envName string, options []string, env []string) ([]string, []string) {
	optionIdx := -1
	for i, o := range options {
		if strings.HasPrefix(o, optionName) {
			optionIdx = i
			break
		}
	}
	if optionIdx != -1 {
		// We can do replace here as we've just verified it has the right prefix
		env = append(env, strings.Replace(options[optionIdx], optionName, envName, 1))
		options = append(options[:optionIdx], options[optionIdx+1:]...)
	}
	return options, env
}

// method to add the user agent prefix to the Mountpoint headers
// https://github.com/awslabs/mountpoint-s3/pull/548
func addUserAgentToOptions(options []string, userAgent string) []string {
	// first remove it from the options in case it's in there
	for i := len(options) - 1; i >= 0; i-- {
		if strings.Contains(options[i], userAgentPrefix) {
			options = append(options[:i], options[i+1:]...)
		}
	}
	// add the hard coded S3 CSI driver user agent string
	return append(options, userAgentPrefix+"="+userAgent)
}

func (m *SystemdMounter) Unmount(target string) error {
	timeoutCtx, cancel := context.WithTimeout(m.Ctx, 30*time.Second)
	defer cancel()

	basepath := filepath.Dir(target)
	err := awsprofile.CleanupAWSProfile(basepath)
	if err != nil {
		klog.V(4).Infof("Unmount: Failed to clean up AWS Profile in %s: %v", basepath, err)
	}

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
	return nil
}

const (
	mountpointArgRegion = "region"
	mountpointArgCache  = "cache"
)

// ExtractMountpointArgument extracts value of a given argument from `mountpointArgs`.
func ExtractMountpointArgument(mountpointArgs []string, argument string) (string, bool) {
	// `mountpointArgs` normalized to `--arg=val` in `S3NodeServer.NodePublishVolume`.
	prefix := fmt.Sprintf("--%s=", argument)
	for _, arg := range mountpointArgs {
		if strings.HasPrefix(arg, prefix) {
			val := strings.SplitN(arg, "=", 2)[1]
			return val, true
		}
	}
	return "", false
}
