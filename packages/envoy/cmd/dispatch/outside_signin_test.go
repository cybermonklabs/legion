package main

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/routes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/weborigin"
)

const (
	// outsiderEmail is an address outside the sign-in domain example.com that project PRIV's
	// access list names directly.
	outsiderEmail = "outsider@other.example"
	// strangerEmail is an address outside the sign-in domain that no list names.
	strangerEmail = "stranger@other.example"
	// privOwner owns PRIV.
	privOwner = "alice@example.com"
)

// outsideSignIn is a Dispatch signing example.com's people in with Google, against pool standing
// in for Google, and answering the people from outside the domain that a project names. One
// database holds the people, the projects and their access lists, as one does in a deployment.
type outsideSignIn struct {
	pool     *oidctest.Issuer
	database *store.Store
	people   testPeople
	boot     bootConfig
	handler  http.Handler
	// metrics is the registry the router brokerRouter builds counts on, as main's met is; nil
	// before brokerRouter runs.
	metrics *metrics.Registry
}

// newOutsideSignIn builds the server main serves for the Google sign-in of example.com with
// overrides applied, over a fresh database holding project PRIV, owned by alice, whose access
// list names outsiderEmail.
func newOutsideSignIn(t *testing.T, overrides map[string]string) *outsideSignIn {
	t.Helper()
	values := map[string]string{
		"DISPATCH_SIGNIN_ISSUER": "https://accounts.google.com",
		"DISPATCH_SIGNIN_GROUP":  "",
		"DISPATCH_SIGNIN_DOMAIN": "example.com",
	}
	for name, value := range overrides {
		values[name] = value
	}
	s := &outsideSignIn{pool: signInPool(t, "dispatch-client", "client-secret"), database: storetest.Open(t), boot: resolveWith(t, values)}
	// Resolved under Google's issuer, then pointed at the fake one, so no test calls Google.
	s.boot.SignInIssuer = s.pool.URL()
	s.people = testPeople{PgPeopleStore: store.NewPgPeopleStore(s.database.Pool, testSigningKey, nil, peopleOptions(s.boot)...), pool: s.database.Pool}
	if _, err := s.database.Pool.Exec(context.Background(), `insert into projects (key, name, default_assignee) values ('PRIV', 'Private', $1)`, privOwner); err != nil {
		t.Fatalf("seed PRIV: %v", err)
	}
	s.listOn(t, "PRIV", access.PrincipalPerson, outsiderEmail)
	s.handler = s.router(t, s.boot)
	return s
}

// router is the router main serves for boot over the server's one database, its sessions
// included, as main's are (store.NewPgSessionStore), at the canonical origin every request of these
// tests names (http://dispatch.test), the one a second origin is the second of.
func (s *outsideSignIn) router(t *testing.T, boot bootConfig) http.Handler {
	t.Helper()
	return routerFor(t, boot, routes.AppContextOptions{SignIn: signInFlow(boot), People: s.people, Store: s.database, Sessions: store.NewPgSessionStore(s.database.Pool), ServerURL: "http://dispatch.test"})
}

// listOn names principal, of kind, on project key's access list as its owner does.
func (s *outsideSignIn) listOn(t *testing.T, key, kind, principal string) {
	t.Helper()
	s.inTx(t, func(tx pgx.Tx) error {
		_, _, err := access.AddPrincipal(context.Background(), tx, events.NewBroker(), key, kind, principal, model.Actor{Kind: "user", ID: privOwner})
		return err
	})
}

