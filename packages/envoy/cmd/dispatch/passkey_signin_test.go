package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/logtest"
)

// The two origins of a deployment whose people from outside sign in with a passkey: the canonical
// one (the tailnet's) every request of outsideSignIn names, and the outside origin, whose host is
// the passkey's relying party.
const (
	canonicalOrigin = "http://dispatch.test"
	passkeyOrigin   = "https://outside.example.com"
	passkeyRPID     = "outside.example.com"
)

// passkeyRig is a Dispatch signing example.com's people in with Google and, with
// DISPATCH_SIGNIN_PASSKEY=1 at passkeyOrigin, the people outside it that a project names with a
// passkey: outsideSignIn's server, with alice, PRIV's owner, signed in at her browser.
type passkeyRig struct {
	*outsideSignIn
	alice *http.Cookie
}

// newPasskeyRig builds the rig with overrides applied to its settings, over a fresh database
// holding PRIV, owned by alice, whose list names outsiderEmail directly.
func newPasskeyRig(t *testing.T, overrides map[string]string) *passkeyRig {
	t.Helper()
	values := map[string]string{"DISPATCH_SIGNIN_PASSKEY": "1", "DISPATCH_OUTSIDE_ORIGIN": passkeyOrigin}
	maps.Copy(values, overrides)
	r := &passkeyRig{outsideSignIn: newOutsideSignIn(t, values)}
	signedIn := codeFlowSignIn(t, r.handler, r.pool, map[string]any{
		"sub": "google-alice", "email": privOwner, "email_verified": true, "hd": "example.com",
	})
	r.alice = sessionCookieOf(t, "alice's Google sign-in", signedIn)
	if _, err := r.database.Pool.Exec(context.Background(), `update projects set owner = $1 where key = 'PRIV'`, privOwner); err != nil {
		t.Fatalf("give PRIV to alice: %v", err)
	}
	return r
}

// serverWith is another server over the rig's database, signing in through the rig's stand-in for
// Google, with overrides applied to the rig's settings, and the boot it was built from.
func (r *passkeyRig) serverWith(t *testing.T, overrides map[string]string) (http.Handler, bootConfig) {
	t.Helper()
	values := map[string]string{
		"DISPATCH_SIGNIN_ISSUER": googleIssuer, "DISPATCH_SIGNIN_GROUP": "", "DISPATCH_SIGNIN_DOMAIN": "example.com",
		"DISPATCH_SIGNIN_PASSKEY": "1", "DISPATCH_OUTSIDE_ORIGIN": passkeyOrigin,
	}
	maps.Copy(values, overrides)
	boot := resolveWith(t, values)
	boot.SignInIssuer = r.pool.URL()
	return r.router(t, boot), boot
}

// sessionCookieOf is the session cookie response set, failing the test naming what when it set none.
func sessionCookieOf(t *testing.T, what string, response *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == "dsession" && cookie.Value != "" && cookie.MaxAge >= 0 {
			return cookie
		}
	}
	t.Fatalf("%s answered %d %s and set no session cookie", what, response.Code, response.Body.String())
	return nil
}

