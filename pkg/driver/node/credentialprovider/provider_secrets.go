package credentialprovider

import (
	"context"
	"os"

	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/envprovider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

const (
	// The keys used to store credentials in the secret map provided as part of PV secret map in NodePublishVolumeRequest.
	// We are using the same keys as used by the AWS to accept access key and secret for driver level credentials.
	keyID           string = "key_id"
	secretAccessKey string = "access_key"
)

// provideFromSecrets provides secrets AWS credentials.
func (c *Provider) provideFromSecrets(ctx context.Context, provideCtx ProvideContext) (envprovider.Environment, error) {
	klog.V(4).Infof("credentialprovider: Using secrets from persistent volume secret map")
	env := envprovider.Environment{}

	region, _ := c.stsRegion(provideCtx)
	if region != "" {
		env.Set(envprovider.EnvRegion, region)
	}
	defaultRegion := os.Getenv(envprovider.EnvDefaultRegion)
	if defaultRegion == "" && region != "" {
		defaultRegion = region
		env.Set(envprovider.EnvDefaultRegion, defaultRegion)
	}

	keyId, hasKeyId := provideCtx.Secrets[keyID]
	secretAccessKey, hasSecretAccessKey := provideCtx.Secrets[secretAccessKey]
	if hasKeyId && hasSecretAccessKey {
		env.Set(envprovider.EnvAccessKeyID, keyId)
		env.Set(envprovider.EnvSecretAccessKey, secretAccessKey)
		return env, nil
	}
	return nil, status.Error(codes.InvalidArgument, "credentialprovider: Missing access key or secret access key in persistent volume secret map")
}
