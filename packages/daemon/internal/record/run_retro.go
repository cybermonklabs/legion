package record

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
)

// RetroOutcome is how a retro Legion ran after its issue's close ended: the retro's own report
// took it (RetroOutcomePassed), or something other than a report ended it (RetroOutcomeStopped):
// the 24-hour bound, or a `todo` that put the issue back in the workflow. It is "" while the retro
// is open, and in a row that holds no retro after a close.
type RetroOutcome string

const (
	RetroOutcomePassed  RetroOutcome = "passed"
	RetroOutcomeStopped RetroOutcome = "stopped"
)

// RetroEndCause is what ended a retro after a close: its report (RetroEndReport), its bound
// (RetroEndBound), or the issue's return to `todo` (RetroEndTodo). It is "" exactly where the
// outcome is, while the retro is open.
type RetroEndCause string

const (
	RetroEndReport RetroEndCause = "report"
	RetroEndBound  RetroEndCause = "bound"
	RetroEndTodo   RetroEndCause = "todo"
)

// RunRetro is the one row of table run_retros for an issue and a run (its generation): when the
// run started, when its retro started, and, for the retro Legion runs after the issue's close, the
// moment that retro opened, its bound, the claim of its agent, its end and how it ended. It is
// the one source of the rule that an issue whose last run had its retro gets none after its
// close and of every fence that rule's retro stands behind: the outbox start, the completion
// route, the linger and the pod gate. Migration 20261125213821_run_retros adds the table and
// fills each recorded issue's current run from what the database held then; from then on the
// methods below are its one writer and its one reader.
//
// StartedAt is when the run first started: its own first phase, or the first phase of an issue
// beneath it. RetroStartedAt is when a retro step of the run first started, whatever its result
// and whether before the close or after, and nil while none has. OpenedAt, BoundAt, Agent and
// Entry belong to the retro after a close: OpenedAt is when the close opened it, BoundAt is
// RetroAfterCloseBound after the retro step's start, Agent is the claim token of the agent that
// does it, and Entry is the phase entry of a retro that was already running at the close (its
// completion is taken on that entry), 0 for every other row. EndedAt, Outcome and EndedBy are set
// together when it ends. The table's checks hold each of these pairings, so a row that breaks one
// is refused at its write rather than read back.
type RunRetro struct {
	Issue          string
	Generation     uint64
	Tree           string
	StartedAt      time.Time
	RetroStartedAt *time.Time
	OpenedAt       *time.Time
	BoundAt        *time.Time
	Agent          claim.Token
	Entry          int64
	EndedAt        *time.Time
	Outcome        RetroOutcome
	EndedBy        RetroEndCause
}

// RetroHad is whether a retro step of the run has started: the fact the close reads to decide
// that the run's retro was held. It does not say the retro was after a close.
func (r RunRetro) RetroHad() bool { return r.RetroStartedAt != nil }

// AfterClose is whether the row carries a retro Legion opened after the issue's close.
func (r RunRetro) AfterClose() bool { return r.OpenedAt != nil }

// Open is whether the retro after the close is open: opened and not ended. The pod gate, the
// linger and the completion route all ask it.
func (r RunRetro) Open() bool { return r.OpenedAt != nil && r.EndedAt == nil }

// EndedHow is how the retro after a close of an ended row ended, as a refusal of a report that
// came after its end names it: by its report, at its bound (that moment, in UTC) with no report,
// or when the issue was set back to todo.
func (r RunRetro) EndedHow() string {
	switch r.EndedBy {
	case RetroEndBound:
		return fmt.Sprintf("at its bound, %s, with no report", r.BoundAt.UTC().Format(time.RFC3339))
	case RetroEndTodo:
		return "when the issue was set back to todo"
	}
	return "by its report"
}

// runRetroColumns is every column of run_retros in the order scanRunRetro reads them.
const runRetroColumns = `issue, generation, tree, started_at, retro_started_at, opened_at, bound_at, agent, entry,
	ended_at, outcome, ended_by`

// MarkRunStarted records that the run (issue, generation) of tree started at at, keeping the
// earliest start the row already holds; a row that already holds that tree and a start no later
// is left unwritten. Its callers are the workflow's transition out of `admitted` and each
// ancestor of the issue that moved.
func (s *Postgres) MarkRunStarted(ctx context.Context, tx pgx.Tx, issue string, generation uint64, tree string, at time.Time) error {
	if _, err := tx.Exec(ctx, `insert into run_retros (issue, generation, tree, started_at) values ($1, $2, $3, $4)
		on conflict (issue, generation) do update set tree = excluded.tree,
		started_at = least(run_retros.started_at, excluded.started_at)
		where run_retros.tree is distinct from excluded.tree or excluded.started_at < run_retros.started_at`,
		issue, int64(generation), tree, at); err != nil {
		return fmt.Errorf("mark run %d of %s started: %w", generation, issue, err)
	}
	return nil
}

