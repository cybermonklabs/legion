package workflow

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// The retro after a close (dispatch://INFRA-5602). An issue Legion started whose last run had no
// retro gets that retro after its close, as a merged issue gets its production check after its
// merge: the close keeps the retro's agent and its session, starts its step row, and hands it a
// task with no phase, fenced on the run's record (record.RunRetro) rather than on the issue's
// phase, since a closed issue stands in none. Every close of an issue runs through leave, which
// asks retroAtClose once and opens the retro with openRetroAfterClose. The retro ends by its
// report (retroReported), at its bound (retroBoundEnded) or at a todo that takes the issue into a
// new run (EndOpenRetro); finishRetroAfterClose stops what the retro kept.

// RetroAfterCloseBound is how long the retro Legion runs after an issue's close may wait for its
// report: 24 hours from the start of the retro's step row, and never sooner. At the bound the
// retro ends as stopped with no rating, its claim is closed, and a later completion is refused
// with the reason (record.RetroEndBound). It is well past the longest retro measured when the
// bound was chosen and inside the 72-hour default linger, which waits for the retro
// (lingerExpired).
const RetroAfterCloseBound = 24 * time.Hour

// LingerEventID is the event id a tree's linger expiry is applied under, with source "outbox"
// (intake.LingerExpired, the linger_close row of root at generation): applied once, it closes the
// tree, and the store keeps the id as processed. The end of the last retro after a close of a tree
// past its linger reads it (closeTreeAfterRetros): a tree whose expiry already applied is closed
// again by that end's own rows, since a second linger_close row under the same id would apply
// nothing.
func LingerEventID(root string, generation uint64) string {
	return fmt.Sprintf("linger:%s:%d", root, generation)
}

// retroAgentRole is the role whose agent does issue's retro after its close: the one place that
// chooses it, so the close, the start and the claim the close keeps all name the same agent. It is
// the role that works the retro phase (phase.RoleFor), the implementer.
func (e *Engine) retroAgentRole(issue record.Issue) claim.Role {
	return phase.RoleFor(phase.Retro)
}

// retroCloseKind is what a close owes the retro of the issue's newest run (retroAtClose).
type retroCloseKind int

const (
	// retroCloseNone: nothing. The issue never started, its newest run had a retro row before the
	// close, or the close is no done.
	retroCloseNone retroCloseKind = iota
	// retroCloseOwed: the newest run started and no retro of it ever started; the close starts
	// it.
	retroCloseOwed
	// retroCloseRunning: the issue stands in its retro, delivered to its agent, which keeps its
	// turn, its step row and its claim.
	retroCloseRunning
	// retroCloseQueued: the issue stands in its retro whose start is queued and not delivered; the
	// task goes out with no phase on the step row the move into retro started.
	retroCloseQueued
)

// retroClosePlan is what a close does for the retro of the issue's run: kind, the run (its
// generation), the role and the claim token of the retro's agent (retroAgentRole), the phase
// entry of a retro running at the close (0 for every other kind), and when the retro's step row
// started, for a retro already running or queued (the bound counts from it).
type retroClosePlan struct {
	kind       retroCloseKind
	run        uint64
	role       claim.Role
	agent      claim.Token
	entry      int64
	retroStart time.Time
}

// handsTask is whether the close hands the retro's agent the task of the retro plan names: a retro
// owed, or one whose start was queued and not delivered. A running retro's turn goes on.
func (p retroClosePlan) handsTask() bool {
	return p.kind == retroCloseOwed || p.kind == retroCloseQueued
}

