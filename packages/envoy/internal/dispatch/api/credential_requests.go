// credential_requests.go relays Dispatch's credential pages to the secrets broker (its account
// routes). Dispatch lists the credential requests waiting on the signed-in person, and the expired
// ones nobody has dismissed, each secret request carrying the broker's approve_url, the page where
// that person, signed in to the broker, decides it; it relays a machine login's approve or deny
// with the code the person typed; it relays a person's dismissal of an expired request; and it
// relays a person's revoke of a grant or a machine login. Every handler does the same five things:
// require a human caller, require the broker to be configured, resolve or read its input, call
// the matching cipher.Client method, and forward the broker's exact status and body — the
// broker decides. The pending list alone answers null rather than 404 FEATURE_OFF without a broker.
// The one thing Dispatch supplies is who acts: a machine login's approve and deny, a dismissal, a
// revoke and the machine-login list send the login actingPerson resolved, in Dispatch's canonical
// lowercase form, as the approver, which the broker takes on Dispatch's word, and never forward
// the browser's body, so nothing a browser sends can name the approver.
//
// These decisions write no Dispatch event - the broker records the decision itself - so the agent
// that made one for its person is recorded on Dispatch's own access-log line instead
// (noteActingAgent), which is the only record of the decision Dispatch keeps.
package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/cipher"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// noteActingAgent names the agent behind a decision on this request's access-log line: the
// session the act names, and otherwise the personal token, by its owner, which is what the line
// already says of a token that named no session (accesslog.KindToken). A browser's actor names
// no agent, so its line keeps the person optionalActor noted. The last Note wins and a session
// noted after a token keeps that token as the line's credential, so one line still carries both
// who acted and what they presented.
func noteActingAgent(r *http.Request, actor model.Actor) {
	if actor.Act == nil {
		return
	}
	if actor.Act.Session != "" {
		accesslog.Note(r.Context(), accesslog.KindSession, actor.Act.Session)
		return
	}
	accesslog.Note(r.Context(), accesslog.KindToken, actor.ID)
}

// requireAgentSecrets answers 404 FEATURE_OFF when this Dispatch has no broker configured
// (DISPATCH_AGENT_SECRETS_URL unset); every credential-request route but the pending list needs it
// after actingPerson.
func (s *server) requireAgentSecrets(w http.ResponseWriter) (*cipher.Client, bool) {
	if s.deps.AgentSecrets == nil {
		writeError(w, "FEATURE_OFF", http.StatusNotFound, "agent-secrets is not configured on this Dispatch")
		return nil, false
	}
	return s.deps.AgentSecrets, true
}

