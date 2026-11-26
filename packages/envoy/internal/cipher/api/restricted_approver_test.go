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
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/logtest"
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
	t.Parallel()
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
		for what, got := range map[string]struct {
			answer answerRead
			verb   string
		}{
			restrictedGuest + "'s revoke-by-approver of the judged grant": {revoke(t, restrictedGuest, grant), "end a grant"},
			restrictedGuest + "'s dismissal of the expired request":       {dismiss(t, restrictedGuest), "dismiss"},
		} {
			requireAnswered(t, what, got.answer, http.StatusForbidden, "NOT_APPROVER")
			requireRestrictedWords(t, what, got.answer, got.verb)
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
	t.Parallel()
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

	got := deny(t, restrictedGuest)
	requireAnswered(t, restrictedGuest+"'s deny of a service's login", got, http.StatusForbidden, "NOT_APPROVER")
	requireRestrictedWords(t, restrictedGuest+"'s deny of a service's login", got, "deny")
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

// requireRestrictedWords fails t unless got, what answered, refuses in words that open by saying
// the person is a restricted member, and say they may not verb: the person refused reads why first,
// in the verb of what they did, and never the words of a refusal about whose the row is.
func requireRestrictedWords(t *testing.T, what string, got answerRead, verb string) {
	t.Helper()
	refusal := decode[wireError](t, []byte(got.body)).Error
	if !strings.HasPrefix(refusal, "a person Dispatch calls a restricted member is no approver of") || !strings.Contains(refusal, verb) {
		t.Errorf("%s says %q; want it to open by saying the person is a restricted member, and to say what they %s", what, refusal, verb)
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
	t.Parallel()
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
		requireRestrictedWords(t, restrictedGuest+"'s dismissal of the expired service login", got, "dismiss")
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

// machineLoginIDs is the ids of the machine logins GET /v1/launcher-credentials lists to person.
func (ts *testServer) machineLoginIDs(t *testing.T, person string) []string {
	t.Helper()
	var ids []string
	for _, login := range ts.machineLogins(t, person) {
		ids = append(ids, login.CredentialID)
	}
	return ids
}

// revokeMachineLoginAs is POST /v1/launcher-credentials/{id}/revoke-by-approver relayed for
// person.
func (ts *testServer) revokeMachineLoginAs(t *testing.T, person, id string) answerRead {
	t.Helper()
	status, body := ts.ui(t, http.MethodPost, "/v1/launcher-credentials/"+id+"/revoke-by-approver", map[string]any{"approver": person})
	return answerOf(t, status, body)
}

// machineLoginLive reports whether machine login id has not been revoked.
func (ts *testServer) machineLoginLive(t *testing.T, id string) bool {
	t.Helper()
	return slices.Equal(ts.column(t, `select (revoked_at is null)::text from launcher_credentials where id=$1`, id), []string{"true"})
}

// TestARestrictedMemberListsAndEndsNoServicesMachineLogin: every service's machine login is listed
// to every signed-in person, and anyone signed in may end one, which ends every pod it enrolled.
// A person Dispatch calls a restricted member is no approver of a secret and sees nothing beyond
// their own projects: GET /v1/launcher-credentials lists them their own machine login and neither
// the Legion daemon's token login nor a service's legacy login, and POST
// /v1/launcher-credentials/{id}/revoke-by-approver answers them 404 NOT_FOUND for either, as for
// an id no login has, ending nothing; their own machine login they still end. While Dispatch
// answers a person a member kind this broker does not know, both routes hold that person to their
// own logins as they hold a restricted member: the list holds neither service login, and the
// revoke of the daemon's login answers 404 NOT_FOUND and ends nothing. The control: bob, a full
// member, lists both service logins and ends the daemon's.
func TestARestrictedMemberListsAndEndsNoServicesMachineLogin(t *testing.T) {
	t.Parallel()
	ts, access, stand := restrictedStand(t)
	tokenLogin, _ := ts.mintServiceLauncherCredential(t)
	legacyLogin, _ := ts.mintOutstandingServiceCredential(t, testApprover)
	ownLogin, _ := ts.mintLauncherCredential(t, restrictedGuest, "guest-laptop")

	t.Run("a restricted member lists and ends their own machine login alone", func(t *testing.T) {
		if got := ts.machineLoginIDs(t, restrictedGuest); !slices.Contains(got, ownLogin) || slices.Contains(got, tokenLogin) || slices.Contains(got, legacyLogin) {
			t.Fatalf("GET /v1/launcher-credentials?approver=%s = %v; want their own login %s and neither service login (%s, %s)", restrictedGuest, got, ownLogin, tokenLogin, legacyLogin)
		}
		for _, id := range []string{tokenLogin, legacyLogin} {
			requireAnswered(t, restrictedGuest+"'s revoke of the service login "+id, ts.revokeMachineLoginAs(t, restrictedGuest, id), http.StatusNotFound, "NOT_FOUND")
			if !ts.machineLoginLive(t, id) {
				t.Fatalf("the service login %s ended on %s's refused revoke", id, restrictedGuest)
			}
		}
		if got := ts.revokeMachineLoginAs(t, restrictedGuest, ownLogin); got.status != http.StatusOK || ts.machineLoginLive(t, ownLogin) {
			t.Fatalf("%s's revoke of their own machine login = %d %s, live afterwards %v; want 200 and it ended", restrictedGuest, got.status, got.body, ts.machineLoginLive(t, ownLogin))
		}
	})

	stand.SetMemberKind(hiddenInsider, "guest-of-honour")
	access.Serve(stand)
	t.Run("while Dispatch answers a member kind the broker does not know, both routes hold the person to their own logins", func(t *testing.T) {
		if got := ts.machineLoginIDs(t, hiddenInsider); slices.Contains(got, tokenLogin) || slices.Contains(got, legacyLogin) {
			t.Fatalf("GET /v1/launcher-credentials?approver=%s while her kind cannot be read = %v; want neither service login (%s, %s)", hiddenInsider, got, tokenLogin, legacyLogin)
		}
		requireAnswered(t, hiddenInsider+"'s revoke of the daemon's login while her kind cannot be read", ts.revokeMachineLoginAs(t, hiddenInsider, tokenLogin), http.StatusNotFound, "NOT_FOUND")
		if !ts.machineLoginLive(t, tokenLogin) {
			t.Fatalf("the daemon's login %s ended on %s's refused revoke", tokenLogin, hiddenInsider)
		}
	})

	t.Run("a full member lists both service logins and ends the daemon's", func(t *testing.T) {
		if got := ts.machineLoginIDs(t, "bob@example.com"); !slices.Contains(got, tokenLogin) || !slices.Contains(got, legacyLogin) {
			t.Fatalf("GET /v1/launcher-credentials?approver=bob@example.com = %v; want both service logins (%s, %s)", got, tokenLogin, legacyLogin)
		}
		if got := ts.revokeMachineLoginAs(t, "bob@example.com", tokenLogin); got.status != http.StatusOK || decode[stateBody](t, []byte(got.body)).State != "revoked" || ts.machineLoginLive(t, tokenLogin) {
			t.Fatalf("bob's revoke of the daemon's login = %d %s; want 200 revoked, and it ended", got.status, got.body)
		}
	})
}

// TestAFullMemberEndsTheirOwnMachineLoginWhileTheirKindCannotBeRead: a person ends a lost
// machine's login on Dispatch's machine-login page whatever Dispatch answers of them. While the
// broker cannot read bob's member kind — Dispatch answers a kind this broker does not know, is
// down, or refuses the broker's own account — the machine-login routes hold him to his own logins,
// as they hold a restricted member: GET /v1/launcher-credentials lists his own login and not the
// Legion daemon's, his revoke of his own login ends it, and his revoke of the daemon's answers 404
// NOT_FOUND, ending nothing. Over his nine calls the broker writes the WARN line that says so
// (api.MachineLoginsOwnOnlyMessage) once, since it writes it at most once per person per minute.
// The control: once Dispatch answers him a full member, his list holds the daemon's login again.
func TestAFullMemberEndsTheirOwnMachineLoginWhileTheirKindCannotBeRead(t *testing.T) {
	// Not parallel: it captures the process's default logger, whose records are every test's that logs meanwhile (logtest.SetDefault, logtest.Swap, slog.SetDefault).
	const bob = "bob@example.com"
	ts, access, stand := restrictedStand(t)
	tokenLogin, _ := ts.mintServiceLauncherCredential(t)
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	for _, fault := range []struct {
		name, host string
		set        func(*projectaccesstest.Server)
	}{
		{"Dispatch answers a member kind this broker does not know", "bob-laptop-1", func(s *projectaccesstest.Server) { s.SetMemberKind(bob, "guest-of-honour") }},
		{"Dispatch is down", "bob-laptop-2", func(s *projectaccesstest.Server) { s.SetDown(true) }},
		{"Dispatch refuses the broker's own account", "bob-laptop-3", func(s *projectaccesstest.Server) {
			s.SetAnswer(http.StatusForbidden, `{"code":"ACCOUNT_NOT_LISTED","error":"this account may not ask"}`)
		}},
	} {
		t.Run(fault.name, func(t *testing.T) {
			access.Serve(stand)
			own, _ := ts.mintLauncherCredential(t, bob, fault.host)
			faulty := projectaccesstest.NewServer(t, nil, map[string]string{bob: projectaccess.MemberFull})
			fault.set(faulty)
			access.Serve(faulty)
			if got := ts.machineLoginIDs(t, bob); !slices.Contains(got, own) || slices.Contains(got, tokenLogin) {
				t.Fatalf("GET /v1/launcher-credentials?approver=%s = %v; want his own login %s and not the daemon's %s", bob, got, own, tokenLogin)
			}
			requireAnswered(t, bob+"'s revoke of the daemon's login", ts.revokeMachineLoginAs(t, bob, tokenLogin), http.StatusNotFound, "NOT_FOUND")
			if !ts.machineLoginLive(t, tokenLogin) {
				t.Fatalf("the daemon's login %s ended on %s's refused revoke", tokenLogin, bob)
			}
			if got := ts.revokeMachineLoginAs(t, bob, own); got.status != http.StatusOK || decode[stateBody](t, []byte(got.body)).State != "revoked" || ts.machineLoginLive(t, own) {
				t.Fatalf("%s's revoke of his own machine login = %d %s; want 200 revoked, and it ended", bob, got.status, got.body)
			}
			if faulty.Calls() == 0 {
				t.Fatal("the faulty Dispatch was never asked: the row is blind")
			}
		})
	}
	var held int
	for _, rec := range capture.Records() {
		if rec.Message == api.MachineLoginsOwnOnlyMessage && rec.Level == slog.LevelWarn {
			held++
		}
	}
	if held != 1 {
		t.Fatalf("the log holds %d WARN lines of %q for his nine calls; want one", held, api.MachineLoginsOwnOnlyMessage)
	}

	access.Serve(stand)
	t.Run("once Dispatch answers, his list holds the daemon's login", func(t *testing.T) {
		if got := ts.machineLoginIDs(t, bob); !slices.Contains(got, tokenLogin) {
			t.Fatalf("GET /v1/launcher-credentials?approver=%s once Dispatch answers = %v; want the daemon's login %s", bob, got, tokenLogin)
		}
	})
}

// TestTheOwnLoginsLineIsWrittenOncePerPersonPerMinute: while the broker cannot read a person's
// member kind, each machine-login call holds them to their own logins, and the WARN line that says
// so (api.MachineLoginsOwnOnlyMessage) is written once per person per minute, however often they
// call: Dispatch's page reads the list each time it opens. While Dispatch is down, bob lists his
// machine logins three times and carol once: one line names bob and one names carol.
func TestTheOwnLoginsLineIsWrittenOncePerPersonPerMinute(t *testing.T) {
	// Not parallel: it captures the process's default logger, whose records are every test's that logs meanwhile (logtest.SetDefault, logtest.Swap, slog.SetDefault).
	const bob = "bob@example.com"
	ts, access, _ := restrictedStand(t)
	down := projectaccesstest.NewServer(t, nil, nil)
	down.SetDown(true)
	access.Serve(down)
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	for range 3 {
		ts.machineLoginIDs(t, bob)
	}
	ts.machineLoginIDs(t, hiddenInsider)
	if down.Calls() == 0 {
		t.Fatal("the down Dispatch was never asked: the row is blind")
	}
	held := map[string]int{}
	for _, rec := range capture.Records() {
		if rec.Message != api.MachineLoginsOwnOnlyMessage || rec.Level != slog.LevelWarn {
			continue
		}
		rec.Attrs(func(a slog.Attr) bool {
			if a.Key == "person" {
				held[a.Value.String()]++
			}
			return true
		})
	}
	if want := map[string]int{bob: 1, hiddenInsider: 1}; !maps.Equal(held, want) {
		t.Fatalf("the WARN lines of %q name %v; want %v", api.MachineLoginsOwnOnlyMessage, held, want)
	}
}

// TestARevokeWhoseCallerHungUpWritesNothingAndNoErrorLine: Dispatch's server relays the revoke of
// a grant by the person it names as its approver, and hangs up while the broker asks Dispatch what
// kind of member that person is. A caller that hung up says nothing of Dispatch, so the revoke
// does not go ahead as it would while Dispatch does not answer: it ends nothing, writes no ERROR
// line, and is counted cancelled (answer.Recorder.NoteCallerEnded), as every route counts a caller
// that hung up.
func TestARevokeWhoseCallerHungUpWritesNothingAndNoErrorLine(t *testing.T) {
	// Not parallel: it captures the process's default logger, whose records are every test's that logs meanwhile (logtest.SetDefault, logtest.Swap, slog.SetDefault).
	ts := newTestServer(t)
	box, boxKey := ts.newSessionEnrollment(t, "hang-up-box-"+t.Name(), "operator@example.com")
	grant := ts.approveAs(t, ts.requestAs(t, boxKey, box, "DEEL_API_KEY"), testApprover)
	before := ts.writtenRows(t)
	ctx, hangUp := context.WithCancel(t.Context())
	// Dispatch has the broker's question when its own caller goes: the broker's read fails with
	// the request's cancellation, as it does when Dispatch's server hangs up on the route.
	hanging := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		hangUp()
		<-r.Context().Done()
	}))
	t.Cleanup(hanging.Close)
	ts.machine.Projects = projectaccess.New(hanging.URL, projectaccesstest.NewServer(t, nil, nil).Account, nil)
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)

	request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/grants/"+grant+"/revoke-by-approver", strings.NewReader(`{"approver":"`+testApprover+`"}`))
	request.Header.Set("Authorization", "Bearer "+ts.uiToken)
	request.Header.Set("Content-Type", "application/json")
	ts.handler.ServeHTTP(httptest.NewRecorder(), request)

	for _, rec := range capture.Records() {
		if rec.Level >= slog.LevelError {
			t.Errorf("the revoke whose caller hung up wrote the ERROR line %q; want none", rec.Message)
		}
	}
	if got := ts.column(t, `select (revoked_at is null)::text from grants where id=$1`, grant); !slices.Equal(got, []string{"true"}) {
		t.Fatalf("grant %s live after the revoke whose caller hung up = %v; want true", grant, got)
	}
	if after := ts.writtenRows(t); !slices.Equal(after, before) {
		t.Fatalf("rows written = %v after the revoke whose caller hung up, want %v as before", after, before)
	}
	scrape := httptest.NewRecorder()
	ts.registry.Handler().ServeHTTP(scrape, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if want := `broker_answers_total{outcome="cancelled"`; !strings.Contains(scrape.Body.String(), want) {
		t.Fatalf("the revoke whose caller hung up was not counted cancelled:\n%s", scrape.Body.String())
	}
}

