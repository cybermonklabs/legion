package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/going"
)

// successorLinkOf is a Link value naming target as the successor-version.
func successorLinkOf(target string) string {
	return "<" + target + `>; rel="successor-version"`
}

// unfilledLinkOf is the Link of an answer that holds no value for its successor's path: /api/v2,
// with the successor's route in its title (departure 3 of the plan of dispatch://INFRA-4071).
func unfilledLinkOf(route string) string {
	return `</api/v2>; rel="successor-version"; title="` + route + `"`
}

// Every /api/v1 answer links the /api/v2 route that answers for it (R3.1): its path filled from
// the request's own path values, the successor's query where the v1 route became a filter of it
// (row 87), a binding's literal and the v1 value it names, a both-version row's own /api/v2 path,
// and the index's /api/v2; whether the answer is a success or a refusal, a route whose link needs
// nothing its handler resolves links its route. No /api/v2 answer carries a Link.
func TestAV1AnswerLinksTheV2RouteThatAnswersForIt(t *testing.T) {
	handler := newTestHandler(t)
	key := createTestIssue(t, handler, "LINK", "Linked")
	notes := uploadDocument(t, handler, "/api/v1/issues/"+key, "notes.md", "# Notes\n\nOne.\n").Artifact
	for _, test := range []struct{ what, method, target, want string }{
		{"a route of its own", http.MethodGet, "/api/v1/issues/" + key, "/api/v2/issues/" + key},
		{"a row that became a filter", http.MethodGet, "/api/v1/issues/" + key + "/artifacts/" + notes.Slug,
			"/api/v2/issues/" + key + "/artifacts?slug=" + url.QueryEscape(notes.Slug)},
		{"a row whose successor binds its path", http.MethodGet, "/api/v1/me/documents/" + notes.ID + "/read",
			"/api/v2/me/marks/document/" + notes.ID},
		{"a refusal of a route of its own", http.MethodGet, "/api/v1/issues/LINK-404", "/api/v2/issues/LINK-404"},
		{"a row the INFRA-979 tree serves in both versions", http.MethodGet, "/api/v1/settings/delivery", "/api/v2/settings/delivery"},
		{"the index", http.MethodGet, "/api/v1", "/api/v2"},
	} {
		response := dispatchRequest(t, handler, test.method, test.target, nil, "alice")
		if got := response.Header().Values("Link"); !slices.Equal(got, []string{successorLinkOf(test.want)}) {
			t.Errorf("%s: %s %s answered %d with Link %q, want %q", test.what, test.method, test.target, response.Code, got, successorLinkOf(test.want))
		}
	}
	if v2 := dispatchRequest(t, handler, http.MethodGet, "/api/v2/issues/"+key, nil, "alice"); v2.Code != http.StatusOK || v2.Header().Values("Link") != nil {
		t.Errorf("GET /api/v2/issues/%s answered %d with Link %q, want 200 and no Link", key, v2.Code, v2.Header().Values("Link"))
	}
}

