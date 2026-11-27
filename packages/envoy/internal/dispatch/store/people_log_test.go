package store

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Each line a committed sign-in logs (signInRecorded.log) and each account-binding catch-up
// (LogAccountBindingCatchUp) names the address by its login's hash, never as written: a replaced
// account with no binding in the fleet, a first recording with none, a replaced account's revoke, a
// first recording's revoke, a row that existed ending its sessions, and a catch-up. Each keeps the
// fields it logged beside the address. The rows whose fields the database writes are read again
// against a real sign-in in TestSignInSubjectRevokesThePersonalTokensOfTheAccountItReplaces and
// TestPgPeopleStoreEndsTheSessionsOfAnAddressAnotherGoogleAccountSignsInTo.
//
// Control: log "email", email again on any of those lines, and its row fails on the address.
func TestASignInsStoreLinesNameTheAddressByItsLoginsHash(t *testing.T) {
	const email = "Ada@Example.com"
	caught := AccountBindingCatchUp{Replaced: []string{"sub-old"}, TokensRevoked: 2, Owed: []string{"sub-old"}, Holder: "sub-new"}
	for _, row := range []struct {
		name     string
		recorded signInRecorded
		message  string
		fields   []string
	}{
		{"a replaced account while a build without the binding is in the fleet", signInRecorded{replaced: true, stored: "sub-old"},
			"ended every session of the address and revoked nothing", []string{"replaced_subject=sub-old"}},
		{"a first recording while a build without the binding is in the fleet", signInRecorded{firstRecording: true, subject: "sub-new"},
			"revoked nothing of it", []string{"subject=sub-new"}},
		{"a replaced account's revoke", signInRecorded{replaced: true, revoked: true, tokensRevoked: 3},
			"ended every session and revoked every personal token of the address", []string{"tokens_revoked=3"}},
		{"a first recording's revoke", signInRecorded{firstRecording: true, revoked: true, tokensRevoked: 2, owed: 1},
			"revoked every personal token of it that records another account or none", []string{"tokens_revoked=2", "accounts=1"}},
		{"a row that existed ending its sessions", signInRecorded{existed: true, firstRecording: true, revoked: true},
			`msg="dispatch: a sign-in under another Google account ended every session of the address"`, nil},
		{"a catch-up", signInRecorded{revoked: true, caught: caught},
			"caught up what it did not revoke", []string{"replaced_subjects=[sub-old]", "tokens_revoked=2", "holder=sub-new"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			logs := captureLogs(t)
			row.recorded.log(email)
			out := logs.String()
			lines := 0
			for line := range strings.Lines(out) {
				if strings.Contains(line, row.message) {
					lines++
				}
			}
			if lines != 1 {
				t.Fatalf("logged %q; want one line holding %q", out, row.message)
			}
			for _, want := range append([]string{"level=WARN", "login_hash=" + model.LoginHash(email)}, row.fields...) {
				if !strings.Contains(out, want) {
					t.Errorf("the line %q does not hold %s", out, want)
				}
			}
			if strings.Contains(strings.ToLower(out), strings.ToLower(email)) {
				t.Errorf("the log holds the address %s:\n%s", email, out)
			}
		})
	}
}
