package main

//ci:reads packages/envoy/internal/dispatch/embedqueue/testdata/main_coverage_query.sql readCoverageOracles reads it (os.ReadFile through ../../internal/dispatch/embedqueue/testdata) for requireCoverageIsMains, which TestTheServerFillsTheCoverageCountsAfterItListens, TestAFillCutOffByAStopFillsAgainAtTheNextStart, TestAFilledStartListensBeforeItsRecountEnds, TestAFailedFillKeepsServingAndRefusesUntilAFillEnds and TestAFillPastItsTimeoutIsAFailedFill call
//ci:reads packages/envoy/internal/dispatch/embedqueue/testdata/main_route_coverage_statement.sql readCoverageOracles reads it (os.ReadFile through ../../internal/dispatch/embedqueue/testdata) for requireCoverageIsMains, which TestTheServerFillsTheCoverageCountsAfterItListens, TestAFillCutOffByAStopFillsAgainAtTheNextStart, TestAFilledStartListensBeforeItsRecountEnds, TestAFailedFillKeepsServingAndRefusesUntilAFillEnds and TestAFillPastItsTimeoutIsAFailedFill call
//ci:reads packages/envoy/internal/dispatch/embedqueue/testdata/main_route_coverage_settings.sql readCoverageOracles reads it (os.ReadFile through ../../internal/dispatch/embedqueue/testdata) for requireCoverageIsMains, which TestTheServerFillsTheCoverageCountsAfterItListens, TestAFillCutOffByAStopFillsAgainAtTheNextStart, TestAFilledStartListensBeforeItsRecountEnds, TestAFailedFillKeepsServingAndRefusesUntilAFillEnds and TestAFillPastItsTimeoutIsAFailedFill call

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/embedqueue"
	"github.com/sjawhar/envoy/internal/dispatch/horizontest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The lines the coverage counts loop logs (embedqueue/counts.go), as a server's text log writes
// them.
const (
	coverageFilledLine     = `msg="dispatch embedqueue: coverage counts filled"`
	coverageRecountedLine  = `msg="dispatch embedqueue: coverage counts recounted"`
	coverageFillFailedLine = `level=ERROR msg="dispatch embedqueue: coverage counts fill failed; it runs again later"`
)

// embedqueueTestdata is the directory of embedqueue's tests that holds its frozen coverage texts,
// each a file those tests embed: the statement before the kept counts (main_coverage_query.sql,
// mainCoverageQuery there), main's route statement (main_route_coverage_statement.sql,
// mainRouteCoverageStatement) and the settings main ran it under
// (main_route_coverage_settings.sql, mainRouteCoverageSettings).
const embedqueueTestdata = "../../internal/dispatch/embedqueue/testdata"

// coverageOracles are embedqueue's two frozen coverage statements, which every answer of the
// route is compared with: before, the statement before the kept counts (mainCoverageQuery); route,
// main's route statement (mainRouteCoverageStatement); and routeSettings, the settings main ran it
// under (mainRouteCoverageSettings).
type coverageOracles struct {
	before, route, routeSettings string
}

// readCoverageOracles reads the two frozen statements and the route's settings from the files
// embedqueue's tests embed (embedqueueTestdata), so the server's answers are held to the one
// frozen text the package's own tests hold the report to, and not to a second copy.
func readCoverageOracles(t *testing.T) coverageOracles {
	t.Helper()
	read := func(name string) string {
		text, err := os.ReadFile(filepath.Join(embedqueueTestdata, name))
		if err != nil {
			t.Fatalf("read embedqueue's frozen coverage text: %v", err)
		}
		return string(text)
	}
	return coverageOracles{
		before:        read("main_coverage_query.sql"),
		route:         read("main_route_coverage_statement.sql"),
		routeSettings: read("main_route_coverage_settings.sql"),
	}
}

