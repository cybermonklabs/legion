package record

import (
	"cmp"
	"slices"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/signals"
)

// SlotRule is one rule of which issues hold one of the cap's admission slots and how the waiting
// line gives the slots out: the phases that keep a slot, which waits on another issue give it
// back and after how long, whether such a wait stops the phase under way, the order of the line,
// what a start may take from the cap, what a head that conflicts with its base does, and where
// inside merging the slot goes back. It is a value with a name, so that the daemon runs its own
// rule (MainRule) and a replay of a recorded window runs the daemon's own admission pass under
// another rule the repository keeps (SlotRules), and a rule is read by name rather than copied
// into a script.
//
// Every reader of the rule takes it from the pass, the engine or the projection it was given to
// (workflow.Config.SlotRule, which the admission pass reads through its engine, LineFacts.Rule):
// no reader reads a rule of its own choosing, and no rule is held in a package variable a reader
// could change, so two replays under two rules can run at once in one process.
type SlotRule struct {
	// Name is the rule's name, the word a caller picks it by (SlotRuleNamed). The zero value's
	// empty name is the daemon's own rule (OrMain).
	Name string
	// Commit is the commit of cybermonklabs/legion the rule's choices come from, so a reader of a
	// replay's result can read the code that made them.
	Commit string
	// KeepsSlotIn is every phase in which an issue the daemon has started keeps its slot
	// (SlotRule.PhaseKeepsSlot). A phase it leaves out holds none, and the start of that phase's
	// worker waits for no slot (workflow.StartOrOwe).
	KeepsSlotIn []phase.Phase
	// LinkWaitGivesBack says whether a started issue that waits on an unfinished Dispatch
	// blocked_by link gives its slot back for the wait (blockers.waitsOf).
	LinkWaitGivesBack bool
	// ChildrenWaitGivesBack says whether a started parent that waits on its open Dispatch
	// children gives its slot back for the wait (blockers.waitsOf).
	ChildrenWaitGivesBack bool
	// WaitGrace is how long a wait on another issue that gives the slot back (LinkWaitGivesBack,
	// ChildrenWaitGivesBack) lasts before it does: a slot holder whose wait is younger than the
	// grace keeps its slot through it, as a holder keeps it through a wait the rule keeps the slot
	// for (blockers.KeptWaits), and the first admission pass at or after the wait's start plus the
	// grace gives the slot back as a rule with no grace gives it back at once (LineFacts.InWaitGrace,
	// blockers.waitsOf). The wait's start is the pass that first found the holder waiting while it
	// kept its slot, which marks it on the slot (Slot.WaitsSince). No pass is asked for at the
	// grace's end, since the daemon has no wake an admission pass can ask for at a moment of its
	// choosing: the slot goes back at the next pass after the grace, which every fact of the
	// project and every controller tick makes, and that delay is the rule's own (GraceOver). Zero,
	// the daemon's own and every rule's but wait-grace's and wait-grace-1h's, gives the slot back
	// at the pass that finds the wait.
	WaitGrace time.Duration
	// WaitStopsPhase says whether an open Dispatch child of a started parent stops the phase under
	// way, cybermonklabs/legion#4592's wait (its spec, R9 to R13): under a rule where it
	// does, a parent whose phases have started and that stands in planning through accepting
	// (OpenChildHolds), its pull request not merged, moves into awaiting_children, a phase that
	// keeps no slot, in place of each start an open child holds and in the middle of a phase the
	// admission pass finds it in; its worker is told the phase ended and its task is retired; and
	// once no child of it is open the pass returns it to the start it held, or to the first step
	// after the implementer's for a held test, review or acceptance (ReturnStep). The started
	// parents are judged apart from the children-first wait of a first phase (StartedParents,
	// LineFacts.StartedParentWaits), the engine's guard on every phase start reads it
	// (workflow's childrenHoldingStart), and the admission pass moves and returns the parents
	// (admit's childrenWaits). A blocked_by link stops no phase under any rule. Under a rule where it
	// does not, the rules the recorded windows ran before #4592, an open child holds no start of a
	// started parent, whose phases go on as they did.
	WaitStopsPhase bool
	// Order is the order of the waiting line (Waiting), within each priority's band where the rule
	// bands the line (PriorityFirst).
	Order LineOrder
	// PriorityFirst says whether the waiting line is banded by priority before its order: each
	// entry stands in the band of the priority that places it (PriorityOf: the issue's own Dispatch
	// priority, else its tree root's), P0 first and an entry with neither after P3, and within one
	// band the line keeps Order (Waiting, cybermonklabs/legion#5365). The daemon's own rule bands
	// the line, and so does every rule built as the daemon's own with one other choice changed; the
	// rules the recorded windows ran do not, since their releases came before the band.
	PriorityFirst bool
	// Take is what a start may take from the cap (admit's capLets).
	Take StartTake
	// Conflict is what a head that conflicts with its base does once a second read confirms the
	// conflict (workflow's mergeability, TableFor). Past the review it is also what the decision on
	// the conflict answers (classify.ConflictNeedsMergeUnder, ConflictWaitsForMerging), whom the
	// merge forward asks (MergeForwardAsker) and what a move past the review at a conflicting head
	// does (workflow's mergeForwardOnEntry).
	Conflict ConflictPath
	// ReleaseAtReady says whether an issue in merging gives its slot back at the merger's ready
	// mark: the merger's completion of merging, which records the head it stands on
	// (PhaseRow.HandoffCommit) while the issue waits in merging for the merge itself. It is read
	// only under a rule that keeps a slot in merging (KeepsSlotIn).
	ReleaseAtReady bool
	// P0AboveCap says whether an issue that is P0 starts outside the cap, R10 of the tool's spec:
	// the cap counts only the holders that did not start outside it, so the pass that would start
	// such an issue but for the cap starts it, whatever the number of slots in use, while fewer such
	// issues than the P0 bound in force hold a slot, or always where none is in force; and every
	// other hold on a start holds it as before. Under it the priority Dispatch resolves decides: the
	// issue's own, else that of the nearest issue above it on its Dispatch parent chain that has one,
	// in any project, for the P0 start, the line's priority order and the holder counts; where
	// Dispatch supplies none, as to this branch's daemon, whose record keeps the own priority alone,
	// the issue's own priority stands in (decidingPriority), and a replay under the field supplies
	// the resolved one. Below P0 the cap holds as the daemon's own rule holds it. Every reader reads
	// it through StartsOutsideCap and HoldersOf (p0_start.go), whose doc says what replaces them
	// when cybermonklabs/legion#5488 merges; until then it is off in MainRule and the recorded
	// rules, and on in p0-at-once, p0-idle, signals and signals-backoff.
	P0AboveCap bool
	// IdleReturn is how long a slot holder that waits on its children or on a person may go with
	// no turn of any of its roles before it gives its slot back (LineFacts.IdleReturns, admit's
	// releaseIdleSlots). Such a holder carries the mark of a release for a wait
	// (Issue.ReleasedForWaitAt), and the line keeps its place by that mark, ahead of every slot
	// given back after it, while it waits (Waiting). Zero, the daemon's own and every rule's but
	// idle-return-1h's and p0-idle's, gives no slot back for idleness.
	IdleReturn time.Duration
	// Signals is the mode in which a pass under the rule admits below P0 by signals, as the
	// tool's spec R11 gives it (signals.Ceiling: the cap becomes a ceiling, and a start needs
	// each signal that has a limit under it; signals.Backoff: an allowance replaces the
	// ceiling), read through SignalsLet by admit's capLets. The pass reads the signals from the
	// snapshot its caller supplied (admit's UseSignals); with none, or one that is off, the
	// pass admits under the cap as the daemon's own rule does. signals.Off, the daemon's own and
	// every rule's but signals' and signals-backoff's, names no mode, so the pass takes the
	// snapshot's, which no caller supplies in the daemon until it is turned on.
	Signals signals.Mode
}

