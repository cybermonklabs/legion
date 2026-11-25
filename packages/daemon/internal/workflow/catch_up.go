package workflow

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/texts"
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
// differently: it is given its phase's task again when it holds that phase with nothing pending
// (readyWorkerTask).
//
// The catch-up a start writes in place of a launch (intake.ClaimReady.Paced) is a wake of the
// architect — it reads its whole context when the catch-up arrives, which is what the per-minute
// cap counts — so it takes its turn in the daemon's pacing (Config.Reserve) and the row is due at
// the moment that turn comes, rather than at once. The turn is taken here, inside the fact's
// transaction, so a start row that runs again after a crash takes no second one: the fact applies
// once for the row that wrote it. A launch's own ready takes none, since the launch took its turn
// at the machine's gate (supervise's reserveWake); so does every ready while the pacing is off.
//
// A launch's ready that carries what the last let-go of the architect's process ended
// (intake.ClaimReady.Relaunch) has its catch-up carry it (QueueCatchUp): the first turn of a
// relaunched architect can be this catch-up's rather than its prompt's, and the architect then
// reads that its subagent died with its issues. A prompt the claim's machine sends tells the same
// record and is what marks it told, so a catch-up that is held, superseded or never queued here
// costs nothing (dispatch://INFRA-3276 R3).
func (e *Engine) claimReady(ctx context.Context, tx pgx.Tx, fact intake.ClaimReady) (intake.Result, error) {
	if fact.Role != claim.RoleArchitect {
		return intake.Result{}, e.readyWorkerTask(ctx, tx, fact)
	}
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
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
	now := e.now()
	var paced time.Time
	if fact.Paced && e.cfg.Reserve != nil {
		paced = e.cfg.Reserve(now)
	}
	return intake.Result{}, QueueCatchUp(ctx, tx, e.store, e.cfg.Steps, *issue, fact.Launch, fact.Relaunch, now, paced)
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
// relaunch is what the last let-go of the architect's process ended, which a launch's ready
// carries while the claim's record of it is untold (intake.ClaimReady.Relaunch), and nil from
// every other caller; the notice carries it beside the tree. The copy tells nothing: the record is
// the claim's, and it is told at the confirmation of a prompt the claim's machine sends that tells
// the same record (supervise's confirm, Delivery.message), which the outbox's publish of this
// notice does not see.
//
// A catch-up an earlier launch never had delivered is dropped first (DropCatchUps, at the caller's
// now, which is the clock a lane's lease is written from), so the agent is given one catch-up,
// computed now. A root's names again the decisions the dropped one named — the drop releases them
// in this transaction — with any kept since. A catch-up that carries kept decisions is not dropped
// while any row of its telling — itself, the row it copies, or a re-held copy of either — is under
// a live lease: that row may be publishing now, and the decisions the catch-up claims are that
// telling's to settle, so this one names only what nobody claims. A child's names no decision: what
// a person decided while nobody could hear it is kept for the tree (ParkedDecisionTask keys it by
// tree) and stays the root architect's, so a child's catch-up carries no Kept. Each decision it
// names carries the person's words cut to texts.OneAct where it carries the words of one decision
// and to texts.EachOfSeveral each where it carries those of several (keptFacts, keptBound), named
// in the words object by their path with the person and the time; the row keeps them whole. A
// kept approval names its document as a dispatch:// link (stepRef), as the gates do.
// The notice goes through the outbox like any other, so it is held and re-held as they are.
//
// paced is the moment the daemon's wake pacing reserved for this catch-up, which is both the
// notice row's next_at and the moment the notice itself carries (record.Notice.PacedAt): the zero
// value, which every caller but the paced ready passes (CatchUpTheTreesOwed for HandOverKept, and
// the ready of a launch), is now, so a catch-up no pacing holds is due as it always was. The row
// keeps the moment of its own because a failed attempt rewrites next_at to its backoff: the
// moment is what keeps a ready from pulling the row forward before its turn (the daemon's
// releaseWaiting), and with now beside it (record.Notice.PacedFrom) it is the wait in the line
// that wake writes when it runs (the daemon's wakeRan). The pair is written only where the
// pacing reserved something: a catch-up it held to no moment reports no wait, having had none.
func QueueCatchUp(ctx context.Context, tx pgx.Tx, store record.Store, steps []record.DesignStep, issue record.Issue, launch uint64,
	relaunch *texts.RelaunchFacts, now, paced time.Time) error {
	catchUp, err := treeCatchUp(ctx, tx, store, steps, issue)
	if err != nil {
		return err
	}
	catchUp.Launch = launch
	if err := store.DropCatchUps(ctx, tx, issue.Key, now); err != nil {
		return err
	}
	told := texts.CatchUpRecord{NoticeHead: texts.NoticeHead{Kind: texts.CatchUp, Issue: issue.Key}, CatchUp: &catchUp, Relaunch: relaunch}
	var kept *record.KeptDecisions
	if claim.IsTreeRoot(issue.Key, issue.Tree) {
		// A decision kept since the start's task was written (one a person made while the tree's
		// architect claim had ended, before this relaunch), and one a start or catch-up that
		// reached no architect handed over, are told here. The catch-up names only what no other
		// start or catch-up claims (a start still queued keeps its own), and claims what it names
		// in this transaction; its publish forgets those rows (the outbox's notice), and a
		// catch-up a newer one supersedes hands them to a fresh catch-up (HandOverKept).
		if kept, err = keptOwed(ctx, tx, store, steps, issue.Key); err != nil {
			return err
		}
		if kept != nil {
			catchUp.Kept, told.Words = keptFacts(*kept)
			if kept.Approved > 0 {
				catchUp.Kept.Document = stepRef(issue.Key, kept.Step.OrSpec())
			}
		}
	}
	notice, err := record.NoticeOf(told)
	if err != nil {
		return err
	}
	at := now
	if !paced.IsZero() {
		at = paced
		notice.PacedAt, notice.PacedFrom = paced, now
	}
	return EnqueueNaming(ctx, tx, store, issue.Key, issue.Tree, notice, kept, at)
}

// keptFacts is kept as a catch-up names it: each decision with the person's words cut to the bound
// keptBound gives, as the kept-decisions task cuts them (KeptDecisionsOf), and the words object
// that names each one by its path with the person, the time they wrote it and the count of the
// rest, or that more follows than the act's event carried. A kept approval names its step as the
// kept-decisions task does: the spec where an older daemon kept the approval with no step.
// QueueCatchUp names the approval's document, which the root's key gives.
func keptFacts(kept record.KeptDecisions) (*texts.KeptFacts, map[string]texts.WordsMeta) {
	facts := &texts.KeptFacts{}
	if kept.Approved > 0 {
		facts.KeptApproval = texts.KeptApproval{Step: string(kept.Step.OrSpec()), Approved: kept.Approved, Latest: kept.Latest}
	}
	bound := keptBound(kept.Decisions)
	var words map[string]texts.WordsMeta
	for i, mark := range kept.Decisions {
		named := texts.DecisionMark{Ref: mark.Ref, At: mark.At, Issue: mark.Issue, Decision: string(mark.Decision), Actor: mark.Actor,
			Options: mark.Options, AnswerAt: mark.AnswerAt, More: mark.More}
		if mark.Answer != "" {
			carried := mark.Carry(bound)
			named.Answer, named.More = carried.Text, carried.More
			if words == nil {
				words = map[string]texts.WordsMeta{}
			}
			words[fmt.Sprintf("catch_up.kept.decisions[%d].answer", i)] = texts.WordsMeta{By: mark.Actor, At: mark.AnswerAt, Rest: carried.Rest, More: carried.More}
		}
		facts.Decisions = append(facts.Decisions, named)
	}
	return facts, words
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
// The daemon's own delivery of a relaunch's record is no task (supervise.HoldsTask), so a claim
// that holds that alone is read as holding nothing. The phase's task that this queues takes that
// delivery's place while no prompt of it waits on a result, or waits for its turn to end
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
// A claim level with its row is sent the phase's resume task, enqueued here and marked as a
// resume task, so the one rule about already-delivered work stays the outbox's: it does not
// deliver a resume task to a claim that by then holds its phase's task for this run. That row
// reads no slot and carries no entry, the claim being on the entry its row names already; the
// owed entry's start takes the slot mark the slot rule gives its phase, as every start
// Engine.started builds does (StartOrOwe), so it waits for the issue's admission slot in a phase
// that keeps a slot and for none in accepting, retro or merging (record.KeepsSlot). Most often such an issue
// holds that slot already: a claim whose agent has just said it is ready is running, and
// admission has long since given its tree a slot. The one such issue at work that holds none is
// one that gave its row back to wait on another issue (admit's releaseSlotsForWaits,
// record.Issue.ReleasedForWaitAt), and the two branches still hold for it: the wait stops none of
// its work, so a level claim's resume is no new work and goes at once, while an owed entry's work
// is work the claim does not have, and its start waits until the issue holds a slot again, as the
// slot repair's does (admit's resumeSlottedWorkers). The rule is "Admission and the `legion`
// label" in docs/site/src/content/docs/legion/concepts.md.
func (e *Engine) readyWorkerTask(ctx context.Context, tx pgx.Tx, fact intake.ClaimReady) error {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil || record.OutOfWorkflow(issue.Status) {
		return err
	}
	role := RoleFor(issue.Phase)
	if role == "" || role == claim.RoleArchitect || role != fact.Role {
		return nil
	}
	root, err := e.store.Issue(ctx, tx, issue.Tree)
	if err != nil || root == nil || root.Lingers() {
		return err
	}
	finished, err := e.phaseFinished(ctx, tx, *issue)
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
	if held := run.Claim; held == nil || supervise.HoldsWork(held.Pending) {
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
		if err := e.SendOwedEntry(ctx, tx, *issue, role, row); err != nil {
			return err
		}
		e.logOnCommit(ctx, "workflow: a ready worker behind the entry its phases row names is sent that entry's work",
			"issue", issue.Key, "role", role, "phase", issue.Phase, "entry", row.Entry, "launch", fact.Launch)
		return nil
	}
	resume, err := e.ResumeTask(ctx, tx, *issue)
	if err != nil {
		return err
	}
	if err := e.enqueue(ctx, tx, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: role,
		Generation: issue.Generation, Phase: issue.Phase, Task: resume, ResumeTask: true}); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a ready worker that holds its issue's phase with nothing pending is given its phase's task",
		"issue", issue.Key, "role", role, "phase", issue.Phase, "launch", fact.Launch)
	return nil
}

