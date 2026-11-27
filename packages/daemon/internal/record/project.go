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

// AcceptedStatus says whether Dispatch shows an issue accepted: retro, the head the step before its
// retro passed accepted, or done (DoneStatus). It is the Dispatch half of R1's first condition for
// a basis (dispatch://INFRA-3320): register_gate refuses an ancestor Dispatch shows so, and the
// daemon ends a step's wait on such an ancestor's request, wherever it reads the status.
func AcceptedStatus(status string) bool {
	return status == DoneStatus || status == "retro"
}

// BasisNotYetAccepted is what every BASIS_ACCEPTED refusal of a registration on a basis ends with:
// register_gate's of an ancestor that Dispatch shows accepted or the daemon records so (api's
// registrationBasis), and the daemon's of one accepted since that read (workflow's acceptedBasis).
const BasisNotYetAccepted = "a basis is an ancestor not yet accepted, so request a person's approval of the issue's own document"

// OutOfWorkflowStatuses are the four Dispatch statuses that take an issue out of the workflow,
// and the one list of them: OutOfWorkflow is this list read as a predicate, so a status added
// here moves every reader of the rule with it.
//
// The list is exported because one reader cannot call the predicate at all. The pod gate is
// deliberately one statement of "still the workflow's to run" (pods.Standing.InWorkflow),
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

// EndChildRule is how an architect ends a child, in the short form that the daemon's refusals
// and notices carry (dispatch://INFRA-3178 R6). The daemon takes the done, backlog, icebox or
// triage of the architect of an issue above a child as it takes a person's move (workflow's
// sessionStatusWrite). So the move is the architect's own status write in Dispatch, and no
// operation of the legion tool makes it. Each thing the rule says stops a loss
// that the move alone does not stop. A fold names its carrier, and the issues that wait on the
// folded child wait on the carrier first, because done starts them. Dispatch closes an issue only
// once each issue under it is done, and moves an issue out of work with the open issues under it
// only when the write says so (R24 of dispatch://INFRA-3320), so a child with open children is
// closed from the bottom, each of them done first, and its backlog or icebox takes
// include_children. A running child's worker
// pushes first, because done ends its turn and its pod at once. An open pull request leaves the
// merge queue and closes unless its work should land, because the daemon changes no pull request.
// Backlog and icebox are for a person's request or a restart, and finished work is done. A child
// already done takes no status, because a move out of done ends its production check. And a read
// back after each move finds a move that the daemon set back.
//
// It lives here because two packages send it, and api does not import workflow: workflow's
// refusals and notices over children still in the workflow, and api's refusals of close_root on a
// child and of the removed child route. The architect prompts carry the same rules whole
// (prompts/go/architect-common.md). The legion tool's description carries these words
// (packages/pi-legion/src/tools.ts), and these tests hold the copies together:
// TestTheEndChildRuleGolden in internal/api writes this constant and StatusNotTakenYet to
// packages/contracts/fixtures/daemon-api/end-child-rule.json, and
// packages/pi-legion/src/tools.test.ts holds the description to that file.
const EndChildRule = "End a child with your own status write in Dispatch. " +
	"Write done with a reason when the child's work is done, folded into another issue or no longer needed: " +
	"dispatch issue-update --issue <KEY> --status done --reason '<why>'. " +
	"Dispatch closes an issue only once each issue under it is done, so when the child has open children, close each of them done first, from the bottom. " +
	"For a fold, name the carrier in the reason: the issue that carries the work. " +
	"Before the done of a fold, make each issue that is blocked_by the child wait on the carrier instead. " +
	"Done starts those issues, and the Referenced by list of dispatch read names each of them as blocked_by. " +
	"Before done on a running child, have its worker push the work that the carrier needs. " +
	"Done ends the worker's turn and the pod at once. " +
	"Unless the work of the child's open pull request should land, close that pull request with your own App (gh pr close). " +
	"When it is in the merge queue, first take it out (GraphQL dequeuePullRequest). " +
	"Do this before done, or at once after a done that you wrote first. " +
	"Write backlog or icebox only when a person asked for that move, or as the first step of a restart. " +
	"A backlog or icebox of a child that has open children takes include_children, which moves those children with it. " +
	"A restart is backlog, then rerun_child. " +
	"Say why in a dispatch comment. " +
	"Write no status on a child that is already done, because a move out of done ends an open production check. " +
	"After each move, read the child back (dispatch read --issue <KEY>). " +
	"When the daemon set the move back, write it again."

// StatusNotTakenYet is the sentence that the architect's refusals over children still in the
// workflow, the pr-merged notice and the retro-held notice carry; workflow's endChildWords holds
// it with EndChildRule and names each text that carries them. The daemon reads the architect's
// status write on a child from that write's Dispatch event, and the event consumer can deliver
// the event after the architect calls again. Until then the record holds the child in the
// workflow and the refusal names it, so the architect waits for read_record to show its move
// (dispatch://INFRA-3178 R1).
//
// It lives here, beside EndChildRule, because the legion tool's description carries both texts
// (packages/pi-legion/src/tools.ts). The tests that hold EndChildRule to that description
// hold this sentence to it too: TestTheEndChildRuleGolden writes both to end-child-rule.json.
const StatusNotTakenYet = "A status that you wrote and that the daemon has not taken yet does not count until read_record shows it."

