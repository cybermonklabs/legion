package claudebridge

import (
	"bytes"
	"log"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// A LEGION_DAEMON_URL whose made-up password holds an unencoded "@" leaves the password's tail
// where the host goes, and the resolver's error names that host: the line logging a report that
// got no answer holds no part of the password, its tail in its ASCII form or in the `xn--` form a
// tail that is not ASCII is dialed by.
func TestAFailedTurnReportLogsNoPartOfThePassword(t *testing.T) {
	passwordAt := "legion:madeuphead0123" + "@"
	for name, tail := range map[string]string{
		"an ASCII tail":            "madeuptail0123.invalid",
		"a tail that is not ASCII": "madeuptail0123\u00f6rd.invalid",
	} {
		t.Run(name, func(t *testing.T) {
			var logged bytes.Buffer
			reports := &turnReports{daemonPoster: &daemonPoster{
				url:        "http://" + passwordAt + tail,
				bootToken:  "boot-token",
				generation: 1,
				client:     &http.Client{Timeout: reportTimeout},
				clock:      Config{}.clock(),
				log:        log.New(&logged, "", 0),
			}}
			reports.post(claim.TurnOutcomeRequest{BootToken: "boot-token", Generation: 1, Sequence: 1, Outcome: claim.TurnClean})
			lower := strings.ToLower(logged.String())
			if !strings.Contains(lower, "could not report the outcome of turn 1") {
				t.Fatalf("the log %q names no failed report", logged.String())
			}
			for _, part := range []string{"madeuphead", "madeuptail", "xn--"} {
				if strings.Contains(lower, part) {
					t.Errorf("the log %q holds %q of the password", logged.String(), part)
				}
			}
		})
	}
}
