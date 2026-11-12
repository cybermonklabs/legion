package workflow

import (
	"context"
	"crypto/rand"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// RoleFor is the role that works a phase: the one a transition starts, and the one whose handoff
// record the `legion` tool's `handoff_complete` reads for a phase with a handoff (phase.Handoff,
// whose writer a test holds to this; packages/pi-shared/src/handoff-record.ts). A phase no role
// works — awaiting_person, done, held — has none. Accepting is the tree architect's, a claim per
// tree that no row starts: every site
// that starts, resumes or reports a phase's worker skips it (start, admit's startMidPhaseMembers,
// booted, Stranded).
//
// It is exported for admission too, which starts the mid-phase children of a tree it re-admits
// and needs each child's own phase's role to start it on.
func RoleFor(p phase.Phase) claim.Role {
	switch p {
	case phase.Planning:
		return claim.RolePlanner
	case phase.Implementing, phase.Retro:
		return claim.RoleImplementer
	case phase.Testing:
		return claim.RoleTester
	case phase.Reviewing:
		return claim.RoleReviewer
	case phase.Accepting:
		return claim.RoleArchitect
	case phase.Merging:
		return claim.RoleMerger
	default:
		return ""
	}
}

// PhaseFinished says whether issue's phase is over by its own terminating fact rather than by its
// worker's completion, so no worker of it is started or resumed for it. Merging's fact is the pull
// request being merged: the merger merges, the daemon merges nothing, and a merged root then
// stands in merging until the rest of its tree leaves the workflow (closeMerged), which can be
// days. Accepting's is the architect's sign-off, which moves the issue on in the transaction that
// records it, so an issue standing in accepting is always still waiting for it; every other
// phase ends with its worker's completion, which moves the issue too. A recorded completion
// answers nothing here: the merger's is recorded and moves no issue.
func PhaseFinished(ctx context.Context, store record.Store, tx pgx.Tx, issue record.Issue) (bool, error) {
	if issue.Phase != phase.Merging {
		return false, nil
	}
	pr, err := store.PullRequest(ctx, tx, issue.Key)
	if err != nil || pr == nil {
		return false, err
	}
	return pr.State == record.PullRequestMerged, nil
}

func (e *Engine) phaseFinished(ctx context.Context, tx pgx.Tx, issue record.Issue) (bool, error) {
	return PhaseFinished(ctx, e.store, tx, issue)
}

// PhaseWorkerRole is the role that works issue's phase, and "" where no worker is owed a turn: a
// phase no role works (held among them), and the architect, whose turn is its own rather than a
// worker's. Both refusals are OpenPhaseRole's, read here rather than stated a second time.
//
// It is the one statement of "the role that works this issue's phase" a phase worker's silence
// is judged by, so that the three readers of that term cannot drift apart: SilentPhaseWorker
// holds the role of the claim it is given against it, the handler of a silence fact asks it of
// the issue it has read before it reads anything else (workerSilent), and admission selects the
// run of every candidate of its two repairs by it (admit's phaseWorkers).
func PhaseWorkerRole(issue record.Issue) claim.Role {
	return OpenPhaseRole(issue.Phase)
}

// StopActs is whether a queued stop, row id, still acts on its claim when the outbox runs it, with
// lastStart the newest start the outbox ran against the claim (its last_start_row), and root the
// issue's tree root as recorded, read only for a tree close. The outbox executor asks it of every
// suspend, issue close and tree close, and StartFor of every queued stop, so all read one rule. A
// tree close acts while its tree lingers at the root generation it names (record.Issue.LingersAt):
// never once re-admission has moved the root on. A suspend — an issue's close, a child's leave, or
// a re-entered child's interrupted run — a release — the idle pod's, which keeps the session and
// the phase — and an issue close — a child's leave as done — act unless
// a newer start has already run: that start replaced the run the stop was written for, so acting
// would stop the run it began and retire the task with it, whatever the retry timing was. A row
// with no id is not older than anything: the store gives every row one, and an unknown id must not
// silently drop a stop. Any other operation is not a stop.
func StopActs(id int64, stop record.SuperviseRequest, lastStart int64, root *record.Issue) bool {
	switch stop.Op {
	case "tree_close":
		return root != nil && root.LingersAt(stop.Linger)
	case "suspend", "release", "issue_close":
		return id <= 0 || id >= lastStart
	default:
		return false
	}
}

// StartFor is whether a re-admitted tree's promotion starts the role of a mid-phase child, from
// run, the role's queued rows and claim for generation, with the child in phase current and root
// its re-admitted tree root. It does
// not when the role is already started for this run: the newest of its operations that will still
// act is a start. A stop that still acts (StopActs) undoes any older start, which may run first,
// so past one only a newer queued start for current counts. With none, a queued start for current
// counts, and so does a claim of root's tree that may have a process: a live one, or one whose
// launch is uncertain holding current's task for generation undelivered or unconfirmed (the task
// its start gave it). No start acts while the tree lingers, and the tree's close queues a suspend
// of every claim, so such a claim was started since the tree ran again. A claim still in another
// tree was not: a carried child's role whose turn in its old tree has not ended is live there, and
// promotion starts it, a start that waits until the claim has stopped in that tree (the outbox's
// stopElsewhere) and then resumes it in this one. A failed, retired, suspended or queued claim runs
// nothing, so promotion starts it whatever task it holds. Whether the start's resume task is
// delivered is the outbox executor's decision when the start runs: not to a claim that holds a task
// for the same generation and phase by then (record.SuperviseRequest's ResumeTask).
//
// Stranded reads the same rule for a failed claim, and for a suspended one no operator suspended: a
// claim for which StartFor is true has no queued start that will wake it. A
// suspension that holds for the operator (supervise's RuleHolds) is woken by no start, queued or
// not, since each waits for the operator's resume, so Stranded lists it without asking. The
// architect's start names no phase, so its current is "".
//
// Its one reading of run.Queued is queuedStartActs, which SilentPhaseWorker reads too: a start
// already queued for the phase is what makes a promotion start needless here and a quiet worker
// no silent one there, and the two rules must answer that term alike. The stop that term is
// weighed against is read once here (actingStop) and handed to it.
func StartFor(run record.RoleRun, root record.Issue, generation uint64, current phase.Phase) bool {
	stop := actingStop(run, root)
	if queuedStartActs(run, stop, current) {
		return false
	}
	held := run.Claim
	if stop != 0 || held == nil || held.Tree != root.Key {
		return true
	}
	holds := held.Pending != nil && held.Pending.StillWorked(generation, current, held.State)
	return !slices.Contains(supervise.LiveStates(), held.State) && !(holds && held.State == supervise.StateLaunchUncertain)
}

// actingStop is the id of the newest queued stop of run that will still act when the outbox runs
// it (StopActs), and 0 where none will: the row past which an older start counts for nothing,
// since a stop that acts undoes any start before it and may run first. The stops are weighed
// against the newest start the outbox has already run on the claim
// (record.RoleClaim.LastStartRow), which is 0 for a run this daemon holds no claim of, and root
// is read for a tree close alone.
func actingStop(run record.RoleRun, root record.Issue) int64 {
	var lastStart int64
	if run.Claim != nil {
		lastStart = run.Claim.LastStartRow
	}
	var stop int64
	for _, queued := range run.Queued {
		if StopActs(queued.ID, queued.Request, lastStart, &root) {
			stop = max(stop, queued.ID)
		}
	}
	return stop
}

// queuedStartActs is whether a start the outbox has still to run will reach the claim with
// current's task: a queued start naming that phase with an id above stop, the newest queued stop
// that still acts (actingStop), which the caller reads once and gives it rather than having it
// read again here for every judgement. It is one statement of that term because the two rules
// that read it must read it alike — StartFor, for which a start already queued is a promotion
// start that would be a second copy, and SilentPhaseWorker, for which a worker whose task is
// already on its way is no worker nothing will prompt — and because a reader who changes one of
// them is then changing both.
func queuedStartActs(run record.RoleRun, stop int64, current phase.Phase) bool {
	for _, queued := range run.Queued {
		if queued.Request.Op == "start" && queued.Request.Phase == current && queued.ID > stop {
			return true
		}
	}
	return false
}

// SilentPhaseWorker is whether the worker of issue's phase is one nothing will prompt: its
// process is up and between turns, it holds no task, its own session reports nothing of its own
// still to come, and the last step the daemon saw its agent take is older than bound. The daemon
// sends a phase's task as one outbox row and the claim machine retires it at the end of the
// first turn that confirmed it (supervise's settle and retirePending,
// internal/supervise/delivery.go), so a live claim holding no task is a claim the daemon owes
// nothing and will prompt for nothing; what carries a worker through the rest of its phase is a
// timer inside its own process (packages/pi-shared/src/phase-reminder.ts), which dies with that
// process, so a process replaced or ended between turns leaves a phase that nothing moves and a
// record in which nothing says so.
//
// Both supervise.StateReady and supervise.StateIdle are in the rule because the state does not
// separate the fault from health — each is a process waiting to be prompted, whether its last
// turn ended a minute or four hours ago — and what separates them is the role the claim holds: a
// role that works its own issue's current phase is owed a turn, where a role of another phase is
// right to be quiet. The reading this work was filed for found such quiet claims in both
// states, most of them ready and the rest idle, so a rule of one state would have named only
// some of them (dispatch://INFRA-3163, plan.md, "What the reading found").
//
// Two terms are the claim's own session's report of what it is doing (record.RoleClaim's
// ActivityAt, ActivityBackground and ActivityNextReminderAt): a session that reported at all and
// found background work of its own running, and one whose phase reminder is due later than now,
// are left alone, because that worker's own process ends the wait or starts that turn, and
// neither shows as a step. They are the reading the idle release already holds a pod for
// (IdleRelease, IdleHoldBackgroundWork and IdleHoldReminder). A report a process that is gone
// left behind is no term of this rule at all: the read these fields come from drops a report
// whose stamped generation is not the claim's own (record's readRoleClaims, whose rule store's
// scanClaim holds), so what the exclusion reads is what the process running now says and
// nothing else. It has to be read there and cannot be waited out — a Claude Code worker
// reports at its Stop hook alone (packages/claude-envoy/src/legion-phase-hook.ts, sendActivity),
// and a worker nothing will prompt is exactly one that reaches no Stop, so the report its dead
// process left would stand for as long as the claim does. A reminder already due rather than
// ahead leaves no worker alone: that timer is the very one this rule backs up, and it died with
// the process that armed it.
//
// Three terms — no queued stop of the claim that still acts (actingStop), the claim recorded in
// root's own tree, and no queued start for the phase (queuedStartActs) — are the question
// admission's repair of a slot whose worker has no task asks before it starts one (StartFor,
// admit's resumeSlottedWorkers), because a claim any of them names is one something will reach
// and the two repairs may never act on one claim.
//
// A bound of zero or less answers false for every run, which is the one guard against a bound
// no deployment can ask for: config.readMinutes refuses zero and every other non-positive
// value and the key defaults, so the only such bound is one a test passes. A claim whose record
// holds no step is a claim judged from nothing: the record of a claim whose agent has neither
// stepped nor begun a turn holds no moment at all (supervise.StepRecord.LastStep), and the
// daemon guesses none, as the stall backstop guesses none.
//
// A suspension held over the claim for its agent's turn is no term of this rule, because no
// record carries it: supervise.Claim.SuspensionHeld lives in the machine's memory and the store
// never writes it, so neither caller can see one. Such a claim is named silent and started, and
// the outbox refuses that start while the suspension holds (supervise's RuleHolds), which is why
// the notice says what the daemon wrote rather than what the worker was sent (silence.go's
// workerSilentRecord).
//
// It reads values and no store, as StartFor does, so it is tested as a table
// (silent_phase_worker_test.go). Its two callers are the handler of the silence fact the
// daemon's one-minute sweep writes, which re-reads the record inside the fact's own transaction
// (workerSilent), and admission's tick list, which reads each issue's run for the role it answers
// (admit's silentWorkers); the sweep itself judges a live claim and calls no rule. Every claim
// fact it shares with StartFor — that this daemon holds the claim at all, its state, the update
// round it is in, and the task it holds — it reads from run.Claim, the value StartFor reads, so
// the two rules can never judge one claim from two readings of it, and it reads the moment of the
// last step from that same claim (record.RoleClaim.Step). role is the role the run was read for,
// and the rule holds it against PhaseWorkerRole(issue), the role the work of the issue's own
// phase belongs to, which both callers ask of the record themselves.
func SilentPhaseWorker(issue, root record.Issue, gate record.TreeGate, role claim.Role, run record.RoleRun,
	bound time.Duration, now time.Time,
) bool {
	if bound <= 0 || !unpromptedPhaseWorker(issue, root, gate, role, run) {
		return false
	}
	c := run.Claim
	if !c.ActivityAt.IsZero() && c.ActivityBackground {
		return false
	}
	if c.ActivityNextReminderAt.After(now) {
		return false
	}
	lastStep := c.Step.LastStep()
	return !lastStep.IsZero() && lastStep.Add(bound).Before(now)
}

// unpromptedPhaseWorker is the structural half of that rule: a live claim of the role the work
// of issue's own phase belongs to, on an issue at work in a tree that does not linger, which is
// up and between turns, holds no task it still works, is in no update round, and has nothing
// queued that will reach it. It is everything SilentPhaseWorker asks that is not about a moment
// — no step, no report, no reminder — so the two rules that stand on it cannot come to disagree
// about which claim is a phase worker nothing will reach.
//
// Its second caller is the rule over a worker whose own session reports background work that
// has stood past this deployment's bound (silence.go's backgroundStalled, the one rule that
// wants a claim this one holds and the activity terms above refuse). That worker is told of and
// never prompted, so the two differ in what they do and not in which claim they are about.
func unpromptedPhaseWorker(issue, root record.Issue, gate record.TreeGate, role claim.Role, run record.RoleRun) bool {
	if run.Claim == nil {
		return false
	}
	if working := PhaseWorkerRole(issue); working == "" || working != role {
		return false
	}
	if !record.AtWork(issue, gate) || root.Lingers() {
		return false
	}
	c := run.Claim
	if c.State != supervise.StateReady && c.State != supervise.StateIdle {
		return false
	}
	if c.UpdateRound != "" {
		return false
	}
	if c.Pending != nil && c.Pending.StillWorked(issue.Generation, issue.Phase, c.State) {
		return false
	}
	stop := actingStop(run, root)
	return stop == 0 && c.Tree == root.Key && !queuedStartActs(run, stop, issue.Phase)
}

func (e *Engine) enqueue(ctx context.Context, tx pgx.Tx, issue string, payload record.OutboxPayload) error {
	return e.enqueueAt(ctx, tx, issue, payload, e.now())
}

// enqueueAt enqueues payload for issue due at at: a deadline row (a linger's close, a production
// check's bounds), where enqueue's rows are due now.
func (e *Engine) enqueueAt(ctx context.Context, tx pgx.Tx, issue string, payload record.OutboxPayload, at time.Time) error {
	row, err := record.NewOutboxRow(issue, payload, at)
	if err != nil {
		return err
	}
	return e.store.Enqueue(ctx, tx, row)
}

// status queues the Dispatch write for a status the workflow has just given issue. A status that
// reads as out of the workflow is the daemon's own close — the done a merge writes — and it goes
// as a close row, fenced on the run it ends (record.CloseStatusWrite): the ordinary rule drops a
// row whose issue has left the workflow, which is every close, so a close judged by it would be
// lost on its first ISSUE_CHANGED and leave the issue open in Dispatch for good.
func (e *Engine) status(ctx context.Context, tx pgx.Tx, issue record.Issue, status string) error {
	if status == "" || issue.Status == status {
		return nil
	}
	if record.OutOfWorkflow(status) {
		return e.enqueue(ctx, tx, issue.Key, record.CloseStatusWrite(status, issue.Status, "", issue.Generation))
	}
	return e.enqueue(ctx, tx, issue.Key, record.OrdinaryStatusWrite(status, issue.Status, ""))
}

// notice tells the architect that owns the issue; a notice of no kind (noticeFor's, for a move
// that tells it nothing) enqueues nothing.
func (e *Engine) notice(ctx context.Context, tx pgx.Tx, issue string, notice record.Notice) error {
	if notice.Kind == "" {
		return nil
	}
	return e.enqueue(ctx, tx, issue, notice)
}

// noticeWithController tells the architect that owns the issue and, in an outbox row of its own,
// the project's controller: the two publishes retry apart, so a controller topic that keeps failing never sends
// the architect the same notice again. Every hold is told this way, since the architect answers one
// with a retry or an escalation to the controller, and so is the tree architect's own failed
// claim, since every other notice of the tree reaches that architect.
func (e *Engine) noticeWithController(ctx context.Context, tx pgx.Tx, issue string, notice record.Notice) error {
	if err := e.notice(ctx, tx, issue, notice); err != nil {
		return err
	}
	return e.enqueue(ctx, tx, issue, record.ControllerNotice(notice))
}

// logOnCommit writes one Info line once the fact's transaction commits (intake.OnCommit), so a
// fact rolled back writes nothing and one retried after a failed commit writes it once.
func (e *Engine) logOnCommit(ctx context.Context, msg string, args ...any) {
	intake.OnCommit(ctx, func() { e.log.Info(msg, args...) })
}

// warnOnCommit is logOnCommit at Warn, for a committed fact that stalls a round and tells nobody
// else: a log monitor keys on it where an Info line would drown.
func (e *Engine) warnOnCommit(ctx context.Context, msg string, args ...any) {
	intake.OnCommit(ctx, func() { e.log.Warn(msg, args...) })
}

// refused is a committed 409: the fact changed nothing, and the caller is told why.
func refused(code, message string) intake.Result {
	return intake.Result{Refusal: &intake.Refusal{Status: 409, Code: code, Message: message}}
}

// refusedLingering refuses a worker's request on issue while its tree lingers: linger holds the
// member where it stood (record.TreeLingers), so the request changed nothing.
func refusedLingering(issue record.Issue, request string) intake.Result {
	return refused("TREE_LINGERING", fmt.Sprintf("the tree %s is lingering after it left the workflow, so %s of %s changed nothing", issue.Tree, request, issue.Key))
}

// clearHandoff empties the handoff a role reported for its previous phase — its commit, the
// Dispatch handoff record it named, its verdict and summary, and the review round's decision,
// which ends with the round — keeping its claim, the record it last named (LastHandoff) and the
// review rounds, when a transition starts it on a new phase. A role's recorded handoff is then
// always its current phase's: the implementer's round-1 commit cannot advance round 2, a reviewer
// starting round 2 or a tester starting a new round reads as not completed until it completes
// again (record.PhaseRow.Completed), a merger sent back has no summary until its next one, and no
// round inherits an earlier decision. The architect reports no handoff and has no row to clear.
func (e *Engine) clearHandoff(ctx context.Context, tx pgx.Tx, issue string, role claim.Role) error {
	if role == "" || role == claim.RoleArchitect {
		return nil
	}
	row, err := e.phaseRow(ctx, tx, issue, role)
	if err != nil {
		return err
	}
	// Nothing to clear is nothing to write.
	if handoffOf(row) == (phaseHandoff{}) {
		return nil
	}
	return e.store.PutPhase(ctx, tx, phaseHandoff{}.onto(row))
}

// phaseHandoff is the handoff a role reported for one phase, which is the one list of the fields
// clearHandoff empties. Both halves of that clear are derived from this list: the write is the
// empty handoff put back on the row (onto), and the decision to write compares the handoff the
// row carries with that same empty one, so the fields cleared and the fields the decision stands
// on cannot drift apart. It is a struct of its own, and not record.PhaseRow, because the decision
// is a comparison and a phase row cannot be compared: the row holds the map of its backward moves
// (record.PhaseRow.BackwardMoves), and Go refuses == on a struct type that holds a map whatever
// its values are, two nil maps included.
type phaseHandoff struct {
	commit         string
	dispatchRecord string
	version        int
	verdict        string
	summary        string
	decision       *record.ReviewDecision
	completedAt    time.Time
}

// handoffOf is the handoff row carries.
func handoffOf(row record.PhaseRow) phaseHandoff {
	return phaseHandoff{commit: row.HandoffCommit, dispatchRecord: row.HandoffRecord, version: row.HandoffVersion,
		verdict: row.Verdict, summary: row.Summary, decision: row.Decision, completedAt: row.CompletedAt,
	}
}

// onto is row carrying h, with every other field of the row — its claim, the record it last
// named, its rounds and their counts — left as it stands.
func (h phaseHandoff) onto(row record.PhaseRow) record.PhaseRow {
	row.HandoffCommit, row.HandoffRecord, row.HandoffVersion = h.commit, h.dispatchRecord, h.version
	row.Verdict, row.Summary, row.Decision = h.verdict, h.summary, h.decision
	row.CompletedAt = h.completedAt
	return row
}

// suspend stops role's worker on issue, for reason, which the suspended claim logs.
func (e *Engine) suspend(ctx context.Context, tx pgx.Tx, issue record.Issue, role claim.Role, reason string) error {
	return e.enqueue(ctx, tx, issue.Key, record.SuperviseRequest{Op: "suspend", Tree: issue.Tree, Role: role, Generation: issue.Generation, Reason: reason})
}

// testerVerdictLabel labels the tester's verdict a started worker's task carries. The reviewer's
// prompt names it as the tester's result rather than its own (prompts/go/reviewer.md), which a test
// ties to it.
const testerVerdictLabel = "Tester's verdict"

// start starts the phase worker of the issue's current phase, a start stamped with that phase and
// with the assignment it mints for that role (assign).
// A start that must not resume the claim's recorded session says so, freshSession, and only
// the operator's `legion claims retry --fresh-session` does (record.SuperviseRequest's
// FreshSession). A phase with no worker, and accepting, whose architect admission started with
// the tree, start nobody.
func (e *Engine) start(ctx context.Context, tx pgx.Tx, issue record.Issue, role claim.Role, task string, freshSession bool) error {
	if role == "" || role == claim.RoleArchitect {
		return nil
	}
	assignment, err := e.assign(ctx, tx, issue.Key, role)
	if err != nil {
		return err
	}
	return e.started(ctx, tx, issue, role, task, assignment, freshSession)
}

// restart starts the phase worker of a phase entry the issue never left: the architect's or the
// operator's retry of a held phase, which puts the issue back in the phase its worker's claim
// failed in, with the same work to do. It hands the role the assignment that entry already gave
// it, minting none, because the entry is the one the worker is still on: a claim whose task went
// unconfirmed keeps it (the outbox delivers no second copy of a task its claim still works), and
// a completion of it would otherwise be refused HANDOFF_STALE_ASSIGNMENT against a row naming an
// assignment nothing ever delivered, for good. Carrying the row's own value rather than nothing
// keeps the fence live for a worker the retry does start afresh: its task names the entry, so a
// later entry of the same phase still refuses this one's completion. A retry told not to resume
// the claim's recorded session passes that on, freshSession, as a start does.
func (e *Engine) restart(ctx context.Context, tx pgx.Tx, issue record.Issue, role claim.Role, task string, freshSession bool) error {
	if role == "" || role == claim.RoleArchitect {
		return nil
	}
	row, err := e.phaseRow(ctx, tx, issue.Key, role)
	if err != nil {
		return err
	}
	return e.started(ctx, tx, issue, role, task, row.Assignment, freshSession)
}

// ResumeSlottedWorker starts again the worker of the phase issue stands in, with restart's
// semantics: the assignment the role's phases row already names, so a completion still in flight
// is never refused for an assignment nothing ever delivered, and the phase's resume task, which
// says to carry the work on rather than beginning it. It is exported for admission's repair of
// a slot whose worker has no task (admit's resumeSlottedWorkers) and read by this package's own
// repair of a phase worker nothing will prompt (silence.go's repairSilence), which are its two
// callers: every other way back into a phase is a transition with a task of its own. The task
// names no head: the merger reads the head it merges on GitHub, so no task of the daemon's names
// one.
func (e *Engine) ResumeSlottedWorker(ctx context.Context, tx pgx.Tx, issue record.Issue, role claim.Role) error {
	return e.restart(ctx, tx, issue, role, ResumePhaseTask(issue), false)
}

// started enqueues the start both make, for a role start and restart have already held to one
// that works a phase. It goes through StartOrOwe, so the worker comes back on the one row that
// carries its new task, and that row waits for the issue's admission slot: a transition's start
// puts a worker to work, which is exactly what the cap counts.
func (e *Engine) started(ctx context.Context, tx pgx.Tx, issue record.Issue, role claim.Role, task, assignment string, freshSession bool) error {
	return StartOrOwe(ctx, tx, e.store, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: role, Task: task,
		Generation: issue.Generation, Phase: issue.Phase, Assignment: assignment, FreshSession: freshSession}, e.now())
}

