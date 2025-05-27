package version_test

import (
	"fmt"
	"reflect"
	"runtime"
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/version"
)

func TestGetVersion(t *testing.T) {
	got := version.GetVersion()
	expected := version.VersionInfo{
		DriverVersion: "",
		GitCommit:     "",
		BuildDate:     "",
		GoVersion:     runtime.Version(),
		Compiler:      runtime.Compiler,
		Platform:      fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
	}
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("structs not equal\ngot:\n%+v\nexpected: \n%+v", got, expected)
	}
}

func TestGetVersionJSON(t *testing.T) {
	got, err := version.GetVersionJSON()
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	expected := fmt.Sprintf(`{
  "driverVersion": "",
  "gitCommit": "",
  "buildDate": "",
  "goVersion": "%s",
  "compiler": "%s",
  "platform": "%s"
}`, runtime.Version(), runtime.Compiler, fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH))

	if got != expected {
		t.Fatalf("json not equal\ngot:\n%s\nexpected:\n%s", got, expected)
	}
}
