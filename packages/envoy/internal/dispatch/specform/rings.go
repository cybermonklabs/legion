package specform

import (
	"container/heap"
	"slices"

	"github.com/sjawhar/envoy/internal/dispatch/specform/ste"
)

// footnoteRing is one loop of the footnotes that the texts of a judgment hold: footnotes each of
// which holds each other one, as they refer to each other in turn, or one footnote alone. So all
// footnotes of a ring use the same verbs with the footnotes that they hold. notes is in the order of
// the texts (carry), and the first gives the ring's findings of the rule synonym-rotation
// (rotations). below is the ring of each footnote outside the ring that a footnote of it refers to,
// and above the ring of each footnote outside it that refers to a footnote of it, each once for
// each reference.
type footnoteRing struct {
	notes        []*Footnote
	below, above []int
}

// footnoteRings is the rings of order, the footnotes that the texts hold in the order of the texts,
// each footnote in one ring, and the ring of each footnote. A ring comes after each ring that it
// refers to, so a walk of the rings in order reads the rings that a ring holds before it. It is
// Tarjan's algorithm with a stack of its own, so a long chain of footnotes needs no deep call
// stack, and its work is in proportion to the footnotes and their references.
func footnoteRings(order []*Footnote) ([]footnoteRing, map[*Footnote]int) {
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
					rings[below].above = append(rings[below].above, id)
				}
			}
		}
	}
	return rings, ring
}

// usesAll is each verb of each synonym group of which part uses each verb that whole uses, of the
// groups that whole uses a verb of.
func usesAll(whole, part ste.Verbs) ste.Verbs {
	return whole.Groups() &^ (whole ^ part).Groups()
}

// verbCount is how many times each set of verbs of one synonym group stands, as the words of the
// footnotes of a ring use them, or as the footnotes that a ring or a text refers to use them with
// the footnotes that they hold. A set counted no times has no key.
type verbCount map[ste.Verbs]int

// add counts verbs by more times, or by fewer for a negative by.
func (count verbCount) add(verbs ste.Verbs, by int) {
	if count[verbs] += by; count[verbs] == 0 {
		delete(count, verbs)
	}
}

// union is every verb of a set that stands.
func (count verbCount) union() ste.Verbs {
	var all ste.Verbs
	for verbs := range count {
		all |= verbs
	}
	return all
}

// ringGives is the verbs of one synonym group that a ring of footnotes gives its finding of the
// rule synonym-rotation in (rotations), from verbs, the verbs of the group that its footnotes use
// with the footnotes that they hold, own, the sets that its footnotes use in their own words, and
// below, the sets of the footnotes outside it that they refer to: two verbs or more, where neither
// the words of one footnote of the ring, which are Lint's finding of that footnote, nor one
// footnote that the ring refers to uses each of them, which is that footnote's finding or one that
// it holds.
func ringGives(verbs ste.Verbs, own, below verbCount) ste.Verbs {
	if verbs.Count() < 2 || own[verbs] > 0 || below[verbs] > 0 {
		return 0
	}
	return verbs
}

// textGives is the verbs of one synonym group that a text of prose gives its finding of the rule
// synonym-rotation in with the footnotes that it refers to (ste.Rotations): own is the verbs of the
// group that its words use, and below the sets that its footnotes use with the footnotes that they
// hold.
func textGives(own ste.Verbs, below verbCount) ste.Verbs {
	verbs := own | below.union()
	if verbs.Count() < 2 || own == verbs || below[verbs] > 0 {
		return 0
	}
	return verbs
}

// ringGroup is one ring of footnotes, or one text, by its place in the judgment, and one synonym
// group, by all of its verbs.
type ringGroup struct {
	at    int
	group ste.Verbs
}

// ringCount is what rotationCanChange reads of one ring of footnotes for one synonym group: the
// sets of the group that the words of its footnotes use (own), and that the footnotes outside it
// that they refer to use with the footnotes that they hold (below).
type ringCount struct {
	own, below verbCount
}

// ringQueue is the rings whose verbs a way out can change, by their place in the order of the
// rings, the least first: a ring comes after each ring that it refers to, so the walk reads each
// ring once the rings that it refers to are read.
type ringQueue []int

