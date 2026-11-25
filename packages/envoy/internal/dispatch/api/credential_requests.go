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
// A restricted member decides one kind of record here, at a browser alone: a pending machine login
// of their own, which is how an outside person approves the device login of their own agents
// (ownMachineLogin). Every other decision of theirs is refused before the broker is asked to make
// it. What such a member lists and ends Dispatch decides too, not the broker: their machine logins
// (memberMachineLogins) and their grants (memberGrants), the grants of the sessions that act for
// them alone.
//
// These decisions write no Dispatch event - the broker records the decision itself - so the agent
// that made one for its person is recorded on Dispatch's own access-log line instead
// (noteActingAgent), which is the only record of the decision Dispatch keeps.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/wire"
	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/cipher"
	"github.com/sjawhar/envoy/internal/dispatch/errkind"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// restrictedOwnLoginRoutes are the rows of the deployment a restricted member reaches, and at a
// browser alone (restrictedOwnLoginAtBrowser, which both gates read: withinProjectScope for the
// writes, withinReadScope for the read): the lookup of a machine login by the code its machine
// shows and that login's approve and deny, and the list of their machine logins and the revoke of
// one. Each handler then holds the member to a machine login of their own (ownMachineLogin for a
// pending one, memberMachineLogins, their own machines' and their pods', for an issued one).
// Through a token a restricted member is refused every one of them: no token of theirs holds a
// scope - their agent's named broker token, a broker token acting as them, and their personal
// token (personalTokenActor) - so gateRouteScopes, which runs first, answers 403
// OAUTH_SCOPE_REQUIRED, and restrictedOwnLoginRefusal's 403 RESTRICTED_MEMBER stands behind it for
// any credential of theirs that held a row's scopes; so no agent of theirs decides, lists or ends
// any credential, their own machine's included.
var restrictedOwnLoginRoutes = map[string]bool{
	"POST /api/v1/credential-requests/machine-lookup": true,
	"POST /api/v1/credential-requests/{id}/approve":   true,
	"POST /api/v1/credential-requests/{id}/deny":      true,
	"GET /api/v1/machine-logins":                      true,
	"POST /api/v1/machine-logins/{id}/revoke":         true,
}

// restrictedOwnLoginAtBrowser is the one rule of a restricted member's call of a row of
// restrictedOwnLoginRoutes, which both gates read. governed reports whether it decides the call:
// the row is one of them and reader's viewer is a restricted member's. allowed reports whether the
// call reaches the handler: the member is at their browser (atBrowser), whose cookie's credential
// holds no list and names no project (model.BrowserActor), so no list holds the call. A governed
// call that is not allowed is refused restrictedOwnLoginRefusal, by either gate; an ungoverned one
// meets the gate's own rule.
func restrictedOwnLoginAtBrowser(route apiRoute, reader model.Actor, r *http.Request) (governed, allowed bool) {
	if !restrictedOwnLoginRoutes[route.Method+" "+route.Pattern] || reader.Viewer == nil || !reader.Viewer.Restricted {
		return false, false
	}
	return true, atBrowser(r)
}

// restrictedOwnLoginRefusal answers a restricted member's call of a row of restrictedOwnLoginRoutes
// other than at their browser: 403 RESTRICTED_MEMBER, naming the row.
func restrictedOwnLoginRefusal(w http.ResponseWriter, route apiRoute) {
	writeError(w, codeRestrictedMember, http.StatusForbidden, fmt.Sprintf(
		"a restricted member reaches %s %s at their browser alone, for a machine login of their own, and never through a token", route.Method, route.Pattern))
}

// restrictedCaller reports whether the caller is a restricted member, read from the reader the
// route's gate (withinProjectScope, withinReadScope) resolved for this request.
func (s *server) restrictedCaller(w http.ResponseWriter, r *http.Request) (restricted, ok bool) {
	reader, ok := s.readerOf(w, r)
	if !ok {
		return false, false
	}
	return reader.Viewer != nil && reader.Viewer.Restricted, true
}

// machineLoginKind is the kind the broker's lookup answers for a machine login: its
// record.KindLauncherCredential, as wire.RecordResponse's Kind names it. Dispatch names it here
// rather than importing the broker's record package, which would build the broker's signing and
// proof code into the Dispatch server for one string; TestTheMachineLoginKindIsTheBrokers holds
// the two equal.
const machineLoginKind = "launcher_credential"

