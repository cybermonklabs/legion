// Package projection joins durable workflow facts into the daemon state response.
package projection

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Project joins durable issue facts to current supervision claims for GET /legion/v1/state. The
// caller owns the repeatable-read transaction, so its record reads and claims share one snapshot.
// project is the Dispatch project whose pending status writes and held lifecycle rows the state
// lists. steps are the design-gate steps the deployment gates (config.Gates.Steps), which each
// root's view answers its own state of; the empty list gates none. stranded is the claims the
// daemon judges nothing will wake (workflow.Stranded), handed in because that judgement reads
// the gated steps and the released wakes over the store and this package may not read
// internal/workflow: it is what says an issue's work has stopped on its worker rather than on
// its place in a queue (whyStates), and the caller reads it in the same transaction, so the
// state is still one snapshot. Nil is nothing stranded. reports is each claim's latest
// capability report, by claim (LEGION-663): a claim with one shows it whatever its incarnation —
// the view carries the reporting process's, for a reader to compare with the locator's — and a
// claim with none shows no capabilities. Nil is no reports. The role whose phase entry and given
// entry each issue's view carries is the role that works its current phase (phase.RoleFor). now
// is the instant the held rows are read at (record.Store.HeldRows) and their waits are measured
// from.
func Project(ctx context.Context, tx pgx.Tx, s record.Store, project string, steps []record.DesignStep, claims []supervise.Claim, stranded []supervise.Claim, reports map[claim.Token]capabilities.Report, now time.Time) (api.State, error) {
	issues, err := s.Issues(ctx, tx)
	if err != nil {
		return api.State{}, err
	}
	knownIssues := make(map[string]struct{}, len(issues))
	// lingering is every tree whose root lingers, or closed after it lingered: only a root carries a
	// linger deadline, and re-admission clears it.
	lingering := make(map[string]bool)
	// memoryByTree is every tree's memory draw, by the tree's key: only a root's record carries
	// one, and a root's key is its own tree (record.Issue.MemoryGroup). Every issue of the tree is
	// shown its root's, so an issue's view answers what its own roles run with.
	memoryByTree := make(map[string]memoryDraw)
	for _, issue := range issues {
		knownIssues[issue.Key] = struct{}{}
		if issue.Lingers() {
			lingering[issue.Key] = true
		}
		if issue.MemoryGroup != "" {
			memoryByTree[issue.Key] = memoryDraw{group: issue.MemoryGroup, mode: issue.MemoryDrawMode, at: issue.MemoryDrawnAt}
		}
	}
	slots, err := s.Slots(ctx, tx)
	if err != nil {
		return api.State{}, err
	}
	pending, err := s.PendingStatusWrites(ctx, tx, project)
	if err != nil {
		return api.State{}, err
	}
	held, err := s.HeldRows(ctx, tx, project, "", now)
	if err != nil {
		return api.State{}, err
	}
	// The open production checks, by issue: the check is read once for the whole state, since a
	// closed check shows nothing and the open ones are few.
	openChecks, err := s.OpenProductionChecks(ctx, tx)
	if err != nil {
		return api.State{}, err
	}
	checks := make(map[string]record.ProductionCheck, len(openChecks))
	for _, check := range openChecks {
		checks[check.Issue] = check
	}

	// The entry each claim was last queued a task of, by the issue and role the claim holds
	// (supervise.Claim.GivenEntry): the number an issue's view shows beside the entry its phases
	// row names. It is keyed by issue and role rather than by claim token because the token on
	// the phases row can be empty, for a role no claim was ever created for, or stale, from a
	// claim an earlier run retired, where the claim of this issue and this role is the one the
	// admission pass weighs (admit's resumeSlottedWorkers).
	givenEntries := make(map[issueRole]int64, len(claims))
	for _, current := range claims {
		givenEntries[issueRole{issue: current.Issue, role: current.Role}] = current.GivenEntry
	}

	claimViews := make(map[claim.Token]api.ClaimView, len(claims))
	for _, current := range claims {
		if current.Locator != nil {
			if err := current.Locator.Validate(); err != nil {
				return api.State{}, fmt.Errorf("project claim %s: %w", current.Token, err)
			}
		}
		// Every field of the claim record is served or withheld by a decision, and the view that
		// serves it is built in one place (api.ClaimViewOf, api.ClaimRecordFields).
		view := api.ClaimViewOf(current)
		if report, reported := reports[current.Token]; reported {
			view.Capabilities = api.CapabilityReportViewOf(report)
		}
		claimViews[current.Token] = view
	}

	slotViews := make(map[string]api.SlotView, len(slots))
	active := make([]string, 0, len(slots))
	for _, slot := range slots {
		if _, known := knownIssues[slot.Issue]; !known {
			continue
		}
		slotViews[slot.Issue] = api.SlotView{Index: slot.Index, AdmittedAt: slot.AdmittedAt}
		active = append(active, slot.Issue)
	}

	projected := api.State{Issues: make(map[string]api.Issue, len(issues))}
	for _, issue := range issues {
		view := api.Issue{
			Key:           issue.Key,
			Generation:    issue.Generation,
			Phase:         issue.Phase,
			Status:        issue.Status,
			Tree:          issue.Tree,
			Repo:          issue.Repo,
			DefaultBranch: issue.DefaultBranch,
			// When this root's linger runs out, nil for every issue that is not a lingering
			// root: it is what a reader of a held issue served no reason reads the suppression
			// of that reason by (api.Issue.LingerUntil).
			LingerUntil: issue.LingerUntil,
			// The parent chain a labelled root is held under, empty for every other record: the
			// hold on it is the ancestry hold, and nothing else on this wire names the chain.
			Ancestry: issue.Ancestry,
			Workers:  map[claim.Role]api.PhaseView{},
		}
		// The tree's memory draw, which every issue of the tree is shown, a done one included: it
		// is what the issue's roles ran with, and nothing redraws it.
		if draw, drawn := memoryByTree[issue.Tree]; drawn {
			view.MemoryGroup, view.MemoryDrawMode, view.MemoryDrawnAt = draw.group, draw.mode, draw.at
		}
		// An escalation waits on the controller only while its tree runs. A held issue of a tree
		// that lingers or closed keeps the reason on its record, and shows it again once the tree is
		// re-admitted, which leaves the issue held.
		//
		// The phase the hold took the issue out of is shown whatever the tree is doing, which is
		// the deliberate difference: the reason is a decision to act on and a lingering tree has
		// none, while the phase is a fact of the record, so every issue the body shows at
		// `phase: held` names the phase that stopped.
		if issue.Hold != nil {
			view.HeldFrom = string(issue.Hold.From)
			if !lingering[issue.Tree] {
				view.HoldReason = string(issue.Hold.Reason)
			}
		}
		if slot, ok := slotViews[issue.Key]; ok {
			view.Slot = &slot
		}
		if issue.AwaitingPerson() {
			view.WaitingOn = waitingOn(*issue.Awaiting)
		}

		phases, err := s.Phases(ctx, tx, issue.Key)
		if err != nil {
			return api.State{}, err
		}
		reviewDecision := ""
		for _, item := range phases {
			if item.Role == claim.RoleReviewer && item.Decision != nil {
				reviewDecision = item.Decision.State
			}
			claimView, live := claimViews[item.Claim]
			if !live {
				continue
			}
			if item.Role == claim.RoleArchitect {
				view.Architect = &claimView
				continue
			}
			// The worker's view exists only while its claim is live, so its counts of the moves
			// back ride with the claim exactly as `rounds` does: a role whose process has gone
			// shows neither, and the record keeps both for the role's next claim.
			worker := api.PhaseView{
				Claim:         claimView,
				HandoffCommit: item.HandoffCommit,
				Pass:          item.Pass,
				Rounds:        item.Rounds,
				BackwardMoves: item.BackwardMoves,
			}
			if item.HandoffRecord != "" {
				worker.HandoffRecord = &api.HandoffRecordView{ID: item.HandoffRecord, Version: item.HandoffVersion}
			}
			view.Workers[item.Role] = worker
		}
		// The entry the role of this issue's current phase stands on, and the entry that role was
		// given: the pair a reader tells a role holding the work its row names from one the daemon
		// has still to send that work to (api.Issue.PhaseEntry). A phase that names no phase worker
		// role carries neither number — a root at admitted whose design gate has never opened is at
		// work (record.AtWork) and phase.RoleFor names no role for that phase, so the admission
		// pass passes it over and a difference written for it would stand for ever — and no row of
		// a role the issue has moved past is read, since that role keeps its entry and is sent
		// nothing.
		// The entry is 0 where the rows hold none of that role (record.PhaseRowOf), a role no
		// transition has ever handed work on, and it is read with the same call admit's
		// owedEntryRoles reads for the same role from the same rows, so the state shows what the
		// admission pass weighs a claim against.
		if role := phase.RoleFor(issue.Phase); role != "" {
			view.PhaseEntry = record.PhaseRowOf(phases, role).Entry
			view.GivenEntry = givenEntries[issueRole{issue: issue.Key, role: role}]
		}

		pr, err := s.PullRequest(ctx, tx, issue.Key)
		if err != nil {
			return api.State{}, err
		}
		if pr != nil {
			view.PullRequest = &api.PullRequestView{
				Repo:           pr.Repo,
				Number:         pr.Number,
				Head:           pr.HeadSHA,
				State:          string(pr.State),
				MergeState:     string(pr.MergeState),
				ReviewDecision: reviewDecision,
			}
		}

		gates, err := s.GatesOf(ctx, tx, issue.Key)
		if err != nil {
			return api.State{}, err
		}
		gate := record.SpecGate(gates)
		if gate != nil {
			view.DesignGate = &api.GateView{
				ArtifactID:      gate.ArtifactID,
				CurrentVersion:  gate.LatestVersion,
				ApprovedVersion: gate.ApprovedVersion,
			}
		}
		// The deployment's gate as this root stands in it, one entry for each step it gates. Only
		// a root holds gate rows — a child's work is held at its root's steps — so a child answers
		// no entry at all rather than one unregistered step for each the deployment gates. The
		// entries are the configured steps and not the rows: a row of a step the deployment no
		// longer gates is kept in the record, where a deployment that gates it again reads its
		// true state, and is in no entry here, since it holds nothing (classify.DesignGatesOpen).
		if issue.Key == issue.Tree {
			view.GatedSteps = api.GatedStepViewsOf(steps, gates)
		}
		if check, ok := checks[issue.Key]; ok {
			view.Check = checkView(check)
		}
		projected.Issues[issue.Key] = view
	}

	for _, current := range claims {
		// The daemon's own controller (`controller: daemon`) holds a claim on no issue: the state
		// shows it as the project's controller (controllerLocator), never as an issue's worker,
		// since an issue keyed "" refuses the whole document at every client, which reads an
		// issue's `key` as a non-empty string.
		if claim.IsController(current.Role, current.Issue, current.Tree) {
			continue
		}
		claimView := claimViews[current.Token]
		view, known := projected.Issues[current.Issue]
		if !known {
			// A claim on an issue the workflow does not record — an operator's spawn, which is
			// what Stage 2 makes and what the merge-queue holder is — is listed for the claim's
			// sake, and says so: an empty phase is no value of the phase enum and an empty status
			// is refused too, so the whole document would be refused over a row that is a
			// legitimate state.
			view = api.Issue{
				Key:     current.Issue,
				Phase:   api.Unrecorded,
				Status:  string(api.Unrecorded),
				Workers: map[claim.Role]api.PhaseView{},
			}
		}
		if current.Role == claim.RoleArchitect {
			if view.Architect == nil {
				view.Architect = &claimView
			}
		} else if _, known := view.Workers[current.Role]; !known {
			view.Workers[current.Role] = api.PhaseView{Claim: claimView}
		}
		projected.Issues[current.Issue] = view
	}

	// Everything admission is judged from is judged over this project's own records. The state
	// lists every project's issues, for the sake of a claim on an issue of another, while the
	// cap, the waiting line and the tick read the project's own live records alone
	// (record.Store.LiveIssues, admit's pass): another project's queued root, and its root a
	// person has answered, would otherwise stand in this project's `admission.waiting`, and
	// whatever either waits on in its `blocked`, as work this daemon neither runs nor admits.
	// They reach one read at all because the issues table is one table shared by every daemon of
	// the database (the issue slots migration, "shared by every daemon of the database").
	own := make([]record.Issue, 0, len(issues))
	for _, issue := range issues {
		if issue.Project == project {
			own = append(own, issue)
		}
	}
	facts, err := record.ReadLineFacts(ctx, tx, s, project, own, steps)
	if err != nil {
		return api.State{}, err
	}
	// What Legion is waiting for, judged from the mirror of what Dispatch says the issues it
	// judges wait on, read in this transaction's snapshot like every other fact here, through the
	// one judgement admission itself promotes from (blockers.JudgeLine): the rows of every issue
	// a blocker can hold, the children-first wait, the line those two leave, and the rows of the
	// issues the line names. The deciders' own judgement (blockers.Blocked) then names exactly
	// the roots admission skips, the issues it holds back and the parents whose children go
	// first.
	rows, facts, line, dropped, err := blockers.JudgeLine(ctx, tx, s, project, own, slots, facts, nil, now)
	if err != nil {
		return api.State{}, err
	}
	projected.Admission.Active = active
	// The roots of the live trees, which no slot says any more: a slot counts one issue the
	// daemon has started, and a tree is live before the daemon has started anything of it — a
	// root waiting in the line for its tree to be opened holds no row, and neither does a child
	// of it. The pass's own facts already name every lifecycle, so this costs no further read. A
	// lifecycle is keyed by the project token (record.ReadLifecycles) and this pass reads them
	// all under this daemon's, so another project's root would be judged against a lifecycle
	// that is not its own.
	trees := record.LiveRoots(own, facts)
	projected.Admission.Trees = slices.Sorted(maps.Keys(trees))
	projected.Admission.Waiting = make([]string, len(line))
	// Each issue the line names carries its own place in it (contract 58), counted from 1 at the
	// front, beside the length of the list it stands in. Nothing is filtered out: a member a
	// Dispatch link blocks stands in the line and takes its place like every other member, and
	// what holds it is `admission.blocked` beside this. Every issue of the line has a view here
	// already, so the place is set where the key is listed: the line is built from `own`, the
	// issues of this project among the issues the loop above gave a view to, each of them.
	//
	// Beside the place, each member carries the line's own fields for a program to read
	// (api.WaitingLineView): the part it waits in and why, its place within that part and the
	// part's length, whether it holds a slot, and what placed it — all of them read from the
	// entry the line holds beside the issue (record.LineEntry), which the loop below walks, so
	// that the fields a member carries and the key listed beside them come from one entry. The
	// two parts are counted once over the whole line before the loop, because this pass holds
	// every entry of it: a member's `partLength` is the count of the entries carrying its part
	// and nothing else, and the other part's count is what the sentence below names.
	parts := make(map[record.LinePart]int, 2)
	for _, entry := range line {
		parts[entry.Part]++
	}
	for i, entry := range line {
		key := entry.Issue.Key
		projected.Admission.Waiting[i] = key
		holds := entry.Holds
		view := projected.Issues[key]
		view.WaitingLine = &api.WaitingLineView{
			Place: i + 1, Length: len(line),
			Part: entry.Part.String(), Kind: entry.Kind.String(),
			PartPlace: entry.PartPlace, PartLength: parts[entry.Part],
			HoldsSlot: &holds,
			RootRank:  entry.RootRank,
		}
		// The two ranks and the root's key are what placed a member of the starting part, and
		// that part alone carries them: a member of the returned part was placed by the moment
		// its work came back and answers neither. The entry carries its tree root's rank, the
		// one fact it is placed by that its own record does not hold; the other two are read
		// off that record, which is the entry's own issue (record.LineEntry.RootRank).
		if entry.Part == record.PartStarting {
			view.WaitingLine.OwnRank, view.WaitingLine.RootKey = entry.Issue.Rank, entry.Issue.Tree
		}
		// The moment the work came back is on a member of the returned part alone, and even
		// there the record may not know it — a gate approved by a registration that read no
		// event carries no moment (record.TreeGate.OpenedAt) — so the wire carries `since`
		// where the entry has one and nothing where it has none.
		if !entry.Since.IsZero() {
			since := entry.Since
			view.WaitingLine.Since = &since
		}
		// The sentence a person reads (waitingLineReason), rendered from the entry itself, the
		// length of the part it stands in and the length of the part it does not, which is what
		// a member of the starting part waits behind. The render reads those and nothing else,
		// so the words have one source and a test of them needs no database.
		other := parts[record.PartReturned]
		if entry.Part == record.PartReturned {
			other = parts[record.PartStarting]
		}
		view.WaitingLine.Reason = waitingLineReason(entry, parts[entry.Part], other)
		projected.Issues[key] = view
	}
	projected.Admission.Blocked = blockers.Blocked(rows, own, line, record.ChildWaitRows(facts.ChildWaits))
	// What the daemon runs nothing of and queues nowhere, beside the two lists above: every
	// record of this project the daemon holds in the workflow (record.InWorkflow) that stands in
	// neither the active set nor the line, with the one word for why (record.HeldList). The rows
	// the pass itself left behind are its fourth answer, and the ladder answers for a candidate
	// no drop site saw, over the same facts and the same line these lists are built from, so one
	// issue reads one word here and in the controller's tick.
	projected.Admission.Held = record.HeldList(own, slots, line, dropped, facts)
	// Why each issue that is not working now is not working, one rendered answer on its own view.
	// It is decided here, after the line and `blocked` are both in hand, because three of the four
	// states are read off them, and from the two judgements this pass still holds apart — the
	// links and the children-first waits — which the published list has merged by now.
	//
	// It reads the held list above for the sentence of most issues it answers, and it is not
	// that list: the list says where every issue the daemon holds in the workflow stands and why
	// nothing of it runs, and this answers why an issue is not working now. The two part at an
	// issue whose work runs past its review, which the list names and this answers for not at
	// all; where they part and why is written once, with the arm that parts them
	// (atWorkPastReview).
	//
	// The running roots are the term that says an issue past its review is at work: it holds no
	// slot since dispatch://INFRA-2560, so no list of the three places it and the answer turns
	// on whether its tree runs at all (whyStates, atWorkPastReview). They are read from the
	// records and the facts this pass already holds, so the reading costs no query, and they
	// are the same roots record.LiveRoots is read from above — live is wider than running,
	// and a parked root's tree is neither.
	whyStates(&projected, own, record.RunningRoots(own, facts), stranded, rows, facts.ChildWaits)
	// Every issue of a live tree carries that tree's quiet time (contract 101): `treeMovedAt`,
	// the newest movement the daemon has seen anywhere in the tree, and `quietHours`, how long
	// it has been still at the instant the rest of this state is measured at. The pair is the
	// tree's, so every issue of one tree carries the same two values — a child past its own
	// review, which holds no slot, and a done child recorded under the tree among them, since
	// what has stopped is the tree and not the issue a reader happens to hold. A tree that is no
	// longer live is measured for nobody: its root is in no `admission.trees` entry, there is
	// nothing of the deployment's left to wait on, and each of its issues carries neither key.
	//
	// The measure is read once for the whole state (quietTrees), from the records and the claims
	// this pass already holds, and a live root is always one of its trees whatever its own
	// status says (record.QuietTrees), so the lookup below cannot miss.
	quiet := quietTrees(own, claims, now)
	for _, issue := range own {
		if _, live := trees[issue.Tree]; !live {
			continue
		}
		measure := quiet[issue.Tree]
		movedAt, hours := measure.MovedAt, measure.Quiet.Hours()
		view := projected.Issues[issue.Key]
		view.TreeMovedAt, view.QuietHours = &movedAt, &hours
		projected.Issues[issue.Key] = view
	}
	projected.PendingStatusWrites = make([]api.PendingStatusWrite, 0, len(pending))
	for _, row := range pending {
		if _, known := knownIssues[row.Issue]; !known {
			continue
		}
		projected.PendingStatusWrites = append(projected.PendingStatusWrites, api.PendingStatusWrite{
			Issue:     row.Issue,
			Payload:   row.Payload,
			Attempts:  row.Attempts,
			NextAt:    row.NextAt,
			LastError: row.LastError,
		})
	}
	projected.HeldOutboxRows = make([]api.HeldOutboxRow, len(held))
	for i, row := range held {
		projected.HeldOutboxRows[i] = api.HeldOutboxRow{
			ID:            row.ID,
			Kind:          string(row.Kind),
			Issue:         row.Issue,
			WaitedSeconds: max(0, int64(now.Sub(row.NextAt).Seconds())),
			WaitsOn:       row.WaitsOn,
		}
	}
	return projected, nil
}