// retroAtClose is the one answer to whether the close of issue at status owes the retro of its
// newest run, read before the close writes the issue done, so an issue standing in its retro is
// still read there. Only a done owes one, so a move to backlog, icebox or triage starts nothing
// and the later done does. An issue whose key is no issue key records no steps, and one with no
// row in run_retros never started. The newest run that started owes its retro unless a retro row
// of it started before the close, of any result: one that passed, one that moved the work back,
// one stopped, and a retro after an earlier close of the same run, whether it ended or is still
// open. An issue in its retro (not held for its children, whose retro has not started) has that
// retro, and the close keeps it rather than owing another: running, or queued and not yet
// delivered, as a start of the agent queued for the retro phase's work says (queuedStartActs: a
// start queued for news alone brings it none). Its bound counts from the start of the retro's row
// that runs now, which, in a run whose work went back from an earlier retro, is later than the
// run's first retro start.
func (e *Engine) retroAtClose(ctx context.Context, tx pgx.Tx, issue record.Issue, status string) (retroClosePlan, error) {
	if status != record.DoneStatus || !recordsSteps(issue) {
		return retroClosePlan{}, nil
	}
	newest, err := e.store.NewestRunRetro(ctx, tx, issue.Key)
	if err != nil || newest == nil {
		return retroClosePlan{}, err
	}
	role := e.retroAgentRole(issue)
	agent, err := stepClaim(issue, issue.Key, role)
	if err != nil {
		return retroClosePlan{}, err
	}
	plan := retroClosePlan{run: newest.Generation, role: role, agent: agent}
	switch {
	case !newest.RetroHad():
		plan.kind = retroCloseOwed
		return plan, nil
	case newest.Generation != issue.Generation || issue.Phase != phase.Retro:
		return retroClosePlan{}, nil
	}
	plan.retroStart = *newest.RetroStartedAt
	w, err := e.walk(ctx, tx, issue)
	if err != nil {
		return retroClosePlan{}, err
	}
	if step, ok := stepAt(w, issue, phase.Retro); ok {
		latest, err := e.store.StepStarts(ctx, tx, issue.Key, step.place)
		if err != nil {
			return retroClosePlan{}, err
		}
		if !latest.StartedAt.IsZero() {
			plan.retroStart = latest.StartedAt
		}
	}
	run, err := e.store.RoleRun(ctx, tx, agent, issue.Key, role, issue.Generation)
	if err != nil {
		return retroClosePlan{}, err
	}
	if queuedStartActs(run, 0, phase.Retro) {
		plan.kind = retroCloseQueued
		return plan, nil
	}
	row, err := e.phaseRow(ctx, tx, issue.Key, role)
	if err != nil {
		return retroClosePlan{}, err
	}
	plan.kind, plan.entry = retroCloseRunning, row.Entry
	return plan, nil
}

// openRetroAfterClose opens the retro plan names on issue's run, in the close's transaction and
// after the close wrote the issue out of the workflow. A retro owed starts its step row under the
// agent's claim, at the run's retro (startStep, which marks the run's retro started); a retro
// running or queued at the close keeps the row it has. The run's record opens the retro with its
// bound, RetroAfterCloseBound after that row's start, the agent and the entry of a running retro,
// and a retro_bound row due at the bound ends it should nobody report it (retroBoundEnded). A
// retro owed or queued is handed its task (retroAfterCloseTask, which names pr, the issue's pull
// request row as the close read it, nil for none) by a start of its agent that names the run and
// no phase, at the issue's generation, as a production check's look names its check: the outbox
// fences it on the run's open retro, resumes a suspended claim, relaunches a retired one in its
// kept session and creates one that never launched. A running retro gets no task: its turn goes
// on, and its completion is taken on its entry. The walk of the run is read for a retro handed its
// task alone, whose step row and task stand on it.
func (e *Engine) openRetroAfterClose(ctx context.Context, tx pgx.Tx, issue record.Issue, plan retroClosePlan, pr *record.PullRequest) error {
	if plan.kind == retroCloseNone {
		return nil
	}
	stood := retroStood(issue, plan.run)
	var w Walk
	if plan.handsTask() {
		var err error
		if w, err = e.walk(ctx, tx, stood); err != nil {
			return err
		}
	}
	retroStart := plan.retroStart
	if plan.kind == retroCloseOwed {
		step, ok := standingStep(w, stood)
		if !ok {
			return fmt.Errorf("open the retro after the close of %s run %d: the walk of its run holds no retro", issue.Key, plan.run)
		}
		if err := e.startStep(ctx, tx, stood, step, plan.agent); err != nil {
			return err
		}
		// The step's start marked the run's retro started at the moment the step row holds
		// (startStep), and the bound counts from that moment.
		run, err := e.store.RunRetro(ctx, tx, issue.Key, plan.run)
		if err != nil {
			return err
		}
		if run == nil || run.RetroStartedAt == nil {
			return fmt.Errorf("open the retro after the close of %s run %d: its start is not recorded on the run", issue.Key, plan.run)
		}
		retroStart = *run.RetroStartedAt
	}
	bound := retroStart.Add(RetroAfterCloseBound).Truncate(time.Microsecond)
	if err := e.store.OpenRetroAfterClose(ctx, tx, issue.Key, plan.run, issue.Tree, e.now().Truncate(time.Microsecond), bound, plan.agent, plan.entry); err != nil {
		return err
	}
	if err := e.enqueueAt(ctx, tx, issue.Key, record.RetroBound{Generation: plan.run}, bound); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: the retro after the close opened", "issue", issue.Key, "run", plan.run, "agent", plan.agent,
		"bound", bound, "running", plan.kind == retroCloseRunning)
	if !plan.handsTask() {
		return nil
	}
	at, err := e.standingPart(ctx, tx, stood, w)
	if err != nil {
		return err
	}
	topic, err := rootArchitectTopic(issue)
	if err != nil {
		return err
	}
	return e.enqueue(ctx, tx, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: plan.role, Generation: issue.Generation,
		RetroRun: plan.run, Task: retroAfterCloseTask(stood, at, pr, topic)})
}

