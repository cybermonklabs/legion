package notify

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// fromTheDaemonWhen reports whether r is the client's under test, which carries says: under the
// listener's bearer when the client has one, or, for a client given none, of the one shape it
// sends. Any other request is another process's: the loopback port the kernel gives a stand-in
// may be one a process of the machine still sends to. It is answered 404 and logged, with its
// method and path quoted and cut short, fails nothing and is recorded as nothing, so a stand-in's
// power over the test stays with the client's own requests. It is internal/daemon's fromTheDaemon
// for a credential no one bearer names.
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

// A listener stand-in answers a request without the listener's bearer 404, records nothing of it
// and fails nothing, while the client's own request under the bearer is recorded and one under the
// bearer that the stand-in does not expect still fails the test. The stand-in records the delete
// it is sent, as TestDeleteSessionAsksTheSessionRouteWithTheBearerTheMomentAndTheInstance's does,
// and fails the test on any other request under the bearer; it runs under failures. Each stranger
// form is sent after the client's delete, where a recorder without the gate would have kept the
// stranger's request in place of the client's.
func TestAStandInListenerFailsItsTestOnlyOnTheClientsOwnRequests(t *testing.T) {
	recorded := &failures{TB: t}
	var mu sync.Mutex
	var deletes []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !fromTheDaemonWhen(recorded, w, r, r.Header.Get("Authorization") == "Bearer envoy-bearer") {
			return
		}
		if r.Method != http.MethodDelete {
			recorded.Errorf("the listener was asked %s %s", r.Method, r.URL.Path)
			return
		}
		mu.Lock()
		deletes = append(deletes, r.URL.Path)
		mu.Unlock()
	}))
	t.Cleanup(server.Close)
	if _, err := New(server.URL, "envoy-bearer").DeleteSession(testwait.Context(t), "ses_implementer", DeleteConditions{NotRegisteredAfter: time.Now()}); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	send := func(method, authorization string) int {
		request, err := http.NewRequestWithContext(testwait.Context(t), method, server.URL+"/computeMetadata/v1/instance/service-accounts/default/identity", nil)
		if err != nil {
			t.Fatal(err)
		}
		if authorization != "" {
			request.Header.Set("Authorization", authorization)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		_ = response.Body.Close()
		return response.StatusCode
	}
	for _, stranger := range []string{"", "Bearer another-process-token-not-a-secret", "token envoy-bearer", "Bearer envoy-bearer-read"} {
		for _, method := range []string{http.MethodGet, http.MethodDelete} {
			if status := send(method, stranger); status != http.StatusNotFound {
				t.Errorf("%s with Authorization %q: status %d, want 404", method, stranger, status)
			}
		}
	}
	mu.Lock()
	kept := slices.Clone(deletes)
	mu.Unlock()
	if got := recorded.reported(); len(got) != 0 || !slices.Equal(kept, []string{"/v1/sessions/ses_implementer"}) {
		t.Fatalf("strangers' requests made the stand-in report %q and keep the deletes %q, want nothing reported and the client's delete alone", got, kept)
	}
	send(http.MethodGet, "Bearer envoy-bearer")
	if got, want := recorded.reported(), []string{"the listener was asked GET /computeMetadata/v1/instance/service-accounts/default/identity"}; !slices.Equal(got, want) {
		t.Fatalf("a request under the bearer that the stand-in does not expect made it report %q, want %q", got, want)
	}
}
