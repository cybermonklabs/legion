package admit

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// RuleTables is where an admission pass under a slot rule the daemon does not run reads, and
// keeps, what that rule records beyond the daemon's own tables: the merger's ready mark of a rule
// that gives the slot back at it (record.LineFacts.Ready), the covers that stand under a rule
// whose start may take a cover (record.LineFacts.Covers), the moment of each issue's first slot in
// its generation under a rule that places the line by it (record.LineFacts.FirstSlots), and the
// moment each issue arrived in its generation under a rule that places the line by arrival
// (record.LineFacts.Arrivals). A replay of a recorded window gives one over tables it makes in
// its own database (UseRuleTables), so no daemon migration carries a table of a rule the daemon
// never runs; the daemon gives none, and its passes read and keep nothing beyond the record.
type RuleTables interface {
	// ReadRuleFacts adds to facts, in tx, what the rule facts carry (record.LineFacts.Rule) reads
	// from the tables about issues, the pass's live records. Every pass reads its facts here
	// before it moves a slot, so a table whose rows the slots of the moment give (the first slots)
	// keeps them here, before it reads them.
	ReadRuleFacts(ctx context.Context, tx pgx.Tx, issues []record.Issue, facts *record.LineFacts) error
	// KeepCovers makes covers the covers that stand, in tx: the holder of each covered waiter, by
	// the waiter's key.
	KeepCovers(ctx context.Context, tx pgx.Tx, covers map[string]string) error
}

// UseRuleTables registers tables, where this admission's passes read and keep what a rule the
// daemon does not run records (RuleTables). New registers none, which is the daemon's: a pass
// reads the record alone and keeps no cover past the judgement that formed it. It is a setter
// rather than a parameter of New for the reason UseExperiments is.
func (a *Admission) UseRuleTables(tables RuleTables) {
	a.tables = tables
}

// keepCovers keeps covers as the covers that stand (RuleTables.KeepCovers), where the admission
// was given tables to keep them in.
func (a *Admission) keepCovers(ctx context.Context, tx pgx.Tx, covers map[string]string) error {
	if a.tables == nil {
		return nil
	}
	if err := a.tables.KeepCovers(ctx, tx, covers); err != nil {
		return fmt.Errorf("keep the covers of %s: %w", a.project, err)
	}
	return nil
}

// covers is what one pass under a slot rule whose start may take a cover (record.TakeCover,
// record.SlotRule.Take) knows of the covers: an issue of the project that has started and waits on
// another started issue gives its slot to that issue, which holds it for the waiter, and the
// waiter is not counted while the cover stands; and a counted waiter offers its slot to each issue
// it waits on that has not started, which then starts on it whatever the cap says (covers.take).
// A holder holds one waiter's slot at most, a covered waiter holds no slot and covers nobody, and
// a holder is not covered itself, so the started issues are at most twice the slots in use.
//
// The covers that stand are read from the record (record.LineFacts.Covers) rather than worked out
// again on each pass: a cover stays until its holder is done or leaves the work, or the waiter no
// longer waits on it (settleCovers). Under every other take the value is the zero covers, which
// cover nothing and offer nothing, so capLets reads the cap alone, as the daemon's own rule does.
type covers struct {
	// held is the holder of each covered waiter, by the waiter's key: the covers that stand.
	held map[string]string
	// holding is the waiter each holder holds the slot of, by the holder's key.
	holding map[string]string
	// offered is, for each issue that has not started and that a counted waiter waits on, the
	// counted waiters that offer it their slot, in slot order.
	offered map[string][]string
}

// coversOf is the covers of the pass over issues, the project's live records, own, its slots in
// slot order, rows, the mirror rows of its judgement, and facts, under the rule facts carry: the
// zero covers under any take but record.TakeCover. Each counted waiter — a slot holder that is
// no holder of a cover — offers its slot to each issue it waits on that has not started
// (coverWaits.reach).
func coversOf(issues []record.Issue, own []record.Slot, rows map[string]blockers.Row, facts record.LineFacts) covers {
	if facts.Rule.OrMain().Take != record.TakeCover {
		return covers{}
	}
	c := standingCovers(facts.Covers)
	c.offered = make(map[string][]string)
	waits := newCoverWaits(issues, own, rows, facts)
	for _, slot := range own {
		if _, holds := c.holding[slot.Issue]; holds {
			continue
		}
		if _, covered := c.held[slot.Issue]; covered {
			continue
		}
		for _, key := range waits.reach(slot.Issue) {
			if !waits.started(key) {
				c.offered[key] = append(c.offered[key], slot.Issue)
			}
		}
	}
	return c
}

// standingCovers is the covers of stored, the covers that stand, with nothing offered.
func standingCovers(stored map[string]string) covers {
	c := covers{held: make(map[string]string, len(stored)), holding: make(map[string]string, len(stored))}
	for waiter, holder := range stored {
		c.hold(waiter, holder)
	}
	return c
}

// hold records that holder holds waiter's slot for it, in both maps, which stay in step. The maps
// are shared by reference, so a copy of c records it too.
func (c covers) hold(waiter, holder string) {
	c.held[waiter], c.holding[holder] = holder, waiter
}

