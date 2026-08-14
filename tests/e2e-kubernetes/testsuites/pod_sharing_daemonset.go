package custom_testsuites

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
)

type s3CSIPodSharingDaemonsetTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

func InitS3CSIPodSharingDaemonsetTestSuite() storageframework.TestSuite {
	return &s3CSIPodSharingDaemonsetTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name: "podsharingdaemonset",
			TestPatterns: []storageframework.TestPattern{
				storageframework.DefaultFsPreprovisionedPV,
			},
		},
	}
}

func (t *s3CSIPodSharingDaemonsetTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

func (t *s3CSIPodSharingDaemonsetTestSuite) SkipUnsupportedTests(_ storageframework.TestDriver, _ storageframework.TestPattern) {
}

func (t *s3CSIPodSharingDaemonsetTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	type local struct {
		resources []*storageframework.VolumeResource
		config    *storageframework.PerTestConfig
	}
	var l local

	f := framework.NewFrameworkWithCustomTimeouts(NamespacePrefix+"podsharing-ds", storageframework.GetDriverTimeouts(driver))
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

	cleanup := func(ctx context.Context) {
		var errs []error
		for _, resource := range l.resources {
			errs = append(errs, resource.CleanupResource(ctx))
		}
		framework.ExpectNoError(errors.NewAggregate(errs), "while cleanup resource")
	}

	ginkgo.Describe("Pod Sharing (Daemonset Architecture)", func() {
		ginkgo.BeforeEach(func(ctx context.Context) {
			l = local{}
			l.config = driver.PrepareTest(ctx, f)
			ginkgo.DeferCleanup(cleanup)
		})

		ginkgo.It("should share data between two pods on the same node via shared mount", func(ctx context.Context) {
			resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
			l.resources = append(l.resources, resource)

			_, pods := createPodsOnSameNode(ctx, f, 2, resource)
			defer deletePodsInOrder(ctx, f, pods)

			checkCrossReadWriteDaemonset(ctx, f, pods[0], pods[1])
		})

		ginkgo.It("should keep second pod serving after first pod is deleted", func(ctx context.Context) {
			resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
			l.resources = append(l.resources, resource)

			_, pods := createPodsOnSameNode(ctx, f, 2, resource)
			defer deletePodsInOrder(ctx, f, pods)

			// Verify both pods can read/write
			checkCrossReadWriteDaemonset(ctx, f, pods[0], pods[1])

			// Delete first pod
			ginkgo.By("Deleting the first pod")
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pods[0]))

			// Second pod should still work
			ginkgo.By("Verifying second pod can still read and write after first pod deletion")
			toWrite := 1024
			path := "/mnt/volume1/after-pod1-delete.txt"
			seed := time.Now().UTC().UnixNano()
			checkWriteToPathSucceedEventually(ctx, f, pods[1], path, toWrite, seed)
			checkReadFromPathSucceedEventually(ctx, f, pods[1], path, toWrite, seed)
		})

		ginkgo.It("should use only a single mount-s3 process for pods sharing the same PV", func(ctx context.Context) {
			resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
			l.resources = append(l.resources, resource)

			targetNode, pods := createPodsOnSameNode(ctx, f, 3, resource)
			defer deletePodsInOrder(ctx, f, pods)

			// All pods should be able to read/write
			checkCrossReadWriteDaemonset(ctx, f, pods[0], pods[1])

			// Assert only one FUSE mount exists for this volume by checking mount table
			// The source mount path is /var/lib/kubelet/plugins/s3.csi.aws.com/mnt/<pvName>
			ginkgo.By("Verifying only one FUSE source mount exists in the mount table")
			pvName := resource.Pv.Name
			dumpMountTable(ctx, f, targetNode, "single mount-s3 check")
			fuseCount := countFuseMountsForVolume(ctx, f, targetNode, pvName)
			gomega.Expect(fuseCount).To(gomega.Equal(1),
				"expected exactly 1 FUSE source mount for volume %s, got %d", pvName, fuseCount)
		})

		ginkgo.It("should reject second pod with different fsGroup on the same PV", func(ctx context.Context) {
			resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
			l.resources = append(l.resources, resource)

			// First pod with fsGroup 1000
			ginkgo.By("Creating first pod with fsGroup 1000")
			pod1 := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelBaseline, "")
			pod1.Spec.SecurityContext = &v1.PodSecurityContext{FSGroup: ptrInt64(1000)}
			pod1, err := createPod(ctx, f.ClientSet, f.Namespace.Name, pod1)
			framework.ExpectNoError(err)
			targetNode := pod1.Spec.NodeName
			defer func() { e2epod.DeletePodWithWait(ctx, f.ClientSet, pod1) }()

			// Verify first pod works
			checkWriteToPathSucceedEventually(ctx, f, pod1, "/mnt/volume1/from-pod1.txt", 512, time.Now().UTC().UnixNano())

			// Second pod with different fsGroup 2000 — should fail to mount
			ginkgo.By("Creating second pod with fsGroup 2000 on the same node (should fail)")
			pod2 := e2epod.MakePod(f.Namespace.Name, map[string]string{"kubernetes.io/hostname": targetNode}, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelBaseline, "")
			pod2.Spec.SecurityContext = &v1.PodSecurityContext{FSGroup: ptrInt64(2000)}
			pod2, err = createPodWithoutWaiting(ctx, f.ClientSet, f.Namespace.Name, pod2)
			framework.ExpectNoError(err)
			defer func() { e2epod.DeletePodWithWait(ctx, f.ClientSet, pod2) }()

			// Wait and verify the pod does NOT become Running (mount should be rejected)
			ginkgo.By("Verifying second pod fails to start due to fsGroup mismatch")
			assertPodFailsToMount(ctx, f, pod2, "cannot share mount")
		})

		ginkgo.It("should allow different fsGroup after previous pod is deleted", func(ctx context.Context) {
			resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
			l.resources = append(l.resources, resource)

			// First pod with fsGroup 1000
			ginkgo.By("Creating first pod with fsGroup 1000")
			pod1 := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelBaseline, "")
			pod1.Spec.SecurityContext = &v1.PodSecurityContext{FSGroup: ptrInt64(1000)}
			pod1, err := createPod(ctx, f.ClientSet, f.Namespace.Name, pod1)
			framework.ExpectNoError(err)
			targetNode := pod1.Spec.NodeName

			// Verify first pod works
			checkWriteToPathSucceedEventually(ctx, f, pod1, "/mnt/volume1/from-fsgroup-1000.txt", 512, time.Now().UTC().UnixNano())

			// Delete the first pod — entry should reset
			ginkgo.By("Deleting first pod to release the mount")
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod1))

			// Second pod with different fsGroup 2000 — should now succeed
			ginkgo.By("Creating second pod with fsGroup 2000 on the same node (should succeed after deletion)")
			pod2 := e2epod.MakePod(f.Namespace.Name, map[string]string{"kubernetes.io/hostname": targetNode}, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelBaseline, "")
			pod2.Spec.SecurityContext = &v1.PodSecurityContext{FSGroup: ptrInt64(2000)}
			pod2, err = createPod(ctx, f.ClientSet, f.Namespace.Name, pod2)
			framework.ExpectNoError(err)
			defer func() { e2epod.DeletePodWithWait(ctx, f.ClientSet, pod2) }()

			// Verify the new pod can read/write
			ginkgo.By("Verifying second pod with different fsGroup can read and write")
			checkWriteToPathSucceedEventually(ctx, f, pod2, "/mnt/volume1/from-fsgroup-2000.txt", 512, time.Now().UTC().UnixNano())
		})

		ginkgo.It("should allow pods with different service accounts to share mount with driver auth", func(ctx context.Context) {
			resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
			l.resources = append(l.resources, resource)

			// Create a second service account in the namespace
			ginkgo.By("Creating a second service account")
			sa2 := &v1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "s3-e2e-sa-different",
					Namespace: f.Namespace.Name,
				},
			}
			sa2, err := f.ClientSet.CoreV1().ServiceAccounts(f.Namespace.Name).Create(ctx, sa2, metav1.CreateOptions{})
			framework.ExpectNoError(err)

			// First pod with default SA
			ginkgo.By("Creating first pod with default service account")
			pod1 := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelBaseline, "")
			pod1, err = createPod(ctx, f.ClientSet, f.Namespace.Name, pod1)
			framework.ExpectNoError(err)
			targetNode := pod1.Spec.NodeName
			defer func() { e2epod.DeletePodWithWait(ctx, f.ClientSet, pod1) }()

			// Verify first pod works
			checkWriteToPathSucceedEventually(ctx, f, pod1, "/mnt/volume1/from-sa-default.txt", 512, time.Now().UTC().UnixNano())

			// Second pod with different SA — should succeed because driver auth doesn't enforce SA match
			ginkgo.By("Creating second pod with different service account on the same node (should succeed with driver auth)")
			pod2 := e2epod.MakePod(f.Namespace.Name, map[string]string{"kubernetes.io/hostname": targetNode}, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelBaseline, "")
			pod2.Spec.ServiceAccountName = sa2.Name
			pod2, err = createPod(ctx, f.ClientSet, f.Namespace.Name, pod2)
			framework.ExpectNoError(err)
			defer func() { e2epod.DeletePodWithWait(ctx, f.ClientSet, pod2) }()

			// Both pods should be able to read/write
			ginkgo.By("Verifying both pods can read and write to shared volume")
			seed := time.Now().UTC().UnixNano()
			checkWriteToPathSucceedEventually(ctx, f, pod2, "/mnt/volume1/from-sa-different.txt", 512, seed)
			checkReadFromPathSucceedEventually(ctx, f, pod1, "/mnt/volume1/from-sa-different.txt", 512, seed)
		})

		ginkgo.It("should handle concurrent pod creation on the same node sharing same volume", func(ctx context.Context) {
			resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
			l.resources = append(l.resources, resource)

			// Get a target node by creating first pod
			ginkgo.By("Creating first pod to identify target node")
			pod1 := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelBaseline, "")
			pod1, err := createPod(ctx, f.ClientSet, f.Namespace.Name, pod1)
			framework.ExpectNoError(err)
			targetNode := pod1.Spec.NodeName
			defer func() { e2epod.DeletePodWithWait(ctx, f.ClientSet, pod1) }()

			// Concurrently create 4 more pods on the same node
			ginkgo.By(fmt.Sprintf("Concurrently creating 4 pods on node %s", targetNode))
			const concurrentPods = 4
			var wg sync.WaitGroup
			podsCh := make(chan *v1.Pod, concurrentPods)
			errsCh := make(chan error, concurrentPods)

			for i := range concurrentPods {
				wg.Add(1)
				go func(idx int) {
					defer wg.Done()
					pod := e2epod.MakePod(f.Namespace.Name, map[string]string{"kubernetes.io/hostname": targetNode}, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelBaseline, "")
					pod, err := createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
					if err != nil {
						errsCh <- fmt.Errorf("pod %d creation failed: %w", idx, err)
						return
					}
					podsCh <- pod
				}(i)
			}
			wg.Wait()
			close(podsCh)
			close(errsCh)

			// Collect errors
			for err := range errsCh {
				framework.ExpectNoError(err)
			}

			// Collect pods
			var concPods []*v1.Pod
			for pod := range podsCh {
				concPods = append(concPods, pod)
				defer func(p *v1.Pod) { e2epod.DeletePodWithWait(ctx, f.ClientSet, p) }(pod)
			}
			gomega.Expect(len(concPods)).To(gomega.Equal(concurrentPods))

			// All pods should be able to read a file written by pod1
			ginkgo.By("Verifying all concurrent pods can access shared data")
			toWrite := 1024
			sharedFile := "/mnt/volume1/concurrent-shared.txt"
			seed := time.Now().UTC().UnixNano()
			checkWriteToPathSucceedEventually(ctx, f, pod1, sharedFile, toWrite, seed)

			for i, pod := range concPods {
				ginkgo.By(fmt.Sprintf("Verifying concurrent pod %d can read shared file", i))
				checkReadFromPathSucceedEventually(ctx, f, pod, sharedFile, toWrite, seed)
			}
		})

		ginkgo.It("should handle concurrent mount and unmount of pods sharing the same volume", func(ctx context.Context) {
			resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
			l.resources = append(l.resources, resource)

			// Create initial pods
			_, pods := createPodsOnSameNode(ctx, f, 3, resource)
			targetNode := pods[0].Spec.NodeName

			// Write a file from pod 0
			toWrite := 1024
			sharedFile := "/mnt/volume1/churn-test.txt"
			seed := time.Now().UTC().UnixNano()
			checkWriteToPathSucceedEventually(ctx, f, pods[0], sharedFile, toWrite, seed)

			// Delete pod 0 and pod 1 while simultaneously creating 2 new pods
			ginkgo.By("Concurrently deleting 2 pods and creating 2 new pods")
			var wg sync.WaitGroup
			newPodsCh := make(chan *v1.Pod, 2)
			deleteErrsCh := make(chan error, 2)
			createErrsCh := make(chan error, 2)

			// Delete pods 0 and 1
			for i := range 2 {
				wg.Add(1)
				go func(idx int) {
					defer wg.Done()
					if err := e2epod.DeletePodWithWait(ctx, f.ClientSet, pods[idx]); err != nil {
						deleteErrsCh <- fmt.Errorf("delete pod %d failed: %w", idx, err)
					}
				}(i)
			}

			// Create 2 new pods on the same node
			for i := range 2 {
				wg.Add(1)
				go func(idx int) {
					defer wg.Done()
					pod := e2epod.MakePod(f.Namespace.Name, map[string]string{"kubernetes.io/hostname": targetNode}, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelBaseline, "")
					pod, err := createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
					if err != nil {
						createErrsCh <- fmt.Errorf("create new pod %d failed: %w", idx, err)
						return
					}
					newPodsCh <- pod
				}(i)
			}
			wg.Wait()
			close(newPodsCh)
			close(deleteErrsCh)
			close(createErrsCh)

			for err := range deleteErrsCh {
				framework.ExpectNoError(err)
			}
			for err := range createErrsCh {
				framework.ExpectNoError(err)
			}

			var newPods []*v1.Pod
			for pod := range newPodsCh {
				newPods = append(newPods, pod)
				defer func(p *v1.Pod) { e2epod.DeletePodWithWait(ctx, f.ClientSet, p) }(pod)
			}

			// pod 2 (never deleted) should still have access
			ginkgo.By("Verifying surviving pod still has access")
			checkReadFromPathSucceedEventually(ctx, f, pods[2], sharedFile, toWrite, seed)

			// New pods should also have access
			for i, pod := range newPods {
				ginkgo.By(fmt.Sprintf("Verifying new pod %d can read shared file", i))
				checkReadFromPathSucceedEventually(ctx, f, pod, sharedFile, toWrite, seed)
			}

			// Clean up pod 2
			defer func() { e2epod.DeletePodWithWait(ctx, f.ClientSet, pods[2]) }()
		})

		ginkgo.It("should handle rapid pod churn on the same volume", func(ctx context.Context) {
			resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
			l.resources = append(l.resources, resource)

			// Create a pod, write data, delete it — repeat N times. Final pod should read all data.
			const iterations = 5
			var targetNode string

			for i := range iterations {
				ginkgo.By(fmt.Sprintf("Churn iteration %d: creating pod", i+1))
				var nodeSelector map[string]string
				if targetNode != "" {
					nodeSelector = map[string]string{"kubernetes.io/hostname": targetNode}
				}
				pod := e2epod.MakePod(f.Namespace.Name, nodeSelector, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelBaseline, "")
				pod, err := createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
				framework.ExpectNoError(err)

				if targetNode == "" {
					targetNode = pod.Spec.NodeName
				}

				// Write a unique file
				path := fmt.Sprintf("/mnt/volume1/churn-%d.txt", i)
				seed := time.Now().UTC().UnixNano() + int64(i)
				checkWriteToPathSucceedEventually(ctx, f, pod, path, 512, seed)

				ginkgo.By(fmt.Sprintf("Churn iteration %d: deleting pod", i+1))
				framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
			}

			// Create final pod and verify all files exist
			ginkgo.By("Creating final pod to verify all churned files are readable")
			finalPod := e2epod.MakePod(f.Namespace.Name, map[string]string{"kubernetes.io/hostname": targetNode}, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelBaseline, "")
			finalPod, err := createPod(ctx, f.ClientSet, f.Namespace.Name, finalPod)
			framework.ExpectNoError(err)
			defer func() { e2epod.DeletePodWithWait(ctx, f.ClientSet, finalPod) }()

			for i := range iterations {
				path := fmt.Sprintf("/mnt/volume1/churn-%d.txt", i)
				ginkgo.By(fmt.Sprintf("Verifying file %s exists", path))
				checkListingPathSucceed(ctx, f, finalPod, path)
			}
		})

		ginkgo.It("should allow N pods to concurrently write different files to the shared volume", func(ctx context.Context) {
			resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
			l.resources = append(l.resources, resource)

			const podCount = 5
			_, pods := createPodsOnSameNode(ctx, f, podCount, resource)
			defer deletePodsInOrder(ctx, f, pods)

			// Each pod writes a unique file concurrently
			ginkgo.By(fmt.Sprintf("Having %d pods concurrently write unique files", podCount))
			toWrite := 1024
			seeds := make([]int64, podCount)
			var wg sync.WaitGroup
			for i := range podCount {
				seeds[i] = time.Now().UTC().UnixNano() + int64(i)
				wg.Add(1)
				go func(idx int) {
					defer ginkgo.GinkgoRecover()
					defer wg.Done()
					path := fmt.Sprintf("/mnt/volume1/pod%d-file.txt", idx)
					checkWriteToPathSucceedEventually(ctx, f, pods[idx], path, toWrite, seeds[idx])
				}(i)
			}
			wg.Wait()

			// Each pod reads all other pods' files
			ginkgo.By("Verifying all pods can read all files")
			for reader := range podCount {
				for writer := range podCount {
					path := fmt.Sprintf("/mnt/volume1/pod%d-file.txt", writer)
					checkReadFromPathSucceedEventually(ctx, f, pods[reader], path, toWrite, seeds[writer])
				}
			}
		})

		ginkgo.It("should survive deleting pods in reverse order", func(ctx context.Context) {
			resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
			l.resources = append(l.resources, resource)

			const podCount = 4
			_, pods := createPodsOnSameNode(ctx, f, podCount, resource)

			// Write from first pod
			toWrite := 1024
			sharedFile := "/mnt/volume1/reverse-delete.txt"
			seed := time.Now().UTC().UnixNano()
			checkWriteToPathSucceedEventually(ctx, f, pods[0], sharedFile, toWrite, seed)

			// Delete pods in reverse order (last created first), verifying remaining pods still work
			for i := podCount - 1; i > 0; i-- {
				ginkgo.By(fmt.Sprintf("Deleting pod %d (of %d remaining)", i, i+1))
				framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pods[i]))

				// Verify pod 0 (first pod) still has access
				ginkgo.By(fmt.Sprintf("Verifying pod 0 still has access after deleting pod %d", i))
				checkReadFromPathSucceedEventually(ctx, f, pods[0], sharedFile, toWrite, seed)
			}

			// Finally delete pod 0
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pods[0]))
		})

		ginkgo.It("should handle pod recreation on same node after all pods are deleted", func(ctx context.Context) {
			resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
			l.resources = append(l.resources, resource)

			// First generation: create 2 pods, write data, delete both
			targetNode, pods := createPodsOnSameNode(ctx, f, 2, resource)

			toWrite := 1024
			file1 := "/mnt/volume1/gen1-file.txt"
			seed1 := time.Now().UTC().UnixNano()
			checkWriteToPathSucceedEventually(ctx, f, pods[0], file1, toWrite, seed1)
			checkReadFromPathSucceedEventually(ctx, f, pods[1], file1, toWrite, seed1)

			ginkgo.By("Deleting all first-generation pods")
			for _, pod := range pods {
				framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
			}

			// Second generation: create 2 new pods on same node, verify old data still readable
			ginkgo.By("Creating second-generation pods on the same node")
			var gen2Pods []*v1.Pod
			for i := range 2 {
				pod := e2epod.MakePod(f.Namespace.Name, map[string]string{"kubernetes.io/hostname": targetNode}, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelBaseline, "")
				pod, err := createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
				framework.ExpectNoError(err, "failed to create gen2 pod %d", i)
				gen2Pods = append(gen2Pods, pod)
				defer func(p *v1.Pod) { e2epod.DeletePodWithWait(ctx, f.ClientSet, p) }(pod)
			}

			// Gen2 pods should be able to read gen1 data (persisted in S3)
			ginkgo.By("Verifying second-generation pods can read first-generation data")
			for i, pod := range gen2Pods {
				ginkgo.By(fmt.Sprintf("Gen2 pod %d reading gen1 file", i))
				checkReadFromPathSucceedEventually(ctx, f, pod, file1, toWrite, seed1)
			}

			// Gen2 pods should share with each other
			file2 := "/mnt/volume1/gen2-file.txt"
			seed2 := time.Now().UTC().UnixNano()
			checkWriteToPathSucceedEventually(ctx, f, gen2Pods[0], file2, toWrite, seed2)
			checkReadFromPathSucceedEventually(ctx, f, gen2Pods[1], file2, toWrite, seed2)
		})

		ginkgo.It("should recover mount map after CSI node pod restart and existing pods keep working", ginkgo.Serial, func(ctx context.Context) {
			resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
			l.resources = append(l.resources, resource)

			// Create 2 pods sharing the volume
			targetNode, pods := createPodsOnSameNode(ctx, f, 2, resource)
			defer deletePodsInOrder(ctx, f, pods)

			// Write data from pod 0
			toWrite := 1024
			sharedFile := "/mnt/volume1/pre-restart.txt"
			seed := time.Now().UTC().UnixNano()
			checkWriteToPathSucceedEventually(ctx, f, pods[0], sharedFile, toWrite, seed)
			checkReadFromPathSucceedEventually(ctx, f, pods[1], sharedFile, toWrite, seed)

			// Kill the CSI node pod on this node (only the node pod, not the mounter)
			ginkgo.By(fmt.Sprintf("Killing CSI node pod on node %s to lose in-memory mount map", targetNode))
			killCSINodePodOnNode(ctx, f, targetNode)

			// Wait for CSI node pod to come back (RebuildMountMap runs on startup)
			ginkgo.By("Waiting for CSI node pod to restart and rebuild mount map")
			waitForCSINodePodReady(ctx, f, targetNode)
			waitForCSINodePodStable(ctx, f, targetNode)

			// Existing pods should still work — FUSE mounts never died (mounter pod stayed up)
			// Use Eventually variant because reads can transiently fail with "Transport endpoint
			// is not connected" for a few seconds after CSI node pod restart.
			ginkgo.By("Verifying existing pods still have working mounts after CSI node restart")
			checkReadFromPathSucceedEventually(ctx, f, pods[0], sharedFile, toWrite, seed)
			checkReadFromPathSucceedEventually(ctx, f, pods[1], sharedFile, toWrite, seed)

			// Write new data to verify write path also works
			postRestartFile := "/mnt/volume1/post-restart.txt"
			postSeed := time.Now().UTC().UnixNano()
			checkWriteToPathSucceedEventually(ctx, f, pods[0], postRestartFile, toWrite, postSeed)
			checkReadFromPathSucceedEventually(ctx, f, pods[1], postRestartFile, toWrite, postSeed)
		})

		ginkgo.It("should allow new pod to join shared mount after CSI node pod restart", ginkgo.Serial, func(ctx context.Context) {
			resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
			l.resources = append(l.resources, resource)

			// Create 1 pod and write data
			targetNode, pods := createPodsOnSameNode(ctx, f, 1, resource)
			defer deletePodsInOrder(ctx, f, pods)

			toWrite := 1024
			sharedFile := "/mnt/volume1/before-restart.txt"
			seed := time.Now().UTC().UnixNano()
			checkWriteToPathSucceedEventually(ctx, f, pods[0], sharedFile, toWrite, seed)

			// Kill CSI node pod
			ginkgo.By(fmt.Sprintf("Killing CSI node pod on node %s", targetNode))
			killCSINodePodOnNode(ctx, f, targetNode)
			waitForCSINodePodReady(ctx, f, targetNode)
			waitForCSINodePodStable(ctx, f, targetNode)

			// Create a new pod on the same node — should join the existing share
			// (mount map was rebuilt, so it knows about the existing source mount)
			ginkgo.By("Creating new pod after CSI node restart — should share existing mount")
			pod2 := e2epod.MakePod(f.Namespace.Name, map[string]string{"kubernetes.io/hostname": targetNode}, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelBaseline, "")
			pod2, err := createPod(ctx, f.ClientSet, f.Namespace.Name, pod2)
			framework.ExpectNoError(err)
			defer func() { e2epod.DeletePodWithWait(ctx, f.ClientSet, pod2) }()

			// New pod should read data written before the restart
			ginkgo.By("Verifying new pod can read data written before CSI node restart")
			checkReadFromPathSucceedEventually(ctx, f, pod2, sharedFile, toWrite, seed)

			// Verify still only 1 FUSE mount (sharing, not a new one)
			ginkgo.By("Verifying only one FUSE source mount exists (mount was shared, not duplicated)")
			pvName := resource.Pv.Name
			fuseCount := countFuseMountsForVolume(ctx, f, targetNode, pvName)
			gomega.Expect(fuseCount).To(gomega.Equal(1),
				"expected exactly 1 FUSE source mount for volume %s after restart, got %d", pvName, fuseCount)
		})

		ginkgo.It("should correctly unmount after CSI node pod restart with recovered refcount", ginkgo.Serial, func(ctx context.Context) {
			resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
			l.resources = append(l.resources, resource)

			// Create 3 pods sharing the volume
			targetNode, pods := createPodsOnSameNode(ctx, f, 3, resource)

			toWrite := 1024
			sharedFile := "/mnt/volume1/refcount-test.txt"
			seed := time.Now().UTC().UnixNano()
			checkWriteToPathSucceedEventually(ctx, f, pods[0], sharedFile, toWrite, seed)

			// Kill CSI node pod — refcount lost from memory
			ginkgo.By(fmt.Sprintf("Killing CSI node pod on node %s", targetNode))
			killCSINodePodOnNode(ctx, f, targetNode)
			waitForCSINodePodReady(ctx, f, targetNode)
			waitForCSINodePodStable(ctx, f, targetNode)

			// Delete pod 0 and pod 1 — refcount should decrement correctly from recovered state
			ginkgo.By("Deleting first two pods after restart")
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pods[0]))
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pods[1]))

			// Pod 2 should still have a working mount (source not torn down because refcount > 0)
			ginkgo.By("Verifying surviving pod still has access after two siblings unmounted post-restart")
			checkReadFromPathSucceedEventually(ctx, f, pods[2], sharedFile, toWrite, seed)

			// Clean up pod 2
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pods[2]))
		})

		ginkgo.It("should persist meta file on disk for mounted volumes", func(ctx context.Context) {
			resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
			l.resources = append(l.resources, resource)

			targetNode, pods := createPodsOnSameNode(ctx, f, 1, resource)
			defer deletePodsInOrder(ctx, f, pods)

			// Verify .meta.json file exists on the node by exec'ing into CSI node pod
			pvName := resource.Pv.Name
			ginkgo.By(fmt.Sprintf("Verifying .meta.json exists for volume %s", pvName))
			metaExists := checkMetaFileExists(ctx, f, targetNode, pvName)
			gomega.Expect(metaExists).To(gomega.BeTrue(),
				"expected .meta.json file to exist for volume %s", pvName)
		})

		ginkgo.It("should clean up meta file, credentials, and source mount after last consumer unmounts", func(ctx context.Context) {
			resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
			l.resources = append(l.resources, resource)

			targetNode, pods := createPodsOnSameNode(ctx, f, 1, resource)
			pvName := resource.Pv.Name
			bucketName := resource.Pv.Spec.CSI.VolumeHandle

			// Meta file should exist while pod is running
			ginkgo.By("Verifying meta file exists while pod is mounted")
			metaExists := checkMetaFileExists(ctx, f, targetNode, pvName)
			gomega.Expect(metaExists).To(gomega.BeTrue())

			// Credential directory should exist while pod is mounted
			ginkgo.By("Verifying credential directory exists in mounter pod while pod is mounted")
			listCommDirContents(ctx, f, targetNode, pvName)
			credDirExists := checkCredentialDirExists(ctx, f, targetNode, pvName)
			gomega.Expect(credDirExists).To(gomega.BeTrue(),
				"expected credential directory for volume %s to exist while pod is mounted", pvName)

			// FUSE source should exist in mount table
			ginkgo.By("Verifying FUSE source mount exists while pod is mounted")
			dumpMountTable(ctx, f, targetNode, "while pod mounted")
			fuseCount := countFuseMountsForVolume(ctx, f, targetNode, pvName)
			gomega.Expect(fuseCount).To(gomega.BeNumerically(">=", 1))

			// mount-s3 process should be running while pod is mounted
			ginkgo.By("Verifying mount-s3 process exists while pod is mounted")
			dumpMountpointProcesses(ctx, f, targetNode, "while pod mounted")
			framework.Gomega().Eventually(ctx, func(ctx context.Context) (int, error) {
				count := countMountpointProcessesForVolume(ctx, f, targetNode, bucketName)
				return count, nil
			}).WithTimeout(60 * time.Second).WithPolling(5 * time.Second).Should(gomega.Equal(1))

			// Delete the pod — last consumer, should clean up meta AND source mount
			ginkgo.By("Deleting the last pod and verifying meta file and source mount are removed")
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pods[0]))

			// Give a moment for unmount to complete
			time.Sleep(5 * time.Second)

			metaExists = checkMetaFileExists(ctx, f, targetNode, pvName)
			gomega.Expect(metaExists).To(gomega.BeFalse(),
				"expected .meta.json file to be removed after last consumer unmounts")

			// Source mount should be gone from mount table
			ginkgo.By("Verifying FUSE source mount is removed from mount table after last consumer unmounts")
			dumpMountTable(ctx, f, targetNode, "after last consumer unmounts")
			fuseCountAfter := countFuseMountsForVolume(ctx, f, targetNode, pvName)
			gomega.Expect(fuseCountAfter).To(gomega.Equal(0),
				"expected 0 FUSE source mounts after last consumer unmounts, got %d", fuseCountAfter)

			// Verify mount-s3 process for this volume has terminated in the mounter pod
			ginkgo.By("Verifying mount-s3 process terminated after last consumer unmounts")
			dumpMountpointProcesses(ctx, f, targetNode, "after last consumer unmounts")
			framework.Gomega().Eventually(ctx, func(ctx context.Context) (int, error) {
				count := countMountpointProcessesForVolume(ctx, f, targetNode, bucketName)
				return count, nil
			}).WithTimeout(60 * time.Second).WithPolling(5 * time.Second).Should(gomega.Equal(0))

			// Verify credential directory is cleaned up from mounter pod's comm volume
			ginkgo.By("Verifying credential directory is removed from mounter pod after last consumer unmounts")
			checkCredentialDirRemoved(ctx, f, targetNode, pvName)
		})

		ginkgo.It("should recover from mounter pod crash with fresh source mount for new pods", ginkgo.Serial, func(ctx context.Context) {
			resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
			l.resources = append(l.resources, resource)

			// Create 2 pods sharing the volume and write data
			targetNode, pods := createPodsOnSameNode(ctx, f, 2, resource)
			defer deletePodsInOrder(ctx, f, pods)

			toWrite := 1024
			sharedFile := "/mnt/volume1/pre-crash-shared.txt"
			seed := time.Now().UTC().UnixNano()
			checkWriteToPathSucceedEventually(ctx, f, pods[0], sharedFile, toWrite, seed)
			checkReadFromPathSucceedEventually(ctx, f, pods[1], sharedFile, toWrite, seed)

			pvName := resource.Pv.Name

			// Capture device ID and meta mtime BEFORE crash for comparison after recovery
			ginkgo.By("Capturing FUSE source device ID and meta mtime before crash")
			dumpMountTable(ctx, f, targetNode, "BEFORE MOUNTER CRASH")
			fuseDeviceBefore := getFuseSourceDeviceID(ctx, f, targetNode, pvName)
			framework.Logf("FUSE device ID before crash: %s", fuseDeviceBefore)
			gomega.Expect(fuseDeviceBefore).ToNot(gomega.BeEmpty(), "FUSE source should exist before crash")
			metaMtimeBefore := getMetaFileMtime(ctx, f, targetNode, pvName)
			framework.Logf("Meta mtime before crash: %s", metaMtimeBefore)

			// Kill the mounter pod — all FUSE mounts die
			ginkgo.By(fmt.Sprintf("Killing mounter pod on node %s to crash all Mountpoint processes", targetNode))
			killMounterPodOnNode(ctx, f, targetNode)
			waitForMounterPodReady(ctx, f, targetNode)

			// Give the CSI node pod time to re-discover the comm dir after mounter pod recovery.
			// The comm dir re-discovery and mount handshake needs a few seconds.
			time.Sleep(15 * time.Second)

			// Existing pods have dead mounts
			ginkgo.By("Verifying existing pods have dead mounts after mounter crash")
			dumpMountTable(ctx, f, targetNode, "AFTER MOUNTER CRASH")
			assertPodIOFails(ctx, f, pods[0], "/mnt/volume1/")
			assertPodIOFails(ctx, f, pods[1], "/mnt/volume1/")

			// Create 3 new pods on the same node using the same PV — without deleting old pods.
			// This simulates scaling up new replicas while old broken pods still exist.
			// The CSI driver should detect dead source, create a fresh FUSE mount, and
			// bind-mount to each new pod's target.
			ginkgo.By("Creating 3 new pods after mounter crash (old pods still running with dead mounts)")
			var newPods []*v1.Pod
			for i := range 3 {
				pod := e2epod.MakePod(f.Namespace.Name, map[string]string{"kubernetes.io/hostname": targetNode}, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelBaseline, "")
				pod, err := createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
				framework.ExpectNoError(err, "failed to create new pod %d after mounter crash", i)
				newPods = append(newPods, pod)
			}
			defer func() {
				for _, p := range newPods {
					e2epod.DeletePodWithWait(ctx, f.ClientSet, p)
				}
			}()

			// All new pods should have healthy I/O
			ginkgo.By("Verifying all new pods can write and read data")
			for i, pod := range newPods {
				writeFile := fmt.Sprintf("/mnt/volume1/new-pod-%d.txt", i)
				writeSeed := time.Now().UTC().UnixNano() + int64(i)
				checkWriteToPathSucceedEventually(ctx, f, pod, writeFile, toWrite, writeSeed)
				checkReadFromPathSucceedEventually(ctx, f, pod, writeFile, toWrite, writeSeed)
			}

			// Cross-pod sharing should work: pod 0 writes, pod 1 and pod 2 read
			ginkgo.By("Verifying cross-pod sharing works among new pods")
			crossFile := "/mnt/volume1/cross-pod-after-crash.txt"
			crossSeed := time.Now().UTC().UnixNano()
			checkWriteToPathSucceedEventually(ctx, f, newPods[0], crossFile, toWrite, crossSeed)
			checkReadFromPathSucceedEventually(ctx, f, newPods[1], crossFile, toWrite, crossSeed)
			checkReadFromPathSucceedEventually(ctx, f, newPods[2], crossFile, toWrite, crossSeed)

			// Old data written before crash (persisted in S3) should be readable
			ginkgo.By("Verifying new pods can read data written before mounter crash")
			checkReadFromPathSucceedEventually(ctx, f, newPods[0], sharedFile, toWrite, seed)

			// Verify only 1 FUSE source mount exists (all 3 pods share the same source)
			ginkgo.By("Verifying only one FUSE source mount exists for all new pods")
			dumpMountTable(ctx, f, targetNode, "AFTER RECOVERY (3 new pods)")
			fuseCount := countFuseMountsForVolume(ctx, f, targetNode, pvName)
			gomega.Expect(fuseCount).To(gomega.Equal(1),
				"expected exactly 1 FUSE source mount for volume %s after recovery with 3 pods, got %d", pvName, fuseCount)

			// Verify the new source mount has a different device ID (fresh FUSE mount, not the dead one)
			ginkgo.By("Verifying fresh source mount has a new device ID")
			fuseDeviceAfter := getFuseSourceDeviceID(ctx, f, targetNode, pvName)
			framework.Logf("FUSE device ID — before crash: %s, after recovery: %s", fuseDeviceBefore, fuseDeviceAfter)
			gomega.Expect(fuseDeviceAfter).ToNot(gomega.BeEmpty(), "expected FUSE source to have a device ID after recovery")
			gomega.Expect(fuseDeviceAfter).ToNot(gomega.Equal(fuseDeviceBefore),
				"new source mount should have a different device ID than the crashed one (old=%s, new=%s)", fuseDeviceBefore, fuseDeviceAfter)

			// Verify the old device ID no longer serves the source path
			ginkgo.By("Verifying old device ID is no longer at the source path")
			oldDeviceStillAtSource := checkDeviceIDAtSourcePath(ctx, f, targetNode, pvName, fuseDeviceBefore)
			gomega.Expect(oldDeviceStillAtSource).To(gomega.BeFalse(),
				"old device ID %s should not be the active source mount anymore", fuseDeviceBefore)

			// Verify meta file was overwritten (mtime changed)
			ginkgo.By("Verifying meta file was overwritten after recovery")
			metaMtimeAfter := getMetaFileMtime(ctx, f, targetNode, pvName)
			framework.Logf("Meta mtime — before: %s, after: %s", metaMtimeBefore, metaMtimeAfter)
			gomega.Expect(metaMtimeAfter).ToNot(gomega.Equal(metaMtimeBefore),
				"meta file mtime should change after fresh mount overwrites it")
		})
		ginkgo.It("should recover from mounter pod crash with fresh source mount for new pods with different params", ginkgo.Serial, func(ctx context.Context) {
			// Create PV with --read-only mount option
			readOnlyResource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, []string{"--read-only"})
			l.resources = append(l.resources, readOnlyResource)

			// Mount pod 1 with read-only params
			ginkgo.By("Creating pod 1 with read-only mount")
			targetNode, pods := createPodsOnSameNode(ctx, f, 1, readOnlyResource)
			defer deletePodsInOrder(ctx, f, pods)

			// Verify pod 1 can read but not write (read-only mount)
			ginkgo.By("Verifying pod 1 has a working read-only mount (read succeeds, write fails)")
			toWrite := 1024
			seed := time.Now().UTC().UnixNano()
			checkListingPathSucceed(ctx, f, pods[0], "/mnt/volume1/")
			checkWriteToPathFails(ctx, f, pods[0], "/mnt/volume1/should-fail.txt", toWrite, seed)

			// Kill mounter pod
			ginkgo.By(fmt.Sprintf("Killing mounter pod on node %s", targetNode))
			killMounterPodOnNode(ctx, f, targetNode)
			waitForMounterPodReady(ctx, f, targetNode)
			time.Sleep(15 * time.Second)

			// Verify pod 1 now has transport errors (dead FUSE mount)
			ginkgo.By("Verifying pod 1 has transport errors after mounter crash")
			assertPodIOFails(ctx, f, pods[0], "/mnt/volume1/")

			// Create a new PV/PVC without --read-only for the new pod (same bucket, different params)
			ginkgo.By("Creating new volume resource without --read-only for new pod")
			writableResource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
			l.resources = append(l.resources, writableResource)

			// Create pod 2 on the same node with writable PV
			ginkgo.By("Creating pod 2 with writable mount on same node after mounter crash")
			pod2 := e2epod.MakePod(f.Namespace.Name, map[string]string{"kubernetes.io/hostname": targetNode}, []*v1.PersistentVolumeClaim{writableResource.Pvc}, admissionapi.LevelBaseline, "")
			pod2, err := createPod(ctx, f.ClientSet, f.Namespace.Name, pod2)
			framework.ExpectNoError(err, "failed to create pod 2 with writable mount")
			defer e2epod.DeletePodWithWait(ctx, f.ClientSet, pod2)

			// Pod 2 should be able to write (proves fresh mount with new params, not stale read-only)
			ginkgo.By("Verifying pod 2 can write (fresh mount without --read-only)")
			writeFile := "/mnt/volume1/after-crash-writable.txt"
			writeSeed := time.Now().UTC().UnixNano()
			checkWriteToPathSucceedEventually(ctx, f, pod2, writeFile, toWrite, writeSeed)
			checkReadFromPathSucceedEventually(ctx, f, pod2, writeFile, toWrite, writeSeed)
		})
	})
}

