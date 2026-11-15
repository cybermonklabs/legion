package workflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Each shape of the start task carries one label of its own, as testerVerdictLabel ties a task's
// text to its reader: a test reads the shape a root was given by the label its text carries. No
// label is part of another, so a task carries the label of its own shape and of no other.
const (
	// ArchitectFreshTreeLabel is in the task of a tree nobody has architected.
	ArchitectFreshTreeLabel = "its tree is open at generation"
	// ArchitectCarriedPhaseLabel is in the task of a root a hand-over carried in.
	ArchitectCarriedPhaseLabel = "the phase a hand-over carried it in at"
	// ArchitectGateRowsLabel is in the task of a root that holds design gate rows while its gate
	// is not open.
	ArchitectGateRowsLabel = "its design gate already stands part way"
	// ArchitectGateOpenLabel is in the task of a root at admitted whose design gate is open
	// (record.TreeGate.Open).
	ArchitectGateOpenLabel = "its design gate is open"
	// ArchitectReadmittedLabel is in the task of a tree that ran at an earlier generation.
	ArchitectReadmittedLabel = "ran at an earlier generation"
	// ArchitectDecompositionLabel is the sentence that asks for a decomposition. Only a tree
	// nobody has architected is asked for one, so this label stands in that shape alone.
	ArchitectDecompositionLabel = "every child of your decomposition filed"
)

// ArchitectStart is everything the first task of a root architect is derived from, which is the
// record and nothing else. The record's part of it is read by one reader (ArchitectStartOf), and
// the derivation chooses the shape, so every start of a root architect and the narrowing of a held
// task read the root alike and derive the same shape from one record: admission's promotion and
// the sweep's raise (Engine.StartArchitect), the sweep's send of an entry the derivation raised
// (Engine.SendArchitectStart) and the daemon's narrowing of a held task (NarrowRootTask). What
// they can still differ in is the record, which each reads when it runs: a root that moved between
// a start and the narrowing of its held task is narrowed into the shape it stands in then. Decided
// is the one value the reader leaves to its caller (ArchitectStartOf says why).
type ArchitectStart struct {
	// Root is the tree's root as the record holds it: its key, its tree, the generation the tree
	// is open at, the phase it stands in and whether its run has stood in a phase of its own
	// (record.Issue.TakenBack).
	Root record.Issue
	// Carried says a hand-over carried the root in at the phase it stands in, which the hand-over
	// row of the root's generation says (ArchitectCarried). The derivation reads it for a root
	// past phase.Admitted alone, so the reader reads that row for such a root alone.
	Carried bool
	// PhaseFinished says the phase a carried root stands in is over by its own terminating fact
	// (PhaseFinished): it stands in merging and its pull request has merged, so the promotion that
	// opens its tree starts no worker of that phase (admit's startMidPhaseMembers). The reader
	// reads it for a carried root alone, the one root whose task names that phase's worker.
	PhaseFinished bool
	// Gate is the steps the deployment gates and the design gate rows the root holds now
	// (record.Store.GatesOf). The zero value is a root that has registered nothing.
	Gate record.TreeGate
	// Served is the issue generation of the last task the tree's architect claim took
	// (record.Store.ArchitectServed), zero where that tree has no architect claim or its claim has
	// taken no task of a run. It is a run, which the claim's own generation is not: that counts
	// the claim's launches, and the claim is kept from one run to the next.
	Served uint64
	// Decided is what a person decided on the tree while nobody in it could hear them, as
	// KeptDecisionsTask writes it, empty where there is nothing.
	Decided string
}

