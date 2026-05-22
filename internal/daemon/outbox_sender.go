package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/victor-develop/advanced-tasker/internal/gitops"
	"github.com/victor-develop/advanced-tasker/internal/inbox"
	"github.com/victor-develop/advanced-tasker/internal/outbox"
	slackpkg "github.com/victor-develop/advanced-tasker/internal/slack"
	"github.com/victor-develop/advanced-tasker/internal/state"
)

// OutboxSender is the daemon that watches outbox/pending/, validates
// rate limits + duplicate guards, calls the provider (or simulates
// when DryRun=true), and moves items to sent/ or failed/.
type OutboxSender struct {
	Bus      *Bus
	Interval time.Duration
	DryRun   bool

	// ProviderSend is overridable for tests. If nil, the daemon uses
	// the default no-op stub (which logs intent and pretends success).
	ProviderSend func(*outbox.Item) (map[string]any, error)
}

// NewOutboxSender constructs an OutboxSender.
func NewOutboxSender(bus *Bus) *OutboxSender {
	return &OutboxSender{
		Bus:      bus,
		Interval: 2 * time.Second,
		DryRun:   bus.DryRunOutbox,
	}
}

// ProcessOnce processes a single pending outbox item by id. Used by
// integration tests that exercise sender behavior (sender_enabled gate,
// rate limits, deferrals) without spinning up the daemon goroutine.
func (s *OutboxSender) ProcessOnce(id string) error {
	return s.process(id)
}

// Run blocks until ctx is cancelled.
func (s *OutboxSender) Run(ctx context.Context) error {
	for {
		if err := sleepCtx(ctx, s.Interval); err != nil {
			return nil
		}
		ids, _ := outbox.ListByState(s.Bus.StateRoot, outbox.StatePending)
		for _, id := range ids {
			if err := s.process(id); err != nil {
				s.Bus.Log("outbox-sender %s: %v", id, err)
			}
		}
	}
}

func (s *OutboxSender) process(id string) error {
	root := s.Bus.StateRoot
	return state.WithStateLock(root, func() error {
		path := outbox.PathFor(root, outbox.StatePending, id)
		it, err := outbox.Read(path)
		if err != nil {
			return err
		}
		// Round-3 D5 safety gate: when outbox.sender_enabled=false,
		// the daemon RUNS but never touches the provider. Items in
		// pending/ are deflected to awaiting-human/ which already
		// requires an explicit `harness outbox approve` before any
		// future send attempt.
		if !outbox.SenderEnabled(root) {
			// stderr-style notice so operators see the deferral when
			// running autopilot from the foreground.
			fmt.Fprintf(os.Stderr, "outbox.sender_enabled=false: deferring O-%s to awaiting-human\n", strings.TrimPrefix(id, "O-"))
			s.Bus.Log("outbox-sender %s: sender_enabled=false → awaiting-human", id)
			if _, err := outbox.Move(root, it, outbox.StatePending, outbox.StateAwaitingHuman); err != nil {
				return err
			}
			return nil
		}
		lim, _ := outbox.LoadLimits(root)
		now := time.Now().UTC()
		if err := outbox.RateCheck(root, it, lim, now); err != nil {
			s.Bus.Log("outbox-sender %s: rate-limit %v", id, err)
			_, _ = inbox.AppendAnomaly(root, "outbox:"+id, err.Error())
			return nil // leave in pending; retry next loop
		}
		if err := outbox.DuplicateCheck(root, it, now); err != nil {
			s.Bus.Log("outbox-sender %s: %v", id, err)
			_, _ = inbox.AppendAnomaly(root, "outbox:"+id, err.Error())
			return nil
		}
		// Sentinel for idempotency across crashes.
		sentinel := path + ".sending"
		_ = os.WriteFile(sentinel, []byte(now.Format(time.RFC3339)), 0o644)
		defer os.Remove(sentinel)

		if s.DryRun {
			s.Bus.Log("[dry-run] would send %s to=%s thread=%s risk=%s", id, it.To, it.Ref.Thread, it.Risk)
			return nil
		}
		var resp map[string]any
		switch {
		case s.ProviderSend != nil:
			r, perr := s.ProviderSend(it)
			if perr != nil {
				return s.handleFailure(it, perr)
			}
			resp = r
		case it.To == "slack":
			// Real Slack send via slack-go. Resolve the bot token from
			// state/sources/slack/config.yaml (which itself supports env,
			// inline, or file token sources). Body is read from the
			// outbox item's body_file path (relative to state root).
			cfgPath := filepath.Join(root, "sources", "slack", "config.yaml")
			cfg, err := slackpkg.LoadConfig(cfgPath)
			if err != nil {
				return s.handleFailure(it, fmt.Errorf("slack config: %w", err))
			}
			token, err := cfg.ResolveToken()
			if err != nil {
				return s.handleFailure(it, fmt.Errorf("slack token: %w", err))
			}
			bodyText, err := readOutboxBody(root, it)
			if err != nil {
				return s.handleFailure(it, fmt.Errorf("body: %w", err))
			}
			r, perr := outbox.SlackProviderSend(token, bodyText, it)
			if perr != nil {
				return s.handleFailure(it, perr)
			}
			resp = r
		default:
			// No provider wired for this `to` value (e.g. github-pr-*
			// hasn't shipped a real provider in this polish round). The
			// item stays in pending with an anomaly so the operator
			// notices.
			s.Bus.Log("outbox-sender %s: no provider for to=%s", id, it.To)
			_, _ = inbox.AppendAnomaly(root, "outbox:"+id, fmt.Sprintf("no provider implemented for to=%s", it.To))
			return nil
		}
		it.SentAt = time.Now().UTC()
		it.SenderResponse = resp
		if _, err := outbox.Move(root, it, outbox.StatePending, outbox.StateSent); err != nil {
			return err
		}
		// Commit (sent/ is git-tracked).
		r := gitops.Repo{Dir: root}
		_ = r.Add(filepath.Join("outbox", "sent", id+".yaml"))
		if _, err := r.Commit(fmt.Sprintf("outbox send %s to %s", id, it.Ref.Thread)); err != nil && !errors.Is(err, gitops.ErrNothingToCommit) {
			return err
		}
		s.Bus.Log("outbox-sender %s: sent", id)
		return nil
	})
}

// readOutboxBody resolves the body content for an outbox item, preferring
// the inline `body` field then falling back to reading body_file (which
// may be absolute or relative to the state root).
func readOutboxBody(root string, it *outbox.Item) (string, error) {
	if it.Body != "" {
		return it.Body, nil
	}
	if it.BodyFile == "" {
		return "", fmt.Errorf("neither body nor body_file set")
	}
	path := it.BodyFile
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (s *OutboxSender) handleFailure(it *outbox.Item, perr error) error {
	root := s.Bus.StateRoot
	it.RetryCount++
	it.LastError = perr.Error()
	if it.RetryCount >= 3 {
		_, _ = outbox.Move(root, it, outbox.StatePending, outbox.StateFailed)
		_, _ = inbox.AppendAnomaly(root, "outbox:"+it.ID, fmt.Sprintf("permanent failure: %v", perr))
		return nil
	}
	// Re-write the pending file so retry_count persists.
	return outbox.Write(outbox.PathFor(root, outbox.StatePending, it.ID), it)
}
