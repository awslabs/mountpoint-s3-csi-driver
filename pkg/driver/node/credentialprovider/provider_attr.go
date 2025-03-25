package credentialprovider

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"

	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/envprovider"
)

// provideFromAttr provides driver-level AWS credentials.
func (c *Provider) provideFromAttr(provideCtx ProvideContext) (envprovider.Environment, error) {
	klog.V(4).Infof("credentialprovider: Using volume attributes identity, provide context: %+v", provideCtx)
	if (provideCtx.AccessKeyID != "" && provideCtx.SecretAccessKey != "") || provideCtx.SessionToken != "" {
		return envprovider.Environment{
			envprovider.EnvAccessKeyID:     provideCtx.AccessKeyID,
			envprovider.EnvSecretAccessKey: provideCtx.SecretAccessKey,
			envprovider.EnvSessionToken:    provideCtx.SessionToken,
		}, nil
	}
	return nil, status.Error(codes.InvalidArgument, "Missing credentials. Please make sure volume attributes identity")
}