// ArchitectStartOf reads from the record what the first task of root's architect is derived from
// (ArchitectStart), all but Decided: whether a hand-over carried the root in at its generation
// (ArchitectCarried) and, for such a root, whether its phase is over by its own terminating fact
// (PhaseFinished), the design gate rows it holds under steps, the design-gate steps the deployment
// gates, and the run its tree's architect claim last served. It is the one reader of them, which
// every derivation of a root architect's task reads them through.
//
// Decided is the caller's because it is what tells a task composed now from a held task narrowed.
// A task composed now names what its tree is owed and nothing claims (ParkedDecisionTask, which
// the engine reads beside this, architectStartWork). A held task names the decisions it already
// holds, and those that stay in the tree are what its narrowing writes (NarrowRootTask). The held
// task's own decisions are claimed by that task, so the unclaimed read would pass over them
// (record.Store.UnclaimedKept), and it takes a lock on every decision of the tree besides.
//
// The hand-over row is read only for a root past phase.Admitted, the one root the derivation
// reads Carried for (its first rule). At admission's promotion that row already stands for a root
// a hand-over carried in at its phase: the re-admission that carried it (admit's readmit) writes
// it at the generation it opens, in the promotion's own pass or one before it, and nothing raises
// a queued root's generation before its promotion. A root whose mark says it left another tree
// without its run (record.Issue.HandedOverFrom) holds no row, and it stands at phase.Admitted,
// where Carried decides nothing. So the row gives the promotion the answer the mark gave it, and
// every reader after the promotion, whose write clears the mark, the same answer again. The pull
// request PhaseFinished reads is read for a carried root alone, and only in merging, the one phase
// it answers for.
func ArchitectStartOf(ctx context.Context, tx pgx.Tx, store record.Store, steps []record.DesignStep,
	root record.Issue,
) (ArchitectStart, error) {
	start := ArchitectStart{Root: root, Gate: record.TreeGate{Steps: steps}}
	var err error
	if start.Gate.Rows, err = store.GatesOf(ctx, tx, root.Key); err != nil {
		return ArchitectStart{}, err
	}
	if start.Served, err = store.ArchitectServed(ctx, tx, root.Tree); err != nil {
		return ArchitectStart{}, err
	}
	if root.Phase != phase.Admitted {
		if start.Carried, err = ArchitectCarried(ctx, tx, store, root); err != nil {
			return ArchitectStart{}, err
		}
		if start.Carried {
			if start.PhaseFinished, err = PhaseFinished(ctx, store, tx, root); err != nil {
				return ArchitectStart{}, err
			}
		}
	}
	return start, nil
}

