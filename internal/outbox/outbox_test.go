package outbox

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRiskValidation covers the validator inputs design/07 cares about.
func TestRiskValidation(t *testing.T) {
	for _, r := range []string{"low", "normal", "high"} {
		if !IsValidRisk(r) {
			t.Errorf("expected %q valid", r)
		}
	}
	for _, r := range []string{"", "LOW", "danger"} {
		if IsValidRisk(r) {
			t.Errorf("expected %q invalid", r)
		}
	}
	if RankRisk(RiskLow) >= RankRisk(RiskNormal) {
		t.Errorf("low must rank below normal")
	}
	if RankRisk(RiskHigh) <= RankRisk(RiskNormal) {
		t.Errorf("high must rank above normal")
	}
}

// TestRateCheck_RejectsAtThreshold exercises the per-thread limit:
// once N items in the last hour exist in sent/, the next send is
// refused.
func TestRateCheck_RejectsAtThreshold(t *testing.T) {
	root := t.TempDir()
	sent := Dir(root, StateSent)
	if err := os.MkdirAll(sent, 0o755); err != nil {
		t.Fatal(err)
	}
	thread := "slack-C0001-1"
	now := time.Now().UTC()
	// Seed 2 sent items in the past 30 min.
	for i := 0; i < 2; i++ {
		it := &Item{
			ID:        "PAST-" + string(rune('a'+i)),
			Ref:       Ref{Thread: thread},
			Risk:      RiskLow,
			Body:      "ok",
			CreatedBy: "tester",
			SentAt:    now.Add(-15 * time.Minute),
			To:        "slack",
		}
		if err := Write(filepath.Join(sent, it.ID+".yaml"), it); err != nil {
			t.Fatal(err)
		}
	}
	// Limits: per-thread = 2 → next should refuse.
	lim := DefaultLimits()
	lim.PerThreadPerHour = 2
	next := &Item{ID: "NEW", Ref: Ref{Thread: thread}, Risk: RiskLow}
	if err := RateCheck(root, next, lim, now); err == nil {
		t.Errorf("expected per-thread limit refusal at threshold")
	}
	// Raising the limit allows it.
	lim.PerThreadPerHour = 5
	if err := RateCheck(root, next, lim, now); err != nil {
		t.Errorf("expected ok under raised limit, got %v", err)
	}
}

// TestSenderEnabled_DefaultsAndExplicit covers round-3 D5's semantics:
// missing key → default TRUE (backwards compat); explicit false → off.
func TestSenderEnabled_DefaultsAndExplicit(t *testing.T) {
	// No config.yaml present at all → defaults to true.
	emptyRoot := t.TempDir()
	if !SenderEnabled(emptyRoot) {
		t.Errorf("missing config.yaml should default to true (backwards compat)")
	}

	// Config without the key → defaults to true.
	noKeyRoot := t.TempDir()
	os.WriteFile(filepath.Join(noKeyRoot, "config.yaml"), []byte("git:\n  auto_commit: true\n"), 0o644)
	if !SenderEnabled(noKeyRoot) {
		t.Errorf("config without outbox.sender_enabled should default to true")
	}

	// Explicit false → off.
	falseRoot := t.TempDir()
	os.WriteFile(filepath.Join(falseRoot, "config.yaml"), []byte("outbox:\n  sender_enabled: false\n"), 0o644)
	if SenderEnabled(falseRoot) {
		t.Errorf("explicit false must disable the sender")
	}

	// Explicit true → on.
	trueRoot := t.TempDir()
	os.WriteFile(filepath.Join(trueRoot, "config.yaml"), []byte("outbox:\n  sender_enabled: true\n"), 0o644)
	if !SenderEnabled(trueRoot) {
		t.Errorf("explicit true must enable the sender")
	}
}
