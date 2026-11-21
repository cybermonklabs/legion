// as_of_cost_test.go holds the record of what a read at a cut costs at production's size (by the
// plan's departure D7). It sends the routes' own requests with as_of
// through the server, so each statement it times is the statement the route sends at a cut, bound
// by the store (asof.Bind) as every read at a cut is bound. The routes' statements are built in
// this package, which is why the test is here and not in internal/dispatch/asof.
package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/asof"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// asOfCorpusURLEnv names the database that holds the copy of production's event log that
// TestAsOfCostOnACorpusCopy reads.
const asOfCorpusURLEnv = "DISPATCH_ASOF_CORPUS_URL"

// asOfCorpusDerived are the tables that testdata/asof_corpus_derive.test.sql writes from the
// copy of the event log, in the order it writes them.
var asOfCorpusDerived = []string{
	"projects", "issues", "artifacts", "artifact_versions", "document_search", "events", "asks", "comments", "messages",
}

// asOfCostCuts are the two fixed cuts that the plan names. The third cut is the copy's newest
// event.
var asOfCostCuts = []string{"2026-10-26T12:00:00Z", "2026-11-10T00:00:00Z"}

// asOfCostSearches are the searches the test sends: two common words and a rare pair, the queries
// the plan measured the keyword table with.
var asOfCostSearches = []struct{ label, query string }{
	{label: "a common word", query: "dispatch"},
	{label: "the commonest word", query: "issue"},
	{label: "a rare pair", query: "snapshot token"},
}

const (
	// asOfCostRuns is how many times the test sends a read that its first run answers within its
	// bound: the first run is the cold one, and the table gives it and the median of the runs. A
	// read whose first run is over its bound is sent once.
	asOfCostRuns = 3
	// asOfCostCap is the longest the test waits for one read. A read still running then is
	// stopped, its statement cancelled, and its row says so: such a read is far over every bound,
	// and three cuts of reads that each run for many minutes would hold the pod for hours.
	asOfCostCap = 2 * time.Minute
	// asOfCostBound is the plan's bound on each read at a cut, on the pod.
	asOfCostBound = 2 * time.Second
	// asOfCostSearchMargin is how much longer than the present search the same search at a cut
	// may take.
	asOfCostSearchMargin = 3 * time.Second
	// asOfCostReader is the person the reads are sent as (headerIdentity).
	asOfCostReader = "alice"
	// asOfCostPage is the size of the event log page the test reads.
	asOfCostPage = 1000
)

// asOfCostRead is the key of the mark that the context of a timed read carries. The tracer times
// only the statements of a context that carries it.
type asOfCostRead struct{}

// asOfCostStarted is the key of the time that the tracer gives the context of a statement it
// times.
type asOfCostStarted struct{}

// asOfCostTracer times each statement and each batch that a timed read sends, from the start of
// its send to the end of its rows. A read's statements can overlap, so the tracer keeps each one's
// time and not a sum.
type asOfCostTracer struct {
	mu    sync.Mutex
	times []time.Duration
}

func (c *asOfCostTracer) start(ctx context.Context) context.Context {
	if ctx.Value(asOfCostRead{}) == nil {
		return ctx
	}
	return context.WithValue(ctx, asOfCostStarted{}, time.Now())
}

func (c *asOfCostTracer) end(ctx context.Context) {
	started, ok := ctx.Value(asOfCostStarted{}).(time.Time)
	if !ok {
		return
	}
	elapsed := time.Since(started)
	c.mu.Lock()
	c.times = append(c.times, elapsed)
	c.mu.Unlock()
}

func (c *asOfCostTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return c.start(ctx)
}

func (c *asOfCostTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	c.end(ctx)
}

func (c *asOfCostTracer) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return c.start(ctx)
}

func (c *asOfCostTracer) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

func (c *asOfCostTracer) TraceBatchEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchEndData) {
	c.end(ctx)
}

// take answers how many statements and batches the tracer timed since the last take, and the
// longest of them, and forgets them.
func (c *asOfCostTracer) take() (int, time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	longest := time.Duration(0)
	for _, elapsed := range c.times {
		longest = max(longest, elapsed)
	}
	count := len(c.times)
	c.times = c.times[:0]
	return count, longest
}

// asOfCostRow is one row of the printed table: what the row times, at which cut, how many times it
// was sent, the first run's time and the median of the runs, how many statements the last run
// sent and the longest of them, and the bound the median is held to. stopped is a read that the
// cap stopped. A row the copy cannot time holds why in note.
type asOfCostRow struct {
	statement, cut string
	runs           int
	first, median  time.Duration
	statements     int
	longest        time.Duration
	bound          time.Duration
	stopped        bool
	note           string
}

