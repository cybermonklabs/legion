package record

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// RankLess compares Dispatch's fractional key, compared as bytes — the order rank.Between
// generates. Equal keys are ordered by issue key.
func RankLess(a, b Issue) bool {
	if a.Rank == b.Rank {
		return a.Key < b.Key
	}
	return a.Rank < b.Rank
}

// DoneStatus is the one Dispatch status that finishes an issue. Nothing else does: an issue
// moved to backlog or icebox has stopped, not finished, and an issue waiting on it still waits
// (blockers.Unfinished). It lives here because the rules that read a record's own status for
// the same question are here too (cycleAncestor).
const DoneStatus = "done"

// AcceptedStatus says whether Dispatch shows an issue accepted: retro, its reviewed head accepted,
// or done (DoneStatus). It is the Dispatch half of R1's first condition for a basis
// (dispatch://INFRA-3320): register_gate refuses an ancestor Dispatch shows so, and the daemon
// ends a step's wait on such an ancestor's request, wherever it reads the status.
func AcceptedStatus(status string) bool {
	return status == DoneStatus || status == "retro"
}

// OutOfWorkflowStatuses are the four Dispatch statuses that take an issue out of the workflow,
// and the one list of them: OutOfWorkflow is this list read as a predicate, so a status added
// here moves every reader of the rule with it.
//
// The list is exported because one reader cannot call the predicate at all. The pod gate is
// deliberately one statement of "still the workflow's to run" (pods.InWorkflowNotLingering),
// and it takes the status rule as a parameter rather than restating it; a `select` cannot call
// a Go predicate, so the store answer that must read that same rule over every issue of a
// project at once — the candidate list of the release of a pod no claim owns
// (store.Store.UnownedPods) — is handed this list and writes it into its filter. Handing the
// list rather than spelling the four statuses into the SQL is what keeps the rule in one place.
var OutOfWorkflowStatuses = []string{"triage", "icebox", "backlog", "done"}

// OutOfWorkflow says whether a Dispatch status takes an issue out of the workflow: every status
// but todo that no phase writes. Admission holds no slot for such an issue, and the engine leaves
// it; todo admits, and in_progress, testing, needs_review and retro are the workflow's own. It
// reads OutOfWorkflowStatuses rather than restating them, so a status and the SQL filter the
// store is handed can never disagree about one issue.
func OutOfWorkflow(status string) bool {
	return slices.Contains(OutOfWorkflowStatuses, status)
}

// InWorkflow says whether an issue is in the workflow: its Dispatch status is one the workflow
// runs in (OutOfWorkflow) and its phase is short of done. One still admitted counts, since it
// will run — a root reopened inside its linger (admit's readmit) stands in admitted with no
// role's phase until its architect reaches planning, and is running again from the reopen. It is
// the production check's "runs again" predicate (workflow's check.go) and the outbox's
// (checkLookHolds): a look of an issue that runs again would resume the checking role and
// relaunch its Sandbox under the run, so none starts.
//
// It lives here, beside the status rule it is written over, because every judgement that reads
// the daemon's own records against Dispatch's mirror needs it — the children-first wait
// (ChildOpen), the acceptance order (OpenBeneath), the state's projection and the API's
// refusals among them — and internal/workflow, where it used to live, is the one package
// several of those may not read.
//
// The rule itself is pods.Standing.InWorkflow, which the pod gate reads with the issue's linger
// (pods.InWorkflowNotLingering) and this reads without it: one statement, so a terminal phase
// added to it moves admission, the linger, the production check and the release gate together.
func InWorkflow(issue Issue) bool {
	return issue.Standing().InWorkflow(OutOfWorkflow)
}

// inWorkStatuses are the five Dispatch statuses an issue in Legion's work shows (R15 of
// dispatch://INFRA-3320): todo, which hands an issue to Legion or releases it, and the four its
// workflow writes as it runs. InLegionsWork is the one reader of the list.
var inWorkStatuses = []string{"todo", "in_progress", "testing", "needs_review", "retro"}

// InLegionsWork says whether issue, a record of the daemon's, is in Legion's work (R15 of
// dispatch://INFRA-3320): its phase is not done and its Status is one of inWorkStatuses. Status
// holds a status write the daemon has queued before Dispatch echoes it (Issue), so the daemon's
// own write counts for its own decisions as written (R16): a record the daemon has set to
// backlog is out of Legion's work while its DispatchStatus still reads in_progress, and one it
// has set to in_progress is in it while its DispatchStatus still reads backlog. The list names
// the statuses in work, where OutOfWorkflowStatuses names those out of the workflow, so a status
// Dispatch adds later is out of Legion's work until it is named here. An issue the daemon does not
// record is in no one's work: there is no record of it to ask about, and Chains reads every key it
// holds no record of as out of Legion's work.
func InLegionsWork(issue Issue) bool {
	return issue.Phase != phase.Done && slices.Contains(inWorkStatuses, issue.Status)
}

// Stopped is whether the issue's own record says its work has stopped: it is not in Legion's work
// (InLegionsWork), whatever stands above it (R20 of dispatch://INFRA-3320: the daemon acts on each
// issue's own status). It is the one statement of that rule, and the readers that must agree —
// the boot's claim release pass and the row it queues — both call it.
func Stopped(issue Issue) bool { return !InLegionsWork(issue) }

// StandsAt is the phase an issue's work stands at: the issue's own phase, or, where that phase
// is phase.Held and the record names the phase the hold came from, that phase (Hold.From). A
// worker whose claim fails leaves its issue at phase.Held with Hold.From naming the phase it was
// working (workflow's claimFailed), and the work has reached exactly where it had before.
//
// It is the one reading of where an issue stands, and the rules that must never read a held
// issue differently all take it: the phase half of the slot rule (PhaseKeepsSlot, through
// KeepsSlot) and the acceptance rules (StandsIn, and Accepted, which reads AcceptedPhase of it).
// A hold the record names no phase for answers phase.Held itself, which is all the daemon knows
// of it.
func StandsAt(issue Issue) phase.Phase {
	if issue.Phase == phase.Held && issue.Hold != nil {
		return issue.Hold.From
	}
	return issue.Phase
}

// StandsIn says whether issue stands in one of phases (StandsAt), counting a hold from one of
// them as standing there. It is the one shape every question about where an issue stands in a
// list of phases is written over — PastReview here, and the workflow's own awaitsMerge, its close
// of a merged issue and its open review round — so no two of those questions read a held issue
// differently; Accepted reads StandsAt as they do, through AcceptedPhase.
func StandsIn(issue Issue, phases ...phase.Phase) bool {
	return slices.Contains(phases, StandsAt(issue))
}

// AcceptedPhase says whether p is one of the phases past the daemon's acceptance of an issue's
// reviewed head: its retro and its merge. The two phases are named here and nowhere else, so that
// no rule of the acceptance order can read a list of its own: one that did is what let a child
// held from retro through. Accepted reads it of where an issue stands, and the workflow reads it
// of the phase a move left (workflow's acceptedEntry) and of the phase a move enters.
func AcceptedPhase(p phase.Phase) bool {
	return p == phase.Retro || p == phase.Merging
}

// Accepted says whether the daemon has already accepted issue's own reviewed head and the issue
// has not closed: it stands in its retro or in its merge, or is held from one of them
// (StandsAt, AcceptedPhase). Work entering the workflow beneath an issue in one of those phases
// would let that issue merge first, which is the one thing a parent must not do
// (dispatch://INFRA-980, "Children first"). The reviewed head of a root whose worker died in
// retro or merging is accepted still, so the hold must not let work in beneath
// it: the line held no issue under such a root, the two doors let one in, and the close of a
// root held from retro handed over nobody.
//
// Every rule of the acceptance order asks this predicate, through the nearest accepted ancestor
// (AcceptedAncestor): the waiting line's hold (HeldUnderAccepted), the doors' refusal (api's
// CHILD_UNDER_ACCEPTED_PARENT, AcceptedAncestorIn) and the wait a merged issue's close keeps
// (OpenBeneath, which leaves out what that hold stopped) — so none of them can count a root as
// accepted that another counts as still at work.
func Accepted(issue Issue) bool {
	return AcceptedPhase(StandsAt(issue))
}

