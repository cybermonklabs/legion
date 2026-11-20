package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/answer"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// createRequestBody is POST /v1/requests's exact shape: a signed
// request object plus two optional,
// unsigned fields, session_id (wake-only) and github_app (the narrowing the release will ask
// for). The v8 "secrets"/"reason"/"issue" top-level fields are gone —
// they now live inside the signed request object itself.
type createRequestBody struct {
	// The session's signed request: a compact ES256 JWS, signed with the session's key and
	// addressed to the broker's public URL, naming the secrets (authorization_details) and the
	// reason.
	Request string `json:"request"`
	// Optional: the Envoy session the broker notifies if this request expires undecided.
	SessionID *string `json:"session_id"`
	// Optional: the narrowing this request's grant will be released with, as POST
	// /v1/grants/{id}/values takes it. It is unsigned, as session_id is. It narrows nothing by
	// itself: the release is what applies a narrowing, and a release that asks for none is
	// owner-wide whatever this field said. The broker checks it before it asks a person to
	// approve the request, so a narrowing it cannot serve is refused here and costs no
	// approval. An object that does not read as a narrowing, with a field it does not take or a
	// field of the wrong type, is refused 400 INVALID_NARROWING naming that field, as the release
	// refuses it; 400 INVALID_REQUEST is kept for the body's own fields.
	GitHubApp *githubNarrowing `json:"github_app"`
}

// createRequestResponse is POST /v1/requests's exact wire shape: request_id, state, secrets,
// grant_id, record_id, approver, lifetime_seconds, coalesced, detail, approve_url — nothing else.
// requests.Request itself carries additional fields (decided_at, decided_by) this route's contract
// does not define, so the handler below builds this dedicated response rather than marshaling the
// Request it gets back from Machine.Create directly.
type createRequestResponse struct {
	// The request's id, which GET /v1/requests/{id} and its cancel route take.
	RequestID string `json:"request_id"`
	// "granted", "pending" (a person must decide it) or "denied".
	State string `json:"state"`
	// How the policy decided each name asked for.
	Secrets []requests.SecretDecision `json:"secrets"`
	// Once granted, the grant to read the values from; null otherwise.
	GrantID *string `json:"grant_id"`
	// The credential-request record the approver decides; null when nobody needs to.
	RecordID *string `json:"record_id"`
	// Who decides it, as its record names them: a person's login (their lowercase email), or
	// "anyone" for a shared secret, which anyone signed in to the broker's approval page may
	// decide. While it is pending it is listed in that person's Dispatch Inbox, linking
	// approve_url; for "anyone", in the Inbox of the person responsible for the agent that asked
	// (the session's operator, a fleet machine's person, or a Legion pod's issue assignee), or in
	// everyone's when no such person is found. Null when the policy decided it at once, so no
	// record names one.
	Approver *string `json:"approver"`
	// How many seconds its grant lasts, as the signed request asked; or, when it asked for none, 12
	// hours; null when every name is tagged default-grant=session and the grant lasts until the
	// session ends. A reused or coalesced request answers
	// its own, which equals the one asked for. A client that asked for a lifetime and reads null,
	// or no such field from a broker before it, was not given that lifetime.
	LifetimeSeconds *int `json:"lifetime_seconds"`
	// True when the request joined an identical one this session already had pending.
	Coalesced bool `json:"coalesced,omitempty"`
	// The broker's one sentence about why it denied the request: the secret that refused it and
	// the rule that did, as "GCLOUD_DEMO: requester-not-listed", several joined with "; ". Null
	// when the broker recorded none — every request it did not deny, and a denial whose rule it
	// cannot name. A session reads it to learn that asking for that secret again will not help.
	Detail *string `json:"detail"`
	// While it is pending, its record's page on the broker, <BROKER_PUBLIC_URL>/approve/<record>,
	// where a person signed in to the broker decides it; absent when the policy decided it at once,
	// on a broker with BROKER_SIGNIN=off, which serves no page, and while the model judge holds
	// it, which judge_deciding says instead.
	ApproveURL string `json:"approve_url,omitempty"`
	// True while the model judge holds the request: its keys are ones the judge decides, and no
	// person is asked unless the judge cannot decide it, when the request comes back with its
	// approve_url. Absent for every other request.
	JudgeDeciding bool `json:"judge_deciding,omitempty"`
}

