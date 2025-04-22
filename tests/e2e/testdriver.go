package e2e

import (
	"context"

	"github.com/scality/mountpoint-s3-csi-driver/tests/e2e-tests/pkg/s3client"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	f "k8s.io/kubernetes/test/e2e/framework"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	"k8s.io/kubernetes/test/e2e/storage/framework"
)

type s3Driver struct {
	client     *s3client.Client
	driverInfo framework.DriverInfo
}

type s3Volume struct {
	bucketName           string
	deleteBucket         s3client.DeleteBucketFunc
	authenticationSource string
}

var _ framework.TestDriver = &s3Driver{}
var _ framework.PreprovisionedVolumeTestDriver = &s3Driver{}
var _ framework.PreprovisionedPVTestDriver = &s3Driver{}

func initS3Driver() *s3Driver {
	return &s3Driver{
		client: s3client.New(),
		driverInfo: framework.DriverInfo{
			Name:        "s3.csi.aws.com", // TODO(S3CSI-10): Change to Scality S3 CSI Driver
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
		e2eskipper.Skipf("Scality S3 Driver only supports static provisioning -- skipping")
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

	var bucketName string
	var deleteBucket s3client.DeleteBucketFunc
	type contextKey string                                            // TODO(S3CSI-11): Refactor once more authentication sources are supported
	const authenticationSourceKey contextKey = "authenticationSource" // TODO(S3CSI-11): Refactor once more authentication sources are supported

	bucketName, deleteBucket = d.client.CreateBucket(ctx)
	val, _ := ctx.Value(authenticationSourceKey).(string) // TODO(S3CSI-11): Refactor once more authentication sources are supported

	return &s3Volume{
		bucketName:           bucketName,
		deleteBucket:         deleteBucket,
		authenticationSource: val,
	}
}

func (d *s3Driver) GetPersistentVolumeSource(readOnly bool, fsType string, testVolume framework.TestVolume) (*v1.PersistentVolumeSource, *v1.VolumeNodeAffinity) {
	volume, _ := testVolume.(*s3Volume)

	volumeAttributes := map[string]string{"bucketName": volume.bucketName}
	if volume.authenticationSource != "" {
		f.Logf("Using authentication source %s for volume", volume.authenticationSource)
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
	err := v.deleteBucket(ctx)
	f.ExpectNoError(err, "Failed to delete S3 Bucket: %s", v.bucketName)
}