// BasisAcceptance says why ancestor no longer meets the daemon's half of R1's first condition for
// a basis (dispatch://INFRA-3320), as the daemon's own store shows it now, or "" where it still
// does: what the daemon last read of Dispatch shows it done or retro (AcceptedStatus), in its
// mirror of Dispatch's issues or in its record of the ancestor; the daemon records it in its retro
// or its merge (Accepted); or the daemon has queued its close (closeQueued). Where neither the
// mirror nor the record knows the ancestor, nothing the daemon holds says that it is accepted. A
// root a person moved to the backlog or to icebox stands at phase done while its tree lingers
// (workflow's beginLinger), and R1 counts backlog and icebox as not accepted, so the phase alone
// says nothing: a real close is the status Dispatch shows, or the close the daemon queued, which
// the daemon's own close of a root (workflow's closeRoot, through writeClose) marks on the record
// (PendingClose) in the transaction that queues the status write to Dispatch, so a reader inside a
// later transaction sees the close before Dispatch's event of it comes back to the mirror. The
// answer is a sentence's subject and verb, naming the ancestor and what holds it ("Dispatch shows
// LEGION-100 done").
//
// It answers the daemon's record of the ancestor too, nil where it records none, and nil where the
// mirror alone answered, which reads no record: a caller that reads more of the record where the
// ancestor is not accepted reads it here, in the same read.
//
// It is the one reading of that half of the condition inside a transaction of the daemon's own: a
// registration on a basis reads it again under the lock every fact applies under (workflow's
// gateRegistered), so it and an ancestor's sign-off or close, whose hold reads the roots that rest
// on the ancestor (workflow's RestingRoots), apply in one order; the pending-basis seed's read and
// the person's approval as its live event read it before either opens a step; and a step carried
// onto a child keeps a basis or a rest only on an ancestor it passes (admit's carriedGate).
func BasisAcceptance(ctx context.Context, store Store, tx pgx.Tx, ancestor string) (string, *Issue, error) {
	mirrored, err := store.DispatchIssuesOf(ctx, tx, []string{ancestor})
	if err != nil {
		return "", nil, fmt.Errorf("read the Dispatch mirror of ancestor %s: %w", ancestor, err)
	}
	for _, row := range mirrored {
		if row.Key == ancestor && AcceptedStatus(row.Status) {
			return fmt.Sprintf("Dispatch shows %s %s", ancestor, row.Status), nil, nil
		}
	}
	recorded, err := store.Issue(ctx, tx, ancestor)
	if err != nil {
		return "", nil, fmt.Errorf("read the record of ancestor %s: %w", ancestor, err)
	}
	switch {
	case recorded == nil:
		return "", nil, nil
	case AcceptedStatus(recorded.DispatchStatus):
		return fmt.Sprintf("Dispatch shows %s %s", ancestor, recorded.DispatchStatus), recorded, nil
	case Accepted(*recorded):
		return fmt.Sprintf("The daemon records %s in %s", ancestor, StandsAt(*recorded)), recorded, nil
	}
	closing, err := closeQueued(ctx, store, tx, *recorded)
	if err != nil || !closing {
		return "", recorded, err
	}
	return fmt.Sprintf("The daemon has queued the close of %s as done", ancestor), recorded, nil
}

// closeQueued says whether issue carries a close the daemon queued (Issue.PendingClose) that
// still closes it: one queued at the run and phase entry the issue stands at now (IssueFence), on
// its way to Dispatch, or refused by Dispatch for a child not done while the issue is in Legion's
// work, which workflow's WriteDueClose queues again once each child is done. A close of an
// earlier run or entry closes nothing, as writeClose and WriteDueClose read it.
func closeQueued(ctx context.Context, store Store, tx pgx.Tx, issue Issue) (bool, error) {
	pending := issue.PendingClose
	if pending == nil || pending.Refused && !InLegionsWork(issue) {
		return false, nil
	}
	rows, err := store.Phases(ctx, tx, issue.Key)
	if err != nil {
		return false, fmt.Errorf("read the phases of %s for its queued close: %w", issue.Key, err)
	}
	return !IssueFence(issue, rows).Outlives(Fence{Generation: pending.Generation, Entry: pending.Entry}), nil
}

// AcceptedAncestor is the nearest issue above key in Legion's work (Chains.Above) that the daemon
// has already accepted (Accepted, which counts an ancestor held from one of those phases), and ""
// when none has been (R19 of dispatch://INFRA-3320: an issue is held with `accepted_ancestor`
// while an issue above it stands in retro or merging).
//
// The issues between are crossed whatever their status: a parent already done holds nothing
// itself — its merge has happened, so there is no order left to keep — and the grandchild under
// it is held by whichever issue above that parent is still merging, which is what the walk goes
// on to find. The hold and the acceptance rules answer alike, which is what keeps them from
// deadlocking: OpenBeneath leaves out the issues this hold has stopped, so an issue the line
// holds is an issue the ancestor's own close does not wait for.
func AcceptedAncestor(chains *Chains, key string) string {
	for _, above := range chains.Above(key) {
		if issue, _ := chains.Record(above); Accepted(issue) {
			return above
		}
	}
	return ""
}

// HeldUnderAccepted says whether the daemon is itself holding issue where it stands because an
// issue above it has already been accepted (AcceptedAncestor): it stands at phase.Admitted, so
// nothing of it has started, and the line will offer it no slot until that ancestor closes
// (Waiting). It is one rule with two readers that must agree exactly: OpenBeneath leaves such an
// issue out too, so the ancestor's own acceptance and close are not deadlocked by the issue its
// hold stopped. The hold never outlives the ancestor that caused it: once that ancestor closes it
// is out of Legion's work, the line reads this rule again and finds no hold, and the issue starts.
func HeldUnderAccepted(chains *Chains, issue Issue) bool {
	return acceptedAncestorHolding(chains, issue) != ""
}

// acceptedAncestorHolding is the ancestor whose acceptance holds issue where it stands, "" when
// none does: the rule HeldUnderAccepted answers yes or no with, written once so that the
// placement it decides and the word that reports it (HeldReasonOf's accepted_ancestor) name one
// ancestor and cannot drift apart.
func acceptedAncestorHolding(chains *Chains, issue Issue) string {
	if issue.Phase != phase.Admitted {
		return ""
	}
	return AcceptedAncestor(chains, issue.Key)
}

// LineKind is why an issue stands in the waiting line, which is also what its promotion does with
// it: the line is a line of issues now that the admission cap counts issues rather than trees, and
// each reason an issue can be in it is promoted its own way (admit's promoteHolds).
type LineKind int

const (
	// LineReturning is an issue that owes its worker a start, holds no row and waits on nothing:
	// a transition applied in full and left only that start waiting for a slot
	// (SuperviseRequest.NeedsSlot), and an issue that still held a row would have had the start
	// released by the row itself. Its promotion grants the slot and nothing else — the held
	// start becomes claimable the moment the slot row exists, and the grant's own lane wake
	// delivers it. An issue owing a start that waits on another issue stands in the line as
	// LineWaitingOnIssue instead, so its start is not delivered while the wait is open.
	LineReturning LineKind = iota + 1
	// LineAnswered is a root that waited on a person and was answered. Its work already runs;
	// its promotion resumes its architect where it stands (workflow's ResumeAwaitedRoot).
	LineAnswered
	// LineQueuedRoot is a root (Chains.IsRoot) a person handed to Legion whose first start has
	// still to come: one that never ran, one a person handed back to Legion after it finished,
	// and one handed over from another tree. Its promotion opens its lifecycle and starts its
	// architect.
	LineQueuedRoot
	// LineFirstPhase is an issue standing at admitted, in Legion's work, whose first phase can
	// start: every gate it waits for is open (LineFacts.GatesOpen). It is an issue under a parent
	// at work that Dispatch shows todo, and an issue that holds its own gate once its own work
	// comes round. Its promotion starts that phase as the gate's opening used to (workflow's
	// gateOpened), and starts a child's own architect with it (workflow's StartFirstPhase).
	LineFirstPhase
	// LineWaitingOnIssue is an issue the daemon has started that waits on another issue: a
	// Dispatch blocker of it that is not done, or an open child its own first phase waits for.
	// It holds no row — a pass took it away for the wait (Issue.ReleasedForWaitAt), or its last
	// transition left it owing a start with none — and the entry stands at the moment that mark
	// records, or at the owed row's own creation time where no mark stands (LineFacts.Owed).
	// Nothing of its work stopped, so its promotion grants it a row again and clears the mark;
	// while the wait lasts the entry says what it waits on (Waits) and the promotion passes over
	// it, as it passes over every candidate that waits on an issue.
	LineWaitingOnIssue
)

// String is the word the wire carries for this kind (api.WaitingLineView.Kind): `returning`,
// `answered`, `queued_root`, `first_phase` and `waiting_on_issue`. These five are the line's
// whole vocabulary on the wire, which a reader of the state branches on, so a kind added to the
// list above needs its word here, and a new contract with it, since a reader before it refuses
// a word it does not name (api's waitingOnIssueContract); anything else gives the empty string,
// which is no word at all.
func (k LineKind) String() string {
	switch k {
	case LineReturning:
		return "returning"
	case LineAnswered:
		return "answered"
	case LineQueuedRoot:
		return "queued_root"
	case LineFirstPhase:
		return "first_phase"
	case LineWaitingOnIssue:
		return "waiting_on_issue"
	}
	return ""
}

// LinePart is which of the waiting line's two parts an entry stands in (Waiting): the issues
// whose work came back, oldest first, and then the issues that have not started, by the board's
// rank. The kind does not give it — a LineFirstPhase entry falls in the returned part when the
// issue holds its own gate and in the starting part when it waits for a gate above it — so the
// entry carries the part the line put it in (LineEntry.Part). The zero value is no part.
type LinePart int

const (
	// PartReturned is the first part: the issues the daemon has already started whose work has
	// come back to them, placed by the moment it did (LineEntry.Since).
	PartReturned LinePart = iota + 1
	// PartStarting is the second part: the issues whose first phase has never started, placed by
	// the Dispatch rank of their top issue and then their own (LineEntry.RootRank, Issue.Rank).
	PartStarting
)

// String is the word the wire carries for this part (api.WaitingLineView.Part): `returned` and
// `starting`, the line's vocabulary as LineKind.String is for the kinds. The zero value is no
// part and gives the empty string.
func (p LinePart) String() string {
	switch p {
	case PartReturned:
		return "returned"
	case PartStarting:
		return "starting"
	}
	return ""
}

