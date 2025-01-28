package mounter

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
	"github.com/google/renameio"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/klog/v2"
	k8sstrings "k8s.io/utils/strings"

	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/envprovider"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/volumecontext"
	"github.com/awslabs/aws-s3-csi-driver/pkg/mountpoint"
)

const hostPluginDirEnv = "HOST_PLUGIN_DIR"

type AuthenticationSource = string

const (
	// This is when users don't provide a `authenticationSource` option in their volume attributes.
	// We're defaulting to `driver` in this case.
	AuthenticationSourceUnspecified AuthenticationSource = ""
	AuthenticationSourceDriver      AuthenticationSource = "driver"
	AuthenticationSourcePod         AuthenticationSource = "pod"
)

const (
	// This is to ensure only owner/group can read the file and no one else.
	serviceAccountTokenPerm = 0440
)

const defaultHostPluginDir = "/var/lib/kubelet/plugins/s3.csi.aws.com/"

const serviceAccountTokenAudienceSTS = "sts.amazonaws.com"

const serviceAccountRoleAnnotation = "eks.amazonaws.com/role-arn"

const podLevelCredentialsDocsPage = "https://github.com/awslabs/mountpoint-s3-csi-driver/blob/main/docs/CONFIGURATION.md#pod-level-credentials"
const stsConfigDocsPage = "https://github.com/awslabs/mountpoint-s3-csi-driver/blob/main/docs/CONFIGURATION.md#configuring-the-sts-region"

var errUnknownRegion = errors.New("NodePublishVolume: Pod-level: unknown region")

type Token struct {
	Token               string    `json:"token"`
	ExpirationTimestamp time.Time `json:"expirationTimestamp"`
}

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
func (c *CredentialProvider) Provide(ctx context.Context, volumeID string, volumeCtx map[string]string, args mountpoint.Args) (*MountCredentials, error) {
	if volumeCtx == nil {
		return nil, status.Error(codes.InvalidArgument, "Missing volume context")
	}

	authenticationSource := volumeCtx[volumecontext.AuthenticationSource]
	switch authenticationSource {
	case AuthenticationSourcePod:
		return c.provideFromPod(ctx, volumeID, volumeCtx, args)
	case AuthenticationSourceUnspecified, AuthenticationSourceDriver:
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
		AuthenticationSource: AuthenticationSourceDriver,
		AccessKeyID:          os.Getenv(envprovider.EnvAccessKeyID),
		SecretAccessKey:      os.Getenv(envprovider.EnvSecretAccessKey),
		SessionToken:         os.Getenv(envprovider.EnvSessionToken),
		Region:               os.Getenv(envprovider.EnvRegion),
		DefaultRegion:        os.Getenv(envprovider.EnvDefaultRegion),
		WebTokenPath:         hostTokenPath,
		StsEndpoints:         os.Getenv(envprovider.EnvSTSRegionalEndpoints),
		AwsRoleArn:           os.Getenv(envprovider.EnvRoleARN),
	}, nil
}

func (c *CredentialProvider) provideFromPod(ctx context.Context, volumeID string, volumeCtx map[string]string, args mountpoint.Args) (*MountCredentials, error) {
	klog.V(4).Infof("NodePublishVolume: Using pod identity")

	tokensJson := volumeCtx[volumecontext.CSIServiceAccountTokens]
	if tokensJson == "" {
		klog.Error("`authenticationSource` configured to `pod` but no service account tokens are received. Please make sure to enable `podInfoOnMountCompat`, see " + podLevelCredentialsDocsPage)
		return nil, status.Error(codes.InvalidArgument, "Missing service account tokens")
	}

	var tokens map[string]*Token
	if err := json.Unmarshal([]byte(tokensJson), &tokens); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "Failed to parse service account tokens: %v", err)
	}

	stsToken := tokens[serviceAccountTokenAudienceSTS]
	if stsToken == nil {
		klog.Errorf("`authenticationSource` configured to `pod` but no service account tokens for %s received. Please make sure to enable `podInfoOnMountCompat`, see "+podLevelCredentialsDocsPage, serviceAccountTokenAudienceSTS)
		return nil, status.Errorf(codes.InvalidArgument, "Missing service account token for %s", serviceAccountTokenAudienceSTS)
	}

	awsRoleARN, err := c.findPodServiceAccountRole(ctx, volumeCtx)
	if err != nil {
		return nil, err
	}

	region, err := c.stsRegion(volumeCtx, args)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "Failed to detect STS AWS Region, please explicitly set the AWS Region, see "+stsConfigDocsPage)
	}

	defaultRegion := os.Getenv(envprovider.EnvDefaultRegion)
	if defaultRegion == "" {
		defaultRegion = region
	}

	podID := volumeCtx[volumecontext.CSIPodUID]
	if podID == "" {
		return nil, status.Error(codes.InvalidArgument, "Missing Pod info. Please make sure to enable `podInfoOnMountCompat`, see "+podLevelCredentialsDocsPage)
	}

	err = c.writeToken(podID, volumeID, stsToken)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to write service account token: %v", err)
	}

	hostPluginDir := hostPluginDirWithDefault()
	hostTokenPath := path.Join(hostPluginDir, c.tokenFilename(podID, volumeID))

	podNamespace := volumeCtx[volumecontext.CSIPodNamespace]
	podServiceAccount := volumeCtx[volumecontext.CSIServiceAccountName]
	cacheKey := podNamespace + "/" + podServiceAccount

	return &MountCredentials{
		AuthenticationSource: AuthenticationSourcePod,

		Region:        region,
		DefaultRegion: defaultRegion,
		StsEndpoints:  os.Getenv(envprovider.EnvSTSRegionalEndpoints),
		WebTokenPath:  hostTokenPath,
		AwsRoleArn:    awsRoleARN,

		// Ensure to disable env credential provider
		AccessKeyID:     "",
		SecretAccessKey: "",

		// Ensure to disable profile provider
		ConfigFilePath:            path.Join(hostPluginDir, "disable-config"),
		SharedCredentialsFilePath: path.Join(hostPluginDir, "disable-credentials"),

		// Ensure to disable IMDS provider
		DisableIMDSProvider: true,

		MountpointCacheKey: cacheKey,
	}, nil
}