// LineOrder is the order of the waiting line, a SlotRule's choice.
type LineOrder string

const (
	// LineReturnedOldestFirst is the daemon's own order: first the issues whose work came back,
	// oldest first, then the issues that have not started, by their tree root's Dispatch rank
	// (Waiting, WaitingLineBand).
	LineReturnedOldestFirst LineOrder = "returned_oldest_first"
	// LineReturnedByRank places an issue whose work came back by its board rank (RankLess over
	// its tree root's record and then its own) rather than by the moment its work came back.
	LineReturnedByRank LineOrder = "returned_by_rank"
	// LineBlockersFirst is the daemon's own order with the issues that a started issue waits on
	// placed first among the issues that have not started.
	LineBlockersFirst LineOrder = "blockers_first"
	// LineReturnedByFirstSlot places an issue whose work came back by the moment of its first slot
	// in its current generation (LineFacts.FirstSlots), the earliest first, rather than by the
	// moment its work came back: an issue the daemon started early that gave its slot back stands
	// ahead, when its work comes back, of the returned issues it started before.
	LineReturnedByFirstSlot LineOrder = "returned_by_first_slot"
	// LineReworkFirst is first in, first out for work already started: the issues whose work came
	// back go first, each by the moment of its first slot in its current generation
	// (LineFacts.FirstSlots), as LineReturnedByFirstSlot places them; then the issues of trees
	// already open that have not started themselves (LineFirstPhase), each tree by the moment its
	// root first took a slot in its current generation and each issue within a tree by its own
	// arrival in the line (LineFacts.Arrivals); and only then the trees not yet opened
	// (LineQueuedRoot), by the board's rank.
	LineReworkFirst LineOrder = "rework_first"
	// LineArrival is one queue in arrival order, with no part ahead of another and no rank: every
	// issue of the line, started or not, by the moment it arrived in the line in its current
	// generation (LineFacts.Arrivals), first in, first out.
	LineArrival LineOrder = "arrival"
)

// StartTake is what a start may take from the cap, a SlotRule's choice.
type StartTake string

const (
	// TakeOwnSlot is the daemon's own: a start that needs a slot takes one of its own, and only
	// while the cap has one free.
	TakeOwnSlot StartTake = "own_slot"
	// TakeCover lets an issue that a counted waiter waits on start on that waiter's slot whatever
	// the cap says, one such cover for each holder: the waiter gives its slot to the issue, which
	// holds it for the waiter until the issue is done or the waiter no longer waits on it, and the
	// waiter takes a slot of its own back at once then (LineFacts.Covers, admit's covers).
	TakeCover StartTake = "cover"
)

// ConflictPath is what a head that conflicts with its base does, a SlotRule's choice.
type ConflictPath string