// LineEntry is one issue of the waiting line with the reason it waits there, the part of the
// line it waits in and what placed it there, and whether it already holds one of the cap's slots.
type LineEntry struct {
	Issue Issue
	Kind  LineKind
	// Part is the part of the line the entry stands in and PartPlace its place within that part
	// alone, counted from 1. Waiting sets both in its last two loops, once each part is sorted,
	// rather than where the entry was built: the kind does not say the part (LinePart), and the
	// place is the order the sort left, which is the order the line publishes and not the order
	// the loops found.
	Part      LinePart
	PartPlace int
	// Since is the moment the issue's work came back to it, which is what placed the entry in
	// the returned part: the owed start row's creation (LineReturning), the person's answer or
	// the later approval that opened its gate (LineAnswered), or the gate's opening
	// (LineFirstPhase of an issue that holds its own gate). It is the sort key itself rather than
	// a second reading of those records, and it is zero on an entry of PartStarting, whose work
	// never started.
	Since time.Time
	// RootKey and RootRank are the issue's top issue (Chains.TopIssue, R15 of
	// dispatch://INFRA-3320: the highest issue on its Dispatch chain in Legion's work, itself
	// where none stands above it) and that issue's Dispatch rank as the daemon records it
	// (Issue.Rank): with the issue's own rank they are what the starting part is ordered by
	// (RankLess over the top issue's record and then the issue's), so a reader of one entry can
	// say what placed it without the records the line was built from. They are set on an entry
	// of PartStarting alone; the returned part is ordered by Since. The projection serves them as
	// `rootKey` and `rootRank` beside the issue's own rank (api.WaitingLineView), and the sentence
	// of a first phase under a parent names the top issue.
	RootKey  string
	RootRank string
	// Holds says the issue held a slot row when the line was built, which the grant reads
	// (admit's slotFor): such a candidate is promoted without taking a second row. Three kinds
	// of entry can hold one: an answered root (LineAnswered); a root that arrived from another
	// tree with the row it held there (LineQueuedRoot), in whatever phase it carries; and a
	// first phase at admitted (LineFirstPhase) of an issue the daemon has started — an issue
	// holding its own gate whose gate has opened, or an issue a person set back to todo. This is
	// the one list of them in the daemon's code: its other comments on a holder in the line point
	// here, and the line carries the fact rather than making each reader read the slots again.
	Holds bool
	// Waits is what the issue waits on, by issue key, for an issue the daemon started and
	// released for that wait (LineWaitingOnIssue, LineFacts.Waits), and empty for every other
	// entry: a candidate of another kind is judged against the mirror rows by the rule of its
	// kind (blockers.LineWaitsOn). The line carries the fact as Holds is carried, so that the
	// promotion that passes over a candidate and the state's own report of what Legion waits for
	// answer from one judgement.
	Waits []string
}

// LineFacts is everything the waiting line is read from beyond the project's own issue records:
// the pass's one reading of the Dispatch chains, the design gates of the issues that hold their
// own gate on them, and the worker starts the project already owes. They are gathered once per
// admission pass (ReadLineFacts) and handed to every judgement that pass makes, because each of
// them is read under the one fact lock (intake.ApplyFact) on every fact of every consumer: a read
// per chain, per gate or per slot would cost the daemon a round trip for each of them on each
// fact.
type LineFacts struct {
	// Chains is the pass's one reading of the Dispatch chains of the project and of each issue
	// the pass was read for (ReadChains): every question of above, under, root, top issue and
	// gate root the line, the holds and the measures ask is asked of it, and nothing of the
	// daemon's stored tree (R15 of dispatch://INFRA-3320).
	Chains *Chains
	// Gates is the design gate of each issue the pass was read for and of each issue above one of
	// them in Legion's work that holds its own gate, by key: one entry for every such issue,
	// holding the steps the deployment gates and whatever rows that issue has registered, so that
	// an issue with nothing registered reads as the gate it is (TreeGate). Which of them an issue
	// waits for is GateHolders' answer.
	Gates map[string]TreeGate
	// Owed is the oldest pending start each issue owes its worker, by issue; an issue owing none
	// has no entry. OwedRows is every one of those rows, oldest first, which the repair of a row
	// that has outlived its fences walks (admit's finishFencedStarts).
	Owed     map[string]OwedStart
	OwedRows []OwedStart
	// Held is the unfinished blockers of each issue a Dispatch blocker holds where it stands, by
	// key: the issues whose first phase the line must not offer a slot to, however long they
	// have waited, and what each of them waits on, which the held list names beside the word
	// (HeldIssue.HeldBy). An issue nothing holds is left out of the map rather than carried with
	// an empty list, so the line's own test of it stays a test of presence. Only the caller can
	// fill it — the judgement is blockers.Holds over the project's mirror rows, and
	// internal/blockers reads this package, so this package may not read it back — and a caller
	// that fills it with nothing says that nothing is held (blockers.HeldIssues).
	Held map[string][]string
	// ChildWaits is the children-first judgement of this pass, by parent key: the issues whose
	// own first phase the line must not offer a slot to because Dispatch still shows an open
	// child under them. Only the caller can fill it — from the judgement ReadLineFacts made in
	// the transaction the line itself is never given (ChildWaitsOf) — and a caller that fills it
	// with nothing says that nothing waits for its children. It is never stored: each admission
	// pass computes it again, so a mirror row that has gone stale costs one pass and no more.
	ChildWaits map[string]ChildWait
	// Waits is what each issue the daemon has started waits on, by issue key, for the started
	// issues that wait on another issue and for no other: the two waits are the unfinished
	// Dispatch blockers of the issue and the open children its own first phase waits for, and an
	// issue with no entry waits on neither. Such an issue holds no admission slot — the pass
	// takes its row away and marks it (Issue.ReleasedForWaitAt, admit's promoteHolds), or its
	// last transition left it owing a start while it held none — and stands in the line as
	// LineWaitingOnIssue until the wait ends. Only the caller can fill it, for the reason Held
	// gives, and it is read again on every pass as ChildWaits is (blockers.startedWaits).
	Waits map[string][]string
	// StartedParentWaits is the open Dispatch children of each issue whose phases have started,
	// by parent key (readChildWaits over StartedParents): an issue in planning through
	// accepting, held from one of them, owing a start, or waiting in awaiting_children, with no
	// entry for one no open child holds. It is what the admission pass moves a started parent into
	// awaiting_children by and takes one back to the start it held by (admit's childrenWaits), and
	// the children the held word awaiting_children names (HeldReasonOf). It is apart from
	// ChildWaits, the wait of an issue whose first phase has not started: a started parent's
	// children hold no first phase, so no children-first notice, no children_first word and no
	// wait of a started issue on another issue (Waits) reads them. ReadLineFacts fills it, since
	// the judgement reads the mirror against the daemon's records in the transaction the line is
	// never given; it is never stored, and each read of the facts judges it again.
	StartedParentWaits map[string]ChildWait
	// childrenFirst is the children-first judgement ReadLineFacts made from the read of the
	// children it judged StartedParentWaits from (readChildWaits), which ChildWaitsOf hands the
	// caller to fill ChildWaits with; nil in facts ReadLineFacts did not read.
	childrenFirst map[string]ChildWait
}

// ChildWait is why one parent's own first phase waits: the open Dispatch children under it, in
// Dispatch key order. It is data alone — the rule that fills it is ChildWaits — so that the
// waiting line, the state's `blocked` and the architect's notice all read one judgement.
type ChildWait struct {
	Parent string
	Open   []OpenChild
}

// OpenChild is one Dispatch child that holds its parent's first phase: its key, the Dispatch
// status it holds it with, whether the daemon has released it, and, when a Dispatch blocked_by
// link of the child names an issue above it on its own parent chain, that ancestor. Such a cycle
// is named and nothing more: breaking the link is a person's decision.
type OpenChild struct {
	Key    string
	Status string
	// Released says the daemon records the child and that record is in the workflow
	// (InWorkflow): the child was released and the daemon runs it. An open child with no record
	// of its own is work its architect filed and has not released. It is set by ChildWaits, from
	// the same record ChildOpen reads, so that one judgement of the child answers both.
	Released bool
	Cycle    string
}

// ReadGates is the design gate of each issue among issues, by key, in one read whatever their
// number: one entry per issue, so that an issue with nothing registered is the gate it is rather
// than a missing key every reader would have to answer for itself. steps are the design-gate steps
// the deployment gates (config.Gates.Steps), the empty list gating none, and every entry carries
// them, because the gate is the list and not a row. A gate's rows are its issue's own, so the read
// is keyed by the issue and asks nothing of where it stands: a reader that holds a pass's chains
// asks LineFacts which of those gates an issue waits for (GateHolders).
func ReadGates(ctx context.Context, tx pgx.Tx, store Store, issues []Issue, steps []DesignStep) (map[string]TreeGate, error) {
	keys := make([]string, len(issues))
	for i, issue := range issues {
		keys[i] = issue.Key
	}
	return gatesOf(ctx, tx, store, keys, steps)
}

// gatesOf is the design gate of each of keys, by key, in one read whatever their number: the body
// ReadGates and ReadLineFacts are written over, so that an issue's gate is built alike wherever the
// key came from.
func gatesOf(ctx context.Context, tx pgx.Tx, store Store, keys []string, steps []DesignStep) (map[string]TreeGate, error) {
	rows, err := store.Gates(ctx, tx, keys)
	if err != nil {
		return nil, err
	}
	gates := make(map[string]TreeGate, len(keys))
	for _, key := range keys {
		gates[key] = TreeGate{Steps: steps, Rows: rows[key]}
	}
	return gates, nil
}