// call answers method path at origin from handler, with body as JSON (nil for none), carrying
// cookies; a write names its own origin, as the page of that origin does.
func call(handler http.Handler, origin, method, path string, body any, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	var reader *bytes.Reader
	if body != nil {
		raw, ok := body.([]byte)
		if !ok {
			raw, _ = json.Marshal(body)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	request := httptest.NewRequest(method, origin+path, reader)
	request.Header.Set("Content-Type", "application/json")
	if method != http.MethodGet {
		request.Header.Set("Origin", origin)
	}
	for _, cookie := range cookies {
		if cookie != nil {
			request.AddCookie(cookie)
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// asOwner answers method path at the canonical origin as alice at her browser.
func (r *passkeyRig) asOwner(method, path string, body any) *httptest.ResponseRecorder {
	return call(r.handler, canonicalOrigin, method, path, body, r.alice)
}

// invite is alice's link for email on PRIV, failing the test for any answer but 201; it answers
// the link's token, its URL fragment.
func (r *passkeyRig) invite(t *testing.T, email string) string {
	t.Helper()
	return linkToken(t, "alice's link for "+email, r.asOwner(http.MethodPost, "/api/v1/projects/PRIV/access/people/"+url.PathEscape(email)+"/invitations", nil))
}

// linkToken is the token of the link response answers, failing the test naming what for any
// answer but 201 {url, expires_at} with a url of the outside origin's enrollment page.
func linkToken(t *testing.T, what string, response *httptest.ResponseRecorder) string {
	t.Helper()
	var answer struct {
		URL       string `json:"url"`
		ExpiresAt string `json:"expires_at"`
	}
	if response.Code != http.StatusCreated || json.Unmarshal(response.Body.Bytes(), &answer) != nil {
		t.Fatalf("%s answered %d %s, want 201 {url, expires_at}", what, response.Code, response.Body.String())
	}
	page, token, ok := strings.Cut(answer.URL, "#")
	if !ok || page != passkeyOrigin+"/enroll" || len(token) != 43 || answer.ExpiresAt == "" {
		t.Fatalf("%s is %+v, want %s/enroll#<43 characters> and when it lapses", what, answer, passkeyOrigin)
	}
	return token
}

// ceremonyCookie is the ceremony cookie a begin set, the `__Host-` one the rig's Secure cookies
// carry.
func ceremonyCookie(response *httptest.ResponseRecorder) *http.Cookie {
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == "__Host-dispatch_passkey_ceremony" && cookie.Value != "" {
			return cookie
		}
	}
	return nil
}

// beginEnrollment is the enrollment's begin with token at the outside origin.
func (r *passkeyRig) beginEnrollment(handler http.Handler, token string) *httptest.ResponseRecorder {
	return call(handler, passkeyOrigin, http.MethodPost, "/auth/passkey/register/begin", map[string]string{"token": token})
}

// enroll runs a whole enrollment through handler with token and device: begin, the device's
// passkey, finish. It answers the finish, or the begin when the begin was refused.
func (r *passkeyRig) enroll(t *testing.T, handler http.Handler, device *virtualAuthenticator, token string) *httptest.ResponseRecorder {
	t.Helper()
	begun := r.beginEnrollment(handler, token)
	if begun.Code != http.StatusOK {
		return begun
	}
	return call(handler, passkeyOrigin, http.MethodPost, "/auth/passkey/register/finish", device.register(t, begun.Body.Bytes()), ceremonyCookie(begun))
}

// enrolled is enroll through the rig's server, failing the test for any answer but 200 naming
// email; it answers the session cookie the enrollment set.
func (r *passkeyRig) enrolled(t *testing.T, device *virtualAuthenticator, token, email string) *http.Cookie {
	t.Helper()
	finished := r.enroll(t, r.handler, device, token)
	if finished.Code != http.StatusOK || !strings.Contains(finished.Body.String(), `"login":"`+email+`"`) {
		t.Fatalf("%s's enrollment answered %d %s, want 200 naming them", email, finished.Code, finished.Body.String())
	}
	return sessionCookieOf(t, email+"'s enrollment", finished)
}

// signIn runs a whole passkey sign-in through handler with device: begin, the device's assertion
// at its next counter, finish.
func (r *passkeyRig) signIn(t *testing.T, handler http.Handler, device *virtualAuthenticator) *httptest.ResponseRecorder {
	t.Helper()
	begun := call(handler, passkeyOrigin, http.MethodPost, "/auth/passkey/login/begin", map[string]string{})
	if begun.Code != http.StatusOK {
		t.Fatalf("the sign-in's begin answered %d %s, want 200", begun.Code, begun.Body.String())
	}
	return call(handler, passkeyOrigin, http.MethodPost, "/auth/passkey/login/finish", device.assert(t, begun.Body.Bytes()), ceremonyCookie(begun))
}

// whoamiOutside is GET /auth/whoami at the outside origin through handler carrying cookie.
func whoamiOutside(handler http.Handler, cookie *http.Cookie) *httptest.ResponseRecorder {
	return call(handler, passkeyOrigin, http.MethodGet, "/auth/whoami", nil, cookie)
}

// passkeysOf counts the passkeys stored for email.
func (r *passkeyRig) passkeysOf(t *testing.T, email string) int {
	t.Helper()
	var n int
	if err := r.database.Pool.QueryRow(context.Background(), `select count(*) from passkeys where email = $1`, email).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// wantCode requires response to be status with code, naming what.
func wantCode(t *testing.T, what string, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status || errorCode(response) != code {
		t.Fatalf("%s answered %d %s, want %d %s", what, response.Code, response.Body.String(), status, code)
	}
}

// signedInRestricted requires whoami at the outside origin with cookie to name email, a restricted
// member.
func signedInRestricted(t *testing.T, handler http.Handler, cookie *http.Cookie, email string) {
	t.Helper()
	whoami := whoamiOutside(handler, cookie)
	if body := whoami.Body.String(); whoami.Code != http.StatusOK || !strings.Contains(body, `"login":"`+email+`"`) || !strings.Contains(body, `"member_kind":"restricted"`) {
		t.Fatalf("whoami at the outside origin answered %d %s, want %s signed in as a restricted member", whoami.Code, body, email)
	}
}

// agePasskeyConfirmation dates email's last confirmation two hours back by the database's clock, so
// their next request finds it older than the hour.
func (r *passkeyRig) agePasskeyConfirmation(t *testing.T, email string) {
	t.Helper()
	tag, err := r.database.Pool.Exec(context.Background(), `update people set confirmed_at = now() - interval '2 hours' where email = $1 and confirmed_at is not null`, email)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("age %s's confirmation: %v (%d rows), want one sign-in standing", email, err, tag.RowsAffected())
	}
}

// TestPasskeyEnrollsOnlyAListedOutsideAddress (R5): the owner's link enrolls a passkey for an
// address outside the domain that the access list names directly, which then signs in as a
// restricted member at the outside origin; the owner gets no link for an address the list does
// not name, nor for one only a group on the list holds; and a link whose email the list stopped
// naming (here by a write that is no removal) enrolls nothing, 403 INVITATION_NOT_LISTED.
//
// Control: answer namedPerson's check as named for every address, and the stranger's link is made
// and enrolls, and the row fails naming stranger@other.example.
func TestPasskeyEnrollsOnlyAListedOutsideAddress(t *testing.T) {
	r := newPasskeyRig(t, nil)
	cookie := r.enrolled(t, newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin), r.invite(t, outsiderEmail), outsiderEmail)
	signedInRestricted(t, r.handler, cookie, outsiderEmail)
	if kind := r.memberKind(t, outsiderEmail); kind != string(model.MemberRestricted) {
		t.Fatalf("people.member_kind of %s = %q, want restricted", outsiderEmail, kind)
	}

	stranger := r.asOwner(http.MethodPost, "/api/v1/projects/PRIV/access/people/"+strangerEmail+"/invitations", nil)
	if stranger.Code == http.StatusCreated {
		_, token, _ := strings.Cut(stranger.Body.String(), "#")
		token, _, _ = strings.Cut(token, `"`)
		enrolled := r.enroll(t, r.handler, newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin), token)
		t.Fatalf("alice got a link for %s, whom no list names, and it enrolled: %d %s", strangerEmail, enrolled.Code, enrolled.Body.String())
	}
	wantCode(t, "alice's link for "+strangerEmail+", whom no list names", stranger, http.StatusConflict, "PERSON_NOT_LISTED")

	putGroup := r.asOwner(http.MethodPut, "/api/v1/access-groups/crew", map[string]any{"members": []string{strangerEmail}})
	if putGroup.Code != http.StatusOK {
		t.Fatalf("alice makes the group crew: %d %s", putGroup.Code, putGroup.Body.String())
	}
	r.listOn(t, "PRIV", access.PrincipalGroup, "crew")
	wantCode(t, "alice's link for "+strangerEmail+", whom only a group on PRIV's list holds",
		r.asOwner(http.MethodPost, "/api/v1/projects/PRIV/access/people/"+strangerEmail+"/invitations", nil), http.StatusConflict, "PERSON_NOT_LISTED")

	const later = "later@other.example"
	r.listOn(t, "PRIV", access.PrincipalPerson, later)
	token := r.invite(t, later)
	if _, err := r.database.Pool.Exec(context.Background(), `delete from project_access where principal = $1`, later); err != nil {
		t.Fatal(err)
	}
	wantCode(t, "the enrollment of a link whose email PRIV's list no longer names", r.beginEnrollment(r.handler, token), http.StatusForbidden, "INVITATION_NOT_LISTED")
	if n := r.passkeysOf(t, later); n != 0 {
		t.Fatalf("%s holds %d passkeys, want none", later, n)
	}
}

// TestTheInvitationIsSingleUseAndExpires (R5): a link enrolls once, 410 INVITATION_USED after;
// it lapses 24 hours after it was made, 410 INVITATION_EXPIRED; and a newer link for the same
// person ends the earlier one, 404 INVITATION_UNKNOWN, as a token no link holds is.
//
// Control: skip the store's used check (Invitation.Refusal answering nil for a used link), and the
// second enrollment with the first link is answered 200 and the row fails.
func TestTheInvitationIsSingleUseAndExpires(t *testing.T) {
	r := newPasskeyRig(t, nil)
	token := r.invite(t, outsiderEmail)
	r.enrolled(t, newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin), token, outsiderEmail)
	wantCode(t, "a second enrollment with the same link", r.enroll(t, r.handler, newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin), token), http.StatusGone, "INVITATION_USED")

	lapsed := r.invite(t, outsiderEmail)
	if _, err := r.database.Pool.Exec(context.Background(), `update outside_invitations set made_at = now() - interval '25 hours', expires_at = now() - interval '1 hour' where used_at is null`); err != nil {
		t.Fatal(err)
	}
	wantCode(t, "an enrollment with a link made 25 hours ago", r.beginEnrollment(r.handler, lapsed), http.StatusGone, "INVITATION_EXPIRED")

	first := r.invite(t, outsiderEmail)
	r.invite(t, outsiderEmail)
	wantCode(t, "an enrollment with a link a newer one replaced", r.beginEnrollment(r.handler, first), http.StatusNotFound, "INVITATION_UNKNOWN")
	wantCode(t, "an enrollment with a token no link holds", r.beginEnrollment(r.handler, strings.Repeat("A", 43)), http.StatusNotFound, "INVITATION_UNKNOWN")
	if n := r.passkeysOf(t, outsiderEmail); n != 1 {
		t.Fatalf("%s holds %d passkeys, want the first enrollment's alone", outsiderEmail, n)
	}
}

