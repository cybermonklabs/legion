package evals

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// attributionDay is the day the hand-written delivery history of seedAttribution happens on.
var attributionDay = time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)

// The hand-written history's repositories: ALPHA's (the deploy repository, whose runs Dispatch
// reads), BETA's, and one repo_projects maps to no project.
const (
	alphaRepo    = "acme/alpha"
	betaRepo     = "acme/beta"
	unmappedRepo = "acme/zeta"
)

// seedAttribution writes, on attributionDay, the delivery history of k15 over three projects:
// ALPHA (acme/alpha, the deploy repository), BETA (acme/beta) and GAMMA (no repository, no row).
//
//   - acme/alpha#1 names BETA-1, so it counts for BETA although its repository is ALPHA's;
//   - acme/alpha#2 names no issue, so it counts for ALPHA through its repository;
//   - acme/beta#1 names ALPHA-1, so it counts for ALPHA although its repository is BETA's;
//   - acme/zeta#1 names no issue and its repository is mapped to no project, so it counts nowhere;
//   - deploy run 101, created after both acme/alpha merges, reaches production and ships them: it
//     counts for BETA (through acme/alpha#1's issue) and for ALPHA (through acme/alpha#2's
//     repository);
//   - deploy runs 102 (reaches production, ships nothing) and 103 (its production job fails) ship
//     none, so they count for ALPHA, the deploy repository's project;
//   - PR-checks run 201 of acme/alpha counts for ALPHA, its repository's project.
func seedAttribution(t *testing.T, r rig) delivery.DeliverySettings {
	t.Helper()
	ctx := t.Context()
	pool := r.database.Pool
	at := func(hour, minute int) time.Time {
		return attributionDay.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
	}
	exec := func(what, statement string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, statement, args...); err != nil {
			t.Fatalf("write %s: %v", what, err)
		}
	}
	actor := `{"kind":"system","id":"attribution-test"}`
	exec("the projects", `insert into projects (key, name, next_number) values ('ALPHA', 'Alpha', 2), ('BETA', 'Beta', 2), ('GAMMA', 'Gamma', 1)`)
	exec("the repositories", `insert into repo_projects (repo, project, created_by, is_default) values ($1, 'ALPHA', $3, true), ($2, 'BETA', $3, true)`,
		alphaRepo, betaRepo, actor)
	exec("the issues", `
		insert into issues (key, project_key, number, title, status, rank, created_by, created_at)
		values ('ALPHA-1', 'ALPHA', 1, 'Alpha work', 'in_progress', '00000001', $1, $2),
		       ('BETA-1', 'BETA', 1, 'Beta work', 'in_progress', '00000001', $1, $2)`, actor, attributionDay.AddDate(0, 0, -1))

	production := "production / apply"
	settings := delivery.DeliverySettings{
		DeployRepo: alphaRepo, DeployWorkflowPath: ".github/workflows/deploy.yml", ProductionJobNames: []string{production},
		PRChecksWorkflowPath: ".github/workflows/pr-checks.yml", PopulationAuthors: []string{"dev-ana"}, ExcludedRepos: []string{},
	}
	stored, err := delivery.PutSettings(ctx, pool, settings, model.Actor{Kind: "system", ID: "attribution-test"})
	if err != nil {
		t.Fatalf("write the delivery settings: %v", err)
	}
	for _, pr := range []struct {
		repo   string
		number int
		issue  *string
		merged time.Time
	}{
		{alphaRepo, 1, new("BETA-1"), at(10, 0)},
		{alphaRepo, 2, nil, at(10, 10)},
		{betaRepo, 1, new("ALPHA-1"), at(11, 0)},
		{unmappedRepo, 1, nil, at(11, 30)},
	} {
		created := pr.merged.Add(-time.Hour)
		if err := delivery.UpsertPullRequest(ctx, pool, delivery.DeliveryPullRequest{
			Repo: pr.repo, Number: pr.number, Title: fmt.Sprintf("Change %d", pr.number), URL: fmt.Sprintf("https://github.com/%s/pull/%d", pr.repo, pr.number),
			Author: "dev-ana", CreatedAt: &created, MergedAt: new(pr.merged), IssueKey: pr.issue, Sessions: []string{},
		}); err != nil {
			t.Fatalf("write %s#%d: %v", pr.repo, pr.number, err)
		}
	}
	success, failure := delivery.DeliveryJobConclusionSuccess, delivery.DeliveryJobConclusionFailure
	for _, run := range []struct {
		id         int64
		kind       delivery.DeliveryRunKind
		created    time.Time
		conclusion delivery.DeliveryRunConclusion
		job        string
		jobResult  delivery.DeliveryJobConclusion
	}{
		{101, delivery.DeliveryRunKindDeploy, at(10, 30), delivery.DeliveryRunConclusionSuccess, production, success},
		{102, delivery.DeliveryRunKindDeploy, at(12, 0), delivery.DeliveryRunConclusionSuccess, production, success},
		{103, delivery.DeliveryRunKindDeploy, at(13, 0), delivery.DeliveryRunConclusionFailure, production, failure},
		{201, delivery.DeliveryRunKindPRChecks, at(9, 0), delivery.DeliveryRunConclusionSuccess, "lint", success},
	} {
		started, completed := run.created.Add(time.Minute), run.created.Add(20*time.Minute)
		stored := delivery.DeliveryRun{
			Repo: alphaRepo, RunID: run.id, Kind: run.kind, HeadSHA: fmt.Sprintf("%040d", run.id), HeadCommitAt: run.created.Add(-time.Minute),
			StartedAt: started, CreatedAt: new(run.created), CompletedAt: &completed, Conclusion: new(run.conclusion),
			URL: fmt.Sprintf("https://github.com/%s/actions/runs/%d", alphaRepo, run.id), HeadBranch: new("main"), Event: new("push"),
		}
		if run.kind == delivery.DeliveryRunKindPRChecks {
			stored.PRNumber, stored.HeadBranch, stored.Event = new(2), new("feature/2"), new("pull_request")
		}
		if err := delivery.UpsertRun(ctx, pool, stored); err != nil {
			t.Fatalf("write run %d: %v", run.id, err)
		}
		job := delivery.DeliveryRunJob{Name: run.job, StartedAt: new(started.Add(time.Minute)), CompletedAt: new(completed.Add(-time.Minute)), Conclusion: new(run.jobResult)}
		if err := delivery.UpsertRunJobs(ctx, pool, alphaRepo, run.id, []delivery.DeliveryRunJob{job}, true); err != nil {
			t.Fatalf("write the jobs of run %d: %v", run.id, err)
		}
	}
	return stored
}

