package workflow

import (
	"context"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The work hold (`gates.hold: work`) as the workflow writes it. Under that hold a tree's closed
// standing design gate holds its running work as well as its merge: at the close every phase
// worker of the tree is told to end its turn and is stopped with its session kept, and at the
// opening each claim the hold stopped is started again with one task. The workflow writes the
// rows; the outbox runs them (internal/daemon's outbox, its suspend and start ops and its
// role message), and supervise holds the claim (supervise.SuspendedByHold, Machine.EndHold).
//
// The hold is the deployment's word (Config.Hold) over the one predicate the merge hold reads too
// (record.TreeGate.Standing, the standing gate), which the hold asks under `work` alone
// (holdIsWork). The record keeps the hold of each tree (record.WorkHold): the close writes it,
// and the boot reads it to tell a tree whose close was never applied from one whose drain is
// under way (workHoldBooted). The opening deletes the hold that stands and keeps it, with what
// ended it, as the record of the tree's last opening (record.Store.OpenWorkHold). Every start of a
// claim the hold stopped builds its task from that record (heldClaimOpening), so a claim the
// opening did not start gets the task the opening gave, and the outbox builds the task's
// sentences again from the record when the start runs (HeldClaimOpening), so a start queued at
// one opening gives the words of the latest.

// HoldStopGrace is how long after the work hold's close message reached a claim a turn still
// running is cut: long enough for a worker to end its turn at its next step and for a merger to
// take its pull request out of the merge queue. The outbox names each stop's deadline that far
// after the message reached the claim (internal/daemon's postHoldStop), the close names the
// deadline of the stop of a start under way that far after the close (workHoldClose), and the
// close message names it to the worker (workHoldSentence).
const HoldStopGrace = 2 * time.Minute

// The sentences of the hold's texts beside today's gate messages (gateClosedHead,
// gateOpenedMessage). Every one is a reader's words and part of the contract.
var (
	// workHoldSentence follows today's first sentence of the close under `work`, and every opening
	// task repeats it. The time it names is HoldStopGrace.
	workHoldSentence = fmt.Sprintf("The deployment holds the tree's work until every gated step stands open again: no phase "+
		"starts, and the daemon stops each phase worker, with its session kept, when its turn ends or %d minutes after this message.",
		int(HoldStopGrace/time.Minute))
)

const (
	// workerCloseEnd ends the close message to a phase worker, and architectCloseEnd the one to an
	// architect, whom the hold does not stop: its own work goes on, and what it reports (a
	// sign-off, a move back) is recorded while the start that report makes waits for the opening.
	workerCloseEnd    = "End your turn now."
	architectCloseEnd = "Your own work goes on, and a step that you report starts nothing until every gated step stands open again."
	// holdStoppedSentence and checkTurnSentence frame today's open message in the opening task.
	holdStoppedSentence = "The work hold stopped your turn."
	checkTurnSentence   = "Check what your turn already did: a tool call may not have finished."
	// noPhaseWork is the work of an opening task to a claim that holds no phase of its issue.
	noPhaseWork = "The issue's other roles may ask you questions."
)

// gateClosedHead is today's first sentence of the close message: the tree whose gate closed, and
// the version, the step and the document that closed it. The step and its own document, never
// "spec version": under more than one gated step the version is that step's document's, and a
// message naming the spec would send a reader to the wrong one. The merge hold's close message is
// this sentence and its own ending (gateClosedSentence).
func gateClosedHead(tree string, step record.DesignStep, document string, version int) string {
	return fmt.Sprintf("The design gate of %s closed: version %d of its %s document (%s) is with its reviewer, "+
		"so the design you are working to may change.", tree, version, step, document)
}

// workCloseMessage is the close message under `work`: today's first sentence, the hold's, and the
// ending of the recipient, an architect or a phase worker.
func workCloseMessage(hold record.WorkHold, architect bool) string {
	end := workerCloseEnd
	if architect {
		end = architectCloseEnd
	}
	return gateClosedHead(hold.Tree, hold.Step, hold.ArtifactID, hold.Version) + " " + workHoldSentence + " " + end
}

// bootOpenedSentence is what the boot's end of a tree's hold says in place of today's open
// message: the tree's gate still stands where it stood, and the deployment moved to `merge` or
// turned the gate off. That is true at that end alone, so no other path says it.
func bootOpenedSentence(tree string) string {
	return "The deployment no longer holds the work of " + tree + "'s tree."
}

// unrecordedOpeningSentence is what a start of a claim the hold stopped says in place of what
// ended the hold where the daemon keeps no record of the hold it can use: a new generation of the
// tree deleted it, the boot dropped an older generation's record, the opening found no row to
// keep, or the record is of an older generation of the root, which no start uses
// (openStoppedClaims). The start cannot name the version that closed the gate, and says so.
func unrecordedOpeningSentence(tree string) string {
	return "The work hold of " + tree + "'s tree has ended, and the daemon keeps no record of the version that closed its gate."
}

// openingSentences is the opening sentences around ended, what ended the hold: the close message's
// two sentences, which name the version that closed the gate, that the hold stopped the turn,
// ended, and the check of what the turn already did, since a cut turn can leave a tool call
// unfinished. With no closing record, the hold's own sentence alone stands in front.
func openingSentences(closing *record.WorkHold, ended string) string {
	opening := workHoldSentence
	if closing != nil {
		opening = gateClosedHead(closing.Tree, closing.Step, closing.ArtifactID, closing.Version) + " " + opening
	}
	return opening + " " + holdStoppedSentence + " " + ended + " " + checkTurnSentence
}

// heldClaimOpening is what the one task of every start of a claim the hold stopped starts with:
// the opening's own start (endWorkHold), each start of a claim the opening did not start
// (openStoppedClaims), and each start the outbox runs against a claim the hold holds
// (HeldClaimOpening). opened is the record of the tree's last opening (record.WorkHold, not
// Standing), nil where the daemon keeps none. With the record, it is the opening's sentences
// around what ended the hold (WorkHold.Opening): today's open message, or the boot's sentence, so
// each start gives the words the opening gave. With none, it is the hold's own sentence and
// unrecordedOpeningSentence of tree.
func heldClaimOpening(opened *record.WorkHold, tree string) string {
	if opened == nil {
		return openingSentences(nil, unrecordedOpeningSentence(tree))
	}
	return openingSentences(opened, opened.Opening)
}

// HeldClaimOpening is the opening sentences of a start of a claim the hold stopped in root's tree,
// built when the start runs (internal/daemon's startOfAHeldClaim) from hold, the tree's record of
// its hold as it stands then (record.Store.TreeWorkHold), nil for none. It is heldClaimOpening of
// that record (lastOpening), so a start queued at one opening that runs after a later close and
// opening gives the words of the later opening: the version that closed the gate last, and what
// ended the hold last. standing reports a hold that stands in the record: the gate closed again,
// no start of a claim the hold stopped goes on, and opening is "".
func HeldClaimOpening(hold *record.WorkHold, root record.Issue) (opening string, standing bool) {
	hold = lastOpening(hold, root)
	if hold != nil && hold.Standing() {
		return "", true
	}
	return heldClaimOpening(hold, root.Tree), false
}

// OpeningTellsTheGate is whether the opening's sentences a start of a claim the hold stopped in
// root's tree gives now (HeldClaimOpening) carry today's open message, from hold, the tree's record
// of its hold as it stands then: a record of root's generation (lastOpening) whose hold no longer
// stands, and whose opening tells the gate's opening (tellsOpening). Such a start is that holder's
// one delivery of the opening (internal/daemon's settleHeldOpening, openHeldClaims).
func OpeningTellsTheGate(hold *record.WorkHold, root record.Issue) bool {
	hold = lastOpening(hold, root)
	return hold != nil && !hold.Standing() && tellsOpening(hold, root)
}

// lastOpening is hold, the record of root's tree's hold, where a start of a claim the hold stopped
// may use it, and nil where the record is of an older generation of the root: the hold of a run
// that ended, which no start uses (openStoppedClaims, HeldClaimOpening).
func lastOpening(hold *record.WorkHold, root record.Issue) *record.WorkHold {
	if hold != nil && hold.RootGeneration != root.Generation {
		return nil
	}
	return hold
}

// workHoldStands is whether the work hold of issue's tree stands in the record (record.WorkHold):
// a hold of the tree root's generation that no opening has ended. The close writes it in the
// transaction that closes the gate under `work`, and the opening ends it in the transaction that
// opens the gate, so a reader of the record in a fact's transaction reads the hold as that fact
// moves it; under `merge` no close writes one. A tree root the record does not hold has no hold.
func workHoldStands(ctx context.Context, tx pgx.Tx, store record.Store, issue record.Issue) (bool, error) {
	root := &issue
	if !claim.IsTreeRoot(issue.Key, issue.Tree) {
		var err error
		if root, err = store.Issue(ctx, tx, issue.Tree); err != nil || root == nil {
			return false, err
		}
	}
	hold, err := store.TreeWorkHold(ctx, tx, issue.Tree)
	if err != nil {
		return false, err
	}
	hold = lastOpening(hold, *root)
	return hold != nil && hold.Standing(), nil
}

// TellsOwnersAcceptance is whether notice, of issue as the record holds it now, tells issue's
// architect that issue waits in its own acceptance, the owner's place, of an issue that stands in
// accepting: a phase-finished notice, and the notice of a return out of awaiting_children into
// accepting, which tells the architect that the issue waits on its sign_off in place of the
// phase-finished notice the wait replaced (ReturnFromChildren). That return's notice is the one of
// its kind that names awaiting_children as the phase the issue left (record.Notice.Phase); the
// notice of a move into the wait names the phase the issue left for it, and tells no acceptance. In
// this release accepting is always the owner's place, the step `acceptance:owner` of a plan and the
// accepting of today's line that an issue with no plan rows walks (plan.Supported takes acceptance
// with the key owner alone), and the phase-finished notice of the step before it is what tells the
// architect to accept (prompts/go/architect-common.md). Under `work` the outbox holds such a notice
// while the tree's standing gate is closed, and lets every later notice of the architect pass it
// (internal/daemon's deliverNotice and earlierNoticeFor). The notice is read against the issue at
// each attempt, so one whose issue has left accepting since, by the architect's own sign-off or move
// back, tells nothing of the kind.
func TellsOwnersAcceptance(issue record.Issue, notice record.Notice) bool {
	if issue.Phase != phase.Accepting {
		return false
	}
	return notice.Kind == "phase-finished" || (notice.Kind == AwaitingChildrenNoticeKind && notice.Phase == phase.AwaitingChildren)
}

// holdReason is the reason a stop of the hold logs on the claim it stops.
func holdReason(hold record.WorkHold) string {
	return fmt.Sprintf("the work hold: version %d of the %s document (%s) of %s closed the design gate",
		hold.Version, hold.Step, hold.ArtifactID, hold.Tree)
}

// holdIsWork is whether the deployment's hold is `work` (Config.Hold).
func (e *Engine) holdIsWork() bool { return e.cfg.Hold == config.HoldWork }

// treeClaim is one role of an in-workflow member of a tree as the hold reads it: the member, the
// role, the claim token and the role's queued rows and claim at the member's generation, whose
// Claim is nil for a role the daemon holds no claim of.
type treeClaim struct {
	issue record.Issue
	role  claim.Role
	token claim.Token
	run   record.RoleRun
}

// state is the claim as the rules of supervise read it, the zero claim for none.
func (c treeClaim) state() supervise.Claim {
	if c.run.Claim == nil {
		return supervise.Claim{}
	}
	return supervise.Claim{State: c.run.Claim.State, SuspendedBy: c.run.Claim.SuspendedBy}
}

// live is whether the claim holds its role now: its process has not been let go
// (supervise.IsGone), which a claim launching or queued for its launch has not either.
func (c treeClaim) live() bool {
	return c.run.Claim != nil && !supervise.IsGone(c.run.Claim.State)
}

// treeClaims reads the claims of every member of root's tree that is in the workflow
// (record.InWorkflow), members by key: each role of claim.Roles, in that order, the architect
// first, and then each other claim the store holds of the member whose role is a worker role
// (claim.IsPhaseWorker), in token order (record.Store.StoredClaims), with the role's queued rows
// and claim at the member's generation, in batches (RoleRuns). A role of claim.Roles is read
// whether or not the store holds a claim of it, since a start under way or a holder owed news can
// have none yet (workHoldClose, heldOpeningMessage). A stored claim is read by the token and role
// the store holds it under, so the hold tells, stops and starts every worker claim of a member,
// not the roles of one list alone: every role but the architect's and the controller's, idle claims
// of finished steps among them. A member out of the workflow is left out, a closed one whose
// production check a role looks at among them: the hold holds no work of an issue that has left
// the workflow. A role of claim.Roles whose token cannot be built is logged and left out, as
// tellGateMoved leaves such a holder untold.
func (e *Engine) treeClaims(ctx context.Context, tx pgx.Tx, root record.Issue) ([]treeClaim, error) {
	members, err := e.store.TreeIssues(ctx, tx, root.Tree)
	if err != nil {
		return nil, err
	}
	project, err := claim.ProjectToken(root.Project)
	if err != nil {
		return nil, err
	}
	members = slices.DeleteFunc(members, func(member record.Issue) bool { return !record.InWorkflow(member) })
	keys := make([]string, len(members))
	for i, member := range members {
		keys[i] = member.Key
	}
	stored, err := e.store.StoredClaims(ctx, tx, keys)
	if err != nil {
		return nil, err
	}
	var claims []treeClaim
	for _, member := range members {
		listed := make(map[claim.Token]bool, len(claim.Roles))
		for _, role := range claim.Roles {
			token, err := claim.NewToken(project, member.Key, role)
			if err != nil {
				e.logOnCommit(ctx, "workflow: the work hold cannot name this role, so it neither tells nor stops it",
					"tree", root.Tree, "issue", member.Key, "role", role, "error", err)
				continue
			}
			claims = append(claims, treeClaim{issue: member, role: role, token: token})
			listed[token] = true
		}
		for _, held := range stored[member.Key] {
			if claim.IsPhaseWorker(held.Role) && !listed[held.Token] {
				claims = append(claims, treeClaim{issue: member, role: held.Role, token: held.Token})
			}
		}
	}
	for batch := range slices.Chunk(claims, strandedRoleRunBatchSize) {
		keys := make([]record.RoleRunKey, len(batch))
		for i, c := range batch {
			keys[i] = record.RoleRunKey{Token: c.token, Issue: c.issue.Key, Role: c.role, Generation: c.issue.Generation}
		}
		runs, err := e.store.RoleRuns(ctx, tx, keys)
		if err != nil {
			return nil, err
		}
		for i := range batch {
			batch[i].run = runs[batch[i].token]
		}
	}
	return claims, nil
}

// workHoldClose is the work hold's close of root's tree at version of step's document: the gate's
// move to closed under `work` (tellGateMoved), and the boot's close of a tree it finds held with
// no record (workHoldBooted). It writes the record and tells each role of the tree what the hold
// does to it:
//
//   - every live architect, the root's and each member's own, the close and "Your own work goes
//     on, and a step that you report starts nothing until every gated step stands open again.":
//     the hold stops no architect;
//   - every live phase worker that is not a merger (planner, implementer, tester, reviewer), idle
//     claims of a finished phase among them, the close and "End your turn now.", carrying its stop
//     (record.RoleMessage.Stop), which the outbox posts once the message reached the claim, with
//     the deadline HoldStopGrace on;
//   - every merger it reaches the same message, marked to wait for the merger's session
//     (record.RoleMessage.Wake): a live merger, a released one, which the message wakes, and one
//     that is neither while a start of it is under way (StartUnderWay), which may launch the
//     merger with its task after this close. The merger takes its pull request out of the merge
//     queue only on this message, so the message waits while the claim is not gone and its
//     session does not hold the role's topic yet, or while its holder is unresponsive. The outbox
//     posts the stop once the message ended, so the stop follows the message (internal/daemon's
//     deliverRoleMessage, closeWaitsForAMerger, postHoldStop);
//   - a released worker that is not a merger nothing: a wake would bring its pod back only to stop
//     it, so its stop is written at once, with the deadline HoldStopGrace after this close and no
//     turn, and the hold takes its `released` source over with its session and its pending task
//     kept. A wake that passed the fence before this close may launch the claim after it: the
//     stop then holds the launch until its ready, and cuts a turn at the deadline where a restart
//     or a failed attempt of the stop lets the ready come first;
//   - a phase worker that is not a merger, neither live nor released, and whose run holds a start
//     under way (StartUnderWay), nothing, and its stop is written at once, with the deadline
//     HoldStopGrace after this close and no turn: the start read the gate open before this close,
//     and it may create, resume or retry the claim after it. The stop's row is newer than the
//     start's, so it still acts on the claim that start launches (StopActs), and the outbox runs
//     it once the start's lease ends: it stops the launching claim at its ready, before the claim
//     takes any task (supervise's holdSuspendUntilReady). That hold lives in memory alone, so
//     where a restart or a failed attempt of the stop lets the ready come first, the stop's next
//     attempt stops a ready or idle claim at once and cuts a turn at the deadline. A stop that
//     finds a claim with no launch yet, queued or with a launch a restart left uncertain, waits on
//     its row's backoff for the launch (internal/daemon's holdStop). Any other stop that finds no
//     launch makes nothing: a claim that runs nothing is not stopped again, and an operator's or
//     the daemon's suspension stands;
//   - and any other claim an operator or the daemon suspended, or a failed or retired one,
//     nothing: the operator's suspension and the daemon's stand above the hold's.
//
// Each stop names the version that closed the gate (record.SuperviseRequest.HoldVersion), and is
// one log line naming the issue, the role and the version. The stop a message carries names the
// step too (record.HoldStop.Step). Each message is marked as a close message
// (record.RoleMessage.Close), so the outbox publishes none of them once the gate opened again, or
// once a newer close of the gate stands.
func (e *Engine) workHoldClose(ctx context.Context, tx pgx.Tx, root record.Issue, step record.DesignStep, document string, version int) error {
	hold := record.WorkHold{Project: root.Project, Tree: root.Tree, RootGeneration: root.Generation, Step: step,
		ArtifactID: document, Version: version, ClosedAt: e.now()}
	if err := e.store.PutWorkHold(ctx, tx, hold); err != nil {
		return err
	}
	claims, err := e.treeClaims(ctx, tx, root)
	if err != nil {
		return err
	}
	reason, workerClose, architectClose := holdReason(hold), workCloseMessage(hold, false), workCloseMessage(hold, true)
	stopOf := func(c treeClaim) record.HoldStop {
		return record.HoldStop{Tree: c.issue.Tree, Role: c.role, Generation: c.issue.Generation, Step: hold.Step, Reason: reason, HoldVersion: hold.Version}
	}
	deadline := hold.ClosedAt.Add(HoldStopGrace)
	told, stopped := 0, 0
	for _, c := range claims {
		var how string
		underWay, _ := StartUnderWay(c.run, root)
		switch {
		case c.role == claim.RoleArchitect && c.live():
			if err := e.enqueue(ctx, tx, c.issue.Key, record.RoleMessage{Role: string(c.token), Body: architectClose, Close: true}); err != nil {
				return err
			}
			told++
			continue
		case c.role == claim.RoleArchitect:
			continue
		case c.role == claim.RoleMerger && (c.live() || c.state().Released() || underWay):
			how = "told to end its turn once its session holds its topic"
			if c.state().Released() {
				how = "woken from its release to end its turn"
			}
			stop := stopOf(c)
			if err := e.enqueue(ctx, tx, c.issue.Key, record.RoleMessage{Role: string(c.token), Body: workerClose,
				Close: true, Wake: true, Stop: &stop}); err != nil {
				return err
			}
			told++
		case c.live():
			how = "told to end its turn"
			stop := stopOf(c)
			if err := e.enqueue(ctx, tx, c.issue.Key, record.RoleMessage{Role: string(c.token), Body: workerClose, Close: true, Stop: &stop}); err != nil {
				return err
			}
			told++
		case c.state().Released() || underWay:
			how = "held at the ready of a start under way"
			if c.state().Released() {
				how = "held over its release at once"
			}
			if err := e.enqueue(ctx, tx, c.issue.Key, stopOf(c).Suspend(deadline, time.Time{})); err != nil {
				return err
			}
		default:
			continue
		}
		stopped++
		e.logOnCommit(ctx, "workflow: the work hold stops a phase worker of the tree", "issue", c.issue.Key, "role", c.role,
			"version", version, "tree", root.Tree, "step", step, "how", how)
	}
	e.logOnCommit(ctx, "workflow: the work hold holds the tree's work", "tree", root.Tree, "step", step, "document", document,
		"version", version, "told", told, "stopped", stopped)
	return nil
}

// workHoldOpen is the work hold's end at the gate's move to open under `work`: a person's approval
// of version of step's document, Dispatch's carried approval after an exact undo, or a basis that
// opened the last closed step on an ancestor's approval. Each live architect and phase worker of
// the tree is told today's open message (gateOpenedMessage, which names each step of onBasis with
// the ancestor's approval it stands on and never as a person's approval of the root's document),
// a worker whose stop the outbox has not made among them, since that stop is not made now, as
// openingDelivery decides for a holder; then the hold ends (tellAndEndWorkHold).
func (e *Engine) workHoldOpen(ctx context.Context, tx pgx.Tx, root record.Issue, step record.DesignStep, document string, version int,
	onBasis []record.DesignGate) error {
	fallback := record.WorkHold{Project: root.Project, Tree: root.Tree, Step: step, ArtifactID: document, Version: version}
	return e.tellAndEndWorkHold(ctx, tx, root, gateOpenedMessage(root.Tree, step, document, version, onBasis), &gateOpening{step: step, version: version},
		fallback, "workflow: the design gate opened and the work hold ends", "tree", root.Tree, "step", step, "document", document, "version", version)
}

// gateOpening is the approval that opened a tree's standing gate under `work`: the step of the
// document approved and its version, which the opening decides each holder's opening by
// (openingDelivery).
type gateOpening struct {
	step    record.DesignStep
	version int
}

// tellAndEndWorkHold tells every live architect and phase worker of root's tree ended, what ended
// its work hold, logs line with args and the number told, and ends the hold (endWorkHold): the
// opening's end of it (workHoldOpen) and the boot's end of a hold the deployment no longer holds
// (workHoldBooted). fallback is the hold the end names where the tree holds no work_holds row.
//
// At the gate's opening (opened, nil at the boot's end, which opens no gate) each holder is told
// the opening once, as under `merge` (heldOpening): a holder the hold did not stop by
// openingDelivery, live or not, a claim the hold stopped by the task that starts it, and a child's
// architect that holds a member owed news by that news (settleChildArchitectOpenings). Each
// architect whose member stands in its owner's acceptance is told that the acceptance is its own
// (tellOwnersAcceptance), which nothing told it while the hold stood.
//
// It takes the tree's work_holds row for update before it reads the tree's claims
// (record.Store.LockWorkHold), and the transaction deletes the row, keeps it as the record of the
// tree's last opening (record.Store.OpenWorkHold) and commits with that lock held. A claim's
// decision that may make the hold's stop reads the same row for share in a transaction of its own
// (supervise.Store.WorkHoldStands, supervise's readHoldForStop). So a decision that asks for the row
// while this holds it waits for the commit and then reads no row, and makes no stop, and this
// waits for no decision's runtime call. A stop can still be written after this read of the claims:
// the stop of a decision that read the row before this took it, and a stop held for a turn, a
// compaction or a ready whose decision's read failed. Once a stop is written, the claim's machine
// reads the row again (supervise's holdReadAfterStop), and that read waits for this commit. Where
// it finds the row gone, the daemon applies intake.WorkHoldOpened, which starts the claim with the
// task this opening gave (workHoldOpened).
func (e *Engine) tellAndEndWorkHold(ctx context.Context, tx pgx.Tx, root record.Issue, ended string, opened *gateOpening,
	fallback record.WorkHold, line string, args ...any) error {
	locked, err := e.store.LockWorkHold(ctx, tx, root.Tree)
	if err != nil {
		return err
	}
	closing := fallback
	if locked != nil {
		closing = *locked
	}
	claims, err := e.treeClaims(ctx, tx, root)
	if err != nil {
		return err
	}
	var opening *heldOpening
	if opened != nil {
		if opening, err = e.readHeldOpening(ctx, tx, root, ended, *opened); err != nil {
			return err
		}
	}
	told := 0
	for _, c := range claims {
		body := ended
		if opening != nil {
			if body, err = e.heldOpeningMessage(ctx, tx, opening, c, root); err != nil {
				return err
			}
		}
		if body == "" || !c.live() {
			continue
		}
		if err := e.enqueue(ctx, tx, c.issue.Key, record.RoleMessage{Role: string(c.token), Body: body}); err != nil {
			return err
		}
		told++
	}
	e.logOnCommit(ctx, line, append(args, "told", told)...)
	if opening != nil && opening.byNews > 0 {
		e.logOnCommit(ctx, "workflow: the opening is owed to each holder owed news, and the task carrying their news tells it",
			"tree", root.Tree, "step", string(opened.step), "version", opened.version, "holders", opening.byNews)
	}
	if err := e.tellOwnersAcceptance(ctx, tx, root); err != nil {
		return err
	}
	return e.endWorkHold(ctx, tx, root, claims, ended, closing, locked != nil)
}

// tellOwnersAcceptance tells, at the end of the work hold of root's tree, the architect of each
// member that stands in its owner's acceptance, in the workflow, that the acceptance is its own:
// it queues again the phase-finished notice that moved the member there (ownersAcceptanceNotice),
// which the outbox routes to the architect that owns the member now. While the hold stood, no
// notice and no catch-up asked an architect for its own acceptance (TellsOwnersAcceptance,
// QueueCatchUp), so an architect launched again during the hold with a session that lost what it
// read before would otherwise learn of it from nothing. A member with a notice of its own
// acceptance still waiting in the outbox (TellsOwnersAcceptance: a phase-finished notice, or the
// notice of its return out of awaiting_children into accepting) is told by that notice, which goes
// once the gate is open, and gets no second. An architect that knew already reads the notice again,
// which tells it nothing new.
func (e *Engine) tellOwnersAcceptance(ctx context.Context, tx pgx.Tx, root record.Issue) error {
	members, err := e.store.TreeIssues(ctx, tx, root.Tree)
	if err != nil {
		return err
	}
	byKey := record.TreeByKey(members)
	accepting := slices.DeleteFunc(members, func(member record.Issue) bool {
		return member.Phase != phase.Accepting || !record.InWorkflow(member)
	})
	if len(accepting) == 0 {
		return nil
	}
	waiting, err := e.store.EarlierNotices(ctx, tx, root.Tree, math.MaxInt64)
	if err != nil {
		return err
	}
	told := map[string]bool{}
	for _, row := range waiting {
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			continue
		}
		if notice, ok := payload.(record.Notice); ok && TellsOwnersAcceptance(byKey[row.Issue], notice) {
			told[row.Issue] = true
		}
	}
	for _, member := range accepting {
		if told[member.Key] {
			continue
		}
		notice, err := e.ownersAcceptanceNotice(ctx, tx, member)
		if err != nil {
			return err
		}
		if err := e.notice(ctx, tx, member.Key, notice); err != nil {
			return err
		}
		e.logOnCommit(ctx, "workflow: the work hold's end tells the architect of a member in its owner's acceptance that the acceptance is its own",
			"issue", member.Key, "tree", root.Tree, "role", notice.Role)
	}
	return nil
}

