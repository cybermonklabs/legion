// Package oauth2calltest is the probe of internal/redact/redacttest for a module outside this one,
// which cannot import an internal package. It holds three parts:
//
//   - The box print test: a type of that module that holds an oauth2call.RemoteText is a holder
//     the gate's holder census lists (cmd/redact-gate), and the holder's print row runs PrintTest
//     in that module's own package, as a holder of this module runs redacttest.PrintTest.
//   - The kept print test: a value of that module that keeps an answer, and with it the record of
//     the credentials its request carried, is printed by KeptPrintTest, as one of this module is
//     by redacttest.KeptPrintTest.
//   - The status-line probe: a server whose reason phrase quotes a URL carrying made-up
//     credentials, and the check's designated assertion, RequireNone, which a print-site row of
//     that module calls as a row of this module calls redacttest.RequireNone; RequireNoPiece is
//     the same assertion for a row whose stand-in echoes the credential the request carried too.
//
// It is for tests: Said makes a box with no record of an answer, so the answer census lists every
// call of it outside a test file (gate.answers.makers).
package oauth2calltest

import (
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/oauth2call"
)

// Holder is redacttest.Holder: a holder of the box for the print test, with its plain-string twins.
type Holder = redacttest.Holder

// Words is redacttest.Words, the probe: words that quote a URL with a made-up credential in its
// user information and its query.
const Words = redacttest.Words

// Reason is redacttest.Reason: words that quote the probe's URL, made-up credentials and all, as
// a status line's reason phrase or any other field of an answer may.
const Reason = redacttest.Reason

// PrintTest is redacttest.PrintTest: h, holding the probe, printed by every printer at every
// placement, beside its twins.
func PrintTest(t *testing.T, h Holder) {
	t.Helper()
	redacttest.PrintTest(t, h)
}

// Token is redacttest.Token, a made-up credential, for a request whose stand-in echoes it.
const Token = redacttest.Token

// KeptPrintTest is redacttest.KeptPrintTest: v, which keeps one of the made-up credentials, or one
// of credentials the request carried, where no printer may reach it (an answer's record of its
// request), printed under every fmt verb and a bad verb, at every holding, beside twin, a plain
// string that holds one.
func KeptPrintTest(t *testing.T, name string, v any, twin string, credentials ...string) {
	t.Helper()
	redacttest.KeptPrintTest(t, name, v, twin, credentials...)
}

// StatusLineServer is redacttest.StatusLineServer: a server that answers every request with
// status, header lines and body under the reason phrase Reason, and answers its base URL.
func StatusLineServer(t testing.TB, status int, header, body string) string {
	t.Helper()
	return redacttest.StatusLineServer(t, status, header, body)
}

// CutServer is redacttest.CutServer: StatusLineServer cutting body off halfway, as a reset
// mid-body reads to the client.
func CutServer(t testing.TB, status int, body string) string {
	t.Helper()
	return redacttest.CutServer(t, status, body)
}

// RequireNone is redacttest.RequireNone, a print-site test's designated assertion: it captures
// text under capture and fails t, naming capture and each made-up credential, for each one text
// holds.
func RequireNone(t testing.TB, capture, text string) {
	t.Helper()
	redacttest.RequireNone(t, capture, text)
}

// RequireNoPiece is redacttest.RequireNoPiece: RequireNone of text under capture that also fails
// t, naming capture, for each of credentials, the values the request carried, that text holds a
// run of 8 characters of.
func RequireNoPiece(t testing.TB, capture, text string, credentials ...string) {
	t.Helper()
	redacttest.RequireNoPiece(t, capture, text, credentials...)
}

// Said is s held as another party's words, for a test that builds the value a client would have
// decoded from an answer: it prints only as the rule reads it and compares as it came.
func Said(s string) oauth2call.RemoteText { return redact.NewRemoteText(s) }