const (
	// ConflictMergeForward is the daemon's own (cybermonklabs/legion#4979): the issue keeps its
	// phase and the base is merged forward (workflow's mergeForward). Up to the review the
	// implementer is asked at once. Past it, in merging the merger is asked, and in accepting or
	// retro, where the merge comes next before merging (classify.MergeComesNext), the head is
	// recorded conflicting, nobody is asked, and the issue's move into merging asks the merger.
	ConflictMergeForward ConflictPath = "merge_forward"
	// ConflictMovesBack moves the issue back to implementing by workflow.TriggerConflict, from
	// accepting, retro and merging as from testing and reviewing.
	ConflictMovesBack ConflictPath = "moves_back_conflict"
	// ConflictChecksRed moves the issue back to implementing by workflow.TriggerChecksRed, from
	// accepting, retro and merging as from testing and reviewing.
	ConflictChecksRed ConflictPath = "moves_back_checks_red"
	// ConflictImplementerMergesForward is cybermonklabs/legion#4464 without #4979, the path of
	// commit bafd3618: the issue keeps its phase in every phase, accepting, retro and merging
	// included, and the implementer is asked at once to merge the base forward there as well
	// (SlotRule.MergeForwardAsker). The merge forward past the review writes no supervise row and
	// takes no slot, as every merge forward does (workflow's mergeForward).
	ConflictImplementerMergesForward ConflictPath = "implementer_merges_forward"
)

// MainRule is the daemon's own rule, the one it is built with: a slot is kept from the start
// through the review and in held, and none past the review nor in awaiting_children; a link wait
// and a children wait give the slot back at the pass that finds them, with no grace; an open
// Dispatch child of a started parent stops the phase under way and moves the parent into
// awaiting_children (cybermonklabs/legion#4592); the line stands by priority first
// (cybermonklabs/legion#5365) and, within one priority, work that came back goes first, oldest
// first, then the line by rank; a start takes a slot of its own, whatever its priority, since no
// issue starts outside the cap (P0AboveCap) until R10 reaches the daemon in
// cybermonklabs/legion#5488; a conflicting head keeps its phase while the base is merged forward,
// and past the review waits for merging, where the merger merges it (cybermonklabs/legion#4979);
// and no slot goes back because its holder is idle. Its commit is the commit the rule was first
// written as a value at; the rule is the daemon's at the tool's commit, which each replay run
// records, so since cybermonklabs/legion#4592 it moves a started parent with an open child into
// awaiting_children.
var MainRule = SlotRule{
	Name:   "main",
	Commit: "0d3786da536722f2eb26a793ab1b9827b6e90f47",
	KeepsSlotIn: []phase.Phase{phase.Admitted, phase.AwaitingPerson, phase.Planning, phase.Implementing, phase.Testing,
		phase.Reviewing, phase.Held},
	LinkWaitGivesBack:     true,
	ChildrenWaitGivesBack: true,
	WaitGrace:             0,
	WaitStopsPhase:        true,
	Order:                 LineReturnedOldestFirst,
	PriorityFirst:         true,
	Take:                  TakeOwnSlot,
	Conflict:              ConflictMergeForward,
	ReleaseAtReady:        false,
	P0AboveCap:            false,
	IdleReturn:            0,
}

// OrMain is r, or MainRule where r is the zero value a caller that names no rule holds: a
// workflow.Config, a LineFacts or an admission built without one runs the daemon's own rule.
func (r SlotRule) OrMain() SlotRule {
	if r.Name == "" {
		return MainRule
	}
	return r
}

// PhaseKeepsSlot says whether an issue standing at at keeps its slot under r (KeepsSlotIn).
// MainRule's list, the daemon's own, is the phases that hold one of the cap's slots: admitted,
// where a root's architect writes its design and a started child waits to be entered again;
// awaiting_person; the four phases a step's worker runs besides the owner's acceptance —
// planning, implementing, testing and reviewing; and held, which is reached here only where the
// record names no earlier phase for the hold (StandsAt). Every other phase holds none: accepting,
// retro and merging (in the default steps, the phases after the review); done, which has
// finished; and awaiting_children, a parent waiting on its open children, which work under slots
// of their own (the spec of #4592, R11). A step after accepting whose phase keeps a slot waits
// for one again (workflow.StartOrOwe).
//
// It is written as one list, in the rule value, so that a later change adds or removes one entry
// and every reader of the rule moves with it. Two read it, and a phase in one and not the other
// would be a deadlock: the cap's own half (KeepsSlot) and the mark a start of a phase worker
// carries for the slot it waits for (workflow.StartOrOwe), whose row the outbox holds unclaimable
// while the issue holds no slot. InSlotlessPhase is the complement of the working phases a rule
// refuses, for the repair that must reach an issue holding no row.
func (r SlotRule) PhaseKeepsSlot(at phase.Phase) bool {
	return slices.Contains(r.OrMain().KeepsSlotIn, at)
}

// ReadsReadyMark says whether r reads the merger's ready mark (LineFacts.Ready): it gives a slot
// back there (ReleaseAtReady) and keeps one in merging, where the mark is made. The read of the
// line's facts reads the mark under such a rule alone (ReadLineFactsUnder), and so do a replay's
// own tables, which keep the mark of a release that made it by a move.
func (r SlotRule) ReadsReadyMark() bool {
	r = r.OrMain()
	return r.ReleaseAtReady && r.PhaseKeepsSlot(phase.Merging)
}

// GraceOver says whether a wait on another issue that began at since has lasted r's grace by now
// (WaitGrace), so that it gives the slot back: at once under a rule with no grace, and from since
// plus the grace on under one with a grace. The pass that gives the slot back is the first at or
// after that moment, so the delay to it is the rule's own.
func (r SlotRule) GraceOver(since, now time.Time) bool {
	return !now.Before(since.Add(r.OrMain().WaitGrace))
}