// InWorkflow says whether an issue is in the workflow: its Dispatch status is one the workflow
// runs in (OutOfWorkflow) and its phase is short of done. One still admitted counts, since it
// will run — a root a person handed back to Legion stands in admitted with no role's phase until
// its task's first step starts (planning, in the default steps), and is running from then on.
// It is the production check's "runs again" predicate (workflow's check.go) and the outbox's
// (checkLookHolds): a look of an issue that runs again would resume the checking role and
// relaunch its Sandbox under the run, so none starts.
//
// It lives here, beside the status rule it is written over, because every judgement that reads
// the daemon's own records against Dispatch's mirror needs it — the children-first wait
// (ChildOpen), the acceptance order (OpenBeneath), the state's projection and the API's
// refusals among them — and internal/workflow, where it used to live, is the one package
// several of those may not read.
//
// The rule itself is pods.Standing.InWorkflow, which the pod gate reads too: one statement, so a
// terminal phase added to it moves admission, the production check and the release gate together.
func InWorkflow(issue Issue) bool {
	return issue.Standing().InWorkflow(OutOfWorkflow)
}

// inWorkStatuses are the five Dispatch statuses an issue in Legion's work shows (R15 of
// dispatch://INFRA-3320): todo, which hands an issue to Legion or releases it, and the four its
// workflow writes as it runs. InWorkStatus reads it in Go, and the store's queries are handed it.
var inWorkStatuses = []string{"todo", "in_progress", "testing", "needs_review", "retro"}

// InWorkStatus says whether status is one of the five Dispatch statuses of work (inWorkStatuses),
// the group R24 of dispatch://INFRA-3320 calls in work: Dispatch's parent rule holds a parent in
// that group while a child of it is.
func InWorkStatus(status string) bool {
	return slices.Contains(inWorkStatuses, status)
}

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
	return issue.Phase != phase.Done && InWorkStatus(issue.Status)
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
// list of phases is written over — the workflow's own awaitsMerge, its close of a merged issue
// and its open review round — so no two of those questions read a held issue differently;
// Accepted and InSlotlessPhase read StandsAt as they do, Accepted through AcceptedPhase.
func StandsIn(issue Issue, phases ...phase.Phase) bool {
	return slices.Contains(phases, StandsAt(issue))
}

// AcceptedPhase says whether p is one of the phases past the daemon's acceptance of an issue's
// head: its retro and its merge. The two phases are named here and nowhere else, so that no rule
// of the acceptance order can read a list of its own: one that did is what let a child held from
// retro through. Accepted reads it of where an issue stands, and the workflow reads it of the
// phase a move left (workflow's acceptedEntry) and of the phase a move enters.
func AcceptedPhase(p phase.Phase) bool {
	return p == phase.Retro || p == phase.Merging
}

// Accepted says whether the daemon has already accepted issue's head, the head the last step of
// its task passed, and the issue has not closed: it stands in its retro or in its merge, or is
// held from one of them (StandsAt, AcceptedPhase). The head is the reviewed one only where the
// task has a reviewer step; a plan with no reviewer step has no reviewed head. Work entering the
// workflow beneath an issue in one of those phases would let that issue merge first, which is the
// one thing a parent must not do (dispatch://INFRA-980, "Children first"). The head of a root
// whose worker died in retro or merging is accepted still, so the hold must not let work in beneath
// it: the line held no issue under such a root, the two doors let one in, and the close of a
// root held from retro handed over nobody.
//
// A parent held before its retro for the issues beneath it (HoldForChildren, a parent whose task's
// last step is not the owner's acceptance) is not accepted: the hold stands in place of its retro until those
// issues have run, so each of them must start beneath it and be counted by its wait. Read as
// accepted, it would hold a child at admitted until the parent closed (HeldUnderAccepted), leave
// that child out of the wait (OpenBeneath), and start its retro and merge without the child's work.
//
// Every rule of the acceptance order asks this predicate, through the nearest accepted ancestor
// (AcceptedAncestor): the waiting line's hold (HeldUnderAccepted), the doors' refusal (api's
// CHILD_UNDER_ACCEPTED_PARENT, AcceptedAncestorIn) and the wait a merged issue's close keeps
// (OpenBeneath, which leaves out what that hold stopped) — so none of them can count a root as
// accepted that another counts as still at work.
func Accepted(issue Issue) bool {
	return AcceptedPhase(StandsAt(issue)) && !HeldForChildren(issue)
}

// BasisAcceptance says why ancestor no longer meets the daemon's half of R1's first condition for
// a basis (dispatch://INFRA-3320), as the daemon's own store shows it now, or "" where it still
// does: what the daemon last read of Dispatch shows it done or retro (AcceptedStatus), in its
// mirror of Dispatch's issues or in its record of the ancestor; the daemon records it in its retro
// or its merge (Accepted); or the daemon has queued its close (closeQueued). Where neither the
// mirror nor the record knows the ancestor, nothing the daemon holds says that it is accepted. A
// root a person moved to the backlog or to icebox keeps the phase it stood in (R20 of
// dispatch://INFRA-3320), and R1 counts backlog and icebox as not accepted, so the phase alone
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
// on the ancestor (RestingRoots), apply in one order; the pending-basis seed's read and the
// person's approval as its live event read it before either opens a step; and a step carried onto
// a child keeps a basis or a rest only on an ancestor it passes (the daemon's carriedGate).
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

// HeldFor says whether issue stands in held for reason.
func HeldFor(issue Issue, reason HoldReason) bool {
	return issue.Phase == phase.Held && issue.Hold != nil && issue.Hold.Reason == reason
}

