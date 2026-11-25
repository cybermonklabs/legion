package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/wire"
)

// grantsAnswered is the secrets broker's answer to GET /v1/grants?approver=erin@other.example in
// the restricted-grant rows, as a broker that does not know erin is a restricted member answers it
// (it lists every grant a full member sees): two grants of erin's own session, one a person
// approved and one the model judge approved in anyone's stead, and four of other people's sessions
// or of no person's: a grant the model judge approved in anyone's stead on bob's session, a grant
// carol approved as anyone on her own session, the automatic grant a service's pod took, and a
// grant erin approved on bob's session. The operator of erin's own session is spelled as a login
// is not stored, so the rows hold Dispatch to the canonical spelling.
const grantsAnswered = `{"grants":[
	{"grant_id":"grant-erin","granted":"approval","record_id":"rec-erin","enrollment":{"kind":"session","runtime_id":"erin-laptop:11:1","operator":"Erin@Other.example","slot":null},"names":["ERIN_TOKEN"],"approver":"alice","decided_by":"alice","expires_at":null,"created_at":"2026-11-20T06:00:00Z"},
	{"grant_id":"grant-erin-judged","granted":"approval","record_id":"rec-erin-judged","enrollment":{"kind":"session","runtime_id":"erin-laptop:11:1","operator":"erin@other.example","slot":null},"names":["ACC_SHARED"],"approver":"anyone","decided_by":"judge:model-a","expires_at":null,"created_at":"2026-11-20T05:00:00Z"},
	{"grant_id":"grant-judged","granted":"approval","record_id":"rec-judged","enrollment":{"kind":"session","runtime_id":"bob-laptop:22:1","operator":"bob","slot":null},"names":["OTHER_SHARED"],"approver":"anyone","decided_by":"judge:model-a","expires_at":null,"created_at":"2026-11-20T04:00:00Z"},
	{"grant_id":"grant-anyone","granted":"approval","record_id":"rec-anyone","enrollment":{"kind":"session","runtime_id":"carol-laptop:33:1","operator":"carol@example.com","slot":null},"names":["CAROL_SHARED"],"approver":"anyone","decided_by":"carol@example.com","expires_at":null,"created_at":"2026-11-20T03:00:00Z"},
	{"grant_id":"grant-pod","granted":"automatic","record_id":null,"enrollment":{"kind":"pod","runtime_id":"5d3c1a2e-pod","operator":"","slot":"worker"},"names":["SERVICE_KEY"],"approver":null,"decided_by":null,"expires_at":null,"created_at":"2026-11-20T02:00:00Z"},
	{"grant_id":"grant-erin-approved","granted":"approval","record_id":"rec-erin-approved","enrollment":{"kind":"session","runtime_id":"bob-laptop:22:1","operator":"bob","slot":null},"names":["BOB_KEY"],"approver":"erin@other.example","decided_by":"erin@other.example","expires_at":null,"created_at":"2026-11-20T01:00:00Z"}]}`

// grantsOfOthers is each grant of grantsAnswered whose session does not act for erin, by what it
// is, and an id the broker lists no grant for.
var grantsOfOthers = []struct{ name, id string }{
	{"the model judge's grant on bob's session", "grant-judged"},
	{"carol's grant as anyone", "grant-anyone"},
	{"a service pod's automatic grant", "grant-pod"},
	{"a grant erin approved on bob's session", "grant-erin-approved"},
}

// grantIDs is the grant ids a GET /api/v1/credential-grants answer lists, in its order.
func grantIDs(t *testing.T, response *httptest.ResponseRecorder) []string {
	t.Helper()
	var listed wire.ApproverGrantsResponse
	if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode GET /api/v1/credential-grants %s: %v", response.Body.String(), err)
	}
	ids := []string{}
	for _, grant := range listed.Grants {
		ids = append(ids, grant.GrantID)
	}
	return ids
}

