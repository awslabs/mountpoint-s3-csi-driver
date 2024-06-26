//go:generate mockgen -source=mount.go -destination=./mocks/mock_mount.go -package=mock_driver
/*
Copyright 2022 The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

   http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package driver

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/awslabs/aws-s3-csi-driver/pkg/system"
	"github.com/google/uuid"
	"k8s.io/klog/v2"
	"k8s.io/mount-utils"
)

const (
	keyIdEnv             = "AWS_ACCESS_KEY_ID"
	accessKeyEnv         = "AWS_SECRET_ACCESS_KEY"
	regionEnv            = "AWS_REGION"
	defaultRegionEnv     = "AWS_DEFAULT_REGION"
	stsEndpointsEnv      = "AWS_STS_REGIONAL_ENDPOINTS"
	MountS3PathEnv       = "MOUNT_S3_PATH"
	awsMaxAttemptsEnv    = "AWS_MAX_ATTEMPTS"
	defaultMountS3Path   = "/usr/bin/mount-s3"
	procMounts           = "/host/proc/mounts"
	userAgentPrefix      = "--user-agent-prefix"
	awsMaxAttemptsOption = "--aws-max-attempts"
	csiDriverPrefix      = "s3-csi-driver/"
)

type MountCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
	Region          string
	DefaultRegion   string
	WebTokenPath    string
	StsEndpoints    string
	AwsRoleArn      string
}

// Get environment variables to pass to mount-s3 for authentication.
func (mc *MountCredentials) Env() []string {
	env := []string{}

	if mc.AccessKeyID != "" && mc.SecretAccessKey != "" {
		env = append(env, keyIdEnv+"="+mc.AccessKeyID)
		env = append(env, accessKeyEnv+"="+mc.SecretAccessKey)
	}
	if mc.WebTokenPath != "" {
		env = append(env, webIdentityTokenEnv+"="+mc.WebTokenPath)
		env = append(env, roleArnEnv+"="+mc.AwsRoleArn)
	}
	if mc.Region != "" {
		env = append(env, regionEnv+"="+mc.Region)
	}
	if mc.DefaultRegion != "" {
		env = append(env, defaultRegionEnv+"="+mc.DefaultRegion)
	}
	if mc.StsEndpoints != "" {
		env = append(env, stsEndpointsEnv+"="+mc.StsEndpoints)
	}

	return env
}

type Fs interface {
	Stat(name string) (os.FileInfo, error)
	MkdirAll(path string, perm os.FileMode) error
	Remove(name string) error
}

type OsFs struct{}

func (OsFs) Stat(name string) (os.FileInfo, error) {
	return os.Stat(name)
}

func (OsFs) MkdirAll(path string, perm os.FileMode) error {
	return os.MkdirAll(path, perm)
}

func (OsFs) Remove(path string) error {
	return os.Remove(path)
}

// Mounter is an interface for mount operations
type Mounter interface {
	Mount(bucketName string, target string, credentials *MountCredentials, options []string) error
	Unmount(target string) error
	IsMountPoint(target string) (bool, error)
}

type ServiceRunner interface {
	StartService(ctx context.Context, config *system.ExecConfig) (string, error)
	RunOneshot(ctx context.Context, config *system.ExecConfig) (string, error)
}

type MountLister interface {
	ListMounts() ([]mount.MountPoint, error)
}

type ProcMountLister struct {
	ProcMountPath string
}

type S3Mounter struct {
	Ctx         context.Context
	Runner      ServiceRunner
	Fs          Fs
	MountLister MountLister
	MpVersion   string
	MountS3Path string
}

func MountS3Path() string {
	mountS3Path := os.Getenv(MountS3PathEnv)
	if mountS3Path == "" {
		mountS3Path = defaultMountS3Path
	}
	return mountS3Path
}

func NewS3Mounter(mpVersion string) (*S3Mounter, error) {
	ctx := context.Background()
	runner, err := system.StartOsSystemdSupervisor()
	if err != nil {
		return nil, fmt.Errorf("failed to start systemd supervisor: %w", err)
	}
	return &S3Mounter{
		Ctx:         ctx,
		Runner:      runner,
		Fs:          &OsFs{},
		MountLister: &ProcMountLister{ProcMountPath: procMounts},
		MpVersion:   mpVersion,
		MountS3Path: MountS3Path(),
	}, nil
}

func (pml *ProcMountLister) ListMounts() ([]mount.MountPoint, error) {
	mounts, err := os.ReadFile(pml.ProcMountPath)
	if err != nil {
		return nil, fmt.Errorf("Failed to read %s: %w", procMounts, err)
	}
	return parseProcMounts(mounts)
}

func (m *S3Mounter) IsMountPoint(target string) (bool, error) {
	if _, err := m.Fs.Stat(target); os.IsNotExist(err) {
		return false, err
	}

	mountPoints, err := m.MountLister.ListMounts()
	if err != nil {
		return false, fmt.Errorf("Failed to list mounts: %w", err)
	}
	for _, mp := range mountPoints {
		if mp.Path == target {
			return true, nil
		}
	}
	return false, nil
}

// Mount the given bucket at the target path. Options will be passed through mostly unchanged,
// with the exception of the user agent prefix which will be added to the Mountpoint headers.
// This method will create the target path if it does not exist and if there is an existing corrupt
// mount, it will attempt an unmount before attempting the mount.
func (m *S3Mounter) Mount(bucketName string, target string,
	credentials *MountCredentials, options []string) error {

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
	_, statErr := m.Fs.Stat(target)
	if statErr != nil {
		// does not exist, create the directory
		if os.IsNotExist(statErr) {
			if err := m.Fs.MkdirAll(target, 0755); err != nil {
				return fmt.Errorf("Failed to create target directory: %w", err)
			}
			cleanupDir = true
			defer func() {
				if cleanupDir {
					if err := m.Fs.Remove(target); err != nil {
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
	if credentials != nil {
		env = credentials.Env()
	}
	env, options = addOptionToEnvironmentVariables(awsMaxAttemptsOption, awsMaxAttemptsEnv, options, env)

	output, err := m.Runner.StartService(timeoutCtx, &system.ExecConfig{
		Name:        "mount-s3-" + m.MpVersion + "-" + uuid.New().String() + ".service",
		Description: "Mountpoint for Amazon S3 CSI driver FUSE daemon",
		ExecPath:    m.MountS3Path,
		Args:        append(addUserAgentToOptions(options), bucketName, target),
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

func addOptionToEnvironmentVariables(optionName string, envName string, options []string, env []string) ([]string, []string) {
	// optionName is passed to the driver as a mount option, but is passed to MP via env variable
	// so we need to remove it from the options and add it to the env.
	optionIdx := -1
	for i, o := range options {
		if strings.HasPrefix(o, optionName) {
			optionIdx = i
			break
		}
	}
	if optionIdx != -1 {
		env = append(env, strings.Replace(options[optionIdx], optionName, envName, 1))
		options = append(options[:optionIdx], options[optionIdx+1:]...)
	}
	return env, options
}

// method to add the user agent prefix to the Mountpoint headers
// https://github.com/awslabs/mountpoint-s3/pull/548
func addUserAgentToOptions(options []string) []string {
	// first remove it from the options in case it's in there
	for i := len(options) - 1; i >= 0; i-- {
		if strings.Contains(options[i], userAgentPrefix) {
			options = append(options[:i], options[i+1:]...)
		}
	}
	// add the hard coded S3 CSI driver user agent string
	return append(options, userAgentPrefix+"="+csiDriverPrefix+GetVersion().DriverVersion)
}

func (m *S3Mounter) Unmount(target string) error {
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
	return nil
}

func parseProcMounts(data []byte) ([]mount.MountPoint, error) {
	var mounts []mount.MountPoint

	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 6 {
			return nil, fmt.Errorf("Invalid line in mounts file: %s", line)
		}

		mountPoint := mount.MountPoint{
			Device: fields[0],
			Path:   fields[1],
			Type:   fields[2],
			Opts:   strings.Split(fields[3], ","),
		}

		// Fields 4 and 5 are Freq and Pass respectively. Ignoring

		mounts = append(mounts, mountPoint)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("Error reading mounts data: %w", err)
	}

	return mounts, nil
}
