package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/exchange"
	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/oauthstore"
	"github.com/sjawhar/envoy/internal/oidc"
)

// thumbprintShape is an RFC 7638 SHA-256 thumbprint as a registrar sends it: 32 bytes,
// base64url-encoded with no padding.
var thumbprintShape = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// podSessionBody is POST /v1/oauth/exchange/pod-sessions's exact shape.
type podSessionBody struct {
	// The pod's own projected Kubernetes ServiceAccount token, whose audience is the broker's
	// (BROKER_K8S_OIDC_AUDIENCE), the very token the registrar hands POST /v1/enrollments as it
	// enrolls that pod, or, where it is set, BROKER_POD_REGISTRATION_AUDIENCE, which no other
	// route accepts. It is what names the pod — its account is the token's subject and its uid
	// the kubernetes.io pod uid — so a registrar cannot register a pod whose token it does not
	// hold, and nothing is taken on the registrar's word but the session name.
	PodToken string `json:"pod_token"`
	// The session name that pod may exchange under.
	Session string `json:"session"`
	// The RFC 7638 thumbprint of the key the session's own role proves (its agent-secrets key),
	// which binds the registration to that key: the exchange refuses a trade for this session
	// proved with another. Absent, the registration is the pod's, as a session pod's one session
	// is registered.
	Thumbprint string `json:"thumbprint,omitempty"`
	// The registrar's own order of its writes for this pod and session, which the Legion daemon
	// sets to the role's launch generation: a write of a lower generation than the row's changes
	// nothing, so a write an old incarnation sent that the broker commits after the new
	// incarnation's leaves the new key in place. Absent is 0, as a session watch writes.
	Generation int64 `json:"generation,omitempty"`
}

// podSessionResponse is POST /v1/oauth/exchange/pod-sessions's answer: the registration as it now
// stands, so a registrar reads back which pod and name it wrote and how long the broker honours
// it without writing it again (oauthstore.PodSessionRetention past this write).
type podSessionResponse struct {
	// The uid of the pod the registration is for, read from the token the body carried.
	PodUID string `json:"pod_uid"`
	// The session name that pod may exchange under.
	Session string `json:"session"`
	// The thumbprint the registration is bound to, as it now stands; absent for one bound to the
	// pod alone.
	Thumbprint string `json:"thumbprint,omitempty"`
	// The generation of the write that last set the registration, as it now stands: a write of a
	// lower one is answered the row it did not change. Absent for one no registrar gave a
	// generation, so a registrar that sends none, as the session watch does, is answered exactly
	// the fields it was answered before generations existed (R9); a reader takes absent as 0.
	Generation int64 `json:"generation,omitempty"`
	// When the broker stops honouring the registration unless it is written again.
	ExpiresAt time.Time `json:"expires_at"`
}

