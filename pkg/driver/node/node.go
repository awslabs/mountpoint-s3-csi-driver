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
	"strconv"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
	"k8s.io/mount-utils"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/mounter"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/targetpath"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/volumecontext"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util"
)

var (
	nodeCaps = []csi.NodeServiceCapability_RPC_Type{
		csi.NodeServiceCapability_RPC_VOLUME_MOUNT_GROUP,
	}
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

const (
	filePerm770 = "770" // User: full access, Group: full access, Others: none
	filePerm660 = "660" // User: read/write, Group: read/write, Others: none
)

// S3NodeServer is the implementation of the csi.NodeServer interface
type S3NodeServer struct {
	NodeID  string
	Mounter mounter.Mounter
}

func NewS3NodeServer(nodeID string, mounter mounter.Mounter) *S3NodeServer {
	return &S3NodeServer{NodeID: nodeID, Mounter: mounter}
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

	targetHost := req.GetTargetPath()
	if len(targetHost) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Target path not provided")
	}

	// Translate target path from host format to container format if needed.
	// Example error: Failed to translate target path "/opt/kubernetes/kubelet/pods/abcd-1234/volumes/kubernetes.io~csi/s3-pv/mount":
	//   path "/opt/kubernetes/kubelet/pods/abcd-1234/volumes/kubernetes.io~csi/s3-pv/mount" does not start with host kubelet path "/var/lib/kubelet"
	targetContainer, err := util.KubeletHostPathToContainerPath(targetHost)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "Failed to translate target path %q: %v", targetHost, err)
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
		mountpointArgs = append(mountpointArgs, mountpoint.ArgReadOnly)
	}

	if capMount := volCap.GetMount(); capMount != nil {
		mountFlags := capMount.GetMountFlags()
		mountpointArgs = append(mountpointArgs, mountFlags...)
	}

	args := mountpoint.ParseArgs(mountpointArgs)

	if args.Has(mountpoint.ArgFsTab) {
		return nil, status.Error(codes.InvalidArgument, "Running mount-s3 with mount flag -o is not supported in CSI Driver.")
	}

	fsGroup := ""
	if capMount := volCap.GetMount(); capMount != nil {
		if volumeMountGroup := capMount.GetVolumeMountGroup(); volumeMountGroup != "" {
			fsGroup = volumeMountGroup
			// We need to add the following flags to support fsGroup
			// If these flags were already set by customer in PV mountOptions then we won't override them
			args.SetIfAbsent(mountpoint.ArgGid, volumeMountGroup)
			args.SetIfAbsent(mountpoint.ArgAllowOther, mountpoint.ArgNoValue)
			args.SetIfAbsent(mountpoint.ArgDirMode, filePerm770)
			args.SetIfAbsent(mountpoint.ArgFileMode, filePerm660)
		}
	}

	if !args.Has(mountpoint.ArgAllowOther) {
		// If customer container is running as root we need to add --allow-root as Mountpoint Pod is not run as root
		args.SetIfAbsent(mountpoint.ArgAllowRoot, mountpoint.ArgNoValue)
	}

	// If cacheEmptyDirSizeLimit is set with cache=emptyDir, validate that an explicit --max-cache-size (in MiB) doesn't exceed it.
	if emptyDirSizeLimit := volumeCtx[volumecontext.CacheEmptyDirSizeLimit]; emptyDirSizeLimit != "" && volumeCtx[volumecontext.Cache] == volumecontext.CacheTypeEmptyDir {
		quantity, err := resource.ParseQuantity(emptyDirSizeLimit)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "Invalid %s %q: %v", volumecontext.CacheEmptyDirSizeLimit, emptyDirSizeLimit, err)
		}
		emptyDirSizeLimitMiB := quantity.Value() / (1024 * 1024)

		// Safety factor to account for Mountpoint overshooting its cache target by around 1-2%.
		const safetyFactor = 0.95
		safeMaxCacheSizeMiB := int64(float64(quantity.Value()) * safetyFactor / (1024 * 1024))

		if maxCacheSize, ok := args.Value(mountpoint.ArgMaxCacheSize); ok {
			maxCacheSizeMiB, err := strconv.ParseInt(maxCacheSize, 10, 64)
			if err != nil {
				return nil, status.Errorf(codes.InvalidArgument, "Invalid %s %q: %v", mountpoint.ArgMaxCacheSize, maxCacheSize, err)
			}
			if maxCacheSizeMiB > emptyDirSizeLimitMiB {
				return nil, status.Errorf(codes.InvalidArgument,
					"%s (%d MiB) exceeds %s (%s = %d MiB). Reduce %s or increase %s.",
					mountpoint.ArgMaxCacheSize, maxCacheSizeMiB,
					volumecontext.CacheEmptyDirSizeLimit, emptyDirSizeLimit, emptyDirSizeLimitMiB,
					mountpoint.ArgMaxCacheSize, volumecontext.CacheEmptyDirSizeLimit)
			}
			// For disk-backed (default) medium, remove user's explicit --max-cache-size if it exceeds
			// the safe threshold, allowing the safe default to be injected below via SetIfAbsent.
			if volumeCtx[volumecontext.CacheEmptyDirMedium] == string(corev1.StorageMediumDefault) && maxCacheSizeMiB > safeMaxCacheSizeMiB {
				args.Remove(mountpoint.ArgMaxCacheSize)
			}
		}

		// For disk-backed (default) medium, statvfs on the cache directory reports the node's root filesystem
		// stats rather than the emptyDir's sizeLimit, so Mountpoint cannot self-limit correctly.
		// Inject --max-cache-size at 95% of the limit to ensure Mountpoint evicts before Kubernetes does.
		// The 5% margin accounts for Mountpoint overshooting its target by around 1-2%.
		// Memory medium has an isolated filesystems with accurate size reporting, so Mountpoint
		// can self-limit without this injection.
		if volumeCtx[volumecontext.CacheEmptyDirMedium] == string(corev1.StorageMediumDefault) {
			args.SetIfAbsent(mountpoint.ArgMaxCacheSize, strconv.FormatInt(safeMaxCacheSizeMiB, 10))
		}
	}

	klog.V(4).Infof("NodePublishVolume: mounting %s at %s with options %v", bucket, targetContainer, args.SortedList())

	credentialCtx := credentialProvideContextFromPublishRequest(req, args)

	if err := ns.Mounter.Mount(ctx, bucket, targetContainer, credentialCtx, args, fsGroup); err != nil {
		os.Remove(targetContainer)
		return nil, status.Errorf(codes.Internal, "Could not mount %q at %q: %v", bucket, targetContainer, err)
	}
	klog.V(4).Infof("NodePublishVolume: %s was mounted", targetContainer)

	return &csi.NodePublishVolumeResponse{}, nil
}

