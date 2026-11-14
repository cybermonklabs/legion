package record

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// claimWaitColumns are claim_waits' columns under the alias w, in the order waitScan reads them.
// Every read of a wait names the table as w, the claim read's join (readRoleClaims) among them,
// so the one list serves them all.
const claimWaitColumns = `w.id, w.project, w.claim_token, w.issue, w.role, w.generation, w.phase,
	w.phase_entry, w.taken_entry, w.session, w.waiting_on, w.wake_at, w.declared_at,
	w.ended_at, w.end_cause, w.ended_by`

// DeclareWait ends the claim's wait that has no end, as replaced by wait, and inserts wait, in
// the caller's transaction (Store.DeclareWait). The end comes first because the partial unique
// index claim_waits_standing refuses a second row of one claim with no end.
func (s *Postgres) DeclareWait(ctx context.Context, tx pgx.Tx, wait ClaimWait) (*ClaimWait, error) {
	switch {
	case wait.ID == "" || wait.Project == "" || wait.Token == "":
		return nil, errors.New("declare a wait: the wait's id, project and claim are required")
	case strings.TrimSpace(wait.WaitingOn) == "":
		return nil, fmt.Errorf("declare wait %s of %s: what it waits on is required", wait.ID, wait.Token)
	case wait.WakeAt.IsZero() || wait.DeclaredAt.IsZero():
		return nil, fmt.Errorf("declare wait %s of %s: its time and its declaration time are required", wait.ID, wait.Token)
	case wait.Generation > maxInt64:
		return nil, fmt.Errorf("declare wait %s of %s: generation %d does not fit a bigint", wait.ID, wait.Token, wait.Generation)
	}
	replaced, err := scanClaimWait(tx.QueryRow(ctx, `update claim_waits as w
		set ended_at = $3, end_cause = $4, ended_by = $5
		where w.project = $1 and w.claim_token = $2 and w.ended_at is null
		returning `+claimWaitColumns,
		wait.Project, string(wait.Token), wait.DeclaredAt, WaitReplaced, wait.ID))
	if err != nil {
		return nil, fmt.Errorf("declare wait %s of %s: end the wait it replaces: %w", wait.ID, wait.Token, err)
	}
	if _, err := tx.Exec(ctx, `insert into claim_waits (id, project, claim_token, issue, role, generation, phase,
		phase_entry, taken_entry, session, waiting_on, wake_at, declared_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		wait.ID, wait.Project, string(wait.Token), wait.Issue, string(wait.Role), int64(wait.Generation), string(wait.Phase),
		wait.PhaseEntry, wait.TakenEntry, wait.Session, wait.WaitingOn, wait.WakeAt, wait.DeclaredAt); err != nil {
		return nil, fmt.Errorf("declare wait %s of %s: %w", wait.ID, wait.Token, err)
	}
	return replaced, nil
}

// EndWait ends the wait waitID of the claim token in project, if it has no end yet
// (Store.EndWait). The condition on the id is what makes an end that names a replaced wait
// change nothing, and the condition on ended_at what keeps an ended wait's first end.
func (s *Postgres) EndWait(ctx context.Context, tx pgx.Tx, project string, token claim.Token, waitID, cause, endedBy string, at time.Time) (bool, error) {
	if cause == "" || endedBy == "" || at.IsZero() {
		return false, fmt.Errorf("end wait %s of %s: its cause, the fact that ends it and its time are required", waitID, token)
	}
	tag, err := tx.Exec(ctx, `update claim_waits set ended_at = $4, end_cause = $5, ended_by = $6
		where project = $1 and claim_token = $2 and id = $3 and ended_at is null`,
		project, string(token), waitID, at, cause, endedBy)
	if err != nil {
		return false, fmt.Errorf("end wait %s of %s: %w", waitID, token, err)
	}
	return tag.RowsAffected() == 1, nil
}

// StandingWait is the claim's wait row that has no end, nil for none (Store.StandingWait).
func (s *Postgres) StandingWait(ctx context.Context, tx pgx.Tx, token claim.Token) (*ClaimWait, error) {
	wait, err := scanClaimWait(tx.QueryRow(ctx, `select `+claimWaitColumns+` from claim_waits w
		where w.claim_token = $1 and w.ended_at is null`, string(token)))
	if err != nil {
		return nil, fmt.Errorf("read the standing wait of %s: %w", token, err)
	}
	return wait, nil
}

// StandingWaits is every wait row of project's claims that has no end, by token
// (Store.StandingWaits). The partial index claim_waits_standing holds exactly these rows.
func (s *Postgres) StandingWaits(ctx context.Context, tx pgx.Tx, project string) ([]ClaimWait, error) {
	rows, err := tx.Query(ctx, `select `+claimWaitColumns+` from claim_waits w
		where w.project = $1 and w.ended_at is null order by w.claim_token`, project)
	if err != nil {
		return nil, fmt.Errorf("read the standing waits of %s: %w", project, err)
	}
	defer rows.Close()
	var waits []ClaimWait
	for rows.Next() {
		var scan waitScan
		if err := rows.Scan(scan.dest()...); err != nil {
			return nil, fmt.Errorf("read the standing waits of %s: %w", project, err)
		}
		waits = append(waits, *scan.wait())
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the standing waits of %s: %w", project, err)
	}
	return waits, nil
}

// scanClaimWait reads the one row of claimWaitColumns row holds, nil for none.
func scanClaimWait(row pgx.Row) (*ClaimWait, error) {
	var scan waitScan
	if err := row.Scan(scan.dest()...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return scan.wait(), nil
}

// waitScan is the destination of claimWaitColumns. Every column is read as nullable, because the
// claim read takes them through a left join (readRoleClaims), which matches no row for a claim
// with no wait; the three end columns are null on every wait with no end.
type waitScan struct {
	id, project, token, issue, role, phase, session, waitingOn, endCause, endedBy *string
	generation, phaseEntry, takenEntry                                            *int64
	wakeAt, declaredAt, endedAt                                                   *time.Time
}

// dest is the scan destinations, in claimWaitColumns' order.
func (w *waitScan) dest() []any {
	return []any{&w.id, &w.project, &w.token, &w.issue, &w.role, &w.generation, &w.phase,
		&w.phaseEntry, &w.takenEntry, &w.session, &w.waitingOn, &w.wakeAt, &w.declaredAt,
		&w.endedAt, &w.endCause, &w.endedBy}
}

// wait is the row scanned, nil where the join matched none.
func (w *waitScan) wait() *ClaimWait {
	if w.id == nil {
		return nil
	}
	return &ClaimWait{
		ID:         *w.id,
		Project:    valueOrZero(w.project),
		Token:      claim.Token(valueOrZero(w.token)),
		Issue:      valueOrZero(w.issue),
		Role:       claim.Role(valueOrZero(w.role)),
		Generation: uint64(valueOrZero(w.generation)),
		Phase:      phase.Phase(valueOrZero(w.phase)),
		PhaseEntry: valueOrZero(w.phaseEntry),
		TakenEntry: valueOrZero(w.takenEntry),
		Session:    valueOrZero(w.session),
		WaitingOn:  valueOrZero(w.waitingOn),
		WakeAt:     valueOrZero(w.wakeAt),
		DeclaredAt: valueOrZero(w.declaredAt),
		EndedAt:    valueOrZero(w.endedAt),
		EndCause:   valueOrZero(w.endCause),
		EndedBy:    valueOrZero(w.endedBy),
	}
}

// valueOrZero is what p points to, the zero value for nil.
func valueOrZero[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}
