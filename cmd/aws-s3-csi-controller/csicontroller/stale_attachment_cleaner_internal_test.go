package csicontroller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	crdv2 "github.com/awslabs/mountpoint-s3-csi-driver/pkg/api/v2"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/volumecontext"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

func TestStaleAttachmentCleanerClearsCreationExpectationWhenAttachmentIsObserved(t *testing.T) {
	testCases := []struct {
		name         string
		authSource   string
		workloadRole string
		workloadSA   string
		workloadNS   string
		volumeAttrs  map[string]string
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
				ObjectMeta: metav1.ObjectMeta{Namespace: testCase.workloadNS},
				Spec: corev1.PodSpec{
					NodeName:           testNodeName,
					ServiceAccountName: testCase.workloadSA,
					SecurityContext:    &corev1.PodSecurityContext{FSGroup: &fsGroup},
				},
			}
			s3pa := newS3PA("s3pa-stale", map[string][]crdv2.WorkloadAttachment{
				mountpointPodName: {{
					WorkloadPodUID: "deleted-workload-uid",
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

			client, reconciler := newReconcilerWithObjects(t, s3pa, mountpointPod)
			fieldFilters := reconciler.buildFieldFilters(workloadPod, pv, testCase.workloadRole)
			reconciler.s3paExpectations.setPending(fieldFilters)

			cleaner := NewStaleAttachmentCleaner(reconciler)
			err := cleaner.RunCleanup(context.Background())
			assert.NoError(t, err)

			assertS3PADeleted(t, client, s3pa.Name)
			if reconciler.s3paExpectations.isPending(fieldFilters) {
				t.Error("expected creation expectation to be cleared after the stale cleaner observed the S3PA")
			}
		})
	}
}
