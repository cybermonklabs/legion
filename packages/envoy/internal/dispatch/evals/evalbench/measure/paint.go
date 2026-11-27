package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/evals/evalbench"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The budgets of the first paint (spec R8 and R16): database time for 84 days with an 84-day
// compare range and for 28 days with none, the compressed body of each read of the evals' first
// paint, and the server time of each read of the evals' first paint, with no repository and with
// one (R16's first paint of 0.4 s, dispatch://INFRA-979/spec#b-r14). The body budget is the evals'
// alone: the root's R3 keeps the row-level merge timeline out of the first paint (it pages,
// dispatch://INFRA-2918 R13), so a read of the Delivery page (paintCase.delivery) has its body
// measured and judged by no budget, and its database time judged as every read's is.
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

// live is the window as a live view names one.
func (w window) live() map[string]string {
	return map[string]string{"from": w.from.Format(time.RFC3339), "to": w.to.Format(time.RFC3339)}
}

// paintCase is one read of a first paint: a GET (path) or a live view's snapshot (view, of kind),
// and its budgets, none where zero. A read of a group is judged with the group's other reads too,
// their database time together against groupBudgetMs. delivery marks a read of the Delivery page:
// the timeline's first paint at this head (timelineCases) and the base's (baseCases), whose body
// no budget judges. storedFigures marks a read of an offered range of the eval list, which the
// stored figures answer (checkStoredFiguresReads). strict marks a read whose database budget binds
// at ten times the stated run rate too (k14's, k14Cases), where the head's first paint's budgets
// are k8's at the stated rate alone (report.overStated).
type paintCase struct {
	name           string
	path           string
	view           map[string]any
	kind           string
	budgetMs       float64
	serverBudgetMs float64
	group          string
	groupBudgetMs  float64
	delivery       bool
	storedFigures  bool
	strict         bool
}

// measuredCase is one read as the instrument saw it: cataloguedOwn is how many statements of the
// background catalogue it ran itself, which its own time counts (ownAndBackground).
type measuredCase struct {
	paintCase
	t0, t1        time.Time
	status        int
	body          []byte
	serverMs      float64
	all           []statement
	own           []statement
	background    []statement
	cataloguedOwn int
	plans         map[int64]string
	statements    map[int64]string
	params        map[int64][]string
}

// dbMs is the database time of statements: their planning and their execution.
func dbMs(statements []statement) float64 {
	total := 0.0
	for _, one := range statements {
		total += one.planMs + one.execMs
	}
	return total
}

// planMs is the planning time of statements, the part of dbMs before their execution.
func planMs(statements []statement) float64 {
	total := 0.0
	for _, one := range statements {
		total += one.planMs
	}
	return total
}

// gzipLevel is the level the server compresses its answers at: gzip.BestSpeed, its
// compressors' one level (api/response_compression.go, gzipWriters), which sends about a ninth
// more bytes than the default level 6, so a body is judged by the bytes that go on the wire.
const gzipLevel = gzip.BestSpeed

// gzipSize is how many bytes body takes compressed at gzipLevel, as the server sends it to a
// caller that takes gzip.
func gzipSize(body []byte) int {
	var packed bytes.Buffer
	writer, _ := gzip.NewWriterLevel(&packed, gzipLevel)
	_, _ = writer.Write(body)
	_ = writer.Close()
	return packed.Len()
}

// headCases are this head's first paint in run run of the first paint's runs (k8; 0 for a paint
// measured once, read as 1): the list read at 84 days with an 84-day compare range and at 28 days
// with none, each with no repository and with the deploy repository, and the evals view the page
// keeps itself current over; then the Delivery timeline's (timelineCases). The reads with no
// repository are answered from the stored figures of the project's own rows, and those with the
// deploy repository from the stored figures of that repository's cut (evals.FiguresOffered),
// under the same database budget as the read of the same range with none, and each under the
// server budget of the first paint. R8 holds each list read to its budget on the first read after
// a restart, so the four list reads take turns at the head: run n starts with the
// ((n-1) mod 4)th of them and goes on in order, so over k8's eight runs each of them, the 28-day
// reads among them, is the first read after a restart twice.
func headCases(project string, today time.Time, run int) []paintCase {
	w84, c84, w28 := before(today, 84), before(today.AddDate(0, 0, -84), 84), before(today, 28)
	repo := evalbench.Repos[0]
	lists := []paintCase{
		{name: "list-84d-compare", path: evalListPath(project, w84, &c84, ""), budgetMs: budget84Ms, serverBudgetMs: budgetServerMs, storedFigures: true},
		{name: "list-84d-compare-repo", path: evalListPath(project, w84, &c84, repo), budgetMs: budget84Ms, serverBudgetMs: budgetServerMs, storedFigures: true},
		{name: "list-28d", path: evalListPath(project, w28, nil, ""), budgetMs: budget28Ms, serverBudgetMs: budgetServerMs, storedFigures: true},
		{name: "list-28d-repo", path: evalListPath(project, w28, nil, repo), budgetMs: budget28Ms, serverBudgetMs: budgetServerMs, storedFigures: true},
	}
	first := (max(run, 1) - 1) % len(lists)
	cases := slices.Concat(lists[first:], lists[:first])
	cases = append(cases, paintCase{name: "view-evals-84d-compare", kind: "evals", budgetMs: budget84Ms, serverBudgetMs: budgetServerMs, storedFigures: true,
		view: evalsView(project, w84, &c84)})
	return append(cases, timelineCases("", today)...)
}