// registerPodSession is POST /v1/oauth/exchange/pod-sessions: a deployment's own daemon, logging
// in as the service BROKER_SERVICE_LOGINS names its account (serviceAuth) and listed in
// BROKER_POD_REGISTRARS for the pod's account, records that one pod may exchange under one
// session name. The token exchange binds a session name to a principal — the listed service
// account — and never to a pod, so two pods of one account are one principal there and nothing
// in that claim can tell them apart; this row is what does, and the exchange refuses a pod of a
// free-session account whose uid no registration names for the name it asks for. A relaunch is
// a second pod uid registered for the same name, so a successor is never refused what its
// predecessor held.
//
// The refusals come in this order: the body; the pod token; the list (403
// POD_REGISTRAR_NOT_LISTED); the caller's own pod (403 POD_CALLER_IS_THE_POD); the thumbprint
// (400 THUMBPRINT_INVALID); the generation (400 GENERATION_INVALID); the policy's; and then the
// store's (409 SESSION_HELD_BY_ANOTHER_REGISTRAR).
//
// The body's token is verified at two audiences (podSessionToken): the broker's own, with the
// verifier an enrolling pod's token is verified with (Deps.Enroll.Pod), which the serviceAuth
// class has already proved the broker has, and, when BROKER_POD_REGISTRATION_AUDIENCE is set, that
// one (Deps.PodRegistrationTokens), which no other route accepts, so a registrar can hold a pod's
// token that registers it and does nothing else. A token either verifier accepts names the pod.
// Any other is answered the verdict of the verifier whose audience it carries: an expired token
// of either audience is 401 POD_TOKEN_EXPIRED, which a registrar retries, and every other, one of
// neither audience included, 401 POD_TOKEN_INVALID.
//
// The caller must then be listed for the pod's account: an entry of BROKER_POD_REGISTRARS
// (Deps.PodRegistrars) pairs the account it logs in as with the account the verified token
// names, or the registration is 403 POD_REGISTRAR_NOT_LISTED, naming both accounts. The list,
// and not BROKER_SERVICE_LOGINS, is the registration's authority: a session pod logs in as a
// service too, and a login alone would let it register any pod whose token it holds for a name
// of its choosing. The check needs the verified token, whose account its refusal names, and
// comes before the caller's own pod, so a login no entry names is refused it for every pod, its
// own included, and with the setting unset every registration is refused it, the one 403 a
// registrar retries.
//
// The pod the token names must not be the caller's own: a token naming the account the caller
// logs in as and the very pod the caller's own token names is 403 POD_CALLER_IS_THE_POD, reached
// through an entry that pairs an account with itself. A registrar records which pod may write
// under which name, and a pod that wrote its own row would pick the name it writes under, which
// is the one choice this row takes from it. BROKER_SERVICE_LOGINS may name an account
// BROKER_SERVICES binds to a service, so every pod of that account logs the service in and
// reaches this route; without this a session pod listed for its own account would register
// itself under the name a sibling pod holds. A registrar whose own token names no pod, and one
// registering any pod but the one it runs in, are untouched. The policy then refuses a token
// bound to no pod (400 POD_NOT_BOUND), an account neither BROKER_EXCHANGE_ACCOUNTS nor
// BROKER_EXCHANGE_READ_ACCOUNTS lists (403 POD_ACCOUNT_NOT_LISTED, a code of its own, since the
// caller's own list refusing is ACCOUNT_NOT_LISTED and the two lists are two facts), an account
// whose entry fixes a session, which every read entry does (409 SESSION_FIXED, since such a pod
// needs no registration and registering one would say something the configuration already
// says), and a session name no request could name back (400 SESSION_INVALID). Each refusal names
// the account or the name it refused and repeats no token.
//
// A session name belongs to the registrar that registered it: the row records the caller's login
// account (registrar.Subject, never its service, which every Legion daemon shares, nor its pod,
// which a restart changes), and while a live registration of the name, of any pod and released
// or not, names another registrar, the write is 409 SESSION_HELD_BY_ANOTHER_REGISTRAR, naming the
// session and the registrar that holds it, and writes nothing (oauthstore.Store.RegisterPodSession).
// Two daemons listed for one pod account are two registrars, so neither registers a pod, its own
// or the other's, under a name the other holds, and the exchange admits a pod under a name only
// when the registrar holding it registered that pod. The refusal is answered to the registrar
// alone and logged at Warn, naming the registrar, the holder, the pod and the session.
//
// The body may name the thumbprint of the key the session's own role proves, which binds the
// registration to that key (oauthstore.Store.RegisterPodSession), and a thumbprint that is not
// 43 base64url characters is 400 THUMBPRINT_INVALID. It may name the write's generation, the
// registrar's own order of its writes for this pod and session, and a negative one is 400
// GENERATION_INVALID. The answer is always the registration as stored after the write, its
// generation included: a write naming no key keeps the binding a live row already holds, and a
// write of a lower generation than the row's changes nothing and is answered the row unchanged.
//
// Each write is logged with what it did, since the registrar is the authority here and these
// lines are the one record of who may write under which name. A write applied is logged at Info
// as registered, naming the registrar and the thumbprint and generation it sent beside the ones
// stored, and the thumbprint it replaced when it took a key off the row, by binding the row to
// another key or by taking a released row, another registrar's lapsed row or a row of no
// registrar whole; one that names no key and no generation, of a row that held no key and names
// neither after it, logs the line it logged before either existed (R9), the registrar included
// in neither. A write the generation rule refused changed nothing, so it is not logged as
// registered: it logs a line of its own at Warn, naming the registrar and what it sent beside the
// row it left as it was.
func (s *server) registerPodSession(w http.ResponseWriter, r *http.Request, registrar machine.ServiceAccount) {
	var body podSessionBody
	if !readJSON(w, r, &body, "INVALID_REQUEST") {
		return
	}
	claims, err := s.podSessionToken(r.Context(), body.PodToken)
	switch {
	case errors.Is(err, oidc.ErrExpired):
		writeError(w, http.StatusUnauthorized, "POD_TOKEN_EXPIRED", "the pod's ServiceAccount token has expired")
		return
	case err != nil:
		writeError(w, http.StatusUnauthorized, "POD_TOKEN_INVALID", "the pod's ServiceAccount token does not verify")
		return
	}
	if !s.deps.PodRegistrars.Lists(registrar.Subject, claims.Subject) {
		writeError(w, http.StatusForbidden, "POD_REGISTRAR_NOT_LISTED",
			fmt.Sprintf("%s is not listed to register pods of %s (BROKER_POD_REGISTRARS)", registrar.Subject, claims.Subject))
		return
	}
	if claims.Subject == registrar.Subject && registrar.PodUID != "" && claims.PodUID == registrar.PodUID {
		writeError(w, http.StatusForbidden, "POD_CALLER_IS_THE_POD",
			fmt.Sprintf("this is the caller's own pod, %s of %s, and a registrar registers a pod other than the one it runs in", claims.PodUID, claims.Subject))
		return
	}
	if body.Thumbprint != "" && !thumbprintShape.MatchString(body.Thumbprint) {
		writeError(w, http.StatusBadRequest, "THUMBPRINT_INVALID",
			"the thumbprint must be a key's RFC 7638 SHA-256 thumbprint: 43 base64url characters, unpadded")
		return
	}
	if body.Generation < 0 {
		writeError(w, http.StatusBadRequest, "GENERATION_INVALID",
			fmt.Sprintf("the generation must be 0 or more, not %d", body.Generation))
		return
	}
	stored, err := s.deps.OAuth.RegisterPodSession(r.Context(), registrar.Subject, claims.Subject, claims.PodUID, body.Session, body.Thumbprint, body.Generation)
	var held *oauthstore.PodSessionHeldError
	switch {
	case errors.Is(err, exchange.ErrNoPodBinding):
		writeError(w, http.StatusBadRequest, "POD_NOT_BOUND", err.Error())
	case errors.Is(err, exchange.ErrNoEntry):
		writeError(w, http.StatusForbidden, "POD_ACCOUNT_NOT_LISTED", err.Error())
	case errors.Is(err, exchange.ErrSessionFixed):
		writeError(w, http.StatusConflict, "SESSION_FIXED", err.Error())
	case errors.Is(err, exchange.ErrSessionInvalid):
		writeError(w, http.StatusBadRequest, "SESSION_INVALID", err.Error())
	case errors.As(err, &held):
		slog.Warn("broker: a pod's session registration was refused: another registrar holds the session name",
			"service", registrar.Service, "registrar", registrar.Subject, "holder", held.Holder,
			"account", claims.Subject, "pod", claims.PodUID, "session", body.Session)
		writeError(w, http.StatusConflict, "SESSION_HELD_BY_ANOTHER_REGISTRAR",
			fmt.Sprintf("the session name %s is held by another registrar, %s, until its registrations of the name lapse, released or not", held.Session, held.Holder))
	case err != nil:
		writeInternal(w, "register the pod's session name", err)
	default:
		attrs := []any{"service", registrar.Service, "account", claims.Subject, "pod", claims.PodUID, "session", body.Session}
		if body.Thumbprint != "" || body.Generation != 0 || stored.JKT != "" || stored.Generation != 0 || stored.Replaced != "" {
			attrs = append(attrs, "registrar", registrar.Subject, "sent_thumbprint", body.Thumbprint, "sent_generation", body.Generation,
				"thumbprint", stored.JKT, "generation", stored.Generation)
		}
		switch {
		case !stored.Applied:
			slog.Warn("broker: a pod's session registration was not written: the registration holds a later generation", attrs...)
		case stored.Replaced != "":
			slog.Info("broker: registered a pod to write under a session name", append(attrs, "replaced_thumbprint", stored.Replaced)...)
		default:
			slog.Info("broker: registered a pod to write under a session name", attrs...)
		}
		writeJSON(w, http.StatusOK, podSessionResponse{PodUID: claims.PodUID, Session: body.Session, Thumbprint: stored.JKT, Generation: stored.Generation, ExpiresAt: stored.ExpiresAt})
	}
}

