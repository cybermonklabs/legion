package workflow

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The end of a step's run follows from the record, at one point every fact passes, so that no way
// out of a step has to remember to end it. A run — one the store holds open, or one no start of
// this release counted at the place an issue stood when the fact began (endStep's uncounted end) —
// is over when (runOver):
//   - its issue no longer stands there: it has no record, or stands at another place, or in
//     another generation (standingStep);
//   - its place starts again (startStep), for a run the store holds open: a new run of the place
//     begins, as the promotion of a tree admitted again starts a member's merger again; or
//   - its tree's run is over: the tree's root lingers, and the run is not a merge that can still
//     land in that tree (mergeCanLand).
//
// The point is SettleFact, which intake.ApplyFact runs after a fact's last handler, in the fact's
// own transaction and before its commit, and which the daemon's boot reconcile runs the same way
// for the admission pass it applies in a transaction of its own (intake.OpenSettlers). It reads the
// runs of the issues the fact touched — those whose record or pull request it wrote, which the
// store keeps with each one's record from before the fact (record.WithIssueWrites), and the members
// of a tree whose root it left lingering or whose linger it closed — and of no other issue, so what
// a fact reads grows with what it changed, never with the store. A start ends the over runs of its
// own issue first (startStep), so the end of the run an issue leaves reaches Dispatch before the
// start of the run it moves into.
//
// What the record does not hold, a fact names: the result of a move of the table and the step its
// work returns to (nameMoveEnd), and the close of a tree's linger (closeLinger), which writes no
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
// ends its moves name, the lingers it closes, and the issues its writes touch with their records
// from before it (record.WithIssueWrites). A ctx that carries them already, opened by another
// engine of the same fact, is answered as it is (intake.Settler).
func (e *Engine) OpenFact(ctx context.Context) context.Context {
	if _, ok := ctx.Value(factRunsKey{}).(*factRuns); !ok {
		ctx = context.WithValue(ctx, factRunsKey{}, &factRuns{ends: map[runAt]StepEnd{}, closed: map[string]bool{}})
	}
	return record.WithIssueWrites(ctx)
}

// SettleFact ends each run the fact left over (runOver), once the fact's last handler has applied
// it, in its transaction: the runs of each issue the fact touched, and of each member of a tree
// whose root the fact wrote and which lingers, or whose linger it closed (intake.Settler). A
// second settle of one fact settles nothing.
func (e *Engine) SettleFact(ctx context.Context, tx pgx.Tx) error {
	runs, ok := ctx.Value(factRunsKey{}).(*factRuns)
	if !ok || runs.settled {
		return nil
	}
	runs.settled = true
	return e.settleRuns(ctx, tx, runs, record.IssuesWritten(ctx), true, nil)
}

// nameMoveEnd names the end of the run issue stands in that a move on trigger into to ends: the
// table's own result for the trigger, the step the work returns to, and the report handoff names
// (endOfTransition). The move names it on the issue as it stood, before the write that moves it.
func (e *Engine) nameMoveEnd(ctx context.Context, issue record.Issue, trigger TriggerKind, to phase.Phase, handoff record.PhaseRow) error {
	step, ok := standingStep(issue)
	if !ok {
		return nil
	}
	runs, err := factRunsOf(ctx)
	if err != nil {
		return err
	}
	runs.ends[runAt{issue: issue.Key, generation: issue.Generation, place: step.place}] = endOfTransition(trigger, step.at, to, handoff)
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
	return e.settleRuns(ctx, tx, runs, []string{issue}, false, &runAt{issue: issue, place: place})
}

// settleRuns ends each run of keys that is over (runOver), with the end the fact named for it or
// stopped. withTrees adds the members of each tree whose root among keys lingers, and of each tree
// whose linger the fact closed. starting is the place a start of one issue is about to begin (its
// generation unread), nil for none. It is the one writer of a run's end (endStep).
func (e *Engine) settleRuns(ctx context.Context, tx pgx.Tx, runs *factRuns, keys []string, withTrees bool, starting *runAt) error {
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
	if withTrees {
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
	rows := make(map[runAt]record.IssueStepRun, len(stored))
	for _, row := range stored {
		rows[runAt{issue: row.Issue, place: row.Place}] = row
	}
	over := func(run runAt, now *record.Issue, restarted bool) (bool, error) {
		return e.runOver(ctx, tx, runs, run, now, restarted, read)
	}
	// The runs the store holds open.
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
		ended, err := over(run, now, restarted)
		if err != nil {
			return err
		}
		if !ended {
			continue
		}
		at, _ := phaseAt(row.Place)
		stood := record.Issue{Key: row.Issue}
		if now != nil {
			stood = *now
		}
		stood.Generation, stood.Phase, stood.Hold = row.Generation, at, nil
		if err := e.endStep(ctx, tx, stood, runs.endOf(run)); err != nil {
			return err
		}
	}
	// The run each issue stood in when the fact began that no start of this release counted: the
	// place has no row, or one the roll forward of a rollback marked uncounted. The daemon's boot
	// gives each such run it finds open an open row (OpenUncountedRuns), which the loop above ends,
	// so this ends a run the boot found none of. It ends only a run that was open when the fact
	// began (uncountedRunOpen, over the issues as they stood then, with no linger closed yet), so a
	// run the end rule held over already, or one of a generation that started none, is never
	// recorded. Such a run is in Dispatch only once its end is written, so a start of its place
	// leaves it be: the start cannot tell it from the run it begins.
	stoodThen := func(key string) (*record.Issue, error) {
		if before, written := record.IssueBefore(ctx, key); written {
			return before, nil
		}
		return read(key)
	}
	began := &factRuns{ends: runs.ends, closed: map[string]bool{}}
	for _, key := range order {
		stood, err := stoodThen(key)
		if err != nil || stood == nil {
			if err != nil {
				return err
			}
			continue
		}
		step, ok := standingStep(*stood)
		if !ok {
			continue
		}
		if row, counted := rows[runAt{issue: key, place: step.place}]; counted && row.Run != record.RunUncounted {
			continue
		}
		run, open, err := e.uncountedRunOpen(ctx, tx, began, *stood, stoodThen)
		if err != nil || !open {
			if err != nil {
				return err
			}
			continue
		}
		now, err := read(key)
		if err != nil {
			return err
		}
		ended, err := over(run, now, false)
		if err != nil {
			return err
		}
		if !ended {
			continue
		}
		if err := e.endStep(ctx, tx, *stood, runs.endOf(run)); err != nil {
			return err
		}
	}
	return nil
}

