package workflow

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// relayParkedDecision hands a person's answer, resolution or reply on ask of key to the one rule
// for a decision nobody in its tree hears (record.RelayParkedDecision), and logs one line for what
// became of it: relayed to the project's controller, which decides whether the tree runs again
// (skill://legion-controller/references/parked-decisions.md); kept alone for the next architect
// start of a root that waits for its slot, whose tree already runs again; not relayed, for a
// session's write on a parked tree or on a root waiting for its slot, or for a lingering root
// someone moved back into the workflow. A decision on a tree that runs, which hears it itself, and
// an ask of an issue Legion does not record log nothing: that is every ordinary ask. The writes are
// in the event's own transaction, which ApplyFact keys by the Dispatch event's id, so a replay
// writes none of them again.
func (e *Engine) relayParkedDecision(ctx context.Context, tx pgx.Tx, key, ask string, decision record.ParkedDecision) error {
	root, err := e.treeRoot(ctx, tx, key)
	if err != nil || root == nil {
		return err
	}
	outcome, err := record.RelayParkedDecision(ctx, tx, e.store, key, *root, root.Lingers(),
		decision, record.Notice{}, e.cfg.Steps)
	if err != nil {
		return err
	}
	args := []any{"issue", key, "root", root.Key, "ask", ask, "decision", decision.Decision, "ref", decision.Ref, "actor", decision.Actor}
	switch outcome {
	case record.RelaySent:
		e.logOnCommit(ctx, "workflow: a person's decision on a parked tree goes to the controller", args...)
	case record.RelayKept:
		e.logOnCommit(ctx, "workflow: a person's decision on a tree waiting for its slot is kept for its architect's start", args...)
	case record.RelayNoPerson:
		if decision.Decision == record.DecisionAskReply {
			e.logOnCommit(ctx, "workflow: a session's reply on a parked tree is not relayed", args...)
		} else {
			e.logOnCommit(ctx, "workflow: a session's answer or resolution on a parked tree is not relayed", args...)
		}
	case record.RelayNoPersonWaiting:
		e.logOnCommit(ctx, "workflow: a session's decision on a tree waiting for its slot is not kept", args...)
	case record.RelayNotParked:
		e.logOnCommit(ctx, "workflow: a decision on a lingering root someone moved back into the workflow is not relayed", args...)
	}
	return nil
}

// treeRoot is the record of the root of key's tree, nil when key is not recorded: key's own row
// when it is its tree's root, read once.
func (e *Engine) treeRoot(ctx context.Context, tx pgx.Tx, key string) (*record.Issue, error) {
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil || issue == nil || issue.Tree == key {
		return issue, err
	}
	return e.store.Issue(ctx, tx, issue.Tree)
}

// ParkedDecisionTask is what root's architect is told at its start when the daemon kept a person's
// decision on an issue of its tree while nobody in the tree could hear it (relayParkedDecision,
// the outbox's parkDecision): each decision's ref, kind and author, newest first, so the architect
// reads what brought it back, as ResumeAwaitedRoot names an answered root's answer. Admission's
// start carries it as its task; every later launch's ready (claimReady: a relaunch, `legion claims
// retry`) adds what was kept since to its catch-up. When the re-admission found the root's spec
// approved at a version below its latest (the admission's readmit, Store.KeepReadmittedApproval: a
// person approved a version the spec has since moved past), it also names both versions, so the
// architect requests approval of the latest. It first forgets what is no longer the person's word
// (ForgetSuperseded), then renders what is left (KeptDecisionsTask).
//
// What it may name is what root's tree is owed now: every row whose issue stands in that tree and
// that no counting claim or hold holds (Store.UnclaimedKept). A claim or a hold an issue left
// behind in another tree counts for nothing, so a child handed over, a parked child orphaned, a
// grandchild handed over beside its parent and a queued root adopted each bring what was decided
// on them to this task with nothing moved, in whatever order their admissions run.
//
// The text names no generation: a start's task waits on its claim and is prompted whenever that
// claim is next ready, which can be generations later, and the one architect session of the root
// reads it then as what a person decided on its tree. It deletes only the rows that are no longer
// the person's word (ForgetSuperseded): the caller enqueues the task with the record of what it
// named (record.KeptDecisions) and claims those rows for it in the same transaction
// (EnqueueNaming). A catch-up that published tells what it named (Store.ForgetKeptDecisions) and
// a start's task tells it at the confirmation of the delivery the claim queued it as; one that
// reached no architect hands it over (HandOverKept). Both results are zero when there is nothing
// to name.
func ParkedDecisionTask(ctx context.Context, tx pgx.Tx, store record.Store, steps []record.DesignStep, root record.Issue) (string, *record.KeptDecisions, error) {
	decisions, approval, err := store.UnclaimedKept(ctx, tx, root.Key)
	if err != nil {
		return "", nil, err
	}
	owed := record.KeptDecisions{KeptApproval: approval}
	for _, d := range decisions {
		owed.Decisions = append(owed.Decisions, d.Mark())
	}
	gates, err := ForgetSuperseded(ctx, tx, store, steps, root.Key, owed)
	if err != nil {
		return "", nil, err
	}
	task, kept := KeptDecisionsTask(gates, steps, root.Key, owed)
	if task == "" {
		return "", nil, nil
	}
	return task, &kept, nil
}

