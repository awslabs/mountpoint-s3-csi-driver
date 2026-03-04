package util

import (
	"strings"
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
	"github.com/go-logr/logr/testr"
)

func TestParseLabels(t *testing.T) {
	t.Run("Valid labels", func(t *testing.T) {
		labelsJSON := `{"app":"myapp","env":"prod","kubernetes.io/instance":"test-instance"}`
		log := testr.New(t)

		labels := ParseLabels(labelsJSON, log)

		assert.Equals(t, 3, len(labels))
		assert.Equals(t, "myapp", labels["app"])
		assert.Equals(t, "prod", labels["env"])
		assert.Equals(t, "test-instance", labels["kubernetes.io/instance"])
	})

	t.Run("Reserved prefix is rejected", func(t *testing.T) {
		labelsJSON := `{"example-label":"example-value","s3.csi.aws.com/label1":"value1","s3.csi.aws.com/label2":"value2"}`
		log := testr.New(t)

		labels := ParseLabels(labelsJSON, log)

		assert.Equals(t, 1, len(labels))
		assert.Equals(t, "example-value", labels["example-label"])
	})

	t.Run("Invalid labels are filtered out", func(t *testing.T) {
		// Label values must be 63 characters or less
		longValue := strings.Repeat("a", 64)
		labelsJSON := `{"app":"myapp","invalid key with spaces":"value","key":"` + longValue + `"}`
		log := testr.New(t)

		labels := ParseLabels(labelsJSON, log)

		// Only the valid label should be present
		assert.Equals(t, 1, len(labels))
		assert.Equals(t, "myapp", labels["app"])
	})
}
