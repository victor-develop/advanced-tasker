package telemetry

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestAppendSummary_ParseRoundtrip exercises the design/10 line format:
//
//	<iso> <kind>  cost=$<f>  dur=<ms>ms  err=<bool>  session=<id>
//
// We write two lines and confirm CostSince aggregates them.
func TestAppendSummary_ParseRoundtrip(t *testing.T) {
	root := t.TempDir()
	if err := AppendSummary(root, "tick", 0.42, 1234, false, "sess-a"); err != nil {
		t.Fatalf("append 1: %v", err)
	}
	if err := AppendSummary(root, "worker", 0.05, 800, false, "sess-b"); err != nil {
		t.Fatalf("append 2: %v", err)
	}
	body, err := os.ReadFile(SummaryPath(root))
	if err != nil {
		t.Fatalf("read summary: %v", err)
	}
	bodyS := string(body)
	for _, want := range []string{
		"cost=$0.4200",  // %.4f format
		"dur=1234ms",
		"err=false",
		"session=sess-a",
		"session=sess-b",
		"cost=$0.0500",
		"dur=800ms",
	} {
		if !strings.Contains(bodyS, want) {
			t.Errorf("summary.log missing %q\n%s", want, bodyS)
		}
	}

	// CostSince(all time) sums both.
	total, err := CostSince(root, time.Time{})
	if err != nil {
		t.Fatalf("CostSince: %v", err)
	}
	if total < 0.46 || total > 0.48 {
		t.Errorf("expected ~0.47 USD aggregated, got %g", total)
	}
}

// TestCostSince_MissingFileReturnsZero — the dashboard renders 0.0 even
// before the first tick.
func TestCostSince_MissingFileReturnsZero(t *testing.T) {
	root := t.TempDir()
	total, err := CostSince(root, time.Time{})
	if err != nil {
		t.Fatalf("CostSince: %v", err)
	}
	if total != 0 {
		t.Errorf("expected 0 cost for missing file, got %g", total)
	}
}