// offers says whether a counted waiter offers its slot to key: one that waits on key and gave its
// slot to no issue yet.
func (c covers) offers(key string) bool {
	_, found := c.waiterFor(key)
	return found
}

// take starts key on the slot of the first counted waiter that offers one to it, if any does, and
// answers that waiter: the waiter is covered by key from now on and offers its slot to nothing
// else. The promotion calls it on every row it grants, whichever half of the cap let the
// candidate through, and the waiter gives its slot up for key's (cover), so a candidate a waiter
// waits on never costs the cap a slot of its own while a waiter offers one.
func (c *covers) take(key string) (string, bool) {
	waiter, found := c.waiterFor(key)
	if !found {
		return "", false
	}
	c.hold(waiter, key)
	return waiter, true
}

// waiterFor is the first counted waiter, in slot order, that waits on key and gave its slot to no
// issue yet.
func (c covers) waiterFor(key string) (string, bool) {
	for _, waiter := range c.offered[key] {
		if _, gave := c.held[waiter]; !gave {
			return waiter, true
		}
	}
	return "", false
}

// coverWaits is what the covers of one pass read of the project: its live records by key, the
// issues that hold a slot of its own, the mirror rows and the facts of the pass's judgement.
type coverWaits struct {
	byKey map[string]record.Issue
	holds map[string]bool
	// gave is the waiters this pass covered, whose records still read the slot they gave.
	gave  map[string]bool
	rows  map[string]blockers.Row
	facts record.LineFacts
	// reached is each answer of reach, by the key it was asked of. An issue this pass covers or
	// gives its slot back was started before and after, so no answer goes stale in the pass.
	reached map[string][]string
}

// newCoverWaits is the coverWaits of issues, own, rows and facts.
func newCoverWaits(issues []record.Issue, own []record.Slot, rows map[string]blockers.Row, facts record.LineFacts) *coverWaits {
	w := &coverWaits{byKey: make(map[string]record.Issue, len(issues)), holds: make(map[string]bool, len(own)),
		gave: map[string]bool{}, rows: rows, facts: facts, reached: map[string][]string{}}
	for _, issue := range issues {
		w.byKey[issue.Key] = issue
	}
	for _, slot := range own {
		w.holds[slot.Issue] = true
	}
	return w
}

// started says whether the daemon has started key, as the line reads it (blockers' startedIssues):
// it holds a slot, gave its slot for a wait or a cover (record.Issue.ReleasedForWaitAt), or owes
// its next worker a start (record.LineFacts.Owed).
func (w *coverWaits) started(key string) bool {
	if w.holds[key] || w.gave[key] {
		return true
	}
	if _, owes := w.facts.Owed[key]; owes {
		return true
	}
	issue, recorded := w.byKey[key]
	return recorded && issue.ReleasedForWaitAt != nil
}

// reach is everything key waits on as a cover reads a wait, each once and in the order it is
// reached: what it waits on itself (blockers.CoverWaits), and then, through each issue among them
// that has not started, what that one waits on, since an issue that waits on another through
// issues that have not started waits on it too, and the first of such a chain that can start is
// the one that starts on the waiter's slot.
func (w *coverWaits) reach(key string) []string {
	if reached, asked := w.reached[key]; asked {
		return reached
	}
	var reached []string
	seen := map[string]bool{key: true}
	next := []string{key}
	for len(next) > 0 {
		from := next[0]
		next = next[1:]
		for _, waited := range blockers.CoverWaits(w.rows, w.byKey[from], w.facts) {
			if seen[waited] {
				continue
			}
			seen[waited] = true
			reached = append(reached, waited)
			if !w.started(waited) {
				next = append(next, waited)
			}
		}
	}
	w.reached[key] = reached
	return reached
}

