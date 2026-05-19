package rollup

import (
	"strings"
	"testing"
)

const sampleRollup = `---
id: github-acme-api-pr-1284
source: github
url: https://github.com/acme/api/pull/1284
state: awaiting-author-response
owner_task: T-12
---

## Goal
Replace fixed-backoff with jittered exponential.

## Current ask
- Alice asks max_retries be configurable
- CI is red

## Open questions
- [ ] Keep old fixed-backoff path?

## Decisions ledger
- 2026-05-14: Use jittered exponential per AWS architecture blog
- 2026-05-16: max_retries default = 5

## Verbatim pins
> "ship before friday" — alice, 2026-05-17
> "we cannot break legacy clients" — bob, 2026-05-18 (— pinned by human)
`

func TestParseValid(t *testing.T) {
	r, err := Parse(sampleRollup)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if r.Front.ID != "github-acme-api-pr-1284" {
		t.Errorf("id wrong: %q", r.Front.ID)
	}
	if r.Front.State != "awaiting-author-response" {
		t.Errorf("state wrong: %q", r.Front.State)
	}
	if len(r.CurrentAsk) != 2 {
		t.Errorf("current ask lines: %v", r.CurrentAsk)
	}
	if len(r.DecisionsLines) != 2 {
		t.Errorf("ledger lines: %v", r.DecisionsLines)
	}
	if len(r.VerbatimPins) != 2 {
		t.Errorf("verbatim pins: %v", r.VerbatimPins)
	}
	if !IsHumanPin(r.VerbatimPins[1]) {
		t.Errorf("expected human pin on second verbatim line: %q", r.VerbatimPins[1])
	}
}

func TestValidate(t *testing.T) {
	r, err := Parse(sampleRollup)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateRejectsBadState(t *testing.T) {
	s := strings.Replace(sampleRollup, "state: awaiting-author-response", "state: nope", 1)
	r, err := Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Validate(); err == nil {
		t.Errorf("expected validation error for bad state")
	}
}

func TestAppendOnlyOK(t *testing.T) {
	old := []string{"A", "B", "C"}
	new := []string{"A", "B", "C", "D"}
	if err := CheckAppendOnly(old, new); err != nil {
		t.Errorf("unexpected: %v", err)
	}
}

func TestAppendOnlyRejectModify(t *testing.T) {
	old := []string{"A", "B", "C"}
	new := []string{"A", "B'", "C"}
	if err := CheckAppendOnly(old, new); err == nil {
		t.Errorf("expected error")
	}
}

func TestAppendOnlyRejectRemove(t *testing.T) {
	old := []string{"A", "B", "C"}
	new := []string{"A", "C"}
	if err := CheckAppendOnly(old, new); err == nil {
		t.Errorf("expected error")
	}
}

func TestAppendOnlyRejectReorder(t *testing.T) {
	old := []string{"A", "B"}
	new := []string{"B", "A"}
	if err := CheckAppendOnly(old, new); err == nil {
		t.Errorf("expected error")
	}
}

func TestHumanPinPreservation(t *testing.T) {
	old := []string{
		`> "ship friday" — alice`,
		`> "no breaking changes" — bob (— pinned by human)`,
	}
	// Adding more is fine.
	if err := CheckHumanPinsPreserved(old, append(old, `> "lgtm" — me`)); err != nil {
		t.Errorf("expected ok: %v", err)
	}
	// Removing non-human is fine.
	if err := CheckHumanPinsPreserved(old, []string{old[1]}); err != nil {
		t.Errorf("expected ok: %v", err)
	}
	// Removing human is rejected.
	if err := CheckHumanPinsPreserved(old, []string{old[0]}); err == nil {
		t.Errorf("expected human-pin removal rejected")
	}
}

func TestCurrentAskCap(t *testing.T) {
	body := strings.Replace(
		sampleRollup,
		"## Current ask\n- Alice asks max_retries be configurable\n- CI is red\n",
		"## Current ask\n- one\n- two\n- three\n- four\n",
		1,
	)
	r, err := Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Validate(); err == nil {
		t.Errorf("expected over-cap rejection")
	}
}
