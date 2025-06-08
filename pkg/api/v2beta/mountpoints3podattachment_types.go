package v2beta

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The following fields are used as matching criteria to determine if a mountpoint s3 pod can be shared by having the same MountpointS3PodAttachment resource:
const (
	FieldNodeName                         = "spec.nodeName"
	FieldPersistentVolumeName             = "spec.persistentVolumeName"
	FieldVolumeID                         = "spec.volumeID"
	FieldMountOptions                     = "spec.mountOptions"
	FieldAuthenticationSource             = "spec.authenticationSource"
	FieldWorkloadFSGroup                  = "spec.workloadFSGroup"
	FieldWorkloadServiceAccountName       = "spec.workloadServiceAccountName"
	FieldWorkloadNamespace                = "spec.workloadNamespace"
	FieldWorkloadServiceAccountIAMRoleARN = "spec.workloadServiceAccountIAMRoleARN"
)

// MountpointS3PodAttachmentSpec defines the desired state of MountpointS3PodAttachment.
type MountpointS3PodAttachmentSpec struct {
	// Important: Run "make generate" to regenerate code after modifying this file

	// Name of the node.
	NodeName string `json:"nodeName"`

	// Name of the Persistent Volume.
	PersistentVolumeName string `json:"persistentVolumeName"`

	// Volume ID.
	VolumeID string `json:"volumeID"`

	// Comma separated mount options taken from volume.
	MountOptions string `json:"mountOptions"`

	// Authentication source taken from volume attribute field `authenticationSource`.
	AuthenticationSource string `json:"authenticationSource"`

	// Workload pod's `fsGroup` from pod security context
	WorkloadFSGroup string `json:"workloadFSGroup"`

	// Workload pod's service account name. Exists only if `authenticationSource: pod`.
	WorkloadServiceAccountName string `json:"workloadServiceAccountName,omitempty"`

	// Workload pod's namespace. Exists only if `authenticationSource: pod`.
	WorkloadNamespace string `json:"workloadNamespace,omitempty"`

	// EKS IAM Role ARN from workload pod's service account annotation (IRSA). Exists only if `authenticationSource: pod` and service account has `eks.amazonaws.com/role-arn` annotation.
	WorkloadServiceAccountIAMRoleARN string `json:"workloadServiceAccountIAMRoleARN,omitempty"`

	// Maps each Mountpoint S3 pod name to its workload attachments
	MountpointS3PodAttachments map[string][]WorkloadAttachment `json:"mountpointS3PodAttachments"`
}

// WorkloadAttachment represents the attachment details of a workload pod to a Mountpoint S3 pod.
type WorkloadAttachment struct {
	// WorkloadPodUID is the unique identifier of the attached workload pod
	WorkloadPodUID string `json:"workloadPodUID"`

	// AttachmentTime represents when the workload pod was attached to the Mountpoint S3 pod
	AttachmentTime metav1.Time `json:"attachmentTime"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=s3pa
// +kubebuilder:selectablefield:JSONPath=`.spec.nodeName`

// MountpointS3PodAttachment is the Schema for the mountpoints3podattachments API.
type MountpointS3PodAttachment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec MountpointS3PodAttachmentSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// MountpointS3PodAttachmentList contains a list of MountpointS3PodAttachment.
type MountpointS3PodAttachmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MountpointS3PodAttachment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MountpointS3PodAttachment{}, &MountpointS3PodAttachmentList{})
}
