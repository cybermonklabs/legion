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
// The point is SettleFact, which intake.ApplyFact runs after a fact's last handler, in the fact's
// own transaction and before its commit, and which two transactions of the daemon's own run the
// same way (intake.OpenSettlers): the boot reconcile's, for the admission pass it applies, and the
// outbox's settling, for each drop of a hand-over fence. It reads the runs of the issues the fact
// touched — those whose record, pull request or hand-over fence it wrote, which the store keeps
// (record.WithIssueWrites), and the members of a tree whose root it left lingering or whose linger
// it closed — and of no other issue, so what a fact reads grows with what it changed, never with
// the store. A start ends the over runs of its own issue first (startStep), so the end of the run
// an issue leaves reaches Dispatch before the start of the run it moves into.
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
// move of the table names for the run it leaves, and the trees whose linger it closed. unrested is
// each ancestor a root stopped resting on in the fact (restEnded), which writes no record of the
// ancestor's tree. settled says the fact's runs are settled already: an engine and the admission
// that holds it share one fact.
type factRuns struct {
	ends     map[runAt]StepEnd
	closed   map[string]bool
	unrested []string
	settled  bool
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
// an issue of, or whose tree holds an ancestor a root stopped resting on in the fact (restEnded),
// has its retro started where nothing holds it any longer (releaseHeldParents), then each run
// the fact left over ends (runOver): the runs of each issue the fact touched, the released parents
// among them, and of each member of a tree whose root the fact wrote and which lingers, or whose
// linger it closed. Then each agent the fact freed goes to the step that waited longest for it
// (handOnAgents), and last each step whose join nothing joins at its issue's generation is joined
// there (joinAgain). A second settle of one fact settles nothing.
func (e *Engine) SettleFact(ctx context.Context, tx pgx.Tx) error {
	runs, ok := ctx.Value(factRunsKey{}).(*factRuns)
	if !ok || runs.settled {
		return nil
	}
	runs.settled = true
	if err := e.releaseHeldParents(ctx, tx, slices.Concat(record.IssuesWritten(ctx), runs.unrested)); err != nil {
		return err
	}
	if err := e.settleRuns(ctx, tx, runs, record.IssuesWritten(ctx), nil); err != nil {
		return err
	}
	if err := e.handOnAgents(ctx, tx, record.IssuesWritten(ctx)); err != nil {
		return err
	}
	return e.joinAgain(ctx, tx, record.IssuesWritten(ctx))
}

// handOnAgents is the one place an agent a step of another task waits for is handed on, after each
// move and each start of the fact: so a move that frees an agent and a move that puts a step of the
// same agent at its task's place, in one fact, each had their own start first (Engine.start), and
// the step a move made ready in the same act waits behind the steps that waited longer. For each
// issue the fact wrote whose plan holds several tasks, each step of a running task that waits for
// its agent, the one that waited longest first (by the moment its wait began, then by task id), is
// started through Engine.start where no running step of the issue holds its agent, which starts it
// in that agent's session. An issue of a tree that lingers starts nothing, as no move does there,
// and neither does an issue in awaiting_children, where no role works it (dispatch://INFRA-3320
// R11): the return's start of the step its wait holds waits for its agent then (ReturnFromChildren,
// Engine.start), and the settle of that return hands the agent on. An issue the record does not
// hold has no step to start: the hand-on goes on to the next.
func (e *Engine) handOnAgents(ctx context.Context, tx pgx.Tx, keys []string) error {
	var order []string
	for _, key := range keys {
		if !slices.Contains(order, key) {
			order = append(order, key)
		}
	}
	for _, key := range order {
		issue, err := e.store.Issue(ctx, tx, key)
		if err != nil {
			return err
		}
		if issue == nil || issue.Phase == phase.AwaitingChildren {
			continue
		}
		stored, err := e.store.Plan(ctx, tx, key, issue.Generation)
		if err != nil {
			return err
		}
		waiting := agentWaiters(stored)
		if len(waiting) == 0 {
			continue
		}
		lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree)
		if err != nil {
			return err
		}
		if lingers {
			continue
		}
		for _, id := range waiting {
			if err := e.handOn(ctx, tx, key, id); err != nil {
				return err
			}
		}
	}
	return nil
}

