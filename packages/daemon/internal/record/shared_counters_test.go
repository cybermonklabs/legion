package record

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// The three counters this release adds to tables the release before also writes,
// issues.check_states_seq, phases.states_serial and phases.return_to, are nullable with a default
// of 0. A migration that a rolling release runs adds only tables, nullable columns and non-unique
// indexes, so the release before still runs on the migrated database during a roll or a rollback.
// A counter that holds no value reads as 0, the value its default gives, the cursor's raise lifts
// it from no value, and each column still refuses a negative number.
func TestTheCountersAddedToSharedTablesAreNullableAndReadNoValueAsZero(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	issue := issueFixture("LEGION-286")
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issue))
		must(t, records.PutPhase(ctx, tx, PhaseRow{Issue: issue.Key, Role: claim.RoleTester, Claim: "legion-286-tester", StatesSerial: 5, ReturnTo: 2}))
		must(t, records.SetCheckStatesSeq(ctx, tx, issue.Key, 7))
	})
	columns := []struct{ table, column string }{{"issues", "check_states_seq"}, {"phases", "states_serial"}, {"phases", "return_to"}}
	for _, c := range columns {
		var nullable, fallback string
		must(t, st.Pool().QueryRow(ctx, `select is_nullable::text, coalesce(column_default, '')::text from information_schema.columns
			where table_schema = current_schema() and table_name = $1 and column_name = $2`, c.table, c.column).Scan(&nullable, &fallback))
		if nullable != "YES" || fallback != "0" {
			t.Fatalf("%s.%s: is_nullable %q, default %q; want YES and 0", c.table, c.column, nullable, fallback)
		}
	}
	if _, err := st.Pool().Exec(ctx, `update issues set check_states_seq = null where key = $1`, issue.Key); err != nil {
		t.Fatalf("clear the issue's cursor: %v", err)
	}
	if _, err := st.Pool().Exec(ctx, `update phases set states_serial = null, return_to = null where issue = $1`, issue.Key); err != nil {
		t.Fatalf("clear the tester's serial and step: %v", err)
	}
	inTx(t, st, func(tx pgx.Tx) {
		cursors, err := records.CheckStatesSeqs(ctx, tx, []string{issue.Key})
		must(t, err)
		if seq, ok := cursors[issue.Key]; !ok || seq != 0 {
			t.Fatalf("the cursor with no value = %d (held %v), want 0", seq, ok)
		}
		phases, err := records.Phases(ctx, tx, issue.Key)
		must(t, err)
		if len(phases) != 1 || phases[0].StatesSerial != 0 || phases[0].ReturnTo != 0 {
			t.Fatalf("the phases with no serial and no step = %#v, want the tester's row at serial 0 and step 0", phases)
		}
		must(t, records.SetCheckStatesSeq(ctx, tx, issue.Key, 3))
	})
	inTx(t, st, func(tx pgx.Tx) {
		cursors, err := records.CheckStatesSeqs(ctx, tx, []string{issue.Key})
		must(t, err)
		if seq := cursors[issue.Key]; seq != 3 {
			t.Fatalf("the cursor raised from no value = %d, want 3", seq)
		}
	})
	if _, err := st.Pool().Exec(ctx, `update issues set check_states_seq = -1 where key = $1`, issue.Key); err == nil {
		t.Fatal("issues.check_states_seq took -1, want its check to refuse it")
	}
	if _, err := st.Pool().Exec(ctx, `update phases set states_serial = -1 where issue = $1`, issue.Key); err == nil {
		t.Fatal("phases.states_serial took -1, want its check to refuse it")
	}
	if _, err := st.Pool().Exec(ctx, `update phases set return_to = -1 where issue = $1`, issue.Key); err == nil {
		t.Fatal("phases.return_to took -1, want its check to refuse it")
	}
}
