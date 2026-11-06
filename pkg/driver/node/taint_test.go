package node

import (
	"context"
	"testing"

	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

func TestCheckDriverRegistered(t *testing.T) {
	limit := func(limit int64, ok bool) func() (int64, bool) {
		return func() (int64, bool) { return limit, ok }
	}
	for _, testCase := range []struct {
		name        string
		drivers     []storagev1.CSINodeDriver
		volumeLimit func() (int64, bool)
		ready       bool
	}{
		{"registered", []storagev1.CSINodeDriver{registered(nil)}, nil, true},
		{"not registered", nil, nil, false},
		{"registered with the mounter's limit", []storagev1.CSINodeDriver{registered(ptr.To[int32](4))}, limit(4, true), true},
		{"registered with no limit, as the mounter", []storagev1.CSINodeDriver{registered(nil)}, limit(0, true), true},
		{"registered with no limit while the mounter has one", []storagev1.CSINodeDriver{registered(nil)}, limit(4, true), false},
		{"registered with another limit", []storagev1.CSINodeDriver{registered(ptr.To[int32](2))}, limit(4, true), false},
		{"registered while no mounter runs", []storagev1.CSINodeDriver{registered(ptr.To[int32](4))}, limit(0, false), false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			clientset := fake.NewSimpleClientset(&storagev1.CSINode{
				ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
				Spec:       storagev1.CSINodeSpec{Drivers: testCase.drivers},
			})

			err := checkDriverRegistered(context.Background(), clientset, "test-node", testCase.volumeLimit)

			assert.Equals(t, testCase.ready, err == nil)
		})
	}
}

// registered is this driver's CSINode entry, with the volume limit count, or none if nil.
func registered(count *int32) storagev1.CSINodeDriver {
	driver := storagev1.CSINodeDriver{Name: csiDriverName, NodeID: "test-node"}
	if count != nil {
		driver.Allocatable = &storagev1.VolumeNodeResources{Count: count}
	}
	return driver
}
