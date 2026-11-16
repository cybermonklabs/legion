package api_test

import (
	"crypto/ecdsa"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/airlock"
	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
)

// judgedSeconds is the seconds the rules on this server's own airlock entry let the model judge
// decide of DEEL_API_KEY: well inside the 15 minutes of the human-tier class, so a record can
// ask for less than the class allows and still more than its entry does.
const judgedSeconds = 300

// judgedModel is the model a forged judge actor here names.
const judgedModel = "claude-opus-4-6"

// newJudgeServer is newTestServer with one airlock entry that gives the judge judgedSeconds of
// DEEL_API_KEY, and the grant chain verifier built against those rules, as cmd/broker builds it
// from the rules file it loaded. Without the rebuild the verifier would hold every judge
// approval to no bound at all and refuse them all, which would pass the tests below for the
// wrong reason.
func newJudgeServer(t *testing.T) *testServer {
	t.Helper()
	return newTestServerWith(t, func(d *api.Deps) {
		d.Machine.Airlock = airlock.Rules{Secrets: []airlock.SecretRule{
			{Name: "DEEL_API_KEY", Hosts: []string{"api.deel.example"}, Judge: new(uint32(judgedSeconds))},
		}}
		d.Machine.AirlockProxyURL = "http://127.0.0.1:13381"
		d.Machine.AirlockCACertificate = "-----BEGIN CERTIFICATE-----\ntest-airlock-ca\n-----END CERTIFICATE-----\n"
		d.Machine.Chain = requests.NewChainVerifier(d.Machine.Store, d.PublicURL, time.Minute, d.Machine.Airlock)
	})
}

// grantOfJudgedRecord drives one request through the routes a session and an approval page use —
// POST /v1/requests, the record's own page, GET /v1/requests/{id} — and then rewrites the
// decision the page wrote into one the model judge made: the judge's own actor and no login at
// all, which is how the broker writes its approvals (record.JudgeActor, a null login). That is
// the forged half of a forged or stale judged record: a row saying the judge decided this, on a
// record the rules may never have let it decide, or no longer do. Everything else about the
// record and its grant is what the real routes wrote.
func grantOfJudgedRecord(t *testing.T, ts *testServer, enrollmentID string, sessionKey *ecdsa.PrivateKey, reason string, lifetime int, names ...string) string {
	t.Helper()
	compact := signAgentSecretRequestFor(t, sessionKey, ts.URL, reason, lifetime, names...)
	status, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": compact, "session_id": nil})
	if status != http.StatusOK {
		t.Fatalf("POST /v1/requests = %d, want 200: %s", status, body)
	}
	created := decode[wireCreateRequestResponse](t, body)
	if created.State != "pending" || created.RecordID == nil {
		t.Fatalf("POST /v1/requests = %+v, want a pending request for a person to decide", created)
	}
	ts.decide(t, testApprover, *created.RecordID, true)

	if _, err := ts.Store.Pool.Exec(t.Context(),
		`update credential_request_events set actor=$1, login=null where record_id=$2 and event='approved'`,
		record.JudgeActor(judgedModel), *created.RecordID); err != nil {
		t.Fatalf("rewrite the decision as the judge's: %v", err)
	}

	status, body = ts.session(t, sessionKey, enrollmentID, http.MethodGet, "/v1/requests/"+created.RequestID, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/requests/{id} = %d: %s", status, body)
	}
	read := decode[wireRequestStatus](t, body)
	if read.GrantID == nil || *read.GrantID == "" {
		t.Fatalf("request status = %+v, want the approval's grant", read)
	}
	return *read.GrantID
}

// TestAForgedJudgedRecordReleasesNothing: a grant whose record carries the model judge's actor
// releases only while the airlock rules still admit that record — the whole of what a judge's
// approval rests on, since no person's login stands behind it. Every release re-derives the
// chain (VerifyChain), so the three records the rules never admit are refused at the route a
// session reads values through, whether the row was forged outright or the rules moved under a
// grant that was honestly judged: a record naming two secrets, one whose signed lifetime is past
// the seconds its own entry gives the judge, and one whose signed reason names no Dispatch
// issue. The record the rules do admit releases, which is what makes the three refusals the
// rules' doing and not the rewrite's.
func TestAForgedJudgedRecordReleasesNothing(t *testing.T) {
	const judgeableReason = "INFRA-1353 needs the key to drive the stack"
	for _, c := range []struct {
		name     string
		reason   string
		lifetime int
		secrets  []string
		released bool
	}{
		{
			name: "a record naming two secrets", reason: judgeableReason,
			lifetime: judgedSeconds, secrets: []string{"DEEL_API_KEY", "WORKER_TOKEN"},
		},
		{
			// Ten minutes: inside the 15 the class allows, past the five this
			// name's entry gives the judge.
			name: "a lifetime past the seconds the entry gives the judge", reason: judgeableReason,
			lifetime: 600, secrets: []string{"DEEL_API_KEY"},
		},
		{
			name: "a reason naming no issue", reason: "the demo needs the key",
			lifetime: judgedSeconds, secrets: []string{"DEEL_API_KEY"},
		},
		{
			name: "a record the rules admit", reason: judgeableReason,
			lifetime: judgedSeconds, secrets: []string{"DEEL_API_KEY"}, released: true,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			ts := newJudgeServer(t)
			enrollmentID, sessionKey := ts.newSessionEnrollment(t, "session-"+t.Name(), testApprover)
			grantID := grantOfJudgedRecord(t, ts, enrollmentID, sessionKey, c.reason, c.lifetime, c.secrets...)

			status, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/grants/"+grantID+"/values", nil)
			if !c.released {
				if status != http.StatusForbidden || decode[wireError](t, body).Code != "GRANT_CHAIN_INVALID" {
					t.Fatalf("POST /v1/grants/{id}/values = %d %s, want 403 GRANT_CHAIN_INVALID", status, body)
				}
				return
			}
			if status != http.StatusOK {
				t.Fatalf("POST /v1/grants/{id}/values = %d %s, want the judged grant released", status, body)
			}
			answer := decode[wireAirlockValues](t, body)
			if answer.Airlock == nil {
				t.Fatalf("the answer carries no airlock: %s", body)
			}
			if _, named := answer.Airlock.Names["DEEL_API_KEY"]; !named {
				t.Fatalf("the airlock names %v; want the judged name among them", answer.Airlock.Names)
			}
		})
	}
}
