package csicontroller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crdv2 "github.com/awslabs/mountpoint-s3-csi-driver/pkg/api/v2"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/volumecontext"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

func TestStaleAttachmentCleanerClearsCreationExpectationWhenAttachmentIsObserved(t *testing.T) {
	testCases := []struct {
		name           string
		authSource     string
		workloadRole   string
		workloadSA     string
		workloadNS     string
		volumeAttrs    map[string]string
		workloadExists bool
	}{
		{
			name:       "driver authentication",
			authSource: credentialprovider.AuthenticationSourceDriver,
			workloadNS: "default",
		},
		{
			name:         "pod authentication",
			authSource:   credentialprovider.AuthenticationSourcePod,
			workloadRole: "arn:aws:iam::123456789012:role/workload",
			workloadSA:   "workload-sa",
			workloadNS:   "workload-namespace",
			volumeAttrs: map[string]string{
				volumecontext.AuthenticationSource: credentialprovider.AuthenticationSourcePod,
			},
		},
		{
			name:           "driver authentication with valid workload",
			authSource:     credentialprovider.AuthenticationSourceDriver,
			workloadNS:     "default",
			workloadExists: true,
		},
		{
			name:         "pod authentication with valid workload",
			authSource:   credentialprovider.AuthenticationSourcePod,
			workloadRole: "arn:aws:iam::123456789012:role/workload",
			workloadSA:   "workload-sa",
			workloadNS:   "workload-namespace",
			volumeAttrs: map[string]string{
				volumecontext.AuthenticationSource: credentialprovider.AuthenticationSourcePod,
			},
			workloadExists: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			const mountpointPodName = "mp-stale"
			fsGroup := int64(1000)

			pv := &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{Name: testPVName},
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						CSI: &corev1.CSIPersistentVolumeSource{
							Driver:           mountpointCSIDriverName,
							VolumeHandle:     testVolumeID,
							VolumeAttributes: testCase.volumeAttrs,
						},
					},
					MountOptions: []string{"allow-delete", "region us-east-1"},
				},
			}
			workloadPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "workload",
					Namespace: testCase.workloadNS,
					UID:       "workload-uid",
				},
				Spec: corev1.PodSpec{
					NodeName:           testNodeName,
					ServiceAccountName: testCase.workloadSA,
					SecurityContext:    &corev1.PodSecurityContext{FSGroup: &fsGroup},
				},
			}
			s3pa := newS3PA("s3pa-stale", map[string][]crdv2.WorkloadAttachment{
				mountpointPodName: {{
					WorkloadPodUID: string(workloadPod.UID),
					AttachmentTime: metav1.NewTime(time.Now().UTC().Add(-staleAttachmentThreshold - time.Second)),
				}},
			})
			s3pa.Spec.MountOptions = strings.Join(pv.Spec.MountOptions, ",")
			s3pa.Spec.AuthenticationSource = testCase.authSource
			s3pa.Spec.WorkloadFSGroup = "1000"
			s3pa.Spec.WorkloadNamespace = testCase.workloadNS
			s3pa.Spec.WorkloadServiceAccountName = testCase.workloadSA
			s3pa.Spec.WorkloadServiceAccountIAMRoleARN = testCase.workloadRole
			mountpointPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      mountpointPodName,
					Namespace: testPodConfig().Namespace,
				},
			}

			objects := []client.Object{s3pa, mountpointPod}
			if testCase.workloadExists {
				objects = append(objects, workloadPod)
			}
			c, reconciler := newReconcilerWithObjects(t, objects...)
			fieldFilters := reconciler.buildFieldFilters(workloadPod, pv, testCase.workloadRole)
			reconciler.s3paExpectations.setPending(fieldFilters)

			cleaner := NewStaleAttachmentCleaner(reconciler)
			err := cleaner.RunCleanup(context.Background())
			assert.NoError(t, err)

			if testCase.workloadExists {
				current := &crdv2.MountpointS3PodAttachment{}
				err := c.Get(context.Background(), client.ObjectKeyFromObject(s3pa), current)
				assert.NoError(t, err)
				attachments := current.Spec.MountpointS3PodAttachments[mountpointPodName]
				if len(attachments) != 1 || attachments[0].WorkloadPodUID != string(workloadPod.UID) {
					t.Errorf("expected valid workload attachment to remain, got %v", attachments)
				}
			} else {
				assertS3PADeleted(t, c, s3pa.Name)
			}
			if reconciler.s3paExpectations.isPending(fieldFilters) {
				t.Error("expected creation expectation to be cleared after the stale cleaner observed the S3PA")
			}
		})
	}
}