// TestAFullMembersLinkEnrollsNoPasskey (R5): a link made for a person who has since signed in with
// Google as a full member (their address on a domain of the Workspace that
// DISPATCH_SIGNIN_WORKSPACE_DOMAINS does not list) enrolls nothing: its begin is 409 FULL_MEMBER,
// as the owner's link for that address is now, so the enrollment page says the address signs in
// with Google rather than asking for a new link.
//
// Control: answer a full member's link as one no owner made (invitedPerson returning
// store.ErrInvitationUnknown), and the begin is 404 INVITATION_UNKNOWN and the row fails.
func TestAFullMembersLinkEnrollsNoPasskey(t *testing.T) {
	r := newPasskeyRig(t, nil)
	token := r.invite(t, outsiderEmail)
	sessionCookieOf(t, outsiderEmail+"'s Google sign-in as a Workspace account", codeFlowSignIn(t, r.handler, r.pool, map[string]any{
		"sub": "google-outsider", "email": outsiderEmail, "email_verified": true, "hd": "example.com",
	}))
	if kind := r.memberKind(t, outsiderEmail); kind != string(model.MemberFull) {
		t.Fatalf("people.member_kind of %s = %q, want full", outsiderEmail, kind)
	}
	wantCode(t, "the enrollment of a link made before its person signed in as a full member", r.beginEnrollment(r.handler, token), http.StatusConflict, "FULL_MEMBER")
	wantCode(t, "alice's link for a full member", r.asOwner(http.MethodPost, "/api/v1/projects/PRIV/access/people/"+outsiderEmail+"/invitations", nil), http.StatusConflict, "FULL_MEMBER")
	if n := r.passkeysOf(t, outsiderEmail); n != 0 {
		t.Fatalf("%s holds %d passkeys, want none", outsiderEmail, n)
	}
}

// TestAPasskeySignInWithALowerSignCountIsRefused (R5): a sign-in whose signature counter is above
// the stored one signs in and stores it; one whose counter is lower than the stored one is 401
// PASSKEY_SIGN_COUNT, sets no cookie and leaves the stored counter.
//
// Control: accept a counter that did not advance (identity.Passkeys.FinishSignIn ignoring the clone
// warning), and the lower counter signs in and the row fails.
func TestAPasskeySignInWithALowerSignCountIsRefused(t *testing.T) {
	r := newPasskeyRig(t, nil)
	device := newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin)
	device.count = 5
	r.enrolled(t, device, r.invite(t, outsiderEmail), outsiderEmail)
	if signedIn := r.signIn(t, r.handler, device); signedIn.Code != http.StatusOK {
		t.Fatalf("a sign-in at counter 6 answered %d %s, want 200", signedIn.Code, signedIn.Body.String())
	}
	device.count = 2
	refused := r.signIn(t, r.handler, device)
	wantCode(t, "a sign-in at counter 3, below the stored 6", refused, http.StatusUnauthorized, "PASSKEY_SIGN_COUNT")
	if sessionCookieSet(refused) {
		t.Fatal("the refused sign-in set a session cookie")
	}
	var stored int64
	if err := r.database.Pool.QueryRow(context.Background(), `select sign_count from passkeys where email = $1`, outsiderEmail).Scan(&stored); err != nil || stored != 6 {
		t.Fatalf("the stored counter is %d (%v) after the refusal, want 6", stored, err)
	}
}

// TestRemovingTheLastListingEndsSignInAndDeletesPasskeys (R5): taking the person off one list while
// another still names them changes nothing of their sign-in; taking them off the last list that
// names them ends the sign-in and deletes their passkeys in the removal's transaction, so their
// cookie is refused at its next use and their passkey signs in no more (401 PASSKEY_UNKNOWN).
//
// Control: skip the end on the last removal (endUnlistedPasskeySignIn answering nil), and the row
// fails naming the passkey still stored.
func TestRemovingTheLastListingEndsSignInAndDeletesPasskeys(t *testing.T) {
	r := newPasskeyRig(t, nil)
	if _, err := r.database.Pool.Exec(context.Background(), `insert into projects (key, name, default_assignee, owner) values ('OTHER', 'Other', $1, $1)`, privOwner); err != nil {
		t.Fatalf("seed OTHER: %v", err)
	}
	r.listOn(t, "OTHER", access.PrincipalPerson, outsiderEmail)
	device := newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin)
	cookie := r.enrolled(t, device, r.invite(t, outsiderEmail), outsiderEmail)

	if removed := r.asOwner(http.MethodDelete, "/api/v1/projects/OTHER/access/people/"+outsiderEmail, nil); removed.Code != http.StatusOK {
		t.Fatalf("alice takes the outsider off OTHER: %d %s", removed.Code, removed.Body.String())
	}
	signedInRestricted(t, r.handler, cookie, outsiderEmail)
	if n := r.passkeysOf(t, outsiderEmail); n != 1 {
		t.Fatalf("after a removal from OTHER, PRIV still naming them, %s holds %d passkeys, want 1", outsiderEmail, n)
	}

	if removed := r.asOwner(http.MethodDelete, "/api/v1/projects/PRIV/access/people/"+outsiderEmail, nil); removed.Code != http.StatusOK {
		t.Fatalf("alice takes the outsider off PRIV: %d %s", removed.Code, removed.Body.String())
	}
	if n := r.passkeysOf(t, outsiderEmail); n != 0 {
		t.Fatalf("after the removal from the last list, %s holds %d passkey(s), want every one deleted", outsiderEmail, n)
	}
	if got := signedOutOf(t, "whoami after the last removal", whoamiOutside(r.handler, cookie)); got.Code != "NO_IDENTITY" || got.Reason != "NOT_LISTED" {
		t.Fatalf("whoami after the last removal: %+v, want NO_IDENTITY NOT_LISTED", got)
	}
	wantCode(t, "a passkey sign-in after the last removal", r.signIn(t, r.handler, device), http.StatusUnauthorized, "PASSKEY_UNKNOWN")
}

// TestADelistedPersonLosesSignInAndPasskeysAtTheHourlyCheck (R5): a person the lists stop naming
// by any write but a removal keeps their sign-in until its confirmation is an hour old; the next
// request after it ends the sign-in and deletes their passkeys in one transaction, and says why
// (NOT_LISTED). A person the lists still name is confirmed again at the hour and stays.
//
// Control: confirm a passkey's sign-in without asking the rule (Membership.confirmLapsedPasskey
// skipping AdmitsRestricted), and the delisted person is still signed in and the row fails.
func TestADelistedPersonLosesSignInAndPasskeysAtTheHourlyCheck(t *testing.T) {
	r := newPasskeyRig(t, nil)
	const kept = "kept@other.example"
	r.listOn(t, "PRIV", access.PrincipalPerson, kept)
	keptCookie := r.enrolled(t, newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin), r.invite(t, kept), kept)
	cookie := r.enrolled(t, newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin), r.invite(t, outsiderEmail), outsiderEmail)

	if _, err := r.database.Pool.Exec(context.Background(), `delete from project_access where principal = $1`, outsiderEmail); err != nil {
		t.Fatal(err)
	}
	signedInRestricted(t, r.handler, cookie, outsiderEmail)
	r.agePasskeyConfirmation(t, outsiderEmail)
	r.agePasskeyConfirmation(t, kept)
	want := signedOutAnswer{Code: "NO_IDENTITY", Reason: "NOT_LISTED", Login: outsiderEmail}
	for _, read := range []string{"the whoami that ran the hourly check", "the whoami after it"} {
		if got := signedOutOf(t, read, whoamiOutside(r.handler, cookie)); got != want {
			t.Fatalf("%s, past the hour after the delisting: %+v, want %+v", read, got, want)
		}
	}
	if n := r.passkeysOf(t, outsiderEmail); n != 0 {
		t.Fatalf("after the hourly check %s holds %d passkey(s), want every one deleted", outsiderEmail, n)
	}
	signedInRestricted(t, r.handler, keptCookie, kept)
	if n := r.passkeysOf(t, kept); n != 1 {
		t.Fatalf("%s, whom PRIV still names, holds %d passkeys after the hour, want 1", kept, n)
	}
}

