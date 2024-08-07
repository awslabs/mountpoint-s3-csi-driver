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
	keyIdEnv                 = "AWS_ACCESS_KEY_ID"
	accessKeyEnv             = "AWS_SECRET_ACCESS_KEY"
	sessionTokenEnv          = "AWS_SESSION_TOKEN"
	configFileEnv            = "AWS_CONFIG_FILE"
	sharedCredentialsFileEnv = "AWS_SHARED_CREDENTIALS_FILE"
	disableIMDSProviderEnv   = "AWS_EC2_METADATA_DISABLED"
	regionEnv                = "AWS_REGION"
	defaultRegionEnv         = "AWS_DEFAULT_REGION"
	stsEndpointsEnv          = "AWS_STS_REGIONAL_ENDPOINTS"
	MountS3PathEnv           = "MOUNT_S3_PATH"
	awsMaxAttemptsEnv        = "AWS_MAX_ATTEMPTS"
	defaultMountS3Path       = "/usr/bin/mount-s3"
	procMounts               = "/host/proc/mounts"
	userAgentPrefix          = "--user-agent-prefix"
	awsMaxAttemptsOption     = "--aws-max-attempts"
)

const (
	// Due to some reason that we haven't been able to identify, reading `/host/proc/mounts`
	// fails on newly spawned Karpenter/GPU nodes with "invalid argument".
	// It's reported that reading `/host/proc/mounts` works after some retries,
	// and we decided to add retry mechanism until we find and fix the root cause of this problem.
	// See https://github.com/awslabs/mountpoint-s3-csi-driver/issues/174.
	procMountsReadMaxRetry     = 3
	procMountsReadRetryBackoff = 100 * time.Millisecond
)

type MountCredentials struct {
	// -- Env variable provider
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string

	// -- Profile provider
	ConfigFilePath            string
	SharedCredentialsFilePath string

	// -- STS provider
	WebTokenPath string
	AwsRoleArn   string

	// -- IMDS provider
	DisableIMDSProvider bool

	// -- Generic
	Region        string
	DefaultRegion string
	StsEndpoints  string
}

// Get environment variables to pass to mount-s3 for authentication.
func (mc *MountCredentials) Env() []string {
	env := []string{}

	// For env variable provider
	if mc.AccessKeyID != "" && mc.SecretAccessKey != "" {
		env = append(env, keyIdEnv+"="+mc.AccessKeyID)
		env = append(env, accessKeyEnv+"="+mc.SecretAccessKey)
		if mc.SessionToken != "" {
			env = append(env, sessionTokenEnv+"="+mc.SessionToken)
		}
	}

	// For profile provider
	if mc.ConfigFilePath != "" {
		env = append(env, configFileEnv+"="+mc.ConfigFilePath)
	}
	if mc.SharedCredentialsFilePath != "" {
		env = append(env, sharedCredentialsFileEnv+"="+mc.SharedCredentialsFilePath)
	}

	// For STS Web Identity provider
	if mc.WebTokenPath != "" {
		env = append(env, webIdentityTokenEnv+"="+mc.WebTokenPath)
		env = append(env, roleArnEnv+"="+mc.AwsRoleArn)
	}

	// For disabling IMDS provider
	if mc.DisableIMDSProvider {
		env = append(env, disableIMDSProviderEnv+"=true")
	}

	// Generic variables
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
	Ctx               context.Context
	Runner            ServiceRunner
	Fs                Fs
	MountLister       MountLister
	MpVersion         string
	MountS3Path       string
	kubernetesVersion string
}

func MountS3Path() string {
	mountS3Path := os.Getenv(MountS3PathEnv)
	if mountS3Path == "" {
		mountS3Path = defaultMountS3Path
	}
	return mountS3Path
}

func NewS3Mounter(mpVersion string, kubernetesVersion string) (*S3Mounter, error) {
	ctx := context.Background()
	runner, err := system.StartOsSystemdSupervisor()
	if err != nil {
		return nil, fmt.Errorf("failed to start systemd supervisor: %w", err)
	}
	return &S3Mounter{
		Ctx:               ctx,
		Runner:            runner,
		Fs:                &OsFs{},
		MountLister:       &ProcMountLister{ProcMountPath: procMounts},
		MpVersion:         mpVersion,
		MountS3Path:       MountS3Path(),
		kubernetesVersion: kubernetesVersion,
	}, nil
}

func (pml *ProcMountLister) ListMounts() ([]mount.MountPoint, error) {
	var (
		mounts []byte
		err    error
	)

	for i := 1; i <= procMountsReadMaxRetry; i += 1 {
		mounts, err = os.ReadFile(pml.ProcMountPath)
		if err == nil {
			if i > 1 {
				klog.V(4).Infof("Successfully read %s after %d retries", pml.ProcMountPath, i)
			}
			break
		}

		klog.Errorf("Failed to read %s on try %d: %v", pml.ProcMountPath, i, err)
		time.Sleep(procMountsReadRetryBackoff)
	}

	if err != nil {
		return nil, fmt.Errorf("Failed to read %s after %d tries: %w", pml.ProcMountPath, procMountsReadMaxRetry, err)
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
	options, env = moveOptionToEnvironmentVariables(awsMaxAttemptsOption, awsMaxAttemptsEnv, options, env)
	options = addUserAgentToOptions(options, UserAgent(m.kubernetesVersion))

	klog.V(4).Infof("FIXME: MP env (with credentials): %v", env)

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