func (s *outsideSignIn) inTx(t *testing.T, write func(tx pgx.Tx) error) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := write(tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// restrictedMember signs email in through handler as the Workspace account subject (hd
// example.com), then records its row as a restricted member's (restrict), and returns the sign-in's
// answer, whose cookie is that member's.
func (s *outsideSignIn) restrictedMember(t *testing.T, handler http.Handler, email, subject string) *httptest.ResponseRecorder {
	t.Helper()
	signedIn := s.workspaceSignIn(t, handler, email, subject)
	s.restrict(t, email, subject)
	return signedIn
}

// restrict records email's row as a restricted member's under subject and the refresh token the
// row holds, as a Google sign-in from outside the domain that an earlier build admitted left it.
func (s *outsideSignIn) restrict(t *testing.T, email, subject string) {
	t.Helper()
	ctx := context.Background()
	membership, _, err := s.people.Membership(ctx, email)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.people.ConfirmSubject(ctx, email, subject, membership.RefreshToken, membership.RefreshToken, model.MemberRestricted); err != nil {
		t.Fatalf("record %s as a restricted member: %v", email, err)
	}
}

// memberKind is the member kind people records for email, or "" with no row.
func (s *outsideSignIn) memberKind(t *testing.T, email string) string {
	t.Helper()
	var kind string
	err := s.database.Pool.QueryRow(context.Background(), `select member_kind from people where email = $1`, email).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return kind
}

// requestWith answers method path from handler carrying the session cookie signIn set, as the
// dashboard's own script sends it, from the server's origin; a cookie signIn deleted is not sent,
// as a browser sends none.
func requestWith(handler http.Handler, method, path string, signIn *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "http://dispatch.test"+path, nil)
	if method != http.MethodGet {
		request.Header.Set("Origin", "http://dispatch.test")
	}
	for _, cookie := range signIn.Result().Cookies() {
		if cookie.Name == "dsession" && cookie.Value != "" && cookie.MaxAge >= 0 {
			request.AddCookie(cookie)
		}
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// signedOutAnswer is what a 401 says of a sign-in it found ended: its code, and the reason and the
// login it gives the person, each empty where it gives none.
type signedOutAnswer struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
	Login  string `json:"login"`
}

// signedOutOf reads recorder as a JSON 401.
func signedOutOf(t *testing.T, what string, recorder *httptest.ResponseRecorder) signedOutAnswer {
	t.Helper()
	var answer signedOutAnswer
	if recorder.Code != http.StatusUnauthorized || json.Unmarshal(recorder.Body.Bytes(), &answer) != nil {
		t.Fatalf("%s answered %d %s, want a JSON 401", what, recorder.Code, recorder.Body.String())
	}
	return answer
}

// plainNoIdentity is the 401 Dispatch answered every request that proves no person before it told
// anyone why they were signed out (identity.WriteError), byte for byte.
const plainNoIdentity = `{"code":"NO_IDENTITY","error":"unauthorized"}` + "\n"

// requirePlainNoIdentity requires recorder to be that 401: its status, its body byte for byte, its
// JSON content type, and no cookie set.
func requirePlainNoIdentity(t *testing.T, what string, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Code != http.StatusUnauthorized || recorder.Body.String() != plainNoIdentity ||
		recorder.Header().Get("Content-Type") != "application/json" || len(recorder.Header().Values("Set-Cookie")) != 0 {
		t.Fatalf("%s answered %d %v %q, want the 401 %q with no cookie", what, recorder.Code, recorder.Header(), recorder.Body.String(), plainNoIdentity)
	}
}

// TestTheOutsideOriginServesNoGoogleSignInWithPasskeysNeverSet (R5): with DISPATCH_SIGNIN_PASSKEY
// never set, a deployment that answers at an outside origin too serves Google's sign-in at its
// canonical origin alone. At the outside origin GET /auth/start and GET /auth/callback answer 404,
// as a path no route serves does. At the canonical origin the Google sign-in of outsiderEmail, whom
// PRIV's access list names directly, gets the 403 page saying it is not in the domain, no session
// cookie and no people row, and alice, an account of example.com, signs in.
// With passkey sign-in on, TestTheOutsideOriginServesNoGoogleSignIn holds the outside origin's 404s
// and the canonical start, and TestAGoogleSignInOfAListedOutsiderIsRefusedWithPasskeysOn the
// refusal; TestTheOutsideOriginServesNoGoogleSignInWithPasskeysOff holds the 404s with the setting
// turned off on a database that had it on.
//
// Control: serve the Google sign-in at the outside origin while passkey sign-in is off
// (googleSignInHere refusing it only beside Passkeys), and the outside origin's /auth/start answers
// 302 and the row fails.
func TestTheOutsideOriginServesNoGoogleSignInWithPasskeysNeverSet(t *testing.T) {
	const canonical, outside = "https://dispatch.example.com", "https://dispatch-outside.example.com"
	s := newOutsideSignIn(t, nil)
	// The harness's deployment, answering at a second origin too: the origin's own settings are
	// judged in TestOutsideSignInSettingsRefusals, and BuildAppContext needs the canonical one.
	boot := s.boot
	if boot.SignInPasskey {
		t.Fatal("the harness's deployment signs people from outside in with a passkey; this row is the deployment that does not")
	}
	boot.OutsideOrigin = outside
	handler := routerFor(t, boot, routes.AppContextOptions{
		SignIn: signInFlow(boot), People: s.people, Store: s.database, Sessions: store.NewPgSessionStore(s.database.Pool),
		ServerURL: canonical,
	})
	for _, path := range []string{"/auth/start", "/auth/callback?code=c&state=s"} {
		wantCode(t, "GET "+path+" at the outside origin", call(handler, outside, http.MethodGet, path, nil), http.StatusNotFound, "NOT_FOUND")
	}
	refused := codeFlowSignInAt(t, handler, s.pool, "dispatch.example.com", map[string]any{"sub": "google-outsider", "email": outsiderEmail, "email_verified": true})
	if refused.Code != http.StatusForbidden || sessionCookieSet(refused) ||
		!strings.Contains(refused.Body.String(), outsiderEmail+" is not in the domain that may use Dispatch") {
		t.Errorf("the Google sign-in of %s at the canonical origin answered %d %s, want the 403 page saying it is not in the domain, with no session", outsiderEmail, refused.Code, refused.Body.String())
	}
	if kind := s.memberKind(t, outsiderEmail); kind != "" {
		t.Errorf("the refused Google sign-in of %s was recorded in people as %q, want no row", outsiderEmail, kind)
	}
	member := codeFlowSignInAt(t, handler, s.pool, "dispatch.example.com", map[string]any{"sub": "google-alice", "email": privOwner, "email_verified": true, "hd": "example.com"})
	if member.Code != http.StatusFound || !sessionCookieSet(member) {
		t.Errorf("the control, %s of example.com, at the canonical origin answered %d %s, want a signed-in redirect", privOwner, member.Code, member.Body.String())
	}
}

// A domain member whose sign-in the hourly check ends gets the 401 Dispatch answered before it told
// anyone why, byte for byte, at the request that ran the check and at every request after it, an
// API read included.
func TestOutsideSignInAnEndedDomainMemberIsAPlainNoIdentity(t *testing.T) {
	s := newOutsideSignIn(t, nil)
	signedIn := googleSignIn(t, s.handler, s.pool, "example.com")
	if signedIn.Code != http.StatusFound || !sessionCookieSet(signedIn) {
		t.Fatalf("an account of example.com: the callback answered %d %s, want a signed-in redirect", signedIn.Code, signedIn.Body.String())
	}
	s.pool.FailToken(http.StatusBadRequest, "invalid_grant")
	ageConfirmation(t, s.people, privOwner)
	requirePlainNoIdentity(t, "the whoami that ran the hourly check", requestWith(s.handler, http.MethodGet, "/auth/whoami", signedIn))
	requirePlainNoIdentity(t, "the whoami after it", requestWith(s.handler, http.MethodGet, "/auth/whoami", signedIn))
	requirePlainNoIdentity(t, "GET /api/v1/projects after it", requestWith(s.handler, http.MethodGet, "/api/v1/projects", signedIn))
	if membership, _, err := s.people.Membership(context.Background(), privOwner); err != nil || membership.RefreshToken != "" {
		t.Fatalf("%s's membership %+v (%v), want the refresh token the check ended gone", privOwner, membership, err)
	}
}

// A restricted member's Google sign-in that an earlier build admitted from outside the domain ends
// at its next hourly confirmation on a deployment that admits no restricted member, and the 401
// says why (OUTSIDE_SIGNIN_OFF, naming the address) there and at every request after: the
// refreshed account is of no Workspace, and no rule this deployment runs admits it.
func TestOutsideSignInARestrictedGoogleSignInEndsAtTheHourlyCheck(t *testing.T) {
	s := newOutsideSignIn(t, nil)
	response := s.restrictedMember(t, s.handler, outsiderEmail, "google-outsider")
	s.pool.UpdateGrants(func(claims map[string]any) { delete(claims, "hd") })
	ageConfirmation(t, s.people, outsiderEmail)
	want := signedOutAnswer{Code: "NO_IDENTITY", Reason: "OUTSIDE_SIGNIN_OFF", Login: outsiderEmail}
	for _, read := range []string{"the whoami that ran the hourly check", "the whoami after it"} {
		if got := signedOutOf(t, read, requestWith(s.handler, http.MethodGet, "/auth/whoami", response)); got != want {
			t.Fatalf("%s: %+v, want %+v", read, got, want)
		}
	}
	if membership, _, err := s.people.Membership(context.Background(), outsiderEmail); err != nil || membership.RefreshToken != "" {
		t.Fatalf("%s's membership %+v (%v), want the refresh token the check ended gone", outsiderEmail, membership, err)
	}
}

// An account of the domain's Workspace signs in as a full member.
func TestOutsideSignInKeepsAFullMemberFull(t *testing.T) {
	s := newOutsideSignIn(t, nil)
	if recorder := googleSignIn(t, s.handler, s.pool, "example.com"); recorder.Code != http.StatusFound || !sessionCookieSet(recorder) {
		t.Fatalf("an account of example.com: the callback answered %d %s, want a signed-in redirect", recorder.Code, recorder.Body.String())
	}
	if kind := s.memberKind(t, "alice@example.com"); kind != string(model.MemberFull) {
		t.Fatalf("people.member_kind of alice@example.com = %q, want full", kind)
	}
}

// The outside origin is the address people from outside reach Dispatch at, and header identity
// takes whoever a request header names, so the two together let any caller who reaches that
// address name itself in the header and be that person: a route of the outside origin would trust
// what the network says (R6). Boot refuses the pair. Seen on the binary at 0f5cba778a: with
// DISPATCH_IDENTITY=header:Tailscale-User-Login, DISPATCH_IDENTITY_HEADER_TRUSTED=1 and
// DISPATCH_OUTSIDE_ORIGIN=https://outside.example, a request with Host outside.example and
// Tailscale-User-Login: alice@example.com was answered GET /auth/whoami 200 as alice and GET
// /api/v1/projects 200. (The run used the deployment's own outside origin and Workspace domain;
// the addresses here stand for them.)
func TestOutsideSignInOriginIsRefusedBesideHeaderIdentity(t *testing.T) {
	env := headerIdentityEnvironment(map[string]string{
		"DISPATCH_SERVER_URL":     "https://dispatch.example",
		"DISPATCH_OUTSIDE_ORIGIN": "https://outside.example",
	})
	if _, err := resolveBootConfig(envGetter(env)); err == nil || !strings.Contains(err.Error(), "DISPATCH_OUTSIDE_ORIGIN") {
		t.Fatalf("resolveBootConfig with DISPATCH_OUTSIDE_ORIGIN beside header identity: err = %v, want a refusal naming DISPATCH_OUTSIDE_ORIGIN", err)
	}
	// The control: the same header identity with no outside origin still boots, as it does today.
	if _, err := resolveBootConfig(envGetter(headerIdentityEnvironment(map[string]string{"DISPATCH_SERVER_URL": "https://dispatch.example"}))); err != nil {
		t.Fatalf("resolveBootConfig with header identity and no outside origin: %v", err)
	}
}

// Each outside origin the server cannot serve refuses startup naming its setting.
func TestOutsideSignInSettingsRefusals(t *testing.T) {
	google := func(overrides map[string]string) map[string]string {
		values := map[string]string{"DISPATCH_SIGNIN_ISSUER": "https://accounts.google.com", "DISPATCH_SIGNIN_GROUP": "", "DISPATCH_SIGNIN_DOMAIN": "example.com"}
		for name, value := range overrides {
			values[name] = value
		}
		return values
	}
	for _, test := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"an outside origin with a path", signInEnvironment(google(map[string]string{"DISPATCH_OUTSIDE_ORIGIN": "https://outside.test/dispatch"})), "DISPATCH_OUTSIDE_ORIGIN: "},
		{"an http outside origin without test hooks", signInEnvironment(google(map[string]string{"DISPATCH_OUTSIDE_ORIGIN": "http://outside.test"})), "DISPATCH_OUTSIDE_ORIGIN: "},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := resolveBootConfig(envGetter(test.env)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("resolveBootConfig: err = %v, want a refusal containing %q", err, test.want)
			}
		})
	}
	// One row from each class of the shared rule (weborigin.Parse) is refused by boot too.
	for _, raw := range []string{"HTTPS://outside.test", "https://outside.test:443", "https://outside.test:", "https://outside.test:0443", "https://outside.test:65536", "https://10.1", "https://a.example.0x1"} {
		if _, err := resolveBootConfig(envGetter(signInEnvironment(google(map[string]string{"DISPATCH_OUTSIDE_ORIGIN": raw})))); err == nil || !strings.HasPrefix(err.Error(), "DISPATCH_OUTSIDE_ORIGIN: ") {
			t.Errorf("DISPATCH_OUTSIDE_ORIGIN=%q: err = %v, want a refusal naming DISPATCH_OUTSIDE_ORIGIN", raw, err)
		}
	}
	// The host must be another than the canonical origin's, which main checks once the canonical
	// origin resolves (DISPATCH_SERVER_URL or envoy.json); and an outside origin needs one.
	for _, test := range []struct{ outside, server, want string }{
		{"https://dispatch.example", "https://dispatch.example", "DISPATCH_OUTSIDE_ORIGIN: "},
		{"https://dispatch.example:8443", "https://DISPATCH.example", "DISPATCH_OUTSIDE_ORIGIN: "},
		{"https://outside.example", "", "DISPATCH_OUTSIDE_ORIGIN: "},
	} {
		if err := checkOutsideOrigin(test.outside, test.server); err == nil || !strings.HasPrefix(err.Error(), test.want) {
			t.Errorf("checkOutsideOrigin(%q, %q) = %v, want a refusal naming DISPATCH_OUTSIDE_ORIGIN", test.outside, test.server, err)
		}
	}
	if err := checkOutsideOrigin("https://outside.example", "https://dispatch.example"); err != nil {
		t.Errorf("an outside origin on another host than the canonical one: %v", err)
	}
	if err := checkOutsideOrigin("", ""); err != nil {
		t.Errorf("no outside origin and no canonical origin: %v", err)
	}
}

