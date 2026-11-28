// consented_token_test.go drives a Dispatch sign-in's token, a broker-issued token of the project
// scope consented, through the server's real broker-token verifier (internal/ciphertoken) against
// the stub broker, on a server whose person signs in with a session cookie, as a browser does.
package api

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// consentedTokenRig is a broker-token rig whose person, brokerTestPerson, signs in with a session
// cookie, and that cookie.
type consentedTokenRig struct {
	*brokerTokenRig
	cookie *http.Cookie
}

// newConsentedTokenRig is newBrokerTokenRigOn on a server whose identity is the session cookie
// (identity.CookieIdentity), with a live session for brokerTestPerson and the cookie that carries
// it.
func newConsentedTokenRig(t *testing.T) consentedTokenRig {
	t.Helper()
	database := storetest.Open(t)
	sessions := store.NewPgSessionStore(database.Pool)
	rig := newBrokerTokenRigOn(t, testServerOptions{
		store: database, identity: identity.CookieIdentity{SigningKey: testSigningKey, Sessions: sessions},
	})
	generation, err := sessions.EnsureSession(context.Background(), brokerTestPerson)
	if err != nil {
		t.Fatalf("establish the session of %s: %v", brokerTestPerson, err)
	}
	cookie, err := http.ParseSetCookie(auth.IssueSessionCookie(brokerTestPerson, generation, testSigningKey, true))
	if err != nil {
		t.Fatalf("parse the session cookie: %v", err)
	}
	return consentedTokenRig{brokerTokenRig: rig, cookie: cookie}
}

// asBrowser carries one request through the server as the person's browser: the session cookie
// and no other credential.
func (r consentedTokenRig) asBrowser(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	request := brokerRequest(t, method, path, body, "", "")
	request.AddCookie(r.cookie)
	return r.serve(request)
}

// storedAuthor is the author the comments table holds for the comment id names.
func storedAuthor(t *testing.T, database *store.Store, id string) model.Actor {
	t.Helper()
	var raw []byte
	if err := database.Pool.QueryRow(t.Context(), `select author from comments where id = $1`, id).Scan(&raw); err != nil {
		t.Fatalf("read the comment's author: %v", err)
	}
	var author model.Actor
	if err := json.Unmarshal(raw, &author); err != nil {
		t.Fatalf("decode the comment's author %s: %v", raw, err)
	}
	return author
}

