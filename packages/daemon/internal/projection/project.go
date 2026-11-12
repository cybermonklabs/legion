// Package projection joins durable workflow facts into the daemon state response.
package projection

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
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
// claim with none shows no capabilities. Nil is no reports. now is the instant the held rows are
// read at (record.Store.HeldRows) and their waits are measured from.
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

	claimViews := make(map[claim.Token]api.ClaimView, len(claims))
	for _, current := range claims {
		if current.Locator != nil {
			if err := current.Locator.Validate(); err != nil {
				return api.State{}, fmt.Errorf("project claim %s: %w", current.Token, err)
			}
		}
		view := api.ClaimView{
			Session:        current.Session,
			State:          string(current.State),
			Locator:        current.Locator,
			PluginContract: current.PluginContract,
			UpdateRound:    current.UpdateRound,
			UpdateHead:     current.UpdateHead,
			// What the daemon last saw this claim's agent do, which the stall backstop and an
			// operator read alike; nil for a claim whose agent has not stepped (api.StepViewOf).
			Step: api.StepViewOf(current.Step),
		}
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
			Workers:       map[claim.Role]api.PhaseView{},
		}
		// The tree's memory draw, which every issue of the tree is shown, a done one included: it
		// is what the issue's roles ran with, and nothing redraws it.
		if draw, drawn := memoryByTree[issue.Tree]; drawn {
			view.MemoryGroup, view.MemoryDrawMode, view.MemoryDrawnAt = draw.group, draw.mode, draw.at
		}
		// An escalation waits on the controller only while its tree runs. A held issue of a tree
		// that lingers or closed keeps the reason on its record, and shows it again once the tree is
		// re-admitted, which leaves the issue held.
		if issue.Hold != nil && !lingering[issue.Tree] {
			view.HoldReason = string(issue.Hold.Reason)
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
				ChecksVerdict:  classify.HeadVerdict(*pr),
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
	waiting := record.WaitingIssues(line)
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
	projected.Admission.Waiting = make([]string, len(waiting))
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
	// entry the line holds beside the issue (record.LineEntry), which `waiting` is the issues
	// of in the same order, so the entry at an issue's index is that issue's own. The two parts
	// are counted once over the whole line before the loop, because this pass holds every entry
	// of it: a member's `partLength` is the count of the entries carrying its part and nothing
	// else, and the other part's count is what the sentence below names.
	parts := make(map[record.LinePart]int, 2)
	for _, entry := range line {
		parts[entry.Part]++
	}
	for i, issue := range waiting {
		projected.Admission.Waiting[i] = issue.Key
		entry := line[i]
		holds := entry.Holds
		view := projected.Issues[issue.Key]
		view.WaitingLine = &api.WaitingLineView{
			Place: i + 1, Length: len(waiting),
			Part: entry.Part.String(), Kind: entry.Kind.String(),
			PartPlace: entry.PartPlace, PartLength: parts[entry.Part],
			HoldsSlot: &holds,
			RootRank:  entry.RootRank, OwnRank: entry.OwnRank, RootKey: entry.RootKey,
		}
		// The moment the work came back is on a member of the returned part alone, and even
		// there the record may not know it — a gate approved by a registration that read no
		// event carries no moment (record.TreeGate.OpenedAt) — so the wire carries `since`
		// where the entry has one and nothing where it has none.
		if !entry.Since.IsZero() {
			since := entry.Since
			view.WaitingLine.Since = &since
		}
		// The sentence a person reads (waitingLineReason), rendered from the view just filled
		// and the length of the part this member does not stand in, which is what a member of
		// the starting part waits behind. The render reads those and nothing else, so the words
		// have one source and a test of them needs no database.
		other := parts[record.PartReturned]
		if entry.Part == record.PartReturned {
			other = parts[record.PartStarting]
		}
		view.WaitingLine.Reason = waitingLineReason(*view.WaitingLine, other)
		projected.Issues[issue.Key] = view
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

// memoryDraw is one tree's memory draw as its root's record holds it (record.Issue.MemoryGroup,
// MemoryDrawMode, MemoryDrawnAt): the group the draw gave the tree, the project's mode it was
// drawn under, and when.
type memoryDraw struct {
	group string
	mode  string
	at    *time.Time
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

// waitingLineReason is the sentence a person reads for one member of the waiting line
// (api.WaitingLineView.Reason): what put it there and when, where it stands in its own part of
// the line, and what that place means for the slot it waits for. It reads the view beside it and
// otherPart, the length of the part this member does not stand in, and nothing else — no record,
// no second count — so the sentence has one source and every number in it is the member's own.
//
// The last clause is the holder's wherever the member already holds one of the cap's slots
// (api.WaitingLineView.HoldsSlot), in every kind of both parts and never read off the kind: a
// member of any kind can hold a slot and any kind can stand in the line without one, and telling
// a holder that it queues for a free slot is the defect this sentence exists to stop. A member of
// the returned part whose record knows no moment (record.LineEntry.Since) names none, and a
// member of the starting part with nothing in the other part says nothing about it: both clauses
// end at the words that are true.
//
// A view carrying no word of the line's vocabulary (record.LinePart.String, record.LineKind.String)
// renders nothing, and its member is served no reason rather than a sentence about a part or a
// kind this release does not know.
func waitingLineReason(view api.WaitingLineView, otherPart int) string {
	switch view.Part {
	case record.PartReturned.String():
		came := ""
		switch view.Kind {
		case record.LineReturning.String():
			came = "Its work came back"
		case record.LineAnswered.String():
			came = "A person answered it, and its work came back"
		case record.LineFirstPhase.String():
			came = "Its design gate opened"
		default:
			return ""
		}
		if view.Since != nil {
			came += " at " + view.Since.UTC().Format(time.RFC3339)
		}
		place := fmt.Sprintf("It is %s of %s whose work came back", englishOrdinal(view.PartPlace), issuesCounted(view.PartLength))
		if view.HoldsSlot != nil && *view.HoldsSlot {
			return came + ". " + place + "." + holdsItsSlot
		}
		takes := "they take"
		if view.PartLength == 1 {
			takes = "it takes"
		}
		return fmt.Sprintf("%s. %s, and %s the free slots first.", came, place, takes)
	case record.PartStarting.String():
		rank := ""
		switch view.Kind {
		case record.LineQueuedRoot.String():
			rank = "its own Dispatch rank"
		case record.LineFirstPhase.String():
			rank = "the Dispatch rank of its tree root " + view.RootKey
		default:
			return ""
		}
		started := "have"
		if view.PartLength == 1 {
			started = "has"
		}
		place := fmt.Sprintf("It is %s of %s that %s not started, by %s",
			englishOrdinal(view.PartPlace), issuesCounted(view.PartLength), started, rank)
		switch {
		case view.HoldsSlot != nil && *view.HoldsSlot:
			return "It has not started. " + place + "." + holdsItsSlot
		case otherPart == 0:
			return "It has not started. " + place + "."
		}
		stand := "stand"
		if otherPart == 1 {
			stand = "stands"
		}
		return fmt.Sprintf("It has not started. %s, and %s whose work came back %s ahead of all of them.",
			place, issuesCounted(otherPart), stand)
	}
	return ""
}

// holdsItsSlot is the last clause of every member that already holds one of the cap's slots: it
// waits for no slot to free, so the clause that tells the rest of the line what it queues for is
// this truth instead (waitingLineReason). It opens on a space because it follows the full stop
// the place clause ends on.
const holdsItsSlot = " It already holds its admission slot, so it resumes at the next pass whatever the cap says."

// englishOrdinal is n as a person writes a place: 1st, 2nd, 3rd and `th` for the rest, with 11,
// 12 and 13 taking `th` whatever their last digit says — so 11th, 12th and 13th, and 21st, 22nd,
// 23rd, 101st and 111th. The places of the line count from 1 (record.LineEntry.PartPlace), so no
// other number reaches it.
func englishOrdinal(n int) string {
	suffix := "th"
	if tens := n % 100; tens < 11 || tens > 13 {
		switch n % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return fmt.Sprintf("%d%s", n, suffix)
}

// issuesCounted is a count of issues with the noun a person reads it with: `1 issue`, and
// `7 issues` for every other count. The verb that follows is the caller's, since the clauses
// that carry this count take different ones (waitingLineReason).
func issuesCounted(n int) string {
	if n == 1 {
		return "1 issue"
	}
	return fmt.Sprintf("%d issues", n)
}

// whyStates fills the one rendered answer an issue that is not working now carries on its own
// view (api.Issue.Why): the state word, and the sentence that says why, from whichever fact
// holds the issue. state is the projection so far, whose `admission.active`,
// `admission.waiting` and `admission.blocked` are all filled and whose members of the line
// carry the sentence already rendered for them (api.WaitingLineView.Reason); own are this
// project's own records; stranded are the claims the daemon judges nothing will wake, handed to
// Project because this package may not read internal/workflow; rows is the Dispatch mirror this
// pass judged the links from and childWaits the children-first judgement it made
// (record.ChildWaitsOf).
//
// The four states are read in one order, so no issue carries two answers: claim_stopped, then
// waiting, then blocked, then unlisted. claim_stopped is first because its fact is about the
// issue's worker rather than its place in a queue, and an issue at work whose worker stopped is
// the commonest question a reader brings. An issue that matches none of the four carries no
// answer at all: an issue of `admission.active` whose worker is running, and an issue at
// phase.Held, which holds a slot and so stands in that same list.
//
// The scope is this project's own records that the record holds in the workflow
// (record.InWorkflow). An issue the record does not hold there — done, backlog, icebox,
// triage — carries no answer, an issue of another project carries none, since admission is
// judged over this project's records alone, and an issue the workflow does not record at all,
// an operator's claim, is in no record of own and so carries none either.
//
// The links and the children-first waits are read here from the two judgements this pass holds
// apart, and never from state.Admission.Blocked: that list merges both into one
// record.BlockedIssue apiece (blockers.Blocked), so a reader of it cannot tell a Dispatch link
// from an open child, and the three blocked fills need them apart.
func whyStates(state *api.State, own []record.Issue, stranded []supervise.Claim, rows map[string]blockers.Row, childWaits map[string]record.ChildWait) {
	// The stranded claim each issue's answer names. The set reports two kinds of claim and no
	// others (workflow.Stranded) — the worker of the issue's phase, and the tree's architect —
	// so where one issue has both, the worker is the one named: its stop is the one that holds
	// this issue's own phase, and the architect's is the whole tree's.
	stoppedAt := make(map[string]supervise.Claim, len(stranded))
	for _, stopped := range stranded {
		if named, known := stoppedAt[stopped.Issue]; known && named.Role != claim.RoleArchitect {
			continue
		}
		stoppedAt[stopped.Issue] = stopped
	}
	// The three lists that place an issue: the line and the blocked list each answer a state of
	// their own, read from their own set below, and `active` is read for `unlisted` alone, which
	// is the issue in none of the three. `admission.trees` is not among them, because it names
	// the roots of live trees rather than placing an issue, so a root stands here on the same
	// terms as a child.
	inLine := make(map[string]struct{}, len(state.Admission.Waiting))
	for _, key := range state.Admission.Waiting {
		inLine[key] = struct{}{}
	}
	// What the published list says each blocked issue waits on: the fallback of the link fill,
	// for an issue the link half names nothing for — a root of the line whose wait
	// blockers.LineWaitsOn judged — so that every blocked issue carries an answer.
	blocked := make(map[string][]string, len(state.Admission.Blocked))
	for _, row := range state.Admission.Blocked {
		blocked[row.Issue] = row.BlockedBy
	}
	// The issues holding a slot, which `unlisted` is the complement of together with the two
	// sets above. Only those two sets' own arms read them, so this holds the slots alone: the
	// arm that reads it is reached only by an issue neither of them names.
	active := make(map[string]struct{}, len(state.Admission.Active))
	for _, key := range state.Admission.Active {
		active[key] = struct{}{}
	}
	// What the link half says an issue waits on, which the waiting clause and the link fill both
	// name: the links this pass judged (blockers.Holds), and the published list where that half
	// names nothing for an issue the list does name. It is read in the two arms that name it and
	// in no other, since judging the links of an issue whose answer is its stopped claim, or of
	// one no list places, is work no sentence reads.
	waitsOn := func(issue record.Issue) []string {
		if links := blockers.Holds(rows, issue); len(links) > 0 {
			return links
		}
		return blocked[issue.Key]
	}
	// Each cycle the children-first judgement named, by the child whose own blocked_by link
	// holds it under an ancestor (record.OpenChild.Cycle): the one fact of the three blocked
	// fills a reader can get nowhere else.
	cycles := make(map[string]string, len(childWaits))
	for _, wait := range childWaits {
		for _, child := range wait.Open {
			if child.Cycle != "" {
				cycles[child.Key] = child.Cycle
			}
		}
	}
	for _, issue := range own {
		view, projected := state.Issues[issue.Key]
		if !projected || !record.InWorkflow(issue) {
			continue
		}
		stopped, stoppedClaim := stoppedAt[issue.Key]
		_, waiting := inLine[issue.Key]
		_, held := blocked[issue.Key]
		var why *api.WhyView
		switch {
		case stoppedClaim:
			why = &api.WhyView{State: api.WhyClaimStopped, Reason: claimStoppedReason(issue, stopped)}
		case waiting:
			// The member's own sentence, and one more clause where a link holds it as well as
			// the line. The clause is added here and never inside the line's own `reason`: the
			// two are separate fields, and only `why` carries it.
			reason := ""
			if view.WaitingLine != nil {
				reason = view.WaitingLine.Reason
			}
			if clause := linkReason(waitsOn(issue)); held && clause != "" {
				if reason != "" {
					reason += " "
				}
				reason += clause
			}
			why = &api.WhyView{State: api.WhyWaiting, Reason: reason}
		case held:
			why = &api.WhyView{State: api.WhyBlocked, Reason: blockedReason(cycles[issue.Key], childWaits[issue.Key], waitsOn(issue))}
		default:
			if _, placed := active[issue.Key]; placed {
				continue
			}
			why = &api.WhyView{State: api.WhyUnlisted, Reason: unlistedReason(issue)}
		}
		// An issue whose sentence renders nothing is served no `why` at all, rather than a state
		// word with no reason beside it: it is the rule the line's own sentence is served by
		// (waitingLineReason), and the two answers a reader reads together stay together.
		if why.Reason == "" {
			continue
		}
		view.Why = why
		state.Issues[issue.Key] = view
	}
}

// claimStoppedReason is the sentence of an issue a stranded claim names (api.WhyClaimStopped):
// where the issue's work stands, which claim stopped and the state it stopped in, and that
// nothing will take it up again. The two fills are the two kinds of claim the stranded set
// reports — the worker of the issue's phase, named by its role, and the tree's architect, whose
// claim is the tree's rather than this issue's — and each names the state the set carries with
// the claim rather than one read of its own: the set is the daemon's judgement, and a claim at
// suspended, failed or retired that is not in it is waiting for a start the daemon has queued.
func claimStoppedReason(issue record.Issue, stopped supervise.Claim) string {
	if stopped.Role == claim.RoleArchitect {
		return fmt.Sprintf("Its phase is %s and its tree's architect claim is stranded at %s, so nothing will wake it.",
			issue.Phase, stopped.State)
	}
	return fmt.Sprintf("Its phase is %s and the %s's claim is stranded at %s, so nothing will wake it.",
		issue.Phase, stopped.Role, stopped.State)
}

// blockedReason is the sentence of an issue something holds where it stands (api.WhyBlocked),
// from the three judgements the state's `blocked` has merged by the time it is published: the
// cycle a child's own blocked_by link makes with an ancestor, the open children below an issue
// whose first phase waits for them, and the Dispatch links an issue waits on.
//
// They are read in that order, most specific first. A cycle is a link too, so reading the link
// fill first would hide the one fact a reader cannot get anywhere else — that the issue it
// waits on is the issue waiting for it, and that only a person taking the link off ends it. And
// the children-first wait is about the issues below the issue, which the link fill would not
// name at all.
func blockedReason(cycle string, wait record.ChildWait, waitsOn []string) string {
	if cycle != "" {
		return fmt.Sprintf("A blocked_by link holds it under %s, which stands above it in its own tree.", cycle)
	}
	if len(wait.Open) > 0 {
		open := make([]string, 0, len(wait.Open))
		for _, child := range wait.Open {
			open = append(open, child.Key)
		}
		return fmt.Sprintf("Its own first phase waits for the open issues below it: %s.", englishIssues(open))
	}
	return linkReason(waitsOn)
}

// linkReason is the clause that names the Dispatch issues an issue waits on that are not done:
// the whole sentence of the link fill of `blocked`, and the last clause of a member of the line
// a link holds as well (whyStates). The relative clause agrees with the list, so one issue reads
// `which is not done`. An empty list renders nothing: an issue whose blockers this pass cannot
// name is served no sentence naming none.
func linkReason(waitsOn []string) string {
	switch len(waitsOn) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf("It waits on %s, which is not done.", waitsOn[0])
	}
	return fmt.Sprintf("It waits on %s, which are not done.", englishIssues(waitsOn))
}

// unlistedReason is the sentence of an issue the record holds in the workflow that stands in
// none of the three lists that place one (api.WhyUnlisted). The word is `unlisted` and not
// `held`, because `held` is a phase of the workflow, whose issues hold a slot and stand in
// `admission.active`.
//
// Two fills, which are the two this record can answer: an issue waiting for its merge, which
// waits for that and nothing else, and every other, which this build records no cause for. The
// approved spec writes a third, for an issue whose recorded cause the record carries, and it
// follows the field that records one: dispatch://INFRA-2964 writes it, and has not merged.
func unlistedReason(issue record.Issue) string {
	if issue.Phase == phase.AwaitingMerge {
		return "It waits for its merge, and for nothing else."
	}
	return "The daemon holds it in the workflow, and it stands in neither the issues at work nor the waiting line, " +
		"so no slot has been offered to it. This build records no cause."
}

// englishIssues is a list of issue keys as a person writes it: `INFRA-123` alone,
// `INFRA-123 and INFRA-456` for two, and `INFRA-1, INFRA-2 and INFRA-3` for three or more, in
// the order the judgement that named them gave them. It is the one list render the sentences of
// `why` share (whyStates), so no two of them punctuate a list differently.
func englishIssues(keys []string) string {
	if len(keys) < 2 {
		return strings.Join(keys, "")
	}
	return strings.Join(keys[:len(keys)-1], ", ") + " and " + keys[len(keys)-1]
}
