package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/evals/evalbench"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// headChecks is this head's checks after the first paint: k13's named-jobs read and P0 list,
// k16's day rows by repository, hand folds, fallback and move of the default repository, and k2's
// statements, index list, refusals and controls.
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
// the 84-day window and the production jobs INFRA's source judges a deploy by, and the list of P0
// issues with no owner through delivery.ListUnownedP0, the read the unowned-p0 eval's day rows are
// written from (Store.WriteUnownedP0Day cuts that list by repository): the first must carry the
// job names in its index condition, the second read issues_open_p0 and scan no table of issues.
func (c *headChecks) k13() {
	ctx := context.Background()
	project, err := delivery.GetProjectSettings(ctx, c.pool, evalbench.Project)
	if err != nil {
		c.r.fail("k13: read the delivery settings of %s: %v", evalbench.Project, err)
		return
	}
	rules := project.Rules()
	if len(rules) == 0 {
		c.r.fail("k13: %s's delivery settings read no deploy repository", evalbench.Project)
		return
	}
	settings := rules[0]
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
		named := namedJobCondition.MatchString(plan)
		c.r.result("k13 named_jobs runs=%d names=%q jobs=%d name_in_index_condition=%t", len(runIDs), names, jobs, named)
		if !named {
			c.r.fail("k13 the named-jobs read carries no job name in its index condition")
		}
	}
	var unowned []delivery.UnownedIssue
	for _, plan := range c.captured(ctx, "k13-unowned-p0", "claimed_by is null and route is null", func() (err error) {
		unowned, err = delivery.ListUnownedP0(ctx, c.pool, c.o.project)
		return err
	}) {
		index, scans := usesIndex(plan, "issues_open_p0"), seqScanned(plan, "issues")
		c.r.result("k13 unowned_p0 project=%s count=%d uses_issues_open_p0=%t seq_scan_of_issues=%t", c.o.project, len(unowned), index, scans)
		if !index || scans {
			c.r.fail("k13 the unowned-P0 list reads issues otherwise than through issues_open_p0")
		}
	}
}