// stored reads every eval_days row of project, every series and repository, in key order.
func (r rig) stored(t *testing.T, project string) []DayRow {
	t.Helper()
	rows, err := r.database.Pool.Query(t.Context(), `
		select `+dayColumns+` from eval_days where project_key = $1 order by eval_id, series, repo, day
	`, project)
	if err != nil {
		t.Fatalf("read the day rows of %s: %v", project, err)
	}
	defer rows.Close()
	var days []DayRow
	for rows.Next() {
		day, err := scanDay(rows)
		if err != nil {
			t.Fatalf("read the day rows of %s: %v", project, err)
		}
		days = append(days, day)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the day rows of %s: %v", project, err)
	}
	return days
}

// members reads every eval_day_members row of project as `<family> <row key> <repo> <day>`.
func (r rig) members(t *testing.T, project string) []string {
	t.Helper()
	rows, err := r.database.Pool.Query(t.Context(), `
		select family || ' ' || row_key || ' ' || repo || ' ' || day::text from eval_day_members
		where project_key = $1 order by family, row_key
	`, project)
	if err != nil {
		t.Fatalf("read the members of %s: %v", project, err)
	}
	defer rows.Close()
	var members []string
	for rows.Next() {
		var member string
		if err := rows.Scan(&member); err != nil {
			t.Fatalf("read the members of %s: %v", project, err)
		}
		members = append(members, member)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the members of %s: %v", project, err)
	}
	return members
}

