package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/experiment"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// invalidOperatorToken is every operator route's one refusal.
const invalidOperatorToken = "Invalid operator token"

// SpawnRequest is the operator's spawn: the claim on Role of Issue, in the tree Tree roots.
// Prompt is an optional test override; without it the daemon composes the role prompt itself.
// Task, when set, is its first delivery, queued before the agent is ready and sent once it is.
type SpawnRequest struct {
	Tree   string     `json:"tree"`
	Issue  string     `json:"issue"`
	Role   claim.Role `json:"role"`
	Prompt string     `json:"prompt,omitempty"`
	Task   string     `json:"task,omitempty"`
}

// DeliverRequest gives a claim a task.
type DeliverRequest struct {
	Task string `json:"task"`
}

// RetryRequest is the operator's retry, whose body is optional: a retry that sends none, as every
// caller before this contract did, resumes the claim's recorded session, or a new one once
// supervise's Machine.forgetSession has dropped it. FreshSession is `legion claims retry
// --fresh-session`, which starts the claim in a new session instead. It is the one effect the
// controller's fresh_session runs as well (freshSession), and it reaches every claim of the
// project that is not in a turn and whose launch is not in flight, where the plain retry reaches
// only a failed or retired claim (supervise's onOperatorRetry row).
type RetryRequest struct {
	FreshSession bool `json:"freshSession,omitempty"`
}

// ForgetSessionRequest names the claim's recorded session that the operator is abandoning.
type ForgetSessionRequest struct {
	Session string `json:"session"`
}

// operator admits a request that carries the operator's bearer, compared in constant time over
// both sides' hashes so neither the token nor its length leaks through the comparison. A daemon
// holding no operator token admits nothing.
func (s *server) operator(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		given, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !s.operatorSet || !bearer || !secureBearerEqual(s.operatorHash, given) {
			s.refuse(w, r, http.StatusForbidden, invalidOperatorToken)
			return
		}
		next(w, r)
	}
}

func secureBearerEqual(expected [sha256.Size]byte, given string) bool {
	presented := sha256.Sum256([]byte(given))
	return subtle.ConstantTimeCompare(presented[:], expected[:]) == 1
}