// asOfCostRig sends the timed reads to the server, as asOfCostReader.
type asOfCostRig struct {
	handler http.Handler
	tracer  *asOfCostTracer
}

// time sends target up to asOfCostRuns times, each run for asOfCostCap at most, and answers its
// row. A run over the bound, or stopped at the cap, is the last. A read at a cut must be answered
// 200 with the cut in Dispatch-As-Of, and a present read 200, or the test stops: a time of a
// refusal is no time of the read.
func (r *asOfCostRig) time(t *testing.T, statement, cut, target string, bound time.Duration) asOfCostRow {
	t.Helper()
	runs := make([]time.Duration, 0, asOfCostRuns)
	row := asOfCostRow{statement: statement, cut: cut, bound: bound}
	for len(runs) < asOfCostRuns {
		r.tracer.take()
		ctx, cancel := context.WithTimeout(context.WithValue(t.Context(), asOfCostRead{}, true), asOfCostCap)
		request := httptest.NewRequest(http.MethodGet, target, nil).WithContext(ctx)
		request.Header.Set("X-Dispatch-User", asOfCostReader)
		response := httptest.NewRecorder()
		started := time.Now()
		r.handler.ServeHTTP(response, request)
		elapsed := time.Since(started)
		expired := ctx.Err() != nil
		cancel()
		runs = append(runs, elapsed)
		row.statements, row.longest = r.tracer.take()
		if response.Code != http.StatusOK && expired {
			row.stopped = true
			break
		}
		if response.Code != http.StatusOK {
			t.Fatalf("%s at %s: GET %s answered %d: %.600s", statement, cut, target, response.Code, response.Body.String())
		}
		if cut != "present" && response.Header().Get(contracts.DispatchAsOfHeader) == "" {
			t.Fatalf("%s at %s: GET %s answered with no %s header, so it did not answer as of the cut",
				statement, cut, target, contracts.DispatchAsOfHeader)
		}
		if bound > 0 && elapsed > bound {
			break
		}
	}
	row.runs = len(runs)
	row.first = runs[0]
	slices.Sort(runs)
	row.median = runs[len(runs)/2]
	return row
}

// asOfCostTargets are the items that the reads at one cut name: the issue with the most events at
// the cut, the issue and the project with the most asks at the cut, the newest event id at the
// cut, and counts that the row labels give.
type asOfCostTargets struct {
	busiestIssue, askedIssue, askedProject string
	newestEventID                          int64
	issues, asks, events                   int64
}

// asOfCostTargetsAt reads the targets of cut from the present tables, which hold every item, with
// no cut on the context, so that no statement of the choice is a statement the test times.
func asOfCostTargetsAt(t *testing.T, database *store.Store, cut asof.Cut) asOfCostTargets {
	t.Helper()
	var targets asOfCostTargets
	err := database.Pool.QueryRow(t.Context(), `
		select (select e.issue_key from public.events e
		         where e.created_at <= $1 and e.issue_key is not null
		         group by e.issue_key order by count(*) desc, e.issue_key limit 1),
		       (select e.issue_key from public.events e
		         where e.type = 'ask.opened' and e.created_at <= $1 and e.issue_key is not null
		         group by e.issue_key order by count(*) desc, e.issue_key limit 1),
		       (select coalesce(i.project_key, a.project_key) from public.events e
		          left join public.issues i on i.key = e.issue_key
		          left join public.artifacts a on a.id = e.artifact_id
		         where e.type = 'ask.opened' and e.created_at <= $1
		         group by 1 order by count(*) desc, 1 limit 1),
		       (select coalesce(max(e.id), 0) from public.events e where e.created_at <= $1),
		       (select count(*) from public.events e where e.type = 'issue.created' and e.created_at <= $1),
		       (select count(*) from public.events e where e.type = 'ask.opened' and e.created_at <= $1),
		       (select count(*) from public.events e where e.created_at <= $1)`, cut.Time()).
		Scan(&targets.busiestIssue, &targets.askedIssue, &targets.askedProject, &targets.newestEventID,
			&targets.issues, &targets.asks, &targets.events)
	if err != nil {
		t.Fatalf("read the targets of the reads at %s: %v", cut, err)
	}
	return targets
}