// rowFacts is one day row as the attribution cases state it: `<slug> <series> <repo>: n=… via=…`,
// with the value its fold reads beside them (sum for a count, num/den for a share, the samples
// for a quantile).
func rowFacts(row DayRow) string {
	slug := strings.TrimPrefix(row.EvalID, row.ProjectKey+"/")
	repo := row.Repo
	if repo == "" {
		repo = "all"
	}
	value := ""
	switch {
	case row.Num != nil:
		value = fmt.Sprintf(" share=%g/%g", *row.Num, *row.Den)
	case row.Samples != nil:
		value = fmt.Sprintf(" samples=%v", row.Samples)
	case row.Sum != nil:
		value = fmt.Sprintf(" sum=%g", *row.Sum)
	}
	if withPRs, held := row.Counts[measures.CountWithPRs]; held {
		value += fmt.Sprintf(" with_prs=%g", withPRs)
	}
	return fmt.Sprintf("%s %s %s: n=%d via=%d%s", slug, row.Series, repo, row.N, row.ViaRepository, value)
}

func allFacts(rows []DayRow) []string {
	facts := make([]string, len(rows))
	for i, row := range rows {
		facts[i] = rowFacts(row)
	}
	slices.Sort(facts)
	return facts
}

// TestEachDeliveryAndCIRowCountsForTheProjectD1Names (k15): over seedAttribution's day, a pull
// request counts for its issue's project and one with no issue for its repository's, with
// via_repository 1; the deploy that ships both counts in both; the deploys that ship none count for
// the deploy repository's project; the CI run counts for its repository's project; the pull
// request of an unmapped repository counts nowhere; and GAMMA, with nothing, gets no row. Then the
// day is written again unchanged (no row rewritten), and
// once acme/alpha#2 names BETA-1 the day's rewrite moves it, and the deploy that shipped it, out of
// ALPHA: ALPHA's acme/alpha rows of the pull-request evals, which held it alone, become zero rows
// rather than going, and its items of the day that it no longer holds are deleted; written again,
// the moved day rewrites nothing.
func TestEachDeliveryAndCIRowCountsForTheProjectD1Names(t *testing.T) {
	r := newRig(t)
	settings := seedAttribution(t, r)
	ctx := t.Context()

	read, err := r.registry.ReadDeliveryDay(ctx, settings, attributionDay)
	if err != nil {
		t.Fatalf("read the day: %v", err)
	}
	if got := read.Projects(); !slices.Equal(got, []string{"ALPHA", "BETA"}) {
		t.Fatalf("the day's rows count for %v, want ALPHA and BETA alone (acme/zeta#1 for none, GAMMA holds nothing)", got)
	}
	written, err := r.registry.WriteDeliveryDay(ctx, settings, attributionDay, "", testBuild)
	if err != nil {
		t.Fatalf("write the day: %v", err)
	}
	t.Logf("wrote the day rows of %v", written)

	// Run 101, created at 10:30, shipped at 10:49, when its production job completed; every job of
	// the history runs 17 minutes and every run 19.
	want := map[string][]string{
		"ALPHA": {
			// acme/alpha#2 (through its repository) and acme/beta#1 (through ALPHA-1).
			"merge-to-production minutes all: n=1 via=1 samples=[39]",
			"merge-to-production minutes acme/alpha: n=1 via=1 samples=[39]",
			"merge-to-production minutes acme/beta: n=0 via=0 samples=[]",
			"first-commit-to-production minutes all: n=0 via=0 samples=[]",
			"first-commit-to-production minutes acme/alpha: n=0 via=0 samples=[]",
			"first-commit-to-production minutes acme/beta: n=0 via=0 samples=[]",
			"open-to-production minutes all: n=1 via=1 samples=[99]",
			"open-to-production minutes acme/alpha: n=1 via=1 samples=[99]",
			"open-to-production minutes acme/beta: n=0 via=0 samples=[]",
			"pr-open-to-merge minutes all: n=2 via=1 samples=[60 60]",
			"pr-open-to-merge minutes acme/alpha: n=1 via=1 samples=[60]",
			"pr-open-to-merge minutes acme/beta: n=1 via=0 samples=[60]",
			"rework-share prs all: n=2 via=1 share=0/2",
			"rework-share prs acme/alpha: n=1 via=1 share=0/1",
			"rework-share prs acme/beta: n=1 via=0 share=0/1",
			// Runs 101 (through acme/alpha#2's repository), 102 and 103 (they ship none); 103 did
			// not reach production, so it is no deploy and is in deploy-run-success alone.
			"deploy-frequency deploys all: n=2 via=2 sum=2 with_prs=1",
			"deploy-frequency deploys acme/alpha: n=2 via=2 sum=2 with_prs=1",
			"deploy-run-success runs all: n=3 via=3 share=2/3",
			"deploy-run-success runs acme/alpha: n=3 via=3 share=2/3",
			// Run 201 and its one job; the deploy push runs' production jobs.
			"pr-ci minutes all: n=1 via=1 samples=[19]",
			"pr-ci minutes acme/alpha: n=1 via=1 samples=[19]",
			"pr-ci job:lint all: n=1 via=1 samples=[17]",
			"pr-ci job:lint acme/alpha: n=1 via=1 samples=[17]",
			"production-deploy minutes all: n=3 via=3 samples=[17 17 17]",
			"production-deploy minutes acme/alpha: n=3 via=3 samples=[17 17 17]",
		},
		"BETA": {
			// acme/alpha#1, through BETA-1, in its own repository's cut.
			"merge-to-production minutes all: n=1 via=0 samples=[49]",
			"merge-to-production minutes acme/alpha: n=1 via=0 samples=[49]",
			"first-commit-to-production minutes all: n=0 via=0 samples=[]",
			"first-commit-to-production minutes acme/alpha: n=0 via=0 samples=[]",
			"open-to-production minutes all: n=1 via=0 samples=[109]",
			"open-to-production minutes acme/alpha: n=1 via=0 samples=[109]",
			"pr-open-to-merge minutes all: n=1 via=0 samples=[60]",
			"pr-open-to-merge minutes acme/alpha: n=1 via=0 samples=[60]",
			"rework-share prs all: n=1 via=0 share=0/1",
			"rework-share prs acme/alpha: n=1 via=0 share=0/1",
			// Run 101, through acme/alpha#1's issue.
			"deploy-frequency deploys all: n=1 via=0 sum=1 with_prs=1",
			"deploy-frequency deploys acme/alpha: n=1 via=0 sum=1 with_prs=1",
			"deploy-run-success runs all: n=1 via=0 share=1/1",
			"deploy-run-success runs acme/alpha: n=1 via=0 share=1/1",
		},
		"GAMMA": nil,
	}
	compared := 0
	for _, project := range slices.Sorted(maps.Keys(want)) {
		got := allFacts(r.stored(t, project))
		slices.Sort(want[project])
		compared += len(got)
		if !slices.Equal(got, want[project]) {
			t.Errorf("the day rows of %s:\n%s\nwant:\n%s", project, strings.Join(got, "\n"), strings.Join(want[project], "\n"))
		}
	}
	wantMembers := map[string][]string{
		"ALPHA": {
			"ci run:acme/alpha/101 acme/alpha 2026-11-03", "ci run:acme/alpha/102 acme/alpha 2026-11-03",
			"ci run:acme/alpha/103 acme/alpha 2026-11-03", "ci run:acme/alpha/201 acme/alpha 2026-11-03",
			"delivery pull_request:acme/alpha#2 acme/alpha 2026-11-03", "delivery pull_request:acme/beta#1 acme/beta 2026-11-03",
			"delivery run:acme/alpha/101 acme/alpha 2026-11-03", "delivery run:acme/alpha/102 acme/alpha 2026-11-03",
			"delivery run:acme/alpha/103 acme/alpha 2026-11-03",
		},
		"BETA":  {"delivery pull_request:acme/alpha#1 acme/alpha 2026-11-03", "delivery run:acme/alpha/101 acme/alpha 2026-11-03"},
		"GAMMA": nil,
	}
	for _, project := range slices.Sorted(maps.Keys(wantMembers)) {
		if got := r.members(t, project); !slices.Equal(got, wantMembers[project]) {
			t.Errorf("the items %s counted: %v, want %v", project, got, wantMembers[project])
		}
	}
	if n := r.count(t, `select count(*) from eval_day_members where row_key like '%acme/zeta%'`); n != 0 {
		t.Errorf("acme/zeta#1 is counted in %d projects, want none: its repository is mapped to no project", n)
	}

	// The same day written again holds the same values: no row is rewritten.
	var newest time.Time
	if err := r.database.Pool.QueryRow(ctx, `select max(updated_at) from eval_days`).Scan(&newest); err != nil {
		t.Fatalf("read the newest write: %v", err)
	}
	if _, err := r.registry.WriteDeliveryDay(ctx, settings, attributionDay, "", testBuild); err != nil {
		t.Fatalf("write the day again: %v", err)
	}
	if rewritten := r.count(t, `select count(*) from eval_days where updated_at > $1`, newest); rewritten != 0 {
		t.Errorf("writing the unchanged day again rewrote %d rows, want none", rewritten)
	}

	// acme/alpha#2 now names BETA-1: it and run 101, which shipped it, leave ALPHA's delivery rows.
	if _, err := r.database.Pool.Exec(ctx, `update delivery_pull_requests set issue_key = 'BETA-1' where repo = $1 and number = 2`, alphaRepo); err != nil {
		t.Fatalf("move acme/alpha#2 to BETA-1: %v", err)
	}
	if _, err := r.registry.WriteDeliveryDay(ctx, settings, attributionDay, "", testBuild); err != nil {
		t.Fatalf("write the moved day: %v", err)
	}
	alpha := allFacts(r.stored(t, "ALPHA"))
	for _, zeroed := range []string{"rework-share prs acme/alpha: n=0 via=0 share=0/0", "merge-to-production minutes acme/alpha: n=0 via=0 samples=[]"} {
		if !slices.Contains(alpha, zeroed) {
			t.Errorf("ALPHA's rows after acme/alpha's last pull request moved to BETA lack the zero row %q:\n%s", zeroed, strings.Join(alpha, "\n"))
		}
	}
	if err := r.database.Pool.QueryRow(ctx, `select max(updated_at) from eval_days`).Scan(&newest); err != nil {
		t.Fatalf("read the newest write: %v", err)
	}
	if _, err := r.registry.WriteDeliveryDay(ctx, settings, attributionDay, "", testBuild); err != nil {
		t.Fatalf("write the moved day again: %v", err)
	}
	if rewritten := r.count(t, `select count(*) from eval_days where updated_at > $1`, newest); rewritten != 0 {
		t.Errorf("writing the moved day again rewrote %d rows, want none: a zero row already zero is left as it is", rewritten)
	}
	for _, kept := range []string{
		"deploy-frequency deploys all: n=1 via=1 sum=1 with_prs=0", "rework-share prs all: n=1 via=0 share=0/1",
		"production-deploy minutes all: n=3 via=3 samples=[17 17 17]",
	} {
		if !slices.Contains(alpha, kept) {
			t.Errorf("ALPHA's rows after the move lack %q:\n%s", kept, strings.Join(alpha, "\n"))
		}
	}
	if got := r.members(t, "ALPHA"); slices.Contains(got, "delivery pull_request:acme/alpha#2 acme/alpha 2026-11-03") ||
		slices.Contains(got, "delivery run:acme/alpha/101 acme/alpha 2026-11-03") || !slices.Contains(got, "ci run:acme/alpha/101 acme/alpha 2026-11-03") {
		t.Errorf("ALPHA's items after the move: %v, want acme/alpha#2 and run 101 gone from delivery and run 101 kept in ci", got)
	}
	if got := r.members(t, "BETA"); !slices.Contains(got, "delivery pull_request:acme/alpha#2 acme/alpha 2026-11-03") {
		t.Errorf("BETA's items after the move: %v, want acme/alpha#2 among them", got)
	}
	t.Logf("compared %d day rows of three projects; after acme/alpha#2 moved to BETA, ALPHA holds %d", compared, len(alpha))
}