// ownMachineLogin looks up at the broker the machine login whose code lookup carries (the body of
// its POST /v1/machine-logins/lookup), and answers the broker's record when it is login's own: a
// machine login (machineLoginKind) whose approver is login, which no shared record's sentinel
// approver ("anyone") is, and which names no service; and, where recordID is
// not empty, the record recordID names, so a member's own code decides no other record. The broker
// names a machine login's token after its approver (exchange.Set.Machine), and holds a restricted
// member's to one project, so this is the one credential such a member may decide; the broker
// itself refuses a decision of one no longer pending. Any other record — another person's machine
// login, a service's, which anyone signed in may deny, a shared secret's request, a secret request
// of the member's own — is 403 RESTRICTED_MEMBER, naming none of its facts; a lookup the broker
// refused is relayed as the broker answered it. It answers false once it has written the answer.
func ownMachineLogin(w http.ResponseWriter, r *http.Request, client *cipher.Client, login string, lookup json.RawMessage, recordID string) (json.RawMessage, bool) {
	answer, err := client.MachineLookup(r.Context(), lookup)
	if err != nil {
		relayBrokerResponse(w, nil, err)
		return nil, false
	}
	var looked wire.RecordResponse
	own := json.Unmarshal(answer, &looked) == nil &&
		looked.Kind == machineLoginKind && namesNoService(looked.Service) &&
		model.CanonicalLogin(looked.Approver) == login &&
		(recordID == "" || looked.RecordID == recordID)
	if !own {
		writeError(w, codeRestrictedMember, http.StatusForbidden,
			"a restricted member decides only a pending machine login of their own, which names them as its approver and no service, by the code their machine shows")
		return nil, false
	}
	return answer, true
}

// namesNoService reports whether a broker record's service field names no service, as a person's
// own machine login does; a service's login, which anyone signed in may decide or revoke, names one.
func namesNoService(service *string) bool {
	return service == nil || *service == ""
}

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
//
// The account and the generation go only to a broker whose settings say it takes them
// (cipher.Client.TakesApproverFields): a broker that answers its settings without approver_fields,
// one from before them, refuses an approve carrying any of them 400 INVALID_DECISION, so it is
// sent the approver and the code alone. Settings that cannot be read leave that unknown, and the
// approve is relayed to no broker (writeApproverFieldsUnread): sent without the fields to a broker
// that takes them, it would leave that broker's account and generation checks to its 50-second
// fallback. The read comes after the re-read, so a refused caller asks the broker nothing, and it
// and the relay share one deadline, the client's window (cipher.New's five seconds) from the end of
// the re-read (relayDeadline): so the approve reaches the broker within that window of its check or
// not at all, the bound the broker's in-flight window counts (machine.ApprovalInFlightWindow),
// however long the read takes.
//
// A restricted member's approve is relayed holding their approval lock (relayMachineLoginApproval).
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
	if !s.restrictedDecisionAllowed(w, r, client, actor, decision.Code) {
		return
	}
	restricted, ok := s.restrictedCaller(w, r)
	if !ok {
		return
	}
	account, generation, err := s.approverStanding(r, actor)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	relay, cancel := relayDeadline(r.Context(), client)
	defer cancel()
	fields, err := client.TakesApproverFields(relay)
	if err != nil {
		writeApproverFieldsUnread(w, err)
		return
	}
	if fields {
		if account.subject != "" && s.deps.SignInIssuer != "" {
			decision.ApproverIssuer, decision.ApproverSubject = s.deps.SignInIssuer, account.subject
			if account.issuer != "" {
				decision.ApproverIssuer = account.issuer
			}
		}
		decision.ApproverSessionGeneration = &generation
	}
	if restricted {
		s.relayMachineLoginApproval(relay, w, r, client, model.CanonicalLogin(actor.ID), decision)
		return
	}
	body, err := client.Approve(relay, r.PathValue("id"), decision)
	relayBrokerResponse(w, body, err)
}

// relayDeadline is ctx bounded to the broker client's window from now, the moment an approve's
// re-read of its caller ended, for the settings read and the relay that follow it, so the two
// together reach the broker within that window of the check. A client with no window (0) adds no
// deadline, as it bounds no call.
func relayDeadline(ctx context.Context, client *cipher.Client) (context.Context, context.CancelFunc) {
	if window := client.HTTP.Timeout; window > 0 {
		return context.WithDeadline(ctx, time.Now().Add(window))
	}
	return context.WithCancel(ctx)
}

