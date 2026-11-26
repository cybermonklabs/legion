package evalfixture

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The Attribution mode's rows (dispatch://INFRA-2916, D1, R15 and A10, k15's case of verification
// version 26): one author of the population, the deploy workflow's production job, a PR-checks
// job, and the run ids, which rise with creation as GitHub's do and stand well clear of
// deliverytest's.
const (
	author        = "dev-attribution"
	productionJob = "production-release / apply"
	checkJob      = "test / unit"
	firstRunID    = 8_000_000_001
)

// Attributed is one row of the Attribution mode and the projects R15 counts it for (r15), in key
// order. ViaRepository is, for each of those projects, whether the row reached it through the
// project's default repository rather than through an issue, which every day row that counts it
// carries in via_repository.
type Attributed struct {
	Row           string   `json:"row"`
	Day           string   `json:"day"`
	CountsFor     []string `json:"counts_for"`
	ViaRepository []bool   `json:"via_repository"`
}

// AttributionResult is what the Attribution mode wrote: its three projects and the default
// repository of each ("" for none), the deploy repository, the repository that is no project's
// default, every row with the projects it counts for, and the project with nothing, which reads
// "no data".
type AttributionResult struct {
	Projects   []string          `json:"projects"`
	Defaults   map[string]string `json:"default_repositories"`
	DeployRepo string            `json:"deploy_repository"`
	NoDefault  string            `json:"repository_of_no_default"`
	Rows       []Attributed      `json:"rows"`
	Nothing    string            `json:"project_with_nothing"`
}

// r15 is R15's rule (spec version 30), the oracle the Attribution mode answers each row's
// projects by, written from its words and not from the evals' attribution: "A pull request counts
// for its issue's project. A pull request with no issue counts for each project whose default
// repository (default_repo) is its repository, and for no project when its repository is no
// project's default. A deploy counts for the projects of the pull requests it ships. A deploy that
// ships no pull request counts for each project whose default repository is the deploy's
// repository. A CI job counts where its pull request counts, and a CI job with no pull request
// counts for each project whose default repository is its repository." defaults is each project's
// default repository, and issues each issue's project.
type r15 struct {
	defaults map[string]string
	issues   map[string]string
}

// counts is the projects a row counts for, each with whether it reached it through a default
// repository and not through an issue.
type counts map[string]bool

// byDefault is each project whose default repository is repo, each reached through it.
func (r r15) byDefault(repo string) counts {
	reached := counts{}
	for project, held := range r.defaults {
		if held != "" && strings.EqualFold(held, repo) {
			reached[project] = true
		}
	}
	return reached
}

// pullRequest is what a pull request of repo naming issue (nil for none) counts for.
func (r r15) pullRequest(repo string, issue *string) counts {
	if issue != nil {
		if project, held := r.issues[*issue]; held {
			return counts{project: false}
		}
	}
	return r.byDefault(repo)
}

// deploy is what a deploy run of repo that ships the pull requests shipped counts for: what they
// count for, each project once, through an issue where any of them reached it through one; and
// what the repository's default counts for where it ships none.
func (r r15) deploy(repo string, shipped []counts) counts {
	if len(shipped) == 0 {
		return r.byDefault(repo)
	}
	reached := counts{}
	for _, one := range shipped {
		for project, via := range one {
			if was, held := reached[project]; held {
				via = via && was
			}
			reached[project] = via
		}
	}
	return reached
}

// ciJob is what a CI job of repo counts for: where its pull request counts (pull, nil for a job
// of no pull request), and the repository's default otherwise.
func (r r15) ciJob(repo string, pull counts) counts {
	if pull == nil {
		return r.byDefault(repo)
	}
	return pull
}

// attributed is row on day, counting for reached.
func attributed(row string, day time.Time, reached counts) Attributed {
	projects := slices.Sorted(maps.Keys(reached))
	via := make([]bool, len(projects))
	for i, project := range projects {
		via[i] = reached[project]
	}
	return Attributed{Row: row, Day: day.Format(time.DateOnly), CountsFor: projects, ViaRepository: via}
}

