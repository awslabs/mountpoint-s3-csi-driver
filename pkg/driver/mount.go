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

	systemd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/google/uuid"
	"k8s.io/klog/v2"
	"k8s.io/mount-utils"
)

const (
	keyIdEnv           = "AWS_ACCESS_KEY_ID"
	accessKeyEnv       = "AWS_SECRET_ACCESS_KEY"
	MountS3PathEnv     = "MOUNT_S3_PATH"
	defaultMountS3Path = "/usr/bin/mount-s3"
	procMounts         = "/host/proc/mounts"
	userAgentPrefix    = "--user-agent-prefix"
	csiDriverPrefix    = "s3-csi-driver/"
)

// Mounter is an interface for mount operations
type Mounter interface {
	mount.Interface
	IsCorruptedMnt(err error) bool
	PathExists(path string) (bool, error)
	MakeDir(pathname string) error
}

type S3Mounter struct {
	mount.Interface
	ctx         context.Context
	runner      SystemdRunner
	connFactory func(context.Context) (SystemdConnection, error)
	mpVersion   string
	mountS3Path string
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
	connFactory := func(ctx context.Context) (SystemdConnection, error) {
		return systemd.NewSystemConnectionContext(ctx)
	}
	return &S3Mounter{
		Interface:   mount.New(""),
		ctx:         ctx,
		runner:      NewSystemdRunner(),
		connFactory: connFactory,
		mpVersion:   mpVersion,
		mountS3Path: MountS3Path(),
	}, nil
}

func (m *S3Mounter) MakeDir(pathname string) error {
	err := os.MkdirAll(pathname, os.FileMode(0755))
	if err != nil {
		if !os.IsExist(err) {
			return err
		}
	}
	return nil
}

// IsCorruptedMnt return true if err is about corrupted mount point
func (m *S3Mounter) IsCorruptedMnt(err error) bool {
	return mount.IsCorruptedMnt(err)
}

func (m *S3Mounter) List() ([]mount.MountPoint, error) {
	mounts, err := os.ReadFile(procMounts)
	if err != nil {
		return nil, fmt.Errorf("Failed to read %s: %w", procMounts, err)
	}
	return parseProcMounts(mounts)
}

func (m *S3Mounter) IsMountPoint(file string) (bool, error) {
	mountPoints, err := m.List()
	if err != nil {
		return false, fmt.Errorf("Failed to cat /proc/mounts: %w", err)
	}
	for _, mp := range mountPoints {
		if mp.Path == file {
			return true, nil
		}
	}
	return false, nil
}

func (m *S3Mounter) PathExists(path string) (bool, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, nil
}

func (m *S3Mounter) Mount(source string, target string, _ string, options []string) error {
	timeoutCtx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer cancel()
	env := accessKeysEnv()

	output, err := m.runner.Run(timeoutCtx, m.mountS3Path,
		m.mpVersion+"-"+uuid.New().String(), "forking", env,
		append([]string{source, target}, addUserAgentToOptions(options)...))
	if err != nil {
		return fmt.Errorf("Mount failed: %w output: %s", err, output)
	}
	if output != "" {
		klog.V(5).Infof("mount-s3 output: %s", output)
	}
	return nil
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
	timeoutCtx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer cancel()

	output, err := m.runner.Run(timeoutCtx, "/usr/bin/umount",
		m.mpVersion+"-"+uuid.New().String(), "oneshot", []string{}, []string{target})
	if err != nil {
		return fmt.Errorf("Unmount failed: %w unmount output: %s", err, output)
	}
	if output != "" {
		klog.V(5).Infof("umount output: %s", output)
	}
	return nil
}

func accessKeysEnv() []string {
	env := []string{}

	keyId := os.Getenv(keyIdEnv)
	accessKey := os.Getenv(accessKeyEnv)
	if keyId != "" && accessKey != "" {
		env = append(env, keyIdEnv+"="+keyId)
		env = append(env, accessKeyEnv+"="+accessKey)
	}
	return env
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
