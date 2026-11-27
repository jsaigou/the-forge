package tui

import (
	"testing"

	"github.com/jsaigou/the-forge/internal/cli"
)

// A zero-sized or unknown GTT pool, or a narrow terminal, must render instead of panicking (divide by zero / negative strings.Repeat).
func TestOverviewGTTMeterSurvivesZeroTotalAndNarrowWidth(t *testing.T) {
	used, zero := int64(5), int64(0)
	for _, width := range []int{20, 80} {
		p := &overviewPage{width: width, status: &cli.Status{}, metrics: &cli.Metrics{GTTUsedBytes: &used, GTTTotalBytes: &zero}}
		_ = p.View()
	}
}