// ConflictWaitsForMerging says whether, under r, a confirmed conflict where the merge comes next
// before merging, in retro or in accepting where no step of the task follows the acceptance
// (classify.MergeComesNext), records the head conflicting, asks nobody and waits for the issue's
// move into merging, where the merger is asked to merge the base forward: the daemon's own path
// (ConflictMergeForward, cybermonklabs/legion#4979), which a rule that names no path runs too.
// Under every other path a conflict in accepting, retro or merging is acted on at once: by the move
// back of a path that moves back (ConflictMovesBack, ConflictChecksRed), or by the implementer's
// merge forward (ConflictImplementerMergesForward). It is read by the decision on a confirmed
// conflict (classify.ConflictNeedsMergeUnder), by a move past the review at a conflicting head
// (workflow's mergeForwardOnEntry) and by a replay's conflict (replay's conflictFor).
func (r SlotRule) ConflictWaitsForMerging() bool {
	return r.conflictPath() == ConflictMergeForward
}

// MergeForwardAsker is the role r's conflict path asks to merge the base forward while the issue
// stands in at: the implementer in every phase under ConflictImplementerMergesForward, and under
// every other path the daemon's own answer (the package's MergeForwardAsker: the merger in merging,
// and the implementer before it). The engine's merge forward asks this role (workflow's
// mergeForward).
func (r SlotRule) MergeForwardAsker(at phase.Phase) claim.Role {
	if r.conflictPath() == ConflictImplementerMergesForward {
		return claim.RoleImplementer
	}
	return MergeForwardAsker(at)
}

// conflictPath is r's conflict path (Conflict), the daemon's own for a rule that names none.
func (r SlotRule) conflictPath() ConflictPath {
	return cmp.Or(r.OrMain().Conflict, ConflictMergeForward)
}

// KeepsSlot is the phase half of r's admission rule, as the package's KeepsSlot is of the
// daemon's own: issue is in the workflow (InWorkflow) and stands, as StandsAt reads it, in a
// phase r keeps a slot in.
func (r SlotRule) KeepsSlot(issue Issue) bool {
	return InWorkflow(issue) && r.PhaseKeepsSlot(StandsAt(issue))
}

// NeedsSlot says whether issue must hold one of the cap's slots now under r, as the package's
// NeedsSlot says it under the daemon's own rule: its tree is live, its root does not linger, and
// it keeps a slot where it stands (KeepsSlot).
func (r SlotRule) NeedsSlot(issue, root Issue, live bool) bool {
	return live && !root.Lingers() && r.KeepsSlot(issue)
}

// InSlotlessPhase is r's phase half read the other way round over the working phases: issue
// stands, as StandsAt reads it, in a phase of phase.Order that r keeps no slot in, so it holds
// none of the cap's slots there while its work goes on. Under MainRule those phases are
// accepting, retro and merging (the package's InSlotlessPhase); under a rule that keeps a slot in
// one of them, or keeps none in a working phase before them, it names the working phases that
// rule leaves without one, whatever their place in the workflow.
//
// It is what the two readers of "holds no slot because its phase keeps none" ask: the repair of
// a worker with no task, whose own domain is the slot holders and which must reach such an issue
// besides (admit's readSlotHolders), and the state's answer of `why`, which serves such an issue
// at work no answer (projection's atWorkInSlotlessPhase). The readers that mean where the merge
// comes next on the task's own line, whatever slot the issue holds, read the workflow's walk and
// no phase of the rule: classify.MergeComesNext, and the decision on a conflict and the merge
// forward (classify.ConflictNeedsMergeUnder, workflow's merge forward), which read the rule's
// conflict path alone (Conflict).
func (r SlotRule) InSlotlessPhase(issue Issue) bool {
	at := StandsAt(issue)
	return slices.Contains(phase.Order, at) && !r.PhaseKeepsSlot(at)
}

// ChildrenFirstCandidates is every issue of issues whose own first phase has not started yet and
// could, by key and sorted: it stands in the workflow, its tree is recorded, not lingering and
// under an open design gate, and it stands at phase.Admitted, its own first phase still to start.
// Those are the only issues whose first phase r's children-first rule can hold, so they are the
// only parents whose Dispatch children this judgement reads (readChildWaits). A parent whose
// phases started is read by StartedParents, since an open child holds every later phase start of
// it too under a rule whose wait stops the phase under way (WaitStopsPhase, the spec of #4592,
// R9). The keys come out sorted, so one pass reads the same set in the same order whatever order
// the records arrived in.
//
// The open gate is what puts the start of the wait where the spec puts it: at the gate's opening,
// before the architect's design-approved turn releases the wave. Before that opening a root's
// architect is still writing the spec and its own first phase is nothing the line would offer a
// slot to, so a root there waits for nothing and is told nothing; from the opening the root and
// every released member of its tree are issues whose first phase the line does offer a slot to,
// and each of them waits for whatever Dispatch still shows hanging under it.
//
// Under a rule whose start may take a cover (TakeCover) the gate's term is that it has opened at
// some time (TreeGate.EverOpened): a parent's wait on its children stands from the first opening
// of its design gate, also while a new spec version has closed the gate again, so a cover a child
// holds for its parent does not end with the version.
func (r SlotRule) ChildrenFirstCandidates(issues []Issue, gates map[string]TreeGate) []string {
	everOpened := r.OrMain().Take == TakeCover
	records := byKey(issues)
	keys := make([]string, 0, len(issues))
	for _, issue := range issues {
		// The root must be among the records and must not linger. A root whose tree has left the
		// workflow is no part of a live read at all — its phase is done and its status is not
		// todo (Store.LiveIssues) — so an unrecorded root is a tree that has gone, and reading
		// the zero record's linger would answer false for exactly the case this test is for.
		root, recorded := records[issue.Tree]
		if issue.Phase != phase.Admitted || !InWorkflow(issue) || !recorded || root.Lingers() {
			continue
		}
		if gate := gates[issue.Tree]; !gate.Open() && !(everOpened && gate.EverOpened()) {
			continue
		}
		keys = append(keys, issue.Key)
	}
	slices.Sort(keys)
	return keys
}

