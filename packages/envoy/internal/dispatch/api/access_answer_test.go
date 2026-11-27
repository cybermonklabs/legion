package api

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestTheAccessAnswerAdmitsOnlyItsAccounts (K3): only a service account DISPATCH_ACCESS_ACCOUNTS
// names reads the membership answer; another verified account, a person and the shared token are
// 403 ACCESS_ACCOUNT_REQUIRED on both routes, and a caller with no credential is 401 NO_IDENTITY.
func TestTheAccessAnswerAdmitsOnlyItsAccounts(t *testing.T) {
	issuer := newServiceTokenIssuer(t)
	handler, database, _ := newTestServer(t, testServerOptions{oidc: issuer.verifier(t), accessAccounts: []string{listedServiceAccount}})
	seedPeople(t, database, accessCarol)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "ACC", "name": "Access"}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create ACC: %d %s", response.Code, response.Body.String())
	}
	token := func(subject string) string {
		return issuer.Mint(t, issuer.signing, issuer.Claims(subject, serviceTokenAudience))
	}
	for _, path := range []string{"/api/v1/access/projects/ACC?person=alice", "/api/v1/access/person?person=alice"} {
		if response := agentRequest(t, handler, http.MethodGet, path, nil, token(listedServiceAccount)); response.Code != http.StatusOK {
			t.Errorf("%s as %s = %d %s, want 200", path, listedServiceAccount, response.Code, response.Body.String())
		}
		wantRefusal(t, agentRequest(t, handler, http.MethodGet, path, nil, token(unlistedServiceAccount)),
			http.StatusForbidden, "ACCESS_ACCOUNT_REQUIRED", unlistedServiceAccount, "GET "+path)
		wantRefusal(t, agentRequest(t, handler, http.MethodGet, path, nil, "agent-token"),
			http.StatusForbidden, "ACCESS_ACCOUNT_REQUIRED", "the shared token", "GET "+path)
		wantRefusal(t, dispatchRequest(t, handler, http.MethodGet, path, nil, "alice"),
			http.StatusForbidden, "ACCESS_ACCOUNT_REQUIRED", "alice", "GET "+path)
		wantRefusal(t, dispatchRequest(t, handler, http.MethodGet, path, nil, ""),
			http.StatusUnauthorized, "NO_IDENTITY", "a caller with no credential", "GET "+path)
	}
	wantRefusal(t, agentRequest(t, handler, http.MethodGet, "/api/v1/access/person", nil, token(listedServiceAccount)),
		http.StatusBadRequest, "INVALID_QUERY", listedServiceAccount, "GET /api/v1/access/person with no person")
	wantRefusal(t, agentRequest(t, handler, http.MethodGet, "/api/v1/access/projects/NOPE?person=alice", nil, token(listedServiceAccount)),
		http.StatusNotFound, "PROJECT_NOT_FOUND", listedServiceAccount, "GET an unknown project's answer")
}

