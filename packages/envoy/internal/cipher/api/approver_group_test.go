package api_test

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/record"
)

// TestARouteRefusesAGroupAsItsApprover: every UI route names the person it acts for, and a login
// spelled as a group (group:<address>) is no person: the list routes and the revoke refuse it 400
// APPROVER_IS_GROUP, so no caller lists a group's records or grants, or revokes one, by naming the
// group, however stale its members are.
func TestARouteRefusesAGroupAsItsApprover(t *testing.T) {
	ts := newTestServer(t)
	group := record.GroupPrefix + "approvers@example.com"
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/v1/pending?approver=" + url.QueryEscape(group), nil},
		{http.MethodGet, "/v1/grants?approver=" + url.QueryEscape(group), nil},
		{http.MethodPost, "/v1/grants/00000000-0000-4000-8000-000000000000/revoke-by-approver", map[string]any{"approver": " Group:Approvers@Example.com "}},
	} {
		status, body := ts.ui(t, c.method, c.path, c.body)
		if status != http.StatusBadRequest || decode[wireError](t, body).Code != "APPROVER_IS_GROUP" {
			t.Errorf("%s %s = %d %s, want 400 APPROVER_IS_GROUP", c.method, c.path, status, body)
		}
	}
}