// taskPullRequest is issue's pull request row, nil for none, where plan hands the retro's agent its
// task, which names it, and nil otherwise: what a root's leave, which reads no pull request of its
// own, hands openRetroAfterClose.
func (e *Engine) taskPullRequest(ctx context.Context, tx pgx.Tx, issue record.Issue, plan retroClosePlan) (*record.PullRequest, error) {
	if !plan.handsTask() {
		return nil, nil
	}
	return e.store.PullRequestRow(ctx, tx, issue.Key)
}

// retroStood is issue as it stood in the retro of run: at that generation, in phase.Retro, with no
// hold and no wait on a person. The retro's step row, its walk, its task and its rating are all
// of that run, whichever generation the issue has reached since (a todo).
func retroStood(issue record.Issue, run uint64) record.Issue {
	issue.Generation, issue.Phase, issue.Hold, issue.Awaiting = run, phase.Retro, nil, nil
	return issue
}

// rootArchitectTopic is the Envoy role topic of the architect of issue's tree root: the role topic
// prefix and that architect's claim token, as the daemon spells every role topic it publishes to.
// A retro's agent sends what it finds there (R2), whichever architect owns the issue.
func rootArchitectTopic(issue record.Issue) (string, error) {
	token, err := stepClaim(issue, issue.Tree, claim.RoleArchitect)
	if err != nil {
		return "", err
	}
	return notify.RoleTopicPrefix + string(token), nil
}

// findingsClause is what the task of a retro whose issue's pull request merged, open or closed,
// says of its findings (R2): one the retro finds unmet goes to the tree's root architect on its
// role topic, which puts it to the person as one decision, and one that another open child of the
// same parent carries is pending and goes to no one.
func findingsClause(topic string) string {
	return " A requirement this retro finds unmet goes to the tree's root architect: publish it to " + topic +
		", and that architect puts it to the person as one decision. A requirement that another open child of the same parent carries is pending and goes to no one."
}

