package evalfixture

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The Attribution mode's rows (dispatch://INFRA-2916, D1 and R15, k15's case): one author of the
// population, the deploy workflow's production job, a PR-checks job, and the run ids, which rise
// with creation as GitHub's do and stand well clear of deliverytest's.
const (
	author        = "dev-attribution"
	productionJob = "production-release / apply"
	checkJob      = "test / unit"
	firstRunID    = 8_000_000_001
)

// Attributed is one row of the Attribution mode and the projects D1 counts it for: "a pull
// request counts for its issue's project, else its repository's project (repo_projects); a deploy
// counts for the projects of the pull requests it ships, and a deploy that ships none counts for
// the deploy repository's project; a CI job counts for its repository's project". ViaRepository
// is whether it reached each of those projects through a repository rather than through an issue,
// which every day row that counts it carries in via_repository.
type Attributed struct {
	Row           string   `json:"row"`
	Day           string   `json:"day"`
	CountsFor     []string `json:"counts_for"`
	ViaRepository []bool   `json:"via_repository"`
}

// AttributionResult is what the Attribution mode wrote: its three projects, the repository each
// of the first two is mapped to, every row with the projects it counts for, and the project with
// nothing, which reads "no data".
type AttributionResult struct {
	Projects   []string          `json:"projects"`
	Repos      map[string]string `json:"repos"`
	DeployRepo string            `json:"deploy_repository"`
	Rows       []Attributed      `json:"rows"`
	Nothing    string            `json:"project_with_nothing"`
}

