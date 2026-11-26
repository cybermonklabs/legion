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
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/explaintest"
	"github.com/sjawhar/envoy/internal/testwait"
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
// four projects, ALPHA, BETA, GAMMA (no row) and OWNER, whose one deploy source names acme/alpha,
// the deploy repository, which is no project's default. No project has a default repository, and
// no pull request's attribution inputs are stored (not read from GitHub yet), so each counts by
// the one list its row's issue_key gives (evals' attribution): its issue's project, or none, which
// reaches no project; but a deploy that ships no pull request counts for its source's project
// (dispatch://INFRA-978, R12).
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
//     none, so they count for OWNER alone, the project whose source reads acme/alpha;
//   - PR-checks run 201 of acme/alpha#3 counts for ALPHA, its pull request's project, and so does
//     its job; PR-checks run 202 of acme/alpha#2 counts for no project.
//
// It answers acme/alpha's rule as the pass reads it (delivery.DeployRules).
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
	exec("the projects", `insert into projects (key, name, default_assignee, next_number) values ('ALPHA', 'Alpha', 'alice', 2), ('BETA', 'Beta', 'alice', 2), ('GAMMA', 'Gamma', 'alice', 1), ('OWNER', 'Owner', 'alice', 1)`)
	exec("the issues", `
		insert into issues (key, project_key, number, title, status, rank, created_by, created_at)
		values ('ALPHA-1', 'ALPHA', 1, 'Alpha work', 'in_progress', '00000001', $1, $2),
		       ('BETA-1', 'BETA', 1, 'Beta work', 'in_progress', '00000001', $1, $2)`, actor, attributionDay.AddDate(0, 0, -1))

	production := "production / apply"
	settings := delivery.DeliverySettings{
		DeployRepo: alphaRepo, DeployWorkflowPath: ".github/workflows/deploy.yml", ProductionJobNames: []string{production},
		PRChecksWorkflowPath: ".github/workflows/pr-checks.yml", PopulationAuthors: []string{"dev-ana"}, ExcludedRepos: []string{},
	}
	saved := putSources(t, pool, "OWNER", settings.PopulationAuthors, deliverytest.SourceOf(settings, new(alphaRepo)))
	stored := saved.Rules()[0]
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
	if pass.delivers() {
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
		if pass.delivers() {
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
// issue for its repository's owner key (owners.go, INFRA-2916 R15); the deploy that ships pull
// requests of two projects counts once in each, its production job's push run too, and, as it
// ships one with no issue, once for its repository's owner key with those two as its sharers; the
// deploys that ship none count for OWNER alone, the project whose source reads acme/alpha
// (INFRA-978 R12), and for no owner key; a PR-checks run counts for its pull request's project,
// and one of a pull request with no issue for that repository's owner key; every via_repository is
// 0 in ALPHA's and BETA's rows and n in an owner key's; and GAMMA, with nothing, gets no row. Then
// the day is written again unchanged (no row rewritten), and once acme/alpha#3 names BETA-1 the
// day's rewrite moves it, the
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

	reads, err := r.registry.readDeliveryDays(ctx, []keptRepo{{name: alphaRepo, deploys: true, rule: settings}}, []time.Time{measures.Midnight(attributionDay)}, nil, nil)
	if err != nil {
		t.Fatalf("read the day: %v", err)
	}
	read := reads[0]
	wantOwners := []string{OwnerKey(alphaRepo, nil, nil), OwnerKey(alphaRepo, []string{"ALPHA", "BETA"}, nil), OwnerKey(zetaRepo, nil, nil)}
	if got := dayProjects(read); !slices.Equal(got, slices.Concat(wantOwners, []string{"ALPHA", "BETA", "OWNER"})) {
		t.Fatalf("the day's rows count for %v, want ALPHA, BETA, OWNER and the owner keys %v (acme/alpha#2 and acme/zeta#1 through their repositories, runs 102 and 103 for OWNER, GAMMA holds nothing)",
			got, wantOwners)
	}
	t.Logf("wrote %v", r.refreshDays(t, nil, attributionDay))

	// Run 101, created at 10:30, shipped at 10:49, when its production job completed; every job of
	// the history runs 17 minutes and every run 19.
	want := map[string][]string{
		"ALPHA": {
			// acme/alpha#3 and acme/beta#1, both through ALPHA-1.
			"merge-to-production minutes all: n=1 via=0 samples=[29]",
			"merge-to-production minutes acme/alpha: n=1 via=0 samples=[29]",
			"merge-to-production minutes acme/beta: n=0 via=0 samples=[]",
			// No pull request records a first commit, so first-commit-to-production times none and,
			// before its first day, holds no row (dropUnshipped).
			"open-to-production minutes all: n=1 via=0 samples=[89]",
			"open-to-production minutes acme/alpha: n=1 via=0 samples=[89]",
			"open-to-production minutes acme/beta: n=0 via=0 samples=[]",
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
			"delivery pull_request:acme/alpha#3 acme/alpha 2026-11-03", "delivery pull_request:acme/alpha#3 acme/alpha 2026-11-03 as acme/alpha",
			"delivery pull_request:acme/beta#1 acme/beta 2026-11-03", "delivery pull_request:acme/beta#1 acme/beta 2026-11-03 as acme/alpha",
			"delivery run:acme/alpha/101 acme/alpha 2026-11-03 as acme/alpha",
		},
		"BETA": {
			"ci run:acme/alpha/101 acme/alpha 2026-11-03 as acme/alpha",
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
	for _, none := range []string{"pull_request:acme/zeta#1", "pull_request:acme/alpha#2", "run:acme/alpha/202"} {
		if n := r.count(t, `select count(*) from eval_day_members where row_key = $1 and not starts_with(project_key, ':')`, none); n != 0 {
			t.Errorf("%s is counted in %d projects, want none: it reaches no project through an issue", none, n)
		}
	}
	// Runs 102 and 103 ship no pull request: they count for OWNER, whose source reads acme/alpha,
	// and for no other project and no owner key (R12), as do their push runs.
	for _, owned := range []string{"run:acme/alpha/102", "run:acme/alpha/103"} {
		if n := r.count(t, `select count(*) from eval_day_members where row_key = $1 and project_key <> 'OWNER'`, owned); n != 0 {
			t.Errorf("%s is counted in %d projects or owner keys besides OWNER, want none", owned, n)
		}
		if n := r.count(t, `select count(*) from eval_day_members where row_key = $1 and project_key = 'OWNER' and family = $2`, owned, KindDelivery); n != 1 {
			t.Errorf("%s is counted %d times in OWNER's deploys, want once", owned, n)
		}
	}
	// What no issue attributes is in its repository's owner-key rows, and every one of them
	// reached its owner through the repository: its via_repository is its n.
	for _, owner := range wantOwners {
		facts := allFacts(r.stored(t, owner))
		if len(facts) == 0 {
			t.Errorf("%s holds no day row, want the rows no issue attributes", owner)
		}
		for _, fact := range facts {
			n, via := -1, -2
			if at := strings.Index(fact, "n="); at < 0 {
				t.Errorf("%s holds %q, which states no count", owner, fact)
			} else if _, err := fmt.Sscanf(fact[at:], "n=%d via=%d", &n, &via); err != nil || n != via {
				t.Errorf("%s holds %q, want every row counted through the repository (via_repository = n)", owner, fact)
			}
		}
		compared += len(facts)
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
	if got := r.members(t, "BETA"); !slices.Contains(got, "delivery pull_request:acme/alpha#3 acme/alpha 2026-11-03") ||
		!slices.Contains(got, "delivery pull_request:acme/alpha#3 acme/alpha 2026-11-03 as acme/alpha") ||
		!slices.Contains(got, "ci run:acme/alpha/201 acme/alpha 2026-11-03 as acme/alpha") {
		t.Errorf("BETA's items after the move: %v, want acme/alpha#3 among them, under no deploy repository and under acme/alpha, and its PR-checks run 201", got)
	}
	t.Logf("compared %d day rows of three projects; after acme/alpha#3 moved to BETA, ALPHA holds %d", compared, len(alpha))
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
	zero := ZeroDeliveryDay("ALPHA", day, []delivery.DeliverySettings{settings})
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
	t.Logf("unowned-p0 of EVAL on %s: %g at %s, then %g at %s", attributionDay.Format(time.DateOnly),
		*first.LastValue, first.LastAt.Format(time.RFC3339Nano), *second.LastValue, second.LastAt.Format(time.RFC3339Nano))
}

// TestEachFilterOfWhatADayNoLongerHoldsReadsItsListOnce: each statement that finds what a day no
// longer holds against the list its write gives it (leftDaysOwedStatement's and
// goneMembersDeleteStatement's items, deleteStaleDaysStatement's and
// deleteStaleSettingsDaysStatement's rows) reads that list once, with the write's values and under
// the generic plan a statement pgx caches can settle on after its fifth run, which estimates every
// list at 10 rows: the list runs under no SubPlan, no nested loop runs it again for each row of
// its other side, and the filter is a SetOp Except over the list or a hash or merge anti-join. The
// two deletes of rows are explained as one statement of the whole list over every key (no bound
// of its range, staleRuns), the most rows any of their statements holds. standingPartsQuery takes
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
		// settings are the `set local` statements the write runs the statement under.
		settings []string
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
		{
			name: "deleteStaleSettingsDaysStatement", statement: deleteStaleSettingsDaysStatement,
			settings: []string{"set local plan_cache_mode = force_custom_plan", "set local enable_mergejoin = off"},
			arguments: []any{[]string{"EVAL"}, []time.Time{day}, projects, evals, series, repos, days, evalIDs, writerComputation,
				rowDeployRepos, deployRepos, []string(nil), []string(nil)},
			parameters: []string{"text[]", "date[]", "text[]", "text[]", "text[]", "text[]", "date[]", "text[]", "text", "text[]", "text[]",
				"text[]", "text[]"},
			literals: []string{"array['EVAL']", "array[" + date + "]::date[]", "array['EVAL']", "array['EVAL/deploy-frequency']", "array['deploys']",
				"array['acme/repo-00000']", "array[" + date + "]::date[]", "array['EVAL/deploy-frequency']", "'computation'",
				"array['acme/deploy-00']", "array['acme/deploy-00']", "null::text[]", "null::text[]"},
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

// TestTheStaleRowsOfASettingsRewriteMatchTheWrittenDaysByProjectAndDay: deleteStaleSettingsDays
// runs its statement under a plan whose read of what the days written no longer hold (gone)
// matches the stored rows to those days by their project and their day at one node, a hash or
// merge join or an index scan (explaintest.RequireJoinedOnAll), and gives enable_mergejoin back the
// value it held. A merge join on the project alone, which the primary key's order gives the planner
// for nothing, tests each row's day against every day written of its project: a settings change
// that rewrote 200 days of one project tested 434,218 pairs to keep its 2,182 rows. The rows are
// EVAL's of the six evals a production rule's change moves, in two repositories under one deploy
// repository, on each of 200 days, beside 10 evals of six series each of EVAL and of another
// project on the same days: 26,400 rows, fewer than the 30,000 ANALYZE samples, so it reads every
// row and the statistics are the same on every run. They are written in one transaction that
// commits synchronously, then vacuumed and analyzed, every page all-visible, so the primary key's
// index-only scan reads eval_days in the project's order: with asynchronous commits, which the
// pod's test server runs, a vacuum just after a commit can mark no page all-visible (as
// embedqueue's TestCoverageFetchesNoWindowRowByRowWhileTheStatisticsLagTheVisibilityMap found).
// Even after a synchronous commit, a vacuum marks a page all-visible only once the commit status
// of its rows' transaction is hinted, which waits until the WAL is flushed past the newest commit
// of any transaction whose id shares its commit-log group of 32: an asynchronous commit of a
// neighbouring id, in another database of the same server (a test running beside this one), keeps
// the pages unmarked until the WAL writer flushes it, at most a few hundred milliseconds later. So
// the test vacuums again until every page is all-visible, for at most allVisibleBound.
// The write rewrites those 200 days and holds every row but one, which it deletes, in a
// transaction planned as rewriteSettings plans its own; its one statement (EVAL's, whose rows fit
// in one) names EVAL's six evals. With merge joins on, the planner estimates the scan of eval_days
// at a few hundred rows of its 2,400 and merges it with the days on the project alone, testing the
// day of each row against all 200 days.
//
// The test reads the plan the statement runs under (explainingTx), not the clock.
func TestTheStaleRowsOfASettingsRewriteMatchTheWrittenDaysByProjectAndDay(t *testing.T) {
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
	allVisible := func() bool {
		if err := r.database.Pool.QueryRow(ctx, `select relpages, relallvisible from pg_class where oid = 'eval_days'::regclass`).Scan(&pages, &visible); err != nil {
			t.Fatalf("read how many pages of eval_days are all-visible: %v", err)
		}
		if pages > 0 && visible == pages {
			return true
		}
		if _, err := r.database.Pool.Exec(ctx, `vacuum eval_days`); err != nil {
			t.Fatalf("vacuum eval_days again: %v", err)
		}
		return false
	}
	if !testwait.Holds(allVisibleBound, 100*time.Millisecond, allVisible) {
		t.Fatalf("the vacuum marked %d of eval_days' %d pages all-visible; the test needs every page", visible, pages)
	}

	projects, written := make([]string, days), make([]time.Time, days)
	for day := range days {
		projects[day], written[day] = "EVAL", first.AddDate(0, 0, day)
	}
	tx, err := r.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the rewrite's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `set local plan_cache_mode = force_custom_plan`); err != nil {
		t.Fatalf("plan the rewrite's transaction as rewriteSettings does: %v", err)
	}
	explaining := &explainingTx{Tx: tx, explained: deleteStaleSettingsDaysStatement}
	if err := deleteStaleSettingsDays(ctx, explaining, projects, written, slugs, []string{deployRepo}, stored[1:]); err != nil {
		t.Fatalf("deleteStaleSettingsDays: %v", err)
	}
	if explaining.plan == nil {
		t.Fatal("deleteStaleSettingsDays ran no deleteStaleSettingsDaysStatement")
	}
	plan := explaintest.OnePlan(t, explaining.plan)
	t.Logf("deleteStaleSettingsDaysStatement as deleteStaleSettingsDays runs it over %d days written:\n%s", days, explaintest.Outline(t, plan))
	explaintest.RequireJoinedOnAll(t, plan, "deleteStaleSettingsDaysStatement as deleteStaleSettingsDays runs it", "CTE gone", "project_key", "day")
	var mergeJoins string
	if err := tx.QueryRow(ctx, `select current_setting('enable_mergejoin')`).Scan(&mergeJoins); err != nil {
		t.Fatalf("read enable_mergejoin: %v", err)
	}
	if mergeJoins != "on" {
		t.Errorf("enable_mergejoin is %s after deleteStaleSettingsDays, want on, the value it held before", mergeJoins)
	}
	var held, zero int
	if err := tx.QueryRow(ctx, `select count(*), count(*) filter (where n = 0) from eval_days where project_key = 'EVAL' and deploy_repo = $1`,
		deployRepo).Scan(&held, &zero); err != nil {
		t.Fatalf("count EVAL's rows of the moved evals: %v", err)
	}
	if held != len(stored)-1 || zero != 0 {
		t.Errorf("eval_days holds %d rows of EVAL's moved evals, %d of them zero rows, want %d and none: the row the write no longer holds deleted",
			held, zero, len(stored)-1)
	}
}

// allVisibleBound is how long TestTheStaleRowsOfASettingsRewriteMatchTheWrittenDaysByProjectAndDay
// vacuums eval_days again for every page to be all-visible: well past the WAL writer's flush of
// an asynchronous commit, which wal_writer_delay (200 ms by default) bounds to three times it.
const allVisibleBound = 5 * time.Second

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

// TestADeployThatShipsNoPullRequestCountsForItsSourcesProjectAlone (dispatch://INFRA-978, R12 and
// A8): over the settled three-source history, acme/alpha-ops' deploy 2001, which ships no pull
// request in a repository that is no project's default, counts for ALPHA, whose source names
// acme/alpha-ops, and for no other project; acme/alpha-app's 1001, in ALPHA's default repository,
// counts for ALPHA once, not once for its source and once more for the default repository; each
// reaches ALPHA through its repository (via_repository). Red under the rule that counts a deploy
// shipping no pull request for no project: ALPHA counts neither.
func TestADeployThatShipsNoPullRequestCountsForItsSourcesProjectAlone(t *testing.T) {
	r := newRefresherRig(t)
	r.seedSources(t)
	r.settle(t)
	for _, deploy := range []string{RunMember(alphaOps, 2001), RunMember(alphaApp, 1001)} {
		if n := r.count(t, `select count(*) from eval_day_members where row_key = $1 and family = $2 and project_key <> 'ALPHA'`, deploy, KindDelivery); n != 0 {
			t.Errorf("%s counts for %d other projects' deploys, want none", deploy, n)
		}
		if n := r.count(t, `select count(*) from eval_day_members where row_key = $1 and family = $2 and project_key = 'ALPHA'`, deploy, KindDelivery); n != 1 {
			t.Errorf("%s counts %d times among ALPHA's deploys, want once", deploy, n)
		}
	}
	if got := r.deploysOn(t, "ALPHA", sourcesDay); got != (defaultTally{n: 2, via: 2}) {
		t.Errorf("ALPHA's deploy-frequency counts %d deploys, %d through a repository; want 2 and 2", got.n, got.via)
	}
}