// spawn creates the claim — or finds it, when it exists — and posts the launch to its machine,
// then the task, so a claim the machine will not launch is given no task either. While the project
// is paused it creates and launches nothing (refusedWhilePaused).
func (s *server) spawn(w http.ResponseWriter, r *http.Request) {
	var req SpawnRequest
	if !s.readBody(w, r, &req, nil) || !s.requireFields(w, r,
		field{"tree", req.Tree}, field{"issue", req.Issue}, field{"role", string(req.Role)},
	) {
		return
	}
	for _, key := range []field{{"tree", req.Tree}, {"issue", req.Issue}} {
		if !claim.IsIssueKey(key.value) {
			s.refuse(w, r, http.StatusBadRequest, fmt.Sprintf("%s %q is not an issue key", key.name, key.value))
			return
		}
	}
	if !claim.IsRole(req.Role) {
		s.refuse(w, r, http.StatusBadRequest, fmt.Sprintf("role %q is not a role", req.Role))
		return
	}
	token, err := claim.NewToken(s.project, req.Issue, req.Role)
	if err != nil {
		s.operatorFailure(w, r, "spawn", token, err)
		return
	}
	ctx, decided := s.decision(r, token)
	defer decided()
	if s.stopped(w, r, "spawn") {
		return
	}
	c := supervise.Claim{
		Token: token, Project: s.project, Tree: req.Tree, Issue: req.Issue, Role: req.Role, State: supervise.StateQueued,
	}
	if s.refusedWhilePaused(w, r, "spawn", c) {
		return
	}
	if req.Tree == req.Issue && req.Role == claim.RoleArchitect {
		if _, err := s.trees.OpenOperatorTreeLifecycle(ctx, s.project, req.Tree, s.drawExperiments(req.Tree)); errors.Is(err, treelifecycle.ErrCleanupReserved) {
			s.refuse(w, r, http.StatusConflict, fmt.Sprintf("operator tree %s is waiting for durable cleanup: %v", req.Tree, err))
			return
		} else if err != nil {
			s.operatorFailure(w, r, "spawn", token, err)
			return
		}
	}
	m, created, err := s.supervisor.Create(ctx, c, req.Prompt)
	if err != nil {
		s.operatorFailure(w, r, "spawn", token, err)
		return
	}
	// The operator is waiting on this answer, so the wake pacing never holds this launch
	// (supervise's reserveWake): one person's spawn is not the wave the pacing exists for, and a
	// route that answered with a claim parked launching would read as a daemon that did nothing.
	if err := m.Handle(ctx, supervise.RequestSpawn{Claim: token, Exempt: true}); err != nil {
		s.operatorFailure(w, r, "spawn", token, err)
		return
	}
	if req.Task != "" {
		if err := m.Handle(ctx, supervise.RequestDeliver{Claim: token, Task: req.Task}); err != nil {
			s.operatorFailure(w, r, "deliver", token, err)
			return
		}
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.operatorDone(r, token)
	s.writeUnparsedAnswer(w, r, status, s.operatorAnswer(ctx, m.Claim()))
}

// drawExperiments draws the tree-unit experiments the project declares for tree, for the open
// that creates the tree's lifecycle row to run in its own transaction
// (store.Store.OpenOperatorTreeLifecycle, experiment.DrawAndRecord): one answer per experiment,
// under the mode the project declares it at and at the instant this server's clock reads,
// recorded against the tree's root issue key (record.Store.PutAssignments, which admission's own
// path draws through too). An operator's tree usually has no issue record at all, and then the
// draw writes nothing — nothing is refused and no record is invented.
func (s *server) drawExperiments(tree string) func(ctx context.Context, tx pgx.Tx) error {
	return func(ctx context.Context, tx pgx.Tx) error {
		return experiment.DrawAndRecord(ctx, tx, s.records, tree, s.experiments, config.ExperimentTree, s.source, s.now())
	}
}

// claimRequest is one operator request on an existing claim: event builds the request the claim's
// machine is posted, or answers the caller itself and reports false. A suspension the machine holds
// for the agent's turn (supervise.ErrSuspendHeld) is not a refusal: it answers 202 with the claim as
// it is, still working.
//
// Both reads of the claim are the machine's published view (supervise.Machine.View), never
// Machine.Claim: a claim whose launch is running holds its machine's lock for as long as the
// runtime waits on its pod, and a route that read through that lock would not even reach the
// machine with the suspension that stops that start (dispatch://INFRA-714 R10). The view is what
// the claim's latest decision left, which is what the answer reports.
func (s *server) claimRequest(request string, event func(http.ResponseWriter, *http.Request, supervise.Claim) (supervise.Event, bool)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := claim.Token(r.PathValue("token"))
		m, ok := s.supervisor.Machine(token)
		if !ok {
			s.refuse(w, r, http.StatusNotFound, "no claim "+string(token))
			return
		}
		ev, ok := event(w, r, m.View().Claim)
		if !ok {
			return
		}
		held, ok := s.claimEvent(w, r, m, token, request, ev)
		if !ok {
			return
		}
		status := http.StatusOK
		if held {
			status = http.StatusAccepted
		}
		s.operatorDone(r, token)
		s.writeUnparsedAnswer(w, r, status, s.operatorAnswer(r.Context(), m.View().Claim))
	}
}

// claimEvent posts one request to a claim's machine (postClaimEvent), as every operator request
// on a claim does (claimRequest): it answers the caller itself when the daemon is stopping or
// the machine refuses, and reports whether the machine held the request for the agent's turn
// (supervise.ErrSuspendHeld), which is no refusal.
func (s *server) claimEvent(w http.ResponseWriter, r *http.Request, m *supervise.Machine, token claim.Token,
	request string, ev supervise.Event) (held, ok bool) {
	posted, err := s.postClaimEvent(w, r, m, token, request, ev)
	switch {
	case !posted:
		return false, false
	case errors.Is(err, supervise.ErrSuspendHeld):
		return true, true
	case err != nil:
		s.operatorFailure(w, r, request, token, err)
		return false, false
	}
	return false, true
}

