package record

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// A fix is the first implementation step of a task run again beside the task's other steps,
// because a check that a CI job runs failed at the task's head (store migration
// 20261123134547_fix_beside_steps). task_fixes holds each fix of a task, numbered from 1, and
// ci_failures holds each failure of such a check at a head of the task, once. Every row is keyed
// by the issue, its generation and the task, as the plan's rows are. Whether a task's run takes a
// fix at all is the task's own (PlanTask.FixesCI).

// FixState is where a fix stands (TaskFix.State).
type FixState string

const (
	// FixReading is a fix whose start reads its failed job from Dispatch first (FixJobRead). Its
	// entry is not raised yet, so no task of it is sent, and the issue read names no running fix
	// (Issue.RunningFix).
	FixReading FixState = "reading"
	// FixRunning is a fix whose entry is raised on the implementer's phases row (TaskFix.Entry):
	// its task is the work of its issue whatever phase the issue stands in (PhaseHolds).
	FixRunning FixState = "running"
	// FixEnded is a fix that is over, however it ended (TaskFix.Result, TaskFix.Way).
	FixEnded FixState = "ended"
)

// TaskFix is one fix of a task: Fix is its number among the task's fixes, from 1, and Check and
// Head are the check that failed and the head it failed at, which the fix started for. Place is
// the place in the task of the step the fix runs again, the task's first implementation step, and
// Entry is the phase entry the fix raised on the implementer's phases row, 0 while it reads its
// job. GotReport says a step that ran beside the fix failed and gave its report to the fix,
// ReturnTo is the earliest place such a failed step returns the task to (0 for none), and Failed
// says the fix's own step failed. EndHead is the head the fix ended at, and Result how it ended:
// "" while it is open, then "passed", "failed" or "stopped". JobName and JobURL are the failed
// job the read found, ReadFailed why the read found none, and ClaimFailed why the fix's claim
// failed; each is "" for none. StartedAt and EndedAt are nil until the fix starts and ends.
type TaskFix struct {
	Issue           string
	Generation      uint64
	Task            string
	Fix             int
	State           FixState
	Check, Head     string
	Place           int
	Entry           int64
	GotReport       bool
	ReturnTo        int
	Failed          bool
	EndHead, Result string
	JobName, JobURL string
	ReadFailed      string
	ClaimFailed     string
	// Way is 0 until the fix's end has decided what runs after it; then which of the three ways
	// it took, 1, 2 or 3; and 4 for a fix a stop, a leave or a new generation ended, which takes
	// no way.
	Way int
	// Pushed is, for each role, the head of that role's branch beside the fix that a push of it
	// showed (RecordBesidePush), nil for none: what the texts of a step beside the fix name of its
	// own branch. PutFix and UpdateFix write none of it.
	Pushed             map[string]string
	StartedAt, EndedAt *time.Time
}

// CIFailureTarget is whom a failure of a check at a head of a task went to (CIFailure.WentTo).
type CIFailureTarget string

const (
	// CIFailureToFix is a failure a fix of the task took (CIFailure.Fix names it).
	CIFailureToFix CIFailureTarget = "fix"
	// CIFailureToImplementer is a failure told to the implementer while its step runs.
	CIFailureToImplementer CIFailureTarget = "implementer"
	// CIFailureToMerger is a failure at the retro or the merge, which the merger decides.
	CIFailureToMerger CIFailureTarget = "merger"
	// CIFailureToOwner is a failure no fix takes, which the task's owner is told of.
	CIFailureToOwner CIFailureTarget = "owner"
)

// ciFailureTargets is every value ci_failures.went_to takes.
var ciFailureTargets = []CIFailureTarget{CIFailureToFix, CIFailureToImplementer, CIFailureToMerger, CIFailureToOwner}

// CIFailure is one failure of check Check at head Head of a task: whom it went to, the fix that
// took it (0 for none), whether its owner was told of it (TellOwnerOnce) and when it was first
// recorded.
type CIFailure struct {
	Issue       string
	Generation  uint64
	Task        string
	Check, Head string
	WentTo      CIFailureTarget
	Fix         int
	OwnerTold   bool
	At          time.Time
}

// ErrFixOpen is PutFix's answer for a task one of whose fixes still reads its job or runs: a task
// runs one fix at a time (the index task_fixes_one_open holds the same rule).
var ErrFixOpen = errors.New("a fix of the task is still reading its job or running")

// fixInsertColumns is the columns PutFix writes of a task_fixes row, in the order fixValues gives
// them, and fixColumns the columns a read scans (scanFix): those and pushed, which only
// RecordBesidePush writes.
const (
	fixInsertColumns = `issue, generation, task, fix, state, check_id, head, place, entry, got_report, return_to, failed,
	end_head, result, job_name, job_url, read_failed, claim_failed, way, started_at, ended_at`
	fixColumns = fixInsertColumns + `, pushed`
)