// TestTheZeroDayIsTheBuildersDayWithNoRow: ZeroDeliveryDay holds the all-repositories row of every
// delivery and CI series (the staging cards only where the settings name their jobs), each n 0
// and its value zero, and WithZeros adds a zero row only where the day holds none of that series.
func TestTheZeroDayIsTheBuildersDayWithNoRow(t *testing.T) {
	day := attributionDay
	settings := delivery.DeliverySettings{DeployRepo: alphaRepo, ProductionJobNames: []string{"production / apply"}, StagingE2EJobName: new("staging-e2e / run")}
	zero := ZeroDeliveryDay(settings, "ALPHA", day)
	want := []string{
		"deploy-frequency deploys all: n=0 via=0 sum=0 with_prs=0",
		"deploy-run-success runs all: n=0 via=0 share=0/0",
		"first-commit-to-production minutes all: n=0 via=0 samples=[]",
		"merge-to-production minutes all: n=0 via=0 samples=[]",
		"open-to-production minutes all: n=0 via=0 samples=[]",
		"pr-ci minutes all: n=0 via=0 samples=[]",
		"pr-open-to-merge minutes all: n=0 via=0 samples=[]",
		"production-deploy minutes all: n=0 via=0 samples=[]",
		"rework-share prs all: n=0 via=0 share=0/0",
		"staging-e2e minutes all: n=0 via=0 samples=[]",
	}
	if got := allFacts(zero.Rows); !slices.Equal(got, want) {
		t.Errorf("the zero day:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	built := ProjectDay{Project: "ALPHA", Day: day, Rows: []DayRow{{
		ProjectKey: "ALPHA", EvalID: EvalID("ALPHA", SlugDeployFrequency), Series: measures.SeriesDeploys, Day: day, Listed: true, N: 4, Sum: new(4.0),
	}}}
	merged := built.WithZeros(zero.Rows)
	if len(merged.Rows) != len(zero.Rows) || !slices.Contains(allFacts(merged.Rows), "deploy-frequency deploys all: n=4 via=0 sum=4") {
		t.Errorf("the day with zeros:\n%s\nwant the built deploy-frequency row and a zero row of every other series", strings.Join(allFacts(merged.Rows), "\n"))
	}
}

