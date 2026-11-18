package api

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/pglisten"
	"github.com/sjawhar/envoy/internal/testwait"
)

// createOutcomeIssue files an issue in project as alice and returns its key.
func createOutcomeIssue(t *testing.T, handler http.Handler, project, title string) string {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": project, "title": title, "force": true,
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue %q: status=%d body=%s", title, response.Code, response.Body.String())
	}
	return decodeBody[testIssueWithSuggestions](t, response).Key
}

// postOutcomeMessage posts a message on issueKey as actor, which appends a message.created event
// on that issue.
func postOutcomeMessage(t *testing.T, handler http.Handler, issueKey, actor, body string) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/messages", map[string]any{
		"body": body,
	}, actor)
	if response.Code != http.StatusCreated {
		t.Fatalf("post a message on %s: status=%d body=%s", issueKey, response.Code, response.Body.String())
	}
}

// outcomeCursor reads the sweep's cursor, nil while no pass has set it.
func outcomeCursor(t *testing.T, database *store.Store) *int64 {
	t.Helper()
	var cursor *int64
	if err := database.Pool.QueryRow(context.Background(), `select event_id from suggestion_outcome_cursor`).Scan(&cursor); err != nil {
		t.Fatalf("read the suggestion outcome cursor: %v", err)
	}
	return cursor
}

// lastEventID is the highest event id the database holds.
func lastEventID(t *testing.T, database *store.Store) int64 {
	t.Helper()
	var id int64
	if err := database.Pool.QueryRow(context.Background(), `select coalesce(max(id), 0) from events`).Scan(&id); err != nil {
		t.Fatalf("read the last event id: %v", err)
	}
	return id
}

// sweepSuggestionOutcomes judges everything there is to judge at the server's page size, holding
// nothing back to settle, up to a safe horizon proven after the call (safeHorizon), so the pass
// reads every event committed before it: what a test that has just written what it wants judged
// needs.
func sweepSuggestionOutcomes(t *testing.T, srv *server) error {
	t.Helper()
	_, err := sweepSuggestionOutcomePages(t.Context(), srv.deps.Store.Pool, safeHorizon(t, srv), suggestionOutcomePage, 0)
	return err
}

// newestEventAt is the created_at of issueKey's newest event, a moment the database's own clock
// stamped.
func newestEventAt(t *testing.T, pool *store.Pool, issueKey string) time.Time {
	t.Helper()
	var at time.Time
	if err := pool.QueryRow(context.Background(), `select max(created_at) from events where issue_key = $1`, issueKey).Scan(&at); err != nil {
		t.Fatalf("read %s's newest event: %v", issueKey, err)
	}
	return at
}

// databaseAge is how far the database's own clock reads past at: the clock the sweep judges an
// event's or a suggestion's age by (now() in readSuggestionPage), which no clock of the test's
// moves.
func databaseAge(t *testing.T, pool *store.Pool, at time.Time) time.Duration {
	t.Helper()
	var seconds float64
	if err := pool.QueryRow(context.Background(), `select extract(epoch from clock_timestamp() - $1::timestamptz)::float8`, at).Scan(&seconds); err != nil {
		t.Fatalf("read the database's clock: %v", err)
	}
	return time.Duration(seconds * float64(time.Second))
}

// awaitDatabaseAge waits until the database's own clock reads at least age past at, the moment
// what was stamped.
func awaitDatabaseAge(t *testing.T, pool *store.Pool, what string, at time.Time, age time.Duration) {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("%s %v old by the database's clock", what, age), func() (time.Duration, bool) {
		read := databaseAge(t, pool, at)
		return read, read >= age
	})
}

// awaitDatabaseClockPastSuggestions waits until the database's own clock reads past every
// write_suggestions row's created_at, so what the test writes next is stamped after the
// suggestions: the rules judge a suggestion by the writes that postdate it on that clock.
func awaitDatabaseClockPastSuggestions(t *testing.T, pool *store.Pool) {
	t.Helper()
	var newest *time.Time
	if err := pool.QueryRow(context.Background(), `select max(created_at) from write_suggestions`).Scan(&newest); err != nil {
		t.Fatalf("read the newest suggestion's created_at: %v", err)
	}
	if newest != nil {
		awaitDatabaseAge(t, pool, "the newest suggestion", *newest, time.Microsecond)
	}
}

// idleSweep is a sweep running as the server runs it - its woken loop on a real pglisten.Hub's
// wake on EventsChannel, at the server's own gap - with its gaps and due times on clock, a fake
// the test moves, and the settle it is given, which the database's clock judges; on a counted
// pool, once its boot pass and the pass the hub's signal after its LISTEN earned have both run
// and it has gone quiet.
type idleSweep struct {
	handler        http.Handler
	server         *server
	counter        *storetest.StatementCounter
	clock          *clocktest.Fake
	target, source string
}

