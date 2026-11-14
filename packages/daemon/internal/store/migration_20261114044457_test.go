package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// claimWaitsVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const claimWaitsVersion = 20261114044457

// 20261114044457 adds claim_waits, the waits phase workers declare, kept apart from claims so no
// write of a claim's machine touches them (dispatch://INFRA-2040 D1). Before it the table does not
// exist. After it a claim holds one wait with no end at most: a second such row of the same claim
// is refused by claim_waits_standing, while an ended row of that claim and another claim's wait
// with no end each stand beside it. A wait must say what it waits on: a text of spaces alone,
// which btrim leaves empty, is refused.
func TestTheClaimWaitsTableHoldsOneWaitWithNoEndPerClaimAfter20261114044457(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, claimWaitsVersion-1)

	var rows int
	err := store.Pool().QueryRow(ctx, "select count(*) from claim_waits").Scan(&rows)
	if err == nil || !strings.Contains(err.Error(), "claim_waits") {
		t.Fatalf("reading claim_waits before the migration = %v, want the table to be unknown", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the claim waits: %v", err)
	}

	const implementer, tester = "legion-legion-legion-209-implementer", "legion-legion-legion-209-tester"
	insert := func(id, token, waitingOn string, ended bool) error {
		_, err := store.Pool().Exec(ctx, `insert into claim_waits (id, project, claim_token, issue, role, generation, phase,
			phase_entry, taken_entry, session, waiting_on, wake_at, declared_at, ended_at, end_cause, ended_by)
			values ($1, 'legion', $2, 'LEGION-209', 'implementer', 3, 'implementing', 4, 4, 'ses_implementer', $3,
			now() + interval '2 hours', now(),
			case when $4 then now() end, case when $4 then 'replaced' end, case when $4 then 'wait-next' end)`,
			id, token, waitingOn, ended)
		return err
	}
	refusedBy := func(err error, code, constraint string) bool {
		var refused *pgconn.PgError
		return errors.As(err, &refused) && refused.Code == code && refused.ConstraintName == constraint
	}

	if err := insert("wait-1", implementer, "the review of the root's pull request", false); err != nil {
		t.Fatalf("record the implementer's wait: %v", err)
	}
	if err := insert("wait-2", implementer, "a second wait beside the first", false); !refusedBy(err, "23505", "claim_waits_standing") {
		t.Fatalf("a second wait with no end of the same claim = %v, want it refused by claim_waits_standing", err)
	}
	if err := insert("wait-0", implementer, "the wait the first one replaced", true); err != nil {
		t.Fatalf("record an ended wait of the claim beside its wait with no end: %v", err)
	}
	if err := insert("wait-tester", tester, "the proof run", false); err != nil {
		t.Fatalf("record another claim's wait beside the implementer's: %v", err)
	}
	if err := insert("wait-blank", "legion-legion-legion-209-reviewer", "   ", false); !refusedBy(err, "23514", "claim_waits_waiting_on_check") {
		t.Fatalf("a wait whose text is blank once trimmed = %v, want it refused by the table's check", err)
	}

	if err := store.Pool().QueryRow(ctx, "select count(*) from claim_waits").Scan(&rows); err != nil {
		t.Fatalf("count the waits after the migration: %v", err)
	}
	if rows != 3 {
		t.Fatalf("claim_waits holds %d rows, want the implementer's two and the tester's one", rows)
	}
}
