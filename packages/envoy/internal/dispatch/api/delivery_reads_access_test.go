package api

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The delivery reads of verification v16's seed (private_seed_test.go): which pull requests, runs,
// counts and groups each caller reads once PRIV is private (GH-1, check c-delivery-by-issue). A
// pull request is shown when the caller sees its issue, one that names no issue to an open viewer
// only; a run is shown with only the pull requests shown, and is left out when it shipped some and
// none of them is shown, or, for a viewer that is not open, when it shipped none; every count,
// facet, measure and title is computed from what is shown.

// The seed's day, the Pipeline page's at, and the days the timeline and the measures are read over:
// the Pipeline page's seven shown days.
var (
	deliveryReadsAt   = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	deliveryReadsFrom = time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	deliveryReadsTo   = deliveryReadsFrom.AddDate(0, 0, 7)
)

// The seed's deploy runs and PR-checks runs, by id: R1, R2 and R3 deploy runs on main, C2 a
// PR-checks run of P2 and C9 one of a pull request no stored row holds.
const (
	deliveryRunR1 int64 = 501
	deliveryRunR2 int64 = 502
	deliveryRunR3 int64 = 503
	deliveryRunC2 int64 = 602
	deliveryRunC9 int64 = 609
)

// The seed's sessions and their groups: S1, P1's first session, acts in PUB and is G1's; S2, P2's
// first session, acts in PRIV and is G2's.
const (
	deliverySessionS1 = "session-s1"
	deliverySessionS2 = "session-s2"
	deliveryGroupG1   = "11111111-1111-4111-8111-111111111111"
	deliveryGroupG2   = "22222222-2222-4222-8222-222222222222"
)

// seedDeliveryReads stands up verification v16's seed with the delivery rows of
// TestADeliveryReadShowsOnlyThePullRequestsItsViewerSees, the rows of PUB when public is set and
// those of PRIV when private is: P1 (PUB-1, first session S1), P3 (no issue, in acme/other) and
// the unowned P0 PUB-2, S1 and G1 are PUB's; P2 and P5 (PRIV-1, P2's first session S2), P4 (PRIV-3,
// merged before the first shown day), R2 (shipping P5), R3 (shipping P4, inside the first shown
// day), C2 (P2's PR-checks run, failed), the unowned P0 PRIV-2, S2 and G2 are PRIV's. R1, which
// ships P1 and P2 whichever of them is written, and C9 are in every seed, and so are the components
// K1 of PUB, attached to PUB-1, and K2 of PRIV, attached to PRIV-1.
//
// The plan's seed has R2 ship P2, which R1 ships too. A pull request ships with one run, the
// earliest to complete of those GitHub created after its merge (delivery.FirstShippingApply), so R1
// ships P2 and R2 could ship nothing of it; P5, a second pull request of PRIV-1, is R2's.
func seedDeliveryReads(t *testing.T, public, private bool) privateSeed {
	t.Helper()
	return seedDeliveryReadsWith(t, testServerOptions{}, public, private)
}

