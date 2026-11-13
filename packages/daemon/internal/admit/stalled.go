package admit

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// quietTrees is the pass's measure of its trees: how long each tree of the project has been
// still and whether any claim of it is at work (record.QuietTrees), over the records the pass
// has already read and one read of the claims of the project (record.Store.ProjectClaims). It
// is one query for the whole pass, like the issues and the slots beside it: a pass that judges
// every tree of the project may make no read per tree.
//
// issues are the pass's own live records and now the instant the pass measures from, so that
// every reading of a tree it makes — this one and the record-only one it is filtered with — is
// taken from one instant. The claim read is keyed by the claim project token, which
// `claims.project` holds, not by the Dispatch project key the issue records carry.
//
// Where it is called from: tellStalledTrees, after blockers.JudgeLineHolds and the releases,
// where the pass holds the facts it judges from and has given back every slot it is going to. The
// measure is behind this method rather than in the pass because the rule that acts on it and
// the read it needs belong together. The state's projection answers the same question from the
// same statement over the claims it already holds (projection's quietTrees), so the pass and
// the state read one statement of the measure and neither keeps a rule of its own. They are
// handed their own records, though: this is given the project's live ones and the state every
// record it holds, a done one included, so a tree whose root is done is measured by the state
// and by no pass of this. Nothing here rests on their agreeing — the rule below tells the
// controller only of trees this daemon runs (record.RunningRoots), every one of which the live
// read holds.
func (a *Admission) quietTrees(ctx context.Context, tx pgx.Tx, issues []record.Issue, now time.Time) (map[string]record.TreeQuiet, error) {
	project, err := claim.ProjectToken(a.project)
	if err != nil {
		return nil, err
	}
	claims, err := a.store.ProjectClaims(ctx, tx, project)
	if err != nil {
		return nil, err
	}
	return record.QuietTrees(issues, claims, now), nil
}

