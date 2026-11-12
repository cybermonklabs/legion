package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// maxRequestBytes bounds a request body: every body this API reads is a handful of short strings.
const maxRequestBytes = 1 << 20

// Supervisor is the claims the daemon supervises, as the routes reach them. Every change to a
// claim is a request posted to its machine, which decides it; the routes decide nothing about a
// claim themselves.
type Supervisor interface {
	// Machine is the claim's machine, if the daemon supervises the claim.
	Machine(token claim.Token) (*supervise.Machine, bool)
	// Create stores c — a new claim, queued — with its role prompt and starts supervising it,
	// reporting true. A claim the daemon already supervises is returned as it is, with false:
	// what may happen to it is its machine's to decide, and its role prompt is not replaced.
	Create(ctx context.Context, c supervise.Claim, rolePrompt string) (*supervise.Machine, bool, error)
	// Claims is every claim the daemon supervises, as its store holds them.
	Claims(ctx context.Context) ([]supervise.Claim, error)
	// Step is one claim's step record as the store holds it: what the daemon last saw that
	// claim's agent do. It is no part of a machine's state (supervise's putStep), so every
	// operator answer about one claim reads it here (operatorAnswer), as the listing of them all
	// reads it with the claims. A claim the store no longer holds has the zero record.
	Step(ctx context.Context, token claim.Token) (supervise.StepRecord, error)
	// PostStaleRelaunch posts to a claim's machine the relaunch its stale registration raised
	// and the route that refused that registration could not await: it can take the pod's launch
	// turn, which nothing bounds (supervise.Machine.RelaunchStaleRegistration). generation is the
	// launch whose registration was refused, so a claim that has launched again since is left
	// alone. released is the route's decision record (server.posted): whatever takes the post
	// calls it exactly once — when the relaunch has run, or at once where nothing will run it
	// (supervision stopped, a claim this daemon does not supervise).
	PostStaleRelaunch(token claim.Token, generation uint64, released func())
}

// register is the agent's first call: its pane's boot token, and the session it became. The token
// names the launch; the registration is that launch's machine's to accept or refuse, and an
// accepted one is issued a secret whose hash the machine has persisted before this answers — a
// store that refuses the write is a 500 with no secret, so no agent holds a secret the daemon
// forgot. A launch of the daemon's own controller registers as the project's controller too
// (registerLaunchedController), and only under `controller: daemon`. A token no launch minted may
// be the controller capability `legion controller start` fetched, which registers the project's
// controller (registerController), and only under `controller: operator`: a daemon that launches
// its own controller registers no other, so such a token is the unknown one it is.
//
// The whole of it is answered within one bound, taken here at the first statement
// (Options.WorkerRPCTimeout, R4 of dispatch://INFRA-2883): the agent runs a whole phase with no
// `legion` tool while this hangs, so it is told to retry rather than left waiting. The bound
// covers the body read, the boot token's resolve, the entry into the claim's machine and every
// store write the decision makes before this answers. A registration that cannot have the
// machine inside it is refused that retryable refusal with nothing recorded, and the work the
// decision would do beyond recording the registration — the relaunch a stale plugin contract
// raises — is posted to the claim's machine rather than awaited (staleRelaunchPosted).
func (s *server) register(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.bound(w, r)
	defer cancel()
	var req claim.RegisterRequest
	if !s.readRegisterBody(w, r, &req) || !requireFields(w,
		field{"bootToken", req.BootToken}, field{"sessionId", req.SessionID},
		field{"ompSessionFile", req.OmpSessionFile}, field{"agentId", req.AgentID},
	) {
		return
	}
	launch, known, err := s.bootTokens.Resolve(ctx, req.BootToken)
	if err != nil {
		if boundSpent(ctx, err) {
			s.refuseRegistrationUnanswered(w, r, req.BootToken, "the boot token's resolve did not answer inside the bound")
			return
		}
		s.logRequestFailure(r, err, "api: resolve a registration's boot token")
		writeJSON(w, http.StatusInternalServerError, errorBody("register failed: the daemon could not resolve the boot token"))
		return
	}
	if !known {
		if s.controllerLaunched {
			writeJSON(w, claim.InvalidBootToken.Status, claim.InvalidBootToken)
			return
		}
		s.registerController(ctx, w, r, req)
		return
	}
	m, supervised := s.supervisor.Machine(launch.Claim)
	if !supervised {
		writeJSON(w, claim.InvalidBootToken.Status, claim.InvalidBootToken)
		return
	}
	if published(m).Role == claim.RoleController {
		if !s.controllerLaunched {
			s.log.Warn("api: refused a launched controller's registration: this daemon leaves the controller to its operator",
				"claim", launch.Claim, "generation", launch.Generation, "session", req.SessionID)
			writeJSON(w, http.StatusConflict, errorBody(fmt.Sprintf(
				"%s is a launch of the daemon's own controller, and this daemon leaves the controller to its operator (controller: operator)", launch.Claim)))
			return
		}
		s.registerLaunchedController(ctx, w, r, req, launch, m)
		return
	}
	secret := rand.Text()
	// The decision is the machine's and runs to its end, or to the bound this route answers
	// within: a caller that hangs up mid-request does not get to leave a registration
	// half-recorded, and a machine that is busy for the whole bound leaves it unrecorded.
	decided, decide := s.decisionOn(ctx, launch.Claim)
	defer decide()
	err = m.HandleWithin(decided, supervise.RequestRegister{
		Claim:          launch.Claim,
		Generation:     launch.Generation,
		Session:        req.SessionID,
		SessionFile:    req.OmpSessionFile,
		CapabilityHash: capabilityHash(secret),
		PluginContract: req.PluginContract,
	})
	if err != nil {
		if errors.Is(err, supervise.ErrMachineBusy) {
			s.refuseRegistrationUnanswered(w, r, req.BootToken, machineBusyReason)
			return
		}
		if boundSpent(decided, err) {
			s.refuseRegistrationUnanswered(w, r, req.BootToken, boundSpentReason)
			return
		}
		s.postStaleRelaunch(launch, err)
		s.claimFailure(w, "register", launch.Claim, err)
		return
	}
	c := published(m)
	s.registrationAccepted(launch, req.BootToken)
	s.log.Info("api: claim registered", "claim", c.Token, "generation", launch.Generation,
		"session", req.SessionID, "agent", req.AgentID, "pluginContract", req.PluginContract)
	writeJSON(w, http.StatusOK, claim.RegisterResponse{
		ClaimToken:   c.Token,
		Tree:         c.Tree,
		Issue:        c.Issue,
		Role:         c.Role,
		Generation:   launch.Generation,
		Secret:       secret,
		PromptAgents: s.promptAgentsList(),
	})
}

