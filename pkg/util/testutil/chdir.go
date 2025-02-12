package testutil

import (
	"os"
	"testing"
)

// Chdir changes working directory to `dir` and resets it back to the original in `t.Cleanup`.
// TODO: Go 1.24 will have `t.Chdir`, remove this once Go 1.24 released and we start using it.
// Copied from https://github.com/golang/go/blob/go1.23.5/src/os/exec/exec_test.go#L198-L218.
func Chdir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Logf("Chdir(%#q)", dir)
	t.Cleanup(func() {
		if err := os.Chdir(prev); err != nil {
			// Couldn't chdir back to the original working directory.
			// panic instead of t.Fatal so that we don't run other tests
			// in an unexpected location.
			panic("couldn't restore working directory: " + err.Error())
		}
	})
}