// ownersAcceptanceNotice is the phase-finished notice that moved issue, standing in its owner's
// acceptance, there: the notice of the step before the acceptance on its walk (acceptedRoleOn, the
// review on today's line), with the handoff that step's phases row keeps, as the move wrote it
// (noticeFor).
func (e *Engine) ownersAcceptanceNotice(ctx context.Context, tx pgx.Tx, issue record.Issue) (record.Notice, error) {
	w, err := e.walk(ctx, tx, issue)
	if err != nil {
		return record.Notice{}, err
	}
	role, err := acceptedRoleOn(issue, w)
	if err != nil {
		return record.Notice{}, err
	}
	row, err := e.phaseRow(ctx, tx, issue.Key, role)
	if err != nil {
		return record.Notice{}, err
	}
	return record.Notice{Kind: "phase-finished", Role: role, Phase: builtinDeployment.Roles[string(role)].Phase,
		Summary: row.Summary, Verdict: row.Verdict}, nil
}

// heldOpening is what the work hold's opening decides each claim's opening by: the opened
// sentence and the approval (gateOpening), what the tree owes of its approved design as the
// opening leaves the record (record.OwedNews) with its told rows, the issue of each child's
// architect whose news carries the opening (settleChildArchitectOpenings), and how many holders
// the news tells it (byNews).
type heldOpening struct {
	gateOpening
	opened string
	owed   record.TreeNews
	told   []record.DesignTold
	owners map[string]bool
	byNews int
}