// MarkRetroStarted records that a retro step of the run started at at, keeping the first start
// the row already holds, which it leaves unwritten. It is written in the transaction that writes
// the retro's step start. A run with no row yet is one whose start no write recorded, and the
// retro's start is the earliest moment the record knows it ran, so the row it writes starts the
// run there too.
func (s *Postgres) MarkRetroStarted(ctx context.Context, tx pgx.Tx, issue string, generation uint64, tree string, at time.Time) error {
	if _, err := tx.Exec(ctx, `insert into run_retros (issue, generation, tree, started_at, retro_started_at) values ($1, $2, $3, $4, $4)
		on conflict (issue, generation) do update set retro_started_at = excluded.retro_started_at
		where run_retros.retro_started_at is null`,
		issue, int64(generation), tree, at); err != nil {
		return fmt.Errorf("mark the retro of run %d of %s started: %w", generation, issue, err)
	}
	return nil
}

// OpenRetroAfterClose opens the retro after the issue's close on the run's row: openedAt, the
// bound it ends at, the claim token of its agent and the entry of a retro already running at the
// close (0 otherwise). A run opens one retro after a close at most, and only once its row holds
// the retro's start (MarkRetroStarted, which the bound counts from): a run with no row, and one
// whose retro after a close was opened already, are refused, and the table refuses a row with no
// retro start.
func (s *Postgres) OpenRetroAfterClose(ctx context.Context, tx pgx.Tx, issue string, generation uint64, tree string,
	openedAt, boundAt time.Time, agent claim.Token, entry int64) error {
	tag, err := tx.Exec(ctx, `update run_retros set tree = $3, opened_at = $4, bound_at = $5, agent = $6, entry = $7
		where issue = $1 and generation = $2 and opened_at is null`,
		issue, int64(generation), tree, openedAt, boundAt, string(agent), entry)
	if err != nil {
		return fmt.Errorf("open the retro after the close of %s run %d: %w", issue, generation, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("open the retro after the close of %s run %d: the record holds no row of that run, or its retro after a close was opened already", issue, generation)
	}
	return nil
}

// EndRetroAfterClose ends the open retro after a close with outcome and cause. It answers the
// row it ended and true, or the zero row and false where the retro is not open (already ended,
// or never opened), which a repeat of the same end is.
func (s *Postgres) EndRetroAfterClose(ctx context.Context, tx pgx.Tx, issue string, generation uint64,
	endedAt time.Time, outcome RetroOutcome, cause RetroEndCause) (RunRetro, bool, error) {
	row, err := scanRunRetro(tx.QueryRow(ctx, `update run_retros set ended_at = $3, outcome = $4, ended_by = $5
		where issue = $1 and generation = $2 and opened_at is not null and ended_at is null returning `+runRetroColumns,
		issue, int64(generation), endedAt, string(outcome), string(cause)))
	if errors.Is(err, pgx.ErrNoRows) {
		return RunRetro{}, false, nil
	}
	if err != nil {
		return RunRetro{}, false, fmt.Errorf("end the retro after the close of %s run %d: %w", issue, generation, err)
	}
	return row, true, nil
}

// RunRetro is the row of the issue's run generation, nil for none.
func (s *Postgres) RunRetro(ctx context.Context, tx pgx.Tx, issue string, generation uint64) (*RunRetro, error) {
	row, err := scanRunRetro(tx.QueryRow(ctx, `select `+runRetroColumns+` from run_retros where issue = $1 and generation = $2`,
		issue, int64(generation)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read run %d of %s: %w", generation, issue, err)
	}
	return &row, nil
}

// NewestRunRetro is the issue's row of its newest run, nil for none: the run a close owes or keeps
// the retro of.
func (s *Postgres) NewestRunRetro(ctx context.Context, tx pgx.Tx, issue string) (*RunRetro, error) {
	row, err := scanRunRetro(tx.QueryRow(ctx, `select `+runRetroColumns+` from run_retros
		where issue = $1 order by generation desc limit 1`, issue))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the newest run of %s: %w", issue, err)
	}
	return &row, nil
}

// NewestRetroAfterClose is the issue's row of its newest run that holds a retro after a close
// (RunRetro.AfterClose), open or ended, nil for none.
func (s *Postgres) NewestRetroAfterClose(ctx context.Context, tx pgx.Tx, issue string) (*RunRetro, error) {
	row, err := scanRunRetro(tx.QueryRow(ctx, `select `+runRetroColumns+` from run_retros
		where issue = $1 and opened_at is not null order by generation desc limit 1`, issue))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the newest retro after a close of %s: %w", issue, err)
	}
	return &row, nil
}

