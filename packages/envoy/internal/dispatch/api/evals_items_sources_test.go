package api

import (
	"maps"
	"net/http"
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
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
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

// TestAPullRequestItemIsTimedToTheDeployItsDayRowsName (dispatch://INFRA-978, R12):
// acme/alpha-app#1 and #2, of ALPHA's issue, merge at 09:00, and ALPHA's GitHub deploy 101 ships
// both at 10:29; a deploy posted to OMEGA's route at 09:30, which OMEGA's events source reads,
// names #1, so OMEGA's merge-to-production times #1 to the post. OMEGA's merge-to-production items
// time #1 as its day rows do: to the post's finish, with no run id, 30 minutes from the merge, and
// the same with ALPHA's repository named as without; its first-commit-to-production and
// open-to-production hold no minutes, since a posted deploy times merge-to-production alone and no
// GitHub deploy counts #1 for OMEGA (dispatch://INFRA-978/comment/ed4092f8-cdb6-4d99-b37c-fbb56755cc57,
// point 3), so those two evals' items list nothing of OMEGA's, as their n counts nothing. The post
// counts for OMEGA alone, so it moves none of ALPHA's timing: ALPHA's items time #1 and #2 to run
// 101, 89 minutes each from their merge, and list both in the other two evals. Each project's
// items' minutes are the samples its day rows of the day hold, and each list holds as many items as
// its eval's n counts.
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
	shipped("OMEGA", app+"#1", omega[app+"#1"], nil, at(9, 30), map[string]float64{"merge_to_production_minutes": 30})
	for _, column := range []string{"open_to_production_minutes", "first_commit_to_production_minutes"} {
		if minutes, held := omega[app+"#1"][column]; !held || minutes != nil {
			t.Errorf("OMEGA's item %s#1 holds %s %v (held %t), want none: a posted deploy times merge-to-production alone", app, column, minutes, held)
		}
	}
	if cut := items("OMEGA", app); !reflect.DeepEqual(cut, omega) {
		t.Errorf("OMEGA's merge-to-production items of %s are %v, want those of every repository, %v", app, cut, omega)
	}
	alpha := items("ALPHA", "")
	if len(alpha) != 2 {
		t.Errorf("ALPHA's merge-to-production items list %v, want %s#1 and #2", slices.Sorted(maps.Keys(alpha)), app)
	}
	for _, number := range []string{"#1", "#2"} {
		shipped("ALPHA", app+number, alpha[app+number], 101.0, at(10, 29), map[string]float64{"merge_to_production_minutes": 89})
	}
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
	for _, slug := range []string{evals.SlugFirstCommitToProduction, evals.SlugOpenToProduction} {
		for project, want := range map[string][]string{"OMEGA": {}, "ALPHA": {app + "#1", app + "#2"}} {
			if listed, n := pullRequestItemsAndN(t, handler, project, slug); !slices.Equal(listed, want) || n != len(want) {
				t.Errorf("%s's %s items list %v against n %d, want %v and n %d", project, slug, listed, n, want, len(want))
			}
		}
	}
}

