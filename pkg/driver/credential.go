package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/klog/v2"
	k8sstrings "k8s.io/utils/strings"
)

type authenticationSource = string

const (
	// This is when users don't provide a `authenticationSource` option in their volume attributes.
	// We're defaulting to `driver` in this case.
	authenticationSourceUnspecified authenticationSource = ""
	authenticationSourceDriver      authenticationSource = "driver"
	authenticationSourcePod         authenticationSource = "pod"
)

const (
	// This is to ensure only owner/group can read the file and no one else.
	serviceAccountTokenPerm = 0440
)

const defaultHostPluginDir = "/var/lib/kubelet/plugins/s3.csi.aws.com/"

const serviceAccountTokenAudienceSTS = "sts.amazonaws.com"

const serviceAccountRoleAnnotation = "eks.amazonaws.com/role-arn"

var errUnknownRegion = errors.New("NodePublishVolume: Pod-level: unknown region")

type CredentialProvider struct {
	client             k8sv1.CoreV1Interface
	containerPluginDir string
	regionFromIMDS     func() (string, error)
}

func NewCredentialProvider(client k8sv1.CoreV1Interface, containerPluginDir string, regionFromIMDS func() (string, error)) *CredentialProvider {
	// `regionFromIMDS` is a `sync.OnceValues` and it only makes request to IMDS once,
	// this call is basically here to pre-warm the cache of IMDS call.
	go func() {
		_, _ = regionFromIMDS()
	}()

	return &CredentialProvider{client, containerPluginDir, regionFromIMDS}
}

// CleanupToken cleans any created service token files for given volume and pod.
func (c *CredentialProvider) CleanupToken(volumeID string, podID string) error {
	err := os.Remove(c.tokenPathContainer(podID, volumeID))
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}

// Provide provides mount credentials for given volume and volume context.
// Depending on the configuration, it either returns driver-level or pod-level credentials.
func (c *CredentialProvider) Provide(ctx context.Context, volumeID string, volumeContext map[string]string, mountpointArgs []string) (*MountCredentials, error) {
	if volumeContext == nil {
		return nil, status.Error(codes.InvalidArgument, "Missing volume context")
	}

	authenticationSource := volumeContext[volumeCtxAuthenticationSource]
	switch authenticationSource {
	case authenticationSourcePod:
		return c.provideFromPod(ctx, volumeID, volumeContext, mountpointArgs)
	case authenticationSourceUnspecified, authenticationSourceDriver:
		return c.provideFromDriver()
	default:
		return nil, fmt.Errorf("unknown `authenticationSource`: %s, only `driver` (default option if not specified) and `pod` supported", authenticationSource)
	}
}

func (c *CredentialProvider) provideFromDriver() (*MountCredentials, error) {
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

func (c *CredentialProvider) provideFromPod(ctx context.Context, volumeID string, volumeContext map[string]string, mountpointArgs []string) (*MountCredentials, error) {
	klog.V(4).Infof("NodePublishVolume: Using pod identity")

	tokensJson := volumeContext[volumeCtxServiceAccountTokens]
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

	awsRoleARN, err := c.findPodServiceAccountRole(ctx, volumeContext)
	if err != nil {
		return nil, err
	}

	region, err := c.stsRegion(volumeContext, mountpointArgs)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "Failed to detect STS AWS Region, please explicitly set the AWS Region, see TODO")
	}

	defaultRegion := os.Getenv(defaultRegionEnv)
	if defaultRegion == "" {
		defaultRegion = region
	}

	podID := volumeContext[volumeCtxPodUID]
	if podID == "" {
		return nil, status.Error(codes.InvalidArgument, "Missing Pod info")
	}

	err = c.writeToken(podID, volumeID, stsToken)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to write service account token: %v", err)
	}

	hostPluginDir := hostPluginDirWithDefault()
	hostTokenPath := path.Join(hostPluginDir, c.tokenFilename(podID, volumeID))

	return &MountCredentials{
		Region:        region,
		DefaultRegion: defaultRegion,
		StsEndpoints:  os.Getenv(stsEndpointsEnv),
		WebTokenPath:  hostTokenPath,
		AwsRoleArn:    awsRoleARN,
	}, nil
}

