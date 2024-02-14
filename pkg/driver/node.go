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
	"os"
	"path"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"k8s.io/mount-utils"
)

const (
	hostTokenDirEnv = "HOST_TOKEN_DIR"
	fstype          = "unused"
	bucketName      = "bucketName"
)

var (
	nodeCaps = []csi.NodeServiceCapability_RPC_Type{}
)

// S3NodeServer is the implementation of the csi.NodeServer interface
type S3NodeServer struct {
	NodeID          string
	BaseCredentials *MountCredentials
	Mounter         Mounter
}

type Token struct {
	Token               string    `json:"token"`
	ExpirationTimestamp time.Time `json:"expirationTimestamp"`
}

func (ns *S3NodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (ns *S3NodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (ns *S3NodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	klog.V(4).Infof("NodePublishVolume: req: %+v", req)

	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID not provided")
	}

	bucket, ok := req.GetVolumeContext()[bucketName]
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "Bucket name not provided")
	}

	target := req.GetTargetPath()
	if len(target) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Target path not provided")
	}

	volCap := req.GetVolumeCapability()
	if volCap == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capability not provided")
	}

	if !ns.isValidVolumeCapabilities([]*csi.VolumeCapability{volCap}) {
		return nil, status.Error(codes.InvalidArgument, "Volume capability not supported")
	}

	mountpointArgs := []string{}
	if req.GetReadonly() || volCap.GetAccessMode().GetMode() == csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY {
		mountpointArgs = append(mountpointArgs, "--read-only")
	}

	// get the mount(point) options (yaml list)
	if capMount := volCap.GetMount(); capMount != nil {
		mountFlags := capMount.GetMountFlags()
		for i := range mountFlags {
			// trim left and right spaces
			// trim spaces in between from multiple spaces to just one i.e. uid   1001 would turn into uid 1001
			// if there is a space between, replace it with an = sign
			mountFlags[i] = strings.Replace(strings.Join(strings.Fields(strings.Trim(mountFlags[i], " ")), " "), " ", "=", -1)
			// prepend -- if it's not already there
			if !strings.HasPrefix(mountFlags[i], "-") {
				mountFlags[i] = "--" + mountFlags[i]
			}
		}
		mountpointArgs = compileMountOptions(mountpointArgs, mountFlags)
	}

	klog.V(4).Infof("NodePublishVolume: mounting %s at %s with options %v", bucket, target, mountpointArgs)
	hostTokenDir := os.Getenv(hostTokenDirEnv)
	if hostTokenDir == "" {
		// set the default in case the env variable isn't found
		hostTokenDir = "/var/lib/kubelet/plugins/s3.csi.aws.com/"
	}
	hostTokenPath := path.Join(hostTokenDir, "token")
	credentials := &MountCredentials{
		AccessKeyID:     os.Getenv(keyIdEnv),
		SecretAccessKey: os.Getenv(accessKeyEnv),
		Region:          os.Getenv(regionEnv),
		DefaultRegion:   os.Getenv(defaultRegionEnv),
		WebTokenPath:    hostTokenPath,
		StsEndpoints:    os.Getenv(stsEndpointsEnv),
		AwsRoleArn:      os.Getenv(roleArnEnv),
	}
	if err := ns.Mounter.Mount(bucket, target, credentials, mountpointArgs); err != nil {
		os.Remove(target)
		return nil, status.Errorf(codes.Internal, "Could not mount %q at %q: %v", bucket, target, err)
	}
	klog.V(4).Infof("NodePublishVolume: %s was mounted", target)

	return &csi.NodePublishVolumeResponse{}, nil
}

/**
 * Compile mounting options into a singular set
 */
func compileMountOptions(currentOptions []string, newOptions []string) []string {
	allMountOptions := sets.NewString()

	for _, currentMountOptions := range currentOptions {
		if len(currentMountOptions) > 0 {
			allMountOptions.Insert(currentMountOptions)
		}
	}

	for _, mountOption := range newOptions {
		// disallow options that don't make sense in CSI
		switch mountOption {
		case "--foreground", "-f", "--help", "-h", "--version", "-v":
			continue
		}
		allMountOptions.Insert(mountOption)
	}

	return allMountOptions.List()
}

func (ns *S3NodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	klog.V(4).Infof("NodeUnpublishVolume: called with args %+v", req)

	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID not provided")
	}
	target := req.GetTargetPath()
	if len(target) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Target path not provided")
	}

	mounted, err := ns.Mounter.IsMountPoint(target)
	if err != nil && os.IsNotExist(err) {
		klog.V(4).Infof("NodeUnpublishVolume: target path %s does not exist, skipping unmount", target)
		return &csi.NodeUnpublishVolumeResponse{}, nil
	} else if err != nil && mount.IsCorruptedMnt(err) {
		klog.V(4).Infof("NodeUnpublishVolume: target path %s is corrupted: %v, will try to unmount", target, err)
		mounted = true
	} else if err != nil {
		return nil, status.Errorf(codes.Internal, "Could not unmount %q: %v", target, err)
	}
	if !mounted {
		klog.V(4).Infof("NodeUnpublishVolume: target path %s not mounted, skipping unmount", target)
		return &csi.NodeUnpublishVolumeResponse{}, nil
	}

	klog.V(4).Infof("NodeUnpublishVolume: unmounting %s", target)
	err = ns.Mounter.Unmount(target)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Could not unmount %q: %v", target, err)
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (ns *S3NodeServer) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (ns *S3NodeServer) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (ns *S3NodeServer) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	klog.V(4).Infof("NodeGetCapabilities: called with args %+v", req)
	var caps []*csi.NodeServiceCapability
	for _, cap := range nodeCaps {
		c := &csi.NodeServiceCapability{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{
					Type: cap,
				},
			},
		}
		caps = append(caps, c)
	}
	return &csi.NodeGetCapabilitiesResponse{Capabilities: caps}, nil
}

func (ns *S3NodeServer) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	klog.V(4).Infof("NodeGetInfo: called with args %+v", req)

	return &csi.NodeGetInfoResponse{
		NodeId: ns.NodeID,
	}, nil
}

func (ns *S3NodeServer) isValidVolumeCapabilities(volCaps []*csi.VolumeCapability) bool {
	hasSupport := func(cap *csi.VolumeCapability) bool {
		for _, c := range volumeCaps {
			if c.GetMode() == cap.AccessMode.GetMode() {
				return true
			}
		}
		return false
	}

	foundAll := true
	for _, c := range volCaps {
		if !hasSupport(c) {
			foundAll = false
		}
	}
	return foundAll
}
