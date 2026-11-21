package store

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// bindingColumnsVersion is the migration that holds the binding's columns to their writers.
const bindingColumnsVersion = 20261118041916

// TestCensusRefusesTheBindingColumnsOverAStoredEmptyAccount takes 20261118041916's census with the
// shipped file over a database at the version before it, as a deploy carrying it does: rows
// recording an account and none answer 0 and pass, and one row holding the empty string as its
// account, the one stored value the new check refuses at its next write, answers 1 and refuses the
// deploy. No writer of Dispatch stores one, so the test writes it directly, which that schema
// allows.
func TestCensusRefusesTheBindingColumnsOverAStoredEmptyAccount(t *testing.T) {
	ctx := context.Background()
	database := openEmptyTestStore(t)
	if err := database.migrate(ctx, migrationsThrough(t, bindingColumnsVersion-1)); err != nil {
		t.Fatalf("migrate through the version before %d: %v", bindingColumnsVersion, err)
	}
	take := func() (*pgmigrate.Report, int64) {
		t.Helper()
		report, err := census(ctx, database.Pool.Config().ConnString(), migrationsThrough(t, bindingColumnsVersion), pgmigrate.CensusOptions{})
		if err != nil {
			t.Fatalf("census: %v", err)
		}
		if len(report.Pending) != 1 || report.Pending[0].Migration.Version != bindingColumnsVersion || report.Pending[0].Count == nil {
			t.Fatalf("want one pending migration, %d, whose census answered:\n%s", bindingColumnsVersion, reportText(report))
		}
		return report, *report.Pending[0].Count
	}
	if _, err := database.Pool.Exec(ctx, `insert into people (email, sub) values ('recorded@x.example', 'account-a'), ('none@x.example', null)`); err != nil {
		t.Fatalf("insert rows recording an account and none: %v", err)
	}
	if report, count := take(); count != 0 || report.Refused() {
		t.Fatalf("rows recording an account and none: census counts %d, refusals %v; want 0 and none:\n%s", count, refusals(report), reportText(report))
	}
	if _, err := database.Pool.Exec(ctx, `insert into people (email, sub) values ('empty@x.example', '')`); err != nil {
		t.Fatalf("insert a row holding the empty string as its account: %v", err)
	}
	report, count := take()
	const want = "20261118041916_binding_columns_write_once.up.sql: its census counts 1 (20261118041916_binding_columns_write_once.census.sql)"
	if got := refusals(report); count != 1 || len(got) != 1 || !strings.HasPrefix(got[0], want) {
		t.Fatalf("a row holding the empty string: census counts %d, refusals %v; want 1 and one refusal starting %q:\n%s", count, got, want, reportText(report))
	}
}

// The census of the account binding's columns (migrations 20261118041916 and 20261118152422, the
// second for broker_revocations): every column a predicate of the binding reads (the session
// check, the personal and broker token admissions, the mint, the approve's re-read and the
// membership answer) or the broker's refusal of an approve checked before a replacement reads, the
// transitions its writers make, and the guard that refuses every other.
// Each refusal row names the guard, and its control turns that guard off in the same setup and sees
// the statement pass; each effect row's control sees the effect go. Every row runs in a transaction
// of its own that is rolled back, so no row's write reaches the next, and a control's dropped
// constraint or disabled trigger reaches no other row.

// bindingRefusal is one transition no writer makes: after setup, statement is refused with
// SQLSTATE code; with disableGuard (the statement that drops or turns off the guard) run after
// setup instead, it passes.
type bindingRefusal struct {
	column, transition string
	setup              []string
	statement          string
	code               string
	disableGuard       string
}

// bindingToken is a personal token recording account-a, minted by r@x.example.
const bindingToken = `insert into agent_tokens (owner, name, token_hash, prefix, subject) values ('r@x.example', 'token', '\x01'::bytea, 'test', 'account-a')`

// bindingRevocationHolder is the address r@x.example recording account-b, which took it from
// account-a, and bindingRevocation is its sign-in's recording of the revocation of account-a,
// naming account-b the holder: the writer's own statement (recordBrokerRevocationSQL) with its
// values written in, so a writer's change is the census's too. On no row it records the
// revocation, owed, at the address's session generation as it stands, which it reads in its own
// statement; run again, for an account replaced again, it records it again under the next
// generation, owed, at the generation the address holds then.
const (
	bindingRevocationHolder = `insert into people (email, sub) values ('r@x.example', 'account-b')`
	bindingRevocationGuard  = `alter table broker_revocations disable trigger broker_revocations_binding_columns`
)

