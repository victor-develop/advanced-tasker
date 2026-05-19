package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/victor-develop/advanced-tasker/internal/gitops"
	"github.com/victor-develop/advanced-tasker/internal/inbox"
	"github.com/victor-develop/advanced-tasker/internal/outbox"
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
		if s.ProviderSend != nil {
			r, perr := s.ProviderSend(it)
			if perr != nil {
				return s.handleFailure(it, perr)
			}
			resp = r
		} else {
			// Default stub: pretend the provider accepted.
			resp = map[string]any{"provider": it.To, "stubbed": true}
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