// coverageDatabase is a database of the test's own holding an item of the issue and window kinds
// and a component, written with their triggers, and no fill of the coverage counts: done_at unset.
func coverageDatabase(t *testing.T) *store.Store {
	t.Helper()
	database := storetest.Open(t)
	for _, statement := range []string{
		`insert into projects (key, name, default_assignee) values ('COV', 'Coverage', 'alice')`,
		`insert into issues (key, project_key, number, title, created_by, rank)
		 select 'COV-' || g, 'COV', g, 'An issue the coverage counts count ' || g, '{"kind":"user","id":"alice"}', 'A'
		 from generate_series(1, 3) g`,
		`select embeddings_enqueue_window('document_block', 'document_block/cov#b' || g, 'A window the coverage counts count ' || g, array['b1'])
		 from generate_series(1, 2) g`,
		`insert into architecture_sources (project_key, id, repo, branch, installation_id, created_by)
		 values ('COV', 1, 'cov/arch', 'main', 1, '{"kind":"system","id":"test"}')`,
		`insert into architecture_snapshots (id, project_key, commit, files) values (910, 'COV', 'c1', '{}')`,
		`insert into components (project_key, id, title, prose, snapshot_id, source_id) values ('COV', 'counted', 'Counted', '', 910, 1)`,
	} {
		if _, err := database.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("seed the coverage corpus: %v\n%s", err, statement)
		}
	}
	return database
}

// coverageHold is a lock the test takes on its own connection: table in access exclusive mode.
// components, which the not-indexed group's statement reads, is one a fill or a recount waits on
// where it runs and the route's read does not take; search_coverage_counts, which the route's read
// sums, is one that read waits on. It answers the holding transaction; committing it ends the
// hold.
func coverageHold(t *testing.T, database *store.Store, table string) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	hold, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the hold: %v", err)
	}
	t.Cleanup(func() { _ = hold.Rollback(context.Background()) })
	if _, err := hold.Exec(ctx, "lock table "+table+" in access exclusive mode"); err != nil {
		t.Fatalf("hold %s: %v", table, err)
	}
	return hold
}

// endHold commits hold.
func endHold(t *testing.T, hold pgx.Tx) {
	t.Helper()
	if err := hold.Commit(context.Background()); err != nil {
		t.Fatalf("end the hold: %v", err)
	}
}

// requireHeld waits until a session of the server waits on hold: the fill or the recount it holds
// (what names it). Its one bound is the test binary's deadline (storetest.WaitForLockWaiters).
func requireHeld(t *testing.T, hold pgx.Tx, what string) {
	t.Helper()
	t.Logf("waiting for the server's %s to wait on the hold", what)
	storetest.WaitForLockWaiters(t, hold, 1)
}

// coverageServer is a process a coverage test started, with the Host its requests name: empty for
// the address they reach, and the dashboard origin's host for a dev sign-in server, which answers a
// request naming any other 421 HOST_MISMATCH.
type coverageServer struct {
	*dispatchProcess
	host string
}