// originRuleRows are the inputs of weborigin's own tables, the origins weborigin.Parse takes
// (TestParseTakesAnOriginWrittenAsABrowserWritesIt) and those it refuses
// (TestParseRefusesWhatIsNoBrowsersOrigin), copied here because a test file cannot import another
// package's tests.
var originRuleRows = []string{
	"https://dispatch-outside.example.com",
	"https://dispatch-outside.example.com/",
	"https://dispatch-outside.example.com:8443",
	"http://dispatch.dispatch.svc.cluster.local:8766",
	"https://10.0.0.1",
	"https://abc--d.example.com",
	"https://a.example.com",
	"https://1outside.example.com",
	"https://" + strings.Repeat("a", 63) + ".example.com",
	"https://single",
	"https://xn--bcher-kva.example.com",
	"https://xn--p1ai.example.com",
	"https://xn--ls8h.example.com",
	"",
	"dispatch-outside.example.com",
	"ftp://dispatch-outside.example.com",
	"https://dispatch-outside.example.com/dispatch",
	"https://dispatch-outside.example.com//",
	"https://dispatch-outside.example.com?a=b",
	"https://dispatch-outside.example.com?",
	"https://dispatch-outside.example.com#top",
	"https://dispatch-outside.example.com#",
	"https://example-user@dispatch-outside.example.com",
	"https://Dispatch-Outside.example.com",
	"https://dispatch-outside..example.com",
	"https://dispatch-outside.example.com.",
	"https://" + strings.Repeat("a", 64) + ".example.com",
	"https://-outside.example.com",
	"https://outside-.example.com",
	"https://ab--cd.example.com",
	"https://outside.ab--cd.com",
	"https://xn--a.example.com",
	"https://xn--1ug.example.com",
	"https://xn--.example.com",
	"https://dispatch-outside.example.com:443",
	"https://outside.test:443",
	"http://dispatch-outside.example.com:80",
	"HTTPS://dispatch-outside.example.com",
	"https://dispatch-outside.example.com:",
	"https://dispatch-outside.example.com:0443",
	"https://dispatch-outside.example.com:0",
	"https://dispatch-outside.example.com:65536",
	"https://dispatch-outside.example.com:99999",
	"https://127.1",
	"https://0x7f.0.0.1",
	"https://2130706433",
	"https://010.0.0.1",
	"https://1.2.3.4.5",
	"https://example.123",
	"https://a.example.0x1",
}

