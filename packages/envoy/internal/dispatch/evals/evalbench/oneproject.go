package evalbench

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The one-project fixture (Spec.OneProject) is the one the Evals tab's checks k22 and k24 read
// beside the base head 82380570f8 (dispatch://INFRA-2918, T27). The base's delivery timeline and
// measures answer the whole deployment, and this head's project routes answer one project, so the
// two agree only where everything the base reads counts for one project: INFRA alone, one deploy
// repository, and every pull request, deploy and run in the base's population (an author of the
// population rule, a repository it does not exclude, every run in the deploy repository) and
// counting for INFRA by root R27 (a pull request through its issue, or naming none through its
// repository as INFRA's default; a run through the pull requests it ships, or shipping none
// through the deploy source that reads its repository). The pull requests are in two
// repositories, the deploy repository and one other, so the Repository facet has a value whose
// number differs from the unfiltered one.

// OneProjectRepos are the two repositories the one-project fixture's pull requests are in: the
// deploy repository, INFRA's default, and one other.
var OneProjectRepos = Repos[:2]

// oneProjectRepo is the repository the one-project fixture writes a pull request the full
// fixture draws into repo to, naming issue ("" for none): the third of Repos folds into the
// second, and one outside the deploy repository that names no issue moves into the deploy
// repository, since it would count for no project there (root R27: a pull request with no issue
// counts for the project whose default repository is its repository).
func oneProjectRepo(repo, issue string) string {
	if repo == Repos[0] || issue == "" {
		return Repos[0]
	}
	return Repos[1]
}

// oneProjectWindows are the windows CheckOneProject holds to merged pull requests of both
// repositories: the tab's presets that k22 and k24 read, each ending at the history's end.
var oneProjectWindows = []int{7, 28, 84}

// OneProjectChecks is what CheckOneProject read of a loaded one-project fixture: each count is
// one of its rules, and a rule holds where its count of rows breaking it is 0 (Failed).
type OneProjectChecks struct {
	// Projects is how many projects the database holds, and Infra whether INFRA is one of them
	// with the deploy repository as its default.
	Projects int64
	Infra    bool
	// PullRequests is how many pull requests it holds; OutsideRepos how many are in neither of
	// OneProjectRepos, OutsideAuthors how many an author outside the population rule wrote, and
	// NotCounting how many count for no project or for another (an issue of no INFRA issue, or no
	// issue outside INFRA's default repository).
	PullRequests, OutsideRepos, OutsideAuthors, NotCounting int64
	// Runs is how many runs it holds; OutsideDeployRepo how many are in another repository,
	// DeploysOffMain how many deploy runs are not on main, which the base reads deploys from, and
	// ChecksOfNoPullRequest how many pull-request runs name no pull request of the fixture.
	Runs, OutsideDeployRepo, DeploysOffMain, ChecksOfNoPullRequest int64
	// Merged is, for each of oneProjectWindows, how many pull requests of each of OneProjectRepos
	// merged in it.
	Merged map[int]map[string]int64
}