// writeApproverFieldsUnread answers an approve whose broker settings could not be read
// (cipher.Client.TakesApproverFields failed), which relays nothing: 500
// AGENT_SECRETS_ACCOUNT_TOKEN_UNREADABLE when Dispatch could not read its own ServiceAccount token,
// as relayBrokerResponse answers it, and otherwise 503 AGENT_SECRETS_SETTINGS_UNREAD, logged at
// WARN with the read's error, for the person to approve again.
func writeApproverFieldsUnread(w http.ResponseWriter, err error) {
	var tokenErr *cipher.AccountTokenError
	if errors.As(err, &tokenErr) {
		relayBrokerResponse(w, nil, err)
		return
	}
	slog.Warn("dispatch: the secrets broker's settings could not be read, so whether it takes a machine-login approve's approver fields is unknown and the approve was not relayed", "error", err)
	writeError(w, "AGENT_SECRETS_SETTINGS_UNREAD", http.StatusServiceUnavailable,
		"the secrets broker's settings could not be read, so the approval was not sent to it; approve again in a moment")
}

// relayMachineLoginApproval relays a restricted member's approve of their own machine login while
// holding their approval lock (store.HoldMachineLoginApprovals), in a transaction it keeps open
// until the broker has answered, so a removal that records a revocation of their machine logins
// waits for it and the pass lists the login it mints. It does not wait for the lock: while another
// approve of the person's is at the broker, or a removal of theirs is being recorded, it relays
// nothing and answers 409 MACHINE_LOGIN_APPROVAL_IN_FLIGHT, so a burst of approves holds at most one
// database connection across a broker call. While a revocation is owed it relays nothing either:
// 409 MACHINE_LOGIN_REVOCATION_OWED, for the person to approve again once a pass has revoked their
// earlier logins and settled it, about a minute after a removal commits (machineLoginSettleAfter).
// A read that fails is 503. The relay runs on relay, which bounds it from the caller's check
// (relayDeadline); the transaction runs on the request's own context.
func (s *server) relayMachineLoginApproval(relay context.Context, w http.ResponseWriter, r *http.Request, client *cipher.Client, person string, decision cipher.Decision) {
	ctx := r.Context()
	tx, err := s.deps.Store.Pool.Begin(ctx)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	owed, err := store.HoldMachineLoginApprovals(ctx, tx, person)
	switch {
	case errors.Is(err, store.ErrMachineLoginApprovalInFlight):
		writeError(w, "MACHINE_LOGIN_APPROVAL_IN_FLIGHT", http.StatusConflict,
			"another approval of this person's machine logins is being relayed to the secrets broker, or a removal of theirs from an access list is being recorded; approve again in a few seconds")
		return
	case err != nil:
		writeError(w, "MACHINE_LOGIN_APPROVALS_UNREAD", http.StatusServiceUnavailable,
			"whether a revocation of this person's machine logins is owed could not be read, so the approval was not relayed; try again in a minute")
		return
	case owed:
		writeError(w, "MACHINE_LOGIN_REVOCATION_OWED", http.StatusConflict,
			"this person's machine logins are being revoked after a removal from an access list; approve again once that is done, in about a minute")
		return
	}
	body, err := client.Approve(relay, r.PathValue("id"), decision)
	relayBrokerResponse(w, body, err)
}

