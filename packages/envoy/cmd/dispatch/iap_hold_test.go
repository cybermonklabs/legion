package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/identity/directorytest"
	"github.com/sjawhar/envoy/internal/dispatch/routes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// The IAP hold through the real router (R9, c-iap-directory): Dispatch under DISPATCH_IDENTITY=iap
// as main builds it (requestIdentityFor, buildAppContext), on Postgres, naming its person from
// assertions signed by a key set served here in IAP's place, and checking people's Workspace
// accounts through the Directory reader against Google hosts served here (directorytest).

// iapPerson is the person iapAssertion's assertions name, lowercased as Dispatch records them.
const iapPerson = "a.person@example.com"

// iapHoldRig is one IAP Dispatch: its router, its database and people store, the Membership the
// router's Standing and the membership pass share, and the fake Google its Directory reader reaches.
type iapHoldRig struct {
	t          *testing.T
	keys       *oidctest.Issuer
	signing    *oidctest.Key
	google     *directorytest.Google
	database   *store.Store
	people     testPeople
	membership *identity.Membership
	handler    http.Handler
}

func newIAPHoldRig(t *testing.T) *iapHoldRig {
	t.Helper()
	keys, signing := iapKeySet(t)
	r := &iapHoldRig{t: t, keys: keys, signing: signing, google: directorytest.ServeGoogle(t), database: storetest.Open(t)}
	boot := iapBoot(t, keys, r.google, nil)
	r.people = testPeople{PgPeopleStore: store.NewPgPeopleStore(r.database.Pool, testSigningKey, nil), pool: r.database.Pool}
	built := routes.AppContextOptions{
		Lifetime: t.Context(), SigningKey: testSigningKey, People: r.people, Store: r.database,
		Sessions: store.NewPgSessionStore(r.database.Pool),
	}
	rule, err := signInRule(boot, accessLister(built.Store))
	if err != nil {
		t.Fatal(err)
	}
	built.Identity, r.membership, err = requestIdentityFor(t.Context(), boot, built.SigningKey, built.People, built.Sessions, built.SignIn, rule)
	if err != nil {
		t.Fatalf("build the IAP identity: %v", err)
	}
	app, err := buildAppContext(boot, rule, withDocumentService(t, boot, built), r.membership)
	if err != nil {
		t.Fatalf("build the router: %v", err)
	}
	handler, background := routes.New(app)
	t.Cleanup(background.Wait)
	r.handler = handler
	return r
}