// TestAPullRequestItemTimesEachLeadTimeToTheDeployItsEvalCounts
// (dispatch://INFRA-978/comment/ed4092f8-cdb6-4d99-b37c-fbb56755cc57, point 3): OMEGA's GitHub
// source reads its default repository acme/omega-app beside an events source. acme/omega-app#1, of
// OMEGA's issue, is first committed at 07:30, opened at 08:00 and merged at 09:00; a deploy posted
// to OMEGA names it and finished at 09:30, before OMEGA's GitHub deploy 101 ships it at 10:29.
// Its item times merge-to-production to the post (30 minutes), and first-commit-to-production
// (179) and open-to-production (149) to run 101, in every one of the three evals' items, each item
// naming as its deploy the one its own eval counts: the post, with no run id, in
// merge-to-production's, and run 101 in the other two's. Each eval's read counts #1 once, at its
// item's minutes: acme/omega-app's merge-to-production row times it at 89 minutes too, which the
// read leaves out for the post's (dispatch://INFRA-978/comment/66926e49-2f97-4c05-8eb0-d62d1f31fee8).
func TestAPullRequestItemTimesEachLeadTimeToTheDeployItsEvalCounts(t *testing.T) {
	t.Parallel()
	const app = "acme/omega-app"
	handler, database, deps := newTestServer(t, testServerOptions{})
	issue := createTestIssue(t, handler, "OMEGA", "One change to the app")
	setDefaultRepository(t, handler, "OMEGA", app)
	saveProjectDelivery(t, database.Pool, delivery.ProjectSettingsInput{
		Project: "OMEGA", Delivery: true,
		Sources:           []delivery.SourceInput{deliveryseed.SourceOf(deployRule(app, "release"), nil), {Kind: delivery.SourceKindEvents}},
		PopulationAuthors: []string{"octocat"}, ExcludedRepos: []string{},
	})
	upsertPR(t, database, delivery.DeliveryPullRequest{
		Repo: app, Number: 1, Title: "feat: " + issue, Author: "octocat", IssueKey: &issue, URL: "https://github.com/" + app + "/pull/1",
		FirstCommitAt: new(sourcesDayAt(7, 30)), CreatedAt: new(sourcesDayAt(8, 0)), MergedAt: new(sourcesDayAt(9, 0)),
	})
	seedSourceRun(t, database, app, 101, 10, "release")
	writeProductionEvent(t, database.Pool, prodevents.Event{
		ProjectKey: "OMEGA", Kind: prodevents.KindDeploy, Source: prodevents.SourceAPI, SourceRef: "release-1",
		FinishedAt: new(sourcesDayAt(9, 30)), PullRequests: []string{app + "#1"},
	})
	leaveAttributionInputsUnread(t, database.Pool)
	next := sourcesDay.AddDate(0, 0, 1)
	refreshEvalDays(t, evals.New(database, deps.Docs, deps.Events, deps.Attributor), database, next.Add(-time.Minute))

	minutes := map[string]float64{
		"merge_to_production_minutes": 30, "first_commit_to_production_minutes": 179, "open_to_production_minutes": 149,
	}
	for _, want := range []struct {
		slug, column string
		finished     time.Time
		run          any
	}{
		{evals.SlugMergeToProduction, "merge_to_production_minutes", sourcesDayAt(9, 30), nil},
		{evals.SlugFirstCommitToProduction, "first_commit_to_production_minutes", sourcesDayAt(10, 29), 101.0},
		{evals.SlugOpenToProduction, "open_to_production_minutes", sourcesDayAt(10, 29), 101.0},
	} {
		query := url.Values{"from": {sourcesDay.Format(time.RFC3339)}, "to": {next.Format(time.RFC3339)}, "row_kind": {evalRowPullRequest}}
		answer := evalGet[evalItemsResponse](t, handler, evalItemsPath("OMEGA", want.slug)+"?"+query.Encode())
		if len(answer.Items) != 1 || answer.Items[0].RowKey != app+"#1" {
			keys := make([]string, len(answer.Items))
			for i, row := range answer.Items {
				keys[i] = row.RowKey
			}
			t.Errorf("OMEGA's %s items list %v, want %s#1 alone", want.slug, keys, app)
			continue
		}
		row := answer.Items[0].Cols
		deployed, _ := row["deployed_at"].(string)
		if when, err := time.Parse(time.RFC3339, deployed); err != nil || !when.Equal(want.finished) || row["deploy_run_id"] != want.run {
			t.Errorf("OMEGA's %s item was deployed at %v by run %v, want at %s by %v", want.slug, row["deployed_at"], row["deploy_run_id"],
				want.finished.Format(time.RFC3339), want.run)
		}
		for _, column := range slices.Sorted(maps.Keys(minutes)) {
			if row[column] != minutes[column] {
				t.Errorf("OMEGA's %s item holds %s %v, want %v", want.slug, column, row[column], minutes[column])
			}
		}
		minutesOf := leadTimeRead(t, handler, "OMEGA", want.slug)
		if n, most := minutesOf.Window["n"], minutesOf.Window["max"]; n == nil || *n != 1 || most == nil || *most != minutes[want.column] {
			t.Errorf("OMEGA's %s reads n %s and max %s, want 1 and its item's %v", want.slug, showFloat(n), showFloat(most), minutes[want.column])
		}
	}
}

