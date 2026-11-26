package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/prodevents"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The tests of GET /api/v1/projects/{key}/deploy-runs/{id} (dispatch://INFRA-2918, D14): the
// drill-down's run detail under the project whose timeline row opened it, read from the repository
// the row names.

// deployRunSource is a github_workflow source reading repo, whose deploys its job release judges.
func deployRunSource(repo string) delivery.SourceInput {
	return delivery.SourceInput{
		Kind: delivery.SourceKindGitHubWorkflow, Repo: &repo, DeployWorkflowPath: new(".github/workflows/deploy.yml"),
		ProductionJobNames: []string{"release"}, PRChecksWorkflowPath: new(".github/workflows/pr-checks.yml"),
	}
}

// putDeploySources saves project's delivery settings, delivery on or off, with sources.
func putDeploySources(t *testing.T, database *store.Store, project string, on bool, sources ...delivery.SourceInput) {
	t.Helper()
	ctx := t.Context()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the settings of %s: %v", project, err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := delivery.PutProjectSettings(ctx, tx, delivery.ProjectSettingsInput{Project: project, Delivery: on, Sources: sources},
		model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("save the delivery settings of %s: %v", project, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the settings of %s: %v", project, err)
	}
}

// openDeployRunIssue opens an issue of project titled title, which no other issue's shares a word
// of, so the create's duplicate check passes it, and answers its key.
func openDeployRunIssue(t *testing.T, handler http.Handler, project, title string) string {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": project, "title": title}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("open an issue of %s: status %d, body %s", project, response.Code, response.Body.String())
	}
	return decodeBody[struct {
		Key string `json:"key"`
	}](t, response).Key
}

// deployRunPR writes repo#n by octocat, merged at merged, naming issue.
func deployRunPR(t *testing.T, database *store.Store, repo string, n int, merged time.Time, issue string) {
	t.Helper()
	opened := merged.Add(-2 * time.Hour)
	upsertPR(t, database, delivery.DeliveryPullRequest{
		Repo: repo, Number: n, Title: "feat: change " + strconv.Itoa(n), URL: "https://github.com/" + repo + "/pull/" + strconv.Itoa(n),
		Author: "octocat", MergedAt: &merged, CreatedAt: &opened, IssueKey: &issue,
	})
}

// deployRunIn writes deploy run id of repo on main, created at created and started a minute later:
// a job named build of repo, then the production job release, each succeeding five minutes after
// it starts.
func deployRunIn(t *testing.T, database *store.Store, repo string, id int64, created time.Time) {
	t.Helper()
	ctx := t.Context()
	started := created.Add(time.Minute)
	built, released, completed := started.Add(5*time.Minute), started.Add(6*time.Minute), started.Add(11*time.Minute)
	success, jobSuccess := delivery.DeliveryRunConclusionSuccess, delivery.DeliveryJobConclusionSuccess
	url := "https://github.com/" + repo + "/actions/runs/" + strconv.FormatInt(id, 10)
	if err := delivery.UpsertRun(ctx, database.Pool, delivery.DeliveryRun{
		Repo: repo, RunID: id, Kind: delivery.DeliveryRunKindDeploy, HeadSHA: "sha" + strconv.FormatInt(id, 10),
		HeadCommitAt: created.Add(-time.Minute), StartedAt: started, CreatedAt: &created, CompletedAt: &completed, Conclusion: &success,
		URL: url, HeadBranch: new("main"), Event: new("push"),
	}); err != nil {
		t.Fatalf("upsert deploy run %d of %s: %v", id, repo, err)
	}
	if err := delivery.UpsertRunJobs(ctx, database.Pool, repo, id, []delivery.DeliveryRunJob{
		{Repo: repo, RunID: id, Name: "build " + repo, StartedAt: &started, CompletedAt: &built, Conclusion: &jobSuccess},
		{Repo: repo, RunID: id, Name: "release", StartedAt: &released, CompletedAt: &completed, Conclusion: &jobSuccess},
	}, true); err != nil {
		t.Fatalf("upsert the jobs of deploy run %d of %s: %v", id, repo, err)
	}
}