// ArchitectStartTask is what a root architect is given with the start its admission queues: the
// one thing that tells a tree's architect, in the turn its own launch starts, that the tree is its
// to take up and what the first act is.
//
// A phase worker's start carries its phase's task, so a worker the daemon launches has work the
// moment it is ready. A root architect's start carried a task only where a person had decided
// something on its tree while nobody in that tree could hear it (ParkedDecisionTask), which is
// rare: the common fresh admission launched an architect with nothing at all, and the first thing
// the daemon ever gave it was the catch-up its ready writes. That catch-up states the tree and
// ends "start or resume it as your role says", which leaves the first act to the agent to infer
// from a context it has just been born into — and an architect that infers nothing holds its tree's
// admission slot while it sits idle, with the root unclaimed and its design gate unregistered,
// until a person messages the session. So the start says it instead, and the catch-up goes on
// stating the tree.
//
// The derivation serves two roots: one standing at phase.Admitted, which is where every tree the
// workflow opens stands, and one a hand-over carried in at the phase that came with it. It gives
// every other root nothing. Which of five shapes a root it serves gets is read here rather than
// by each caller. The first rule that holds wins:
//
//  1. A hand-over carried the root in (Carried) and it is past phase.Admitted. It came with that
//     phase and the design gate it brought, so it is told to take that phase up and that it
//     decomposes nothing (internal/prompts/roles/architect-root.md, "decompose nothing and
//     release no child"). It is asked for no spec.
//  2. The root is past phase.Admitted under its own power. It stands in a working phase, waits on
//     a person, or stands in accepting, and its work is told by that phase's own role, by the
//     resume the daemon composes when a person answers, or by the notice that moved it. A
//     first-act statement would be false of it, so this writes none: the answer is Decided alone,
//     which is the empty string where a person has decided nothing, and which carries no closing
//     sentence.
//  3. The root holds design gate rows. The task reports its gate step by step. Where the gate is
//     not open it stands part way, and the architect carries it on from there. Where it is open
//     (record.TreeGate.Open), the root stands at admitted only while its first phase waits, for an
//     admission slot, an open child under it or an issue it is blocked by (a root released for
//     that wait, record.Issue.ReleasedForWaitAt, among them), and the daemon starts that phase
//     itself once nothing holds it, so that shape is one of its own.
//  4. The tree's architect claim last served a run below the root's generation (Served), or the
//     root's run has stood in a phase of its own (record.Issue.TakenBack). The tree ran at an
//     earlier generation, and re-admission cleared that generation and its design gate with it,
//     so the spec is registered again and nothing is decomposed anew.
//  5. Otherwise nobody has architected this tree, and the task asks for the whole first act.
//
// A root a hand-over carried in that stands at phase.Admitted falls past rule 1 to rules 3, 4 and
// 5. That is right: a child handed over before it ever started has no phase of its own to take up,
// and it writes its own spec like any tree nobody has architected.
//
// Two readings of rule 4 are worth stating, because they are easy to get wrong. The rule reads the
// run the claim served, and not the claim's generation, which counts its launches: the claim is
// kept from one run to the next, so after relaunches in an earlier run its count can stand above
// the root's generation, and where it was never relaunched below it, whatever the tree has run.
// And a tree whose earlier generation ended with no phase of its own leaves no TakenBack mark, so
// it falls through to rule 5 where its claim served nothing or served the run the root stands at.
// A claim that served nothing took no task of any run, so the record holds no earlier run of the
// tree for the task to carry on from. A claim that served the root's own generation took a task
// of this run, which says nothing of an earlier one.
//
// Each shape ends in a closing sentence of its own, which says what starts the tree's next work,
// and every clause of it holds of the root the shape is written for (dispatch://INFRA-2614, R3).
// Only the fresh tree is told that nothing else starts it and that no message is coming: it holds
// no gate row, so no person has a step of it to review, and a child released under its closed
// gate waits for that gate (api's WaveReleaseResponse). A carried root is told whether the
// promotion that opens its tree starts its phase's worker (admit's startMidPhaseMembers): it does,
// except where the phase is the architect's own, no role works it, or its merge has ended it. A
// root whose gate stands part way hears a person's review of a registered step as a notice. A
// root whose gate is open has its first phase started by the daemon. A tree that ran at an
// earlier generation has its members that stood mid-phase started again by its re-admission, whose
// notices reach the architect. The text is for the architect alone, and no reader of a task reads
// it back: what wrote an entry's task is a fact of the record (record.PhaseRow.EntryVerbatim, which
// SendArchitectStart reads), and whether a held task carries the first-task instruction is the
// entry it is of (NarrowRootTask).
//
// Decided leads every shape rather than replacing it, since a person's decision is read first and
// a tree that owes one still has to be taken up.
func ArchitectStartTask(start ArchitectStart) string {
	if start.Root.Phase != phase.Admitted && !start.Carried {
		return start.Decided
	}
	var task strings.Builder
	if start.Decided != "" {
		task.WriteString(start.Decided)
		task.WriteString("\n\n")
	}
	switch {
	case start.Root.Phase != phase.Admitted:
		writeCarriedPhase(&task, start)
	case start.Gate.Open():
		writeGateOpen(&task, start)
	case len(start.Gate.Rows) > 0:
		writeGateStanding(&task, start)
	case start.Served > 0 && start.Served < start.Root.Generation, start.Root.TakenBack:
		writeEarlierGeneration(&task, start)
	default:
		writeFreshTree(&task, start)
	}
	return task.String()
}

// writeFreshTree writes the task of a tree nobody has architected: the whole first act, since
// nothing of this tree has been claimed, decomposed or registered.
func writeFreshTree(task *strings.Builder, start ArchitectStart) {
	fmt.Fprintf(task, "%s is yours and %s %d, holding an admission slot while nothing in it runs. "+
		"Take it up in this turn, as your role says: claim it in Dispatch, have its root spec written and %s, then",
		start.Root.Key, ArchitectFreshTreeLabel, start.Root.Generation, ArchitectDecompositionLabel)
	writeGateAsk(task, start.Gate.Steps, "")
	task.WriteString(" Nothing else starts this tree, and no message is coming.")
}

