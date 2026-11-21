package workflow

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/dispatchbody"

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

func (e *Engine) enqueue(ctx context.Context, tx pgx.Tx, issue string, payload record.OutboxPayload) error {
	return e.enqueueAt(ctx, tx, issue, payload, e.now())
}

// enqueueAt enqueues payload for issue due at at: a deadline row (a linger's close, a production
// check's bounds), where enqueue's rows are due now.
func (e *Engine) enqueueAt(ctx context.Context, tx pgx.Tx, issue string, payload record.OutboxPayload, at time.Time) error {
	row, err := queuedRow(ctx, tx, e.store, issue, payload, at)
	if err != nil {
		return err
	}
	return e.store.Enqueue(ctx, tx, row)
}

// queuedRow is the one place this package builds an outbox row (record.NewOutboxRow), so it is the
// one place every task the daemon queues for a worker passes, whichever path composed it: the rows
// enqueue and enqueueAt write (a wake's start among them, wakeRow, and a ready worker's catch-up,
// readyWorkerTask), the start of a phase worker (startOrOwe, which every start Engine.started
// builds comes through) and a root architect's start that claims the decisions it names
// (EnqueueNaming). It is where a task takes what every task of its worker must name: the
// daemon's two sentences of the reviewer's review round (namingTheReviewRound), always the last
// two of each task of the reviewer, in each phase, so a kind of task added later cannot be queued
// without them.
//
// Only a start of the reviewer that carries a task and does not hold the sentences already is
// named, so no other row reads the record for them. Such a start reads the phase its issue stands
// in and the reviewer's row as tx holds them, which is the row's commit and entry at the moment
// the task is queued, and it then carries the typed fact that its task ends with the daemon's
// sentences (record.SuperviseRequest.RoundNamed), which the claim's send reads, so it adds none.
// A start whose task is the stored task of its entry - the start of the transition that raised
// the entry, whose task enter stored through namingTheReviewRound in the same transaction, the
// promoted worker's and the owed entry's - comes with that fact already, and is queued as it is
// before any read: it holds the sentences already. That is a fact the start carries and never a
// reading of the task's text, which can quote either sentence, a retry's reason among it.
func queuedRow(ctx context.Context, tx pgx.Tx, store record.Store, issue string, payload record.OutboxPayload, at time.Time) (record.OutboxRow, error) {
	start, ok := payload.(record.SuperviseRequest)
	if !ok || start.Op != "start" || start.Task == "" || start.Role != claim.RoleReviewer || start.RoundNamed {
		return record.NewOutboxRow(issue, payload, at)
	}
	standing, err := store.Issue(ctx, tx, issue)
	if err != nil {
		return record.OutboxRow{}, err
	}
	// An issue the record does not hold stands in no phase and holds no row, so its reviewer's
	// task names neither a commit nor an entry.
	var standsIn phase.Phase
	var row record.PhaseRow
	if standing != nil {
		rows, err := store.Phases(ctx, tx, issue)
		if err != nil {
			return record.OutboxRow{}, err
		}
		standsIn, row = standing.Phase, record.PhaseRowOf(rows, start.Role)
	}
	start.Task = namingTheReviewRound(start.Task, start.Role, standsIn, row)
	start.RoundNamed = true
	return record.NewOutboxRow(issue, start, at)
}

// status queues the Dispatch write for a status the workflow has just given issue. reason is the
// text the write posts on the issue before the status lands (record.StatusWrite.Reason), which
// transition gives only for a move nothing else on Dispatch explains (moveReason), and is empty
// for every other write. A move that writes no status, since its target phase shares the status
// the issue already shows (implementing back to planning, both in_progress; accepting back to
// reviewing; merging back to retro) or has none, still posts its reason, as a message of its own:
// carried only on a status row, the reason would reach no issue, which would then show nothing of
// the move at all. A status that reads as out of the workflow is the daemon's own close — the
// done a merge writes — and it goes as a close row, fenced on the run it ends
// (record.CloseStatusWrite): the ordinary rule drops a row whose issue has left the workflow, which
// is every close, so a close judged by it would be lost on its first ISSUE_CHANGED and leave the
// issue open in Dispatch for good.
func (e *Engine) status(ctx context.Context, tx pgx.Tx, issue record.Issue, status, reason string) error {
	if status == "" || issue.Status == status {
		if reason == "" {
			return nil
		}
		return e.enqueue(ctx, tx, issue.Key, record.MessagePost{Body: reason})
	}
	if record.OutOfWorkflow(status) {
		return e.enqueue(ctx, tx, issue.Key, record.CloseStatusWrite(status, issue.Status, "", issue.Generation))
	}
	return e.enqueue(ctx, tx, issue.Key, record.OrdinaryStatusWrite(status, issue.Status, reason))
}

// moveReason is what a move's status write posts on the issue, for the one move nothing else on
// Dispatch explains: a worker's backward move (TriggerBackward, request_backward_move). The request
// reaches the daemon's API and no Dispatch record, so an architect's send-back from accepting
// showed a bare status change, with its reason only in the notice and the next role's task over
// Envoy. Every other move is recorded on Dispatch by the act that makes it, a tester's or a
// reviewer's hand-off, so it posts nothing: transition also carries a review's whole body, which
// does not go on the issue a second time. A person reads this text on the issue, so a backward
// move's names the role that asked and gives its words.
func moveReason(trigger TriggerKind, from, to phase.Phase, reason string) string {
	if trigger != TriggerBackward {
		return ""
	}
	return backwardMoveReason(from, to, reason)
}

