package evalfixture

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/prodevents"
	"github.com/sjawhar/envoy/internal/dispatch/prodevents/signals"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The Failures mode (dispatch://INFRA-978): the record of deploys and failed changes the
// checks of INFRA-978 read (k2, k7 to k9, k11 and k12, and k13's project), written through the
// writers production uses: the project settings through delivery.PutProjectSettings, the pull
// requests, runs, jobs and the revert through the delivery store's writes, the check results and
// the issues as their routes store them, and every row of the record through the production
// signals' loop (signals.Run: the GitHub adapter's deploy rows, the revert and the incident), the
// record route's handler write (signals.RecordCheckFailure) and the record's store
// (prodevents.Write: the posted deploys, the posted failure and the person's flag). A second load
// with the same -now writes the same rows: every write is keyed, and a settings save that changes
// no field moves its updated_at alone.

// The Failures mode's names: the population's one author, the person who flags a deploy, the
// actor of the check results, the system that posts the second project's deploys and its failure,
// and the first run id, the ids rising with creation as GitHub's do and standing clear of the
// Attribution mode's and deliverytest's.
const (
	failuresAuthor   = "dev-failures"
	failuresFirstRun = 9_000_000_001
	// signalsWait is how long WriteFailures waits for the production signals' loop to write the
	// rows its inputs make, counted from the last moment the loop's event horizon stood held back.
	signalsWait = 2 * time.Minute
)

var (
	failuresFlagger = model.Actor{Kind: "user", ID: "alice"}
	failuresTester  = model.Actor{Kind: "session", ID: "evalfixture-tester"}
	failuresPoster  = model.Actor{Kind: "session", ID: "evalfixture-deployer"}
)

// failureRule is one github_workflow source's rule: its deploy workflow, its production job, the
// step that job must pass for a deploy (nil for none) and its pull-request checks workflow.
type failureRule struct {
	workflow, job, checks string
	step                  *string
}

// source is the rule as a source of a save, naming repo (nil: the project's default repository).
func (r failureRule) source(repo *string) delivery.SourceInput {
	return delivery.SourceInput{
		Kind: delivery.SourceKindGitHubWorkflow, Repo: repo, DeployWorkflowPath: new(r.workflow),
		ProductionJobNames: []string{r.job}, ProductionApplyStepName: r.step, PRChecksWorkflowPath: new(r.checks),
	}
}

// The three rules: the first project's default repository's and its release repository's, each
// with its own workflow and production job, and the second project's.
var (
	appRule     = failureRule{workflow: ".github/workflows/deploy.yml", job: "production / apply", checks: ".github/workflows/pr-checks.yml", step: new("Apply to production")}
	releaseRule = failureRule{workflow: ".github/workflows/release.yml", job: "release / production", checks: ".github/workflows/ci.yml"}
	secondRule  = failureRule{workflow: ".github/workflows/deploy.yml", job: "deploy / production", checks: ".github/workflows/pr-checks.yml", step: new("Apply")}
)

// FailureSource is one deploy source a project of the Failures mode holds: its kind, the
// repository it names (nil: it reads the project's default repository and moves with it), and the
// repository it reads ("" for an events source, which reads the project's posted deploys).
type FailureSource struct {
	Kind  string  `json:"kind"`
	Names *string `json:"repo"`
	Reads string  `json:"reads"`
}

// FailureDeploy is one deploy the Failures mode made and its row of the record: a deploy run of a
// source repository, whose row the GitHub adapter wrote (RunID its run), or a deploy posted to a
// project's route (RunID nil). CountsFor is the projects it counts for, in key order
// (failureOracle).
type FailureDeploy struct {
	Name         string    `json:"name"`
	ID           string    `json:"id"`
	ProjectKey   string    `json:"project_key"`
	SourceRef    string    `json:"source_ref"`
	Repo         string    `json:"repo"`
	RunID        *int64    `json:"run_id,omitempty"`
	FinishedAt   time.Time `json:"finished_at"`
	PullRequests []string  `json:"pull_requests"`
	CountsFor    []string  `json:"counts_for"`
}

// NoDeploy is a deploy-workflow run the Failures mode stored that makes no deploy row, and why.
type NoDeploy struct {
	Repo  string `json:"repo"`
	RunID int64  `json:"run_id"`
	Why   string `json:"why"`
}

// FailureSignal is the failure row one signal made, one of each of the five kinds: the deploy it
// blames and the deploy that restored it, each by its FailureDeploy.Name, and the projects that
// count it, in key order (failureOracle).
type FailureSignal struct {
	Signal     string     `json:"signal"`
	ID         string     `json:"id"`
	ProjectKey string     `json:"project_key"`
	SourceRef  string     `json:"source_ref"`
	State      string     `json:"state"`
	Deploy     string     `json:"deploy"`
	DetectedAt time.Time  `json:"detected_at"`
	RestoredAt *time.Time `json:"restored_at"`
	RestoredBy *string    `json:"restored_by_deploy"`
	CountsFor  []string   `json:"counts_for"`
}

// NoSignal is an input of the Failures mode that makes no failure row (k7's negatives): a passing
// after-merge check, a failing one at a commit no deploy holds, and an issue labelled bug that
// links a deployed pull request. Ref is the check result's id or the issue's key.
type NoSignal struct {
	What string `json:"what"`
	Ref  string `json:"ref"`
}

// FailureMeasures is one project's three failure measures over the Failures mode's window, as
// failureOracle states them. Deploys is the deploys counted for the project in the window: the
// total of change-fail-rate and of deployment-rework-rate, and deploy-frequency's sum. A rate or
// a median with nothing to fold is null.
type FailureMeasures struct {
	Deploys        int      `json:"deploys"`
	Failed         int      `json:"failed"`
	ChangeFailRate *float64 `json:"change_fail_rate"`
	Restored       int      `json:"restored"`
	RecoveryMedian *float64 `json:"recovery_median_minutes"`
	NotRestored    int      `json:"not_restored"`
	Restoring      int      `json:"restoring"`
	ReworkRate     *float64 `json:"rework_rate"`
}

// FailureExpects is the counts each check expects of the Failures mode's rows: k2's sources saved
// per project and the first project's deploys per repository (its third repository's at 0); k7's
// signals, the rows each makes and the rows the negatives make; k8's failures counted on arrival;
// k9's and k11's measures per project over the window; and k12's rows placed.
type FailureExpects struct {
	SourcesSaved        map[string]int             `json:"k2_sources_saved"`
	DeploysByRepository map[string]int             `json:"k2_deploys_by_repository"`
	Signals             int                        `json:"k7_signals"`
	RowsBySignal        map[string]int             `json:"k7_rows_by_signal"`
	NegativeRows        int                        `json:"k7_negative_rows"`
	CountedOnArrival    int                        `json:"k8_counted_on_arrival"`
	Measures            map[string]FailureMeasures `json:"k9_k11_measures"`
	RowsPlaced          int                        `json:"k12_rows_placed"`
}

// FailuresResult is what the Failures mode wrote: its two projects with the default repository,
// the deploy sources and the failure sources of each, the first project's second source repository
// (no project's default) and the third repository no source reads, the day the failures are
// measured from and the window the measures are stated over; each deploy and its row, each run
// that makes none, each failure and each input that makes none; k12's rows with the projects each
// counts for; the controller session the projects' Legion reads name, where one was asked for; and
// the counts each check expects.
type FailuresResult struct {
	Projects       []string                   `json:"projects"`
	Defaults       map[string]string          `json:"default_repositories"`
	Sources        map[string][]FailureSource `json:"sources"`
	FailureSources map[string][]string        `json:"failure_sources"`
	SecondRepo     string                     `json:"second_repository"`
	ThirdRepo      string                     `json:"third_repository"`
	MeasuredSince  string                     `json:"failures_measured_since"`
	Window         Window                     `json:"window"`
	Deploys        []FailureDeploy            `json:"deploys"`
	NoDeploys      []NoDeploy                 `json:"runs_without_deploy"`
	Failures       []FailureSignal            `json:"failures"`
	Negatives      []NoSignal                 `json:"negatives"`
	Rows           []Attributed               `json:"rows"`
	Controller     *string                    `json:"controller_session,omitempty"`
	Expects        FailureExpects             `json:"expects"`
}

// failureRun is one deploy-workflow run of the Failures mode: its name in the result, its
// repository and the rule its job is named by, when GitHub created it, whether its production job
// passed its apply step, the pull requests containment ships with it (owner/repo#n), and why it is
// no deploy ("" for a deploy).
type failureRun struct {
	name    string
	repo    string
	rule    failureRule
	created time.Time
	applied bool
	ships   []string
	why     string
}

// failureIssue is one issue of the Failures mode: its key, number, project, repository, title and
// labels, its one own link ("" for none), when it was created, and the check results recorded on
// it, each against a check of its Verification document.
type failureIssue struct {
	key     string
	number  int
	project string
	repo    string
	title   string
	labels  []string
	link    string
	created time.Time
	results []failureResult
}

// failureResult is one check result: its check block, its verdict, the commit it ran at and when.
type failureResult struct {
	block, verdict, commit string
	at                     time.Time
}

// id is the result's id, derived from its project and its block, so a second load names it again.
func (r failureResult) id(project string) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("evalfixture:"+project+":"+r.block)).String()
}

