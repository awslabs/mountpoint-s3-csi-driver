package driver_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path"
	"testing"
	"time"

	"github.com/awslabs/aws-s3-csi-driver/pkg/driver"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestProvidingDriverLevelCredentials(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret-key")
	t.Setenv("AWS_SESSION_TOKEN", "test-session-token")
	t.Setenv("AWS_REGION", "eu-west-1")
	t.Setenv("AWS_DEFAULT_REGION", "eu-north-1")
	t.Setenv("HOST_PLUGIN_DIR", "/test/csi/plugin/dir")
	t.Setenv("AWS_STS_REGIONAL_ENDPOINTS", "regional")
	t.Setenv("AWS_ROLE_ARN", "arn:aws:iam::123456789012:role/Test")

	for _, test := range []struct {
		volumeID      string
		volumeContext map[string]string
	}{
		{
			volumeID:      "test-vol-id",
			volumeContext: map[string]string{"authenticationSource": "driver"},
		},
		{
			volumeID: "test-vol-id",
			// It should default to `driver` if `authenticationSource` is not explicitly set
			volumeContext: map[string]string{},
		},
	} {

		provider := driver.NewCredentialProvider(nil, "", driver.RegionFromIMDSOnce)
		credentials, err := provider.Provide(context.Background(), test.volumeID, test.volumeContext, nil)
		assertEquals(t, nil, err)

		assertEquals(t, credentials.AccessKeyID, "test-access-key")
		assertEquals(t, credentials.SecretAccessKey, "test-secret-key")
		assertEquals(t, credentials.SessionToken, "test-session-token")
		assertEquals(t, credentials.Region, "eu-west-1")
		assertEquals(t, credentials.DefaultRegion, "eu-north-1")
		assertEquals(t, credentials.WebTokenPath, "/test/csi/plugin/dir/token")
		assertEquals(t, credentials.StsEndpoints, "regional")
		assertEquals(t, credentials.AwsRoleArn, "arn:aws:iam::123456789012:role/Test")
	}
}

func TestProvidingDriverLevelCredentialsWithEmptyEnv(t *testing.T) {
	provider := driver.NewCredentialProvider(nil, "", driver.RegionFromIMDSOnce)
	credentials, err := provider.Provide(context.Background(), "test-vol-id", map[string]string{"authenticationSource": "driver"}, nil)
	assertEquals(t, nil, err)

	assertEquals(t, credentials.AccessKeyID, "")
	assertEquals(t, credentials.SecretAccessKey, "")
	assertEquals(t, credentials.SessionToken, "")
	assertEquals(t, credentials.Region, "")
	assertEquals(t, credentials.DefaultRegion, "")
	assertEquals(t, credentials.WebTokenPath, "/var/lib/kubelet/plugins/s3.csi.aws.com/token")
	assertEquals(t, credentials.StsEndpoints, "")
	assertEquals(t, credentials.AwsRoleArn, "")
}

func TestProvidingPodLevelCredentials(t *testing.T) {
	pluginDir := t.TempDir()
	clientset := fake.NewSimpleClientset(serviceAccount("test-sa", "test-ns", map[string]string{
		"eks.amazonaws.com/role-arn": "arn:aws:iam::123456789012:role/Test",
	}))
	t.Setenv("AWS_REGION", "eu-west-1")
	t.Setenv("AWS_DEFAULT_REGION", "eu-north-1")
	t.Setenv("HOST_PLUGIN_DIR", "/test/csi/plugin/dir")
	t.Setenv("AWS_STS_REGIONAL_ENDPOINTS", "regional")

	provider := driver.NewCredentialProvider(clientset.CoreV1(), pluginDir, driver.RegionFromIMDSOnce)

	credentials, err := provider.Provide(context.Background(), "test-vol-id", map[string]string{
		"authenticationSource":                   "pod",
		"csi.storage.k8s.io/pod.uid":             "test-pod",
		"csi.storage.k8s.io/pod.namespace":       "test-ns",
		"csi.storage.k8s.io/serviceAccount.name": "test-sa",
		"csi.storage.k8s.io/serviceAccount.tokens": serviceAccountTokens(t, tokens{
			"sts.amazonaws.com": {
				Token: "test-service-account-token",
			},
		}),
	}, nil)
	assertEquals(t, nil, err)

	assertEquals(t, credentials.AccessKeyID, "")
	assertEquals(t, credentials.SecretAccessKey, "")
	assertEquals(t, credentials.SessionToken, "")
	assertEquals(t, credentials.Region, "eu-west-1")
	assertEquals(t, credentials.DefaultRegion, "eu-north-1")
	assertEquals(t, credentials.WebTokenPath, "/test/csi/plugin/dir/test-pod-test-vol-id.token")
	assertEquals(t, credentials.StsEndpoints, "regional")
	assertEquals(t, credentials.AwsRoleArn, "arn:aws:iam::123456789012:role/Test")

	token, err := os.ReadFile(tokenFilePath(credentials, pluginDir))
	assertEquals(t, nil, err)
	assertEquals(t, "test-service-account-token", string(token))
}