var bindingRevocation = writtenIn(recordBrokerRevocationSQL, `'r@x.example'`, `'https://accounts.google.com'`, `'account-a'`, `'account-b'`)

// bindingFloor is the floor the runbook's recovery writes for p@x.example, the generation the
// secrets broker recorded for the address, where a session row of it made later starts;
// bindingFloorGuard turns off the guard that holds it (migration 20261119060414).
const (
	bindingFloor      = `insert into session_generation_floors (login, generation) values ('p@x.example', 9)`
	bindingFloorGuard = `alter table session_generation_floors disable trigger session_generation_floors_guarded`
)

// writtenIn is statement, a writer's own parameterized statement, with each $n replaced by
// values[n-1], an SQL literal, the highest first so $1 never rewrites the start of $10. A
// statement naming a parameter values does not fill keeps its $n, which Postgres refuses, so a
// writer that takes a new parameter fails its census row rather than passing with a guess.
func writtenIn(statement string, values ...string) string {
	for n := len(values); n >= 1; n-- {
		statement = strings.ReplaceAll(statement, "$"+strconv.Itoa(n), values[n-1])
	}
	return statement
}

// bindingDirectoryRow is the address i@x.example recording account-a; bindingDirectoryActive,
// bindingDirectoryHeld, bindingDirectoryFailed and bindingDirectoryRebound are the writes of an
// active, a Held, a Failed and a rebound Directory outcome for it: the writers' own statements
// (recordDirectoryActiveSQL answered by the Directory user directory-a, recordDirectoryHeldSQL,
// recordDirectoryFailedSQL, recordDirectoryReboundSQL answered by directory-b), with their values
// written in, so a writer's change is the census's too. bindingDirectoryReadAt is the check's read
// of the row the Active write and the rebind rest on: later than any hold a row's setup records, so
// each writes and releases the hold rather than passing by writing nothing.
const (
	bindingDirectoryRow    = `insert into people (email, sub) values ('i@x.example', 'account-a')`
	bindingDirectoryEmail  = `'i@x.example'`
	bindingDirectoryReadAt = `'9999-12-31T00:00:00Z'::timestamptz`
)

var (
	bindingDirectoryActive  = writtenIn(recordDirectoryActiveSQL, bindingDirectoryEmail, `'directory-a'`, bindingDirectoryReadAt)
	bindingDirectoryHeld    = writtenIn(recordDirectoryHeldSQL, bindingDirectoryEmail)
	bindingDirectoryFailed  = writtenIn(recordDirectoryFailedSQL, bindingDirectoryEmail)
	bindingDirectoryRebound = writtenIn(recordDirectoryReboundSQL, bindingDirectoryEmail, `'directory-b'`, bindingDirectoryReadAt)
)

