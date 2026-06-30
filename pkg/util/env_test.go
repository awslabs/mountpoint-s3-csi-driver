/*
Copyright 2015 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Adapted from https://github.com/kubernetes/kubernetes/blob/v1.36.2/pkg/util/env/env_test.go

package util

import (
	"strconv"
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

func TestGetEnvAsIntOrFallback(t *testing.T) {
	const expected = 1

	// Valid int: returns parsed value, no error
	key := "FLOCKER_SET_VAR"
	t.Setenv(key, strconv.Itoa(expected))
	returnVal, err := GetEnvAsIntOrFallback(key, 1)
	assert.NoError(t, err)
	assert.Equals(t, expected, returnVal)

	// Unset: returns fallback, no error
	key = "FLOCKER_UNSET_VAR"
	returnVal, err = GetEnvAsIntOrFallback(key, expected)
	assert.NoError(t, err)
	assert.Equals(t, expected, returnVal)

	// Invalid string: returns fallback + error
	key = "FLOCKER_SET_VAR"
	t.Setenv(key, "not-an-int")
	returnVal, err = GetEnvAsIntOrFallback(key, 1)
	assert.Equals(t, expected, returnVal)
	if err == nil {
		t.Error("expected error")
	}
}
