package e2e

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	custom_testsuites "github.com/awslabs/aws-s3-csi-driver/tests/e2e-kubernetes/testsuites"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/storage/names"
	f "k8s.io/kubernetes/test/e2e/framework"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	"k8s.io/kubernetes/test/e2e/storage/framework"
)

const (
	maxS3ExpressBucketNameLength = 63
)

var (
	CommitId     string
	BucketRegion string // assumed to be the same as k8s cluster's region
	BucketPrefix string
	Performance  bool
)

type s3Driver struct {
	driverInfo framework.DriverInfo
}

type s3Volume struct {
	bucketName           string
	authenticationSource string
}

var _ framework.TestDriver = &s3Driver{}
var _ framework.PreprovisionedVolumeTestDriver = &s3Driver{}
var _ framework.PreprovisionedPVTestDriver = &s3Driver{}

func initS3Driver() *s3Driver {
	return &s3Driver{
		driverInfo: framework.DriverInfo{
			Name:        "s3.csi.aws.com",
			MaxFileSize: framework.FileSizeLarge,
			SupportedFsType: sets.NewString(
				"", // Default fsType
			),
			Capabilities: map[framework.Capability]bool{
				framework.CapPersistence: true,
			},
			RequiredAccessModes: []v1.PersistentVolumeAccessMode{
				v1.ReadWriteMany,
				v1.ReadOnlyMany,
			},
		},
	}
}

func (d *s3Driver) GetDriverInfo() *framework.DriverInfo {
	return &d.driverInfo
}

func (d *s3Driver) SkipUnsupportedTest(pattern framework.TestPattern) {
	if pattern.VolType != framework.PreprovisionedPV {
		e2eskipper.Skipf("S3 Driver only supports static provisioning -- skipping")
	}
}

func (d *s3Driver) PrepareTest(ctx context.Context, f *f.Framework) *framework.PerTestConfig {
	config := &framework.PerTestConfig{
		Driver:    d,
		Prefix:    "s3",
		Framework: f,
	}

	return config
}

func (d *s3Driver) CreateVolume(ctx context.Context, config *framework.PerTestConfig, volumeType framework.TestVolType) framework.TestVolume {
	if volumeType != framework.PreprovisionedPV {
		f.Failf("Unsupported volType: %v is specified", volumeType)
	}
	bucketName := names.SimpleNameGenerator.GenerateName(fmt.Sprintf("%s-e2e-kubernetes-%s-", BucketPrefix, CommitId))
	input := &s3.CreateBucketInput{
		Bucket: aws.String(bucketName),
	}
	if BucketRegion != "us-east-1" {
		input.CreateBucketConfiguration = &types.CreateBucketConfiguration{
			LocationConstraint: types.BucketLocationConstraint(BucketRegion),
		}
	}

	if config.Prefix == custom_testsuites.S3ExpressTestIdentifier {
		// assume us-east-1 since that's where our integration tests currently do their work
		// https://docs.aws.amazon.com/AmazonS3/latest/userguide/s3-express-networking.html
		regionAz := "use1-az4"
		if BucketRegion == "us-west-2" {
			regionAz = "usw2-az1"
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
		input = &s3.CreateBucketInput{
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
		}
	}
	f.Logf("Attempting to create bucket: %s", bucketName)
	_, err := newS3Client().CreateBucket(ctx, input)
	f.ExpectNoError(err)
	f.Logf("Created bucket: %s", bucketName)
	return &s3Volume{bucketName: bucketName, authenticationSource: custom_testsuites.AuthenticationSourceFromContext(ctx)}
}

func (d *s3Driver) GetPersistentVolumeSource(readOnly bool, fsType string, testVolume framework.TestVolume) (*v1.PersistentVolumeSource, *v1.VolumeNodeAffinity) {
	volume, _ := testVolume.(*s3Volume)

	volumeAttributes := map[string]string{"bucketName": volume.bucketName}
	if volume.authenticationSource != "" {
		f.Logf("Using authencation source %s for volume", volume.authenticationSource)
		volumeAttributes["authenticationSource"] = volume.authenticationSource
	}

	return &v1.PersistentVolumeSource{
		CSI: &v1.CSIPersistentVolumeSource{
			Driver:           d.driverInfo.Name,
			VolumeHandle:     volume.bucketName,
			VolumeAttributes: volumeAttributes,
		},
	}, nil
}

func (v *s3Volume) DeleteVolume(ctx context.Context) {
	s3Client := newS3Client()
	objects, err := s3Client.ListObjectsV2(context.TODO(), &s3.ListObjectsV2Input{
		Bucket: aws.String(v.bucketName),
	})
	f.ExpectNoError(err)
	var objectIds []types.ObjectIdentifier
	// get all object keys in the s3 bucket
	for _, obj := range objects.Contents {
		objectIds = append(objectIds, types.ObjectIdentifier{Key: obj.Key})
	}
	// delete all objects from the bucket
	if len(objectIds) > 0 {
		_, err = s3Client.DeleteObjects(context.TODO(), &s3.DeleteObjectsInput{
			Bucket: aws.String(v.bucketName),
			Delete: &types.Delete{Objects: objectIds},
		})
		f.ExpectNoError(err)
	}
	// finally delete the bucket
	input := &s3.DeleteBucketInput{
		Bucket: aws.String(v.bucketName),
	}
	_, err = s3Client.DeleteBucket(context.TODO(), input)
	f.ExpectNoError(err)
	f.Logf("Deleted bucket: %s", v.bucketName)
}

func newS3Client() *s3.Client {
	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithRegion(BucketRegion),
	)
	f.ExpectNoError(err)
	return s3.NewFromConfig(cfg)
}
