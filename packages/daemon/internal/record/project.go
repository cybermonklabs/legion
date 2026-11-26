package record

import (
	"context"
	"fmt"
	"maps"
	"reflect"
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

// EndChildRule is how an architect ends a child, in the short form that the daemon's refusals
// and notices carry (dispatch://INFRA-3178 R6). The daemon takes the owning architect's done,
// backlog, icebox or triage on a child as it takes a person's move (workflow's
// sessionStatusWrite). So the move is the architect's own status write in Dispatch, and no
// operation of the legion tool makes it. Each thing the rule says stops a loss
// that the move alone does not stop. A fold names its carrier, and the issues that wait on the
// folded child wait on the carrier first, because done starts them. A running child's worker
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
	"For a fold, name the carrier in the reason: the issue that carries the work. " +
	"Before the done of a fold, make each issue that is blocked_by the child wait on the carrier instead. " +
	"Done starts those issues, and the Referenced by list of dispatch read names each of them as blocked_by. " +
	"Before done on a running child, have its worker push the work that the carrier needs. " +
	"Done ends the worker's turn and the pod at once. " +
	"Unless the work of the child's open pull request should land, close that pull request with your own App (gh pr close). " +
	"When it is in the merge queue, first take it out (GraphQL dequeuePullRequest). " +
	"Do this before done, or at once after a done that you wrote first. " +
	"Write backlog or icebox only when a person asked for that move, or as the first step of a restart. " +
	"A restart is backlog, then rerun_child. " +
	"Say why in a dispatch comment. " +
	"Write no status on a child that is already done, because a move out of done ends an open production check. " +
	"After each move, read the child back (dispatch read --issue <KEY>). " +
	"When the daemon set the move back, write it again."

// StatusNotTakenYet is the sentence that the architect's refusals over children still in the
// workflow and the retro-held notice carry; workflow's endChildWords holds it with EndChildRule
// and names each text that carries them. The daemon reads the architect's status write on a child
// from that write's Dispatch event, and the event consumer can deliver the event after the
// architect calls again. Until then the record holds the child in the workflow and the refusal
// names it, so the architect waits for read_record to show its move (dispatch://INFRA-3178 R1).
//
// It lives here, beside EndChildRule, because the legion tool's description carries both texts
// (packages/pi-legion/src/tools.ts). The tests that hold EndChildRule to that description
// hold this sentence to it too: TestTheEndChildRuleGolden writes both to end-child-rule.json.
const StatusNotTakenYet = "A status that you wrote and that the daemon has not taken yet does not count until read_record shows it."

// InWorkflow says whether an issue is in the workflow: its Dispatch status is one the workflow
// runs in (OutOfWorkflow) and its phase is short of done. One still admitted counts, since it
// will run — a root reopened inside its linger (admit's readmit) stands in admitted with no
// role's phase until its task's first step starts (planning, in the default steps), and is
// running again from the reopen. The production check's "runs again" predicate, RunsAgain, is
// built on it.
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

// RunsAgain says whether an issue's work runs again: it is in the workflow (InWorkflow) and not
// waiting in verifying for the checks of its goal. It is the production check's "runs again": an
// issue in verifying has merged its work and no role works the wait, so its looks run there, and
// a look is deferred only while its issue's next change, or a rerun, holds the issue's claims and
// its pod. The production check's readers (workflow's check.go, the outbox's checkLookHolds) and
// a claim's failure (workflow's claimFailed) read it.
func RunsAgain(issue Issue) bool {
	return InWorkflow(issue) && issue.Phase != phase.Verifying
}

// Stopped is whether the issue's own record says its work has stopped: it is out of the
// workflow, or its tree lingers. It is the one statement of that rule, and the readers that
// must agree — the boot's claim release pass and the row it queues — both call it.
func Stopped(issue Issue) bool { return !InWorkflow(issue) || issue.Lingers() }

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
// mirror of Dispatch's issues or in its record of the ancestor; the record's own status is done
// (DoneStatus); or the daemon records it in its retro or its merge (Accepted). Where neither the
// mirror nor the record knows the ancestor, nothing the daemon holds says that it is accepted. A
// root a person moved to the backlog or to icebox stands at phase done while its tree lingers
// (workflow's beginLinger), and R1 counts backlog and icebox as not accepted, so the phase alone
// says nothing: a real close is the status Dispatch shows, or the record's own status, which the
// daemon's own close of a root (workflow's closeRoot) writes done in the transaction that queues
// the status write to Dispatch, so a reader inside a later transaction sees the close before
// Dispatch's event of it comes back to the mirror. The answer is a sentence's subject and verb,
// naming the ancestor and what holds it ("Dispatch shows LEGION-100 done").
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
// a child keeps a basis or a rest only on an ancestor it passes (admit's carriedGate).
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
	case recorded.Status == DoneStatus:
		return fmt.Sprintf("The daemon records %s %s", ancestor, recorded.Status), recorded, nil
	case Accepted(*recorded):
		return fmt.Sprintf("The daemon records %s in %s", ancestor, StandsAt(*recorded)), recorded, nil
	}
	return "", recorded, nil
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

// OwnsUnder says whether the architect of architectIssue, an issue of tree, owns target, an issue
// of the same tree, for a move of target into or out of the workflow. members is the tree's
// records (Store.TreeIssues). The tree root's architect owns every issue of its tree. A
// sub-architect owns only the issues under its own issue through their recorded parents
// (Ancestors), and not its own issue: an architect above it moves that issue, as one released it.
// A chain that leaves the tree, or loops back on itself, ends at the root, so only the root's
// architect owns an issue on such a chain.
//
// It is the one statement of that rule, and two readers ask it: the API's operations on a child
// (api's childOfTree) and the engine's exception for the owning architect's status on a
// child (workflow's sessionStatusWrite). Written twice, the API could refuse an architect a child
// whose status the engine takes from that architect, or the other way round.
func OwnsUnder(members []Issue, tree, architectIssue string, target Issue) bool {
	if claim.IsTreeRoot(architectIssue, tree) {
		return true
	}
	return target.Key != architectIssue && descends(target, architectIssue, byKey(members))
}

