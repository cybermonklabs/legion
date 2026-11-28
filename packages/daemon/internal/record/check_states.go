package record

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// CheckState is the newest state of one check at one head of an issue that the daemon took from
// Dispatch's check.state_changed (intake.DispatchCheckState): its state (`yes`, `no` or
// `unknown`), why it is not yes, the roles whose pass counts there, the `ci.case` of a check CI
// proves, and Seq, the sequence of the event in the issue's Dispatch event log, which is what makes
// it the newest.
type CheckState struct {
	Issue, Repo, Head, Check, State, Reason, CICase string
	Roles                                           []string
	Seq                                             int64 // the Dispatch event's sequence
}

// HeadStates is what the daemon holds of an issue's checks at one head: the head's set of checks,
// nil where no event has told it yet and empty where the head has no check, and the newest state of
// each check it holds one of, by check id.
type HeadStates struct {
	Checks []string              // nil: no set held yet
	States map[string]CheckState // by check id
}

func (s *Postgres) PutCheckState(ctx context.Context, tx pgx.Tx, state CheckState) (bool, error) {
	roles := state.Roles
	if roles == nil {
		roles = []string{}
	}
	tag, err := tx.Exec(ctx, `insert into check_states (issue, repo, head, check_id, state, reason, roles, ci_case, dispatch_seq)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		on conflict (issue, repo, head, check_id) do update set state = excluded.state, reason = excluded.reason,
			roles = excluded.roles, ci_case = excluded.ci_case, dispatch_seq = excluded.dispatch_seq
		where check_states.dispatch_seq < excluded.dispatch_seq`,
		state.Issue, state.Repo, state.Head, state.Check, state.State, state.Reason, roles, state.CICase, state.Seq)
	if err != nil {
		return false, fmt.Errorf("put the state of check %s of %s at %s of %s at sequence %d: %w", state.Check, state.Issue, state.Head, state.Repo, state.Seq, err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Postgres) PutCheckSet(ctx context.Context, tx pgx.Tx, issue, repo, head string, checks []string, seq int64) (bool, error) {
	if checks == nil {
		checks = []string{}
	}
	tag, err := tx.Exec(ctx, `insert into check_state_sets (issue, repo, head, checks, dispatch_seq) values ($1, $2, $3, $4, $5)
		on conflict (issue, repo, head) do update set checks = excluded.checks, dispatch_seq = excluded.dispatch_seq
		where check_state_sets.dispatch_seq < excluded.dispatch_seq`,
		issue, repo, head, checks, seq)
	if err != nil {
		return false, fmt.Errorf("put the set of checks of %s at %s of %s at sequence %d: %w", issue, head, repo, seq, err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Postgres) HeadStates(ctx context.Context, tx pgx.Tx, issue, repo, head string) (HeadStates, error) {
	held := HeadStates{States: map[string]CheckState{}}
	err := tx.QueryRow(ctx, `select checks from check_state_sets where issue = $1 and repo = $2 and head = $3`,
		issue, repo, head).Scan(&held.Checks)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		held.Checks = nil
	case err != nil:
		return HeadStates{}, fmt.Errorf("read the set of checks of %s at %s of %s: %w", issue, head, repo, err)
	case held.Checks == nil:
		held.Checks = []string{}
	}
	rows, err := tx.Query(ctx, `select check_id, state, reason, roles, ci_case, dispatch_seq from check_states
		where issue = $1 and repo = $2 and head = $3`, issue, repo, head)
	if err != nil {
		return HeadStates{}, fmt.Errorf("read the check states of %s at %s of %s: %w", issue, head, repo, err)
	}
	state := CheckState{Issue: issue, Repo: repo, Head: head}
	if _, err := pgx.ForEachRow(rows, []any{&state.Check, &state.State, &state.Reason, &state.Roles, &state.CICase, &state.Seq}, func() error {
		if state.Roles == nil {
			state.Roles = []string{}
		}
		held.States[state.Check] = state
		return nil
	}); err != nil {
		return HeadStates{}, fmt.Errorf("read the check states of %s at %s of %s: %w", issue, head, repo, err)
	}
	return held, nil
}

func (s *Postgres) DeleteCheckStates(ctx context.Context, tx pgx.Tx, issue string) error {
	if _, err := tx.Exec(ctx, `with states as (delete from check_states where issue = $1),
		sets as (delete from check_state_sets where issue = $1)
		delete from check_state_serials where issue = $1`, issue); err != nil {
		return fmt.Errorf("delete the check states of %s: %w", issue, err)
	}
	return nil
}

// PutCheckStateSerial records that the daemon applied the issue's state event of serial.
func (s *Postgres) PutCheckStateSerial(ctx context.Context, tx pgx.Tx, issue string, serial int64) error {
	if serial < 1 {
		return fmt.Errorf("put the check states serial %d of %s: a serial counts from 1", serial, issue)
	}
	if _, err := tx.Exec(ctx, `insert into check_state_serials (issue, serial) values ($1, $2) on conflict do nothing`,
		issue, serial); err != nil {
		return fmt.Errorf("put the check states serial %d of %s: %w", serial, issue, err)
	}
	return nil
}

// StatesFenceReached: true where serial is 0, or where the daemon holds serial and every serial
// from the lowest it holds of the issue through serial. A serial below the lowest it holds is
// reached too: its event came before the first the daemon applied.
func (s *Postgres) StatesFenceReached(ctx context.Context, tx pgx.Tx, issue string, serial int64) (bool, error) {
	if serial <= 0 {
		return true, nil
	}
	var lowest *int64
	var through int64
	if err := tx.QueryRow(ctx, `select min(serial), count(*) filter (where serial <= $2) from check_state_serials where issue = $1`,
		issue, serial).Scan(&lowest, &through); err != nil {
		return false, fmt.Errorf("read the check states serials of %s through %d: %w", issue, serial, err)
	}
	switch {
	case lowest == nil:
		return false, nil
	case serial < *lowest:
		return true, nil
	default:
		return through == serial-*lowest+1, nil
	}
}