var bindingRefusals = []bindingRefusal{
	{"people.email", "renamed, on a row recording no account",
		[]string{`insert into people (email) values ('p@x.example')`},
		`update people set email = 'q@x.example' where email = 'p@x.example'`,
		"23514", `alter table people disable trigger people_binding_columns`},
	{"people.email", "renamed, on a row a session names, the column trigger off",
		[]string{`insert into people (email) values ('p@x.example')`, `insert into user_sessions (login) values ('p@x.example')`, `alter table people disable trigger people_binding_columns`},
		`update people set email = 'q@x.example' where email = 'p@x.example'`,
		"23503", `alter table user_sessions drop constraint user_sessions_login_fkey`},
	{"people.sub", "taken off: an account -> null",
		[]string{`insert into people (email, sub) values ('r@x.example', 'account-a')`},
		`update people set sub = null where email = 'r@x.example'`,
		"23514", `alter table people disable trigger people_binding_columns`},
	{"people.sub", "taken off naming a time",
		[]string{`insert into people (email, sub) values ('r@x.example', 'account-a')`},
		`update people set sub = null, sub_recorded_at = now() where email = 'r@x.example'`,
		"23514", `alter table people disable trigger people_binding_columns`},
	{"people.sub", "an account -> the empty string",
		[]string{`insert into people (email, sub) values ('r@x.example', 'account-a')`},
		`update people set sub = '' where email = 'r@x.example'`,
		"23514", `alter table people drop constraint people_sub_not_empty`},
	{"people.sub", "null -> the empty string",
		[]string{`insert into people (email) values ('p@x.example')`},
		`update people set sub = '' where email = 'p@x.example'`,
		"23514", `alter table people drop constraint people_sub_not_empty`},
	{"people.sub", "inserted as the empty string",
		nil,
		`insert into people (email, sub) values ('e@x.example', '')`,
		"23514", `alter table people drop constraint people_sub_not_empty`},
	{"people.sub_recorded_at", "moved back",
		[]string{`insert into people (email, sub) values ('r@x.example', 'account-a')`},
		`update people set sub_recorded_at = '2000-01-01' where email = 'r@x.example'`,
		"23514", `alter table people disable trigger people_binding_columns`},
	{"people.sub_recorded_at", "set to null",
		[]string{`insert into people (email, sub) values ('r@x.example', 'account-a')`},
		`update people set sub_recorded_at = null where email = 'r@x.example'`,
		"23514", `alter table people disable trigger people_binding_columns`},
	{"people.confirmed_at", "set later than now",
		[]string{`insert into people (email) values ('p@x.example')`},
		`update people set refresh_token = 'sealed', confirmed_at = now() + interval '1 day' where email = 'p@x.example'`,
		"23514", `alter table people disable trigger people_binding_columns`},
	{"people.confirmed_at", "inserted later than now",
		nil,
		`insert into people (email, refresh_token, confirmed_at) values ('e@x.example', 'sealed', now() + interval '1 day')`,
		"23514", `alter table people disable trigger people_binding_columns`},
	// Its first recording makes no session here, so the key cannot refuse the delete too.
	{"people row", "deleted while it records an account (no session names it)",
		[]string{`alter table people disable trigger people_first_account_ends_sessions`, `insert into people (email, sub) values ('r@x.example', 'account-a')`},
		`delete from people where email = 'r@x.example'`,
		"23514", `alter table people disable trigger people_recorded_account_kept`},
	{"people row", "deleted while a session names it, recording no account",
		[]string{`insert into people (email) values ('p@x.example')`, `insert into user_sessions (login) values ('p@x.example')`},
		`delete from people where email = 'p@x.example'`,
		"23503", `alter table user_sessions drop constraint user_sessions_login_fkey`},
	{"people row", "truncated, the key to it dropped",
		[]string{`insert into people (email) values ('p@x.example')`, `alter table user_sessions drop constraint user_sessions_login_fkey`},
		`truncate people`,
		"23514", `alter table people disable trigger people_not_truncated`},
	{"people row", "truncated, its trigger off",
		[]string{`insert into people (email) values ('p@x.example')`, `alter table people disable trigger people_not_truncated`},
		`truncate people`,
		"0A000", `alter table user_sessions drop constraint user_sessions_login_fkey`},
	{"user_sessions.generation", "moved back",
		[]string{`insert into people (email) values ('p@x.example')`, `insert into user_sessions (login, generation) values ('p@x.example', 5)`},
		`update user_sessions set generation = 4 where login = 'p@x.example'`,
		"23514", `alter table user_sessions disable trigger user_sessions_binding_columns`},
	{"user_sessions row", "deleted",
		[]string{`insert into people (email) values ('p@x.example')`, `insert into user_sessions (login, generation) values ('p@x.example', 5)`},
		`delete from user_sessions where login = 'p@x.example'`,
		"23514", `alter table user_sessions disable trigger user_sessions_binding_columns`},
	{"user_sessions row", "truncated",
		[]string{`insert into people (email) values ('p@x.example')`, `insert into user_sessions (login) values ('p@x.example')`},
		`truncate user_sessions`,
		"23514", `alter table user_sessions disable trigger user_sessions_not_truncated`},
	{"user_sessions.login", "moved to another address people holds",
		[]string{`insert into people (email) values ('p@x.example'), ('q@x.example')`, `insert into user_sessions (login) values ('p@x.example')`},
		`update user_sessions set login = 'q@x.example' where login = 'p@x.example'`,
		"23514", `alter table user_sessions disable trigger user_sessions_binding_columns`},
	{"user_sessions.login", "inserted for an address people does not hold",
		nil,
		`insert into user_sessions (login) values ('nobody@x.example')`,
		"23503", `alter table user_sessions drop constraint user_sessions_login_fkey`},
	{"agent_tokens.revoked_at", "a revoke undone",
		[]string{bindingToken, `update agent_tokens set revoked_at = now()`},
		`update agent_tokens set revoked_at = null`,
		"23514", `alter table agent_tokens disable trigger agent_tokens_binding_columns`},
	{"agent_tokens.revoked_at", "a revoke moved",
		[]string{bindingToken, `update agent_tokens set revoked_at = now()`},
		`update agent_tokens set revoked_at = revoked_at + interval '1 day'`,
		"23514", `alter table agent_tokens disable trigger agent_tokens_binding_columns`},
	{"agent_tokens.owner", "moved to another address",
		[]string{bindingToken},
		`update agent_tokens set owner = 'q@x.example'`,
		"23514", `alter table agent_tokens disable trigger agent_tokens_binding_columns`},
	{"agent_tokens.token_hash", "changed",
		[]string{bindingToken},
		`update agent_tokens set token_hash = '\x02'::bytea`,
		"23514", `alter table agent_tokens disable trigger agent_tokens_binding_columns`},
	{"agent_tokens.id", "changed",
		[]string{bindingToken},
		`update agent_tokens set id = gen_random_uuid()`,
		"23514", `alter table agent_tokens disable trigger agent_tokens_binding_columns`},
	{"agent_tokens.subject", "changed",
		[]string{bindingToken},
		`update agent_tokens set subject = 'account-b'`,
		"23514", `alter table agent_tokens disable trigger agent_tokens_binding_columns`},
	{"agent_tokens.created_at", "moved",
		[]string{bindingToken},
		`update agent_tokens set created_at = now() + interval '1 day'`,
		"23514", `alter table agent_tokens disable trigger agent_tokens_binding_columns`},
	{"broker_revocations row", "deleted while owed",
		[]string{bindingRevocationHolder, bindingRevocation},
		`delete from broker_revocations where email = 'r@x.example'`,
		"23514", bindingRevocationGuard},
	{"broker_revocations row", "truncated",
		[]string{bindingRevocationHolder, bindingRevocation},
		`truncate broker_revocations`,
		"23514", `alter table broker_revocations disable trigger broker_revocations_not_truncated`},
	{"broker_revocations.email", "moved to another address",
		[]string{bindingRevocationHolder, bindingRevocation},
		`update broker_revocations set email = 'q@x.example'`,
		"23514", bindingRevocationGuard},
	{"broker_revocations.subject", "rewritten",
		[]string{bindingRevocationHolder, bindingRevocation},
		`update broker_revocations set subject = 'account-c'`,
		"23514", bindingRevocationGuard},
	{"broker_revocations.holder_subject", "rewritten in place",
		[]string{bindingRevocationHolder, bindingRevocation},
		`update broker_revocations set holder_subject = 'account-c'`,
		"23514", bindingRevocationGuard},
	{"broker_revocations.holder_subject", "inserted naming a holder people does not record",
		[]string{bindingRevocationHolder},
		`insert into broker_revocations (email, issuer, subject, holder_subject) values ('r@x.example', 'https://accounts.google.com', 'account-a', 'account-c')`,
		"23514", bindingRevocationGuard},
	{"broker_revocations.generation", "recorded again naming a holder people does not record",
		[]string{bindingRevocationHolder, bindingRevocation},
		`update broker_revocations set generation = generation + 1, holder_subject = 'account-c', done_at = null`,
		"23514", bindingRevocationGuard},
	{"broker_revocations.generation", "moved by two",
		[]string{bindingRevocationHolder, bindingRevocation},
		`update broker_revocations set generation = generation + 2`,
		"23514", bindingRevocationGuard},
	{"broker_revocations.done_at", "a settlement undone",
		[]string{bindingRevocationHolder, bindingRevocation, `update broker_revocations set done_at = now()`},
		`update broker_revocations set done_at = null`,
		"23514", bindingRevocationGuard},
	{"broker_revocations.done_at", "a settlement moved",
		[]string{bindingRevocationHolder, bindingRevocation, `update broker_revocations set done_at = now()`},
		`update broker_revocations set done_at = done_at - interval '1 day'`,
		"23514", bindingRevocationGuard},
	{"broker_revocations.tried_at", "set later than now",
		[]string{bindingRevocationHolder, bindingRevocation},
		`update broker_revocations set tried_at = now() + interval '1 day'`,
		"23514", bindingRevocationGuard},
	{"broker_revocations.session_generation", "changed outside a recording",
		[]string{bindingRevocationHolder, bindingRevocation},
		`update broker_revocations set session_generation = session_generation + 1`,
		"23514", bindingRevocationGuard},
	{"broker_revocations.session_generation", "set to null outside a recording",
		[]string{bindingRevocationHolder, bindingRevocation},
		`update broker_revocations set session_generation = null`,
		"23514", bindingRevocationGuard},
	{"broker_revocations.session_generation", "moved by a settlement",
		[]string{bindingRevocationHolder, bindingRevocation},
		`update broker_revocations set done_at = now(), session_generation = session_generation - 1`,
		"23514", bindingRevocationGuard},
	{"broker_revocations.session_generation", "inserted naming another generation than the address's",
		[]string{bindingRevocationHolder},
		`insert into broker_revocations (email, issuer, subject, holder_subject, session_generation) values ('r@x.example', 'https://accounts.google.com', 'account-a', 'account-b', 99)`,
		"23514", bindingRevocationGuard},
	{"broker_revocations.session_generation", "inserted naming none while the address holds one",
		[]string{bindingRevocationHolder},
		`insert into broker_revocations (email, issuer, subject, holder_subject) values ('r@x.example', 'https://accounts.google.com', 'account-a', 'account-b')`,
		"23514", bindingRevocationGuard},
	{"broker_revocations.session_generation", "recorded again naming another generation than the address's",
		[]string{bindingRevocationHolder, bindingRevocation, `update broker_revocations set done_at = now()`},
		`update broker_revocations set generation = generation + 1, done_at = null, session_generation = 99`,
		"23514", bindingRevocationGuard},
	{"broker_revocations.session_generation", "recorded again keeping the generation of an earlier recording",
		[]string{bindingRevocationHolder, bindingRevocation, `update broker_revocations set done_at = now()`,
			`update user_sessions set generation = generation + 1 where login = 'r@x.example'`},
		`update broker_revocations set generation = generation + 1, done_at = null`,
		"23514", bindingRevocationGuard},
	// A recording again makes the revocation owed; a write that moves the generation and settles
	// the row in one is neither a recording nor a settlement of what is owed, and the loop would
	// never pay the generation it names (the mutation M28 of the same-family review of
	// cybermonklabs/legion#4199 dropped the `or new.done_at is not null` that refuses it).
	{"broker_revocations.generation", "recorded again and settled in the same write",
		[]string{bindingRevocationHolder, bindingRevocation, `update broker_revocations set done_at = now()`},
		`update broker_revocations set generation = generation + 1, recorded_at = clock_timestamp(), done_at = now()`,
		"23514", bindingRevocationGuard},
	{"broker_revocations.generation", "recorded again on an owed row and settled in the same write",
		[]string{bindingRevocationHolder, bindingRevocation},
		`update broker_revocations set generation = generation + 1, recorded_at = clock_timestamp(), done_at = now()`,
		"23514", bindingRevocationGuard},
	{"session_generation_floors.generation", "lowered",
		[]string{bindingFloor},
		`update session_generation_floors set generation = 4 where login = 'p@x.example'`,
		"23514", bindingFloorGuard},
	{"session_generation_floors.login", "moved to another address",
		[]string{bindingFloor},
		`update session_generation_floors set login = 'q@x.example' where login = 'p@x.example'`,
		"23514", bindingFloorGuard},
	{"session_generation_floors row", "deleted",
		[]string{bindingFloor},
		`delete from session_generation_floors where login = 'p@x.example'`,
		"23514", bindingFloorGuard},
	{"session_generation_floors row", "truncated",
		[]string{bindingFloor},
		`truncate session_generation_floors`,
		"23514", `alter table session_generation_floors disable trigger session_generation_floors_not_truncated`},
}

