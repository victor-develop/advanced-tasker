package jobs

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ValidActions enumerates allowed report.next[].action values per
// design/06 §"next.action enum and required args".
var ValidActions = []string{
	"outbox.send",
	"task.update",
	"task.create",
	"task.kill",
	"task.defer",
	"task.link",
	"task.unlink",
	"rollup.note",
	"dispatch",
	"ask-human",
}

// ValidConfidences enumerates allowed confidence values.
var ValidConfidences = []string{"low", "med", "high"}

// ValidRisks enumerates allowed risk classifications.
var ValidRisks = []string{"low", "normal", "high"}

// TLDRMaxChars is the upper bound on the report's TLDR length.
const TLDRMaxChars = 200

// ValidateReport runs every check listed in design/06 §"Validation on
// submit-report". The Job is needed for outcome_enum + task ownership
// (artifact paths must live under the task's artifacts dir).
func ValidateReport(j *Job, r *Report) error {
	if r == nil {
		return fmt.Errorf("report is nil")
	}
	if r.JobID != j.ID {
		return fmt.Errorf("report.job_id %q does not match job id %q", r.JobID, j.ID)
	}
	if !inSlice(j.Expects.OutcomeEnum, r.Outcome) {
		return fmt.Errorf("outcome %q not in expects.outcome_enum %v", r.Outcome, j.Expects.OutcomeEnum)
	}
	if !inSlice(ValidConfidences, r.Confidence) {
		return fmt.Errorf("confidence %q invalid (want %v)", r.Confidence, ValidConfidences)
	}
	tldr := strings.TrimSpace(r.TLDR)
	if tldr == "" {
		return fmt.Errorf("tldr must not be empty")
	}
	if len(tldr) > TLDRMaxChars {
		return fmt.Errorf("tldr %d chars exceeds max %d", len(tldr), TLDRMaxChars)
	}
	for i, n := range r.Next {
		if err := validateNext(j, i, n); err != nil {
			return err
		}
	}
	for _, a := range r.Artifacts {
		if err := validateArtifactPath(j.TaskID, a); err != nil {
			return err
		}
	}
	return nil
}

func validateNext(j *Job, idx int, n Next) error {
	if !inSlice(ValidActions, n.Action) {
		return fmt.Errorf("next[%d].action %q invalid (want %v)", idx, n.Action, ValidActions)
	}
	switch n.Action {
	case "outbox.send":
		if !inSlice(ValidRisks, n.Risk) {
			return fmt.Errorf("next[%d] outbox.send requires risk in %v (got %q)", idx, ValidRisks, n.Risk)
		}
		for _, k := range []string{"thread", "body_file"} {
			if _, ok := n.Args[k]; !ok {
				return fmt.Errorf("next[%d] outbox.send missing arg %q", idx, k)
			}
		}
	case "task.update":
		for _, k := range []string{"id"} {
			if _, ok := n.Args[k]; !ok {
				return fmt.Errorf("next[%d] task.update missing arg %q", idx, k)
			}
		}
	case "task.create":
		for _, k := range []string{"title"} {
			if _, ok := n.Args[k]; !ok {
				return fmt.Errorf("next[%d] task.create missing arg %q", idx, k)
			}
		}
	case "task.kill":
		for _, k := range []string{"id", "reason"} {
			if _, ok := n.Args[k]; !ok {
				return fmt.Errorf("next[%d] task.kill missing arg %q", idx, k)
			}
		}
	case "task.defer":
		for _, k := range []string{"id", "reason"} {
			if _, ok := n.Args[k]; !ok {
				return fmt.Errorf("next[%d] task.defer missing arg %q", idx, k)
			}
		}
	case "task.link", "task.unlink":
		for _, k := range []string{"from", "to"} {
			if _, ok := n.Args[k]; !ok {
				return fmt.Errorf("next[%d] %s missing arg %q", idx, n.Action, k)
			}
		}
	case "rollup.note":
		for _, k := range []string{"thread", "verbatim"} {
			if _, ok := n.Args[k]; !ok {
				return fmt.Errorf("next[%d] rollup.note missing arg %q", idx, k)
			}
		}
	case "dispatch":
		for _, k := range []string{"task", "role", "instruction"} {
			if _, ok := n.Args[k]; !ok {
				return fmt.Errorf("next[%d] dispatch missing arg %q", idx, k)
			}
		}
	case "ask-human":
		if _, ok := n.Args["question"]; !ok {
			return fmt.Errorf("next[%d] ask-human missing arg %q", idx, "question")
		}
	}
	return nil
}

func validateArtifactPath(taskID, p string) error {
	// Allow either bare names like "review.md" (interpreted under
	// tasks/<id>/artifacts/) or full relative paths under that prefix.
	if strings.Contains(p, "..") {
		return fmt.Errorf("artifact path %q must not contain '..'", p)
	}
	if filepath.IsAbs(p) {
		return fmt.Errorf("artifact path %q must be relative", p)
	}
	prefix := filepath.Join("tasks", taskID, "artifacts")
	clean := filepath.Clean(p)
	if strings.HasPrefix(clean, prefix+string(filepath.Separator)) || clean == prefix {
		return nil
	}
	// Permit bare artifact names (no directory component); they live
	// implicitly under tasks/<id>/artifacts/.
	if !strings.Contains(clean, string(filepath.Separator)) {
		return nil
	}
	return fmt.Errorf("artifact path %q must be under %s/", p, prefix)
}

func inSlice(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
