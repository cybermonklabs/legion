package api

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// sourcesDay is the day the per-source tests' runs start on.
var sourcesDay = time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)

// seedSourceRun writes run id of repo, a deploy run on main created and started hours into
// sourcesDay that succeeded, holding the one job named job, which succeeded.
func seedSourceRun(t *testing.T, database *store.Store, repo string, id int64, hours int, job string) {
	t.Helper()
	ctx := t.Context()
	created := sourcesDay.Add(time.Duration(hours) * time.Hour)
	started, completed := created.Add(time.Minute), created.Add(30*time.Minute)
	if err := delivery.UpsertRun(ctx, database.Pool, delivery.DeliveryRun{
		Repo: repo, RunID: id, Kind: delivery.DeliveryRunKindDeploy, HeadSHA: "sha" + strconv.FormatInt(id, 10),
		HeadCommitAt: created.Add(-time.Minute), StartedAt: started, CreatedAt: &created, CompletedAt: &completed,
		Conclusion: new(delivery.DeliveryRunConclusionSuccess), URL: "https://github.com/" + repo + "/actions/runs/" + strconv.FormatInt(id, 10),
		HeadBranch: new("main"), Event: new("push"),
	}); err != nil {
		t.Fatalf("seed run %d of %s: %v", id, repo, err)
	}
	jobs := []delivery.DeliveryRunJob{{
		Repo: repo, RunID: id, Name: job, StartedAt: new(started.Add(time.Minute)), CompletedAt: new(completed.Add(-time.Minute)),
		Conclusion: new(delivery.DeliveryJobConclusionSuccess),
	}}
	if err := delivery.UpsertRunJobs(ctx, database.Pool, repo, id, jobs, true); err != nil {
		t.Fatalf("seed run %d's job: %v", id, err)
	}
}

// setDelivery turns project's delivery on or off, as a save of it with its sources unchanged does.
func setDelivery(t *testing.T, database *store.Store, project string, on bool) {
	t.Helper()
	if _, err := database.Pool.Exec(t.Context(), `update project_delivery_settings set delivery = $2 where project_key = $1`, project, on); err != nil {
		t.Fatalf("turn %s's delivery %v: %v", project, on, err)
	}
}

// The delivery routes read every source repository of every delivery-on project, each under its
// own source's rule: ALPHA's two repositories each count the run whose production job their own
// rule names as a deploy, and a run of acme/two holding acme/one's job counts as none; BETA, with
// delivery off, reads acme/three nowhere, so its run is counted by no route and is no run of
// /runs/{id}. With every project off, each route answers 404 DELIVERY_NOT_CONFIGURED naming the
// project route.
func TestTheDeliveryRoutesReadEachSourceRepositoryUnderItsOwnRule(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "ALPHA", "BETA")
	saveProjectRules(t, database, "ALPHA", deployRule("acme/one", "one / release"), deployRule("acme/two", "two / release"))
	saveProjectRules(t, database, "BETA", deployRule("acme/three", "three / release"))
	setDelivery(t, database, "BETA", false)
	seedSourceRun(t, database, "acme/one", 1001, 1, "one / release")
	seedSourceRun(t, database, "acme/two", 1002, 2, "two / release")
	seedSourceRun(t, database, "acme/two", 1003, 3, "one / release")
	seedSourceRun(t, database, "acme/three", 1004, 4, "three / release")
	window := "from=" + sourcesDay.Format(time.RFC3339) + "&to=" + sourcesDay.AddDate(0, 0, 1).Format(time.RFC3339)

	measured := getDeliveryMeasures(t, handler, window)
	if deploys := measured.Measures.DeployFrequency.SuccessfulDeploys; deploys != 2 {
		t.Fatalf("the measures count %d deploys, want 2: acme/one's and acme/two's, each by its own rule", deploys)
	}
	timeline := getDeliveryTimeline(t, handler, window)
	production := map[int64]bool{}
	for _, run := range timeline.Runs {
		production[run.ID] = run.Production != nil
	}
	if len(production) != 3 || !production[1001] || !production[1002] || production[1003] {
		t.Fatalf("the timeline lists runs %v (true where a production job was found), want 1001 and 1002 deploys and 1003 none, and no run of acme/three", production)
	}
	_, pipeline := getDeliveryPipeline(t, handler, "at="+sourcesDay.Add(20*time.Hour).Format(time.RFC3339))
	if card := pipeline.ProductionApply; card.Job != "one / release or two / release" || card.Total.N != 2 {
		t.Fatalf("the Pipeline's production card is %+v, want both repositories' jobs and their two deploys", card)
	}
	if response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/runs/1004", nil, "alice"); response.Code != http.StatusNotFound {
		t.Fatalf("a run of the delivery-off project's repository: status=%d, want 404", response.Code)
	}
	if response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/runs/1002", nil, "alice"); response.Code != http.StatusOK {
		t.Fatalf("a run of acme/two: status=%d body=%s, want 200", response.Code, response.Body.String())
	}

	setDelivery(t, database, "ALPHA", false)
	for _, path := range []string{"/api/v1/delivery/timeline?" + window, "/api/v1/delivery/measures?" + window, "/api/v1/delivery/pipeline", "/api/v1/delivery/runs/1002"} {
		response := dispatchRequest(t, handler, http.MethodGet, path, nil, "alice")
		refusal := decodeBody[refusalAnswer](t, response)
		if response.Code != http.StatusNotFound || refusal.Code != deliveryNotConfiguredCode || !strings.Contains(refusal.Error, "PUT /api/v1/projects/{key}/settings/delivery") {
			t.Errorf("GET %s with no delivery-on project: status=%d body=%s, want 404 DELIVERY_NOT_CONFIGURED naming the project route", path, response.Code, response.Body.String())
		}
	}
}

