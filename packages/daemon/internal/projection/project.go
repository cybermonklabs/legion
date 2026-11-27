// Package projection joins durable workflow facts into the daemon state response.
package projection

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Inputs are the facts one projection is built from: the transaction it reads through, the
// store it reads with, and the project's own live and durable values as the caller holds them
// at that moment.
//
// They are one value rather than a run of positional arguments because a reader of a call site
// sees which fact each one is, and a fact added to the projection moves no call that does not
// hold it.
type Inputs struct {
	// Tx is the repeatable-read transaction the caller owns, so the record reads and the claims
	// share one snapshot. The projection does not commit it.
	Tx pgx.Tx
	// Store is what every record read goes through.
	Store record.Store
	// Project is the Dispatch project whose pending status writes, held lifecycle rows and open
	// intake give-ups the state lists.
	Project string
	// Cap is the deployment's admission cap (config.Config.AdmissionCap), which the admission
	// block carries beside the lists judged under it (blockers.JudgeAdmission), and which the slots
	// reading names beside the slots in use (record.SlotsReadingOf, api.Admission.Slots).
	Cap int
	// Steps are the design-gate steps the deployment gates (config.Gates.Steps). Each root's
	// view answers the steps its tree is gated on, which these steps and the root's gate rows
	// make (record.TreeSteps); the empty list gates none.
	Steps []record.DesignStep
	// Experiments are the project's declared experiments (config.Project.ExperimentsDeclared),
	// whose order each issue's `assignments` ride in (assignmentViews); the empty list declares
	// none, and every answer a record still holds then rides in the experiments' names.
	Experiments []config.Experiment
	// Claims are the supervision claims of this moment, which the claim views are built from.
	Claims []supervise.Claim
	// Stranded are the claims the daemon judges nothing will wake (workflow.Stranded), handed
	// in because that judgement reads the gated steps and the released wakes over the store and
	// this package may not read internal/workflow: it is what says an issue's work has stopped
	// on its worker rather than on its place in a queue (whyStates), and the caller reads it in
	// the same transaction, so the state is still one snapshot. Nil is nothing stranded.
	Stranded []supervise.Claim
	// Reports is each claim's latest capability report, by claim (LEGION-663): a claim with one
	// shows it whatever its incarnation — the view carries the reporting process's, for a reader
	// to compare with the locator's — and a claim with none shows no capabilities. Nil is no
	// reports.
	Reports map[claim.Token]capabilities.Report
	// Wakes is every wake the daemon's pacing still holds, both lanes as one list
	// (api.WakesWaitingOf): the state names them under `wakesWaiting`, and each claim view names
	// the one its own claim waits for, so the two come from one list and cannot disagree. Nil is
	// a daemon holding none, which is every moment outside a wave.
	Wakes []api.WakeWaiting
	// Now is the instant the held rows are read at (record.Store.HeldRows) and their waits are
	// measured from.
	Now time.Time
	// SlotRule is the slot rule the state is read under (record.SlotRule): the daemon's own, and
	// a replay's under the rule it replays, so the line, the held words and the answer of `why`
	// name each issue by the rule that placed it. The zero value is the daemon's own rule
	// (record.SlotRule.OrMain).
	SlotRule record.SlotRule
	// HolderClaims are the project's claims as a slot holder's state reads them, read by the
	// caller in this transaction through the statement the admission pass's tick reads them
	// through (record.Store.HolderClaims), which is the one that holds when a suspended or a
	// retired claim stopped. Nil is Claims as record.HolderClaimsOf reads them, which date a
	// stranded claim by its failure alone: what a caller holding only whole claims gives.
	HolderClaims []record.HolderClaim
}

// ObservedInputs are the facts the observed view's part of the state is built from (Observe): the
// fields of Inputs that it reads, each the same fact as the field of Inputs of the same name. They
// are a value of their own so that the parameter's type says what Observe reads, and the facts of
// the full state alone (Inputs' Experiments, Stranded, Reports, Wakes and HolderClaims) cannot
// reach it. It names no slot rule: the observed view is the daemon's alone, read under the
// daemon's own rule (Inputs.SlotRule's zero value).
type ObservedInputs struct {
	Tx      pgx.Tx
	Store   record.Store
	Project string
	Cap     int
	Steps   []record.DesignStep
	Claims  []supervise.Claim
	Now     time.Time
}

