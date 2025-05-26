//go:build linux

package system_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/scality/mountpoint-s3-csi-driver/pkg/system"
)

func TestPtsSuccess(t *testing.T) {
	pts := system.NewOsPts()
	ptm, n, err := pts.NewPts()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = ptm.Close()
	}()

	// open pts
	ptsFile, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = ptsFile.Close()
	}()

	// write to pts
	testString := "testingpts"
	_, err = ptsFile.WriteString(testString)
	if err != nil {
		t.Fatal(err)
	}

	// read from ptm
	buffer := make([]byte, len(testString))
	_, err = ptm.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}

	if string(buffer) != testString {
		t.Fatalf("Strings do not match, expected %s got %s", testString, string(buffer))
	}
}

func TestPtsBadPath(t *testing.T) {
	_ = os.Setenv(system.PtmxPathEnv, "/bad/path")
	defer func() {
		_ = os.Unsetenv(system.PtmxPathEnv)
	}()
	pts := system.NewOsPts()
	_, _, err := pts.NewPts()
	if err == nil {
		t.Fatalf("Expected NewPts to fail with bad path")
	}
}

func TestPtsBadFile(t *testing.T) {
	filename := "/tmp/not-a-ptmx"
	f, err := os.Create(filename)
	if err != nil {
		t.Fatalf("failed to create test file")
	}
	_ = f.Close()
	defer func() {
		_ = os.Remove(filename)
	}()
	_ = os.Setenv(system.PtmxPathEnv, filename)
	defer func() {
		_ = os.Unsetenv(system.PtmxPathEnv)
	}()
	pts := system.NewOsPts()
	_, _, err = pts.NewPts()
	if err == nil {
		t.Fatalf("Expected NewPts to fail with non ptmx file")
	}
}