// k16 prints the project's day rows by repository, and for one count, one share, one quantile and
// the unowned-p0 count folds each repository's day rows of the 84-day window by hand, against the
// list read with that repository, and the repositories' parts against the project's. A project's
// day rows are those of every key it reads (ownedDays): its own and those of the owner keys it
// reads under the published defaults, as every eval read of it folds them. Its own rows (repo "")
// and the rows of no resolved repository (legionfacts.DefaultRepo) are no repository, so the day
// rows must hold two named repositories for the repository to be a dimension of them. A fold the
// route and the hand both answer null for is "no data" on both sides (R12), a repository with no
// row in the window among them (the hand count of samples is null there, as the route's n is, and
// not 0), and compares nothing, so it is printed and not counted: each fold must hold a value for
// the project and for at least one repository. unowned-p0 folds as the route folds it
// (unownedP0Statistics): the sum of the last values of the newest day the window holds a row of,
// the project's row on its own, so the repositories' rows, the rows of no repository among them,
// must sum to it on that day. Its row of a day is written on that day alone, and a range that
// starts before a series' first day reads no data (R12, evals.FirstDaysIn), so its window is the
// one day the clock reads, the later of the generator's day and today's.
func (c *headChecks) k16() {
	ctx := context.Background()
	rows, err := c.pool.Query(ctx, `select d.repo, count(*) from `+ownedDays+` where o.project_key = $1 group by d.repo order by d.repo`, c.o.project)
	if err != nil {
		c.r.fail("k16: count the day rows by repository: %v", err)
		return
	}
	var repo string
	var count int64
	repositories := 0
	if _, err := pgx.ForEachRow(rows, []any{&repo, &count}, func() error {
		named := repo != "" && repo != legionfacts.DefaultRepo
		if named {
			repositories++
		}
		c.r.result("k16 day_rows project=%s repo=%q rows=%d named_repository=%t", c.o.project, repo, count, named)
		return nil
	}); err != nil {
		c.r.fail("k16: count the day rows by repository: %v", err)
		return
	}
	if repositories < 2 {
		c.r.fail("k16: the day rows of %s hold %d named repositories, beside its own rows and those of no repository, so no repository is a dimension of them", c.o.project, repositories)
	}
	// statistic is one statistic k16 folds by hand: the hand fold over the rows a filter keeps, and
	// the window it is read over.
	type statistic struct {
		slug, series, stat string
		hand               func(filter string) string
		window             window
	}
	over := func(selected, from string) func(string) string {
		return func(filter string) string { return `select ` + selected + ` from ` + from + ` where ` + filter }
	}
	w84 := before(c.today, 84)
	latest := c.today
	if now := time.Now().UTC().Truncate(24 * time.Hour); now.After(latest) {
		latest = now
	}
	folds := []statistic{
		{evals.SlugPROpenToMerge, measures.SeriesMinutes, "n", over(`sum(cardinality(d.samples))::float8`, ownedDays), w84},
		{evals.SlugReworkShare, measures.SeriesPRs, "rate", over(`sum(d.num) / nullif(sum(d.den), 0)`, ownedDays), w84},
		{evals.SlugPROpenToMerge, measures.SeriesMinutes, "median", over(`percentile_cont(0.5) within group (order by sample)`, ownedDays+`, unnest(d.samples) as sample`), w84},
		{evals.SlugUnownedP0, evals.SeriesUnownedP0, "value", func(filter string) string {
			return `with held as (select d.day, d.last_value from ` + ownedDays + ` where ` + filter + ` and d.last_value is not null)
				select sum(last_value) from held where day = (select max(day) from held)`
		}, before(latest.AddDate(0, 0, 1), 1)},
	}
	compared := 0
	for _, fold := range folds {
		// The project's own answer, the loop's first read (repo ""), is what the repositories'
		// rows partition; where that read failed, its FAIL line stands and there is no partition
		// to judge.
		var project *float64
		projectRead, valued := false, 0
		for _, repo := range append([]string{""}, evalbench.Repos...) {
			route, err := c.routeValue(ctx, fold.window, repo, fold.slug, fold.series, fold.stat)
			if err != nil {
				c.r.fail("k16 %s.%s.%s repo=%q: %v", fold.slug, fold.series, fold.stat, repo, err)
				continue
			}
			if repo == "" {
				project, projectRead = route, true
			}
			// A computed eval's fold reads the computation's listed rows alone (eval_days.writer,
			// eval_days.listed). A repository's cut reads the Legion rows stored under
			// legionfacts.DefaultRepo of the project's own key where the repository is its
			// published default, as EvalDaysIn does.
			hand, err := c.handFold(ctx, fold.hand(ownedEval+` and d.writer = 'computation' and d.listed and d.series = $3
				and (d.repo = $4 or (d.repo = '`+legionfacts.DefaultRepo+`' and d.project_key = o.project_key and exists (
					select 1 from eval_project_repos p where p.project_key = $1 and p.repo = $4)))
				and d.day >= $5 and d.day < $6`),
				evals.EvalID(c.o.project, fold.slug), fold.series, repo, fold.window)
			if err != nil {
				c.r.fail("k16 %s.%s.%s repo=%q: %v", fold.slug, fold.series, fold.stat, repo, err)
				continue
			}
			equal, held := same(route, hand), route != nil || hand != nil
			c.r.result("k16 fold eval=%s series=%s stat=%s repo=%q window=%s route=%s hand=%s equal=%t compared=%t", fold.slug, fold.series, fold.stat, repo, fold.window, show(route), show(hand), equal, held)
			if !equal {
				c.r.fail("k16 %s.%s.%s repo=%q: the route answers %s and the hand fold %s", fold.slug, fold.series, fold.stat, repo, show(route), show(hand))
			}
			if held {
				compared++
				if repo != "" {
					valued++
				}
			}
		}
		if !projectRead {
			continue
		}
		if valued == 0 {
			c.r.fail("k16 %s.%s.%s: no repository's fold holds a value, so the repository filter compared nothing", fold.slug, fold.series, fold.stat)
		}
		// The repositories partition the project's rows: a count sums, and a share or a quantile
		// is the fold of the union of their parts, read as the repositories' own folds read them.
		union, err := c.handFold(ctx, fold.hand(ownedEval+` and d.writer = 'computation' and d.listed and d.series = $3 and d.repo <> $4
			and d.day >= $5 and d.day < $6`),
			evals.EvalID(c.o.project, fold.slug), fold.series, "", fold.window)
		if err != nil {
			c.r.fail("k16 %s.%s.%s union: %v", fold.slug, fold.series, fold.stat, err)
			continue
		}
		equal := same(project, union)
		c.r.result("k16 partition eval=%s series=%s stat=%s project=%s fold_of_the_repositories=%s equal=%t", fold.slug, fold.series, fold.stat, show(project), show(union), equal)
		switch {
		case project == nil:
			c.r.fail("k16 %s.%s.%s: the project's fold holds no value, so the partition compared nothing", fold.slug, fold.series, fold.stat)
		case !equal:
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

// ownedDays is the day rows d of every key the project o names reads (eval_project_owners): its own
// and those of each owner key it reads under the published defaults.
const ownedDays = `eval_project_owners o join eval_days d on d.project_key = o.owner`

// ownedEval holds ownedDays to the project $1 and its eval $2: the rows of the eval of the same
// slug under each key the project reads, as evals.EvalDaysIn reads them.
const ownedEval = `o.project_key = $1 and d.eval_id = o.owner || substr($2, length($1) + 1)`

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

// k2Statement is one statement k2 explains: its name, its text and the literals of its
// parameters, and the index dropped before it, none where empty.
type k2Statement struct {
	name, text string
	params     []string
	drop       string
}

// k2 runs k2's statements over the readings of one logged eval and one row kind in a 28-day
// window (the plan's "The checks"): the registry read, the window read, the items read, the two
// statements of a compare read as the server sends them (compareStatements), and, with the index on
// replaces dropped, the window read again and the same read written as an anti-join, which must
// cost at least ten times the window read (k2: "the anti-join form of the same read costs at least
// ten times the shipped read"); then the index list, the two refusals, each by its constraint, and
// the two controls.
func (c *headChecks) k2() {
	ctx := context.Background()
	slug := evalbench.LoggedSlugs()[0]
	evalID := evals.EvalID(c.o.project, slug)
	rowKind := "issue"
	w28, c28 := before(c.today, 28), before(c.today.AddDate(0, 0, -28), 28)
	args := func(w window) []string {
		return []string{quote(evalID), quote(w.from.Format(time.RFC3339)), quote(w.to.Format(time.RFC3339)), quote(rowKind)}
	}
	windowRead := `select ` + evals.ReadingColumns + ` from eval_readings
		where eval_id = $1 and superseded_at is null and measured_at >= $2 and measured_at < $3 and row_kind = $4
		order by measured_at desc, id`
	antiJoin := `select ` + evals.ReadingColumns + ` from eval_readings r
		where r.eval_id = $1 and r.measured_at >= $2 and r.measured_at < $3 and r.row_kind = $4
		  and not exists (select 1 from eval_readings s where s.replaces = r.id)
		order by r.measured_at desc, r.id`
	statements := []k2Statement{
		{"1-registry", `select * from evals_current where project_key = $1 and state = 'active'`, []string{quote(c.o.project)}, ""},
		{"2-window", windowRead, args(w28), ""},
		{"3-items", windowRead + ` limit $5 offset $6`, append(args(w28), "200", "0"), ""},
	}
	compare, err := c.compareStatements(ctx, slug, rowKind, w28, c28)
	if err != nil {
		c.r.fail("k2 the compare read: %v", err)
	}
	statements = append(statements, compare...)
	statements = append(statements,
		k2Statement{"5-window-replaces-dropped", windowRead, args(w28), "eval_readings_replaces"},
		k2Statement{"6-anti-join-replaces-dropped", antiJoin, args(w28), "eval_readings_replaces"})
	plans, medians := map[string]string{}, map[string]float64{}
	for _, statement := range statements {
		var times []float64
		for run := 1; run <= 3; run++ {
			plan, ms, err := c.explainDropped(ctx, statement.text, statement.params, statement.drop)
			if err != nil {
				c.r.fail("k2 %s: %v", statement.name, err)
				break
			}
			times = append(times, ms)
			if run == 1 {
				plans[statement.name] = plan
				c.r.block(fmt.Sprintf("SQL k2 %s dropped=%q params=%s", statement.name, statement.drop, strings.Join(statement.params, ", ")), statement.text)
				c.r.block("PLAN k2 "+statement.name, plan)
			}
		}
		medians[statement.name] = median(times)
		c.r.result("k2 statement=%s eval=%s row_kind=%s window=%s execution_ms=%s", statement.name, evalID, rowKind, w28, formatMs(times))
	}
	for _, name := range []string{"2-window", "3-items", "4-compare-window", "4-compare-range", "5-window-replaces-dropped"} {
		alone := readsOnlyThrough(plans[name], "eval_readings", "eval_readings_row")
		c.r.result("k2 plan=%s reads_eval_readings_through_eval_readings_row_alone=%t", name, alone)
		if !alone {
			c.r.fail("k2 %s does not read eval_readings through eval_readings_row alone", name)
		}
	}
	sameShape := plans["2-window"] != "" && planShape(plans["2-window"]) == planShape(plans["5-window-replaces-dropped"])
	c.r.result("k2 window_read_keeps_its_plan_without_eval_readings_replaces=%t", sameShape)
	if !sameShape {
		c.r.fail("k2 the window read's plan changes when eval_readings_replaces is dropped")
	}
	shipped, antiJoined := medians["5-window-replaces-dropped"], medians["6-anti-join-replaces-dropped"]
	tenTimes := shipped > 0 && antiJoined >= 10*shipped
	c.r.result("k2 anti_join_execution_ms_median=%.2f window_execution_ms_median=%.2f at_least_ten_times=%t", antiJoined, shipped, tenTimes)
	if !tenTimes {
		c.r.fail("k2 the anti-join form of the window read costs %.2f ms against the shipped read's %.2f ms, less than ten times it", antiJoined, shipped)
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
		code       string
		constraint string
	}{
		{"no-value-no-reason", noValue, []any{evalID, rowKind}, "23514", "eval_readings_value_or_reason"},
		{"second-correction", secondCorrection, []any{evalID, rowKind, replaced}, "23505", "eval_readings_replaces"},
	} {
		code, constraint, err := c.insertRolledBack(ctx, "", refused.text, refused.args...)
		if err != nil {
			c.r.fail("k2 refusal %s: %v", refused.name, err)
			continue
		}
		c.r.result("k2 refusal=%s sqlstate=%s constraint=%q", refused.name, code, constraint)
		if code != refused.code || constraint != refused.constraint {
			c.r.fail("k2 refusal %s answered SQLSTATE %q by %q, want %s by %s", refused.name, code, constraint, refused.code, refused.constraint)
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

// orOfWindows matches a statement that reads two windows of measured_at in one: an `or` between
// two tests of it, which R2 rules out for a compare read.
var orOfWindows = regexp.MustCompile(`(?is)\bmeasured_at\b.*\bor\b.*\bmeasured_at\b`)

// compareStatements reads the items of the logged eval slug of rowKind over w, with compare as its
// compare range, through the server's items route (GET /api/v1/projects/{key}/evals/{slug}/items),
// as the Evals tab reads them, and answers the statements it ran of evals.ItemsIn's page of
// readings, named 4-compare-window and 4-compare-range in the order the server ran them (the
// window's and then the compare range's, api/evals_items.go's evalItemSources), each with the
// literals of its parameters as the statement log holds them: the two statements of a compare read
// as the server sends them (R2, k2). It fails where the server ran other than two of them, two over
// one range, or one that reads both ranges in one statement (orOfWindows).
func (c *headChecks) compareStatements(ctx context.Context, slug, rowKind string, w, compare window) ([]k2Statement, error) {
	from, err := fileSize(c.o.pgLog)
	if err != nil {
		return nil, fmt.Errorf("read the statement log: %w", err)
	}
	query := url.Values{"from": {w.from.Format(time.RFC3339)}, "to": {w.to.Format(time.RFC3339)},
		"compare_from": {compare.from.Format(time.RFC3339)}, "compare_to": {compare.to.Format(time.RFC3339)}, "row_kind": {rowKind}}
	status, body, err := getBody(ctx, c.client, c.o.url+"/api/v1/projects/"+c.o.project+"/evals/"+slug+"/items?"+query.Encode(), c.o.token)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("the items read answered %d: %s", status, body)
	}
	// The statement log is written a moment after each statement ends.
	time.Sleep(time.Second)
	executions, err := readStatementLog(c.o.pgLog, from)
	if err != nil {
		return nil, err
	}
	page := oneLine(`select ` + evals.ReadingColumns + ` from eval_readings where`)
	var found []k2Statement
	for _, execution := range executions {
		if !strings.Contains(oneLine(execution.text), page) {
			continue
		}
		if orOfWindows.MatchString(execution.text) {
			return nil, fmt.Errorf("the server read both ranges in one statement: %s", oneLine(execution.text))
		}
		found = append(found, k2Statement{text: execution.text, params: execution.params})
	}
	if len(found) != 2 {
		return nil, fmt.Errorf("the server ran %d statements of the page of readings for a window and a compare range, not two", len(found))
	}
	if len(found[0].params) < 3 || len(found[1].params) < 3 || slices.Equal(found[0].params[1:3], found[1].params[1:3]) {
		return nil, fmt.Errorf("the server's two statements of the page of readings do not read two ranges: %q and %q", found[0].params, found[1].params)
	}
	found[0].name, found[1].name = "4-compare-window", "4-compare-range"
	return found, nil
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

// fallbackLeaseTries is how many times the fallback tries for the refresher's lease, and
// fallbackLeaseWait how long it waits after a try that did not take it: two minutes in all, the
// bound the paint's client gives each read of the server. A refresher pass holds the lease for as
// long as it runs and gives it back as it ends (duty.Exclusive with no period, which duty.RunWoken
// takes), so a try between two passes takes it. A lease no try takes in two minutes is a pass that
// runs longer than that, or a take that keeps failing, and the lines duty.Exclusive writes say which.
const (
	fallbackLeaseTries = 240
	fallbackLeaseWait  = 500 * time.Millisecond
)

// fallback measures, for k16, the read of a repository whose cut a default move has marked stale
// and the refresher has not folded again yet (dispatch://INFRA-2916/artifact/decisions-md, "A
// repository's cut is stored again"), and the same reads once the cut's first paint is folded: it
// marks the project's cut of the deploy repository stale as a move's commit marks it
// (eval_figure_cuts_stale), its first paint owed, and reads the paint's 84-day read with a compare
// range and its 28-day read of that repository once more, as the paint measures each (measure),
// and that 84-day read one window back (pastCase), which fold the day rows of the evals the mark
// names while it owes its first paint and read every other eval's stored figures
// (figuresThrough); then it folds the cut's first paint as the boot and a refresher pass fold it
// before the rest of the refold (evals.FoldFirstPaint), timed, and makes the three reads again,
// which all read the stored figures of the first paint: it holds the current window's ranges and
// the ranges one window back (evals' firstPaintOf), where the rest of the cut's history, the
// reads two windows back, is folded from the day rows until the refold comes down to it. It takes
// the mark away again where it made it; then it does the same with the mark naming every eval, the
// worst case (staleReads).
//
// A reader meets a mark whose first paint is owed only rarely: a default move folds the first paint
// of each cut it marks whose mark names no logged eval in the commit that makes the mark
// (evals.publishDefaults), and that of the others in the first batch of the refold of the same
// pass, and the boot folds the first paint of every marked cut, those of its rename pass among
// them, before it serves, giving up after cmd/dispatch's firstPaintBootTimeout, so only a boot that
// gave up serves such a mark. The architect's ruling
// (dispatch://INFRA-2916/comment/813ac6b9-2c45-41b0-a5e4-b839542f0d0c, option A)
// therefore gates the reads made after the first paint is folded by the first paint's database
// budgets, and records the reads made before it, with the node, without timing them against those
// budgets (judgeStaleReads); both must answer the stored read's items.
//
// A running server's refresher folds a marked cut again in any pass (evals.refoldStaleCuts): its
// first batch folds the first paint, the next records where the refold stands (next_day) and its
// last deletes the mark. So the fallback does all of that holding the refresher's lease
// (evals.RefresherLease) on a connection of the bench's lease pool, as a refresher pass holds it
// (duty.Exclusive with no period), and no refresher pass of any process that takes the lease runs
// until it gives the lease back. A try that finds the lease held is made again, fallbackLeaseWait
// later, up to fallbackLeaseTries; where none takes it, the fallback states no read and fails.
func (c *headChecks) fallback(measured []measuredCase, catalog map[int64]bool) {
	ctx := context.Background()
	callers, err := c.pool.Leases()
	if err != nil {
		c.r.fail("k16 fallback: open the lease pool: %v", err)
		return
	}
	leases, err := callers.For("k16 fallback")
	if err != nil {
		c.r.fail("k16 fallback: reserve a connection of the lease pool: %v", err)
		return
	}
	// With no period and a context nothing cancels, Exclusive reports exactly whether the pass
	// ran: it did not take the lease otherwise.
	read := func(ctx context.Context) { c.staleReads(ctx, measured, catalog) }
	for try := 1; !duty.Exclusive(ctx, nil, leases, evals.RefresherLease, time.Time{}, 0, read); try++ {
		if try == fallbackLeaseTries {
			c.r.fail("k16 fallback: no try of %d, %s apart, took the refresher's lease %s: a refresher pass held it at each, or the take failed (the dispatch duty lines above say which); no read of a stale cut is stated",
				fallbackLeaseTries, fallbackLeaseWait, evals.RefresherLease)
			return
		}
		time.Sleep(fallbackLeaseWait)
	}
}

// staleReads is the fallback's measurement, made under the refresher's lease, under three marks,
// one after the other (staleMarkReads): two of the project's cut of the deploy repository, its
// published default, the mark a move of the project's default away from it makes, naming the evals
// whose rows the move moves out of the cut (defaultRowEvals), its reads named with -stale, and one
// naming every eval (null), the mark of the boot's rename pass and the worst case, its reads named
// with -stale-all; and one of the project's own rows (""), which the main Evals page reads and a
// move marks for the evals of the owner keys it moves, naming every eval, its worst case, its reads
// (list-84d-compare, list-28d and the first one window back) named with -stale-all. The reads made
// after the first paint is folded, named with -ready, are what a reader meets after a move, whose
// commit folds the first paint, or after a boot, which folds it before it serves; the reads made
// before it are what a reader meets only from a boot that gave up its fold (fallback). Each read
// of the paint is compared with the paint's own read of the stored figures; each read one window
// back (pastCase) with the same read made first, with no mark, from the stored figures, its line
// named with -past. The first paint is folded as the server's build (serverBuild), as the server's
// own refresher would fold it. A project that publishes another default, or a move that names no
// eval of the cut, fails beside the first mark, which then states no read.
func (c *headChecks) staleReads(ctx context.Context, measured []measuredCase, catalog map[int64]bool) {
	repo := evalbench.Repos[0]
	build, err := serverBuild(ctx, c.o.url)
	if err != nil {
		c.r.fail("k16 fallback: read the server's build, which the first paint is folded as: %v", err)
		return
	}
	cases, ok := c.staleCases(ctx, measured, catalog, "list-84d-compare-repo", "list-28d-repo")
	own, ownOK := c.staleCases(ctx, measured, catalog, "list-84d-compare", "list-28d")
	if !ok || !ownOK {
		return
	}
	moved, err := c.defaultRowEvals(ctx, repo)
	switch {
	case err != nil:
		c.r.fail("k16 fallback: read the evals a move of %s's default repository from %s names: %v", c.o.project, repo, err)
	case len(moved) == 0:
		c.r.fail("k16 fallback: a move of %s's default repository from %s names no eval of its cut, so no read of a cut that move marks is stated", c.o.project, repo)
	default:
		c.staleMarkReads(ctx, catalog, repo, build, moved, "", cases)
	}
	c.staleMarkReads(ctx, catalog, repo, build, nil, "-all", cases)
	c.staleMarkReads(ctx, catalog, "", build, nil, "-all", own)
}

// staleCases is the reads of the paint named names a mark's reads make (staleMarkReads), each with
// the paint's own measurement of it from the stored figures, and after them the first of them one
// window back (pastCase), measured once here, with no mark, from the stored figures, its RESULT
// line stated. ok is false where the paint holds none of names or a read fails, which it states.
func (c *headChecks) staleCases(ctx context.Context, measured []measuredCase, catalog map[int64]bool, names ...string) ([]staleCase, bool) {
	var cases []staleCase
	for _, name := range names {
		index := slices.IndexFunc(measured, func(one measuredCase) bool { return one.name == name })
		if index < 0 {
			c.r.fail("k16 fallback: the paint has no case %s", name)
			continue
		}
		cases = append(cases, staleCase{read: measured[index].paintCase, stored: measured[index]})
	}
	if len(cases) == 0 || cases[0].read.name != names[0] {
		return cases, len(cases) > 0
	}
	past, err := c.pastCase(cases[0].read)
	if err != nil {
		c.r.fail("k16 fallback: %v", err)
		return nil, false
	}
	stored, err := measure(ctx, c.pool, c.client, c.o, past, catalog)
	if err != nil {
		c.r.fail("k16 fallback: %s: %v", past.name, err)
		return nil, false
	}
	c.r.result("k16 fallback case=%s marked_evals=none status=%d db_ms=%.2f statements=%d server_ms=%.0f body_bytes=%d gzip_bytes=%d",
		past.name, stored.status, dbMs(stored.own), len(stored.own), stored.serverMs, len(stored.body), gzipSize(stored.body))
	return append(cases, staleCase{read: past, stored: stored, past: true}), true
}

// defaultRowEvals is the evals a move of the project's default repository away from repo names in
// repo's cut for the project's own rows stored under legionfacts.DefaultRepo, its evals that hold a
// listed row under it, read by the statement the move's commit reads them with
// (evals.DefaultRowEvalsQuery): it marks the cut as the ownDefault case of evals.staleMarksOf does,
// which is the whole of the mark of the move of INFRA's default the farm measures, a move that
// moves no owner key. The project must publish repo as its default.
func (c *headChecks) defaultRowEvals(ctx context.Context, repo string) ([]string, error) {
	published, err := evals.PublishedDefaultIn(ctx, c.pool, c.o.project)
	if err != nil {
		return nil, err
	}
	if published != repo {
		return nil, fmt.Errorf("%s publishes %q as its default repository, not %s", c.o.project, published, repo)
	}
	rows, err := c.pool.Query(ctx, evals.DefaultRowEvalsQuery, []string{c.o.project})
	if err != nil {
		return nil, err
	}
	var marked []string
	var project, evalID string
	if _, err := pgx.ForEachRow(rows, []any{&project, &evalID}, func() error {
		marked = append(marked, evalID)
		return nil
	}); err != nil {
		return nil, err
	}
	slices.Sort(marked)
	return marked, nil
}

// staleCase is one read the fallback makes under a mark (staleMarkReads): the read, the same read's
// measurement from the stored figures with no mark, whose items it must answer, and whether it is
// the read one window back (pastCase).
type staleCase struct {
	read   paintCase
	stored measuredCase
	past   bool
}

// names is the case's names under a mark, tag naming the mark ("" for the evals a move names, -all
// for every eval): before the cut's first paint is folded, -stale with tag, and after it -ready
// after those for a read of the paint, and -ready with tag in place of them for the read one window
// back, whose name already says which window it reads.
func (s staleCase) names(tag string) (string, string) {
	if s.past {
		return s.read.name + "-stale" + tag, s.read.name + "-ready" + tag
	}
	return s.read.name + "-stale" + tag, s.read.name + "-stale" + tag + "-ready"
}

// pastCase is stored, the paint's 84-day read with a compare range, of the repository or of the
// project's own rows, moved one window back, as a person who steps back a window reads it: its
// window the 84 days ending 84 days before the live view's window end (through-1-84, through being
// the last end day the figures are built through, FigureLead days after today), and its compare
// range the 84 days before that. Under a mark a read of it folds the day rows of the evals the mark
// names until the cut's first paint is folded, whose ranges include the ones one window back
// (evals' firstPaintOf), and reads the stored figures of the first paint from then on, as the
// reads of the current window do.
func (c *headChecks) pastCase(stored paintCase) (paintCase, error) {
	at, err := url.Parse(stored.path)
	if err != nil {
		return paintCase{}, fmt.Errorf("read the path of %s: %w", stored.name, err)
	}
	end := c.today.AddDate(0, 0, evals.FigureLead-1-84)
	w, compare := before(end, 84), before(end.AddDate(0, 0, -84), 84)
	query := at.Query()
	query.Set("from", w.from.Format(time.RFC3339))
	query.Set("to", w.to.Format(time.RFC3339))
	query.Set("compare_from", compare.from.Format(time.RFC3339))
	query.Set("compare_to", compare.to.Format(time.RFC3339))
	at.RawQuery = query.Encode()
	past := stored
	past.name, past.path = stored.name+"-past", at.String()
	return past, nil
}

// staleMarkReads marks the project's cut of repo stale, naming marked (nil: every eval), its first
// paint owed, and makes each read of cases, each named as tag names it before the first paint
// (staleCase.names); then folds the cut's first paint as build (evals.FoldFirstPaint), and makes
// each read again, named as tag names it after the first paint; and takes the mark back where it
// made it. Before the take-back it reads the mark back. The lease keeps out every refresher that
// takes it, so where the mark no longer stands at generation 1 with its refold to begin, naming
// marked, its first paint folded at the end day the fold answered, a writer that takes no lease
// moved it during the measurement, and a read may have read the stored figures of an eval it
// names, or the day rows of one the first paint answers: it states no read of it and fails. One
// RESULT line names the first paint's fold, the evals it folded, the end day it folded through and
// its transaction's time, and each read is judged by judgeStaleReads: its line names the read, how
// many evals the mark names ("all" for every one), its database and server time, its body, the
// same read's database time from the stored figures with no mark, whether the two answers' items
// are equal, and the node.
func (c *headChecks) staleMarkReads(ctx context.Context, catalog map[int64]bool, repo, build string, marked []string, tag string, cases []staleCase) {
	inserted, err := c.pool.Exec(ctx, `
		insert into eval_figure_cuts_stale (project_key, repo, generation, ready_through, next_day, evals) values ($1, $2, 1, null, null, $3)
		on conflict (project_key, repo) do nothing
	`, c.o.project, repo, marked)
	if err != nil {
		c.r.fail("k16 fallback: mark %s's cut of %s stale: %v", c.o.project, repo, err)
		return
	}
	if inserted.RowsAffected() == 1 {
		defer func() {
			if _, err := c.pool.Exec(ctx, `delete from eval_figure_cuts_stale where project_key = $1 and repo = $2 and generation = 1 and next_day is null
				and evals is not distinct from $3`, c.o.project, repo, marked); err != nil {
				c.r.fail("k16 fallback: take back the mark of %s's cut of %s: %v", c.o.project, repo, err)
			}
		}()
	}
	names := "all"
	if marked != nil {
		names = strconv.Itoa(len(marked))
	}
	var reads []staleRead
	read := func(one staleCase, name string, painted bool) {
		run := one.read
		run.name = name
		got, err := measure(ctx, c.pool, c.client, c.o, run, catalog)
		if err != nil {
			c.r.fail("k16 fallback: %s: %v", run.name, err)
			return
		}
		equal := sameItems(got.body, one.stored.body)
		reads = append(reads, staleRead{name: run.name, dbMs: dbMs(got.own), budgetMs: run.budgetMs, painted: painted, equal: equal,
			line: fmt.Sprintf("k16 fallback case=%s marked_evals=%s status=%d db_ms=%.2f statements=%d server_ms=%.0f body_bytes=%d gzip_bytes=%d stored_db_ms=%.2f items_equal=%t node=%s",
				run.name, names, got.status, dbMs(got.own), len(got.own), got.serverMs, len(got.body), gzipSize(got.body), dbMs(one.stored.own), equal, cmp.Or(c.o.node, "unnamed"))})
	}
	for _, one := range cases {
		stale, _ := one.names(tag)
		read(one, stale, false)
	}
	paint, err := evals.FoldFirstPaint(ctx, c.pool, build, c.o.project, repo)
	if err != nil {
		c.r.fail("k16 fallback: fold the first paint of %s's cut of %s: %v", c.o.project, repo, err)
		return
	}
	if !paint.Folded {
		c.r.fail("k16 fallback: the first paint of %s's cut of %s, marked naming %s evals, folded nothing: the mark owed none, or %s's figures are folded nowhere; no read of it is stated",
			c.o.project, repo, names, c.o.project)
		return
	}
	for _, one := range cases {
		_, ready := one.names(tag)
		read(one, ready, true)
	}
	var stands bool
	if err := c.pool.QueryRow(ctx, `select exists (select 1 from eval_figure_cuts_stale where project_key = $1 and repo = $2 and generation = 1 and next_day is null
		and ready_through = $4 and evals is not distinct from $3)`, c.o.project, repo, marked, paint.Through).Scan(&stands); err != nil {
		c.r.fail("k16 fallback: read back the mark of %s's cut of %s: %v", c.o.project, repo, err)
		return
	}
	if !stands {
		c.r.fail("k16 fallback: the mark of %s's cut of %s no longer stands at generation 1 with its refold to begin, naming %s evals, its first paint folded through %s, though the fallback held the refresher's lease, so its reads may have read the stored figures of an eval it names; no read of it is stated",
			c.o.project, repo, names, paint.Through.Format(time.DateOnly))
		return
	}
	c.r.result("k16 fallback first-paint repo=%q marked_evals=%s evals=%d through=%s transaction_ms=%d locks_held_ms=%d",
		repo, names, paint.Evals, paint.Through.Format(time.DateOnly), paint.Transaction.Milliseconds(), paint.LocksHeld.Milliseconds())
	judgeStaleReads(c.r, c.o.info, reads)
}

// staleRead is one read the fallback made under a mark (staleMarkReads): its name and RESULT line,
// its database time, the paint's budget of the read it repeats (budget84Ms for an 84-day read with
// a compare range, the current window's and the one a window back, budget28Ms for the 28-day
// read), whether the cut's first paint was folded before it, and whether its items equal those of
// the read of the stored figures with no mark.
type staleRead struct {
	name, line     string
	dbMs, budgetMs float64
	painted, equal bool
}

// judgeStaleReads prints each read's line and judges it by the architect's ruling
// (dispatch://INFRA-2916/comment/813ac6b9-2c45-41b0-a5e4-b839542f0d0c, option A): a read whose
// items differ from the stored read's fails, made before the first paint or after it, since the
// stored figures and the day rows answer the same values (sameItems); a read made after the cut's
// first paint is folded fails over its budget, unless info; and a read made before it is not
// timed against the budgets, its line recording its time and node with budget_ms=none.
func judgeStaleReads(r *report, info bool, reads []staleRead) {
	for _, read := range reads {
		budget := "none (before the first paint)"
		if read.painted {
			budget = fmt.Sprintf("%.0f", read.budgetMs)
		}
		r.result("%s budget_ms=%s", read.line, budget)
		if !read.equal {
			r.fail("k16 fallback: %s answers other items than the read of the stored figures with no mark, where the day rows and the stored figures answer the same values", read.name)
		}
		if read.painted && !info && read.dbMs > read.budgetMs {
			r.fail("k16 fallback: %s, made after the cut's first paint was folded, costs %.2f ms of database time, over its budget of %.0f ms", read.name, read.dbMs, read.budgetMs)
		}
	}
}

// moveWait bounds each wait of the move (headChecks.move): for the refresher to publish a move,
// and for the refold it starts to end; movePoll is how often the move reads which has come.
const (
	moveWait = 15 * time.Minute
	movePoll = 200 * time.Millisecond
)

// move measures, for k16, on the bench node, the time from a person's move of the project's
// default repository to the end of its refold: it moves the default from the deploy repository
// (evalbench.Repos[0]) to evalbench.Repos[1] as a person does (PUT
// /api/v1/projects/{key}/default-repository), reads until the refresher has published it
// (evals.PublishedDefaultIn), in the commit that marks the cuts it moves and folds the first paint
// of each whose mark names no logged eval (evals.publishDefaults), and then until no cut stands
// marked (eval_figure_cuts_stale),
// the end of the refold the refresher runs once that page has committed (evals.refoldStaleCuts);
// then it moves the default back the same way, so the runs after this one read the default they
// read before. Each time runs from the write, and holds the refresher's wake; it is late by at
// most movePoll. It first waits for any mark the fallback or the boot left to go, so the marks it
// waits on are the move's own. A wait that ends past moveWait fails, and the move goes no further.
func (c *headChecks) move() {
	ctx := context.Background()
	from, to := evalbench.Repos[0], evalbench.Repos[1]
	published, err := evals.PublishedDefaultIn(ctx, c.pool, c.o.project)
	if err != nil {
		c.r.fail("k16 move: read %s's published default repository: %v", c.o.project, err)
		return
	}
	if published != from {
		c.r.fail("k16 move: %s publishes %q as its default repository, not %s, so no move from it is measured", c.o.project, published, from)
		return
	}
	if _, err := c.awaitNoMark(ctx); err != nil {
		c.r.fail("k16 move: before the move: %v", err)
		return
	}
	if c.moveDefault(ctx, from, to) {
		c.moveDefault(ctx, to, from)
	}
}

// moveDefault moves the project's default repository from was to now and prints the move's line
// (move), answering whether it came to the end of its refold.
func (c *headChecks) moveDefault(ctx context.Context, was, now string) bool {
	body, err := json.Marshal(map[string]any{"repo": now, "actor": map[string]string{"kind": "session", "id": "evalbench"}})
	if err != nil {
		c.r.fail("k16 move from=%s to=%s: %v", was, now, err)
		return false
	}
	started := time.Now()
	status, answer, err := putJSON(ctx, c.client, c.o.url+"/api/v1/projects/"+c.o.project+"/default-repository", c.o.token, body)
	if err != nil || status != http.StatusOK {
		c.r.fail("k16 move from=%s to=%s: the write answered %d (%v): %s", was, now, status, err, answer)
		return false
	}
	for {
		published, err := evals.PublishedDefaultIn(ctx, c.pool, c.o.project)
		if err != nil {
			c.r.fail("k16 move from=%s to=%s: read the published default repository: %v", was, now, err)
			return false
		}
		if published == now {
			break
		}
		if time.Since(started) > moveWait {
			c.r.fail("k16 move from=%s to=%s: the refresher had not published it %s after the write (it publishes %q)", was, now, moveWait, published)
			return false
		}
		time.Sleep(movePoll)
	}
	publishedAfter := time.Since(started)
	marks, err := c.awaitNoMark(ctx)
	if err != nil {
		c.r.fail("k16 move from=%s to=%s: published after %d ms: %v", was, now, publishedAfter.Milliseconds(), err)
		return false
	}
	c.r.result("k16 move from=%s to=%s published_ms=%d refold_done_ms=%d cuts_marked_seen=%d poll_ms=%d node=%s", was, now,
		publishedAfter.Milliseconds(), time.Since(started).Milliseconds(), marks, movePoll.Milliseconds(), cmp.Or(c.o.node, "unnamed"))
	return true
}

// awaitNoMark reads, every movePoll, how many cuts of the stored figures stand marked stale, until
// none does, and answers the most it read; it fails once moveWait has passed.
func (c *headChecks) awaitNoMark(ctx context.Context) (int, error) {
	started, most := time.Now(), 0
	for {
		var marks int
		if err := c.pool.QueryRow(ctx, `select count(*) from eval_figure_cuts_stale`).Scan(&marks); err != nil {
			return most, fmt.Errorf("count the stale cuts: %w", err)
		}
		most = max(most, marks)
		if marks == 0 {
			return most, nil
		}
		if time.Since(started) > moveWait {
			return most, fmt.Errorf("%d cuts still stand marked stale after %s", marks, moveWait)
		}
		time.Sleep(movePoll)
	}
}

// putJSON is the status and body of a PUT of body, JSON, with the agent token.
func putJSON(ctx context.Context, client *http.Client, target, token string, body []byte) (int, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, target, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	return response.StatusCode, answer, err
}

// sameItems reports whether two answers of the eval list hold the same items, byte for byte as
// JSON: the stored figures and the day rows answer the same values (evals.StoredFigures.SeriesValues).
func sameItems(a, b []byte) bool {
	var one, other struct {
		Items json.RawMessage `json:"items"`
	}
	if json.Unmarshal(a, &one) != nil || json.Unmarshal(b, &other) != nil {
		return false
	}
	return len(one.Items) > 0 && bytes.Equal(one.Items, other.Items)
}