// OpenRetroOfIssue is the issue's open retro after a close, nil for none. An issue holds one at
// most: the close opens one only on its newest run, and the issue's next run ends it first
// (RetroEndTodo); were there two, the newest run's is the one answered.
func (s *Postgres) OpenRetroOfIssue(ctx context.Context, tx pgx.Tx, issue string) (*RunRetro, error) {
	row, err := scanRunRetro(tx.QueryRow(ctx, `select `+runRetroColumns+` from run_retros
		where issue = $1 and opened_at is not null and ended_at is null order by generation desc limit 1`, issue))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the open retro after a close of %s: %w", issue, err)
	}
	return &row, nil
}

// OpenRetrosOfTree is every open retro after a close of the tree's issues, by bound, then by
// issue, so the first is the one due first.
func (s *Postgres) OpenRetrosOfTree(ctx context.Context, tx pgx.Tx, tree string) ([]RunRetro, error) {
	return queryRunRetros(ctx, tx, fmt.Sprintf("read the open retros after a close of tree %s", tree),
		`select `+runRetroColumns+` from run_retros where tree = $1 and opened_at is not null and ended_at is null
		order by bound_at, issue, generation`, tree)
}

// TreeTakesRetroWake is whether the architect of tree's root takes the wake of a retro's finding at
// now, the caller's clock: a retro after the close of an issue of the tree is open, or one ended no
// earlier than pods.RetroWakeWindow before now, in a tree not past its linger
// (pods.RetroWakeHoldsSQL, which the pod gate embeds too). A retro's agent publishes its finding and
// reports a moment later, so the wake of the finding can come after the report ended the tree's last
// open retro, for as long as Envoy keeps the message.
func (s *Postgres) TreeTakesRetroWake(ctx context.Context, tx pgx.Tx, tree string, now time.Time) (bool, error) {
	var takes bool
	if err := tx.QueryRow(ctx, `select `+pods.RetroWakeHoldsSQL("$1", "$2::timestamptz"), tree, now).Scan(&takes); err != nil {
		return false, fmt.Errorf("read whether the root architect of tree %s takes a retro's wake: %w", tree, err)
	}
	return takes, nil
}

// CarryRunRetro copies the run's start and its retro's start from generation fromGen to toGen of
// the same issue, in toTree: a carried orphan's run is its new tree's from then on, and the bump
// of its generation must not lose the facts the close reads of the run. A run with no row
// carries nothing, since it never started, and a row toGen already holds keeps the earlier start
// and the first retro start of the two.
func (s *Postgres) CarryRunRetro(ctx context.Context, tx pgx.Tx, issue string, fromGen, toGen uint64, toTree string) error {
	if _, err := tx.Exec(ctx, `insert into run_retros (issue, generation, tree, started_at, retro_started_at)
		select issue, $3, $4, started_at, retro_started_at from run_retros where issue = $1 and generation = $2
		on conflict (issue, generation) do update set tree = excluded.tree,
		started_at = least(run_retros.started_at, excluded.started_at),
		retro_started_at = coalesce(run_retros.retro_started_at, excluded.retro_started_at)`,
		issue, int64(fromGen), int64(toGen), toTree); err != nil {
		return fmt.Errorf("carry run %d of %s to run %d in %s: %w", fromGen, issue, toGen, toTree, err)
	}
	return nil
}

// queryRunRetros is every row query answers, in its order; describe names the read in an error.
func queryRunRetros(ctx context.Context, tx pgx.Tx, describe, query string, args ...any) ([]RunRetro, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", describe, err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (RunRetro, error) { return scanRunRetro(row) })
	if err != nil {
		return nil, fmt.Errorf("%s: %w", describe, err)
	}
	return out, nil
}

// scanRunRetro reads one row of runRetroColumns, each moment in UTC as the rest of the record
// reads its moments.
func scanRunRetro(row scanner) (RunRetro, error) {
	var r RunRetro
	var generation int64
	var agent, outcome, endedBy string
	if err := row.Scan(&r.Issue, &generation, &r.Tree, &r.StartedAt, &r.RetroStartedAt, &r.OpenedAt, &r.BoundAt,
		&agent, &r.Entry, &r.EndedAt, &outcome, &endedBy); err != nil {
		return RunRetro{}, err
	}
	r.Generation, r.Agent, r.Outcome, r.EndedBy = uint64(generation), claim.Token(agent), RetroOutcome(outcome), RetroEndCause(endedBy)
	r.StartedAt = r.StartedAt.UTC()
	inUTC(r.RetroStartedAt)
	inUTC(r.OpenedAt)
	inUTC(r.BoundAt)
	inUTC(r.EndedAt)
	return r, nil
}

// inUTC sets the moment at points to, where it points to one, in UTC.
func inUTC(at *time.Time) {
	if at != nil {
		*at = at.UTC()
	}
}
