package credentialprovider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/google/renameio"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/envprovider"
)

const (
	serviceAccountTokenAudienceSTS         = "sts.amazonaws.com"
	serviceAccountTokenAudiencePodIdentity = "pods.eks.amazonaws.com"
	serviceAccountRoleAnnotation           = "eks.amazonaws.com/role-arn"
)

const podLevelCredentialsDocsPage = "https://github.com/awslabs/mountpoint-s3-csi-driver/blob/main/docs/CONFIGURATION.md#pod-level-credentials"
const stsConfigDocsPage = "https://github.com/awslabs/mountpoint-s3-csi-driver/blob/main/docs/CONFIGURATION.md#configuring-the-sts-region"

type serviceAccountToken struct {
	Token               string    `json:"token"`
	ExpirationTimestamp time.Time `json:"expirationTimestamp"`
}

// provideFromPod provides pod-level AWS credentials.
func (c *Provider) provideFromPod(ctx context.Context, provideCtx ProvideContext) (envprovider.Environment, error) {
	klog.V(4).Infof("credentialprovider: Using pod identity and %s mount kind", provideCtx.MountKind)

	podID := provideCtx.GetCredentialPodID()
	if podID == "" {
		return nil, status.Error(codes.InvalidArgument, "Missing Pod info. Please make sure to enable `podInfoOnMountCompat`, see "+podLevelCredentialsDocsPage)
	}

	// 1. Parse ServiceAccountTokens map
	tokensJson := provideCtx.ServiceAccountTokens
	if tokensJson == "" {
		klog.Error("credentialprovider: `authenticationSource` configured to `pod` but no service account tokens are received. Please make sure to enable `podInfoOnMountCompat`, see " + podLevelCredentialsDocsPage)
		return nil, status.Error(codes.InvalidArgument, "Missing service account tokens. Please make sure to enable `podInfoOnMountCompat`, see "+podLevelCredentialsDocsPage)
	}

	var tokens map[string]*serviceAccountToken
	if err := json.Unmarshal([]byte(tokensJson), &tokens); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "Failed to parse service account tokens: %v", err)
	}

	stsToken := tokens[serviceAccountTokenAudienceSTS]
	if stsToken == nil {
		klog.Errorf("credentialprovider: `authenticationSource` configured to `pod` but no service account token for %s received. Please make sure to enable `podInfoOnMountCompat`, see "+podLevelCredentialsDocsPage, serviceAccountTokenAudienceSTS)
		return nil, status.Errorf(codes.InvalidArgument, "Missing service account token for %s", serviceAccountTokenAudienceSTS)
	}

	eksToken := tokens[serviceAccountTokenAudiencePodIdentity]
	if provideCtx.IsPodMountpoint() && eksToken == nil {
		klog.Errorf("credentialprovider: `authenticationSource` configured to `pod` but no service account token for %s received. Please make sure to enable `podInfoOnMountCompat`, see "+podLevelCredentialsDocsPage, serviceAccountTokenAudiencePodIdentity)
		return nil, status.Errorf(codes.InvalidArgument, "Missing service account token for %s", serviceAccountTokenAudiencePodIdentity)
	}

	// 2. Create environment to be returned with common variables (used in both cases: IRSA and EKS PI)
	podNamespace := provideCtx.PodNamespace
	podServiceAccount := provideCtx.ServiceAccountName

	env := envprovider.Environment{
		envprovider.EnvEC2MetadataDisabled: "true",
	}

	if provideCtx.IsSystemDMountpoint() {
		cacheKey := podNamespace + "/" + podServiceAccount
		// This is only needed for `SystemdMounter` to ensure cache folders are not shared accidentally,
		// with `PodMounter`, cache folders are unique to the Mountpoint Pods.
		env[envprovider.EnvMountpointCacheKey] = cacheKey
		env[envprovider.EnvConfigFile] = filepath.Join(provideCtx.EnvPath, "disable-config")
		env[envprovider.EnvSharedCredentialsFile] = filepath.Join(provideCtx.EnvPath, "disable-credentials")
	}

	// 3. Provide credentials with IRSA. If not configured, provide credentials with EKS Pod Identity instead.
	irsaCredentialsEnvironment, irsaCredentialsEnvironmentError := c.createIRSACredentialsEnvironment(ctx, provideCtx)
	if irsaCredentialsEnvironmentError == nil {
		klog.V(4).Infof("Providing credentials from pod with STS Web Identity provider (IRSA)")

		// Copy STS Token file to WritePath
		tokenName := podLevelSTSWebIdentityServiceAccountTokenName(podID, provideCtx.VolumeID)
		err := renameio.WriteFile(filepath.Join(provideCtx.WritePath, tokenName), []byte(stsToken.Token), CredentialFilePerm)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "Failed to write service account STS token: %v", err)
		}

		env.Merge(irsaCredentialsEnvironment)
		return env, nil
	} else if errors.Is(irsaCredentialsEnvironmentError, errMissingServiceAccountAnnotationForIRSA) {
		if provideCtx.IsSystemDMountpoint() {
			// SystemD mounts do not support EKS Pod Identity, hence we are returning error here if there is missing IRSA role annotation
			return nil, status.Errorf(codes.InvalidArgument, "Missing role annotation on pod's service account %s/%s", podNamespace, podServiceAccount)
		}

		klog.V(4).Infof("Providing credentials from pod with Container credential provider (EKS Pod Identity)")
		eksPodIdentityCredentialsEnvironment, eksPodIdentityCredentialsEnvironmentError := c.createEKSPodIdentityCredentialsEnvironment(provideCtx)

		if eksPodIdentityCredentialsEnvironmentError == nil {
			// Copy EKS Token file to WritePath
			tokenNameEKS := podLevelEksPodIdentityServiceAccountTokenName(podID, provideCtx.VolumeID)
			err := renameio.WriteFile(filepath.Join(provideCtx.WritePath, tokenNameEKS), []byte(eksToken.Token), CredentialFilePerm)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "Failed to write service account EKS Pod Identity token: %v", err)
			}

			env.Merge(eksPodIdentityCredentialsEnvironment)
			return env, nil
		} else {
			klog.V(4).Infof("Error providing credentials from pod Container credential provider (EKS Pod Identity)")
			return nil, eksPodIdentityCredentialsEnvironmentError
		}
	}

	klog.V(4).Infof("Error providing credentials from pod with STS Web Identity provider (IRSA)")
	return nil, irsaCredentialsEnvironmentError
}

