// Package assert provides utilities for making assertions during tests.
package assert

import (
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
