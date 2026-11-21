// groups_e2e_test.go boots the broker as e2e_test.go does, with a Google group owning secrets: a
// group reader at stand-ins of the Directory API and IAM, and the anthropic-federation minter at a
// stand-in of Anthropic's token exchange, every step driven as HTTP against the broker's own
// routes and its approval page.
package e2e

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/groups"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The group, its member, a person outside it, the reader account the broker reads the group as,
// the account an anthropic-federation secret is bound to, and the bearers the stand-ins hand
// out: example names alone, since the repository is public, and none of them a credential.
const (
	approversGroup    = "approvers@example.com"
	approversForm     = record.GroupPrefix + approversGroup
	groupMember       = "member@example.com"
	groupOutsider     = "outsider@example.com"
	readerAccount     = "group-reader@example-project.iam.gserviceaccount.com"
	federationAccount = "anthropic-admin@example-project.iam.gserviceaccount.com"
	podOwnBearer      = "pod-" + "own"
	readerBearer      = "reader-" + "minted"
	identityBearer    = "identity-" + "minted"
	federatedToken    = "sk-ant-oat01-" + "e2e-federated"
)

// groupSecrets are the namespace of this test: the group's agent-tier secret, which a member's
// session gets at once; its human-tier secret, which a member approves; a shared agent-tier
// secret whose requesters name the group; and the group's anthropic-federation secret, bound to
// federationAccount.
func groupSecrets() []secrets.LocalSecret {
	listed := policytest.Secret("LISTED_TOKEN", policy.OwnerShared, policy.TierAgent, "listed-v1")
	listed.Tags[policy.TagRequesters] = approversForm
	value, _ := json.Marshal(map[string]string{
		"organization_id": "00000000-0000-4000-8000-000000000000", "federation_rule_id": "fdrl_01examplerule",
		"service_account_id": "svac_01exampletarget", "service_account": federationAccount,
	})
	return []secrets.LocalSecret{
		policytest.Secret("TEAM_TOKEN", approversForm, policy.TierAgent, "team-token-v1"),
		policytest.Secret("TEAM_KEY", approversForm, policy.TierHuman, "team-key-v1"),
		listed,
		policytest.TypeSecret("FED_ADMIN", approversForm, policy.TierHuman, policy.TypeAnthropicFederation, string(value)),
	}
}

// e2eClock is the clock the group reader dates its reads by: the two-hour cut-off is no test's
// to wait out.
type e2eClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *e2eClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *e2eClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// groupStands are the broker's three upstreams for this test: IAM, which mints the reader
// account's access token and the federation account's identity token; the Directory API, which
// lists members as the reader account alone, and can be made to fail; and Anthropic's token
// exchange. Each keeps what it was asked.
type groupStands struct {
	iam, directory, anthropic *httptest.Server

	mu         sync.Mutex
	members    []string
	failing    bool
	iamAsked   []map[string]any
	iamPaths   []string
	exchanges  []map[string]any
	dirBearers []string
}

func newGroupStands(t *testing.T, members ...string) *groupStands {
	t.Helper()
	s := &groupStands{members: members}
	s.iam = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		s.mu.Lock()
		s.iamPaths, s.iamAsked = append(s.iamPaths, r.URL.Path), append(s.iamAsked, body)
		s.mu.Unlock()
		switch r.URL.Path {
		case "/v1/projects/-/serviceAccounts/" + readerAccount + ":generateAccessToken":
			_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": readerBearer, "expireTime": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
		case "/v1/projects/-/serviceAccounts/" + federationAccount + ":generateIdToken":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": identityBearer})
		default:
			http.Error(w, `{"error":{"code":403,"status":"PERMISSION_DENIED"}}`, http.StatusForbidden)
		}
	}))
	t.Cleanup(s.iam.Close)
	s.directory = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.dirBearers = append(s.dirBearers, r.Header.Get("Authorization"))
		members, failing := append([]string(nil), s.members...), s.failing
		s.mu.Unlock()
		switch {
		case failing:
			http.Error(w, `{"error":{"code":503,"errors":[{"reason":"backendError"}]}}`, http.StatusServiceUnavailable)
		case r.Header.Get("Authorization") != "Bearer "+readerBearer || r.URL.Path != "/admin/directory/v1/groups/"+approversGroup+"/members":
			http.Error(w, `{"error":{"code":401,"errors":[{"reason":"authError"}]}}`, http.StatusUnauthorized)
		default:
			listed := make([]map[string]string, 0, len(members))
			for _, m := range members {
				listed = append(listed, map[string]string{"email": m, "type": "USER", "role": "MEMBER"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"members": listed})
		}
	}))
	t.Cleanup(s.directory.Close)
	s.anthropic = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		s.mu.Lock()
		s.exchanges = append(s.exchanges, body)
		s.mu.Unlock()
		if r.URL.Path != "/v1/oauth/token" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": federatedToken, "token_type": "Bearer", "expires_in": 3600})
	}))
	t.Cleanup(s.anthropic.Close)
	return s
}