// retroAfterCloseTask is the task of the retro after the close of stood's issue: the retro's
// first line and the standing of its run's retro, as every retro task opens (the working copy,
// and for a run with plan rows its task and its `Steps:` line), the pull request where the issue
// has one, then what R2 says: the issue is closed, the retro moves it nowhere and the retro
// skill's steps that move work back do not apply, and where a finding goes (findingsClause, the
// root architect's topic), then how the retro is reported. The start that carries it names no
// phase.
func retroAfterCloseTask(stood record.Issue, at Standing, pr *record.PullRequest, topic string) string {
	text := continueLine(stood) + at.lines
	if pr != nil && pr.Number != 0 {
		text += fmt.Sprintf(" %s: #%d.", pullRequestLabel, pr.Number)
	}
	text += " This issue is closed: the retro moves it nowhere, and the retro skill's steps that move work back do not apply to it." +
		findingsClause(topic) +
		" Report the retro with the legion tool's handoff_complete, with plan_rating and plan_reason, and no verdict."
	return text + at.owners
}

// spareRetroAgent adds to spared the role of the agent of issue's open retro after a close, where
// it has one: a done issue moved to backlog, icebox or triage, or closed again, keeps that retro's
// agent and its pod while the retro is open.
func (e *Engine) spareRetroAgent(ctx context.Context, tx pgx.Tx, issue record.Issue, spared []claim.Role) ([]claim.Role, error) {
	open, err := e.store.OpenRetroOfIssue(ctx, tx, issue.Key)
	if err != nil || open == nil {
		return spared, err
	}
	return spareRole(spared, open.Agent), nil
}

// spareRole adds the role of agent's claim token to spared, once.
func spareRole(spared []claim.Role, agent claim.Token) []claim.Role {
	if role, ok := agent.Role(); ok && !slices.Contains(spared, role) {
		spared = append(spared, role)
	}
	return spared
}

// retroHoldsRole says whether role's claim on issue is the agent of the issue's open retro after a
// close: the close or end of another work of the same role, a production check under
// `production_check: implementer`, then stops nothing of it, since the retro's end does
// (finishRetroAfterClose).
func (e *Engine) retroHoldsRole(ctx context.Context, tx pgx.Tx, issue record.Issue, role claim.Role) (bool, error) {
	open, err := e.store.OpenRetroOfIssue(ctx, tx, issue.Key)
	if err != nil || open == nil {
		return false, err
	}
	agent, ok := open.Agent.Role()
	return ok && agent == role, nil
}

// retroReported takes the report of a retro after a close (intake.RetroReport, which the
// completion route applies for the claim that serves it): the retro's step row ends passed, the
// rating of the plan is written onto that run's record as a retro's completion writes it
// (rateRetro), the run's record ends passed by the report, and nothing writes a status, so the
// issue stays as its close left it. It is taken whatever the tree's linger, since the retro runs
// after the close (refusedLingering is a running issue's). A report of a retro that is not open is
// refused RETRO_NOT_OPEN, naming how it ended, and one from a claim other than the retro's agent
// RETRO_AGENT_MISMATCH; neither writes anything, and each leaves its event unspent.
func (e *Engine) retroReported(ctx context.Context, tx pgx.Tx, fact intake.RetroReport) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	run, err := e.store.RunRetro(ctx, tx, fact.Issue, fact.Generation)
	if err != nil {
		return intake.Result{}, err
	}
	if run == nil || !run.Open() {
		result := refused("RETRO_NOT_OPEN", retroNotOpen(fact.Issue, fact.Generation, run))
		result.Unspent = true
		return result, nil
	}
	if run.Agent != fact.Claim {
		result := refused("RETRO_AGENT_MISMATCH", fmt.Sprintf("the retro after the close of %s run %d is %s's, not %s's; this report changed nothing",
			fact.Issue, fact.Generation, run.Agent, fact.Claim))
		result.Unspent = true
		return result, nil
	}
	if err := e.nameRetroEnd(ctx, tx, *issue, run.Generation, StepEnd{Result: ResultPassed}); err != nil {
		return intake.Result{}, err
	}
	ended, _, err := e.store.EndRetroAfterClose(ctx, tx, fact.Issue, fact.Generation, e.now(), record.RetroOutcomePassed, record.RetroEndReport)
	if err != nil {
		return intake.Result{}, err
	}
	if err := e.rateRetro(ctx, tx, retroStood(*issue, run.Generation), fact.PlanRating); err != nil {
		return intake.Result{}, err
	}
	// The report is a completion, which marks the issue moved as handoff's does.
	if err := e.store.MarkIssueMoved(ctx, tx, issue.Key, e.now()); err != nil {
		return intake.Result{}, err
	}
	e.logOnCommit(ctx, "workflow: the retro after the close reported", "issue", issue.Key, "run", run.Generation, "agent", run.Agent,
		"rating", fact.PlanRating.Rating)
	return intake.Result{}, e.finishRetroAfterClose(ctx, tx, *issue, ended)
}

