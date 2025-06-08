package mounter

import (
	"context"

	crdv2beta "github.com/awslabs/mountpoint-s3-csi-driver/pkg/api/v2beta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type FakeCache struct {
	TestItems []crdv2beta.MountpointS3PodAttachment
}

func (f *FakeCache) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return nil
}

func (f *FakeCache) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	s3paList := list.(*crdv2beta.MountpointS3PodAttachmentList)
	s3paList.Items = f.TestItems
	return nil
}

func (f *FakeCache) GetInformer(ctx context.Context, obj client.Object, opts ...cache.InformerGetOption) (cache.Informer, error) {
	return nil, nil
}

func (f *FakeCache) GetInformerForKind(ctx context.Context, gvk schema.GroupVersionKind, opts ...cache.InformerGetOption) (cache.Informer, error) {
	return nil, nil
}

func (f *FakeCache) RemoveInformer(ctx context.Context, obj client.Object) error {
	return nil
}

func (f *FakeCache) IndexField(ctx context.Context, obj client.Object, field string, extractValue client.IndexerFunc) error {
	return nil
}

func (f *FakeCache) Start(ctx context.Context) error {
	return nil
}

func (f *FakeCache) WaitForCacheSync(ctx context.Context) bool {
	return true
}