// readChainFacts is the pass's reading of the Dispatch chains of project, every key of issues
// named so that each of them is walked whatever its status (ReadChains), and the design gate of
// each issue of issues and of each issue above one of them in Legion's work that holds its own
// gate (gatesOf): four queries whatever the number of issues, links and gates. It is what
// ReadLineFacts and ReadRunningRoots are written over, so a gate a pass waits for is read alike by
// both.
func readChainFacts(ctx context.Context, tx pgx.Tx, store Store, project string, issues []Issue, steps []DesignStep) (*Chains, map[string]TreeGate, error) {
	keys := make([]string, len(issues))
	for i, issue := range issues {
		keys[i] = issue.Key
	}
	chains, err := ReadChains(ctx, tx, store, project, keys)
	if err != nil {
		return nil, nil, err
	}
	gated := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		gated[key] = struct{}{}
		for _, above := range chains.Above(key) {
			if chains.HoldsOwnGate(above) {
				gated[above] = struct{}{}
			}
		}
	}
	gates, err := gatesOf(ctx, tx, store, slices.Sorted(maps.Keys(gated)), steps)
	if err != nil {
		return nil, nil, err
	}
	return chains, gates, nil
}

// ReadLineFacts reads the facts of one admission pass for issues: the pass's one reading of the
// Dispatch chains and the gates on them (readChainFacts), one read of the starts the project
// owes, and both judgements of a parent's open children — the started parents'
// (LineFacts.StartedParentWaits) and the children-first waits ChildWaitsOf hands on — from the
// children the chain read already holds (readChildWaits), whatever the number of chains, gates
// and issues involved (R16 of dispatch://INFRA-3320: one pass reads the copy in a number of
// queries that does not grow with the number of issues or links). project is the Dispatch
// project key and steps the design-gate steps the deployment gates.
func ReadLineFacts(ctx context.Context, tx pgx.Tx, store Store, project string, issues []Issue, steps []DesignStep) (LineFacts, error) {
	chains, gates, err := readChainFacts(ctx, tx, store, project, issues, steps)
	if err != nil {
		return LineFacts{}, err
	}
	rows, err := store.OwedStarts(ctx, tx, project)
	if err != nil {
		return LineFacts{}, err
	}
	owed := make(map[string]OwedStart, len(rows))
	for _, row := range rows {
		// The read is oldest first, and an issue that owes two starts — one the lane holds a
		// lease on and one a later transition wrote — keeps the place the first one took.
		if _, seen := owed[row.Issue]; !seen {
			owed[row.Issue] = row
		}
	}
	facts := LineFacts{Chains: chains, Gates: gates, Owed: owed, OwedRows: rows}
	// The children are judged last, since the gates say which issues are children-first
	// candidates and an issue owing a start is a started parent (StartedParents).
	facts.childrenFirst, facts.StartedParentWaits = readChildWaits(issues, facts)
	return facts, nil
}

// GateHolders is the issues above key whose design gates key waits for, nearest first (R19 of
// dispatch://INFRA-3320): each issue above it in Legion's work that holds its own gate
// (Chains.HoldsOwnGate), whatever the status of the issues between. Their gates are among the
// pass's (Gates).
func (f LineFacts) GateHolders(key string) []string {
	var holders []string
	for _, above := range f.Chains.Above(key) {
		if f.Chains.HoldsOwnGate(above) {
			holders = append(holders, above)
		}
	}
	return holders
}

// ShutGates is the issues whose gate holds key's work where it stands, nearest first, and nothing
// where every gate it waits for is open. A root waits for its own design gate to open
// (TreeGate.Open), which its architect's design opens. An issue under a parent waits for each
// gate step it holds itself that does not stand open — a step registered on it, whether it ran
// as a root or not — and not for a design its architect never registered: an issue a person
// queued as a root and that never started holds no step, and under a parent at work it starts on
// todo (R19). And every issue waits for the gate of each gate holder above it (GateHolders) that
// is not open.
func (f LineFacts) ShutGates(key string) []string {
	var shut []string
	if f.Chains.IsRoot(key) {
		if !f.Gates[key].Open() {
			shut = append(shut, key)
		}
	} else {
		for _, row := range f.Gates[key].Rows {
			if !GateOpen(row) {
				shut = append(shut, key)
				break
			}
		}
	}
	for _, holder := range f.GateHolders(key) {
		if !f.Gates[holder].Open() {
			shut = append(shut, holder)
		}
	}
	return shut
}

// GatesOpen says whether every gate key's work waits for stands open (ShutGates).
func (f LineFacts) GatesOpen(key string) bool { return len(f.ShutGates(key)) == 0 }

// Queued says whether issue waits in the line for its first start as a root (QueuedRoot): its own
// record and design gate say its first start has still to come, and it is a root on the pass's
// chains (Chains.IsRoot). The record is read first, so an issue whose record already says no
// costs no walk of its chain.
func (f LineFacts) Queued(issue Issue) bool {
	return QueuedRoot(issue, f.Gates[issue.Key]) && f.Chains.IsRoot(issue.Key)
}

// AtWork is AtWork for an issue of the pass: whether it holds its own gate is the pass's chains'
// answer (Chains.HoldsOwnGate) and its gate is its own (Gates).
func (f LineFacts) AtWork(issue Issue) bool {
	return AtWork(issue, f.Chains.HoldsOwnGate(issue.Key), f.Gates[issue.Key])
}

// GateOpen is whether an approval stands on the gate's current document version: a person's, or
// the ancestor's approval a basis opened the step on, which sets ApprovedVersion to the version the
// root registered (OpenOnBasis), so this one rule reads both. It is the one statement of it, which
// classify.DesignGateOpen and Store.PutGate's own write both call. internal/classify reads this
// package, so the rule lives here, where every reader can reach it.
func GateOpen(gate DesignGate) bool {
	return gate.ApprovedVersion != nil && *gate.ApprovedVersion == gate.LatestVersion
}

// RequiredSteps is the steps a root must hold an open gate at before its tree works: the steps
// the deployment gates, in the order it names them, or the single step spec when it gates none.
// It is the one statement of the rule, which classify.RequiredSteps calls; internal/classify
// reads this package, so the rule lives here, where every reader can reach it.
//
// The empty list keeps its own hold. A deployment that gates no step asks no person to approve
// anything, and its registration opens the gate at once; but the registration itself is still
// what says the architect has written the spec, and a rule reading "every gated step is open"
// would be true of a tree whose architect had registered nothing and would start it. So the
// empty list requires the spec step's record, which the registration writes and opens in one act.
func RequiredSteps(configured []DesignStep) []DesignStep {
	if len(configured) == 0 {
		return []DesignStep{DesignStepSpec}
	}
	return configured
}

// TreeGate is one tree's design gate as every rule over it reads it: the steps the deployment
// gates (Steps, the empty list standing for the single step spec, RequiredSteps) and the row the
// tree's root holds for each step its architect has registered (Rows, nil for a root that has
// registered none). The gate is a list of steps and the work waits on all of them, so a rule
// that used to read one row reads this instead and no reader can answer from one step where the
// deployment gates three.
//
// The zero value is the gate of a root with nothing registered, which is the answer a reader
// wants for a tree the pass holds no gate row for: it is open at no step and has opened at none.
type TreeGate struct {
	Steps []DesignStep
	Rows  map[DesignStep]DesignGate
}

// Open is whether the gate lets the tree's work run: an approval stands on the current document
// version of every required step (GateOpen), a person's or the one a basis opened the step on. A
// required step with no row holds the work, as an unregistered gate always has, so a root with no
// row at all is held too. It is the one rule for the question, which classify.DesignGatesOpen
// reads it through.
func (g TreeGate) Open() bool {
	for _, step := range RequiredSteps(g.Steps) {
		if row, registered := g.Rows[step]; !registered || !GateOpen(row) {
			return false
		}
	}
	return true
}

// EverOpened is whether every required step's gate has stood open at some point
// (DesignGate.EverOpened), which is what says the root's architect has had its whole design
// approved once: after that the root stands at admitted with its own first phase waiting in the
// line, and its children do the work. A step with no row has never opened, so a gate the
// architect has still to finish registering has not either.
func (g TreeGate) EverOpened() bool {
	for _, step := range RequiredSteps(g.Steps) {
		if row, registered := g.Rows[step]; !registered || !row.EverOpened {
			return false
		}
	}
	return true
}

// OpenedAt is when a root's work came back to it through its design gate: the moment the last of
// its required steps opened, zero for a gate that is not open. A step a person's approval opened
// gives where Dispatch recorded that approval (DesignGate.ApprovedMark); a step a basis opened
// gives the daemon's own time of the registration (GateBasis.RegisteredAt), never a zero time,
// since the ancestor's approval is a place in another issue's event log. It is the waiting line's
// ordering key for a root whose own first phase can start, merged on the same time axis as an
// owed start row's creation and an answered root's answer. A step whose approval carries no
// moment — one a registration read from Dispatch rather than from an event — gives the zero
// time, which puts that root at the head of its part when it is the only step: the line knows
// its work came back and not when, and the alternative, dropping it to the back by rank, loses
// what it does know.
func (g TreeGate) OpenedAt() time.Time {
	if !g.Open() {
		return time.Time{}
	}
	var at time.Time
	for _, step := range RequiredSteps(g.Steps) {
		at = later(at, g.Rows[step].openedAt())
	}
	return at
}

