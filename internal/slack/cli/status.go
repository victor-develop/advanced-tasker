package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	slackpkg "github.com/victor-develop/advanced-tasker/internal/slack"
)

// StatusReport is the structured output of `slack-poller status --json`.
type StatusReport struct {
	ConfigPath       string          `json:"config_path"`
	TrackedChannels  []ChannelStatus `json:"tracked_channels"`
	TrackedThreads   []ThreadStatus  `json:"tracked_threads"`
	PollInterval     string          `json:"poll_interval"`
	WriteUpdatePings bool            `json:"write_update_pings"`
}

// ChannelStatus reports per-channel polling state.
type ChannelStatus struct {
	ID           string `json:"id"`
	Reason       string `json:"reason,omitempty"`
	LastTS       string `json:"last_ts"`
	CursorPath   string `json:"cursor_path,omitempty"`
	LastPollTime string `json:"last_poll_time,omitempty"`
}

// ThreadStatus reports per-thread polling state.
type ThreadStatus struct {
	ID          string `json:"id"`
	Channel     string `json:"channel"`
	ThreadTS    string `json:"thread_ts"`
	LastReplyTS string `json:"last_reply_ts"`
	CursorPath  string `json:"cursor_path,omitempty"`
	HasMeta     bool   `json:"has_meta"`
	LastEventAt string `json:"last_event_at,omitempty"`
	OwnerTask   string `json:"owner_task,omitempty"`
}

// newStatusCmd implements `slack-poller status [--json]`.
func newStatusCmd(opts *Options) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Print tracked channels, threads, cursors, and config path",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			stateDir := resolveStateDir(opts.StateDir)
			cfg, path, err := loadConfigOrExit(stateDir)
			if err != nil {
				return err
			}
			report, err := buildStatus(stateDir, path, cfg)
			if err != nil {
				return errf(ExitUsage, "%v", err)
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(report)
			}
			printStatusToWriter(cmd.OutOrStdout(), report)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false,
		"emit JSON instead of human-readable text")
	return cmd
}

// buildStatus assembles the report from on-disk state.
func buildStatus(stateDir, configPath string, cfg *slackpkg.Config) (*StatusReport, error) {
	r := &StatusReport{
		ConfigPath:       configPath,
		PollInterval:     cfg.PollInterval.Duration.String(),
		WriteUpdatePings: cfg.WriteUpdatePings != nil && *cfg.WriteUpdatePings,
	}

	cursors, err := slackpkg.NewCursorStore(filepath.Join(stateDir,
		"sources", "slack", "cursors"))
	if err != nil {
		return nil, fmt.Errorf("init cursors: %w", err)
	}

	for _, ch := range cfg.Watch.Channels {
		ts, err := cursors.GetChannelCursor(ch.ID)
		if err != nil {
			return nil, fmt.Errorf("get channel cursor %s: %w", ch.ID, err)
		}
		curPath := filepath.Join(stateDir, "sources", "slack",
			"cursors", "channels", ch.ID+".json")
		st := ChannelStatus{
			ID:         ch.ID,
			Reason:     ch.Reason,
			LastTS:     ts,
			CursorPath: curPath,
		}
		if info, statErr := os.Stat(curPath); statErr == nil {
			st.LastPollTime = info.ModTime().UTC().Format(time.RFC3339)
		}
		r.TrackedChannels = append(r.TrackedChannels, st)
	}

	// Discover tracked threads under threads/slack-*.
	threadsDir := filepath.Join(stateDir, "threads")
	entries, err := os.ReadDir(threadsDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read threads dir: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || !strings.HasPrefix(name, "slack-") {
			continue
		}
		ts, err := cursors.GetThreadCursor(name)
		if err != nil {
			return nil, fmt.Errorf("get thread cursor %s: %w", name, err)
		}
		channel, threadTS, _ := slackpkg.ParseThreadID(name)
		curPath := filepath.Join(stateDir, "sources", "slack",
			"cursors", "threads", name+".json")
		st := ThreadStatus{
			ID:          name,
			Channel:     channel,
			ThreadTS:    threadTS,
			LastReplyTS: ts,
			CursorPath:  curPath,
		}
		metaPath := filepath.Join(threadsDir, name, "meta.json")
		if metaBytes, mErr := os.ReadFile(metaPath); mErr == nil {
			st.HasMeta = true
			var m slackpkg.Meta
			if err := json.Unmarshal(metaBytes, &m); err == nil {
				st.LastEventAt = m.LastEventAt
				if m.OwnerTask != nil {
					st.OwnerTask = *m.OwnerTask
				}
			}
		}
		r.TrackedThreads = append(r.TrackedThreads, st)
	}
	sort.Slice(r.TrackedThreads, func(i, j int) bool {
		return r.TrackedThreads[i].ID < r.TrackedThreads[j].ID
	})
	return r, nil
}

// printStatusToWriter renders the human-readable form.
func printStatusToWriter(w io.Writer, r *StatusReport) {
	fmt.Fprintf(w, "config path: %s\n", r.ConfigPath)
	fmt.Fprintf(w, "poll interval: %s\n", r.PollInterval)
	fmt.Fprintf(w, "write_update_pings: %v\n", r.WriteUpdatePings)
	fmt.Fprintf(w, "\ntracked channels (%d):\n", len(r.TrackedChannels))
	for _, c := range r.TrackedChannels {
		fmt.Fprintf(w, "  %s  last_ts=%s  last_poll=%s  reason=%q\n",
			c.ID, displayOrDash(c.LastTS),
			displayOrDash(c.LastPollTime), c.Reason)
	}
	fmt.Fprintf(w, "\ntracked threads (%d):\n", len(r.TrackedThreads))
	for _, t := range r.TrackedThreads {
		owner := t.OwnerTask
		if owner == "" {
			owner = "-"
		}
		fmt.Fprintf(w, "  %s  last_reply_ts=%s  last_event_at=%s  owner=%s\n",
			t.ID, displayOrDash(t.LastReplyTS),
			displayOrDash(t.LastEventAt), owner)
	}
}

func displayOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