// commitOf is the 40-hex commit the Failures mode names what by under project, distinct per
// project, so two projects loaded into one database share no commit a deploy row holds.
func commitOf(project, what string) string {
	return fmt.Sprintf("%x", sha1.Sum([]byte(project+"/"+what)))
}

// WriteFailures is the Failures mode under project, the first project, and project+"B", the second:
//
//   - the first project's default repository acme/<project>-app, read by a github_workflow source
//     that names no repository, and acme/<project>-release, no project's default, read by a second
//     source that names it, each with its own workflow and production job; the second project's
//     default repository acme/<project>b-app, read by its one github_workflow source, beside an
//     events source; and acme/<project>-tools, which no source reads. The first project counts all
//     four failure sources and the second all but reverts, each from the window's first day;
//   - on that day, three pull requests of the first project's default repository (one whose issue
//     is the first project's, one whose issue is the second's, one with no issue) and one with no
//     issue of the third repository; a PR-checks run of the first with its job and one GitHub names
//     no pull request of; deploy runs of the default repository that ship those three (R1), whose
//     apply step fails (R2, no deploy) and that ship none (R3), one of the release repository (R4)
//     and one of the third repository (R5, no deploy);
//   - on the next day, a pull request of the second project's repository and its deploy (R6), a
//     merged revert of the first pull request and the default repository's deploy that ships it
//     (R7), and two deploys posted to the second project's route, one with no pull request (P1) and
//     one naming one (P2);
//   - one failure of each signal: a fail of an after-merge check at R3's head (check_fail), the
//     revert, which blames R1 and which R7 restores (revert), an issue of the second project
//     labelled incident linking R6's pull request (incident_issue), a failure posted on P1 (api)
//     and a person's flag of R4 (person); and a passing after-merge check at R1's head, a fail at a
//     commit no deploy holds and an issue labelled bug linking a deployed pull request, which make
//     none;
//   - with controller not "", a Legion read of each project whose controller observation names it
//     (legion.RecordRead), written where the project's newest read names another session or none.
//
// Every time is relative to now: the window is the two days from the midnight three days before
// now's. The issues take the projects' first numbers (<project>-1 and -2, <project>B-1 to -3), and a
// load is refused where a project holds one of those keys under another title, so the mode goes
// into projects of its own.
func WriteFailures(ctx context.Context, pool *store.Pool, project string, now time.Time, controller string) (FailuresResult, error) {
	second := project + "B"
	lower := strings.ToLower(project)
	appRepo, releaseRepo := "acme/"+lower+"-app", "acme/"+lower+"-release"
	secondRepo, toolsRepo := "acme/"+strings.ToLower(second)+"-app", "acme/"+lower+"-tools"
	day := measures.Midnight(now).AddDate(0, 0, -3)
	at := func(days, hour, minute int) time.Time {
		return day.AddDate(0, 0, days).Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
	}
	result := FailuresResult{
		Projects: []string{project, second}, Defaults: map[string]string{project: appRepo, second: secondRepo},
		Sources: map[string][]FailureSource{}, SecondRepo: releaseRepo, ThirdRepo: toolsRepo,
		FailureSources: map[string][]string{
			project: delivery.FailureSources,
			second:  {delivery.FailureSourceCheckResults, delivery.FailureSourceIncidentLabel, delivery.FailureSourceEvents},
		},
		MeasuredSince: day.Format(time.DateOnly), Window: Window{From: day, To: day.AddDate(0, 0, 2)},
	}
	if _, err := pool.Exec(ctx, `
		insert into projects (key, name, default_assignee, next_number, default_repo)
		select key, key, 'alice', 1, repo from unnest($1::text[], $2::text[]) as seeded(key, repo)
		on conflict (key) do update set default_repo = excluded.default_repo`,
		result.Projects, []string{appRepo, secondRepo}); err != nil {
		return FailuresResult{}, fmt.Errorf("write the projects of the failures mode: %w", err)
	}
	for _, input := range []delivery.ProjectSettingsInput{
		{Project: project, Sources: []delivery.SourceInput{appRule.source(nil), releaseRule.source(new(releaseRepo))}},
		{Project: second, Sources: []delivery.SourceInput{secondRule.source(nil), {Kind: delivery.SourceKindEvents}}},
	} {
		input.Delivery, input.PopulationAuthors, input.ExcludedRepos = true, []string{failuresAuthor}, []string{}
		input.FailureSources, input.FailuresMeasuredSince = result.FailureSources[input.Project], &day
		saved, err := saveSettings(ctx, pool, input, actor)
		if err != nil {
			return FailuresResult{}, err
		}
		for _, source := range saved.Sources {
			reads, _ := delivery.SourceRepoOf(source, saved.DefaultRepo)
			result.Sources[input.Project] = append(result.Sources[input.Project], FailureSource{Kind: source.Kind, Names: source.Repo, Reads: reads})
		}
	}

	key := func(p string, number int) string { return fmt.Sprintf("%s-%d", p, number) }
	work, bug := key(project, 1), key(project, 2)
	shipped, secondWork, incident := key(second, 1), key(second, 2), key(second, 3)
	pullURL := func(repo string, number int) string {
		return fmt.Sprintf("https://github.com/%s/pull/%d", repo, number)
	}
	runs := []failureRun{
		{"R1", appRepo, appRule, at(0, 10, 0), true, []string{appRepo + "#1", appRepo + "#2", appRepo + "#3"}, ""},
		{"R2", appRepo, appRule, at(0, 12, 0), false, nil, "its production job's apply step failed"},
		{"R3", appRepo, appRule, at(0, 14, 0), true, nil, ""},
		{"R4", releaseRepo, releaseRule, at(0, 15, 0), true, nil, ""},
		{"R5", toolsRepo, appRule, at(0, 16, 0), true, nil, "no deploy source reads " + toolsRepo},
		{"R6", secondRepo, secondRule, at(1, 10, 0), true, []string{secondRepo + "#1"}, ""},
		{"R7", appRepo, appRule, at(1, 12, 0), true, nil, ""},
	}
	runID := func(index int) int64 { return int64(failuresFirstRun + index) }
	head := func(index int) string { return commitOf(project, fmt.Sprintf("run-%d", runID(index))) }
	checkFail := failureResult{block: "k-after-merge-fail", verdict: "fail", commit: head(2), at: at(1, 9, 30)}
	passing := failureResult{block: "k-after-merge-pass", verdict: "pass", commit: head(0), at: at(1, 9, 40)}
	unmatched := failureResult{block: "k-unmatched-commit", verdict: "fail", commit: commitOf(project, "unmatched"), at: at(1, 9, 50)}
	issues := []failureIssue{
		{key: work, number: 1, project: project, repo: appRepo, title: "A change of the first project", created: at(-1, 9, 0),
			results: []failureResult{checkFail, passing, unmatched}},
		{key: bug, number: 2, project: project, repo: appRepo, title: "A bug the first project tracks", labels: []string{"bug"},
			link: pullURL(appRepo, 3), created: at(1, 15, 30)},
		{key: shipped, number: 1, project: second, repo: appRepo, title: "A change of the second project in the first project's repository", created: at(-1, 9, 10)},
		{key: secondWork, number: 2, project: second, repo: secondRepo, title: "A change of the second project", created: at(-1, 9, 20)},
		{key: incident, number: 3, project: second, repo: secondRepo, title: "Production broke after a deploy", labels: []string{"incident"},
			link: pullURL(secondRepo, 1), created: at(1, 15, 0)},
	}
	var keywordFill bool
	if err := pool.QueryRow(ctx, `select to_regclass('public.search_text_versions_fill') is not null`).Scan(&keywordFill); err != nil {
		return FailuresResult{}, fmt.Errorf("read whether the database has a keyword fill: %w", err)
	}
	for _, issue := range issues {
		if err := writeFailureIssue(ctx, pool, issue, keywordFill); err != nil {
			return FailuresResult{}, err
		}
	}
	// The record route's write after its commit: a fail of an after-merge check that a deploy row
	// holds makes its row at once; one whose deploy row has not landed yet is the loop's below.
	for _, failed := range []failureResult{checkFail, unmatched} {
		if _, err := signals.RecordCheckFailure(ctx, pool, failed.id(project)); err != nil {
			return FailuresResult{}, fmt.Errorf("write the check_fail of %s: %w", failed.block, err)
		}
	}

	rule := r15{defaults: result.Defaults, issues: map[string]string{work: project, shipped: second, secondWork: second}}
	pulls := map[string]counts{}
	for _, pr := range []struct {
		repo           string
		number         int
		issue          *string
		opened, merged time.Time
	}{
		{appRepo, 1, &work, at(0, 8, 0), at(0, 9, 0)},
		{appRepo, 2, &shipped, at(0, 8, 10), at(0, 9, 10)},
		{appRepo, 3, nil, at(0, 8, 20), at(0, 9, 20)},
		{toolsRepo, 1, nil, at(0, 8, 30), at(0, 9, 30)},
		{secondRepo, 1, &secondWork, at(1, 8, 0), at(1, 9, 0)},
	} {
		name := fmt.Sprintf("%s#%d", pr.repo, pr.number)
		title, titleKeys, named := fmt.Sprintf("Change %d", pr.number), []string{}, "no issue"
		if pr.issue != nil {
			title, titleKeys, named = *pr.issue+": "+title, []string{*pr.issue}, "issue "+*pr.issue
		}
		if err := delivery.UpsertPullRequest(ctx, pool, delivery.DeliveryPullRequest{
			Repo: pr.repo, Number: pr.number, Title: title, URL: pullURL(pr.repo, pr.number), Author: failuresAuthor,
			CreatedAt: new(pr.opened), MergedAt: new(pr.merged), FirstCommitAt: new(pr.opened.Add(-time.Hour)),
			MergeCommitSHA: new(commitOf(project, name)), Additions: new(10), Deletions: new(2), IssueKey: pr.issue,
			Attribution: &delivery.AttributionInputs{TitleKeys: titleKeys, CitedIssues: []string{}, BranchKeys: []string{}, CommitKeys: []string{}},
		}); err != nil {
			return FailuresResult{}, fmt.Errorf("write the pull request %s of the failures mode: %w", name, err)
		}
		pulls[name] = rule.pullRequest(pr.repo, pr.issue)
		result.Rows = append(result.Rows, attributed(fmt.Sprintf("pull request %s (%s, merged %s)", name, named, pr.merged.Format(time.RFC3339)),
			pr.merged, pulls[name]))
	}

	// The CI jobs of the first project's default repository: a PR-checks run of #1, whose issue is
	// the first project's, and one GitHub names no pull request of.
	for index, number := range []*int{new(1), nil} {
		id := runID(len(runs) + index)
		started := at(0, 8, 40+5*index)
		branch, of, pull := "feature/fork", "no pull request", counts(nil)
		if number != nil {
			branch, of, pull = fmt.Sprintf("feature/%d", *number), fmt.Sprintf("#%d", *number), pulls[fmt.Sprintf("%s#%d", appRepo, *number)]
		}
		if err := writeRun(ctx, pool, delivery.DeliveryRun{
			Repo: appRepo, RunID: id, Kind: delivery.DeliveryRunKindPRChecks, PRNumber: number,
			HeadSHA: commitOf(project, fmt.Sprintf("run-%d", id)), HeadCommitAt: started.Add(-time.Minute), StartedAt: started,
			CreatedAt: new(started.Add(-10 * time.Second)), CompletedAt: new(started.Add(8 * time.Minute)),
			Conclusion: new(delivery.DeliveryRunConclusionSuccess),
			URL:        fmt.Sprintf("https://github.com/%s/actions/runs/%d", appRepo, id), HeadBranch: new(branch), Event: new("pull_request"),
		}, delivery.DeliveryRunJob{
			Name: checkJob, StartedAt: new(started.Add(5 * time.Second)), CompletedAt: new(started.Add(8*time.Minute - 5*time.Second)),
			Conclusion: new(delivery.DeliveryJobConclusionSuccess),
		}); err != nil {
			return FailuresResult{}, err
		}
		result.Rows = append(result.Rows, attributed(fmt.Sprintf("CI job %q of PR-checks run %s/%d of %s (started %s)", checkJob, appRepo, id, of,
			started.Format(time.RFC3339)), started, rule.ciJob(appRepo, pull)))
	}

	owners := map[string]string{appRepo: project, releaseRepo: project, secondRepo: second}
	reaches := map[string]counts{}
	for index, run := range runs {
		id, started := runID(index), run.created.Add(30*time.Second)
		job := delivery.DeliveryRunJob{
			Name: run.rule.job, StartedAt: new(started.Add(time.Minute)), CompletedAt: new(run.created.Add(10 * time.Minute)),
			Conclusion: new(delivery.DeliveryJobConclusionSuccess), ApplyStep: run.rule.step,
		}
		if run.rule.step != nil {
			job.ApplyStepConclusion = new(delivery.DeliveryJobConclusionSuccess)
			if !run.applied {
				job.ApplyStepConclusion = new(delivery.DeliveryJobConclusionFailure)
			}
		}
		if err := writeRun(ctx, pool, delivery.DeliveryRun{
			Repo: run.repo, RunID: id, Kind: delivery.DeliveryRunKindDeploy, HeadSHA: head(index),
			HeadCommitAt: run.created.Add(-time.Minute), StartedAt: started, CreatedAt: new(run.created),
			CompletedAt: new(run.created.Add(11 * time.Minute)), Conclusion: new(delivery.DeliveryRunConclusionSuccess),
			URL: fmt.Sprintf("https://github.com/%s/actions/runs/%d", run.repo, id), HeadBranch: new("main"), Event: new("push"),
		}, job); err != nil {
			return FailuresResult{}, err
		}
		if run.why != "" {
			result.NoDeploys = append(result.NoDeploys, NoDeploy{Repo: run.repo, RunID: id, Why: run.why})
			continue
		}
		// A deploy counts for the projects of the pull requests it ships, and one that ships none
		// for the project whose source reads its repository (D1, R12), also where that is no
		// project's default.
		reached := counts{owners[run.repo]: true}
		if len(run.ships) > 0 {
			shippedPulls := make([]counts, len(run.ships))
			for i, pull := range run.ships {
				shippedPulls[i] = pulls[pull]
			}
			reached = rule.deploy(run.repo, shippedPulls)
		}
		reaches[run.name] = reached
		ships := "ships none"
		if len(run.ships) > 0 {
			ships = "ships " + strings.Join(run.ships, ", ")
		}
		result.Rows = append(result.Rows, attributed(fmt.Sprintf("deploy run %s/%d (%s, %s, started %s)", run.repo, id, run.name, ships,
			started.Format(time.RFC3339)), started, reached))
	}
	// The revert of #1, merged before R7, which ships it; GitHub's revert button opened it, whose
	// author is outside the population, so it is a revert alone and no population pull request.
	if err := delivery.RecordRevert(ctx, pool, delivery.Revert{
		Repo: appRepo, Number: 4, RevertsRepo: appRepo, RevertsNumber: 1, RevertsMergeCommitSHA: new(commitOf(project, appRepo+"#1")),
		MergedAt: at(1, 11, 0), MergeCommitSHA: new(commitOf(project, appRepo+"#4")),
	}); err != nil {
		return FailuresResult{}, fmt.Errorf("write the revert of the failures mode: %w", err)
	}

	// The production signals' rows: the GitHub adapter's deploy rows, the revert, the incident and
	// the check_fail whose deploy row lands with them.
	deployRun := func(ctx context.Context, index int) (prodevents.Event, bool, error) {
		return prodevents.DeployForRun(ctx, pool, runs[index].repo, runID(index))
	}
	signalRefs := map[string]string{
		prodevents.SignalRevert:        signals.RevertSourceRef(appRepo, 4),
		prodevents.SignalIncidentIssue: signals.IncidentSourceRef(incident),
	}
	landed := func(ctx context.Context) (bool, error) {
		for index, run := range runs {
			if run.why != "" {
				continue
			}
			row, held, err := deployRun(ctx, index)
			if err != nil || !held {
				return false, err
			}
			if index == 2 {
				var artifact string
				if err := pool.QueryRow(ctx, `select check_artifact_id::text from check_results where id = $1`, checkFail.id(project)).Scan(&artifact); err != nil {
					return false, fmt.Errorf("read the Verification document of %s: %w", checkFail.block, err)
				}
				signalRefs[prodevents.SignalCheckFail] = signals.CheckSourceRef(artifact, checkFail.block, row.ID)
			}
		}
		for signal, ref := range signalRefs {
			owner := project
			if signal == prodevents.SignalIncidentIssue {
				owner = second
			}
			if _, held, err := prodevents.BySourceRef(ctx, pool, owner, ref); err != nil || !held {
				return false, err
			}
		}
		return true, nil
	}
	if err := awaitSignals(ctx, pool, landed); err != nil {
		return FailuresResult{}, err
	}
	deploys := map[string]prodevents.Event{}
	for index, run := range runs {
		if run.why != "" {
			continue
		}
		row, _, err := deployRun(ctx, index)
		if err != nil {
			return FailuresResult{}, err
		}
		deploys[run.name] = row
		result.Deploys = append(result.Deploys, deployOf(run.name, row, new(runID(index)), reaches[run.name]))
	}

	// The second project's posted deploys and the failure posted on P1, through the record's store
	// as its route writes them, and the person's flag of R4, keyed as that route keys it.
	for _, post := range []struct {
		name, ref, repo string
		finished        time.Time
		pulls           []string
	}{
		{"P1", "release-1", "", at(1, 13, 0), nil},
		{"P2", "release-2", secondRepo, at(1, 14, 0), []string{delivery.PullRequestName(secondRepo, 1)}},
	} {
		row, _, err := prodevents.Write(ctx, pool, prodevents.Event{
			ProjectKey: second, Kind: prodevents.KindDeploy, Status: prodevents.StatusSucceeded, Source: prodevents.SourceAPI,
			SourceRef: post.ref, StartedAt: new(post.finished.Add(-5 * time.Minute)), FinishedAt: new(post.finished),
			Repo: post.repo, PullRequests: post.pulls, Actor: failuresPoster,
		})
		if err != nil {
			return FailuresResult{}, fmt.Errorf("post the deploy %s of %s: %w", post.ref, second, err)
		}
		reached := counts{second: false}
		deploys[post.name] = row
		result.Deploys = append(result.Deploys, deployOf(post.name, row, nil, reached))
		result.Rows = append(result.Rows, attributed(fmt.Sprintf("deploy %s posted to %s (%s, finished %s)", post.ref, second, post.name,
			post.finished.Format(time.RFC3339)), post.finished, reached))
	}
	signalRefs[prodevents.SignalAPI] = "page-1"
	signalRefs[prodevents.SignalPerson] = prodevents.PersonSourceRef(deploys["R4"].ID, failuresFlagger.ID)
	for _, post := range []prodevents.Event{
		{ProjectKey: second, Source: prodevents.SourceAPI, SourceRef: signalRefs[prodevents.SignalAPI], Signal: new(prodevents.SignalAPI),
			DeployID: new(deploys["P1"].ID), DetectedAt: new(at(1, 13, 30)), Actor: failuresPoster},
		{ProjectKey: project, Source: prodevents.SourcePerson, SourceRef: signalRefs[prodevents.SignalPerson], Signal: new(prodevents.SignalPerson),
			DeployID: new(deploys["R4"].ID), DetectedAt: new(at(1, 16, 0)), Actor: failuresFlagger},
	} {
		post.Kind = prodevents.KindFailure
		if _, _, err := prodevents.Write(ctx, pool, post); err != nil {
			return FailuresResult{}, fmt.Errorf("post the failure %s of %s: %w", post.SourceRef, post.ProjectKey, err)
		}
	}

	if controller != "" {
		for _, p := range result.Projects {
			named, held, err := legion.ControllerSession(ctx, pool, p)
			if err != nil {
				return FailuresResult{}, err
			}
			if held && named == controller {
				continue
			}
			observations := legion.State{ControllerLocator: &legion.ControllerLocator{SessionID: controller}}.Observations()
			if _, err := legion.RecordRead(ctx, pool, p, now, 0, observations); err != nil {
				return FailuresResult{}, err
			}
		}
		result.Controller = &controller
	}

	oracle := failureOracle{sources: result.FailureSources}
	names := map[string]string{}
	for name, row := range deploys {
		names[row.ID] = name
	}
	byName := map[string]FailureDeploy{}
	for _, deploy := range result.Deploys {
		byName[deploy.Name] = deploy
	}
	for _, signal := range []string{prodevents.SignalCheckFail, prodevents.SignalRevert, prodevents.SignalIncidentIssue, prodevents.SignalAPI, prodevents.SignalPerson} {
		owner := project
		if signal == prodevents.SignalIncidentIssue || signal == prodevents.SignalAPI {
			owner = second
		}
		row, held, err := prodevents.BySourceRef(ctx, pool, owner, signalRefs[signal])
		if err != nil {
			return FailuresResult{}, err
		}
		if !held || row.DeployID == nil || row.DetectedAt == nil {
			return FailuresResult{}, fmt.Errorf("the %s signal of the failures mode made no failure row of %s under %s", signal, owner, signalRefs[signal])
		}
		failure := FailureSignal{
			Signal: signal, ID: row.ID, ProjectKey: row.ProjectKey, SourceRef: row.SourceRef, State: row.State,
			Deploy: names[*row.DeployID], DetectedAt: row.DetectedAt.UTC(), RestoredAt: row.RestoredAt, CountsFor: []string{},
		}
		if row.RestoredByDeploy != nil {
			failure.RestoredBy = new(names[*row.RestoredByDeploy])
		}
		for _, p := range byName[failure.Deploy].CountsFor {
			if oracle.counts(p, signal) {
				failure.CountsFor = append(failure.CountsFor, p)
			}
		}
		result.Failures = append(result.Failures, failure)
	}
	result.Negatives = []NoSignal{
		{What: "a passing after-merge check at R1's head", Ref: passing.id(project)},
		{What: "a failing after-merge check at a commit no deploy holds", Ref: unmatched.id(project)},
		{What: "an issue labelled bug linking " + pullURL(appRepo, 3), Ref: bug},
	}
	result.Expects = expectsOf(result, oracle, project)
	return result, nil
}

