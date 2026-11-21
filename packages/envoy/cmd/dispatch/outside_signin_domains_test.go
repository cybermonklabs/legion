package main

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// companyAddress is an address on second.example, a domain the Workspace of example.com owns
// beside its sign-in domain.
const companyAddress = "bob@second.example"

// workspaceAddressRefusal is what the refusal page says to an account outside the Workspace at
// companyAddress.
const workspaceAddressRefusal = companyAddress + " is an address on a domain the Google Workspace owns, so it signs in only as a Workspace account"

// With DISPATCH_SIGNIN_WORKSPACE_DOMAINS naming second.example, an account outside the Workspace
// (no hd) at an address on it is refused even while PRIV's list names the address: the 403 page
// says why, and it gets no cookie and no people row, so it never reads what the list grants the
// member before the member's first sign-in. The Workspace account at the same address (hd the
// sign-in domain) then signs in as the full member.
func TestOutsideSignInRefusesACompanyDomainAddressForAnOutsideAccount(t *testing.T) {
	s := newOutsideSignIn(t, map[string]string{"DISPATCH_SIGNIN_WORKSPACE_DOMAINS": "second.example"})
	s.listOn(t, "PRIV", access.PrincipalPerson, companyAddress)
	outsider := codeFlowSignIn(t, s.handler, s.pool, map[string]any{"sub": "google-outsider", "email": companyAddress, "email_verified": true})
	if outsider.Code != http.StatusForbidden || sessionCookieSet(outsider) || !strings.Contains(outsider.Body.String(), workspaceAddressRefusal) {
		t.Fatalf("an account outside the Workspace at %s: the callback answered %d %s, want the 403 page saying %q, with no session", companyAddress, outsider.Code, outsider.Body.String(), workspaceAddressRefusal)
	}
	if kind := s.memberKind(t, companyAddress); kind != "" {
		t.Fatalf("the refused account was recorded in people as %q", kind)
	}
	member := codeFlowSignIn(t, s.handler, s.pool, map[string]any{"sub": "google-member", "email": companyAddress, "email_verified": true, "hd": "example.com"})
	if member.Code != http.StatusFound || !sessionCookieSet(member) {
		t.Fatalf("the Workspace account at %s: the callback answered %d %s, want a signed-in redirect", companyAddress, member.Code, member.Body.String())
	}
	if kind := s.memberKind(t, companyAddress); kind != string(model.MemberFull) {
		t.Fatalf("people.member_kind of %s = %q, want full", companyAddress, kind)
	}
	whoami := requestWith(s.handler, http.MethodGet, "/auth/whoami", member)
	if body := whoami.Body.String(); whoami.Code != http.StatusOK || !strings.Contains(body, `"login":"`+companyAddress+`"`) || strings.Contains(body, "member_kind") {
		t.Fatalf("the member's whoami answered %d %s, want 200 naming %s as a full member", whoami.Code, body, companyAddress)
	}
}

// A Unicode spelling of a domain the Workspace owns is that domain. With
// DISPATCH_SIGNIN_WORKSPACE_DOMAINS written as the A-label xn--bcher-kva.example, an account
// outside the Workspace at Bob@Bücher.example is refused as one at the A-label address would be,
// even while PRIV's list names the address, with no cookie and no people row; and with the setting
// written in Unicode, bücher.example, which the rule holds as its A-label, the account at the
// A-label address is refused alike.
func TestOutsideSignInRefusesAUnicodeSpellingOfACompanyDomainAddress(t *testing.T) {
	for _, tc := range []struct{ setting, email, login string }{
		{"xn--bcher-kva.example", "Bob@Bücher.example", "bob@bücher.example"},
		{"bücher.example", "bob@xn--bcher-kva.example", "bob@xn--bcher-kva.example"},
	} {
		t.Run(tc.setting+", "+tc.email, func(t *testing.T) {
			s := newOutsideSignIn(t, map[string]string{"DISPATCH_SIGNIN_WORKSPACE_DOMAINS": tc.setting})
			rule, err := signInRule(s.boot, identity.ListerFunc(nil))
			if outside, ok := rule.(identity.OutsideRule); err != nil || !ok || !slices.Equal(outside.WorkspaceDomains, []string{"xn--bcher-kva.example"}) {
				t.Errorf("signInRule with %s = %#v, %v; want the outside rule listing xn--bcher-kva.example", tc.setting, rule, err)
			}
			s.listOn(t, "PRIV", access.PrincipalPerson, tc.login)
			outsider := codeFlowSignIn(t, s.handler, s.pool, map[string]any{"sub": "google-outsider", "email": tc.email, "email_verified": true})
			refusal := tc.login + " is an address on a domain the Google Workspace owns, so it signs in only as a Workspace account"
			if outsider.Code != http.StatusForbidden || sessionCookieSet(outsider) || !strings.Contains(outsider.Body.String(), refusal) {
				t.Fatalf("an account outside the Workspace at %s: the callback answered %d %s, want the 403 page saying %q, with no session", tc.email, outsider.Code, outsider.Body.String(), refusal)
			}
			if kind := s.memberKind(t, tc.login); kind != "" {
				t.Fatalf("the refused account was recorded in people as %q", kind)
			}
		})
	}
}

