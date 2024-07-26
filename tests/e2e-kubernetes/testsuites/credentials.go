package custom_testsuites

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
	"github.com/google/uuid"
	ginkgo "github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/test/e2e/framework"
	e2eevents "k8s.io/kubernetes/test/e2e/framework/events"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
	"k8s.io/utils/ptr"
)

const (
	iamPolicyS3FullAccess     = "arn:aws:iam::aws:policy/AmazonS3FullAccess"
	iamPolicyS3ReadOnlyAccess = "arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess"
	iamPolicyS3NoAccess       = "arn:aws:iam::aws:policy/AmazonEC2ReadOnlyAccess" // `AmazonEC2ReadOnlyAccess` gives no S3 access
)

const (
	stsAssumeRoleCredentialDuration = 15 * time.Minute

	// Since we create and immediately assume roles in our tests, the delay between IAM and STS causes
	// "AccessDenied" exceptions until they are in sync. We're retrying on "AccessDenied" as a workaround.
	stsAssumeRoleTimeout              = 2 * time.Minute
	stsAssumeRoleRetryCode            = "AccessDenied"
	stsAssumeRoleRetryMaxAttemps      = 0 // This will cause SDK to retry indefinetly, but we do have a timeout on the operation
	stsAssumeRoleRetryMaxBackoffDelay = 10 * time.Second
)

const roleARNAnnotation = "eks.amazonaws.com/role-arn"

const credentialSecretName = "aws-secret"

type s3CSICredentialsTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

func InitS3CSICredentialsTestSuite() storageframework.TestSuite {
	return &s3CSICredentialsTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name: "credentials",
			TestPatterns: []storageframework.TestPattern{
				storageframework.DefaultFsPreprovisionedPV,
			},
		},
	}
}