// tellStalledTrees tells the project's controller, once for each stall, of every tree of this
// daemon's own that has stopped. A tree has stopped when all of this holds: its root is one
// the daemon runs (record.RunningRoots), it has been quiet for longer than
// `stalled_tree_notice_hours` (record.TreeQuiet.Quiet, UseStalledTreeNotice), no claim of it
// is at work (record.TreeQuiet.AtWork), its newest movement stands after the stall its root
// was last told about (record.Issue.TreeStalledToldAt), and either some issue of it is held by
// nothing the daemon knows of (everyIssueWaitsByDesign over holds, the caller's own) or the
// project's line can no longer move (lineCanMove).
//
// The holds of the daemon's own silence a tree only while the project's line can still move,
// which is the whole of why lineCanMove is read here and not per tree: a project no work of
// which can start again by itself has every tree of it named, however its members wait
// (dispatch://INFRA-2560, R5). The clause is a fact of the project and not of one tree, so it
// is answered once for the pass beside the per-tree walk rather than inside it, where every
// tree would ask the same question again.
//
// holds is why the daemon starts no issue of the project now (record.StartHold), by issue key,
// as the walk that built this pass's waiting line wrote it (record.WaitingAndHolds, handed over
// by blockers.JudgeLineHolds). The rule is given it rather than reading it because reading it
// is a walk of every issue of the project and the pass has made that walk already, to judge the
// line it promotes from: the measure and the line are then one reading of one project, which no
// second walk could promise, and the pass walks the project's issues once however many rules
// read the holds.
//
// Why the mark is a movement and not a time of telling: the rule reads the tree's own movement
// already, so a tree told about is a tree whose movement the daemon has named. The mark is
// written only for a notice that went into the outbox, in this same transaction (the answer
// wakeController gives), so a stall found while no controller was registered, or while an
// older stalled-tree notice of the same root still waited to be published, is told at a later
// pass rather than marked told and lost. The notice is keyed per issue
// (record.NoticePerClaim), so a second tree that stops while the first tree's notice waits is
// told in the same pass.
//
// What it reads, and in what order. This runs inside promoteHolds, on every fact, every tick
// and every boot, and on a healthy project the answer is always that nothing has stopped, so
// each read is made only once the pass cannot answer without it, and every read it makes is one
// read for the whole pass and never a read per tree:
//
//  1. the records alone (record.QuietTrees with no claims), which costs no query. A claim's
//     step times only ever move a tree's measured movement forward, so the quiet time with the
//     claims is never longer than the quiet time without them: a root the records alone show
//     inside the bound is inside it whatever the claims say. A pass whose running roots are all
//     inside it ends there, which is every pass of a project whose trees are working;
//  2. the claims of the project (quietTrees, one read), which carry the step times the records
//     do not and whether an agent of the tree is at work. The told mark is compared with this
//     movement and never with the record-only one, which can stand behind it;
//  3. whether a controller is registered (record.Store.ControllerRegistered, one read). It is a
//     refusal wakeController makes below, and one that answers for every root of this pass at
//     once, so a project with no controller registered builds no membership and judges no tree
//     for a notice that could not be queued;
//  4. the membership of the candidate trees and the silencing rule over the holds the caller
//     handed over (everyIssueWaitsByDesign, lineCanMove), which cost no query and no walk of
//     the project's issues: the holds are the line's own walk's, made before this method was
//     reached. The rule that decides which trees are told is asked here, before anything is
//     built or read for one tree, so that a tree it silences costs the pass nothing further;
//  5. for the trees that are going to be told and for no other, whether each root's own
//     tree-stalled notice is still unsent (record.Store.ControllerNoticesPending, one batched
//     read) and then the pull requests of those trees' members (record.Store.PullRequestsOf,
//     one batched read). Neither is a read per root, and no reason is built for a root the
//     first of them refuses. wakeController still decides: it asks the registration and the
//     pending notice again, and the mark is written only for a row that went into the outbox.
//
// The liveness clause adds no read of its own: its three terms are the slots this pass already
// holds, the claims step 2 read and the holds the caller handed over (lineCanMove), and it is
// answered only once a root has come through the bound, the measure and the registration above.
func (a *Admission) tellStalledTrees(ctx context.Context, tx pgx.Tx, issues []record.Issue,
	facts record.LineFacts, slots []record.Slot, holds map[string]record.StartHold,
) error {
	if a.stalledTreeNotice <= 0 {
		return nil
	}
	now := a.now()
	// Step 1: the reading that costs no query. A root inside the bound here is inside it under
	// any claim of its tree, so this is the whole of a working project's pass.
	recorded := record.QuietTrees(issues, nil, now)
	running := record.RunningRoots(issues, facts)
	candidates := make([]record.Issue, 0, len(running))
	for _, issue := range issues {
		if _, runs := running[issue.Key]; !runs {
			continue
		}
		if recorded[issue.Key].Quiet > a.stalledTreeNotice {
			candidates = append(candidates, issue)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	// Step 2: the measure the rule decides on.
	quiet, err := a.quietTrees(ctx, tx, issues, now)
	if err != nil {
		return err
	}
	stalled := make([]record.Issue, 0, len(candidates))
	for _, root := range candidates {
		measure := quiet[root.Key]
		if measure.AtWork || measure.Quiet <= a.stalledTreeNotice {
			continue
		}
		if told := root.TreeStalledToldAt; told != nil && !measure.MovedAt.After(*told) {
			continue
		}
		stalled = append(stalled, root)
	}
	if len(stalled) == 0 {
		return nil
	}
	// Step 3: the refusal that answers for every root of this pass at once, asked before
	// anything is built for a notice it would refuse. A controller that is not registered
	// refuses the whole pass with one read, so nothing below is gathered or judged for a notice
	// nothing would read.
	registered, err := a.store.ControllerRegistered(ctx, tx, a.project)
	if err != nil || !registered {
		return err
	}
	// Step 4: the tree is its root and the issues under it the workflow still runs
	// (record.TreeMember), which is the membership the measure counts and the membership the
	// notice names: a child the record shows as done is no member of either, since a comment on
	// its old pull request is no movement of the tree and its phase is nobody's to act on. It is
	// gathered for the candidate roots above alone, since no other tree of the project is named
	// by anything this pass writes.
	telling := make(map[string]struct{}, len(stalled))
	for _, root := range stalled {
		telling[root.Key] = struct{}{}
	}
	members := make(map[string][]record.Issue, len(stalled))
	for _, issue := range issues {
		if _, tells := telling[issue.Tree]; !tells || !record.TreeMember(issue) {
			continue
		}
		members[issue.Tree] = append(members[issue.Tree], issue)
	}
	// Why the daemon does not start each issue of the project now is holds, which the caller
	// hands over: the walk that built the waiting line this pass judged wrote it as it passed
	// each issue over (record.WaitingAndHolds through blockers.JudgeLineHolds). The rules below
	// ask that set and derive nothing from the line: the same term, read once from an owed start
	// row, once from the line's membership and once from an issue's need of a slot, disagreed
	// with itself three times inside one review of this change, each reading right about the
	// shape in front of it (dispatch://INFRA-2560, R5).
	//
	// It is handed over rather than read here because reading it is a walk of every issue of the
	// project and no pass may walk them twice: the pass has already made that walk to judge its
	// line, so this method's answer costs it nothing. A pass that never reaches this method — a
	// deployment that sets no bound, a project whose trees are all working — pays nothing for
	// the holds it is handed and never looks at them.
	//
	// The silencing rule is asked here, before anything beyond that membership is built or read
	// for one tree, because it is the rule that decides which trees are told: a tree it silences
	// costs this pass no read, no reason and no further work of any kind.
	//
	// A hold of the daemon's own silences a tree only while the project's line can still move
	// (lineCanMove, dispatch://INFRA-2560, R5). The answer is one fact of the project, so it is
	// taken once here and asked of every tree of the pass: where the line cannot move, nothing
	// of the project can start again by itself and every tree of it is named, however its
	// members wait, and the walk of the members below is not made at all.
	canMove := a.lineCanMove(issues, slots, holds, quiet)
	var stopped []record.Issue
	named := 0
	for _, root := range stalled {
		if canMove && a.everyIssueWaitsByDesign(members[root.Key], holds) {
			continue
		}
		stopped = append(stopped, root)
		named += len(members[root.Key])
	}
	if len(stopped) == 0 {
		return nil
	}
	// Step 5: the refusal that answers for one root, asked of the trees this pass is going to
	// tell and of no other. A root whose own tree-stalled notice still waits unsent is left for
	// a later pass; the kind is keyed by its issue and by no role at all (record.NoticePerClaim,
	// wakeController), which is the pair asked for here. Every root is asked in the one read
	// (record.Store.ControllerNoticesPending), because this runs on every fact, every tick and
	// every boot of a deployment whose projects hold hundreds of trees, and a read per root
	// would put that many round trips on the fact lock.
	roots := make([]string, 0, len(stopped))
	for _, root := range stopped {
		roots = append(roots, root.Key)
	}
	pending, err := a.store.ControllerNoticesPending(ctx, tx, a.project, record.TreeStalledNotice, roots, "")
	if err != nil {
		return err
	}
	unsent := make([]record.Issue, 0, len(stopped))
	keys := make([]string, 0, named)
	for _, root := range stopped {
		if _, waits := pending[root.Key]; waits {
			continue
		}
		unsent = append(unsent, root)
		for _, member := range members[root.Key] {
			keys = append(keys, member.Key)
		}
	}
	if len(unsent) == 0 {
		return nil
	}
	requests, err := a.store.PullRequestsOf(ctx, tx, keys)
	if err != nil {
		return err
	}
	for _, root := range unsent {
		measure := quiet[root.Key]
		notice := record.ControllerNotice{Kind: record.TreeStalledNotice,
			Reason: stalledTreeReason(root.Key, measure, members[root.Key], requests)}
		told, err := a.wakeController(ctx, tx, notice, root.Key, a.now())
		if err != nil {
			return fmt.Errorf("tell the controller that the tree of %s has stopped: %w", root.Key, err)
		}
		if !told {
			continue
		}
		if err := a.store.MarkTreeStalledTold(ctx, tx, root.Key, measure.MovedAt); err != nil {
			return err
		}
		intake.OnCommit(ctx, func() {
			a.log.Warn("admission: told the controller a tree has stopped", "issue", root.Key,
				"quiet", measure.Quiet, "movedAt", measure.MovedAt)
		})
	}
	return nil
}

// silencedTree is the measure's answer for every hold the daemon can have on an issue
// (record.StartHold): whether an issue standing still for that hold is waiting by design, so
// that a tree whose every quiet issue is held for such reasons has not stopped and is told to
// nobody. It is the one table of those answers, and the rules below are two lookups in it:
// nothing here derives a hold of its own from the waiting line, because the line and the holds
// come out of one pass of the record package (record.WaitingAndHolds) and a second derivation is
// the fault dispatch://INFRA-2560 was filed for.
//
// The six that silence are R5's four waits by design, read as the holds the daemon's own pass
// writes for them: a root waiting on a person, which only a person's act moves; an issue a
// Dispatch blocked-by link holds; an issue the children-first rule holds; and the fourth, "an
// issue that needs a slot and holds none, whatever holds its start", which is three holds of
// the set rather than one, because the daemon keeps such an issue out of the line in three
// ways: the cap's own hold on a member of the line, a member whose tree's root waits on a
// person, and a member whose tree's design gate is closed. The test of that wait is the need
// and not the mechanism: each earlier reading of it named a mechanism and missed a shape that
// answers to the same cause.
//
// Two directions decide the rest, and they are different questions. A hold that means nothing
// holds the issue (record.StartHoldNone) does not silence, and neither does a hold this table
// does not know at run time, which a lookup that misses answers false for: an issue standing
// still for no recorded reason is the thing this issue was filed about, so the default is to
// name the tree. The other is a judgement about a known state: the cap's own hold silences a
// member whose start the daemon never wrote at all — a fault rather than a wait, read from the
// start row — and that is accepted rather than overlooked, because such an issue holds no
// admission slot and so costs the project none of the capacity this measure exists to recover,
// while its root, which does hold a slot, is judged by the other holds and is named when
// nothing else of the tree moves.
//
// The holds that do not silence are each a tree's own work or a person's: an issue under an
// accepted ancestor waits for that ancestor to close, a root a live tree has not released waits
// for its architect, an issue of a tree that has left the workflow waits for a re-admission,
// and an issue a person has taken out of the workflow waits for that person. None of them is
// the daemon holding work it means to start, which is what a wait by design is.
//
// A hold with no row here fails admit's TestTheMeasureAnswersEveryStartHold, which reads the
// set from the record package's own source: a hold added to the set and left out of this table
// would otherwise be ignored quietly, which is the fault rather than a new place to hide it.
var silencedTree = map[record.StartHold]bool{
	record.StartHoldNone:         false,
	record.StartHoldPerson:       true,
	record.StartHoldRootPerson:   true,
	record.StartHoldBlocker:      true,
	record.StartHoldChildren:     true,
	record.StartHoldDesign:       true,
	record.StartHoldCap:          true,
	record.StartHoldAccepted:     false,
	record.StartHoldUnreleased:   false,
	record.StartHoldTreeGone:     false,
	record.StartHoldLeftWorkflow: false,
}

// silences says whether an issue the daemon holds for hold is waiting by design, from the one
// table above. A hold the table does not name answers false, which is the default the table's
// doc states: an issue standing still for a reason nothing has decided about is named.
func silences(hold record.StartHold) bool { return silencedTree[hold] }

// lineCanMove says whether the project's line can still move: whether any work of this project
// can start again by itself. It is the liveness clause of the silencing rule
// (dispatch://INFRA-2560, R5): the waits the daemon makes on purpose leave a tree unnamed only
// while this holds, because a wait is a wait for something, and a project where nothing is
// coming waits for nothing.
//
// Why a project that fails both terms is stopped for good: a slot is freed by a phase or by a
// status change, and both of those need a worker, and a worker needs a slot. So where no slot
// holder can finish and no agent is at work, no slot is ever given back, no issue of the line
// is ever promoted, and every tree of the project stands exactly where it is until a person
// moves one. Naming those trees is the whole point of the rule — that project is the deadlock
// it was filed to find — so the caller silences nothing there, however its members wait.
//
// The two terms, either of which is the line moving, in the order they are asked, which is
// cheapest first:
//
//  1. the claim term: some claim of the project is at work, taken from the measure the pass has
//     already made (record.TreeQuiet.AtWork over every tree of quiet). That cut is the
//     measure's own (record's restingStates: idle, suspended, failed and retired are no work
//     under way), so this rule and the per-tree refusal above read one statement of what being
//     at work is. It costs a walk of a map the pass holds and no read at all. It is the
//     project's answer and not one tree's: an agent working anywhere in the project will free
//     its slot when it is done, and that slot goes to the line;
//  2. the slot term: some issue of the project holds one of the cap's slots and is silenced by
//     no hold of the daemon's (silences over the pass's own holds) — that issue's worker can
//     finish, which frees the slot. The slots are the pass's own rows, the ones it judged the
//     line from, so the fact is the slots table's and not a phase rule's reading of it: an
//     issue holds a slot when a row names it. The table carries every project's rows while the
//     cap counts this project's alone (ownSlots), and the walk below is over this project's own
//     records, so a row of another project matches no issue here and says nothing about this
//     line. The holds are asked of a holder exactly as they are asked of a member, from the one
//     table, and the cap's own hold is simply one a holder never carries: it is an issue of the
//     line that holds no slot (record.StartHoldCap, record.LineEntry.Holds).
//
// The slot term is over every issue of the project, and never over the trees the pass is about
// to tell. An issue of a tree nobody is telling — a tree inside the bound, a tree already
// told, a tree of a root this daemon does not run — holding a slot and silenced by nothing is
// the line moving, because its worker frees that slot for the line like any other. A later
// reader narrowing this walk to the stalled trees' members would make the rule answer
// "stopped" for every project whose quiet trees all wait, which is the opposite of what it
// says.
//
// It is answered once per pass, by the caller, and never per tree: the question is the
// project's and its answer is the same for every tree of the pass.
func (a *Admission) lineCanMove(issues []record.Issue, slots []record.Slot,
	holds map[string]record.StartHold, quiet map[string]record.TreeQuiet,
) bool {
	for _, measure := range quiet {
		if measure.AtWork {
			return true
		}
	}
	holders := make(map[string]struct{}, len(slots))
	for _, slot := range slots {
		holders[slot.Issue] = struct{}{}
	}
	for _, issue := range issues {
		if _, holdsSlot := holders[issue.Key]; !holdsSlot {
			continue
		}
		if silences(holds[issue.Key]) {
			continue
		}
		// This holder waits for nothing the daemon knows of: its worker can finish and give
		// the slot back, so the project's line can move.
		return true
	}
	return false
}

// everyIssueWaitsByDesign says whether every issue of a quiet tree is held by something the
// daemon itself knows about: a hold of its own pass that silences a tree (silencedTree), which
// is an issue that has not stopped but is holding still for a reason that will end.
//
// It answers one tree and is half of the silencing rule: a tree is left unnamed only where
// this answers true and the project's line can still move (lineCanMove, which the caller asks
// once for the whole pass). The liveness half is no part of this walk, because it is a fact of
// the project and this is a walk of one tree's members.
//
// Every, and never any: "A tree whose every quiet issue is one of those is never named"
// (dispatch://INFRA-2560, R5). A tree with one issue in a wait of the daemon's and one that has
// simply stopped is named, because nothing is going to move the stopped one, and that tree —
// a root waiting on a person over a child whose worker died, say — is exactly what this rule
// was filed to find. Answered on any one member, that single wait would silence the whole tree
// for as long as it lasted.
//
// Every member of the tree is every quiet issue of it, which is what makes this the rule R5
// states. The caller reaches this only for a tree nothing of which is at work
// (record.TreeQuiet.AtWork) and whose quiet time stands past the deployment's bound, and that
// quiet time is measured over the whole tree at once (record.QuietTrees: the newest movement
// of any member and of any claim of a member). So no issue of a candidate tree has moved
// inside the bound, and the walk below is the walk over the tree's quiet issues. The members
// are the measure's own (record.TreeMember), and a tree always holds at least its root, so no
// empty walk ever answers true here.
//
// Which hold a member carries is no judgement of this rule's: the pass that reads the project's
// issues, claims and slots writes it (record.WaitingAndHolds) and the table beside it says what
// it means, so a member held by something this measure has never heard of is named rather than
// excused. A task, a notice or a message queued for a role is no hold of the daemon's at all: a
// notice queued to a suspended role waits for a role nothing brings back, so an issue with one
// in the outbox has stopped like any other, and nothing of the outbox reaches the set. An issue
// in `held` is no wait either — it waits for its architect, which is the tree's own work — and
// the pass holds it for nothing, so it answers record.StartHoldNone and its tree is told like
// any other stall. An issue whose merge is still to land stands in `merging`, where the merger
// works, and is read the same way: the pass holds that issue for nothing either, so it answers
// record.StartHoldNone, silences nothing, and its tree is named once every claim of the tree is
// at rest and nothing of it has moved inside the bound. A merger at work is what keeps such a
// tree unnamed, and that is read from the claim and never from the phase
// (record.TreeQuiet.AtWork, which the caller asks before this walk).
func (a *Admission) everyIssueWaitsByDesign(members []record.Issue, holds map[string]record.StartHold) bool {
	for _, issue := range members {
		if silences(holds[issue.Key]) {
			continue
		}
		// Nothing of the daemon's holds this issue: it has stopped, and so has its tree.
		return false
	}
	return true
}

// stalledTreeReason is what a stalled-tree notice tells the project's controller: the root, how
// long the tree has been quiet, and, for each member of the tree (record.TreeMember), the phase
// its record carries — `held` for a held issue — the role whose turn that issue's standing is
// (workflow.RoleFor over record.StandsAt, which is the phase a held issue was held from) or
// that the phase has no role, and the state of its pull request. It ends by telling the
// controller to read the tree and decide.
//
// The membership is the measure's own and is read from the one predicate, so the notice names
// exactly the issues whose stillness was measured: a root, whatever its own status says, and
// every issue under it the workflow still runs. The caller gathers members by that same
// predicate (tellStalledTrees), so each member it hands over is named here and nothing is
// tested a second time.
//
// It is written in short sentences a controller agent reads without decoding anything: the
// daemon knows the tree has stopped and knows nothing of why, so what it hands over is the
// facts and the decision, never a guess. The pull request is named from the record of its own
// table (requests, keyed by issue), because the state a stopped tree is most often in is a
// merged one — the tree whose work landed and whose roles then stopped — which no list of the
// open pull requests could answer.
func stalledTreeReason(root string, measure record.TreeQuiet, members []record.Issue, requests map[string]record.PullRequest) string {
	var reason strings.Builder
	fmt.Fprintf(&reason, "%s has stopped. The tree has been quiet for %.1f hours. No agent of the tree is at work.",
		root, measure.Quiet.Hours())
	for _, member := range members {
		fmt.Fprintf(&reason, "\n%s is in %s. ", member.Key, member.Phase)
		if role := workflow.RoleFor(record.StandsAt(member)); role != "" {
			fmt.Fprintf(&reason, "The %s has the turn. ", role)
		} else {
			reason.WriteString("No role works this phase. ")
		}
		if pr, recorded := requests[member.Key]; recorded {
			fmt.Fprintf(&reason, "Pull request %d is %s.", pr.Number, pr.State)
		} else {
			reason.WriteString("The record holds no pull request.")
		}
	}
	reason.WriteString("\nRead the tree and decide what it needs. You can ask a role what it waits on." +
		" You can move an issue back a phase. You can park the tree.")
	return reason.String()
}