// postClaimEvent posts one request to a claim's machine under a decision of the route's, which
// it ends once the machine has answered, and hands that answer back for the caller to read: the
// refusals of one request mean different things to different routes (claimEvent,
// suspendForFreshSession), so each writes its own. The one answer this writes itself is the
// daemon's stop, which no claim's state explains and every route gives in the same words
// (stopped); posted is false there, and nothing was asked of the machine.
func (s *server) postClaimEvent(w http.ResponseWriter, r *http.Request, m *supervise.Machine, token claim.Token,
	request string, ev supervise.Event) (posted bool, err error) {
	ctx, decided := s.decision(r, token)
	defer decided()
	if s.stopped(w, r, request) {
		return false, nil
	}
	return true, m.Handle(ctx, ev)
}

// deliverEvent is the operator's task for a claim, which starts a turn the pause holds for every
// claim but the controller's (refusedWhilePaused).
func (s *server) deliverEvent(w http.ResponseWriter, r *http.Request, c supervise.Claim) (supervise.Event, bool) {
	var req DeliverRequest
	if !s.readBody(w, r, &req, nil) || !s.requireFields(w, r, field{"task", req.Task}) {
		return nil, false
	}
	if s.refusedWhilePaused(w, r, "deliver", c) {
		return nil, false
	}
	return supervise.RequestDeliver{Claim: c.Token, Task: req.Task}, true
}

func suspendEvent(_ http.ResponseWriter, _ *http.Request, c supervise.Claim) (supervise.Event, bool) {
	return supervise.RequestSuspend{Claim: c.Token, Reason: "the operator suspended it", By: supervise.SuspendedByOperator}, true
}

// resumeEvent is the operator's resume of a suspended claim, a launch the pause holds for every
// claim but the controller's (refusedWhilePaused).
func (s *server) resumeEvent(w http.ResponseWriter, r *http.Request, c supervise.Claim) (supervise.Event, bool) {
	if s.refusedWhilePaused(w, r, "resume", c) {
		return nil, false
	}
	return supervise.RequestResume{Claim: c.Token}, true
}

func stopEvent(_ http.ResponseWriter, _ *http.Request, c supervise.Claim) (supervise.Event, bool) {
	return supervise.RequestStop{Claim: c.Token}, true
}

// readOptionalBody decodes a request body the route reads when there is one: an operator command
// that sends none — every `legion claims` subcommand with no flags of its own, and every caller
// from before the body existed — leaves into as it was. A body that is there is read as strictly
// as any other.
func (s *server) readOptionalBody(w http.ResponseWriter, r *http.Request, into any) bool {
	body, err := decodeJSON(w, r, into, true)
	if err == nil {
		return true
	}
	if errors.Is(err, io.EOF) && len(bytes.TrimSpace(body)) == 0 {
		return true
	}
	s.refuseBadBody(w, r, err)
	return false
}