func (t *s3CSICredentialsTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

func (t *s3CSICredentialsTestSuite) SkipUnsupportedTests(_ storageframework.TestDriver, pattern storageframework.TestPattern) {
	if pattern.VolType != storageframework.PreprovisionedPV {
		e2eskipper.Skipf("Suite %q does not support %v", t.tsInfo.Name, pattern.VolType)
	}
}

func (t *s3CSICredentialsTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	// The CSI driver supports the following mechanisms (in order):
	// 	 1) AWS credentials passed via Kubernetes secrets
	// 	 2) IAM Roles for Service Accounts (IRSA)
	// 	 3) IAM instance profile
	// In our test environment we add "AmazonS3FullAccess" policy to our EC2 instances
	// (see "eksctl-patch.json" and "kops-patch.yaml") which allows 3) to work.
	// In order to test if 1) and 2) works, we're trying to set a more restricted role (e.g. with "AmazonS3ReadOnlyAccess" policy),
	// to ensure 1) and 2) correctly works and it does not fallback to 3).

	f := framework.NewFrameworkWithCustomTimeouts(NamespacePrefix+"credentials", storageframework.GetDriverTimeouts(driver))
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged

	type local struct {
		config *storageframework.PerTestConfig

		// A list of cleanup functions to be called after each test to clean resources created during the test.
		cleanup []func(context.Context) error
	}

	var l local

	deferCleanup := func(f func(context.Context) error) {
		l.cleanup = append(l.cleanup, f)
	}

	cleanup := func(ctx context.Context) {
		var errs []error
		for _, f := range l.cleanup {
			errs = append(errs, f(ctx))
		}
		framework.ExpectNoError(errors.NewAggregate(errs), "while cleanup resource")
	}
	ginkgo.BeforeEach(func(ctx context.Context) {
		l = local{}
		l.config = driver.PrepareTest(ctx, f)
		ginkgo.DeferCleanup(cleanup)
	})

	createVolume := func(ctx context.Context) *storageframework.VolumeResource {
		vol := storageframework.CreateVolumeResource(ctx, driver, l.config, pattern, t.GetTestSuiteInfo().SupportedSizeRange)
		deferCleanup(vol.CleanupResource)

		return vol
	}

	createPod := func(ctx context.Context, vol *storageframework.VolumeResource) *v1.Pod {
		pod, err := e2epod.CreateClientPod(ctx, f.ClientSet, f.Namespace.Name, vol.Pvc)
		framework.ExpectNoError(err)
		deferCleanup(func(ctx context.Context) error { return e2epod.DeletePodWithWait(ctx, f.ClientSet, pod) })

		return pod
	}

	createPodWithVolume := func(ctx context.Context) *v1.Pod {
		vol := createVolume(ctx)
		return createPod(ctx, vol)
	}

	createPodAllowsDelete := func(ctx context.Context) *v1.Pod {
		vol := createVolumeResourceWithMountOptions(ctx, l.config, pattern, []string{"allow-delete"})
		return createPod(ctx, vol)
	}

	const (
		testVolumePath = e2epod.VolumeMountPath1
		testFilePath   = testVolumePath + "/file.txt"
		testWriteSize  = 1024 // 1KB
	)

	type writtenFile struct {
		path string
		seed int64
		size int
	}

	expectWriteToSucceed := func(pod *v1.Pod) writtenFile {
		seed := time.Now().UTC().UnixNano()
		framework.Logf("checking writing to %s", testFilePath)
		checkWriteToPath(f, pod, testFilePath, testWriteSize, seed)
		return writtenFile{testFilePath, seed, testWriteSize}
	}

	expectReadToSucceed := func(pod *v1.Pod, file writtenFile) {
		framework.Logf("checking reading from %s", file.path)
		checkReadFromPath(f, pod, file.path, file.size, file.seed)
	}

	expectDeleteToSucceed := func(pod *v1.Pod, file writtenFile) {
		framework.Logf("checking if deletion of %s succeeds", file.path)
		checkDeletingPath(f, pod, file.path)
	}

	expectWriteToFail := func(pod *v1.Pod) {
		seed := time.Now().UTC().UnixNano()
		framework.Logf("checking if writing to %s fails", testFilePath)
		checkWriteToPathFails(f, pod, testFilePath, testWriteSize, seed)
	}

	expectListToSucceed := func(pod *v1.Pod) {
		framework.Logf("checking listing %s", testVolumePath)
		checkListingPath(f, pod, testVolumePath)
	}

	expectReadOnly := func(pod *v1.Pod) {
		expectListToSucceed(pod)
		expectWriteToFail(pod)
	}

	expectFullAccess := func(pod *v1.Pod) {
		writtenFile := expectWriteToSucceed(pod)
		expectReadToSucceed(pod, writtenFile)
		expectDeleteToSucceed(pod, writtenFile)
		expectListToSucceed(pod)
	}

	expectFailToMount := func(ctx context.Context) {
		vol := createVolume(ctx)

		client := f.ClientSet.CoreV1().Pods(f.Namespace.Name)

		pod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{vol.Pvc}, admissionapi.LevelBaseline, "")
		pod, err := client.Create(ctx, pod, metav1.CreateOptions{})
		framework.ExpectNoError(err)
		deferCleanup(func(ctx context.Context) error { return e2epod.DeletePodWithWait(ctx, f.ClientSet, pod) })

		eventSelector := fields.Set{
			"involvedObject.kind":      "Pod",
			"involvedObject.name":      pod.Name,
			"involvedObject.namespace": f.Namespace.Name,
			"reason":                   events.FailedMountVolume,
		}.AsSelector().String()
		framework.Logf("Waiting for FailedMount event: %s", eventSelector)

		err = e2eevents.WaitTimeoutForEvent(ctx, f.ClientSet, f.Namespace.Name, eventSelector, "MountVolume.SetUp failed", 30*time.Second)
		if err == nil {
			framework.Logf("Got FailedMount event: %s", eventSelector)
		} else {
			framework.Logf("Didn't get FailedMount event: %s", eventSelector)
		}

		pod, err = client.Get(ctx, pod.Name, metav1.GetOptions{})
		framework.ExpectNoError(err)
		gomega.Expect(pod.Status.Phase).To(gomega.Equal(v1.PodPending))
	}

	// Since we're modifying cluster-wide resources in driver-level tests,
	// we shouldn't run them in parallel with other tests.
	//                                    |
	//                              -------------
	ginkgo.Describe("Driver Level", ginkgo.Serial, func() {
		ginkgo.BeforeEach(func(ctx context.Context) {
			// Since we're using cluster-wide resources and we're running multiple tests in the same cluster,
			// we need to clean up all crendential related resources before each test to ensure we've a
			// clean starting point in each test.

			sa := csiDriverServiceAccount(ctx, f)
			overrideServiceAccountRole(ctx, f, sa, "")

			framework.ExpectNoError(deleteCredentialSecret(ctx, f))

			// Trigger recreation of our pods to ensure they're not using deleted resources
			killCSIDriverPods(ctx, f)
		})

		ginkgo.Context("Credentials via Kubernetes Secrets", func() {
			updateCredentials := func(ctx context.Context, policyARN string) {
				credentials, cleanupCredentials := createTemporaryCredentialsForPolicy(ctx, f, policyARN)
				deferCleanup(cleanupCredentials)

				_, deleteSecret := createCredentialSecret(ctx, f, credentials)
				deferCleanup(deleteSecret)

				// Trigger recreation of our pods to use the new credentials
				killCSIDriverPods(ctx, f)
			}

			ginkgo.It("should use read-only access aws credentials", func(ctx context.Context) {
				updateCredentials(ctx, iamPolicyS3ReadOnlyAccess)
				pod := createPodWithVolume(ctx)
				expectReadOnly(pod)
			})

			ginkgo.It("should use full access aws credentials", func(ctx context.Context) {
				updateCredentials(ctx, iamPolicyS3FullAccess)
				pod := createPodAllowsDelete(ctx)
				expectFullAccess(pod)
			})

			ginkgo.It("should fail to mount if aws credentials does not allow s3::ListObjectsV2", func(ctx context.Context) {
				updateCredentials(ctx, iamPolicyS3NoAccess)
				expectFailToMount(ctx)
			})
		})

		ginkgo.Context("IAM Roles for Service Accounts (IRSA)", ginkgo.Ordered, func() {
			var oidcProvider string
			ginkgo.BeforeAll(func(ctx context.Context) {
				oidcProvider = oidcProviderForCluster(ctx, f)
				if oidcProvider == "" {
					ginkgo.Skip("OIDC provider is not configured, skipping IRSA tests")
				}
			})

			updateServiceAccountRole := func(ctx context.Context, policyARN string) {
				sa := csiDriverServiceAccount(ctx, f)

				role, removeRole := createRole(ctx, f, assumeRoleWithWebIdentityPolicyDocument(ctx, oidcProvider, sa), policyARN)
				deferCleanup(removeRole)

				restoreServiceAccountRole := overrideServiceAccountRole(ctx, f, sa, *role.Arn)
				deferCleanup(restoreServiceAccountRole)

				// Trigger recreation of our pods to use the new IAM role
				killCSIDriverPods(ctx, f)
			}

			ginkgo.It("should use service account's read-only role", func(ctx context.Context) {
				updateServiceAccountRole(ctx, iamPolicyS3ReadOnlyAccess)
				pod := createPodWithVolume(ctx)
				expectReadOnly(pod)
			})

			ginkgo.It("should use service account's full access role", func(ctx context.Context) {
				updateServiceAccountRole(ctx, iamPolicyS3FullAccess)
				pod := createPodAllowsDelete(ctx)
				expectFullAccess(pod)
			})

			ginkgo.It("should fail to mount if service account's role does not allow s3::ListObjectsV2", func(ctx context.Context) {
				updateServiceAccountRole(ctx, iamPolicyS3NoAccess)
				expectFailToMount(ctx)
			})
		})
	})
}

