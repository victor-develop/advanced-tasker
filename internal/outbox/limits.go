package outbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// RateLimits is what config.yaml.limits exposes for the outbox-sender.
type RateLimits struct {
	PerThreadPerHour  int
	PerChannelPerHour int
	GlobalPerHour     int
	RevokeWindow      time.Duration
}

// DefaultLimits is used when config.yaml is silent on a key.
func DefaultLimits() RateLimits {
	return RateLimits{
		PerThreadPerHour:  5,
		PerChannelPerHour: 20,
		GlobalPerHour:     100,
		RevokeWindow:      5 * time.Minute,
	}
}

// LoadLimits reads state/config.yaml and returns a populated RateLimits.
// Missing keys fall back to defaults.
func LoadLimits(stateRoot string) (RateLimits, error) {
	lim := DefaultLimits()
	b, err := os.ReadFile(filepath.Join(stateRoot, "config.yaml"))
	if err != nil {
		return lim, err
	}
	var raw map[string]any
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return lim, err
	}
	limits, ok := raw["limits"].(map[string]any)
	if !ok {
		return lim, nil
	}
	if v, ok := limits["outbox_per_thread_per_hour"]; ok {
		lim.PerThreadPerHour = asInt(v, lim.PerThreadPerHour)
	}
	if v, ok := limits["outbox_per_channel_per_hour"]; ok {
		lim.PerChannelPerHour = asInt(v, lim.PerChannelPerHour)
	}
	if v, ok := limits["outbox_global_per_hour"]; ok {
		lim.GlobalPerHour = asInt(v, lim.GlobalPerHour)
	}
	if v, ok := limits["outbox_revoke_window"]; ok {
		switch x := v.(type) {
		case string:
			if d, err := time.ParseDuration(x); err == nil {
				lim.RevokeWindow = d
			}
		}
	}
	return lim, nil
}

func asInt(v any, fallback int) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	}
	return fallback
}

// RateCheck inspects outbox/sent/ for items in the last hour and
// returns an error if sending it would violate any configured limit.
// Empty error means OK to send.
func RateCheck(stateRoot string, it *Item, lim RateLimits, now time.Time) error {
	cutoff := now.Add(-time.Hour)
	sentDir := Dir(stateRoot, StateSent)
	entries, err := os.ReadDir(sentDir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var thread, channel, global int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		past, err := Read(filepath.Join(sentDir, e.Name()))
		if err != nil {
			continue
		}
		if past.SentAt.IsZero() || past.SentAt.Before(cutoff) {
			continue
		}
		global++
		if past.Ref.Thread == it.Ref.Thread {
			thread++
		}
		if sameChannel(past.Ref.Thread, it.Ref.Thread) {
			channel++
		}
	}
	if thread >= lim.PerThreadPerHour {
		return fmt.Errorf("per-thread rate limit reached (%d/%d for %s)", thread, lim.PerThreadPerHour, it.Ref.Thread)
	}
	if channel >= lim.PerChannelPerHour {
		return fmt.Errorf("per-channel rate limit reached (%d/%d)", channel, lim.PerChannelPerHour)
	}
	if global >= lim.GlobalPerHour {
		return fmt.Errorf("global rate limit reached (%d/%d)", global, lim.GlobalPerHour)
	}
	return nil
}

// sameChannel approximates "same Slack channel / same GH repo" by
// comparing the channel/repo prefix of two thread IDs:
//   slack-C0492-1715814123.001200 → channel C0492
//   github-acme-api-pr-1284       → repo acme-api
func sameChannel(a, b string) bool {
	return channelPrefix(a) == channelPrefix(b)
}

func channelPrefix(thread string) string {
	parts := strings.Split(thread, "-")
	if len(parts) < 2 {
		return thread
	}
	if parts[0] == "slack" {
		return parts[0] + "-" + parts[1]
	}
	if parts[0] == "github" && len(parts) >= 3 {
		return parts[0] + "-" + parts[1] + "-" + parts[2]
	}
	return thread
}

// SenderEnabled reports whether the outbox sender daemon is allowed to
// call upstream provider APIs.
//
// Per round-3 D5: the value lives at `outbox.sender_enabled` in
// state/config.yaml. Missing key → defaults to TRUE for backwards
// compatibility with pre-round-3 state directories (those keep the
// round-2 sender behavior the acceptance script depends on). The
// `harness init` template seeds the key explicitly as FALSE so freshly
// initialized harnesses are safe-by-default.
func SenderEnabled(stateRoot string) bool {
	b, err := os.ReadFile(filepath.Join(stateRoot, "config.yaml"))
	if err != nil {
		return true
	}
	var raw map[string]any
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return true
	}
	out, ok := raw["outbox"].(map[string]any)
	if !ok {
		return true
	}
	v, present := out["sender_enabled"]
	if !present {
		return true
	}
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return !strings.EqualFold(x, "false")
	}
	return true
}

// DuplicateCheck rejects sends identical to one in the last 10 minutes
// (content + thread + sender). Returns an error on duplicate detection.
func DuplicateCheck(stateRoot string, it *Item, now time.Time) error {
	cutoff := now.Add(-10 * time.Minute)
	sentDir := Dir(stateRoot, StateSent)
	entries, err := os.ReadDir(sentDir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		past, err := Read(filepath.Join(sentDir, e.Name()))
		if err != nil {
			continue
		}
		if past.SentAt.IsZero() || past.SentAt.Before(cutoff) {
			continue
		}
		if past.Ref.Thread != it.Ref.Thread {
			continue
		}
		if past.CreatedBy != it.CreatedBy {
			continue
		}
		if past.Body == it.Body && past.Body != "" {
			return fmt.Errorf("duplicate of %s within 10 minutes", past.ID)
		}
	}
	return nil
}
