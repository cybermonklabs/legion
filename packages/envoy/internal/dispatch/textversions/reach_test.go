package textversions

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/textversions/textversionstest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestTheReachStaysBeforeAnOpenTransactionThatWroteAnEvent holds the reach before the start of a
// transaction that wrote an event and is still open: transaction A writes an event and stays open
// while B's later event commits. The horizon stands below A's event, so a pass of the fill writes
// neither, and the reach it stores and the reach a read computes stay before A's start; once A
// commits and before the fill writes its event they still do. Once the fill runs, the reach passes
// both starts and both events have their rows.
func TestTheReachStaysBeforeAnOpenTransactionThatWroteAnEvent(t *testing.T) {
	database, broker, fill := reachFixture(t)
	ctx := t.Context()
	a, aStart := textversionstest.BeginTransaction(t, database)
	defer func() { _ = a.Rollback(context.Background()) }()
	pastTheMargin(t, database, aStart)
	aEvent := appendCommentEvent(t, a, broker, "comment.created", uuid.NewString(), "The audit starts on Tuesday.", aStart)
	b := commitComment(t, database, broker, otherIssue)
	if _, err := fill.pass(ctx); err != nil {
		t.Fatalf("a pass of the fill while A is open: %v", err)
	}
	if rows := textversionstest.RowsOf(t, database, aEvent, b.id); rows != 0 {
		t.Fatalf("the fill wrote %d rows of events the horizon had not passed", rows)
	}
	requireReachBefore(t, database, aStart, "while A, which wrote an event, is open")
	if err := a.Commit(ctx); err != nil {
		t.Fatalf("commit A: %v", err)
	}
	requireReachBefore(t, database, aStart, "once A committed, before the fill wrote its event")
	if err := fill.toHorizon(ctx); err != nil {
		t.Fatalf("the fill: %v", err)
	}
	textversionstest.RequireRowsOf(t, database, textversionstest.OneEach(aEvent, b.id))
	awaitReachPast(t, database, aStart, b.startedAt)
}

// TestTheReachStaysBeforeAnOpenTransactionThatHasWrittenNothing holds the reach before the start
// of an open transaction that has written no event yet: transaction A begins and has written
// nothing while B's event commits, and the fill writes B's event, which the horizon passes since
// A holds no transaction id. The reach stays before A's start all the same, since A can still
// write an event that carries it; once A writes one and commits, and before the fill writes it,
// the reach still does. Once the fill runs, the reach passes both starts and both events have
// their rows.
func TestTheReachStaysBeforeAnOpenTransactionThatHasWrittenNothing(t *testing.T) {
	database, broker, fill := reachFixture(t)
	ctx := t.Context()
	a, aStart := textversionstest.BeginTransaction(t, database)
	defer func() { _ = a.Rollback(context.Background()) }()
	pastTheMargin(t, database, aStart)
	b := commitComment(t, database, broker, otherIssue)
	if err := fill.toHorizon(ctx); err != nil {
		t.Fatalf("the fill while A is open: %v", err)
	}
	textversionstest.RequireRowsOf(t, database, textversionstest.OneEach(b.id))
	requireReachBefore(t, database, aStart, "while A, which has written nothing, is open")
	aEvent := appendCommentEvent(t, a, broker, "comment.created", uuid.NewString(), "The audit starts on Tuesday.", aStart)
	if err := a.Commit(ctx); err != nil {
		t.Fatalf("commit A: %v", err)
	}
	requireReachBefore(t, database, aStart, "once A wrote an event and committed, before the fill wrote it")
	if err := fill.toHorizon(ctx); err != nil {
		t.Fatalf("the fill: %v", err)
	}
	textversionstest.RequireRowsOf(t, database, textversionstest.OneEach(aEvent, b.id))
	awaitReachPast(t, database, aStart, b.startedAt)
}

