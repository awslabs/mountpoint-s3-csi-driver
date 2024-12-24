package regionprovider

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"k8s.io/klog/v2"
)

// RegionFromIMDSOnce tries to detect AWS region by making a request to IMDS.
// It only makes request to IMDS once and caches the value.
var RegionFromIMDSOnce = sync.OnceValues(func() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		klog.V(5).Infof("regionprovider: Failed to create config for IMDS client: %v", err)
		return "", fmt.Errorf("could not create config for imds client: %w", err)
	}

	client := imds.NewFromConfig(cfg)
	output, err := client.GetRegion(ctx, &imds.GetRegionInput{})
	if err != nil {
		klog.V(5).Infof("regionprovider: Failed to get region from IMDS: %v", err)
		return "", fmt.Errorf("failed to get region from imds: %w", err)
	}

	return output.Region, nil
})