// deployOf is row as the result names it, counting for reached.
func deployOf(name string, row prodevents.Event, runID *int64, reached counts) FailureDeploy {
	return FailureDeploy{
		Name: name, ID: row.ID, ProjectKey: row.ProjectKey, SourceRef: row.SourceRef, Repo: row.Repo, RunID: runID,
		FinishedAt: row.FinishedAt.UTC(), PullRequests: row.PullRequests, CountsFor: slices.Sorted(maps.Keys(reached)),
	}
}

// expectsOf is the counts each check expects of result: first's sources and its deploys per
// repository, the third repository's among them at 0, are k2's.
func expectsOf(result FailuresResult, oracle failureOracle, first string) FailureExpects {
	expects := FailureExpects{
		SourcesSaved: map[string]int{}, DeploysByRepository: map[string]int{result.ThirdRepo: 0},
		Signals: len(result.Failures), RowsBySignal: map[string]int{}, Measures: map[string]FailureMeasures{},
		RowsPlaced: len(result.Rows),
	}
	for project, sources := range result.Sources {
		expects.SourcesSaved[project] = len(sources)
	}
	for _, deploy := range result.Deploys {
		if deploy.RunID != nil && slices.Contains(deploy.CountsFor, first) {
			expects.DeploysByRepository[deploy.Repo]++
		}
	}
	for _, failure := range result.Failures {
		expects.RowsBySignal[failure.Signal]++
		if failure.State == prodevents.StateCounted {
			expects.CountedOnArrival++
		}
	}
	for _, project := range result.Projects {
		expects.Measures[project] = oracle.measures(project, result.Deploys, result.Failures, result.Window)
	}
	return expects
}