// Boot judges DISPATCH_OUTSIDE_ORIGIN's spelling by the component's own rule, not a copy: with test
// hooks on (so an http row is judged by the rule, not by boot's own scheme condition) and a
// canonical origin no row names (so boot's other-host condition refuses none), boot takes exactly
// the rows weborigin.Parse takes, and stores what Parse returns.
func TestOutsideSignInOriginRuleIsTheComponents(t *testing.T) {
	const canonical = "https://canonical.invalid"
	for _, raw := range originRuleRows {
		parsed, ruleErr := weborigin.Parse(raw)
		boot, bootErr := resolveBootConfig(envGetter(signInEnvironment(map[string]string{"DISPATCH_TEST_HOOKS": "1", "DISPATCH_OUTSIDE_ORIGIN": raw})))
		if bootErr == nil {
			bootErr = checkOutsideOrigin(boot.OutsideOrigin, canonical)
		}
		switch {
		case raw == "":
			// Empty is unset: no outside origin at all, which the rule has no say in.
			if bootErr != nil || boot.OutsideOrigin != "" {
				t.Errorf("DISPATCH_OUTSIDE_ORIGIN empty: boot %q, %v; want no outside origin", boot.OutsideOrigin, bootErr)
			}
		case ruleErr != nil:
			if bootErr == nil || !strings.HasPrefix(bootErr.Error(), "DISPATCH_OUTSIDE_ORIGIN: ") || !strings.Contains(bootErr.Error(), ruleErr.Error()) {
				t.Errorf("DISPATCH_OUTSIDE_ORIGIN=%q: boot answered %v, want the rule's refusal %q naming the setting", raw, bootErr, ruleErr)
			}
		case bootErr != nil || boot.OutsideOrigin != parsed.String():
			t.Errorf("DISPATCH_OUTSIDE_ORIGIN=%q: boot %q, %v; want the rule's %q", raw, boot.OutsideOrigin, bootErr, parsed.String())
		}
	}
}

