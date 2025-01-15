// Package version provides build- and run-time version information of the driver.
package version

import (
	"encoding/json"
	"fmt"
	"runtime"
)

// Populated during build-time in `Makefile`.
var (
	driverVersion string
	gitCommit     string
	buildDate     string
)

// A VersionInfo represents build- and run-time version information of the driver.
type VersionInfo struct {
	DriverVersion string `json:"driverVersion"`
	GitCommit     string `json:"gitCommit"`
	BuildDate     string `json:"buildDate"`
	GoVersion     string `json:"goVersion"`
	Compiler      string `json:"compiler"`
	Platform      string `json:"platform"`
}

// GetVersion returns a `VersionInfo`.
func GetVersion() VersionInfo {
	return VersionInfo{
		DriverVersion: driverVersion,
		GitCommit:     gitCommit,
		BuildDate:     buildDate,
		GoVersion:     runtime.Version(),
		Compiler:      runtime.Compiler,
		Platform:      fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
	}
}

// GetVersionJSON returns JSON string representation of `VersionInfo`.
func GetVersionJSON() (string, error) {
	info := GetVersion()
	marshalled, err := json.MarshalIndent(&info, "", "  ")
	if err != nil {
		return "", err
	}
	return string(marshalled), nil
}
