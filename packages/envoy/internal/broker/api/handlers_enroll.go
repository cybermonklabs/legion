package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
)

// createEnrollmentBody is POST /v1/enrollments's body (the shared broker contract's
// enrollment route). It names no approver: the policy picks a request's approver at request time,
// never at enrollment. slot is optional and pod-only: omitted or "" is the runtime's one
// enrollment, and a slot names one of several independent enrollments of the same pod
// (enroll.Enrollment.Slot), chosen by the launcher whose proof authenticates the call.
type createEnrollmentBody struct {
	// What the session is: "session" (an agent session on a machine its person logged in, or a
	// machine enrolling under the launcher credential its token exchange minted, which acts for
	// that machine's fleet rather than for a person) or "pod".
	Kind string `json:"kind"`
	// The session's runtime: a pod's UID, or whatever names the session on its machine (the
	// helper's host:pid:start time, a container's id). A launcher enrolls one live session per
	// runtime id (and slot).
	RuntimeID string `json:"runtime_id"`
	// A pod only, optional: which of the pod's independent enrollments this is.
	Slot string `json:"slot"`
	// Optional: the operator the session runs for. The broker records the machine credential's own
	// operator whatever is sent, and refuses any other login. Absent or null for a pod.
	Operator *string `json:"operator"`
	// The RFC 7638 JWK thumbprint of the session's P-256 signing key.
	Thumbprint string `json:"thumbprint"`
	// Optional: the Envoy session the broker notifies when one of this session's pending requests
	// expires.
	SessionID *string `json:"session_id"`
	// A pod only: its projected service-account token, which proves the runtime id is its UID.
	PodToken *string `json:"pod_token"`
	// A pod a launcher enrols only, optional: what the pod runs for, which every later process of
	// it states again (the Legion daemon sends the role claim token, legion-<project>-<issue>-<role>,
	// matching ^[a-z][a-z0-9-]{0,127}$, else 400 INVALID_CLAIM). A request the pod makes belongs to
	// that claim, so the next pod of the claim reads and cancels it and takes the grants a dormant
	// pod of it left (enroll.ownerKey).
	Claim string `json:"claim"`
}

// enrollmentResponse is POST /v1/enrollments's answer: 201 for a new enrollment, 200 when the same
// key is already enrolled in that runtime and slot.
type enrollmentResponse struct {
	// The enrollment's id, which the session's Proof headers name.
	EnrollmentID string `json:"enrollment_id"`
	// When the enrollment's lease lapses unless the session renews it.
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
	// The pod slot it holds; null for every enrollment without one.
	Slot *string `json:"slot"`
}