// later is whichever of two moments is the later one.
func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// AtWork says whether a worker of issue has work to do in the phase the issue stands in. It is
// not the admission rule: an issue keeps one of the cap's slots through phases no worker of it
// works (KeepsSlot), unless it waits on another issue (blockers.waitsOf), so this answers a
// narrower question than the cap asks. holdsGate is whether the issue holds its own design gate,
// the chain's answer (Chains.HoldsOwnGate, R15 of dispatch://INFRA-3320), and gate is that gate,
// the zero value for an issue with no step of it registered; a reader that holds a pass's facts
// asks LineFacts.AtWork, which reads both.
//
// Four readers are left. Three ask about a worker rather than about the cap: CanWaitOnPerson,
// which lets a root hand work to a person only while its architect has work to hand over;
// admit's resumeSlottedWorkers, which starts the phase worker of an issue that has lost its task
// and must pass over one whose phase has no worker at all; and workflow's unpromptedPhaseWorker,
// which finds a live phase worker nothing will prompt. The fourth, blockers.waitsOf, asks it
// of a started issue at phase.Admitted to decide a slot release: an issue holding its own gate
// there whose design gate has never opened is its architect at work, so it waits on nothing and
// keeps its row.
//
// Planning, implementing, testing, reviewing and retro each have a phase worker with work.
// Merging has one too, and keeps it: its merger's own merge is what ends the phase, so there is
// no armed READY waiting on a person here any more. Admitted is work only for an issue that holds
// its own gate whose gate has never opened at every step the deployment gates, where its
// architect is writing the design; after that opening the issue's own turns start no phase
// worker, and the issues under it do the work. Every other phase is a wait: an issue under a
// parent at admitted, accepting, held, awaiting_person, awaiting_children, done.
func AtWork(issue Issue, holdsGate bool, gate TreeGate) bool {
	switch issue.Phase {
	case phase.Planning, phase.Implementing, phase.Testing, phase.Reviewing, phase.Retro:
		return true
	case phase.Merging:
		// The merger works this phase, so an issue in it is at work. Its own merge is what ends
		// it: there is no armed READY to wait for here any more.
		return true
	case phase.Admitted:
		return holdsGate && !gate.EverOpened()
	default:
		return false
	}
}

// PhaseKeepsSlot is the one list of the phases that hold one of the cap's slots: admitted, where
// a root's architect writes its design and a started child waits to be entered again;
// awaiting_person; the four phases a worker works up to the review — planning, implementing,
// testing and reviewing; and held, which is reached here only where the record names no earlier
// phase for the hold (StandsAt). Every other phase holds none: accepting, retro and merging each
// stand past the issue's review, done has finished, and awaiting_children is a parent waiting on
// its open children, which work under slots of their own (dispatch://INFRA-3320 R11).
//
// It is written as one list so that a later change adds or removes one entry and every reader of
// the rule moves with it. Two read it, and a phase in one and not the other would be a deadlock:
// the cap's own half (KeepsSlot) and the mark a start of a phase worker carries for the slot it
// waits for (workflow.StartOrOwe), whose row the outbox holds unclaimable while the issue holds
// no slot. PastReview is the complement of the three phases past the review that this refuses,
// for the repair that must reach an issue holding no row.
func PhaseKeepsSlot(at phase.Phase) bool {
	switch at {
	case phase.Admitted, phase.AwaitingPerson, phase.Planning, phase.Implementing, phase.Testing,
		phase.Reviewing, phase.Held:
		return true
	default:
		return false
	}
}

// KeepsSlot is the phase half of the admission rule: an issue holds one of the cap's slots from
// the moment the daemon starts it until its work is past its review — accepting, retro and
// merging hold none — or until it is done or Dispatch takes it out of the workflow
// (InWorkflow). Every phase up to the review keeps the row — admitted, awaiting_person, the four
// worker phases and a hold the record names no phase for (PhaseKeepsSlot) — so an issue waiting
// for a person or for its review keeps its place against the cap and wins none back from the
// line when the wait ends. A wait on another issue is the one wait that is not read here: an
// issue the daemon has started that waits on one gives its row back and stands in the line for
// one again, by the rule beside this (blockers.waitsOf, admit's releaseSlotsForWaits), save the
// roots that rule names as keepers. That rule reads this one first, so an issue past its review
// waits on nothing there (blockers.waitsOf). The rule whole is "Admission and the `legion` label"
// in docs/site/src/content/docs/legion/concepts.md.
//
// The cap bounds the work in progress, and an issue whose review has passed writes no more of
// the change: its sign-off, its retro and its merge run on no slot at all, so their starts are
// claimable the moment they are written (workflow.StartOrOwe) and a tree's last phases count
// against none of the issues that have a change still to write. Work the architect sends back
// from accepting or from retro to implementing is at work again, so it takes a slot again, and
// it waits for one among the issues whose work came back. A merged root stands in merging until
// the rest of its tree leaves the workflow (workflow's closeMerged), and it stands there on no
// slot, as every phase past the review does.
//
// Where the issue stands is StandsAt's reading and not the record's field alone, so an issue a
// failed claim held keeps exactly the row the phase it was working keeps.
//
// It is the one statement of that half, and the two rules that must never disagree read it: the
// release (NeedsSlot, which admit's releaseSlots asks of every issue holding a row) and the
// grant (admit's slotFor, which asks it of every candidate of the waiting line that holds no
// row). Written apart, one pass could grant a row the next pass takes away, and the issue would
// queue again for the slot it had just been given.
//
// Whether the issue is in the workflow at all is InWorkflow's one statement, which this reads
// rather than writing a second copy of: the linger InWorkflow leaves out is read by no part of
// the slot rule either, and NeedsSlot asks the tree's own terms beside this.
func KeepsSlot(issue Issue) bool {
	return InWorkflow(issue) && PhaseKeepsSlot(StandsAt(issue))
}

// PastReview says whether issue's work stands past its own review: in accepting, in its retro
// or in its merge, or held from one of them (StandsIn, as Accepted is written). Those are
// exactly the three phases that hold none of the cap's slots (PhaseKeepsSlot), so this is that
// list read the other way round.
//
// It exists so that the list lives in this package alone. The repair of a worker with no task
// (admit's resumeSlottedWorkers) must reach an issue standing past its review, because such an
// issue holds no row and the repair's own domain is the slot holders. Written there as a list of
// its own, a phase added to or taken out of PhaseKeepsSlot would move the cap and leave the
// repair behind, and the worker of that phase would be repaired by nothing. Read from here, the
// two move together.
//
// It is not Accepted, and the two must not be merged: Accepted names the two phases whose
// reviewed head the daemon has accepted, which is the rule the acceptance order is written over
// (its doc names them there and nowhere else), while this one is the slot rule's complement and
// counts accepting with them.
func PastReview(issue Issue) bool {
	return StandsIn(issue, phase.Accepting, phase.Retro, phase.Merging)
}

// NeedsSlot says whether issue must hold one of the cap's slots now: it is in Legion's work
// (InLegionsWork) and keeps a slot where it stands (KeepsSlot). An issue holds its admission slot
// from the moment the daemon starts it until its work is past its review, until it is done or
// until Dispatch shows it out of Legion's work, except while it waits on another issue before
// that point (docs/site/src/content/docs/legion/concepts.md, "Admission and the `legion` label"),
// so the cap counts the issues the daemon has started whose change is still to write, rather than
// the ones a worker of them is running this second. Only the issue's own status is read (R20 of
// dispatch://INFRA-3320): a person's park, close or icebox of the issue frees its slot, and the
// status of an issue above it frees nothing.
//
// The status is the record's own (Issue.Status), which holds a status the daemon has written
// before Dispatch echoes it (InLegionsWork), so a pass that reads the record between the daemon's
// own park and its echo frees the slot already.
//
// It is the one statement of the release of an issue that needs no slot at all, and the two
// readers that must never disagree both call it: admission releases the slot of every issue it
// answers false for (admit's releaseSlots), and the waiting line offers a returning issue a slot
// only where it answers true (Waiting). The other release, of a started issue that goes on
// working and waits on another issue, is not stated here: it is admit's releaseSlotsForWaits,
// judged by blockers.waitsOf. This answers true of such an issue all the same, so the issue
// keeps its place in the line while it holds no slot. The design gate is read by no part of
// this: a gate open or closed changes neither whether the issue is in work nor whether its phase
// keeps a row (KeepsSlot). The slot is accounting only: taking one away suspends no claim, moves
// no phase and writes no Dispatch status.
func NeedsSlot(issue Issue) bool {
	return InLegionsWork(issue) && KeepsSlot(issue)
}