// outsideHostRows are hosts of each kind weborigin.PasskeyHost judges: names whose top-level domain
// is in the public suffix list's ICANN section (one listed only as a wildcard rule, one
// internationalised, one under a private-section suffix), and an IP address, a single label and
// top-level domains off the list, the reserved ones this package's other tests use among them.
var outsideHostRows = []string{
	"dispatch-outside.example.com",
	"outside.np",
	"outside.xn--p1ai",
	"outside.github.io",
	"10.0.0.1",
	"single",
	"outside.corp",
	"outside.test",
	"outside.example",
}

// Under Google sign-in, boot judges DISPATCH_OUTSIDE_ORIGIN's host by the component's own rule for
// the host of a passkey's relying party, not a copy: it refuses exactly the hosts
// weborigin.PasskeyHost refuses, with that rule's reason under the setting's name, and takes the
// rest. The dev OIDC issuer the browser suite puts in Google's place has no such rule, so under it
// boot takes a loopback host.
func TestOutsideSignInOriginHostRuleIsTheComponents(t *testing.T) {
	google := map[string]string{"DISPATCH_SIGNIN_ISSUER": "https://accounts.google.com", "DISPATCH_SIGNIN_GROUP": "", "DISPATCH_SIGNIN_DOMAIN": "example.com"}
	refused := 0
	for _, host := range outsideHostRows {
		env := maps.Clone(google)
		env["DISPATCH_OUTSIDE_ORIGIN"] = "https://" + host
		ruleErr := weborigin.PasskeyHost(host)
		boot, bootErr := resolveBootConfig(envGetter(signInEnvironment(env)))
		switch {
		case ruleErr != nil:
			refused++
			if bootErr == nil || bootErr.Error() != "DISPATCH_OUTSIDE_ORIGIN's "+ruleErr.Error() {
				t.Errorf("DISPATCH_OUTSIDE_ORIGIN=https://%s under Google sign-in: boot answered %v, want the rule's refusal %q under the setting's name", host, bootErr, ruleErr)
			}
		case bootErr != nil || boot.OutsideOrigin != "https://"+host:
			t.Errorf("DISPATCH_OUTSIDE_ORIGIN=https://%s under Google sign-in: boot %q, %v; want it taken", host, boot.OutsideOrigin, bootErr)
		}
	}
	if refused == 0 || refused == len(outsideHostRows) {
		t.Fatalf("the rule refused %d of %d hosts, so the rows do not hold both a host it takes and one it refuses", refused, len(outsideHostRows))
	}
	harness := maps.Clone(google)
	maps.Copy(harness, map[string]string{
		"DISPATCH_SIGNIN_ISSUER": "http://127.0.0.1:9", "DISPATCH_TEST_HOOKS": "1", "DISPATCH_TEST_SIGNIN_DOMAIN_ISSUER": "1",
		"DISPATCH_OUTSIDE_ORIGIN": "http://localhost:5199",
	})
	if boot, err := resolveBootConfig(envGetter(signInEnvironment(harness))); err != nil || boot.OutsideOrigin != "http://localhost:5199" {
		t.Errorf("DISPATCH_OUTSIDE_ORIGIN=http://localhost:5199 under the dev OIDC issuer: boot %q, %v; want it taken", boot.OutsideOrigin, err)
	}
}