// createPodsOnSameNode creates n pods on the same node using the given volume resource.
// Returns the target node name and slice of created pods.
func createPodsOnSameNode(ctx context.Context, f *framework.Framework, n int, resource *storageframework.VolumeResource) (string, []*v1.Pod) {
	var pods []*v1.Pod
	var targetNode string

	for i := range n {
		var nodeSelector map[string]string
		if i > 0 && targetNode != "" {
			nodeSelector = map[string]string{"kubernetes.io/hostname": targetNode}
		}

		ginkgo.By(fmt.Sprintf("Creating pod %d with volume", i+1))
		pod := e2epod.MakePod(f.Namespace.Name, nodeSelector, []*v1.PersistentVolumeClaim{resource.Pvc}, admissionapi.LevelBaseline, "")
		pod, err := createPod(ctx, f.ClientSet, f.Namespace.Name, pod)
		framework.ExpectNoError(err)

		if i == 0 {
			targetNode = pod.Spec.NodeName
		} else {
			gomega.Expect(pod.Spec.NodeName).To(gomega.Equal(targetNode))
		}
		pods = append(pods, pod)
	}
	return targetNode, pods
}

// deletePodsInOrder deletes pods sequentially in the order they appear in the slice.
func deletePodsInOrder(ctx context.Context, f *framework.Framework, pods []*v1.Pod) {
	for _, pod := range pods {
		e2epod.DeletePodWithWait(ctx, f.ClientSet, pod)
	}
}