func TestProvidingPodLevelCredentialsWithMissingInformation(t *testing.T) {
	pluginDir := t.TempDir()
	clientset := fake.NewSimpleClientset(
		serviceAccount("test-sa", "test-ns", map[string]string{
			"eks.amazonaws.com/role-arn": "arn:aws:iam::123456789012:role/Test",
		}),
		serviceAccount("test-sa-missing-role", "test-ns", map[string]string{}),
	)

	provider := driver.NewCredentialProvider(clientset.CoreV1(), pluginDir, driver.RegionFromIMDSOnce)

	for name, test := range map[string]struct {
		volumeID      string
		volumeContext map[string]string
	}{
		"unknown service account": {
			volumeID: "test-vol-id",
			volumeContext: map[string]string{
				"authenticationSource":                   "pod",
				"csi.storage.k8s.io/pod.uid":             "test-pod",
				"csi.storage.k8s.io/pod.namespace":       "test-ns",
				"csi.storage.k8s.io/serviceAccount.name": "test-unknown-sa",
				"csi.storage.k8s.io/serviceAccount.tokens": serviceAccountTokens(t, tokens{
					"sts.amazonaws.com": {
						Token: "test-service-account-token",
					},
				}),
			},
		},
		"missing service account token": {
			volumeID: "test-vol-id",
			volumeContext: map[string]string{
				"authenticationSource":                   "pod",
				"csi.storage.k8s.io/pod.uid":             "test-pod",
				"csi.storage.k8s.io/pod.namespace":       "test-ns",
				"csi.storage.k8s.io/serviceAccount.name": "test-sa",
			},
		},
		"missing sts audience in service account tokens": {
			volumeID: "test-vol-id",
			volumeContext: map[string]string{
				"authenticationSource":                   "pod",
				"csi.storage.k8s.io/pod.uid":             "test-pod",
				"csi.storage.k8s.io/pod.namespace":       "test-ns",
				"csi.storage.k8s.io/serviceAccount.name": "test-sa",
				"csi.storage.k8s.io/serviceAccount.tokens": serviceAccountTokens(t, tokens{
					"unknown": {
						Token: "test-service-account-token",
					},
				}),
			},
		},
		"missing service account name": {
			volumeID: "test-vol-id",
			volumeContext: map[string]string{
				"authenticationSource":             "pod",
				"csi.storage.k8s.io/pod.uid":       "test-pod",
				"csi.storage.k8s.io/pod.namespace": "test-ns",
				"csi.storage.k8s.io/serviceAccount.tokens": serviceAccountTokens(t, tokens{
					"sts.amazonaws.com": {
						Token: "test-service-account-token",
					},
				}),
			},
		},
		"missing pod namespace": {
			volumeID: "test-vol-id",
			volumeContext: map[string]string{
				"authenticationSource":                   "pod",
				"csi.storage.k8s.io/pod.uid":             "test-pod",
				"csi.storage.k8s.io/serviceAccount.name": "test-sa",
				"csi.storage.k8s.io/serviceAccount.tokens": serviceAccountTokens(t, tokens{
					"sts.amazonaws.com": {
						Token: "test-service-account-token",
					},
				}),
			},
		},
		"missing pod id": {
			volumeID: "test-vol-id",
			volumeContext: map[string]string{
				"authenticationSource":                   "pod",
				"csi.storage.k8s.io/pod.namespace":       "test-ns",
				"csi.storage.k8s.io/serviceAccount.name": "test-sa",
				"csi.storage.k8s.io/serviceAccount.tokens": serviceAccountTokens(t, tokens{
					"sts.amazonaws.com": {
						Token: "test-service-account-token",
					},
				}),
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			credentials, err := provider.Provide(context.Background(), test.volumeID, test.volumeContext, nil)
			assertEquals(t, nil, credentials)
			if err == nil {
				t.Error("it should fail with missing information")
			}

			_, err = os.ReadFile(path.Join(pluginDir, "test-pod-test-vol-id.token"))
			assertEquals(t, true, os.IsNotExist(err))
		})
	}
}

