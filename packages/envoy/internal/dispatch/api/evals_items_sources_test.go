package api

import (
	"maps"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliveryseed"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/prodevents"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestAProjectWithNoSourceListsThePullRequestsItsDayRowsCount: over deliverytest's history, the
// second project (DELIVX) holds no deploy source of its own, and its merges count for it through
// their issues (INFRA-978 R27; the history's attribution inputs are left unread, so the issue each
// pull request names is its attribution, leaveAttributionInputsUnread). Its pr-open-to-merge items
// list exactly the pull requests its day rows count, as a number and the rows behind it are one
// set.
func TestAProjectWithNoSourceListsThePullRequestsItsDayRowsCount(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	fixture := deliverytest.Seed(t, database.Pool, deliveryseed.Spec{Days: 5})
	leaveAttributionInputsUnread(t, database.Pool)
	refreshEvalDays(t, evals.New(database, deps.Docs, deps.Events, deps.Attributor), database, fixture.To.Add(-time.Minute))
	project := fixture.SecondProject
	window := evals.Range{From: fixture.From, To: fixture.To}

	counted, err := evals.CountedIn(t.Context(), database.Pool, project, evals.KindDelivery, []string{""}, nil, window)
	if err != nil {
		t.Fatalf("read the items %s's day rows count: %v", project, err)
	}
	want := map[string]bool{}
	for _, item := range counted {
		if strings.HasPrefix(item.RowKey, "pull_request:") {
			want[item.RowKey] = true
		}
	}
	if len(want) == 0 {
		t.Fatalf("%s's day rows count no pull request over %s..%s, so the comparison compares nothing",
			project, window.From.Format(time.DateOnly), window.To.Format(time.DateOnly))
	}

	query := url.Values{
		"from": {window.From.Format(time.RFC3339)}, "to": {window.To.Format(time.RFC3339)},
		"row_kind": {evalRowPullRequest}, "limit": {"200"},
	}
	answer := evalGet[evalItemsResponse](t, handler, "/api/v1/projects/"+project+"/evals/"+evals.SlugPROpenToMerge+"/items?"+query.Encode())
	got := map[string]bool{}
	for _, row := range answer.Items {
		repo, _ := row.Cols["repo"].(string)
		number, _ := row.Cols["number"].(float64)
		got[evals.PullRequestMember(repo, int(number))] = true
	}
	if listed, wanted := slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)); !slices.Equal(listed, wanted) {
		t.Errorf("%s's pr-open-to-merge items list %v, want the %d pull requests its day rows count, %v", project, listed, len(wanted), wanted)
	}
	t.Logf("%s's pr-open-to-merge items list the %d pull requests its day rows count", project, len(want))
}

// seedSourceCheck writes a completed PR check with a build job. The pull request's stored
// attribution makes the check count for the issue's project even in a non-default repository.
func seedSourceCheck(t *testing.T, database *store.Store, repo, issue string, id int64, hour int, fails bool) {
	t.Helper()
	start := sourcesDay.Add(time.Duration(hour) * time.Hour)
	end := start.Add(10 * time.Minute)
	pr := delivery.DeliveryPullRequest{
		Repo: repo, Number: int(id), Title: "feat: " + issue, Author: "octocat", IssueKey: &issue,
		URL:       "https://github.com/" + repo + "/pull/" + strconv.FormatInt(id, 10),
		CreatedAt: new(start.Add(-time.Hour)), MergedAt: new(end.Add(time.Minute)),
	}
	upsertPR(t, database, pr)
	runConclusion, jobConclusion := delivery.DeliveryRunConclusionSuccess, delivery.DeliveryJobConclusionSuccess
	if fails {
		runConclusion, jobConclusion = delivery.DeliveryRunConclusionFailure, delivery.DeliveryJobConclusionFailure
	}
	ctx := t.Context()
	if err := delivery.UpsertRun(ctx, database.Pool, delivery.DeliveryRun{
		Repo: repo, RunID: id, Kind: delivery.DeliveryRunKindPRChecks, PRNumber: &pr.Number,
		StartedAt: start, HeadCommitAt: start, HeadSHA: "sha" + strconv.FormatInt(id, 10),
		CompletedAt: &end, Conclusion: &runConclusion, Event: new("pull_request"), HeadBranch: new("feature"),
		URL: "https://github.com/" + repo + "/actions/runs/" + strconv.FormatInt(id, 10),
	}); err != nil {
		t.Fatalf("seed check run %s/%d: %v", repo, id, err)
	}
	if err := delivery.UpsertRunJobs(ctx, database.Pool, repo, id, []delivery.DeliveryRunJob{{
		Repo: repo, RunID: id, Name: "build", StartedAt: &start, CompletedAt: &end, Conclusion: &jobConclusion,
	}}, true); err != nil {
		t.Fatalf("seed the build job of %s/%d: %v", repo, id, err)
	}
}