// checkCrossReadWriteDaemonset verifies that two pods sharing a mount can see each other's writes.
func checkCrossReadWriteDaemonset(ctx context.Context, f *framework.Framework, pod1, pod2 *v1.Pod) {
	toWrite := 1024
	basePath := "/mnt/volume1"

	// Pod1 writes, pod2 reads
	file1 := filepath.Join(basePath, "ds-cross-file1.txt")
	seed1 := time.Now().UTC().UnixNano()
	checkWriteToPathSucceedEventually(ctx, f, pod1, file1, toWrite, seed1)
	checkReadFromPathSucceedEventually(ctx, f, pod2, file1, toWrite, seed1)

	// Pod2 writes, pod1 reads
	file2 := filepath.Join(basePath, "ds-cross-file2.txt")
	seed2 := time.Now().UTC().UnixNano()
	checkWriteToPathSucceedEventually(ctx, f, pod2, file2, toWrite, seed2)
	checkReadFromPathSucceedEventually(ctx, f, pod1, file2, toWrite, seed2)
}

// verifyCSINodeLogs checks the CSI node pod logs for expected sharing messages.
// This is a helper that can be used to verify that mount sharing is actually happening
// at the CSI driver level (FUSE source + bind mount) rather than separate mounts.
func verifyCSINodeLogs(ctx context.Context, f *framework.Framework, nodeName string, expectedSubstring string) bool {
	pods, err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=s3-csi-node",
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil || len(pods.Items) == 0 {
		return false
	}

	logs, err := e2epod.GetPodLogs(ctx, f.ClientSet, csiDriverDaemonSetNamespace, pods.Items[0].Name, "s3-plugin")
	if err != nil {
		return false
	}

	return containsSubstring(logs, expectedSubstring)
}

