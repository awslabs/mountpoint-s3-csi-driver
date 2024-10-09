package node_test

import (
	"errors"
	"io/fs"
	"testing"

	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node"
	mock_driver "github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/mocks"
	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/golang/mock/gomock"
	"golang.org/x/net/context"
)

type nodeServerTestEnv struct {
	mockCtl     *gomock.Controller
	mockMounter *mock_driver.MockMounter
	server      *node.S3NodeServer
}

func initNodeServerTestEnv(t *testing.T) *nodeServerTestEnv {
	mockCtl := gomock.NewController(t)
	defer mockCtl.Finish()
	mockMounter := mock_driver.NewMockMounter(mockCtl)
	credentialProvider := node.NewCredentialProvider(nil, t.TempDir(), node.RegionFromIMDSOnce)
	server := node.NewS3NodeServer(
		"test-nodeID",
		mockMounter,
		credentialProvider,
	)
	return &nodeServerTestEnv{
		mockCtl:     mockCtl,
		mockMounter: mockMounter,
		server:      server,
	}
}

func TestNodePublishVolume(t *testing.T) {
	var (
		volumeId   = "test-volume-id"
		bucketName = "test-bucket-name"
		stdVolCap  = &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{},
			},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
			},
		}
		targetPath = "/target/path"
	)
	testCases := []struct {
		name     string
		testFunc func(t *testing.T)
	}{
		{
			name: "success: normal mount",
			testFunc: func(t *testing.T) {
				nodeTestEnv := initNodeServerTestEnv(t)
				ctx := context.Background()
				req := &csi.NodePublishVolumeRequest{
					VolumeId:         volumeId,
					VolumeCapability: stdVolCap,
					TargetPath:       targetPath,
					VolumeContext:    map[string]string{"bucketName": bucketName},
				}

				nodeTestEnv.mockMounter.EXPECT().Mount(gomock.Eq(bucketName), gomock.Eq(targetPath), gomock.Any(), gomock.Any())
				_, err := nodeTestEnv.server.NodePublishVolume(ctx, req)
				if err != nil {
					t.Fatalf("NodePublishVolume is failed: %v", err)
				}

				nodeTestEnv.mockCtl.Finish()
			},
		},
		{
			name: "success: reader only volume access type",
			testFunc: func(t *testing.T) {
				nodeTestEnv := initNodeServerTestEnv(t)
				ctx := context.Background()
				req := &csi.NodePublishVolumeRequest{
					VolumeId: volumeId,
					VolumeCapability: &csi.VolumeCapability{
						AccessType: &csi.VolumeCapability_Mount{
							Mount: &csi.VolumeCapability_MountVolume{},
						},
						AccessMode: &csi.VolumeCapability_AccessMode{
							Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
						},
					},
					TargetPath:    targetPath,
					VolumeContext: map[string]string{"bucketName": bucketName},
				}

				nodeTestEnv.mockMounter.EXPECT().Mount(gomock.Eq(bucketName), gomock.Eq(targetPath), gomock.Any(), gomock.Eq([]string{"--read-only"}))
				_, err := nodeTestEnv.server.NodePublishVolume(ctx, req)
				if err != nil {
					t.Fatalf("NodePublishVolume is failed: %v", err)
				}

				nodeTestEnv.mockCtl.Finish()
			},
		},
		{
			name: "success: mount with mount options and read only",
			testFunc: func(t *testing.T) {
				nodeTestEnv := initNodeServerTestEnv(t)
				ctx := context.Background()
				req := &csi.NodePublishVolumeRequest{
					VolumeId: volumeId,
					VolumeCapability: &csi.VolumeCapability{
						AccessType: &csi.VolumeCapability_Mount{
							Mount: &csi.VolumeCapability_MountVolume{
								MountFlags: []string{"foo", "bar", "--test 123"},
							},
						},
						AccessMode: &csi.VolumeCapability_AccessMode{
							Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
						},
					},
					TargetPath:    targetPath,
					VolumeContext: map[string]string{"bucketName": bucketName},
					Readonly:      true,
				}

				nodeTestEnv.mockMounter.EXPECT().Mount(gomock.Eq(bucketName), gomock.Eq(targetPath), gomock.Any(), gomock.Eq([]string{"--bar", "--foo", "--read-only", "--test=123"}))
				_, err := nodeTestEnv.server.NodePublishVolume(ctx, req)
				if err != nil {
					t.Fatalf("NodePublishVolume is failed: %v", err)
				}

				nodeTestEnv.mockCtl.Finish()
			},
		},
		{
			name: "success: foreground option is removed",
			testFunc: func(t *testing.T) {
				nodeTestEnv := initNodeServerTestEnv(t)
				ctx := context.Background()
				req := &csi.NodePublishVolumeRequest{
					VolumeId: volumeId,
					VolumeCapability: &csi.VolumeCapability{
						AccessType: &csi.VolumeCapability_Mount{
							Mount: &csi.VolumeCapability_MountVolume{
								MountFlags: []string{"--foreground", "-f", "--test 123"},
							},
						},
						AccessMode: &csi.VolumeCapability_AccessMode{
							Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
						},
					},
					VolumeContext: map[string]string{"bucketName": bucketName},
					TargetPath:    targetPath,
					Readonly:      true,
				}

				nodeTestEnv.mockMounter.EXPECT().Mount(
					gomock.Eq(bucketName), gomock.Eq(targetPath), gomock.Any(),
					gomock.Eq([]string{"--read-only", "--test=123"})).Return(nil)
				_, err := nodeTestEnv.server.NodePublishVolume(ctx, req)
				if err != nil {
					t.Fatalf("NodePublishVolume is failed: %v", err)
				}

				nodeTestEnv.mockCtl.Finish()
			},
		},
		{
			name: "fail: missing volume id",
			testFunc: func(t *testing.T) {
				nodeTestEnv := initNodeServerTestEnv(t)
				ctx := context.Background()
				req := &csi.NodePublishVolumeRequest{
					VolumeCapability: stdVolCap,
					TargetPath:       targetPath,
					VolumeContext:    map[string]string{"bucketName": bucketName},
				}

				_, err := nodeTestEnv.server.NodePublishVolume(ctx, req)
				if err == nil {
					t.Fatalf("NodePublishVolume is failed: %v", err)
				}
				nodeTestEnv.mockCtl.Finish()
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, tc.testFunc)
	}
}

