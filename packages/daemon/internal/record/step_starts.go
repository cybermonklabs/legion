package record

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// StepPlace is one place of a task where a step runs: the task, the kind of step (step, retro or
// merge) and its place in the task, from 1 for a step and 0 for a retro or a merge, which no plan
// names (workflow's stepOfWalk).
type StepPlace struct {
	Task  string
	Kind  string
	Place int
}

// StepRun is where the latest run of one place of an issue stands in the store.
type StepRun string

const (
	// RunOpen is a latest start whose run has no end yet: the next end of the step running there
	// is that run's end.
	RunOpen StepRun = "open"
	// RunEnded is a latest start whose run has its end: a later end of that run writes nothing.
	RunEnded StepRun = "ended"
	// RunUncounted is a place whose counted run is over and whose run now, where it holds one,
	// started under an earlier release, which counts no start. Only the roll forward of a rollback
	// writes it (store migration 20261117064152_step_starts_roll_forward), for each place of an
	// issue that release moved, and the daemon's boot opens the run it stands for at the next count
	// (OpenUncountedStep), so its end ends an open run like any other.
	RunUncounted StepRun = "uncounted"
)

// StepStart is the count of starts of one place of an issue, the agent and the moment its latest
// start wrote, and where that start's run stands: what the end of the step running there computes
// the record's id from and writes again (workflow's endStep), so the start and the end agree
// whichever reaches Dispatch first. The zero StepStart is a place no start of this release has
// raised: a step that was running when the release deployed. StartedAt is zero where the moment
// is unknown: an end of such a step whose role's entry recorded none. StartCommit and FixCheck are
// the commit the latest start named and, for a fix's start, the check the fix is of, and
// EndCommit the commit the end of that start's run named: "" for none, and EndCommit "" until the
// run ends. BesideFix is the latest fix of the task that ran beside the latest start's run
// (MarkStepBesideFix): the open fix that start named, or a fix that started while the run was open,
// 0 for none; and PushBranch the branch beside a fix that start's step pushes to
// (SetStepPushBranch), "" for none: each start writes both again, so neither outlives the start
// that took it.
type StepStart struct {
	Starts      int64
	Agent       string
	StartedAt   time.Time
	Run         StepRun
	StartCommit string
	EndCommit   string
	FixCheck    string
	BesideFix   int
	PushBranch  string
}

// ErrStepStartMismatch is the answer of a write to one start of a place (MarkStepBesideFix,
// SetStepPushBranch) where that start is not the place's open run: a later start counted since,
// or the run ended.
var ErrStepStartMismatch = errors.New("the start is not the open run of its place")

// RaiseStepStarts counts one more start of place on issue, marks its run open whatever the run
// before it stood at, and answers that start's write: run, given the count with that start in it,
// is the write of the start's record (workflow's startStep). The row keeps the agent and the
// moment that write names, which the run's end writes again, and the write itself — the identity
// of the record, its id among them, and the moment; a start writes nothing else (workflow's
// startStep) — which the roll forward of a rollback ends the record under where an earlier
// release ended the run (store migration 20261117064152_step_starts_roll_forward). The start names
// no fix and no branch beside one until MarkStepBesideFix and SetStepPushBranch write them for it,
// so a binding of an earlier start never reaches it. It is called in the transaction that writes
// the start's record.
func (s *Postgres) RaiseStepStarts(ctx context.Context, tx pgx.Tx, issue string, place StepPlace, run func(starts int64) StepRecord) (StepRecord, error) {
	var starts int64
	if err := tx.QueryRow(ctx, `insert into step_starts (issue, task, kind, place, starts, agent, run) values ($1, $2, $3, $4, 1, '', 'open')
		on conflict (issue, task, kind, place) do update set starts = step_starts.starts + 1, run = 'open'
		returning starts`, issue, place.Task, place.Kind, place.Place).Scan(&starts); err != nil {
		return StepRecord{}, fmt.Errorf("count a start of %s %s/%s/%d: %w", issue, place.Task, place.Kind, place.Place, err)
	}
	write := run(starts)
	if err := s.keepStart(ctx, tx, issue, place, write); err != nil {
		return StepRecord{}, err
	}
	return write, nil
}

// keepStart keeps write, the record of the latest start of place on issue, on the place's row: the
// agent, the moment, the commit and the fix's check it names, which the run's end writes again, and
// the write itself, which the roll forward of a rollback ends the record under where an earlier
// release ended the run. The start is a run of its own, so the row names no end commit, no fix
// beside it and no branch beside a fix after it.
func (s *Postgres) keepStart(ctx context.Context, tx pgx.Tx, issue string, place StepPlace, write StepRecord) error {
	identity, err := json.Marshal(write)
	if err != nil {
		return fmt.Errorf("keep the start of %s %s/%s/%d: %w", issue, place.Task, place.Kind, place.Place, err)
	}
	if _, err := tx.Exec(ctx, `update step_starts set agent = $5, started_at = $6, record = $7, start_commit = $8, fix_check = $9,
			end_commit = '', beside_fix = 0
		where issue = $1 and task = $2 and kind = $3 and place = $4`,
		issue, place.Task, place.Kind, place.Place, write.Agent, write.StartedAt, identity,
		textOf(write.StartCommit), textOf(write.FixCheck)); err != nil {
		return fmt.Errorf("keep the start of %s %s/%s/%d: %w", issue, place.Task, place.Kind, place.Place, err)
	}
	return nil
}