// idleSweepSettle is the settle of the rows that judge an event as soon as it may be: the
// database's clock passes it before the row moves the sweep's own.
const idleSweepSettle = 200 * time.Millisecond

func startIdleSweep(t *testing.T, project string, settle time.Duration) idleSweep {
	t.Helper()
	handler, database, deps := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, project)
	target := createOutcomeIssue(t, handler, project, "Target issue")
	source := createOutcomeIssue(t, handler, project, "Source issue")

	hub := &pglisten.Hub{Owner: "suggestion outcome test", Dial: database.Pool.Listener}
	wake := hub.Wake(EventsChannel)
	connected := make(chan struct{})
	hub.OnConnected(sync.OnceFunc(func() { close(connected) }))
	hubCtx, stopHub := context.WithCancel(context.Background())
	hubDone := make(chan struct{})
	go func() {
		defer close(hubDone)
		hub.Run(hubCtx)
	}()
	t.Cleanup(func() {
		stopHub()
		<-hubDone
	})

	srv := directServer(deps)
	counter := &storetest.StatementCounter{}
	counted := &store.Store{Pool: storetest.Traced(t, database.Pool, counter, nil)}
	schedule := suggestionSchedule{minGap: suggestionOutcomeSchedule.minGap, settle: settle, page: suggestionOutcomePage}
	fake := clocktest.New(time.Now())
	// The sweep reads through the counted pool and observes the horizon through the server's own
	// (store.Pool.Horizon), so the counter sees the sweep's own statements alone.
	runSweep(t, func(ctx context.Context) {
		runSuggestionOutcomeSweep(ctx, fake, counted.Pool, srv.horizon(), wake, schedule)
	})
	sweep := idleSweep{handler: handler, server: srv, counter: counter, clock: fake, target: target, source: source}
	testwait.Receive(t, "the hub's connection", connected)
	testwait.Eventually(t, "the boot pass to set the cursor", func() bool { return outcomeCursor(t, database) != nil })
	// The hub's signal after its LISTEN earns one more pass, held to the gap after the boot pass
	// began on the sweep's clock; after it the loop is idle.
	sweep.pass(t, "the pass the hub's signal after its LISTEN earned")
	storetest.AwaitQuiet(t, counter, 500*time.Millisecond, idleSweepWhy)
	return sweep
}

// pass moves the sweep's clock to the one timer its loop holds - the pass a wake earned, held to
// the gap, or a due time - and so runs the pass the loop was waiting for.
func (s idleSweep) pass(t *testing.T, what string) {
	t.Helper()
	s.clock.Set(s.clock.AwaitArmed(t, what, 1)[0])
}

// passesUntil runs the passes the loop waits for, moving the sweep's clock to each timer it holds
// in turn - the pass a wake earned, a horizon still behind the event's commit asking again a gap
// later, a due time - until holds.
func (s idleSweep) passesUntil(t *testing.T, what string, holds func() bool) {
	t.Helper()
	testwait.Eventually(t, what, func() bool {
		if holds() {
			return true
		}
		if armed := s.clock.Armed(); len(armed) > 0 {
			s.clock.Set(armed[0])
		}
		return false
	})
}

// idleSweepWhy is why a test of the idle sweep holds a span: a pass runs on the loop's own
// goroutine once its timer fires, and nothing the loop keeps says that pass has ended.
const idleSweepWhy = "the sweep's woken loop passes on its own goroutine and keeps no state that says a pass " +
	"has ended, the database is real I/O (no synctest), and no later action of the test is ordered after a " +
	"pass that does not come"

// With no event written, the sweep holds no timer, so no pass is due on any clock however far
// its own moves, and it makes no statement at all - no pass, no lease attempt - though a
// suggestion is pending.
func TestTheSuggestionSweepMakesNoStatementWithNoEvents(t *testing.T) {
	sweep := startIdleSweep(t, "OIDLE", idleSweepSettle)
	seedWriteSuggestion(t, sweep.server, "issue", sweep.source, "", "alice", "issue", sweep.target, sweep.target)

	if armed := sweep.clock.Armed(); len(armed) != 0 {
		t.Fatalf("the idle sweep holds timers for %v with no event written; want none", armed)
	}
	sweep.clock.Advance(15 * suggestionOutcomeSchedule.minGap)
	storetest.RequireNoStatements(t, sweep.counter, 3*time.Second, "the sweep with no event written", idleSweepWhy)
	if row := readSuggestionOutcome(t, sweep.server, sweep.source, sweep.target); row.Outcome != "ignored" {
		t.Fatalf("outcome = %+v with no event written; want ignored", row)
	}
}