// StartOrOwe writes the start of a phase worker as one outbox row in the caller's own
// transaction: the caller's own request, the task on it. The outbox's start case does the whole
// start on that row — it stops the role elsewhere, re-points a carried claim at this tree,
// provisions the workspace and creates the claim when there is none, records the start against
// the claim, spawns, resumes or retries it by state, and delivers the task — so a worker that
// comes back comes back with its work, rather than spending a turn asking what it is for. It is
// exported because admission starts phase workers too (admit's startMidPhaseMembers), so there
// is one implementation of the rule and one place to test it.
//
// Every start it writes is marked for the issue's admission slot, which the outbox holds
// unclaimable while the issue holds none (store.go's outboxInOrder). The cap counts the issues
// the daemon has started, each holding its row from its start until its work ends
// (record.KeepsSlot), so a start claimable at once would put a worker to work on an issue
// holding no slot — where the cap is full that is what it would do — and a worker put to work
// on an issue the cap has not admitted is what a start of a phase worker may never be. A
// transition's start (Engine.started), admission's start of a mid-phase member and the boot's
// resume by rule all come through here, so all of them wait. The mark is this function's and
// not the caller's: it is a fact about every start of a phase worker, not about one request.
// The row carries its task while it waits, so the delivery is made as soon as the slot comes
// and the worker is up.
//
// The row is written through Store.ReplaceOwedStart first and enqueued only when that replaced
// nothing. ReplaceOwedStart matches by issue and not by role because the row's id and created_at
// are the issue's place in the waiting line: an issue that already owes a start and has not had
// its slot keeps that place and carries the later transition's task instead, so two transitions
// before the slot start the worker once. The superseded role never starts, which is right —
// the issue has left that phase, so that role never had work to do, while the role the later
// transition names has it. Only when there is no such row — the first start, or one the lane has
// already leased because the slot came — is a row of its own enqueued, which is what the daemon
// has always done for back-to-back transitions.
//
// No other start of the daemon's comes through here, and none may: the tree architect's start
// (admit's promotion, awaiting_reconcile.go), which waits for no slot of its own, since the row
// the cap counts is its root's and the root holds it from its promotion until its work ends
// (record.KeepsSlot), the production check's look (check.go), the pod wake (wake.go), the resume
// of a cancelled idle release (the daemon's outbox) and the roles admission brings back beside a
// member's phase worker (admit's startMidPhaseMembers) are enqueued directly and stay claimable
// at once. None of them puts a worker to work on a phase, so none of them is the cap's to hold,
// and a slot they waited for might never be offered.
func StartOrOwe(ctx context.Context, tx pgx.Tx, store record.Store, issue string, payload record.SuperviseRequest, at time.Time) error {
	payload.NeedsSlot = true
	row, err := record.NewOutboxRow(issue, payload, at)
	if err != nil {
		return err
	}
	replaced, err := store.ReplaceOwedStart(ctx, tx, row)
	if err != nil || replaced {
		return err
	}
	return store.Enqueue(ctx, tx, row)
}

