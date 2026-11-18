package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/sjawhar/envoy/internal/broker/exchange"
	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/oidc"
)

// thumbprintShape is an RFC 7638 SHA-256 thumbprint as a registrar sends it: 32 bytes,
// base64url-encoded with no padding.
var thumbprintShape = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// podSessionBody is POST /v1/oauth/exchange/pod-sessions's exact shape.
type podSessionBody struct {
	// The pod's own projected Kubernetes ServiceAccount token, whose audience is the broker's
	// (BROKER_K8S_OIDC_AUDIENCE): the very token the registrar hands POST /v1/enrollments as it
	// enrolls that pod. It is what names the pod — its account is the token's subject and its uid
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
// in as the service BROKER_SERVICE_LOGINS names its account (serviceAuth), records that one pod
// may exchange under one session name. The token exchange binds a session name to a principal —
// the listed service account — and never to a pod, so two pods of one account are one principal
// there and nothing in that claim can tell them apart; this row is what does, and the exchange
// refuses a pod of a free-session account whose uid no registration names for the name it asks
// for. A relaunch is a second pod uid registered for the same name, so a successor is never
// refused what its predecessor held.
//
// The body's token is verified with the verifier an enrolling pod's token is verified with
// (Deps.Enroll.Pod), which the serviceAuth class has already proved the broker has: an expired
// token is 401 POD_TOKEN_EXPIRED and one that does not verify 401 POD_TOKEN_INVALID. The pod it
// names must not be the caller's own: a token naming the account the caller logs in as and the
// very pod the caller's own token names is 403 POD_CALLER_IS_THE_POD. A registrar records which
// pod may write under which name, and a pod that wrote its own row would pick the name it
// writes under, which is the one choice this row takes from it. BROKER_SERVICE_LOGINS may name
// an account BROKER_SERVICES binds to a service, so every pod of that account logs the service
// in and reaches this route; without this a session pod would register itself under the name a
// sibling pod holds. A registrar whose own token names no pod, and one registering any pod but
// the one it runs in, are untouched. The policy
// then refuses a token bound to no pod (400 POD_NOT_BOUND), an account neither
// BROKER_EXCHANGE_ACCOUNTS nor BROKER_EXCHANGE_READ_ACCOUNTS lists (403 POD_ACCOUNT_NOT_LISTED, a
// code of its own, since the caller's own list refusing is ACCOUNT_NOT_LISTED and the two lists
// are two facts), an account whose entry fixes a session, which every read entry does (409
// SESSION_FIXED, since such a pod needs no registration and registering one would say something
// the configuration already says), and a session name no request could name back (400
// SESSION_INVALID). Each refusal names the account or the name it refused and repeats no token.
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
// as registered, naming the thumbprint and generation the registrar sent beside the ones stored,
// and the thumbprint it replaced when it bound the row to another key; one that names no key and
// no generation, of a row that names neither, logs the line it logged before either existed (R9).
// A write the generation rule refused changed nothing, so it is not logged as registered: it logs
// a line of its own at Warn, naming what it sent beside the row it left as it was.
func (s *server) registerPodSession(w http.ResponseWriter, r *http.Request, registrar machine.ServiceAccount) {
	var body podSessionBody
	if !readJSON(w, r, &body, "INVALID_REQUEST") {
		return
	}
	claims, err := s.deps.Enroll.Pod.VerifyAccount(r.Context(), body.PodToken)
	switch {
	case errors.Is(err, oidc.ErrExpired):
		writeError(w, http.StatusUnauthorized, "POD_TOKEN_EXPIRED", "the pod's ServiceAccount token has expired")
		return
	case err != nil:
		writeError(w, http.StatusUnauthorized, "POD_TOKEN_INVALID", "the pod's ServiceAccount token does not verify")
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
	stored, err := s.deps.OAuth.RegisterPodSession(r.Context(), claims.Subject, claims.PodUID, body.Session, body.Thumbprint, body.Generation)
	switch {
	case errors.Is(err, exchange.ErrNoPodBinding):
		writeError(w, http.StatusBadRequest, "POD_NOT_BOUND", err.Error())
	case errors.Is(err, exchange.ErrNoEntry):
		writeError(w, http.StatusForbidden, "POD_ACCOUNT_NOT_LISTED", err.Error())
	case errors.Is(err, exchange.ErrSessionFixed):
		writeError(w, http.StatusConflict, "SESSION_FIXED", err.Error())
	case errors.Is(err, exchange.ErrSessionInvalid):
		writeError(w, http.StatusBadRequest, "SESSION_INVALID", err.Error())
	case err != nil:
		writeInternal(w, "register the pod's session name", err)
	default:
		attrs := []any{"service", registrar.Service, "account", claims.Subject, "pod", claims.PodUID, "session", body.Session}
		if body.Thumbprint != "" || body.Generation != 0 || stored.JKT != "" || stored.Generation != 0 {
			attrs = append(attrs, "sent_thumbprint", body.Thumbprint, "sent_generation", body.Generation,
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
