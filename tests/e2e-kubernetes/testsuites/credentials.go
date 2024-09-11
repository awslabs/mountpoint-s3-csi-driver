package custom_testsuites

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	authenticationv1 "k8s.io/api/authentication/v1"
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

const serviceAccountTokenAudienceSTS = "sts.amazonaws.com"
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
	// The CSI driver supports driver-level and pod-level credentials:
	//   Driver-level (in order):
	// 	 	1) AWS credentials passed via Kubernetes secrets
	// 	 	2) IAM Roles for Service Accounts (IRSA)
	// 	 	3) IAM instance profile
	//   Pod-level:
	// 		1) IAM Roles for Service Accounts (IRSA)
	//
	// In our test environment we add "AmazonS3FullAccess" policy to our EC2 instances
	// (see "eksctl-patch.json" and "kops-patch.yaml") which allows Driver-level 3) to work.
	// In order to test if other driver-level and pod-level credentials correctly work,
	// we're trying to set a more restricted role (e.g. with "AmazonS3ReadOnlyAccess" policy),
	// in these test cases to ensure it does not fallback to Driver-level 3) credentials.

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
		slices.Reverse(l.cleanup) // clean items in reverse order similar to how `defer` works
		for _, f := range l.cleanup {
			errs = append(errs, f(ctx))
		}
		framework.ExpectNoError(errors.NewAggregate(errs), "while cleanup resource")
	}
	BeforeEach(func(ctx context.Context) {
		l = local{}
		l.config = driver.PrepareTest(ctx, f)
		DeferCleanup(cleanup)
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
		deferCleanup(vol.CleanupResource)
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

	expectFailToMount := func(ctx context.Context, withServiceAccountName string, mountOptions []string) {
		vol := createVolumeResourceWithMountOptions(ctx, l.config, pattern, mountOptions)
		deferCleanup(vol.CleanupResource)

		client := f.ClientSet.CoreV1().Pods(f.Namespace.Name)

		pod := e2epod.MakePod(f.Namespace.Name, nil, []*v1.PersistentVolumeClaim{vol.Pvc}, admissionapi.LevelBaseline, "")
		if withServiceAccountName != "" {
			pod.Spec.ServiceAccountName = withServiceAccountName
		}

		pod, err := client.Create(ctx, pod, metav1.CreateOptions{})
		framework.ExpectNoError(err)
		deferCleanup(func(ctx context.Context) error {
			// Since CSI driver returns an error in case of missing role annotation,
			// it takes some time to delete the object, with `gracePeriod=0` we're forcing an immediate deletion.
			return e2epod.DeletePodWithGracePeriod(ctx, f.ClientSet, pod, 0)
		})

		eventSelector := fields.Set{
			"involvedObject.kind":      "Pod",
			"involvedObject.name":      pod.Name,
			"involvedObject.namespace": f.Namespace.Name,
			"reason":                   events.FailedMountVolume,
		}.AsSelector().String()
		framework.Logf("Waiting for FailedMount event: %s", eventSelector)

		err = e2eevents.WaitTimeoutForEvent(ctx, f.ClientSet, f.Namespace.Name, eventSelector, "MountVolume.SetUp failed", 5*time.Minute)
		if err == nil {
			framework.Logf("Got FailedMount event: %s", eventSelector)
		} else {
			framework.Logf("Didn't get FailedMount event: %s", eventSelector)
		}

		pod, err = client.Get(ctx, pod.Name, metav1.GetOptions{})
		framework.ExpectNoError(err)
		gomega.Expect(pod.Status.Phase).To(gomega.Equal(v1.PodPending))
	}

	// Since we're modifying cluster-wide resources in credential tests,
	// we shouldn't run them in parallel with other tests.
	//                        |
	//                      ------
	Describe("Credentials", Serial, Ordered, func() {
		cleanClusterWideResources := func(ctx context.Context) {
			// Since we're using cluster-wide resources and we're running multiple tests in the same cluster,
			// we need to clean up all credential related resources before each test to ensure we've a
			// clean starting point in each test.
			By("Cleaning up cluster-wide resources")

			sa := csiDriverServiceAccount(ctx, f)
			overrideServiceAccountRole(ctx, f, sa, "")

			framework.ExpectNoError(deleteCredentialSecret(ctx, f))

			// Trigger recreation of our pods to ensure they're not using deleted resources
			killCSIDriverPods(ctx, f)
		}

		var (
			oidcProvider      string
			policyRoleMapping = map[string]*iamtypes.Role{}
		)

		BeforeAll(func(ctx context.Context) {
			oidcProvider = oidcProviderForCluster(ctx, f)

			var afterAllCleanup []func(context.Context) error

			By("Pre-creating IAM roles for common policies")
			for _, policyARN := range []string{
				iamPolicyS3FullAccess,
				iamPolicyS3ReadOnlyAccess,
				iamPolicyS3NoAccess,
			} {
				role, removeRole := createRole(ctx, f, assumeRolePolicyDocument(ctx), policyARN)
				policyRoleMapping[policyARN] = role
				afterAllCleanup = append(afterAllCleanup, removeRole)
			}

			DeferCleanup(func(ctx context.Context) {
				var errs []error
				for _, f := range afterAllCleanup {
					errs = append(errs, f(ctx))
				}
				framework.ExpectNoError(errors.NewAggregate(errs), "while cleanup global resource")
			})
		})

		AfterEach(func(ctx context.Context) {
			cleanClusterWideResources(ctx)
		})

		updateCSIDriversServiceAccountRole := func(ctx context.Context, policyARN string) {
			By("Updating CSI Driver's Service Account Role")
			sa := csiDriverServiceAccount(ctx, f)

			role, removeRole := createRole(ctx, f, assumeRoleWithWebIdentityPolicyDocument(ctx, oidcProvider, sa), policyARN)
			deferCleanup(removeRole)

			sa, restoreServiceAccountRole := overrideServiceAccountRole(ctx, f, sa, *role.Arn)
			deferCleanup(restoreServiceAccountRole)

			waitUntilRoleIsAssumableWithWebIdentity(ctx, f, sa)

			// Trigger recreation of our pods to use the new IAM role
			killCSIDriverPods(ctx, f)
		}

		updateDriverLevelKubernetesSecret := func(ctx context.Context, policyARN string) {
			By("Updating Kubernetes Secret with temporary credentials")

			role, ok := policyRoleMapping[policyARN]
			if !ok {
				framework.Failf("Missing role mapping for policy %s", policyARN)
			}
			assumeRoleOutput := assumeRole(ctx, f, *role.Arn)

			_, deleteSecret := createCredentialSecret(ctx, f, assumeRoleOutput.Credentials)
			deferCleanup(deleteSecret)

			// Trigger recreation of our pods to use the new credentials
			killCSIDriverPods(ctx, f)
		}

		Describe("Driver Level", Ordered, func() {
			BeforeEach(func(ctx context.Context) {
				cleanClusterWideResources(ctx)
			})

			Context("IAM Instance Profiles", func() {
				// We always have instance profile with "AmazonS3FullAccess" policy in EC2 instances of our test cluster,
				// see the comments in the beginning of this function.
				It("should use ec2 instance profile's full access role", func(ctx context.Context) {
					pod := createPodAllowsDelete(ctx)
					expectFullAccess(pod)
				})
			})

			Context("IAM Roles for Service Accounts (IRSA)", Ordered, func() {
				BeforeEach(func(ctx context.Context) {
					if oidcProvider == "" {
						Skip("OIDC provider is not configured, skipping IRSA tests")
					}
				})

				It("should use service account's read-only role", func(ctx context.Context) {
					updateCSIDriversServiceAccountRole(ctx, iamPolicyS3ReadOnlyAccess)
					pod := createPodWithVolume(ctx)
					expectReadOnly(pod)
				})

				It("should use service account's full access role", func(ctx context.Context) {
					updateCSIDriversServiceAccountRole(ctx, iamPolicyS3FullAccess)
					pod := createPodAllowsDelete(ctx)
					expectFullAccess(pod)
				})

				It("should fail to mount if service account's role does not allow s3::ListObjectsV2", func(ctx context.Context) {
					updateCSIDriversServiceAccountRole(ctx, iamPolicyS3NoAccess)
					expectFailToMount(ctx, "", nil)
				})
			})

			Context("Credentials via Kubernetes Secrets", func() {
				It("should use read-only access aws credentials", func(ctx context.Context) {
					updateDriverLevelKubernetesSecret(ctx, iamPolicyS3ReadOnlyAccess)
					pod := createPodWithVolume(ctx)
					expectReadOnly(pod)
				})

				It("should use full access aws credentials", func(ctx context.Context) {
					updateDriverLevelKubernetesSecret(ctx, iamPolicyS3FullAccess)
					pod := createPodAllowsDelete(ctx)
					expectFullAccess(pod)
				})

				It("should fail to mount if aws credentials does not allow s3::ListObjectsV2", func(ctx context.Context) {
					updateDriverLevelKubernetesSecret(ctx, iamPolicyS3NoAccess)
					expectFailToMount(ctx, "", nil)
				})
			})
		})

		Describe("Pod level", func() {
			enablePodLevelIdentity := func(ctx context.Context) context.Context {
				return contextWithAuthenticationSource(ctx, "pod")
			}

			enableDriverLevelIdentity := func(ctx context.Context) context.Context {
				return contextWithAuthenticationSource(ctx, "driver")
			}

			Context("IAM Roles for Service Accounts (IRSA)", Ordered, func() {
				BeforeEach(func(ctx context.Context) {
					if oidcProvider == "" {
						Skip("OIDC provider is not configured, skipping IRSA tests")
					}
				})

				assignPolicyToServiceAccount := func(ctx context.Context, sa *v1.ServiceAccount, policyARN string) *v1.ServiceAccount {
					role, removeRole := createRole(ctx, f, assumeRoleWithWebIdentityPolicyDocument(ctx, oidcProvider, sa), policyARN)
					deferCleanup(removeRole)

					sa, _ = overrideServiceAccountRole(ctx, f, sa, *role.Arn)
					waitUntilRoleIsAssumableWithWebIdentity(ctx, f, sa)
					return sa
				}

				createServiceAccountWithPolicy := func(ctx context.Context, policyARN string) *v1.ServiceAccount {
					sa, removeSA := createServiceAccount(ctx, f)
					deferCleanup(removeSA)

					return assignPolicyToServiceAccount(ctx, sa, policyARN)
				}

				createPodWithServiceAccountAndPolicy := func(ctx context.Context, policyARN string, allowDelete bool) (*v1.Pod, *v1.ServiceAccount) {
					By("Creating Pod with ServiceAccount")

					var mountOptions []string
					if allowDelete {
						mountOptions = append(mountOptions, "allow-delete")
					}
					vol := createVolumeResourceWithMountOptions(enablePodLevelIdentity(ctx), l.config, pattern, mountOptions)
					deferCleanup(vol.CleanupResource)

					sa := createServiceAccountWithPolicy(ctx, policyARN)

					pod, err := createPodWithServiceAccount(ctx, f.ClientSet, f.Namespace.Name, []*v1.PersistentVolumeClaim{vol.Pvc}, sa.Name)
					framework.ExpectNoError(err)
					deferCleanup(func(ctx context.Context) error { return e2epod.DeletePodWithWait(ctx, f.ClientSet, pod) })

					return pod, sa
				}

				It("should use pod's service account's read-only role", func(ctx context.Context) {
					pod, _ := createPodWithServiceAccountAndPolicy(ctx, iamPolicyS3ReadOnlyAccess, false)
					expectReadOnly(pod)
				})

				It("should use pod's service account's full access role", func(ctx context.Context) {
					pod, _ := createPodWithServiceAccountAndPolicy(ctx, iamPolicyS3FullAccess, true)
					expectFullAccess(pod)
				})

				It("should fail to mount if pod's service account's role does not allow s3::ListObjectsV2", func(ctx context.Context) {
					sa := createServiceAccountWithPolicy(ctx, iamPolicyS3NoAccess)
					expectFailToMount(enablePodLevelIdentity(ctx), sa.Name, nil)
				})

				It("should fail to mount if pod's service account does not have an associated role", func(ctx context.Context) {
					sa, removeSA := createServiceAccount(ctx, f)
					deferCleanup(removeSA)

					expectFailToMount(enablePodLevelIdentity(ctx), sa.Name, nil)
				})

				It("should refresh credentials after receiving new tokens", func(ctx context.Context) {
					// TODO:
					// 1. Trigger a manual `TokenRequest` or wait for it's own lifecylce
					// 2. Assert new token file is written to the Pod
				})

				It("should use up to date role associated with pod's service account", func(ctx context.Context) {
					vol := createVolumeResourceWithMountOptions(enablePodLevelIdentity(ctx), l.config, pattern, []string{"allow-delete"})
					deferCleanup(vol.CleanupResource)

					// Create a SA with full access role
					sa := createServiceAccountWithPolicy(ctx, iamPolicyS3FullAccess)

					pod, err := createPodWithServiceAccount(ctx, f.ClientSet, f.Namespace.Name, []*v1.PersistentVolumeClaim{vol.Pvc}, sa.Name)
					framework.ExpectNoError(err)

					expectFullAccess(pod)

					// Associate SA with read-only access role
					sa = assignPolicyToServiceAccount(ctx, sa, iamPolicyS3ReadOnlyAccess)

					// Re-create the pod
					framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
					pod, err = createPodWithServiceAccount(ctx, f.ClientSet, f.Namespace.Name, []*v1.PersistentVolumeClaim{vol.Pvc}, sa.Name)
					framework.ExpectNoError(err)
					defer func() {
						framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, f.ClientSet, pod))
					}()

					// The pod should only have a read-only access now
					expectReadOnly(pod)
				})

				It("should not use csi driver's service account tokens", func(ctx context.Context) {
					updateCSIDriversServiceAccountRole(ctx, iamPolicyS3FullAccess)

					pod, _ := createPodWithServiceAccountAndPolicy(ctx, iamPolicyS3ReadOnlyAccess, true)
					expectReadOnly(pod)
				})

				It("should not use driver-level kubernetes secrets", func(ctx context.Context) {
					updateDriverLevelKubernetesSecret(ctx, iamPolicyS3FullAccess)

					pod, _ := createPodWithServiceAccountAndPolicy(ctx, iamPolicyS3ReadOnlyAccess, true)
					expectReadOnly(pod)
				})

				It("should not mix different pod's service account tokens even when they are using the same volume", func(ctx context.Context) {
					vol := createVolumeResourceWithMountOptions(enablePodLevelIdentity(ctx), l.config, pattern, []string{"allow-delete"})
					deferCleanup(vol.CleanupResource)

					saFullAccess := createServiceAccountWithPolicy(ctx, iamPolicyS3FullAccess)
					saReadOnlyAccess := createServiceAccountWithPolicy(ctx, iamPolicyS3ReadOnlyAccess)

					podFullAccess, err := createPodWithServiceAccount(ctx, f.ClientSet, f.Namespace.Name, []*v1.PersistentVolumeClaim{vol.Pvc}, saFullAccess.Name)
					framework.ExpectNoError(err)
					deferCleanup(func(ctx context.Context) error { return e2epod.DeletePodWithWait(ctx, f.ClientSet, podFullAccess) })

					podReadOnlyAccess, err := createPodWithServiceAccount(ctx, f.ClientSet, f.Namespace.Name, []*v1.PersistentVolumeClaim{vol.Pvc}, saReadOnlyAccess.Name)
					framework.ExpectNoError(err)
					deferCleanup(func(ctx context.Context) error { return e2epod.DeletePodWithWait(ctx, f.ClientSet, podReadOnlyAccess) })

					expectReadOnly(podReadOnlyAccess)
					expectFullAccess(podFullAccess)

					// Write a file on full-access pod and expect it to be readable by read-only pod,
					// but writes from read-only pod should still fail.
					writtenFile := expectWriteToSucceed(podFullAccess)
					expectReadToSucceed(podReadOnlyAccess, writtenFile)
					expectWriteToFail(podReadOnlyAccess)
				})

				It("should not use pod's service account's role if 'authenticationSource' is 'driver'", func(ctx context.Context) {
					updateDriverLevelKubernetesSecret(ctx, iamPolicyS3ReadOnlyAccess)

					vol := createVolumeResourceWithMountOptions(enableDriverLevelIdentity(ctx), l.config, pattern, []string{"allow-delete"})
					deferCleanup(vol.CleanupResource)

					sa := createServiceAccountWithPolicy(ctx, iamPolicyS3FullAccess)

					pod, err := createPodWithServiceAccount(ctx, f.ClientSet, f.Namespace.Name, []*v1.PersistentVolumeClaim{vol.Pvc}, sa.Name)
					framework.ExpectNoError(err)
					deferCleanup(func(ctx context.Context) error { return e2epod.DeletePodWithWait(ctx, f.ClientSet, pod) })

					expectReadOnly(pod)
				})
			})
		})
	})
}

