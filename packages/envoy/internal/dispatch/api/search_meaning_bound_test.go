package api

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// endSearchStatementsAt points the server's bound on a search statement, searchStatementBound,
// at `at` for one test. beginSearchScan sets the bound from it on every search transaction, so a
// keyword or meaning statement is then ended by the server rather than answered, without a test
// waiting production's fifteen seconds out, and the 503 the search route answers names the bound
// in force.
//
// The bound and the scan lists are package variables, read by every test that runs a search, a
// related-item check or a scan, many of them parallel:
// TestMeaningScanAppliesTheServersBoundToItsTransaction,
// TestKeywordScanAppliesTheBoundAndTheConnectionCheckToItsTransaction and
// TestEverySearchStatementRunsInsideItsBoundedTransaction among them. Swapping them is safe only
// because every test that writes one - this helper's callers
// (TestSearchReportsAMeaningStatementTheServerEnded,
// TestSearchReportsAKeywordStatementTheServerEnded, TestASearchWhoseClientLeftIsNotATimeout and
// TestRelatedCheckReportsAMeaningStatementTheServerEndedAsATimeout), which write the bound, and
// TestMeaningScanRollsBackWhenASettingFails, which writes searchMeaningScan - calls no
// t.Parallel: Go runs the part of a top-level test after its t.Parallel only once every
// top-level test that does not call it has returned and put the variables back, so no reader
// sees a shortened bound or a replaced list. A writer that called t.Parallel would race every one
// of those readers. It is the same seam, under the same constraint, as the claim path's
// (issue_claim_test.go).
//
// The shortened bound alone does not end a statement. On a test database of a few rows a meaning
// statement itself executes in well under a millisecond (0.14 to 0.53 ms measured on Postgres
// 18); what takes longer is its plan, 9 to 18 ms for each custom plan the server makes. A
// backend that reuses a plan it has already made answers the statement inside the bound: 39
// runs of 40 did with plan_cache_mode = force_generic_plan, and one run of the whole package saw
// a search answered in 7 ms. So each test below also holds a table the statement reads
// (holdMeaningStatements, holdKeywordStatements): the statement waits however fast it would
// run, and the bound is what ends it. Were the bound ever to end one of the `set local`
// statements it precedes instead, beginSearchScan would fail with that same cancellation, and
// each caller below reports it the same way, so what these tests assert does not depend on which
// of the transaction's statements the bound ends.
func endSearchStatementsAt(t *testing.T, at time.Duration) {
	t.Helper()
	bound := searchStatementBound
	searchStatementBound = at
	t.Cleanup(func() { searchStatementBound = bound })
}

// holdMeaningStatements locks embeddings, the table every meaning statement reads its vectors
// from, in ACCESS EXCLUSIVE mode on a transaction of the test's own, and answers the function
// that rolls that transaction back; the test's cleanup rolls it back too, should the test end
// first. A meaning statement started while the lock is held waits for it, and Postgres checks a
// statement's statement_timeout while the statement waits for a lock as it does while the
// statement runs, so until the release such a statement can be ended by the server's bound on
// it and never answered. That is what lets these tests assert what a caller makes of a
// statement the server ended without betting that the bound is shorter than the statement
// (endSearchStatementsAt says why that bet loses).
//
// Nothing else on these tests' paths reads embeddings: search's own key and numnode reads, the
// related-item check's keyword stage and the fill's progress read (embeddingProjectKeysFilled)
// read other tables, so only the meaning statement waits. The lock is taken after the test has
// written its embeddings (processAllPending) and released before the server's cleanups, so
// neither those writes nor the server's own shutdown queue behind it.
func holdMeaningStatements(t *testing.T, database *store.Store) (release func()) {
	t.Helper()
	return holdLock(t, database, "lock table embeddings in access exclusive mode")
}

// holdKeywordStatements is holdMeaningStatements for the keyword statement: it locks messages,
// which the statement's message arm reads (searchKeywordLegs). Nothing else on a keyword search's
// path reads messages: search's own key and numnode reads read issues or nothing, and
// attachAskStates runs only once the statement has answered. The keyword statement of a set of
// kinds that leaves messages out, as a search naming only the issue's kind runs, waits for the
// lock too: the set drops the arms' rows above their union (searchKinds.exclusion), so its text
// still names messages, and Postgres locks every table a statement names before it plans or runs
// it.
func holdKeywordStatements(t *testing.T, database *store.Store) (release func()) {
	t.Helper()
	return holdLock(t, database, "lock table messages in access exclusive mode")
}