// retroNotOpen is why a report of the retro after the close of issue's run is refused: no such
// retro was opened, or it ended, by what and how (run, nil for no row of the run).
func retroNotOpen(issue string, generation uint64, run *record.RunRetro) string {
	if run == nil || !run.AfterClose() {
		return fmt.Sprintf("%s run %d has no retro after its close; this report changed nothing", issue, generation)
	}
	return fmt.Sprintf("the retro after the close of %s run %d ended %s %s; this report changed nothing", issue, generation, run.Outcome, run.EndedHow())
}

// retroBoundEnded is the bound of the retro after a close coming due (intake.RetroBoundEnded, the
// retro_bound row openRetroAfterClose queued): the retro of the run the fact names, still open,
// ends stopped with no rating, its run's record ends stopped by the bound, and what it kept stops
// (finishRetroAfterClose). Never sooner than the bound: a fact applied before it is a wait until
// then. A retro already ended, or a run with none, changes nothing.
func (e *Engine) retroBoundEnded(ctx context.Context, tx pgx.Tx, fact intake.RetroBoundEnded) (intake.Result, error) {
	run, err := e.store.RunRetro(ctx, tx, fact.Issue, fact.Generation)
	if err != nil || run == nil || !run.Open() {
		return intake.Result{}, err
	}
	if e.now().Before(*run.BoundAt) {
		return intake.Result{}, wait.Until(*run.BoundAt, fmt.Sprintf("the retro after the close of %s run %d is bound at %s", fact.Issue, fact.Generation, run.BoundAt.Format(time.RFC3339)))
	}
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	if err := e.nameRetroEnd(ctx, tx, *issue, run.Generation, stepStopped); err != nil {
		return intake.Result{}, err
	}
	ended, _, err := e.store.EndRetroAfterClose(ctx, tx, fact.Issue, fact.Generation, e.now(), record.RetroOutcomeStopped, record.RetroEndBound)
	if err != nil {
		return intake.Result{}, err
	}
	e.warnOnCommit(ctx, "workflow: the retro after the close ended at its bound with no report", "issue", issue.Key, "run", run.Generation,
		"agent", run.Agent, "bound", *run.BoundAt)
	return intake.Result{}, e.finishRetroAfterClose(ctx, tx, *issue, ended)
}

// EndOpenRetro ends issue's open retro after a close, where it has one, as stopped by a todo with
// no rating: the todo takes the issue into a new run, as a child's re-entry
// (ReenterChild) and admission's re-admission (admit's readmit) each do, and the closed run's
// retro ends there, as the re-entry ends an open production check as not run. Its step row ends
// stopped at the fact's settle. It stops no claim and takes no pod: those are the new run's.
func (e *Engine) EndOpenRetro(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
	open, err := e.store.OpenRetroOfIssue(ctx, tx, issue.Key)
	if err != nil || open == nil {
		return err
	}
	if err := e.nameRetroEnd(ctx, tx, issue, open.Generation, stepStopped); err != nil {
		return err
	}
	if _, _, err := e.store.EndRetroAfterClose(ctx, tx, issue.Key, open.Generation, e.now(), record.RetroOutcomeStopped, record.RetroEndTodo); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a todo ended the open retro after the close", "issue", issue.Key, "run", open.Generation, "agent", open.Agent)
	return nil
}

