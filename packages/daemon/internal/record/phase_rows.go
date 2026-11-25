package record

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
)

// PhaseRowOf is role's row among rows now, read with no walk: the row with the highest entry, and
// the zero row where the record holds no row of that role: a role no transition has ever handed
// work on, which stands on no entry, names no handoff and holds no claim. A role that stands at two
// steps of a task has a row for each, and a step's start raises its entry above every row of its
// role (workflow's enter), so the row with the highest entry is the row of the step the role works
// now, or worked last, which is the row PhaseRowAt gives where the task stands; an issue with no
// plan rows has one row for each role. Rows level on one entry, which only a release before that
// rule wrote, are told apart by their step, the later first (rowAbove). Every reader of one role's
// row among an issue's that holds no walk picks it here — the state the projection writes
// (projection.Project), the entry the admission pass weighs a claim against (admit's
// resumeSlottedWorkers), the outbox's fence on a start whose entry its role has left
// (internal/daemon's outbox), the claim of a role's worker (WorkerClaim) — and the statements that
// pick it among many issues' rows (Store.PhaseEntries, the implementer's commit of
// Store.HandedOffWithoutOpenOrMergedPullRequest), the fold of a new generation
// (Store.ClearGeneration) and the way back of the step key keep the same order, so that rule is
// stated once and read the same way by all of them.
func PhaseRowOf(rows []PhaseRow, role claim.Role) PhaseRow {
	var now PhaseRow
	found := false
	for _, row := range rows {
		if row.Role == role && (!found || rowAbove(row, now)) {
			now, found = row, true
		}
	}
	return now
}

// rowAbove says whether row stands above than among one role's rows: a higher entry, or the same
// entry and a later step, by step task and then step place. It is the order `entry desc,
// step_task desc, step_place desc` of the statements that pick a role's row now (PhaseRowOf).
func rowAbove(row, than PhaseRow) bool {
	switch {
	case row.Entry != than.Entry:
		return row.Entry > than.Entry
	case row.StepTask != than.StepTask:
		return row.StepTask > than.StepTask
	}
	return row.StepPlace > than.StepPlace
}

// OtherStepsEntry is the highest entry a row of row's role holds other than row: the row of
// another step of that role, or the role's retro row (place 0), where its agent does the retro as
// that role, which stands once its retro is entered; and 0 where the role has no other row: a
// role at one step with no retro row, and every role of an issue with no plan rows. A step's
// start raises its entry above every row of its role (workflow's enter), so an entry at or below
// this one is of another step's run or the retro's, or of an earlier run of row's own step that
// the task has since left for one of them.
// The entry fence of a completion reads it (intake.EntryFence), so the same head is never taken as
// the same work across two runs that ask for different work.
func OtherStepsEntry(rows []PhaseRow, row PhaseRow) int64 {
	var entry int64
	for _, other := range rows {
		if other.Role == row.Role && (other.StepTask != row.StepTask || other.StepPlace != row.StepPlace) && other.Entry > entry {
			entry = other.Entry
		}
	}
	return entry
}

// ServedClaim is the claim role's row on entry names among rows: the agent of the run a completion
// serving entry reports, since no two rows of a role stand on one entry (workflow's enter), and ""
// where no row of role stands on entry, a run its row was raised past since. The entry fence reads
// it beside the claim of the row the task stands at, so a turn of another agent of the role is told
// apart from a turn of the same agent's other run (intake.EntryFence).
func ServedClaim(rows []PhaseRow, role claim.Role, entry int64) claim.Token {
	row, _ := rowOnEntry(rows, role, entry)
	return row.Claim
}

// rowOnEntry is role's row among rows that stands on entry, and whether one does: none for entry
// 0, and at most one otherwise, since no two rows of a role stand on one entry (workflow's enter).
// The run a completion serving entry reports is read through it (ServedClaim, CompletionRow).
func rowOnEntry(rows []PhaseRow, role claim.Role, entry int64) (PhaseRow, bool) {
	if entry > 0 {
		for _, row := range rows {
			if row.Role == role && row.Entry == entry {
				return row, true
			}
		}
	}
	return PhaseRow{}, false
}