// writeCarriedPhase writes the task of a root a hand-over carried in. It stands in the phase that
// came with it, and its workers already run from the phase records the carry brought, so it is
// asked for no spec and no decomposition (internal/prompts/roles/architect-root.md). What its
// closing sentence says turns on that phase. A phase its own terminating fact has already ended
// (PhaseFinished: merging, its pull request merged) starts no worker, since the promotion that
// opens the root's tree passes it over (admit's startMidPhaseMembers), and the root stands in it
// until the rest of its tree has left the workflow, when the daemon closes it (closeMerged,
// closeAwaitingMerge). Otherwise it turns on the role that works the phase (phase.RoleFor): the
// worker of a phase a worker works is started by that promotion, accepting is the architect's own
// phase, and a phase no role works, which is held for a carried root, starts no worker until the
// architect retries it.
func writeCarriedPhase(task *strings.Builder, start ArchitectStart) {
	fmt.Fprintf(task, "%s is yours and stands in %s, %s, with the design gate it brought. "+
		"Take it up in this turn, as your role says: claim it in Dispatch and resume the tree from that phase. "+
		"Decompose nothing and release no child.",
		start.Root.Key, start.Root.Phase, ArchitectCarriedPhaseLabel)
	if start.PhaseFinished {
		task.WriteString(" Its pull request has merged, so the daemon starts no worker for that phase, and the root stands " +
			"there until the rest of its tree has left the workflow, when the daemon closes it.")
		return
	}
	switch phase.RoleFor(start.Root.Phase) {
	case "":
		task.WriteString(" No worker works that phase, so the daemon starts none for it.")
	case claim.RoleArchitect:
		task.WriteString(" That phase is your own, so the daemon starts no worker for it.")
	default:
		task.WriteString(" The daemon starts that phase's own worker itself.")
	}
}

// writeGateStanding writes the task of a root that holds design gate rows while its gate is not
// open: where its gate stands, step by step, and then the tree taken up from there. A spec is
// registered under those rows already, so the task asks for no decomposition.
func writeGateStanding(task *strings.Builder, start ArchitectStart) {
	fmt.Fprintf(task, "%s is yours and %s: %s. "+
		"Take it up in this turn, as your role says: claim it in Dispatch, carry that gate on from where it stands "+
		"and take the tree up from there. Its own first phase starts only once its design gate opens.",
		start.Root.Key, ArchitectGateRowsLabel, gateStanding(start.Gate))
}

// writeGateOpen writes the task of a root at admitted whose design gate is open: where each step
// stands, which says why it is open, and then the tree taken up from where it stands. Such a root
// stands at admitted only while its first phase waits, so the task says what the daemon waits
// for before it starts that phase: an admission slot, which a root released for the wait holds
// none of, and the open children under it and the issues it is blocked by, which the waiting line
// reads (blockers.waitsOf).
func writeGateOpen(task *strings.Builder, start ArchitectStart) {
	fmt.Fprintf(task, "%s is yours and %s: %s. "+
		"Take it up in this turn, as your role says: claim it in Dispatch and take the tree up from where it stands. "+
		"The daemon starts its first phase itself once it holds an admission slot and neither an open child under it "+
		"nor an issue it is blocked by holds that phase back.",
		start.Root.Key, ArchitectGateOpenLabel, gateStanding(start.Gate))
}

// writeEarlierGeneration writes the task of a tree that ran at an earlier generation. Re-admission
// cleared that generation and its design gate with it, so the spec is registered again; the
// children of the old decomposition are not filed again. The task says nothing of what runs in
// the tree, since the re-admission starts again each member that stood mid-phase (admit's
// startMidPhaseMembers), and says only what the root's own first phase waits for.
func writeEarlierGeneration(task *strings.Builder, start ArchitectStart) {
	fmt.Fprintf(task, "%s is yours and its tree %s and is open again at generation %d, holding an admission slot. "+
		"Re-admission cleared the old generation and its design gate with it. "+
		"Take it up in this turn, as your role says: claim it in Dispatch, then",
		start.Root.Key, ArchitectReadmittedLabel, start.Root.Generation)
	writeGateAsk(task, start.Gate.Steps, " again")
	task.WriteString(" Decompose nothing anew. Its own first phase starts only once its design gate opens again.")
}