// Boot refuses DISPATCH_SIGNIN_WORKSPACE_DOMAINS without the sign-in domain it lists the
// Workspace's other domains beside, beside header identity with the other sign-in settings, and
// with an entry that is not a lowercase domain name IDNA lookup takes (an upper-case letter, or a
// label such as xn--a that decodes to nothing), naming the entry's position and never its value,
// whether outside sign-in is on or off. It takes a list of domains, each trimmed, an
// internationalized one written in Unicode or as its A-labels.
func TestOutsideSignInWorkspaceDomainsSettingRefusals(t *testing.T) {
	const name = "DISPATCH_SIGNIN_WORKSPACE_DOMAINS"
	google := func(outside, domains string) map[string]string {
		return signInEnvironment(map[string]string{"DISPATCH_SIGNIN_ISSUER": "https://accounts.google.com", "DISPATCH_SIGNIN_GROUP": "", "DISPATCH_SIGNIN_DOMAIN": "example.com", "DISPATCH_SIGNIN_OUTSIDE": outside, name: domains})
	}
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"no sign-in at all", noSignInEnvironment(map[string]string{name: "second.example"}), name + " needs DISPATCH_SIGNIN_DOMAIN"},
		{"a Cognito group", signInEnvironment(map[string]string{name: "second.example"}), name + " needs DISPATCH_SIGNIN_DOMAIN"},
		{"header identity", headerIdentityEnvironment(map[string]string{name: "second.example"}), name + " must be unset because header identity"},
	} {
		t.Run("beside "+tc.name, func(t *testing.T) {
			if _, err := resolveBootConfig(envGetter(tc.env)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("resolveBootConfig: err = %v, want a refusal containing %q", err, tc.want)
			}
		})
	}
	for value, want := range map[string]string{
		"Second.example":                 "entry 1 of 1 must be a lowercase domain name such as example.com; got a value of 14 characters",
		"Bücher.example":                 "entry 1 of 1 must be a lowercase domain name such as example.com; got a value of 14 characters",
		"second.example,xn--a.example":   "entry 2 of 2 must be a lowercase domain name such as example.com; got a value of 13 characters",
		"second.example,https://x.test":  "entry 2 of 2 must be a lowercase domain name such as example.com; got a value of 14 characters",
		"second.example,bob@x.test":      "entry 2 of 2 must be a lowercase domain name such as example.com; got a value of 10 characters",
		"second.example,third":           "entry 2 of 2 must be a lowercase domain name such as example.com; got a value of 5 characters",
		"second.example,,third.example":  "entry 2 of 3 is empty",
		"second.example,":                "entry 2 of 2 is empty",
		strings.Repeat("a", 64) + ".com": "entry 1 of 1 must be a lowercase domain name such as example.com; got a value of 68 characters",
	} {
		for _, outside := range []string{"1", ""} {
			t.Run(value+", DISPATCH_SIGNIN_OUTSIDE="+strconv.Quote(outside), func(t *testing.T) {
				_, err := resolveBootConfig(envGetter(google(outside, value)))
				if err == nil || err.Error() != name+" "+want {
					t.Fatalf("resolveBootConfig: err = %v, want %q", err, name+" "+want)
				}
			})
		}
	}
	for value, want := range map[string][]string{
		"second.example":                       {"second.example"},
		" second.example , eu.third.example ":  {"second.example", "eu.third.example"},
		"bücher.example,xn--bcher-kva.example": {"bücher.example", "xn--bcher-kva.example"},
		"":                                     nil,
	} {
		for _, outside := range []string{"1", ""} {
			if boot := resolveWith(t, google(outside, value)); !slices.Equal(boot.SignInWorkspaceDomains, want) {
				t.Errorf("%s=%q, DISPATCH_SIGNIN_OUTSIDE=%q: SignInWorkspaceDomains = %q, want %q", name, value, outside, boot.SignInWorkspaceDomains, want)
			}
		}
	}
}

