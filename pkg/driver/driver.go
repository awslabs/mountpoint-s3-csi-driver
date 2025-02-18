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
	"net"
	"os"

	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/mounter"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/version"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"k8s.io/mount-utils"
)

const (
	driverName = "s3.csi.aws.com"

	grpcServerMaxReceiveMessageSize = 1024 * 1024 * 2 // 2MB

	unixSocketPerm = os.FileMode(0700) // only owner can write and read.
)

var usePodMounter = os.Getenv("MOUNTER_KIND") == "pod"
var mountpointPodNamespace = os.Getenv("MOUNTPOINT_NAMESPACE")

type Driver struct {
	Endpoint string
	Srv      *grpc.Server
	NodeID   string

	NodeServer *node.S3NodeServer
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

	version := version.GetVersion()
	klog.Infof("Driver version: %v, Git commit: %v, build date: %v, nodeID: %v, mount-s3 version: %v, kubernetes version: %v",
		version.DriverVersion, version.GitCommit, version.BuildDate, nodeID, mpVersion, kubernetesVersion)

	// `credentialprovider.RegionFromIMDSOnce` is a `sync.OnceValues` and it only makes request to IMDS once,
	// this call is basically here to pre-warm the cache of IMDS call.
	go func() {
		_, _ = credentialprovider.RegionFromIMDSOnce()
	}()

	credProvider := credentialprovider.New(clientset.CoreV1(), credentialprovider.RegionFromIMDSOnce)

	var mounterImpl mounter.Mounter
	if usePodMounter {
		mounterImpl, err = mounter.NewPodMounter(clientset.CoreV1(), credProvider, mountpointPodNamespace, mount.New(""), nil, kubernetesVersion)
		if err != nil {
			klog.Fatalln(err)
		}
		klog.Infoln("Using pod mounter")
	} else {
		mounterImpl, err = mounter.NewSystemdMounter(credProvider, mpVersion, kubernetesVersion)
		if err != nil {
			klog.Fatalln(err)
		}
		klog.Infoln("Using systemd mounter")
	}

	nodeServer := node.NewS3NodeServer(nodeID, mounterImpl)

	return &Driver{
		Endpoint:   endpoint,
		NodeID:     nodeID,
		NodeServer: nodeServer,
	}, nil
}

func (d *Driver) Run() error {
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

func kubernetesVersion(clientset *kubernetes.Clientset) (string, error) {
	version, err := clientset.ServerVersion()
	if err != nil {
		return "", fmt.Errorf("cannot get kubernetes server version: %w", err)
	}

	return version.String(), nil
}
