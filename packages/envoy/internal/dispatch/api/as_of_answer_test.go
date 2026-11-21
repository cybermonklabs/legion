package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/asof"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestAPastTimeAnswerNamesItsCutAndItsAbsentFields: an issue read at a cut names its cut in
// Dispatch-As-Of, and in Dispatch-As-Of-Absent exactly the members the answer leaves out, each
// once by its pointer in asof.AbsentFields, and each of them is omitted from the answer, never
// written null. The issue has a route and a session holds it, so the read leaves out the route's
// reach and the holder's liveness, which only the route roster gives, now; it leaves out its
// design gates and its backlink count, which keep no history; and of its two open asks, the one
// answered after the cut leaves out its revision and its options' part ids, which the present
// tables give as they stand after its answer, while the one that did not change gives both.
// Progress counts the task items of the spec's version at the cut, not those of the version a
// write after the cut stored a count for (decision 9). The present read gives every one of those
// members, so the test cannot pass on an answer that leaves out nothing. A refusal at the cut, of
// an issue no record holds, carries no Dispatch-As-Of: only an answer names the cut.
//
// And a member that AbsentFields does not hold for the route makes the writer refuse the answer:
// in a test build the handler panics, naming the member, rather than name a member the contract
// does not hold.
func TestAPastTimeAnswerNamesItsCutAndItsAbsentFields(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: newAsOfListener(t), testHooks: true})
	settle := func() {
		t.Helper()
		asOfWrite(t, handler, http.MethodPost, "/api/v1/artifacts/_test/quiesce", nil, "alice")
		awaitBackgroundWrites(t, served)
		safeHorizon(t, served)
	}
	const project = "ASOA"
	asOfWrite(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": project, "name": "The answer at a cut"}, "alice")
	issue := asOfIssue(t, handler, map[string]any{
		"project": project, "title": "Name the cut",
		"spec": "# Spec\n\n- [x] Read the past.\n- [ ] Name the cut.\n",
	})
	asOfWrite(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{"route": "role:reviewer"}, "alice")
	asOfSessionWrite(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/claim", map[string]any{})
	options := []map[string]string{{"label": "Yes"}, {"label": "No"}}
	answered := decodeBody[model.Ask](t, asOfSessionWrite(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks",
		map[string]any{"question": "Is this ask answered after the cut?", "options": options})).ID
	kept := decodeBody[model.Ask](t, asOfSessionWrite(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks",
		map[string]any{"question": "Does this ask stay as it was?", "options": options})).ID
	settle()
	cut := databaseCut(t, database)
	time.Sleep(5 * time.Millisecond)
	asOfWrite(t, handler, http.MethodPost, "/api/v1/asks/"+answered+"/answer", map[string]any{"selected": []string{"Yes"}}, "alice")
	asOfWrite(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops":     []map[string]string{{"op": "insert", "after": "end", "markdown": "- [x] A task the spec gained after the cut.\n"}},
		"summary": "The spec after the cut",
	}, "alice")
	settle()

	leftOut := []string{"route_status", "route_holder", "route_status_reason", "holder_state", "last_seen", "takeable_at", "design_gates", "referenced_by_count"}
	present := decodeBody[map[string]any](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key, nil, "alice"))
	for _, member := range leftOut {
		if _, given := present[member]; !given {
			t.Fatalf("the present read gives no %s, so its absence at the cut proves nothing: %v", member, present)
		}
	}
	if tasks := progressTasks(present); tasks != "2/3" {
		t.Fatalf("the present read's progress.tasks is %s, want 2/3: the spec's newest version", tasks)
	}

	response := dispatchRequest(t, handler, http.MethodGet, withAsOf(t, "/api/v1/issues/"+issue.Key, cut), nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("the issue read at the cut: status=%d body=%s", response.Code, response.Body.String())
	}
	if named := response.Header().Get(contracts.DispatchAsOfHeader); named != cut.String() {
		t.Errorf("%s = %q, want the cut %s", contracts.DispatchAsOfHeader, named, cut)
	}
	want := asof.HeaderValue([]string{
		"/route_status", "/route_holder", "/route_status_reason", "/holder_state", "/last_seen", "/takeable_at",
		"/design_gates", "/referenced_by_count",
		"/open_asks/*/referenced_by_count", "/open_asks/*/revision", "/open_asks/*/options/*/id",
	})
	if named := response.Header().Get(contracts.DispatchAsOfAbsentHeader); named != want {
		t.Errorf("%s = %q, want %q", contracts.DispatchAsOfAbsentHeader, named, want)
	}

	read := decodeBody[map[string]any](t, response)
	for _, member := range leftOut {
		if value, given := read[member]; given {
			t.Errorf("the answer at the cut gives %s as %v: a member it leaves out is omitted, never written", member, value)
		}
	}
	if tasks := progressTasks(read); tasks != "1/2" {
		t.Errorf("progress.tasks at the cut is %s, want 1/2: the task items of the spec's version at the cut", tasks)
	}
	components, _ := read["components"].(map[string]any)
	if _, ids := components["ids"]; !ids {
		t.Errorf("components at the cut is %v, want its ids: an attachment that is not explicit holds no member at any time", components)
	}
	asks := map[string]map[string]any{}
	openAsks, _ := read["open_asks"].([]any)
	for _, element := range openAsks {
		ask, _ := element.(map[string]any)
		id, _ := ask["id"].(string)
		asks[id] = ask
	}
	if len(asks) != 2 || asks[answered] == nil || asks[kept] == nil {
		t.Fatalf("the open asks at the cut are %v, want the two asks open then, %s and %s", read["open_asks"], answered, kept)
	}
	for id, ask := range asks {
		if value, given := ask["referenced_by_count"]; given {
			t.Errorf("open ask %s gives referenced_by_count %v at the cut: the reference graph keeps no history", id, value)
		}
		_, revision := ask["revision"]
		ids := 0
		askOptions, _ := ask["options"].([]any)
		for _, element := range askOptions {
			if option, _ := element.(map[string]any); option["id"] != nil {
				ids++
			}
		}
		switch {
		case id == answered && (revision || ids > 0):
			t.Errorf("open ask %s, answered after the cut, gives its revision (%t) or %d option part ids at the cut: its parts are the present's", id, revision, ids)
		case id == kept && (!revision || ids != len(askOptions)):
			t.Errorf("open ask %s, unchanged since the cut, gives revision %t and %d of %d option part ids at the cut, want them all", id, revision, ids, len(askOptions))
		}
	}

	missing := dispatchRequest(t, handler, http.MethodGet, withAsOf(t, "/api/v1/issues/"+project+"-99", cut), nil, "alice")
	if missing.Code != http.StatusNotFound || missing.Header().Get(contracts.DispatchAsOfHeader) != "" {
		t.Errorf("a missing issue at the cut answers %d with %s %q, want 404 and no header: a refusal answers nothing as of the cut",
			missing.Code, contracts.DispatchAsOfHeader, missing.Header().Get(contracts.DispatchAsOfHeader))
	}

	t.Run("a member AbsentFields does not hold", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/api/v1/issues/"+issue.Key, nil)
		refusal := func() (refused any) {
			defer func() { refused = recover() }()
			serveAtCut(recorder, request, "GET /api/v1/issues/{key}", cut, func(w http.ResponseWriter, r *http.Request) {
				leaveOut(r.Context(), "/route_status", "/title")
				WriteJSON(w, http.StatusOK, map[string]string{"title": "Name the cut"})
			})
			return nil
		}()
		if refusal == nil || !strings.Contains(fmt.Sprint(refusal), "/title") {
			t.Fatalf("the writer took an answer that leaves out /title, which AbsentFields does not hold for the route (refusal %v)", refusal)
		}
		if named := recorder.Header().Get(contracts.DispatchAsOfAbsentHeader); named != "" {
			t.Errorf("the refused answer names %q absent", named)
		}
	})
}

