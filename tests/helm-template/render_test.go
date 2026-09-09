package helmtemplate

import "testing"

// TestChartRenders verifies the Helm chart renders without error.
func TestChartRenders(t *testing.T) {
	renderChart(t)
}