// coverage is what GET /api/v1/search/coverage answers now, asked with the agent token, which the
// route admits, failing the test when no answer comes within 30 s, three of the route's bounds.
func (s coverageServer) coverage(t *testing.T) httpAnswer {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url("/api/v1/search/coverage"), nil)
	if err != nil {
		t.Fatalf("build the coverage request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+dispatchTestAgentToken)
	if s.host != "" {
		request.Host = s.host
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET /api/v1/search/coverage: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the coverage answer: %v", err)
	}
	return httpAnswer{status: response.StatusCode, header: response.Header, body: body}
}

// requireCoverageRefused fails the test unless the route answers 503 with code and an error that
// is not empty and contains mention, the value the spec names in it ("" when it names none beyond
// the code), as api's requireSearchTimedOut holds SEARCH_TIMEOUT: the code and that value are
// what a client reads, and the sentence around them may be reworded.
func requireCoverageRefused(t *testing.T, server coverageServer, code, mention string) {
	t.Helper()
	answer := server.coverage(t)
	var refusal struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(answer.body, &refusal); err != nil || answer.status != http.StatusServiceUnavailable ||
		refusal.Code != code || refusal.Error == "" || !strings.Contains(refusal.Error, mention) {
		want := "a non-empty error"
		if mention != "" {
			want = fmt.Sprintf("an error naming %q", mention)
		}
		t.Fatalf("the coverage route answers %s, want 503 %s with %s", answer, code, want)
	}
}

// requireHealthy fails the test unless /healthz answers 200 at once: the server serves.
func requireHealthy(t *testing.T, p *dispatchProcess) {
	t.Helper()
	if answer := p.send(t, http.MethodGet, "/healthz", "", nil); answer.status != http.StatusOK {
		t.Fatalf("/healthz answers %s, want 200: the server keeps serving", answer)
	}
}

// coverageDifference compares report, the route's answer, with embedqueue's two frozen statements
// (coverageOracles) read now for the report's identity, both at one snapshot: a REPEATABLE READ
// transaction reads the statement before the kept counts, then sets main's route settings and
// reads main's route statement. It answers how report differs from each, one line per kind, ""
// when it differs from neither (statementDifference).
func coverageDifference(t *testing.T, database *store.Store, oracles coverageOracles, report model.SearchCoverage) string {
	t.Helper()
	identity := ""
	if report.Identity != nil {
		identity = *report.Identity
	}
	ctx := context.Background()
	tx, err := database.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("begin the snapshot main's statements read: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	differences := statementDifference(t, tx, "mainCoverageQuery", oracles.before, identity, report)
	if _, err := tx.Exec(ctx, oracles.routeSettings); err != nil {
		t.Fatalf("set the settings main ran its route statement under: %v", err)
	}
	differences = append(differences, statementDifference(t, tx, "mainRouteCoverageStatement", oracles.route, identity, report)...)
	return strings.Join(differences, "\n")
}

// statementDifference compares report with what statement, the frozen statement named side,
// answers for identity in tx, and answers one line for each kind they differ on: none when every
// covered kind's items, current, failed and lacking and every not-indexed count are the same, each
// oldest lacking age is within 2 s of the statement's (the route read a moment before), and every
// covered kind the statement answers no row for, which holds no item, the route answers all zeros.
func statementDifference(t *testing.T, tx pgx.Tx, side, statement, identity string, report model.SearchCoverage) []string {
	t.Helper()
	rows, err := tx.Query(context.Background(), statement, identity)
	if err != nil {
		t.Fatalf("run %s: %v", side, err)
	}
	defer rows.Close()
	kinds := map[string]model.SearchCoverageCounts{
		"issue": report.Kinds.Issue, "document_block": report.Kinds.DocumentBlock, "comment_block": report.Kinds.CommentBlock,
		"message_block": report.Kinds.MessageBlock, "ask_part": report.Kinds.AskPart,
	}
	notIndexed := map[string]int64{}
	for _, kind := range report.NotIndexed {
		notIndexed[kind.Kind] = kind.Items
	}
	var differences []string
	for rows.Next() {
		var kind string
		var indexed bool
		var items, current, failed int64
		var oldest *int64
		if err := rows.Scan(&kind, &indexed, &items, &current, &failed, &oldest); err != nil {
			t.Fatalf("read %s: %v", side, err)
		}
		if !indexed {
			if counted, ok := notIndexed[kind]; !ok || counted != items {
				differences = append(differences, fmt.Sprintf("%s: the route counts %d, %s %d", kind, counted, side, items))
			}
			delete(notIndexed, kind)
			continue
		}
		got, ok := kinds[kind]
		ages := got.OldestLackingSeconds == nil && oldest == nil ||
			got.OldestLackingSeconds != nil && oldest != nil && max(*got.OldestLackingSeconds-max(*oldest, 0), max(*oldest, 0)-*got.OldestLackingSeconds) <= 2
		if !ok || got.Items != items || got.Current != current || got.Failed != failed || got.Lacking != items-current-failed || !ages {
			differences = append(differences, fmt.Sprintf("%s: the route answers %+v, %s items %d, current %d, failed %d, oldest %v",
				kind, got, side, items, current, failed, oldest))
		}
		delete(kinds, kind)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %s: %v", side, err)
	}
	for kind, got := range kinds {
		if got != (model.SearchCoverageCounts{}) {
			differences = append(differences, fmt.Sprintf("%s: the route answers %+v, %s no row: no item", kind, got, side))
		}
	}
	for kind, counted := range notIndexed {
		differences = append(differences, fmt.Sprintf("%s: the route counts %d, %s no row", kind, counted, side))
	}
	return differences
}

// requireCoverageIsMains requires a 200 response with the kept counts and waits for them to
// match both of main's statements (coverageDifference). The dev embedder can write between the
// route's read and the statements' snapshot while its queue drains.
func requireCoverageIsMains(t *testing.T, server coverageServer, database *store.Store) {
	t.Helper()
	oracles := readCoverageOracles(t)
	testwait.Until(t, "the route's kept counts to match both of main's statements", func() (string, bool) {
		answer := server.coverage(t)
		if answer.status != http.StatusOK {
			t.Fatalf("the coverage route answers %s, want 200 with the kept counts", answer)
		}
		var report model.SearchCoverage
		if err := json.Unmarshal(answer.body, &report); err != nil {
			t.Fatalf("read the coverage route's answer: %v\n%s", err, answer.body)
		}
		difference := coverageDifference(t, database, oracles, report)
		return difference, difference == ""
	})
}

// filled reports whether search_coverage_filled holds its row: a fill has ended.
func filled(t *testing.T, database *store.Store) bool {
	t.Helper()
	var done bool
	if err := database.Pool.QueryRow(context.Background(), `select exists (select 1 from search_coverage_filled)`).Scan(&done); err != nil {
		t.Fatalf("read search_coverage_filled: %v", err)
	}
	return done
}

// devEmbedderOptions is a process that embeds meaning search's text with the dev embedder
// (DISPATCH_DEV_EMBEDDER=1), which only a dev sign-in server, signing its own cookies, may run.
var devEmbedderOptions = dispatchOptions{Env: map[string]string{
	"DISPATCH_DEV_SIGNIN":              "1",
	"DISPATCH_DEV_EMBEDDER":            "1",
	"DISPATCH_IDENTITY":                "cookie",
	"DISPATCH_IDENTITY_HEADER_TRUSTED": "",
	"DISPATCH_INSECURE_COOKIE":         "1",
	"DISPATCH_SERVER_URL":              "http://127.0.0.1",
}}

// TestTheServerFillsTheCoverageCountsAfterItListens holds R7k: a server started on counts no
// fill has filled (done_at unset), its fill held where it runs, listens - its listening line
// names since_start_ms - and serves: /healthz answers, and the coverage route answers 503
// SEARCH_COVERAGE_NOT_FILLED with an error that is not empty. Once the hold ends the log holds
// `coverage counts filled` and the route answers the kept counts, equal to both of main's
// statements' (requireCoverageIsMains). With no embedder and with the dev embedder.
func TestTheServerFillsTheCoverageCountsAfterItListens(t *testing.T) {
	t.Parallel()
	for _, run := range []struct {
		name, host string
		options    dispatchOptions
	}{{"no embedder", "", dispatchOptions{}}, {"the dev embedder", "127.0.0.1", devEmbedderOptions}} {
		t.Run(run.name, func(t *testing.T) {
			t.Parallel()
			database := coverageDatabase(t)
			hold := coverageHold(t, database, "components")
			process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), run.options)
			server := coverageServer{process, run.host}
			process.waitHealthy(t)
			if !process.logged(process.listeningLine(), "since_start_ms=") {
				t.Errorf("the listening line names no since_start_ms:\n%s", process.Output.String())
			}
			requireHeld(t, hold, "fill")
			requireHealthy(t, process)
			requireCoverageRefused(t, server, "SEARCH_COVERAGE_NOT_FILLED", "")
			if process.logged(coverageFilledLine) || filled(t, database) {
				t.Fatal("the fill ended while it was held")
			}

			endHold(t, hold)
			process.awaitLog(t, coverageFilledLine)
			requireCoverageIsMains(t, server, database)
		})
	}
}