// nameRetroEnd names end as the end of the retro step row of issue's run (nameEnd), which the
// fact's settle writes once the run's record no longer holds the row open (runOver).
func (e *Engine) nameRetroEnd(ctx context.Context, tx pgx.Tx, issue record.Issue, run uint64, end StepEnd) error {
	stood := retroStood(issue, run)
	w, err := e.walk(ctx, tx, stood)
	if err != nil {
		return err
	}
	step, ok := standingStep(w, stood)
	if !ok {
		return nil
	}
	return e.nameEnd(ctx, stood, step, end)
}

// finishRetroAfterClose stops what the retro after issue's close kept, once ended ended it by its
// report or its bound. The retro's agent is done with the issue, so its claim is closed (retired,
// its session kept), unless it is the role of the issue's open production check, whose close
// stops it (closeCheck). The pod goes with an IssueSuspend, unless the check is still open: its
// close writes the suspension. An issue back in the workflow keeps all of it: its claims and its
// pod are the new run's.
//
// The tree's root architect, which a message to its role topic woke while a retro after a close
// of the tree was open, is the daemon's to suspend again once no such retro is open and the
// root has left the workflow or lingers, and the root's pod goes with it where no check of
// the root is open. A tree past its linger closes again (closeTreeAfterRetros).
func (e *Engine) finishRetroAfterClose(ctx context.Context, tx pgx.Tx, issue record.Issue, ended record.RunRetro) error {
	if InWorkflow(issue) {
		return nil
	}
	root, err := e.store.Issue(ctx, tx, issue.Tree)
	if err != nil {
		return err
	}
	if root == nil {
		return fmt.Errorf("finish the retro after the close of %s: its tree root %s is not recorded", issue.Key, issue.Tree)
	}
	open, err := e.store.OpenRetrosOfTree(ctx, tx, issue.Tree)
	if err != nil {
		return err
	}
	check, err := e.store.ProductionCheck(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	checking := check != nil && check.Open()
	reason := fmt.Sprintf("the retro after %s's close ended %s", issue.Key, ended.Outcome)
	if role, ok := ended.Agent.Role(); ok && !(checking && check.Role == role) {
		if err := e.enqueue(ctx, tx, issue.Key, record.SuperviseRequest{Op: "issue_close", Tree: issue.Tree, Role: role,
			Generation: issue.Generation, Reason: reason}); err != nil {
			return err
		}
	}
	if !checking {
		if err := e.enqueue(ctx, tx, issue.Key, record.IssueSuspend{Tree: issue.Tree, Generation: issue.Generation, TreeGeneration: root.Generation}); err != nil {
			return err
		}
	}
	if len(open) == 0 && record.Stopped(*root) {
		if err := e.suspend(ctx, tx, *root, claim.RoleArchitect, fmt.Sprintf("no retro after a close of the tree of %s is open", root.Key)); err != nil {
			return err
		}
		if root.Key != issue.Key {
			rootCheck, err := e.store.ProductionCheck(ctx, tx, root.Key)
			if err != nil {
				return err
			}
			if rootCheck == nil || !rootCheck.Open() {
				if err := e.enqueue(ctx, tx, root.Key, record.IssueSuspend{Tree: root.Tree, Generation: root.Generation, TreeGeneration: root.Generation}); err != nil {
					return err
				}
			}
		}
	}
	return e.closeTreeAfterRetros(ctx, tx, *root, open)
}

// closeTreeAfterRetros closes root's tree again once the last open retro after a close of a tree
// past its linger has ended, open being the tree's retros still open: the retro reopened the
// tree's lifecycle for its agent, and nothing else would close it. Where the linger's expiry has
// not applied — it waited for the retro (lingerExpired) — a linger_close row due now applies it,
// as the close of the tree's last production check re-arms it (closeCheck). Where it applied
// already, a second row under the same event would apply nothing (LingerEventID), so the end writes
// the close itself: a tree_close of every member's claims and the removal of every member's
// workspace (closeTreeRows). A tree with a retro or a production check still open, or whose linger
// still runs, waits for that.
func (e *Engine) closeTreeAfterRetros(ctx context.Context, tx pgx.Tx, root record.Issue, open []record.RunRetro) error {
	if len(open) > 0 || root.LingerUntil == nil || e.now().Before(*root.LingerUntil) {
		return nil
	}
	checks, err := e.store.OpenProductionChecksOfTree(ctx, tx, root.Tree)
	if err != nil || len(checks) > 0 {
		return err
	}
	expired, err := e.store.Processed(ctx, tx, "outbox", LingerEventID(root.Key, root.Generation))
	if err != nil {
		return err
	}
	if !expired {
		e.logOnCommit(ctx, "workflow: the last open retro after a close of a tree past its linger ended; the tree's close is re-armed", "tree", root.Key,
			"generation", root.Generation)
		return e.enqueue(ctx, tx, root.Key, record.LingerClose{Generation: root.Generation})
	}
	members, err := e.treeMembers(ctx, tx, root)
	if err != nil {
		return err
	}
	if err := e.closeTreeRows(ctx, tx, root, members); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: the last open retro after a close of a tree past its linger ended; the tree closes again", "tree", root.Key,
		"generation", root.Generation, "issues", len(members))
	return e.closeLinger(ctx, root.Tree)
}

