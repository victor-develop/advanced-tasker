package slack

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFormatChannelAccessReason_NotInChannel verifies the operator-actionable
// reason string. Per round-3 §D3, the not_in_channel anomaly reason must be:
//
//	bot not in channel; invite via /invite @<bot> in #<channel>
func TestFormatChannelAccessReason_NotInChannel(t *testing.T) {
	got := formatChannelAccessReason("C0492", "not_in_channel")
	want := "bot not in channel; invite via /invite @<bot> in #C0492"
	if got != want {
		t.Errorf("formatChannelAccessReason = %q, want %q", got, want)
	}
}

// TestFormatChannelAccessReason_ChannelNotFound checks the actionable text
// for an unknown / private channel.
func TestFormatChannelAccessReason_ChannelNotFound(t *testing.T) {
	got := formatChannelAccessReason("C_GHOST", "channel_not_found")
	if !strings.Contains(got, "C_GHOST") || !strings.Contains(got, "check ID") {
		t.Errorf("missing channel id or actionable hint: %q", got)
	}
}

// TestFormatChannelAccessReason_Unknown falls back to the original message.
func TestFormatChannelAccessReason_Unknown(t *testing.T) {
	got := formatChannelAccessReason("C0492", "some_other_thing")
	if !strings.Contains(got, "some_other_thing") {
		t.Errorf("unknown reason not preserved: %q", got)
	}
}

// TestAnomalyChannelAccess_FileContents writes a not_in_channel anomaly
// through the writer and asserts the JSON on disk includes the actionable
// reason string, the channel id, and the canonical filename.
func TestAnomalyChannelAccess_FileContents(t *testing.T) {
	stateRoot := t.TempDir()
	w := NewWriter(stateRoot)

	reason := formatChannelAccessReason("C0492", "not_in_channel")
	a := AnomalyChannelAccess("C0492", reason)
	wrote, err := w.WriteAnomaly(a)
	if err != nil {
		t.Fatalf("WriteAnomaly: %v", err)
	}
	if !wrote {
		t.Fatal("expected wrote=true")
	}
	path := filepath.Join(stateRoot, "inbox", "anomalies",
		"slack-channel-access-C0492.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read anomaly: %v", err)
	}
	var got Anomaly
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Channel != "C0492" {
		t.Errorf("channel = %q", got.Channel)
	}
	if got.Kind != "channel_access" {
		t.Errorf("kind = %q", got.Kind)
	}
	if !strings.Contains(got.Reason, "/invite") {
		t.Errorf("anomaly reason not operator-actionable: %q", got.Reason)
	}
}
