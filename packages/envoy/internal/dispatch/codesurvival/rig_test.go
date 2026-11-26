package codesurvival

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/codesurvival/codesurvivaltest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival/fold"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// testBuild is the build the run's tests write their day rows as.
const testBuild = "codesurvival-test"

// The groups most of the run's tests give a project: the agents, by a head-branch prefix and a
// login, and every other change after them.
var testGroups = []delivery.CodeGroup{
	{Name: "agents", BranchPrefixes: []string{"agent/"}, AuthorLogins: []string{"agent-app[bot]"}},
	{Name: "others"},
}

// testClock is the clock a rig's run and regroup read, which the test moves.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// syncBuffer is a log the run writes from its own goroutine and a test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// lines is every JSON line logged with message msg, decoded.
func (b *syncBuffer) lines(t *testing.T, msg string) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var found []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode the log line %q: %v", line, err)
		}
		if record["msg"] == msg {
			found = append(found, record)
		}
	}
	return found
}

// rig is one test's Dispatch: its own database with the eval registry, the GitHub double, and the
// run and the regroup over them, each on the rig's clock.
type rig struct {
	t        *testing.T
	ctx      context.Context
	database *store.Store
	registry *evals.Store
	github   *codesurvivaltest.GitHub
	clock    *testClock
	logs     *syncBuffer
	run      *Run
	regroup  *Regroup
}

func newRig(t *testing.T) *rig {
	t.Helper()
	database := storetest.Open(t)
	broker := events.NewBroker()
	// Settlement is an hour away, so no background settlement writes while a test reads.
	documents := docs.New(docs.Deps{Store: database, Events: broker, Settle: time.Hour})
	t.Cleanup(func() {
		if err := documents.Shutdown(context.Background()); err != nil {
			t.Errorf("shut down the document service: %v", err)
		}
	})
	attributor := delivery.NewAttributor(refs.Renames{})
	github := codesurvivaltest.New(t)
	r := &rig{t: t, ctx: testwait.Context(t), database: database, registry: evals.New(database, documents, broker, attributor),
		github: github, clock: &testClock{}, logs: &syncBuffer{}}
	logger := slog.New(slog.NewJSONHandler(r.logs, nil))
	t.Cleanup(func() {
		if t.Failed() {
			r.logs.mu.Lock()
			defer r.logs.mu.Unlock()
			t.Logf("the run's log:\n%s", r.logs.buf.String())
		}
	})
	r.run = &Run{Pool: database.Pool, Client: github.Client(t), Attributor: attributor, Build: testBuild, Now: r.clock.Now, Logger: logger}
	r.regroup = &Regroup{Pool: database.Pool, Build: testBuild, Now: r.clock.Now, Logger: logger}
	return r
}

// project makes project key, with defaultRepo as its default repository ("" for none) and groups
// as its code groups (none for nil), and seeds its evals.
func (r *rig) project(key, defaultRepo string, groups []delivery.CodeGroup) {
	r.t.Helper()
	var repo any
	if defaultRepo != "" {
		repo = defaultRepo
	}
	if _, err := r.database.Pool.Exec(r.ctx, `insert into projects (key, name, default_assignee, default_repo) values ($1, $1, 'alice', $2)`, key, repo); err != nil {
		r.t.Fatalf("make the project %s: %v", key, err)
	}
	if groups != nil {
		r.saveGroups(key, groups)
	}
	if _, err := r.registry.Ensure(r.ctx, key); err != nil {
		r.t.Fatalf("seed the evals of %s: %v", key, err)
	}
}

// saveGroups saves groups as key's code groups, as the settings route saves them.
func (r *rig) saveGroups(key string, groups []delivery.CodeGroup) {
	r.t.Helper()
	tx, err := r.database.Pool.Begin(r.ctx)
	if err != nil {
		r.t.Fatalf("begin the settings of %s: %v", key, err)
	}
	defer tx.Rollback(r.ctx)
	if _, err := delivery.PutProjectSettings(r.ctx, tx, delivery.ProjectSettingsInput{Project: key, CodeGroups: &groups},
		model.Actor{Kind: "system", ID: "codesurvival-test"}); err != nil {
		r.t.Fatalf("save the code groups of %s: %v", key, err)
	}
	if err := tx.Commit(r.ctx); err != nil {
		r.t.Fatalf("commit the settings of %s: %v", key, err)
	}
}