// approverStanding reads again whether a decision's caller (an approve's or a deny's), actor as
// actingPerson resolved it, still holds the address, and answers the account its own credential is
// bound to (credentialAccount: the `sub`, "" for none, credentialSubjectSQL, and its issuer as
// people.sub_issuer spells it, "" for Dispatch's sign-in issuer and the outside origin for a
// restricted member's passkey) and the address's session generation (user_sessions.generation, 0
// when it holds none), in one statement, so the generation is the one the check of the caller saw:
//
//   - a cookie: refused once the login's session generation is another; its account is the
//     address's recorded `sub`, the cookie's own while the generation stands;
//   - a personal token: refused once revoked, or once its account no longer holds the address
//     (tokenAccountReplaced: people records another account, or the token records none and was
//     made before people recorded the address's); its account is the one its mint recorded.
//
// A broker-issued token never decides: none holds the approve scope these routes require
// (brokerPersonScopes), so it is refused before it gets here, and refused here too, before any
// read (refuseUnboundCredential), since nothing below has an arm for it.
//
// Each account the statement binds is the address's recorded one, since each that is not is
// refused in that same statement, so its issuer is the one people records with it
// (people.sub_issuer), which the approve names it under.
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
// build without the binding in the fleet the re-read checks no account and answers none, its
// issuer included, so the approve is relayed naming no account, as that build relays it.
//
// The same statement reads whether Dispatch holds the caller, as each credential's own check does,
// and a held caller is then 401 MEMBERSHIP_HELD, so a person held between the request's start and
// its relay approves and denies nothing: for a personal token, Standing's steps on its owner's
// people row, asking Standing for a confirmation older than an hour (personalTokenHeld) once the
// transaction has ended, since Standing reads through the pool itself; for a cookie, the session
// generation a hold advances. Under IAP identity (Standing.ChecksDirectory) a person deciding at
// their browser presents only the assertion IAP signs for the request, which no hold ends, so their
// standing is read from Standing once the transaction has ended (browserDeciderHeld). A revocation
// answers before a hold.
func (s *server) approverStanding(r *http.Request, actor model.Actor) (account credentialAccount, generation int64, err error) {
	if err := refuseUnboundCredential(actor); err != nil {
		return credentialAccount{}, 0, err
	}
	cookieLogin, cookieGeneration := s.mintCookie(r, actor)
	var tokenID *string
	if actor.Act != nil && actor.Act.TokenID != "" {
		tokenID = &actor.Act.TokenID
	}
	var cookieGone, tokenGone bool
	var owner *string
	var hold ownerHold
	unread := func(err error) (credentialAccount, int64, error) {
		slog.Warn("dispatch: a credential decision was refused: whether its caller still holds the address could not be read", "error", err)
		return credentialAccount{}, 0, errDeciderUnreadable()
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
			coalesce((select sub_issuer from people where email = $1::text and $5::boolean), ''),
			coalesce((select generation from user_sessions where login = $1::text), 0),
			owner.login, coalesce(owner.recorded, false), coalesce(owner.stored, false), owner.age
		from (select 1) as one
		left join (
			select canonical_login(agent_tokens.owner) as login, `+ownerHoldColumns+`
			from agent_tokens
			left join people on people.email = canonical_login(agent_tokens.owner)
			where agent_tokens.id = $4::uuid
		) as owner (login, recorded, stored, age) on true
	`, actor.ID, cookieLogin, cookieGeneration, tokenID, held).
		Scan(&cookieGone, &tokenGone, &account.subject, &account.issuer, &generation,
			&owner, &hold.recorded, &hold.stored, &hold.age); err != nil {
		return unread(err)
	}
	// The read is done: end the transaction, and its lock, before Standing, which takes a pooled
	// connection of its own (store.ErrNestedAcquire).
	if err := tx.Rollback(r.Context()); err != nil {
		return unread(err)
	}
	switch {
	case cookieGone:
		return credentialAccount{}, 0, identity.ErrNoIdentity
	case tokenGone:
		return credentialAccount{}, 0, errTokenRevoked()
	}
	if owner != nil {
		if err := s.personalTokenHeld(r.Context(), *owner, hold); err != nil {
			return credentialAccount{}, 0, err
		}
	}
	if err := s.browserDeciderHeld(r.Context(), actor); err != nil {
		return credentialAccount{}, 0, err
	}
	return account, generation, nil
}

// credentialAccount is the account a decision's caller's credential is bound to
// (approverStanding): its `sub`, "" for none, and its issuer as people.sub_issuer spells it.
type credentialAccount struct {
	subject, issuer string
}

// errDeciderUnreadable is a decision, a mint or a scope change whose caller's standing could not
// be read: the statement of approverStanding failed, or under IAP identity Standing failed or
// answered unavailable (browserDeciderHeld). It is never an accept.
func errDeciderUnreadable() error {
	return errorf(http.StatusServiceUnavailable, "IDENTITY_UNAVAILABLE",
		"whether this caller may still decide could not be read; try again in a minute")
}

// browserDeciderHeld answers nil while actor may act at their browser on what outlives it: decide
// a credential request (approverStanding), mint a personal token (createAgentToken) or change a
// token's scopes (updateAgentTokenScopes). It reads anything only under IAP identity
// (Standing.ChecksDirectory) and only for a person at their browser (actor.Act nil). IAP signs a
// fresh assertion for each request and keeps no session that a hold could end, and the Directory,
// not a sign-in, records and ends a hold, so the request asks Standing, as a personal token's use
// does there (personalTokenHeld): a held answer is 401 MEMBERSHIP_HELD in the Directory's words,
// and one it cannot give, or an unavailable one (identity.Standing.Unavailable: no Active Directory
// answer in 24 hours), 503 IDENTITY_UNAVAILABLE. IAP itself still admits the person's browser;
// only the request is refused. A credential's caller is held by its own check (personalTokenHeld,
// brokerPersonAdmitted), and outside IAP a hold ends a cookie's session, so for either it reads
// nothing. Each caller asks once the request's body has been read, which a browser may hold back
// as long as it likes, and outside any transaction, since Standing takes a pooled connection of
// its own (store.ErrNestedAcquire).
func (s *server) browserDeciderHeld(ctx context.Context, actor model.Actor) error {
	if actor.Act != nil || s.deps.Standing == nil || !s.deps.Standing.ChecksDirectory() {
		return nil
	}
	standing, err := s.deps.Standing.Standing(ctx, actor.ID)
	if err != nil {
		// The person by their login's hash and the failure by its kind: the store's error quotes
		// the address it read (model.LoginHash, errkind.Of).
		slog.Warn("dispatch: a decision, a mint or a scope change at a browser was refused: whether Dispatch holds its caller could not be read",
			"login_hash", model.LoginHash(actor.ID), "error_kind", errkind.Of(err))
		return errDeciderUnreadable()
	}
	switch {
	case standing.Unavailable():
		return errDeciderUnreadable()
	case standing.Held:
		return errMembershipHeld(membershipHeldDeciderDirectoryText)
	}
	return nil
}

// restrictedDecisionAllowed answers whether an approve or a deny may go on to the broker: at once
// for a caller who is no restricted member, and for one only when the code they typed names a
// pending machine login of their own, the record the path names (ownMachineLogin). It runs before
// approverStanding, so the caller's standing is still read last, right before the broker is asked
// to decide. It answers false once it has written the refusal.
func (s *server) restrictedDecisionAllowed(w http.ResponseWriter, r *http.Request, client *cipher.Client, actor model.Actor, code *string) bool {
	restricted, ok := s.restrictedCaller(w, r)
	if !ok || !restricted {
		return ok
	}
	if code == nil {
		writeError(w, codeRestrictedMember, http.StatusForbidden,
			"a restricted member decides only a pending machine login of their own, by the code their machine shows, and this decision carries none")
		return false
	}
	lookup, err := json.Marshal(wire.MachineLookupRequest{Code: *code})
	if err != nil {
		s.writeHandlerError(w, err)
		return false
	}
	_, ok = ownMachineLogin(w, r, client, model.CanonicalLogin(actor.ID), lookup, r.PathValue("id"))
	return ok
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
	if !s.restrictedDecisionAllowed(w, r, client, actor, decision.Code) {
		return
	}
	if _, _, err := s.approverStanding(r, actor); err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	body, err := client.Deny(r.Context(), r.PathValue("id"), decision)
	relayBrokerResponse(w, body, err)
}

// lookupMachineCredential relays the lookup of a machine login by the code the person typed. A
// restricted member is answered only a pending machine login of their own (ownMachineLogin).
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
	restricted, ok := s.restrictedCaller(w, r)
	if !ok {
		return
	}
	if restricted {
		if own, ok := ownMachineLogin(w, r, client, model.CanonicalLogin(actor.ID), relayBody, ""); ok {
			relayBrokerResponse(w, own, nil)
		}
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

// listCredentialGrants answers the live grants of the caller's Live grants page, which the broker
// lists for the caller's login: those the caller approved, those of the sessions that act for the
// caller, those the model judge approved in anyone's stead and those a pod that acts for no person
// took without asking. A restricted member is answered the grants of the sessions that act for them
// alone (memberGrants): Dispatch decides it, and drops every other row the broker lists whatever
// its approver, so the list holds no grant of a project the member is not on whether or not the
// broker itself knows what kind of member they are. Every other caller is answered the broker's
// list verbatim.
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
	restricted, ok := s.restrictedCaller(w, r)
	if !ok {
		return
	}
	if !restricted {
		body, err := client.Grants(r.Context(), login)
		relayBrokerResponse(w, body, err)
		return
	}
	own, err := memberGrants(r.Context(), client, login)
	if err != nil {
		relayBrokerResponse(w, nil, err)
		return
	}
	WriteJSON(w, http.StatusOK, wire.ApproverGrantsResponse{Grants: own})
}

// revokeCredentialGrant ends a grant as the caller. For every caller but a restricted member the
// broker decides: it allows the revoke only when the caller's login is the grant's approver or the
// person its enrollment acts for (an unmapped person's pod acts for no one), or, for a grant the
// model judge approved in anyone's stead, anyone who may see it. Dispatch decides for a restricted
// member: it relays their revoke only of a grant of memberGrants, one whose session acts for them,
// and answers any other id, an id the broker lists no grant for among them, 403 RESTRICTED_MEMBER
// with the one sentence that names nothing of the grant, sending the broker no revoke. While the
// broker's list cannot be read, a restricted member's revoke fails closed: it is answered as that
// read failed (relayBrokerResponse: the broker's own refusal, or 503 AGENT_SECRETS_UNAVAILABLE),
// whatever the id, and revokes nothing. The browser's body carries nothing Dispatch reads.
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
	restricted, ok := s.restrictedCaller(w, r)
	if !ok {
		return
	}
	login, id := model.CanonicalLogin(actor.ID), r.PathValue("id")
	if restricted {
		own, err := memberGrants(r.Context(), client, login)
		if err != nil {
			relayBrokerResponse(w, nil, err)
			return
		}
		if !slices.ContainsFunc(own, func(grant wire.ApproverGrantResp) bool { return grant.GrantID == id }) {
			writeError(w, codeRestrictedMember, http.StatusForbidden,
				"a restricted member revokes only a grant of a session that acts for them")
			return
		}
	}
	body, err := client.RevokeByApprover(r.Context(), id, login)
	relayBrokerResponse(w, body, err)
}

// memberGrants is the grants the broker lists for person (GET /v1/grants) whose session acts for
// person: those whose enrollment's operator is person. On a grant list the broker writes that
// field as the person the enrollment acts for at the read (wire.RecordEnrollmentResp's Operator,
// requests' operatorOf), and it is the field the broker's own rule for a restricted member reads
// (GrantsForApprover's notOwn), so the two agree on which grants are the member's. A grant person
// approved on another person's session, one the model judge or a person approved in anyone's
// stead on another session, a group's, and an automatic grant of a pod that acts for no person are
// none of them, whatever their approver. An empty person has no grant, so a row whose session acts
// for no person is never anyone's here.
func memberGrants(ctx context.Context, client *cipher.Client, person string) ([]wire.ApproverGrantResp, error) {
	listed, err := client.Grants(ctx, person)
	if err != nil {
		return nil, err
	}
	var grants wire.ApproverGrantsResponse
	if err := json.Unmarshal(listed, &grants); err != nil {
		return nil, fmt.Errorf("read %s's grants from the secrets broker: %w", person, err)
	}
	own := []wire.ApproverGrantResp{}
	for _, grant := range grants.Grants {
		if person != "" && model.CanonicalLogin(grant.Enrollment.Operator) == person {
			own = append(own, grant)
		}
	}
	return own, nil
}

// --- GET /api/v1/machine-logins, POST .../{id}/revoke ---

// listMachineLogins answers the machine logins the caller may revoke that can still reach a
// secret: every service's login, whoever approved it, the caller's own machines', and the logins
// of the caller's own pods (pods whose Kubernetes account the broker's BROKER_POD_OPERATORS maps
// to the caller, each row's person naming them), an expired one whose sessions still run among
// them. Dispatch names the caller, so no one lists another person's machines or pods. A restricted
// member, at their browser alone (restrictedOwnLoginRoutes), is answered their own machines' and
// their pods' logins and no service's (memberMachineLogins).
func (s *server) listMachineLogins(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	client, ok := s.requireAgentSecrets(w)
	if !ok {
		return
	}
	restricted, ok := s.restrictedCaller(w, r)
	if !ok {
		return
	}
	login := model.CanonicalLogin(actor.ID)
	if !restricted {
		body, err := client.MachineLogins(r.Context(), login)
		relayBrokerResponse(w, body, err)
		return
	}
	own, err := memberMachineLogins(r.Context(), client, login)
	if err != nil {
		relayBrokerResponse(w, nil, err)
		return
	}
	WriteJSON(w, http.StatusOK, wire.LauncherCredentialsResponse{Credentials: own})
}

// revokeMachineLogin ends a machine login, expired or not, and with it every session that login
// enrolled (a service's login: every pod it started): the broker allows it for a service's login
// from anyone signed in, for a person's machine login only when the caller is the person who
// approved it, and for a person's pod login only when the caller is the person the broker's
// BROKER_POD_OPERATORS maps the pod's account to. The browser's body carries nothing Dispatch
// reads. A restricted member, at their browser alone (restrictedOwnLoginRoutes), revokes only a
// login of memberMachineLogins, their own machines' and their pods'; any other id, a service's
// login's among them, is 403 RESTRICTED_MEMBER and no revoke reaches the broker, so the broker's
// own 404 for a restricted member's revoke of a service's login is no answer this route gives.
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
	restricted, ok := s.restrictedCaller(w, r)
	if !ok {
		return
	}
	login, id := model.CanonicalLogin(actor.ID), r.PathValue("id")
	if restricted {
		own, err := memberMachineLogins(r.Context(), client, login)
		if err != nil {
			relayBrokerResponse(w, nil, err)
			return
		}
		if !slices.ContainsFunc(own, func(credential wire.LauncherCredentialResp) bool { return credential.CredentialID == id }) {
			writeError(w, codeRestrictedMember, http.StatusForbidden,
				"a restricted member revokes only a machine login of their own, which names them as its approver and no service, or the login of a pod the secrets broker maps to them")
			return
		}
	}
	body, err := client.RevokeMachineLogin(r.Context(), id, login)
	relayBrokerResponse(w, body, err)
}

// ownMachineLogins is the machine logins the broker lists for person that are person's own: a
// login whose approver is person and which names no service. The revocation a removal owes ends
// only these (revokeMachineLoginsOf): a pod login it ended would log in again with its token, so a
// removal leaves the logins of the person's pods to the broker. A restricted member lists and
// revokes memberMachineLogins, which holds these and their pods' logins.
func ownMachineLogins(ctx context.Context, client *cipher.Client, person string) ([]wire.LauncherCredentialResp, error) {
	return machineLoginsOf(ctx, client, person, func(login wire.LauncherCredentialResp) bool {
		return namesNoService(login.Service) && model.CanonicalLogin(login.ApprovedBy) == person
	})
}

// memberMachineLogins is the machine logins a restricted member lists and revokes: the broker's
// own set for such a member (enroll.Service.OwnLiveCredentials, which the broker's machine-login
// routes choose for them in machineLoginsOwnOnly), the logins of their own machines
// (ownMachineLogins' rule) and the logins of the pods whose Kubernetes account the broker's
// BROKER_POD_OPERATORS maps to them, whose person field names them. A service's login is in
// neither, whoever approved it, so it is no restricted member's to list or revoke.
func memberMachineLogins(ctx context.Context, client *cipher.Client, person string) ([]wire.LauncherCredentialResp, error) {
	return machineLoginsOf(ctx, client, person, func(login wire.LauncherCredentialResp) bool {
		return namesNoService(login.Service) && model.CanonicalLogin(login.ApprovedBy) == person ||
			login.Person != nil && model.CanonicalLogin(*login.Person) == person
	})
}

// machineLoginsOf is the machine logins the broker lists for person that keep holds of.
func machineLoginsOf(ctx context.Context, client *cipher.Client, person string, keep func(wire.LauncherCredentialResp) bool) ([]wire.LauncherCredentialResp, error) {
	listed, err := client.MachineLogins(ctx, person)
	if err != nil {
		return nil, err
	}
	var logins wire.LauncherCredentialsResponse
	if err := json.Unmarshal(listed, &logins); err != nil {
		return nil, fmt.Errorf("read %s's machine logins from the secrets broker: %w", person, err)
	}
	kept := []wire.LauncherCredentialResp{}
	for _, login := range logins.Credentials {
		if keep(login) {
			kept = append(kept, login)
		}
	}
	return kept, nil
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