// A slug row's successor is under the artifact the slug names, or the issue the document belongs
// to, which the v1 request does not carry: its handler records the value it resolved and the
// answer links that route, a keyed write's included. A request refused before the handler
// resolves one links /api/v2 and names the route in its title (departure 3).
func TestASlugRowLinksTheValueItsHandlerResolved(t *testing.T) {
	handler := newTestHandler(t)
	key := createTestIssue(t, handler, "SLUG", "Slugged")
	owner := "/api/v1/issues/" + key + "/artifacts/"
	plan := uploadDocument(t, handler, "/api/v1/issues/"+key, "rollout.md", "# Rollout\n\nOne.\n").Artifact

	read := dispatchRequest(t, handler, http.MethodGet, owner+plan.Slug+"/versions/1", nil, "alice")
	if want := successorLinkOf("/api/v2/artifacts/" + plan.ID + "/versions/1"); read.Code != http.StatusOK || read.Header().Get("Link") != want {
		t.Errorf("GET %s%s/versions/1 answered %d with Link %q, want 200 and %q", owner, plan.Slug, read.Code, read.Header().Get("Link"), want)
	}
	named := dispatchRequest(t, handler, http.MethodPost, owner+plan.Slug+"/versions", map[string]string{"summary": "First draft"}, "alice")
	if want := successorLinkOf("/api/v2/artifacts/" + plan.ID + "/versions"); named.Code != http.StatusCreated || named.Header().Get("Link") != want {
		t.Errorf("POST %s%s/versions answered %d %s with Link %q, want 201 and %q", owner, plan.Slug, named.Code, named.Body.String(), named.Header().Get("Link"), want)
	}

	missing := dispatchRequest(t, handler, http.MethodGet, owner+"no-such-document/versions/1", nil, "alice")
	if want := unfilledLinkOf("GET /api/v2/artifacts/{id}/versions/{number}"); missing.Code != http.StatusNotFound || missing.Header().Get("Link") != want {
		t.Errorf("GET %sno-such-document/versions/1 answered %d with Link %q, want 404 and %q", owner, missing.Code, missing.Header().Get("Link"), want)
	}
	// Row 17's keep refuses a body without its sum before it reads the document whose issue its
	// successor is under.
	keep := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+plan.ID+"/blocks/b-heading/keep", map[string]string{}, "alice")
	if want := unfilledLinkOf("PUT /api/v2/issues/{key}/decisions/{block_id}/state"); keep.Code != http.StatusBadRequest || keep.Header().Get("Link") != want {
		t.Errorf("POST /api/v1/artifacts/%s/blocks/b-heading/keep {} answered %d %s with Link %q, want 400 and %q", plan.ID, keep.Code, keep.Body.String(), keep.Header().Get("Link"), want)
	}
}

// A keyed write whose successor its handler resolves keeps that value with its stored answer, so
// a replay under its key, which runs no handler, links what the first answer linked rather than
// /api/v2 with the route in its title, and carries no other header than the first answer did.
func TestAKeyedReplayOfASlugWriteLinksWhatItsFirstAnswerLinked(t *testing.T) {
	handler := newTestHandler(t)
	key := createTestIssue(t, handler, "REPLAY", "Replayed")
	plan := uploadDocument(t, handler, "/api/v1/issues/"+key, "rollout.md", "# Rollout\n\nOne.\n").Artifact
	target := "/api/v1/issues/" + key + "/artifacts/" + plan.Slug + "/edits"
	edit := map[string]any{"ops": []map[string]string{{"op": "insert", "after": "end", "markdown": "Two."}}}
	idempotencyKey := uuid.NewString()
	first := keyedRequest(t, handler, http.MethodPost, target, edit, "alice", idempotencyKey)
	again := keyedRequest(t, handler, http.MethodPost, target, edit, "alice", idempotencyKey)
	want := []string{successorLinkOf("/api/v2/artifacts/" + plan.ID + "/edits")}
	if first.Code != http.StatusOK || !slices.Equal(first.Header().Values("Link"), want) {
		t.Fatalf("POST %s answered %d %s with Link %q, want 200 and %q", target, first.Code, first.Body.String(), first.Header().Values("Link"), want)
	}
	if !replayed(again) || again.Code != first.Code || again.Body.String() != first.Body.String() || !slices.Equal(again.Header().Values("Link"), want) {
		t.Fatalf("the same POST under its key answered %d with Link %q (replayed %v), want the first answer replayed with Link %q",
			again.Code, again.Header().Values("Link"), replayed(again), want)
	}
	for name := range again.Header() {
		if strings.HasPrefix(name, storedSuccessorValue) {
			t.Errorf("the replay carries the stored value %q, which no answer sends", name)
		}
	}
}