//-- IAM & STS utils

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
	framework.Logf("Assuming IAM role %s", roleArn)

	client := sts.NewFromConfig(awsConfig(ctx))
	return waitUntilRoleIsAssumable(ctx, client.AssumeRole, &sts.AssumeRoleInput{
		RoleArn:         ptr.To(roleArn),
		RoleSessionName: ptr.To(f.BaseName),
		DurationSeconds: ptr.To(int32(stsAssumeRoleCredentialDuration.Seconds())),
	})
}

// waitUntilRoleIsAssumable waits until the given role is assumable.
// This is needed because we're creating new roles in our test cases and then trying to assume those roles,
// but there is a delay between IAM and STS services and newly created roles/policies does not appear on STS immediately.
func waitUntilRoleIsAssumable[Input any, Output any](ctx context.Context, assumeFunc func(context.Context, *Input, ...func(*sts.Options)) (*Output, error), input *Input) *Output {
	ctx, cancel := context.WithTimeout(ctx, stsAssumeRoleTimeout)
	defer cancel()

	output, err := assumeFunc(ctx, input, func(o *sts.Options) {
		o.Retryer = retry.AddWithErrorCodes(o.Retryer, stsAssumeRoleRetryCode)
		o.Retryer = retry.AddWithMaxAttempts(o.Retryer, stsAssumeRoleRetryMaxAttemps)
		o.Retryer = retry.AddWithMaxBackoffDelay(o.Retryer, stsAssumeRoleRetryMaxBackoffDelay)
	})
	framework.ExpectNoError(err)
	gomega.Expect(output).ToNot(gomega.BeNil())

	return output
}

