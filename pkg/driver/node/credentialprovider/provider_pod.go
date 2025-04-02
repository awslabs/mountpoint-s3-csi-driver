package credentialprovider

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/google/renameio"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/envprovider"
)

const (
	serviceAccountTokenAudienceSTS         = "sts.amazonaws.com"
	serviceAccountTokenAudiencePodIdentity = "pods.eks.amazonaws.com"
	serviceAccountRoleAnnotation           = "eks.amazonaws.com/role-arn"
	podIdentityCredURI                     = "http://169.254.170.23/v1/credentials"
)

const podLevelCredentialsDocsPage = "https://github.com/awslabs/mountpoint-s3-csi-driver/blob/main/docs/CONFIGURATION.md#pod-level-credentials"
const stsConfigDocsPage = "https://github.com/awslabs/mountpoint-s3-csi-driver/blob/main/docs/CONFIGURATION.md#configuring-the-sts-region"

type serviceAccountToken struct {
	Token               string    `json:"token"`
	ExpirationTimestamp time.Time `json:"expirationTimestamp"`
}

// provideFromPod provides pod-level AWS credentials.
func (c *Provider) provideFromPod(ctx context.Context, provideCtx ProvideContext) (envprovider.Environment, error) {
	klog.V(4).Infof("credentialprovider: Using pod identity")

	tokensJson := provideCtx.ServiceAccountTokens
	if tokensJson == "" {
		klog.Error("credentialprovider: `authenticationSource` configured to `pod` but no service account tokens are received. Please make sure to enable `podInfoOnMountCompat`, see " + podLevelCredentialsDocsPage)
		return nil, status.Error(codes.InvalidArgument, "Missing service account tokens. Please make sure to enable `podInfoOnMountCompat`, see "+podLevelCredentialsDocsPage)
	}

	var tokens map[string]*serviceAccountToken
	if err := json.Unmarshal([]byte(tokensJson), &tokens); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "Failed to parse service account tokens: %v", err)
	}

	podID := provideCtx.PodID
	volumeID := provideCtx.VolumeID
	if podID == "" {
		return nil, status.Error(codes.InvalidArgument, "Missing Pod info. Please make sure to enable `podInfoOnMountCompat`, see "+podLevelCredentialsDocsPage)
	}

	stsToken := tokens[serviceAccountTokenAudienceSTS]
	if stsToken == nil {
		klog.Errorf("credentialprovider: `authenticationSource` configured to `pod` but no service account tokens for %s received. Please make sure to enable `podInfoOnMountCompat`, see "+podLevelCredentialsDocsPage, serviceAccountTokenAudienceSTS)
		return nil, status.Errorf(codes.InvalidArgument, "Missing service account token for %s", serviceAccountTokenAudienceSTS)
	}

	tokenName := podLevelServiceAccountTokenName(podID, volumeID)
	err := renameio.WriteFile(filepath.Join(provideCtx.WritePath, tokenName), []byte(stsToken.Token), CredentialFilePerm)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to write service account STS token: %v", err)
	}
	tokenFile := filepath.Join(provideCtx.EnvPath, tokenName)

	eksToken := tokens[serviceAccountTokenAudiencePodIdentity]
	if eksToken == nil {
		klog.Errorf("credentialprovider: `authenticationSource` configured to `pod` but no service account tokens for %s received. Please make sure to enable `podInfoOnMountCompat`, see "+podLevelCredentialsDocsPage, serviceAccountTokenAudiencePodIdentity)
		return nil, status.Errorf(codes.InvalidArgument, "Missing service account token for %s", serviceAccountTokenAudiencePodIdentity)
	}

	tokenNameEKS := eksPodIdentityServiceAccountTokenName(podID, volumeID)
	err = renameio.WriteFile(filepath.Join(provideCtx.WritePath, tokenNameEKS), []byte(eksToken.Token), CredentialFilePerm)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to write service account EKS Pod Identity token: %v", err)
	}
	tokenFileEKS := filepath.Join(provideCtx.EnvPath, tokenNameEKS)

	podNamespace := provideCtx.PodNamespace
	podServiceAccount := provideCtx.ServiceAccountName
	cacheKey := podNamespace + "/" + podServiceAccount

	irsaCredentialsEnvironment, irsaCredentialsEnvironmentError := c.createIRSACredentialsEnvironment(ctx, provideCtx, tokenFile)
	eksPodIdentityCredentialsEnvironment, eksPodIdentityCredentialsEnvironmentError := c.createEKSPodIdentityCredentialsEnvironment(tokenFileEKS)

	if irsaCredentialsEnvironmentError != nil && eksPodIdentityCredentialsEnvironmentError != nil {
		klog.Error("IRSA and EKS Pod Identity failed")                                                                                                                    // TODO: Improve error message
		return nil, status.Errorf(codes.Internal, "IRSA and EKS Pod Identity failed: %v, %v", irsaCredentialsEnvironmentError, eksPodIdentityCredentialsEnvironmentError) // TODO: Improve error message
	}

	env := envprovider.Environment{
		envprovider.EnvEC2MetadataDisabled: "true",

		// TODO: These were needed with `systemd` but probably won't be necessary with containerization.
		envprovider.EnvMountpointCacheKey:    cacheKey,
		envprovider.EnvConfigFile:            filepath.Join(provideCtx.EnvPath, "disable-config"),
		envprovider.EnvSharedCredentialsFile: filepath.Join(provideCtx.EnvPath, "disable-credentials"),
	}

	if irsaCredentialsEnvironmentError != nil {
		klog.Error("irsaCredentialsEnvironmentError") // TODO: Improve error message
	} else {
		env.Merge(irsaCredentialsEnvironment)
	}

	if eksPodIdentityCredentialsEnvironmentError != nil {
		klog.Error("eksPodIdentityCredentialsEnvironmentError") // TODO: Improve error message
	} else {
		env.Merge(eksPodIdentityCredentialsEnvironment)
	}

	return env, nil
}

