package leakguard

import (
	"net/http"
	"slices"
	"strconv"
	"testing"
)

// R11 of dispatch://INFRA-3562, at the real gh's own timing: gh prints `net/http: TLS handshake
// timeout` only once net/http's TLSHandshakeTimeout has run out, 10 s after its handshake started
// (http.DefaultTransport's; the tester measured gh 2.98.0 printing it 10.07 s after its start, at a
// proxy that answers CONNECT and then sends nothing). The guard bounds its own gh read at the
// product's ghReadBound, which this package's tests do not stretch, so the guard must still tell
// OnTLSTimeout the host of a read whose handshake runs that long, as it tells it for a stand-in
// that prints the line at once (TestTheGuardTellsAGhTLSHandshakeTimeoutsHost): otherwise no push,
// whose only Go TLS client is this read, records the timeout it met. The stand-in here waits gh's
// handshake timeout before it prints the line, and curl answers, as it did in the pods R11 names.
func TestTheGuardTellsTheHostOfAGhReadThatRunsToGhsOwnHandshakeTimeout(t *testing.T) {
	handshake := http.DefaultTransport.(*http.Transport).TLSHandshakeTimeout
	stalls := "sleep " + strconv.Itoa(int(handshake.Seconds())) + "\n" + ghTLSTimeout
	var told []string
	rig := newTransportRig(t, stalls, noGraphQL, curlAnswers("private"), answersTheRefs)
	rig.guard.opts.OnTLSTimeout = func(host string) { told = append(told, host) }
	visibility, err := rig.guard.Visibility(bound(t), theRepo)
	if err != nil || visibility != Private {
		t.Fatalf("read %q, %v; want curl's private", visibility, err)
	}
	if want := []string{"api.github.com"}; !slices.Equal(told, want) {
		t.Errorf("the guard told the hosts %q of a gh read whose TLS handshake ran to gh's own %s timeout (ghReadBound %s), want %q",
			told, handshake, ghReadBound, want)
	}
}
