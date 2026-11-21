// restricted_approver_test.go: the approver routes Dispatch's server relays for a person treat a
// person Dispatch calls a restricted member as no approver of a secret
// (requests/restricted_approver.go), even one on the private project's own list: GET /v1/grants
// does not list them a grant as its approver, and POST /v1/grants/{id}/revoke-by-approver and
// POST /v1/credential-requests/{record}/dismiss refuse them 403 NOT_APPROVER, saying why, writing
// nothing. While Dispatch answers a person a member kind this broker does not know, so whether
// they are a restricted member cannot be read, the dismissal and the revoke are 503
// MEMBER_KIND_UNCHECKABLE and write nothing: that failure lasts until somebody fixes it, so the
// revoke goes ahead only while Dispatch does not answer at all. A full member on the same list
// lists, ends and dismisses the same rows. Nor is such a person an approver of a service's
// machine login: its legacy login is on no list of theirs and theirs to dismiss or deny nowhere,
// its token login is on no list of theirs and theirs to end nowhere, and a service pod's grant is
// on no grant list of theirs.
package api_test

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

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
// of it and their dismissal of the request are 403 NOT_APPROVER, each saying that a restricted
// member is no approver of a secret, with nothing written. While Dispatch answers carol's member
// kind with one this broker does not know, her dismissal and her revoke of the second grant are
// 503 MEMBER_KIND_UNCHECKABLE with nothing written. The control: once Dispatch calls carol a full
// member, her grant list holds the first grant, and she dismisses the request and ends that grant.
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
		for what, got := range map[string]answerRead{
			restrictedGuest + "'s revoke-by-approver of the judged grant": revoke(t, restrictedGuest, grant),
			restrictedGuest + "'s dismissal of the expired request":       dismiss(t, restrictedGuest),
		} {
			requireAnswered(t, what, got, http.StatusForbidden, "NOT_APPROVER")
			if refusal := decode[wireError](t, []byte(got.body)).Error; !strings.Contains(refusal, "restricted member") {
				t.Errorf("%s says %q; want it to say the person is a restricted member", what, refusal)
			}
		}
		if after := ts.writtenRows(t); !slices.Equal(after, before) {
			t.Fatalf("rows written = %v after the refused revoke and dismissal, want %v as before", after, before)
		}
	})

	stand.SetMemberKind(hiddenInsider, "guest-of-honour")
	access.Serve(stand)
	t.Run("while Dispatch answers a member kind the broker does not know, a member's dismissal and revoke are refused", func(t *testing.T) {
		requireAnswered(t, hiddenInsider+"'s dismissal while her kind cannot be read", dismiss(t, hiddenInsider), http.StatusServiceUnavailable, "MEMBER_KIND_UNCHECKABLE")
		requireAnswered(t, hiddenInsider+"'s revoke-by-approver while her kind cannot be read", revoke(t, hiddenInsider, unreadGrant), http.StatusServiceUnavailable, "MEMBER_KIND_UNCHECKABLE")
		if after := ts.writtenRows(t); !slices.Equal(after, before) {
			t.Fatalf("rows written = %v after the refused dismissal and revoke, want %v as before", after, before)
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
	recordID := ts.insertLegacyServiceLogin(t, "WXYZ-ABCD", time.Now().Add(15*time.Minute))
	deny := func(t *testing.T, person string) answerRead {
		t.Helper()
		status, answer := ts.ui(t, http.MethodPost, "/v1/credential-requests/"+recordID+"/deny", map[string]any{"approver": person, "code": "WXYZ-ABCD"})
		return answerOf(t, status, answer)
	}
	decisions := func(t *testing.T) []string {
		t.Helper()
		return ts.column(t, `select count(*)::text from credential_request_events where record_id=$1`, recordID)
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

// insertLegacyServiceLogin writes the Legion daemon's machine login on cluster.example as a broker
// from before token logins opened it, waiting on anyone, with the user code code and the deadline
// expires, and answers its record's id.
func (ts *testServer) insertLegacyServiceLogin(t *testing.T, code string, expires time.Time) string {
	t.Helper()
	body := record.Body{
		Request:         signMachineLoginRequest(t, newSigningKey(t), ts.URL, "", "cluster.example", "legion-daemon"),
		Approver:        record.AnyoneApprover,
		Enrollment:      record.Enrollment{Kind: "-", RuntimeID: "-"},
		LifetimeSeconds: new(int((7 * 24 * time.Hour).Seconds())),
		RulesVersion:    "pre-token-logins",
		ExpiresAt:       expires.UTC().Truncate(time.Second),
		Code:            code,
	}
	if _, err := ts.Store.Pool.Exec(context.Background(), `insert into credential_requests (id, body, kind, approver, code, expires_at) values ($1,$2,'launcher_credential',$3,$4,$5)`,
		body.ID(), body.Canonical(), body.Approver, body.Code, body.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	return body.ID()
}

// restrictedStand is a Dispatch stand-in that calls restrictedGuest a restricted member and carol
// and bob full members, behind a client a test swaps, and a broker that asks it from both the
// request machine and the enroll service.
func restrictedStand(t *testing.T) (*testServer, *projectaccesstest.Swappable, *projectaccesstest.Server) {
	t.Helper()
	access := &projectaccesstest.Swappable{}
	stand := projectaccesstest.NewServer(t, nil, map[string]string{
		restrictedGuest:   projectaccess.MemberRestricted,
		hiddenInsider:     projectaccess.MemberFull,
		"bob@example.com": projectaccess.MemberFull,
	})
	access.Serve(stand)
	ts := newTestServerWith(t, func(d *api.Deps) {
		d.Enroll.Projects = access
		d.Machine.Projects = access
	})
	return ts, access, stand
}

// TestAServicesLegacyLoginIsOnNoRestrictedMembersList: a service's machine login a broker from
// before token logins opened waits on anyone, so it is on every signed-in person's Inbox, pending
// and, once its deadline passed, expired until somebody dismisses it, which takes it off every
// list. A person Dispatch calls a restricted member is no approver of it: GET /v1/pending lists
// them neither record, and their dismissal of the expired one is 403 NOT_APPROVER, saying why,
// dismissing nothing. The control: bob, a full member, lists both and dismisses the expired one.
func TestAServicesLegacyLoginIsOnNoRestrictedMembersList(t *testing.T) {
	ts, _, _ := restrictedStand(t)
	pending := ts.insertLegacyServiceLogin(t, "WXYZ-ABCD", time.Now().Add(15*time.Minute))
	expired := ts.insertLegacyServiceLogin(t, "QRST-UVWX", time.Now().Add(-time.Hour))
	listed := func(t *testing.T, person string) []string {
		t.Helper()
		status, body := ts.ui(t, http.MethodGet, "/v1/pending?approver="+url.QueryEscape(person)+"&state=pending&state=expired", nil)
		if status != http.StatusOK {
			t.Fatalf("GET /v1/pending?approver=%s = %d %s; want 200", person, status, body)
		}
		var ids []string
		for _, row := range decode[struct {
			Pending []wirePendingEntry `json:"pending"`
		}](t, body).Pending {
			ids = append(ids, row.RecordID)
		}
		return ids
	}
	dismiss := func(t *testing.T, person string) answerRead {
		t.Helper()
		status, body := ts.ui(t, http.MethodPost, "/v1/credential-requests/"+expired+"/dismiss", map[string]any{"approver": person})
		return answerOf(t, status, body)
	}

	t.Run("a restricted member lists neither, and dismisses nothing", func(t *testing.T) {
		if got := listed(t, restrictedGuest); slices.Contains(got, pending) || slices.Contains(got, expired) {
			t.Errorf("GET /v1/pending?approver=%s = %v; want neither service login (%s, %s)", restrictedGuest, got, pending, expired)
		}
		got := dismiss(t, restrictedGuest)
		requireAnswered(t, restrictedGuest+"'s dismissal of the expired service login", got, http.StatusForbidden, "NOT_APPROVER")
		if refusal := decode[wireError](t, []byte(got.body)).Error; !strings.Contains(refusal, "restricted member") {
			t.Errorf("the refused dismissal says %q; want it to say the person is a restricted member", refusal)
		}
		if n := ts.column(t, `select count(*)::text from credential_request_dismissals where record_id=$1`, expired); !slices.Equal(n, []string{"0"}) {
			t.Fatalf("dismissals of the expired service login after the refused one = %v; want none", n)
		}
	})

	t.Run("a full member lists both, and dismisses the expired one", func(t *testing.T) {
		if got := listed(t, "bob@example.com"); !slices.Contains(got, pending) || !slices.Contains(got, expired) {
			t.Fatalf("GET /v1/pending?approver=bob@example.com = %v; want both service logins (%s, %s)", got, pending, expired)
		}
		if got := dismiss(t, "bob@example.com"); got.status != http.StatusOK {
			t.Fatalf("bob's dismissal of the expired service login = %d %s; want 200", got.status, got.body)
		}
	})
}

// TestARestrictedMemberListsAndEndsNoServicesMachineLogin: every service's machine login is listed
// to every signed-in person, and anyone signed in may end one, which ends every pod it enrolled.
// A person Dispatch calls a restricted member is no approver of a secret and sees nothing beyond
// their own projects: GET /v1/launcher-credentials lists them their own machine login and neither
// the Legion daemon's token login nor a service's legacy login, and POST
// /v1/launcher-credentials/{id}/revoke-by-approver answers them 404 NOT_FOUND for either, as for
// an id no login has, ending nothing; their own machine login they still end. While Dispatch
// answers a person a member kind this broker does not know, both routes answer that person 503
// MEMBER_KIND_UNCHECKABLE, and the revoke ends nothing: unlike a grant's named approver, the person
// would end the sessions of a service. The control: bob, a full member, lists both service logins
// and ends the daemon's.
func TestARestrictedMemberListsAndEndsNoServicesMachineLogin(t *testing.T) {
	ts, access, stand := restrictedStand(t)
	tokenLogin, _ := ts.mintServiceLauncherCredential(t)
	legacyLogin, _ := ts.mintOutstandingServiceCredential(t, testApprover)
	ownLogin, _ := ts.mintLauncherCredential(t, restrictedGuest, "guest-laptop")
	listed := func(t *testing.T, person string) []string {
		t.Helper()
		var ids []string
		for _, login := range ts.machineLogins(t, person) {
			ids = append(ids, login.CredentialID)
		}
		return ids
	}
	revoke := func(t *testing.T, person, id string) answerRead {
		t.Helper()
		status, body := ts.ui(t, http.MethodPost, "/v1/launcher-credentials/"+id+"/revoke-by-approver", map[string]any{"approver": person})
		return answerOf(t, status, body)
	}
	live := func(t *testing.T, id string) bool {
		t.Helper()
		return slices.Equal(ts.column(t, `select (revoked_at is null)::text from launcher_credentials where id=$1`, id), []string{"true"})
	}

	t.Run("a restricted member lists and ends their own machine login alone", func(t *testing.T) {
		if got := listed(t, restrictedGuest); !slices.Contains(got, ownLogin) || slices.Contains(got, tokenLogin) || slices.Contains(got, legacyLogin) {
			t.Fatalf("GET /v1/launcher-credentials?approver=%s = %v; want their own login %s and neither service login (%s, %s)", restrictedGuest, got, ownLogin, tokenLogin, legacyLogin)
		}
		for _, id := range []string{tokenLogin, legacyLogin} {
			requireAnswered(t, restrictedGuest+"'s revoke of the service login "+id, revoke(t, restrictedGuest, id), http.StatusNotFound, "NOT_FOUND")
			if !live(t, id) {
				t.Fatalf("the service login %s ended on %s's refused revoke", id, restrictedGuest)
			}
		}
		if got := revoke(t, restrictedGuest, ownLogin); got.status != http.StatusOK || live(t, ownLogin) {
			t.Fatalf("%s's revoke of their own machine login = %d %s, live afterwards %v; want 200 and it ended", restrictedGuest, got.status, got.body, live(t, ownLogin))
		}
	})

	stand.SetMemberKind(hiddenInsider, "guest-of-honour")
	access.Serve(stand)
	t.Run("while Dispatch answers a member kind the broker does not know, both routes are refused", func(t *testing.T) {
		status, body := ts.ui(t, http.MethodGet, "/v1/launcher-credentials?approver="+url.QueryEscape(hiddenInsider), nil)
		requireAnswered(t, hiddenInsider+"'s machine-login list while her kind cannot be read", answerOf(t, status, body), http.StatusServiceUnavailable, "MEMBER_KIND_UNCHECKABLE")
		requireAnswered(t, hiddenInsider+"'s revoke of the daemon's login while her kind cannot be read", revoke(t, hiddenInsider, tokenLogin), http.StatusServiceUnavailable, "MEMBER_KIND_UNCHECKABLE")
		if !live(t, tokenLogin) {
			t.Fatalf("the daemon's login %s ended on %s's refused revoke", tokenLogin, hiddenInsider)
		}
	})

	t.Run("a full member lists both service logins and ends the daemon's", func(t *testing.T) {
		if got := listed(t, "bob@example.com"); !slices.Contains(got, tokenLogin) || !slices.Contains(got, legacyLogin) {
			t.Fatalf("GET /v1/launcher-credentials?approver=bob@example.com = %v; want both service logins (%s, %s)", got, tokenLogin, legacyLogin)
		}
		if got := revoke(t, "bob@example.com", tokenLogin); got.status != http.StatusOK || decode[stateBody](t, []byte(got.body)).State != "revoked" || live(t, tokenLogin) {
			t.Fatalf("bob's revoke of the daemon's login = %d %s; want 200 revoked, and it ended", got.status, got.body)
		}
	})
}

// TestAServicePodsGrantIsOnNoRestrictedMembersList: the grant a service pod took without asking
// names no approver and no operator, so the person rule shows it on every signed-in person's Live
// grants. A person Dispatch calls a restricted member sees nothing beyond their own projects, so
// GET /v1/grants does not list it to them; bob, a full member, the control, sees it.
func TestAServicePodsGrantIsOnNoRestrictedMembersList(t *testing.T) {
	ts, _, _ := restrictedStand(t)
	credentialID, launcherKey := ts.mintOutstandingServiceCredential(t, testApprover)
	podUID := uuid.NewString()
	key := newSigningKey(t)
	status, body := ts.launcher(t, launcherKey, credentialID, http.MethodPost, "/v1/enrollments", map[string]any{
		"kind": "pod", "runtime_id": podUID, "slot": "implementer-g1", "thumbprint": thumbprintOf(t, key), "pod_token": ts.podToken(t, workerAccount, podUID),
	})
	if status != http.StatusCreated {
		t.Fatalf("enroll the service pod = %d %s; want 201", status, body)
	}
	pod := podSession{slot: "implementer-g1", enrollmentID: decode[wireEnrolled](t, body).EnrollmentID, key: key}
	granted := pod.request(t, ts, "WORKER_TOKEN")
	if granted.State != "granted" || granted.GrantID == nil {
		t.Fatalf("the service pod's request = %+v; want WORKER_TOKEN granted at once", granted)
	}
	for _, who := range []struct {
		person string
		want   bool
	}{{restrictedGuest, false}, {"bob@example.com", true}} {
		status, body := ts.ui(t, http.MethodGet, "/v1/grants?approver="+url.QueryEscape(who.person), nil)
		if status != http.StatusOK {
			t.Fatalf("GET /v1/grants?approver=%s = %d %s; want 200", who.person, status, body)
		}
		listed := slices.ContainsFunc(decode[struct {
			Grants []wireApproverGrant `json:"grants"`
		}](t, body).Grants, func(g wireApproverGrant) bool { return g.GrantID == *granted.GrantID })
		if listed != who.want {
			t.Errorf("GET /v1/grants?approver=%s holds the service pod's grant %s: %v, want %v", who.person, *granted.GrantID, listed, who.want)
		}
	}
}
