package record

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// errRunRetroNotImplemented is what each store method of the run retro record answers until the
// migration and the store code of INFRA-5602's task t2 land: the table the methods read and write
// (run_retros) does not exist before that migration, so a caller reaches the stub and not a
// missing relation.
var errRunRetroNotImplemented = errors.New("run_retros: not implemented until INFRA-5602 t2")

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
// close (INFRA-5602's P1) and of every fence that rule's retro stands behind: the outbox start,
// the completion route, the linger and the pod gate. The code that writes and reads it lands in
// INFRA-5602's tasks t2 to t4; this type holds the shape those tasks build to.
//
// StartedAt is when the run first started: its own first phase, or the first phase of an issue
// beneath it. RetroStartedAt is when a retro step of the run first started, whatever its result
// and whether before the close or after, and nil while none has. OpenedAt, BoundAt, Agent and
// Entry belong to the retro after a close: OpenedAt is when the close opened it, BoundAt is
// RetroAfterCloseBound after the retro step's start, Agent is the claim token of the agent that
// does it, and Entry is the phase entry of a retro that was already running at the close (its
// completion is taken on that entry), 0 for every other row. EndedAt, Outcome and EndedBy are set
// together when it ends.
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
// that the run's retro was held (P1). It does not say the retro was after a close.
func (r RunRetro) RetroHad() bool { return false }

// AfterClose is whether the row carries a retro Legion opened after the issue's close.
func (r RunRetro) AfterClose() bool { return false }

// Open is whether the retro after the close is open: opened and not ended. The pod gate, the
// linger and the completion route all ask it.
func (r RunRetro) Open() bool { return false }

// MarkRunStarted records that the run (issue, generation) of tree started at at, keeping the
// earliest start the row already holds. Its callers are the workflow's transition out of
// `admitted` and each ancestor of the issue that moved.
func (s *Postgres) MarkRunStarted(ctx context.Context, tx pgx.Tx, issue string, generation uint64, tree string, at time.Time) error {
	return errRunRetroNotImplemented
}

// MarkRetroStarted records that a retro step of the run started at at, keeping the first start
// the row already holds. It is written in the transaction that writes the retro's step start.
func (s *Postgres) MarkRetroStarted(ctx context.Context, tx pgx.Tx, issue string, generation uint64, tree string, at time.Time) error {
	return errRunRetroNotImplemented
}

// OpenRetroAfterClose opens the retro after the issue's close on the run's row: openedAt, the
// bound it ends at, the claim token of its agent and the entry of a retro already running at the
// close (0 otherwise). It answers the row it wrote.
func (s *Postgres) OpenRetroAfterClose(ctx context.Context, tx pgx.Tx, issue string, generation uint64, tree string,
	openedAt, boundAt time.Time, agent claim.Token, entry int64) (RunRetro, error) {
	return RunRetro{}, errRunRetroNotImplemented
}

// EndRetroAfterClose ends the open retro after a close with outcome and cause. It answers the
// row it ended and true, or the zero row and false where the retro is not open (already ended,
// or never opened), which a repeat of the same end is.
func (s *Postgres) EndRetroAfterClose(ctx context.Context, tx pgx.Tx, issue string, generation uint64,
	endedAt time.Time, outcome RetroOutcome, cause RetroEndCause) (RunRetro, bool, error) {
	return RunRetro{}, false, errRunRetroNotImplemented
}

// RunRetro is the row of the issue's run generation, nil for none.
func (s *Postgres) RunRetro(ctx context.Context, tx pgx.Tx, issue string, generation uint64) (*RunRetro, error) {
	return nil, errRunRetroNotImplemented
}

// RunRetros is every row of the issue, newest run first.
func (s *Postgres) RunRetros(ctx context.Context, tx pgx.Tx, issue string) ([]RunRetro, error) {
	return nil, errRunRetroNotImplemented
}

// OpenRetroOfIssue is the issue's open retro after a close, nil for none.
func (s *Postgres) OpenRetroOfIssue(ctx context.Context, tx pgx.Tx, issue string) (*RunRetro, error) {
	return nil, errRunRetroNotImplemented
}

// OpenRetrosOfTree is every open retro after a close of the tree's issues, by bound, then by
// issue.
func (s *Postgres) OpenRetrosOfTree(ctx context.Context, tx pgx.Tx, tree string) ([]RunRetro, error) {
	return nil, errRunRetroNotImplemented
}

// CarryRunRetro copies the run's start and its retro's start from generation fromGen to toGen of
// the same issue, in toTree: a carried orphan's run is its new tree's from then on, and the bump
// of its generation must not lose the facts P1 reads.
func (s *Postgres) CarryRunRetro(ctx context.Context, tx pgx.Tx, issue string, fromGen, toGen uint64, toTree string) error {
	return errRunRetroNotImplemented
}
