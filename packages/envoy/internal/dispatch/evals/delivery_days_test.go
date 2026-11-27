package evals

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// attributionDay is the day the hand-written delivery history of seedAttribution happens on.
var attributionDay = time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)

// The hand-written history's repositories: acme/alpha (the deploy repository, whose runs Dispatch
// reads), acme/beta, and acme/zeta, whose one pull request names no issue.
const (
	alphaRepo = "acme/alpha"
	betaRepo  = "acme/beta"
	zetaRepo  = "acme/zeta"
)

// seedAttribution writes, on attributionDay, the delivery history of the attribution rule over
// three projects, ALPHA, BETA and GAMMA (no row), with acme/alpha the deploy repository. No
// project has a default repository, and no pull request's attribution inputs are stored (not read
// from GitHub yet), so each counts by the one list its row's issue_key gives (evals'
// attribution): its issue's project, or none, which reaches no project.
//
//   - acme/alpha#1 names BETA-1, so it counts for BETA;
//   - acme/alpha#2 names no issue, so it counts for no project;
//   - acme/alpha#3 names ALPHA-1, so it counts for ALPHA;
//   - acme/beta#1 names ALPHA-1, so it counts for ALPHA although it is no pull request of the
//     deploy repository;
//   - acme/zeta#1 names no issue, so it counts for no project;
//   - deploy run 101, created after the three acme/alpha merges, reaches production and ships
//     them: it counts once for BETA (acme/alpha#1) and once for ALPHA (acme/alpha#3), and so do its
//     production job's push run's results; acme/alpha#2 adds no project;
//   - deploy runs 102 (reaches production, ships nothing) and 103 (its production job fails) ship
//     none, so they count for no project;
//   - PR-checks run 201 of acme/alpha#3 counts for ALPHA, its pull request's project, and so does
//     its job; PR-checks run 202 of acme/alpha#2 counts for no project.
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
		{alphaRepo, 3, new("ALPHA-1"), at(10, 20)},
		{betaRepo, 1, new("ALPHA-1"), at(11, 0)},
		{zetaRepo, 1, nil, at(11, 30)},
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
		pr         int
	}{
		{101, delivery.DeliveryRunKindDeploy, at(10, 30), delivery.DeliveryRunConclusionSuccess, production, success, 0},
		{102, delivery.DeliveryRunKindDeploy, at(12, 0), delivery.DeliveryRunConclusionSuccess, production, success, 0},
		{103, delivery.DeliveryRunKindDeploy, at(13, 0), delivery.DeliveryRunConclusionFailure, production, failure, 0},
		{201, delivery.DeliveryRunKindPRChecks, at(9, 30), delivery.DeliveryRunConclusionSuccess, "lint", success, 3},
		{202, delivery.DeliveryRunKindPRChecks, at(9, 15), delivery.DeliveryRunConclusionSuccess, "lint", success, 2},
	} {
		started, completed := run.created.Add(time.Minute), run.created.Add(20*time.Minute)
		stored := delivery.DeliveryRun{
			Repo: alphaRepo, RunID: run.id, Kind: run.kind, HeadSHA: fmt.Sprintf("%040d", run.id), HeadCommitAt: run.created.Add(-time.Minute),
			StartedAt: started, CreatedAt: new(run.created), CompletedAt: &completed, Conclusion: new(run.conclusion),
			URL: fmt.Sprintf("https://github.com/%s/actions/runs/%d", alphaRepo, run.id), HeadBranch: new("main"), Event: new("push"),
		}
		if run.kind == delivery.DeliveryRunKindPRChecks {
			stored.PRNumber, stored.HeadBranch, stored.Event = new(run.pr), new(fmt.Sprintf("feature/%d", run.pr)), new("pull_request")
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
// ` as <deploy repo>` for an item counted under it, then ` shipped by <run key>` if shipped.
func (r rig) members(t *testing.T, project string) []string {
	t.Helper()
	rows, err := r.database.Pool.Query(t.Context(), `
		select family || ' ' || row_key || ' ' || repo || ' ' || day::text || case when deploy_repo <> '' then ' as ' || deploy_repo else '' end
			|| case when shipped_by <> '' then ' shipped by ' || shipped_by else '' end
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
			for _, c := range run.projects {
				projects[c.project] = true
			}
		}
	}
	delete(projects, "")
	return slices.Sorted(maps.Keys(projects))
}

// refreshDays writes days, in order, as a pass of the refresher's walks writes them, each day one
// page of the pass, under testBuild as of the end of the last day: each day's delivery and CI rows
// of every project (refresh.deliveryWalkDay), where delivery settings exist, and each day's Legion
// rows of each project of legion (refresh.writeLegion), each with the zero rows of the series
// begun before it. The walks' batch begins at the first of days (refresh.beginBatch), so each
// series' first input day is the first of the days before it, or of those the batch writes, that
// holds input of it, and a day holding rows of a walk's family is written whatever it builds. The
// pass first loads what a pass loads, its seed of every project no seed has reached included
// (refresh.load). It answers the days written.
func (r rig) refreshDays(t *testing.T, legion []string, days ...time.Time) []string {
	t.Helper()
	ctx := t.Context()
	pass := r.registry.newRefresh(testBuild, days[len(days)-1].AddDate(0, 0, 1).Add(-time.Minute), walkBatch)
	if err := pass.load(ctx, &seeding{everyProject: true}); err != nil {
		t.Fatalf("load the refresher's pass: %v", err)
	}
	pass.walking = true
	first, last := days[0], days[len(days)-1]
	walks := []walkKey{}
	if pass.settings != nil {
		walks = append(walks, walkKey{family: walkDelivery})
	}
	for _, project := range legion {
		walks = append(walks, walkKey{family: walkLegion, project: project})
	}
	for _, key := range walks {
		if err := pass.beginBatch(ctx, key, first, last); err != nil {
			t.Fatalf("begin the batch of the %s walk of %q: %v", key.family, key.project, err)
		}
	}
	for _, day := range days {
		pass.beginPage()
		if pass.settings != nil {
			if err := pass.deliveryWalkDay(ctx, day); err != nil {
				t.Fatalf("write the delivery rows of %s: %v", day.Format(time.DateOnly), err)
			}
		}
		for _, project := range legion {
			if err := pass.writeLegion(ctx, project, day); err != nil {
				t.Fatalf("write the Legion rows of %s on %s: %v", project, day.Format(time.DateOnly), err)
			}
		}
	}
	return pass.report.sorted()
}

// TestEachDeliveryAndCIRowCountsForTheProjectItIsAttributedTo: over seedAttribution's day, written
// as the refresher's walk writes it, a pull request counts for its issue's project and one with no
// issue for its repository's owner key (owners.go, R15); the deploy that ships pull requests of two
// projects counts once in each, its production job's push run too, and the deploys that ship none
// count for their repository's owner key; a PR-checks run counts for its pull request's project,
// and one of a pull request with no issue for that repository's owner key; every via_repository is
// 0 in a project's rows and n in an owner key's; and GAMMA, with nothing, gets no row. Then the
// day is written again unchanged (no row rewritten), and once acme/alpha#3 names BETA-1 the day's
// rewrite moves it, the
// deploy that shipped it and its PR-checks run out of ALPHA: ALPHA's acme/alpha rows of the
// pull-request evals, which held it alone, are deleted, as a whole walk over the moved pull
// request holds none, and so is every lead time of ALPHA, whose one day now ships none of its pull
// requests, and every row of a deploy or a CI run; its items of the day that it no longer holds
// are deleted; written again, the moved day rewrites nothing.
func TestEachDeliveryAndCIRowCountsForTheProjectItIsAttributedTo(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	settings := seedAttribution(t, r)
	ctx := t.Context()

	reads, err := r.registry.readDeliveryDays(ctx, settings, []keptRepo{{name: alphaRepo, deploys: true}}, []time.Time{measures.Midnight(attributionDay)}, nil)
	if err != nil {
		t.Fatalf("read the day: %v", err)
	}
	read := reads[0]
	wantOwners := []string{OwnerKey(alphaRepo, nil, nil), OwnerKey(alphaRepo, []string{"ALPHA", "BETA"}, nil), OwnerKey(zetaRepo, nil, nil)}
	if got := dayProjects(read); !slices.Equal(got, slices.Concat(wantOwners, []string{"ALPHA", "BETA"})) {
		t.Fatalf("the day's rows count for %v, want ALPHA, BETA and the owner keys %v (acme/alpha#2 and acme/zeta#1 through their repositories, GAMMA holds nothing)",
			got, wantOwners)
	}
	t.Logf("wrote %v", r.refreshDays(t, nil, attributionDay))

	// Run 101, created at 10:30, shipped at 10:49, when its production job completed; every job of
	// the history runs 17 minutes and every run 19.
	want := map[string][]string{
		"ALPHA": {
			// acme/alpha#3 and acme/beta#1, both through ALPHA-1. acme/alpha ships acme/alpha's pull
			// requests alone, so acme/beta's lead times to production time none and hold no row: a
			// repository has no zero row (repositoryRows).
			"merge-to-production minutes all: n=1 via=0 samples=[29]",
			"merge-to-production minutes acme/alpha: n=1 via=0 samples=[29]",
			// No pull request records a first commit, so first-commit-to-production times none and,
			// before its first day, holds no row (dropUnshipped).
			"open-to-production minutes all: n=1 via=0 samples=[89]",
			"open-to-production minutes acme/alpha: n=1 via=0 samples=[89]",
			"pr-open-to-merge minutes all: n=2 via=0 samples=[60 60]",
			"pr-open-to-merge minutes acme/alpha: n=1 via=0 samples=[60]",
			"pr-open-to-merge minutes acme/beta: n=1 via=0 samples=[60]",
			"rework-share prs all: n=2 via=0 share=0/2",
			"rework-share prs acme/alpha: n=1 via=0 share=0/1",
			"rework-share prs acme/beta: n=1 via=0 share=0/1",
			// Run 101, through acme/alpha#3's issue; runs 102 and 103 ship none.
			"deploy-frequency deploys all: n=1 via=0 sum=1 with_prs=1",
			"deploy-frequency deploys acme/alpha: n=1 via=0 sum=1 with_prs=1",
			"deploy-run-success runs all: n=1 via=0 share=1/1",
			"deploy-run-success runs acme/alpha: n=1 via=0 share=1/1",
			// Run 201, of acme/alpha#3, and its one job; run 101's production job.
			"pr-ci minutes all: n=1 via=0 samples=[19]",
			"pr-ci minutes acme/alpha: n=1 via=0 samples=[19]",
			"pr-ci job:lint all: n=1 via=0 samples=[17]",
			"pr-ci job:lint acme/alpha: n=1 via=0 samples=[17]",
			"production-deploy minutes all: n=1 via=0 samples=[17]",
			"production-deploy minutes acme/alpha: n=1 via=0 samples=[17]",
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
			// Run 101, through acme/alpha#1's issue, and its production job.
			"deploy-frequency deploys all: n=1 via=0 sum=1 with_prs=1",
			"deploy-frequency deploys acme/alpha: n=1 via=0 sum=1 with_prs=1",
			"deploy-run-success runs all: n=1 via=0 share=1/1",
			"deploy-run-success runs acme/alpha: n=1 via=0 share=1/1",
			"production-deploy minutes all: n=1 via=0 samples=[17]",
			"production-deploy minutes acme/alpha: n=1 via=0 samples=[17]",
		},
		"GAMMA": nil,
		wantOwners[0]: {
			"merge-to-production minutes all: n=1 via=1 samples=[39]",
			"merge-to-production minutes acme/alpha: n=1 via=1 samples=[39]",
			"open-to-production minutes all: n=1 via=1 samples=[99]",
			"open-to-production minutes acme/alpha: n=1 via=1 samples=[99]",
			"pr-open-to-merge minutes all: n=1 via=1 samples=[60]",
			"pr-open-to-merge minutes acme/alpha: n=1 via=1 samples=[60]",
			"rework-share prs all: n=1 via=1 share=0/1",
			"rework-share prs acme/alpha: n=1 via=1 share=0/1",
			"deploy-frequency deploys all: n=1 via=1 sum=1 with_prs=0",
			"deploy-frequency deploys acme/alpha: n=1 via=1 sum=1 with_prs=0",
			"deploy-run-success runs all: n=2 via=2 share=1/2",
			"deploy-run-success runs acme/alpha: n=2 via=2 share=1/2",
			"pr-ci minutes all: n=1 via=1 samples=[19]",
			"pr-ci minutes acme/alpha: n=1 via=1 samples=[19]",
			"pr-ci job:lint all: n=1 via=1 samples=[17]",
			"pr-ci job:lint acme/alpha: n=1 via=1 samples=[17]",
			"production-deploy minutes all: n=2 via=2 samples=[17 17]",
			"production-deploy minutes acme/alpha: n=2 via=2 samples=[17 17]",
		},
		wantOwners[1]: {
			"deploy-frequency deploys all: n=1 via=1 sum=1 with_prs=1",
			"deploy-frequency deploys acme/alpha: n=1 via=1 sum=1 with_prs=1",
			"deploy-run-success runs all: n=1 via=1 share=1/1",
			"deploy-run-success runs acme/alpha: n=1 via=1 share=1/1",
			"production-deploy minutes all: n=1 via=1 samples=[17]",
			"production-deploy minutes acme/alpha: n=1 via=1 samples=[17]",
		},
		wantOwners[2]: {
			"pr-open-to-merge minutes all: n=1 via=1 samples=[60]",
			"pr-open-to-merge minutes acme/zeta: n=1 via=1 samples=[60]",
			"rework-share prs all: n=1 via=1 share=0/1",
			"rework-share prs acme/zeta: n=1 via=1 share=0/1",
		},
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
			"ci run:acme/alpha/101 acme/alpha 2026-11-03 as acme/alpha", "ci run:acme/alpha/201 acme/alpha 2026-11-03 as acme/alpha",
			"delivery pull_request:acme/alpha#3 acme/alpha 2026-11-03", "delivery pull_request:acme/alpha#3 acme/alpha 2026-11-03 as acme/alpha shipped by run:acme/alpha/101",
			"delivery pull_request:acme/beta#1 acme/beta 2026-11-03", "delivery pull_request:acme/beta#1 acme/beta 2026-11-03 as acme/alpha",
			"delivery run:acme/alpha/101 acme/alpha 2026-11-03 as acme/alpha",
		},
		"BETA": {
			"ci run:acme/alpha/101 acme/alpha 2026-11-03 as acme/alpha",
			"delivery pull_request:acme/alpha#1 acme/alpha 2026-11-03", "delivery pull_request:acme/alpha#1 acme/alpha 2026-11-03 as acme/alpha shipped by run:acme/alpha/101",
			"delivery run:acme/alpha/101 acme/alpha 2026-11-03 as acme/alpha",
		},
		"GAMMA": nil,
		wantOwners[0]: {
			"ci run:acme/alpha/102 acme/alpha 2026-11-03 as acme/alpha",
			"ci run:acme/alpha/103 acme/alpha 2026-11-03 as acme/alpha",
			"ci run:acme/alpha/202 acme/alpha 2026-11-03 as acme/alpha",
			"delivery pull_request:acme/alpha#2 acme/alpha 2026-11-03",
			"delivery pull_request:acme/alpha#2 acme/alpha 2026-11-03 as acme/alpha shipped by run:acme/alpha/101",
			"delivery run:acme/alpha/102 acme/alpha 2026-11-03 as acme/alpha",
			"delivery run:acme/alpha/103 acme/alpha 2026-11-03 as acme/alpha",
		},
		wantOwners[1]: {
			"ci run:acme/alpha/101 acme/alpha 2026-11-03 as acme/alpha",
			"delivery run:acme/alpha/101 acme/alpha 2026-11-03 as acme/alpha",
		},
		wantOwners[2]: {
			"delivery pull_request:acme/zeta#1 acme/zeta 2026-11-03",
			"delivery pull_request:acme/zeta#1 acme/zeta 2026-11-03 as acme/alpha",
		},
	}
	for _, project := range slices.Sorted(maps.Keys(wantMembers)) {
		if got := r.members(t, project); !slices.Equal(got, wantMembers[project]) {
			t.Errorf("the items %s counted: %v, want %v", project, got, wantMembers[project])
		}
	}
	for _, none := range []string{"pull_request:acme/zeta#1", "pull_request:acme/alpha#2", "run:acme/alpha/102", "run:acme/alpha/103", "run:acme/alpha/202"} {
		if n := r.count(t, `select count(*) from eval_day_members where row_key = $1 and not starts_with(project_key, ':')`, none); n != 0 {
			t.Errorf("%s is counted in %d projects, want none: it reaches no project through an issue", none, n)
		}
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

	// acme/alpha#3 now names BETA-1: it, run 101, which shipped it, and run 201, its PR-checks
	// run, leave ALPHA's rows.
	if _, err := r.database.Pool.Exec(ctx, `update delivery_pull_requests set issue_key = 'BETA-1' where repo = $1 and number = 3`, alphaRepo); err != nil {
		t.Fatalf("move acme/alpha#3 to BETA-1: %v", err)
	}
	r.refreshDays(t, nil, attributionDay)
	wantBeta := []string{
		"merge-to-production minutes all: n=2 via=0 samples=[49 29]",
		"merge-to-production minutes acme/alpha: n=2 via=0 samples=[49 29]",
		"open-to-production minutes all: n=2 via=0 samples=[109 89]",
		"open-to-production minutes acme/alpha: n=2 via=0 samples=[109 89]",
		"pr-open-to-merge minutes all: n=2 via=0 samples=[60 60]",
		"pr-open-to-merge minutes acme/alpha: n=2 via=0 samples=[60 60]",
		"rework-share prs all: n=2 via=0 share=0/2",
		"rework-share prs acme/alpha: n=2 via=0 share=0/2",
		"deploy-frequency deploys all: n=1 via=0 sum=1 with_prs=1",
		"deploy-frequency deploys acme/alpha: n=1 via=0 sum=1 with_prs=1",
		"deploy-run-success runs all: n=1 via=0 share=1/1",
		"deploy-run-success runs acme/alpha: n=1 via=0 share=1/1",
		"production-deploy minutes all: n=1 via=0 samples=[17]",
		"production-deploy minutes acme/alpha: n=1 via=0 samples=[17]",
		"pr-ci minutes all: n=1 via=0 samples=[19]",
		"pr-ci minutes acme/alpha: n=1 via=0 samples=[19]",
		"pr-ci job:lint all: n=1 via=0 samples=[17]",
		"pr-ci job:lint acme/alpha: n=1 via=0 samples=[17]",
	}
	slices.Sort(wantBeta)
	if got := allFacts(r.stored(t, "BETA")); !slices.Equal(got, wantBeta) {
		t.Errorf("BETA's rows after the move:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(wantBeta, "\n"))
	}
	alpha := allFacts(r.stored(t, "ALPHA"))
	gone := append(slices.Clone(leadTimeSlugs), SlugDeployFrequency, SlugDeployRunSuccess, SlugPRCI, SlugProductionDeploy)
	for _, fact := range alpha {
		if strings.HasPrefix(fact, "rework-share prs acme/alpha: ") || slices.ContainsFunc(gone, func(slug string) bool { return strings.HasPrefix(fact, slug+" ") }) {
			t.Errorf("ALPHA's rows after acme/alpha's last pull request moved to BETA hold %q, want no row of acme/alpha's pull requests, no lead time and no deploy or CI row, as a whole walk holds none", fact)
		}
	}
	if err := r.database.Pool.QueryRow(ctx, `select max(updated_at) from eval_days`).Scan(&newest); err != nil {
		t.Fatalf("read the newest write: %v", err)
	}
	stored := r.count(t, `select count(*) from eval_days`)
	r.refreshDays(t, nil, attributionDay)
	if rewritten := r.count(t, `select count(*) from eval_days where updated_at > $1`, newest); rewritten != 0 || r.count(t, `select count(*) from eval_days`) != stored {
		t.Errorf("writing the moved day again rewrote %d rows, or deleted some of its %d, want none", rewritten, stored)
	}
	for _, kept := range []string{"rework-share prs all: n=1 via=0 share=0/1", "pr-open-to-merge minutes acme/beta: n=1 via=0 samples=[60]"} {
		if !slices.Contains(alpha, kept) {
			t.Errorf("ALPHA's rows after the move lack %q:\n%s", kept, strings.Join(alpha, "\n"))
		}
	}
	if got := r.members(t, "ALPHA"); !slices.Equal(got, []string{
		"delivery pull_request:acme/beta#1 acme/beta 2026-11-03", "delivery pull_request:acme/beta#1 acme/beta 2026-11-03 as acme/alpha",
	}) {
		t.Errorf("ALPHA's items after the move: %v, want acme/beta#1's alone", got)
	}
	wantBetaMembers := append(slices.Clone(wantMembers["BETA"]), "ci run:acme/alpha/201 acme/alpha 2026-11-03 as acme/alpha",
		"delivery pull_request:acme/alpha#3 acme/alpha 2026-11-03", "delivery pull_request:acme/alpha#3 acme/alpha 2026-11-03 as acme/alpha shipped by run:acme/alpha/101")
	slices.Sort(wantBetaMembers)
	if got := r.members(t, "BETA"); !slices.Equal(got, wantBetaMembers) {
		t.Errorf("BETA's items after the move: %v, want %v", got, wantBetaMembers)
	}
	t.Logf("compared %d day rows; after acme/alpha#3 moved to BETA, ALPHA holds %d", compared, len(alpha))
}

// TestARepositoryHoldsNoZeroRowAndARewriteThatEmptiesItsPartDeletesItsRow: R12 writes no zero row
// for a repository (repositoryRows). Over two days of acme/alpha, the deploy repository, each with
// a pull request of EVAL-1, which counts for EVAL, and one that names no issue, which counts for
// the repository's plain owner key, the first day's deploy ships the day's two pull requests, and
// the second day's one deploy, created before that day's merges, ships none: the second day's pull
// requests have no deploy yet, as those of the last hour of a history have none (k34,
// dispatch://INFRA-2916). Written as the refresher's walk writes it, the second day holds no lead
// time to production of a repository under any key, where the first day's acme/alpha rows time its
// pull requests; each key's all-repositories row of each lead time holds its zero row, the series
// having begun the day before; the owner key's deploy-frequency and deploy-run-success rows, of all
// repositories and of acme/alpha, count the deploy; and every other row of the day stands as the
// day's input builds it, EVAL's zero rows of the deploy series among them. Then the second day's
// deploy fails its production job and the day is written again: the owner key's acme/alpha row of
// deploy-frequency, which counted that deploy, counts nothing and is deleted, its all-repositories
// row holds its zero row, its deploy-run-success rows count the failure, and EVAL's rows stand.
func TestARepositoryHoldsNoZeroRowAndARewriteThatEmptiesItsPartDeletesItsRow(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	pool := r.database.Pool
	first, second := attributionDay, attributionDay.AddDate(0, 0, 1)
	exec := func(what, statement string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, statement, args...); err != nil {
			t.Fatalf("write %s: %v", what, err)
		}
	}
	exec("the issue EVAL-1", `
		insert into issues (key, project_key, number, title, status, rank, created_by, created_at)
		values ('EVAL-1', 'EVAL', 1, 'Eval work', 'in_progress', '00000001', '{"kind":"system","id":"zero-row-test"}', $1)
	`, first.AddDate(0, 0, -1))
	production := "production / apply"
	if _, err := delivery.PutSettings(ctx, pool, delivery.DeliverySettings{
		DeployRepo: alphaRepo, DeployWorkflowPath: ".github/workflows/deploy.yml", ProductionJobNames: []string{production},
		PRChecksWorkflowPath: ".github/workflows/pr-checks.yml", PopulationAuthors: []string{"dev-ana"}, ExcludedRepos: []string{},
	}, model.Actor{Kind: "system", ID: "zero-row-test"}); err != nil {
		t.Fatalf("write the delivery settings: %v", err)
	}
	// Each pull request opens an hour before its merge, its first commit half an hour before that.
	for _, pr := range []struct {
		number int
		issue  *string
		merged time.Time
	}{
		{1, new("EVAL-1"), first.Add(10 * time.Hour)},
		{2, nil, first.Add(10*time.Hour + 10*time.Minute)},
		{3, new("EVAL-1"), second.Add(11 * time.Hour)},
		{4, nil, second.Add(11*time.Hour + 10*time.Minute)},
	} {
		created, firstCommit := pr.merged.Add(-time.Hour), pr.merged.Add(-90*time.Minute)
		if err := delivery.UpsertPullRequest(ctx, pool, delivery.DeliveryPullRequest{
			Repo: alphaRepo, Number: pr.number, Title: fmt.Sprintf("Change %d", pr.number), URL: fmt.Sprintf("https://github.com/%s/pull/%d", alphaRepo, pr.number),
			Author: "dev-ana", CreatedAt: &created, MergedAt: new(pr.merged), FirstCommitAt: &firstCommit, IssueKey: pr.issue, Sessions: []string{},
		}); err != nil {
			t.Fatalf("write %s#%d: %v", alphaRepo, pr.number, err)
		}
	}
	// Each deploy starts a minute after GitHub creates it and ends 19 minutes later; its production
	// job runs the 17 minutes between, and ships at its end.
	for _, run := range []struct {
		id      int64
		created time.Time
	}{{101, first.Add(10*time.Hour + 30*time.Minute)}, {102, second.Add(10*time.Hour + 30*time.Minute)}} {
		started, completed := run.created.Add(time.Minute), run.created.Add(20*time.Minute)
		if err := delivery.UpsertRun(ctx, pool, delivery.DeliveryRun{
			Repo: alphaRepo, RunID: run.id, Kind: delivery.DeliveryRunKindDeploy, HeadSHA: fmt.Sprintf("%040d", run.id), HeadCommitAt: run.created.Add(-time.Minute),
			StartedAt: started, CreatedAt: new(run.created), CompletedAt: &completed, Conclusion: new(delivery.DeliveryRunConclusionSuccess),
			URL: fmt.Sprintf("https://github.com/%s/actions/runs/%d", alphaRepo, run.id), HeadBranch: new("main"), Event: new("push"),
		}); err != nil {
			t.Fatalf("write run %d: %v", run.id, err)
		}
		job := delivery.DeliveryRunJob{Name: production, StartedAt: new(started.Add(time.Minute)), CompletedAt: new(completed.Add(-time.Minute)),
			Conclusion: new(delivery.DeliveryJobConclusionSuccess)}
		if err := delivery.UpsertRunJobs(ctx, pool, alphaRepo, run.id, []delivery.DeliveryRunJob{job}, true); err != nil {
			t.Fatalf("write the jobs of run %d: %v", run.id, err)
		}
	}
	t.Logf("wrote %v", r.refreshDays(t, nil, first, second))

	owner := OwnerKey(alphaRepo, nil, nil)
	on := func(key string, day time.Time) []string {
		t.Helper()
		var rows []DayRow
		for _, row := range r.stored(t, key) {
			if row.Day.Equal(day) {
				rows = append(rows, row)
			}
		}
		return allFacts(rows)
	}
	repositoryLeadTimes := func(day time.Time) int {
		t.Helper()
		n := 0
		for _, slug := range leadTimeSlugs {
			n += r.count(t, `select count(*) from eval_days where day = $1 and repo <> '' and eval_id like '%/' || $2`, day, slug)
		}
		return n
	}
	// The first day's acme/alpha rows time the pull requests run 101 shipped at 10:49, in EVAL's
	// rows (acme/alpha#1) and the owner key's (acme/alpha#2).
	firstDay := map[string][]string{
		"EVAL": {
			"merge-to-production minutes acme/alpha: n=1 via=0 samples=[49]",
			"first-commit-to-production minutes acme/alpha: n=1 via=0 samples=[139]",
			"open-to-production minutes acme/alpha: n=1 via=0 samples=[109]",
		},
		owner: {
			"merge-to-production minutes acme/alpha: n=1 via=1 samples=[39]",
			"first-commit-to-production minutes acme/alpha: n=1 via=1 samples=[129]",
			"open-to-production minutes acme/alpha: n=1 via=1 samples=[99]",
		},
	}
	for key, want := range firstDay {
		got := on(key, first)
		for _, fact := range want {
			if !slices.Contains(got, fact) {
				t.Errorf("%s's rows of %s lack %q:\n%s", key, first.Format(time.DateOnly), fact, strings.Join(got, "\n"))
			}
		}
	}
	if n := repositoryLeadTimes(first); n != 6 {
		t.Errorf("%s holds %d repositories' rows of a lead time to production, want acme/alpha's 3 of EVAL and 3 of %s", first.Format(time.DateOnly), n, owner)
	}

	// The second day: acme/alpha#3 and acme/alpha#4 merge with no deploy yet, and run 102, which
	// reached production at 10:49, before they merged, ships neither.
	wantEval := []string{
		"merge-to-production minutes all: n=0 via=0 samples=[]",
		"first-commit-to-production minutes all: n=0 via=0 samples=[]",
		"open-to-production minutes all: n=0 via=0 samples=[]",
		"pr-open-to-merge minutes all: n=1 via=0 samples=[60]",
		"pr-open-to-merge minutes acme/alpha: n=1 via=0 samples=[60]",
		"rework-share prs all: n=1 via=0 share=0/1",
		"rework-share prs acme/alpha: n=1 via=0 share=0/1",
		// The deploy series EVAL began the day before with run 101, which hold their zero rows.
		"deploy-frequency deploys all: n=0 via=0 sum=0 with_prs=0",
		"deploy-run-success runs all: n=0 via=0 share=0/0",
		"production-deploy minutes all: n=0 via=0 samples=[]",
	}
	slices.Sort(wantEval)
	// ownerRows is the owner key's rows of the second day, sorted: frequency its deploy-frequency
	// rows, success its deploy-run-success rows' facts and card its production-deploy rows'.
	ownerRows := func(frequency []string, success, card string) []string {
		return slices.Sorted(slices.Values(slices.Concat([]string{
			"merge-to-production minutes all: n=0 via=0 samples=[]",
			"first-commit-to-production minutes all: n=0 via=0 samples=[]",
			"open-to-production minutes all: n=0 via=0 samples=[]",
			"pr-open-to-merge minutes all: n=1 via=1 samples=[60]",
			"pr-open-to-merge minutes acme/alpha: n=1 via=1 samples=[60]",
			"rework-share prs all: n=1 via=1 share=0/1",
			"rework-share prs acme/alpha: n=1 via=1 share=0/1",
			"deploy-run-success runs all: " + success,
			"deploy-run-success runs acme/alpha: " + success,
			"production-deploy minutes all: " + card,
			"production-deploy minutes acme/alpha: " + card,
		}, frequency)))
	}
	wantOwner := ownerRows([]string{
		"deploy-frequency deploys all: n=1 via=1 sum=1 with_prs=0",
		"deploy-frequency deploys acme/alpha: n=1 via=1 sum=1 with_prs=0",
	}, "n=1 via=1 share=1/1", "n=1 via=1 samples=[17]")
	for key, want := range map[string][]string{"EVAL": wantEval, owner: wantOwner} {
		if got := on(key, second); !slices.Equal(got, want) {
			t.Errorf("%s's rows of %s:\n%s\nwant:\n%s", key, second.Format(time.DateOnly), strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
	if n := repositoryLeadTimes(second); n != 0 {
		t.Errorf("%s holds %d repositories' rows of a lead time to production, want none: its pull requests have no deploy yet, and a repository has no zero row",
			second.Format(time.DateOnly), n)
	}

	// Run 102's production job fails: the deploy reaches production no more.
	exec("run 102's failure", `update delivery_runs set conclusion = 'failure' where repo = $1 and run_id = 102`, alphaRepo)
	exec("run 102's production job's failure", `update delivery_run_jobs set conclusion = 'failure' where repo = $1 and run_id = 102`, alphaRepo)
	r.refreshDays(t, nil, second)
	wantOwner = ownerRows([]string{"deploy-frequency deploys all: n=0 via=0 sum=0 with_prs=0"}, "n=1 via=1 share=0/1", "n=1 via=1 samples=[17]")
	if got := on(owner, second); !slices.Equal(got, wantOwner) {
		t.Errorf("%s's rows of %s after run 102 failed:\n%s\nwant:\n%s", owner, second.Format(time.DateOnly), strings.Join(got, "\n"), strings.Join(wantOwner, "\n"))
	}
	if n := r.count(t, `select count(*) from eval_days where project_key = $1 and eval_id = $2 and day = $3 and repo = $4`,
		owner, EvalID(owner, SlugDeployFrequency), second, alphaRepo); n != 0 {
		t.Errorf("%s holds %d rows of %s's deploy-frequency in %s after its one deploy failed, want none: it counts nothing there", second.Format(time.DateOnly), n, owner, alphaRepo)
	}
	if got := on("EVAL", second); !slices.Equal(got, wantEval) {
		t.Errorf("EVAL's rows of %s after run 102 failed:\n%s\nwant them as they stood:\n%s", second.Format(time.DateOnly), strings.Join(got, "\n"), strings.Join(wantEval, "\n"))
	}
}

// A pull request's ranked candidates make its attribution list as the attribution passes over and
// holds them (attributionListOf): a candidate whose issue owns the pull request's repository ends
// the list as its terminal, one whose issue owns another repository is passed over, one whose
// issue owns none is a condition, once, in rank order, and one whose issue no row holds or whose
// chain ran out of depth is passed over. The owner key of lists that are not constant carries them
// after the repository and the sharers (OwnerKey), sorted and once each, and reads back as it was
// made (parseOwner); a key whose every list is none is the plain key.
func TestAPullRequestsCandidatesMakeItsAttributionList(t *testing.T) {
	t.Parallel()
	here := func(project string) delivery.ResolvedCandidate {
		return delivery.ResolvedCandidate{Key: project + "-1", Project: project, Owned: alphaRepo, OwnedHere: true}
	}
	elsewhere := func(project string) delivery.ResolvedCandidate {
		return delivery.ResolvedCandidate{Key: project + "-2", Project: project, Owned: betaRepo}
	}
	unowned := func(project string) delivery.ResolvedCandidate {
		return delivery.ResolvedCandidate{Key: project + "-3", Project: project}
	}
	for _, c := range []struct {
		name       string
		candidates []delivery.ResolvedCandidate
		want       string
		constant   bool
	}{
		{"no candidate", nil, ">", true},
		{"one that owns the repository", []delivery.ResolvedCandidate{here("ALPHA")}, ">ALPHA", true},
		{"one that owns another, then one that owns none", []delivery.ResolvedCandidate{elsewhere("ALPHA"), unowned("BETA")}, "BETA>", false},
		{"one that owns none", []delivery.ResolvedCandidate{unowned("ALPHA")}, "ALPHA>", false},
		{"a chain too deep, then one that owns the repository", []delivery.ResolvedCandidate{
			{Key: "ALPHA-4", Project: "ALPHA", TooDeep: true}, here("BETA")}, ">BETA", true},
		{"an issue no row holds, then one that owns none", []delivery.ResolvedCandidate{{Key: "GONE-1"}, unowned("BETA")}, "BETA>", false},
		{"two conditions, a repeat, a terminal and one after it", []delivery.ResolvedCandidate{
			unowned("ALPHA"), elsewhere("GAMMA"), unowned("BETA"), unowned("ALPHA"), here("GAMMA"), unowned("DELTA")}, "ALPHA,BETA>GAMMA", false},
	} {
		list := attributionListOf(c.candidates)
		if got := list.String(); got != c.want || list.constant() != c.constant {
			t.Errorf("%s: the list is %q (constant %t), want %q (constant %t)", c.name, got, list.constant(), c.want, c.constant)
		}
	}

	beta := attributionList{conditions: []string{"BETA"}}
	for _, c := range []struct {
		sharers            []string
		lists              []attributionList
		want               string
		wantSharers, carry []string
	}{
		{nil, nil, ":acme/alpha|", nil, nil},
		{[]string{"GAMMA", "ALPHA", "GAMMA"}, []attributionList{{}, {}}, ":acme/alpha|ALPHA,GAMMA", []string{"ALPHA", "GAMMA"}, nil},
		{nil, []attributionList{beta}, ":acme/alpha||BETA>", nil, []string{"BETA>"}},
		{[]string{"DELTA"}, []attributionList{beta, {}, {conditions: []string{"ALPHA", "BETA"}, terminal: "GAMMA"}, beta},
			":acme/alpha|DELTA|>;ALPHA,BETA>GAMMA;BETA>", []string{"DELTA"}, []string{">", "ALPHA,BETA>GAMMA", "BETA>"}},
	} {
		key := OwnerKey(alphaRepo, c.sharers, c.lists)
		if key != c.want {
			t.Errorf("the owner key of %v and %v is %q, want %q", c.sharers, c.lists, key, c.want)
		}
		if repo, sharers, carried, ok := parseOwner(key); !ok || repo != alphaRepo || !slices.Equal(sharers, c.wantSharers) || !slices.Equal(carried, c.carry) {
			t.Errorf("%q reads back as repository %q, sharers %v and lists %v (%t), want %s, %v and %v", key, repo, sharers, carried, ok, alphaRepo, c.wantSharers, c.carry)
		}
	}
}

// TestTheZeroDayIsTheBuildersDayWithNoRow: ZeroDeliveryDay holds the all-repositories row of every
// delivery and CI series (the staging cards only where the settings name their jobs), each n 0
// and its value zero, those of the evals that read the deploy repository keyed by the repository
// kept, and WithZeros adds a zero row only where the day holds none of that series.
func TestTheZeroDayIsTheBuildersDayWithNoRow(t *testing.T) {
	t.Parallel()
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
// write after one more such issue opens moves the value and the time; and a third write, which
// finds the count it stored, leaves the row as it was, its time and the moment it was written.
func TestTheUnownedP0RowIsTheCountAtItsLastWrite(t *testing.T) {
	t.Parallel()
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
	if err := r.registry.WriteUnownedP0Day(ctx, "EVAL", attributionDay, testBuild); err != nil {
		t.Fatalf("write the unowned-p0 row a third time: %v", err)
	}
	third := r.day(t, EvalID("EVAL", SlugUnownedP0), SeriesUnownedP0, "", attributionDay)
	if third.LastValue == nil || *third.LastValue != 2 || third.LastAt == nil || !third.LastAt.Equal(*second.LastAt) || !third.UpdatedAt.Equal(second.UpdatedAt) {
		t.Fatalf("the row written again with the count unchanged = last_value %s at %v, updated at %s; want 2 at %v, updated at %s, as the second write left it",
			showValue(third.LastValue), third.LastAt, third.UpdatedAt.Format(time.RFC3339Nano), second.LastAt, second.UpdatedAt.Format(time.RFC3339Nano))
	}
	t.Logf("unowned-p0 of EVAL on %s: %g at %s, then %g at %s, kept by the third write", attributionDay.Format(time.DateOnly),
		*first.LastValue, first.LastAt.Format(time.RFC3339Nano), *second.LastValue, second.LastAt.Format(time.RFC3339Nano))
}

// unownedP0Facts is project's unowned-p0 rows of day, one `<repo> n=… sum=… last=…` each, repo
// "all" for the project's own row, in order.
func unownedP0Facts(t *testing.T, r rig, project string, day time.Time) []string {
	t.Helper()
	var facts []string
	for _, row := range r.stored(t, project) {
		if row.EvalID == EvalID(project, SlugUnownedP0) && row.Day.Equal(day) {
			facts = append(facts, fmt.Sprintf("%s n=%d sum=%s last=%s", cmp.Or(row.Repo, "all"), row.N, showValue(row.Sum), showValue(row.LastValue)))
		}
	}
	slices.Sort(facts)
	return facts
}

// TestTheUnownedP0RowsPartitionTheCountByRepository: beside the project's row, WriteUnownedP0Day
// writes a row for each repository its open P0 issues with no owner resolve to (the rule the
// Legion rows cut an issue by, legionfacts.ReadRepos): an issue's own repository, its parent's,
// and legionfacts.DefaultRepo for one whose chain names none. Each row's count is its n, sum and
// last value, so the repositories' rows add up to the project's. A claim that leaves a repository
// with no such issue writes that repository a row of 0 for the day, so a read of it never reads
// the count that went away; the next day, whose write finds that row's 0 the repository's newest,
// writes it none.
func TestTheUnownedP0RowsPartitionTheCountByRepository(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	actor := `{"kind":"system","id":"p0-test"}`
	insert := func(number, priority int, repo, parent string, claimed bool) {
		t.Helper()
		var claimedBy, claimedAt any
		if claimed {
			claimedBy, claimedAt = actor, attributionDay
		}
		if _, err := r.database.Pool.Exec(ctx, `
			insert into issues (key, project_key, number, title, status, rank, priority, repo, parent_key, created_by, created_at, claimed_by, claimed_at)
			values ($1, 'EVAL', $2, 'Incident', 'triage', $3, $4, nullif($5, ''), nullif($6, ''), $7, $8, $9, $10)
		`, fmt.Sprintf("EVAL-%d", number), number, fmt.Sprintf("%08d", number), priority, repo, parent, actor, attributionDay, claimedBy, claimedAt); err != nil {
			t.Fatalf("write issue %d: %v", number, err)
		}
	}
	insert(1, 0, "acme/web", "", false)
	insert(2, 2, "acme/api", "", false)
	insert(3, 0, "", "EVAL-2", false)
	insert(4, 0, "", "", false)
	insert(5, 0, "", "", false)
	insert(6, 0, "acme/web", "", true)
	write := func(day time.Time) {
		t.Helper()
		if err := r.registry.WriteUnownedP0Day(ctx, "EVAL", day, testBuild); err != nil {
			t.Fatalf("write the unowned-p0 rows of %s: %v", day.Format(time.DateOnly), err)
		}
	}
	expect := func(what string, day time.Time, want ...string) {
		t.Helper()
		if got := unownedP0Facts(t, r, "EVAL", day); !slices.Equal(got, want) {
			t.Errorf("%s, the unowned-p0 rows of %s are\n\t%s\nwant\n\t%s", what, day.Format(time.DateOnly), strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
		}
	}

	// EVAL-1 in its own repository, EVAL-3 in its parent's, EVAL-4 and EVAL-5 in none; EVAL-2 is
	// no P0 and EVAL-6 is claimed.
	write(attributionDay)
	expect("with four issues counted", attributionDay,
		":default n=2 sum=2 last=2", "acme/api n=1 sum=1 last=1", "acme/web n=1 sum=1 last=1", "all n=4 sum=4 last=4")

	if _, err := r.database.Pool.Exec(ctx, `update issues set claimed_by = $1, claimed_at = $2 where key = 'EVAL-1'`, actor, attributionDay); err != nil {
		t.Fatalf("claim EVAL-1: %v", err)
	}
	write(attributionDay)
	expect("once EVAL-1 is claimed", attributionDay,
		":default n=2 sum=2 last=2", "acme/api n=1 sum=1 last=1", "acme/web n=0 sum=0 last=0", "all n=3 sum=3 last=3")

	next := attributionDay.AddDate(0, 0, 1)
	write(next)
	expect("on the next day", next, ":default n=2 sum=2 last=2", "acme/api n=1 sum=1 last=1", "all n=3 sum=3 last=3")
}

// TestAMoveAboveAnUnownedP0IssueMovesItsCount: a write that moves the parent or the repository of
// an issue that is no P0 moves the repository each unowned P0 issue below it resolves to, and so
// the repository it is counted under: the pass that reads the write's event writes the project's
// unowned-p0 rows of today again. EVAL-2, an unowned P0 issue below EVAL-1, which names no
// repository, is counted under legionfacts.DefaultRepo; then EVAL-1 moves below EVAL-3, of
// acme/api, and then gets acme/web as its own.
func TestAMoveAboveAnUnownedP0IssueMovesItsCount(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	r.exec(t, "the issues", `
		insert into issues (key, project_key, number, title, status, rank, priority, repo, parent_key, created_by)
		values ('EVAL-3', 'EVAL', 3, 'A service', 'in_progress', '00000003', 2, 'acme/api', null, '{"kind":"user","id":"alice"}'),
			('EVAL-1', 'EVAL', 1, 'A project', 'in_progress', '00000001', 2, null, null, '{"kind":"user","id":"alice"}'),
			('EVAL-2', 'EVAL', 2, 'Broken', 'triage', '00000002', 0, null, 'EVAL-1', '{"kind":"user","id":"alice"}')`)
	r.settle(t)
	expect := func(what string, want ...string) {
		t.Helper()
		if got := unownedP0Facts(t, r.rig, "EVAL", refresherToday); !slices.Equal(got, want) {
			t.Errorf("%s, the unowned-p0 rows of today are\n\t%s\nwant\n\t%s", what, strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
		}
	}
	expect("before the moves", ":default n=1 sum=1 last=1", "all n=1 sum=1 last=1")

	r.exec(t, "move EVAL-1 below EVAL-3", `update issues set parent_key = 'EVAL-3' where key = 'EVAL-1'`)
	r.writeIssueEvent(t, "EVAL-1", "issue.updated", `{"key":"EVAL-1","priority":2,"status":"in_progress","changed":["parent"]}`)
	r.pass(t)
	expect("once EVAL-1 is below EVAL-3", ":default n=0 sum=0 last=0", "acme/api n=1 sum=1 last=1", "all n=1 sum=1 last=1")

	r.exec(t, "give EVAL-1 a repository", `update issues set repo = 'acme/web' where key = 'EVAL-1'`)
	r.writeIssueEvent(t, "EVAL-1", "issue.updated", `{"key":"EVAL-1","priority":2,"status":"in_progress","changed":["repo"]}`)
	r.pass(t)
	expect("once EVAL-1 names acme/web", ":default n=0 sum=0 last=0", "acme/api n=0 sum=0 last=0", "acme/web n=1 sum=1 last=1", "all n=1 sum=1 last=1")
}

// TestEachFilterOfWhatADayNoLongerHoldsReadsItsListOnce: each statement that finds what a day no
// longer holds against the list its write gives it (leftDaysStatement's,
// leftDaysOwedStatement's and goneMembersDeleteStatement's items and deleteStaleDaysStatement's
// rows) reads that list once, with the write's values and under
// the generic plan a statement pgx caches can settle on after its fifth run, which estimates every
// list at 10 rows: the list runs under no SubPlan, no nested loop runs it again for each row of
// its other side, and the filter is a SetOp Except over the list or a hash or merge anti-join. The
// delete of rows is explained as one statement of the whole list over every key (no bound
// of its range, staleRuns), the most rows any of its statements holds. standingPartsQuery takes
// no list of the write's own readings (standingParts leaves them out as it reads them, since their
// ids grow with the batch), and its generic plan matches each reading to its keys by their day, as
// a hash or merge join on it or an index condition, rather than testing each reading against every
// key of its series. The day is past work_mem, as one of INFRA's is: 80,000 items across 40 deploy
// repositories and 80,000 rows written through writeProjectDay, and 80,000 readings of a logged
// eval on the same day, the three tables analyzed. Each statement is explained with the arrays a
// second write of the day sends, one item, one row and one reading fewer than stored, in a
// transaction prepared as its write prepares it, and then with literals for its generic plan,
// which reads none of them.
//
// The test judges the plan and not the clock, which a loaded runner would fail; k20 measures the
// write's time on the build farm. That measurement is what this test stands for: written as
// `not in (…)`, the filters ran the list again for each stored row once it outgrew work_mem, so
// one day's write of k20's load at INFRA's size held eval_days for 13 minutes, and on the test
// Postgres a day of 80,000 items took more than 180 s, where an anti-join took 1.6 s; and written
// as `not exists`, the stale rows' generic plan was a nested-loop anti-join that had not written
// 80,000 rows after 2 minutes.
func TestEachFilterOfWhatADayNoLongerHoldsReadsItsListOnce(t *testing.T) {
	t.Parallel()
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
	if err := r.registry.writeProjectDay(ctx, deliveryFamily, ProjectDay{Project: "EVAL", Day: day, Rows: rows, Members: members}, testBuild, true); err != nil {
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
	memberProjects, memberDays := make([]string, len(again)), make([]time.Time, len(again))
	for i, member := range again {
		families[i], memberDeployRepos[i], keys[i], shippedBy[i] = member.Family, member.DeployRepo, member.RowKey, member.ShippedBy
		memberProjects[i], memberDays[i] = "EVAL", day
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
		// lists is the CTEs a form of the statement holds its list in, whose scans are scans of
		// the list as much as the function scan of unnest that fills them.
		lists                []string
		arguments            []any
		parameters, literals []string
	}{
		{
			name: "leftDaysOwedStatement", statement: leftDaysOwedStatement, lists: []string{"given"},
			arguments: []any{[]string{"EVAL"}, []time.Time{day}, memberProjects, memberDays, families, memberDeployRepos, keys, shippedBy,
				deliveryDayFamilies, KindDelivery, []string(nil), []string(nil)},
			parameters: []string{"text[]", "date[]", "text[]", "date[]", "text[]", "text[]", "text[]", "text[]", "text[]", "text", "text[]", "text[]"},
			literals: []string{"array['EVAL']", "array[" + date + "]::date[]", "array['EVAL']", "array[" + date + "]::date[]", "array['delivery']",
				"array['acme/deploy-00']", "array['pull_request:acme/web#1']", "array['']", "array['delivery', 'ci']", "'delivery'", "null::text[]",
				"null::text[]"},
		},
		{
			name: "leftDaysStatement", statement: leftDaysStatement, lists: []string{"given"},
			arguments: []any{[]string{"EVAL"}, []time.Time{day}, memberProjects, memberDays, families, memberDeployRepos, keys, shippedBy,
				deliveryDayFamilies, KindDelivery, []string(nil), []string(nil)},
			parameters: []string{"text[]", "date[]", "text[]", "date[]", "text[]", "text[]", "text[]", "text[]", "text[]", "text", "text[]", "text[]"},
			literals: []string{"array['EVAL']", "array[" + date + "]::date[]", "array['EVAL']", "array[" + date + "]::date[]", "array['delivery']",
				"array['acme/deploy-00']", "array['pull_request:acme/web#1']", "array['']", "array['delivery', 'ci']", "'delivery'", "null::text[]",
				"null::text[]"},
		},
		{
			name: "goneMembersDeleteStatement", statement: goneMembersDeleteStatement,
			arguments: []any{[]string{"EVAL"}, []time.Time{day}, deliveryDayFamilies, memberProjects, families, memberDeployRepos, keys, memberDays,
				[]string(nil), []string(nil)},
			parameters: []string{"text[]", "date[]", "text[]", "text[]", "text[]", "text[]", "text[]", "date[]", "text[]", "text[]"},
			literals: []string{"array['EVAL']", "array[" + date + "]::date[]", "array['delivery', 'ci']", "array['EVAL']", "array['delivery']",
				"array['acme/deploy-00']", "array['pull_request:acme/web#1']", "array[" + date + "]::date[]", "null::text[]", "null::text[]"},
		},
		{
			name: "deleteStaleDaysStatement", statement: deleteStaleDaysStatement,
			arguments: []any{[]string{"EVAL"}, []time.Time{day}, evalIDs, writerComputation, projects, evals, rowDeployRepos, series, repos, days,
				[]string(nil), []string(nil)},
			parameters: []string{"text[]", "date[]", "text[]", "text", "text[]", "text[]", "text[]", "text[]", "text[]", "date[]", "text[]", "text[]"},
			literals: []string{"array['EVAL']", "array[" + date + "]::date[]", "array['EVAL/deploy-frequency']", "'computation'", "array['EVAL']",
				"array['EVAL/deploy-frequency']", "array['acme/deploy-00']", "array['deploys']", "array['acme/repo-00000']", "array[" + date + "]::date[]",
				"null::text[]", "null::text[]"},
		},
	} {
		t.Run(filter.name, func(t *testing.T) {
			tx, err := r.database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin %s's transaction: %v", filter.name, err)
			}
			defer tx.Rollback(ctx)
			explaintest.RequireStatementReadsListOnce(t, ctx, tx, filter.name, filter.statement, filter.arguments,
				filter.parameters, filter.literals, filter.lists...)
		})
	}
	// The generic plan of standingPartsQuery matches each reading to its keys by a hash join whose
	// Hash Cond is the series and the day, (date_trunc('day', r.measured_at, 'UTC') = k.day).
	t.Run("standingPartsQuery", func(t *testing.T) {
		tx, err := r.database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin standingPartsQuery's transaction: %v", err)
		}
		defer tx.Rollback(ctx)
		generic := explaintest.GenericPlan(t, ctx, tx, "standingPartsQuery", standingPartsQuery, []string{"text", "text[]", "text[]", "timestamptz[]"},
			[]string{"'" + evalID + "'", "array['minutes']", "array['']", "array[" + date + "]::timestamptz[]"})
		explaintest.RequireJoinedOn(t, generic, "standingPartsQuery under the generic plan", "k.day")
		t.Logf("standingPartsQuery under the generic plan:\n%s", explaintest.Outline(t, generic))
	})

	// The second write itself, which leaves one item and one row fewer: the row the day no longer
	// holds is deleted.
	if err := r.registry.writeProjectDay(ctx, deliveryFamily, ProjectDay{Project: "EVAL", Day: day, Rows: kept, Members: again}, testBuild, true); err != nil {
		t.Fatalf("write the day again with one item and one row fewer: %v", err)
	}
	if held := r.count(t, `select count(*) from eval_day_members where project_key = 'EVAL'`); held != items-1 {
		t.Errorf("eval_day_members holds %d items of EVAL, want %d", held, items-1)
	}
	if stored, zero := r.count(t, `select count(*) from eval_days where project_key = 'EVAL' and writer = 'computation'`),
		r.count(t, `select count(*) from eval_days where project_key = 'EVAL' and writer = 'computation' and n = 0`); stored != items-1 || zero != 0 {
		t.Errorf("eval_days holds %d rows of EVAL, %d of them zero rows, want %d and none: the row the day no longer holds deleted", stored, zero, items-1)
	}
}