// TestTheOutsideOriginServesNoGoogleSignIn (R5): at the outside origin GET /auth/start and GET
// /auth/callback answer 404, as a path no route serves; at the canonical origin /auth/start still
// sends the browser to Google. TestTheOutsideOriginServesNoGoogleSignInWithPasskeysOff is the same
// row with passkey sign-in turned off, and TestTheOutsideOriginServesNoGoogleSignInWithPasskeysNeverSet
// with it never set.
//
// Control: serve the Google sign-in at both origins (googleSignInHere passing every request), and
// the outside origin's /auth/start answers 302 and the row fails.
func TestTheOutsideOriginServesNoGoogleSignIn(t *testing.T) {
	r := newPasskeyRig(t, nil)
	for _, path := range []string{"/auth/start", "/auth/callback?code=c&state=s"} {
		wantCode(t, "GET "+path+" at the outside origin", call(r.handler, passkeyOrigin, http.MethodGet, path, nil), http.StatusNotFound, "NOT_FOUND")
	}
	if started := call(r.handler, canonicalOrigin, http.MethodGet, "/auth/start", nil); started.Code != http.StatusFound {
		t.Fatalf("GET /auth/start at the canonical origin answered %d %s, want 302 to Google", started.Code, started.Body.String())
	}
}

// TestTheOutsideOriginIsItsHostHoweverSpelled (R5): a request whose Host names the outside origin's
// host with its default port (`:443`, `:0443`), with trailing dots, or in capitals, is answered as
// the outside origin: GET /auth/start is its 404, not the canonical origin's redirect to Google, and
// a passkey sign-in begins there. The canonical origin's own host still starts Google's sign-in.
//
// Control: compare the Host with the outside origin's host as written (publicOrigin's
// strings.EqualFold alone), and `/auth/start` at `outside.example.com:443` answers 302 and the row
// fails; strip one trailing dot and the port written `443` alone, and `outside.example.com..`
// answers 302 and the row fails.
func TestTheOutsideOriginIsItsHostHoweverSpelled(t *testing.T) {
	r := newPasskeyRig(t, nil)
	for _, host := range []string{
		passkeyRPID + ":443", passkeyRPID + ":0443", passkeyRPID + ".", passkeyRPID + "..",
		"OUTSIDE.Example.COM", "outside.example.com.:443",
	} {
		start := httptest.NewRequest(http.MethodGet, "https://"+host+"/auth/start", nil)
		started := httptest.NewRecorder()
		r.handler.ServeHTTP(started, start)
		wantCode(t, "GET /auth/start with Host "+host, started, http.StatusNotFound, "NOT_FOUND")
		begin := httptest.NewRequest(http.MethodPost, "https://"+host+"/auth/passkey/login/begin", strings.NewReader("{}"))
		begin.Header.Set("Content-Type", "application/json")
		begin.Header.Set("Origin", passkeyOrigin)
		begun := httptest.NewRecorder()
		r.handler.ServeHTTP(begun, begin)
		if begun.Code != http.StatusOK {
			t.Errorf("a sign-in's begin with Host %s answered %d %s, want 200, the outside origin's", host, begun.Code, begun.Body.String())
		}
	}
	if started := call(r.handler, canonicalOrigin, http.MethodGet, "/auth/start", nil); started.Code != http.StatusFound {
		t.Fatalf("GET /auth/start at the canonical origin answered %d %s, want 302 to Google", started.Code, started.Body.String())
	}
}

// TestTheOutsideOriginServesNoGoogleSignInWithPasskeysOff (R5): with DISPATCH_SIGNIN_PASSKEY unset
// and DISPATCH_OUTSIDE_ORIGIN still set on the same database (a rollback, or an outside origin
// kept for agents), GET /auth/start and GET /auth/callback still answer 404 at the outside origin:
// no deployment signs anyone in with Google there. The canonical origin still sends the browser to
// Google.
//
// Control: refuse Google at the outside origin only while passkey sign-in is on (googleSignInHere
// reading Passkeys), and the outside origin's /auth/start answers 302 and the row fails.
func TestTheOutsideOriginServesNoGoogleSignInWithPasskeysOff(t *testing.T) {
	r := newPasskeyRig(t, nil)
	off, _ := r.serverWith(t, map[string]string{"DISPATCH_SIGNIN_PASSKEY": ""})
	for _, path := range []string{"/auth/start", "/auth/callback?code=c&state=s"} {
		wantCode(t, "GET "+path+" at the outside origin with passkeys off", call(off, passkeyOrigin, http.MethodGet, path, nil), http.StatusNotFound, "NOT_FOUND")
	}
	if started := call(off, canonicalOrigin, http.MethodGet, "/auth/start", nil); started.Code != http.StatusFound {
		t.Fatalf("GET /auth/start at the canonical origin with passkeys off answered %d %s, want 302 to Google", started.Code, started.Body.String())
	}
}

// TestAGatedAuthRouteLogsOneLine: a request one of the outside origin's gates answers 404 (Google's
// /auth/start at the outside origin, a passkey ceremony at the tailnet origin) writes one
// access-log line, its route's own, as every request does: the gate answers the 404 a path no
// route serves without a second, unmatched, line.
//
// Control: answer the gate's 404 through staticHandler, which logs an unmatched line of its own,
// and each request writes two lines and the row fails naming them.
func TestAGatedAuthRouteLogsOneLine(t *testing.T) {
	r := newPasskeyRig(t, nil)
	logged := &logtest.Buffer{}
	logtest.SetDefault(t, slog.NewTextHandler(logged, nil))
	for _, gated := range []struct {
		origin, method, path string
		body                 any
	}{
		{passkeyOrigin, http.MethodGet, "/auth/start", nil},
		{canonicalOrigin, http.MethodPost, "/auth/passkey/login/begin", map[string]string{}},
	} {
		before := logged.Len()
		what := gated.method + " " + gated.path + " at " + gated.origin
		wantCode(t, what, call(r.handler, gated.origin, gated.method, gated.path, gated.body), http.StatusNotFound, "NOT_FOUND")
		written := logged.String()[before:]
		if lines := strings.Count(written, `msg="dispatch: request"`); lines != 1 {
			t.Errorf("%s wrote %d access-log lines, want its route's one:\n%s", what, lines, written)
		}
	}
}

