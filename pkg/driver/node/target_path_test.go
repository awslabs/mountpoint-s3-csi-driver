package node_test

import (
	"testing"

	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node"
)

func TestParsingTargetPath(t *testing.T) {
	for name, test := range map[string]struct {
		targetPath string
		parsed     node.TargetPath
		err        error
	}{
		"regular target path": {
			targetPath: "/var/lib/kubelet/pods/d8c872d7-a29c-4362-81b1-9912370d0813/volumes/kubernetes.io~csi/s3-csi-driver-volume/mount",
			parsed: node.TargetPath{
				PodID:    "d8c872d7-a29c-4362-81b1-9912370d0813",
				VolumeID: "s3-csi-driver-volume",
			},
		},
		"volume id with escapes": {
			// Kubernetes replaces "/" with "~" in Volume IDs.
			targetPath: "/var/lib/kubelet/pods/8b40411d-8f81-45b5-ace4-0b3104238871/volumes/kubernetes.io~csi/s3-csi~driver/mount",
			parsed: node.TargetPath{
				PodID:    "8b40411d-8f81-45b5-ace4-0b3104238871",
				VolumeID: "s3-csi~driver",
			},
		},
		"different kubelet path": {
			targetPath: "/etc/kubelet/pods/f0ed9a5b-73cb-412c-82c1-0d9c74cb8378/volumes/kubernetes.io~csi/vol-id/mount",
			parsed: node.TargetPath{
				PodID:    "f0ed9a5b-73cb-412c-82c1-0d9c74cb8378",
				VolumeID: "vol-id",
			},
		},
		"missing mount": {
			targetPath: "/etc/kubelet/pods/f0ed9a5b-73cb-412c-82c1-0d9c74cb8378/volumes/kubernetes.io~csi/vol-id",
			err:        node.ErrInvalidTargetPath,
		},
		"missing volume id": {
			targetPath: "/etc/kubelet/pods/f0ed9a5b-73cb-412c-82c1-0d9c74cb8378/volumes/kubernetes.io~csi/mount",
			err:        node.ErrInvalidTargetPath,
		},
		"missing csi plugin name": {
			targetPath: "/etc/kubelet/pods/f0ed9a5b-73cb-412c-82c1-0d9c74cb8378/volumes/vol-id/mount",
			err:        node.ErrInvalidTargetPath,
		},
		"missing volumes": {
			targetPath: "/etc/kubelet/pods/f0ed9a5b-73cb-412c-82c1-0d9c74cb8378/kubernetes.io~csi/vol-id/mount",
			err:        node.ErrInvalidTargetPath,
		},
		"missing pod id": {
			targetPath: "/kubelet/kubernetes.io~csi/vol-id/mount",
			err:        node.ErrInvalidTargetPath,
		},
		"empty string": {
			targetPath: "",
			err:        node.ErrInvalidTargetPath,
		},
	} {
		t.Run(name, func(t *testing.T) {
			parsed, err := node.ParseTargetPath(test.targetPath)
			if test.err != nil {
				assertEquals(t, test.err, err)
			} else {
				assertEquals(t, test.parsed, parsed)
			}
		})
	}
}

func assertEquals[T comparable](t *testing.T, expected T, got T) {
	if expected != got {
		t.Errorf("Expected %#v, Got %#v", expected, got)
	}
}