// A meaning query's transaction carries the server's own bound, read from the transaction itself
// rather than from the list that set it: a search whose caller has gone cannot be cancelled while
// the pool is full, since pgx cancels out of band and a CancelRequest needs a connection of its
// own, so the statement has to end itself. The connection check is read beside it, the other half
// of that: a backend whose client closed its connection ends its statement within a second. The
// two index settings are read with them: all four reach a query the same way, and a list that
// lost one of them fails here.
//
// The settings are local to the transaction, so the connection goes back to the pool without them.
func TestMeaningScanAppliesTheServersBoundToItsTransaction(t *testing.T) {
	t.Parallel()
	_, database, deps := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	tx, err := directServer(deps).beginMeaningScan(ctx)
	if err != nil {
		t.Fatalf("begin the meaning scan: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var timeout, check, efSearch, iterativeScan string
	if err := tx.QueryRow(ctx, `select current_setting('statement_timeout'), current_setting('client_connection_check_interval'),
		current_setting('hnsw.ef_search'), current_setting('hnsw.iterative_scan')`).Scan(&timeout, &check, &efSearch, &iterativeScan); err != nil {
		t.Fatalf("read the settings the meaning transaction applies: %v", err)
	}
	if timeout != "15s" {
		t.Fatalf("statement_timeout = %q in a meaning query's transaction, want 15s: the server's own bound is what "+
			"ends a search whose caller has gone, since pgx cannot deliver a cancellation when the pool is full", timeout)
	}
	if check != "1s" {
		t.Fatalf("client_connection_check_interval = %q in a meaning query's transaction, want 1s: the check is what "+
			"ends a statement whose client closed its connection before the bound does", check)
	}
	if efSearch != "400" || iterativeScan != "relaxed_order" {
		t.Fatalf("hnsw.ef_search = %q and hnsw.iterative_scan = %q, want 400 and relaxed_order", efSearch, iterativeScan)
	}

	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll the meaning transaction back: %v", err)
	}
	var pooled, pooledCheck string
	if err := database.Pool.QueryRow(ctx, `select current_setting('statement_timeout'),
		current_setting('client_connection_check_interval')`).Scan(&pooled, &pooledCheck); err != nil {
		t.Fatalf("read statement_timeout and client_connection_check_interval outside a meaning transaction: %v", err)
	}
	if pooled == "15s" || pooledCheck == "1s" {
		t.Fatalf("statement_timeout = %q and client_connection_check_interval = %q outside the meaning transaction, "+
			"want the server's own defaults: `set local` must not follow a pooled connection to the next request", pooled, pooledCheck)
	}
}

// The keyword statement's transaction carries the same bound and the same connection check as a
// meaning query's, read with `show` inside the transaction beginKeywordScan opens, and the
// server's own defaults outside it before and after: both are local to the transaction, so the
// connection goes back to the pool without them.
func TestKeywordScanAppliesTheBoundAndTheConnectionCheckToItsTransaction(t *testing.T) {
	t.Parallel()
	_, database, deps := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	defaultTimeout, defaultCheck := showSearchScanSettings(t, database.Pool, "before the keyword scan")
	if defaultTimeout == "15s" || defaultCheck == "1s" {
		t.Fatalf("statement_timeout = %q and client_connection_check_interval = %q on the pool before any scan, "+
			"want defaults other than the scan's, or this test reads nothing", defaultTimeout, defaultCheck)
	}

	tx, err := directServer(deps).beginKeywordScan(ctx)
	if err != nil {
		t.Fatalf("begin the keyword scan: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if timeout, check := showSearchScanSettings(t, tx, "inside the keyword scan"); timeout != "15s" || check != "1s" {
		t.Fatalf("statement_timeout = %q and client_connection_check_interval = %q in the keyword statement's "+
			"transaction, want 15s and 1s", timeout, check)
	}

	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll the keyword transaction back: %v", err)
	}
	if timeout, check := showSearchScanSettings(t, database.Pool, "after the keyword scan"); timeout != defaultTimeout || check != defaultCheck {
		t.Fatalf("statement_timeout = %q and client_connection_check_interval = %q outside the keyword transaction, "+
			"want the defaults %q and %q: `set local` must not follow a pooled connection to the next request",
			timeout, check, defaultTimeout, defaultCheck)
	}
}

// showSearchScanSettings reads statement_timeout and client_connection_check_interval with
// `show` on q, where says where for a failure.
func showSearchScanSettings(t *testing.T, q queryer, where string) (timeout, check string) {
	t.Helper()
	ctx := context.Background()
	if err := q.QueryRow(ctx, `show statement_timeout`).Scan(&timeout); err != nil {
		t.Fatalf("show statement_timeout %s: %v", where, err)
	}
	if err := q.QueryRow(ctx, `show client_connection_check_interval`).Scan(&check); err != nil {
		t.Fatalf("show client_connection_check_interval %s: %v", where, err)
	}
	return timeout, check
}

// A setting the server refuses fails the whole scan's setup, and beginMeaningScan rolls the
// transaction it opened back rather than handing the caller a half-set one or leaving the
// connection it was opened on acquired for good. The settings are one Exec now, so a refused
// entry fails the statement the sound ones were sent in, and the rollback is the only thing
// that returns the connection: the pool's count of acquired connections is what reads it.
//
// The refused entry is a bad duration for statement_timeout, a parameter Postgres itself
// defines: hnsw.ef_search is pgvector's, and a server that has not loaded the module reads it
// as a placeholder and takes any text for it.
//
// It reads the error's text too. The Go side no longer names which setting failed, since there
// is one Exec to name; Postgres's own message still does, and that is what the server logs.
func TestMeaningScanRollsBackWhenASettingFails(t *testing.T) {
	// Not parallel: it writes the package variable searchMeaningScan.
	_, database, deps := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	healthy := searchMeaningScan
	searchMeaningScan = []string{`set local statement_timeout = 'not a duration'`}
	t.Cleanup(func() { searchMeaningScan = healthy })

	before := database.Pool.Stat().AcquiredConns()
	tx, err := directServer(deps).beginMeaningScan(ctx)
	if err == nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("begin the meaning scan over a setting the server refuses = no error, want the failure reported")
	}
	if tx != nil {
		t.Fatalf("begin the meaning scan answered a transaction beside its error (%v), want none", err)
	}
	if got := database.Pool.Stat().AcquiredConns(); got != before {
		t.Fatalf("the pool holds %d acquired connections after a failed meaning scan, want %d: the transaction a "+
			"setting failed on is rolled back here, never left holding its connection", got, before)
	}
	if !strings.Contains(err.Error(), "statement_timeout") {
		t.Fatalf("a refused setting answered %v, want Postgres's own message naming statement_timeout: the Go side "+
			"no longer names which setting failed", err)
	}
}

