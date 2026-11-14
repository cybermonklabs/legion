package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/evals/evalbench"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// headChecks is this head's checks after the first paint: k13's named-jobs read and P0 count,
// k16's day rows by repository and hand folds, and k2's statements, index list, refusals and
// controls.
type headChecks struct {
	r       *report
	o       options
	pool    *store.Pool
	explain *explainer
	client  *http.Client
	today   time.Time
}

// captured runs fn with pg_stat_statements reset before it and answers the plan of each statement
// it ran whose text holds marker, explained with the parameters the statement log holds for it.
func (c *headChecks) captured(ctx context.Context, name, marker string, fn func() error) []string {
	from, err := fileSize(c.o.pgLog)
	if err != nil {
		c.r.fail("%s: read the statement log: %v", name, err)
		return nil
	}
	if err := resetStatements(ctx, c.pool); err != nil {
		c.r.fail("%s: %v", name, err)
		return nil
	}
	started := time.Now()
	if err := fn(); err != nil {
		c.r.fail("%s: %v", name, err)
		return nil
	}
	time.Sleep(time.Second)
	ran, err := snapshotStatements(ctx, c.pool)
	if err != nil {
		c.r.fail("%s: %v", name, err)
		return nil
	}
	executions, err := readStatementLog(c.o.pgLog, from)
	if err != nil {
		c.r.fail("%s: %v", name, err)
		return nil
	}
	var plans []string
	for _, statement := range ran {
		if !strings.Contains(statement.query, marker) {
			continue
		}
		for _, execution := range executions {
			if execution.queryID != statement.queryID || execution.at.Before(started.Add(-100*time.Millisecond)) {
				continue
			}
			plan, _, err := c.explain.explain(ctx, execution.text, execution.params)
			if err != nil {
				c.r.fail("%s queryid=%d: explain failed: %v", name, statement.queryID, err)
				break
			}
			c.r.result("%s queryid=%d calls=%d exec_ms=%.3f", name, statement.queryID, statement.calls, statement.execMs)
			c.r.block(fmt.Sprintf("SQL %s queryid=%d", name, statement.queryID), execution.text)
			c.r.block(fmt.Sprintf("PLAN %s queryid=%d", name, statement.queryID), plan)
			plans = append(plans, plan)
			break
		}
	}
	if len(plans) == 0 {
		c.r.fail("%s: no statement holding %q was captured", name, marker)
	}
	return plans
}

// k13 reads the named-jobs statement through delivery.ListRunJobsForRuns over the deploy runs of
// the 84-day window and the production jobs the settings judge a deploy by, and the count of P0
// issues with no owner through delivery.ListUnownedP0: the first must carry the job names in its
// index condition, the second read issues_open_p0 and scan no table of issues.
func (c *headChecks) k13() {
	ctx := context.Background()
	settings, err := delivery.GetSettings(ctx, c.pool)
	if err != nil {
		c.r.fail("k13: read the delivery settings: %v", err)
		return
	}
	w84 := before(c.today, 84)
	rows, err := c.pool.Query(ctx, `select run_id from delivery_runs where repo = $1 and kind = 'deploy' and started_at >= $2 and started_at < $3 order by run_id`,
		settings.DeployRepo, w84.from, w84.to)
	if err != nil {
		c.r.fail("k13: read the window's deploy runs: %v", err)
		return
	}
	runIDs, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		c.r.fail("k13: read the window's deploy runs: %v", err)
		return
	}
	names := settings.Production().JudgedJobNames()
	var jobs int
	for _, plan := range c.captured(ctx, "k13-named-jobs", "from delivery_run_jobs where repo = $1 and run_id = any($2) and name = any(", func() error {
		byRun, err := delivery.ListRunJobsForRuns(ctx, c.pool, settings.DeployRepo, runIDs, names)
		for _, listed := range byRun {
			jobs += len(listed)
		}
		return err
	}) {
		named := regexp.MustCompile(`Index Cond: .*\bname = ANY`).MatchString(plan)
		c.r.result("k13 named_jobs runs=%d names=%q jobs=%d name_in_index_condition=%t", len(runIDs), names, jobs, named)
		if !named {
			c.r.fail("k13 the named-jobs read carries no job name in its index condition")
		}
	}
	var unowned int
	for _, plan := range c.captured(ctx, "k13-unowned-p0", "claimed_by is null and route is null", func() error {
		issues, err := delivery.ListUnownedP0(ctx, c.pool, c.o.project)
		unowned = len(issues)
		return err
	}) {
		index, scans := usesIndex(plan, "issues_open_p0"), seqScanned(plan, "issues")
		c.r.result("k13 unowned_p0 project=%s count=%d uses_issues_open_p0=%t seq_scan_of_issues=%t", c.o.project, unowned, index, scans)
		if !index || scans {
			c.r.fail("k13 the unowned-P0 count reads issues otherwise than through issues_open_p0")
		}
	}
}