func (ns *S3NodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	klog.V(4).Infof("NodeUnpublishVolume: called with args %+v", req)

	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID not provided")
	}

	targetHost := req.GetTargetPath()
	if len(targetHost) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Target path not provided")
	}

	targetContainer, err := util.KubeletHostPathToContainerPath(targetHost)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "Failed to translate target path %q: %v", targetHost, err)
	}

	mounted, err := ns.Mounter.IsMountPoint(targetContainer)
	if err != nil && os.IsNotExist(err) {
		klog.V(4).Infof("NodeUnpublishVolume: target path %s does not exist, skipping unmount", targetContainer)
		return &csi.NodeUnpublishVolumeResponse{}, nil
	} else if err != nil && mount.IsCorruptedMnt(err) {
		klog.V(4).Infof("NodeUnpublishVolume: target path %s is corrupted: %v, will try to unmount", targetContainer, err)
		mounted = true
	} else if err != nil {
		return nil, status.Errorf(codes.Internal, "Could not unmount %q: %v", targetContainer, err)
	}
	if !mounted {
		klog.V(4).Infof("NodeUnpublishVolume: target path %s not mounted, skipping unmount", targetContainer)
		return &csi.NodeUnpublishVolumeResponse{}, nil
	}

	credentialCtx := credentialCleanupContextFromUnpublishRequest(req)

	klog.V(4).Infof("NodeUnpublishVolume: unmounting %s", targetContainer)
	err = ns.Mounter.Unmount(ctx, targetContainer, credentialCtx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Could not unmount %q: %v", targetContainer, err)
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

func credentialProvideContextFromPublishRequest(req *csi.NodePublishVolumeRequest, args mountpoint.Args) credentialprovider.ProvideContext {
	volumeCtx := req.GetVolumeContext()

	podID := volumeCtx[volumecontext.CSIPodUID]
	if podID == "" {
		podID, _ = podIDFromTargetPath(req.GetTargetPath())
	}

	authSource := credentialprovider.AuthenticationSourceDriver
	if volumeCtx[volumecontext.AuthenticationSource] != credentialprovider.AuthenticationSourceUnspecified {
		authSource = volumeCtx[volumecontext.AuthenticationSource]
	}

	bucketRegion, _ := args.Value(mountpoint.ArgRegion)

	return credentialprovider.ProvideContext{
		WorkloadPodID:        podID,
		VolumeID:             req.GetVolumeId(),
		AuthenticationSource: authSource,
		PodNamespace:         volumeCtx[volumecontext.CSIPodNamespace],
		ServiceAccountTokens: serviceAccountTokensFromRequest(req),
		ServiceAccountName:   volumeCtx[volumecontext.CSIServiceAccountName],
		StsRegion:            volumeCtx[volumecontext.STSRegion],
		BucketRegion:         bucketRegion,
	}
}

// serviceAccountTokensFromRequest checks secrets first, then volume context.
// In Kubernetes v1.35+, tokens can be delivered via the secrets field (KEP-5538).
// We don't set serviceAccountTokenInSecrets in our CSIDriver spec yet, but this
// fallback ensures we're ready when we do.
func serviceAccountTokensFromRequest(req *csi.NodePublishVolumeRequest) string {
	if tokens, ok := req.GetSecrets()[volumecontext.CSIServiceAccountTokens]; ok {
		return tokens
	}
	return req.GetVolumeContext()[volumecontext.CSIServiceAccountTokens]
}

func credentialCleanupContextFromUnpublishRequest(req *csi.NodeUnpublishVolumeRequest) credentialprovider.CleanupContext {
	podID, _ := podIDFromTargetPath(req.GetTargetPath())
	return credentialprovider.CleanupContext{
		VolumeID: req.GetVolumeId(),
		PodID:    podID,
	}
}

func podIDFromTargetPath(target string) (string, bool) {
	targetPath, err := targetpath.Parse(target)
	if err != nil {
		klog.V(4).Infof("Failed to parse target path %s: %v", target, err)
		return "", false
	}
	return targetPath.PodID, true
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