// The search route reports a meaning statement the server ended as what it is - 503
// SEARCH_TIMEOUT, naming the bound in force - and never as an answer. runSearch keeps its
// keyword-only answer for a query whose embedding could not be had; papering a cut-off search
// over as a degraded result would serve a caller a page of keyword hits as though meaning search
// had run.
//
// It holds for a search naming no kind, which runs searchQueryMeaning, and for one naming the
// issue's kind, which runs the meaning statement of its own set of kinds (searchKinds.statements)
// in a transaction beginMeaningScan opens the same way.
//
// The bounded search runs while embeddings is held (holdMeaningStatements), so its meaning
// statement cannot answer before the shortened bound ends it. A server that held no bound on the
// statement would wait for that lock until the test binary's deadline guard fails the case by
// name.
func TestSearchReportsAMeaningStatementTheServerEnded(t *testing.T) {
	// Not parallel: it writes the package variable searchMeaningScan (endMeaningStatementsAt).
	const title, spec = "Celestial navigation device maintenance", "Routine upkeep notes."
	embedder := &fakeEmbedder{vectors: map[string][]float32{
		issueEmbeddingText(title, spec): angledVector(0.9, 2),
		"wibbleflorp":                   queryVector(),
	}}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	issue := createInteractionIssue(t, handler, "BOUND", title, spec)
	processAllPending(t, database, embedder)

	for _, search := range []struct{ name, query string }{
		{"no kind", "q=wibbleflorp"},
		{"kind issue", "q=wibbleflorp&kind=issue"},
	} {
		t.Run(search.name, func(t *testing.T) {
			// The query shares no word with the issue, so the hit below came from the meaning
			// query alone: this server answered the same search before the bound was shortened,
			// and what the 503 after it reports is the statement the server ended, not a broken
			// fixture.
			healthy := searchResponse(t, handler, search.query)
			if healthy.Degraded != "" || len(healthy.Results) != 1 || healthy.Results[0].ID != issue.Key {
				t.Fatalf("search before the bound was shortened = %+v, want the issue %s found by meaning", healthy, issue.Key)
			}

			const bound = time.Millisecond
			endSearchStatementsAt(t, bound)
			release := holdMeaningStatements(t, database)
			ended := searchBeforeTheBinaryDeadline(t, handler, aliceSearch(context.Background(), "/api/v1/search", search.query))
			release()
			requireSearchTimedOut(t, ended, bound, issue.Key)
		})
	}
}