// passkeySettingReachesItsReaders hands DISPATCH_SIGNIN_PASSKEY, through the settings read alone, to
// each reader: boot, which takes 1 and refuses any other value naming the setting; the sign-in rule,
// a PasskeyRule with it on and the domain rule with it off (signInRule); and the router, which
// serves the passkey ceremonies at the outside origin with it on, a sign-in's begin answering 200,
// and answers that begin 404 with it off, on the same database.
func passkeySettingReachesItsReaders(t *testing.T) {
	r := newPasskeyRig(t, nil)
	if !r.boot.SignInPasskey {
		t.Fatal("DISPATCH_SIGNIN_PASSKEY=1: SignInPasskey false")
	}
	if rule, err := signInRule(r.boot, accessLister(r.database)); err != nil {
		t.Fatalf("signInRule with DISPATCH_SIGNIN_PASSKEY=1: %v", err)
	} else if _, ok := rule.(identity.PasskeyRule); !ok {
		t.Errorf("signInRule with DISPATCH_SIGNIN_PASSKEY=1 = %T, want identity.PasskeyRule", rule)
	}
	if begun := call(r.handler, passkeyOrigin, http.MethodPost, "/auth/passkey/login/begin", map[string]string{}); begun.Code != http.StatusOK {
		t.Errorf("DISPATCH_SIGNIN_PASSKEY=1: a sign-in's begin at the outside origin answered %d %s, want 200", begun.Code, begun.Body.String())
	}
	off, offBoot := r.serverWith(t, map[string]string{"DISPATCH_SIGNIN_PASSKEY": ""})
	if offBoot.SignInPasskey {
		t.Error("DISPATCH_SIGNIN_PASSKEY unset: SignInPasskey true")
	}
	if rule, err := signInRule(offBoot, accessLister(r.database)); err != nil {
		t.Fatalf("signInRule with DISPATCH_SIGNIN_PASSKEY unset: %v", err)
	} else if _, ok := rule.(identity.DomainRule); !ok {
		t.Errorf("signInRule with DISPATCH_SIGNIN_PASSKEY unset = %T, want identity.DomainRule", rule)
	}
	wantCode(t, "DISPATCH_SIGNIN_PASSKEY unset: a sign-in's begin at the outside origin", call(off, passkeyOrigin, http.MethodPost, "/auth/passkey/login/begin", map[string]string{}), http.StatusNotFound, "NOT_FOUND")
	refusedWith(t, map[string]string{
		"DISPATCH_SIGNIN_ISSUER": "https://accounts.google.com", "DISPATCH_SIGNIN_GROUP": "", "DISPATCH_SIGNIN_DOMAIN": "example.com",
		"DISPATCH_OUTSIDE_ORIGIN": passkeyOrigin, "DISPATCH_SIGNIN_PASSKEY": "true",
	}, "DISPATCH_SIGNIN_PASSKEY must be 1 or unset")
}

// TestTheTailnetOriginServesNoPasskeyRoute (R5): at the canonical origin the four passkey routes
// answer 404, as a path no route serves; at the outside origin a sign-in's begin answers 200.
//
// Control: serve the ceremonies at both origins (atOutsideOrigin passing every request), and the
// canonical origin's login begin answers 200 and the row fails.
func TestTheTailnetOriginServesNoPasskeyRoute(t *testing.T) {
	r := newPasskeyRig(t, nil)
	for _, path := range []string{"/auth/passkey/register/begin", "/auth/passkey/register/finish", "/auth/passkey/login/begin", "/auth/passkey/login/finish"} {
		wantCode(t, "POST "+path+" at the canonical origin", call(r.handler, canonicalOrigin, http.MethodPost, path, map[string]string{}), http.StatusNotFound, "NOT_FOUND")
	}
	if begun := call(r.handler, passkeyOrigin, http.MethodPost, "/auth/passkey/login/begin", map[string]string{}); begun.Code != http.StatusOK {
		t.Fatalf("the sign-in's begin at the outside origin answered %d %s, want 200", begun.Code, begun.Body.String())
	}
}

// TestTurningPasskeysOffEndsEveryRestrictedCredential (E-I5's rollback): with
// DISPATCH_SIGNIN_PASSKEY unset on the same database, a restricted member's cookie and personal
// token are each refused at their next use, the rule answers no restricted member
// (AdmitsRestricted: OUTSIDE_SIGNIN_OFF), and the passkey ceremonies answer 404. A broker token
// naming them is held to the same answer as the personal token (api.restrictedOwnerAdmitted).
//
// Control: confirm a passkey's sign-in with no passkey store (Membership.confirmPasskey not
// refusing one), and the cookie is still admitted and the row fails.
func TestTurningPasskeysOffEndsEveryRestrictedCredential(t *testing.T) {
	r := newPasskeyRig(t, nil)
	cookie := r.enrolled(t, newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin), r.invite(t, outsiderEmail), outsiderEmail)
	minted := call(r.handler, passkeyOrigin, http.MethodPost, "/api/v1/me/agent-tokens", map[string]string{"name": "outsider's agent"}, cookie)
	var token struct {
		Token string `json:"token"`
	}
	if minted.Code != http.StatusCreated || json.Unmarshal(minted.Body.Bytes(), &token) != nil || token.Token == "" {
		t.Fatalf("the outsider mints a personal token at their browser: %d %s", minted.Code, minted.Body.String())
	}
	bearer := func(handler http.Handler) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, passkeyOrigin+"/api/v1/projects", nil)
		request.Header.Set("Authorization", "Bearer "+token.Token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	if read := bearer(r.handler); read.Code != http.StatusOK {
		t.Fatalf("the outsider's token with passkeys on answered %d %s, want 200", read.Code, read.Body.String())
	}

	off, offBoot := r.serverWith(t, map[string]string{"DISPATCH_SIGNIN_PASSKEY": ""})
	if whoami := whoamiOutside(off, cookie); whoami.Code != http.StatusUnauthorized {
		t.Fatalf("the outsider's cookie with passkeys off answered %d %s, want 401", whoami.Code, whoami.Body.String())
	}
	wantCode(t, "the outsider's personal token with passkeys off", bearer(off), http.StatusUnauthorized, "TOKEN_OWNER_NOT_ADMITTED")
	for _, path := range []string{"/auth/passkey/register/begin", "/auth/passkey/login/begin"} {
		wantCode(t, "POST "+path+" with passkeys off", call(off, passkeyOrigin, http.MethodPost, path, map[string]string{}), http.StatusNotFound, "NOT_FOUND")
	}
	rule, err := signInRule(offBoot, accessLister(r.database))
	if err != nil {
		t.Fatal(err)
	}
	if err := rule.AdmitsRestricted(context.Background(), outsiderEmail); err == nil || !strings.Contains(err.Error(), "nobody from outside") {
		t.Fatalf("the rule with passkeys off admits %s: %v, want ErrOutsideSignInOff", outsiderEmail, err)
	}
}

// TestTurningPasskeysOffTellsThePersonWhy (E-I5's rollback): with DISPATCH_SIGNIN_PASSKEY unset on
// the same database, the passkey member's cookie is refused 401 NO_IDENTITY with reason
// OUTSIDE_SIGNIN_OFF and their address as login, at every request, so the sign-in page says
// Dispatch no longer signs them in.
//
// Control: tell a passkey's row only by whether its sign-in stands (DomainRule.SignedOutReason
// reading endedRestricted alone), and the 401 names no reason and the row fails.
func TestTurningPasskeysOffTellsThePersonWhy(t *testing.T) {
	r := newPasskeyRig(t, nil)
	cookie := r.enrolled(t, newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin), r.invite(t, outsiderEmail), outsiderEmail)
	off, _ := r.serverWith(t, map[string]string{"DISPATCH_SIGNIN_PASSKEY": ""})
	want := signedOutAnswer{Code: "NO_IDENTITY", Reason: "OUTSIDE_SIGNIN_OFF", Login: outsiderEmail}
	for _, read := range []string{"the first whoami with passkeys off", "the whoami after it"} {
		if got := signedOutOf(t, read, whoamiOutside(off, cookie)); got != want {
			t.Fatalf("%s: %+v, want %+v", read, got, want)
		}
	}
}

