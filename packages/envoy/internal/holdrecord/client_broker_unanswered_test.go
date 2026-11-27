package holdrecord_test

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// TestABrokerCallThatGetsNoAnswerPrintsNoRunOfTheOperatorsToken: whatever answers in the broker's
// place answers with what net/http's error of a request that got no answer quotes, the operator's
// token echoed in it: a redirect whose Location does not parse (unparsed, the row's read;
// delete-unparsed, the cohort's signed delete), and a status line whose code does not parse
// (status-line). Each error names the method and the route, then net/http's operation, the URL
// without its query and the failure's class (redact.Unanswered): no run of the token, nor of the
// delete's signature, which its query carried. The twins show net/http's own error quotes the
// token. Captures holdrecord.no-answer/<case>.
func TestABrokerCallThatGetsNoAnswerPrintsNoRunOfTheOperatorsToken(t *testing.T) {
	ctx := context.Background()
	unparsed := redirectstest.EchoBearer(t, redirectstest.Unparsed)
	statusLine := rawBroker(t, func(r *http.Request) string {
		return "HTTP/1.1 " + bearerOf(r) + " OK\r\nContent-Length: 0\r\n\r\n"
	})
	t.Run("twin-unparsed", func(t *testing.T) {
		redirectstest.RequireEcho(t, operatorRequest(t, http.MethodGet, unparsed+"/v1/nats-hold"), operatorToken)
	})
	t.Run("twin-status-line", func(t *testing.T) {
		redirectstest.RequireEcho(t, operatorRequest(t, http.MethodGet, statusLine+"/v1/nats-hold"), operatorToken)
	})
	for _, x := range []struct{ name, base string }{{"unparsed", unparsed}, {"status-line", statusLine}} {
		t.Run(x.name, func(t *testing.T) {
			c, _ := wordsClient(t, x.base)
			_, err := c.Row(ctx)
			redacttest.RequireNoPiece(t, "holdrecord.no-answer/"+x.name, errText(err), operatorToken)
			if err == nil || !strings.HasPrefix(err.Error(), "holdrecord: GET /v1/nats-hold: Get ") {
				t.Errorf("Row answered %v, want the route named and then net/http's operation", err)
			}
		})
	}
	t.Run("delete-unparsed", func(t *testing.T) {
		var key ed25519.PrivateKey
		echo := func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", redirectstest.Unparsed(bearerOf(r)))
			w.WriteHeader(http.StatusFound)
		}
		handler := func(w http.ResponseWriter, r *http.Request) { cohortThen(cohortAnswer(t, key), echo)(w, r) }
		c, signing := wordsClient(t, standIn(t, handler))
		key = signing
		err := c.DeleteCohort(ctx, wordsHold)
		redacttest.RequireNoPiece(t, "holdrecord.no-answer/delete-unparsed", errText(err), operatorToken, deleteSig(t, key))
		if err == nil || !strings.HasPrefix(err.Error(), "holdrecord: DELETE /v1/nats-hold/records: Delete ") {
			t.Errorf("DeleteCohort answered %v, want the route named and then net/http's operation", err)
		}
	})
}