// TestAFillCutOffByAStopFillsAgainAtTheNextStart holds R7l: a server stopped while its fill is
// held leaves done_at unset, and the next start, with nothing held, fills again and logs
// `coverage counts filled`; the route then answers the kept counts, equal to main's statement's.
func TestAFillCutOffByAStopFillsAgainAtTheNextStart(t *testing.T) {
	t.Parallel()
	database := coverageDatabase(t)
	hold := coverageHold(t, database, "components")
	first := startDispatchProcess(t, database.Pool.Config().ConnString())
	first.waitHealthy(t)
	requireHeld(t, hold, "fill")
	first.Terminate(t)
	first.WaitExit(t, "SIGTERM")
	if first.logged(coverageFilledLine) || filled(t, database) {
		t.Fatalf("a fill cut off by a stop recorded its end:\n%s", first.Output.String())
	}
	endHold(t, hold)

	second := startDispatchProcess(t, database.Pool.Config().ConnString())
	second.waitHealthy(t)
	second.awaitLog(t, coverageFilledLine)
	if !filled(t, database) {
		t.Fatal("the next start's fill ended and left done_at unset")
	}
	requireCoverageIsMains(t, coverageServer{dispatchProcess: second}, database)
}

// TestAFilledStartListensBeforeItsRecountEnds holds R7m: a server started on counts a fill has
// filled, its recount held where it runs, listens, and /healthz and the route answer at once,
// the route 200 with the kept counts; once the hold ends the log holds `coverage counts
// recounted`, it never holds `coverage counts filled`, and the kept counts equal main's statement's
// (which reads components, and so is read once the hold ends).
func TestAFilledStartListensBeforeItsRecountEnds(t *testing.T) {
	t.Parallel()
	database := coverageDatabase(t)
	if err := embedqueue.FillCoverage(context.Background(), database); err != nil {
		t.Fatalf("fill the counts before the start: %v", err)
	}
	hold := coverageHold(t, database, "components")
	process := startDispatchProcess(t, database.Pool.Config().ConnString())
	server := coverageServer{dispatchProcess: process}
	process.waitHealthy(t)
	requireHeld(t, hold, "recount")
	requireHealthy(t, process)
	if answer := server.coverage(t); answer.status != http.StatusOK {
		t.Fatalf("while the recount is held the coverage route answers %s, want 200 with the kept counts", answer)
	}
	if process.logged(coverageRecountedLine) {
		t.Fatal("the recount ended while it was held")
	}

	endHold(t, hold)
	process.awaitLog(t, coverageRecountedLine)
	if process.logged(coverageFilledLine) {
		t.Errorf("a start on filled counts filled them:\n%s", process.Output.String())
	}
	requireCoverageIsMains(t, server, database)
}

