package modeltoken

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// fromTheSignIn reads the call r carries and says whether it is the sign-in's under test: a JSON
// body naming the configured app client (ClientId "client-id", testConfig's), the credential every
// call of Cognito's custom authentication presents. Any other request is another process's: the
// loopback port the kernel gives a stand-in may be one a process of the machine still sends to. It
// is answered 404 and logged, with its method and path quoted and cut short, and fails nothing, so
// the stand-in's power to fail the test stays with the sign-in's own calls.
func fromTheSignIn(t testing.TB, w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err == nil && body["ClientId"] == "client-id" {
		return body, true
	}
	t.Logf("a stand-in answered 404 to %.16q %.200q, which carries no credential of the sign-in's", r.Method, r.URL.Path)
	w.WriteHeader(http.StatusNotFound)
	return nil, false
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

// The Cognito stand-in answers a request without the app client's id 404, fails nothing and
// counts no call, while a call under the id that it does not model still fails the test. It runs
// under failures, which reads what it reported: nothing for the read of a metadata server's
// identity token a helper of the machine sent to a reused port (dispatch://INFRA-3904), for a body
// that is no JSON and for a call of another app client; and the call under the configured client's
// id of an operation the sign-in never makes.
func TestAStandInFailsItsTestOnlyOnTheSignInsOwnCalls(t *testing.T) {
	recorded := &failures{TB: t}
	cognito := &fakeCognito{t: recorded}
	server := httptest.NewServer(cognito)
	t.Cleanup(server.Close)
	send := func(method, target, body string) int {
		request, err := http.NewRequestWithContext(testwait.Context(t), method, server.URL+"/", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if target != "" {
			request.Header.Set("X-Amz-Target", "AWSCognitoIdentityProviderService."+target)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("%s %s: %v", method, target, err)
		}
		_ = response.Body.Close()
		return response.StatusCode
	}
	for _, stranger := range []struct{ method, target, body string }{
		{http.MethodGet, "", ""},
		{http.MethodPost, "InitiateAuth", "not json"},
		{http.MethodPost, "InitiateAuth", `{"ClientId":"another-client","AuthFlow":"CUSTOM_AUTH"}`},
	} {
		if status := send(stranger.method, stranger.target, stranger.body); status != http.StatusNotFound {
			t.Errorf("%s %s %q without the app client's id: status %d, want 404", stranger.method, stranger.target, stranger.body, status)
		}
	}
	if got := recorded.reported(); len(got) != 0 || cognito.calls.Load() != 0 {
		t.Fatalf("requests without the app client's id made the stand-in report %q and count %d calls, want nothing", got, cognito.calls.Load())
	}
	send(http.MethodPost, "ForgotPassword", `{"ClientId":"client-id"}`)
	if got, want := recorded.reported(), []string{`unexpected Cognito call "ForgotPassword"`}; !slices.Equal(got, want) {
		t.Fatalf("a call under the app client's id that the stand-in does not model made it report %q, want %q", got, want)
	}
}
