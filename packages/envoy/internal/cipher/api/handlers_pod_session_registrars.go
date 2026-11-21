package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
)

// podSessionBindingBody is PUT /v1/pod-session-registrars/{session}'s exact shape.
type podSessionBindingBody struct {
	// The login account of the daemon the name is to be bound to: a ServiceAccount subject that an
	// entry of BROKER_POD_REGISTRARS names as its registrar.
	Registrar string `json:"registrar"`
}

// podSessionBinding is one session name's binding as GET /v1/pod-session-registrars lists it.
type podSessionBinding struct {
	// The session name.
	SessionName string `json:"session_name"`
	// The login account of the registrar the name is bound to: the only registrar that registers
	// pods under it, and the only one whose registrations trade under it.
	Registrar string `json:"registrar"`
	// When the name was bound to that registrar: its first registration, or the operator's last
	// move of it.
	RegisteredAt time.Time `json:"registered_at"`
}

// podSessionMoveAnswer is PUT /v1/pod-session-registrars/{session}'s answer: the binding as the
// move stored it, and the registrar it replaced.
type podSessionMoveAnswer struct {
	// The session name.
	SessionName string `json:"session_name"`
	// The registrar the name is now bound to, the one the body named.
	Registrar string `json:"registrar"`
	// The moment of the move, from which the name is bound to that registrar.
	RegisteredAt time.Time `json:"registered_at"`
	// The registrar the name was bound to before the move, the body's own for a move that changed
	// nothing; absent for a name no registrar was bound to.
	PreviousRegistrar string `json:"previous_registrar,omitempty"`
}

// podSessionBindingsAnswer is GET /v1/pod-session-registrars's answer.
type podSessionBindingsAnswer struct {
	// Every session name's binding, ordered by session name; empty when no name is bound.
	Bindings []podSessionBinding `json:"bindings"`
}

// movePodSessionBinding is PUT /v1/pod-session-registrars/{session}: the deployment's operator binds
// a session name to the daemon the body names, whatever daemon it is bound to now. A session name
// belongs to the daemon that first registered it at POST /v1/oauth/exchange/pod-sessions
// (oauthstore.Store.RegisterPodSession), through release and lapse alike, and this route is the
// one way it changes hands: a project that moves its sessions from a
// daemon to another, as a project that turns private moves them to its own daemon, has the operator
// move each name the first daemon bound before the second registers it, since the second is refused
// every name the first holds.
//
// Only the operator calls it (operatorAuth): the operator entry of BROKER_NATS_SERVICES, presenting
// its own platform token, which the auth service's own verifier verifies; every other caller, every
// other entry's token and every daemon's broker login among them, is 403 not_operator. A daemon that
// could move a name could take another daemon's sessions, which is what the binding is there to
// stop.
//
// The body is exactly {"registrar": "<daemon login account>"}, at most 1 MiB (413 too_large); an
// unknown field or a body that is not one JSON object is 400 bad_request. A session name no
// registration could name (exchange.ValidSession) is 400 session_invalid, and a registrar that no
// BROKER_POD_REGISTRARS entry names as a registrar, for any pod account, is 400
// registrar_not_listed, naming it: a name bound to such an account would admit no registration of
// it at all. Each refusal moves nothing.
//
// The move is one transaction that reads the name's binding locked and changes it in one statement
// (oauthstore.Store.MovePodSessionBinding), and binds a name no registrar was bound to as well. It
// never removes the binding, since a removal would leave the name to whichever daemon registered it
// first in between: every registration of the name by another daemon is refused, before the move
// and after it. From the move's commit the name is the new daemon's: its registrations under the
// name are admitted, the old daemon's are refused 409 SESSION_NAME_HELD_BY_ANOTHER_REGISTRAR, and the
// exchange refuses each live registration of the name the old daemon wrote with the final
// invalid_grant session_name_not_this_registrars:. It revokes no access token already minted, which
// lives to its own expiry (BROKER_ACCESS_TOKEN_SECONDS). The answer is 200 with the binding as
// stored and the registrar it replaced.
//
// Each move is logged at Info, naming the operator, the session, the registrar it replaced ("" for
// none) and the registrar it is bound to, since the operator is the authority here and these lines
// are the record of which name changed hands, and when.
func (s *server) movePodSessionBinding(w http.ResponseWriter, r *http.Request, operator string) {
	var body podSessionBindingBody
	if !readHoldBody(w, r, &body) {
		return
	}
	session := r.PathValue("session")
	if !exchange.ValidSession(session) {
		writeHoldRefusal(w, http.StatusBadRequest, "session_invalid",
			fmt.Sprintf("the session name of %d bytes is not one a registration could name: printable UTF-8 with no whitespace, at most %d bytes", len(session), exchange.MaxSessionBytes), nil)
		return
	}
	if !s.deps.PodRegistrars.ListsRegistrar(body.Registrar) {
		writeHoldRefusal(w, http.StatusBadRequest, "registrar_not_listed",
			fmt.Sprintf("%q is not a registrar an entry of BROKER_POD_REGISTRARS names, so a session name bound to it would admit no registration", body.Registrar), nil)
		return
	}
	binding, previous, err := s.deps.OAuth.MovePodSessionBinding(r.Context(), session, body.Registrar)
	if err != nil {
		writeUnavailable(w, "DATABASE_UNAVAILABLE", "move the session name's binding", err)
		return
	}
	slog.Info("broker: the operator moved a session name's binding to another registrar",
		"operator", operator, "session", session, "previous_registrar", previous, "registrar", binding.Registrar)
	writeJSON(w, http.StatusOK, podSessionMoveAnswer{
		SessionName: binding.Session, Registrar: binding.Registrar, RegisteredAt: binding.RegisteredAt, PreviousRegistrar: previous,
	})
}

// listPodSessionBindings is GET /v1/pod-session-registrars: every session name's binding, for the
// operator alone (operatorAuth), ordered by session name, as the operator reads which names a daemon
// holds before it moves them.
func (s *server) listPodSessionBindings(w http.ResponseWriter, r *http.Request, _ string) {
	stored, err := s.deps.OAuth.PodSessionBindings(r.Context())
	if err != nil {
		writeUnavailable(w, "DATABASE_UNAVAILABLE", "list the session names' bindings", err)
		return
	}
	bindings := make([]podSessionBinding, 0, len(stored))
	for _, binding := range stored {
		bindings = append(bindings, podSessionBinding{SessionName: binding.Session, Registrar: binding.Registrar, RegisteredAt: binding.RegisteredAt})
	}
	writeJSON(w, http.StatusOK, podSessionBindingsAnswer{Bindings: bindings})
}
