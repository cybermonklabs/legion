package oauthstore_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/testpool"
)

// registrarA and registrarB are two daemons' login accounts, the registrars the writes below
// name. Every test but the three of the session name's binding writes as registrarA alone, so each
// row it writes is its own registrar's and keeps the generation rule.
const (
	registrarA = "system:serviceaccount:legion-system:legion-daemon-a"
	registrarB = "system:serviceaccount:legion-system:legion-daemon-b"
)

// TestAWriteWithoutAThumbprintKeepsTheBinding: a registration bound to a role's key stays bound
// while the row lives. A later write naming no key, which is how a session watch writes,
// extends the row and leaves the key required: an exchange proved with another key is refused as
// bound to another key, and one proved with the bound key is admitted.
func TestAWriteWithoutAThumbprintKeepsTheBinding(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	const pod, session, kA, kB = "pod-uid-issue", "reviewer-g1", "thumbprint-of-role-a", "thumbprint-of-role-b"
	if stored, err := s.RegisterPodSession(ctx, registrarA, pod, session, kB, 0); err != nil || stored.JKT != kB {
		t.Fatalf("RegisterPodSession with %s = %+v, %v; want the registration bound to %s", kB, stored, err, kB)
	}
	if stored, err := s.RegisterPodSession(ctx, registrarA, pod, session, "", 0); err != nil || stored.JKT != kB {
		t.Fatalf("RegisterPodSession with no thumbprint = %+v, %v; want the binding to %s kept", stored, err, kB)
	}
	if registered, err := s.PodSessionRegistered(ctx, pod, session, kA); registered || !errors.Is(err, oauthstore.ErrPodSessionBoundToAnotherKey) {
		t.Fatalf("PodSessionRegistered with %s after a write naming no key = %t, %v; want false, ErrPodSessionBoundToAnotherKey: the write removed the binding to %s", kA, registered, err, kB)
	}
	if registered, err := s.PodSessionRegistered(ctx, pod, session, kB); !registered || err != nil {
		t.Fatalf("PodSessionRegistered with %s = %t, %v; want true", kB, registered, err)
	}
	if registered, err := s.PodSessionRegistered(ctx, pod, "another-session", kB); registered || err != nil {
		t.Fatalf("PodSessionRegistered for a session never registered = %t, %v; want false and no error", registered, err)
	}
}

// TestALateOlderGenerationLeavesTheNewerKey: a role's new incarnation writes (P, S, kB, gen 2),
// and its old incarnation's write (P, S, kA, gen 1), which the broker commits only afterwards,
// changes nothing: the row stays bound to kB at generation 2 with the expiry the newer write
// left, the late write is answered that row, and the exchange keeps admitting kB and refusing kA.
func TestALateOlderGenerationLeavesTheNewerKey(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	const pod, session, kA, kB = "pod-uid-issue", "implementer-g1", "thumbprint-of-the-old-incarnation", "thumbprint-of-the-new-incarnation"
	newer, err := s.RegisterPodSession(ctx, registrarA, pod, session, kB, 2)
	if err != nil || newer.JKT != kB || newer.Generation != 2 {
		t.Fatalf("RegisterPodSession with %s at generation 2 = %+v, %v; want the registration bound to %s at generation 2", kB, newer, err, kB)
	}
	late, err := s.RegisterPodSession(ctx, registrarA, pod, session, kA, 1)
	if err != nil || late.JKT != kB || late.Generation != 2 || !late.ExpiresAt.Equal(newer.ExpiresAt) {
		t.Fatalf("a late write with %s at generation 1 after %s at generation 2 = %+v, %v; want it answered the unchanged row %+v: the old incarnation's write put its key back", kA, kB, late, err, newer)
	}
	if registered, err := s.PodSessionRegistered(ctx, pod, session, kA); registered || !errors.Is(err, oauthstore.ErrPodSessionBoundToAnotherKey) {
		t.Fatalf("PodSessionRegistered with the old key %s = %t, %v; want false, ErrPodSessionBoundToAnotherKey", kA, registered, err)
	}
	if registered, err := s.PodSessionRegistered(ctx, pod, session, kB); !registered || err != nil {
		t.Fatalf("PodSessionRegistered with the new key %s = %t, %v; want true", kB, registered, err)
	}
}