// relayBrokerResponse writes the broker's answer verbatim: its raw JSON body on success, its
// exact status and body for a *cipher.Error (the browser must see the broker's own status
// and code, e.g. 409 RECORD_TERMINAL — Dispatch never re-wraps it), 500
// AGENT_SECRETS_ACCOUNT_TOKEN_UNREADABLE for a call that stopped at Dispatch's own ServiceAccount
// token, or 503 AGENT_SECRETS_UNAVAILABLE for anything else (a transport failure). The token
// gets its own code because the broker was never asked: the fix is in Dispatch's deployment, and
// the log line naming the file is the operator's only pointer to it.
func relayBrokerResponse(w http.ResponseWriter, body json.RawMessage, err error) {
	if err != nil {
		var brokerErr *cipher.Error
		if errors.As(err, &brokerErr) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(brokerErr.Status)
			_, _ = w.Write(brokerErr.Body)
			return
		}
		var tokenErr *cipher.AccountTokenError
		if errors.As(err, &tokenErr) {
			slog.Error("dispatch: cannot read its ServiceAccount token for the secrets broker", "path", tokenErr.Path, "error", tokenErr.Err)
			writeError(w, "AGENT_SECRETS_ACCOUNT_TOKEN_UNREADABLE", http.StatusInternalServerError, "Dispatch cannot read its own ServiceAccount token for the secrets broker")
			return
		}
		writeError(w, "AGENT_SECRETS_UNAVAILABLE", http.StatusServiceUnavailable, "the secrets broker is unreachable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// resolveApproverMe reads ?approver=, which the UI only ever sends as the literal string "me" —
// it asks for the viewer's own list, never a login it names itself — and resolves it to the
// caller's canonical login. Anything else is refused before it ever reaches the broker.
func resolveApproverMe(w http.ResponseWriter, r *http.Request, actor model.Actor) (string, bool) {
	if approver := r.URL.Query().Get("approver"); approver != "me" {
		writeError(w, "APPROVER_ME_ONLY", http.StatusBadRequest, `approver must be the literal value "me"`)
		return "", false
	}
	return model.CanonicalLogin(actor.ID), true
}

// readRelayBody reads a mutation's body verbatim, capped like every other JSON mutation, and
// hands it to the broker unparsed: Dispatch relays, it does not model these shapes. A string the
// body holds that carries U+0000 is refused as decodeJSON refuses one: the broker reads these
// strings against PostgreSQL too (a machine login's typed code is a query's parameter there), and
// Dispatch is these routes' only caller.
func (s *server) readRelayBody(w http.ResponseWriter, r *http.Request) (json.RawMessage, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONRequestBytes))
	if err != nil {
		writeError(w, "REQUEST_TOO_LARGE", http.StatusRequestEntityTooLarge, "request body exceeds the size limit")
		return nil, false
	}
	if refusal := unstorableJSON("", body); refusal != nil {
		s.writeHandlerError(w, refusal)
		return nil, false
	}
	return json.RawMessage(body), true
}

// credentialDecisionInput is the body of POST /api/v1/credential-requests/{id}/approve and
// POST /api/v1/credential-requests/{id}/deny.
type credentialDecisionInput struct {
	Code *string `json:"code"`
}

// decisionFor builds the broker's decision body for an approve or deny: the approver is the
// caller's canonical login, and the one field read from the browser's body is a machine login's
// typed code. Any other field the browser sends, an approver among them, is ignored, never
// forwarded. An empty body is a decision with no code.
func (s *server) decisionFor(w http.ResponseWriter, r *http.Request, actor model.Actor) (cipher.Decision, bool) {
	body, ok := s.readRelayBody(w, r)
	if !ok {
		return cipher.Decision{}, false
	}
	var input credentialDecisionInput
	if len(body) > 0 {
		if err := json.Unmarshal(body, &input); err != nil {
			writeError(w, "INVALID_DECISION", http.StatusBadRequest, "request body must be a JSON object")
			return cipher.Decision{}, false
		}
	}
	return cipher.Decision{Approver: model.CanonicalLogin(actor.ID), Code: input.Code}, true
}

// --- GET /api/v1/credential-requests ---

// listCredentialPending answers the viewer's list, the requests still pending and then the expired
// ones nobody has dismissed (cipher.Client.Pending asks the broker for both), or null when
// this Dispatch has no broker. Every page reads this list (the Needs-you badge counts it), and a
// deployment without a broker is an ordinary one, so "no broker" is an answer here rather than the
// 404 FEATURE_OFF the other credential routes give: a 404 made every page of such a deployment log
// a failed request.
func (s *server) listCredentialPending(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	login, ok := resolveApproverMe(w, r, actor)
	if !ok {
		return
	}
	if s.deps.AgentSecrets == nil {
		WriteJSON(w, http.StatusOK, nil)
		return
	}
	body, err := s.deps.AgentSecrets.Pending(r.Context(), login)
	relayBrokerResponse(w, body, err)
}

// --- POST .../{id}/approve, .../{id}/deny, .../machine-lookup ---