func (c *CredentialProvider) writeToken(podID string, volumeID string, token *Token) error {
	return WriteFileAtomic(c.tokenPathContainer(podID, volumeID), []byte(token.Token), serviceAccountTokenPerm)
}

func (c *CredentialProvider) tokenPathContainer(podID string, volumeID string) string {
	return path.Join(c.containerPluginDir, c.tokenFilename(podID, volumeID))
}

func (c *CredentialProvider) tokenFilename(podID string, volumeID string) string {
	var filename strings.Builder
	filename.WriteString(podID)
	filename.WriteRune('-')
	// `volumeID` might contain `/`, we need to escape it
	filename.WriteString(k8sstrings.EscapeQualifiedName(volumeID))
	filename.WriteString(".token")
	return filename.String()
}

func (c *CredentialProvider) findPodServiceAccountRole(ctx context.Context, volumeContext map[string]string) (string, error) {
	podNamespace := volumeContext[volumeCtxPodNamespace]
	podServiceAccount := volumeContext[volumeCtxServiceAccountName]
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
		klog.Error("`authenticationSource` configured to `pod` but pod's service account is not annotated with a role, see TODO")
		return "", status.Errorf(codes.InvalidArgument, "Missing role annotation on pod's service account %s/%s", podNamespace, podServiceAccount)
	}

	return roleArn, nil
}

// stsRegion tries to detect AWS region to use for STS.
//
// It looks for the following (in-order):
//  1. `stsRegion` passed via volume context
//  2. Region set for S3 bucket via mount options
//  3. `AWS_REGION` or `AWS_DEFAULT_REGION` env variables
//  4. Calling IMDS to detect region
//
// It returns an error if all of them fails.
func (c *CredentialProvider) stsRegion(volumeContext map[string]string, mountpointArgs []string) (string, error) {
	region := volumeContext[volumeCtxSTSRegion]
	if region != "" {
		klog.V(5).Infof("NodePublishVolume: Pod-level: Detected STS region %s from volume context", region)
		return region, nil
	}

	for _, arg := range mountpointArgs {
		// we normalize all `mountpointArgs` to have `--arg=val` in `S3NodeServer.NodePublishVolume`
		if strings.HasPrefix(arg, "--region=") {
			region = strings.SplitN(arg, "=", 2)[1]
			klog.V(5).Infof("NodePublishVolume: Pod-level: Detected STS region %s from S3 bucket region", region)
			return region, nil
		}
	}

	region = os.Getenv(regionEnv)
	if region != "" {
		klog.V(5).Infof("NodePublishVolume: Pod-level: Detected STS region %s from `AWS_REGION` env variable", region)
		return region, nil
	}

	region = os.Getenv(defaultRegionEnv)
	if region != "" {
		klog.V(5).Infof("NodePublishVolume: Pod-level: Detected STS region %s from `AWS_DEFAULT_REGION` env variable", region)
		return region, nil
	}

	// We're ignoring the error here, makes a call to IMDS only once and logs the error in case of error
	region, _ = c.regionFromIMDS()
	if region != "" {
		klog.V(5).Infof("NodePublishVolume: Pod-level: Detected STS region %s from IMDS", region)
		return region, nil
	}

	return "", errUnknownRegion
}

func hostPluginDirWithDefault() string {
	hostPluginDir := os.Getenv(hostPluginDirEnv)
	if hostPluginDir == "" {
		hostPluginDir = defaultHostPluginDir
	}
	return hostPluginDir
}

// RegionFromIMDSOnce tries to detect AWS region by making a request to IMDS.
// It only makes request to IMDS once and caches the value.
var RegionFromIMDSOnce = sync.OnceValues(func() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		klog.V(5).Infof("NodePublishVolume: Pod-level: Failed to create config for IMDS client: %v", err)
		return "", fmt.Errorf("could not create config for imds client: %w", err)
	}

	client := imds.NewFromConfig(cfg)
	output, err := client.GetRegion(ctx, &imds.GetRegionInput{})
	if err != nil {
		klog.V(5).Infof("NodePublishVolume: Pod-level: Failed to get region from IMDS: %v", err)
		return "", fmt.Errorf("failed to get region from imds: %w", err)
	}

	return output.Region, nil
})