// treeCatchUp is what issue's architect owns as the record holds it. A tree root's architect owns
// its whole tree: the catch-up says whether a hand-over created the root at this generation, and
// whether its old root's gate came with it, and each design-gate step the root must hold open, its
// document (stepDocuments, the start task's reading of the same rows: a dispatch:// link on the
// issue whose document the step's row is bound to, the old root's for a row a carried gate
// brought, or the bare artifact reference where the record does not say that issue, which the
// outbox names from Dispatch when it publishes the catch-up, NameCatchUpDocuments) and
// how it stands. The steps are the ones the tree is gated on (classify.TreeSteps), so a criteria
// row the architect registered beside a deployment that gates spec and not criteria is listed
// after the spec, gated as the spec is. A child's architect owns the child and every issue whose
// recorded parent chain reaches it (record.Beneath), at the child's own generation: a child
// registers no gate and no hand-over creates one, so its catch-up states no step and is neither
// handed over nor carried.
//
// Of the issues it owns, in key order, each one that is not done is named with its fields, and
// each done one by its key alone. Beside them come the children not yet released whole
// (unreleasedChildren), and the keys, in key order, of the issues waiting for this architect's
// acceptance (awaitingAcceptance). No worker runs in accepting, and the one phase-finished notice
// that moved an issue there went to the architect that owned the issue then: a root's ends
// undelivered while the root's architect claim has failed (the outbox's notice), and a child's
// went past its failed architect to the architect above it (the owner walk). So an architect that
// comes back learns of the wait from its catch-up alone, and the list says what is the
// architect's own to do.
func treeCatchUp(ctx context.Context, tx pgx.Tx, store record.Store, steps []record.DesignStep, issue record.Issue) (texts.CatchUpFacts, error) {
	catchUp := texts.CatchUpFacts{Generation: issue.Generation, Gates: []texts.CatchUpGate{}}
	if claim.IsTreeRoot(issue.Key, issue.Tree) {
		handed, err := ArchitectCarried(ctx, tx, store, issue)
		if err != nil {
			return texts.CatchUpFacts{}, err
		}
		if handed != nil {
			catchUp.HandedOver, catchUp.Carried = true, handed.GateCarried
		}
		gates, err := store.GatesOf(ctx, tx, issue.Key)
		if err != nil {
			return texts.CatchUpFacts{}, err
		}
		documents, err := stepDocuments(ctx, tx, store, gates)
		if err != nil {
			return texts.CatchUpFacts{}, err
		}
		gated := classify.TreeSteps(steps, gates)
		required := classify.RequiredSteps(gated)
		catchUp.Gates = make([]texts.CatchUpGate, 0, len(required))
		for _, step := range required {
			stated := texts.CatchUpGate{Step: string(step), Gated: slices.Contains(gated, step), Document: stepRef(issue.Key, step)}
			if recorded, ok := gates[step]; ok {
				stated.Artifact, stated.Version, stated.Open = recorded.ArtifactID, recorded.LatestVersion, classify.DesignGateOpen(recorded)
				if recorded.ApprovedVersion != nil {
					stated.Approved = *recorded.ApprovedVersion
				}
				stated.Document = documents[step]
			} else {
				stated.Unregistered = true
			}
			catchUp.Gates = append(catchUp.Gates, stated)
		}
	}
	members, err := store.TreeIssues(ctx, tx, issue.Tree)
	if err != nil {
		return texts.CatchUpFacts{}, err
	}
	owned := map[string]bool{issue.Key: true}
	for _, under := range record.Beneath(issue, members) {
		owned[under.Key] = true
	}
	catchUp.Issues = make([]texts.CatchUpIssue, 0, len(owned))
	var accepting []record.Issue
	for _, member := range members {
		if !owned[member.Key] {
			continue
		}
		if member.Phase == phase.Done && member.Status == record.DoneStatus {
			catchUp.Done = append(catchUp.Done, member.Key)
			continue
		}
		listed := texts.CatchUpIssue{Key: member.Key, Title: member.Title, Phase: member.Phase, Status: member.Status}
		if member.Parent != nil {
			listed.Parent = *member.Parent
		}
		catchUp.Issues = append(catchUp.Issues, listed)
		if member.Phase == phase.Accepting && record.InWorkflow(member) {
			accepting = append(accepting, member)
		}
	}
	if catchUp.Unreleased, err = unreleasedChildren(ctx, tx, store, issue.Project, owned, members); err != nil {
		return texts.CatchUpFacts{}, err
	}
	if catchUp.Accepting, err = awaitingAcceptance(ctx, tx, store, issue, accepting); err != nil {
		return texts.CatchUpFacts{}, err
	}
	return catchUp, nil
}

