package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// P0BoundMode is what one change of the P0 bound made while the daemon runs does
// (dispatch://INFRA-5636 C3), as migrations/20261125181828_p0_bound.up.sql's mode column holds it.
type P0BoundMode string

const (
	// P0BoundSet sets the bound to the change's number.
	P0BoundSet P0BoundMode = "bound"
	// P0BoundUnbounded sets the bound to none, whatever legion.yaml's admission_p0_bound says.
	P0BoundUnbounded P0BoundMode = "unbounded"
	// P0BoundCleared takes the live value away, which returns the project to legion.yaml's.
	P0BoundCleared P0BoundMode = "cleared"
)

// P0BoundChange is one change of the P0 bound the project's controller or the operator made while
// the daemon runs: what it did (Mode), the number a set names (Bound, 0 for the other two modes),
// who made it, in a pause's actor shape, why and when. Project is the project's Dispatch key. ID is
// the row's, 0 before the row is written.
type P0BoundChange struct {
	ID      int64
	Project string
	Mode    P0BoundMode
	Bound   int
	By      PauseActor
	Reason  string
	At      time.Time
}

// Stands is whether the change is a live value that replaces legion.yaml's: a set of a number or
// of none. A clear is no live value.
func (c P0BoundChange) Stands() bool { return c.Mode == P0BoundSet || c.Mode == P0BoundUnbounded }

// EffectiveP0Bound is the P0 bound a pass admits under: how many P0 issues may hold a slot at once,
// or nil for no bound. It is the live value where one stands — newest, the project's newest change
// (P0BoundTx), found false where none was ever made — and configured, legion.yaml's
// admission_p0_bound (nil where the file gives none), where none stands.
func EffectiveP0Bound(configured *int, newest P0BoundChange, found bool) *int {
	if !found || !newest.Stands() {
		return configured
	}
	if newest.Mode == P0BoundUnbounded {
		return nil
	}
	return new(newest.Bound)
}

// p0BoundColumns is a change row as scanP0BoundChange reads it.
const p0BoundColumns = `id, project, mode, coalesce(bound, 0), by_kind, by_session, reason, changed_at`

func scanP0BoundChange(row pgx.Row) (P0BoundChange, error) {
	var c P0BoundChange
	var mode, kind string
	if err := row.Scan(&c.ID, &c.Project, &mode, &c.Bound, &kind, &c.By.Session, &c.Reason, &c.At); err != nil {
		return P0BoundChange{}, err
	}
	c.Mode, c.By.Kind, c.At = P0BoundMode(mode), PauseActorKind(kind), c.At.UTC()
	return c, nil
}

// P0BoundTx is, through tx, project's newest change of the P0 bound, and whether one was ever
// made. project is the Dispatch key.
func P0BoundTx(ctx context.Context, tx pgx.Tx, project string) (P0BoundChange, bool, error) {
	c, err := scanP0BoundChange(tx.QueryRow(ctx, `select `+p0BoundColumns+` from project_p0_bound_changes
		where project = $1 order by id desc limit 1`, project))
	if errors.Is(err, pgx.ErrNoRows) {
		return P0BoundChange{}, false, nil
	}
	if err != nil {
		return P0BoundChange{}, false, fmt.Errorf("read the P0 bound of %s: %w", project, err)
	}
	return c, true, nil
}

// RecordP0BoundChangeTx appends change through tx and answers it with its id. It refuses a mode
// the table does not hold, a set of a number below 1 or above 2147483647 (the most the bound's
// integer column holds), a number on a change that sets none, a reason that is only white space
// and an actor of neither shape before the table's checks would.
func RecordP0BoundChangeTx(ctx context.Context, tx pgx.Tx, change P0BoundChange) (P0BoundChange, error) {
	var bound *int
	switch change.Mode {
	case P0BoundSet:
		if change.Bound < 1 || change.Bound > math.MaxInt32 {
			return P0BoundChange{}, fmt.Errorf("set the P0 bound of %s to %d: a bound is from 1 to 2147483647", change.Project, change.Bound)
		}
		bound = &change.Bound
	case P0BoundUnbounded, P0BoundCleared:
		if change.Bound != 0 {
			return P0BoundChange{}, fmt.Errorf("change the P0 bound of %s (%s): only a set names a number, and this one names %d",
				change.Project, change.Mode, change.Bound)
		}
	default:
		return P0BoundChange{}, fmt.Errorf("change the P0 bound of %s: %q is no change of it", change.Project, change.Mode)
	}
	if strings.TrimSpace(change.Reason) == "" {
		return P0BoundChange{}, fmt.Errorf("change the P0 bound of %s: a change says why it is made", change.Project)
	}
	if err := change.By.validate(); err != nil {
		return P0BoundChange{}, fmt.Errorf("change the P0 bound of %s: %w", change.Project, err)
	}
	if err := tx.QueryRow(ctx, `insert into project_p0_bound_changes (project, mode, bound, by_kind, by_session, reason, changed_at)
		values ($1, $2, $3, $4, $5, $6, $7) returning id`,
		change.Project, string(change.Mode), bound, string(change.By.Kind), change.By.Session, change.Reason, change.At).Scan(&change.ID); err != nil {
		return P0BoundChange{}, fmt.Errorf("change the P0 bound of %s: %w", change.Project, err)
	}
	return change, nil
}
