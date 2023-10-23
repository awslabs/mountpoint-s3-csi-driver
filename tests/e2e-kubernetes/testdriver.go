package e2e

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/storage/names"
	f "k8s.io/kubernetes/test/e2e/framework"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	"k8s.io/kubernetes/test/e2e/storage/framework"
)

var (
	PullRequest  string
	BucketRegion string // assumed to be the same as k8s cluster's region
)

type s3Driver struct {
	driverInfo framework.DriverInfo
}

type s3Volume struct {
	bucketName string
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
	bucketName := names.SimpleNameGenerator.GenerateName(fmt.Sprintf("e2e-kubernetes-%s-", PullRequest))
	input := &s3.CreateBucketInput{
		Bucket: aws.String(bucketName),
	}
	_, err := newS3Client().CreateBucket(input)
	f.ExpectNoError(err)
	f.Logf("Created bucket: %s", bucketName)
	return &s3Volume{bucketName: bucketName}
}

func (d *s3Driver) GetPersistentVolumeSource(readOnly bool, fsType string, testVolume framework.TestVolume) (*v1.PersistentVolumeSource, *v1.VolumeNodeAffinity) {
	volume, _ := testVolume.(*s3Volume)
	return &v1.PersistentVolumeSource{
		CSI: &v1.CSIPersistentVolumeSource{
			Driver:       d.driverInfo.Name,
			VolumeHandle: volume.bucketName,
		},
	}, nil
}

func (v *s3Volume) DeleteVolume(ctx context.Context) {
	s3Client := newS3Client()
	// delete all objects from a bucket
	iter := s3manager.NewDeleteListIterator(s3Client, &s3.ListObjectsInput{
		Bucket: aws.String(v.bucketName),
	})
	err := s3manager.NewBatchDeleteWithClient(s3Client).Delete(aws.BackgroundContext(), iter)
	f.ExpectNoError(err)
	// finally delete the bucket
	input := &s3.DeleteBucketInput{
		Bucket: aws.String(v.bucketName),
	}
	_, err = s3Client.DeleteBucket(input)
	f.ExpectNoError(err)
	f.Logf("Deleted bucket: %s", v.bucketName)
}

func newS3Client() *s3.S3 {
	session, err := session.NewSession(&aws.Config{Region: aws.String(BucketRegion)})
	f.ExpectNoError(err)
	return s3.New(session)
}