func TestProvidingPodLevelCredentialsRegionPopulation(t *testing.T) {
	clientset := fake.NewSimpleClientset(serviceAccount("test-sa", "test-ns", map[string]string{
		"eks.amazonaws.com/role-arn": "arn:aws:iam::123456789012:role/Test",
	}))

	volumeID := "test-vol-id"
	volumeContext := map[string]string{
		"authenticationSource":                   "pod",
		"csi.storage.k8s.io/pod.uid":             "test-pod",
		"csi.storage.k8s.io/pod.namespace":       "test-ns",
		"csi.storage.k8s.io/serviceAccount.name": "test-sa",
		"csi.storage.k8s.io/serviceAccount.tokens": serviceAccountTokens(t, tokens{
			"sts.amazonaws.com": {
				Token: "test-service-account-token",
			},
		}),
	}

	t.Run("no region", func(t *testing.T) {
		pluginDir := t.TempDir()
		provider := driver.NewCredentialProvider(clientset.CoreV1(), pluginDir, func() (string, error) {
			return "", errors.New("unknown region")
		})

		credentials, err := provider.Provide(context.Background(), volumeID, volumeContext, nil)
		assertEquals(t, nil, credentials)
		if err == nil {
			t.Error("it should fail if there is not any region information")
		}

		_, err = os.ReadFile(path.Join(pluginDir, "test-pod-test-vol-id.token"))
		assertEquals(t, true, os.IsNotExist(err))
	})

	t.Run("region from imds", func(t *testing.T) {
		pluginDir := t.TempDir()
		provider := driver.NewCredentialProvider(clientset.CoreV1(), pluginDir, func() (string, error) {
			return "us-east-1", nil
		})

		credentials, err := provider.Provide(context.Background(), volumeID, volumeContext, nil)
		assertEquals(t, nil, err)
		assertEquals(t, credentials.Region, "us-east-1")
		assertEquals(t, credentials.DefaultRegion, "us-east-1")

		token, err := os.ReadFile(tokenFilePath(credentials, pluginDir))
		assertEquals(t, nil, err)
		assertEquals(t, "test-service-account-token", string(token))
	})

	t.Run("region from env", func(t *testing.T) {
		pluginDir := t.TempDir()
		provider := driver.NewCredentialProvider(clientset.CoreV1(), pluginDir, func() (string, error) {
			return "us-east-1", nil
		})

		t.Setenv("AWS_REGION", "eu-west-1")

		credentials, err := provider.Provide(context.Background(), volumeID, volumeContext, nil)
		assertEquals(t, nil, err)
		assertEquals(t, credentials.Region, "eu-west-1")
		assertEquals(t, credentials.DefaultRegion, "eu-west-1")

		token, err := os.ReadFile(tokenFilePath(credentials, pluginDir))
		assertEquals(t, nil, err)
		assertEquals(t, "test-service-account-token", string(token))
	})

	t.Run("default region from env", func(t *testing.T) {
		pluginDir := t.TempDir()
		provider := driver.NewCredentialProvider(clientset.CoreV1(), pluginDir, func() (string, error) {
			return "us-east-1", nil
		})

		t.Setenv("AWS_DEFAULT_REGION", "eu-west-1")

		credentials, err := provider.Provide(context.Background(), volumeID, volumeContext, nil)
		assertEquals(t, nil, err)
		assertEquals(t, credentials.Region, "eu-west-1")
		assertEquals(t, credentials.DefaultRegion, "eu-west-1")

		token, err := os.ReadFile(tokenFilePath(credentials, pluginDir))
		assertEquals(t, nil, err)
		assertEquals(t, "test-service-account-token", string(token))
	})

	t.Run("default and regular region from env", func(t *testing.T) {
		pluginDir := t.TempDir()
		provider := driver.NewCredentialProvider(clientset.CoreV1(), pluginDir, func() (string, error) {
			return "us-east-1", nil
		})

		t.Setenv("AWS_REGION", "eu-west-1")
		t.Setenv("AWS_DEFAULT_REGION", "eu-north-1")

		credentials, err := provider.Provide(context.Background(), volumeID, volumeContext, nil)
		assertEquals(t, nil, err)
		assertEquals(t, credentials.Region, "eu-west-1")
		assertEquals(t, credentials.DefaultRegion, "eu-north-1")

		token, err := os.ReadFile(tokenFilePath(credentials, pluginDir))
		assertEquals(t, nil, err)
		assertEquals(t, "test-service-account-token", string(token))
	})

	t.Run("region from mountpoint options", func(t *testing.T) {
		pluginDir := t.TempDir()
		provider := driver.NewCredentialProvider(clientset.CoreV1(), pluginDir, func() (string, error) {
			return "us-east-1", nil
		})

		t.Setenv("AWS_REGION", "eu-west-1")

		credentials, err := provider.Provide(context.Background(), volumeID, volumeContext, []string{"--region=us-west-1"})
		assertEquals(t, nil, err)
		assertEquals(t, credentials.Region, "us-west-1")
		assertEquals(t, credentials.DefaultRegion, "us-west-1")

		token, err := os.ReadFile(tokenFilePath(credentials, pluginDir))
		assertEquals(t, nil, err)
		assertEquals(t, "test-service-account-token", string(token))
	})

	t.Run("missing region from mountpoint options", func(t *testing.T) {
		pluginDir := t.TempDir()
		provider := driver.NewCredentialProvider(clientset.CoreV1(), pluginDir, func() (string, error) {
			return "us-east-1", nil
		})

		t.Setenv("AWS_REGION", "eu-west-1")

		credentials, err := provider.Provide(context.Background(), volumeID, volumeContext, []string{"--read-only"})
		assertEquals(t, nil, err)
		assertEquals(t, credentials.Region, "eu-west-1")
		assertEquals(t, credentials.DefaultRegion, "eu-west-1")

		token, err := os.ReadFile(tokenFilePath(credentials, pluginDir))
		assertEquals(t, nil, err)
		assertEquals(t, "test-service-account-token", string(token))
	})

	t.Run("region from mountpoint options with default region from env", func(t *testing.T) {
		pluginDir := t.TempDir()
		provider := driver.NewCredentialProvider(clientset.CoreV1(), pluginDir, func() (string, error) {
			return "us-east-1", nil
		})

		t.Setenv("AWS_REGION", "eu-west-1")
		t.Setenv("AWS_DEFAULT_REGION", "eu-north-1")

		credentials, err := provider.Provide(context.Background(), volumeID, volumeContext, []string{"--region=us-west-1"})
		assertEquals(t, nil, err)
		assertEquals(t, credentials.Region, "us-west-1")
		assertEquals(t, credentials.DefaultRegion, "eu-north-1")

		token, err := os.ReadFile(tokenFilePath(credentials, pluginDir))
		assertEquals(t, nil, err)
		assertEquals(t, "test-service-account-token", string(token))
	})

	t.Run("region from volume context", func(t *testing.T) {
		pluginDir := t.TempDir()
		provider := driver.NewCredentialProvider(clientset.CoreV1(), pluginDir, func() (string, error) {
			return "us-east-1", nil
		})

		t.Setenv("AWS_REGION", "eu-west-1")

		volumeContext["stsRegion"] = "ap-south-1"

		credentials, err := provider.Provide(context.Background(), volumeID, volumeContext, []string{"--region=us-west-1"})
		assertEquals(t, nil, err)
		assertEquals(t, credentials.Region, "ap-south-1")
		assertEquals(t, credentials.DefaultRegion, "ap-south-1")

		token, err := os.ReadFile(tokenFilePath(credentials, pluginDir))
		assertEquals(t, nil, err)
		assertEquals(t, "test-service-account-token", string(token))
	})

	t.Run("region from volume context with default region from env", func(t *testing.T) {
		pluginDir := t.TempDir()
		provider := driver.NewCredentialProvider(clientset.CoreV1(), pluginDir, func() (string, error) {
			return "us-east-1", nil
		})

		t.Setenv("AWS_REGION", "eu-west-1")
		t.Setenv("AWS_DEFAULT_REGION", "eu-north-1")

		volumeContext["stsRegion"] = "ap-south-1"

		credentials, err := provider.Provide(context.Background(), volumeID, volumeContext, []string{"--region=us-west-1"})
		assertEquals(t, nil, err)
		assertEquals(t, credentials.Region, "ap-south-1")
		assertEquals(t, credentials.DefaultRegion, "eu-north-1")

		token, err := os.ReadFile(tokenFilePath(credentials, pluginDir))
		assertEquals(t, nil, err)
		assertEquals(t, "test-service-account-token", string(token))
	})
}