// TestTheReachStaysBeforeACommittedEventTheFillHasNotWritten holds the reach before the start of a
// committed event the fill has not written, though the cursor stands past a later one: A begins
// first and B second, B's event commits, and then A's, which draws the later id and carries the
// earlier start. A batch up to the horizon a pass read before A committed writes B's event and
// not A's, so the cursor stands past an event whose start is later than A's; the reach it stores
// and the reach a read computes stay before A's start, which no transaction holds open any more.
// Once the fill runs, the reach passes both starts and both events have their rows.
func TestTheReachStaysBeforeACommittedEventTheFillHasNotWritten(t *testing.T) {
	database, broker, fill := reachFixture(t)
	ctx := t.Context()
	a, aStart := textversionstest.BeginTransaction(t, database)
	defer func() { _ = a.Rollback(context.Background()) }()
	pastTheMargin(t, database, aStart)
	b := commitComment(t, database, broker, otherIssue)
	safe, err := fill.horizon.Await(ctx)
	if err != nil {
		t.Fatalf("await the horizon: %v", err)
	}
	if safe != b.id {
		t.Fatalf("the horizon is %d; want B's event %d, the newest", safe, b.id)
	}
	aEvent := appendCommentEvent(t, a, broker, "comment.created", uuid.NewString(), "The audit starts on Tuesday.", aStart)
	if err := a.Commit(ctx); err != nil {
		t.Fatalf("commit A: %v", err)
	}
	if aEvent <= b.id {
		t.Fatalf("A's event %d does not come after B's %d", aEvent, b.id)
	}
	if _, err := fill.batch(ctx, safe); err != nil {
		t.Fatalf("a batch up to the horizon read before A committed: %v", err)
	}
	textversionstest.RequireRowsOf(t, database, textversionstest.OneEach(b.id))
	if rows := textversionstest.RowsOf(t, database, aEvent); rows != 0 {
		t.Fatalf("the batch up to event %d wrote %d rows of A's event %d", safe, rows, aEvent)
	}
	requireReachBefore(t, database, aStart, "after A committed an event the fill has not written")
	if err := fill.toHorizon(ctx); err != nil {
		t.Fatalf("the fill: %v", err)
	}
	textversionstest.RequireRowsOf(t, database, textversionstest.OneEach(aEvent, b.id))
	awaitReachPast(t, database, aStart, b.startedAt)
}

