// Package regionprovider provides utilities for detecting region by
// looking environment variables, mount options, or calling IMDS.
package regionprovider

import (
	"errors"

	"k8s.io/klog/v2"

	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/envprovider"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/volumecontext"
	"github.com/awslabs/aws-s3-csi-driver/pkg/mountpoint"
)

// ErrUnknownRegion is the error returned when the region could not be detected.
var ErrUnknownRegion = errors.New("regionprovider: unknown region")

// A Provider provides methods for detecting regions.
type Provider struct {
	regionFromIMDS func() (string, error)
}

// New creates a new [Provider] by using given [regionFromIMDS].
func New(regionFromIMDS func() (string, error)) *Provider {
	// `regionFromIMDS` is a `sync.OnceValues` and it only makes request to IMDS once,
	// this call is basically here to pre-warm the cache of IMDS call.
	go func() {
		_, _ = regionFromIMDS()
	}()

	return &Provider{regionFromIMDS: regionFromIMDS}
}

// SecurityTokenService tries to detect AWS region to use for STS.
//
// It looks for the following (in-order):
//  1. `stsRegion` passed via volume context
//  2. Region set for S3 bucket via mount options
//  3. `AWS_REGION` or `AWS_DEFAULT_REGION` env variables
//  4. Calling IMDS to detect region
//
// It returns [ErrUnknownRegion] if all of them fails.
func (p *Provider) SecurityTokenService(volumeContext map[string]string, args mountpoint.Args) (string, error) {
	region := volumeContext[volumecontext.STSRegion]
	if region != "" {
		klog.V(5).Infof("regionprovider: Detected STS region %s from volume context", region)
		return region, nil
	}

	if region, ok := args.Value(mountpoint.ArgRegion); ok {
		klog.V(5).Infof("regionprovider: Detected STS region %s from S3 bucket region", region)
		return region, nil
	}

	region = envprovider.Region()
	if region != "" {
		klog.V(5).Infof("regionprovider: Detected STS region %s from env variable", region)
		return region, nil
	}

	// We're ignoring the error here, makes a call to IMDS only once and logs the error in case of error
	region, _ = p.regionFromIMDS()
	if region != "" {
		klog.V(5).Infof("regionprovider: Detected STS region %s from IMDS", region)
		return region, nil
	}

	return "", ErrUnknownRegion
}
