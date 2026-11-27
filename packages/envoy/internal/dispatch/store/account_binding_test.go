package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The account binding's fleet (AccountBindingHolds, migration 20261120222130_account_binding_fleet):
// a build without the binding is seen as the owner lock sees a build before it, from its dashboard
// publish, its registration and the mark either stamps, but by account_binding rather than
// owner_lock_protocol, so a build at owner lock protocol 1 that lacks the binding counts too. These
// tests write such a build's rows as it writes them: its columns named, account_binding left out.
// registerFleetProcess (ownerlock_fleet_test.go) writes such a process: it names no
// account_binding.

// accountBindingFleetVersion is migration 20261120222130_account_binding_fleet's version.
const accountBindingFleetVersion = 20261120222130

// registerBoundProcess writes a server_processes row as a process of this build leaves it.
func registerBoundProcess(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.Pool.Exec(context.Background(), `
		insert into server_processes (instance, signing_keys, owner_lock_protocol, account_binding)
		values (gen_random_uuid(), '{fingerprint}', $1, $2)`, OwnerLockProtocol, AccountBindingProtocol); err != nil {
		t.Fatalf("register a process of this build: %v", err)
	}
}

// publishUnbound writes a dashboard_builds row as the boot of a build at owner lock protocol 1
// without the binding publishes it (legion-envoy-v32.20.2's dashboard.Publish), so its insert
// stamps the binding's mark (account_binding_legacy_arrival) and not the owner lock's.
func publishUnbound(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.Pool.Exec(context.Background(), `
		insert into dashboard_builds (build, entry, commit, owner_lock_protocol) values ($1, '', 'unbound0build', 1)
		on conflict (build) do update set last_started_at = now()`, fmt.Sprintf("unbound-%d", time.Now().UnixNano())); err != nil {
		t.Fatalf("publish as a build without the binding: %v", err)
	}
}

// ageBindingMark moves the binding's mark age into the past.
func ageBindingMark(t *testing.T, s *Store, age time.Duration) {
	t.Helper()
	if _, err := s.Pool.Exec(context.Background(),
		`update account_binding_legacy_mark set seen_at = now() - make_interval(secs => $1)`, age.Seconds()); err != nil {
		t.Fatalf("age the binding's mark: %v", err)
	}
}

// hasBindingMark reports whether account_binding_legacy_mark holds its row.
func hasBindingMark(t *testing.T, s *Store) bool {
	t.Helper()
	var marked bool
	if err := s.Pool.QueryRow(context.Background(), `select exists (select from account_binding_legacy_mark)`).Scan(&marked); err != nil {
		t.Fatalf("read the binding's mark: %v", err)
	}
	return marked
}

// bindingHolds reads AccountBindingHolds in a transaction of its own, and the owner lock's own
// fleet read beside it (public.owner_lock_fleet), which a control compares.
func bindingHolds(t *testing.T, s *Store) (holds, ownerLockLegacy bool) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if holds, err = AccountBindingHolds(ctx, tx); err != nil {
		t.Fatal(err)
	}
	legacy, mark := AccountBindingWindows()
	if err := tx.QueryRow(ctx, `select public.owner_lock_fleet($1, $2)`, legacy, mark).Scan(&ownerLockLegacy); err != nil {
		t.Fatal(err)
	}
	return holds, ownerLockLegacy
}