// readHeldOpening reads heldOpening of root's tree in tx, after the write that opened the gate,
// and settles the opening of each member in accepting that a child's architect holds and that is
// owed news (settleChildArchitectOpenings).
func (e *Engine) readHeldOpening(ctx context.Context, tx pgx.Tx, root record.Issue, opened string, at gateOpening) (*heldOpening, error) {
	facts, err := record.TreeNewsFacts(ctx, e.store, tx, e.cfg.Steps, root.Tree)
	if err != nil {
		return nil, err
	}
	members, err := e.store.TreeIssues(ctx, tx, root.Tree)
	if err != nil {
		return nil, err
	}
	opening := &heldOpening{gateOpening: at, opened: opened, owed: record.OwedNews(facts), told: facts.Told, owners: map[string]bool{}}
	owners, err := e.settleChildArchitectOpenings(ctx, tx, root, members, opening.owed, opening.told, opened, at.step, at.version)
	if err != nil {
		return nil, err
	}
	for _, owner := range owners {
		opening.owners[owner] = true
	}
	opening.byNews = len(owners)
	return opening, nil
}

// heldOpeningMessage is the message the work hold's opening sends claim c of root's tree, "" for
// none, the caller sending nothing to a claim that is not live:
//
//   - a holder the hold did not stop, the root's architect or the role that holds its member's
//     phase, is told as openingDelivery decides, as tellGateMoved decides for every holder under
//     `merge`. Owed news, it is sent nothing and its told row owes the opening, which the task
//     carrying its news leads with, whether its claim is live or not: a released claim, a claim
//     that failed, retired or that an operator suspended, and a role with no claim among them.
//     A live holder owed no news is sent the opened message, and its row is settled as told. A
//     holder that is not live and owed no news is sent nothing, and its row stays as the close
//     left it;
//   - a child's architect whose news carries the opening is sent nothing
//     (settleChildArchitectOpenings);
//   - any other live claim, a child's architect or a worker of a phase its issue has left, is sent
//     the opened message, as it was sent the close.
//
// A holder the hold stopped is told the opening by the one task that starts it, which settles its
// row (openHeldClaims).
func (e *Engine) heldOpeningMessage(ctx context.Context, tx pgx.Tx, opening *heldOpening, c treeClaim, root record.Issue) (string, error) {
	audience, holder := record.AudienceHolder, c.role != claim.RoleArchitect && RoleFor(c.issue.Phase) == c.role
	if c.role == claim.RoleArchitect && c.issue.Key == root.Tree {
		audience, holder = record.AudienceArchitect, true
	}
	switch {
	case holder && !c.state().WorkHeld():
		way := openingDelivery(opening.owed, opening.told, c.issue.Key, audience, opening.opened, opening.step, opening.version)
		if !way.byNews && !c.live() {
			return "", nil
		}
		if err := e.store.SettleOpening(ctx, tx, c.issue.Key, audience, way.byNews); err != nil {
			return "", err
		}
		if way.byNews {
			opening.byNews++
			return "", nil
		}
		return way.message, nil
	case c.role == claim.RoleArchitect && opening.owners[c.issue.Key]:
		return "", nil
	}
	return opening.opened, nil
}