// cleanupFromPod removes any credential files that were created for pod-level authentication via [Provider.provideFromPod].
func (c *Provider) cleanupFromPod(cleanupCtx CleanupContext) error {
	tokenNameSTS := podLevelSTSWebIdentityServiceAccountTokenName(cleanupCtx.PodID, cleanupCtx.VolumeID)
	errSTS := c.cleanupToken(cleanupCtx.WritePath, tokenNameSTS)
	if errSTS != nil {
		errSTS = status.Errorf(codes.Internal, "Failed to cleanup service account STS token: %v", errSTS)
	}

	tokenNameEKS := podLevelEksPodIdentityServiceAccountTokenName(cleanupCtx.PodID, cleanupCtx.VolumeID)
	errEKS := c.cleanupToken(cleanupCtx.WritePath, tokenNameEKS)
	if errEKS != nil {
		errEKS = status.Errorf(codes.Internal, "Failed to cleanup service account EKS Pod Identity token: %v", errEKS)
	}

	return errors.Join(errSTS, errEKS)
}

var errMissingServiceAccountAnnotationForIRSA = errors.New("Missing role annotation on pod's service account")

// findPodServiceAccountRole tries to provide associated AWS IAM role for service account specified in the volume context.
func (c *Provider) findPodServiceAccountRole(ctx context.Context, provideCtx ProvideContext) (string, error) {
	podNamespace := provideCtx.PodNamespace
	podServiceAccount := provideCtx.ServiceAccountName

	// In PodMounter we get IAM Role ARN from MountpointS3PodAttachment custom resource
	if provideCtx.IsPodMountpoint() {
		if provideCtx.ServiceAccountEKSRoleARN != "" {
			return provideCtx.ServiceAccountEKSRoleARN, nil
		} else {
			return "", errMissingServiceAccountAnnotationForIRSA
		}
	}

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
		return "", errMissingServiceAccountAnnotationForIRSA
	}

	return roleArn, nil
}

// podLevelSTSWebIdentityServiceAccountTokenName returns service account token name for Pod-level identity.
// It escapes from slashes to make this token name path-safe.
func podLevelSTSWebIdentityServiceAccountTokenName(podID string, volumeID string) string {
	id := escapedVolumeIdentifier(podID, volumeID)
	return id + ".token"
}

// podLevelEksPodIdentityServiceAccountTokenName returns service account token name for Pod-level identity with EKS Pod Identity.
// It escapes from slashes to make this token name path-safe.
func podLevelEksPodIdentityServiceAccountTokenName(podID string, volumeID string) string {
	id := escapedVolumeIdentifier(podID, volumeID)
	return id + "-eks-pod-identity.token"
}

// createIRSACredentialsEnvironment creates an environment with the environment variables needed for pod-level authentication with IRSA
func (c *Provider) createIRSACredentialsEnvironment(ctx context.Context, provideCtx ProvideContext) (envprovider.Environment, error) {
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

	podID := provideCtx.GetCredentialPodID()
	tokenName := podLevelSTSWebIdentityServiceAccountTokenName(podID, provideCtx.VolumeID)
	tokenFile := filepath.Join(provideCtx.EnvPath, tokenName)

	return envprovider.Environment{
		envprovider.EnvRoleARN:              roleARN,
		envprovider.EnvWebIdentityTokenFile: tokenFile,
		envprovider.EnvRegion:               region,
		envprovider.EnvDefaultRegion:        defaultRegion,
	}, nil
}

// createEKSPodIdentityCredentialsEnvironment creates an environment with the environment variables needed for pod-level authentication with EKS Pod Identity
func (c *Provider) createEKSPodIdentityCredentialsEnvironment(provideCtx ProvideContext) (envprovider.Environment, error) {
	podID := provideCtx.GetCredentialPodID()
	tokenName := podLevelEksPodIdentityServiceAccountTokenName(podID, provideCtx.VolumeID)
	tokenFile := filepath.Join(provideCtx.EnvPath, tokenName)

	eksPodIdentityAgentCredentialsURI := os.Getenv("EKS_POD_IDENTITY_AGENT_CONTAINER_CREDENTIALS_FULL_URI")
	if eksPodIdentityAgentCredentialsURI == "" {
		klog.Warningf("credentialprovider: Seems like EKS Pod Identity is disabled. If you would like to enable it, please provide the eksPodIdentityAgent.containerCredentialsFullURI Helm value.")
		return nil, status.Errorf(codes.InvalidArgument, "Failed to detect EKS_POD_IDENTITY_AGENT_CONTAINER_CREDENTIALS_FULL_URI driver configuration flag")
	}

	return envprovider.Environment{
		envprovider.EnvContainerCredentialsFullURI:     eksPodIdentityAgentCredentialsURI,
		envprovider.EnvContainerAuthorizationTokenFile: tokenFile,
	}, nil
}