// TestALeadTimesItemsListNoPullRequestThatOnlyAPostShips
// (dispatch://INFRA-978/comment/ed4092f8-cdb6-4d99-b37c-fbb56755cc57, point 3): OMEGA's GitHub
// source reads its default repository acme/omega-app beside an events source. acme/omega-app#1, of
// OMEGA's issue, and acme/alpha-app#1, of ALPHA's, are first committed at 07:30, opened at 08:00 and
// merged at 09:00; a deploy posted to OMEGA names both and finished at 09:30; OMEGA's GitHub deploy
// 201 ships acme/omega-app#1 at 10:29, and ALPHA's 101 ships acme/alpha-app#1 at 10:29 for ALPHA
// alone. OMEGA's merge-to-production counts both, each at the post, and lists both. Its
// first-commit-to-production and open-to-production count acme/omega-app#1 alone, at 201, and list
// it alone: no GitHub deploy counts acme/alpha-app#1 for OMEGA, and its events repository's member
// of it counts in merge-to-production alone. Each list holds as many items as its eval's n. Red
// where those two lists name acme/alpha-app#1 through that member: two items against n 1.
func TestALeadTimesItemsListNoPullRequestThatOnlyAPostShips(t *testing.T) {
	t.Parallel()
	const omegaApp, alphaApp = "acme/omega-app", "acme/alpha-app"
	handler, database, deps := newTestServer(t, testServerOptions{})
	issues := map[string]string{
		alphaApp: createTestIssue(t, handler, "ALPHA", "A change to the app"),
		omegaApp: createTestIssue(t, handler, "OMEGA", "A change to the app"),
	}
	saveProjectRules(t, database, "ALPHA", deployRule(alphaApp, "release"))
	setDefaultRepository(t, handler, "OMEGA", omegaApp)
	saveProjectDelivery(t, database.Pool, delivery.ProjectSettingsInput{
		Project: "OMEGA", Delivery: true,
		Sources:           []delivery.SourceInput{deliveryseed.SourceOf(deployRule(omegaApp, "release"), nil), {Kind: delivery.SourceKindEvents}},
		PopulationAuthors: []string{"octocat"}, ExcludedRepos: []string{},
	})
	for repo, issue := range issues {
		upsertPR(t, database, delivery.DeliveryPullRequest{
			Repo: repo, Number: 1, Title: "feat: " + issue, Author: "octocat", IssueKey: &issue, URL: "https://github.com/" + repo + "/pull/1",
			FirstCommitAt: new(sourcesDayAt(7, 30)), CreatedAt: new(sourcesDayAt(8, 0)), MergedAt: new(sourcesDayAt(9, 0)),
		})
	}
	seedSourceRun(t, database, alphaApp, 101, 10, "release")
	seedSourceRun(t, database, omegaApp, 201, 10, "release")
	writeProductionEvent(t, database.Pool, prodevents.Event{
		ProjectKey: "OMEGA", Kind: prodevents.KindDeploy, Source: prodevents.SourceAPI, SourceRef: "release-1",
		FinishedAt: new(sourcesDayAt(9, 30)), PullRequests: []string{omegaApp + "#1", alphaApp + "#1"},
	})
	leaveAttributionInputsUnread(t, database.Pool)
	refreshEvalDays(t, evals.New(database, deps.Docs, deps.Events, deps.Attributor), database, sourcesDay.AddDate(0, 0, 1).Add(-time.Minute))

	for _, slug := range []string{evals.SlugMergeToProduction, evals.SlugFirstCommitToProduction, evals.SlugOpenToProduction} {
		want := []string{omegaApp + "#1"}
		if slug == evals.SlugMergeToProduction {
			want = []string{alphaApp + "#1", omegaApp + "#1"}
		}
		if listed, n := pullRequestItemsAndN(t, handler, "OMEGA", slug); !slices.Equal(listed, want) || n != len(want) {
			t.Errorf("OMEGA's %s items list %v against n %d, want %v and n %d", slug, listed, n, want, len(want))
		}
	}
}

