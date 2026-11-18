package ghbranch

import (
	"fmt"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

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

// The repository stand-in answers a request without the installation token 404, fails nothing
// and records no read, while a request under the token that it does not expect still fails the
// test. It runs under failures, which reads what it reported: nothing for the read of a metadata
// server's identity token a helper of the machine sent to a reused port
// (dispatch://INFRA-3904), in each form a stranger's credential takes, and the read of the base's
// tree under the token, which a Create that read the tree would make.
func TestAStandInFailsItsTestOnlyOnTheClientsOwnRequests(t *testing.T) {
	recorded := &failures{TB: t}
	g := &github{refs: map[string]string{"main": baseCommit}}
	url := g.serve(recorded)
	send := func(path, authorization string) int {
		request, err := http.NewRequestWithContext(testwait.Context(t), http.MethodGet, url+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if authorization != "" {
			request.Header.Set("Authorization", authorization)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = response.Body.Close()
		return response.StatusCode
	}
	const metadata = "/computeMetadata/v1/instance/service-accounts/default/identity"
	// The last stranger is the client's own token with a read lease's suffix, which no client of
	// this package sends.
	for _, stranger := range []string{"", "Bearer another-process-token-not-a-secret", "token installation-token", "Bearer " + widgets(url).Token + "-read"} {
		for _, path := range []string{metadata, "/repos/acme/widgets/git/ref/heads/main"} {
			if status := send(path, stranger); status != http.StatusNotFound {
				t.Errorf("GET %s with Authorization %q: status %d, want 404", path, stranger, status)
			}
		}
	}
	if got := recorded.reported(); len(got) != 0 || len(g.reads()) != 0 {
		t.Fatalf("requests without the installation token made the stand-in report %q and record the reads %q, want nothing", got, g.reads())
	}
	send("/repos/acme/widgets/git/trees/"+baseCommit, "Bearer installation-token")
	if got, want := recorded.reported(), []string{"unexpected request GET /repos/acme/widgets/git/trees/" + baseCommit}; !slices.Equal(got, want) {
		t.Fatalf("a request under the installation token that the stand-in does not expect made it report %q, want %q", got, want)
	}
}