// forgetSession clears a recorded session only when the operator names that exact session and the
// claim has no live process. The session store is deliberately untouched.
func (s *server) forgetSession(w http.ResponseWriter, r *http.Request) {
	var req ForgetSessionRequest
	if !s.readBody(w, r, &req, nil) {
		return
	}
	if !s.requireFields(w, r, field{"session", req.Session}) {
		return
	}
	token := claim.Token(r.PathValue("token"))
	m, ok := s.supervisor.Machine(token)
	if !ok {
		s.refuse(w, r, http.StatusNotFound, "no claim "+string(token))
		return
	}
	c := m.Claim()
	if !c.HoldsSession(req.Session) {
		s.refuse(w, r, http.StatusConflict, fmt.Sprintf(
			"forget-session refused: %s records session %s; pass --session %s to start fresh", c.Token, c.Session, c.Session))
		return
	}
	if !forgettable(c.State) {
		s.refuse(w, r, http.StatusConflict, fmt.Sprintf(
			"forget-session refused: %s is %s; suspend, fail, or retire it before starting fresh", c.Token, c.State))
		return
	}
	if s.beforeForgetSessionHandle != nil {
		s.beforeForgetSessionHandle()
	}
	ctx, decided := s.decision(r, token)
	defer decided()
	if s.stopped(w, r, "forget-session") {
		return
	}
	if err := m.Handle(ctx, supervise.RequestForgetSession{Claim: token, Session: req.Session}); err != nil {
		s.operatorFailure(w, r, "forget-session", token, err)
		return
	}
	s.log.Info("api: operator forgot session", "claim", token, "session", c.Session, "sessionFile", c.SessionFile)
	s.operatorDone(r, token)
	view := s.operatorAnswer(ctx, m.Claim())
	view.ForgottenSession, view.ForgottenSessionFile = c.Session, c.SessionFile
	s.writeUnparsedAnswer(w, r, http.StatusOK, view)
}

func forgettable(state supervise.ClaimState) bool {
	return state == supervise.StateQueued || state == supervise.StateSuspended || state == supervise.StateFailed || state == supervise.StateRetired
}

// retryClaim is the operator's `legion claims retry`: the claim's own retry (retryStart), or,
// when the body asks for a new session (RetryRequest), the fresh session the controller's
// fresh_session runs too (freshSession). The two differ in what they reach and in what they
// leave behind, and nothing else answers either: a plain retry acts on a failed or retired
// claim, resuming the session it recorded, or a new one once supervise's Machine.forgetSession
// has dropped it, while a fresh session reaches every claim of the project that is not in a turn
// and whose launch is not in flight, and drops that session first. Only the fresh session answers
// the claim with a phase restart that was refused named on it
// (OperatorClaim.PhaseNotRestarted).
func (s *server) retryClaim(w http.ResponseWriter, r *http.Request) {
	token := claim.Token(r.PathValue("token"))
	m, ok := s.supervisor.Machine(token)
	if !ok {
		s.refuse(w, r, http.StatusNotFound, "no claim "+string(token))
		return
	}
	var req RetryRequest
	if !s.readOptionalBody(w, r, &req) {
		return
	}
	var (
		status       int
		notRestarted string
	)
	if req.FreshSession {
		// Whether the claim started again is not this door's line to write: the one Info line
		// it logs names the request and nothing it did (operatorDone), and freshSession logs
		// its own Warn where it stopped the claim and did not start it.
		status, notRestarted, _, ok = s.freshSession(w, r, m, token)
	} else if s.refusedWhilePaused(w, r, "retry", m.View().Claim) {
		return
	} else {
		status, ok = s.retryStart(w, r, m, token, false)
	}
	if !ok {
		return
	}
	s.operatorDone(r, token)
	view := s.operatorAnswer(r.Context(), m.Claim())
	view.PhaseNotRestarted = notRestarted
	s.writeUnparsedAnswer(w, r, status, view)
}

// retryStart starts a claim that is running nothing: one that failed or retired, whatever retired
// it. It answers every refusal itself and reports the status its caller's success answer takes.
// Whether the retry goes through the issue's phase is restartWorkedPhase's to decide: where it
// starts that phase again, a relaunched phase worker is handed the phase's task, since a worker
// relaunched outside its own phase is handed none and does nothing. Every claim it does not reach,
// an architect among them, is the machine's to answer (machineRetry), relaunched on its own with
// whatever task it still holds. It is how an operator relaunches the claims a runtime failed to
// launch — every failed claim in turn, with `legion claims retry --failed` — once the cause is
// gone.
//
// The session is the claim's own either way: this start relaunches the recorded one, or a new one
// once supervise's Machine.forgetSession has dropped it. A caller that wants a new session forgets
// it before calling here (freshSession), which is also what reaches a claim this start refuses.
func (s *server) retryStart(w http.ResponseWriter, r *http.Request, m *supervise.Machine, token claim.Token, fresh bool) (int, bool) {
	c := m.Claim()
	if supervise.FailedOrRetired(c.State) {
		if status, refusal, restarted := s.restartWorkedPhase(r, token, c, fresh); restarted {
			if refusal != "" {
				s.refuse(w, r, status, refusal)
				return 0, false
			}
			return status, true
		}
	}
	return s.machineRetry(w, r, m, token, fresh)
}