// KeptGates is the gates a list of kept decisions is asked of: those of each issue a decision was
// made on, and the tree's own for its kept approval, by issue.
type KeptGates map[string]map[record.DesignStep]record.DesignGate

// ReadKeptGates reads in tx the gates owed is asked of (KeptGates), in one read
// (record.Store.Gates), and writes nothing: a delivery's read-only transaction reads them so (the
// daemon's readNoticeTree). An issue with no gate has no entry, which gateOf reads as no gate.
func ReadKeptGates(ctx context.Context, tx pgx.Tx, store record.Store, tree string, owed record.KeptDecisions) (KeptGates, error) {
	var issues []string
	for _, d := range owed.Decisions {
		if !slices.Contains(issues, d.Issue) {
			issues = append(issues, d.Issue)
		}
	}
	if owed.Approved > 0 && !slices.Contains(issues, tree) {
		issues = append(issues, tree)
	}
	return store.Gates(ctx, tx, issues)
}

// ForgetSuperseded reads the gates owed is asked of (ReadKeptGates) and deletes in tx the row of
// every decision of owed that is no longer the person's word (keptSuperseded, SupersedingApproval),
// and returns the gates it read, for the caller to render the task from (KeptDecisionsTask). It is
// the write half of a kept-decisions task, and every caller that renders one at a start, a ready or
// a send calls it first; the delivery of a catch-up, whose read is read-only, renders from
// ReadKeptGates and deletes nothing.
//
// A decision that is no longer the person's word is forgotten: a changes request that a later
// approval followed, or an approval the gate no longer holds, since a later changes request or
// approval took its place. Nothing of the decision is left to tell. If the row stays KEPT, every
// later start, catch-up and hand-over asks it again. Also, a later changes request that clears the
// gate's approval on a tree that runs, which keeps nothing, would make an old changes request read
// as the person's word again. A row another tree's claim still holds is not this tree's to
// delete, and it stays (Store.ForgetKeptDecisions). The approval a re-admission kept stays in its
// row when superseded, as it always has (Store.KeepReadmittedApproval replaces it at the next
// re-admission). The gates a decision is asked of go when a generation is cleared, so each clear
// runs this forget first, over the decisions kept on the issues it clears
// (Engine.ClearGeneration hands it to record.Store.ClearGeneration, which builds that set), and
// asks what every start and catch-up asks.
func ForgetSuperseded(ctx context.Context, tx pgx.Tx, store record.Store, steps []record.DesignStep, tree string,
	owed record.KeptDecisions) (KeptGates, error) {
	gates, err := ReadKeptGates(ctx, tx, store, tree, owed)
	if err != nil {
		return nil, err
	}
	var spent []record.DecisionMark
	for _, d := range owed.Decisions {
		if keptSuperseded(gates[d.Issue], steps, d) != nil {
			spent = append(spent, d)
		}
	}
	if len(spent) > 0 {
		if err := store.ForgetKeptDecisions(ctx, tx, tree, record.KeptDecisions{Decisions: spent}, 0); err != nil {
			return nil, err
		}
	}
	return gates, nil
}

