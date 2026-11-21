package workflow

import (
	"context"
	"fmt"
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
// (record.TreeGate.HoldsWork, over the standing gate). The record keeps the hold of each tree
// (record.WorkHold): the close writes it, the opening deletes it, and the boot reads it to tell a
// tree whose close was never applied from one whose drain is under way (workHoldBooted).

// HoldStopGrace is how long after the work hold's close message reached a claim a turn still
// running is cut: long enough for a worker to end its turn at its next step and for a merger to
// take its pull request out of the merge queue. The outbox names each stop's deadline that far
// after the message reached the claim (internal/daemon's postHoldStop), and the close message
// names it to the worker (workHoldSentence).
const HoldStopGrace = 2 * time.Minute

// The sentences of the hold's texts beside today's gate messages (gateClosedSentence,
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
	// architect, whom the hold does not stop.
	workerCloseEnd    = "End your turn now."
	architectCloseEnd = "Your own work goes on."
	// holdStoppedSentence and checkTurnSentence frame today's open message in the opening task.
	holdStoppedSentence = "The work hold stopped your turn."
	checkTurnSentence   = "Check what your turn already did: a tool call may not have finished."
	// noPhaseWork is the work of an opening task to a claim that holds no phase of its issue.
	noPhaseWork = "The issue's other roles may ask you questions."
)

// gateClosedSentence is today's first sentence of the close message: the tree whose gate closed,
// and the version, the step and the document that closed it. The step and its own document, never
// "spec version": under more than one gated step the version is that step's document's, and a
// message naming the spec would send a reader to the wrong one.
func gateClosedSentence(tree string, step record.DesignStep, document string, version int) string {
	return fmt.Sprintf("The design gate of %s closed: version %d of its %s document (%s) is with its reviewer, "+
		"so the design you are working to may change.", tree, version, step, document)
}

// mergeCloseMessage is today's close message, which `merge` sends word for word.
func mergeCloseMessage(tree string, step record.DesignStep, document string, version int) string {
	return gateClosedSentence(tree, step, document, version) +
		" Your phase goes on; the merge waits until every gated step stands open again."
}

// gateOpenedMessage is today's open message, which both holds send at the opening.
func gateOpenedMessage(tree string, step record.DesignStep, document string, version int) string {
	return fmt.Sprintf("The design gate of %s opened: a person approved version %d of its %s document (%s), "+
		"and every step the deployment gates now stands open, so the design you are working to is the approved one. "+
		"A merge the closed gate held is released.", tree, version, step, document)
}

// workCloseMessage is the close message under `work`: today's first sentence, the hold's, and the
// ending of the recipient, an architect or a phase worker.
func workCloseMessage(hold record.WorkHold, architect bool) string {
	end := workerCloseEnd
	if architect {
		end = architectCloseEnd
	}
	return gateClosedSentence(hold.Tree, hold.Step, hold.ArtifactID, hold.Version) + " " + workHoldSentence + " " + end
}

// bootOpenedSentence is what the hold's end says where no person's approval is to be named: the
// boot's end of a tree's hold with its gate still where it stood, the deployment having moved to
// `merge` or turned the gate off, and the start of a claim the hold stopped that no opening
// covered (openStoppedClaims).
func bootOpenedSentence(tree string) string {
	return "The deployment no longer holds the work of " + tree + "'s tree."
}

