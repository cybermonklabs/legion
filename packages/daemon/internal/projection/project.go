// Package projection joins durable workflow facts into the daemon state response.
package projection

import (
	"cmp"
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
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Project joins durable issue facts to current supervision claims for GET /legion/v1/state. The
// caller owns the repeatable-read transaction, so its record reads and claims share one snapshot.
// project is the Dispatch project whose pending status writes and held lifecycle rows the state
// lists. steps are the design-gate steps the deployment gates (config.Gates.Steps), which each
// root's view answers its own state of; the empty list gates none. experiments are the project's
// declared experiments (config.Project.ExperimentsDeclared), whose order each issue's
// `assignments` ride in (assignmentViews); the empty list declares none, and every answer a
// record still holds then rides in the experiments' names. stranded is the claims the
// daemon judges nothing will wake (workflow.Stranded), handed in because that judgement reads
// the gated steps and the released wakes over the store and this package may not read
// internal/workflow: it is what says an issue's work has stopped on its worker rather than on
// its place in a queue (whyStates), and the caller reads it in the same transaction, so the
// state is still one snapshot. Nil is nothing stranded. reports is each claim's latest
// capability report, by claim (LEGION-663): a claim with one shows it whatever its incarnation —
// the view carries the reporting process's, for a reader to compare with the locator's — and a
// claim with none shows no capabilities. Nil is no reports. now is the instant the held rows are
// read at (record.Store.HeldRows) and their waits are measured from.
func Project(ctx context.Context, tx pgx.Tx, s record.Store, project string, steps []record.DesignStep, experiments []config.Experiment, claims []supervise.Claim, stranded []supervise.Claim, reports map[claim.Token]capabilities.Report, now time.Time) (api.State, error) {
	issues, err := s.Issues(ctx, tx)
	if err != nil {
		return api.State{}, err
	}
	knownIssues := make(map[string]struct{}, len(issues))
	// lingering is every tree whose root lingers, or closed after it lingered: only a root carries a
	// linger deadline, and re-admission clears it.
	lingering := make(map[string]bool)
	for _, issue := range issues {
		knownIssues[issue.Key] = struct{}{}
		if issue.Lingers() {
			lingering[issue.Key] = true
		}
	}
	// Every experiment answer the record holds for this project's issues, by issue key, in one
	// statement (record.Store.Assignments). The database is shared by every project's daemon, so
	// the read is joined to the issue records of project rather than taking the table whole,
	// exactly as the issue read above is filtered to it; it is not listed by the keys in hand,
	// since that list would select the same rows at the cost of a parameter the size of the
	// table. A row says which unit it covers: a `tree` row is its tree root's and every issue of
	// the tree is shown it, an `issue` row is the issue's own. So an issue's answers are its
	// root's tree rows and its own issue rows, which assignmentViews joins.
	assignments, err := s.Assignments(ctx, tx, project)
	if err != nil {
		return api.State{}, err
	}
	place := declaredPlaces(experiments)
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
		// Every experiment answer the issue runs under, a done issue's included: it is what its
		// roles ran with, and nothing redraws it. The three memory fields are the answer of the
		// experiment named `memory` spelled out for the readers that knew it before any
		// experiment was declared (contract 49).
		view.Assignments = assignmentViews(assignments, issue.Key, issue.Tree, place)
		view.MemoryGroup, view.MemoryDrawMode, view.MemoryDrawnAt = memoryOf(view.Assignments)
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
	rows, facts, line, err := blockers.JudgeLine(ctx, tx, s, project, own, slots, facts, nil)
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
	// Why each issue that is not working now is not working, one rendered answer on its own view.
	// It is decided here, after the line and `blocked` are both in hand, because three of the four
	// states are read off them, and from the two judgements this pass still holds apart — the
	// links and the children-first waits — which the published list has merged by now.
	whyStates(&projected, own, stranded, rows, facts.ChildWaits)
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

// assignmentViews is every experiment answer that applies to the issue key, whose tree's root is
// tree, in the order the project's file declares the experiments: its tree's tree-unit rows,
// which every issue of the tree runs under, and its own issue-unit rows. An issue no answer
// applies to gets nil, which leaves the key off the wire.
//
// place is one experiment's place in that declared order, and the length of it for an
// experiment the file no longer names (declaredPlaces): such a row stays in the record and
// rides after every declared one, among its like in the experiments' names, which is the order
// the record keys its rows by. The order is put on here rather than on the whole body at the
// route, because here the rows are built once and sorted once; the record cannot put it on at
// all, since the file's order is the project's rather than the store's.
func assignmentViews(assignments map[string][]record.Assignment, key, tree string, place func(string) int) []api.AssignmentView {
	rows := assignments[key]
	if key != tree {
		rows = make([]record.Assignment, 0, len(assignments[tree])+len(rows))
		for _, row := range assignments[tree] {
			if row.Unit == string(config.ExperimentTree) {
				rows = append(rows, row)
			}
		}
		for _, row := range assignments[key] {
			if row.Unit == string(config.ExperimentIssue) {
				rows = append(rows, row)
			}
		}
	}
	if len(rows) == 0 {
		return nil
	}
	views := make([]api.AssignmentView, len(rows))
	for i, row := range rows {
		views[i] = api.AssignmentView{Experiment: row.Experiment, Variant: row.Variant, Mode: row.Mode, DrawnAt: row.DrawnAt}
	}
	slices.SortStableFunc(views, func(a, b api.AssignmentView) int {
		return cmp.Or(cmp.Compare(place(a.Experiment), place(b.Experiment)), cmp.Compare(a.Experiment, b.Experiment))
	})
	return views
}

// declaredPlaces is the place each experiment of declared holds in the order the project's file
// declares them (config.Project.ExperimentsDeclared: the `memory` block's experiment first, then
// the `experiments` key's own in the file's order), and the length of that list for every other
// name, so an experiment the file no longer names sorts after every declared one whatever the
// counts are.
func declaredPlaces(declared []config.Experiment) func(string) int {
	places := make(map[string]int, len(declared))
	for i, experiment := range declared {
		places[experiment.Name] = i
	}
	return func(name string) int {
		if i, found := places[name]; found {
			return i
		}
		return len(places)
	}
}

// memoryOf is the three memory fields of an issue's view, read off the answer of the experiment
// named `memory` among the issue's own (contract 49). They answer exactly what the four
// issues.memory_* columns answered before this release, so the measurement job that reads them
// needs no change, and the two classes the old columns told apart are told apart here by the
// instant:
//
//   - a row that records a draw (DrawnAt is set) fills all three, whatever arm it drew: the arm
//     `memory` is the group `memory`, and every other arm — the control `none` and the empty arm
//     an experiment at mode off draws, which the memory draw before this release wrote as the
//     word `none` — is the group `none`, with the row's own mode and instant beside it. A
//     deployment at mode off has answered `none` for every tree it opened, and goes on doing so;
//   - a row with no instant fills none of the three. That row is a hand-over settling a unit at
//     no arm for an experiment the tree it left held no row for, which is the one shape the old
//     column left empty (an empty memory_group with memory_drawn_at null, which the projection
//     before this release skipped): a tree that answered nothing must go on answering nothing,
//     or every reader counting exposed trees counts it anew.
//
// An issue no `memory` answer applies to carries none of the three either, which is what a tree
// first opened before the experiment existed carries. The assignment itself is untouched by
// this: `assignments` carries the settled row with its empty arm, which is how a reader tells a
// unit settled at no arm from a unit never drawn.
func memoryOf(views []api.AssignmentView) (string, string, *time.Time) {
	for _, view := range views {
		if view.Experiment != config.MemoryExperimentName || view.DrawnAt == nil {
			continue
		}
		// Every arm but the one named `memory` is the group `none` on the wire, the empty arm
		// a mode-off draw and a hand-over leave included: the wire enum has those two words
		// alone (packages/contracts/src/legion-api.ts, legionIssue).
		group := config.MemoryVariantNone
		if view.Variant == config.MemoryVariantMemory {
			group = view.Variant
		}
		return group, view.Mode, view.DrawnAt
	}
	return "", "", nil
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
