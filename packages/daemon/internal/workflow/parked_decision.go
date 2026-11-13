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
	outcome, err := record.RelayParkedDecision(ctx, tx, e.store, key, *root, root.Lingers(), decision, 0, "", e.cfg.Steps)
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
// architect requests approval of the latest. What it names, and what it drops because the person
// decided again since, is KeptDecisionsTask's to decide, which asks the one rule of each decision.
//
// What it may name is what root's tree is owed now: every row whose issue stands in that tree and
// that no counting claim or hold holds (Store.UnclaimedKept). A claim or a hold an issue left
// behind in another tree counts for nothing, so a child handed over, a parked child orphaned, a
// grandchild handed over beside its parent and a queued root adopted each bring what was decided
// on them to this task with nothing moved, in whatever order their admissions run.
//
// The text names no generation: a start's task waits on its claim and is prompted whenever that
// claim is next ready, which can be generations later, and the one architect session of the root
// reads it then as what a person decided on its tree. It deletes only the rows a later approval
// superseded (KeptDecisionsTask): the caller enqueues the task with the record of what it named
// (record.KeptDecisions) and claims those rows for it in the same transaction (EnqueueNaming). A
// catch-up that published tells what it named (Store.ForgetKeptDecisions) and a start's task tells
// it at the confirmation of the delivery the claim queued it as; one that reached no architect
// hands it over (HandOverKept). Both results are zero when there is nothing to name.
func ParkedDecisionTask(ctx context.Context, tx pgx.Tx, store record.Store, steps []record.DesignStep, root record.Issue) (string, *record.KeptDecisions, error) {
	decisions, approval, err := store.UnclaimedKept(ctx, tx, root.Key)
	if err != nil {
		return "", nil, err
	}
	owed := record.KeptDecisions{KeptApproval: approval}
	for _, d := range decisions {
		owed.Decisions = append(owed.Decisions, record.DecisionMark{Ref: d.Ref, At: d.At, Issue: d.Issue,
			Decision: d.Decision, Actor: d.Actor, Seq: d.Seq})
	}
	task, kept, err := KeptDecisionsTask(ctx, tx, store, steps, root.Key, owed)
	if err != nil || task == "" {
		return "", nil, err
	}
	return task, &kept, nil
}

// KeptDecisionsTask is the text that names owed to the architect of tree, and the part of owed it
// names (kept). It is the one wording a start task and a catch-up both use (ParkedDecisionTask)
// and the one a held task is written back to when the task waits to be sent (supervise's
// Deps.RootTask, the daemon's rootTask): a task that names nothing is "", which is retired rather
// than sent.
//
// It is also the one place where a kept decision is asked whether it is still the person's word,
// in the loop that writes it, from the gates as the record holds them now: each decision through
// keptReviewStands and the approval through keptApprovalStands, both of which ask
// classify.ReviewSuperseded. So no task can name a kept decision that nobody asked about. The
// approval is the root's own, and it is named only while steps gates a step
// (gates.design off asks no person), on the document of its own step
// (record.DesignStep.Document), which is the document the person reviewed and the one whose
// approval the architect requests again.
//
// A decision a later approval superseded is dropped from the task and its row is deleted in tx
// (Store.ForgetKeptDecisions): the approval is the person's word, the gate records it, and nothing
// of the decision is left to tell. If the row stays KEPT, every later start, catch-up and
// hand-over asks it again. Also, a later changes request that clears the gate's approval on a tree
// that runs, which keeps nothing, would make the old decision read as the person's word again. A
// row another tree's claim still holds is not this tree's to delete, and it stays. A dropped
// approval stays in its row as it always has (Store.KeepReadmittedApproval replaces it at the next
// re-admission). The gates a decision is asked of go when a generation is cleared, so each clear
// first forgets what they show was superseded (Engine.ForgetSupersededKept).
func KeptDecisionsTask(ctx context.Context, tx pgx.Tx, store record.Store, steps []record.DesignStep, tree string,
	owed record.KeptDecisions) (string, record.KeptDecisions, error) {
	gates := issueGates{store: store, read: map[string]map[record.DesignStep]record.DesignGate{}}
	var kept record.KeptDecisions
	var spent []record.DecisionMark
	named := make([]string, 0, len(owed.Decisions))
	for _, d := range owed.Decisions {
		read, err := gates.of(ctx, tx, d.Issue)
		if err != nil {
			return "", record.KeptDecisions{}, err
		}
		if !keptReviewStands(read, steps, d) {
			spent = append(spent, d)
			continue
		}
		kept.Decisions = append(kept.Decisions, d)
		named = append(named, fmt.Sprintf("%s (%s by %s)", d.Ref, d.Decision, d.Actor))
	}
	if len(spent) > 0 {
		if err := store.ForgetKeptDecisions(ctx, tx, tree, record.KeptDecisions{Decisions: spent}, 0); err != nil {
			return "", record.KeptDecisions{}, err
		}
	}
	if owed.Approved > 0 && len(steps) > 0 {
		read, err := gates.of(ctx, tx, tree)
		if err != nil {
			return "", record.KeptDecisions{}, err
		}
		if keptApprovalStands(read, owed.KeptApproval) {
			kept.KeptApproval = owed.KeptApproval
		}
	}
	var parts []string
	if len(named) > 0 {
		parts = append(parts, fmt.Sprintf("While nobody in %s's tree could hear it, a person decided on the tree: %s. Read each before you continue.",
			tree, strings.Join(named, "; ")))
	}
	if kept.Approved > 0 {
		parts = append(parts, fmt.Sprintf("The %s is approved at version %d and is now at version %d: request approval of version %d, "+
			"as skill://legion-architect/references/design-gate.md (\"The armed gate\") says.",
			kept.Step.Document(), kept.Approved, kept.Latest, kept.Latest))
	}
	return strings.Join(parts, " "), kept, nil
}