// TestPasskeyCeremonyIsTakenOnce: a ceremony's answer is used once. The same finish sent again,
// cookie and body alike, is 400 CEREMONY_EXPIRED and signs nobody in, even for a passkey whose
// signature counter stays 0, which the counter check cannot tell from a first use.
//
// Control: record the used ceremony without its unique key (store's useCeremonyTx answering nil
// when the insert found the challenge taken), and the replayed finish signs in and the row fails.
func TestPasskeyCeremonyIsTakenOnce(t *testing.T) {
	r := newPasskeyRig(t, nil)
	device := newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin)
	r.enrolled(t, device, r.invite(t, outsiderEmail), outsiderEmail)
	begun := call(r.handler, passkeyOrigin, http.MethodPost, "/auth/passkey/login/begin", map[string]string{})
	answer := device.assertAt(t, begun.Body.Bytes())
	if first := call(r.handler, passkeyOrigin, http.MethodPost, "/auth/passkey/login/finish", answer, ceremonyCookie(begun)); first.Code != http.StatusOK {
		t.Fatalf("the sign-in at counter 0 answered %d %s, want 200", first.Code, first.Body.String())
	}
	replayed := call(r.handler, passkeyOrigin, http.MethodPost, "/auth/passkey/login/finish", answer, ceremonyCookie(begun))
	wantCode(t, "the same finish sent again", replayed, http.StatusBadRequest, "CEREMONY_EXPIRED")
	if sessionCookieSet(replayed) {
		t.Fatal("the replayed finish set a session cookie")
	}
}

// TestAReplayAfterTheDatabasesClockRanAheadIsRefused: a used ceremony's row is kept a minute past
// when it lapses by the database's clock, so a database clock that runs ahead of the process's,
// which accepts the ceremony's cookie until it lapses by its own, deletes no used row while its
// cookie still verifies: the replay of a sign-in whose row the database already counts 30 seconds
// lapsed is 400 CEREMONY_EXPIRED.
//
// Control: delete the used rows as soon as they lapse by the database's clock (useCeremonyTx
// without its margin), and the replay signs in, 200, and the row fails.
func TestAReplayAfterTheDatabasesClockRanAheadIsRefused(t *testing.T) {
	r := newPasskeyRig(t, nil)
	device := newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin)
	r.enrolled(t, device, r.invite(t, outsiderEmail), outsiderEmail)
	begun := call(r.handler, passkeyOrigin, http.MethodPost, "/auth/passkey/login/begin", map[string]string{})
	answer := device.assertAt(t, begun.Body.Bytes())
	if first := call(r.handler, passkeyOrigin, http.MethodPost, "/auth/passkey/login/finish", answer, ceremonyCookie(begun)); first.Code != http.StatusOK {
		t.Fatalf("the sign-in at counter 0 answered %d %s, want 200", first.Code, first.Body.String())
	}
	if _, err := r.database.Pool.Exec(context.Background(), `update passkey_ceremonies set expires_at = now() - interval '30 seconds'`); err != nil {
		t.Fatal(err)
	}
	replayed := call(r.handler, passkeyOrigin, http.MethodPost, "/auth/passkey/login/finish", answer, ceremonyCookie(begun))
	wantCode(t, "the same finish sent again, its row lapsed 30 seconds by the database's clock", replayed, http.StatusBadRequest, "CEREMONY_EXPIRED")
}

// TestPasskeyRefusesAWorkspaceAddress: no link is made, and no passkey is enrolled, for an
// address on a domain of the company's Workspace: the sign-in domain, each domain
// DISPATCH_SIGNIN_WORKSPACE_DOMAINS lists (the shapes a Workspace's domains take: other top-level
// domains, a domain under a guest suffix, a test domain under another's name), or a subdomain of
// one, 400 WORKSPACE_ADDRESS. A link made before the person's domain was listed enrolls nothing
// once it is (403 WORKSPACE_ADDRESS), and a passkey enrolled before signs in no more and is
// deleted.
//
// Control: answer no address as the Workspace's (the rule's WorkspaceAddress answering false), and
// the first link is made and the row fails naming its address.
func TestPasskeyRefusesAWorkspaceAddress(t *testing.T) {
	domains := "example.ai,examplelabs.com,example.com.guest.example,example.ai.test-example-a.com"
	r := newPasskeyRig(t, map[string]string{"DISPATCH_SIGNIN_WORKSPACE_DOMAINS": domains})
	for _, address := range []string{
		"bob@example.com", "bob@mail.example.com", "bob@example.ai", "bob@examplelabs.com",
		"bob@example.com.guest.example", "bob@example.ai.test-example-a.com", "bob@EXAMPLE.ai",
	} {
		listed := strings.ToLower(address)
		r.listOn(t, "PRIV", access.PrincipalPerson, listed)
		wantCode(t, "alice's link for "+listed, r.asOwner(http.MethodPost, "/api/v1/projects/PRIV/access/people/"+listed+"/invitations", nil), http.StatusBadRequest, "WORKSPACE_ADDRESS")
	}

	const moved = "carl@acquired.example"
	r.listOn(t, "PRIV", access.PrincipalPerson, moved)
	device := newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin)
	r.enrolled(t, device, r.invite(t, moved), moved)
	pending := r.invite(t, outsiderEmail)
	listedServer, _ := r.serverWith(t, map[string]string{"DISPATCH_SIGNIN_WORKSPACE_DOMAINS": domains + ",acquired.example,other.example"})
	wantCode(t, "an enrollment with a link made before its domain was the Workspace's", r.beginEnrollment(listedServer, pending), http.StatusForbidden, "WORKSPACE_ADDRESS")
	wantCode(t, "a passkey sign-in once the address's domain is the Workspace's", r.signIn(t, listedServer, device), http.StatusForbidden, "WORKSPACE_ADDRESS")
	if n := r.passkeysOf(t, moved); n != 0 {
		t.Fatalf("%s holds %d passkeys once its domain is the Workspace's, want every one deleted", moved, n)
	}
}

// TestPasskeyWhoamiNamesThePasskeySignIn: a 401 of GET /auth/whoami at the outside origin carries
// "sign_in":"passkey", so its page offers the passkey and no Google sign-in; at the canonical
// origin the 401 is byte for byte the one Dispatch always answered.
//
// Control: name the passkey sign-in at both origins (whoamiSignIn ignoring the origin), and the
// canonical origin's 401 carries it and the row fails.
func TestPasskeyWhoamiNamesThePasskeySignIn(t *testing.T) {
	r := newPasskeyRig(t, nil)
	outside := whoamiOutside(r.handler, nil)
	if outside.Code != http.StatusUnauthorized || !strings.Contains(outside.Body.String(), `"sign_in":"passkey"`) {
		t.Fatalf("whoami at the outside origin with no cookie: %d %s, want 401 naming the passkey sign-in", outside.Code, outside.Body.String())
	}
	requirePlainNoIdentity(t, "whoami at the canonical origin with no cookie", call(r.handler, canonicalOrigin, http.MethodGet, "/auth/whoami", nil))
}

