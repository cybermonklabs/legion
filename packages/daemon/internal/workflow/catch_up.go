package workflow

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// claimReady tells an architect at each launch the issues it owns. For a child's own architect it
// is the first instruction: a child's first phase start launches that architect with no task, and
// a re-entered child resumes its session. A tree's root architect is launched with the task its
// record derives (ArchitectStartTask, Engine.StartArchitect), which names the decisions kept while
// nobody in its tree could hear them (ParkedDecisionTask), or with none where the derivation gives
// none, and a re-admitted tree resumes its session. A retry that asks for a new session starts
// one (supervise's freshSession), as does any launch whose session no store holds any more
// (startFresh). So at each launch's ready the daemon sends it a catch-up notice of the issues it
// owns (QueueCatchUp) — a root architect its whole tree, a child's architect the child and every
// issue under it — and so does a start that finds the claim still running (the outbox's start), as
// the TypeScript daemon sends the overseer catch-up at every ready of an active root
// (onTreeReady). A relaunch in the same generation is told again: it may be an architect that died
// during the turn a catch-up started, resumed idle, or a fresh agent whose workspace was lost, and
// nothing else wakes either.
// The ready is one fact per launch (workflowRuntime.applyTerminal keys it by the claim's launch
// generation), so the boot's replay of a ready already applied tells nothing more, and one never
// applied tells it once. The catch-up a root architect's ready writes names every decision nobody
// else claims, so a decision a release left unclaimed reaches this architect here; one still
// claimed by a queued start or catch-up is that row's to settle. A child's names none: what a
// person decided while nobody could hear it is kept for the tree, and stays its root architect's.
// A tree that lingers or has left the workflow is told nothing, and so is the architect of an
// issue the daemon has not recorded — an operator's spawn of an issue no record of the project
// holds, the project's own or another project's, whose record the daemon's records do not read
// (internal/daemon's projectRecords), so its ready drops and queues no catch-up keyed by that
// issue — or of a child that has left the workflow or is done. A phase worker's ready is answered
// differently: a merge forward still owed to its role is asked again at a launch later than the one
// the ask was last sent at (askAgainAtLaunch), and it is given its phase's task again when it holds
// that phase with nothing pending (readyWorkerTask).
//
// The catch-up a start writes in place of a launch (intake.ClaimReady.Paced) is a wake of the
// architect — it reads its whole context when the catch-up arrives, which is what the per-minute
// cap counts — so it takes its turn in the daemon's pacing: it is written held, with no due
// moment (record.CatchUpHeldAt), and joins the pacing's queue once this fact commits
// (Config.HoldCatchUp), whose let-out makes it due (dispatch://INFRA-5682 R1). The join is the
// commit's hook rather than a step of the fact, so a fact that rolls back spends no turn, and a
// start row that runs again after a crash joins once: the fact applies once for the row that
// wrote it. A launch's own ready is held to nothing, since the launch took its turn at the
// machine's gate (supervise's reserveWake); nor is any ready while the pacing is off.
//
// A launch's ready that carries the sentence of what the last let-go of the architect's process
// ended (intake.ClaimReady.Relaunch) has its catch-up open with it (QueueCatchUp): the first turn
// of a relaunched architect can be this catch-up's rather than its task's, and the architect then
// reads that its subagent died before it reads its issues. A take of a task the claim holds tells
// the same record and is what marks it told, so a catch-up that is held,
// superseded or never queued here costs nothing (dispatch://INFRA-3276 R3).
func (e *Engine) claimReady(ctx context.Context, tx pgx.Tx, fact intake.ClaimReady) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	if fact.Role != claim.RoleArchitect {
		if err := e.askAgainAtLaunch(ctx, tx, *issue, fact); err != nil {
			return intake.Result{}, err
		}
		return intake.Result{}, e.readyWorkerTask(ctx, tx, *issue, fact)
	}
	if claim.IsTreeRoot(issue.Key, issue.Tree) {
		if issue.Lingers() || record.OutOfWorkflow(issue.Status) {
			return intake.Result{}, nil
		}
	} else {
		// A child's own record does not say whether its tree lingers: the close arms the linger on
		// the root alone (record.TreeLingers).
		if !record.InWorkflow(*issue) {
			return intake.Result{}, nil
		}
		if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
			return intake.Result{}, err
		}
	}
	var hold func(id int64, issue string)
	if fact.Paced {
		hold = e.cfg.HoldCatchUp
	}
	return intake.Result{}, queueCatchUp(ctx, tx, e.store, e.cfg.Steps, *issue, fact.Launch, fact.Relaunch, e.now(), hold)
}