// KeptDecisionsTask is the text that names owed to the architect of tree, and the part of owed it
// names (kept), rendered from gates (ReadKeptGates) and writing nothing. It is the one wording a
// start task and a catch-up both use (ParkedDecisionTask), the one a held task is written back to
// when the task waits to be sent (supervise's Deps.RootTask, the daemon's rootTask, NarrowRootTask),
// beside the roots that rest on the root's approval (ArchitectStart.Resting), and the one a
// catch-up is written again in when it is delivered (the daemon's readNoticeTree): a task that
// names nothing is "", which is retired rather than sent.
//
// Each decision is asked in the loop that writes it whether it is still the person's word
// (SupersedingApproval), so no task can name a kept decision that nobody asked about. A kept
// decision is asked as its relay is: at the version reviewed, at its own place in Dispatch's log
// and of the document reviewed (record.DecisionMark.Version, Seq, At and Artifact, which the
// outbox's parkDecision keeps from the notice's Version, Review and Artifact). The approval is
// the root's own, and it is named only while a person approves its step (record.TakesStep:
// gates.design off asks no person, and a step the deployment stopped gating asks no person either,
// unless it is criteria the root registered beside a deployment that gates spec and not criteria),
// on the document of that step (record.DesignStep.Document), which is the document the person
// reviewed and the one whose approval the architect requests again. It is asked through the one
// ask the news makes of an approval it tells (StepApprovalStands, SupersedingApproval), as a
// person's approval: of the document the re-admission kept it of (record.KeptApproval's Artifact;
// a pair kept before that field is asked of whichever document the gate holds), at the version the
// person approved, with no mark. It stands while the step has no gate, which records nothing
// later, or while the gate holds that version of that document approved by a person. It is not
// named once a later approval replaced it, which is the person's
// word in its place, so the sentence would ask them again for what they already decided; nor once
// a later changes request cleared it, which is the person's word against that version; nor once
// the step's gate holds another document.
//
// Before the new generation registers the step there is no gate row, since the re-admission's
// clear deleted it, so the kept approval stands and is named. Once it registers, the gate holds no
// approval below the latest: a registration records only an approval of the latest version
// (dispatch.Approval.ApprovedLatest, which the api's gateRegister reads), and carries an older one
// only from a stored gate of the same document (gateRegistered), which the clear deleted. The kept
// approval is below the latest, so it is no longer named unless a person approves that same
// version of that document again.
//
// The approval kept is a person's approval of an earlier version of the root's own document,
// never the version an ancestor's approval opened a step at (Admission.keepStaleApproval reads
// record.DesignGate.ApprovedByPerson), and it is asked as a person's (StepApprovalStands with no
// basis), so the task names no approval of the root's document that a basis gave
// (dispatch://INFRA-3320, R2 and R3). Beside the request of the latest version it names R1's path:
// a registration on an approved ancestor's approval, where the root is written as a view on that
// ancestor.
func KeptDecisionsTask(gates KeptGates, steps []record.DesignStep, tree string, owed record.KeptDecisions) (string, record.KeptDecisions) {
	var kept record.KeptDecisions
	named := make([]string, 0, len(owed.Decisions))
	for _, d := range owed.Decisions {
		if keptSuperseded(gates[d.Issue], steps, d) != nil {
			continue
		}
		kept.Decisions = append(kept.Decisions, d)
		named = append(named, fmt.Sprintf("%s (%s by %s)", d.Ref, d.Decision, d.Actor))
	}
	step := owed.Step.OrSpec()
	if owed.Approved > 0 && record.TakesStep(steps, step) &&
		StepApprovalStands(gates[tree], step, owed.Artifact, owed.Approved, false) {
		kept.KeptApproval = owed.KeptApproval
	}
	var parts []string
	if len(named) > 0 {
		parts = append(parts, fmt.Sprintf("While nobody in %s's tree could hear it, a person decided on the tree: %s. Read each before you continue.",
			tree, strings.Join(named, "; ")))
	}
	if kept.Approved > 0 {
		document := kept.Step.Document()
		parts = append(parts, fmt.Sprintf("A person approved version %d of the %s, and the %s is now at version %d. "+
			"Request approval of version %d, as skill://legion-architect/references/design-gate.md (\"The armed gate\") says. "+
			"The other way is to write %s as a view on an approved ancestor and register the step on that ancestor's approval, as the same page says.",
			kept.Approved, document, document, kept.Latest, kept.Latest, tree))
	}
	return strings.Join(parts, " "), kept
}

