package claudebridge

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/claim"
)

// TestATurnReportGoesToTheRouteJoinedOntoTheDaemonsPath: LEGION_DAEMON_URL is what the pane is
// told, and it can carry a path, a query or a fragment. A report goes to the daemon's path with the
// turn-outcome route joined onto it, the query kept and the fragment dropped, where the route
// appended to the URL's text sat inside the query or the fragment and the daemon was asked a path
// it does not serve. A stand-in daemon records the path and the query each report reaches it with,
// and the report's URL is the one it reaches, which holds no fragment: no request sends one.
func TestATurnReportGoesToTheRouteJoinedOntoTheDaemonsPath(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "boot-token")
	if err := os.WriteFile(tokenFile, []byte("boot-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, suffix, path, query string
	}{
		{"no-path", "", "/legion/v1/claims/turn-outcome", ""},
		{"trailing-slash", "/", "/legion/v1/claims/turn-outcome", ""},
		{"path", "/daemon", "/daemon/legion/v1/claims/turn-outcome", ""},
		{"query", "/daemon?sig=abc", "/daemon/legion/v1/claims/turn-outcome", "sig=abc"},
		{"query-trailing-slash", "/daemon/?sig=abc", "/daemon/legion/v1/claims/turn-outcome", "sig=abc"},
		{"fragment", "#pane", "/legion/v1/claims/turn-outcome", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			asked := make(chan url.URL, 1)
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				asked <- *request.URL
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"refusedRun":0,"failed":false}`))
			}))
			defer daemon.Close()
			var logged bytes.Buffer
			reports := startTurnReports(Config{DaemonURL: daemon.URL + c.suffix, BootTokenFile: tokenFile, Generation: 4}, log.New(&logged, "", 0))
			if reports == nil {
				t.Fatalf("the bridge started no reporter: %q", logged.String())
			}
			want := daemon.URL + c.path
			if c.query != "" {
				want += "?" + c.query
			}
			if reports.url != want {
				t.Errorf("a LEGION_DAEMON_URL ending %q gave the report the URL %q, want %q", c.suffix, reports.url, want)
			}
			reports.post(claim.TurnOutcomeRequest{BootToken: reports.bootToken, Generation: reports.generation, Sequence: 1, Outcome: claim.TurnClean})
			select {
			case got := <-asked:
				if got.Path != c.path || got.RawQuery != c.query {
					t.Errorf("a LEGION_DAEMON_URL ending %q sent the report to path %q, query %q; want path %q, query %q", c.suffix, got.Path, got.RawQuery, c.path, c.query)
				}
			default:
				t.Fatalf("no report reached the daemon; the bridge logged %q", logged.String())
			}
		})
	}
}

// TestALegionDaemonURLThatDoesNotParseIsNotPrinted: a LEGION_DAEMON_URL url.Parse refuses starts
// no reporter, said once in the bridge's own words, never url.Parse's error, which quotes the URL
// whole, its query included. The twin shows url.Parse's error holds the credential. Captures
// claudebridge.daemon-url/unparsed.
func TestALegionDaemonURLThatDoesNotParseIsNotPrinted(t *testing.T) {
	const token = "p7rq2-not-a-secret-token-9c3e"
	daemon := "http://127.0.0.1:1/daemon%zz?sig=" + token
	tokenFile := filepath.Join(t.TempDir(), "boot-token")
	if err := os.WriteFile(tokenFile, []byte("boot-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Run("twin", func(t *testing.T) {
		if _, err := url.Parse(daemon); err == nil || !strings.Contains(err.Error(), token) {
			t.Fatalf("url.Parse's error = %v, want one that holds %q, so the row can see a leak", err, token)
		}
	})
	t.Run("unparsed", func(t *testing.T) {
		var logged bytes.Buffer
		if reports := startTurnReports(Config{DaemonURL: daemon, BootTokenFile: tokenFile, Generation: 4}, log.New(&logged, "", 0)); reports != nil {
			t.Error("the bridge started a reporter; want none for a URL that does not parse")
		}
		oauth2calltest.RequireNoPiece(t, "claudebridge.daemon-url/unparsed", logged.String(), token)
		if want := "reporting no turn outcomes: LEGION_DAEMON_URL does not parse as a URL (its value is not printed)\n"; logged.String() != want {
			t.Errorf("the bridge logged %q, want %q", logged.String(), want)
		}
	})
}
