package credentialprovider_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil"
)

// Secrets that must never appear in an error from Provide. Such errors become the NodePublishVolume
// error, which the kubelet shows as a FailedMount event to anyone who can read the workload's events.
const (
	disclosureSTSToken     = "sts-token-must-not-leak"
	disclosureEKSToken     = "eks-token-must-not-leak"
	disclosureAccessKeyID  = "AKIAMUSTNOTLEAK00000"
	disclosureSecretKey    = "secret-access-key-must-not-leak"
	disclosureSessionToken = "session-token-must-not-leak"
)

func assertErrorDiscloses(t *testing.T, err error, secrets ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected Provide to fail")
	}
	t.Logf("Provide failed with: %v", err)
	for _, secret := range secrets {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error discloses %q: %v", secret, err)
		}
	}
}

func podLevelDisclosureCtx(t *testing.T, writePath, tokensJSON string) credentialprovider.ProvideContext {
	t.Helper()
	return credentialprovider.ProvideContext{
		AuthenticationSource: credentialprovider.AuthenticationSourcePod,
		WritePath:            writePath,
		EnvPath:              testEnvPath,
		WorkloadPodID:        testPodID,
		VolumeID:             testVolumeID,
		PodNamespace:         testPodNamespace,
		ServiceAccountName:   testPodServiceAccount,
		ServiceAccountTokens: tokensJSON,
		MountKind:            credentialprovider.MountKindDaemonset,
	}
}

func TestProvideErrorsDoNotDiscloseCredentials(t *testing.T) {
	testutil.CleanRegionEnv(t)
	validTokens := func(t *testing.T) string {
		return serviceAccountTokens(t, tokens{
			serviceAccountTokenAudienceSTS: {Token: disclosureSTSToken},
			serviceAccountTokenAudienceEKS: {Token: disclosureEKSToken},
		})
	}

	t.Run("pod-level: malformed service account tokens", func(t *testing.T) {
		provider := credentialprovider.New(fake.NewSimpleClientset().CoreV1(), dummyRegionProvider)
		// A token where an object is expected, so decoding fails with the secret in its input.
		malformed := `{"` + serviceAccountTokenAudienceSTS + `":"` + disclosureSTSToken + `"}`

		_, _, err := provider.Provide(context.Background(), podLevelDisclosureCtx(t, t.TempDir(), malformed))

		assertErrorDiscloses(t, err, disclosureSTSToken)
	})

	t.Run("pod-level: token for one audience missing", func(t *testing.T) {
		provider := credentialprovider.New(fake.NewSimpleClientset().CoreV1(), dummyRegionProvider)
		onlySTS := serviceAccountTokens(t, tokens{serviceAccountTokenAudienceSTS: {Token: disclosureSTSToken}})

		_, _, err := provider.Provide(context.Background(), podLevelDisclosureCtx(t, t.TempDir(), onlySTS))

		assertErrorDiscloses(t, err, disclosureSTSToken)
	})

	t.Run("pod-level: service account lookup fails", func(t *testing.T) {
		// No service account exists, so the IAM role lookup fails while both tokens are present.
		provider := credentialprovider.New(fake.NewSimpleClientset().CoreV1(), dummyRegionProvider)

		_, _, err := provider.Provide(context.Background(), podLevelDisclosureCtx(t, t.TempDir(), validTokens(t)))

		assertErrorDiscloses(t, err, disclosureSTSToken, disclosureEKSToken)
	})

	t.Run("pod-level: token cannot be written", func(t *testing.T) {
		clientset := fake.NewSimpleClientset(serviceAccount(testPodServiceAccount, testPodNamespace,
			map[string]string{"eks.amazonaws.com/role-arn": testRoleARN}))
		provider := credentialprovider.New(clientset.CoreV1(), dummyRegionProvider)
		missingDir := filepath.Join(t.TempDir(), "missing")
		provideCtx := podLevelDisclosureCtx(t, missingDir, validTokens(t))
		provideCtx.StsRegion = "us-east-1"

		_, _, err := provider.Provide(context.Background(), provideCtx)

		assertErrorDiscloses(t, err, disclosureSTSToken, disclosureEKSToken)
	})

	t.Run("driver-level: long-term credentials cannot be written", func(t *testing.T) {
		t.Setenv("AWS_ACCESS_KEY_ID", disclosureAccessKeyID)
		t.Setenv("AWS_SECRET_ACCESS_KEY", disclosureSecretKey)
		t.Setenv("AWS_SESSION_TOKEN", disclosureSessionToken)
		provider := credentialprovider.New(nil, dummyRegionProvider)

		_, _, err := provider.Provide(context.Background(), credentialprovider.ProvideContext{
			AuthenticationSource: credentialprovider.AuthenticationSourceDriver,
			WritePath:            filepath.Join(t.TempDir(), "missing"),
			EnvPath:              testEnvPath,
			WorkloadPodID:        testPodID,
			VolumeID:             testVolumeID,
			MountKind:            credentialprovider.MountKindDaemonset,
		})

		assertErrorDiscloses(t, err, disclosureAccessKeyID, disclosureSecretKey, disclosureSessionToken)
	})
}