// createRequest requests secrets for the proof-verified caller, never for an enrollment named in
// the body: a session may only ever request secrets for itself. The request object itself (not
// this handler) names the secrets, the reason, and — implicitly, via the policy — the approver.
func (s *server) createRequest(w http.ResponseWriter, r *http.Request, enrollmentID string) {
	var body createRequestBody
	if !readJSON(w, r, &body, "INVALID_REQUEST") {
		return
	}
	// The narrowing the release will ask for, read exactly as the release's own route reads it
	// (grantValues), so one narrowing is refused by one parser and the caller reads one answer
	// for one mistake.
	narrowing, ok := parsedNarrowing(w, body.GitHubApp)
	if !ok {
		return
	}
	req, err := s.deps.Machine.Create(r.Context(), enrollmentID, body.Request, derefOr(body.SessionID, ""), narrowing)
	switch {
	case errors.Is(err, record.ErrRequestInvalid):
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", err.Error())
		return
	case errors.Is(err, requests.ErrMixedApprovers):
		writeError(w, http.StatusBadRequest, "MIXED_APPROVERS", err.Error())
		return
	case errors.Is(err, requests.ErrGrantRequiresLifetime):
		// A secret's max-grant tag bounds the lifetime its request may sign.
		writeError(w, http.StatusBadRequest, "GRANT_REQUIRES_LIFETIME", err.Error())
		return
	case errors.Is(err, requests.ErrRequestPending):
		writeError(w, http.StatusConflict, "REQUEST_PENDING", err.Error())
		return
	case errors.Is(err, requests.ErrSessionIDInvalid):
		writeError(w, http.StatusBadRequest, "SESSION_ID_INPUT", err.Error())
		return
	case errors.Is(err, policy.ErrUnknownSecret):
		writeError(w, http.StatusBadRequest, "UNKNOWN_SECRET", err.Error())
		return
	case errors.Is(err, policy.ErrPendingValue):
		// A secret `agent-secrets secret create` made and has not yet filled: not final, unlike
		// UNKNOWN_SECRET, so the caller may ask again once the owner stores the value.
		writeError(w, http.StatusBadRequest, "PENDING_VALUE", err.Error())
		return
	case writeNarrowingRefusal(w, err):
		// Create refused the narrowing the request carries, before anybody was asked to approve
		// it. The release route answers those refusals through the same helper.
		return
	case errors.Is(err, secrets.ErrMintNoInstallation):
		// Create's check found no installation of the App on the account the narrowing names,
		// before anybody was asked to approve the request. The caller can still name an account
		// the App is installed on, so this route answers it as a refused narrowing. The release
		// route does not: it wraps the same refusal in requests.ErrMintFailed and answers it 502
		// MINT_FAILED, as it answers every refused mint. So this arm is the request route's own,
		// outside writeNarrowingRefusal, which holds only the refusals both routes answer alike.
		writeError(w, http.StatusUnprocessableEntity, "NARROWING_REFUSED", err.Error())
		return
	case errors.Is(err, pgx.ErrNoRows):
		// The proof was verified moments ago against a live enrollment, but it was revoked (or
		// its lease lapsed) before Create read it or locked it to write — the same race
		// enroll.Renew guards against. The caller's proof is no longer good for anything.
		writeError(w, http.StatusUnauthorized, "PROOF_INVALID", "enrollment is not live")
		return
	case writeMemberKindUncheckable(w, "the secret request", err):
		// The broker could not learn whether Dispatch calls the session's person a restricted
		// member, so it decides nothing and writes no request: 503, and the session asks again.
		return
	case err != nil:
		writeInternal(w, "create request", err)
		return
	}
	writeJSON(w, http.StatusOK, createRequestResponse{
		RequestID:       req.ID,
		State:           req.State,
		Secrets:         req.Secrets,
		GrantID:         req.GrantID,
		RecordID:        req.RecordID,
		Approver:        req.Approver,
		LifetimeSeconds: req.LifetimeSeconds,
		Coalesced:       req.Coalesced,
		Detail:          req.Detail,
		ApproveURL:      s.pendingApproveURL(req),
		JudgeDeciding:   req.JudgeDeciding,
	})
}

