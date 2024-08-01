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
	"github.com/container-storage-interface/spec/lib/go/csi"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func init() {
	driver.RegionFromIMDS = func() (string, error) {
		return "us-east-1", nil
	}
}

func TestProvidingDriverLevelCredentials(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret-key")
	t.Setenv("AWS_SESSION_TOKEN", "test-session-token")
	t.Setenv("AWS_REGION", "eu-west-1")
	t.Setenv("AWS_DEFAULT_REGION", "eu-north-1")
	t.Setenv("HOST_PLUGIN_DIR", "/test/csi/plugin/dir")
	t.Setenv("AWS_STS_REGIONAL_ENDPOINTS", "regional")
	t.Setenv("AWS_ROLE_ARN", "arn:aws:iam::123456789012:role/Test")

	for _, req := range []*csi.NodePublishVolumeRequest{
		{
			VolumeContext: map[string]string{"authenticationSource": "driver"},
		},
		{
			// It should default to `driver` if `authenticationSource` is not explicitly set
			VolumeContext: map[string]string{},
		},
	} {

		provider := driver.NewCredentialProvider(nil, "")
		credentials, err := provider.Provide(context.Background(), req, nil)
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
	provider := driver.NewCredentialProvider(nil, "")
	credentials, err := provider.Provide(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeContext: map[string]string{"authenticationSource": "driver"},
	}, nil)
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

	provider := driver.NewCredentialProvider(clientset.CoreV1(), pluginDir)

	credentials, err := provider.Provide(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId: "test-vol-id",
		VolumeContext: map[string]string{
			"authenticationSource":                   "pod",
			"csi.storage.k8s.io/pod.namespace":       "test-ns",
			"csi.storage.k8s.io/serviceAccount.name": "test-sa",
			"csi.storage.k8s.io/serviceAccount.tokens": serviceAccountTokens(t, tokens{
				"sts.amazonaws.com": {
					Token: "test-service-account-token",
				},
			}),
		},
	}, nil)
	assertEquals(t, nil, err)

	tokenPath := path.Join(pluginDir, "test-vol-id.token")

	assertEquals(t, credentials.AccessKeyID, "")
	assertEquals(t, credentials.SecretAccessKey, "")
	assertEquals(t, credentials.SessionToken, "")
	assertEquals(t, credentials.Region, "eu-west-1")
	assertEquals(t, credentials.DefaultRegion, "eu-north-1")
	assertEquals(t, credentials.WebTokenPath, "/test/csi/plugin/dir/test-vol-id.token")
	assertEquals(t, credentials.StsEndpoints, "regional")
	assertEquals(t, credentials.AwsRoleArn, "arn:aws:iam::123456789012:role/Test")

	token, err := os.ReadFile(tokenPath)
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

	provider := driver.NewCredentialProvider(clientset.CoreV1(), pluginDir)

	for name, test := range map[string]struct {
		input *csi.NodePublishVolumeRequest
	}{
		"unknown service account": {
			input: &csi.NodePublishVolumeRequest{
				VolumeId: "test-vol-id",
				VolumeContext: map[string]string{
					"authenticationSource":                   "pod",
					"csi.storage.k8s.io/pod.namespace":       "test-ns",
					"csi.storage.k8s.io/serviceAccount.name": "test-unknown-sa",
					"csi.storage.k8s.io/serviceAccount.tokens": serviceAccountTokens(t, tokens{
						"sts.amazonaws.com": {
							Token: "test-service-account-token",
						},
					}),
				},
			},
		},
		"missing service account token": {
			input: &csi.NodePublishVolumeRequest{
				VolumeId: "test-vol-id",
				VolumeContext: map[string]string{
					"authenticationSource":                   "pod",
					"csi.storage.k8s.io/pod.namespace":       "test-ns",
					"csi.storage.k8s.io/serviceAccount.name": "test-sa",
				},
			},
		},
		"missing sts audience in service account tokens": {
			input: &csi.NodePublishVolumeRequest{
				VolumeId: "test-vol-id",
				VolumeContext: map[string]string{
					"authenticationSource":                   "pod",
					"csi.storage.k8s.io/pod.namespace":       "test-ns",
					"csi.storage.k8s.io/serviceAccount.name": "test-sa",
					"csi.storage.k8s.io/serviceAccount.tokens": serviceAccountTokens(t, tokens{
						"unknown": {
							Token: "test-service-account-token",
						},
					}),
				},
			},
		},
		"missing service account name": {
			input: &csi.NodePublishVolumeRequest{
				VolumeId: "test-vol-id",
				VolumeContext: map[string]string{
					"authenticationSource":             "pod",
					"csi.storage.k8s.io/pod.namespace": "test-ns",
					"csi.storage.k8s.io/serviceAccount.tokens": serviceAccountTokens(t, tokens{
						"sts.amazonaws.com": {
							Token: "test-service-account-token",
						},
					}),
				},
			},
		},
		"missing pod namespace": {
			input: &csi.NodePublishVolumeRequest{
				VolumeId: "test-vol-id",
				VolumeContext: map[string]string{
					"authenticationSource":                   "pod",
					"csi.storage.k8s.io/serviceAccount.name": "test-sa",
					"csi.storage.k8s.io/serviceAccount.tokens": serviceAccountTokens(t, tokens{
						"sts.amazonaws.com": {
							Token: "test-service-account-token",
						},
					}),
				},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			credentials, err := provider.Provide(context.Background(), test.input, nil)
			assertEquals(t, nil, credentials)
			if err == nil {
				t.Error("it should fail with missing information")
			}

			_, err = os.ReadFile(path.Join(pluginDir, "test-vol-id.token"))
			assertEquals(t, true, os.IsNotExist(err))
		})
	}
}