func waitUntilRoleIsAssumableWithWebIdentity(ctx context.Context, f *framework.Framework, sa *v1.ServiceAccount) {
	roleARN := sa.Annotations[roleARNAnnotation]
	framework.Logf("Waiting until IAM role %s for ServiceAccount %s is assumable with web identity", roleARN, sa.Name)

	saClient := f.ClientSet.CoreV1().ServiceAccounts(sa.Namespace)
	serviceAccountToken, err := saClient.CreateToken(ctx, sa.Name, &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			Audiences: []string{serviceAccountTokenAudienceSTS},
		},
	}, metav1.CreateOptions{})
	framework.ExpectNoError(err)

	client := sts.NewFromConfig(awsConfig(ctx))
	waitUntilRoleIsAssumable(ctx, client.AssumeRoleWithWebIdentity, &sts.AssumeRoleWithWebIdentityInput{
		RoleArn:          ptr.To(roleARN),
		RoleSessionName:  ptr.To(f.BaseName),
		WebIdentityToken: ptr.To(serviceAccountToken.Status.Token),
		DurationSeconds:  ptr.To(int32(stsAssumeRoleCredentialDuration.Seconds())),
	})
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

	framework.ExpectNoError(waitForKubernetesObject(ctx, framework.GetObject(client.Get, secret.Name, metav1.GetOptions{})))

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
	if sa.Annotations == nil {
		sa.Annotations = make(map[string]string)
	}
	sa.Annotations[roleARNAnnotation] = roleARN
}

