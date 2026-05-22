package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ClaudeP is the production driver that execs `claude -p --print
// --output-format stream-json --verbose`, tees JSONL to StreamJSONPath,
// and parses the final {"type":"result",...} line for cost/duration.
//
// See design/10 §"LLM driver interface". The binary path is overridable
// (for tests / unusual installs) via NewClaudeP; default is "claude".
type ClaudeP struct {
	Binary string
	// ExtraArgs are appended after the default `-p --print
	// --output-format stream-json --verbose`.
	ExtraArgs []string
}

// NewClaudeP returns a ClaudeP driver. If binary is empty, defaults to
// "claude" on $PATH.
func NewClaudeP(binary string) *ClaudeP {
	if binary == "" {
		binary = "claude"
	}
	return &ClaudeP{Binary: binary}
}

// Name returns "claude-p" per the Driver contract.
func (c *ClaudeP) Name() string { return "claude-p" }

// allowedToolsFor returns the role-scoped tool whitelist passed to
// `claude -p --allowedTools`. Per design/01 P7 "all side effects go
// through one CLI", every role that mutates state needs Bash access to
// `harness`; read-only roles get Read/Grep/Glob.
//
// The strings are claude-CLI tool patterns. See `claude --help`.
func allowedToolsFor(role Role) []string {
	switch role {
	case RoleCommander:
		return []string{"Bash(harness:*)", "Read", "Grep", "Glob"}
	case RoleWorker:
		// Workers have varied needs per design/06 §Roles; v1 grants a
		// permissive set. Tighter per-role scoping is future work.
		return []string{"Bash(harness:*)", "Read", "Grep", "Glob", "Edit", "Write"}
	case RoleAuditor:
		// Per design/11 §"Tools allowed to the audit role": read + the
		// audit and read-only harness verbs only.
		return []string{
			"Bash(harness audit:*)",
			"Bash(harness show:*)",
			"Bash(harness ls:*)",
			"Bash(harness render:*)",
			"Read",
		}
	case RoleUpdater:
		// Pure text in/out per design/05. No tools needed.
		return nil
	}
	return []string{"Read"}
}

// Invoke shells out to `claude -p` and parses the stream-json result.
//
// We do NOT call `claude` in tests — tests use NewFake. This function
// is exercised in production and by `harness doctor` smoke checks.
func (c *ClaudeP) Invoke(ctx context.Context, prompt string, opts InvokeOptions) (InvokeResult, error) {
	args := []string{"-p", "--print", "--output-format", "stream-json", "--verbose"}
	// Role-scoped tool allow-list. Without this, `claude -p` defaults to
	// no tool permissions and the commander/worker/auditor cannot call
	// `harness` even though the dashboard tells them to. Surfaced during
	// the polish-round-1 5-minute real Opus run: commander tick-logs said
	// "I need permission to run harness commands" and chose --idle.
	// --permission-mode=dontAsk pairs with --allowedTools so the
	// subprocess never blocks on a (impossible) human prompt.
	if tools := allowedToolsFor(opts.Role); len(tools) > 0 {
		args = append(args, "--permission-mode", "dontAsk",
			"--allowedTools", strings.Join(tools, " "))
	}
	if opts.SystemPrompt != "" {
		args = append(args, "--system-prompt", opts.SystemPrompt)
	}
	args = append(args, c.ExtraArgs...)

	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	start := time.Now()
	cmd := exec.CommandContext(ctx, c.Binary, args...)
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	dur := time.Since(start)

	jsonl := stdout.String()
	if opts.StreamJSONPath != "" {
		_ = os.MkdirAll(filepath.Dir(opts.StreamJSONPath), 0o755)
		_ = os.WriteFile(opts.StreamJSONPath, []byte(jsonl), 0o644)
	}

	res := InvokeResult{
		DurationMS:  dur.Milliseconds(),
		RawArtifact: opts.StreamJSONPath,
	}
	parseStreamJSON(jsonl, &res)
	if runErr != nil {
		res.IsError = true
		if res.Output == "" {
			res.Output = fmt.Sprintf("claude exec failed: %v: %s", runErr, stderr.String())
		}
		return res, runErr
	}
	return res, nil
}

// parseStreamJSON extracts the assistant text and final result event
// from a stream-json transcript. It is tolerant of unexpected line
// shapes so a single malformed line doesn't poison the result.
func parseStreamJSON(jsonl string, res *InvokeResult) {
	var assistantText strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(jsonl))
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		t, _ := ev["type"].(string)
		switch t {
		case "assistant":
			if msg, ok := ev["message"].(map[string]any); ok {
				if content, ok := msg["content"].([]any); ok {
					for _, c := range content {
						if cm, ok := c.(map[string]any); ok {
							if txt, ok := cm["text"].(string); ok {
								assistantText.WriteString(txt)
							}
						}
					}
				}
			}
		case "result":
			if v, ok := ev["total_cost_usd"].(float64); ok {
				res.CostUSD = v
			}
			if v, ok := ev["duration_ms"].(float64); ok {
				res.DurationMS = int64(v)
			}
			if v, ok := ev["is_error"].(bool); ok {
				res.IsError = v
			}
			if v, ok := ev["session_id"].(string); ok {
				res.SessionID = v
			}
		}
	}
	if assistantText.Len() > 0 {
		res.Output = assistantText.String()
	}
}
