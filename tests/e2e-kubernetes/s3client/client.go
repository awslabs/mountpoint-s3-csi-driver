// Package s3client provides an Amazon S3 client to be used in tests for creating and deleting Amazon S3 buckets.
package s3client

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/kubernetes/test/e2e/framework"
)

// DefaultRegion is the default AWS region to use if unspecified.
// It is public in order to be modified from test binary which receives region to use as a flag.
var DefaultRegion string

// See https://docs.aws.amazon.com/AmazonS3/latest/userguide/s3-express-networking.html#s3-express-endpoints
var expressAZs = map[string]string{
	"us-east-1":  "use1-az4",
	"us-west-2":  "usw2-az1",
	"eu-north-1": "eun1-az1",
}

const s3BucketNameMaxLength = 63
const s3BucketNamePrefix = "s3-csi-k8s-e2e-"

// DeleteBucketFunc is a cleanup function thats returned as a result of "Create*Bucket" calls.
// It clears the content of the bucket if not empty, and then deletes the bucket.
type DeleteBucketFunc func(context.Context) error

// A Client is an S3 client for creating an deleting S3 buckets.
type Client struct {
	region string
	client *s3.Client
}

// New returns a new client with "DefaultRegion".
func New() *Client {
	return NewWithRegion(DefaultRegion)
}

// NewWithRegion returns a new client with the given `region`.
func NewWithRegion(region string) *Client {
	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion(region),
		config.WithRetryer(func() aws.Retryer {
			return retry.NewStandard(func(opts *retry.StandardOptions) {
				opts.MaxAttempts = 5
				opts.MaxBackoff = 2 * time.Minute
			})
		}),
	)
	framework.ExpectNoError(err)
	return &Client{region: region, client: s3.NewFromConfig(cfg)}
}

// CreateStandardBucket creates a new standard S3 bucket with a random name,
// and returns the bucket name and a clean up function.
func (c *Client) CreateStandardBucket(ctx context.Context) (string, DeleteBucketFunc) {
	bucketName := c.randomBucketName("")

	input := &s3.CreateBucketInput{
		Bucket: aws.String(bucketName),
	}

	if c.region != "us-east-1" {
		input.CreateBucketConfiguration = &types.CreateBucketConfiguration{
			LocationConstraint: types.BucketLocationConstraint(c.region),
		}
	}

	return bucketName, c.create(ctx, input)
}

// CreateDirectoryBucket creates a new directory S3 bucket with a random name (by following
// "Directory bucket naming rules") and returns the bucket name and a clean up function.
func (c *Client) CreateDirectoryBucket(ctx context.Context) (string, DeleteBucketFunc) {
	regionAz := expressAZs[c.region]
	if regionAz == "" {
		framework.Failf("Unknown S3 Express region %s\n", c.region)
	}

	// refer to s3 express bucket naming conventions
	// https://docs.aws.amazon.com/AmazonS3/latest/userguide/directory-bucket-naming-rules.html
	suffix := fmt.Sprintf("--%s--x-s3", regionAz)
	bucketName := c.randomBucketName(suffix)

	// s3 express doesn't allow non-virtually routable names
	bucketName = strings.Replace(bucketName, ".", "", -1)

	return bucketName, c.create(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(bucketName),
		CreateBucketConfiguration: &types.CreateBucketConfiguration{
			Location: &types.LocationInfo{
				Name: aws.String(regionAz),
				Type: types.LocationTypeAvailabilityZone,
			},
			Bucket: &types.BucketInfo{
				DataRedundancy: types.DataRedundancySingleAvailabilityZone,
				Type:           types.BucketTypeDirectory,
			},
		},
	})
}

// WipeoutBucket removes all objects from given `bucketName`.
func (c *Client) WipeoutBucket(ctx context.Context, bucketName string) error {
	objects, err := c.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucketName),
	})
	if err != nil {
		return err
	}

	var objectIds []types.ObjectIdentifier
	// get all object keys in the s3 bucket
	for _, obj := range objects.Contents {
		objectIds = append(objectIds, types.ObjectIdentifier{Key: obj.Key})
	}

	// delete all objects from the bucket
	if len(objectIds) > 0 {
		_, err = c.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(bucketName),
			Delete: &types.Delete{Objects: objectIds},
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *Client) create(ctx context.Context, input *s3.CreateBucketInput) DeleteBucketFunc {
	bucketName := *input.Bucket

	_, err := c.client.CreateBucket(ctx, input)
	framework.ExpectNoError(err, "Failed to create S3 bucket")
	if err == nil {
		framework.Logf("S3 Bucket %s created", bucketName)
	}

	return func(ctx context.Context) error {
		return c.delete(ctx, bucketName)
	}
}

func (c *Client) delete(ctx context.Context, bucketName string) error {
	framework.Logf("Deleting S3 Bucket %s...", bucketName)

	err := c.WipeoutBucket(ctx, bucketName)
	if err != nil {
		return err
	}

	// finally delete the bucket
	_, err = c.client.DeleteBucket(ctx, &s3.DeleteBucketInput{
		Bucket: aws.String(bucketName),
	})
	if err != nil {
		return err
	}

	framework.Logf("S3 Bucket %s deleted", bucketName)

	return nil
}

// randomBucketName generates a random bucket name by using prefix (`s3BucketNamePrefix`) and `suffix`
// and generating random string for the remaining space according to S3's limit (63 as of today).
func (c *Client) randomBucketName(suffix string) string {
	prefixLen := len(s3BucketNamePrefix)
	suffixLen := len(suffix)
	rand := utilrand.String(s3BucketNameMaxLength - prefixLen - suffixLen)
	return s3BucketNamePrefix + rand + suffix
}
