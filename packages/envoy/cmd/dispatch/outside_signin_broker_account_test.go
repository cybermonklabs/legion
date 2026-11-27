package main

import (
	"encoding/json"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/cipher"
)

// approveRelayed is the call the broker is told of for an approve of rec1 by reassignedEmail,
// naming the account subject under s's sign-in issuer, and no account at all for "", and carrying
// the address's session generation as it stands now (sessionGeneration): called right after the
// approve, with nothing between to move it, that is the generation the approve's check read.
func (s *outsideSignIn) approveRelayed(t *testing.T, subject string) string {
	t.Helper()
	code, generation := "ABCD-1234", s.sessionGeneration(t, reassignedEmail)
	decision := cipher.Decision{Approver: reassignedEmail, Code: &code, ApproverSessionGeneration: &generation}
	if subject != "" {
		decision.ApproverIssuer, decision.ApproverSubject = s.pool.URL(), subject
	}
	body, err := json.Marshal(decision)
	if err != nil {
		t.Fatal(err)
	}
	return "POST /v1/credential-requests/rec1/approve " + string(body)
}
