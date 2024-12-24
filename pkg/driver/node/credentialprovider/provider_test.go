package credentialprovider_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/credentialprovider/awsprofile/awsprofiletest"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/envprovider"
	"github.com/awslabs/aws-s3-csi-driver/pkg/driver/node/volumecontext"
	"github.com/awslabs/aws-s3-csi-driver/pkg/util/testutil/assert"
)

const testAccessKeyID = "test-access-key-id"
const testSecretAccessKey = "test-secret-access-key"
const testSessionToken = "test-session-token"

const testRoleARN = "arn:aws:iam::111122223333:role/pod-a-role"
const testWebIdentityToken = "test-web-identity-token"

const testEnvPath = "/test-env"

func TestProvidingDriverLevelCredentials(t *testing.T) {
	volumeContextVariants := []map[string]string{
		{
			volumecontext.AuthenticationSource: credentialprovider.AuthenticationSourceDriver,
		},
		// It should default to driver-level identity if `authenticationSource` is not passed
		{
			volumecontext.AuthenticationSource: credentialprovider.AuthenticationSourceUnspecified,
		},
		{},
	}

	t.Run("only long-term credentials", func(t *testing.T) {
		for _, volCtx := range volumeContextVariants {
			setEnvForLongTermCredentials(t)
			writePath := t.TempDir()

			provider := credentialprovider.New(nil)
			credentials, err := provider.Provide(context.Background(), volCtx)
			assert.NoError(t, err)
			assert.Equals(t, credentialprovider.AuthenticationSourceDriver, credentials.Source())

			env, err := credentials.Dump(writePath, testEnvPath)
			assert.NoError(t, err)

			assert.Equals(t, envprovider.Environment{
				"AWS_PROFILE=s3-csi",
				"AWS_CONFIG_FILE=/test-env/s3-csi-config",
				"AWS_SHARED_CREDENTIALS_FILE=/test-env/s3-csi-credentials",
			}, env)
			assertLongTermCredentials(t, writePath)
		}
	})

	t.Run("only sts web identity credentials", func(t *testing.T) {
		for _, volCtx := range volumeContextVariants {
			setEnvForStsWebIdentityCredentials(t)
			writePath := t.TempDir()

			provider := credentialprovider.New(nil)
			credentials, err := provider.Provide(context.Background(), volCtx)
			assert.NoError(t, err)
			assert.Equals(t, credentialprovider.AuthenticationSourceDriver, credentials.Source())

			env, err := credentials.Dump(writePath, testEnvPath)
			assert.NoError(t, err)

			assert.Equals(t, envprovider.Environment{
				fmt.Sprintf("AWS_ROLE_ARN=%s", testRoleARN),
				"AWS_WEB_IDENTITY_TOKEN_FILE=/test-env/serviceaccount.token",
			}, env)
			assertWebIdentityTokenFile(t, writePath)
		}
	})

	t.Run("both long-term and sts web identity credentials", func(t *testing.T) {
		for _, volCtx := range volumeContextVariants {
			setEnvForLongTermCredentials(t)
			setEnvForStsWebIdentityCredentials(t)
			writePath := t.TempDir()

			provider := credentialprovider.New(nil)
			credentials, err := provider.Provide(context.Background(), volCtx)
			assert.NoError(t, err)
			assert.Equals(t, credentialprovider.AuthenticationSourceDriver, credentials.Source())

			env, err := credentials.Dump(writePath, testEnvPath)
			assert.NoError(t, err)

			assert.Equals(t, envprovider.Environment{
				"AWS_PROFILE=s3-csi",
				"AWS_CONFIG_FILE=/test-env/s3-csi-config",
				"AWS_SHARED_CREDENTIALS_FILE=/test-env/s3-csi-credentials",
				fmt.Sprintf("AWS_ROLE_ARN=%s", testRoleARN),
				"AWS_WEB_IDENTITY_TOKEN_FILE=/test-env/serviceaccount.token",
			}, env)
			assertLongTermCredentials(t, writePath)
			assertWebIdentityTokenFile(t, writePath)
		}
	})

	t.Run("no credentials", func(t *testing.T) {
		for _, volCtx := range volumeContextVariants {
			provider := credentialprovider.New(nil)
			credentials, err := provider.Provide(context.Background(), volCtx)
			assert.NoError(t, err)
			assert.Equals(t, credentialprovider.AuthenticationSourceDriver, credentials.Source())

			env, err := credentials.Dump(t.TempDir(), testEnvPath)
			assert.NoError(t, err)
			assert.Equals(t, envprovider.Environment{}, env)
		}
	})
}