// stepDocuments is the document each of rows, a root's design gate rows by step, is bound to, by
// step: the step's dispatch:// link (stepRef) on the issue whose document the row's artifact is.
// That is the issue the row records (record.DesignGate.DocumentIssue), which a root's own
// registration and a carry that knew it write, and, for a row that records none (one a daemon
// before the column wrote, a carry of a row whose issue nothing said), the issue the store's rows
// on the same artifact say (record.Store.DocumentIssues): the document's own registration, the
// row of an issue that is no root carried in with its gate, and any row that records it. A row
// neither says, a carried copy whose old root's rows are gone (a re-admission of that root
// cleared them) or were a copy themselves, names the bare artifact reference
// (texts.ArtifactReference) rather than a document that may be another issue's, and the daemon
// names it from Dispatch when the outbox runs the start row and queues the task (and when rootTask
// writes a queued task again at its send), when the outbox publishes the catch-up, and when the
// claim sends a task of news (NameStartDocuments, NameCatchUpDocuments, NameNewsDocuments). The
// architect-start task (ArchitectStartOf), the catch-up (treeCatchUp) and the news of an approved
// design (GateDocuments) all read it, so they name one document for one row whatever daemon wrote
// the row.
func stepDocuments(ctx context.Context, tx pgx.Tx, store record.Store, rows map[record.DesignStep]record.DesignGate) (map[record.DesignStep]string, error) {
	var unnamed []string
	for _, row := range rows {
		if row.DocumentIssue == "" {
			unnamed = append(unnamed, row.ArtifactID)
		}
	}
	var issues map[string]string
	if len(unnamed) > 0 {
		var err error
		if issues, err = store.DocumentIssues(ctx, tx, unnamed); err != nil {
			return nil, err
		}
	}
	documents := make(map[record.DesignStep]string, len(rows))
	for step, row := range rows {
		issue := row.DocumentIssue
		if issue == "" {
			issue = issues[row.ArtifactID]
		}
		if issue == "" {
			documents[step] = texts.ArtifactReference(row.ArtifactID)
			continue
		}
		documents[step] = stepRef(issue, step)
	}
	return documents, nil
}

