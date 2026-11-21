package api

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestTheAccessAnswerAdmitsOnlyItsAccounts (K3): only a service account DISPATCH_ACCESS_ACCOUNTS
// names reads the membership answer; another verified account, a person and the shared token are
// 403 ACCESS_ACCOUNT_REQUIRED on both routes, and a caller with no credential is 401 NO_IDENTITY.
func TestTheAccessAnswerAdmitsOnlyItsAccounts(t *testing.T) {
	issuer := newServiceTokenIssuer(t)
	handler, database, _ := newTestServer(t, testServerOptions{oidc: issuer.verifier(t), accessAccounts: []string{listedServiceAccount}})
	seedPeople(t, database, accessCarol)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "ACC", "name": "Access"}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create ACC: %d %s", response.Code, response.Body.String())
	}
	token := func(subject string) string {
		return issuer.Mint(t, issuer.signing, issuer.Claims(subject, serviceTokenAudience))
	}
	for _, path := range []string{"/api/v1/access/projects/ACC?person=alice", "/api/v1/access/person?person=alice"} {
		if response := agentRequest(t, handler, http.MethodGet, path, nil, token(listedServiceAccount)); response.Code != http.StatusOK {
			t.Errorf("%s as %s = %d %s, want 200", path, listedServiceAccount, response.Code, response.Body.String())
		}
		wantRefusal(t, agentRequest(t, handler, http.MethodGet, path, nil, token(unlistedServiceAccount)),
			http.StatusForbidden, "ACCESS_ACCOUNT_REQUIRED", unlistedServiceAccount, "GET "+path)
		wantRefusal(t, agentRequest(t, handler, http.MethodGet, path, nil, "agent-token"),
			http.StatusForbidden, "ACCESS_ACCOUNT_REQUIRED", "the shared token", "GET "+path)
		wantRefusal(t, dispatchRequest(t, handler, http.MethodGet, path, nil, "alice"),
			http.StatusForbidden, "ACCESS_ACCOUNT_REQUIRED", "alice", "GET "+path)
		wantRefusal(t, dispatchRequest(t, handler, http.MethodGet, path, nil, ""),
			http.StatusUnauthorized, "NO_IDENTITY", "a caller with no credential", "GET "+path)
	}
	wantRefusal(t, agentRequest(t, handler, http.MethodGet, "/api/v1/access/person", nil, token(listedServiceAccount)),
		http.StatusBadRequest, "INVALID_QUERY", listedServiceAccount, "GET /api/v1/access/person with no person")
	wantRefusal(t, agentRequest(t, handler, http.MethodGet, "/api/v1/access/projects/NOPE?person=alice", nil, token(listedServiceAccount)),
		http.StatusNotFound, "PROJECT_NOT_FOUND", listedServiceAccount, "GET an unknown project's answer")
}

// TestTheAccessAnswerNamesTheProjectAndThePerson: the answer names the project's owner and
// visibility, the person as the people table spells them, their member kind and whether they see
// it. With no person, member_kind is none and visible says whether the project is public, the
// rule's answer for a viewer with no person, which the secrets broker reads it as
// (projectaccess.Client.States).
func TestTheAccessAnswerNamesTheProjectAndThePerson(t *testing.T) {
	issuer := newServiceTokenIssuer(t)
	handler, database, _ := newTestServer(t, testServerOptions{oidc: issuer.verifier(t), accessAccounts: []string{listedServiceAccount}})
	for _, key := range []string{"ACC", "HID"} {
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": key, "name": key}, "alice"); response.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", key, response.Code, response.Body.String())
		}
	}
	if _, err := database.Pool.Exec(context.Background(), `update projects set visibility = 'private' where key = 'HID'`); err != nil {
		t.Fatalf("make HID private: %v", err)
	}
	bearer := issuer.Mint(t, issuer.signing, issuer.Claims(listedServiceAccount, serviceTokenAudience))
	response := agentRequest(t, handler, http.MethodGet, "/api/v1/access/projects/ACC?person=Alice", nil, bearer)
	answer := decodeBody[model.AccessAnswer](t, response)
	want := model.AccessAnswer{Project: "ACC", Visibility: model.ProjectPublic, Owner: "alice", Person: "alice", MemberKind: model.MemberAnswerFull, Visible: true, SlackChannels: []string{}}
	if response.Code != http.StatusOK || answer.Project != want.Project || answer.Visibility != want.Visibility || answer.Owner != want.Owner ||
		answer.Person != want.Person || answer.MemberKind != want.MemberKind || answer.Visible != want.Visible || !slices.Equal(answer.SlackChannels, want.SlackChannels) {
		t.Fatalf("the answer = %d %+v, want %+v", response.Code, answer, want)
	}
	for _, project := range []struct {
		key     string
		visible bool
	}{{"ACC", true}, {"HID", false}} {
		alone := decodeBody[model.AccessAnswer](t, agentRequest(t, handler, http.MethodGet, "/api/v1/access/projects/"+project.key, nil, bearer))
		if alone.Visible != project.visible || alone.MemberKind != model.MemberAnswerNone || alone.Person != "" || alone.Project != project.key {
			t.Fatalf("the answer with no person = %+v, want %s with visible %v (whether it is public) and member_kind none", alone, project.key, project.visible)
		}
	}
}