// TestAWriteOfTheSameGenerationIsApplied: a write of the row's own generation is applied, as a
// renewal of one incarnation and every write of a session watch, which sends none, are: a key it
// names replaces the row's, and its expiry is never earlier than the row's.
func TestAWriteOfTheSameGenerationIsApplied(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	const pod, kA, kB = "pod-uid-issue", "thumbprint-of-role-a", "thumbprint-of-role-b"
	for _, generation := range []int64{0, 3} {
		session := fmt.Sprintf("session-at-generation-%d", generation)
		first, err := s.RegisterPodSession(ctx, registrarA, pod, session, kA, generation)
		if err != nil {
			t.Fatalf("RegisterPodSession at generation %d: %v", generation, err)
		}
		again, err := s.RegisterPodSession(ctx, registrarA, pod, session, kB, generation)
		if err != nil || again.JKT != kB || again.Generation != generation || again.ExpiresAt.Before(first.ExpiresAt) {
			t.Fatalf("a second write at generation %d with %s after %+v = %+v, %v; want it applied: bound to %s at generation %d", generation, kB, first, again, err, kB, generation)
		}
	}
}

// TestAWriteSaysWhetherItWasAppliedAndWhatItReplaced: each write answers whether it set the row
// and the key it replaced with another. A first write replaces nothing; a renewal with the key
// the row holds, a write naming no key and a write binding a row bound to the pod alone replace
// nothing; a write naming another key replaces the row's; and a write of a lower generation is
// not applied and replaces nothing.
func TestAWriteSaysWhetherItWasAppliedAndWhatItReplaced(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	const pod, kA, kB = "pod-uid-issue", "thumbprint-of-role-a", "thumbprint-of-role-b"
	for _, w := range []struct {
		what, session, jkt string
		generation         int64
		applied            bool
		replaced           string
	}{
		{"a first write with kA", "implementer-g1", kA, 1, true, ""},
		{"a renewal with kA", "implementer-g1", kA, 1, true, ""},
		{"a write naming no key", "implementer-g1", "", 1, true, ""},
		{"a write with kB", "implementer-g1", kB, 2, true, kA},
		{"a late write with kA at a lower generation", "implementer-g1", kA, 1, false, ""},
		{"a first write naming no key", "watched-session", "", 0, true, ""},
		{"a write binding the pod's row to kA", "watched-session", kA, 0, true, ""},
	} {
		stored, err := s.RegisterPodSession(ctx, registrarA, pod, w.session, w.jkt, w.generation)
		if err != nil || stored.Applied != w.applied || stored.Replaced != w.replaced {
			t.Fatalf("%s of %s at generation %d = %+v, %v; want applied %t, replacing %q", w.what, w.session, w.generation, stored, err, w.applied, w.replaced)
		}
	}
}

// writeAnswer is what one RegisterPodSession call answered.
type writeAnswer struct {
	stored oauthstore.PodSessionRegistration
	err    error
}

// whileHeld runs other, another writer's statement given args, in a transaction of its own, calls
// write while that transaction is open, waits until write waits on it, commits it, and answers
// what write answered: what it did once the other writer had committed.
func whileHeld(t *testing.T, pool *pgxpool.Pool, other string, args []any, write func() (oauthstore.PodSessionRegistration, error)) writeAnswer {
	t.Helper()
	ctx := context.Background()
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the other writer's transaction: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, other, args...); err != nil {
		t.Fatalf("the other writer's %q: %v", other, err)
	}
	answered := make(chan writeAnswer, 1)
	go func() {
		stored, err := write()
		answered <- writeAnswer{stored, err}
	}()
	storetest.AwaitLockWaiters(t, pool, holder, 1)
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("commit the other writer's transaction: %v", err)
	}
	return <-answered
}

// insertedBy is the statement another write's first insert of a registration is: the pod $1,
// the session $2, the key $3, the generation $4 and the registrar $5. Each caller names the
// registrar of the write it races, since a row of another registrar or of none is taken whole by
// that write and the cases here hold the generation rule of the writer's own row.
const insertedBy = `insert into oauth_pod_sessions (pod_uid, session, expires_at, jkt, generation, registrar)
	values ($1, $2, now() + interval '1 day', $3, $4, $5)`