func TestProvidingPodLevelCredentialsForDifferentPodsWithDifferentRoles(t *testing.T) {
	pluginDir := t.TempDir()
	clientset := fake.NewSimpleClientset(
		serviceAccount("test-sa-1", "test-ns", map[string]string{
			"eks.amazonaws.com/role-arn": "arn:aws:iam::123456789012:role/Test1",
		}),
		serviceAccount("test-sa-2", "test-ns", map[string]string{
			"eks.amazonaws.com/role-arn": "arn:aws:iam::123456789012:role/Test2",
		}),
	)
	t.Setenv("AWS_REGION", "eu-west-1")
	t.Setenv("AWS_DEFAULT_REGION", "eu-north-1")
	t.Setenv("HOST_PLUGIN_DIR", "/test/csi/plugin/dir")
	t.Setenv("AWS_STS_REGIONAL_ENDPOINTS", "regional")

	provider := driver.NewCredentialProvider(clientset.CoreV1(), pluginDir, driver.RegionFromIMDSOnce)

	credentialsPodOne, err := provider.Provide(context.Background(), "test-vol-id", map[string]string{
		"authenticationSource":                   "pod",
		"csi.storage.k8s.io/pod.uid":             "test-pod-1",
		"csi.storage.k8s.io/pod.namespace":       "test-ns",
		"csi.storage.k8s.io/serviceAccount.name": "test-sa-1",
		"csi.storage.k8s.io/serviceAccount.tokens": serviceAccountTokens(t, tokens{
			"sts.amazonaws.com": {
				Token: "test-service-account-token-1",
			},
		}),
	}, nil)
	assertEquals(t, nil, err)

	credentialsPodTwo, err := provider.Provide(context.Background(), "test-vol-id", map[string]string{
		"authenticationSource":                   "pod",
		"csi.storage.k8s.io/pod.uid":             "test-pod-2",
		"csi.storage.k8s.io/pod.namespace":       "test-ns",
		"csi.storage.k8s.io/serviceAccount.name": "test-sa-2",
		"csi.storage.k8s.io/serviceAccount.tokens": serviceAccountTokens(t, tokens{
			"sts.amazonaws.com": {
				Token: "test-service-account-token-2",
			},
		}),
	}, nil)
	assertEquals(t, nil, err)

	// PodOne
	assertEquals(t, credentialsPodOne.AccessKeyID, "")
	assertEquals(t, credentialsPodOne.SecretAccessKey, "")
	assertEquals(t, credentialsPodOne.SessionToken, "")
	assertEquals(t, credentialsPodOne.Region, "eu-west-1")
	assertEquals(t, credentialsPodOne.DefaultRegion, "eu-north-1")
	assertEquals(t, credentialsPodOne.WebTokenPath, "/test/csi/plugin/dir/test-pod-1-test-vol-id.token")
	assertEquals(t, credentialsPodOne.StsEndpoints, "regional")
	assertEquals(t, credentialsPodOne.AwsRoleArn, "arn:aws:iam::123456789012:role/Test1")

	token, err := os.ReadFile(tokenFilePath(credentialsPodOne, pluginDir))
	assertEquals(t, nil, err)
	assertEquals(t, "test-service-account-token-1", string(token))

	// PodTwo
	assertEquals(t, credentialsPodTwo.AccessKeyID, "")
	assertEquals(t, credentialsPodTwo.SecretAccessKey, "")
	assertEquals(t, credentialsPodTwo.SessionToken, "")
	assertEquals(t, credentialsPodTwo.Region, "eu-west-1")
	assertEquals(t, credentialsPodTwo.DefaultRegion, "eu-north-1")
	assertEquals(t, credentialsPodTwo.WebTokenPath, "/test/csi/plugin/dir/test-pod-2-test-vol-id.token")
	assertEquals(t, credentialsPodTwo.StsEndpoints, "regional")
	assertEquals(t, credentialsPodTwo.AwsRoleArn, "arn:aws:iam::123456789012:role/Test2")

	token, err = os.ReadFile(tokenFilePath(credentialsPodTwo, pluginDir))
	assertEquals(t, nil, err)
	assertEquals(t, "test-service-account-token-2", string(token))
}