func containsSubstring(s, substr string) bool {
	return len(s) > 0 && len(substr) > 0 && findSubstring(s, substr)
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// ptrInt64 returns a pointer to the given int64 value.
func ptrInt64(v int64) *int64 {
	return &v
}

// assertPodFailsToMount waits for a pod to have a mount failure event containing the given substring.
// The pod should be stuck in ContainerCreating due to volume mount failure.
func assertPodFailsToMount(ctx context.Context, f *framework.Framework, pod *v1.Pod, errorSubstring string) {
	framework.Gomega().Eventually(ctx, func(ctx context.Context) (bool, error) {
		events, err := f.ClientSet.CoreV1().Events(pod.Namespace).List(ctx, metav1.ListOptions{
			FieldSelector: "involvedObject.name=" + pod.Name,
		})
		if err != nil {
			return false, err
		}
		for _, event := range events.Items {
			if event.Reason == "FailedMount" && strings.Contains(event.Message, errorSubstring) {
				framework.Logf("Found expected FailedMount event: %s", event.Message)
				return true, nil
			}
		}
		return false, nil
	}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(gomega.BeTrue())
}

// countFuseMountsForVolume execs into the CSI node pod on the given node and checks
// /proc/self/mountinfo for FUSE mounts matching the volume ID's source path.
// The source mount path is: /var/lib/kubelet/plugins/s3.csi.aws.com/mnt/<volumeID>
// Returns the count of FUSE source mounts for that volume (should be exactly 1 for shared volumes).
// Retries on transient exec errors (container restarting, etc.) for up to 1 minute.
func countFuseMountsForVolume(ctx context.Context, f *framework.Framework, nodeName, volumeID string) int {
	var result int
	framework.Gomega().Eventually(ctx, func(ctx context.Context) (int, error) {
		// Find the CSI node pod on this node
		pods, err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app=s3-csi-node",
			FieldSelector: "spec.nodeName=" + nodeName,
		})
		if err != nil || len(pods.Items) == 0 {
			framework.Logf("Failed to find CSI node pod on node %s (retrying): %v", nodeName, err)
			return -1, fmt.Errorf("CSI node pod not found on node %s", nodeName)
		}

		csiPod := &pods.Items[0]

		// Count FUSE source mounts for this specific volume
		cmd := fmt.Sprintf("cat /proc/self/mountinfo | grep 's3.csi.aws.com/mnt/%s' | grep fuse | wc -l", volumeID)
		stdout, stderr, err := execInPodWithNamespace(ctx, f, csiDriverDaemonSetNamespace, csiPod.Name, "s3-plugin", []string{"/bin/sh", "-c", cmd})
		if err != nil {
			framework.Logf("Failed to exec in CSI node pod (retrying) (stdout=%s, stderr=%s): %v", stdout, stderr, err)
			return -1, fmt.Errorf("exec failed: %w", err)
		}

		count := 0
		fmt.Sscanf(strings.TrimSpace(stdout), "%d", &count)
		framework.Logf("Found %d FUSE source mount(s) for volume %s on node %s", count, volumeID, nodeName)
		result = count
		return count, nil
	}).WithTimeout(1 * time.Minute).WithPolling(5 * time.Second).Should(gomega.BeNumerically(">=", 0))
	return result
}

