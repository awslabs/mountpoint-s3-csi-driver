package targetpath_test

import (
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/targetpath"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

func TestParsingTargetPath(t *testing.T) {
	for name, test := range map[string]struct {
		targetPath string
		parsed     targetpath.TargetPath
		err        error
	}{
		"regular target path": {
			targetPath: "/var/lib/kubelet/pods/d8c872d7-a29c-4362-81b1-9912370d0813/volumes/kubernetes.io~csi/s3-csi-driver-volume/mount",
			parsed: targetpath.TargetPath{
				PodID:    "d8c872d7-a29c-4362-81b1-9912370d0813",
				VolumeID: "s3-csi-driver-volume",
			},
		},
		"volume id with escapes": {
			// Kubernetes replaces "/" with "~" in Volume IDs.
			targetPath: "/var/lib/kubelet/pods/8b40411d-8f81-45b5-ace4-0b3104238871/volumes/kubernetes.io~csi/s3-csi~driver/mount",
			parsed: targetpath.TargetPath{
				PodID:    "8b40411d-8f81-45b5-ace4-0b3104238871",
				VolumeID: "s3-csi~driver",
			},
		},
		"different kubelet path": {
			targetPath: "/etc/kubelet/pods/f0ed9a5b-73cb-412c-82c1-0d9c74cb8378/volumes/kubernetes.io~csi/vol-id/mount",
			parsed: targetpath.TargetPath{
				PodID:    "f0ed9a5b-73cb-412c-82c1-0d9c74cb8378",
				VolumeID: "vol-id",
			},
		},
		"missing mount": {
			targetPath: "/etc/kubelet/pods/f0ed9a5b-73cb-412c-82c1-0d9c74cb8378/volumes/kubernetes.io~csi/vol-id",
			err:        targetpath.ErrInvalidTargetPath,
		},
		"missing volume id": {
			targetPath: "/etc/kubelet/pods/f0ed9a5b-73cb-412c-82c1-0d9c74cb8378/volumes/kubernetes.io~csi/mount",
			err:        targetpath.ErrInvalidTargetPath,
		},
		"missing csi plugin name": {
			targetPath: "/etc/kubelet/pods/f0ed9a5b-73cb-412c-82c1-0d9c74cb8378/volumes/vol-id/mount",
			err:        targetpath.ErrInvalidTargetPath,
		},
		"missing volumes": {
			targetPath: "/etc/kubelet/pods/f0ed9a5b-73cb-412c-82c1-0d9c74cb8378/kubernetes.io~csi/vol-id/mount",
			err:        targetpath.ErrInvalidTargetPath,
		},
		"missing pod id": {
			targetPath: "/kubelet/kubernetes.io~csi/vol-id/mount",
			err:        targetpath.ErrInvalidTargetPath,
		},
		"empty string": {
			targetPath: "",
			err:        targetpath.ErrInvalidTargetPath,
		},
	} {
		t.Run(name, func(t *testing.T) {
			parsed, err := targetpath.Parse(test.targetPath)
			if test.err != nil {
				assert.Equals(t, test.err, err)
			} else {
				assert.Equals(t, test.parsed, parsed)
			}
		})
	}
}
