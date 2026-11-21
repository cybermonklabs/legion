// restricted_approver_test.go: the approver routes Dispatch's server relays for a person treat a
// person Dispatch calls a restricted member as no approver of a secret
// (requests/restricted_approver.go), even one on the private project's own list: GET /v1/grants
// does not list them a grant as its approver, and POST /v1/grants/{id}/revoke-by-approver and
// POST /v1/credential-requests/{record}/dismiss refuse them 403 NOT_APPROVER, writing nothing.
// While Dispatch answers a person a member kind this broker does not know, so whether they are a
// restricted member cannot be read, the dismissal is 503 MEMBER_KIND_UNCHECKABLE and writes
// nothing, and the revoke goes ahead, since a revoke only takes access away. A full member on the
// same list lists, ends and dismisses the same rows.
package api_test

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/record"
)

// restrictedGuest is on PRIV's list from outside the sign-in domain, so Dispatch calls them a
// restricted member.
const restrictedGuest = "guest@outside.example"

// TestTheApproverRoutesGiveARestrictedMemberNothing: two grants of SHARED_KEY the model judge
// approved in anyone's stead, and an expired request for it, all of PRIV, whose list names carol
// and restrictedGuest. restrictedGuest's grant list does not hold the first grant, and their revoke
// of it and their dismissal of the request are 403 NOT_APPROVER, with nothing written. While
// Dispatch answers carol's member kind with one this broker does not know, her dismissal is 503
// MEMBER_KIND_UNCHECKABLE with nothing written, and her revoke of the second grant ends it. The
// control: once Dispatch calls carol a full member, her grant list holds the first grant, and she
// dismisses the request and ends that grant.
func TestTheApproverRoutesGiveARestrictedMemberNothing(t *testing.T) {
	ts, access := hiddenServer(t, sharedKeyJudged)
	// The judge's grant asks Dispatch afresh whether a reader naming no person sees PRIV, which
	// only a public PRIV allows (requests.Machine.ApplyJudgeDecision).
	access.Serve(projectaccesstest.NewServer(t, map[string]projectaccesstest.Row{
		"PRIV": {Visibility: "public", Owner: testApprover},
	}, nil))
	judgedGrant := func(t *testing.T, box string) string {
		t.Helper()
		held := ts.askPrivate(t, box, "SHARED_KEY")
		judged, err := ts.machine.ApplyJudgeDecision(t.Context(), *held.RecordID, hiddenJudgeModel, map[string]any{"verdict": "grant"})
		if err != nil || judged.GrantID == "" {
			t.Fatalf("the judge's approval of SHARED_KEY on %s while PRIV is public = %+v, %v; want a grant", box, judged, err)
		}
		if got := ts.column(t, `select approver from grants where id=$1`, judged.GrantID); !slices.Equal(got, []string{record.AnyoneApprover}) {
			t.Fatalf("the judged grant's approver = %v; want [%s]", got, record.AnyoneApprover)
		}
		return judged.GrantID
	}
	grant := judgedGrant(t, "box-judged")
	unreadGrant := judgedGrant(t, "box-judged-unread")
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
	listsGrant := func(t *testing.T, person, grantID string) bool {
		t.Helper()
		status, body := ts.ui(t, http.MethodGet, "/v1/grants?approver="+url.QueryEscape(person), nil)
		if status != http.StatusOK {
			t.Fatalf("GET /v1/grants?approver=%s = %d %s; want 200", person, status, body)
		}
		return slices.ContainsFunc(decode[struct {
			Grants []wireApproverGrant `json:"grants"`
		}](t, body).Grants, func(g wireApproverGrant) bool { return g.GrantID == grantID })
	}
	revoke := func(t *testing.T, person, grantID string) answerRead {
		t.Helper()
		status, body := ts.ui(t, http.MethodPost, "/v1/grants/"+grantID+"/revoke-by-approver", map[string]any{"approver": person})
		return answerOf(t, status, body)
	}
	dismiss := func(t *testing.T, person string) answerRead {
		t.Helper()
		status, body := ts.ui(t, http.MethodPost, "/v1/credential-requests/"+*lapsed.RecordID+"/dismiss", map[string]any{"approver": person})
		return answerOf(t, status, body)
	}
	revoked := func(t *testing.T, got answerRead, what string) {
		t.Helper()
		if got.status != http.StatusOK || decode[stateBody](t, []byte(got.body)).State != "revoked" {
			t.Fatalf("%s = %d %s; want 200 revoked", what, got.status, got.body)
		}
	}

	t.Run("a restricted member on PRIV's list lists, ends and dismisses none of it", func(t *testing.T) {
		if listsGrant(t, restrictedGuest, grant) {
			t.Errorf("GET /v1/grants?approver=%s lists the judged grant %s; want it left out", restrictedGuest, grant)
		}
		requireAnswered(t, restrictedGuest+"'s revoke-by-approver of the judged grant", revoke(t, restrictedGuest, grant), http.StatusForbidden, "NOT_APPROVER")
		requireAnswered(t, restrictedGuest+"'s dismissal of the expired request", dismiss(t, restrictedGuest), http.StatusForbidden, "NOT_APPROVER")
		if after := ts.writtenRows(t); !slices.Equal(after, before) {
			t.Fatalf("rows written = %v after the refused revoke and dismissal, want %v as before", after, before)
		}
	})

	stand.SetMemberKind(hiddenInsider, "guest-of-honour")
	access.Serve(stand)
	t.Run("while Dispatch answers a member kind the broker does not know, a member's dismissal is refused and her revoke goes ahead", func(t *testing.T) {
		requireAnswered(t, hiddenInsider+"'s dismissal while her kind cannot be read", dismiss(t, hiddenInsider), http.StatusServiceUnavailable, "MEMBER_KIND_UNCHECKABLE")
		if after := ts.writtenRows(t); !slices.Equal(after, before) {
			t.Fatalf("rows written = %v after the refused dismissal, want %v as before", after, before)
		}
		revoked(t, revoke(t, hiddenInsider, unreadGrant), hiddenInsider+"'s revoke-by-approver while her kind cannot be read")
		if got := ts.column(t, `select revoked_by from grants where id=$1`, unreadGrant); !slices.Equal(got, []string{record.HumanActor(hiddenInsider)}) {
			t.Fatalf("the grant's revoked_by = %v; want [%s]", got, record.HumanActor(hiddenInsider))
		}
	})

	stand.SetMemberKind(hiddenInsider, projectaccess.MemberFull)
	access.Serve(stand)
	t.Run("a full member on PRIV's list lists, ends and dismisses it", func(t *testing.T) {
		if !listsGrant(t, hiddenInsider, grant) {
			t.Fatalf("GET /v1/grants?approver=%s does not list the judged grant %s", hiddenInsider, grant)
		}
		if got := dismiss(t, hiddenInsider); got.status != http.StatusOK {
			t.Fatalf("%s's dismissal of the expired request = %d %s; want 200", hiddenInsider, got.status, got.body)
		}
		revoked(t, revoke(t, hiddenInsider, grant), hiddenInsider+"'s revoke-by-approver of the judged grant")
	})
}