// With DISPATCH_SIGNIN_OUTSIDE unset, DISPATCH_SIGNIN_WORKSPACE_DOMAINS changes nothing (R9): the
// rule is the domain rule alone, which never reads it, and a server with it set answers a
// Workspace sign-in at an address on the listed domain, that member's whoami, and a refused
// account outside the Workspace at the same address exactly as a server without it.
func TestOutsideSignInWorkspaceDomainsChangeNothingWithOutsideSignInOff(t *testing.T) {
	without := newOutsideSignIn(t, map[string]string{"DISPATCH_SIGNIN_OUTSIDE": ""})
	with := newOutsideSignIn(t, map[string]string{"DISPATCH_SIGNIN_OUTSIDE": "", "DISPATCH_SIGNIN_WORKSPACE_DOMAINS": "second.example"})
	rule, err := signInRule(with.boot, identity.ListerFunc(nil))
	if domain, ok := rule.(identity.DomainRule); err != nil || !ok || domain != (identity.DomainRule{Domain: "example.com"}) {
		t.Fatalf("signInRule with outside sign-in off and %s set = %#v, %v; want the domain rule of example.com alone", "DISPATCH_SIGNIN_WORKSPACE_DOMAINS", rule, err)
	}
	answers := func(s *outsideSignIn) (start, member, whoami, outsider signInAnswer) {
		start = answerOf(startSignIn(t, s.handler))
		signedIn := codeFlowSignIn(t, s.handler, s.pool, map[string]any{"sub": "google-member", "email": companyAddress, "email_verified": true, "hd": "example.com"})
		member = answerOf(signedIn)
		whoami = answerOf(requestWith(s.handler, http.MethodGet, "/auth/whoami", signedIn))
		outsider = answerOf(codeFlowSignIn(t, s.handler, s.pool, map[string]any{"sub": "google-outsider", "email": "carol@second.example", "email_verified": true}))
		return start, member, whoami, outsider
	}
	wantStart, wantMember, wantWhoami, wantOutsider := answers(without)
	gotStart, gotMember, gotWhoami, gotOutsider := answers(with)
	for _, read := range []struct {
		what      string
		got, want signInAnswer
	}{
		{"GET /auth/start", gotStart, wantStart},
		{"the Workspace account's callback", gotMember, wantMember},
		{"the member's whoami", gotWhoami, wantWhoami},
		{"the callback of an account outside the Workspace", gotOutsider, wantOutsider},
	} {
		if !read.got.equal(read.want) {
			t.Errorf("%s with %s set: %+v, want %+v as without it", read.what, "DISPATCH_SIGNIN_WORKSPACE_DOMAINS", read.got, read.want)
		}
	}
	if wantMember.Code != http.StatusFound || wantOutsider.Code != http.StatusForbidden || !strings.Contains(wantOutsider.Body, "is not in the domain") {
		t.Fatalf("without the setting: the member answered %d and the outsider %d %s, want a signed-in redirect and the 403 page saying not in the domain", wantMember.Code, wantOutsider.Code, wantOutsider.Body)
	}
}