// An event the suggestion's actor writes on the suggested issue wakes the idle sweep through its
// notification on a real pglisten.Hub, and the pass that wake earns marks the suggestion
// acted_on.
func TestAnEventByTheActorOnTheSuggestedIssueWakesTheSweepAndMarksActedOn(t *testing.T) {
	sweep := startIdleSweep(t, "OWAKE", idleSweepSettle)
	seedWriteSuggestion(t, sweep.server, "issue", sweep.source, "", "alice", "issue", sweep.target, sweep.target)

	postOutcomeMessage(t, sweep.handler, sweep.target, "alice", "Adding the new detail here instead.")
	pool := sweep.server.deps.Store.Pool
	awaitDatabaseAge(t, pool, "the message", newestEventAt(t, pool, sweep.target), idleSweepSettle)
	sweep.passesUntil(t, "the suggestion to be marked acted_on", func() bool {
		return readSuggestionOutcome(t, sweep.server, sweep.source, sweep.target).Outcome == "acted_on"
	})
	if row := readSuggestionOutcome(t, sweep.server, sweep.source, sweep.target); row.Detail != "the suggested issue was updated directly" {
		t.Fatalf("outcome = %+v; want the direct-update detail", row)
	}
}

// An agent that comments on what it filed and, moments later, updates the suggested issue acted
// on the suggestion. The comment wakes the sweep, but an event is judged by only once it is
// settle old, by which time the update is written too, so the rules see both and the acted_on
// rules run before the overridden one: the comment is not judged alone as an override. The
// pass the comment earned waits on the sweep's clock, which the row moves only once both writes
// are settle old by the database's clock, so how long the second write takes decides nothing.
func TestWritesMomentsApartAreJudgedTogetherAsActedOn(t *testing.T) {
	const settle = 1500 * time.Millisecond
	sweep := startIdleSweep(t, "OTOGETHER", settle)
	seedWriteSuggestion(t, sweep.server, "issue", sweep.source, "", "alice", "issue", sweep.target, sweep.target)
	pool := sweep.server.deps.Store.Pool

	postOutcomeMessage(t, sweep.handler, sweep.source, "alice", "Looking into this here first.")
	// Moments later by the database's clock, the one the sweep judges settle by.
	awaitDatabaseAge(t, pool, "the comment", newestEventAt(t, pool, sweep.source), 300*time.Millisecond)
	postOutcomeMessage(t, sweep.handler, sweep.target, "alice", "Adding the new detail here instead.")
	awaitDatabaseAge(t, pool, "the update", newestEventAt(t, pool, sweep.target), settle)
	sweep.passesUntil(t, "the suggestion to be judged", func() bool {
		return readSuggestionOutcome(t, sweep.server, sweep.source, sweep.target).Outcome != "ignored"
	})
	if row := readSuggestionOutcome(t, sweep.server, sweep.source, sweep.target); row.Outcome != "acted_on" {
		t.Fatalf("outcome = %+v; want acted_on, the update written moments after the comment", row)
	}
}