// TestTheUnownedP0RowIsTheCountAtItsLastWrite: WriteUnownedP0Day writes the project's count of open
// P0 issues with no claim and no route as the day row's last value, at the write's time; a second
// write after one more such issue opens moves the value and the time.
func TestTheUnownedP0RowIsTheCountAtItsLastWrite(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	actor := `{"kind":"system","id":"p0-test"}`
	insert := func(number int, status string, claimed, routed bool) {
		t.Helper()
		var claimedBy, claimedAt, route any
		if claimed {
			claimedBy, claimedAt = actor, attributionDay
		}
		if routed {
			route = "triage"
		}
		if _, err := r.database.Pool.Exec(ctx, `
			insert into issues (key, project_key, number, title, status, rank, priority, created_by, created_at, claimed_by, claimed_at, route)
			values ($1, 'EVAL', $2, 'Incident', $3, $4, 0, $5, $6, $7, $8, $9)
		`, fmt.Sprintf("EVAL-%d", number), number, status, fmt.Sprintf("%08d", number), actor, attributionDay, claimedBy, claimedAt, route); err != nil {
			t.Fatalf("write P0 issue %d: %v", number, err)
		}
	}
	insert(1, "done", false, false)
	insert(2, "in_progress", true, false)
	insert(3, "triage", false, true)
	insert(4, "triage", false, false)
	if err := r.registry.WriteUnownedP0Day(ctx, "EVAL", attributionDay, testBuild); err != nil {
		t.Fatalf("write the unowned-p0 row: %v", err)
	}
	first := r.day(t, EvalID("EVAL", SlugUnownedP0), SeriesUnownedP0, "", attributionDay)
	if first.LastValue == nil || *first.LastValue != 1 || first.LastAt == nil || first.BuiltBy != testBuild || !first.Listed {
		t.Fatalf("the unowned-p0 row = %+v, want last_value 1 (only EVAL-4 is open, unclaimed and unrouted) at the write's time", first)
	}
	insert(5, "triage", false, false)
	if err := r.registry.WriteUnownedP0Day(ctx, "EVAL", attributionDay, testBuild); err != nil {
		t.Fatalf("write the unowned-p0 row again: %v", err)
	}
	second := r.day(t, EvalID("EVAL", SlugUnownedP0), SeriesUnownedP0, "", attributionDay)
	if second.LastValue == nil || *second.LastValue != 2 || second.LastAt == nil || !second.LastAt.After(*first.LastAt) {
		t.Fatalf("the rewritten row = last_value %s at %v, want 2 after %v", showValue(second.LastValue), second.LastAt, first.LastAt)
	}
	t.Logf("unowned-p0 of EVAL on %s: %g at %s, then %g at %s", attributionDay.Format(time.DateOnly),
		*first.LastValue, first.LastAt.Format(time.RFC3339Nano), *second.LastValue, second.LastAt.Format(time.RFC3339Nano))
}