// joinAgain is the one place the join of a task's needs is queued again: for each issue the fact
// wrote, each step of a running task that waits for its join (record.StepWaitJoin) and whose join
// no task_branch row of the issue's generation makes (record.Store.TaskBranchQueued) goes to
// Engine.start, the start of every step, which joins its needs at that generation. Each start of a
// task queues its join in the transaction that moves it, so such a step is one whose join was
// queued at a generation the issue has left: admission's re-admission carries a running task, its
// wait among it, to the next generation, and the row of the generation it left finishes without
// acting (the outbox's taskBranch), so nothing else would end the wait. An issue of a tree that
// lingers joins nothing, as no move starts anything there, and neither does an issue in
// awaiting_children: a stopped task's restart at its first step that open children hold
// (startAtFirstStep) moves the task into running, whose step waits for its join, and starts
// nothing, and the return's start of that step joins its needs (ReturnFromChildren, Engine.start).
func (e *Engine) joinAgain(ctx context.Context, tx pgx.Tx, keys []string) error {
	var order []string
	for _, key := range keys {
		if !slices.Contains(order, key) {
			order = append(order, key)
		}
	}
	for _, key := range order {
		issue, err := e.store.Issue(ctx, tx, key)
		if err != nil {
			return err
		}
		if issue == nil || issue.Phase == phase.AwaitingChildren {
			continue
		}
		stored, err := e.store.Plan(ctx, tx, key, issue.Generation)
		if err != nil {
			return err
		}
		var unjoined []string
		for _, row := range planTasksOf(stored) {
			if row.State != record.TaskRunning || row.StepWait != record.StepWaitJoin {
				continue
			}
			queued, err := e.store.TaskBranchQueued(ctx, tx, key, issue.Generation, row.Task)
			if err != nil {
				return err
			}
			if !queued {
				unjoined = append(unjoined, row.Task)
			}
		}
		if len(unjoined) == 0 {
			continue
		}
		lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree)
		if err != nil {
			return err
		}
		if lingers {
			continue
		}
		for _, id := range unjoined {
			now, err := e.store.Issue(ctx, tx, key)
			if err != nil {
				return err
			}
			s, err := e.ScopeOf(ctx, tx, *now, id)
			if err != nil {
				return err
			}
			if err := e.startAfterWait(ctx, tx, s, "workflow: the join of a step's task is queued again at its issue's generation",
				"generation", now.Generation); err != nil {
				return err
			}
		}
	}
	return nil
}

// planTasksOf is each task of stored, none for no plan.
func planTasksOf(stored *record.StoredPlan) []record.PlanTask {
	if stored == nil {
		return nil
	}
	return stored.Tasks
}

// startAfterWait starts the step at the place of w's task, of view, the copy of the issue read for
// that task (view), whose start waited for its join or for its agent, through Engine.start, the
// start of every step: at the end of its join (taskJoined), at its join again at the issue's
// generation (joinAgain) and at the hand-on of its freed agent (handOn). Its task is told where its
// work stands and its own pull request (standingPart). Where the start that waited was a move
// forward, the task is told the report of the step that move ended in its place, before the pull
// request (record.PlanTask.StartReport, taskReported), as that start's own would have been
// (transition). Where it was a start again of the task at its first step or a move back
// (Walk.StartReason), the task is told why, and its entry counts as a move back, as that start's
// own would have had it waited for nothing (startAtFirstStep, transition). Where it was the return
// out of awaiting_children, the task is told the return's own task as written
// (record.PlanTask.StartTask: the task the wait held, with the children that merged during it), and
// its entry counts as a move back too, as the return's start would have had it waited for nothing
// (ReturnFromChildren). Its entry names the phase the start that waited moved the issue from
// (record.PhaseRow.EntryFrom), which that start kept on its task's row
// (record.PlanTask.StepWaitFrom, waitsFrom), as that start's own entry would have named it: so an
// entry a role's move back raised out of retro or merging names that phase after a wait for its
// agent too, and the admission pass leaves its start alone (acceptedEntry). The row's values are
// read on s's plan. said is the log line, which names the issue, the task, its place and role, the
// reason, whether a report or a return's task is told, that phase, and args.
func (e *Engine) startAfterWait(ctx context.Context, tx pgx.Tx, s TaskScope, said string, args ...any) error {
	w, view := s.walk, s.issue
	role := w.RoleAt(w.Place)
	if err := e.clearHandoff(ctx, tx, view.Key, role); err != nil {
		return err
	}
	reason := w.StartReason
	var from phase.Phase
	var report, returned string
	if row, ok := s.plan.TaskOf(w.Task); ok {
		from, report, returned = row.StepWaitFrom, row.StartReport, row.StartTask
	}
	e.logOnCommit(ctx, said, append([]any{"issue", view.Key, "tree", view.Tree, "task", w.Task, "place", placeWords(w.Place), "role", role,
		"reason", reason, "reported", report != "", "returned", returned != "", "from", from}, args...)...)
	told := func(at Standing) string {
		return taskReported(view, at, report, reason)
	}
	if returned != "" {
		told = func(Standing) string { return returned }
	}
	return e.start(ctx, tx, s, role, taskPull{}, told, from, reason != "" || returned != "")
}

