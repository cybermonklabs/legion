package collabtest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testcleanup"
)

// windowContext is a context whose caller holds one of the pool's connections
// (store.HoldsConnection), with the release that lets it go: the state a transaction is in from
// its begin. store.OpenXidWindow then marks it written, as the rigs' tracer does at the
// transaction's first statement that writes.
func windowContext(t *testing.T) (context.Context, func()) {
	t.Helper()
	ctx, release := store.HoldsConnection(context.Background())
	testcleanup.Add(t, release)
	return ctx, release
}

// Check, the collaborator check every rig's decorator calls, reports a call made inside a
// transaction's xid window and no other: none before the transaction's first write, one once the
// window is open, and none once the connection is let go. This row is the check's own control in
// every run: a Check that stopped reporting would pass every rig row that wraps a collaborator.
func TestCheckReportsACallInsideTheXidWindowAlone(t *testing.T) {
	caught := &RecordedFailures{TB: t}
	ctx, release := windowContext(t)

	Check(caught, ctx, "embed.Embedder", "Embed")
	if failures := caught.Reported(); len(failures) != 0 {
		t.Fatalf("a call before the transaction's first write was reported: %q", failures)
	}
	if !store.OpenXidWindow(ctx) {
		t.Fatal("a context holding a connection of the pool took no xid window mark")
	}
	Check(caught, ctx, "embed.Embedder", "Embed")
	failures := caught.Reported()
	if len(failures) != 1 || !strings.HasPrefix(failures[0], "embed.Embedder.Embed was called while its caller holds a connection of the pool") {
		t.Fatalf("a call inside the xid window was reported as %q, want one failure naming embed.Embedder.Embed", failures)
	}
	release()
	Check(caught, ctx, "embed.Embedder", "Embed")
	if after := caught.Reported(); len(after) != 1 {
		t.Fatalf("a call once the connection was let go was reported: %q", after[1:])
	}
}

// HTTPClient checks each request it sends, with the request's own context, its method and its
// URL, and sends it all the same: a request made inside an xid window is reported, naming its
// host, method and path, and answered; one made before the window opens is only answered.
func TestHTTPClientChecksEachRequestItSends(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	testcleanup.Add(t, server.Close)
	caught := &RecordedFailures{TB: t}
	client := HTTPClient(caught)
	ctx, _ := windowContext(t)
	send := func(when string) {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/agents", nil)
		if err != nil {
			t.Fatalf("%s: build the request: %v", when, err)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("%s: send the request: %v", when, err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("%s: the request was answered %d, want %d: the client did not send it on", when, response.StatusCode, http.StatusNoContent)
		}
	}

	send("before the first write")
	if failures := caught.Reported(); len(failures) != 0 {
		t.Fatalf("a request before the transaction's first write was reported: %q", failures)
	}
	store.OpenXidWindow(ctx)
	send("inside the window")
	host := strings.TrimPrefix(server.URL, "http://")
	failures := caught.Reported()
	if len(failures) != 1 || !strings.HasPrefix(failures[0], "HTTP "+host+".GET /v1/agents was called while") {
		t.Fatalf("a request inside the xid window was reported as %q, want one failure naming HTTP %s.GET /v1/agents", failures, host)
	}
}