// signInAnswer is a response as two servers over their own issuers and databases can be compared:
// its status, its Location without what each sign-in draws afresh (the state, the nonce) and the
// issuer's own address, its body (none for a redirect, whose body only repeats its Location) with a
// JSON body's event head dropped, and each cookie it sets without its signed value and expiry.
type signInAnswer struct {
	Code     int
	Location string
	Body     string
	Cookies  []string
}

func (a signInAnswer) equal(b signInAnswer) bool {
	return a.Code == b.Code && a.Location == b.Location && a.Body == b.Body && slices.Equal(a.Cookies, b.Cookies)
}

func answerOf(response *httptest.ResponseRecorder) signInAnswer {
	answer := signInAnswer{Code: response.Code, Body: response.Body.String()}
	if location, err := url.Parse(response.Header().Get("Location")); err == nil && location.String() != "" {
		query := location.Query()
		query.Del("state")
		query.Del("nonce")
		answer.Location = location.Path + "?" + query.Encode()
		answer.Body = ""
	}
	var body map[string]any
	if json.Unmarshal(response.Body.Bytes(), &body) == nil {
		delete(body, "event_head")
		encoded, _ := json.Marshal(body)
		answer.Body = string(encoded)
	}
	for _, cookie := range response.Result().Cookies() {
		answer.Cookies = append(answer.Cookies, strings.Join([]string{cookie.Name, strconv.FormatBool(cookie.Value != ""), cookie.Path, strconv.Itoa(cookie.MaxAge), strconv.FormatBool(cookie.Secure), strconv.FormatBool(cookie.HttpOnly), strconv.Itoa(int(cookie.SameSite))}, ";"))
	}
	return answer
}

// workspaceDomainsReachTheirReader hands DISPATCH_SIGNIN_WORKSPACE_DOMAINS, through the settings
// read alone, to its one reader: the outside rule main builds (signInRule), which refuses an
// account outside the Workspace at an address on a listed domain. The domain rule, with outside
// sign-in off, is built without it.
func workspaceDomainsReachTheirReader(t *testing.T) {
	google := map[string]string{"DISPATCH_SIGNIN_ISSUER": "https://accounts.google.com", "DISPATCH_SIGNIN_GROUP": "", "DISPATCH_SIGNIN_DOMAIN": "example.com", "DISPATCH_SIGNIN_OUTSIDE": "1", "DISPATCH_SIGNIN_WORKSPACE_DOMAINS": " second.example "}
	boot := resolveWith(t, google)
	if !slices.Equal(boot.SignInWorkspaceDomains, []string{"second.example"}) {
		t.Errorf("SignInWorkspaceDomains = %q, want [second.example]", boot.SignInWorkspaceDomains)
	}
	refusedWith(t, map[string]string{"DISPATCH_SIGNIN_WORKSPACE_DOMAINS": "second.example"}, "DISPATCH_SIGNIN_WORKSPACE_DOMAINS needs DISPATCH_SIGNIN_DOMAIN")
	rule, err := signInRule(boot, identity.ListerFunc(nil))
	if outside, ok := rule.(identity.OutsideRule); err != nil || !ok || !slices.Equal(outside.WorkspaceDomains, []string{"second.example"}) {
		t.Errorf("signInRule = %#v, %v; want the outside rule listing second.example", rule, err)
	}
	off := maps.Clone(google)
	off["DISPATCH_SIGNIN_OUTSIDE"] = ""
	if rule, err := signInRule(resolveWith(t, off), nil); err != nil || rule != (identity.DomainRule{Domain: "example.com"}) {
		t.Errorf("signInRule with outside sign-in off = %#v, %v; want the domain rule alone", rule, err)
	}
	s := newOutsideSignIn(t, map[string]string{"DISPATCH_SIGNIN_WORKSPACE_DOMAINS": " second.example "})
	s.listOn(t, "PRIV", access.PrincipalPerson, companyAddress)
	if response := s.signInAs(t, s.handler, companyAddress); response.Code != http.StatusForbidden || sessionCookieSet(response) || !strings.Contains(response.Body.String(), workspaceAddressRefusal) {
		t.Errorf("the router's sign-in of an account outside the Workspace at %s answered %d %s, want the 403 page saying %q", companyAddress, response.Code, response.Body.String(), workspaceAddressRefusal)
	}
}