// A suggestion younger than settle is not judged, whatever older event a page carries: here
// another person's message on the suggested issue, written before the suggestion, is settle old at
// a pass while the suggestion is not. The agent comments on what it filed and, moments later,
// updates the suggested issue; judged at the older event's turn, the comment alone would read as an
// override. Judged once it is settle old, the rules see both writes, and it is acted_on.
//
// A pass reads every age against now(), its transaction's start. The young pass's transaction
// starts once the other person's message is settle old and before the suggestion is recorded, so
// that pass reads the message as settled and the suggestion and the comment after it as younger
// than settle, however long the writes and the safe horizon between take. It takes no transaction
// id until the pass locks the cursor, so it holds back neither those writes nor that horizon.
func TestAYoungSuggestionIsNotJudgedByAnOlderEventOfItsIssues(t *testing.T) {
	const settle = 2 * time.Second
	ctx := context.Background()
	handler, database, deps := newTestServer(t, testServerOptions{})
	srv := directServer(deps)
	createSuggestionProject(t, handler, "OYOUNG")
	target := createOutcomeIssue(t, handler, "OYOUNG", "Target issue")
	source := createOutcomeIssue(t, handler, "OYOUNG", "Source issue")
	if err := sweepSuggestionOutcomes(t, srv); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	// Filing the source recorded suggestions of its own; the test judges only the one it seeds.
	if _, err := database.Pool.Exec(ctx, `delete from write_suggestions where source_issue_key = $1`, source); err != nil {
		t.Fatalf("clear the suggestions filing recorded: %v", err)
	}

	// Every age below is read on the database's clock, the one the sweep judges settle by.
	postOutcomeMessage(t, handler, target, "bob", "A note of my own on the target.")
	older := lastEventID(t, database)
	awaitDatabaseAge(t, database.Pool, "bob's message", newestEventAt(t, database.Pool, target), settle)
	pass, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("open the young pass's transaction: %v", err)
	}
	defer func() { _ = pass.Rollback(context.Background()) }()
	seedWriteSuggestion(t, srv, "issue", source, "", "alice", "issue", target, target)
	awaitDatabaseClockPastSuggestions(t, database.Pool) // the events must postdate the suggestion row
	postOutcomeMessage(t, handler, source, "alice", "Looking into this here first.")

	safe := safeHorizon(t, srv)
	if _, _, err := judgeSuggestionPage(ctx, pass, safe, suggestionOutcomePage, settle); err != nil {
		t.Fatalf("judge a page while the suggestion is young: %v", err)
	}
	if err := pass.Commit(ctx); err != nil {
		t.Fatalf("commit the young pass: %v", err)
	}
	switch cursor := outcomeCursor(t, database); {
	case cursor == nil:
		t.Fatalf("the cursor is unset after the young pass; want it at event %d, bob's message", older)
	case *cursor != older:
		t.Fatalf("cursor = %d after the young pass; want %d: the page carries bob's message, settle old, and stops at alice's younger comment", *cursor, older)
	}
	if row := readSuggestionOutcome(t, srv, source, target); row.Outcome != "ignored" {
		t.Fatalf("outcome = %+v while the suggestion is younger than settle; want ignored until it is settle old", row)
	}

	postOutcomeMessage(t, handler, target, "alice", "Adding the new detail here instead.")
	awaitDatabaseAge(t, database.Pool, "the update, the newest write", newestEventAt(t, database.Pool, target), settle)
	if _, err := sweepSuggestionOutcomePages(ctx, database.Pool, safeHorizon(t, srv), suggestionOutcomePage, settle); err != nil {
		t.Fatalf("sweep once the suggestion is settle old: %v", err)
	}
	if row := readSuggestionOutcome(t, srv, source, target); row.Outcome != "acted_on" {
		t.Fatalf("outcome = %+v; want acted_on, the update written moments after the comment", row)
	}
}

// A suggestion is recorded in a transaction of its own, after the write that offered it commits
// (persistSuggestions), and that transaction can stay open across a later write: the row carries
// a created_at from before an event it never saw - now() is the transaction's start - and becomes
// visible only after a pass has read that event and moved its cursor past it. The next pass
// judges it against that event all the same, with no further event written, because a suggestion
// no pass has swept is judged against all its events rather than only the page's.
func TestASuggestionCommittedAfterALaterStartedEventIsStillJudged(t *testing.T) {
	ctx := context.Background()
	handler, database, deps := newTestServer(t, testServerOptions{})
	srv := directServer(deps)
	createSuggestionProject(t, handler, "ORACE")
	target := createOutcomeIssue(t, handler, "ORACE", "Target issue")
	source := createOutcomeIssue(t, handler, "ORACE", "Source issue")
	if err := sweepSuggestionOutcomes(t, srv); err != nil {
		t.Fatalf("first sweep: %v", err)
	}

	// The suggestion's transaction starts before the event, which is what makes its created_at
	// older than one, and takes no transaction id until it inserts below, so it holds back
	// neither the event's commit nor the safe horizon the pass between them reads.
	recording, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("open the suggestion's transaction: %v", err)
	}
	defer func() { _ = recording.Rollback(context.Background()) }()
	// The event must postdate the suggestion's created_at, its transaction's start.
	var recordingAt time.Time
	if err := recording.QueryRow(ctx, `select now()`).Scan(&recordingAt); err != nil {
		t.Fatalf("read the suggestion's transaction's start: %v", err)
	}
	awaitDatabaseAge(t, database.Pool, "the suggestion's transaction", recordingAt, time.Microsecond)
	postOutcomeMessage(t, handler, target, "alice", "Adding the new detail here instead.")

	if err := sweepSuggestionOutcomes(t, srv); err != nil {
		t.Fatalf("sweep before the suggestion commits: %v", err)
	}
	if cursor, last := outcomeCursor(t, database), lastEventID(t, database); cursor == nil || *cursor != last {
		t.Fatalf("cursor = %v after the pass that read the event; want the last event id %d", cursor, last)
	}
	if _, err := recording.Exec(ctx, `
		insert into write_suggestions
			(source_kind, source_issue_key, actor_kind, actor_id, role, rank, suggested_kind, suggested_id, suggested_issue_key)
		values ('issue', $1, 'user', 'alice', 'related', 0, 'issue', $2, $2)
	`, source, target); err != nil {
		t.Fatalf("record the suggestion: %v", err)
	}
	if err := recording.Commit(ctx); err != nil {
		t.Fatalf("commit the suggestion: %v", err)
	}

	if err := sweepSuggestionOutcomes(t, srv); err != nil {
		t.Fatalf("sweep after the suggestion commits: %v", err)
	}
	if row := readSuggestionOutcome(t, srv, source, target); row.Outcome != "acted_on" {
		t.Fatalf("outcome = %+v; want acted_on, from the event the earlier pass had already read", row)
	}
}

