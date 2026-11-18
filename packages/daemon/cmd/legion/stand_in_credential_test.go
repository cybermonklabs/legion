package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// fromTheDaemon reports whether r carries bearer, the credential the legion command under test
// presents to a stand-in daemon. Any other request is another process's: the loopback port the
// kernel gives a stand-in may be one a process of the machine still sends to. It is answered 404
// and logged, with its method and path quoted and cut short, and fails nothing, so a stand-in's
// power to fail the test stays with the command's own requests. It is internal/daemon's
// fromTheDaemon over a testing.TB, so a test can run a stand-in under failures.
func fromTheDaemon(t testing.TB, w http.ResponseWriter, r *http.Request, bearer string) bool {
	t.Helper()
	if r.Header.Get("Authorization") == "Bearer "+bearer {
		return true
	}
	t.Logf("a stand-in answered 404 to %.16q %.200q, which carries no credential of the daemon's", r.Method, r.URL.Path)
	w.WriteHeader(http.StatusNotFound)
	return false
}

// failures is a testing.TB that keeps each failure a stand-in reports with Errorf instead of
// failing the test it runs in, so a test reads whether a request failed it.
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

// reported is every failure kept so far.
func (f *failures) reported() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.failed)
}

// A stand-in daemon of the command's tests answers a request without the operator token 404 and
// fails nothing, while a request under the token that it does not expect still fails the test. The
// stand-in is the contract-11 daemon's shape (TestControllerStartAgainstADaemonBeforeContract12
// NamesTheSideToUpgrade): it fails the test on any request its body decoding accepts. It runs under
// failures, which reads what it reported: nothing for the read of a metadata server's identity
// token a helper of the machine sent to a reused port (dispatch://INFRA-3904), in each form a
// stranger's credential takes, and the request under the operator token.
func TestAStandInDaemonFailsItsTestOnlyOnTheCommandsOwnRequests(t *testing.T) {
	recorded := &failures{TB: t}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !fromTheDaemon(recorded, w, r, controllerOperatorToken) {
			return
		}
		recorded.Errorf("the contract-11 stand-in accepted %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	const metadata = "/computeMetadata/v1/instance/service-accounts/default/identity"
	send := func(authorization string) int {
		request, err := http.NewRequestWithContext(testwait.Context(t), http.MethodGet, server.URL+metadata, nil)
		if err != nil {
			t.Fatal(err)
		}
		if authorization != "" {
			request.Header.Set("Authorization", authorization)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("GET %s: %v", metadata, err)
		}
		_ = response.Body.Close()
		return response.StatusCode
	}
	for _, stranger := range []string{"", "Bearer another-process-token-not-a-secret", "token " + controllerOperatorToken, "Bearer " + controllerOperatorToken + "-read"} {
		if status := send(stranger); status != http.StatusNotFound {
			t.Errorf("GET %s with Authorization %q: status %d, want 404", metadata, stranger, status)
		}
	}
	if got := recorded.reported(); len(got) != 0 {
		t.Fatalf("requests without the operator token made the stand-in report %q, want nothing", got)
	}
	send("Bearer " + controllerOperatorToken)
	if got, want := recorded.reported(), []string{"the contract-11 stand-in accepted GET " + metadata}; !slices.Equal(got, want) {
		t.Fatalf("a request under the operator token that the stand-in does not expect made it report %q, want %q", got, want)
	}
}
