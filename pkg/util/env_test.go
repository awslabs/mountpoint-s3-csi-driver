// References https://github.com/kubernetes/kubernetes/blob/v1.36.2/pkg/util/env/env_test.go

package util

import (
	"strconv"
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

func TestGetEnvAsInt(t *testing.T) {
	const expected = 1

	// Valid int: returns parsed value, no error
	key := "TEST_INT_VAR"
	t.Setenv(key, strconv.Itoa(expected))
	returnVal, err := GetEnvAsInt(key)
	assert.NoError(t, err)
	assert.Equals(t, expected, returnVal)

	// Unset: returns error
	key = "TEST_UNSET_VAR"
	_, err = GetEnvAsInt(key)
	if err == nil {
		t.Error("expected error for unset variable")
	}
	t.Logf("%s", err)

	// Invalid string: returns error
	key = "TEST_INVALID_VAR"
	t.Setenv(key, "not-an-int")
	_, err = GetEnvAsInt(key)
	if err == nil {
		t.Error("expected error for invalid string")
	}
	t.Logf("%s", err)
}