// The search route reports a keyword statement the server ended as it reports a meaning one: 503
// SEARCH_TIMEOUT, naming the bound in force, and no result. It holds on both paths that run the
// keyword statement: the bar's keystroke request (keywords_only), and a search answered by
// keywords because no embedder is configured (degraded); and on each for a search naming no kind,
// which runs searchQuery, and for one naming the issue's kind, which runs the keyword statement of
// its own set of kinds (searchKinds.statements). Each search is first answered on this server, so
// the 503 after it is the statement the server ended and not a broken fixture.
//
// The bounded search runs while messages is held (holdKeywordStatements), so its keyword
// statement cannot answer before the shortened bound ends it. A server that held no bound on the
// statement would wait for that lock until the test binary's deadline guard fails the case by
// name.
func TestSearchReportsAKeywordStatementTheServerEnded(t *testing.T) {
	// Not parallel: it writes the package variable searchStatementBound (endSearchStatementsAt).
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "KBOUND", "Quillmotor gearbox maintenance", "Routine upkeep notes.")
	for _, path := range []struct {
		name, path, query string
		degraded          bool
	}{
		{"keywords only", "/api/v1/search/results", "q=quillmotor&keywords_only=true", false},
		{"keywords only, kind issue", "/api/v1/search/results", "q=quillmotor&keywords_only=true&kind=issue", false},
		{"degraded because no embedder is configured", "/api/v1/search", "q=quillmotor", true},
		{"degraded because no embedder is configured, kind issue", "/api/v1/search", "q=quillmotor&kind=issue", true},
	} {
		t.Run(path.name, func(t *testing.T) {
			healthy := searchResponseOn(t, handler, path.path, path.query)
			if (healthy.Degraded != "") != path.degraded || len(healthy.Results) != 1 || healthy.Results[0].ID != issue.Key {
				t.Fatalf("search before the bound was shortened = %+v, want the issue %s found by keywords, degraded %v",
					healthy, issue.Key, path.degraded)
			}

			const bound = time.Millisecond
			endSearchStatementsAt(t, bound)
			release := holdKeywordStatements(t, database)
			ended := searchBeforeTheBinaryDeadline(t, handler, aliceSearch(context.Background(), path.path, path.query))
			release()
			requireSearchTimedOut(t, ended, bound, issue.Key)
		})
	}
}

// A search whose client has left is not a timeout of the server's, whatever Postgres answered its
// statement with: the route answers it as a request its client closed, never 503
// SEARCH_TIMEOUT. Postgres ends a statement with SQLSTATE 57014 for its bound and for the
// CancelRequest a client sends as it leaves alike, so the route tells the two apart by whether
// the request's context had ended when the 57014 came back (searchFailed).
//
// A client that leaves while pgx still reads ends that read with the context's own error, which
// no reading of 57014 takes for the bound; the case that needs the context read is the 57014
// that meets a context already ended. The test stages that meeting with no race: the search runs
// while embeddings is held (holdMeaningStatements) under a shortened bound, which ends its
// meaning statement with 57014, and the tracer ends the request's context the moment that answer
// comes back (leaveAsTheStatementEnds), before the search reads it. A route that read every
// 57014 as its bound answers this search 503 SEARCH_TIMEOUT.
func TestASearchWhoseClientLeftIsNotATimeout(t *testing.T) {
	// Not parallel: it writes the package variable searchStatementBound (endSearchStatementsAt).
	const title, spec = "Celestial navigation device maintenance", "Routine upkeep notes."
	embedder := &fakeEmbedder{vectors: map[string][]float32{
		issueEmbeddingText(title, spec): angledVector(0.9, 2),
		"wibbleflorp":                   queryVector(),
	}}
	leaving := &leaveAsTheStatementEnds{}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder, tracer: leaving})
	issue := createInteractionIssue(t, handler, "LEFT", title, spec)
	processAllPending(t, database, embedder)
	healthy := searchResponse(t, handler, "q=wibbleflorp")
	if healthy.Degraded != "" || len(healthy.Results) != 1 || healthy.Results[0].ID != issue.Key {
		t.Fatalf("search before the bound was shortened = %+v, want the issue %s found by meaning", healthy, issue.Key)
	}

	endSearchStatementsAt(t, time.Millisecond)
	release := holdMeaningStatements(t, database)
	ctx, leave := context.WithCancel(context.Background())
	t.Cleanup(leave)
	answer := searchBeforeTheBinaryDeadline(t, handler,
		aliceSearch(context.WithValue(ctx, leaveKey{}, leave), "/api/v1/search", "q=wibbleflorp"))
	release()
	if !leaving.left.Load() {
		t.Fatalf("no statement of the search came back ended by the server, so its client never left: the search "+
			"answered %d %s", answer.Code, answer.Body.String())
	}
	if answer.Code == http.StatusServiceUnavailable || strings.Contains(answer.Body.String(), "SEARCH_TIMEOUT") {
		t.Fatalf("a search whose client had left when Postgres's 57014 came back answered %d %s, want no 503 "+
			"SEARCH_TIMEOUT: nobody waited on the bound", answer.Code, answer.Body.String())
	}
}

// leaveKey marks the context of the one request leaveAsTheStatementEnds may end, carrying the
// function that ends it.
type leaveKey struct{}

// leaveAsTheStatementEnds is a tracer that ends a request's context (leaveKey) the moment
// Postgres answers one of that request's statements with SQLSTATE 57014: pgx calls
// TraceQueryEnd once the answer has come back and before it hands the error to the code that
// sent the statement. left records that it did.
type leaveAsTheStatementEnds struct {
	left atomic.Bool
}