// coverageFillTestBackoff is DISPATCH_TEST_COVERAGE_FILL_BACKOFF in the two rows that wait out a
// failed fill's retry, in place of a deployment's 10 s first bound: the retry still comes at a
// random moment inside it (full jitter), and each row reads that the bound reached the loop
// (requireFirstCoverageFillRetry).
const coverageFillTestBackoff = 100 * time.Millisecond

// requireFirstCoverageFillRetry checks the wait the server chose, not how long the retry took.
// Only the first failure uses the initial bound. The caller already waited for a failure line.
func requireFirstCoverageFillRetry(t *testing.T, process *dispatchProcess) {
	t.Helper()
	line := process.logLines(coverageFillFailedLine)[0]
	retryIn, found := horizontest.Field(line, "retryIn")
	if !found {
		t.Fatalf("the first failed fill logged no retryIn: %s", line)
	}
	retry, err := time.ParseDuration(retryIn)
	if err != nil {
		t.Fatalf("read retryIn from the first failed fill: %v\n%s", err, line)
	}
	if retry > coverageFillTestBackoff {
		t.Fatalf("the first failed fill logged retryIn=%s, want at most %s:\n%s", retry, coverageFillTestBackoff, line)
	}
}

// TestAFailedFillKeepsServingAndRefusesUntilAFillEnds holds R7n for a fill that fails
// (DISPATCH_TEST_COVERAGE_FILL_FAILURES=1, before its first statement): the server keeps serving,
// logs the failure at ERROR, and answers 503 SEARCH_COVERAGE_NOT_FILLED while the fill after it is
// held; once that one ends the route answers the kept counts, equal to main's statement's.
func TestAFailedFillKeepsServingAndRefusesUntilAFillEnds(t *testing.T) {
	t.Parallel()
	database := coverageDatabase(t)
	hold := coverageHold(t, database, "components")
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{Env: map[string]string{
		"DISPATCH_TEST_COVERAGE_FILL_FAILURES": "1",
		"DISPATCH_TEST_COVERAGE_FILL_BACKOFF":  coverageFillTestBackoff.String(),
	}})
	process.waitHealthy(t)
	process.awaitLog(t, coverageFillFailedLine, "fill was made to fail")
	requireFirstCoverageFillRetry(t, process)
	requireHealthy(t, process)
	requireCoverageRefused(t, coverageServer{dispatchProcess: process}, "SEARCH_COVERAGE_NOT_FILLED", "")

	endHold(t, hold)
	process.awaitLog(t, coverageFilledLine)
	requireCoverageIsMains(t, coverageServer{dispatchProcess: process}, database)
}