// TestAPastTimeReadStoresNoDisplayTrees: a read at a cut with Dispatch-Trees draws the tree of
// each text it answers that no row stores, as the present read does, but it stores none of them.
// The store binds each statement under a cut and refuses a write (asof.Bind), so a store of the
// trees under the cut fails each time and logs a warning; the read stores nothing and logs
// nothing. A tree is keyed by its text alone, so the present read stores it.
func TestAPastTimeReadStoresNoDisplayTrees(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	asOfWrite(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "ASTR", "name": "Trees at a cut"}, "alice")
	issue := asOfIssue(t, handler, map[string]any{"project": "ASTR", "title": "A thread read with its trees"})
	asOfWrite(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{"body": "A comment whose **tree** no read stored."}, "alice")
	settleServed(t, served)
	cut := databaseCut(t, database)
	// The comment's write stored its tree; the read at the cut finds none stored.
	if _, err := database.Pool.Exec(t.Context(), `delete from text_trees`); err != nil {
		t.Fatalf("forget the stored trees: %v", err)
	}

	logs := captureLogs(t)
	target := "/api/v1/issues/" + issue.Key + "/comments"
	answer := treesRequest(t, handler, withAsOf(t, target, cut), true)
	if _, drawn := valueAt(t, answer, "0.body_tree"); !drawn {
		t.Fatalf("GET %s as of the cut with Dispatch-Trees draws no tree: %s", target, answer.Body.String())
	}
	served.detached.wait()
	if strings.Contains(logs.String(), "store display trees failed") {
		t.Errorf("the read at the cut tried to store its display trees:\n%s", logs.String())
	}
}

// progressTasks is an issue read's progress.tasks as done/total, or null.
func progressTasks(read map[string]any) string {
	progress, _ := read["progress"].(map[string]any)
	tasks, _ := progress["tasks"].(map[string]any)
	if tasks == nil {
		return "null"
	}
	return fmt.Sprintf("%v/%v", tasks["done"], tasks["total"])
}