// HeldForChildren says whether issue is a parent held before its retro for the issues beneath it
// (HoldForChildren).
func HeldForChildren(issue Issue) bool {
	return HeldFor(issue, HoldForChildren)
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
// nothing of it has started, and the line will offer it no slot while that issue stands in retro
// or merging (Waiting). It is one rule with two readers that must agree exactly: OpenBeneath
// leaves such an issue out too, so the ancestor's own acceptance and close are not deadlocked by
// the issue its hold stopped. The hold never outlives the ancestor that caused it: once that
// ancestor closes it is out of Legion's work, the line reads this rule again and finds no hold,
// and the issue starts.
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
	// and one a hand-over of the release before made. Its promotion opens its lifecycle and starts its
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

// LinePart is the part an entry stands in within its priority band (Waiting): returning work,
// oldest first, then work that has not started, by board rank. A LineFirstPhase entry falls
// in the returned part when the issue holds its own gate and in the starting part otherwise.
type LinePart int

const (
	// PartReturned is the first part of each priority's band: the issues the daemon has already
	// started whose work has come back to them, placed by the moment it did (LineEntry.Since).
	PartReturned LinePart = iota + 1
	// PartStarting is the second part of each band, ordered by top issue rank and then own rank.
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

// LineEntry is one issue of the waiting line with the reason it waits there, the priority and the
// part of the line it waits in and what placed it there, and whether it already holds one of the
// cap's slots.
type LineEntry struct {
	Issue Issue
	Kind  LineKind
	// Priority is the priority that placed the entry, the first key of the whole line:
	// PriorityOf, the priority Dispatch resolves the issue to where Dispatch sent one, else, with
	// an older Dispatch that resolves none, the issue's own, else its top issue's, else
	// NoPriority, whose band stands after P3. The issue's own priority is Issue.Priority, which
	// this does not change. A P0 that Dispatch passes down starts outside the cap
	// (Issue.StartsNow); only the top issue's priority, the fallback of an older Dispatch, orders
	// the line and nothing else.
	Priority Priority
	// Part is the part of the line the entry stands in within its priority's band, and PartPlace
	// its place within that band and part alone, counted from 1. Waiting sets both in its last
	// loops, once the line is sorted, rather than where the entry was built: the kind does not
	// say the part (LinePart), and the place is the order the sort left, which is the order the
	// line publishes and not the order the loops found.
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
	// (Issue.Rank). The starting part carries both fields because it is ordered by those ranks.
	// The returned part is ordered by Since and carries RootKey only to explain priority.
	// The projection serves the rank fields only for the starting part.
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
// them is read under the one fact lock (intake.ApplyFact) on every pass (the pass rule on admit's
// Admission.Apply): a read per chain, per gate or per slot would cost the daemon a round trip for
// each of them on each pass.
type LineFacts struct {
	// Chains is the pass's one reading of the Dispatch chains of the project and of each issue
	// the pass was read for (ReadChains): every question of above, under, root, top issue and
	// gate root the line, the holds and the measures ask is asked of it, and nothing of the
	// daemon's stored tree (R15 of dispatch://INFRA-3320). It is the pass's one chain: read once,
	// with the daemon's record of every key it holds by key (Chains.Record), its walks kept as
	// they are first asked, and handed to every walk of the pass — the cycle of each child
	// (childWaits), the child left out of a parent's open children (withoutHeldUnderAccepted), the
	// issues beneath each parent held before its retro (retroHoldsOf), the line (Waiting), the held
	// list's ladder (HeldList, HeldReasonOf) and the controller's tick, which reads these facts
	// (daemon's waitedOn) — so no walk of a pass reads a second copy of a chain or a record.
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
	// awaiting_children by and returns one by, to the step ReturnStep gives its held start (admit's
	// childrenWaits), and the children the held word awaiting_children names (HeldReasonOf). It is
	// apart from ChildWaits, the wait of an issue whose first phase has not started: a started
	// parent's children hold no first phase, so no children-first notice, no children_first word
	// and no wait of a started issue on another issue (Waits) reads them. ReadLineFacts fills it,
	// since the judgement reads the mirror against the daemon's records in the transaction the line
	// is never given; it is never stored, and each read of the facts judges it again.
	StartedParentWaits map[string]ChildWait
	// RetroHolds is the three holds on the acceptance of each parent held before its retro
	// (HoldForChildren, RetroHeldParents), by parent key (retroHoldsOf): the issues beneath it
	// still in the workflow (OpenBeneath), the roots still in the workflow that rest on its
	// approval (RestingRootsOn) and its open Dispatch children, judged as StartedParentWaits is
	// (readChildWaits). They are the three that hold its retro (workflow's acceptanceHeld), which
	// the held word children_before_retro names in R8's order (HeldReasonOf) and the admission
	// pass compares from one pass to the next (admit's childrenWaits). ReadLineFacts fills it, and
	// reads nothing for it where no parent is held before its retro.
	RetroHolds map[string]RetroHolds
	// childrenFirst is the children-first judgement ReadLineFacts made from the read of the
	// children it judged StartedParentWaits from (readChildWaits), which ChildWaitsOf hands the
	// caller to fill ChildWaits with; nil in facts ReadLineFacts did not read.
	childrenFirst map[string]ChildWait
}

// ChildWait is why one parent's own first phase waits: the open Dispatch children under it, in
// Dispatch key order. It is data alone — the rule that fills it is childWaits — so that the
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
	// of its own is work its architect filed and has not released. It is set by childWaits, from
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
// owes, the three judgements of a parent's open children — the started parents'
// (LineFacts.StartedParentWaits), the parents' held before their retro and the children-first
// waits ChildWaitsOf hands on — from the children the chain read already holds (readChildWaits),
// and the roots that rest on each parent held before its retro (RestingRootsOn: two reads, and
// none where no parent is held so), with which it puts together the three holds on the
// acceptance of each such parent once (LineFacts.RetroHolds, retroHoldsOf), whatever the number
// of chains, gates and issues involved (R16 of dispatch://INFRA-3320: one pass reads the copy in a
// number of queries that does not grow with the number of issues or links). project is the
// Dispatch project key and steps the design-gate steps the deployment gates.
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
	// The children are judged after the rest, since the gates say which issues are children-first
	// candidates and an issue owing a start is a started parent (StartedParents). The parents held
	// before their retro (RetroHeldParents) are found once, for that judgement and for the read of
	// the roots that rest on them (RestingRootsOn) after it, which reads nothing where no parent is
	// held so, and the three holds of each are put together from the two once (retroHoldsOf).
	// Their walks of the parent chain read the pass's one chain (LineFacts.Chains).
	held := RetroHeldParents(issues)
	var children map[string]ChildWait
	facts.childrenFirst, facts.StartedParentWaits, children = readChildWaits(issues, held, facts)
	resting, err := RestingRootsOn(ctx, tx, store, held)
	if err != nil {
		return LineFacts{}, err
	}
	facts.RetroHolds = retroHoldsOf(facts.Chains, held, children, resting)
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
// where every gate it waits for is open. An issue whose run starts as a root's (Chains.StartsAsRoot)
// waits for its own design gate to open (TreeGate.Open), which its architect's design opens. Any
// other issue under a parent waits for each gate step it holds itself that does not stand open — a
// step registered on it, whether it ran as a root or not — and not for a design its architect
// never registered: an issue a person queued as a root and that never started holds no step, and
// under a parent at work it starts on todo (R19). And every issue waits for the gate of each gate
// holder above it (GateHolders) that is not open.
func (f LineFacts) ShutGates(key string) []string {
	var shut []string
	if f.Chains.StartsAsRoot(key) {
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
// record and design gate say its first start has still to come, its run starts as a root's on the
// pass's chains (Chains.StartsAsRoot), a root's or a new run of an issue that holds its own gate,
// and nothing holds it where it stands (HeldWhereItStands). A run that starts as a root's can stand
// under a parent, a gate holder past its first run, and it meets the holds of an issue under a
// parent before it starts as every such issue does; the promotion starts only what the line
// holds, so it starts no such run while a hold stands, and the same todo queues it once the hold
// ends. The record is read first, so an issue whose record already says no costs no walk of its
// chain.
func (f LineFacts) Queued(issue Issue) bool {
	return QueuedRoot(issue, f.Gates[issue.Key]) && f.Chains.StartsAsRoot(issue.Key) && !f.HeldWhereItStands(issue)
}

// HeldWhereItStands says whether issue's chain holds it where it stands, so that the line starts
// nothing of it — its first phase (Waiting) or its queued promotion (Queued): its chain is unknown
// (R16 of dispatch://INFRA-3320), which says neither whether it is a root nor what stands above
// it; or it stands under a parent in one of R19's three holds — its parent is out of Legion's work
// and an issue above that parent is in it (ancestry), its top issue waits on a person
// (root_awaiting_person), or an issue above it stands in its retro or its merge
// (HeldUnderAccepted). It is the one statement of those holds that the line reads. The returning
// work of an issue the daemon has started reads it on a known chain alone (Waiting): R16 holds an
// unstarted issue on an unknown chain, so such a chain holds no return, and R19's three holds do.
// Which of them holds the issue, and which issue holds it, is the held list's word (HeldReasonOf).
func (f LineFacts) HeldWhereItStands(issue Issue) bool {
	chains := f.Chains
	chain := chains.Chain(issue.Key)
	if !chain.Known() {
		return true
	}
	above := chains.Above(issue.Key)
	if len(above) == 0 {
		return false
	}
	if above[0] != chain.Links[0] {
		return true
	}
	if top, _ := chains.Record(chains.TopIssue(issue.Key)); top.AwaitingPerson() {
		return true
	}
	return HeldUnderAccepted(chains, issue)
}

// AwaitsTodo says whether issue waits for a person's todo to start its run (R19 and R20 of
// dispatch://INFRA-3320): it stands at admitted in Legion's work, holds no slot row (holds false),
// and Dispatch shows it at a status in work other than todo, which is all that keeps it out of the
// line: at todo it would be a queued root (Queued) that holds no row, which the promotion starts
// with its architect (AwaitsPromotion). A run that starts as a root's starts only on the handover
// a todo makes, and two such runs come to stand elsewhere: a root a person moved from todo to
// in_progress before its first promotion, and a new run that R20 opens at the status a person
// moved the issue to (workflow's NewRun), as when a person reopens an issue the merge of its pull
// request closed straight to in_progress. The queued rule is read as it stands, over the issue at
// todo, and is not stated a second time here, so an issue a hold of Queued's stops waits for that
// hold and not for a todo.
func (f LineFacts) AwaitsTodo(issue Issue, holds bool) bool {
	if holds || issue.Phase != phase.Admitted || issue.Status == "todo" || !InLegionsWork(issue) {
		return false
	}
	atTodo := issue
	atTodo.Status = "todo"
	return f.Queued(atTodo)
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

// TreeSteps is the steps one tree is gated on: the steps the deployment gates, in the order it
// names them, and criteria after them when the tree's root holds a criteria row and the
// deployment gates spec but not criteria (JoinsByRegistration). gates are the root's rows by
// step.
//
// `gates.design` names the steps every root must register before its tree works, and any step a
// root registers holds its tree too: under a deployment that gates spec alone of the two, an
// architect may register its root's Verification document as criteria (api's registeredStep),
// and from then on a person's approval of that document holds the tree as the spec's does, and a
// new version of it closes the gate. A criteria row exists only by the architect's registration:
// nothing else writes one, so a tree whose architect registered none — every gate registered
// before register_gate took the step — stays gated on the deployment's steps alone. A row kept
// after a switch to `gates.design: off` gates nothing: that deployment gates no step, so no step
// joins its list.
//
// Every rule that asks whether a tree is gated reads this list. TreeGate and classify's
// StandingGate, DesignGatesOpen and FirstClosedStep apply it themselves, so a caller passes them
// the deployment's list; a caller that walks the steps itself walks this answer. The answer is
// the deployment's own slice when nothing joins it, and a fresh one otherwise, so no caller
// writes into the configured list.
func TreeSteps(deployment []DesignStep, gates map[DesignStep]DesignGate) []DesignStep {
	if _, registered := gates[DesignStepCriteria]; !registered || !JoinsByRegistration(deployment, DesignStepCriteria) {
		return deployment
	}
	return append(slices.Clip(deployment), DesignStepCriteria)
}

// JoinsByRegistration says whether step is one the deployment does not gate that still gates a
// tree whose root registers it (TreeSteps): criteria, under a deployment that gates spec and not
// criteria. It is the one statement of which step that is (TakesStep reads it), so the
// registration and every reader of the rows agree on it.
func JoinsByRegistration(deployment []DesignStep, step DesignStep) bool {
	return step == DesignStepCriteria && slices.Contains(deployment, DesignStepSpec) && !slices.Contains(deployment, DesignStepCriteria)
}

// TakesStep says whether a root's registration of step holds its tree: a step the deployment
// gates, or the one that joins the tree by its registration (JoinsByRegistration). It reads no
// rows, so the step being registered counts in its own registration. Both sides read it:
// register_gate takes such a step and refuses every other (api's registeredStep), and the workflow
// gates its row from the first write (Engine.gatesStep).
func TakesStep(deployment []DesignStep, step DesignStep) bool {
	return slices.Contains(deployment, step) || JoinsByRegistration(deployment, step)
}

// TreeGate is one tree's design gate as every rule over it reads it: the steps the deployment
// gates (Steps, the empty list standing for the single step spec, Required) and the row the
// tree's root holds for each step its architect has registered (Rows, nil for a root that has
// registered none). The gate is a list of steps and the work waits on all of them, so a rule
// that used to read one row reads this instead and no reader can answer from one step where the
// deployment gates three. The steps the work waits on are the ones the tree is gated on
// (TreeSteps over Steps and Rows), which Open, EverOpened and OpenedAt all read in the one form
// (Required).
//
// The zero value is the gate of a root with nothing registered, which is the answer a reader
// wants for a tree the pass holds no gate row for: it is open at no step and has opened at none.
type TreeGate struct {
	Steps []DesignStep
	Rows  map[DesignStep]DesignGate
}

// Required is the steps the tree's work waits on: the steps the tree is gated on (TreeSteps), so a
// criteria row its architect registered beside a deployment's spec holds the tree in every rule of
// the gate alike, or the single step spec where that list is empty. It is the one statement of the
// rule, and every reader of the steps a tree's work waits on reads them here.
//
// The empty list keeps its own hold. A deployment that gates no step asks no person to approve
// anything, and its registration opens the gate at once; but the registration itself is still
// what says the architect has written the spec, and a rule reading "every gated step is open"
// would be true of a tree whose architect had registered nothing and would start it. So the
// empty list requires the spec step's record, which the registration writes and opens in one act.
func (g TreeGate) Required() []DesignStep {
	steps := TreeSteps(g.Steps, g.Rows)
	if len(steps) == 0 {
		return []DesignStep{DesignStepSpec}
	}
	return steps
}

// Open is whether the gate lets the tree's work run: an approval stands on the current document
// version of every required step (GateOpen), a person's or the one a basis opened the step on. A
// required step with no row holds the work, as an unregistered gate always has, so a root with no
// row at all is held too. It is the one rule for the question, which classify.DesignGatesOpen
// reads it through.
func (g TreeGate) Open() bool {
	for _, step := range g.Required() {
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
// architect has still to finish registering has not either. A criteria row registered after
// that release is written as opened (workflow's gateRegistered), so a tree whose gate opened
// before its architect registered its Verification document still reads as released; one
// registered before the first opening holds this answer until a person approves it.
func (g TreeGate) EverOpened() bool {
	for _, step := range g.Required() {
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
	for _, step := range g.Required() {
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
// awaiting_person; the four phases a step's worker runs besides the owner's acceptance —
// planning, implementing, testing and reviewing; and held, which is reached here only where the
// record names no earlier phase for the hold (StandsAt). Every other phase holds none: accepting,
// retro and merging (in the default steps, the phases after the review); done, which has
// finished; and awaiting_children, a parent waiting on its open children, which work under slots
// of their own (dispatch://INFRA-3320 R11). A step after accepting whose phase keeps a slot waits
// for one again (workflow.StartOrOwe).
//
// It is written as one list so that a later change adds or removes one entry and every reader of
// the rule moves with it. Two read it, and a phase in one and not the other would be a deadlock:
// the cap's own half (KeepsSlot) and the mark a start of a phase worker carries for the slot it
// waits for (workflow.StartOrOwe), whose row the outbox holds unclaimable while the issue holds
// no slot. InSlotlessPhase is the complement of the three working phases this refuses, for the
// repair that must reach an issue holding no row.
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
// the moment the daemon starts it until it stands in accepting, retro, merging or
// awaiting_children, which hold none, or until it is done or Dispatch takes it out of the
// workflow (InWorkflow). Every other phase keeps the row — admitted, awaiting_person, the four
// worker phases and a hold the record names no phase for (PhaseKeepsSlot) — so an issue waiting
// for a person or for its review keeps its place against the cap and wins none back from the line
// when the wait ends. A parent in awaiting_children gives its row back while its open children
// work under slots of their own, and a start of planning through reviewing that its return makes
// waits in the line for one again (workflow's ReturnFromChildren). Any other wait on another
// issue is not read here: an issue the daemon has started that waits on one gives its row back
// and stands in the line for one again, by the rule beside this (blockers.waitsOf, admit's
// releaseSlotsForWaits), save the roots that rule names as keepers. That rule reads this one
// first, so an issue in accepting, retro, merging or awaiting_children waits on nothing there
// (blockers.waitsOf). The rule whole is "Admission and the `legion` label" in
// docs/site/src/content/docs/legion/concepts.md.
//
// The cap bounds the work in progress, and an issue in accepting, retro or merging writes no more
// of the change there: accepting, retro and merging run on no slot at all, so their starts are
// claimable the moment they are written (workflow.StartOrOwe) and a tree's last phases count
// against none of the issues that have a change still to write. Work the architect sends back
// from accepting or from retro to implementing is at work again, so it takes a slot again, and
// it waits for one among the issues whose work came back; so does a step after accepting whose
// phase keeps a slot. A merged root stands in merging until the rest of its tree leaves the
// workflow (workflow's closeMerged), and it stands there on no slot, as accepting and retro do.
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
// rather than writing a second copy of, and NeedsSlot asks the tree's own terms beside this.
func KeepsSlot(issue Issue) bool {
	return InWorkflow(issue) && PhaseKeepsSlot(StandsAt(issue))
}

// InSlotlessPhase says whether issue stands in a working phase that holds none of the cap's slots,
// or is held from one (StandsAt): a phase of phase.Order that PhaseKeepsSlot refuses, which in
// this release is accepting, retro or merging in every order a task's steps run in. It is
// PhaseKeepsSlot read the other way round over the working phases, and it reads that list rather
// than a copy of its own.
//
// It exists so that the list lives in this package alone. The repair of a worker with no task
// (admit's resumeSlottedWorkers) must reach an issue standing in one of those phases, because
// such an issue holds no row and the repair's own domain is the slot holders; and the `why` of an
// issue at work in one of them is no answer (projection's atWorkInSlotlessPhase). Written there
// as a list of their own, a phase added to or taken out of PhaseKeepsSlot would move the cap and
// leave them behind, and the worker of that phase would be repaired by nothing. Read from here,
// they move together.
//
// It is not Accepted, and the two must not be merged: Accepted names the two phases whose head,
// the one the last step of the task passed, the daemon has accepted, which is the rule the
// acceptance order is written over (its doc names them there and nowhere else), and leaves out a
// parent held before its retro for its children; this one is the slot rule's complement and
// counts accepting, and that parent, with them.
func InSlotlessPhase(issue Issue) bool {
	at := StandsAt(issue)
	return slices.Contains(phase.Order, at) && !PhaseKeepsSlot(at)
}

// NeedsSlot says whether issue must hold one of the cap's slots now: it is in Legion's work
// (InLegionsWork) and keeps a slot where it stands (KeepsSlot). An issue holds its admission slot
// while it stands in a phase that keeps one (PhaseKeepsSlot), from the moment the daemon starts it
// until it is done or until Dispatch shows it out of Legion's work, except while it waits on
// another issue there (docs/site/src/content/docs/legion/concepts.md, "Admission and the `legion`
// label"), so the cap counts the issues the daemon has started whose change is still to write,
// rather than the ones a worker of them is running this second. Only the issue's own status is
// read (R20 of dispatch://INFRA-3320): a person's park, close or icebox of the issue frees its
// slot, and the status of an issue above it frees nothing.
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

// WaitingLineBand is where the line puts an issue whose work came back: in the band of the
// priority it takes (PriorityOf), and within that band in its first part, oldest first (Waiting).
// It states a placement, never a position: which place in the line that gives an issue depends on
// what else waits. The texts that state the line's order carry this fragment word for word — the
// daemon's own comments, the architect's skills and the operator's guides — and workflow's
// TestTheShippedWordsNameTheWaitingLineBand reads it from here and holds the files it lists to
// it. That test reads whether a file carries the words, never whether the sentence around them
// is true, which is why it also bans, in every one of those files, the wording that claims a
// position instead.
const WaitingLineBand = "within its priority, among the issues whose work came back, oldest first"

// Waiting is the line of issues that wait for an admission slot, in the order admission gives
// them one. The cap counts issues, so children wait in this line as roots do (R19 of
// dispatch://INFRA-3320).
//
// Priority is the first sort key: the issue's own, else its top issue's on this pass's
// Dispatch chain, else after P3 (PriorityOf). Each band keeps the two parts below.
// A higher priority does not take a slot back. The pass still skips issues whose wait
// has not ended, and a promotion that already holds a slot does not take a second one.
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
// An issue the daemon has not started whose chain is unknown (Chain.Known: a row the copy lacks, a
// loop, more than 32 links) starts nothing (R16): it stands in no part of the line until its chain
// is read right. The returning work of an issue the daemon has started is not held for an unknown
// chain, and waits for its slot as any returning work does. An unanswered awaiting root waits on
// the person rather than a slot, whatever Dispatch status it currently shows, so it is not in the
// line until it is Answered. A root without the label waits for nothing. An issue under a parent is
// held where it stands, and waits for nothing here, in three cases that the chain alone decides
// (R19): its parent is not in Legion's work and an issue above the parent is (ancestry), its top
// issue waits on a person (root_awaiting_person), or an issue above it stands in its retro or its
// merge (HeldUnderAccepted). The unknown chain and the three cases are one rule
// (LineFacts.HeldWhereItStands), which the queued roots (Queued) and the first phases read, so a
// gate holder past its first run under a parent, whose run starts as a root's, meets them too; the
// returning work reads it on a known chain alone, so a returning issue meets the three cases and
// not the unknown chain. The other hold, on Dispatch blocked-by links, takes an issue out of here
// only where it holds it where it stands: an issue whose first phase would start waits at admitted
// until the last issue it names is done (LineFacts.Held, blockers.Holds), so the line never offers
// it a slot. A root's links are read at its promotion instead, which passes it over and keeps its
// place (blockers.WaitsOn); either way the state names it under admission.blocked.
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
	// The line walks the pass's one chain (LineFacts.Chains), the one the pass's own walks read.
	chains := facts.Chains
	// Every drop site reports what it left behind through one ladder, read over the facts the
	// line itself is judged on and the slot rows it is judged with, so that the word an issue
	// reads here and the word the assembly reads for an issue no site saw are read from one set of
	// facts (HeldList).
	var heldRows []HeldIssue
	hold := func(issue Issue) {
		_, holds := slotted[issue.Key]
		reason, heldBy := HeldReasonOf(issue, holds, facts)
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
		// An issue under a parent in one of R19's holds starts nothing here: its parent out of
		// Legion's work under an issue in it (ancestry), its top issue waiting on a person
		// (root_awaiting_person), or, at admitted, an accepted ancestor — HeldWhereItStands, read
		// on a known chain alone. A returning issue takes no slot while the hold stands and enters
		// the line once the hold ends. An unknown chain holds none of this work: R16 holds an
		// issue the daemon has not started, and this one has started.
		if chains.Chain(issue.Key).Known() && facts.HeldWhereItStands(issue) {
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
		// An issue its chain holds where it stands starts nothing here (HeldWhereItStands, the one
		// statement of those holds, which the queued roots above read too): one whose chain is
		// unknown (R16), neither a root nor an issue under a parent until the re-read of the chain
		// moves it; and an issue under a parent in one of R19's holds. Under a parent out of work
		// with an issue above that parent in work, it is that issue's unreleased work. Under a top
		// issue that handed work to a person, it starts with that issue's resume (admit's
		// promotion of the answered root). Beneath an issue the daemon has already accepted, it
		// waits for that issue to close, since starting it would let the ancestor merge work its
		// own head does not carry; the chain is read past the issues between, so a parent already
		// done does not hide the merging issue above it, and the architect is told why when the
		// issue enters.
		if facts.HeldWhereItStands(issue) {
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
	// Resolve rank and inherited priority from the same pass snapshot.
	topOf := func(issue Issue) Issue {
		if !chains.Chain(issue.Key).Known() {
			return issue
		}
		if top, recorded := chains.Record(chains.TopIssue(issue.Key)); recorded {
			return top
		}
		return issue
	}
	tops := make(map[string]Issue, len(starting))
	for _, entry := range starting {
		tops[entry.Issue.Key] = topOf(entry.Issue)
	}
	sort.Slice(starting, func(i, j int) bool {
		left, right := starting[i].Issue, starting[j].Issue
		if leftTop, rightTop := tops[left.Key], tops[right.Key]; leftTop.Key != rightTop.Key {
			return RankLess(leftTop, rightTop)
		}
		return RankLess(left, right)
	})

	// Join the sorted parts in order and sort stably by band (PriorityOf: the one Dispatch
	// resolves, else, with an older Dispatch, the issue's own, else its top issue's). Each band
	// retains its returning work by age, then its first starts by top and own rank.
	line := make([]LineEntry, 0, len(returned)+len(starting))
	for _, waiting := range returned {
		entry := waiting.entry
		top := topOf(entry.Issue)
		entry.Part, entry.Since, entry.RootKey = PartReturned, waiting.at, top.Key
		entry.Priority = PriorityOf(entry.Issue, top)
		line = append(line, entry)
	}
	for _, entry := range starting {
		top := tops[entry.Issue.Key]
		entry.Part, entry.RootKey, entry.RootRank = PartStarting, top.Key, top.Rank
		entry.Priority = PriorityOf(entry.Issue, top)
		line = append(line, entry)
	}
	sort.SliceStable(line, func(i, j int) bool { return line[i].Priority.band() < line[j].Priority.band() })
	for i := range line {
		line[i].PartPlace = 1
		if i > 0 && line[i-1].Priority == line[i].Priority && line[i-1].Part == line[i].Part {
			line[i].PartPlace = line[i-1].PartPlace + 1
		}
	}
	return line, heldRows
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
// for its first start: one that never ran, one handed back after it finished and one a hand-over of the release before made, each standing
// in todo, not waiting on a person (whose answer resumes it where it stands) and with a record
// that does not already say this generation runs (ran). Whether the issue is a root is the
// chain's question (Chains.IsRoot), which the caller holding a pass's facts asks with it
// (LineFacts.Queued); a caller that holds a root's record and no chains, such as a start that
// waits for its root's tree to run again, asks it of that root. It is the one statement of the
// rule's record half, read by the line itself, by the set of running roots (TreeRuns) and by the
// relay of a person's decision (WaitsForSlot), so that no two of them disagree about a root that
// has still to start.
//
// The slot does not answer it: a root a hand-over of the release before carried in, in a phase
// that keeps a slot, may carry the row it held there while its own first start has still to come,
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

// TreeRuns says whether root, a gate root, runs (R4 of dispatch://INFRA-3320 as R15 reads it): it
// is in Legion's work and does not wait in the line for a slot it does not hold, holds being
// whether it holds a slot row: for its first start (QueuedRoot, over gate, its own design gate),
// or for the slot its work needs again, among the work that came back after a stop (R20,
// waitsWithoutSlot). Whether root is a gate root is the chain's question (Chains.GateRoot), which
// RunningRoots asks of every root of a pass and TreeRunning of one.
//
// It is the one statement of "the tree runs", and the readers that must agree all call it: the
// claims nothing will wake (workflow.Stranded), the question whether a session's status write
// lands on a running tree (workflow's sessionStatusWrite), the status repair's passed-over roots,
// the stalled-tree measure and the state's `treeRuns`. Each of them acts on the root as it stands
// now, and a root waiting in the line runs nothing yet: its architect's first task comes with its
// promotion, and a returned root's work comes back with the slot its start waits for, so a claim
// of it is not stranded, a status write on it is a person's move, and its quiet is no stall. The
// readers that decide what a started root, or an issue under it, owes once its slot comes ask
// TreeStarted instead.
func TreeRuns(root Issue, gate TreeGate, holds bool) bool {
	return TreeStarted(root, gate) && !waitsWithoutSlot(root, holds)
}

// waitsWithoutSlot says whether issue waits in the line for a slot it does not hold, holds being
// whether it holds a slot row: it needs one where it stands (NeedsSlot), the rule the line offers
// a returning issue a slot by (Waiting), it holds none, and it waits on no other issue
// (Issue.ReleasedForWaitAt), whose wait the line passes over and whose work the issues it waits
// on carry meanwhile. Such an issue's work came back to it without its slot: a stop freed the row
// and its return owes its phase's worker a start, or stands it at admitted with its gate open
// (R20 of dispatch://INFRA-3320), and work sent back from past its review into a phase that keeps
// a row waits the same way (KeepsSlot).
func waitsWithoutSlot(issue Issue, holds bool) bool {
	return !holds && NeedsSlot(issue) && issue.ReleasedForWaitAt == nil
}

// TreeStarted says whether root, a gate root, has made its first start and is in Legion's work: it
// does not wait in the line for its first start (QueuedRoot, over gate, its own design gate). It
// states R9 of dispatch://INFRA-3320, "No phase of a parent starts while an open child stands under
// it", over every issue whose phases started, a start written and not delivered among them, and
// R20's "The daemon acts on each issue's own status". It is not TreeRuns (R4: "does not wait in the
// line for a slot it does not hold"): a root that waits for the slot of its own returned work runs
// nothing by R4, while it is still a parent whose start is written and not delivered (R9), the
// issues under it work on with slots of their own (R20), and a start its phase worker owes is
// written marked for that slot, which it waits for in the line. Its readers decide for such a
// root or the issues under it: the admission pass's children waits and the repair's issues past
// their review (admit's childrenWaits, readSlotHolders), and the boot's resume of a suspended
// phase worker (workflow's booted), which puts back a start a stop or a crash lost, marked for
// the slot where the issue holds none.
func TreeStarted(root Issue, gate TreeGate) bool {
	return InLegionsWork(root) && !QueuedRoot(root, gate)
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
// (Chains.GateRoot) and run (TreeRuns), read from the records, the slot rows and the pass's own
// facts already in hand.
func RunningRoots(issues []Issue, slots []Slot, facts LineFacts) map[string]struct{} {
	slotted := make(map[string]struct{}, len(slots))
	for _, slot := range slots {
		slotted[slot.Issue] = struct{}{}
	}
	running := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		_, holds := slotted[issue.Key]
		if facts.Chains.GateRoot(issue.Key) == issue.Key && TreeRuns(issue, facts.Gates[issue.Key], holds) {
			running[issue.Key] = struct{}{}
		}
	}
	return running
}

// StartedRoots is the set of the keys among issues that are gate roots on the pass's chains
// (Chains.GateRoot) and have made their first start (TreeStarted), read from the records and the
// pass's own facts already in hand.
func StartedRoots(issues []Issue, facts LineFacts) map[string]struct{} {
	started := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		if facts.Chains.GateRoot(issue.Key) == issue.Key && TreeStarted(issue, facts.Gates[issue.Key]) {
			started[issue.Key] = struct{}{}
		}
	}
	return started
}

// ReadRunningRoots is RunningRoots for a caller that holds no facts of the pass: the chains of
// project with every key of issues named and the gates on them (readChainFacts), and the slot
// rows, five reads whatever the number of roots, and then the one rule. It answers the chains it
// read with them, so a caller asks its other questions of the issues' chains of the same read.
// project is the Dispatch project key and steps the design-gate steps the deployment gates.
func ReadRunningRoots(ctx context.Context, tx pgx.Tx, store Store, project string, issues []Issue, steps []DesignStep) (map[string]struct{}, *Chains, error) {
	chains, gates, err := readChainFacts(ctx, tx, store, project, issues, steps)
	if err != nil {
		return nil, nil, err
	}
	slots, err := store.Slots(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	return RunningRoots(issues, slots, LineFacts{Chains: chains, Gates: gates}), chains, nil
}