// issue makes the issue <project>-<number>, with repo as its own repository ("" for none), and
// answers its key.
func (r *rig) issue(project string, number int, repo string) string {
	r.t.Helper()
	key := fmt.Sprintf("%s-%d", project, number)
	var own any
	if repo != "" {
		own = repo
	}
	if _, err := r.database.Pool.Exec(r.ctx, `
		insert into issues (key, project_key, number, title, status, created_by, rank, repo)
		values ($1, $2, $3, $1, 'todo', '{"kind":"system","id":"test"}'::jsonb, $1, $4)`, key, project, number, own); err != nil {
		r.t.Fatalf("make the issue %s: %v", key, err)
	}
	return key
}

// passAt makes one run pass with the clock at now, and answers the requests the double was sent
// during it.
func (r *rig) passAt(now time.Time) []codesurvivaltest.Request {
	r.t.Helper()
	r.clock.set(now)
	r.github.Forget()
	r.run.Pass(r.ctx)
	return r.github.Requests()
}

// repo is repo's row of code_repos, failing the test where there is none.
func (r *rig) repo(repo string) Repo {
	r.t.Helper()
	row, found, err := GetRepo(r.ctx, r.database.Pool, repo)
	if err != nil || !found {
		r.t.Fatalf("the row of %s: found %t, %v", repo, found, err)
	}
	return row
}

// days is the state of each stored day of repo, by day.
func (r *rig) days(repo string) map[time.Time]survival.DayState {
	r.t.Helper()
	statuses, err := DayStatuses(r.ctx, r.database.Pool, repo, time.Time{}, time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		r.t.Fatalf("the days of %s: %v", repo, err)
	}
	states := map[time.Time]survival.DayState{}
	for _, status := range statuses {
		states[status.Day] = status.State
	}
	return states
}

// measuredAt is when each day of repo that holds a measurement was measured, by day.
func (r *rig) measuredAt(repo string) map[time.Time]time.Time {
	r.t.Helper()
	rows, err := r.database.Pool.Query(r.ctx, `select day, measured_at from code_days where repo = $1 and measured_at is not null`, repo)
	if err != nil {
		r.t.Fatalf("the measured days of %s: %v", repo, err)
	}
	at := map[time.Time]time.Time{}
	var day, measured time.Time
	if _, err := pgx.ForEachRow(rows, []any{&day, &measured}, func() error {
		at[day.UTC()] = measured.UTC()
		return nil
	}); err != nil {
		r.t.Fatalf("the measured days of %s: %v", repo, err)
	}
	return at
}

// dump is every row of tables as Postgres writes it as text, in order.
func (r *rig) dump(tables ...string) []string {
	r.t.Helper()
	var rows []string
	for _, table := range tables {
		found, err := r.database.Pool.Query(r.ctx, `select '`+table+`: ' || t::text from `+table+` t order by 1`)
		if err != nil {
			r.t.Fatalf("read %s: %v", table, err)
		}
		listed, err := pgx.CollectRows(found, pgx.RowTo[string])
		if err != nil {
			r.t.Fatalf("read %s: %v", table, err)
		}
		rows = append(rows, listed...)
	}
	return rows
}

// evalRows is every stored row of project's code evals, as text, in order.
func (r *rig) evalRows(project string) []string {
	r.t.Helper()
	found, err := r.database.Pool.Query(r.ctx, `
		select concat_ws(' ', eval_id, repo, series, day, n, num, den, counts::text, via_repository, xmin::text)
		from eval_days where project_key = $1 and eval_id = any($2) order by 1`, project, codeEvalIDs(project))
	if err != nil {
		r.t.Fatalf("read the code rows of %s: %v", project, err)
	}
	rows, err := pgx.CollectRows(found, pgx.RowTo[string])
	if err != nil {
		r.t.Fatalf("read the code rows of %s: %v", project, err)
	}
	return rows
}

// evalDays is every day project holds a row of either code eval of, in order.
func (r *rig) evalDays(project string) []time.Time {
	r.t.Helper()
	found, err := r.database.Pool.Query(r.ctx, `
		select distinct day from eval_days where project_key = $1 and eval_id = any($2) order by 1`, project, codeEvalIDs(project))
	if err != nil {
		r.t.Fatalf("read the code days of %s: %v", project, err)
	}
	days, err := pgx.CollectRows(found, func(row pgx.CollectableRow) (time.Time, error) {
		var day time.Time
		err := row.Scan(&day)
		return day.UTC(), err
	})
	if err != nil {
		r.t.Fatalf("read the code days of %s: %v", project, err)
	}
	return days
}