// execInPodWithNamespace executes a command in a specific pod/container/namespace using
// the SPDY executor, bypassing the e2e framework's namespace restriction.
func execInPodWithNamespace(ctx context.Context, f *framework.Framework, namespace, podName, containerName string, cmd []string) (string, string, error) {
	config, err := framework.LoadConfig()
	if err != nil {
		return "", "", fmt.Errorf("failed to load kube config: %w", err)
	}

	req := f.ClientSet.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		Param("container", containerName).
		Param("stdout", "true").
		Param("stderr", "true")
	for _, c := range cmd {
		req = req.Param("command", c)
	}

	exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		return "", "", fmt.Errorf("failed to create SPDY executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	return stdout.String(), stderr.String(), err
}

// killCSINodePodOnNode kills only the CSI node pod (s3-csi-node) on the specified node.
// This simulates a CSI node pod restart which loses the in-memory mount map.
// The mounter pod (s3-csi-daemonset-mounter) is NOT killed — FUSE mounts stay alive.
func killCSINodePodOnNode(ctx context.Context, f *framework.Framework, nodeName string) {
	pods, err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=s3-csi-node",
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	framework.ExpectNoError(err)
	gomega.Expect(len(pods.Items)).To(gomega.BeNumerically(">", 0),
		"expected at least one CSI node pod on node %s", nodeName)

	for i := range pods.Items {
		framework.Logf("Killing CSI node pod %s on node %s", pods.Items[i].Name, nodeName)
		framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, &pods.Items[i]))
	}
}