// requestDecision is GET /v1/requests/{id}'s nested "decision" object: who decided the request
// and when.
type requestDecision struct {
	// Who decided it: the approver's login, "session:<enrollment id>" (the session cancelled it),
	// "launcher:<credential id>" (its launcher ended the session, or its operator ended its machine
	// login from one of their machines) or "broker" (the session's lease lapsed).
	By string `json:"by"`
	// When it was decided.
	At time.Time `json:"at"`
}

// requestStatusResponse is GET /v1/requests/{id}'s answer: state, grant_id, record_id, approver,
// lifetime_seconds, decided_at, decision, detail and, while it is pending, approve_url — nothing
// else.
type requestStatusResponse struct {
	// "pending", "granted", "denied", "expired" or "cancelled".
	State string `json:"state"`
	// Once granted, the grant to read the values from; null otherwise.
	GrantID *string `json:"grant_id"`
	// The credential-request record the approver decides; null when nobody needs to.
	RecordID *string `json:"record_id"`
	// Who decides it, as its record names them: a person's login (their lowercase email), or
	// "anyone" for a shared secret, which anyone signed in to the broker's approval page may
	// decide. The record fixes it when the request is made, so a later change to the secret's
	// owner does not move it, and it stays once the request is decided. Null when the policy
	// decided it at once, so no record names one.
	Approver *string `json:"approver"`
	// How many seconds its grant lasts: as the signed request asked, else 15 minutes when any name
	// is human tier and 12 hours otherwise; null for a
	// grant that lasts until the session ends (every name tagged default-grant=session).
	LifetimeSeconds *int `json:"lifetime_seconds"`
	// When it left "pending"; null while pending.
	DecidedAt *time.Time `json:"decided_at"`
	// Who decided it and when; null while pending, when the policy decided it at once, and when it
	// expired.
	Decision *requestDecision `json:"decision"`
	// The broker's one sentence about why it denied the request, as POST /v1/requests answers it:
	// the secret that refused it and the rule that did, as "GCLOUD_DEMO: requester-not-listed".
	// Null when the broker recorded none, a request it did not deny among them.
	Detail *string `json:"detail"`
	// While it is pending, its record's page on the broker, <BROKER_PUBLIC_URL>/approve/<record>,
	// where a person signed in to the broker decides it; absent once it is no longer pending, when
	// the policy decided it at once, on a broker with BROKER_SIGNIN=off, which serves no page, and
	// while the model judge holds it.
	ApproveURL string `json:"approve_url,omitempty"`
	// True while the model judge holds the request, as POST /v1/requests answers it; absent for
	// every other request.
	JudgeDeciding bool `json:"judge_deciding,omitempty"`
}

// grantValuesBody is POST /v1/grants/{id}/values's optional body: no body at all, or one with no
// github_app, releases every value as the grant holds it.
type grantValuesBody struct {
	// Optional: narrows the token of every github-app secret the grant holds to one account's
	// installation and, within it, to named repositories, to read access, or to both. A grant
	// that holds no github-app secret refuses it (NARROWING_NOT_APPLICABLE), one whose
	// github-app secret does not list the account refuses it (NARROWING_OWNER_NOT_LISTED), and
	// one holding a name behind the airlock refuses it (NARROWING_BEHIND_AIRLOCK), since the
	// proxy spends that grant's values and mints them owner-wide. It
	// narrows this release alone: the grant's next release without it is owner-wide.
	GitHubApp *githubNarrowing `json:"github_app"`
	// Optional: the command this release starts, as its argument vector. The broker logs it with
	// the grant's reason: its first 32 arguments, each cut to 256 bytes, with `[redacted]` in place
	// of each value of 8 bytes or more that the release hands out, each string of 8 bytes or more
	// in a stored JSON value, and each credential of a well-known shape. Any other secret in it is
	// logged as sent. It decides nothing about the release. A broker from before this field
	// refuses a body that holds it, as INVALID_NARROWING naming the unknown field, and releases
	// nothing.
	Command []string `json:"command"`
	// Optional: the reason this call of the session gave, which can differ from the reason of the
	// request the grant came of when the call got a live grant back. The broker logs it beside the
	// grant's reason where the two differ, held to a request's rules: the runes a request's
	// reason may not carry dropped and the rest cut to 400 runes. It decides nothing about the
	// release. A broker from before this field refuses a body that holds it, as
	// INVALID_NARROWING naming the unknown field, and releases nothing.
	Reason string `json:"reason"`
}

