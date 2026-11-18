package agentsecrets

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// fromTheDaemonWhen reports whether r is the client's under test, which carries says: the
// credential the client presents to a stand-in broker, a bearer, a body or a launcher proof. Any
// other request is another process's: the loopback port the kernel gives a stand-in may be one a
// process of the machine still sends to. It is answered 404 and logged, with its method and path
// quoted and cut short, and fails nothing, so a stand-in's power to fail the test stays with the
// client's own requests. It is internal/daemon's fromTheDaemon for a credential no one bearer
// names.
func fromTheDaemonWhen(t testing.TB, w http.ResponseWriter, r *http.Request, carries bool) bool {
	t.Helper()
	if carries {
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

// The registration broker answers a write without the daemon's own token 404, fails nothing and
// records no write, while a write under the token whose body is no JSON still fails the test. It
// runs under failures, which reads what it reported: nothing for a post of another process's body
// in each form a stranger's credential takes, and the write under the daemon's token.
func TestAStandInBrokerFailsItsTestOnlyOnTheClientsOwnWrites(t *testing.T) {
	recorded := &failures{TB: t}
	broker, server := newRegistrationBroker(recorded, "pod-uid-1")
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
	for _, stranger := range []string{"", "Bearer another-process-token-not-a-secret", "token the-daemon's-token", "Bearer the-daemon's-token-read"} {
		if status := send(stranger); status != http.StatusNotFound {
			t.Errorf("a write with Authorization %q: status %d, want 404", stranger, status)
		}
	}
	if got := recorded.reported(); len(got) != 0 || len(broker.received()) != 0 {
		t.Fatalf("writes without the daemon's token made the broker report %q and record %d writes, want nothing", got, len(broker.received()))
	}
	send("Bearer the-daemon's-token")
	if got := recorded.reported(); len(got) != 1 || !strings.HasPrefix(got[0], `the registration's body "not json" is not JSON: `) {
		t.Fatalf("a write under the daemon's token whose body is no JSON made the broker report %q, want its body refused", got)
	}
}
