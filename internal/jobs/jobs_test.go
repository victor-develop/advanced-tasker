package jobs

import (
	"strings"
	"testing"
)

func mkJob() *Job {
	return &Job{
		ID:          "J-abc",
		Role:        "pr-reviewer",
		TaskID:      "T-12",
		Instruction: "do thing",
		Context:     Context{},
		Expects:     Expects{OutcomeEnum: []string{"approve", "reject"}},
	}
}

func TestValidateReportOK(t *testing.T) {
	j := mkJob()
	r := &Report{
		JobID: "J-abc", Outcome: "approve", Confidence: "med", TLDR: "ok",
		Next: []Next{
			{Action: "task.update", Args: map[string]interface{}{"id": "T-12"}},
			{Action: "outbox.send", Risk: "low", Args: map[string]interface{}{"thread": "x", "body_file": "y"}},
		},
		Artifacts: []string{"r.md"},
	}
	if err := ValidateReport(j, r); err != nil {
		t.Errorf("expected ok: %v", err)
	}
}

func TestValidateReportBadOutcome(t *testing.T) {
	j := mkJob()
	r := &Report{JobID: "J-abc", Outcome: "huh", Confidence: "med", TLDR: "ok"}
	if err := ValidateReport(j, r); err == nil {
		t.Errorf("expected err for outcome not in enum")
	}
}

func TestValidateReportLongTLDR(t *testing.T) {
	j := mkJob()
	r := &Report{JobID: "J-abc", Outcome: "approve", Confidence: "med", TLDR: strings.Repeat("a", 201)}
	if err := ValidateReport(j, r); err == nil {
		t.Errorf("expected long-tldr rejection")
	}
}

func TestValidateReportOutboxRequiresRisk(t *testing.T) {
	j := mkJob()
	r := &Report{JobID: "J-abc", Outcome: "approve", Confidence: "med", TLDR: "ok",
		Next: []Next{{Action: "outbox.send", Args: map[string]interface{}{"thread": "x", "body_file": "y"}}},
	}
	if err := ValidateReport(j, r); err == nil {
		t.Errorf("expected risk-required rejection")
	}
}

func TestValidateReportArtifactsOutsideTaskDir(t *testing.T) {
	j := mkJob()
	r := &Report{JobID: "J-abc", Outcome: "approve", Confidence: "med", TLDR: "ok",
		Artifacts: []string{"other-task/oops.md"}}
	if err := ValidateReport(j, r); err == nil {
		t.Errorf("expected artifact-path rejection")
	}
}
