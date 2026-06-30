/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Adapted from https://github.com/kubernetes/kubernetes/blob/v1.36.2/test/e2e/storage/testsuites/volumelimits.go.
// The upstream test requires dynamic provisioning; we use static provisioning with pre-created S3 buckets.

package custom_testsuites

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/onsi/ginkgo/v2"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
)

type s3CSIVolumeLimitsTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

const (
	// The test uses generic pod startup / PV deletion timeouts. As it creates
	// much more volumes at once, these timeouts are multiplied by this number.
	// Using real nr. of volumes (e.g. 128 on GCE) would be really too much.
	// Upstream uses 10; we use 2 since our default maxVolumesPerNode is 10 (not 128).
	testSlowMultiplier = 2

	// How long to wait until CSINode gets attach limit from installed CSI driver.
	csiNodeInfoTimeout = 2 * time.Minute
)

var _ storageframework.TestSuite = &s3CSIVolumeLimitsTestSuite{}

// InitS3CSIVolumeLimitsTestSuite returns s3CSIVolumeLimitsTestSuite that implements TestSuite interface
// using custom test patterns
func InitS3CSIVolumeLimitsTestSuite() storageframework.TestSuite {
	return &s3CSIVolumeLimitsTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name: "volumelimits",
			TestPatterns: []storageframework.TestPattern{
				storageframework.DefaultFsPreprovisionedPV,
			},
		},
	}
}