// restartWorkedPhase starts again the phase the claim's role works, which is what hands a
// relaunched phase worker that phase's task. Both recoveries run it: the retry of a failed or
// retired claim (retryStart), and the fresh session of a phase worker that comes back owing no
// task (freshSession). restarted is false for a claim no phase start reaches — an architect, a
// claim whose issue works another role's phase or has left the workflow, and any claim on a daemon
// that records no issues — and its caller starts such a claim another way.
//
//   - an issue held for that worker's own role (Options.RoleFor of the phase it is held from) is
//     retried as its architect's retry_or_escalate retries it (intake.RetryOrEscalate): the phase
//     it was held from comes back, and its role is started on it;
//   - an issue standing in that worker's own phase has that phase started again
//     (intake.RestartPhase). It is where an operator's stop leaves an issue, since a stop is no
//     death and holds nothing, and where a failure the workflow could not hold leaves one.
//
// Either way the start is the workflow's, so a restart that ran reports 202, for its caller to
// answer with the claim as it is until that start runs. A restart that did not run reports the
// status and the sentence it is refused with, and writes neither itself, because the two
// callers answer a refusal differently: the retry gives it as its own answer (retryStart),
// while the fresh session has started the claim before it gets here and answers success with
// the refusal named in it (freshSession). A restart that leaves the issue held relaunched
// nothing (a lingering tree's members stay where they stood), so it is refused naming the issue
// rather than reported as a restart.
func (s *server) restartWorkedPhase(r *http.Request, token claim.Token, c supervise.Claim, fresh bool) (status int, refusal string, restarted bool) {
	if !isPhaseWorker(c.Role) || s.pool == nil || s.roleFor == nil {
		return 0, "", false
	}
	if stopping := s.stopRefusal("retry"); stopping != "" {
		return http.StatusServiceUnavailable, stopping, true
	}
	recorded, err := s.recordedIssue(r.Context(), c.Issue)
	if err != nil {
		s.logRequestFailure(r, err, "api: read the issue of a phase worker to retry", "claim", token)
		return http.StatusInternalServerError, fmt.Sprintf("could not read %s's record: %v", c.Issue, err), true
	}
	if recorded == nil || s.roleFor(recorded.WorkingPhase()) != c.Role {
		return 0, "", false
	}
	working := recorded.WorkingPhase()
	fact := intake.Fact(intake.RestartPhase{Issue: c.Issue, Role: c.Role, FreshSession: fresh})
	if recorded.Phase == phase.Held {
		fact = intake.RetryOrEscalate{Issue: c.Issue, Decision: intake.RetryDecision}
	}
	result, err := s.apply(r.Context(), "api", requestFactID("operator/retry"), fact)
	if err != nil {
		s.logRequestFailure(r, err, "api: apply the operator's retry of a phase", "claim", token, "issue", c.Issue)
		return http.StatusInternalServerError, fmt.Sprintf("could not retry %s's phase %s: %v", c.Issue, working, err), true
	}
	if result.Refusal != nil {
		return result.Refusal.Status, result.Refusal.Message, true
	}
	after, err := s.recordedIssue(r.Context(), c.Issue)
	if err != nil {
		s.logRequestFailure(r, err, "api: read the issue after the operator's retry of its phase", "claim", token, "issue", c.Issue)
		return http.StatusInternalServerError, fmt.Sprintf("could not read %s's record after the retry: %v", c.Issue, err), true
	}
	if after != nil && after.Phase == phase.Held {
		return http.StatusConflict, fmt.Sprintf("%s is still held after the retry of its phase %s, so nothing relaunched %s: its tree lingers, and a lingering tree's members stay where they stood", c.Issue, working, token), true
	}
	s.log.Info("api: a retry restarted a phase", "claim", token, "issue", c.Issue, "phase", working)
	return http.StatusAccepted, "", true
}