// keptSuperseded is what took the place of the kept decision mark (SupersedingApproval), nil while
// it stands, asked of gates, the gates of the issue it was made on, under steps, the steps the
// deployment gates. It is asked of the step whose document its reference names (RefSuperseded),
// at the version reviewed, at the review's place in Dispatch's log and of the document reviewed,
// as the relay of the same decision is asked (NoticeSuperseded): an approval of a later version
// is the person's word in a changes request's place whichever of the two Dispatch marked first,
// the approval of a document the step's gate has moved to answers no review of the one it left,
// and a kept approval stands only while the gate still holds it. A row kept with no version (an
// earlier release's, or a notice's that carried none) is asked at the version its gate holds
// approved, and one kept with no document is asked of whichever document the gate holds. A
// reference that names no step's document is asked of no gate, and stands.
func keptSuperseded(gates map[record.DesignStep]record.DesignGate, steps []record.DesignStep, mark record.DecisionMark) *Supersession {
	return RefSuperseded(gates, steps, mark.Issue, mark.Ref, Decision{Kind: mark.Decision, Artifact: mark.Artifact,
		Version: mark.Version, Mark: record.EventMark{Seq: mark.Seq, At: mark.At}})
}

// ParkedDecisionTask is ParkedDecisionTask over the engine's store and gated steps.
func (e *Engine) ParkedDecisionTask(ctx context.Context, tx pgx.Tx, root record.Issue) (string, *record.KeptDecisions, error) {
	return ParkedDecisionTask(ctx, tx, e.store, e.cfg.Steps, root)
}

// EnqueueNaming enqueues payload for issue, due at, a root-architect start or a catch-up notice
// whose task names kept of what tree is owed (ParkedDecisionTask), and claims those rows for that
// outbox row in the same transaction (Store.ClaimKeptDecisions), so no other start or catch-up of
// that tree names them until that row settles them: a catch-up that published tells them, a start
// whose task the claim queued leaves them to that delivery's confirmation, and a row that reached
// no architect hands them over (HandOverKept). A nil kept enqueues payload as enqueueAt does.
func EnqueueNaming(ctx context.Context, tx pgx.Tx, store record.Store, issue, tree string, payload record.OutboxPayload,
	kept *record.KeptDecisions, at time.Time) error {
	if kept == nil {
		row, err := queuedRow(ctx, tx, store, issue, payload, at)
		if err != nil {
			return err
		}
		return store.Enqueue(ctx, tx, row)
	}
	_, err := enqueueNamingID(ctx, tx, store, issue, tree, payload, kept, at)
	return err
}

// enqueueNamingID is EnqueueNaming for a caller that needs the id of the row it writes: the row
// is always written with that id kept (Store.EnqueueID), which refuses another project's key where
// Store.Enqueue skips it, and kept, where it is not nil, is claimed for that row.
func enqueueNamingID(ctx context.Context, tx pgx.Tx, store record.Store, issue, tree string, payload record.OutboxPayload,
	kept *record.KeptDecisions, at time.Time) (int64, error) {
	row, err := queuedRow(ctx, tx, store, issue, payload, at)
	if err != nil {
		return 0, err
	}
	id, err := store.EnqueueID(ctx, tx, row)
	if err != nil {
		return 0, err
	}
	if kept != nil {
		if err := store.ClaimKeptDecisions(ctx, tx, tree, *kept, id); err != nil {
			return 0, err
		}
	}
	return id, nil
}

// EnqueueNaming is EnqueueNaming over the engine's store.
func (e *Engine) EnqueueNaming(ctx context.Context, tx pgx.Tx, issue, tree string, payload record.OutboxPayload, kept *record.KeptDecisions, at time.Time) error {
	return EnqueueNaming(ctx, tx, e.store, issue, tree, payload, kept, at)
}