// The two source repositories' deploy-frequency items judge production by each repository's
// job, so neither repository's run of the other's job reaches production. The pr-ci items
// include the checks of both repositories. Each repository's counted items and the project's
// combined count agree with the corresponding day rows.
func TestTheItemsOfAProjectWithTwoSourcesReadEachUnderItsOwnRule(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{})
	const project, one, two = "ALPHA", "acme/one", "acme/two"
	issue := createTestIssue(t, handler, project, "Changes in two repositories")
	saveProjectRules(t, database, project, deployRule(one, "one / release"), deployRule(two, "two / release"))
	seedSourceRun(t, database, one, 101, 1, "one / release")
	seedSourceRun(t, database, two, 201, 2, "two / release")
	seedSourceRun(t, database, two, 202, 3, "one / release")
	seedSourceRun(t, database, one, 102, 4, "two / release")
	seedSourceCheck(t, database, one, issue, 103, 5, false)
	seedSourceCheck(t, database, two, issue, 203, 6, true)
	seedSourceCheck(t, database, two, issue, 204, 7, false)
	leaveAttributionInputsUnread(t, database.Pool)
	next := sourcesDay.AddDate(0, 0, 1)
	refreshEvalDays(t, evals.New(database, deps.Docs, deps.Events, deps.Attributor), database, next.Add(-time.Minute))

	for _, check := range []struct {
		slug, kind, series, count string
		rows                      map[string]bool // row key and whether the series counts it
	}{
		{evals.SlugDeployFrequency, evalRowDeploy, "deploys", "sum", map[string]bool{
			one + "/101": true, one + "/102": false, two + "/201": true, two + "/202": false,
		}},
		{evals.SlugPRCI, evalRowRun, "minutes", "n", map[string]bool{
			one + "/103": true, two + "/203": true, two + "/204": true,
		}},
	} {
		t.Run(check.slug, func(t *testing.T) {
			path := evalItemsPath(project, check.slug)
			query := url.Values{"from": {sourcesDay.Format(time.RFC3339)}, "to": {next.Format(time.RFC3339)}}
			query.Set("row_kind", check.kind)
			answer := evalGet[evalItemsResponse](t, handler, path+"?"+query.Encode())
			got := map[string]bool{}
			counted := map[string]float64{}
			for _, row := range answer.Items {
				repo := row.RowKey[:max(0, strings.LastIndex(row.RowKey, "/"))]
				if row.Cols["repo"] != repo {
					t.Errorf("row %q names repository %v, want %s", row.RowKey, row.Cols["repo"], repo)
				}
				inSeries := row.Cols["reached_production"] == true
				if check.kind == evalRowRun {
					_, inSeries = row.Cols["minutes"].(float64)
				}
				got[row.RowKey] = inSeries
				if inSeries {
					counted[repo]++
					counted[""]++
				}
			}
			if !maps.Equal(got, check.rows) || len(answer.Items) != len(check.rows) {
				t.Fatalf("%s lists %v (true when counted), want %v", check.slug, got, check.rows)
			}

			query.Set("row_kind", evalRowDay)
			query.Set("series", check.series)
			for _, repo := range []string{"", one, two} {
				if repo != "" {
					query.Set("repo", repo)
				}
				days := evalGet[evalItemsResponse](t, handler, path+"?"+query.Encode())
				// Without a repository filter there can be one day row per source.
				var total float64
				for _, day := range days.Items {
					value, ok := day.Cols[check.count].(float64)
					if !ok {
						t.Errorf("%s's day row %s has no numeric %s", check.slug, day.RowKey, check.count)
					}
					total += value
				}
				if total != counted[repo] {
					t.Errorf("%s's %q day rows count %v, want %v items", check.slug, repo, total, counted[repo])
				}
			}
		})
	}
}