// podSessionToken verifies a registration's pod token, raw: at the broker's own audience
// (Deps.Enroll.Pod) and, where BROKER_POD_REGISTRATION_AUDIENCE is set, at that audience too
// (Deps.PodRegistrationTokens). A token either verifier accepts is answered its claims. Any other
// is answered the error of the verifier whose audience it carries: an oidc.ErrAudience from one
// verifier yields to the other's error. A verifier judges a token's audience before its expiry,
// so an expired token of either audience answers oidc.ErrExpired, and the daemon retries it; a
// token of neither audience answers the second verifier's oidc.ErrAudience. registerPodSession is
// the one caller, so no other route accepts a token of the registration audience.
func (s *server) podSessionToken(ctx context.Context, raw string) (enroll.AccountClaims, error) {
	claims, err := s.deps.Enroll.Pod.VerifyAccount(ctx, raw)
	if err == nil || s.deps.PodRegistrationTokens == nil {
		return claims, err
	}
	registration, registrationErr := s.deps.PodRegistrationTokens.VerifyAccount(ctx, raw)
	switch {
	case registrationErr == nil:
		return registration, nil
	case errors.Is(err, oidc.ErrAudience):
		return enroll.AccountClaims{}, registrationErr
	default:
		return enroll.AccountClaims{}, err
	}
}

