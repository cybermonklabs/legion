package controller

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// fromTheDaemonWhen reports whether r is the probe's under test, which carries says: for the
// liveness probe, which may hold no token, a request of the one shape it sends. Any other request
// is another process's: the loopback port the kernel gives a stand-in may be one a process of the
// machine still sends to. It is answered 404 and logged, with its method and path quoted and cut
// short, fails nothing and is recorded as nothing. It is internal/daemon's fromTheDaemon for a
// credential no one bearer names.
func fromTheDaemonWhen(t testing.TB, w http.ResponseWriter, r *http.Request, carries bool) bool {
	t.Helper()
	if carries {
		return true
	}
	t.Logf("a stand-in answered 404 to %.16q %.200q, which carries no credential of the daemon's", r.Method, r.URL.Path)
	w.WriteHeader(http.StatusNotFound)
	return false
}

// The listener stand-in keeps what the probe's own lookup carried, whatever another process sends
// it after: a read of a metadata server's identity token a helper of the machine sent to a reused
// port (dispatch://INFRA-3904) is answered 404 and changes nothing a test reads, while a role
// lookup, the one shape the probe sends, is recorded, so a test reading the record fails on a
// lookup the probe should not have made.
func TestTheListenerStandInRecordsOnlyTheProbesOwnRequests(t *testing.T) {
	l := &listener{status: http.StatusOK, body: holder("ses_ctl", probeNow)}
	url := l.serve(t)
	var log bytes.Buffer
	if got := newTestProber(url, "envoy-bearer", &log).Probe(testwait.Context(t), "ses_ctl"); got != Live {
		t.Fatalf("Probe = %s, want live; log %s", got, log.String())
	}
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
	for _, stranger := range []string{"", "Bearer another-process-token-not-a-secret", "Bearer envoy-bearer"} {
		if status := send("/computeMetadata/v1/instance/service-accounts/default/identity", stranger); status != http.StatusNotFound {
			t.Errorf("a metadata read with Authorization %q: status %d, want 404", stranger, status)
		}
	}
	if l.path != "/v1/roles/legion-demo-controller" || l.bearer != "Bearer envoy-bearer" {
		t.Fatalf("after the strangers' requests the stand-in keeps %s with %q, want the probe's lookup with its bearer", l.path, l.bearer)
	}
	send("/v1/roles/legion-demo-architect", "Bearer envoy-bearer")
	if l.path != "/v1/roles/legion-demo-architect" {
		t.Fatalf("a role lookup the probe should not have made left the record at %s, want it recorded for the test to read", l.path)
	}
}