// TestADeployRunAnswersItsJobsFromTheNamedSourceRepository: project SHIP deploys from two sources in
// two repositories, OTHR from a third, and IDLE, with delivery off, names a fourth. A run id is one
// repository's: run 100 of each of SHIP's repositories ships a SHIP pull request, and each answers
// its own jobs and address on SHIP's route with its repository, the first's id, address and jobs
// byte for byte what GET /api/v1/delivery/runs/{id} answers of a deployment deploying from it,
// beside a record holding no deploy row. A deploy in OTHR's repository that shipped a SHIP pull
// request answers its jobs on SHIP's route, as its timeline row is SHIP's; one there that ships
// only OTHR's pull request answers 404 on SHIP's route and its jobs on OTHR's; and one there that
// ships no pull request answers its jobs on OTHR's route, whose deploy source found it, and 404 on
// SHIP's (INFRA-978 R12). A deploy of IDLE's repository shipping a SHIP pull request answers 404,
// as no delivery-on project's source reads it, and so does a run id the named repository holds no
// run of. A query naming no repo, or naming it twice, answers 400 INVALID_QUERY naming repo.
func TestADeployRunAnswersItsJobsFromTheNamedSourceRepository(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createTestProject(t, handler, "SHIP", "OTHR", "IDLE")
	putDeploySources(t, database, "SHIP", true, deployRunSource("acme/alpha"), deployRunSource("acme/beta"))
	putDeploySources(t, database, "OTHR", true, deployRunSource("acme/gamma"))
	putDeploySources(t, database, "IDLE", false, deployRunSource("acme/delta"))
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, pr := range []struct {
		repo    string
		number  int
		merged  time.Time
		project string
		title   string
	}{
		{"acme/alpha", 1, at, "SHIP", "Widgets"}, {"acme/beta", 1, at, "SHIP", "Gadgets"}, {"acme/gamma", 5, at, "SHIP", "Sprockets"},
		{"acme/gamma", 6, at.Add(3 * time.Hour), "OTHR", "Cogwheels"}, {"acme/delta", 1, at, "SHIP", "Levers"},
	} {
		deployRunPR(t, database, pr.repo, pr.number, pr.merged, openDeployRunIssue(t, handler, pr.project, pr.title))
	}
	deployRunIn(t, database, "acme/alpha", 100, at.Add(time.Hour))
	deployRunIn(t, database, "acme/beta", 100, at.Add(time.Hour))
	deployRunIn(t, database, "acme/gamma", 300, at.Add(time.Hour))
	deployRunIn(t, database, "acme/gamma", 301, at.Add(4*time.Hour))
	deployRunIn(t, database, "acme/gamma", 302, at.Add(6*time.Hour))
	deployRunIn(t, database, "acme/delta", 400, at.Add(time.Hour))

	target := func(project string, id int64, query string) string {
		return "/api/v1/projects/" + project + "/deploy-runs/" + strconv.FormatInt(id, 10) + query
	}
	answers := []struct {
		project string
		id      int64
		repo    string
	}{
		{"SHIP", 100, "acme/alpha"}, {"SHIP", 100, "acme/beta"}, {"SHIP", 300, "acme/gamma"}, {"OTHR", 301, "acme/gamma"}, {"OTHR", 302, "acme/gamma"},
	}
	for _, want := range answers {
		path := target(want.project, want.id, "?repo="+want.repo)
		response := dispatchRequest(t, handler, http.MethodGet, path, nil, "alice")
		if response.Code != http.StatusOK {
			t.Errorf("GET %s: status %d, body %s; want the run's jobs", path, response.Code, response.Body.String())
			continue
		}
		detail := decodeBody[delivery.DeliveryRunDetailView](t, response)
		names := make([]string, len(detail.Jobs))
		for i, job := range detail.Jobs {
			names[i] = job.Name
		}
		wantURL := "https://github.com/" + want.repo + "/actions/runs/" + strconv.FormatInt(want.id, 10)
		if detail.ID != want.id || detail.URL != wantURL || !slices.Equal(names, []string{"build " + want.repo, "release"}) {
			t.Errorf("GET %s = run %d at %s with jobs %v; want run %d at %s with build %s and release, its repository's", path,
				detail.ID, detail.URL, names, want.id, wantURL, want.repo)
		}
	}

	if _, err := delivery.PutSettings(t.Context(), database.Pool, delivery.DeliverySettings{
		DeployRepo: "acme/alpha", DeployWorkflowPath: ".github/workflows/deploy.yml", ProductionJobNames: []string{"release"},
		PRChecksWorkflowPath: ".github/workflows/pr-checks.yml", PopulationAuthors: []string{"octocat"},
	}, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("seed the deployment's delivery settings: %v", err)
	}
	fields := func(body json.RawMessage) map[string]json.RawMessage {
		t.Helper()
		var decoded map[string]json.RawMessage
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		return decoded
	}
	base, project := fields(getBody(t, handler, "/api/v1/delivery/runs/100")), fields(getBody(t, handler, target("SHIP", 100, "?repo=acme/alpha")))
	if string(project["deploy"]) != "null" || string(project["failures"]) != "[]" {
		t.Errorf("SHIP's run 100 of acme/alpha, of which no deploy row is written, answers the record deploy %s, failures %s; want null and []",
			project["deploy"], project["failures"])
	}
	delete(project, "deploy")
	delete(project, "failures")
	if !reflect.DeepEqual(project, base) {
		t.Errorf("SHIP's run 100 of acme/alpha answers %v beside its record, GET /api/v1/delivery/runs/100 from acme/alpha %v; want the same fields", project, base)
	}

	for _, refused := range []struct {
		what, path string
		status     int
		code       string
	}{
		{"a deploy of OTHR's repository shipping no SHIP pull request", target("SHIP", 301, "?repo=acme/gamma"), http.StatusNotFound, "NOT_FOUND"},
		{"a deploy of OTHR's repository shipping no pull request", target("SHIP", 302, "?repo=acme/gamma"), http.StatusNotFound, "NOT_FOUND"},
		{"a repository only a delivery-off project's source reads", target("SHIP", 400, "?repo=acme/delta"), http.StatusNotFound, "NOT_FOUND"},
		{"a run id the named repository holds no run of", target("SHIP", 300, "?repo=acme/alpha"), http.StatusNotFound, "NOT_FOUND"},
		{"no repo", target("SHIP", 100, ""), http.StatusBadRequest, "INVALID_QUERY"},
		{"repo named twice", target("SHIP", 100, "?repo=acme/alpha&repo=acme/beta"), http.StatusBadRequest, "INVALID_QUERY"},
	} {
		response := dispatchRequest(t, handler, http.MethodGet, refused.path, nil, "alice")
		answer := decodeBody[refusalAnswer](t, response)
		if response.Code != refused.status || answer.Code != refused.code {
			t.Errorf("%s: GET %s = %d %s; want %d %s", refused.what, refused.path, response.Code, response.Body.String(), refused.status, refused.code)
		}
		if refused.status == http.StatusBadRequest && !strings.Contains(answer.Error, "repo") {
			t.Errorf("%s: the refusal %q does not name repo", refused.what, answer.Error)
		}
	}
}

