package api

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/testwait"
)

// fingerprint is one read of every table of a server's database (databaseFingerprint): each
// table's row count and the md5 of its rows, by table name; when its reads ended; and the next
// background write the wait before them left armed (quiet), which a fingerprint read after
// quiet.At cannot tell from the write of a request between the two.
type fingerprint struct {
	tables map[string]string
	ended  time.Time
	quiet  docs.BackgroundQuiet
}

// backgroundBound is how long one wait for a server's background writes, with the reads of the
// fingerprint after it, may take before it fails naming what had not ended; less when the test
// binary's deadline, less binaryDeadlineMargin, comes first (backgroundContext).
const backgroundBound = time.Minute

// backgroundSoon is how far ahead the wait looks for a write armed to start later: a document
// settlement armed to run within it is waited for, and one armed for after it is left armed and
// recorded with the moment it may start (docs.BackgroundQuiet), so a test whose document
// settlement is an hour away does not wait the hour.
const backgroundSoon = 10 * time.Second

// databaseFingerprint hashes every row of every table of served's database, so a test can tell a
// request wrote nothing anywhere: no row inserted, updated or deleted. It first waits out every
// write the server's earlier requests left to its background workers (backgroundWrites): one that
// commits between two fingerprints would read as the write of the request between them. It takes
// the server rather than the store so that no fingerprint can be taken without that wait. A write
// the wait leaves armed for later is recorded with the moment it may start (fingerprint.quiet),
// and assertUnchanged fails by name when the second fingerprint read on past that moment. The wait
// and the reads share one bound (backgroundContext), so a writer that never ends, a pool with no
// connection free or a lock that holds a read fails the test by name before the binary's deadline.
func databaseFingerprint(t *testing.T, served *server) fingerprint {
	t.Helper()
	return takeFingerprint(t, served, nil)
}

// heldWriter is one background writer a test of the wait (database_fingerprint_wait_test.go) holds
// past the answer of the request that left it. release, which the fingerprint calls as its wait
// begins, lets the writer go heldWriterLag later, so a wait that did not wait for it is reading by
// the time it writes; ended answers whether it has written, which the fingerprint asks as its reads
// begin, so such a wait fails naming the writer, whatever its reads then see.
type heldWriter struct {
	name    string
	release func()
	ended   func() bool
}

// heldWriterLag is how long after a fingerprint's wait begins a held writer is let go (heldWriter).
// It guards the red of each test of the wait: a wait that skips the held writer's branch must
// reach its reads before the writer writes, so the lag must outlast one pass of the wait over the
// writers it does not skip - a few counts read under their locks, the document service's two looks
// at its rooms and timers, and its statements, one listing the documents with a row after their
// first and one check of each. Beside a machine at a one-minute load of about 600, a whole pass of
// the wait over a server holding such a document took at most 9 ms in 100 passes, so 2 s leaves a
// pass room to run two hundred times slower before a skipped branch could go unseen. A wait that
// did not skip the writer waits out the lag and then the writer, so the lag cannot fail a test; it
// lengthens each by 2 s, a thirtieth of the minute the wait itself may take (backgroundBound).
const heldWriterLag = 2 * time.Second

// takeFingerprint is databaseFingerprint, holding held, when it is not nil, through the wait: its
// release runs as the wait for the server's background writes begins, and the reads begin only
// once it has ended, or the test fails naming it.
func takeFingerprint(t *testing.T, served *server, held *heldWriter) fingerprint {
	t.Helper()
	ctx, cancel := backgroundContext(t)
	defer cancel()
	return fingerprintWithin(t, ctx, served, held)
}

