package controller_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The CSI driver's service account with its RBAC, and the ValidatingAdmissionPolicy that limits it to removing the
// agent-not-ready taint from the Node its pod runs on. The Helm chart renders the same policy.
var nodePolicyManifests = []string{
	"../../deploy/kubernetes/base/serviceaccount-csi-node.yaml",
	"../../deploy/kubernetes/base/validatingadmissionpolicy-csi-node.yaml",
}

const nodeServiceAccountNamespace = "kube-system"
const nodeServiceAccountName = "s3-csi-driver-sa"
const nodeNameExtraKey = "authentication.kubernetes.io/node-name"
const agentNotReadyTaintKey = "s3.csi.aws.com/agent-not-ready"

var _ = Describe("Node ValidatingAdmissionPolicy", Ordered, func() {
	const (
		ownNodeName   = "node-policy-own"
		otherNodeName = "node-policy-other"
	)

	otherTaint := corev1.Taint{Key: "example.com/other", Effect: corev1.TaintEffectNoSchedule}
	agentNotReadyTaint := corev1.Taint{Key: agentNotReadyTaintKey, Effect: corev1.TaintEffectNoExecute}

	// ownNodeClient authenticates with a token of the CSI driver's service account bound to a pod on the own node, like
	// the node pod's own token.
	var ownNodeClient kubernetes.Interface
	clientset := func() kubernetes.Interface {
		GinkgoHelper()
		cs, err := kubernetes.NewForConfig(cfg)
		Expect(err).NotTo(HaveOccurred())
		return cs
	}

	BeforeAll(func() {
		for _, manifest := range nodePolicyManifests {
			applyManifest(manifest)
		}

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "node-policy-plugin", Namespace: nodeServiceAccountNamespace},
			Spec: corev1.PodSpec{
				NodeName:           ownNodeName,
				ServiceAccountName: nodeServiceAccountName,
				Containers:         []corev1.Container{{Name: "plugin", Image: defaultContainerImage}},
			},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0)))).To(Succeed())
		})

		ownNodeClient = tokenClient(&authenticationv1.BoundObjectReference{
			Kind: "Pod", APIVersion: "v1", Name: pod.Name, UID: pod.UID,
		})

		// The API server reads the node from the token, as the policy expects.
		review, err := ownNodeClient.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(review.Status.UserInfo.Username).To(Equal(
			fmt.Sprintf("system:serviceaccount:%s:%s", nodeServiceAccountNamespace, nodeServiceAccountName)))
		Expect(review.Status.UserInfo.Extra[nodeNameExtraKey]).To(ConsistOf(authenticationv1.ExtraValue{ownNodeName}))
	})

	BeforeEach(func() {
		for _, name := range []string{ownNodeName, otherNodeName} {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        name,
					Labels:      map[string]string{"example.com/label": "value"},
					Annotations: map[string]string{"example.com/annotation": "value"},
				},
				Spec: corev1.NodeSpec{Taints: []corev1.Taint{otherTaint, agentNotReadyTaint}},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			DeferCleanup(func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, node))).To(Succeed())
			})
		}
	})

	// patchNode applies the JSON patch that `change` builds from node `name` as `cs`.
	patchNode := func(cs kubernetes.Interface, name string, change func(*corev1.Node) []map[string]any) error {
		GinkgoHelper()
		node, err := clientset().CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		patch, err := json.Marshal(change(node))
		Expect(err).NotTo(HaveOccurred())
		_, err = cs.CoreV1().Nodes().Patch(ctx, name, k8stypes.JSONPatchType, patch, metav1.PatchOptions{})
		return err
	}

	// replaceTaints replaces the taints in the same way the node plugin's removeNotReadyTaint does.
	replaceTaints := func(change func([]corev1.Taint) []corev1.Taint) func(*corev1.Node) []map[string]any {
		return func(node *corev1.Node) []map[string]any {
			return []map[string]any{
				{"op": "test", "path": "/spec/taints", "value": node.Spec.Taints},
				{"op": "replace", "path": "/spec/taints", "value": change(node.Spec.Taints)},
			}
		}
	}

	withoutTaint := func(key string) func([]corev1.Taint) []corev1.Taint {
		return func(taints []corev1.Taint) []corev1.Taint {
			var keep []corev1.Taint
			for _, taint := range taints {
				if taint.Key != key {
					keep = append(keep, taint)
				}
			}
			return keep
		}
	}

	expectTaints := func(name string, present, absent string) {
		GinkgoHelper()
		node := &corev1.Node{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, node)).To(Succeed())
		Expect(node.Spec.Taints).To(ContainElement(HaveField("Key", present)))
		Expect(node.Spec.Taints).NotTo(ContainElement(HaveField("Key", absent)))
	}

	It("denies removing the agent-not-ready taint from another node", func() {
		// The policy takes effect a moment after it is created.
		Eventually(func() error {
			return patchNode(ownNodeClient, otherNodeName, replaceTaints(withoutTaint(agentNotReadyTaintKey)))
		}).WithTimeout(defaultWaitTimeout).WithPolling(defaultWaitRetryPeriod).
			Should(MatchError(ContainSubstring("may only update its own Node object, not " + otherNodeName)))
	})

	It("allows removing the agent-not-ready taint from its own node", func() {
		// The API server also adds node.kubernetes.io/not-ready to new nodes; the policy keeps it.
		Expect(patchNode(ownNodeClient, ownNodeName, replaceTaints(withoutTaint(agentNotReadyTaintKey)))).To(Succeed())
		expectTaints(ownNodeName, otherTaint.Key, agentNotReadyTaintKey)
	})

	It("allows removing the agent-not-ready taint when the other taints change order", func() {
		Expect(patchNode(ownNodeClient, ownNodeName, replaceTaints(func(taints []corev1.Taint) []corev1.Taint {
			keep := withoutTaint(agentNotReadyTaintKey)(taints)
			for i, j := 0, len(keep)-1; i < j; i, j = i+1, j-1 {
				keep[i], keep[j] = keep[j], keep[i]
			}
			return keep
		}))).To(Succeed())
		expectTaints(ownNodeName, otherTaint.Key, agentNotReadyTaintKey)
	})

	It("denies tokens that are not bound to a pod on a node", func() {
		err := patchNode(tokenClient(nil), ownNodeName, replaceTaints(withoutTaint(agentNotReadyTaintKey)))
		Expect(err).To(MatchError(ContainSubstring("the service account token is not bound to a pod on a node")))
	})

	It("denies removing another taint from its own node", func() {
		err := patchNode(ownNodeClient, ownNodeName, replaceTaints(withoutTaint(otherTaint.Key)))
		Expect(err).To(MatchError(ContainSubstring("may only remove the s3.csi.aws.com/agent-not-ready taint")))
	})

	It("denies adding a taint to its own node", func() {
		err := patchNode(ownNodeClient, ownNodeName, replaceTaints(func(taints []corev1.Taint) []corev1.Taint {
			return append(taints, corev1.Taint{Key: "example.com/added", Effect: corev1.TaintEffectNoSchedule})
		}))
		Expect(err).To(MatchError(ContainSubstring("may only remove the s3.csi.aws.com/agent-not-ready taint")))
	})

	It("denies changing a label of its own node", func() {
		err := patchNode(ownNodeClient, ownNodeName, func(*corev1.Node) []map[string]any {
			return []map[string]any{{"op": "replace", "path": "/metadata/labels/example.com~1label", "value": "changed"}}
		})
		Expect(err).To(MatchError(ContainSubstring("may only change the taints of the Node its pod runs on")))
	})

	It("denies changing an annotation of its own node", func() {
		err := patchNode(ownNodeClient, ownNodeName, func(*corev1.Node) []map[string]any {
			return []map[string]any{{"op": "replace", "path": "/metadata/annotations/example.com~1annotation", "value": "changed"}}
		})
		Expect(err).To(MatchError(ContainSubstring("may only change the taints of the Node its pod runs on")))
	})

	It("denies cordoning its own node", func() {
		err := patchNode(ownNodeClient, ownNodeName, func(*corev1.Node) []map[string]any {
			return []map[string]any{{"op": "add", "path": "/spec/unschedulable", "value": true}}
		})
		Expect(err).To(MatchError(ContainSubstring("may only change the taints of the Node its pod runs on")))
	})

	It("denies updating the status of its own node", func() {
		patch := []byte(`{"status":{"conditions":[{"type":"Ready","status":"False"}]}}`)
		_, err := ownNodeClient.CoreV1().Nodes().Patch(ctx, ownNodeName, k8stypes.StrategicMergePatchType, patch,
			metav1.PatchOptions{}, "status")
		Expect(err).To(HaveOccurred())
	})

	It("denies deleting its own node", func() {
		err := ownNodeClient.CoreV1().Nodes().Delete(ctx, ownNodeName, metav1.DeleteOptions{})
		Expect(err).To(HaveOccurred())
		expectTaints(ownNodeName, otherTaint.Key, "")
	})
})