// maxReleasedSessions is the most entries a release's session list may hold, counted as sent,
// before a name given twice is dropped: with readJSON's 1 MiB it bounds what one call asks of the
// store, by count as well as by bytes, so a caller with more names, or with many long ones, sends
// them in more than one call.
const maxReleasedSessions = 4096

// podSessionReleaseBody is POST /v1/oauth/exchange/pod-sessions/release's exact shape.
type podSessionReleaseBody struct {
	// The session names whose registrations by the caller's login account the call releases: 1 to
	// 4096 entries, counted as sent, each a string, so a null entry is refused. Any string is a
	// name: one the registration route could never have taken (empty, over 200 bytes, or with
	// white space or an unprintable character, a NUL byte among them) matches no registration, so
	// it is released with nothing to release, and a name given twice counts once. The broker drops
	// such a name before it queries its store, which is also what keeps a NUL byte, which Postgres
	// refuses in text, from failing the release.
	Sessions []string `json:"sessions"`
}

// UnmarshalJSON reads a release's body as readJSON reads every body, with no field the body does
// not declare, and refuses a null entry of sessions, naming its index. Decoded into a []string, as
// readJSON alone would, a null entry is the empty string, which is a name released with nothing to
// release, so {"sessions": [null]} would be answered 200 for a body outside the route's shape,
// where {"sessions": [1]} is refused. The refusal is a decode error, so readJSON answers it 400
// INVALID_REQUEST, as it answers an entry that is not a string. readJSON's own
// DisallowUnknownFields does not reach a method of this kind, which decodes the bytes it is handed
// itself, so the decoder here sets its own. A sessions that is absent or null, and a body of null,
// which encoding/json hands this method too, leave Sessions empty, which the handler refuses 400
// SESSIONS_INVALID with the count.
func (b *podSessionReleaseBody) UnmarshalJSON(data []byte) error {
	// It shadows the package type: encoding/json names this type and field in a refusal a client reads.
	type podSessionReleaseBody struct {
		Sessions []*string `json:"sessions"`
	}
	var sent podSessionReleaseBody
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&sent); err != nil {
		return err
	}
	sessions := make([]string, len(sent.Sessions))
	for i, session := range sent.Sessions {
		if session == nil {
			return fmt.Errorf("sessions[%d] is null, and each session name is a string", i)
		}
		sessions[i] = *session
	}
	b.Sessions = sessions
	return nil
}

// podSessionReleaseResponse is POST /v1/oauth/exchange/pod-sessions/release's answer: two counts,
// a fixed size whatever the list, so it fits any client's read limit.
type podSessionReleaseResponse struct {
	// The number of distinct session names the request asked to release.
	Sessions int `json:"sessions"`
	// The number of live registrations of those names that the caller's login account wrote and
	// that are released after the call, by this call or an earlier one: 0 for names that account
	// holds no registration of, which are released all the same.
	Released int `json:"released"`
}