// window is project's parts of the two code evals over the days of [from, to), as the list read
// reads them (evals.DayWindowIn), in the project's own cut, or repo's where repo is named.
func (r *rig) window(project string, from, to time.Time, repos ...string) (survivalParts, reworkParts []measures.DayPart) {
	r.t.Helper()
	page, err := r.registry.Current(r.ctx, []string{project}, "", 200, 0)
	if err != nil {
		r.t.Fatalf("read the evals of %s: %v", project, err)
	}
	entries := slices.DeleteFunc(page.Items, func(entry evals.Entry) bool {
		return !slices.Contains(codeEvalIDs(project), entry.Definition.EvalID)
	})
	if len(entries) != 2 {
		r.t.Fatalf("%s holds %d code evals, want 2", project, len(entries))
	}
	rows, err := evals.DayWindowIn(r.ctx, r.database.Pool, []string{project}, entries, nil, repos, evals.Range{From: from, To: to}, nil)
	if err != nil {
		r.t.Fatalf("read the code rows of %s over %s..%s: %v", project, from.Format(time.DateOnly), to.Format(time.DateOnly), err)
	}
	for _, row := range rows {
		switch row.EvalID {
		case evals.EvalID(project, evals.SlugCodeSurvival):
			survivalParts = append(survivalParts, row.Part)
		case evals.EvalID(project, evals.SlugPostMergeRework):
			reworkParts = append(reworkParts, row.Part)
		}
	}
	return survivalParts, reworkParts
}

// figures is a window's survival figures of group, its rework figures of group, and its status.
type figures struct {
	survival, rework fold.GroupFigures
	status           fold.Status
}

// read is project's figures of group over [from, to), folded as the evals' sources fold them.
func (r *rig) read(project, group string, from, to time.Time, repos ...string) figures {
	r.t.Helper()
	survivalParts, reworkParts := r.window(project, from, to, repos...)
	return figures{
		survival: fold.FoldSurvival(survivalParts, fold.GroupSeries(fold.SeriesSurvival, group)),
		rework:   fold.FoldRework(reworkParts, fold.GroupSeries(fold.SeriesRework, group)),
		status:   fold.FoldStatus(survivalParts, from, to),
	}
}

// value is a figure as a failure states it.
func value(figure *float64) string {
	if figure == nil {
		return "no data"
	}
	return fmt.Sprintf("%g", *figure)
}

// patchReads is how many times each commit's patch was read through either diff route among
// requests, by SHA.
func patchReads(requests []codesurvivaltest.Request) map[string]int {
	reads := map[string]int{}
	for _, request := range requests {
		if sha, ok := request.Patch(); ok {
			reads[sha]++
		}
	}
	return reads
}

// routeOf is the route of the run a request is one of, "" for none.
func routeOf(request codesurvivaltest.Request) string {
	parts := strings.Split(strings.TrimPrefix(request.Path, "/"), "/")
	switch {
	case request.Method == "POST" && len(parts) == 4 && parts[0] == "app" && parts[1] == "installations" && parts[3] == "access_tokens":
		return "token"
	case request.Method != "GET" || len(parts) < 3 || parts[0] != "repos":
		return ""
	}
	rest := parts[3:]
	switch {
	case len(rest) == 0:
		return "repository"
	case len(rest) == 1 && rest[0] == "installation":
		return "installation"
	case len(rest) == 4 && rest[0] == "git" && rest[1] == "ref" && rest[2] == "heads":
		return "branch"
	case len(rest) == 1 && rest[0] == "commits" && request.Query.Get("since") != "" && request.Query.Get("per_page") == "100":
		return "chain"
	case len(rest) == 2 && rest[0] == "commits" && request.Accept == codesurvivaltest.DiffMediaType && len(request.Query) == 0:
		return "diff"
	case len(rest) == 2 && rest[0] == "commits" && request.Query.Get("per_page") == "100" && request.Query.Get("page") != "":
		return "files"
	case len(rest) == 2 && rest[0] == "compare" && request.Accept == codesurvivaltest.DiffMediaType:
		return "compare"
	case len(rest) == 1 && rest[0] == "pulls" && request.Query.Get("state") == "closed" && request.Query.Get("sort") == "updated" &&
		request.Query.Get("direction") == "desc" && request.Query.Get("per_page") == "100":
		return "pulls"
	case len(rest) == 3 && rest[0] == "pulls" && rest[2] == "commits":
		return "attribution"
	}
	return ""
}