// tokenClient returns a client that authenticates only with a token of the CSI driver's service account, bound to `bound`,
// or to no object if `bound` is nil.
func tokenClient(bound *authenticationv1.BoundObjectReference) kubernetes.Interface {
	GinkgoHelper()
	cs, err := kubernetes.NewForConfig(cfg)
	Expect(err).NotTo(HaveOccurred())
	expiration := int64(600)
	token, err := cs.CoreV1().ServiceAccounts(nodeServiceAccountNamespace).CreateToken(ctx, nodeServiceAccountName,
		&authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &expiration, BoundObjectRef: bound}},
		metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())

	tokenCfg := rest.AnonymousClientConfig(cfg)
	tokenCfg.BearerToken = token.Status.Token
	tokenCS, err := kubernetes.NewForConfig(tokenCfg)
	Expect(err).NotTo(HaveOccurred())
	return tokenCS
}

// applyManifest creates every object in the multi-document YAML file at `path`, and deletes them after the suite.
func applyManifest(path string) {
	GinkgoHelper()
	file, err := os.Open(path)
	Expect(err).NotTo(HaveOccurred())
	defer file.Close()

	decoder := yaml.NewYAMLOrJSONDecoder(file, 4096)
	for {
		obj := &unstructured.Unstructured{}
		err := decoder.Decode(&obj.Object)
		if errors.Is(err, io.EOF) {
			break
		}
		Expect(err).NotTo(HaveOccurred())
		if len(obj.Object) == 0 {
			continue
		}
		Expect(k8sClient.Create(ctx, obj)).To(Succeed(), "creating %s %s", obj.GetKind(), obj.GetName())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		})
	}
}
