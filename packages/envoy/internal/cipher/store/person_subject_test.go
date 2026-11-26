package store_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// rowAccount is the account person's sign-in on thumbprint records, the zero value for none.
func rowAccount(t *testing.T, st *store.Store, person, thumbprint string) store.IssuerSubject {
	t.Helper()
	var account store.IssuerSubject
	if err := st.Pool.QueryRow(context.Background(), `select coalesce(issuer, ''), coalesce(subject, '')
		from person_signins where person = $1 and thumbprint = $2`, person, thumbprint).Scan(&account.Issuer, &account.Subject); err != nil {
		t.Fatalf("read the account of %s's sign-in on %s: %v", person, thumbprint, err)
	}
	return account
}

// replace records, in a transaction of its own, that subject no longer holds person's address
// and holder does, as the subject revoke records it, carrying no session generation.
func replace(t *testing.T, st *store.Store, person, subject, holder string) {
	t.Helper()
	ctx := context.Background()
	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := store.LockPerson(ctx, tx, person); err != nil {
		t.Fatalf("LockPerson: %v", err)
	}
	if _, err := store.RecordReplacedSubjectTx(ctx, tx, person, signInIssuer, subject, holder, 1); err != nil {
		t.Fatalf("RecordReplacedSubjectTx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestATradeRecordsTheAccountItsPersonTokenNames: the first trade of a box key records the
// account the person's token names; a row from before migration 0047, which records none, is
// ended by its next trade and replaced by a row of its own, recording that trade's account, so no
// row's account ever changes; and a trade of a box key another account signed in with is
// ErrSignInSubjectChanged, leaving the row as it was.
func TestATradeRecordsTheAccountItsPersonTokenNames(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	mine := accountOf(signInPerson)
	if err := record(t, st, signInPerson, signInBoxOne, signInMachine); err != nil {
		t.Fatalf("the first trade: %v", err)
	}
	if got := rowAccount(t, st, signInPerson, signInBoxOne); got != mine {
		t.Fatalf("the sign-in records the account %+v, want %+v, the one its person token names", got, mine)
	}

	if _, err := st.Pool.Exec(ctx, `insert into person_signins (person, thumbprint, machine_account) values ($1, $2, $3)`,
		signInPerson, signInBoxTwo, signInMachine); err != nil {
		t.Fatalf("write a sign-in from before migration 0047: %v", err)
	}
	legacy := signInFor(t, st, signInPerson, signInBoxTwo)
	if err := record(t, st, signInPerson, signInBoxTwo, signInMachine); err != nil {
		t.Fatalf("the first trade of a sign-in recording no account: %v", err)
	}
	if got := rowAccount(t, st, signInPerson, signInBoxTwo); got != mine {
		t.Fatalf("the older sign-in records %+v after its trade, want the trader's %+v", got, mine)
	}
	if again := signInFor(t, st, signInPerson, signInBoxTwo); again.ID == legacy.ID {
		t.Fatalf("the older sign-in's trade kept the row %s, which recorded no account, want it ended and a row of its own", legacy.ID)
	}

	other := store.IssuerSubject{Issuer: signInIssuer, Subject: "209" + signInPerson}
	for _, box := range []string{signInBoxOne, signInBoxTwo} {
		if err := recordAs(t, st, signInPerson, box, signInMachine, other); !errors.Is(err, store.ErrSignInSubjectChanged) {
			t.Fatalf("another account's trade of the box key %s = %v, want %v", box, err, store.ErrSignInSubjectChanged)
		}
		if got := rowAccount(t, st, signInPerson, box); got != mine {
			t.Fatalf("after the other account's trade the sign-in on %s records %+v, want %+v still", box, got, mine)
		}
	}
	if err := recordAs(t, st, signInPerson, signInBoxOne, signInMachine, store.IssuerSubject{Issuer: "https://issuer.example", Subject: mine.Subject}); !errors.Is(err, store.ErrSignInSubjectChanged) {
		t.Fatalf("the same sub of another issuer = %v, want %v: an account is the issuer and the sub together", err, store.ErrSignInSubjectChanged)
	}
}

// TestATradeOfAReplacedAccountIsRefusedOnEveryBoxKey: once an account is recorded as no longer
// holding a person's address, its trade is ErrSubjectReplaced on a box key it never traded
// and writes nothing, while the holder's trade goes on; the same account is no one's replaced
// account under another address; and a later record naming it as the holder, the address coming
// back, lets it trade again and refuses the account it replaced in turn.
func TestATradeOfAReplacedAccountIsRefusedOnEveryBoxKey(t *testing.T) {
	st := storetest.Open(t)
	replaced := accountOf(signInPerson)
	holder := store.IssuerSubject{Issuer: signInIssuer, Subject: "209" + signInPerson}
	replace(t, st, signInPerson, replaced.Subject, holder.Subject)

	if err := recordAs(t, st, signInPerson, signInBoxOne, signInMachine, replaced); !errors.Is(err, store.ErrSubjectReplaced) {
		t.Fatalf("the replaced account's trade of a new box key = %v, want %v", err, store.ErrSubjectReplaced)
	}
	if live, err := st.PersonSignIns(context.Background(), signInPerson); err != nil || len(live) != 0 {
		t.Fatalf("the refused trade left the sign-ins %+v, %v; want none", live, err)
	}
	if err := recordAs(t, st, signInPerson, signInBoxTwo, signInMachine, holder); err != nil {
		t.Fatalf("the holder's trade: %v", err)
	}
	if err := recordAs(t, st, signInOther, signInBoxThree, signInMachine, replaced); err != nil {
		t.Fatalf("that account's trade under another address: %v, want it recorded: the record is of one address", err)
	}

	replace(t, st, signInPerson, holder.Subject, replaced.Subject)
	if err := recordAs(t, st, signInPerson, signInBoxOne, signInMachine, replaced); err != nil {
		t.Fatalf("the account the address came back to: %v, want it recorded", err)
	}
	if err := recordAs(t, st, signInPerson, signInBoxThree, signInMachine, holder); !errors.Is(err, store.ErrSubjectReplaced) {
		t.Fatalf("the account the address moved away from = %v, want %v", err, store.ErrSubjectReplaced)
	}
}

// The guard on replaced_person_subjects (migration 0047, guard_replaced_person_subjects): a row
// moves only as the revoke moves it, whatever statement is sent. guardPerson's rows are
// guardReplaced and guardOther, each recorded at session generation 5, guardHolder holding the
// address.
const (
	guardPerson   = "guard@example.com"
	guardReplaced = "109guard"
	guardOther    = "309guard"
	guardHolder   = "209guard"
	guardTrigger  = "replaced_person_subjects_guarded"
)

// guardMarker is the statement that names the row of guardPerson and subject in
// store.ReplacedSubjectDeleteSetting, as id (an SQL expression of a transaction id) builds it,
// set for the transaction alone (local) or for the connection.
func guardMarker(id, subject string, local bool) string {
	scope := "true"
	if !local {
		scope = "false"
	}
	return `select set_config('` + store.ReplacedSubjectDeleteSetting + `', jsonb_build_array(` + id + `, '` + guardPerson + `', '` + signInIssuer + `', '` + subject + `')::text, ` + scope + `)`
}

// guardRefusal is one write the revoke never makes: after setup, statement is refused with
// SQLSTATE 23514; with `alter table <table> disable trigger <trigger>` run after setup instead, it
// passes. table is replaced_person_subjects when empty.
type guardRefusal struct {
	name, statement, table, trigger string
	setup                           []string
}

var guardRefusals = []guardRefusal{
	{name: "a delete without the marker",
		statement: `delete from replaced_person_subjects where person = '` + guardPerson + `' and subject = '` + guardReplaced + `'`, trigger: guardTrigger},
	{name: "a delete whose marker names another row",
		setup:     []string{guardMarker("pg_current_xact_id()::text", guardOther, true)},
		statement: `delete from replaced_person_subjects where person = '` + guardPerson + `' and subject = '` + guardReplaced + `'`, trigger: guardTrigger},
	{name: "a truncate", statement: `truncate replaced_person_subjects`, trigger: "replaced_person_subjects_not_truncated"},
	{name: "a change of person",
		statement: `update replaced_person_subjects set person = 'moved@example.com' where subject = '` + guardReplaced + `'`, trigger: guardTrigger},
	{name: "a change of issuer",
		statement: `update replaced_person_subjects set issuer = 'https://issuer.example' where subject = '` + guardReplaced + `'`, trigger: guardTrigger},
	{name: "a change of subject",
		statement: `update replaced_person_subjects set subject = '409guard' where subject = '` + guardReplaced + `'`, trigger: guardTrigger},
	{name: "a lowering of session_generation",
		statement: `update replaced_person_subjects set session_generation = 4 where subject = '` + guardReplaced + `'`, trigger: guardTrigger},
	{name: "a change of replaced_at",
		statement: `update replaced_person_subjects set replaced_at = replaced_at - interval '1 hour' where subject = '` + guardReplaced + `'`, trigger: guardTrigger},
	{name: "a delete of the address's applied generation",
		statement: `delete from person_replacement_generations where person = '` + guardPerson + `'`,
		table:     "person_replacement_generations", trigger: "person_replacement_generations_guarded"},
	{name: "a truncate of the applied generations", statement: `truncate person_replacement_generations`,
		table: "person_replacement_generations", trigger: "person_replacement_generations_not_truncated"},
	{name: "a lowering of the address's applied generation",
		statement: `update person_replacement_generations set session_generation = 4 where person = '` + guardPerson + `'`,
		table:     "person_replacement_generations", trigger: "person_replacement_generations_guarded"},
	{name: "a change of the applied generation's person",
		statement: `update person_replacement_generations set person = 'moved@example.com' where person = '` + guardPerson + `'`,
		table:     "person_replacement_generations", trigger: "person_replacement_generations_guarded"},
}

// isCheckViolation reports whether err is Postgres's SQLSTATE 23514.
func isCheckViolation(err error) bool {
	var pgError *pgconn.PgError
	return errors.As(err, &pgError) && pgError.Code == "23514"
}

// sameGeneration reports whether a and b are one session generation, or both none.
func sameGeneration(a, b *int64) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// generationText is generation by value, "none" for nil.
func generationText(generation *int64) string {
	if generation == nil {
		return "none"
	}
	return strconv.FormatInt(*generation, 10)
}

// TestTheReplacedSubjectRowsMoveOnlyAsTheRevokeMovesThem holds the guards on
// replaced_person_subjects and person_replacement_generations to the revoke's writes, one subtest
// per write, each in a transaction of its own that is rolled back. Every write the revoke never
// makes is refused with SQLSTATE 23514, and its control runs it again with the guard's trigger off
// and sees it pass, so the trigger named is what refuses it: a delete without the marker or with
// one naming another row, a truncate, a change of person, issuer or subject, a session generation
// lowered, and a change of replaced_at; an address's applied generation deleted, truncated,
// lowered or moved to another person; and a marker another transaction holds open, one a committed
// transaction left on the connection (set with is_local false), and one naming the transaction
// before on the same connection let nothing be deleted. The revoke's own writes pass, run through
// RecordReplacedSubjectTx: the insert, a repeat that raises the session generation (and one carrying
// a lower one, which a later replacement superseded, so it writes nothing), and the delete of the
// holder's row.
func TestTheReplacedSubjectRowsMoveOnlyAsTheRevokeMovesThem(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	for _, subject := range []string{guardReplaced, guardOther} {
		tx, err := st.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.RecordReplacedSubjectTx(ctx, tx, guardPerson, signInIssuer, subject, guardHolder, 5); err != nil {
			t.Fatalf("record %s replaced: %v", subject, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// run runs steps in tx, failing the test on any error but the last step's, which it answers.
	run := func(t *testing.T, tx pgx.Tx, steps ...string) error {
		t.Helper()
		for i, step := range steps {
			_, err := tx.Exec(ctx, step)
			if i == len(steps)-1 {
				return err
			}
			if err != nil {
				t.Fatalf("setup %q: %v", step, err)
			}
		}
		return nil
	}
	// rolledBack runs steps in a transaction of its own on the pool, rolled back.
	rolledBack := func(t *testing.T, steps ...string) error {
		t.Helper()
		tx, err := st.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		return run(t, tx, steps...)
	}
	disable := func(table, trigger string) string {
		if table == "" {
			table = "replaced_person_subjects"
		}
		return `alter table ` + table + ` disable trigger ` + trigger
	}
	for _, row := range guardRefusals {
		t.Run("refused/"+row.name, func(t *testing.T) {
			if err := rolledBack(t, append(slices.Clone(row.setup), row.statement)...); !isCheckViolation(err) {
				t.Errorf("%s answered %v, want it refused with SQLSTATE 23514", row.name, err)
			}
			if err := rolledBack(t, append(slices.Clone(row.setup), disable(row.table, row.trigger), row.statement)...); err != nil {
				t.Errorf("control: with %s off, %s answered %v, want it to pass, so that trigger is what refuses it", row.trigger, row.name, err)
			}
		})
	}
	deleteReplaced := `delete from replaced_person_subjects where person = '` + guardPerson + `' and subject = '` + guardReplaced + `'`

	t.Run("refused/a delete another transaction's open marker names", func(t *testing.T) {
		marking, err := st.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer marking.Rollback(ctx)
		if err := run(t, marking, guardMarker("pg_current_xact_id()::text", guardReplaced, true)); err != nil {
			t.Fatalf("the first transaction's marker: %v", err)
		}
		if err := rolledBack(t, deleteReplaced); !isCheckViolation(err) {
			t.Errorf("a second transaction's delete of the row the first, still open, names = %v, want it refused with SQLSTATE 23514", err)
		}
		if err := rolledBack(t, disable("", guardTrigger), deleteReplaced); err != nil {
			t.Errorf("control: with %s off, the second transaction's delete = %v, want it to pass", guardTrigger, err)
		}
	})

	// onOneConnection runs first in a transaction it commits and then second in a transaction it
	// rolls back, both on one connection, answering the error of second's last step; each
	// "<previous>" in second is the first transaction's id. The connection's setting is reset
	// before it goes back to the pool.
	onOneConnection := func(t *testing.T, first []string, second ...string) error {
		t.Helper()
		conn, err := st.Pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		defer conn.Exec(ctx, `reset `+store.ReplacedSubjectDeleteSetting)
		committed, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, step := range first {
			if _, err := committed.Exec(ctx, step); err != nil {
				t.Fatalf("the first transaction's %q: %v", step, err)
			}
		}
		var previous string
		if err := committed.QueryRow(ctx, `select pg_current_xact_id()::text`).Scan(&previous); err != nil {
			t.Fatal(err)
		}
		if err := committed.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		steps := make([]string, len(second))
		for i, step := range second {
			steps[i] = strings.ReplaceAll(step, "<previous>", previous)
		}
		return run(t, tx, steps...)
	}
	// The second transaction reads the marker the first left on the connection, so the refusal
	// below is of a marker the delete sees, not of one already gone.
	leftOn := `do $$ begin if coalesce(current_setting('` + store.ReplacedSubjectDeleteSetting + `', true), '') not like '%"` + guardReplaced + `"]' then
		raise exception 'the first transaction left no marker on the connection'; end if; end $$`
	t.Run("refused/a delete under a marker a committed transaction left on the connection", func(t *testing.T) {
		left := []string{guardMarker("pg_current_xact_id()::text", guardReplaced, false)}
		if err := onOneConnection(t, left, leftOn, deleteReplaced); !isCheckViolation(err) {
			t.Errorf("the next transaction's delete, naming nothing itself = %v, want it refused with SQLSTATE 23514", err)
		}
		if err := onOneConnection(t, left, leftOn, disable("", guardTrigger), deleteReplaced); err != nil {
			t.Errorf("control: with %s off, the next transaction's delete = %v, want it to pass", guardTrigger, err)
		}
	})
	t.Run("refused/a delete under a marker naming the transaction before", func(t *testing.T) {
		previous := guardMarker("'<previous>'", guardReplaced, true)
		if err := onOneConnection(t, nil, previous, deleteReplaced); !isCheckViolation(err) {
			t.Errorf("a delete under a marker carrying the previous transaction's id = %v, want it refused with SQLSTATE 23514", err)
		}
		if err := onOneConnection(t, nil, previous, disable("", guardTrigger), deleteReplaced); err != nil {
			t.Errorf("control: with %s off, the delete = %v, want it to pass", guardTrigger, err)
		}
	})

	// recorded is the session generation the row of subject holds in tx, and whether it exists.
	recorded := func(t *testing.T, tx pgx.Tx, subject string) (*int64, bool) {
		t.Helper()
		var generation *int64
		err := tx.QueryRow(ctx, `select session_generation from replaced_person_subjects where person = $1 and issuer = $2 and subject = $3`,
			guardPerson, signInIssuer, subject).Scan(&generation)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false
		}
		if err != nil {
			t.Fatal(err)
		}
		return generation, true
	}
	// revoked runs write in a transaction it rolls back, then check in that transaction.
	revoked := func(t *testing.T, write func(tx pgx.Tx) error, check func(tx pgx.Tx)) {
		t.Helper()
		tx, err := st.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if err := write(tx); err != nil {
			t.Fatalf("the revoke's own write: %v", err)
		}
		check(tx)
	}
	t.Run("allowed/the insert", func(t *testing.T) {
		revoked(t, func(tx pgx.Tx) error {
			_, err := store.RecordReplacedSubjectTx(ctx, tx, guardPerson, signInIssuer, "409guard", guardHolder, 7)
			return err
		}, func(tx pgx.Tx) {
			if got, ok := recorded(t, tx, "409guard"); !ok || got == nil || *got != 7 {
				t.Errorf("the inserted row holds %v (%t), want generation 7", got, ok)
			}
		})
	})
	for _, repeat := range []struct {
		name    string
		carried int64
		want    int64
		// superseded is the generation the revoke answers it was superseded by, nil when it applies.
		superseded *int64
	}{
		{"a repeat that raises session_generation", 9, 9, nil},
		{"a repeat carrying a lower session_generation, which a later replacement superseded, so it writes nothing", 3, 5, new(int64(5))},
	} {
		t.Run("allowed/"+repeat.name, func(t *testing.T) {
			revoked(t, func(tx pgx.Tx) error {
				supersededBy, err := store.RecordReplacedSubjectTx(ctx, tx, guardPerson, signInIssuer, guardReplaced, guardHolder, repeat.carried)
				if err == nil && !sameGeneration(supersededBy, repeat.superseded) {
					t.Errorf("%s answered superseded by %s, want %s", repeat.name, generationText(supersededBy), generationText(repeat.superseded))
				}
				return err
			}, func(tx pgx.Tx) {
				if got, ok := recorded(t, tx, guardReplaced); !ok || got == nil || *got != repeat.want {
					t.Errorf("after %s the row holds %v (%t), want generation %d", repeat.name, got, ok, repeat.want)
				}
			})
		})
	}
	t.Run("allowed/the delete of the holder's row", func(t *testing.T) {
		revoked(t, func(tx pgx.Tx) error {
			_, err := store.RecordReplacedSubjectTx(ctx, tx, guardPerson, signInIssuer, guardHolder, guardReplaced, 11)
			return err
		}, func(tx pgx.Tx) {
			if _, ok := recorded(t, tx, guardReplaced); ok {
				t.Errorf("the address came back to %s, and its row is still there", guardReplaced)
			}
			if got, ok := recorded(t, tx, guardHolder); !ok || got == nil || *got != 11 {
				t.Errorf("the account the address moved from holds %v (%t), want generation 11", got, ok)
			}
		})
	})
}

// TestThePersonSubjectsMigrationHoldsAnAccountWholeOrNotAtAll: a sign-in and a machine login record an
// account as an issuer and a `sub` together, never one without the other and never an empty one,
// so a row either names one account or plainly names none; and a replaced account is recorded
// under a lowercase login, as every login the broker compares is.
func TestThePersonSubjectsMigrationHoldsAnAccountWholeOrNotAtAll(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	for _, c := range []struct {
		name, statement string
		args            []any
	}{
		{"a sign-in with an issuer and no sub", `insert into person_signins (person, thumbprint, machine_account, issuer) values ($1, $2, $3, $4)`,
			[]any{signInPerson, signInBoxOne, signInMachine, signInIssuer}},
		{"a sign-in with a sub and no issuer", `insert into person_signins (person, thumbprint, machine_account, subject) values ($1, $2, $3, $4)`,
			[]any{signInPerson, signInBoxOne, signInMachine, "109"}},
		{"a sign-in with an empty sub", `insert into person_signins (person, thumbprint, machine_account, issuer, subject) values ($1, $2, $3, $4, '')`,
			[]any{signInPerson, signInBoxOne, signInMachine, signInIssuer}},
		{"a machine login with an approving issuer and no sub", `insert into launcher_credentials (id, operator, host, key_thumbprint, public_jwk, expires_at, approver_issuer)
			values (gen_random_uuid(), $1, 'example-host', $2, '{}'::jsonb, now() + interval '1 hour', $3)`, []any{signInPerson, signInBoxOne, signInIssuer}},
		{"a machine login with an empty approving issuer", `insert into launcher_credentials (id, operator, host, key_thumbprint, public_jwk, expires_at, approver_issuer, approver_subject)
			values (gen_random_uuid(), $1, 'example-host', $2, '{}'::jsonb, now() + interval '1 hour', '', '109')`, []any{signInPerson, signInBoxOne}},
		{"a replaced account under a login in capitals", `insert into replaced_person_subjects (person, issuer, subject, session_generation) values ('Ada@Example.com', $1, '109', 1)`,
			[]any{signInIssuer}},
	} {
		if _, err := st.Pool.Exec(ctx, c.statement, c.args...); err == nil {
			t.Errorf("%s was written, want Postgres to refuse it", c.name)
		}
	}
	// The same rows naming a whole account, or none, are written: what the checks refuse is half
	// an account and an empty one, and nothing else about these statements.
	for _, c := range []struct {
		name, statement string
		args            []any
	}{
		{"a sign-in with a whole account", `insert into person_signins (person, thumbprint, machine_account, issuer, subject) values ($1, $2, $3, $4, '109')`,
			[]any{signInPerson, signInBoxOne, signInMachine, signInIssuer}},
		{"a sign-in with none", `insert into person_signins (person, thumbprint, machine_account) values ($1, $2, $3)`,
			[]any{signInPerson, signInBoxTwo, signInMachine}},
		{"a machine login with a whole approving account", `insert into launcher_credentials (id, operator, host, key_thumbprint, public_jwk, expires_at, approver_issuer, approver_subject)
			values (gen_random_uuid(), $1, 'example-host', $2, '{}'::jsonb, now() + interval '1 hour', $3, '109')`, []any{signInPerson, signInBoxOne, signInIssuer}},
		{"a replaced account under a lowercase login", `insert into replaced_person_subjects (person, issuer, subject, session_generation) values ('ada@example.com', $1, '109', 1)`,
			[]any{signInIssuer}},
	} {
		if _, err := st.Pool.Exec(ctx, c.statement, c.args...); err != nil {
			t.Errorf("%s was refused (%v), want it written", c.name, err)
		}
	}
}

// TestAReplacedAccountWithNoGenerationIsRefused: every revoke carries a session generation, so
// replaced_person_subjects.session_generation is not null (migration 0047): a row inserted with
// none is refused with SQLSTATE 23502 (not_null_violation), and nothing is written.
//
// Control, in the test: with the column's not null dropped, in a transaction rolled back, the same
// insert is written.
func TestAReplacedAccountWithNoGenerationIsRefused(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	const insert = `insert into replaced_person_subjects (person, issuer, subject) values ('nogen@example.com', $1, '109')`
	var pgError *pgconn.PgError
	if _, err := st.Pool.Exec(ctx, insert, signInIssuer); !errors.As(err, &pgError) || pgError.Code != "23502" {
		t.Errorf("a replaced account inserted with no generation answered %v, want it refused with SQLSTATE 23502", err)
	}
	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `alter table replaced_person_subjects alter column session_generation drop not null`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, insert, signInIssuer); err != nil {
		t.Errorf("control: with not null dropped, the insert answered %v; want it written, so not null is what refuses it", err)
	}
}
