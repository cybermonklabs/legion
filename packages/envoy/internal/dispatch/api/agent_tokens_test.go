package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

type agentTokenResponse struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Scopes     []string   `json:"scopes"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
	Token      string     `json:"token"`
}

type agentTokenRow struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Scopes     []string   `json:"scopes"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
}

func TestAgentTokensArePrivateRevocableAndAttributeSessionWrites(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)

	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	issueResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Token attribution",
	}, "alice")
	if issueResponse.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", issueResponse.Code, issueResponse.Body.String())
	}
	// The deployment's shared token holds no scope, so every token route refuses it by naming
	// the one it needs; the person's own token is admitted below.
	for _, test := range []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{name: "list", method: http.MethodGet, path: "/api/v1/me/agent-tokens"},
		{name: "mint", method: http.MethodPost, path: "/api/v1/me/agent-tokens", body: map[string]string{"name": "agent"}},
		{name: "revoke", method: http.MethodDelete, path: "/api/v1/me/agent-tokens/00000000-0000-0000-0000-000000000000"},
	} {
		t.Run("the shared token cannot "+test.name, func(t *testing.T) {
			response := agentRequest(t, handler, test.method, test.path, test.body, "agent-token")
			if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"code":"`+codeScopeRequired+`"`) {
				t.Fatalf("%s agent token endpoint: status=%d body=%s, want 403 %s", test.name, response.Code, response.Body.String(), codeScopeRequired)
			}
		})
	}
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, issueResponse)

	mintedResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/me/agent-tokens", map[string]string{
		"name": "architect",
	}, "alice")
	if mintedResponse.Code != http.StatusCreated {
		t.Fatalf("mint agent token: status=%d body=%s", mintedResponse.Code, mintedResponse.Body.String())
	}
	minted := decodeBody[agentTokenResponse](t, mintedResponse)
	if !strings.HasPrefix(minted.Token, "dsp_") || len(minted.Token) != len("dsp_")+43 {
		t.Fatalf("minted token = %q, want dsp_ plus 43 base64url characters", minted.Token)
	}
	if minted.ID == "" || minted.Name != "architect" || minted.Prefix != minted.Token[len("dsp_"):len("dsp_")+8] || minted.CreatedAt.IsZero() || minted.LastUsedAt != nil || minted.RevokedAt != nil {
		t.Fatalf("minted token row = %#v", minted)
	}

	aliceTokens := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/agent-tokens", nil, "alice")
	if aliceTokens.Code != http.StatusOK {
		t.Fatalf("list alice agent tokens: status=%d body=%s", aliceTokens.Code, aliceTokens.Body.String())
	}
	listed := decodeBody[[]agentTokenRow](t, aliceTokens)
	if len(listed) != 1 || listed[0].ID != minted.ID || listed[0].Name != "architect" || listed[0].Prefix != minted.Prefix {
		t.Fatalf("alice agent tokens = %#v", listed)
	}

	bobTokens := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/agent-tokens", nil, "bob")
	if bobTokens.Code != http.StatusOK {
		t.Fatalf("list bob agent tokens: status=%d body=%s", bobTokens.Code, bobTokens.Body.String())
	}
	if got := decodeBody[[]agentTokenRow](t, bobTokens); len(got) != 0 {
		t.Fatalf("bob agent tokens = %#v, want none", got)
	}

	bobRevoke := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/me/agent-tokens/"+minted.ID, nil, "bob")
	if bobRevoke.Code != http.StatusNotFound {
		t.Fatalf("bob revokes alice token: status=%d body=%s", bobRevoke.Code, bobRevoke.Body.String())
	}

	askResponse := agentRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Which option?",
		"actor": map[string]any{
			"kind": "session", "id": "architect-session", "owner": "mallory",
		},
	}, minted.Token)
	if askResponse.Code != http.StatusCreated {
		t.Fatalf("create ask with personal token: status=%d body=%s", askResponse.Code, askResponse.Body.String())
	}
	ask := decodeBody[struct {
		Author struct {
			Kind  string  `json:"kind"`
			ID    string  `json:"id"`
			Owner *string `json:"owner"`
		} `json:"author"`
	}](t, askResponse)
	if ask.Author.Kind != "session" || ask.Author.ID != "architect-session" || ask.Author.Owner == nil || *ask.Author.Owner != "alice" {
		t.Fatalf("personal-token ask author = %#v, want session owned by alice", ask.Author)
	}

	usedTokens := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/agent-tokens", nil, "alice")
	used := decodeBody[[]agentTokenRow](t, usedTokens)
	if len(used) != 1 || used[0].LastUsedAt == nil {
		t.Fatalf("personal-token use did not record last_used_at: %#v", used)
	}
	firstUsedAt := *used[0].LastUsedAt
	if response := agentRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key, nil, minted.Token); response.Code != http.StatusOK {
		t.Fatalf("read with personal token: status=%d body=%s", response.Code, response.Body.String())
	}
	usedTokens = dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/agent-tokens", nil, "alice")
	used = decodeBody[[]agentTokenRow](t, usedTokens)
	if len(used) != 1 || used[0].LastUsedAt == nil || !used[0].LastUsedAt.Equal(firstUsedAt) {
		t.Fatalf("last_used_at updated more than once per minute: first=%s second=%v", firstUsedAt, used)
	}

	// A personal token holds both scopes by default, so Alice's own reads the list her browser
	// reads rather than being turned away with the shared token.
	own := agentRequest(t, handler, http.MethodGet, "/api/v1/me/agent-tokens", nil, minted.Token)
	if own.Code != http.StatusOK {
		t.Fatalf("list alice's tokens with her own token: status=%d body=%s", own.Code, own.Body.String())
	}
	if got := decodeBody[[]agentTokenRow](t, own); len(got) != 1 || got[0].ID != minted.ID {
		t.Fatalf("alice's own token listed %#v, want the row her browser lists", got)
	}

	sharedAsk := agentRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Does shared fallback work?",
		"actor": map[string]any{
			"kind": "session", "id": "shared-session", "owner": "mallory",
		},
	}, "agent-token")
	if sharedAsk.Code != http.StatusCreated {
		t.Fatalf("create ask with shared token: status=%d body=%s", sharedAsk.Code, sharedAsk.Body.String())
	}
	shared := decodeBody[struct {
		Author struct {
			Owner *string `json:"owner"`
		} `json:"author"`
	}](t, sharedAsk)
	if shared.Author.Owner != nil {
		t.Fatalf("shared-token ask owner = %q, want absent", *shared.Author.Owner)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		revoked := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/me/agent-tokens/"+minted.ID, nil, "alice")
		if revoked.Code != http.StatusNoContent {
			t.Fatalf("revoke token attempt %d: status=%d body=%s", attempt, revoked.Code, revoked.Body.String())
		}
	}
	revokedTokens := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/agent-tokens", nil, "alice")
	if got := decodeBody[[]agentTokenRow](t, revokedTokens); len(got) != 1 || got[0].RevokedAt == nil {
		t.Fatalf("revoked agent token list = %#v", got)
	}

	denied := agentRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "This must not be created.",
		"actor":    map[string]string{"kind": "session", "id": "architect-session"},
	}, minted.Token)
	if denied.Code != http.StatusUnauthorized || !strings.Contains(denied.Body.String(), `"code":"TOKEN_REVOKED"`) {
		t.Fatalf("revoked token write: status=%d body=%s", denied.Code, denied.Body.String())
	}
}

// INFRA-258. A token's plaintext is shown once, in the mint's answer, and agent_tokens holds only
// its hash, so an Idempotency-Key never keeps that answer: request_keys holds no copy of the token.
// The mint is still made once: a repeat under the key, its answer lost or not, is 409
// KEY_COMMITTED - the token was made and is not shown again - and no second token is minted.
func TestAKeyedTokenMintKeepsNoPlaintextAndMintsOnce(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	mint := func() *httptest.ResponseRecorder {
		t.Helper()
		return keyedRequest(t, handler, http.MethodPost, "/api/v1/me/agent-tokens", map[string]string{"name": "laptop"}, "alice", "mint-1")
	}
	first := mint()
	if first.Code != http.StatusCreated {
		t.Fatalf("mint: status=%d body=%s", first.Code, first.Body.String())
	}
	token := decodeBody[agentTokenResponse](t, first).Token
	if !strings.HasPrefix(token, "dsp_") {
		t.Fatalf("the mint answered no token: %s", first.Body.String())
	}
	var held int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from request_keys
		where position(convert_to($1, 'UTF8') in coalesce(response, ''::bytea)) > 0
		   or position($1 in coalesce(recovery::text, '')) > 0
	`, token).Scan(&held); err != nil {
		t.Fatalf("search the keys for the token: %v", err)
	}
	if held != 0 {
		t.Fatal("request_keys holds the minted token's plaintext")
	}

	for _, repeat := range []string{"at once", "after the lease"} {
		if repeat == "after the lease" {
			if _, err := database.Pool.Exec(context.Background(),
				`update request_keys set leased_at = now() - interval '2 minutes' where idempotency_key = 'mint-1'`); err != nil {
				t.Fatal(err)
			}
		}
		again := mint()
		if code := decodeBody[struct {
			Code string `json:"code"`
		}](t, again).Code; again.Code != http.StatusConflict || code != contracts.IdempotencyKeyCommitted ||
			strings.Contains(again.Body.String(), token) {
			t.Fatalf("a repeat %s: status=%d body=%s, want 409 %s without the token", repeat, again.Code, again.Body.String(), contracts.IdempotencyKeyCommitted)
		}
	}
	var tokens int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from agent_tokens where name = 'laptop'`).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if tokens != 1 {
		t.Fatalf("%d tokens minted, want one", tokens)
	}
}

// outsideTokenRig is two API servers over one database, as one deployment is before and after its
// operator stops admitting restricted members: on admits them (its sign-in rule, testOutsideRule,
// is Deps.RestrictedAdmission) and off does not. The database holds ACC, which alice owns and whose
// access list names erin, a restricted member, and erin's personal token, which she minted at her
// browser.
type outsideTokenRig struct {
	on, off  http.Handler
	database *store.Store
	token    string
}

func newOutsideTokenRig(t *testing.T) outsideTokenRig {
	t.Helper()
	on, database := newAccessServerWith(t, testServerOptions{admitsRestricted: true})
	off, _, _ := newTestServer(t, testServerOptions{store: database})
	if listed := dispatchRequest(t, on, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names erin on ACC's list: status=%d body=%s", listed.Code, listed.Body.String())
	}
	minted := dispatchRequest(t, on, http.MethodPost, "/api/v1/me/agent-tokens", map[string]string{"name": "erin's agent"}, accessErin)
	if minted.Code != http.StatusCreated {
		t.Fatalf("erin mints a token at her browser: status=%d body=%s", minted.Code, minted.Body.String())
	}
	return outsideTokenRig{on: on, off: off, database: database, token: decodeBody[agentTokenResponse](t, minted).Token}
}

// erinsTokenWhoami is GET /api/v1/whoami's answer to erin's token where it acts: a restricted
// member's token, minted at her browser naming nothing, holds no scope and lists ACC, the one
// project she sees.
const erinsTokenWhoami = `{"kind":"agent","owner":"erin@other.example","member_kind":"restricted","projects":["ACC"],"project":"ACC","scopes":[],"service":null,"session":null}`

// TestARestrictedOwnersTokenActsOnlyWhileTheDeploymentAdmitsThem (R5): a restricted member's
// personal token is held, at every use, to the rule that admits the member. It acts while the
// deployment admits restricted members and a project names the owner. Over the same database on a
// deployment that admits none it is 401 TOKEN_OWNER_NOT_ADMITTED saying so, on a read and on a
// write, which appends nothing. Once no project names the owner it is refused with the same code,
// saying so, and once one names them again it acts again: nothing revoked it.
func TestARestrictedOwnersTokenActsOnlyWhileTheDeploymentAdmitsThem(t *testing.T) {
	t.Parallel()
	rig := newOutsideTokenRig(t)
	whoami := func(handler http.Handler) *httptest.ResponseRecorder {
		t.Helper()
		return agentRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, rig.token)
	}
	wantActs := func(when string) {
		t.Helper()
		if response := whoami(rig.on); response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != erinsTokenWhoami {
			t.Fatalf("%s: erin's token on GET /api/v1/whoami = %d %s, want 200 %s", when, response.Code, response.Body.String(), erinsTokenWhoami)
		}
	}
	wantRefused := func(response *httptest.ResponseRecorder, call, saying string) {
		t.Helper()
		wantRefusal(t, response, http.StatusUnauthorized, codeTokenOwnerNotAdmitted, "erin's token", call)
		if !strings.Contains(response.Body.String(), saying) {
			t.Fatalf("erin's token: %s = %s, want the refusal to say %q", call, response.Body.String(), saying)
		}
	}
	wantActs("restricted members admitted and ACC's list naming erin")

	wantRefused(whoami(rig.off), "GET /api/v1/whoami with restricted members not admitted", "admits nobody from outside it")
	before := allEvents(t, rig.database)
	wantRefused(agentRequest(t, rig.off, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "ACC", "title": "Written once restricted members were no longer admitted",
		"actor": map[string]string{"kind": "session", "id": "erin-agent"},
	}, rig.token), "POST /api/v1/issues with restricted members not admitted", "admits nobody from outside it")
	if after := allEvents(t, rig.database); after != before {
		t.Fatalf("erin's refused write moved events from %d to %d", before, after)
	}

	if unlisted := dispatchRequest(t, rig.on, http.MethodDelete, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); unlisted.Code != http.StatusOK {
		t.Fatalf("alice takes erin off ACC's list: status=%d body=%s", unlisted.Code, unlisted.Body.String())
	}
	wantRefused(whoami(rig.on), "GET /api/v1/whoami once no project names erin", "no project names them any more")

	if listed := dispatchRequest(t, rig.on, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names erin on ACC's list again: status=%d body=%s", listed.Code, listed.Body.String())
	}
	wantActs("ACC's list naming erin again")
}

// TestARestrictedOwnersTokenIsRefusedWhenTheListsCannotBeRead: whether a project still names a
// restricted owner is read at each use of the token, and a read that fails is 503
// IDENTITY_UNAVAILABLE, never an accept. The lists are made unreadable by renaming the table of
// access lists, which leaves the token's row and the owner's people row readable.
func TestARestrictedOwnersTokenIsRefusedWhenTheListsCannotBeRead(t *testing.T) {
	t.Parallel()
	rig := newOutsideTokenRig(t)
	rename := func(from, to string) {
		t.Helper()
		if _, err := rig.database.Pool.Exec(context.Background(), `alter table `+from+` rename to `+to); err != nil {
			t.Fatalf("rename %s to %s: %v", from, to, err)
		}
	}
	rename("project_access", "project_access_unreadable")
	response := agentRequest(t, rig.on, http.MethodGet, "/api/v1/whoami", nil, rig.token)
	rename("project_access_unreadable", "project_access")
	wantRefusal(t, response, http.StatusServiceUnavailable, "IDENTITY_UNAVAILABLE", "erin's token", "GET /api/v1/whoami while the lists cannot be read")
	if again := agentRequest(t, rig.on, http.MethodGet, "/api/v1/whoami", nil, rig.token); again.Code != http.StatusOK {
		t.Fatalf("erin's token once the lists read again: status=%d body=%s, want 200", again.Code, again.Body.String())
	}
}

// TestARestrictedOwnersTokenIsHeldToTheRuleWhateverTheCaseOfItsOwner: a token's owner is compared
// with the people row case folded, as the store compares an email, so a token whose row names erin
// in other letters than her people row is still a restricted member's token: refused where
// restricted members are not admitted, acting where they are and ACC lists her, and refused again
// once no project names her.
func TestARestrictedOwnersTokenIsHeldToTheRuleWhateverTheCaseOfItsOwner(t *testing.T) {
	t.Parallel()
	rig := newOutsideTokenRig(t)
	const spelled = "Erin@Other.Example"
	if spelled == accessErin || model.CanonicalLogin(spelled) != accessErin {
		t.Fatalf("%q must differ from erin's row %q only in case", spelled, accessErin)
	}
	token := personalTokenListing(t, rig.database, spelled, []string{"ACC"}, model.ScopePerson)
	whoami := func(handler http.Handler) *httptest.ResponseRecorder {
		t.Helper()
		return agentRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, token)
	}
	wantRefused := func(response *httptest.ResponseRecorder, call, saying string) {
		t.Helper()
		wantRefusal(t, response, http.StatusUnauthorized, codeTokenOwnerNotAdmitted, "the token owned by "+spelled, call)
		if !strings.Contains(response.Body.String(), saying) {
			t.Fatalf("the token owned by %s: %s = %s, want the refusal to say %q", spelled, call, response.Body.String(), saying)
		}
	}
	wantRefused(whoami(rig.off), "GET /api/v1/whoami with restricted members not admitted", "admits nobody from outside it")
	if response := whoami(rig.on); response.Code != http.StatusOK {
		t.Fatalf("the token owned by %s: GET /api/v1/whoami with restricted members admitted and ACC's list naming erin = %d %s, want 200",
			spelled, response.Code, response.Body.String())
	}
	if unlisted := dispatchRequest(t, rig.on, http.MethodDelete, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); unlisted.Code != http.StatusOK {
		t.Fatalf("alice takes erin off ACC's list: status=%d body=%s", unlisted.Code, unlisted.Body.String())
	}
	wantRefused(whoami(rig.on), "GET /api/v1/whoami once no project names erin", "no project names them any more")
}

// TestARestrictedOwnersTokenIsHeldToTheRuleWhateverSpacesSurroundItsOwner: a token whose row names
// erin with spaces around her address is her token, held to the rule as her address reads in her
// people row. With other.example among the Workspace's domains the rule refuses her
// (identity.ErrWorkspaceAddress), so it refuses the token too, saying so, while ACC's list still
// names her. The same token acts under the rule that lists no such domain, which shows the token
// reaches the restricted owner's check rather than being refused for another reason.
func TestARestrictedOwnersTokenIsHeldToTheRuleWhateverSpacesSurroundItsOwner(t *testing.T) {
	t.Parallel()
	rig := newOutsideTokenRig(t)
	const spelled = "  erin@other.example  "
	if spelled == accessErin || model.CanonicalLogin(spelled) != accessErin {
		t.Fatalf("%q must differ from erin's row %q only in the spaces around it", spelled, accessErin)
	}
	token := personalTokenListing(t, rig.database, spelled, []string{"ACC"}, model.ScopePerson)
	actor, err := directServer(Deps{Store: rig.database, RestrictedAdmission: testOutsideRule(rig.database.Pool)}).personalTokenActor(context.Background(), token)
	if err != nil || actor.Owner == nil {
		t.Fatalf("the token owned by %q with restricted members admitted and ACC's list naming erin: %+v, %v; want it to act", spelled, actor, err)
	}
	ofTheWorkspace := directServer(Deps{Store: rig.database, RestrictedAdmission: testOutsideRule(rig.database.Pool, "other.example")})
	_, err = ofTheWorkspace.personalTokenActor(context.Background(), token)
	wantAPIError(t, err, http.StatusUnauthorized, codeTokenOwnerNotAdmitted, "the token owned by erin with spaces around her address, with other.example a Workspace domain")
	const onAWorkspaceDomain = "on a domain the Google Workspace owns"
	if !strings.Contains(err.Error(), onAWorkspaceDomain) {
		t.Fatalf("the token owned by %q with other.example a Workspace domain: %v, want the refusal to say %q", spelled, err, onAWorkspaceDomain)
	}
}

// TestAFullMembersTokenAnswersAsBeforeWhereNoRestrictedMemberIsAdmitted (R9): on a deployment that
// admits no restricted member, the token of a full member and that of an owner no people row holds
// answer GET /api/v1/whoami as they always did, and the same two statements resolve each, the
// token's lookup and the record of its use, neither reading an access list. The instrument is shown
// seeing a list read in the same run: a restricted owner's token, which must read one, resolves in
// three.
func TestAFullMembersTokenAnswersAsBeforeWhereNoRestrictedMemberIsAdmitted(t *testing.T) {
	t.Parallel()
	rig := newOutsideTokenRig(t)
	minted := dispatchRequest(t, rig.off, http.MethodPost, "/api/v1/me/agent-tokens", map[string]string{"name": "alice's agent"}, "alice")
	if minted.Code != http.StatusCreated {
		t.Fatalf("alice mints a token: status=%d body=%s", minted.Code, minted.Body.String())
	}
	const nobody = "nobody@example.com"
	if rows := countRows(t, rig.database, `select count(*) from people where email = $1`, nobody); rows != 0 {
		t.Fatalf("people holds %d rows for %s, want none", rows, nobody)
	}
	recorder := &storetest.StatementRecorder{}
	traced := &store.Store{Pool: storetest.Traced(t, rig.database.Pool, recorder, nil)}
	for _, test := range []struct {
		name, token, whoami string
		admitsRestricted    bool
		statements          int
		readsLists          bool
	}{
		{"alice, a full member", decodeBody[agentTokenResponse](t, minted).Token,
			`{"kind":"agent","owner":"alice","project":null,"scopes":["person","approve"],"service":null,"session":null}`, false, 2, false},
		{"an owner no people row holds", personalTokenOwnedBy(t, rig.database, nobody, model.ScopePerson),
			`{"kind":"agent","owner":"nobody@example.com","project":null,"scopes":["person"],"service":null,"session":null}`, false, 2, false},
		{"erin, a restricted member, with restricted members admitted", rig.token, erinsTokenWhoami, true, 3, true},
	} {
		handler := rig.off
		if test.admitsRestricted {
			handler = rig.on
		}
		response := agentRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, test.token)
		if got := strings.TrimSpace(response.Body.String()); response.Code != http.StatusOK || got != test.whoami {
			t.Errorf("%s: GET /api/v1/whoami = %d %s, want 200 %s", test.name, response.Code, got, test.whoami)
		}
		before := len(recorder.Statements())
		actor, err := directServer(Deps{Store: traced, RestrictedAdmission: restrictedAdmissionOf(test.admitsRestricted, traced.Pool)}).personalTokenActor(context.Background(), test.token)
		statements := recorder.Statements()[before:]
		readsLists := strings.Contains(strings.Join(statements, "\n"), access.Marker)
		if err != nil || actor.Owner == nil || len(statements) != test.statements || readsLists != test.readsLists {
			t.Errorf("%s: the token resolved to %+v, %v in %d statements, reading the lists %t; want its owner in %d, reading the lists %t:\n%s",
				test.name, actor, err, len(statements), readsLists, test.statements, test.readsLists, strings.Join(statements, "\n---\n"))
		}
	}
}

// tokenKeptIn answers which table keeps token, found by its hash as personalTokenActor finds it:
// agent_tokens, agent_read_tokens, or "" for neither. Both keeping it fails the test.
func tokenKeptIn(t *testing.T, database *store.Store, token string) string {
	t.Helper()
	hash := sha256.Sum256([]byte(token))
	var writing, reading int
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from agent_tokens where token_hash = $1),
		       (select count(*) from agent_read_tokens where token_hash = $1)
	`, hash[:]).Scan(&writing, &reading); err != nil {
		t.Fatalf("look the token up: %v", err)
	}
	switch {
	case writing > 0 && reading > 0:
		t.Fatalf("both agent_tokens and agent_read_tokens keep the token")
	case writing > 0:
		return "agent_tokens"
	case reading > 0:
		return "agent_read_tokens"
	}
	return ""
}

