// handlers_ui_machinelogin.go: POST /v1/machine-logins/lookup — resolves a pending machine login
// by its human-readable confirmation code, for the operator's own page (decision 13: a direct link
// can never approve a machine login, only the typed code selects it) — and GET
// /v1/launcher-credentials and POST /v1/launcher-credentials/{id}/revoke-by-approver, the machine
// logins a person may revoke that can still reach a secret, and the route that ends one. Part of
// the routes a listed service calls with its own ServiceAccount token (accountAuth); a machine
// login is decided on the caller's word with its typed code (handlers_ui_records.go).
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/enroll"
	"github.com/sjawhar/envoy/internal/cipher/machine"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/wire"
)

// lookupMachineLoginBody is wire.MachineLookupRequest under the name cmd/cipher-refgen reads
// from the call site (bodies.go), as the package's eight response aliases are.
type lookupMachineLoginBody = wire.MachineLookupRequest

// lookupMachineLogin resolves a pending machine login by its human-readable confirmation code, the
// user code the machine's device authorization minted, and is the only route
// that selects a machine record. Deciding it takes the same code again (CODE_REQUIRED /
// CODE_MISMATCH), so a direct link to the record can never approve it (decision 13).
func (s *server) lookupMachineLogin(w http.ResponseWriter, r *http.Request, _ string) {
	var body lookupMachineLoginBody
	if !readJSON(w, r, &body, "INVALID_LOOKUP") {
		return
	}
	if body.Code == "" {
		writeError(w, http.StatusBadRequest, "CODE_REQUIRED", "code is required")
		return
	}
	view, err := s.deps.MachineLogin.LookupByCode(r.Context(), body.Code)
	switch {
	case errors.Is(err, machine.ErrNotFound):
		writeError(w, http.StatusNotFound, "NO_SUCH_CODE", "no pending machine login has this code")
		return
	case err != nil:
		writeInternal(w, "lookup machine login", err)
		return
	}
	detail, err := s.deps.Machine.ReadRecord(r.Context(), view.RecordID)
	if err != nil {
		writeInternal(w, "read credential request", err)
		return
	}
	writeJSON(w, http.StatusOK, buildRecordResponse(detail))
}

// launcherCredentialsResponse is GET /v1/launcher-credentials's and GET /v1/operator/machines's
// answer: wire.LauncherCredentialsResponse itself, named here because the broker's generated
// HTTP reference documents a route's answer from a type this package declares, as
// introspectionResponse names oauthwire.Introspection (handlers_introspect.go).
type launcherCredentialsResponse = wire.LauncherCredentialsResponse

// listLauncherCredentials lists the machine logins the person ?approver= names may revoke
// (enroll.Service.LiveCredentials, through writeLauncherCredentials): for a person Dispatch calls a
// restricted member, who is no approver of a secret and sees nothing beyond their own projects,
// and for anyone while Dispatch cannot say what kind of member the person is, their own machine
// logins alone (enroll.Service.OwnLiveCredentials), as their own machine lists them
// (machineLoginsOwnOnly).
func (s *server) listLauncherCredentials(w http.ResponseWriter, r *http.Request, _ string) {
	approver := r.URL.Query().Get("approver")
	if !requireApprover(w, approver) {
		return
	}
	list := s.deps.Enroll.LiveCredentials
	if s.machineLoginsOwnOnly(r, approver, "list") {
		list = s.deps.Enroll.OwnLiveCredentials
	}
	s.writeLauncherCredentials(w, r, list, approver)
}

// machineLoginsOwnOnly reports whether the machine-login routes Dispatch relays for person hold
// person to their own machine logins (enroll.Service.OwnLiveCredentials, RevokeOwnCredential): for
// a person Dispatch calls a restricted member (projectaccess.Restricted), since a service's login is
// every signed-in person's only as an approver of it, and ending one ends every session it
// enrolled; and for anyone while Dispatch cannot say what kind of member they are, which it logs
// (MachineLoginsOwnOnlyMessage) once per person per ownOnlyLogEvery, unless the caller went away.
// A person's own logins stay theirs to list and end whatever Dispatch answers, as a lost machine's
// login must, and a service's login is out of reach of a person the broker cannot read, as it is
// of a restricted member. route names the route for the log line. It asks Dispatch through the
// enroll service's client within projectaccess.RelayedReadBound, and asks nothing on a broker with
// no client of Dispatch.
func (s *server) machineLoginsOwnOnly(r *http.Request, person, route string) bool {
	if s.deps.Enroll.Projects == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), projectaccess.RelayedReadBound)
	defer cancel()
	login := record.CanonicalLogin(person)
	restricted, err := projectaccess.Restricted(ctx, s.deps.Enroll.Projects, login)
	if err != nil {
		if r.Context().Err() == nil && s.ownOnlyLogged.Allow(login) {
			slog.Warn(MachineLoginsOwnOnlyMessage, "route", route, "person", login, "error", err)
		}
		return true
	}
	return restricted
}

