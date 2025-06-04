// Package volumecontext provides utilities for accessing volume context passed via CSI RPC.
package volumecontext

const (
	BucketName           = "bucketName"
	AuthenticationSource = "authenticationSource"
	STSRegion            = "stsRegion"

	Cache                                = "cache"
	CacheEmptyDirSizeLimit               = "cacheEmptyDirSizeLimit"
	CacheEmptyDirMedium                  = "cacheEmptyDirMedium"
	CacheEphemeralStorageClassName       = "cacheEphemeralStorageClassName"
	CacheEphemeralStorageResourceRequest = "cacheEphemeralStorageResourceRequest"

	MountpointPodServiceAccountName = "mountpointPodServiceAccountName"

	MountpointContainerResourcesRequestsCpu    = "mountpointContainerResourcesRequestsCpu"
	MountpointContainerResourcesRequestsMemory = "mountpointContainerResourcesRequestsMemory"
	MountpointContainerResourcesLimitsCpu      = "mountpointContainerResourcesLimitsCpu"
	MountpointContainerResourcesLimitsMemory   = "mountpointContainerResourcesLimitsMemory"

	CSIServiceAccountName   = "csi.storage.k8s.io/serviceAccount.name"
	CSIServiceAccountTokens = "csi.storage.k8s.io/serviceAccount.tokens"
	CSIPodNamespace         = "csi.storage.k8s.io/pod.namespace"
	CSIPodUID               = "csi.storage.k8s.io/pod.uid"
)