//-- IAM & STS utils

// createTemporaryCredentialsForPolicy creates a new IAM role with given `policyARN` and makes `sts::AssumeRole`
// call to obtain temporary AWS credentials for the newly created IAM role.
// The returned function removes created IAM role.
func createTemporaryCredentialsForPolicy(ctx context.Context, f *framework.Framework, policyARN string) (*ststypes.Credentials, func(context.Context) error) {
	framework.Logf("Creating temporary credentials")
	role, removeRole := createRole(ctx, f, assumeRolePolicyDocument(ctx), policyARN)
	assumeRole := assumeRole(ctx, f, *role.Arn)

	return assumeRole.Credentials, func(ctx context.Context) error {
		framework.Logf("Cleaning up temporary credentials")
		return removeRole(ctx)
	}
}

func stsCallerIdentity(ctx context.Context) *sts.GetCallerIdentityOutput {
	client := sts.NewFromConfig(awsConfig(ctx))

	identity, err := client.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	framework.ExpectNoError(err)

	return identity
}

func assumeRolePolicyDocument(ctx context.Context) string {
	arn := stsCallerIdentity(ctx).Arn
	return fmt.Sprintf(`{
	"Version": "2012-10-17",
	"Statement": [
	    {
	        "Effect": "Allow",
	        "Principal": {
                "AWS": "%s"
            },
	        "Action": "sts:AssumeRole"
	    }
	]
}`, *arn)
}