// refuseRegistrationUnanswered is the retryable refusal a registration gets when this process
// could not answer it inside its bound, and the memory of the launch it belongs to: that launch
// got no answer from this daemon, so it must not be charged for one (Answering.Refused). The
// launch is named by its boot token, which is all a registration carries.
func (s *server) refuseRegistrationUnanswered(w http.ResponseWriter, r *http.Request, bootToken, reason string) {
	if s.refusals != nil {
		s.refusals.RefusedRegistration(bootToken)
	}
	s.retryable(w, r, reason)
}

// refuseReadyUnanswered is refuseRegistrationUnanswered for a ready, which names its launch by
// the claim and generation it carries rather than by a boot token.
func (s *server) refuseReadyUnanswered(w http.ResponseWriter, r *http.Request, token claim.Token, generation uint64, reason string) {
	if s.refusals != nil {
		s.refusals.RefusedReady(token, generation)
	}
	s.retryable(w, r, reason, "claim", token, "generation", generation)
}

// machineBusyReason, controllerLockHeldReason and boundSpentReason are why a route could not
// answer inside its bound, as the line it logs names it (retryable). Each is written once and
// read by both of the routes that can meet it, so the two never come to say the same wait
// differently.
const (
	machineBusyReason        = "the claim's machine was deciding something else for the whole bound"
	controllerLockHeldReason = "the controller lock was held by another caller for the whole bound"
	boundSpentReason         = "a store read or write of this route was still running when the bound ran out"
)

// boundSpent is whether err is this route's own bound running out rather than a failure of the
// work itself: a store call takes the route's context (bound), so a read or a write still
// running at the deadline comes back as that deadline, wrapped by whatever made the call. Such
// an answer is the retryable refusal R4 asks for and never a 500: the caller asks again, and the
// launch is one this daemon did not answer.
func boundSpent(ctx context.Context, err error) bool {
	return ctx.Err() != nil && errors.Is(err, ctx.Err())
}

// registrationAccepted is what a registration this daemon answered and the claim's machine
// accepted owes this process's memory: the registration it refused of that launch, and whatever
// it refused of the claim's earlier generations, are no longer waivers to give
// (Answering.Registered, dispatch://INFRA-2883 R6). The run of waived generations goes on to the
// ready (readyAccepted). A deployment that keeps no such memory has nothing to forget.
func (s *server) registrationAccepted(launch BootToken, bootToken string) {
	if s.refusals == nil {
		return
	}
	s.refusals.Registered(launch.Claim, launch.Generation, supervise.HashBootToken(bootToken))
}