// k16 prints the project's day rows by repository, and for one count, one share and one quantile
// folds each repository's day rows of the 84-day window by hand, against the list read with that
// repository, and the repositories' parts against the project's.
func (c *headChecks) k16() {
	ctx := context.Background()
	rows, err := c.pool.Query(ctx, `select repo, count(*) from eval_days where project_key = $1 group by repo order by repo`, c.o.project)
	if err != nil {
		c.r.fail("k16: count the day rows by repository: %v", err)
		return
	}
	var repo string
	var count int64
	repos := 0
	if _, err := pgx.ForEachRow(rows, []any{&repo, &count}, func() error {
		repos++
		c.r.result("k16 day_rows project=%s repo=%q rows=%d", c.o.project, repo, count)
		return nil
	}); err != nil {
		c.r.fail("k16: count the day rows by repository: %v", err)
		return
	}
	if repos < 2 {
		c.r.fail("k16: the day rows of %s hold %d repositories, so no repository is a dimension of them", c.o.project, repos)
	}
	w84 := before(c.today, 84)
	folds := []struct {
		slug, series, stat string
		hand               string
	}{
		{evals.SlugPROpenToMerge, measures.SeriesMinutes, "n", `select coalesce(sum(cardinality(samples)), 0)::float8 from eval_days`},
		{evals.SlugReworkShare, measures.SeriesPRs, "rate", `select sum(num) / nullif(sum(den), 0) from eval_days`},
		{evals.SlugPROpenToMerge, measures.SeriesMinutes, "median", `select percentile_cont(0.5) within group (order by sample) from eval_days, unnest(samples) as sample`},
	}
	compared := 0
	for _, fold := range folds {
		project, _ := c.routeValue(ctx, w84, "", fold.slug, fold.series, fold.stat)
		for _, repo := range append([]string{""}, evalbench.Repos...) {
			route, err := c.routeValue(ctx, w84, repo, fold.slug, fold.series, fold.stat)
			if err != nil {
				c.r.fail("k16 %s.%s.%s repo=%q: %v", fold.slug, fold.series, fold.stat, repo, err)
				continue
			}
			hand, err := c.handFold(ctx, fold.hand+` where project_key = $1 and eval_id = $2 and series = $3 and repo = $4 and day >= $5 and day < $6`,
				evals.EvalID(c.o.project, fold.slug), fold.series, repo, w84)
			if err != nil {
				c.r.fail("k16 %s.%s.%s repo=%q: %v", fold.slug, fold.series, fold.stat, repo, err)
				continue
			}
			compared++
			equal := same(route, hand)
			c.r.result("k16 fold eval=%s series=%s stat=%s repo=%q window=%s route=%s hand=%s equal=%t", fold.slug, fold.series, fold.stat, repo, w84, show(route), show(hand), equal)
			if !equal {
				c.r.fail("k16 %s.%s.%s repo=%q: the route answers %s and the hand fold %s", fold.slug, fold.series, fold.stat, repo, show(route), show(hand))
			}
		}
		// The repositories partition the project's rows: a count sums, and a share or a quantile
		// is the fold of the union of their parts.
		union, err := c.handFold(ctx, fold.hand+` where project_key = $1 and eval_id = $2 and series = $3 and repo <> $4 and day >= $5 and day < $6`,
			evals.EvalID(c.o.project, fold.slug), fold.series, "", w84)
		if err != nil {
			c.r.fail("k16 %s.%s.%s union: %v", fold.slug, fold.series, fold.stat, err)
			continue
		}
		equal := same(project, union)
		c.r.result("k16 partition eval=%s series=%s stat=%s project=%s fold_of_the_repositories=%s equal=%t", fold.slug, fold.series, fold.stat, show(project), show(union), equal)
		if !equal {
			c.r.fail("k16 %s.%s.%s: the project answers %s and the fold of its repositories' rows %s", fold.slug, fold.series, fold.stat, show(project), show(union))
		}
	}
	c.r.result("k16 folds_compared=%d", compared)
	if compared == 0 {
		c.r.fail("k16: no fold compared")
	}
}

