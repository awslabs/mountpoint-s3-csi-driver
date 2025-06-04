package v2beta

import (
	"context"
	"fmt"

	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// getIndexFields returns the set of field extractors
func getIndexFields() map[string]func(*MountpointS3PodAttachment) string {
	return map[string]func(*MountpointS3PodAttachment) string{
		FieldNodeName:                         func(cr *MountpointS3PodAttachment) string { return cr.Spec.NodeName },
		FieldPersistentVolumeName:             func(cr *MountpointS3PodAttachment) string { return cr.Spec.PersistentVolumeName },
		FieldVolumeID:                         func(cr *MountpointS3PodAttachment) string { return cr.Spec.VolumeID },
		FieldMountOptions:                     func(cr *MountpointS3PodAttachment) string { return cr.Spec.MountOptions },
		FieldAuthenticationSource:             func(cr *MountpointS3PodAttachment) string { return cr.Spec.AuthenticationSource },
		FieldWorkloadFSGroup:                  func(cr *MountpointS3PodAttachment) string { return cr.Spec.WorkloadFSGroup },
		FieldWorkloadServiceAccountName:       func(cr *MountpointS3PodAttachment) string { return cr.Spec.WorkloadServiceAccountName },
		FieldWorkloadNamespace:                func(cr *MountpointS3PodAttachment) string { return cr.Spec.WorkloadNamespace },
		FieldWorkloadServiceAccountIAMRoleARN: func(cr *MountpointS3PodAttachment) string { return cr.Spec.WorkloadServiceAccountIAMRoleARN },
	}
}

// SetupManagerIndices sets up indices for a manager
func SetupManagerIndices(mgr manager.Manager) error {
	for field, extractor := range getIndexFields() {
		if err := setupManagerIndex(mgr, field, extractor); err != nil {
			return fmt.Errorf("failed to setup index for field %s: %w", field, err)
		}
	}
	return nil
}

// SetupCacheIndices sets up indices for a cache
func SetupCacheIndices(cache ctrlcache.Cache) error {
	for field, extractor := range getIndexFields() {
		if err := setupCacheIndex(cache, field, extractor); err != nil {
			return fmt.Errorf("failed to setup index for field %s: %w", field, err)
		}
	}
	return nil
}

func setupManagerIndex(mgr manager.Manager, field string, extractor func(*MountpointS3PodAttachment) string) error {
	return mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&MountpointS3PodAttachment{},
		field,
		func(obj client.Object) []string {
			return []string{extractor(obj.(*MountpointS3PodAttachment))}
		},
	)
}

func setupCacheIndex(cache ctrlcache.Cache, field string, extractor func(*MountpointS3PodAttachment) string) error {
	return cache.IndexField(
		context.Background(),
		&MountpointS3PodAttachment{},
		field,
		func(obj client.Object) []string {
			return []string{extractor(obj.(*MountpointS3PodAttachment))}
		},
	)
}