// textOf is the text a field of a step record names, "" for one it leaves out.
func textOf(field *string) string {
	if field == nil {
		return ""
	}
	return *field
}

// StepStarts is the count of starts of place on issue, with the agent, the moment and the run of
// the latest, unchanged: the zero StepStart where the place has no row.
func (s *Postgres) StepStarts(ctx context.Context, tx pgx.Tx, issue string, place StepPlace) (StepStart, error) {
	start, err := scanStepStart(tx.QueryRow(ctx, `select `+stepStartColumns+` from step_starts
		where issue = $1 and task = $2 and kind = $3 and place = $4`, issue, place.Task, place.Kind, place.Place))
	if errors.Is(err, pgx.ErrNoRows) {
		return StepStart{}, nil
	}
	if err != nil {
		return StepStart{}, fmt.Errorf("read the starts of %s %s/%s/%d: %w", issue, place.Task, place.Kind, place.Place, err)
	}
	return start, nil
}

// StepEntry is one run of a step of an issue: the place it runs at and the count of starts of that
// place that began it (StepStart.Starts). The zero StepEntry is no run the record counts.
type StepEntry struct {
	Place  StepPlace
	Starts int64
}

// OpenStep is the run of a step of issue that has no end yet whose start wrote the latest moment,
// a run whose start wrote none coming after every one that wrote one. A start of the step the
// issue stands in is such a start: it first ends each run of the issue that is over (workflow's
// startStep), then writes the moment now. An issue can have several runs open at once, since the
// fact's settle ends each run when its own end rule holds (workflow's settleRuns): a run whose
// place no step of its walk holds stays open beside the issue's current run until a settle of the
// issue ends it (workflow's endOffWalk). A run the daemon's boot opened for a step that started
// under an earlier release (OpenUncountedStep) is open as any other, and is answered with the
// count its row holds: 0 where the place had no row. It is the zero StepEntry where no run is
// open. An architect's request about the step an issue stands in names the run its route read
// here, and the fact refuses it where another run stands under the fact lock (sign_off,
// request_backward_move).
func (s *Postgres) OpenStep(ctx context.Context, tx pgx.Tx, issue string) (StepEntry, error) {
	var entry StepEntry
	err := tx.QueryRow(ctx, `select task, kind, place, starts from step_starts where issue = $1 and run = 'open'
		order by started_at desc nulls last, task, kind, place limit 1`, issue).Scan(&entry.Place.Task, &entry.Place.Kind, &entry.Place.Place, &entry.Starts)
	if errors.Is(err, pgx.ErrNoRows) {
		return StepEntry{}, nil
	}
	if err != nil {
		return StepEntry{}, fmt.Errorf("read the open step of %s: %w", issue, err)
	}
	return entry, nil
}

// EndStepStart is the end of the step running at place on issue, in the transaction that writes
// that run's end, at endCommit, the commit the end names ("" for none). An open run it marks ended
// with that commit, and answers that start, marked, and true. It marks nothing otherwise, and
// answers false: the run's end was written already, since the settle of an earlier fact ended it.
// The update locks the row, so two transactions that end one run at one time end it once: the
// second finds no open run once the first commits.
func (s *Postgres) EndStepStart(ctx context.Context, tx pgx.Tx, issue string, place StepPlace, endCommit string) (StepStart, bool, error) {
	start, err := scanStepStart(tx.QueryRow(ctx, `update step_starts set run = 'ended', end_commit = $5
		where issue = $1 and task = $2 and kind = $3 and place = $4 and run = 'open'
		returning `+stepStartColumns, issue, place.Task, place.Kind, place.Place, endCommit))
	if errors.Is(err, pgx.ErrNoRows) {
		return StepStart{}, false, nil
	}
	if err != nil {
		return StepStart{}, false, fmt.Errorf("end the latest start of %s %s/%s/%d: %w", issue, place.Task, place.Kind, place.Place, err)
	}
	return start, true, nil
}

