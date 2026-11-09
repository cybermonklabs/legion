// reason_test.go drives the reason a request names through the request machine: a request that
// names none is denied the moment one of its names would wait for a person — a human-tier
// credential, a name the session's operator withheld from it, another person's agent-tier
// credential — because nobody is asked to approve work the record does not describe, and the
// refusal says so by name rather than leaving a card on an approver's page that reads only "the
// agent wants this". A request every name of which is granted at once needs no reason and is
// granted as it always was, and what counts as a reason is what is left of it once it is trimmed:
// whitespace alone is none, one character is one.
package requests

import (
	"context"
	"crypto/ecdsa"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/record"
)

// recordRows counts the credential-request records the fixture's broker has written. A request
// denied for want of a reason opens none: no person is to be asked about it, so there is no card
// to answer, and the count is of the whole table because each fixture is a schema of its own.
func recordRows(t *testing.T, m *Machine) int {
	t.Helper()
	var count int
	if err := m.Store.Pool.QueryRow(context.Background(), `select count(*) from credential_requests`).Scan(&count); err != nil {
		t.Fatalf("count credential_requests: %v", err)
	}
	return count
}

// deniedForWantOfAReason is the whole verdict on a request denied because it named no reason:
// denied with no grant and no record, a detail naming secret as the name that refused it, no
// credential-request record for anyone's page, and a request.created audit row that says denied.
func deniedForWantOfAReason(t *testing.T, m *Machine, req Request, secret string) {
	t.Helper()
	want := secret + ": " + policy.ReasonNoReason
	if req.State != "denied" || req.GrantID != nil || req.RecordID != nil {
		t.Fatalf("Create(%s) with no reason = %+v; want it denied with no grant and no record", secret, req)
	}
	if req.Detail == nil || *req.Detail != want {
		t.Fatalf("detail = %v, want %q", req.Detail, want)
	}
	if rows := recordRows(t, m); rows != 0 {
		t.Fatalf("credential_requests rows = %d, want 0: a request no one is asked to approve opens no card", rows)
	}
	if state, _, _ := createdAudit(t, m, req.ID); state != "denied" {
		t.Fatalf("request.created audit state = %q, want denied", state)
	}
}

// withholdFromSession has the operator revoke the session's automatic grant of name, which
// withholds it from that session from then on (RevokeByApprover's withhold), so the session's
// next request for it is one a person has to approve.
func withholdFromSession(t *testing.T, m *Machine, enr string, key *ecdsa.PrivateKey, name string) {
	t.Helper()
	ctx := context.Background()
	auto, err := m.Create(ctx, enr, signRequest(t, m, key, "to withhold it again", name), "")
	if err != nil || auto.GrantID == nil {
		t.Fatalf("Create(%s) = %+v, %v; want the automatic grant the operator then revokes", name, auto, err)
	}
	if err := m.RevokeByApprover(ctx, *auto.GrantID, fixtureOperator, record.HumanActor(fixtureOperator)); err != nil {
		t.Fatalf("RevokeByApprover(%s): %v", name, err)
	}
}

// TestAReasonlessRequestOfANameThatWouldWaitIsDenied: each of the three ways a name comes to need
// a person's approval — the secret's own human tier, a withhold by the session's operator, and
// another person's agent-tier secret asked for by a session that is not that person — is denied
// when the request names no reason, and the session is told which secret refused it and what to
// do about it rather than being left with a card nobody can read. The denial leaves nothing
// behind: no grant, no credential-request record for an approver's page, and a request.created
// audit row that records the denial, which is what a later read of the trail answers "what became
// of this request?" from. The same request with a reason waits for that person, which is what
// makes the denial the missing reason's doing and not the name's.
func TestAReasonlessRequestOfANameThatWouldWaitIsDenied(t *testing.T) {
	for _, tc := range []struct {
		name, secret string
		withheld     bool
	}{
		{name: "a human-tier credential", secret: "DEEL_API_KEY"},
		{name: "a name the session's operator withheld from it", secret: "AUTO_TOKEN", withheld: true},
		{name: "another person's agent-tier credential", secret: "ALICE_KEY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			m, enr, key, _ := newFixture(t)
			if tc.withheld {
				withholdFromSession(t, m, enr, key, tc.secret)
			}

			want := tc.secret + ": " + policy.ReasonNoReason
			req, err := m.Create(ctx, enr, signRequest(t, m, key, "", tc.secret), "")
			if err != nil {
				t.Fatalf("Create(%s) with no reason: %v", tc.secret, err)
			}
			deniedForWantOfAReason(t, m, req, tc.secret)
			if len(req.Secrets) != 1 || req.Secrets[0].Name != tc.secret || req.Secrets[0].Decision != policy.Deny {
				t.Fatalf("decisions = %+v; want %s decided %s", req.Secrets, tc.secret, policy.Deny)
			}
			got, err := m.Get(ctx, req.ID)
			if err != nil || got.Detail == nil || *got.Detail != want {
				t.Fatalf("Get = %+v, %v; want the row's decision_detail to read %q", got, err, want)
			}

			waiting, err := m.Create(ctx, enr, signRequest(t, m, key, "rotating the key", tc.secret), "")
			if err != nil || waiting.State != "pending" || waiting.RecordID == nil {
				t.Fatalf("Create(%s) with a reason = %+v, %v; want it to wait for its approver", tc.secret, waiting, err)
			}
		})
	}
}