func TestProvidingPodLevelCredentialsWithSlashInVolumeID(t *testing.T) {
	pluginDir := t.TempDir()
	clientset := fake.NewSimpleClientset(serviceAccount("test-sa", "test-ns", map[string]string{
		"eks.amazonaws.com/role-arn": "arn:aws:iam::123456789012:role/Test",
	}))
	t.Setenv("AWS_REGION", "eu-west-1")
	t.Setenv("AWS_DEFAULT_REGION", "eu-north-1")
	t.Setenv("HOST_PLUGIN_DIR", "/test/csi/plugin/dir")
	t.Setenv("AWS_STS_REGIONAL_ENDPOINTS", "regional")

	provider := driver.NewCredentialProvider(clientset.CoreV1(), pluginDir, driver.RegionFromIMDSOnce)

	credentials, err := provider.Provide(context.Background(), "test-vol-id/1", map[string]string{
		"authenticationSource":                   "pod",
		"csi.storage.k8s.io/pod.uid":             "test-pod",
		"csi.storage.k8s.io/pod.namespace":       "test-ns",
		"csi.storage.k8s.io/serviceAccount.name": "test-sa",
		"csi.storage.k8s.io/serviceAccount.tokens": serviceAccountTokens(t, tokens{
			"sts.amazonaws.com": {
				Token: "test-service-account-token",
			},
		}),
	}, nil)
	assertEquals(t, nil, err)

	assertEquals(t, credentials.AccessKeyID, "")
	assertEquals(t, credentials.SecretAccessKey, "")
	assertEquals(t, credentials.SessionToken, "")
	assertEquals(t, credentials.Region, "eu-west-1")
	assertEquals(t, credentials.DefaultRegion, "eu-north-1")
	assertEquals(t, credentials.WebTokenPath, "/test/csi/plugin/dir/test-pod-test-vol-id~1.token")
	assertEquals(t, credentials.StsEndpoints, "regional")
	assertEquals(t, credentials.AwsRoleArn, "arn:aws:iam::123456789012:role/Test")

	token, err := os.ReadFile(tokenFilePath(credentials, pluginDir))
	assertEquals(t, nil, err)
	assertEquals(t, "test-service-account-token", string(token))
}