// approveCredentialRecord and denyCredentialRecord decide a machine login, whose decision carries
// the code the person typed. A secret request is decided on the broker's own page alone: the broker
// answers either route for one 403 DECIDE_ON_BROKER_PAGE with its approve_url, relayed verbatim like
// every answer.
//
// A decision is relayed only while its caller still holds the address: an approve mints a machine
// login in the approver's name, and the broker decides a denial whatever account asks, so a deny
// relayed for a caller who lost the address would let the replaced account deny the new holder's
// pending machine login. The caller resolved before the body was read, which a client may hold
// back as long as it likes (and a keyed decision resolves before its body is spooled), so once the
// body has arrived the caller's standing is read again, right before the broker is called
// (approverStanding), and a caller who lost the address since is refused as their next request
// would be, with no broker call. The broker's client bounds the rest to its own five seconds. The
// approve names the approving Google account too: the one bound to the credential that made it
// (credentialSubjectSQL), read in that same re-read, never whichever account holds the address when
// the approve is relayed. The broker records it on the login it mints, so a later revocation of that
// account ends the login, and the broker refuses an approval by an account already revoked. It
// carries the address's session generation the same re-read saw (approver_session_generation), so
// the broker refuses it once a revocation of a replacement after that read reaches it first,
// whatever account it names: a sign-in that commits between this check and the relay records its
// revocation at a greater generation. A deny mints nothing and names no account.
func (s *server) approveCredentialRecord(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	noteActingAgent(r, actor)
	client, ok := s.requireAgentSecrets(w)
	if !ok {
		return
	}
	decision, ok := s.decisionFor(w, r, actor)
	if !ok {
		return
	}
	subject, generation, err := s.approverStanding(r, actor)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	if subject != "" && s.deps.SignInIssuer != "" {
		decision.ApproverIssuer, decision.ApproverSubject = s.deps.SignInIssuer, subject
	}
	decision.ApproverSessionGeneration = &generation
	body, err := client.Approve(r.Context(), r.PathValue("id"), decision)
	relayBrokerResponse(w, body, err)
}

// approverStanding reads again whether a decision's caller (an approve's or a deny's), actor as
// actingPerson resolved it, still holds the address, and answers the Google account its own
// credential is bound to ("" for none, credentialSubjectSQL) and the address's session generation
// (user_sessions.generation, 0 when it holds none), in one statement, so the generation is the one
// the check of the caller saw:
//
//   - a cookie: refused once the login's session generation is another; its account is the
//     address's recorded `sub`, the cookie's own while the generation stands;
//   - a personal token: refused once revoked, or once its account no longer holds the address
//     (tokenAccountReplaced: people records another account, or the token records none and was
//     made before people recorded the address's); its account is the one its mint recorded.
//
// A broker-issued token never decides: none holds the approve scope these routes require
// (brokerPersonScopes), so it is refused before it gets here.
//
// A personal token that records no account, made after the recording, is bound to the address's
// account (credentialSubjectSQL). The read runs in a transaction of its own after the address's
// advisory lock, taken shared (store.AddressLockSharedSQL), the lock every sign-in that records the
// address's account takes first: a sign-in in flight makes the decision wait and then read what it
// recorded, so a credential that records no account and was made before that recording is never
// relayed while the sign-in is uncommitted, on an address no row holds too, however long the
// sign-in takes; a decision read before the sign-in takes the lock is relayed within the broker's
// request deadline, which brokerCredentialMargin covers. A caller who lost the address is refused
// as their next request is: 401 NO_IDENTITY for a cookie, 401 TOKEN_REVOKED for a personal token;
// a read that fails is 503 IDENTITY_UNAVAILABLE. It writes nothing.
//
// Each account check, and the account it answers, is the account binding's, and holds only while
// the binding does (accountsHeld, read in the same transaction after the address's lock): beside a
// build without the binding in the fleet the re-read checks no account and answers none, so the
// approve is relayed naming no account, as that build relays it.
func (s *server) approverStanding(r *http.Request, actor model.Actor) (subject string, generation int64, err error) {
	cookieLogin, cookieGeneration := s.mintCookie(r, actor)
	var tokenID *string
	if actor.Act != nil && actor.Act.TokenID != "" {
		tokenID = &actor.Act.TokenID
	}
	var cookieGone, tokenGone bool
	unread := func(err error) (string, int64, error) {
		slog.Warn("dispatch: a credential decision was refused: whether its caller still holds the address could not be read", "error", err)
		return "", 0, errorf(http.StatusServiceUnavailable, "IDENTITY_UNAVAILABLE",
			"whether this caller may still decide could not be read; try again in a minute")
	}
	tx, err := s.deps.Store.Pool.Begin(r.Context())
	if err != nil {
		return unread(err)
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(), store.AddressLockSharedSQL, actor.ID); err != nil {
		return unread(err)
	}
	held, err := s.accountsHeld(r.Context(), tx)
	if err != nil {
		return unread(err)
	}
	if err := tx.QueryRow(r.Context(), `
		select exists (select 1 from user_sessions where login = $2::text and generation <> $3::bigint),
			exists (select 1 from agent_tokens left join people on people.email = canonical_login(agent_tokens.owner)
				where agent_tokens.id = $4::uuid and (agent_tokens.revoked_at is not null or `+tokenAccountReplaced("$5::boolean")+`)),
			coalesce(`+credentialSubjectSQL("$1::text", "$2::text", "$4::uuid", "$5::boolean")+`, ''),
			coalesce((select generation from user_sessions where login = $1::text), 0)
	`, actor.ID, cookieLogin, cookieGeneration, tokenID, held).
		Scan(&cookieGone, &tokenGone, &subject, &generation); err != nil {
		return unread(err)
	}
	switch {
	case cookieGone:
		return "", 0, identity.ErrNoIdentity
	case tokenGone:
		return "", 0, errTokenRevoked()
	}
	return subject, generation, nil
}