// OpenUncountedStep opens, at the daemon's boot, the run at place on issue that no start of this
// release counted, as a start opens a run: marked open at the count 0 for a place with no row, a
// step that started under the release before this one deployed, and at the count after the one its
// row holds for a row that reads RunUncounted, a step an earlier release started after a rollback,
// so the record of the run that row counted keeps its own id — with the write run gives at that count
// kept as a start keeps its own (keepStart): its agent, its moment and its identity. The run's end
// is then the end of an open run (EndStepStart), under the id of that count, and the next start of
// the place counts one more. It queues nothing. It answers false, and writes nothing, where the
// place's row is open or ended: a start of this release counted that run, or a boot before this
// one opened it.
func (s *Postgres) OpenUncountedStep(ctx context.Context, tx pgx.Tx, issue string, place StepPlace, run func(starts int64) StepRecord) (bool, error) {
	var starts int64
	err := tx.QueryRow(ctx, `update step_starts set starts = starts + 1, run = 'open'
		where issue = $1 and task = $2 and kind = $3 and place = $4 and run = 'uncounted'
		returning starts`, issue, place.Task, place.Kind, place.Place).Scan(&starts)
	if errors.Is(err, pgx.ErrNoRows) {
		tag, err := tx.Exec(ctx, `insert into step_starts (issue, task, kind, place, starts, agent, run) values ($1, $2, $3, $4, 0, '', 'open')
			on conflict (issue, task, kind, place) do nothing`, issue, place.Task, place.Kind, place.Place)
		if err != nil {
			return false, fmt.Errorf("open the uncounted step of %s %s/%s/%d: %w", issue, place.Task, place.Kind, place.Place, err)
		}
		if tag.RowsAffected() == 0 {
			return false, nil
		}
	} else if err != nil {
		return false, fmt.Errorf("open the uncounted step of %s %s/%s/%d: %w", issue, place.Task, place.Kind, place.Place, err)
	}
	if err := s.keepStart(ctx, tx, issue, place, run(starts)); err != nil {
		return false, err
	}
	return true, nil
}

// MarkStepBesideFix writes that fix, a fix of its task, ran beside start number starts of place on
// issue (StepStart.BesideFix): in the transaction that counted that start (RaiseStepStarts), which
// writes 0 for every start, where the start named the fix open, and in the transaction that starts
// the fix, where the start's run was open then. A start that is not the place's open run is
// refused with ErrStepStartMismatch, and nothing is written.
func (s *Postgres) MarkStepBesideFix(ctx context.Context, tx pgx.Tx, issue string, place StepPlace, starts int64, fix int) error {
	describe := fmt.Sprintf("mark start %d of %s %s/%s/%d beside fix %d", starts, issue, place.Task, place.Kind, place.Place, fix)
	if fix < 0 {
		return fmt.Errorf("%s: a fix counts from 1", describe)
	}
	tag, err := tx.Exec(ctx, `update step_starts set beside_fix = $6
		where issue = $1 and task = $2 and kind = $3 and place = $4 and starts = $5 and run = 'open'`,
		issue, place.Task, place.Kind, place.Place, starts, fix)
	if err != nil {
		return fmt.Errorf("%s: %w", describe, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%s: %w", describe, ErrStepStartMismatch)
	}
	return nil
}

// SetStepPushBranch binds branch, a branch beside a fix, to start number starts of place on issue:
// the branch that step pushes to until its run ends (StepStart.PushBranch). A start that holds a
// binding keeps it, so a step keeps its branch when a later fix of its task starts, and a start
// that is not the place's open run is refused with ErrStepStartMismatch, writing nothing.
func (s *Postgres) SetStepPushBranch(ctx context.Context, tx pgx.Tx, issue string, place StepPlace, starts int64, branch string) error {
	describe := fmt.Sprintf("bind branch %q to start %d of %s %s/%s/%d", branch, starts, issue, place.Task, place.Kind, place.Place)
	if branch == "" {
		return fmt.Errorf("%s: a binding names a branch", describe)
	}
	// The update takes the open start alone and binds it only where it holds no branch yet; the
	// row lock it takes keeps two bindings of one start in one order.
	var bound string
	err := tx.QueryRow(ctx, `update step_starts set push_branch = case when push_branch = '' then $6 else push_branch end
		where issue = $1 and task = $2 and kind = $3 and place = $4 and starts = $5 and run = 'open'
		returning push_branch`,
		issue, place.Task, place.Kind, place.Place, starts, branch).Scan(&bound)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%s: %w", describe, ErrStepStartMismatch)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", describe, err)
	}
	return nil
}

// stepStartColumns is the columns of a step_starts row in the order scanStepStart reads them.
const stepStartColumns = `starts, agent, started_at, run, start_commit, end_commit, fix_check, beside_fix, push_branch`

// scanStepStart reads one step_starts row of stepStartColumns.
func scanStepStart(row pgx.Row) (StepStart, error) {
	var start StepStart
	var at *time.Time
	if err := row.Scan(&start.Starts, &start.Agent, &at, &start.Run, &start.StartCommit, &start.EndCommit, &start.FixCheck,
		&start.BesideFix, &start.PushBranch); err != nil {
		return StepStart{}, err
	}
	if at != nil {
		start.StartedAt = at.UTC()
	}
	return start, nil
}
