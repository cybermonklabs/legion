package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestAGroupOnlyItsOwnerEdits: the first write of a group makes it and its writer owns it; after
// that only the owner writes it, gives it to another full member, or deletes it.
func TestAGroupOnlyItsOwnerEdits(t *testing.T) {
	handler, database := newAccessServer(t)
	made := dispatchRequest(t, handler, http.MethodPut, "/api/v1/access-groups/crew", map[string]any{"members": []string{accessCarol}}, "alice")
	if group := decodeBody[model.AccessGroup](t, made); made.Code != http.StatusOK || group.Owner != "alice" || !slices.Equal(group.Members, []string{accessCarol}) {
		t.Fatalf("alice makes crew: %d %s, want alice's group of carol", made.Code, made.Body.String())
	}
	before := allEvents(t, database)
	wantRefusal(t, dispatchRequest(t, handler, http.MethodPut, "/api/v1/access-groups/crew", map[string]any{"members": []string{"bob@example.com"}}, "bob"),
		http.StatusForbidden, "GROUP_OWNER_REQUIRED", "bob", "PUT alice's group")
	wantRefusal(t, dispatchRequest(t, handler, http.MethodDelete, "/api/v1/access-groups/crew", nil, "bob"),
		http.StatusForbidden, "GROUP_OWNER_REQUIRED", "bob", "DELETE alice's group")
	if after := allEvents(t, database); after != before {
		t.Fatalf("bob's refused writes moved events from %d to %d", before, after)
	}
	wantRefusal(t, dispatchRequest(t, handler, http.MethodPut, "/api/v1/access-groups/visitors", map[string]any{"members": []string{}}, accessErin),
		http.StatusForbidden, "RESTRICTED_MEMBER", accessErin, "PUT a new group")
	wantRefusal(t, dispatchRequest(t, handler, http.MethodPut, "/api/v1/access-groups/theirs", map[string]any{"members": []string{}, "owner": accessDave}, "alice"),
		http.StatusBadRequest, "PROJECT_ACCESS_INPUT", "alice", "PUT a new group owned by somebody else")

	given := dispatchRequest(t, handler, http.MethodPut, "/api/v1/access-groups/crew", map[string]any{"members": []string{accessDave, accessCarol}, "owner": accessDave}, "alice")
	if group := decodeBody[model.AccessGroup](t, given); given.Code != http.StatusOK || group.Owner != accessDave || !slices.Equal(group.Members, []string{accessCarol, accessDave}) {
		t.Fatalf("alice gives crew to dave: %d %s", given.Code, given.Body.String())
	}
	wantRefusal(t, dispatchRequest(t, handler, http.MethodPut, "/api/v1/access-groups/crew", map[string]any{"members": []string{}}, "alice"),
		http.StatusForbidden, "GROUP_OWNER_REQUIRED", "alice", "PUT crew after giving it away")
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/access-groups", nil, "bob")
	if page := decodeBody[model.AccessGroupPage](t, listed); listed.Code != http.StatusOK || len(page.Groups) != 1 || page.Groups[0].Name != "crew" || page.Next != "" {
		t.Fatalf("GET /api/v1/access-groups = %d %s, want crew", listed.Code, listed.Body.String())
	}
	if deleted := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/access-groups/crew", nil, accessDave); deleted.Code != http.StatusNoContent {
		t.Fatalf("dave deletes crew: %d %s", deleted.Code, deleted.Body.String())
	}
	wantRefusal(t, dispatchRequest(t, handler, http.MethodDelete, "/api/v1/access-groups/crew", nil, accessDave),
		http.StatusNotFound, "GROUP_NOT_FOUND", accessDave, "DELETE crew again")
}

// TestAGroupRefusesARestrictedMember (v24 R2): a group holds full members only, so a write naming a
// restricted member is 400 GROUP_MEMBER_RESTRICTED, naming them, and stores no member at all.
func TestAGroupRefusesARestrictedMember(t *testing.T) {
	handler, database := newAccessServer(t)
	response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/access-groups/mixed", map[string]any{"members": []string{accessCarol, accessErin}}, "alice")
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"GROUP_MEMBER_RESTRICTED"`) ||
		!strings.Contains(response.Body.String(), accessErin) {
		t.Fatalf("a group naming %s = %d %s, want 400 GROUP_MEMBER_RESTRICTED naming them", accessErin, response.Code, response.Body.String())
	}
	if stored := countRows(t, database, `select count(*) from access_group_members where person = $1`, accessErin); stored != 0 {
		t.Fatalf("%s was stored in %d groups, want none", accessErin, stored)
	}
	if stored := countRows(t, database, `select count(*) from access_groups where name = 'mixed'`); stored != 0 {
		t.Fatalf("the refused group was stored")
	}
}