// tokenWhoami is what GET /api/v1/whoami answers a personal token: its kind, the person it
// belongs to, the scopes it holds as whoami names them, and its cut, null for none.
type tokenWhoami struct {
	Kind   string          `json:"kind"`
	Owner  *string         `json:"owner"`
	Scopes []string        `json:"scopes"`
	Cut    json.RawMessage `json:"cut"`
}

// listedScopes is the caller's token list as name and scopes, in the order the list answers it.
func listedScopes(t *testing.T, handler http.Handler, login string) [][]string {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/agent-tokens", nil, login)
	if response.Code != http.StatusOK {
		t.Fatalf("list %s's tokens: status=%d body=%s", login, response.Code, response.Body.String())
	}
	var listed [][]string
	for _, row := range decodeBody[[]agentTokenRow](t, response) {
		listed = append(listed, append([]string{row.Name}, row.Scopes...))
	}
	return listed
}

// TestAPersonalTokenMayHoldReadBesidePersonOrAlone: a person mints a read-only personal token
// holding read alone, or read beside person. It keeps the dsp_ form and length of every personal
// token, and it is kept in agent_read_tokens, which a build before read-only tokens never reads.
// whoami names read among its scopes, and person where it holds it, with no cut. The list holds
// it with its scopes, newest first beside the token that writes. A write refuses it 403
// READ_ONLY_TOKEN and writes nothing, and a read answers it by the read's own scopes: the inbox,
// which needs person, refuses read alone 403 OAUTH_SCOPE_REQUIRED and serves read beside person.
// read beside approve is 400 TOKEN_SCOPES_INPUT and mints nothing, since settling what a person
// settles is a write.
func TestAPersonalTokenMayHoldReadBesidePersonOrAlone(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "READ", "name": "Reading",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "READ", "title": "An issue a read-only token reads",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, created).Key
	writer := mintPersonalToken(t, handler, "alice", "writer", []string{model.ScopePerson})
	if table := tokenKeptIn(t, database, writer.Token); table != "agent_tokens" {
		t.Fatalf("the token that writes is kept in %q, want agent_tokens", table)
	}

	for _, scopes := range [][]string{
		{model.ScopeRead, model.ScopeApprove},
		{model.ScopeRead, model.ScopePerson, model.ScopeApprove},
	} {
		refused := dispatchRequest(t, handler, http.MethodPost, "/api/v1/me/agent-tokens",
			map[string]any{"name": "refused", "scopes": scopes}, "alice")
		if refusal := decodeBody[refusalAnswer](t, refused); refused.Code != http.StatusBadRequest || refusal.Code != codeTokenScopesInput {
			t.Fatalf("mint a token holding %v: status=%d body=%s, want 400 %s", scopes, refused.Code, refused.Body.String(), codeTokenScopesInput)
		}
	}

	const comment = "Written under a token that may only read."
	for _, reader := range []struct {
		name   string
		scopes []string
		// whoami is the scopes whoami names, and inbox what GET /api/v1/inbox answers.
		whoami []string
		inbox  int
	}{
		{"reader", []string{model.ScopeRead}, []string{model.ScopeRead}, http.StatusForbidden},
		{"person-reader", []string{model.ScopeRead, model.ScopePerson}, []string{model.ScopePerson, model.ScopeRead}, http.StatusOK},
	} {
		minted := mintPersonalToken(t, handler, "alice", reader.name, reader.scopes)
		if !strings.HasPrefix(minted.Token, "dsp_") || len(minted.Token) != len("dsp_")+43 ||
			minted.Prefix != minted.Token[len("dsp_"):len("dsp_")+agentTokenPrefixLength] {
			t.Fatalf("%s: minted %q with prefix %q, want dsp_ plus 43 base64url characters, as every personal token", reader.name, minted.Token, minted.Prefix)
		}
		if !slices.Equal(minted.Scopes, reader.scopes) {
			t.Fatalf("%s: the mint answered scopes %v, want %v", reader.name, minted.Scopes, reader.scopes)
		}
		if table := tokenKeptIn(t, database, minted.Token); table != "agent_read_tokens" {
			t.Fatalf("%s: the token is kept in %q, want agent_read_tokens", reader.name, table)
		}

		whoami := agentRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, minted.Token)
		if whoami.Code != http.StatusOK {
			t.Fatalf("%s: whoami: status=%d body=%s", reader.name, whoami.Code, whoami.Body.String())
		}
		who := decodeBody[tokenWhoami](t, whoami)
		if who.Kind != "agent" || who.Owner == nil || *who.Owner != "alice" || !slices.Equal(who.Scopes, reader.whoami) || string(who.Cut) != "null" {
			t.Fatalf("%s: whoami = %s, want an agent of alice's holding %v and no cut", reader.name, whoami.Body.String(), reader.whoami)
		}

		written := agentRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/comments", map[string]any{
			"body": comment, "actor": map[string]string{"kind": "session", "id": "session-of-" + reader.name},
		}, minted.Token)
		if refusal := decodeBody[refusalAnswer](t, written); written.Code != http.StatusForbidden || refusal.Code != contracts.DispatchReadOnlyToken {
			t.Fatalf("%s: a comment: status=%d body=%s, want 403 %s", reader.name, written.Code, written.Body.String(), contracts.DispatchReadOnlyToken)
		}

		inbox := agentRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, minted.Token)
		if inbox.Code != reader.inbox {
			t.Fatalf("%s: the inbox: status=%d body=%s, want %d", reader.name, inbox.Code, inbox.Body.String(), reader.inbox)
		}
		if reader.inbox == http.StatusForbidden && gateRefusalOf(inbox).Code != codeScopeRequired {
			t.Fatalf("%s: the inbox refused %s, want %s: the token holds no person", reader.name, inbox.Body.String(), codeScopeRequired)
		}
	}

	var comments, refused int
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from comments where body = $1),
		       (select count(*) from agent_tokens where name = 'refused') + (select count(*) from agent_read_tokens where name = 'refused')
	`, comment).Scan(&comments, &refused); err != nil {
		t.Fatalf("count the comments and the refused tokens: %v", err)
	}
	if comments != 0 || refused != 0 {
		t.Fatalf("the store holds %d comments a read-only token sent and %d tokens a refused mint made, want none", comments, refused)
	}
	want := [][]string{{"person-reader", model.ScopeRead, model.ScopePerson}, {"reader", model.ScopeRead}, {"writer", model.ScopePerson}}
	if listed := listedScopes(t, handler, "alice"); !slices.EqualFunc(listed, want, slices.Equal) {
		t.Fatalf("alice's tokens are listed %v, want %v: both tables, newest first, each with its scopes", listed, want)
	}
}

// TestAnExistingTokenCannotBeChangedToRead: no token changes to or from read only. A PATCH giving
// a token that writes read, alone or beside person, and any PATCH of a read-only token, whatever
// set it names, is 400 TOKEN_SCOPES_INPUT naming that a person makes a new token, and each token
// keeps the scopes it held. Another person's read-only token is 404 NOT_FOUND, as any token not
// the caller's is. The control: the token that writes still changes between person and approve.
func TestAnExistingTokenCannotBeChangedToRead(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	writer := mintPersonalToken(t, handler, "alice", "writer", []string{model.ScopePerson})
	reader := mintPersonalToken(t, handler, "alice", "reader", []string{model.ScopeRead, model.ScopePerson})
	patch := func(login, id string, scopes []string) *httptest.ResponseRecorder {
		t.Helper()
		return dispatchRequest(t, handler, http.MethodPatch, "/api/v1/me/agent-tokens/"+id, map[string]any{"scopes": scopes}, login)
	}
	requireRefused := func(what string, response *httptest.ResponseRecorder) {
		t.Helper()
		refusal := decodeBody[refusalAnswer](t, response)
		if response.Code != http.StatusBadRequest || refusal.Code != codeTokenScopesInput || !strings.Contains(refusal.Error, "A person makes a new token") {
			t.Fatalf("%s: status=%d body=%s, want 400 %s naming that a person makes a new token", what, response.Code, response.Body.String(), codeTokenScopesInput)
		}
	}
	for _, scopes := range [][]string{{model.ScopeRead}, {model.ScopeRead, model.ScopePerson}} {
		requireRefused(fmt.Sprintf("the token that writes changed to %v", scopes), patch("alice", writer.ID, scopes))
	}
	for _, scopes := range [][]string{
		{model.ScopeRead}, {model.ScopeRead, model.ScopePerson}, {model.ScopePerson}, {model.ScopePerson, model.ScopeApprove}, {},
	} {
		requireRefused(fmt.Sprintf("the read-only token changed to %v", scopes), patch("alice", reader.ID, scopes))
	}
	if response := patch("bob", reader.ID, []string{model.ScopePerson}); response.Code != http.StatusNotFound || decodeBody[refusalAnswer](t, response).Code != "NOT_FOUND" {
		t.Fatalf("bob changes alice's read-only token: status=%d body=%s, want 404 NOT_FOUND", response.Code, response.Body.String())
	}
	want := [][]string{{"reader", model.ScopeRead, model.ScopePerson}, {"writer", model.ScopePerson}}
	if listed := listedScopes(t, handler, "alice"); !slices.EqualFunc(listed, want, slices.Equal) {
		t.Fatalf("after the refused changes alice's tokens are %v, want %v as they were minted", listed, want)
	}
	if table := tokenKeptIn(t, database, writer.Token); table != "agent_tokens" {
		t.Fatalf("the token that writes is kept in %q after the refused changes, want agent_tokens", table)
	}

	changed := patch("alice", writer.ID, []string{model.ScopePerson, model.ScopeApprove})
	if changed.Code != http.StatusOK {
		t.Fatalf("the token that writes changed to person and approve: status=%d body=%s, want 200", changed.Code, changed.Body.String())
	}
	want = [][]string{{"reader", model.ScopeRead, model.ScopePerson}, {"writer", model.ScopePerson, model.ScopeApprove}}
	if listed := listedScopes(t, handler, "alice"); !slices.EqualFunc(listed, want, slices.Equal) {
		t.Fatalf("after the change alice's tokens are %v, want %v", listed, want)
	}
}

// TestRevokingAReadOnlyPersonalTokenEndsIt: a read-only token, holding read alone or beside
// person, is revoked by the DELETE that revokes any personal token, and a second DELETE answers
// 204 as the first did. From then on whoami and a read answer it 401 TOKEN_REVOKED, and the list
// shows it revoked. Another person's DELETE of it is 404 and leaves it working.
func TestRevokingAReadOnlyPersonalTokenEndsIt(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "REVOKE", "name": "Revoking",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "REVOKE", "title": "An issue a revoked token reads",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, created).Key

	for _, scopes := range [][]string{{model.ScopeRead}, {model.ScopeRead, model.ScopePerson}} {
		name := strings.Join(scopes, "-")
		minted := mintPersonalToken(t, handler, "alice", name, scopes)
		reads := func(what string, want int) {
			t.Helper()
			for _, path := range []string{"/api/v1/whoami", "/api/v1/issues/" + issue} {
				response := agentRequest(t, handler, http.MethodGet, path, nil, minted.Token)
				if response.Code != want {
					t.Fatalf("%s: GET %s %s: status=%d body=%s, want %d", name, path, what, response.Code, response.Body.String(), want)
				}
				if want == http.StatusUnauthorized && gateRefusalOf(response).Code != "TOKEN_REVOKED" {
					t.Fatalf("%s: GET %s %s answered %s, want TOKEN_REVOKED", name, path, what, response.Body.String())
				}
			}
		}
		reads("before the revoke", http.StatusOK)
		if response := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/me/agent-tokens/"+minted.ID, nil, "bob"); response.Code != http.StatusNotFound {
			t.Fatalf("%s: bob revokes alice's token: status=%d body=%s, want 404", name, response.Code, response.Body.String())
		}
		reads("after bob's refused revoke", http.StatusOK)
		for attempt := 1; attempt <= 2; attempt++ {
			if response := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/me/agent-tokens/"+minted.ID, nil, "alice"); response.Code != http.StatusNoContent {
				t.Fatalf("%s: revoke attempt %d: status=%d body=%s, want 204", name, attempt, response.Code, response.Body.String())
			}
		}
		reads("after the revoke", http.StatusUnauthorized)
		listed := decodeBody[[]agentTokenRow](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/agent-tokens", nil, "alice"))
		index := slices.IndexFunc(listed, func(row agentTokenRow) bool { return row.ID == minted.ID })
		if index < 0 || listed[index].RevokedAt == nil {
			t.Fatalf("%s: alice's tokens are listed %+v, want this one revoked", name, listed)
		}
	}
}

// TestAReadOnlyPersonalTokenRecordsItsLastUse: a read-only token's use is written to its own row
// at most once a minute, as a token that writes has its use written. One resolution writes
// last_used_at; another within the minute writes nothing; once the minute has passed the next
// one writes it again. Neither writes the row of the person's token that writes.
func TestAReadOnlyPersonalTokenRecordsItsLastUse(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	writer := mintPersonalToken(t, handler, "alice", "writer", []string{model.ScopePerson})
	reader := mintPersonalToken(t, handler, "alice", "reader", []string{model.ScopeRead, model.ScopePerson})
	lastUsed := func() (reading, writing *time.Time) {
		t.Helper()
		if err := database.Pool.QueryRow(context.Background(), `
			select (select last_used_at from agent_read_tokens where id = $1),
			       (select last_used_at from agent_tokens where id = $2)
		`, reader.ID, writer.ID).Scan(&reading, &writing); err != nil {
			t.Fatalf("read the tokens' last use: %v", err)
		}
		return reading, writing
	}
	resolve := func() {
		t.Helper()
		if response := agentRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, reader.Token); response.Code != http.StatusOK {
			t.Fatalf("whoami under the read-only token: status=%d body=%s", response.Code, response.Body.String())
		}
	}
	if reading, _ := lastUsed(); reading != nil {
		t.Fatalf("the read-only token was used at %s before any request", reading)
	}

	resolve()
	first, writing := lastUsed()
	if first == nil {
		t.Fatal("one resolution of the read-only token wrote no last_used_at")
	}
	resolve()
	second, _ := lastUsed()
	if second == nil || !second.Equal(*first) {
		t.Fatalf("a second resolution within the minute wrote last_used_at %v over %s, want it left", second, first)
	}

	var aged time.Time
	if err := database.Pool.QueryRow(context.Background(), `
		update agent_read_tokens set last_used_at = now() - interval '2 minutes' where id = $1 returning last_used_at
	`, reader.ID).Scan(&aged); err != nil {
		t.Fatalf("age the token's last use: %v", err)
	}
	resolve()
	third, writingAfter := lastUsed()
	if third == nil || !third.After(aged) {
		t.Fatalf("a resolution past the minute left last_used_at %v at %s, want it written again", third, aged)
	}
	if writing != nil || writingAfter != nil {
		t.Fatalf("resolving the read-only token wrote the last use of the token that writes: %v, then %v", writing, writingAfter)
	}
}