// leaseResponse is POST /v1/enrollments/{id}/renew's answer.
type leaseResponse struct {
	// When the renewed lease lapses.
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

// renewEnrollmentBody is POST /v1/enrollments/{id}/renew's optional body: no body at all is a
// renewal carrying no token.
type renewEnrollmentBody struct {
	// A pod only: its current projected service-account token, which must name the pod UID and
	// service account the enrollment was made for. Required once the enrollment has renewed with
	// it (enroll.Service.Renew).
	PodToken *string `json:"pod_token"`
}

// selfResponse is GET /v1/enrollments/self's answer: the calling session as the broker knows it.
type selfResponse struct {
	// The session's enrollment id.
	EnrollmentID string `json:"enrollment_id"`
	// The session's live grants, soonest to expire first.
	Grants []requests.Grant `json:"grants"`
	// "session" or "pod".
	Kind string `json:"kind"`
	// When the session's lease lapses unless it is renewed.
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
	// The person the session runs for; null for a pod.
	Operator *string `json:"operator"`
	// The pod slot it holds; null for every enrollment without one.
	Slot *string `json:"slot"`
}

// createEnrollment authenticates by launcher proof now (payload carries "lid"), never a bearer
// token; cred is the launcher credential's own identity, read by server.authenticate from the
// verified proof's launcher id. A machine's own credential enrols a session that carries no
// operator and acts for its fleet's service, as a pod's enrolment does for the service its
// token proves.
func (s *server) createEnrollment(w http.ResponseWriter, r *http.Request, cred enroll.Credential) {
	var body createEnrollmentBody
	if !readJSON(w, r, &body, "INVALID_ENROLLMENT") {
		return
	}
	if body.Kind != "session" && body.Kind != "pod" {
		writeError(w, http.StatusBadRequest, "INVALID_KIND", `kind must be "session" or "pod"`)
		return
	}
	kind := body.Kind

	result, err := s.deps.Enroll.Create(r.Context(), cred, enroll.Enrollment{
		Kind:       kind,
		RuntimeID:  body.RuntimeID,
		Slot:       body.Slot,
		Operator:   body.Operator,
		Thumbprint: body.Thumbprint,
		SessionID:  body.SessionID,
		PodToken:   derefOr(body.PodToken, ""),
		Claim:      body.Claim,
	})
	switch {
	case errors.Is(err, enroll.ErrUnauthenticated):
		writeError(w, http.StatusUnauthorized, "LAUNCHER_INVALID", "the launcher credential is not valid")
		return
	case errors.Is(err, enroll.ErrInvalidSlot):
		writeError(w, http.StatusBadRequest, "INVALID_SLOT", err.Error())
		return
	case errors.Is(err, enroll.ErrInvalidClaim):
		writeError(w, http.StatusBadRequest, "INVALID_CLAIM", err.Error())
		return
	case errors.Is(err, enroll.ErrOperatorMismatch):
		writeError(w, http.StatusForbidden, "OPERATOR_MISMATCH", err.Error())
		return
	// A session name another principal holds and the enrollment's own principal may not take,
	// which the enrollment's own bind refuses (enroll.ErrSessionHeld; an account
	// BROKER_SESSION_ACCOUNT_PAIRS pairs with the holder takes the name instead): the launcher
	// enrols under a name of its own, as the refusal's own text says and as POST /v1/pod-sessions
	// answers a name the configuration has fixed (SESSION_FIXED).
	case errors.Is(err, enroll.ErrSessionHeld):
		writeError(w, http.StatusForbidden, "SESSION_HELD", err.Error())
		return
	case errors.Is(err, enroll.ErrAlreadyEnrolled):
		writeError(w, http.StatusConflict, "ALREADY_ENROLLED", err.Error())
		return
	case errors.Is(err, enroll.ErrPodTokenExpired):
		writeError(w, http.StatusForbidden, "POD_TOKEN_EXPIRED", err.Error())
		return
	case errors.Is(err, enroll.ErrPodIdentity):
		writeError(w, http.StatusForbidden, "POD_IDENTITY_MISMATCH", err.Error())
		return
	case errors.Is(err, enroll.ErrPodSelfEnrollOnly):
		writeError(w, http.StatusForbidden, "POD_SELF_ENROLL_ONLY", err.Error())
		return
	// A pod enrolling itself under a session name (enroll.ErrPodSelfEnrollNoSession, whose own
	// doc says why a name such a pod picked would be a sibling pod's).
	case errors.Is(err, enroll.ErrPodSelfEnrollNoSession):
		writeError(w, http.StatusForbidden, "POD_SELF_ENROLL_NO_SESSION", err.Error())
		return
	case errors.Is(err, enroll.ErrPodNotLaunched):
		writeError(w, http.StatusForbidden, "POD_NOT_LAUNCHED", err.Error())
		return
	case err != nil:
		writeInternal(w, "create enrollment", err)
		return
	}

	status := http.StatusCreated
	if result.Existing {
		status = http.StatusOK
	}
	writeJSON(w, status, enrollmentResponse{
		EnrollmentID:   result.ID.String(),
		LeaseExpiresAt: result.LeaseExpires,
		Slot:           strPtr(result.Slot),
	})
}

// deleteEnrollment is idempotent: revoking an id that is already revoked, or that never existed,
// still answers 204. enroll.Service.Revoke returns ErrNotLive for exactly that no-op case (by
// design, so it never writes a spurious audit row for an enrollment that never transitioned);
// DELETE's own idempotent semantics mean the caller doesn't need to know or care which happened.
// Ownership is checked first: a launcher credential may only revoke an enrollment of its own
// operator, and a pod enrollment, which carries no operator, only when it is the credential that
// enrolled the pod (enroll.Service.Revoke says why, and what that leaves a restarted Legion daemon)
// — so a mismatch is always 403 (OPERATOR_MISMATCH, or NOT_ENROLLING_CREDENTIAL for another
// service credential's pod), never a no-op 204, regardless of whether the target enrollment is live.
func (s *server) deleteEnrollment(w http.ResponseWriter, r *http.Request, cred enroll.Credential) {
	id, ok := pathUUID(w, r, "id", "ENROLLMENT_ID_INPUT", "enrollment")
	if !ok {
		return
	}
	err := s.deps.Enroll.Revoke(r.Context(), cred, id, record.LauncherActor(cred.ID.String()))
	switch {
	case errors.Is(err, enroll.ErrOperatorMismatch):
		writeError(w, http.StatusForbidden, "OPERATOR_MISMATCH", err.Error())
		return
	case errors.Is(err, enroll.ErrNotEnrollingCredential):
		writeError(w, http.StatusForbidden, "NOT_ENROLLING_CREDENTIAL", err.Error())
		return
	case err != nil && !errors.Is(err, enroll.ErrNotLive):
		writeInternal(w, "revoke enrollment", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// maxCarriedBodyBytes is POST /v1/enrollments/carried's body cap, in place of maxJSONBodyBytes:
// the longest list Carried takes, written compactly, with every byte of every id escaped as
// JSON's longest escape (\u00XX, six bytes), its quotes and commas, and the object around it.
// About 6 MiB. Under maxJSONBodyBytes a list of 4,096 plain 255-byte ids, which Carried answers,
// would be refused 413 before it reached Carried.
const maxCarriedBodyBytes = int64(enroll.MaxCarriedSessionIDs*(6*enroll.MaxCarriedSessionIDBytes+3) + len(`{"session_ids":[]}`))

// carriedBody is POST /v1/enrollments/carried's body, which readCarriedBody decodes.
type carriedBody struct {
	// The session ids to ask about: 1 to 4096 of them, each 1 to 255 bytes. An id named twice is
	// answered once.
	SessionIDs []string `json:"session_ids"`
}

// carriedResponse is POST /v1/enrollments/carried's answer.
type carriedResponse struct {
	// Every pod enrollment a login of the caller's own account launched that still carries
	// something for one of the named sessions, sorted by session id and then enrollment id; empty,
	// never null, when none does.
	Carried []carriedEnrollment `json:"carried"`
}

// carriedEnrollment is one enrollment POST /v1/enrollments/carried answers.
type carriedEnrollment struct {
	// The session id the enrollment names.
	SessionID string `json:"session_id"`
	// The enrollment's id.
	EnrollmentID string `json:"enrollment_id"`
	// "live": no end has reached it, and it counts whatever it holds, since it may still be
	// granted something or ask for something. "dormant": a box's end left it holding a live grant
	// or a pending request for the session's next enrollment.
	State string `json:"state"`
	// When it stops carrying unless something ends it first: a live enrollment's lease end, which
	// each renewal moves, and a dormant one's carry window end (BROKER_SESSION_CARRY_SECONDS past
	// its end), when the broker's sweep revokes what it holds.
	EndsAt time.Time `json:"ends_at"`
}

// carriedEnrollments answers which pod enrollments a login of the caller's own account launched
// still carry something for the named sessions, that the next enrollment of the session under
// the same login would take: what enroll.Service.Carried reads, and why each row counts, is that
// method's. A Legion daemon asks before it frees a session id it holds, and frees one only when
// the answer names none of its enrollments (plan E's t-e-54). Another login's enrollments
// are never answered, so another daemon's session of the same id never keeps this one from
// freeing it. A credential whose login names no account, one a person approved through a code,
// launched nothing it could be answered for (403 SERVICE_LOGIN_REQUIRED), and is refused before
// its body is read; any other is answered with the rows its own login launched.
func (s *server) carriedEnrollments(w http.ResponseWriter, r *http.Request, cred enroll.Credential) {
	// The caller is checked first, by the sentinel it answers, so the reference generator
	// (cmd/broker-refgen) reads the refusal's text from it; Carried's own check of the caller
	// then never refuses, and its answer needs no case of its own.
	switch err := enroll.CheckCarriedCaller(cred); {
	case errors.Is(err, enroll.ErrServiceLoginRequired):
		writeError(w, http.StatusForbidden, "SERVICE_LOGIN_REQUIRED", err.Error())
		return
	}
	var body carriedBody
	if !readCarriedBody(w, r, &body) {
		return
	}
	carried, err := s.deps.Enroll.Carried(r.Context(), cred, body.SessionIDs)
	switch {
	case errors.Is(err, enroll.ErrInvalidSessionIDs):
		writeError(w, http.StatusBadRequest, "INVALID_SESSION_IDS", err.Error())
		return
	case err != nil:
		writeInternal(w, "read carried enrollments", err)
		return
	}
	out := carriedResponse{Carried: make([]carriedEnrollment, 0, len(carried))}
	for _, c := range carried {
		out.Carried = append(out.Carried, carriedEnrollment{
			SessionID: c.SessionID, EnrollmentID: c.EnrollmentID.String(), State: c.State, EndsAt: c.EndsAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// readCarriedBody reads POST /v1/enrollments/carried's body into body, a carriedBody of at most
// maxCarriedBodyBytes (413 REQUEST_TOO_LARGE), as readJSON reads one under its own cap, but one id at a time: a list
// longer than enroll.MaxCarriedSessionIDs is refused at the entry past it, before the rest of the
// body is read or held (400 INVALID_SESSION_IDS). Decoding the whole list first let a body of empty
// ids at the cap decode to about two million of them, 180 MiB allocated, before the count was
// checked (side review of #4355). Any other fault of the body is 400 INVALID_SESSION_IDS too. It
// takes the body's address as readJSON does, which is how the reference generator names the
// route's request body. It writes the refusal itself and reports whether it read the body.
func readCarriedBody(w http.ResponseWriter, r *http.Request, body *carriedBody) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxCarriedBodyBytes)
	read, err := decodeCarriedBody(json.NewDecoder(r.Body))
	var tooLarge *http.MaxBytesError
	switch {
	case err == nil:
		*body = read
		return true
	case errors.As(err, &tooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", fmt.Sprintf("request body exceeds %d bytes", maxCarriedBodyBytes))
	case errors.Is(err, enroll.ErrInvalidSessionIDs):
		writeError(w, http.StatusBadRequest, "INVALID_SESSION_IDS", err.Error())
	default:
		writeError(w, http.StatusBadRequest, "INVALID_SESSION_IDS", "body must be one valid JSON object: "+err.Error())
	}
	return false
}

// decodeCarriedBody decodes one carriedBody from d, a token at a time: an object whose one field
// is session_ids, a list of strings, and nothing after it. It stops at the list's entry past
// enroll.MaxCarriedSessionIDs with enroll.ErrInvalidSessionIDs. A body with no list answers no
// ids, which Carried refuses as naming none.
func decodeCarriedBody(d *json.Decoder) (carriedBody, error) {
	var body carriedBody
	if err := wantDelim(d, '{'); err != nil {
		return carriedBody{}, err
	}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return carriedBody{}, err
		}
		if key != "session_ids" || body.SessionIDs != nil {
			return carriedBody{}, fmt.Errorf("the body's field %v is not session_ids, or names it twice", key)
		}
		if err := wantDelim(d, '['); err != nil {
			return carriedBody{}, err
		}
		body.SessionIDs = []string{}
		for d.More() {
			if len(body.SessionIDs) == enroll.MaxCarriedSessionIDs {
				return carriedBody{}, fmt.Errorf("%w: it names more than %d", enroll.ErrInvalidSessionIDs, enroll.MaxCarriedSessionIDs)
			}
			var id string
			if err := d.Decode(&id); err != nil {
				return carriedBody{}, err
			}
			body.SessionIDs = append(body.SessionIDs, id)
		}
		if err := wantDelim(d, ']'); err != nil {
			return carriedBody{}, err
		}
	}
	if err := wantDelim(d, '}'); err != nil {
		return carriedBody{}, err
	}
	if _, err := d.Token(); err != io.EOF {
		if err == nil {
			err = errors.New("body must contain one JSON value")
		}
		return carriedBody{}, err
	}
	return body, nil
}

// wantDelim reads d's next token and fails unless it is delim.
func wantDelim(d *json.Decoder, delim json.Delim) error {
	got, err := d.Token()
	if err != nil {
		return err
	}
	if got != delim {
		return fmt.Errorf("the body has %v where %v belongs", got, delim)
	}
	return nil
}

// renewEnrollment renews the caller's own enrollment. The proof-verified enrollment id is
// authoritative — a session can only ever act as itself — so a URL {id} that names a different
// enrollment is refused rather than silently honored or silently ignored. A pod's renewal carries
// its current projected token in the body, which enroll.Service.Renew verifies against the pod UID
// and service account the enrollment was made for; a pod enrollment that has never renewed with
// one still renews with no body, logged here, naming the enrollment, until the token-less path is
// removed (Renew says when).
//
// A machine's own session whose launcher credential has ended (enroll.ErrMachineLoginEnded: the
// machine login was revoked, lapsed, or its account was dropped from BROKER_VM_SERVICES) is
// answered ENROLLMENT_NOT_LIVE, which is what the enrollment now is, rather than a code of its
// own, which would read as a retryable failure to a renewer that has not heard of it. Every
// renewer treats it as terminal, and what each does then is its own: a session the helper holds
// is enrolled again by the helper's own loop, under whatever credential it holds then
// (helper.Server.enrollLoop); `agent-secrets renew` stops, exit 76 naming the enrollment, and
// leaves the enrolling again to whatever enrolled the box or pod in the first place, which for
// a box enrolled through `enroll-session-key` is a person running that command again. The WARN
// beside it is the only record naming which of the two refusals an operator is looking at.
func (s *server) renewEnrollment(w http.ResponseWriter, r *http.Request, id string) {
	urlID, ok := pathUUID(w, r, "id", "ENROLLMENT_ID_INPUT", "enrollment")
	if !ok {
		return
	}
	if urlID != id {
		writeError(w, http.StatusForbidden, "NOT_YOURS", "a session may only renew its own enrollment")
		return
	}
	var body renewEnrollmentBody
	if r.ContentLength != 0 && !readJSON(w, r, &body, "INVALID_RENEWAL") {
		return
	}
	renewal, err := s.deps.Enroll.Renew(r.Context(), id, derefOr(body.PodToken, ""))
	switch {
	case errors.Is(err, enroll.ErrNotLive):
		writeError(w, http.StatusUnauthorized, "ENROLLMENT_NOT_LIVE", "enrollment is not live")
		return
	case errors.Is(err, enroll.ErrMachineLoginEnded):
		slog.Warn("broker: machine session renewal refused: its machine login has ended", "enrollment", id)
		writeError(w, http.StatusUnauthorized, "ENROLLMENT_NOT_LIVE", err.Error())
		return
	case errors.Is(err, enroll.ErrPodTokenExpired):
		writeError(w, http.StatusForbidden, "POD_TOKEN_EXPIRED", err.Error())
		return
	case errors.Is(err, enroll.ErrPodIdentity):
		writeError(w, http.StatusForbidden, "POD_IDENTITY_MISMATCH", err.Error())
		return
	case errors.Is(err, enroll.ErrPodTokenRequired):
		slog.Warn("broker: pod enrollment renewal refused: projected token required", "enrollment", id)
		writeError(w, http.StatusForbidden, "POD_TOKEN_REQUIRED", err.Error())
		return
	case err != nil:
		writeInternal(w, "renew enrollment", err)
		return
	}
	if renewal.WithoutPodToken {
		slog.Warn("broker: pod enrollment renewed without its projected token", "enrollment", id, "pod_uid", renewal.RuntimeID, "slot", renewal.Slot)
	}
	writeJSON(w, http.StatusOK, leaseResponse{LeaseExpiresAt: renewal.LeaseExpires})
}

func (s *server) readSelf(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	enr, err := s.deps.Enroll.Get(ctx, id)
	if errors.Is(err, enroll.ErrNotLive) {
		writeError(w, http.StatusUnauthorized, "PROOF_INVALID", "enrollment is not live")
		return
	}
	if err != nil {
		writeInternal(w, "read enrollment", err)
		return
	}
	grants, err := s.deps.Machine.LiveGrants(ctx, id)
	if err != nil {
		writeInternal(w, "read live grants", err)
		return
	}
	if grants == nil {
		grants = []requests.Grant{}
	}
	writeJSON(w, http.StatusOK, selfResponse{
		EnrollmentID:   enr.ID.String(),
		Grants:         grants,
		Kind:           enr.Kind,
		LeaseExpiresAt: enr.LeaseExpires,
		Operator:       enr.Operator,
		Slot:           strPtr(enr.Slot),
	})
}

func derefOr(s *string, def string) string {
	if s == nil {
		return def
	}
	return *s
}
