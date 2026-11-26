package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// writeGitHubAppRefusal answers a github_app field the broker does not answer
// (requests.ErrGitHubAppFieldRefused) 400 GITHUB_APP_FIELD_REMOVED, and reports true: every one
// with BROKER_GITHUB_APP_FIELD=off, with the message a broker before the answer gave, and one the
// broker cannot serve, naming why. The field is declared on each body that takes it, rather than
// left to readJSON's refusal of a field the body does not take, because that refusal is 400
// INVALID_REQUEST on POST /v1/requests: agent-secrets of an earlier release reads that code as a
// broker from before github_app and sends the same request again without it, which would run its
// command with a token wider than the one it asked for. A body that carries github_app beside a
// field the route does not take is refused at that field first, since the body is read once; no
// released command line sends such a body.
func writeGitHubAppRefusal(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, requests.ErrGitHubAppFieldRefused) {
		return false
	}
	writeError(w, http.StatusBadRequest, "GITHUB_APP_FIELD_REMOVED", err.Error())
	return true
}

// gitHubAppAnswer is the github_app object a release answers for a body that carried one: the
// narrowing it applied, as the body named it, which a released client compares with what it
// asked for before it runs anything.
type gitHubAppAnswer struct {
	// The one GitHub account whose installation the token was minted for, lowercased, as the
	// body's github_app named it.
	Owner string `json:"owner"`
	// The repositories of that account the token reaches, without the account, sorted and each
	// once; absent when the body named none, which is every repository of that account the
	// profile the request was served as reaches.
	Repositories []string `json:"repositories,omitempty"`
	// True when the body's read_only asked for a token that reads alone: the broker then mints
	// each of actions, checks, contents, metadata, pull_requests and statuses at read where the
	// profile holds it, and none it does not. Absent otherwise, when the token holds the
	// profile's own permissions.
	ReadOnly bool `json:"read_only,omitempty"`
}

// writeProfileRefusal answers the refusals of a GitHub profile that POST /v1/requests and the
// release route share, and reports true, so neither route can answer the same refusal
// differently. Each route asks it before its own arms: neither class is ever wrapped in an
// error another arm answers.
//
// 400 TARGET_REFUSED is a target the profile cannot serve (requests.ErrTargetRefused): outside
// the profile, on a name with no github block, or missing on a value-token profile of several
// accounts. Its message names the profile's accounts and repositories, so the caller names a
// target inside them. 400 TARGET_REPOSITORIES_REFUSED is the one such target that has a code of
// its own (requests.ErrTargetRepositoriesRefused): a target naming repositories of a profile
// served from a github-installations secret, whose tokens were minted ahead of the request and
// cannot be cut to repositories; its message names the account to name instead.
//
// 422 PROFILE_MINT_REFUSED is a scope the minter refused (secrets.ErrMintNarrowingRefused): its
// permissions cut to what an installation holds leave none but metadata, a bare token of more
// than one installation, or GitHub refused a repository it names; the message says which, and the
// profiles file or the caller's target fixes it. So is an App with no installation on an account
// the scope names (secrets.ErrMintNoInstallation) that Create's check mint found before anybody
// was asked; a release wraps that same refusal in requests.ErrMintFailed and answers it 502
// MINT_FAILED, as it answers every failed mint, so it is not answered here.
//
// Either way Create wrote nothing and asked nobody, and a release released nothing.
func writeProfileRefusal(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, requests.ErrTargetRepositoriesRefused):
		writeError(w, http.StatusBadRequest, "TARGET_REPOSITORIES_REFUSED", err.Error())
	case errors.Is(err, requests.ErrTargetRefused):
		writeError(w, http.StatusBadRequest, "TARGET_REFUSED", err.Error())
	case errors.Is(err, secrets.ErrMintNarrowingRefused),
		errors.Is(err, secrets.ErrMintNoInstallation) && !errors.Is(err, requests.ErrMintFailed):
		writeError(w, http.StatusUnprocessableEntity, "PROFILE_MINT_REFUSED", err.Error())
	default:
		return false
	}
	return true
}

// projectAccessUnavailable is the one sentence a session reads for a 503
// PROJECT_ACCESS_UNAVAILABLE. It names no project: what the K3 client answered names the tag's
// project key ("dispatch answered /api/v1/access/projects/PRIV 503"), and the broker names no
// project to a session, so that text goes to the broker's log alone
// (writeProjectAccessUnavailable).
const projectAccessUnavailable = "the broker could not ask Dispatch about the project of a requested secret, so nothing was decided or released; ask again once Dispatch answers"