// newRestrictedGrantsServer is the access server with the fake broker rig as its secrets broker,
// erin on ACC's list and the broker answering grantsAnswered to every GET /v1/grants.
func newRestrictedGrantsServer(t *testing.T) (http.Handler, *fakeBrokerRig) {
	t.Helper()
	rig := newFakeBrokerRig(t)
	served, _ := newAccessServerWith(t, testServerOptions{admitsRestricted: true, agentSecretsURL: rig.server.URL})
	if listed := dispatchRequest(t, served, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names erin on ACC's list: status=%d body=%s", listed.Code, listed.Body.String())
	}
	rig.bodyByRoute["GET /v1/grants"] = grantsAnswered
	return served, rig
}

// TestARestrictedMembersGrantListHoldsTheirOwnSessionsGrantsAlone: Dispatch, not the broker,
// holds a restricted member's Live grants list to the grants of the sessions that act for them.
// The broker lists erin all six grants of grantsAnswered; her browser's GET
// /api/v1/credential-grants answers the two of her own session, the judged one among them, and
// none of the model judge's grant on bob's session, carol's grant as anyone, the service pod's
// automatic grant or the grant she approved on bob's session, and names none of their secrets.
//
// Control: answer a restricted member's list as a full member's (the handler before this change)
// and erin is answered all six, each named.
func TestARestrictedMembersGrantListHoldsTheirOwnSessionsGrantsAlone(t *testing.T) {
	served, rig := newRestrictedGrantsServer(t)

	listed := dispatchRequest(t, served, http.MethodGet, "/api/v1/credential-grants?approver=me", nil, accessErin)
	if listed.Code != http.StatusOK {
		t.Fatalf("erin's GET /api/v1/credential-grants = %d %s, want 200", listed.Code, listed.Body.String())
	}
	if got := grantIDs(t, listed); !slices.Equal(got, []string{"grant-erin", "grant-erin-judged"}) {
		t.Errorf("erin's GET /api/v1/credential-grants lists %v, want grant-erin and grant-erin-judged alone", got)
	}
	for _, other := range []string{"OTHER_SHARED", "CAROL_SHARED", "SERVICE_KEY", "BOB_KEY", "carol@example.com", `"bob"`} {
		if strings.Contains(listed.Body.String(), other) {
			t.Errorf("erin's GET /api/v1/credential-grants names %s, a fact of a grant not hers: %s", other, listed.Body.String())
		}
	}
	if !strings.Contains(rig.lastQuery, "approver=erin%40other.example") {
		t.Errorf("the list asked the broker with %q, want erin as the approver", rig.lastQuery)
	}
}

// TestARestrictedMembersRevokeOfAGrantNotTheirsReachesNoBroker: erin's revoke of each grant of
// grantsAnswered whose session does not act for her, whatever its approver, is 403
// RESTRICTED_MEMBER from Dispatch, and no revoke reaches the broker: the broker is asked her list
// and nothing else.
//
// Control: relay a restricted member's revoke as a full member's (the handler before this change),
// and each revoke reaches the broker, named.
func TestARestrictedMembersRevokeOfAGrantNotTheirsReachesNoBroker(t *testing.T) {
	served, rig := newRestrictedGrantsServer(t)

	for _, other := range grantsOfOthers {
		rig.routes = nil
		refused := dispatchRequest(t, served, http.MethodPost, "/api/v1/credential-grants/"+other.id+"/revoke", nil, accessErin)
		if refused.Code != http.StatusForbidden || !strings.Contains(refused.Body.String(), `"code":"RESTRICTED_MEMBER"`) {
			t.Errorf("erin's revoke of %s = %d %s, want 403 RESTRICTED_MEMBER", other.name, refused.Code, refused.Body.String())
		}
		if !slices.Equal(rig.routes, []string{"GET /v1/grants"}) {
			t.Errorf("erin's revoke of %s sent the broker %v, want her list read alone and no revoke", other.name, rig.routes)
		}
	}
}