func (s *server) denyCredentialRecord(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	noteActingAgent(r, actor)
	client, ok := s.requireAgentSecrets(w)
	if !ok {
		return
	}
	decision, ok := s.decisionFor(w, r, actor)
	if !ok {
		return
	}
	if _, _, err := s.approverStanding(r, actor); err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	body, err := client.Deny(r.Context(), r.PathValue("id"), decision)
	relayBrokerResponse(w, body, err)
}

func (s *server) lookupMachineCredential(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	noteActingAgent(r, actor)
	client, ok := s.requireAgentSecrets(w)
	if !ok {
		return
	}
	relayBody, ok := s.readRelayBody(w, r)
	if !ok {
		return
	}
	body, err := client.MachineLookup(r.Context(), relayBody)
	relayBrokerResponse(w, body, err)
}

// --- POST .../{id}/dismiss ---

// dismissCredentialRecord takes an expired credential request, a secret request or a machine login
// whose deadline passed before anybody decided it, off the caller's list for good, and off every
// other list that holds it. Like a revoke it needs no code, so the browser's body carries nothing
// Dispatch reads: the approver is the caller. The broker allows it only for a person whose list
// holds the record, and refuses a record that can still be decided with 409 RECORD_PENDING and
// one that was decided, or cancelled before its deadline, with 409 RECORD_TERMINAL.
func (s *server) dismissCredentialRecord(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	noteActingAgent(r, actor)
	client, ok := s.requireAgentSecrets(w)
	if !ok {
		return
	}
	body, err := client.Dismiss(r.Context(), r.PathValue("id"), model.CanonicalLogin(actor.ID))
	relayBrokerResponse(w, body, err)
}

// --- GET /api/v1/credential-grants, POST .../{id}/revoke ---

func (s *server) listCredentialGrants(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	client, ok := s.requireAgentSecrets(w)
	if !ok {
		return
	}
	login, ok := resolveApproverMe(w, r, actor)
	if !ok {
		return
	}
	body, err := client.Grants(r.Context(), login)
	relayBrokerResponse(w, body, err)
}

// revokeCredentialGrant ends a grant as the caller: the broker allows it only when the caller's
// login is the grant's approver or the person its enrollment acts for (an unmapped person's pod
// acts for no one), or, for a grant the model judge approved in anyone's stead, anyone who may see
// it. The browser's body carries nothing Dispatch reads.
func (s *server) revokeCredentialGrant(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	noteActingAgent(r, actor)
	client, ok := s.requireAgentSecrets(w)
	if !ok {
		return
	}
	body, err := client.RevokeByApprover(r.Context(), r.PathValue("id"), model.CanonicalLogin(actor.ID))
	relayBrokerResponse(w, body, err)
}

