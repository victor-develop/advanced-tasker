package daemon

import (
	"context"
	"time"

	"github.com/victor-develop/advanced-tasker/internal/audit"
)

// AuditDaemon ticks the audit agent on a slow cadence.
type AuditDaemon struct {
	Bus      *Bus
	Cadence  time.Duration
	Escalate bool
}

// NewAuditDaemon constructs an AuditDaemon (4h default cadence per design/11).
func NewAuditDaemon(bus *Bus) *AuditDaemon {
	return &AuditDaemon{Bus: bus, Cadence: 4 * time.Hour, Escalate: true}
}

// Run blocks until ctx is cancelled. Runs an initial audit immediately
// (acceptance: short autopilot runs must produce a report file).
func (a *AuditDaemon) Run(ctx context.Context) error {
	if _, err := audit.Run(ctx, a.Bus.StateRoot, a.Bus.Driver, a.Escalate); err != nil {
		a.Bus.Log("audit-daemon initial: %v", err)
	}
	t := time.NewTicker(a.Cadence)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if _, err := audit.Run(ctx, a.Bus.StateRoot, a.Bus.Driver, a.Escalate); err != nil {
				a.Bus.Log("audit-daemon: %v", err)
			}
		}
	}
}
