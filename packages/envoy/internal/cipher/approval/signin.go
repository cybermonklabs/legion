package approval

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/oidc"
)

// pendingSignIn is a sign-in in progress, held in the browser's signInCookie between the redirect
// to the provider and the callback: the state the callback compares, the nonce the ID token must
// carry, the PKCE verifier, and the page the sign-in returns to.
type pendingSignIn struct {
	State    string `json:"state"`
	Nonce    string `json:"nonce"`
	Verifier string `json:"verifier"`
	Next     string `json:"next"`
}

// approvePrefix is the path every page of a record starts with, and the only place a sign-in may
// return to.
const approvePrefix = "/approve/"

// validNext reports whether next is a record's page, /approve/<record id>: a sign-in returns
// nowhere else, so its callback is no open redirect.
func validNext(next string) bool {
	id, ok := strings.CutPrefix(next, approvePrefix)
	return ok && record.ValidID(id)
}

// beginSignIn sends the browser to the provider to sign in and come back to recordID's page,
// holding the sign-in in its signInCookie. hint is the login_hint: the record's approver for a
// signed-in person switching accounts, which names that person to the provider's account
// chooser, and empty for a browser with no live session, whose sign-in reads no record, so its
// redirect says nothing of whether the record exists. selectAccount asks the provider for its
// account chooser (prompt=select_account), for a person switching accounts. The session the
// browser may hold now is left alone: the callback ends it when it makes the new one.
func (p *Page) beginSignIn(w http.ResponseWriter, r *http.Request, recordID, hint string, selectAccount bool) {
	pending := pendingSignIn{State: randomValue(), Nonce: randomValue(), Verifier: oauth2.GenerateVerifier(), Next: approvePrefix + recordID}
	// The one failure here is the provider's discovery document, which the sign-in's code flow
	// reads when it is first needed; nothing is written to the browser until it has succeeded.
	authURL, err := p.signIn.OnlineAuthURL(r.Context(), p.redirectURL, oidc.OnlineSignIn{
		State: pending.State, Nonce: pending.Nonce, Verifier: pending.Verifier, LoginHint: hint, SelectAccount: selectAccount,
	})
	if err != nil {
		p.writeSignInUnavailable(w, err)
		return
	}
	raw, err := json.Marshal(pending)
	if err != nil {
		writeUnavailable(w, "start the sign-in", err)
		return
	}
	setCookie(w, signInCookie, base64.RawURLEncoding.EncodeToString(raw), signInMaxAge)
	redirect(w, http.StatusFound, authURL)
}