// hangsUpAfterKinds is a projectaccess.Access whose member-kind read is answered, and whose
// caller hangs up just after: hangUp runs once the answer is in hand, before the read returns.
// Every other question it answers as answers does.
type hangsUpAfterKinds struct {
	answers projectaccess.Access
	hangUp  func()
}

func (h hangsUpAfterKinds) Access(ctx context.Context, project, person string) (projectaccess.Answer, error) {
	return h.answers.Access(ctx, project, person)
}

func (h hangsUpAfterKinds) States(ctx context.Context, projects []string) (map[string]projectaccess.Answer, error) {
	return h.answers.States(ctx, projects)
}

func (h hangsUpAfterKinds) MemberKind(ctx context.Context, person string) (string, error) {
	kind, err := h.answers.MemberKind(ctx, person)
	h.hangUp()
	return kind, err
}

// TestARevokeWhoseCallerHungUpAfterTheMemberKindReadWritesNoErrorLine: Dispatch's server relays
// the revoke of a grant by the person it names as its approver, Dispatch answers that the person
// is a full member, and Dispatch's server hangs up just after, before the revoke's transaction
// begins. A caller that hung up says nothing of the broker or of Dispatch, so the revoke writes no
// ERROR line and is counted cancelled (answer.Recorder.NoteCallerEnded), as a hang-up during the
// read is; it ends nothing, since its transaction never began.
func TestARevokeWhoseCallerHungUpAfterTheMemberKindReadWritesNoErrorLine(t *testing.T) {
	// Not parallel: it captures the process's default logger, whose records are every test's that logs meanwhile (logtest.SetDefault, logtest.Swap, slog.SetDefault).
	ts := newTestServer(t)
	box, boxKey := ts.newSessionEnrollment(t, "hang-up-after-box-"+t.Name(), "operator@example.com")
	grant := ts.approveAs(t, ts.requestAs(t, boxKey, box, "DEEL_API_KEY"), testApprover)
	before := ts.writtenRows(t)
	ctx, hangUp := context.WithCancel(t.Context())
	stand := projectaccesstest.NewServer(t, nil, map[string]string{testApprover: projectaccess.MemberFull})
	ts.machine.Projects = hangsUpAfterKinds{answers: projectaccess.New(stand.URL, stand.Account, nil), hangUp: hangUp}
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)

	request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/grants/"+grant+"/revoke-by-approver", strings.NewReader(`{"approver":"`+testApprover+`"}`))
	request.Header.Set("Authorization", "Bearer "+ts.uiToken)
	request.Header.Set("Content-Type", "application/json")
	ts.handler.ServeHTTP(httptest.NewRecorder(), request)

	if stand.Calls() == 0 {
		t.Fatal("the member kind was never read: the row is blind to a hang-up after it")
	}
	for _, rec := range capture.Records() {
		if rec.Level >= slog.LevelError {
			t.Errorf("the revoke whose caller hung up after the member-kind read wrote the ERROR line %q; want none", rec.Message)
		}
	}
	if got := ts.column(t, `select (revoked_at is null)::text from grants where id=$1`, grant); !slices.Equal(got, []string{"true"}) {
		t.Fatalf("grant %s live after the revoke whose caller hung up = %v; want true", grant, got)
	}
	if after := ts.writtenRows(t); !slices.Equal(after, before) {
		t.Fatalf("rows written = %v after the revoke whose caller hung up, want %v as before", after, before)
	}
	scrape := httptest.NewRecorder()
	ts.registry.Handler().ServeHTTP(scrape, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if want := `broker_answers_total{outcome="cancelled"`; !strings.Contains(scrape.Body.String(), want) {
		t.Fatalf("the revoke whose caller hung up after the member-kind read was not counted cancelled:\n%s", scrape.Body.String())
	}
}