// keptReviewStands says whether a kept decision (mark) is still the person's word, so a task can
// name it (KeptDecisionsTask). A changes request or an approval of a design document is asked of
// the gate of that document as the record holds it now (GateOfRef over gates, the gates of
// mark.Issue) through the one rule (classify.ReviewSuperseded). An approval the gate records after
// the decision is the person's word in its place. The row keeps no version, so it is asked at the
// version the gate holds approved and at the decision's own place in Dispatch's log
// (record.DecisionMark.Seq and At, which the outbox's parkDecision keeps from the notice's
// Review). The ask mirror asks an approval request the same way (withHuman), so only the marks
// decide: the decision stands only when it comes after that approval. Both marks are Dispatch's,
// so the two are ordered by Dispatch's sequences when both carry one and otherwise by Dispatch's
// times (record.EventMark.After), never by the daemon's clock against Dispatch's. A kept approval
// that the gate's approval is (record.EventMark.Same) is no later word, so it stands. An answer on
// an ask, a carried gate's opening, a document that no gate of the issue holds, a step the
// deployment does not gate (its approval is its registration's and no person's), and a gate with
// no approval hold nothing against the decision.
func keptReviewStands(gates map[record.DesignStep]record.DesignGate, steps []record.DesignStep, mark record.DecisionMark) bool {
	if mark.Decision != record.DecisionDesignChangesRequested && mark.Decision != record.DecisionDesignApproved {
		return true
	}
	gate, ok := GateOfRef(gates, mark.Issue, mark.Ref)
	if !ok || !slices.Contains(steps, gate.Step) || gate.ApprovedVersion == nil {
		return true
	}
	review := record.EventMark{Seq: mark.Seq, At: mark.At}
	if mark.Decision == record.DecisionDesignApproved && review.Same(gate.ApprovedMark) {
		return true
	}
	return !classify.ReviewSuperseded(gate, *gate.ApprovedVersion, review)
}

// keptApprovalStands says whether the record still supports the sentence a kept approval puts in
// an architect's start (KeptDecisionsTask): the person approved kept.Approved of kept.Step's
// document, which has reached kept.Latest since, so the architect requests approval of that. The
// gate of that step as the record holds it now is asked whether an approval followed the kept one
// (classify.ReviewSuperseded), and one of a later version did: the person's word is that later
// approval, and the sentence would ask them again for what they already decided. The gate's
// approval of kept.Approved itself is the kept approval as the new generation's registration read
// it again, not a later word, so it is not asked; neither is a step with no row yet, or no
// approval, which hold nothing against the sentence.
func keptApprovalStands(gates map[record.DesignStep]record.DesignGate, kept record.KeptApproval) bool {
	gate, ok := gates[kept.Step.OrSpec()]
	if kept.Approved == 0 || !ok || gate.ApprovedVersion == nil || *gate.ApprovedVersion == kept.Approved {
		return true
	}
	return !classify.ReviewSuperseded(gate, kept.Approved, record.EventMark{})
}

