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
	"os"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
	AgentNotReadyNodeTaintKey = "s3.csi.aws.com/agent-not-ready" // key of taints to be removed on driver startup

	// CSI driver readiness checking configuration
	csiDriverName = "s3.csi.aws.com"
	csiSocketPath = "/var/lib/kubelet/plugins/s3.csi.aws.com/csi.sock"

	// Default timeouts for CSI driver readiness checking
	defaultReadinessTimeout      = 30 * time.Second
	defaultReadinessPollInterval = 500 * time.Millisecond
	csiCallTimeout               = 2 * time.Second
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

// isCSIDriverReady checks if the CSI driver is ready to handle requests
// This verifies the CSI socket exists and the driver responds correctly to gRPC calls
func isCSIDriverReady() bool {
	// Check if CSI socket exists
	if _, err := os.Stat(csiSocketPath); err != nil {
		klog.V(4).Infof("taint: CSI socket not found: %v", err)
		return false
	}

	// Try to connect to the CSI socket
	conn, err := grpc.NewClient("unix://"+csiSocketPath, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		klog.V(4).Infof("taint: Failed to connect to CSI socket: %v", err)
		return false
	}
	defer conn.Close()

	// Test CSI driver responsiveness with GetPluginInfo call
	client := csi.NewIdentityClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), csiCallTimeout)
	defer cancel()

	resp, err := client.GetPluginInfo(ctx, &csi.GetPluginInfoRequest{})
	if err != nil {
		klog.V(4).Infof("taint: GetPluginInfo failed: %v", err)
		return false
	}

	if resp.GetName() != csiDriverName {
		klog.V(4).Infof("taint: Unexpected driver name: got %s, expected %s", resp.GetName(), csiDriverName)
		return false
	}

	klog.V(4).Infof("taint: CSI driver %s is ready and responsive", csiDriverName)
	return true
}

// waitForCSIDriverReady polls the CSI socket until the driver is ready or timeout occurs
func waitForCSIDriverReady() error {
	klog.Infof("taint: Waiting for CSI driver readiness (socket: %s)", csiSocketPath)

	ctx, cancel := context.WithTimeout(context.Background(), defaultReadinessTimeout)
	defer cancel()

	ticker := time.NewTicker(defaultReadinessPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for CSI driver readiness after %v", defaultReadinessTimeout)
		case <-ticker.C:
			if isCSIDriverReady() {
				klog.Infof("taint: CSI driver is ready")
				return nil
			}
		}
	}
}

// RemoveTaintInBackground is a goroutine that waits for CSI driver registration and then removes the taint
func RemoveTaintInBackground(clientset kubernetes.Interface) {
	klog.Infof("taint: Starting taint removal process")

	// Wait for CSI driver to be ready by actively checking the socket
	if err := waitForCSIDriverReady(); err != nil {
		klog.Errorf("taint: CSI driver readiness check failed: %v", err)
		klog.Infof("taint: Proceeding with taint removal anyway to avoid blocking indefinitely")
	} else {
		klog.Infof("taint: CSI driver readiness confirmed, proceeding with taint removal")
	}

	// Remove the taint with exponential backoff
	backoffErr := wait.ExponentialBackoff(taintRemovalBackoff, func() (bool, error) {
		err := removeNotReadyTaint(clientset)
		if err != nil {
			klog.Errorf("taint: Failed to remove taint: %v", err)
			return false, nil
		}
		return true, nil
	})

	if backoffErr != nil {
		klog.Errorf("taint: Retries exhausted, giving up taint removal: %v", backoffErr)
	}
}

// removeNotReadyTaint removes the taint s3.csi.aws.com/agent-not-ready from the local node
// This taint can be optionally applied by users to prevent startup race conditions as described in:
// https://github.com/awslabs/mountpoint-s3-csi-driver/edit/main/docs/TROUBLESHOOTING.md#my-pod-is-stuck-at-containercreating-with-error-driver-name-s3csiawscom-not-found-in-the-list-of-registered-csi-drivers
func removeNotReadyTaint(clientset kubernetes.Interface) error {
	nodeName := os.Getenv("CSI_NODE_NAME")
	if nodeName == "" {
		klog.V(4).Infof("CSI_NODE_NAME missing, skipping taint removal")
		return nil
	}

	if clientset == nil {
		klog.V(4).Infof("Kubernetes clientset is nil, skipping taint removal")
		return nil
	}

	node, err := clientset.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	var taintsToKeep []corev1.Taint
	for _, taint := range node.Spec.Taints {
		if taint.Key != AgentNotReadyNodeTaintKey {
			taintsToKeep = append(taintsToKeep, taint)
		} else {
			klog.V(4).Infof("Queued taint for removal", "key", taint.Key, "effect", taint.Effect)
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

	_, err = clientset.CoreV1().Nodes().Patch(context.Background(), nodeName, k8stypes.JSONPatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return err
	}
	klog.Infof("taint: Removed taint(s) from local node: %s", nodeName)
	return nil
}
