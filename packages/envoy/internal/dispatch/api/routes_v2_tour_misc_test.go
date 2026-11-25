package api

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/cireconcile"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func init() { addTourSteps("misc", miscTourSteps) }

// miscTourSteps are the route tour's steps of the other routes: each route, a search
// over several projects within a window beside the plain one, and a page of the going actions
// beside the whole catalogue.
func miscTourSteps() []tourStep {
	alice := tourPerson("alice")
	asAlice := func(query url.Values) func(*testing.T, *routeTour) tourRequest {
		return func(*testing.T, *routeTour) tourRequest { return tourRequest{Query: query, As: alice} }
	}
	groupMissing := []tourMissing{{Param: "id", Value: uuid.NewString(), Code: apicode.SessionGroupNotFound}}
	onGroup := func(body any) func(*testing.T, *routeTour) tourRequest {
		return func(t *testing.T, tour *routeTour) tourRequest {
			return tourRequest{Path: map[string]string{"id": tour.sessionGroup(t)}, Body: body, As: alice}
		}
	}
	window := func() (string, string) {
		return time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
	}
	return []tourStep{
		{
			Method: http.MethodGet, Pattern: "/agents", Status: http.StatusOK,
			Setup: asAlice(url.Values{"limit": {"5"}}),
		},
		{
			Method: http.MethodGet, Pattern: "/agents/{session_id}/answers", Status: http.StatusOK,
			Setup: func(*testing.T, *routeTour) tourRequest {
				from, to := window()
				return tourRequest{
					Path:  map[string]string{"session_id": tourRecipient},
					Query: url.Values{"from": {from}, "to": {to}, "max_chars": {"1000"}, "limit": {"10"}},
					As:    alice,
				}
			},
		},
		{
			// The tour's recipient session and one no record names, which is answered no row.
			Method: http.MethodGet, Pattern: "/known-sessions", Status: http.StatusOK,
			Setup: asAlice(url.Values{"id": {tourRecipient, "session-nobody-recorded"}}),
		},
		{
			Method: http.MethodGet, Pattern: "/block-links", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				spec := "dispatch://" + tour.issue(t).Key + "/spec"
				return tourRequest{Query: url.Values{"from": {spec}, "to": {spec}}, As: alice}
			},
		},
		{
			// One reference that names the toured issue and one that names nothing, which is
			// answered gone rather than refused; the path names no stored object.
			Method: http.MethodGet, Pattern: "/reference-targets", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				issue := "dispatch://" + tour.issue(t).Key
				return tourRequest{Query: url.Values{"ref": {issue, "dispatch://NOPE-1"}}, As: alice}
			},
		},
		{
			Method: http.MethodGet, Pattern: "/block-schema", Status: http.StatusOK,
			Setup: func(*testing.T, *routeTour) tourRequest { return tourRequest{} },
		},
		{
			Method: http.MethodGet, Pattern: "/search-results", Status: http.StatusOK,
			Setup: asAlice(url.Values{"q": {"toured issue"}}),
		},
		{
			Method: http.MethodGet, Pattern: "/search-results", Name: "narrowed to two kinds", Status: http.StatusOK,
			Setup: asAlice(url.Values{"q": {"toured issue"}, "kind": {"issue", "document"}}),
		},
		{
			Method: http.MethodGet, Pattern: "/search-results", Name: "several projects within a window", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				from, to := window()
				return tourRequest{Query: url.Values{
					"q": {"toured issue"}, "project": {tour.project(t), tour.project(t)}, "from": {from}, "to": {to},
				}, As: alice}
			},
		},
		{
			// One person's nodes: alice's comment on a toured issue is one of them.
			Method: http.MethodGet, Pattern: "/nodes", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				tour.comment(t, tour.issue(t))
				return tourRequest{Query: url.Values{"person": {"alice"}}, As: alice}
			},
		},
		{
			Method: http.MethodGet, Pattern: "/search-coverage", Status: http.StatusOK,
			Setup: asAlice(nil),
		},
		{
			Method: http.MethodGet, Pattern: "/going-actions", Status: http.StatusOK,
			Setup: asAlice(nil),
		},
		{
			Method: http.MethodGet, Pattern: "/going-actions", Name: "a page of it", Status: http.StatusOK,
			Setup: asAlice(url.Values{"limit": {"2"}, "offset": {"1"}}),
		},
		{
			Method: http.MethodPost, Pattern: "/going-requests", Status: http.StatusCreated,
			Setup: func(*testing.T, *routeTour) tourRequest {
				return tourRequest{Body: map[string]any{"words": "Which way does the tour go?"}, As: alice}
			},
		},
		{
			Method: http.MethodGet, Pattern: "/going-requests/{id}/stream", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				return tourRequest{Path: map[string]string{"id": tour.goingRequest(t)}, As: alice}
			},
			Missing: []tourMissing{{Param: "id", Value: uuid.NewString(), Code: apicode.GoingRequestNotFound}},
		},
		{
			Method: http.MethodGet, Pattern: "/limit-set", Status: http.StatusOK,
			Setup: asAlice(nil),
		},
		{
			Method: http.MethodGet, Pattern: "/users", Status: http.StatusOK,
			Setup: asAlice(url.Values{"limit": {"5"}}),
		},
		{
			Method: http.MethodGet, Pattern: "/dashboard-builds", Status: http.StatusOK,
			Setup: asAlice(nil),
		},
		{
			Method: http.MethodPost, Pattern: "/session-groups", Status: http.StatusCreated,
			Setup: func(*testing.T, *routeTour) tourRequest {
				return tourRequest{Body: map[string]any{
					"name": "tour group " + uuid.NewString(), "sessions": []string{"session-tour-" + uuid.NewString()},
				}, As: alice}
			},
		},
		{
			Method: http.MethodDelete, Pattern: "/session-groups/{id}", Status: http.StatusNoContent,
			Setup: onGroup(nil), Missing: groupMissing,
		},
		{
			Method: http.MethodPatch, Pattern: "/session-groups/{id}", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				return onGroup(map[string]any{"name": "tour group " + uuid.NewString()})(t, tour)
			},
			Missing: groupMissing,
		},
		{
			Method: http.MethodGet, Pattern: "/deploy-runs/{id}", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				return tourRequest{Path: map[string]string{"id": tour.deployRun(t)}, As: alice}
			},
		},
		{
			Method: http.MethodPost, Pattern: "/markdown-round-trips", Status: http.StatusOK,
			Setup: func(*testing.T, *routeTour) tourRequest {
				return tourRequest{Body: map[string]any{"markdown": "1. one\n1. two\n"}, As: alice}
			},
		},
		{
			Method: http.MethodGet, Pattern: "/ci-reconcile-passes", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				endedCIPass(t, tour.database, uuid.NewString(), time.Minute, "complete")
				return tourRequest{Query: url.Values{"limit": {"5"}}, As: alice}
			},
		},
	}
}