// fixKey is the key of a task_fixes or ci_failures row of a task, as $1 to $3.
const fixKey = `issue = $1 and generation = $2 and task = $3`

// PutFix writes f, a new fix of its task, reading its failed job (FixReading), for a check at a
// head, at a place of the task. Its number is the one after the task's latest fix (LatestFix; 1
// for a task with none), and a number that is not that one is refused. So is a fix of a task one
// of whose fixes still reads its job or runs (ErrFixOpen). Each refusal is made before anything
// is written, so the transaction stays usable.
func (s *Postgres) PutFix(ctx context.Context, tx pgx.Tx, f TaskFix) error {
	describe := fmt.Sprintf("put fix %d of task %s of %s at generation %d", f.Fix, f.Task, f.Issue, f.Generation)
	if f.State != FixReading || f.Check == "" || f.Head == "" || f.Place < 1 {
		return fmt.Errorf("%s: a fix starts reading its failed job, for a check at a head, at a place of the task; it is %q, for %q at %q, at place %d",
			describe, f.State, f.Check, f.Head, f.Place)
	}
	var latest int
	var open bool
	if err := tx.QueryRow(ctx, `select coalesce(max(fix), 0), coalesce(bool_or(state in ('reading', 'running')), false)
		from task_fixes where `+fixKey, f.Issue, int64(f.Generation), f.Task).Scan(&latest, &open); err != nil {
		return fmt.Errorf("%s: %w", describe, err)
	}
	if open {
		return fmt.Errorf("%s: %w", describe, ErrFixOpen)
	}
	if f.Fix != latest+1 {
		return fmt.Errorf("%s: the task's next fix is fix %d", describe, latest+1)
	}
	if _, err := tx.Exec(ctx, `insert into task_fixes (`+fixInsertColumns+`)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)`, fixValues(f)...); err != nil {
		return fmt.Errorf("%s: %w", describe, err)
	}
	return nil
}

// UpdateFix writes f over the stored fix of the same issue, generation, task and number, every
// other field as f carries it but Pushed, which RecordBesidePush alone writes: a fix read before
// a push and written after it keeps that push. A fix the store does not hold is refused.
func (s *Postgres) UpdateFix(ctx context.Context, tx pgx.Tx, f TaskFix) error {
	values, setPushed := fixValues(f), ""
	if s.rewritePushedWithFix {
		pushed, err := json.Marshal(f.Pushed)
		if err != nil {
			return fmt.Errorf("update fix %d of task %s of %s at generation %d: %w", f.Fix, f.Task, f.Issue, f.Generation, err)
		}
		if f.Pushed == nil {
			pushed = []byte(`{}`)
		}
		values, setPushed = append(values, pushed), ", pushed = $22"
	}
	tag, err := tx.Exec(ctx, `update task_fixes set state = $5, check_id = $6, head = $7, place = $8, entry = $9, got_report = $10,
			return_to = $11, failed = $12, end_head = $13, result = $14, job_name = $15, job_url = $16, read_failed = $17,
			claim_failed = $18, way = $19, started_at = $20, ended_at = $21`+setPushed+`
		where `+fixKey+` and fix = $4`, values...)
	if err != nil {
		return fmt.Errorf("update fix %d of task %s of %s at generation %d: %w", f.Fix, f.Task, f.Issue, f.Generation, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("update fix %d of task %s of %s at generation %d: the store holds no such fix", f.Fix, f.Task, f.Issue, f.Generation)
	}
	return nil
}

// fixValues is f's columns as fixInsertColumns lists them.
func fixValues(f TaskFix) []any {
	return []any{f.Issue, int64(f.Generation), f.Task, f.Fix, f.State, f.Check, f.Head, f.Place, f.Entry, f.GotReport, f.ReturnTo, f.Failed,
		f.EndHead, f.Result, f.JobName, f.JobURL, f.ReadFailed, f.ClaimFailed, f.Way, f.StartedAt, f.EndedAt}
}

// RecordBesidePush writes that a push showed head at the branch beside fix of task that role
// pushes to, and a head of "" that the branch was deleted: it changes that role's key of the
// fix's Pushed and no other, in one statement on the stored row, so a push another role made at
// the same time and a fix written from a read before either keep each other's heads. A push of a
// branch beside a fix the store does not hold, and an unset role, write nothing.
func (s *Postgres) RecordBesidePush(ctx context.Context, tx pgx.Tx, issue string, generation uint64, task string, fix int, role, head string) error {
	if role == "" {
		return nil
	}
	if s.rewritePushedWithFix {
		stored, err := s.FixAt(ctx, tx, issue, generation, task, fix)
		if err != nil || stored == nil {
			return err
		}
		pushed := maps.Clone(stored.Pushed)
		if pushed == nil {
			pushed = map[string]string{}
		}
		if head == "" {
			delete(pushed, role)
		} else {
			pushed[role] = head
		}
		stored.Pushed = pushed
		return s.UpdateFix(ctx, tx, *stored)
	}
	if _, err := tx.Exec(ctx, `update task_fixes
		set pushed = case when $6::text = '' then pushed - $5::text else pushed || jsonb_build_object($5::text, $6::text) end
		where `+fixKey+` and fix = $4`, issue, int64(generation), task, fix, role, head); err != nil {
		return fmt.Errorf("record the push of %s's branch beside fix %d of task %s of %s at generation %d: %w",
			role, fix, task, issue, generation, err)
	}
	return nil
}

// FixAt is fix number fix of task, in any state, nil where the store holds none: the fix a step's
// bound branch beside a fix names (StepStart.PushBranch), whichever fix of the task is the latest.
func (s *Postgres) FixAt(ctx context.Context, tx pgx.Tx, issue string, generation uint64, task string, fix int) (*TaskFix, error) {
	stored, err := scanFix(tx.QueryRow(ctx, `select `+fixColumns+` from task_fixes
		where `+fixKey+` and fix = $4`, issue, int64(generation), task, fix))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read fix %d of task %s of %s at generation %d: %w", fix, task, issue, generation, err)
	}
	return stored, nil
}

