package api

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
)

// oversizedUndeclared is a keyed write of method to path, as the caller credential names, whose
// body runs one byte past route's limit (routeBody) and declares no length, so the keyed layer
// finds it too large only as it spools it.
func oversizedUndeclared(t *testing.T, method, route, path string, credential http.Header, key string) *http.Request {
	t.Helper()
	limit, _ := routeBody(method, route)
	request := httptest.NewRequest(method, path, strings.NewReader(`"`+strings.Repeat("x", int(limit)-1)+`"`))
	request.ContentLength = -1
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(contracts.IdempotencyKeyHeader, key)
	maps.Copy(request.Header, credential)
	return request
}

// wantTooLarge fails unless answer is the route's 413, which names no key: the layer judged none.
func wantTooLarge(t *testing.T, what string, answer *httptest.ResponseRecorder) {
	t.Helper()
	if code, _ := refusalOf(t, answer); answer.Code != http.StatusRequestEntityTooLarge || code != "REQUEST_TOO_LARGE" {
		t.Errorf("%s: status=%d body=%.300s, want 413 REQUEST_TOO_LARGE", what, answer.Code, answer.Body.String())
	}
	if echoed := answer.Header().Get(contracts.IdempotencyKeyHeader); echoed != "" {
		t.Errorf("%s: the answer names the key %q back, want none: the layer judged no key", what, echoed)
	}
}

// The review of #4411 at c43631485c: a keyed write whose body declares no length and runs past its
// route's limit reached its handler as its caller, so a route that reads no body before its write
// made the write with no key claimed. Seven routes read none: the three revokes, which call the
// secrets broker; the comment reopen, whose refused commit failed the document's room, and with it
// other people's writes on that document; and the three architecture syncs, whose import another
// caller's sync of the source can join. Each is now answered the route's 413 at its caller lookup,
// as main answers it before its handler, and makes no write, no broker call and no import.
func TestAnUndeclaredBodyPastTheLimitReachesNoWrite(t *testing.T) {
	for _, revoke := range []struct{ route, path string }{
		{"/api/v1/credential-grants/{id}/revoke", "/api/v1/credential-grants/grant1/revoke"},
		{"/api/v1/machine-logins/{id}/revoke", "/api/v1/machine-logins/cred1/revoke"},
		{"/api/v1/signins/{id}/revoke", "/api/v1/signins/signin1/revoke"},
	} {
		t.Run("POST "+revoke.route, func(t *testing.T) {
			rig := newFakeBrokerRig(t)
			handler := newCredentialTestHandler(t, rig.server.URL)
			for attempt := range 2 {
				answer := httptest.NewRecorder()
				handler.ServeHTTP(answer, oversizedUndeclared(t, http.MethodPost, revoke.route, revoke.path, asUser("alice"), "revoke-1"))
				wantTooLarge(t, fmt.Sprintf("attempt %d", attempt+1), answer)
			}
			if rig.calls != 0 {
				t.Errorf("the broker was called %d times under one key, want none: the revoke ran with no key claimed", rig.calls)
			}
		})
	}

	t.Run("POST /api/v1/comments/{id}/reopen", func(t *testing.T) {
		handler, database, deps := newTestServer(t, testServerOptions{documentHooks: true})
		issue := createArtifactIssue(t, handler)
		artifact := createBeforeDocument(t, handler, issue.Key, "reopen.md")
		resolved := createAnchoredComment(t, handler, issue.Key, artifact.ID, "a resolved thread")
		open := createAnchoredComment(t, handler, issue.Key, artifact.ID, "an open thread")
		if answer := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+resolved.ID+"/resolve", nil, "alice"); answer.Code != http.StatusOK {
			t.Fatalf("resolve the thread: status=%d body=%s", answer.Code, answer.Body.String())
		}
		rooms := deps.Docs.(*docs.Service)
		before, err := rooms.RoomCounters(artifact.ID)
		if err != nil {
			t.Fatalf("read the document's room counters: %v", err)
		}

		answer := httptest.NewRecorder()
		handler.ServeHTTP(answer, oversizedUndeclared(t, http.MethodPost, "/api/v1/comments/{id}/reopen", "/api/v1/comments/"+resolved.ID+"/reopen", asUser("alice"), "reopen-1"))
		wantTooLarge(t, "the reopen", answer)
		// Another person's write on the document right after: a room the reopen failed refuses
		// every write on it, its 413 among them, until the room is evicted and loaded again.
		other := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+open.ID+"/resolve", nil, "bob")

		after, err := rooms.RoomCounters(artifact.ID)
		if err != nil {
			t.Fatalf("read the document's room counters: %v", err)
		}
		if failed := after.RoomFailures - before.RoomFailures; failed != 0 {
			t.Errorf("the reopen failed the document's room %d times, want none: it wrote the live document with no key claimed", failed)
		}
		if other.Code != http.StatusOK {
			t.Errorf("bob's resolve on the same document right after: status=%d body=%.300s, want 200", other.Code, other.Body.String())
		}
		var stillResolved bool
		if err := database.Pool.QueryRow(context.Background(), `select resolved from comments where id = $1`, resolved.ID).Scan(&stillResolved); err != nil {
			t.Fatalf("read the thread: %v", err)
		}
		if !stillResolved {
			t.Error("the thread was reopened by a write with no key claimed, want it resolved still")
		}
		if keys := countRequestKeys(t, database); keys != 0 {
			t.Errorf("%d key rows were kept, want none", keys)
		}
	})

	for _, sync := range []struct{ route, path string }{
		{"/api/v1/projects/{key}/architecture-sources/{owner}/{repo}/sync", sourcePath("CORE", "legion/arch") + "/sync"},
		{"/api/v1/projects/{key}/architecture-sources/sync", "/api/v1/projects/CORE/architecture-sources/sync"},
		{"/api/v1/projects/{key}/architecture-source/sync", "/api/v1/projects/CORE/architecture-source/sync"},
	} {
		t.Run("POST "+sync.route, func(t *testing.T) {
			fake := twoSourceFake()
			handler, database := newArchitectureSourceServer(t, fake)
			createTestProject(t, handler, "CORE")
			saved := putSource(t, handler, "CORE", "legion/arch", "main")
			fake.headReads = nil

			answer := httptest.NewRecorder()
			handler.ServeHTTP(answer, oversizedUndeclared(t, http.MethodPost, sync.route, sync.path, asUser("alice"), "sync-1"))
			wantTooLarge(t, "the sync", answer)
			if len(fake.headReads) != 0 {
				t.Errorf("the sync read %v from GitHub, want no import started: it ran with no key claimed, and another caller's sync of the source joins its run", fake.headReads)
			}
			if listed := listSources(t, handler, "CORE"); len(listed) != 1 || listed[0].String() != saved.String() {
				t.Errorf("the source reads %v after the sync, want it as it was saved: %v", sourceStrings(listed), saved)
			}
			if keys := countRequestKeys(t, database); keys != 0 {
				t.Errorf("%d key rows were kept, want none", keys)
			}
		})
	}
}