func (queue ringQueue) Len() int           { return len(queue) }
func (queue ringQueue) Less(a, b int) bool { return queue[a] < queue[b] }
func (queue ringQueue) Swap(a, b int)      { queue[a], queue[b] = queue[b], queue[a] }
func (queue *ringQueue) Push(ring any)     { *queue = append(*queue, ring.(int)) }
func (queue *ringQueue) Pop() any {
	last := (*queue)[len(*queue)-1]
	*queue = (*queue)[:len(*queue)-1]
	return last
}

// rotationCanChange reports whether a way out of a finding of the footnote note, which changes the
// verbs of the synonym groups that its own words use from before to after, gives a finding of the
// rule synonym-rotation of the verbs of footnotes that the judgment does not give as many times,
// word for word as the refusal names it: of a ring of footnotes with the footnotes that it refers
// to (rotations), named in the place of the footnote that gives it, or of a text of prose with
// the footnotes that it refers to (held). The finding of the footnote's own words is wholeText's to
// read. It reads each group that the way out changes on its own, as each such finding is of one
// group (rotationGives).
func (j *footnoteJudge) rotationCanChange(note *Footnote, before, after ste.Verbs) bool {
	if j.counted == nil {
		j.carry()
	}
	for _, group := range (before ^ after).GroupsEach() {
		if j.rotationGives(note, group, before&group, after&group) {
			return true
		}
	}
	return false
}

// rotationGives reports whether the footnote note, whose own words use the verbs before of group
// and after the way out the verbs after, gives a finding of group that the judgment does not give
// as many times (rotationCanChange). Only the ring of note and the rings that hold it can change
// their verbs, so it walks up from the ring of note, in the order of the rings, and stops at each
// ring whose verbs stay as they were. Each ring keeps how many times each set of the group stands
// in its own words and below it (ringCount), so a ring whose verbs change, or that refers to one
// whose verbs change, is read in the sets of the group that it holds, not in each footnote: a
// footnote that many texts or footnotes refer to costs each of them one step, and a ring whose
// verbs stay as they were costs nothing above it. So the ring of each footnote that loses its last
// verb of a group below a ring is walked once for that verb, and the work of a judgment grows with
// its footnotes and their references (the work table's rows of a chain of footnotes and of one claim
// that refers to many footnotes, each of whose ways out takes a verb away).
func (j *footnoteJudge) rotationGives(note *Footnote, group, before, after ste.Verbs) bool {
	start := j.ring[note]
	own := j.ringCount(start, group).own.clone()
	own.add(before, -1)
	own.add(after, 1)
	changed := map[int]ste.Verbs{}
	below, texts := map[int]verbCount{}, map[int]verbCount{}
	queue, queued := ringQueue{start}, map[int]bool{start: true}
	for queue.Len() > 0 {
		id := heap.Pop(&queue).(int)
		now := j.ringCount(id, group).own.union()
		if id == start {
			now = own.union()
		}
		if over, ok := below[id]; ok {
			now |= over.union()
		} else {
			now |= j.ringCount(id, group).below.union()
		}
		was := j.verbs[j.rings[id].notes[0]] & group
		if now == was {
			continue
		}
		changed[id] = now
		for _, parent := range j.rings[id].above {
			if _, ok := below[parent]; !ok {
				below[parent] = j.ringCount(parent, group).below.clone()
			}
			below[parent].add(was, -1)
			below[parent].add(now, 1)
			if !queued[parent] {
				queued[parent] = true
				heap.Push(&queue, parent)
			}
		}
		for _, text := range j.ringTexts()[id] {
			key := ringGroup{text, group}
			if _, ok := texts[text]; !ok {
				texts[text] = j.textCount(key).clone()
			}
			texts[text].add(was, -1)
			texts[text].add(now, 1)
		}
	}
	added, removed := map[string]int{}, map[string]int{}
	tally := func(was, now ste.Verbs, key func(ste.Verbs) string) {
		if was == now {
			return
		}
		if was != 0 {
			removed[key(was)]++
		}
		if now != 0 {
			added[key(now)]++
		}
	}
	for id := range queued {
		count := j.ringCount(id, group)
		ownNow, belowNow := count.own, count.below
		if id == start {
			ownNow = own
		}
		if over, ok := below[id]; ok {
			belowNow = over
		}
		was := j.verbs[j.rings[id].notes[0]] & group
		now, ok := changed[id]
		if !ok {
			now = was
		}
		place, held := j.formPlace(j.rings[id].notes[0])
		if !held {
			continue
		}
		tally(ringGives(was, count.own, count.below), ringGives(now, ownNow, belowNow), func(words ste.Verbs) string {
			return place + ": " + rotationBreaks(words, "the footnotes that it refers to")[0].detail
		})
	}
	for text, over := range texts {
		own := j.textVerbs(text) & group
		tally(textGives(own, j.textCount(ringGroup{text, group})), textGives(own, over), func(words ste.Verbs) string {
			return rotationBreaks(words, "its footnotes")[0].detail
		})
	}
	for key, count := range added {
		if count > removed[key] {
			return true
		}
	}
	return false
}

