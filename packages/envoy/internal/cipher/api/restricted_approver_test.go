// restricted_approver_test.go: the approver routes Dispatch's server relays for a person treat a
// person Dispatch calls a restricted member as no approver of a secret
// (requests/restricted_approver.go), even one on the private project's own list: GET /v1/grants
// does not list them a grant as its approver, and POST /v1/grants/{id}/revoke-by-approver and
// POST /v1/credential-requests/{record}/dismiss refuse them 403 NOT_APPROVER. While Dispatch
// answers a person a member kind this broker does not know, so whether they are a restricted
// member cannot be read, the revoke and the dismissal are 503 MEMBER_KIND_UNCHECKABLE. None of
// those writes anything, and a full member on the same list ends and dismisses the same rows.
package api_test

import (
	"net/http"
	"net/url"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/record"
)

// restrictedGuest is on PRIV's list from outside the sign-in domain, so Dispatch calls them a
// restricted member.
const restrictedGuest = "guest@outside.example"

// TestTheApproverRoutesGiveARestrictedMemberNothing: a grant of SHARED_KEY the model judge
// approved in anyone's stead, and an expired request for it, both of PRIV, whose list names carol
// and restrictedGuest. restrictedGuest's grant list does not hold the grant, and their revoke of it
// and their dismissal of the request are 403 NOT_APPROVER. While Dispatch answers carol's member
// kind with one this broker does not know, her revoke and her dismissal are 503
// MEMBER_KIND_UNCHECKABLE. Nothing is written by any of them. The control: once Dispatch calls
// carol a full member, her grant list holds the grant, and she ends it and dismisses the request.
func TestTheApproverRoutesGiveARestrictedMemberNothing(t *testing.T) {
	ts, access := hiddenServer(t, sharedKeyJudged)
	// The judge's grant asks Dispatch afresh whether a reader naming no person sees PRIV, which
	// only a public PRIV allows (requests.Machine.ApplyJudgeDecision).
	access.Serve(projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{
		"PRIV": {Visibility: "public", Owner: testApprover},
	}, nil))
	held := ts.askPrivate(t, "box-judged", "SHARED_KEY")
	judged, err := ts.machine.ApplyJudgeDecision(t.Context(), *held.RecordID, hiddenJudgeModel, map[string]any{"verdict": "grant"})
	if err != nil || judged.GrantID == "" {
		t.Fatalf("the judge's approval of SHARED_KEY while PRIV is public = %+v, %v; want a grant", judged, err)
	}
	if got := ts.column(t, `select approver from grants where id=$1`, judged.GrantID); !slices.Equal(got, []string{record.AnyoneApprover}) {
		t.Fatalf("the judged grant's approver = %v; want [%s]", got, record.AnyoneApprover)
	}
	lapsed := ts.askPrivate(t, "box-lapsed", "SHARED_KEY")
	ts.overdue(t, lapsed.RequestID)
	if got := ts.column(t, `select approver from credential_requests where id=$1`, *lapsed.RecordID); !slices.Equal(got, []string{record.AnyoneApprover}) {
		t.Fatalf("the expired request's approver = %v; want [%s]", got, record.AnyoneApprover)
	}

	stand := projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{
		"PRIV": {Visibility: "private", Owner: testApprover, Listed: []string{hiddenInsider, restrictedGuest}},
	}, map[string]string{
		testApprover:    projectaccess.MemberFull,
		hiddenInsider:   projectaccess.MemberFull,
		restrictedGuest: projectaccess.MemberRestricted,
	})
	access.Serve(stand)
	before := ts.writtenRows(t)
	listsGrant := func(t *testing.T, person string) bool {
		t.Helper()
		status, body := ts.ui(t, http.MethodGet, "/v1/grants?approver="+url.QueryEscape(person), nil)
		if status != http.StatusOK {
			t.Fatalf("GET /v1/grants?approver=%s = %d %s; want 200", person, status, body)
		}
		return slices.ContainsFunc(decode[struct {
			Grants []wireApproverGrant `json:"grants"`
		}](t, body).Grants, func(g wireApproverGrant) bool { return g.GrantID == judged.GrantID })
	}
	revoke := func(t *testing.T, person string) answerRead {
		t.Helper()
		status, body := ts.ui(t, http.MethodPost, "/v1/grants/"+judged.GrantID+"/revoke-by-approver", map[string]any{"approver": person})
		return answerOf(t, status, body)
	}
	dismiss := func(t *testing.T, person string) answerRead {
		t.Helper()
		status, body := ts.ui(t, http.MethodPost, "/v1/credential-requests/"+*lapsed.RecordID+"/dismiss", map[string]any{"approver": person})
		return answerOf(t, status, body)
	}

	t.Run("a restricted member on PRIV's list lists, ends and dismisses none of it", func(t *testing.T) {
		if listsGrant(t, restrictedGuest) {
			t.Errorf("GET /v1/grants?approver=%s lists the judged grant %s; want it left out", restrictedGuest, judged.GrantID)
		}
		requireAnswered(t, restrictedGuest+"'s revoke-by-approver of the judged grant", revoke(t, restrictedGuest), http.StatusForbidden, "NOT_APPROVER")
		requireAnswered(t, restrictedGuest+"'s dismissal of the expired request", dismiss(t, restrictedGuest), http.StatusForbidden, "NOT_APPROVER")
		if after := ts.writtenRows(t); !slices.Equal(after, before) {
			t.Fatalf("rows written = %v after the refused revoke and dismissal, want %v as before", after, before)
		}
	})

	stand.SetMemberKind(hiddenInsider, "guest-of-honour")
	access.Serve(stand)
	t.Run("while Dispatch answers a member kind the broker does not know, a member ends and dismisses none of it", func(t *testing.T) {
		requireAnswered(t, hiddenInsider+"'s revoke-by-approver while her kind cannot be read", revoke(t, hiddenInsider), http.StatusServiceUnavailable, "MEMBER_KIND_UNCHECKABLE")
		requireAnswered(t, hiddenInsider+"'s dismissal while her kind cannot be read", dismiss(t, hiddenInsider), http.StatusServiceUnavailable, "MEMBER_KIND_UNCHECKABLE")
		if after := ts.writtenRows(t); !slices.Equal(after, before) {
			t.Fatalf("rows written = %v after the refused revoke and dismissal, want %v as before", after, before)
		}
	})

	stand.SetMemberKind(hiddenInsider, projectaccess.MemberFull)
	access.Serve(stand)
	t.Run("a full member on PRIV's list lists, ends and dismisses it", func(t *testing.T) {
		if !listsGrant(t, hiddenInsider) {
			t.Fatalf("GET /v1/grants?approver=%s does not list the judged grant %s", hiddenInsider, judged.GrantID)
		}
		if got := dismiss(t, hiddenInsider); got.status != http.StatusOK {
			t.Fatalf("%s's dismissal of the expired request = %d %s; want 200", hiddenInsider, got.status, got.body)
		}
		if got := revoke(t, hiddenInsider); got.status != http.StatusOK || decode[stateBody](t, []byte(got.body)).State != "revoked" {
			t.Fatalf("%s's revoke-by-approver of the judged grant = %d %s; want 200 revoked", hiddenInsider, got.status, got.body)
		}
	})
}