// machineRetry is the retry an architect or a phase worker that runs nothing gets from its own
// machine: fresh budgets, and the session the claim records relaunched, or a new one once
// supervise's Machine.forgetSession has dropped it. The machine refuses it for a claim in any
// other state, naming that state, and for the daemon's controller, naming who retries it
// (supervise's operatorRetry). A caller that asked for a new session has forgotten the recorded one
// before it gets here (freshSession), so the relaunch spawns rather than resumes. A relaunch the
// machine holds for the agent's turn answers 202, as every operator request on a claim answers one
// (claimRequest).
func (s *server) machineRetry(w http.ResponseWriter, r *http.Request, m *supervise.Machine, token claim.Token, fresh bool) (int, bool) {
	held, ok := s.claimEvent(w, r, m, token, "retry", supervise.RequestOperatorRetry{Claim: token, FreshSession: fresh})
	if !ok {
		return 0, false
	}
	if held {
		return http.StatusAccepted, true
	}
	return http.StatusOK, true
}

// forgetRecordedSession has the claim drop the session it recorded, for a caller that is
// starting it in a new one (freshSession), and hands back the machine's refusal, if any, for
// that caller to answer: a claim running the session, which no fresh session reaches, one that
// recorded another session since the route read it, or a forget the store would not write. What
// such a refusal means depends on what the caller did before it — a fresh session that had
// already stopped the claim gives it as a sentence of its answer (notStartedAgain) — so this
// writes no answer of its own.
func (s *server) forgetRecordedSession(r *http.Request, m *supervise.Machine, token claim.Token, session string) error {
	ctx, decided := s.decision(r, token)
	defer decided()
	if err := m.Handle(ctx, supervise.RequestForgetSession{Claim: token, Session: session}); err != nil {
		return err
	}
	s.log.Info("api: a fresh session left the claim's recorded session behind", "claim", token, "session", session)
	return nil
}