// backwardMoveReason is a worker's backward move as the issue shows it: the phase it left, the one
// it went to, the role that asked (the role running the phase left, which backward holds the
// requester to) and that role's own words, or that it gave none.
func backwardMoveReason(from, to phase.Phase, reason string) string {
	asked := fmt.Sprintf("Legion moved this issue from %s back to %s at the %s's request", from, to, phase.RoleFor(from))
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return asked + ", which gave no reason."
	}
	if strings.HasSuffix(reason, ".") || strings.HasSuffix(reason, "!") || strings.HasSuffix(reason, "?") {
		return asked + ": " + reason
	}
	return asked + ": " + reason + "."
}

// backwardReasonRefusal is the refusal code and message for a backward move whose post Dispatch
// would refuse, and two empty strings for one it takes. The move posts its reason on the issue
// (moveReason), and Dispatch refuses a body past the bounds it publishes, or one that holds a
// decision block, on every attempt: refused at the post, the reason would reach no issue and the
// worker would never learn why. So backward refuses it before anything moves, as closeRoot does a
// close's reason (api's closeRoot). The text measured is the post the outbox makes, this move's
// sentence under its marker (record.LongestPost), read by the server's own reader
// (dispatchbody.Check), so the daemon refuses exactly what Dispatch refuses.
func backwardReasonRefusal(from, to phase.Phase, reason string) (string, string) {
	over := dispatchbody.Check(record.LongestPost(backwardMoveReason(from, to, reason)))
	if over == nil {
		return "", ""
	}
	if dispatchbody.IsDecisionBlock(over) {
		return "BACKWARD_REASON_DECISION_BLOCK", fmt.Sprintf("the reason holds a decision block, and the move posts the reason on the issue as a message, where nobody can answer one: %s; the move was not applied, so ask again with a reason that states what is wrong rather than asking a question", over)
	}
	return "BACKWARD_REASON_TOO_LONG", fmt.Sprintf("the reason makes a message Dispatch refuses: %s; the move was not applied, so ask again with a shorter reason", over)
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
// claim, since every notice of the tree that no child's own architect takes reaches that architect.
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
// round inherits an earlier decision. The architect reports no handoff, so it has none to clear:
// its phases row holds only the entry it was handed work on (EnterArchitect), which no move
// between phases clears.
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
// worker takes. Its start carries one mark, that its task is the one enter stored
// (startMarks.stored), and no other, a fresh session among them: the operator's `legion claims
// retry --fresh-session` reaches a retry of a held phase (restart) and never a transition's start.
// movedBack is whether the move into that phase was a move back to an
// earlier one, which is written on the row beside the entry and is the transition's to read, not
// this function's: the issue record already names the phase it moved into by the time a start
// is made. A phase with no worker starts nobody. Accepting starts no worker either: its role is
// the architect, which is already started — admission starts a root's with its tree and a child's
// own starts with the child's first phase (StartFirstPhase).
//
// Every phase start of a child, the move into accepting among them, first starts the child's own
// architect again when its claim has failed (reviveArchitect). The phase starts are the one place
// in the workflow that does this, since the transitions are the only phase starts. Outside the
// workflow, a re-admission brings a mid-phase child's failed architect back with the member's
// other roles (admit's startMidPhaseMembers), and so does an operator's retry. A restart of a held
// phase (restart), a promoted start (StartPromotedWorker), an owed entry (SendOwedEntry) and a
// resume task each hand a role the work of a phase the issue already stands in, and the owed-entry
// sweep runs every minute, so none of them revives the architect.
func (e *Engine) start(ctx context.Context, tx pgx.Tx, issue record.Issue, role claim.Role, task string, movedBack bool) error {
	if role == "" {
		return nil
	}
	if err := e.reviveArchitect(ctx, tx, issue); err != nil {
		return err
	}
	if role == claim.RoleArchitect {
		return nil
	}
	entry, sent, err := e.enter(ctx, tx, issue, role, task, movedBack, false)
	if err != nil {
		return err
	}
	return e.started(ctx, tx, issue, role, sent, supervise.OriginPhaseStart, entry, startMarks{stored: true})
}

// reviveArchitect queues the start of the architect of issue, a child, when the store records its
// claim failed and holds no start of it queued that the outbox will act on: a failed sub-architect
// starts again at its child's next phase start (dispatch://INFRA-3915 R2), and until then its
// child's notices go to the architect above it (record.OwningArchitect). The outbox's start
// retries a failed claim with fresh budgets (supervise.RequestRetry), in the session it recorded,
// or a new one once supervise's Machine.forgetSession has dropped it. This start asks for no new
// session itself. A tree root's architect is admission's, started with its tree,
// so a root revives nothing here.
//
// The claim's state is read in the transaction, and no row carries a mark that asks for the
// revive: an older daemon's strict decoder refuses an outbox row with a field it does not know
// (record.DecodeOutboxPayload), and a row it refuses fails every read of its role's runs
// (record.Store.RoleRuns), each admission pass of the issue among them. So the start is the plain
// one (childArchitectStart), with no phase, task, entry, slot mark or kept decisions. The read is
// stable: a failed claim stays failed until something starts it, and a start already queued with
// no phase or with the phase issue now stands in is that start (architectStartQueued) — the one
// the child's first phase start queues whatever the claim's state (gateOpened), or an earlier
// phase start's the outbox has not run yet — so no second start is queued beside it. An issue
// whose key makes no claim token has no claim to revive (ownArchitectRun).
func (e *Engine) reviveArchitect(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
	run, child, err := e.ownArchitectRun(ctx, tx, issue)
	if err != nil || !child {
		return err
	}
	if run.Claim == nil || run.Claim.State != supervise.StateFailed {
		return nil
	}
	if architectStartQueued(run, issue.Phase) {
		return nil
	}
	if err := e.enqueue(ctx, tx, issue.Key, childArchitectStart(issue)); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a child's phase start starts its failed architect again", "issue", issue.Key, "tree", issue.Tree, "phase", issue.Phase)
	return nil
}

// ownArchitectRun is the run of issue's own architect at issue's generation, its queued rows and
// its claim (record.Store.RoleRun), and false for an issue with no architect of its own to start:
// a tree root, whose architect is admission's, started with its tree, and an issue whose key makes
// no claim token. reviveArchitect and ownArchitectStarting read it.
func (e *Engine) ownArchitectRun(ctx context.Context, tx pgx.Tx, issue record.Issue) (record.RoleRun, bool, error) {
	if claim.IsTreeRoot(issue.Key, issue.Tree) {
		return record.RoleRun{}, false, nil
	}
	project, err := claim.ProjectToken(e.cfg.Project)
	if err != nil {
		return record.RoleRun{}, false, err
	}
	token, err := claim.NewToken(project, issue.Key, claim.RoleArchitect)
	if err != nil {
		return record.RoleRun{}, false, nil
	}
	run, err := e.store.RoleRun(ctx, tx, token, issue.Key, claim.RoleArchitect, issue.Generation)
	return run, err == nil, err
}

// architectStartQueued is whether run, a child's own architect's, holds a queued start the outbox
// will act on for the child standing in current: one of no phase, as a phase start queues
// (childArchitectStart), or of current, as a re-admission brings a mid-phase child's architect
// back (admit's startMidPhaseMembers). The outbox finishes, without acting, a start of a phase the
// issue has left, so a start of another phase does not count. It is queuedStartActs with no stop
// weighed against it (a stop of 0, which every row's id is above): every such start counts,
// whatever stop is queued beside it.
func architectStartQueued(run record.RoleRun, current phase.Phase) bool {
	return queuedStartActs(run, 0, "") || queuedStartActs(run, 0, current)
}

// childArchitectStart is the plain start of a child's own architect, issue's, with no phase, task,
// entry, slot mark or kept decisions: its catch-up at its launch's ready is its first instruction
// (claimReady). A root architect's start carries the task its record derives and the entry it
// raises (StartArchitect); a child's architect is handed no entry, and the rule that closes the
// difference between an architect's owed and held work reaches a tree's root alone
// (ArchitectAtWork). The child's first phase start queues it (gateOpened), and so does its next
// phase start while the claim has failed (reviveArchitect).
func childArchitectStart(issue record.Issue) record.SuperviseRequest {
	return record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleArchitect, Generation: issue.Generation}
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
// freshSession, and only the operator's `legion claims retry --fresh-session` does
// (record.SuperviseRequest's FreshSession).
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
	return e.started(ctx, tx, issue, role, task, supervise.OriginPhaseStart, row.Entry, startMarks{resend: true, freshSession: freshSession})
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
// what a sweep's and a ready's start carry, and a phase start's origin; a door that marks its
// start, or names an origin of its own, calls sendOwedEntry.
func (e *Engine) SendOwedEntry(ctx context.Context, tx pgx.Tx, issue record.Issue, role claim.Role, row record.PhaseRow) error {
	return e.sendOwedEntry(ctx, tx, issue, role, row, supervise.OriginPhaseStart, startMarks{})
}

// sendOwedEntry is the body SendOwedEntry, the builder of the boot's resume by rule and of the
// cutover's repair of a merger the `awaiting_merge` cutover left (resume.go's
// startPhaseOwner) and the silence repair (silence.go's repairSilence) share, with the origin
// and the marks of the door the start comes through (startMarks). A mark names that door and not
// the task's text, so what a claim behind its entry is sent is composed in one place however the
// start is made, and the boot's resume of a claim the daemon suspended goes on telling the outbox
// whose launch budget is at risk. The origin is the door's too: the task is the work of a phase
// entry, which admission's sends, the boot's resume and the cutover's repair name a phase start's
// (supervise.OriginPhaseStart), and the silence repair names its own (supervise.OriginSilence), so
// the repair's writes are counted apart from every other source's.
//
// Every start it writes is a re-assertion (startMarks.reasserts), whichever door it came
// through: the admission pass's sweep of the claims behind their entry, on every pass and on the
// sweep of its own each minute, its repair of a slot whose worker has no task, readyWorkerTask,
// the boot's resume by rule, the cutover's repair and the silence repair. Each of them sends work the record already
// owes the role — the entry and its task were written by the fact that raised the number, and
// that fact marked the issue moved where it wrote them (enter's two callers, start and
// StartPromotedWorker, both come through started as news of the issue) — so the movement is the
// fact that created the entry and not each delivery of it. R3 counts the daemon's own
// re-assertions as no movement (dispatch://INFRA-2560), and these must be among them: the minute
// sweep alone would otherwise reset a stuck tree's quiet time sixty times an hour, and the
// measure would never reach the bound on the stop it most needs to see. It is set here, once,
// rather than by each door, so no door can send an owed entry as news.
func (e *Engine) sendOwedEntry(ctx context.Context, tx pgx.Tx, issue record.Issue, role claim.Role, row record.PhaseRow,
	origin string, marks startMarks) error {
	task := row.EntryTask
	marks.stored = task != ""
	if task == "" {
		task = ResumePhaseTask(issue)
	}
	marks.reasserts = true
	return e.started(ctx, tx, issue, role, task, origin, row.Entry, marks)
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
// (record.SuperviseRequest.ResumeTask), and its task is the one enter stored (startMarks.stored).
// Its origin is a phase start's and not a resume's for the same reason it raises the entry: the
// re-admission hands the role its phase anew. The entry it raises is of no move back: the
// promotion hands the role the phase it already stands in, and moves the issue nowhere.
func (e *Engine) StartPromotedWorker(ctx context.Context, tx pgx.Tx, issue record.Issue, role claim.Role) error {
	task := ResumePhaseTask(issue)
	entry, sent, err := e.enter(ctx, tx, issue, role, task, false, false)
	if err != nil {
		return err
	}
	return e.started(ctx, tx, issue, role, sent, supervise.OriginPhaseStart, entry, startMarks{resumeTask: true, stored: true})
}

// startMarks are the marks a start's request carries beside its entry, which tell the outbox and
// the claim's machine how to read that entry and that task, and how to start the claim
// (record.SuperviseRequest), and the one mark the record reads instead, whether the start is news
// of its issue. The zero value marks none of them, which is a start of the work of an entry the
// claim is behind, resuming the session the claim records, and news of the issue.
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
	// mid-phase worker sets it (StartPromotedWorker), and so do the boot's resume, the cutover's
	// repair and the silence repair of a claim level with the entry its phases row names
	// (startPhaseOwner, which booted and cutoverMergerRepair share, and repairSilence).
	resumeTask bool
	// stored says the task is the stored task of the entry the start carries
	// (record.PhaseRow.EntryTask), as enter stored it, so the queue adds no sentence of the
	// reviewer's round to it (record.SuperviseRequest.RoundNamed, queuedRow): it holds them
	// already, word for word as the record does. The three starts that carry that text set it: a
	// transition's start and the promotion's start of a mid-phase worker, each carrying what enter
	// answers (start, StartPromotedWorker), and the send of an owed entry whose row stores a task
	// (sendOwedEntry). A task composed for the start is given the sentences whatever its text holds.
	// Only the reviewer's task holds them, so only a start of the reviewer carries the fact the
	// mark makes (started): the stored task of any other role names no review round.
	stored bool
	// ruleResume says the start is a boot step's start of a claim no fact of its issue asked to
	// start, so the outbox finishes the row rather than retrying the claim it resumed when that
	// claim spends its launch budget (record.SuperviseRequest.RuleResume). The boot sets it on the
	// two starts it makes of such a claim, and no other start carries it: its resume by rule
	// (booted) and the cutover's one-time repair of a merger the idle release suspended or left
	// live (cutoverMergerRepair), whose starts one builder makes (startPhaseOwner). The mark
	// names the door the start comes through — the boot's start of a claim no fact asked for,
	// where the outbox finishes the row rather than retrying a claim that is spending its launch
	// budget — and not the task's text. So each of the two carries it on the work of the entry its
	// role's phases row names as well as on the phase's resume task (sendOwedEntry), and changing
	// what the task says does not change whose budget is at risk.
	ruleResume bool
	// freshSession says the start must not resume the session the claim records but begin one of
	// its own (record.SuperviseRequest.FreshSession). Only the operator's `legion claims retry
	// --fresh-session` asks for it, which reaches the retry of a held phase alone (restart).
	freshSession bool
	// reasserts says the start sends work the record already owes the role rather than carrying
	// a fact new to the record, so it marks no movement of the issue (reassertion, startOrOwe). It
	// rides on no request: started reads it to tell startOrOwe which movement the start is, and
	// the outbox never sees it. sendOwedEntry sets it on every start it writes, which is where
	// the rule for the sends of an owed entry stands, and the three doors that send a claim level
	// with its entry the phase's resume task set it too, the boot's resume by rule, the cutover's
	// repair of a merger the `awaiting_merge` cutover left and the silence repair
	// (startPhaseOwner, which booted and cutoverMergerRepair share, and repairSilence), since
	// that resume is the same work sent again. The other three starts are news of the issue and
	// leave it unset: a transition's start and the promotion's start of a mid-phase worker, each
	// raising the entry it hands (enter), and the held phase's retry, which an architect's or an
	// operator's own request brings (restart).
	reasserts bool
}

// started enqueues the start each of its six callers makes, each of which has held role to one
// that works a phase and composed the task itself, at the entry that task is of — 0 for a
// resume that hands the role no new work, which is the boot's resume, the cutover's repair and
// the silence repair of a claim level with the entry its phases row names (startPhaseOwner,
// which booted and cutoverMergerRepair share, and repairSilence) — and each of which names its
// own marks and the origin of its task (record.SuperviseRequest.Origin): the phase's resume task
// the boot hands a level claim is a resume's (supervise.OriginResume), the one the cutover's
// repair hands a level merger is a phase start's (supervise.OriginPhaseStart), since the merging
// work is new to that merger, both starts of the silence repair, whichever task they hand, are
// the repair's own (supervise.OriginSilence), and every other start here hands the role a phase
// entry's work, a phase start's (supervise.OriginPhaseStart). It is the one place a start of a
// phase worker is built, so a reader asking what such a row carries reads this request and no
// other, and a field added here reaches every one of those starts. It goes through startOrOwe,
// so the worker comes back on the one row that carries its new task, and that row waits for the
// issue's admission slot where the phase it starts holds one: a transition's start into a phase
// up to the review puts a worker to work on the change, which is exactly what the cap counts. The
// issue carries the phase the transition wrote on it, so the rule reads the phase the start is
// for. Whether the start is news of its issue is marks.reasserts, handed on as the movement
// startOrOwe marks or leaves unmarked.
//
// One of the six is called from outside the engine's own transitions: sendOwedEntry, which
// admission calls through SendOwedEntry for the claims it sweeps and for the slot it repairs
// (admit's owedEntryRoles and resumeSlottedWorkers), which readyWorkerTask calls for a worker
// whose agent has just reported ready while its claim stands below the entry its phases row
// names (catch_up.go), which the boot's resume by rule and the cutover's repair call for a claim
// standing below it (startPhaseOwner, for booted's suspended claims and for the mergers of
// cutoverMergerRepair), and which the silence repair calls for a silent claim standing below it
// (repairSilence). A ready worker level with that row is given the phase's resume task instead,
// and that one start is enqueued directly rather than built here, for the reason StartOrOwe's
// own comment gives.
func (e *Engine) started(ctx context.Context, tx pgx.Tx, issue record.Issue, role claim.Role, task, origin string, entry int64, marks startMarks) error {
	move := newsOfTheIssue
	if marks.reasserts {
		move = reassertion
	}
	return startOrOwe(ctx, tx, e.store, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: role, Task: task,
		Origin: origin, Generation: issue.Generation, Phase: issue.Phase, Entry: entry, ResendEntry: marks.resend,
		ResumeTask: marks.resumeTask, RoundNamed: marks.stored && role == claim.RoleReviewer, RuleResume: marks.ruleResume,
		FreshSession: marks.freshSession}, e.now(), move)
}