// TestTheAccessAnswerSaysWhetherTheAskingCredentialsAccountHoldsTheAddress: a membership
// question naming a person also describes the credential being checked, and the answer carries
// holder. It is the reviewer's broker-token case at the surface the broker asks: Google account A
// held carol's address and signed in at the broker, never at Dispatch, so Dispatch recorded no
// account for carol; then B, which Google gave the address to, signs in to Dispatch. From that
// sign-in on, a question naming A answers holder false, and the project route answers visible
// false for it, while B's answers holder true and sees the project; before it, while Dispatch
// records no account, A is the holder as far as Dispatch knows. An account of another issuer is
// never the holder of an address Dispatch records an account for.
//
// A credential recording no account (account=none) is held to when it was made
// (credential_created_at), against when Dispatch recorded the address's account: with no recording
// time, every one is the holder (the deploy control); once the time is set, one with no creation
// time is not (fail closed), nor one made before, nor one made 1 minute after (inside
// brokerCredentialMargin, the two clocks apart), while one made 10 minutes after is. A question
// that names no account, the ?person= every earlier broker asks, answers as it always did, before
// and after the recording: no holder, and the project visible (the model's C1, R14-3). Half an
// account, account=none beside an account, a creation time that is no whole Unix second, a
// creation time that describes no account, and an account or a creation time with no person are
// 400 INVALID_QUERY.
//
// Controls: accountHolder answering true whatever people records (the account check removed): the
// question naming A after B's sign-in answers holder true and visible true; accountHolder without
// the margin: the credential made 1 minute after the recording answers holder true; every question
// read as describing a credential (describes forced true): the ?person= question answers holder
// false and visible false once the address's account is recorded.
func TestTheAccessAnswerSaysWhetherTheAskingCredentialsAccountHoldsTheAddress(t *testing.T) {
	const google = "https://accounts.google.com"
	issuer := newServiceTokenIssuer(t)
	handler, database, _ := newTestServer(t, testServerOptions{oidc: issuer.verifier(t), accessAccounts: []string{listedServiceAccount}, signInIssuer: google})
	seedPeople(t, database, accessCarol)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "ACC", "name": "Access"}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create ACC: %d %s", response.Code, response.Body.String())
	}
	bearer := issuer.Mint(t, issuer.signing, issuer.Claims(listedServiceAccount, serviceTokenAudience))
	ask := func(path string) (int, model.AccessAnswer, model.PersonAnswer) {
		t.Helper()
		response := agentRequest(t, handler, http.MethodGet, path, nil, bearer)
		if response.Code != http.StatusOK {
			return response.Code, model.AccessAnswer{}, model.PersonAnswer{}
		}
		if strings.HasPrefix(path, "/api/v1/access/person") {
			return response.Code, model.AccessAnswer{}, decodeBody[model.PersonAnswer](t, response)
		}
		return response.Code, decodeBody[model.AccessAnswer](t, response), model.PersonAnswer{}
	}
	account := func(issuer, subject string) string {
		return "account_issuer=" + url.QueryEscape(issuer) + "&account_subject=" + url.QueryEscape(subject)
	}
	wantHolder := func(when, path string, want bool) {
		t.Helper()
		code, project, person := ask(path)
		holder := project.Holder
		if strings.HasPrefix(path, "/api/v1/access/person") {
			holder = person.Holder
		}
		if code != http.StatusOK || holder == nil || *holder != want {
			got := "absent"
			if holder != nil {
				got = strconv.FormatBool(*holder)
			}
			t.Errorf("%s, GET %s = %d holder %s, want 200 holder %t", when, path, code, got, want)
			return
		}
		if strings.HasPrefix(path, "/api/v1/access/projects") && project.Visible != want {
			t.Errorf("%s, GET %s answers visible %t, want %t: a credential whose account does not hold the address sees nothing", when, path, project.Visible, want)
		}
	}
	// wantAsBefore is a question that says nothing of the credential: 200, no holder, and the
	// project visible as its rule reads it (ACC is public).
	wantAsBefore := func(when, path string) {
		t.Helper()
		code, project, person := ask(path)
		holder := project.Holder
		if strings.HasPrefix(path, "/api/v1/access/person") {
			holder = person.Holder
		}
		if code != http.StatusOK || holder != nil {
			got := "absent"
			if holder != nil {
				got = strconv.FormatBool(*holder)
			}
			t.Errorf("%s, GET %s = %d holder %s, want 200 and no holder, as before", when, path, code, got)
			return
		}
		if strings.HasPrefix(path, "/api/v1/access/projects") && !project.Visible {
			t.Errorf("%s, GET %s answers visible false, want true, as before", when, path)
		}
	}

	carol := url.QueryEscape(accessCarol)
	wantHolder("while Dispatch records no account for carol", "/api/v1/access/projects/ACC?person="+carol+"&"+account(google, "account-a"), true)
	wantHolder("while Dispatch records no account for carol", "/api/v1/access/person?person="+carol+"&"+account(google, "account-a"), true)
	if _, err := database.Pool.Exec(context.Background(), `update people set sub = 'account-b' where email = $1`, accessCarol); err != nil {
		t.Fatalf("record B's sign-in: %v", err)
	}
	// No recording time yet, as a row from before the column holds: the account rows below are
	// held to the account alone, and the time rows after them set one.
	storetest.SetRecordingTime(t, database, accessCarol, nil)
	for _, route := range []string{"/api/v1/access/projects/ACC", "/api/v1/access/person"} {
		wantHolder("after B signed in to Dispatch, asking for A", route+"?person="+carol+"&"+account(google, "account-a"), false)
		wantHolder("after B signed in to Dispatch, asking for A under the issuer's other spelling", route+"?person="+carol+"&"+account(google+"/", "account-a"), false)
		wantHolder("after B signed in to Dispatch, asking for B", route+"?person="+carol+"&"+account(google, "account-b"), true)
		wantHolder("after B signed in to Dispatch, asking for B's sub under another issuer", route+"?person="+carol+"&"+account("https://issuer.example", "account-b"), false)
		wantAsBefore("after B signed in, a question naming no account, with no recording time", route+"?person="+carol)
		wantHolder("after B signed in, account=none, with no recording time", route+"?person="+carol+"&account=none", true)
	}
	var recordedAt time.Time
	if err := database.Pool.QueryRow(context.Background(), `select date_trunc('second', now() - interval '20 minutes')`).Scan(&recordedAt); err != nil {
		t.Fatal(err)
	}
	storetest.SetRecordingTime(t, database, accessCarol, recordedAt)
	created := func(at time.Time) string {
		return "&credential_created_at=" + strconv.FormatInt(at.Unix(), 10)
	}
	for _, route := range []string{"/api/v1/access/projects/ACC", "/api/v1/access/person"} {
		wantAsBefore("a question naming no account, once recorded", route+"?person="+carol)
		wantHolder("account=none with no creation time, once recorded", route+"?person="+carol+"&account=none", false)
		wantHolder("a credential recording no account made before the recording", route+"?person="+carol+"&account=none"+created(recordedAt.Add(-time.Hour)), false)
		wantHolder("a credential recording no account made 1 minute after the recording", route+"?person="+carol+"&account=none"+created(recordedAt.Add(time.Minute)), false)
		wantHolder("a credential recording no account made 10 minutes after the recording", route+"?person="+carol+"&account=none"+created(recordedAt.Add(10*time.Minute)), true)
		wantHolder("B's own account, whatever its time", route+"?person="+carol+"&"+account(google, "account-b"), true)
	}
	for _, path := range []string{
		"/api/v1/access/person?person=" + carol + "&account_issuer=" + url.QueryEscape(google),
		"/api/v1/access/projects/ACC?person=" + carol + "&account_subject=account-a",
		"/api/v1/access/projects/ACC?" + account(google, "account-a"),
		"/api/v1/access/person?person=" + carol + "&account=none&" + account(google, "account-a"),
		"/api/v1/access/person?person=" + carol + "&account=some",
		"/api/v1/access/person?person=" + carol + "&credential_created_at=yesterday",
		"/api/v1/access/projects/ACC?account=none",
		"/api/v1/access/projects/ACC" + strings.Replace(created(recordedAt), "&", "?", 1),
		"/api/v1/access/person?person=" + carol + created(recordedAt.Add(10*time.Minute)),
		"/api/v1/access/projects/ACC?person=" + carol + created(recordedAt.Add(10*time.Minute)),
	} {
		wantRefusal(t, agentRequest(t, handler, http.MethodGet, path, nil, bearer), http.StatusBadRequest, "INVALID_QUERY", listedServiceAccount, "GET "+path)
	}
}