// QueueCatchUp is the one composer of an architect's catch-up, which both the ready (claimReady)
// and a hand-over that finds a root with an architect to tell (HandOverKept) write through, so the
// two never compose one differently. It states what issue's architect owns as the record holds it
// (treeCatchUp) — a root's whole tree, a child's own issue and every issue under it — for launch,
// the launch it was written for, so a failed forward of an older one is not queued again behind it
// (the outbox's rehold). The row is queued on issue's own key, and the outbox publishes it to
// issue's own architect and to no other (the daemon's noticeArchitect): a child's catch-up that
// reached the architect above it would be read there as that architect's own state.
//
// relaunch is the sentence of what the last let-go of the architect's process ended, which a
// launch's ready carries while the claim's record of it is untold (intake.ClaimReady.Relaunch),
// and "" from every other caller. The notice's reason — the text the architect reads of the
// catch-up — opens with it, so the architect reads what died with its last process before it
// reads its issues. The copy tells nothing: the record is the claim's, and it is told at the
// accepted take of a task that tells the same record (supervise's accept, Delivery.message),
// which the outbox's publish of this notice does not see.
//
// A catch-up an earlier launch never had delivered is dropped first (DropCatchUps, at the caller's
// now, which is the clock a lane's lease is written from), so the agent is given one catch-up,
// computed now. A root's names again the decisions the dropped one named — the drop releases them
// in this transaction — with any kept since. A catch-up that carries kept decisions is not dropped
// while any row of its telling — itself, the row it copies, or a re-held copy of either — is under
// a live lease: that row may be publishing now, and the decisions the catch-up claims are that
// telling's to settle, so this one names only what nobody claims. A child's names no decision: what
// a person decided while nobody could hear it is kept for the tree (ParkedDecisionTask keys it by
// tree) and stays the root architect's, so a child's catch-up carries no Decided and no Kept. The
// notice goes through the outbox like any other, so it is held and re-held as they are.
//
// The catch-up is due now. The one the wake pacing holds is the paced ready's alone (claimReady,
// queueCatchUp): every other caller writes one no pacing holds (CatchUpTheTreesOwed for
// HandOverKept).
func QueueCatchUp(ctx context.Context, tx pgx.Tx, store record.Store, steps []record.DesignStep, issue record.Issue, launch uint64, relaunch string, now time.Time) error {
	return queueCatchUp(ctx, tx, store, steps, issue, launch, relaunch, now, nil)
}

// queueCatchUp is QueueCatchUp, and with a hold it writes the catch-up held instead: the row's
// next_at and the moment the notice carries are record.CatchUpHeldAt, which is no due moment,
// with now beside it as the moment the hold began (record.Notice.PacedAt, PacedFrom), and hold is
// called with the row's id once the transaction commits, which is where the daemon joins the row
// to its wake pacing's queue (Config.HoldCatchUp). The row keeps the moment of its own because a
// failed attempt rewrites next_at to its backoff: the moment is what keeps a ready from pulling
// the row forward before its turn (the daemon's releaseWaiting), and with the moment the hold
// began it is the wait in the line that wake writes when it runs (the daemon's wakeRan). Neither
// is written on a catch-up no pacing holds, which reports no wait, having had none.
func queueCatchUp(ctx context.Context, tx pgx.Tx, records record.Store, steps []record.DesignStep, issue record.Issue, launch uint64,
	relaunch string, now time.Time, hold func(id int64, issue string)) error {
	catchUp, waits, err := treeCatchUp(ctx, tx, records, steps, issue)
	if err != nil {
		return err
	}
	catchUp.Launch = launch
	if _, err := records.DropCatchUps(ctx, tx, issue.Key, now); err != nil {
		return err
	}
	var kept *record.KeptDecisions
	reason := fmt.Sprintf("the issues under %s, which you own, at generation %d as the daemon records them now; start or resume your issue as your role says", issue.Key, issue.Generation)
	if claim.IsTreeRoot(issue.Key, issue.Tree) {
		// A decision kept since the start's task was written (one a person made while the tree's
		// architect claim had ended, before this relaunch), and one a start or catch-up that
		// reached no architect handed over, are told here. The catch-up names only what no other
		// start or catch-up claims (a start still queued keeps its own), and claims what it names
		// in this transaction; its publish forgets those rows (the outbox's notice), and a
		// catch-up a newer one supersedes hands them to a fresh catch-up (HandOverKept).
		var decided string
		if decided, kept, err = ParkedDecisionTask(ctx, tx, records, steps, issue); err != nil {
			return err
		}
		catchUp.Decided, catchUp.Kept = decided, kept
		reason = fmt.Sprintf("the tree of %s at generation %d as the daemon records it now; start or resume it as your role says", issue.Key, issue.Generation)
	}
	if len(waits.accepting) > 0 {
		reason += fmt.Sprintf("; in accepting, waiting for your acceptance, which no worker gives: %s (read the evidence, then sign_off or request_backward_move)", strings.Join(waits.accepting, ", "))
	}
	reason += strings.Join(waits.resting, "")
	if relaunch != "" {
		reason = relaunch + "\n\n" + reason
	}
	notice := record.Notice{Kind: "catch-up", Role: claim.RoleArchitect, Reason: reason, CatchUp: &catchUp}
	if hold == nil {
		return EnqueueNaming(ctx, tx, records, issue.Key, issue.Tree, notice, kept, now)
	}
	notice.PacedAt, notice.PacedFrom = record.CatchUpHeldAt, now
	id, err := enqueueNamingID(ctx, tx, records, issue.Key, issue.Tree, notice, kept, record.CatchUpHeldAt)
	if err != nil {
		return err
	}
	store.OnCommit(ctx, func() { hold(id, issue.Key) })
	return nil
}

