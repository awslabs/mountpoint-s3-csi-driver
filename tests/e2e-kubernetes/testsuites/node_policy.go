package custom_testsuites

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	authenticationv1 "k8s.io/api/authentication/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/version"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/kubernetes/test/e2e/framework"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
)

const nodePolicyName = "s3-csi-driver-node"
const nodeNameExtraKey = "authentication.kubernetes.io/node-name"

// s3CSINodePolicyTestSuite checks the ValidatingAdmissionPolicy that limits the CSI driver's service account to removing
// the s3.csi.aws.com/agent-not-ready taint from the Node its pod runs on. It uses a real token of the CSI driver's service
// account, bound to a running node pod, like the node pod's own token. The taint-removal suite checks the node pod's own startup removal.
type s3CSINodePolicyTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

func InitS3NodePolicyTestSuite() storageframework.TestSuite {
	return &s3CSINodePolicyTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name: "node-policy",
			TestPatterns: []storageframework.TestPattern{
				storageframework.DefaultFsPreprovisionedPV,
			},
		},
	}
}

func (t *s3CSINodePolicyTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

func (t *s3CSINodePolicyTestSuite) SkipUnsupportedTests(_ storageframework.TestDriver, pattern storageframework.TestPattern) {
	if pattern.VolType != storageframework.PreprovisionedPV {
		e2eskipper.Skipf("Suite %q does not support %v", t.tsInfo.Name, pattern.VolType)
	}
}

func (t *s3CSINodePolicyTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	f := framework.NewFrameworkWithCustomTimeouts(NamespacePrefix+"node-policy", storageframework.GetDriverTimeouts(driver))
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

	// It taints a real node, so it doesn't run in parallel with other tests.
	Describe("Node ValidatingAdmissionPolicy", Serial, func() {
		It("should let the CSI driver's service account remove only the agent-not-ready taint from the node its pod runs on", func(ctx context.Context) {
			cs := f.ClientSet
			serverVersion, err := cs.Discovery().ServerVersion()
			framework.ExpectNoError(err)
			if version.MustParseGeneric(serverVersion.GitVersion).LessThan(version.MajorMinor(1, 30)) {
				Skip("the policy needs Kubernetes 1.30 or later")
			}
			if _, err := cs.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(ctx, nodePolicyName, metav1.GetOptions{}); apierrors.IsNotFound(err) {
				Skip("the node ValidatingAdmissionPolicy is not installed")
			}

			// A running node pod, and a token of its service account bound to it.
			pod := csiDriverPod(ctx, f)
			ownNode := pod.Spec.NodeName
			sa := csiDriverServiceAccount(ctx, f)
			expiration := int64(600)
			token, err := cs.CoreV1().ServiceAccounts(sa.Namespace).CreateToken(ctx, sa.Name, &authenticationv1.TokenRequest{
				Spec: authenticationv1.TokenRequestSpec{
					ExpirationSeconds: &expiration,
					BoundObjectRef:    &authenticationv1.BoundObjectReference{Kind: "Pod", APIVersion: "v1", Name: pod.Name, UID: pod.UID},
				},
			}, metav1.CreateOptions{})
			framework.ExpectNoError(err)
			nodeClient := clientWithToken(f.ClientConfig(), token.Status.Token)

			// The API server reads the node from the token, as the policy expects.
			review, err := nodeClient.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
			framework.ExpectNoError(err)
			Expect(review.Status.UserInfo.Username).To(Equal(fmt.Sprintf("system:serviceaccount:%s:%s", sa.Namespace, sa.Name)))
			Expect(review.Status.UserInfo.Extra[nodeNameExtraKey]).To(ConsistOf(authenticationv1.ExtraValue{ownNode}))

			nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
			framework.ExpectNoError(err)
			otherNode := ""
			for _, n := range nodes.Items {
				if n.Name != ownNode {
					otherNode = n.Name
					break
				}
			}
			Expect(otherNode).NotTo(BeEmpty(), "the cluster needs a second node")

			// Taint the real nodes briefly, with NoSchedule so running pods stay.
			const otherTaintKey = "s3.csi.aws.com/e2e-node-policy"
			for _, node := range []string{ownNode, otherNode} {
				for _, key := range []string{agentNotReadyTaintKey, otherTaintKey} {
					framework.ExpectNoError(patchTaints(ctx, cs, node, false, withTaint(v1.Taint{Key: key, Effect: v1.TaintEffectNoSchedule})))
					DeferCleanup(func(ctx context.Context) error {
						return patchTaints(ctx, cs, node, false, withoutTaint(key))
					})
				}
			}

			// Requests that the policy must deny are dry runs, so they can't change a real node.
			const dryRun = true
			expectDenied := func(err error, message string) {
				GinkgoHelper()
				Expect(err).To(MatchError(ContainSubstring(message)))
			}
			mergePatch := func(node, patch string, subresources ...string) error {
				_, err := nodeClient.CoreV1().Nodes().Patch(ctx, node, types.MergePatchType, []byte(patch),
					metav1.PatchOptions{DryRun: []string{metav1.DryRunAll}}, subresources...)
				return err
			}

			By("denying the agent-not-ready taint removal on another node")
			expectDenied(patchTaints(ctx, nodeClient, otherNode, dryRun, withoutTaint(agentNotReadyTaintKey)),
				"may only update its own Node object, not "+otherNode)

			By("denying adding a taint to its own node")
			expectDenied(patchTaints(ctx, nodeClient, ownNode, dryRun, withTaint(v1.Taint{Key: otherTaintKey + "-added", Effect: v1.TaintEffectNoSchedule})),
				"may only remove the s3.csi.aws.com/agent-not-ready taint")

			By("denying removing another taint from its own node")
			expectDenied(patchTaints(ctx, nodeClient, ownNode, dryRun, withoutTaint(otherTaintKey)),
				"may only remove the s3.csi.aws.com/agent-not-ready taint")

			By("denying a label change on its own node")
			expectDenied(mergePatch(ownNode, `{"metadata":{"labels":{"s3.csi.aws.com/e2e-node-policy":"denied"}}}`),
				"may only change the taints of the Node its pod runs on")

			By("denying an annotation change on its own node")
			expectDenied(mergePatch(ownNode, `{"metadata":{"annotations":{"s3.csi.aws.com/e2e-node-policy":"denied"}}}`),
				"may only change the taints of the Node its pod runs on")

			By("denying cordoning its own node")
			expectDenied(mergePatch(ownNode, `{"spec":{"unschedulable":true}}`),
				"may only change the taints of the Node its pod runs on")

			By("denying a status update of its own node")
			Expect(mergePatch(ownNode, `{"status":{"conditions":[{"type":"Ready","status":"False"}]}}`, "status")).To(HaveOccurred())

			By("denying deleting its own node")
			err = nodeClient.CoreV1().Nodes().Delete(ctx, ownNode, metav1.DeleteOptions{DryRun: []string{metav1.DryRunAll}})
			Expect(err).To(HaveOccurred())

			By("allowing the agent-not-ready taint removal on its own node")
			framework.ExpectNoError(patchTaints(ctx, nodeClient, ownNode, false, withoutTaint(agentNotReadyTaintKey)))
			node, err := cs.CoreV1().Nodes().Get(ctx, ownNode, metav1.GetOptions{})
			framework.ExpectNoError(err)
			Expect(node.Spec.Taints).NotTo(ContainElement(HaveField("Key", agentNotReadyTaintKey)))
			Expect(node.Spec.Taints).To(ContainElement(HaveField("Key", otherTaintKey)))
		})
	})
}

// clientWithToken returns a client for the same API server that authenticates only with the given bearer token.
func clientWithToken(config *rest.Config, token string) clientset.Interface {
	c := rest.AnonymousClientConfig(config)
	c.BearerToken = token
	client, err := clientset.NewForConfig(c)
	framework.ExpectNoError(err)
	return client
}

func withTaint(taint v1.Taint) func([]v1.Taint) []v1.Taint {
	return func(taints []v1.Taint) []v1.Taint {
		for _, t := range taints {
			if t.Key == taint.Key {
				return taints
			}
		}
		return append(taints, taint)
	}
}

func withoutTaint(key string) func([]v1.Taint) []v1.Taint {
	return func(taints []v1.Taint) []v1.Taint {
		var keep []v1.Taint
		for _, t := range taints {
			if t.Key != key {
				keep = append(keep, t)
			}
		}
		return keep
	}
}

// patchTaints changes a node's taints with the same JSON patch the node pod sends in removeNotReadyTaint: test the
// current taints, then replace them. A node without taints gets an add instead, since the test needs the path. The
// patch also carries the resourceVersion it read, so it doesn't overwrite concurrent taint changes, and it retries on
// a conflict.
func patchTaints(ctx context.Context, cs clientset.Interface, node string, dryRun bool, change func([]v1.Taint) []v1.Taint) error {
	options := metav1.PatchOptions{}
	if dryRun {
		options.DryRun = []string{metav1.DryRunAll}
	}
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		var n *v1.Node
		n, err = cs.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
		if err != nil {
			return err
		}
		resourceVersion := map[string]any{"op": "add", "path": "/metadata/resourceVersion", "value": n.ResourceVersion}
		ops := []map[string]any{resourceVersion, {"op": "add", "path": "/spec/taints", "value": change(n.Spec.Taints)}}
		if len(n.Spec.Taints) > 0 {
			ops = []map[string]any{
				resourceVersion,
				{"op": "test", "path": "/spec/taints", "value": n.Spec.Taints},
				{"op": "replace", "path": "/spec/taints", "value": change(n.Spec.Taints)},
			}
		}
		patch, merr := json.Marshal(ops)
		if merr != nil {
			return merr
		}
		_, err = cs.CoreV1().Nodes().Patch(ctx, node, types.JSONPatchType, patch, options)
		// Retry when the node changed in between, for example by the node lifecycle controller or Karpenter.
		if err == nil || !(apierrors.IsConflict(err) || strings.Contains(err.Error(), "testing value")) {
			return err
		}
	}
	return err
}
