/*
Copyright 2022 The Kubernetes Authors

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

package node

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
	AgentNotReadyNodeTaintKey = "s3.csi.aws.com/agent-not-ready" // key of taints to be removed on driver startup

	// CSI driver name for registration verification
	csiDriverName = "s3.csi.aws.com"

	// TaintWatcherDuration is the maximum duration for the not-ready taint watcher to run.
	TaintWatcherDuration = 1 * time.Minute
)

var (
	// taintRemovalBackoff is the exponential backoff configuration for node taint removal
	taintRemovalBackoff = wait.Backoff{
		Duration: 500 * time.Millisecond,
		Factor:   2,
		Steps:    10, // Max delay = 0.5 seconds * 2^9 = ~4 minutes
	}
)

// Struct for JSON patch operations
type JSONPatch struct {
	OP    string      `json:"op,omitempty"`
	Path  string      `json:"path,omitempty"`
	Value interface{} `json:"value"`
}

// StartNotReadyTaintWatcher checks for and removes the s3.csi.aws.com/agent-not-ready taint
// from the current node after verifying the CSI driver is properly registered.
func StartNotReadyTaintWatcher(clientset kubernetes.Interface, nodeID string, maxWatchDuration time.Duration) {
	if nodeID == "" {
		klog.V(4).Infof("nodeID is empty, skipping taint watcher")
		return
	}

	klog.Infof("Starting taint watcher for node %s (max duration: %v)", nodeID, maxWatchDuration)

	attemptTaintRemoval := func(n *corev1.Node) {
		if !hasNotReadyTaint(n) {
			klog.V(4).Infof("No agent-not-ready taint found on node %s, skipping taint removal", n.Name)
			return
		}

		klog.Infof("Found agent-not-ready taint on node %s, attempting removal", n.Name)

		backoff := wait.Backoff{
			Duration: 2 * time.Second,
			Factor:   1.5,
			Steps:    5,
		}

		ctx, cancel := context.WithTimeout(context.Background(), maxWatchDuration)
		defer cancel()

		err := wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
			// First check if driver is registered
			if err := checkDriverRegistered(ctx, clientset, n.Name); err != nil {
				klog.V(4).Infof("CSI driver not yet registered, retrying for node %s: %v", n.Name, err)
				return false, nil // Continue retrying
			}

			if err := removeNotReadyTaint(ctx, clientset, n); err != nil {
				klog.Errorf("Failed to remove agent-not-ready taint, retrying for node %s: %v", n.Name, err)
				return false, nil // Continue retrying
			}
			klog.Infof("Successfully removed agent-not-ready taint from node %s", n.Name)
			return true, nil
		})

		if err != nil {
			klog.Errorf("Timed out trying to remove agent-not-ready taint from node %s: %v", n.Name, err)
		}
	}

	node, err := clientset.CoreV1().Nodes().Get(context.Background(), nodeID, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("Failed to get node %s: %v", nodeID, err)
		return
	}

	attemptTaintRemoval(node)
}

func hasNotReadyTaint(n *corev1.Node) bool {
	for _, t := range n.Spec.Taints {
		if t.Key == AgentNotReadyNodeTaintKey {
			return true
		}
	}
	return false
}

// checkDriverRegistered verifies that the CSI driver is registered in the CSINode object
func checkDriverRegistered(ctx context.Context, clientset kubernetes.Interface, nodeName string) error {
	csiNode, err := clientset.StorageV1().CSINodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get CSINode for %s: %w", nodeName, err)
	}

	for _, driver := range csiNode.Spec.Drivers {
		if driver.Name == csiDriverName {
			klog.V(4).Infof("CSI driver %s found in CSINode for node %s", csiDriverName, nodeName)
			return nil
		}
	}

	return fmt.Errorf("CSI driver %s not found in CSINode for node %s", csiDriverName, nodeName)
}

// removeNotReadyTaint removes the taint s3.csi.aws.com/agent-not-ready from the local node
// This taint can be optionally applied by users to prevent startup race conditions as described in:
// https://github.com/awslabs/mountpoint-s3-csi-driver/blob/main/docs/TROUBLESHOOTING.md#my-pod-is-stuck-at-containercreating-with-error-driver-name-s3csiawscom-not-found-in-the-list-of-registered-csi-drivers
func removeNotReadyTaint(ctx context.Context, clientset kubernetes.Interface, node *corev1.Node) error {
	if clientset == nil {
		klog.V(4).Infof("Kubernetes clientset is nil, skipping taint removal")
		return nil
	}

	var taintsToKeep []corev1.Taint
	for _, taint := range node.Spec.Taints {
		if taint.Key != AgentNotReadyNodeTaintKey {
			taintsToKeep = append(taintsToKeep, taint)
		} else {
			klog.V(4).Infof("Queued taint for removal: key=%s, effect=%s", taint.Key, taint.Effect)
		}
	}

	if len(taintsToKeep) == len(node.Spec.Taints) {
		klog.V(4).Infof("No taints to remove on node, skipping taint removal")
		return nil
	}

	patchRemoveTaints := []JSONPatch{
		{
			OP:    "test",
			Path:  "/spec/taints",
			Value: node.Spec.Taints,
		},
		{
			OP:    "replace",
			Path:  "/spec/taints",
			Value: taintsToKeep,
		},
	}

	patch, err := json.Marshal(patchRemoveTaints)
	if err != nil {
		return err
	}

	_, err = clientset.CoreV1().Nodes().Patch(ctx, node.Name, k8stypes.JSONPatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return err
	}
	klog.Infof("Removed taint(s) from local node %s", node.Name)
	return nil
}