// TestAConsentedTokenActsWithTheScopesItsPersonApproved: a Dispatch sign-in's token reaches
// Dispatch through the real verifier as the session it proves, acting for its person with the
// consented scopes alone. With [person], whoami names the person, the session and the scope
// person; a comment it writes is stored as that session, owned by the person and proven; and the
// inbox, which needs person, answers. With [p0 person], whoami names both, as the broker sorts
// them, and the comment and the inbox are answered as for [person]. With [], whoami names the
// empty set, the comment is written all the same, since the comment route needs no scope, and the
// inbox refuses it 403 OAUTH_SCOPE_REQUIRED. Each is refused 403 OAUTH_SCOPE_REQUIRED by the
// version review route, which needs approve. A consented answer whose scope is
// "person legion-status" holds a word no
// sign-in may hold and is no session's token: whoami, the comment, the inbox and the review are
// each 401 BROKER_TOKEN_NOT_A_SESSION, and the comment is not written. legion-status and not
// approve, since the verifier refuses any answer holding approve before it reads the consented
// shape, so a row holding approve reaches none of that shape's code. Controls: a person-scoped
// token reaches the inbox, and the person's browser cookie reviews the same version.
//
// The named reverts: give a consented caller brokerPersonScopes, and the empty consent reaches the
// inbox; drop the consentable check from shapeOf's consented shape, and the token claiming
// legion-status answers whoami.
func TestAConsentedTokenActsWithTheScopesItsPersonApproved(t *testing.T) {
	t.Parallel()
	rig := newConsentedTokenRig(t)
	caller := newBrokerCaller(t)
	if created := rig.asBrowser(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": "CON", "name": "Consented"}); created.Code != http.StatusCreated {
		t.Fatalf("create the project with the cookie: status=%d body=%s", created.Code, created.Body.String())
	}
	created := rig.asBrowser(t, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CON", "title": "Written under a Dispatch sign-in", "spec": "# Spec\n\nA passage.",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create the issue with the cookie: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, created)
	review := versionReviewPath(issue.PrimaryArtifactID, "1")
	reviewBody := map[string]any{"state": "approved"}

	for _, row := range []struct {
		name   string
		scope  string
		scopes []string
		inbox  int
	}{
		{name: "person", scope: model.ScopePerson, scopes: []string{model.ScopePerson}, inbox: http.StatusOK},
		{name: "person and p0", scope: model.ScopeP0 + " " + model.ScopePerson, scopes: []string{model.ScopeP0, model.ScopePerson}, inbox: http.StatusOK},
		{name: "none", scope: "", scopes: []string{}, inbox: http.StatusForbidden},
	} {
		token := "a-consented-token-of-" + row.name
		rig.broker.Says(token, consentedToken(caller.jkt, row.scope))

		who := rig.serve(caller.presenting(t, http.MethodGet, "/api/v1/whoami", token, nil))
		if who.Code != http.StatusOK {
			t.Fatalf("%s: whoami = %d %s, want 200", row.name, who.Code, who.Body.String())
		}
		answer := decodeBody[whoamiAnswer](t, who)
		if answer.Owner == nil || *answer.Owner != brokerTestPerson || answer.Session == nil || *answer.Session != brokerTestSession ||
			answer.Scopes == nil || !slices.Equal(answer.Scopes, row.scopes) {
			t.Fatalf("%s: whoami = %s, want owner %s, session %s and scopes %v", row.name, who.Body.String(), brokerTestPerson, brokerTestSession, row.scopes)
		}

		commented := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", token, map[string]any{
			"body": "Written under a consent of " + row.name + ".",
		}))
		if commented.Code != http.StatusCreated {
			t.Fatalf("%s: comment = %d %s, want 201", row.name, commented.Code, commented.Body.String())
		}
		author := storedAuthor(t, rig.database, decodeBody[struct {
			ID string `json:"id"`
		}](t, commented).ID)
		requireSessionProven(t, row.name+": the comment's author", author, true)
		if author.Owner == nil || *author.Owner != brokerTestPerson {
			t.Fatalf("%s: the comment's author owner = %v, want %s", row.name, author.Owner, brokerTestPerson)
		}

		inbox := rig.serve(caller.presenting(t, http.MethodGet, "/api/v1/inbox", token, nil))
		if inbox.Code != row.inbox {
			t.Fatalf("%s: inbox = %d %s, want %d", row.name, inbox.Code, inbox.Body.String(), row.inbox)
		}
		if row.inbox == http.StatusForbidden {
			if code, _ := refusalOf(t, inbox); code != apicode.OAuthScopeRequired.V1Name() {
				t.Fatalf("%s: inbox refusal = %s, want %s", row.name, code, apicode.OAuthScopeRequired.V1Name())
			}
		}

		reviewed := rig.serve(caller.presenting(t, http.MethodPost, review, token, reviewBody))
		if code, _ := refusalOf(t, reviewed); reviewed.Code != http.StatusForbidden || code != apicode.OAuthScopeRequired.V1Name() {
			t.Fatalf("%s: review = %d %s, want 403 %s", row.name, reviewed.Code, reviewed.Body.String(), apicode.OAuthScopeRequired.V1Name())
		}
	}

	const notConsentable = "a-consented-token-claiming-legion-status"
	rig.broker.Says(notConsentable, consentedToken(caller.jkt, model.ScopePerson+" "+ciphertoken.ScopeLegionStatus))
	for _, call := range []struct {
		name, method, path string
		body               any
	}{
		{"whoami", http.MethodGet, "/api/v1/whoami", nil},
		{"comment", http.MethodPost, "/api/v1/issues/" + issue.Key + "/comments", map[string]any{"body": "Written under a consent of approve."}},
		{"inbox", http.MethodGet, "/api/v1/inbox", nil},
		{"review", http.MethodPost, review, reviewBody},
	} {
		refused := rig.serve(caller.presenting(t, call.method, call.path, notConsentable, call.body))
		if code, _ := refusalOf(t, refused); refused.Code != http.StatusUnauthorized || code != "BROKER_TOKEN_NOT_A_SESSION" {
			t.Fatalf("legion-status: %s = %d %s, want 401 BROKER_TOKEN_NOT_A_SESSION", call.name, refused.Code, refused.Body.String())
		}
	}
	var comments int
	if err := rig.database.Pool.QueryRow(t.Context(), `select count(*) from comments`).Scan(&comments); err != nil {
		t.Fatalf("count the comments: %v", err)
	}
	if comments != 3 {
		t.Fatalf("the store holds %d comments, want the three the consented tokens wrote and none from the refused one", comments)
	}

	const personToken = "a-person-scoped-token"
	rig.broker.Says(personToken, personScopedTokenAnswer(caller.jkt))
	if inbox := rig.serve(caller.presenting(t, http.MethodGet, "/api/v1/inbox", personToken, nil)); inbox.Code != http.StatusOK {
		t.Fatalf("control: a person-scoped token's inbox = %d %s, want 200", inbox.Code, inbox.Body.String())
	}
	if reviewed := rig.asBrowser(t, http.MethodPost, review, reviewBody); reviewed.Code != http.StatusCreated {
		t.Fatalf("control: the cookie's review = %d %s, want 201", reviewed.Code, reviewed.Body.String())
	}
}