// SlotRules is every named rule the repository keeps, the daemon's own first, and then the rules
// the replay's comparison runs beside it: the rules three recorded windows ran under
// (legion-v20.42.5, legion-v19.57.3, legion-v22.14.1) and the sixteen candidates of the
// comparison (keep-past-review, blockers-first, returned-by-rank, cover, wait-grace and
// wait-grace-1h, one rule at two graces, returned-by-first-slot, rework-first and
// rework-first-keep, one order with and without the slot kept past the review, arrival-order,
// before-p0, the daemon's own with no P0 started outside the cap, measured for reference,
// p0-at-once, idle-return-1h and p0-idle, two answers apart and together, and signals and
// signals-backoff, which admit below P0 by signals). The daemon's own rule
// orders the line by priority first since cybermonklabs/legion#5365, which the comparison ran as
// the candidate priority-first before that change merged, and stops a started parent's phase for
// its open children since cybermonklabs/legion#4592 (WaitStopsPhase); each candidate, built from
// the daemon's own, keeps both, and the recorded rules, whose releases came before them, keep
// neither. `legion replay rules` prints them. Each answer is built afresh, so a caller that
// changes a rule it was handed changes no other caller's.
func SlotRules() []SlotRule {
	return []SlotRule{mainRuleWith(MainRule.Name, func(*SlotRule) {}), windowOneRule(), windowTwoRule(), windowThreeRule(),
		keepPastReviewRule(), blockersFirstRule(), returnedByRankRule(), coverRule(),
		waitGraceRule("wait-grace", waitGrace), waitGraceRule("wait-grace-1h", longWaitGrace), returnedByFirstSlotRule(),
		reworkFirstRule(), reworkFirstKeepRule(), arrivalOrderRule(), beforeP0Rule(), p0AtOnceRule(), idleReturnRule(), p0IdleRule(),
		signalsRule(), signalsBackoffRule()}
}

// The commits the recorded rules come from: release 20.42.5, the last a deployment ran over its
// window one, whose slot rule is the one 20.25.1 (52b3cf5c) ran before it and whose transition
// table carries the five rows that move a conflicting head back to implementing; release 19.57.3,
// which the same deployment ran over its window two; and release 22.14.1, which it ran from the
// start of its window three.
const (
	conflictMovesBackCommit        = "958173da0ea4cd03290a1964edf9eb92e6685200"
	awaitingMergeCommit            = "9542c6fbbecd184ccee73523ab3defa39d6cf23a"
	implementerMergesForwardCommit = "bafd3618eb58722419c77f34174c1b77525c891b"
)

// mainRuleWith is MainRule under name, moved by change and nothing else, its phases a copy: the
// one way a rule that is the daemon's own with one choice changed is built, so its other choices
// are the daemon's own by construction. Its commit is MainRule's, the code its every other choice
// comes from.
func mainRuleWith(name string, change func(*SlotRule)) SlotRule {
	rule := MainRule
	rule.Name = name
	rule.KeepsSlotIn = slices.Clone(MainRule.KeepsSlotIn)
	change(&rule)
	return rule
}

// everyWorkflowPhase is every phase an issue the daemon has started stands in until it is done:
// the phases a rule that keeps a started issue's slot through its whole work keeps one in.
func everyWorkflowPhase() []phase.Phase {
	return []phase.Phase{phase.Admitted, phase.AwaitingPerson, phase.Planning, phase.Implementing, phase.Testing,
		phase.Reviewing, phase.Accepting, phase.Retro, phase.Merging, phase.Held}
}

// windowOneRule is the rule named legion-v20.42.5, which releases 20.25.1 and 20.42.5 ran: the
// daemon's own slot choices — no slot past the review (#3573) and none during a wait on another
// issue (#3715) — and a head that conflicts with its base moving back to implementing by workflow's
// TriggerConflict, the rows of that release's transition table the daemon's own table no longer
// carries (workflow.TableFor), where main keeps the phase and merges the base forward (#4464). Its
// line stands by no priority (PriorityFirst), as every release before #5365's, and no open child
// stops a started parent's phase (WaitStopsPhase), as in every release before #4592's.
func windowOneRule() SlotRule {
	return mainRuleWith("legion-v20.42.5", func(rule *SlotRule) {
		rule.Commit = conflictMovesBackCommit
		rule.Conflict = ConflictMovesBack
		rule.PriorityFirst = false
		rule.WaitStopsPhase = false
	})
}