// failureOracle is D7's rule (R9 and R11), the oracle the Failures mode states
// each project's measures by, written from its words and not from the evals' builder: "P's deploys
// on day D are the deploy rows finished on D that the attribution counts for P, and the failures
// counted for P are the unrejected failure rows on those deploys whose signal maps to a source P
// has on (check_fail → check_results, revert → reverts, incident_issue → incident_label, api →
// events, person → counted whenever any source is on). Change fail rate: deploys with a counted
// failure over deploys. Recovery: the median of restored_at less the deploy's finished_at over the
// counted failures restored on D, beside the count of counted failures not restored. Rework rate:
// deploys finished on D named as restoring a counted failure over deploys finished on D." sources
// is each project's failure sources.
type failureOracle struct {
	sources map[string][]string
}

// signalSource is the failure source each signal but a person's flag maps to.
var signalSource = map[string]string{
	prodevents.SignalCheckFail:     delivery.FailureSourceCheckResults,
	prodevents.SignalRevert:        delivery.FailureSourceReverts,
	prodevents.SignalIncidentIssue: delivery.FailureSourceIncidentLabel,
	prodevents.SignalAPI:           delivery.FailureSourceEvents,
}

// counts reports whether project counts a failure of signal.
func (o failureOracle) counts(project, signal string) bool {
	if signal == prodevents.SignalPerson {
		return len(o.sources[project]) > 0
	}
	return slices.Contains(o.sources[project], signalSource[signal])
}