// endWorkHold ends root's work hold at the engine's now: the tree's work_holds row goes, and the
// record keeps it, with ended, what ended the hold, as the record of the tree's last opening
// (record.Store.OpenWorkHold). Each claim the hold stopped is started (openHeldClaims) with the
// task built from that record (heldClaimOpening), which is the opening's sentences around ended.
// closing is the hold its caller read with the row locked (tellAndEndWorkHold), which names the
// version that closed the gate, or the caller's fallback where the row is gone already (stood is
// false). A fallback is no row, so the record keeps no opening of it. The start then builds the
// sentences of a hold no record keeps, which tell no opening of the gate (HeldClaimOpening), so
// the opening settles no holder row as told (openHeldClaims' told).
func (e *Engine) endWorkHold(ctx context.Context, tx pgx.Tx, root record.Issue, claims []treeClaim, ended string,
	closing record.WorkHold, stood bool) error {
	at := e.now()
	if err := e.store.OpenWorkHold(ctx, tx, root.Tree, at, ended); err != nil {
		return err
	}
	closing.OpenedAt, closing.Opening = at, ended
	return e.openHeldClaims(ctx, tx, root, claims, heldClaimOpening(&closing, root.Tree), closing.Version,
		stood && tellsOpening(&closing, root))
}

// tellsOpening is whether the opening's sentences built from opened, the record of root's tree's
// last opening (heldClaimOpening), tell the gate's opening: the record names what ended the hold,
// and that is today's open message, not the boot's sentence (bootOpenedSentence) of a gate that did
// not open. With no record, the sentences say only that the hold ended.
func tellsOpening(opened *record.WorkHold, root record.Issue) bool {
	return opened != nil && opened.Opening != "" && opened.Opening != bootOpenedSentence(root.Tree)
}

