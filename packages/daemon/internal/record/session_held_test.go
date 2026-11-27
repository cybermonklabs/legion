package record

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// SessionHeld answers whether claim_sessions holds every name a claim's record gives its session
// and file for that claim, as RoleHolders reads a holder: true for the claim holding them, false
// for another claim of the same project and for the same claim in another project, and false for a
// claim that records no session.
func TestSessionHeldReadsTheHoldsOfItsClaim(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	holder := claim.Token("legion-legion-LEGION-208-implementer")
	other := claim.Token("legion-legion-LEGION-208-tester")
	const session, file = "ses_holder", "/sessions/ses_holder.jsonl"
	if _, err := st.HoldSessions(ctx, "legion", holder, supervise.RecordedHolds(session, file)); err != nil {
		t.Fatalf("hold %s for %s: %v", session, holder, err)
	}
	for _, tc := range []struct {
		name          string
		project       string
		token         claim.Token
		session, file string
		want          bool
	}{
		{"the claim holding every name", "legion", holder, session, file, true},
		{"another claim recording the same session", "legion", other, session, file, false},
		{"the holder in another project", "acme", holder, session, file, false},
		{"the holder recording a file it does not hold", "legion", holder, session, "/sessions/ses_other.jsonl", false},
		{"a claim recording no session", "legion", holder, "", "", false},
	} {
		inTx(t, st, func(tx pgx.Tx) {
			held, err := SessionHeld(ctx, tx, tc.project, tc.token, tc.session, tc.file)
			if err != nil || held != tc.want {
				t.Errorf("%s: SessionHeld = %t, %v; want %t", tc.name, held, err, tc.want)
			}
		})
	}
}

// A read of SessionHeld that fails inside a fact's transaction leaves that transaction usable: the
// read runs under a savepoint, so its failure rolls back only the savepoint, the fact's next
// statement runs, and the fact commits. stepAgent reads it inside the fact that ends a step, which
// would otherwise be aborted whole by a failed read of a step's agent (25P02 on its next statement).
//
// The control (the pull request records the run): SessionHeld reading on tx itself, with no
// savepoint: "the fact's next statement after the failed read: ERROR: current transaction is
// aborted, commands ignored until end of transaction block (SQLSTATE 25P02)".
func TestAFailedSessionHeldReadLeavesTheFactUsable(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	holder := claim.Token("legion-legion-LEGION-208-implementer")
	if _, err := st.HoldSessions(ctx, "legion", holder, supervise.RecordedHolds("ses_holder", "")); err != nil {
		t.Fatalf("hold ses_holder for %s: %v", holder, err)
	}
	inTx(t, st, func(tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `alter table claim_sessions rename to claim_sessions_away`); err != nil {
			t.Fatalf("rename claim_sessions: %v", err)
		}
	})
	if err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		held, err := SessionHeld(ctx, tx, "legion", holder, "ses_holder", "")
		if err == nil || held {
			t.Errorf("SessionHeld with no claim_sessions table = %t, %v; want false and the read's error", held, err)
		}
		var one int
		if err := tx.QueryRow(ctx, `select 1`).Scan(&one); err != nil {
			t.Fatalf("the fact's next statement after the failed read: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("the fact did not commit after a failed read of a step's agent: %v", err)
	}
}
