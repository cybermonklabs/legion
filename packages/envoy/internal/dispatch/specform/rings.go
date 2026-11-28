package specform

import (
	"slices"

	"github.com/sjawhar/envoy/internal/dispatch/specform/ste"
)

// footnoteRing is one loop of the footnotes that the texts of a judgment hold: footnotes each of
// which holds each other one, as they refer to each other in turn, or one footnote alone. So all
// footnotes of a ring use the same verbs with the footnotes that they hold. notes is in the order of
// the texts (carry), and the first gives the ring's findings of the rule synonym-rotation
// (rotations). below is the ring of each footnote outside the ring that a footnote of it refers to,
// once for each reference.
type footnoteRing struct {
	notes []*Footnote
	below []int
}

// footnoteRings is the rings of order, the footnotes that the texts hold in the order of the texts,
// each footnote in one ring. A ring comes after each ring that it refers to, so a walk of the
// rings in order reads the rings that a ring holds before it. It is Tarjan's algorithm with a
// stack of its own, so a long chain of footnotes needs no deep call stack, and its work is in
// proportion to the footnotes and their references.
func footnoteRings(order []*Footnote) []footnoteRing {
	at := make(map[*Footnote]int, len(order))
	for index, note := range order {
		at[note] = index
	}
	count := len(order)
	seen, low, ringOf := make([]int, count), make([]int, count), make([]int, count)
	onStack := make([]bool, count)
	var stack []int
	var rings []footnoteRing
	type frame struct{ node, child int }
	next := 0
	visit := func(node int) {
		next++
		seen[node], low[node] = next, next
		stack = append(stack, node)
		onStack[node] = true
	}
	for root := range order {
		if seen[root] != 0 {
			continue
		}
		visit(root)
		frames := []frame{{root, 0}}
		for len(frames) > 0 {
			top := &frames[len(frames)-1]
			if notes := order[top.node].Notes; top.child < len(notes) {
				child := at[notes[top.child]]
				top.child++
				switch {
				case seen[child] == 0:
					visit(child)
					frames = append(frames, frame{child, 0})
				case onStack[child]:
					low[top.node] = min(low[top.node], seen[child])
				}
				continue
			}
			node := top.node
			frames = frames[:len(frames)-1]
			if len(frames) > 0 {
				parent := frames[len(frames)-1].node
				low[parent] = min(low[parent], low[node])
			}
			if low[node] != seen[node] {
				continue
			}
			var ring footnoteRing
			for {
				member := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[member] = false
				ringOf[member] = len(rings)
				ring.notes = append(ring.notes, order[member])
				if member == node {
					break
				}
			}
			slices.SortFunc(ring.notes, func(a, b *Footnote) int { return at[a] - at[b] })
			rings = append(rings, ring)
		}
	}
	ring := make(map[*Footnote]int, count)
	for index, note := range order {
		ring[note] = ringOf[index]
	}
	for id := range rings {
		for _, note := range rings[id].notes {
			for _, child := range note.Notes {
				if below := ring[child]; below != id {
					rings[id].below = append(rings[id].below, below)
				}
			}
		}
	}
	return rings
}

// usesAll is each verb of each synonym group of which part uses each verb that whole uses, of the
// groups that whole uses a verb of.
func usesAll(whole, part ste.Verbs) ste.Verbs {
	return whole.Groups() &^ (whole ^ part).Groups()
}