// waitForCSINodePodReady waits for the CSI node pod on the given node to be Running and Ready.
func waitForCSINodePodReady(ctx context.Context, f *framework.Framework, nodeName string) {
	framework.Gomega().Eventually(ctx, func(ctx context.Context) (bool, error) {
		pods, err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app=s3-csi-node",
			FieldSelector: "spec.nodeName=" + nodeName,
		})
		if err != nil {
			return false, nil
		}
		for _, pod := range pods.Items {
			if pod.Status.Phase == v1.PodRunning && isPodReady(&pod) {
				framework.Logf("CSI node pod %s is ready on node %s", pod.Name, nodeName)
				return true, nil
			}
		}
		return false, nil
	}).WithTimeout(3 * time.Minute).WithPolling(5 * time.Second).Should(gomega.BeTrue())
}

// isPodReady checks if all containers in a pod have Ready condition.
func isPodReady(pod *v1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == v1.PodReady && cond.Status == v1.ConditionTrue {
			return true
		}
	}
	return false
}

// checkMetaFileExists execs into the CSI node pod and checks if the .meta.json file
// exists for the given volume ID.
// Retries on transient exec errors (container restarting, etc.) for up to 1 minute.
// Only retries when exec fails; if exec succeeds and file is MISSING, that's a real result.
func checkMetaFileExists(ctx context.Context, f *framework.Framework, nodeName, volumeID string) bool {
	var result bool
	framework.Gomega().Eventually(ctx, func(ctx context.Context) (string, error) {
		pods, err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app=s3-csi-node",
			FieldSelector: "spec.nodeName=" + nodeName,
		})
		if err != nil || len(pods.Items) == 0 {
			framework.Logf("Failed to find CSI node pod on node %s (retrying): %v", nodeName, err)
			return "", fmt.Errorf("CSI node pod not found on node %s", nodeName)
		}

		csiPod := &pods.Items[0]
		metaPath := fmt.Sprintf("/var/lib/kubelet/plugins/s3.csi.aws.com/meta/%s.meta.json", volumeID)
		cmd := fmt.Sprintf("test -f %s && echo EXISTS || echo MISSING", metaPath)

		stdout, _, err := execInPodWithNamespace(ctx, f, csiDriverDaemonSetNamespace, csiPod.Name, "s3-plugin", []string{"/bin/sh", "-c", cmd})
		if err != nil {
			framework.Logf("Failed to check meta file (retrying): %v", err)
			return "", fmt.Errorf("exec failed: %w", err)
		}

		output := strings.TrimSpace(stdout)
		framework.Logf("Meta file check for %s on node %s: %s", volumeID, nodeName, output)
		result = output == "EXISTS"
		return output, nil
	}).WithTimeout(1 * time.Minute).WithPolling(5 * time.Second).ShouldNot(gomega.BeEmpty())
	return result
}

// killMounterPodOnNode kills the s3-csi-daemonset-mounter pod on the specified node.
// This kills all mount-s3 processes, making existing FUSE mounts dead.
func killMounterPodOnNode(ctx context.Context, f *framework.Framework, nodeName string) {
	pods, err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=s3-csi-daemonset-mounter",
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	framework.ExpectNoError(err)
	gomega.Expect(len(pods.Items)).To(gomega.BeNumerically(">", 0),
		"expected at least one mounter pod on node %s", nodeName)

	for i := range pods.Items {
		framework.Logf("Killing mounter pod %s on node %s", pods.Items[i].Name, nodeName)
		framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, &pods.Items[i]))
	}
}

// waitForMounterPodReady waits for the mounter pod on the given node to be Running and Ready.
func waitForMounterPodReady(ctx context.Context, f *framework.Framework, nodeName string) {
	framework.Gomega().Eventually(ctx, func(ctx context.Context) (bool, error) {
		pods, err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app=s3-csi-daemonset-mounter",
			FieldSelector: "spec.nodeName=" + nodeName,
		})
		if err != nil {
			return false, nil
		}
		for _, pod := range pods.Items {
			if pod.Status.Phase == v1.PodRunning && isPodReady(&pod) {
				framework.Logf("Mounter pod %s is ready on node %s", pod.Name, nodeName)
				return true, nil
			}
		}
		return false, nil
	}).WithTimeout(3 * time.Minute).WithPolling(5 * time.Second).Should(gomega.BeTrue())
}

