package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The rebuild route is a POST, so the write limit stands in front of it as in front of every
// route that is not a GET (register, holdWriteLimit): with every slot of the limit held, a
// rebuild is answered 503 SERVICE_BUSY at its entry, before it resolves its document or opens
// anything, and the document's history is as it was. Once the slots are given back the same
// rebuild reaches the document service, which refuses a history that loads 409 DOCUMENT_LOADS.
//
// The entry is told from the transaction by when the refusal comes. A route mounted outside the
// limit reaches its transaction (server.begin), which waits for a slot until the server's bound
// on a caller's waits (docs.WriteWaitBound) and is then refused the same 503; the entry refuses
// at once. So the request carries a context that ends, as a caller's does, which is what that
// bound is measured on, and the refusal must come before it.
func TestARebuildIsShedAtItsEntryWhileEveryWriteSlotIsHeld(t *testing.T) {
	ctx := context.Background()
	handler, database, deps, served := newRegisteredTestServer(t, testServerOptions{settle: time.Hour})
	issue := createArtifactIssue(t, handler)
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "healthy.md", "content": "before\n",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create the document: status=%d body=%s", created.Code, created.Body.String())
	}
	artifact := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, created).Artifact
	documentService := deps.Docs.(*docs.Service)
	if err := documentService.Evict(ctx, artifact.ID); err != nil {
		t.Fatalf("evict the document: %v", err)
	}
	updates := func() int { return storedUpdateCount(t, database, artifact.ID) }
	before := updates()
	rebuild := func() *httptest.ResponseRecorder {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/artifacts/"+artifact.ID+"/rebuild", strings.NewReader("{}"))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Dispatch-User", "alice")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	slots := served.writes.semaphore()
	if !slots.TryAcquire(apiWriteLimit) {
		t.Fatalf("the limit had fewer than its %d slots free before the test took them", apiWriteLimit)
	}
	release := sync.OnceFunc(func() { slots.Release(apiWriteLimit) })
	t.Cleanup(release)

	started := time.Now()
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- rebuild() }()
	var busy *httptest.ResponseRecorder
	select {
	case busy = <-answered:
	case <-time.After(2 * docs.WriteWaitBound):
		release()
		late := <-answered
		t.Fatalf("the rebuild was not answered in %s with every slot held; once they were given back it answered %d %s",
			2*docs.WriteWaitBound, late.Code, late.Body.String())
	}
	elapsed := time.Since(started)
	t.Logf("with all %d slots held, the rebuild was answered %d %s after %s",
		apiWriteLimit, busy.Code, strings.TrimSpace(busy.Body.String()), elapsed.Round(time.Millisecond))
	if busy.Code != http.StatusServiceUnavailable || refusalCode(busy.Body.Bytes()) != "SERVICE_BUSY" {
		t.Fatalf("the rebuild with every slot held was answered %d %s, want 503 SERVICE_BUSY", busy.Code, busy.Body.String())
	}
	if elapsed >= docs.WriteWaitBound {
		t.Fatalf("the rebuild was refused %s in, past the %s a caller's wait for a slot is bounded by: it waited at its transaction, where the entry sheds it at once",
			elapsed.Round(time.Millisecond), docs.WriteWaitBound)
	}
	if after := updates(); after != before {
		t.Fatalf("the refused rebuild changed the document's updates from %d to %d", before, after)
	}
	if text, err := documentService.Text(ctx, artifact.ID); err != nil || text != "before\n" {
		t.Fatalf("the document after the refused rebuild reads %q (%v), want its own text", text, err)
	}

	release()
	refused := rebuild()
	if refused.Code != http.StatusConflict || refusalCode(refused.Body.Bytes()) != "DOCUMENT_LOADS" {
		t.Fatalf("the rebuild once the slots were free was answered %d %s, want 409 DOCUMENT_LOADS", refused.Code, refused.Body.String())
	}
	if after := updates(); after != before {
		t.Fatalf("the refused rebuild changed the document's updates from %d to %d", before, after)
	}
}
