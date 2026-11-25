package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestAGroupOnlyItsOwnerEdits: the first write of a group makes it and its writer owns it; after
// that only the owner writes it, gives it to another full member, or deletes it. A restricted
// member makes none: 403 RESTRICTED_MEMBER, the refusal of a deployment write (withinProjectScope).
func TestAGroupOnlyItsOwnerEdits(t *testing.T) {
	t.Parallel()
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

// TestTheGroupWritesNeedTheApproveScope: a group's members matter on a private project whose list
// names it, so writing a group - a new member, or a new owner who then controls it - and deleting
// one, which takes it off every list that names it, decide who may see that project as the
// access-list writes do. A credential of the owner's that holds `person` alone is refused on both
// routes exactly as on the five list writes, and its member add admits nobody; the owner's
// credential that holds `approve` as well writes, and the admission follows.
func TestTheGroupWritesNeedTheApproveScope(t *testing.T) {
	t.Parallel()
	handler, database := newAccessServer(t)
	if made := dispatchRequest(t, handler, http.MethodPut, "/api/v1/access-groups/crew", map[string]any{"members": []string{accessCarol}}, "alice"); made.Code != http.StatusOK {
		t.Fatalf("alice makes crew: %d %s", made.Code, made.Body.String())
	}
	if named := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/ACC/access/groups/crew", nil, "alice"); named.Code != http.StatusOK {
		t.Fatalf("alice names crew on ACC: %d %s", named.Code, named.Body.String())
	}
	// No route makes a project private in this release (409 PRIVATE_PROJECTS_OFF), and a full
	// member sees every public project, so the list decides dave's admission only once ACC is
	// private.
	if _, err := database.Pool.Exec(context.Background(), `update projects set visibility = 'private' where key = 'ACC'`); err != nil {
		t.Fatalf("make ACC private: %v", err)
	}
	daveSeesACC := func() bool {
		t.Helper()
		answer, err := access.Answer(context.Background(), database.Pool, "ACC", accessDave)
		if err != nil {
			t.Fatalf("read dave's membership answer on ACC: %v", err)
		}
		return answer.Visible
	}
	if daveSeesACC() {
		t.Fatalf("dave sees private ACC before crew names him")
	}
	personOnly := personalTokenOwnedBy(t, database, "alice", model.ScopePerson)
	approving := personalTokenOwnedBy(t, database, "alice", model.ScopePerson, model.ScopeApprove)
	// The refusal a list write answers this token, which each group write must answer byte for byte.
	listRefusal := agentRequest(t, handler, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessDave, nil, personOnly)
	wantApproveRefusal(t, listRefusal, "PUT dave on ACC's list")

	addDave := map[string]any{"members": []string{accessCarol, accessDave}}
	for _, write := range []struct {
		method string
		body   any
		call   string
	}{
		{http.MethodPut, addDave, "add dave to crew"},
		{http.MethodPut, map[string]any{"members": []string{accessCarol}, "owner": accessDave}, "give crew to dave"},
		{http.MethodDelete, nil, "delete crew"},
	} {
		before := allEvents(t, database)
		refused := agentRequest(t, handler, write.method, "/api/v1/access-groups/crew", write.body, personOnly)
		wantApproveRefusal(t, refused, write.call)
		if refused.Body.String() != listRefusal.Body.String() {
			t.Fatalf("%s with alice's person token answers %s, want the list write's %s", write.call, refused.Body.String(), listRefusal.Body.String())
		}
		group, err := access.ReadGroup(context.Background(), database.Pool, "crew")
		if err != nil {
			t.Fatalf("read crew after the refused %s: %v", write.call, err)
		}
		if after := allEvents(t, database); after != before || group.Owner != "alice" || !slices.Equal(group.Members, []string{accessCarol}) {
			t.Fatalf("the refused %s moved events from %d to %d and left crew %+v", write.call, before, after, group)
		}
		if got := readAccess(t, handler, "alice").Groups; !slices.Equal(got, []string{"crew"}) || daveSeesACC() {
			t.Fatalf("after the refused %s ACC's groups = %v and dave sees ACC = %v, want crew and false", write.call, got, daveSeesACC())
		}
	}

	if added := agentRequest(t, handler, http.MethodPut, "/api/v1/access-groups/crew", addDave, approving); added.Code != http.StatusOK {
		t.Fatalf("add dave to crew with alice's approving token: %d %s, want 200", added.Code, added.Body.String())
	}
	if !daveSeesACC() {
		t.Fatalf("dave does not see ACC after the approving token added him to crew")
	}
	if deleted := agentRequest(t, handler, http.MethodDelete, "/api/v1/access-groups/crew", nil, approving); deleted.Code != http.StatusNoContent {
		t.Fatalf("delete crew with alice's approving token: %d %s, want 204", deleted.Code, deleted.Body.String())
	}
	if got := readAccess(t, handler, "alice").Groups; len(got) != 0 || daveSeesACC() {
		t.Fatalf("after the approving delete ACC's groups = %v and dave sees ACC = %v, want none and false", got, daveSeesACC())
	}
}

// TestAGroupRefusesARestrictedMember (v24 R2): a group holds full members only, so a write naming a
// restricted member is 400 GROUP_MEMBER_RESTRICTED, naming them, and stores no member at all.
func TestAGroupRefusesARestrictedMember(t *testing.T) {
	t.Parallel()
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