// chainOf is the records an admission pass walks its parent chains over (Ancestors): the pass's
// own records, by key, and the records above them that its read leaves out (LineFacts.Parents),
// which is how a walk crosses an ancestor already done rather than stopping at it. It is not the
// pass's own records, which say what runs and what each issue ranks against: a done ancestor in
// it says neither.
func chainOf(byKey, above map[string]Issue) map[string]Issue {
	chain := make(map[string]Issue, len(byKey)+len(above))
	maps.Copy(chain, byKey)
	maps.Copy(chain, above)
	return chain
}

// passChain is the one chain of an admission pass (ReadLineFacts, and the engine's judgement of
// one parent's children, OpenChildWaits): the pass's own records, issues, by key (own) and the
// records it walks its parent chains over (chain, chainOf with above, LineFacts.Parents). Each is
// built the first time a walk reads it and kept for every walk after. ReadLineFacts walks the
// chain for the cycle of each child (childWaits), for the child left out of the started parents'
// waits and of the retro-held parents' (withoutHeldUnderAccepted) and for the issues beneath each
// parent held before its retro (retroHoldsOf), and hands it on in its facts (LineFacts.pass), so
// the line (Waiting) and the held list's ladder (HeldList) judged from those facts read it too
// wherever they are handed the very records it was built over (passOver). One pass so builds it
// once, and builds it not at all before the line where it reads no Dispatch child and holds no
// parent before its retro. Every walk only reads the two maps, which every walk after the first
// shares, so none may write them. own and chain fill on first read, through any copy of the facts,
// so the facts of one pass are read on one goroutine.
type passChain struct {
	issues         []Issue
	above          map[string]Issue
	keyed, chained map[string]Issue
}

// own is the pass's own records, by key.
func (p *passChain) own() map[string]Issue {
	if p.keyed == nil {
		p.keyed = byKey(p.issues)
	}
	return p.keyed
}

// chain is the records the pass walks its parent chains over (chainOf).
func (p *passChain) chain() map[string]Issue {
	if p.chained == nil {
		p.chained = chainOf(p.own(), p.above)
	}
	return p.chained
}

// over says whether p was built over issues and above: the very slice of records it holds, at the
// same length, and the same map of the records above them, or no record above either. It compares
// identity and not content, which answers for the records only while no caller writes one of them
// in place between the build and the read, and none does: a pass that changes a record reads the
// records and their facts again together (admit's reread).
func (p *passChain) over(issues []Issue, above map[string]Issue) bool {
	switch {
	case p == nil || len(issues) != len(p.issues) || len(above) != len(p.above):
		return false
	case len(issues) > 0 && &issues[0] != &p.issues[0]:
		return false
	}
	return len(above) == 0 || reflect.ValueOf(above).UnsafePointer() == reflect.ValueOf(p.above).UnsafePointer()
}