// githubNarrowing is one release's narrowing of its GitHub App tokens: as the request names it,
// and as the answer names what the broker applied.
type githubNarrowing struct {
	// The one GitHub account whose installation the token is minted for: a login the secret's
	// github-owners lists, compared without regard to case. The answer names it lowercased.
	Owner string `json:"owner"`
	// Optional: names of the account's repositories, without the account, at least one and at most
	// 100: the token reaches those repositories alone, but keeps every permission the App holds, its
	// organization permissions among them, unless read_only is set too. The answer names them
	// sorted, each once.
	Repositories []string `json:"repositories,omitempty"`
	// Optional: true limits the token to contents, metadata, pull_requests, actions, checks and
	// statuses, each at read, less each of the last three the installation does not hold. A
	// machine that must only read needs it, with or without repositories.
	ReadOnly bool `json:"read_only,omitempty"`
	// unread is why the github_app object a body sent does not read as a narrowing, "" when it
	// does. UnmarshalJSON sets it in place of returning an error, so parsedNarrowing answers it
	// with the narrowing's own code rather than readJSON with the body's (UnmarshalJSON).
	unread string `json:"-"`
}

// UnmarshalJSON reads a github_app object strictly, as a narrowing: a field a narrowing does not
// take and a field of the wrong JSON type are the narrowing's mistakes, which parsedNarrowing
// refuses 400 INVALID_NARROWING naming github_app.<field>, on POST /v1/requests as on the
// release's route. Returned as a decode error, readJSON would answer them with the body's own
// code, and on POST /v1/requests that is INVALID_REQUEST: the code agent-secrets reads as a
// broker from before github_app, which it answers by sending the same request again without the
// narrowing (brokerTookNoNarrowing), so a narrowing this broker read and refused would go on to
// the person the check before the approval exists to spare. A JSON null never reaches it: the
// field's pointer stays nil, a body that names no narrowing.
func (n *githubNarrowing) UnmarshalJSON(data []byte) error {
	type plain githubNarrowing // the same fields, without this method
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var read plain
	if err := decoder.Decode(&read); err != nil {
		*n = githubNarrowing{unread: unreadNarrowing(err)}
		return nil
	}
	*n = githubNarrowing(read)
	return nil
}

// unreadNarrowing is the refusal of a github_app object that does not read as a narrowing,
// naming the field at fault as github_app.<field>, the way policy.ParseGitHubNarrowing's
// refusals name theirs.
func unreadNarrowing(err error) string {
	var wrongType *json.UnmarshalTypeError
	if errors.As(err, &wrongType) {
		field := "github_app"
		if wrongType.Field != "" {
			field += "." + wrongType.Field
		}
		return field + " must be " + jsonValueOf(wrongType.Type) + ", not " + jsonKindOf(wrongType.Value)
	}
	if name, unknown := strings.CutPrefix(err.Error(), "json: unknown field "); unknown {
		if unquoted, unquoteErr := strconv.Unquote(name); unquoteErr == nil {
			name = unquoted
		}
		return "github_app." + name + " is not a field of a narrowing"
	}
	return "github_app does not read as a narrowing: " + err.Error()
}

// jsonValueOf names the JSON a narrowing's field of Go type t takes.
func jsonValueOf(t reflect.Type) string {
	switch t.Kind() {
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "true or false"
	case reflect.Slice:
		return "an array of " + strings.TrimPrefix(jsonValueOf(t.Elem()), "a ") + "s"
	default:
		return "an object"
	}
}

