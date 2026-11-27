package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// TestTheStateReadNamesNoTailOfAPasswordInLEGIONDAEMONURL: a LEGION_DAEMON_URL whose made-up
// password holds an unencoded "@" and no host after it leaves the password's tail where url.Parse
// reads the host, and the resolver's error names that host, in its own form (at-tail) or in the
// `xn--` form a tail that is not ASCII is dialled by (at-tail-not-ascii). `legion state`'s read
// (get) names its failure through oauth2call.Unanswered, so its refusal names the read with the
// authority as the mark of a URL with credentials, and the host the resolver could not find as the
// mark or, for a tail that is not ASCII, fixed words alone: no run of the password. Each twin,
// net/http's own error of the read, holds a run of the password. Captures
// legion.state-credential/<case>.
func TestTheStateReadNamesNoTailOfAPasswordInLEGIONDAEMONURL(t *testing.T) {
	const head = `legion state: read http://[a URL with credentials]/legion/v1/state: Get "http://[a URL with credentials]/legion/v1/state": `
	tail := "q7vn2-" + oauth2calltest.Token + "-4c1e"
	for _, c := range []struct{ name, password, want string }{
		{"at-tail", "p@" + tail + ".invalid", head + "dial tcp: lookup [a URL with credentials]"},
		{"at-tail-not-ascii", "p@" + tail + "\u00f6rd.invalid", head + "(the request failed; its error is not printed: it names the host of an address that holds a credential, and that host is not ASCII)\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			daemonURL := "http://legion:" + c.password
			response, err := daemonClient.Get(daemonURL + "/legion/v1/state")
			if err == nil {
				_ = response.Body.Close()
				t.Fatalf("%s answered %d: the row needs a read that gets no answer", daemonURL, response.StatusCode)
			}
			if oauth2calltest.Piece(err.Error(), c.password) == "" {
				t.Fatalf("the twin %q holds no run of the password, so the row cannot see a leak", err)
			}
			t.Chdir(t.TempDir())
			t.Setenv("LEGION_DAEMON_URL", daemonURL)
			t.Setenv("LEGION_ISSUE", "")
			var out, errb bytes.Buffer
			code := run(context.Background(), []string{"legion", "state"}, &out, &errb)
			oauth2calltest.RequireNoPiece(t, "legion.state-credential/"+c.name, out.String()+errb.String(), c.password)
			if lower := strings.ToLower(errb.String()); code != 1 || !strings.HasPrefix(errb.String(), c.want) || strings.Contains(lower, "xn--") {
				t.Errorf("legion state = %d, stderr %q; want 1 and %q", code, errb.String(), c.want)
			}
		})
	}
}