// AgentsReviewedHead is the commit agent's newest deciding review of role on task read, among rows:
// the commit (ReviewedHead) of a row that each of agent's deciding reviews writes it onto, every
// row of role on task other than the step at place that names agent's claim (AgentsOtherRow,
// workflow's shareReviewedHead), and "" where no such row holds one, so agent reads the whole
// change. Those rows hold one commit, the newest, so whichever is read holds it. A run of agent's
// step at place is named it as its own where workflow's stepRunRow says so. A row is agent's only
// while it names agent's claim, and only a run writes another claim on it (workflow's enter): where
// another agent's run has taken every row of role on task that agent worked, agent is named "",
// though a review of its own read a commit before. A later version of the plan that gives agent's
// step to another agent, with no run of it, takes no row. agent "" (an issue whose key names no
// claims) has no row the reviews write onto, and is named "".
func AgentsReviewedHead(rows []PhaseRow, role claim.Role, task string, place int, agent claim.Token) string {
	step := PhaseRow{Role: role, StepTask: task, StepPlace: place, Claim: agent}
	for _, other := range rows {
		if AgentsOtherRow(step, other) && other.ReviewedHead != "" {
			return other.ReviewedHead
		}
	}
	return ""
}

// AnotherAgentsReview is the commit another agent's review on row's task read, which the task of a
// review step names beside its own `none` (reviewround.AnotherAgentsSentence), among rows: where row
// holds no commit of its own (ReviewedHead ""), the commit held by the row of row's role on row's
// task at the nearest earlier place, from 1, whose agent (its Claim) is another than row's and whose
// review read one. "" for every other row: one that holds a commit, one that names no agent of its
// own (NamesItsAgent), and one with no such row before it. A row holds no commit while its agent
// has no deciding review of its own on the task (AgentsReviewedHead), so such an agent reads the
// whole change and is told what another agent's review read, so it can see what changed since.
func AnotherAgentsReview(rows []PhaseRow, row PhaseRow) string {
	if row.ReviewedHead != "" || !NamesItsAgent(row) {
		return ""
	}
	commit, nearest := "", 0
	for _, other := range rows {
		if other.Role == row.Role && other.StepTask == row.StepTask && other.StepPlace > nearest && other.StepPlace < row.StepPlace &&
			NamesItsAgent(other) && other.Claim != row.Claim && other.ReviewedHead != "" {
			commit, nearest = other.ReviewedHead, other.StepPlace
		}
	}
	return commit
}

// AgentsOtherRow says whether other is a row of another step of row's role on row's task that
// row's agent ran: it names row's claim. A deciding review's commit is written onto row and onto
// each such row (workflow's shareReviewedHead), and read from them (AgentsReviewedHead). False for
// every other where row names no agent of its own (NamesItsAgent).
func AgentsOtherRow(row, other PhaseRow) bool {
	return NamesItsAgent(row) && other.Role == row.Role && other.StepTask == row.StepTask &&
		other.StepPlace != row.StepPlace && other.Claim == row.Claim
}

// AgentsRowOf is role's row among rows that agent ran last, the row whose commit and entry a task
// queued for agent names (workflow's queuedRow): the role's row now (PhaseRowOf) where that row is
// agent's, names no agent of its own (NamesItsAgent), or agent is "" (an issue whose key names no
// claims). Where the row now is the step of another agent of the role, it is the highest of agent's
// own rows of role that run a step (rowAbove), and the zero row of role where agent ran none, which
// names no commit and no entry. So a wake of an earlier step's agent is never named another agent's
// commit as its own.
func AgentsRowOf(rows []PhaseRow, role claim.Role, agent claim.Token) PhaseRow {
	now := PhaseRowOf(rows, role)
	if agent == "" || !NamesItsAgent(now) || now.Claim == agent {
		return now
	}
	own, found := PhaseRow{Role: role}, false
	for _, row := range rows {
		if row.Role == role && NamesItsAgent(row) && row.Claim == agent && (!found || rowAbove(row, own)) {
			own, found = row, true
		}
	}
	return own
}

// StartLeft says whether a start of role's phase entry entry is of work the role has left, read
// from an issue's phases rows, and answers the entry of the row it weighed the start against: the
// role's row now (PhaseRowOf), the row of the step the task stands at, or stood at last. A role's
// entries rise together, each step's above every row of its role (workflow's enter), so the row
// now holds the role's highest entry and a start of another step of the role, another agent's
// included, or of an earlier run of this one, is below it. The outbox asks this before it runs a
// phase worker's start, and again under the claim's lock before the start suspends or re-roles it.
func StartLeft(rows []PhaseRow, role claim.Role, entry int64) (int64, bool) {
	now := PhaseRowOf(rows, role)
	return now.Entry, entry < now.Entry
}

