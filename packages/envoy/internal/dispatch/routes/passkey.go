package routes

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/readclass"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The passkey ceremonies of a restricted member, served at the outside origin alone and only where
// passkey sign-in is on (AppContextOptions.Passkeys): POST /auth/passkey/register/begin takes an
// owner's link and answers the options navigator.credentials.create takes, POST
// /auth/passkey/register/finish takes what it made, enrolls the passkey with the link and signs the
// person in, and /auth/passkey/login/begin and /login/finish sign in with a passkey already
// enrolled. At the canonical (tailnet) origin each answers 404 exactly as a path no route serves
// does. At the outside origin GET /auth/start and GET /auth/callback answer that 404 too, with
// passkey sign-in on or off (googleSignInHere), so a restricted member signs in there with a passkey
// alone. These are the only routes that do work for a caller with no credential besides the Google
// sign-in's two.
const (
	// passkeyCeremonyCookie is the ceremony cookie's name, with the `__Host-` prefix, which a
	// browser takes only from this host itself, Secure, with Path=/ and no Domain: so no other host
	// under the outside origin's domain can set a ceremony for this one. Under
	// DISPATCH_INSECURE_COOKIE (plain http, which a browser refuses such a cookie over) it is
	// passkeyCeremonyCookieInsecure (passkeyCeremonyCookieName).
	passkeyCeremonyCookie         = "__Host-dispatch_passkey_ceremony"
	passkeyCeremonyCookieInsecure = "dispatch_passkey_ceremony"
	// passkeyCeremonyMaxAge is how long the ceremony cookie lives, in seconds: the ceremony's own
	// timeout, which identity.Passkeys enforces again from the session it carries.
	passkeyCeremonyMaxAge = int(identity.PasskeyCeremonyTimeout / time.Second)
	// passkeyCeremonyFormat leads every ceremony cookie value, so a later shape can be told from
	// this one.
	passkeyCeremonyFormat = "p1"
	// passkeyCeremonyPurpose is the purpose the ceremony cookie is signed for
	// (auth.SigningKeys.SignFor), so it never verifies as a session cookie or an OAuth state.
	passkeyCeremonyPurpose = "dispatch passkey ceremony v1"
	// maxPasskeyBody bounds a ceremony's request body: an enrollment's answer with no attestation
	// statement is well under a kilobyte, and a credential id is at most 1023 bytes.
	maxPasskeyBody = 64 << 10
	// passkeySignIn is what GET /auth/whoami's 401 names as the outside origin's way of signing in.
	passkeySignIn = "passkey"
)

// mountPasskeys mounts the four ceremonies on mux where passkey sign-in is on; the two begins,
// which write for a caller with no credential, under the begins' bounds (limitPasskeyBegins).
func (r *router) mountPasskeys(mux *http.ServeMux) {
	if r.ctx.Passkeys == nil {
		return
	}
	none := []readclass.Class{readclass.None}
	for path, handler := range map[string]http.HandlerFunc{
		"/auth/passkey/register/begin":  r.limitPasskeyBegins(r.passkeyRegisterBegin),
		"/auth/passkey/register/finish": r.passkeyRegisterFinish,
		"/auth/passkey/login/begin":     r.limitPasskeyBegins(r.passkeyLoginBegin),
		"/auth/passkey/login/finish":    r.passkeyLoginFinish,
	} {
		readclass.Handle(mux, none, "POST "+path, accesslog.HandlerFunc(path, r.atOutsideOrigin(handler)))
	}
}

// atOutsideOrigin serves next only for a request that came to the outside origin (publicOrigin)
// from a page of that origin: its Origin is the outside origin, or it carries none and the browser
// says it is same-origin, as enforceCookieOrigin holds a cookie's writes. A request to the canonical
// origin answers the 404 an unknown path under /auth answers (writeNoRoute, logged once, as its
// route); one from another origin's page is 403, as a cookie's write from it is.
func (r *router) atOutsideOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if !r.atOutside(req) {
			writeNoRoute(w, req)
			return
		}
		origin := req.Header.Get("Origin")
		if origin != r.ctx.OutsideOrigin && (origin != "" || req.Header.Get("Sec-Fetch-Site") != "same-origin") {
			writeError(w, http.StatusForbidden, "invalid request origin")
			return
		}
		next(w, req)
	}
}