// pullRequestItemsAndN is project's pull request items of slug over sourcesDay, by row key in
// order, and the n its eval's minutes series states over the same day, 0 where it states none.
func pullRequestItemsAndN(t *testing.T, handler http.Handler, project, slug string) ([]string, int) {
	t.Helper()
	query := url.Values{"from": {sourcesDay.Format(time.RFC3339)}, "to": {sourcesDay.AddDate(0, 0, 1).Format(time.RFC3339)}, "row_kind": {evalRowPullRequest}}
	answer := evalGet[evalItemsResponse](t, handler, evalItemsPath(project, slug)+"?"+query.Encode())
	keys := make([]string, len(answer.Items))
	for i, row := range answer.Items {
		keys[i] = row.RowKey
	}
	slices.Sort(keys)
	n := 0
	if held := leadTimeRead(t, handler, project, slug).Window["n"]; held != nil {
		n = int(*held)
	}
	return keys, n
}

// leadTimeRead is project's slug over sourcesDay as its eval list states it, its minutes series;
// a series of no statistic where the eval states none.
func leadTimeRead(t *testing.T, handler http.Handler, project, slug string) evals.SeriesValues {
	t.Helper()
	window := url.Values{"from": {sourcesDay.Format(time.RFC3339)}, "to": {sourcesDay.AddDate(0, 0, 1).Format(time.RFC3339)}}
	list := evalGet[evalListResponse](t, handler, "/api/v1/projects/"+project+"/evals?limit=200&"+window.Encode())
	for _, series := range findEval(t, list.Items, project, slug).Series {
		if series.Series.Series == measures.SeriesMinutes {
			return series
		}
	}
	return evals.SeriesValues{}
}

// showFloat is a statistic as a failure names it: its value, or "none".
func showFloat(value *float64) string {
	if value == nil {
		return "none"
	}
	return strconv.FormatFloat(*value, 'g', -1, 64)
}

// TestAPullRequestAnOwnerKeyAndAPostBothTimeIsOneItem
// (dispatch://INFRA-978/comment/9cdcf9d2-532d-4431-9332-3ced4d4329f2): acme/omega-app is OMEGA's
// default repository, which OMEGA's GitHub source reads, beside an events source. #1 and #2 of it,
// of no issue, merge at 09:00, so each counts for OMEGA through the repository; its GitHub deploy
// 101 ships both at 10:29, and deploys posted to OMEGA name #1, finished at 09:30, and #2, finished
// at 11:00 (postsHistory). OMEGA's merge-to-production counts each once, from the deploy that
// finished first, and its items list each once, timed to that deploy (expectEachOnce).
func TestAPullRequestAnOwnerKeyAndAPostBothTimeIsOneItem(t *testing.T) {
	t.Parallel()
	const app = "acme/omega-app"
	handler, _, _ := postsHistory(t, app, false)
	expectEachOnce(t, "after the posts", handler, app, 1)
}