// closeTree is the operator closing a tree no workflow issue backs, through its root claim: the
// tree's close, as the workflow's tree_close is for a tree whose linger expired. Without it a
// root the operator spawned could never end — under a sandbox, its Sandbox and the tree volume
// would stay for good. A workflow issue's tree is the workflow's to close, and a worker's claim is
// stopped, not closed.
//
// The tree's claims are read first and held as this route's decisions (RouteDecisions), so a
// daemon's stop that begins during the close keeps their worker stream connections; a read that
// fails there closes nothing, and a close the daemon's stop has already begun is refused. Then the
// root goes first, since its close is where the supervisor answers both of the questions that
// decide it — whether a workflow issue backs the tree, and whether this claim is its tree's root
// at all — where the answers and the close they decide sit together; a refused close stops
// nothing, and the refusal an operator sees is the machine's. A close of a tree already retired
// answers 200: the operator asked for an outcome that holds, and a retry after a timeout is not a
// failure. Then every other claim of the tree that has not retired is stopped, as tree_close stops
// each, so no worker is left on a tree volume that is being deleted. The close reserves the tree's
// cleanup first, so no claim of the tree can bind or launch meanwhile; after every claim stopped,
// the store's reserved cleanup has the runtime release the tree (Options.Releaser) and confirms
// it. A release failure leaves the reservation durable for the same close to resume; a claim the
// daemon supervises no machine for cannot be stopped that way, since the stop route answers 404 for
// it, until the daemon restarts and builds a machine for every stored claim; the 500 says so for
// that claim. Nothing retries either for the operator.
func (s *server) closeTree(w http.ResponseWriter, r *http.Request) {
	token := claim.Token(r.PathValue("token"))
	root, ok := s.supervisor.Machine(token)
	if !ok {
		s.refuse(w, r, http.StatusNotFound, "no claim "+string(token))
		return
	}
	c := root.Claim()
	ctx, decided := s.decision(r, token)
	defer decided()
	// The tree's other claims are this route's decisions from before the root's close, which waits
	// out the root's exit: a daemon's stop that begins meanwhile keeps their worker stream
	// connections open, and each one's stop below sends its shutdown frame over its own
	// (RouteDecisions). A claim the store gains later is recorded with the read the stops use.
	claims, err := s.supervisor.Claims(ctx)
	if err != nil {
		s.logFailure("api: read the claims of a tree the operator closes", "tree", c.Tree, "error", err)
		s.refuse(w, r, http.StatusInternalServerError, fmt.Sprintf("closed nothing: the daemon could not read the claims of tree %s", c.Tree))
		return
	}
	defer s.decisions.begin(treeOthers(claims, c.Tree, token))()
	if s.stopped(w, r, "close") {
		return
	}
	if err := root.Handle(ctx, supervise.RequestOperatorClose{Claim: token}); err != nil {
		s.operatorFailure(w, r, "close", token, err)
		return
	}
	lifecycle, cleanup, err := s.trees.ReserveOperatorTreeCleanup(ctx, s.project, c.Tree)
	if err != nil {
		s.logFailure("api: reserve cleanup of an operator-closed tree", "tree", c.Tree, "error", err)
		s.refuse(w, r, http.StatusConflict, fmt.Sprintf("closed %s's root claim, but its durable tree cleanup could not be reserved: %v. Retry legion claims close once that is resolved", c.Tree, err))
		return
	}
	claims, err = s.supervisor.Claims(ctx)
	if err != nil {
		s.logFailure("api: read the claims of a tree the operator closed", "tree", c.Tree, "error", err)
		s.refuse(w, r, http.StatusInternalServerError, fmt.Sprintf("closed %s's root claim %s, but the daemon could not read the tree's other claims to stop them", c.Tree, token))
		return
	}
	others := treeOthers(claims, c.Tree, token)
	// The one record that no stop check follows (stopped): a claim the tree gained while its root
	// exited, by an operator's spawn into the tree being closed, is recorded only here, so a daemon's
	// stop that begins in that same moment may already have closed its connection, and its stop then
	// ends its agent without the shutdown frame. Its stored state is still right (retired); a check
	// here would instead leave the root closed and the tree's other claims unstopped.
	defer s.decisions.begin(others)()
	var unstopped []string
	for _, other := range others {
		m, ok := s.supervisor.Machine(other)
		if !ok {
			s.log.Error("api: a claim of a tree the operator closed has no machine to stop", "claim", other)
			unstopped = append(unstopped, fmt.Sprintf("%s (the daemon supervises no machine for it, so legion claims stop cannot reach it until the daemon restarts)", other))
			continue
		}
		if err := m.Handle(ctx, supervise.RequestStop{Claim: other}); err != nil {
			s.logFailure("api: stop a claim of a tree the operator closed", "claim", other, "error", err)
			unstopped = append(unstopped, fmt.Sprintf("%s (%v)", other, err))
		}
	}
	if len(unstopped) > 0 {
		s.refuse(w, r, http.StatusInternalServerError, fmt.Sprintf("closed %s's root claim %s, but these claims of the tree were not stopped: %s. Stop each now with legion claims stop, then close again: the tree's durable cleanup waits for every claim of it",
			c.Tree, token, strings.Join(unstopped, "; ")))
		return
	}
	if cleanup {
		if err := s.trees.CleanupReservedTree(ctx, s.project, c.Tree, lifecycle.Epoch, s.releaser); err != nil {
			s.logFailure("api: cleanup an operator-closed tree", "tree", c.Tree, "error", err)
			s.refuse(w, r, http.StatusInternalServerError, fmt.Sprintf("closed %s's claims, but durable resource cleanup is pending: %v. Retry legion claims close after the reported cleanup error is resolved", c.Tree, err))
			return
		}
	}
	s.operatorDone(r, token)
	s.writeUnparsedAnswer(w, r, http.StatusOK, s.operatorAnswer(ctx, root.Claim()))
}