// seedDeliveryReadsWith is seedDeliveryReads on a server built with options, a traced store's.
func seedDeliveryReadsWith(t *testing.T, options testServerOptions, public, private bool) privateSeed {
	t.Helper()
	seed := seedPrivateProjectWith(t, options)
	ctx, pool := t.Context(), seed.database.Pool
	if _, err := delivery.PutSettings(ctx, pool, delivery.DeliverySettings{
		DeployRepo: "acme/widgets", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobNames: []string{"release / release"}, PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: []string{"octocat"},
	}, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("write the delivery settings: %v", err)
	}
	for _, statement := range []string{
		`insert into architecture_sources (project_key, id, repo, branch, installation_id, created_by) values
			('PUB', 1, 'acme/pub-arch', 'main', 1, '{"kind":"system","id":"test"}'),
			('PRIV', 1, 'acme/priv-arch', 'main', 1, '{"kind":"system","id":"test"}')`,
		`insert into architecture_snapshots (id, project_key, commit, files) values (910, 'PUB', 'p1', '{}'), (911, 'PRIV', 'q1', '{}')`,
		`insert into components (project_key, id, title, prose, parent, snapshot_id, source_id) values
			('PUB', 'k1', 'Component one', '', null, 910, 1),
			('PRIV', 'k2', 'Component two', '', null, 911, 1)`,
		`insert into issue_components (issue_key, mode, reason) values ('PUB-1', 'explicit', null), ('PRIV-1', 'explicit', null)`,
		`insert into issue_component_members (issue_key, project_key, component_id) values ('PUB-1', 'PUB', 'k1'), ('PRIV-1', 'PRIV', 'k2')`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("write the components: %v\n%s", err, statement)
		}
	}

	issue := func(project, title string, priority *int, want string) {
		t.Helper()
		body := map[string]any{"project": project, "title": title, "force": true}
		if priority != nil {
			body["priority"] = *priority
		}
		created := decodeBody[struct {
			Key string `json:"key"`
		}](t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues", body, http.StatusCreated))
		if created.Key != want {
			t.Fatalf("carol creates %q as %s, want %s", title, created.Key, want)
		}
	}
	p0 := 0
	group := func(id, name, session string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			insert into delivery_session_groups (id, name, created_by, created_at, updated_by, updated_at)
			values ($1, $2, '{"kind":"user","id":"carol@example.com"}', '2026-09-20T00:00:00Z', '{"kind":"user","id":"carol@example.com"}', '2026-09-20T00:00:00Z')
		`, id, name); err != nil {
			t.Fatalf("write group %s: %v", name, err)
		}
		if _, err := pool.Exec(ctx, `insert into delivery_session_group_members (session_id, group_id) values ($1, $2)`, session, id); err != nil {
			t.Fatalf("put %s in group %s: %v", session, name, err)
		}
	}
	pr := func(repo string, number int, issueKey string, sessions []string, merged time.Time, unfetchable bool) {
		t.Helper()
		opened := merged.Add(-2 * time.Hour)
		row := delivery.DeliveryPullRequest{
			Repo: repo, Number: number, Title: "change " + strconv.Itoa(number), Author: "octocat",
			URL:       fmt.Sprintf("https://github.com/%s/pull/%d", repo, number),
			CreatedAt: &opened, MergedAt: &merged, Sessions: sessions,
		}
		if issueKey != "" {
			row.IssueKey = &issueKey
		}
		upsertPR(t, seed.database, row)
		if unfetchable {
			if err := delivery.MarkPullRequestUnfetchable(ctx, pool, repo, number, "the pull request is gone"); err != nil {
				t.Fatalf("mark %s#%d unfetchable: %v", repo, number, err)
			}
		}
	}
	deploy := func(id int64, started time.Time) {
		t.Helper()
		seedPipelineRun(t, pool, delivery.DeliveryRun{
			RunID: id, Kind: delivery.DeliveryRunKindDeploy, StartedAt: started,
			CompletedAt: new(started.Add(40 * time.Minute)), Conclusion: new(delivery.DeliveryRunConclusionSuccess),
			HeadBranch: new("main"), Event: new("push"),
		}, []pipelineFixtureJob{{name: "release / release", conclusion: "success", start: started, end: started.Add(30 * time.Minute)}})
	}
	checks := func(id int64, number int, started time.Time, conclusion string) {
		t.Helper()
		seedPipelineRun(t, pool, delivery.DeliveryRun{
			RunID: id, Kind: delivery.DeliveryRunKindPRChecks, PRNumber: new(number), StartedAt: started,
			CompletedAt: new(started.Add(20 * time.Minute)), Conclusion: new(delivery.DeliveryRunConclusion(conclusion)),
			Event: new("pull_request"),
		}, []pipelineFixtureJob{{name: "build / unit", conclusion: conclusion, start: started, end: started.Add(15 * time.Minute)}})
	}
	day := func(month time.Month, date, hour int) time.Time {
		return time.Date(2026, month, date, hour, 0, 0, 0, time.UTC)
	}

	if public {
		issue(seedPublic, "Unowned P0 of PUB", &p0, "PUB-2")
		seed.asSession(t, deliverySessionS1, seedPublic, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPublic]+"/comments", map[string]any{"body": "S1 on PUB."}, http.StatusCreated)
		group(deliveryGroupG1, "Group one", deliverySessionS1)
		pr("acme/widgets", 1, seed.issue[seedPublic], []string{deliverySessionS1}, day(10, 1, 1), false)
		pr("acme/other", 3, "", nil, day(10, 1, 6), true)
	}
	if private {
		issue(seedPrivate, "Unowned P0 of PRIV", &p0, "PRIV-2")
		issue(seedPrivate, "Shipped before the window", nil, "PRIV-3")
		seed.asSession(t, deliverySessionS2, seedPrivate, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPrivate]+"/comments", map[string]any{"body": "S2 on PRIV."}, http.StatusCreated)
		group(deliveryGroupG2, "Group two", deliverySessionS2)
		pr("acme/widgets", 2, seed.issue[seedPrivate], []string{deliverySessionS2}, day(10, 1, 2), false)
		pr("acme/widgets", 5, seed.issue[seedPrivate], nil, day(10, 1, 4), true)
		// P4 merges the evening before the first shown day, and R3, the first deploy after it,
		// ships it an hour into that day.
		pr("acme/widgets", 4, "PRIV-3", nil, day(9, 25, 20), false)
		deploy(deliveryRunR3, day(9, 26, 1))
		deploy(deliveryRunR2, day(10, 1, 5))
		checks(deliveryRunC2, 2, day(10, 2, 8), "failure")
	}
	deploy(deliveryRunR1, day(10, 1, 3))
	checks(deliveryRunC9, 99, day(10, 2, 9), "success")
	return seed
}

// deliveryReadsTargets are the three reads the test compares, by name.
var deliveryReadsTargets = []struct{ name, target string }{
	{"timeline", "/api/v1/delivery/timeline?from=" + deliveryReadsFrom.Format(time.RFC3339) + "&to=" + deliveryReadsTo.Format(time.RFC3339)},
	{"measures", "/api/v1/delivery/measures?from=" + deliveryReadsFrom.Format(time.RFC3339) + "&to=" + deliveryReadsTo.Format(time.RFC3339)},
	{"pipeline", "/api/v1/delivery/pipeline?at=" + deliveryReadsAt.Format(time.RFC3339)},
}

// maskedDeliveryRead is one delivery read's answer as a field map, with computed_at and freshness,
// the two fields two seeds may answer apart, taken out, at the top and in the timeline's measures.
func maskedDeliveryRead(t *testing.T, seed privateSeed, caller privateCaller, target string) map[string]any {
	t.Helper()
	return maskedDeliveryBody(t, seed.mustRequest(t, caller, http.MethodGet, target, nil, http.StatusOK))
}

// maskedDeliveryBody is a delivery answer as maskedDeliveryRead masks it.
func maskedDeliveryBody(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	body := decodeBody[map[string]any](t, response)
	delete(body, "computed_at")
	delete(body, "freshness")
	if measured, ok := body["measures"].(map[string]any); ok {
		delete(measured, "computed_at")
		delete(measured, "freshness")
	}
	return body
}

// deliveryFieldsApart names every top-level field two answers hold apart, with both values.
func deliveryFieldsApart(got, want map[string]any) []string {
	fields := map[string]bool{}
	for field := range got {
		fields[field] = true
	}
	for field := range want {
		fields[field] = true
	}
	apart := []string{}
	for _, field := range slices.Sorted(maps.Keys(fields)) {
		if !reflect.DeepEqual(got[field], want[field]) {
			apart = append(apart, fmt.Sprintf("%s: got %s, want %s", field, mustJSONValue(got[field]), mustJSONValue(want[field])))
		}
	}
	return apart
}

func mustJSONValue(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// shownDelivery is what a caller's timeline and measures hold that the test names: the pull
// requests, each run with the pull requests it lists, when each waiting pull request shipped, the
// components, the session groups by name, the unowned P0 issues and the unfetchable count.
type shownDelivery struct {
	prs         []string
	runs        map[int64][]string
	waiting     []string
	components  []string
	groups      []string
	unowned     []string
	unfetchable int
}

func readShownDelivery(t *testing.T, seed privateSeed, caller privateCaller) shownDelivery {
	t.Helper()
	timeline := decodeBody[deliveryTimelineWithMeasures](t, seed.mustRequest(t, caller, http.MethodGet, deliveryReadsTargets[0].target, nil, http.StatusOK))
	shown := shownDelivery{
		prs: deliveryPRIDs(timeline.PRs), runs: map[int64][]string{}, waiting: []string{},
		components: slices.Sorted(maps.Keys(timeline.Components)), groups: []string{}, unowned: []string{},
		unfetchable: timeline.Freshness.UnfetchableCount,
	}
	for _, run := range timeline.Runs {
		ids := []string{}
		for _, pr := range run.PRs {
			ids = append(ids, pr.ID)
		}
		shown.runs[run.ID] = ids
	}
	for _, waiting := range timeline.Waiting {
		shipped := "waiting"
		if waiting.DeployedAt != nil {
			shipped = waiting.DeployedAt.UTC().Format(time.RFC3339)
		}
		shown.waiting = append(shown.waiting, waiting.MergedAt.UTC().Format(time.RFC3339)+" shipped "+shipped)
	}
	for _, group := range timeline.SessionGroups {
		shown.groups = append(shown.groups, group.Name)
	}
	for _, issue := range timeline.Measures.UnownedP0 {
		shown.unowned = append(shown.unowned, issue.Key)
	}
	return shown
}

// TestADeliveryReadShowsOnlyThePullRequestsItsViewerSees (check c-delivery-by-issue): the delivery
// timeline, the measures and the Pipeline page show each caller only the pull requests whose issue
// it sees, and those that name no issue to an open viewer alone; a run is shown with the pull
// requests shown and left out when it shipped some and none of them is shown, and every count,
// facet, measure, title, session group and component comes from what is shown. The seed is
// seedDeliveryReads'.
//
// For alice, who sees PUB and not PRIV, each of the three reads equals, field for field with
// computed_at and the freshness masked, the answer of a seed where PRIV's rows were never written,
// so her production_apply counts R1 alone and her pr_checks C9 alone. bob and bob's token that
// lists PRIV read the timeline and the measures of a seed where PUB's rows were never written: P2
// and P5, R1 with P2 only, R2, and P4 waiting with R3 shipping it, the component K2 and the group
// G2. Neither may read the Pipeline page or a run (403 DEPLOYMENT_READ_REFUSED). carol at her
// browser, and carol's token that lists PRIV, a full member's, whose list decides only which
// private projects it sees, read all of it. A run's own read answers a run the caller is not shown
// exactly as an id no run holds: alice's read of R2, of R3 and of C2.
//
// Controls, each run on its own, each failing the test naming what it moved: answer every group
// (alice's session_groups hold G2); filter the pull requests after the counts are made (alice's
// facet_counts count P2); compute the Pipeline page's job timings from every deploy run (her
// production_apply counts R2); compute pr_checks from every PR-checks run (her pr_checks holds C2's
// failing job); answer every run's detail (she reads R2's jobs); filter the waiting pull requests
// before attribution (her timeline holds R3, and her production_apply counts it); keep every
// component in derive() (her components hold K2).
func TestADeliveryReadShowsOnlyThePullRequestsItsViewerSees(t *testing.T) {
	t.Parallel()
	whole := seedDeliveryReads(t, true, true)
	publicOnly := seedDeliveryReads(t, true, false)
	privateOnly := seedDeliveryReads(t, false, true)

	for _, compared := range []struct {
		caller privateCaller
		like   privateSeed
		reads  []string
	}{
		{callerAlice, publicOnly, []string{"timeline", "measures", "pipeline"}},
		{callerBob, privateOnly, []string{"timeline", "measures"}},
		{callerBobPrivToken, privateOnly, []string{"timeline", "measures"}},
	} {
		for _, read := range deliveryReadsTargets {
			if !slices.Contains(compared.reads, read.name) {
				continue
			}
			got := maskedDeliveryRead(t, whole, compared.caller, read.target)
			want := maskedDeliveryRead(t, compared.like, compared.caller, read.target)
			for _, apart := range deliveryFieldsApart(got, want) {
				t.Errorf("%s: the %s differs from the seed that holds only what %s is shown, in %s", compared.caller, read.name, compared.caller, apart)
			}
		}
	}

	shipped := deliveryReadsFrom.Add(90 * time.Minute).Format(time.RFC3339)
	onlyPrivate := shownDelivery{
		prs:     []string{"acme/widgets#2", "acme/widgets#5"},
		runs:    map[int64][]string{deliveryRunR1: {"acme/widgets#2"}, deliveryRunR2: {"acme/widgets#5"}, deliveryRunR3: {}},
		waiting: []string{"2026-09-25T20:00:00Z shipped " + shipped}, components: []string{"PRIV/k2"},
		groups: []string{"Group two"}, unowned: []string{"PRIV-2"}, unfetchable: 1,
	}
	allOfIt := shownDelivery{
		prs: []string{"acme/other#3", "acme/widgets#1", "acme/widgets#2", "acme/widgets#5"},
		runs: map[int64][]string{
			deliveryRunR1: {"acme/widgets#1", "acme/widgets#2"}, deliveryRunR2: {"acme/widgets#5"}, deliveryRunR3: {},
		},
		waiting: []string{"2026-09-25T20:00:00Z shipped " + shipped}, components: []string{"PRIV/k2", "PUB/k1"},
		groups: []string{"Group one", "Group two"}, unowned: []string{"PRIV-2", "PUB-2"}, unfetchable: 2,
	}
	for caller, want := range map[privateCaller]shownDelivery{
		callerAlice: {
			prs: []string{"acme/other#3", "acme/widgets#1"}, runs: map[int64][]string{deliveryRunR1: {"acme/widgets#1"}},
			waiting: []string{}, components: []string{"PUB/k1"}, groups: []string{"Group one"}, unowned: []string{"PUB-2"}, unfetchable: 1,
		},
		callerBob:            onlyPrivate,
		callerBobPrivToken:   onlyPrivate,
		callerCarol:          allOfIt,
		callerCarolPrivToken: allOfIt,
	} {
		if got := readShownDelivery(t, whole, caller); !reflect.DeepEqual(got, want) {
			t.Errorf("%s reads the timeline as %+v, want %+v", caller, got, want)
		}
	}

	// The Pipeline page as carol reads it: every deploy run's production job and both PR-checks
	// runs, C2's failed job among the failing jobs.
	pipeline := decodeBody[deliveryPipelineResponse](t, whole.mustRequest(t, callerCarol, http.MethodGet, deliveryReadsTargets[2].target, nil, http.StatusOK))
	if got := pipeline.ProductionApply.Total.N; got != 3 {
		t.Errorf("carol's production_apply counts %d deploys, want R1, R2 and R3", got)
	}
	if got := pipeline.PRChecks.Run.Total.N; got != 2 {
		t.Errorf("carol's pr_checks count %d runs, want C2 and C9", got)
	}
	if failing := pipeline.PRChecks.FailingJobs; len(failing) != 1 || failing[0].LastRunURL != "https://github.com/acme/widgets/actions/runs/602" {
		t.Errorf("carol's pr_checks failing jobs = %+v, want C2's build / unit", failing)
	}

	run := func(id int64) string { return "/api/v1/delivery/runs/" + strconv.FormatInt(id, 10) }
	for _, caller := range []privateCaller{callerBob, callerBobPrivToken} {
		for _, target := range []string{deliveryReadsTargets[2].target, run(deliveryRunR1)} {
			if refused := whole.request(t, caller, http.MethodGet, target, nil); refused.Code != http.StatusForbidden ||
				decodeBody[struct {
					Code string `json:"code"`
				}](t, refused).Code != codeDeploymentReadRefused {
				t.Errorf("%s: GET %s = %d %s, want 403 DEPLOYMENT_READ_REFUSED", caller, target, refused.Code, refused.Body.String())
			}
		}
	}
	// A run alice is not shown answers as one no run holds, which none of these ids is in the seed
	// of PUB's rows alone.
	for _, id := range []int64{deliveryRunR2, deliveryRunR3, deliveryRunC2} {
		got := whole.request(t, callerAlice, http.MethodGet, run(id), nil)
		want := publicOnly.request(t, callerAlice, http.MethodGet, run(id), nil)
		if want.Code != http.StatusNotFound || got.Code != want.Code || got.Body.String() != want.Body.String() {
			t.Errorf("alice: GET %s = %d %s, want what an id no run holds answers: %d %s", run(id), got.Code, got.Body.String(), want.Code, want.Body.String())
		}
	}
	jobs := func(caller privateCaller, id int64) []string {
		t.Helper()
		names := []string{}
		for _, job := range decodeBody[delivery.DeliveryRunDetailView](t, whole.mustRequest(t, caller, http.MethodGet, run(id), nil, http.StatusOK)).Jobs {
			names = append(names, job.Name)
		}
		return names
	}
	if got := jobs(callerAlice, deliveryRunR1); !slices.Equal(got, []string{"release / release"}) {
		t.Errorf("alice: GET %s lists jobs %v, want R1's release / release", run(deliveryRunR1), got)
	}
	if got := jobs(callerCarol, deliveryRunR2); !slices.Equal(got, []string{"release / release"}) {
		t.Errorf("carol: GET %s lists jobs %v, want R2's release / release", run(deliveryRunR2), got)
	}
}

// deliveryRunFailed is run 702 of the second private-projects dogfood (2026-11-19): a deploy run
// on main that failed, so it shipped nothing, and whose only pull request is another project's,
// still waiting.
const deliveryRunFailed int64 = 702

// seedRunThatShippedNothing stands up verification v16's seed with seedDeliveryReads' delivery
// settings, P2 (PRIV-1) and R1, which ships it, and, with failed, the shape of the dogfood's run
// 702: P25 (PUB-1), merged after R1, and run 702, a deploy run on main started after P25's merge
// whose production job failed, so it shipped nothing and P25 still waits.
func seedRunThatShippedNothing(t *testing.T, failed bool) privateSeed {
	t.Helper()
	seed := seedPrivateProject(t)
	ctx, pool := t.Context(), seed.database.Pool
	if _, err := delivery.PutSettings(ctx, pool, delivery.DeliverySettings{
		DeployRepo: "acme/widgets", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobNames: []string{"release / release"}, PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: []string{"octocat"},
	}, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("write the delivery settings: %v", err)
	}
	pr := func(number int, issueKey string, merged time.Time) {
		t.Helper()
		opened := merged.Add(-2 * time.Hour)
		upsertPR(t, seed.database, delivery.DeliveryPullRequest{
			Repo: "acme/widgets", Number: number, Title: "change " + strconv.Itoa(number), Author: "octocat",
			URL:       fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number),
			CreatedAt: &opened, MergedAt: &merged, IssueKey: &issueKey,
		})
	}
	deploy := func(id int64, started time.Time, conclusion string) {
		t.Helper()
		seedPipelineRun(t, pool, delivery.DeliveryRun{
			RunID: id, Kind: delivery.DeliveryRunKindDeploy, StartedAt: started,
			CompletedAt: new(started.Add(40 * time.Minute)), Conclusion: new(delivery.DeliveryRunConclusion(conclusion)),
			HeadBranch: new("main"), Event: new("push"),
		}, []pipelineFixtureJob{{name: "release / release", conclusion: conclusion, start: started, end: started.Add(30 * time.Minute)}})
	}
	day := func(hour int) time.Time { return time.Date(2026, 10, 1, hour, 0, 0, 0, time.UTC) }
	pr(2, seed.issue[seedPrivate], day(2))
	deploy(deliveryRunR1, day(3), string(delivery.DeliveryRunConclusionSuccess))
	if failed {
		pr(25, seed.issue[seedPublic], day(6))
		deploy(deliveryRunFailed, day(7), string(delivery.DeliveryRunConclusionFailure))
	}
	return seed
}

// belowTheReadGate is caller's GET of target answered by handle alone, with the request's path
// values set from path: what the route's handler answers once the route's read-class gate
// (withinReadScope) lets the caller through. That gate refuses a deployment read to every viewer
// that is not open, so for such a viewer this is the handler's own rule.
func (s privateSeed) belowTheReadGate(t *testing.T, caller privateCaller, handle http.HandlerFunc, target string, path map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.Header = s.readerRequest(t, caller).Header
	for name, value := range path {
		request.SetPathValue(name, value)
	}
	recorder := httptest.NewRecorder()
	handle(recorder, request)
	return recorder
}

// TestAViewerThatIsNotOpenSeesOnlyTheRunsThatShippedAPullRequestItSees: a viewer that is not open
// (bob, a restricted member of PRIV, and bob's token that lists PRIV) is shown a deploy run only
// when the run shipped a pull request that viewer is shown. The dogfood's run 702, which failed and
// so shipped nothing, and whose only pull request is PUB's P25, still waiting, is in none of its
// reads: its timeline and its measures equal those of a seed where run 702 and P25 were never
// written, and so do its Pipeline page and its read of run 702, which answers as an id no run
// holds. R1, which shipped PRIV's P2, is still shown. The Pipeline page and a run's own read refuse
// such a viewer before any read (403 DEPLOYMENT_READ_REFUSED, for every id), so the test reads both
// through the route and from their handlers below that gate (belowTheReadGate), where run 702
// answers 404 NOT_FOUND exactly as in the seed without it. alice, an open viewer, still reads run
// 702 in each read: an open viewer's rule is unchanged, and leaves out only a run that shipped pull
// requests none of which is shown, here R1.
//
// Controls, each run on its own, each failing the test: show a run that shipped nothing to every
// viewer in deliveryPopulation.runShown, the rule before (bob's timeline lists run 702, and his
// measures and Pipeline page count it); show a run that shipped nothing to every viewer in
// deliveryRunShown, the rule before (bob's read of run 702 below the gate answers its jobs); show
// it to no viewer in either (alice's timeline, deploy_run_success, production_apply and read of run
// 702 lose it).
func TestAViewerThatIsNotOpenSeesOnlyTheRunsThatShippedAPullRequestItSees(t *testing.T) {
	t.Parallel()
	carried := seedRunThatShippedNothing(t, true)
	without := seedRunThatShippedNothing(t, false)
	failedRun := strconv.FormatInt(deliveryRunFailed, 10)
	run := "/api/v1/delivery/runs/" + failedRun
	pipeline := deliveryReadsTargets[2].target

	for _, caller := range []privateCaller{callerBob, callerBobPrivToken} {
		for _, read := range deliveryReadsTargets[:2] {
			got := maskedDeliveryRead(t, carried, caller, read.target)
			want := maskedDeliveryRead(t, without, caller, read.target)
			for _, apart := range deliveryFieldsApart(got, want) {
				t.Errorf("%s: the %s differs from the seed without run 702, in %s", caller, read.name, apart)
			}
		}
		if got := readShownDelivery(t, carried, caller).runs; !reflect.DeepEqual(got, map[int64][]string{deliveryRunR1: {"acme/widgets#2"}}) {
			t.Errorf("%s reads the timeline's runs as %v, want R1 with P2 alone", caller, got)
		}
		for _, target := range []string{pipeline, run} {
			got := carried.request(t, caller, http.MethodGet, target, nil)
			want := without.request(t, caller, http.MethodGet, target, nil)
			if want.Code != http.StatusForbidden || got.Code != want.Code || got.Body.String() != want.Body.String() {
				t.Errorf("%s: GET %s = %d %s, want what the seed without run 702 answers: %d %s", caller, target, got.Code, got.Body.String(), want.Code, want.Body.String())
			}
		}
		id := map[string]string{"id": failedRun}
		got := carried.belowTheReadGate(t, caller, carried.server.getDeliveryRun, run, id)
		want := without.belowTheReadGate(t, caller, without.server.getDeliveryRun, run, id)
		if want.Code != http.StatusNotFound || got.Code != want.Code || got.Body.String() != want.Body.String() {
			t.Errorf("%s: below the read gate, GET %s = %d %s, want what an id no run holds answers: %d %s", caller, run, got.Code, got.Body.String(), want.Code, want.Body.String())
		}
		gotPipeline := carried.belowTheReadGate(t, caller, carried.server.getDeliveryPipeline, pipeline, nil)
		wantPipeline := without.belowTheReadGate(t, caller, without.server.getDeliveryPipeline, pipeline, nil)
		if gotPipeline.Code != http.StatusOK || wantPipeline.Code != http.StatusOK {
			t.Fatalf("%s: below the read gate, GET %s = %d %s and, without run 702, %d %s; want 200 from both", caller, pipeline,
				gotPipeline.Code, gotPipeline.Body.String(), wantPipeline.Code, wantPipeline.Body.String())
		}
		for _, apart := range deliveryFieldsApart(maskedDeliveryBody(t, gotPipeline), maskedDeliveryBody(t, wantPipeline)) {
			t.Errorf("%s: below the read gate, the pipeline differs from the seed without run 702, in %s", caller, apart)
		}
	}

	if got := readShownDelivery(t, carried, callerAlice).runs; !reflect.DeepEqual(got, map[int64][]string{deliveryRunFailed: {}}) {
		t.Errorf("alice reads the timeline's runs as %v, want run 702 alone, with no pull request", got)
	}
	if got := decodeBody[deliveryMeasuresResponse](t, carried.mustRequest(t, callerAlice, http.MethodGet, deliveryReadsTargets[1].target, nil, http.StatusOK)).Measures.DeployRunSuccess.Concluded; got != 1 {
		t.Errorf("alice's deploy_run_success counts %d concluded runs, want run 702's alone", got)
	}
	if got := decodeBody[deliveryPipelineResponse](t, carried.mustRequest(t, callerAlice, http.MethodGet, pipeline, nil, http.StatusOK)).ProductionApply.Total.N; got != 1 {
		t.Errorf("alice's production_apply counts %d deploys, want run 702's alone", got)
	}
	jobs := []string{}
	for _, job := range decodeBody[delivery.DeliveryRunDetailView](t, carried.mustRequest(t, callerAlice, http.MethodGet, run, nil, http.StatusOK)).Jobs {
		jobs = append(jobs, job.Name)
	}
	if !slices.Equal(jobs, []string{"release / release"}) {
		t.Errorf("alice: GET %s lists jobs %v, want run 702's release / release", run, jobs)
	}
}

// TestAnUnmergedPullRequestsChangeMovesThePipelineView: a PR-checks run is shown when its pull
// request, the deploy repository's row of its number, is, merged or not (prChecksShown), so a change
// to a pull request that never merged can move a Pipeline view, and the live hub marks the view
// for it. alice, an open viewer, holds a Pipeline view whose PR checks count C, a run of #31,
// which no row holds yet and so names no issue. #31 is then stored unmerged, naming PRIV's issue,
// which alice does not see: the view's next frame counts no PR-checks run.
//
// Control: the hub does not mark a Pipeline view for a change to a pull request with no merge
// time (touchedBy); no frame comes, and the test fails.
func TestAnUnmergedPullRequestsChangeMovesThePipelineView(t *testing.T) {
	t.Parallel()
	seed := seedPrivateProject(t)
	seedHubSettings(t, seed.database)
	clock := newNoonHubClock()
	started := clock.Now().Add(-2 * time.Hour)
	seedPipelineRun(t, seed.database.Pool, delivery.DeliveryRun{
		RunID: 731, Kind: delivery.DeliveryRunKindPRChecks, PRNumber: new(31), StartedAt: started,
		CompletedAt: new(started.Add(20 * time.Minute)), Conclusion: new(delivery.DeliveryRunConclusionSuccess),
		Event: new("pull_request"),
	}, []pipelineFixtureJob{{name: "build / unit", conclusion: "success", start: started, end: started.Add(15 * time.Minute)}})
	alice, err := seed.server.readerFor(t.Context(), model.BrowserActor(seedAlice))
	if err != nil {
		t.Fatalf("resolve what alice sees: %v", err)
	}
	rig := hubOn(t, seed.handler, seed.database, seed.deps, clock, nil)
	checks := func(what string, raw json.RawMessage) int {
		t.Helper()
		var prChecks deliveryPipelinePRChecks
		if err := json.Unmarshal(raw, &prChecks); err != nil {
			t.Fatalf("decode %s's pr_checks: %v (%s)", what, err, raw)
		}
		return prChecks.Run.Total.N
	}
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, deliverySubscription{Sub: "p", View: livePipelineView(nil), Reader: alice})
	var body struct {
		PRChecks json.RawMessage `json:"pr_checks"`
	}
	if err := json.Unmarshal(conn.snapshot(t, 5*time.Second, "p").Body, &body); err != nil {
		t.Fatalf("decode the snapshot: %v", err)
	}
	if got := checks("the snapshot", body.PRChecks); got != 1 {
		t.Fatalf("alice's Pipeline view counts %d PR-checks runs before #31 is stored, want C", got)
	}
	rig.waitListening(t)
	// The connection's own re-check (a notification may have gone by unheard while it was down)
	// reads the view again; it runs before the write below, so only the write's change can.
	rig.waitFor(t, 0, 5*time.Second, "the re-check after the listener connected", func(note deliveryHubNote) bool {
		return note.what == "readers rechecked"
	})
	rig.settle(t)

	upsertPR(t, seed.database, delivery.DeliveryPullRequest{
		Repo: "acme/widgets", Number: 31, Title: "change 31", URL: "https://github.com/acme/widgets/pull/31",
		Author: "octocat", Partial: true, IssueKey: new(seed.issue[seedPrivate]),
	})
	delta := conn.delta(t, 5*time.Second, "p")
	raw, set := delta.Set["pr_checks"]
	if !set {
		t.Fatalf("the delta after #31 was stored unmerged sets no pr_checks: %+v", delta)
	}
	if got := checks("the delta", raw); got != 0 {
		t.Errorf("after #31 was stored naming PRIV's issue, alice's Pipeline view counts %d PR-checks runs, want none", got)
	}
}