func TestProvidingPodLevelCredentialsRegionPopulation(t *testing.T) {
	clientset := fake.NewSimpleClientset(serviceAccount("test-sa", "test-ns", map[string]string{
		"eks.amazonaws.com/role-arn": "arn:aws:iam::123456789012:role/Test",
	}))

	nodePublishvolumeReq := csi.NodePublishVolumeRequest{
		VolumeId: "test-vol-id",
		VolumeContext: map[string]string{
			"authenticationSource":                   "pod",
			"csi.storage.k8s.io/pod.namespace":       "test-ns",
			"csi.storage.k8s.io/serviceAccount.name": "test-sa",
			"csi.storage.k8s.io/serviceAccount.tokens": serviceAccountTokens(t, tokens{
				"sts.amazonaws.com": {
					Token: "test-service-account-token",
				},
			}),
		},
	}

	originalRegionFromIMDS := driver.RegionFromIMDS
	defer func() {
		driver.RegionFromIMDS = originalRegionFromIMDS
	}()

	t.Run("no region", func(t *testing.T) {
		pluginDir := t.TempDir()
		provider := driver.NewCredentialProvider(clientset.CoreV1(), pluginDir)

		driver.RegionFromIMDS = func() (string, error) {
			return "", errors.New("unknown region")
		}

		credentials, err := provider.Provide(context.Background(), &nodePublishvolumeReq, nil)
		assertEquals(t, nil, credentials)
		if err == nil {
			t.Error("it should fail if there is not any region information")
		}

		_, err = os.ReadFile(path.Join(pluginDir, "test-vol-id.token"))
		assertEquals(t, true, os.IsNotExist(err))
	})

	t.Run("region from imds", func(t *testing.T) {
		pluginDir := t.TempDir()
		provider := driver.NewCredentialProvider(clientset.CoreV1(), pluginDir)

		driver.RegionFromIMDS = func() (string, error) {
			return "us-east-1", nil
		}

		credentials, err := provider.Provide(context.Background(), &nodePublishvolumeReq, nil)
		assertEquals(t, nil, err)
		assertEquals(t, credentials.Region, "us-east-1")
		assertEquals(t, credentials.DefaultRegion, "us-east-1")

		token, err := os.ReadFile(path.Join(pluginDir, "test-vol-id.token"))
		assertEquals(t, nil, err)
		assertEquals(t, "test-service-account-token", string(token))
	})

	t.Run("region from env", func(t *testing.T) {
		pluginDir := t.TempDir()
		provider := driver.NewCredentialProvider(clientset.CoreV1(), pluginDir)

		driver.RegionFromIMDS = func() (string, error) {
			return "us-east-1", nil
		}

		t.Setenv("AWS_REGION", "eu-west-1")

		credentials, err := provider.Provide(context.Background(), &nodePublishvolumeReq, nil)
		assertEquals(t, nil, err)
		assertEquals(t, credentials.Region, "eu-west-1")
		assertEquals(t, credentials.DefaultRegion, "eu-west-1")

		token, err := os.ReadFile(path.Join(pluginDir, "test-vol-id.token"))
		assertEquals(t, nil, err)
		assertEquals(t, "test-service-account-token", string(token))
	})

	t.Run("default region from env", func(t *testing.T) {
		pluginDir := t.TempDir()
		provider := driver.NewCredentialProvider(clientset.CoreV1(), pluginDir)

		driver.RegionFromIMDS = func() (string, error) {
			return "us-east-1", nil
		}

		t.Setenv("AWS_DEFAULT_REGION", "eu-west-1")

		credentials, err := provider.Provide(context.Background(), &nodePublishvolumeReq, nil)
		assertEquals(t, nil, err)
		assertEquals(t, credentials.Region, "eu-west-1")
		assertEquals(t, credentials.DefaultRegion, "eu-west-1")

		token, err := os.ReadFile(path.Join(pluginDir, "test-vol-id.token"))
		assertEquals(t, nil, err)
		assertEquals(t, "test-service-account-token", string(token))
	})

	t.Run("default and regular region from env", func(t *testing.T) {
		pluginDir := t.TempDir()
		provider := driver.NewCredentialProvider(clientset.CoreV1(), pluginDir)

		driver.RegionFromIMDS = func() (string, error) {
			return "us-east-1", nil
		}

		t.Setenv("AWS_REGION", "eu-west-1")
		t.Setenv("AWS_DEFAULT_REGION", "eu-north-1")

		credentials, err := provider.Provide(context.Background(), &nodePublishvolumeReq, nil)
		assertEquals(t, nil, err)
		assertEquals(t, credentials.Region, "eu-west-1")
		assertEquals(t, credentials.DefaultRegion, "eu-north-1")

		token, err := os.ReadFile(path.Join(pluginDir, "test-vol-id.token"))
		assertEquals(t, nil, err)
		assertEquals(t, "test-service-account-token", string(token))
	})

	t.Run("region from mountpoint options", func(t *testing.T) {
		pluginDir := t.TempDir()
		provider := driver.NewCredentialProvider(clientset.CoreV1(), pluginDir)

		driver.RegionFromIMDS = func() (string, error) {
			return "us-east-1", nil
		}

		t.Setenv("AWS_REGION", "eu-west-1")

		credentials, err := provider.Provide(context.Background(), &nodePublishvolumeReq, []string{"--region=us-west-1"})
		assertEquals(t, nil, err)
		assertEquals(t, credentials.Region, "us-west-1")
		assertEquals(t, credentials.DefaultRegion, "us-west-1")

		token, err := os.ReadFile(path.Join(pluginDir, "test-vol-id.token"))
		assertEquals(t, nil, err)
		assertEquals(t, "test-service-account-token", string(token))
	})

	t.Run("missing region from mountpoint options", func(t *testing.T) {
		pluginDir := t.TempDir()
		provider := driver.NewCredentialProvider(clientset.CoreV1(), pluginDir)

		driver.RegionFromIMDS = func() (string, error) {
			return "us-east-1", nil
		}

		t.Setenv("AWS_REGION", "eu-west-1")

		credentials, err := provider.Provide(context.Background(), &nodePublishvolumeReq, []string{"--read-only"})
		assertEquals(t, nil, err)
		assertEquals(t, credentials.Region, "eu-west-1")
		assertEquals(t, credentials.DefaultRegion, "eu-west-1")

		token, err := os.ReadFile(path.Join(pluginDir, "test-vol-id.token"))
		assertEquals(t, nil, err)
		assertEquals(t, "test-service-account-token", string(token))
	})

	t.Run("region from mountpoint options with default region from env", func(t *testing.T) {
		pluginDir := t.TempDir()
		provider := driver.NewCredentialProvider(clientset.CoreV1(), pluginDir)

		driver.RegionFromIMDS = func() (string, error) {
			return "us-east-1", nil
		}

		t.Setenv("AWS_REGION", "eu-west-1")
		t.Setenv("AWS_DEFAULT_REGION", "eu-north-1")

		credentials, err := provider.Provide(context.Background(), &nodePublishvolumeReq, []string{"--region=us-west-1"})
		assertEquals(t, nil, err)
		assertEquals(t, credentials.Region, "us-west-1")
		assertEquals(t, credentials.DefaultRegion, "eu-north-1")

		token, err := os.ReadFile(path.Join(pluginDir, "test-vol-id.token"))
		assertEquals(t, nil, err)
		assertEquals(t, "test-service-account-token", string(token))
	})

	t.Run("region from volume context", func(t *testing.T) {
		pluginDir := t.TempDir()
		provider := driver.NewCredentialProvider(clientset.CoreV1(), pluginDir)

		driver.RegionFromIMDS = func() (string, error) {
			return "us-east-1", nil
		}

		t.Setenv("AWS_REGION", "eu-west-1")

		req := nodePublishvolumeReq
		req.VolumeContext["stsRegion"] = "ap-south-1"

		credentials, err := provider.Provide(context.Background(), &nodePublishvolumeReq, []string{"--region=us-west-1"})
		assertEquals(t, nil, err)
		assertEquals(t, credentials.Region, "ap-south-1")
		assertEquals(t, credentials.DefaultRegion, "ap-south-1")

		token, err := os.ReadFile(path.Join(pluginDir, "test-vol-id.token"))
		assertEquals(t, nil, err)
		assertEquals(t, "test-service-account-token", string(token))
	})

	t.Run("region from volume context with default region from env", func(t *testing.T) {
		pluginDir := t.TempDir()
		provider := driver.NewCredentialProvider(clientset.CoreV1(), pluginDir)

		driver.RegionFromIMDS = func() (string, error) {
			return "us-east-1", nil
		}

		t.Setenv("AWS_REGION", "eu-west-1")
		t.Setenv("AWS_DEFAULT_REGION", "eu-north-1")

		req := nodePublishvolumeReq
		req.VolumeContext["stsRegion"] = "ap-south-1"

		credentials, err := provider.Provide(context.Background(), &nodePublishvolumeReq, []string{"--region=us-west-1"})
		assertEquals(t, nil, err)
		assertEquals(t, credentials.Region, "ap-south-1")
		assertEquals(t, credentials.DefaultRegion, "eu-north-1")

		token, err := os.ReadFile(path.Join(pluginDir, "test-vol-id.token"))
		assertEquals(t, nil, err)
		assertEquals(t, "test-service-account-token", string(token))
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

func assertEquals[T comparable](t *testing.T, expected T, got T) {
	if expected != got {
		t.Errorf("Expected %#v, Got %#v", expected, got)
	}
}