// atOutside reports whether req came to the outside origin.
func (r *router) atOutside(req *http.Request) bool {
	return r.ctx.OutsideOrigin != "" && publicOrigin(r.ctx.ServerURL, r.ctx.OutsideOrigin, req) == r.ctx.OutsideOrigin
}

// googleSignInHere serves next, the Google sign-in's start or callback, at the canonical origin
// alone: on the outside origin's host, at any port, it answers the 404 a path no route serves
// answers (writeNoRoute, logged once, as its route), with passkey sign-in on or off, since Google
// signs in the Workspace's people alone and they sign in at the canonical origin, and a person from
// outside signs in with a passkey or not at all. The host is compared by name at any port, as
// markOutsideOrigin marks it: a browser sends the outside host's cookies to every port of it, and
// the router issues the session there under the outside origin's name, so a Google sign-in served
// at another port would set the outside origin's session for a person of the Workspace.
func (r *router) googleSignInHere(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if r.ctx.OutsideOrigin != "" && sameHostName(r.ctx.OutsideOrigin, req.Host) {
			writeNoRoute(w, req)
			return
		}
		next(w, req)
	}
}

// whoamiSignIn is what GET /auth/whoami's 401 names as the way req's origin signs a person in:
// "passkey" at the outside origin, with passkey sign-in on or off, since that origin serves no
// Google sign-in either way (googleSignInHere) and its page must offer no Google link that answers
// 404 there; nothing elsewhere, so the answer at the canonical origin is what it always was.
func (r *router) whoamiSignIn(req *http.Request) string {
	if r.atOutside(req) {
		return passkeySignIn
	}
	return ""
}

// passkeyRegisterBegin is POST /auth/passkey/register/begin, body {"token"}: the link's fragment.
// It answers the creation options (protocol.CredentialCreation) with the link's invitation, the
// project it is for and the owner who made it (identity.EnrollmentOptions), and sets the ceremony
// cookie; it uses no link and writes nothing: the ceremony rides in the signed cookie.
func (r *router) passkeyRegisterBegin(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, maxPasskeyBody)).Decode(&body); err != nil {
		writeCodeError(w, http.StatusBadRequest, "the body is {\"token\": the link's fragment}", "PASSKEY_REFUSED")
		return
	}
	options, ceremony, err := r.ctx.Passkeys.BeginEnrollment(req.Context(), strings.TrimSpace(body.Token))
	if err != nil {
		writePasskeyRefusal(w, err, true)
		return
	}
	if !r.setPasskeyCeremony(w, ceremony) {
		return
	}
	api.WriteJSON(w, http.StatusOK, options)
}

// passkeyRegisterFinish is POST /auth/passkey/register/finish, body the PublicKeyCredential
// navigator.credentials.create made: it enrolls the passkey with the ceremony's link, in one
// transaction that uses the link and signs the person in, and answers {"login"} with the session
// cookie, host-only, so it is the outside origin's alone.
func (r *router) passkeyRegisterFinish(w http.ResponseWriter, req *http.Request) {
	ceremony, ok := r.takePasskeyCeremony(w, req)
	if !ok {
		return
	}
	email, subject, err := r.ctx.Passkeys.FinishEnrollment(req.Context(), ceremony, http.MaxBytesReader(w, req.Body, maxPasskeyBody))
	if err != nil {
		writePasskeyRefusal(w, err, true)
		return
	}
	r.passkeySignedIn(w, req, email, subject)
}

// passkeyLoginBegin is POST /auth/passkey/login/begin: the request options
// (protocol.CredentialAssertion) of a discoverable sign-in, naming no credential, and the ceremony
// cookie. It writes nothing: the ceremony rides in the signed cookie.
func (r *router) passkeyLoginBegin(w http.ResponseWriter, req *http.Request) {
	assertion, ceremony, err := r.ctx.Passkeys.BeginSignIn()
	if err != nil {
		writePasskeyRefusal(w, err, false)
		return
	}
	if !r.setPasskeyCeremony(w, ceremony) {
		return
	}
	api.WriteJSON(w, http.StatusOK, assertion)
}