// quietTrees is the state's measure of its trees: how long each tree the state's own records
// name has been still and whether any claim of it is at work (record.QuietTrees). It makes no
// query of its own. own is the project's records, the same list the waiting line and the live
// roots are judged from; claims are the claims Project was handed, read in its one snapshot
// with those records, which carry each agent's state and step times already
// (record.TreeClaimsOf), so the state never reads the claims a second time; now is the instant
// the rest of the state is measured at.
//
// Where it is called from: Project, just past the live roots the measure answers for, which
// writes each live tree's measure onto every issue of that tree (`treeMovedAt`, `quietHours`).
// It stays a function of its own rather than a line of Project because its three arguments are
// the three facts the measure may read and no others: a later reader of the same question takes
// it from here and cannot quietly read a fourth. The admission pass measures the same thing
// with the same statement, over the claims its own read gives it (admit's quietTrees), so the
// two read one statement of the measure and neither keeps a rule of its own.
//
// One statement is what they share, and not one answer: the records each is handed differ.
// This one is given `own`, every record of the project the state holds, a done one included,
// while the pass is given record.Store.LiveIssues, which leaves out a done record holding no
// slot. A root is a member of its tree whatever its own status says (record.TreeMember), so a
// tree whose root is done is measured here from that root and is no tree of the pass's at all,
// and the two can read one tree from different members. Nothing rests on their agreeing: the
// state reports what it measures, and the pass tells the controller only of the trees it runs
// (record.RunningRoots), which are trees whose records the live read holds.
func quietTrees(own []record.Issue, claims []supervise.Claim, now time.Time) map[string]record.TreeQuiet {
	return record.QuietTrees(own, record.TreeClaimsOf(claims), now)
}