// TestAWriteNamesTheKeyTheRowHeldWhenItWrote: the key a write says it replaced is the one the row
// held when this write changed it, not one an earlier read of the row saw. A write that waits on
// another writer's change of the row's key names the key that writer committed; a write that
// finds no row and then waits on another writer's insert of it names the key that writer
// inserted, rather than reporting a first write.
func TestAWriteNamesTheKeyTheRowHeldWhenItWrote(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)
	const pod, kA, kB, kC = "pod-uid-issue", "thumbprint-a", "thumbprint-b", "thumbprint-c"
	writeC := func(session string) func() (oauthstore.PodSessionRegistration, error) {
		return func() (oauthstore.PodSessionRegistration, error) {
			return s.RegisterPodSession(ctx, registrarA, pod, session, kC, 1)
		}
	}

	if _, err := s.RegisterPodSession(ctx, registrarA, pod, "changed", kA, 1); err != nil {
		t.Fatalf("register %s with %s: %v", "changed", kA, err)
	}
	changed := whileHeld(t, pool, `update oauth_pod_sessions set jkt = $3 where pod_uid = $1 and session = $2`,
		[]any{pod, "changed", kB}, writeC("changed"))
	if changed.err != nil || !changed.stored.Applied || changed.stored.JKT != kC || changed.stored.Replaced != kB {
		t.Errorf("a write with %s waiting on another writer's change of %s to %s = %+v, %v; want it applied, bound to %s, replacing %s, the key the other writer committed",
			kC, kA, kB, changed.stored, changed.err, kC, kB)
	}

	inserted := whileHeld(t, pool, insertedBy, []any{pod, "inserted", kB, 1, registrarA}, writeC("inserted"))
	if inserted.err != nil || !inserted.stored.Applied || inserted.stored.JKT != kC || inserted.stored.Replaced != kB {
		t.Errorf("a write with %s waiting on another writer's insert of the row with %s = %+v, %v; want it applied, bound to %s, replacing %s, the key the other writer inserted",
			kC, kB, inserted.stored, inserted.err, kC, kB)
	}
}

// newStoreDefaulting is newStore on a migrated database of its own whose
// default_transaction_isolation is isolation, as a deployment's database may be configured, so a
// transaction that names no level of its own runs at it. It fails t unless a transaction begun
// with no level reports isolation.
func newStoreDefaulting(t *testing.T, isolation string) (*oauthstore.Store, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	dsn := storetest.DatabaseURL(t)
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open the test database: %v", err)
	}
	err = st.Migrate(ctx)
	st.Close()
	if err != nil {
		t.Fatalf("migrate the test database: %v", err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to the test database: %v", err)
	}
	var name string
	err = conn.QueryRow(ctx, `select current_database()`).Scan(&name)
	if err == nil {
		_, err = conn.Exec(ctx, `alter database `+pgx.Identifier{name}.Sanitize()+` set default_transaction_isolation = '`+isolation+`'`)
	}
	_ = conn.Close(ctx)
	if err != nil {
		t.Fatalf("set the test database's default_transaction_isolation to %s: %v", isolation, err)
	}
	pool, err := testpool.NewPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	var level string
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `show transaction_isolation`).Scan(&level)
	}); err != nil || level != isolation {
		t.Fatalf("a transaction that names no level runs at %q, %v; want %q, the database's default", level, err, isolation)
	}
	return oauthstore.New(pool), pool
}

// rowOf is the key and generation the pod's registration for session holds.
func rowOf(t *testing.T, pool *pgxpool.Pool, pod, session string) (jkt string, generation int64) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `select coalesce(jkt, ''), generation from oauth_pod_sessions
		where pod_uid = $1 and session = $2`, pod, session).Scan(&jkt, &generation); err != nil {
		t.Fatalf("read the registration of %s: %v", session, err)
	}
	return jkt, generation
}

// together runs two writes at once, released by one barrier, each on a pool connection of its
// own, and answers what each answered.
func together(first, second func() (oauthstore.PodSessionRegistration, error)) (writeAnswer, writeAnswer) {
	start := make(chan struct{})
	var wg sync.WaitGroup
	var a, b writeAnswer
	wg.Go(func() {
		<-start
		stored, err := first()
		a = writeAnswer{stored, err}
	})
	wg.Go(func() {
		<-start
		stored, err := second()
		b = writeAnswer{stored, err}
	})
	close(start)
	wg.Wait()
	return a, b
}

// tally counts the rounds that went wrong in each of two ways, the generation rule (a write's
// error, the row it left, or what a write says it stored) and the key a write says it replaced,
// and keeps the first of each, so a run says how often each went wrong rather than only that one
// round did.
type tally struct {
	rounds, wrongRule, wrongReplaced int
	firstRule, firstReplaced         string
}

// add records round, with what was wrong with its generation rule and with its replaced key, each
// "" when nothing was.
func (r *tally) add(round int, rule, replaced string) {
	r.rounds++
	if rule != "" {
		r.wrongRule++
		if r.firstRule == "" {
			r.firstRule = fmt.Sprintf("round %d: %s", round, rule)
		}
	}
	if replaced != "" {
		r.wrongReplaced++
		if r.firstReplaced == "" {
			r.firstReplaced = fmt.Sprintf("round %d: %s", round, replaced)
		}
	}
}