// windowTwoRule is the rule named legion-v19.57.3, which release 19.57.3 ran (#2751 and #3206): a
// started issue keeps its slot in every phase of the workflow until the merger's ready mark inside
// merging, which that release's awaiting_merge was and a replay reads as a mark (ReleaseAtReady,
// LineFacts.Ready); no wait on another issue gives the slot back; red checks, that release's one
// path of a conflicting head, move the issue back to implementing by workflow's TriggerChecksRed;
// and the line stands by no priority (PriorityFirst) and no open child stops a started parent's
// phase (WaitStopsPhase).
func windowTwoRule() SlotRule {
	return mainRuleWith("legion-v19.57.3", func(rule *SlotRule) {
		rule.Commit = awaitingMergeCommit
		rule.KeepsSlotIn = everyWorkflowPhase()
		rule.LinkWaitGivesBack, rule.ChildrenWaitGivesBack = false, false
		rule.Conflict = ConflictChecksRed
		rule.ReleaseAtReady = true
		rule.PriorityFirst = false
		rule.WaitStopsPhase = false
	})
}

// windowThreeRule is the rule named legion-v22.14.1, which release 22.14.1 ran: the daemon's own
// slot choices, and the conflict path of cybermonklabs/legion#4464 without #4979's
// (ConflictImplementerMergesForward): a conflicting head keeps its phase in every phase, accepting,
// retro and merging included, and the implementer is asked at once to merge the base forward,
// where main leaves a conflict in accepting or retro to wait for merging and asks the merger there.
// Its line stands by no priority (PriorityFirst), and no open child stops a started parent's
// phase (WaitStopsPhase).
func windowThreeRule() SlotRule {
	return mainRuleWith("legion-v22.14.1", func(rule *SlotRule) {
		rule.Commit = implementerMergesForwardCommit
		rule.Conflict = ConflictImplementerMergesForward
		rule.PriorityFirst = false
		rule.WaitStopsPhase = false
	})
}

// keepPastReviewRule is the daemon's own rule with the slot kept in accepting, retro and merging
// too, so a send-back past the review finds its issue holding its slot. A parent with an open
// Dispatch child still moves into awaiting_children, from accepting too, and gives its slot back
// there, as under the daemon's own rule (WaitStopsPhase).
func keepPastReviewRule() SlotRule {
	return mainRuleWith("keep-past-review", func(rule *SlotRule) {
		rule.KeepsSlotIn = everyWorkflowPhase()
	})
}

// blockersFirstRule is the daemon's own rule with each issue that a started issue waits on placed
// first among the issues that have not started (LineBlockersFirst).
func blockersFirstRule() SlotRule {
	return mainRuleWith("blockers-first", func(rule *SlotRule) {
		rule.Order = LineBlockersFirst
	})
}

// returnedByRankRule is the daemon's own rule with an issue whose work came back placed in the
// line by its board rank rather than by the moment it gave its slot back (LineReturnedByRank).
func returnedByRankRule() SlotRule {
	return mainRuleWith("returned-by-rank", func(rule *SlotRule) {
		rule.Order = LineReturnedByRank
	})
}

// coverRule is the rule of the tool's spec at version 4 and its decisions at version 1: a started
// issue keeps its slot in every phase until its work is done, through every wait on another issue,
// for its children and for a link alike; an issue a counted waiter waits on starts at once on that
// waiter's slot, which it then holds for the waiter, one waiter for each holder, and the waiter is
// not counted while the cover stands (TakeCover; admit's covers). A parent with an open Dispatch
// child is such a waiter in whatever phase it stands: it is not moved into awaiting_children
// (WaitStopsPhase off, where every other rule built from the daemon's own moves it since
// cybermonklabs/legion#4592), it keeps its phase, and its open children are waits a cover reads
// (StartedParents, blockers.CoverWaits), so a child of it that has not started starts on its slot
// and holds it for the parent while the cover stands, as an issue it waits on through a blocked_by
// link does. The line keeps the daemon's own order: the
// issues that came back oldest first and the rest by the board's rank, with nothing put ahead of
// the rank. A conflicting head keeps its phase, as on main, which the spec was written against.
func coverRule() SlotRule {
	return mainRuleWith("cover", func(rule *SlotRule) {
		rule.KeepsSlotIn = everyWorkflowPhase()
		rule.LinkWaitGivesBack, rule.ChildrenWaitGivesBack = false, false
		rule.WaitStopsPhase = false
		rule.Take = TakeCover
	})
}

// waitGraceRule is the rule named name, the daemon's own rule with a wait on another issue that
// gives the slot back only once it has lasted grace (WaitGrace): a holder keeps its slot through a
// shorter wait, which neither frees a slot nor sends the holder to the line, and a longer one gives
// it back at the first pass at or after the grace's end, as the daemon's rule gives it back at
// once. The repository keeps it at two graces, wait-grace (waitGrace) and wait-grace-1h
// (longWaitGrace), so the comparison shows what a longer grace keeps and costs. Its reason is a
// deployment's reported cases: a started issue's blocked_by link set and taken off within a minute
// took its slot back and placed it last of 79 in the line's returned part, which orders by the
// moment the work came back, so one minute of a wait put work in progress behind every issue that
// came back before it. The grace is a link wait's and a children wait's of a first phase
// (LinkWaitGivesBack, ChildrenWaitGivesBack): a started parent with an open Dispatch child moves
// into awaiting_children at once, as under the daemon's own rule (WaitStopsPhase), whose phase
// keeps no slot whatever the grace.
func waitGraceRule(name string, grace time.Duration) SlotRule {
	return mainRuleWith(name, func(rule *SlotRule) {
		rule.WaitGrace = grace
	})
}

