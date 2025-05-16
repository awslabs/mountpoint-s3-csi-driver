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

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

// BucketNameParameter is the parameter name for specifying the bucket in StorageClass
const BucketNameParameter = "bucketName"

func (d *Driver) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	klog.V(4).Infof("CreateVolume: called with args %#v", req)

	// Check if request contains required parameters
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume name not provided")
	}

	// Validate volume capabilities
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume capabilities not provided")
	}

	// Verify the requested volume capabilities are supported
	supportedCaps := []csi.VolumeCapability_AccessMode_Mode{
		csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER, // ReadWriteMany
		csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,  // ReadOnlyMany
	}

	for _, cap := range req.GetVolumeCapabilities() {
		found := false
		for _, supportedCap := range supportedCaps {
			if cap.GetAccessMode().GetMode() == supportedCap {
				found = true
				break
			}
		}
		if !found {
			return nil, status.Errorf(codes.InvalidArgument, "Access mode %v not supported", cap.GetAccessMode().GetMode())
		}
	}

	// Get parameters from StorageClass
	params := req.GetParameters()

	// Check for bucket name in the parameters
	bucketName, exists := params[BucketNameParameter]
	if !exists {
		return nil, status.Errorf(codes.InvalidArgument, "%s not specified in StorageClass parameters", BucketNameParameter)
	}

	if bucketName == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Empty %s value provided in StorageClass parameters", BucketNameParameter)
	}

	// Extract namespace from the request
	var namespace string
	if req.GetParameters()["csi.storage.k8s.io/pvc/namespace"] != "" {
		namespace = req.GetParameters()["csi.storage.k8s.io/pvc/namespace"]
	} else {
		// Fallback to default namespace if not provided
		namespace = "default"
	}

	// Generate a unique subdirectory based on namespace and PVC name to support StatefulSets
	// Format: namespace_pvcName
	pvcName := req.GetName()
	subDir := fmt.Sprintf("%s_%s", namespace, pvcName)

	klog.V(4).Infof("CreateVolume: using bucket %s with subdirectory %s for volume %s", bucketName, subDir, req.Name)

	// Create the volume context with the bucket name and subdirectory
	volumeContext := map[string]string{
		"bucketName": bucketName,
		"subDir":     subDir,
	}

	// Construct and return response
	volume := &csi.Volume{
		VolumeId:      req.Name,
		CapacityBytes: req.GetCapacityRange().GetRequiredBytes(),
		VolumeContext: volumeContext,
	}

	return &csi.CreateVolumeResponse{
		Volume: volume,
	}, nil
}

func (d *Driver) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	klog.V(4).Infof("DeleteVolume: called with args: %#v", req)

	// For S3 CSI driver, we don't delete the actual S3 bucket
	// We just acknowledge the volume deletion request

	return &csi.DeleteVolumeResponse{}, nil
}

func (d *Driver) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (d *Driver) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (d *Driver) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	klog.V(4).Infof("ControllerGetCapabilities: called with args %#v", req)
	caps := []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
	}
	var capsResponse []*csi.ControllerServiceCapability
	for _, cap := range caps {
		c := &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: cap,
				},
			},
		}
		capsResponse = append(capsResponse, c)
	}
	return &csi.ControllerGetCapabilitiesResponse{Capabilities: capsResponse}, nil
}

func (d *Driver) GetCapacity(ctx context.Context, req *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	klog.V(4).Infof("GetCapacity: called with args %#v", req)
	return nil, status.Error(codes.Unimplemented, "")
}

func (d *Driver) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	klog.V(4).Infof("ListVolumes: called with args %#v", req)
	return nil, status.Error(codes.Unimplemented, "")
}

func (d *Driver) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	klog.V(4).Infof("ValidateVolumeCapabilities: called with args %#v", req)

	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID is required")
	}

	if req.VolumeCapabilities == nil || len(req.VolumeCapabilities) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume capabilities are required")
	}

	// Check if the volume exists (for dynamic provisioning, we don't actually check S3 bucket existence)
	// We just validate if the capabilities requested are supported

	supportedCaps := []csi.VolumeCapability_AccessMode_Mode{
		csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER, // ReadWriteMany
		csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,  // ReadOnlyMany
	}

	for _, cap := range req.VolumeCapabilities {
		found := false
		for _, supportedCap := range supportedCaps {
			if cap.GetAccessMode().GetMode() == supportedCap {
				found = true
				break
			}
		}
		if !found {
			return &csi.ValidateVolumeCapabilitiesResponse{
				Confirmed: nil,
				Message:   "Access mode not supported",
			}, nil
		}
	}

	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: req.VolumeCapabilities,
		},
	}, nil
}

func (d *Driver) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (d *Driver) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (d *Driver) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (d *Driver) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (d *Driver) ControllerGetVolume(ctx context.Context, req *csi.ControllerGetVolumeRequest) (*csi.ControllerGetVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (d *Driver) ControllerModifyVolume(context.Context, *csi.ControllerModifyVolumeRequest) (*csi.ControllerModifyVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}
