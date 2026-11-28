package api

// v2_role_holders.go is an issue's role holders under /api/v2: the two routes #4228 added to
// /api/v1 before /api/v2's change merged (testdata/v1-only-routes.txt), each answered at
// /api/v2/issues/{key}/role-holder-set, a singleton of the issue whose PUT replaces it whole. A
// singleton that holds a set is named the singular of the plural /api/v1 named it by, and -set
// (the architect's ruling, dispatch://INFRA-4071/comment/fa6c9d57-e6a5-462b-8d95-063e5b48b163),
// so the name is singular, as R1.1 of dispatch://INFRA-2946/spec names a singleton, and a reader
// of the /api/v1 name can tell it. Both run the work their /api/v1 route runs
// (getIssueRoleHolders, putIssueRoleHoldersFrom), the issue checked first and answered 404
// ISSUE_NOT_FOUND (dispatch://INFRA-4071/spec R7).

import (
	"cmp"
	"net/http"
	"slices"

	"github.com/sjawhar/envoy/internal/legionagents"
)

// issueRoleHoldersText is what both role-holder routes' descriptions say of the set they answer.
const issueRoleHoldersText = "{issue, holders}, each holder {role, session, recorded_at} in workflow order, [] when " +
	"no session holds a role"

// putIssueRoleHoldersV2 answers PUT /api/v2/issues/{key}/role-holder-set: the publication PUT
// /api/v1/issues/{key}/role-holders makes (putIssueRoleHoldersFrom), once the no-op rule of
// dispatch://INFRA-4071/spec R8 has found the body is a change. The set records no setter, and the
// daemon of the issue's project is its one writer, so a body naming the stored set answers it and
// writes nothing (noSetterRecorded), once the core has refused the writers a write refuses, asked
// the proof a write would ask for and found the project marked and each pair's daemon_proven what
// the write would write (putIssueRoleHoldersFrom). The set is compared in workflow order, the
// order it is stored and answered in, since the body names a set and its order says nothing.
func (s *server) putIssueRoleHoldersV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !s.requireIssueV2(w, r, r.PathValue("key")) {
		return
	}
	body := v2Body[issueRoleHoldersInput](r)
	s.putIssueRoleHoldersFrom(w, r, body, func(stored issueRoleHolders) bool {
		sent := body
		ordered := slices.SortedFunc(slices.Values(*body.Holders), func(left, right issueRoleHolder) int {
			return cmp.Compare(slices.Index(legionagents.Roles, left.Role), slices.Index(legionagents.Roles, right.Role))
		})
		sent.Holders = &ordered
		held := make([]issueRoleHolder, len(stored.Holders))
		for index, holder := range stored.Holders {
			held[index] = issueRoleHolder{Role: holder.Role, Session: holder.Session}
		}
		present := sent
		present.Holders = &held
		return putChangesNothing(sent, present, noSetterRecorded, "")
	})
}