// assertPodIOFails verifies that I/O to the given path inside the pod fails.
// After a mounter crash, FUSE mounts become dead and any I/O returns an error.
func assertPodIOFails(ctx context.Context, f *framework.Framework, pod *v1.Pod, path string) {
	// Try to list files — should fail with "Transport endpoint is not connected" or similar
	cmd := fmt.Sprintf("ls %s 2>&1 || true", path)
	stdout, stderr, err := e2epod.ExecCommandInContainerWithFullOutput(f, pod.Name, pod.Spec.Containers[0].Name, "/bin/sh", "-c", cmd)
	framework.Logf("Pod %s I/O check: stdout=%q stderr=%q err=%v", pod.Name, stdout, stderr, err)

	// The command itself should succeed (we use || true) but output should indicate mount failure
	combinedOutput := stdout + stderr
	mountFailed := strings.Contains(combinedOutput, "Transport endpoint is not connected") ||
		strings.Contains(combinedOutput, "No such device") ||
		strings.Contains(combinedOutput, "Input/output error")
	gomega.Expect(mountFailed).To(gomega.BeTrue(),
		"expected I/O failure after mounter crash, got: %s", combinedOutput)
}

// readMetaFileContent reads the .meta.json file content for a given volume from the CSI node pod.
// Returns the raw JSON string, or empty string if the file doesn't exist.
// Retries on transient exec errors for up to 30 seconds.
func readMetaFileContent(ctx context.Context, f *framework.Framework, nodeName, volumeID string) string {
	var result string
	framework.Gomega().Eventually(ctx, func(ctx context.Context) (string, error) {
		pods, err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app=s3-csi-node",
			FieldSelector: "spec.nodeName=" + nodeName,
		})
		if err != nil || len(pods.Items) == 0 {
			framework.Logf("Failed to find CSI node pod on node %s (retrying): %v", nodeName, err)
			return "", fmt.Errorf("CSI node pod not found on node %s", nodeName)
		}

		csiPod := &pods.Items[0]
		metaPath := fmt.Sprintf("/var/lib/kubelet/plugins/s3.csi.aws.com/meta/%s.meta.json", volumeID)
		cmd := fmt.Sprintf("cat %s 2>/dev/null || echo ''", metaPath)

		stdout, _, err := execInPodWithNamespace(ctx, f, csiDriverDaemonSetNamespace, csiPod.Name, "s3-plugin", []string{"/bin/sh", "-c", cmd})
		if err != nil {
			framework.Logf("Failed to read meta file (retrying): %v", err)
			return "", fmt.Errorf("exec failed: %w", err)
		}

		result = strings.TrimSpace(stdout)
		// Return a sentinel so Eventually knows exec succeeded
		return "exec-ok", nil
	}).WithTimeout(30 * time.Second).WithPolling(5 * time.Second).Should(gomega.Equal("exec-ok"))
	return result
}

// checkDeviceIDAtSourcePath checks if the given device ID is the active (topmost) FUSE mount
// at the source path for the volume. Returns true if the device ID is still serving.
// Retries on transient exec errors for up to 30 seconds.
func checkDeviceIDAtSourcePath(ctx context.Context, f *framework.Framework, nodeName, volumeID, deviceID string) bool {
	var result bool
	framework.Gomega().Eventually(ctx, func(ctx context.Context) (string, error) {
		pods, err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app=s3-csi-node",
			FieldSelector: "spec.nodeName=" + nodeName,
		})
		if err != nil || len(pods.Items) == 0 {
			return "", fmt.Errorf("CSI node pod not found on node %s: %v", nodeName, err)
		}

		csiPod := &pods.Items[0]
		// Get the topmost (last) mount entry at the source path and check its device ID
		cmd := fmt.Sprintf("cat /proc/self/mountinfo | grep 's3.csi.aws.com/mnt/%s ' | grep fuse | tail -1 | awk '{print $3}'", volumeID)
		stdout, _, err := execInPodWithNamespace(ctx, f, csiDriverDaemonSetNamespace, csiPod.Name, "s3-plugin", []string{"/bin/sh", "-c", cmd})
		if err != nil {
			framework.Logf("Failed to check device ID at source path (retrying): %v", err)
			return "", fmt.Errorf("exec failed: %w", err)
		}

		topDeviceID := strings.TrimSpace(stdout)
		framework.Logf("Topmost device ID at source path for %s: %s (checking against old: %s)", volumeID, topDeviceID, deviceID)
		result = topDeviceID == deviceID
		// Return a non-empty sentinel so Eventually knows exec succeeded
		return "done", nil
	}).WithTimeout(30 * time.Second).WithPolling(5 * time.Second).Should(gomega.Equal("done"))
	return result
}

// getMetaFileMtime returns the modification time of the .meta.json file for the given volume.
// Uses stat to get the mtime. Returns empty string if file doesn't exist.
// Retries on transient exec errors for up to 30 seconds.
func getMetaFileMtime(ctx context.Context, f *framework.Framework, nodeName, volumeID string) string {
	var result string
	framework.Gomega().Eventually(ctx, func(ctx context.Context) (string, error) {
		pods, err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app=s3-csi-node",
			FieldSelector: "spec.nodeName=" + nodeName,
		})
		if err != nil || len(pods.Items) == 0 {
			return "", fmt.Errorf("CSI node pod not found on node %s: %v", nodeName, err)
		}

		csiPod := &pods.Items[0]
		metaPath := fmt.Sprintf("/var/lib/kubelet/plugins/s3.csi.aws.com/meta/%s.meta.json", volumeID)
		cmd := fmt.Sprintf("stat -c '%%Y' %s 2>/dev/null || echo ''", metaPath)

		stdout, _, err := execInPodWithNamespace(ctx, f, csiDriverDaemonSetNamespace, csiPod.Name, "s3-plugin", []string{"/bin/sh", "-c", cmd})
		if err != nil {
			framework.Logf("Failed to get meta file mtime (retrying): %v", err)
			return "", fmt.Errorf("exec failed: %w", err)
		}
		result = strings.TrimSpace(stdout)
		return result, nil
	}).WithTimeout(30 * time.Second).WithPolling(5 * time.Second).ShouldNot(gomega.BeEmpty())
	return result
}

// getFuseSourceDeviceID execs into the CSI node pod and extracts the device ID (major:minor)
// of the FUSE source mount for the given volume. Returns empty string if not found.
// Retries on transient exec errors for up to 30 seconds.
func getFuseSourceDeviceID(ctx context.Context, f *framework.Framework, nodeName, volumeID string) string {
	var result string
	framework.Gomega().Eventually(ctx, func(ctx context.Context) (string, error) {
		pods, err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app=s3-csi-node",
			FieldSelector: "spec.nodeName=" + nodeName,
		})
		if err != nil || len(pods.Items) == 0 {
			return "", fmt.Errorf("CSI node pod not found on node %s: %v", nodeName, err)
		}

		csiPod := &pods.Items[0]
		cmd := fmt.Sprintf("cat /proc/self/mountinfo | grep 's3.csi.aws.com/mnt/%s ' | grep fuse | awk '{print $3}' | tail -1", volumeID)
		stdout, _, err := execInPodWithNamespace(ctx, f, csiDriverDaemonSetNamespace, csiPod.Name, "s3-plugin", []string{"/bin/sh", "-c", cmd})
		if err != nil {
			framework.Logf("Failed to get FUSE source device ID (retrying): %v", err)
			return "", fmt.Errorf("exec failed: %w", err)
		}
		result = strings.TrimSpace(stdout)
		// Return a sentinel so Eventually knows we succeeded even if device ID is empty (no mount)
		return "exec-ok", nil
	}).WithTimeout(30 * time.Second).WithPolling(5 * time.Second).Should(gomega.Equal("exec-ok"))
	return result
}

// dumpMountTable logs mountinfo entries relevant to S3 CSI (source mounts, bind mounts, mountpoint-s3 FUSE).
// Filters for lines containing s3.csi.aws.com or mountpoint-s3.
// Retries a few times on transient exec errors since it's useful for debugging, but is non-fatal.
func dumpMountTable(ctx context.Context, f *framework.Framework, nodeName, label string) {
	var dumped bool
	// Short retry — non-critical, best-effort debugging helper.
	// We don't use Should() here because failure to dump is non-fatal.
	for attempts := 0; attempts < 5; attempts++ {
		pods, err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app=s3-csi-node",
			FieldSelector: "spec.nodeName=" + nodeName,
		})
		if err != nil || len(pods.Items) == 0 {
			framework.Logf("[%s] Failed to find CSI node pod on node %s (attempt %d): %v", label, nodeName, attempts+1, err)
			time.Sleep(3 * time.Second)
			continue
		}

		csiPod := &pods.Items[0]
		cmd := "cat /proc/self/mountinfo | grep -E 's3\\.csi\\.aws\\.com|mountpoint-s3'"
		stdout, _, err := execInPodWithNamespace(ctx, f, csiDriverDaemonSetNamespace, csiPod.Name, "s3-plugin",
			[]string{"/bin/sh", "-c", cmd})
		if err != nil {
			framework.Logf("[%s] Failed to dump mount table (attempt %d): %v", label, attempts+1, err)
			time.Sleep(3 * time.Second)
			continue
		}
		if stdout == "" {
			framework.Logf("[%s] Mount table on node %s: (no s3/mountpoint entries)", label, nodeName)
		} else {
			framework.Logf("[%s] Mount table on node %s:\n%s", label, nodeName, stdout)
		}
		dumped = true
		break
	}
	if !dumped {
		framework.Logf("[%s] Could not dump mount table on node %s after retries (non-fatal)", label, nodeName)
	}
}