// queuedRowCovers is whether a row the outbox already holds for this role covers its phase: a
// start of the phase that will still act, which carries the phase's task itself, or a stop that
// will still act, which is about to end the claim this ready came from. Both are StartFor's rule
// about queued rows (actingStop, queuedStartActs), read without its question about the claim's own
// liveness: this claim is live, which is precisely why it is being given its task. A start queued
// for news alone carries no task of the phase (startsWork), so it covers nothing, and the worker is
// given its task beside it.
func queuedRowCovers(run record.RoleRun, root record.Issue, current phase.Phase) bool {
	stop := actingStop(run, root)
	return stop != 0 || queuedStartActs(run, stop, current)
}

// readyWorkerTask gives a phase worker whose claim has just become ready its phase's task again,
// when it holds that phase and has nothing pending. Every start of a worker is a start of work it
// must be told to do: the start row that moved the issue into this phase ran once, long before
// this launch, and after a restart — the worker's process relaunched, its claim retried, its
// session resumed — nothing else sends it anything. A tester that came back from a restart with no
// task waited, and its issue waited with it, for 301 hours across 48 waits
// (dispatch://INFRA-714 R7).
//
// The conditions are the ones a resume by rule already uses (resume.go's booted), read for this
// one claim: the issue is recorded and in the workflow, its phase's role (RoleFor) is this claim's,
// its tree does not linger, and its phase's own terminating fact has not arrived (PhaseFinished) —
// a merged root stands in merging for as long as the rest of its tree runs, and its merger must
// not be handed merging again at every launch. A claim of another project's issue finds no record
// here, as a claim of an issue nobody records finds none: the daemon's store reads no record of
// another project (internal/daemon's projectRecords). A claim that holds a task is left alone: it
// has work, and a second task would be a second prompt for work under way. So is a role a queued
// row already covers (queuedRowCovers).
//
// A phase worker relaunched by an update round's replacement of its pod is given nothing at this
// ready where it answered the round inside the turn of the take of its task: that turn's report
// finished the task in the session the worker still runs, though its phase was not complete
// (supervise's take), and the round's end sends only what is still pending. Its session goes on,
// and the session's own resume reminder wakes it while its phase is open (finishedInItsSession,
// below): its phase's task again would be the re-send that dispatch://INFRA-2811 R4 removes. A
// worker whose phase is over is given nothing either: the issue has left the phase for one its
// role does not work (RoleFor), or the phase's terminating fact has come (PhaseFinished). The
// ready does not read a phase row's completion (record.PhaseRow.CompletedAt), which the silence
// repair reads (PhaseWorkerOwedTurn): a merger that the boot relaunches with a READY completion
// still on its row may still be owed its merging task (cutoverMergerRepair).
//
// The daemon's own delivery of a relaunch's record is no task (supervise.HoldsTask), so a claim
// that holds that alone is read as holding nothing. The phase's task that this queues takes that
// delivery's place while no take of it is accepted, or once the accepted take's turn has run
// (supervise's queue), and carries the sentence in front of itself while the record is untold.
// A worker that came back holding only that delivery — its ready found it holding one from an
// earlier launch its agent never read — would otherwise be given no phase task at all. A task of
// news of the approved design alone is no work of the phase either (supervise.HoldsWork): it tells
// the worker what changed and nothing of what to do, so a claim that holds only that is given its
// phase's task too. An owed entry's task takes the news task's place while no turn has taken it,
// and carries that news itself (supervise's Machine.supersedes); the phase's resume task carries
// no entry, so it waits behind the news task until a turn has taken it.
//
// What such a claim is sent is decided by the entry its role's phases row names
// (record.PhaseRow.Entry), read here as admission reads it for the same decision (admit's
// owedEntryRoles): the whole row, below the skips above, so a ready this function passes over
// costs no query and the row the rule is weighed against is the row whose work is sent. A claim
// behind that entry — the entry it was given is under the row's and no queued start carries that
// entry (OwedEntry) — is sent the work of that entry at that entry (SendOwedEntry), which is the
// task the transition wrote when it raised the number. It is sent that rather than the phase's
// resume task because the resume says to carry the existing phase work on, and the work of an
// entry this claim was never given is work it does not have; the resume would also spend the turn
// on the wrong prompt, since the completion it answers with is refused as behind that entry and
// the entry's own task follows from the next admission pass or the entry sweep anyway.
//
// A claim level with its row is sent the phase's resume task, enqueued here, marked as a resume
// task and under a resume's origin, so the one rule about already-delivered work stays the
// outbox's: it does not deliver a resume task to a claim that by then holds its phase's task for
// this run. That row reads no slot and carries no entry, the claim being on the entry its row
// names already; the owed entry's start takes the slot mark the slot rule gives its phase, as
// every start Engine.started builds does (StartOrOwe), so it waits for the issue's admission slot
// in a phase that keeps a slot and for none in accepting, retro or merging (record.KeepsSlot).
// Most often such an issue holds that slot already: a claim whose agent has just said it is ready
// is running, and admission has long since given its tree a slot. The one such issue at work that
// holds none is one that gave its row back to wait on another issue (admit's releaseSlotsForWaits,
// record.Issue.ReleasedForWaitAt), and the two branches still hold for it: the wait stops none of
// its work, so a level claim's resume is no new work and goes at once, while an owed entry's work
// is work the claim does not have, and its start waits until the issue holds a slot again, as the
// slot repair's does (admit's resumeSlottedWorkers). The rule is "Admission and the `legion`
// label" in docs/site/src/content/docs/legion/concepts.md.
//
// A level claim that finished the last task written to it, in the session it runs now, is sent
// nothing (finishedInItsSession, dispatch://INFRA-2811 R4). Such a worker is one that waits: it
// did its work and ended its turn, and its process was relaunched under it — a death, a stale
// plugin, a displacement, a cancelled idle release — with its session resumed. The resume task
// would only ask it to do again what it already did, and the session's own resume reminder
// (packages/pi-shared's armResumeReminder) is what wakes a worker whose phase is still open. A
// claim never written a task is sent the resume, and so is one whose session is new since it
// finished (a fresh session, or an abandoned one replaced), since a new session has no reminder
// state to wake it. The rule covers the resume task alone: the owed entry's work above, a
// transition's start, a wake's task and every other task are written as before.
func (e *Engine) readyWorkerTask(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.ClaimReady) error {
	if record.OutOfWorkflow(issue.Status) {
		return nil
	}
	role := RoleFor(issue.Phase)
	if role == "" || role == claim.RoleArchitect || role != fact.Role {
		return nil
	}
	root, err := e.store.Issue(ctx, tx, issue.Tree)
	if err != nil || root == nil || root.Lingers() {
		return err
	}
	finished, err := e.phaseFinished(ctx, tx, issue)
	if err != nil || finished {
		return err
	}
	project, err := claim.ProjectToken(issue.Project)
	if err != nil {
		return err
	}
	token, err := claim.NewToken(project, issue.Key, role)
	if err != nil {
		return err
	}
	run, err := e.store.RoleRun(ctx, tx, token, issue.Key, role, issue.Generation)
	if err != nil {
		return err
	}
	// A claim holding only the daemon's own delivery of a relaunch's record, or only a task of news
	// alone, holds no work (above).
	held := run.Claim
	if held == nil || supervise.HoldsWork(held.Pending) {
		return nil
	}
	if queuedRowCovers(run, *root, issue.Phase) {
		return nil
	}
	row, err := e.phaseRow(ctx, tx, issue.Key, role)
	if err != nil {
		return err
	}
	if OwedEntry(run, *root, row.Entry) {
		if err := e.SendOwedEntry(ctx, tx, issue, role, row); err != nil {
			return err
		}
		e.logOnCommit(ctx, "workflow: a ready worker behind the entry its phases row names is sent that entry's work",
			"issue", issue.Key, "role", role, "phase", issue.Phase, "entry", row.Entry, "launch", fact.Launch)
		return nil
	}
	if finishedInItsSession(*held) {
		e.logOnCommit(ctx, "workflow: a ready worker that finished its last task in this session is given nothing; its own reminder wakes it",
			"claim", token, "issue", issue.Key, "role", role, "phase", issue.Phase, "launch", fact.Launch, "task", held.TaskFinished,
			"session", held.Session)
		return nil
	}
	resume, err := e.ResumeTask(ctx, tx, issue)
	if err != nil {
		return err
	}
	if err := e.enqueue(ctx, tx, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: role,
		Generation: issue.Generation, Phase: issue.Phase, Task: resume, Origin: supervise.OriginResume,
		ResumeTask: true}); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a ready worker that holds its issue's phase with nothing pending is given its phase's task",
		"issue", issue.Key, "role", role, "phase", issue.Phase, "launch", fact.Launch)
	return nil
}

