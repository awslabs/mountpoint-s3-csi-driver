package testutil

import "testing"

// CleanRegionEnv ensures that no AWS Region environment variables are set in the test case `t`.
// The calling process of `go test` may contain environment variables like `AWS_REGION` and `AWS_DEFAULT_REGION`,
// which could be inherited by our test cases and cause unexpected test failures.
// This function provides a clean environment for the test case `t` by removing these variables.
func CleanRegionEnv(t *testing.T) {
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_REGION", "")
}
