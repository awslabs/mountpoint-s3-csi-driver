// Package credentialprovider provides utilities for obtaining AWS credentials to use.
// Depending on the configuration, it either uses Pod-level or Driver-level credentials.
package credentialprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/klog/v2"

	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/volumecontext"
)

// An AuthenticationSource represents the source where the credentials was obtained.
type AuthenticationSource = string

const (
	// This is when users don't provide a `authenticationSource` option in their volume attributes.
	// We're defaulting to `driver` in this case.
	AuthenticationSourceUnspecified AuthenticationSource = ""
	AuthenticationSourceDriver      AuthenticationSource = "driver"
	AuthenticationSourcePod         AuthenticationSource = "pod"
)

const (
	envAccessKeyID           = "AWS_ACCESS_KEY_ID"
	envSecretAccessKey       = "AWS_SECRET_ACCESS_KEY"
	envSessionToken          = "AWS_SESSION_TOKEN"
	envConfigFile            = "AWS_CONFIG_FILE"
	envSharedCredentialsFile = "AWS_SHARED_CREDENTIALS_FILE"
	envRoleARN               = "AWS_ROLE_ARN"
	envWebIdentityTokenFile  = "AWS_WEB_IDENTITY_TOKEN_FILE"
)

const (
	serviceAccountTokenAudienceSTS = "sts.amazonaws.com"

	serviceAccountRoleAnnotation = "eks.amazonaws.com/role-arn"
)

const podLevelCredentialsDocsPage = "https://github.com/awslabs/mountpoint-s3-csi-driver/blob/main/docs/CONFIGURATION.md#pod-level-credentials"

type serviceAccountToken struct {
	Token               string    `json:"token"`
	ExpirationTimestamp time.Time `json:"expirationTimestamp"`
}

// A Provider provides methods for accessing AWS credentials.
type Provider struct {
	client k8sv1.CoreV1Interface
}

// New creates a new [Provider] with given client.
func New(client k8sv1.CoreV1Interface) *Provider {
	return &Provider{client}
}

// Provide provides credentials for given volume context.
// Depending on the configuration, it either returns driver-level or pod-level credentials.
func (c *Provider) Provide(ctx context.Context, volumeContext map[string]string) (Credentials, error) {
	if volumeContext == nil {
		return nil, status.Error(codes.InvalidArgument, "Missing volume context")
	}

	authenticationSource := volumeContext[volumecontext.AuthenticationSource]
	switch authenticationSource {
	case AuthenticationSourcePod:
		return c.provideFromPod(ctx, volumeContext)
	case AuthenticationSourceUnspecified, AuthenticationSourceDriver:
		return c.provideFromDriver()
	default:
		return nil, fmt.Errorf("unknown `authenticationSource`: %s, only `driver` (default option if not specified) and `pod` supported", authenticationSource)
	}
}

// provideFromDriver provides driver-level AWS credentials.
func (c *Provider) provideFromDriver() (Credentials, error) {
	klog.V(4).Infof("credentialprovider: Using driver identity")

	source := AuthenticationSourceDriver
	var credentials []Credentials

	// Long-term AWS credentials
	accessKeyID := os.Getenv(envAccessKeyID)
	secretAccessKey := os.Getenv(envSecretAccessKey)
	if accessKeyID != "" && secretAccessKey != "" {
		sessionToken := os.Getenv(envSessionToken)
		credentials = append(credentials, &longTermCredentials{
			source,
			accessKeyID,
			secretAccessKey,
			sessionToken,
		})
	} else {
		// Profile provider
		// TODO: This is not officially supported and won't work by default with containerization. Should we remove it?
		configFile := os.Getenv(envConfigFile)
		sharedCredentialsFile := os.Getenv(envSharedCredentialsFile)
		if configFile != "" && sharedCredentialsFile != "" {
			credentials = append(credentials, &sharedProfileCredentials{
				source,
				configFile,
				sharedCredentialsFile,
			})
		}
	}

	// STS Web Identity provider
	webIdentityTokenFile := os.Getenv(envWebIdentityTokenFile)
	roleARN := os.Getenv(envRoleARN)
	if webIdentityTokenFile != "" && roleARN != "" {
		credentials = append(credentials, &stsWebIdentityCredentials{
			source:               source,
			roleARN:              roleARN,
			webIdentityTokenFile: webIdentityTokenFile,
		})
	}

	// Here we don't return an error even `credentials` are empty, because there might be Instance Profile Role
	// configured and Mountpoint/CRT would fallback to that if we just return empty credentials/environment-variables.
	return &multiCredentials{source: source, credentials: credentials}, nil
}

// provideFromPod provides pod-level AWS credentials.
func (c *Provider) provideFromPod(ctx context.Context, volumeContext map[string]string) (Credentials, error) {
	klog.V(4).Infof("credentialprovider: Using pod identity")

	tokensJson := volumeContext[volumecontext.CSIServiceAccountTokens]
	if tokensJson == "" {
		klog.Error("`authenticationSource` configured to `pod` but no service account tokens are received. Please make sure to enable `podInfoOnMountCompat`, see " + podLevelCredentialsDocsPage)
		return nil, status.Error(codes.InvalidArgument, "Missing service account tokens. Please make sure to enable `podInfoOnMountCompat`, see "+podLevelCredentialsDocsPage)
	}

	var tokens map[string]*serviceAccountToken
	if err := json.Unmarshal([]byte(tokensJson), &tokens); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "Failed to parse service account tokens: %v", err)
	}

	stsToken := tokens[serviceAccountTokenAudienceSTS]
	if stsToken == nil {
		klog.Errorf("`authenticationSource` configured to `pod` but no service account tokens for %s received. Please make sure to enable `podInfoOnMountCompat`, see "+podLevelCredentialsDocsPage, serviceAccountTokenAudienceSTS)
		return nil, status.Errorf(codes.InvalidArgument, "Missing service account token for %s", serviceAccountTokenAudienceSTS)
	}

	roleARN, err := c.findPodServiceAccountRole(ctx, volumeContext)
	if err != nil {
		return nil, err
	}

	podNamespace := volumeContext[volumecontext.CSIPodNamespace]
	podServiceAccount := volumeContext[volumecontext.CSIServiceAccountName]
	cacheKey := podNamespace + "/" + podServiceAccount

	return &stsWebIdentityCredentials{
		source:           AuthenticationSourcePod,
		webIdentityToken: stsToken.Token,
		roleARN:          roleARN,
		cacheKey:         cacheKey,
	}, nil
}

// findPodServiceAccountRole tries to provide associated AWS IAM role for service account specified in the volume context.
func (c *Provider) findPodServiceAccountRole(ctx context.Context, volumeContext map[string]string) (string, error) {
	podNamespace := volumeContext[volumecontext.CSIPodNamespace]
	podServiceAccount := volumeContext[volumecontext.CSIServiceAccountName]
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
