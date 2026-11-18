package dispatch

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// fromTheDaemon reports whether r carries bearer, the credential the client under test presents
// to a stand-in. Any other request is another process's: the loopback port the kernel gives a
// stand-in may be one a process of the machine still sends to. It is answered 404 and logged, with
// its method and path quoted and cut short, and fails nothing, so a stand-in's power to fail the
// test stays with the client's own requests. It is internal/daemon's fromTheDaemon over a
// testing.TB, so a test can run a stand-in under failures.
func fromTheDaemon(t testing.TB, w http.ResponseWriter, r *http.Request, bearer string) bool {
	t.Helper()
	if r.Header.Get("Authorization") == "Bearer "+bearer {
		return true
	}
	t.Logf("a stand-in answered 404 to %.16q %.200q, which carries no credential of the daemon's", r.Method, r.URL.Path)
	w.WriteHeader(http.StatusNotFound)
	return false
}

// standIn is a Dispatch stand-in serving handle to each request that carries the client's bearer
// (dispatchToken) and answering any other request 404 (fromTheDaemon), so a handler that fails the
// test on a request it does not expect fails it on the client's requests alone. It is closed when
// the test ends.
func standIn(t testing.TB, handle http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fromTheDaemon(t, w, r, dispatchToken) {
			handle(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// failures is a testing.TB that keeps each failure a stand-in reports instead of failing the test
// it runs in, so a test reads whether a request failed it. Fatalf ends the handler's goroutine,
// as testing's does.
type failures struct {
	testing.TB
	mu     sync.Mutex
	failed []string
}

func (f *failures) Errorf(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed = append(f.failed, fmt.Sprintf(format, args...))
}

func (f *failures) Fatalf(format string, args ...any) {
	f.Errorf(format, args...)
	runtime.Goexit()
}

// reported is every failure kept so far.
func (f *failures) reported() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.failed)
}

// A Dispatch stand-in answers a request without the client's bearer 404 and fails nothing, while
// one that carries the bearer to a path its handler does not expect still fails the test. The
// handler is one of the shape every test of this file starts (standIn): it fails the test on any
// request but the one it models. It runs under failures, which reads what it reported: nothing
// for the read of a metadata server's identity token a helper of the machine sent to a reused port
// (dispatch://INFRA-3904), in each form a stranger's credential takes, and the request under the
// client's own bearer.
func TestAStandInFailsItsTestOnlyOnTheClientsOwnRequests(t *testing.T) {
	recorded := &failures{TB: t}
	server := standIn(recorded, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/issues" {
			recorded.Fatalf("request = %s %s, want GET /api/v1/issues", r.Method, r.URL.Path)
		}
		writeJSON(t, w, []any{})
	})
	const metadata = "/computeMetadata/v1/instance/service-accounts/default/identity"
	send := func(authorization string) (int, error) {
		request, err := http.NewRequestWithContext(testwait.Context(t), http.MethodGet, server.URL+metadata, nil)
		if err != nil {
			t.Fatal(err)
		}
		// Each request on a connection of its own: the transport sends a GET again on a reused
		// connection that closed unanswered, as the handler's Fatalf closes it.
		request.Close = true
		if authorization != "" {
			request.Header.Set("Authorization", authorization)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return 0, err
		}
		_ = response.Body.Close()
		return response.StatusCode, nil
	}
	for _, stranger := range []string{"", "Bearer another-process-token-not-a-secret", "token " + dispatchToken, "Bearer " + dispatchToken + "-read"} {
		if status, err := send(stranger); err != nil || status != http.StatusNotFound {
			t.Errorf("GET %s with Authorization %q = %d, %v; want 404", metadata, stranger, status, err)
		}
	}
	if got := recorded.reported(); len(got) != 0 {
		t.Fatalf("requests without the client's bearer made the stand-in report %q, want nothing", got)
	}
	_, _ = send("Bearer " + dispatchToken)
	if got, want := recorded.reported(), []string{"request = GET " + metadata + ", want GET /api/v1/issues"}; !slices.Equal(got, want) {
		t.Fatalf("a request under the client's bearer that the stand-in does not expect made it report %q, want %q", got, want)
	}
}
