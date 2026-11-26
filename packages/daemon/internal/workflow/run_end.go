package workflow

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The end of a step's run follows from the record, at one point every fact passes, so that no way
// out of a step has to remember to end it. A run is one the store holds open: a start opens it,
// and the daemon's boot opens each run no start of this release counted before any fact applies
// (OpenUncountedRuns). It is over when (runOver):
//   - its issue no longer stands there: it has no record, or stands at another place, or in
//     another generation (standingStep);
//   - its place starts again (startStep): a new run of the place begins, as the promotion of a
//     tree admitted again starts a member's merger again; or
//   - its tree's run is over: the tree's root lingers, and the run is not a merge that can still
//     land in that tree (mergeCanLand).
//
// The first and the last do not end the retro of a run whose retro after the close is open
// (record.RunRetro.Open): it runs after its issue left its phases, and its record ends first.
//
// The point is SettleFact, which intake.ApplyFact runs after a fact's last handler, in the fact's
// own transaction and before its commit, and which two transactions of the daemon's own run the
// same way (intake.OpenSettlers): the boot reconcile's, for the admission pass it applies, and the
// outbox's settling, for each drop of a hand-over fence. It reads the runs of the issues the fact
// touched — those whose record, pull request or hand-over fence it wrote, which the store keeps
// (record.WithIssueWrites), those whose run it named an end for (nameEnd), and the members of a
// tree whose root it left lingering or whose linger it closed — and of no other issue, so what a
// fact reads grows with what it changed, never with
// the store. A start ends the over runs of its own issue first (startStep), so the end of the run
// an issue leaves reaches Dispatch before the start of the run it moves into.
//
// What the record does not hold, a fact names: the result of a move of the table and the step its
// work returns to (nameMoveEnd), the end of a retro after a close (nameRetroEnd), and the close of
// a tree's linger (closeLinger), which writes no
// record of its own. Every other end is stopped, so a way out added later that names nothing still
// ends its run.

// runAt is one run of a step: its issue, the issue's generation, and the place of its step.
type runAt struct {
	issue      string
	generation uint64
	place      record.StepPlace
}

// factRuns is what one fact names of the runs it ends that the record does not hold: the end each
// move of the table names for the run it leaves, and the trees whose linger it closed. settled says
// the fact's runs are settled already: an engine and the admission that holds it share one fact.
type factRuns struct {
	ends    map[runAt]StepEnd
	closed  map[string]bool
	settled bool
}

// factRunsKey is the context key of a fact's factRuns.
type factRunsKey struct{}

// errOutsideFact is a move named, a linger closed or a step started in a transaction no fact opened
// (OpenFact): its run's end would be decided nowhere.
var errOutsideFact = errors.New("a step's run is moved, closed or started outside a fact's settle (workflow.Engine.OpenFact)")

// factRunsOf is the factRuns ctx carries, or errOutsideFact.
func factRunsOf(ctx context.Context) (*factRuns, error) {
	runs, ok := ctx.Value(factRunsKey{}).(*factRuns)
	if !ok {
		return nil, errOutsideFact
	}
	return runs, nil
}

// endOf is the end the fact named for run, and stopped where it named none.
func (runs *factRuns) endOf(run runAt) StepEnd {
	if end, ok := runs.ends[run]; ok {
		return end
	}
	return stepStopped
}

// OpenFact answers ctx carrying what the engine keeps of one fact while its handlers apply it: the
// ends its moves name, the lingers it closes, and the issues its writes touch
// (record.WithIssueWrites). A ctx that carries them already, opened by another engine of the same
// fact, is answered as it is (intake.Settler).
func (e *Engine) OpenFact(ctx context.Context) context.Context {
	if _, ok := ctx.Value(factRunsKey{}).(*factRuns); !ok {
		ctx = context.WithValue(ctx, factRunsKey{}, &factRuns{ends: map[runAt]StepEnd{}, closed: map[string]bool{}})
	}
	return record.WithIssueWrites(ctx)
}

// SettleFact settles the record the fact left, once its last handler has applied it, in its
// transaction (intake.Settler): first each parent held before its retro whose tree the fact wrote
// an issue of is released where nothing beneath it is open (releaseHeldParents), and then each run
// the fact left over ends (runOver): the runs of each issue the fact touched, the released parents
// among them, of each issue whose run it named an end for (nameEnd) though it wrote no record of
// that issue, as the end of a retro after a close names one (retroReported), and of each member of
// a tree whose root the fact wrote and which lingers, or whose linger it closed. A second settle of
// one fact settles nothing.
func (e *Engine) SettleFact(ctx context.Context, tx pgx.Tx) error {
	runs, ok := ctx.Value(factRunsKey{}).(*factRuns)
	if !ok || runs.settled {
		return nil
	}
	runs.settled = true
	if err := e.releaseHeldParents(ctx, tx, record.IssuesWritten(ctx)); err != nil {
		return err
	}
	return e.settleRuns(ctx, tx, runs, append(record.IssuesWritten(ctx), runs.named()...), nil)
}