// TestAReasonlessRequestIsDeniedByTheOneNameThatWouldWait: the rule reads the whole request, not
// its first name. A request for a name granted at once beside one that would wait is denied
// whole, and its detail names the waiting one alone, since that is the name the session has to
// ask again for; the name it would have got at once keeps the decision it would have had, so the
// session can see what the request cost it. A request every name of which is granted at once
// carries no one's approval and so needs no reason: it is granted with a grant and no detail, as
// it was before the rule existed.
func TestAReasonlessRequestIsDeniedByTheOneNameThatWouldWait(t *testing.T) {
	ctx := context.Background()
	m, enr, key, _ := newFixture(t)

	denied, err := m.Create(ctx, enr, signRequest(t, m, key, "", "AUTO_TOKEN", "DEEL_API_KEY"), "")
	if err != nil {
		t.Fatalf("Create(AUTO_TOKEN, DEEL_API_KEY) with no reason: %v", err)
	}
	deniedForWantOfAReason(t, m, denied, "DEEL_API_KEY")
	if len(denied.Secrets) != 2 || denied.Secrets[0].Decision != policy.Automatic || denied.Secrets[1].Decision != policy.Deny {
		t.Fatalf("decisions = %+v; want AUTO_TOKEN %s and DEEL_API_KEY %s", denied.Secrets, policy.Automatic, policy.Deny)
	}

	granted, err := m.Create(ctx, enr, signRequest(t, m, key, "", "AUTO_TOKEN"), "")
	if err != nil || granted.State != "granted" || granted.GrantID == nil || granted.Detail != nil {
		t.Fatalf("Create(AUTO_TOKEN) with no reason = %+v, %v; want it granted at once: no name waits, so none needs a reason", granted, err)
	}
	if released, err := m.Values(ctx, *granted.GrantID, enr, nil); err != nil || released.Values["AUTO_TOKEN"] == "" {
		t.Fatalf("Values of the reasonless automatic grant = %v, %v; want the value", released.Values, err)
	}
}

// TestADenialTheRuleAndThePolicyBothSpeakToSaysBoth: SERVICE_KEY is the fixture service's own
// secret, which no person approves, so the policy denies it to this operator's session with no
// reason to name; DEEL_API_KEY is human-tier, so with no reason the rule denies it by name. The
// detail has to carry both sentences: told only "ask again with --reason", the session would ask
// again with one and be refused all the same, learning nothing about the name that refuses it
// whatever it says.
func TestADenialTheRuleAndThePolicyBothSpeakToSaysBoth(t *testing.T) {
	ctx := context.Background()
	m, enr, key, _ := newFixture(t)

	req, err := m.Create(ctx, enr, signRequest(t, m, key, "", "SERVICE_KEY", "DEEL_API_KEY"), "")
	if err != nil {
		t.Fatalf("Create(SERVICE_KEY, DEEL_API_KEY) with no reason: %v", err)
	}
	want := "DEEL_API_KEY: " + policy.ReasonNoReason + "; policy denies at least one requested secret"
	if req.State != "denied" || req.Detail == nil || *req.Detail != want {
		t.Fatalf("Create = %+v (detail %v); want it denied reading %q", req, req.Detail, want)
	}
	if len(req.Secrets) != 2 || req.Secrets[0].Decision != policy.Deny || req.Secrets[1].Decision != policy.Deny {
		t.Fatalf("decisions = %+v; want both names denied", req.Secrets)
	}
	got, err := m.Get(ctx, req.ID)
	if err != nil || got.Detail == nil || *got.Detail != want {
		t.Fatalf("Get = %+v, %v; want the row's decision_detail to read %q", got, err, want)
	}
}

// TestWhatCountsAsAReasonIsWhatIsLeftOfItTrimmed is the rule's boundary: the reason is read
// trimmed, so a request signing a space alone is denied exactly as one signing none is — the
// session gets the same answer whether its person typed --reason "" or --reason " " — while one
// character of reason is a reason, and the request waits for its approver with a
// credential-request record for that person to decide.
func TestWhatCountsAsAReasonIsWhatIsLeftOfItTrimmed(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		wait         bool
	}{
		{name: "whitespace alone is no reason", reason: " "},
		{name: "one character is a reason", reason: "x", wait: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			m, enr, key, _ := newFixture(t)

			req, err := m.Create(ctx, enr, signRequest(t, m, key, tc.reason, "DEEL_API_KEY"), "")
			if err != nil {
				t.Fatalf("Create(DEEL_API_KEY) with reason %q: %v", tc.reason, err)
			}
			if tc.wait {
				if req.State != "pending" || req.RecordID == nil || req.GrantID != nil {
					t.Fatalf("Create = %+v; want it pending with a record to decide", req)
				}
				if rows := recordRows(t, m); rows != 1 {
					t.Fatalf("credential_requests rows = %d, want 1: the approver has a card to answer", rows)
				}
				if state, _, _ := createdAudit(t, m, req.ID); state != "pending" {
					t.Fatalf("request.created audit state = %q, want pending", state)
				}
				return
			}
			deniedForWantOfAReason(t, m, req, "DEEL_API_KEY")
			if len(req.Secrets) != 1 || req.Secrets[0].Decision != policy.Deny {
				t.Fatalf("decisions = %+v; want DEEL_API_KEY decided %s", req.Secrets, policy.Deny)
			}
		})
	}
}