// releasePodSessions is POST /v1/oauth/exchange/pod-sessions/release: a deployment's own daemon
// releases the session names it registered at POST /v1/oauth/exchange/pod-sessions, as it does
// when the claim those names served ends, so a pod it registered under them, still holding a
// token it can present, trades under them no more.
//
// Who may call is who may call the registration route (serviceAuth): an account
// BROKER_SERVICE_LOGINS lists, presenting its own projected ServiceAccount token for the broker's
// audience, and nothing else. The class answers before the handler, as it does for the
// registration: 401 for the token, 403 ACCOUNT_NOT_LISTED for an account the logins do not name,
// and 429 with Retry-After from the account class's per-address bucket. BROKER_POD_REGISTRARS
// does not gate it, so a registrar whose entry was taken out still releases what it registered
// while it was listed. A release is the registrar account's (registrar.Subject), not a pod's: it
// releases every live registration of each named session that the account wrote, whichever pod of
// the account logged in to write it. So every pod of an account paired with itself in
// BROKER_POD_REGISTRARS, which BROKER_SERVICE_LOGINS lists so that the pair registers anything,
// logs in as that registrar and can release every registration that account wrote, by session
// name alone, with no token of the pod whose trade it ends. Every pod that runs as any registrar's
// account logs in as that registrar in the same way and can release every registration that
// account wrote. So a deployment lists as a registrar only an account whose pods may end the
// trades of every pod that account registers. An account paired with itself is the common case of
// an account whose pods may not. The caller stops every write of the names it releases first, its
// renewal and any write in flight: a registration write by that account after the release makes
// that registration live again.
//
// The body is exactly {"sessions": [...]}, at most 1 MiB (413 REQUEST_TOO_LARGE above it); an
// unknown field, a body that is not JSON, and an entry that is not a string are 400
// INVALID_REQUEST, a null entry naming its index (podSessionReleaseBody.UnmarshalJSON). A list
// that is missing, null or empty, a body of null included, or that holds more than
// maxReleasedSessions entries counted as sent, is 400 SESSIONS_INVALID, naming the count. Any
// string is a name and none is refused: one the registration route could never have taken
// matches no registration (exchange.ValidSession), so it is released with nothing to release. The
// store drops such a name before its query (oauthstore.Store.ReleasePodSessions), which is also
// what keeps a NUL byte, which JSON's \u0000 carries and Postgres refuses in text, out of the
// query, where it would fail the release as a 500.
//
// The release is one transaction (oauthstore.Store.ReleasePodSessions): every live registration of
// each named session that the caller's login account wrote is marked released, once, so a repeat
// does not move the mark. Nothing is deleted and no expiry moves, and no registration of another
// registrar is touched or revealed. The answer is 200 {"sessions", "released"}: the number of
// distinct names asked, and the number of live registrations of those names that the caller's
// login account wrote and that are released after the call. A 200 means every named session is
// released: from its commit on, the exchange refuses each registration of each named session that
// the caller's login account wrote (a permanent invalid_grant); the rows stay until they expire, at
// most oauthstore.PodSessionRetention after their last write, and keep each name bound to that
// account until then, so no other registrar takes it; and a later registration write of a pod and
// name by that account makes that one registration live again and nothing else. released: 0, for a
// name that account holds no registration of, is still released. A repeat of the call answers the
// same and changes nothing, so a retry after a lost answer is safe.
//
// Every other answer releases nothing: the refusals above, a 5xx, and a 404 or 405 from a broker
// of an older build or with no OAuth provider, which mounts no /v1/oauth/ route. Only a 200 says
// the names are released, and anything else is "not released, try again".
//
// It revokes no access token: one minted before the release commits, or by an exchange that read
// the registration just before, lives to its own expiry, at most BROKER_ACCESS_TOKEN_SECONDS (900
// seconds by default) after the release.
//
// Each release is logged at Info, naming the service, the registrar, the number of names asked
// and the number of registrations released, since the registrar is the authority here and these
// lines are the record of which registrar stopped its pods' trades, and when.
func (s *server) releasePodSessions(w http.ResponseWriter, r *http.Request, registrar machine.ServiceAccount) {
	var body podSessionReleaseBody
	if !readJSON(w, r, &body, "INVALID_REQUEST") {
		return
	}
	if len(body.Sessions) == 0 || len(body.Sessions) > maxReleasedSessions {
		writeError(w, http.StatusBadRequest, "SESSIONS_INVALID",
			fmt.Sprintf("sessions holds %d names; a release names 1 to %d, and a caller with more sends them in more than one call", len(body.Sessions), maxReleasedSessions))
		return
	}
	slices.Sort(body.Sessions)
	names := slices.Compact(body.Sessions)
	released, err := s.deps.OAuth.ReleasePodSessions(r.Context(), registrar.Subject, names)
	if err != nil {
		writeInternal(w, "release the pod session names", err)
		return
	}
	slog.Info("broker: released a registrar's pod-session registrations",
		"service", registrar.Service, "registrar", registrar.Subject, "sessions", len(names), "released", released)
	writeJSON(w, http.StatusOK, podSessionReleaseResponse{Sessions: len(names), Released: released})
}