// movement says whether a start of a phase worker is news of its issue, which startOrOwe marks
// (record.Store.MarkIssueMoved). It is a type of its own rather than a bare bool because it is
// handed beside the start's own moment and request, where a bare bool says nothing at the call
// site of what it decides.
type movement bool

const (
	// newsOfTheIssue is a start that carries a fact new to the record: a transition putting a
	// worker to work on the entry it raised, the promotion's start of a mid-phase member, which
	// raises one too, and a held phase's retry.
	newsOfTheIssue movement = true
	// reassertion is the daemon sending a phase worker again work the record already owes it:
	// every send of an owed entry (sendOwedEntry) and the phase's resume task sent to a claim
	// level with its entry by the boot's resume by rule, by the cutover's one-time repair
	// (cutoverMergerRepair) and by the silence repair. It is no movement of the issue (startOrOwe
	// says why).
	reassertion movement = false
)

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
// A start whose phase holds one of the cap's slots is marked for it, and the outbox holds such
// a row unclaimable while the issue holds no slot (store.go's outboxInOrder). The cap counts
// the issues the daemon has started whose change is still to write, each holding its row from
// its start until its work is past its review, except while it waits on another issue
// (record.KeepsSlot, blockers.waitsOf), so a start claimable at once
// would put a worker to work on an issue holding no slot — where the cap is full that is what
// it would do — and a worker put to work on an issue the cap has not admitted is what a start
// of a phase worker may never be. A transition's start, admission's start of a mid-phase member,
// the boot's resume by rule, the silence repair and the cutover's one-time start of a merger the
// `awaiting_merge` cutover left released (cutoverMergerRepair) all come through here, and all five
// come through Engine.started, so each of them waits where the phase it starts holds a row. The mark
// is this function's and not the caller's: it is the one reading of the phase the start carries
// (record.PhaseKeepsSlot), never a decision of one request. Every caller writes the phase on the
// record before it calls, so the payload's phase is the phase the issue stands in. The row
// carries its task while it waits, so the delivery is made as soon as the slot comes and the
// worker is up.
//
// A start the rule leaves unmarked stands in no waiting line — Store.OwedStarts lists a marked
// row alone, since that line is about slot-waiting — and it is coalesced like every other start
// of a phase worker. Store.EnqueueStart takes out the issue's pending start that names a phase,
// carries that phase's task and waits for what this start waits for, so a retro's or a merger's
// start is written in place of the start the issue already holds rather than beside it: two
// rows serving the phase the issue stands in, at its own generation and at one entry, would
// start its worker twice for one piece of work, and no fence of the outbox's ends the second.
// That pair is what the held phase's retry writes (restart, which sends the entry its row
// already names), while the first start is still queued. The one row is claimable the moment it
// commits, since no slot holds it back, and the outbox's own phase, generation and entry fences
// still end a stale one.
//
// A start that waits for a slot and one that waits for none take each other out nowhere, and
// need not: the mark is read from the phase, so two such rows never name the same phase, the
// issue stands in the newer one's, and the phase fence ends the older. What keeping them apart
// keeps is the waiting line, whose place belongs to the row that waits and is not a past-review
// row's to hand over, and the lane's own order behind a row that waits, a close's suspend among
// it (admit's promotion of a re-admitted tree queues its start behind that suspend, so the
// worker comes back after the stop rather than before it).
//
// A start with no task or no phase is refused here, before the mark. The two guards that would
// otherwise catch it both read the mark or the phase: record.validateOutboxPayload asks a task
// and a phase of a marked row alone, and the outbox's phase fence holds a row that names a phase
// alone (daemon's outbox, "serves a phase the issue has left"). While every start was marked,
// validation refused such a row; an unmarked one would now reach a worker with no work and no
// phase to fence it on. Every start the daemon writes here is built by Engine.started, which
// writes the phase the issue stands in and a task its caller composed, so the refusal rejects no
// start the daemon writes today. The task and the phase are also what tells a phase worker's
// start from the issue's other supervise starts, which is how Store.EnqueueStart finds the one
// row an issue holds for a wait (record's pendingPhaseStart), so a start written without them
// would stand beside that row rather than in its place.
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
// the cap counts is its root's, the production check's look (check.go), the pod wake (wake.go),
// the resume of a cancelled idle release (the daemon's outbox), the roles admission brings back
// beside a member's phase worker (admit's startMidPhaseMembers) and the phase's resume task a
// ready worker level with the entry its phases row names is given (catch_up.go's
// readyWorkerTask) are enqueued directly and stay claimable at once. None of the first five puts
// a worker to work on a phase, so none of them is the cap's to hold, and a slot they waited for
// might never be offered. The last is sent to a worker already at work on the phase it is in —
// its agent has just reported ready — so in a phase up to its review the issue holds the row the
// cap counts already or gave it back to wait on another issue, a wait that stops none of its
// work, and past its review it needs none (record.KeepsSlot), so this one has nothing to wait
// for in any case. The work of an entry that same ready worker stands below is sent through
// SendOwedEntry, and that start comes through here with the rest.
func StartOrOwe(ctx context.Context, tx pgx.Tx, store record.Store, issue string, payload record.SuperviseRequest, at time.Time) error {
	return startOrOwe(ctx, tx, store, issue, payload, at, newsOfTheIssue)
}