// RunningFix is the fix of task that reads its job or runs, nil where none does: at most one,
// since a task runs one fix at a time (PutFix).
func (s *Postgres) RunningFix(ctx context.Context, tx pgx.Tx, issue string, generation uint64, task string) (*TaskFix, error) {
	fix, err := scanFix(tx.QueryRow(ctx, `select `+fixColumns+` from task_fixes
		where `+fixKey+` and state in ('reading', 'running')`, issue, int64(generation), task))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the running fix of task %s of %s at generation %d: %w", task, issue, generation, err)
	}
	return fix, nil
}

// LatestFix is the fix of task with the highest number, in any state, nil where the task has none.
func (s *Postgres) LatestFix(ctx context.Context, tx pgx.Tx, issue string, generation uint64, task string) (*TaskFix, error) {
	fix, err := scanFix(tx.QueryRow(ctx, `select `+fixColumns+` from task_fixes
		where `+fixKey+` order by fix desc limit 1`, issue, int64(generation), task))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the latest fix of task %s of %s at generation %d: %w", task, issue, generation, err)
	}
	return fix, nil
}

// scanFix reads one task_fixes row of fixColumns.
func scanFix(row pgx.Row) (*TaskFix, error) {
	var f TaskFix
	var generation int64
	var pushed []byte
	if err := row.Scan(&f.Issue, &generation, &f.Task, &f.Fix, &f.State, &f.Check, &f.Head, &f.Place, &f.Entry, &f.GotReport,
		&f.ReturnTo, &f.Failed, &f.EndHead, &f.Result, &f.JobName, &f.JobURL, &f.ReadFailed, &f.ClaimFailed, &f.Way,
		&f.StartedAt, &f.EndedAt, &pushed); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(pushed, &f.Pushed); err != nil {
		return nil, fmt.Errorf("read the pushes of fix %d of task %s of %s: %w", f.Fix, f.Task, f.Issue, err)
	}
	if len(f.Pushed) == 0 {
		f.Pushed = nil
	}
	f.Generation = uint64(generation)
	f.StartedAt, f.EndedAt = inUTC(f.StartedAt), inUTC(f.EndedAt)
	return &f, nil
}

// inUTC is at read in UTC, as every time the record answers is; nil stays nil.
func inUTC(at *time.Time) *time.Time {
	if at == nil {
		return nil
	}
	utc := at.UTC()
	return &utc
}

// ciFailureKey is the key of one ci_failures row, as $1 to $5.
const ciFailureKey = fixKey + ` and check_id = $4 and head = $5`

// PutCIFailure writes f, a failure of a check at a head of a task, and answers true; false, and
// nothing written, where the store holds a failure of that check at that head already, which
// stays as it was. A target that is no CIFailureTarget is refused before anything is written.
func (s *Postgres) PutCIFailure(ctx context.Context, tx pgx.Tx, f CIFailure) (bool, error) {
	describe := fmt.Sprintf("put the failure of %s at %s of task %s of %s at generation %d", f.Check, f.Head, f.Task, f.Issue, f.Generation)
	if !slices.Contains(ciFailureTargets, f.WentTo) || f.Fix < 0 {
		return false, fmt.Errorf("%s: it went to %q, fix %d; a failure goes to a fix, the implementer, the merger or the owner", describe, f.WentTo, f.Fix)
	}
	tag, err := tx.Exec(ctx, `insert into ci_failures (issue, generation, task, check_id, head, went_to, fix, owner_told, at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9) on conflict (issue, generation, task, check_id, head) do nothing`,
		f.Issue, int64(f.Generation), f.Task, f.Check, f.Head, f.WentTo, f.Fix, f.OwnerTold, f.At)
	if err != nil {
		return false, fmt.Errorf("%s: %w", describe, err)
	}
	return tag.RowsAffected() == 1, nil
}

