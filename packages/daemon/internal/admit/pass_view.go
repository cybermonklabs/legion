package admit

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/signals"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// PassView is the project as one admission pass leaves it: what the pass read and judged, and the
// slots it moved. A replay of a recorded window measures its own run from these views (its
// snapshot timeline), so it reads the states the daemon's own pass decided from, with no read of
// its own; nothing in the daemon observes them.
//
// Each part is the pass's own read, made again by the pass itself wherever it wrote something
// that made it stale (reread), and so is the line, but for the members the last round of the
// promotion promoted, which the pass reads no more and which are left out of Line here: they hold
// what their promotion gave them and wait in the line no longer. Issues is the records the pass
// last read, so a record its last round wrote (a promoted root's status, a child's first phase)
// stands as it was read; Slots holds every slot the pass gave out.
type PassView struct {
	// Project is the pass's Dispatch project, Cap the cap it ran with and Rule the slot rule it
	// judged by.
	Project string
	Cap     int
	Rule    record.SlotRule
	// At is the pass's clock at its end.
	At time.Time
	// Issues is the project's live records, and Slots the slots once the pass made its last move.
	Issues []record.Issue
	Slots  []record.Slot
	// Facts, Rows, Line and Dropped are the pass's last judgement of the line (blockers.JudgeLine):
	// the facts with the held set, the child waits and the started issues' waits filled, the
	// Dispatch mirror rows it read, the line less the members its last round promoted, and the
	// rows the line left behind with the word for why.
	Facts   record.LineFacts
	Rows    map[string]blockers.Row
	Line    []record.LineEntry
	Dropped []record.HeldIssue
	// Given is each slot the pass gave out and Taken each slot it took back, in the order it moved
	// them, as its record line names them (logPassMoves).
	Given []record.SlotGiven
	Taken []record.SlotTaken

	// Kind is the kind of the fact whose pass this is: an event, a tick or a signal's deadline
	// (signals.PassKind), which a replay records beside each slot the pass gave out. It is empty
	// for the view of a moment no pass ran at (View).
	Kind signals.PassKind
}

// UsePassObserver registers observe, which is handed the view of each admission pass once the
// fact the pass ran in has committed (store.OnCommit), so a pass whose fact rolls back is never
// seen. A replay of a recorded window registers one to measure its run from the states the pass
// decided from; the daemon registers none, and an admission with no observer gathers no view. It
// is a setter rather than a parameter of New for the reason UseExperiments is.
func (a *Admission) UsePassObserver(observe func(PassView)) {
	a.observer = observe
}

// View is the project as a pass would read it now, read and judged as the pass reads it (reread,
// blockers.JudgeLine) and writing nothing: the view an observer is handed of a pass, for a moment
// no pass ran at, such as the start state a replay writes before its first fact.
func (a *Admission) View(ctx context.Context, tx pgx.Tx) (PassView, error) {
	issues, slots, facts, err := a.reread(ctx, tx)
	if err != nil {
		return PassView{}, err
	}
	rows, facts, line, dropped, err := blockers.JudgeLine(ctx, tx, a.store, a.project, issues, ownSlots(issues, slots), facts, nil, a.now())
	if err != nil {
		return PassView{}, fmt.Errorf("judge the line of %s: %w", a.project, err)
	}
	return a.passView(issues, slots, facts, rows, line, dropped, &passMoves{}, nil, ""), nil
}

// observePass hands the pass's view to the observer once the fact it ran in commits: the reads the
// pass holds at its end, the line less the members its last round promoted (promoted), and its
// moves. An admission with no observer gathers nothing.
func (a *Admission) observePass(ctx context.Context, issues []record.Issue, slots []record.Slot, facts record.LineFacts,
	rows map[string]blockers.Row, line []record.LineEntry, dropped []record.HeldIssue, moves *passMoves, promoted []string,
	kind signals.PassKind,
) {
	if a.observer == nil {
		return
	}
	view, observe := a.passView(issues, slots, facts, rows, line, dropped, moves, promoted, kind), a.observer
	store.OnCommit(ctx, func() { observe(view) })
}

// passView is the view of a pass over its reads and moves, its line less the members promoted
// names.
func (a *Admission) passView(issues []record.Issue, slots []record.Slot, facts record.LineFacts, rows map[string]blockers.Row,
	line []record.LineEntry, dropped []record.HeldIssue, moves *passMoves, promoted []string, kind signals.PassKind,
) PassView {
	gone := make(map[string]bool, len(promoted))
	for _, key := range promoted {
		gone[key] = true
	}
	waiting := make([]record.LineEntry, 0, len(line))
	for _, entry := range line {
		if !gone[entry.Issue.Key] {
			waiting = append(waiting, entry)
		}
	}
	return PassView{Project: a.project, Cap: a.cap, Rule: a.engine.SlotRule(), At: a.now(), Issues: issues, Slots: slices.Clone(slots),
		Facts: facts, Rows: rows, Line: waiting, Dropped: dropped, Given: slices.Clone(moves.given), Taken: slices.Clone(moves.taken), Kind: kind}
}