// checkReadFromPathSucceedEventually retries reading from a path in a pod, tolerating

// waitForCSINodePodStable waits for the CSI node pod to have been Running continuously
// for at least 5 seconds. This prevents race conditions where we exec into a pod that
// just started but hasn't fully initialized (e.g., RebuildMountMap + DiscoverCommDir).
func waitForCSINodePodStable(ctx context.Context, f *framework.Framework, nodeName string) {
	framework.Gomega().Eventually(ctx, func(ctx context.Context) (bool, error) {
		pods, err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app=s3-csi-node",
			FieldSelector: "spec.nodeName=" + nodeName,
		})
		if err != nil || len(pods.Items) == 0 {
			return false, nil
		}
		pod := &pods.Items[0]
		if pod.Status.Phase != v1.PodRunning || !isPodReady(pod) {
			return false, nil
		}
		// Check that the s3-plugin container has been running for at least 5 seconds
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name == "s3-plugin" {
				if cs.State.Running == nil {
					return false, nil
				}
				runningFor := time.Since(cs.State.Running.StartedAt.Time)
				if runningFor < 5*time.Second {
					framework.Logf("CSI node pod s3-plugin container running for only %v, waiting for stability", runningFor)
					return false, nil
				}
				return true, nil
			}
		}
		return false, nil
	}).WithTimeout(1 * time.Minute).WithPolling(2 * time.Second).Should(gomega.BeTrue())
	framework.Logf("CSI node pod on node %s is stable (running for >5s)", nodeName)
}

// countMountpointProcessesForVolume execs into the mounter pod on the given node and counts
// mount-s3 processes for the specific bucket. Uses /proc/*/cmdline.
// Returns the count or -1 on error. Callers should use Eventually for retry logic.
func countMountpointProcessesForVolume(ctx context.Context, f *framework.Framework, nodeName, bucketName string) int {
	pods, err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=s3-csi-daemonset-mounter",
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil || len(pods.Items) == 0 {
		framework.Logf("mounter pod not found on node %s (retrying): %v", nodeName, err)
		return -1
	}

	mounterPod := &pods.Items[0]
	// Count mount-s3 processes for this specific bucket using case pattern match (no grep, avoids self-match)
	cmd := fmt.Sprintf("count=0; for p in /proc/[0-9]*/cmdline; do line=$(cat $p 2>/dev/null | tr '\\0' ' '); case \"$line\" in '/mountpoint-s3/bin/mount-s3 %s '*) count=$((count+1));; esac; done; echo $count", bucketName)
	stdout, _, err := execInPodWithNamespace(ctx, f, csiDriverDaemonSetNamespace, mounterPod.Name, "mounter", []string{"/bin/sh", "-c", cmd})
	if err != nil {
		framework.Logf("exec into mounter pod failed (retrying): %v", err)
		return -1
	}

	count := 0
	fmt.Sscanf(strings.TrimSpace(stdout), "%d", &count)
	framework.Logf("mount-s3 process count for bucket %s on node %s: %d", bucketName, nodeName, count)
	return count
}

// dumpMountpointProcesses logs all mount-s3 processes running in the mounter pod on the given node.
// This is a debugging helper — non-fatal on failure.
func dumpMountpointProcesses(ctx context.Context, f *framework.Framework, nodeName, label string) {
	pods, err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=s3-csi-daemonset-mounter",
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil || len(pods.Items) == 0 {
		framework.Logf("[%s] Failed to find mounter pod on node %s: %v", label, nodeName, err)
		return
	}

	mounterPod := &pods.Items[0]
	cmd := "for p in /proc/[0-9]*/cmdline; do printf '%s: ' $p; cat $p | tr '\\0' ' '; echo; done 2>/dev/null || true"
	stdout, _, err := execInPodWithNamespace(ctx, f, csiDriverDaemonSetNamespace, mounterPod.Name, "mounter", []string{"/bin/sh", "-c", cmd})
	if err != nil {
		framework.Logf("[%s] Failed to list mount-s3 processes (non-fatal): %v", label, err)
		return
	}
	if stdout == "" {
		framework.Logf("[%s] mount-s3 processes on node %s: (none)", label, nodeName)
	} else {
		framework.Logf("[%s] mount-s3 processes on node %s:\n%s", label, nodeName, stdout)
	}
}

// listCommDirContents dumps the contents of /comm/ inside the mounter pod for debugging.
func listCommDirContents(ctx context.Context, f *framework.Framework, nodeName, volumeID string) {
	pods, err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=s3-csi-daemonset-mounter",
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil || len(pods.Items) == 0 {
		framework.Logf("DEBUG: Could not find mounter pod for listing: %v", err)
		return
	}
	cmd := fmt.Sprintf("echo '--- /comm/ top-level ---'; ls -la /comm/ 2>&1; echo '--- /comm/%s/ ---'; ls -la /comm/%s/ 2>&1 || true", volumeID, volumeID)
	stdout, _, err := execInPodWithNamespace(ctx, f, csiDriverDaemonSetNamespace, pods.Items[0].Name, "mounter", []string{"/bin/sh", "-c", cmd})
	if err != nil {
		framework.Logf("DEBUG: listing /comm/ failed: %v", err)
		return
	}
	framework.Logf("DEBUG /comm/ contents on node %s (looking for %s):\n%s", nodeName, volumeID, stdout)
}

// queryCredentialDirStatus execs into the mounter pod and returns "EXISTS" or "MISSING" for the credential directory.
// Returns an error if the exec fails or the mounter pod is not found.
func queryCredentialDirStatus(ctx context.Context, f *framework.Framework, nodeName, volumeID string) (string, error) {
	pods, err := f.ClientSet.CoreV1().Pods(csiDriverDaemonSetNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=s3-csi-daemonset-mounter",
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil || len(pods.Items) == 0 {
		return "", fmt.Errorf("mounter pod not found on node %s: %v", nodeName, err)
	}

	mounterPod := &pods.Items[0]
	credDir := fmt.Sprintf("/comm/%s", volumeID)
	cmd := fmt.Sprintf("test -d %s && echo EXISTS || echo MISSING", credDir)

	stdout, _, err := execInPodWithNamespace(ctx, f, csiDriverDaemonSetNamespace, mounterPod.Name, "mounter", []string{"/bin/sh", "-c", cmd})
	if err != nil {
		return "", fmt.Errorf("exec failed: %w", err)
	}

	output := strings.TrimSpace(stdout)
	framework.Logf("Credential dir check for %s on node %s: %s", volumeID, nodeName, output)
	return output, nil
}

// checkCredentialDirExists polls until the credential directory EXISTS in the mounter pod (up to 1 minute).
func checkCredentialDirExists(ctx context.Context, f *framework.Framework, nodeName, volumeID string) bool {
	var result bool
	framework.Gomega().Eventually(ctx, func(ctx context.Context) (string, error) {
		status, err := queryCredentialDirStatus(ctx, f, nodeName, volumeID)
		if err != nil {
			framework.Logf("Failed to check credential dir (retrying): %v", err)
			return "", err
		}
		if status != "EXISTS" {
			return status, fmt.Errorf("credential dir not yet present (got %s)", status)
		}
		result = true
		return status, nil
	}).WithTimeout(1 * time.Minute).WithPolling(5 * time.Second).Should(gomega.Equal("EXISTS"))
	return result
}

// checkCredentialDirRemoved polls until the credential directory is MISSING from the mounter pod (up to 1 minute).
func checkCredentialDirRemoved(ctx context.Context, f *framework.Framework, nodeName, volumeID string) bool {
	var result bool
	framework.Gomega().Eventually(ctx, func(ctx context.Context) (string, error) {
		status, err := queryCredentialDirStatus(ctx, f, nodeName, volumeID)
		if err != nil {
			framework.Logf("Failed to check credential dir removal (retrying): %v", err)
			return "", err
		}
		if status != "MISSING" {
			return status, fmt.Errorf("credential dir still present (got %s)", status)
		}
		result = true
		return status, nil
	}).WithTimeout(1 * time.Minute).WithPolling(5 * time.Second).Should(gomega.Equal("MISSING"))
	return result
}