// A backlog longer than a page is read page after page within one sweep: the event that resolves
// the suggestion is on the third page of two events each, and one page alone reports itself full
// and leaves the suggestion pending.
func TestASweepReadsEveryPageOfABacklog(t *testing.T) {
	ctx := context.Background()
	handler, database, deps := newTestServer(t, testServerOptions{})
	srv := directServer(deps)
	createSuggestionProject(t, handler, "OPAGE")
	target := createOutcomeIssue(t, handler, "OPAGE", "Target issue")
	source := createOutcomeIssue(t, handler, "OPAGE", "Source issue")
	noise := createOutcomeIssue(t, handler, "OPAGE", "Unrelated issue")
	seedWriteSuggestion(t, srv, "issue", source, "", "alice", "issue", target, target)
	if err := sweepSuggestionOutcomes(t, srv); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	for range 4 {
		postOutcomeMessage(t, handler, noise, "bob", "Unrelated progress.")
	}
	postOutcomeMessage(t, handler, target, "alice", "Adding the new detail here instead.")

	const page = 2
	full, _, err := judgeSuggestionPageInTransaction(ctx, database.Pool, safeHorizon(t, srv), page, 0)
	if err != nil {
		t.Fatalf("one page: %v", err)
	}
	if !full {
		t.Fatal("a page of two out of five new events reported itself short")
	}
	if row := readSuggestionOutcome(t, srv, source, target); row.Outcome != "ignored" {
		t.Fatalf("outcome = %+v after the first page; want ignored, its event two pages on", row)
	}
	if _, err := sweepSuggestionOutcomePages(ctx, database.Pool, safeHorizon(t, srv), page, 0); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if row := readSuggestionOutcome(t, srv, source, target); row.Outcome != "acted_on" {
		t.Fatalf("outcome = %+v after the sweep; want acted_on", row)
	}
	if cursor, last := outcomeCursor(t, database), lastEventID(t, database); cursor == nil || *cursor != last {
		t.Fatalf("cursor = %v after the sweep; want the last event id %d", cursor, last)
	}
}

// touchedPassCost is what one page of a later pass cost: its statements, and the rows it read
// from each table it reads (sequential reads plus live rows fetched through an index).
type touchedPassCost struct {
	statements       int64
	suggestionRows   int64
	eventRows        int64
	sequentialReads  int64
	resolvedAsWanted bool
}

// oldSuggestions is how many old suggestions measureTouchedPass seeds: half on an issue nothing
// writes to, half on an ask on a project document nothing writes to.
const oldSuggestions = 1000

