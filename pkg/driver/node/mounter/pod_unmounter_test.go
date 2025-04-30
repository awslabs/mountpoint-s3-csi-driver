package mounter_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	crdv1beta "github.com/awslabs/aws-s3-csi-driver/pkg/api/v1beta"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/mounter"
	"github.com/awslabs/aws-s3-csi-driver/pkg/podmounter/mppod"
	"github.com/awslabs/aws-s3-csi-driver/pkg/podmounter/mppod/watcher"
	"github.com/awslabs/aws-s3-csi-driver/pkg/util/testutil/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/mount-utils"
)

const (
	nodeName = "test-node"
)

func setupPodWatcher(t *testing.T, pods ...*corev1.Pod) (*watcher.Watcher, *fake.Clientset) {
	client := fake.NewClientset()
	podWatcher := watcher.New(client, mountpointPodNamespace, nodeName, 10*time.Second)
	stopCh := make(chan struct{})
	t.Cleanup(func() {
		close(stopCh)
	})

	for _, pod := range pods {
		if pod != nil {
			_, err := client.CoreV1().Pods(mountpointPodNamespace).Create(context.Background(), pod, metav1.CreateOptions{})
			assert.NoError(t, err)
		}
	}

	err := podWatcher.Start(stopCh)
	assert.NoError(t, err)

	return podWatcher, client
}

func countUnmountCalls(mounter *mount.FakeMounter) int {
	unmountCalls := 0
	for _, action := range mounter.GetLog() {
		if action.Action == mount.FakeActionUnmount {
			unmountCalls++
		}
	}
	return unmountCalls
}

func TestHandleS3PodAttachmentUpdate(t *testing.T) {
	tests := []struct {
		name          string
		nodeID        string
		s3pa          *crdv1beta.MountpointS3PodAttachment
		pod           *corev1.Pod
		unmountError  error
		expectUnmount bool
	}{
		{
			name:   "different node",
			nodeID: "node1",
			s3pa: &crdv1beta.MountpointS3PodAttachment{
				Spec: crdv1beta.MountpointS3PodAttachmentSpec{
					NodeName: "node2",
				},
			},
			expectUnmount: false,
		},
		{
			name:   "same node with empty workload",
			nodeID: nodeName,
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod1",
					Namespace: mountpointPodNamespace,
					UID:       "uid1",
				},
			},
			s3pa: &crdv1beta.MountpointS3PodAttachment{
				Spec: crdv1beta.MountpointS3PodAttachmentSpec{
					NodeName: nodeName,
					MountpointS3PodAttachments: map[string][]crdv1beta.WorkloadAttachment{
						"pod1": {},
					},
				},
			},
			expectUnmount: true,
		},
		{
			name:   "same node with empty workload and unmount error",
			nodeID: nodeName,
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod1",
					Namespace: mountpointPodNamespace,
					UID:       "uid1",
				},
			},
			s3pa: &crdv1beta.MountpointS3PodAttachment{
				Spec: crdv1beta.MountpointS3PodAttachmentSpec{
					NodeName: nodeName,
					MountpointS3PodAttachments: map[string][]crdv1beta.WorkloadAttachment{
						"pod1": {},
					},
				},
			},
			unmountError:  errors.New("unmount error"),
			expectUnmount: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kubeletPath := t.TempDir()
			t.Setenv("KUBELET_PATH", kubeletPath)
			t.Chdir(kubeletPath)

			sourceMountDir := t.TempDir()

			podWatcher, client := setupPodWatcher(t, tt.pod)

			if tt.pod != nil {
				podPath := filepath.Join(kubeletPath, "pods", string(tt.pod.UID))
				commDir := mppod.PathOnHost(podPath)
				err := os.MkdirAll(commDir, 0750)
				assert.NoError(t, err)

				err = os.MkdirAll(filepath.Join(sourceMountDir, string(tt.pod.UID)), 0750)
				assert.NoError(t, err)
			}

			fakeMounter := mount.NewFakeMounter(nil)
			if tt.unmountError != nil {
				fakeMounter.UnmountFunc = func(path string) error {
					return tt.unmountError
				}
			}

			credProvider := credentialprovider.New(client.CoreV1(), func() (string, error) {
				return dummyIMDSRegion, nil
			})
			s3paCache := &mounter.FakeCache{}

			unmounter := mounter.NewPodUnmounter(tt.nodeID, fakeMounter, podWatcher, s3paCache, credProvider, sourceMountDir)
			unmounter.HandleS3PodAttachmentUpdate(nil, tt.s3pa)

			unmountCalls := countUnmountCalls(fakeMounter)
			expectedUnmounts := 0
			if tt.expectUnmount {
				expectedUnmounts = 1
			}
			assert.Equals(t, expectedUnmounts, unmountCalls)
		})
	}
}

