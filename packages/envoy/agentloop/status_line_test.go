package agentloop

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAProviderAnswerNamesItsStatusWithNoPhraseItWrote: the decision endpoint, or whatever answers
// in its place, refuses the decision, and redirects it, under a reason phrase that quotes a
// credential's URL. The decision's detail names the status by its code and the standard phrase
// (redact.StatusLine), and none of the phrase the endpoint wrote: Decide's refusal
// (decide-refused) and the redirect its transport refuses (redirect). Captures
// agentloop.status-line/<case>.
func TestAProviderAnswerNamesItsStatusWithNoPhraseItWrote(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, header, body, want string
		status                   int
	}{
		{"decide-refused", "Content-Type: application/json\r\n", `{"error":"the decider refused"}`, "403 Forbidden", http.StatusForbidden},
		{"redirect", "Location: http://elsewhere.example/\r\n", "", "302 Found", http.StatusFound},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "token")
			writeToken(t, path, "the-decision-token")
			result, err := Decide(context.Background(), DecideRequest{
				Model: Model{
					Endpoint: Endpoint{Kind: SystemOne, BaseURL: redacttest.StatusLineServer(t, c.status, c.header, c.body), Token: fileToken(path)},
					ID:       "systemone-standin", Price: systemOnePrice,
				},
				State:     json.RawMessage(`{"note":"The note says plum."}`),
				Questions: decideQuestions(),
				Bounds:    Bounds{Time: 30 * time.Second},
			})
			if err != nil {
				t.Fatal(err)
			}
			redacttest.RequireNone(t, "agentloop.status-line/"+c.name, result.Detail)
			if result.Fault != FaultProviderError || !strings.Contains(result.Detail, c.want) {
				t.Errorf("result %+v, want %q naming %q", result, FaultProviderError, c.want)
			}
		})
	}
}