// jsonKindOf names the JSON a caller sent, from encoding/json's word for it ("string",
// "number", "bool", "array", "object", a number sometimes followed by its literal).
func jsonKindOf(value string) string {
	kind, _, _ := strings.Cut(value, " ")
	switch kind {
	case "array", "object":
		return "an " + kind
	case "bool":
		return "a boolean"
	default:
		return "a " + kind
	}
}

// parsedNarrowing is the github_app field of a body as the narrowing the broker works with. Both
// routes that take one read it through here — the request's own (createRequest) and the
// release's (grantValues) — so one narrowing is refused by one parser and a caller reads one
// answer for one mistake. A body naming no narrowing answers nil and true. An object that did not
// read as a narrowing (githubNarrowing.UnmarshalJSON), and a narrowing the parser refuses, answer
// false, with the 400 INVALID_NARROWING already written, naming the field it refused: its caller
// returns and writes nothing more.
func parsedNarrowing(w http.ResponseWriter, sent *githubNarrowing) (*secrets.GitHubNarrowing, bool) {
	if sent == nil {
		return nil, true
	}
	if sent.unread != "" {
		writeError(w, http.StatusBadRequest, "INVALID_NARROWING", sent.unread)
		return nil, false
	}
	narrowing, refusal := policy.ParseGitHubNarrowing(sent.Owner, sent.Repositories, sent.ReadOnly)
	if refusal != nil {
		writeError(w, http.StatusBadRequest, "INVALID_NARROWING", "github_app."+refusal.Field+" "+refusal.Reason)
		return nil, false
	}
	return &narrowing, true
}

// grantValuesResponse is POST /v1/grants/{id}/values's answer.
type grantValuesResponse struct {
	// When the grant expires; the session can read the values again until then. Null for a grant
	// that lasts until the session ends.
	ExpiresAt *time.Time `json:"expires_at"`
	// Each granted name's value, read fresh from the secret store.
	Values map[string]string `json:"values"`
	// The narrowing this release applied to its GitHub App tokens, absent when it applied none. A
	// client that asked for one and finds none here is answered by a broker from before
	// narrowing, which released the owner-wide tokens.
	GitHubApp *githubNarrowing `json:"github_app,omitempty"`
	// How to reach the values this release did not hand over: the names the airlock holds back
	// are not in "values" at all, and this says where the proxy that holds them is. Absent when
	// no granted name is behind the airlock, which is every release of a broker with no rules
	// file.
	Airlock *airlockRelease `json:"airlock,omitempty"`
}

// airlockRelease is how a session reaches the values the airlock holds back: the proxy it sends
// their requests through, the certificate authority that proxy's certificates are signed by, the
// password every connection to it opens with, and one entry per name held back.
type airlockRelease struct {
	// The URL the command points HTTPS_PROXY at (BROKER_AIRLOCK_PROXY_URL).
	ProxyURL string `json:"proxy_url"`
	// The PEM certificate the command must trust to reach the proxy, which signs a certificate
	// for each host it carries a request to.
	CACertificate string `json:"ca_certificate"`
	// The password every proxy connection authenticates with: one of this grant's own
	// placeholders, the first (name, slot) pair in sorted order.
	ProxyPassword string `json:"proxy_password"`
	// Each name the airlock holds back, by the name the command knows it as.
	Names map[string]airlockName `json:"names"`
}

// airlockName is one name the airlock holds back.
type airlockName struct {
	// What the command's environment variable of that name holds: the released shape of the
	// secret with a placeholder wherever its value would be.
	Value string `json:"value"`
	// Each slot of the secret and the placeholder standing for it, so a command that takes one
	// value of the secret apart knows which placeholder is which.
	Slots map[string]string `json:"slots"`
	// The hosts the secret's airlock entry allows, which the command keeps off its NO_PROXY so
	// their requests reach the proxy.
	Hosts []string `json:"hosts"`
}

// requestWaitStep is how often a held read reads its request again.
const requestWaitStep = 500 * time.Millisecond

