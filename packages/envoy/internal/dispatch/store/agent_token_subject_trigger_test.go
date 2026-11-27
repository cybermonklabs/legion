package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestTheDatabaseRefusesAnyChangeOfAPersonalTokensAccount: the trigger on agent_tokens (migration
// 20261117160116's, folded into 20261118041916's agent_tokens_binding_columns) refuses every
// update that changes the Google account a personal token recorded at its mint
// (agent_tokens.subject), whatever statement sends it, with SQLSTATE 23514: a plain update, an
// aliased one, `update only`, a row-constructor assignment, taking the account off, a backfill of
// tokens recording none, and an insert's conflict update. The account restated passes. The
// census (TestEveryBindingColumnMovesOnlyAsItsWritersMoveIt) holds the token's other columns and
// the writes that leave the account as it was; this test holds the statement shapes.
//
// Control, in the test: with the trigger dropped, every one of those writes succeeds, so it is the
// trigger that refuses them.
func TestTheDatabaseRefusesAnyChangeOfAPersonalTokensAccount(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	for _, token := range []struct{ name, subject string }{{"recording-a", "account-a"}, {"recording-none", ""}} {
		hash := sha256.Sum256([]byte(token.name))
		if _, err := database.Pool.Exec(ctx, `insert into agent_tokens (owner, name, token_hash, prefix, subject)
			values ('carol@example.com', $1, $2, 'test', nullif($3, ''))`, token.name, hash[:], token.subject); err != nil {
			t.Fatalf("seed the token %s: %v", token.name, err)
		}
	}
	changes := []struct{ name, statement string }{
		{"a plain update", `update agent_tokens set subject = 'account-b' where name = 'recording-a'`},
		{"an aliased update", `update agent_tokens as t set subject = 'account-b' where t.name = 'recording-a'`},
		{"update only", `update only agent_tokens set subject = 'account-b' where name = 'recording-a'`},
		{"a row-constructor assignment", `update agent_tokens set (name, subject) = ('recording-a', 'account-b') where name = 'recording-a'`},
		{"taking the account off", `update agent_tokens set subject = null where name = 'recording-a'`},
		{"a backfill of tokens recording none", `update agent_tokens set subject = 'someone' where subject is null`},
		{"an insert's conflict update", `insert into agent_tokens (owner, name, token_hash, prefix, subject)
			select owner, name, token_hash, prefix, 'account-b' from agent_tokens where name = 'recording-a'
			on conflict (token_hash) do update set subject = excluded.subject`},
	}
	attempt := func(statement string) error {
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		_, err = tx.Exec(ctx, statement)
		return err
	}
	for _, change := range changes {
		var pgError *pgconn.PgError
		if err := attempt(change.statement); !errors.As(err, &pgError) || pgError.Code != "23514" {
			t.Errorf("%s answered %v, want it refused with SQLSTATE 23514: a token's recorded account never changes", change.name, err)
		}
	}
	if err := attempt(`update agent_tokens set subject = subject`); err != nil {
		t.Errorf("the account restated, which leaves the recorded account as it was, answered %v, want it to pass", err)
	}

	if _, err := database.Pool.Exec(ctx, `drop trigger agent_tokens_binding_columns on agent_tokens`); err != nil {
		t.Fatalf("drop the trigger for the control: %v", err)
	}
	for _, change := range changes {
		if err := attempt(change.statement); err != nil {
			t.Errorf("control: with the trigger dropped, %s answered %v, want it to pass, so the test would not see what refuses it", change.name, err)
		}
	}
}

// TestTheDatabaseStampsAndGuardsTheRecordingTime: the people trigger (migration 20261117175133's
// rule, kept by 20261118041916's people_binding_columns) makes people.sub_recorded_at the
// database's: an insert or an update that records or changes sub stamps it from
// clock_timestamp(), whatever time the statement gave, and moving it with sub restated is refused
// with SQLSTATE 23514. A write that leaves both as they were passes and moves nothing. The census
// (TestEveryBindingColumnMovesOnlyAsItsWritersMoveIt) holds the time moved back and set to null.
//
// Control, in the test: with the trigger dropped, moving the time with sub restated passes, and
// sub changed alone leaves the time where it was.
func TestTheDatabaseStampsAndGuardsTheRecordingTime(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	recordedAt := func(email string) *time.Time {
		t.Helper()
		var at *time.Time
		if err := database.Pool.QueryRow(ctx, `select sub_recorded_at from people where email = $1`, email).Scan(&at); err != nil {
			t.Fatalf("read %s's recording time: %v", email, err)
		}
		return at
	}
	exec := func(statement string) error {
		_, err := database.Pool.Exec(ctx, statement)
		return err
	}
	refused := func(err error) bool {
		var pgError *pgconn.PgError
		return errors.As(err, &pgError) && pgError.Code == "23514"
	}
	ancient := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

	if err := exec(`insert into people (email, sub, sub_recorded_at) values ('recorded@example.com', 'account-a', '2000-01-01')`); err != nil {
		t.Fatalf("insert a row recording an account: %v", err)
	}
	stamped := recordedAt("recorded@example.com")
	if stamped == nil || !stamped.After(ancient) {
		t.Fatalf("an insert recording an account and naming the time 2000-01-01 stored %v, want the database's own time", stamped)
	}
	if err := exec(`insert into people (email, sub_recorded_at) values ('none@example.com', '2000-01-01')`); err != nil {
		t.Fatalf("insert a row recording no account: %v", err)
	}
	if got := recordedAt("none@example.com"); got != nil {
		t.Errorf("an insert recording no account and naming a time stored %s, want none", got)
	}
	if err := exec(`update people set sub = sub, sub_recorded_at = '2000-01-01' where email = 'recorded@example.com'`); !refused(err) {
		t.Errorf("moving the time with sub restated answered %v, want it refused with SQLSTATE 23514", err)
	}
	if err := exec(`update people set member_kind = member_kind, sub = sub where email = 'recorded@example.com'`); err != nil {
		t.Errorf("a write leaving sub and its time as they were answered %v, want it to pass", err)
	}
	if got := recordedAt("recorded@example.com"); got == nil || !got.Equal(*stamped) {
		t.Errorf("a write leaving sub as it was moved the recording time from %s to %v", stamped, got)
	}
	if err := exec(`update people set sub = 'account-c' where email = 'recorded@example.com'`); err != nil {
		t.Fatalf("change sub alone: %v", err)
	}
	moved := recordedAt("recorded@example.com")
	if moved == nil || !moved.After(*stamped) {
		t.Errorf("changing sub alone left the recording time %v, want it later than %s", moved, stamped)
	}
	if err := exec(`update people set sub = 'account-d', sub_recorded_at = '2000-01-01' where email = 'recorded@example.com'`); err != nil {
		t.Fatalf("change sub naming a time: %v", err)
	}
	if got := recordedAt("recorded@example.com"); got == nil || !got.After(*moved) {
		t.Errorf("changing sub while naming the time 2000-01-01 stored %v, want the database's time, later than %s", got, moved)
	}

	if err := exec(`drop trigger people_binding_columns on people`); err != nil {
		t.Fatalf("drop the trigger for the control: %v", err)
	}
	if err := exec(`update people set sub = sub, sub_recorded_at = '2000-01-01' where email = 'recorded@example.com'`); err != nil {
		t.Errorf("control: with the trigger dropped, moving the time with sub restated answered %v, want it to pass", err)
	}
	before := recordedAt("recorded@example.com")
	if err := exec(`update people set sub = 'account-e' where email = 'recorded@example.com'`); err != nil {
		t.Fatal(err)
	}
	if got := recordedAt("recorded@example.com"); got == nil || before == nil || !got.Equal(*before) {
		t.Errorf("control: with the trigger dropped, changing sub alone moved the time from %v to %v, want it left, so the test would not see what stamps it", before, got)
	}
}
