package evalbench

import (
	"cmp"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// shown is *p, or nil.
func shown[T any](p *T) string {
	if p == nil {
		return "nil"
	}
	return fmt.Sprint(*p)
}

// infraDigest is the SHA-256 of everything INFRA's history plans at spec, the issues, their
// events, the pull requests, the runs and each run's jobs, each field as it is drawn.
func infraDigest(spec Spec) string {
	hash := sha256.New()
	history := planIssues(spec, infraFixture)
	for _, i := range history.issues {
		fmt.Fprintln(hash, i.number, i.key, i.parent, i.repo, i.opened, shown(i.closed), i.status, shown(i.priority), i.reached, i.stretch, i.claimed, i.routed, i.events, i.observed)
	}
	for _, event := range planEvents(spec, history) {
		fmt.Fprintf(hash, "%+v\n", event)
	}
	prs := planPullRequests(spec, history)
	for _, pr := range prs {
		fmt.Fprintln(hash, pr.repo, pr.number, pr.created, pr.merged, shown(pr.firstCommit), pr.author, pr.additions, pr.deletions, pr.rework, pr.issue, pr.session, pr.mergeCommitSHA)
	}
	deploy := infraFixture.deploys[0]
	for i, run := range planRuns(spec, infraFixture, deploy, prs) {
		row := run.build(spec, deploy.firstRunID+int64(i))
		fmt.Fprintln(hash, run.kind, run.created, run.prNumber, run.draw, row.id, shown(row.prNumber), row.sha, row.headAt, row.started, row.created, shown(row.completed), shown(row.conclusion), row.branch, row.event)
		for _, job := range row.jobs {
			fmt.Fprintln(hash, job.name, job.started, shown(job.completed), shown(job.conclusion))
		}
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

// TestTheK14PlannersShareNoStateThatMovesINFRAsHistory: the generator draws each project's history
// from streams of its own, and INFRA's from the unprefixed pre-k14 streams (infraFixture.rand), so
// planning the k14 projects' issues, pull requests and runs between two plans of INFRA's leaves
// INFRA's issues, events, pull requests, runs and jobs identical field for field at both rates: no
// state the k14 planners share with INFRA's (a random stream, a package variable) moves INFRA's
// history. No planner reads Spec.K14, so the flag itself digests alike on both sides; the test holds
// the k14 planners apart from INFRA's, and INFRA's fixture to its pre-k14 streams and run ids.
// (math.Exp differs by up to 1 ULP between amd64 and arm64, so no hardcoded architecture-specific
// digest is compared.)
func TestTheK14PlannersShareNoStateThatMovesINFRAsHistory(t *testing.T) {
	t.Parallel()
	for _, tenTimes := range []bool{false, true} {
		base := Spec{Now: time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC), Seed: 2916, Scale: 0.01, TenTimes: tenTimes}
		withK14 := base
		withK14.K14 = true
		want := infraDigest(base)
		for _, fixture := range k14Fixtures {
			history := planIssues(withK14, fixture)
			prs := planPullRequests(withK14, history)
			for _, deploy := range fixture.deploys {
				planRuns(withK14, fixture, deploy, prs)
			}
		}
		if got := infraDigest(withK14); got != want {
			t.Errorf("ten times %t: INFRA's history with K14 digests to %s; want %s, without K14", tenTimes, got, want)
		}
		if infraFixture.key != Project || infraFixture.deploys[0].firstRunID != firstRunID || infraFixture.rand(base.Seed, "issues").Uint64() != newRand(base.Seed, "issues").Uint64() {
			t.Errorf("ten times %t: infraFixture departed from the unprefixed pre-k14 stream or run ids", tenTimes)
		}
	}
}

// runSpan is one deploy repository's run ids, [first, first+runs).
type runSpan struct {
	repo        string
	first, runs int64
}

// TestK14ProjectsDrawTheirOwnHistoriesAtINFRAsRate: each k14 project's history is INFRA's shape at
// INFRA's rate under its own key and repositories: as many issues as INFRA's, each keyed by the
// project; its pull requests in its own repositories, naming its own issues; and each of its deploy
// repositories INFRA's runs a day, whose pull-request runs name that repository's pull requests,
// from a first run id no other repository's ids meet, INFRA's and the failure day's included, at
// ten times the rate. Its settings are a save the delivery rules accept.
func TestK14ProjectsDrawTheirOwnHistoriesAtINFRAsRate(t *testing.T) {
	t.Parallel()
	spec := Spec{Now: time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC), Seed: 2916, Scale: 0.01, TenTimes: true}
	deploys, checks := spec.dailyRuns()
	perRepo := int64(runDays * (deploys + checks))
	maxRuns := int64(runDays * 10 * (deployRuns + checkRuns))
	spans := []runSpan{{repo: Repos[0], first: firstRunID, runs: maxRuns}}
	for _, fixture := range k14Fixtures {
		history := planIssues(spec, fixture)
		if len(history.issues) != spec.scaled(issues, 12) {
			t.Errorf("%s holds %d issues; want INFRA's %d", fixture.key, len(history.issues), spec.scaled(issues, 12))
		}
		for _, issue := range history.issues {
			if !strings.HasPrefix(issue.key, fixture.key+"-") || issue.repo != "" && !slices.Contains(fixture.repos[:], issue.repo) {
				t.Fatalf("%s's issue %s names %q; want its own key and one of %v", fixture.key, issue.key, issue.repo, fixture.repos)
			}
		}
		prs := planPullRequests(spec, history)
		for _, pr := range prs {
			if !slices.Contains(fixture.repos[:], pr.repo) || pr.issue != "" && !strings.HasPrefix(pr.issue, fixture.key+"-") {
				t.Fatalf("%s's pull request %s#%d names %q; want one of its repositories and its own issues", fixture.key, pr.repo, pr.number, pr.issue)
			}
		}
		for _, deploy := range fixture.deploys {
			runs := planRuns(spec, fixture, deploy, prs)
			if int64(len(runs)) != perRepo {
				t.Errorf("%s's %s holds %d runs; want INFRA's %d", fixture.key, deploy.repo, len(runs), perRepo)
			}
			numbers := map[int]bool{}
			for _, pr := range prs {
				if pr.repo == deploy.repo {
					numbers[pr.number] = true
				}
			}
			for _, run := range runs {
				if run.kind == delivery.DeliveryRunKindPRChecks && len(numbers) > 0 && !numbers[run.prNumber] {
					t.Fatalf("%s's pull-request run names #%d, no pull request of the repository", deploy.repo, run.prNumber)
				}
			}
			spans = append(spans, runSpan{repo: deploy.repo, first: deploy.firstRunID, runs: maxRuns})
		}
		if err := delivery.ValidateProjectSettings(fixture.sources(spec)); err != nil {
			t.Errorf("%s's settings are refused: %v", fixture.key, err)
		}
	}
	slices.SortFunc(spans, func(a, b runSpan) int { return cmp.Compare(a.first, b.first) })
	for index, span := range spans {
		if index > 0 && spans[index-1].first+spans[index-1].runs > span.first {
			t.Errorf("the run ids of %s meet %s's", spans[index-1].repo, span.repo)
		}
		if span.first+span.runs > FailureDayFirstRunID {
			t.Errorf("the run ids of %s reach the failure day's, %d", span.repo, FailureDayFirstRunID)
		}
	}
}

// TestK14LoadWritesTheTwoProjectsWithTheirSourcesAndFailures: a k14 load at a small scale, into a
// database of its own the server's migrations made, writes PAIR with two github_workflow sources,
// one reading its default repository and one naming acme/pair-service, and its merged reverts, and
// MIXED with a github_workflow source reading its default repository and an events source, and its
// posted deploys and the posted failures that blame them; each deploy repository's runs under its
// own ids; each project's failures counted from a day; and INFRA's runs as a load without them holds
// them.
func TestK14LoadWritesTheTwoProjectsWithTheirSourcesAndFailures(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(24 * time.Hour)
	spec := Spec{Now: now, Seed: 2916, Scale: 0.02, K14: true, BuiltBy: "k14-test"}
	counts, err := load(ctx, database, spec, t.Logf, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range k14CountedTables {
		if counts[table] == 0 {
			t.Errorf("the k14 load holds no row of %s", table)
		}
	}
	pool := database.Pool
	since := now.AddDate(0, 0, -failureDays)
	for _, want := range []struct {
		project, failure string
		sources          []string
		rules            []string
	}{
		{PairProject, delivery.FailureSourceReverts, []string{"github_workflow:", "github_workflow:" + PairRepos[1]}, []string{PairRepos[0], PairRepos[1]}},
		{MixedProject, delivery.FailureSourceEvents, []string{"github_workflow:", "events:"}, []string{MixedRepos[0]}},
	} {
		settings, err := delivery.GetProjectSettings(ctx, pool, want.project)
		if err != nil {
			t.Fatal(err)
		}
		var sources, rules []string
		for _, source := range settings.Sources {
			sources = append(sources, source.Kind+":"+shownRepo(source.Repo))
		}
		for _, rule := range settings.Rules() {
			rules = append(rules, rule.DeployRepo)
		}
		slices.Sort(rules)
		if !settings.Delivery || !slices.Equal(sources, want.sources) || !slices.Equal(rules, want.rules) ||
			!slices.Equal(settings.FailureSources, []string{want.failure}) || settings.FailuresMeasuredSince == nil || !settings.FailuresMeasuredSince.Equal(since) {
			t.Errorf("%s's settings are delivery %t, sources %q reading %q, failures %q since %v; want sources %q reading %q, failures %q since %s",
				want.project, settings.Delivery, sources, rules, settings.FailureSources, settings.FailuresMeasuredSince, want.sources, want.rules, want.failure, since.Format(time.DateOnly))
		}
	}
	deploys, checks := spec.dailyRuns()
	perRepo := int64(runDays * (deploys + checks))
	for _, deploy := range append([]deployRepo{infraFixture.deploys[0]}, slices.Concat(k14Fixtures[0].deploys, k14Fixtures[1].deploys)...) {
		var runs, first, last int64
		if err := pool.QueryRow(ctx, `select count(*), min(run_id), max(run_id) from delivery_runs where repo = $1`, deploy.repo).Scan(&runs, &first, &last); err != nil {
			t.Fatal(err)
		}
		if runs != perRepo || first != deploy.firstRunID || last != deploy.firstRunID+perRepo-1 {
			t.Errorf("%s holds %d runs, ids %d to %d; want %d from %d", deploy.repo, runs, first, last, perRepo, deploy.firstRunID)
		}
	}
	var issueRows, prs, foreign int64
	if err := pool.QueryRow(ctx, `
		select (select count(*) from issues where project_key = $1),
		       (select count(*) from delivery_pull_requests where repo = any($2)),
		       (select count(*) from delivery_pull_requests where repo = any($2) and issue_key is not null and issue_key not like $1 || '-%')
	`, PairProject, PairRepos[:]).Scan(&issueRows, &prs, &foreign); err != nil {
		t.Fatal(err)
	}
	if issueRows != int64(spec.scaled(issues, 12)) || prs != int64(spec.scaled(pullRequests, 3)) || foreign != 0 {
		t.Errorf("PAIR holds %d issues and %d pull requests, %d of them naming another project's issue; want %d, %d and none",
			issueRows, prs, foreign, spec.scaled(issues, 12), spec.scaled(pullRequests, 3))
	}
	var posted, failures, unblamed, restoredWithout int64
	if err := pool.QueryRow(ctx, `
		select count(*) filter (where kind = 'deploy' and source = 'api'),
		       count(*) filter (where kind = 'failure' and signal = 'api'),
		       count(*) filter (where kind = 'failure' and not exists (
		         select 1 from production_events d where d.id = production_events.deploy_id and d.project_key = $1 and d.kind = 'deploy')),
		       count(*) filter (where restored_at is not null and restored_by is null)
		from production_events where project_key = $1
	`, MixedProject).Scan(&posted, &failures, &unblamed, &restoredWithout); err != nil {
		t.Fatal(err)
	}
	if posted == 0 || failures == 0 || unblamed != 0 || restoredWithout != 0 {
		t.Errorf("MIXED holds %d posted deploys and %d posted failures, %d blaming no deploy of its own and %d restored by no one; want both, every failure blaming one of its deploys",
			posted, failures, unblamed, restoredWithout)
	}
	var reverts, unmatched int64
	if err := pool.QueryRow(ctx, `
		select count(*), count(*) filter (where not exists (
		  select 1 from delivery_pull_requests p where p.repo = r.reverts_repo and p.number = r.reverts_number
		    and p.merge_commit_sha = r.reverts_merge_commit_sha and p.merged_at < r.merged_at and p.repo = r.repo))
		from delivery_reverts r where r.repo = any($1)
	`, PairRepos[:2]).Scan(&reverts, &unmatched); err != nil {
		t.Fatal(err)
	}
	if reverts == 0 || unmatched != 0 {
		t.Errorf("PAIR's deploy repositories hold %d reverts, %d of no pull request merged before them in the repository; want some, each of one", reverts, unmatched)
	}
}

// shownRepo is a source's repository, "" for none.
func shownRepo(repo *string) string {
	if repo == nil {
		return ""
	}
	return *repo
}