// readRequest answers one request to any enrollment of the principal that made it: the same
// person, the same Legion claim, the same machine (requests.Machine.OwnerAndState), and a request
// of an enrollment that records no owner to that enrollment alone. Everyone else is 403 NOT_YOURS.
// With ?wait=N, N a whole number of seconds from 1 to record.MaxRequestWaitSeconds, it holds the
// read while the request is pending (readHeld) and answers the first state that is not pending, or
// pending once N seconds have passed since the request reached the broker
// (answer.Recorder.Arrived), so a slow proof check shortens the hold rather than lengthening it.
// The state it reads is the database's, so a decision made on either broker process — the
// approval page's, the model judge's, the sweeper's expiry, the session's own cancel — is seen
// within one step of its commit. Any other wait is refused REQUEST_WAIT_INPUT before any read; an
// absent or empty one answers at once, as before the parameter.
//
// A store that fails is 503 DATABASE_UNAVAILABLE, logged at ERROR and counted failed, which
// agent-secrets reads as a failed read and waits through (retryable), where a 500 would end its
// wait with nothing run while the request stays pending. A held read whose caller went away before
// the broker owed its answer, before its wait ran out or in the one step (half a second) after it,
// is 503 REQUEST_ENDED, logs nothing, as the reread's does, and is counted cancelled
// (answer.Recorder.NoteCallerEnded). Any other read that ends early is a store failure: a read
// with no wait owes its answer at once, and the pool sets no statement timeout, so a store that
// stalls holds a read until its caller gives up or the request deadline ends it. That stall is
// logged and counted failed, as it is when it holds the proof check. The wait counts from the
// request's arrival, so a caller that goes away while its proof is still being checked leaves
// inside its wait. One that leaves while the broker looks its enrollment up or records the
// proof's jti does not reach this handler: the proof check ends instead, as `broker: verify proof
// failed`, a failed 503 (sessionCaller). One that leaves during Replay's prune of expired jtis,
// whose error the check ignores, reaches it, and its first read fails inside the window, so it is
// REQUEST_ENDED and counted cancelled, by the rule above.
func (s *server) readRequest(w http.ResponseWriter, r *http.Request, enrollmentID string) {
	ctx := r.Context()
	id, ok := pathUUID(w, r, "id", "REQUEST_ID_INPUT", "request")
	if !ok {
		return
	}
	wait, ok := requestWait(w, r.URL.Query().Get("wait"))
	if !ok {
		return
	}
	recorder := answer.FromContext(ctx)
	deadline := recorder.Arrived().Add(wait)
	req, err := s.readHeld(ctx, id, enrollmentID, deadline)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no such request")
		return
	case errors.Is(err, requests.ErrNotYours):
		writeError(w, http.StatusForbidden, "NOT_YOURS", err.Error())
		return
	case err != nil && wait > 0 && errors.Is(ctx.Err(), context.Canceled) && time.Now().Before(deadline.Add(requestWaitStep)):
		recorder.NoteCallerEnded()
		writeError(w, http.StatusServiceUnavailable, "REQUEST_ENDED", "the request ended before the read finished; send it again")
		return
	case err != nil:
		writeUnavailable(w, "DATABASE_UNAVAILABLE", "read request", err)
		return
	}
	resp := requestStatusResponse{
		State: req.State, GrantID: req.GrantID, RecordID: req.RecordID, Approver: req.Approver, LifetimeSeconds: req.LifetimeSeconds,
		DecidedAt: req.DecidedAt, Detail: req.Detail, ApproveURL: s.pendingApproveURL(req), JudgeDeciding: req.JudgeDeciding,
	}
	if req.DecidedBy != nil && req.DecidedAt != nil {
		resp.Decision = &requestDecision{By: *req.DecidedBy, At: *req.DecidedAt}
	}
	writeJSON(w, http.StatusOK, resp)
}

