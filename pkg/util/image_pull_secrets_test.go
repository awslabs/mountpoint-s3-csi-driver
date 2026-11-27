package util

import (
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
	"github.com/go-logr/logr/testr"
	corev1 "k8s.io/api/core/v1"
)

func TestParseImagePullSecrets(t *testing.T) {
	log := testr.New(t)

	assert.Equals(t, []corev1.LocalObjectReference{
		{Name: "registry-one"},
		{Name: "registry-two"},
	}, ParseImagePullSecrets(`["registry-one","registry-two"]`, log))
	assert.Equals(t, []corev1.LocalObjectReference{{Name: "valid"}}, ParseImagePullSecrets(`["valid","INVALID NAME"]`, log))
	assert.Equals(t, 0, len(ParseImagePullSecrets(`not-json`, log)))
	assert.Equals(t, 0, len(ParseImagePullSecrets(`[]`, log)))
}