// Row 178, every project's architecture sources, answers what /api/v2 splits by project: one
// Link value per project its list holds, /api/v2/projects when it holds none, and a refusal names
// the route in its title, as any refusal that resolved nothing does.
func TestEveryProjectsSourcesLinkEachProjectTheyHold(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	const target = "/api/v1/settings/architecture-sources"

	none := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
	if want := []string{successorLinkOf("/api/v2/projects")}; none.Code != http.StatusOK || !slices.Equal(none.Header().Values("Link"), want) {
		t.Errorf("GET %s with no source answered %d %s with Link %q, want 200 and %q", target, none.Code, none.Body.String(), none.Header().Values("Link"), want)
	}
	for _, project := range []string{"CORE", "OPS"} {
		if created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": project, "name": project}, "alice"); created.Code != http.StatusCreated {
			t.Fatalf("create project %s: status=%d body=%s", project, created.Code, created.Body.String())
		}
	}
	if _, err := database.Pool.Exec(context.Background(), `
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by) values
			('CORE', 'acme/core', 'main', 1, '{"kind":"user","id":"alice"}'),
			('CORE', 'acme/infra', 'main', 1, '{"kind":"user","id":"alice"}'),
			('OPS', 'acme/ops', 'main', 1, '{"kind":"user","id":"alice"}')
	`); err != nil {
		t.Fatalf("add the sources: %v", err)
	}
	listed := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
	want := []string{
		successorLinkOf("/api/v2/projects/CORE/architecture-sources"),
		successorLinkOf("/api/v2/projects/OPS/architecture-sources"),
	}
	if listed.Code != http.StatusOK || !slices.Equal(listed.Header().Values("Link"), want) {
		t.Errorf("GET %s over CORE's two sources and OPS's one answered %d with Link %q, want 200 and %q", target, listed.Code, listed.Header().Values("Link"), want)
	}
	refused := dispatchRequest(t, handler, http.MethodGet, target, nil, "")
	if want := unfilledLinkOf("GET /api/v2/projects/{key}/architecture-sources"); refused.Code != http.StatusUnauthorized || refused.Header().Get("Link") != want {
		t.Errorf("GET %s with no credential answered %d with Link %q, want 401 and %q", target, refused.Code, refused.Header().Get("Link"), want)
	}
}

// The successor's writer forwards Flush: the v1 event stream flushes its head and every frame to
// the connection through it, and its Link is on the head it flushed.
func TestTheV1StreamFlushesThroughItsSuccessorLink(t *testing.T) {
	rig := newGoingRoutesRig(t, nil)
	ctx := context.Background()
	run, err := rig.server.recordGoingRequest(ctx, goingAsker, "", "who flushes this?")
	if err != nil {
		t.Fatalf("record the run: %v", err)
	}
	if _, err := rig.server.appendGoingEvent(ctx, run.ID, going.Text("Everybody.")); err != nil {
		t.Fatalf("append a frame: %v", err)
	}
	if _, err := rig.server.endGoingRun(ctx, run.ID, going.End(1, 0)); err != nil {
		t.Fatalf("end the run: %v", err)
	}

	sink := &goingStreamSink{header: http.Header{}, flushed: make(chan struct{}, 4)}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/going/requests/"+run.ID+"/stream", nil)
	request.Header.Set("X-Dispatch-User", "alice")
	rig.handler.ServeHTTP(sink, request)
	select {
	case <-sink.flushed:
	case <-time.After(time.Second):
		t.Fatal("the stream never flushed the connection through the /api/v1 wrapper")
	}
	if !strings.Contains(sink.text(), `data: {"text":"Everybody."}`) {
		t.Errorf("the stream wrote %q, want its frame", sink.text())
	}
	if got, want := sink.header.Values("Link"), []string{successorLinkOf("/api/v2/going-requests/" + run.ID + "/stream")}; !slices.Equal(got, want) {
		t.Errorf("the stream's head carries Link %q, want %q", got, want)
	}
}
