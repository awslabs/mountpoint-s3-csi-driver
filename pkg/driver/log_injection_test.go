package driver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/golang/mock/gomock"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node"
	mock_driver "github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/mounter/mocks"
)

// mountpointBucketError is Mountpoint's argument error, which the mounter relays through its error file, for a
// bucket name carrying CR/LF and a forged klog record.
const mountpointBucketError = "Mountpoint for mount test-volume-id failed: error: invalid value " +
	"'bucket_log_injection_before\r\nI9999 00:00:00.000000 1 FORGED_RECORD bucket_log_injection_after' for " +
	"'<BUCKET_NAME>': the bucket must have a valid name (only letters, numbers, . and -) or a valid ARN\n"

// The gRPC error that kubelet logs and turns into a FailedMount event must stay on one line, whatever text the
// mounter's error carries.
func TestNodePublishVolumeErrorHasNoControlCharacters(t *testing.T) {
	mockCtl := gomock.NewController(t)
	mockMounter := mock_driver.NewMockMounter(mockCtl)
	mockMounter.EXPECT().Mount(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(errors.New(mountpointBucketError))
	server := node.NewS3NodeServer("test-node", mockMounter)

	req := &csi.NodePublishVolumeRequest{
		VolumeId: "test-volume-id",
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
		},
		TargetPath:    "/var/lib/kubelet/target/path",
		VolumeContext: map[string]string{"bucketName": "test-bucket-name"},
	}
	_, err := logErrorsInterceptor(context.Background(), req, &grpc.UnaryServerInfo{FullMethod: "/csi.v1.Node/NodePublishVolume"},
		func(ctx context.Context, req any) (any, error) {
			return server.NodePublishVolume(ctx, req.(*csi.NodePublishVolumeRequest))
		})

	if status.Code(err) != codes.Internal {
		t.Fatalf("want the Internal code kept, got %v", err)
	}
	msg := status.Convert(err).Message()
	if strings.ContainsAny(msg, "\r\n") {
		t.Errorf("gRPC error message has a raw CR/LF: %q", msg)
	}
	if !strings.Contains(msg, `bucket_log_injection_before\r\nI9999 00:00:00.000000 1 FORGED_RECORD`) {
		t.Errorf("gRPC error message should keep Mountpoint's error, escaped: %q", msg)
	}
}

// A gRPC error carrying details keeps them, and an error that needs no escaping is returned as is.
func TestLogErrorsInterceptorKeepsStatus(t *testing.T) {
	st, _ := status.New(codes.Aborted, "dead\nmount").WithDetails(&csi.NodeGetInfoResponse{NodeId: "n"})
	_, err := logErrorsInterceptor(context.Background(), nil, &grpc.UnaryServerInfo{},
		func(context.Context, any) (any, error) { return nil, st.Err() })
	got := status.Convert(err)
	if got.Code() != codes.Aborted || got.Message() != `dead\nmount` || len(got.Details()) != 1 {
		t.Errorf("got code %v, message %q, %d details", got.Code(), got.Message(), len(got.Details()))
	}

	clean := status.Error(codes.NotFound, "no such volume")
	if _, err := logErrorsInterceptor(context.Background(), nil, &grpc.UnaryServerInfo{},
		func(context.Context, any) (any, error) { return nil, clean }); err != clean {
		t.Errorf("an error without control characters should be returned as is, got %v", err)
	}
}

// A context error that needs escaping keeps its code, as the gRPC server would give it.
func TestLogErrorsInterceptorKeepsContextErrorCode(t *testing.T) {
	_, err := logErrorsInterceptor(context.Background(), nil, &grpc.UnaryServerInfo{},
		func(context.Context, any) (any, error) {
			return nil, fmt.Errorf("mount of bucket\r\nFORGED: %w", context.DeadlineExceeded)
		})
	got := status.Convert(err)
	if got.Code() != codes.DeadlineExceeded || got.Message() != `mount of bucket\r\nFORGED: context deadline exceeded` {
		t.Errorf("got code %v, message %q", got.Code(), got.Message())
	}
}