// CIFailureAt is the failure of check at head of task, nil where the store holds none.
func (s *Postgres) CIFailureAt(ctx context.Context, tx pgx.Tx, issue string, generation uint64, task, check, head string) (*CIFailure, error) {
	f := CIFailure{Issue: issue, Generation: generation, Task: task, Check: check, Head: head}
	err := tx.QueryRow(ctx, `select went_to, fix, owner_told, at from ci_failures where `+ciFailureKey,
		issue, int64(generation), task, check, head).Scan(&f.WentTo, &f.Fix, &f.OwnerTold, &f.At)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the failure of %s at %s of task %s of %s at generation %d: %w", check, head, task, issue, generation, err)
	}
	f.At = f.At.UTC()
	return &f, nil
}

// MarkCIFailureFix writes that fix took the failure of check at head of task: the row goes to a
// fix and names its number. A fix numbered below 1, a failure that names a fix already and a
// failure the store holds no row of are refused, each writing nothing.
func (s *Postgres) MarkCIFailureFix(ctx context.Context, tx pgx.Tx, issue string, generation uint64, task, check, head string, fix int) error {
	describe := fmt.Sprintf("mark the failure of %s at %s of task %s of %s at generation %d as fix %d's", check, head, task, issue, generation, fix)
	if fix < 1 {
		return fmt.Errorf("%s: a fix counts from 1", describe)
	}
	tag, err := tx.Exec(ctx, `update ci_failures set went_to = $6, fix = $7 where `+ciFailureKey+` and fix = 0`,
		issue, int64(generation), task, check, head, CIFailureToFix, fix)
	if err != nil {
		return fmt.Errorf("%s: %w", describe, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	stored, err := s.CIFailureAt(ctx, tx, issue, generation, task, check, head)
	if err != nil {
		return fmt.Errorf("%s: %w", describe, err)
	}
	if stored == nil {
		return fmt.Errorf("%s: the store holds no such failure", describe)
	}
	return fmt.Errorf("%s: fix %d took it already", describe, stored.Fix)
}

// TellOwnerOnce marks the failure of check at head of task told to its owner, and answers true
// the first time and false after, so the owner is told of one failure once. A failure the store
// holds no row of is refused: there would be nothing recorded to tell.
func (s *Postgres) TellOwnerOnce(ctx context.Context, tx pgx.Tx, issue string, generation uint64, task, check, head string) (bool, error) {
	describe := fmt.Sprintf("tell the owner of the failure of %s at %s of task %s of %s at generation %d", check, head, task, issue, generation)
	tag, err := tx.Exec(ctx, `update ci_failures set owner_told = true where `+ciFailureKey+` and not owner_told`,
		issue, int64(generation), task, check, head)
	if err != nil {
		return false, fmt.Errorf("%s: %w", describe, err)
	}
	if tag.RowsAffected() == 1 {
		return true, nil
	}
	stored, err := s.CIFailureAt(ctx, tx, issue, generation, task, check, head)
	if err != nil {
		return false, fmt.Errorf("%s: %w", describe, err)
	}
	if stored == nil {
		return false, fmt.Errorf("%s: the store holds no such failure", describe)
	}
	return false, nil
}

// MarkRunFixesCI marks every task of the plan of issue at generation as one whose run takes a
// fix (PlanTask.FixesCI). A generation with no plan rows has no task to mark, and nothing is
// written.
func (s *Postgres) MarkRunFixesCI(ctx context.Context, tx pgx.Tx, issue string, generation uint64) error {
	if _, err := tx.Exec(ctx, `update plan_tasks set fixes_ci = true where issue = $1 and generation = $2`, issue, int64(generation)); err != nil {
		return fmt.Errorf("mark the run of %s at generation %d to take a fix: %w", issue, generation, err)
	}
	return nil
}

// RunFixesCI says whether the run of issue at generation takes a fix: a task of its plan holds
// PlanTask.FixesCI. A generation with no plan rows takes none.
func (s *Postgres) RunFixesCI(ctx context.Context, tx pgx.Tx, issue string, generation uint64) (bool, error) {
	var takes bool
	if err := tx.QueryRow(ctx, `select exists (select 1 from plan_tasks where issue = $1 and generation = $2 and fixes_ci)`,
		issue, int64(generation)).Scan(&takes); err != nil {
		return false, fmt.Errorf("read whether the run of %s at generation %d takes a fix: %w", issue, generation, err)
	}
	return takes, nil
}