// TestAFillPastItsTimeoutIsAFailedFill holds R7n for a fill held until its 1 s timeout.
// The server keeps serving, logs the failure at ERROR as a timeout, and answers 503
// SEARCH_COVERAGE_NOT_FILLED until a later fill ends. The route then answers the kept counts,
// equal to main's statements.
func TestAFillPastItsTimeoutIsAFailedFill(t *testing.T) {
	t.Parallel()
	database := coverageDatabase(t)
	hold := coverageHold(t, database, "components")
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{Env: map[string]string{
		"DISPATCH_TEST_COVERAGE_FILL_TIMEOUT": "1s",
		"DISPATCH_TEST_COVERAGE_FILL_BACKOFF": coverageFillTestBackoff.String(),
	}})
	process.waitHealthy(t)
	process.awaitLog(t, coverageFillFailedLine, "timed_out=true")
	requireFirstCoverageFillRetry(t, process)
	requireHealthy(t, process)
	requireCoverageRefused(t, coverageServer{dispatchProcess: process}, "SEARCH_COVERAGE_NOT_FILLED", "")

	// The timeout is proven by its line and the refusal above, both before the hold ends.
	endHold(t, hold)
	process.awaitLog(t, coverageFilledLine)
	requireCoverageIsMains(t, coverageServer{dispatchProcess: process}, database)
}

// TestTheCoverageRouteAnswersFoldBehindPastTheBound holds R7j's route: against a server whose
// counts are filled, with the fold held by the counts lock on the test's own connection
// (pg_advisory_lock, which the fold's transaction lock waits on) and 100,001 delta rows unfolded,
// the route answers 503 SEARCH_COVERAGE_FOLD_BEHIND naming the bound of 100,000.
func TestTheCoverageRouteAnswersFoldBehindPastTheBound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := coverageDatabase(t)
	process := startDispatchProcess(t, database.Pool.Config().ConnString())
	process.waitHealthy(t)
	process.awaitLog(t, coverageFilledLine)
	conn, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire the connection that holds the counts lock: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `select pg_advisory_lock(hashtext('search-coverage-counts'))`); err != nil {
		t.Fatalf("hold the counts lock: %v", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), `select pg_advisory_unlock_all()`) }()
	if _, err := conn.Exec(ctx, `
		insert into search_coverage_deltas (kind, model, matched, dead, items)
		select 'component', '', false, false, case when g = 100001 then 0 when g % 2 = 0 then 1 else -1 end
		from generate_series(1, 100001) g`); err != nil {
		t.Fatalf("leave 100,001 delta rows unfolded: %v", err)
	}
	requireCoverageRefused(t, coverageServer{dispatchProcess: process}, "SEARCH_COVERAGE_FOLD_BEHIND", "100,000")
}

// TestTheCoverageRouteAnswersItsTimeoutAt503 holds R7p's server row: a server whose route's
// statement is bound to 200 ms (DISPATCH_TEST_COVERAGE_STATEMENT_TIMEOUT), with
// search_coverage_counts held in access exclusive mode by the test once its counts are filled,
// answers the route 503 SEARCH_COVERAGE_TIMEOUT naming 200ms.
func TestTheCoverageRouteAnswersItsTimeoutAt503(t *testing.T) {
	t.Parallel()
	database := coverageDatabase(t)
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{Env: map[string]string{
		"DISPATCH_TEST_COVERAGE_STATEMENT_TIMEOUT": "200ms",
	}})
	process.waitHealthy(t)
	process.awaitLog(t, coverageFilledLine)
	coverageHold(t, database, "search_coverage_counts")
	requireCoverageRefused(t, coverageServer{dispatchProcess: process}, "SEARCH_COVERAGE_TIMEOUT", "200ms")
}