func (c *CredentialProvider) writeToken(podID string, volumeID string, token *Token) error {
	return renameio.WriteFile(c.tokenPathContainer(podID, volumeID), []byte(token.Token), serviceAccountTokenPerm)
}

func (c *CredentialProvider) tokenPathContainer(podID string, volumeID string) string {
	return path.Join(c.containerPluginDir, c.tokenFilename(podID, volumeID))
}

func (c *CredentialProvider) tokenFilename(podID string, volumeID string) string {
	var filename strings.Builder
	// `podID` is a UUID, but escape it to ensure it doesn't contain `/`
	filename.WriteString(k8sstrings.EscapeQualifiedName(podID))
	filename.WriteRune('-')
	// `volumeID` might contain `/`, we need to escape it
	filename.WriteString(k8sstrings.EscapeQualifiedName(volumeID))
	filename.WriteString(".token")
	return filename.String()
}

func (c *CredentialProvider) findPodServiceAccountRole(ctx context.Context, volumeCtx map[string]string) (string, error) {
	podNamespace := volumeCtx[volumecontext.CSIPodNamespace]
	podServiceAccount := volumeCtx[volumecontext.CSIServiceAccountName]
	if podNamespace == "" || podServiceAccount == "" {
		klog.Error("`authenticationSource` configured to `pod` but no pod info found. Please make sure to enable `podInfoOnMountCompat`, see " + podLevelCredentialsDocsPage)
		return "", status.Error(codes.InvalidArgument, "Missing Pod info. Please make sure to enable `podInfoOnMountCompat`, see "+podLevelCredentialsDocsPage)
	}

	response, err := c.client.ServiceAccounts(podNamespace).Get(ctx, podServiceAccount, metav1.GetOptions{})
	if err != nil {
		return "", status.Errorf(codes.InvalidArgument, "Failed to get pod's service account %s/%s: %v", podNamespace, podServiceAccount, err)
	}

	roleArn := response.Annotations[serviceAccountRoleAnnotation]
	if roleArn == "" {
		klog.Error("`authenticationSource` configured to `pod` but pod's service account is not annotated with a role, see " + podLevelCredentialsDocsPage)
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
func (c *CredentialProvider) stsRegion(volumeCtx map[string]string, args mountpoint.Args) (string, error) {
	region := volumeCtx[volumecontext.STSRegion]
	if region != "" {
		klog.V(5).Infof("NodePublishVolume: Pod-level: Detected STS region %s from volume context", region)
		return region, nil
	}

	if region, ok := args.Value(mountpoint.ArgRegion); ok {
		klog.V(5).Infof("NodePublishVolume: Pod-level: Detected STS region %s from S3 bucket region", region)
		return region, nil
	}

	region = os.Getenv(envprovider.EnvRegion)
	if region != "" {
		klog.V(5).Infof("NodePublishVolume: Pod-level: Detected STS region %s from `AWS_REGION` env variable", region)
		return region, nil
	}

	region = os.Getenv(envprovider.EnvDefaultRegion)
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