// openingSentences is what the one task of the opening starts with: the close message's two
// sentences, which name the version that closed the gate, that the hold
// stopped the turn, what ended the hold (today's open message, or the boot's sentence), and the
// check of what the turn already did, since a cut turn can leave a tool call unfinished. A hold no
// record names the closing version of says the hold's own sentence alone in front.
func openingSentences(closing *record.WorkHold, ended string) string {
	opening := workHoldSentence
	if closing != nil {
		opening = gateClosedSentence(closing.Tree, closing.Step, closing.ArtifactID, closing.Version) + " " + opening
	}
	return opening + " " + holdStoppedSentence + " " + ended + " " + checkTurnSentence
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

// treeClaims reads every role of every member of root's tree that is in the workflow
// (record.InWorkflow), members by key and roles in claim.Roles's order, the architect first, with
// the role's queued rows and claim at the member's generation, in batches (RoleRuns). A member
// out of the workflow is left out, a closed one whose production check a role looks at among
// them: the hold holds no work of an issue that has left the workflow. A role whose token cannot
// be built is logged and left out, as tellGateMoved leaves such a holder untold.
func (e *Engine) treeClaims(ctx context.Context, tx pgx.Tx, root record.Issue) ([]treeClaim, error) {
	members, err := e.store.TreeIssues(ctx, tx, root.Tree)
	if err != nil {
		return nil, err
	}
	project, err := claim.ProjectToken(root.Project)
	if err != nil {
		return nil, err
	}
	var claims []treeClaim
	for _, member := range members {
		if !record.InWorkflow(member) {
			continue
		}
		for _, role := range claim.Roles {
			token, err := claim.NewToken(project, member.Key, role)
			if err != nil {
				e.logOnCommit(ctx, "workflow: the work hold cannot name this role, so it neither tells nor stops it",
					"tree", root.Tree, "issue", member.Key, "role", role, "error", err)
				continue
			}
			claims = append(claims, treeClaim{issue: member, role: role, token: token})
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
//     on.": the hold stops no architect;
//   - every live phase worker (planner, implementer, tester, reviewer, merger), idle claims of a
//     finished phase among them, the close and "End your turn now.", carrying its stop
//     (record.RoleMessage.Stop), which the outbox posts once the message reached the claim, with
//     the deadline HoldStopGrace on;
//   - a released merger the same message, marked to wake it (record.RoleMessage.Wake), so it can
//     take its pull request out of the merge queue;
//   - a released worker that is not a merger nothing: a wake would bring its pod back only to stop
//     it, so its stop is written at once, and the hold takes its `released` source over with its
//     bundle and its pending task kept;
//   - and a claim an operator or the daemon suspended, or a failed or retired one, nothing: the
//     operator's suspension and the daemon's stand above the hold's.
//
// Each stop is one log line naming the issue, the role and the version.
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
	stopOf := func(c treeClaim) record.SuperviseRequest {
		return record.SuperviseRequest{Op: "suspend", Tree: c.issue.Tree, Role: c.role, Generation: c.issue.Generation,
			SuspendedBy: supervise.SuspendedByHold, Reason: reason}
	}
	told, stopped := 0, 0
	for _, c := range claims {
		var how string
		switch {
		case c.role == claim.RoleArchitect && c.live():
			if err := e.enqueue(ctx, tx, c.issue.Key, record.RoleMessage{Role: string(c.token), Body: architectClose}); err != nil {
				return err
			}
			told++
			continue
		case c.role == claim.RoleArchitect:
			continue
		case c.live():
			how = "told to end its turn"
			stop := stopOf(c)
			if err := e.enqueue(ctx, tx, c.issue.Key, record.RoleMessage{Role: string(c.token), Body: workerClose, Stop: &stop}); err != nil {
				return err
			}
			told++
		case c.role == claim.RoleMerger && c.state().Released():
			how = "woken from its release to end its turn"
			stop := stopOf(c)
			if err := e.enqueue(ctx, tx, c.issue.Key, record.RoleMessage{Role: string(c.token), Body: workerClose,
				Wake: true, Stop: &stop}); err != nil {
				return err
			}
			told++
		case c.state().Released():
			how = "held over its release at once"
			if err := e.enqueue(ctx, tx, c.issue.Key, stopOf(c)); err != nil {
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
// of version of step's document, or Dispatch's carried approval after an exact undo. Every live
// architect and phase worker of the tree is told today's open message, a worker whose stop the
// outbox has not made among them, since that stop is not made now; then the hold ends
// (tellAndEndWorkHold).
func (e *Engine) workHoldOpen(ctx context.Context, tx pgx.Tx, root record.Issue, step record.DesignStep, document string, version int) error {
	fallback := &record.WorkHold{Project: root.Project, Tree: root.Tree, Step: step, ArtifactID: document, Version: version}
	return e.tellAndEndWorkHold(ctx, tx, root, gateOpenedMessage(root.Tree, step, document, version), fallback,
		"workflow: the design gate opened and the work hold ends", "tree", root.Tree, "step", step, "document", document, "version", version)
}

// tellAndEndWorkHold tells every live architect and phase worker of root's tree ended, what ended
// its work hold, logs line with args and the number told, and ends the hold (endWorkHold): the
// opening's end of it (workHoldOpen) and the boot's end of a hold the deployment no longer holds
// (workHoldBooted).
func (e *Engine) tellAndEndWorkHold(ctx context.Context, tx pgx.Tx, root record.Issue, ended string, fallback *record.WorkHold,
	line string, args ...any) error {
	claims, err := e.treeClaims(ctx, tx, root)
	if err != nil {
		return err
	}
	told := 0
	for _, c := range claims {
		if !c.live() {
			continue
		}
		if err := e.enqueue(ctx, tx, c.issue.Key, record.RoleMessage{Role: string(c.token), Body: ended}); err != nil {
			return err
		}
		told++
	}
	e.logOnCommit(ctx, line, append(args, "told", told)...)
	return e.endWorkHold(ctx, tx, root, claims, ended, fallback)
}

// endWorkHold ends root's work hold: the record goes, and each claim the hold stopped is started
// (openHeldClaims) with the opening's sentences around ended, what ended the hold. The record names
// the version that closed the gate; a tree whose record is gone already is named by fallback.
func (e *Engine) endWorkHold(ctx context.Context, tx pgx.Tx, root record.Issue, claims []treeClaim, ended string, fallback *record.WorkHold) error {
	closing, err := e.store.WorkHold(ctx, tx, root.Tree)
	if err != nil {
		return err
	}
	if closing == nil {
		closing = fallback
	}
	if err := e.store.DeleteWorkHold(ctx, tx, root.Tree); err != nil {
		return err
	}
	return e.openHeldClaims(ctx, tx, root, claims, openingSentences(closing, ended), closing)
}

// openHeldClaims starts each claim of claims the hold stopped (stored suspended by hold), and no
// other, with one task that starts with opening, never a second. A claim that holds its issue's
// unfinished phase is given its phase's work built as the boot builds it (startSuspendedOwner):
// the work of the entry its phases row names where the claim stands behind that entry — a start
// written for the role while the gate was closed, a backward move's or an owed entry's, whose row
// this start is written in place of, so the role gets one task, the newest entry's
// (Store.EnqueueStart) — and the phase's resume task where it does not. A claim that holds no
// phase is told that the issue's other roles may ask it questions. The start carries opening apart
// (record.SuperviseRequest.Opening): where the claim kept a task of its own, supervise puts those
// sentences in front of it instead (Machine.EndHold). A claim that already has such a start queued
// is given no second. Each start is one log line naming the issue, the role and the version that
// closed the gate.
func (e *Engine) openHeldClaims(ctx context.Context, tx pgx.Tx, root record.Issue, claims []treeClaim, opening string, closing *record.WorkHold) error {
	closedAt := 0
	if closing != nil {
		closedAt = closing.Version
	}
	for _, c := range claims {
		if c.role == claim.RoleArchitect || !c.state().WorkHeld() || openingQueued(c.run, root) {
			continue
		}
		holds := phase.RoleFor(c.issue.Phase) == c.role
		if holds {
			finished, err := e.phaseFinished(ctx, tx, c.issue)
			if err != nil {
				return err
			}
			holds = !finished
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
			"version", closedAt, "tree", root.Tree, "phase", holds)
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
	return e.started(ctx, tx, c.issue, c.role, ResumePhaseTask(c.issue), 0, startMarks{resumeTask: true, reasserts: true, opening: opening})
}

// openingQueued is whether run already has a start of the opening queued that still acts: one
// carrying the opening's sentences, newer than any stop that will still act (actingStop).
func openingQueued(run record.RoleRun, root record.Issue) bool {
	stop := actingStop(run, root)
	for _, queued := range run.Queued {
		if queued.Request.Op == "start" && queued.Request.Opening != "" && queued.ID > stop {
			return true
		}
	}
	return false
}

// workHoldBooted is the boot's reconcile of the work hold over the project's running trees
// (booted; owners is what the boot read, suspendedPhaseOwners). A lingering tree is left alone,
// since its linger suspended every claim of it, and so is a root still waiting for its tree to be
// opened. For each running tree:
//
//   - a record of an older generation of the root is dropped: the hold of a run that ended holds
//     no newer one;
//   - a tree that holds work (record.TreeGate.HoldsWork) with no record is closed now: the
//     deployment moved from `merge` to `work` while its gate stood closed;
//   - a tree that holds work with its record starts nothing: its drain is under way, and every
//     stop stands as its row stored it;
//   - a tree that holds no work with its record has its hold ended: the deployment moved to
//     `merge` or turned the gate off. Every live holder is told so, and each claim the hold
//     stopped starts with its task;
//   - and a tree that holds neither starts each claim the hold stopped that no start covers yet,
//     with no message.
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
		closed, holdsWork := owners.gates[root.Key].HoldsWork(e.holdIsWork())
		switch {
		case holdsWork && !held:
			e.logOnCommit(ctx, "workflow: the boot applies the work hold to a tree whose standing gate stands closed", "tree", root.Key,
				"step", closed.Step.OrSpec(), "version", closed.LatestVersion)
			if err := e.workHoldClose(ctx, tx, root, closed.Step.OrSpec(), closed.ArtifactID, closed.LatestVersion); err != nil {
				return err
			}
		case holdsWork:
		case held:
			if err := e.tellAndEndWorkHold(ctx, tx, root, bootOpenedSentence(root.Key), nil,
				"workflow: the boot ends the work hold of a tree the deployment no longer holds", "tree", root.Key, "hold", e.cfg.Hold); err != nil {
				return err
			}
		default:
			if _, stops := stopped[root.Key]; !stops {
				continue
			}
			if err := e.openStoppedClaims(ctx, tx, root); err != nil {
				return err
			}
		}
	}
	return nil
}

// workHoldOpened is the work hold of a tree found ended under a claim it stopped
// (intake.WorkHoldOpened): the outbox ran the hold's stop of a worker in a turn, the gate opened
// again while the stop was held for that turn, and the turn ended after the opening committed,
// which made the stop. The opening read the claim live and started nothing of it (openHeldClaims
// starts only a claim stored stopped by the hold), so the claim is started now, as the boot starts
// a claim the hold stopped in a tree that holds no work (openStoppedClaims): a stop not yet made
// when the gate opens again is not made, and one made since is undone. A tree that holds work
// again, one that lingers, and a root the record does not hold are left alone. Under `merge` no
// tree holds work, so the gate is read under `work` alone.
func (e *Engine) workHoldOpened(ctx context.Context, tx pgx.Tx, fact intake.WorkHoldOpened) (intake.Result, error) {
	root, err := e.store.Issue(ctx, tx, fact.Tree)
	if err != nil || root == nil || !claim.IsTreeRoot(root.Key, root.Tree) || root.Lingers() {
		return intake.Result{}, err
	}
	if e.holdIsWork() {
		gates, err := e.store.GatesOf(ctx, tx, root.Key)
		if err != nil {
			return intake.Result{}, err
		}
		if closed, holds := (record.TreeGate{Steps: e.cfg.Steps, Rows: gates}).HoldsWork(true); holds {
			e.logOnCommit(ctx, "workflow: the work hold holds the tree again; a claim it stopped waits for the next opening", "tree", root.Key,
				"step", closed.Step.OrSpec(), "version", closed.LatestVersion)
			return intake.Result{}, nil
		}
	}
	return intake.Result{}, e.openStoppedClaims(ctx, tx, *root)
}

// openStoppedClaims starts each claim of root's tree the hold stopped that no opening start
// covers yet (openHeldClaims), with no message: the tree holds no work and keeps no record of a
// hold, so the opening names the hold's own sentence in front, and that the deployment no longer
// holds the tree's work.
func (e *Engine) openStoppedClaims(ctx context.Context, tx pgx.Tx, root record.Issue) error {
	claims, err := e.treeClaims(ctx, tx, root)
	if err != nil {
		return err
	}
	return e.openHeldClaims(ctx, tx, root, claims, openingSentences(nil, bootOpenedSentence(root.Key)), nil)
}