// measures is project's three measures over window, of deploys and the failures that blame them.
func (o failureOracle) measures(project string, deploys []FailureDeploy, failures []FailureSignal, window Window) FailureMeasures {
	in := map[string]FailureDeploy{}
	for _, deploy := range deploys {
		if slices.Contains(deploy.CountsFor, project) && !deploy.FinishedAt.Before(window.From) && deploy.FinishedAt.Before(window.To) {
			in[deploy.Name] = deploy
		}
	}
	folded := FailureMeasures{Deploys: len(in)}
	failed, restoring := map[string]bool{}, map[string]bool{}
	var minutes []float64
	for _, failure := range failures {
		deploy, held := in[failure.Deploy]
		if !held || failure.State != prodevents.StateCounted || !o.counts(project, failure.Signal) {
			continue
		}
		failed[failure.Deploy] = true
		if failure.RestoredAt == nil {
			folded.NotRestored++
			continue
		}
		if failure.RestoredBy != nil {
			if _, mine := in[*failure.RestoredBy]; mine {
				restoring[*failure.RestoredBy] = true
			}
		}
		if !failure.RestoredAt.Before(window.From) && failure.RestoredAt.Before(window.To) {
			minutes = append(minutes, failure.RestoredAt.Sub(deploy.FinishedAt).Minutes())
		}
	}
	folded.Failed, folded.Restoring, folded.Restored = len(failed), len(restoring), len(minutes)
	if folded.Deploys > 0 {
		folded.ChangeFailRate = new(float64(folded.Failed) / float64(folded.Deploys))
		folded.ReworkRate = new(float64(folded.Restoring) / float64(folded.Deploys))
	}
	if len(minutes) > 0 {
		slices.Sort(minutes)
		middle := len(minutes) / 2
		median := minutes[middle]
		if len(minutes)%2 == 0 {
			median = (minutes[middle-1] + minutes[middle]) / 2
		}
		folded.RecoveryMedian = &median
	}
	return folded
}

