package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/evals/evalbench"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The budgets of the first paint (spec R8 and R16): database time for 84 days with an 84-day
// compare range and for 28 days with none, the compressed body, and the server time of the first
// paint that reads the day rows of one repository.
const (
	budget84Ms     = 150
	budget28Ms     = 60
	budgetGzip     = 300 * 1000
	budgetServerMs = 400
)

// window is a range of whole UTC days, [from, to).
type window struct{ from, to time.Time }

// before is the days whole days ending at end.
func before(end time.Time, days int) window {
	return window{from: end.AddDate(0, 0, -days), to: end}
}

func (w window) String() string {
	return w.from.Format(time.DateOnly) + ".." + w.to.Format(time.DateOnly)
}

// liveWindow is the window as a live view names one.
func (w window) live() map[string]string {
	return map[string]string{"from": w.from.Format(time.RFC3339), "to": w.to.Format(time.RFC3339)}
}

// paintCase is one read of a first paint: a GET (path) or a live view's snapshot (view, of kind),
// and its budgets, none where zero.
type paintCase struct {
	name           string
	path           string
	view           map[string]any
	kind           string
	budgetMs       float64
	serverBudgetMs float64
}

// measuredCase is one read as the instrument saw it.
type measuredCase struct {
	paintCase
	t0, t1     time.Time
	status     int
	body       []byte
	serverMs   float64
	all        []statement
	own        []statement
	background []statement
	plans      map[int64]string
	statements map[int64]string
	params     map[int64][]string
}

// dbMs is the database time of statements.
func dbMs(statements []statement) float64 {
	total := 0.0
	for _, one := range statements {
		total += one.execMs
	}
	return total
}

// gzipSize is how many bytes body takes compressed, at gzip's default level, as a server that
// compresses its answers sends it.
func gzipSize(body []byte) int {
	var packed bytes.Buffer
	writer := gzip.NewWriter(&packed)
	_, _ = writer.Write(body)
	_ = writer.Close()
	return packed.Len()
}

// headCases are this head's first paint: the list read at 84 days with an 84-day compare range
// and at 28 days with none, each with no repository and with the deploy repository, and the
// evals view the page keeps itself current over.
func headCases(project string, today time.Time) []paintCase {
	w84, c84, w28 := before(today, 84), before(today.AddDate(0, 0, -84), 84), before(today, 28)
	list := func(w window, compare *window, repo string) string {
		query := url.Values{"from": {w.from.Format(time.RFC3339)}, "to": {w.to.Format(time.RFC3339)}, "limit": {"200"}}
		if compare != nil {
			query.Set("compare_from", compare.from.Format(time.RFC3339))
			query.Set("compare_to", compare.to.Format(time.RFC3339))
		}
		if repo != "" {
			query.Set("repo", repo)
		}
		return "/api/v1/projects/" + project + "/evals?" + query.Encode()
	}
	compare := url.Values{"compare_from": {c84.from.Format(time.RFC3339)}, "compare_to": {c84.to.Format(time.RFC3339)}, "limit": {"200"}}
	repo := evalbench.Repos[0]
	return []paintCase{
		{name: "list-84d-compare", path: list(w84, &c84, ""), budgetMs: budget84Ms},
		{name: "list-84d-compare-repo", path: list(w84, &c84, repo), budgetMs: budget84Ms, serverBudgetMs: budgetServerMs},
		{name: "list-28d", path: list(w28, nil, ""), budgetMs: budget28Ms},
		{name: "list-28d-repo", path: list(w28, nil, repo), budgetMs: budget28Ms, serverBudgetMs: budgetServerMs},
		{name: "view-evals-84d-compare", kind: "evals", budgetMs: budget84Ms,
			view: map[string]any{"kind": "evals", "project": project, "window": w84.live(), "query": compare.Encode()}},
	}
}

