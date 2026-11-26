package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A board move takes the owner locks of its rank neighbours with every other issue it names, in one
// lock order (store.LockOwners), so while it waits for its gated anchor it holds its other
// neighbour, which sorts first. A status write on that neighbour waits for the move and finishes
// once the gate opens; a dependency write naming the neighbour finishes while the move still waits.
func TestBoardMoveStatusAndDependencyWriteDoNotDeadlock(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{settle: stagedAhead})
	keys := createListedIssues(t, handler, "CORE", 6)
	anchor, parent, moving, neighbor, dependent := keys[1], keys[2], keys[3], keys[4], keys[5]
	requireOwnerLockOrder(t, database, neighbor, anchor)
	if _, err := database.Pool.Exec(context.Background(), `
		update issues set parent_key = $1 where key = any($2::text[])
	`, parent, []string{moving, neighbor}); err != nil {
		t.Fatal(err)
	}
	gate, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	if err := store.LockOwner(context.Background(), gate, store.IssueOwner(anchor)); err != nil {
		t.Fatal(err)
	}
	moves := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		moves <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+moving, map[string]any{
			"rank": map[string]string{"after": anchor, "before": neighbor}, "status": "todo",
		}, "alice")
	}()
	waitForDatabaseLocks(t, gate, 1)
	statusWrites := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		statusWrites <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+neighbor, map[string]string{"status": "todo"}, "alice")
	}()
	waitForDatabaseLocks(t, gate, 2)
	dependencyWrites := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		dependencyWrites <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+dependent, map[string]any{"blocked_by": []string{neighbor}}, "alice")
	}()
	if response := awaitResponse(t, dependencyWrites); response.Code != http.StatusOK {
		t.Fatalf("dependency write beside a waiting board move: %d %s", response.Code, response.Body.String())
	}
	if err := gate.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if response := awaitResponse(t, moves); response.Code != http.StatusOK || decodeBody[model.Issue](t, response).Status != "todo" {
		t.Fatalf("board move: %d %s", response.Code, response.Body.String())
	}
	if response := awaitResponse(t, statusWrites); response.Code != http.StatusOK || decodeBody[model.Issue](t, response).Status != "todo" {
		t.Fatalf("status write on the board move's neighbour: %d %s", response.Code, response.Body.String())
	}
}

func TestDependencyWaitDoesNotHoldIssueRankOrNumberLocks(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{settle: stagedAhead})
	keys := createListedIssues(t, handler, "CORE", 3)
	other := createListedIssues(t, handler, "SIDE", 2)
	gate, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	if err := lockProjectIssueDependencies(context.Background(), gate, "CORE"); err != nil {
		t.Fatal(err)
	}
	waiting := make(chan *httptest.ResponseRecorder, 2)
	go func() {
		waiting <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+keys[0], map[string]any{
			"blocked_by": []string{keys[1]}, "rank": map[string]string{"after": keys[1]},
		}, "alice")
	}()
	go func() {
		waiting <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "CORE", "title": "Waiting dependent", "blocked_by": []string{keys[1]}, "force": true,
		}, "alice")
	}()
	waitForDatabaseLocks(t, gate, 2)
	for _, write := range []struct {
		method string
		path   string
		body   map[string]any
		status int
	}{
		{http.MethodPatch, "/api/v1/issues/" + keys[0], map[string]any{"status": "todo"}, http.StatusOK},
		{http.MethodPatch, "/api/v1/issues/" + keys[2], map[string]any{"rank": map[string]string{"after": keys[1]}}, http.StatusOK},
		{http.MethodPost, "/api/v1/issues", map[string]any{"project": "CORE", "title": "Independent root", "force": true}, http.StatusCreated},
		{http.MethodPatch, "/api/v1/issues/" + other[0], map[string]any{"blocked_by": []string{other[1]}}, http.StatusOK},
	} {
		responses := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			responses <- dispatchRequest(t, handler, write.method, write.path, write.body, "alice")
		}()
		response := awaitResponse(t, responses)
		if response.Code != write.status {
			t.Fatalf("unrelated write during dependency wait: %s %s: %d %s", write.method, write.path, response.Code, response.Body.String())
		}
	}
	if err := gate.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		response := awaitResponse(t, waiting)
		if response.Code != http.StatusOK && response.Code != http.StatusCreated {
			t.Fatalf("dependency write after release: %d %s", response.Code, response.Body.String())
		}
		if got := decodeBody[model.Issue](t, response).BlockedBy; !slices.Equal(got, []string{keys[1]}) {
			t.Fatalf("dependency write lost its target: %v", got)
		}
	}
}