// assign mints the assignment this start hands role on issue, writes it onto the role's phases row
// and answers it for the start's payload. Both writes are the transition's own transaction, so the
// row names the new phase's assignment before the outbox can run that start, which is what makes
// the comparison safe: while a role's turn outlives its phase its claim still serves the earlier
// assignment (the machine refuses a second delivery while one is pending), and the row already
// names this one.
//
// Only a start that hands a role work it was not already on mints one. A resume (workflow's
// resume by rule, admission's promotion of a mid-phase child), a production check's look and a
// held phase's retry (restart) mint none, so none of them moves the row and a worker's in-flight
// completion is never refused for an assignment it was never given.
//
// The token is 26 random base32 characters (crypto/rand.Text), which is more than it needs to be:
// it is only ever compared with a value the daemon itself wrote, never read from a request — a
// completion carries no identity at all (api.HandoffCompleteRequest) — so it has to be unique
// among the assignments of one role, not unguessable. It is minted rather than derived because
// nothing at hand is unique per phase entry: the issue's generation is one run's whole life, the
// round counts only returns to implementing, and the start's own outbox row id is not known until
// the row is written.
func (e *Engine) assign(ctx context.Context, tx pgx.Tx, issue string, role claim.Role) (string, error) {
	row, err := e.phaseRow(ctx, tx, issue, role)
	if err != nil {
		return "", err
	}
	row.Assignment = rand.Text()
	if err := e.store.PutPhase(ctx, tx, row); err != nil {
		return "", err
	}
	return row.Assignment, nil
}

