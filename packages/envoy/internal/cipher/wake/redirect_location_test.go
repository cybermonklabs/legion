package wake

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestANoticeRedirectLogsNoLocationOfIt: whatever answers at Envoy's address answers the notice,
// which carries the broker's account token, with a redirect whose Location quotes that token. One
// net/http cannot parse (unparsed) is net/http's error whatever the client's redirect policy, since
// Go parses a Location before it asks CheckRedirect; one that parses names an address on another
// host nothing listens at (elsewhere), which a following client asks, naming the Location in its
// error. The warning Notify logs holds no run of the token and no word of the Location. The twin
// shows the stand-in's echo reaches net/http's own error. Captures wake.redirect/<case>.
func TestANoticeRedirectLogsNoLocationOfIt(t *testing.T) {
	const token = "vf2hc-not-a-secret-RDX-5a9d"
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := closed.Addr().String()
	_ = closed.Close()
	echo := func(location func(bearer string) string) string {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", location(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
			w.WriteHeader(http.StatusFound)
		}))
		t.Cleanup(server.Close)
		return server.URL
	}
	unparsed := func(bearer string) string { return "/moved%zz" + bearer }
	t.Run("twin", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodPost, echo(unparsed)+"/v1/messages/send", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			_ = response.Body.Close()
			t.Fatal("net/http answered the unparsed Location with no error: the rows cannot see one")
		}
		if !strings.Contains(err.Error(), token) {
			t.Fatalf("net/http's own error holds no echo of the token, so the rows cannot see a leak: %q", err)
		}
	})
	for _, c := range []struct {
		name     string
		location func(bearer string) string
	}{
		{"unparsed", unparsed},
		{"elsewhere", func(bearer string) string { return "http://" + dead + "/moved?access_token=" + bearer }},
	} {
		t.Run(c.name, func(t *testing.T) {
			var logged bytes.Buffer
			logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
			Envoy{URL: echo(c.location), Account: staticAccount(token), HTTP: &http.Client{}}.Notify(context.Background(), "sess-123", "req-456", "approved")
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