// report logs both counts and fails t naming each way any round went wrong.
func (r *tally) report(t *testing.T) {
	t.Helper()
	t.Logf("%d rounds: the generation rule wrong in %d, the replaced key wrong in %d", r.rounds, r.wrongRule, r.wrongReplaced)
	if r.wrongRule > 0 {
		t.Errorf("the generation rule went wrong in %d of %d rounds; the first, %s", r.wrongRule, r.rounds, r.firstRule)
	}
	if r.wrongReplaced > 0 {
		t.Errorf("the replaced key went wrong in %d of %d rounds; the first, %s", r.wrongReplaced, r.rounds, r.firstReplaced)
	}
}

// TestConcurrentFirstRegistrationsKeepTheHigherGeneration: two first writes of one pod and
// session, of generations 1 and 2 with different keys, leave the row at generation 2 and its key
// whichever commits first, and the write of generation 1 is answered unapplied when it came
// second. Both writes may read no row, so the one that inserts second finds the other's insert,
// waits for it to commit, and reads the row as it committed, which only a statement of its own
// under read committed sees. On a database whose default isolation is repeatable read the
// transaction's one snapshot could not see that row and the insert would fail to serialize, so
// RegisterPodSession names read committed itself, and both databases run here.
//
// Two writes of one generation that change an existing row's key at once both apply, and each
// names the key the row held when it changed it: the first to commit the registered key, and the
// second the first's, never a key an earlier snapshot saw.
//
// Behind a barrier the two writes interleave only in some rounds, so each database also runs the
// interleaving that matters every time: the first write's insert held open until the second
// waits on it, in each order of the generations.
func TestConcurrentFirstRegistrationsKeepTheHigherGeneration(t *testing.T) {
	const pod, k0, k1, k2, rounds = "pod-uid-issue", "thumbprint-registered", "thumbprint-of-generation-1", "thumbprint-of-generation-2", 200
	for _, isolation := range []string{"read committed", "repeatable read"} {
		t.Run("database default "+isolation, func(t *testing.T) {
			ctx := context.Background()
			s, pool := newStoreDefaulting(t, isolation)
			write := func(session, jkt string, generation int64) func() (oauthstore.PodSessionRegistration, error) {
				return func() (oauthstore.PodSessionRegistration, error) {
					return s.RegisterPodSession(ctx, registrarA, pod, session, jkt, generation)
				}
			}
			keptGeneration2 := func(t *testing.T, what, session string) {
				t.Helper()
				if jkt, generation := rowOf(t, pool, pod, session); jkt != k2 || generation != 2 {
					t.Fatalf("%s: the row holds %s at generation %d, want %s at generation 2", what, jkt, generation, k2)
				}
			}

			t.Run("the other write held open", func(t *testing.T) {
				second := whileHeld(t, pool, insertedBy, []any{pod, "first-1", k1, 1, registrarA}, write("first-1", k2, 2))
				if got := second.stored; second.err != nil || !got.Applied || got.JKT != k2 || got.Generation != 2 || got.Replaced != k1 {
					t.Fatalf("the write of generation 2 waiting on generation 1's insert = %+v, %v; want it applied, bound to %s at generation 2, replacing %s", got, second.err, k2, k1)
				}
				keptGeneration2(t, "generation 2 after generation 1", "first-1")

				second = whileHeld(t, pool, insertedBy, []any{pod, "first-2", k2, 2, registrarA}, write("first-2", k1, 1))
				if got := second.stored; second.err != nil || got.Applied || got.JKT != k2 || got.Generation != 2 || got.Replaced != "" {
					t.Fatalf("the write of generation 1 waiting on generation 2's insert = %+v, %v; want it not applied, answered %s at generation 2", got, second.err, k2)
				}
				keptGeneration2(t, "generation 1 after generation 2", "first-2")
			})

			t.Run("first writes behind a barrier", func(t *testing.T) {
				var result tally
				cameSecond := 0
				for round := 1; round <= rounds; round++ {
					session := fmt.Sprintf("first-round-%d", round)
					low, high := together(write(session, k1, 1), write(session, k2, 2))
					l, h := low.stored, high.stored
					var rule, replaced string
					if low.err != nil || high.err != nil {
						result.add(round, fmt.Sprintf("the write of generation 1 = %v and of generation 2 = %v, want both written", low.err, high.err), "")
						continue
					}
					switch jkt, generation := rowOf(t, pool, pod, session); {
					case jkt != k2 || generation != 2:
						rule = fmt.Sprintf("the row holds %s at generation %d, want %s at generation 2", jkt, generation, k2)
					case !h.Applied || h.JKT != k2 || h.Generation != 2:
						rule = fmt.Sprintf("generation 2 answered %+v, want it applied, bound to %s at generation 2", h, k2)
					case l.Applied && (l.JKT != k1 || l.Generation != 1):
						rule = fmt.Sprintf("generation 1 came first and answered %+v, want it bound to %s at generation 1", l, k1)
					case !l.Applied && (l.JKT != k2 || l.Generation != 2):
						rule = fmt.Sprintf("generation 1 came second and answered %+v, want it answered %s at generation 2", l, k2)
					case l.Replaced != "":
						replaced = fmt.Sprintf("generation 1 answered %+v, replacing a key, want none: it wrote first or not at all", l)
					case l.Applied && h.Replaced != k1:
						replaced = fmt.Sprintf("generation 1 came first and generation 2 then answered %+v, want it replacing %s", h, k1)
					case !l.Applied && h.Replaced != "":
						replaced = fmt.Sprintf("generation 1 came second and generation 2 answered %+v, want it replacing nothing: it created the row", h)
					}
					result.add(round, rule, replaced)
					if !l.Applied {
						cameSecond++
					}
				}
				t.Logf("the write of generation 1 came second, and was not applied, in %d of %d rounds", cameSecond, rounds)
				result.report(t)
			})

			t.Run("key changes behind a barrier", func(t *testing.T) {
				var result tally
				for round := 1; round <= rounds; round++ {
					session := fmt.Sprintf("change-round-%d", round)
					if _, err := s.RegisterPodSession(ctx, registrarA, pod, session, k0, 1); err != nil {
						t.Fatalf("round %d: register %s with %s: %v", round, session, k0, err)
					}
					a, b := together(write(session, k1, 1), write(session, k2, 1))
					if a.err != nil || b.err != nil {
						result.add(round, fmt.Sprintf("the write with %s = %v and with %s = %v, want both written", k1, a.err, k2, b.err), "")
						continue
					}
					var rule, replaced string
					switch jkt, generation := rowOf(t, pool, pod, session); {
					case generation != 1 || (jkt != k1 && jkt != k2):
						rule = fmt.Sprintf("the row holds %s at generation %d, want %s or %s at generation 1", jkt, generation, k1, k2)
					case !a.stored.Applied || a.stored.JKT != k1 || !b.stored.Applied || b.stored.JKT != k2:
						rule = fmt.Sprintf("the writes answered %+v and %+v, want each applied, bound to its own key", a.stored, b.stored)
					default:
						first, second := a.stored, b.stored
						if jkt == k1 {
							first, second = b.stored, a.stored
						}
						if first.Replaced != k0 || second.Replaced != first.JKT {
							replaced = fmt.Sprintf("the first to commit answered %+v and the second %+v, want the first replacing %s and the second replacing %s",
								first, second, k0, first.JKT)
						}
					}
					result.add(round, rule, replaced)
				}
				result.report(t)
			})
		})
	}
}