// openHeldClaims starts each claim of claims the hold stopped (stored suspended by hold), and no
// other, with one task that starts with opening, never a second. A claim that holds its issue's
// unfinished phase is given its phase's work built as the boot builds it (startSuspendedOwner):
// the work of the entry its phases row names where the claim stands behind that entry — a start
// written for the role while the gate was closed, a backward move's or an owed entry's, whose row
// this start is written in place of, so the role gets one task, the newest entry's
// (Store.EnqueueStart) — and the phase's resume task where it does not. A claim that holds no
// phase is told that the issue's other roles may ask it questions. The start carries opening apart
// (record.SuperviseRequest.Opening): where the claim kept a task of a run of its own, supervise
// puts those sentences in front of it instead. Where the claim kept an operator's task, the claim
// takes that task first, and this start's task follows it (Machine.EndHold). A claim with a queued
// start of the opening that stands over the start this would write (OpeningStartStands) is given
// no second: the queued start builds its words from the tree's record when it runs
// (internal/daemon's startOfAHeldClaim, HeldClaimOpening), so it gives this opening's words. A
// queued start that carries no phase work does not stand over the start of a claim that holds its
// phase now. Each start is one log line naming the issue, the role and version, the version that
// closed the gate, which is 0 where no record or stop names it.
//
// Where opening tells the gate's opening (told, tellsOpening), the holder row of each such claim
// that holds its member's phase is settled as told (record.Store.SettleOpening): the one task tells
// the opening, so the news its start carries leads with no second gate sentence, and no later task
// of news tells it again (openingDelivery's one delivery of the opening per holder). That holds
// for a claim the opening read live and whose stop was made after it too, whose row the opening
// left owing the opening (workHoldOpened).
func (e *Engine) openHeldClaims(ctx context.Context, tx pgx.Tx, root record.Issue, claims []treeClaim, opening string, version int, told bool) error {
	for _, c := range claims {
		if c.role == claim.RoleArchitect || !c.state().WorkHeld() {
			continue
		}
		holds := phase.RoleFor(c.issue.Phase) == c.role
		if told && holds {
			if err := e.store.SettleOpening(ctx, tx, c.issue.Key, record.AudienceHolder, false); err != nil {
				return err
			}
		}
		if holds {
			finished, err := e.phaseFinished(ctx, tx, c.issue)
			if err != nil {
				return err
			}
			holds = !finished
		}
		var current phase.Phase
		if holds {
			current = c.issue.Phase
		}
		if OpeningStartStands(c.run, root, current, 0) {
			continue
		}
		var err error
		switch {
		case holds:
			err = e.startHeldPhase(ctx, tx, c, opening)
		default:
			err = e.enqueue(ctx, tx, c.issue.Key, record.SuperviseRequest{Op: "start", Tree: c.issue.Tree, Role: c.role,
				Generation: c.issue.Generation, Task: supervise.OpeningTask(opening, noPhaseWork), Opening: opening})
		}
		if err != nil {
			return err
		}
		e.logOnCommit(ctx, "workflow: the work hold's opening starts a claim it stopped", "issue", c.issue.Key, "role", c.role,
			"version", version, "tree", root.Tree, "phase", holds)
	}
	return nil
}