// TestAPullRequestAnOwnerKeyAndAPostBothTimeIsOneItemOnceItsRepositoryIsRenamed
// (dispatch://INFRA-978/comment/b4eecb12-e9e5-4f37-978d-e2155c36c6d7): the end of a rename's
// transient. Over the history of TestAPullRequestAnOwnerKeyAndAPostBothTimeIsOneItem, the boot's
// rename pass (store.RewriteRenamedRepositories) renames acme/omega-app to acme/omega-next: the
// pull requests, the run, the posted deploys' pull requests, OMEGA's default repository and the
// day rows' owner keys and deploy repositories, and not the pull request of each day row's
// samples (eval_days.sample_keys), which the owner key's row and OMEGA's events repository's row
// hold under the old name. The refresher's passes after it build the rows the pass's writes name
// again, both of them, so no row's keys name the old name and each row times acme/omega-next#1 and
// #2: OMEGA's merge-to-production counts each once again, from the deploy that finished first, and
// its items list each once, timed to that deploy (expectOnceThroughARename). Red where the
// refresher builds one of the two rows again and leaves the other's keys under the old name: the
// two no longer name one pull request, so both its samples count (n 4 and max 120), and an item
// misses the deploy that row counts.
func TestAPullRequestAnOwnerKeyAndAPostBothTimeIsOneItemOnceItsRepositoryIsRenamed(t *testing.T) {
	t.Parallel()
	expectOnceThroughARename(t, false, 1)
}

// TestAPullRequestItsProjectsGitHubRowAndAPostBothTimeIsOneItemOnceItsRepositoryIsRenamed
// (dispatch://INFRA-978/comment/66926e49-2f97-4c05-8eb0-d62d1f31fee8): the rename of
// TestAPullRequestAnOwnerKeyAndAPostBothTimeIsOneItemOnceItsRepositoryIsRenamed, where #1 and #2 are
// of OMEGA's issue, so OMEGA's own row of acme/omega-app times them beside its events repository's
// row, each row by its own deploys, both naming their samples' pull requests: OMEGA's
// merge-to-production counts each once before the rename and after the refresher's passes that
// follow it, from the deploy that finished first, none through a repository, its other two lead
// times count both at run 101, and every list holds as many items as its number's n.
func TestAPullRequestItsProjectsGitHubRowAndAPostBothTimeIsOneItemOnceItsRepositoryIsRenamed(t *testing.T) {
	t.Parallel()
	expectOnceThroughARename(t, true, 0)
}

// expectOnceThroughARename fails unless, over postsHistory(app, issued), OMEGA counts #1 and #2
// once each (expectEachOnce, via of them through a repository) before the boot's rename pass
// renames acme/omega-app to acme/omega-next; unless right after the pass the two
// merge-to-production rows that time #1, acme/omega-app's and OMEGA's events repository's, name it
// under the old name; and unless, once the refresher's passes after it build those rows again, no
// row names the old name, both name acme/omega-next's, and OMEGA counts each once again.
func expectOnceThroughARename(t *testing.T, issued bool, via int) {
	t.Helper()
	const app, renamed = "acme/omega-app", "acme/omega-next"
	handler, database, refresh := postsHistory(t, app, issued)
	expectEachOnce(t, "before the rename", handler, app, via)
	ctx, pool := t.Context(), database.Pool
	if _, err := pool.Exec(ctx, `insert into repo_projects (repo, project, created_by, renamed_to)
		values ($1, 'OMEGA', '{"kind":"user","id":"alice"}', $2)`, app, renamed); err != nil {
		t.Fatalf("declare the rename of %s: %v", app, err)
	}
	pairs, err := store.ReadRepoRenames(ctx, pool)
	if err != nil {
		t.Fatalf("read the rename pairs: %v", err)
	}
	if pass := store.RewriteRenamedRepositories(ctx, pool, pairs); pass.Err() != nil || pass.Failures != 0 {
		t.Fatalf("the boot's rename pass = %+v (%v), want no failure and nothing left", pass, pass.Err())
	}
	// keyed is how many merge-to-production rows of the day name pr among their samples' keys.
	keyed := func(pr string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `
			select count(*) from eval_days where eval_id like '%/' || $1 and day = $2 and repo = '' and $3 = any(sample_keys)
		`, evals.SlugMergeToProduction, sourcesDay, pr).Scan(&n); err != nil {
			t.Fatalf("read the rows that time %s: %v", pr, err)
		}
		return n
	}
	if n := keyed(app + "#1"); n != 2 {
		t.Fatalf("right after the pass %d merge-to-production rows time %s#1, want %s's and OMEGA's events repository's under the old name", n, app, app)
	}
	refresh()
	for _, number := range []string{"#1", "#2"} {
		if n := keyed(app + number); n != 0 {
			t.Errorf("after the refresher's passes %d merge-to-production rows time %s%s under the old name, want none", n, app, number)
		}
		if n := keyed(renamed + number); n != 2 {
			t.Errorf("after the refresher's passes %d merge-to-production rows time %s%s, want %s's and OMEGA's events repository's", n, renamed, number, renamed)
		}
	}
	expectEachOnce(t, "after the rename", handler, renamed, via)
}