// fingerprintWithin is takeFingerprint under ctx's bound, failing t: a test of the fingerprint's
// refusal reads what it failed with (refusedFingerprint).
func fingerprintWithin(t testing.TB, ctx context.Context, served *server, held *heldWriter) fingerprint {
	t.Helper()
	var waiting func()
	if held != nil {
		waiting = held.release
	}
	quiet := backgroundWrites(t, ctx, served, waiting)
	if held != nil && !held.ended() {
		t.Fatalf("the fingerprint's reads began while %s had not written: its wait did not wait for it", held.name)
	}
	database := served.deps.Store
	rows, err := database.Pool.Query(ctx, `select tablename from pg_tables where schemaname = 'public'`)
	if err != nil {
		t.Fatalf("list tables: %v", boundedError(ctx, err))
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read table names: %v", boundedError(ctx, err))
	}
	if len(tables) == 0 {
		t.Fatal("the schema has no tables; a fingerprint of it proves nothing")
	}
	taken := fingerprint{tables: make(map[string]string, len(tables)), quiet: quiet}
	for _, table := range tables {
		var digest string
		if err := database.Pool.QueryRow(ctx, fmt.Sprintf(
			`select count(*) || ':' || coalesce(md5(string_agg(t::text, ',' order by t::text)), '') from %s t`,
			pgx.Identifier{table}.Sanitize(),
		)).Scan(&digest); err != nil {
			t.Fatalf("fingerprint %s: %v", table, boundedError(ctx, err))
		}
		taken.tables[table] = digest
	}
	taken.ended = time.Now()
	if !quiet.At.IsZero() && !taken.ended.Before(quiet.At) {
		t.Fatalf("%s could write from %s, before this fingerprint's reads ended at %s, so the fingerprint is not one state of the database",
			quiet.Writer, quiet.At.Format(time.RFC3339Nano), taken.ended.Format(time.RFC3339Nano))
	}
	return taken
}

// refusedFingerprint takes a fingerprint of served within bound, on a goroutine of its own
// (testwait.FailureOf), and answers what it failed with, or "" when it was taken: a test of the
// fingerprint's refusal reads the fingerprint's own answer, not the code that decides it.
func refusedFingerprint(t *testing.T, served *server, bound time.Duration) string {
	t.Helper()
	ctx, cancel := context.WithTimeoutCause(context.Background(), bound, fmt.Errorf("the refused fingerprint's bound of %s ended", bound))
	defer cancel()
	return testwait.FailureOf(t, func(t testing.TB) { fingerprintWithin(t, ctx, served, nil) })
}

// assertUnchanged fails for each table whose rows differ between two fingerprints of one database,
// and fails by name when a background write the first fingerprint's wait left armed could start
// before the second's reads ended: a table it changed would read as the request's.
func assertUnchanged(t *testing.T, request string, before, after fingerprint) {
	t.Helper()
	for _, failure := range unchangedFailures(request, before, after) {
		t.Error(failure)
	}
}

// unchangedFailures is what assertUnchanged fails with for two fingerprints of one database, one
// failure each, and nothing when the pair shows that no write landed between them.
func unchangedFailures(request string, before, after fingerprint) []string {
	var failures []string
	if !before.quiet.At.IsZero() && !after.ended.Before(before.quiet.At) {
		failures = append(failures, fmt.Sprintf("%s: %s could write from %s, before the second fingerprint's reads ended at %s, so a table it changed cannot be told from the request's",
			request, before.quiet.Writer, before.quiet.At.Format(time.RFC3339Nano), after.ended.Format(time.RFC3339Nano)))
	}
	for _, table := range slices.Sorted(maps.Keys(after.tables)) {
		if before.tables[table] != after.tables[table] {
			failures = append(failures, fmt.Sprintf("%s changed table %s", request, table))
		}
	}
	return failures
}

// awaitBackgroundWrites waits for every write srv's earlier requests left to its background
// workers (backgroundWrites), under backgroundContext's bound.
func awaitBackgroundWrites(t *testing.T, srv *server) {
	t.Helper()
	ctx, cancel := backgroundContext(t)
	defer cancel()
	backgroundWrites(t, ctx, srv, nil)
}

// backgroundContext is the bound of one wait for a server's background writes and the reads after
// it: backgroundBound, or what the test binary's deadline leaves less binaryDeadlineMargin.
func backgroundContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	bound := testwait.Bounded(t, backgroundBound, binaryDeadlineMargin)
	if bound <= 0 {
		t.Fatalf("the test binary's deadline leaves no time, past binaryDeadlineMargin (%s), for a wait for the server's background writes: raise -timeout", binaryDeadlineMargin)
	}
	return context.WithTimeoutCause(context.Background(), bound, fmt.Errorf("the wait for the server's background writes and the fingerprint's reads ran past their bound of %s", bound))
}

// boundedError is err, naming the bound that ended it when ctx's bound did.
func boundedError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%w: %w", context.Cause(ctx), err)
	}
	return err
}

