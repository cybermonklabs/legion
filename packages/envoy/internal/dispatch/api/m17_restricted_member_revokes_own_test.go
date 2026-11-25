package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/wire"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// The two revoke rows a person's own Settings offers: a grant whose session acts for them, and a
// sign-in of theirs.
const (
	ownGrantRevoke  = "POST /api/v1/credential-grants/{id}/revoke"
	ownSignInRevoke = "POST /api/v1/signins/{id}/revoke"
)

// ownRevokeBroker is a secrets broker that decides whose a grant or a sign-in is as the real one
// does for a full member: a grant's revoke is its approver's or its enrollment's operator's (403
// NOT_APPROVER for anyone else), and a sign-in's is its person's (anyone else's is an unknown id,
// 404). Its grant list (GET /v1/grants) is a full member's, the grants a login approved or whose
// session acts for it, which Dispatch holds a restricted member's to their own sessions'
// (memberGrants). It counts the revokes Dispatch sends it.
type ownRevokeBroker struct {
	server  *httptest.Server
	mu      sync.Mutex
	grants  map[string][2]string // id: approver, operator
	signIns map[string]string    // id: person
	calls   int
}

func newOwnRevokeBroker(t *testing.T) *ownRevokeBroker {
	t.Helper()
	broker := &ownRevokeBroker{
		grants: map[string][2]string{
			"grant-erin":  {"", accessErin},
			"grant-alice": {"alice", ""},
		},
		signIns: map[string]string{"signin-erin": accessErin, "signin-alice": "alice"},
	}
	broker.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		broker.mu.Lock()
		defer broker.mu.Unlock()
		if r.Method == http.MethodPost {
			broker.calls++
		}
		w.Header().Set("Content-Type", "application/json")
		var body struct{ Approver, Person string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path == "/v1/grants" && r.Method == http.MethodGet {
			login := r.URL.Query().Get("approver")
			listed := wire.ApproverGrantsResponse{Grants: []wire.ApproverGrantResp{}}
			for id, held := range broker.grants {
				if held[0] == login || held[1] == login {
					approver := held[0]
					listed.Grants = append(listed.Grants, wire.ApproverGrantResp{
						GrantID: id, Granted: "approval", Approver: &approver,
						Enrollment: wire.RecordEnrollmentResp{Kind: "session", RuntimeID: id, Operator: held[1]},
						Names:      []string{"SECRET"},
					})
				}
			}
			_ = json.NewEncoder(w).Encode(listed)
			return
		}
		if id, ok := strings.CutPrefix(r.URL.Path, "/v1/grants/"); ok && r.Method == http.MethodPost {
			id = strings.TrimSuffix(id, "/revoke-by-approver")
			held, exists := broker.grants[id]
			switch {
			case !exists:
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"code":"NOT_FOUND","error":"no grant has that id"}`))
			case body.Approver != held[0] && body.Approver != held[1]:
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"code":"NOT_APPROVER","error":"only the grant's approver or its enrollment's operator may revoke it"}`))
			default:
				delete(broker.grants, id)
				_, _ = w.Write([]byte(`{"state":"revoked"}`))
			}
			return
		}
		if id, ok := strings.CutPrefix(r.URL.Path, "/v1/person-signins/"); ok && r.Method == http.MethodPost {
			id = strings.TrimSuffix(id, "/revoke")
			if broker.signIns[id] == "" || broker.signIns[id] != body.Person {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"code":"NOT_FOUND","error":"no sign-in of that person has that id"}`))
				return
			}
			delete(broker.signIns, id)
			_, _ = w.Write([]byte(`{"state":"revoked","tokens_ended":1}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"NOT_FOUND","error":"no such route"}`))
	}))
	t.Cleanup(broker.server.Close)
	return broker
}

// holds reports whether the broker still holds the grant or the sign-in id names.
func (b *ownRevokeBroker) holds(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, grant := b.grants[id]
	_, signIn := b.signIns[id]
	return grant || signIn
}

// revokes is how many revokes Dispatch has sent the broker.
func (b *ownRevokeBroker) revokes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// newOwnRevokeServer mounts the real routes with the secrets broker at brokerURL, alice and carol
// full members and erin a restricted member, and no private project. The deployment signs in
// people from outside its domain and ACC, alice's, names erin on its list, so erin's personal token
// acts (a restricted owner's token acts only while the rule that signs her in admits her). Each of
// alice and erin holds a personal token that lists ACC, answered by caller name.
func newOwnRevokeServer(t *testing.T, brokerURL string) (http.Handler, map[string]string) {
	t.Helper()
	database := storetest.Open(t)
	documentService := docs.New(docs.Deps{
		Store: database, Events: events.NewBroker(), Origins: text.NewOrigins(testServerURL), Settle: 20 * time.Millisecond,
	})
	t.Cleanup(func() { _ = documentService.Shutdown(t.Context()) })
	seedPeople(t, database, "alice", accessCarol, accessErin)
	markRestricted(t, database, accessErin)
	deps, err := NewDeps(DepsInput{
		Store:                 database,
		Identity:              headerIdentity(database),
		AgentTokens:           sharedAgentTokens(t, "agent-token"),
		ServerURL:             testServerURL,
		Docs:                  documentService,
		Events:                events.NewBroker(),
		AgentSecretsURL:       brokerURL,
		AgentSecretsTokenFile: accountTokenFile(t, credentialTestAccountToken),
		RestrictedAdmission:   restrictedAdmissionOf(true, database.Pool),
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	t.Cleanup(Register(mux, t.Context(), deps).Wait)
	if created := dispatchRequest(t, mux, http.MethodPost, "/api/v1/projects", map[string]string{"key": "ACC", "name": "Access project"}, "alice"); created.Code != http.StatusCreated {
		t.Fatalf("alice creates ACC: status=%d body=%s", created.Code, created.Body.String())
	}
	if listed := dispatchRequest(t, mux, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names erin on ACC's list: status=%d body=%s", listed.Code, listed.Body.String())
	}
	tokens := map[string]string{}
	for _, login := range []string{"alice", accessErin} {
		minted := mintPersonalToken(t, mux, login, "lists ACC", nil)
		if _, err := database.Pool.Exec(context.Background(), `update agent_tokens set projects = '{ACC}' where id = $1::uuid`, minted.ID); err != nil {
			t.Fatalf("list ACC on %s's token: %v", login, err)
		}
		tokens[login] = minted.Token
	}
	return mux, tokens
}

// revokePath is row's path with id in its {id}.
func revokePath(row, id string) string {
	_, pattern, _ := strings.Cut(row, " ")
	return strings.Replace(pattern, "{id}", id, 1)
}

// TestThePersonsOwnRowsAreTheTwoRevokesOfTheDeployment: personsOwnAtTheBroker names exactly the
// grant and sign-in revokes, each still classed scopeDeployment, so a credential that lists
// projects is refused them as every deployment row, and TestNoWriteReachesAHiddenObject holds them
// to that refusal as before.
func TestThePersonsOwnRowsAreTheTwoRevokesOfTheDeployment(t *testing.T) {
	if len(personsOwnAtTheBroker) != 2 || !personsOwnAtTheBroker[ownGrantRevoke] || !personsOwnAtTheBroker[ownSignInRevoke] {
		t.Fatalf("personsOwnAtTheBroker = %v, want %s and %s alone", personsOwnAtTheBroker, ownGrantRevoke, ownSignInRevoke)
	}
	for name := range personsOwnAtTheBroker {
		if writeClasses[name] != scopeDeployment {
			t.Errorf("%s is classed %q, want %q", name, writeClasses[name], scopeDeployment)
		}
	}
}

// TestARestrictedMemberEndsTheirOwnGrantsAndSignIns: a person ends their own sign-in and revokes
// their own grant whatever their member kind. erin, a restricted member at a browser, revokes the
// grant of a session she operates and her sign-in, and the broker no longer holds either. Her
// revoke of alice's sign-in stays the broker's to refuse, answered as carol's, a full member's, is
// (404 for a sign-in of somebody else). Her revoke of alice's grant is Dispatch's to refuse
// (memberGrants): 403 RESTRICTED_MEMBER where carol is answered the broker's 403 NOT_APPROVER, and
// no revoke of hers reaches the broker. Both of alice's stay held.
func TestARestrictedMemberEndsTheirOwnGrantsAndSignIns(t *testing.T) {
	broker := newOwnRevokeBroker(t)
	handler, _ := newOwnRevokeServer(t, broker.server.URL)

	for _, row := range []struct {
		row, others, own string
		// dispatchRefuses is whether Dispatch, not the broker, refuses erin's revoke of others.
		dispatchRefuses bool
	}{
		{ownGrantRevoke, "grant-alice", "grant-erin", true},
		{ownSignInRevoke, "signin-alice", "signin-erin", false},
	} {
		t.Run(row.row, func(t *testing.T) {
			full := dispatchRequest(t, handler, http.MethodPost, revokePath(row.row, row.others), nil, accessCarol)
			before := broker.revokes()
			restricted := dispatchRequest(t, handler, http.MethodPost, revokePath(row.row, row.others), nil, accessErin)
			switch {
			case full.Code < 400:
				t.Errorf("carol revoking alice's %s = %d %s, want a refusal", row.others, full.Code, strings.TrimSpace(full.Body.String()))
			case row.dispatchRefuses:
				if restricted.Code != http.StatusForbidden || !strings.Contains(restricted.Body.String(), `"code":"`+codeRestrictedMember+`"`) {
					t.Errorf("erin revoking alice's %s = %d %s, want 403 %s", row.others, restricted.Code, strings.TrimSpace(restricted.Body.String()), codeRestrictedMember)
				}
				if sent := broker.revokes() - before; sent != 0 {
					t.Errorf("erin's revoke of alice's %s sent the broker %d revokes, want none", row.others, sent)
				}
			case restricted.Code != full.Code || restricted.Body.String() != full.Body.String():
				t.Errorf("erin revoking alice's %s = %d %s, want carol's refusal %d %s",
					row.others, restricted.Code, strings.TrimSpace(restricted.Body.String()), full.Code, strings.TrimSpace(full.Body.String()))
			}
			if !broker.holds(row.others) {
				t.Errorf("alice's %s was revoked by somebody else", row.others)
			}

			own := dispatchRequest(t, handler, http.MethodPost, revokePath(row.row, row.own), nil, accessErin)
			if own.Code != http.StatusOK {
				t.Errorf("erin revoking her own %s = %d %s, want 200", row.own, own.Code, strings.TrimSpace(own.Body.String()))
			}
			if broker.holds(row.own) {
				t.Errorf("erin's own %s is still held after her revoke", row.own)
			}
		})
	}
}

// TestACredentialThatListsProjectsRevokesNoGrantOrSignIn: a revoke of a grant or a sign-in is
// still the deployment's to a credential that lists projects, whoever its person is, so alice's
// token that lists ACC is refused as before this change, byte for byte (R9), naming its list,
// which is the refusal's reason. erin's token, a restricted member's, holds no scope (R16), so the
// scope gate refuses it first, 403 OAUTH_SCOPE_REQUIRED. Neither reaches the broker. alice at a
// browser revokes her own grant and sign-in as before.
func TestACredentialThatListsProjectsRevokesNoGrantOrSignIn(t *testing.T) {
	broker := newOwnRevokeBroker(t)
	handler, tokens := newOwnRevokeServer(t, broker.server.URL)

	for _, row := range []struct{ row, alices, erins string }{
		{ownGrantRevoke, "grant-alice", "grant-erin"},
		{ownSignInRevoke, "signin-alice", "signin-erin"},
	} {
		t.Run(row.row, func(t *testing.T) {
			want := `{"code":"PROJECT_SCOPE_DEPLOYMENT","error":"this request's credential may write project ACC and nothing else; ` + row.row + ` belongs to the deployment rather than to a project"}`
			before := broker.revokes()
			refused := agentRequest(t, handler, http.MethodPost, revokePath(row.row, row.alices), nil, tokens["alice"])
			if got := strings.TrimSpace(refused.Body.String()); refused.Code != http.StatusForbidden || got != want {
				t.Errorf("alice's token that lists ACC revoking %s = %d %s, want 403 %s", row.alices, refused.Code, got, want)
			}
			wantRefusal(t, agentRequest(t, handler, http.MethodPost, revokePath(row.row, row.erins), nil, tokens[accessErin]),
				http.StatusForbidden, codeScopeRequired, "erin's token that lists ACC", "revoking "+row.erins)
			if calls := broker.revokes() - before; calls != 0 {
				t.Fatalf("the broker was sent %d revokes by a credential that lists projects", calls)
			}

			own := dispatchRequest(t, handler, http.MethodPost, revokePath(row.row, row.alices), nil, "alice")
			if own.Code != http.StatusOK || broker.holds(row.alices) {
				t.Errorf("alice revoking her own %s = %d %s, want 200 and it no longer held", row.alices, own.Code, strings.TrimSpace(own.Body.String()))
			}
		})
	}
}
