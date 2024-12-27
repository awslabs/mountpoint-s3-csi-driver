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

package node

import (
	"context"
	"maps"
	"os"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
	"k8s.io/mount-utils"

	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/envprovider"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/mounter"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/regionprovider"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/volumecontext"
	"github.com/awslabs/aws-s3-csi-driver/pkg/mountpoint"
)

const (
	defaultKubeletPath = "/var/lib/kubelet"
)

var kubeletPath = getKubeletPath()

var (
	nodeCaps = []csi.NodeServiceCapability_RPC_Type{}
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

// S3NodeServer is the implementation of the csi.NodeServer interface
type S3NodeServer struct {
	nodeID             string
	mounter            mounter.Mounter
	credentialProvider *credentialprovider.Provider
	regionProvider     *regionprovider.Provider
	kubernetesVersion  string
}

func NewS3NodeServer(nodeID string, mounter mounter.Mounter, credentialProvider *credentialprovider.Provider, regionProvider *regionprovider.Provider, kubernetesVersion string) *S3NodeServer {
	return &S3NodeServer{nodeID: nodeID, mounter: mounter, credentialProvider: credentialProvider, regionProvider: regionProvider, kubernetesVersion: kubernetesVersion}
}

func (ns *S3NodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (ns *S3NodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (ns *S3NodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	klog.V(4).Infof("NodePublishVolume: new request: %+v", logSafeNodePublishVolumeRequest(req))

	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID not provided")
	}

	volumeCtx := req.GetVolumeContext()

	bucket, ok := volumeCtx[volumecontext.BucketName]
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "Bucket name not provided")
	}

	target := req.GetTargetPath()
	if len(target) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Target path not provided")
	}

	if !strings.HasPrefix(target, kubeletPath) {
		klog.Errorf("NodePublishVolume: target path %q is not in kubelet path %q. This might cause mounting issues, please ensure you have correct kubelet path configured.", target, kubeletPath)
	}

	volCap := req.GetVolumeCapability()
	if volCap == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capability not provided")
	}

	if !ns.isValidVolumeCapabilities([]*csi.VolumeCapability{volCap}) {
		return nil, status.Error(codes.InvalidArgument, "Volume capability not supported")
	}

	args := []string{}
	if req.GetReadonly() || volCap.GetAccessMode().GetMode() == csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY {
		args = append(args, mountpoint.ArgReadOnly)
	}

	if capMount := volCap.GetMount(); capMount != nil {
		mountFlags := capMount.GetMountFlags()
		args = append(args, mountFlags...)
	}

	mountpointArgs := mountpoint.ParseArgs(args)
	env := envprovider.Provide()

	mountpointArgs, env = ns.moveArgumentsToEnv(mountpointArgs, env)

	credentials, err := ns.credentialProvider.Provide(ctx, volumeCtx)
	if err != nil {
		klog.Errorf("NodePublishVolume: failed to provide credentials: %v", err)
		return nil, err
	}

	mountpointArgs = ns.addUserAgentToArguments(mountpointArgs, credentials)

	// We need to ensure we're using region for STS if Pod-level identity is used.
	if credentials.Source() == credentialprovider.AuthenticationSourcePod {
		env, err = ns.overrideRegionEnvFromSTSRegion(volumeCtx, mountpointArgs, env)
		if err != nil {
			return nil, err
		}
	}

	klog.V(4).Infof("NodePublishVolume: mounting %s at %s with options %v", bucket, target, mountpointArgs.SortedList())

	if err := ns.mounter.Mount(bucket, target, credentials, env, mountpointArgs); err != nil {
		os.Remove(target)
		return nil, status.Errorf(codes.Internal, "Could not mount %q at %q: %v", bucket, target, err)
	}
	klog.V(4).Infof("NodePublishVolume: %s was mounted", target)

	return &csi.NodePublishVolumeResponse{}, nil
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

	mounted, err := ns.mounter.IsMountPoint(target)
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
	err = ns.mounter.Unmount(target)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Could not unmount %q: %v", target, err)
	}

	klog.V(4).Infof("NodeUnpublishVolume: unmounted %s", target)

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
		NodeId: ns.nodeID,
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

// moveArgumentsToEnv moves `--aws-max-attempts` from arguments to environment variables if provided.
func (ns *S3NodeServer) moveArgumentsToEnv(args mountpoint.Args, env envprovider.Environment) (mountpoint.Args, envprovider.Environment) {
	if maxAttempts, ok := args.Remove(mountpoint.ArgAWSMaxAttempts); ok {
		env = append(env, envprovider.Format(envprovider.EnvMaxAttempts, maxAttempts))
	}

	return args, env
}

// addUserAgentToArguments adds user-agent to Mountpoint arguments.
func (ns *S3NodeServer) addUserAgentToArguments(args mountpoint.Args, credentials credentialprovider.Credentials) mountpoint.Args {
	// Remove existing user-agent if provided to ensure we always use the correct user-agent
	_, _ = args.Remove(mountpoint.ArgUserAgentPrefix)
	userAgent := mounter.UserAgent(credentials.Source(), ns.kubernetesVersion)
	args.Insert(envprovider.Format(mountpoint.ArgUserAgentPrefix, userAgent))

	return args
}

// overrideRegionEnvFromSTSRegion overrides provided region with the region configured for STS.
func (ns *S3NodeServer) overrideRegionEnvFromSTSRegion(volumeContext map[string]string, args mountpoint.Args, env envprovider.Environment) (envprovider.Environment, error) {
	env = envprovider.Remove(env, envprovider.EnvRegion)
	region, err := ns.regionProvider.SecurityTokenService(volumeContext, args)
	if err != nil {
		return env, err
	}
	env = append(env, envprovider.Format(envprovider.EnvRegion, region))
	return env, nil
}

// logSafeNodePublishVolumeRequest returns a copy of given `csi.NodePublishVolumeRequest`
// with sensitive fields removed.
func logSafeNodePublishVolumeRequest(req *csi.NodePublishVolumeRequest) *csi.NodePublishVolumeRequest {
	safeVolumeContext := maps.Clone(req.VolumeContext)
	delete(safeVolumeContext, volumecontext.CSIServiceAccountTokens)

	return &csi.NodePublishVolumeRequest{
		VolumeId:          req.VolumeId,
		PublishContext:    req.PublishContext,
		StagingTargetPath: req.StagingTargetPath,
		TargetPath:        req.TargetPath,
		VolumeCapability:  req.VolumeCapability,
		Readonly:          req.Readonly,
		VolumeContext:     safeVolumeContext,
	}
}

func getKubeletPath() string {
	kubeletPath := os.Getenv("KUBELET_PATH")
	if kubeletPath == "" {
		return defaultKubeletPath
	}
	return kubeletPath
}
