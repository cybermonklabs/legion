package api_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
)

// TestProjectPrivacyNeverLogsTheBrokersToken (Tester4672's red test, F1): a Dispatch that repeats
// the bearer it was sent, in a refusal's code or in a malformed projects value, must not get that
// bearer into the broker's log. GET /v1/project-privacy answers each 502 dispatch_unavailable in
// fixed words already; its WARN line must still be written, naming the failure's kind alone
// (redact.Class, or ErrBrokerRefused's fixed text): a line that is not written holds no bearer
// either, so each row also names the line it wants.
func TestProjectPrivacyNeverLogsTheBrokersToken(t *testing.T) {
	logged := policytest.CaptureLog(t)
	for _, c := range []struct {
		name   string
		answer http.HandlerFunc
		line   string
	}{
		{"a 500 whose code repeats the bearer", answerWith(http.StatusInternalServerError, `{"code":"INTERNAL `+privacyStubToken+`"}`),
			`WARN broker: read Dispatch's private projects failed error="the request failed"`},
		{"a 401 whose code repeats the bearer", answerWith(http.StatusUnauthorized, `{"code":"REFUSED `+privacyStubToken+`"}`),
			`WARN broker: read Dispatch's private projects failed error="projectaccess: the broker's own account token was refused or could not be read"`},
		{"a projects value that repeats the bearer", answerWith(http.StatusOK, `{"projects":"`+privacyStubToken+`"}`),
			`WARN broker: read Dispatch's private projects failed error="the request failed"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			stub := newPrivacyStub(t, c.answer)
			g := newPrivacyServer(t, dispatchAt(stub, nil))
			before := len(logged.String())
			got := g.privacy(t, g.accountToken(t, privacyListenerAccount, "", time.Hour))
			if got.status != http.StatusBadGateway || strings.Contains(string(got.body), privacyStubToken) {
				t.Fatalf("GET %s = %d %s, want 502 with no bearer in it", privacyPath, got.status, got.body)
			}
			written := logged.String()[before:]
			if strings.Contains(written, privacyStubToken) {
				t.Fatalf("the broker's log repeats the bearer it presented to Dispatch:\n%s", written)
			}
			if !strings.Contains(written, c.line) {
				t.Fatalf("the broker's log after the call holds no line %s; it holds:\n%s", c.line, written)
			}
		})
	}
}
