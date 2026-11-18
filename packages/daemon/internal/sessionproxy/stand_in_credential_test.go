package sessionproxy

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
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

// The Admin API fake answers a request without a token of this package's credential fakes 404,
// records nothing and fails nothing, while a call under one of those tokens that it does not answer
// still fails the test. It runs under failures. The read is of a metadata server's identity token, a
// helper of the machine's request to a reused port (dispatch://INFRA-3904).
func TestTheAdminAPIFakeFailsItsTestOnlyOnTheConnectorsOwnCalls(t *testing.T) {
	recorded := &failures{TB: t}
	f := &adminAPIFake{t: recorded}
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	send := func(authorization string) int {
		request, err := http.NewRequestWithContext(testwait.Context(t), http.MethodGet, server.URL+"/computeMetadata/v1/instance/service-accounts/default/identity", nil)
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
	for _, stranger := range []string{"", "Bearer another-process-token-not-a-secret", "token " + podToken, "Bearer " + podToken + "-read"} {
		if status := send(stranger); status != http.StatusNotFound {
			t.Errorf("a read with Authorization %q: status %d, want 404", stranger, status)
		}
	}
	if got := recorded.reported(); len(got) != 0 || len(f.requests()) != 0 {
		t.Fatalf("requests without the connector's tokens made the fake report %q and record %+v, want nothing", got, f.requests())
	}
	send("Bearer " + impersonatedAPIToken)
	if got, want := recorded.reported(), []string{"the connector called GET /computeMetadata/v1/instance/service-accounts/default/identity, which the Admin API fake does not answer"}; !slices.Equal(got, want) {
		t.Fatalf("a call under the API token that the fake does not answer made it report %q, want %q", got, want)
	}
}

// The upstream hands out the connection of the proxy's own dial, whatever connected before it: a
// connection from another process of the machine, which sends nothing, is passed over and closed
// with the test.
func TestTheUpstreamHandsOutOnlyTheProxysOwnDial(t *testing.T) {
	up := newUpstream(t)
	stranger, err := net.Dial("tcp", up.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stranger.Close() })
	dialer := &fakeDialer{addr: up.listener.Addr().String()}
	own, err := dialer.Dial(testwait.Context(t), testInstance)
	if err != nil {
		t.Fatalf("the fake dialer's dial: %v", err)
	}
	t.Cleanup(func() { _ = own.Close() })
	got := up.next(t)
	if got.RemoteAddr().String() != own.LocalAddr().String() {
		t.Fatalf("next = the connection from %s, want the dialer's own from %s, not the stranger's from %s",
			got.RemoteAddr(), own.LocalAddr(), stranger.LocalAddr())
	}
}