// passkeyLoginFinish is POST /auth/passkey/login/finish, body the PublicKeyCredential
// navigator.credentials.get made: it signs the person in and answers {"login"} with the session
// cookie.
func (r *router) passkeyLoginFinish(w http.ResponseWriter, req *http.Request) {
	ceremony, ok := r.takePasskeyCeremony(w, req)
	if !ok {
		return
	}
	email, subject, err := r.ctx.Passkeys.FinishSignIn(req.Context(), ceremony, http.MaxBytesReader(w, req.Body, maxPasskeyBody))
	if err != nil {
		writePasskeyRefusal(w, err, false)
		return
	}
	r.passkeySignedIn(w, req, email, subject)
}

// passkeySignedIn issues the session cookie of a finished ceremony (issueSession, which confirms
// the address still records subject) and answers {"login"}.
func (r *router) passkeySignedIn(w http.ResponseWriter, req *http.Request, email, subject string) {
	if !r.issueSession(w, req, email, subject) {
		return
	}
	accesslog.Note(req.Context(), accesslog.KindUser, email)
	api.WriteJSON(w, http.StatusOK, map[string]string{"login": email})
}

// setPasskeyCeremony sets the ceremony cookie carrying ceremony, answering 500 when it cannot be
// signed.
func (r *router) setPasskeyCeremony(w http.ResponseWriter, ceremony identity.PasskeyCeremony) bool {
	value, err := signPasskeyCeremony(r.ctx.SigningKey, ceremony)
	if err != nil {
		slog.Error("dispatch: the passkey ceremony could not be signed", "error", err)
		writeCodeError(w, http.StatusInternalServerError, "the passkey ceremony could not be started", "PASSKEY_ERROR")
		return false
	}
	http.SetCookie(w, passkeyCeremonyCookieFor(value, passkeyCeremonyMaxAge, !r.ctx.InsecureCookie))
	return true
}

// takePasskeyCeremony clears the ceremony cookie before anything else, so each ceremony finishes
// at most once from this browser, and answers the ceremony it carried: one whose MAC verifies under
// a key of the set and that has not lapsed, else 400 CEREMONY_EXPIRED.
func (r *router) takePasskeyCeremony(w http.ResponseWriter, req *http.Request) (identity.PasskeyCeremony, bool) {
	http.SetCookie(w, passkeyCeremonyCookieFor("", -1, !r.ctx.InsecureCookie))
	cookie, err := req.Cookie(passkeyCeremonyCookieName(!r.ctx.InsecureCookie))
	if err != nil {
		writePasskeyRefusal(w, identity.ErrPasskeyCeremonyExpired, false)
		return identity.PasskeyCeremony{}, false
	}
	ceremony, ok := verifyPasskeyCeremony(r.ctx.SigningKey, cookie.Value, time.Now())
	if !ok {
		writePasskeyRefusal(w, identity.ErrPasskeyCeremonyExpired, false)
		return identity.PasskeyCeremony{}, false
	}
	return ceremony, true
}

// passkeyCeremonyCookieName is the ceremony cookie's name: the `__Host-` one when the cookie is
// Secure, the plain one under DISPATCH_INSECURE_COOKIE.
func passkeyCeremonyCookieName(secure bool) string {
	if secure {
		return passkeyCeremonyCookie
	}
	return passkeyCeremonyCookieInsecure
}

func passkeyCeremonyCookieFor(value string, maxAge int, secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     passkeyCeremonyCookieName(secure),
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   maxAge,
	}
}

// signPasskeyCeremony is the ceremony cookie's value for ceremony: the format, the base64url JSON
// of ceremony, and its MAC under keys' current key for passkeyCeremonyPurpose, joined by dots.
func signPasskeyCeremony(keys auth.SigningKeys, ceremony identity.PasskeyCeremony) (string, error) {
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(ceremony); err != nil {
		return "", err
	}
	payload := passkeyCeremonyFormat + "." + base64.RawURLEncoding.EncodeToString(bytes.TrimSuffix(body.Bytes(), []byte("\n")))
	return payload + "." + keys.SignFor(passkeyCeremonyPurpose, payload), nil
}

