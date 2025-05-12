package mounter_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/mounter"
	mpmounter "github.com/awslabs/aws-s3-csi-driver/pkg/mountpoint/mounter"
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

func dirExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}
	return info.IsDir()
}

func TestHandleS3PodAttachmentUpdate(t *testing.T) {
	tests := []struct {
		name                  string
		nodeID                string
		pod                   *corev1.Pod
		unmountError          error
		createSourcePath      bool
		isMountPoint          bool
		expectUnmount         bool
		expectSourcePathExist bool
	}{
		{
			name:   "different node",
			nodeID: "node1",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod1",
					Namespace: mountpointPodNamespace,
					UID:       "uid1",
					Annotations: map[string]string{
						mppod.AnnotationNeedsUnmount: "true",
					},
				},
				Spec: corev1.PodSpec{
					NodeName: "different-node",
				},
			},
			createSourcePath:      true,
			isMountPoint:          true,
			expectUnmount:         false,
			expectSourcePathExist: true,
		},
		{
			name:   "same node with unmount annotation",
			nodeID: nodeName,
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod1",
					Namespace: mountpointPodNamespace,
					UID:       "uid1",
					Annotations: map[string]string{
						mppod.AnnotationNeedsUnmount: "true",
					},
					Labels: map[string]string{
						mppod.LabelVolumeId: "vol1",
					},
				},
				Spec: corev1.PodSpec{
					NodeName: nodeName,
				},
			},
			createSourcePath:      true,
			isMountPoint:          true,
			expectUnmount:         true,
			expectSourcePathExist: false,
		},
		{
			name:   "same node without unmount annotation",
			nodeID: nodeName,
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod1",
					Namespace: mountpointPodNamespace,
					UID:       "uid1",
				},
				Spec: corev1.PodSpec{
					NodeName: nodeName,
				},
			},
			createSourcePath:      true,
			isMountPoint:          true,
			expectUnmount:         false,
			expectSourcePathExist: true,
		},
		{
			name:   "same node with unmount annotation, not mountpoint",
			nodeID: nodeName,
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod1",
					Namespace: mountpointPodNamespace,
					UID:       "uid1",
					Annotations: map[string]string{
						mppod.AnnotationNeedsUnmount: "true",
					},
					Labels: map[string]string{
						mppod.LabelVolumeId: "vol1",
					},
				},
				Spec: corev1.PodSpec{
					NodeName: nodeName,
				},
			},
			createSourcePath:      true,
			isMountPoint:          false,
			expectUnmount:         false,
			expectSourcePathExist: false,
		},
		{
			name:   "same node with unmount annotation, no source directory",
			nodeID: nodeName,
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod1",
					Namespace: mountpointPodNamespace,
					UID:       "uid1",
					Annotations: map[string]string{
						mppod.AnnotationNeedsUnmount: "true",
					},
					Labels: map[string]string{
						mppod.LabelVolumeId: "vol1",
					},
				},
				Spec: corev1.PodSpec{
					NodeName: nodeName,
				},
			},
			createSourcePath:      false,
			isMountPoint:          false,
			expectUnmount:         false,
			expectSourcePathExist: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kubeletPath := t.TempDir()
			parentDir, err := filepath.EvalSymlinks(filepath.Dir(kubeletPath))
			assert.NoError(t, err)
			kubeletPath = filepath.Join(parentDir, filepath.Base(kubeletPath))
			t.Setenv("KUBELET_PATH", kubeletPath)
			t.Chdir(kubeletPath)

			sourceMountDir := mounter.SourceMountDir(kubeletPath)

			podWatcher, client := setupPodWatcher(t, tt.pod)

			fakeMounter := mount.NewFakeMounter(nil)
			if tt.unmountError != nil {
				fakeMounter.UnmountFunc = func(path string) error {
					return tt.unmountError
				}
			}

			if tt.pod != nil {
				podPath := filepath.Join(kubeletPath, "pods", string(tt.pod.UID))
				commDir := mppod.PathOnHost(podPath)
				err := os.MkdirAll(commDir, 0750)
				assert.NoError(t, err)

				if tt.createSourcePath {
					sourcePath := filepath.Join(sourceMountDir, string(tt.pod.Name))
					err = os.MkdirAll(sourcePath, 0750)
					assert.NoError(t, err)

					if tt.isMountPoint {
						fakeMounter.Mount("mountpoint-s3", sourcePath, "fuse", []string{})
					}
				}
			}

			credProvider := credentialprovider.New(client.CoreV1(), func() (string, error) {
				return dummyIMDSRegion, nil
			})

			unmounter := mounter.NewPodUnmounter(tt.nodeID, mpmounter.NewWithMount(fakeMounter), podWatcher, credProvider)
			unmounter.HandleMountpointPodUpdate(nil, tt.pod)

			unmountCalls := countUnmountCalls(fakeMounter)
			expectedUnmounts := 0
			if tt.expectUnmount {
				expectedUnmounts = 1
			}
			assert.Equals(t, expectedUnmounts, unmountCalls)
			assert.Equals(t, tt.expectSourcePathExist, dirExists(filepath.Join(sourceMountDir, string(tt.pod.Name))))
		})
	}
}