// backgroundWrites waits, within ctx, for every write srv's earlier requests left to the server's
// background workers, which is every writer register starts beside the requests and every one a
// request starts without waiting for it. A search of the call graph listed every goroutine, and
// every callback run on a goroutine of its own, that the server's packages can start, with each
// write it can reach; each that a test server can start is one of these, is refused
// (backgroundRefusal), or writes no row:
//
//   - srv.detached: every keyed handler, every delivery a request did not wait for (a broadcast's
//     recipients, a going run), a related-item check's pick, meaning stage and cross-project leg,
//     the term-statistics refresh a pick starts or joins, a display-tree store, a related record's
//     retry and the route roster fetch;
//   - the mention sender (srv.mentions): every send it holds, queued or in flight, each of which
//     writes its attempt's row and the comment's events when it settles, and every refresh of the
//     claims of those sends whose statement runs, which can still write a row whose send has left
//     held (mentionSenderWriting);
//   - the architecture importer (architecture.Importer.Syncing): every sync of a source, the
//     flight a caller that left first leaves running among them, which writes the source's
//     snapshot, components and facts;
//   - the GitHub App client's cached lookups (githubapp.Client.Fetching): each fetch a caller
//     started, the one a caller that left first leaves running among them, which records a rate
//     limit GitHub answers it with in github_rate_limits;
//   - the heartbeats of the server's sockets (srv.heartbeats): each resolution of a socket's
//     caller under way, which for a personal agent token stamps its last use, and which runs to its
//     end when its socket closes meanwhile (whileCallerResolves);
//   - the server's event horizon (events.Horizon.Busy): an observation in flight and the write of
//     the safe horizon it proved to event_horizon, which the last read of an event stream relay
//     generation that stopped meanwhile makes on the relay's goroutine;
//   - the document service behind srv (docs.Service.AwaitBackgroundWrites): its settlements, running or
//     armed to run before backgroundSoon from now, its compactions, its document sockets and the
//     rest of its background writes;
//   - the statements the database itself still runs for the test's database (statementsRunning):
//     a writer whose call a deadline ended (horizonPersistTimeout is one) has its statement
//     cancelled by a cancel pgx sends on a connection of its own, which can land late, or never on
//     a loaded server, and until it does the statement runs on and commits once a lock it waits on
//     is let go. Every count above falls as its writer's call returns, so this is the one branch
//     that sees such a statement. Its wait fails naming the statements its last read before the
//     bound found (awaitStatements).
//
// One writer can start another - a detached run that writes a document arms its settlement - so it
// passes over all of them again after any wait, and returns only from a pass that waited for none.
// It answers the next write the document service left armed for later. It refuses, by name, a
// server with a writer no wait can see (backgroundRefusal), at each pass. The other goroutines
// register starts write no row: the hub's listener reads, the boot mark consumes a transaction id
// and stores nothing (events.Horizon.Mark), and the test hooks' fanout answers each call before
// its request does.
func backgroundWrites(t testing.TB, ctx context.Context, srv *server, waiting func()) docs.BackgroundQuiet {
	t.Helper()
	if refusal := backgroundRefusal(srv); refusal != "" {
		t.Fatal(refusal)
	}
	if waiting != nil {
		waiting()
	}
	for {
		if refusal := backgroundRefusal(srv); refusal != "" {
			t.Fatal(refusal)
		}
		waited := false
		if detachedRunning(&srv.detached) {
			waited = true
			awaitWait(t, ctx, "the server's detached work (server.detached)", srv.detached.wait)
		}
		if mentionSenderWriting(srv.mentions) {
			waited = true
			awaitCondition(t, ctx, "the mention sender's queued and running sends and its claim refreshes", func() bool { return !mentionSenderWriting(srv.mentions) })
		}
		if srv.deps.Architecture.Syncing() {
			waited = true
			awaitCondition(t, ctx, "the architecture importer's syncs, a flight whose caller left among them (architecture.Importer.Syncing)", func() bool { return !srv.deps.Architecture.Syncing() })
		}
		if srv.deps.GitHub.Fetching() {
			waited = true
			awaitCondition(t, ctx, "the GitHub App client's cached lookups, a fetch whose caller left among them (githubapp.Client.Fetching)", func() bool { return !srv.deps.GitHub.Fetching() })
		}
		if detachedRunning(&srv.heartbeats) {
			waited = true
			awaitWait(t, ctx, "a socket heartbeat's resolution of its caller (server.heartbeats)", srv.heartbeats.wait)
		}
		if srv.deps.Store != nil {
			if horizon := srv.horizon(); horizon.Busy() {
				waited = true
				awaitCondition(t, ctx, "the event horizon's observation and its write to event_horizon", func() bool { return !horizon.Busy() })
			}
		}
		var quiet docs.BackgroundQuiet
		if srv.deps.Docs != nil {
			var err error
			if quiet, err = srv.deps.Docs.AwaitBackgroundWrites(ctx, time.Now().Add(backgroundSoon)); err != nil {
				t.Fatalf("wait for the document service's background writes: %v", boundedError(ctx, err))
			}
			waited = waited || quiet.Waited
		}
		if srv.deps.Store != nil {
			running, err := statementsRunning(ctx, srv)
			if err != nil {
				t.Fatalf("read the statements the database runs: %v", boundedError(ctx, err))
			}
			if running != "" {
				waited = true
				awaitStatements(t, ctx, srv, running)
			}
		}
		if !waited {
			return quiet
		}
	}
}

