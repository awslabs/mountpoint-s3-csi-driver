package util

import (
	"strings"
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
	"github.com/go-logr/logr/testr"
)

func TestParseAnnotations(t *testing.T) {
	t.Run("Valid annotations", func(t *testing.T) {
		longValue := strings.Repeat("a", 128)
		annotationsJSON := `{"example.com/exclude":"true","description":"` + longValue + `"}`

		annotations := ParseAnnotations(annotationsJSON, testr.New(t))

		assert.Equals(t, 2, len(annotations))
		assert.Equals(t, "true", annotations["example.com/exclude"])
		assert.Equals(t, longValue, annotations["description"])
	})

	t.Run("Reserved prefix is rejected", func(t *testing.T) {
		annotationsJSON := `{"example.com/exclude":"true","s3.csi.aws.com/volume-name":"override"}`

		annotations := ParseAnnotations(annotationsJSON, testr.New(t))

		assert.Equals(t, 1, len(annotations))
		assert.Equals(t, "true", annotations["example.com/exclude"])
	})

	t.Run("Invalid keys are filtered out", func(t *testing.T) {
		annotationsJSON := `{"example.com/exclude":"true","invalid key with spaces":"value"}`

		annotations := ParseAnnotations(annotationsJSON, testr.New(t))

		assert.Equals(t, 1, len(annotations))
		assert.Equals(t, "true", annotations["example.com/exclude"])
	})

	t.Run("Invalid JSON is ignored", func(t *testing.T) {
		annotations := ParseAnnotations(`{"example.com/exclude":`, testr.New(t))

		assert.Equals(t, 0, len(annotations))
	})
}