// writeProjectAccessUnavailable logs err, which names the project Dispatch was asked about, and
// answers the session 503 PROJECT_ACCESS_UNAVAILABLE with projectAccessUnavailable alone.
func writeProjectAccessUnavailable(w http.ResponseWriter, op string, err error) {
	slog.Warn("broker: "+op+": Dispatch could not be asked about a project", "error", err)
	writeError(w, http.StatusServiceUnavailable, "PROJECT_ACCESS_UNAVAILABLE", projectAccessUnavailable)
}

// createRequestBody is POST /v1/requests's exact shape: a signed request object plus one optional,
// unsigned field, session_id (wake-only). The v8 "secrets"/"reason"/"issue" top-level fields are
// gone — they now live inside the signed request object itself.
type createRequestBody struct {
	// The session's signed request: a compact ES256 JWS, signed with the session's key and
	// addressed to the broker's public URL, naming the secrets (authorization_details) and the
	// reason.
	Request string `json:"request"`
	// Optional: the Envoy session the broker notifies if this request expires undecided.
	SessionID *string `json:"session_id"`
	// Optional: the narrowing of a stored github-app secret an agent-secrets of a release before
	// --target sends. The request is served as a profile of that secret the profiles file grants
	// the caller at once, narrowed to its account and repositories (requests.Machine.CreateNarrowed).
	// With BROKER_GITHUB_APP_FIELD=off, and where the broker cannot serve it, it is refused 400
	// GITHUB_APP_FIELD_REMOVED, and nothing is requested.
	GitHubApp json.RawMessage `json:"github_app"`
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
	var req requests.Request
	var err error
	if body.GitHubApp != nil {
		narrowing, refused := s.deps.Machine.GitHubAppNarrowing(body.GitHubApp)
		if writeGitHubAppRefusal(w, refused) {
			return
		}
		req, err = s.deps.Machine.CreateNarrowed(r.Context(), enrollmentID, body.Request, derefOr(body.SessionID, ""), narrowing)
	} else {
		req, err = s.deps.Machine.Create(r.Context(), enrollmentID, body.Request, derefOr(body.SessionID, ""))
	}
	if writeGitHubAppRefusal(w, err) || writeProfileRefusal(w, err) {
		return
	}
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
	case errors.Is(err, policy.ErrServedOnlyAsProfiles):
		// A source asked for by its own name, which profiles of other names use: the message
		// names those the policy serves, so the caller asks for one of those instead, or says
		// that the policy refuses each of them. It is final, as UNKNOWN_SECRET is, and asked
		// before that code, since the policy's error is that one too.
		writeError(w, http.StatusForbidden, "SERVED_ONLY_AS_PROFILES", err.Error())
		return
	case errors.Is(err, policy.ErrProfileRefused):
		// A profile the policy refuses against its source, or a github-app secret no profile
		// names: the message gives the reason and its detail, which the profiles file or the
		// source's tags fix.
		writeError(w, http.StatusForbidden, "PROFILE_REFUSED", err.Error())
		return
	case errors.Is(err, policy.ErrUnknownSecret):
		writeError(w, http.StatusBadRequest, "UNKNOWN_SECRET", err.Error())
		return
	case errors.Is(err, requests.ErrProjectAccessUnavailable):
		// A requested secret belongs to a Dispatch project, and the broker could not ask Dispatch
		// about it. Nothing was decided; the same request succeeds once Dispatch answers.
		writeProjectAccessUnavailable(w, "create request", err)
		return
	case errors.Is(err, policy.ErrPendingValue):
		// A secret `agent-secrets secret create` made and has not yet filled: not final, unlike
		// UNKNOWN_SECRET, so the caller may ask again once the owner stores the value.
		writeError(w, http.StatusBadRequest, "PENDING_VALUE", err.Error())
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

// grantValuesBody is POST /v1/grants/{id}/values's optional body: no body at all releases every
// value as the grant holds it, and so does one naming only what the session says of the release.
// One the route cannot read is refused 400 INVALID_RELEASE.
type grantValuesBody struct {
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
	// Optional: the narrowing of a stored github-app secret an agent-secrets of a release before
	// --target sends. The grant's one GitHub profile, whose target must be this narrowing's, is
	// released in the installations form under the stored secret's name, outside the airlock, and
	// the answer echoes it in github_app (requests.Machine.ValuesNarrowed). With
	// BROKER_GITHUB_APP_FIELD=off, and for a grant of another shape, it is refused 400
	// GITHUB_APP_FIELD_REMOVED, and nothing is released.
	GitHubApp json.RawMessage `json:"github_app"`
}

// grantValuesResponse is POST /v1/grants/{id}/values's answer.
type grantValuesResponse struct {
	// When the grant expires; the session can read the values again until then. Null for a grant
	// that lasts until the session ends.
	ExpiresAt *time.Time `json:"expires_at"`
	// Each granted name's value, read fresh from the secret store.
	Values map[string]string `json:"values"`
	// For each GitHub profile whose permissions this release cut to what an installation holds,
	// by profile name and then account, every entry the cut left out: "<permission>" for a
	// permission the installation holds none of, "<permission>:<level>" for one it holds at a
	// lower level, which the token holds at that level instead. Absent when no cut left anything
	// out.
	GitHubLeftOut map[string]map[string][]string `json:"github_left_out,omitempty"`
	// The grant's target as its request signed it: one https://github.com/<account> for the
	// account form, or one https://github.com/<account>/<repository> for each repository, sorted.
	// Absent when the grant has no target. A client that signed a target compares it with this,
	// since a broker from before targets reads the signed object without refusing the field and
	// releases the whole profile.
	Locations []string `json:"locations,omitempty"`
	// How to reach the values this release did not hand over: the names the airlock holds back
	// are not in "values" at all, and this says where the proxy that holds them is. Absent when
	// no granted name is behind the airlock, which is every release of a broker with no rules
	// file.
	Airlock *oauthwire.AirlockRelease `json:"airlock,omitempty"`
	// The narrowing this release applied, as a body's github_app field named it; absent for a
	// body with no github_app. A released client that asked for one compares it before it runs.
	GitHubApp *gitHubAppAnswer `json:"github_app,omitempty"`
}

// requestWaitStep is how often a held read reads its request again.
const requestWaitStep = 500 * time.Millisecond

// readRequest answers one request to any enrollment of the principal that made it: the same
// person, the same Legion claim, the same machine (requests.Machine.OwnerAndState), and a request
// of an enrollment that records no owner to that enrollment alone. Everyone else is 403 NOT_YOURS,
// but before that owner check a session other than the one that holds the request is answered 404
// exactly as for an id no request has when the request touches a private project the session
// cannot see, whoever owns it, and 503 PROJECT_ACCESS_UNAVAILABLE when Dispatch could not say
// (requests.Machine.HideRequestFromSession), so NOT_YOURS says only that a request exists in a
// project the caller sees.
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
	case errors.Is(err, requests.ErrProjectAccessUnavailable):
		writeProjectAccessUnavailable(w, "read request", err)
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

// readHeld reads request id for enrollmentID. It first reads the session rule
// (Machine.HideRequestFromSession), whose pgx.ErrNoRows it answers as for an id no request has;
// then the caller's owner once (Machine.EnrollmentOwner), then the request's owner, state and
// whether the model judge holds it (Machine.OwnerAndState, one statement), refusing
// requests.ErrNotOwner when the request's owner is not the caller's; while the state is pending
// and deadline has not passed, it reads them again every requestWaitStep; then it reads the whole
// request (Machine.Get) once. A request the judge held at the first read and holds no more has
// been handed to its person, still pending: that ends the hold too, so the caller learns at once
// who decides it now and where. A deadline already passed is the one read. The owner is checked
// at every step, so a request that moves to an enrollment of another owner while the read is held
// is refused at the next step, as a new read of it is. Any other error is the store's, or ctx's
// when ctx ended first.
func (s *server) readHeld(ctx context.Context, id, enrollmentID string, deadline time.Time) (requests.Request, error) {
	if err := s.deps.Machine.HideRequestFromSession(ctx, id, enrollmentID); err != nil {
		return requests.Request{}, err
	}
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

// cancelRequest ends a pending request for any enrollment of the principal that made it, by the
// read route's rule (requests.Machine.Cancel): 403 NOT_YOURS for every other enrollment, but before
// that owner check a session other than the one that holds the request is answered 404 exactly as
// for an id no request has when the request touches a private project the session cannot see, and
// 503 PROJECT_ACCESS_UNAVAILABLE, cancelling nothing, when Dispatch could not say.
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
	case errors.Is(err, requests.ErrProjectAccessUnavailable):
		writeProjectAccessUnavailable(w, "cancel request", err)
		return
	case err != nil:
		writeInternal(w, "cancel request", err)
		return
	}
	writeJSON(w, http.StatusOK, stateResponse{State: "cancelled"})
}