// GateDocuments is stepDocuments for a text the daemon builds outside the workflow: the news of an
// approved design, which the daemon's send reads in its own transaction (the daemon's
// readNewsAtSend). So the news names a gate row's document by the one rule the architect-start
// task and the catch-up name it by, and the bare artifact reference that rule gives is named from
// Dispatch where the daemon sends the task (NameNewsDocuments).
func GateDocuments(ctx context.Context, tx pgx.Tx, store record.Store, rows map[record.DesignStep]record.DesignGate) (map[record.DesignStep]string, error) {
	return stepDocuments(ctx, tx, store, rows)
}

// NameCatchUpDocuments names, in catchUp, a catch-up as QueueCatchUp queued it, each gate step's
// document that it gives as the bare artifact reference (stepDocuments) as the step's document of
// the issue issueOf answers holds the artifact, where it answers one (namedDocument), as
// NameStartDocuments names the start task's. issueOf is the outbox's read of Dispatch when it
// publishes the catch-up, outside any transaction; a step it answers nothing for keeps the bare
// reference.
func NameCatchUpDocuments(catchUp *record.CatchUp, issueOf func(artifact string) (string, bool)) {
	for i, gate := range catchUp.Gates {
		catchUp.Gates[i].Document = namedDocument(gate.Document, gate.Step, issueOf)
	}
}