// measureTouchedPass seeds oldSuggestions suggestions on an issue and a project document nothing
// writes to, pending when pending is set and already resolved when not, lets a sweep pass over
// them, then records one new suggestion, the event that resolves it and an event on another
// project document, and measures the one page that judges them. The page runs in a transaction of
// the test's own, so pg_stat_xact_user_tables reads exactly that page's row counts before it
// commits. The document event is there so the page looks pending suggestions up by document too,
// the lookup migration 0155 indexed.
//
// Both measurements seed the same rows, so the table, its indexes and its statistics are the same
// size and Postgres plans each read the same way; the only difference is whether the old rows are
// pending. The page's connection also runs with enable_seqscan off: a test's table holds too few
// rows for Postgres to plan a read as it would at production's size (storetest.Planned says the
// same), and with it off a read that has an index reads only the rows it matches, while a read
// with none is still a sequential scan of every row. The table is vacuumed before the page, as
// autovacuum leaves it, so the dead index entries of rows the earlier pass marked swept are not
// part of the count.
func measureTouchedPass(t *testing.T, project string, pending bool) touchedPassCost {
	t.Helper()
	ctx := context.Background()
	handler, database, deps := newTestServer(t, testServerOptions{})
	srv := directServer(deps)
	createSuggestionProject(t, handler, project)
	target := createOutcomeIssue(t, handler, project, "Target issue")
	source := createOutcomeIssue(t, handler, project, "Source issue")
	quietIssue := createOutcomeIssue(t, handler, project, "Quiet issue")
	quietDocument := createProjectDocument(t, handler, project, "Quiet notes", "# Quiet notes\n")
	activeDocument := createProjectDocument(t, handler, project, "Active notes", "# Active notes\n")
	asked := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+quietDocument.ID+"/asks", map[string]any{
		"question": "Which quiet option do we keep?",
	}, "alice")
	if asked.Code != http.StatusCreated {
		t.Fatalf("open an ask on the quiet document: status=%d body=%s", asked.Code, asked.Body.String())
	}
	quietAsk := decodeBody[testAskWithSuggestions](t, asked).ID
	outcome := "overridden"
	if pending {
		outcome = "ignored"
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into write_suggestions
			(source_kind, source_issue_key, source_artifact_id, source_ask_id, actor_kind, actor_id, role, rank,
			 suggested_kind, suggested_id, suggested_issue_key, outcome)
		select 'issue', $1::text, null, null, 'user', 'alice', 'related', 0, 'issue', $1::text || '-' || n, $1::text, $5
		  from generate_series(1, $4::int / 2) n
		union all
		select 'ask', null, $2::uuid, $3::uuid, 'user', 'alice', 'related', 0, 'issue', $1::text || '-' || n, $1::text, $5
		  from generate_series(1, $4::int / 2) n
	`, quietIssue, quietDocument.ID, quietAsk, oldSuggestions, outcome); err != nil {
		t.Fatalf("seed old suggestions: %v", err)
	}
	if err := sweepSuggestionOutcomes(t, srv); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	var stillPending int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from write_suggestions
		 where (source_issue_key = $1 or source_artifact_id = $2) and suggested_id like $1 || '-%' and outcome = 'ignored'
	`, quietIssue, quietDocument.ID).Scan(&stillPending); err != nil {
		t.Fatalf("count old pending suggestions: %v", err)
	}
	if want := map[bool]int{true: oldSuggestions, false: 0}[pending]; stillPending != want {
		t.Fatalf("%d of the old suggestions are pending after the first sweep; want %d", stillPending, want)
	}
	seedWriteSuggestion(t, srv, "issue", source, "", "alice", "issue", target, target)
	postOutcomeMessage(t, handler, target, "alice", "Adding the new detail here instead.")
	commented := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+activeDocument.ID+"/comments", map[string]any{
		"body": "Progress on the active notes.",
	}, "bob")
	if commented.Code != http.StatusCreated {
		t.Fatalf("comment on the active document: status=%d body=%s", commented.Code, commented.Body.String())
	}
	if _, err := database.Pool.Exec(ctx, `vacuum analyze write_suggestions`); err != nil {
		t.Fatalf("vacuum write_suggestions: %v", err)
	}

	// Read outside the page's transaction: an observation of the horizon waits on every
	// transaction id open on the instance, this test's own included (events.Horizon.Await).
	safe := safeHorizon(t, srv)
	counter := &storetest.StatementCounter{}
	counted := storetest.Traced(t, database.Pool, counter, map[string]string{"enable_seqscan": "off"})
	tx, err := counted.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the page's transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	before := counter.Count()
	if _, _, err := judgeSuggestionPage(ctx, tx, safe, suggestionOutcomePage, 0); err != nil {
		t.Fatalf("judge the page: %v", err)
	}
	cost := touchedPassCost{statements: counter.Count() - before}
	rows, err := tx.Query(ctx, `
		select relname, seq_tup_read, coalesce(idx_tup_fetch, 0)
		  from pg_stat_xact_user_tables
		 where relname in ('write_suggestions', 'events')`)
	if err != nil {
		t.Fatalf("read the page's table statistics: %v", err)
	}
	for rows.Next() {
		var table string
		var sequential, fetched int64
		if err := rows.Scan(&table, &sequential, &fetched); err != nil {
			t.Fatalf("scan the page's table statistics: %v", err)
		}
		cost.sequentialReads += sequential
		switch table {
		case "write_suggestions":
			cost.suggestionRows = sequential + fetched
		case "events":
			cost.eventRows = sequential + fetched
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the page's table statistics: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the page: %v", err)
	}
	cost.resolvedAsWanted = readSuggestionOutcome(t, srv, source, target).Outcome == "acted_on"
	return cost
}