// retroWake wakes the architect of a tree root whose claim the tree's close suspended, or the
// linger's end retired, on a message somebody published to its role topic while no live session
// held it (a listener's role_message report naming held, a claim that is not released, as the
// listener's report read it: Pods.ClaimByToken): while a retro after a close of the tree is open,
// the retro's agent publishes its findings there, and the woken architect gets the message Envoy
// kept. It wakes nothing for a claim that is no tree root's own architect while a retro after a
// close of its tree is open (RetroWakeOf), for a claim in any other state (a release's is
// wakeReleased's, a live one hears the message itself), and where such a wake is already queued.
// The wake is a start of the architect with the wake's kind, no phase and no run, marked as a
// retro's wake (record.SuperviseRequest.RetroWake), which the outbox lets past the guard that
// drops a wake of a claim the daemon suspended.
func (e *Engine) retroWake(ctx context.Context, tx pgx.Tx, held supervise.Claim) error {
	if held.Role != claim.RoleArchitect ||
		!(held.State == supervise.StateSuspended && held.SuspendedBy == supervise.SuspendedByDaemon || held.State == supervise.StateRetired) {
		return nil
	}
	token := held.Token
	root, err := e.store.Issue(ctx, tx, held.Tree)
	if err != nil || root == nil {
		return err
	}
	if _, architect, err := RetroWakeOf(ctx, e.store, tx, *root, token); err != nil || !architect {
		return err
	}
	run, err := e.store.RoleRun(ctx, tx, token, root.Key, claim.RoleArchitect, root.Generation)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(run.Queued, func(queued record.QueuedSupervise) bool {
		return queued.Request.Op == "start" && queued.Request.RetroWake
	}) {
		return nil
	}
	wake := wakeRow(*root, claim.RoleArchitect, pods.WakeRoleMessage, 0, retroArchitectWakeTask(string(token)))
	wake.RetroWake = true
	e.logOnCommit(ctx, "wake: a message to the root architect of a closed tree wakes it while a retro after a close of the tree is open",
		"tree", root.Key, "architect", token, "state", held.State)
	return e.enqueue(ctx, tx, root.Key, wake)
}

// retroArchitectWakeTask is the task of a root architect woken for a message to its role topic
// token while a retro after a close of its tree is open (retroWake): what stopped its session, the
// message, and what a retro's finding asks of it.
func retroArchitectWakeTask(token string) string {
	return "Your tree closed, and the daemon stopped your session with it. A retro Legion runs after the close of an issue of your tree is open, " +
		"and this wake is a message somebody published to your role topic (" + token + ") while your session was stopped. " +
		"Envoy keeps a role message for one hour: a message kept for your role reaches this session as an Envoy card that shows kept_at, " +
		"possibly before this task. A requirement a retro finds unmet is yours to put to the person as one decision; " +
		"one that another open child of the same parent carries is pending and goes to no one."
}