func TestCleaningUpTokenFileForAVolume(t *testing.T) {
	t.Run("existing token", func(t *testing.T) {
		pluginDir := t.TempDir()
		volumeID := "test-vol-id"
		podID := "test-pod-id"
		tokenPath := path.Join(pluginDir, podID+"-"+volumeID+".token")
		err := os.WriteFile(tokenPath, []byte("test-service-account-token"), 0400)
		assertEquals(t, nil, err)

		provider := driver.NewCredentialProvider(nil, pluginDir, driver.RegionFromIMDSOnce)
		err = provider.CleanupToken(volumeID, podID)
		assertEquals(t, nil, err)

		_, err = os.ReadFile(tokenPath)
		assertEquals(t, true, os.IsNotExist(err))
	})

	t.Run("non-existing token", func(t *testing.T) {
		provider := driver.NewCredentialProvider(nil, t.TempDir(), driver.RegionFromIMDSOnce)

		err := provider.CleanupToken("non-existing-vol-id", "non-existing-pod-id")
		assertEquals(t, nil, err)
	})
}

type tokens = map[string]struct {
	Token               string `json:"token"`
	ExpirationTimestamp time.Time
}

func serviceAccountTokens(t *testing.T, tokens tokens) string {
	buf, err := json.Marshal(&tokens)
	assertEquals(t, nil, err)
	return string(buf)
}

func serviceAccount(name, namespace string, annotations map[string]string) *v1.ServiceAccount {
	return &v1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name:        name,
		Namespace:   namespace,
		Annotations: annotations,
	}}
}

func tokenFilePath(credentials *driver.MountCredentials, pluginDir string) string {
	return path.Join(pluginDir, path.Base(credentials.WebTokenPath))
}

func assertEquals[T comparable](t *testing.T, expected T, got T) {
	if expected != got {
		t.Errorf("Expected %#v, Got %#v", expected, got)
	}
}
