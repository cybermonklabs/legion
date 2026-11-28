package api_test

import (
	"crypto/ecdsa"
	"net/http"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/record"
)

// judgedSeconds is the seconds DEEL_API_KEY's judge tag on this server gives the model judge: well
// inside the 15 minutes of the human-tier class, so a record can ask for less than the class
// allows and still more than its tag does.
const judgedSeconds = 300

// judgedModel is the model a forged judge actor here names.
const judgedModel = "claude-opus-4-6"

// newJudgeServer is newTestServer with DEEL_API_KEY behind the airlock, its proxy tag naming a kind
// of the profiles file, and tagged with a judge of judgedSeconds. The grant chain verifier reads
// that tag from the server's live policy, as cmd/cipherd builds it; a test that moves the tag under
// a grant retags the key afterwards.
func newJudgeServer(t *testing.T) *testServer {
	t.Helper()
	return newProxiedTestServer(t, []string{"DEEL_API_KEY"}, func(d *api.Deps) {
		tagJudge(t, d, "DEEL_API_KEY", judgedSeconds, "")
	})
}

// grantOfJudgedRecord drives one request through the routes a session and an approval page use —
// POST /v1/requests, the record's own page, GET /v1/requests/{id} — and then rewrites the
// decision the page wrote into one the model judge made: the judge's own actor and no login at
// all, which is how the broker writes its approvals (record.JudgeActor, a null login). That is
// the forged half of a forged or stale judged record: a row saying the judge decided this, on a
// record the tags may never have let it decide, or no longer do. Everything else about the
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
// releases only while its keys' judge tags still admit that record — the whole of what a judge's
// approval rests on, since no person's login stands behind it. Every release re-derives the
// chain (VerifyChain), so the two records the tags do not admit are refused at the route a
// session reads values through, whether the row was forged outright or the tag moved under a
// grant that was honestly judged: a record naming a key that needed a decision and that carries no
// judge tag, and one whose signed lifetime is past the seconds the key's tag gives the judge now.
// The records the tags do admit release — one whose reason names an issue and one whose reason
// names none, since what the reason says is the judge's to read and no condition of the chain, and
// one whose judge key stands beside WORKER_TOKEN, which the session got at once and which needed
// no decision of anyone's — which is what makes the two refusals the tags' doing and not the
// rewrite's.
func TestAForgedJudgedRecordReleasesNothing(t *testing.T) {
	t.Parallel()
	const judgeableReason = "INFRA-1353 needs the key to drive the stack"
	for _, c := range []struct {
		name         string
		reason       string
		lifetime     int
		secrets      []string
		chainSeconds uint32
		released     bool
	}{
		{
			name: "a record naming a key that needed a decision and that carries no judge tag", reason: judgeableReason,
			lifetime: judgedSeconds, secrets: []string{"SHARED_KEY"},
		},
		{
			name: "a judge key beside a key the session got at once", reason: judgeableReason,
			lifetime: judgedSeconds, secrets: []string{"DEEL_API_KEY", "WORKER_TOKEN"}, released: true,
		},
		{
			// The tag gave the judge five minutes when the request was written, and two now.
			name: "a lifetime past the seconds the key's tag gives the judge now", reason: judgeableReason,
			lifetime: judgedSeconds, secrets: []string{"DEEL_API_KEY"}, chainSeconds: 120,
		},
		{
			name: "a reason naming no issue", reason: "the demo needs the key",
			lifetime: judgedSeconds, secrets: []string{"DEEL_API_KEY"}, released: true,
		},
		{
			name: "a record the tags admit", reason: judgeableReason,
			lifetime: judgedSeconds, secrets: []string{"DEEL_API_KEY"}, released: true,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			ts := newJudgeServer(t)
			enrollmentID, sessionKey := ts.newSessionEnrollment(t, "session-"+t.Name(), testApprover)
			grantID := grantOfJudgedRecord(t, ts, enrollmentID, sessionKey, c.reason, c.lifetime, c.secrets...)
			if c.chainSeconds != 0 {
				policytest.JudgeTags(t, ts.Secrets, ts.machine.Policy, "DEEL_API_KEY", int(c.chainSeconds), "")
			}

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
			if slices.Contains(c.secrets, "WORKER_TOKEN") && answer.Values["WORKER_TOKEN"] != "worker-v1" {
				t.Fatalf("the values are %v; want WORKER_TOKEN, which the session got at once, released beside the judged name", answer.Values)
			}
		})
	}
}
