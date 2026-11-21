package claudebridge

import (
	"bytes"
	"log"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/claim"
)

// TestARefusedTurnReportNamesItsStatusWithNoPhraseItWrote: the daemon, or whatever answers in its
// place, refuses a turn's report under a reason phrase that quotes a credential's URL: with words
// that are no outcome (words), with an outcome (outcome), and with a body past what the bridge
// reads (unread). The line the bridge logs names the status by its code and the standard phrase
// (oauth2call.StatusLine), and none of the phrase the daemon's stand-in wrote. Captures
// claudebridge.status-line/<case>.
func TestARefusedTurnReportNamesItsStatusWithNoPhraseItWrote(t *testing.T) {
	for _, c := range []struct {
		name, body, want string
		status           int
	}{
		{"words", "the claim is not this pane's", "409 Conflict the claim is not this pane's", http.StatusConflict},
		{"outcome", `{"refused_run":1}`, "(clean): 422 Unprocessable Entity", http.StatusUnprocessableEntity},
		{"unread", strings.Repeat("x", maxAnswerBytes+1), "400 Bad Request (its answer could not be read", http.StatusBadRequest},
	} {
		t.Run(c.name, func(t *testing.T) {
			var logged bytes.Buffer
			reports := &turnReports{
				url:    oauth2calltest.StatusLineServer(t, c.status, "", c.body) + "/legion/v1/claims/turn-outcome",
				client: &http.Client{},
				log:    log.New(&logged, "", 0),
			}
			reports.post(claim.TurnOutcomeRequest{Sequence: 1, Outcome: claim.TurnClean})
			oauth2calltest.RequireNone(t, "claudebridge.status-line/"+c.name, logged.String())
			if !strings.Contains(logged.String(), c.want) {
				t.Errorf("the bridge logged %q, want %q in it", logged.String(), c.want)
			}
		})
	}
}