// sourcesDayAt is hour:minute of sourcesDay.
func sourcesDayAt(hour, minute int) time.Time {
	return sourcesDay.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
}

// postsHistory is a server holding OMEGA, whose default repository app its GitHub source reads
// beside an events source; #1 and #2 of app, first committed at 07:30, opened at 08:00 and merged
// at 09:00 on sourcesDay, of OMEGA's issue where issued is set, and so counted in OMEGA's own rows,
// and of no issue otherwise, and so counted for OMEGA through app's plain owner key; app's GitHub
// deploy 101, which ships both at 10:29; and deploys posted to OMEGA that name #1, finished at
// 09:30, and #2, finished at 11:00. It builds the day rows (refreshEvalDays) and answers the
// server, its store and the refresh, as of the end of sourcesDay, which builds again the days a
// later write names.
func postsHistory(t *testing.T, app string, issued bool) (http.Handler, *store.Store, func()) {
	t.Helper()
	handler, database, deps := newTestServer(t, testServerOptions{})
	var issue *string
	if issued {
		issue = new(createTestIssue(t, handler, "OMEGA", "Two changes to the app"))
	} else {
		createTestProject(t, handler, "OMEGA")
	}
	setDefaultRepository(t, handler, "OMEGA", app)
	saveProjectDelivery(t, database.Pool, delivery.ProjectSettingsInput{
		Project: "OMEGA", Delivery: true,
		Sources:           []delivery.SourceInput{deliveryseed.SourceOf(deployRule(app, "release"), nil), {Kind: delivery.SourceKindEvents}},
		PopulationAuthors: []string{"octocat"}, ExcludedRepos: []string{},
	})
	posts := map[int]time.Time{1: sourcesDayAt(9, 30), 2: sourcesDayAt(11, 0)}
	for number, finished := range posts {
		upsertPR(t, database, delivery.DeliveryPullRequest{
			Repo: app, Number: number, Title: "Change " + strconv.Itoa(number), Author: "octocat", IssueKey: issue,
			URL:           "https://github.com/" + app + "/pull/" + strconv.Itoa(number),
			FirstCommitAt: new(sourcesDayAt(7, 30)), CreatedAt: new(sourcesDayAt(8, 0)), MergedAt: new(sourcesDayAt(9, 0)),
		})
		writeProductionEvent(t, database.Pool, prodevents.Event{
			ProjectKey: "OMEGA", Kind: prodevents.KindDeploy, Source: prodevents.SourceAPI, SourceRef: "release-" + strconv.Itoa(number),
			FinishedAt: new(finished), PullRequests: []string{app + "#" + strconv.Itoa(number)},
		})
	}
	seedSourceRun(t, database, app, 101, 10, "release")
	leaveAttributionInputsUnread(t, database.Pool)
	registry := evals.New(database, deps.Docs, deps.Events, deps.Attributor)
	refresh := func() {
		t.Helper()
		refreshEvalDays(t, registry, database, sourcesDay.AddDate(0, 0, 1).Add(-time.Minute))
	}
	refresh()
	return handler, database, refresh
}