// writeFailureIssue stores issue in a transaction of its own, as the issue and result routes store
// what it holds: the issue, its own link, its Verification document, the issue.created event of an
// issue with labels (the incidents feed's input) and, for each check result, its result.recorded
// event and then its row naming that event, the issue's last_seq at its last event. An issue held
// already is the fixture's where its title is, and was written whole with it; one of another title
// refuses the load. The events are dated in the fixture's past, so, where the database has the
// keyword fill, the statement that writes them nulls the fill's stored reach (legiontest's write
// says why).
func writeFailureIssue(ctx context.Context, pool *store.Pool, issue failureIssue, keywordFill bool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the issue %s: %w", issue.key, err)
	}
	defer tx.Rollback(ctx)
	by, err := json.Marshal(actor)
	if err != nil {
		return fmt.Errorf("encode the author of %s: %w", issue.key, err)
	}
	var title string
	err = tx.QueryRow(ctx, `
		insert into issues (key, project_key, number, title, status, rank, repo, labels, created_by, created_at)
		values ($1, $2, $3, $4, 'todo', $5, $6, $7, $8::jsonb, $9)
		on conflict (key) do nothing
		returning title`,
		issue.key, issue.project, issue.number, issue.title, fmt.Sprintf("%08d", issue.number), issue.repo,
		append([]string{}, issue.labels...), by, issue.created).Scan(&title)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.QueryRow(ctx, `select title from issues where key = $1`, issue.key).Scan(&title); err != nil {
			return fmt.Errorf("read the issue %s: %w", issue.key, err)
		}
		if title != issue.title {
			return fmt.Errorf("%s holds the issue %q, not the failures mode's %q: load -failures into projects of its own", issue.key, title, issue.title)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("write the issue %s: %w", issue.key, err)
	}
	var seeded []legiontest.SeededIssueEvent
	if issue.link != "" {
		if _, err := tx.Exec(ctx, `insert into issue_external_links (issue_key, url, kind) values ($1, $2, 'url')`, issue.key, issue.link); err != nil {
			return fmt.Errorf("link %s to %s: %w", issue.key, issue.link, err)
		}
	}
	if len(issue.labels) > 0 {
		links := []map[string]string{}
		if issue.link != "" {
			links = append(links, map[string]string{"url": issue.link, "kind": "url"})
		}
		seeded = append(seeded, legiontest.SeededIssueEvent{
			Type: "issue.created", Actor: failuresFlagger, At: issue.created,
			Payload: map[string]any{
				"key": issue.key, "project": issue.project, "title": issue.title, "labels": issue.labels, "external_links": links,
			},
		})
	}
	var artifact string
	if len(issue.results) > 0 {
		if err := tx.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
			values ($1, $2, 'verification-md', 'verification.md', 'doc', $3::jsonb)
			returning id::text`, issue.key, issue.project, by).Scan(&artifact); err != nil {
			return fmt.Errorf("write the Verification document of %s: %w", issue.key, err)
		}
	}
	for _, result := range issue.results {
		seeded = append(seeded, legiontest.SeededIssueEvent{
			Type: "result.recorded", Actor: failuresTester, At: result.at,
			Payload: model.CheckResultEventPayload{
				ResultID: result.id(issue.project), Issue: issue.key, CheckIssue: issue.key, CheckArtifactID: artifact,
				CheckBlockID: result.block, CheckVersion: 1, When: model.CheckWhenAfterMerge, Verdict: result.verdict, Role: "tester",
				CommitSHA: result.commit,
			},
		})
	}
	if err := legiontest.WriteIssueEvents(ctx, tx, issue.key, seeded, keywordFill); err != nil {
		return err
	}
	tester, err := json.Marshal(failuresTester)
	if err != nil {
		return fmt.Errorf("encode the tester: %w", err)
	}
	for index, result := range issue.results {
		seq := len(seeded) - len(issue.results) + index + 1
		if _, err := tx.Exec(ctx, `
			insert into check_results (id, issue_key, check_issue_key, check_artifact_id, check_block_id, check_version,
				check_sha256, role, commit_sha, verdict, actor, event_seq, created_at)
			values ($1, $2, $2, $3, $4, 1, $5, 'tester', $6, $7, $8::jsonb, $9, $10)`,
			result.id(issue.project), issue.key, artifact, result.block, commitOf(issue.project, "block/"+result.block),
			result.commit, result.verdict, tester, seq, result.at); err != nil {
			return fmt.Errorf("write the result %s of %s: %w", result.block, issue.key, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the issue %s: %w", issue.key, err)
	}
	return nil
}

// signalsCallers numbers the lease pool's reservations awaitSignals takes, one per load that waits,
// since a process reserves each name once (store.LeaseCallers.For).
var signalsCallers atomic.Int64

// awaitSignals runs the production signals' loop as the server runs it (signals.Run, under its
// lease, so it waits its turn behind a server's loop that holds it) until landed answers true, and
// stops it; a load whose rows have landed already (a second load, or one into a database a
// server's loop writes) starts none. The loop's event feeds read up to the event horizon, which any
// open transaction on the Postgres instance holds back (events.Horizon), so the wait stands while
// the loop's horizon is held back, and refuses to wait signalsWait past that.
func awaitSignals(ctx context.Context, pool *store.Pool, landed func(context.Context) (bool, error)) error {
	if done, err := landed(ctx); err != nil || done {
		return err
	}
	callers, err := pool.Leases()
	if err != nil {
		return fmt.Errorf("the lease pool: %w", err)
	}
	leases, err := callers.For(fmt.Sprintf("evalfixture production signals %d", signalsCallers.Add(1)))
	if err != nil {
		return err
	}
	horizon := events.NewHorizon(pool)
	loop, stop := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		signals.Run(loop, signals.Deps{Pool: pool, Horizon: horizon, Leases: leases})
	}()
	defer func() {
		stop()
		<-stopped
	}()
	deadline := time.Now().Add(signalsWait)
	for {
		done, err := landed(ctx)
		if err != nil || done {
			return err
		}
		if horizon.Gap() != events.GapNone {
			deadline = time.Now().Add(signalsWait)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the production signals' loop wrote the failures mode's deploy and failure rows in no %s", signalsWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