// requestWait reads GET /v1/requests/{id}'s ?wait: 0 for none, else the wait it names, refusing
// anything but a whole number of seconds from 1 to record.MaxRequestWaitSeconds.
func requestWait(w http.ResponseWriter, raw string) (time.Duration, bool) {
	if raw == "" {
		return 0, true
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds < 1 || seconds > record.MaxRequestWaitSeconds {
		writeError(w, http.StatusBadRequest, "REQUEST_WAIT_INPUT", fmt.Sprintf("wait is a whole number of seconds from 1 to %d", record.MaxRequestWaitSeconds))
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}

// readHeld reads request id for enrollmentID. It reads the caller's owner once
// (Machine.EnrollmentOwner), then the request's owner, state and whether the model judge holds it
// (Machine.OwnerAndState, one statement), refusing requests.ErrNotOwner when the request's owner is
// not the caller's; while the state is pending and deadline has not passed, it reads them again
// every requestWaitStep; then it reads the whole request (Machine.Get) once. A request the judge
// held at the first read and holds no more has been handed to its person, still pending: that ends
// the hold too, so the caller learns at once who decides it now and where. A deadline already
// passed is the one read. The owner is checked at every step, so a request that moves to an
// enrollment of another owner while the read is held is refused at the next step, as a new read of
// it is. Any other error is the store's, or ctx's when ctx ended first.
func (s *server) readHeld(ctx context.Context, id, enrollmentID string, deadline time.Time) (requests.Request, error) {
	caller, err := s.deps.Machine.EnrollmentOwner(ctx, enrollmentID)
	if err != nil {
		return requests.Request{}, fmt.Errorf("read the caller's owner: %v", err)
	}
	first, heldByJudge := true, false
	for {
		owner, state, judging, err := s.deps.Machine.OwnerAndState(ctx, id)
		if err != nil {
			return requests.Request{}, err
		}
		if owner != caller {
			return requests.Request{}, requests.ErrNotOwner
		}
		if first {
			first, heldByJudge = false, judging
		}
		left := time.Until(deadline)
		if state != "pending" || left <= 0 || (heldByJudge && !judging) {
			return s.deps.Machine.Get(ctx, id)
		}
		select {
		case <-ctx.Done():
			return requests.Request{}, ctx.Err()
		case <-time.After(min(requestWaitStep, left)):
		}
	}
}

func (s *server) cancelRequest(w http.ResponseWriter, r *http.Request, enrollmentID string) {
	id, ok := pathUUID(w, r, "id", "REQUEST_ID_INPUT", "request")
	if !ok {
		return
	}
	err := s.deps.Machine.Cancel(r.Context(), id, enrollmentID)
	switch {
	case errors.Is(err, requests.ErrNotYours):
		writeError(w, http.StatusForbidden, "NOT_YOURS", err.Error())
		return
	case errors.Is(err, requests.ErrTerminal):
		writeError(w, http.StatusConflict, "REQUEST_TERMINAL", err.Error())
		return
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no such request")
		return
	case err != nil:
		writeInternal(w, "cancel request", err)
		return
	}
	writeJSON(w, http.StatusOK, stateResponse{State: "cancelled"})
}

// writeNarrowingRefusal answers err where it is one of the refusals of a GitHub App narrowing,
// and reports whether it answered. The request route (createRequest, where Create's check makes
// these refusals before anybody is asked) and the release route (grantValues, where Values
// makes them again) answer them in this one place, so neither route can answer the same
// refusal differently. Each is the caller's own input to fix, so each is a 4xx and none is
// MINT_FAILED.
func writeNarrowingRefusal(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, requests.ErrNarrowingNotApplicable):
		// The request or the grant holds no github-app secret, so nothing it releases could be
		// narrowed. Bad input.
		writeError(w, http.StatusBadRequest, "NARROWING_NOT_APPLICABLE", err.Error())
	case errors.Is(err, requests.ErrNarrowingBehindAirlock):
		// It holds a name the airlock keeps back, whose value the proxy spends on the session's
		// behalf and mints owner-wide once per pod, so no narrowing of its release could hold
		// for it. A conflict between the narrowing and the names asked for, not bad input, so
		// 409 rather than 400.
		writeError(w, http.StatusConflict, "NARROWING_BEHIND_AIRLOCK", err.Error())
	case errors.Is(err, requests.ErrNarrowingOwnerNotListed):
		// The narrowing names an account a github-app secret of it does not list; no minter ran
		// and GitHub was not asked.
		writeError(w, http.StatusForbidden, "NARROWING_OWNER_NOT_LISTED", err.Error())
	case errors.Is(err, secrets.ErrMintNarrowingRefused):
		// GitHub refused the narrowed token: a repository the installation does not reach, or a
		// permission the App does not hold. The message says which it is, and the caller fixes
		// the narrowing.
		writeError(w, http.StatusUnprocessableEntity, "NARROWING_REFUSED", err.Error())
	default:
		return false
	}
	return true
}