// backgroundRefusal names why no fingerprint of srv can tell the server's own writes from a
// request's, and answers "" when nothing stands in the way. Each of these writes at moments no wait
// can see, so a server running one is refused rather than waited for:
//
//   - the delivery hub, once it has run a generation (deliveryHub.ran): a view it reads again can
//     record a deploy run's warning (firstToWarn, delivery_run_warnings) at any moment while the
//     generation runs, and an insert under way when the generation stopped, which cancels its
//     context, can still commit after its worker has returned;
//   - the event stream relay, whose generation an events view starts and keeps from its first
//     subscription (a feed subscribes again before it lets the old subscription go,
//     live_events.go) to its last cancel, and which observes the event horizon on its own
//     goroutine at every event commit and on a gap's fallback timer, writing what it proved to
//     event_horizon (events.Horizon.PersistProven, writePersisted). A relay whose generation is
//     starting is refused too: its first cursor waits for a proven horizon, which writes it as
//     well. Once a generation has stopped, its context is cancelled with it, so its goroutines
//     start no observation that can persist; the one already in flight the wait sees on the
//     horizon itself (events.Horizon.Busy);
//   - an open live socket whose heartbeat writes each time it resolves its caller again
//     (heartbeatWrite), on the socket's own clock.
func backgroundRefusal(srv *server) string {
	if hub := srv.deliveryHub; hub != nil {
		hub.mu.Lock()
		ran := hub.ran
		hub.mu.Unlock()
		if ran {
			return "the server's delivery hub has run (a live socket subscribed to a delivery view): a view it reads again can record a deploy run's warning at any moment while a generation runs, and a warning insert under way when its generation stopped can commit after its worker returned, so no fingerprint of this server can tell its writes from a request's"
		}
	}
	if relay := srv.eventRelay; relay != nil {
		relay.mu.Lock()
		running := relay.generation != nil || relay.starting != nil
		relay.mu.Unlock()
		if running {
			return "the server's event stream relay runs (a live socket subscribed to an events view): it observes the event log's safe horizon on its own goroutine and writes it to event_horizon at moments no wait sees, so no fingerprint of this server can tell its writes from a request's"
		}
	}
	for _, socket := range srv.liveSockets.all() {
		if write := heartbeatWrite(srv, socket.credential); write != "" {
			return "the server holds a live socket admitted by " + write + ": its heartbeat writes it on the socket's own clock, so no fingerprint of this server can tell its writes from a request's"
		}
	}
	return ""
}

// heartbeatWrite names the credential a live socket was admitted with and what its heartbeat
// writes each time it resolves that credential again (whileCallerResolves, callerStillResolves),
// and answers "" when it writes nothing. The class search lists every write a heartbeat's
// resolution can reach: a personal agent token's last use (personalTokenActor), and a person's
// records (store.PgPeopleStore), which a browser identity's resolution of its person writes. Under
// the test server's header identity that is Record alone, an insert of the person the upgrade
// already recorded, which conflicts and changes nothing; a sign-in cookie's confirms or ends the
// person's membership (identity.CookieIdentity, identity.Membership). The shared agent token, an
// OIDC service token and a broker's token resolve without a write.
func heartbeatWrite(srv *server, credential *model.Credential) string {
	switch {
	case credential == nil:
		return ""
	case credential.TokenID != "":
		return "a personal agent token, whose resolution stamps agent_tokens.last_used_at once its last use is a minute old (personalTokenActor)"
	case credential == personCredential:
		switch srv.deps.Identity.(type) {
		case identity.HeaderIdentity, *identity.HeaderIdentity, identity.NoBrowserIdentity:
			return ""
		}
		return fmt.Sprintf("a person's sign-in under %T, whose resolution can confirm or end the person's membership (people, user_sessions)", srv.deps.Identity)
	}
	return ""
}