func (l *leaveAsTheStatementEnds) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (l *leaveAsTheStatementEnds) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if leave, ok := ctx.Value(leaveKey{}).(context.CancelFunc); ok && searchStatementEnded(data.Err) {
		l.left.Store(true)
		leave()
	}
}

// aliceSearch is a search request on path with query, as alice, on ctx.
func aliceSearch(ctx context.Context, path, query string) *http.Request {
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, path+"?"+query, nil)
	request.Header.Set("X-Dispatch-User", "alice")
	return request
}

// searchBeforeTheBinaryDeadline serves request and answers what the handler wrote, failing the
// case by name when the handler has not answered before the test binary's deadline
// (answeredBeforeTheBinaryDeadline).
func searchBeforeTheBinaryDeadline(t *testing.T, handler http.Handler, request *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	return answeredBeforeTheBinaryDeadline(t, "the search", func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	})
}

// requireSearchTimedOut fails the test unless answer is the refusal of a search whose statement
// the server ended at bound: 503 SEARCH_TIMEOUT, a message naming the bound in force, and no
// result naming found.
func requireSearchTimedOut(t *testing.T, answer *httptest.ResponseRecorder, bound time.Duration, found string) {
	t.Helper()
	if answer.Code != http.StatusServiceUnavailable {
		t.Fatalf("search under a statement the server ends: status=%d body=%s, want 503 SEARCH_TIMEOUT",
			answer.Code, answer.Body.String())
	}
	refusal := decodeBody[apiErrorBody](t, answer)
	if want := "ran past its " + bound.String() + " bound"; refusal.Code != "SEARCH_TIMEOUT" || !strings.Contains(refusal.Error, want) {
		t.Fatalf("search under a statement the server ends answered %s, want the code SEARCH_TIMEOUT and an error "+
			"naming the %s bound in force (%q)", answer.Body.String(), bound, want)
	}
	if strings.Contains(answer.Body.String(), found) {
		t.Fatalf("search under a statement the server ends answered results: %s", answer.Body.String())
	}
}

// The related-item check reports a meaning statement the server ended the way it already reports
// a meaning search that did not answer: the check is still made, by keywords alone, and
// says why. Before this it would have read the server's own bound as "meaning search failed",
// which is what it says when something broke.
//
// The check answers when the statement is ended rather than at the end of its meaning allowance,
// which is what tells the two apart: the statement was ended, not waited out. Here that
// allowance is an hour (server.meaningBound, with the check's own allowance raised to match),
// and the check runs while embeddings is held (holdMeaningStatements), so its meaning statement
// cannot answer before the shortened bound ends it: a check that answered at all answered
// because the server ended the statement, and one that waited the allowance out fails by name
// before the binary's deadline. The text shares one word with the issue and means the same
// thing, so the keyword stage has the item to fall back on and the meaning stage has a reason
// to be running at all.
func TestRelatedCheckReportsAMeaningStatementTheServerEndedAsATimeout(t *testing.T) {
	// Not parallel: it writes the package variable searchMeaningScan (endMeaningStatementsAt).
	const title, spec = "Nightly backups overrun their window", "The backup job runs past six in the morning."
	const text = "Nightly snapshots finish late"
	embedder := &fakeEmbedder{vectors: map[string][]float32{
		issueEmbeddingText(title, spec): angledVector(0.9, 2),
		text:                            queryVector(),
	}}
	handler, database, srv := meaningServer(t, embedder)
	createSuggestionProject(t, handler, "MBOUND")
	key := createSearchIssue(t, handler, "MBOUND", title, spec)
	processAllPending(t, database, embedder)

	// The check searched by meaning on this database, under the case's bound (meaningVerdictServer),
	// before the bound on its statement was shortened.
	healthy := meaningVerdictServer(t, srv.deps).relatedCheck(context.Background(), relatedInput{project: "MBOUND", text: text})
	if healthy.Search != "meaning_and_keywords" || healthy.SearchNote != "" {
		t.Fatalf("suggestions = %+v before the bound was shortened, want found by meaning and keywords", healthy)
	}

	const meaningBound = time.Hour
	srv.meaningBound, srv.suggestionTimeout = meaningBound, meaningBound
	endSearchStatementsAt(t, time.Millisecond)
	release := holdMeaningStatements(t, database)
	suggestions := answeredBeforeTheBinaryDeadline(t, "the check", func() *model.Suggestions {
		return srv.relatedCheck(context.Background(), relatedInput{project: "MBOUND", text: text})
	})
	release()
	if suggestions.Outcome != "found" || suggestions.Search != "keywords_only" ||
		suggestions.SearchNote != "meaning search did not answer within "+meaningBound.String() {
		t.Fatalf("suggestions = %+v, want found by keywords only, the server having ended the meaning statement", suggestions)
	}
	if !slices.Contains(relatedOwnerKeys(suggestions.Related), key) {
		t.Fatalf("related = %+v, want %s from the keyword search", suggestions.Related, key)
	}
}