// deployRowOf writes the deploy row the GitHub adapter writes for run id of repo (deployRunIn,
// created at created), under project, the project that owns the repository, as having shipped prs.
func deployRowOf(t *testing.T, database *store.Store, project, repo string, id int64, created time.Time, prs ...string) prodevents.Event {
	t.Helper()
	started, finished := created.Add(time.Minute), created.Add(12*time.Minute)
	row, _, err := prodevents.UpsertGitHubDeploy(t.Context(), database.Pool, prodevents.Event{
		ProjectKey: project, Kind: prodevents.KindDeploy, Source: prodevents.SourceGitHub, Status: prodevents.StatusSucceeded,
		SourceRef: prodevents.GitHubSourceRef(id), Repo: repo, StartedAt: &started, FinishedAt: &finished, PullRequests: prs,
		URL: "https://github.com/" + repo + "/actions/runs/" + strconv.FormatInt(id, 10), Actor: model.Actor{Kind: "system", ID: "github"},
	})
	if err != nil {
		t.Fatalf("write the deploy row of run %d of %s under %s: %v", id, repo, project, err)
	}
	return row
}

// TestADeployRunAnswersItsJobsAndItsRecord (dispatch://INFRA-2918, D14; R14): beside its jobs, a
// deploy run answers its record: the deploy row the GitHub adapter wrote for it, and every failure
// that blames that row, oldest detection first, each with its id, project, signal, state, reason,
// restore and note, as the writes of the drill-down's controls left it.
//
// SHIP deploys from acme/alpha and OTHR from acme/gamma, and the adapter wrote each run's deploy row
// under the project that owns its repository. Alice flags run 100 of acme/alpha through
// POST /api/v1/projects/SHIP/production-events, naming its run, the pull request she blames and a
// note, and Bob marks her flag restored; a system's page of OTHR blames the same deploy row, and Bob
// rejects it with a reason; a system's page blames run 101 alone. Run 100 answers Alice's flag and
// the rejected page, both kept, and not run 101's page, which run 101 answers alone. Run 300 of
// acme/gamma, OTHR's repository, shipped one of SHIP's pull requests, so SHIP's route answers it,
// with its deploy row under OTHR and Bob's flag posted to OTHR, the deploy row's project, with its
// note. Run 102 of acme/alpha, of which no deploy row is written, answers the record deploy null
// and failures [].
func TestADeployRunAnswersItsJobsAndItsRecord(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createTestProject(t, handler, "SHIP", "OTHR")
	putDeploySources(t, database, "SHIP", true, deployRunSource("acme/alpha"))
	putDeploySources(t, database, "OTHR", true, deployRunSource("acme/gamma"))
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	deployRunPR(t, database, "acme/alpha", 1, at, openDeployRunIssue(t, handler, "SHIP", "Widgets"))
	deployRunPR(t, database, "acme/alpha", 2, at.Add(3*time.Hour), openDeployRunIssue(t, handler, "SHIP", "Gadgets"))
	deployRunPR(t, database, "acme/gamma", 5, at, openDeployRunIssue(t, handler, "SHIP", "Sprockets"))
	deployRunIn(t, database, "acme/alpha", 100, at.Add(time.Hour))
	deployRunIn(t, database, "acme/alpha", 101, at.Add(4*time.Hour))
	deployRunIn(t, database, "acme/alpha", 102, at.Add(6*time.Hour))
	deployRunIn(t, database, "acme/gamma", 300, at.Add(time.Hour))
	ship100 := deployRowOf(t, database, "SHIP", "acme/alpha", 100, at.Add(time.Hour), "acme/alpha#1")
	ship101 := deployRowOf(t, database, "SHIP", "acme/alpha", 101, at.Add(4*time.Hour), "acme/alpha#2")
	othr300 := deployRowOf(t, database, "OTHR", "acme/gamma", 300, at.Add(time.Hour), "acme/gamma#5")

	written := func(what string, response *httptest.ResponseRecorder, status int) prodevents.Event {
		t.Helper()
		if response.Code != status {
			t.Fatalf("%s: status %d, body %s; want %d", what, response.Code, response.Body.String(), status)
		}
		return decodeBody[prodevents.Event](t, response)
	}
	alerts := map[string]any{"kind": "session", "id": "ses-alerts"}
	const flagNote, shippedNote, reason = "Sign-in failed for every user after this deploy.", "Checkout broke after this deploy.", "A flaky probe."
	flag := written("Alice's flag of run 100", dispatchRequest(t, handler, http.MethodPost, productionEventsPath("SHIP"), map[string]any{
		"kind": "failure", "cicd.pipeline.run.id": "100", "repo": "acme/alpha", "pull_requests": []string{"acme/alpha#1"}, "note": flagNote,
	}, "alice"), http.StatusCreated)
	page := written("a system's page of OTHR blaming run 100", bearerRequest(t, handler, http.MethodPost, productionEventsPath("OTHR"), map[string]any{
		"kind": "failure", "source_ref": "alerts:page-1", "deploy_id": ship100.ID, "actor": alerts,
	}), http.StatusCreated)
	later := written("a system's page blaming run 101", bearerRequest(t, handler, http.MethodPost, productionEventsPath("SHIP"), map[string]any{
		"kind": "failure", "source_ref": "alerts:page-2", "cicd.pipeline.run.id": "101", "repo": "acme/alpha", "actor": alerts,
	}), http.StatusCreated)
	page = written("Bob's reject of the page", dispatchRequest(t, handler, http.MethodPut, productionEventStatePath("OTHR", page.ID),
		map[string]any{"state": "rejected", "reason": reason}, "bob"), http.StatusOK)
	flag = written("Bob's restore of Alice's flag", dispatchRequest(t, handler, http.MethodPut, productionEventStatePath("SHIP", flag.ID),
		map[string]any{"state": "restored"}, "bob"), http.StatusOK)
	shipped := written("Bob's flag of run 300, to OTHR", dispatchRequest(t, handler, http.MethodPost, productionEventsPath("OTHR"), map[string]any{
		"kind": "failure", "cicd.pipeline.run.id": "300", "repo": "acme/gamma", "pull_requests": []string{"acme/gamma#5"}, "note": shippedNote,
	}, "bob"), http.StatusCreated)

	read := func(id int64, repo string) (projectDeployRunView, map[string]json.RawMessage) {
		t.Helper()
		body := getBody(t, handler, "/api/v1/projects/SHIP/deploy-runs/"+strconv.FormatInt(id, 10)+"?repo="+repo)
		var view projectDeployRunView
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &view); err != nil {
			t.Fatalf("decode run %d of %s: %v", id, repo, err)
		}
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Fatalf("decode run %d of %s's fields: %v", id, repo, err)
		}
		return view, fields
	}
	for _, want := range []struct {
		what     string
		id       int64
		repo     string
		deploy   prodevents.Event
		failures []prodevents.Event
	}{
		{"run 100 of SHIP's repository", 100, "acme/alpha", ship100, []prodevents.Event{flag, page}},
		{"run 101 of SHIP's repository", 101, "acme/alpha", ship101, []prodevents.Event{later}},
		{"run 300 of OTHR's repository, which shipped a SHIP pull request", 300, "acme/gamma", othr300, []prodevents.Event{shipped}},
	} {
		got, _ := read(want.id, want.repo)
		names := make([]string, len(got.Jobs))
		for i, job := range got.Jobs {
			names[i] = job.Name
		}
		if got.ID != want.id || !slices.Equal(names, []string{"build " + want.repo, "release"}) {
			t.Errorf("%s: SHIP's route answers run %d with jobs %v; want run %d with build %s and release", want.what, got.ID, names, want.id, want.repo)
		}
		if got.Deploy == nil || mustJSON(t, *got.Deploy) != mustJSON(t, want.deploy) {
			t.Errorf("%s: the deploy row answered is %s; want the adapter's row %s", want.what, mustJSON(t, got.Deploy), mustJSON(t, want.deploy))
		}
		if mustJSON(t, got.Failures) != mustJSON(t, want.failures) {
			t.Errorf("%s: the failures answered are %s; want the rows the writes left, %s", want.what, mustJSON(t, got.Failures), mustJSON(t, want.failures))
		}
	}

	// What each write left, read back from the deploy-runs answer: the fields the drill-down draws.
	record, _ := read(100, "acme/alpha")
	if len(record.Failures) != 2 {
		t.Fatalf("run 100 answers %d failures, want Alice's flag and the page", len(record.Failures))
	}
	alice, rejected := record.Failures[0], record.Failures[1]
	switch {
	case alice.ID != flag.ID || alice.ProjectKey != "SHIP" || alice.Signal == nil || *alice.Signal != prodevents.SignalPerson || alice.Actor.ID != "alice":
		t.Errorf("the first failure is %s of %s, signal %v by %s; want Alice's flag %s of SHIP, signal person", alice.ID, alice.ProjectKey, alice.Signal, alice.Actor.ID, flag.ID)
	case alice.Note != flagNote || !slices.Equal(alice.PullRequests, []string{"acme/alpha#1"}) || alice.DeployID == nil || *alice.DeployID != ship100.ID:
		t.Errorf("Alice's flag reads back the note %q, pull requests %v and deploy %v; want %q, acme/alpha#1 and %s", alice.Note, alice.PullRequests, alice.DeployID, flagNote, ship100.ID)
	case alice.State != prodevents.StateCounted || alice.RestoredAt == nil || alice.RestoredBy == nil || alice.RestoredBy.ID != "bob":
		t.Errorf("Alice's flag reads back state %s, restored at %v by %v; want counted, restored by bob", alice.State, alice.RestoredAt, alice.RestoredBy)
	}
	switch {
	case rejected.ID != page.ID || rejected.ProjectKey != "OTHR" || rejected.Signal == nil || *rejected.Signal != prodevents.SignalAPI:
		t.Errorf("the second failure is %s of %s, signal %v; want the page %s of OTHR, signal api", rejected.ID, rejected.ProjectKey, rejected.Signal, page.ID)
	case rejected.State != prodevents.StateRejected || rejected.StateReason == nil || *rejected.StateReason != reason || rejected.StateBy == nil || rejected.StateBy.ID != "bob" || rejected.StateAt == nil:
		t.Errorf("the page reads back state %s, reason %v by %v at %v; want rejected, %q, by bob", rejected.State, rejected.StateReason, rejected.StateBy, rejected.StateAt, reason)
	}
	other, _ := read(300, "acme/gamma")
	if other.Deploy == nil || other.Deploy.ProjectKey != "OTHR" || len(other.Failures) != 1 || other.Failures[0].ProjectKey != "OTHR" || other.Failures[0].Note != shippedNote {
		t.Errorf("run 300 of OTHR's repository answers on SHIP's route the record %s; want OTHR's deploy row and Bob's flag of OTHR with the note %q", mustJSON(t, other), shippedNote)
	}
	if _, none := read(102, "acme/alpha"); string(none["deploy"]) != "null" || string(none["failures"]) != "[]" {
		t.Errorf("run 102, of which no deploy row is written, answers the record deploy %s, failures %s; want null and []", none["deploy"], none["failures"])
	}
	t.Logf("run 100 answers its deploy row %s with Alice's restored flag (note %q) and OTHR's rejected page (%q); run 300 of OTHR's repository answers on SHIP's route OTHR's row %s with Bob's flag (note %q)",
		ship100.ID, alice.Note, *rejected.StateReason, othr300.ID, other.Failures[0].Note)
}
