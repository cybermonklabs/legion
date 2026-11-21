package evals

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/explaintest"
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

// seedAttribution writes, on attributionDay, the delivery history of the attribution rule over
// three projects: ALPHA (acme/alpha, the deploy repository), BETA (acme/beta) and GAMMA (no
// repository, no row).
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
	exec("the projects", `insert into projects (key, name, default_assignee, next_number) values ('ALPHA', 'Alpha', 'alice', 2), ('BETA', 'Beta', 'alice', 2), ('GAMMA', 'Gamma', 'alice', 1)`)
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

// members reads every eval_day_members row of project as `<family> <row key> <repo> <day>`, and
// ` as <deploy repo>` after it for an item counted under a deploy repository (deploy_repos.go).
func (r rig) members(t *testing.T, project string) []string {
	t.Helper()
	rows, err := r.database.Pool.Query(t.Context(), `
		select family || ' ' || row_key || ' ' || repo || ' ' || day::text || case when deploy_repo <> '' then ' as ' || deploy_repo else '' end
		from eval_day_members where project_key = $1 order by family, row_key, deploy_repo
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

// dayProjects is every project a row of read counts for, by key.
func dayProjects(read deliveryRead) []string {
	projects := map[string]bool{}
	for _, read := range read.days {
		for _, pr := range read.prs {
			projects[pr.project] = true
		}
		for _, deploy := range read.deploys {
			for _, c := range deploy.projects {
				projects[c.project] = true
			}
		}
		for _, run := range slices.Concat(read.checks, read.pushes) {
			projects[run.project] = true
		}
	}
	delete(projects, "")
	return slices.Sorted(maps.Keys(projects))
}

// refreshDays writes days, in order, as a pass of the refresher's walks writes them, each day one
// page of the pass, under testBuild as of the end of the last day: each day's delivery and CI rows
// of every project (refresh.deliveryWalkDay), where delivery settings exist, and each day's Legion
// rows of each project of legion (refresh.writeLegion), each with the zero rows of the series
// begun before it. The pass first loads what a pass loads, its seed of every project no seed has
// reached included (refresh.load). It answers the days written.
func (r rig) refreshDays(t *testing.T, legion []string, days ...time.Time) []string {
	t.Helper()
	ctx := t.Context()
	pass := r.registry.newRefresh(testBuild, days[len(days)-1].AddDate(0, 0, 1).Add(-time.Minute), walkBatch)
	if err := pass.load(ctx, &seeding{everyProject: true}); err != nil {
		t.Fatalf("load the refresher's pass: %v", err)
	}
	pass.walking = true
	for _, day := range days {
		pass.beginPage()
		if pass.settings != nil {
			if err := pass.deliveryWalkDay(ctx, day); err != nil {
				t.Fatalf("write the delivery rows of %s: %v", day.Format(time.DateOnly), err)
			}
		}
		for _, project := range legion {
			if err := pass.writeLegion(ctx, project, day, false); err != nil {
				t.Fatalf("write the Legion rows of %s on %s: %v", project, day.Format(time.DateOnly), err)
			}
		}
	}
	return pass.report.sorted()
}

// TestEachDeliveryAndCIRowCountsForTheProjectItIsAttributedTo: over seedAttribution's day, written
// as the refresher's walk writes it, a pull request counts for its issue's project and one with no
// issue for its repository's, with via_repository 1; the deploy that ships both counts in both;
// the deploys that ship none count for the deploy repository's project; the CI run counts for its
// repository's project; the pull request of an unmapped repository counts nowhere; and GAMMA, with
// nothing, gets no row. Then the day is written again unchanged (no row rewritten), and once
// acme/alpha#2 names BETA-1 the day's rewrite moves it, and the deploy that shipped it, out of
// ALPHA: ALPHA's acme/alpha rows of the pull-request evals, which held it alone, become zero rows
// rather than going, and its items of the day that it no longer holds are deleted; written again,
// the moved day rewrites nothing.
func TestEachDeliveryAndCIRowCountsForTheProjectItIsAttributedTo(t *testing.T) {
	r := newRig(t)
	settings := seedAttribution(t, r)
	ctx := t.Context()

	read, err := r.registry.readDeliveryDay(ctx, settings, []keptRepo{{name: alphaRepo, deploys: true}}, attributionDay, nil, nil)
	if err != nil {
		t.Fatalf("read the day: %v", err)
	}
	if got := dayProjects(read); !slices.Equal(got, []string{"ALPHA", "BETA"}) {
		t.Fatalf("the day's rows count for %v, want ALPHA and BETA alone (acme/zeta#1 for none, GAMMA holds nothing)", got)
	}
	t.Logf("wrote %v", r.refreshDays(t, nil, attributionDay))

	// Run 101, created at 10:30, shipped at 10:49, when its production job completed; every job of
	// the history runs 17 minutes and every run 19.
	want := map[string][]string{
		"ALPHA": {
			// acme/alpha#2 (through its repository) and acme/beta#1 (through ALPHA-1).
			"merge-to-production minutes all: n=1 via=1 samples=[39]",
			"merge-to-production minutes acme/alpha: n=1 via=1 samples=[39]",
			"merge-to-production minutes acme/beta: n=0 via=0 samples=[]",
			// No pull request records a first commit, so first-commit-to-production times none and,
			// before its first day, holds no row (dropUnshipped).
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
	// acme/alpha's runs make it the one repository whose rows of the evals that read the deploy
	// repository are kept: those evals' items are counted under it, and the pull requests once more
	// under none, as pr-open-to-merge and rework-share count them.
	wantMembers := map[string][]string{
		"ALPHA": {
			"ci run:acme/alpha/101 acme/alpha 2026-11-03 as acme/alpha", "ci run:acme/alpha/102 acme/alpha 2026-11-03 as acme/alpha",
			"ci run:acme/alpha/103 acme/alpha 2026-11-03 as acme/alpha", "ci run:acme/alpha/201 acme/alpha 2026-11-03 as acme/alpha",
			"delivery pull_request:acme/alpha#2 acme/alpha 2026-11-03", "delivery pull_request:acme/alpha#2 acme/alpha 2026-11-03 as acme/alpha",
			"delivery pull_request:acme/beta#1 acme/beta 2026-11-03", "delivery pull_request:acme/beta#1 acme/beta 2026-11-03 as acme/alpha",
			"delivery run:acme/alpha/101 acme/alpha 2026-11-03 as acme/alpha", "delivery run:acme/alpha/102 acme/alpha 2026-11-03 as acme/alpha",
			"delivery run:acme/alpha/103 acme/alpha 2026-11-03 as acme/alpha",
		},
		"BETA": {
			"delivery pull_request:acme/alpha#1 acme/alpha 2026-11-03", "delivery pull_request:acme/alpha#1 acme/alpha 2026-11-03 as acme/alpha",
			"delivery run:acme/alpha/101 acme/alpha 2026-11-03 as acme/alpha",
		},
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
	r.refreshDays(t, nil, attributionDay)
	if rewritten := r.count(t, `select count(*) from eval_days where updated_at > $1`, newest); rewritten != 0 {
		t.Errorf("writing the unchanged day again rewrote %d rows, want none", rewritten)
	}

	// acme/alpha#2 now names BETA-1: it and run 101, which shipped it, leave ALPHA's delivery rows.
	if _, err := r.database.Pool.Exec(ctx, `update delivery_pull_requests set issue_key = 'BETA-1' where repo = $1 and number = 2`, alphaRepo); err != nil {
		t.Fatalf("move acme/alpha#2 to BETA-1: %v", err)
	}
	r.refreshDays(t, nil, attributionDay)
	alpha := allFacts(r.stored(t, "ALPHA"))
	for _, zeroed := range []string{"rework-share prs acme/alpha: n=0 via=0 share=0/0", "merge-to-production minutes acme/alpha: n=0 via=0 samples=[]"} {
		if !slices.Contains(alpha, zeroed) {
			t.Errorf("ALPHA's rows after acme/alpha's last pull request moved to BETA lack the zero row %q:\n%s", zeroed, strings.Join(alpha, "\n"))
		}
	}
	if err := r.database.Pool.QueryRow(ctx, `select max(updated_at) from eval_days`).Scan(&newest); err != nil {
		t.Fatalf("read the newest write: %v", err)
	}
	r.refreshDays(t, nil, attributionDay)
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
		slices.Contains(got, "delivery pull_request:acme/alpha#2 acme/alpha 2026-11-03 as acme/alpha") ||
		slices.Contains(got, "delivery run:acme/alpha/101 acme/alpha 2026-11-03 as acme/alpha") ||
		!slices.Contains(got, "ci run:acme/alpha/101 acme/alpha 2026-11-03 as acme/alpha") {
		t.Errorf("ALPHA's items after the move: %v, want acme/alpha#2 and run 101 gone from delivery and run 101 kept in ci", got)
	}
	if got := r.members(t, "BETA"); !slices.Contains(got, "delivery pull_request:acme/alpha#2 acme/alpha 2026-11-03") ||
		!slices.Contains(got, "delivery pull_request:acme/alpha#2 acme/alpha 2026-11-03 as acme/alpha") {
		t.Errorf("BETA's items after the move: %v, want acme/alpha#2 among them, under no deploy repository and under acme/alpha", got)
	}
	t.Logf("compared %d day rows of three projects; after acme/alpha#2 moved to BETA, ALPHA holds %d", compared, len(alpha))
}

// TestTheZeroDayIsTheBuildersDayWithNoRow: ZeroDeliveryDay holds the all-repositories row of every
// delivery and CI series (the staging cards only where the settings name their jobs), each n 0
// and its value zero, those of the evals that read the deploy repository keyed by the repository
// kept, and WithZeros adds a zero row only where the day holds none of that series.
func TestTheZeroDayIsTheBuildersDayWithNoRow(t *testing.T) {
	day := attributionDay
	settings := delivery.DeliverySettings{DeployRepo: alphaRepo, ProductionJobNames: []string{"production / apply"}, StagingE2EJobName: new("staging-e2e / run")}
	zero := ZeroDeliveryDay(settings, "ALPHA", day, []string{alphaRepo})
	for _, row := range zero.Rows {
		want := ""
		if readsDeployRepo(strings.TrimPrefix(row.EvalID, "ALPHA/")) {
			want = alphaRepo
		}
		if row.DeployRepo != want {
			t.Errorf("the zero row of %s is of deploy repository %q, want %q", row.EvalID, row.DeployRepo, want)
		}
	}
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
		ProjectKey: "ALPHA", EvalID: EvalID("ALPHA", SlugDeployFrequency), DeployRepo: alphaRepo, Listed: true,
		DayPart: measures.DayPart{Series: measures.SeriesDeploys, Day: day, N: 4, Sum: new(4.0)},
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

// TestEachFilterOfWhatADayNoLongerHoldsReadsItsListOnce: each statement that finds what a day no
// longer holds against the list its write gives it (leftDaysOwedStatement's and
// goneMembersDeleteStatement's items, zeroStaleDaysStatement's and zeroStaleSettingsDaysStatement's
// rows, and standingPartsQuery's readings less the write's own) reads that list once, with the
// write's values and under the generic plan a statement pgx caches can settle on after its fifth
// run, which estimates every list at 10 rows: the list runs under no SubPlan, no nested loop runs
// it again for each row of its other side, and the filter is a SetOp Except over the list or a
// hash or merge anti-join. standingPartsQuery's generic plan also matches each reading to its keys
// by their day, as a hash or merge join on it or an index condition, rather than testing each
// reading against every key of its series. The day is past work_mem, as one of INFRA's is: 80,000
// items across 40 deploy repositories and 80,000 rows written through WriteProjectDay, and 80,000
// readings of a logged eval on the same day, the three tables analyzed. Each statement is
// explained with the arrays a second write of the day sends, one item, one row and one reading
// fewer than stored, in a transaction prepared as its write prepares it, and then with literals
// for its generic plan, which reads none of them.
//
// The test judges the plan and not the clock, which a loaded runner would fail; k20 measures the
// write's time on the build farm. That measurement is what this test stands for: written as
// `not in (…)`, the filters ran the list again for each stored row once it outgrew work_mem, so
// one day's write of k20's load at INFRA's size held eval_days for 13 minutes, and on the test
// Postgres a day of 80,000 items took more than 180 s, where an anti-join took 1.6 s; and written
// as `not exists`, zeroStaleDays' generic plan was a nested-loop anti-join that had not written
// 80,000 rows after 2 minutes.
func TestEachFilterOfWhatADayNoLongerHoldsReadsItsListOnce(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	day := attributionDay
	const items = 80000
	deployRepos := make([]string, 40)
	for i := range deployRepos {
		deployRepos[i] = fmt.Sprintf("acme/deploy-%02d", i)
	}
	members, rows := make([]Member, items), make([]DayRow, items)
	for i := range items {
		members[i] = Member{Family: KindDelivery, DeployRepo: deployRepos[i%40], RowKey: PullRequestMember("acme/web", i+1), Repo: "acme/web"}
		rows[i] = DayRow{
			ProjectKey: "EVAL", EvalID: EvalID("EVAL", SlugDeployFrequency), DeployRepo: deployRepos[i%40],
			Repo: fmt.Sprintf("acme/repo-%05d", i), Listed: true,
			DayPart: measures.DayPart{Series: "deploys", Day: day, N: 1, Sum: new(1.0)},
		}
	}
	if err := r.registry.WriteProjectDay(ctx, ProjectDay{Project: "EVAL", Day: day, Rows: rows, Members: members}, testBuild); err != nil {
		t.Fatalf("write a day of %d items and %d rows: %v", items, items, err)
	}
	// The readings: the write that registers the logged eval, and the day's other readings
	// stored beside its one.
	r.write(t, "latency", alice, row(t, map[string]any{"series": "minutes", "row_kind": "run", "row_key": "first", "value": 1, "measured_at": day.Add(23 * time.Hour)}))
	evalID := EvalID(r.project, "latency")
	if _, err := r.database.Pool.Exec(ctx, `
		insert into eval_readings (eval_id, series, row_kind, row_key, value, measured_at, recorded_by)
		select $1, 'minutes', 'run', 'run-' || n, n % 97 + 1, $2::timestamptz + make_interval(secs => n), '{"kind": "user", "id": "alice"}'
		from generate_series(1, $3::int) as n
	`, evalID, day, items); err != nil {
		t.Fatalf("store %d readings of %s: %v", items, evalID, err)
	}
	for _, table := range []string{"eval_days", "eval_day_members", "eval_readings"} {
		if _, err := r.database.Pool.Exec(ctx, "analyze "+table); err != nil {
			t.Fatalf("analyze %s: %v", table, err)
		}
	}
	stored, err := r.database.Pool.Query(ctx, `select id::text from eval_readings where eval_id = $1 and row_key like 'run-%' order by id`, evalID)
	if err != nil {
		t.Fatalf("read the ids of %s's readings: %v", evalID, err)
	}
	ids, err := pgx.CollectRows(stored, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the ids of %s's readings: %v", evalID, err)
	}

	// The arrays a second write of the day sends, holding one item, one row and one reading fewer.
	again, kept, written := members[1:], rows[1:], ids[1:]
	families, memberDeployRepos, keys, shippedBy := make([]string, len(again)), make([]string, len(again)), make([]string, len(again)), make([]string, len(again))
	for i, member := range again {
		families[i], memberDeployRepos[i], keys[i], shippedBy[i] = member.Family, member.DeployRepo, member.RowKey, member.ShippedBy
	}
	projects, evals, rowDeployRepos := make([]string, len(kept)), make([]string, len(kept)), make([]string, len(kept))
	series, repos, days := make([]string, len(kept)), make([]string, len(kept)), make([]time.Time, len(kept))
	for i, row := range kept {
		projects[i], evals[i], rowDeployRepos[i] = row.ProjectKey, row.EvalID, row.DeployRepo
		series[i], repos[i], days[i] = row.Series, row.Repo, row.Day
	}
	evalIDs := make([]string, len(deliveryDaySlugs))
	for i, slug := range deliveryDaySlugs {
		evalIDs[i] = EvalID("EVAL", slug)
	}
	standingKey := dayKey{series: "minutes", day: day}
	// The standing readings are the one stored reading the write does not name, and the
	// registering write's.
	tx, err := r.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the standing readings' read: %v", err)
	}
	standing, err := standingParts(ctx, tx, evalID, []dayKey{standingKey}, written)
	_ = tx.Rollback(ctx)
	if err != nil {
		t.Fatalf("standingParts: %v", err)
	}
	if got := len(standing[standingKey]); got != 2 {
		t.Errorf("standingParts answers %d standing readings of %s on %s, want 2: the stored reading the write does not name, and the registering write's",
			got, evalID, day.Format(time.DateOnly))
	}

	date := "'" + day.Format(time.DateOnly) + "'"
	for _, filter := range []struct {
		name      string
		statement string
		// settings are the `set local` statements the write runs the statement under.
		settings []string
		// lists is the CTEs a form of the statement holds its list in, whose scans are scans of
		// the list as much as the function scan of unnest that fills them.
		lists                []string
		arguments            []any
		parameters, literals []string
		// joinedOn is a key the generic plan matches rows by (explaintest.RequireJoinedOn), none
		// where empty.
		joinedOn string
	}{
		{
			name: "leftDaysOwedStatement", statement: leftDaysOwedStatement, lists: []string{"given"},
			arguments:  []any{"EVAL", day, families, memberDeployRepos, keys, shippedBy, deliveryDayFamilies, KindDelivery},
			parameters: []string{"text", "date", "text[]", "text[]", "text[]", "text[]", "text[]", "text"},
			literals: []string{"'EVAL'", date, "array['delivery']", "array['acme/deploy-00']", "array['pull_request:acme/web#1']", "array['']",
				"array['delivery', 'ci']", "'delivery'"},
		},
		{
			name: "goneMembersDeleteStatement", statement: goneMembersDeleteStatement,
			arguments:  []any{"EVAL", deliveryDayFamilies, day, families, memberDeployRepos, keys},
			parameters: []string{"text", "text[]", "date", "text[]", "text[]", "text[]"},
			literals:   []string{"'EVAL'", "array['delivery', 'ci']", date, "array['delivery']", "array['acme/deploy-00']", "array['pull_request:acme/web#1']"},
		},
		{
			name: "zeroStaleDaysStatement", statement: zeroStaleDaysStatement,
			arguments:  []any{"EVAL", day, evalIDs, evals, series, repos, testBuild, writerComputation, rowDeployRepos},
			parameters: []string{"text", "date", "text[]", "text[]", "text[]", "text[]", "text", "text", "text[]"},
			literals: []string{"'EVAL'", date, "array['EVAL/deploy-frequency']", "array['EVAL/deploy-frequency']", "array['deploys']",
				"array['acme/repo-00000']", "'test-build'", "'computation'", "array['acme/deploy-00']"},
		},
		{
			name: "zeroStaleSettingsDaysStatement", statement: zeroStaleSettingsDaysStatement,
			settings: []string{"set local plan_cache_mode = force_custom_plan", "set local enable_mergejoin = off"},
			arguments: []any{[]string{"EVAL"}, []time.Time{day}, projects, evals, series, repos, days, evalIDs, testBuild, writerComputation,
				rowDeployRepos, deployRepos},
			parameters: []string{"text[]", "date[]", "text[]", "text[]", "text[]", "text[]", "date[]", "text[]", "text", "text", "text[]", "text[]"},
			literals: []string{"array['EVAL']", "array[" + date + "]::date[]", "array['EVAL']", "array['EVAL/deploy-frequency']", "array['deploys']",
				"array['acme/repo-00000']", "array[" + date + "]::date[]", "array['EVAL/deploy-frequency']", "'test-build'", "'computation'",
				"array['acme/deploy-00']", "array['acme/deploy-00']"},
		},
		{
			// The generic plan matches each reading to its keys by a hash join whose Hash Cond is
			// the series and the day, (date_trunc('day', r.measured_at, 'UTC') = k.day); the plan
			// with the values, of one key, reads it on the outer side of a nested loop.
			name: "standingPartsQuery", statement: standingPartsQuery, joinedOn: "k.day",
			arguments:  []any{evalID, []string{standingKey.series}, []string{standingKey.repo}, []time.Time{standingKey.day}, written},
			parameters: []string{"text", "text[]", "text[]", "timestamptz[]", "text[]"},
			literals: []string{"'" + evalID + "'", "array['minutes']", "array['']", "array[" + date + "]::timestamptz[]",
				"array['00000000-0000-0000-0000-000000000000']"},
		},
	} {
		t.Run(filter.name, func(t *testing.T) {
			tx, err := r.database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin %s's transaction: %v", filter.name, err)
			}
			defer tx.Rollback(ctx)
			for _, setting := range filter.settings {
				if _, err := tx.Exec(ctx, setting); err != nil {
					t.Fatalf("%s: %v", setting, err)
				}
			}
			_, generic := explaintest.RequireStatementReadsListOnce(t, ctx, tx, filter.name, filter.statement, filter.arguments,
				filter.parameters, filter.literals, filter.lists...)
			if filter.joinedOn != "" {
				explaintest.RequireJoinedOn(t, generic, filter.name+" under the generic plan", filter.joinedOn)
			}
		})
	}

	// The second write itself, which leaves one item fewer and makes one row zero.
	if err := r.registry.WriteProjectDay(ctx, ProjectDay{Project: "EVAL", Day: day, Rows: kept, Members: again}, testBuild); err != nil {
		t.Fatalf("write the day again with one item and one row fewer: %v", err)
	}
	if held := r.count(t, `select count(*) from eval_day_members where project_key = 'EVAL'`); held != items-1 {
		t.Errorf("eval_day_members holds %d items of EVAL, want %d", held, items-1)
	}
	if zero := r.count(t, `select count(*) from eval_days where project_key = 'EVAL' and writer = 'computation' and n = 0`); zero != 1 {
		t.Errorf("eval_days holds %d zero rows of EVAL, want 1: the row the day no longer holds", zero)
	}
}

// TestTheZeroRowsOfASettingsRewriteMatchTheWrittenDaysByProjectAndDay: zeroStaleSettingsDays runs
// its statement under a plan whose read of what the days written no longer hold (gone) matches
// the stored rows to those days by their project and their day at one node, a hash or merge join
// or an index scan (explaintest.RequireJoinedOnAll), and gives enable_mergejoin back the value it
// held. A merge join on the project alone, which the primary key's order gives the planner for
// nothing, tests each row's day against every day written of its project: a settings change that
// rewrote 200 days of one project tested 434,218 pairs to keep its 2,182 rows. The rows are EVAL's
// of the six evals a production rule's change moves, in two repositories under one deploy
// repository, on each of 200 days, beside 10 evals of six series each of EVAL and of another
// project on the same days: 26,400 rows, fewer than the 30,000 ANALYZE samples, so it reads every
// row and the statistics are the same on every run. They are written in one transaction that
// commits synchronously, then vacuumed and analyzed, every page all-visible, so the primary key's
// index-only scan reads eval_days in the project's order: with asynchronous commits, which the
// pod's test server runs, a vacuum just after a commit can mark no page all-visible (as
// embedqueue's TestCoverageFetchesNoWindowRowByRowWhileTheStatisticsLagTheVisibilityMap found).
// The write rewrites those 200 days, holds every row but one, and names the six evals of ten
// projects (a rewrite names every project's), in a transaction planned as rewriteSettings plans
// its own. With merge joins on, the planner estimates the scan of eval_days at a few hundred rows
// of its 2,400 and merges it with the days on the project alone, testing the day of each row
// against all 200 days.
//
// The test reads the plan the statement runs under (explainingTx), not the clock.
func TestTheZeroRowsOfASettingsRewriteMatchTheWrittenDaysByProjectAndDay(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	const days, deployRepo = 200, "acme/deploy"
	slugs := []string{SlugDeployFrequency, SlugDeployRunSuccess, SlugFirstCommitToProduction, SlugMergeToProduction,
		SlugOpenToProduction, SlugProductionDeploy}
	first := attributionDay.AddDate(0, 0, 1-days)
	var stored []DayRow
	for _, slug := range slugs {
		for _, repo := range []string{"", "acme/web"} {
			for day := range days {
				stored = append(stored, DayRow{
					ProjectKey: "EVAL", EvalID: EvalID("EVAL", slug), DeployRepo: deployRepo, Repo: repo, Listed: true,
					DayPart: measures.DayPart{Series: "deploys", Day: first.AddDate(0, 0, day), N: 1, Sum: new(1.0)},
				})
			}
		}
	}
	write, err := r.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the write of the day rows: %v", err)
	}
	defer write.Rollback(ctx)
	if _, err := write.Exec(ctx, `set local synchronous_commit = on`); err != nil {
		t.Fatalf("commit the write of the day rows synchronously: %v", err)
	}
	if err := upsertDays(ctx, write, stored, testBuild); err != nil {
		t.Fatalf("write EVAL's rows of the moved evals: %v", err)
	}
	if _, err := write.Exec(ctx, `insert into projects (key, name, default_assignee) values ('OTHER', 'Other', 'alice')`); err != nil {
		t.Fatalf("make project OTHER: %v", err)
	}
	if _, err := write.Exec(ctx, `
		insert into eval_days (project_key, eval_id, series, repo, day, listed, n, sum, built_by)
		select project, project || '/legion-' || eval, 'series-' || series, '', $1::date + after, true, 1, 1, $2
		from unnest(array['EVAL', 'OTHER']) as project, generate_series(1, 10) as eval, generate_series(1, 6) as series,
			generate_series(0, $3::int - 1) as after`, first, testBuild, days); err != nil {
		t.Fatalf("store the other evals' rows: %v", err)
	}
	if err := write.Commit(ctx); err != nil {
		t.Fatalf("commit the day rows: %v", err)
	}
	if _, err := r.database.Pool.Exec(ctx, `vacuum analyze eval_days`); err != nil {
		t.Fatalf("vacuum and analyze eval_days: %v", err)
	}
	var pages, visible int
	if err := r.database.Pool.QueryRow(ctx, `select relpages, relallvisible from pg_class where oid = 'eval_days'::regclass`).Scan(&pages, &visible); err != nil {
		t.Fatalf("read how many pages of eval_days are all-visible: %v", err)
	}
	if pages == 0 || visible != pages {
		t.Fatalf("the vacuum marked %d of eval_days' %d pages all-visible; the test needs every page", visible, pages)
	}

	projects, written := make([]string, days), make([]time.Time, days)
	for day := range days {
		projects[day], written[day] = "EVAL", first.AddDate(0, 0, day)
	}
	var evalIDs []string
	for _, project := range []string{"EVAL", "A", "B", "C", "D", "E", "F", "G", "H", "I"} {
		for _, slug := range slugs {
			evalIDs = append(evalIDs, EvalID(project, slug))
		}
	}
	tx, err := r.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the rewrite's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `set local plan_cache_mode = force_custom_plan`); err != nil {
		t.Fatalf("plan the rewrite's transaction as rewriteSettings does: %v", err)
	}
	explaining := &explainingTx{Tx: tx, explained: zeroStaleSettingsDaysStatement}
	if err := zeroStaleSettingsDays(ctx, explaining, projects, written, evalIDs, []string{deployRepo}, stored[1:], testBuild); err != nil {
		t.Fatalf("zeroStaleSettingsDays: %v", err)
	}
	if explaining.plan == nil {
		t.Fatal("zeroStaleSettingsDays ran no zeroStaleSettingsDaysStatement")
	}
	plan := explaintest.OnePlan(t, explaining.plan)
	t.Logf("zeroStaleSettingsDaysStatement as zeroStaleSettingsDays runs it over %d days written:\n%s", days, explaintest.Outline(t, plan))
	explaintest.RequireJoinedOnAll(t, plan, "zeroStaleSettingsDaysStatement as zeroStaleSettingsDays runs it", "CTE gone", "project_key", "day")
	var mergeJoins string
	if err := tx.QueryRow(ctx, `select current_setting('enable_mergejoin')`).Scan(&mergeJoins); err != nil {
		t.Fatalf("read enable_mergejoin: %v", err)
	}
	if mergeJoins != "on" {
		t.Errorf("enable_mergejoin is %s after zeroStaleSettingsDays, want on, the value it held before", mergeJoins)
	}
	var zero int
	if err := tx.QueryRow(ctx, `select count(*) from eval_days where project_key = 'EVAL' and deploy_repo = $1 and n = 0`, deployRepo).Scan(&zero); err != nil {
		t.Fatalf("count EVAL's zero rows: %v", err)
	}
	if zero != 1 {
		t.Errorf("eval_days holds %d zero rows of EVAL's moved evals, want 1: the row the write no longer holds", zero)
	}
}

// explainingTx is a transaction that explains its statement explained, with the arguments it is
// run with and under whatever its caller set in the transaction, before it runs it, and keeps the
// plan: the plan a function that runs the statement in its caller's transaction gives it.
type explainingTx struct {
	pgx.Tx
	explained string
	plan      []byte
}

// Exec explains sql before it runs it where it is the statement explained.
func (tx *explainingTx) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	if sql == tx.explained {
		if err := tx.Tx.QueryRow(ctx, "explain (format json) "+sql, arguments...).Scan(&tx.plan); err != nil {
			return pgconn.CommandTag{}, fmt.Errorf("explain the statement: %w", err)
		}
	}
	return tx.Tx.Exec(ctx, sql, arguments...)
}
