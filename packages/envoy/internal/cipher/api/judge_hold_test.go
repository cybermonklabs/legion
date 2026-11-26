// packages/envoy/internal/cipher/api/judge_hold_test.go
// What the broker's answers say about a request for keys the model judge decides — held for the
// judge, linking no approval page, refused by the decision routes — and what GET /v1/settings
// says about which keys those are and on what guidance.
package api_test

import (
	"crypto/ecdsa"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// tagJudge gives the judge seconds of name, and guidance unless it is "", on the server d builds:
// the judge and guidance tags on its stored secret, as its owner's retag writes them, and the
// policy read again (policytest.JudgeTags).
func tagJudge(t *testing.T, d *api.Deps, name string, seconds int, guidance string) {
	t.Helper()
	store, isAWS := d.Machine.Secrets.(secrets.AWS)
	local, isLocal := store.Client.(*secrets.Local)
	if !isAWS || !isLocal {
		t.Fatalf("the test server's store is %T, not the secrets.Local its fixture holds", d.Machine.Secrets)
	}
	policytest.JudgeTags(t, local, d.Policy, name, seconds, guidance)
}

// judgeTags tags DEEL_API_KEY, testApprover's own human-tier secret, with a judge of 900 seconds
// and guidance, and SHARED_KEY, which nobody owns, with a judge of 300.
func judgeTags(t *testing.T, d *api.Deps) {
	t.Helper()
	tagJudge(t, d, "DEEL_API_KEY", 900, "grant for the payroll export job")
	tagJudge(t, d, "SHARED_KEY", 300, "")
}

// withJudge wires a judge to the server's machine, with the tags judgeTags writes.
func withJudge(t *testing.T) func(*api.Deps) {
	return func(d *api.Deps) {
		judgeTags(t, d)
		d.Machine.Judge = func(requests.PendingRequest) {}
	}
}

// withTagsAndNoJudge is withJudge's tags on a broker that runs no judge.
func withTagsAndNoJudge(t *testing.T) func(*api.Deps) {
	return func(d *api.Deps) { judgeTags(t, d) }
}

// createFor signs and sends one request for names from the session, answering its status code and
// its create answer.
func (ts *testServer) createFor(t *testing.T, sessionKey *ecdsa.PrivateKey, enrollmentID, reason string, lifetime int, names ...string) (int, wireCreateRequestResponse, []byte) {
	t.Helper()
	status, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": signAgentSecretRequestFor(t, sessionKey, ts.URL, reason, lifetime, names...), "session_id": nil})
	return status, decode[wireCreateRequestResponse](t, body), body
}

// TestARequestTheJudgeHoldsLinksNoPage: the create answer and the status answer of a request for a
// key the judge decides say the judge is deciding it and carry no approval page — there is nothing
// for a person to approve — and the person's pending list does not show it. No reason cites
// anything. The same request on a broker that runs no judge is an ordinary card, with its page.
func TestARequestTheJudgeHoldsLinksNoPage(t *testing.T) {
	ts := newTestServerWith(t, withJudge(t))
	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "session-"+t.Name(), testApprover)
	status, held, body := ts.createFor(t, sessionKey, enrollmentID, "export the payroll", 600, "DEEL_API_KEY")
	if status != http.StatusOK || held.State != "pending" || held.ApproveURL != nil || held.JudgeDeciding == nil || !*held.JudgeDeciding {
		t.Fatalf("the create answer = %d %s; want pending, judge_deciding true and no approve_url", status, body)
	}
	code, body := ts.session(t, sessionKey, enrollmentID, http.MethodGet, "/v1/requests/"+held.RequestID, nil)
	got := decode[wireRequestStatus](t, body)
	if code != http.StatusOK || got.ApproveURL != nil || got.JudgeDeciding == nil || !*got.JudgeDeciding {
		t.Fatalf("the status = %d %s; want judge_deciding true and no approve_url", code, body)
	}
	_, body = ts.ui(t, http.MethodGet, "/v1/pending?approver="+testApprover, nil)
	if strings.Contains(string(body), *held.RecordID) {
		t.Fatalf("GET /v1/pending = %s; want the held request on nobody's list", body)
	}

	plain := newTestServerWith(t, withTagsAndNoJudge(t))
	enrollmentID, sessionKey = plain.newSessionEnrollment(t, "session-"+t.Name()+"-plain", testApprover)
	status, card, body := plain.createFor(t, sessionKey, enrollmentID, "export the payroll", 600, "DEEL_API_KEY")
	if status != http.StatusOK || card.State != "pending" || card.ApproveURL == nil || card.JudgeDeciding != nil {
		t.Fatalf("the create answer on a broker with no judge = %d %s; want an ordinary card with its approve_url", status, body)
	}
}