// TestTheOutsideOriginsWhoamiNamesThePasskeySignInWithPasskeysOff: the outside origin serves no
// Google sign-in whatever the passkey setting (googleSignInHere), so its whoami 401 names the
// passkey sign-in with DISPATCH_SIGNIN_PASSKEY turned off on a database that had it on, as with it
// on, and its page offers no Google link that answers 404 there; at the canonical origin the 401 is
// the one Dispatch always answered.
//
// Control: name the passkey sign-in only while passkey sign-in is on (whoamiSignIn reading
// Passkeys), and the outside origin's 401 names no sign-in and the row fails.
func TestTheOutsideOriginsWhoamiNamesThePasskeySignInWithPasskeysOff(t *testing.T) {
	r := newPasskeyRig(t, nil)
	off, _ := r.serverWith(t, map[string]string{"DISPATCH_SIGNIN_PASSKEY": ""})
	outside := call(off, passkeyOrigin, http.MethodGet, "/auth/whoami", nil)
	if outside.Code != http.StatusUnauthorized || !strings.Contains(outside.Body.String(), `"sign_in":"passkey"`) {
		t.Fatalf("whoami at the outside origin with passkeys off and no cookie: %d %s, want 401 naming the passkey sign-in", outside.Code, outside.Body.String())
	}
	requirePlainNoIdentity(t, "whoami at the canonical origin with passkeys off and no cookie", call(off, canonicalOrigin, http.MethodGet, "/auth/whoami", nil))
}

// TestPasskeyCeremonyRefusesAnotherOriginsPage: a ceremony posted by a page of another origin,
// the canonical one included, is 403 at the outside origin and begins nothing.
//
// Control: take any Origin (atOutsideOrigin skipping its check), and the canonical origin's page
// begins a sign-in and the row fails.
func TestPasskeyCeremonyRefusesAnotherOriginsPage(t *testing.T) {
	r := newPasskeyRig(t, nil)
	for _, origin := range []string{canonicalOrigin, "https://elsewhere.example"} {
		request := httptest.NewRequest(http.MethodPost, passkeyOrigin+"/auth/passkey/login/begin", strings.NewReader("{}"))
		request.Header.Set("Origin", origin)
		response := httptest.NewRecorder()
		r.handler.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden || ceremonyCookie(response) != nil {
			t.Fatalf("a login begin from %s's page answered %d %s, want 403 and no ceremony", origin, response.Code, response.Body.String())
		}
	}
}

// TestPasskeySettingsRefusals: DISPATCH_SIGNIN_PASSKEY=1 refuses startup without the domain,
// without the outside origin, under IAP identity and beside header identity, each naming the
// setting; any value but 1 refuses too.
//
// Control: drop the refusal of a passkey sign-in with no outside origin (checkSignInSettings), and
// the boot without DISPATCH_OUTSIDE_ORIGIN starts and the row fails naming it.
func TestPasskeySettingsRefusals(t *testing.T) {
	google := map[string]string{"DISPATCH_SIGNIN_ISSUER": "https://accounts.google.com", "DISPATCH_SIGNIN_GROUP": "", "DISPATCH_SIGNIN_DOMAIN": "example.com"}
	with := func(extra map[string]string) map[string]string {
		values := maps.Clone(google)
		maps.Copy(values, extra)
		return values
	}
	refusedWith(t, with(map[string]string{"DISPATCH_SIGNIN_PASSKEY": "1", "DISPATCH_SIGNIN_DOMAIN": "", "DISPATCH_SIGNIN_GROUP": ""}), "DISPATCH_SIGNIN_PASSKEY needs DISPATCH_SIGNIN_DOMAIN")
	refusedWith(t, with(map[string]string{"DISPATCH_SIGNIN_PASSKEY": "1"}), "DISPATCH_SIGNIN_PASSKEY needs DISPATCH_OUTSIDE_ORIGIN")
	refusedWith(t, with(map[string]string{"DISPATCH_SIGNIN_PASSKEY": "yes", "DISPATCH_OUTSIDE_ORIGIN": passkeyOrigin}), "DISPATCH_SIGNIN_PASSKEY must be 1 or unset")
	refusedWith(t, map[string]string{
		"DISPATCH_IDENTITY": "iap", "DISPATCH_IAP_AUDIENCE": "/projects/1/global/backendServices/2", "DISPATCH_SIGNIN_DOMAIN": "example.com",
		"DISPATCH_SIGNIN_ISSUER": "", "DISPATCH_SIGNIN_CLIENT_ID": "", "DISPATCH_SIGNIN_CLIENT_SECRET": "", "DISPATCH_SIGNIN_GROUP": "",
		"DISPATCH_SIGNIN_PASSKEY": "1",
	}, "DISPATCH_SIGNIN_PASSKEY")
	refusedWith(t, map[string]string{
		"DISPATCH_IDENTITY": "header:X-Dispatch-User", "DISPATCH_IDENTITY_HEADER_TRUSTED": "1",
		"DISPATCH_SIGNIN_ISSUER": "", "DISPATCH_SIGNIN_CLIENT_ID": "", "DISPATCH_SIGNIN_CLIENT_SECRET": "", "DISPATCH_SIGNIN_GROUP": "", "DISPATCH_SIGNIN_DOMAIN": "",
		"DISPATCH_SIGNIN_PASSKEY": "1",
	}, "DISPATCH_SIGNIN_PASSKEY")
}

// TestThePasskeyBeginsAreBoundedPerAddress: a ceremony's begin takes no credential, so one client
// address, as the load balancer names it, gets ten at once and then one a second: the eleventh at
// once is 429 PASSKEY_RATE_LIMITED with Retry-After, while another address still begins.
//
// Control: mount the begins without limitPasskeyBegins, and the eleventh begin answers 200 and the
// row fails.
func TestThePasskeyBeginsAreBoundedPerAddress(t *testing.T) {
	r := newPasskeyRig(t, nil)
	begin := func(address string) *httptest.ResponseRecorder {
		return beginThrough(r.handler, balancerPeer, address+", 35.191.0.10")
	}
	for n := range 10 {
		if begun := begin("198.51.100.7"); begun.Code != http.StatusOK {
			t.Fatalf("begin %d from 198.51.100.7 answered %d %s, want 200", n+1, begun.Code, begun.Body.String())
		}
	}
	limited := begin("198.51.100.7")
	wantCode(t, "the eleventh begin from 198.51.100.7 at once", limited, http.StatusTooManyRequests, "PASSKEY_RATE_LIMITED")
	if limited.Header().Get("Retry-After") == "" {
		t.Errorf("the 429 carries no Retry-After")
	}
	if other := begin("198.51.100.8"); other.Code != http.StatusOK {
		t.Fatalf("a begin from 198.51.100.8 answered %d %s, want 200", other.Code, other.Body.String())
	}
}

// TestTheCeremonyCookieIsTheHostsAlone: the ceremony cookie is `__Host-dispatch_passkey_ceremony`,
// Secure, HttpOnly, SameSite=Strict, with Path=/ and no Domain, which a browser takes only from the
// outside origin's own host, so no other host under its domain can plant a ceremony; a finish
// carrying the same value under the plain name is 400 CEREMONY_EXPIRED. Under
// DISPATCH_INSECURE_COOKIE, which a browser refuses a `__Host-` cookie with, it is the plain name,
// not Secure.
//
// Control: name the cookie `dispatch_passkey_ceremony` with Path=/auth/passkey, and the begin sets
// no `__Host-` cookie and the row fails.
func TestTheCeremonyCookieIsTheHostsAlone(t *testing.T) {
	r := newPasskeyRig(t, nil)
	device := newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin)
	begun := r.beginEnrollment(r.handler, r.invite(t, outsiderEmail))
	cookie := ceremonyCookie(begun)
	if begun.Code != http.StatusOK || cookie == nil || !cookie.Secure || !cookie.HttpOnly || cookie.Path != "/" || cookie.Domain != "" || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("the begin answered %d and set %+v, want a Secure, HttpOnly, SameSite=Strict __Host- cookie with Path=/ and no Domain", begun.Code, begun.Result().Cookies())
	}
	plain := &http.Cookie{Name: "dispatch_passkey_ceremony", Value: cookie.Value}
	wantCode(t, "a finish carrying the ceremony under the plain name", call(r.handler, passkeyOrigin, http.MethodPost, "/auth/passkey/register/finish", device.register(t, begun.Body.Bytes()), plain), http.StatusBadRequest, "CEREMONY_EXPIRED")

	insecure, _ := r.serverWith(t, map[string]string{"DISPATCH_INSECURE_COOKIE": "1"})
	set := call(insecure, passkeyOrigin, http.MethodPost, "/auth/passkey/login/begin", map[string]string{}).Result().Cookies()
	if len(set) != 1 || set[0].Name != "dispatch_passkey_ceremony" || set[0].Secure || set[0].Path != "/" {
		t.Fatalf("the begin under DISPATCH_INSECURE_COOKIE set %+v, want one plain dispatch_passkey_ceremony cookie, not Secure, with Path=/", set)
	}
}