// CanWaitOnPerson says whether root may enter awaiting_person now: it is in Legion's work,
// standing at admitted, it holds its own gate and its architect has work there (AtWork, over
// holdsGate and gate, its own design gate), and the daemon has started it, which is what the slot
// row it holds says (slotted). The row no longer says a worker of the issue has work — an issue
// keeps it while it waits on a person, and while it waits on its review unless it also waits on
// another issue (KeepsSlot, blockers.waitsOf), and a root like this one, at admitted with a
// design gate that has never opened, keeps it while it waits on another issue too
// (blockers.waitsOf) — it says this root is one of the issues the daemon has started and not yet
// finished with. A root with no row was never started and has no work of its own to hand to
// anybody.
//
// At admitted, AtWork is true for an issue holding its own gate whose design gate has never
// opened — the architect writing the spec, which holds the whole of the work under it — so the
// gate is read here too: an issue whose gate has opened has handed the work to the issues under
// it, and its own first phase waits in the line rather than on a person.
//
// It is the one statement of the rule, and the two readers that must never disagree both call
// it: the pass that starts a wait (workflow's decideAwait, rule 0) and the boot's catch-up of
// the roots that pass can move (the daemon's catchesUp). A root this answers false for starts no
// wait on a person, so the boot need not read its Dispatch window either.
//
// It is not NeedsSlot read backwards: NeedsSlot says what the cap must hold a row for, of every
// issue, from the record alone, while this says what one root may hand to a person, and asks the
// row itself. The wait it starts keeps that row, since an issue in awaiting_person is an issue
// the daemon has started (KeepsSlot).
func CanWaitOnPerson(root Issue, holdsGate bool, gate TreeGate, slotted bool) bool {
	return slotted && root.Phase == phase.Admitted && InLegionsWork(root) && AtWork(root, holdsGate, gate)
}

// WaitingLineBand is where the line puts an issue whose work came back: in its first part, oldest
// first (Waiting). It states a placement, never a position: which place in the line that gives an
// issue depends on what else waits. The texts that state the line's order carry this fragment
// word for word — the daemon's own comments, the architect's skills and the operator's guides —
// and workflow's TestTheShippedWordsNameTheWaitingLineBand reads it from here and holds the
// files it lists to it. That test reads whether a file carries the words, never whether the
// sentence around them is true, which is why it also bans, in every one of those files, the
// wording that claims a position instead.
const WaitingLineBand = "among the issues whose work came back, oldest first"