// --- GET /api/v1/machine-logins, POST .../{id}/revoke ---

// listMachineLogins answers the machine logins the caller may revoke that can still reach a
// secret: every service's login, whoever approved it, the caller's own machines', and the logins
// of the caller's own pods (pods whose Kubernetes account the broker's BROKER_POD_OPERATORS maps
// to the caller, each row's person naming them), an expired one whose sessions still run among
// them. Dispatch names the caller, so no one lists another person's machines or pods.
func (s *server) listMachineLogins(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	client, ok := s.requireAgentSecrets(w)
	if !ok {
		return
	}
	body, err := client.MachineLogins(r.Context(), model.CanonicalLogin(actor.ID))
	relayBrokerResponse(w, body, err)
}

// revokeMachineLogin ends a machine login, expired or not, and with it every session that login
// enrolled (a service's login: every pod it started): the broker allows it for a service's login
// from anyone signed in, for a person's machine login only when the caller is the person who
// approved it, and for a person's pod login only when the caller is the person the broker's
// BROKER_POD_OPERATORS maps the pod's account to. The browser's body carries nothing Dispatch
// reads.
func (s *server) revokeMachineLogin(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	noteActingAgent(r, actor)
	client, ok := s.requireAgentSecrets(w)
	if !ok {
		return
	}
	body, err := client.RevokeMachineLogin(r.Context(), r.PathValue("id"), model.CanonicalLogin(actor.ID))
	relayBrokerResponse(w, body, err)
}

// --- GET /api/v1/signins, POST .../{id}/revoke ---

// listSignIns answers the sign-ins the caller made on their machines with `agent-secrets login`,
// one per box key they have not revoked. Dispatch names the person from its own session, never
// from the request, so nobody lists another person's sign-ins; the broker lists only that
// person's.
func (s *server) listSignIns(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	client, ok := s.requireAgentSecrets(w)
	if !ok {
		return
	}
	body, err := client.SignIns(r.Context(), model.CanonicalLogin(actor.ID))
	relayBrokerResponse(w, body, err)
}

// revokeSignIn ends one of the caller's own sign-ins: that machine trades no token for them
// again and the access tokens already minted under it end with it, while their sign-ins on other
// machines keep working. Dispatch names the person, so a caller reaches none but their own: the
// broker answers a sign-in of anyone else as an unknown id. The browser's body carries nothing
// Dispatch reads.
func (s *server) revokeSignIn(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	noteActingAgent(r, actor)
	client, ok := s.requireAgentSecrets(w)
	if !ok {
		return
	}
	body, err := client.RevokeSignIn(r.Context(), r.PathValue("id"), model.CanonicalLogin(actor.ID))
	relayBrokerResponse(w, body, err)
}

// --- GET /api/v1/credentials/capabilities ---

// credentialCapabilities is what the credential pages may offer against the broker this Dispatch
// is configured with. It relays facts Dispatch reads from the broker rather than settings of its
// own: a browser reaches no broker, and a deployment that turns the device grant off turns it
// off in one place, the broker.
type credentialCapabilities struct {
	// Whether the page where a person types a machine's code is worth serving: whether, that
	// is, this broker still serves RFC 8628's device grant, read from its RFC 8414 metadata
	// (cipher.Client.ServesDeviceGrant). False for a Dispatch with no broker at all,
	// where no code is ever printed for anyone to type.
	MachineCodePage bool `json:"machine_code_page"`
}

// getCredentialCapabilities answers that one fact. Unlike every other route here it answers for
// a Dispatch with no broker rather than 404 FEATURE_OFF, because the question it answers is
// exactly "what can this deployment do": a 404 would leave the page unable to tell a Dispatch
// with no broker from a route it failed to reach.
func (s *server) getCredentialCapabilities(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.actingPerson(w, r); !ok {
		return
	}
	capabilities := credentialCapabilities{}
	if s.deps.AgentSecrets != nil {
		capabilities.MachineCodePage = s.deps.AgentSecrets.ServesDeviceGrant(r.Context())
	}
	WriteJSON(w, http.StatusOK, capabilities)
}
