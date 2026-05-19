// Package rollup parses, validates, and writes per-thread rollup.md
// files. The validation rules (schema, append-only ledger, human-pin
// preservation) are shared between the `harness rollup update` CLI verb
// and the post-commit hook, per design/05 §Implementation hints.
package rollup

import (
	"bufio"
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Frontmatter is the YAML block at the top of every rollup.md.
type Frontmatter struct {
	ID           string   `yaml:"id"`
	Source       string   `yaml:"source"`
	URL          string   `yaml:"url,omitempty"`
	State        string   `yaml:"state"`
	LastEvent    string   `yaml:"last_event,omitempty"`
	OwnerTask    string   `yaml:"owner_task,omitempty"`
	Participants []string `yaml:"participants,omitempty"`
}

// Rollup is a parsed rollup.md file.
type Rollup struct {
	Front          Frontmatter
	Goal           string
	CurrentAsk     []string // ≤3 lines
	OpenQuestions  []string // ≤5 lines
	DecisionsLines []string // ledger (append-only)
	VerbatimPins   []string
}

// ValidStates enumerates the rollup state enum from design/02.
var ValidStates = []string{
	"new",
	"awaiting-our-response",
	"awaiting-author-response",
	"awaiting-external",
	"in-progress",
	"blocked",
	"resolved",
	"archived",
}

// MaxCurrentAskLines is the soft cap from design/02/04. CLI enforces.
const (
	MaxCurrentAskLines    = 3
	MaxOpenQuestionsLines = 5
)

// Section header literals — kept centralized so the parser and the
// validator agree on exact spelling.
const (
	HeaderGoal           = "## Goal"
	HeaderCurrentAsk     = "## Current ask"
	HeaderOpenQuestions  = "## Open questions"
	HeaderDecisions      = "## Decisions ledger"
	HeaderVerbatim       = "## Verbatim pins"
	humanPinSuffix       = "(— pinned by human)"
)

// Parse parses the textual rollup.md content. It does NOT validate the
// schema; call Validate for that.
func Parse(body string) (*Rollup, error) {
	r := &Rollup{}

	// Frontmatter must be the first thing in the file.
	rest, fm, err := splitFrontmatter(body)
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal([]byte(fm), &r.Front); err != nil {
		return nil, fmt.Errorf("parse frontmatter: %w", err)
	}

	sections := splitSections(rest)
	r.Goal = strings.TrimSpace(sections[HeaderGoal])
	r.CurrentAsk = collectBullets(sections[HeaderCurrentAsk])
	r.OpenQuestions = collectBullets(sections[HeaderOpenQuestions])
	r.DecisionsLines = collectBullets(sections[HeaderDecisions])
	r.VerbatimPins = collectQuoteLines(sections[HeaderVerbatim])
	return r, nil
}

// Validate checks the schema-level invariants on a parsed rollup.
// Returns nil on success; otherwise a descriptive error.
func (r *Rollup) Validate() error {
	if r.Front.ID == "" {
		return errors.New("frontmatter missing id")
	}
	if r.Front.Source == "" {
		return errors.New("frontmatter missing source")
	}
	if r.Front.State == "" {
		return errors.New("frontmatter missing state")
	}
	if !isValidState(r.Front.State) {
		return fmt.Errorf("invalid state %q (want one of %v)", r.Front.State, ValidStates)
	}
	if len(r.CurrentAsk) > MaxCurrentAskLines {
		return fmt.Errorf("Current ask has %d lines (max %d)", len(r.CurrentAsk), MaxCurrentAskLines)
	}
	if len(r.OpenQuestions) > MaxOpenQuestionsLines {
		return fmt.Errorf("Open questions has %d lines (max %d)", len(r.OpenQuestions), MaxOpenQuestionsLines)
	}
	return nil
}

// CheckAppendOnly compares the OLD vs NEW Decisions ledger lines and
// enforces design/05 §"Step 2: Append-only ledger check": every old
// line must appear in new content, unchanged and in order; new lines
// may only appear after all old lines.
func CheckAppendOnly(oldLines, newLines []string) error {
	if len(newLines) < len(oldLines) {
		return fmt.Errorf("decisions ledger lost lines (old=%d new=%d)", len(oldLines), len(newLines))
	}
	for i, o := range oldLines {
		if newLines[i] != o {
			return fmt.Errorf("decisions ledger violated append-only at line %d: old=%q new=%q", i+1, o, newLines[i])
		}
	}
	return nil
}

// CheckHumanPinsPreserved enforces design/05 §"Step 3": every "(— pinned
// by human)" line in the OLD rollup must appear verbatim in NEW. New
// human pins may be added; non-human pins may be added/removed freely.
func CheckHumanPinsPreserved(oldPins, newPins []string) error {
	for _, p := range oldPins {
		if !strings.Contains(p, humanPinSuffix) {
			continue
		}
		found := false
		for _, q := range newPins {
			if q == p {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("human-pinned verbatim removed: %q", p)
		}
	}
	return nil
}

// IsHumanPin reports whether a verbatim pin line carries the human-pin
// suffix.
func IsHumanPin(line string) bool {
	return strings.Contains(line, humanPinSuffix)
}

// ---------- parsing helpers ----------

func splitFrontmatter(body string) (rest, fm string, err error) {
	body = strings.TrimLeft(body, "\n")
	if !strings.HasPrefix(body, "---") {
		return "", "", errors.New("missing leading --- frontmatter delimiter")
	}
	rest = body[len("---"):]
	rest = strings.TrimLeft(rest, " \t")
	if !strings.HasPrefix(rest, "\n") {
		return "", "", errors.New("frontmatter delimiter not followed by newline")
	}
	rest = rest[1:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", "", errors.New("missing closing --- frontmatter delimiter")
	}
	fm = rest[:end]
	rest = rest[end+len("\n---"):]
	if strings.HasPrefix(rest, "\n") {
		rest = rest[1:]
	}
	return rest, fm, nil
}

// splitSections returns a map from "## Header" exact string to that
// section's body text (excluding the header line).
func splitSections(body string) map[string]string {
	out := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var current string
	var buf strings.Builder
	flush := func() {
		if current != "" {
			out[current] = buf.String()
		}
		buf.Reset()
	}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "## ") {
			flush()
			current = strings.TrimRight(line, " \t")
			continue
		}
		if current != "" {
			buf.WriteString(line)
			buf.WriteByte('\n')
		}
	}
	flush()
	return out
}

// collectBullets returns lines that begin with "- " (after trimming),
// stripping the leading marker. Other content (blank lines, free text)
// is ignored.
func collectBullets(section string) []string {
	out := []string{}
	for _, line := range strings.Split(section, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "- ") {
			out = append(out, strings.TrimPrefix(t, "- "))
		} else if strings.HasPrefix(t, "- [") || strings.HasPrefix(t, "- [ ]") {
			out = append(out, strings.TrimPrefix(t, "- "))
		}
	}
	return out
}

// collectQuoteLines returns lines that begin with "> " (after trimming),
// preserving the rest of the line content verbatim. Used for Verbatim
// pins parsing.
func collectQuoteLines(section string) []string {
	out := []string{}
	for _, line := range strings.Split(section, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "> ") {
			out = append(out, t)
		}
	}
	return out
}

func isValidState(s string) bool {
	for _, v := range ValidStates {
		if v == s {
			return true
		}
	}
	return false
}