// PhaseRowAt is role's row among rows where the task of an issue with plan rows stands: task, at
// at, whose steps are steps. A role can stand at two steps of a task, of one agent or of two (a
// second agent of the role, `implementer-2`), and each run of a step keeps a row of its own
// (StepTask, StepPlace), so the row read is the row of
// one of the role's places in the task: its step at at where the step there is the role's; else
// its latest place before at, the retro, the role its agent runs as (PlaceWorker), and the merge, the
// merger's, standing after every step; else its first place after at, so a read or a write before
// the role's first step reaches the row of that step, as the one row of a role did; and place 0
// where the task gives the role no place, so every write of such a role lands on one row. Where
// the row of that place does not stand yet, the role's row of no step (StepTask "", a row a new
// generation cleared or the release before wrote) is the role's row, as it was before a step was
// written on it, and the step's entry moves it to the step (workflow's enter). Where neither
// stands, it is the zero row stamped with role, task and that place, which a caller that writes it
// back writes as that row. For an issue with no plan rows (task ""), each role has one row,
// PhaseRowOf's.
//
// It is the one reading of a role's row where a walk is known: the engine's (workflow's phaseRow,
// enter and reportsBefore) and the completion route's, which knows the place it was handed. A
// reader with no walk reads PhaseRowOf, the row with the highest entry, which is this row at where
// the task stands while the role works there or worked there last.
func PhaseRowAt(rows []PhaseRow, role claim.Role, task string, at TaskPlace, steps []PlanStep) PhaseRow {
	if task == "" {
		return PhaseRowOf(rows, role)
	}
	row, _ := StepRowOf(rows, role, task, RolePlace(role, task, at, steps))
	return row
}

// RowFound is how StepRowOf found a role's row of a step.
type RowFound int

const (
	// RowOfTheStep is the row the step's runs wrote.
	RowOfTheStep RowFound = iota
	// RowOfNoStep is the role's row of no step (StepTask ""), a row a new generation cleared or the
	// release before wrote, which was the role's one row and so is the step's.
	RowOfNoStep
	// NoRowOfTheStep is neither: the zero row stamped with the role, the task and the place.
	NoRowOfTheStep
)

// StepRowOf is role's row among rows of the step at place of task (place 0 for the retro, the
// merge and a role the task gives no place), and how it was found (RowFound): the row the step's
// runs wrote; else the role's row of no step; else the zero row stamped with role, task and place.
// It is the one lookup of a step's row: where a role's row is read (PhaseRowAt) and where a step's
// entry raises it (workflow's stepRunRow), which moves a row of no step to the step and starts a
// step's first row from the role's row now.
func StepRowOf(rows []PhaseRow, role claim.Role, task string, place int) (PhaseRow, RowFound) {
	var noStep *PhaseRow
	for i, row := range rows {
		switch {
		case row.Role != role:
		case row.StepTask == task && row.StepPlace == place:
			return row, RowOfTheStep
		case row.StepTask == "" && noStep == nil:
			noStep = &rows[i]
		}
	}
	if noStep != nil {
		return *noStep, RowOfNoStep
	}
	return PhaseRow{Role: role, StepTask: task, StepPlace: place}, NoRowOfTheStep
}

// CompletionRow is role's row among rows of the run a completion of role serving entry reports, on
// a task whose steps are steps standing at at (task "" for an issue with no plan rows). Where the
// role works the place the task stands at (works), it is the row there (PhaseRowAt): the role's run
// now, which every other run of the role is behind, so the entry fence holds a turn of another run
// to it (intake.EntryFence). Otherwise the completion is the report of a run the task has left,
// and its row is the row whose entry is the one served: a step's start raises its entry above
// every row of its role (workflow's enter), so no two rows of a role stand on one entry, and the
// served entry names the run whichever of the role's places the task stands at since, before,
// between or after its steps. Where no row stands on that entry (a completion of no entry, or of
// one a later run of its step raised past), it is the role's row now (PhaseRowOf), the run the
// role ran last, and where the role has no row at all, the row PhaseRowAt stamps for it. The
// completion route and the workflow's handoff both read it, so a completion is held to one run,
// recorded on that run's row and written onto that run's step.
func CompletionRow(rows []PhaseRow, role claim.Role, entry int64, works bool, task string, at TaskPlace, steps []PlanStep) PhaseRow {
	if works {
		return PhaseRowAt(rows, role, task, at, steps)
	}
	if row, ok := rowOnEntry(rows, role, entry); ok {
		return row
	}
	if now := PhaseRowOf(rows, role); now.Role == role {
		return now
	}
	return PhaseRowAt(rows, role, task, at, steps)
}