func assumeRoleWithWebIdentityPolicyDocument(ctx context.Context, oidcProvider string, sa *v1.ServiceAccount) string {
	awsAccount := stsCallerIdentity(ctx).Account

	buf, err := json.Marshal(&jsonMap{
		"Version": "2012-10-17",
		"Statement": []jsonMap{
			{
				"Effect": "Allow",
				"Principal": jsonMap{
					"Federated": fmt.Sprintf("arn:aws:iam::%s:oidc-provider/%s", *awsAccount, oidcProvider),
				},
				"Action": "sts:AssumeRoleWithWebIdentity",
				"Condition": jsonMap{
					"StringEquals": jsonMap{
						fmt.Sprintf("%s:aud", oidcProvider): "sts.amazonaws.com",
						fmt.Sprintf("%s:sub", oidcProvider): fmt.Sprintf("system:serviceaccount:%s:%s", sa.Namespace, sa.Name),
					},
				},
			},
		},
	})
	framework.ExpectNoError(err)

	return string(buf)
}

func createRole(ctx context.Context, f *framework.Framework, assumeRolePolicyDocument string, policyARNs ...string) (*iamtypes.Role, func(context.Context) error) {
	framework.Logf("Creating IAM role")

	client := iam.NewFromConfig(awsConfig(ctx))

	roleName := fmt.Sprintf("%s-%s", f.BaseName, uuid.NewString())
	role, err := client.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName:                 ptr.To(roleName),
		AssumeRolePolicyDocument: ptr.To(assumeRolePolicyDocument),
	})
	framework.ExpectNoError(err)

	deleteRole := func(ctx context.Context) error {
		framework.Logf("Deleting IAM role")
		_, err := client.DeleteRole(ctx, &iam.DeleteRoleInput{
			RoleName: ptr.To(roleName),
		})
		return err
	}

	for _, p := range policyARNs {
		_, err := client.AttachRolePolicy(ctx, &iam.AttachRolePolicyInput{
			RoleName:  ptr.To(roleName),
			PolicyArn: ptr.To(p),
		})
		framework.ExpectNoError(err)
	}

	return role.Role, func(ctx context.Context) error {
		var errs []error
		for _, p := range policyARNs {
			_, err := client.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{
				RoleName:  ptr.To(roleName),
				PolicyArn: ptr.To(p),
			})
			errs = append(errs, err)
		}
		errs = append(errs, deleteRole(ctx))
		return errors.NewAggregate(errs)
	}
}

func assumeRole(ctx context.Context, f *framework.Framework, roleArn string) *sts.AssumeRoleOutput {
	framework.Logf("Assuming IAM role")

	client := sts.NewFromConfig(awsConfig(ctx))

	ctx, cancel := context.WithTimeout(ctx, stsAssumeRoleTimeout)
	defer cancel()

	output, err := client.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn:         ptr.To(roleArn),
		RoleSessionName: ptr.To(f.BaseName),
		DurationSeconds: ptr.To(int32(stsAssumeRoleCredentialDuration.Seconds())),
	}, func(o *sts.Options) {
		o.Retryer = retry.AddWithErrorCodes(o.Retryer, stsAssumeRoleRetryCode)
		o.Retryer = retry.AddWithMaxAttempts(o.Retryer, stsAssumeRoleRetryMaxAttemps)
		o.Retryer = retry.AddWithMaxBackoffDelay(o.Retryer, stsAssumeRoleRetryMaxBackoffDelay)
	})

	framework.ExpectNoError(err)
	gomega.Expect(output).ToNot(gomega.BeNil())
	return output
}

