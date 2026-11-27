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

// TestABrokerRedirectSendsTheOperatorsTokenNowhere: whatever answers in the broker's place
// answers the row's read (row, a 302) and the cohort's signed delete (delete, a 307, which would
// send the DELETE again) with a redirect to a stand-in on the same host name at another port,
// whose requests it keeps (redirectstest.Sent), and whose Location quotes the operator's token
// (redirectstest.To). The client follows no redirect (redirects.Refused): the call fails naming
// the redirect's status, and the stand-in is sent nothing, though net/http copies Authorization on
// to the same host name. The twin shows a client that follows the redirect sends the stand-in the
// token. Captures holdrecord.redirect-followed/<case>, what the stand-in was sent.
func TestABrokerRedirectSendsTheOperatorsTokenNowhere(t *testing.T) {
	ctx := context.Background()
	host, sent := redirectstest.Sent(t)
	moved := func(status int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", redirectstest.To(host, bearerOf(r)))
			w.WriteHeader(status)
		}
	}
	t.Run("row", func(t *testing.T) {
		base := standIn(t, moved(http.StatusFound))
		c, _ := wordsClient(t, base)
		_, err := c.Row(ctx)
		redacttest.RequireNoPiece(t, "holdrecord.redirect-followed/row", sent(), operatorToken)
		if err == nil || !strings.Contains(err.Error(), "GET /v1/nats-hold answered 302, a redirect") {
			t.Errorf("Row answered %v, want the redirect refused by its status", err)
		}
	})
	t.Run("twin-row", func(t *testing.T) {
		redirectstest.RequireSentOn(t, operatorRequest(t, http.MethodGet, standIn(t, moved(http.StatusFound))+"/v1/nats-hold"), operatorToken, sent)
	})
	t.Run("delete", func(t *testing.T) {
		var key ed25519.PrivateKey
		handler := func(w http.ResponseWriter, r *http.Request) {
			cohortThen(cohortAnswer(t, key), moved(http.StatusTemporaryRedirect))(w, r)
		}
		c, signing := wordsClient(t, standIn(t, handler))
		key = signing
		err := c.DeleteCohort(ctx, wordsHold)
		redacttest.RequireNoPiece(t, "holdrecord.redirect-followed/delete", sent(), operatorToken)
		if err == nil || !strings.Contains(err.Error(), "DELETE /v1/nats-hold/records answered 307, a redirect") {
			t.Errorf("DeleteCohort answered %v, want the redirect refused by its status", err)
		}
	})
	t.Run("twin-delete", func(t *testing.T) {
		redirectstest.RequireSentOn(t, operatorRequest(t, http.MethodDelete, standIn(t, moved(http.StatusTemporaryRedirect))+"/v1/nats-hold/records"), operatorToken, sent)
	})
}