// named is the key of each issue the fact named the end of a run of (nameEnd), in key order.
func (runs *factRuns) named() []string {
	keys := make([]string, 0, len(runs.ends))
	for run := range runs.ends {
		keys = append(keys, run.issue)
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}

// nameMoveEnd names the end of the run issue stands in on w, the walk of its run, that a move on
// trigger into to ends: the table's own result for the trigger, the step the work returns to, and
// the report handoff names (endOfTransition). The move names it on the issue as it stood, before
// the write that moves it.
func (e *Engine) nameMoveEnd(ctx context.Context, w Walk, issue record.Issue, trigger TriggerKind, to phase.Phase, handoff record.PhaseRow) error {
	step, ok := standingStep(w, issue)
	if !ok {
		return nil
	}
	return e.nameEnd(ctx, issue, step, endOfTransition(w, trigger, step.at, to, handoff))
}

// nameEnd names end as the end of the run of step, the step issue stands in, which the fact's
// settle writes (settleRuns): a move of the table's (nameMoveEnd), and the stop of a task, which
// ends its step as the stop says (stopTask).
func (e *Engine) nameEnd(ctx context.Context, issue record.Issue, step standing, end StepEnd) error {
	runs, err := factRunsOf(ctx)
	if err != nil {
		return err
	}
	runs.ends[runAt{issue: issue.Key, generation: issue.Generation, place: step.place}] = end
	return nil
}

// closeLinger names tree's linger closed by this fact, which the record keeps no mark of: the
// merge of a member it neither hands over nor leaves an open pull request can no longer land
// (mergeCanLand).
func (e *Engine) closeLinger(ctx context.Context, tree string) error {
	runs, err := factRunsOf(ctx)
	if err != nil {
		return err
	}
	runs.closed[tree] = true
	return nil
}

// settleStart ends, before a start of place on issue, each of the issue's runs that is over, the
// run of that place among them: so the end of the run the issue leaves, and of the run its place
// held, comes before the start that follows them.
func (e *Engine) settleStart(ctx context.Context, tx pgx.Tx, issue string, place record.StepPlace) error {
	runs, err := factRunsOf(ctx)
	if err != nil {
		return err
	}
	return e.settleRuns(ctx, tx, runs, []string{issue}, &runAt{issue: issue, place: place})
}

// settleRuns ends each open run of keys that is over (runOver), with the end the fact named for it
// or stopped. starting is the place a start of one issue is about to begin (its generation unread),
// whose settle reads that issue alone (settleStart); nil is the fact's own settle (SettleFact),
// which adds the members of each tree whose root among keys lingers, and of each tree whose linger
// the fact closed. It alone ends a run (endStep, and endOffWalk for a run on no step of its walk,
// which both write the end through endRun). Every run it can end has an open row: a start writes
// it, and the daemon's boot writes it for each run no start of this release counted
// (OpenUncountedRuns), before any fact applies. It ends each run whose end rule holds, so an issue
// may have several runs open at once, each ended on its own.
func (e *Engine) settleRuns(ctx context.Context, tx pgx.Tx, runs *factRuns, keys []string, starting *runAt) error {
	issues := map[string]*record.Issue{}
	read := func(key string) (*record.Issue, error) {
		if issue, ok := issues[key]; ok {
			return issue, nil
		}
		issue, err := e.store.Issue(ctx, tx, key)
		if err != nil {
			return nil, err
		}
		issues[key] = issue
		return issue, nil
	}
	var order []string
	for _, key := range keys {
		if !slices.Contains(order, key) {
			order = append(order, key)
		}
	}
	if starting == nil {
		var trees []string
		for _, key := range order {
			issue, err := read(key)
			if err != nil {
				return err
			}
			if issue != nil && issue.Key == issue.Tree && issue.Lingers() {
				trees = append(trees, issue.Tree)
			}
		}
		for tree := range runs.closed {
			if !slices.Contains(trees, tree) {
				trees = append(trees, tree)
			}
		}
		slices.Sort(trees)
		for _, tree := range trees {
			members, err := e.store.TreeIssues(ctx, tx, tree)
			if err != nil {
				return err
			}
			for i := range members {
				member := members[i]
				issues[member.Key] = &member
				if !slices.Contains(order, member.Key) {
					order = append(order, member.Key)
				}
			}
		}
	}
	stored, err := e.store.StepRuns(ctx, tx, order)
	if err != nil {
		return err
	}
	for _, row := range stored {
		if row.Run != record.RunOpen {
			continue
		}
		run := runAt{issue: row.Issue, generation: row.Generation, place: row.Place}
		now, err := read(row.Issue)
		if err != nil {
			return err
		}
		restarted := starting != nil && starting.issue == row.Issue && starting.place == row.Place
		ended, err := e.runOver(ctx, tx, runs, run, now, restarted, read)
		if err != nil {
			return err
		}
		if !ended {
			continue
		}
		stood := record.Issue{Key: row.Issue}
		if now != nil {
			stood = *now
		}
		// The run's own walk, at its generation, names the phase its place is the step of, so the
		// end is written for the step of that run's plan, whichever run the issue stands in now.
		stood.Generation, stood.Hold = row.Generation, nil
		w, err := e.walk(ctx, tx, stood)
		if err != nil {
			return err
		}
		var onWalk bool
		if stood.Phase, onWalk = phaseAt(w, row.Place); !onWalk {
			if err := e.endOffWalk(ctx, tx, row, runs.endOf(run)); err != nil {
				return err
			}
			continue
		}
		if err := e.endStep(ctx, tx, stood, w, runs.endOf(run)); err != nil {
			return err
		}
	}
	return nil
}

// endOffWalk ends row, an open run that is over and whose place no step of the walk of its own
// generation holds: the plan rows that named its step are gone, as the way back of a rollback
// drops them while a task's step runs, so no standing names its step, and endStep, which writes
// the end of the step a standing names, would write nothing and leave the run open for good. Its
// end is the record its start kept (record.IssueStepRun.Kept), written by endRun, the writer of
// endStep's ends too, so the two writes of the run agree. The log says why it ended. A row with no
// kept start's record breaks the store invariant: every writer of a row keeps the write in its
// transaction, so ending it without a record would mark it ended while Dispatch never learned it
// started.
func (e *Engine) endOffWalk(ctx context.Context, tx pgx.Tx, row record.IssueStepRun, end StepEnd) error {
	if row.Kept.ID == "" {
		return fmt.Errorf("end the off-walk run of %s %s/%s/%d: its open start row holds no kept record", row.Issue, row.Place.Task, row.Place.Kind, row.Place.Place)
	}
	ended, err := e.endRun(ctx, tx, row.Issue, row.Place, end, func(record.StepStart) record.StepRecord { return row.Kept })
	if err != nil || !ended {
		return err
	}
	e.logOnCommit(ctx, "workflow: a run on no step of the walk of its run is ended", "issue", row.Issue, "generation", row.Generation,
		"task", row.Place.Task, "kind", row.Place.Kind, "place", row.Place.Place, "result", end.Result,
		"reason", "no step of the walk of its run holds its place: the plan rows that named its step are gone")
	return nil
}

// issueReader reads the record of an issue by key, nil for none: the settle's read of the store as
// it stands, or the boot's (OpenUncountedRuns).
type issueReader func(key string) (*record.Issue, error)

// runOver is the end rule, the one answer to whether run is over. now is its issue as it stands
// (nil for none), restarted whether a start of its place is about to begin, and read how its tree's
// root is read. A run is over when its issue has no record, or stands at another place on the walk
// of its run or in another generation (standingStep, which an issue its plan holds stands in no
// step of); when a start of its place begins; or when its tree's root lingers, unless it is a
// merge that can still land in that linger (mergeCanLand, whose closed lingers runs holds). One
// run is none of these whatever the issue's standing: the retro of a run whose retro after the
// close is open (record.RunRetro.Open), which runs after the issue left its phases, at the run it
// is of, while the tree lingers, and ends by its report, its bound or a todo, each of which ends
// the record first. The record is read only where the rule of the standing answers over.
func (e *Engine) runOver(ctx context.Context, tx pgx.Tx, runs *factRuns, run runAt, now *record.Issue, restarted bool, read issueReader) (bool, error) {
	if restarted || now == nil {
		return true, nil
	}
	over, err := e.runOverByStanding(ctx, tx, runs, run, *now, read)
	if err != nil || !over || run.place.Kind != record.PlaceRetro {
		return over, err
	}
	retro, err := e.store.RunRetro(ctx, tx, run.issue, run.generation)
	if err != nil {
		return false, err
	}
	return retro == nil || !retro.Open(), nil
}

// runOverByStanding is runOver's rule of now, run's issue as it stands: it stands in another
// generation, or at another place on the walk of its run, or its tree's root lingers and run is no
// merge that can still land in that linger.
func (e *Engine) runOverByStanding(ctx context.Context, tx pgx.Tx, runs *factRuns, run runAt, now record.Issue, read issueReader) (bool, error) {
	if now.Generation != run.generation {
		return true, nil
	}
	w, err := e.walk(ctx, tx, now)
	if err != nil {
		return false, err
	}
	if step, ok := standingStep(w, now); !ok || step.place != run.place {
		return true, nil
	}
	root := &now
	if now.Tree != now.Key {
		if root, err = read(now.Tree); err != nil {
			return false, err
		}
	}
	if root == nil || !root.Lingers() {
		return false, nil
	}
	if !awaitsMerge(now) {
		return true, nil
	}
	canLand, err := e.mergeCanLand(ctx, tx, runs, now, *root)
	return !canLand, err
}

// uncountedRunOpen is the boot's answer (OpenUncountedRuns) to whether issue, as read reads its
// tree, stands in an open run that no start of this release counted, and that run: issue
// stands in a step whose runs are recorded on the walk of its run (standingStep), the end rule does
// not hold that run over (runOver), its tree is not the operator's, which runs no step of the
// workflow (record.OperatorHoldsTree), and its generation started a run (generationStarted).
func (e *Engine) uncountedRunOpen(ctx context.Context, tx pgx.Tx, runs *factRuns, issue record.Issue, read issueReader) (runAt, bool, error) {
	w, err := e.walk(ctx, tx, issue)
	if err != nil {
		return runAt{}, false, err
	}
	step, ok := standingStep(w, issue)
	if !ok {
		return runAt{}, false, nil
	}
	run := runAt{issue: issue.Key, generation: issue.Generation, place: step.place}
	over, err := e.runOver(ctx, tx, runs, run, &issue, false, read)
	if err != nil || over {
		return run, false, err
	}
	operator, err := record.OperatorHoldsTree(ctx, e.store, tx, issue)
	if err != nil || operator {
		return run, false, err
	}
	started, err := e.generationStarted(ctx, tx, issue, read)
	return run, started, err
}

// generationStarted says whether the run issue stands in has started, with its tree's root read
// by read. It has not while that root waits for the promotion that opens its tree
// (record.AwaitsPromotion): a root a re-admission or a hand-over put back in the line, whose
// carried phase starts its run at that promotion (admit's readmit), and each member such a
// re-admission left mid-phase, which the promotion starts again (admit's startMidPhaseMembers).
// The run a member stood in before ended with the linger the re-admission cleared, so the
// re-admission leaves the end rule (runOver) no linger to read, and this is the clause that holds
// such a member's run unstarted. Every other issue that stands in a step started its run there.
func (e *Engine) generationStarted(ctx context.Context, tx pgx.Tx, issue record.Issue, read issueReader) (bool, error) {
	root := &issue
	if !claim.IsTreeRoot(issue.Key, issue.Tree) {
		var err error
		if root, err = read(issue.Tree); err != nil || root == nil {
			return true, err
		}
	}
	lifecycle, gate, err := record.RootFacts(ctx, e.store, tx, *root, e.cfg.Steps)
	if err != nil {
		return false, err
	}
	slots, err := e.store.Slots(ctx, tx)
	if err != nil {
		return false, err
	}
	holds := slices.ContainsFunc(slots, func(slot record.Slot) bool { return slot.Issue == root.Key })
	return !record.AwaitsPromotion(*root, lifecycle, gate, holds), nil
}

// mergeCanLand says whether member's merge, which awaits its landing in the tree root lingers, can
// still land there: while the linger runs and no fact has closed it, while its pull request is open,
// or where the close of root's linger handed it over, whose re-admission ends the merge's run
// instead. The pull request's state is all it reads, so it reads the row alone
// (record.Store.PullRequestRow), with none of what the pull request remembers of its settled
// heads.
func (e *Engine) mergeCanLand(ctx context.Context, tx pgx.Tx, runs *factRuns, member, root record.Issue) (bool, error) {
	if record.Lingering(root, e.now()) && !runs.closed[root.Tree] {
		return true, nil
	}
	pr, err := e.store.PullRequestRow(ctx, tx, member.Key)
	if err != nil {
		return false, err
	}
	if pr != nil && pr.State == record.PullRequestOpen {
		return true, nil
	}
	handover, err := e.store.Handover(ctx, tx, member.Key)
	if err != nil {
		return false, err
	}
	return handover != nil && handover.Tree == root.Tree && handover.Generation == root.Generation, nil
}

// phaseAt is the phase whose step runs at place on w, the walk of the run: of its line (Walk.Line,
// phase.Order for a run with no plan rows), the phase whose step (placeOnWalk) is at place.
func phaseAt(w Walk, place record.StepPlace) (phase.Phase, bool) {
	for _, p := range w.Line {
		if at, _, _, ok := placeOnWalk(w, p); ok && at == place {
			return p, true
		}
	}
	return "", false
}
