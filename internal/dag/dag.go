// Package dag does cycle detection over the "blocked-on" dependency
// graph. Edges point from a blocked task → its blocker (the task it
// needs to finish first). The CLI must reject any new edge that would
// close a cycle.
package dag

import (
	"fmt"
	"sort"

	"github.com/victor-develop/advanced-tasker/internal/store"
)

// Graph indexes tasks by ID and exposes blocked_on lookups.
type Graph struct {
	edges map[string][]string // task -> list of blockers
}

// FromStatuses builds a graph from a snapshot of statuses.
func FromStatuses(statuses []*store.Status) *Graph {
	g := &Graph{edges: map[string][]string{}}
	for _, s := range statuses {
		g.edges[s.ID] = append([]string(nil), s.BlockedOn...)
	}
	return g
}

// AddEdge speculatively adds a blocked-on edge (from → to). Returns
// (cycle, cyclePath, error). If cycle is true, no mutation persists.
func (g *Graph) AddEdge(from, to string) (bool, []string, error) {
	if from == to {
		return true, []string{from, to}, nil
	}
	// Adding from → to (meaning `from` is blocked on `to`). A cycle
	// exists iff `to` can already reach `from` via existing edges.
	if path, ok := g.findPath(to, from, nil, map[string]bool{}); ok {
		full := append([]string{from}, path...)
		return true, full, nil
	}
	g.edges[from] = appendUnique(g.edges[from], to)
	return false, nil, nil
}

// FindCycles returns one cycle per strongly-connected component of size
// > 1 (or self-loops). Empty result means the graph is a DAG.
func (g *Graph) FindCycles() [][]string {
	var cycles [][]string
	seen := map[string]bool{}
	for _, node := range g.sortedKeys() {
		if seen[node] {
			continue
		}
		path := []string{}
		onPath := map[string]bool{}
		g.dfs(node, &cycles, seen, onPath, &path)
	}
	return cycles
}

func (g *Graph) dfs(node string, cycles *[][]string, seen, onPath map[string]bool, path *[]string) {
	seen[node] = true
	onPath[node] = true
	*path = append(*path, node)
	for _, nb := range g.edges[node] {
		if onPath[nb] {
			// Found a cycle. Extract the subpath from nb onwards.
			start := 0
			for i, p := range *path {
				if p == nb {
					start = i
					break
				}
			}
			cyc := append([]string{}, (*path)[start:]...)
			cyc = append(cyc, nb)
			*cycles = append(*cycles, cyc)
			continue
		}
		if !seen[nb] {
			g.dfs(nb, cycles, seen, onPath, path)
		}
	}
	*path = (*path)[:len(*path)-1]
	onPath[node] = false
}

// findPath returns the (start→...→target) path of node names if one
// exists in the graph.
func (g *Graph) findPath(start, target string, path []string, seen map[string]bool) ([]string, bool) {
	if seen[start] {
		return nil, false
	}
	seen[start] = true
	path = append(path, start)
	if start == target {
		return path, true
	}
	for _, nb := range g.edges[start] {
		if p, ok := g.findPath(nb, target, append([]string(nil), path...), seen); ok {
			return p, true
		}
	}
	return nil, false
}

func (g *Graph) sortedKeys() []string {
	out := make([]string, 0, len(g.edges))
	for k := range g.edges {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func appendUnique(xs []string, x string) []string {
	for _, y := range xs {
		if y == x {
			return xs
		}
	}
	return append(xs, x)
}

// FormatCycle returns a human-readable cycle string.
func FormatCycle(c []string) string {
	if len(c) == 0 {
		return ""
	}
	s := c[0]
	for _, n := range c[1:] {
		s += " → " + n
	}
	return s
}

// Upstream returns every task reachable from start via blocked-on edges
// (i.e. things `start` is transitively blocked on).
func (g *Graph) Upstream(start string) []string {
	out := []string{}
	seen := map[string]bool{}
	g.collect(start, seen, &out, false)
	// remove start itself
	filtered := out[:0]
	for _, x := range out {
		if x != start {
			filtered = append(filtered, x)
		}
	}
	sort.Strings(filtered)
	return filtered
}

// Downstream returns every task that is transitively blocked on start.
func (g *Graph) Downstream(start string) []string {
	// Invert and DFS.
	inv := map[string][]string{}
	for from, tos := range g.edges {
		for _, to := range tos {
			inv[to] = append(inv[to], from)
		}
	}
	out := []string{}
	seen := map[string]bool{}
	var walk func(string)
	walk = func(n string) {
		for _, nb := range inv[n] {
			if seen[nb] {
				continue
			}
			seen[nb] = true
			out = append(out, nb)
			walk(nb)
		}
	}
	walk(start)
	sort.Strings(out)
	return out
}

func (g *Graph) collect(n string, seen map[string]bool, out *[]string, inclusive bool) {
	if seen[n] {
		return
	}
	seen[n] = true
	if inclusive || len(*out) > 0 {
		*out = append(*out, n)
	} else {
		// First call: include start so callers can filter.
		*out = append(*out, n)
	}
	for _, nb := range g.edges[n] {
		g.collect(nb, seen, out, true)
	}
}

// Error helpers ---------------------------------------------------------

// CycleError is returned by callers that want a structured cycle error.
type CycleError struct {
	From  string
	To    string
	Cycle []string
}

func (e *CycleError) Error() string {
	return fmt.Sprintf("would create dependency cycle: %s", FormatCycle(e.Cycle))
}