// evalListQuery is the query of a read of the eval list over w: its window, its compare range
// where compare names one, its repository where repo names one, and its page of 200.
func evalListQuery(w window, compare *window, repo string) url.Values {
	query := url.Values{"from": {w.from.Format(time.RFC3339)}, "to": {w.to.Format(time.RFC3339)}, "limit": {"200"}}
	if compare != nil {
		query.Set("compare_from", compare.from.Format(time.RFC3339))
		query.Set("compare_to", compare.to.Format(time.RFC3339))
	}
	if repo != "" {
		query.Set("repo", repo)
	}
	return query
}

// evalListPath is the read of project's eval list over w (evalListQuery).
func evalListPath(project string, w window, compare *window, repo string) string {
	return "/api/v1/projects/" + project + "/evals?" + evalListQuery(w, compare, repo).Encode()
}

// k14Cases are k14's first paint of a k14 project (evalbench.K14Projects) in run run of its runs
// (0 read as 1): the Evals tab's list read at 84 days with an 84-day compare range and at 28 days
// with none, each with no repository, which the stored figures of the project's own cut answer,
// the fold of all its deploy repositories' rows (R14, T19 path 2), under the first paint's server
// budget and its database budgets, which R14 binds at both rates (strict). Run n starts with the
// ((n-1) mod 2)th of them, so over two runs each is the first read after a restart once.
func k14Cases(project string, today time.Time, run int) []paintCase {
	w84, c84, w28 := before(today, 84), before(today.AddDate(0, 0, -84), 84), before(today, 28)
	lists := []paintCase{
		{name: "list-84d-compare", path: evalListPath(project, w84, &c84, ""), budgetMs: budget84Ms, serverBudgetMs: budgetServerMs, storedFigures: true, strict: true},
		{name: "list-28d", path: evalListPath(project, w28, nil, ""), budgetMs: budget28Ms, serverBudgetMs: budgetServerMs, storedFigures: true, strict: true},
	}
	first := (max(run, 1) - 1) % len(lists)
	return slices.Concat(lists[first:], lists[:first])
}

// timelineCases are the Delivery timeline's first paint at this head (packages/dispatch/web
// DeliveryPage.tsx), the reads base mode times at the base: GET /api/v1/delivery/timeline over
// the 84-day window with the measures read of the 84-day compare range the page's panel makes
// beside it (GET /api/v1/delivery/measures), which spec R8 holds to 150 ms together, and the
// timeline over the 28-day window, held to 60 ms. The timeline reads each deploy run's failed jobs
// beside its production jobs, so it reads more the more jobs failed: prefix names the history the
// window holds, none for the history as loaded and "failure-day-" once a failure day is added to it
// (evalbench.AddFailureDay, mode failure-day).
func timelineCases(prefix string, today time.Time) []paintCase {
	w84, c84, w28 := before(today, 84), before(today.AddDate(0, 0, -84), 84), before(today, 28)
	read := func(route string, w window) string {
		return route + "?" + url.Values{"from": {w.from.Format(time.RFC3339)}, "to": {w.to.Format(time.RFC3339)}}.Encode()
	}
	group := prefix + "timeline-84d-compare"
	return []paintCase{
		{name: prefix + "timeline-84d", path: read("/api/v1/delivery/timeline", w84), group: group, groupBudgetMs: budget84Ms, delivery: true},
		{name: prefix + "measures-compare-84d", path: read("/api/v1/delivery/measures", c84), group: group, groupBudgetMs: budget84Ms, delivery: true},
		{name: prefix + "timeline-28d", path: read("/api/v1/delivery/timeline", w28), budgetMs: budget28Ms, delivery: true},
	}
}

// baseCases are the base's Delivery page's first paint, read as main's page reads it at the commit
// this head merges (packages/dispatch/web DeliveryPage.tsx): at 84 days with a compare range, the
// timeline view of the 84-day window, whose answer carries the window's measures, and a measures
// view of the 84-day compare range before it, the view that page subscribes for a range of its own
// (a brush), and at 28 days the timeline view of the 28-day window. Main's page has no compare
// range and no Pipeline section of its own, and main's pipeline view names no window (its Pipeline
// page reads the seven days to an `at`), so the base's paint is the timeline of the window and the
// measures of the compare range, which form one group as timelineCases' reads do, and the
// timeline of the 28-day window: the reads timelineCases time at this head. No budget judges
// them, the base being what the head is set beside. prefix names the history the windows hold, as
// timelineCases' does ("failure-day-" in mode base-failure-day).
func baseCases(prefix string, today time.Time) []paintCase {
	w84, c84, w28 := before(today, 84), before(today.AddDate(0, 0, -84), 84), before(today, 28)
	group := prefix + "timeline-84d-compare"
	return []paintCase{
		{name: prefix + "base-timeline-84d", kind: "timeline", view: map[string]any{"kind": "timeline", "window": w84.live(), "query": ""}, group: group, delivery: true},
		{name: prefix + "base-measures-compare-84d", kind: "measures", view: map[string]any{"kind": "measures", "window": c84.live(), "query": ""}, group: group, delivery: true},
		{name: prefix + "base-timeline-28d", kind: "timeline", view: map[string]any{"kind": "timeline", "window": w28.live(), "query": ""}, delivery: true},
	}
}