// CheckOneProject reads the one-project fixture's rules off the database a load wrote, its
// history ending at now. It reads only the tables both heads hold.
func CheckOneProject(ctx context.Context, pool *store.Pool, now time.Time) (OneProjectChecks, error) {
	var checks OneProjectChecks
	if err := pool.QueryRow(ctx, `
		select (select count(*) from projects),
		       exists (select 1 from projects where key = $1 and default_repo = $2),
		       (select count(*) from delivery_pull_requests),
		       (select count(*) from delivery_pull_requests where repo <> all($3::text[])),
		       (select count(*) from delivery_pull_requests where author <> all($4::text[])),
		       (select count(*) from delivery_pull_requests p
		         where case when p.issue_key is null then p.repo <> $2
		                    else not exists (select 1 from issues i where i.key = p.issue_key and i.project_key = $1) end),
		       (select count(*) from delivery_runs),
		       (select count(*) from delivery_runs where repo <> $2),
		       (select count(*) from delivery_runs where kind = $5 and head_branch <> 'main'),
		       (select count(*) from delivery_runs r
		         where r.kind = $6 and not exists (
		           select 1 from delivery_pull_requests p where p.repo = r.repo and p.number = r.pr_number))
	`, Project, Repos[0], OneProjectRepos, authors, string(delivery.DeliveryRunKindDeploy),
		string(delivery.DeliveryRunKindPRChecks)).Scan(&checks.Projects, &checks.Infra, &checks.PullRequests,
		&checks.OutsideRepos, &checks.OutsideAuthors, &checks.NotCounting, &checks.Runs, &checks.OutsideDeployRepo,
		&checks.DeploysOffMain, &checks.ChecksOfNoPullRequest); err != nil {
		return OneProjectChecks{}, fmt.Errorf("read the one-project fixture's rules: %w", err)
	}
	checks.Merged = map[int]map[string]int64{}
	end := now.UTC().Truncate(24 * time.Hour)
	for _, days := range oneProjectWindows {
		merged := map[string]int64{}
		for _, repo := range OneProjectRepos {
			var count int64
			if err := pool.QueryRow(ctx, `
				select count(*) from delivery_pull_requests where repo = $1 and merged_at >= $2 and merged_at < $3
			`, repo, end.AddDate(0, 0, -days), end).Scan(&count); err != nil {
				return OneProjectChecks{}, fmt.Errorf("count the pull requests of %s merged in the %d days before %s: %w", repo, days, end.Format(time.DateOnly), err)
			}
			merged[repo] = count
		}
		checks.Merged[days] = merged
	}
	return checks, nil
}

// Failed names each rule the fixture breaks, none where it holds them all.
func (c OneProjectChecks) Failed() []string {
	var failed []string
	if c.Projects != 1 || !c.Infra {
		failed = append(failed, fmt.Sprintf("the database holds %d projects, not %s alone with %s as its default repository", c.Projects, Project, Repos[0]))
	}
	broken := []struct {
		count int64
		rule  string
	}{
		{c.OutsideRepos, "pull requests outside " + fmt.Sprint(OneProjectRepos)},
		{c.OutsideAuthors, "pull requests by an author outside the population rule"},
		{c.NotCounting, "pull requests that count for no project or for another"},
		{c.OutsideDeployRepo, "runs outside the deploy repository " + Repos[0]},
		{c.DeploysOffMain, "deploy runs not on main"},
		{c.ChecksOfNoPullRequest, "pull-request runs of no pull request of the fixture"},
	}
	for _, one := range broken {
		if one.count > 0 {
			failed = append(failed, fmt.Sprintf("%d %s", one.count, one.rule))
		}
	}
	if c.PullRequests == 0 || c.Runs == 0 {
		failed = append(failed, fmt.Sprintf("the fixture holds %d pull requests and %d runs, so it examined nothing", c.PullRequests, c.Runs))
	}
	for _, days := range oneProjectWindows {
		for _, repo := range OneProjectRepos {
			if c.Merged[days][repo] == 0 {
				failed = append(failed, fmt.Sprintf("no pull request of %s merged in the %d-day window, so the Repository facet's %s reads the unfiltered number", repo, days, repo))
			}
		}
	}
	return failed
}

// Lines is the checks as the lines the generator prints, one a rule.
func (c OneProjectChecks) Lines() []string {
	lines := []string{
		fmt.Sprintf("projects=%d infra_with_default=%t", c.Projects, c.Infra),
		fmt.Sprintf("pull_requests=%d outside_repos=%d outside_authors=%d not_counting_for_%s=%d", c.PullRequests, c.OutsideRepos, c.OutsideAuthors, Project, c.NotCounting),
		fmt.Sprintf("runs=%d outside_deploy_repo=%d deploys_off_main=%d checks_of_no_pull_request=%d", c.Runs, c.OutsideDeployRepo, c.DeploysOffMain, c.ChecksOfNoPullRequest),
	}
	days := make([]int, 0, len(c.Merged))
	for one := range c.Merged {
		days = append(days, one)
	}
	slices.Sort(days)
	for _, one := range days {
		line := fmt.Sprintf("window_days=%d", one)
		for _, repo := range OneProjectRepos {
			line += fmt.Sprintf(" merged[%s]=%d", repo, c.Merged[one][repo])
		}
		lines = append(lines, line)
	}
	return lines
}