// TestAsOfCostOnACorpusCopy times, on a copy of production's event log, the statements that the
// routes send at a cut: the issue read, the issue listing, an issue's asks, the inbox, open asks,
// pages of the event log, and whole searches at a past time, each at three cuts (2026-10-26
// 12:00Z, 2026-11-10 00:00Z and the copy's newest event), with each read at the present for
// comparison. It prints one table: what each row reads, its cut, how many runs it
// sent, the first run's time and the median of the runs (the whole request, the server's Go
// included), how many statements the request sent and the longest of them, and the plan's bound
// the median is held to. A read is sent three times, or once when its first run is over its
// bound, and a run is stopped at asOfCostCap. The test fails no bound: what a copy answers in
// depends on the machine, and the pull request records the figures. It is gated
// on DISPATCH_ASOF_CORPUS_URL, so CI never runs it.
//
// The database DISPATCH_ASOF_CORPUS_URL names is a new database of the test server (createdb, so
// that it holds the extensions the migrations need), and it holds the copy as the table
// corpus.events, with the columns of GET /api/v1/events/log, which a gzip CSV of the log's pages
// loads:
//
//	create schema corpus;
//	create table corpus.events (id bigint primary key, issue_key text, artifact_id uuid, project text,
//	    seq integer, type text not null, actor jsonb not null, payload jsonb not null, created_at timestamptz not null);
//	\copy corpus.events (id, issue_key, artifact_id, project, seq, type, actor, payload, created_at) from program 'gunzip -c events.csv.gz' csv
//
// events refers to its issue, its document and its project, which the log does not hold, so the
// copy is loaded beside it and not into it.
//
// The test migrates the database. The shadows read the present tables beside events, which the
// event log does not hold, so on its first run the test derives them from the copy and moves the
// copy into public.events (testdata/asof_corpus_derive.test.sql), and vacuums them. Each run then
// fills the keyword table to the copy's newest event (textversions.FillToHorizon), which the first
// run does from the start of the log. What a copy derived from the event log cannot give, the
// output says: no event holds a document's text, so each document version is empty, and no event
// holds an embedding, so a search with meaning is not timed. A database that holds no
// corpus.events, such as a restored dump, is read as it stands, with no such notes.
func TestAsOfCostOnACorpusCopy(t *testing.T) {
	databaseURL := os.Getenv(asOfCorpusURLEnv)
	if databaseURL == "" {
		t.Skip(asOfCorpusURLEnv + " must name a database that holds a copy of production's event log in corpus.events " +
			"(TestAsOfCostOnACorpusCopy's doc comment says how); the test times reads at a cut on it")
	}
	ctx := t.Context()
	database, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open the corpus copy: %v", err)
	}
	t.Cleanup(database.Pool.Close)
	if !asOfCorpusHoldsTheCopy(t, database) {
		t.Skip("the database " + asOfCorpusURLEnv + " names holds no copy of the event log: corpus.events and public.events " +
			"are empty or missing (TestAsOfCostOnACorpusCopy's doc comment says how to load it)")
	}
	started := time.Now()
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate the corpus copy: %v", err)
	}
	t.Logf("migrated the copy in %v", time.Since(started).Round(time.Millisecond))
	derived := deriveAsOfCorpus(t, database)

	var newest time.Time
	if err := database.Pool.QueryRow(ctx, `select max(created_at) from public.events`).Scan(&newest); err != nil {
		t.Fatalf("read the copy's newest event: %v", err)
	}
	cuts := make([]asof.Cut, 0, len(asOfCostCuts)+1)
	for _, raw := range asOfCostCuts {
		cut, err := asof.ParseCut(raw)
		if err != nil {
			t.Fatalf("parse the cut %s: %v", raw, err)
		}
		cuts = append(cuts, cut)
	}
	cuts = append(cuts, asof.CutAt(newest))

	started = time.Now()
	fillTextVersionsPast(t, database, cuts[len(cuts)-1])
	if _, err := database.Pool.Exec(ctx, `vacuum analyze public.search_text_versions`); err != nil {
		t.Fatalf("vacuum the keyword table: %v", err)
	}
	t.Logf("filled the keyword table to the copy's newest event in %v", time.Since(started).Round(time.Millisecond))

	tracer := &asOfCostTracer{}
	handler, _, _, _ := newRegisteredTestServer(t, testServerOptions{store: database, tracer: tracer})
	rig := &asOfCostRig{handler: handler, tracer: tracer}

	// Each read is timed at the present too, with the targets of the newest cut: what the same
	// read costs on this copy and machine with no cut, beside which a time at a cut is read.
	rows := []asOfCostRow{}
	for _, read := range asOfCostReads(asOfCostTargetsAt(t, database, cuts[len(cuts)-1])) {
		rows = append(rows, rig.time(t, "present "+read.statement, "present", read.target, 0))
	}
	present := map[string]time.Duration{}
	for _, search := range asOfCostSearches {
		row := rig.time(t, "present search, keyword only: "+search.label, "present",
			"/api/v1/search/results?"+url.Values{"q": {search.query}, "keywords_only": {"true"}}.Encode(), 0)
		present[search.query] = row.median
		rows = append(rows, row)
	}
	if derived {
		rows = append(rows, asOfCostRow{statement: "present search with meaning", cut: "present",
			note: "not timed: the copy holds no embedding (no event holds one), so the meaning legs would read no window"})
	}

	for _, cut := range cuts {
		for _, read := range asOfCostReads(asOfCostTargetsAt(t, database, cut)) {
			rows = append(rows, rig.time(t, read.statement, cut.String(), withAsOf(t, read.target, cut), asOfCostBound))
		}
		for _, search := range asOfCostSearches {
			rows = append(rows, rig.time(t, "search at the cut, keyword only, the document leg included: "+search.label, cut.String(),
				withAsOf(t, "/api/v1/search/results?"+url.Values{"q": {search.query}, "keywords_only": {"true"}}.Encode(), cut),
				present[search.query]+asOfCostSearchMargin))
		}
		if derived {
			rows = append(rows, asOfCostRow{statement: "search at the cut with meaning", cut: cut.String(),
				note: "not timed: the copy holds no embedding (no event holds one), so the meaning legs would read no window"})
		}
	}

	if derived {
		t.Logf("documents carry no text in the copy (no event holds it), so the document leg reads each document's version " +
			"at the cut and tests an empty vector")
	}
	t.Logf("the copy's newest event is at %s\n%s", cuts[len(cuts)-1], asOfCostTable(rows))
	t.Logf("sizes:\n%s", asOfCorpusSizes(t, database))
}