// grantValues releases a live grant's values to the session that holds it (requests.Machine.Values):
// every other session is 403 NOT_YOURS, but before that owner check a session other than the
// holder is answered 403 GRANT_NOT_LIVE exactly as for an id no grant has when the grant touches a
// private project the session cannot see, and 503 PROJECT_ACCESS_UNAVAILABLE, releasing nothing,
// when Dispatch could not say.
func (s *server) grantValues(w http.ResponseWriter, r *http.Request, enrollmentID string) {
	id, ok := pathUUID(w, r, "id", "GRANT_ID_INPUT", "grant")
	if !ok {
		return
	}
	var body grantValuesBody
	if r.ContentLength != 0 && !readJSON(w, r, &body, "INVALID_RELEASE") {
		return
	}
	call := requests.ReleaseCall{Command: body.Command, Reason: body.Reason}
	var release requests.Release
	var err error
	var narrowing *requests.GitHubAppNarrowing
	if body.GitHubApp != nil {
		asked, refused := s.deps.Machine.GitHubAppNarrowing(body.GitHubApp)
		if writeGitHubAppRefusal(w, refused) {
			return
		}
		narrowing = &asked
		release, err = s.deps.Machine.ValuesNarrowed(r.Context(), id, enrollmentID, call, asked)
	} else {
		release, err = s.deps.Machine.Values(r.Context(), id, enrollmentID, call)
	}
	if writeGitHubAppRefusal(w, err) || writeProfileRefusal(w, err) {
		return
	}
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
	case errors.Is(err, requests.ErrMintedOnly):
		// A stored secret under a proxy block of kind dpop: that name is a capability the broker
		// mints and holds, and nothing of the stored value is released, to the session or to the
		// proxy. The refusal is the policy's, never a fault of the broker's.
		writeError(w, http.StatusForbidden, "MINTED_ONLY", err.Error())
		return
	case errors.Is(err, requests.ErrMintFailed):
		// The type's upstream (GitHub or Google) refused or failed a mint the profile's scope is
		// not at fault for: an App with no installation, a Google login that needs a new sign-in,
		// an upstream that could not be asked. Its message says why and never holds the stored
		// value.
		writeError(w, http.StatusBadGateway, "MINT_FAILED", err.Error())
		return
	case errors.Is(err, requests.ErrProjectAccessUnavailable):
		// The grant holds a secret of a Dispatch project, which is checked again on every release,
		// and Dispatch did not answer. Nothing was released and the grant stays live.
		writeProjectAccessUnavailable(w, "read grant values", err)
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
	if narrowing != nil {
		// The answer a released client reads: its values and the narrowing applied, nothing
		// it does not know.
		writeJSON(w, http.StatusOK, grantValuesResponse{ExpiresAt: release.Expires, Values: values,
			GitHubApp: &gitHubAppAnswer{Owner: narrowing.Owner, Repositories: narrowing.Repositories, ReadOnly: narrowing.ReadOnly}})
		return
	}
	answer := grantValuesResponse{ExpiresAt: release.Expires, Values: values, GitHubLeftOut: release.GitHubLeftOut, Locations: release.Locations}
	answer.Airlock = airlockReleaseOf(release.Airlock)
	writeJSON(w, http.StatusOK, answer)
}