// MachineLoginsOwnOnlyMessage is logged, at WARN, when a machine-login list or revoke held the
// person to their own logins because Dispatch could not say whether they are a restricted member
// (machineLoginsOwnOnly), at most once per person per ownOnlyLogEvery, followed by route=<list or
// revoke>, person=<the canonical login> and error=<why it could not ask>.
const MachineLoginsOwnOnlyMessage = "broker: a machine-login route holds the person to their own logins, since Dispatch could not say whether the person is a restricted member"

// ownOnlyLogEvery is how often MachineLoginsOwnOnlyMessage may name one person: Dispatch's page
// reads the list each time it opens, so an outage would otherwise write a line for every open and
// every revoke, and one line a minute still dates the outage for that person.
const ownOnlyLogEvery = time.Minute

// writeLauncherCredentials answers the machine logins list names for person, each one that can
// still reach a secret: not revoked, and either unexpired or expired with a session it enrolled
// still running. Dispatch's machine-logins page lists every service's login, whoever approved it,
// and the person's own machines' and pod logins (enroll.Service.LiveCredentials); the person's own
// machine lists their machines' and pod logins alone (enroll.Service.OwnLiveCredentials). A
// person's pod login names its person (wire.LauncherCredentialResp.Person), so Dispatch shows it
// as the person's own and not as a service's login. Both answer through it, so each row the two
// share is the same bytes.
func (s *server) writeLauncherCredentials(w http.ResponseWriter, r *http.Request, list func(context.Context, string) ([]enroll.LiveCredential, error), person string) {
	rows, err := list(r.Context(), person)
	if err != nil {
		writeInternal(w, "list launcher credentials", err)
		return
	}
	out := make([]wire.LauncherCredentialResp, len(rows))
	for i, c := range rows {
		out[i] = wire.LauncherCredentialResp{CredentialID: c.ID.String(), Host: c.Host, Service: c.Service, Subject: c.Subject, Person: c.Person, ApprovedBy: c.ApprovedBy, IssuedAt: c.IssuedAt, ExpiresAt: c.ExpiresAt, Expired: c.Expired}
	}
	writeJSON(w, http.StatusOK, launcherCredentialsResponse{Credentials: out})
}

// revokeLauncherCredential ends a machine login as the person the caller names
// (enroll.Service.RevokeCredential, through revokeCredentialAs): a service's as anyone signed in, a
// person's machine's as the person who approved it, and a person's pod login as the person
// BROKER_POD_OPERATORS maps the pod's account to, and as nobody else, although such a login
// carries a service and a subject. A person Dispatch calls a restricted member ends their own
// logins alone (enroll.Service.RevokeOwnCredential), and a service's is 404 to them, as an id no
// login has; so does anyone while Dispatch cannot say what kind of member the person is
// (machineLoginsOwnOnly), so a lost machine's login is ended whatever Dispatch answers. The caller
// that relayed it is logged beside that person, whose name the broker still takes on the caller's
// word: a revoke only takes power away.
func (s *server) revokeLauncherCredential(w http.ResponseWriter, r *http.Request, callerName string) {
	id, ok := pathUUID(w, r, "id", "CREDENTIAL_ID_INPUT", "launcher credential")
	if !ok {
		return
	}
	var body revokeByApproverBody
	if !readJSON(w, r, &body, "INVALID_REVOKE") {
		return
	}
	if !requireApprover(w, body.Approver) {
		return
	}
	// The approver is the caller's word, of any length requireApprover takes, so the line cuts it
	// (answer.ClipFields).
	slog.Info("machine login revocation relayed", answer.ClipFields([]any{"caller", callerName, "approver", body.Approver, "credential", id})...)
	revoke := s.deps.Enroll.RevokeCredential
	if s.machineLoginsOwnOnly(r, body.Approver, "revoke") {
		revoke = s.deps.Enroll.RevokeOwnCredential
	}
	s.revokeCredentialAs(w, r, revoke, id, body.Approver, record.HumanActor(body.Approver))
}

// revokeCredentialAs ends machine login id, expired or not, with revoke on the word of person
// (enroll.Service.RevokeCredential: a service's for anyone, a person's machine's for the person who
// approved it, a person's pod login for the person BROKER_POD_OPERATORS maps the pod's account to;
// RevokeOwnCredential: a person's machine's and that person's pod logins alone), recording actor on
// every row it writes: no launcher proof signed with it authenticates again, and every session it
// enrolled ends — a service's login's pods and a person's pod among them — with their grants and
// pending requests. Revoking one already revoked answers the same.
func (s *server) revokeCredentialAs(w http.ResponseWriter, r *http.Request, revoke func(ctx context.Context, id, person, actor string) error, id, person, actor string) {
	err := revoke(r.Context(), id, person, actor)
	switch {
	case errors.Is(err, enroll.ErrNoCredential):
		writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	case errors.Is(err, enroll.ErrNotApprover):
		writeError(w, http.StatusForbidden, "NOT_APPROVER", err.Error())
		return
	case err != nil:
		writeInternal(w, "revoke launcher credential", err)
		return
	}
	writeJSON(w, http.StatusOK, stateResponse{State: "revoked"})
}