// TestARestrictedMemberDeniesNoServicesLoginOnTheRoute: a service's login a broker from before
// token logins opened, still pending, which anyone signed in may deny
// (TestAPendingServiceLoginFromBeforeTokenLoginsIsNeverApproved). POST
// /v1/credential-requests/{record}/deny relayed for restrictedGuest is 403 NOT_APPROVER, and for
// carol while Dispatch answers her a member kind this broker does not know 503
// MEMBER_KIND_UNCHECKABLE, each deciding nothing; for bob, a full member, the control, it is 200
// denied.
func TestARestrictedMemberDeniesNoServicesLoginOnTheRoute(t *testing.T) {
	access := &projectaccesstest.Swappable{}
	stand := projectaccesstest.NewServer(t, nil, map[string]string{
		restrictedGuest:   projectaccess.MemberRestricted,
		hiddenInsider:     projectaccess.MemberFull,
		"bob@example.com": projectaccess.MemberFull,
	})
	access.Serve(stand)
	ts := newTestServerWith(t, func(d *api.Deps) { d.Enroll.Projects = access })
	body := record.Body{
		Request:         signMachineLoginRequest(t, newSigningKey(t), ts.URL, "", "cluster.example", "legion-daemon"),
		Approver:        record.AnyoneApprover,
		Enrollment:      record.Enrollment{Kind: "-", RuntimeID: "-"},
		LifetimeSeconds: new(int((7 * 24 * time.Hour).Seconds())),
		RulesVersion:    "pre-token-logins",
		ExpiresAt:       time.Now().Add(15 * time.Minute).UTC().Truncate(time.Second),
		Code:            "WXYZ-ABCD",
	}
	if _, err := ts.Store.Pool.Exec(context.Background(), `insert into credential_requests (id, body, kind, approver, code, expires_at) values ($1,$2,'launcher_credential',$3,$4,$5)`,
		body.ID(), body.Canonical(), body.Approver, body.Code, body.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	deny := func(t *testing.T, person string) answerRead {
		t.Helper()
		status, answer := ts.ui(t, http.MethodPost, "/v1/credential-requests/"+body.ID()+"/deny", map[string]any{"approver": person, "code": body.Code})
		return answerOf(t, status, answer)
	}
	decisions := func(t *testing.T) []string {
		t.Helper()
		return ts.column(t, `select count(*)::text from credential_request_events where record_id=$1`, body.ID())
	}

	requireAnswered(t, restrictedGuest+"'s deny of a service's login", deny(t, restrictedGuest), http.StatusForbidden, "NOT_APPROVER")
	stand.SetMemberKind(hiddenInsider, "guest-of-honour")
	access.Serve(stand)
	requireAnswered(t, hiddenInsider+"'s deny while her kind cannot be read", deny(t, hiddenInsider), http.StatusServiceUnavailable, "MEMBER_KIND_UNCHECKABLE")
	if got := decisions(t); !slices.Equal(got, []string{"0"}) {
		t.Fatalf("decisions of the service's login after the refused denies = %v; want none", got)
	}
	if got := deny(t, "bob@example.com"); got.status != http.StatusOK || decode[stateBody](t, []byte(got.body)).State != "denied" {
		t.Fatalf("bob's deny of the service's login = %d %s; want 200 denied", got.status, got.body)
	}
}
