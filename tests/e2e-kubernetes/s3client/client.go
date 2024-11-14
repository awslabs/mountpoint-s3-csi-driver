// Package s3client provides an Amazon S3 client to be used in tests for creating and deleting Amazon S3 buckets.
package s3client

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
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

const maxS3ExpressBucketNameLength = 63

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
	)
	framework.ExpectNoError(err)
	return &Client{region: region, client: s3.NewFromConfig(cfg)}
}

// CreateStandardBucket creates a standard S3 bucket with the given name,
// and returns the bucket name and a clean up function.
func (c *Client) CreateStandardBucket(ctx context.Context, bucketName string) (string, DeleteBucketFunc) {
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

// CreateDirectoryBucket creates a directory S3 bucket with the given name by modifying it according to
// "Directory bucket naming rules" and returns the bucket name and a clean up function.
func (c *Client) CreateDirectoryBucket(ctx context.Context, bucketName string) (string, DeleteBucketFunc) {
	regionAz := expressAZs[c.region]
	if regionAz == "" {
		framework.Failf("Unknown S3 Express region %s\n", c.region)
	}

	// refer to s3 express bucket naming conventions
	// https://docs.aws.amazon.com/AmazonS3/latest/userguide/directory-bucket-naming-rules.html
	suffix := fmt.Sprintf("--%s--x-s3", regionAz)
	// s3 express doesn't allow non-virtually routable names
	bucketName = strings.Replace(bucketName, ".", "", -1)
	if len(bucketName)+len(suffix) > maxS3ExpressBucketNameLength {
		bucketName = strings.TrimRight(bucketName[:maxS3ExpressBucketNameLength-len(suffix)], "-")
	}
	bucketName = fmt.Sprintf("%s%s", bucketName, suffix)

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
