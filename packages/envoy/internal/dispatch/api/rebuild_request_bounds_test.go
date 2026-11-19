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

// rebuildWatch is the document service with every rebuild a route asks it for recorded - when it
// was asked and the deadline of the context it was handed - before the service's own rebuild runs.
type rebuildWatch struct {
	docs.API
	mu    sync.Mutex
	asked []watchedRebuild
}

// watchedRebuild is one rebuild a route asked for: when, and the deadline its context carried,
// if it carried one.
type watchedRebuild struct {
	at       time.Time
	deadline time.Time
	bounded  bool
}

func (w *rebuildWatch) RebuildDocument(ctx context.Context, artifactID string, markdown *string, actor model.Actor) (docs.RebuildReport, docs.VersionResult, error) {
	deadline, bounded := ctx.Deadline()
	w.mu.Lock()
	w.asked = append(w.asked, watchedRebuild{at: time.Now(), deadline: deadline, bounded: bounded})
	w.mu.Unlock()
	return w.API.RebuildDocument(ctx, artifactID, markdown, actor)
}

func (w *rebuildWatch) rebuilds() []watchedRebuild {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]watchedRebuild(nil), w.asked...)
}

// newRebuildWatchServer is a registered test server whose rebuilds the returned watch records,
// with an evicted document holding "before\n" for a test to rebuild.
func newRebuildWatchServer(t *testing.T) (http.Handler, *server, *rebuildWatch, model.Artifact) {
	t.Helper()
	watch := &rebuildWatch{}
	var documentService *docs.Service
	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{settle: time.Hour, documents: func(service *docs.Service) docs.API {
		documentService = service
		watch.API = service
		return watch
	}})
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
	if err := documentService.Evict(context.Background(), artifact.ID); err != nil {
		t.Fatalf("evict the document: %v", err)
	}
	return handler, served, watch, artifact
}

// The rebuild route runs its document work under the server's bound on a caller's whole document
// write: the context RebuildDocument is handed is Join's, whose deadline is WriteRequestBound from
// when the request began (docs.Service.Join, boundWriteRequest), so every statement, lock wait
// and memory wait of the rebuild ends there. The request's start is marked as the request enters
// the route's chain (trackTransactions, docs.WithCallerRequest), under a key only docs reads, so
// the deadline is held between the bound measured from just before the request was sent and the
// bound measured from when the rebuild was asked for. Handed the request's own context instead,
// the rebuild runs under no deadline at all.
//
// The request carries a context that ends, as a caller's does: Join bounds only a caller's
// request, and a context with no Done channel reads as the server's own work (boundCallerWrite).
// The document is an ordinary one, so the rebuild the route asks for is refused 409
// DOCUMENT_LOADS once its deadline has been read.
func TestTheRebuildRouteRunsItsRebuildUnderTheWholeWriteBound(t *testing.T) {
	handler, _, watch, artifact := newRebuildWatchServer(t)
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/artifacts/"+artifact.ID+"/rebuild", strings.NewReader("{}"))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "alice")
	response := httptest.NewRecorder()
	sent := time.Now()
	handler.ServeHTTP(response, request)
	asked := watch.rebuilds()
	if len(asked) != 1 {
		t.Fatalf("the route asked for %d rebuilds and answered %d %s, want one", len(asked), response.Code, response.Body.String())
	}
	rebuild := asked[0]
	if !rebuild.bounded {
		t.Fatalf("the route asked for its rebuild %s into the request with a context carrying no deadline, want the %s whole-write bound",
			rebuild.at.Sub(sent).Round(time.Microsecond), docs.WriteRequestBound)
	}
	t.Logf("the route asked for its rebuild %s into the request, under a deadline %s after the request was sent",
		rebuild.at.Sub(sent).Round(time.Microsecond), rebuild.deadline.Sub(sent).Round(time.Microsecond))
	if earliest, latest := sent.Add(docs.WriteRequestBound), rebuild.at.Add(docs.WriteRequestBound); rebuild.deadline.Before(earliest) || rebuild.deadline.After(latest) {
		t.Fatalf("the rebuild's deadline is %s after the request was sent, want %s from the request's start: between %s and %s",
			rebuild.deadline.Sub(sent), docs.WriteRequestBound, earliest.Sub(sent), latest.Sub(sent))
	}
	if response.Code != http.StatusConflict || refusalCode(response.Body.Bytes()) != "DOCUMENT_LOADS" {
		t.Fatalf("the rebuild of an ordinary document was answered %d %s, want 409 DOCUMENT_LOADS", response.Code, response.Body.String())
	}
}

// The rebuild route is a POST under routeHandler, so its body is read under the same idle
// deadline as every other write's (holdWriteLimit, deadlinedBody), which is bodyIdleLimit where
// the deployment runs, with no drainReadLimit: a rebuild whose body stops arriving is answered 503
// BODY_STALLED once that deadline has passed since its last bytes, and the rebuild is never asked
// for. Mounted outside the drain, the route's read of the body has no deadline and waits for as
// long as the client keeps the connection open: here, the 30 s after which the client gives up
// (sendPartOfBody) with no answer at all. That holds at any idle deadline, so the stall is waited
// out under one of a second, as the other stalled bodies' tests wait it out
// (TestARefusalOfACutBodyKeepsTheHeadersEveryAnswerCarries).
func TestARebuildWhoseBodyStallsIsAnsweredBodyStalledAtItsIdleDeadline(t *testing.T) {
	handler, served, watch, artifact := newRebuildWatchServer(t)
	served.drainReadLimit = time.Second
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	header.Set("X-Dispatch-User", "alice")
	body := []byte(`{"markdown":"rebuilt\n"}`)
	const part = 4
	started := time.Now()
	stalled := sendPartOfBody(t, server.Listener.Addr().String(), "/api/v1/artifacts/"+artifact.ID+"/rebuild", header, len(body), body[:part])
	refused, refusal := stalled.answer()
	elapsed := time.Since(started)
	t.Logf("a rebuild whose %d-byte body stopped after %d bytes was answered %d %s after %s",
		len(body), part, refused.StatusCode, strings.TrimSpace(string(refusal)), elapsed.Round(time.Millisecond))
	if refused.StatusCode != http.StatusServiceUnavailable || refusalCode(refusal) != "BODY_STALLED" {
		t.Fatalf("a rebuild whose body stopped arriving was answered %d %s after %s, want 503 BODY_STALLED at its idle deadline", refused.StatusCode, refusal, elapsed)
	}
	if elapsed < served.idleBound() {
		t.Fatalf("a rebuild whose body stopped arriving was refused BODY_STALLED after %s, before its %s idle deadline", elapsed, served.idleBound())
	}
	if asked := watch.rebuilds(); len(asked) != 0 {
		t.Fatalf("the route asked for %d rebuilds of a request whose body never arrived, want none", len(asked))
	}
}