// statementsRunning names the statements the database runs for a session of the test's own
// database (each test has one, testpg.URL) other than the session that asks, and answers "" when
// it runs none. It reads the database's own account (pg_stat_activity), so it sees a statement
// whose client already stopped waiting for it, which no count in the server's code still holds.
func statementsRunning(ctx context.Context, srv *server) (string, error) {
	var running string
	err := srv.deps.Store.Pool.QueryRow(ctx, `
		select coalesce(string_agg(pid || ': ' || left(regexp_replace(query, '\s+', ' ', 'g'), 80), '; ' order by pid), '')
		from pg_stat_activity
		where datname = current_database() and pid <> pg_backend_pid()
			and backend_type = 'client backend' and state in ('active', 'fastpath function call')`).Scan(&running)
	return running, err
}

// awaitStatements waits, within ctx, until the database runs no statement for the test's own
// database but the asking session's (statementsRunning), reading its account every testwait.Poll,
// and fails naming running, the statements its last read before ctx ended found. The read runs on
// ctx, so the read that ctx's end cuts off answers nothing about the statements; were the wait to
// name that read's error, it would name the read, not the writer it waited for.
func awaitStatements(t testing.TB, ctx context.Context, srv *server, running string) {
	t.Helper()
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("the statements the database still runs for this test's database (%s) did not end: %v", running, context.Cause(ctx))
		case <-time.After(testwait.Poll):
		}
		now, err := statementsRunning(ctx, srv)
		switch {
		case err != nil && ctx.Err() != nil:
			t.Fatalf("the statements the database still runs for this test's database (%s) did not end: %v", running, context.Cause(ctx))
		case err != nil:
			t.Fatalf("read the statements the database runs: %v", err)
		case now == "":
			return
		}
		running = now
	}
}

// detachedRunning answers whether any of work runs now.
func detachedRunning(work *backgroundWork) bool {
	work.mu.Lock()
	defer work.mu.Unlock()
	return work.running > 0
}

// mentionSendsHeld is how many sends the mention sender holds, queued or in flight: an attempt
// leaves held only once its send has settled (mentionSender.next). A server a test builds directly
// (directServer) has no sender.
func mentionSendsHeld(sender *mentionSender) int {
	if sender == nil {
		return 0
	}
	sender.mu.Lock()
	defer sender.mu.Unlock()
	return len(sender.held)
}

// mentionSenderWriting answers whether the mention sender can still write: it holds a send, or a
// refresh of the claims of the sends it held runs its statement (mentionSender.refreshing), which
// writes without the lock after the snapshot it took, so a send it named may have left held.
func mentionSenderWriting(sender *mentionSender) bool {
	if sender == nil {
		return false
	}
	sender.mu.Lock()
	defer sender.mu.Unlock()
	return len(sender.held) > 0 || sender.refreshing > 0
}

// awaitWait runs wait, which returns once what it waits for has ended, failing the test naming
// what when ctx ends first.
func awaitWait(t testing.TB, ctx context.Context, what string, wait func()) {
	t.Helper()
	ended := make(chan struct{})
	go func() {
		wait()
		close(ended)
	}()
	select {
	case <-ended:
	case <-ctx.Done():
		t.Fatalf("%s did not end: %v", what, context.Cause(ctx))
	}
}

// awaitCondition asks done every testwait.Poll until it holds, failing the test naming what when
// ctx ends first.
func awaitCondition(t testing.TB, ctx context.Context, what string, done func() bool) {
	t.Helper()
	for !done() {
		select {
		case <-ctx.Done():
			if done() {
				return
			}
			t.Fatalf("%s did not end: %v", what, context.Cause(ctx))
		case <-time.After(testwait.Poll):
		}
	}
}