// goingRequest asks a question as alice and answers its run's id once the run has stored its
// ending: the tour's server configures no model, so the run stores its error frame at once, and a
// stream of it reads that frame and closes.
func (tour *routeTour) goingRequest(t *testing.T) string {
	t.Helper()
	created := dispatchRequest(t, tour.handler, http.MethodPost, "/api/v1/going/requests",
		map[string]any{"words": "Where does the tour end?"}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("ask a question: %d %s", created.Code, created.Body.String())
	}
	awaitBackgroundWrites(t, tour.served)
	return decodeBody[goingRequest](t, created).ID
}

// sessionGroup makes a Delivery session group of one session of its own, answering its id.
func (tour *routeTour) sessionGroup(t *testing.T) string {
	t.Helper()
	created := dispatchRequest(t, tour.handler, http.MethodPost, "/api/v1/delivery/session-groups", map[string]any{
		"name": "tour group " + uuid.NewString(), "sessions": []string{"session-tour-" + uuid.NewString()},
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create a session group: %d %s", created.Code, created.Body.String())
	}
	return decodeBody[delivery.SessionGroup](t, created).ID
}

// tourDeployRun is the deploy-repository run deployRun records.
const tourDeployRun = 7

// deployRun configures the deployment's delivery settings and records one run of its deploy
// repository with a job, answering the run's id.
func (tour *routeTour) deployRun(t *testing.T) string {
	t.Helper()
	ctx := t.Context()
	if _, err := delivery.PutSettings(ctx, tour.database.Pool, delivery.DeliverySettings{
		DeployRepo: "acme/widgets", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobNames: []string{"release"}, PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: []string{"octocat"},
	}, model.Actor{Kind: "system", ID: "tour"}); err != nil {
		t.Fatalf("configure delivery: %v", err)
	}
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if err := delivery.UpsertRun(ctx, tour.database.Pool, delivery.DeliveryRun{
		Repo: "acme/widgets", RunID: tourDeployRun, Kind: delivery.DeliveryRunKindDeploy, HeadSHA: "abc",
		HeadCommitAt: start, StartedAt: start, URL: "https://github.com/acme/widgets/actions/runs/7",
		HeadBranch: new("main"), Event: new("push"),
	}); err != nil {
		t.Fatalf("record a deploy run: %v", err)
	}
	if err := delivery.UpsertRunJobs(ctx, tour.database.Pool, "acme/widgets", tourDeployRun, []delivery.DeliveryRunJob{{
		Repo: "acme/widgets", RunID: tourDeployRun, Name: "release", StartedAt: new(start),
		CompletedAt: new(start.Add(time.Minute)), Conclusion: new(delivery.DeliveryJobConclusionSuccess),
	}}, true); err != nil {
		t.Fatalf("record the run's job: %v", err)
	}
	return "7"
}

// searchedIssues is the keys of the issues a search answered, sorted.
func searchedIssues(results []model.SearchResult) []string {
	var keys []string
	for _, result := range results {
		if result.Kind == "issue" {
			keys = append(keys, result.ID)
		}
	}
	slices.Sort(keys)
	return keys
}

// GET /api/v2/search-results narrows every leg to each project project= names, repeated, and to
// the items written within [from, to): a search of two projects of three answers the issues of
// those two, echoing both; one of a window that ends before the issues were written answers none;
// and one project alone answers that project's issue, as /api/v1's one-project search does.
func TestTheV2SearchNarrowsToTheProjectsAndTheWindowItNames(t *testing.T) {
	handler := newTestHandler(t)
	var keys []string
	for _, project := range []string{"SEVA", "SEVB", "SEVC"} {
		keys = append(keys, createInteractionIssue(t, handler, project, "Zebraquill "+project, "Body text.").Key)
	}
	search := func(query url.Values) searchResultsV2 {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v2/search-results?"+query.Encode(), nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("search %v: %d %s", query, response.Code, response.Body.String())
		}
		return decodeBody[searchResultsV2](t, response)
	}
	two := search(url.Values{"q": {"zebraquill"}, "project": {"SEVA", "SEVB"}})
	if got := searchedIssues(two.Items); !slices.Equal(got, keys[:2]) || !slices.Equal(two.Filters.Project, []string{"SEVA", "SEVB"}) {
		t.Fatalf("a search of SEVA and SEVB answered %v, filters %+v; want %v", got, two.Filters, keys[:2])
	}
	earlier := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	before := search(url.Values{"q": {"zebraquill"}, "to": {earlier}})
	if before.Total != 0 || len(before.Items) != 0 || before.Filters.To == "" {
		t.Fatalf("a search of a window ending an hour ago answered %d of %d, filters %+v; want none", len(before.Items), before.Total, before.Filters)
	}
	within := search(url.Values{"q": {"zebraquill"}, "from": {earlier}, "to": {time.Now().Add(time.Minute).UTC().Format(time.RFC3339)}})
	if got := searchedIssues(within.Items); !slices.Equal(got, keys) {
		t.Fatalf("a search of the window the issues were written in answered %v, want %v", got, keys)
	}
	one := search(url.Values{"q": {"zebraquill"}, "project": {"SEVC"}})
	if got := searchedIssues(one.Items); !slices.Equal(got, keys[2:]) || !slices.Equal(one.Filters.Project, []string{"SEVC"}) {
		t.Fatalf("a search of SEVC answered %v, filters %+v; want %v", got, one.Filters, keys[2:])
	}
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v2/search-results?q=zebraquill&from="+url.QueryEscape(earlier)+
		"&to="+url.QueryEscape(earlier), nil, "alice")
	if code, _ := refusalOf(t, response); response.Code != http.StatusBadRequest || code != "INVALID_QUERY" {
		t.Fatalf("a from not before to answered %d %s, want 400 INVALID_QUERY", response.Code, response.Body.String())
	}
}