// stalledKinds is a projectaccess.Access whose member-kind reads, once stall is set, Dispatch never
// answers: each waits until its caller's context ends, and records how long that context had left.
// A read whose context has no deadline would wait as long as its caller, so it is answered at once
// with an error and counted as unbounded, which the row reads as the fault. Until stall is set,
// and for every other question, it answers as answers does.
type stalledKinds struct {
	answers   projectaccess.Access
	stall     atomic.Bool
	mu        sync.Mutex
	unbounded int
	bounds    []time.Duration
}

func (s *stalledKinds) Access(ctx context.Context, project, person string) (projectaccess.Answer, error) {
	return s.answers.Access(ctx, project, person)
}

func (s *stalledKinds) States(ctx context.Context, projects []string) (map[string]projectaccess.Answer, error) {
	return s.answers.States(ctx, projects)
}

func (s *stalledKinds) MemberKind(ctx context.Context, person string) (string, error) {
	if !s.stall.Load() {
		return s.answers.MemberKind(ctx, person)
	}
	deadline, bounded := ctx.Deadline()
	s.mu.Lock()
	if !bounded {
		s.unbounded++
		s.mu.Unlock()
		return "", errors.New("stalledKinds: a member-kind read with no deadline")
	}
	s.bounds = append(s.bounds, time.Until(deadline))
	s.mu.Unlock()
	<-ctx.Done()
	return "", ctx.Err()
}

