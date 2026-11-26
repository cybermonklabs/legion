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
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
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
func (s *server) register(w http.ResponseWriter, r *http.Request) {
	var req claim.RegisterRequest
	if !s.readRegisterBody(w, r, &req) || !s.requireFields(w, r,
		field{"bootToken", req.BootToken}, field{"sessionId", req.SessionID},
		field{"ompSessionFile", req.OmpSessionFile}, field{"agentId", req.AgentID},
	) {
		return
	}
	launch, known, err := s.bootTokens.Resolve(r.Context(), req.BootToken)
	if err != nil {
		s.logRequestFailure(r, err, "api: resolve a registration's boot token")
		s.refuse(w, r, http.StatusInternalServerError, "register failed: the daemon could not resolve the boot token")
		return
	}
	if !known {
		if s.controllerLaunched {
			s.writeAnswer(w, r, AnswerRefusal, claim.InvalidBootToken.Status, claim.InvalidBootToken)
			return
		}
		s.registerController(w, r, req)
		return
	}
	m, supervised := s.supervisor.Machine(launch.Claim)
	if !supervised {
		s.writeAnswer(w, r, AnswerRefusal, claim.InvalidBootToken.Status, claim.InvalidBootToken)
		return
	}
	if m.Claim().Role == claim.RoleController {
		if !s.controllerLaunched {
			s.log.Warn("api: refused a launched controller's registration: this daemon leaves the controller to its operator",
				"claim", launch.Claim, "generation", launch.Generation, "session", req.SessionID)
			s.refuse(w, r, http.StatusConflict, fmt.Sprintf(
				"%s is a launch of the daemon's own controller, and this daemon leaves the controller to its operator (controller: operator)", launch.Claim))
			return
		}
		s.registerLaunchedController(w, r, req, launch, m)
		return
	}
	secret := rand.Text()
	// The decision is the machine's and runs to its end, or to the daemon's stop: a caller that hangs
	// up mid-request does not get to leave a registration half-recorded.
	ctx, decided := s.decision(r, launch.Claim)
	defer decided()
	err = m.Handle(ctx, supervise.RequestRegister{
		Claim:          launch.Claim,
		Generation:     launch.Generation,
		Session:        req.SessionID,
		SessionFile:    req.OmpSessionFile,
		CapabilityHash: capabilityHash(secret),
		PluginContract: req.PluginContract,
	})
	if err != nil {
		s.claimFailure(w, r, "register", launch.Claim, err)
		return
	}
	c := m.Claim()
	s.log.Info("api: claim registered", "claim", c.Token, "generation", launch.Generation,
		"session", req.SessionID, "agent", req.AgentID, "pluginContract", req.PluginContract)
	s.writeAnswer(w, r, AnswerRegister, http.StatusOK, claim.RegisterResponse{
		ClaimToken:   c.Token,
		Tree:         c.Tree,
		Issue:        c.Issue,
		Role:         c.Role,
		Generation:   launch.Generation,
		Secret:       secret,
		PromptAgents: s.promptAgentsList(),
	})
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
// A ready that carries no report — a boot's, or a role-regain's — first queues the publication of
// the issue's role sessions to Dispatch (Options.RoleHoldersChanged), before the machine takes the
// ready and sends the first task; the daemon queues none for an issue of another project, whose
// rows only that project's daemon runs. A queue the daemon could not write refuses the ready with
// 503, so no task goes before the publication is queued, and the plugin tries the ready again.
func (s *server) ready(w http.ResponseWriter, r *http.Request) {
	var req claim.ReadyRequest
	if !s.readBody(w, r, &req, s.claimSecretCheck) || !s.requireFields(w, r,
		field{"claimToken", string(req.ClaimToken)}, field{"sessionId", req.SessionID}, field{"secret", req.Secret},
	) || !s.requireGeneration(w, r, req.Generation) {
		return
	}
	m, _, ok := s.authenticated(req.ClaimToken, req.Secret)
	if !ok {
		s.writeAnswer(w, r, AnswerRefusal, claim.InvalidSecret.Status, claim.InvalidSecret)
		return
	}
	if s.refuseIfStaleClaim(w, r, m.Claim()) {
		return
	}
	ctx, decided := s.decision(r, req.ClaimToken)
	defer decided()
	if req.Capabilities == nil && s.roleHoldersChanged != nil {
		if c := m.Claim(); c.Issue != "" && c.Role != claim.RoleController {
			if err := s.roleHoldersChanged(ctx, c); err != nil {
				s.log.Warn("api: refused a ready: the daemon could not queue the publication of its issue's role sessions",
					"claim", c.Token, "issue", c.Issue, "error", err)
				s.refuse(w, r, http.StatusServiceUnavailable, "the daemon could not queue the role publication; try the ready again")
				return
			}
		}
	}
	err := m.Handle(ctx, supervise.RequestReady{
		Claim: req.ClaimToken, Generation: req.Generation, Session: req.SessionID,
	})
	if err != nil {
		s.claimFailure(w, r, "ready", req.ClaimToken, err)
		return
	}
	c := m.Claim()
	if req.Capabilities != nil {
		report := s.capabilityReport(c, req)
		if s.capabilityReported != nil {
			s.capabilityReported(ctx, c, report)
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
	if !s.readBody(w, r, &req, s.claimSecretCheck) || !s.requireFields(w, r,
		field{"claimToken", string(req.ClaimToken)}, field{"sessionId", req.SessionID}, field{"secret", req.Secret},
		field{"reason", req.Reason},
	) || !s.requireGeneration(w, r, req.Generation) {
		return
	}
	m, _, ok := s.authenticated(req.ClaimToken, req.Secret)
	if !ok {
		s.writeAnswer(w, r, AnswerRefusal, claim.InvalidSecret.Status, claim.InvalidSecret)
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
		s.claimFailure(w, r, "exit", req.ClaimToken, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// turnOutcome is a producer reporting how one turn of its agent ended, clean or refused by the
// provider, with its own sequence within the launch generation. The daemon keeps the report and
// derives the claim's run of refused turns from the reports it holds, which fails no claim. A turn
// that ran no prompt of the daemon's is judged from its report, as a prompt's turn is from its
// result (supervise's turnOutcome): a refusal whose status cannot clear fails the claim at once,
// and so does one the producer classed as OpenAI's cyber-policy refusal (`code`), whatever its
// status. The answer says what the run now is and whether this report failed the claim.
//
// The caller is the claim's, proved one of two ways. An Oh My Pi session's pi-legion holds the
// registration's secret and names the claim with it, as the ready and the exit do. The Claude
// bridge holds no secret — its pane's registration is claude-envoy's, inside Claude Code, and the
// secret never leaves that process — so it presents the launch's own boot token, the 0600 file
// its pane is told, which the daemon resolves to that launch's claim exactly as a registration's
// is resolved. A body carrying both, or neither, is refused: each says who is calling, and a
// caller that cannot say is not answered.
func (s *server) turnOutcome(w http.ResponseWriter, r *http.Request) {
	var req claim.TurnOutcomeRequest
	if !s.readBody(w, r, &req, s.claimSecretCheck) || !s.requireGeneration(w, r, req.Generation) {
		return
	}
	if req.Sequence == 0 {
		// Zero is how a body that named no sequence decodes, and every turn of such a producer
		// would collide on one key and be counted once, so it is refused rather than recorded.
		s.refuse(w, r, http.StatusBadRequest, "sequence is required, counted from 1 within the generation")
		return
	}
	if req.Outcome != claim.TurnClean && req.Outcome != claim.TurnRefused {
		s.refuse(w, r, http.StatusBadRequest, fmt.Sprintf("outcome %q is neither %q nor %q", req.Outcome, claim.TurnClean, claim.TurnRefused))
		return
	}
	token, m, ok := s.reportCaller(w, r, turnOutcomeReport, req.AgentCaller, req.BootCaller, req.Generation)
	if !ok {
		return
	}
	status := req.HTTPStatus
	if status != 0 && (status < 100 || status > 599) {
		// A number outside the HTTP range is no status a provider answered, and the status decides
		// whether the claim fails (supervise's classifyProviderError), so the report is taken as
		// one whose status cannot be read, which fails nothing. The rollout is warn-first: the
		// report is not refused (the refusal would be `400 httpStatus <n> is no HTTP status`), so
		// a producer that sends such a number still has its turn counted, and the warning names it.
		s.log.Warn("api: a turn outcome named an HTTP status outside 100 to 599; it is taken as no status",
			"claim", token, "generation", req.Generation, "sequence", req.Sequence, "httpStatus", status)
		status = 0
	}
	code := req.Code
	if code != "" && code != claim.TurnCodeCyberPolicy {
		// The daemon reads one class off a report (claim.TurnOutcomeRequest.Code), and the class
		// decides whether the claim fails, so a word it does not know is taken as none, which
		// changes nothing the status decides. Warn-first, as a status outside the HTTP range is:
		// the report is not refused, so its turn is still counted, and the warning names the word.
		s.log.Warn("api: a turn outcome named a code the daemon does not read; it is taken as no code",
			"claim", token, "generation", req.Generation, "sequence", req.Sequence, "code", code)
		code = ""
	}
	ctx, decided := s.decision(r, token)
	defer decided()
	// The answer is read under the report's own decision (supervise's HandleTurnOutcome), so
	// `failed` is true only for the report that failed the claim (claim.TurnOutcomeResponse).
	answer, err := m.HandleTurnOutcome(ctx, supervise.RequestTurnOutcome{
		Claim: token, Generation: req.Generation, Sequence: req.Sequence,
		Refused: req.Outcome == claim.TurnRefused, Message: req.Message,
		HTTPStatus: status, Unprompted: req.Unprompted, Code: code,
	})
	if err != nil {
		s.claimFailure(w, r, "turn outcome", token, err)
		return
	}
	s.writeAnswer(w, r, AnswerTurnOutcome, http.StatusOK, claim.TurnOutcomeResponse{RefusedRun: answer.RefusedRun, Failed: answer.Failed})
}

// claimReport names one of the two reports a claim's host sends under the claim's own identity,
// a turn outcome or a report of model requests, as each refusal and line of the shared caller
// check (reportCaller) names it.
type claimReport string

const (
	turnOutcomeReport   claimReport = "turn outcome"
	modelRequestsReport claimReport = "report of model requests"
)

// reportCaller is the claim a report is sent for and its machine: the claim the body names,
// authenticated by the registration's secret, or the claim the launch's boot token resolves to
// (claim.TurnOutcomeRequest's and claim.ModelRequestsRequest's two caller types). It writes the
// refusal itself when the body names neither identity, both, or one the daemon does not
// recognise.
func (s *server) reportCaller(w http.ResponseWriter, r *http.Request, report claimReport, agent claim.AgentCaller, boot claim.BootCaller, generation uint64) (claim.Token, *supervise.Machine, bool) {
	if caller, named := (claim.TurnOutcomeRequest{AgentCaller: agent, BootCaller: boot}).Caller(); named {
		switch caller := caller.(type) {
		case claim.BootCaller:
			return s.reportByBootToken(w, r, report, caller, generation)
		case claim.AgentCaller:
			return s.reportBySecret(w, r, caller)
		}
	}
	// The body named neither caller, or both — and, were a third ever added to claim without
	// this route, one this route cannot act on, which is the same answer.
	s.refuse(w, r, http.StatusBadRequest, fmt.Sprintf("a %s names its caller with claimToken, sessionId and secret, or with bootToken; give one", report))
	return "", nil, false
}

// reportByBootToken is the claim of the launch a boot token was minted for, held to the same two
// rules the shim's hello on that token is held to (stream's Listener.hello): a token of a launch
// the claim has moved past is refused, and so is a report for any generation but that launch's. A
// pane left running from a replaced launch — one the daemon could not stop, one whose container
// outlived its stop — would otherwise add its provider's refusals to the run the daemon counts for
// the pane that replaced it, and its unprompted refusals to that claim's run of provider-error
// turns, which a refusal that cannot clear fails the claim on (supervise's turnOutcome), or its
// model requests to the launch it is not.
func (s *server) reportByBootToken(w http.ResponseWriter, r *http.Request, report claimReport, caller claim.BootCaller, generation uint64) (claim.Token, *supervise.Machine, bool) {
	launch, known, err := s.bootTokens.Resolve(r.Context(), caller.BootToken)
	if err != nil {
		s.log.Error("api: resolve a "+string(report)+"'s boot token", "error", err)
		s.refuse(w, r, http.StatusInternalServerError, string(report)+" failed: the daemon could not resolve the boot token")
		return "", nil, false
	}
	m, supervised := s.supervisor.Machine(launch.Claim)
	if !known || launch.Stale || !supervised {
		s.writeAnswer(w, r, AnswerRefusal, claim.InvalidBootToken.Status, claim.InvalidBootToken)
		return "", nil, false
	}
	if generation != launch.Generation {
		s.log.Warn("api: refused a "+string(report)+" reported for another generation", "claim", launch.Claim,
			"launch", launch.Generation, "reported", generation)
		s.writeAnswer(w, r, AnswerRefusal, claim.StaleGeneration.Status, claim.StaleGeneration)
		return "", nil, false
	}
	return launch.Claim, m, true
}

// reportBySecret is the claim the body names, authenticated by the secret its registration was
// issued, as the ready and the exit are.
func (s *server) reportBySecret(w http.ResponseWriter, r *http.Request, caller claim.AgentCaller) (claim.Token, *supervise.Machine, bool) {
	if !s.requireFields(w, r,
		field{"claimToken", string(caller.ClaimToken)}, field{"sessionId", caller.SessionID}, field{"secret", caller.Secret},
	) {
		return "", nil, false
	}
	m, _, ok := s.authenticated(caller.ClaimToken, caller.Secret)
	if !ok {
		s.writeAnswer(w, r, AnswerRefusal, claim.InvalidSecret.Status, claim.InvalidSecret)
		return "", nil, false
	}
	if s.refuseIfStaleClaim(w, r, m.Claim()) {
		return "", nil, false
	}
	return caller.ClaimToken, m, true
}

// modelRequests is a host's report of the model requests of the sessions it runs for one launch of
// a claim (claim.ModelRequestsRequest): the role's own session, each subagent's it starts, or the
// controller's. The daemon keeps each request once, by the claim, the launch generation and the
// request's own id, in the minute it ended, for the provider's headroom and refusals, and answers
// the ids of the batch it now holds, new or held already, so the host sends the rest again under
// the same ids.
//
// The body is decoded strictly, so a member the request does not name — a prompt, an answer, an
// error's text, the cache reads, a project — is refused with the whole body: the project is the
// claim's. The caller is authenticated exactly as a turn outcome's is (reportCaller): the Oh My Pi
// plugin by the claim's secret, the Claude Code bridge by its launch's boot token. The controller
// the daemon launches (`controller: daemon`) is a claim like any role's; the operator's
// (`controller: operator`) holds no claim the daemon supervises, and names its own token,
// claim.ControllerToken, with the session and the secret its registration was issued, which are
// checked against the controller record as the controller's grants are (modelRequestsByController).
// The generation is the authenticated launch's: a body naming another is refused as stale.
//
// The batch is one fact (intake.ModelRequests) under an id that names the claim, the generation
// and the batch's ids, so a batch sent again is a duplicate that keeps nothing twice; a batch
// carrying an id already held keeps that id's first report. The fact carries the batch's word on
// whether its host reports each 429 its transport retries, false where the body says nothing of
// it, as a host of a contract before the member's says nothing.
func (s *server) modelRequests(w http.ResponseWriter, r *http.Request) {
	var req claim.ModelRequestsRequest
	if !s.readBody(w, r, &req, s.claimSecretCheck) || !s.requireGeneration(w, r, req.Generation) {
		return
	}
	if problem := modelRequestsProblem(req.Requests); problem != "" {
		s.refuse(w, r, http.StatusBadRequest, problem)
		return
	}
	token, generation, ok := s.modelRequestsCaller(w, r, req)
	if !ok {
		return
	}
	if s.pool == nil || s.records == nil {
		s.writeFailure(w, r, http.StatusInternalServerError, "FACTS_UNAVAILABLE", "fact intake is unavailable")
		return
	}
	ids := make([]string, len(req.Requests))
	for i, request := range req.Requests {
		ids[i] = request.ID
	}
	result, err := s.apply(r.Context(), "api", modelRequestsFactID(token, generation, ids),
		intake.ModelRequests{Claim: token, Generation: generation, Requests: req.Requests, RetriedRefusalsReported: req.RetriedRefusalsReported})
	if err != nil {
		s.logFailure("api: keep a report of model requests", "claim", token, "generation", generation, "error", err)
		s.refuse(w, r, http.StatusInternalServerError, "report of model requests failed: the daemon could not keep it")
		return
	}
	if result.Refusal != nil {
		s.writeRefusal(w, r, result.Refusal)
		return
	}
	// The fact's strings are kept as the store keeps text (storetext.Text, which intake.ApplyFact
	// applies to every fact), so the ids are read back in that form and answered in the host's.
	stored := make([]string, len(ids))
	for i, id := range ids {
		stored[i] = storetext.Text(id)
	}
	var held []string
	if err := store.TransactWith(r.Context(), s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		held, err = s.records.ModelRequestsHeld(ctx, tx, s.dispatchProject, token, generation, stored)
		return err
	}); err != nil {
		s.logFailure("api: read the model requests a report kept", "claim", token, "generation", generation, "error", err)
		s.refuse(w, r, http.StatusInternalServerError, "report of model requests failed: the daemon could not read what it kept")
		return
	}
	kept := make([]string, 0, len(ids))
	for i, id := range ids {
		if slices.Contains(held, stored[i]) {
			kept = append(kept, id)
		}
	}
	s.writeAnswer(w, r, AnswerModelRequests, http.StatusOK, claim.ModelRequestsResponse{Kept: kept})
}

// modelRequestsCaller is the claim and launch generation a report of model requests is kept
// under: the operator's controller by its controller record (modelRequestsByController), every
// other caller as a turn outcome's (reportCaller), whose generation is its launch's. It writes the
// refusal itself where there is none.
func (s *server) modelRequestsCaller(w http.ResponseWriter, r *http.Request, req claim.ModelRequestsRequest) (claim.Token, uint64, bool) {
	if caller, named := req.Caller(); named {
		if agent, ok := caller.(claim.AgentCaller); ok && agent.ClaimToken == claim.ControllerToken(s.project) && s.controller != nil {
			if _, supervised := s.supervisor.Machine(agent.ClaimToken); !supervised {
				return s.modelRequestsByController(w, r, agent, req.Generation)
			}
		}
	}
	token, m, ok := s.reportCaller(w, r, modelRequestsReport, req.AgentCaller, req.BootCaller, req.Generation)
	if !ok {
		return "", 0, false
	}
	if generation := m.Claim().Generation; generation != req.Generation {
		s.log.Warn("api: refused a report of model requests reported for another generation", "claim", token,
			"launch", generation, "reported", req.Generation)
		s.writeAnswer(w, r, AnswerRefusal, claim.StaleGeneration.Status, claim.StaleGeneration)
		return "", 0, false
	}
	return token, req.Generation, true
}

// modelRequestsByController authenticates the operator's controller (`legion controller start`),
// which the daemon supervises no claim of, by the one check of a controller session against its
// record (controllerSessionRegistered) that its grants are minted by, and holds it to the record's
// contract (refuseIfStaleController); its generation is the record's, which its registration
// answered it.
func (s *server) modelRequestsByController(w http.ResponseWriter, r *http.Request, caller claim.AgentCaller, generation uint64) (claim.Token, uint64, bool) {
	if !s.requireFields(w, r, field{"sessionId", caller.SessionID}, field{"secret", caller.Secret}) {
		return "", 0, false
	}
	stored, found, err := s.controller.Controller(r.Context(), s.project)
	if err != nil {
		s.logRequestFailure(r, err, "api: read the controller record for a report of model requests")
		s.refuse(w, r, http.StatusInternalServerError, "report of model requests failed: the daemon could not read its controller record")
		return "", 0, false
	}
	if !found || !controllerSessionRegistered(stored, caller.SessionID, caller.Secret) {
		s.writeAnswer(w, r, AnswerRefusal, claim.InvalidSecret.Status, claim.InvalidSecret)
		return "", 0, false
	}
	if stored.PluginContract != DaemonAPIVersion() {
		s.logStaleRefusal(r, "session", stored.Session, stored.PluginContract)
		refusal := claim.StaleControllerContract(stored.PluginContract, DaemonAPIVersion())
		s.writeAnswer(w, r, AnswerRefusal, refusal.Status, refusal)
		return "", 0, false
	}
	if generation != stored.Generation {
		s.writeAnswer(w, r, AnswerRefusal, claim.StaleGeneration.Status, claim.StaleGeneration)
		return "", 0, false
	}
	return caller.ClaimToken, generation, true
}

// modelRequestsProblem is why a batch of model requests cannot be read as one, "" where it can:
// it holds 1 to claim.MaxModelRequests requests, each with an id no other request of it has, a
// session, a model and the moment it ended, its uncached input tokens absent or at least 0, and
// its refusal absent or one of the three classes.
func modelRequestsProblem(requests []claim.ModelRequest) string {
	if len(requests) == 0 || len(requests) > claim.MaxModelRequests {
		return fmt.Sprintf("requests holds %d requests; a report carries 1 to %d", len(requests), claim.MaxModelRequests)
	}
	seen := make(map[string]int, len(requests))
	for i, request := range requests {
		for _, f := range []field{{"id", request.ID}, {"session", request.Session}, {"model", request.Model}} {
			if strings.TrimSpace(f.value) == "" {
				return fmt.Sprintf("requests[%d].%s is required", i, f.name)
			}
		}
		if first, twice := seen[request.ID]; twice {
			return fmt.Sprintf("requests[%d].id %q is requests[%d]'s too: an id names one request", i, request.ID, first)
		}
		seen[request.ID] = i
		if request.EndedAt.IsZero() {
			return fmt.Sprintf("requests[%d].endedAt is required", i)
		}
		if request.UncachedInputTokens != nil && *request.UncachedInputTokens < 0 {
			return fmt.Sprintf("requests[%d].uncachedInputTokens is %d; it counts tokens from 0", i, *request.UncachedInputTokens)
		}
		switch request.Refusal {
		case "", claim.RefusalOwn, claim.RefusalProvider, claim.RefusalUnknown:
		default:
			return fmt.Sprintf("requests[%d].refusal %q is none of %q, %q and %q", i, request.Refusal, claim.RefusalOwn, claim.RefusalProvider, claim.RefusalUnknown)
		}
	}
	return ""
}

// modelRequestsFactID names a batch's fact: the claim, the launch generation and a digest of the
// batch's ids in order of the ids, so the same batch sent again is the fact already applied and a
// batch of other ids is a fact of its own.
func modelRequestsFactID(token claim.Token, generation uint64, ids []string) string {
	sorted := slices.Sorted(slices.Values(ids))
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return fmt.Sprintf("model-requests:%s:%d:%x", token, generation, sum)
}

// activity is a live role's session reporting what it is doing, after each of its turns and
// whenever the report's content changes (INFRA-945), so the idle rule can decide whether the
// session's pod may be released. It decides nothing about the claim, so it posts no request to
// the claim's machine: the report is written onto the claim's row alone
// (store.Store.PutClaimActivity, reached through Options.ClaimActivity), where a machine's own
// write never carries it and a launch's write clears it, and where a report older than the one
// recorded is dropped rather than written. It is written onto the launch whose secret
// authenticated it — the generation read with the capability check — and nowhere else.
//
// It refuses exactly as its siblings do — a blank field with the field's name, a claim this
// daemon does not hold or a wrong secret with claim.InvalidSecret, a stale plugin with the
// stale-contract 409 — and answers 204, as ready and exit do. A relaunch can move the claim on
// after the check, while the report is on its way to the store, and what the report is answered
// then depends on when the store's statement began (store.Store.PutClaimActivity). One that began
// after the launch's write committed finds no row at its launch and is refused with
// claim.InvalidSecret too, since its secret was that launch's. One whose update waited behind the
// row lock that write held writes nothing, since the update checks the generation again against
// the row the launch committed, and answers 204, since the store answers from the row as the
// statement found it. Neither is written onto the new launch. A report the daemon cannot write is
// a 500 the session's next one replaces: a claim with no report after its last turn is never
// released, so nothing lost here costs anything but a release.
func (s *server) activity(w http.ResponseWriter, r *http.Request) {
	var req claim.ActivityRequest
	if !s.readBody(w, r, &req, s.claimSecretCheck) || !s.requireFields(w, r,
		field{"claimToken", string(req.ClaimToken)}, field{"sessionId", req.SessionID}, field{"secret", req.Secret},
	) {
		return
	}
	if req.Activity.At.IsZero() {
		s.refuse(w, r, http.StatusBadRequest, "activity.at is required")
		return
	}
	m, held, ok := s.authenticated(req.ClaimToken, req.Secret)
	if !ok {
		s.writeAnswer(w, r, AnswerRefusal, claim.InvalidSecret.Status, claim.InvalidSecret)
		return
	}
	if s.refuseIfStaleClaim(w, r, m.Claim()) {
		return
	}
	if s.claimActivity == nil {
		s.refuse(w, r, http.StatusInternalServerError, "the daemon has no store to record session activity in")
		return
	}
	known, err := s.claimActivity.PutClaimActivity(r.Context(), req.ClaimToken, held.Generation, req.Activity)
	if err != nil {
		s.logRequestFailure(r, err, "api: a session's activity report was not recorded", "claim", req.ClaimToken,
			"session", req.SessionID)
		s.refuse(w, r, http.StatusInternalServerError, "record session activity: "+err.Error())
		return
	}
	if !known {
		s.writeAnswer(w, r, AnswerRefusal, claim.InvalidSecret.Status, claim.InvalidSecret)
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
// Pause is the project pause that held the wake (store.PodResume.Pause), absent for a wake no
// pause held: a held wake pairs with no release, and its wait to the first turn measures the
// pause, so a reader of the release measurements counts it apart (decision 15 of
// dispatch://INFRA-3759).
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
	Pause       int64  `json:"pause,omitempty"`
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
//
// placedByPod places a caller that states no contract by its pod on the records: a resume's
// `pause` leaves the body below pauseContract (shapeAnswer), and a plugin below
// shapedAnswersContract states no contract, so its pod is what places it.
func (s *server) releaseRecords(w http.ResponseWriter, r *http.Request) {
	var req ReleaseRecordsRequest
	if !s.readBody(w, r, &req, s.claimSecretCheck) || !s.requireFields(w, r,
		field{"claimToken", string(req.ClaimToken)}, field{"secret", req.Secret}, field{"since", req.Since},
	) {
		return
	}
	token := req.ClaimToken
	from, err := time.Parse(time.RFC3339, req.Since)
	if err != nil {
		s.refuse(w, r, http.StatusBadRequest, "since must be an RFC 3339 time: "+err.Error())
		return
	}
	m, _, ok := s.authenticated(token, req.Secret)
	if !ok {
		s.writeAnswer(w, r, AnswerRefusal, claim.InvalidSecret.Status, claim.InvalidSecret)
		return
	}
	if s.refuseIfStaleClaim(w, r, m.Claim()) {
		return
	}
	if s.releaseRecordStore == nil {
		s.refuse(w, r, http.StatusInternalServerError, "the daemon keeps no release records")
		return
	}
	records, err := s.releaseRecordStore.PodReleaseRecords(r.Context(), s.project, from.UTC())
	if err != nil {
		s.logRequestFailure(r, err, "api: the release records could not be read", "claim", token)
		s.refuse(w, r, http.StatusInternalServerError, "read the release records: "+err.Error())
		return
	}
	s.writeAnswer(w, r, AnswerReleaseRecords, http.StatusOK, releaseRecordsBody(records))
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
			FirstTurnAt: recordedAt(resume.FirstTurnAt), Stuck: resume.Stuck, Pause: resume.Pause})
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

// authenticated is the claim's machine, with the claim as it stood when secret was checked
// against it, when secret is the one its registration was issued. A claim the daemon does not
// supervise, one no agent has registered on, and a wrong secret all answer the same: nothing
// about the claim is told to a caller that cannot prove it holds it.
func (s *server) authenticated(token claim.Token, secret string) (*supervise.Machine, supervise.Claim, bool) {
	m, ok := s.supervisor.Machine(token)
	if !ok {
		return nil, supervise.Claim{}, false
	}
	held := m.Claim()
	if len(held.CapabilityHash) == 0 || subtle.ConstantTimeCompare(held.CapabilityHash, capabilityHash(secret)) != 1 {
		return nil, supervise.Claim{}, false
	}
	return m, held, true
}

// claimFailure answers an agent request its machine did not take: a claim refusal with its own
// status and sentence (the plugin exits on the registration's 403 or 409), a request the claim's
// state does not allow with 409 and the machine's reason, and anything else — the store refusing
// a write — with a 500 that says what failed and nothing the caller could use.
func (s *server) claimFailure(w http.ResponseWriter, r *http.Request, request string, token claim.Token, err error) {
	var refusal claim.Refusal
	if errors.As(err, &refusal) {
		s.log.Info("api: refused a claim request", "request", request, "claim", token, "reason", err)
		s.writeAnswer(w, r, AnswerRefusal, refusal.Status, refusal)
		return
	}
	var refused *supervise.RefusedError
	if errors.As(err, &refused) {
		s.refuse(w, r, http.StatusConflict, refused.Error())
		return
	}
	s.logFailure("api: a claim request failed", "request", request, "claim", token, "error", err)
	s.refuse(w, r, http.StatusInternalServerError, request+" failed: the daemon could not record it")
}

// capabilityHash is the one hash a registration's secret is stored and compared by.
func capabilityHash(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// decodeJSON decodes r's body (bounded by maxRequestBytes) into into once: strict disallows a
// member the route does not read, lenient ignores it (the register route, and the lenient
// identity read of an undecodable body). It reports "more than one JSON value" for trailing
// bytes either way, and a string anywhere in the body that holds U+0000, naming where it stands
// (unstorableJSON): PostgreSQL's text and jsonb cannot hold that character, so the store would
// keep U+FFFD in its place, which the caller would never see. The raw bytes are returned too, so
// a strict failure can be retried leniently for a route family's identity.
func decodeJSON(w http.ResponseWriter, r *http.Request, into any, strict bool) ([]byte, error) {
	if requestDecoded != nil {
		requestDecoded(r.Pattern, reflect.TypeOf(into).Elem())
	}
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
	return body, unstorableJSON(body)
}

// refuseBadBody is the 400 every undecodable body gets that names no stale caller: the decoder's
// error, never the body itself (it may carry a secret), logged with the route.
func (s *server) refuseBadBody(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Warn("api: refused an undecodable request body", "route", r.URL.Path, "error", err)
	s.refuse(w, r, http.StatusBadRequest, "invalid request body: "+err.Error())
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
	return c.PluginContract, supervise.StaleRefusesCalls(c, DaemonAPIVersion(), s.pluginRelaunchable, s.noPluginInstall) &&
		c.State != supervise.StateWorking
}

// logStaleRefusal is the one log line every stale-caller refusal writes: never the body,
// never a secret.
func (s *server) logStaleRefusal(r *http.Request, identityKey, identityValue string, got int) {
	s.log.Warn("api: refused a call from a process on another plugin contract",
		"route", r.URL.Path, identityKey, identityValue, "pluginContract", got, "daemonApiVersion", DaemonAPIVersion())
}

// refuseIfStaleClaim is the claim-route refusal shape (ready, exit): the sentence-only refusal
// naming both contracts, by way of claim.Refusal, which a claim route already answers with on any
// other refusal.
func (s *server) refuseIfStaleClaim(w http.ResponseWriter, r *http.Request, c supervise.Claim) bool {
	got, stale := s.staleClaim(c)
	if !stale {
		return false
	}
	s.logStaleRefusal(r, "claim", string(c.Token), got)
	refusal := claim.StalePluginContract(got, DaemonAPIVersion())
	s.writeAnswer(w, r, AnswerRefusal, refusal.Status, refusal)
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
	s.writeFailure(w, r, http.StatusConflict, "STALE_PLUGIN_CONTRACT", claim.StalePluginContract(got, DaemonAPIVersion()).Message)
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
	m, _, ok := s.authenticated(id.ClaimToken, id.Secret)
	if !ok {
		return false
	}
	return s.refuseIfStaleClaim(w, r, m.Claim())
}

type field struct{ name, value string }

// requireFields refuses the first field that is blank, by its wire name, with the sentence alone:
// the claim and operator routes' refusal shape (the credential and workflow routes add a code,
// requireFailureFields).
func (s *server) requireFields(w http.ResponseWriter, r *http.Request, fields ...field) bool {
	for _, f := range fields {
		if strings.TrimSpace(f.value) == "" {
			s.refuse(w, r, http.StatusBadRequest, f.name+" is required")
			return false
		}
	}
	return true
}

// requireGeneration refuses a request whose launch generation is zero, the one count from 1 the
// claim and operator routes read: a request that names no generation is fenced against nothing.
func (s *server) requireGeneration(w http.ResponseWriter, r *http.Request, generation uint64) bool {
	if generation == 0 {
		s.refuse(w, r, http.StatusBadRequest, "generation is required")
		return false
	}
	return true
}