// balancerPeer is a connection's address in Google's external load balancer's ranges, whose
// X-Forwarded-For a begin's bound reads.
const balancerPeer = "35.191.0.10:443"

// beginThrough is a sign-in's begin at the outside origin over a connection from peer, carrying
// the X-Forwarded-For lines forwarded.
func beginThrough(handler http.Handler, peer string, forwarded ...string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, passkeyOrigin+"/auth/passkey/login/begin", strings.NewReader("{}"))
	request.RemoteAddr = peer
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", passkeyOrigin)
	for _, line := range forwarded {
		request.Header.Add("X-Forwarded-For", line)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// TestABeginOffTheBalancerIsBoundedByItsConnection: a begin whose connection does not come from
// the load balancer (a caller on the tailnet reaching the outside origin's backend) is bounded by
// its connection's own address, whatever X-Forwarded-For it writes: eleven begins at once from one
// connection, each naming another address, are ten served and one 429 PASSKEY_RATE_LIMITED.
//
// Control: bound such a begin by auth.ForwardedAddress (the rightmost entry the caller wrote), and
// the eleventh begin answers 200 and the row fails.
func TestABeginOffTheBalancerIsBoundedByItsConnection(t *testing.T) {
	r := newPasskeyRig(t, nil)
	for n := range 10 {
		if begun := beginThrough(r.handler, "100.64.0.9:51000", fmt.Sprintf("198.51.100.%d", n)); begun.Code != http.StatusOK {
			t.Fatalf("begin %d from one connection answered %d %s, want 200", n+1, begun.Code, begun.Body.String())
		}
	}
	wantCode(t, "the eleventh begin at once from one connection, naming another address", beginThrough(r.handler, "100.64.0.9:51000", "198.51.100.200"), http.StatusTooManyRequests, "PASSKEY_RATE_LIMITED")
}

// TestAFloodOfBeginsFromManyAddressesLeavesAFreshAddressServed: 400 begins at once from 400
// addresses through the load balancer do not refuse the person's begins, each from a fresh
// address: the begins share no bound, only each address's own, so a stranger with many addresses
// cannot lock the person out of signing in or enrolling.
//
// Control: bound every address together as well (a route-wide bucket of 50 a second, 200 at once,
// as before), and the person's begin after the flood is 429 PASSKEY_RATE_LIMITED and the row fails.
func TestAFloodOfBeginsFromManyAddressesLeavesAFreshAddressServed(t *testing.T) {
	r := newPasskeyRig(t, nil)
	for n := range 400 {
		beginThrough(r.handler, balancerPeer, fmt.Sprintf("198.18.%d.%d, 35.191.0.10", n/250, n%250))
	}
	for n := range 20 {
		if begun := beginThrough(r.handler, balancerPeer, fmt.Sprintf("203.0.113.%d, 35.191.0.10", n)); begun.Code != http.StatusOK {
			t.Fatalf("the person's begin %d from a fresh address after the flood answered %d %s, want 200", n+1, begun.Code, begun.Body.String())
		}
	}
}

// TestAFullAddressMapLeavesAFreshAddressServed: 10,000 strangers' addresses, one begin each
// through the load balancer, fill the bound's memory of addresses; the person's begin from an
// address it does not hold is still served, so no number of strangers' addresses refuses it.
//
// Control: refuse a new address while the map is full (passkeyBeginLimiter.allow answering false
// at the cap, as at aa6950fcf1), and the person's begin is 429 PASSKEY_RATE_LIMITED and the row
// fails.
func TestAFullAddressMapLeavesAFreshAddressServed(t *testing.T) {
	r := newPasskeyRig(t, nil)
	for n := range 10000 {
		beginThrough(r.handler, balancerPeer, fmt.Sprintf("198.18.%d.%d, 35.191.0.10", n/250, n%250))
	}
	if begun := beginThrough(r.handler, balancerPeer, "203.0.113.77, 35.191.0.10"); begun.Code != http.StatusOK {
		t.Fatalf("the person's begin from a fresh address with the map full answered %d %s, want 200", begun.Code, begun.Body.String())
	}
}

// TestTheBeginsWriteNothing: a ceremony's begin, which takes no credential, writes no row: the
// signed ceremony cookie carries what its finish needs, and the finish records the challenge as
// used only after the answer verifies (store.PgPasskeys.SignIn and Enroll), so a flood of begins
// costs the database nothing.
//
// Control: record each begin's ceremony in passkey_ceremonies, as the begins did before this row,
// and the table holds a row per begin and the row fails.
func TestTheBeginsWriteNothing(t *testing.T) {
	r := newPasskeyRig(t, nil)
	for range 3 {
		if begun := beginThrough(r.handler, balancerPeer); begun.Code != http.StatusOK {
			t.Fatalf("a sign-in's begin answered %d %s, want 200", begun.Code, begun.Body.String())
		}
	}
	if begun := r.beginEnrollment(r.handler, r.invite(t, outsiderEmail)); begun.Code != http.StatusOK {
		t.Fatalf("an enrollment's begin answered %d %s, want 200", begun.Code, begun.Body.String())
	}
	var rows int
	if err := r.database.Pool.QueryRow(context.Background(), `select count(*) from passkey_ceremonies`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("after four begins passkey_ceremonies holds %d rows, want none", rows)
	}
}

// TestABeginWithTwoForwardedForLinesIsRefused: Google's load balancer in front of the outside
// origin writes X-Forwarded-For as one line, so a begin through it carrying two is no request it
// forwarded as it writes them: it is 400 FORWARDED_FOR_INVALID, rather than bounded by the
// balancer's own address, which every such request would share, while a begin with one line is
// served.
//
// Control: bound the begin by auth.ForwardedAddress whatever its lines (auth.BoundAddress without
// the line count), and the two-line begin answers 200 and the row fails.
func TestABeginWithTwoForwardedForLinesIsRefused(t *testing.T) {
	r := newPasskeyRig(t, nil)
	wantCode(t, "a begin through the balancer with two X-Forwarded-For lines", beginThrough(r.handler, balancerPeer, "198.51.100.9", "203.0.113.7, 35.191.0.10"), http.StatusBadRequest, "FORWARDED_FOR_INVALID")
	if begun := beginThrough(r.handler, balancerPeer, "203.0.113.7, 35.191.0.10"); begun.Code != http.StatusOK {
		t.Fatalf("a begin with one X-Forwarded-For line answered %d %s, want 200", begun.Code, begun.Body.String())
	}
}
