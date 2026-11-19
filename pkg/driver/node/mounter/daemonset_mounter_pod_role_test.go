package mounter_test

import (
	"context"
	"sync"
	"testing"

	"github.com/golang/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stesting "k8s.io/client-go/testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider"
	mock_credentialprovider "github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/credentialprovider/mocks"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/envprovider"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/mountpoint"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

const (
	podRoleTestServiceAccount = "workload-sa"
	podRoleTestNamespace      = "default"
	podRoleTestRoleARN        = "arn:aws:iam::111122223333:role/new-role"
)

// A volume's Mountpoint keeps the credential environment it started with, so the role a pod-level
// mount uses must stay the one it was mounted with. Changing the service account's IRSA annotation
// must neither switch a running pod's credential files to the new role nor let a new pod share the
// mount under the old role.
func TestDaemonsetMounter_PodLevelRoleIsFixedAtMount(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mountedRole string
		newRole     string
	}{
		{name: "EKS Pod Identity, then IRSA annotation added", mountedRole: "", newRole: podRoleTestRoleARN},
		{name: "IRSA, then annotation removed", mountedRole: podRoleTestRoleARN, newRole: ""},
		{name: "IRSA, then annotation changed", mountedRole: "arn:aws:iam::111122223333:role/old-role", newRole: podRoleTestRoleARN},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testCtx := setupDM(t)
			roles := testCtx.useRoleRecordingCredProvider()
			testCtx.createServiceAccount(tc.mountedRole)

			targetA := testCtx.targetPath("pod-a-uid")
			testCtx.mountPodLevelAndServe("pod-a-uid")
			assert.Equals(t, []string{tc.mountedRole}, roles.get())

			testCtx.setServiceAccountRole(tc.newRole)

			// A republish of the running pod keeps providing the mounted role, without reading the service account.
			testCtx.client.ClearActions()
			for range 3 {
				err := testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, targetA, testCtx.podLevelProvideCtx("pod-a-uid"),
					nil, mountpoint.ParseArgs(nil), "", nil)
				assert.NoError(t, err)
			}
			assert.Equals(t, []string{tc.mountedRole, tc.mountedRole, tc.mountedRole, tc.mountedRole}, roles.get())
			assert.Equals(t, 0, serviceAccountReads(testCtx.client.Actions()))

			// A new pod with the service account's new role must not share the mount.
			err := testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath("pod-b-uid"), testCtx.podLevelProvideCtx("pod-b-uid"),
				nil, mountpoint.ParseArgs(nil), "", nil)
			if err == nil {
				t.Fatal("expected a pod whose service account role changed to be rejected from the existing mount")
			}
			assert.Contains(t, err.Error(), "serviceAccountEKSRoleARN mismatch")
			assert.Equals(t, 4, len(roles.get()))
		})
	}

	t.Run("A new pod with the unchanged role shares the mount", func(t *testing.T) {
		testCtx := setupDM(t)
		roles := testCtx.useRoleRecordingCredProvider()
		testCtx.createServiceAccount(podRoleTestRoleARN)

		testCtx.mountPodLevelAndServe("pod-a-uid")
		err := testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath("pod-b-uid"), testCtx.podLevelProvideCtx("pod-b-uid"),
			nil, mountpoint.ParseArgs(nil), "", nil)
		assert.NoError(t, err)
		assert.Equals(t, []string{podRoleTestRoleARN, podRoleTestRoleARN}, roles.get())
	})

	t.Run("The mounted role survives a node plugin restart", func(t *testing.T) {
		testCtx := setupDM(t)
		testCtx.useRoleRecordingCredProvider()
		testCtx.createServiceAccount(podRoleTestRoleARN)
		testCtx.mountPodLevelAndServe("pod-a-uid")
		testCtx.setServiceAccountRole("")

		// A new s3-csi-node rebuilds its mount map from the meta files.
		roles := testCtx.useRoleRecordingCredProvider()
		assert.NoError(t, testCtx.dm.RebuildMountMap())

		err := testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath("pod-a-uid"), testCtx.podLevelProvideCtx("pod-a-uid"),
			nil, mountpoint.ParseArgs(nil), "", nil)
		assert.NoError(t, err)
		assert.Equals(t, []string{podRoleTestRoleARN}, roles.get())
	})

	t.Run("A first mount fails while the service account cannot be read, and succeeds once it can", func(t *testing.T) {
		testCtx := setupDM(t)
		roles := testCtx.useRoleRecordingCredProvider()

		err := testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath("pod-a-uid"), testCtx.podLevelProvideCtx("pod-a-uid"),
			nil, mountpoint.ParseArgs(nil), "", nil)
		if err == nil {
			t.Fatal("expected a pod-level mount to fail without its service account")
		}
		assert.Contains(t, err.Error(), "Failed to get pod's service account")
		assert.Equals(t, 0, len(roles.get()))

		testCtx.createServiceAccount(podRoleTestRoleARN)
		testCtx.mountPodLevelAndServe("pod-a-uid")
		assert.Equals(t, []string{podRoleTestRoleARN}, roles.get())
	})

	t.Run("A republish still refreshes credentials when the service account cannot be read", func(t *testing.T) {
		testCtx := setupDM(t)
		roles := testCtx.useRoleRecordingCredProvider()
		testCtx.createServiceAccount(podRoleTestRoleARN)

		testCtx.mountPodLevelAndServe("pod-a-uid")
		err := testCtx.client.CoreV1().ServiceAccounts(podRoleTestNamespace).Delete(testCtx.ctx, podRoleTestServiceAccount, metav1.DeleteOptions{})
		assert.NoError(t, err)

		err = testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath("pod-a-uid"), testCtx.podLevelProvideCtx("pod-a-uid"),
			nil, mountpoint.ParseArgs(nil), "", nil)
		assert.NoError(t, err)
		assert.Equals(t, []string{podRoleTestRoleARN, podRoleTestRoleARN}, roles.get())
	})
}

