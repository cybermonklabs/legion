package workflow

import (
	"context"
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

// RoleFor is phase.RoleFor, which is where the role that works a phase is decided
// (dispatch://INFRA-2800 moved it there, because the state projection reads it and
// internal/workflow's own tests import internal/projection). This forwarder is kept under the
// name this package carried it under for one reader that no build of this module reaches:
// packages/pi-legion/scripts/skill-scenarios/daemon-pane.go, which `go run -overlay` builds as a
// main inside the packages/daemon of each checkout a comparison run names, this head's and the
// one it is compared with. That program must therefore compile against both, so it calls the
// name both carry, and this forwards to the one definition rather than restating the table —
// a second table is what would drift.
//
// It goes once no checkout a comparison run can name predates dispatch://INFRA-2800: then
// daemon-pane.go calls phase.RoleFor against every checkout it is built inside, and this module
// holds one exported name for the rule again rather than two a new caller could pick between.
// Until then this name has callers of its own inside this module, seven of them: readyWorkerTask
// (catch_up.go), leftItsPhase and noticeFor's two reads (engine.go), tellGateMoved (gate.go),
// tellChecks (review.go) and one test (checks_red_test.go). Each reads the same table as a
// caller of phase.RoleFor, since that is the one definition and this forwards to it.
func RoleFor(p phase.Phase) claim.Role {
	return phase.RoleFor(p)
}

func (e *Engine) phaseFinished(ctx context.Context, tx pgx.Tx, issue record.Issue) (bool, error) {
	return PhaseFinished(ctx, e.store, tx, issue)
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
// act is a start. A stop that still acts (actingStop) undoes any older start, which may run first,
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
// Its reading of run.Queued is queuedStartActs, which SilentPhaseWorker shares.
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

// queuedStartActs is whether a queued start for current with an id above stop, the newest stop
// that still acts (actingStop), will reach the claim. StartFor and SilentPhaseWorker both read
// it, so they answer that term alike.
func queuedStartActs(run record.RoleRun, stop int64, current phase.Phase) bool {
	for _, queued := range run.Queued {
		if queued.Request.Op == "start" && queued.Request.Phase == current && queued.ID > stop {
			return true
		}
	}
	return false
}

// PastBound is whether at is more than bound before now. A zero at is no recorded moment, and a
// bound of zero or less is one no deployment can set (config's readMinutes and readHours refuse
// it), so both answer false.
func PastBound(at time.Time, bound time.Duration, now time.Time) bool {
	if at.IsZero() || bound <= 0 {
		return false
	}
	return at.Add(bound).Before(now)
}

// ReportExplainsQuiet is whether a claim's own session's report explains why it takes no turn:
// it found background work of its own running, or its phase reminder is due later than now. The
// launch that replaces a process clears its report (store's putClaim), and a report is written
// only onto the launch whose secret authenticated it (store's PutClaimActivity), so the report
// read is the running process's. A reminder already due explains nothing: it is the timer a
// silence repair backs up.
func ReportExplainsQuiet(background bool, nextReminder time.Time, now time.Time) bool {
	return background || nextReminder.After(now)
}

// SilentPhaseWorker is whether role's claim in run is a phase worker nothing will prompt: a claim
// unpromptedPhaseWorker holds, whose session's report does not explain its quiet
// (ReportExplainsQuiet), and whose agent's last recorded step is past bound (PastBound). A live
// claim holding no task is owed nothing by the daemon, and the timer that carries a worker
// through its phase runs in its own process (packages/pi-shared/src/phase-reminder.ts), so it
// dies with that process.
//
// Terms the code does not show:
//   - a suspension held for the agent's turn is not a term: no record carries it
//     (supervise.Claim.SuspensionHeld), and the outbox refuses the start while it holds.
//   - a finished phase or an accepted completion is not a term: this reads no store, so its
//     callers ask PhaseWorkerOwedTurn as well.
func SilentPhaseWorker(issue, root record.Issue, gate record.TreeGate, role claim.Role, run record.RoleRun,
	bound time.Duration, now time.Time,
) bool {
	if !unpromptedPhaseWorker(issue, root, gate, role, run) {
		return false
	}
	c := run.Claim
	if ReportExplainsQuiet(c.ActivityBackground, c.ActivityNextReminderAt, now) {
		return false
	}
	return PastBound(c.Step.LastStep(), bound, now)
}

// unpromptedPhaseWorker is the half of SilentPhaseWorker that is not about a moment: a live claim
// of the role that works issue's phase (OpenPhaseRole), on an issue at work in a tree that does
// not linger, up and between turns, in no update round, holding no task it still works, with no
// acting stop and no queued start and recorded in root's tree. Ready and idle are both a process
// waiting to be prompted; the role, not the state, tells a fault from health. The last three
// terms are StartFor's, read the other way, so no claim a start of the daemon's is already on
// its way to is named. Admission's repair of a role behind its entry (admit's owedEntryRoles)
// can name the same claim when it is behind the entry its phases row names, and both then send
// that entry's work (repairSilence, SendOwedEntry); whichever writes first leaves a queued start
// the other's rule reads and passes over. BackgroundStalledWorker stands on it too, so the two
// rules agree on which claim they are about.
func unpromptedPhaseWorker(issue, root record.Issue, gate record.TreeGate, role claim.Role, run record.RoleRun) bool {
	if run.Claim == nil {
		return false
	}
	if working := OpenPhaseRole(issue.Phase); working == "" || working != role {
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

// PhaseWorkerOwedTurn is whether role's worker of issue's phase is still owed a turn, the half of
// a silence only the store can answer: false for a phase its terminating fact has met
// (PhaseFinished) and for a role whose completion of this phase entry the workflow accepted (the
// phase row's CompletedAt, which the next phase's start clears in clearHandoff). It answers the
// role's phase row too, an empty row naming the issue and role where the record holds none, so
// the silence repair reads it once.
func PhaseWorkerOwedTurn(ctx context.Context, store record.Store, tx pgx.Tx, issue record.Issue, role claim.Role) (record.PhaseRow, bool, error) {
	finished, err := PhaseFinished(ctx, store, tx, issue)
	if err != nil || finished {
		return record.PhaseRow{}, false, err
	}
	rows, err := store.Phases(ctx, tx, issue.Key)
	if err != nil {
		return record.PhaseRow{}, false, err
	}
	row := record.PhaseRow{Issue: issue.Key, Role: role}
	for _, r := range rows {
		if r.Role == role {
			row = r
			break
		}
	}
	return row, row.CompletedAt.IsZero(), nil
}

// OwedEntry is whether a role is owed the work the entry its phases row names stands for: entry
// is that number (record.PhaseRow.Entry), run the role's queued rows and claim for the issue's
// generation, and root the issue's tree root as recorded, which StopActs reads for a tree close.
// Admission asks it of every role of an issue at work and sends the ones it answers true for the
// work of that entry (admit's owedEntryRoles and resumeSlottedWorkers, SendOwedEntry).
//
// It answers true when the entry the claim was given — record.RoleClaim.GivenEntry, and 0 for a
// role the daemon holds no claim row of at all, which was given nothing — is below entry, and no
// queued start of the role carries an entry at or above it. That second term is what keeps one
// entry to one start row: the given number moves when the outbox queues the delivery, not when
// the transition writes the row, and an admission pass runs on every fact of the project, so
// without it every pass between the write and the delivery would write another start for the
// same entry. A stop that still acts (actingStop) undoes the start it outlives, which may run
// first, so past one only a queued start newer than the stop counts, which is the body StartFor
// reads the same rows with.
//
// Two tests the caller holds are deliberately not here, because each is about the issue rather
// than about the role's entry: that the role is the one the issue's current phase names
// (phase.RoleFor), and that a worker of the issue has work where it stands (record.AtWork).
// admit's owedEntryRoles answers both from the records it has already read for the whole pass,
// and asking them here would mean reading the issue again per role.
//
// Where StartFor asks what a re-admitted tree's promotion must start, and so weighs whether the
// claim may already have a process, this reads nothing of the claim's state: a registered, ready
// or idle claim holding none of entry's work is the very claim this exists to close the
// difference for. The states no start may reach — an operator's suspension, a retirement — are
// the caller's own skips.
func OwedEntry(run record.RoleRun, root record.Issue, entry int64) bool {
	held := run.Claim
	var given int64
	if held != nil {
		given = held.GivenEntry
	}
	if given >= entry {
		return false
	}
	stop := actingStop(run, root)
	for _, queued := range run.Queued {
		if queued.Request.Op == "start" && queued.Request.Entry >= entry && queued.ID > stop {
			return false
		}
	}
	return true
}

// actingStop is the newest queued stop of run that will still act when the outbox runs it
// (StopActs), by row id, and 0 when no queued stop will. root is the issue's tree root as
// recorded, which StopActs reads for a tree close, and the last start the outbox ran against the
// claim (record.RoleClaim.LastStartRow, 0 for a role the daemon holds no claim row of) is what
// every other stop is weighed against.
//
// Such a stop undoes every start older than it, and it may run before them, so every rule that
// reads it counts only a queued start newer than it: StartFor, asking what a re-admitted tree's
// promotion must start, OwedEntry, asking whether a role is owed its entry's work, and
// unpromptedPhaseWorker, asking whether nothing will prompt a live claim. One body is what keeps
// them from reading the same rows differently.
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
// with the entry it raises for that role (enter), whose task is word for word the task the
// worker is sent. A start that must not resume the claim's recorded session says so,
// freshSession, and only the operator's `legion claims retry --fresh-session` does
// (record.SuperviseRequest's FreshSession). A phase with no worker, and accepting, whose
// architect admission started with the tree, start nobody.
func (e *Engine) start(ctx context.Context, tx pgx.Tx, issue record.Issue, role claim.Role, task string, freshSession bool) error {
	if role == "" || role == claim.RoleArchitect {
		return nil
	}
	entry, err := e.enter(ctx, tx, issue, role, task)
	if err != nil {
		return err
	}
	return e.started(ctx, tx, issue, role, task, entry, startMarks{freshSession: freshSession})
}

// restart starts the phase worker of a phase entry the issue never left: the architect's or the
// operator's retry of a held phase, which puts the issue back in the phase its worker's claim
// failed in, with a task of its own that names the retry's reason. It hands the role the entry
// that row already names, raising none, because the entry is the one the worker is still on: a
// claim whose task went unconfirmed keeps it (the outbox delivers no second copy of a task its
// claim still works), and a completion of it would otherwise be refused for an entry nothing
// ever delivered, for good.
// Carrying the row's own number rather than nothing keeps the fence live for a worker the retry
// does start afresh: its start names that entry, so a later entry of the same phase still refuses
// this one's completion. A retry told not to resume the claim's recorded session passes that on,
// freshSession, as a start does.
//
// The start is marked a resend (record.SuperviseRequest.ResendEntry), because it hands the role
// the work of the entry its row already names: a claim that confirmed that entry's last task and
// whose turn then ended holds that entry as its given one and no pending delivery, so the entry
// test alone would decline the retry's task and relaunch the worker with no prompt
// (supervise.Machine.queue).
func (e *Engine) restart(ctx context.Context, tx pgx.Tx, issue record.Issue, role claim.Role, task string, freshSession bool) error {
	if role == "" || role == claim.RoleArchitect {
		return nil
	}
	row, err := e.phaseRow(ctx, tx, issue.Key, role)
	if err != nil {
		return err
	}
	return e.started(ctx, tx, issue, role, task, row.Entry, startMarks{resend: true, freshSession: freshSession})
}

// SendOwedEntry sends role the work the entry of its phases row names, at that entry, for a claim
// that is behind it. row is that row, read by the caller: the admission pass weighs the claim
// against the entry it names and hands it here, so the row the rule was weighed against is the
// row whose work is sent and the rows are read once (admit's owedEntryRoles).
//
// The row's EntryTask is that work: the text the transition composed and wrote in the same
// transaction as the number it raised (enter), so it is sent as it stands.
//
// A row carrying no task is sent the phase's resume task instead (ResumePhaseTask), which says to
// carry the work on rather than beginning it. Two rows are in that state: one the upgrade to the
// entry left with no task, since the release before it recorded none, and one a re-admission
// reset (record.Store.ClearGeneration).
//
// It is exported for admission, which repairs a slot whose worker has no task with it
// (resumeSlottedWorkers) and sweeps the claims left behind their role's entry: every other way
// back into a phase is a transition with a task of its own. Its start carries no mark, which is
// what a sweep's and a ready's start carry; a door that marks its start calls sendOwedEntry.
func (e *Engine) SendOwedEntry(ctx context.Context, tx pgx.Tx, issue record.Issue, role claim.Role, row record.PhaseRow) error {
	return e.sendOwedEntry(ctx, tx, issue, role, row, startMarks{})
}

// sendOwedEntry is the body SendOwedEntry, the boot's resume by rule (resume.go's booted) and
// the silence repair (silence.go's repairSilence) share, with the marks of the door the start
// comes through (startMarks). A mark names that door and not the task's text, so what a claim
// behind its entry is sent is composed in one place however the start is made, and the boot's
// resume of a claim the daemon suspended by rule goes on telling the outbox whose launch budget
// is at risk.
func (e *Engine) sendOwedEntry(ctx context.Context, tx pgx.Tx, issue record.Issue, role claim.Role, row record.PhaseRow,
	marks startMarks) error {
	task := row.EntryTask
	if task == "" {
		task = ResumePhaseTask(issue)
	}
	return e.started(ctx, tx, issue, role, task, row.Entry, marks)
}

// StartPromotedWorker starts the phase worker of a mid-phase member a re-admitted tree's
// promotion brings back (admit's startMidPhaseMembers), on the resume task of the phase it stands
// in (ResumePhaseTask).
//
// It raises the entry, where the promotion's other role starts raise none: the promotion is
// handing that role its work again, and the re-admission cleared the task its entry named with
// the rest of the generation (record.Store.ClearTreeGeneration), so the entry the record now
// holds names work nothing would send. The start is marked a resume task, so the outbox still
// hands no second prompt to a claim that holds its phase's task
// (record.SuperviseRequest.ResumeTask).
func (e *Engine) StartPromotedWorker(ctx context.Context, tx pgx.Tx, issue record.Issue, role claim.Role) error {
	task := ResumePhaseTask(issue)
	entry, err := e.enter(ctx, tx, issue, role, task)
	if err != nil {
		return err
	}
	return e.started(ctx, tx, issue, role, task, entry, startMarks{resumeTask: true})
}

// startMarks are the marks a start's request carries beside its entry, which tell the outbox and
// the claim's machine how to read that entry and that task, and how to start the claim
// (record.SuperviseRequest). The zero value marks none of them, which is a start of the work of
// an entry the claim is behind, resuming the session the claim records.
type startMarks struct {
	// resend says the start hands the role the work of the entry its phases row already names
	// rather than a new entry, so the claim's given entry does not decline the task
	// (record.SuperviseRequest.ResendEntry). The held phase's retry sets it (restart), because
	// the claim it reaches may hold that very entry as its given one; sendOwedEntry sends the
	// entry its row names too and sets it not, because the claim it reaches is behind that entry
	// and the entry test passes the task on its own.
	resend bool
	// resumeTask says the task is the phase's resume task (ResumePhaseTask), so a claim that
	// already holds a task of this generation and phase is given it once it is ready and the
	// start delivers none (record.SuperviseRequest.ResumeTask). The promotion's start of a
	// mid-phase worker sets it (StartPromotedWorker), and so do the boot's resume and the
	// silence repair of a claim level with the entry its phases row names (booted,
	// repairSilence).
	resumeTask bool
	// ruleResume says the start is the boot's resume by rule, so the outbox finishes the row
	// rather than retrying the claim it resumed when that claim spends its launch budget
	// (record.SuperviseRequest.RuleResume). The boot sets it (booted), and that start alone: the
	// mark names the door the start comes through — the boot's resume of a claim the daemon
	// suspended by rule, where the outbox finishes the row rather than retrying a claim that is
	// spending its launch budget — and not the task's text. So the boot carries it on the work
	// of the entry its role's phases row names as well as on the phase's resume task
	// (sendOwedEntry), and changing what the task says does not change whose budget is at risk.
	ruleResume bool
	// freshSession says the start must not resume the session the claim records but begin one of
	// its own (record.SuperviseRequest.FreshSession). Only the operator's `legion claims retry
	// --fresh-session` asks for it, which reaches a start and a retry of a held phase alike
	// (start, restart).
	freshSession bool
}

// started enqueues the start each of its six callers makes, each of which has held role to one
// that works a phase and composed the task itself, at the entry that task is of — 0 for a
// resume that hands the role no new work, which is the boot's resume and the silence repair of
// a claim level with the entry its phases row names (booted, repairSilence) — and each of which
// names its own marks. It is the one place a start of a phase worker is built, so a reader
// asking what such a row carries reads this request and no other, and a field added here
// reaches every one of those starts. It goes through StartOrOwe, so the worker comes back on
// the one row that carries its new task, and that row waits for the issue's admission slot: a
// transition's start puts a worker to work, which is exactly what the cap counts.
//
// One of the six is called from outside the engine's own transitions: sendOwedEntry, which
// admission calls through SendOwedEntry for the claims it sweeps and for the slot it repairs
// (admit's owedEntryRoles and resumeSlottedWorkers), which readyWorkerTask calls for a worker
// whose agent has just reported ready while its claim stands below the entry its phases row
// names (catch_up.go), which the boot's resume by rule calls for a suspended claim standing
// below it (booted), and which the silence repair calls for a silent claim standing below it
// (repairSilence). A ready worker level with that row is given the phase's resume task
// instead, and that one start is enqueued directly rather than built here, for the reason
// StartOrOwe's own comment gives.
func (e *Engine) started(ctx context.Context, tx pgx.Tx, issue record.Issue, role claim.Role, task string, entry int64, marks startMarks) error {
	return StartOrOwe(ctx, tx, e.store, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: role, Task: task,
		Generation: issue.Generation, Phase: issue.Phase, Entry: entry, ResendEntry: marks.resend, ResumeTask: marks.resumeTask,
		RuleResume: marks.ruleResume, FreshSession: marks.freshSession}, e.now())
}

// StartOrOwe writes the start of a phase worker as one outbox row in the caller's own
// transaction: the caller's own request, the task on it. The outbox's start case does the whole
// start on that row — it stops the role elsewhere, re-points a carried claim at this tree,
// provisions the workspace and creates the claim when there is none, records the start against
// the claim, spawns, resumes or retries it by state, and delivers the task — so a worker that
// comes back comes back with its work, rather than spending a turn asking what it is for.
//
// It is exported for the tests of other packages that queue a start the way the engine does
// (internal/admit's slot cases, internal/daemon's outbox cases), so that what those cases write
// is the row production writes. The one production caller is Engine.started, which is where the
// entry a start hands and each of its marks are set, so production code adds no caller here: a
// request composed at this call would carry no entry and no mark. Admission's start of a
// mid-phase worker goes through started too, by way of Engine.StartPromotedWorker (admit's
// startMidPhaseMembers).
//
// Every start it writes is marked for the issue's admission slot, which the outbox holds
// unclaimable while the issue holds none (store.go's outboxInOrder). The cap counts the issues
// the daemon has started, each holding its row from its start until its work ends, except
// while it waits on another issue (record.KeepsSlot, blockers.waitsOf), so a start claimable at
// once would put a worker to work on an issue
// holding no slot — where the cap is full that is what it would do — and a worker put to work
// on an issue the cap has not admitted is what a start of a phase worker may never be. A
// transition's start, admission's start of a mid-phase member, the boot's resume by rule and the
// silence repair all come through here, and all four come through Engine.started, so all of them
// wait. The mark is this function's and not the caller's: it is a fact about every start of a
// phase worker, not about one request.
// The row carries its task while it waits, so the delivery is made as soon as the slot comes
// and the worker is up.
//
// The row is written through Store.EnqueueStart, the one statement that takes out the start the
// issue already owes and writes this one in its place. It matches that row by issue and not by
// role because the row's creation time is the issue's place in the waiting line: an issue that
// already owes a start and has not had its slot keeps that place — what is written is a row of
// its own carrying that creation time — and carries the later transition's task instead, so two
// transitions before the slot start the worker once. The superseded role never starts, which is
// right — the issue has left that phase, so that role never had work to do, while the role the
// later transition names has it. Where there is no such row — the first start, or one the lane
// has already leased because the slot came — the same statement writes the caller's own row,
// which is what the daemon has always done for back-to-back transitions.
//
// No other start of the daemon's comes through here, and none may: the tree architect's start
// (admit's promotion, awaiting_reconcile.go), which waits for no slot of its own, since the row
// the cap counts is its root's and the root holds it from its promotion until its work ends,
// except while it waits on another issue (record.KeepsSlot, blockers.waitsOf), the production
// check's look (check.go), the pod wake (wake.go), the resume of a cancelled idle release (the
// daemon's outbox), the roles admission brings back beside a member's phase worker (admit's
// startMidPhaseMembers) and the phase's resume task a ready worker level with the entry its
// phases row names is given (catch_up.go's readyWorkerTask) are enqueued directly and stay
// claimable at once. None of the first five puts a worker to work on a phase, so none of them is
// the cap's to hold, and a slot they waited for might never be offered. The last is sent to a
// worker already at work on the phase it is in — its agent has just reported ready, and
// admission gave its tree its slot long before — so the issue holds the row the cap counts
// already, or gave it back to wait on another issue, a wait that stops none of its work, and
// this one has nothing to wait for either way. The work of an entry that same ready worker
// stands below is sent through SendOwedEntry, and that start comes through here with the rest.
func StartOrOwe(ctx context.Context, tx pgx.Tx, store record.Store, issue string, payload record.SuperviseRequest, at time.Time) error {
	payload.NeedsSlot = true
	row, err := record.NewOutboxRow(issue, payload, at)
	if err != nil {
		return err
	}
	return store.EnqueueStart(ctx, tx, row)
}

// enter raises the phase entry this start hands role on issue, writes task as the work of that
// entry and the moment it raised it, and answers the number for the start's payload. All of it is
// the transition's own transaction, so the row names the new entry before the outbox can run that
// start, which is what makes the comparison safe: while a role's turn outlives its phase its claim
// still serves the earlier entry (the machine refuses a second delivery while one is pending), and
// the row already names this one.
//
// Two starts raise one, each handing its role work it was not already on: a transition's start of
// the phase the issue has moved into (start), and the promotion's start of a mid-phase worker a
// re-admitted tree brings back (StartPromotedWorker). Every other start carries the entry its
// role's phases row already names — the send of an owed entry (SendOwedEntry), the held phase's
// retry (restart) — or is a start of no entry (record.SuperviseRequest.Entry), so none of them
// moves the row and a worker's in-flight completion is never refused for an entry it was never
// given.
//
// The number counts the entries this role has been handed on this issue, which is why it is raised
// here: this is the one place a role is handed work it was not on. It needs to be unique among
// those entries and no more, since it is only ever compared with a value the daemon itself wrote,
// never read from a request — a completion carries no identity at all
// (api.HandoffCompleteRequest). Nothing else at hand counts them: the issue's generation is one
// run's whole life, and the round counts only returns to implementing.
//
// The task it writes is the text the worker is sent, word for word, so the record holds exactly
// what the worker is told and a claim left behind the entry can be sent that work again from the
// record alone (SendOwedEntry).
func (e *Engine) enter(ctx context.Context, tx pgx.Tx, issue record.Issue, role claim.Role, task string) (int64, error) {
	row, err := e.phaseRow(ctx, tx, issue.Key, role)
	if err != nil {
		return 0, err
	}
	row.Entry++
	row.EntryTask, row.EntryRaisedAt = task, e.now()
	if err := e.store.PutPhase(ctx, tx, row); err != nil {
		return 0, err
	}
	return row.Entry, nil
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
	return record.Notice{Kind: "held", Role: phase.RoleFor(issue.Hold.From), Phase: issue.Hold.From, Reason: string(issue.Hold.Reason)}
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