// issueGates reads the gates of each issue a kept decision was made on once, for the decisions
// of one task or one clear.
type issueGates struct {
	store record.Store
	read  map[string]map[record.DesignStep]record.DesignGate
}

func (g issueGates) of(ctx context.Context, tx pgx.Tx, issue string) (map[record.DesignStep]record.DesignGate, error) {
	if read, ok := g.read[issue]; ok {
		return read, nil
	}
	read, err := g.store.GatesOf(ctx, tx, issue)
	if err != nil {
		return nil, err
	}
	g.read[issue] = read
	return read, nil
}

// ForgetSupersededKept deletes every decision kept for tree that a later approval superseded
// (keptReviewStands), in tx. Call it before a clear of a generation deletes the gates
// (record.Store.ClearGeneration, ClearTreeGeneration). Those gates are the only record of the
// approval that superseded the decision. The start or catch-up that names the decision after the
// clear asks a gate that is gone or new, and the decision would read as the person's word again:
// a person who requested changes and then approved the latest version as their hand-over would
// send the new architect to revise the spec they approved. Every row owed to tree is asked, and a
// row a queued task holds is deleted too, since that task is asked again at its send (the
// daemon's rootTask) and its gates are gone by then. A row that an outbox row claims stays
// (Store.ForgetKeptDecisions): its text is already written, and that row settles it.
func (e *Engine) ForgetSupersededKept(ctx context.Context, tx pgx.Tx, tree string) error {
	decisions, err := e.store.ParkedDecisions(ctx, tx, tree)
	if err != nil {
		return err
	}
	gates := issueGates{store: e.store, read: map[string]map[record.DesignStep]record.DesignGate{}}
	var spent record.KeptDecisions
	for _, d := range decisions {
		read, err := gates.of(ctx, tx, d.Issue)
		if err != nil {
			return err
		}
		mark := record.DecisionMark{Ref: d.Ref, At: d.At, Issue: d.Issue, Decision: d.Decision, Actor: d.Actor, Seq: d.Seq}
		if !keptReviewStands(read, e.cfg.Steps, mark) {
			spent.Decisions = append(spent.Decisions, mark)
		}
	}
	if len(spent.Decisions) == 0 {
		return nil
	}
	return e.store.ForgetKeptDecisions(ctx, tx, tree, spent, 0)
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
	row, err := record.NewOutboxRow(issue, payload, at)
	if err != nil {
		return err
	}
	if kept == nil {
		return store.Enqueue(ctx, tx, row)
	}
	id, err := store.EnqueueID(ctx, tx, row)
	if err != nil {
		return err
	}
	return store.ClaimKeptDecisions(ctx, tx, tree, *kept, id)
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
// wake, so it is due at once (the zero moment QueueCatchUp reads as now).
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
		if err := QueueCatchUp(ctx, tx, store, steps, *root, launches[key], now, time.Time{}); err != nil {
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
	if !claim.IsTreeRoot(root.Key, root.Tree) || root.Lingers() || record.OutOfWorkflow(root.Status) {
		return false, nil
	}
	waits, err := record.WaitsForSlot(ctx, tx, store, root, steps)
	return !waits, err
}

// GatedSteps is the design-gate steps the deployment gates, in the order it names them, which
// admission reads to decide a re-admitted root's kept approval (Admission.keepStaleApproval).
func (e *Engine) GatedSteps() []record.DesignStep { return e.cfg.Steps }

// GateOfRef is the gate of gates whose step's document ref names on issue key (stepRef): the gate a
// spec decision a notice carries was made on, false when no row answers to it.
func GateOfRef(gates map[record.DesignStep]record.DesignGate, key, ref string) (record.DesignGate, bool) {
	for step, gate := range gates {
		if stepRef(key, step) == ref {
			return gate, true
		}
	}
	return record.DesignGate{}, false
}

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

// itemRef is the Dispatch reference of an ask or comment of an issue.
func itemRef(key, kind, id string) string { return "dispatch://" + key + "/" + kind + "/" + id }