// HandOverKept is the one rule a release of kept decisions follows: a release never leaves a kept
// decision unclaimed while the tree it is owed to has an architect to tell. It unclaims
// everything outbox row from holds (Store.ReleaseKeptClaims) — from told no architect what it
// named — and writes a fresh catch-up to the architect of each tree the released rows are owed to
// now (CatchUpTheTreesOwed). That is read from the rows rather than from the row's own issue,
// because an issue the row named can have left for a tree of its own since the row was composed:
// the tree it stands in now is what owes the person an answer, and its architect is the one told
// at once. A row that holds nothing returns at once, so a row whose settle committed and whose
// finish did not writes no second catch-up when it runs again.
//
// Four finishes hand over, and only four: an architect start whose task reached no claim (the
// outbox's settleKept), a catch-up a newer one supersedes (catchUpSuperseded, the outbox's
// notice), a receipt timeout on a catch-up the re-hold finds superseded, which queues no copy
// to carry its claims on (the daemon's reholdNotice, through the same supersededKept), and a
// receipt timeout on a catch-up whose re-holds are spent, which queues no copy either while its
// architect still holds its launch (reholdNotice, through spentKept). Every other
// undelivered finish keeps the older rule — the row's deletion releases what it claimed, and the
// architect's next ready names every unclaimed decision in its catch-up. Each of those endings is
// one where the architect can take no notice at all now — its row was deleted under its lease, it
// is unroutable, its architect stopped with its finished tree or will not hold its role again, its
// holder refused the delivery or the listener's grant denied it — so a fresh catch-up would reach
// nobody either, and what the ending releases waits for that architect's next launch, whose ready
// names every unclaimed decision. A receipt timeout that draws a copy neither tells nor hands
// over: the re-held copy carries the claims on (Store.MoveKeptClaims).
func HandOverKept(ctx context.Context, tx pgx.Tx, store record.Store, steps []record.DesignStep, from int64, now time.Time) error {
	trees, err := store.ReleaseKeptClaims(ctx, tx, from)
	if err != nil || len(trees) == 0 {
		return err
	}
	return CatchUpTheTreesOwed(ctx, tx, store, steps, trees, now)
}

// CatchUpTheTreesOwed writes a fresh catch-up to the architect of each of trees that has one to
// hear it now (ArchitectHears), naming everything that tree is owed and nothing claims, for the
// launch its architect claim stands at (Store.ArchitectLaunches), so a failed forward of an older
// catch-up is not queued again behind it. A tree with no architect to hear it — no record, no
// tree root, lingering, out of the workflow, or waiting in `todo` for its slot — is written
// nothing, and what it is owed waits KEPT for its next start or ready to name, since a catch-up
// queued for nobody would only be dropped by the ready that follows. No catch-up of a hand-over
// is paced: it answers a person who is waiting, and it reaches an architect the daemon did not
// wake, so it is due at once (the zero moment QueueCatchUp reads as now). Nor does one carry the
// sentence of what a relaunch ended, which only a launch's ready knows: the prompt the claim's
// own machine sends carries that sentence whatever becomes of a catch-up.
func CatchUpTheTreesOwed(ctx context.Context, tx pgx.Tx, store record.Store, steps []record.DesignStep, trees []string, now time.Time) error {
	launches, err := store.ArchitectLaunches(ctx, tx, trees)
	if err != nil {
		return err
	}
	for _, key := range trees {
		root, err := store.Issue(ctx, tx, key)
		if err != nil {
			return err
		}
		if root == nil {
			continue
		}
		hears, err := ArchitectHears(ctx, tx, store, *root, steps)
		if err != nil {
			return err
		}
		if !hears {
			continue
		}
		if err := QueueCatchUp(ctx, tx, store, steps, *root, launches[key], "", now); err != nil {
			return err
		}
	}
	return nil
}