// TestAJudgeKeysLongerLifetimeIsRefusedWhenWritten: a judge key grants at most its own judge
// seconds, and a request that asks for longer is refused in its create answer, naming the --ttl
// that would do, as a max-grant tag refuses one. Nothing waits for anybody.
func TestAJudgeKeysLongerLifetimeIsRefusedWhenWritten(t *testing.T) {
	ts := newTestServerWith(t, withJudge(t))
	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "session-"+t.Name(), testApprover)
	status, _, body := ts.createFor(t, sessionKey, enrollmentID, "read the shared key", 600, "SHARED_KEY")
	refusal := decode[wireError](t, body)
	if status != http.StatusBadRequest || refusal.Code != "GRANT_REQUIRES_LIFETIME" || !strings.Contains(refusal.Error, "--ttl 5m") {
		t.Fatalf("a request for 600 seconds of a key whose judge grants 300 = %d %s; want 400 GRANT_REQUIRES_LIFETIME naming --ttl 5m", status, body)
	}
}

// TestTheDecisionRoutesRefuseARecordTheJudgeHolds: POST /v1/credential-requests/{record}/approve
// and /deny, which refuse every secret request, refuse one the model judge holds by saying so —
// 403 JUDGE_DECIDES — and never with the approval page they name for a request a person decides,
// which a card on a broker with no judge still gets. Nothing is written.
func TestTheDecisionRoutesRefuseARecordTheJudgeHolds(t *testing.T) {
	ts := newTestServerWith(t, withJudge(t))
	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "session-"+t.Name(), testApprover)
	status, held, body := ts.createFor(t, sessionKey, enrollmentID, "export the payroll", 600, "DEEL_API_KEY")
	if status != http.StatusOK || held.RecordID == nil {
		t.Fatalf("POST /v1/requests = %d %s; want a pending request", status, body)
	}
	for _, route := range []string{"approve", "deny"} {
		status, body := ts.ui(t, http.MethodPost, "/v1/credential-requests/"+*held.RecordID+"/"+route, map[string]any{"approver": testApprover})
		refusal := decode[wirePageRefusal](t, body)
		if status != http.StatusForbidden || refusal.Code != "JUDGE_DECIDES" || refusal.ApproveURL != "" || strings.Contains(string(body), "/approve/") {
			t.Fatalf("%s of the held request = %d %s; want 403 JUDGE_DECIDES and no page", route, status, body)
		}
	}
	code, body := ts.session(t, sessionKey, enrollmentID, http.MethodGet, "/v1/requests/"+held.RequestID, nil)
	if got := decode[wireRequestStatus](t, body); code != http.StatusOK || got.State != "pending" {
		t.Fatalf("after the refused decisions the request reads %d %s; want pending", code, body)
	}

	plain := newTestServer(t)
	enrollmentID, sessionKey = plain.newSessionEnrollment(t, "session-"+t.Name()+"-plain", testApprover)
	_, card, _ := plain.createFor(t, sessionKey, enrollmentID, "export the payroll", 600, "DEEL_API_KEY")
	status, body = plain.ui(t, http.MethodPost, "/v1/credential-requests/"+*card.RecordID+"/approve", map[string]any{"approver": testApprover})
	if refusal := decode[wirePageRefusal](t, body); status != http.StatusForbidden || refusal.Code != "DECIDE_ON_BROKER_PAGE" || refusal.ApproveURL == "" {
		t.Fatalf("approve of a card = %d %s; want 403 DECIDE_ON_BROKER_PAGE naming its page", status, body)
	}
}

// TestTheDecisionRoutesRefuseARecordOfAnotherAudienceAsBefore: a secret request signed for another
// BROKER_PUBLIC_URL, which no read of this broker verifies, is refused 403 by the decision routes
// as every secret request is, never 500: on a broker with a judge, the hold is read and not the
// record, so a held one is JUDGE_DECIDES and any other DECIDE_ON_BROKER_PAGE. Nothing is written.
func TestTheDecisionRoutesRefuseARecordOfAnotherAudienceAsBefore(t *testing.T) {
	for _, c := range []struct {
		name   string
		judged bool
		code   string
	}{
		{"held by the judge", true, "JUDGE_DECIDES"},
		{"a person's card", false, "DECIDE_ON_BROKER_PAGE"},
	} {
		t.Run(c.name, func(t *testing.T) {
			adjust := func(*api.Deps) {}
			if c.judged {
				adjust = withJudge(t)
			}
			ts := newTestServerWith(t, adjust)
			enrollmentID, sessionKey := ts.newSessionEnrollment(t, "session-"+t.Name(), testApprover)
			current := ts.machine.Audience
			ts.machine.Audience = "https://the-old-public-url.example"
			stale, err := ts.machine.Create(t.Context(), enrollmentID, signAgentSecretRequestFor(t, sessionKey, ts.machine.Audience, "export the payroll", 600, "DEEL_API_KEY"), "")
			ts.machine.Audience = current
			if err != nil || stale.RecordID == nil || stale.State != "pending" {
				t.Fatalf("Create under the old audience = %+v, %v; want a pending request", stale, err)
			}
			for _, route := range []string{"approve", "deny"} {
				status, body := ts.ui(t, http.MethodPost, "/v1/credential-requests/"+*stale.RecordID+"/"+route, map[string]any{"approver": testApprover})
				if code := decode[wirePageRefusal](t, body).Code; status != http.StatusForbidden || code != c.code {
					t.Fatalf("%s of a record signed for another audience = %d %s; want 403 %s", route, status, body, c.code)
				}
			}
		})
	}
}