// RolePlace is the place (PhaseRow.StepPlace) of the row of role PhaseRowAt reads at at, on task,
// whose steps are steps: role's place nearest at (NearestPlace) among its steps, the retro, the
// role its agent runs as (PlaceWorker: the role of the task's last step of the type implementation,
// and the implementer where that step is the owner's or the task has none, so the row read there is
// the row its entry raises), and the merge, the merger's; from 1 for a step, 0 for the retro, the
// merge and a role the task gives no place. It is also the place of a row of no step (RunPlace).
func RolePlace(role claim.Role, task string, at TaskPlace, steps []PlanStep) int {
	var places []TaskPlace
	for _, step := range steps {
		if claim.Role(step.Role) == role {
			places = append(places, TaskPlace{Kind: PlaceStep, Position: step.Position})
		}
	}
	if retro, _ := PlaceWorker(task, TaskPlace{Kind: PlaceRetro}, steps); role == retro.Role {
		places = append(places, TaskPlace{Kind: PlaceRetro})
	}
	if role == phase.RoleFor(phase.Merging) {
		places = append(places, TaskPlace{Kind: PlaceMerge})
	}
	place, _ := NearestPlace(places, at)
	return place.Position
}

// RunPlace is the place of the step whose run row holds, on task, whose steps are steps, standing
// at at: the step row ran (PhaseRow.StepPlace), and for a row of no step, one a new generation
// cleared or the release before wrote, which is its role's one row, its role's place nearest at
// (RolePlace). The completion route keys a completion of a role at two steps by it (StandsTwice;
// api's completion route), and the workflow reads the run a report is of by it (Walk.runPlace).
func RunPlace(row PhaseRow, task string, at TaskPlace, steps []PlanStep) int {
	if row.StepTask == "" {
		return RolePlace(row.Role, task, at, steps)
	}
	return row.StepPlace
}

// StandsTwice says whether steps name role at two places or more: a role that stands at two steps
// of one task, whose runs keep a row each.
func StandsTwice(role claim.Role, steps []PlanStep) bool {
	places := 0
	for _, step := range steps {
		if claim.Role(step.Role) == role {
			places++
		}
	}
	return places >= 2
}

// NamesItsAgent says whether row names the agent whose runs wrote it: the claim (PhaseRow.Claim)
// that a step's entry writes on a row that runs a step of the issue's plan (StepTask set). It is
// false for a row of an issue with no plan rows, a row the issue's own new generation cleared
// (ClearGeneration, and ClearTreeGeneration for the root it re-admits, clear StepTask and keep
// Claim), a row no entry was raised on, and a row the release before wrote, whose entry writes no
// claim. Only RowWorker reads such a row as the role's own token's. The entry fence asks another
// question, which claim ran a given entry, and reads the claim alone (ServedClaim, and the row's
// claim in intake.EntryFence): there a row naming no claim is no other agent's turn.
func NamesItsAgent(row PhaseRow) bool { return row.StepTask != "" && row.Claim != "" }

// RowWorker is the claim of the agent whose runs wrote row, role's phases row of issue: the claim
// row names where it names its agent (NamesItsAgent), and the role's own token,
// claim.NewToken(project, issue, role), for every other row. A step's agent can be a second agent
// of the role in a task, whose claim has a name of its own (`…-implementer-2` beside
// `…-implementer`), or the agent of a key, whose claim is the one it started on an earlier step of
// another role, so a role's own token does not name it. project is the project's token, as
// claim.NewToken takes it, and a role that is
// no role of Legion yields its refusal. The worker of a role now (WorkerClaim) and the agent whose
// review a step's row holds the commit of (workflow's stepRunRow) are read through it.
func RowWorker(project, issue string, role claim.Role, row PhaseRow) (claim.Token, error) {
	if NamesItsAgent(row) {
		return row.Claim, nil
	}
	return claim.NewToken(project, issue, role)
}