// browser is a request from iapPerson's browser through IAP: a fresh assertion IAP signed, and,
// for a write, the dashboard's own origin.
func (r *iapHoldRig) browser(method, path, body string) *httptest.ResponseRecorder {
	r.t.Helper()
	request := httptest.NewRequest(method, "http://dispatch.test"+path, strings.NewReader(body))
	request.Header.Set(identity.IAPAssertionHeader, iapAssertion(r.t, r.keys, r.signing, testIAPAudience, "example.com"))
	if method != http.MethodGet {
		request.Header.Set("Origin", "http://dispatch.test")
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	r.handler.ServeHTTP(response, request)
	return response
}

// mintToken mints a dsp_ token on iapPerson's browser, as the agent tokens page does under IAP.
func (r *iapHoldRig) mintToken() string {
	r.t.Helper()
	minted := r.browser(http.MethodPost, "/api/v1/me/agent-tokens", `{"name":"an agent of mine"}`)
	var token struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(minted.Body.Bytes(), &token); minted.Code != http.StatusCreated || err != nil || token.Token == "" {
		r.t.Fatalf("POST /api/v1/me/agent-tokens through IAP answered %d %s, want 201 with a token", minted.Code, minted.Body.String())
	}
	return token.Token
}

// use is GET /api/v1/whoami with token, as an agent sends it.
func (r *iapHoldRig) use(token string) *httptest.ResponseRecorder {
	return bearerRequest(r.handler, http.MethodGet, "/api/v1/whoami", "", token)
}

// directory makes the fake Directory answer iapPerson's own account, suspended as suspended says.
func (r *iapHoldRig) directory(suspended bool) {
	r.google.SetUser(iapPerson, directorytest.User{ID: "u-person", PrimaryEmail: iapPerson, Suspended: suspended})
}

// pass makes iapPerson's row due a Directory check, dating its Directory columns two hours back,
// and runs one membership pass over the router's Membership, as the maintenance sweep runs it.
func (r *iapHoldRig) pass() {
	r.t.Helper()
	if r.membership == nil {
		r.t.Fatal("requestIdentityFor built no Membership under IAP identity, so no membership pass runs")
	}
	r.exec(`update people set directory_confirmed_at = directory_confirmed_at - interval '2 hours',
		directory_checked_at = directory_checked_at - interval '2 hours', not_held_until = not_held_until - interval '2 hours',
		held_since = held_since - interval '2 hours' where email = $1`, iapPerson)
	if err := (identity.MembershipPass{Membership: r.membership, People: r.people.PgPeopleStore}).Run(context.Background()); err != nil {
		r.t.Fatalf("the membership pass: %v", err)
	}
}

func (r *iapHoldRig) exec(statement string, args ...any) {
	r.t.Helper()
	if _, err := r.database.Pool.Exec(context.Background(), statement, args...); err != nil {
		r.t.Fatalf("%s: %v", statement, err)
	}
}

// heldSince is iapPerson's held_since, nil for none.
func (r *iapHoldRig) heldSince() *time.Time {
	r.t.Helper()
	var at *time.Time
	if err := r.database.Pool.QueryRow(context.Background(), `select held_since from people where email = $1`, iapPerson).Scan(&at); err != nil {
		r.t.Fatal(err)
	}
	return at
}

// wantAnswer fails the test unless response is status with code (none for a 200), saying text
// when it is not empty.
func wantAnswer(t *testing.T, what string, response *httptest.ResponseRecorder, status int, code, text string) {
	t.Helper()
	var refusal struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(response.Body.Bytes(), &refusal)
	if response.Code != status || (status != http.StatusOK && refusal.Code != code) || (text != "" && refusal.Error != text) {
		t.Fatalf("%s answered %d %s, want %d %s %q", what, response.Code, strings.TrimSpace(response.Body.String()), status, code, text)
	}
}

// TestIAPReleaseNeedsNoAssertion (c-iap-directory): under IAP a held person's dsp_ token is served
// again after the membership pass's Active outcome, with no request from the person. The person
// mints a token through IAP, the Directory answers their account active, then suspended, then
// active again; between the last two passes the person sends nothing.
//
// Control: a pass that skips held rows; the hold stays, and the test fails naming it.
func TestIAPReleaseNeedsNoAssertion(t *testing.T) {
	r := newIAPHoldRig(t)
	r.directory(false)
	token := r.mintToken()
	wantAnswer(t, "the person's token, their account active", r.use(token), http.StatusOK, "", "")

	r.directory(true)
	r.pass()
	// The reader the boot built signs through DISPATCH_DIRECTORY_ACCOUNT for DISPATCH_DIRECTORY_ADMIN.
	if signs := r.google.Signs(); len(signs) == 0 || signs[0].Principal != directoryCheckSigner || signs[0].Subject != directoryCheckAdmin {
		t.Fatalf("the pass's Directory check asked signJwt %+v; want it on %s for %s, the boot's two settings", signs, directoryCheckSigner, directoryCheckAdmin)
	}
	if held := r.heldSince(); held == nil {
		t.Fatal("the pass did not hold the person whose account is suspended")
	}

	r.directory(false)
	r.pass()
	wantAnswer(t, "the person's token once the pass finds their account active again, with no request from them", r.use(token), http.StatusOK, "", "")
	if held := r.heldSince(); held != nil {
		t.Fatalf("the person's row is held since %s after the Active outcome; want released", held)
	}
}

// TestIAPAssertionAloneReleasesNothing (c-iap-directory): under IAP a valid assertion of a held
// person, on a read and on a write, releases nothing: IAP's sign-in records the person and nothing
// more, so the hold Directory recorded stands.
//
// Control: let an IAP assertion release a hold (an IAP sign-in that clears held_since); the row
// reads released after the assertions, and the test fails.
func TestIAPAssertionAloneReleasesNothing(t *testing.T) {
	r := newIAPHoldRig(t)
	r.directory(false)
	r.mintToken()
	r.directory(true)
	r.pass()
	held := r.heldSince()
	if held == nil {
		t.Fatal("the pass did not hold the person whose account is suspended")
	}

	if whoami := r.browser(http.MethodGet, "/auth/whoami", ""); whoami.Code != http.StatusOK {
		t.Fatalf("GET /auth/whoami with the held person's valid assertion answered %d %s, want 200: IAP admits them", whoami.Code, whoami.Body.String())
	}
	r.browser(http.MethodPost, "/api/v1/me/agent-tokens", `{"name":"another agent"}`)
	if after := r.heldSince(); after == nil || !after.Equal(*held) {
		t.Fatalf("the held person's row after two valid assertions is held since %v; want %s, as before", after, held)
	}
}

// TestIAPRecreatedAddressConfirmsThroughTheRoutersCheck: under IAP, the Membership the router's
// Standing and the membership pass ask is handed the router's check of access that outlives the
// browser (buildAppContext, routes.AppContext.Outliving). The person's address is deleted and
// created again for a new account, which the Directory answers with another id: the pass holds the
// row while the earlier person's personal token is unrevoked, and once it is revoked the next pass
// confirms the new account, stores its id and releases the hold, with no operator command. The new
// person then mints a token of their own, which acts.
//
// Control: leave out buildAppContext's handoff. The Membership's question has no database and
// fails, so the second pass holds the row again, and the test fails naming the stored id.
func TestIAPRecreatedAddressConfirmsThroughTheRoutersCheck(t *testing.T) {
	r := newIAPHoldRig(t)
	r.directory(false)
	earlier := r.mintToken()
	wantAnswer(t, "the earlier person's token, their account active", r.use(earlier), http.StatusOK, "", "")

	r.google.SetUser(iapPerson, directorytest.User{ID: "u-person-2", PrimaryEmail: iapPerson})
	r.pass()
	if held := r.heldSince(); held == nil {
		t.Fatal("the pass did not hold the row once the address answered another account, while the earlier person's token is unrevoked")
	}

	r.exec(`update agent_tokens set revoked_at = now() where canonical_login(owner) = $1 and revoked_at is null`, iapPerson)
	r.pass()
	var stored string
	if err := r.database.Pool.QueryRow(context.Background(), `select coalesce(directory_user_id, 'null') from people where email = $1`, iapPerson).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if held := r.heldSince(); stored != "u-person-2" || held != nil {
		t.Fatalf("after the earlier person's token was revoked and a pass ran, the row stores id %s and is held since %v; want u-person-2 and no hold", stored, held)
	}
	newer := r.mintToken()
	wantAnswer(t, "the new person's own token", r.use(newer), http.StatusOK, "", "")
}