// memoryDraw is one tree's memory draw as its root's record holds it (record.Issue.MemoryGroup,
// MemoryDrawMode, MemoryDrawnAt): the group the draw gave the tree, the project's mode it was
// drawn under, and when.
type memoryDraw struct {
	group string
	mode  string
	at    *time.Time
}

// issueRole is one issue and one of its roles, the pair a claim is held under (supervise.Claim's
// Issue and Role): the key of the lookup that answers what each role was last given.
type issueRole struct {
	issue string
	role  claim.Role
}

// checkView is an open production check as the state shows it (api.CheckView): the role the check
// records as the one that works it, the last wake once a look has woken, and the next wake —
// whichever names the next look, the time the checking agent named for itself on a pending report
// or the retry of a look nobody reported (record.ProductionCheck.WakeAt), and the check's bound,
// the moment its last look starts, where neither stands.
func checkView(check record.ProductionCheck) *api.CheckView {
	next := check.BoundAt
	if check.WakeAt != nil {
		next = *check.WakeAt
	}
	view := &api.CheckView{
		Repo: check.Repo, PullRequest: check.Number, MergeCommit: check.MergeSHA, OpenedAt: check.OpenedAt,
		Role: string(check.Role), Looks: check.Looks, LookOpen: check.LookOpen, NextWakeAt: next, Last: check.Last,
	}
	if check.LastWakeAt != nil {
		view.LastWake = &api.CheckWakeView{Kind: check.LastWakeKind, Detail: check.LastWakeDetail, At: *check.LastWakeAt}
	}
	return view
}

// waitingOn is the view of what a root in awaiting_person waits on: the gate shape when the wait
// names a spec version, otherwise the question; the answer only once a person gave one. The act
// that answered travels as the record's own word for it (record.Answer), `declined` for a person
// who closed an ask of the root rather than answer it among them, so a value the record gains is
// served as it stands and the contract number is what holds a plugin to the list it parses
// (api.DaemonAPIVersion).
func waitingOn(w record.Awaiting) *api.WaitingOnView {
	view := &api.WaitingOnView{Kind: "ask", Ask: w.Ask}
	if w.GateVersion != 0 {
		view = &api.WaitingOnView{Kind: "gate", GateVersion: w.GateVersion, ArtifactID: w.Artifact}
	}
	if !w.Since.At.IsZero() {
		since := w.Since.At
		view.Since = &since
	}
	if w.AnsweredAt != nil {
		answeredAt := *w.AnsweredAt
		view.AnsweredAt, view.AnsweredBy = &answeredAt, w.AnsweredBy
	}
	return view
}