func (t *s3CSIVolumeLimitsTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

func (t *s3CSIVolumeLimitsTestSuite) SkipUnsupportedTests(driver storageframework.TestDriver, _ storageframework.TestPattern) {
	dInfo := driver.GetDriverInfo()
	if !dInfo.Capabilities[storageframework.CapVolumeLimits] {
		e2eskipper.Skipf("Driver %s does not support volume limits", dInfo.Name)
	}
}

func (t *s3CSIVolumeLimitsTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	type local struct {
		resources []*storageframework.VolumeResource
		config    *storageframework.PerTestConfig
	}
	var l local

	f := framework.NewFrameworkWithCustomTimeouts(NamespacePrefix+"volumelimits", storageframework.GetDriverTimeouts(driver))
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

	cleanup := func(ctx context.Context) {
		var errs []error
		for _, resource := range l.resources {
			errs = append(errs, resource.CleanupResource(ctx))
		}
		framework.ExpectNoError(errors.NewAggregate(errs), "while cleanup resource")
	}
	ginkgo.BeforeEach(func(ctx context.Context) {
		l = local{}
		l.config = driver.PrepareTest(ctx, f)
		ginkgo.DeferCleanup(cleanup)
	})

	// This checks that CSIMaxVolumeLimitChecker works as expected.
	// A randomly chosen node should be able to handle as many CSI volumes as
	// it claims to handle in CSINode.Spec.Drivers[x].Allocatable.
	// The test uses one single pod with a lot of volumes to work around any
	// max pod limit on a node.
	// And one extra pod with a CSI volume should get Pending with a condition
	// that says it's unschedulable because of volume limit.
	// BEWARE: the test may create lot of volumes and it's really slow.
	f.It("should support volume limits", f.WithSerial(), func(ctx context.Context) {
		driverInfo := driver.GetDriverInfo()

		ginkgo.By("Picking a node")
		// Some CSI drivers are deployed to a single node (e.g csi-hostpath),
		// so we use that node instead of picking a random one.
		nodeName := l.config.ClientNodeSelection.Name
		if nodeName == "" {
			node, err := e2enode.GetRandomReadySchedulableNode(ctx, f.ClientSet)
			framework.ExpectNoError(err)
			nodeName = node.Name
		}
		framework.Logf("Selected node %s", nodeName)

		ginkgo.By("Checking node limits")
		limit, err := getCSINodeLimits(ctx, f, nodeName, driverInfo.Name)
		framework.ExpectNoError(err)
		framework.Logf("Node %s can handle %d volumes of driver %s", nodeName, limit, driverInfo.Name)

		// Create <limit> PVCs for one gigantic pod.
		var pvcs []*v1.PersistentVolumeClaim
		ginkgo.By(fmt.Sprintf("Creating %d volume(s)", limit))
		for range limit {
			resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
			l.resources = append(l.resources, resource)
			pvcs = append(pvcs, resource.Pvc)
		}

		ginkgo.By("Creating pod to use all PVC(s)")
		// pod.Spec.NodeName should not be set directly because it will bypass the scheduler. Use SetNodeSelection instead for these 2 occurences.
		// https://github.com/kubernetes/kubernetes/blob/24e2b02af5543d7910c2bb074c7264df5a8f0467/test/e2e/framework/pod/node_selection.go#L96-L101
		selection := e2epod.NodeSelection{Name: nodeName}
		pod := e2epod.MakePod(f.Namespace.Name, nil, pvcs, admissionapi.LevelBaseline, "")
		e2epod.SetNodeSelection(&pod.Spec, selection)
		pod, err = createPodWithoutWaiting(ctx, f.ClientSet, f.Namespace.Name, pod)
		framework.ExpectNoError(err)
		defer func() {
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
		}()

		ginkgo.By("Waiting for the pod running")
		err = e2epod.WaitTimeoutForPodRunningInNamespace(ctx, f.ClientSet, pod.Name, f.Namespace.Name, testSlowMultiplier*f.Timeouts.PodStart)
		framework.ExpectNoError(err)

		ginkgo.By("Creating an extra pod with one volume to exceed the limit")
		extraResource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
		l.resources = append(l.resources, extraResource)

		extraPod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{extraResource.Pvc}, admissionapi.LevelBaseline, "")
		e2epod.SetNodeSelection(&extraPod.Spec, selection)
		extraPod, err = createPodWithoutWaiting(ctx, f.ClientSet, f.Namespace.Name, extraPod)
		framework.ExpectNoError(err, "creating extra pod beyond limit")
		defer func() {
			framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, extraPod))
		}()

		ginkgo.By("Waiting for the pod to get unschedulable with the right message")
		err = e2epod.WaitForPodCondition(ctx, f.ClientSet, f.Namespace.Name, extraPod.Name, "Unschedulable", f.Timeouts.PodStart, func(pod *v1.Pod) (bool, error) {
			if pod.Status.Phase == v1.PodPending {
				// Matches Message " 0/1 nodes are available: 1 node(s) exceed max volume count."
				reg, err := regexp.Compile(`max.+volume.+count`)
				if err != nil {
					return false, err
				}
				for _, cond := range pod.Status.Conditions {
					matched := reg.MatchString(cond.Message)
					if cond.Type == v1.PodScheduled && cond.Status == v1.ConditionFalse && cond.Reason == "Unschedulable" && matched {
						return true, nil
					}
				}
			}
			if pod.Status.Phase != v1.PodPending {
				return true, fmt.Errorf("expected pod to be in phase Pending, but got phase: %v", pod.Status.Phase)
			}
			return false, nil
		})
		framework.ExpectNoError(err)
	})

	// TODO: Uncomment when pod sharing is implemented. Alternatively just add to test above
	// Verifies that the scheduler's volumeHandle dedup works correctly:
	// a workload reusing an already-mounted PV should be admitted without counting
	// against the volume limit (same PV = same Mountpoint process = counted once).
	// f.It("should admit a pod reusing an already-mounted PV without counting it twice", f.WithSerial(), func(ctx context.Context) {
	// 	driverInfo := driver.GetDriverInfo()

	// 	ginkgo.By("Picking a node")
	// 	nodeName := l.config.ClientNodeSelection.Name
	// 	if nodeName == "" {
	// 		node, err := e2enode.GetRandomReadySchedulableNode(ctx, f.ClientSet)
	// 		framework.ExpectNoError(err)
	// 		nodeName = node.Name
	// 	}

	// 	ginkgo.By("Checking node limits")
	// 	limit, err := getCSINodeLimits(ctx, f, nodeName, driverInfo.Name)
	// 	framework.ExpectNoError(err)
	// 	framework.Logf("Node %s can handle %d volumes of driver %s", nodeName, limit, driverInfo.Name)

	// 	// Create <limit> PVCs for one gigantic pod.
	// 	var pvcs []*v1.PersistentVolumeClaim
	// 	ginkgo.By(fmt.Sprintf("Creating %d volume(s)", limit))
	// 	for range limit {
	// 		resource := createVolumeResourceWithMountOptions(ctx, l.config, pattern, nil)
	// 		l.resources = append(l.resources, resource)
	// 		pvcs = append(pvcs, resource.Pvc)
	// 	}

	// 	ginkgo.By("Creating pod to use all PVC(s)")
	// 	// pod.Spec.NodeName should not be set directly because it will bypass the scheduler. Use SetNodeSelection instead for these 2 occurences.
	// 	// https://github.com/kubernetes/kubernetes/blob/24e2b02af5543d7910c2bb074c7264df5a8f0467/test/e2e/framework/pod/node_selection.go#L96-L101
	// 	selection := e2epod.NodeSelection{Name: nodeName}
	// 	pod := e2epod.MakePod(f.Namespace.Name, nil, pvcs, admissionapi.LevelBaseline, "")
	// 	e2epod.SetNodeSelection(&pod.Spec, selection)
	// 	pod, err = createPodWithoutWaiting(ctx, f.ClientSet, f.Namespace.Name, pod)
	// 	framework.ExpectNoError(err)
	// 	defer func() {
	// 		framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
	// 	}()

	// 	err = e2epod.WaitTimeoutForPodRunningInNamespace(ctx, f.ClientSet, pod.Name, f.Namespace.Name, testSlowMultiplier*f.Timeouts.PodStart)
	// 	framework.ExpectNoError(err)

	// 	// Create a new pod referencing an EXISTING PVC (mounted by the first pod).
	// 	// The scheduler should admit it because the same volumeHandle is not counted twice.
	// 	ginkgo.By("Creating a pod reusing an already-mounted PV (should not be stuck in pending)")
	// 	sharedPod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{pvcs[0]}, admissionapi.LevelBaseline, "")
	// 	e2epod.SetNodeSelection(&sharedPod.Spec, selection)
	// 	sharedPod, err = createPod(ctx, f.ClientSet, f.Namespace.Name, sharedPod)
	// 	framework.ExpectNoError(err, "pod reusing an already-mounted PV should be admitted (not counted against volume limit)")
	// 	defer func() {
	// 		framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, sharedPod))
	// 	}()
	// })

	ginkgo.It("should verify that all csinodes have volume limits", func(ctx context.Context) {
		driverInfo := driver.GetDriverInfo()
		if !driverInfo.Capabilities[storageframework.CapVolumeLimits] {
			ginkgo.Skip(fmt.Sprintf("driver %s does not support volume limits", driverInfo.Name))
		}

		nodeNames := []string{}
		if l.config.ClientNodeSelection.Name != "" {
			// Some CSI drivers are deployed to a single node (e.g csi-hostpath),
			// so we check that node instead of checking all of them
			nodeNames = append(nodeNames, l.config.ClientNodeSelection.Name)
		} else {
			nodeList, err := e2enode.GetReadySchedulableNodes(ctx, f.ClientSet)
			framework.ExpectNoError(err)
			for _, node := range nodeList.Items {
				nodeNames = append(nodeNames, node.Name)
			}
		}

		for _, nodeName := range nodeNames {
			ginkgo.By("Checking csinode limits")
			_, err := getCSINodeLimits(ctx, f, nodeName, driverInfo.Name)
			if err != nil {
				framework.Failf("Expected volume limits to be set, error: %v", err)
			}
		}
	})
}

