package credentialprovider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"k8s.io/klog/v2"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/envprovider"
)

var errUnknownRegion = errors.New("credentialprovider: pod-level: unknown region")

// stsRegion tries to detect AWS region to use for STS.
//
// It looks for the following (in-order):
//  1. `stsRegion` passed via volume context
//  2. Region set for S3 bucket via mount options
//  3. `AWS_REGION` or `AWS_DEFAULT_REGION` env variables
//  4. Calling IMDS to detect region
//
// It returns an error if all of them fails.
func (p *Provider) stsRegion(provideCtx ProvideContext) (string, error) {
	region := provideCtx.StsRegion
	if region != "" {
		klog.V(5).Infof("credentialprovider: pod-level: Detected STS region %s from volume context", region)
		return region, nil
	}

	region = provideCtx.BucketRegion
	if region != "" {
		klog.V(5).Infof("credentialprovider: pod-level: Detected STS region %s from S3 bucket region", region)
		return region, nil
	}

	region = os.Getenv(envprovider.EnvRegion)
	if region != "" {
		klog.V(5).Infof("credentialprovider: pod-level: Detected STS region %s from `AWS_REGION` env variable", region)
		return region, nil
	}

	region = os.Getenv(envprovider.EnvDefaultRegion)
	if region != "" {
		klog.V(5).Infof("credentialprovider: pod-level: Detected STS region %s from `AWS_DEFAULT_REGION` env variable", region)
		return region, nil
	}

	// We're ignoring the error here, makes a call to IMDS only once and logs the error in case of error
	region, _ = p.regionFromIMDS()
	if region != "" {
		klog.V(5).Infof("credentialprovider: pod-level: Detected STS region %s from IMDS", region)
		return region, nil
	}

	return "", errUnknownRegion
}

// RegionFromIMDSOnce tries to detect AWS region by making a request to IMDS.
// It only makes request to IMDS once and caches the value.
var RegionFromIMDSOnce = sync.OnceValues(func() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		klog.V(5).Infof("credentialprovider: pod-level: Failed to create config for IMDS client: %v", err)
		return "", fmt.Errorf("could not create config for imds client: %w", err)
	}

	client := imds.NewFromConfig(cfg)
	output, err := client.GetRegion(ctx, &imds.GetRegionInput{})
	if err != nil {
		klog.V(5).Infof("credentialprovider: pod-level: Failed to get region from IMDS: %v", err)
		return "", fmt.Errorf("failed to get region from imds: %w", err)
	}

	return output.Region, nil
})