// readyAccepted is what a ready this daemon answered and the claim's machine accepted owes this
// process's memory: the generation came up, so the ready it refused of that generation is no
// longer a waiver to give, and the run of generations it has waived ends here
// (Answering.Readied, dispatch://INFRA-2883 R6). A deployment that keeps no such memory has
// nothing to forget.
func (s *server) readyAccepted(token claim.Token, generation uint64) {
	if s.refusals == nil {
		return
	}
	s.refusals.Readied(token, generation)
}

// postStaleRelaunch posts to the claim's machine the relaunch its stale registration raised,
// which the decision no longer makes itself: it can take the pod's launch turn, which nothing
// bounds (supervise.Machine.RelaunchStaleRegistration). The post carries this route's decision
// record, so the daemon's stop waits for the relaunch as it waits for a decision made inside the
// request, and whatever takes it releases that record — at once, where nothing will run it.
// Every other refusal of a registration posts nothing.
func (s *server) postStaleRelaunch(launch BootToken, err error) {
	if !errors.Is(err, supervise.ErrStaleRegistrationRelaunch) {
		return
	}
	s.supervisor.PostStaleRelaunch(launch.Claim, launch.Generation, s.posted(launch.Claim))
}

// promptAgentsList is Options.PromptAgents as the registration answers it: never nil, so a role
// whose prompts dispatch no task agent reads `[]` rather than `null`.
func (s *server) promptAgentsList() []string {
	if len(s.promptAgents) == 0 {
		return []string{}
	}
	return s.promptAgents
}

// ready is the agent saying it can be prompted; its machine sends the pending delivery, if any,
// once the claim's connection is registered. A ready that carries the session's capability report
// (LEGION-663) hands it, normalised, to Options.CapabilityReported once the claim has taken the
// ready: what the report says is never a reason to refuse the ready — the daemon runs the session
// and reports what it lacks — and a row the table has no live row for is dropped and logged by its
// name cleaned as a detail is (capabilities.Normalize).
//
// It answers within the registration's own bound (Options.WorkerRPCTimeout, register), since its
// refusal is the same answer: an agent whose ready never returns is an agent that cannot be
// prompted, and one told to retry asks again. A ready whose machine is busy for the whole bound
// takes nothing and is remembered as a launch this process did not answer; one the machine
// accepts ends the claim's run of waived generations (readyAccepted).
func (s *server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.bound(w, r)
	defer cancel()
	var req claim.ReadyRequest
	if !s.readBody(w, r, &req, s.claimSecretCheck) || !requireFields(w,
		field{"claimToken", string(req.ClaimToken)}, field{"sessionId", req.SessionID}, field{"secret", req.Secret},
	) || !requireGeneration(w, req.Generation) {
		return
	}
	m, ok := s.authenticatedWithin(req.ClaimToken, req.Secret)
	if !ok {
		writeJSON(w, claim.InvalidSecret.Status, claim.InvalidSecret)
		return
	}
	if s.refuseIfStaleClaim(w, r, published(m)) {
		return
	}
	decided, decide := s.decisionOn(ctx, req.ClaimToken)
	defer decide()
	err := m.HandleWithin(decided, supervise.RequestReady{
		Claim: req.ClaimToken, Generation: req.Generation, Session: req.SessionID,
	})
	if err != nil {
		if errors.Is(err, supervise.ErrMachineBusy) {
			s.refuseReadyUnanswered(w, r, req.ClaimToken, req.Generation, machineBusyReason)
			return
		}
		if boundSpent(decided, err) {
			s.refuseReadyUnanswered(w, r, req.ClaimToken, req.Generation, boundSpentReason)
			return
		}
		s.claimFailure(w, "ready", req.ClaimToken, err)
		return
	}
	s.readyAccepted(req.ClaimToken, req.Generation)
	c := published(m)
	if req.Capabilities != nil {
		report := s.capabilityReport(c, req)
		if s.capabilityReported != nil {
			s.capabilityReported(decided, c, report)
		}
	}
	if s.claimReady != nil {
		s.claimReady(c)
	}
	w.WriteHeader(http.StatusNoContent)
}

// capabilityReport is the ready's capability report as the daemon keeps it: the rows normalised
// (capabilities.Normalize), the claim's process as the reporter — or a locator naming the claim
// alone, for a claim its machine records no process for — and the generation the ready ran at.
func (s *server) capabilityReport(c supervise.Claim, req claim.ReadyRequest) capabilities.Report {
	kept, dropped := capabilities.Normalize(req.Capabilities.Rows)
	if len(dropped) > 0 {
		s.log.Warn("api: a ready's capability report named rows the table has no live row for",
			"claim", req.ClaimToken, "session", req.SessionID, "names", dropped)
	}
	locator := runtime.Locator{Claim: req.ClaimToken}
	if c.Locator != nil {
		locator = *c.Locator
	}
	return capabilities.Report{
		Claim:      req.ClaimToken,
		Generation: req.Generation,
		Locator:    locator,
		MeasuredAt: req.Capabilities.MeasuredAt,
		ReportedAt: time.Now().UTC(),
		ElapsedMs:  req.Capabilities.ElapsedMs,
		Rows:       kept,
	}
}