// startOrOwe is StartOrOwe told whether the start is news of its issue (movement). The starts
// that are not are written through here all the same, so they take the slot mark, the one
// statement that writes a start and the refusal of a start with no task or no phase from the one
// implementation rather than from a second start path that would have to keep in step with this
// one.
func startOrOwe(ctx context.Context, tx pgx.Tx, store record.Store, issue string, payload record.SuperviseRequest, at time.Time,
	move movement,
) error {
	if payload.Task == "" || payload.Phase == "" {
		return fmt.Errorf("the start of a phase worker on %s requires a task and a phase", issue)
	}
	payload.NeedsSlot = record.PhaseKeepsSlot(payload.Phase)
	row, err := queuedRow(ctx, tx, store, issue, payload, at)
	if err != nil {
		return err
	}
	// Putting a worker to work is a movement of the issue (the rule in workflow's dispatchIssue),
	// and it is marked here rather than at each caller because this is where every start of a
	// phase worker is written: every one of them is built by Engine.started, which a transition's
	// start, the held phase's retry, admission's start of a mid-phase member, the sends of an
	// owed entry and the three resumes of a claim level with its entry (the boot's, the cutover
	// repair's and the silence repair's) all go through. The moment
	// is the one the caller was handed, the fact's own, not a clock read again here.
	//
	// It is marked whether or not the row written takes out one the issue already owes: an issue
	// that has waited for its slot since its first start and takes a later transition's task has
	// moved, and the row it keeps is only its place in the waiting line.
	//
	// A reassertion is not marked. It is the daemon sending a phase worker work the record
	// already owes it: every send of an owed entry (sendOwedEntry, whose callers are admission's
	// sweep on every pass and each minute, its slot repair, readyWorkerTask, the boot's resume by
	// rule, the cutover's one-time repair (cutoverMergerRepair) and the silence repair), and the
	// phase's resume task the boot's resume, the cutover's repair and the silence repair send a
	// claim level with its entry. A movement is a fact new to the record, and the
	// daemon's own re-assertions are none (dispatch://INFRA-2560, R3): nothing outside the daemon
	// happened, and the work is not new to the record. The fact that made it so is the one that
	// raised the entry, and that fact marked the issue where it raised it: a transition's phase
	// write (Engine.transition) and its start, which comes through here as news, and a
	// promotion's (admit's promoteHolds and readmit) with its start of a mid-phase member. It is
	// the case of the pod wake (wake.go's wakeRow), whose start brings a woken role back to work
	// it already holds and is enqueued directly, so it marks nothing either. The reason is what
	// the mark is for: the quiet time measures a tree that has stopped, and these sends are the
	// daemon reacting to that stop or closing a difference it already knew of, so marking them
	// would make the measure read its own repair as recovery — a stopped tree's clock set back by
	// the minute sweep sixty times an hour, and the stalled-tree notice put off for as long as
	// the daemon kept sending. When a send works, the worker's own claim steps move the tree
	// through the quiet measure (record.QuietTrees), which is where a recovery is read.
	if move == newsOfTheIssue {
		if err := store.MarkIssueMoved(ctx, tx, issue, at); err != nil {
			return err
		}
	}
	return store.EnqueueStart(ctx, tx, row)
}

