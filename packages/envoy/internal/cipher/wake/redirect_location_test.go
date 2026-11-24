package wake

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// TestANoticeRedirectLogsNoLocationOfIt: whatever answers at Envoy's address answers the notice,
// which carries the broker's account token, with a redirect whose Location quotes that token. One
// net/http cannot parse (unparsed) is net/http's error whatever the client's redirect policy, since
// Go parses a Location before it asks CheckRedirect; one that parses names an address on another
// host nothing listens at (elsewhere), which a following client would ask, naming the Location in
// its error, and which Notify, whose client follows no redirect, logs as the 302 it is
// (TestANoticeFollowsNoRedirect). The warning Notify logs holds no run of the token and no word of
// the Location. The twin shows the stand-in's echo reaches net/http's own error. Captures
// wake.redirect/<case>.
func TestANoticeRedirectLogsNoLocationOfIt(t *testing.T) {
	const token = "vf2hc-not-a-secret-RDX-5a9d"
	dead := redirectstest.Dead(t)
	t.Run("twin", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodPost, redirectstest.EchoBearer(t, redirectstest.Unparsed)+"/v1/messages/send", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		redirectstest.RequireEcho(t, request, token)
	})
	for _, c := range []struct {
		name     string
		location func(bearer string) string
	}{
		{"unparsed", redirectstest.Unparsed},
		{"elsewhere", func(bearer string) string { return redirectstest.To(dead, bearer) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			var logged bytes.Buffer
			logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
			Envoy{URL: redirectstest.EchoBearer(t, c.location), Account: staticAccount(token), HTTP: &http.Client{}}.Notify(context.Background(), "sess-123", "req-456", "approved")
			if !strings.Contains(logged.String(), "wake envoy: send") {
				t.Fatalf("Notify logged no failed send: %q", logged.String())
			}
			redacttest.RequireNoPiece(t, "wake.redirect/"+c.name, logged.String(), token)
			if strings.Contains(logged.String(), "moved") {
				t.Errorf("the warning names the Location: %q", logged.String())
			}
		})
	}
}