// overrideServiceAccountRole overrides and updates given Service Account's EKS Role ARN annotation.
// This causes pod's using this Service Account to assume this new `roleARN` while authenticating with AWS.
// The returned function restored Service Account's EKS Role ARN annotation to it's original value.
func overrideServiceAccountRole(ctx context.Context, f *framework.Framework, sa *v1.ServiceAccount, roleARN string) (*v1.ServiceAccount, func(context.Context) error) {
	originalRoleARN := sa.Annotations[roleARNAnnotation]
	framework.Logf("Overriding ServiceAccount %s's role", sa.Name)

	client := f.ClientSet.CoreV1().ServiceAccounts(sa.Namespace)
	annotateServiceAccountWithRole(sa, roleARN)
	sa, err := client.Update(ctx, sa, metav1.UpdateOptions{})
	framework.ExpectNoError(err)

	return sa, func(ctx context.Context) error {
		sa, err := client.Get(ctx, sa.Name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}

		framework.Logf("Restoring ServiceAccount %s's role", sa.Name)
		annotateServiceAccountWithRole(sa, originalRoleARN)
		_, err = client.Update(ctx, sa, metav1.UpdateOptions{})
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

//-- Test Driver Context utils

type contextKey string

const authenticationSourceKey contextKey = "authenticationSource"

// contextWithAdditionalVolumeAttributes enhances given context with given authentication source.
// This value is used by `s3Volume.CreateVolume` and `s3Volume.GetPersistentVolumeSource`.
//
// This is kinda a magical way to pass values to those functions, but since Kubernetes Storage Test framework
// does not allow us to passing extra values, this is the only way to achieve that without duplicating the framework code.
func contextWithAuthenticationSource(ctx context.Context, authenticationSource string) context.Context {
	return context.WithValue(ctx, authenticationSourceKey, authenticationSource)
}

// AuthenticationSourceFromContext returns authentication source set for given context.
func AuthenticationSourceFromContext(ctx context.Context) string {
	val, _ := ctx.Value(authenticationSourceKey).(string)
	return val
}
