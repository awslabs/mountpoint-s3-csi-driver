package node_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/golang/mock/gomock"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/envprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/mounter"
	mock_driver "github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/mounter/mocks"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/volumecontext"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

const testNodeID = "test-nodeID"

type nodeServerTestEnv struct {
	mockCtl     *gomock.Controller
	mockMounter *mock_driver.MockMounter
	server      *node.S3NodeServer
}

func initNodeServerTestEnv(t *testing.T) *nodeServerTestEnv {
	mockCtl := gomock.NewController(t)
	mockMounter := mock_driver.NewMockMounter(mockCtl)
	server := node.NewS3NodeServer(testNodeID, mockMounter)
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
		targetPath = "/var/lib/kubelet/target/path"
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

				nodeTestEnv.mockMounter.EXPECT().Mount(
					gomock.Eq(context.Background()),
					gomock.Eq(bucketName),
					gomock.Eq(targetPath),
					gomock.Eq(credentialprovider.ProvideContext{
						VolumeID:             volumeId,
						AuthenticationSource: credentialprovider.AuthenticationSourceDriver,
					}),
					// The mounter reads the PV's cache opt-in from the volume attributes.
					gomock.Eq(map[string]string{"bucketName": bucketName}),
					gomock.Any(),
					gomock.Eq(""),
					gomock.Eq(envprovider.Environment{}),
				)
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

				nodeTestEnv.mockMounter.EXPECT().Mount(
					gomock.Eq(context.Background()),
					gomock.Eq(bucketName),
					gomock.Eq(targetPath),
					gomock.Eq(credentialprovider.ProvideContext{
						VolumeID:             volumeId,
						AuthenticationSource: credentialprovider.AuthenticationSourceDriver,
					}),
					gomock.Any(),
					gomock.Eq(mountpoint.ParseArgs([]string{"--read-only", "--allow-root"})),
					gomock.Eq(""),
					gomock.Eq(envprovider.Environment{}),
				)
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

				nodeTestEnv.mockMounter.EXPECT().Mount(
					gomock.Eq(context.Background()),
					gomock.Eq(bucketName),
					gomock.Eq(targetPath),
					gomock.Eq(credentialprovider.ProvideContext{
						VolumeID:             volumeId,
						AuthenticationSource: credentialprovider.AuthenticationSourceDriver,
					}),
					gomock.Any(),
					gomock.Eq(mountpoint.ParseArgs([]string{"--bar", "--foo", "--read-only", "--allow-root", "--test=123"})),
					gomock.Eq(""),
					gomock.Eq(envprovider.Environment{}),
				)
				_, err := nodeTestEnv.server.NodePublishVolume(ctx, req)
				if err != nil {
					t.Fatalf("NodePublishVolume is failed: %v", err)
				}

				nodeTestEnv.mockCtl.Finish()
			},
		},
		{
			name: "fail: fstab style option is present",
			testFunc: func(t *testing.T) {
				nodeTestEnv := initNodeServerTestEnv(t)
				ctx := context.Background()
				req := &csi.NodePublishVolumeRequest{
					VolumeId: volumeId,
					VolumeCapability: &csi.VolumeCapability{
						AccessType: &csi.VolumeCapability_Mount{
							Mount: &csi.VolumeCapability_MountVolume{
								MountFlags: []string{"-o rw,nosuid,nodev,allow-other"},
							},
						},
						AccessMode: &csi.VolumeCapability_AccessMode{
							Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
						},
					},
					TargetPath:    targetPath,
					VolumeContext: map[string]string{"bucketName": bucketName},
				}

				_, err := nodeTestEnv.server.NodePublishVolume(ctx, req)
				if err == nil {
					t.Fatalf("NodePublishVolume is failed: %v", err)
				}
				nodeTestEnv.mockCtl.Finish()
			},
		},
		{
			name: "fail: --ca-bundle option is present",
			testFunc: func(t *testing.T) {
				nodeTestEnv := initNodeServerTestEnv(t)
				ctx := context.Background()
				req := &csi.NodePublishVolumeRequest{
					VolumeId: volumeId,
					VolumeCapability: &csi.VolumeCapability{
						AccessType: &csi.VolumeCapability_Mount{
							Mount: &csi.VolumeCapability_MountVolume{
								MountFlags: []string{"--ca-bundle=/etc/ssl/certs/custom-ca.pem"},
							},
						},
						AccessMode: &csi.VolumeCapability_AccessMode{
							Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
						},
					},
					TargetPath:    targetPath,
					VolumeContext: map[string]string{"bucketName": bucketName},
				}

				_, err := nodeTestEnv.server.NodePublishVolume(ctx, req)
				if err == nil {
					t.Fatalf("NodePublishVolume should fail when --ca-bundle is set")
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
					gomock.Eq(context.Background()),
					gomock.Eq(bucketName),
					gomock.Eq(targetPath),
					gomock.Eq(credentialprovider.ProvideContext{
						VolumeID:             volumeId,
						AuthenticationSource: credentialprovider.AuthenticationSourceDriver,
					}),
					gomock.Any(),
					gomock.Eq(mountpoint.ParseArgs([]string{"--read-only", "--allow-root", "--test=123"})),
					gomock.Eq(""),
					gomock.Eq(envprovider.Environment{}),
				).Return(nil)
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
		{
			name: "success: reads SA tokens from secrets field",
			testFunc: func(t *testing.T) {
				nodeTestEnv := initNodeServerTestEnv(t)
				ctx := context.Background()
				tokensJSON := `{"sts.amazonaws.com":{"token":"sts-token","expirationTimestamp":"2026-01-01T00:00:00Z"}}`
				req := &csi.NodePublishVolumeRequest{
					VolumeId:         volumeId,
					VolumeCapability: stdVolCap,
					TargetPath:       targetPath,
					VolumeContext: map[string]string{
						"bucketName":           bucketName,
						"authenticationSource": "pod",
					},
					Secrets: map[string]string{
						"csi.storage.k8s.io/serviceAccount.tokens": tokensJSON,
					},
				}

				nodeTestEnv.mockMounter.EXPECT().Mount(
					gomock.Eq(ctx),
					gomock.Eq(bucketName),
					gomock.Eq(targetPath),
					gomock.Eq(credentialprovider.ProvideContext{
						VolumeID:             volumeId,
						AuthenticationSource: credentialprovider.AuthenticationSourcePod,
						ServiceAccountTokens: tokensJSON,
					}),
					gomock.Any(),
					gomock.Any(),
					gomock.Eq(""),
					gomock.Eq(envprovider.Environment{}),
				)
				_, err := nodeTestEnv.server.NodePublishVolume(ctx, req)
				if err != nil {
					t.Fatalf("NodePublishVolume failed: %v", err)
				}

				nodeTestEnv.mockCtl.Finish()
			},
		},
		{
			name: "success: falls back to volume context for SA tokens when secrets field is empty",
			testFunc: func(t *testing.T) {
				nodeTestEnv := initNodeServerTestEnv(t)
				ctx := context.Background()
				tokensJSON := `{"sts.amazonaws.com":{"token":"sts-token","expirationTimestamp":"2026-01-01T00:00:00Z"}}`
				req := &csi.NodePublishVolumeRequest{
					VolumeId:         volumeId,
					VolumeCapability: stdVolCap,
					TargetPath:       targetPath,
					VolumeContext: map[string]string{
						"bucketName":           bucketName,
						"authenticationSource": "pod",
						"csi.storage.k8s.io/serviceAccount.tokens": tokensJSON,
					},
				}

				nodeTestEnv.mockMounter.EXPECT().Mount(
					gomock.Eq(ctx),
					gomock.Eq(bucketName),
					gomock.Eq(targetPath),
					gomock.Eq(credentialprovider.ProvideContext{
						VolumeID:             volumeId,
						AuthenticationSource: credentialprovider.AuthenticationSourcePod,
						ServiceAccountTokens: tokensJSON,
					}),
					gomock.Any(),
					gomock.Any(),
					gomock.Eq(""),
					gomock.Eq(envprovider.Environment{}),
				)
				_, err := nodeTestEnv.server.NodePublishVolume(ctx, req)
				if err != nil {
					t.Fatalf("NodePublishVolume failed: %v", err)
				}

				nodeTestEnv.mockCtl.Finish()
			},
		},
		{
			name: "success: secrets field takes precedence over volume context for SA tokens",
			testFunc: func(t *testing.T) {
				nodeTestEnv := initNodeServerTestEnv(t)
				ctx := context.Background()
				secretsTokens := `{"sts.amazonaws.com":{"token":"secrets-token","expirationTimestamp":"2026-01-01T00:00:00Z"}}`
				volumeCtxTokens := `{"sts.amazonaws.com":{"token":"volume-ctx-token","expirationTimestamp":"2026-01-01T00:00:00Z"}}`
				req := &csi.NodePublishVolumeRequest{
					VolumeId:         volumeId,
					VolumeCapability: stdVolCap,
					TargetPath:       targetPath,
					VolumeContext: map[string]string{
						"bucketName":           bucketName,
						"authenticationSource": "pod",
						"csi.storage.k8s.io/serviceAccount.tokens": volumeCtxTokens,
					},
					Secrets: map[string]string{
						"csi.storage.k8s.io/serviceAccount.tokens": secretsTokens,
					},
				}

				nodeTestEnv.mockMounter.EXPECT().Mount(
					gomock.Eq(ctx),
					gomock.Eq(bucketName),
					gomock.Eq(targetPath),
					gomock.Eq(credentialprovider.ProvideContext{
						VolumeID:             volumeId,
						AuthenticationSource: credentialprovider.AuthenticationSourcePod,
						ServiceAccountTokens: secretsTokens,
					}),
					gomock.Any(),
					gomock.Any(),
					gomock.Eq(""),
					gomock.Eq(envprovider.Environment{}),
				)
				_, err := nodeTestEnv.server.NodePublishVolume(ctx, req)
				if err != nil {
					t.Fatalf("NodePublishVolume failed: %v", err)
				}

				nodeTestEnv.mockCtl.Finish()
			},
		},
		{
			name: "success: config proxy",
			testFunc: func(t *testing.T) {
				nodeTestEnv := initNodeServerTestEnv(t)
				ctx := context.Background()
				req := &csi.NodePublishVolumeRequest{
					VolumeId:         volumeId,
					VolumeCapability: stdVolCap,
					TargetPath:       targetPath,
					VolumeContext: map[string]string{
						"bucketName":                bucketName,
						"mountpointEnv.HTTPS_PROXY": "proxy:3128",
						"mountpointEnv.NO_PROXY":    "noproxy.com",
					},
				}

				nodeTestEnv.mockMounter.EXPECT().Mount(
					gomock.Eq(context.Background()),
					gomock.Eq(bucketName),
					gomock.Eq(targetPath),
					gomock.Eq(credentialprovider.ProvideContext{
						VolumeID:             volumeId,
						AuthenticationSource: credentialprovider.AuthenticationSourceDriver,
					}),
					gomock.Any(),
					gomock.Any(),
					gomock.Eq(""),
					gomock.Eq(envprovider.Environment{
						envprovider.EnvHTTPSProxy: "proxy:3128",
						envprovider.EnvNoProxy:    "noproxy.com",
					}),
				)
				_, err := nodeTestEnv.server.NodePublishVolume(ctx, req)
				if err != nil {
					t.Fatalf("NodePublishVolume is failed: %v", err)
				}

				nodeTestEnv.mockCtl.Finish()
			},
		},
		{
			name: "fail: config unallowed environment",
			testFunc: func(t *testing.T) {
				nodeTestEnv := initNodeServerTestEnv(t)
				ctx := context.Background()
				req := &csi.NodePublishVolumeRequest{
					VolumeId:         volumeId,
					VolumeCapability: stdVolCap,
					TargetPath:       targetPath,
					VolumeContext: map[string]string{
						"bucketName":        bucketName,
						"mountpointEnv.FOO": "BAR",
					},
				}
				_, err := nodeTestEnv.server.NodePublishVolume(ctx, req)
				if err == nil {
					t.Fatal("NodePublishVolume is success")
				}

				expectedErrMsg := "environment variable not allowed: mountpointEnv.FOO"
				if !strings.Contains(err.Error(), expectedErrMsg) {
					t.Errorf("Expected error message %q, but got %q", expectedErrMsg, err.Error())
				}

				nodeTestEnv.mockCtl.Finish()
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, tc.testFunc)
	}
}

func TestNodePublishVolumeForPodMounter(t *testing.T) {
	var (
		volumeId   = "test-volume-id"
		bucketName = "test-bucket-name"
		targetPath = "/var/lib/kubelet/target/path"
	)
	testCases := []struct {
		name     string
		testFunc func(t *testing.T)
	}{
		{
			name: "success: sets gid, allow-other, dir-mode, file-mode flags if fsGroup is provided",
			testFunc: func(t *testing.T) {
				nodeTestEnv := initNodeServerTestEnv(t)
				ctx := context.Background()
				req := &csi.NodePublishVolumeRequest{
					VolumeId: volumeId,
					VolumeCapability: &csi.VolumeCapability{
						AccessType: &csi.VolumeCapability_Mount{
							Mount: &csi.VolumeCapability_MountVolume{
								MountFlags:       []string{},
								VolumeMountGroup: "123",
							},
						},
						AccessMode: &csi.VolumeCapability_AccessMode{
							Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
						},
					},
					VolumeContext: map[string]string{"bucketName": bucketName},
					TargetPath:    targetPath,
				}

				nodeTestEnv.mockMounter.EXPECT().Mount(
					gomock.Eq(context.Background()),
					gomock.Eq(bucketName),
					gomock.Eq(targetPath),
					gomock.Eq(credentialprovider.ProvideContext{
						VolumeID:             volumeId,
						AuthenticationSource: credentialprovider.AuthenticationSourceDriver,
					}),
					gomock.Any(),
					gomock.Eq(mountpoint.ParseArgs([]string{"--gid=123", "--allow-other", "--dir-mode=770", "--file-mode=660"})),
					gomock.Eq("123"),
					gomock.Eq(envprovider.Environment{}),
				).Return(nil)
				_, err := nodeTestEnv.server.NodePublishVolume(ctx, req)
				if err != nil {
					t.Fatalf("NodePublishVolume is failed: %v", err)
				}

				nodeTestEnv.mockCtl.Finish()
			},
		},
		{
			name: "success: sets gid, allow-other, dir-mode, file-mode flags if fsGroup is provided and allow-other flag is provided in mountOptions",
			testFunc: func(t *testing.T) {
				nodeTestEnv := initNodeServerTestEnv(t)
				ctx := context.Background()
				req := &csi.NodePublishVolumeRequest{
					VolumeId: volumeId,
					VolumeCapability: &csi.VolumeCapability{
						AccessType: &csi.VolumeCapability_Mount{
							Mount: &csi.VolumeCapability_MountVolume{
								MountFlags:       []string{"--allow-other"},
								VolumeMountGroup: "123",
							},
						},
						AccessMode: &csi.VolumeCapability_AccessMode{
							Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
						},
					},
					VolumeContext: map[string]string{"bucketName": bucketName},
					TargetPath:    targetPath,
				}

				nodeTestEnv.mockMounter.EXPECT().Mount(
					gomock.Eq(context.Background()),
					gomock.Eq(bucketName),
					gomock.Eq(targetPath),
					gomock.Eq(credentialprovider.ProvideContext{
						VolumeID:             volumeId,
						AuthenticationSource: credentialprovider.AuthenticationSourceDriver,
					}),
					gomock.Any(),
					gomock.Eq(mountpoint.ParseArgs([]string{"--gid=123", "--allow-other", "--dir-mode=770", "--file-mode=660"})),
					gomock.Eq("123"),
					gomock.Eq(envprovider.Environment{}),
				).Return(nil)
				_, err := nodeTestEnv.server.NodePublishVolume(ctx, req)
				if err != nil {
					t.Fatalf("NodePublishVolume is failed: %v", err)
				}

				nodeTestEnv.mockCtl.Finish()
			},
		},
		{
			name: "success: sets only allow-root flag if fsGroup is empty string and allow-other flag is not provided in mountOptions",
			testFunc: func(t *testing.T) {
				nodeTestEnv := initNodeServerTestEnv(t)
				ctx := context.Background()
				req := &csi.NodePublishVolumeRequest{
					VolumeId: volumeId,
					VolumeCapability: &csi.VolumeCapability{
						AccessType: &csi.VolumeCapability_Mount{
							Mount: &csi.VolumeCapability_MountVolume{
								MountFlags:       []string{},
								VolumeMountGroup: "",
							},
						},
						AccessMode: &csi.VolumeCapability_AccessMode{
							Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
						},
					},
					VolumeContext: map[string]string{"bucketName": bucketName},
					TargetPath:    targetPath,
				}

				nodeTestEnv.mockMounter.EXPECT().Mount(
					gomock.Eq(context.Background()),
					gomock.Eq(bucketName),
					gomock.Eq(targetPath),
					gomock.Eq(credentialprovider.ProvideContext{
						VolumeID:             volumeId,
						AuthenticationSource: credentialprovider.AuthenticationSourceDriver,
					}),
					gomock.Any(),
					gomock.Eq(mountpoint.ParseArgs([]string{"--allow-root"})),
					gomock.Eq(""),
					gomock.Eq(envprovider.Environment{}),
				).Return(nil)
				_, err := nodeTestEnv.server.NodePublishVolume(ctx, req)
				if err != nil {
					t.Fatalf("NodePublishVolume is failed: %v", err)
				}

				nodeTestEnv.mockCtl.Finish()
			},
		},
		{
			name: "success: does not set allow-root flag if fsGroup is empty string and allow-other flag is provided in mountOptions",
			testFunc: func(t *testing.T) {
				nodeTestEnv := initNodeServerTestEnv(t)
				ctx := context.Background()
				req := &csi.NodePublishVolumeRequest{
					VolumeId: volumeId,
					VolumeCapability: &csi.VolumeCapability{
						AccessType: &csi.VolumeCapability_Mount{
							Mount: &csi.VolumeCapability_MountVolume{
								MountFlags:       []string{"--allow-other"},
								VolumeMountGroup: "",
							},
						},
						AccessMode: &csi.VolumeCapability_AccessMode{
							Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
						},
					},
					VolumeContext: map[string]string{"bucketName": bucketName},
					TargetPath:    targetPath,
				}

				nodeTestEnv.mockMounter.EXPECT().Mount(
					gomock.Eq(context.Background()),
					gomock.Eq(bucketName),
					gomock.Eq(targetPath),
					gomock.Eq(credentialprovider.ProvideContext{
						VolumeID:             volumeId,
						AuthenticationSource: credentialprovider.AuthenticationSourceDriver,
					}),
					gomock.Any(),
					gomock.Eq(mountpoint.ParseArgs([]string{"--allow-other"})),
					gomock.Eq(""),
					gomock.Eq(envprovider.Environment{}),
				).Return(nil)
				_, err := nodeTestEnv.server.NodePublishVolume(ctx, req)
				if err != nil {
					t.Fatalf("NodePublishVolume is failed: %v", err)
				}

				nodeTestEnv.mockCtl.Finish()
			},
		},
		{
			name: "success: uses gid, allow-other, dir-mode, file-mode from mountOptions if fsGroup is set and these flags are provided in mountOptions",
			testFunc: func(t *testing.T) {
				nodeTestEnv := initNodeServerTestEnv(t)
				ctx := context.Background()
				mountFlags := []string{"--gid 456", "--allow-other", "--dir-mode=555", "--file-mode=444"}
				req := &csi.NodePublishVolumeRequest{
					VolumeId: volumeId,
					VolumeCapability: &csi.VolumeCapability{
						AccessType: &csi.VolumeCapability_Mount{
							Mount: &csi.VolumeCapability_MountVolume{
								MountFlags:       mountFlags,
								VolumeMountGroup: "123",
							},
						},
						AccessMode: &csi.VolumeCapability_AccessMode{
							Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
						},
					},
					VolumeContext: map[string]string{"bucketName": bucketName},
					TargetPath:    targetPath,
				}

				nodeTestEnv.mockMounter.EXPECT().Mount(
					gomock.Eq(context.Background()),
					gomock.Eq(bucketName),
					gomock.Eq(targetPath),
					gomock.Eq(credentialprovider.ProvideContext{
						VolumeID:             volumeId,
						AuthenticationSource: credentialprovider.AuthenticationSourceDriver,
					}),
					gomock.Any(),
					gomock.Eq(mountpoint.ParseArgs(mountFlags)),
					gomock.Eq("123"),
					gomock.Eq(envprovider.Environment{}),
				).Return(nil)
				_, err := nodeTestEnv.server.NodePublishVolume(ctx, req)
				if err != nil {
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

func TestNodePublishVolumeMounterErrorCode(t *testing.T) {
	const (
		volumeID   = "test-volume-id"
		bucketName = "test-bucket-name"
		targetPath = "/var/lib/kubelet/pods/pod-uid/volumes/kubernetes.io~csi/test-pv-name/mount"
	)

	testCases := []struct {
		name     string
		mountErr error
		wantCode codes.Code
	}{
		{
			name:     "a mounter's own rejection reaches the kubelet with its code",
			mountErr: status.Error(codes.InvalidArgument, "requests a local cache, but s3-csi-daemonset-mounter has no cache volume"),
			wantCode: codes.InvalidArgument,
		},
		{
			name:     "a plain mount failure is Internal",
			mountErr: errors.New("failed to send mount options"),
			wantCode: codes.Internal,
		},
		{
			name:     "a rejection wrapped on the way out keeps its code",
			mountErr: fmt.Errorf("cannot share mount for volume %s: %w", volumeID, status.Error(codes.InvalidArgument, "requests a local cache")),
			wantCode: codes.InvalidArgument,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			nodeTestEnv := initNodeServerTestEnv(t)
			defer nodeTestEnv.mockCtl.Finish()

			nodeTestEnv.mockMounter.EXPECT().
				Mount(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
				Return(testCase.mountErr)

			_, err := nodeTestEnv.server.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
				VolumeId: volumeID,
				VolumeCapability: &csi.VolumeCapability{
					AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
					AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
				},
				TargetPath:    targetPath,
				VolumeContext: map[string]string{volumecontext.BucketName: bucketName},
			})
			assert.Equals(t, testCase.wantCode, status.Code(err))
		})
	}
}

func TestNodePublishVolumeMaxCacheSizeInjection(t *testing.T) {
	var (
		volumeId   = "test-volume-id"
		bucketName = "test-bucket-name"
		targetPath = "/var/lib/kubelet/target/path"
	)

	testCases := []struct {
		name         string
		volumeCtx    map[string]string
		mountFlags   []string
		expectedArgs []string
		expectError  bool
	}{
		{
			name: "injects max-cache-size from cacheEmptyDirSizeLimit",
			volumeCtx: map[string]string{
				volumecontext.BucketName:             bucketName,
				volumecontext.Cache:                  volumecontext.CacheTypeEmptyDir,
				volumecontext.CacheEmptyDirSizeLimit: "50Mi",
			},
			expectedArgs: []string{"--allow-root", "--max-cache-size=47"},
		},
		{
			name: "converts GiB to MiB correctly",
			volumeCtx: map[string]string{
				volumecontext.BucketName:             bucketName,
				volumecontext.Cache:                  volumecontext.CacheTypeEmptyDir,
				volumecontext.CacheEmptyDirSizeLimit: "2Gi",
			},
			expectedArgs: []string{"--allow-root", "--max-cache-size=1945"},
		},
		{
			name: "does not inject when medium is Memory (tmpfs has isolated filesystem)",
			volumeCtx: map[string]string{
				volumecontext.BucketName:             bucketName,
				volumecontext.Cache:                  volumecontext.CacheTypeEmptyDir,
				volumecontext.CacheEmptyDirSizeLimit: "50Mi",
				volumecontext.CacheEmptyDirMedium:    string(corev1.StorageMediumMemory),
			},
			expectedArgs: []string{"--allow-root"},
		},
		{
			name: "does not inject when medium is HugePages (has isolated filesystem)",
			volumeCtx: map[string]string{
				volumecontext.BucketName:             bucketName,
				volumecontext.Cache:                  volumecontext.CacheTypeEmptyDir,
				volumecontext.CacheEmptyDirSizeLimit: "50Mi",
				volumecontext.CacheEmptyDirMedium:    string(corev1.StorageMediumHugePages),
			},
			expectedArgs: []string{"--allow-root"},
		},
		{
			name: "explicit max-cache-size below size limit takes precedence over auto-injected value",
			volumeCtx: map[string]string{
				volumecontext.BucketName:             bucketName,
				volumecontext.Cache:                  volumecontext.CacheTypeEmptyDir,
				volumecontext.CacheEmptyDirSizeLimit: "50Mi",
			},
			mountFlags:   []string{"--max-cache-size=40"},
			expectedArgs: []string{"--allow-root", "--max-cache-size=40"},
		},
		{
			name: "explicit max-cache-size equal to size limit is clamped to safe value for default medium",
			volumeCtx: map[string]string{
				volumecontext.BucketName:             bucketName,
				volumecontext.Cache:                  volumecontext.CacheTypeEmptyDir,
				volumecontext.CacheEmptyDirSizeLimit: "50Mi",
			},
			mountFlags:   []string{"--max-cache-size=50"}, // 50 MiB > safe threshold (47 MiB), gets clamped
			expectedArgs: []string{"--allow-root", "--max-cache-size=47"},
		},
		{
			name: "explicit max-cache-size in danger zone is clamped to safe value for default medium",
			volumeCtx: map[string]string{
				volumecontext.BucketName:             bucketName,
				volumecontext.Cache:                  volumecontext.CacheTypeEmptyDir,
				volumecontext.CacheEmptyDirSizeLimit: "50Mi",
			},
			mountFlags:   []string{"--max-cache-size=48"}, // 48 MiB > safe threshold (47 MiB) but <= limit (50 MiB)
			expectedArgs: []string{"--allow-root", "--max-cache-size=47"},
		},
		{
			name: "explicit max-cache-size near limit is preserved for Memory medium",
			volumeCtx: map[string]string{
				volumecontext.BucketName:             bucketName,
				volumecontext.Cache:                  volumecontext.CacheTypeEmptyDir,
				volumecontext.CacheEmptyDirSizeLimit: "50Mi",
				volumecontext.CacheEmptyDirMedium:    string(corev1.StorageMediumMemory),
			},
			mountFlags:   []string{"--max-cache-size=50"}, // Memory has accurate statvfs, no clamping needed
			expectedArgs: []string{"--allow-root", "--max-cache-size=50"},
		},
		{
			name: "explicit max-cache-size exceeding size limit returns error",
			volumeCtx: map[string]string{
				volumecontext.BucketName:             bucketName,
				volumecontext.Cache:                  volumecontext.CacheTypeEmptyDir,
				volumecontext.CacheEmptyDirSizeLimit: "50Mi",
			},
			mountFlags:  []string{"--max-cache-size=100"},
			expectError: true,
		},
		{
			name: "explicit max-cache-size exceeding size limit returns error for Memory medium",
			volumeCtx: map[string]string{
				volumecontext.BucketName:             bucketName,
				volumecontext.Cache:                  volumecontext.CacheTypeEmptyDir,
				volumecontext.CacheEmptyDirSizeLimit: "50Mi",
				volumecontext.CacheEmptyDirMedium:    string(corev1.StorageMediumMemory),
			},
			mountFlags:  []string{"--max-cache-size=100"},
			expectError: true,
		},
		{
			name: "does not inject when cache is not emptyDir",
			volumeCtx: map[string]string{
				volumecontext.BucketName:             bucketName,
				volumecontext.Cache:                  volumecontext.CacheTypeEphemeral,
				volumecontext.CacheEmptyDirSizeLimit: "50Mi",
			},
			expectedArgs: []string{"--allow-root"},
		},
		{
			name: "no injection when cacheEmptyDirSizeLimit is not set",
			volumeCtx: map[string]string{
				volumecontext.BucketName: bucketName,
			},
			expectedArgs: []string{"--allow-root"},
		},
		{
			name: "returns error for invalid cacheEmptyDirSizeLimit",
			volumeCtx: map[string]string{
				volumecontext.BucketName:             bucketName,
				volumecontext.Cache:                  volumecontext.CacheTypeEmptyDir,
				volumecontext.CacheEmptyDirSizeLimit: "not-a-quantity",
			},
			expectError: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			nodeTestEnv := initNodeServerTestEnv(t)
			ctx := context.Background()

			volCap := &csi.VolumeCapability{
				AccessType: &csi.VolumeCapability_Mount{
					Mount: &csi.VolumeCapability_MountVolume{
						MountFlags: tc.mountFlags,
					},
				},
				AccessMode: &csi.VolumeCapability_AccessMode{
					Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
				},
			}

			req := &csi.NodePublishVolumeRequest{
				VolumeId:         volumeId,
				VolumeCapability: volCap,
				TargetPath:       targetPath,
				VolumeContext:    tc.volumeCtx,
			}

			if !tc.expectError {
				nodeTestEnv.mockMounter.EXPECT().Mount(
					gomock.Eq(ctx),
					gomock.Eq(bucketName),
					gomock.Eq(targetPath),
					gomock.Any(),
					gomock.Any(),
					gomock.Eq(mountpoint.ParseArgs(tc.expectedArgs)),
					gomock.Eq(""),
					gomock.Eq(envprovider.Environment{}),
				).Return(nil)
			}

			_, err := nodeTestEnv.server.NodePublishVolume(ctx, req)
			if tc.expectError {
				if err == nil {
					t.Fatal("expected error but got nil")
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			}

			nodeTestEnv.mockCtl.Finish()
		})
	}
}

func TestNodeUnpublishVolume(t *testing.T) {
	var (
		volumeId   = "test-volume-id"
		targetPath = "/var/lib/kubelet/target/path"
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
				nodeTestEnv.mockMounter.EXPECT().Unmount(gomock.Eq(ctx), gomock.Eq(targetPath), gomock.Any())
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
				nodeTestEnv.mockMounter.EXPECT().Unmount(
					gomock.Eq(ctx),
					gomock.Eq(targetPath),
					gomock.Eq(credentialprovider.CleanupContext{
						VolumeID: volumeId,
					}),
				).Return(errors.New(""))
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

func TestNodeGetCapabilitiesForPodMounter(t *testing.T) {
	nodeTestEnv := initNodeServerTestEnv(t)
	ctx := context.Background()
	req := &csi.NodeGetCapabilitiesRequest{}

	resp, err := nodeTestEnv.server.NodeGetCapabilities(ctx, req)
	if err != nil {
		t.Fatalf("NodeGetCapabilities failed: %v", err)
	}

	assert.Equals(t, []*csi.NodeServiceCapability{
		{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{
					Type: csi.NodeServiceCapability_RPC_VOLUME_MOUNT_GROUP,
				},
			},
		},
	}, resp.GetCapabilities())

	nodeTestEnv.mockCtl.Finish()
}

func TestNodeGetInfo(t *testing.T) {
	testCases := []struct {
		name     string
		testFunc func(t *testing.T)
	}{
		{
			name: "returns the limit a VolumeLimiter mounter decides, at each call",
			testFunc: func(t *testing.T) {
				limiter := &limitingMounter{limit: 1}
				server := node.NewS3NodeServer(testNodeID, limiter)

				resp, err := server.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
				assert.NoError(t, err)
				assert.Equals(t, int64(1), resp.MaxVolumesPerNode)

				limiter.limit = 4
				resp, err = server.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
				assert.NoError(t, err)
				assert.Equals(t, int64(4), resp.MaxVolumesPerNode)
			},
		},
		{
			name: "fails while a VolumeLimiter mounter can't decide the limit",
			testFunc: func(t *testing.T) {
				limiter := &limitingMounter{err: errors.New("no mounter yet")}
				server := node.NewS3NodeServer(testNodeID, limiter)

				_, err := server.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
				assert.Equals(t, codes.Unavailable, status.Code(err))
			},
		},
		{
			name: "returns no limit for a mounter that isn't a VolumeLimiter, as the pod mounter",
			testFunc: func(t *testing.T) {
				nodeTestEnv := initNodeServerTestEnv(t)

				ctx := context.Background()
				req := &csi.NodeGetInfoRequest{}

				resp, err := nodeTestEnv.server.NodeGetInfo(ctx, req)
				if err != nil {
					t.Fatalf("NodeGetInfo failed: %v", err)
				}
				assert.Equals(t, testNodeID, resp.NodeId)
				assert.Equals(t, int64(0), resp.MaxVolumesPerNode)
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, tc.testFunc)
	}
}

var _ node.VolumeLimiter = &limitingMounter{}

// limitingMounter is a mounter that decides the node's volume limit.
type limitingMounter struct {
	dummyMounter
	limit int64
	err   error
}

func (m *limitingMounter) VolumeLimit(context.Context) (int64, error) {
	return m.limit, m.err
}

var _ mounter.Mounter = &dummyMounter{}

type dummyMounter struct{}

func (d *dummyMounter) Mount(ctx context.Context, bucketName string, target string, provideCtx credentialprovider.ProvideContext, volumeCtx map[string]string, args mountpoint.Args, fsGroup string, userEnv envprovider.Environment) error {
	return nil
}

func (d *dummyMounter) Unmount(ctx context.Context, target string, cleanupCtx credentialprovider.CleanupContext) error {
	return nil
}

func (d *dummyMounter) IsMountPoint(target string) (bool, error) {
	return true, nil
}

// Fake credentials shaped like real ones, so both the verbatim and the shape checks have something to find.
// The JWT is three base64url parts: {"alg":"RS256"}, {"sub":"test"} and "signature".
const (
	fakeServiceAccountJWT = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ0ZXN0In0.c2lnbmF0dXJl"
	fakeAccessKeyID       = "AKIAIOSFODNN7EXAMPLE"
	fakeSecretAccessKey   = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
)

// The kubelet hands csi-node the workload's service account tokens on every publish, in the volume context
// and (Kubernetes v1.35+) in the request's secrets. At the most verbose level csi-node logs every publish and
// unpublish, so neither may carry a token or key into the logs.
func TestNodePublishAndUnpublishLogNoCredentials(t *testing.T) {
	logs := testutil.CaptureKlogAtVerbosity(t, 10)

	nodeTestEnv := initNodeServerTestEnv(t)
	nodeTestEnv.mockMounter.EXPECT().Mount(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
	nodeTestEnv.mockMounter.EXPECT().IsMountPoint(gomock.Any()).Return(true, nil)
	nodeTestEnv.mockMounter.EXPECT().Unmount(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)

	tokens := fmt.Sprintf(`{"sts.amazonaws.com":{"token":%q,"expirationTimestamp":"2026-10-30T01:00:00Z"}}`, fakeServiceAccountJWT)
	const targetPath = "/var/lib/kubelet/pods/d8c872d7-a29c-4362-81b1-9912370d0813/volumes/kubernetes.io~csi/s3-pv/mount"
	ctx := context.Background()

	_, err := nodeTestEnv.server.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId: "s3-pv",
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
		},
		TargetPath: targetPath,
		VolumeContext: map[string]string{
			volumecontext.BucketName:              "test-bucket",
			volumecontext.AuthenticationSource:    "pod",
			volumecontext.CSIServiceAccountTokens: tokens,
		},
		Secrets: map[string]string{
			volumecontext.CSIServiceAccountTokens: tokens,
			// Not something the kubelet sends today, but node-publish secrets are free-form: none may be logged.
			"accessKeyId":     fakeAccessKeyID,
			"secretAccessKey": fakeSecretAccessKey,
		},
	})
	assert.NoError(t, err)

	_, err = nodeTestEnv.server.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "s3-pv",
		TargetPath: targetPath,
	})
	assert.NoError(t, err)

	testutil.AssertNoCredentialsLogged(t, logs(), fakeServiceAccountJWT, fakeAccessKeyID, fakeSecretAccessKey)
}

// NodePublishVolume parses a request built from the kubelet and the PV, so a malformed one must be refused
// with InvalidArgument and never panic. A request it accepts must reach the mounter with no `-o` or
// `--ca-bundle`, a target under the kubelet path, and only user environment variables on the allowlist.
func FuzzNodePublishVolumeNeverCrashesOnMalformedRequests(f *testing.F) {
	const target = "/var/lib/kubelet/pods/d8c872d7-a29c-4362-81b1-9912370d0813/volumes/kubernetes.io~csi/s3-pv/mount"
	type seed struct {
		volumeID, bucket   string
		hasBucket          bool
		targetPath         string
		mountFlags         string // one flag per line
		ctxKey, ctxValue   string
		readOnly, hasMount bool
	}
	for _, s := range []seed{
		{"s3-pv", "bucket", true, target, "", "", "", false, true},
		{"s3-pv", "bucket", true, target, "--allow-other\n--uid=1000\n--region us-west-2", "authenticationSource", "pod", true, true},
		{"s3-pv", "bucket", true, target, "-o allow_other", "", "", false, true},
		{"s3-pv", "bucket", true, target, " --ca-bundle=/etc/ssl/evil.pem ", "", "", false, true},
		{"s3-pv", "bucket", true, target, "--cache /tmp\n--max-cache-size=abc", "cache", "emptyDir", false, true},
		{"s3-pv", "bucket", true, target, "--max-cache-size=99999999999999999999", "cacheEmptyDirSizeLimit", "1Gi", false, true},
		{"s3-pv", "bucket", true, target, "", "mountpointEnv.AWS_ENDPOINT_URL", "http://attacker", false, true},
		{"s3-pv", "bucket", true, target, "", "mountpointEnv.HTTPS_PROXY", "http://proxy:3128", false, true},
		{"s3-pv", "bucket", true, target, "", "csi.storage.k8s.io/serviceAccount.tokens", "{not json", false, true},
		{"s3-pv", "bucket", true, "/etc/passwd", "", "", "", false, true},
		{"s3-pv", "bucket", true, "/var/lib/kubelet/../../etc", "", "", "", false, true},
		{"s3-pv", "", false, target, "", "", "", false, true},
		{"", "bucket", true, target, "", "", "", false, true},
		{"s3-pv", "bucket", true, "", "", "", "", false, true},
		{"s3-pv", "bucket", true, target, "", "", "", false, false},
		{"s3-pv\x00", "bucket\n", true, target + "\x00", "\x00", "\x00", "\x00", true, true},
	} {
		f.Add(s.volumeID, s.bucket, s.hasBucket, s.targetPath, s.mountFlags, s.ctxKey, s.ctxValue, s.readOnly, s.hasMount)
	}

	f.Fuzz(func(t *testing.T, volumeID, bucket string, hasBucket bool, targetPath, mountFlags, ctxKey, ctxValue string, readOnly, hasMount bool) {
		nodeTestEnv := initNodeServerTestEnv(t)
		nodeTestEnv.mockMounter.EXPECT().
			Mount(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
			DoAndReturn(func(_ context.Context, gotBucket, gotTarget string, _ credentialprovider.ProvideContext,
				_ map[string]string, args mountpoint.Args, _ string, userEnv envprovider.Environment) error {
				if args.Has(mountpoint.ArgFsTab) || args.Has(mountpoint.ArgCABundle) {
					t.Errorf("the mounter got a refused flag: %v", args.SortedList())
				}
				if !strings.HasPrefix(gotTarget, util.ContainerKubeletPath()) {
					t.Errorf("the mounter got target %q, outside the kubelet path %q", gotTarget, util.ContainerKubeletPath())
				}
				if hasBucket && gotBucket != bucket {
					t.Errorf("the mounter got bucket %q, want %q", gotBucket, bucket)
				}
				for key := range userEnv {
					if !slices.Contains([]string{envprovider.EnvHTTPSProxy, envprovider.EnvNoProxy}, key) {
						t.Errorf("the mounter got user environment variable %q, which is not on the allowlist", key)
					}
				}
				return nil
			}).AnyTimes()

		volCtx := map[string]string{}
		if ctxKey != "" {
			volCtx[ctxKey] = ctxValue
		}
		if hasBucket {
			volCtx["bucketName"] = bucket
		}
		volCap := &csi.VolumeCapability{
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
		}
		if hasMount {
			var flags []string
			if mountFlags != "" {
				flags = strings.Split(mountFlags, "\n")
			}
			volCap.AccessType = &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{MountFlags: flags}}
		}

		_, err := nodeTestEnv.server.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
			VolumeId:         volumeID,
			TargetPath:       targetPath,
			VolumeCapability: volCap,
			VolumeContext:    volCtx,
			Readonly:         readOnly,
		})
		// The mounter accepts everything here, so only the request itself can be refused, and only as invalid.
		if err != nil && status.Code(err) != codes.InvalidArgument {
			t.Errorf("NodePublishVolume refused a request with %v, want InvalidArgument: %v", status.Code(err), err)
		}
	})
}