// TestTheAccessAnswerReadsAnEmptyAccountAsTheAdmissionDoes: the membership answer reads whether
// the address records an account as the admission does, `sub is not null`, so a row holding the
// empty string, which the database refuses (people_sub_not_empty) and the test can store only with
// that check dropped, records an account no credential names: a question naming account A answers
// holder false, as the admission refuses A's credential there (brokerAccountReplaced).
//
// Control: accountHolder reading an empty sub as no account (`case sub == ""`): the question
// naming A answers holder true.
func TestTheAccessAnswerReadsAnEmptyAccountAsTheAdmissionDoes(t *testing.T) {
	const google = "https://accounts.google.com"
	issuer := newServiceTokenIssuer(t)
	handler, database, _ := newTestServer(t, testServerOptions{oidc: issuer.verifier(t), accessAccounts: []string{listedServiceAccount}, signInIssuer: google})
	seedPeople(t, database, accessCarol)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "ACC", "name": "Access"}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create ACC: %d %s", response.Code, response.Body.String())
	}
	for _, statement := range []string{
		`alter table people drop constraint people_sub_not_empty`,
		`update people set sub = '' where email = '` + accessCarol + `'`,
	} {
		if _, err := database.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	bearer := issuer.Mint(t, issuer.signing, issuer.Claims(listedServiceAccount, serviceTokenAudience))
	path := "/api/v1/access/person?person=" + url.QueryEscape(accessCarol) + "&account_issuer=" + url.QueryEscape(google) + "&account_subject=account-a"
	response := agentRequest(t, handler, http.MethodGet, path, nil, bearer)
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s = %d %s, want 200", path, response.Code, response.Body.String())
	}
	if holder := decodeBody[model.PersonAnswer](t, response).Holder; holder == nil || *holder {
		got := "absent"
		if holder != nil {
			got = strconv.FormatBool(*holder)
		}
		t.Errorf("with carol's row holding the empty string as its account, GET %s answers holder %s, want false: the admission reads that row as recording an account, and A's credential is not it", path, got)
	}
}