// TestTheReachStaysBeforeANoInheritMembersOpenTransaction: a login the test creates NOINHERIT,
// a member of a role that may insert into events and holding no privilege of its own, can write
// an event after a SET ROLE. While it holds a transaction open, later events commit and the fill
// writes them, and the reach stays before that transaction's start; once it ends, the reach
// passes it.
func TestTheReachStaysBeforeANoInheritMembersOpenTransaction(t *testing.T) {
	database, broker, fill := reachFixture(t)
	ctx := t.Context()
	group := createRole(t, database, "writers", "nologin")
	login, password := createLoginRole(t, database, "member", "noinherit")
	for _, statement := range []string{
		`grant insert on events to ` + group,
		`grant ` + group + ` to ` + login,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	var inherits, inserts bool
	if err := database.Pool.QueryRow(ctx, `
		select r.rolinherit, has_table_privilege(r.oid, 'public.events', 'INSERT') from pg_roles r where r.rolname = $1
	`, login).Scan(&inherits, &inserts); err != nil {
		t.Fatalf("read the login's role: %v", err)
	}
	if inherits || inserts {
		t.Fatalf("the login inherits: %t, may insert into events itself: %t; want neither", inherits, inserts)
	}
	member := storeAs(t, database, login, password)
	held, heldStart := textversionstest.BeginTransaction(t, member)
	defer func() { _ = held.Rollback(context.Background()) }()
	pastTheMargin(t, database, heldStart)
	b := commitComment(t, database, broker, otherIssue)
	if err := fill.toHorizon(ctx); err != nil {
		t.Fatalf("the fill while the member's transaction is open: %v", err)
	}
	textversionstest.RequireRowsOf(t, database, textversionstest.OneEach(b.id))
	requireReachBefore(t, database, heldStart, "while a NOINHERIT member of a role with insert on events holds a transaction open")
	if err := held.Rollback(ctx); err != nil {
		t.Fatalf("end the member's transaction: %v", err)
	}
	awaitReachPast(t, database, heldStart, b.startedAt)
}

// TestTheReachHoldsForATransactionWhoseStartItCannotRead: the reach is read by a role that cannot
// read another role's transaction start, one without pg_read_all_stats and in no role of the
// other's, as a deployment's Dispatch role may be. While a role that may insert into events holds
// a transaction open, the reach that reader computes does not move, though time passes; once that
// role may insert no more and holds a transaction open again, the reach moves. The test creates
// both login roles; a database that refuses it fails the test, naming why.
func TestTheReachHoldsForATransactionWhoseStartItCannotRead(t *testing.T) {
	database, _, _ := reachFixture(t)
	ctx := t.Context()
	writer, writerPassword := createLoginRole(t, database, "writer", "")
	reader, readerPassword := createLoginRole(t, database, "reader", "")
	for _, statement := range []string{
		`grant insert on events to ` + writer,
		`grant select on events, search_text_versions_fill to ` + reader,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	var readsStats, readsWriter bool
	if err := database.Pool.QueryRow(ctx, `
		select pg_has_role($1, 'pg_read_all_stats', 'USAGE'), pg_has_role($1, $2, 'USAGE')
	`, reader, writer).Scan(&readsStats, &readsWriter); err != nil {
		t.Fatalf("read the reader's roles: %v", err)
	}
	if readsStats || readsWriter {
		t.Fatalf("the reader has pg_read_all_stats: %t, has the writer's privileges: %t; want neither", readsStats, readsWriter)
	}
	asWriter := storeAs(t, database, writer, writerPassword)
	asReader := storeAs(t, database, reader, readerPassword)
	stored := readCursorRow(t, database).reach
	if stored == nil {
		t.Fatal("the caught-up fill stored no reach")
	}

	held, _ := textversionstest.BeginTransaction(t, asWriter)
	defer func() { _ = held.Rollback(context.Background()) }()
	var shownStart *time.Time
	if err := asReader.Pool.QueryRow(ctx, `
		select a.xact_start from pg_stat_activity a join pg_roles r on r.oid = a.usesysid
		 where r.rolname = $1 and a.datname = current_database()
	`, writer).Scan(&shownStart); err != nil {
		t.Fatalf("read the writer's backend as the reader: %v", err)
	}
	if shownStart != nil {
		t.Fatalf("the reader reads the writer's transaction start %s; want it hidden", shownStart)
	}
	first := reachAs(t, asReader)
	// A reach computed after a second would be past the first, which T0 - 1 s bounds.
	time.Sleep(1100 * time.Millisecond)
	second := reachAs(t, asReader)
	if !first.Equal(*stored) || !second.Equal(*stored) {
		t.Fatalf("while the writer's transaction is open the reader reads the reach %s, then %s; want the stored %s both times",
			first, second, stored)
	}
	if err := held.Rollback(ctx); err != nil {
		t.Fatalf("end the writer's transaction: %v", err)
	}

	if _, err := database.Pool.Exec(ctx, `revoke insert on events from `+writer); err != nil {
		t.Fatalf("revoke the writer's insert: %v", err)
	}
	idle, _ := textversionstest.BeginTransaction(t, asWriter)
	defer func() { _ = idle.Rollback(context.Background()) }()
	time.Sleep(1100 * time.Millisecond)
	if third := reachAs(t, asReader); !third.After(second) {
		t.Fatalf("while a role that may not insert into events holds a transaction open the reader reads the reach %s; want it past %s",
			third, second)
	}
}

// reachFixture is a database holding textversionstest.Issue and otherIssue, a broker, and a fill
// on it caught up with one event, so it has stored a reach from a cursor at the log's newest
// event.
func reachFixture(t *testing.T) (*store.Store, *events.Broker, *filler) {
	t.Helper()
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	seedOtherIssue(t, database)
	broker := events.NewBroker()
	fill := testFiller(database, events.NewHorizon(database.Pool), io.Discard)
	commitComment(t, database, broker, textversionstest.Issue)
	if err := fill.toHorizon(t.Context()); err != nil {
		t.Fatalf("catch the fill up: %v", err)
	}
	return database, broker, fill
}

// pastTheMargin waits until the database's clock is more than a second past start. A reach is
// never later than T0 - 1 s (store.ComputeTextVersionsReach), so a reach read within a second of
// start stays before it whatever the computation reads; past that margin only start's own
// transaction, or an event that carries it, holds the reach before start, which is what the
// reach tests prove. The wait is Eventually's minute at a pace of its own, 100 ms, since each
// read of the clock is a statement to the database.
func pastTheMargin(t *testing.T, database *store.Store, start time.Time) {
	t.Helper()
	past := start.Add(time.Second + 100*time.Millisecond)
	testwait.WithinPolling(t, testwait.Bounded(t, time.Minute, time.Second), 100*time.Millisecond,
		fmt.Sprintf("the database's clock to pass %s", past.Format(time.RFC3339Nano)), func() bool {
			var now time.Time
			if err := database.Pool.QueryRow(t.Context(), `select clock_timestamp()`).Scan(&now); err != nil {
				t.Fatalf("read the database's clock: %v", err)
			}
			return now.After(past)
		})
}

// requireReachBefore holds the reach the fill stored, and the reach a read computes now
// (store.TextVersionsReach), before at.
func requireReachBefore(t *testing.T, database *store.Store, at time.Time, when string) {
	t.Helper()
	stored := readCursorRow(t, database).reach
	if stored != nil && !stored.Before(at) {
		t.Fatalf("%s the fill stored the reach %s, not before %s", when, stored, at)
	}
	reach, reached, err := database.TextVersionsReach(t.Context())
	if err != nil {
		t.Fatalf("%s read the reach: %v", when, err)
	}
	if !reached || !reach.Before(at) {
		t.Fatalf("%s the reach read is %s (reached: %t); want one before %s", when, reach, reached, at)
	}
}

// awaitReachPast waits until the reach a read computes is past every one of times, which it is
// once a second has passed since the last of them and no transaction holds it back.
func awaitReachPast(t *testing.T, database *store.Store, times ...time.Time) {
	t.Helper()
	var reach time.Time
	var reached bool
	if !testwait.Holds(15*time.Second, 100*time.Millisecond, func() bool {
		var err error
		if reach, reached, err = database.TextVersionsReach(t.Context()); err != nil {
			t.Fatalf("read the reach: %v", err)
		}
		past := reached
		for _, at := range times {
			past = past && reach.After(at)
		}
		return past
	}) {
		t.Fatalf("the reach is %s (reached: %t) 15 s on; want it past %v", reach, reached, times)
	}
}

// reachAs reads the reach as database's role, which must have reached a time.
func reachAs(t *testing.T, database *store.Store) time.Time {
	t.Helper()
	reach, reached, err := database.TextVersionsReach(t.Context())
	if err != nil || !reached {
		t.Fatalf("read the reach: %s, reached: %t, %v", reach, reached, err)
	}
	return reach
}

// createRole creates a role named for kind with options, which CREATE ROLE takes, and drops it
// once the test ends, its privileges on this database's tables revoked first. A database that
// refuses to create it fails the test, naming what the test needs.
func createRole(t *testing.T, database *store.Store, kind, options string) string {
	t.Helper()
	name := "keyword_fill_" + kind + "_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	if _, err := database.Pool.Exec(t.Context(), "create role "+name+" "+options); err != nil {
		t.Fatalf("create role %s: the database refused it (%v); this test needs a role that may create roles (CREATEROLE) or a superuser", name, err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		for _, statement := range []string{
			`revoke all on all tables in schema public from ` + name,
			`drop role ` + name,
		} {
			if _, err := database.Pool.Exec(ctx, statement); err != nil {
				t.Errorf("%s: %v", statement, err)
			}
		}
	})
	return name
}

// createLoginRole creates a login role named for kind, with options and a password of its own
// (createRole), and answers its name and password.
func createLoginRole(t *testing.T, database *store.Store, kind, options string) (string, string) {
	t.Helper()
	password := strings.ReplaceAll(uuid.NewString(), "-", "")
	return createRole(t, database, kind, "login password '"+password+"' "+options), password
}

// storeAs is a store on database's own database that connects as role, closed once the test
// ends, before role is dropped.
func storeAs(t *testing.T, database *store.Store, role, password string) *store.Store {
	t.Helper()
	return storetest.Reopened(t, database, func(config *pgxpool.Config) {
		config.ConnConfig.User, config.ConnConfig.Password = role, password
		config.MinConns, config.MinIdleConns = 0, 0
	})
}
