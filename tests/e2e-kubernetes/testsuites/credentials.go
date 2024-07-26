package custom_testsuites

import (
	"context"
	"fmt"
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
	assumeRolePolicyDocumentTemplate = `{
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
}`
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

	const (
		testVolumePath = "/mnt/volume1"
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

	// Since we're modifying cluster-wise resources in driver-level tests,
	// we shouldn't run them in parallel with other tests.
	//                                    |
	//                              -------------
	ginkgo.Describe("Driver Level", ginkgo.Serial, func() {
		ginkgo.Context("Credentials via Kubernetes Secrets", func() {
			updateCredentials := func(ctx context.Context, policyARN string) {
				credentials, cleanupCredentials := createTemporaryCredentialsForPolicy(ctx, f, policyARN)
				deferCleanup(cleanupCredentials)

				_, deleteSecret := createCredentialSecret(ctx, f, credentials)
				deferCleanup(deleteSecret)

				// Trigger recreation of our pods to use the new credentials
				killCSIDriverPods(ctx, f)
				// We should recreate our pods after deleting Kubernetes Secret to ensure
				// we're not using deleted credentials
				deferCleanup(func(ctx context.Context) error {
					killCSIDriverPods(ctx, f)
					return nil
				})
			}

			ginkgo.It("should use read-only access aws credentials", func(ctx context.Context) {
				updateCredentials(ctx, iamPolicyS3ReadOnlyAccess)
				pod := createPodWithVolume(ctx)

				expectListToSucceed(pod)
				expectWriteToFail(pod)
			})

			ginkgo.It("should use full access aws credentials", func(ctx context.Context) {
				updateCredentials(ctx, iamPolicyS3FullAccess)

				vol := createVolumeResourceWithMountOptions(ctx, l.config, pattern, []string{"allow-delete"})
				pod := createPod(ctx, vol)

				writtenFile := expectWriteToSucceed(pod)
				expectReadToSucceed(pod, writtenFile)
				expectDeleteToSucceed(pod, writtenFile)
				expectListToSucceed(pod)
			})

			ginkgo.It("should fail to mount if aws credentials does not allow s3::ListObjectsV2", func(ctx context.Context) {
				updateCredentials(ctx, iamPolicyS3NoAccess)
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
			})
		})

	})
}

func createTemporaryCredentialsForPolicy(ctx context.Context, f *framework.Framework, policyARN string) (*ststypes.Credentials, func(context.Context) error) {
	framework.Logf("Creating temporary credentials")
	callerIdentity := stsCallerIdentity(ctx)
	role, removeRole := createRole(ctx, f, *callerIdentity.Arn, policyARN)
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

func createRole(ctx context.Context, f *framework.Framework, assumableByPrincipal string, policyARNs ...string) (*iamtypes.Role, func(context.Context) error) {
	framework.Logf("Creating IAM role")

	client := iam.NewFromConfig(awsConfig(ctx))

	roleName := fmt.Sprintf("%s-%s", f.BaseName, uuid.NewString())
	role, err := client.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName:                 ptr.To(roleName),
		AssumeRolePolicyDocument: ptr.To(fmt.Sprintf(assumeRolePolicyDocumentTemplate, assumableByPrincipal)),
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

func createCredentialSecret(ctx context.Context, f *framework.Framework, credentials *ststypes.Credentials) (*v1.Secret, func(context.Context) error) {
	framework.Logf("Creating Kubernetes Secret for AWS Credentials")

	client := f.ClientSet.CoreV1().Secrets(csiDriverDaemonSetNamespace)
	secretName := "aws-secret"

	secret, err := client.Create(ctx, &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: secretName,
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
		return client.Delete(ctx, secretName, metav1.DeleteOptions{})
	}
}