func TestNodeUnpublishVolume(t *testing.T) {
	var (
		volumeId   = "test-volume-id"
		targetPath = "/target/path"
	)
	testCases := []struct {
		name     string
		testFunc func(t *testing.T)
	}{
		{
			name: "success: happy path",
			testFunc: func(t *testing.T) {
				nodeTestEnv := initNodeServerTestEnv(t)
				ctx := context.Background()
				req := &csi.NodeUnpublishVolumeRequest{
					VolumeId:   volumeId,
					TargetPath: targetPath,
				}

				nodeTestEnv.mockMounter.EXPECT().IsMountPoint(gomock.Eq(targetPath)).Return(true, nil)
				nodeTestEnv.mockMounter.EXPECT().Unmount(gomock.Eq(targetPath)).Return(nil)
				_, err := nodeTestEnv.server.NodeUnpublishVolume(ctx, req)
				if err != nil {
					t.Fatalf("NodePublishVolume failed: %v", err)
				}

				nodeTestEnv.mockCtl.Finish()
			},
		},
		{
			name: "success: not mounted",
			testFunc: func(t *testing.T) {
				nodeTestEnv := initNodeServerTestEnv(t)
				ctx := context.Background()
				req := &csi.NodeUnpublishVolumeRequest{
					VolumeId:   volumeId,
					TargetPath: targetPath,
				}

				nodeTestEnv.mockMounter.EXPECT().IsMountPoint(gomock.Eq(targetPath)).Return(false, nil)
				_, err := nodeTestEnv.server.NodeUnpublishVolume(ctx, req)
				if err != nil {
					t.Fatalf("NodePublishVolume failed: %v", err)
				}

				nodeTestEnv.mockCtl.Finish()
			},
		},
		{
			name: "failure: unmount failure is error",
			testFunc: func(t *testing.T) {
				nodeTestEnv := initNodeServerTestEnv(t)
				ctx := context.Background()
				req := &csi.NodeUnpublishVolumeRequest{
					VolumeId:   volumeId,
					TargetPath: targetPath,
				}

				nodeTestEnv.mockMounter.EXPECT().IsMountPoint(gomock.Eq(targetPath)).Return(true, nil)
				nodeTestEnv.mockMounter.EXPECT().Unmount(gomock.Eq(targetPath)).Return(errors.New(""))
				_, err := nodeTestEnv.server.NodeUnpublishVolume(ctx, req)
				if err == nil {
					t.Fatalf("NodePublishVolume must fail")
				}

				nodeTestEnv.mockCtl.Finish()
			},
		},
		{
			name: "success: inexistent dir",
			testFunc: func(t *testing.T) {
				nodeTestEnv := initNodeServerTestEnv(t)
				ctx := context.Background()
				req := &csi.NodeUnpublishVolumeRequest{
					VolumeId:   volumeId,
					TargetPath: targetPath,
				}

				expectedError := fs.ErrNotExist
				nodeTestEnv.mockMounter.EXPECT().IsMountPoint(gomock.Eq(targetPath)).Return(false, expectedError)
				_, err := nodeTestEnv.server.NodeUnpublishVolume(ctx, req)
				if err != nil {
					t.Fatalf("NodePublishVolume failed: %v", err)
				}

				nodeTestEnv.mockCtl.Finish()
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, tc.testFunc)
	}
}

func TestNodeGetCapabilities(t *testing.T) {
	nodeTestEnv := initNodeServerTestEnv(t)
	ctx := context.Background()
	req := &csi.NodeGetCapabilitiesRequest{}

	resp, err := nodeTestEnv.server.NodeGetCapabilities(ctx, req)
	if err != nil {
		t.Fatalf("NodeGetCapabilities failed: %v", err)
	}

	capabilities := resp.GetCapabilities()
	if len(capabilities) != 0 {
		t.Fatalf("NodeGetCapabilities failed: capabilities not empty")
	}

	nodeTestEnv.mockCtl.Finish()
}