// bindingTransition is one transition a writer makes, which every guard lets through.
type bindingTransition struct {
	column, transition string
	setup              []string
	statement          string
}

var bindingTransitions = []bindingTransition{
	{"people.sub", "null -> an account (the first recording)",
		[]string{`insert into people (email) values ('p@x.example')`},
		`update people set sub = 'account-a' where email = 'p@x.example'`},
	{"people.sub", "an account -> another (a sign-in under another account)",
		[]string{`insert into people (email, sub) values ('r@x.example', 'account-a')`},
		`update people set sub = 'account-b' where email = 'r@x.example'`},
	{"people.sub", "restated",
		[]string{`insert into people (email, sub) values ('r@x.example', 'account-a')`},
		`update people set sub = 'account-a', signed_in_at = now() where email = 'r@x.example'`},
	{"people.confirmed_at", "confirmed now",
		[]string{`insert into people (email) values ('p@x.example')`},
		`update people set refresh_token = 'sealed', confirmed_at = now() where email = 'p@x.example'`},
	{"people.confirmed_at", "ended",
		[]string{`insert into people (email, refresh_token, confirmed_at) values ('p@x.example', 'sealed', now())`},
		`update people set refresh_token = null, confirmed_at = null where email = 'p@x.example'`},
	{"people.confirmed_at", "the database's clock, read late in its transaction (a confirmation that keeps the session)",
		[]string{`insert into people (email, refresh_token, confirmed_at) values ('p@x.example', 'sealed', now())`, `select pg_sleep(0.05)`},
		`update people set confirmed_at = clock_timestamp() where email = 'p@x.example'`},
	{"people.refresh_token", "replaced by a new sealed token",
		[]string{`insert into people (email, refresh_token, confirmed_at) values ('p@x.example', 'sealed', now())`},
		`update people set refresh_token = refresh_token || '2' where email = 'p@x.example'`},
	{"people.member_kind", "full -> restricted -> full",
		[]string{`insert into people (email, sub) values ('r@x.example', 'account-a')`, `update people set member_kind = 'restricted' where email = 'r@x.example'`},
		`update people set member_kind = 'full' where email = 'r@x.example'`},
	{"people row", "deleted, recording no account and named by no session",
		[]string{`insert into people (email) values ('p@x.example')`},
		`delete from people where email = 'p@x.example'`},
	{"user_sessions.generation", "absent -> 0 (EnsureSession)",
		[]string{`insert into people (email) values ('p@x.example')`},
		`insert into user_sessions (login) values ('p@x.example') on conflict (login) do nothing`},
	{"user_sessions.generation", "n -> n+1 (revokeSessionsSQL)",
		[]string{`insert into people (email) values ('p@x.example')`, `insert into user_sessions (login) values ('p@x.example')`},
		writtenIn(revokeSessionsSQL, `'p@x.example'`)},
	{"agent_tokens.revoked_at", "null -> the revoke's time",
		[]string{bindingToken},
		`update agent_tokens set revoked_at = now() where revoked_at is null`},
	{"agent_tokens.revoked_at", "a revoke restated (the revoke route's coalesce)",
		[]string{bindingToken, `update agent_tokens set revoked_at = now()`},
		`update agent_tokens set revoked_at = coalesce(revoked_at, now())`},
	{"agent_tokens.last_used_at", "a use",
		[]string{bindingToken},
		`update agent_tokens set last_used_at = now()`},
	{"agent_tokens.scopes", "changed by its owner",
		[]string{bindingToken},
		`update agent_tokens set scopes = array['person']`},
	{"people.held_since", "a hold recorded, then released by a sign-in (EndSubject's EndHold, SignInSubject)",
		[]string{`insert into people (email, sub, refresh_token, confirmed_at) values ('h@x.example', 'account-a', 'sealed', now())`,
			writtenIn(endSubjectSQL[auth.EndHold], `'h@x.example'`)},
		`update people set refresh_token = 'sealed', confirmed_at = now(), signed_in_at = now(), held_since = null, signed_out_at = null where email = 'h@x.example'`},
	{"people.signed_out_at", "a sign-out that keeps the token, then forgets it (KeepAfterSignOut, ForgetSignedOut)",
		[]string{`insert into people (email, sub, refresh_token, confirmed_at, signed_in_at) values ('o@x.example', 'account-a', 'sealed', now(), now())`,
			`update people set signed_out_at = clock_timestamp() where email = 'o@x.example'`},
		`update people set refresh_token = null, confirmed_at = null, signed_out_at = null where email = 'o@x.example'`},
	{"people.hosted_domain", "recorded at a confirmation, then forgotten (ConfirmSubject, ForgetHostedDomains)",
		[]string{`insert into people (email, sub, refresh_token, confirmed_at) values ('d@x.example', 'account-a', 'sealed', now())`,
			`update people set confirmed_at = now(), hosted_domain = 'x.example' where email = 'd@x.example'`},
		`update people set hosted_domain = null where hosted_domain is not null`},
	{"people.confirm_tried_at", "claimed by the membership pass (ClaimOverdueConfirmation)",
		[]string{`insert into people (email, sub, refresh_token, confirmed_at) values ('c@x.example', 'account-a', 'sealed', now() - interval '2 hours')`},
		`update people set confirm_tried_at = clock_timestamp() where email = 'c@x.example'`},
	{"people.directory_confirmed_at", "an active Directory answer after a Held one, which releases the hold (RecordDirectoryHeld, then RecordDirectoryActive)",
		[]string{bindingDirectoryRow, bindingDirectoryHeld},
		bindingDirectoryActive},
	{"people.directory_checked_at", "a Failed check after an active answer, as the pass's claim stamps it too (RecordDirectoryFailed, ClaimDirectoryDue)",
		[]string{bindingDirectoryRow, bindingDirectoryActive},
		bindingDirectoryFailed},
	{"people.not_held_until", "fixed at an active answer plus 24 hours, then moved by the next (RecordDirectoryActive)",
		[]string{bindingDirectoryRow, bindingDirectoryActive, `select pg_sleep(0.01)`},
		bindingDirectoryActive},
	{"people.directory_user_id", "stored at the first active answer, then replaced by a rebind of a recreated address (RecordDirectoryActive, RecordDirectoryRebound)",
		[]string{bindingDirectoryRow, bindingDirectoryActive},
		bindingDirectoryRebound},
	{"broker_revocations row", "recorded (the sign-in's insert, naming the holder people records)",
		[]string{bindingRevocationHolder},
		bindingRevocation},
	{"broker_revocations.generation", "recorded again (the sign-in's upsert, for an account replaced again)",
		[]string{bindingRevocationHolder, bindingRevocation, `update broker_revocations set done_at = now()`},
		bindingRevocation},
	{"broker_revocations.session_generation", "recorded again at the address's generation, moved since the first recording (the sign-in's upsert)",
		[]string{bindingRevocationHolder, bindingRevocation, `update broker_revocations set done_at = now()`,
			`update user_sessions set generation = generation + 1 where login = 'r@x.example'`},
		bindingRevocation},
	{"broker_revocations.tried_at", "claimed (a pass's claim)",
		[]string{bindingRevocationHolder, bindingRevocation},
		`update broker_revocations set tried_at = now()`},
	{"broker_revocations.done_at", "settled (the broker's confirmation, a reclaim, the operator's clear)",
		[]string{bindingRevocationHolder, bindingRevocation},
		`update broker_revocations set done_at = now() where done_at is null`},
	{"session_generation_floors row", "written by the runbook's raise (the address's first floor)",
		nil,
		bindingFloor},
	{"session_generation_floors.generation", "raised by a later raise, the broker's generation having moved",
		[]string{bindingFloor},
		`insert into session_generation_floors (login, generation) values ('p@x.example', 11)
			on conflict (login) do update set generation = excluded.generation where excluded.generation > session_generation_floors.generation`},
	{"session_generation_floors.generation", "left where it is by a raise below it",
		[]string{bindingFloor},
		`insert into session_generation_floors (login, generation) values ('p@x.example', 2)
			on conflict (login) do update set generation = excluded.generation where excluded.generation > session_generation_floors.generation`},
}

