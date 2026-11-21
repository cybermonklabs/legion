package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestTheAgentsListNamesNoRoleOfAPrivateProjectItsReaderDoesNotSee: GET /api/v1/agents answers
// the sessions its reader may read (access.SessionClause), each with the roles the listener says
// it holds. A role's name can spell a project's key (legion-<project>-…, contracts.RoleProject),
// so a row naming legion-priv-architect tells a reader that does not see PRIV that PRIV exists.
// session-pub, which acted in PUB alone, holds that role in the hidden-holder seed: alice, an open
// member who sees PUB alone, and rhea, a restricted member PUB's list names, each read session-pub
// with no role, as access.RoleProjectClause leaves out every reference to such a role. carol, who
// sees PRIV, reads the role, and with no private project alice reads it too (R9).
//
// Control: the roles served as the listener sent them, and alice and rhea read
// legion-priv-architect on session-pub.
func TestTheAgentsListNamesNoRoleOfAPrivateProjectItsReaderDoesNotSee(t *testing.T) {
	seed := seedHiddenHolders(t, testServerOptions{})
	const rhea = "rhea@outside.example"
	seedPeople(t, seed.database, rhea)
	markRestricted(t, seed.database, rhea)
	seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/projects/"+seedPublic+"/access/people/"+rhea, nil, http.StatusOK)

	rolesOf := func(who string, response []byte) map[string][]string {
		t.Helper()
		var rows []struct {
			SessionID string   `json:"session_id"`
			Roles     []string `json:"roles"`
		}
		if err := json.Unmarshal(response, &rows); err != nil {
			t.Fatalf("%s: decode GET /api/v1/agents: %v\n%s", who, err, response)
		}
		roles := map[string][]string{}
		for _, row := range rows {
			roles[row.SessionID] = row.Roles
		}
		return roles
	}
	read := func(who string, login string) map[string][]string {
		t.Helper()
		response := dispatchRequest(t, seed.handler, http.MethodGet, "/api/v1/agents", nil, login)
		if response.Code != http.StatusOK {
			t.Fatalf("%s: GET /api/v1/agents = %d %s", who, response.Code, response.Body.String())
		}
		return rolesOf(who, response.Body.Bytes())
	}

	for _, reader := range []struct{ name, login string }{{"alice", seedAlice}, {"rhea, a restricted member of PUB", rhea}} {
		roles, listed := read(reader.name, reader.login)[sessionOfPublic]
		if !listed {
			t.Fatalf("%s reads no %s, so the row tests nothing", reader.name, sessionOfPublic)
		}
		if slices.Contains(roles, privateProjectRole) {
			t.Errorf("with PRIV private, %s reads %s holding %v, naming %s, a role of PRIV, which %s does not see",
				reader.name, sessionOfPublic, roles, privateProjectRole, reader.name)
		}
	}
	if roles := read("carol", seedCarol)[sessionOfPublic]; !slices.Contains(roles, privateProjectRole) {
		t.Errorf("carol, who sees PRIV, reads %s holding %v, want %s among them", sessionOfPublic, roles, privateProjectRole)
	}

	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	if roles := read("alice, with no private project", seedAlice)[sessionOfPublic]; !slices.Contains(roles, privateProjectRole) {
		t.Errorf("with no private project alice reads %s holding %v, want %s among them (R9)", sessionOfPublic, roles, privateProjectRole)
	}
}