// WriteAttribution is the Attribution mode, under project: the projects project, project+"B" (the
// other project) and project+"C" (the one with nothing); the deploy repository, mapped to project
// and named by the delivery settings with its production job, and a second repository mapped to
// the other project; and, on the day before now, a pull request of the deploy repository whose
// issue is the other project's, one that names no issue, a deploy run that ships both, a later
// one that ships neither, and a PR-checks run with one job. The delivery settings are one row a
// database holds, so this mode replaces the Delivery mode's settings and is not combined with it.
func WriteAttribution(ctx context.Context, pool *store.Pool, project string, now time.Time) (AttributionResult, error) {
	other, nothing := project+"B", project+"C"
	lower := strings.ToLower(project)
	deployRepo := "acme/" + lower + "-platform"
	otherRepo := "acme/" + lower + "b-service"
	result := AttributionResult{
		Projects: []string{project, other, nothing}, DeployRepo: deployRepo, Nothing: nothing,
		Repos: map[string]string{deployRepo: project, otherRepo: other},
	}
	exec := func(what, statement string, args ...any) error {
		if _, err := pool.Exec(ctx, statement, args...); err != nil {
			return fmt.Errorf("write the %s of k15's case: %w", what, err)
		}
		return nil
	}
	if _, err := delivery.PutSettings(ctx, pool, deliverySettingsOf(deployRepo), actor); err != nil {
		return AttributionResult{}, fmt.Errorf("write the delivery settings of k15's case: %w", err)
	}
	if err := exec("projects", `
		insert into projects (key, name, next_number)
		select key, key, 1 from unnest($1::text[]) as seeded(key)
		on conflict (key) do nothing`, result.Projects); err != nil {
		return AttributionResult{}, err
	}
	if err := exec("repositories", `
		insert into repo_projects (repo, project, created_by, is_default)
		values ($1, $2, $5::jsonb, true), ($3, $4, $5::jsonb, true)`,
		deployRepo, project, otherRepo, other, fmt.Sprintf(`{"kind":%q,"id":%q}`, actor.Kind, actor.ID)); err != nil {
		return AttributionResult{}, err
	}
	offset, err := issueOffset(ctx, pool, other)
	if err != nil {
		return AttributionResult{}, err
	}
	day := dayOf(now).AddDate(0, 0, -1)
	at := func(minutes int) time.Time { return day.Add(10*time.Hour + time.Duration(minutes)*time.Minute) }
	issue := fmt.Sprintf("%s-%d", other, offset+1)
	if err := exec("issue", `
		insert into issues (key, project_key, number, title, status, rank, created_by, created_at, closed_at)
		values ($1, $2, $3, 'An issue of the other project', 'done', $4, $5::jsonb, $6, $7)`,
		issue, other, offset+1, fmt.Sprintf("%08d", offset+1), fmt.Sprintf(`{"kind":%q,"id":%q}`, actor.Kind, actor.ID),
		at(-120), at(60)); err != nil {
		return AttributionResult{}, err
	}

	prs := []struct {
		number          int
		issue           *string
		opened, merged  time.Time
		countsFor       string
		throughTheIssue bool
	}{
		{1, &issue, at(0), at(30), other, true},
		{2, nil, at(5), at(40), project, false},
	}
	for _, pr := range prs {
		title := fmt.Sprintf("Change %d", pr.number)
		titleKeys := []string{}
		if pr.issue != nil {
			title, titleKeys = *pr.issue+": "+title, []string{*pr.issue}
		}
		written := delivery.DeliveryPullRequest{
			Repo: deployRepo, Number: pr.number, Title: title, URL: fmt.Sprintf("https://github.com/%s/pull/%d", deployRepo, pr.number),
			Author: author, CreatedAt: new(pr.opened), MergedAt: new(pr.merged), FirstCommitAt: new(pr.opened.Add(-time.Hour)),
			MergeCommitSHA: new(fmt.Sprintf("%040x", pr.number)), Additions: new(10), Deletions: new(2), IssueKey: pr.issue,
			Attribution: &delivery.AttributionInputs{TitleKeys: titleKeys, CitedIssues: []string{}, BranchKeys: []string{}, CommitKeys: []string{}},
		}
		if err := delivery.UpsertPullRequest(ctx, pool, written); err != nil {
			return AttributionResult{}, fmt.Errorf("write the pull request %s#%d of k15's case: %w", deployRepo, pr.number, err)
		}
		named := "no issue"
		if pr.issue != nil {
			named = "issue " + *pr.issue
		}
		result.Rows = append(result.Rows, Attributed{
			Row:       fmt.Sprintf("pull request %s#%d (%s, merged %s)", deployRepo, pr.number, named, pr.merged.Format(time.RFC3339)),
			Day:       pr.merged.Format(time.DateOnly),
			CountsFor: []string{pr.countsFor}, ViaRepository: []bool{!pr.throughTheIssue},
		})
	}

	// Two deploy runs on main that reach production: the first, created after both merges, ships
	// both pull requests; the second, created after it with nothing merged between, ships none.
	deploys := []struct {
		created time.Time
		row     string
		counts  []string
		via     []bool
	}{
		{at(60), "ships #1 and #2", []string{other, project}, []bool{false, true}},
		{at(180), "ships none", []string{project}, []bool{true}},
	}
	for index, deploy := range deploys {
		runID := int64(firstRunID + index)
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
		result.Rows = append(result.Rows, Attributed{
			Row:       fmt.Sprintf("deploy run %s/%d (%s, started %s)", deployRepo, runID, deploy.row, started.Format(time.RFC3339)),
			Day:       started.Format(time.DateOnly),
			CountsFor: deploy.counts, ViaRepository: deploy.via,
		})
	}

	// A PR-checks run of the second pull request with one job: a CI job of the deploy
	// repository, the one repository whose runs Dispatch reads.
	runID := int64(firstRunID + len(deploys))
	started := at(10)
	completed := started.Add(8 * time.Minute)
	check := delivery.DeliveryRun{
		Repo: deployRepo, RunID: runID, Kind: delivery.DeliveryRunKindPRChecks, PRNumber: new(2),
		HeadSHA: fmt.Sprintf("%040x", 200), HeadCommitAt: started.Add(-time.Minute), StartedAt: started,
		CreatedAt: new(started.Add(-10 * time.Second)), CompletedAt: new(completed),
		Conclusion: new(delivery.DeliveryRunConclusionSuccess),
		URL:        fmt.Sprintf("https://github.com/%s/actions/runs/%d", deployRepo, runID),
		HeadBranch: new("feature/2"), Event: new("pull_request"),
	}
	job := delivery.DeliveryRunJob{
		Name: checkJob, StartedAt: new(started.Add(5 * time.Second)), CompletedAt: new(completed.Add(-5 * time.Second)),
		Conclusion: new(delivery.DeliveryJobConclusionSuccess),
	}
	if err := writeRun(ctx, pool, check, job); err != nil {
		return AttributionResult{}, err
	}
	result.Rows = append(result.Rows, Attributed{
		Row:       fmt.Sprintf("CI job %q of PR-checks run %s/%d (started %s)", checkJob, deployRepo, runID, started.Format(time.RFC3339)),
		Day:       started.Format(time.DateOnly),
		CountsFor: []string{project}, ViaRepository: []bool{true},
	})
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