// groupAsk is POST /v1/requests's answer, the fields this test reads.
type groupAsk struct {
	RequestID string  `json:"request_id"`
	State     string  `json:"state"`
	GrantID   *string `json:"grant_id"`
	RecordID  *string `json:"record_id"`
	Approver  *string `json:"approver"`
	Detail    *string `json:"detail"`
}

// ask is the session's POST /v1/requests for name, signed with lifetime (nil for none).
func (s *e2eServer) ask(t *testing.T, key *ecdsa.PrivateKey, enrollmentID, reason string, lifetime *int, name string) groupAsk {
	t.Helper()
	compact, err := record.Sign(key, s.URL, []record.AuthorizationDetail{{Type: "agent_secret", Identifier: name, Actions: []string{"inject"}}}, reason, "", lifetime, time.Now())
	if err != nil {
		t.Fatalf("record.Sign: %v", err)
	}
	status, body := s.session(t, key, enrollmentID, http.MethodPost, "/v1/requests", map[string]any{"request": compact, "session_id": nil})
	if status != http.StatusOK {
		t.Fatalf("POST /v1/requests (%s) = %d: %s", name, status, body)
	}
	return decode[groupAsk](t, body)
}

// refused fails t unless the session's release of grantID answers 403 with code.
func (s *e2eServer) refused(t *testing.T, key *ecdsa.PrivateKey, enrollmentID, grantID, code string) {
	t.Helper()
	status, body := s.session(t, key, enrollmentID, http.MethodPost, "/v1/grants/"+grantID+"/values", nil)
	if status != http.StatusForbidden || decode[wireError](t, body).Code != code {
		t.Fatalf("values of grant %s = %d %s, want 403 %s", grantID, status, body, code)
	}
}