// bindingEffect is the one change of the account the binding allows, and the end of the address's
// sessions it carries: after setup and statement, p@x.example's session generation is want
// (noSessionRow for none); with disableGuard run after setup instead, it is withoutGuard.
type bindingEffect struct {
	name               string
	setup              []string
	statement          string
	want, withoutGuard int64
	disableGuard       string
}

// noSessionRow is the generation the effect rows read when user_sessions holds no row for the
// address.
const noSessionRow = -1

var bindingEffects = []bindingEffect{
	{"an account recorded on a row recording none ends its sessions",
		[]string{`insert into people (email) values ('p@x.example')`, `insert into user_sessions (login, generation) values ('p@x.example', 3)`},
		`update people set sub = 'account-a' where email = 'p@x.example'`,
		4, 3, `alter table people disable trigger people_account_ends_sessions`},
	{"another account replacing it ends its sessions",
		[]string{`insert into people (email, sub) values ('p@x.example', 'account-a')`, `update user_sessions set generation = 7 where login = 'p@x.example'`},
		`update people set sub = 'account-b' where email = 'p@x.example'`,
		8, 7, `alter table people disable trigger people_account_ends_sessions`},
	{"a fresh row's first account ends the sessions of a row from before the key",
		[]string{`alter table user_sessions drop constraint user_sessions_login_fkey`, `insert into user_sessions (login, generation) values ('p@x.example', 2)`},
		`insert into people (email, sub) values ('p@x.example', 'account-a')`,
		3, 2, `alter table people disable trigger people_first_account_ends_sessions`},
	{"a fresh row's first account, with no session yet, starts at generation 1",
		nil,
		`insert into people (email, sub) values ('p@x.example', 'account-a')`,
		1, noSessionRow, `alter table people disable trigger people_first_account_ends_sessions`},
	{"a session row made where the address has a floor starts at it (a floor written by hand, then EnsureSession)",
		[]string{`insert into people (email) values ('p@x.example')`, bindingFloor},
		`insert into user_sessions (login) values ('p@x.example') on conflict (login) do nothing`,
		9, 0, `alter table user_sessions disable trigger user_sessions_start_at_floor`},
	{"a first account recorded where the address has a floor starts its sessions at the floor, not at 1",
		[]string{bindingFloor},
		`insert into people (email, sub) values ('p@x.example', 'account-a')`,
		9, 1, `alter table user_sessions disable trigger user_sessions_start_at_floor`},
	{"a session row made where the deployment has a floor starts at it (the reset after a lone restore, then a first sign-in)",
		[]string{`insert into session_generation_floors (login, generation) values ('*', 12)`},
		`insert into people (email, sub) values ('p@x.example', 'account-a')`,
		12, 1, `alter table user_sessions disable trigger user_sessions_start_at_floor`},
	{"the greater of the address's floor and the deployment's decides where a new session row starts",
		[]string{`insert into session_generation_floors (login, generation) values ('*', 12)`, bindingFloor},
		`insert into people (email, sub) values ('p@x.example', 'account-a')`,
		12, 1, `alter table user_sessions disable trigger user_sessions_start_at_floor`},
}

