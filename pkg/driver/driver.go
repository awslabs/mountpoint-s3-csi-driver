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
	"io"
	"io/fs"
	"net"
	"os"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

const (
	driverName          = "s3.csi.aws.com"
	webIdentityTokenEnv = "AWS_WEB_IDENTITY_TOKEN_FILE"
	roleArnEnv          = "AWS_ROLE_ARN"

	grpcServerMaxReceiveMessageSize = 1024 * 1024 * 2 // 2MB

	unixSocketPerm = os.FileMode(0700) // only owner can write and read.
)

var (
	volumeCaps = []csi.VolumeCapability_AccessMode{
		{
			Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
		},
		{
			Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
		},
	}
)

type Driver struct {
	Endpoint string
	Srv      *grpc.Server
	NodeID   string

	NodeServer *S3NodeServer
}

func NewDriver(endpoint string, mpVersion string, nodeID string) (*Driver, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("cannot create in-cluster config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("cannot create kubernetes clientset: %w", err)
	}

	kubernetesVersion, err := kubernetesVersion(clientset)
	if err != nil {
		klog.Errorf("failed to get kubernetes version: %v", err)
	}

	klog.Infof("Driver version: %v, Git commit: %v, build date: %v, nodeID: %v, mount-s3 version: %v, kubernetes version: %v",
		driverVersion, gitCommit, buildDate, nodeID, mpVersion, kubernetesVersion)

	mounter, err := NewS3Mounter(mpVersion, kubernetesVersion)
	if err != nil {
		klog.Fatalln(err)
	}

	credentialProvider := NewCredentialProvider(clientset.CoreV1(), containerPluginDir, RegionFromIMDSOnce)
	nodeServer := NewS3NodeServer(nodeID, mounter, credentialProvider)

	return &Driver{
		Endpoint:   endpoint,
		NodeID:     nodeID,
		NodeServer: nodeServer,
	}, nil
}

func (d *Driver) Run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tokenFile := os.Getenv(webIdentityTokenEnv)
	if tokenFile != "" {
		klog.Infof("Found AWS_WEB_IDENTITY_TOKEN_FILE, syncing token")
		go tokenFileTender(ctx, tokenFile, "/csi/token")
	}

	scheme, addr, err := ParseEndpoint(d.Endpoint)
	if err != nil {
		return err
	}

	listener, err := net.Listen(scheme, addr)
	if err != nil {
		return err
	}

	if scheme == "unix" {
		// Go's `net` package does not support specifying permissions on Unix sockets it creates.
		// There are two ways to change permissions:
		// 	 - Using `syscall.Umask` before `net.Listen`
		//   - Calling `os.Chmod` after `net.Listen`
		// The first one is not nice because it affects all files created in the process,
		// the second one has a time-window where the permissions of Unix socket would depend on `umask`
		// between `net.Listen` and `os.Chmod`. Since we don't start accepting connections on the socket until
		// `grpc.Serve` call, we should be fine with `os.Chmod` option.
		// See https://github.com/golang/go/issues/11822#issuecomment-123850227.
		if err := os.Chmod(addr, unixSocketPerm); err != nil {
			klog.Errorf("Failed to change permissions on unix socket %s: %v", addr, err)
			return fmt.Errorf("Failed to change permissions on unix socket %s: %v", addr, err)
		}
	}

	logErr := func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		resp, err := handler(ctx, req)
		if err != nil {
			klog.Errorf("GRPC error: %v", err)
		}
		return resp, err
	}
	opts := []grpc.ServerOption{
		grpc.UnaryInterceptor(logErr),
		grpc.MaxRecvMsgSize(grpcServerMaxReceiveMessageSize),
	}
	d.Srv = grpc.NewServer(opts...)

	csi.RegisterIdentityServer(d.Srv, d)
	csi.RegisterControllerServer(d.Srv, d)
	csi.RegisterNodeServer(d.Srv, d.NodeServer)

	klog.Infof("Listening for connections on address: %#v", listener.Addr())
	return d.Srv.Serve(listener)
}

func (d *Driver) Stop() {
	klog.Infof("Stopping server")
	d.Srv.Stop()
}

func tokenFileTender(ctx context.Context, sourcePath string, destPath string) {
	for {
		timer := time.After(10 * time.Second)
		err := ReplaceFile(destPath, sourcePath, 0600)
		if err != nil {
			klog.Infof("Failed to sync AWS web token file: %v", err)
		}
		select {
		case <-timer:
			continue
		case <-ctx.Done():
			return
		}
	}
}

// replaceFile safely replaces a file with a new file by copying to a temporary location first
// then renaming.
func ReplaceFile(destPath string, sourcePath string, perm fs.FileMode) error {
	tmpDest := destPath + ".tmp"

	sourceFile, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	destFile, err := os.OpenFile(tmpDest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer destFile.Close()

	buf := make([]byte, 64*1024)
	_, err = io.CopyBuffer(destFile, sourceFile, buf)
	if err != nil {
		return err
	}

	err = os.Rename(tmpDest, destPath)
	if err != nil {
		return fmt.Errorf("Failed to rename file %s: %w", destPath, err)
	}

	return nil
}

func kubernetesVersion(clientset *kubernetes.Clientset) (string, error) {
	version, err := clientset.ServerVersion()
	if err != nil {
		return "", fmt.Errorf("cannot get kubernetes server version: %w", err)
	}

	return version.String(), nil
}