// TestTheMachineLoginRoutesReadTheMemberKindWithinTheRelaysWait: Dispatch waits 5 seconds for the
// broker's answer to a route it relays, and one read of Dispatch may take longer
// (projectaccess.RelayedReadBound's reason), so the member-kind read of each machine-login route
// has a bound of its own below that wait. A read Dispatch does not answer within it is one
// Dispatch did not answer: bob, a full member, is listed his own login and not the Legion daemon's,
// and ends his own login.
func TestTheMachineLoginRoutesReadTheMemberKindWithinTheRelaysWait(t *testing.T) {
	t.Parallel()
	const bob = "bob@example.com"
	const dispatchRelayWait = 5 * time.Second
	stand := projectaccesstest.NewServer(t, nil, map[string]string{bob: projectaccess.MemberFull})
	stalled := &stalledKinds{answers: projectaccess.New(stand.URL, stand.Account, nil)}
	ts := newTestServerWith(t, func(d *api.Deps) { d.Enroll.Projects = stalled })
	tokenLogin, _ := ts.mintServiceLauncherCredential(t)
	own, _ := ts.mintLauncherCredential(t, bob, "bob-laptop")
	stalled.stall.Store(true)

	if got := ts.machineLoginIDs(t, bob); !slices.Contains(got, own) || slices.Contains(got, tokenLogin) {
		t.Fatalf("GET /v1/launcher-credentials?approver=%s while Dispatch does not answer = %v; want his own login %s and not the daemon's %s", bob, got, own, tokenLogin)
	}
	if got := ts.revokeMachineLoginAs(t, bob, own); got.status != http.StatusOK || ts.machineLoginLive(t, own) {
		t.Fatalf("%s's revoke of his own machine login while Dispatch does not answer = %d %s; want 200, and it ended", bob, got.status, got.body)
	}
	stalled.mu.Lock()
	defer stalled.mu.Unlock()
	if stalled.unbounded != 0 || len(stalled.bounds) != 2 {
		t.Fatalf("the two routes asked %d member-kind reads with no deadline and %d with one; want two, each bounded", stalled.unbounded, len(stalled.bounds))
	}
	for _, bound := range stalled.bounds {
		if bound >= dispatchRelayWait {
			t.Fatalf("a machine-login route read a member kind with %s to run; want less than the %s Dispatch waits for the route", bound, dispatchRelayWait)
		}
	}
}

// TestAServicePodsGrantIsOnNoRestrictedMembersList: the grant a service pod took without asking
// names no approver and no operator, so the person rule shows it on every signed-in person's Live
// grants. A person Dispatch calls a restricted member sees nothing beyond their own projects, so
// GET /v1/grants does not list it to them; bob, a full member, the control, sees it.
func TestAServicePodsGrantIsOnNoRestrictedMembersList(t *testing.T) {
	t.Parallel()
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