// startHeldPhase is the opening's start of c, a claim the hold stopped that holds its issue's
// phase: the work of the entry its phases row names where the claim stands behind it, and the
// phase's resume task where it does not, each behind opening.
func (e *Engine) startHeldPhase(ctx context.Context, tx pgx.Tx, c treeClaim, opening string) error {
	row, err := e.phaseRow(ctx, tx, c.issue.Key, c.role)
	if err != nil {
		return err
	}
	if GivenEntry(c.run) < row.Entry {
		return e.sendOwedEntry(ctx, tx, c.issue, c.role, row, startMarks{opening: opening})
	}
	resume, err := e.ResumeTask(ctx, tx, c.issue)
	if err != nil {
		return err
	}
	return e.started(ctx, tx, c.issue, c.role, resume, 0, startMarks{resumeTask: true, reasserts: true, opening: opening})
}

// workHoldBooted is the boot's reconcile of the work hold over the project's running trees
// (booted; owners is what the boot read, suspendedPhaseOwners). A lingering tree is left alone,
// since its linger suspended every claim of it, and so is a root still waiting for its tree to be
// opened. For each running tree:
//
//   - a hold that stands of an older generation of the root is dropped, with the record of the
//     tree's last opening: the hold of a run that ended holds no newer one. A record of an opening
//     of an older generation with no hold that stands is not dropped here, and no start uses it
//     (openStoppedClaims);
//   - a tree that holds work (the hold is `work` and the tree's standing gate stands closed,
//     record.TreeGate.Standing) with no hold that stands is closed now: the deployment moved from
//     `merge` to `work` while its gate stood closed;
//   - a tree that holds work with its hold that stands starts nothing: its drain is under way,
//     and every stop stands as its row stored it;
//   - a tree that holds no work with its hold that stands has its hold ended: the deployment
//     moved to `merge` or turned the gate off. Every live holder is told so, and each claim the
//     hold stopped starts with its task;
//   - and a tree that holds neither starts each claim the hold stopped that no start covers yet,
//     with no message (openStoppedClaims with 0): the task the tree's last opening gave, or the
//     task that says the daemon keeps no record of the hold where the record of that opening is
//     gone or is of an older generation of the root. The log line of each start names the
//     version that record names, else 0.
//
// It reads no gate of its own: the gate of each running tree is the one the boot's read holds
// (phaseOwners.gates), which nothing the boot writes before this moves, and under `merge` no tree
// holds work whatever its gate. The trees whose claims a tree that holds neither may start are
// read once for the project (Store.WorkHeldTrees), so a tree with no claim the hold stopped reads
// nothing more.
func (e *Engine) workHoldBooted(ctx context.Context, tx pgx.Tx, owners phaseOwners) error {
	recorded, err := e.store.WorkHolds(ctx, tx, e.cfg.Project)
	if err != nil {
		return err
	}
	holds := make(map[string]record.WorkHold, len(recorded))
	for _, hold := range recorded {
		holds[hold.Tree] = hold
	}
	stopped, err := e.store.WorkHeldTrees(ctx, tx, e.cfg.Project)
	if err != nil {
		return err
	}
	for _, root := range owners.issues {
		if _, runs := owners.running[root.Key]; !runs {
			continue
		}
		hold, held := holds[root.Key]
		if held && hold.RootGeneration != root.Generation {
			if err := e.store.DeleteWorkHold(ctx, tx, root.Key); err != nil {
				return err
			}
			e.logOnCommit(ctx, "workflow: the boot drops the work hold of a generation of the tree that has ended", "tree", root.Key,
				"held", hold.RootGeneration, "generation", root.Generation)
			held = false
		}
		closed, stands := owners.gates[root.Key].Standing()
		holdsWork := stands && e.holdIsWork()
		switch {
		case holdsWork && !held:
			e.logOnCommit(ctx, "workflow: the boot applies the work hold to a tree whose standing gate stands closed", "tree", root.Key,
				"step", closed.Step.OrSpec(), "version", closed.LatestVersion)
			if err := e.workHoldClose(ctx, tx, root, closed.Step.OrSpec(), closed.ArtifactID, closed.LatestVersion); err != nil {
				return err
			}
		case holdsWork:
		case held:
			if err := e.tellAndEndWorkHold(ctx, tx, root, bootOpenedSentence(root.Key), nil, hold,
				"workflow: the boot ends the work hold of a tree the deployment no longer holds", "tree", root.Key, "hold", e.cfg.Hold); err != nil {
				return err
			}
		default:
			if _, stops := stopped[root.Key]; !stops {
				continue
			}
			if err := e.openStoppedClaims(ctx, tx, root, 0); err != nil {
				return err
			}
		}
	}
	return nil
}

