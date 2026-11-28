package api

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// invalidOperatorToken is every operator route's one refusal.
const invalidOperatorToken = "Invalid operator token"

// DeliverRequest gives a claim a task.
type DeliverRequest struct {
	Task string `json:"task"`
}

// RetryRequest is the operator's retry, whose body is optional: a retry that sends none, as every
// caller before this contract did, resumes the claim's recorded session, or a new one once
// supervise's Machine.forgetSession has dropped it. FreshSession is `legion claims retry
// --fresh-session`, which starts the agent in a new session instead. It is the one effect the
// controller's fresh_session runs as well (freshSession), and it reaches every agent of the
// project that is not in a turn and whose launch is not in flight, where the plain retry reaches
// only an agent that failed or that a stop retired (supervise's onAskedRetry row).
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

// claimRequest is one operator request on an existing claim: event builds the request the claim's
// machine is posted, or answers the caller itself and reports false, and post posts it, answers
// every refusal itself and reports the status the success answer takes. A suspend and a stop
// are posted through claimEvent (answeredStatus): a suspension the machine holds for the agent's
// turn (supervise.ErrSuspendHeld) is not a refusal, and answers 202 with the claim as it is,
// still working. A resume starts the claim, and so does a deliver to a suspended claim, which
// revives it for the task (supervise's deliverResuming): both are answered once that start is
// under way (startedStatus, dispatch://INFRA-5649 R4), and a deliver to a claim that runs is
// answered there as before, since it writes no launch.
//
// Both reads of the claim are the machine's published view (supervise.Machine.View), never
// Machine.Claim: a claim whose launch is running holds its machine's lock for as long as the
// runtime waits on its pod, and a route that read through that lock would not even reach the
// machine with the suspension that stops that start (dispatch://INFRA-714 R10). The view is what
// the claim's latest decision left, which is what the answer reports.
func (s *server) claimRequest(request string, event func(http.ResponseWriter, *http.Request, supervise.Claim) (supervise.Event, bool),
	post func(http.ResponseWriter, *http.Request, *supervise.Machine, claim.Token, string, supervise.Event) (int, bool)) http.HandlerFunc {
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
		status, ok := post(w, r, m, token, request, ev)
		if !ok {
			return
		}
		s.operatorDone(r, token)
		s.writeUnparsedAnswer(w, r, status, s.operatorAnswer(r.Context(), m.View().Claim))
	}
}

// answeredStatus posts ev through claimEvent and reports the status of its success answer: 202
// for a request the machine held for the agent's turn, 200 for every other it took. Every request
// claimRequest posts is the operator's (supervise.SuspendedByOperator).
func (s *server) answeredStatus(w http.ResponseWriter, r *http.Request, m *supervise.Machine, token claim.Token,
	request string, ev supervise.Event) (int, bool) {
	held, ok := s.claimEvent(w, r, m, token, supervise.SuspendedByOperator, request, ev)
	switch {
	case !ok:
		return 0, false
	case held:
		return http.StatusAccepted, true
	}
	return http.StatusOK, true
}

// startedStatus posts ev, a request of the operator's that starts the claim, through
// startUnderWay, answers every refusal itself, and reports the status of its success answer: 202
// for a start this request wrote launching and for a request the machine held, 200 for one that
// wrote no launch, such as a launch the wake pacing parked.
func (s *server) startedStatus(w http.ResponseWriter, r *http.Request, m *supervise.Machine, token claim.Token,
	request string, ev supervise.Event) (int, bool) {
	underWay, stopping, err := s.startUnderWay(r, m, token, request, ev)
	switch {
	case stopping != "":
		s.refuse(w, r, http.StatusServiceUnavailable, stopping)
		return 0, false
	case underWay, errors.Is(err, supervise.ErrSuspendHeld):
		return http.StatusAccepted, true
	case err != nil:
		s.operationFailure(w, r, supervise.SuspendedByOperator, request, token, err)
		return 0, false
	}
	return http.StatusOK, true
}