// readPending is the sign-in in progress r's signInCookie holds, when it holds one.
func readPending(r *http.Request) (pendingSignIn, bool) {
	c, err := r.Cookie(signInCookie)
	if err != nil {
		return pendingSignIn{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return pendingSignIn{}, false
	}
	var pending pendingSignIn
	if err := json.Unmarshal(raw, &pending); err != nil || pending.State == "" {
		return pendingSignIn{}, false
	}
	return pending, true
}

// ServeCallback is GET /signin/callback, where the provider returns the browser with a code. The
// sign-in in progress is used once: its cookie is cleared whatever happens next. The callback
// refuses, creating no session, a browser with no sign-in in progress, a state that is not the
// one the sign-in sent (compared in constant time), a sign-in returning anywhere but a record's
// page, a code longer than oidc.MaxCallbackCode, which it sends the provider no request for, a
// code the provider will not exchange with the sign-in's PKCE verifier, an ID token that does not
// verify (issuer, audience, signature, expiry, nonce), a person outside the Workspace domain or
// with an unverified email (oidc.WorkspaceLogin), and a Google account that no longer holds the
// person's address, which Dispatch named replaced when another account took it
// (store.ErrBrowserSessionSubjectReplaced). Otherwise it mints a fresh random session secret into
// the session cookie, stores only its SHA-256 with the session's expiry and the account that
// signed in (the ID token's issuer, as oidc.CanonicalIssuer spells it, and its `sub`), ends the
// session the browser held before (no fixation, no lingering session), and answers 303 to the
// sign-in's page. The account is what a revoke of a replaced account ends the session by, and the
// replaced account's refusal is read under the person's lock in the transaction that writes the
// session, so a sign-in racing that revoke either is refused or writes a session the revoke
// deletes (store.Store.CreateBrowserSession).
//
// From the exchange onward it also writes the sign-in's outcome into the broker's sign-in
// status, which GET /signin/status answers: the time of a sign-in that completed, or the time
// and coded reason of one that failed (status.go). The refusals above it write nothing - a
// browser with no sign-in in progress, a state that is not the sign-in's, a next outside the page
// and a code longer than any the broker sends is a request that was never this broker's sign-in
// to fail - and neither does the refusal of a replaced account, which is the broker's own record
// refusing a sign-in the provider completed, and none of the status's codes.
func (p *Page) ServeCallback(w http.ResponseWriter, r *http.Request, _ Session) {
	pending, ok := readPending(r)
	clearCookie(w, signInCookie)
	if !ok {
		writeMessage(w, http.StatusBadRequest, codeSignInNotStarted, say("No sign-in in progress",
			"This browser has no sign-in to the secrets broker in progress, or it took more than ten minutes. Open the request again to sign in."))
		return
	}
	query := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(pending.State)) != 1 {
		slog.Warn("approval sign-in refused: the state is not the sign-in's")
		writeMessage(w, http.StatusBadRequest, codeSignInStateInvalid, say("Sign-in refused",
			"This sign-in is not the one this browser started, so it was refused. Open the request again to sign in."))
		return
	}
	if !validNext(pending.Next) {
		slog.Warn("approval sign-in refused: it returns somewhere other than a request's page")
		writeMessage(w, http.StatusBadRequest, codeSignInNextInvalid, say("Sign-in refused", "A sign-in returns only to a request's page."))
		return
	}
	if providerError := query.Get("error"); providerError != "" {
		// The error is the callback's query, which whoever holds the browser writes, so the line
		// cuts it (answer.ClipFields).
		slog.Warn("approval sign-in refused by the provider", answer.ClipFields([]any{"error", providerError})...)
		writeMessage(w, http.StatusForbidden, codeSignInProviderRefused, say("Not signed in",
			"The sign-in provider did not sign you in. Open the request again to try once more."))
		return
	}
	code := query.Get("code")
	if len(code) > oidc.MaxCallbackCode {
		slog.Warn("approval sign-in refused: the code is longer than the broker sends a provider", "bytes", len(code))
		writeMessage(w, http.StatusBadRequest, codeSignInCodeInvalid, say("Sign-in refused",
			"This sign-in's code is not one the sign-in provider issues, so it was refused. Open the request again to sign in."))
		return
	}
	claims, err := p.signIn.OnlineExchange(r.Context(), p.redirectURL, code, oidc.OnlineSignIn{
		State: pending.State, Nonce: pending.Nonce, Verifier: pending.Verifier,
	})
	if err != nil {
		// A callback is a sign-in with a code in hand, so a provider that cannot be reached here
		// is an exchange that failed, and the status says so (issuer_unreachable); the person
		// reads that the broker could not reach the provider rather than that they were refused.
		p.recordFailedSignIn(r.Context(), err)
		if errors.Is(err, oidc.ErrIssuerUnavailable) {
			p.writeSignInUnavailable(w, err)
			return
		}
		slog.Warn("approval sign-in refused: the code exchange or the ID token failed", "error", err)
		writeMessage(w, http.StatusForbidden, codeSignInUnverified, say("Sign-in refused",
			"The sign-in provider's answer did not verify, so you are not signed in. Open the request again to sign in."))
		return
	}
	login, err := oidc.WorkspaceLogin(claims, p.cfg.SignIn.Domain)
	if err != nil {
		slog.Warn("approval sign-in refused", "login", login, "error", err)
		p.recordFailedSignIn(r.Context(), err)
		writeMessage(w, http.StatusForbidden, codeSignInAccountRefused, say("Sign-in refused", workspaceRefusal(err, p.cfg.SignIn.Domain)))
		return
	}
	secret := randomBytes()
	lifetime := p.cfg.SignIn.SessionLifetime
	in := store.BrowserSignIn{
		Hash: sha256.Sum256(secret), Login: login,
		Account: store.IssuerSubject{Issuer: oidc.CanonicalIssuer(claims.Issuer), Subject: claims.Subject},
		Expires: time.Now().Add(lifetime),
	}
	if old, ok := sessionSecret(r); ok {
		previous := sha256.Sum256(old)
		in.Previous = &previous
	}
	switch err := p.cfg.Store.CreateBrowserSession(r.Context(), in); {
	case errors.Is(err, store.ErrBrowserSessionSubjectReplaced):
		slog.Warn("approval sign-in refused: another account holds the address now", "login", login)
		writeMessage(w, http.StatusForbidden, codeSignInAccountReplaced, say("Sign-in refused",
			"The Google account you signed in with no longer holds "+login+": another account holds that address now, and the secrets broker signs in no account it replaced."))
		return
	case err != nil:
		writeUnavailable(w, "keep the sign-in", err)
		return
	}
	setCookie(w, sessionCookie, base64.RawURLEncoding.EncodeToString(secret), lifetime)
	slog.Info("approval sign-in", "login", login)
	p.recordSignIn(r.Context())
	redirect(w, http.StatusSeeOther, pending.Next)
}

// workspaceRefusal is what the page tells a person oidc.WorkspaceLogin refused.
func workspaceRefusal(err error, domain string) string {
	switch {
	case errors.Is(err, oidc.ErrNotInDomain):
		return "Only an account of the " + domain + " Workspace may sign in to the secrets broker."
	case errors.Is(err, oidc.ErrEmailNotVerified):
		return "Your account's email address is not verified, so you may not sign in to the secrets broker."
	}
	return "The sign-in provider named no account the secrets broker can sign in."
}

// ServeLogout is POST /signin/logout, from the broker's own origin only: it ends the session the
// browser holds (its row is deleted, so a copy of the cookie signs nothing in) and clears the
// cookie, answering a page saying so with a link back to the request the form names. It sends the
// browser nowhere: the provider's own sign-in is the person's.
func (p *Page) ServeLogout(w http.ResponseWriter, r *http.Request, sess Session) {
	if !p.sameOrigin(r) {
		refuseOrigin(w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		writeMessage(w, http.StatusBadRequest, codeFormUnreadable, say("Not signed out", "The form could not be read."))
		return
	}
	if secret, ok := sessionSecret(r); ok {
		if err := p.cfg.Store.DeleteBrowserSession(r.Context(), sha256.Sum256(secret)); err != nil {
			writeUnavailable(w, "end the sign-in", err)
			return
		}
	}
	clearCookie(w, sessionCookie)
	if sess.live() {
		slog.Info("approval sign-out", "login", sess.Login)
	}
	m := say("Signed out", "You are signed out of the secrets broker.")
	if next := r.PostForm.Get("next"); validNext(next) {
		m.Link = &link{Href: next, Text: "Back to the request"}
	}
	writeMessage(w, http.StatusOK, "", m)
}
