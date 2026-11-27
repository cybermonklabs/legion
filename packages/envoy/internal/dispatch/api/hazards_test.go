// hazards_test.go holds the red team's hazards of cybermonklabs/legion#4156 closed at their source
// (plan task t-a-34): a token is never read as a browser, every access.Resolve takes the request's
// own credential, the owner a create writes is a full member, groups are bounded and paged, and
// project.access_changed carries no access list.
package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// TestACreateCannotMakeARestrictedMemberTheOwner: a create by a credential that acts for nobody,
// the deployment's shared token, takes the default_assignee it names as the project's owner, and
// an owner is a full member, as R1's owner transfer holds one. Naming bob, a restricted member, is
// 400 OWNER_NOT_ALLOWED and writes no project; naming carol, a full member, makes her the owner.
func TestACreateCannotMakeARestrictedMemberTheOwner(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	seedPeople(t, database, accessCarol)
	markRestricted(t, database, "bob")
	create := func(key, assignee string) *httptest.ResponseRecorder {
		return agentRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]any{
			"key": key, "name": key, "default_assignee": assignee,
			"actor": map[string]string{"kind": "session", "id": "ses_creator"},
		}, "agent-token")
	}
	ownerOf := func(key string) []string {
		t.Helper()
		rows, err := database.Pool.Query(context.Background(), `select coalesce(owner, '') from projects where key = $1`, key)
		if err != nil {
			t.Fatalf("read %s's owner: %v", key, err)
		}
		defer rows.Close()
		var owners []string
		for rows.Next() {
			var owner string
			if err := rows.Scan(&owner); err != nil {
				t.Fatalf("read %s's owner: %v", key, err)
			}
			owners = append(owners, owner)
		}
		return owners
	}

	refused := create("NEW", "bob")
	if owners := ownerOf("NEW"); refused.Code != http.StatusBadRequest || !strings.Contains(refused.Body.String(), `"code":"OWNER_NOT_ALLOWED"`) || len(owners) != 0 {
		t.Fatalf("the shared token creates NEW for bob, a restricted member: %d %s, and NEW's owner rows are %v; want 400 OWNER_NOT_ALLOWED and no project",
			refused.Code, refused.Body.String(), owners)
	}
	if created := create("NEW", accessCarol); created.Code != http.StatusCreated || !slices.Equal(ownerOf("NEW"), []string{accessCarol}) {
		t.Fatalf("the shared token creates NEW for carol: %d %s, owner %v; want 201 and carol the owner", created.Code, created.Body.String(), ownerOf("NEW"))
	}
}