// The binding holds while no build without it is in the fleet: none registered, or only this
// build's processes, a process without it silent past the legacy window, or its mark past the
// mark's window. It does not hold while such a process heartbeated within the legacy window,
// whatever its owner lock protocol, or while its mark is younger than the mark's window, a
// dashboard publish's included: the window from that publish to the registration, in which a boot
// writes before it registers, is what server_processes alone misses.
//
// Control, in the test: for a build at owner lock protocol 1 without the binding, the owner lock's
// own fleet read answers that no earlier build is in the fleet, so a gate on that fence alone
// would start the binding beside it.
func TestTheAccountBindingHoldsOnlyWhileNoBuildWithoutItIsInTheFleet(t *testing.T) {
	s := migratedEmptyStore(t)
	for _, state := range []struct {
		name           string
		seed           func(t *testing.T)
		holds          bool
		ownerLockBlind bool
	}{
		{name: "no process registered", seed: func(*testing.T) {}, holds: true},
		{name: "a live process of this build", seed: func(t *testing.T) { registerBoundProcess(t, s) }, holds: true},
		{name: "a live process at owner lock protocol 1 without the binding", ownerLockBlind: true,
			seed: func(t *testing.T) { registerFleetProcess(t, s, OwnerLockProtocol, 0) }},
		{name: "a live process before the owner lock", seed: func(t *testing.T) { registerFleetProcess(t, s, 0, 0) }},
		{name: "a process without the binding whose heartbeat stopped within the legacy window", ownerLockBlind: true, seed: func(t *testing.T) {
			registerFleetProcess(t, s, OwnerLockProtocol, registryLiveWindow+time.Minute)
			ageBindingMark(t, s, OwnerLockMarkWindow+time.Minute)
		}},
		{name: "a process without the binding whose heartbeat stopped before the legacy window", holds: true, seed: func(t *testing.T) {
			registerFleetProcess(t, s, OwnerLockProtocol, OwnerLockLegacyWindow+time.Minute)
			ageBindingMark(t, s, OwnerLockMarkWindow+time.Minute)
		}},
		{name: "a dashboard publish without the binding just now, before its registration", ownerLockBlind: true,
			seed: func(t *testing.T) { publishUnbound(t, s) }},
		{name: "a dashboard publish without the binding older than the legacy window, within the mark's", ownerLockBlind: true, seed: func(t *testing.T) {
			publishUnbound(t, s)
			ageBindingMark(t, s, OwnerLockLegacyWindow+time.Minute)
		}},
		{name: "a dashboard publish without the binding before the mark's window", holds: true, seed: func(t *testing.T) {
			publishUnbound(t, s)
			ageBindingMark(t, s, OwnerLockMarkWindow+time.Minute)
		}},
	} {
		t.Run(state.name, func(t *testing.T) {
			t.Cleanup(func() {
				_, _ = s.Pool.Exec(context.Background(), `delete from server_processes; delete from dashboard_builds;
					delete from account_binding_legacy_mark; delete from owner_lock_legacy_mark`)
			})
			state.seed(t)
			holds, ownerLockLegacy := bindingHolds(t, s)
			if holds != state.holds {
				t.Errorf("AccountBindingHolds = %t, want %t", holds, state.holds)
			}
			if state.ownerLockBlind && ownerLockLegacy {
				t.Errorf("control: the owner lock's fleet read finds an earlier build here; want it blind to a build at owner lock protocol 1, the case only the binding's fence sees")
			}
		})
	}
}

// The migration stamps the binding's mark at the newest dashboard publish already in
// dashboard_builds when that is within the mark's window, since that publish came before the
// trigger, and every row there reads account_binding 0 then; a publish older than the window, and
// none at all, which is every test database, leave no mark.
func TestTheMigrationMarksABuildWithoutTheBindingThatPublishedBeforeIt(t *testing.T) {
	for _, c := range []struct {
		name    string
		publish bool
		age     time.Duration
		marked  bool
	}{
		{name: "a publish just before the migration", publish: true, marked: true},
		{name: "a publish inside the mark's window", publish: true, age: OwnerLockMarkWindow - time.Minute, marked: true},
		{name: "a publish before the mark's window", publish: true, age: OwnerLockMarkWindow + time.Minute},
		{name: "no publish before the migration"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			s := openEmptyTestStore(t)
			if err := s.migrate(ctx, migrationsThrough(t, accountBindingFleetVersion-1)); err != nil {
				t.Fatalf("migrate through %d: %v", accountBindingFleetVersion-1, err)
			}
			if c.publish {
				if _, err := s.Pool.Exec(ctx, `
					insert into dashboard_builds (build, entry, commit, owner_lock_protocol, last_started_at)
					values ('unbound', '/assets/index-unbound.js', 'unbound0build', 1, now() - make_interval(secs => $1))`, c.age.Seconds()); err != nil {
					t.Fatalf("publish as a build without the binding: %v", err)
				}
			}
			if err := s.migrate(ctx, migrationFiles); err != nil {
				t.Fatalf("migrate from %d: %v", accountBindingFleetVersion, err)
			}
			if marked := hasBindingMark(t, s); marked != c.marked {
				t.Errorf("after the migration the binding's mark is stamped: %t, want %t", marked, c.marked)
			}
			if holds, _ := bindingHolds(t, s); holds == c.marked {
				t.Errorf("after the migration AccountBindingHolds = %t, want %t", holds, !c.marked)
			}
		})
	}
}

// bindingGateWaiter reports whether backend pid waits for the binding's gate exclusively, as an
// arrival of a build without the binding takes it.
func bindingGateWaiter(t *testing.T, s *Store, pid uint32) bool {
	t.Helper()
	var waiting bool
	if err := s.Pool.QueryRow(context.Background(), `
		select exists (select from pg_locks
			where locktype = 'advisory' and database = (select oid from pg_database where datname = current_database())
			  and classid::bigint = $1 and objid::bigint = $2 and objsubid = 2 and pid = $3
			  and mode = 'ExclusiveLock' and not granted)`,
		OwnerLockGateNamespace, AccountBindingGateKey, pid).Scan(&waiting); err != nil {
		t.Fatal(err)
	}
	return waiting
}