// workHoldOpened is the work hold of a tree found ended under a claim it stopped
// (intake.WorkHoldOpened): the opening committed and may have started nothing of the claim
// (openHeldClaims starts only a claim stored stopped by the hold), and the claim stands stopped by
// the hold. A decision lets its lock on the tree's work hold row go as soon as it has read the
// row, so the opening can read the claim live and commit before the stop is written
// (tellAndEndWorkHold, supervise's readHoldForStop). A held stop made after a read that failed can
// follow the opening too. Once the stop is written, the claim's machine reads the row again, and
// where it finds the row gone, the daemon applies this fact at once (supervise's
// holdReadAfterStop, the daemon's workHoldEndedUnderStop). The daemon applies it at once too where
// the stop's outbox row asks again and its decision finds the row gone and the claim stopped
// (supervise's readHoldForStop). The claim is started now with the task the opening gave, as the
// boot starts a claim the hold stopped in a tree that holds no work (openStoppedClaims). The log
// line of its start names the version the record of the opening names, else the version the
// hold's stop named (intake.WorkHoldOpened.Version). A claim an opening start covers is not
// started again, so an opening that read the claim stopped and this fact never start it twice. A
// tree that lingers and a root the record does not hold are left alone, and so is a tree whose
// record shows a hold that stands again (openStoppedClaims), which reads that record in this
// transaction and no gate.
func (e *Engine) workHoldOpened(ctx context.Context, tx pgx.Tx, fact intake.WorkHoldOpened) (intake.Result, error) {
	root, err := e.store.Issue(ctx, tx, fact.Tree)
	if err != nil || root == nil || !claim.IsTreeRoot(root.Key, root.Tree) || root.Lingers() {
		return intake.Result{}, err
	}
	return intake.Result{}, e.openStoppedClaims(ctx, tx, *root, fact.Version)
}