// waitGrace is the grace of the rule wait-grace, five minutes, read from the recorded window the
// comparison names window one (94 hours at a cap of 157). There a slot that came free was taken in
// the pass that freed it (776 of 819 releases), so the median time from a slot's release to its
// next taking, 19 microseconds, names no grace; the median time from one pass that freed a slot to
// the next was 4 minutes 44 seconds (760 such passes), the moment a slot next comes free, and five
// minutes is that time rounded up. Of the 114 waits on another issue that gave a slot back there,
// one lasted under a minute (15 seconds) and the next shortest 14 minutes, the median 20 hours, so
// five minutes keeps the slot through the one-minute class and through no longer wait of the
// window, at a cost of five more minutes of slot for each longer wait: 9.4 slot-hours of the
// window's 14,768. The pass that gives the slot back is the first after the grace's end, the
// rule's own delay: over that window's waits the next pass a replay makes (an event, a recorded
// move, a slot moved or a tick) came a median 2.6 seconds after the grace's end, at most 31.5.
const waitGrace = 5 * time.Minute

// longWaitGrace is the grace of the rule wait-grace-1h, one hour: wait-grace at a longer grace, to
// show beside it what a longer grace keeps and what it costs. Of the same 114 waits of window one,
// 8 ended within an hour (7.0%) and one more stood open at the window's end after less than an
// hour, so an hour keeps the slot through 9 (7.9%), where five minutes keeps it through 1; and each
// wait holds its slot for the shorter of its length and the hour, 109.28 slot-hours in all, where
// five minutes holds 9.42. The comparison reads both from each window's record (replay's
// graceKeeps).
const longWaitGrace = time.Hour

// returnedByFirstSlotRule is the daemon's own rule with an issue whose work came back placed in the
// line by the moment of its first slot in its current generation (LineReturnedByFirstSlot) rather
// than by the moment its work came back. Its reason is a deployment's reported cases: a started
// issue sent back from merging for one forward merge stood last of 76 in the line's returned part,
// and one whose one-minute wait gave its slot back last of 79, since that part orders by the moment
// the work came back; under this rule each stands where its first admission puts it, ahead of the
// returned work the daemon started after it. The daemon records no first slot once a slot goes
// back (its slot row goes with it), so a replay keeps the first slots in a table of its own
// (LineFacts.FirstSlots, admit's RuleTables), and a deployment that adopts this rule needs that
// record in the daemon's own store first.
func returnedByFirstSlotRule() SlotRule {
	return mainRuleWith("returned-by-first-slot", func(rule *SlotRule) {
		rule.Order = LineReturnedByFirstSlot
	})
}

// reworkFirstRule is the daemon's own rule with the line ordered first in, first out for work
// already started (LineReworkFirst): work that came back is admitted again before any new work,
// by its first slot in its current generation; then the work of trees already open, each tree by
// its root's first slot and each issue by its arrival in the line; and the board decides only
// among the trees not yet opened. Its reason is an owner's: work already started should be burned
// down first, and the board's rank is not trusted to be kept up to date. The daemon records no
// first slot once a slot goes back and no arrival, so a replay keeps both in tables of its own
// (LineFacts.FirstSlots, LineFacts.Arrivals; admit's RuleTables), and a deployment that adopts
// this rule needs those records in the daemon's own store first.
func reworkFirstRule() SlotRule {
	return mainRuleWith("rework-first", func(rule *SlotRule) {
		rule.Order = LineReworkFirst
	})
}

// reworkFirstKeepRule is rework-first with the slot kept in accepting, retro and merging too, as
// keep-past-review keeps it (LineReworkFirst, everyWorkflowPhase), so work sent back past the review
// never waits for a slot at all, and the comparison reads the cost of returning work under the
// owner's order with and without that candidate's slot. A parent with an open Dispatch child still
// moves into awaiting_children and gives its slot back there, as under the daemon's own rule
// (WaitStopsPhase).
func reworkFirstKeepRule() SlotRule {
	return mainRuleWith("rework-first-keep", func(rule *SlotRule) {
		rule.Order = LineReworkFirst
		rule.KeepsSlotIn = everyWorkflowPhase()
	})
}

// arrivalOrderRule is the daemon's own rule with the line one queue in arrival order and no rank
// at all (LineArrival): the plainest first in, first out, beside which every rule that reads the
// board shows what the board's order changes. The daemon records no arrival, so a replay keeps the
// arrivals in a table of its own (LineFacts.Arrivals, admit's RuleTables), and a deployment that
// adopts this rule needs that record in the daemon's own store first.
func arrivalOrderRule() SlotRule {
	return mainRuleWith("arrival-order", func(rule *SlotRule) {
		rule.Order = LineArrival
	})
}

// beforeP0Rule is the daemon's own rule with no issue started outside the cap (P0AboveCap off): the
// rule before R10, under which a P0 issue waits in the line as any issue does, measured beside the
// rules that start one outside the cap for reference. The daemon's own rule starts none outside the
// cap until cybermonklabs/legion#5488 merges and turns its field on, so until then this rule equals
// main in every choice but its name.
func beforeP0Rule() SlotRule {
	return mainRuleWith("before-p0", func(rule *SlotRule) {
		rule.P0AboveCap = false
	})
}

