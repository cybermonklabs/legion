package record

import (
	"context"
	"maps"
	"slices"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
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

// Stopped is whether the issue's own record says its work has stopped: it is out of the
// workflow, or its tree lingers. It is the one statement of that rule, and the readers that
// must agree — the boot's claim release pass and the row it queues — both call it.
func Stopped(issue Issue) bool { return !InWorkflow(issue) || issue.Lingers() }

// StandsIn says whether issue stands in one of phases, counting a hold from one of them as
// standing there: a worker whose claim fails leaves its issue at phase.Held with Hold.From
// naming the phase it was working (workflow's claimFailed), and the work has reached exactly
// where it had before. It is the one shape every question about where an issue stands is
// written over — Accepted here, and the workflow's own awaitsMerge, its close of a merged
// issue and its open review round — so no two of those questions read a held issue differently.
func StandsIn(issue Issue, phases ...phase.Phase) bool {
	at := issue.Phase
	if at == phase.Held && issue.Hold != nil {
		at = issue.Hold.From
	}
	return slices.Contains(phases, at)
}

// Accepted says whether the daemon has already accepted issue's own reviewed head and the issue
// has not closed: it stands in its retro, its merge or its wait for that merge, or is held from
// one of them (StandsIn). Work entering the workflow beneath an issue in one of those phases
// would let that issue merge first, which is the one thing a parent must not do
// (dispatch://INFRA-980, "Children first"). The reviewed head of a root whose worker died in
// retro, merging or awaiting_merge is accepted still, so the hold must not let work in beneath
// it: the line held no issue under such a root, the two doors let one in, and the close of a
// root held from retro handed over nobody.
//
// The three phases are named here and nowhere else, so that no rule of the acceptance order can
// read a list of its own: one that did is what let a child held from retro through. Every one
// of those rules asks this predicate instead, through the nearest accepted ancestor
// (AcceptedAncestor): the waiting line's hold (HeldUnderAccepted), the doors' refusal (api's
// CHILD_UNDER_ACCEPTED_PARENT, AcceptedAncestorIn) and the wait a merged issue's close keeps
// (OpenBeneath, which leaves out what that hold stopped) — so none of them can count a root as
// accepted that another counts as still at work.
func Accepted(issue Issue) bool {
	return StandsIn(issue, phase.Retro, phase.Merging, phase.AwaitingMerge)
}

// Ancestors is every issue above issue inside its own tree, nearest first and the tree's own
// root last: the one parent-chain walk every rule that reads "above" is written over, so that
// no two of them answer differently about the same chain. AcceptedAncestor, Above, descends and
// cycleAncestor all read it. byKey is the records the caller holds the chain in — a tree's
// members for a reader of one tree, the project's own for a reader of a whole pass.
//
// The walk ends at the tree's own root, which stands above every member of the tree whatever
// each member's record says it hangs under — the same reading Beneath gives a root, whose
// descendants are every other member of its tree. A chain that leaves byKey, or loops back on
// itself, ends there and the root is read next: a gap in the chain is a gap, not the end of it,
// and the daemon must not judge an issue by how much of the way up to its root it can see.
// Nothing above the root is read at all, because every rule here is a rule about one tree, and
// a root's own Dispatch parent belongs to another: a root has no ancestors here, so the walk is
// never entered for one. A root's recorded parent is a Dispatch fact about where the work was
// filed, not a place in a tree — a root that was handed over keeps the parent it was filed
// under, in a tree that adopted nothing — and read as a chain it put the root's own first phase
// under another tree's acceptance (HeldUnderAccepted) and named a link into that tree a cycle
// (cycleAncestor). The one rule that does read a root's Dispatch chain reads the chain the
// record carries rather than these records (Issue.Ancestry, the ancestry hold in Waiting).
//
// Which records the caller holds is therefore the one thing that can still make two readers
// disagree, and the callers do not hold the same ones: the waiting line is read over the
// project's live records (Store.LiveIssues, which leaves out an issue already done), while the
// two doors that refuse work beneath an accepted ancestor are read over the whole tree
// (Store.TreeIssues, AcceptedAncestorOf, OpenBeneath). So the line's pass reads the records of
// the ancestors its live read leaves out and walks the chain over both (LineFacts.Parents,
// Store.AncestorsOf): without them a parent already done hid every issue above it, and
// admission offered a first-phase slot to a child the doors hold.
func Ancestors(issue Issue, byKey map[string]Issue) []Issue {
	if claim.IsTreeRoot(issue.Key, issue.Tree) {
		return nil
	}
	above := make([]Issue, 0, 4)
	seen := map[string]bool{issue.Key: true}
	for key := issue.Parent; key != nil && !seen[*key]; {
		seen[*key] = true
		parent, recorded := byKey[*key]
		if !recorded {
			break
		}
		above = append(above, parent)
		if parent.Key == issue.Tree {
			return above
		}
		key = parent.Parent
	}
	if root, recorded := byKey[issue.Tree]; recorded && !seen[issue.Tree] {
		above = append(above, root)
	}
	return above
}

// chainOf is the records one admission pass walks its parent chains over (Ancestors): the pass's
// own records, by key, and the records above them that its read leaves out (LineFacts.Parents),
// which is how a walk crosses an ancestor already done rather than stopping at it. It is one
// statement of the one map, because the two walks of a pass must answer alike about one chain —
// the line's hold under an accepted ancestor (Waiting) and the children-first cycle
// (ChildWaits) — and because a reader of either must not mistake it for the pass's own records,
// which say what runs and what each issue ranks against: a done ancestor says neither.
func chainOf(byKey, above map[string]Issue) map[string]Issue {
	chain := make(map[string]Issue, len(byKey)+len(above))
	maps.Copy(chain, byKey)
	maps.Copy(chain, above)
	return chain
}

// AcceptedAncestor is the nearest issue above issue (Ancestors) that the daemon has already
// accepted (Accepted, which counts an ancestor held from one of those phases), and "" when no
// ancestor has been.
//
// A parent already done does not end the walk: the spec's grandchild under a done parent is held
// by whichever issue above that parent is still merging, which is what the walk goes on to find.
// A done ancestor holds nothing itself — its merge has happened, so there is no order left to
// keep. The hold and the acceptance rules answer alike, which is what keeps them from
// deadlocking: OpenBeneath leaves out the issues this hold has stopped, so an issue the line
// holds is an issue the ancestor's own close does not wait for, and the expiry of the linger of
// the tree that held it hands it over as a root of its own, as it hands over every member that
// never started (workflow's unfinishedHandOvers).
func AcceptedAncestor(issue Issue, byKey map[string]Issue) string {
	for _, above := range Ancestors(issue, byKey) {
		if Accepted(above) {
			return above.Key
		}
	}
	return ""
}

// HeldUnderAccepted says whether the daemon is itself holding issue where it stands because an
// ancestor of it has already been accepted (AcceptedAncestor): it stands at phase.Admitted, so
// nothing of it has started, and the line will offer it no slot until that ancestor closes
// (Waiting). It is one rule with two readers that must agree exactly: OpenBeneath leaves such an
// issue out too, so the ancestor's own acceptance and close are not deadlocked by the issue its
// hold stopped. The hold never outlives the ancestor that caused it: once that ancestor closes,
// the line reads this rule again and finds no hold, so the issue starts in its running tree; and
// the expiry of a closed root's linger hands it to Legion as a root of its own without reading
// this rule (workflow's unfinishedHandOvers), since the expiry hands over every member that is in
// the workflow and never started, whatever held it.
func HeldUnderAccepted(issue Issue, byKey map[string]Issue) bool {
	return issue.Phase == phase.Admitted && AcceptedAncestor(issue, byKey) != ""
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
	// LineAnswered is a root that waited on a person and was answered. Its tree already runs; its
	// promotion resumes its architect where it stands (workflow's ResumeAwaitedRoot).
	LineAnswered
	// LineQueuedRoot is a root whose tree has not been opened: one that never ran, one a person
	// handed back to Legion after it finished, and one handed over from another tree. Its
	// promotion opens the tree lifecycle and hands the architect its tree.
	LineQueuedRoot
	// LineFirstPhase is an issue of a tree that already runs, standing at admitted with its
	// tree's design gate open, whose first phase can start: a released child, and the root itself
	// once its own work comes round. Its promotion starts that phase as the gate's opening used
	// to (workflow's gateOpened).
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
// issue is its tree's own root and in the starting part when it is a child — so the entry
// carries the part the line put it in (LineEntry.Part). The zero value is no part.
type LinePart int

const (
	// PartReturned is the first part: the issues the daemon has already started whose work has
	// come back to them, placed by the moment it did (LineEntry.Since).
	PartReturned LinePart = iota + 1
	// PartStarting is the second part: the issues whose first phase has never started, placed by
	// the Dispatch rank of their tree's root and then their own (LineEntry.RootRank, Issue.Rank).
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
	// the later approval that opened the tree's gate (LineAnswered), or the gate's opening
	// (LineFirstPhase of a tree's own root). It is the sort key itself rather than a second
	// reading of those records, and it is zero on an entry of PartStarting, whose work never
	// started.
	Since time.Time
	// RootRank is the Dispatch rank of the issue's tree root: with the issue's own rank it is
	// what the starting part is ordered by (RankLess over the root's record and then the
	// issue's), so a reader of one entry can say what placed it without the records the line
	// was built from. It is set on an entry of PartStarting alone; the returned part is ordered
	// by Since. The other two halves of that reading are read off the entry's own issue and are
	// no fields of this struct: the issue's rank is Issue.Rank and its tree root's key is
	// Issue.Tree, which is what the projection serves as `ownRank` and `rootKey`
	// (api.WaitingLineView) and what the sentence of a child's first phase names.
	RootRank string
	// Holds says the issue held a slot row when the line was built, which the grant reads
	// (admit's slotFor): such a candidate is promoted without taking a second row. Three kinds
	// of entry can hold one: an answered root (LineAnswered); a root that arrived from another
	// tree with the row it held there (LineQueuedRoot), in whatever phase it carries; and a
	// first phase at admitted (LineFirstPhase) of an issue the daemon has started — a tree's own
	// root whose gate has opened, or a child a person set back to todo. This is the one list of
	// them in the daemon's code: its other comments on a holder in the line point here, and the
	// line carries the fact rather than making each reader read the slots again.
	Holds bool
	// Waits is what the issue waits on, by issue key, for an issue the daemon started and
	// released for that wait (LineWaitingOnIssue, LineFacts.Waits), and empty for every other
	// entry: a candidate of another kind is judged against the mirror rows by the rule of its
	// kind (blockers.LineWaitsOn). The line carries the fact as Holds is carried, so that the
	// promotion that passes over a candidate and the state's own report of what Legion waits for
	// answer from one judgement.
	Waits []string
}

// LineFacts is everything the waiting line and the set of live trees are read from beyond the
// project's own issue records: the tree lifecycles, the roots' design gates, the records above
// the pass's own that its live read leaves out, and the worker starts the project already owes.
// They are gathered once per admission pass (ReadLineFacts) and handed to every judgement that
// pass makes, because each of them is read under the one fact lock (intake.ApplyFact) on every
// fact of every consumer: a read per tree, per gate or per slot would cost the daemon a round
// trip for each of them on each fact.
type LineFacts struct {
	// Lifecycles is the lifecycle of each tree root of the records, by root key. A tree with no
	// row has no entry, which reads as a tree nothing has opened.
	Lifecycles map[string]treelifecycle.Lifecycle
	// Gates is the design gate of each tree root of the records, by root key: one entry for
	// every root, holding the steps the deployment gates and whatever rows that root has
	// registered, so that a root with nothing registered reads as the gate it is (TreeGate).
	Gates map[string]TreeGate
	// Owed is the oldest pending start each issue owes its worker, by issue; an issue owing none
	// has no entry. OwedRows is every one of those rows, oldest first, which the repair of a row
	// that has outlived its fences walks (admit's finishFencedStarts).
	Owed     map[string]OwedStart
	OwedRows []OwedStart
	// Parents is the record of each issue on a parent chain above the pass's own records that
	// those records leave out — an ancestor already done — by key, so that the one chain walk
	// (Ancestors) crosses a done parent rather than stopping at it. A pass whose records hold
	// every parent they name reads nothing for it and has no entry here.
	Parents map[string]Issue
	// Held is the set of issues a Dispatch blocker holds where they stand, by key: the issues
	// whose first phase the line must not offer a slot to, however long they have waited. Only
	// the caller can fill it — the judgement is blockers.Holds over the project's mirror rows,
	// and internal/blockers reads this package, so this package may not read it back — and a
	// caller that fills it with nothing says that nothing is held (blockers.HeldIssues).
	Held map[string]struct{}
	// ChildWaits is the children-first judgement of this pass, by parent key: the issues whose
	// own first phase the line must not offer a slot to because Dispatch still shows an open
	// child under them. Only the caller can fill it — the judgement reads the Dispatch mirror's
	// parent column against the daemon's own records, which takes the transaction the line itself
	// is never given (ChildWaitsOf) — and a caller that fills it with nothing says that nothing
	// waits for its children. It is never stored: each admission pass computes it again, so a
	// mirror row that has gone stale costs one pass and no more.
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
}

// ChildWait is why one parent's own first phase waits: the open Dispatch children under it, in
// Dispatch key order. It is data alone — the rule that fills it is ChildWaits — so that the
// waiting line, the state's `blocked` and the architect's notice all read one judgement.
type ChildWait struct {
	Parent string
	Open   []OpenChild
}

// OpenChild is one Dispatch child that holds its parent's first phase: its key, the Dispatch
// status it holds it with, and, when a Dispatch blocked_by link of the child names an issue above
// it on its own parent chain, that ancestor. Such a cycle is named and nothing more: breaking the
// link is a person's decision.
type OpenChild struct {
	Key    string
	Status string
	Cycle  string
}

// treeRoots is the key of every tree root among issues, which is what a read of the trees of a
// whole pass is keyed by: their lifecycles and their design gates alike.
func treeRoots(issues []Issue) []string {
	roots := make([]string, 0, len(issues))
	for _, issue := range issues {
		if claim.IsTreeRoot(issue.Key, issue.Tree) {
			roots = append(roots, issue.Key)
		}
	}
	return roots
}

// ReadLifecycles is the lifecycle of each tree root among issues, by root key, in one read
// whatever the number of trees: ReadLineFacts's first read, and the whole of what a judgement
// that asks only whether a tree runs needs (workflow.Stranded). project is the Dispatch project
// key; the lifecycles are keyed by the claim project token the tree_lifecycles table uses
// (claim.ProjectToken), which is derived here so no caller has to remember the difference.
func ReadLifecycles(ctx context.Context, tx pgx.Tx, store Store, project string, issues []Issue) (map[string]treelifecycle.Lifecycle, error) {
	token, err := claim.ProjectToken(project)
	if err != nil {
		return nil, err
	}
	return store.TreeLifecycles(ctx, tx, token, treeRoots(issues))
}

// ReadParents is the record of each issue on a parent chain above issues that issues itself
// leaves out, by key (LineFacts.Parents): the pass's live read holds no issue already done, and
// a done parent in the middle of a chain would otherwise hide every issue above it from the one
// chain walk (Ancestors). It is one read whatever the depth of the chains — the store walks them
// in the database (Store.AncestorsOf) — and no read at all for the ordinary pass, whose records
// hold every parent they name.
func ReadParents(ctx context.Context, tx pgx.Tx, store Store, project string, issues []Issue) (map[string]Issue, error) {
	held := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		held[issue.Key] = struct{}{}
	}
	var missing []string
	for _, issue := range issues {
		if issue.Parent == nil {
			continue
		}
		if _, inHand := held[*issue.Parent]; !inHand && !slices.Contains(missing, *issue.Parent) {
			missing = append(missing, *issue.Parent)
		}
	}
	if len(missing) == 0 {
		return map[string]Issue{}, nil
	}
	rows, err := store.AncestorsOf(ctx, tx, project, missing)
	if err != nil {
		return nil, err
	}
	parents := make(map[string]Issue, len(rows))
	for _, row := range rows {
		parents[row.Key] = row
	}
	return parents, nil
}

// ReadTreeGates is the design gate of each tree root among issues, by root key, in one read
// whatever the number of trees: one entry per root, so that a root with nothing registered is
// the gate it is rather than a missing key every reader would have to answer for itself. steps
// are the design-gate steps the deployment gates (config.Gates.Steps), the empty list gating
// none, and every entry carries them, because the gate is the list and not a row.
func ReadTreeGates(ctx context.Context, tx pgx.Tx, store Store, issues []Issue, steps []DesignStep) (map[string]TreeGate, error) {
	roots := treeRoots(issues)
	rows, err := store.Gates(ctx, tx, roots)
	if err != nil {
		return nil, err
	}
	gates := make(map[string]TreeGate, len(roots))
	for _, root := range roots {
		gates[root] = TreeGate{Steps: steps, Rows: rows[root]}
	}
	return gates, nil
}

// ReadLineFacts reads the facts of one admission pass for issues: one tree-lifecycle read
// (ReadLifecycles), one gate read (ReadTreeGates), one read of the records above the pass's own
// (ReadParents) and one read of the starts the project owes, whatever the number of trees,
// gates and issues involved. project is the Dispatch project key and steps the design-gate
// steps the deployment gates.
func ReadLineFacts(ctx context.Context, tx pgx.Tx, store Store, project string, issues []Issue, steps []DesignStep) (LineFacts, error) {
	lifecycles, err := ReadLifecycles(ctx, tx, store, project, issues)
	if err != nil {
		return LineFacts{}, err
	}
	gates, err := ReadTreeGates(ctx, tx, store, issues, steps)
	if err != nil {
		return LineFacts{}, err
	}
	parents, err := ReadParents(ctx, tx, store, project, issues)
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
	return LineFacts{Lifecycles: lifecycles, Gates: gates, Parents: parents, Owed: owed, OwedRows: rows}, nil
}

// GateOpen is whether a person's approval stands on the gate's current document version: the one
// statement of it, which classify.DesignGateOpen and Store.PutGate's own write both call.
// internal/classify reads this package, so the rule lives here, where every reader can reach it.
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

// Open is whether the gate lets the tree's work run: a person's approval stands on the current
// document version of every required step (GateOpen). A required step with no row holds the
// work, as an unregistered gate always has, so a root with no row at all is held too. It is the
// one rule for the question, which classify.DesignGatesOpen reads it through.
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
// its required steps was approved (DesignGate.ApprovedMark), zero for a gate that is not open.
// It is the waiting line's ordering key for a root whose own first phase can start, merged on
// the same time axis as an owed start row's creation and an answered root's answer. A step whose
// approval carries no moment — one a registration read from Dispatch rather than from an event —
// gives the zero time, which puts that root at the head of its part when it is the only step:
// the line knows its work came back and not when, and the alternative, dropping it to the back
// by rank, loses what it does know.
func (g TreeGate) OpenedAt() time.Time {
	if !g.Open() {
		return time.Time{}
	}
	var at time.Time
	for _, step := range RequiredSteps(g.Steps) {
		at = later(at, g.Rows[step].ApprovedMark.At)
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
// works (KeepsSlot), so this answers a narrower question than the cap asks. gate is the design
// gate of the issue's tree root, the zero value for a root with no step of it registered.
//
// Three readers are left. Two ask about a worker rather than about the cap: CanWaitOnPerson,
// which lets a root hand work to a person only while its architect has work to hand over; and
// admit's resumeSlottedWorkers, which starts the phase worker of a slot holder that has lost its
// task and must pass over a holder whose phase has no worker at all. The third,
// blockers.waitsOf, asks it of a started issue at phase.Admitted to decide a slot release: a
// tree's root there whose design gate has never opened is its architect at work, so it waits on
// nothing and keeps its row.
//
// Planning, implementing, testing, reviewing and retro each have a phase worker with work.
// Merging has one too, unless a closed design gate is holding the READY its merger already built
// (ReadyPending), which is a wait on a person rather than work. Admitted is work only for
// a root whose design gate has never opened at every step the deployment gates, where its
// architect is writing the design; after that opening the root's own turns start no phase
// worker, and its children do the work. Every other phase is a wait: a child at admitted,
// accepting, awaiting_merge, held, awaiting_person, done.
func AtWork(issue Issue, gate TreeGate) bool {
	switch issue.Phase {
	case phase.Planning, phase.Implementing, phase.Testing, phase.Reviewing, phase.Retro:
		return true
	case phase.Merging:
		return issue.ReadyPending == nil
	case phase.Admitted:
		return claim.IsTreeRoot(issue.Key, issue.Tree) && !gate.EverOpened()
	default:
		return false
	}
}

// KeepsSlot is the phase half of the admission rule: an issue holds one of the cap's slots from
// the moment the daemon starts it until its phase reaches done or awaiting_merge, or Dispatch
// takes it out of the workflow (InWorkflow). Every phase between keeps the row — admitted,
// the worker phases, merging, accepting, held and awaiting_person — so an issue waiting for a
// person or for its review holds its place against the cap and wins none back from the line
// when the wait ends. A wait on another issue is the one wait that is not read here: an issue
// the daemon has started that waits on one gives its row back and stands in the line for one
// again, by the rule beside this (blockers.waitsOf, admit's releaseSlotsForWaits). Neither reads
// this one: they reach only the rows it keeps, because releaseSlots runs before them in admit's
// promoteHolds, so they apply to the phases it keeps a row for without restating them.
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
	return InWorkflow(issue) && issue.Phase != phase.AwaitingMerge
}

// NeedsSlot says whether issue must hold one of the cap's slots now: its tree is running — live,
// with a root that does not linger after its close — and the issue keeps a slot where it stands
// (KeepsSlot). An issue holds its admission slot from the first moment the daemon starts it
// until it is done, leaves the workflow or waits only for its merge, so the cap counts the
// issues the daemon has started and has still to finish, rather than the ones a worker of them
// is running this second. Nothing of a tree that has left the workflow works until its
// re-admission runs it again, which is why the tree's own terms are here.
//
// The Dispatch status KeepsSlot reads is asked because a person's park, close or icebox takes
// the issue out of the workflow before anything moves its phase: the workflow's own leave
// records the linger in the same fact, but a pass that reads the record between the two would
// otherwise count a parked root's slot against the cap for good.
//
// It is the one statement of the release, and the two readers that must never disagree both
// call it: admission releases the slot of every issue it answers false for (admit's
// releaseSlots), and the waiting line offers a returning issue a slot only where it answers
// true (Waiting). root is the record of the issue's tree root; live is whether the tree is
// live (LiveRoots). The design gate is read by no part of this: a gate open or closed changes
// neither whether the tree runs nor whether the issue's phase keeps a row (KeepsSlot). The slot
// is accounting only: taking one away suspends no claim,
// moves no phase and writes no Dispatch status.
func NeedsSlot(issue, root Issue, live bool) bool {
	return live && !root.Lingers() && KeepsSlot(issue)
}

// CanWaitOnPerson says whether root may enter awaiting_person now: it is a root of a tree in the
// workflow, standing at admitted and not lingering after its close, a worker of it has work
// there (AtWork), and the daemon has started it, which is what the slot row it holds says
// (slotted). The row no longer says a worker of the issue has work — an issue keeps it while it
// waits as well (KeepsSlot) — it says this root is one of the issues the daemon has started and
// not yet finished with. A root with no row was never started and has no work of its own to
// hand to anybody.
//
// At admitted, AtWork is true for a tree root whose design gate has never opened — the architect
// writing the spec, which holds the whole of the tree's work — so the gate is read here too: a
// root whose gate has opened has handed the work to its children, and its own first phase waits
// in the line rather than on a person.
//
// It is the one statement of the rule, and the two readers that must never disagree both call
// it: the pass that starts a wait (workflow's decideAwait, rule 0) and the boot's catch-up of
// the roots that pass can move (the daemon's catchesUp). A root this answers false for starts no
// wait on a person, so the boot need not read its Dispatch window either. gate is the root's own
// design gate, the zero value for a root with no step of it registered.
//
// It is not NeedsSlot read backwards: NeedsSlot says what the cap must hold a row for, of every
// issue, from the record alone, while this says what one root may hand to a person, and asks the
// row itself. The wait it starts keeps that row, since an issue in awaiting_person is an issue
// the daemon has started (KeepsSlot).
func CanWaitOnPerson(root Issue, gate TreeGate, slotted bool) bool {
	return slotted && root.Phase == phase.Admitted && !root.Lingers() && InWorkflow(root) && AtWork(root, gate)
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
// them one. The cap counts the issues the daemon has started rather than the trees, so a child
// waits in this line as a root does, and the line has two parts.
//
// First, by when their work came back, oldest first: the issues the daemon has already started
// whose work has come back to them. Four kinds of record say that work came back, and each
// gives the moment it did:
//
//   - an issue that owes its worker a start and waits on nothing takes that row's own creation
//     time and id (OwedStart), which is durable — the transition wrote the row in its own
//     transaction — so a restart finds the line exactly as it was, and a second transition
//     before the slot replaces that row's payload without moving it (Store.ReplaceOwedStart);
//   - a root a person answered while it waited in awaiting_person takes its Awaiting.AnsweredAt;
//   - a tree's own root standing at admitted under an open gate, whose first phase can start,
//     takes the moment that gate opened (gateReturn). It holds its row throughout — an issue
//     the daemon has started keeps one (KeepsSlot) — so its first phase is work coming back
//     rather than work that never started, and its promotion takes no further slot.
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
// Then, by the Dispatch rank of their tree's root (RankLess) and, within one root's rank, by
// their own: the issues whose first phase can start and that hold no slot row. These are the
// roots whose tree has not been opened — one that never ran, a re-admitted one and a handed-over
// one — and the released children of a tree that already runs, standing at admitted under an
// open design gate. Each takes its tree root's place in the board's order, so a started tree's
// children go before the roots ranked below their root, and a root ranked above a tree is not
// kept out by a tree that keeps filing children.
//
// An unanswered awaiting root waits on the person rather than a slot, whatever Dispatch status it
// currently shows, so it is not in the line until it is Answered. A root without the label waits
// for nothing. A root that never ran, whose recorded parent is a live tree's member, is adopted
// into that tree before admission reads the line (admit's adoptQueuedRoots), so it is not read as
// waiting there. A root under a parent unrecorded or recorded as a root, whose Dispatch chain
// (Ancestry) holds any live root but its own key, is held under an unreleased child of that tree
// (the ancestry hold) and waits for nothing: it is that tree's unreleased work, which its
// architect's release, or its parent's, brings into the tree, exactly as release_children's own
// walk of the chain reaches it. A recorded root on the chain that is not live is passed over. A
// tree is live here as everywhere (LiveRoots), so the tree of a root held under an unreleased
// child, or of a root waiting on a person, is live too; once no live root is left on its chain
// (each lingers or leaves the workflow, or a person moves an ancestor out from under it), the
// root waits as its own again. A root that ran, an answered one among them, records no chain
// (workflow's Ancestry), so the hold never takes an answered root out of the line. The other
// hold, on Dispatch blocked-by links, takes an issue out of here only where it holds it where it
// stands: an issue whose first phase would start waits at admitted until the last issue it names
// is done (LineFacts.Held, blockers.Holds), so the line never offers it a slot. A root's links
// are read at its promotion instead, which passes it over and keeps its place
// (blockers.WaitsOn); either way the state names it under admission.blocked.
//
// A root is in the line as a root that never ran (LineQueuedRoot) only while its own record says
// the generation it carries has not run — it stands at admitted with its design gate never
// opened, or it arrived from another tree carrying the phase it ran there (ran). So a person's
// todo on a running root puts it back in no queue: the root whose gate has opened waits here for
// its own first phase, and the root in accepting, in awaiting_merge or at work waits for
// nothing. The slot does not say it either, for the reason the queued-root loop states at the
// skip it guards below.
//
// An issue holding a slot row is in the line where its promotion needs more than that row, the
// three kinds LineEntry.Holds names, and each is promoted without taking a second slot: the
// entry says it holds one, and admit's slotFor reads that. A row does take an owed start out of
// the line, because the row is what releases that start (SuperviseRequest.NeedsSlot)
// and an issue holding one waits for nothing here, and it takes out a queued root the daemon
// granted that row in this tree, whose architect is writing the spec and whose second
// promotion would reopen an open tree. An issue whose tree is not live, or whose root lingers,
// is out of the line too: nothing of a tree that has left the workflow is started again until
// its re-admission opens it.
func Waiting(issues []Issue, slots []Slot, facts LineFacts) []LineEntry {
	slotted := make(map[string]struct{}, len(slots))
	for _, slot := range slots {
		slotted[slot.Issue] = struct{}{}
	}
	byKey := make(map[string]Issue, len(issues))
	for _, issue := range issues {
		byKey[issue.Key] = issue
	}
	// The parent chain is walked over the records above these as well (chainOf): a parent
	// already done is no part of the live read, and a walk that stopped at it would hide every
	// issue above it, so the line would offer a slot to a child the acceptance rules hold
	// (AcceptedAncestor).
	chain := chainOf(byKey, facts.Parents)
	queued := queuedRoots(issues, facts)
	live := liveRoots(issues, facts, queued)
	queuedKeys := make(map[string]struct{}, len(queued))
	for _, issue := range queued {
		queuedKeys[issue.Key] = struct{}{}
	}

	// The tree of a candidate must be live and must not linger, and the root must be recorded:
	// the whole line is work the daemon is about to start, and a tree that has left runs nothing.
	runs := func(issue Issue) bool {
		root, recorded := byKey[issue.Tree]
		if !recorded || root.Lingers() {
			return false
		}
		_, running := live[issue.Tree]
		return running
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
			// An answered root belongs to this part whether or not its tree lifecycle still
			// stands open, and whether or not it holds a slot row: its wait kept that row
			// (KeepsSlot), and its place is the moment its work came back, which is its answer
			// or, when a person's approval opened its gate later than that, the approval.
			_, holds := slotted[issue.Key]
			returned = append(returned, timed{entry: LineEntry{Issue: issue, Kind: LineAnswered, Holds: holds},
				at: later(*issue.Awaiting.AnsweredAt, facts.Gates[issue.Tree].OpenedAt())})
			continue
		}
		if _, waitingAsRoot := queuedKeys[issue.Key]; waitingAsRoot {
			// A root whose tree is not open yet is in the second part whatever rows it owes: its
			// promotion opens the tree, and the starts of a run it carries are queued then.
			continue
		}
		// An issue that already holds a row waits for nothing here: the row is what releases
		// the start it owes (SuperviseRequest.NeedsSlot), so its worker is given that task
		// without the line, and a place in the line would only take a slot from an issue
		// holding none.
		if _, held := slotted[issue.Key]; held {
			continue
		}
		// The issue must need a slot where it stands (NeedsSlot), the same question the release
		// of one asks, so that no issue the release would free is offered one here: an issue
		// that owes a start its phase has moved past — a run that ended before its slot came,
		// whose row the repair pass finishes (admit's finishFencedStarts) — waits for nothing
		// and must not take a slot from an issue that does.
		root, recorded := byKey[issue.Tree]
		_, running := live[issue.Tree]
		if !recorded || !NeedsSlot(issue, root, running) {
			continue
		}
		// An issue the daemon has started that waits on another issue stands here as the waiting
		// kind, whatever else it owes: its entry carries what it waits on, the promotion passes
		// over it until that wait ends (LineEntry.Waits), and nothing of its work moves
		// meanwhile. Two records say such an issue has started while it holds no row, and each
		// gives the moment its entry stands at: the mark of the row a pass took away for the
		// wait (Issue.ReleasedForWaitAt), and, where no mark stands, the start it owes its next
		// worker, which nothing but a transition of a run writes and whose row carries its own
		// creation time and id, as a return's entry does. An issue that never started has
		// neither — a child at admitted waiting for its first phase had no row to give back and
		// owes nothing — so none is a candidate here.
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
			continue
		}
		// This issue holds no row and waits on nothing: the skip above took every holder out of
		// the line, so the entry says so and its promotion takes one for it.
		returned = append(returned, timed{entry: LineEntry{Issue: issue, Kind: LineReturning}, at: owed.CreatedAt, id: owed.ID})
	}

	var starting []LineEntry
	for _, issue := range queued {
		if issue.Answered() {
			continue
		}
		// A queued root holding a row the daemon granted it in this tree is one it has already
		// started — a root whose architect is writing the spec, which a person set back to todo
		// — and a second promotion would reopen an open tree and start a second architect. A
		// root that arrived from another tree is the other half of that branch and stays: an
		// issue holds its row from the first moment the daemon starts it until it is done, leaves
		// the workflow or waits only for its merge (KeepsSlot), so such a root carries the row of
		// the tree it left (HandedOverFrom, the one mark of that arrival, which ran is written
		// over and the promotion that opens the root's own tree clears) while the tree of its
		// own has still to be opened. Dropped from the line, nothing would promote it, the
		// release would keep its row (NeedsSlot) and no later pass would find it again. The
		// tree it left usually takes that row back first — its root lingers from its close, and
		// the release reads the linger in the transaction that records the close, before the
		// hand-over writes the root of its own — so the carried row is the case this is written
		// against rather than the usual one. It is the one statement of that reasoning: the
		// doc above, QueuedRoot and WaitsForSlot each point here rather than repeating it.
		_, holds := slotted[issue.Key]
		if holds && issue.HandedOverFrom == "" {
			continue
		}
		if slices.ContainsFunc(issue.Ancestry, func(key string) bool {
			_, under := live[key]
			return under && key != issue.Key
		}) {
			continue
		}
		starting = append(starting, LineEntry{Issue: issue, Kind: LineQueuedRoot, Holds: holds})
	}
	for _, issue := range issues {
		if issue.Phase != phase.Admitted || issue.AwaitingPerson() || OutOfWorkflow(issue.Status) {
			continue
		}
		// An issue the first loop has already put in the line, waiting on another issue, stands
		// there once: its promotion grants it the row back and nothing else, and the pass that
		// reads the line after it offers the first phase this branch would. A queued root never
		// reaches that branch (the first loop's own skip above), so this is the one place it can
		// double.
		if _, waits := releasedForWait[issue.Key]; waits {
			continue
		}
		if _, waitingAsRoot := queuedKeys[issue.Key]; waitingAsRoot || !runs(issue) {
			continue
		}
		// A root that handed work to a person starts nothing of its tree while it waits: its
		// members start with its resume (admit's promotion of the answered root).
		if root := byKey[issue.Tree]; root.AwaitingPerson() {
			continue
		}
		gate := facts.Gates[issue.Tree]
		if !gate.Open() {
			continue
		}
		// A child an unfinished Dispatch issue holds where it stands waits for that issue, not
		// for a slot: offered one here it would take it from an issue that can work, and its own
		// first phase would start on work its blocker has still to land. Its tree's architect is
		// told when it starts, and the state names it under admission.blocked meanwhile.
		if _, blocked := facts.Held[issue.Key]; blocked {
			continue
		}
		// A parent whose own Dispatch children are still open does its work after theirs
		// (LineFacts.ChildWaits): offered a slot here it would build beside the children whose
		// merges its own head is meant to carry. Its tree's architect is told once per issue and
		// run of its tree when the wait begins (admit's tellChildrenFirst), and the state names
		// it under admission.blocked meanwhile.
		if _, waits := facts.ChildWaits[issue.Key]; waits {
			continue
		}
		// An issue beneath an ancestor the daemon has already accepted waits for that ancestor
		// to close (HeldUnderAccepted): starting it would let the ancestor merge work its own
		// head does not carry. A person's move of such an issue to todo records it here at
		// admitted and leaves it here; the architect is told why when it enters. The chain is
		// walked over the records above this pass's own as well, so that a parent already done
		// does not hide the merging issue above it.
		if HeldUnderAccepted(issue, chain) {
			continue
		}
		_, holds := slotted[issue.Key]
		entry := LineEntry{Issue: issue, Kind: LineFirstPhase, Holds: holds}
		// A tree's own root has held a slot since its architect began the design and keeps it
		// while its children do the work (KeepsSlot): its first phase is work coming back, so
		// it stands in the first part at the moment the gate opened (TreeGate.OpenedAt), and
		// its promotion takes no second slot. A child of the tree holds none until the daemon
		// starts it, so its first phase is new work and waits in the second part by its tree
		// root's rank; a child the daemon started and a person set back to todo stands here
		// holding the row of that run, and its promotion takes no second one either.
		if claim.IsTreeRoot(issue.Key, issue.Tree) {
			returned = append(returned, timed{entry: entry, at: gate.OpenedAt()})
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
	sort.Slice(starting, func(i, j int) bool {
		left, right := starting[i].Issue, starting[j].Issue
		// A root of its own tree compares by its own rank, which is its tree root's; a child
		// takes its tree root's place in the board's order and then its own.
		if left.Tree != right.Tree {
			return RankLess(byKey[left.Tree], byKey[right.Tree])
		}
		return RankLess(left, right)
	})

	// Each entry's part is the slice it ended in, set here rather than where the entry was
	// built, so that a loop added above cannot leave an entry with no part at all: an entry at
	// LinePart(0) would be served no sentence and no `why` and nothing would say so
	// (projection's waitingLineReason). Its place within that part is the order the part was
	// just sorted into and not the order the loops above found it; the moment that placed a
	// returned entry is the sort key itself, taken from the timed value rather than read a
	// second time from the records (LineEntry.Since); and the rank that placed a starting entry
	// is its tree root's, which the sort above read the same record for.
	line := make([]LineEntry, 0, len(returned)+len(starting))
	for i, waiting := range returned {
		entry := waiting.entry
		entry.Part, entry.PartPlace, entry.Since = PartReturned, i+1, waiting.at
		line = append(line, entry)
	}
	for i := range starting {
		starting[i].Part, starting[i].PartPlace, starting[i].RootRank =
			PartStarting, i+1, byKey[starting[i].Issue.Tree].Rank
	}
	return append(line, starting...)
}

// WaitingIssues is the issues of a waiting line, in its order, for a reader that wants the
// issues rather than the entries. One reader is left: blockers.judgeAgain, which hands them to
// Judged to learn which Dispatch mirror rows the rebuilt line needs read. The state's
// `admission.waiting` was the other until the projection began reading each entry's own fields
// (projection's Project), and it now walks the entries and takes the key off each.
func WaitingIssues(line []LineEntry) []Issue {
	issues := make([]Issue, len(line))
	for i, entry := range line {
		issues[i] = entry.Issue
	}
	return issues
}

// queuedRoots is every root handed to Legion whose tree has not been opened and which may take a
// slot: one that never ran, a re-admitted one and a handed-over one (QueuedRoot). It is read
// before the ancestry hold, because the hold is read against the live trees these same roots
// make (liveRoots).
func queuedRoots(issues []Issue, facts LineFacts) []Issue {
	queued := make([]Issue, 0, len(issues))
	for _, issue := range issues {
		if QueuedRoot(issue, facts.Gates[issue.Key]) {
			queued = append(queued, issue)
		}
	}
	return queued
}

// QueuedRoot says whether issue is a root handed to Legion that waits in the line for its tree
// to be opened: a root that never ran, a re-admitted one and a handed-over one, each standing in
// todo, not waiting on a person (whose answer resumes it where it stands) and with a record that
// does not already say this generation runs (ran). It is the one statement of the rule, read by
// the line itself, by the set of live trees (liveRoots), by the set of running ones (TreeRuns)
// and by the relay of a person's decision (WaitsForSlot), so that no two of them disagree about
// a root that has still to start.
//
// The tree's lifecycle does not answer this, though it is what says the tree is live
// (liveRoots): a lifecycle that stands open says the tree once opened, not that this generation
// of the root is running, and a root whose tree ran and ended queues again with that row still
// standing until its cleanup confirms. The slot does not answer it either: a root arriving from
// another tree in accepting may carry the row it held there while the tree of its own has still
// to be opened, which Waiting's queued-root loop states at the skip it guards.
func QueuedRoot(issue Issue, gate TreeGate) bool {
	if !issue.HandedOver || !claim.IsTreeRoot(issue.Key, issue.Tree) || ran(issue, gate) {
		return false
	}
	return !issue.AwaitingPerson() && issue.Status == "todo"
}

// ran says whether this root's own record shows the generation it carries already running under
// Legion. Two things say so, and the slot says neither any more. The record stands in a phase of
// its own (Issue.CarriesRun — neither admitted nor done): it is at work, waiting for its merge,
// in accepting, held or waiting on a person. Or its design gate has opened at every step the
// deployment gates at some point (TreeGate.EverOpened), after which the root stands at admitted
// while its architect revises the design and its own first phase waits in the line as a member
// of its tree (LineFirstPhase).
// A re-admission clears the gate with the rest of the generation, so a tree Legion takes back
// queues again as a root that never ran, which is what it is.
//
// The one record that carries a run and has still to run here is a root that has just arrived
// from another tree with the phase it ran there (Issue.HandedOverFrom): that run belonged to the
// tree it left, and the tree of its own it becomes has yet to be opened, so its promotion opens
// it and starts the role of whatever phase it carries. The mark is the arrival and not the
// root's whole life: the promotion that opens its tree clears it (admit's promoteHolds), because
// from then on the run the record carries is this tree's own. Without that a person's todo on
// such a root, running and holding no row of its own tree in accepting or awaiting_merge, would
// queue it here again and promote it a second time, moving its awaiting_merge to merging where
// its merge closes nothing.
func ran(issue Issue, gate TreeGate) bool {
	return issue.HandedOverFrom == "" && (issue.CarriesRun() || gate.EverOpened())
}

// treeLive is TreeLive over the facts of one root already in hand: its tree's lifecycle and its
// own design gate. It is the one statement of the rule; the pass that judges many trees reads it
// through liveRoots and the one that judges a single root through TreeLive, so no reader of
// "this tree is live" can answer differently from another.
func treeLive(root Issue, lifecycle treelifecycle.Lifecycle, gate TreeGate) bool {
	switch {
	case root.Lingers():
		return false
	case root.AwaitingPerson():
		return true
	}
	return lifecycle.OpenUnderWorkflow() || QueuedRoot(root, gate)
}

// TreeLive says whether root's tree is live: its lifecycle stands open under the workflow's own
// authority and the root does not linger after its close, or the root waits in the line for its
// tree to be opened (held under an unreleased child or not), or it waits on a person with its
// architect live. A todo child under a live tree runs in it, or waits with its root held under an
// unreleased child and moves with it into the tree that adopts it; under any other it is an
// orphan.
//
// The lifecycle is what says a tree runs, not the slot rows: a slot now counts one issue the
// daemon has started, so a tree runs with every one of its issues waiting for a slot, and a
// slotted child is no tree of its own. A pass that judges many trees reads them all at once
// instead (LiveRoots over ReadLineFacts).
//
// Live is wider than running (TreeRuns): a root still waiting in the line for its tree to be
// opened is live, so that a todo child recorded under it joins the tree its promotion opens,
// and runs nothing yet. steps are the design-gate steps the deployment gates.
func TreeLive(ctx context.Context, store Store, tx pgx.Tx, root Issue, steps []DesignStep) (bool, error) {
	if root.Lingers() {
		return false, nil
	}
	if root.AwaitingPerson() {
		return true, nil
	}
	lifecycle, gate, err := rootFacts(ctx, store, tx, root, steps)
	if err != nil {
		return false, err
	}
	return treeLive(root, lifecycle, gate), nil
}

// TreeRuns says whether this generation of root's tree is running under the workflow: its
// lifecycle stands open under the workflow's own authority, the root does not linger after its
// close, and the root is not itself waiting in the line for that tree to be opened (QueuedRoot).
//
// It is the one statement of "the tree runs", and the three readers that must agree all call it:
// the claims nothing will wake (workflow.Stranded), the boot's resume of a suspended phase
// worker (workflow's booted) and the question whether a session's status write lands on a
// running tree (workflow's sessionStatusWrite). Each of them acts on the tree as it stands
// now, and a root waiting in the line runs nothing yet: its architect's first task comes with
// its promotion, so a claim of it is not stranded, a suspended worker of it is not resumed, and
// a status write on it is a person's move. The last clause is what a re-admission inside a
// linger needs: readmit clears the linger while the lifecycle of the run that ended still
// stands open, so the lifecycle alone would call such a root running.
func TreeRuns(root Issue, lifecycle treelifecycle.Lifecycle, gate TreeGate) bool {
	return lifecycle.OpenUnderWorkflow() && !root.Lingers() && !QueuedRoot(root, gate)
}

// TreeRunning is TreeRuns for a caller holding one root and no facts of a pass: it reads that
// root's tree lifecycle and its design gate, and asks the one rule. steps are the design-gate
// steps the deployment gates.
func TreeRunning(ctx context.Context, store Store, tx pgx.Tx, root Issue, steps []DesignStep) (bool, error) {
	if !claim.IsTreeRoot(root.Key, root.Tree) || root.Lingers() {
		return false, nil
	}
	lifecycle, gate, err := rootFacts(ctx, store, tx, root, steps)
	if err != nil {
		return false, err
	}
	return TreeRuns(root, lifecycle, gate), nil
}

// OperatorHoldsTree says whether the operator opened the lifetime of the issue's tree (an
// operator root spawn under the tree root's key) and its cleanup has not confirmed. Only a root
// waiting for its slot can meet one: promotion waits on the operator's lifetime
// (treelifecycle.ErrAuthorityHeld) and never opens a workflow tree under it. A tree with no
// lifecycle row is no tree the operator holds, which is the read's own answer for it.
//
// It is the one statement of that rule, and the readers that must agree — the park of such a
// root (workflow.Engine.leave) and the claim release row every other park queues
// (outbox.releaseClaim) — both call it.
func OperatorHoldsTree(ctx context.Context, store Store, tx pgx.Tx, issue Issue) (bool, error) {
	project, err := claim.ProjectToken(issue.Project)
	if err != nil {
		return false, err
	}
	lifecycle, found, err := store.TreeLifecycle(ctx, tx, project, issue.Tree)
	if err != nil {
		return false, err
	}
	return found && lifecycle.HeldByOperator(), nil
}

// RunningRoots is the set of keys whose trees are running (TreeRuns), read from the records and
// the pass's own facts already in hand.
func RunningRoots(issues []Issue, facts LineFacts) map[string]struct{} {
	running := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		if claim.IsTreeRoot(issue.Key, issue.Tree) && TreeRuns(issue, facts.Lifecycles[issue.Key], facts.Gates[issue.Key]) {
			running[issue.Key] = struct{}{}
		}
	}
	return running
}

// ReadRunningRoots is RunningRoots for a caller that holds no facts of the pass: the lifecycles
// and the gates of the roots among issues, in two reads whatever the number of trees, and then
// the one rule. project is the Dispatch project key and steps the design-gate steps the
// deployment gates.
func ReadRunningRoots(ctx context.Context, tx pgx.Tx, store Store, project string, issues []Issue, steps []DesignStep) (map[string]struct{}, error) {
	lifecycles, err := ReadLifecycles(ctx, tx, store, project, issues)
	if err != nil {
		return nil, err
	}
	gates, err := ReadTreeGates(ctx, tx, store, issues, steps)
	if err != nil {
		return nil, err
	}
	return RunningRoots(issues, LineFacts{Lifecycles: lifecycles, Gates: gates}), nil
}

// rootFacts is one root's tree lifecycle and design gate, for the single-root readings of the two
// rules above — TreeLive, which the recording of a todo child under a live tree asks, and
// TreeRunning, which workflow's sessionStatusWrite asks of a session's status write: a pass that
// judges many trees reads them in bulk instead (ReadLineFacts). steps are the design-gate steps
// the deployment gates.
func rootFacts(ctx context.Context, store Store, tx pgx.Tx, root Issue, steps []DesignStep) (treelifecycle.Lifecycle, TreeGate, error) {
	project, err := claim.ProjectToken(root.Project)
	if err != nil {
		return treelifecycle.Lifecycle{}, TreeGate{}, err
	}
	lifecycle, _, err := store.TreeLifecycle(ctx, tx, project, root.Key)
	if err != nil {
		return treelifecycle.Lifecycle{}, TreeGate{}, err
	}
	rows, err := store.GatesOf(ctx, tx, root.Key)
	if err != nil {
		return lifecycle, TreeGate{}, err
	}
	return lifecycle, TreeGate{Steps: steps, Rows: rows}, nil
}

// LiveRoots is the set of keys whose trees are live (TreeLive), read from the records and the
// pass's own facts already in hand.
func LiveRoots(issues []Issue, facts LineFacts) map[string]struct{} {
	return liveRoots(issues, facts, queuedRoots(issues, facts))
}

// liveRoots is LiveRoots over queued, the roots waiting for their tree to be opened already read
// from the same records: a root among them is live by treeLive's own last clause, and reading
// them once keeps the ancestry hold and the line judged against the same set of trees, so a todo
// child of such a root records into its tree.
func liveRoots(issues []Issue, facts LineFacts, queued []Issue) map[string]struct{} {
	live := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		if claim.IsTreeRoot(issue.Key, issue.Tree) && treeLive(issue, facts.Lifecycles[issue.Key], facts.Gates[issue.Key]) {
			live[issue.Key] = struct{}{}
		}
	}
	for _, issue := range queued {
		live[issue.Key] = struct{}{}
	}
	// A root of a tree that lingers is live in no reading of it, whatever its own record says it
	// waits for: its re-admission is what opens the tree again, and it is that write, not this,
	// that ends the linger.
	for _, issue := range issues {
		if issue.Lingers() {
			delete(live, issue.Key)
		}
	}
	return live
}
