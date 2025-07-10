// Package assert provides utilities for making assertions during tests.
package assert

import (
	"os"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// Equals fails the test if `want` and `got` are not equal.
func Equals(t *testing.T, want any, got any) {
	t.Helper()
	if diff := cmp.Diff(want, got, cmpopts.EquateErrors()); diff != "" {
		t.Errorf("Assertion failure (-want +got):\n%s", diff)
	}
}

// NoError fails the test if `err` is not nil.
func NoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("Expected no error, but got: %s", err)
	}
}

// FileNotExists fails the test if the file at the given path exists.
func FileNotExists(t *testing.T, path string) {
	t.Helper()
	_, err := os.Stat(path)
	if err == nil {
		t.Fatalf("Expected file %q to not exist, but it does", path)
	}
	if !os.IsNotExist(err) {
		t.Fatalf("Expected file %q to not exist, got unexpected error: %s", path, err)
	}
}