// paint measures the first paint (the package comment) and, at this head and outside -info,
// k2, k13 and k16 after it, in its first run alone where the paint is one of several (-run); in
// mode failure-day, the timeline's first paint alone, its run-jobs reads and the failure day the
// database holds, and in mode base-failure-day the base's Delivery page over the same history,
// with the failure day the database holds. Its windows end at the day of -now, where the history
// the generator wrote ends (job.sh's EVALBENCH_NOW), whatever day the clock has reached since: a
// run that crosses a midnight reads the windows of the history it loaded, and its paint line names
// both days.
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
	today := o.now
	r.result("paint mode=%s project=%s today=%s wall_clock_day=%s quiet_seconds=%.0f catalog_seconds=%.0f", o.mode, o.project,
		today.Format(time.DateOnly), time.Now().UTC().Format(time.DateOnly), o.quiet.Seconds(), o.catalog.Seconds())

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

	cases := headCases(o.project, today, o.run)
	switch o.mode {
	case "base":
		cases = baseCases("", today)
	case "base-failure-day":
		cases = baseCases("failure-day-", today)
	case "failure-day":
		cases = timelineCases("failure-day-", today)
	case "k14":
		cases = k14Cases(o.project, today, o.run)
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
	printHistories(r, o, measured)
	if o.runsFile != "" && o.mode != "k14" {
		if err := recordRun(o.runsFile, runOf(o.run, measured)); err != nil {
			r.fail("k8 record run %d in %s: %v", o.run, o.runsFile, err)
		}
	}
	if o.info {
		return nil
	}
	if o.mode == "k14" {
		return judgeK14Paint(r, o, measured)
	}
	indexes, err := failureIndexes(ctx, pool)
	if err != nil {
		return err
	}
	if o.mode == "base" {
		baseControls(r, measured, indexes)
		return nil
	}
	if o.mode == "base-failure-day" {
		return failureDayLine(ctx, r, pool, today)
	}
	checkRunJobsReads(r, o, measured, indexes)
	if o.mode == "failure-day" {
		return failureDayLine(ctx, r, pool, today)
	}
	checkFirstPaintReads(r, measured)
	checkStoredFiguresReads(r, measured)
	// k13's named-jobs read and P0 list, k16's day rows and folds, k2, the probe, k16's fallback
	// and its move of the default repository read the loaded database, which a restart of the
	// server leaves as it was (the move puts the default back), so they are measured once, after
	// the first run's paint.
	if o.run > 1 {
		return nil
	}
	checks := &headChecks{r: r, o: o, pool: pool, explain: explain, client: client, today: today}
	checks.k13()
	checks.k16()
	checks.k2()
	checks.probe(measured)
	checks.fallback(measured, catalog)
	checks.move()
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
// cost: its statements, told apart from the server's background by the catalogue and by the
// backends the statement log shows them running on (ownAndBackground).
func measure(ctx context.Context, pool *store.Pool, client *http.Client, o options, one paintCase, catalog map[int64]bool) (measuredCase, error) {
	got := measuredCase{paintCase: one, serverMs: -1}
	serverFrom, err := fileSize(o.serverLog)
	if err != nil {
		return got, fmt.Errorf("read the server log %s: %w", o.serverLog, err)
	}
	logFrom, err := fileSize(o.pgLog)
	if err != nil {
		return got, fmt.Errorf("read the statement log %s: %w", o.pgLog, err)
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
	snapshot := time.Now()
	executions, err := readStatementLog(o.pgLog, logFrom)
	if err != nil {
		return got, err
	}
	got.own, got.background, got.cataloguedOwn = ownAndBackground(got.all, catalog, executions, got.t0.Add(-100*time.Millisecond), got.t1, snapshot)
	got.serverMs, err = serverTime(o.serverLog, serverFrom, one)
	return got, err
}

// ownAndBackground splits all, every statement pg_stat_statements counted from the reset before a
// read to the snapshot after it, into the read's own and the server's background, and answers how
// many statements of the catalogue the read ran itself. A statement that writes is background
// (writes), and a statement the catalogue does not hold is the read's own. A statement the
// catalogue holds is the read's own as far as the read ran it: the read's backends are those the
// statement log (executions) shows running one of its own statements between start and end, the
// read's span, and of the catalogued statement's executions the log shows between start and
// snapshot, those on the read's backends are the read's and the rest a background loop's, which
// runs on a connection of its own while the read holds its own. Its calls and its time are split
// between the two sides by that count; one the log shows on no backend of the read is background
// whole. So a statement a background loop also runs, which a read once lost from its time whole
// because the catalogue holds its query id, counts for the read where the read ran it.
func ownAndBackground(all []statement, catalog map[int64]bool, executions []logged, start, end, snapshot time.Time) (own, background []statement, cataloguedOwn int) {
	ownIDs, readPIDs := map[int64]bool{}, map[int64]bool{}
	for _, one := range all {
		if !catalog[one.queryID] && !writes(one.query) {
			ownIDs[one.queryID] = true
		}
	}
	for _, execution := range executions {
		if ownIDs[execution.queryID] && !execution.at.Before(start) && !execution.at.After(end) {
			readPIDs[execution.pid] = true
		}
	}
	for _, one := range all {
		switch {
		case writes(one.query):
			background = append(background, one)
		case !catalog[one.queryID]:
			own = append(own, one)
		default:
			ours, theirs := 0, 0
			for _, execution := range executions {
				if execution.queryID != one.queryID || execution.at.Before(start) || execution.at.After(snapshot) {
					continue
				}
				if readPIDs[execution.pid] {
					ours++
				} else {
					theirs++
				}
			}
			switch {
			case ours == 0:
				background = append(background, one)
			case theirs == 0:
				own = append(own, one)
				cataloguedOwn++
			default:
				share := float64(ours) / float64(ours+theirs)
				own = append(own, scaled(one, share))
				background = append(background, scaled(one, 1-share))
				cataloguedOwn++
			}
		}
	}
	return own, background, cataloguedOwn
}

// scaled is one with its calls, its time, its rows and its buffers scaled by share, the part of its
// executions one side of a read made (ownAndBackground).
func scaled(one statement, share float64) statement {
	part := func(n int64) int64 { return int64(math.Round(float64(n) * share)) }
	one.calls, one.rows, one.hit, one.read = part(one.calls), part(one.rows), part(one.hit), part(one.read)
	one.planMs, one.execMs = one.planMs*share, one.execMs*share
	return one
}

// writeStatement matches a statement that writes: an insert, update, delete or merge, at its head or
// in a data-modifying `with`. A GET of the eval or delivery routes writes nothing, so such a
// statement in a read's window is a background writer's that the catalogue's window did not catch,
// such as a Legion facts pass whose wake fell inside the read (its `insert into issue_facts` once
// counted 267 ms against a 28-day read), and it is reported among the background statements.
var writeStatement = regexp.MustCompile(`(?is)^\s*(insert|update|delete|merge)\b|\b(insert\s+into|update\s+\w|delete\s+from)\b`)

// writes reports whether query writes (writeStatement).
func writes(query string) bool {
	return writeStatement.MatchString(query)
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
// (outside -info): a read of the evals' first paint by its body too, and a read of the Delivery
// page by its database time alone, with a line saying its body was measured outside the budget.
// The database-time budgets are k8's at the stated run rate, so at ten times that rate a read over
// one is a RECORD line (report.overStated); the server time and the body are the first paint's
// limit, which the root's R14 holds at ten times too, so they fail at either rate, as its answer,
// its own statements and a paint with no plan examined do.
func judgePaint(r *report, o options, measured []measuredCase) {
	total, plans := 0.0, 0
	for _, one := range measured {
		own := dbMs(one.own)
		total += own
		plans += len(one.plans)
		gz := gzipSize(one.body)
		r.result("paint case=%s status=%d db_ms=%.2f plan_ms=%.2f statements=%d calls=%d catalogued_own_statements=%d background_statements=%d background_db_ms=%.2f db_ms_everything_in_flight=%.2f server_ms=%.0f client_ms=%.1f body_bytes=%d gzip_bytes=%d plans=%d",
			one.name, one.status, own, planMs(one.own), len(one.own), calls(one.own), one.cataloguedOwn, len(one.background), dbMs(one.background), dbMs(one.all),
			one.serverMs, float64(one.t1.Sub(one.t0).Microseconds())/1000, len(one.body), gz, len(one.plans))
		for _, statement := range one.own {
			r.result("statement case=%s queryid=%d calls=%d plan_ms=%.3f exec_ms=%.3f rows=%d shared_hit=%d shared_read=%d", one.name, statement.queryID,
				statement.calls, statement.planMs, statement.execMs, statement.rows, statement.hit, statement.read)
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
			r.result("background case=%s queryid=%d calls=%d plan_ms=%.3f exec_ms=%.3f %s", one.name, statement.queryID, statement.calls, statement.planMs, statement.execMs, oneLine(statement.query))
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
			if one.strict {
				r.fail("paint case=%s db_ms=%.2f is over its budget of %.0f ms, which binds at both rates", one.name, own, one.budgetMs)
			} else {
				r.overStated("paint case=%s db_ms=%.2f is over its budget of %.0f ms", one.name, own, one.budgetMs)
			}
		}
		switch {
		case one.delivery:
			r.result("paint-body case=%s gzip_bytes=%d budget=none: a read of the Delivery page, whose body is measured and judged by no "+
				"budget, since the %d-byte budget of R8 bounds the evals' first paint, which the root's R3 keeps the row-level merge timeline out of",
				one.name, gz, budgetGzip)
		case gz > budgetGzip:
			r.fail("paint case=%s gzip_bytes=%d is over its budget of %d", one.name, gz, budgetGzip)
		}
		if one.serverBudgetMs > 0 && (one.serverMs < 0 || one.serverMs > one.serverBudgetMs) {
			r.fail("paint case=%s server_ms=%.0f is over its budget of %.0f ms, or the server logged none", one.name, one.serverMs, one.serverBudgetMs)
		}
	}
	judgeGroups(r, o, measured)
	r.result("paint-total mode=%s reads=%d db_ms=%.2f plans=%d", o.mode, len(measured), total, plans)
	if !o.info && plans == 0 {
		r.fail("paint: no plan examined")
	}
}

// judgeGroups prints each group of reads (paintCase.group) with their database time together, in
// the order the paint met them, and judges it against the group's budget (outside -info), which
// k8 states at the stated run rate (report.overStated).
func judgeGroups(r *report, o options, measured []measuredCase) {
	var order []string
	reads, ms, budgets := map[string][]string{}, map[string]float64{}, map[string]float64{}
	for _, one := range measured {
		if one.group == "" {
			continue
		}
		if _, seen := reads[one.group]; !seen {
			order = append(order, one.group)
		}
		reads[one.group] = append(reads[one.group], one.name)
		ms[one.group] += dbMs(one.own)
		budgets[one.group] = one.groupBudgetMs
	}
	for _, group := range order {
		r.result("paint-group mode=%s group=%s reads=%s db_ms=%.2f budget_ms=%.0f", o.mode, group, strings.Join(reads[group], ","), ms[group], budgets[group])
		if !o.info && budgets[group] > 0 && ms[group] > budgets[group] {
			r.overStated("paint-group group=%s db_ms=%.2f is over its budget of %.0f ms", group, ms[group], budgets[group])
		}
	}
}

// actorWord matches a statement that names an event's actor.
var actorWord = regexp.MustCompile(`(?i)\bactor\b`)

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
	return true, !actorWord.MatchString(text)
}

// populationTables is the tables that hold a project's population, the items its day rows are
// folded from: its issues and their events, its pull requests, deploy runs and their jobs, its
// readings, and the Legion facts, filings, observations, handoffs and session titles its Legion
// evals read. A first paint of the evals reads the stored figures and the day rows they fold, and
// reads none of these but by the two narrow reads checkFirstPaintReads lets pass.
var populationTables = []string{"issues", "events", "delivery_pull_requests", "delivery_runs", "delivery_run_jobs", "eval_readings",
	"issue_facts", "issue_filings", "legion_observations", "handoffs", "session_titles"}

// openP0Scans are the two scans of issues a read of the evals' first paint may make: through
// issues_open_p0, the partial index of the open P0 issues alone, which the list the unowned-p0
// rows are written from reads (k13's P0 list holds it to that index) and the live hub reads to
// keep that eval current. It holds a few hundred issues of every project, not one project's
// population.
var openP0Scans = []string{"Index Scan issues_open_p0", "Index Only Scan issues_open_p0"}

// checkFirstPaintReads is k8's population clause ("no read of the page reads a project's
// population, which each plan shows") and k13's first clause: no statement of this head's first
// paint of the evals scans a table of the population (populationTables), but for a read of events
// by its id alone, which is listed for information, where any other read of events, or one of an
// event's actor, resolves a session title out of events (eventsRead), and a read of issues through
// issues_open_p0 (openP0Scans). Every other scan of issues fails, an index scan as well as a
// sequential one, as does any scan of the other tables. The Delivery timeline's reads
// (paintCase.delivery) are k13's run-jobs rule's (checkRunJobsReads), and not this clause's.
func checkFirstPaintReads(r *report, measured []measuredCase) {
	examined, titleReads, idReads, populationReads := 0, 0, 0, 0
	for _, one := range measured {
		if one.delivery {
			continue
		}
		for queryID, plan := range one.plans {
			examined++
			text := one.statements[queryID]
			scans := relations(plan)
			for _, table := range populationTables {
				switch table {
				case "events":
					switch reads, byID := eventsRead(text, plan); {
					case reads && byID:
						idReads++
						r.result("k13-events-by-id case=%s queryid=%d scans=%q: %s", one.name, queryID, scans["events"], oneLine(text))
					case reads:
						titleReads++
						r.fail("k13 case=%s queryid=%d reads events other than by its id, or an event's actor: %s", one.name, queryID, oneLine(text))
					}
				case "issues":
					for _, scan := range scans[table] {
						if !slices.Contains(openP0Scans, scan) {
							populationReads++
							r.fail("k8 case=%s queryid=%d reads the population: it scans issues by %q, not through issues_open_p0 alone: %s", one.name, queryID, scan, oneLine(text))
						}
					}
				default:
					if len(scans[table]) > 0 {
						populationReads++
						r.fail("k8 case=%s queryid=%d reads the population: it scans %s (%q): %s", one.name, queryID, table, scans[table], oneLine(text))
					}
				}
			}
		}
	}
	r.result("k13 first_paint_statements_examined=%d title_reads_of_events=%d reads_of_events_by_id=%d population_reads=%d", examined, titleReads, idReads, populationReads)
	if examined == 0 {
		r.fail("k13: no statement of the first paint examined")
	}
}

// checkStoredFiguresReads is the rule every read of an offered range of the eval list
// (paintCase.storedFigures) is held to in every run: it runs both stored-figures reads of
// evals.ReadStoredFiguresIn (storedFiguresReads), the range-figures read and the day-figures read,
// the reads with a repository as well as those with none, so a read that stopped reading one of
// the two, or both, fails by name.
func checkStoredFiguresReads(r *report, measured []measuredCase) {
	for _, one := range measured {
		if !one.storedFigures {
			continue
		}
		found, err := storedFiguresReads(one)
		if err != nil {
			r.fail("k16 stored-figures case=%s: %v", one.name, err)
			continue
		}
		ids := make([]string, len(found))
		for index, statement := range found {
			ids[index] = fmt.Sprintf("%s=%d", statement.name, statement.queryID)
		}
		r.result("k16 stored-figures case=%s reads=[%s]", one.name, strings.Join(ids, " "))
	}
}

// titleRead matches a statement that reads an event's actor out of events: a session title
// resolved out of the event log.
var titleRead = regexp.MustCompile(`(?is)\bactor\b.*\bfrom events\b`)

// baseControls is k8's negative control and k13's at the base: the base's first paint's database
// time against this head's budget, its statements that read events other than by id (where the
// base resolves session titles), and its run-jobs reads that name no job and read through no
// index of the failed jobs alone (runJobsScans, the rule checkRunJobsReads holds this head to). A
// control that examined nothing shows nothing of the base, so each fails then: k8's with no read
// measured, or with a page that shows no history (no pull request attributed to an issue on the
// timeline, or none in the measures: historyOf), k13's with no statement examined or none of them
// a read of delivery_run_jobs. k8's control fails too where the base's first paint of 84 days with
// a compare range (the reads of baseCases' group, not its 28-day read) costs no more database time
// than this head's 84-day budget: k8's negative control is that the base passes it (564 ms at the
// commit before the change), and a base within the budget shows no read the change brought under
// it. That control is k8's at the stated run rate: at ten times the rate, where the database time is
// judged by setting the head's reads beside the base's, it is a RECORD line (report.overStated).
//
// Where a background loop of the base resolves session titles with the same statement its
// timeline does, that statement is in the background catalogue, and each read's own time counts
// it only where the read ran it (ownAndBackground). The control therefore also names every
// catalogued statement left in the background that reads an event's actor out of events, and
// states the paint's time with everything in flight beside its own.
func baseControls(r *report, measured []measuredCase, failed map[string]bool) {
	attributed, measuresPRs := 0, 0
	for _, one := range measured {
		if history, read := historyOf(one.body); read {
			attributed += history.attributed
			measuresPRs += history.measuresPRs
		}
	}
	if attributed == 0 || measuresPRs == 0 {
		r.fail("k8-control: the base's first paint shows %d pull requests attributed to an issue on its timeline and %d in its measures, so it measured no Delivery history", attributed, measuresPRs)
	}
	total, inFlight := 0.0, 0.0
	examined, readsEvents, catalogued, unnamed, jobReads := 0, 0, 0, 0, 0
	for _, one := range measured {
		if one.group != "" {
			total += dbMs(one.own)
			inFlight += dbMs(one.all)
		}
		for queryID, plan := range one.plans {
			examined++
			if reads, byID := eventsRead(one.statements[queryID], plan); reads && !byID {
				readsEvents++
				r.result("k13-control case=%s queryid=%d reads events other than by its id: %s", one.name, queryID, oneLine(one.statements[queryID]))
			}
			if scans := runJobsScans(plan, failed); len(scans) > 0 {
				jobReads++
				if !everyScanKeyed(scans) {
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
	r.result("k8-control base_first_paint_db_ms=%.2f db_ms_everything_in_flight=%.2f this_heads_budget_ms=%d exceeds=%t reads_measured=%d", total, inFlight, budget84Ms, total > budget84Ms, len(measured))
	r.result("k13-control statements_examined=%d statements_reading_events_other_than_by_id=%d catalogued_title_reads=%d run_jobs_reads=%d run_jobs_reads_without_a_name=%d", examined, readsEvents, catalogued, jobReads, unnamed)
	switch {
	case len(measured) == 0:
		r.fail("k8-control: no read of the base's first paint was measured")
	case total <= budget84Ms:
		r.overStated("k8-control: the base's first paint costs %.2f ms of database time, within this head's budget of %d ms, so the negative control shows no read the change brought under the budget", total, budget84Ms)
	}
	switch {
	case examined == 0:
		r.fail("k13-control: no statement of the base's first paint was examined")
	case jobReads == 0:
		r.fail("k13-control: no statement of the base's first paint reads delivery_run_jobs, so the run-jobs control examined nothing")
	}
}

// deliveryHistory is what one Delivery read's answer shows of the history: a timeline's pull
// requests, those attributed to an issue and those by the issue's project (the part of its key
// before the number), and the measures' change_failure_rate.per_pr.total, successful deploys and
// deploys that shipped a pull request. The base counts every population pull request merged in the
// window in per_pr.total; this head builds change-fail-rate only where a failure source exists
// (measures.Compute), so with none it answers 0 there, and the timeline's prs are the pull
// requests to set beside the base's.
type deliveryHistory struct {
	timeline, measures                   bool
	prs, attributed                      int
	byProject                            map[string]int
	measuresPRs, deploys, deploysWithPRs int
}

// deliveryMeasures is the part of the measures historyOf reads.
type deliveryMeasures struct {
	DeployFrequency struct {
		SuccessfulDeploys int `json:"successful_deploys"`
		DeploysWithPRs    int `json:"deploys_with_prs"`
	} `json:"deploy_frequency"`
	ChangeFailureRate struct {
		PerPR struct {
			Total int `json:"total"`
		} `json:"per_pr"`
	} `json:"change_failure_rate"`
}

// historyOf reads the history a Delivery read's answer shows, at this head and at the base, whose
// answers have one shape: a timeline's answer (GET /api/v1/delivery/timeline, a timeline view)
// holds its pull requests at prs and carries its window's measures answer whole at measures, and a
// measures answer (GET /api/v1/delivery/measures, a measures view) holds the measures at measures.
// read is false for a body that is neither.
func historyOf(body []byte) (history deliveryHistory, read bool) {
	var timeline struct {
		PRs *[]struct {
			Issue *string `json:"issue"`
		} `json:"prs"`
		Measures *struct {
			Measures *deliveryMeasures `json:"measures"`
		} `json:"measures"`
	}
	var answer struct {
		Measures *deliveryMeasures `json:"measures"`
	}
	if json.Unmarshal(body, &timeline) != nil || json.Unmarshal(body, &answer) != nil {
		return deliveryHistory{}, false
	}
	measures := answer.Measures
	if timeline.PRs != nil {
		history.timeline, history.prs, history.byProject = true, len(*timeline.PRs), map[string]int{}
		for _, pr := range *timeline.PRs {
			if pr.Issue == nil {
				continue
			}
			history.attributed++
			project := *pr.Issue
			if at := strings.LastIndex(project, "-"); at > 0 {
				project = project[:at]
			}
			history.byProject[project]++
		}
		measures = nil
		if timeline.Measures != nil {
			measures = timeline.Measures.Measures
		}
	}
	if measures != nil {
		history.measures = true
		history.measuresPRs = measures.ChangeFailureRate.PerPR.Total
		history.deploys, history.deploysWithPRs = measures.DeployFrequency.SuccessfulDeploys, measures.DeployFrequency.DeploysWithPRs
	}
	return history, history.timeline || history.measures
}

// printHistories prints, for each Delivery read of the paint, the history its answer shows
// (historyOf), so a run at this head and one at the base can be set side by side: one history
// loaded into each shows the same pull requests, attributed to the same project.
func printHistories(r *report, o options, measured []measuredCase) {
	for _, one := range measured {
		if !one.delivery {
			continue
		}
		history, read := historyOf(one.body)
		if !read {
			r.result("delivery-history mode=%s case=%s status=%d: the answer shows no pull request and no measures", o.mode, one.name, one.status)
			continue
		}
		line := fmt.Sprintf("delivery-history mode=%s case=%s", o.mode, one.name)
		if history.timeline {
			projects := make([]string, 0, len(history.byProject))
			for _, project := range slices.Sorted(maps.Keys(history.byProject)) {
				projects = append(projects, fmt.Sprintf("%s=%d", project, history.byProject[project]))
			}
			line += fmt.Sprintf(" timeline_prs=%d attributed=%d attributed_by_project=[%s]", history.prs, history.attributed, strings.Join(projects, " "))
		}
		if history.measures {
			line += fmt.Sprintf(" measures_per_pr_total=%d successful_deploys=%d deploys_with_prs=%d", history.measuresPRs, history.deploys, history.deploysWithPRs)
		}
		r.result("%s", line)
	}
}

// checkRunJobsReads is k13's run-jobs rule at this head, over every read of its first paint, the
// Delivery timeline's above all, and the rule k13's control holds the base to: each scan of
// delivery_run_jobs reads through an index its index condition keys by the job's name, or through
// one that holds the failed jobs alone, by its condition's `conclusion = 'failure'` or by the
// index's own predicate, which Postgres then plans no condition for (runJobsScans). So no read of
// the page reads a run's every job, and the timeline's read of the failed ones is in proportion to
// the failures. A sequential scan of the table is keyed by nothing, whatever its statement names:
// at a small -scale the planner can read the small table whole for a read that names its jobs,
// so the rule can fail there on such a read, and the run at INFRA's size is the one that judges
// it. A paint none of whose timeline reads reads delivery_run_jobs examined nothing of the
// timeline, so it fails then.
func checkRunJobsReads(r *report, o options, measured []measuredCase, failed map[string]bool) {
	examined, jobReads, unkeyed, timelineReads := 0, 0, 0, 0
	for _, one := range measured {
		for queryID, plan := range one.plans {
			examined++
			scans := runJobsScans(plan, failed)
			if len(scans) == 0 {
				continue
			}
			jobReads++
			if one.delivery {
				timelineReads++
			}
			keys := make([]string, len(scans))
			for i, scan := range scans {
				keys[i] = scan.node + " by " + strings.Join(scan.keys, "+")
			}
			if !everyScanKeyed(scans) {
				unkeyed++
				r.fail("k13-control case=%s queryid=%d reads delivery_run_jobs with no job name in its index condition, and not through the failed jobs alone (scans %q): %s", one.name, queryID, keys, oneLine(one.statements[queryID]))
				continue
			}
			r.result("k13-control case=%s queryid=%d reads delivery_run_jobs through %q: %s", one.name, queryID, keys, oneLine(one.statements[queryID]))
		}
	}
	r.result("k13-control mode=%s failed_jobs_indexes=%q statements_examined=%d run_jobs_reads=%d run_jobs_reads_without_a_name=%d timeline_run_jobs_reads=%d",
		o.mode, slices.Sorted(maps.Keys(failed)), examined, jobReads, unkeyed, timelineReads)
	if timelineReads == 0 {
		r.fail("k13-control: no read of the timeline reads delivery_run_jobs, so the run-jobs rule examined nothing of it")
	}
}

// failureDayLine prints the failure day the database holds (evalbench.AddFailureDay): the day, its
// runs and their failed jobs, the day's deploy runs on main with the failed ones and their failed
// jobs, the history's own included, and whether the timeline's windows hold the day. A failure-day
// paint over a database with no failure day, or one outside the 84-day window, measured none, so
// it fails then.
func failureDayLine(ctx context.Context, r *report, pool *store.Pool, today time.Time) error {
	var first *time.Time
	var runs, failedJobs int64
	if err := pool.QueryRow(ctx, `
		select min(coalesce(r.created_at, r.started_at)), count(distinct r.run_id), count(j.run_id)
		from delivery_runs r
		left join delivery_run_jobs j on j.repo = r.repo and j.run_id = r.run_id and j.conclusion = 'failure'
		where r.repo = $1 and r.run_id >= $2
	`, evalbench.Repos[0], evalbench.FailureDayFirstRunID).Scan(&first, &runs, &failedJobs); err != nil {
		return fmt.Errorf("read the failure day: %w", err)
	}
	if first == nil {
		r.fail("paint-failure-day: the database holds no failure day, so the failure-day paint measured the history as loaded")
		return nil
	}
	day := first.UTC().Truncate(24 * time.Hour)
	var dayRuns, dayFailedRuns, dayFailedJobs int64
	if err := pool.QueryRow(ctx, `
		select count(distinct r.run_id), count(distinct r.run_id) filter (where r.conclusion = 'failure'), count(j.run_id)
		from delivery_runs r
		left join delivery_run_jobs j on j.repo = r.repo and j.run_id = r.run_id and j.conclusion = 'failure'
		where r.repo = $1 and r.kind = 'deploy' and r.head_branch = 'main'
		  and coalesce(r.created_at, r.started_at) >= $2 and coalesce(r.created_at, r.started_at) < $3
	`, evalbench.Repos[0], day, day.AddDate(0, 0, 1)).Scan(&dayRuns, &dayFailedRuns, &dayFailedJobs); err != nil {
		return fmt.Errorf("read the failure day's deploy runs: %w", err)
	}
	in84, in28 := holds(before(today, 84), day), holds(before(today, 28), day)
	r.result("paint-failure-day day=%s added_runs=%d added_failed_jobs=%d day_deploy_runs=%d day_failed_runs=%d day_failed_jobs=%d in_84d_window=%t in_28d_window=%t",
		day.Format(time.DateOnly), runs, failedJobs, dayRuns, dayFailedRuns, dayFailedJobs, in84, in28)
	if !in84 {
		r.fail("paint-failure-day: the failure day %s is outside the 84-day window %s, so the timeline's reads of it measured none", day.Format(time.DateOnly), before(today, 84))
	}
	return nil
}

// holds reports whether w holds the day starting at day.
func holds(w window, day time.Time) bool {
	return !day.Before(w.from) && day.Before(w.to)
}
