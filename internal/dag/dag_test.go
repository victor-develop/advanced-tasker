package dag

import (
	"testing"

	"github.com/victor-develop/advanced-tasker/internal/store"
)

func mk(id string, blockers ...string) *store.Status {
	return &store.Status{ID: id, BlockedOn: blockers, State: store.StateReady}
}

func TestAddEdge_NoCycle(t *testing.T) {
	g := FromStatuses([]*store.Status{mk("T-1"), mk("T-2"), mk("T-3")})
	if cycle, _, err := g.AddEdge("T-1", "T-2"); err != nil || cycle {
		t.Errorf("expected no cycle, got cycle=%v err=%v", cycle, err)
	}
	if cycle, _, err := g.AddEdge("T-2", "T-3"); err != nil || cycle {
		t.Errorf("expected no cycle, got cycle=%v err=%v", cycle, err)
	}
}

func TestAddEdge_DirectCycle(t *testing.T) {
	g := FromStatuses([]*store.Status{mk("T-1"), mk("T-2", "T-1")})
	cycle, path, err := g.AddEdge("T-1", "T-2")
	if err != nil {
		t.Fatal(err)
	}
	if !cycle {
		t.Errorf("expected cycle (T-2 already blocks T-1)")
	}
	if len(path) < 2 {
		t.Errorf("expected cycle path, got %v", path)
	}
}

func TestAddEdge_TransitiveCycle(t *testing.T) {
	g := FromStatuses([]*store.Status{
		mk("T-1"),
		mk("T-2", "T-1"),
		mk("T-3", "T-2"),
	})
	cycle, _, err := g.AddEdge("T-1", "T-3")
	if err != nil {
		t.Fatal(err)
	}
	if !cycle {
		t.Errorf("expected transitive cycle (T-1→T-3, T-3→T-2→T-1)")
	}
}

func TestAddEdge_SelfLoop(t *testing.T) {
	g := FromStatuses([]*store.Status{mk("T-1")})
	cycle, _, _ := g.AddEdge("T-1", "T-1")
	if !cycle {
		t.Error("self-loop should be reported as cycle")
	}
}

func TestFindCycles_OnExistingGraph(t *testing.T) {
	g := FromStatuses([]*store.Status{
		mk("T-1", "T-2"),
		mk("T-2", "T-3"),
		mk("T-3", "T-1"),
	})
	cycles := g.FindCycles()
	if len(cycles) == 0 {
		t.Fatal("expected cycle, got none")
	}
}

func TestUpstreamDownstream(t *testing.T) {
	g := FromStatuses([]*store.Status{
		mk("T-1", "T-2"),
		mk("T-2", "T-3"),
		mk("T-3"),
		mk("T-4", "T-2"),
	})
	up := g.Upstream("T-1")
	if !contains(up, "T-2") || !contains(up, "T-3") {
		t.Errorf("upstream of T-1: %v", up)
	}
	down := g.Downstream("T-3")
	if !contains(down, "T-2") || !contains(down, "T-1") || !contains(down, "T-4") {
		t.Errorf("downstream of T-3: %v", down)
	}
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