// TestARestrictedMembersRevokeOfAnUnknownGrantAnswersAsAForeignOne: an id the broker lists no grant
// for is answered byte for byte as a grant of someone else's is, so the answer tells erin nothing
// of whether a grant has that id, and no revoke reaches the broker.
//
// Control: the handler before this change sends the unknown id's revoke to the broker, whose
// answer is relayed, and this row fails naming it.
func TestARestrictedMembersRevokeOfAnUnknownGrantAnswersAsAForeignOne(t *testing.T) {
	served, rig := newRestrictedGrantsServer(t)

	foreign := dispatchRequest(t, served, http.MethodPost, "/api/v1/credential-grants/grant-judged/revoke", nil, accessErin)
	rig.routes = nil
	unknown := dispatchRequest(t, served, http.MethodPost, "/api/v1/credential-grants/grant-unknown/revoke", nil, accessErin)
	if unknown.Code != http.StatusForbidden || unknown.Code != foreign.Code || unknown.Body.String() != foreign.Body.String() {
		t.Errorf("erin's revoke of an unknown id = %d %s, want the foreign grant's answer %d %s", unknown.Code, unknown.Body.String(), foreign.Code, foreign.Body.String())
	}
	if strings.Contains(unknown.Body.String(), "grant-") {
		t.Errorf("the refusal names a grant id: %s", unknown.Body.String())
	}
	if !slices.Equal(rig.routes, []string{"GET /v1/grants"}) {
		t.Errorf("erin's revoke of an unknown id sent the broker %v, want her list read alone and no revoke", rig.routes)
	}
}

// TestARestrictedMemberRevokesTheirOwnSessionsGrant: erin's revoke of a grant of her own session
// is relayed once Dispatch has read her list and found it there: the broker is asked her list and
// then sent the one revoke, naming her, and its answer is relayed.
//
// Control: the handler before this change relays the revoke without reading her list, and this row
// fails naming the calls the broker was sent.
func TestARestrictedMemberRevokesTheirOwnSessionsGrant(t *testing.T) {
	served, rig := newRestrictedGrantsServer(t)
	rig.bodyByRoute["POST /v1/grants/grant-erin/revoke-by-approver"] = `{"state":"revoked"}`

	rig.routes = nil
	revoked := dispatchRequest(t, served, http.MethodPost, "/api/v1/credential-grants/grant-erin/revoke", nil, accessErin)
	if revoked.Code != http.StatusOK || revoked.Body.String() != `{"state":"revoked"}` {
		t.Errorf("erin's revoke of her own session's grant = %d %s, want the broker's 200 relayed", revoked.Code, revoked.Body.String())
	}
	if want := []string{"GET /v1/grants", "POST /v1/grants/grant-erin/revoke-by-approver"}; !slices.Equal(rig.routes, want) {
		t.Errorf("erin's revoke of her own session's grant sent the broker %v, want %v", rig.routes, want)
	}
	if !strings.Contains(rig.lastBody, `"approver":"erin@other.example"`) {
		t.Errorf("the revoke named %s, want erin", rig.lastBody)
	}
}