// passOver is the chain a walk reads for issues under facts: the pass's one chain (passChain,
// LineFacts.pass) where it was built over these very records and the same records above them
// (passChain.over), and otherwise one of its own over issues and facts.Parents, built as the
// pass builds its own. A chain built over other records is never read.
func passOver(issues []Issue, facts LineFacts) *passChain {
	if facts.pass.over(issues, facts.Parents) {
		return facts.pass
	}
	return &passChain{issues: issues, above: facts.Parents}
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
// nothing of it has started, and the line will offer it no slot while that ancestor stands in
// retro or merging (Waiting). It is one rule with two readers that must agree exactly: OpenBeneath leaves such an
// issue out too, so the ancestor's own acceptance and close are not deadlocked by the issue its
// hold stopped. The hold never outlives the ancestor that caused it: once that ancestor closes,
// the line reads this rule again and finds no hold, so the issue starts in its running tree; and
// the expiry of a closed root's linger hands it to Legion as a root of its own without reading
// this rule (workflow's unfinishedHandOvers), since the expiry hands over every member that is in
// the workflow and never started, whatever held it.
func HeldUnderAccepted(issue Issue, byKey map[string]Issue) bool {
	return acceptedAncestorHolding(issue, byKey) != ""
}

// acceptedAncestorHolding is the ancestor whose acceptance holds issue where it stands, "" when
// none does: the rule HeldUnderAccepted answers yes or no with, written once so that the
// placement it decides and the word that reports it (HeldReasonOf's accepted_ancestor) name one
// ancestor and cannot drift apart. The held ladder takes the key from here rather than asking
// the rule and then walking the chain a second time for the ancestor's name. byKey is the
// records the chain is walked over (Ancestors).
func acceptedAncestorHolding(issue Issue, byKey map[string]Issue) string {
	if issue.Phase != phase.Admitted {
		return ""
	}
	return AcceptedAncestor(issue, byKey)
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
	// to (workflow's gateOpened), and starts a child's own architect with it
	// (workflow's StartFirstPhase).
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

// LinePart is which of the waiting line's two parts an entry stands in within the band of its
// priority (Waiting): the issues whose work came back, oldest first, and then the issues that have
// not started, by the board's rank. The kind does not give it — a LineFirstPhase entry falls in
// the returned part when the issue is its tree's own root and in the starting part when it is a
// child — so the entry carries the part the line put it in (LineEntry.Part). The zero value is no
// part.
type LinePart int

const (
	// PartReturned is the first part of each priority's band: the issues the daemon has already
	// started whose work has come back to them, placed by the moment it did (LineEntry.Since).
	PartReturned LinePart = iota + 1
	// PartStarting is the second part of each priority's band: the issues whose first phase has
	// never started, placed by the Dispatch rank of their tree's root and then their own
	// (LineEntry.RootRank, Issue.Rank).
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
	// an older Dispatch that resolves none, the issue's own, else its tree root's, else
	// NoPriority, whose band stands after P3. The issue's own priority is Issue.Priority, which
	// this does not change. A P0 that Dispatch passes down starts outside the cap
	// (Issue.StartsNow); only the tree root's priority, the fallback of an older Dispatch, orders
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
// pass (the pass rule on admit's Admission.Apply): a read per tree, per gate or per slot would
// cost the daemon a round trip for each of them on each pass.
type LineFacts struct {
	// Lifecycles is the lifecycle of each tree root of the records, by root key. A tree with no
	// row has no entry, which reads as a tree nothing has opened.
	Lifecycles map[string]treelifecycle.Lifecycle
	// Gates is the design gate of each tree root of the records, by root key: one entry for
	// every root, holding the steps the deployment gates and whatever rows that root has
	// registered, so that a root with nothing registered reads as the gate it is (TreeGate).
	// The roots are both sets the pass knows — those among its own issues and those its read
	// leaves out (Roots) — so that classification reads a gate for every tree a member of it
	// stands in, whichever of the two readers built the facts (ReadLineFacts, HeldReasonOf).
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
	//
	// Nothing may be added to it for classification (HeldReasonOf): Waiting walks it in
	// chainOf, and that chain decides HeldUnderAccepted, which places issues. A record added
	// here to name a hold better would move the line itself.
	Parents map[string]Issue
	// Roots is the record of each tree root the pass's own issues do not hold, by tree key
	// (ReadTreeRoots): the tick reads the project's live issues, which hold no root that is
	// done and no parked one, and without their records it could not tell a closed tree from a
	// parked one. Their gates are read beside them (ReadLineFacts), so the two words that speak
	// of a root read one record and one gate of it. Waiting's placement reads this map nowhere
	// — classification alone does (HeldFacts.treeRoot) — and the gates it reads are the gates
	// of roots among the pass's own issues, so the line cannot change for either read.
	Roots map[string]Issue
	// Now is the moment the pass reads the record, which classification alone reads: the
	// tree_closed word asks whether a root's linger is still running at it (Lingering,
	// HeldReasonOf), so that a parked root's members read tree_parked once its linger has run
	// out rather than tree_closed for ever. The one caller that fills it is the judgement every
	// reader of the line comes through (blockers.JudgeLine), which takes the clock of the pass
	// that called it. The zero moment reads every armed linger as still running, which is what a
	// caller that builds the line itself — a test, a reader with no clock — gets, and it leaves
	// each such member reading tree_closed. Waiting's placement reads it nowhere, so the line
	// cannot change for it.
	Now time.Time
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
	// pass is the pass's one chain (passChain), which ReadLineFacts built and its facts hand on
	// (passOver); nil in facts ReadLineFacts did not read. Its maps fill on first read, so the
	// facts of one pass are read on one goroutine.
	pass *passChain
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

// ReadTreeRoots is the record of each tree root the issues themselves leave out, by tree key
// (LineFacts.Roots): a pass whose read holds no root that is done and no parked one — the
// tick's live read — would otherwise have no record of the tree a member stands in, and could
// not say whether that tree is closed or parked (HeldReasonOf). It collects the trees no issue
// in hand roots and reads them in one query whatever their number, as ReadParents collects the
// parents its records leave out, and no read at all for a pass whose records hold every root
// they name.
//
// It is read for classification alone. The waiting line places no issue by it, so a root this
// read finds or misses moves nothing of the line.
func ReadTreeRoots(ctx context.Context, tx pgx.Tx, store Store, project string, issues []Issue) (map[string]Issue, error) {
	held := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		held[issue.Key] = struct{}{}
	}
	// A tree is marked as held the moment it is named, so a project of many closed trees costs
	// one map write for each rather than a scan of the list built so far, and `missing` keeps
	// the order the issues name their trees in.
	var missing []string
	for _, issue := range issues {
		if _, inHand := held[issue.Tree]; !inHand {
			held[issue.Tree] = struct{}{}
			missing = append(missing, issue.Tree)
		}
	}
	if len(missing) == 0 {
		return map[string]Issue{}, nil
	}
	rows, err := store.IssuesOf(ctx, tx, project, missing)
	if err != nil {
		return nil, err
	}
	roots := make(map[string]Issue, len(rows))
	for _, row := range rows {
		roots[row.Key] = row
	}
	return roots, nil
}

// ReadTreeGates is the design gate of each tree root among issues, by root key, in one read
// whatever the number of trees: one entry per root, so that a root with nothing registered is
// the gate it is rather than a missing key every reader would have to answer for itself. steps
// are the design-gate steps the deployment gates (config.Gates.Steps), the empty list gating
// none, and every entry carries them, because the gate is the list and not a row.
//
// It covers the roots among issues and no other: a pass that classifies the members of trees
// its own records leave out reads those roots' gates as well (ReadLineFacts), and a reader that
// asks about the roots in hand — the boot's catch-up of the trees a wait can move (the daemon's
// rootGates), the running roots (ReadRunningRoots) — asks about these.
func ReadTreeGates(ctx context.Context, tx pgx.Tx, store Store, issues []Issue, steps []DesignStep) (map[string]TreeGate, error) {
	return gatesOf(ctx, tx, store, treeRoots(issues), steps)
}

// gatesOf is the design gate of each of roots, by root key, in one read whatever their number:
// the body ReadTreeGates and ReadLineFacts are written over, so that a root's gate is built
// alike wherever the key came from. roots carries no key twice,
// which neither caller can hand it: the roots among a pass's own issues are issue keys, and the
// roots a pass's records leave out are keyed by the trees those issues leave unrooted.
func gatesOf(ctx context.Context, tx pgx.Tx, store Store, roots []string, steps []DesignStep) (map[string]TreeGate, error) {
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
// (ReadLifecycles), one read of the records above the pass's own (ReadParents), one read of the
// tree roots it leaves out (ReadTreeRoots), one gate read (gatesOf), one read of the starts the
// project owes, the three judgements of a parent's open children — the started parents'
// (LineFacts.StartedParentWaits), the children-first waits ChildWaitsOf hands on and those of the
// parents held before their retro (LineFacts.RetroHolds) — from one read of the children
// (readChildWaits: no read where no issue can wait on its children, and two where any can), and
// after the children, where any parent is held before its retro, the roots resting on them
// (RestingRootsOn: two reads, and none where no parent is held so), whatever the number of trees,
// gates and issues involved. project is the Dispatch project key and steps the design-gate steps
// the deployment gates.
//
// The roots are read before the gates because the gates cover both sets of roots: the roots
// among the pass's own issues (treeRoots) and the roots those issues leave out, which this read
// has just found (LineFacts.Roots). The two readers of the ladder hold different records of one
// root — the state the project's whole record, the controller's tick the live issues alone
// (Store.LiveIssues, which leaves out a root that is done or parked) — so a gate read over the
// first set alone gave the tick the zero gate for a root it met only here, whose Open() is
// false, and the member of that tree read design_gate where the state read the ladder's own
// word. Read over both, one issue reads one word wherever it is read (C1 of
// dispatch://INFRA-3107). It costs no further read: the keys join one Gates call.
func ReadLineFacts(ctx context.Context, tx pgx.Tx, store Store, project string, issues []Issue, steps []DesignStep) (LineFacts, error) {
	lifecycles, err := ReadLifecycles(ctx, tx, store, project, issues)
	if err != nil {
		return LineFacts{}, err
	}
	parents, err := ReadParents(ctx, tx, store, project, issues)
	if err != nil {
		return LineFacts{}, err
	}
	roots, err := ReadTreeRoots(ctx, tx, store, project, issues)
	if err != nil {
		return LineFacts{}, err
	}
	gates, err := gatesOf(ctx, tx, store, append(treeRoots(issues), slices.Sorted(maps.Keys(roots))...), steps)
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
	facts := LineFacts{Lifecycles: lifecycles, Gates: gates, Parents: parents, Roots: roots, Owed: owed, OwedRows: rows}
	// The children are read after the rest, since the gates say which issues are children-first
	// candidates, an issue owing a start is a started parent (StartedParents), and the cycle walk
	// crosses the records above the pass's own. The parents held before their retro
	// (RetroHeldParents) are found once, for that read and for the read of the roots that rest on
	// them (RestingRootsOn) after it, which reads nothing where no parent is held so, and the three
	// holds of each are put together from the two reads once (retroHoldsOf). Their walks of the
	// parent chain read the pass's one chain (passChain), which the facts carry (LineFacts.pass).
	held := RetroHeldParents(issues)
	pass := &passChain{issues: issues, above: facts.Parents}
	facts.pass = pass
	var children map[string]ChildWait
	if facts.childrenFirst, facts.StartedParentWaits, children, err = readChildWaits(ctx, tx, store, project, held, facts, pass); err != nil {
		return LineFacts{}, err
	}
	resting, err := RestingRootsOn(ctx, tx, store, held)
	if err != nil {
		return LineFacts{}, err
	}
	facts.RetroHolds = retroHoldsOf(pass, held, children, resting)
	return facts, nil
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
// narrower question than the cap asks. gate is the design
// gate of the issue's tree root, the zero value for a root with no step of it registered.
//
// Four readers are left. Three ask about a worker rather than about the cap: CanWaitOnPerson,
// which lets a root hand work to a person only while its architect has work to hand over;
// admit's resumeSlottedWorkers, which starts the phase worker of an issue that has lost its task
// and must pass over one whose phase has no worker at all; and workflow's unpromptedPhaseWorker,
// which finds a live phase worker nothing will prompt. The fourth, blockers.waitsOf, asks it
// of a started issue at phase.Admitted to decide a slot release: a tree's root there whose
// design gate has never opened is its architect at work, so it waits on nothing and keeps its
// row.
//
// Planning, implementing, testing, reviewing and retro each have a phase worker with work.
// Merging has one too, and keeps it: its merger's own merge is what ends the phase, so there is
// no armed READY waiting on a person here any more. Admitted is work only for
// a root whose design gate has never opened at every step the deployment gates, where its
// architect is writing the design; after that opening the root's own turns start no phase
// worker, and its children do the work. Every other phase is a wait: a child at admitted,
// accepting, held, awaiting_person, awaiting_children, done.
func AtWork(issue Issue, gate TreeGate) bool {
	switch issue.Phase {
	case phase.Planning, phase.Implementing, phase.Testing, phase.Reviewing, phase.Retro:
		return true
	case phase.Merging:
		// The merger works this phase, so an issue in it is at work. Its own merge is what ends
		// it: there is no armed READY to wait for here any more.
		return true
	case phase.Admitted:
		return claim.IsTreeRoot(issue.Key, issue.Tree) && !gate.EverOpened()
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
// rather than writing a second copy of: the linger InWorkflow leaves out is read by no part of
// the slot rule either, and NeedsSlot asks the tree's own terms beside this.
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

// NeedsSlot says whether issue must hold one of the cap's slots now: its tree is running — live,
// with a root that does not linger after its close — and the issue keeps a slot where it stands
// (KeepsSlot). An issue holds its admission slot while it stands in a phase that keeps one
// (PhaseKeepsSlot), from the moment the daemon starts it until it is done or until it leaves
// the workflow, except while it waits on another issue there (docs/site/src/content/docs/legion/concepts.md,
// "Admission and the `legion` label"), so the cap counts the
// issues the daemon has started whose change is still to write, rather than the ones a worker of
// them is running this second. Nothing of a tree that has left the workflow works until its
// re-admission runs it again, which is why the tree's own terms are here.
//
// The Dispatch status KeepsSlot reads is asked because a person's park, close or icebox takes
// the issue out of the workflow before anything moves its phase: the workflow's own leave
// records the linger in the same fact, but a pass that reads the record between the two would
// otherwise count a parked root's slot against the cap for good.
//
// It is the one statement of the release of an issue that needs no slot at all, and the two
// readers that must never disagree both call it: admission releases the slot of every issue it
// answers false for (admit's releaseSlots), and the waiting line offers a returning issue a slot
// only where it answers true (Waiting). The other release, of a started issue that goes on
// working and waits on another issue, is not stated here: it is admit's releaseSlotsForWaits,
// judged by blockers.waitsOf. This answers true of such an issue all the same, so the issue
// keeps its place in the line while it holds no slot. root is the record of
// the issue's tree root; live is whether the tree is live (LiveRoots). The design gate is read
// by no part of this: a gate open or closed changes neither whether the tree runs nor whether
// the issue's phase keeps a row (KeepsSlot). The slot is accounting only: taking one away
// suspends no claim, moves no phase and writes no Dispatch status.
func NeedsSlot(issue, root Issue, live bool) bool {
	return live && !root.Lingers() && KeepsSlot(issue)
}

// CanWaitOnPerson says whether root may enter awaiting_person now: it is a root of a tree in the
// workflow, standing at admitted and not lingering after its close, a worker of it has work
// there (AtWork), and the daemon has started it, which is what the slot row it holds says
// (slotted). The row no longer says a worker of the issue has work — an issue keeps it while it
// waits on a person, and while it waits on its review unless it also waits on another issue
// (KeepsSlot, blockers.waitsOf), and a root like this one, at admitted with a
// design gate that has never opened, keeps it while it waits on another issue too
// (blockers.waitsOf) — it says this root is one of the issues the daemon has started and not yet
// finished with. A root with no row was never started and has no work of its own to hand to
// anybody.
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
// them one. The cap counts the issues the daemon has started rather than the trees, so a child
// waits in this line as a root does.
//
// Priority is the first key of the whole line: each entry stands in the band
// of the priority that places it (PriorityOf: the issue's own Dispatch priority, else its tree
// root's, else after P3), P0 first. Within one band the line keeps the order below, in two parts.
// Priority orders the line only: it takes no slot back from running work. A LineWaitingOnIssue
// entry is placed by its priority and passed over while its wait lasts, as before, and an entry
// that already holds a slot is placed by its priority and promoted at its place, taking no slot.
// The members of a re-admitted tree stand in no line until their root's promotion, so they wait
// behind their root's band.
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
//   - a tree's own root standing at admitted under an open gate, whose first phase can start,
//     takes the moment that gate opened (gateReturn). It holds the row it was started with —
//     an issue the daemon has started keeps one (KeepsSlot) — since a root that waits on
//     another issue, an open child of its own or an unfinished blocked_by link, has given its
//     row back and stands in the line by the next kind instead. So its first phase is work
//     coming back rather than work that never started, and its promotion takes no further slot.
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
// opened, or it arrived from another tree carrying the phase it ran there, or it is a re-admitted
// root whose tree has no lifecycle open under the workflow, whose gate says nothing until its
// promotion opens the tree (ran). So a person's todo on a running root puts it back in no queue:
// the root whose gate has opened in a tree that runs waits here for its own first phase, and the
// root in accepting, in merging or at work waits for nothing. The slot does not say it either, for
// the reason the queued-root loop states at the skip it guards below.
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
//
// Beside the line it answers with a held row for every candidate it leaves behind, the word for
// why coming from the one ladder (HeldReasonOf) rather than from the site that drops it, so one
// issue reads one word (C1). Four of its eighteen skips report nothing: three place the
// candidate, and one passes over an issue released for a wait, which the first loop has placed
// already (LineWaitingOnIssue), so such an issue stands in the line and in no held row. The
// other fourteen report. Two of those fourteen are deferrals — a queued root the second loop may
// place, and an answered root the first loop places — and a deferral reports a row the other
// loop may then place: HeldList drops a row for a key the line names, so a deferral that is
// placed costs one row and no wrong answer.
func Waiting(issues []Issue, slots []Slot, facts LineFacts) ([]LineEntry, []HeldIssue) {
	slotted := make(map[string]struct{}, len(slots))
	for _, slot := range slots {
		slotted[slot.Issue] = struct{}{}
	}
	// The parent chain is walked over the records above these as well (chainOf): a parent
	// already done is no part of the live read, and a walk that stopped at it would hide every
	// issue above it, so the line would offer a slot to a child the acceptance rules hold
	// (AcceptedAncestor). It is the pass's one chain (passChain), through passOver.
	pass := passOver(issues, facts)
	byKey, chain := pass.own(), pass.chain()
	queued := queuedRoots(issues, facts)
	live := liveRoots(issues, facts, queued)
	queuedKeys := make(map[string]struct{}, len(queued))
	// unopened is the queued roots whose promotion has still to open their tree (AwaitsPromotion):
	// nothing of such a tree starts before that promotion, which offers the root's own work and
	// brings back its members (admit's returnOwedMembers), so the line offers none of its members
	// a place (inUnopenedTree). A queued root holding a row the daemon granted it in this tree has
	// been promoted already, and its members stand in the line as the members of any tree that
	// runs. The ladder's tree_closed word reads the open-gate members of these trees
	// (reopensAtPromotion), which is the same set of roots wherever their gate is open.
	unopened := make(map[string]struct{}, len(queued))
	for _, issue := range queued {
		queuedKeys[issue.Key] = struct{}{}
		if _, holds := slotted[issue.Key]; AwaitsPromotion(issue, facts.Lifecycles[issue.Key], facts.Gates[issue.Key], holds) {
			unopened[issue.Key] = struct{}{}
		}
	}
	inUnopenedTree := func(issue Issue) bool {
		_, unopenedTree := unopened[issue.Tree]
		return unopenedTree
	}
	// Every drop site reports what it left behind through one ladder, built over the facts the
	// line itself is judged on, so that the word an issue reads here and the word the assembly
	// reads for an issue no site saw are read from one set of facts (HeldList).
	heldFacts := HeldFacts{ByKey: byKey, Live: live, Chain: chain, Facts: facts}
	var heldRows []HeldIssue
	hold := func(issue Issue) {
		reason, heldBy := HeldReasonOf(issue, heldFacts)
		heldRows = append(heldRows, HeldIssue{Issue: issue.Key, Reason: reason, HeldBy: heldBy})
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
		// A root whose tree is not open yet is in the second part whatever rows it owes: its
		// promotion opens the tree, and the starts of a run it carries are queued then. A member of
		// a tree whose root waits for that promotion is offered no slot either, whatever start it
		// owes: the promotion starts the member's phase role, which a start it already owes stands
		// for (workflow.StartFor), and only then does the tree run. Without that, a start a lease hid
		// from finishFencedStarts through the tree's close would put the member in the line before
		// its tree had opened (dispatch://INFRA-5471).
		if _, waitingAsRoot := queuedKeys[issue.Key]; waitingAsRoot || inUnopenedTree(issue) {
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
		root, recorded := byKey[issue.Tree]
		_, running := live[issue.Tree]
		if !recorded || !NeedsSlot(issue, root, running) {
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
		// return's entry does. An issue that never started has
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
			hold(issue)
			continue
		}
		// This issue holds no row and waits on nothing: the skip above took every holder out of
		// the line, so the entry says so and its promotion takes one for it.
		returned = append(returned, timed{entry: LineEntry{Issue: issue, Kind: LineReturning}, at: owed.CreatedAt, id: owed.ID})
	}

	var starting []LineEntry
	for _, issue := range queued {
		if issue.Answered() {
			hold(issue)
			continue
		}
		// A queued root holding a row the daemon granted it in this tree is one it has already
		// started — a root whose architect is writing the spec, which a person set back to todo
		// — and a second promotion would reopen an open tree and start a second architect. A
		// root that arrived from another tree is the other half of that branch and stays: an
		// issue holds its row from the moment the daemon starts it until its work is past its
		// review (KeepsSlot), and a queued root keeps it even while it waits on another issue
		// (blockers.waitsOf leaves a queued root out), so a root that arrives carrying a phase up
		// to the review carries the row of the tree it left (HandedOverFrom, the one mark of that
		// arrival, which ran is written over and the promotion that opens the root's own tree
		// clears) while the tree of its own has still to be opened. Dropped from the line,
		// nothing would promote it, the release would keep its row (NeedsSlot) and no later pass
		// would find it again. A root that arrives in accepting, retro or merging holds no row at all and
		// reaches the line by the other half of the branch, as any queued root does. The tree it
		// left usually takes a carried row back first — its root lingers from its close, and the
		// release reads the linger in the transaction that records the close, before the
		// hand-over writes the root of its own — so the carried row is the case this is written
		// against rather than the usual one. It is the one statement of that reasoning: the doc
		// above, QueuedRoot, WaitsForSlot and AwaitsPromotion, which reads it, each point here
		// rather than repeating it.
		_, holds := slotted[issue.Key]
		if _, awaits := unopened[issue.Key]; !awaits {
			hold(issue)
			continue
		}
		// The live roots the issue's own Dispatch chain names are the ancestry hold, and the
		// word that reports it names the same roots: one statement, read here and by the ladder
		// (liveAncestry, HeldAncestry), so the hold and the word cannot drift apart.
		if len(liveAncestry(issue, live)) > 0 {
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
		if issue.Phase != phase.Admitted || issue.AwaitingPerson() || OutOfWorkflow(issue.Status) {
			hold(issue)
			continue
		}
		// A member of a tree whose root waits for its promotion takes no first phase before that
		// promotion opens the tree (inUnopenedTree). Such a root's gate is shut, and the gate's own
		// skip below holds the member, but in two cases: a re-admitted root whose tree has no
		// lifecycle open under the workflow, whose gate says nothing until its promotion (ran), and
		// a root another tree handed over with an open gate (dispatch://INFRA-5471). A root whose
		// label a person took off while its re-admission still owes its members their return takes
		// no first phase of its own either: nothing hands its tree to the daemon, so the admission
		// pass brings no member back (record.MembersReturnOwed), and a first phase would start its
		// work ahead of them. A marked root that is still handed over is not held here: the pass
		// brings its members back before the line is judged, and a root that pass passes over for
		// the promotion's hold (admit's heldKeys) is offered its first phase here and stopped by
		// the promotion loop through the same hold, so the candidates behind it keep waiting.
		if _, waitingAsRoot := queuedKeys[issue.Key]; waitingAsRoot || !runs(issue) || inUnopenedTree(issue) ||
			(issue.MembersOwed && !issue.HandedOver) {
			hold(issue)
			continue
		}
		// A root that handed work to a person starts nothing of its tree while it waits: its
		// members start with its resume (admit's promotion of the answered root).
		if root := byKey[issue.Tree]; root.AwaitingPerson() {
			hold(issue)
			continue
		}
		gate := facts.Gates[issue.Tree]
		if !gate.Open() {
			hold(issue)
			continue
		}
		// A child an unfinished Dispatch issue holds where it stands waits for that issue, not
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
		// and run of its tree when the wait begins (admit's tellChildrenFirst): the root's own
		// architect for a root, and for a child the architect above it, since the child's own
		// starts only with that first phase. A person's todo on a child that ran or was held starts
		// and stops nothing (workflow's ReenterChild), so its own architect, if it held its role,
		// still holds it and is told. The state names it under admission.blocked meanwhile.
		if _, waits := facts.ChildWaits[issue.Key]; waits {
			hold(issue)
			continue
		}
		// An issue beneath an ancestor the daemon has already accepted waits while that ancestor
		// stands in retro or merging (HeldUnderAccepted): starting it would let the ancestor merge work its own
		// head does not carry. A person's move of such an issue to todo records it here at
		// admitted and leaves it here; the architect is told why when it enters. The chain is
		// walked over the records above this pass's own as well, so that a parent already done
		// does not hide the merging issue above it.
		if HeldUnderAccepted(issue, chain) {
			hold(issue)
			continue
		}
		_, holds := slotted[issue.Key]
		entry := LineEntry{Issue: issue, Kind: LineFirstPhase, Holds: holds}
		// A tree's own root has held a slot since its architect began the design (KeepsSlot).
		// Under an open gate it gives that row back while it waits on another issue: an open
		// child of its own is its children-first wait, and an unfinished blocked_by link is the
		// other (blockers.waitsOf, admit's releaseSlotsForWaits). A root released so stands in
		// the line by the first loop and is passed over at the top of this one, and the skips
		// above hold one whose wait the release has still to read. A root that reaches here
		// waits on neither and holds the row of its run: its first phase is work coming back,
		// so it stands in the first part at the moment the gate opened (TreeGate.OpenedAt), and
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
	// (projection's waitingLineReason). The moment that placed a returned entry is the sort key
	// itself, taken from the timed value rather than read a second time from the records
	// (LineEntry.Since); and the rank that placed a starting entry is its tree root's, which the
	// sort above read the same record for.
	//
	// Priority is the first key of the whole line: each entry stands in the band of the priority
	// that places it (PriorityOf: the one Dispatch resolves it to, else, with an older Dispatch
	// that resolves none, its own, else its tree root's, else after P3), and within one band the
	// returned part comes first, oldest first, and then the starting part by its tree root's rank
	// and then its own, the order each part was just sorted into. So the two parts are laid out
	// one after the other and sorted again by band alone, stably, which keeps that order within
	// each band; an entry's place within its part counts within its band and part, read off that
	// final order.
	line := make([]LineEntry, 0, len(returned)+len(starting))
	for _, waiting := range returned {
		entry := waiting.entry
		entry.Part, entry.Since, entry.Priority = PartReturned, waiting.at, PriorityOf(entry.Issue, byKey[entry.Issue.Tree])
		line = append(line, entry)
	}
	for _, entry := range starting {
		entry.Part, entry.RootRank, entry.Priority = PartStarting, byKey[entry.Issue.Tree].Rank, PriorityOf(entry.Issue, byKey[entry.Issue.Tree])
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

// queuedRoots is every root handed to Legion whose tree has not been opened and which may take a
// slot: one that never ran, a re-admitted one and a handed-over one (QueuedRoot). It is read
// before the ancestry hold, because the hold is read against the live trees these same roots
// make (liveRoots).
func queuedRoots(issues []Issue, facts LineFacts) []Issue {
	queued := make([]Issue, 0, len(issues))
	for _, issue := range issues {
		if QueuedRoot(issue, facts.Lifecycles[issue.Key], facts.Gates[issue.Key]) {
			queued = append(queued, issue)
		}
	}
	return queued
}

// QueuedRoot says whether issue is a root handed to Legion that waits in the line for its tree
// to be opened: a root that never ran, a re-admitted one and a handed-over one, each standing in
// todo, not waiting on a person (whose answer resumes it where it stands) and with a record that
// does not already say this generation runs (ran, over lifecycle, its tree's lifecycle, and gate,
// its own design gate). It is the one statement of the rule, read by the line itself, by the set
// of live trees (liveRoots), by the set of running ones (TreeRuns) and by the relay of a person's
// decision (WaitsForSlot), so that no two of them disagree about a root that has still to start.
//
// The tree's lifecycle does not answer this alone, though it is what says the tree is live
// (liveRoots): a lifecycle that stands open says the tree once opened, not that this generation
// of the root is running, and a root whose tree ran and ended queues again with that row still
// standing until its cleanup confirms. It is read in one case only, which ran states: a
// re-admitted root whose members are still owed their return. The slot does not answer it
// either: a root arriving from another tree in a phase that keeps a slot may carry the row it
// held there while the tree of its own has still to be opened, which Waiting's queued-root loop
// states at the skip it guards.
func QueuedRoot(issue Issue, lifecycle treelifecycle.Lifecycle, gate TreeGate) bool {
	if !issue.HandedOver || !claim.IsTreeRoot(issue.Key, issue.Tree) || ran(issue, lifecycle, gate) {
		return false
	}
	return !issue.AwaitingPerson() && issue.Status == "todo"
}

// ran says whether this root's own record shows the generation it carries already running under
// Legion. Two things say so, and the slot says neither any more. The record stands in a phase of
// its own (Issue.CarriesRun — neither admitted nor done): it is at work, merging, in accepting,
// held or waiting on a person. Or its design gate has opened at every step the
// deployment gates at some point (TreeGate.EverOpened), after which the root stands at admitted
// while its architect revises the design and its own first phase waits in the line as a member
// of its tree (LineFirstPhase).
// A re-admission clears the gate with the rest of the generation, so a tree Legion takes back
// queues again as a root that never ran, which is what it is.
//
// The gate says nothing of a root whose re-admission still owes its members their return
// (Issue.MembersOwed) while its tree's lifecycle is not open under the workflow: its cleanup
// reserved or confirmed, as after a linger that ended, or no lifecycle at all. Such a tree has
// nothing open to run in, and only the promotion opens its next epoch, so the root stays queued
// whatever its gate says, and that promotion brings the members back (dispatch://INFRA-5471). A
// re-admission inside the linger finds the lifecycle open, and the gate's opening makes that tree
// run at once (MembersReturnOwed).
//
// The one record that carries a run and has still to run here is a root that has just arrived
// from another tree with the phase it ran there (Issue.HandedOverFrom): that run belonged to the
// tree it left, and the tree of its own it becomes has yet to be opened, so its promotion opens
// it and starts the role of whatever phase it carries. The mark is the arrival and not the
// root's whole life: the promotion that opens its tree clears it (admit's promoteHolds), because
// from then on the run the record carries is this tree's own. Without that a person's todo on
// such a root, running and holding no row of its own tree in accepting or merging, would queue
// it here again and promote it a second time, starting its merger over a merge that has already
// been made.
func ran(issue Issue, lifecycle treelifecycle.Lifecycle, gate TreeGate) bool {
	gateSaysRan := gate.EverOpened() && !(issue.MembersOwed && !lifecycle.OpenUnderWorkflow())
	return issue.HandedOverFrom == "" && (issue.CarriesRun() || gateSaysRan)
}

// AwaitsPromotion says whether root waits for the promotion that opens its tree: it is a queued
// root (QueuedRoot, over lifecycle and gate, its tree's lifecycle and its own design gate), and
// holds, whether it holds a slot row, names no row the daemon granted it in this tree. A queued
// root holding such a row has been promoted already, while a root that arrived from another tree
// (Issue.HandedOverFrom) may carry the row it held there and still wait; Waiting's queued-root
// loop, which reads this, states why. Until that promotion nothing of the tree runs: the root's
// carried phase, and each member a re-admission left mid-phase, start their runs there (admit's
// returnOwedMembers), so the boot and the settle open and record no run of such a tree
// (workflow's generationStarted).
func AwaitsPromotion(root Issue, lifecycle treelifecycle.Lifecycle, gate TreeGate, holds bool) bool {
	return QueuedRoot(root, lifecycle, gate) && !(holds && root.HandedOverFrom == "")
}

// MembersReturnOwed says whether the admission pass acts on root's mark now: a re-admission owes
// the members their return (Issue.MembersOwed), the root's design gate has opened
// (TreeGate.EverOpened) and its tree runs (TreeRuns, over lifecycle, its tree's lifecycle), so the
// root is no queued root and no promotion will come for it. That is a re-admitted root whose
// architect registered an approved design while the root still waited in the line: the promotion
// that brings a re-admitted tree's members back (admit's returnOwedMembers) no longer comes, and
// the pass brings them back instead, once, and clears the mark (dispatch://INFRA-5471). This
// release's promotion clears the mark itself, so a tree it opened never reads true here. A true
// answer can also be a stale mark an earlier release's promotion left, which never names the
// column (admit's promotedByEarlierRelease): the pass clears it and starts nothing (admit's
// returnMembersOfRunningTrees).
//
// The root must still be handed to Legion (Issue.HandedOver), as a promotion asks of a queued root:
// a root whose label a person took off while it waited is no queued root either, and the tree runs
// by its lifecycle, but nothing hands that tree's work to the daemon, so the mark stands until the
// label is back and the members come back then.
func MembersReturnOwed(root Issue, lifecycle treelifecycle.Lifecycle, gate TreeGate) bool {
	return root.MembersOwed && root.HandedOver && claim.IsTreeRoot(root.Key, root.Tree) && gate.EverOpened() &&
		TreeRuns(root, lifecycle, gate)
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
	return lifecycle.OpenUnderWorkflow() || QueuedRoot(root, lifecycle, gate)
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
	lifecycle, gate, err := RootFacts(ctx, store, tx, root, steps)
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
	return lifecycle.OpenUnderWorkflow() && !root.Lingers() && !QueuedRoot(root, lifecycle, gate)
}

// TreeRunning is TreeRuns for a caller holding one root and no facts of a pass: it reads that
// root's tree lifecycle and its design gate, and asks the one rule. steps are the design-gate
// steps the deployment gates.
func TreeRunning(ctx context.Context, store Store, tx pgx.Tx, root Issue, steps []DesignStep) (bool, error) {
	if !claim.IsTreeRoot(root.Key, root.Tree) || root.Lingers() {
		return false, nil
	}
	lifecycle, gate, err := RootFacts(ctx, store, tx, root, steps)
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
// It is the one statement of that rule, and every reader that must agree calls it: the park of
// such a root (workflow.Engine.leave), the claim release row every other park queues
// (outbox.releaseClaim), and the boot's answer to whether a run no start counted is open
// (workflow.Engine.uncountedRunOpen).
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

// RootFacts is one root's tree lifecycle and design gate, for the single-root readings of the
// rules above — TreeLive, which the recording of a todo child under a live tree asks, TreeRunning,
// which workflow's sessionStatusWrite asks of a session's status write, and QueuedRoot and
// AwaitsPromotion for a reader holding one root (WaitsForSlot, the outbox's startRoot, workflow's
// generationStarted): a pass that judges many trees reads them in bulk instead (ReadLineFacts).
// steps are the design-gate steps the deployment gates.
func RootFacts(ctx context.Context, store Store, tx pgx.Tx, root Issue, steps []DesignStep) (treelifecycle.Lifecycle, TreeGate, error) {
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