// clone is a copy of count, which a walk changes in place of count.
func (count verbCount) clone() verbCount {
	copied := make(verbCount, len(count))
	for verbs, times := range count {
		copied[verbs] = times
	}
	return copied
}

// ringCount is the sets of group that the ring id holds (ringCount), read the first time that a
// way out asks.
func (j *footnoteJudge) ringCount(id int, group ste.Verbs) ringCount {
	key := ringGroup{id, group}
	if count, ok := j.ringCounts[key]; ok {
		return count
	}
	count := ringCount{own: verbCount{}, below: verbCount{}}
	for _, note := range j.rings[id].notes {
		count.own.add(j.judgment(note).verbs&group, 1)
	}
	for _, child := range j.rings[id].below {
		count.below.add(j.verbs[j.rings[child].notes[0]]&group, 1)
	}
	if j.ringCounts == nil {
		j.ringCounts = map[ringGroup]ringCount{}
	}
	j.ringCounts[key] = count
	return count
}

// textCount is the sets of a group that the footnotes that a text refers to use with the footnotes
// that they hold, once for each reference, read the first time that a way out asks.
func (j *footnoteJudge) textCount(key ringGroup) verbCount {
	if count, ok := j.textCounts[key]; ok {
		return count
	}
	count := verbCount{}
	for _, note := range j.texts[key.at].Notes {
		count.add(j.verbs[note]&key.group, 1)
	}
	if j.textCounts == nil {
		j.textCounts = map[ringGroup]verbCount{}
	}
	j.textCounts[key] = count
	return count
}

// textVerbs is the verbs of the synonym groups that the words of the text at index use, read the
// first time that a way out asks.
func (j *footnoteJudge) textVerbs(index int) ste.Verbs {
	if verbs, ok := j.textVerbsRead[index]; ok {
		return verbs
	}
	_, verbs := ste.LintVerbs(oneLine(j.texts[index].Body))
	if j.textVerbsRead == nil {
		j.textVerbsRead = map[int]ste.Verbs{}
	}
	j.textVerbsRead[index] = verbs
	return verbs
}

// ringTexts is, for each ring of footnotes, the index of each text of prose that refers to a
// footnote of it, once for each reference: the texts whose findings of the rule synonym-rotation
// read the verbs of the ring (held).
func (j *footnoteJudge) ringTexts() [][]int {
	if j.textsOfRing == nil {
		j.textsOfRing = make([][]int, len(j.rings))
		for index, text := range j.texts {
			if !prose(text.Kind) || text.Kind == KindLine {
				continue
			}
			for _, note := range text.Notes {
				j.textsOfRing[j.ring[note]] = append(j.textsOfRing[j.ring[note]], index)
			}
		}
	}
	return j.textsOfRing
}

// formPlace is the place that the findings of the form rules of note name (placeWords), and
// whether a text of prose holds note, so that a finding of note stands at all: the first text of
// prose to hold it gives them (owned), through the footnote that it refers to directly.
func (j *footnoteJudge) formPlace(note *Footnote) (string, bool) {
	if j.formPlaces == nil {
		j.formPlaces = map[*Footnote]string{}
		given := map[*Footnote]bool{}
		for _, text := range j.texts {
			if prose(text.Kind) && text.Kind != KindLine {
				j.walk(text, given, func(via, each *Footnote) { j.formPlaces[each] = j.placeWords(via, each) })
			}
		}
	}
	place, ok := j.formPlaces[note]
	return place, ok
}
