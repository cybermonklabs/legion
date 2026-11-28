package replay

import (
	"cmp"
	"maps"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// The streams of the run's seed: each draw of a replay reads a source of its own, seeded by the
// run's seed on its own stream, so a draw of one kind never moves the draws of another: the
// experiments' (newChain), the never-started scenario's, a stretch's conflict's, at its end or at a
// boot inside it, and the loop of a conflict it made. The last two are seeded afresh at each
// stretch's draw, by the stretch's place as well (driver.seedDraw), so no draw of a conflict reads
// the draws before it.
const (
	experimentStream   = 0x7265706c6179     // "replay"
	conflictStream     = 0x636f6e666c696374 // "conflict"
	loopStream         = 0x6c6f6f70         // "loop"
	neverStartedStream = 0x6e65766572       // "never"
)

// cutSlotWaits is the branch's AfterSendBack less-slot-wait (assumption 13): the first piece of
// the work of each visit a send-back began takes its recorded length less the visit's recorded
// wait for a slot, from the move back into the visit to its worker start, the bound of the time a
// worker that never went cold would take; a visit whose worker started at the move keeps its
// length.
func (s *script) cutSlotWaits() {
	for _, pieces := range s.pieces {
		for _, p := range pieces {
			visit := p.visit
			if visit == nil || visit.Before || visit.In == nil || visit.Segment == nil || !phase.MovesBack(visit.In.From, visit.In.To) ||
				!p.start.Equal(visit.Segment.Start) {
				continue
			}
			if wait := visit.Segment.Start.Sub(visit.Enter); wait > 0 {
				p.cut = wait
			}
		}
	}
}

// neverStartedIn says whether the record never started key's work: an issue of t with no slot and
// no work segment, or one t does not hold.
func neverStartedIn(t Trace, key string) bool {
	iss, ok := t.Issues[key]
	if !ok {
		return true
	}
	if len(iss.Slots) > 0 {
		return false
	}
	return !slices.ContainsFunc(iss.Visits, func(visit Visit) bool { return visit.Segment != nil })
}

// startedIn says whether the record started iss inside t's window: the earlier of its first slot
// and its first work segment begins at or after the window's start and before the end of its
// inputs (Trace.End), its run-out's where it holds one, whose recorded work a replay runs as it runs
// the window's. An issue whose first slot it held from before the window's reach (SlotSpan.Before)
// did not start in it.
func startedIn(t Trace, iss *IssueTrace) bool {
	var first time.Time
	if len(iss.Slots) > 0 {
		if iss.Slots[0].Before {
			return false
		}
		first = iss.Slots[0].Start
	}
	for _, visit := range iss.Visits {
		if visit.Segment != nil && (first.IsZero() || visit.Segment.Start.Before(first)) {
			first = visit.Segment.Start
		}
	}
	return !first.IsZero() && !first.Before(t.From) && first.Before(t.End)
}

// finishedIn says whether the record moved iss to done inside t's window or its run-out
// (startedIn).
func finishedIn(t Trace, iss *IssueTrace) bool {
	return slices.ContainsFunc(iss.Visits, func(visit Visit) bool {
		return visit.Out != nil && visit.Out.To == phase.Done && !visit.Out.At.Before(t.From) && visit.Out.At.Before(t.End)
	})
}

// treeRoot says whether iss entered the workflow as a tree of its own, a root, rather than as a
// child of another tree, by its first entry.
func treeRoot(iss *IssueTrace) bool {
	return len(iss.Entries) == 0 || iss.Entries[0].Tree == "" || iss.Entries[0].Tree == iss.Key
}

// DrawnWork is how many issues a never-started scenario drawn or drawn-done gave the work of
// another issue (script.drawNeverStarted), by the pool the issue drawn from was drawn in: the
// issues of the same tree, those of the same priority and repository, or the whole window.
type DrawnWork struct {
	Tree               int `json:"tree"`
	PriorityRepository int `json:"priorityRepository"`
	Window             int `json:"window"`
}

// drawNeverStarted is the branch's never-started scenario drawn or drawn-done (assumption 8):
// each issue of t's project handed to Legion that the record never started is given the work of an
// issue drawn with source from the issues the record started in the window (drawn), or from those
// it finished in it (drawn-done), of the same kind — a root for a root and a child for a child,
// where the window holds one — and among those from the issues of its own tree, where its tree holds
// any; else from those of its priority and its repository, where any is; else from all of them.
// Each piece of that work stands at its recorded offsets from the new issue's own starts, unended
// where the drawn issue's was. Where the window holds no issue to draw from, the issue keeps no
// work, as under none. Each issue draws once, in the order of their keys. It answers how many drew
// from each pool.
func (s *script) drawNeverStarted(t Trace, scenario string, source *rand.Rand) DrawnWork {
	var drawn DrawnWork
	var roots, children, pool []string
	for _, key := range slices.Sorted(maps.Keys(t.Issues)) {
		iss := t.Issues[key]
		if iss.Project != t.Project || len(s.pieces[key]) == 0 || !startedIn(t, iss) {
			continue
		}
		if scenario == neverStartedDrawnDone && !finishedIn(t, iss) {
			continue
		}
		if treeRoot(iss) {
			roots = append(roots, key)
		} else {
			children = append(children, key)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(t.Issues)) {
		iss := t.Issues[key]
		if iss.Project != t.Project || len(iss.Entries) == 0 || !neverStartedIn(t, key) {
			continue
		}
		kind := children
		if treeRoot(iss) {
			kind = roots
		}
		if len(kind) == 0 {
			kind = slices.Concat(roots, children)
		}
		if len(kind) == 0 {
			continue
		}
		pool = pool[:0]
		count := &drawn.Tree
		for _, other := range kind {
			if tree := iss.Entries[0].Tree; tree != "" && entryTree(t.Issues[other]) == tree {
				pool = append(pool, other)
			}
		}
		if len(pool) == 0 {
			count = &drawn.PriorityRepository
			for _, other := range kind {
				if o := t.Issues[other]; o.Priority == iss.Priority && o.Repo == iss.Repo {
					pool = append(pool, other)
				}
			}
		}
		candidates := pool
		if len(candidates) == 0 {
			candidates, count = kind, &drawn.Window
		}
		s.pieces[key] = clonePieces(s.pieces[candidates[source.IntN(len(candidates))]], key)
		if s.drawnWork == nil {
			s.drawnWork = map[string]bool{}
		}
		s.drawnWork[key] = true
		*count++
	}
	return drawn
}

// entryTree is the tree iss first entered the workflow in, "" where the trace holds no entry of it.
func entryTree(iss *IssueTrace) string {
	if len(iss.Entries) == 0 {
		return ""
	}
	return iss.Entries[0].Tree
}

// clonePieces is pieces, another issue's recorded work, given to key (piece.drawn): each piece
// with its phase, its role, its recorded length and its end, opened by key's own starts, and each
// act of its workers at its offset — the turns, a registration, a claim's failure, a close of the
// root and a ready mark, each of key — but for the Dispatch events the copied issue's workers made
// and the roots its architect took into its tree, which stay the copied issue's. A clone of a piece
// under way at the window's start (opensAtSeed) opens as the piece would have opened had it not
// been (openingOf): on the move into its phase where that is accepting's or a workerless visit's,
// and otherwise on its role's start.
func clonePieces(pieces []*piece, key string) []*piece {
	clones := make([]*piece, 0, len(pieces))
	for _, p := range pieces {
		clone := &piece{issue: key, index: len(clones), visit: p.visit, phase: p.phase, role: p.role, opens: p.opens, start: p.start,
			end: p.end, ends: p.ends, hold: p.hold, m: p.m, cut: p.cut, drawn: true}
		clone.source = cmp.Or(p.source, p.issue)
		if clone.opens == opensAtSeed {
			clone.opens = openingOf(clone.phase, clone.visit)
		}
		for _, a := range p.acts {
			switch a.kind {
			case actEvent, actAdoption:
				continue
			case actClaimFailed:
				a.failed.Claim.Issue = key
			}
			clone.acts = append(clone.acts, a)
		}
		clones = append(clones, clone)
	}
	return clones
}

// moveGeneration is the generation the recorded move that ends p is entered at: the record's
// generation of p's issue at the move, or, for a drawn piece and for a piece of a generation its
// issue left in the replay before the piece opened (piece.leftUnrun), the generation the issue
// stands at in the replay.
func (d *driver) moveGeneration(p *piece, move *Move) (uint64, error) {
	if !p.drawn && !p.leftUnrun {
		return d.generationAt(p.issue, move.At), nil
	}
	issue, err := d.recorded(p.issue)
	if err != nil || issue == nil {
		return 1, err
	}
	return issue.Generation, nil
}

// gateFor is gate, a registration of p's work, as p's issue registers it: a drawn piece registers
// its own issue's document, its primary artifact as the replay's copy of Dispatch shows it, in
// place of the copied issue's.
func (d *driver) gateFor(p *piece, gate GateFact) GateFact {
	if !p.drawn {
		return gate
	}
	if iss, ok := d.shadow.issues[p.issue]; ok {
		if artifact := iss.str("primary_artifact_id"); artifact != "" {
			gate.Artifact = artifact
		}
	}
	return gate
}
