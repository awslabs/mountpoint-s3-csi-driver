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

func NewDriver(endpoint string, mpVersion string, nodeID string) *Driver {
	klog.Infof("Driver version: %v, Git commit: %v, build date: %v, nodeID: %v, mount-s3 version: %v",
		driverVersion, gitCommit, buildDate, nodeID, mpVersion)

	mounter, err := NewS3Mounter(mpVersion)
	if err != nil {
		klog.Fatalln(err)
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatal(err)
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		klog.Fatal(err)
	}

	return &Driver{
		Endpoint:   endpoint,
		NodeID:     nodeID,
		NodeServer: &S3NodeServer{NodeID: nodeID, Mounter: mounter, K8sClient: clientset.CoreV1()},
	}
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

	logErr := func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		resp, err := handler(ctx, req)
		if err != nil {
			klog.Errorf("GRPC error: %v", err)
		}
		return resp, err
	}
	opts := []grpc.ServerOption{
		grpc.UnaryInterceptor(logErr),
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
