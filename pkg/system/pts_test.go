//go:build linux

package system_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/system"
)

func TestPtsSuccess(t *testing.T) {
	pts := system.NewOsPts()
	ptm, n, err := pts.NewPts()
	if err != nil {
		t.Fatal(err)
	}
	defer ptm.Close()

	// open pts
	ptsFile, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer ptsFile.Close()

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
	os.Setenv(system.PtmxPathEnv, "/bad/path")
	defer os.Unsetenv(system.PtmxPathEnv)
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
		t.Fatalf("Failed to create test file")
	}
	f.Close()
	defer os.Remove(filename)
	os.Setenv(system.PtmxPathEnv, filename)
	defer os.Unsetenv(system.PtmxPathEnv)
	pts := system.NewOsPts()
	_, _, err = pts.NewPts()
	if err == nil {
		t.Fatalf("Expected NewPts to fail with non ptmx file")
	}
}