// serviceAccountReads counts the ServiceAccount reads among the client's recorded actions.
func serviceAccountReads(actions []k8stesting.Action) int {
	n := 0
	for _, action := range actions {
		if action.GetResource().Resource == "serviceaccounts" && action.GetVerb() == "get" {
			n++
		}
	}
	return n
}

// recordedRoles is the role ARN of every Provide call, in order.
type recordedRoles struct {
	mu    sync.Mutex
	roles []string
}

func (r *recordedRoles) get() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.roles...)
}

// useRoleRecordingCredProvider rebuilds the mounter around a provider that records the role ARN of
// every Provide call.
func (testCtx *dmTestCtx) useRoleRecordingCredProvider() *recordedRoles {
	roles := &recordedRoles{}
	provider := mock_credentialprovider.NewMockProviderInterface(gomock.NewController(testCtx.t))
	provider.EXPECT().Provide(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, provideCtx credentialprovider.ProvideContext) (envprovider.Environment, credentialprovider.AuthenticationSource, error) {
			roles.mu.Lock()
			defer roles.mu.Unlock()
			roles.roles = append(roles.roles, provideCtx.ServiceAccountEKSRoleARN)
			return envprovider.Environment{}, credentialprovider.AuthenticationSourcePod, nil
		}).AnyTimes()
	provider.EXPECT().Cleanup(gomock.Any()).Return(nil).AnyTimes()
	testCtx.useCredProvider(provider)
	return roles
}

func (testCtx *dmTestCtx) podLevelProvideCtx(podUID string) credentialprovider.ProvideContext {
	return credentialprovider.ProvideContext{
		WorkloadPodID:        podUID,
		VolumeID:             testCtx.volumeID,
		AuthenticationSource: credentialprovider.AuthenticationSourcePod,
		ServiceAccountName:   podRoleTestServiceAccount,
		PodNamespace:         podRoleTestNamespace,
	}
}

// mountPodLevelAndServe is mountAndServe for a pod-level mount.
func (testCtx *dmTestCtx) mountPodLevelAndServe(podUID string) {
	testCtx.t.Helper()
	mountRes := make(chan error, 1)
	go func() {
		mountRes <- testCtx.dm.Mount(testCtx.ctx, testCtx.bucketName, testCtx.targetPath(podUID), testCtx.podLevelProvideCtx(podUID),
			nil, mountpoint.ParseArgs(nil), "", nil)
	}()
	testCtx.receiveMountOptions()
	assert.NoError(testCtx.t, <-mountRes)
}

func (testCtx *dmTestCtx) createServiceAccount(roleARN string) {
	testCtx.t.Helper()
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name:        podRoleTestServiceAccount,
		Namespace:   podRoleTestNamespace,
		Annotations: roleAnnotation(roleARN),
	}}
	_, err := testCtx.client.CoreV1().ServiceAccounts(podRoleTestNamespace).Create(testCtx.ctx, sa, metav1.CreateOptions{})
	assert.NoError(testCtx.t, err)
}

func (testCtx *dmTestCtx) setServiceAccountRole(roleARN string) {
	testCtx.t.Helper()
	sas := testCtx.client.CoreV1().ServiceAccounts(podRoleTestNamespace)
	sa, err := sas.Get(testCtx.ctx, podRoleTestServiceAccount, metav1.GetOptions{})
	assert.NoError(testCtx.t, err)
	sa.Annotations = roleAnnotation(roleARN)
	_, err = sas.Update(testCtx.ctx, sa, metav1.UpdateOptions{})
	assert.NoError(testCtx.t, err)
}

func roleAnnotation(roleARN string) map[string]string {
	if roleARN == "" {
		return nil
	}
	return map[string]string{"eks.amazonaws.com/role-arn": roleARN}
}
