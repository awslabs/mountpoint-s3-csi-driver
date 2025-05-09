package s3client

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/kubernetes/test/e2e/framework"
)

var (
	DefaultAccessKey       string
	DefaultSecretAccessKey string
	DefaultS3EndpointUrl   string
)

const DefaultRegion = "us-east-1"
const s3BucketNameMaxLength = 63
const s3BucketNamePrefix = "s3-csi-k8s-e2e-"

// DeleteBucketFunc is a cleanup function thats returned as a result of "Create*Bucket" calls.
// It clears the content of the bucket if not empty, and then deletes the bucket.
type DeleteBucketFunc func(context.Context) error

type Client struct {
	region string
	client *s3.Client
}

// New returns a new client with "DefaultRegion".
func New(region string, accessKey string, secretKey string) *Client {
	if accessKey == "" {
		accessKey = DefaultAccessKey
	}
	if secretKey == "" {
		secretKey = DefaultSecretAccessKey
	}
	if region == "" {
		region = DefaultRegion
	}
	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			accessKey,
			secretKey,
			"",
		)),
		config.WithRetryer(func() aws.Retryer {
			return retry.NewStandard(func(opts *retry.StandardOptions) {
				opts.MaxAttempts = 5
				opts.MaxBackoff = 2 * time.Minute
			})
		}),
	)
	framework.ExpectNoError(err)
	return &Client{region: region, client: s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = true
		o.BaseEndpoint = aws.String(DefaultS3EndpointUrl)
	})}
}

// CreateBucket creates a new standard S3 bucket with a random name,
// and returns the bucket name and a clean up function.
func (c *Client) CreateBucket(ctx context.Context) (string, DeleteBucketFunc) {
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

// randomBucketName generates a random bucket name by using prefix (`s3BucketNamePrefix`) and `suffix`
// and generating random string for the remaining space according to S3's limit (63 as of today).
func (c *Client) randomBucketName(suffix string) string {
	prefixLen := len(s3BucketNamePrefix)
	suffixLen := len(suffix)
	rand := utilrand.String(s3BucketNameMaxLength - prefixLen - suffixLen)
	return s3BucketNamePrefix + rand + suffix
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

// DeleteObject deletes a single object with the given key from the specified bucket
func (c *Client) DeleteObject(ctx context.Context, bucketName string, key string) error {
	framework.Logf("Deleting object %s from bucket %s", key, bucketName)

	_, err := c.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(key),
	})

	if err != nil {
		framework.Logf("Failed to delete object %s: %v", key, err)
		return err
	}

	framework.Logf("Successfully deleted object %s from bucket %s", key, bucketName)
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

// GetObjectOwnerID returns the canonical ID of the owner of a specific object.
// It uses ListObjectsV2 with FetchOwner=true, requiring only list permission (no ACLs).
func (c *Client) GetObjectOwnerID(ctx context.Context, bucket, key string) (string, error) {
	// First, check if the object exists using HeadObject
	_, err := c.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return "", fmt.Errorf("head object failed for %s/%s: %w", bucket, key, err)
	}

	out, err := c.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:     aws.String(bucket),
		Prefix:     aws.String(key),
		MaxKeys:    aws.Int32(1),
		FetchOwner: aws.Bool(true),
	})
	if err != nil {
		return "", err
	}

	if len(out.Contents) == 0 || out.Contents[0].Owner == nil || out.Contents[0].Owner.ID == nil {
		return "", fmt.Errorf("owner not returned for %s/%s", bucket, key)
	}
	return *out.Contents[0].Owner.ID, nil
}

// ListObjects lists all objects in a bucket and returns the response
func (c *Client) ListObjects(ctx context.Context, bucket string) (*s3.ListObjectsV2Output, error) {
	out, err := c.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
	})
	if err != nil {
		return nil, err
	}

	framework.Logf("Found %d objects in bucket %s", len(out.Contents), bucket)
	return out, nil
}