// settleCovers brings the covers that stand up to the pass, under a rule whose start may take a
// cover (record.TakeCover), and does nothing under every other: it runs after the releases and
// before the line is promoted, over the pass's issues, slots, mirror rows and facts.
//
// First each cover that ends goes: its holder holds no slot any longer — it is done, or it left
// the work — or its waiter no longer waits on it. A waiter whose cover ended and that still needs
// a slot where it stands takes its slot back at once, in this pass and whatever the cap says,
// since it counts again from the moment the cover ends; it is the one way the count rises above
// the cap while the holder still runs. Then each counted waiter, in slot order, that waits on a
// started issue of the project that holds a slot, is neither a holder nor covered and does not
// wait on the waiter in turn, gives its slot to that issue, the first such issue it reaches
// (coverWaits.reach): it is covered from now on, and the slot it gave is one the cap no longer
// counts. In a cycle of waits each issue counts, as the rule's spec states.
//
// It answers whether it moved any slot or cover, so the caller reads the records again, and the
// waiters it covered, whose slots it freed. The covers it leaves are kept (keepCovers).
func (a *Admission) settleCovers(ctx context.Context, tx pgx.Tx, issues []record.Issue, slots []record.Slot,
	rows map[string]blockers.Row, facts record.LineFacts, moves *passMoves,
) (bool, []string, error) {
	if a.engine.SlotRule().Take != record.TakeCover {
		return false, nil, nil
	}
	own := record.OwnSlots(issues, slots)
	waits := newCoverWaits(issues, own, rows, facts)
	live := record.LiveRoots(issues, facts)
	needsSlot := func(key string) bool {
		issue, recorded := waits.byKey[key]
		_, running := live[issue.Tree]
		return recorded && facts.NeedsSlot(issue, waits.byKey[issue.Tree], running)
	}
	standing := standingCovers(nil)
	changed := false
	inUse := len(own)
	now := a.now()
	for _, waiter := range slices.Sorted(maps.Keys(facts.Covers)) {
		holder := facts.Covers[waiter]
		if needsSlot(waiter) && waits.holds[holder] && slices.Contains(waits.reach(waiter), holder) {
			standing.hold(waiter, holder)
			continue
		}
		changed = true
		if !needsSlot(waiter) || waits.holds[waiter] {
			continue
		}
		slot := record.Slot{Issue: waiter, Index: nextSlotIndex(slots), AdmittedAt: now}
		if err := a.store.PutSlot(ctx, tx, slot); err != nil {
			return changed, nil, fmt.Errorf("give %s its slot back at the end of its cover: %w", waiter, err)
		}
		if waits.byKey[waiter].ReleasedForWaitAt != nil {
			if err := a.store.ClearReleasedForWait(ctx, tx, waiter); err != nil {
				return changed, nil, fmt.Errorf("clear the slot release of %s at the end of its cover: %w", waiter, err)
			}
		}
		slots = append(slots, slot)
		waits.holds[waiter] = true
		inUse++
		a.logSlot(ctx, "taken", waiter, inUse)
		moves.give(waiter, record.LineWaitingOnIssue, inUse)
		a.log.Info("admission gives a slot back to an issue whose cover ended", "issue", waiter, "holder", holder)
	}
	var freed []string
	for _, slot := range record.OwnSlots(issues, slots) {
		waiter := slot.Issue
		if _, holds := standing.holding[waiter]; holds {
			continue
		}
		for _, key := range waits.reach(waiter) {
			_, holds := standing.holding[key]
			_, covered := standing.held[key]
			if !waits.holds[key] || holds || covered || slices.Contains(waits.reach(key), waiter) {
				continue
			}
			var err error
			if slots, inUse, err = a.cover(ctx, tx, waiter, key, slots, inUse, facts, moves); err != nil {
				return changed, freed, err
			}
			standing.hold(waiter, key)
			delete(waits.holds, waiter)
			waits.gave[waiter] = true
			freed = append(freed, waiter)
			changed = true
			break
		}
	}
	if !changed {
		return false, nil, nil
	}
	return true, freed, a.keepCovers(ctx, tx, standing.held)
}

// cover gives waiter's slot to holder, the started issue it waits on, which holds it for the
// waiter from now on: the waiter's row goes with the mark of a release for a wait, one statement
// (record.Store.ReleaseSlotForWait), so the line stands it among the issues whose work came back,
// passed over while it waits (blockers' waitsOf of a covered waiter), and the record says it has
// started though it holds no slot. It answers slots without the waiter's and the slots in use
// after it. The release is logged as every release is, and the cover once the fact commits.
func (a *Admission) cover(ctx context.Context, tx pgx.Tx, waiter, holder string, slots []record.Slot, inUse int,
	facts record.LineFacts, moves *passMoves,
) ([]record.Slot, int, error) {
	if err := a.store.ReleaseSlotForWait(ctx, tx, waiter, a.now()); err != nil {
		return slots, inUse, fmt.Errorf("give the slot of %s to %s, which it waits on: %w", waiter, holder, err)
	}
	slots = slices.DeleteFunc(slices.Clone(slots), func(slot record.Slot) bool { return slot.Issue == waiter })
	inUse--
	a.logSlot(ctx, "released", waiter, inUse)
	moves.take(waiter, coverReason(waiter, holder, facts), inUse)
	store.OnCommit(ctx, func() {
		a.log.Info(CoverTakenLine, "waiter", waiter, "holder", holder, "slots", inUse, "cap", a.cap)
	})
	return slots, inUse, nil
}

// CoverTakenLine is the line an admission pass writes once its fact commits for each cover it
// takes: a waiter's slot given to the issue it waits on, which holds it for the waiter (cover).
const CoverTakenLine = "admission: an issue holds the slot of a waiter that waits on it"

// coverReason is the path the pass's record names for the slot waiter gave to holder (cover): a
// wait on its open children where holder is one of them, the children its first phase waits for
// (record.LineFacts.ChildWaits) or, its phases started, the children that hold its next start
// (record.LineFacts.StartedParentWaits), both waits a cover reads (blockers.CoverWaits); and a
// wait on a blocked_by link otherwise.
func coverReason(waiter, holder string, facts record.LineFacts) record.SlotTakenReason {
	if openChildOf(facts.ChildWaits, waiter, holder) || openChildOf(facts.StartedParentWaits, waiter, holder) {
		return record.TakenChildrenWait
	}
	return record.TakenLinkWait
}