// p0AtOnceRule is the daemon's own rule with an issue whose own priority is P0 started outside the
// cap (P0AboveCap, StartsOutsideCap): an owner's direction that a P0 issue starts at once, with the
// daemon's own rule below P0. The cap counts the holders below P0 alone, and a replay's P0 bound,
// from its window, limits the P0 holders. It reads the issue's own priority (Issue.Priority),
// which the daemon's store keeps from each Dispatch observation (cybermonklabs/legion#5365). It is
// the daemon's own rule once cybermonklabs/legion#5488 turns main's field on, and goes then.
func p0AtOnceRule() SlotRule {
	return mainRuleWith("p0-at-once", func(rule *SlotRule) {
		rule.P0AboveCap = true
	})
}

// idleReturnRule is the daemon's own rule with a slot holder that waits on its children or on a
// person giving its slot back once none of its roles has had a turn for idleReturn (IdleReturn),
// so that the slot goes to work that can start, while the holder keeps its place in the line.
// cybermonklabs/legion#2751 gave a slot back for those waits, and cybermonklabs/legion#3206 took
// those releases away: the judgement of whether any agent of an issue had work read six states
// and judged wrong in five of them on one day, holding slots whose architects were idle with
// their next step with a person while other issues queued, and the cap was made to count the work
// a project has in hand, a spec in a person's queue among it. This rule judges by no state of
// the work but by the time since the holder's last turn, and only of a holder that waits on its
// children or on a person. Under the daemon's own rule a started parent already gives its slot
// back while it waits for its children, in awaiting_children (WaitStopsPhase), so this rule
// differs from it only for a holder that waits on a person.
func idleReturnRule() SlotRule {
	return mainRuleWith("idle-return-1h", func(rule *SlotRule) {
		rule.IdleReturn = idleReturn
	})
}

// idleReturn is the time without a turn after which a holder gives its slot back under
// idle-return-1h and p0-idle: one hour, the longer of the two graces the comparison runs for a
// wait on another issue (longWaitGrace).
const idleReturn = time.Hour

// p0IdleRule is p0-at-once and idle-return-1h together (P0AboveCap, IdleReturn), so the
// comparison reads whether the two answers add up or overlap.
func p0IdleRule() SlotRule {
	return mainRuleWith("p0-idle", func(rule *SlotRule) {
		rule.P0AboveCap = true
		rule.IdleReturn = idleReturn
	})
}

// signalsRule is the daemon's own rule with R10's start of a P0 issue outside the cap
// (P0AboveCap) and admission by signals below P0 (Signals, signals.Ceiling), a candidate that only
// a replay runs: the next issue in the line's priority order starts while each signal that has a
// limit is under it and the holders below P0 are fewer than the ceiling, which the cap becomes. It
// starts an issue below P0 only at an admission pass that an event or a signal's deadline starts,
// never at the daemon's tick; the tick still gives back each slot the daemon's own rule gives
// back, and starts each P0 issue that R10 starts. No pass takes a slot from a holder because a
// signal is over its limit.
func signalsRule() SlotRule {
	return mainRuleWith("signals", func(rule *SlotRule) {
		rule.P0AboveCap = true
		rule.Signals = signals.Ceiling
	})
}

// signalsBackoffRule is signals with an allowance in place of the ceiling (signals.Backoff,
// signals.Allowance): the allowance starts at the ceiling, is multiplied by 90% in integers,
// rounded down and at least 1, at each refusal by the project's own limit and at each kill, and
// rises by 1 for each hour with neither, up to the ceiling.
func signalsBackoffRule() SlotRule {
	return mainRuleWith("signals-backoff", func(rule *SlotRule) {
		rule.P0AboveCap = true
		rule.Signals = signals.Backoff
	})
}

// SignalsLet is what r decides for an entry below P0 that takes one of the cap's slots at a pass of
// kind, from the snapshot s the pass's caller supplied, the holders below P0 that the cap counts
// (capped) and the cap as a ceiling: signals.Decide in the rule's own mode, or in the snapshot's
// where the rule names none (Signals), so the daemon's own rule admits by signals once its caller
// supplies a snapshot that says so, and every other rule keeps the cap where its caller supplies
// none. It is a pure function of its arguments and decides nothing for an entry that starts outside
// the cap (StartsOutsideCap), which only the P0 bound decides.
func (r SlotRule) SignalsLet(kind signals.PassKind, s signals.Snapshot, capped, ceiling int) signals.Verdict {
	return signals.Decide(r.OrMain().Signals.Or(s.Mode), kind, s, capped, ceiling)
}

// KeepsOwnersDirection says whether r admits as the owner directed on 2026-11-24, the one kind of
// rule a comparison of rules may name best (the replay's spec, R7): R10 above P0 (P0AboveCap), and
// below it the next issue in the line's priority order (PriorityFirst) while each signal of R11 is
// under its limit, with the cap as a ceiling only (Signals, in either of its modes). Of the rules
// the repository keeps, signals and signals-backoff keep it; the daemon's own rule admits below P0
// under the cap, and every other rule is measured beside them.
func (r SlotRule) KeepsOwnersDirection() bool {
	r = r.OrMain()
	return r.P0AboveCap && r.PriorityFirst && r.Signals != signals.Off
}

// SlotRuleNamed is the rule SlotRules keeps under name, and whether it keeps one.
func SlotRuleNamed(name string) (SlotRule, bool) {
	for _, rule := range SlotRules() {
		if rule.Name == name {
			return rule, true
		}
	}
	return SlotRule{}, false
}
