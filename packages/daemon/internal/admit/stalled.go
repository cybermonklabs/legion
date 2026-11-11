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
// Where it is called from: tellStalledTrees, after blockers.JudgeLine and the releases, where
// the pass holds the facts it judges from and has given back every slot it is going to. The
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
// was last told about (record.Issue.TreeStalledToldAt), and not every issue of it waits by a
// rule of the daemon's own (everyIssueWaitsByDesign).
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
// each read is made only once the pass cannot answer without it:
//
//  1. the records alone (record.QuietTrees with no claims), which costs no query. A claim's
//     step times only ever move a tree's measured movement forward, so the quiet time with the
//     claims is never longer than the quiet time without them: a root the records alone show
//     inside the bound is inside it whatever the claims say. A pass whose running roots are all
//     inside it ends there, which is every pass of a project whose trees are working;
//  2. the claims of the project (quietTrees, one read), which carry the step times the records
//     do not and whether an agent of the tree is at work. The told mark is compared with this
//     movement and never with the record-only one, which can stand behind it;
//  3. whether a controller is registered, and whether this root's own tree-stalled notice is
//     still unsent (record.Store.ControllerRegistered, ControllerNoticePending). Both are
//     refusals wakeController makes below, and a root either one refuses is left out here, so
//     that no membership, reason or pull-request read is built for a notice that cannot be
//     queued. wakeController still decides: it asks both again, and the mark is written only
//     for a row that went into the outbox;
//  4. the membership of the stalled trees, the slots of their members and one batched read of
//     those members' pull requests (record.Store.PullRequestsOf), each built for the roots that
//     are already candidates and for no other issue of the project.
func (a *Admission) tellStalledTrees(ctx context.Context, tx pgx.Tx, issues []record.Issue, slots []record.Slot,
	facts record.LineFacts, line []record.LineEntry,
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
	// Step 3: the two refusals, each asked before anything is built for the notice it would
	// refuse. A controller that is not registered refuses every root of this pass at once.
	registered, err := a.store.ControllerRegistered(ctx, tx, a.project)
	if err != nil || !registered {
		return err
	}
	// A root whose own tree-stalled notice still waits unsent is refused on its own. The kind is
	// keyed by its issue and by no role at all (record.NoticePerClaim, wakeController), which is
	// the pair asked for here.
	unsent := make([]record.Issue, 0, len(stalled))
	for _, root := range stalled {
		pending, err := a.store.ControllerNoticePending(ctx, tx, a.project, record.TreeStalledNotice, root.Key, "")
		if err != nil {
			return err
		}
		if !pending {
			unsent = append(unsent, root)
		}
	}
	if len(unsent) == 0 {
		return nil
	}
	// Step 4: the tree is its root and the issues under it the workflow still runs
	// (record.TreeMember), which is the membership the measure counts and the membership the
	// notice names: a child the record shows as done is no member of either, since a comment on
	// its old pull request is no movement of the tree and its phase is nobody's to act on. It is
	// gathered for the roots above alone, since no other tree of the project is named by
	// anything this pass writes.
	telling := make(map[string]struct{}, len(unsent))
	for _, root := range unsent {
		telling[root.Key] = struct{}{}
	}
	members := make(map[string][]record.Issue, len(unsent))
	of := make(map[string]struct{}, len(unsent))
	for _, issue := range issues {
		if _, tells := telling[issue.Tree]; !tells || !record.TreeMember(issue) {
			continue
		}
		members[issue.Tree] = append(members[issue.Tree], issue)
		of[issue.Key] = struct{}{}
	}
	// The slots of those members and the places the waiting line holds for them, by issue,
	// which the fourth wait below reads as one pair: an issue of the line that holds no slot is
	// an issue the cap itself is holding. Only a member is ever asked for, so neither map names
	// an issue of another tree.
	//
	// It is the line rather than the owed start row because the line is where both shapes of
	// that wait stand. An issue whose work came back owes a start the outbox holds unclaimable
	// while the slots table names no row of it (record.SuperviseRequest.NeedsSlot, the outbox's
	// own outboxInOrder), and it is in the line's first part. An issue waiting for its first
	// phase owes no row at all, because the start is written by the promotion that grants the
	// slot (record.LineFirstPhase, Engine.StartFirstPhase), and it is in the line's second
	// part. Read from the owed row alone, as this rule first was, the second shape is held by
	// nothing and its tree is told as stopped — a root at admitted waiting for its children
	// with one released child in the line, which is the commonest shape a tree waiting for the
	// cap takes, and which eight trees of the INFRA deployment stood in at 2026-11-11T16:36Z.
	slotted := make(map[string]struct{}, len(of))
	for _, slot := range slots {
		if _, member := of[slot.Issue]; member {
			slotted[slot.Issue] = struct{}{}
		}
	}
	waiting := make(map[string]struct{}, len(of))
	for _, entry := range line {
		if _, member := of[entry.Issue.Key]; member {
			waiting[entry.Issue.Key] = struct{}{}
		}
	}
	var stopped []record.Issue
	named := 0
	for _, root := range unsent {
		if a.everyIssueWaitsByDesign(members[root.Key], slotted, waiting, facts) {
			continue
		}
		stopped = append(stopped, root)
		named += len(members[root.Key])
	}
	if len(stopped) == 0 {
		return nil
	}
	keys := make([]string, 0, named)
	for _, root := range stopped {
		for _, member := range members[root.Key] {
			keys = append(keys, member.Key)
		}
	}
	requests, err := a.store.PullRequestsOf(ctx, tx, keys)
	if err != nil {
		return err
	}
	for _, root := range stopped {
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

// everyIssueWaitsByDesign says whether every issue of a quiet tree stands in one of the four
// waits the daemon makes on purpose, each of which is an issue that has not stopped but is
// holding still for a reason the daemon itself knows.
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
// The four waits:
//
//  1. a root waiting on a person (record.Issue.AwaitingPerson): the tree waits for an answer,
//     and only a person's act moves it. Only a root ever stands in that phase, so this wait
//     answers for the root alone and every member under it must stand in one of the other
//     three;
//  2. an issue a Dispatch blocked-by link holds where it stands (record.LineFacts.Held, the
//     judgement blockers.LineWaitsOn reads), which the line passes over every pass;
//  3. an issue the children-first rule holds (record.LineFacts.ChildWaits, waitsForChildren),
//     whose own first phase waits for the open Dispatch children under it. It is that
//     judgement and never record.ChildrenFirstCandidates, which names every issue at admitted
//     under an open gate whether or not a child of it is open: taken for this, it would call
//     the commonest quiet shape there is a wait, a tree whose root stands at admitted doing
//     nothing;
//  4. an issue of the waiting line that holds no slot (the line of the pass, with the pass's
//     own slots): the cap itself is holding it, which is the waiting line working as designed.
//     Both shapes of that wait stand in the line and neither is read from anywhere else: an
//     issue whose work came back owes a start marked for its admission slot, which the outbox
//     holds unclaimable while the slots table names no row of it, and an issue waiting for its
//     first phase owes no start at all, because the promotion that grants the slot is what
//     writes it. Read from the owed row alone, this rule called the second shape a tree that
//     had stopped (dispatch://INFRA-2560, R5, and the test over the behaviour in
//     first_slot_wait_test.go).
//
// Only the cap's own hold is a wait of the daemon's here. A task, a notice or a message queued
// for a role is none: a notice queued to a suspended role waits for a role nothing brings
// back, so an issue with one in the outbox has stopped like any other. An issue in `held` and
// an issue in `awaiting_merge` are no wait either — a held issue waits for its architect,
// which is the tree's own work, and an issue awaiting its merge waits for GitHub, which is
// nothing the daemon holds it for — so a tree holding one of them is told like any other
// stall.
func (a *Admission) everyIssueWaitsByDesign(members []record.Issue, slotted, waiting map[string]struct{},
	facts record.LineFacts,
) bool {
	for _, issue := range members {
		if issue.AwaitingPerson() {
			continue
		}
		if _, held := facts.Held[issue.Key]; held {
			continue
		}
		if a.waitsForChildren(facts.ChildWaits, issue) {
			continue
		}
		if _, waits := waiting[issue.Key]; waits {
			if _, holds := slotted[issue.Key]; !holds {
				continue
			}
		}
		// This issue stands in none of the four: it has stopped, and so has its tree.
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
