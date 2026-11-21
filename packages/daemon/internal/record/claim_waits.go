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

// StaleWaits is every wait row of project's claims that has no end and whose record has moved
// under it since its declaration (Store.StaleWaits), each read beside its issue, its tree root, its
// role's phases row and its claim by the one statement, so the reconciler that runs on every fact
// (workflow.EndStaleWaits) costs one query however many waits stand. The standing rows are found
// through claim_waits_standing, and each join is by a primary key.
//
// The filter keeps a row when any column a term of the rule over a wait reads (workflow.WaitStands)
// no longer reads as the declaration recorded or required it: the issue's phase or generation, the
// issue's status out of the workflow (OutOfWorkflowStatuses, handed to the statement rather than
// spelled in it, as store's UnownedPods is handed it), the root's linger, the phases row's entry or
// a completion recorded on it, or the claim's session. The term that the phase is the role's own
// needs no column of its own: the declaration required it, and it changes only with the phase. Nor
// does the design gate's hold, which the gate's closing ends where it closes (workflow's
// putGateTold). A row the filter keeps is only a candidate: which term failed, and so whether and
// how the wait ends, is workflow.WaitEndCause's to say.
//
// completed_at is never null: a row with no completion holds the zero time PutPhase writes for
// it, which is the column's default too, so the filter compares it with that zero rather than with
// null. A role with no phases row reads as the zero row, on entry 0 with no completion
// (PhaseRowOf).
func (s *Postgres) StaleWaits(ctx context.Context, tx pgx.Tx, project string) ([]WaitReading, error) {
	rows, err := tx.Query(ctx, `select `+claimWaitColumns+`,
		i.key, i.tree, i.phase, i.generation, i.status, i.linger_until, r.key, r.tree, r.linger_until,
		p.entry, p.completed_at, c.session
		from claim_waits w
		join claims c on c.token = w.claim_token and c.project = w.project
		join issues i on i.key = w.issue
		join issues r on r.key = i.tree
		left join phases p on p.issue = w.issue and p.role = w.role
		where w.project = $1 and w.ended_at is null
		and (i.phase <> w.phase or i.generation <> w.generation or i.status = any($2) or r.linger_until is not null
			or coalesce(p.entry, 0) <> w.phase_entry or p.completed_at <> $3 or c.session <> w.session)
		order by w.claim_token`, project, OutOfWorkflowStatuses, time.Time{})
	if err != nil {
		return nil, fmt.Errorf("read the stale waits of %s: %w", project, err)
	}
	defer rows.Close()
	var readings []WaitReading
	for rows.Next() {
		var wait waitScan
		var reading WaitReading
		var issuePhase string
		var generation int64
		var entry *int64
		var completed *time.Time
		if err := rows.Scan(append(wait.dest(), &reading.Issue.Key, &reading.Issue.Tree, &issuePhase, &generation,
			&reading.Issue.Status, &reading.Issue.LingerUntil, &reading.Root.Key, &reading.Root.Tree, &reading.Root.LingerUntil,
			&entry, &completed, &reading.Claim.Session)...); err != nil {
			return nil, fmt.Errorf("read the stale waits of %s: %w", project, err)
		}
		reading.Claim.Wait = wait.wait()
		reading.Issue.Phase, reading.Issue.Generation = phase.Phase(issuePhase), uint64(generation)
		reading.Row = PhaseRow{Issue: reading.Issue.Key, Role: reading.Claim.Wait.Role, Entry: valueOrZero(entry),
			CompletedAt: valueOrZero(completed)}
		readings = append(readings, reading)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the stale waits of %s: %w", project, err)
	}
	return readings, nil
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