// finishedInItsSession is the resume rule's test of a claim (dispatch://INFRA-2811 R4): the
// record has written it a task, and the session it finished its latest finished task in
// (TaskFinishedSession) is the session it holds. It compares no numbers, so a claim whose latest
// task was dropped unfinished after it finished an earlier one in this session passes it too.
// supervise's finishedLastTask, which the `finished` of the value-written line reads, also asks
// that the last task finished be the last one written, and so reads such a claim as not finished.
// The phase's resume task is queued for a claim of which this is false — one never written a
// task, or whose session is new since it finished — and for no other, at a ready
// (readyWorkerTask) and at the boot (booted).
func finishedInItsSession(held record.RoleClaim) bool {
	return held.TaskWritten != 0 && held.Session == held.TaskFinishedSession
}

// treeWaits is what a catch-up's reason names of the issues an architect owns beyond the catch-up
// itself (treeCatchUp): the keys of the issues waiting for this architect's acceptance, and a
// clause for each listed issue that roots rest on (restingClause).
type treeWaits struct {
	accepting []string
	resting   []string
}

// treeCatchUp is what issue's architect owns as the record holds it. A tree root's architect owns
// its whole tree: the catch-up says whether a hand-over created the root at this generation, and
// whether its old root's gate came with it, each design-gate step the root must hold open and how
// it stands, and every issue of the tree in key order. The steps are the ones the tree's work
// waits on (record.TreeGate.Required), so a criteria row the architect registered beside a
// deployment that gates spec and not criteria is listed after the spec, gated as the spec is. A
// child's architect owns the child and every issue whose recorded parent chain reaches it
// (record.Beneath), listed in key order at the child's own generation: a child registers no gate
// and no hand-over creates one, so its catch-up states no step and is neither handed over nor
// carried.
//
// With it come the keys, in key order, of the listed issues waiting for this architect's
// acceptance (awaitingAcceptance). No worker runs in accepting, and the one phase-finished notice
// that moved an issue there went to the architect that owned the issue then: a root's ends
// undelivered while the root's architect claim has failed (the outbox's notice), and a child's
// went past its failed architect to the architect above it (the owner walk). So an architect that
// comes back learns of the wait from its catch-up alone; the reason names these keys so that the
// catch-up, which a revived architect reads as its issues already running, says what is the
// architect's own to do.
//
// With them come, for each listed issue still in the workflow, in key order, the roots that rest
// on its approval and are still in the workflow (record.RestingRoots, dispatch://INFRA-3320 R8),
// read for all of them at once (record.RestingRootsOn): their work holds that issue's acceptance
// and its close, and a resting-root notice that told of one is ended undelivered when the
// architect's claim has failed, as the phase-finished notice is, so the reason names them beside
// the accepting list.
func treeCatchUp(ctx context.Context, tx pgx.Tx, store record.Store, steps []record.DesignStep, issue record.Issue) (record.CatchUp, treeWaits, error) {
	catchUp := record.CatchUp{Generation: issue.Generation, Gates: []record.CatchUpGate{}}
	if claim.IsTreeRoot(issue.Key, issue.Tree) {
		handed, err := ArchitectCarried(ctx, tx, store, issue)
		if err != nil {
			return record.CatchUp{}, treeWaits{}, err
		}
		if handed != nil {
			catchUp.HandedOver, catchUp.Carried = true, handed.GateCarried
		}
		gates, err := store.GatesOf(ctx, tx, issue.Key)
		if err != nil {
			return record.CatchUp{}, treeWaits{}, err
		}
		gated := classify.TreeSteps(steps, gates)
		required := record.TreeGate{Steps: steps, Rows: gates}.Required()
		catchUp.Gates = make([]record.CatchUpGate, 0, len(required))
		for _, step := range required {
			stated := record.CatchUpGate{Step: step, Gated: slices.Contains(gated, step)}
			if recorded, ok := gates[step]; ok {
				stated.Artifact, stated.Version, stated.Open = recorded.ArtifactID, recorded.LatestVersion, classify.DesignGateOpen(recorded)
				// A step open on an ancestor's approval holds the version the root registered as
				// its ApprovedVersion, which no person approved, so it states no approved version:
				// the basis beside it is what opened the step (dispatch://INFRA-3320, R2).
				if recorded.ApprovedVersion != nil && !recorded.OnBasis() {
					stated.Approved = *recorded.ApprovedVersion
				}
				if recorded.Basis != nil {
					stated.Pending, stated.Basis = recorded.PendingOnBasis(), record.CatchUpBasisOf(*recorded.Basis)
				}
			} else {
				stated.Unregistered = true
			}
			catchUp.Gates = append(catchUp.Gates, stated)
		}
	}
	members, err := store.TreeIssues(ctx, tx, issue.Tree)
	if err != nil {
		return record.CatchUp{}, treeWaits{}, err
	}
	owned := map[string]bool{issue.Key: true}
	for _, under := range record.Beneath(issue, members) {
		owned[under.Key] = true
	}
	catchUp.Issues = make([]record.CatchUpIssue, 0, len(owned))
	var accepting []record.Issue
	var live []string
	for _, member := range members {
		if !owned[member.Key] {
			continue
		}
		listed := record.CatchUpIssue{Key: member.Key, Title: member.Title, Phase: member.Phase, Status: member.Status}
		if member.Parent != nil {
			listed.Parent = *member.Parent
		}
		catchUp.Issues = append(catchUp.Issues, listed)
		if !record.InWorkflow(member) {
			continue
		}
		if member.Phase == phase.Accepting {
			accepting = append(accepting, member)
		}
		live = append(live, member.Key)
	}
	var waits treeWaits
	if waits.accepting, err = awaitingAcceptance(ctx, tx, store, issue, accepting); err != nil {
		return record.CatchUp{}, treeWaits{}, err
	}
	resting, err := record.RestingRootsOn(ctx, tx, store, live)
	if err != nil {
		return record.CatchUp{}, treeWaits{}, err
	}
	for _, key := range live {
		if roots := resting[key]; len(roots) > 0 {
			waits.resting = append(waits.resting, restingClause(key, roots))
		}
	}
	return catchUp, waits, nil
}

