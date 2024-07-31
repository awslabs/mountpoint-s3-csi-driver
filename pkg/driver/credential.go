package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/klog/v2"
)

type authenticationSource = string

const (
	// This is when users don't provide a `authenticationSource` option in their volume attributes.
	// We're defaulting to `driver` in this case.
	authenticationSourceUnspecified authenticationSource = ""
	authenticationSourceDriver      authenticationSource = "driver"
	authenticationSourcePod         authenticationSource = "pod"
)

const defaultHostPluginDir = "/var/lib/kubelet/plugins/s3.csi.aws.com/"

const serviceAccountTokenAudienceSTS = "sts.amazonaws.com"

const serviceAccountRoleAnnotation = "eks.amazonaws.com/role-arn"

type credentialProvider struct {
	client k8sv1.CoreV1Interface
}

func (c *credentialProvider) provide(ctx context.Context, req *csi.NodePublishVolumeRequest) (*MountCredentials, error) {
	authenticationSource := req.VolumeContext[volumeCtxAuthenticationSource]
	switch authenticationSource {
	case authenticationSourcePod:
		return c.provideFromPod(ctx, req)
	case authenticationSourceUnspecified, authenticationSourceDriver:
		return c.provideFromDriver()
	default:
		return nil, fmt.Errorf("unknown `authenticationSource`: %s, only `driver` (default option if not specified) and `pod` supported", authenticationSource)
	}
}

func (c *credentialProvider) provideFromDriver() (*MountCredentials, error) {
	klog.V(4).Infof("NodePublishVolume: Using driver identity")

	hostPluginDir := hostPluginDirWithDefault()
	hostTokenPath := path.Join(hostPluginDir, "token")

	return &MountCredentials{
		AccessKeyID:     os.Getenv(keyIdEnv),
		SecretAccessKey: os.Getenv(accessKeyEnv),
		SessionToken:    os.Getenv(sessionTokenEnv),
		Region:          os.Getenv(regionEnv),
		DefaultRegion:   os.Getenv(defaultRegionEnv),
		WebTokenPath:    hostTokenPath,
		StsEndpoints:    os.Getenv(stsEndpointsEnv),
		AwsRoleArn:      os.Getenv(roleArnEnv),
	}, nil
}

func (c *credentialProvider) provideFromPod(ctx context.Context, req *csi.NodePublishVolumeRequest) (*MountCredentials, error) {
	klog.V(4).Infof("NodePublishVolume: Using pod identity")

	tokensJson := req.VolumeContext[volumeCtxServiceAccountTokens]
	if tokensJson == "" {
		klog.Error("`authenticationSource` configured to `pod` but no service account tokens are received. Please make sure to enable `podInfoOnMountCompat`, see TODO")
		return nil, status.Error(codes.InvalidArgument, "Missing service account tokens")
	}

	var tokens map[string]*Token
	if err := json.Unmarshal([]byte(tokensJson), &tokens); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "Failed to parse service account tokens: %v", err)
	}

	stsToken := tokens[serviceAccountTokenAudienceSTS]
	if stsToken == nil {
		klog.Errorf("`authenticationSource` configured to `pod` but no service account tokens for %s received. Please make sure to enable `podInfoOnMountCompat`, see TODO", serviceAccountTokenAudienceSTS)
		return nil, status.Errorf(codes.InvalidArgument, "Missing service account token for %s", serviceAccountTokenAudienceSTS)
	}

	volumeID := req.GetVolumeId()

	// TODO: Cleanup these files on unmount and startup.
	// TODO: Should we make the write atomic by writing to a temporary path and renaming afterwards?
	err := os.WriteFile(path.Join("/csi/", volumeID+".token"), []byte(stsToken.Token), 0400)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to write service account token: %v", err)
	}

	hostPluginDir := hostPluginDirWithDefault()
	hostTokenPath := path.Join(hostPluginDir, volumeID+".token")

	awsRoleARN, err := c.findPodServiceAccountRole(ctx, req)
	if err != nil {
		return nil, err
	}

	region := os.Getenv(regionEnv)
	defaultRegion := os.Getenv(defaultRegionEnv)

	// TODO: How to handle missing region? We should ideally try the following:
	//	1. Region set by users explicitly
	//	2. Env variable
	//	3. From IMDS
	//	4. Fail
	if region == "" {
		if defaultRegion != "" {
			region = defaultRegion
		} else {
			region, err = regionFromIMDS()
			if err != nil {
				return nil, status.Errorf(codes.InvalidArgument, "Failed to detect AWS Region using IMDS: %v, please explicitly set AWS Region", err)
			}
		}
	}

	if defaultRegion == "" {
		defaultRegion = region
	}

	return &MountCredentials{
		Region:        region,
		DefaultRegion: defaultRegion,
		StsEndpoints:  os.Getenv(stsEndpointsEnv),
		WebTokenPath:  hostTokenPath,
		AwsRoleArn:    awsRoleARN,
	}, nil
}

func (c *credentialProvider) findPodServiceAccountRole(ctx context.Context, req *csi.NodePublishVolumeRequest) (string, error) {
	podNamespace := req.VolumeContext[volumeCtxPodNamespace]
	podServiceAccount := req.VolumeContext[volumeCtxServiceAccountName]
	if podNamespace == "" || podServiceAccount == "" {
		klog.Error("`authenticationSource` configured to `pod` but no pod info found. Please make sure to enable `podInfoOnMountCompat`, see TODO")
		return "", status.Error(codes.InvalidArgument, "Missing Pod info")
	}

	response, err := c.client.ServiceAccounts(podNamespace).Get(ctx, podServiceAccount, metav1.GetOptions{})
	if err != nil {
		return "", status.Errorf(codes.InvalidArgument, "Failed to get pod's service account %s/%s: %v", podNamespace, podServiceAccount, err)
	}

	roleArn := response.Annotations[serviceAccountRoleAnnotation]
	if roleArn == "" {
		klog.Error("`authenticationSource` configured to `pod` but pod's service account is not annoated with a role, see TODO")
		return "", status.Errorf(codes.InvalidArgument, "Missing role annotation on pod's service account %s/%s", podNamespace, podServiceAccount)
	}

	return roleArn, nil
}

func hostPluginDirWithDefault() string {
	hostPluginDir := os.Getenv(hostPluginDirEnv)
	if hostPluginDir == "" {
		hostPluginDir = defaultHostPluginDir
	}
	return hostPluginDir
}

var regionFromIMDS = sync.OnceValues(func() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return "", fmt.Errorf("could not create config for imds client: %w", err)
	}

	client := imds.NewFromConfig(cfg)
	output, err := client.GetRegion(ctx, &imds.GetRegionInput{})
	if err != nil {
		return "", fmt.Errorf("failed to get region from imds: %w", err)
	}

	return output.Region, nil
})