// A thousand suggestions still pending on an issue nothing writes to cost a later pass nothing: the
// page that judges a new suggestion and the event that resolves it makes as many statements, and
// reads as many rows of write_suggestions and of events, with them as with none pending (the same
// thousand rows, resolved), and reads no table sequentially. The pass of the previous release read
// every pending row on every pass.
func TestOldPendingSuggestionsOnOtherIssuesCostAPassNothing(t *testing.T) {
	without := measureTouchedPass(t, "OCOST", false)
	with := measureTouchedPass(t, "OCOST", true)
	t.Logf("none pending: %+v; 1,000 pending: %+v", without, with)
	if !without.resolvedAsWanted || !with.resolvedAsWanted {
		t.Fatalf("the page did not mark the new suggestion acted_on (without old pending: %v, with: %v)", without.resolvedAsWanted, with.resolvedAsWanted)
	}
	if with.statements != without.statements {
		t.Errorf("the page made %d statements with 1,000 old pending suggestions and %d without", with.statements, without.statements)
	}
	if with.suggestionRows != without.suggestionRows {
		t.Errorf("the page read %d write_suggestions rows with 1,000 old pending suggestions and %d without", with.suggestionRows, without.suggestionRows)
	}
	if with.eventRows != without.eventRows {
		t.Errorf("the page read %d events rows with 1,000 old pending suggestions and %d without", with.eventRows, without.eventRows)
	}
	if with.sequentialReads != 0 || without.sequentialReads != 0 {
		t.Errorf("the page read %d rows sequentially with 1,000 old pending suggestions and %d without; want none", with.sequentialReads, without.sequentialReads)
	}
}

// safeHorizon is the safe horizon of srv's one events.Horizon once an observation taken during
// the call is proven safe (events.Horizon.Await), so it is at or above every event committed
// before the call: what a test that has just written what it wants read needs.
func safeHorizon(t *testing.T, srv *server) int64 {
	t.Helper()
	safe, err := srv.horizon().Await(t.Context())
	if err != nil {
		t.Fatalf("await the safe horizon: %v", err)
	}
	return safe
}

// suggestionOutcomeAt is when a suggestion was judged, nil while it is still pending.
func suggestionOutcomeAt(t *testing.T, srv *server, sourceIssueKey, suggestedID string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := srv.deps.Store.Pool.QueryRow(context.Background(), `
		select outcome_at from write_suggestions where source_issue_key = $1 and suggested_id = $2
	`, sourceIssueKey, suggestedID).Scan(&at); err != nil {
		t.Fatalf("read when the suggestion was judged: %v", err)
	}
	return at
}

