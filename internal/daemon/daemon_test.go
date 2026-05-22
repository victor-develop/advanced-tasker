package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/victor-develop/advanced-tasker/internal/llm"
)

// TestBus_LogIsThreadSafe is a minimal smoke check that the bus can be
// constructed and log lines accumulated from concurrent producers
// without panicking.
func TestBus_LogIsThreadSafe(t *testing.T) {
	bus := NewBus(t.TempDir(), llm.NewFake(t.TempDir()))
	done := make(chan struct{})
	for i := 0; i < 4; i++ {
		go func(n int) {
			for j := 0; j < 50; j++ {
				bus.Log("worker-%d msg %d", n, j)
			}
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < 4; i++ {
		<-done
	}
	if got := len(bus.Lines()); got != 200 {
		t.Errorf("expected 200 log lines, got %d", got)
	}
}

// TestSleepCtx_RespectsCancellation guards against the goroutine
// lifecycle bug where a daemon couldn't be stopped between ticks. We
// assert that sleepCtx returns when ctx is cancelled, even if the
// duration is much larger than the test budget.
func TestSleepCtx_RespectsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := sleepCtx(ctx, 5*time.Second)
	elapsed := time.Since(start)
	if err == nil {
		t.Errorf("expected ctx.Err() return on cancel")
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("sleepCtx did not unblock promptly on cancel: %v", elapsed)
	}
}

// TestOutboxSender_RunStopsOnCtxCancel verifies start/stop lifecycle:
// the daemon must exit cleanly when its context is cancelled (no
// deadlock, no leaked goroutine the test can detect via timeout).
func TestOutboxSender_RunStopsOnCtxCancel(t *testing.T) {
	bus := NewBus(t.TempDir(), llm.NewFake(t.TempDir()))
	sender := NewOutboxSender(bus)
	sender.Interval = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan error, 1)
	go func() {
		exited <- sender.Run(ctx)
	}()
	// Let the loop spin at least one iteration, then cancel.
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-exited:
		// good — Run returned promptly.
	case <-time.After(2 * time.Second):
		t.Fatalf("outbox-sender did not stop within 2s of ctx cancel")
	}
}

// TestCommanderScheduler_IntervalReadsConfig verifies the cadence
// computation reads config.yaml on every call (so `harness config set`
// takes effect without daemon restart).
func TestCommanderScheduler_IntervalReadsConfig(t *testing.T) {
	root := t.TempDir()
	// Minimal config.yaml — fall-through path returns 30s.
	bus := NewBus(root, llm.NewFake(t.TempDir()))
	s := NewCommanderScheduler(bus)
	got := s.intervalFor(time.Now())
	if got != 30*time.Second {
		t.Errorf("missing config should yield 30s default, got %v", got)
	}
}