// NameNewsDocuments names, in documents, a tree's gate documents as GateDocuments read them for
// the news of an approved design (News.Documents), each one it gives as the bare artifact
// reference as the step's document of the issue issueOf answers holds the artifact, where it
// answers one (namedDocument), as the catch-up's are named (NameCatchUpDocuments). issueOf is the
// daemon's read of Dispatch where it sends the task (the daemon's designNews), outside any
// transaction; a step it answers nothing for keeps the bare reference.
func NameNewsDocuments(documents map[record.DesignStep]string, issueOf func(artifact string) (string, bool)) {
	for step, document := range documents {
		documents[step] = namedDocument(document, step, issueOf)
	}
}

// namedDocument is document, the document of a gate row of step as stepDocuments gives it, named as
// step's document of the issue issueOf answers holds its artifact where it is the bare artifact
// reference and issueOf answers one, and as it is otherwise: the catch-up's naming
// (NameCatchUpDocuments) and the news's (NameNewsDocuments) are this one.
func namedDocument(document string, step record.DesignStep, issueOf func(artifact string) (string, bool)) string {
	artifact, bare := texts.ArtifactOfReference(document)
	if !bare {
		return document
	}
	if issue, ok := issueOf(artifact); ok {
		return stepRef(issue, step)
	}
	return document
}

// unreleasedChildren is each child the Dispatch mirror shows under an issue of owned, the issues
// an architect owns, that its architect filed and has not released: the daemon holds no record of
// it, in this tree or any other, and it still holds work (record.ChildOpen of a child with no
// record), with its parent and its Dispatch status, in key order within each parent. A child of
// members, the tree's records, is released; the mirror's other children are read by key, once.
func unreleasedChildren(ctx context.Context, tx pgx.Tx, store record.Store, project string, owned map[string]bool,
	members []record.Issue) ([]texts.UnreleasedChild, error) {
	parents := slices.Sorted(maps.Keys(owned))
	children, err := store.DispatchChildren(ctx, tx, project, parents)
	if err != nil {
		return nil, err
	}
	recorded := make(map[string]bool, len(members))
	for _, member := range members {
		recorded[member.Key] = true
	}
	var unrecorded []string
	for _, parent := range parents {
		for _, child := range children[parent] {
			if !recorded[child.Key] {
				unrecorded = append(unrecorded, child.Key)
			}
		}
	}
	if len(unrecorded) == 0 {
		return nil, nil
	}
	elsewhere, err := store.IssuesOf(ctx, tx, project, unrecorded)
	if err != nil {
		return nil, err
	}
	for _, other := range elsewhere {
		recorded[other.Key] = true
	}
	var unreleased []texts.UnreleasedChild
	for _, parent := range parents {
		for _, child := range children[parent] {
			if !recorded[child.Key] && record.ChildOpen(child, nil) {
				unreleased = append(unreleased, texts.UnreleasedChild{Key: child.Key, Parent: parent, Status: child.Status})
			}
		}
	}
	return unreleased, nil
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
