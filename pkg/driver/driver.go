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
	"net"
	"strings"
	"time"

	"github.com/awslabs/aws-s3-csi-driver/pkg/util"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"k8s.io/klog/v2"
)

const (
	driverName = "s3.csi.aws.com"
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

	NodeID  string
	Mounter Mounter
}

func NewDriver(endpoint string, mpVersion string, nodeID string) *Driver {
	klog.Infof("Driver version: %v, Git commit: %v, build date: %v, nodeID: %v", driverVersion, gitCommit, buildDate, nodeID)

	mounter, err := newS3Mounter(mpVersion)
	if err != nil {
		klog.Fatalln(err)
	}

	return &Driver{
		Endpoint: endpoint,
		NodeID:   nodeID,
		Mounter:  mounter,
	}
}

func (d *Driver) Run() error {
	printRunningMountpoints()

	scheme, addr, err := util.ParseEndpoint(d.Endpoint)
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
	csi.RegisterNodeServer(d.Srv, d)

	klog.Infof("Listening for connections on address: %#v", listener.Addr())
	return d.Srv.Serve(listener)
}

func (d *Driver) Stop() {
	klog.Infof("Stopping server")
	d.Srv.Stop()
}

func printRunningMountpoints() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	systemdConn, err := ConnectOsSystemd(ctx)
	if err != nil {
		klog.Infof("Could not connect to systemd: %v", err)
		return
	}
	statuses, err := systemdConn.ListUnitsContext(ctx)
	if err != nil {
		klog.Infof("Could not get systemd status: %v", err)
		return
	}

	for _, status := range statuses {
		if strings.Contains(status.Name, "mount-s3") {
			klog.Infof("Existing mount-s3 systemd service: %v", status)
		}
	}
}