// enter raises the phase entry this start hands role on issue, writes the task of that entry and
// the moment it raised it, and answers the number for the start's payload and the task the start
// sends. All of it is the transition's own transaction, so the row names the new entry before the
// outbox can run that start, which is what makes the comparison safe: while a role's turn
// outlives its phase its claim still serves the earlier entry (the machine refuses a second
// delivery while one is pending), and the row already names this one.
//
// Three starts raise one, each handing its role work it was not already on: a transition's start
// of the phase the issue has moved into (start), the promotion's start of a mid-phase worker a
// re-admitted tree brings back (StartPromotedWorker), and a root architect's start of work it was
// not on (EnterArchitect: StartArchitect and ResumeAwaitedRoot). Every other start carries the
// entry its role's phases row already names — the send of an owed entry (SendOwedEntry), the
// sweep's start of a root architect behind its entry (SendArchitectStart), the held phase's retry
// (restart) — or is a start of no entry (record.SuperviseRequest.Entry), so none of them moves
// the row and a worker's in-flight completion is never refused for an entry it was never given.
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
// record alone (SendOwedEntry). For the reviewer entering a review round that text is task ending
// with the sentences of its round, the commit its row holds and then the entry just raised, as the
// queue writes them into every other task of the reviewer (namingTheReviewRound, queuedRow). That
// is why enter answers the text and its two phase-worker callers send what it answers, marked as
// the stored task (startMarks.stored) so the queue writes the sentences no second time, never
// their own task: the number is known only once it is raised, here.
//
// movedBack is written with them: whether the start raising this entry was a move back to an
// earlier phase. Its one reader is the completion route's entry fence, which takes a completion
// of an earlier entry at the head the record holds and refuses it whatever head it names when
// the entry standing now was raised by a move back, since such an entry asks for the phase's
// work to be done again (record.PhaseRow.EntryMovedBack, api/handoff.go). It is written here
// rather than counted anywhere (record.PhaseRow.EntryMovedBack says why no count answers it).
//
// verbatim is whether the sweep's send of a root architect's entry again sends that task as
// written (record.PhaseRow.EntryVerbatim), which only the resume of an answered root says. Every
// raise writes it, so a raise that does not say it clears the mark an earlier entry's resume left.
func (e *Engine) enter(ctx context.Context, tx pgx.Tx, issue record.Issue, role claim.Role, task string, movedBack, verbatim bool) (int64, string, error) {
	row, err := e.phaseRow(ctx, tx, issue.Key, role)
	if err != nil {
		return 0, "", err
	}
	row.Entry++
	row.EntryVerbatim, row.EntryRaisedAt, row.EntryMovedBack = verbatim, e.now(), movedBack
	// The entry's sentence is named only while the row holds the task of that entry, as the row
	// written here does: raised is that row, read for the sentences alone, and the row is given its
	// task once, the text the worker takes.
	raised := row
	raised.EntryTask = task
	row.EntryTask = namingTheReviewRound(task, role, issue.Phase, raised)
	if err := e.store.PutPhase(ctx, tx, row); err != nil {
		return 0, "", err
	}
	// Every entry a phase worker is handed is the start of a run of its step: a transition's start
	// and the promotion's start of a mid-phase member (StartPromotedWorker). The architect's own
	// entry is no step of the issue's task.
	if step, ok := standingStep(issue); ok && role != claim.RoleArchitect {
		agent, err := stepClaim(issue, issue.Key, role)
		if err != nil {
			return 0, "", err
		}
		if err := e.startStep(ctx, tx, issue, step, agent); err != nil {
			return 0, "", err
		}
	}
	return row.Entry, row.EntryTask, nil
}

