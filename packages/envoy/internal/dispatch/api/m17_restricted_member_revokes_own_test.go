package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

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
