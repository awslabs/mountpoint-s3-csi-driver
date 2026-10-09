// Package helmtemplate checks the Helm chart renders with `helm template`.
package helmtemplate

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

const releaseName = "s3-csi"

// chartPath returns the absolute path to the Helm chart.
func chartPath() string {
	_, f, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(f), "..", "..", "charts", "aws-mountpoint-s3-csi-driver")
}

// helmBin returns the helm binary to use ("helm" on PATH by default).
func helmBin() string {
	if b := os.Getenv("HELM_BIN"); b != "" {
		return b
	}
	return "helm"
}

// renderChart runs `helm template` with the chart's default values and fails
// the test if the chart does not render.
func renderChart(t *testing.T) {
	t.Helper()
	// unsupportedDevInstall=true bypasses the guard that blocks `helm template`
	// on the chart outside the official Helm repo.
	cmd := exec.Command(helmBin(), "template", releaseName, chartPath(), "--set", "unsupportedDevInstall=true")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm template failed: %v\nstderr: %s", err, stderr.String())
	}
}