// bindingGateHeld reports whether backend pid holds the binding's gate, in any mode.
func bindingGateHeld(t *testing.T, s *Store, pid uint32) bool {
	t.Helper()
	var held bool
	if err := s.Pool.QueryRow(context.Background(), `
		select exists (select from pg_locks
			where locktype = 'advisory' and database = (select oid from pg_database where datname = current_database())
			  and classid::bigint = $1 and objid::bigint = $2 and objsubid = 2 and pid = $3 and granted)`,
		OwnerLockGateNamespace, AccountBindingGateKey, pid).Scan(&held); err != nil {
		t.Fatal(err)
	}
	return held
}

// sendUnboundArrival runs insert, an arrival of a build without the binding, on conn, a
// connection of its own, and delivers its error once it ends.
func sendUnboundArrival(conn *pgx.Conn, insert string) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := conn.Exec(context.Background(), insert)
		done <- err
	}()
	return done
}

// A transaction that found the binding holding keeps its gate shared to its end, so a build
// without the binding that arrives meanwhile waits for it: its dashboard publish's insert queues
// on the gate, uncommitted, and stamps no mark until that transaction ends; a transaction that
// asks while the arrival waits finds the binding not holding, since its try of the gate fails
// behind the queued arrival. Once the first transaction commits, the arrival commits and its mark
// keeps the binding off. A registration's wait is bounded at 2 seconds, then refused 55P03. A
// transaction that finds a build without the binding in the fleet holds no gate at all.
func TestAnArrivalWithoutTheBindingWaitsForATransactionThatFoundItHolding(t *testing.T) {
	s := migratedEmptyStore(t)
	ctx := context.Background()
	holder, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if holds, err := AccountBindingHolds(ctx, holder); err != nil || !holds {
		t.Fatalf("AccountBindingHolds on an empty fleet = %t, %v; want true", holds, err)
	}
	if !bindingGateHeld(t, s, holder.Conn().PgConn().PID()) {
		t.Fatalf("a transaction that found the binding holding holds no binding gate; want it held shared to its end")
	}

	conn, err := pgx.ConnectConfig(ctx, s.Pool.Config().ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	published := sendUnboundArrival(conn, `insert into dashboard_builds (build, entry, commit, owner_lock_protocol) values ('unbound', '', 'unbound0build', 1)`)
	for deadline := time.Now().Add(10 * time.Second); !bindingGateWaiter(t, s, conn.PgConn().PID()); time.Sleep(5 * time.Millisecond) {
		select {
		case err := <-published:
			t.Fatalf("the publish without the binding ended (%v) while a transaction that found the binding holding was open; want it to wait for the gate", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the publish without the binding did not wait for the binding's gate within 10s")
		}
	}
	if hasBindingMark(t, s) {
		t.Error("the binding's mark is in while its arrival waits for the gate; want it uncommitted")
	}
	if holds, _ := bindingHolds(t, s); holds {
		t.Error("a transaction that asks while an arrival waits for the gate finds the binding holding; want its try to fail and answer that a build without it is in the fleet")
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-published:
		if err != nil {
			t.Fatalf("the publish without the binding, once the holder committed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the publish without the binding still waited 10s after the holder committed")
	}
	if !hasBindingMark(t, s) {
		t.Error("the publish without the binding stamped no mark")
	}
	besides, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer besides.Rollback(ctx)
	if holds, err := AccountBindingHolds(ctx, besides); err != nil || holds {
		t.Errorf("AccountBindingHolds after the arrival = %t, %v; want false", holds, err)
	}
	if bindingGateHeld(t, s, besides.Conn().PgConn().PID()) {
		t.Error("a transaction that found a build without the binding in the fleet holds the binding's gate; want none")
	}
	if err := besides.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	// The registration's bound, on a fleet with the mark aged out and a holder again.
	ageBindingMark(t, s, OwnerLockMarkWindow+time.Minute)
	holder, err = s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if holds, err := AccountBindingHolds(ctx, holder); err != nil || !holds {
		t.Fatalf("AccountBindingHolds with the mark aged out = %t, %v; want true", holds, err)
	}
	started := time.Now()
	registered := sendUnboundArrival(conn, `insert into server_processes (instance, signing_keys, owner_lock_protocol) values (gen_random_uuid(), '{fingerprint}', 1)`)
	select {
	case err := <-registered:
		var pgErr *pgconn.PgError
		waited := time.Since(started)
		if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
			t.Errorf("the registration without the binding behind a holder answered %v; want 55P03 lock_not_available", err)
		}
		if waited < 1500*time.Millisecond || waited > 4500*time.Millisecond {
			t.Errorf("the registration without the binding was refused after %s; want about its 2 s bound", waited.Round(time.Millisecond))
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the registration without the binding still waited 20s behind a holder; want it refused at its 2 s bound")
	}
}

// A sign-in beside a build without the binding records the account and moves the recording time,
// as the database's triggers do for any write of sub, and revokes and owes nothing, as that
// build's sign-in does, while the database keeps the replacement (account_binding_replacements);
// once no such build is in the fleet, the next replacement catches the kept one up first (account-a
// is owed, account-b holding the address) and then revokes the address's tokens and owes the
// account it replaces itself, leaving no replacement kept (the control, in the test, that the rows
// it reads can show the binding's writes).
func TestASignInBesideABuildWithoutTheBindingRevokesAndOwesNothing(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := brokerPeople(database)
	const email = "binding-fleet@example.com"
	signInAll(t, people, email, "account-a")
	tokenA := mintToken(t, database.Pool, email, "token-a", "account-a")
	tokenNone := mintToken(t, database.Pool, email, "token-none", "")
	recordedAt := func() time.Time {
		t.Helper()
		var at time.Time
		if err := database.Pool.QueryRow(ctx, `select sub_recorded_at from people where email = $1`, email).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	owed := func() int {
		t.Helper()
		var count int
		if err := database.Pool.QueryRow(ctx, `select count(*) from broker_revocations where email = $1`, email).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	first := recordedAt()

	registerFleetProcess(t, database, OwnerLockProtocol, 0)
	signInAll(t, people, email, "account-b")
	var sub string
	if err := database.Pool.QueryRow(ctx, `select sub from people where email = $1`, email).Scan(&sub); err != nil || sub != "account-b" {
		t.Fatalf("people records %q, %v after account-b's sign-in; want account-b", sub, err)
	}
	if !recordedAt().After(first) {
		t.Errorf("account-b's sign-in beside a build without the binding left the recording time at %s; want it moved", first)
	}
	for name, id := range map[string]string{"token-a": tokenA, "token-none": tokenNone} {
		if _, revoked := tokenState(t, database.Pool, id); revoked {
			t.Errorf("account-b's sign-in beside a build without the binding revoked %s; want nothing revoked", name)
		}
	}
	if n := owed(); n != 0 {
		t.Errorf("account-b's sign-in beside a build without the binding owes %d broker revocations; want none", n)
	}
	if kept := keptReplacements(t, database, email); !slices.Equal(kept, []string{"account-a"}) {
		t.Errorf("account-b's sign-in beside a build without the binding left the replacements %q kept; want account-a's", kept)
	}

	if _, err := database.Pool.Exec(ctx, `delete from server_processes`); err != nil {
		t.Fatal(err)
	}
	ageBindingMark(t, database, OwnerLockMarkWindow+time.Minute)
	signInAll(t, people, email, "account-c")
	for name, id := range map[string]string{"token-a": tokenA, "token-none": tokenNone} {
		if _, revoked := tokenState(t, database.Pool, id); !revoked {
			t.Errorf("control: account-c's sign-in with no build without the binding in the fleet left %s live; want it revoked", name)
		}
	}
	if got := owedSubjects(t, database, email); !slices.Equal(got, []string{"account-a", "account-b"}) {
		t.Errorf("control: account-c's sign-in with no build without the binding in the fleet owes the broker revocations of %q; want account-a's, which it caught up, and account-b's, which it replaced", got)
	}
	if kept := keptReplacements(t, database, email); len(kept) != 0 {
		t.Errorf("account-c's sign-in with the binding holding left the replacements %q kept; want none", kept)
	}
}

// keptReplacements answers the accounts the database keeps as replaced at email while the binding
// was off (account_binding_replacements), in the order they were replaced, a first recording's
// as the empty string.
func keptReplacements(t *testing.T, s *Store, email string) []string {
	t.Helper()
	rows, err := s.Pool.Query(context.Background(), `select coalesce(replaced_subject, '') from account_binding_replacements where email = $1 order by replaced_at`, email)
	if err != nil {
		t.Fatal(err)
	}
	kept, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return kept
}

// owedSubjects answers the accounts email's owed broker revocations name, in the order they were
// recorded.
func owedSubjects(t *testing.T, s *Store, email string) []string {
	t.Helper()
	rows, err := s.Pool.Query(context.Background(), `select subject from broker_revocations where email = $1 and done_at is null order by recorded_at`, email)
	if err != nil {
		t.Fatal(err)
	}
	subjects, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return subjects
}

// A replacement made while the binding is off is kept, whichever build makes it, and caught up
// once the binding holds (CatchUpAccountBindingOf): account-b replaces account-a through this
// build and account-c replaces account-b through a build without the binding (its sign-in's
// update of sub, spelled here), and between them tokens recording no account are made. While the
// binding is off the catch-up writes nothing. Once it holds, it revokes every token of the address
// that records no account made since the first replacement, and the token that records account-a,
// which the binding refuses and which a later build without the binding would admit again; owes the
// broker a revocation of account-a and of account-b, each naming account-c as the holder; and
// deletes the kept rows; a second catch-up does nothing.
//
// Control, run by hand: with the trigger people_account_binding_replacement dropped, account-c's
// replacement through a build without the binding is not kept (account-b's, this build's
// sign-in keeps itself), and the row fails at the replacements kept.
func TestAReplacementMadeWhileTheBindingIsOffIsCaughtUpOnceItHolds(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := brokerPeople(database)
	const email = "binding-catch-up@example.com"
	signInAll(t, people, email, "account-a")
	tokenA := mintToken(t, database.Pool, email, "token-a", "account-a")

	registerFleetProcess(t, database, OwnerLockProtocol, 0)
	signInAll(t, people, email, "account-b")
	madeByA := mintToken(t, database.Pool, email, "made-in-the-overlap", "")
	if _, err := database.Pool.Exec(ctx, `update people set signed_in_at = now(), confirmed_at = now(), sub = 'account-c' where email = $1`, email); err != nil {
		t.Fatalf("account-c's sign-in through a build without the binding: %v", err)
	}
	madeLater := mintToken(t, database.Pool, email, "made-after-the-second", "")
	if kept := keptReplacements(t, database, email); !slices.Equal(kept, []string{"account-a", "account-b"}) {
		t.Fatalf("the replacements kept are %q; want account-a's and account-b's, whichever build made them", kept)
	}
	if held, caught, err := CatchUpAccountBindingOf(ctx, database.Pool, email, testRevocationIssuer); err != nil || held || len(caught.Replaced) != 0 {
		t.Fatalf("a catch-up while the binding is off answered held %t, %+v, %v; want nothing done", held, caught, err)
	}
	if got := owedSubjects(t, database, email); len(got) != 0 {
		t.Fatalf("a catch-up while the binding is off owes %q; want nothing", got)
	}

	if _, err := database.Pool.Exec(ctx, `delete from server_processes`); err != nil {
		t.Fatal(err)
	}
	ageBindingMark(t, database, OwnerLockMarkWindow+time.Minute)
	held, caught, err := CatchUpAccountBindingOf(ctx, database.Pool, email, testRevocationIssuer)
	if err != nil || !held {
		t.Fatalf("the catch-up once the binding holds answered held %t, %v", held, err)
	}
	want := AccountBindingCatchUp{Replaced: []string{"account-a", "account-b"}, RefusedLive: true, TokensRevoked: 3, Owed: []string{"account-a", "account-b"}, Holder: "account-c"}
	if !reflect.DeepEqual(caught, want) {
		t.Errorf("the catch-up did %+v; want %+v", caught, want)
	}
	for name, id := range map[string]string{"made-in-the-overlap": madeByA, "made-after-the-second": madeLater} {
		if _, revoked := tokenState(t, database.Pool, id); !revoked {
			t.Errorf("the catch-up left %s, which records no account and was made after a kept replacement, live; want it revoked", name)
		}
	}
	if _, revoked := tokenState(t, database.Pool, tokenA); !revoked {
		t.Error("the catch-up left token-a live, which records account-a and which a later build without the binding would admit again; want it revoked")
	}
	var holders []string
	rows, err := database.Pool.Query(ctx, `select holder_subject from broker_revocations where email = $1 order by recorded_at`, email)
	if err != nil {
		t.Fatal(err)
	}
	if holders, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatal(err)
	}
	if got := owedSubjects(t, database, email); !slices.Equal(got, []string{"account-a", "account-b"}) || !slices.Equal(holders, []string{"account-c", "account-c"}) {
		t.Errorf("the catch-up owes the broker revocations of %q naming the holders %q; want account-a's and account-b's, each naming account-c", got, holders)
	}
	if kept := keptReplacements(t, database, email); len(kept) != 0 {
		t.Errorf("the catch-up left the replacements %q kept; want none", kept)
	}

	readGenerations := func() string {
		t.Helper()
		var g string
		if err := database.Pool.QueryRow(ctx, `select coalesce(string_agg(subject || '@' || generation, ',' order by subject), '') from broker_revocations where email = $1`, email).Scan(&g); err != nil {
			t.Fatal(err)
		}
		return g
	}
	generations := readGenerations()
	held, again, err := CatchUpAccountBindingOf(ctx, database.Pool, email, testRevocationIssuer)
	if err != nil || !held || !reflect.DeepEqual(again, AccountBindingCatchUp{}) {
		t.Errorf("a second catch-up answered held %t, %+v, %v; want nothing done", held, again, err)
	}
	if got := readGenerations(); got != generations {
		t.Errorf("a second catch-up moved the broker revocations from %q to %q; want them as they were", generations, got)
	}
}

// The trigger keeps every change of an address's account that a writer without the binding makes,
// whatever the fleet reads (keep_account_binding_replacement, migration 20261121180429), and nothing
// for this build's sign-in, which marks its transaction and keeps or revokes its own change. With
// no build without the binding registered and no mark, so the binding holds (a process the fleet
// does not see, past the prune age with its last arrival more than an hour old): account-b's
// sign-in through such a build (its update of sub, spelled here) is kept and announced, and the
// catch-up revokes the token recording no account made after it and owes the broker account-a. A
// first recording through it (an insert recording sub) is kept too. This build's sign-in of
// account-c with the binding holding keeps nothing; with a build without the binding registered,
// account-d's sign-in keeps its change once, by its own keep and not again by the trigger.
//
// Control, run by hand: with the trigger reading the fleet as 20261121121717 did (it keeps a change
// only while account_binding_fleet_read finds a build without the binding), account-b's change and
// the first recording are not kept, and the row fails at "kept".
func TestTheTriggerKeepsEveryChangeAWriterWithoutTheBindingMakesWhateverTheFleetReads(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := brokerPeople(database)
	const email, first = "unmarked-writer@example.com", "unmarked-first@example.com"
	signInAll(t, people, email, "account-a")
	if holds, _ := bindingHolds(t, database); !holds {
		t.Fatal("setup: the binding does not hold with no build without it registered")
	}
	listener, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Release()
	if _, err := listener.Exec(ctx, `listen `+BrokerRevocationsChannel); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `update people set signed_in_at = now(), confirmed_at = now(), sub = 'account-b' where email = $1`, email); err != nil {
		t.Fatalf("account-b's sign-in through a build without the binding: %v", err)
	}
	if kept := keptReplacements(t, database, email); !slices.Equal(kept, []string{"account-a"}) {
		t.Errorf("account-b's sign-in through a build without the binding, with the binding read as holding, left %q kept; want account-a's", kept)
	}
	wait, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, err = listener.Conn().WaitForNotification(wait)
	cancel()
	if err != nil {
		t.Errorf("the kept change was not announced on %s (%v); want the revocation loop woken", BrokerRevocationsChannel, err)
	}
	madeSince := mintToken(t, database.Pool, email, "made after the change", "")
	held, caught, err := CatchUpAccountBindingOf(ctx, database.Pool, email, testRevocationIssuer)
	if err != nil || !held || !slices.Equal(caught.Replaced, []string{"account-a"}) || !slices.Equal(caught.Owed, []string{"account-a"}) || caught.Holder != "account-b" {
		t.Errorf("the catch-up answered held %t, %+v, %v; want account-a's replacement caught up and owed, account-b holding", held, caught, err)
	}
	if _, revoked := tokenState(t, database.Pool, madeSince); !revoked {
		t.Error("the catch-up left the token recording no account made after the change live; want it revoked")
	}
	if _, err := database.Pool.Exec(ctx, `insert into people (email, sub) values ($1, 'account-z')`, first); err != nil {
		t.Fatalf("a first recording through a build without the binding: %v", err)
	}
	if kept := keptReplacements(t, database, first); !slices.Equal(kept, []string{""}) {
		t.Errorf("a first recording through a build without the binding left %q kept; want the first recording", kept)
	}

	signInAll(t, people, email, "account-c")
	if kept := keptReplacements(t, database, email); len(kept) != 0 {
		t.Errorf("this build's sign-in with the binding holding left %q kept; want nothing, since it revokes and owes its own change", kept)
	}
	registerFleetProcess(t, database, OwnerLockProtocol, 0)
	signInAll(t, people, email, "account-d")
	if kept := keptReplacements(t, database, email); !slices.Equal(kept, []string{"account-c"}) {
		t.Errorf("this build's sign-in beside a build without the binding left %q kept; want account-c's once, kept by the sign-in", kept)
	}
}

// An account that takes its address back while the binding is off holds it, so the catch-up owes
// the broker nothing for it: account-b replaces account-a, and account-a takes the address back
// through a build without the binding; once the binding holds, the catch-up owes account-b's
// revocation alone, naming account-a as the holder.
func TestACatchUpOwesNothingToTheAccountThatHoldsTheAddressAgain(t *testing.T) {
	ctx := context.Background()
	database := migratedEmptyStore(t)
	people := brokerPeople(database)
	const email = "binding-take-back@example.com"
	signInAll(t, people, email, "account-a")
	registerFleetProcess(t, database, OwnerLockProtocol, 0)
	signInAll(t, people, email, "account-b")
	if _, err := database.Pool.Exec(ctx, `update people set signed_in_at = now(), confirmed_at = now(), sub = 'account-a' where email = $1`, email); err != nil {
		t.Fatalf("account-a's sign-in through a build without the binding: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `delete from server_processes`); err != nil {
		t.Fatal(err)
	}
	ageBindingMark(t, database, OwnerLockMarkWindow+time.Minute)
	held, caught, err := CatchUpAccountBindingOf(ctx, database.Pool, email, testRevocationIssuer)
	if err != nil || !held {
		t.Fatalf("the catch-up answered held %t, %v", held, err)
	}
	if !slices.Equal(caught.Owed, []string{"account-b"}) || caught.Holder != "account-a" {
		t.Errorf("the catch-up owes %q naming the holder %q; want account-b's alone, naming account-a", caught.Owed, caught.Holder)
	}
	if got := owedSubjects(t, database, email); !slices.Equal(got, []string{"account-b"}) {
		t.Errorf("the broker is owed the revocations of %q; want account-b's alone", got)
	}
}

// holdUnboundPublish begins the dashboard publish of a build without the binding on a connection
// of its own and holds it uncommitted, as its asset write holds it (legion-envoy-v32.34.0's
// dashboard.Publish: the insert, the assets, then the commit): its insert holds the binding's gate
// exclusively, and its mark is not committed.
func holdUnboundPublish(t *testing.T, s *Store) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.ConnectConfig(ctx, s.Pool.Config().ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	publish, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publish.Exec(ctx, `insert into dashboard_builds (build, entry, commit, owner_lock_protocol) values ('unbound', '', 'unbound0build', 1)`); err != nil {
		t.Fatalf("publish as a build without the binding: %v", err)
	}
	if !bindingGateHeld(t, s, conn.PgConn().PID()) {
		t.Fatal("the held publish of a build without the binding holds no binding gate")
	}
	return publish
}

// A sign-in that reads the binding as off keeps its own change (keepAccountChangeSQL), whatever
// the trigger people_account_binding_replacement reads when the sign-in's update fires it: the
// fleet can change between the two readings. While a build without the binding arrives, its
// dashboard publish holds the binding's gate exclusively and uncommitted, so the sign-in's try of
// the gate fails and it revokes and owes nothing, while the trigger finds no such build in what
// is committed; and a process without the binding whose mark has aged out can deregister after
// the sign-in read it and before the trigger reads. In each case the sign-in keeps the change and
// wakes the revocation loop, and once the binding holds (the arrival rolled back, or committed
// and aged out, or the process gone) the catch-up revokes the token recording no account made in
// the overlap and the token recording the replaced account, and owes the broker the replaced
// account, naming the holder. A first recording made during an arrival is kept too, and its
// catch-up revokes and owes as a first recording with the binding on does.
//
// Controls, run by hand: with SignInSubject's keepAccountChangeSQL removed, every case keeps
// nothing and fails at "kept"; with the trigger announcing only a change it keeps (migration
// 20261121180429), every case fails at "announced".
func TestASignInThatReadsTheBindingOffKeepsItsChangeWhateverTheTriggerReads(t *testing.T) {
	ctx := context.Background()
	arrival := func(commit bool) func(t *testing.T, s *Store, email string, signIn func() error) {
		return func(t *testing.T, s *Store, email string, signIn func() error) {
			publish := holdUnboundPublish(t, s)
			if err := signIn(); err != nil {
				t.Fatalf("the sign-in while a publish without the binding holds the gate: %v", err)
			}
			if !commit {
				if err := publish.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
				return
			}
			if err := publish.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			ageBindingMark(t, s, OwnerLockMarkWindow+time.Minute)
		}
	}
	deregistering := func(t *testing.T, s *Store, email string, signIn func() error) {
		instance := registerFleetProcess(t, s, OwnerLockProtocol, 0)
		ageBindingMark(t, s, OwnerLockMarkWindow+time.Minute)
		row, err := s.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer row.Rollback(ctx)
		if _, err := row.Exec(ctx, `select from people where email = $1 for update`, email); err != nil {
			t.Fatal(err)
		}
		signedIn := make(chan error, 1)
		go func() { signedIn <- signIn() }()
		// The sign-in reads the fleet right after the address's lock and before it reaches the
		// address's row, which the test holds.
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			select {
			case err := <-signedIn:
				t.Fatalf("the sign-in ended (%v) while the test held the address's row; want it to wait there, after its fleet read", err)
			default:
			}
			var waits bool
			if err := s.Pool.QueryRow(ctx, `
				select exists (select from pg_stat_activity where datname = current_database() and $1::integer = any(pg_blocking_pids(pid)))`,
				row.Conn().PgConn().PID()).Scan(&waits); err != nil {
				t.Fatal(err)
			}
			if waits {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the sign-in did not wait for the address's row within 10s")
			}
		}
		if _, err := s.Pool.Exec(ctx, `delete from server_processes where instance = $1::uuid`, instance); err != nil {
			t.Fatal(err)
		}
		if err := row.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-signedIn; err != nil {
			t.Fatalf("the sign-in, once the address's row was let go: %v", err)
		}
	}
	for _, c := range []struct {
		name           string
		firstRecording bool
		window         func(t *testing.T, s *Store, email string, signIn func() error)
	}{
		{"a replacement while a publish without the binding holds the gate, which rolls back", false, arrival(false)},
		{"a replacement while a publish without the binding holds the gate, which commits and ages out", false, arrival(true)},
		{"a replacement read beside a process without the binding that deregisters before the trigger reads", false, deregistering},
		{"a first recording while a publish without the binding holds the gate, which rolls back", true, arrival(false)},
	} {
		t.Run(c.name, func(t *testing.T) {
			database := migratedEmptyStore(t)
			people := brokerPeople(database)
			const email = "kept-by-the-sign-in@example.com"
			var before string
			want := AccountBindingCatchUp{Replaced: []string{"account-a"}, RefusedLive: true, TokensRevoked: 2, Owed: []string{"account-a"}, Holder: "account-b"}
			wantKept := []string{"account-a"}
			if c.firstRecording {
				if _, err := database.Pool.Exec(ctx, `insert into people (email) values ($1)`, email); err != nil {
					t.Fatal(err)
				}
				before = mintToken(t, database.Pool, email, "of an account that never signed in", "account-z")
				want = AccountBindingCatchUp{FirstRecording: true, TokensRevoked: 2, Owed: []string{"account-z"}, Holder: "account-b"}
				wantKept = []string{""}
			} else {
				signInAll(t, people, email, "account-a")
				before = mintToken(t, database.Pool, email, "token-a", "account-a")
			}
			listener, err := database.Pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Release()
			if _, err := listener.Exec(ctx, `listen `+BrokerRevocationsChannel); err != nil {
				t.Fatal(err)
			}

			c.window(t, database, email, func() error {
				return people.SignInSubject(ctx, email, "account-b", "refresh-account-b", "", model.MemberFull)
			})
			overlap := mintToken(t, database.Pool, email, "made in the overlap", "")
			if _, revoked := tokenState(t, database.Pool, before); revoked {
				t.Errorf("account-b's sign-in, which read the binding as off, revoked the token from before it; want nothing revoked")
			}
			if got := owedSubjects(t, database, email); len(got) != 0 {
				t.Errorf("account-b's sign-in, which read the binding as off, owes the broker %q; want nothing", got)
			}
			if kept := keptReplacements(t, database, email); !slices.Equal(kept, wantKept) {
				t.Errorf("account-b's sign-in, which read the binding as off, left %q kept; want %q, kept by the sign-in itself", kept, wantKept)
			}
			wait, cancel := context.WithTimeout(ctx, 10*time.Second)
			_, err = listener.Conn().WaitForNotification(wait)
			cancel()
			if err != nil {
				t.Errorf("account-b's sign-in kept its change without announcing it on %s (%v); want the revocation loop woken", BrokerRevocationsChannel, err)
			}

			if holds, _ := bindingHolds(t, database); !holds {
				t.Fatal("the binding does not hold once the build without it is gone; the row cannot read the catch-up")
			}
			held, caught, err := CatchUpAccountBindingOf(ctx, database.Pool, email, testRevocationIssuer)
			if err != nil || !held {
				t.Fatalf("the catch-up once the binding holds answered held %t, %v", held, err)
			}
			if !reflect.DeepEqual(caught, want) {
				t.Errorf("the catch-up did %+v; want %+v", caught, want)
			}
			if _, revoked := tokenState(t, database.Pool, overlap); !revoked {
				t.Error("the catch-up left the token made in the overlap, which records no account, live; want it revoked")
			}
			if _, revoked := tokenState(t, database.Pool, before); !revoked {
				t.Error("the catch-up left the token from before the change live; want it revoked")
			}
			if got := owedSubjects(t, database, email); !slices.Equal(got, want.Owed) {
				t.Errorf("once the binding holds the broker is owed the revocations of %q; want %q", got, want.Owed)
			}
		})
	}
}