func TestCleanupDanglingMounts(t *testing.T) {
	tests := []struct {
		name          string
		pods          []*corev1.Pod
		s3paItems     []crdv1beta.MountpointS3PodAttachment
		unmountError  error
		expectedCalls int
	}{
		{
			name: "no dangling mounts",
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod1",
						Namespace: mountpointPodNamespace,
						UID:       "uid1",
					},
				},
			},
			s3paItems: []crdv1beta.MountpointS3PodAttachment{
				{
					Spec: crdv1beta.MountpointS3PodAttachmentSpec{
						MountpointS3PodAttachments: map[string][]crdv1beta.WorkloadAttachment{
							"pod1": []crdv1beta.WorkloadAttachment{crdv1beta.WorkloadAttachment{
								WorkloadPodUID: "workload1",
							}},
						},
					},
				},
			},
			expectedCalls: 0,
		},
		{
			name: "with dangling mount",
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod1",
						Namespace: mountpointPodNamespace,
						UID:       "uid1",
					},
				},
			},
			s3paItems: []crdv1beta.MountpointS3PodAttachment{
				{
					Spec: crdv1beta.MountpointS3PodAttachmentSpec{
						MountpointS3PodAttachments: map[string][]crdv1beta.WorkloadAttachment{
							"pod1": {},
						},
					},
				},
			},
			expectedCalls: 1,
		},
		{
			name: "with dangling mount and unmount error",
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod1",
						Namespace: mountpointPodNamespace,
						UID:       "uid1",
					},
				},
			},
			s3paItems: []crdv1beta.MountpointS3PodAttachment{
				{
					Spec: crdv1beta.MountpointS3PodAttachmentSpec{
						MountpointS3PodAttachments: map[string][]crdv1beta.WorkloadAttachment{
							"pod1": {},
						},
					},
				},
			},
			unmountError:  errors.New("unmount error"),
			expectedCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			podWatcher, client := setupPodWatcher(t, tt.pods...)
			kubeletPath := t.TempDir()
			t.Setenv("KUBELET_PATH", kubeletPath)
			t.Chdir(kubeletPath)
			sourceMountDir := t.TempDir()

			for _, pod := range tt.pods {
				podPath := filepath.Join(kubeletPath, "pods", string(pod.UID))
				commDir := mppod.PathOnHost(podPath)
				err := os.MkdirAll(commDir, 0750)
				assert.NoError(t, err)

				err = os.MkdirAll(filepath.Join(sourceMountDir, string(pod.UID)), 0750)
				assert.NoError(t, err)
			}

			fakeMounter := mount.NewFakeMounter(nil)
			if tt.unmountError != nil {
				fakeMounter.UnmountFunc = func(path string) error {
					return tt.unmountError
				}
			}

			s3paCache := &mounter.FakeCache{
				TestItems: tt.s3paItems,
			}

			credProvider := credentialprovider.New(client.CoreV1(), func() (string, error) {
				return dummyIMDSRegion, nil
			})

			unmounter := mounter.NewPodUnmounter(nodeName, fakeMounter, podWatcher, s3paCache, credProvider, sourceMountDir)
			unmounter.CleanupDanglingMounts()

			unmountCalls := countUnmountCalls(fakeMounter)
			assert.Equals(t, tt.expectedCalls, unmountCalls)
		})
	}
}