// TestAPullRequestItemIsTimedToTheDeployItsDayRowsName: acme/alpha-app#1 and #2, of ALPHA's issue,
// merge at 09:00, and ALPHA's GitHub deploy 101 ships both at 10:29; a deploy posted to OMEGA's
// route at 09:30, which OMEGA's events source reads, names #1, so OMEGA's merge-to-production
// times #1 to the post. OMEGA's merge-to-production items time #1 as its day rows do: to the
// post's finish, with no run id, 30 minutes from the merge, 90 from the opening and 120 from the
// first commit, and the same with ALPHA's repository named as without. ALPHA's time #2, which no
// post names, to run 101, 89 minutes. Each project's items' minutes are the samples its day rows
// of the day hold.
func TestAPullRequestItemIsTimedToTheDeployItsDayRowsName(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{})
	const app = "acme/alpha-app"
	issue := createTestIssue(t, handler, "ALPHA", "Two changes to the app")
	createTestProject(t, handler, "OMEGA")
	saveProjectRules(t, database, "ALPHA", deployRule(app, "release"))
	saveProjectDelivery(t, database.Pool, delivery.ProjectSettingsInput{
		Project: "OMEGA", Delivery: true, Sources: []delivery.SourceInput{{Kind: delivery.SourceKindEvents}},
		PopulationAuthors: []string{"octocat"}, ExcludedRepos: []string{},
	})
	at := func(hour, minute int) time.Time {
		return sourcesDay.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
	}
	for _, number := range []int{1, 2} {
		upsertPR(t, database, delivery.DeliveryPullRequest{
			Repo: app, Number: number, Title: "feat: " + issue, Author: "octocat", IssueKey: &issue,
			URL:           "https://github.com/" + app + "/pull/" + strconv.Itoa(number),
			FirstCommitAt: new(at(7, 30)), CreatedAt: new(at(8, 0)), MergedAt: new(at(9, 0)),
		})
	}
	seedSourceRun(t, database, app, 101, 10, "release")
	writeProductionEvent(t, database.Pool, prodevents.Event{
		ProjectKey: "OMEGA", Kind: prodevents.KindDeploy, Source: prodevents.SourceAPI, SourceRef: "release-1",
		FinishedAt: new(at(9, 30)), PullRequests: []string{app + "#1"},
	})
	leaveAttributionInputsUnread(t, database.Pool)
	next := sourcesDay.AddDate(0, 0, 1)
	refreshEvalDays(t, evals.New(database, deps.Docs, deps.Events, deps.Attributor), database, next.Add(-time.Minute))

	// items is project's merge-to-production pull requests of the day, by row key, those of repo
	// alone where it is not "".
	items := func(project, repo string) map[string]map[string]any {
		t.Helper()
		query := url.Values{"from": {sourcesDay.Format(time.RFC3339)}, "to": {next.Format(time.RFC3339)}, "row_kind": {evalRowPullRequest}}
		if repo != "" {
			query.Set("repo", repo)
		}
		answer := evalGet[evalItemsResponse](t, handler, evalItemsPath(project, evals.SlugMergeToProduction)+"?"+query.Encode())
		rows := map[string]map[string]any{}
		for _, row := range answer.Items {
			rows[row.RowKey] = row.Cols
		}
		return rows
	}
	// shipped fails unless project's item key is timed to a deploy of run id run (nil for a posted
	// deploy) that finished at finished, with the lead times minutes.
	shipped := func(project, key string, row map[string]any, run any, finished time.Time, minutes map[string]float64) {
		t.Helper()
		if row == nil {
			t.Errorf("%s's merge-to-production items hold no %s", project, key)
			return
		}
		deployed, _ := row["deployed_at"].(string)
		if when, err := time.Parse(time.RFC3339, deployed); err != nil || !when.Equal(finished) || row["deploy_run_id"] != run {
			t.Errorf("%s's item %s was deployed at %v by run %v, want at %s by %v", project, key, row["deployed_at"], row["deploy_run_id"],
				finished.Format(time.RFC3339), run)
		}
		for _, column := range slices.Sorted(maps.Keys(minutes)) {
			if row[column] != minutes[column] {
				t.Errorf("%s's item %s holds %s %v, want %v", project, key, column, row[column], minutes[column])
			}
		}
	}

	omega := items("OMEGA", "")
	if len(omega) != 1 {
		t.Errorf("OMEGA's merge-to-production items list %v, want %s#1 alone", slices.Sorted(maps.Keys(omega)), app)
	}
	shipped("OMEGA", app+"#1", omega[app+"#1"], nil, at(9, 30), map[string]float64{
		"merge_to_production_minutes": 30, "open_to_production_minutes": 90, "first_commit_to_production_minutes": 120,
	})
	if cut := items("OMEGA", app); !reflect.DeepEqual(cut, omega) {
		t.Errorf("OMEGA's merge-to-production items of %s are %v, want those of every repository, %v", app, cut, omega)
	}
	alpha := items("ALPHA", "")
	shipped("ALPHA", app+"#2", alpha[app+"#2"], 101.0, at(10, 29), map[string]float64{"merge_to_production_minutes": 89})
	for project, rows := range map[string]map[string]map[string]any{"ALPHA": alpha, "OMEGA": omega} {
		listed := []float64{}
		for _, row := range rows {
			if minutes, ok := row["merge_to_production_minutes"].(float64); ok {
				listed = append(listed, minutes)
			}
		}
		slices.Sort(listed)
		var stored []float64
		if err := database.Pool.QueryRow(t.Context(), `
			select coalesce(array_agg(s.sample order by s.sample), '{}') from eval_days d cross join lateral unnest(d.samples) as s(sample)
			where d.project_key = $1 and d.eval_id = $2 and d.series = 'minutes' and d.repo = '' and d.day = $3
		`, project, evals.EvalID(project, evals.SlugMergeToProduction), sourcesDay).Scan(&stored); err != nil {
			t.Fatalf("read %s's merge-to-production samples: %v", project, err)
		}
		if !slices.Equal(listed, stored) {
			t.Errorf("%s's merge-to-production items time %v, want the samples its day rows hold, %v", project, listed, stored)
		}
	}
}