// issueReader reads the record of an issue by key, nil for none: the settle's read of the store as
// it stands, or of each issue as it stood when the fact began.
type issueReader func(key string) (*record.Issue, error)

// runOver is the end rule, the one answer to whether run is over. now is its issue as it stands
// (nil for none), restarted whether a start of its place is about to begin, and read how its tree's
// root is read. A run is over when its issue has no record, or stands at another place or in
// another generation (standingStep); when a start of its place begins; or when its tree's root
// lingers, unless it is a merge that can still land in that linger (mergeCanLand, whose closed
// lingers runs holds).
func (e *Engine) runOver(ctx context.Context, tx pgx.Tx, runs *factRuns, run runAt, now *record.Issue, restarted bool, read issueReader) (bool, error) {
	if restarted || now == nil {
		return true, nil
	}
	if step, ok := standingStep(*now); !ok || step.place != run.place || now.Generation != run.generation {
		return true, nil
	}
	root := now
	if now.Tree != now.Key {
		var err error
		if root, err = read(now.Tree); err != nil {
			return false, err
		}
	}
	if root == nil || !root.Lingers() {
		return false, nil
	}
	if !awaitsMerge(*now) {
		return true, nil
	}
	canLand, err := e.mergeCanLand(ctx, tx, runs, *now, *root)
	return !canLand, err
}

// uncountedRunOpen is the one answer, for the boot and the settle alike, to whether issue, as read
// reads its tree, stands in an open run that no start of this release counted, and that run: issue
// stands in a step whose runs are recorded (standingStep), the end rule does not hold that run over
// (runOver), its tree is not the operator's, which runs no step of the workflow
// (record.OperatorHoldsTree), and its generation started a run (generationStarted).
func (e *Engine) uncountedRunOpen(ctx context.Context, tx pgx.Tx, runs *factRuns, issue record.Issue, read issueReader) (runAt, bool, error) {
	step, ok := standingStep(issue)
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
	started, err := e.generationStarted(ctx, tx, issue)
	return run, started, err
}

// generationStarted says whether issue's generation started a run. It did not for a root a
// hand-over carried in at this generation (record.HandedOverRoot) that its promotion has not
// reached: the root holds no admission slot and no mark of one taken away for a wait
// (record.Issue.ReleasedForWaitAt), and its carried phase starts its run at that promotion
// (admit's readmit). Every other issue that stands in a step started its run there.
func (e *Engine) generationStarted(ctx context.Context, tx pgx.Tx, issue record.Issue) (bool, error) {
	if !claim.IsTreeRoot(issue.Key, issue.Tree) || issue.ReleasedForWaitAt != nil {
		return true, nil
	}
	carried, err := e.store.HandedOverRoot(ctx, tx, issue.Key)
	if err != nil || carried == nil || carried.Generation != issue.Generation {
		return true, err
	}
	slots, err := e.store.Slots(ctx, tx)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(slots, func(slot record.Slot) bool { return slot.Issue == issue.Key }), nil
}

// mergeCanLand says whether member's merge, which awaits its landing in the tree root lingers, can
// still land there: while the linger runs and no fact has closed it, while its pull request is open,
// or where the close of root's linger handed it over, whose re-admission ends the merge's run
// instead.
func (e *Engine) mergeCanLand(ctx context.Context, tx pgx.Tx, runs *factRuns, member, root record.Issue) (bool, error) {
	if record.Lingering(root, e.now()) && !runs.closed[root.Tree] {
		return true, nil
	}
	pr, err := e.store.PullRequest(ctx, tx, member.Key)
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

// phaseAt is the phase whose step runs at place: of the workflow's line (phase.Order), the phase
// whose step (stepOf) is at place.
func phaseAt(place record.StepPlace) (phase.Phase, bool) {
	for _, p := range phase.Order {
		if at, _, _, ok := stepOf(p); ok && at == place {
			return p, true
		}
	}
	return "", false
}