// The two meaning statements of a search of several projects keep the items of those projects
// and no other: each meaning leg, on the candidate row's own column and on the owner each arm
// joins, answers the issues of the two projects named out of three whose vectors all stand near
// the query.
func TestTheSeveralProjectMeaningStatementsKeepTheProjectsNamed(t *testing.T) {
	const spec = "Body text."
	titles := map[string]string{"MSVA": "Astrolabe calibration", "MSVB": "Sextant calibration", "MSVC": "Quadrant calibration"}
	vectors := map[string][]float32{}
	axis := 2
	for _, title := range titles {
		vectors[issueEmbeddingText(title, spec)] = angledVector(0.9, axis)
		axis++
	}
	embedder := &fakeEmbedder{vectors: vectors}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	keys := map[string]string{}
	for project, title := range titles {
		keys[project] = createInteractionIssue(t, handler, project, title, spec).Key
	}
	processAllPending(t, database, embedder)
	want := []string{keys["MSVA"], keys["MSVB"]}
	slices.Sort(want)
	parameters := searchParameters{text: "unmatched words", projects: []string{"MSVA", "MSVB"}, limit: searchDefaultLimit}
	for name, statement := range map[string]string{"column": searchQueryMeaningSeveral, "joined": searchQueryMeaningSeveralJoined} {
		rows, err := database.Pool.Query(context.Background(), statement,
			searchMeaningQueryArguments(parameters, queryVector(), embedder.Identity())...)
		if err != nil {
			t.Fatalf("%s: run the statement: %v", name, err)
		}
		found, err := scanSearchRows(rows, parameters.text, parameters.limit, 0, false)
		rows.Close()
		if err != nil {
			t.Fatalf("%s: read the rows: %v", name, err)
		}
		if got := searchedIssues(found.Results); !slices.Equal(got, want) {
			t.Errorf("the %s statement over MSVA and MSVB answered %v, want %v", name, got, want)
		}
	}
}