// TestARestrictedMembersGrantRevokeFailsClosedWhileTheBrokersListCannotBeRead: while Dispatch
// cannot read a restricted member's grant list from the broker, their revoke, of their own
// session's grant too, revokes nothing: a list the broker refused is answered as the broker refused
// it, and a list Dispatch cannot read is 503 AGENT_SECRETS_UNAVAILABLE. Their list is answered the
// same and lists nothing the broker sent.
//
// Control: the handler before this change relays the revoke whatever the list, and this row fails
// naming the revoke the broker was sent.
func TestARestrictedMembersGrantRevokeFailsClosedWhileTheBrokersListCannotBeRead(t *testing.T) {
	for _, broken := range []struct {
		name               string
		status             int
		body               string
		wantStatus         int
		wantBody, wantCode string
	}{
		{"the broker refuses the list", http.StatusInternalServerError, `{"code":"INTERNAL","error":"list grants for approver"}`,
			http.StatusInternalServerError, `{"code":"INTERNAL","error":"list grants for approver"}`, "INTERNAL"},
		{"the list is not a grant list", http.StatusOK, `{"grants":"grant-erin"}`,
			http.StatusServiceUnavailable, "", "AGENT_SECRETS_UNAVAILABLE"},
	} {
		t.Run(broken.name, func(t *testing.T) {
			served, rig := newRestrictedGrantsServer(t)
			rig.statusByRoute["GET /v1/grants"] = broken.status
			rig.bodyByRoute["GET /v1/grants"] = broken.body

			rig.routes = nil
			revoke := dispatchRequest(t, served, http.MethodPost, "/api/v1/credential-grants/grant-erin/revoke", nil, accessErin)
			if revoke.Code != broken.wantStatus || !strings.Contains(revoke.Body.String(), `"code":"`+broken.wantCode+`"`) ||
				(broken.wantBody != "" && revoke.Body.String() != broken.wantBody) {
				t.Errorf("erin's revoke while %s = %d %s, want %d %s", broken.name, revoke.Code, revoke.Body.String(), broken.wantStatus, broken.wantCode)
			}
			if len(rig.revokes()) != 0 {
				t.Errorf("erin's revoke while %s sent the broker %v, want no revoke", broken.name, rig.revokes())
			}

			listed := dispatchRequest(t, served, http.MethodGet, "/api/v1/credential-grants?approver=me", nil, accessErin)
			if listed.Code != broken.wantStatus || strings.Contains(listed.Body.String(), `"grants":"grant-erin"`) {
				t.Errorf("erin's list while %s = %d %s, want %d and nothing the broker sent but its refusal", broken.name, listed.Code, listed.Body.String(), broken.wantStatus)
			}
		})
	}
}

// TestAFullMembersGrantListAndRevokeAreTheBrokers: Dispatch decides nothing of a full member's
// grants. alice's GET /api/v1/credential-grants is the broker's list byte for byte, asked once;
// her revoke of a grant as anyone reaches the broker with no list read first, and the broker's
// refusal of a grant not hers is relayed byte for byte. This row passes with and without the
// change: it is the control the restricted rows must leave standing.
func TestAFullMembersGrantListAndRevokeAreTheBrokers(t *testing.T) {
	served, rig := newRestrictedGrantsServer(t)
	refusal := `{"code":"NOT_APPROVER","error":"only the grant's approver or the person its session acts for may revoke it"}`
	rig.statusByRoute["POST /v1/grants/grant-erin-approved/revoke-by-approver"] = http.StatusForbidden
	rig.bodyByRoute["POST /v1/grants/grant-erin-approved/revoke-by-approver"] = refusal

	rig.routes = nil
	listed := dispatchRequest(t, served, http.MethodGet, "/api/v1/credential-grants?approver=me", nil, "alice")
	if listed.Code != http.StatusOK || listed.Body.String() != grantsAnswered {
		t.Errorf("alice's GET /api/v1/credential-grants = %d %s, want the broker's list verbatim", listed.Code, listed.Body.String())
	}
	if !slices.Equal(rig.routes, []string{"GET /v1/grants"}) || !strings.Contains(rig.lastQuery, "approver=alice") {
		t.Errorf("alice's list sent the broker %v with %q, want one list naming alice", rig.routes, rig.lastQuery)
	}

	rig.routes = nil
	revoked := dispatchRequest(t, served, http.MethodPost, "/api/v1/credential-grants/grant-anyone/revoke", nil, "alice")
	if revoked.Code != http.StatusOK || !slices.Equal(rig.routes, []string{"POST /v1/grants/grant-anyone/revoke-by-approver"}) ||
		!strings.Contains(rig.lastBody, `"approver":"alice"`) {
		t.Errorf("alice's revoke of a grant as anyone = %d %s, sent %v with body %s; want 200 and the one revoke naming her", revoked.Code, revoked.Body.String(), rig.routes, rig.lastBody)
	}

	rig.routes = nil
	refused := dispatchRequest(t, served, http.MethodPost, "/api/v1/credential-grants/grant-erin-approved/revoke", nil, "alice")
	if refused.Code != http.StatusForbidden || refused.Body.String() != refusal || len(rig.revokes()) != 1 {
		t.Errorf("alice's revoke of a grant not hers = %d %s with %v sent, want the broker's 403 relayed after one revoke", refused.Code, refused.Body.String(), rig.revokes())
	}
}