// task is what a started worker is told. It names the phase the worker starts, which the issue
// record already holds: one role runs several phases (the implementer runs implementing and
// retro), and a resumed session cannot otherwise tell a new phase from its last.
// The handoff a phase ended with is a Dispatch handoff record on the issue, which the line names by
// its phase word and version with the command that reads that version, the record the daemon
// accepted, whatever was written to the phase since; a commit is named only when the completion
// reported one (the implementer's, retro's, the merger's).
// A handoff's verdict is always the tester's, since the daemon accepts one from no other role on
// a phase completion (api/handoff.go, VERDICT_ROLE_FORBIDDEN; the implementer's check report is no
// phase completion and starts nobody), so the line names the tester: the reviewer it starts on a
// pass reads it as the tester's report, never as a field of its own completion.
func task(issue record.Issue, handoff record.PhaseRow, pr *record.PullRequest, reason string) string {
	text := continueLine(issue)
	if handoff.HandoffRecord != "" {
		word := handoffWord(handoff.Role)
		text += fmt.Sprintf(" Handoff: %s version %d (dispatch handoff-read --issue %s --phase %s --version %d).", word, handoff.HandoffVersion, issue.Key, word, handoff.HandoffVersion)
	}
	if handoff.HandoffCommit != "" {
		text += " Handoff commit: " + handoff.HandoffCommit + "."
	}
	if handoff.Verdict != "" {
		text += " " + testerVerdictLabel + ": " + handoff.Verdict + "."
	}
	if pr != nil && pr.Number != 0 {
		text += fmt.Sprintf(" Pull request: #%d.", pr.Number)
	}
	if reason != "" {
		text += " Reason: " + reason + "."
	}
	return text
}