// openStoppedClaims starts each claim of root's tree the hold stopped that no opening start
// covers yet (openHeldClaims), with no message, and with the task the tree's last opening gave:
// it reads the tree's record of its hold (record.Store.TreeWorkHold) in this transaction and
// builds the task from it (heldClaimOpening). A record of an older generation of the root is the
// hold of a run that ended, so it is not used, and one line says so. A hold that stands there
// means the hold stands again, so it starts nothing and logs one line. Where the record keeps no
// hold of the tree it can use, the task says that the daemon keeps no record of the version that
// closed the gate. The log line of each start names the version the record names, else version:
// the version the hold's stop named (workHoldOpened), or 0 from the boot (workHoldBooted, a tree
// that holds neither).
func (e *Engine) openStoppedClaims(ctx context.Context, tx pgx.Tx, root record.Issue, version int) error {
	hold, err := e.store.TreeWorkHold(ctx, tx, root.Tree)
	if err != nil {
		return err
	}
	if hold != nil && lastOpening(hold, root) == nil {
		e.logOnCommit(ctx, "workflow: the record of the work hold is of a generation of the tree that has ended; the start of a claim it stopped names no version in its task",
			"tree", root.Tree, "held", hold.RootGeneration, "generation", root.Generation)
		hold = nil
	}
	if hold != nil && hold.Standing() {
		e.logOnCommit(ctx, "workflow: the work hold holds the tree again; a claim it stopped waits for the next opening", "tree", root.Tree,
			"step", hold.Step, "version", hold.Version)
		return nil
	}
	if hold != nil {
		version = hold.Version
	}
	claims, err := e.treeClaims(ctx, tx, root)
	if err != nil {
		return err
	}
	return e.openHeldClaims(ctx, tx, root, claims, heldClaimOpening(hold, root.Tree), version, tellsOpening(hold, root))
}