// ArchitectHears says whether root has an architect to hear a person's decision now: it is its
// tree's root, it does not linger, it has not left the workflow, and it does not wait in `todo`
// for an admission slot (record.WaitsForSlot, read with steps, the design-gate steps the
// deployment gates). It is the one reading of that question the daemon
// has, so the hand-over of what a row named (HandOverKept) and the hold of a root architect's task
// that names decisions (supervise's Deps.RootHears, the daemon's rootHears) never drift: a root
// that hears is told at once, and one that does not keeps what it is owed until it runs again.
func ArchitectHears(ctx context.Context, tx pgx.Tx, store record.Store, root record.Issue, steps []record.DesignStep) (bool, error) {
	if !ArchitectHearsSlotted(root) {
		return false, nil
	}
	waits, err := record.WaitsForSlot(ctx, tx, store, root, steps)
	return !waits, err
}

// ArchitectHearsSlotted is ArchitectHears for a root the caller already knows waits for no
// admission slot, which reads nothing: record.WaitsForSlot answers false for such a root, so what
// is left is the rest of that one reading. Admission asks it of the roots its selection holds
// (admit's readSlotHolders, through architectServed): each holds a slot, stands in accepting, retro or merging
// in a tree that runs, or was released for a wait in a tree that runs, and no queued root is one
// of those.
func ArchitectHearsSlotted(root record.Issue) bool {
	return claim.IsTreeRoot(root.Key, root.Tree) && !root.Lingers() && !record.OutOfWorkflow(root.Status)
}

// GatedSteps is the design-gate steps the deployment gates, in the order it names them, which
// admission reads to decide a re-admitted root's kept approval (Admission.keepStaleApproval).
func (e *Engine) GatedSteps() []record.DesignStep { return e.cfg.Steps }

// stepRef is the Dispatch reference of the document a design-gate step reviews on issue key: the
// issue's spec for the spec step, which is the reference every design decision carried before a
// deployment could gate more than one step, and the slug Dispatch files the document under for
// the other two. A person's decision is kept and relayed under it (record.ParkedDecision.Ref,
// the outbox's parkDecision), and the newest decision per reference is the one kept, so two
// steps of one root must answer to two references or each person's decision would replace the
// other's.
func stepRef(key string, step record.DesignStep) string {
	switch step {
	case record.DesignStepDecisions:
		return "dispatch://" + key + "/artifact/" + dispatch.DecisionsSlug
	case record.DesignStepCriteria:
		return "dispatch://" + key + "/artifact/" + dispatch.VerificationSlug
	}
	return "dispatch://" + key + "/spec"
}

// refStep is the design-gate step whose document ref names on issue key, the one stepRef gives
// it, and false for a reference that names no step's document (an ask's or a comment's). It is
// how a sender that holds only a decision's reference (record.ParkedDecision.Ref,
// record.Notice.Ref) finds the step SupersedingApproval asks of (RefSuperseded).
func refStep(key, ref string) (record.DesignStep, bool) {
	for _, step := range record.DesignSteps {
		if stepRef(key, step) == ref {
			return step, true
		}
	}
	return "", false
}

// ChangesSuperseded says whether notice, a design-changes-requested notice on issue, is no longer
// the changes request the gate of its document records as told, issue's gates being gates: the
// gate records another (record.DesignGate.ToldChangesVersion and ToldChangesReason). The gate
// records every changes request it applies as told (applyArtifact), and tells one a newer review
// already replaced without recording it, as the person's words, when no approval followed it; so a
// notice the gate's record does not match is of a request the person has since replaced. The
// outbox's parkDecision relays no such notice to the controller: kept, it would take the place of
// the newer decision kept under the same document's ref, and of the claim a start holds on it. A
// gate that records no request, or no gate of the notice's document, says nothing against it.
// It does not decide whether the request is still the person's word, which SupersedingApproval
// decides and parkDecision asks first (NoticeSuperseded): a request a newer one replaced
// still is, and the workflow tells it. It decides only which request holds the document's place
// among what is kept and relayed.
func ChangesSuperseded(gates map[record.DesignStep]record.DesignGate, issue string, notice record.Notice) bool {
	for step, gate := range gates {
		if stepRef(issue, step) == notice.Ref {
			return gate.ToldChangesVersion != nil && (*gate.ToldChangesVersion != notice.Version || gate.ToldChangesReason != notice.Reason)
		}
	}
	return false
}

// itemRef is the Dispatch reference of an ask or comment of an issue.
func itemRef(key, kind, id string) string { return "dispatch://" + key + "/" + kind + "/" + id }