func continueLine(issue record.Issue) string {
	return fmt.Sprintf("Continue %s. Issue: %s. Phase: %s.", issue.Title, issue.Key, issue.Phase)
}

// handoffWord is the word of the Dispatch handoff record role writes (phase.Handoff's writer), ""
// for a role that writes none.
func handoffWord(role claim.Role) string {
	for _, p := range phase.All {
		if word, writer, ok := phase.Handoff(p); ok && writer == role {
			return word
		}
	}
	return ""
}

// ResumePhaseTask is what a worker started for a phase already under way is told. No handoff
// begins that phase — the run that did is over — so the task names the phase and says to carry
// the work on, which is what a resumed session needs to tell this phase from its last.
func ResumePhaseTask(issue record.Issue) string {
	return continueLine(issue) + " Resume the existing phase work."
}

// HeldNotice is the notice of issue's hold as its architect answers one, with a retry or an
// escalation: the phase the hold came from, the role that works it, and why it is held. Admission
// tells a carried root's own architect with it, since the hold's first notice went to the
// architect of the tree the root was handed over from.
func HeldNotice(issue record.Issue) record.Notice {
	return record.Notice{Kind: "held", Role: RoleFor(issue.Hold.From), Phase: issue.Hold.From, Reason: string(issue.Hold.Reason)}
}