// getCSINodeLimits reads the volume limit for the given driver from the CSINode object.
func getCSINodeLimits(ctx context.Context, f *framework.Framework, nodeName, driverName string) (int, error) {
	// Retry with a timeout, the driver might just have been installed and kubelet takes a while to publish everything.
	var limit int
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, csiNodeInfoTimeout, true, func(ctx context.Context) (bool, error) {
		csiNode, err := f.ClientSet.StorageV1().CSINodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			framework.Logf("%s", err)
			return false, nil
		}
		var csiDriver *storagev1.CSINodeDriver
		for i, c := range csiNode.Spec.Drivers {
			if c.Name == driverName {
				csiDriver = &csiNode.Spec.Drivers[i]
				break
			}
		}
		if csiDriver == nil {
			framework.Logf("CSINodeInfo does not have driver %s yet", driverName)
			return false, nil
		}
		if csiDriver.Allocatable == nil {
			return false, fmt.Errorf("CSINodeInfo does not have Allocatable for driver %s", driverName)
		}
		if csiDriver.Allocatable.Count == nil {
			return false, fmt.Errorf("CSINodeInfo does not have Allocatable.Count for driver %s", driverName)
		}
		limit = int(*csiDriver.Allocatable.Count)
		return true, nil
	})
	if err != nil {
		return 0, fmt.Errorf("could not get CSINode limit for driver %s: %w", driverName, err)
	}
	return limit, nil
}