// agentWaiters is each task of stored, the plan of a run, in the state running whose step waits for
// its agent, the one that waited longest first: by the moment its wait began, then by task id.
// None for no plan; a plan of one task has no step of another task to wait behind, so none either.
func agentWaiters(stored *record.StoredPlan) []string {
	if stored == nil {
		return nil
	}
	var waiting []record.PlanTask
	for _, row := range stored.Tasks {
		if row.State == record.TaskRunning && row.StepWait == record.StepWaitAgent && row.StepWaitSince != nil {
			waiting = append(waiting, row)
		}
	}
	slices.SortStableFunc(waiting, func(a, b record.PlanTask) int { return a.StepWaitSince.Compare(*b.StepWaitSince) })
	ids := make([]string, len(waiting))
	for i, row := range waiting {
		ids[i] = row.Task
	}
	return ids
}

// handOn starts the step of task id of the issue keyed key that waits for its agent, where no
// running step of the issue holds that agent, read again for that task (view): each start before
// it in the same settle changed the issue's running steps. It starts as every step that waited
// starts (startAfterWait).
func (e *Engine) handOn(ctx context.Context, tx pgx.Tx, key, id string) error {
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil || issue == nil {
		return err
	}
	s, err := e.ScopeOf(ctx, tx, *issue, id)
	if err != nil {
		return err
	}
	w := s.walk
	agent := w.AgentAt(w.Place)
	if w.State != record.TaskRunning || w.Wait != record.StepWaitAgent || agent == "" {
		return nil
	}
	if _, busy := issue.StepOfAgent(agent); busy {
		return nil
	}
	return e.startAfterWait(ctx, tx, s, "workflow: a freed agent starts the step that waited for it", "agent", agent)
}

