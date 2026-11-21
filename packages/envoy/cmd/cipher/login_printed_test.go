// packages/envoy/cmd/cipher/login_printed_test.go
//
// What `agent-secrets login` prints of the sign-in issuer's device answer: the page a person opens
// and the code they type there, both the issuer's text.
package main

import (
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
)

// TestTheSignInPromptPrintsTheIssuersPageOnlyWhereItIsAPlainURL: a verification page that carries
// a user, a password or a query prints as redact.NotPrinted, and a user code with a line break in
// it prints Go-quoted, so neither a credential nor a forged line reaches the terminal. Control:
// Google's own page and code print as they came.
func TestTheSignInPromptPrintsTheIssuersPageOnlyWhereItIsAPlainURL(t *testing.T) {
	planted := "hunter2pw-" + "signinpage-" + "8e2b"
	for _, row := range []struct{ name, page, code, want string }{
		{"a page with a user and a password", "https://tokuser9f3:" + planted + "@www.google.com/device", "ABCD-EFGH", "open " + redact.NotPrinted + " and type the code ABCD-EFGH"},
		{"a page with a query", "https://www.google.com/device?token=" + planted, "ABCD-EFGH", "open " + redact.NotPrinted},
		{"a code with a line break", "https://www.google.com/device", "ABCD\nagent-secrets: FORGED", "type the code " + strconv.Quote("ABCD\nagent-secrets: FORGED")},
		{"control: Google's page and code", "https://www.google.com/device", "ABCD-EFGH", "To sign in, open https://www.google.com/device and type the code ABCD-EFGH"},
	} {
		t.Run(row.name, func(t *testing.T) {
			got := signInPrompt(row.page, row.code)
			if strings.Contains(got, "\n") {
				t.Errorf("the prompt %q holds a line break", got)
			}
			wantNamedNotPrinted(t, "the prompt", got, []string{row.want}, planted, "tokuser9f3")
		})
	}
}