// baseCases are the base Delivery page's first paint at 84 days with a compare range: the
// timeline view, the measures view of the compare range (the panel's own read of it), and the
// Pipeline section's view of the window and of the compare range (02a7b3c6's
// packages/dispatch/web DeliveryPage.tsx and PipelineSection.tsx).
func baseCases(today time.Time) []paintCase {
	w84, c84 := before(today, 84), before(today.AddDate(0, 0, -84), 84)
	return []paintCase{
		{name: "base-timeline-84d", kind: "timeline", view: map[string]any{"kind": "timeline", "window": w84.live(), "query": ""}},
		{name: "base-measures-compare-84d", kind: "measures", view: map[string]any{"kind": "measures", "window": c84.live(), "query": ""}},
		{name: "base-pipeline-84d", kind: "pipeline", view: map[string]any{"kind": "pipeline", "window": w84.live()}},
		{name: "base-pipeline-compare-84d", kind: "pipeline", view: map[string]any{"kind": "pipeline", "window": c84.live()}},
	}
}

// paint measures the first paint (the package comment) and, at this head and outside -info,
// k2, k13 and k16 after it.
func paint(ctx context.Context, r *report, o options) error {
	database, err := store.Open(ctx, o.database)
	if err != nil {
		return err
	}
	defer database.Pool.Close()
	pool := database.Pool
	if _, err := pool.Exec(ctx, `create extension if not exists pg_stat_statements`); err != nil {
		return fmt.Errorf("create pg_stat_statements: %w", err)
	}
	if err := setStatementLog(ctx, pool, true); err != nil {
		return err
	}
	defer func() {
		if err := setStatementLog(context.WithoutCancel(ctx), pool, false); err != nil {
			r.fail("statement-log %v", err)
		}
	}()
	today := time.Now().UTC().Truncate(24 * time.Hour)
	r.result("paint mode=%s project=%s today=%s quiet_seconds=%.0f catalog_seconds=%.0f", o.mode, o.project, today.Format(time.DateOnly), o.quiet.Seconds(), o.catalog.Seconds())

	// The catalogue of the server's background statements: a window with no request in it.
	time.Sleep(o.quiet)
	if err := resetStatements(ctx, pool); err != nil {
		return err
	}
	time.Sleep(o.catalog)
	backgroundRun, err := snapshotStatements(ctx, pool)
	if err != nil {
		return err
	}
	catalog := map[int64]bool{}
	for _, one := range backgroundRun {
		catalog[one.queryID] = true
	}
	r.result("catalog statements=%d calls=%d db_ms=%.1f", len(backgroundRun), calls(backgroundRun), dbMs(backgroundRun))

	cases := headCases(o.project, today)
	if o.mode == "base" {
		cases = baseCases(today)
	}
	firstLog, err := fileSize(o.pgLog)
	if err != nil {
		return fmt.Errorf("read the statement log %s: %w", o.pgLog, err)
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	measured := make([]measuredCase, len(cases))
	for index, one := range cases {
		measured[index], err = measure(ctx, pool, client, o, one, catalog)
		if err != nil {
			return fmt.Errorf("%s: %w", one.name, err)
		}
	}

	// The plans, once every read of the paint has run, so no explain warms a page a later read
	// of the paint finds warm.
	time.Sleep(time.Second)
	executions, err := readStatementLog(o.pgLog, firstLog)
	if err != nil {
		return err
	}
	explain, err := newExplainer(ctx, o.database)
	if err != nil {
		return err
	}
	defer explain.conn.Close(context.WithoutCancel(ctx))
	for index := range measured {
		end := measured[index].t1.Add(3 * time.Second)
		if index+1 < len(measured) {
			end = measured[index+1].t0
		}
		planCase(ctx, r, explain, &measured[index], executions, end)
	}
	judgePaint(r, o, measured)
	if o.info {
		return nil
	}
	if o.mode == "base" {
		baseControls(r, measured)
		return nil
	}
	checkFirstPaintReads(r, measured)
	checks := &headChecks{r: r, o: o, pool: pool, explain: explain, client: client, today: today}
	checks.k13()
	checks.k16()
	checks.k2()
	checks.probe(measured)
	return nil
}

// calls is how many executions statements counted.
func calls(statements []statement) int64 {
	var total int64
	for _, one := range statements {
		total += one.calls
	}
	return total
}

// measure runs one read of the paint, with pg_stat_statements reset before it, and reads what it
// cost.
func measure(ctx context.Context, pool *store.Pool, client *http.Client, o options, one paintCase, catalog map[int64]bool) (measuredCase, error) {
	got := measuredCase{paintCase: one, serverMs: -1}
	serverFrom, err := fileSize(o.serverLog)
	if err != nil {
		return got, fmt.Errorf("read the server log %s: %w", o.serverLog, err)
	}
	if err := resetStatements(ctx, pool); err != nil {
		return got, err
	}
	got.t0 = time.Now()
	if one.view != nil {
		got.status, got.body, err = liveSnapshot(ctx, o.url, o.token, one.view)
	} else {
		got.status, got.body, err = getBody(ctx, client, o.url+one.path, o.token)
	}
	got.t1 = time.Now()
	if err != nil {
		return got, err
	}
	// pg_stat_statements counts a statement as it ends, and the access log is written as the
	// handler returns, a moment after the answer leaves it.
	time.Sleep(500 * time.Millisecond)
	if got.all, err = snapshotStatements(ctx, pool); err != nil {
		return got, err
	}
	for _, statement := range got.all {
		if catalog[statement.queryID] {
			got.background = append(got.background, statement)
		} else {
			got.own = append(got.own, statement)
		}
	}
	got.serverMs, err = serverTime(o.serverLog, serverFrom, one)
	return got, err
}

// getBody is the status and body of a GET with the agent token.
func getBody(ctx context.Context, client *http.Client, target, token string) (int, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	return response.StatusCode, body, err
}

// liveSnapshot subscribes view over GET /ws/live with the agent token and answers the body of
// the snapshot the server sends it.
func liveSnapshot(ctx context.Context, base, token string, view map[string]any) (int, []byte, error) {
	dialer := websocket.Dialer{HandshakeTimeout: 30 * time.Second}
	conn, _, err := dialer.DialContext(ctx, "ws"+strings.TrimPrefix(base, "http")+"/ws/live", http.Header{"Authorization": {"Bearer " + token}})
	if err != nil {
		return 0, nil, fmt.Errorf("open the live socket: %w", err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(map[string]any{"type": "subscribe", "sub": "bench", "view": view}); err != nil {
		return 0, nil, fmt.Errorf("subscribe: %w", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Minute))
	for {
		var frame struct {
			Type    string          `json:"type"`
			Sub     string          `json:"sub"`
			Body    json.RawMessage `json:"body"`
			Code    string          `json:"code"`
			Message string          `json:"message"`
		}
		if err := conn.ReadJSON(&frame); err != nil {
			return 0, nil, fmt.Errorf("read the live socket: %w", err)
		}
		switch {
		case frame.Type == "snapshot" && frame.Sub == "bench":
			_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			return http.StatusOK, frame.Body, nil
		case frame.Type == "error":
			return 0, nil, fmt.Errorf("the server refused the view: %s %s", frame.Code, frame.Message)
		}
	}
}

// serverTime is the server's own time for the read: the access log's elapsed_ms for a GET, the
// live hub's recompute line for a view's snapshot. It waits up to three seconds for the line.
func serverTime(path string, from int64, one paintCase) (float64, error) {
	pattern := regexp.MustCompile(`msg="dispatch: request" .* path=` + regexp.QuoteMeta(strings.SplitN(one.path, "?", 2)[0]) + ` .*elapsed_ms=([0-9]+)`)
	if one.view != nil {
		pattern = regexp.MustCompile(`dispatch: delivery live recompute view=` + regexp.QuoteMeta(one.kind) + ` cause=subscribe elapsed_ms=([0-9]+)`)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		file, err := os.Open(path)
		if err != nil {
			return -1, err
		}
		if _, err := file.Seek(from, 0); err != nil {
			file.Close()
			return -1, err
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 1<<20), 16<<20)
		for scanner.Scan() {
			if match := pattern.FindStringSubmatch(scanner.Text()); match != nil {
				file.Close()
				ms, _ := strconv.ParseFloat(match[1], 64)
				return ms, nil
			}
		}
		file.Close()
		if time.Now().After(deadline) {
			return -1, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// planCase explains each of the read's own statements with the parameters of its first execution
// in the read's span of the statement log, [t0, end), and keeps the text and parameters it explained.
func planCase(ctx context.Context, r *report, explain *explainer, one *measuredCase, executions []logged, end time.Time) {
	one.plans, one.statements, one.params = map[int64]string{}, map[int64]string{}, map[int64][]string{}
	for _, own := range one.own {
		var found *logged
		for index := range executions {
			execution := &executions[index]
			if execution.queryID == own.queryID && !execution.at.Before(one.t0.Add(-100*time.Millisecond)) && execution.at.Before(end) {
				found = execution
				break
			}
		}
		if found == nil {
			r.fail("plan case=%s queryid=%d: no execution in the statement log between %s and %s, so its plan was not captured: %s",
				one.name, own.queryID, one.t0.UTC().Format(time.RFC3339Nano), end.UTC().Format(time.RFC3339Nano), oneLine(own.query))
			continue
		}
		one.statements[own.queryID], one.params[own.queryID] = found.text, found.params
		plan, _, err := explain.explain(ctx, found.text, found.params)
		if err == errUtility {
			continue
		}
		if err != nil {
			r.fail("plan case=%s queryid=%d: explain failed: %v: %s", one.name, own.queryID, err, oneLine(found.text))
			continue
		}
		one.plans[own.queryID] = plan
	}
}

// judgePaint prints each read of the paint with its statements and plans, and judges its budgets
// (outside -info).
func judgePaint(r *report, o options, measured []measuredCase) {
	total, plans := 0.0, 0
	for _, one := range measured {
		own := dbMs(one.own)
		total += own
		plans += len(one.plans)
		gz := gzipSize(one.body)
		r.result("paint case=%s status=%d db_ms=%.2f statements=%d calls=%d background_statements=%d background_db_ms=%.2f db_ms_everything_in_flight=%.2f server_ms=%.0f client_ms=%.1f body_bytes=%d gzip_bytes=%d plans=%d",
			one.name, one.status, own, len(one.own), calls(one.own), len(one.background), dbMs(one.background), dbMs(one.all),
			one.serverMs, float64(one.t1.Sub(one.t0).Microseconds())/1000, len(one.body), gz, len(one.plans))
		for _, statement := range one.own {
			r.result("statement case=%s queryid=%d calls=%d exec_ms=%.3f rows=%d shared_hit=%d shared_read=%d", one.name, statement.queryID,
				statement.calls, statement.execMs, statement.rows, statement.hit, statement.read)
			text := statement.query
			if logged, held := one.statements[statement.queryID]; held {
				text = logged
			}
			r.block(fmt.Sprintf("SQL case=%s queryid=%d", one.name, statement.queryID), text)
			if plan, held := one.plans[statement.queryID]; held {
				r.block(fmt.Sprintf("PLAN case=%s queryid=%d", one.name, statement.queryID), plan)
			}
		}
		for _, statement := range one.background {
			r.result("background case=%s queryid=%d calls=%d exec_ms=%.3f %s", one.name, statement.queryID, statement.calls, statement.execMs, oneLine(statement.query))
		}
		if o.info {
			continue
		}
		if one.status != http.StatusOK {
			r.fail("paint case=%s answered %d", one.name, one.status)
		}
		if len(one.own) == 0 {
			r.fail("paint case=%s: no statement of its own was counted, so its database time measures nothing", one.name)
		}
		if one.budgetMs > 0 && own > one.budgetMs {
			r.fail("paint case=%s db_ms=%.2f is over its budget of %.0f ms", one.name, own, one.budgetMs)
		}
		if gz > budgetGzip {
			r.fail("paint case=%s gzip_bytes=%d is over its budget of %d", one.name, gz, budgetGzip)
		}
		if one.serverBudgetMs > 0 && (one.serverMs < 0 || one.serverMs > one.serverBudgetMs) {
			r.fail("paint case=%s server_ms=%.0f is over its budget of %.0f ms, or the server logged none", one.name, one.serverMs, one.serverBudgetMs)
		}
	}
	r.result("paint-total mode=%s reads=%d db_ms=%.2f plans=%d", o.mode, len(measured), total, plans)
	if !o.info && plans == 0 {
		r.fail("paint: no plan examined")
	}
}

// eventsRead is how a statement reads events: whether it reads the table at all, and whether it
// reads it only by the event id, through events_pkey, and names no actor. A read by id is the live
// hub's own bookkeeping of where the event log stands; any other read of events, or one of an
// event's actor, is a session title resolved out of events, which R13 rules out.
func eventsRead(text, plan string) (reads, byID bool) {
	scans, reads := relations(plan)["events"]
	if !reads {
		return false, false
	}
	for _, scan := range scans {
		if !strings.HasSuffix(scan, " events_pkey") {
			return true, false
		}
	}
	return true, !regexp.MustCompile(`(?i)\bactor\b`).MatchString(text)
}

// checkFirstPaintReads is k13's first clause: no statement of this head's first paint resolves a
// session title out of events (eventsRead) or scans issues. A read of events by its id alone is
// listed for information.
func checkFirstPaintReads(r *report, measured []measuredCase) {
	examined, titleReads, idReads, issueScans := 0, 0, 0, 0
	for _, one := range measured {
		for queryID, plan := range one.plans {
			examined++
			text := one.statements[queryID]
			switch reads, byID := eventsRead(text, plan); {
			case reads && byID:
				idReads++
				r.result("k13-events-by-id case=%s queryid=%d scans=%q: %s", one.name, queryID, relations(plan)["events"], oneLine(text))
			case reads:
				titleReads++
				r.fail("k13 case=%s queryid=%d reads events other than by its id, or an event's actor: %s", one.name, queryID, oneLine(text))
			}
			if seqScanned(plan, "issues") {
				issueScans++
				r.fail("k13 case=%s queryid=%d scans issues: %s", one.name, queryID, oneLine(text))
			}
		}
	}
	r.result("k13 first_paint_statements_examined=%d title_reads_of_events=%d reads_of_events_by_id=%d scans_of_issues=%d", examined, titleReads, idReads, issueScans)
	if examined == 0 {
		r.fail("k13: no statement of the first paint examined")
	}
}

// baseControls is k8's negative control and k13's at the base head: the base's first paint's
// database time against this head's budget, its statements that read events other than by id
// (where the base resolves session titles), and its run-jobs reads that name no job.
//
// The base's Legion facts consumer resolves session titles with the same statement its timeline
// does, so that statement is in the background catalogue and out of each read's own time. The
// control therefore also names every catalogued statement that ran while a read was in flight and
// reads an event's actor out of events, and states the paint's time with everything in flight
// beside its own.
func baseControls(r *report, measured []measuredCase) {
	total, inFlight, readsEvents, catalogued, unnamed, jobReads := 0.0, 0.0, 0, 0, 0, 0
	titleRead := regexp.MustCompile(`(?is)\bactor\b.*\bfrom events\b`)
	for _, one := range measured {
		total += dbMs(one.own)
		inFlight += dbMs(one.all)
		for queryID, plan := range one.plans {
			if reads, byID := eventsRead(one.statements[queryID], plan); reads && !byID {
				readsEvents++
				r.result("k13-control case=%s queryid=%d reads events other than by its id: %s", one.name, queryID, oneLine(one.statements[queryID]))
			}
			if _, reads := relations(plan)["delivery_run_jobs"]; reads {
				jobReads++
				if !regexp.MustCompile(`Index Cond: .*\bname = ANY`).MatchString(plan) {
					unnamed++
					r.result("k13-control case=%s queryid=%d reads delivery_run_jobs with no job name in its index condition: %s", one.name, queryID, oneLine(one.statements[queryID]))
				}
			}
		}
		for _, statement := range one.background {
			if titleRead.MatchString(statement.query) {
				catalogued++
				r.result("k13-control case=%s queryid=%d catalogued statement reads an event's actor out of events, exec_ms=%.3f: %s", one.name, statement.queryID, statement.execMs, oneLine(statement.query))
			}
		}
	}
	r.result("k8-control base_first_paint_db_ms=%.2f db_ms_everything_in_flight=%.2f this_heads_budget_ms=%d exceeds=%t", total, inFlight, budget84Ms, total > budget84Ms)
	r.result("k13-control statements_reading_events_other_than_by_id=%d catalogued_title_reads=%d run_jobs_reads=%d run_jobs_reads_without_a_name=%d", readsEvents, catalogued, jobReads, unnamed)
}