// WorkerClaim is the claim of the agent that works role's step of issue now, read from the record:
// the worker (RowWorker) of role's row now (PhaseRowOf).
//
// Each derivation of the claim of a role's worker from an issue and a role calls it, so a row that
// names a role reaches the agent that works that role's step when the row runs. It is one rule with
// the start of that step, which names the claim the step's entry writes on the row (workflow's
// stepReach), because a row of no step stands only where the agent of the place its issue stands
// at is the role's own: an issue a new generation put back at admitted, a run of the release
// before, whose plans name no key, and an issue with no plan rows. The clear of a tree leaves the
// steps of every member whose generation it keeps (Store.ClearTreeGeneration), so no row loses its
// step while its issue stands at a step of a second agent of a role or of a keyed agent, and the
// start of a step and every reader of its worker name one agent.
func WorkerClaim(project, issue string, role claim.Role, rows []PhaseRow) (claim.Token, error) {
	return RowWorker(project, issue, role, PhaseRowOf(rows, role))
}

// WorkerClaimOf is WorkerClaim over entries, a batched read of many issues' phases rows
// (Store.PhaseEntries), for a pass over many issues that reads their rows in one statement.
func WorkerClaimOf(project, issue string, role claim.Role, entries map[PhaseRole]PhaseEntry) (claim.Token, error) {
	entry := entries[PhaseRole{Issue: issue, Role: role}]
	return WorkerClaim(project, issue, role, []PhaseRow{{Issue: issue, Role: role, Claim: entry.Claim, StepTask: entry.StepTask}})
}

// StartClaim is the claim a start row of role on issue acts on: the claim the row names
// (SuperviseRequest.Claim), the one a phase worker's start was queued for, or, for a row that names
// none, one the release before wrote, the claim WorkerClaim gives role's step over rows. It is the
// one rule of that claim: the outbox's start acts on it, and the served state and the idle
// release's notice name a start that waits for its pod by it, so all three name one claim. Its
// error is WorkerClaim's, for a role or an issue no token spells.
func StartClaim(project, issue string, role claim.Role, named claim.Token, rows []PhaseRow) (claim.Token, error) {
	if named != "" {
		return named, nil
	}
	return WorkerClaim(project, issue, role, rows)
}

// StartClaims is each of starts, starts of issue that wait for its pod (pods.WaitingStart), with
// the claim it starts in its Claim (StartClaim over rows), each once and in the order of starts: a
// row that names a claim and a row of its role that names none can start one claim, and two rows
// can be one start queued twice. The readers of those rows keep each row (Store.StartsWaitingForPod,
// pods.IdleView.StartsWaitingForPod), so this is the one place they are made distinct.
func StartClaims(project, issue string, starts []pods.WaitingStart, rows []PhaseRow) ([]pods.WaitingStart, error) {
	claims := make([]pods.WaitingStart, 0, len(starts))
	for _, start := range starts {
		token, err := StartClaim(project, issue, start.Role, start.Claim, rows)
		if err != nil {
			return nil, fmt.Errorf("the claim of %s's start of %s that waits for its pod: %w", issue, start.Role, err)
		}
		if started := (pods.WaitingStart{Role: start.Role, Claim: token}); !slices.Contains(claims, started) {
			claims = append(claims, started)
		}
	}
	return claims, nil
}

// IdlePhaseRows is the phases rows an idle view keeps (pods.IdleView.Phases), one of each role
// with its claim and the task of the step it runs, as the rows WorkerClaim and StartClaim read.
func IdlePhaseRows(view pods.IdleView) []PhaseRow {
	rows := make([]PhaseRow, 0, len(view.Phases))
	for role, row := range view.Phases {
		rows = append(rows, PhaseRow{Issue: view.Issue, Role: role, Claim: row.Claim, StepTask: row.StepTask})
	}
	return rows
}

// PhaseReader reads an issue's phases rows (Store.Phases), what ReadWorkerClaim needs of a store.
type PhaseReader interface {
	Phases(ctx context.Context, tx pgx.Tx, issue string) ([]PhaseRow, error)
}

// ReadWorkerClaim is WorkerClaim over issue's phases rows as tx reads them: the claim of the agent
// that works role's step of issue now, read in the caller's transaction. It is the one reader of
// that claim for a caller that holds no rows of its own — the engine (Engine.workerClaim), the
// claim a backstop is armed for (workflow.BackstopTargetAt), the operator's routes (internal/api's workerToken) and
// the outbox's executors (internal/daemon's workerToken and lookBoundAfterPause) — so each reads
// the rows and the rule the same way.
func ReadWorkerClaim(ctx context.Context, phases PhaseReader, tx pgx.Tx, project, issue string, role claim.Role) (claim.Token, error) {
	rows, err := phases.Phases(ctx, tx, issue)
	if err != nil {
		return "", fmt.Errorf("read the phases rows of %s: %w", issue, err)
	}
	return WorkerClaim(project, issue, role, rows)
}
