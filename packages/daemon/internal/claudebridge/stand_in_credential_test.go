package claudebridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// fromTheBridge reads the report r carries and says whether it is the bridge's under test: a
// turn-outcome report whose boot token is the launch's (testBootToken), the credential the bridge
// presents in the report's own body. Any other request is another process's: the loopback port
// the kernel gives a stand-in may be one a process of the machine still sends to. It is answered
// 404 and logged, with its method and path quoted and cut short, and fails nothing, so the
// stand-in's power to fail the test stays with the bridge's own reports. A report that does not
// decode carries no boot token, and is answered so too: the test's wait for the report then fails.
func fromTheBridge(t testing.TB, w http.ResponseWriter, r *http.Request) (claim.TurnOutcomeRequest, bool) {
	t.Helper()
	var report claim.TurnOutcomeRequest
	body, err := io.ReadAll(r.Body)
	if err == nil && json.Unmarshal(body, &report) == nil && report.BootToken == testBootToken {
		return report, true
	}
	t.Logf("a stand-in answered 404 to %.16q %.200q, which carries no credential of the bridge's", r.Method, r.URL.Path)
	w.WriteHeader(http.StatusNotFound)
	return claim.TurnOutcomeRequest{}, false
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

// The rig's daemon stub answers a request without the launch's boot token 404, fails nothing and
// takes no report, while a report under the token to a route it does not expect still fails the
// test. It runs under failures, which reads what it reported: nothing for the read of a metadata
// server's identity token a helper of the machine sent to a reused port (dispatch://INFRA-3904),
// for a post of another process's body and for a report under another token; and the report under
// the launch's token to another route.
func TestAStandInFailsItsTestOnlyOnTheBridgesOwnReports(t *testing.T) {
	recorded := &failures{TB: t}
	rig := &reportingRig{reports: make(chan claim.TurnOutcomeRequest, 16),
		answer: func(claim.TurnOutcomeRequest) claim.TurnOutcomeResponse { return claim.TurnOutcomeResponse{} }}
	daemon := rig.serve(recorded)
	send := func(method, path string, body []byte) int {
		request, err := http.NewRequestWithContext(testwait.Context(t), method, daemon.URL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		_ = response.Body.Close()
		return response.StatusCode
	}
	report := func(token string) []byte {
		encoded, err := json.Marshal(claim.TurnOutcomeRequest{BootToken: token, Generation: 4, Sequence: 1, Outcome: claim.TurnClean})
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	for _, stranger := range []struct {
		method, path string
		body         []byte
	}{
		{http.MethodGet, "/computeMetadata/v1/instance/service-accounts/default/identity", nil},
		{http.MethodPost, "/legion/v1/claims/turn-outcome", []byte(`{"not":"a report"`)},
		{http.MethodPost, "/legion/v1/claims/turn-outcome", report("another-launch's-token")},
		{http.MethodPost, "/legion/v1/claims/other", report(strings.ToUpper(testBootToken))},
	} {
		if status := send(stranger.method, stranger.path, stranger.body); status != http.StatusNotFound {
			t.Errorf("%s %s without the launch's boot token: status %d, want 404", stranger.method, stranger.path, status)
		}
	}
	if got := recorded.reported(); len(got) != 0 || len(rig.reports) != 0 {
		t.Fatalf("requests without the launch's boot token made the stub report %q and take %d reports, want nothing", got, len(rig.reports))
	}
	send(http.MethodPost, "/legion/v1/claims/other", report(testBootToken))
	if got, want := recorded.reported(), []string{"the bridge posted to /legion/v1/claims/other, want the turn-outcome route"}; !slices.Equal(got, want) {
		t.Fatalf("a report under the launch's boot token to another route made the stub report %q, want %q", got, want)
	}
}