// Waiting is the line of issues that wait for an admission slot, in the order admission gives
// them one. The cap counts the issues the daemon has started, so an issue under a parent waits in
// this line as a root does, and the line has two parts (R19 of dispatch://INFRA-3320).
//
// First, by when their work came back, oldest first: the issues the daemon has already started
// whose work has come back to them. Four kinds of record say that work came back, and each
// gives the moment it did:
//
//   - an issue that owes its worker a start and waits on nothing takes that row's own creation
//     time and id (OwedStart), which is durable — the transition wrote the row in its own
//     transaction — so a restart finds the line exactly as it was, and a second transition
//     before the slot writes a start of its own carrying the later task and that row's creation
//     time, with the earlier row taken out, which leaves the issue where it stood
//     (Store.EnqueueStart);
//   - a root a person answered while it waited in awaiting_person takes its Awaiting.AnsweredAt;
//   - an issue that holds its own gate (Chains.HoldsOwnGate), standing at admitted with every
//     gate it waits for open, whose first phase can start, takes the moment its own gate opened
//     (TreeGate.OpenedAt). It holds the row it was started with — an issue the daemon has
//     started keeps one (KeepsSlot) — since one that waits on another issue, an open child of
//     its own or an unfinished blocked_by link, has given its row back and stands in the line by
//     the next kind instead. So its first phase is work coming back rather than work that never
//     started, and its promotion takes no further slot.
//   - an issue the daemon started that waits on another issue takes the moment its row was
//     taken away for that wait (Issue.ReleasedForWaitAt, LineWaitingOnIssue), or, where it owes
//     a start and carries no mark, that owed row's own creation time and id. It holds no row
//     while it waits, which is the whole of the rule: a started issue that waits holds none of
//     the cap's slots and stands in this line for one again, and until its wait ends its
//     entry's own answer passes it over (LineEntry.Waits), the start it owes with it.
//
// Where a root has both an answer and an open gate, the later of the two is when its work came
// back. The four are merged on the one time axis, and ties are broken by the owed row's id and
// then the issue key. A limit on work in progress works by finishing started work first, so work
// that was in progress goes before work that never started.
//
// Then, by the Dispatch rank of their top issue (Chains.TopIssue, RankLess over its record) and,
// within one top issue's rank, by their own: the issues whose first phase can start and that
// hold no slot row. These are the roots a person handed to Legion whose first start has still to
// come (Queued), and the issues under a parent in Legion's work that Dispatch shows todo,
// standing at admitted with every gate above them open (GatesOpen), whether release_children or a
// person wrote the todo and whether or not they carry the label. Each takes its top issue's place
// in the board's order, so the issues under a started top issue go before the roots ranked below
// it, and a root ranked above it is not kept out by a top issue that keeps filing children.
//
// An issue whose chain is unknown (Chain.Known: a row the copy lacks, a loop, more than 32 links)
// starts nothing (R16): it stands in no part of the line until its chain is read right. An
// unanswered awaiting root waits on the person rather than a slot, whatever Dispatch status it
// currently shows, so it is not in the line until it is Answered. A root without the label waits
// for nothing. An issue under a parent is held where it stands, and waits for nothing here, in
// three cases that the chain alone decides (R19): its parent is not in Legion's work and an issue
// above the parent is (ancestry), its top issue waits on a person (root_awaiting_person), or an
// issue above it stands in its retro or its merge (HeldUnderAccepted). The other hold, on
// Dispatch blocked-by links, takes an issue out of here only where it holds it where it stands:
// an issue whose first phase would start waits at admitted until the last issue it names is done
// (LineFacts.Held, blockers.Holds), so the line never offers it a slot. A root's links are read at
// its promotion instead, which passes it over and keeps its place (blockers.WaitsOn); either way
// the state names it under admission.blocked.
//
// A root is in the line as a root that never ran (LineQueuedRoot) only while its own record says
// the generation it carries has not run — it stands at admitted with its design gate never
// opened, or it arrived from another tree carrying the phase it ran there (ran). So a person's
// todo on a running root puts it back in no queue: the root whose gate has opened waits here for
// its own first phase, and the root in accepting, in merging or at work waits for nothing. The
// slot does not say it either, for the reason the queued-root loop states at the skip it guards
// below.
//
// An issue holding a slot row is in the line where its promotion needs more than that row, the
// three kinds LineEntry.Holds names, and each is promoted without taking a second slot: the
// entry says it holds one, and admit's slotFor reads that. A row does take an owed start out of
// the line, because the row is what releases that start (SuperviseRequest.NeedsSlot) and an issue
// holding one waits for nothing here, and it takes out a queued root the daemon granted that row,
// whose architect is writing the spec and whose second promotion would start a second architect.
// An issue out of Legion's work is out of the line too (NeedsSlot): nothing the daemon or a person
// stopped is started again until Dispatch shows it in work.
//
// Beside the line it answers with a held row for every candidate it leaves behind, the word for
// why coming from the one ladder (HeldReasonOf) rather than from the site that drops it, so one
// issue reads one word (C1). The skips that place a candidate, and the one that passes over an
// issue released for a wait, which the first loop has placed already (LineWaitingOnIssue),
// report nothing, so such an issue stands in the line and in no held row. Every other skip
// reports. Two of them are deferrals — a queued root the second loop may place, and an answered
// root the first loop places — and a deferral reports a row the other loop may then place:
// HeldList drops a row for a key the line names, so a deferral that is placed costs one row and
// no wrong answer.
func Waiting(issues []Issue, slots []Slot, facts LineFacts) ([]LineEntry, []HeldIssue) {
	slotted := make(map[string]struct{}, len(slots))
	for _, slot := range slots {
		slotted[slot.Issue] = struct{}{}
	}
	chains := facts.Chains
	// Every drop site reports what it left behind through one ladder, read over the facts the
	// line itself is judged on, so that the word an issue reads here and the word the assembly
	// reads for an issue no site saw are read from one set of facts (HeldList).
	var heldRows []HeldIssue
	hold := func(issue Issue) {
		reason, heldBy := HeldReasonOf(issue, facts)
		heldRows = append(heldRows, HeldIssue{Issue: issue.Key, Reason: reason, HeldBy: heldBy})
	}
	queued := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		if facts.Queued(issue) {
			queued[issue.Key] = struct{}{}
		}
	}

	type timed struct {
		entry LineEntry
		at    time.Time
		id    int64
	}
	var returned []timed
	// The issues this first loop puts in the line as LineWaitingOnIssue, which the branches
	// below skip so that the line carries each issue once: an issue released for a wait stands
	// at admitted as often as not, where the first-phase branch would reach it again.
	releasedForWait := make(map[string]struct{})
	for _, issue := range issues {
		if issue.Answered() {
			// An answered root belongs to this part whether or not it holds a slot row: its wait
			// kept that row (KeepsSlot), and its place is the moment its work came back, which is
			// its answer or, when a person's approval opened its gate later than that, the
			// approval.
			_, holds := slotted[issue.Key]
			returned = append(returned, timed{entry: LineEntry{Issue: issue, Kind: LineAnswered, Holds: holds},
				at: later(*issue.Awaiting.AnsweredAt, facts.Gates[issue.Key].OpenedAt())})
			continue
		}
		if _, waitingAsRoot := queued[issue.Key]; waitingAsRoot {
			// A root whose first start has still to come is in the second part whatever rows it
			// owes: its promotion starts it, and the starts of a run it carries are queued then.
			hold(issue)
			continue
		}
		// An issue that already holds a row waits for nothing here: the row is what releases
		// the start it owes (SuperviseRequest.NeedsSlot), so its worker is given that task
		// without the line, and a place in the line would only take a slot from an issue
		// holding none.
		if _, slotHolder := slotted[issue.Key]; slotHolder {
			hold(issue)
			continue
		}
		// The issue must need a slot where it stands (NeedsSlot), the same question the release
		// of one asks, so that no issue the release would free is offered one here: an issue
		// that owes a start its phase has moved past — a run that ended before its slot came,
		// whose row the repair pass finishes (admit's finishFencedStarts) — waits for nothing
		// and must not take a slot from an issue that does.
		if !NeedsSlot(issue) {
			hold(issue)
			continue
		}
		// An issue the daemon has started that waits on another issue stands here as the waiting
		// kind, whatever else it owes: its entry carries what it waits on, the promotion passes
		// over it until that wait ends (LineEntry.Waits), and nothing of its work moves
		// meanwhile. Two records say such an issue has started while it holds no row, and each
		// gives the moment its entry stands at: the mark of the row a pass took away for the
		// wait (Issue.ReleasedForWaitAt), and, where no mark stands, the start it owes its next
		// worker, which a transition of a run writes, or SendOwedEntry for an issue that already
		// has a claim or a phases row, and whose row carries its own creation time and id, as a
		// return's entry does. An issue that never started has neither — an issue at admitted
		// waiting for its first phase had no row to give back and owes nothing — so none is a
		// candidate here.
		owed, owes := facts.Owed[issue.Key]
		marked := issue.ReleasedForWaitAt != nil
		if _, waits := facts.Waits[issue.Key]; marked || (owes && waits) {
			at, id := owed.CreatedAt, owed.ID
			if marked {
				at, id = *issue.ReleasedForWaitAt, 0
			}
			releasedForWait[issue.Key] = struct{}{}
			returned = append(returned, timed{entry: LineEntry{Issue: issue, Kind: LineWaitingOnIssue,
				Waits: facts.Waits[issue.Key]}, at: at, id: id})
			continue
		}
		if !owes {
			hold(issue)
			continue
		}
		// This issue holds no row and waits on nothing: the skip above took every holder out of
		// the line, so the entry says so and its promotion takes one for it.
		returned = append(returned, timed{entry: LineEntry{Issue: issue, Kind: LineReturning}, at: owed.CreatedAt, id: owed.ID})
	}

	var starting []LineEntry
	for _, issue := range issues {
		if _, waitingAsRoot := queued[issue.Key]; !waitingAsRoot {
			continue
		}
		// A queued root holding a slot row is one the daemon has already started — a root whose
		// architect is writing the spec, which a person set back to todo, or a root a hand-over of
		// the release before carried in (CarriedIn) that its promotion started — and a second
		// promotion would start a second architect, so a root that holds a row is never a queued
		// root. It is the one statement of that reasoning: the doc above, QueuedRoot, ran and
		// AwaitsPromotion, which reads it, each point here rather than repeating it.
		_, holds := slotted[issue.Key]
		if !AwaitsPromotion(issue, facts.Gates[issue.Key], holds) {
			hold(issue)
			continue
		}
		starting = append(starting, LineEntry{Issue: issue, Kind: LineQueuedRoot, Holds: holds})
	}
	for _, issue := range issues {
		// An issue the first loop has already put in the line, waiting on another issue, stands
		// there once: its promotion grants it the row back and nothing else, and the pass that
		// reads the line after it offers the first phase this branch would. It is passed over
		// before any skip below can report it, since it stands in the line and takes no held
		// row. A queued root never reaches that branch (the first loop's own skip above), so
		// this is the one place it can double.
		if _, waits := releasedForWait[issue.Key]; waits {
			continue
		}
		if issue.Phase != phase.Admitted || issue.AwaitingPerson() || !InLegionsWork(issue) {
			hold(issue)
			continue
		}
		if _, waitingAsRoot := queued[issue.Key]; waitingAsRoot {
			hold(issue)
			continue
		}
		// An issue whose chain is unknown starts nothing (R16): neither as a root nor under a
		// parent, since the chain is what says which of the two it is. The re-read of the chain
		// is what moves it.
		chain := chains.Chain(issue.Key)
		if !chain.Known() {
			hold(issue)
			continue
		}
		// A root nothing has handed to Legion waits for a person's handover (the label, R19 of
		// dispatch://INFRA-3320): one still at todo, which the daemon never started, takes no
		// slot here. One the daemon started goes on, since taking the label off an issue at work
		// stops nothing.
		if !issue.HandedOver && issue.Status == "todo" && chains.IsRoot(issue.Key) {
			hold(issue)
			continue
		}
		_, holds := slotted[issue.Key]
		// An issue under a parent at work starts once Dispatch shows it todo, and only while its
		// parent is in Legion's work: under a parent out of work with an issue above that parent
		// in work, it is that issue's unreleased work and waits (the ancestry hold, R19).
		if above := chains.Above(issue.Key); len(above) > 0 && above[0] != chain.Links[0] {
			hold(issue)
			continue
		}
		// A top issue that handed work to a person starts nothing under it while it waits: the
		// issues under it start with its resume (admit's promotion of the answered root).
		if top, _ := chains.Record(chains.TopIssue(issue.Key)); top.AwaitingPerson() {
			hold(issue)
			continue
		}
		if !facts.GatesOpen(issue.Key) {
			hold(issue)
			continue
		}
		// An issue an unfinished Dispatch issue holds where it stands waits for that issue, not
		// for a slot: offered one here it would take it from an issue that can work, and its own
		// first phase would start on work its blocker has still to land. The architect above it is
		// told when it starts, and the state names it under admission.blocked meanwhile.
		if _, blocked := facts.Held[issue.Key]; blocked {
			hold(issue)
			continue
		}
		// A parent whose own Dispatch children are still open does its work after theirs
		// (LineFacts.ChildWaits): offered a slot here it would build beside the children whose
		// merges its own head is meant to carry. The architect that owns it is told once per issue
		// and run when the wait begins (admit's tellChildrenFirst), and the state names it under
		// admission.blocked meanwhile.
		if _, waits := facts.ChildWaits[issue.Key]; waits {
			hold(issue)
			continue
		}
		// An issue beneath an issue the daemon has already accepted waits for that issue to
		// close (HeldUnderAccepted): starting it would let the ancestor merge work its own head
		// does not carry. A person's move of such an issue to todo records it here at admitted
		// and leaves it here; the architect is told why when it enters. The chain is read past
		// the issues between, so a parent already done does not hide the merging issue above it.
		if HeldUnderAccepted(chains, issue) {
			hold(issue)
			continue
		}
		entry := LineEntry{Issue: issue, Kind: LineFirstPhase, Holds: holds}
		// An issue that holds its own gate has held a slot since its architect began the design
		// (KeepsSlot). With every gate it waits for open it gives that row back while it waits on
		// another issue: an open child of its own is its children-first wait, and an unfinished
		// blocked_by link is the other (blockers.waitsOf, admit's releaseSlotsForWaits). One
		// released so stands in the line by the first loop and is passed over at the top of this
		// one, and the skips above hold one whose wait the release has still to read. One that
		// reaches here waits on neither and holds the row of its run: its first phase is work
		// coming back, so it stands in the first part at the moment its gate opened
		// (TreeGate.OpenedAt), and its promotion takes no second slot. An issue under a parent
		// holds none until the daemon starts it, so its first phase is new work and waits in the
		// second part by its top issue's rank; one the daemon started and a person set back to
		// todo stands here holding the row of that run, and its promotion takes no second one
		// either.
		if chains.HoldsOwnGate(issue.Key) {
			returned = append(returned, timed{entry: entry, at: facts.Gates[issue.Key].OpenedAt()})
			continue
		}
		starting = append(starting, entry)
	}
	sort.Slice(returned, func(i, j int) bool {
		left, right := returned[i], returned[j]
		if !left.at.Equal(right.at) {
			return left.at.Before(right.at)
		}
		if left.id != right.id {
			return left.id < right.id
		}
		return left.entry.Issue.Key < right.entry.Issue.Key
	})
	// Each starting entry takes its top issue's place in the board's order and then its own
	// (R19). The top issue's rank is read off its record as the daemon holds it (Issue.Rank),
	// which every issue in Legion's work has, the top issue being one; an issue with no issue
	// above it in work is its own top issue.
	tops := make(map[string]Issue, len(starting))
	for _, entry := range starting {
		top, recorded := chains.Record(chains.TopIssue(entry.Issue.Key))
		if !recorded {
			top = entry.Issue
		}
		tops[entry.Issue.Key] = top
	}
	sort.Slice(starting, func(i, j int) bool {
		left, right := starting[i].Issue, starting[j].Issue
		if leftTop, rightTop := tops[left.Key], tops[right.Key]; leftTop.Key != rightTop.Key {
			return RankLess(leftTop, rightTop)
		}
		return RankLess(left, right)
	})

	// Each entry's part is the slice it ended in, set here rather than where the entry was
	// built, so that a loop added above cannot leave an entry with no part at all: an entry at
	// LinePart(0) would be served no sentence and no `why` and nothing would say so
	// (projection's waitingLineReason). Its place within that part is the order the part was
	// just sorted into and not the order the loops above found it; the moment that placed a
	// returned entry is the sort key itself, taken from the timed value rather than read a
	// second time from the records (LineEntry.Since); and the top issue that placed a starting
	// entry is the one the sort above read.
	line := make([]LineEntry, 0, len(returned)+len(starting))
	for i, waiting := range returned {
		entry := waiting.entry
		entry.Part, entry.PartPlace, entry.Since = PartReturned, i+1, waiting.at
		line = append(line, entry)
	}
	for i := range starting {
		top := tops[starting[i].Issue.Key]
		starting[i].Part, starting[i].PartPlace, starting[i].RootKey, starting[i].RootRank =
			PartStarting, i+1, top.Key, top.Rank
	}
	return append(line, starting...), heldRows
}

