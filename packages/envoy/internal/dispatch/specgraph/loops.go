package specgraph

import (
	"cmp"
	"maps"
	"slices"
	"strings"
)

// edge is one relation from a node to another: its target and the relation's name.
type edge struct {
	to, relation string
}

// findLoops is one cycle of each strongly connected part of the graph edges holds that holds a
// cycle (loopParts), ported from level_check.py's find_loops: the shortest cycle from the part's
// smallest id back to it, found breadth first with each node's edges in (target, relation) order.
// Each cycle starts and ends at that smallest id, and the cycles are sorted by it.
func findLoops(edges map[string][]edge) [][]string {
	for node := range edges {
		slices.SortFunc(edges[node], func(a, b edge) int { return cmp.Or(cmp.Compare(a.to, b.to), cmp.Compare(a.relation, b.relation)) })
	}
	var loops [][]string
	for _, part := range loopParts(edges) {
		loops = append(loops, shortestCycle(edges, slices.Min(part), part))
	}
	slices.SortFunc(loops, func(a, b []string) int { return cmp.Compare(a[0], b[0]) })
	return loops
}

// loopParts is each strongly connected part of the graph edges holds that holds a cycle, by
// Tarjan's algorithm: a part of two nodes or more, or one node with an edge to itself. Each node of
// a loop is in exactly one part, and a part is every node of its loops.
func loopParts(edges map[string][]edge) [][]string {
	index := map[string]int{}
	low := map[string]int{}
	onStack := map[string]bool{}
	var stack []string
	var parts [][]string
	var visit func(node string)
	visit = func(node string) {
		index[node], low[node] = len(index), len(index)
		stack = append(stack, node)
		onStack[node] = true
		for _, e := range edges[node] {
			if _, seen := index[e.to]; !seen {
				visit(e.to)
				low[node] = min(low[node], low[e.to])
			} else if onStack[e.to] {
				low[node] = min(low[node], index[e.to])
			}
		}
		if low[node] == index[node] {
			var part []string
			for {
				member := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[member] = false
				part = append(part, member)
				if member == node {
					break
				}
			}
			if len(part) > 1 || slices.ContainsFunc(edges[node], func(e edge) bool { return e.to == node }) {
				parts = append(parts, part)
			}
		}
	}
	for _, node := range slices.Sorted(maps.Keys(edges)) {
		if _, seen := index[node]; !seen {
			visit(node)
		}
	}
	return parts
}

// statementEdges is the parent and serves relations of each of statements to another of them, as
// the edges of Validate's loop rule: a relation to an id that is no statement of the list is none.
func statementEdges(statements []Statement) map[string][]edge {
	held := make(map[string]bool, len(statements))
	for _, s := range statements {
		held[s.ID] = true
	}
	edges := map[string][]edge{}
	for _, s := range statements {
		if held[s.Parent] {
			edges[s.ID] = append(edges[s.ID], edge{to: s.Parent, relation: "parent"})
		}
		for _, id := range s.Serves {
			if held[id] {
				edges[s.ID] = append(edges[s.ID], edge{to: id, relation: "serves"})
			}
		}
	}
	return edges
}

// shortestCycle is the shortest path from start back to start inside part, as the list
// start, relation, node, relation, …, start.
func shortestCycle(edges map[string][]edge, start string, part []string) []string {
	type step struct {
		from, relation string
	}
	previous := map[string]step{}
	queue := []string{start}
	var end step
	found := false
	for len(queue) > 0 && !found {
		node := queue[0]
		queue = queue[1:]
		for _, e := range edges[node] {
			if !slices.Contains(part, e.to) {
				continue
			}
			if e.to == start {
				end, found = step{from: node, relation: e.relation}, true
				break
			}
			if _, seen := previous[e.to]; !seen {
				previous[e.to] = step{from: node, relation: e.relation}
				queue = append(queue, e.to)
			}
		}
	}
	// Walk back from the last node to start, then reverse.
	path := []string{start, end.relation}
	for node := end.from; node != start; node = previous[node].from {
		path = append(path, node, previous[node].relation)
	}
	path = append(path, start)
	slices.Reverse(path)
	return path
}

// loopDetail writes a cycle as the loop rule's Detail names it: "a -[parent]-> b -[serves]-> a".
func loopDetail(cycle []string) string {
	var b strings.Builder
	b.WriteString(cycle[0])
	for i := 1; i+1 < len(cycle); i += 2 {
		b.WriteString(" -[" + cycle[i] + "]-> " + cycle[i+1])
	}
	return b.String()
}
