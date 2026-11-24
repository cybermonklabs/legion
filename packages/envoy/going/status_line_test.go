package going_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAChatAnswerNamesItsStatusWithNoPhraseItWrote: the chat completions service, or whatever
// answers in its place, writes a reason phrase that quotes a credential's URL: on a refusal with
// an error code (coded), on one with none (uncoded), and on a stream whose frame is not JSON
// (frame). Chat's error names the status by its code and the standard phrase (redact.StatusLine),
// and none of the phrase the service wrote. Captures going.status-line/<case>.
func TestAChatAnswerNamesItsStatusWithNoPhraseItWrote(t *testing.T) {
	for _, c := range []struct {
		name, header, body, want string
		status                   int
	}{
		{"coded", "Content-Type: application/json\r\n", `{"error":{"code":"rate_limited"}}`, "HTTP 429 Too Many Requests, error code", http.StatusTooManyRequests},
		{"uncoded", "", "", "HTTP 502 Bad Gateway", http.StatusBadGateway},
		{"frame", "Content-Type: text/event-stream\r\n", "data: not json\n\n", "HTTP 200 OK, a frame that is not JSON", http.StatusOK},
	} {
		t.Run(c.name, func(t *testing.T) {
			client, err := going.NewHTTPClient(going.HTTPConfig{
				Place: "endpoint", Endpoint: redacttest.StatusLineServer(t, c.status, c.header, c.body), Model: "gpt-test",
				Tokens: staticTokens{token: "chat-bearer"},
			})
			if err != nil {
				t.Fatalf("NewHTTPClient: %v", err)
			}
			_, err = client.Chat(t.Context(), testRequest(), nil)
			if err == nil {
				t.Fatal("Chat answered no error")
			}
			redacttest.RequireNone(t, "going.status-line/"+c.name, err.Error())
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("Chat's error is %q, want %q in it", err, c.want)
			}
		})
	}
}
