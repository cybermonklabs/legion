package record

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
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
	for _, row := range rows {
		if entry > 0 && row.Role == role && row.Entry == entry {
			return row.Claim
		}
	}
	return ""
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
// at, whose steps are steps. A role can stand at two steps of a task, one agent doing both, and
// each run of a step keeps a row of its own (StepTask, StepPlace), so the row read is the row of
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
	for _, row := range rows {
		if entry > 0 && row.Role == role && row.Entry == entry {
			return row
		}
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
// the row its entry raises), and the merge, the
// merger's; from 1 for a step, 0 for the retro, the merge and a role the task gives no place. It is
// also the place a completion's key names where the role stands at two steps and its row is of no
// step (StandsTwice; api's completion route).
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

// WorkerClaim is the claim of the agent that works role's step of issue now, read from the record:
// the claim role's row names (PhaseRow.Claim) while that row runs a step of the issue's plan
// (StepTask set), which the step's entry writes. A step's agent can be the agent of a key, whose
// claim is the one it started on an earlier step of another role, so a role's own token does not
// name it. Every other row gives that token, claim.NewToken(project, issue, role): an issue with no
// plan rows, a row the issue's own new generation cleared (ClearGeneration, and ClearTreeGeneration
// for the root it re-admits, clear StepTask and keep Claim), a row no entry was raised on, and a row
// the release before wrote. project is the project's token, as claim.NewToken takes it, and a role
// that is no role of Legion yields its refusal.
//
// Each derivation of the claim of a role's worker from an issue and a role calls it, so a row that
// names a role reaches the agent that works that role's step when the row runs. It is one rule with
// the start of that step, which names the claim the step's entry writes on the row (workflow's
// stepReach), because a row of no step stands only where the agent of the place its issue stands
// at is the role's own: an issue a new generation put back at admitted, a run of the release
// before, whose plans name no key, and an issue with no plan rows. The clear of a tree leaves the
// steps of every member whose generation it keeps (Store.ClearTreeGeneration), so no row loses its
// step while its issue stands at a step of a keyed agent, and the start of a step and every reader
// of its worker name one agent.
func WorkerClaim(project, issue string, role claim.Role, rows []PhaseRow) (claim.Token, error) {
	if row := PhaseRowOf(rows, role); row.StepTask != "" && row.Claim != "" {
		return row.Claim, nil
	}
	return claim.NewToken(project, issue, role)
}

// WorkerClaimOf is WorkerClaim over entries, a batched read of many issues' phases rows
// (Store.PhaseEntries), for a pass over many issues that reads their rows in one statement.
func WorkerClaimOf(project, issue string, role claim.Role, entries map[PhaseRole]PhaseEntry) (claim.Token, error) {
	entry := entries[PhaseRole{Issue: issue, Role: role}]
	return WorkerClaim(project, issue, role, []PhaseRow{{Issue: issue, Role: role, Claim: entry.Claim, StepTask: entry.StepTask}})
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
