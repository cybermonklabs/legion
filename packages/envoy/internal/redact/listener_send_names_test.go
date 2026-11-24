package redact_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
)

// TestAListenerSendsWordsPrintAndAnUndeclaredFieldDoesNot: a send to the Envoy listener carries
// the session it is for, what the message wants back, the message's text and the event frame that
// carries the text again, which listenerwire declares public, and a string under a body name no
// contract declares. The listener's answers quote the session and the reply wish ("no live session
// s1", "message is required"), and share runs of eight characters with a message that names its
// own trouble ("the send finds no live session yet"). They print as the listener wrote them, so
// Dispatch still tells a send that found no live session from any other refusal, and the
// undeclared value is redacted as every value under an unlisted name is.
func TestAListenerSendsWordsPrintAndAnUndeclaredFieldDoesNot(t *testing.T) {
	const undeclared = "v1_hint_7d3f2a9c"
	for _, reason := range []string{"no live session s1", "message is required"} {
		t.Run(reason, func(t *testing.T) {
			text := "the send finds " + reason + " yet"
			frame, err := json.Marshal(map[string]string{"body": text})
			if err != nil {
				t.Fatal(err)
			}
			send, err := json.Marshal(map[string]string{
				"target_session": "s1", "expects_reply": "required", "message": text, "payload": string(frame),
				"hint": undeclared,
			})
			if err != nil {
				t.Fatal(err)
			}
			request, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:7777/v1/messages/send", strings.NewReader(string(send)))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Content-Type", "application/json")
			body := `{"error":"` + reason + ` (hint ` + undeclared + `)"}`
			answer, err := redact.ReadAnswer(&http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(body)), Request: request}, 1<<10)
			if err != nil {
				t.Fatal(err)
			}
			words := answer.Words().String()
			if !strings.Contains(words, reason+" (hint ") {
				t.Errorf("the words are %q; want the listener's reason as it wrote it, %q", words, reason)
			}
			// Control: a string under a body name no contract declares is still a credential.
			if strings.Contains(words, undeclared) || !strings.Contains(words, "[redacted]") {
				t.Errorf("the words are %q; want the undeclared hint value redacted", words)
			}
		})
	}
}