// WriteAttribution is the Attribution mode under project, k15's case: the projects project (the
// first project), project+"B" (the other project) and project+"C" (the one with nothing); the
// first project's default repository, which the delivery settings name as the deploy repository
// with its production job, the other project's, which holds no row, and a third repository that
// is no project's default; and, on the day before now, a pull request of the deploy repository
// whose issue is the other project's (an issue that names the deploy repository as its own, so
// the attribution sweep holds it there), one of the deploy repository that names no issue and one
// of the third repository that names none, a deploy run that ships the deploy repository's two and
// a later one that ships none, and two PR-checks runs of the deploy repository with one job each,
// one of the pull request whose issue is the other project's and one of no pull request. Each
// row's projects are r15's. The delivery settings are one row a database holds, so this mode
// replaces the Delivery mode's settings and is not combined with it.
func WriteAttribution(ctx context.Context, pool *store.Pool, project string, now time.Time) (AttributionResult, error) {
	other, nothing := project+"B", project+"C"
	lower := strings.ToLower(project)
	deployRepo, otherRepo, noDefault := "acme/"+lower+"-platform", "acme/"+lower+"-web", "acme/"+lower+"-tools"
	result := AttributionResult{
		Projects: []string{project, other, nothing}, DeployRepo: deployRepo, NoDefault: noDefault, Nothing: nothing,
		Defaults: map[string]string{project: deployRepo, other: otherRepo, nothing: ""},
	}
	exec := func(what, statement string, args ...any) error {
		if _, err := pool.Exec(ctx, statement, args...); err != nil {
			return fmt.Errorf("write the %s of k15's case: %w", what, err)
		}
		return nil
	}
	// Each project names alice as its default assignee, the column every project fills (store
	// migration 20261111210103), as the Legion mode's history does; no attribution reads it. A
	// project the database holds already takes the case's default repository too.
	if err := exec("projects", `
		insert into projects (key, name, default_assignee, next_number, default_repo)
		select key, key, 'alice', 1, nullif(repo, '') from unnest($1::text[], $2::text[]) as seeded(key, repo)
		on conflict (key) do update set default_repo = excluded.default_repo`,
		result.Projects, []string{deployRepo, otherRepo, ""}); err != nil {
		return AttributionResult{}, err
	}
	// The one delivery_settings row is replaced only once the projects are written, so a case
	// whose projects the database refuses leaves the settings an earlier load wrote.
	if _, err := delivery.PutSettings(ctx, pool, deliverySettingsOf(deployRepo), actor); err != nil {
		return AttributionResult{}, fmt.Errorf("write the delivery settings of k15's case: %w", err)
	}
	day := measures.Midnight(now).AddDate(0, 0, -1)
	at := func(minutes int) time.Time { return day.Add(10*time.Hour + time.Duration(minutes)*time.Minute) }
	offset, err := issueOffset(ctx, pool, other)
	if err != nil {
		return AttributionResult{}, err
	}
	otherIssue := fmt.Sprintf("%s-%d", other, offset+1)
	if err := exec("issue", `
		insert into issues (key, project_key, number, title, status, rank, repo, created_by, created_at, closed_at)
		values ($1, $2, $3, 'An issue the case attributes by', 'done', $4, $5, $6::jsonb, $7, $8)`,
		otherIssue, other, offset+1, fmt.Sprintf("%08d", offset+1), deployRepo,
		fmt.Sprintf(`{"kind":%q,"id":%q}`, actor.Kind, actor.ID), at(-120), at(60)); err != nil {
		return AttributionResult{}, err
	}
	rule := r15{defaults: result.Defaults, issues: map[string]string{otherIssue: other}}

	prs := []struct {
		repo           string
		number         int
		issue          *string
		opened, merged time.Time
	}{
		{deployRepo, 1, &otherIssue, at(0), at(30)},
		{deployRepo, 2, nil, at(5), at(40)},
		{noDefault, 1, nil, at(10), at(45)},
	}
	pulls := map[string]counts{}
	for _, pr := range prs {
		title := fmt.Sprintf("Change %d", pr.number)
		titleKeys := []string{}
		if pr.issue != nil {
			title, titleKeys = *pr.issue+": "+title, []string{*pr.issue}
		}
		written := delivery.DeliveryPullRequest{
			Repo: pr.repo, Number: pr.number, Title: title, URL: fmt.Sprintf("https://github.com/%s/pull/%d", pr.repo, pr.number),
			Author: author, CreatedAt: new(pr.opened), MergedAt: new(pr.merged), FirstCommitAt: new(pr.opened.Add(-time.Hour)),
			MergeCommitSHA: new(fmt.Sprintf("%040x", len(pulls)+1)), Additions: new(10), Deletions: new(2), IssueKey: pr.issue,
			Attribution: &delivery.AttributionInputs{TitleKeys: titleKeys, CitedIssues: []string{}, BranchKeys: []string{}, CommitKeys: []string{}},
		}
		if err := delivery.UpsertPullRequest(ctx, pool, written); err != nil {
			return AttributionResult{}, fmt.Errorf("write the pull request %s#%d of k15's case: %w", pr.repo, pr.number, err)
		}
		named := "no issue"
		if pr.issue != nil {
			named = "issue " + *pr.issue
		}
		reached := rule.pullRequest(pr.repo, pr.issue)
		pulls[fmt.Sprintf("%s#%d", pr.repo, pr.number)] = reached
		result.Rows = append(result.Rows, attributed(
			fmt.Sprintf("pull request %s#%d (%s, merged %s)", pr.repo, pr.number, named, pr.merged.Format(time.RFC3339)), pr.merged, reached))
	}

	// Two PR-checks runs of the deploy repository, the one repository whose runs Dispatch reads,
	// with one job each: one of #1, whose issue is the other project's, and one GitHub names no
	// pull request of (a pull_request run whose pull request it does not report, as it does not
	// for a fork's). They are created before either deploy run, so they take the first run ids.
	checks := []struct {
		number  *int
		started time.Time
	}{
		{new(1), at(10)},
		{nil, at(15)},
	}

	// Two deploy runs on main that reach production: the first, created after every merge, ships
	// the deploy repository's two pull requests (containment ships the pull requests of its own
	// repository alone); the second, created after it with nothing merged between, ships none.
	deploys := []struct {
		created time.Time
		row     string
		ships   []string
	}{
		{at(60), "ships #1 and #2", []string{deployRepo + "#1", deployRepo + "#2"}},
		{at(180), "ships none", nil},
	}
	for index, deploy := range deploys {
		runID := int64(firstRunID + len(checks) + index)
		started := deploy.created.Add(30 * time.Second)
		completed := started.Add(10 * time.Minute)
		run := delivery.DeliveryRun{
			Repo: deployRepo, RunID: runID, Kind: delivery.DeliveryRunKindDeploy, HeadSHA: fmt.Sprintf("%040x", 100+index),
			HeadCommitAt: deploy.created.Add(-time.Minute), StartedAt: started, CreatedAt: new(deploy.created),
			CompletedAt: new(completed), Conclusion: new(delivery.DeliveryRunConclusionSuccess),
			URL:        fmt.Sprintf("https://github.com/%s/actions/runs/%d", deployRepo, runID),
			HeadBranch: new("main"), Event: new("push"),
		}
		job := delivery.DeliveryRunJob{
			Name: productionJob, StartedAt: new(started.Add(time.Minute)), CompletedAt: new(completed.Add(-time.Minute)),
			Conclusion: new(delivery.DeliveryJobConclusionSuccess),
		}
		if err := writeRun(ctx, pool, run, job); err != nil {
			return AttributionResult{}, err
		}
		var shipped []counts
		for _, pull := range deploy.ships {
			shipped = append(shipped, pulls[pull])
		}
		result.Rows = append(result.Rows, attributed(
			fmt.Sprintf("deploy run %s/%d (%s, started %s)", deployRepo, runID, deploy.row, started.Format(time.RFC3339)), started,
			rule.deploy(deployRepo, shipped)))
	}

	for index, check := range checks {
		runID := int64(firstRunID + index)
		completed := check.started.Add(8 * time.Minute)
		branch, of, pull := "feature/fork", "no pull request", counts(nil)
		if check.number != nil {
			branch, of = fmt.Sprintf("feature/%d", *check.number), fmt.Sprintf("#%d", *check.number)
			pull = pulls[fmt.Sprintf("%s#%d", deployRepo, *check.number)]
		}
		run := delivery.DeliveryRun{
			Repo: deployRepo, RunID: runID, Kind: delivery.DeliveryRunKindPRChecks, PRNumber: check.number,
			HeadSHA: fmt.Sprintf("%040x", 200+index), HeadCommitAt: check.started.Add(-time.Minute), StartedAt: check.started,
			CreatedAt: new(check.started.Add(-10 * time.Second)), CompletedAt: new(completed),
			Conclusion: new(delivery.DeliveryRunConclusionSuccess),
			URL:        fmt.Sprintf("https://github.com/%s/actions/runs/%d", deployRepo, runID),
			HeadBranch: new(branch), Event: new("pull_request"),
		}
		job := delivery.DeliveryRunJob{
			Name: checkJob, StartedAt: new(check.started.Add(5 * time.Second)), CompletedAt: new(completed.Add(-5 * time.Second)),
			Conclusion: new(delivery.DeliveryJobConclusionSuccess),
		}
		if err := writeRun(ctx, pool, run, job); err != nil {
			return AttributionResult{}, err
		}
		result.Rows = append(result.Rows, attributed(
			fmt.Sprintf("CI job %q of PR-checks run %s/%d of %s (started %s)", checkJob, deployRepo, runID, of, check.started.Format(time.RFC3339)),
			check.started, rule.ciJob(deployRepo, pull)))
	}
	return result, nil
}

// writeRun stores one run and its one job, listed after the run ended.
func writeRun(ctx context.Context, pool *store.Pool, run delivery.DeliveryRun, job delivery.DeliveryRunJob) error {
	if err := delivery.UpsertRun(ctx, pool, run); err != nil {
		return fmt.Errorf("write the run %s/%d of k15's case: %w", run.Repo, run.RunID, err)
	}
	job.Repo, job.RunID = run.Repo, run.RunID
	if err := delivery.UpsertRunJobs(ctx, pool, run.Repo, run.RunID, []delivery.DeliveryRunJob{job}, true); err != nil {
		return fmt.Errorf("write the job of the run %s/%d of k15's case: %w", run.Repo, run.RunID, err)
	}
	return nil
}