// WaitingIssues is the issues of a waiting line, in its order, for a reader that wants the
// issues rather than the entries. One reader is left: blockers.JudgeLine, which hands them to
// Judged to learn which Dispatch mirror rows the rebuilt line needs read, and loadMissing reads
// those not read yet. The state's `admission.waiting` was the other until the projection began
// reading each entry's own fields (projection's Project), and it now walks the entries and takes the key off each.
func WaitingIssues(line []LineEntry) []Issue {
	issues := make([]Issue, len(line))
	for i, entry := range line {
		issues[i] = entry.Issue
	}
	return issues
}

// QueuedRoot says whether issue, a root, is one a person handed to Legion that waits in the line
// for its first start: one that never ran, a re-admitted one and a handed-over one, each standing
// in todo, not waiting on a person (whose answer resumes it where it stands) and with a record
// that does not already say this generation runs (ran). Whether the issue is a root is the
// chain's question (Chains.IsRoot), which the caller holding a pass's facts asks with it
// (LineFacts.Queued); a caller that holds a root's record and no chains, such as a start that
// waits for its root's tree to run again, asks it of that root. It is the one statement of the
// rule's record half, read by the line itself, by the set of running roots (TreeRuns) and by the
// relay of a person's decision (WaitsForSlot), so that no two of them disagree about a root that
// has still to start.
//
// The slot does not answer it: a root a hand-over of the release before carried in, in a phase up
// to its review, may carry the row it held there while its own first start has still to come,
// which Waiting's queued-root loop states at the skip it guards.
func QueuedRoot(issue Issue, gate TreeGate) bool {
	if !issue.HandedOver || ran(issue, gate) {
		return false
	}
	return !issue.AwaitingPerson() && issue.Status == "todo"
}

// ran says whether this root's own record shows the generation it carries already running under
// Legion. Two things say so, and the slot says neither any more. The record stands in a phase of
// its own (Issue.CarriesRun — neither admitted nor done): it is at work, merging, in accepting,
// held or waiting on a person. Or its design gate has opened at every step the deployment gates
// at some point (TreeGate.EverOpened), after which the root stands at admitted while its architect
// revises the design and its own first phase waits in the line (LineFirstPhase).
//
// The one record that carries a run and has still to start here is a root a hand-over of the
// release before carried in (CarriedIn): its promotion starts it in the phase it carries
// (workflow.Engine.StartCarriedRun). The mark stays once the promotion has run, since the
// architect's start reads it (workflow.ArchitectCarried), so the record does not say the root has
// started. A root holding a slot row is never queued (AwaitsPromotion), so a person's todo on a
// promoted carried root in a phase that keeps its row queues nothing. One case is left: a promoted
// carried root in a phase that keeps no row (accepting, retro, merging or awaiting_children) that a
// person sets back to todo at the same generation is queued and promoted a second time, which
// starts its architect and the worker of its phase again at a new entry. No hand-over makes such a
// root any more, so the case ends once each root the first boot converted has run past it.
func ran(issue Issue, gate TreeGate) bool {
	return !CarriedIn(issue) && (issue.CarriesRun() || gate.EverOpened())
}

// CarriedIn says whether issue is a root a hand-over of the release before carried in at the
// generation it stands at (Issue.CarriedGeneration, which the first boot's conversion copied from
// that release's hand-over rows): the issue's own facts, not the tree it was recorded in, tell it
// apart (R19 of dispatch://INFRA-3320). No hand-over makes one any more.
func CarriedIn(issue Issue) bool {
	return issue.CarriedGeneration != 0 && issue.CarriedGeneration == issue.Generation
}

// AwaitsPromotion says whether root waits for the promotion that starts it: it is a queued root
// (QueuedRoot, over gate, its own design gate) and holds no slot row (holds). A root holding a row
// has been started already; Waiting's queued-root loop, which reads this, states why. Until that
// promotion nothing of the root runs: a carried phase starts there, so the boot and the settle
// open and record no run of it (workflow's generationStarted).
func AwaitsPromotion(root Issue, gate TreeGate, holds bool) bool {
	return QueuedRoot(root, gate) && !holds
}

// TreeLive says whether root's tree is live (R15 of dispatch://INFRA-3320): root is a gate root in
// Legion's work, an issue in work that holds its own gate (Chains.GateRoot), as Dispatch's chains
// show it now. A root still waiting in the line for its first start is in Legion's work, so its
// tree is live, and so is a root waiting on a person; a root a person stopped, or the daemon
// closed, is not. It reads the chains of root's project with root named (ReadChains), three
// queries; a pass that judges many roots reads them all at once instead (LiveRoots over
// ReadLineFacts).
func TreeLive(ctx context.Context, store Store, tx pgx.Tx, root Issue) (bool, error) {
	if !InLegionsWork(root) {
		return false, nil
	}
	chains, err := ReadChains(ctx, tx, store, root.Project, []string{root.Key})
	if err != nil {
		return false, err
	}
	return chains.GateRoot(root.Key) == root.Key, nil
}

// TreeRuns says whether root, a gate root, runs (R4 of dispatch://INFRA-3320 as R15 reads it): it
// is in Legion's work and does not wait in the line for a first start whose slot it does not hold
// (QueuedRoot, over gate, its own design gate). Whether root is a gate root is the chain's
// question (Chains.GateRoot), which RunningRoots asks of every root of a pass and TreeRunning of
// one.
//
// It is the one statement of "the tree runs", and the readers that must agree all call it: the
// claims nothing will wake (workflow.Stranded), the boot's resume of a suspended phase worker
// (workflow's booted) and the question whether a session's status write lands on a running tree
// (workflow's sessionStatusWrite). Each of them acts on the root as it stands now, and a root
// waiting in the line runs nothing yet: its architect's first task comes with its promotion, so a
// claim of it is not stranded, a suspended worker of it is not resumed, and a status write on it
// is a person's move.
func TreeRuns(root Issue, gate TreeGate) bool {
	return InLegionsWork(root) && !QueuedRoot(root, gate)
}

// TreeRunning is TreeRuns for a caller holding one root and no facts of a pass: it reads the
// chains of root's project with root named and root's design gate (readChainFacts), and asks the
// one rule of root where the chains make it a gate root. steps are the design-gate steps the
// deployment gates.
func TreeRunning(ctx context.Context, store Store, tx pgx.Tx, root Issue, steps []DesignStep) (bool, error) {
	if !InLegionsWork(root) {
		return false, nil
	}
	running, err := ReadRunningRoots(ctx, tx, store, root.Project, []Issue{root}, steps)
	if err != nil {
		return false, err
	}
	_, runs := running[root.Key]
	return runs, nil
}

// OperatorHolds says whether the operator opened the lifetime of the issue itself (an operator
// spawn under its key) and its cleanup has not confirmed: the issue's own lifecycle, which an
// operator's hold of a tree became a hold of each issue of (R22 of dispatch://INFRA-3320). An
// issue with no lifecycle row is no issue the operator holds, which is the read's own answer for
// it.
//
// It is the one statement of that rule, and every reader that must agree calls it: the park of
// such an issue (workflow.Engine.leave), the claim release row every other park queues
// (outbox.releaseClaim), and the boot's answer to whether a run no start counted is open
// (workflow.Engine.uncountedRunOpen).
func OperatorHolds(ctx context.Context, store Store, tx pgx.Tx, issue Issue) (bool, error) {
	project, err := claim.ProjectToken(issue.Project)
	if err != nil {
		return false, err
	}
	lifecycles, err := store.IssueLifecycles(ctx, tx, project, []string{issue.Key})
	if err != nil {
		return false, err
	}
	lifecycle, found := lifecycles[issue.Key]
	return found && lifecycle.HeldByOperator(), nil
}

// RunningRoots is the set of the keys among issues that are gate roots on the pass's chains
// (Chains.GateRoot) and run (TreeRuns), read from the records and the pass's own facts already in
// hand.
func RunningRoots(issues []Issue, facts LineFacts) map[string]struct{} {
	running := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		if facts.Chains.GateRoot(issue.Key) == issue.Key && TreeRuns(issue, facts.Gates[issue.Key]) {
			running[issue.Key] = struct{}{}
		}
	}
	return running
}

// ReadRunningRoots is RunningRoots for a caller that holds no facts of the pass: the chains of
// project with every key of issues named and the gates on them (readChainFacts), four reads
// whatever the number of roots, and then the one rule. project is the Dispatch project key and
// steps the design-gate steps the deployment gates.
func ReadRunningRoots(ctx context.Context, tx pgx.Tx, store Store, project string, issues []Issue, steps []DesignStep) (map[string]struct{}, error) {
	chains, gates, err := readChainFacts(ctx, tx, store, project, issues, steps)
	if err != nil {
		return nil, err
	}
	return RunningRoots(issues, LineFacts{Chains: chains, Gates: gates}), nil
}

// LiveRoots is the set of the keys among issues whose trees are live (TreeLive): each gate root
// in Legion's work on the pass's chains (Chains.GateRoot), read from the records and the pass's
// own facts already in hand.
func LiveRoots(issues []Issue, facts LineFacts) map[string]struct{} {
	live := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		if facts.Chains.GateRoot(issue.Key) == issue.Key {
			live[issue.Key] = struct{}{}
		}
	}
	return live
}
