package claudebridge

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
	"github.com/sjawhar/legion/daemon/internal/claim"
)

// TestATurnReportRedirectSendsTheBootTokenNowhereAndLogsNoLocation: whatever answers at the
// daemon's address answers a turn's report, whose body carries the launch's boot token, with a 307
// whose Location quotes that token. One net/http cannot parse (unparsed) is net/http's error
// whatever the client's redirect policy, since Go parses a Location before it asks CheckRedirect;
// one naming an address on another host nothing listens at (elsewhere) is named in a following
// client's error; one naming a stand-in that counts what it is asked (followed) has a following
// client send it the report again, the boot token in its body. The pane's line holds no run of the
// token and no word of the Location, and the stand-in is asked nothing: the bridge follows no
// redirect, so a 3xx is the daemon's refusal of the report, logged by its status. The twins show
// net/http's own error holds the echo and a following client sends the stand-in the report.
// Captures claudebridge.redirect/<case>.
func TestATurnReportRedirectSendsTheBootTokenNowhereAndLogsNoLocation(t *testing.T) {
	const token = "kq4vn-boot-not-a-secret-token-7e2f"
	target, asked := redirectstest.Target(t)
	dead := redirectstest.Dead(t)
	// echo answers every report 307 with the Location location makes of the boot token the
	// report's body carries, as whatever answers in the daemon's place can.
	echo := func(t *testing.T, location func(bootToken string) string) string {
		t.Helper()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var report claim.TurnOutcomeRequest
			_ = json.NewDecoder(r.Body).Decode(&report)
			w.Header().Set("Location", location(report.BootToken))
			w.WriteHeader(http.StatusTemporaryRedirect)
		}))
		t.Cleanup(server.Close)
		return server.URL
	}
	followed := func(bootToken string) string { return redirectstest.To(target, bootToken) }
	report, err := json.Marshal(claim.TurnOutcomeRequest{BootToken: token, Generation: 4, Sequence: 1, Outcome: claim.TurnClean})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("twin", func(t *testing.T) {
		response, err := http.Post(echo(t, redirectstest.Unparsed)+"/legion/v1/claims/turn-outcome", "application/json", bytes.NewReader(report))
		if err == nil {
			_ = response.Body.Close()
			t.Fatal("net/http answered the unparsed Location with no error: the rows cannot see one")
		}
		if !strings.Contains(err.Error(), token) {
			t.Fatalf("net/http's own error holds no echo of the boot token, so the rows cannot see a leak: %q", err)
		}
	})
	t.Run("twin-followed", func(t *testing.T) {
		response, err := http.Post(echo(t, followed)+"/legion/v1/claims/turn-outcome", "application/json", bytes.NewReader(report))
		if err != nil {
			t.Fatalf("a following client's report = %v, want the stand-in's 500", err)
		}
		_ = response.Body.Close()
		if n := asked(); n != 1 {
			t.Fatalf("a following client asked the redirect's target %d times, want once, so the rows cannot see a report sent there", n)
		}
	})
	for _, c := range []struct {
		name     string
		location func(bootToken string) string
		want     string
	}{
		{"unparsed", redirectstest.Unparsed, "could not report the outcome of turn 1 (clean)"},
		{"elsewhere", func(bootToken string) string { return "http://" + dead + "/moved?access_token=" + bootToken }, "the daemon refused the outcome of turn 1 (clean): 307 Temporary Redirect"},
		{"followed", followed, "the daemon refused the outcome of turn 1 (clean): 307 Temporary Redirect"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tokenFile := filepath.Join(t.TempDir(), "boot-token")
			if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var logged bytes.Buffer
			reports := startTurnReports(Config{DaemonURL: echo(t, c.location), BootTokenFile: tokenFile, Generation: 4}, log.New(&logged, "", 0))
			if reports == nil {
				t.Fatalf("the bridge started no reporter: %q", logged.String())
			}
			reports.post(claim.TurnOutcomeRequest{BootToken: reports.bootToken, Generation: reports.generation, Sequence: 1, Outcome: claim.TurnClean})
			oauth2calltest.RequireNoPiece(t, "claudebridge.redirect/"+c.name, logged.String(), token)
			if strings.Contains(logged.String(), "moved") {
				t.Errorf("the pane's line names the Location: %q", logged.String())
			}
			if !strings.Contains(logged.String(), c.want) {
				t.Errorf("the pane's line is %q, want %q in it", logged.String(), c.want)
			}
			if n := asked(); n != 0 {
				t.Errorf("the redirect's target was asked %d times, want none", n)
			}
		})
	}
}