// TestTheSettingsNameEachJudgeKeyAndItsGuidance: GET /v1/settings names each key whose judge tag
// gives the judge seconds of it, with those seconds and its guidance tag, so an agent reads before
// it asks who decides a request for it and what the judge will want. It is the policy's answer: a
// broker with the same tags and no judge names the same keys, and one whose keys carry no judge
// tag names none.
func TestTheSettingsNameEachJudgeKeyAndItsGuidance(t *testing.T) {
	type judgeKey struct {
		Seconds  int    `json:"seconds"`
		Guidance string `json:"guidance"`
	}
	type settings struct {
		JudgeKeys map[string]judgeKey `json:"judge_keys"`
	}
	for name, adjust := range map[string]func(*api.Deps){
		"a broker with a judge":                    withJudge(t),
		"a broker with the same tags and no judge": withTagsAndNoJudge(t),
	} {
		ts := newTestServerWith(t, adjust)
		code, body := ts.req(t, http.MethodGet, "/v1/settings", nil, nil)
		got := decode[settings](t, body).JudgeKeys
		if code != http.StatusOK || len(got) != 2 || got["DEEL_API_KEY"] != (judgeKey{900, "grant for the payroll export job"}) || got["SHARED_KEY"] != (judgeKey{Seconds: 300}) {
			t.Errorf("GET /v1/settings on %s = %d %s; want judge_keys DEEL_API_KEY at 900 with its guidance and SHARED_KEY at 300", name, code, body)
		}
	}
	unjudged := newTestServer(t)
	if _, body := unjudged.req(t, http.MethodGet, "/v1/settings", nil, nil); strings.Contains(string(body), "judge_keys") {
		t.Fatalf("GET /v1/settings with no key carrying a judge tag = %s; want no judge_keys", body)
	}
}

// TestTheSettingsNameTheSourceOfAJudgedProfile: a profile made from a stored secret of another
// name is served under the profile's name, and the source is served only through it, so judge_keys
// names the profile, at its source's seconds and guidance, and says which source those are
// (source), for `secret show` of that source to name its judged profiles. A name served as itself
// carries no source.
func TestTheSettingsNameTheSourceOfAJudgedProfile(t *testing.T) {
	type judgeKey struct {
		Seconds  int    `json:"seconds"`
		Guidance string `json:"guidance"`
		Source   string `json:"source"`
	}
	type settings struct {
		JudgeKeys map[string]judgeKey `json:"judge_keys"`
	}
	ts := newProfiledTestServer(t, policytest.KindsFile(
		`{"name": "DEEL_READ", "source": "DEEL_API_KEY", "proxy": "example"}`,
	), func(d *api.Deps) {
		store := d.Machine.Secrets.(secrets.AWS).Client.(*secrets.Local)
		judged := policytest.Judged(policytest.Secret("DEEL_API_KEY", testApprover, policy.TierHuman, "deel-v1"), 900, "grant for the payroll export job")
		store.Put(judged)
		store.Put(policytest.Judged(policytest.Secret("SHARED_KEY", policy.OwnerShared, policy.TierHuman, "shared-v1"), 300, ""))
		if err := d.Policy.Refresh(t.Context()); err != nil {
			t.Fatalf("policy refresh: %v", err)
		}
	})
	code, body := ts.req(t, http.MethodGet, "/v1/settings", nil, nil)
	got := decode[settings](t, body).JudgeKeys
	if code != http.StatusOK || got["DEEL_READ"] != (judgeKey{900, "grant for the payroll export job", "DEEL_API_KEY"}) || got["SHARED_KEY"] != (judgeKey{Seconds: 300}) {
		t.Errorf("GET /v1/settings = %d %s; want DEEL_READ at its source DEEL_API_KEY's 900 seconds and guidance, naming that source, and SHARED_KEY at 300 with none", code, body)
	}
}
