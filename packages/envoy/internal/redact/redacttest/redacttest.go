// Package redacttest is the probe a test gives a print site of a remote's words: words that quote
// a URL carrying a credential in its user information and in a query or fragment value, what the
// rule for remote text prints of them, and the assertion that a printed text holds none of the
// credentials. Every credential here is made up.
//
// The assertion is the designated one of the check c-redact-remote-text's mutation gate
// (cmd/redact-gate): RequireNone first prints the production output it was given in one line,
// `REDACT-CAPTURE <capture> <base64>`, and fails, where that output holds a credential, with a
// message that begins `REDACT-ASSERT <capture>`. The gate reads its evidence from the captured
// line alone, never from the message.
package redacttest

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

const (
	// User and Password are the user information the probe's URL carries.
	User     = "not-a-secret-user"
	Password = "not-a-secret-password"
	// Token is the value of the access-token parameter the probe's URLs carry.
	Token = "not-a-secret-token"
	// param is the parameter OAuth carries an access token under, in two parts so no line of
	// this file reads as a credential to a scanner.
	param = "access" + "_token"
)

const (
	// Words quotes the probe's URL, with User, Password and Token in it, as a refusal may.
	Words = "the issuer at https://" + User + ":" + Password + "@issuer.example/path?" + param + "=" + Token
	// Shown is Words as the rule prints it.
	Shown = "the issuer at https://issuer.example/path?" + param + "="
	// FragmentWords quotes a URL with Token in a fragment's parameter, as an implicit-flow
	// callback carries one.
	FragmentWords = "sign in again at https://issuer.example/callback#" + param + "=" + Token
	// FragmentShown is FragmentWords as the rule prints it.
	FragmentShown = "sign in again at https://issuer.example/callback#" + param + "="
	// Unreadable quotes the probe's URL with words after its query, which the rule cannot read
	// as clean: it prints as redact.WordsNotReadable.
	Unreadable = Words + " refused the subject token"
	// Code is a code that is itself a URL carrying the probe's credentials, and CodeShown what
	// the rule prints of it: a code is a remote field too.
	Code      = "https://" + User + ":" + Password + "@issuer.example/code?" + param + "=" + Token
	CodeShown = "https://issuer.example/code?" + param + "="
)

// Secrets are the credentials no printed text may hold.
var Secrets = []string{User, Password, Token}

// Held is each of Secrets that text holds.
func Held(text string) []string {
	var held []string
	for _, secret := range Secrets {
		if strings.Contains(text, secret) {
			held = append(held, secret)
		}
	}
	return held
}

// Capture prints text, the production output a print site's test read, as the gate's evidence
// line for capture.
func Capture(t testing.TB, capture, text string) {
	t.Helper()
	t.Logf("REDACT-CAPTURE %s %s", capture, base64.StdEncoding.EncodeToString([]byte(text)))
}

// RequireNone captures text under capture and fails t, naming capture and each secret, for each
// of Secrets text holds. It is a print-site test's designated assertion.
func RequireNone(t testing.TB, capture, text string) {
	t.Helper()
	Capture(t, capture, text)
	for _, secret := range Held(text) {
		t.Errorf("REDACT-ASSERT %s: the output prints the secret %q: %q", capture, secret, text)
	}
}

// holder holds an error in an unexported field, as a value a caller formats may.
type holder struct {
	err error
}

// Formatted is v under every verb R2 names, directly and inside an unexported field of another
// value, with its LogValue where it has one and a slog line that holds it, each named by a form
// with no space in it.
func Formatted(v error) map[string]string {
	out := map[string]string{"Error()": v.Error()}
	for _, verb := range []string{"%s", "%q", "%v", "%+v", "%#v", "%x", "%X"} {
		out[verb] = fmt.Sprintf(verb, v)
		out["held-"+verb] = fmt.Sprintf(verb, holder{v})
		out["held-pointer-"+verb] = fmt.Sprintf(verb, &holder{v})
	}
	if lv, ok := v.(slog.LogValuer); ok {
		out["LogValue"] = lv.LogValue().String()
	}
	var logged strings.Builder
	slog.New(slog.NewTextHandler(&logged, nil)).Info("probe", "error", v)
	out["slog"] = logged.String()
	return out
}

// RequireNoneFormatted is RequireNone for each form of Formatted(v), under capture/<form>.
func RequireNoneFormatted(t testing.TB, capture string, v error) {
	t.Helper()
	for form, text := range Formatted(v) {
		RequireNone(t, capture+"/"+form, text)
	}
}