// backendStatement is one statement a traced pool started and the backend process it ran on
// (conn.PgConn().PID()). storetest.StatementRecorder records the text alone, and which
// transaction a statement ran in is a fact of the backend it ran on.
type backendStatement struct {
	pid uint32
	sql string
}

// backendStatements records every statement started on the pool it traces (storetest.Traced),
// in the order they started, each with its backend.
type backendStatements struct {
	mu   sync.Mutex
	seen []backendStatement
}

func (b *backendStatements) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seen = append(b.seen, backendStatement{pid: conn.PgConn().PID(), sql: data.SQL})
	return ctx
}

func (b *backendStatements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// statements is a copy of what b has recorded so far.
func (b *backendStatements) statements() []backendStatement {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.seen)
}

// searchBoundSetting is the statement_timeout setting R1 holds the transaction of every search
// statement to: 15 s in the whole milliseconds store.StatementTimeoutSetting writes.
// searchClientConnectionCheck, the interval at which Postgres checks, while the statement runs,
// that its client is still connected, is the other setting unboundedBecause holds every search
// statement's transaction to.
var searchBoundSetting = store.StatementTimeoutSetting(15 * time.Second)

// unboundedBecause says why the statement at position at of seen did not run inside a
// transaction that, after its begin, ran a setting statement holding both searchBoundSetting and
// searchClientConnectionCheck, and is empty when it did. It reads the statements its own
// backend ran before it, newest first: a commit or a rollback met before any begin is a statement
// run outside any transaction, and a begin met before one Exec with both settings is a
// transaction that did not apply both together. A rollback to a savepoint ends no transaction.
func unboundedBecause(seen []backendStatement, at int) string {
	pid := seen[at].pid
	set := false
	for earlier := at - 1; earlier >= 0; earlier-- {
		if seen[earlier].pid != pid {
			continue
		}
		text := strings.ToLower(strings.TrimSpace(seen[earlier].sql))
		switch {
		case text == "commit" || text == "rollback" || text == "end" || text == "abort":
			return fmt.Sprintf("it ran on backend %d in no transaction: the last one there ended (%s) before it and none began after",
				pid, text)
		case text == "begin" || strings.HasPrefix(text, "begin ") || strings.HasPrefix(text, "start transaction"):
			if !set {
				return fmt.Sprintf("it ran on backend %d in a transaction that ran no statement holding both %q and %q after its begin",
					pid, searchBoundSetting, searchClientConnectionCheck)
			}
			return ""
		case !set && strings.HasPrefix(text, "set local "):
			// beginSearchScan sends all settings in one Exec, separated by "; ".
			var bound, check bool
			for setting := range strings.SplitSeq(text, "; ") {
				switch setting {
				case searchBoundSetting:
					bound = true
				case searchClientConnectionCheck:
					check = true
				}
			}
			set = bound && check
		}
	}
	return fmt.Sprintf("it ran on backend %d, which began no transaction before it", pid)
}

// refusingQueryEmbedder is fakeEmbedder with one query it answers no embedding for, as a model
// service that did not answer: a search for that text is answered by keywords alone and stamped
// degraded, on the same server whose other searches run the meaning statements.
type refusingQueryEmbedder struct {
	*fakeEmbedder
	refused string
}

func (e refusingQueryEmbedder) Embed(ctx context.Context, texts []string, inputType embed.InputType) ([][]float32, error) {
	if inputType == embed.InputQuery && slices.Contains(texts, e.refused) {
		return nil, errors.New("the model service did not answer")
	}
	return e.fakeEmbedder.Embed(ctx, texts, inputType)
}

// The names of searchStatements' four fields, as a test names a set's statement
// (searchSetStatementName).
const (
	searchKeywordField       = "keyword"
	searchMeaningEveryField  = "meaningEvery"
	searchMeaningScopedField = "meaningScoped"
	searchMeaningJoinedField = "meaningJoined"
)

// searchStatementField is one of a set's four texts with the name of the field that holds it.
type searchStatementField struct{ name, text string }

// fields lists the set's SQL texts in searchStatements' field order.
func (s *searchStatements) fields() [4]searchStatementField {
	return [...]searchStatementField{
		{searchKeywordField, s.keyword},
		{searchMeaningEveryField, s.meaningEvery},
		{searchMeaningScopedField, s.meaningScoped},
		{searchMeaningJoinedField, s.meaningJoined},
	}
}

// searchSetStatementName is how a test names one of a set's four texts: `set 5 meaningScoped`.
func searchSetStatementName(set searchKinds, field string) string {
	return fmt.Sprintf("set %d %s", set, field)
}