// The delivery answers judge by the targets of the one delivery-on project whose sources read
// every deploy repository, two of ALPHA's here, and by the catalogue's once a second project's
// source reads one too.
func TestTheDeliveryTargetsAreTheOneOwningProjects(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	createTestProject(t, handler, "ALPHA", "BETA")
	saveProjectRules(t, database, "ALPHA", deployRule("acme/one", "one / release"), deployRule("acme/two", "two / release"))
	saveProjectRules(t, database, "BETA", deployRule("acme/three", "three / release"))
	setDelivery(t, database, "BETA", false)
	if _, err := evals.New(database, deps.Docs, deps.Events, deps.Attributor).Register(t.Context(), evals.Registration{
		Project: "ALPHA", Slug: evals.SlugDeployFrequency, Caller: model.Actor{Kind: "user", ID: "alice"}, Target: evals.Set(new(7.0)),
	}); err != nil {
		t.Fatalf("set ALPHA's deploy-frequency target: %v", err)
	}
	window := "from=" + sourcesDay.Format(time.RFC3339) + "&to=" + sourcesDay.AddDate(0, 0, 1).Format(time.RFC3339)
	target := func() float64 {
		t.Helper()
		targets := getDeliveryMeasures(t, handler, window).Targets
		if targets.DeploysPerDay == nil {
			t.Fatal("the measures state no deploys-a-day target")
		}
		return targets.DeploysPerDay.Value
	}
	if got := target(); got != 7 {
		t.Fatalf("with ALPHA reading both repositories the deploys-a-day target is %v, want ALPHA's 7", got)
	}
	setDelivery(t, database, "BETA", true)
	catalogue := catalogueTargets()
	if got := target(); catalogue.DeploysPerDay == nil || got != catalogue.DeploysPerDay.Value {
		t.Fatalf("with ALPHA and BETA each reading a repository the target is %v, want the catalogue's %+v", got, catalogue.DeploysPerDay)
	}
}

// A run read where the one delivery-on project's one source is an events source, which reads no
// repository, is 404 NOT_FOUND saying that no source reads a repository, rather than naming an
// empty list of them.
func TestARunReadWhereNoSourceReadsARepositorySaysSo(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "POSTED")
	saveProjectDelivery(t, database.Pool, delivery.ProjectSettingsInput{
		Project: "POSTED", Delivery: true, Sources: []delivery.SourceInput{{Kind: delivery.SourceKindEvents}},
		PopulationAuthors: []string{"octocat"}, ExcludedRepos: []string{},
	})
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/runs/7", nil, "alice")
	refusal := decodeBody[refusalAnswer](t, response)
	if response.Code != http.StatusNotFound || refusal.Code != "NOT_FOUND" ||
		refusal.Error != "no run 7: no deploy source of a delivery-on project reads a repository" {
		t.Fatalf("a run read with no source repository: status=%d body=%s, want 404 NOT_FOUND saying no source reads a repository",
			response.Code, response.Body.String())
	}
}