// claimEvent posts one request of by's to a claim's machine (postClaimEvent), as every operator
// request on an agent does (claimRequest): it answers the caller itself when the daemon is
// stopping or the machine refuses, and reports whether the machine held the request for the
// agent's turn (supervise.ErrSuspendHeld), which is no refusal.
func (s *server) claimEvent(w http.ResponseWriter, r *http.Request, m *supervise.Machine, token claim.Token,
	by supervise.SuspendSource, request string, ev supervise.Event) (held, ok bool) {
	posted, err := s.postClaimEvent(w, r, m, token, request, ev)
	switch {
	case !posted:
		return false, false
	case errors.Is(err, supervise.ErrSuspendHeld):
		return true, true
	case err != nil:
		s.operationFailure(w, r, by, request, token, err)
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

// startUnderWay posts ev, a request that can start a claim that runs nothing — the operator's
// resume and deliver (startedStatus), the controller's resume_agent (agentEventAction), a fresh
// session's resume (freshSession), and the machine's own retry of a failed or retired claim
// (machineRetry) — and reports once that start is under way rather than once it has ended
// (dispatch://INFRA-5649 R4). A request that starts nothing, such as a deliver to a running
// claim, writes no launch and reports what the machine answered. A start under Kubernetes waits minutes
// on a pod, and a route that waited for all of it held its caller past the caller's own timeout:
// the controller's fresh_session timed out 45 times of 138 calls on 2026-11-12, after 142 to 356 s
// (dispatch://INFRA-3439, folded into dispatch://INFRA-5649).
//
// underWay is whether this decision wrote the claim launching and the store took that write
// (supervise.Machine.HandleStart), with no error answered before the route answers: the
// published view then shows the launch, which is what the caller is answered with, and the start
// goes on or has just ended. The route answers at that write, never later, so a start that hangs
// holds nobody. A decision that ended without writing a launch reports false, with err what the
// machine answered, as Handle answers it: a refusal, or nil for a launch the wake pacing parked.
// One whose answer, an error, had arrived by the moment the route answers reports that error, as
// Handle would, since that is the fuller answer. That the launch was written, not whether its
// start has ended, decides underWay, so one request answers alike however fast its runtime
// starts a process. stopping is the refusal of a request the daemon's stop came before, which
// asked nothing of the machine.
//
// The decision runs on a context of its own (decision), so the route's answer does not cancel it:
// it outlives the caller's connection, it ends at the daemon's drain, and a suspension cancels the
// start it runs, as it cancels every start (supervise's stopStart). It takes a store mark of its
// own (store.ForConcurrentWorker), because it runs on a goroutine beside the request. The claim
// stays recorded as a route's decision (RouteDecisions) until that decision ends, as it stayed
// while the route waited for it, so the daemon's stop treats the claim as it did before. Where
// the route answered before the end, the end is logged (logStartEnded), and the claim's state —
// `read_state`, `legion claims list` — carries what the start came to: running, or failed with
// the start's failure.
func (s *server) startUnderWay(r *http.Request, m *supervise.Machine, token claim.Token, request string,
	ev supervise.Event) (underWay bool, stopping string, err error) {
	ctx, decided := s.decision(r, token)
	if stopping := s.stopRefusal(request); stopping != "" {
		decided()
		return false, stopping, nil
	}
	launched, done := m.HandleStart(store.ForConcurrentWorker(ctx), ev)
	select {
	case err := <-done:
		decided()
		return err == nil && wasClosed(launched), "", err
	case <-launched:
	}
	select {
	case err := <-done:
		decided()
		return err == nil, "", err
	default:
	}
	go func() {
		err := <-done
		decided()
		s.logStartEnded(token, request, err)
	}()
	return true, "", nil
}

// wasClosed is whether ch has been closed, without waiting.
func wasClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// logStartEnded writes the end of a start whose route answered while it ran (startUnderWay): the
// answer said the start was under way, so this line and the claim's state are where its end is
// read. A start that ended in an error is logged as a failure (logFailure), at Info once the
// daemon's stop has begun, since the stop is what ends a start at the drain.
func (s *server) logStartEnded(token claim.Token, request string, err error) {
	if err != nil {
		s.logFailure("api: a start answered while it ran ended in an error", "request", request, "claim", token, "error", err)
		return
	}
	s.log.Info("api: a start answered while it ran ended", "request", request, "claim", token)
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
	return supervise.RequestResume{Claim: c.Token, By: supervise.SuspendedByOperator}, true
}

func stopEvent(_ http.ResponseWriter, _ *http.Request, c supervise.Claim) (supervise.Event, bool) {
	return supervise.RequestStop{Claim: c.Token, By: supervise.SuspendedByOperator}, true
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
	if err := m.Handle(ctx, supervise.RequestForgetSession{Claim: token, Session: req.Session, By: supervise.SuspendedByOperator}); err != nil {
		s.operationFailure(w, r, supervise.SuspendedByOperator, "forget-session", token, err)
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
// and whose launch is not in flight, and drops that session first. Either answers what the claim
// does not record on it (OperatorClaim.PhaseNotRestarted): a start that waits for its issue's
// admission slot, for both, and, for the fresh session alone, a phase restart that was refused,
// or a start refused once the request had changed the claim.
//
// The claim answered is the machine's published view (supervise.Machine.View), never
// Machine.Claim, which waits out a start still running (dispatch://INFRA-5649 R4).
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
		status, notRestarted, _, ok = s.freshSession(w, r, m, token, supervise.SuspendedByOperator)
	} else if s.refusedWhilePaused(w, r, "retry", m.View().Claim) {
		return
	} else {
		status, notRestarted, _, ok = s.retryStart(w, r, m, token, false, "", supervise.SuspendedByOperator)
	}
	if !ok {
		return
	}
	s.operatorDone(r, token)
	view := s.operatorAnswer(r.Context(), m.View().Claim)
	view.PhaseNotRestarted = notRestarted
	s.writeUnparsedAnswer(w, r, status, view)
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
		s.operationFailure(w, r, supervise.SuspendedByOperator, "close", token, err)
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
		if err := m.Handle(ctx, supervise.RequestStop{Claim: other, By: supervise.SuspendedByOperator}); err != nil {
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

// operatorDone logs the one Info line every operator request on an agent that succeeded writes: its
// route, the claim and the caller's address. The operator's bearer names nobody, so the address is
// what the line can say of the caller; with it, a day's operator requests are counted from the log
// alone. A request the claim's machine refused (409) writes its answer and logs no line. One that
// fails otherwise logs its failure instead (operationFailure), and that line names who asked in
// its `by` field, so the operator's failed requests are counted there, apart from the
// controller's, whose requests the agent door fails through the same line.
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

// operationFailure answers an agent action its machine did not take, the operator's or the
// controller's (by): 409 with the machine's reason when the claim's state does not allow it, 409
// when the request's first act found the step it read the claim on moved to another claim
// (supervise.ErrStepLeft, agentDoor), with nothing of the claim changed, and otherwise a 500 that
// says what failed — the caller is the one who has to act on a launch the runtime refused. The
// line it logs names the caller in its `by` field, so the operator's failures are told from the
// controller's (operatorDone).
func (s *server) operationFailure(w http.ResponseWriter, r *http.Request, by supervise.SuspendSource, request string,
	token claim.Token, err error) {
	var refused *supervise.RefusedError
	switch {
	case errors.As(err, &refused):
		s.refuse(w, r, http.StatusConflict, refused.Error())
		return
	case errors.Is(err, supervise.ErrStepLeft):
		s.refuse(w, r, http.StatusConflict, fmt.Sprintf(
			"%s refused: the step %s was read on is another claim's now, and nothing of %s changed, so ask again: %v", request, token, token, err))
		return
	}
	s.logFailure("api: an agent action failed", "by", string(by), "request", request, "claim", token, "error", err)
	s.refuse(w, r, http.StatusInternalServerError, fmt.Sprintf("%s %s: %v", request, token, err))
}
