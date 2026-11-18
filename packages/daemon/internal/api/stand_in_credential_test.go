package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// registrationsToken is the daemon's own token, the bearer of every write its agent-secrets client
// makes to the broker.
const registrationsToken = "the-daemon's-token"

// aLease is the githubtest.Spec.Credential of this package's GitHub stand-ins: a bearer of the
// shape handoffTokens leases (<role>-token-for-<owner>, "-read" after it for a read lease), so a
// test that reads which lease a request carried still tells one from another, and a request with
// none is another process's.
func aLease(r *http.Request) bool {
	token, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	role, owner, leased := strings.Cut(strings.TrimSuffix(token, "-read"), "-token-for-")
	return bearer && leased && role != "" && owner != ""
}

// fromTheDaemon reports whether r carries bearer, the credential the daemon under test presents to
// a stand-in. Any other request is another process's: the loopback port the kernel gives a
// stand-in may be one a process of the machine still sends to. It is answered 404 and logged, with
// its method and path quoted and cut short, and fails nothing, so a stand-in's power to fail the
// test stays with the daemon's own requests. It is internal/daemon's fromTheDaemon over a
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

// The broker stand-in answers a write without the daemon's token 404, fails nothing and takes no
// write, while a write under the token whose body does not decode still fails the test. It runs
// under failures, which reads what it reported: nothing for a post of another process's body in
// each form a stranger's credential takes, and the undecodable write under the daemon's token.
func TestAStandInFailsItsTestOnlyOnTheDaemonsOwnRequests(t *testing.T) {
	recorded := &failures{TB: t}
	broker := &exchangeBroker{t: recorded}
	server := httptest.NewServer(broker)
	t.Cleanup(server.Close)
	send := func(authorization string) int {
		request, err := http.NewRequestWithContext(testwait.Context(t), http.MethodPost, server.URL+"/v1/oauth/exchange/pod-sessions", strings.NewReader("not json"))
		if err != nil {
			t.Fatal(err)
		}
		if authorization != "" {
			request.Header.Set("Authorization", authorization)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		_ = response.Body.Close()
		return response.StatusCode
	}
	for _, stranger := range []string{"", "Bearer another-process-token-not-a-secret", "token " + registrationsToken, "Bearer " + registrationsToken + "-read"} {
		if status := send(stranger); status != http.StatusNotFound {
			t.Errorf("a write with Authorization %q: status %d, want 404", stranger, status)
		}
	}
	if got := recorded.reported(); len(got) != 0 || len(broker.received()) != 0 {
		t.Fatalf("writes without the daemon's token made the broker report %q and take %d writes, want nothing", got, len(broker.received()))
	}
	send("Bearer " + registrationsToken)
	if got := recorded.reported(); len(got) != 1 || !strings.HasPrefix(got[0], "the registration's body does not decode: ") {
		t.Fatalf("an undecodable write under the daemon's token made the broker report %q, want its body refused", got)
	}
}
