package holdrecord_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestABrokerAnswerThatCannotBeReadPrintsNoRunOfTheOperatorsToken: whatever answers in the
// broker's place answers 200 with a chunked body whose trailer line echoes the operator's token and
// does not parse, which net/http's error of the body's read quotes (the twin). The error the read
// answers names the route and the failure's class (redact.ReadAnswer), and no run of the token
// (trailer). An answer past the client's limit is refused as that (redact.ErrAnswerTooLarge),
// never cut at the limit and read as if whole (past-limit). Captures holdrecord.body-read/trailer.
func TestABrokerAnswerThatCannotBeReadPrintsNoRunOfTheOperatorsToken(t *testing.T) {
	ctx := context.Background()
	trailer := rawBroker(t, func(r *http.Request) string {
		return "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n2\r\n{}\r\n0\r\n" + bearerOf(r) + "\r\n\r\n"
	})
	t.Run("twin", func(t *testing.T) {
		response, err := http.DefaultClient.Do(operatorRequest(t, http.MethodGet, trailer+"/v1/nats-hold"))
		if err != nil {
			t.Fatalf("the stand-in answered no status line: %v", err)
		}
		defer response.Body.Close()
		if _, err := io.ReadAll(response.Body); err == nil || !strings.Contains(err.Error(), operatorToken) {
			t.Fatalf("net/http's error of the body's read holds no echo of the token, so the row cannot see one: %v", err)
		}
	})
	t.Run("trailer", func(t *testing.T) {
		c, _ := wordsClient(t, trailer)
		_, err := c.Row(ctx)
		redacttest.RequireNoPiece(t, "holdrecord.body-read/trailer", errText(err), operatorToken)
		if err == nil || !strings.HasPrefix(err.Error(), "holdrecord: read the answer to GET /v1/nats-hold: ") {
			t.Errorf("Row answered %v, want the read of the route's answer named", err)
		}
	})
	t.Run("past-limit", func(t *testing.T) {
		// A whole JSON object followed by more spaces than the limit: cut at the limit, it still
		// reads as JSON.
		long := standIn(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"gen":1}`)
			_, _ = io.WriteString(w, strings.Repeat(" ", 16<<20))
		})
		c, _ := wordsClient(t, long)
		if _, err := c.Row(ctx); !errors.Is(err, redact.ErrAnswerTooLarge) {
			t.Errorf("Row of an answer past the limit answered %v, want redact.ErrAnswerTooLarge", err)
		}
	})
}