// searchKindsAsked is the kinds set ranks - each of searchIndexedKinds whose bit it leaves
// clear - as a request names them in its kind parameters. A request naming all five runs set 0,
// as one naming none does, and searchNoKind ranks no kind, so no request runs it
// (parseSearchKinds).
func searchKindsAsked(set searchKinds) []string {
	var asked []string
	for index, kind := range searchIndexedKinds {
		if set&(1<<index) == 0 {
			asked = append(asked, kind)
		}
	}
	return asked
}

// TestEverySearchStatementRunsInsideItsBoundedTransaction holds R1 over its class. Each text a
// search runs - the keyword statement and the three meaning statements meaningStatementFor picks
// from, in each of the 32 sets of kinds searchKinds.statements builds them for - runs inside a
// transaction that, after its begin, ran a setting statement holding both the server's 15 s bound
// and the 1 s check that the client is still connected. Which transaction a statement ran in is a
// fact of the backend it ran on, so the tracer records each statement's backend process, and each
// statement is read against what its backend ran before it (unboundedBecause).
//
// It holds that in one search of each shape a search takes: keywords only; keywords because no
// embedder answered; meaning over every project; meaning over one project once the embeddings
// project_key fill is done; meaning joined, for a filtered search; and a broadened run of a
// keyword and of a meaning search, whose second statement is held to the same as its first.
//
// And it holds that for every set of kinds. The scan tests above
// (TestKeywordScanAppliesTheBoundAndTheConnectionCheckToItsTransaction and
// TestMeaningScanAppliesTheServersBoundToItsTransaction) read the settings of the transaction
// beginKeywordScan and beginMeaningScan open, once each and not once per set: a scan takes no set
// of kinds, so its settings are the same whichever set's statement then runs in it. Which
// statement runs in it is what depends on the set, and the request's kind parameters alone choose
// that (parseSearchKinds), so the proof for each set is this test's. It names each of the
// 128 texts by its set and field (`set 5 meaningScoped`), and for every set runs a search of each
// of the four statements naming the kinds that set ranks: one kind alone, two, three, four, or
// all five, which is set 0 as naming none is. searchNoKind, which leaves every kind out, is a set
// no request names, since naming a kind takes that kind's bit back out; its four searches run
// through runSearch, the search route's own entry point, with that set in their parameters. The
// searches of a set are of one word, so one whose kinds match no row of the fixture answers an
// empty page without being broadened (searchRarestWords). The test ends by requiring that each of
// the 128 texts ran inside its bounded transaction in one of its searches.
//
// Each search is also held to the statement it names and the number of times it runs it, and to
// the answer that says which shape it was (degraded, broadened), so a fixture that stopped
// reaching a shape fails rather than passing on statements it never ran.
func TestEverySearchStatementRunsInsideItsBoundedTransaction(t *testing.T) {
	t.Parallel()
	// degradedQuery is the one query the embedder refuses. broadenedQuery holds a word no item
	// holds, so no item holds every word of it; its embedding is at cosine 0 to every stored
	// vector (defaultVector), so the meaning list answers nothing for it either, and the search
	// runs again over its rarest words.
	const degradedQuery, broadenedQuery = "lighthouse keeper", "lighthouse zzzabsent"
	embedder := refusingQueryEmbedder{
		fakeEmbedder: &fakeEmbedder{vectors: map[string][]float32{broadenedQuery: queryVector()}},
		refused:      degradedQuery,
	}
	statements := &backendStatements{}
	handler, database, deps := newTestServer(t, testServerOptions{embedder: embedder, tracer: statements})
	issue := createInteractionIssue(t, handler, "BOUND", "Lighthouse keeper roster", "The keeper logs the tide at dawn.")
	searchComment(t, handler, issue.Key, "alice", "The lighthouse keeper logs the tide at dawn.")
	processAllPending(t, database, embedder)
	RefreshEverySearchTermStats(context.Background(), database.Pool)
	recordEmbeddingProjectKeyFill(t, database, true)

	// texts names every text of every set. A text two statements shared would be named for one
	// of them alone, so the count of distinct texts is held.
	sets := len(searchStatementsByKinds)
	texts := map[string]string{}
	want := 0
	for set := range searchKinds(sets) {
		fields := set.statements().fields()
		want += len(fields)
		for _, field := range fields {
			texts[field.text] = searchSetStatementName(set, field.name)
		}
	}
	if len(texts) != want {
		t.Fatalf("the %d sets of kinds build %d distinct texts, want %d: a text two statements share is counted for one of them alone",
			sets, len(texts), want)
	}

	// over is a search the route serves, on path with query, as alice. direct is a search of
	// parameters through runSearch, the route's own entry point, which is how a set no request
	// names is run.
	over := func(path string, query url.Values) func(*testing.T) model.SearchResponse {
		return func(t *testing.T) model.SearchResponse {
			t.Helper()
			return searchResponseOn(t, handler, path, query.Encode())
		}
	}
	direct := func(parameters searchParameters) func(*testing.T) model.SearchResponse {
		return func(t *testing.T) model.SearchResponse {
			t.Helper()
			response, err := directServer(deps).runSearch(context.Background(), parameters)
			if err != nil {
				t.Fatalf("run the search %+v: %v", parameters, err)
			}
			return response
		}
	}
	type searchShape struct {
		name                string
		search              func(*testing.T) model.SearchResponse
		statement           string
		runs                int
		degraded, broadened bool
	}
	shapes := []searchShape{
		{"keyword, keywords only", over("/api/v1/search/results",
			url.Values{"q": {"lighthouse"}, "keywords_only": {"true"}}), searchQuery, 1, false, false},
		{"keyword, degraded because no embedder answered", over("/api/v1/search",
			url.Values{"q": {degradedQuery}}), searchQuery, 1, true, false},
		{"meaning, every project", over("/api/v1/search",
			url.Values{"q": {"lighthouse"}}), searchQueryMeaning, 1, false, false},
		{"meaning, one project with the fill done", over("/api/v1/search",
			url.Values{"q": {"lighthouse"}, "project": {"BOUND"}}), searchQueryMeaningScoped, 1, false, false},
		{"meaning, joined for a filtered search", over("/api/v1/search/results",
			url.Values{"q": {"lighthouse"}, "author": {"alice"}}), searchQueryMeaningJoined, 1, false, false},
		{"keyword, broadened", over("/api/v1/search/results",
			url.Values{"q": {broadenedQuery}, "keywords_only": {"true"}}), searchQuery, 2, false, true},
		{"meaning, broadened", over("/api/v1/search",
			url.Values{"q": {broadenedQuery}}), searchQueryMeaning, 2, false, true},
	}
	for set := range searchKinds(sets) {
		own := set.statements()
		if set == searchNoKind {
			every := searchParameters{text: "lighthouse", kinds: set, limit: searchDefaultLimit}
			keywords, scoped, joined := every, every, every
			keywords.keywordsOnly, scoped.project, joined.author = true, "BOUND", "alice"
			name := fmt.Sprintf("set %d, every kind left out, which no request names", set)
			shapes = append(shapes,
				searchShape{name + ", keyword, keywords only", direct(keywords), own.keyword, 1, false, false},
				searchShape{name + ", meaning, every project", direct(every), own.meaningEvery, 1, false, false},
				searchShape{name + ", meaning, one project with the fill done", direct(scoped), own.meaningScoped, 1, false, false},
				searchShape{name + ", meaning, joined for a filtered search", direct(joined), own.meaningJoined, 1, false, false},
			)
			continue
		}
		kinds := searchKindsAsked(set)
		query := func(more url.Values) url.Values {
			more["q"], more["kind"] = []string{"lighthouse"}, kinds
			return more
		}
		name := fmt.Sprintf("set %d, kind %s", set, strings.Join(kinds, " and "))
		shapes = append(shapes,
			searchShape{name + ", keyword, keywords only",
				over("/api/v1/search/results", query(url.Values{"keywords_only": {"true"}})), own.keyword, 1, false, false},
			searchShape{name + ", meaning, every project",
				over("/api/v1/search", query(url.Values{})), own.meaningEvery, 1, false, false},
			searchShape{name + ", meaning, one project with the fill done",
				over("/api/v1/search", query(url.Values{"project": {"BOUND"}})), own.meaningScoped, 1, false, false},
			searchShape{name + ", meaning, joined for a filtered search",
				over("/api/v1/search/results", query(url.Values{"author": {"alice"}})), own.meaningJoined, 1, false, false},
		)
	}

	// covered is every text that ran inside its bounded transaction in one of the searches.
	covered := map[string]bool{}
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			mark := len(statements.statements())
			response := shape.search(t)
			if (response.Degraded != "") != shape.degraded || response.Query.Broadened != shape.broadened {
				t.Fatalf("the search answered degraded=%q broadened=%v, want degraded %v and broadened %v: the fixture no longer reaches this shape",
					response.Degraded, response.Query.Broadened, shape.degraded, shape.broadened)
			}
			seen := statements.statements()
			ran := map[string]int{}
			for at := mark; at < len(seen); at++ {
				name, ok := texts[seen[at].sql]
				if !ok {
					continue
				}
				ran[name]++
				if why := unboundedBecause(seen, at); why != "" {
					t.Errorf("%s, run %d of this search: %s", name, ran[name], why)
				} else {
					covered[name] = true
				}
			}
			if want := map[string]int{texts[shape.statement]: shape.runs}; !maps.Equal(ran, want) {
				t.Fatalf("the search ran %v of search's %d statements, want %v", ran, len(texts), want)
			}
		})
	}
	for set := range searchKinds(sets) {
		for _, field := range set.statements().fields() {
			if name := searchSetStatementName(set, field.name); !covered[name] {
				t.Errorf("%s ran inside its bounded transaction in none of this test's searches, so nothing here holds it to its bound", name)
			}
		}
	}
}