// exit is the agent reporting its own end: a worker's claim is released and retires, the tree's
// root claim is suspended until its tree closes, and the reason is logged.
func (s *server) exit(w http.ResponseWriter, r *http.Request) {
	var req claim.ExitRequest
	if !s.readBody(w, r, &req, s.claimSecretCheck) || !requireFields(w,
		field{"claimToken", string(req.ClaimToken)}, field{"sessionId", req.SessionID}, field{"secret", req.Secret},
		field{"reason", req.Reason},
	) || !requireGeneration(w, req.Generation) {
		return
	}
	m, ok := s.authenticated(req.ClaimToken, req.Secret)
	if !ok {
		writeJSON(w, claim.InvalidSecret.Status, claim.InvalidSecret)
		return
	}
	if s.refuseIfStaleClaim(w, r, m.Claim()) {
		return
	}
	ctx, decided := s.decision(r, req.ClaimToken)
	defer decided()
	err := m.Handle(ctx, supervise.RequestExit{
		Claim: req.ClaimToken, Generation: req.Generation, Session: req.SessionID, Reason: req.Reason,
	})
	if err != nil {
		s.claimFailure(w, "exit", req.ClaimToken, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// activity is a live role's session reporting what it is doing, after each of its turns and
// whenever the report's content changes (INFRA-945), so the idle rule can decide whether the
// session's pod may be released. It decides nothing about the claim, so it posts no request to
// the claim's machine: the report is written onto the claim's row alone
// (store.Store.PutClaimActivity, reached through Options.ClaimActivity), where a machine's own
// write neither carries it nor erases it, and where a report older than the one recorded is
// dropped rather than written.
//
// It refuses exactly as its siblings do — a blank field with the field's name, a claim this
// daemon does not hold or a wrong secret with claim.InvalidSecret, a stale plugin with the
// stale-contract 409 — and answers 204, as ready and exit do. A report the daemon cannot write
// is a 500 the session's next one replaces: a claim with no report after its last turn is never
// released, so nothing lost here costs anything but a release.
func (s *server) activity(w http.ResponseWriter, r *http.Request) {
	var req claim.ActivityRequest
	if !s.readBody(w, r, &req, s.claimSecretCheck) || !requireFields(w,
		field{"claimToken", string(req.ClaimToken)}, field{"sessionId", req.SessionID}, field{"secret", req.Secret},
	) {
		return
	}
	if req.Activity.At.IsZero() {
		writeJSON(w, http.StatusBadRequest, errorBody("activity.at is required"))
		return
	}
	m, ok := s.authenticated(req.ClaimToken, req.Secret)
	if !ok {
		writeJSON(w, claim.InvalidSecret.Status, claim.InvalidSecret)
		return
	}
	if s.refuseIfStaleClaim(w, r, m.Claim()) {
		return
	}
	if s.claimActivity == nil {
		writeJSON(w, http.StatusInternalServerError, errorBody("the daemon has no store to record session activity in"))
		return
	}
	known, err := s.claimActivity.PutClaimActivity(r.Context(), req.ClaimToken, req.Activity)
	if err != nil {
		s.logRequestFailure(r, err, "api: a session's activity report was not recorded", "claim", req.ClaimToken,
			"session", req.SessionID)
		writeJSON(w, http.StatusInternalServerError, errorBody("record session activity: "+err.Error()))
		return
	}
	if !known {
		writeJSON(w, claim.InvalidSecret.Status, claim.InvalidSecret)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ReleaseRecord is one release of an issue pod as a reader of the records sees it: the run it
// released, the roles whose claims it suspended, when it completed, and how many bytes of
// unpushed work it saved as a bundle (zero when the workspace held none).
type ReleaseRecord struct {
	ID          int64    `json:"id"`
	Issue       string   `json:"issue"`
	Generation  uint64   `json:"generation"`
	Roles       []string `json:"roles"`
	At          string   `json:"at"`
	BundleBytes int      `json:"bundleBytes"`
}

// ResumeRecord is one wake of a released pod and what it cost: the release it ended (0 when no
// release row is recorded for it), the role the wake was for and what woke it, when the wake
// reached the daemon, when the replacement pod was Ready and when the woken role took its first
// turn — each absent until it is observed — and whether the wake went ten minutes with no turn.
type ResumeRecord struct {
	ID          int64  `json:"id"`
	Release     int64  `json:"release"`
	Issue       string `json:"issue"`
	Role        string `json:"role"`
	Kind        string `json:"kind"`
	At          string `json:"at"`
	PodReadyAt  string `json:"podReadyAt,omitempty"`
	FirstTurnAt string `json:"firstTurnAt,omitempty"`
	Stuck       bool   `json:"stuck"`
}

// HoldRecord is one streak of the idle rule keeping a pod past the idle period, with the rule's
// own name for what held it (workflow.IdleHold's String) and the detail that word leaves out
// (the kinds a background-work hold's session reported), absent where there is none. A streak
// that the rule read again is one record, not one per pass: `firstAt` is its first pass, `at`
// its newest, and `passes` how many it has made.
type HoldRecord struct {
	ID      int64  `json:"id"`
	Issue   string `json:"issue"`
	Reason  string `json:"reason"`
	Detail  string `json:"detail,omitempty"`
	FirstAt string `json:"firstAt"`
	At      string `json:"at"`
	Passes  int64  `json:"passes"`
}

// ReleaseRecordsRequest is the body of POST /legion/v1/claims/release-records: the claim the
// reading pod holds, that claim's secret, and the start of the window it reads as an RFC 3339
// instant.
type ReleaseRecordsRequest struct {
	ClaimToken claim.Token `json:"claimToken"`
	Secret     string      `json:"secret"`
	Since      string      `json:"since"`
}

// ReleaseRecordsResponse is the body POST /legion/v1/claims/release-records answers: every
// release, resume and hold of the project recorded at or after the `since` the caller named,
// oldest first. Each list is never null, so a window with nothing in it reads as three empty
// lists rather than three absent ones.
type ReleaseRecordsResponse struct {
	Releases []ReleaseRecord `json:"releases"`
	Resumes  []ResumeRecord  `json:"resumes"`
	Holds    []HoldRecord    `json:"holds"`
}

// releaseRecords is a pod reading its deployment's idle-release records: what has been released,
// what came back and how long it took, and every pod the rule kept with the reason it read. A pod
// holds its claim credential and LEGION_DAEMON_URL and no operator token, so the route
// authenticates by the claim exactly as ready, exit and activity do. It is a POST because the
// claim's secret is the credential: in a query string it would reach every proxy and access log
// between the pod and the daemon, so it rides in the body as its siblings' does.
//
// The caller is the plugin, not a role: the `legion` tool's read_release_records posts the claim
// token and secret its registration left in the session's process
// (packages/pi-shared/src/legion-phase-ops.ts), so no role handles that credential.
//
// It refuses a blank field by its name with 400, a claim this daemon does not hold and a wrong
// secret with claim.InvalidSecret, and a stale plugin with the stale-contract 409, as its
// siblings do. `since` is required and must be an RFC 3339 instant: a window nobody named is not
// a window, and the whole table is not an answer.
func (s *server) releaseRecords(w http.ResponseWriter, r *http.Request) {
	var req ReleaseRecordsRequest
	if !s.readBody(w, r, &req, s.claimSecretCheck) || !requireFields(w,
		field{"claimToken", string(req.ClaimToken)}, field{"secret", req.Secret}, field{"since", req.Since},
	) {
		return
	}
	token := req.ClaimToken
	from, err := time.Parse(time.RFC3339, req.Since)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("since must be an RFC 3339 time: "+err.Error()))
		return
	}
	m, ok := s.authenticated(token, req.Secret)
	if !ok {
		writeJSON(w, claim.InvalidSecret.Status, claim.InvalidSecret)
		return
	}
	if s.refuseIfStaleClaim(w, r, m.Claim()) {
		return
	}
	if s.releaseRecordStore == nil {
		writeJSON(w, http.StatusInternalServerError, errorBody("the daemon keeps no release records"))
		return
	}
	records, err := s.releaseRecordStore.PodReleaseRecords(r.Context(), s.project, from.UTC())
	if err != nil {
		s.logRequestFailure(r, err, "api: the release records could not be read", "claim", token)
		writeJSON(w, http.StatusInternalServerError, errorBody("read the release records: "+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, releaseRecordsBody(records))
}

// releaseRecordsBody is store.PodReleaseRecords on the wire: every time as RFC 3339 in UTC, an
// unobserved one left out, and every list present.
func releaseRecordsBody(records store.PodReleaseRecords) ReleaseRecordsResponse {
	body := ReleaseRecordsResponse{
		Releases: make([]ReleaseRecord, 0, len(records.Releases)),
		Resumes:  make([]ResumeRecord, 0, len(records.Resumes)),
		Holds:    make([]HoldRecord, 0, len(records.Holds)),
	}
	for _, release := range records.Releases {
		roles := release.Roles
		if roles == nil {
			roles = []string{}
		}
		body.Releases = append(body.Releases, ReleaseRecord{ID: release.ID, Issue: release.Issue,
			Generation: release.Generation, Roles: roles, At: recordedAt(release.At), BundleBytes: release.BundleBytes})
	}
	for _, resume := range records.Resumes {
		body.Resumes = append(body.Resumes, ResumeRecord{ID: resume.ID, Release: resume.Release, Issue: resume.Issue,
			Role: resume.Role, Kind: resume.Kind, At: recordedAt(resume.At), PodReadyAt: recordedAt(resume.PodReadyAt),
			FirstTurnAt: recordedAt(resume.FirstTurnAt), Stuck: resume.Stuck})
	}
	for _, hold := range records.Holds {
		body.Holds = append(body.Holds, HoldRecord{ID: hold.ID, Issue: hold.Issue, Reason: hold.Reason,
			Detail: hold.Detail, FirstAt: recordedAt(hold.FirstAt), At: recordedAt(hold.At), Passes: hold.Passes})
	}
	return body
}

// recordedAt is a recorded moment on the wire: RFC 3339 in UTC, and the empty string for one
// nothing has observed, which the omitempty fields then leave out.
func recordedAt(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339)
}

// authenticated is the claim's machine when secret is the one its registration was issued. A
// claim the daemon does not supervise, one no agent has registered on, and a wrong secret all
// answer the same: nothing about the claim is told to a caller that cannot prove it holds it.
func (s *server) authenticated(token claim.Token, secret string) (*supervise.Machine, bool) {
	return s.authenticatedBy(token, secret, (*supervise.Machine).Claim)
}

// authenticatedWithin is authenticated for a route that answers within a bound: the claim is read
// as its machine last published it, so the compare never waits out the decision in flight
// (published). A secret the decision in flight is about to clear authenticates here exactly as it
// would have a moment earlier, which is what the caller's own request was.
func (s *server) authenticatedWithin(token claim.Token, secret string) (*supervise.Machine, bool) {
	return s.authenticatedBy(token, secret, published)
}

// authenticatedBy is the compare both of them make, over the claim read reads the machine with:
// the two differ in nothing else, so the rule that a caller is told nothing it cannot prove it
// holds is written once.
func (s *server) authenticatedBy(token claim.Token, secret string,
	read func(*supervise.Machine) supervise.Claim) (*supervise.Machine, bool) {
	m, ok := s.supervisor.Machine(token)
	if !ok {
		return nil, false
	}
	if !holdsCapability(read(m), secret) {
		return nil, false
	}
	return m, true
}

// holdsCapability is whether secret is the one c's registration was issued. A claim that holds
// none — none has registered, or a relaunch cleared it — holds no secret at all.
func holdsCapability(c supervise.Claim, secret string) bool {
	held := c.CapabilityHash
	return len(held) > 0 && subtle.ConstantTimeCompare(held, capabilityHash(secret)) == 1
}

// published is the claim as its machine last published it, which a route that answers within a
// bound reads in place of Machine.Claim: Claim waits out the decision in flight, and a decision
// that holds the machine through a runtime call holds it for minutes — the very wait the bound
// exists for (supervise.Machine.View, register, ready).
func published(m *supervise.Machine) supervise.Claim {
	return m.View().Claim
}

// claimFailure answers an agent request its machine did not take: a claim refusal with its own
// status and sentence (the plugin exits on the registration's 403 or 409), a request the claim's
// state does not allow with 409 and the machine's reason, and anything else — the store refusing
// a write — with a 500 that says what failed and nothing the caller could use.
func (s *server) claimFailure(w http.ResponseWriter, request string, token claim.Token, err error) {
	var refusal claim.Refusal
	if errors.As(err, &refusal) {
		s.log.Info("api: refused a claim request", "request", request, "claim", token, "reason", err)
		writeJSON(w, refusal.Status, refusal)
		return
	}
	var refused *supervise.RefusedError
	if errors.As(err, &refused) {
		writeJSON(w, http.StatusConflict, errorBody(refused.Error()))
		return
	}
	s.logFailure("api: a claim request failed", "request", request, "claim", token, "error", err)
	writeJSON(w, http.StatusInternalServerError, errorBody(request+" failed: the daemon could not record it"))
}

// capabilityHash is the one hash a registration's secret is stored and compared by.
func capabilityHash(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// decodeJSON decodes r's body (bounded by maxRequestBytes) into into once: strict disallows a
// member the route does not read, lenient ignores it (the register route, and the lenient
// identity read of an undecodable body). It reports "more than one JSON value" for trailing
// bytes either way. The raw bytes are returned too, so a strict failure can be retried leniently
// for a route family's identity.
func decodeJSON(w http.ResponseWriter, r *http.Request, into any, strict bool) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if strict {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(into); err != nil {
		return body, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return body, errors.New("more than one JSON value")
	}
	return body, nil
}

// refuseBadBody is the 400 every undecodable body gets that names no stale caller: the decoder's
// error, never the body itself (it may carry a secret), logged with the route.
func (s *server) refuseBadBody(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Warn("api: refused an undecodable request body", "route", r.URL.Path, "error", err)
	writeJSON(w, http.StatusBadRequest, errorBody("invalid request body: "+err.Error()))
}

// identityCheck is readBody's lenient fallback for a route family that has a caller to name: it
// reads body's identity fields leniently (unknown fields ignored) and, when they authenticate a
// caller whose recorded contract differs from this daemon's, writes the stale-plugin refusal and
// reports true. A route with no stale identity (register, the operator routes, the
// controller secret and GitHub-credential routes) passes no check: any undecodable body there
// gets refuseBadBody.
type identityCheck func(w http.ResponseWriter, r *http.Request, body []byte) bool

// readBody decodes the request's one JSON object strictly, refusing a member the route does not
// read. On a decode failure it tries check's lenient identity read before refusing: a caller it
// can still name whose claim or controller record speaks another contract is refused with the
// stale-plugin 409 instead of the 400 every other undecodable body gets.
func (s *server) readBody(w http.ResponseWriter, r *http.Request, into any, check identityCheck) bool {
	body, err := decodeJSON(w, r, into, true)
	if err == nil {
		return true
	}
	if check != nil && check(w, r, body) {
		return false
	}
	s.refuseBadBody(w, r, err)
	return false
}

// readRegisterBody decodes the registration body leniently: it is the one body that carries
// the plugin contract the record-and-relaunch path needs, so a bump that changes another field of
// this body still reaches that path rather than refusing the process a 400 it cannot act on.
func (s *server) readRegisterBody(w http.ResponseWriter, r *http.Request, into *claim.RegisterRequest) bool {
	_, err := decodeJSON(w, r, into, false)
	if err != nil {
		s.refuseBadBody(w, r, err)
		return false
	}
	return true
}

// staleClaim reports c's recorded contract and whether the claim is a stale process the daemon is
// relaunching. A claim holds a capability hash only in a live state (registered, ready, working,
// idle — supervise.HoldsCapability): the relaunch that follows a stale registration or a
// boot-time refreshPlugin moves the claim out of those states in the same decision that discovers
// it, and Machine.persist clears the hash the moment it does, so a caller that can still
// authenticate here is necessarily in that window — after the daemon has found it stale (at
// boot, or at the registration that recorded the mismatch) and before its relaunch has taken
// effect. A relaunch the launch path refuses (a tree cleanup reserved, a store error) leaves the
// claim live and stale in that window longer, and the orphan sweep re-posts its relaunch every
// interval until one starts (refreshStalePlugins in internal/daemon), so the sentence this
// refusal carries stays true for as long as the caller can authenticate. A daemon that can
// relaunch nothing (PluginRelaunchable false, LEGION-164's degraded Kubernetes mode) never
// starts that relaunch, so a stale claim there is left running and answered as any other:
// refusing it would stall agents that work today.
//
// Three of its four terms are the rule internal/supervise holds for both readers of it
// (supervise.StaleRefusesCalls): the recorded contract is not this daemon's, the claim is in no
// update round, and a relaunch onto this daemon's plugin can reach the claim at all — which is
// false on a daemon that can relaunch nothing and false for a claim whose launch installs no
// plugin (Options.PluginRelaunchable, Options.NoPluginInstall — a Claude Code role's plugin in
// a pod is its image's own claude-envoy, which no install reaches). Nothing relaunches either
// of those claims for its contract: the stale-plugin lane posts them nothing and their stale
// registrations are accepted and charged nothing (internal/daemon's refreshStalePlugins,
// supervise's staleRegistration), so this refusal's own sentence would be false of them and no
// sweep would ever make it true. Only a new image moves such a worker's plugin, and the update
// round over its pod is what brings one — a round that opens on pod-shape drift alone, so a
// release that changes no image opens none, and a refusal here would hold the worker's grants,
// its handoff_complete and its ready for the rest of the deployment's life. The round term is
// there for the same reason read the other way: the round's whole point is that the worker
// pushes its work before its process is replaced, and it cannot push without the daemon it
// already speaks to, so while the round is open the daemon serves the claim on its previous
// contract — grants, handoff_complete and every other grant-bearing route, as well as the claim
// routes — and the end of the round is what relaunches the process onto the current plugin.
//
// Its fourth term is this route family's own, which the shared rule leaves to it: a claim whose
// worker is in a turn is not stale here. The stale-plugin lane holds the relaunch of a working
// claim until that turn ends (holdStaleRelaunch), precisely so the turn can push what it has
// rather than lose it to a process ended where it stood. Grants are what mint the GitHub App
// token, so refusing them for the length of that turn would take away the one thing the hold
// exists to protect: the turn would run on, unable to push and unable to hand off, and the work
// would go with the relaunch the hold was waiting to start. Working, stale, relaunchable and in
// no round is the held relaunch, the controller in the moment before its relaunch starts, or a
// claim the boot pass has not decided yet; serving the last two for that moment is what degraded
// mode does for every stale claim. The window this refusal describes opens when the relaunch
// starts, not when the daemon decides on it.
//
// supervise's own reader of the shared rule adds no such term, because the one place it is
// asked has answered it already: a pending delivery is handed to no claim that is not ready or
// idle (supervise's staleRefusesClaim, sendPending).
func (s *server) staleClaim(c supervise.Claim) (int, bool) {
	return c.PluginContract, supervise.StaleRefusesCalls(c, DaemonAPIVersion, s.pluginRelaunchable, s.noPluginInstall) &&
		c.State != supervise.StateWorking
}

// logStaleRefusal is the one log line every stale-caller refusal writes: never the body,
// never a secret.
func (s *server) logStaleRefusal(r *http.Request, identityKey, identityValue string, got int) {
	s.log.Warn("api: refused a call from a process on another plugin contract",
		"route", r.URL.Path, identityKey, identityValue, "pluginContract", got, "daemonApiVersion", DaemonAPIVersion)
}

// refuseIfStaleClaim is the claim-route refusal shape (ready, exit): errorBody naming both
// contracts, by way of claim.Refusal, which a claim route already answers with on any other
// refusal.
func (s *server) refuseIfStaleClaim(w http.ResponseWriter, r *http.Request, c supervise.Claim) bool {
	got, stale := s.staleClaim(c)
	if !stale {
		return false
	}
	s.logStaleRefusal(r, "claim", string(c.Token), got)
	refusal := claim.StalePluginContract(got, DaemonAPIVersion)
	writeJSON(w, refusal.Status, refusal)
	return true
}

// refuseIfStaleClaimFailure is the credential- and workflow-route refusal shape (the grants
// route's claim form, every grant-bearing route): Failure{Code: "STALE_PLUGIN_CONTRACT"}.
func (s *server) refuseIfStaleClaimFailure(w http.ResponseWriter, r *http.Request, c supervise.Claim) bool {
	got, stale := s.staleClaim(c)
	if !stale {
		return false
	}
	s.logStaleRefusal(r, "claim", string(c.Token), got)
	writeFailure(w, http.StatusConflict, "STALE_PLUGIN_CONTRACT", claim.StalePluginContract(got, DaemonAPIVersion).Message)
	return true
}

// claimSecretCheck is readBody's identity check for ready and exit: {claimToken, secret},
// authenticated exactly as authenticated does.
func (s *server) claimSecretCheck(w http.ResponseWriter, r *http.Request, body []byte) bool {
	var id struct {
		ClaimToken claim.Token `json:"claimToken"`
		Secret     string      `json:"secret"`
	}
	if json.Unmarshal(body, &id) != nil || id.ClaimToken == "" || id.Secret == "" {
		return false
	}
	m, ok := s.authenticated(id.ClaimToken, id.Secret)
	if !ok {
		return false
	}
	return s.refuseIfStaleClaim(w, r, m.Claim())
}

type field struct{ name, value string }

// requireFields refuses the first field that is blank, by its wire name, with the sentence alone:
// the claim and operator routes' refusal shape (the credential and workflow routes add a code,
// requireFailureFields).
func requireFields(w http.ResponseWriter, fields ...field) bool {
	for _, f := range fields {
		if strings.TrimSpace(f.value) == "" {
			writeJSON(w, http.StatusBadRequest, errorBody(f.name+" is required"))
			return false
		}
	}
	return true
}

// requireGeneration refuses a request whose launch generation is zero, the one count from 1 the
// claim and operator routes read: a request that names no generation is fenced against nothing.
func requireGeneration(w http.ResponseWriter, generation uint64) bool {
	if generation == 0 {
		writeJSON(w, http.StatusBadRequest, errorBody("generation is required"))
		return false
	}
	return true
}

func errorBody(message string) map[string]string { return map[string]string{"error": message} }