// GET /api/v2/ci-reconcile-passes answers the passes that started within [from, to), and reads a
// pass with no end as running only while it is the newest pass of all, inside the window or not:
// an open pass a newer one has overtaken reads ended_without_completing in a window that ends
// before the newer began, though its process is live and the lease is held. With neither bound it
// reads the last 24 hours, as /api/v1 does.
func TestTheV2CIReconcilePassesReadTheirWindowAndTheTablesNewestPass(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	instance := seedServerProcess(t, database, 0)
	overtaken := openCIPass(t, database, instance, 26*time.Hour)
	newest := openCIPass(t, database, instance, time.Minute)
	holdCIReconcileLease(t, database)
	read := func(target string) page[ciPassView] {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", target, response.Code, response.Body.String())
		}
		return decodeBody[page[ciPassView]](t, response)
	}
	from := url.QueryEscape(time.Now().Add(-27 * time.Hour).UTC().Format(time.RFC3339))
	to := url.QueryEscape(time.Now().Add(-25 * time.Hour).UTC().Format(time.RFC3339))
	earlier := read("/api/v2/ci-reconcile-passes?from=" + from + "&to=" + to)
	if earlier.Total != 1 || len(earlier.Items) != 1 || earlier.Items[0].ID != overtaken ||
		earlier.Items[0].State != string(cireconcile.PassStateAbandoned) {
		t.Fatalf("the window 27 to 25 hours ago answered %+v, want pass %d alone, %s", earlier, overtaken, cireconcile.PassStateAbandoned)
	}
	recent := read("/api/v2/ci-reconcile-passes")
	if recent.Total != 1 || len(recent.Items) != 1 || recent.Items[0].ID != newest ||
		recent.Items[0].State != string(cireconcile.PassStateRunning) {
		t.Fatalf("the last 24 hours answered %+v, want pass %d alone, %s", recent, newest, cireconcile.PassStateRunning)
	}
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v2/ci-reconcile-passes?from="+to+"&to="+from, nil, "alice")
	if code, _ := refusalOf(t, response); response.Code != http.StatusBadRequest || code != "INVALID_QUERY" {
		t.Fatalf("a from after to answered %d %s, want 400 INVALID_QUERY", response.Code, response.Body.String())
	}
}