// verifyPasskeyCeremony returns the ceremony a cookie's value carries when its MAC verifies under
// any key of keys for passkeyCeremonyPurpose and it has not lapsed at now.
func verifyPasskeyCeremony(keys auth.SigningKeys, value string, now time.Time) (identity.PasskeyCeremony, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 || parts[0] != passkeyCeremonyFormat {
		return identity.PasskeyCeremony{}, false
	}
	if !keys.VerifyFor(passkeyCeremonyPurpose, parts[0]+"."+parts[1], parts[2]) {
		return identity.PasskeyCeremony{}, false
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return identity.PasskeyCeremony{}, false
	}
	var ceremony identity.PasskeyCeremony
	if err := json.Unmarshal(body, &ceremony); err != nil || ceremony.Session.Challenge == "" || ceremony.Expired(now) {
		return identity.PasskeyCeremony{}, false
	}
	return ceremony, true
}

// passkeyRefusals are the ceremonies' refusals, each by its code and status, in the order they are
// matched. NotListed is enrollment's code for identity.ErrNotListed (the link's person), and
// NOT_LISTED a sign-in's (the person whose sign-in it ended).
var passkeyRefusals = []struct {
	match  error
	code   string
	status int
}{
	{store.ErrInvitationUnknown, "INVITATION_UNKNOWN", http.StatusNotFound},
	{store.ErrInvitationUsed, "INVITATION_USED", http.StatusGone},
	{store.ErrInvitationExpired, "INVITATION_EXPIRED", http.StatusGone},
	{store.ErrInvitationNotListed, "INVITATION_NOT_LISTED", http.StatusForbidden},
	{store.ErrListedByAnotherOwner, "LISTED_BY_ANOTHER_OWNER", http.StatusForbidden},
	{store.ErrEnrollmentSuperseded, "CEREMONY_EXPIRED", http.StatusBadRequest},
	{auth.ErrAddressHeldByMember, "FULL_MEMBER", http.StatusConflict},
	{identity.ErrWorkspaceAddress, "WORKSPACE_ADDRESS", http.StatusForbidden},
	{identity.ErrMembershipUnreadable, "MEMBERSHIP_UNREADABLE", http.StatusServiceUnavailable},
	{store.ErrPasskeyUnknown, "PASSKEY_UNKNOWN", http.StatusUnauthorized},
	{store.ErrPasskeyAccountMoved, "PASSKEY_UNKNOWN", http.StatusUnauthorized},
	{store.ErrPasskeySignCount, "PASSKEY_SIGN_COUNT", http.StatusUnauthorized},
	{identity.ErrPasskeyCeremonyExpired, "CEREMONY_EXPIRED", http.StatusBadRequest},
	{store.ErrPasskeyCeremonyUsed, "CEREMONY_EXPIRED", http.StatusBadRequest},
	{identity.ErrPasskeyRefused, "PASSKEY_REFUSED", http.StatusBadRequest},
}

// writePasskeyRefusal answers a ceremony's refusal by its code (passkeyRefusals), identity.ErrNotListed
// as INVITATION_NOT_LISTED in an enrollment and NOT_LISTED in a sign-in, and any other error as a
// 500 that names no person, logged.
func writePasskeyRefusal(w http.ResponseWriter, err error, enrollment bool) {
	if errors.Is(err, identity.ErrNotListed) {
		code := "NOT_LISTED"
		if enrollment {
			code = "INVITATION_NOT_LISTED"
		}
		writeCodeError(w, http.StatusForbidden, "no Dispatch project's access list names this person directly any more", code)
		return
	}
	for _, refusal := range passkeyRefusals {
		if errors.Is(err, refusal.match) {
			writeCodeError(w, refusal.status, refusal.match.Error(), refusal.code)
			return
		}
	}
	slog.Error("dispatch: a passkey ceremony failed", "error", err)
	writeCodeError(w, http.StatusInternalServerError, "the passkey ceremony could not be completed", "PASSKEY_ERROR")
}
