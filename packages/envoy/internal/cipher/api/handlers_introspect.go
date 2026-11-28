// handlers_introspect.go: POST /v1/oauth/introspect — RFC 7662 token introspection, the route a
// resource server (Dispatch) reads one of the broker's opaque access tokens back through. It is
// authenticated by the caller's own Kubernetes ServiceAccount token (accountAuth), which is what
// RFC 7662 Section 2.1 allows in place of OAuth client authentication, so the provider's
// Introspect answers it directly.
package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
)

// introspectionResponse is POST /v1/oauth/introspect's answer: oauthwire.Introspection itself,
// named here because the broker's generated HTTP reference documents a route's answer from a type
// this package declares. The route adds no field of its own.
type introspectionResponse = oauthwire.Introspection

// introspect is POST /v1/oauth/introspect: the RFC 7662 request, a form whose token parameter
// names the access token to read back, answered with what the broker knows about it — whether it
// is live and, when it is, the session, the person and the projects it carries, the Google
// account the credential behind a person's token records, the scope it was granted, when it
// expires and the key it is bound to.
//
// A token the broker does not know, one it has revoked and one that has expired are all answered
// with active false (RFC 7662 Section 2.2), so the answer discloses nothing to a caller holding a
// string at random; the broker's own failure to read its storage is a refusal, 503, since
// answering that as "not live" would deny a sound token and hide the outage. A row whose stored
// project claim this broker could not have written is 500 STORED_CLAIM_MALFORMED, naming the
// session and launcher credential whose token it is, so an operator looks at that row and not at
// the database. A live token whose person Dispatch holds is answered active and held; one whose
// person's hold the gate cannot tell is 503 HOLD_UNCHECKABLE, naming the person and why in fixed
// words, with the gate's own error in the log alone. Dispatch answers either 503 as
// BROKER_UNAVAILABLE, and the Envoy listener answers it with a 503.
//
// It spends no bucket of its own: authenticate has already spent its source address's bucket
// (DefaultAccountLimit), the one every accountAuth route shares, before the caller's token was
// verified, so a listed caller is held to one limit across the class rather than to a second one
// here. The listed caller its own token named is not logged: reading a token back acts on
// nobody's behalf.
func (s *server) introspect(w http.ResponseWriter, r *http.Request, _ string) {
	r.Body = http.MaxBytesReader(w, r.Body, maxOAuthFormBytes)
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", fmt.Sprintf("the form must parse and be at most %d bytes", maxOAuthFormBytes))
		return
	}
	if r.PostForm.Get("token") == "" {
		writeError(w, http.StatusBadRequest, "TOKEN_REQUIRED", "token is required")
		return
	}
	// answer is declared with this package's own name for the provider's answer so the generated
	// HTTP reference can read the route's body type off it.
	var answer introspectionResponse
	answer, err := s.deps.OAuth.Introspect(r.Context(), r.PostForm.Get("token"))
	var uncheckable *oauth.HoldUncheckableError
	switch {
	case errors.Is(err, oauth.ErrStoredClaimMalformed):
		slog.Error("broker: introspect token failed: a stored claim is malformed", "error", err)
		writeError(w, http.StatusInternalServerError, "STORED_CLAIM_MALFORMED", err.Error())
		return
	case errors.As(err, &uncheckable):
		slog.Error("broker: introspect token failed: whether Dispatch holds its person cannot be read", "person", uncheckable.Verdict.Person, "error", uncheckable.Verdict.Err)
		writeError(w, http.StatusServiceUnavailable, "HOLD_UNCHECKABLE", uncheckable.Error())
		return
	case err != nil:
		writeUnavailable(w, "DATABASE_UNAVAILABLE", "introspect token", err)
		return
	}
	writeJSON(w, http.StatusOK, answer)
}