// cleanupFromPod removes any credential files that were created for pod-level authentication authentication via [Provider.provideFromPod].
func (c *Provider) cleanupFromPod(cleanupCtx CleanupContext) error {
	tokenName := podLevelServiceAccountTokenName(cleanupCtx.PodID, cleanupCtx.VolumeID)
	tokenPath := filepath.Join(cleanupCtx.WritePath, tokenName)
	err := os.Remove(tokenPath)
	if err != nil && errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// findPodServiceAccountRole tries to provide associated AWS IAM role for service account specified in the volume context.
func (c *Provider) findPodServiceAccountRole(ctx context.Context, provideCtx ProvideContext) (string, error) {
	podNamespace := provideCtx.PodNamespace
	podServiceAccount := provideCtx.ServiceAccountName
	if podNamespace == "" || podServiceAccount == "" {
		klog.Error("credentialprovider: `authenticationSource` configured to `pod` but no pod info found. Please make sure to enable `podInfoOnMountCompat`, see " + podLevelCredentialsDocsPage)
		return "", status.Error(codes.InvalidArgument, "Missing Pod info. Please make sure to enable `podInfoOnMountCompat`, see "+podLevelCredentialsDocsPage)
	}

	response, err := c.client.ServiceAccounts(podNamespace).Get(ctx, podServiceAccount, metav1.GetOptions{})
	if err != nil {
		return "", status.Errorf(codes.InvalidArgument, "Failed to get pod's service account %s/%s: %v", podNamespace, podServiceAccount, err)
	}

	roleArn := response.Annotations[serviceAccountRoleAnnotation]
	if roleArn == "" {
		klog.Error("credentialprovider: `authenticationSource` configured to `pod` but pod's service account is not annotated with a role, see " + podLevelCredentialsDocsPage)
		return "", status.Errorf(codes.InvalidArgument, "Missing role annotation on pod's service account %s/%s", podNamespace, podServiceAccount)
	}

	return roleArn, nil
}

// podLevelServiceAccountTokenName returns service account token name for Pod-level identity.
// It escapes from slashes to make this token name path-safe.
func podLevelServiceAccountTokenName(podID string, volumeID string) string { // TODO: Consider reusability with eksPodIdentityServiceAccountTokenName or at least renaming this one.
	id := escapedVolumeIdentifier(podID, volumeID)
	return id + ".token"
}

// eksPodIdentityServiceAccountTokenName returns service account token name for Pod-level identity with EKS Pod Identity.
// It escapes from slashes to make this token name path-safe.
func eksPodIdentityServiceAccountTokenName(podID string, volumeID string) string {
	id := escapedVolumeIdentifier(podID, volumeID)
	return id + "-eks-pod-identity.token"
}

// createIRSACredentialsEnvironment creates an environment with the environment variables needed for pod-level authentication with IRSA
func (c *Provider) createIRSACredentialsEnvironment(ctx context.Context, provideCtx ProvideContext, tokenFile string) (envprovider.Environment, error) {
	roleARN, err := c.findPodServiceAccountRole(ctx, provideCtx)
	if err != nil {
		return nil, err
	}

	region, err := c.stsRegion(provideCtx)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "Failed to detect STS AWS Region, please explicitly set the AWS Region, see "+stsConfigDocsPage)
	}

	defaultRegion := os.Getenv(envprovider.EnvDefaultRegion)
	if defaultRegion == "" {
		defaultRegion = region
	}

	return envprovider.Environment{
		envprovider.EnvRoleARN:              roleARN,
		envprovider.EnvWebIdentityTokenFile: tokenFile,
		envprovider.EnvRegion:               region,
		envprovider.EnvDefaultRegion:        defaultRegion,
	}, nil
}

// createEKSPodIdentityCredentialsEnvironment creates an environment with the environment variables needed for pod-level authentication with EKS Pod Identity
func (c *Provider) createEKSPodIdentityCredentialsEnvironment(tokenFile string) (envprovider.Environment, error) {
	return envprovider.Environment{
		envprovider.EnvPodIdentityCredURI:   podIdentityCredURI,
		envprovider.EnvPodIdentityTokenFile: tokenFile,
	}, nil
}