// TestARegistrationInsideAnotherTransactionIsRefused: RegisterPodSession opens a transaction of
// its own at read committed, which the generation rule and the replaced key rest on, so it
// refuses a context that already carries a transaction, whose level and lifetime it would not
// choose, and writes nothing in it.
func TestARegistrationInsideAnotherTransactionIsRefused(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = s.RegisterPodSession(oauthstore.WithTx(ctx, tx), registrarA, "pod-uid-issue", "implementer-g1", "thumbprint-a", 1)
	var rows int
	if scanErr := tx.QueryRow(ctx, `select count(*) from oauth_pod_sessions`).Scan(&rows); scanErr != nil {
		t.Fatalf("count the registrations in the transaction: %v", scanErr)
	}
	if err == nil || rows != 0 {
		t.Fatalf("RegisterPodSession inside another transaction = %v, and that transaction holds %d registrations; want it refused, writing none", err, rows)
	}
}

// registrationsOf is every registration of session, each as its pod and the registrar that wrote
// it, in pod order.
func registrationsOf(t *testing.T, pool *pgxpool.Pool, session string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select pod_uid || ' of ' || coalesce(registrar, 'no registrar') from oauth_pod_sessions
		where session = $1 order by pod_uid`, session)
	if err != nil {
		t.Fatalf("read the registrations of %s: %v", session, err)
	}
	registrations, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the registrations of %s: %v", session, err)
	}
	return registrations
}

// TestTwoDaemonsFirstWritesOfOneNameAdmitOne: two registrars' first writes of one session name,
// each for a pod of its own, touch no row of oauth_pod_sessions in common, so nothing but the
// primary key of the name's binding (pod_session_registrars.session_name) keeps both from being
// admitted. registrarA's write is held open once it has bound the name and inserted its row,
// before it commits (the store's write hook), and registrarB's starts then, on another replica of
// the broker; A's is let go once B's has returned or is seen waiting on a lock behind A's backend.
// B's insert of the binding conflicts on that key with A's uncommitted one, so it waits, then
// does nothing once A's commits, and B's write reads the name bound to A and is refused
// ErrPodSessionHeld naming A, and A's row alone stands.
//
// Control: make the binding insert's on conflict (session_name) do nothing a do update set
// registrar = excluded.registrar, the last writer winning, and B's write, which still waits on
// A's, then takes the binding, is applied, and both rows stand.
func TestTwoDaemonsFirstWritesOfOneNameAdmitOne(t *testing.T) {
	ctx := context.Background()
	_, pool := newStore(t)
	first, second := oauthstore.New(pool), oauthstore.New(pool)
	const session, podA, podB, kA, kB = "implementer-g1", "pod-uid-of-a", "pod-uid-of-b", "thumbprint-of-a", "thumbprint-of-b"
	heldBy := make(chan uint32, 1)
	resume := make(chan struct{})
	letGo := sync.OnceFunc(func() { close(resume) })
	defer letGo()
	oauthstore.SetPodSessionWriteHook(first, func(ctx context.Context) {
		tx, _ := oauthstore.TxFrom(ctx)
		heldBy <- tx.Conn().PgConn().PID()
		<-resume
	})

	firstDone, secondDone := make(chan writeAnswer, 1), make(chan writeAnswer, 1)
	go func() {
		stored, err := first.RegisterPodSession(ctx, registrarA, podA, session, kA, 1)
		firstDone <- writeAnswer{stored, err}
	}()
	var pid uint32
	select {
	case pid = <-heldBy:
	case a := <-firstDone:
		t.Fatalf("%s's first write of %s = %+v, %v before it was held open, want it held after its insert", registrarA, session, a.stored, a.err)
	case <-time.After(10 * time.Second):
		t.Fatalf("%s's first write of %s was not held open after its insert within ten seconds", registrarA, session)
	}

	go func() {
		stored, err := second.RegisterPodSession(ctx, registrarB, podB, session, kB, 1)
		secondDone <- writeAnswer{stored, err}
	}()
	var answer writeAnswer
	returnedWhileHeld := false
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		select {
		case answer = <-secondDone:
			returnedWhileHeld = true
		default:
		}
		if returnedWhileHeld {
			break
		}
		waiting := storetest.LockWaitersBehind(t, pool, pid)
		if len(waiting) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s's write of %s had neither returned nor waited on a lock behind %s's held write (backend %d) after ten seconds; the last poll saw the backends %v waiting behind it",
				registrarB, session, registrarA, pid, waiting)
		}
	}
	letGo()
	held := <-firstDone
	if !returnedWhileHeld {
		answer = <-secondDone
	}

	if held.err != nil || !held.stored.Applied || held.stored.JKT != kA {
		t.Errorf("%s's first write of %s for %s = %+v, %v; want it applied, bound to %s", registrarA, session, podA, held.stored, held.err, kA)
	}
	var refused *oauthstore.PodSessionHeldError
	if !errors.As(answer.err, &refused) || !errors.Is(answer.err, oauthstore.ErrPodSessionHeld) || refused.Session != session || refused.Holder != registrarA {
		t.Errorf("%s's first write of %s for %s, racing %s's = %+v, %v; want it refused ErrPodSessionHeld naming %s as the holder of %s",
			registrarB, session, podB, registrarA, answer.stored, answer.err, registrarA, session)
	}
	if returnedWhileHeld {
		t.Errorf("%s's write of %s returned while %s's write of it was held open, uncommitted; want it waiting on the name's binding until that write committed", registrarB, session, registrarA)
	}
	if got, want := registrationsOf(t, pool, session), []string{podA + " of " + registrarA}; !slices.Equal(got, want) {
		t.Errorf("oauth_pod_sessions holds %v for %s, want %v alone: both registrars' first writes of the name were applied", got, session, want)
	}
}

// TestAWriteTakesARowNoLiveRegistrationOfItsOwnHolds: a row of a pod and name that is no live,
// unreleased registration of the writing registrar's own is taken whole by that registrar's next
// write. Each row is written by hand with no binding of its name, to reach states the route does
// not produce: a released row of registrarA's, a lapsed row of registrarB's and a live row no
// registrar wrote (as a broker of the build before registrars writes one), each bound to an old
// key at a high generation. Before A's write the exchange honours none of them, in the order
// PodSessionRegistered answers: the released row is refused as released, naming A, before the
// name's missing binding is read, and the other two are answered as no registration is, the
// lapsed one being no live row and the unnamed one naming no registrar, so neither reaches the
// binding either. A's write binds each name to A, which none was bound to, and sets each row to
// A, the key A's write names (none, for a write naming none), A's lower generation and a fresh
// expiry, with no release mark, so nothing of the registration that ended carries over and the
// exchange honours the pod again. A lapsed row of A's own, which A did not release, keeps the
// generation rule: A's write of a lower generation binds the name and is not applied, and the row
// is left as it was.
func TestAWriteTakesARowNoLiveRegistrationOfItsOwnHolds(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)
	const kOld, kNew, high = "thumbprint-of-the-ended-registration", "thumbprint-of-the-next-one", 9
	for _, c := range []struct {
		what, pod, session string
		// The registrar of the row before A's write ("" for none), whether it is released, and
		// whether it has lapsed.
		registrar        string
		released, lapsed bool
		// The key A's write names, and whether that write takes the row whole.
		jkt   string
		taken bool
	}{
		{"a released row of A's", "pod-uid-released", "released-session", registrarA, true, false, kNew, true},
		{"a released row of A's, written again naming no key", "pod-uid-released-unbound", "released-unbound-session", registrarA, true, false, "", true},
		{"a lapsed row of B's", "pod-uid-lapsed-of-b", "lapsed-of-b-session", registrarB, false, true, kNew, true},
		{"a live row no registrar wrote", "pod-uid-unnamed", "unnamed-session", "", false, false, kNew, true},
		{"a lapsed row of A's own", "pod-uid-lapsed-of-a", "lapsed-of-a-session", registrarA, false, true, kNew, false},
	} {
		t.Run(c.what, func(t *testing.T) {
			if _, err := pool.Exec(ctx, `insert into oauth_pod_sessions (pod_uid, session, expires_at, jkt, generation, registrar, released_at)
				values ($1, $2, case when $3 then now() - interval '1 minute' else now() + interval '1 day' end, $4, $5, nullif($6, ''), case when $7 then now() end)`,
				c.pod, c.session, c.lapsed, kOld, high, c.registrar, c.released); err != nil {
				t.Fatalf("insert %s: %v", c.what, err)
			}
			registered, err := s.PodSessionRegistered(ctx, c.pod, c.session, kOld)
			var released *oauthstore.PodSessionReleasedError
			switch {
			case c.released && (registered || !errors.As(err, &released) || !errors.Is(err, oauthstore.ErrPodSessionReleased) || released.Registrar != registrarA):
				t.Fatalf("PodSessionRegistered of %s with its own key = %t, %v; want false, ErrPodSessionReleased naming %s", c.what, registered, err, registrarA)
			case !c.released && (registered || err != nil):
				t.Fatalf("PodSessionRegistered of %s with its own key = %t, %v; want false and no error, as for no registration", c.what, registered, err)
			}

			stored, err := s.RegisterPodSession(ctx, registrarA, c.pod, c.session, c.jkt, 1)
			row := registrationRow(t, pool, c.pod, c.session)
			if holder, _ := bindingOf(t, pool, c.session); holder != registrarA {
				t.Fatalf("%s's write to %s = %+v, %v, leaving %s bound to %q; want it bound to %s: a write of a name bound to none binds it", registrarA, c.what, stored, err, c.session, holder, registrarA)
			}
			if !c.taken {
				want := fmt.Sprintf("registrar %s, key %q, generation %d, released false, live false", registrarA, kOld, high)
				if err != nil || stored.Applied || stored.JKT != kOld || stored.Generation != high || row != want {
					t.Fatalf("%s's write of generation 1 with %s to %s = %+v, %v, leaving the row %s; want it not applied, answered %s at generation %d, and the row %s: a lapsed row of the registrar's own keeps the generation rule",
						registrarA, c.jkt, c.what, stored, err, row, kOld, high, want)
				}
				return
			}
			want := fmt.Sprintf("registrar %s, key %q, generation 1, released false, live true", registrarA, c.jkt)
			fresh := time.Now().Add(oauthstore.PodSessionRetention - time.Hour)
			if err != nil || !stored.Applied || stored.JKT != c.jkt || stored.Generation != 1 || stored.ExpiresAt.Before(fresh) || stored.Replaced != kOld || row != want {
				t.Fatalf("%s's write of generation 1 with %q to %s = %+v, %v, leaving the row %s; want it applied, bound to %q at generation 1, expiring a retention from now, replacing %s, and the row %s: the row is taken whole",
					registrarA, c.jkt, c.what, stored, err, row, c.jkt, kOld, want)
			}
			if registered, err := s.PodSessionRegistered(ctx, c.pod, c.session, kNew); !registered || err != nil {
				t.Fatalf("PodSessionRegistered of %s with %s after %s's write took it = %t, %v; want true", c.what, kNew, registrarA, registered, err)
			}
		})
	}
}

// TestARegistrationNamesTheBindingOnce: a session name's first registration binds it to its
// registrar, and nothing after moves that binding. registrarA's write of pod P for S binds S to A
// at the moment of that write. A's write of a second pod P2 for S, as a relaunch writes, is
// applied and leaves the binding as it was, its moment included. Once every row of S has lapsed
// and the sweep has deleted them all, registrarB's write of S is refused ErrPodSessionHeld naming
// A and writes nothing, and the binding is still A's from that first moment. That last case is one
// no other test holds: the registration route's tests lapse a name's rows and leave them in the
// table, and here no row of the name is left at all, so a check that read the name's rows, lapsed
// or live, would admit B.
func TestARegistrationNamesTheBindingOnce(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)
	const session, pod, relaunched, podB = "implementer-g1", "pod-uid-first", "pod-uid-relaunched", "pod-uid-of-b"

	if _, err := s.RegisterPodSession(ctx, registrarA, pod, session, "", 0); err != nil {
		t.Fatalf("%s's first write of %s for %s: %v", registrarA, session, pod, err)
	}
	holder, since := bindingOf(t, pool, session)
	if holder != registrarA || since.IsZero() {
		t.Fatalf("after %s's first write of %s, pod_session_registrars binds it to %q since %v; want %s", registrarA, session, holder, since, registrarA)
	}
	if stored, err := s.RegisterPodSession(ctx, registrarA, relaunched, session, "", 0); err != nil || !stored.Applied {
		t.Fatalf("%s's write of %s for a second pod, %s = %+v, %v; want it applied", registrarA, session, relaunched, stored, err)
	}
	if got, at := bindingOf(t, pool, session); got != registrarA || !at.Equal(since) {
		t.Fatalf("after %s's write of %s for a second pod, %s is bound to %q since %v; want %s since %v, as its first write bound it", registrarA, session, session, got, at, registrarA, since)
	}

	if _, err := pool.Exec(ctx, `update oauth_pod_sessions set expires_at = now() - interval '1 minute' where session = $1`, session); err != nil {
		t.Fatalf("lapse the registrations of %s: %v", session, err)
	}
	if err := s.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := registrationsOf(t, pool, session); len(got) != 0 {
		t.Fatalf("the sweep left the registrations %v of %s, want none", got, session)
	}

	_, err := s.RegisterPodSession(ctx, registrarB, podB, session, "", 0)
	var refused *oauthstore.PodSessionHeldError
	if !errors.As(err, &refused) || !errors.Is(err, oauthstore.ErrPodSessionHeld) || refused.Session != session || refused.Holder != registrarA {
		t.Errorf("%s's write of %s once the sweep deleted every row of it = %v; want it refused ErrPodSessionHeld naming %s as the holder of %s", registrarB, session, err, registrarA, session)
	}
	if got := registrationsOf(t, pool, session); len(got) != 0 {
		t.Errorf("oauth_pod_sessions holds %v for %s after %s's refused write, want none", got, session, registrarB)
	}
	if got, at := bindingOf(t, pool, session); got != registrarA || !at.Equal(since) {
		t.Errorf("after %s's refused write of %s, %s is bound to %q since %v; want %s since %v", registrarB, session, session, got, at, registrarA, since)
	}
}

// bindingOf is the registrar session is bound to and the moment it was bound, or "" and the zero
// time for a name bound to none.
func bindingOf(t *testing.T, pool *pgxpool.Pool, session string) (registrar string, since time.Time) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `select registrar, registered_at from pod_session_registrars where session_name = $1`, session).Scan(&registrar, &since)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", time.Time{}
	case err != nil:
		t.Fatalf("read the binding of %s: %v", session, err)
	}
	return registrar, since
}

// registrationRow is the pod's registration for session as the table holds it: its registrar,
// its key, its generation, whether it is released and whether it is live.
func registrationRow(t *testing.T, pool *pgxpool.Pool, pod, session string) string {
	t.Helper()
	var registrar, jkt string
	var generation int64
	var released, live bool
	if err := pool.QueryRow(context.Background(), `select coalesce(registrar, ''), coalesce(jkt, ''), generation, released_at is not null, expires_at > now()
		from oauth_pod_sessions where pod_uid = $1 and session = $2`, pod, session).Scan(&registrar, &jkt, &generation, &released, &live); err != nil {
		t.Fatalf("read the registration of %s for %s: %v", pod, session, err)
	}
	return fmt.Sprintf("registrar %s, key %q, generation %d, released %t, live %t", registrar, jkt, generation, released, live)
}