// TestAConsentedTokenSetsP0OnlyWhereItsPersonTickedIt: p0 is a scope a person may tick for a
// Dispatch sign-in (dispatch://INFRA-5943/ask/3dfbf22b-74af-4e6d-af17-3bf368aac9fa), and the
// token traded from it carries the scopes ticked (brokerTokenActor reads caller.Consented), which
// the p0 rule reads (refuseP0WithoutScope). So a consented token holding p0 and person makes an
// issue's own priority 0, on a create and on a PATCH of an issue whose own priority is none or 2,
// and one holding person alone, its person having left p0 unticked, is refused each write 403
// OAUTH_SCOPE_REQUIRED naming p0, under the challenge for p0, and writes nothing: no issue for a
// create, and no priority, field or event for a PATCH (p0IssueWrites, the writes
// TestEveryOperationThatTakesAnIssuePriorityNeedsP0ForZero sends).
//
// The named reverts: give a consented caller brokerPersonScopes in brokerTokenActor, and the
// token holding p0 is refused; read the asked scopes in place of the approved ones, and the token
// holding person alone writes.
func TestAConsentedTokenSetsP0OnlyWhereItsPersonTickedIt(t *testing.T) {
	t.Parallel()
	rig := newLegionStatusRig(t, false)
	for _, token := range []struct {
		name, scope string
		p0          p0Verdict
	}{
		{"a consented token holding person and p0", model.ScopeP0 + " " + model.ScopePerson, p0Written},
		{"a consented token holding person alone", model.ScopePerson, p0Refused},
	} {
		caller := newBrokerCaller(t)
		bearer := "consented." + caller.jkt
		rig.broker.Says(bearer, consentedToken(caller.jkt, token.scope))
		credential := p0Credential{
			legionCredential: legionCredential{name: token.name, send: func(t *testing.T, method, path string, body map[string]any) *httptest.ResponseRecorder {
				return rig.serve(caller.presenting(t, method, path, bearer, body))
			}},
			p0: token.p0,
		}
		t.Run(token.name, func(t *testing.T) {
			for _, operation := range slices.Sorted(maps.Keys(p0IssueWrites)) {
				write := p0IssueWrites[operation]
				for _, from := range write.from {
					t.Run(operation+" on an issue whose own priority is "+priorityName(from), func(t *testing.T) {
						write.send(t, rig, credential, from)
					})
				}
			}
		})
	}
}