// EnterArchitect raises the phase entry a root's architect is handed on root, writes task as the
// work of that entry, whether the sweep sends it again as written (verbatim, enter) and the
// moment it was raised, and answers the number for the start that hands it
// (record.SuperviseRequest.Entry). A root's architect holds a row of the phases record exactly as
// every phase worker does, so what it was handed, what that work is and how long it has stood are
// read from the record rather than from the outbox row that carried it.
//
// It is the one way a root architect's entry is raised, and two starts that hand the architect
// work it was not on make it. StartArchitect does, for admission's promotion of a queued root
// (admit's LineQueuedRoot branch) and for the sweep's raise for an architect whose row names no
// entry, with the derivation's task, which the sweep derives again. ResumeAwaitedRoot does, for
// the resume of a root a person answered, with the task that names the answer, which the sweep
// sends as written. Each composes the architect's task itself and enqueues its start itself,
// because an architect's start waits for no admission slot, so neither writes through start or
// started, both of which are a phase worker's. The rule it is held to is enter's own, which is
// why it is one call and not a second implementation: it is made in the transaction that writes
// that start. The entry is raised by no move back (record.PhaseRow.EntryMovedBack): that mark's
// one reader is the completion route's entry fence, and the architect completes nothing there.
func (e *Engine) EnterArchitect(ctx context.Context, tx pgx.Tx, root record.Issue, task string, verbatim bool) (int64, error) {
	entry, _, err := e.enter(ctx, tx, root, claim.RoleArchitect, task, false, verbatim)
	return entry, err
}

