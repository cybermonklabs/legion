package leakguard

import (
	"fmt"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// fromTheDaemonWhen reports whether r is the code's under test, which carries says: its bearer, or,
// where the code sends a server no credential, the one shape of request it sends there. Any other
// request is another process's: the loopback port the kernel gives a stand-in may be one a process
// of the machine still sends to. It is answered 404 and logged, with its method and path quoted
// and cut short, fails nothing and is recorded as nothing. It is internal/daemon's fromTheDaemon
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

// sourceBearer is the githubtest.Spec.Credential of a stand-in GitHub a Source of sourceOver reads:
// its bearer, sourceToken.
func sourceBearer(r *http.Request) bool {
	return r.Header.Get("Authorization") == "Bearer "+sourceToken
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

// A stand-in GitHub a Source reads answers a request without the source's bearer 404, records
// nothing of it and fails nothing, while a request under the bearer that it does not expect still
// fails the test. It is the redirect test's API stand-in (sourceBearer ahead of its Record),
// run under failures.
func TestAStandInFailsItsTestOnlyOnTheSourcesOwnReads(t *testing.T) {
	recorded := &failures{TB: t}
	var mu sync.Mutex
	var asked []string
	url := githubtest.Server(recorded, githubtest.Spec{Credential: sourceBearer, Token: sourceToken,
		Record: func(r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			asked = append(asked, r.URL.Path)
		},
		Handle: func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != tarballRoute+theTag {
				githubtest.Unexpected(recorded, w, r)
			}
		}})
	send := func(authorization string) int {
		request, err := http.NewRequestWithContext(testwait.Context(t), http.MethodGet, url+"/computeMetadata/v1/instance/service-accounts/default/identity", nil)
		if err != nil {
			t.Fatal(err)
		}
		if authorization != "" {
			request.Header.Set("Authorization", authorization)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_ = response.Body.Close()
		return response.StatusCode
	}
	for _, stranger := range []string{"", "Bearer another-process-token-not-a-secret", "token " + sourceToken, "Bearer " + sourceToken + "-read"} {
		if status := send(stranger); status != http.StatusNotFound {
			t.Errorf("a read with Authorization %q: status %d, want 404", stranger, status)
		}
	}
	mu.Lock()
	kept := slices.Clone(asked)
	mu.Unlock()
	if got := recorded.reported(); len(got) != 0 || len(kept) != 0 {
		t.Fatalf("reads without the source's bearer made the stand-in report %q and record %q, want nothing", got, kept)
	}
	send("Bearer " + sourceToken)
	if got, want := recorded.reported(), []string{"unexpected GitHub request GET /computeMetadata/v1/instance/service-accounts/default/identity"}; !slices.Equal(got, want) {
		t.Fatalf("a read under the source's bearer that the stand-in does not expect made it report %q, want %q", got, want)
	}
}