// asOfCostTimed is one read of the table but a search: what it reads, and its path and query.
type asOfCostTimed struct{ statement, target string }

// asOfCostReads are the reads of the table but the searches, on targets.
func asOfCostReads(targets asOfCostTargets) []asOfCostTimed {
	return []asOfCostTimed{
		{"issue read GET /api/v1/issues/{key}: the issue with the most events", "/api/v1/issues/" + url.PathEscape(targets.busiestIssue)},
		{fmt.Sprintf("issue listing GET /api/v1/issues: every issue (%d)", targets.issues), "/api/v1/issues"},
		{"an issue's asks GET /api/v1/issues/{key}/asks?state=all&thread=1: the issue with the most asks",
			"/api/v1/issues/" + url.PathEscape(targets.askedIssue) + "/asks?state=all&thread=1"},
		{fmt.Sprintf("inbox GET /api/v1/inbox: every open ask (of %d asks)", targets.asks), "/api/v1/inbox"},
		{"open asks GET /api/v1/asks/open?project=: the project with the most asks",
			"/api/v1/asks/open?" + url.Values{"project": {targets.askedProject}}.Encode()},
		{fmt.Sprintf("event log GET /api/v1/events/log: the page that ends at the cut (of %d events)", targets.events),
			"/api/v1/events/log?after=" + strconv.FormatInt(max(targets.newestEventID-asOfCostPage, 0), 10)},
		{"event log GET /api/v1/events/log?after=0: the first page", "/api/v1/events/log?after=0"},
	}
}

// asOfCorpusHoldsTheCopy reports whether database holds a copy of the event log: rows in
// corpus.events, which the test has not derived yet, or rows in public.events, which it has.
func asOfCorpusHoldsTheCopy(t *testing.T, database *store.Store) bool {
	t.Helper()
	for _, table := range []string{"corpus.events", "public.events"} {
		var exists, holds bool
		if err := database.Pool.QueryRow(t.Context(), `select to_regclass($1) is not null`, table).Scan(&exists); err != nil {
			t.Fatalf("read whether %s exists: %v", table, err)
		}
		if !exists {
			continue
		}
		if err := database.Pool.QueryRow(t.Context(), "select exists (select from "+table+")").Scan(&holds); err != nil {
			t.Fatalf("read whether %s holds rows: %v", table, err)
		}
		if holds {
			return true
		}
	}
	return false
}