// TestGroupOwnersEndToEnd boots a broker whose secrets a Google group owns and drives it as HTTP:
//
//  1. the member's session gets the group's agent-tier secret with no approval, and a session of
//     a person outside the group waits for the group's approval of it;
//  2. the outsider's session asks for the group's human-tier secret: the request is on the
//     member's Inbox list and not the outsider's, the outsider's page offers no decision, the
//     member approves it on the broker's page, the session gets the value, and the record names
//     the group as approver and the member as the one who decided;
//  3. a secret whose requesters name the group goes to the member's session at once and is
//     denied to the outsider's, requester-not-listed;
//  4. the member's session asks for the group's anthropic-federation secret, a member approves
//     it, and the release is the sk-ant-oat01- token Anthropic's exchange answers for the bound
//     account's identity token;
//  5. the Directory fails, the clock moves past two hours with the policy unchanged, and both
//     live grants of the group's secrets, the approved one and the automatic one, are refused at
//     their next release, the approved one with one "group approval lapsed" line and no
//     broken-chain line.
func TestGroupOwnersEndToEnd(t *testing.T) {
	// The pod's own access token, which the federation minter's identity-token mint spends.
	ciphertokentest.NewMetadataStand(t)
	stands := newGroupStands(t, groupMember)
	clock := &e2eClock{now: time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)}
	ts := newE2EServerWith(t, e2eOptions{
		secrets:      groupSecrets(),
		directoryURL: stands.directory.URL, directoryAccount: readerAccount, iamURL: stands.iam.URL,
		anthropicURL:    stands.anthropic.URL + "/v1/oauth/token",
		serviceAccounts: map[string]string{"FED_ADMIN": federationAccount},
		now:             clock.Now,
	})
	testwait.Eventually(t, "the first policy load's read of the group", func() bool { return ts.Groups.Fresh(approversGroup) })
	if log := ts.Log.String(); !strings.Contains(log, groups.ReadMessage+" group="+approversGroup+" members=1 result=ok") {
		t.Fatalf("the broker logged no successful read of %s:\n%s", approversGroup, log)
	}

	member, memberKey := ts.loginAndEnroll(t, groupMember, "e2e-member")
	outsider, outsiderKey := ts.loginAndEnroll(t, groupOutsider, "e2e-outsider")

	automatic := ts.ask(t, memberKey, member, "the team's agent", nil, "TEAM_TOKEN")
	if automatic.State != "granted" || automatic.GrantID == nil || automatic.RecordID != nil {
		t.Fatalf("the member's TEAM_TOKEN = %+v; want granted at once, with no record", automatic)
	}
	ts.awaitValue(t, memberKey, member, *automatic.GrantID, "TEAM_TOKEN", "team-token-v1")
	waiting := ts.ask(t, outsiderKey, outsider, "the outsider's agent", nil, "TEAM_TOKEN")
	if waiting.State != "pending" || waiting.Approver == nil || *waiting.Approver != approversForm {
		t.Fatalf("the outsider's TEAM_TOKEN = %+v; want pending on %s", waiting, approversForm)
	}

	asked := ts.ask(t, outsiderKey, outsider, "the outsider needs the team key", nil, "TEAM_KEY")
	if asked.State != "pending" || asked.RecordID == nil {
		t.Fatalf("the outsider's TEAM_KEY = %+v; want pending with a record", asked)
	}
	recordID := *asked.RecordID
	ts.awaitPendingFor(t, groupMember, recordID)
	ts.refutePendingFor(t, groupOutsider, recordID)
	if form := ciphertest.DecisionForm(t, ts.signedIn(t, groupOutsider, recordID).Page(t, recordID).Body, recordID); form != nil {
		t.Fatalf("%s, outside the group, is offered a decision on the group's request", groupOutsider)
	}
	ts.signedIn(t, groupMember, recordID).Decide(t, recordID, true)
	approved := ts.grantOf(t, outsiderKey, outsider, asked.RequestID)
	ts.awaitValue(t, outsiderKey, outsider, approved, "TEAM_KEY", "team-key-v1")
	var recordApprover, decisionLogin, decidedBy string
	if err := ts.Store.Pool.QueryRow(context.Background(), `select cr.approver, ev.login, r.decided_by from credential_requests cr
		join credential_request_events ev on ev.record_id = cr.id and ev.event = 'approved'
		join requests r on r.record_id = cr.id where cr.id = $1`, recordID).Scan(&recordApprover, &decisionLogin, &decidedBy); err != nil {
		t.Fatalf("read the record of %s: %v", recordID, err)
	}
	if recordApprover != approversForm || decisionLogin != groupMember || decidedBy != groupMember {
		t.Fatalf("the record names approver %q, its approval's login %q and decided_by %q; want %s, %s and %s",
			recordApprover, decisionLogin, decidedBy, approversForm, groupMember, groupMember)
	}

	if listed := ts.ask(t, memberKey, member, "listed for the group", nil, "LISTED_TOKEN"); listed.State != "granted" {
		t.Fatalf("the member's LISTED_TOKEN = %+v; want granted at once", listed)
	}
	if denied := ts.ask(t, outsiderKey, outsider, "not on the list", nil, "LISTED_TOKEN"); denied.State != "denied" || denied.Detail == nil || !strings.Contains(*denied.Detail, policy.ReasonRequesterNotListed) {
		t.Fatalf("the outsider's LISTED_TOKEN = %+v; want denied %s", denied, policy.ReasonRequesterNotListed)
	}

	hour := 3600
	federation := ts.ask(t, memberKey, member, "the team's admin task", &hour, "FED_ADMIN")
	if federation.State != "pending" || federation.RecordID == nil {
		t.Fatalf("the member's FED_ADMIN = %+v; want pending on the group", federation)
	}
	ts.signedIn(t, groupMember, *federation.RecordID).Decide(t, *federation.RecordID, true)
	ts.awaitValue(t, memberKey, member, ts.grantOf(t, memberKey, member, federation.RequestID), "FED_ADMIN", federatedToken)
	stands.mu.Lock()
	minted := false
	for i, path := range stands.iamPaths {
		if path == "/v1/projects/-/serviceAccounts/"+federationAccount+":generateIdToken" {
			minted = stands.iamAsked[i]["audience"] == "https://api.anthropic.com" && stands.iamAsked[i]["includeEmail"] == true
		}
	}
	exchanged := len(stands.exchanges) == 1 && stands.exchanges[0]["assertion"] == identityBearer && stands.exchanges[0]["federation_rule_id"] == "fdrl_01examplerule"
	stands.mu.Unlock()
	if !minted || !exchanged {
		t.Fatalf("IAM minted the bound account's identity token for Anthropic: %v; Anthropic got one exchange of it: %v", minted, exchanged)
	}

	stands.mu.Lock()
	stands.failing = true
	stands.mu.Unlock()
	clock.advance(2*time.Hour + time.Minute)
	ts.Groups.ReadAll(t.Context())
	if ts.Groups.Fresh(approversGroup) {
		t.Fatal("the group is fresh after a failed read and two hours")
	}
	ts.refused(t, outsiderKey, outsider, approved, "GRANT_NOT_LIVE")
	ts.refused(t, memberKey, member, *automatic.GrantID, "GRANT_NOT_LIVE")
	log := ts.Log.String()
	if !strings.Contains(log, requests.GrantApproverLapsedMessage+" grant="+approved) || !strings.Contains(log, "decider="+groupMember) {
		t.Fatalf("the broker logged no %q line for grant %s:\n%s", requests.GrantApproverLapsedMessage, approved, log)
	}
	if strings.Contains(log, requests.GrantChainInvalidMessage) {
		t.Fatalf("a lapsed group approval was logged as a broken chain:\n%s", log)
	}
	if !strings.Contains(log, groups.ReadMessage+" group="+approversGroup+" members=1 result=") || !strings.Contains(log, "503") {
		t.Fatalf("the broker logged no failed read of %s:\n%s", approversGroup, log)
	}
}
