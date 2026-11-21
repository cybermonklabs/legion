// The role withhold routes: PUT and DELETE /v1/role-withholds, through which a Legion daemon keeps
// a role from a session its claim does not hold and gives it back, and the session a role lookup
// answers for (roleReadActingSession). The withhold itself is the store's (store.Registry.
// WithholdRole and LiftRoleWithhold); what is here is who may ask for it.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/listenerwire"
)

// roleWithheldByAnotherError is the error PUT /v1/role-withholds answers 409 with where another
// daemon's withhold stands on the role.
const roleWithheldByAnotherError = "role_withheld_by_another"

// roleWithholdHandler serves PUT /v1/role-withholds, which withholds a role from a session
// (store.Registry.WithholdRole: the session's standing claim is freed and no claim of the role by
// it lands), and DELETE /v1/role-withholds, which lifts that withhold (store.Registry.
// LiftRoleWithhold), each with the body listenerwire.RoleWithholdRequest. Both answer 204 with no
// body; a lift of a role not withheld from that session answers 204 and changes nothing.
//
// Only a Legion daemon's own broker access token may call either (daemonCaller): a token whose
// session is legion-daemon:<K> and whose one project is K, for a role of project K
// (contracts.RoleProject), and the withhold records that session as its writer, so only a token
// of the same legion-daemon:<K> lifts it. Every other caller is refused 403 and nothing changes:
// a session's own broker token, the withheld session's included, whichever session it names; the
// shared bearer and a service-account token, which prove no session; and a request with no
// credential on an open listener. So this route does not take the authorization of DELETE
// /v1/interests/<session_id>, which admits a broker token acting on its own session and would let
// the withheld session lift its own withhold. A withhold another daemon wrote is 409
// role_withheld_by_another on a PUT and 403 on a DELETE, and stands.
//
// logger is registerV1Routes's own: each refused caller and each withhold and lift is logged
// once, naming the role, the session and the daemon, never a token.
func roleWithholdHandler(d *listenerDeps, logger *logging.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut && r.Method != http.MethodDelete {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		project, ok := daemonCaller(r)
		if !ok {
			logger.Warn("listener refused a role withhold: the caller is no Legion daemon's own broker token",
				slog.String("method", r.Method))
			writeJSONError(w, http.StatusForbidden, "only a Legion daemon's own broker access token (session legion-daemon:<project>) may withhold or lift a role")
			return
		}
		var body listenerwire.RoleWithholdRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid json")
			return
		}
		body.Role = strings.TrimSpace(body.Role)
		body.Session = strings.TrimSpace(body.Session)
		if body.Session == "" {
			writeJSONError(w, http.StatusBadRequest, "session is required", "session")
			return
		}
		if body.Role == "" {
			writeJSONError(w, http.StatusBadRequest, "role is required", "role")
			return
		}
		// The role is read as POST /v1/roles/set reads it, in either form, and its claim record
		// is keyed by the name (store.Registry.ClaimedRole).
		rolesProject, name, legacy := contracts.CanonicalRole(body.Role)
		if !roles.Valid(name) {
			writeJSONError(w, http.StatusBadRequest, "role must match "+roles.PatternString+", after its project and a dot when it names one", "role")
			return
		}
		own := contracts.RoleProject(name)
		if !legacy && rolesProject != own {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("role %s works for project %s, the project its name gives, not %s", name, own, rolesProject), "role")
			return
		}
		daemon := daemonSessionPrefix + project
		if own != project {
			logger.Warn("listener refused a role withhold: the role is not the daemon's project's",
				slog.String("role", name), slog.String("session_id", body.Session), slog.String("daemon", daemon))
			writeJSONError(w, http.StatusForbidden, fmt.Sprintf("role %s works for project %s, and %s withholds only roles of project %s", name, own, daemon, project), "role")
			return
		}
		key, _ := d.registry.ClaimedRole(body.Role)
		ctx, cancel := context.WithTimeout(r.Context(), sendRequestBudget)
		defer cancel()
		write, done := d.registry.WithholdRole, "withheld"
		if r.Method == http.MethodDelete {
			write, done = d.registry.LiftRoleWithhold, "lifted the withhold of"
		}
		err := write(ctx, key, body.Session, daemon)
		switch {
		case err == nil:
			logger.Info("listener "+done+" a role",
				slog.String("role", key), slog.String("session_id", body.Session), slog.String("daemon", daemon))
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, store.ErrRoleWithheldByOther) && r.Method == http.MethodPut:
			writeJSON(w, http.StatusConflict, map[string]string{"error": roleWithheldByAnotherError, "role": key})
		case errors.Is(err, store.ErrRoleWithheldByOther):
			logger.Warn("listener refused a lift: another daemon withheld the role",
				slog.String("role", key), slog.String("session_id", body.Session), slog.String("daemon", daemon))
			writeJSONError(w, http.StatusForbidden, err.Error())
		case errors.Is(err, store.ErrMalformedRoleClaim):
			writeRoleClaimUnreadable(w, key, err)
		case errors.Is(err, nats.ErrKeyExists):
			// Every write lost its condition to a write of the role's record that landed after
			// its read, the role's own holder renewing it at each try: nothing was written.
			w.Header().Set("Retry-After", "1")
			writeJSONError(w, http.StatusServiceUnavailable, err.Error())
		default:
			writeStoreError(ctx, w, err, http.StatusInternalServerError)
		}
	}
}

// roleReadActingSession is the session GET /v1/roles/<role> answers for, which is what decides
// whether its answer says the role is withheld from it: the session a broker access token names,
// or, under any other credential, the session the request states in ?session_id=, which every
// route believes from a credential that proves no session (brokerauth.go). A broker token
// stating another session than its own is refused, as sessionNotBound refuses a write that acts
// as another session: the answer says something about the session it is for. No session at all
// is answered as before, withheld from nobody.
func roleReadActingSession(r *http.Request) (session, refusal string) {
	stated := strings.TrimSpace(r.URL.Query().Get("session_id"))
	caller, ok := brokerCaller(r)
	switch {
	case !ok:
		return stated, ""
	case stated == "" || stated == caller.Session:
		return caller.Session, ""
	default:
		return "", fmt.Sprintf("this access token acts as session %s, so it may not read a role as %s", caller.Session, stated)
	}
}