func (s *server) grantValues(w http.ResponseWriter, r *http.Request, enrollmentID string) {
	id, ok := pathUUID(w, r, "id", "GRANT_ID_INPUT", "grant")
	if !ok {
		return
	}
	var body grantValuesBody
	if r.ContentLength != 0 && !readJSON(w, r, &body, "INVALID_NARROWING") {
		return
	}
	narrowing, ok := parsedNarrowing(w, body.GitHubApp)
	if !ok {
		return
	}
	var applied *githubNarrowing
	if narrowing != nil {
		applied = &githubNarrowing{Owner: narrowing.Owner, Repositories: narrowing.Repositories, ReadOnly: narrowing.ReadOnly}
	}
	release, err := s.deps.Machine.Values(r.Context(), id, enrollmentID, narrowing, requests.ReleaseCall{Command: body.Command, Reason: body.Reason})
	switch {
	case errors.Is(err, requests.ErrNotYours):
		writeError(w, http.StatusForbidden, "NOT_YOURS", err.Error())
		return
	case errors.Is(err, requests.ErrGrantNotLive):
		writeError(w, http.StatusForbidden, "GRANT_NOT_LIVE", err.Error())
		return
	case errors.Is(err, requests.ErrGrantChainInvalid):
		writeError(w, http.StatusForbidden, "GRANT_CHAIN_INVALID", err.Error())
		return
	case errors.Is(err, requests.ErrSecretNotInStore):
		writeError(w, http.StatusNotFound, "SECRET_NOT_IN_STORE", err.Error())
		return
	case errors.Is(err, requests.ErrMintFailed):
		// The type's upstream (GitHub or Google) refused or failed a mint the narrowing, if any, is
		// not at fault: an App with no installation, a Google login that needs a new sign-in, an
		// upstream that could not be asked. Its message says why and never holds the stored value.
		writeError(w, http.StatusBadGateway, "MINT_FAILED", err.Error())
		return
	case writeNarrowingRefusal(w, err):
		// The narrowing refusals Values makes for a grant released a second time, answered as
		// the request route answers Create's own.
		return
	case writeMemberKindUncheckable(w, "the release", err):
		// As the request route answers it: nothing is released while the broker cannot learn
		// whether the session's person is a restricted member.
		return
	case err != nil:
		writeInternal(w, "read grant values", err)
		return
	}
	values := release.Values
	if values == nil {
		values = map[string]string{}
	}
	answer := grantValuesResponse{ExpiresAt: release.Expires, Values: values, GitHubApp: applied}
	if release.Airlock != nil {
		answer.Airlock = &airlockRelease{
			ProxyURL:      release.Airlock.ProxyURL,
			CACertificate: release.Airlock.CACertificate,
			ProxyPassword: release.Airlock.ProxyPassword,
			Names:         map[string]airlockName{},
		}
		for name, held := range release.Airlock.Names {
			answer.Airlock.Names[name] = airlockName{Value: held.Value, Slots: held.Slots, Hosts: held.Hosts}
		}
	}
	writeJSON(w, http.StatusOK, answer)
}

// revokeGrant ends a grant for the session that holds it — session proof only, per the shared
// broker contract (human revocation is the UI's revoke-by-approver route, which Dispatch's server
// calls with the human's login).
func (s *server) revokeGrant(w http.ResponseWriter, r *http.Request, enrollmentID string) {
	id, ok := pathUUID(w, r, "id", "GRANT_ID_INPUT", "grant")
	if !ok {
		return
	}
	err := s.deps.Machine.RevokeGrant(r.Context(), id, enrollmentID)
	switch {
	case errors.Is(err, requests.ErrNotYours):
		writeError(w, http.StatusForbidden, "NOT_YOURS", err.Error())
		return
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no such grant")
		return
	case err != nil:
		writeInternal(w, "revoke grant", err)
		return
	}
	writeJSON(w, http.StatusOK, stateResponse{State: "revoked"})
}