// task is what a started worker is told. It names the phase the worker starts, which the issue
// record already holds: one role runs several phases (the implementer runs implementing and
// retro), and a resumed session cannot otherwise tell a new phase from its last.
// The handoff a phase ended with is a Dispatch handoff record on the issue, which the line names by
// its phase word and version with the command that reads that version, the record the daemon
// accepted, whatever was written to the phase since; a commit is named, as `Handoff commit:`,
// whenever the completion reported one: the head the planner's, the implementer's, the tester's or
// the reviewer's record names, and the commit retro and the merger stand on.
// A handoff's verdict is always the tester's, since the daemon accepts one from no other role on
// a phase completion (api/handoff.go, VERDICT_ROLE_FORBIDDEN; the implementer's check report is no
// phase completion and starts nobody), so the line names the tester: the reviewer it starts on a
// pass reads it as the tester's report, never as a field of its own completion.
//
// A merger started in merging at the head a merge forward landed past the review, while the
// review that landing asked of the reviewer stands for that head (mergeForwardReviewOwed), is told
// to merge that head only once the reviewer's approval of it is on the pull request: the daemon
// records no decision outside reviewing, so the approval is the merger's to read on GitHub. The
// merger's task is built at retro's completion, the one move into merging, and at the retry of a
// held merging (retryPhase); each reads the pull request in its own transaction, so pr says what
// the record holds.
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
	if issue.Phase == phase.Merging && pr != nil && mergeForwardReviewOwed(*pr) {
		text += fmt.Sprintf(" Head %s is the merge of %s forward that landed after the review, and the reviewer is asked to read it: merge head %s only once the reviewer's approval of it is on the pull request.",
			pr.HeadSHA, pr.MergeForward.Base, pr.HeadSHA)
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
// tells a carried root's own architect with it, since the hold's first notice was routed in the
// tree the root was handed over from, where it reached an architect above the root unless the
// root's own architect held its role then.
func HeldNotice(issue record.Issue) record.Notice {
	return record.Notice{Kind: "held", Role: phase.RoleFor(issue.Hold.From), Phase: issue.Hold.From, Reason: string(issue.Hold.Reason)}
}

// ChildReenteredNotice is the notice the architect above a re-entered child is told, whichever path
// re-enters it: reenterChild for a live tree's reopened child, or admission's
// reenterStrandedChildren for one a reopen declined to re-enter for want of the label until its
// root's own promotion made the tree live.
func ChildReenteredNotice(child, root string) record.Notice {
	return record.Notice{Kind: "child-status", Role: claim.RoleArchitect, Reason: fmt.Sprintf("%s is todo; it runs again under %s", child, root)}
}

// ChildStartedNotice is the notice the architect above a child is told whenever the child starts
// its first phase (Engine.StartFirstPhase), its reason saying why it starts now and that the
// child's own architect starts with it. waited is every issue the child's mirror row still names,
// in Dispatch's own order, all of them done by now: the last of them closed, which is what freed
// the child.
//
// Empty, the child's own links say nothing about why it starts, and the reason says what is true
// of every child the line starts instead: it reached the front of the waiting line and took a
// slot. That covers both an issue that never named a blocker — which is most of them now that a
// child waits for a slot like any other issue — and one whose blocked-by links a person or the
// architect took off while it waited, which the record cannot tell apart from the first. Naming
// links that were taken off would be a guess, and a reason the architect reads and cannot check
// is worse than a plain one.
//
// The reason then says what the reader keeps of the child: from this start the child's own
// architect owns it — its decomposition, its children and its acceptance — while that architect
// holds its role. So the reader hears the child's child-status and child-closed and that
// architect's worker-died and worker-stalled, and, while that architect does not hold its role,
// every other notice of the child too (record.OwningArchitect).
//
// A start nobody hears of is the silent state the hold's report exists to remove, so no child
// starts without one. It is the same child-status kind a child's leave and a re-entry are told
// with — the daemon's notice routing starts the walk for one at the child's parent, so it reaches
// the architect above the child — and not phase-finished: no phase of the child ended, it is
// beginning its first.
func ChildStartedNotice(child, root string, waited []string) record.Notice {
	why := fmt.Sprintf("every issue it waited on (%s) is done", strings.Join(waited, ", "))
	if len(waited) == 0 {
		why = "it reached the front of the waiting line and took an admission slot"
	}
	reason := fmt.Sprintf("%s starts planning under %s: %s. Its own architect starts with it and owns it while that architect "+
		"holds its role: its decomposition, its children and its acceptance. You hear its child-status and child-closed "+
		"notices, a worker-died or worker-stalled of its architect, and its other notices while its architect does not "+
		"hold its role, which the daemon's holdsRole decides", child, root, why)
	return record.Notice{Kind: "child-status", Role: claim.RoleArchitect, Reason: reason}
}

func (e *Engine) lingerAt() time.Time { return e.now().Add(e.cfg.Linger) }