// nameMoveEnd names the end of the run issue stands in on w, the walk of its run, that a move on
// trigger into to, the place of the move's row, ends: the table's own result for the trigger, the
// step the work returns to, and the report handoff names (endOfTransition). The move names it on
// the issue as it stood, before the write that moves it.
func (e *Engine) nameMoveEnd(ctx context.Context, w Walk, issue record.Issue, trigger TriggerKind, to stepSite, handoff record.PhaseRow) error {
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

// restEnded names ancestor as one a root stopped resting on in this fact (dispatch://INFRA-3320
// R8): the root left the workflow (tellRestingLeft), or its step's wait on ancestor's approval
// request ended (tellPendingEnded). A parent held before its retro waits for each root still in
// the workflow that rests on it (StartHeldRetro), and neither end writes a record of the parent's
// tree, so the fact's settle reads ancestor's tree as it reads a tree the fact wrote
// (releaseHeldParents). A transaction no fact opened has no settle to read it in, and names
// nothing: the next admission pass of the held parent's project reads it again (admit's
// childrenWaits).
func (e *Engine) restEnded(ctx context.Context, ancestor string) {
	if runs, ok := ctx.Value(factRunsKey{}).(*factRuns); ok && !slices.Contains(runs.unrested, ancestor) {
		runs.unrested = append(runs.unrested, ancestor)
	}
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
		// end is written for the step of that run's plan, whichever run the issue stands in now:
		// the walk of the run's task, read on its scope (scopeOrNoPlan).
		stood.Generation, stood.Hold = row.Generation, nil
		ranPlan, err := e.store.Plan(ctx, tx, stood.Key, stood.Generation)
		if err != nil {
			return err
		}
		ran, onPlan := scopeOrNoPlan(stood, ranPlan, row.Place.Task)
		stood, w := ran.issue, ran.walk
		stood.Hold = nil
		var onWalk bool
		if onPlan {
			stood.Phase, onWalk = phaseAt(w, row.Place)
		}
		if !onWalk {
			if err := e.endOffWalk(ctx, tx, row, runs.endOf(run)); err != nil {
				return err
			}
			continue
		}
		site := placeNamed(record.TaskPlace{Kind: row.Place.Kind, Position: row.Place.Place}, stood.Phase)
		if err := e.endStep(ctx, tx, stood, w, site, runs.endOf(run)); err != nil {
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
// root is read. A run is over when its issue has no record, or the task of its run stands at
// another place on the walk of its run or in another generation (standingStep, on the copy of the
// issue read for that task, viewOn, which a task its plan no longer holds has none of, and which a
// task its plan holds stands in no step of); when a start of its place begins; or when its tree's
// root lingers, unless it is a merge that can still land in that linger (mergeCanLand, whose
// closed lingers runs holds).
func (e *Engine) runOver(ctx context.Context, tx pgx.Tx, runs *factRuns, run runAt, now *record.Issue, restarted bool, read issueReader) (bool, error) {
	if restarted || now == nil {
		return true, nil
	}
	if now.Generation != run.generation {
		return true, nil
	}
	stored, err := e.store.Plan(ctx, tx, now.Key, now.Generation)
	if err != nil {
		return false, err
	}
	s, onPlan := scopeOrNoPlan(*now, stored, run.place.Task)
	view, w := s.issue, s.walk
	if !onPlan {
		return true, nil
	}
	if step, ok := standingStep(w, view); !ok || step.place != run.place {
		return true, nil
	}
	root := now
	if now.Tree != now.Key {
		if root, err = read(now.Tree); err != nil {
			return false, err
		}
	}
	if root == nil || !root.Lingers() {
		return false, nil
	}
	if !awaitsMerge(view) {
		return true, nil
	}
	canLand, err := e.mergeCanLand(ctx, tx, runs, view, *root)
	return !canLand, err
}

// uncountedRunOpen is the boot's answer (OpenUncountedRuns) to whether s's copy, as read reads its
// tree, stands in an open run that no start of this release counted, and that run: the copy stands
// in a step whose runs are recorded on the walk of its task (standingStep), the end rule does not
// hold that run over (runOver), its tree is not the operator's, which runs no step of the workflow
// (record.OperatorHoldsTree), and its generation started a run (generationStarted).
func (e *Engine) uncountedRunOpen(ctx context.Context, tx pgx.Tx, runs *factRuns, s TaskScope, read issueReader) (runAt, bool, error) {
	issue, w := s.issue, s.walk
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
// (record.Store.MergePullRequestRow), with none of what the pull request remembers of its settled
// heads.
func (e *Engine) mergeCanLand(ctx context.Context, tx pgx.Tx, runs *factRuns, member, root record.Issue) (bool, error) {
	if record.Lingering(root, e.now()) && !runs.closed[root.Tree] {
		return true, nil
	}
	pr, err := e.store.MergePullRequestRow(ctx, tx, member.Key)
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
// phase.Order for a run with no plan rows), the phase of the position whose step (placeOnWalk) is
// at place, so of two steps that run as step, each its own.
func phaseAt(w Walk, place record.StepPlace) (phase.Phase, bool) {
	for at, p := range w.Line {
		if step, _, _, ok := placeOnWalk(w, placeNamed(placeAt(w.Line, at), p)); ok && step == place {
			return p, true
		}
	}
	return "", false
}