func TestCleanupDanglingMounts(t *testing.T) {
	tests := []struct {
		name          string
		pods          []*corev1.Pod
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
					Spec: corev1.PodSpec{
						NodeName: nodeName,
					},
				},
			},
			expectedCalls: 0,
		},
		{
			name: "pod marked for unmount",
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod1",
						Namespace: mountpointPodNamespace,
						UID:       "uid1",
						Annotations: map[string]string{
							mppod.AnnotationNeedsUnmount: "true",
						},
						Labels: map[string]string{
							mppod.LabelVolumeId: "vol1",
						},
					},
					Spec: corev1.PodSpec{
						NodeName: nodeName,
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
						Annotations: map[string]string{
							mppod.AnnotationNeedsUnmount: "true",
						},
						Labels: map[string]string{
							mppod.LabelVolumeId: "vol1",
						},
					},
					Spec: corev1.PodSpec{
						NodeName: nodeName,
					},
				},
			},
			unmountError:  errors.New("unmount error"),
			expectedCalls: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kubeletPath := t.TempDir()
			parentDir, err := filepath.EvalSymlinks(filepath.Dir(kubeletPath))
			assert.NoError(t, err)
			kubeletPath = filepath.Join(parentDir, filepath.Base(kubeletPath))
			t.Setenv("KUBELET_PATH", kubeletPath)
			t.Chdir(kubeletPath)

			sourceMountDir := mounter.SourceMountDir(kubeletPath)

			fakeMounter := mount.NewFakeMounter(nil)
			if tt.unmountError != nil {
				fakeMounter.UnmountFunc = func(path string) error {
					return tt.unmountError
				}
			}

			for _, pod := range tt.pods {
				podPath := filepath.Join(kubeletPath, "pods", string(pod.UID))
				commDir := mppod.PathOnHost(podPath)
				err := os.MkdirAll(commDir, 0750)
				assert.NoError(t, err)

				sourcePath := filepath.Join(sourceMountDir, string(pod.Name))
				err = os.MkdirAll(sourcePath, 0750)
				assert.NoError(t, err)

				fakeMounter.Mount("mountpoint-s3", sourcePath, "fuse", []string{})
			}

			podWatcher, client := setupPodWatcher(t, tt.pods...)
			credProvider := credentialprovider.New(client.CoreV1(), func() (string, error) {
				return dummyIMDSRegion, nil
			})

			unmounter := mounter.NewPodUnmounter(nodeName, mpmounter.NewWithMount(fakeMounter), podWatcher, credProvider)
			err = unmounter.CleanupDanglingMounts()
			assert.NoError(t, err)

			unmountCalls := countUnmountCalls(fakeMounter)
			assert.Equals(t, tt.expectedCalls, unmountCalls)
		})
	}
}