// awaitingAcceptance is the keys, in the order given, of accepting — issues of issue's tree that
// wait in accepting and are still in the workflow — whose acceptance is issue's architect's: the
// architect the phase-finished notice that moved each there goes to, which is the owner walk from
// the issue itself over the architect claims this transaction reads (record.ReadOwnerWalk). So a
// root's architect is not asked to accept an issue whose own architect, or an architect between
// the two, holds its role, and is asked to accept one whose architects up to the root have all
// failed, retired, never started, or been suspended by the workflow or an operator (a claim the
// idle release suspended still holds its role). An issue whose key makes no claim token has no
// architect, so the walk makes it the tree root's to accept. No claim is read when nothing waits.
func awaitingAcceptance(ctx context.Context, tx pgx.Tx, store record.Store, issue record.Issue, accepting []record.Issue) ([]string, error) {
	if len(accepting) == 0 {
		return nil, nil
	}
	project, err := claim.ProjectToken(issue.Project)
	if err != nil {
		return nil, err
	}
	architect, err := claim.NewToken(project, issue.Key, claim.RoleArchitect)
	if err != nil {
		return nil, err
	}
	walk, err := record.ReadOwnerWalk(ctx, store, tx, project, issue.Tree)
	if err != nil {
		return nil, err
	}
	awaiting := make([]string, 0, len(accepting))
	for _, waiting := range accepting {
		owner, err := walk.Architect(waiting, false)
		if err != nil {
			return nil, err
		}
		if owner == architect {
			awaiting = append(awaiting, waiting.Key)
		}
	}
	return awaiting, nil
}