//-- Credential Secret utils

// createCredentialSecret creates a Kubernetes Secret with given AWS credentials with the namespace and
// the name our CSI driver expects.
// The returned function removes created Kubernetes Secret.
func createCredentialSecret(ctx context.Context, f *framework.Framework, credentials *ststypes.Credentials) (*v1.Secret, func(context.Context) error) {
	framework.Logf("Creating Kubernetes Secret for AWS Credentials")

	client := f.ClientSet.CoreV1().Secrets(csiDriverDaemonSetNamespace)

	secret, err := client.Create(ctx, &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: credentialSecretName,
		},
		StringData: map[string]string{
			"key_id":        *credentials.AccessKeyId,
			"access_key":    *credentials.SecretAccessKey,
			"session_token": *credentials.SessionToken,
		},
	}, metav1.CreateOptions{})
	framework.ExpectNoError(err)

	return secret, func(ctx context.Context) error {
		framework.Logf("Deleting Kubernetes Secret")
		return deleteCredentialSecret(ctx, f)
	}
}

func deleteCredentialSecret(ctx context.Context, f *framework.Framework) error {
	framework.Logf("Deleting Kubernetes Secret")
	client := f.ClientSet.CoreV1().Secrets(csiDriverDaemonSetNamespace)

	err := client.Delete(ctx, credentialSecretName, metav1.DeleteOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	return framework.Gomega().Eventually(ctx, framework.HandleRetry(func(ctx context.Context) (*v1.Secret, error) {
		secret, err := client.Get(ctx, credentialSecretName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return secret, err
	})).WithTimeout(1 * time.Minute).Should(gomega.BeNil())
}

//-- Service Account utils

func annotateServiceAccountWithRole(sa *v1.ServiceAccount, roleARN string) {
	sa.Annotations[roleARNAnnotation] = roleARN
}

// overrideServiceAccountRole overrides and updates given Service Account's EKS Role ARN annotation.
// This causes pod's using this Service Account to assume this new `roleARN` while authenticating with AWS.
// The returned function restored Service Account's EKS Role ARN annotation to it's original value.
func overrideServiceAccountRole(ctx context.Context, f *framework.Framework, sa *v1.ServiceAccount, roleARN string) func(context.Context) error {
	originalRoleARN := sa.Annotations[roleARNAnnotation]
	framework.Logf("Overriding ServiceAccount %s's role", sa.Name)

	client := f.ClientSet.CoreV1().ServiceAccounts(sa.Namespace)
	annotateServiceAccountWithRole(sa, roleARN)
	sa, err := client.Update(ctx, sa, metav1.UpdateOptions{})
	framework.ExpectNoError(err)

	return func(ctx context.Context) error {
		framework.Logf("Restoring ServiceAccount %s's role", sa.Name)
		annotateServiceAccountWithRole(sa, originalRoleARN)
		_, err := client.Update(ctx, sa, metav1.UpdateOptions{})
		return err
	}
}

//-- OIDC utils

// oidcProviderForCluster tries to find configured OpenID Connect (OIDC) provider for the cluster we're testing against.
// It returns an empty string if it cannot find a suitable OIDC provider.
func oidcProviderForCluster(ctx context.Context, f *framework.Framework) string {
	client := f.ClientSet.CoreV1().RESTClient()

	response, err := client.Get().AbsPath("/.well-known/openid-configuration").DoRaw(ctx)
	if err != nil {
		framework.Logf("failed to find OIDC provider: %v", err)
		return ""
	}

	var configuration map[string]interface{}
	err = json.Unmarshal(response, &configuration)
	if err != nil {
		framework.Logf("failed to parse OIDC configuration: %v", err)
		return ""
	}

	issuer, _ := configuration["issuer"].(string)

	if !strings.HasPrefix(issuer, "https://oidc.eks") {
		// For now, we only set up a _public_ OIDC provider via `eksctl`,
		// with `kops` we're setting up a _local_ OIDC provider which wouldn't work with AWS IAM.
		// So, we're ignoring non-EKS OIDC providers.
		return ""
	}

	// For EKS, OIDC provider ID is the URL of the provider without "https://"
	return strings.TrimPrefix(issuer, "https://")
}