// operatorDone logs the one Info line every operator request on a claim that succeeded writes: its
// route, the claim and the caller's address. The operator's bearer names nobody, so the address is
// what the line can say of the caller; with it, a day's operator requests are counted from the log
// alone. A request that fails logs its failure instead (operatorFailure).
func (s *server) operatorDone(r *http.Request, token claim.Token) {
	s.log.Info("api: operator request", "route", r.Pattern, "claim", token, "caller", r.RemoteAddr)
}

// treeOthers is every claim of tree in claims, but root and those already retired.
func treeOthers(claims []supervise.Claim, tree string, root claim.Token) []claim.Token {
	var others []claim.Token
	for _, c := range claims {
		if c.Tree == tree && c.Token != root && c.State != supervise.StateRetired {
			others = append(others, c.Token)
		}
	}
	return others
}

// list answers every claim the daemon supervises, in token order: as its machine last published it
// (supervise.Machine.View), or, for a claim no machine supervises, as the store holds it. The
// machine's is what knows whether a suspension is held. It never waits on a decision in flight: a
// relaunch holds its machine for as long as the runtime waits out its pods, and with many claims
// one is nearly always relaunching.
//
// The step record is the store's either way (supervise.Claim.Step), as it is in every other
// operator answer (operatorAnswer). It is no part of a machine's state: the daemon writes it
// through a narrow call of its own as the agent's frames arrive and never reads it back into the
// machine's claim (supervise's putStep, store.PutClaimStep), so the machine's copy is the record
// as it stood when that machine was built — at this daemon's boot, for every claim it re-adopted
// — and taking the view whole would show an operator a call hours out of date, or none at all,
// for every supervised claim. This route reads the record with the claims it is already reading,
// where a route about one claim reads that claim's alone. These routes are the one surface a
// controller's record reaches, so what they show has to be the record the backstop reads.
//
// What the last pod-update sweep decided is answered beside them (Options.PodUpdateRounds), and a
// daemon with no sweep behind it answers the zero record, which says so: an operator who is served
// no record at all cannot tell a daemon that holds no issue back from one too old to say. Every
// answer this route writes carries the record, so an answer without it is an older daemon's and
// nothing else (OperatorClaims.PodUpdateRounds).
func (s *server) list(w http.ResponseWriter, r *http.Request) {
	claims, err := s.supervisor.Claims(r.Context())
	if err != nil {
		s.logRequestFailure(r, err, "api: read the claims")
		s.refuse(w, r, http.StatusInternalServerError, "the daemon could not read its claims")
		return
	}
	sort.Slice(claims, func(i, j int) bool { return claims[i].Token < claims[j].Token })
	views := make([]OperatorClaim, len(claims))
	for i, c := range claims {
		step := c.Step
		if m, ok := s.supervisor.Machine(c.Token); ok {
			c = m.View().Claim
		}
		views[i] = operatorView(c, step)
	}
	rounds := PodUpdateRounds{}
	if s.podUpdateRounds != nil {
		rounds = s.podUpdateRounds()
	}
	s.writeUnparsedAnswer(w, r, http.StatusOK, OperatorClaims{Claims: views, PodUpdateRounds: &rounds})
}

// operatorFailure answers an operator request its machine did not take: 409 with the machine's
// reason when the claim's state does not allow it, and otherwise a 500 that says what failed — the
// operator is the one who has to act on a launch the runtime refused.
func (s *server) operatorFailure(w http.ResponseWriter, r *http.Request, request string, token claim.Token, err error) {
	var refused *supervise.RefusedError
	if errors.As(err, &refused) {
		s.refuse(w, r, http.StatusConflict, refused.Error())
		return
	}
	s.logFailure("api: an operator request failed", "request", request, "claim", token, "error", err)
	s.refuse(w, r, http.StatusInternalServerError, fmt.Sprintf("%s %s: %v", request, token, err))
}