// TestEveryBindingColumnMovesOnlyAsItsWritersMoveIt holds the census above: each refused
// transition is refused, by the guard it names, and passes with that guard off; each writer's
// transition passes with every guard on; and recording an account ends the address's sessions,
// which it no longer does with its trigger off.
func TestEveryBindingColumnMovesOnlyAsItsWritersMoveIt(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	// run runs setup and then statement in one transaction it rolls back, and answers the
	// statement's error (a setup error fails the test) after read scans what it reads, if given.
	run := func(t *testing.T, setup []string, statement string, read func(pgx.Tx)) error {
		t.Helper()
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		for _, step := range setup {
			if _, err := tx.Exec(ctx, step); err != nil {
				t.Fatalf("setup %q: %v", step, err)
			}
		}
		if _, err := tx.Exec(ctx, statement); err != nil {
			return err
		}
		if read != nil {
			read(tx)
		}
		return nil
	}
	for _, row := range bindingRefusals {
		t.Run("refused/"+row.column+"/"+row.transition, func(t *testing.T) {
			var pgError *pgconn.PgError
			if err := run(t, row.setup, row.statement, nil); !errors.As(err, &pgError) || pgError.Code != row.code {
				t.Errorf("%s: %s answered %v, want it refused with SQLSTATE %s", row.column, row.transition, err, row.code)
			}
			if err := run(t, append(slices.Clone(row.setup), row.disableGuard), row.statement, nil); err != nil {
				t.Errorf("control: with %q, %s answered %v, want it to pass, so the guard named is what refuses it", row.disableGuard, row.transition, err)
			}
		})
	}
	for _, row := range bindingTransitions {
		t.Run("allowed/"+row.column+"/"+row.transition, func(t *testing.T) {
			if err := run(t, row.setup, row.statement, nil); err != nil {
				t.Errorf("%s: %s, a writer's own transition, answered %v, want it to pass", row.column, row.transition, err)
			}
		})
	}
	generation := func(t *testing.T, tx pgx.Tx) int64 {
		t.Helper()
		var got int64
		err := tx.QueryRow(ctx, `select generation from user_sessions where login = 'p@x.example'`).Scan(&got)
		if errors.Is(err, pgx.ErrNoRows) {
			return noSessionRow
		}
		if err != nil {
			t.Fatalf("read the session generation: %v", err)
		}
		return got
	}
	for _, row := range bindingEffects {
		t.Run("effect/"+row.name, func(t *testing.T) {
			var got int64
			if err := run(t, row.setup, row.statement, func(tx pgx.Tx) { got = generation(t, tx) }); err != nil {
				t.Fatalf("%s: %v", row.name, err)
			}
			if got != row.want {
				t.Errorf("%s: the session generation is %d, want %d", row.name, got, row.want)
			}
			if err := run(t, append(slices.Clone(row.setup), row.disableGuard), row.statement, func(tx pgx.Tx) { got = generation(t, tx) }); err != nil {
				t.Fatalf("control: %s: %v", row.name, err)
			}
			if got != row.withoutGuard {
				t.Errorf("control: with %q, the session generation is %d, want %d, so the trigger named is what ends the sessions", row.disableGuard, got, row.withoutGuard)
			}
		})
	}
}