func TestProvidingPodLevelCredentials(t *testing.T) {
	t.Run("correct values", func(t *testing.T) {
		clientset := fake.NewSimpleClientset(serviceAccount("test-sa", "test-ns", map[string]string{
			"eks.amazonaws.com/role-arn": testRoleARN,
		}))

		provider := credentialprovider.New(clientset.CoreV1())
		credentials, err := provider.Provide(context.Background(), map[string]string{
			volumecontext.AuthenticationSource:  credentialprovider.AuthenticationSourcePod,
			volumecontext.CSIPodNamespace:       "test-ns",
			volumecontext.CSIServiceAccountName: "test-sa",
			volumecontext.CSIServiceAccountTokens: serviceAccountTokens(t, tokens{
				"sts.amazonaws.com": {
					Token: testWebIdentityToken,
				},
			}),
		})
		assert.NoError(t, err)
		assert.Equals(t, credentialprovider.AuthenticationSourcePod, credentials.Source())

		writePath := t.TempDir()

		env, err := credentials.Dump(writePath, testEnvPath)
		assert.NoError(t, err)

		assert.Equals(t, envprovider.Environment{
			fmt.Sprintf("AWS_ROLE_ARN=%s", testRoleARN),
			"AWS_WEB_IDENTITY_TOKEN_FILE=/test-env/serviceaccount.token",

			// Having a unique cache key for namespace/serviceaccount pair
			"UNSTABLE_MOUNTPOINT_CACHE_KEY=test-ns/test-sa",

			// Disable long-term credentials
			"AWS_CONFIG_FILE=/test-env/disable-config",
			"AWS_SHARED_CREDENTIALS_FILE=/test-env/disable-credentials",

			// Disable EC2 credentials
			"AWS_EC2_METADATA_DISABLED=true",
		}, env)
		assertWebIdentityTokenFile(t, writePath)
	})

	t.Run("missing information", func(t *testing.T) {
		clientset := fake.NewSimpleClientset(
			serviceAccount("test-sa", "test-ns", map[string]string{
				"eks.amazonaws.com/role-arn": testRoleARN,
			}),
			serviceAccount("test-sa-missing-role", "test-ns", map[string]string{}),
		)

		for name, test := range map[string]struct {
			volumeContext map[string]string
		}{
			"unknown service account": {
				volumeContext: map[string]string{
					volumecontext.AuthenticationSource:  credentialprovider.AuthenticationSourcePod,
					volumecontext.CSIPodNamespace:       "test-ns",
					volumecontext.CSIServiceAccountName: "test-unknown-sa",
					volumecontext.CSIServiceAccountTokens: serviceAccountTokens(t, tokens{
						"sts.amazonaws.com": {
							Token: testWebIdentityToken,
						},
					}),
				},
			},
			"missing service account token": {
				volumeContext: map[string]string{
					volumecontext.AuthenticationSource:  credentialprovider.AuthenticationSourcePod,
					volumecontext.CSIPodNamespace:       "test-ns",
					volumecontext.CSIServiceAccountName: "test-sa",
				},
			},
			"missing sts audience in service account tokens": {
				volumeContext: map[string]string{
					volumecontext.AuthenticationSource:  credentialprovider.AuthenticationSourcePod,
					volumecontext.CSIPodNamespace:       "test-ns",
					volumecontext.CSIServiceAccountName: "test-sa",
					volumecontext.CSIServiceAccountTokens: serviceAccountTokens(t, tokens{
						"unknown": {
							Token: testWebIdentityToken,
						},
					}),
				},
			},
			"missing service account name": {
				volumeContext: map[string]string{
					volumecontext.AuthenticationSource: credentialprovider.AuthenticationSourcePod,
					volumecontext.CSIPodNamespace:      "test-ns",
					volumecontext.CSIServiceAccountTokens: serviceAccountTokens(t, tokens{
						"sts.amazonaws.com": {
							Token: testWebIdentityToken,
						},
					}),
				},
			},
			"missing pod namespace": {
				volumeContext: map[string]string{
					volumecontext.AuthenticationSource:  credentialprovider.AuthenticationSourcePod,
					volumecontext.CSIServiceAccountName: "test-sa",
					volumecontext.CSIServiceAccountTokens: serviceAccountTokens(t, tokens{
						"sts.amazonaws.com": {
							Token: testWebIdentityToken,
						},
					}),
				},
			},
		} {
			t.Run(name, func(t *testing.T) {
				provider := credentialprovider.New(clientset.CoreV1())
				_, err := provider.Provide(context.Background(), test.volumeContext)
				if err == nil {
					t.Error("it should fail with missing information")
				}
			})
		}
	})
}

//-- Utilities for tests

func setEnvForLongTermCredentials(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", testAccessKeyID)
	t.Setenv("AWS_SECRET_ACCESS_KEY", testSecretAccessKey)
	t.Setenv("AWS_SESSION_TOKEN", testSessionToken)
}

func assertLongTermCredentials(t *testing.T, basepath string) {
	t.Helper()

	awsprofiletest.AssertCredentialsFromAWSProfile(
		t,
		"s3-csi",
		filepath.Join(basepath, "s3-csi-config"),
		filepath.Join(basepath, "s3-csi-credentials"),
		"test-access-key-id",
		"test-secret-access-key",
		"test-session-token",
	)
}

func setEnvForStsWebIdentityCredentials(t *testing.T) {
	t.Helper()

	tokenPath := filepath.Join(t.TempDir(), "token")
	assert.NoError(t, os.WriteFile(tokenPath, []byte(testWebIdentityToken), 0600))

	t.Setenv("AWS_ROLE_ARN", testRoleARN)
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", tokenPath)
}

func assertWebIdentityTokenFile(t *testing.T, basepath string) {
	t.Helper()

	got, err := os.ReadFile(filepath.Join(basepath, "serviceaccount.token"))
	assert.NoError(t, err)
	assert.Equals(t, []byte(testWebIdentityToken), got)
}

type tokens = map[string]struct {
	Token               string `json:"token"`
	ExpirationTimestamp time.Time
}

func serviceAccountTokens(t *testing.T, tokens tokens) string {
	buf, err := json.Marshal(&tokens)
	assert.NoError(t, err)
	return string(buf)
}

func serviceAccount(name, namespace string, annotations map[string]string) *v1.ServiceAccount {
	return &v1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name:        name,
		Namespace:   namespace,
		Annotations: annotations,
	}}
}