// ChildReenteredNotice is the notice a re-entered child's tree architect is told, whichever path
// re-enters it: reenterChild for a live tree's reopened child, or admission's
// reenterStrandedChildren for one a reopen declined to re-enter for want of the label until its
// root's own promotion made the tree live.
func ChildReenteredNotice(child, root string) record.Notice {
	return record.Notice{Kind: "child-status", Role: claim.RoleArchitect, Reason: fmt.Sprintf("%s is todo; it runs again under %s", child, root)}
}

// ChildStartedNotice is the notice the tree's architect is told whenever a child of it starts
// its first phase (Engine.StartFirstPhase), its reason saying why it starts now. waited is every
// issue the child's mirror row still names, in Dispatch's own order, all of them done by now:
// the last of them closed, which is what freed the child.
//
// Empty, the child's own links say nothing about why it starts, and the reason says what is true
// of every child the line starts instead: it reached the front of the waiting line and took a
// slot. That covers both an issue that never named a blocker — which is most of them now that a
// child waits for a slot like any other issue — and one whose blocked-by links a person or the
// architect took off while it waited, which the record cannot tell apart from the first. Naming
// links that were taken off would be a guess, and a reason the architect reads and cannot check
// is worse than a plain one.
//
// A start nobody hears of is the silent state the hold's report exists to remove, so no child
// starts without one. It is the same child-status kind a child's leave and a re-entry are told
// with — the daemon's notice routing sends one on a non-root child to the architect that owns it
// — and not phase-finished: no phase of the child ended, it is beginning its first.
func ChildStartedNotice(child, root string, waited []string) record.Notice {
	reason := fmt.Sprintf("%s starts planning under %s: every issue it waited on (%s) is done", child, root, strings.Join(waited, ", "))
	if len(waited) == 0 {
		reason = fmt.Sprintf("%s starts planning under %s: it reached the front of the waiting line and took an admission slot", child, root)
	}
	return record.Notice{Kind: "child-status", Role: claim.RoleArchitect, Reason: reason}
}

func (e *Engine) lingerAt() time.Time { return e.now().Add(e.cfg.Linger) }