// outsideOriginReachesItsReaders hands DISPATCH_OUTSIDE_ORIGIN, through the settings read alone, to
// each reader: the router, which answers a request whose Host is the outside origin's as that
// origin (where Google's sign-in start answers 404) and every other request as the canonical origin
// (whose start comes back to its own callback); the document service, which reads a dashboard link
// of either origin as a reference; and rebuild-refs, which reindexes those links and refuses an
// outside origin the server would refuse.
func outsideOriginReachesItsReaders(t *testing.T) {
	t.Helper()
	pool := signInPool(t, "dispatch-client", "client-secret")
	boot := resolveWith(t, map[string]string{"DISPATCH_SIGNIN_ISSUER": pool.URL(), "DISPATCH_OUTSIDE_ORIGIN": " https://outside.example/ "})
	if boot.OutsideOrigin != "https://outside.example" {
		t.Fatalf("OutsideOrigin = %q, want https://outside.example", boot.OutsideOrigin)
	}
	handler := routerFor(t, boot, routes.AppContextOptions{SignIn: signInFlow(boot), ServerURL: "https://dispatch.example"})
	for _, host := range []string{"outside.example", "OUTSIDE.example"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://"+host+"/auth/start", nil))
		if recorder.Code != http.StatusNotFound {
			t.Errorf("GET /auth/start with Host %s: %d, Location %q; want 404, the outside origin's", host, recorder.Code, recorder.Header().Get("Location"))
		}
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://dispatch.example/auth/start", nil))
	location, err := url.Parse(recorder.Header().Get("Location"))
	if recorder.Code != http.StatusFound || err != nil || location.Query().Get("redirect_uri") != "https://dispatch.example/auth/callback" {
		t.Errorf("GET /auth/start with Host dispatch.example: %d, Location %q (%v); want a redirect_uri of https://dispatch.example/auth/callback", recorder.Code, recorder.Header().Get("Location"), err)
	}
	deps := documentDeps(boot, "https://dispatch.example", docs.Deps{})
	for _, page := range []string{"https://dispatch.example/issues/ABC-1", "https://outside.example/issues/ABC-1"} {
		if refs := text.Extract(page, deps.Origins); len(refs) != 1 || refs[0].Kind != "issue" {
			t.Errorf("the document service reads %s as %+v, want the issue it names", page, refs)
		}
	}
	origin, err := rebuildRefsOutsideOrigin(envGetter(map[string]string{"DISPATCH_OUTSIDE_ORIGIN": "https://outside.example/"}), "https://dispatch.example")
	if err != nil || origin != "https://outside.example" {
		t.Errorf("rebuild-refs reads DISPATCH_OUTSIDE_ORIGIN as %q, %v; want https://outside.example", origin, err)
	}
	if _, err := rebuildRefsOutsideOrigin(envGetter(map[string]string{"DISPATCH_OUTSIDE_ORIGIN": "https://dispatch.example"}), "https://dispatch.example"); err == nil {
		t.Error("rebuild-refs took an outside origin on the canonical origin's host, which the server refuses")
	}
}