// deriveAsOfCorpus writes, on the first run, the present tables the shadows join on from the copy
// in corpus.events, and the copy into public.events (testdata/asof_corpus_derive.test.sql), then
// vacuums them, as production's tables are; a later run finds public.events filled and writes
// nothing. It reports whether the present tables are derived from corpus.events, now or by an
// earlier run, and then logs them with their rows. A database whose public.events is filled and
// that holds no corpus.events (a restored dump) is not derived: it reports false and logs nothing.
func deriveAsOfCorpus(t *testing.T, database *store.Store) bool {
	t.Helper()
	ctx := t.Context()
	var filled, copied bool
	if err := database.Pool.QueryRow(ctx, `select exists (select from public.events), to_regclass('corpus.events') is not null`).Scan(&filled, &copied); err != nil {
		t.Fatalf("read whether the copy is derived: %v", err)
	}
	if !copied {
		return false
	}
	if !filled {
		statements, err := os.ReadFile("testdata/asof_corpus_derive.test.sql")
		if err != nil {
			t.Fatalf("read the derivation: %v", err)
		}
		started := time.Now()
		if _, err := database.Pool.Exec(ctx, string(statements)); err != nil {
			t.Fatalf("derive the present tables from the copy: %v", err)
		}
		t.Logf("derived the present tables from corpus.events in %v", time.Since(started).Round(time.Millisecond))
		started = time.Now()
		for _, table := range asOfCorpusDerived {
			if _, err := database.Pool.Exec(ctx, "vacuum analyze public."+table); err != nil {
				t.Fatalf("vacuum public.%s: %v", table, err)
			}
		}
		t.Logf("vacuumed the derived tables in %v", time.Since(started).Round(time.Millisecond))
	}
	counts := make([]string, 0, len(asOfCorpusDerived))
	for _, table := range asOfCorpusDerived {
		var rows int64
		if err := database.Pool.QueryRow(ctx, "select count(*) from public."+table).Scan(&rows); err != nil {
			t.Fatalf("count public.%s: %v", table, err)
		}
		counts = append(counts, fmt.Sprintf("%s %d", table, rows))
	}
	t.Logf("tables derived from the copy of the event log (testdata/asof_corpus_derive.test.sql), with their rows: %s",
		strings.Join(counts, ", "))
	return true
}

// asOfCostTable is rows as a table of aligned columns.
func asOfCostTable(rows []asOfCostRow) string {
	var table strings.Builder
	writer := tabwriter.NewWriter(&table, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "statement\tcut\truns\tfirst ms\tmedian ms\tstatements\tlongest statement ms\tbound ms\twithin")
	for _, row := range rows {
		if row.note != "" {
			fmt.Fprintf(writer, "%s\t%s\t0\t-\t-\t-\t-\t-\t%s\n", row.statement, row.cut, row.note)
			continue
		}
		bound, within := "-", "-"
		if row.bound > 0 {
			bound = strconv.FormatInt(row.bound.Milliseconds(), 10)
			within = "yes"
			if row.median > row.bound {
				within = "OVER"
			}
		}
		first, median := strconv.FormatInt(row.first.Milliseconds(), 10), strconv.FormatInt(row.median.Milliseconds(), 10)
		if row.stopped {
			first, median = ">"+first, "-"
			within = fmt.Sprintf("OVER: stopped at the cap of %v, its statement cancelled", asOfCostCap)
		}
		fmt.Fprintf(writer, "%s\t%s\t%d\t%s\t%s\t%d\t%d\t%s\t%s\n", row.statement, row.cut, row.runs, first, median,
			row.statements, row.longest.Milliseconds(), bound, within)
	}
	_ = writer.Flush()
	return table.String()
}

// asOfCorpusSizes is the size of events with its indexes, of each index of events, and of the
// keyword table with its indexes.
func asOfCorpusSizes(t *testing.T, database *store.Store) string {
	t.Helper()
	rows, err := database.Pool.Query(t.Context(), `
		select 'events, its indexes and its TOAST (pg_total_relation_size)', pg_total_relation_size('public.events'), 0
		union all
		select 'events heap (pg_relation_size)', pg_relation_size('public.events'), 1
		union all
		select 'index ' || c.relname, pg_relation_size(c.oid), 2
		  from pg_index x join pg_class c on c.oid = x.indexrelid
		 where x.indrelid = 'public.events'::regclass
		union all
		select 'search_text_versions, its indexes and its TOAST (pg_total_relation_size)', pg_total_relation_size('public.search_text_versions'), 3
		order by 3, 1`)
	if err != nil {
		t.Fatalf("read the copy's sizes: %v", err)
	}
	defer rows.Close()
	var sizes strings.Builder
	writer := tabwriter.NewWriter(&sizes, 0, 0, 2, ' ', 0)
	for rows.Next() {
		var name string
		var bytes int64
		var order int
		if err := rows.Scan(&name, &bytes, &order); err != nil {
			t.Fatalf("scan the copy's sizes: %v", err)
		}
		fmt.Fprintf(writer, "%s\t%.1f MB\n", name, float64(bytes)/(1<<20))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the copy's sizes: %v", err)
	}
	_ = writer.Flush()
	return sizes.String()
}
