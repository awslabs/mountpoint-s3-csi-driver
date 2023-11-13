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
	keyIdEnv        = "AWS_ACCESS_KEY_ID"
	accessKeyEnv    = "AWS_SECRET_ACCESS_KEY"
	pluginDir       = "/var/lib/kubelet/plugins/s3.csi.aws.com"
	mountS3Path     = pluginDir + "/mountpoint-s3/bin/mount-s3"
	userAgentPrefix = "--user-agent-prefix"
	csiDriverPrefix = "s3-csi-driver/"
	keyIdEnv        = "AWS_ACCESS_KEY_ID"
	accessKeyEnv    = "AWS_SECRET_ACCESS_KEY"
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
}

func newS3Mounter(mpVersion string) (Mounter, error) {
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
	keyId := os.Getenv(keyIdEnv)
	accessKey := os.Getenv(accessKeyEnv)
	env := []string{}
	if keyId != "" && accessKey != "" {
		env = append(env, keyIdEnv+"="+keyId)
		env = append(env, accessKeyEnv+"="+accessKey)
	}

	output, err := m.runner.Run(timeoutCtx, mountS3Path, m.mpVersion+"-"+uuid.New().String(),
		env, append([]string{source, target}, addUserAgentToOptions(options)...))
	if err != nil {
		return fmt.Errorf("Mount failed: %w mount-s3 output: %s", err, output)
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
	return append(options, userAgentPrefix+" "+csiDriverPrefix+GetVersion().DriverVersion)
}