// revokeGrant ends a grant for the session that holds it — session proof only, per the shared
// broker contract (human revocation is the UI's revoke-by-approver route, which Dispatch's server
// calls with the human's login). Every other session is 403 NOT_YOURS, but before that owner check
// a session other than the holder is answered 404 exactly as for an id no grant has when the grant
// touches a private project the session cannot see, and 503 PROJECT_ACCESS_UNAVAILABLE, revoking
// nothing, when Dispatch could not say (requests.Machine.RevokeGrant).
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
	case errors.Is(err, requests.ErrProjectAccessUnavailable):
		writeProjectAccessUnavailable(w, "revoke grant", err)
		return
	case err != nil:
		writeInternal(w, "revoke grant", err)
		return
	}
	writeJSON(w, http.StatusOK, stateResponse{State: "revoked"})
}

// airlockReleaseOf is a release's airlock in its wire shape, nil for a release that holds no name
// back.
func airlockReleaseOf(release *requests.AirlockRelease) *oauthwire.AirlockRelease {
	if release == nil {
		return nil
	}
	names := make(map[string]oauthwire.AirlockName, len(release.Names))
	for name, held := range release.Names {
		names[name] = oauthwire.AirlockName{Value: held.Value, Slots: held.Slots, Hosts: held.Hosts}
	}
	return &oauthwire.AirlockRelease{ProxyURL: release.ProxyURL, CACertificate: release.CACertificate, ProxyPassword: release.ProxyPassword, Names: names}
}