// routeValue is one statistic of one series of one eval as the list read answers it over w, the
// rows of repo where it names one.
func (c *headChecks) routeValue(ctx context.Context, w window, repo, slug, series, stat string) (*float64, error) {
	query := url.Values{"from": {w.from.Format(time.RFC3339)}, "to": {w.to.Format(time.RFC3339)}, "limit": {"200"}}
	if repo != "" {
		query.Set("repo", repo)
	}
	status, body, err := getBody(ctx, c.client, c.o.url+"/api/v1/projects/"+c.o.project+"/evals?"+query.Encode(), c.o.token)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("the list read answered %d: %s", status, body)
	}
	var answer struct {
		Items []struct {
			ID     string `json:"id"`
			Series []struct {
				Series string              `json:"series"`
				Window map[string]*float64 `json:"window"`
			} `json:"series"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return nil, err
	}
	for _, item := range answer.Items {
		if item.ID != evals.EvalID(c.o.project, slug) {
			continue
		}
		for _, one := range item.Series {
			if one.Series == series {
				value, held := one.Window[stat]
				if !held {
					return nil, fmt.Errorf("series %s answers no statistic %s", series, stat)
				}
				return value, nil
			}
		}
	}
	return nil, fmt.Errorf("the list read answers no series %s of %s", series, slug)
}

// handFold runs one fold over eval_days, by hand.
func (c *headChecks) handFold(ctx context.Context, query, evalID, series, repo string, w window) (*float64, error) {
	var value *float64
	if err := c.pool.QueryRow(ctx, query, c.o.project, evalID, series, repo, w.from, w.to).Scan(&value); err != nil {
		return nil, err
	}
	return value, nil
}

// same reports whether two folds agree, to the last few bits a float sum can differ by.
func same(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return math.Abs(*a-*b) <= 1e-9*math.Max(1, math.Abs(*b))
}

// show is a fold's value, or null.
func show(value *float64) string {
	if value == nil {
		return "null"
	}
	return fmt.Sprintf("%.10g", *value)
}

// k2 runs the six statements of k2 over the readings of one logged eval and one row kind in a
// 28-day window (the plan's "The checks"), the index list, the two refusals and the two controls.
func (c *headChecks) k2() {
	ctx := context.Background()
	evalID := evals.EvalID(c.o.project, evalbench.LoggedSlugs()[0])
	rowKind := "issue"
	w28, c28 := before(c.today, 28), before(c.today.AddDate(0, 0, -28), 28)
	args := func(w window) []string {
		return []string{quote(evalID), quote(w.from.Format(time.RFC3339)), quote(w.to.Format(time.RFC3339)), quote(rowKind)}
	}
	windowRead := `select ` + evals.ReadingColumns + ` from eval_readings
		where eval_id = $1 and superseded_at is null and measured_at >= $2 and measured_at < $3 and row_kind = $4
		order by measured_at desc, id`
	compareRead := `(` + windowRead + `) union all (` + strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(windowRead,
		"$4", "$8"), "$3", "$7"), "$2", "$6"), "$1", "$5") + `)`
	antiJoin := `select ` + evals.ReadingColumns + ` from eval_readings r
		where r.eval_id = $1 and r.measured_at >= $2 and r.measured_at < $3 and r.row_kind = $4
		  and not exists (select 1 from eval_readings s where s.replaces = r.id)
		order by r.measured_at desc, r.id`
	type k2Statement struct {
		name, text string
		params     []string
		drop       string
	}
	statements := []k2Statement{
		{"1-registry", `select * from evals_current where project_key = $1 and state = 'active'`, []string{quote(c.o.project)}, ""},
		{"2-window", windowRead, args(w28), ""},
		{"3-items", windowRead + ` limit $5 offset $6`, append(args(w28), "200", "0"), ""},
		{"4-compare-union-all", compareRead, append(args(w28), args(c28)...), ""},
		{"5-window-replaces-dropped", windowRead, args(w28), "eval_readings_replaces"},
		{"6-anti-join-replaces-dropped", antiJoin, args(w28), "eval_readings_replaces"},
	}
	plans := map[string]string{}
	for _, statement := range statements {
		var times []string
		for run := 1; run <= 3; run++ {
			plan, ms, err := c.explainDropped(ctx, statement.text, statement.params, statement.drop)
			if err != nil {
				c.r.fail("k2 %s: %v", statement.name, err)
				break
			}
			times = append(times, fmt.Sprintf("%.2f", ms))
			if run == 1 {
				plans[statement.name] = plan
				c.r.block(fmt.Sprintf("SQL k2 %s dropped=%q params=%s", statement.name, statement.drop, strings.Join(statement.params, ", ")), statement.text)
				c.r.block("PLAN k2 "+statement.name, plan)
			}
		}
		c.r.result("k2 statement=%s eval=%s row_kind=%s window=%s execution_ms=[%s]", statement.name, evalID, rowKind, w28, strings.Join(times, " "))
	}
	for _, name := range []string{"2-window", "3-items", "4-compare-union-all", "5-window-replaces-dropped"} {
		plan := plans[name]
		row, scan := usesIndex(plan, "eval_readings_row"), seqScanned(plan, "eval_readings")
		c.r.result("k2 plan=%s uses_eval_readings_row=%t seq_scan_of_eval_readings=%t", name, row, scan)
		if plan == "" || !row || scan {
			c.r.fail("k2 %s does not read eval_readings through eval_readings_row alone", name)
		}
	}
	if !strings.Contains(plans["4-compare-union-all"], "Append") {
		c.r.fail("k2 the compare read is not an Append of two windowed reads")
	}
	sameShape := plans["2-window"] != "" && planShape(plans["2-window"]) == planShape(plans["5-window-replaces-dropped"])
	c.r.result("k2 window_read_keeps_its_plan_without_eval_readings_replaces=%t", sameShape)
	if !sameShape {
		c.r.fail("k2 the window read's plan changes when eval_readings_replaces is dropped")
	}
	c.r.result("k2 plans_examined=%d", len(plans))
	if len(plans) < 6 {
		c.r.fail("k2: %d plans examined, below six", len(plans))
	}

	indexes, err := c.pool.Query(ctx, `select indexname, indexdef from pg_indexes where tablename = 'eval_readings' order by indexname`)
	if err != nil {
		c.r.fail("k2 index list: %v", err)
		return
	}
	var name, definition string
	listed := map[string]bool{}
	if _, err := pgx.ForEachRow(indexes, []any{&name, &definition}, func() error {
		listed[name] = true
		c.r.result("k2 index %s: %s", name, definition)
		return nil
	}); err != nil {
		c.r.fail("k2 index list: %v", err)
	}
	c.r.result("k2 indexes_listed=%d", len(listed))
	if !listed["eval_readings_row"] || !listed["eval_readings_replaces"] {
		c.r.fail("k2 eval_readings lacks eval_readings_row or eval_readings_replaces after the restore")
	}

	var replaced string
	if err := c.pool.QueryRow(ctx, `select replaces::text from eval_readings where eval_id = $1 and replaces is not null order by measured_at desc limit 1`, evalID).Scan(&replaced); err != nil {
		c.r.fail("k2: find a reading %s has corrected once: %v", evalID, err)
		return
	}
	noValue := `insert into eval_readings (eval_id, series, row_kind, row_key, measured_at, recorded_by) values ($1, 'value', $2, 'k2-no-value', now(), '{"kind":"system","id":"evalbench"}')`
	secondCorrection := `insert into eval_readings (eval_id, series, row_kind, row_key, value, measured_at, recorded_by, replaces)
		values ($1, 'value', $2, 'k2-second-correction', 1, now(), '{"kind":"system","id":"evalbench"}', $3::uuid)`
	refusals := 0
	for _, refused := range []struct {
		name, text string
		args       []any
		want       string
	}{
		{"no-value-no-reason", noValue, []any{evalID, rowKind}, "23514"},
		{"second-correction", secondCorrection, []any{evalID, rowKind, replaced}, "23505"},
	} {
		code, constraint, err := c.insertRolledBack(ctx, "", refused.text, refused.args...)
		if err != nil {
			c.r.fail("k2 refusal %s: %v", refused.name, err)
			continue
		}
		c.r.result("k2 refusal=%s sqlstate=%s constraint=%q", refused.name, code, constraint)
		if code != refused.want {
			c.r.fail("k2 refusal %s answered SQLSTATE %q, want %s", refused.name, code, refused.want)
			continue
		}
		refusals++
	}
	c.r.result("k2 refusals=%d", refusals)
	if refusals < 2 {
		c.r.fail("k2: %d refusals, below two", refusals)
	}

	// The controls: without the row index the items read scans the table, and without the index
	// on replaces a second correction is stored.
	plan, ms, err := c.explainDropped(ctx, windowRead+` limit $5 offset $6`, append(args(w28), "200", "0"), "eval_readings_row")
	if err != nil {
		c.r.fail("k2 control items read without eval_readings_row: %v", err)
	} else {
		c.r.block("PLAN k2 control-items-row-index-dropped", plan)
		scan := seqScanned(plan, "eval_readings")
		c.r.result("k2 control=items-read-without-eval_readings_row execution_ms=%.2f seq_scan_of_eval_readings=%t", ms, scan)
		if !scan {
			c.r.fail("k2 control: with eval_readings_row dropped the items read does not scan eval_readings")
		}
	}
	code, _, err := c.insertRolledBack(ctx, "eval_readings_replaces", secondCorrection, evalID, rowKind, replaced)
	stored := err == nil && code == ""
	c.r.result("k2 control=second-correction-without-eval_readings_replaces stored=%t sqlstate=%q", stored, code)
	if !stored {
		c.r.fail("k2 control: with eval_readings_replaces dropped the second correction is not stored (%v)", err)
	}
}

// explainDropped explains text with params, with index dropped first where it names one, in one
// transaction the explainer rolls back, which restores the index.
func (c *headChecks) explainDropped(ctx context.Context, text string, params []string, index string) (string, float64, error) {
	if index == "" {
		return c.explain.explain(ctx, text, params)
	}
	return c.explain.explainAfter(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `drop index `+index); err != nil {
			return fmt.Errorf("drop %s: %w", index, err)
		}
		return nil
	}, text, params)
}

// insertRolledBack runs one insert in a transaction it rolls back, with index dropped first where
// it names one, and answers the SQLSTATE and constraint it was refused with, or none.
func (c *headChecks) insertRolledBack(ctx context.Context, index, text string, args ...any) (string, string, error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if index != "" {
		if _, err := tx.Exec(ctx, `drop index `+index); err != nil {
			return "", "", fmt.Errorf("drop %s: %w", index, err)
		}
	}
	_, err = tx.Exec(ctx, text, args...)
	var refused *pgconn.PgError
	if errors.As(err, &refused) {
		return refused.Code, refused.ConstraintName, nil
	}
	return "", "", err
}