// writeGateAsk writes what the architect does about the design gate once the spec is written: a
// request for approval of each document the deployment gates and a registration of each of their
// steps, or, where it gates none, the one registration that opens the gate with no approval
// (skills/legion-architect/references/design-gate.md, "The steps"). again is " again" for a tree
// whose earlier generation's gate re-admission cleared, and empty for a tree registering a first
// time.
func writeGateAsk(task *strings.Builder, steps []record.DesignStep, again string) {
	switch {
	case len(steps) == 1:
		fmt.Fprintf(task, " request approval of %s and register that step of its design gate%s.", gatedDocuments(steps), again)
	case len(steps) > 1:
		fmt.Fprintf(task, " request approval of each document this deployment gates and register each of their steps of its design gate%s: %s.",
			again, gatedDocuments(steps))
	default:
		fmt.Fprintf(task, " register that spec%s at its current version, which opens the design gate with no approval.", again)
	}
}

// gateStanding says where a root's design gate stands, one clause per step, in the deployment's
// order: whether the root has registered the step, at which document version, and what a person
// has said of that version. It reads record.RequiredSteps rather than the gated list, so a
// deployment that gates no step still reports the one registration that opens its gate; under
// that deployment no person approves anything, so no clause of it names one. A changes request
// below the registered version is an answer to a document the architect has moved past, which
// leaves the current version unanswered.
func gateStanding(gate record.TreeGate) string {
	steps := record.RequiredSteps(gate.Steps)
	said := make([]string, 0, len(steps))
	for _, step := range steps {
		row, registered := gate.Rows[step]
		switch {
		case !registered:
			said = append(said, fmt.Sprintf("its %s is not registered", step.Document()))
		case len(gate.Steps) == 0:
			said = append(said, fmt.Sprintf("its %s is registered at version %d, which opened the gate with no approval",
				step.Document(), row.LatestVersion))
		case record.GateOpen(row):
			said = append(said, fmt.Sprintf("its %s is registered at version %d and a person has approved that version",
				step.Document(), row.LatestVersion))
		case row.ToldChangesVersion != nil && *row.ToldChangesVersion == row.LatestVersion:
			said = append(said, fmt.Sprintf("its %s is registered at version %d and a person has asked for changes to it",
				step.Document(), row.LatestVersion))
		default:
			said = append(said, fmt.Sprintf("its %s is registered at version %d and no person has answered it",
				step.Document(), row.LatestVersion))
		}
	}
	return strings.Join(said, "; ")
}

// gatedDocuments names every gated step's own document, in the deployment's order, as a sentence
// lists them: "its decisions document and its Verification document".
func gatedDocuments(steps []record.DesignStep) string {
	named := make([]string, len(steps))
	for i, step := range steps {
		named[i] = "its " + step.Document()
	}
	if len(named) == 1 {
		return named[0]
	}
	return strings.Join(named[:len(named)-1], ", ") + " and " + named[len(named)-1]
}

// NarrowRootTask is a root architect's held task rewritten from what it still names of its tree's
// kept decisions (the daemon's rootTask, which drops a decision whose issue has left this tree).
// The decisions' own text is written again from them, as it always was (KeptDecisionsTask), and
// where the task it rewrites carried the first-task instruction that instruction is written again
// with it: a decision leaving the tree says nothing about whether the tree has been taken up, and
// a rewrite that dropped the instruction would hand a fresh architect the empty task the
// instruction exists to replace. start is the record as it stands at the narrowing
// (ArchitectStartOf), whose Decided this writes from stays; the shape is derived again from it, so
// a root that moved between the send and the narrowing is rewritten into the shape it stands in
// now, and a root that moved past admitted under its own power, which the derivation serves no
// task, is rewritten into the kept decisions' own text alone. The empty string is a task that
// names nothing, which its caller retires rather than sends.
//
// entry is the phase entry the held task is of (supervise.Delivery.Entry), and it is what says
// whether the task was the derivation's, not the task's text. A task that holds kept decisions
// was queued by a start that claimed them (EnqueueNaming), and two starts of a root's architect
// do that: StartArchitect and SendArchitectStart's derived send, each of which carries the entry
// its task is the work of. The one other raise of an architect entry, the resume of an answered
// root (ResumeAwaitedRoot), names no kept decision, so its task is never narrowed. A task of no
// entry was queued by a release before the architect held an entry, whose start carried a task
// only where it named decisions, and whose text was those decisions alone.
func NarrowRootTask(start ArchitectStart, entry int64, stays record.KeptDecisions) string {
	start.Decided = KeptDecisionsTask(start.Root.Tree, stays)
	if entry == 0 {
		return start.Decided
	}
	return ArchitectStartTask(start)
}