// expectEachOnce fails unless OMEGA's merge-to-production over sourcesDay, on handler holding
// postsHistory's history with its repository named repo, counts #1 and #2 of repo once each, from
// the deploy that finished first: n 2 and max 89, via of the two through the repository; unless
// its items list each once, timed to that deploy: #1 to its post, with no run id, 30 minutes, and
// #2 to run 101, 89 minutes, the one set the number folds; and unless its first-commit-to-production
// and open-to-production, which run 101 times alone, count both and list both.
func expectEachOnce(t *testing.T, what string, handler http.Handler, repo string, via int) {
	t.Helper()
	window := url.Values{"from": {sourcesDay.Format(time.RFC3339)}, "to": {sourcesDay.AddDate(0, 0, 1).Format(time.RFC3339)}}
	list := evalGet[evalListResponse](t, handler, "/api/v1/projects/OMEGA/evals?limit=200&"+window.Encode())
	minutes := seriesValues(t, findEval(t, list.Items, "OMEGA", evals.SlugMergeToProduction), measures.SeriesMinutes)
	for statistic, want := range map[string]float64{"n": 2, "max": 89} {
		if got := minutes.Window[statistic]; got == nil || *got != want {
			t.Errorf("%s: OMEGA's merge-to-production states %s %v, want %v", what, statistic, got, want)
		}
	}
	if minutes.ViaRepository != via {
		t.Errorf("%s: OMEGA's merge-to-production counts %d pull requests through a repository, want %d", what, minutes.ViaRepository, via)
	}

	query := url.Values{"from": window["from"], "to": window["to"], "row_kind": {evalRowPullRequest}}
	answer := evalGet[evalItemsResponse](t, handler, evalItemsPath("OMEGA", evals.SlugMergeToProduction)+"?"+query.Encode())
	rows := map[string]map[string]any{}
	for _, row := range answer.Items {
		if _, twice := rows[row.RowKey]; twice {
			t.Errorf("%s: OMEGA's merge-to-production items list %s twice, want once", what, row.RowKey)
		}
		rows[row.RowKey] = row.Cols
	}
	if len(rows) != 2 {
		t.Errorf("%s: OMEGA's merge-to-production items list %v, want %s#1 and #2", what, slices.Sorted(maps.Keys(rows)), repo)
	}
	for _, want := range []struct {
		key      string
		finished time.Time
		run      any
		minutes  float64
	}{{repo + "#1", sourcesDayAt(9, 30), nil, 30}, {repo + "#2", sourcesDayAt(10, 29), 101.0, 89}} {
		row := rows[want.key]
		deployed, _ := row["deployed_at"].(string)
		when, err := time.Parse(time.RFC3339, deployed)
		if row == nil || err != nil || !when.Equal(want.finished) || row["deploy_run_id"] != want.run || row["merge_to_production_minutes"] != want.minutes {
			t.Errorf("%s: OMEGA's item %s is %v, want deployed at %s by run %v, %v minutes from its merge", what, want.key, row,
				want.finished.Format(time.RFC3339), want.run, want.minutes)
		}
	}
	both := []string{repo + "#1", repo + "#2"}
	for _, slug := range []string{evals.SlugFirstCommitToProduction, evals.SlugOpenToProduction} {
		if listed, n := pullRequestItemsAndN(t, handler, "OMEGA", slug); !slices.Equal(listed, both) || n != len(both) {
			t.Errorf("%s: OMEGA's %s items list %v against n %d, want %v and n %d", what, slug, listed, n, both, len(both))
		}
	}
}