// Project joins durable issue facts to current supervision claims for GET /legion/v1/state. The
// role whose phase entry and given entry each issue's view carries is the role that works its
// current phase (phase.RoleFor), and the two handshake times beside them are that role's claim's,
// but for a tree's own root, whose times are its architect's claim's in every phase
// (supervise.Claim.Handshake).
func Project(ctx context.Context, in Inputs) (api.State, error) {
	issues, err := in.Store.Issues(ctx, in.Tx)
	if err != nil {
		return api.State{}, err
	}
	knownIssues := make(map[string]struct{}, len(issues))
	keys := make([]string, 0, len(issues))
	// lingering is every tree whose root lingers, or closed after it lingered: only a root carries a
	// linger deadline, and re-admission clears it.
	lingering := make(map[string]bool)
	for _, issue := range issues {
		knownIssues[issue.Key] = struct{}{}
		keys = append(keys, issue.Key)
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
	assignments, err := in.Store.Assignments(ctx, in.Tx, in.Project)
	if err != nil {
		return api.State{}, err
	}
	// The task of each issue that has plan rows at its generation, in one statement for the whole
	// state (record.Store.PlanTasksOf), joined to the issue records of project as the answers
	// above are. Such an issue counts its returns on its task (record.ReturnCounts), so its
	// implementer's view shows the task's counts; an issue with no plan rows shows its
	// implementer's phases row, where the counts lived before plans. A plan this release takes
	// holds one task (plan.Supported).
	planTasks, err := in.Store.PlanTasksOf(ctx, in.Tx, in.Project)
	if err != nil {
		return api.State{}, err
	}
	taskCounts := make(map[string]record.ReturnCounts, len(planTasks))
	for _, task := range planTasks {
		taskCounts[task.Issue] = task.Counts
	}
	place := declaredPlaces(in.Experiments)
	slots, err := in.Store.Slots(ctx, in.Tx)
	if err != nil {
		return api.State{}, err
	}
	pending, err := in.Store.PendingStatusWrites(ctx, in.Tx, in.Project)
	if err != nil {
		return api.State{}, err
	}
	held, err := in.Store.HeldRows(ctx, in.Tx, in.Project, "", in.Now)
	if err != nil {
		return api.State{}, err
	}
	giveUps, err := in.Store.OpenIntakeGiveUps(ctx, in.Tx, in.Project, stateNewestGiveUps)
	if err != nil {
		return api.State{}, err
	}
	// The open production checks, by issue: the check is read once for the whole state, since a
	// closed check shows nothing and the open ones are few.
	openChecks, err := in.Store.OpenProductionChecks(ctx, in.Tx)
	if err != nil {
		return api.State{}, err
	}
	checks := make(map[string]record.ProductionCheck, len(openChecks))
	for _, check := range openChecks {
		checks[check.Issue] = check
	}
	// The three facts each issue's view reads of its own rows — its phase rows, its pull request and
	// its design gates — are each read once for every issue the state lists, before the loop below,
	// which looks each issue up in the answers and makes no statement of its own. Each bulk read
	// answers an issue what its read by key answers it of what this state reads
	// (record.Store.AllPhases beside Phases, PullRequestsOf beside PullRequest, Gates beside
	// GatesOf), and leaves the rest out: the phase rows are read for the issues of project alone,
	// as the assignments above are, with only the columns this state reads of them
	// (record.Store.AllPhases), and PullRequestsOf loads none of what each pull request remembers
	// of its settled heads, since the state shows none of it and writes no record back. An issue
	// holding no row has no entry, which reads as the empty list, the nil pull request and the
	// empty gates.
	phases, err := in.Store.AllPhases(ctx, in.Tx, in.Project)
	if err != nil {
		return api.State{}, err
	}
	pullRequests, err := in.Store.PullRequestsOf(ctx, in.Tx, keys)
	if err != nil {
		return api.State{}, err
	}
	gatesOf, err := in.Store.Gates(ctx, in.Tx, keys)
	if err != nil {
		return api.State{}, err
	}

	// The wake each claim's own view names, read from the one list the state names under
	// `wakesWaiting`, so a claim view and that list report the same wake (api.ClaimWakes).
	claimWakes := api.ClaimWakes(in.Wakes)
	// The entry each claim was last queued a task of, by the issue and role the claim holds
	// (supervise.Claim.GivenEntry), and beside it the claim's handshake of the last entry it was
	// queued a task of (supervise.Claim.Handshake): the numbers and the two times an issue's view
	// shows beside the entry its phases row names. They are keyed by issue and role rather than by
	// claim token because the token on the phases row can be empty, for a role no claim was ever
	// created for, or stale, from a claim an earlier run retired, where the claim of this issue and
	// this role is the one the admission pass weighs (admit's resumeSlottedWorkers). One claim
	// stands per issue and role (claim.NewToken), so one entry holds both readings of it.
	entries := make(map[issueRole]roleEntry, len(in.Claims))
	for _, current := range in.Claims {
		entries[issueRole{issue: current.Issue, role: current.Role}] = roleEntry{given: current.GivenEntry, handshake: current.Handshake}
	}

	claimViews := make(map[claim.Token]api.ClaimView, len(in.Claims))
	for _, current := range in.Claims {
		if current.Locator != nil {
			if err := current.Locator.Validate(); err != nil {
				return api.State{}, fmt.Errorf("project claim %s: %w", current.Token, err)
			}
		}
		// Every field of the claim record is served or withheld by a decision, and the view that
		// serves it is built in one place (api.ClaimViewOf, api.ClaimRecordFields).
		view := api.ClaimViewOf(current)
		// Where the claim's update round stands. It is a reading over several of the claim's
		// update columns rather than one column's own value, which is why it is taken here and
		// not in the view's build (api.UpdateStateOf).
		view.UpdateState = api.UpdateStateOf(current)
		if wake, waits := claimWakes[current.Token]; waits {
			view.Wake = &wake
		}
		if report, reported := in.Reports[current.Token]; reported {
			view.Capabilities = api.CapabilityReportViewOf(report)
		}
		claimViews[current.Token] = view
	}

	// Which claim each issue's `architect` and each role of its `workers` show, the slot each issue
	// holds and what Dispatch observes of each issue's own record are decided once, for this view
	// and for the observed view of the state (Observe) alike, by the one definition both read
	// (listIssues), and put on both views' issues by one function (putListed): the two views cannot
	// disagree about what Dispatch observes.
	listed, unrecorded := listIssues(issues, phases, slots, in.Claims)

	// The wakes the pacing holds, as the state names them beside the claim views that name
	// their own: one list, set here, so the two readings come from one place.
	projected := api.State{Issues: make(map[string]api.Issue, len(issues)+len(unrecorded)), WakesWaiting: in.Wakes}
	for _, issue := range issues {
		entry := listed[issue.Key]
		// The issue's own Dispatch priority, null on the wire for none (api.Issue.Priority).
		priority := api.PriorityView(issue.Priority)
		view := api.Issue{
			Key:           issue.Key,
			Generation:    issue.Generation,
			Status:        issue.Status,
			Repo:          issue.Repo,
			DefaultBranch: issue.DefaultBranch,
			// When this root's linger runs out, nil for every issue that is not a lingering
			// root: it is what a reader of a held issue served no reason reads the suppression
			// of that reason by (api.Issue.LingerUntil).
			LingerUntil: issue.LingerUntil,
			// The parent chain a labelled root is held under, empty for every other record: the
			// hold on it is the ancestry hold, and nothing else on this wire names the chain.
			Ancestry: issue.Ancestry,
			Priority: &priority,
			Workers:  map[claim.Role]api.PhaseView{},
		}
		// The priority Dispatch resolves the issue to, absent where Dispatch sent none
		// (api.Issue.ResolvedPriority), and the issue it resolves it from, empty where it is the
		// issue's own or none (api.Issue.PriorityFrom): an issue Dispatch resolved no priority
		// for has no issue it comes from.
		if issue.Resolved.Known {
			resolved := api.PriorityView(issue.Resolved.Priority)
			view.ResolvedPriority, view.PriorityFrom = &resolved, issue.Resolved.From
		}
		// The correction of the issue's repository and a change still to be made: what tells a
		// reader of a record that names one repository that the issue worked another before, or
		// that Dispatch has named a third the daemon has not applied, and why. Each is one value
		// of the record, served as the wire's two flat keys of it.
		if corrected := issue.RepoCorrection; corrected != nil {
			at := corrected.At
			view.RepoBefore, view.RepoCorrectedAt = corrected.Before, &at
		}
		if change := issue.RepoChange; change != nil {
			view.RepoChangePending, view.RepoChangeReason = change.Pending, change.Reason
		}
		// The phase, the tree, the slot, the claims, and what the root waits on from a person,
		// which is present only while it is in awaiting_person.
		entry.putListed(&view, claimViews)
		// The implementer's counts of the moves back are its phases row's (putClaims), or its
		// issue's task's where the issue has plan rows (record.ReturnCounts). They ride with the
		// claim as the row's do, so they replace the row's only where a row showed the worker.
		if planned, ok := taskCounts[issue.Key]; ok {
			if shown, live := entry.workers[claim.RoleImplementer]; live && shown.row != nil {
				worker := view.Workers[claim.RoleImplementer]
				worker.Pass, worker.Rounds, worker.BackwardMoves = planned.Pass, planned.Rounds, planned.BackwardMoves
				view.Workers[claim.RoleImplementer] = worker
			}
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

		rows := phases[issue.Key]
		// The reviewer's own row, the single row of its role (the phases table is keyed by issue
		// and role): the round's decision is read from it, and so is what the round waits on from
		// GitHub (mergeWaitView). The zero row is an issue whose reviewer has never had one.
		reviewer := record.PhaseRowOf(rows, claim.RoleReviewer)
		// The entry the role of this issue's current phase stands on and the entry that role was
		// given: what a reader tells a role holding the work its row names from one the daemon has
		// still to send that work to (api.Issue.PhaseEntry and GivenEntry). A root carries them as
		// a child does, since phase.RoleFor answers the architect for admitted and accepting and a
		// phase role for each phase after; a phase no role works carries neither, where no row
		// would be read and a difference written for one would never close. No row of a role the
		// issue has moved past is read either, since that role keeps its entry and is sent nothing.
		// The entry is 0 where the rows hold none of that role (record.PhaseRowOf), a role no
		// transition has ever handed work on, which is the number admit's owedEntryRoles weighs
		// the same role's claim against, so the state shows what the admission pass weighs.
		role := phase.RoleFor(issue.Phase)
		if role != "" {
			view.PhaseEntry, view.GivenEntry = record.PhaseRowOf(rows, role).Entry, entries[issueRole{issue: issue.Key, role: role}].given
		}
		// Beside them the two times of a handshake: when a task of an entry was first queued to a
		// claim and when a turn of its worker first confirmed one (api.Issue.EntryQueuedAt and
		// EntryConfirmedAt), read from the claim's own record of it (supervise.Claim.Handshake),
		// which outlasts the turn that confirmed the task. A tree's own root carries its
		// architect's whatever phase it stands in: in awaiting_person, where no role works, and in
		// each phase a phase role works, where the two numbers are that role's and the times are
		// still the architect's. Any other issue carries the handshake of the role of its current
		// phase, and none in a phase no role works. Either is shown for the issue's current
		// generation alone (handshakeTimes), so a re-admitted root carries neither until its new
		// architect entry is queued.
		handshakeRole := role
		if claim.IsTreeRoot(issue.Key, issue.Tree) {
			handshakeRole = claim.RoleArchitect
		}
		if handshakeRole != "" {
			view.EntryQueuedAt, view.EntryConfirmedAt =
				handshakeTimes(entries[issueRole{issue: issue.Key, role: handshakeRole}].handshake, issue.Generation)
		}

		var pr *record.PullRequest
		if recorded, found := pullRequests[issue.Key]; found {
			pr = &recorded
		}
		if pr != nil {
			reviewDecision := ""
			if reviewer.Decision != nil {
				reviewDecision = reviewer.Decision.State
			}
			view.PullRequest = &api.PullRequestView{
				Repo:           pr.Repo,
				Number:         pr.Number,
				Head:           pr.HeadSHA,
				State:          string(pr.State),
				MergeState:     string(pr.MergeState),
				ReviewDecision: reviewDecision,
			}
		}
		view.MergeWait = mergeWaitView(issue, reviewer, pr)

		gates := gatesOf[issue.Key]
		gate := record.SpecGate(gates)
		if gate != nil {
			view.DesignGate = &api.GateView{
				ArtifactID:      gate.ArtifactID,
				CurrentVersion:  gate.LatestVersion,
				ApprovedVersion: gate.ApprovedVersion,
			}
		}
		// The tree's gate as this root stands in it, one entry for each step the tree is gated on
		// (record.TreeSteps): each step the deployment gates, and criteria where the root's
		// architect registered its Verification document beside a deployment that gates spec and
		// not criteria, since that row holds the tree as the spec's does. Only a root holds gate
		// rows — a child's work is held at its root's steps — so a child answers no entry at all
		// rather than one unregistered step for each the deployment gates. The entries are the
		// tree's steps and not the rows: a row of a step the deployment no longer gates is kept in
		// the record, where a deployment that gates it again reads its true state, and is in no
		// entry here, since it holds nothing (classify.DesignGatesOpen).
		if issue.Key == issue.Tree {
			view.GatedSteps = api.GatedStepViewsOf(record.TreeSteps(in.Steps, gates), gates)
		}
		if check, ok := checks[issue.Key]; ok {
			view.Check = checkView(check)
		}
		projected.Issues[issue.Key] = view
	}

	// A claim on an issue the workflow does not record — an operator's spawn, which is what Stage 2
	// makes and what the merge-queue holder is — is listed for the claim's sake, and says so
	// (listIssues): an empty phase is no value of the phase enum and an empty status is refused
	// too, so the whole document would be refused over a row that is a legitimate state.
	for _, key := range unrecorded {
		entry := listed[key]
		view := api.Issue{
			Key:     key,
			Status:  string(api.Unrecorded),
			Workers: map[claim.Role]api.PhaseView{},
		}
		entry.putListed(&view, claimViews)
		projected.Issues[key] = view
	}

	// Each issue's own reading of admission — its place in the waiting line and why it is not
	// working now — is read over this project's own records. The state lists every project's
	// issues, for the sake of a claim on an issue of another, while admission reads the project's
	// own records alone: another project's queued root, and its root a person has answered, would
	// otherwise read as work this daemon neither runs nor admits. They reach one read at all
	// because the issues table is one table shared by every daemon of the database (the issue
	// slots migration, "shared by every daemon of the database").
	own := make([]record.Issue, 0, len(issues))
	for _, issue := range issues {
		if issue.Project == in.Project {
			own = append(own, issue)
		}
	}
	// The admission block is the one judgement of it (blockers.JudgeAdmissionUnder, which builds it
	// with blockers.AdmissionOf), read in this transaction's snapshot like every other fact here,
	// under the rule the state is read under (Inputs.SlotRule, the daemon's own unless a replay
	// names another), so the line, the held words and the answer of `why` below place each issue by
	// the rule that placed it in the pass. The line it judged, the mirror rows it judged the links
	// from and the facts it read are what each issue's view below is read from, so a member's place
	// and every issue's `why` come from the judgement whose lists the block serves. The block
	// carries the P0 holders as well, the count admission's cap readers make (record.HoldersOf), so
	// the number the state gives is the number the pass admitted under.
	judged, err := blockers.JudgeAdmissionUnder(ctx, in.Tx, in.Store, in.Project, in.Steps, in.SlotRule, slots, in.Cap, in.Now)
	if err != nil {
		return api.State{}, err
	}
	projected.Admission = api.AdmissionOf(judged.Block)
	rows, facts, line, trees := judged.Rows, judged.Facts, judged.Line, judged.Live
	// Each issue the line names carries its own place in it (contract 58), counted from 1 at the
	// front, beside the length of the list it stands in. Nothing is filtered out: a member a
	// Dispatch link blocks stands in the line and takes its place like every other member, and
	// what holds it is `admission.blocked` beside this. Every issue of the line has a view here
	// already, so the place is set where the key is listed: the line is built from the project's
	// live records, each of them an issue of `own` the loop above gave a view to.
	//
	// Beside the place, each member carries the line's own fields for a program to read
	// (api.WaitingLineView): the priority that placed it, the part it waits in within that
	// priority's band and why, its place within that band and part and the length of that band's
	// part, whether it holds a slot, and what placed it — all of them read from the entry the
	// line holds beside the issue (record.LineEntry), which the loop below walks, so that the
	// fields a member carries and the key listed beside them come from one entry. The parts are
	// counted once over the whole line before the loop, per band, because this pass holds every
	// entry of it: a member's `partLength` is the count of the entries carrying its priority and
	// its part and nothing else, and the count of the other part of its band is what the sentence
	// below names.
	type bandPart struct {
		priority record.Priority
		part     record.LinePart
	}
	parts := make(map[bandPart]int)
	for _, entry := range line {
		parts[bandPart{entry.Priority, entry.Part}]++
	}
	banded := in.SlotRule.OrMain().PriorityFirst
	for i, entry := range line {
		key := entry.Issue.Key
		holds := entry.Holds
		placed := api.PriorityView(entry.Priority)
		standsIn := bandPart{entry.Priority, entry.Part}
		view := projected.Issues[key]
		view.WaitingLine = &api.WaitingLineView{
			Place: i + 1, Length: len(line),
			Part: entry.Part.String(), Kind: entry.Kind.String(),
			PartPlace: entry.PartPlace, PartLength: parts[standsIn],
			HoldsSlot: &holds,
			RootRank:  entry.RootRank,
			Priority:  &placed,
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
		// The sentence a person reads (waitingLineReasonUnder), rendered from the entry itself, the
		// length of the part of its band it stands in and the length of that band's other part,
		// which is what a member of the starting part waits behind. The render reads those and
		// nothing else but whether the rule the state is read under bands the line by priority,
		// so the words have one source and a test of them needs no database. Under a rule that
		// does not, every entry carries NoPriority (record.LineEntry.Priority), so the counts above
		// are of the whole part and the sentence names no priority.
		other := bandPart{entry.Priority, record.PartReturned}
		if entry.Part == record.PartReturned {
			other.part = record.PartStarting
		}
		view.WaitingLine.Reason = waitingLineReasonUnder(banded, entry, parts[standsIn], parts[other])
		projected.Issues[key] = view
	}
	// What the slots in use are spent on: each holder counted by what it is doing, the two
	// readings of started work that waits without a slot, and the holders that rank below the
	// first issue that waits for one, with the sentence `legion state` prints for them. It is read
	// by the one assembly the admission pass's tick reads it by (blockers.SlotsReading), over the
	// judgement, the line and the block's `blocked` and `held`, so the body, its line and the tick
	// notice give one reading of one project. Each holder's `slot` carries the state it was counted
	// under and when it entered it.
	holderClaims := in.HolderClaims
	if holderClaims == nil {
		holderClaims = record.HolderClaimsOf(in.Claims)
	}
	reading, holders := blockers.SlotsReading(blockers.SlotsInputs{
		Cap: in.Cap, Issues: judged.Issues, Slots: slots, Rows: rows, Facts: facts, Line: line,
		Blocked: projected.Admission.Blocked, Held: projected.Admission.Held, Claims: holderClaims, Stranded: in.Stranded,
	})
	projected.Admission.Slots = &reading
	for key, holder := range holders {
		view, shown := projected.Issues[key]
		if !shown || view.Slot == nil {
			continue
		}
		slot := *view.Slot
		slot.State, slot.Since = string(holder.State), holder.Since
		view.Slot = &slot
		projected.Issues[key] = view
	}
	// Each issue of the project carries its own Dispatch rank and its tree root's, the two keys
	// the line orders its starting part by and the reading places a holder against the first
	// issue that waits for a slot by, so the count of holders below that issue can be read off
	// the body. An issue of another project, which the state lists for a claim's sake, is no
	// issue the admission reading places and carries neither.
	ownByKey := make(map[string]record.Issue, len(own))
	for _, issue := range own {
		ownByKey[issue.Key] = issue
	}
	for _, issue := range own {
		view := projected.Issues[issue.Key]
		view.Rank = issue.Rank
		if root, recorded := ownByKey[issue.Tree]; recorded {
			view.RootRank = root.Rank
		}
		projected.Issues[issue.Key] = view
	}
	// Why each issue that is not working now is not working, one rendered answer on its own view.
	// It is decided here, after the line and `blocked` are both in hand, because three of the four
	// states are read off them, and from the two judgements this pass still holds apart — the
	// links and the children-first waits — which the published list has merged by now.
	//
	// It reads the held list above for the sentence of most issues it answers, and it is not
	// that list: the list says where every issue the daemon holds in the workflow stands and why
	// nothing of it runs, and this answers why an issue is not working now. The two part at an
	// issue whose work runs in accepting, retro or merging, which the list names and this answers for not at
	// all; where they part and why is written once, with the arm that parts them
	// (atWorkInSlotlessPhase).
	//
	// The running roots are the term that says an issue in accepting, retro or merging is at work: it holds no
	// slot since dispatch://INFRA-2560, so no list of the three places it and the answer turns
	// on whether its tree runs at all (whyStates, atWorkInSlotlessPhase). They are read from the
	// records and the facts this pass already holds, so the reading costs no query, and they
	// are read from the same facts as the live trees of the block (record.LiveRoots) — live is
	// wider than running, and a parked root's tree is neither.
	running := record.RunningRoots(own, facts)
	whyStates(&projected, own, running, in.Stranded, rows, facts)
	// Every root of this project's own records carries whether its tree runs (`treeRuns`), read
	// from the same running roots, so the state serves the daemon's own rule (record.TreeRuns)
	// rather than leaving a reader to rebuild it from the fields beside it, which decide it in no
	// case alone: a root a re-admission left waiting in the line runs nothing whatever its
	// architect's claim reads, and a root a person answered runs while it waits there for its
	// resume (dispatch://INFRA-3320, R4). Another project's root carries none, since this pass
	// reads the lifecycles of this daemon's project alone.
	for _, issue := range own {
		if !claim.IsTreeRoot(issue.Key, issue.Tree) {
			continue
		}
		_, runs := running[issue.Key]
		view := projected.Issues[issue.Key]
		view.TreeRuns = &runs
		projected.Issues[issue.Key] = view
	}
	// Every issue of a live tree carries that tree's quiet time (contract 111): `treeMovedAt`,
	// the newest movement the daemon has seen anywhere in the tree, and `quietHours`, how long
	// it has been still at the instant the rest of this state is measured at. The pair is the
	// tree's, so every issue of one tree carries the same two values — a child in accepting,
	// retro or merging, which holds no slot, and a done child recorded under the tree among
	// them, since what has stopped is the tree and not the issue a reader happens to hold. A
	// tree that is no longer live is measured for nobody: its root is in no `admission.trees`
	// entry, there is nothing of the deployment's left to wait on, and each of its issues carries
	// neither key.
	//
	// The measure is read once for the whole state (quietTrees), from the records and the claims
	// this pass already holds, and a live root is always one of its trees whatever its own
	// status says (record.QuietTrees), so the lookup below cannot miss.
	quiet := quietTrees(own, in.Claims, in.Now)
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
			WaitedSeconds: max(0, int64(in.Now.Sub(row.NextAt).Seconds())),
			WaitsOn:       row.WaitsOn,
		}
	}
	projected.IntakeGiveUps = intakeGiveUpsView(giveUps)
	return projected, nil
}

// Observe is the part of the state that the observed view of the state route reads
// (api.StateSource.ObservedState), read through in.Tx, the caller's repeatable-read transaction:
// all of that part but its daemon block and its settings, which the caller sets as it sets them on
// the full state (Project). The admission block is the full state's own: the same judgement
// (blockers.JudgeAdmission) over the same reads, unfiltered and in the same order, but for the
// slots reading (api.Admission.Slots), which only the full state serves, since it is for `legion
// state` alone and reads the holders' claims and the stranded judgement, which this read leaves
// out. The issues are every issue the full state lists, each with the members the definition of
// the full state's views decides (listIssues), put on by the function that puts them on the full
// state's issues (putListed), and each claim view as api.ClaimViewOf builds it. Every other member
// of an issue is empty. It reads the issues, the project's phase rows in one statement, the slots
// and what the admission judgement reads, and it makes no statement for any one issue.
func Observe(ctx context.Context, in ObservedInputs) (api.State, error) {
	issues, err := in.Store.Issues(ctx, in.Tx)
	if err != nil {
		return api.State{}, err
	}
	phases, err := in.Store.AllPhases(ctx, in.Tx, in.Project)
	if err != nil {
		return api.State{}, err
	}
	slots, err := in.Store.Slots(ctx, in.Tx)
	if err != nil {
		return api.State{}, err
	}
	judged, err := blockers.JudgeAdmission(ctx, in.Tx, in.Store, in.Project, in.Steps, slots, in.Cap, in.Now)
	if err != nil {
		return api.State{}, err
	}
	listed, _ := listIssues(issues, phases, slots, in.Claims)
	views := make(map[claim.Token]api.ClaimView, len(in.Claims))
	for _, current := range in.Claims {
		views[current.Token] = api.ClaimViewOf(current)
	}
	observed := api.State{Admission: api.AdmissionOf(judged.Block), Issues: make(map[string]api.Issue, len(listed))}
	for key, entry := range listed {
		view := api.Issue{Key: key}
		entry.putListed(&view, views)
		observed.Issues[key] = view
	}
	return observed, nil
}

// listedIssue is one issue the state lists, as both views of the state show what Dispatch observes
// of it (listIssues): its own record's observation, the slot it holds, and the claims its
// `architect` and its `workers` show.
type listedIssue struct {
	// observed is what Dispatch observes of the issue's own record (record.ObservationOf), and for
	// an issue the workflow does not record its phase alone, api.Unrecorded.
	observed record.IssueObservation
	// slot is the admission slot the issue holds, nil where it holds none. Only an issue the record
	// holds is given one.
	slot *record.Slot
	// architect is the claim the issue's `architect` shows, nil where it shows none.
	architect *supervise.Claim
	// workers is the claim each role of the issue's `workers` shows, by role; nil where it shows
	// none.
	workers map[claim.Role]shownWorker
}

// shownWorker is the claim one role of an issue's `workers` shows, and the phases row the claim was
// found through, which the full view reads the role's counts from; row is nil for a claim shown
// without one.
type shownWorker struct {
	claim *supervise.Claim
	row   *record.PhaseRow
}

// listIssues is the one definition of what both views of the state show of each issue Dispatch
// observes (Project, Observe): every issue of issues, by key, and every issue a claim names that
// issues does not hold, whose keys unrecorded lists in the order of their first claim. phases is
// each issue's phases rows (record.Store.AllPhases), slots every slot the store holds and claims the
// project's claims, all read in the caller's one snapshot.
//
// The rule, in the order it is applied:
//
//   - an issue's own record decides its phase, its tree and its wait (record.ObservationOf);
//   - an issue the record holds shows the slot it holds, and a slot of a key the record does not
//     hold is shown nowhere;
//   - a phases row whose claim token names a claim of claims shows that claim: the architect's row
//     as the issue's `architect`, which is no worker of its `workers`, and every other role's row as
//     that role's entry of `workers`. A row whose claim is not among claims shows nothing, so a
//     worker's view, and the counts its row carries, exist only while its claim does;
//   - then each claim, in claims' order, that no row showed: on its issue's `architect` where that
//     is empty, and on its role's entry of `workers` where that is empty. The daemon's own
//     controller (`controller: daemon`) holds a claim on no issue and is skipped: the state shows it
//     as the project's controller, never as an issue's worker, since an issue keyed "" refuses the
//     whole document at every client;
//   - a claim on an issue the record does not hold lists that issue at api.Unrecorded, with no
//     tree, no slot and no wait.
func listIssues(issues []record.Issue, phases map[string][]record.PhaseRow, slots []record.Slot, claims []supervise.Claim) (map[string]listedIssue, []string) {
	byToken := make(map[claim.Token]*supervise.Claim, len(claims))
	for i := range claims {
		byToken[claims[i].Token] = &claims[i]
	}
	listed := make(map[string]listedIssue, len(issues))
	for _, issue := range issues {
		entry := listedIssue{observed: record.ObservationOf(issue)}
		rows := phases[issue.Key]
		for i := range rows {
			row := &rows[i]
			shown, live := byToken[row.Claim]
			if !live {
				continue
			}
			if row.Role == claim.RoleArchitect {
				entry.architect = shown
				continue
			}
			entry.showWorker(row.Role, shownWorker{claim: shown, row: row})
		}
		listed[issue.Key] = entry
	}
	for i := range slots {
		entry, known := listed[slots[i].Issue]
		if !known {
			continue
		}
		entry.slot = &slots[i]
		listed[slots[i].Issue] = entry
	}
	var unrecorded []string
	for i := range claims {
		current := &claims[i]
		if claim.IsController(current.Role, current.Issue, current.Tree) {
			continue
		}
		entry, known := listed[current.Issue]
		if !known {
			unrecorded = append(unrecorded, current.Issue)
			entry = listedIssue{observed: record.IssueObservation{Phase: api.Unrecorded}}
		}
		if current.Role == claim.RoleArchitect {
			if entry.architect == nil {
				entry.architect = current
			}
		} else if _, shown := entry.workers[current.Role]; !shown {
			entry.showWorker(current.Role, shownWorker{claim: current})
		}
		listed[current.Issue] = entry
	}
	return listed, unrecorded
}

// showWorker makes worker the claim role shows in the issue's `workers`.
func (l *listedIssue) showWorker(role claim.Role, worker shownWorker) {
	if l.workers == nil {
		l.workers = make(map[claim.Role]shownWorker, 1)
	}
	l.workers[role] = worker
}

// putListed puts onto view, an issue of either view of the state (Project, Observe), what
// listIssues decided of it: the phase, the tree and the wait its own record decides, the slot it
// holds, and the claims its `architect` and its `workers` show (putClaims). It is the one place
// those members are built from the records, so each member the observed view reads (api's
// observedOf) is the full view's.
func (l listedIssue) putListed(view *api.Issue, views map[claim.Token]api.ClaimView) {
	view.Phase, view.WaitingOn = l.observed.Phase, l.observed.WaitingOn
	if l.observed.Tree != nil {
		view.Tree = *l.observed.Tree
	}
	if l.slot != nil {
		view.Slot = &api.SlotView{Index: l.slot.Index, AdmittedAt: l.slot.AdmittedAt}
	}
	l.putClaims(view, views)
}

// putClaims puts the claims l shows onto view, each as its claim view (views, by token): the
// architect as `architect`, and each worker with the counts of the phases row it was found through,
// its handoff, its pass, its rounds and its counts of the moves back. Those counts ride with the
// claim: a role whose claim has gone shows none of them, and the record keeps them for the role's
// next claim. The architect's own row is read all the same by Project, for the entry of a root at
// admitted (phase.RoleFor). view's `workers` is made here where it is nil and a worker is shown.
func (l listedIssue) putClaims(view *api.Issue, views map[claim.Token]api.ClaimView) {
	if l.architect != nil {
		architect := views[l.architect.Token]
		view.Architect = &architect
	}
	if len(l.workers) > 0 && view.Workers == nil {
		view.Workers = make(map[claim.Role]api.PhaseView, len(l.workers))
	}
	for role, worker := range l.workers {
		shown := api.PhaseView{Claim: views[worker.claim.Token]}
		if row := worker.row; row != nil {
			shown.HandoffCommit, shown.Pass, shown.Rounds, shown.BackwardMoves = row.HandoffCommit, row.Pass, row.Rounds, row.BackwardMoves
			if row.HandoffRecord != "" {
				shown.HandoffRecord = &api.HandoffRecordView{ID: row.HandoffRecord, Version: row.HandoffVersion}
			}
		}
		view.Workers[role] = shown
	}
}

// stateNewestGiveUps is how many open give-ups the state lists (api.IntakeGiveUpsView.Newest).
// The state is read by every agent's read_state and by `legion state`, and a burst of give-ups
// runs to thousands (2,293 terminated in a day on one daemon, 2026-11-18): fifty records of a few
// hundred bytes each keep the key to tens of kilobytes of a body that measured 4 MB, and the
// per-consumer counts and newest sequences say the rest, which a bulk replay or discard takes by
// consumer and sequence without the list naming each record.
const stateNewestGiveUps = 50

// stateGiveUpErrorRunes is how much of each listed give-up's last error the state carries, cut on
// a rune boundary: enough for the error's first sentence and its cause, which tell one failure
// from another. The record keeps the error up to storetext.FarSideErrorRunes and the give-up's log
// line the whole of it, so the state, which reaches every agent's context, carries no more than a
// reader needs to choose between a replay and a discard.
const stateGiveUpErrorRunes = 300

// intakeGiveUpsView is open as the state shows it: each consumer's count and newest sequence, and
// each listed record with its subject as the store keeps text (storetext.Text), since the record
// keeps the publisher's bytes and the state is JSON text a reader takes in, its error cut to
// stateGiveUpErrorRunes, and a null event id for a message that did not decode.
func intakeGiveUpsView(open record.IntakeGiveUps) api.IntakeGiveUpsView {
	view := api.IntakeGiveUpsView{
		Consumers: make([]api.IntakeGiveUpCountView, len(open.Consumers)),
		Newest:    make([]api.IntakeGiveUpView, len(open.Newest)),
	}
	for i, count := range open.Consumers {
		view.Consumers[i] = api.IntakeGiveUpCountView{Name: count.Consumer, Open: count.Open, NewestSeq: count.NewestSeq}
	}
	for i, failure := range open.Newest {
		listed := api.IntakeGiveUpView{
			ID: failure.ID, Consumer: failure.Consumer, Subject: storetext.Text(failure.Subject), StreamSeq: failure.StreamSeq,
			StoredAt: failure.StoredAt, Deliveries: failure.Deliveries, GaveUpAt: failure.GivenUpAt,
			Reason: failure.GiveUpReason, LastError: firstRunes(failure.LastError, stateGiveUpErrorRunes),
		}
		if failure.EventID != "" {
			listed.EventID = &failure.EventID
		}
		view.Newest[i] = listed
	}
	return view
}

// firstRunes is text's first n runes, cut on a rune boundary, or text whole when it is shorter.
func firstRunes(text string, n int) string {
	for offset := range text {
		if n == 0 {
			return text[:offset]
		}
		n--
	}
	return text
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
		views[i] = api.AssignmentView{Experiment: row.Experiment, Variant: row.Variant, Mode: row.Mode, DrawnAt: row.DrawnAt, Plan: row.Plan}
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

// issueRole is one issue and one of its roles, the pair a claim is held under (supervise.Claim's
// Issue and Role): the key of the lookup that answers what each role was last given.
type issueRole struct {
	issue string
	role  claim.Role
}

// roleEntry is what the claim of one issue and role says of its entry: the entry it was last
// queued a task of (supervise.Claim.GivenEntry), and its handshake of the last entry it was
// queued a task of (supervise.Claim.Handshake), the zero handshake where it was queued none.
type roleEntry struct {
	given     int64
	handshake supervise.EntryHandshake
}

// handshakeTimes is the two times of the handshake h as the view of an issue at generation
// carries them (api.Issue.EntryQueuedAt and EntryConfirmedAt). Both are nil where h names no
// entry, a claim that has been queued no task carrying one since the handshake was recorded,
// which a root of the release before this one is, and where h is of another run than the
// issue's, which a re-admitted root is until its new architect entry is queued: the handshake of
// an earlier run is no part of this one. The confirmed time alone is nil where no turn has
// confirmed a task of the entry, the architect that took nothing, which the view leaves absent
// rather than writing an instant that never happened.
func handshakeTimes(h supervise.EntryHandshake, generation uint64) (*time.Time, *time.Time) {
	if h.Entry == 0 || h.Generation != generation {
		return nil, nil
	}
	var queued, confirmed *time.Time
	if !h.QueuedAt.IsZero() {
		at := h.QueuedAt.UTC()
		queued = &at
	}
	if !h.ConfirmedAt.IsZero() {
		at := h.ConfirmedAt.UTC()
		confirmed = &at
	}
	return queued, confirmed
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

// mergeWaitView is what an issue's review round waits on from GitHub (api.MergeWaitView), and nil
// for a round that waits on nothing there. The rule is classify.RoundWaitsOnMergeState, the one
// the engine reads to decide whether this round waits on a read of GitHub (workflow's
// reviewRound), so the view carries the object for exactly the rounds the daemon is reading for.
// The daemon also reads the merge state where no round waits - a head moved, or a base pushed
// under an issue past implementing (classify.MergeStateReadOwed) - to find a conflict with the
// base, and those reads put no wait on the view. The rule holds only for an open pull request, so
// a record of a closed or merged one carries no wait here, and not for a head the record holds in
// a confirmed conflict with its base, whose approval ends the round whatever the state. It never
// holds for a nil one, so the reads below always have a pull request.
//
// It reads the record alone: the head the round waits on, the state the record holds for it, when
// that state was last read and when GitHub last moved the pull request, the fence's runs stamped
// with that head, and the marks the reviewer's decision carries once the architect and the
// reviewer have been told of a stuck round. It counts the head's own runs through the one
// predicate the daemon's readers share (classify.HeadRuns, which classify.ChecksConcludedOnHead
// answers from too), so a run stored before the fence carried its head counts for no head and the
// number here can never drift from the test the engine makes. The fence holds only runs a CI
// settlement brought, so a zero is a head whose CI is still running as much as one GitHub
// reported no check run on. The two times are absent when they are zero: a read time the record
// does not hold (record.PullRequest.StateReadAt says when that is) and a lifecycle observation
// that carried no clock are each a fact of their own, which a zero instant on the wire would read
// as a moment in 1 CE.
func mergeWaitView(issue record.Issue, reviewer record.PhaseRow, pr *record.PullRequest) *api.MergeWaitView {
	if !classify.RoundWaitsOnMergeState(issue, reviewer, pr) {
		return nil
	}
	view := &api.MergeWaitView{WaitsOn: pr.HeadSHA, MergeState: string(pr.MergeState)}
	if read := pr.StateReadAt; !read.IsZero() {
		view.StateReadAt = &read
	}
	if updated := pr.HeadUpdatedAt; !updated.IsZero() {
		view.HeadUpdatedAt = &updated
	}
	view.CheckRuns = len(classify.HeadRuns(*pr))
	if reviewer.Decision != nil {
		view.ToldHead, view.ToldState = reviewer.Decision.ToldStuckHead, string(reviewer.Decision.ToldStuckState)
	}
	return view
}