// An event that can still commit is never passed over. Transaction A appends an event on the
// suggested issue, the one that resolves the suggestion, and stays open while transaction B
// appends a later id and commits, so the highest committed id stands above an id that can still
// commit. A pass run then reads only up to the safe horizon (events.Horizon), which stays below
// A's id while A is open: it judges nothing, leaves the cursor below that id, and asks for a pass
// of its own no sooner than the loop's gap rather than spinning on the gap above the horizon.
// Once A commits, the next pass reads its event and judges the suggestion by it, once. A pass
// reading `id > cursor` uncapped moves the cursor past A's event on the first pass, and the
// suggestion is then never judged.
func TestTheSuggestionSweepTrailsTheSafeHorizon(t *testing.T) {
	ctx := t.Context()
	handler, database, deps := newTestServer(t, testServerOptions{})
	srv := directServer(deps)
	createSuggestionProject(t, handler, "OHORIZON")
	target := createOutcomeIssue(t, handler, "OHORIZON", "Target issue")
	source := createOutcomeIssue(t, handler, "OHORIZON", "Source issue")
	other := createOutcomeIssue(t, handler, "OHORIZON", "Other issue")
	if err := sweepSuggestionOutcomes(t, srv); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	// Filing the source recorded suggestions of its own; the test judges only the one it seeds,
	// whose actor is the one appendOutOfOrder writes its events as.
	if _, err := database.Pool.Exec(ctx, `delete from write_suggestions where source_issue_key = $1`, source); err != nil {
		t.Fatalf("clear the suggestions filing recorded: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into write_suggestions
			(source_kind, source_issue_key, actor_kind, actor_id, role, rank, suggested_kind, suggested_id, suggested_issue_key)
		values ('issue', $1, 'session', 'seed', 'related', 0, 'issue', $2, $2)
	`, source, target); err != nil {
		t.Fatalf("record the suggestion: %v", err)
	}
	awaitDatabaseClockPastSuggestions(t, database.Pool) // the events must postdate the suggestion row

	schedule := suggestionSchedule{minGap: time.Second, page: suggestionOutcomePage}
	low, _, commitLow := appendOutOfOrder(t, database, deps.Events, target, other)
	started := time.Now()
	next, err := suggestionOutcomePass(ctx, clock.Real(), database.Pool, srv.horizon(), schedule)
	if err != nil {
		t.Fatalf("the pass run while the lower id is still open: %v", err)
	}
	switch cursor := outcomeCursor(t, database); {
	case cursor == nil:
		t.Fatalf("the cursor is unset after a pass run while event %d could still commit", low)
	case *cursor >= low:
		t.Fatalf("cursor = %d after a pass run while event %d could still commit; want it below that id", *cursor, low)
	}
	if row := readSuggestionOutcome(t, srv, source, target); row.Outcome != "ignored" {
		t.Fatalf("outcome = %+v while the event that resolves the suggestion could still commit; want ignored", row)
	}
	if next.IsZero() || next.Before(started.Add(schedule.minGap)) {
		t.Fatalf("the pass held above the horizon asked to run again at %v, %v after it started; want a due time no sooner than its %v gap",
			next, next.Sub(started), schedule.minGap)
	}

	commitLow()
	if safe := safeHorizon(t, srv); safe < low {
		t.Fatalf("the safe horizon is %d once event %d has committed; want it at or above that id", safe, low)
	}
	if _, err := suggestionOutcomePass(ctx, clock.Real(), database.Pool, srv.horizon(), schedule); err != nil {
		t.Fatalf("the pass run once the lower id has committed: %v", err)
	}
	if row := readSuggestionOutcome(t, srv, source, target); row.Outcome != "acted_on" || row.Detail != "the suggested issue was updated directly" {
		t.Fatalf("outcome = %+v once the event that resolves the suggestion has committed; want acted_on by the direct update", row)
	}
	switch cursor := outcomeCursor(t, database); {
	case cursor == nil:
		t.Fatalf("the cursor is unset after the pass that read event %d", low)
	case *cursor < low:
		t.Fatalf("cursor = %d after the pass that read event %d; want it at or above that id", *cursor, low)
	}
	judged := suggestionOutcomeAt(t, srv, source, target)
	if _, err := suggestionOutcomePass(ctx, clock.Real(), database.Pool, srv.horizon(), schedule); err != nil {
		t.Fatalf("the pass after the suggestion was judged: %v", err)
	}
	if again := suggestionOutcomeAt(t, srv, source, target); judged == nil || again == nil || !again.Equal(*judged) {
		t.Fatalf("the suggestion was judged at %v and again at %v; want one judgement", judged, again)
	}
}

// The first pass of a just-started process starts at the newest event, not at 0. A fresh
// events.Horizon has proven nothing, and one snapshot never proves its own A (events.Horizon), so
// the one observation the first pass takes leaves H at 0. With the cursor still null, a pass that
// read that 0 as its safe horizon would move the cursor to the highest event id at or below it
// (startSuggestionCursor), which is 0, and every later pass would then page through the whole
// event log before reaching anything it has to judge.
func TestTheFirstPassOfAJustStartedProcessStartsAtTheNewestEvent(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	// srv's is the one events.Horizon of a process that has just started: on the store's horizon
	// pool, with nothing proven and no observation taken, as the server's is on its boot pass.
	srv := directServer(deps)
	createSuggestionProject(t, handler, "OSTART")
	target := createOutcomeIssue(t, handler, "OSTART", "Target issue")
	postOutcomeMessage(t, handler, target, "alice", "A message on the target issue")
	if cursor := outcomeCursor(t, database); cursor != nil {
		t.Fatalf("cursor = %d before any pass has run; want it null", *cursor)
	}

	newest := lastEventID(t, database)
	schedule := suggestionSchedule{minGap: time.Second, settle: time.Minute, page: suggestionOutcomePage}
	if _, err := suggestionOutcomePass(t.Context(), clock.Real(), database.Pool, srv.horizon(), schedule); err != nil {
		t.Fatalf("the first pass of a just-started process: %v", err)
	}
	switch cursor := outcomeCursor(t, database); {
	case cursor == nil:
		t.Fatalf("the cursor is unset after the first pass; want it at event %d, the newest there is", newest)
	case *cursor != newest:
		t.Fatalf("cursor = %d after the first pass of a just-started process; want %d, the newest event id, so the passes after it read the log from there rather than from the start",
			*cursor, newest)
	}
}
