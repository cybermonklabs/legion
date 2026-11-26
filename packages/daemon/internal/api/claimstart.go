package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// retryStart starts an agent that is running nothing: one that failed or retired, whatever retired
// it. It answers every refusal itself and reports the status its caller's success answer takes.
// Whether the retry goes through the issue's phase is restartWorkedPhase's to decide: where it
// starts that phase again, a relaunched phase worker is handed the phase's task, since a worker
// relaunched outside its own phase is handed none and does nothing. Every agent it does not reach,
// an architect among them, is the machine's to answer (machineRetry), relaunched on its own with
// whatever task it still holds. It is how an operator relaunches the agents a runtime failed to
// launch — every failed agent in turn, with `legion claims retry --failed` — once the cause is
// gone.
//
// The session is the claim's own either way: this start relaunches the recorded one, or a new one
// once supervise's Machine.forgetSession has dropped it. A caller that wants a new session forgets
// it before calling here (freshSession), which is also what reaches an agent this start refuses. by
// is who asks — the operator or the controller — which the machine's retry names
// (machineRetry).
func (s *server) retryStart(w http.ResponseWriter, r *http.Request, m *supervise.Machine, token claim.Token, fresh bool,
	by supervise.SuspendSource) (int, bool) {
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
	return s.machineRetry(w, r, m, token, fresh, by)
}

// restartWorkedPhase starts again the phase the claim's role works, which is what hands a
// relaunched phase worker that phase's task. Both recoveries run it: the retry of a failed or
// retired agent (retryStart), and the fresh session of a phase worker that comes back owing no
// task (freshSession). restarted is false for an agent no phase start reaches — an architect, an
// agent whose issue works another role's phase or has left the workflow, and any agent on a daemon
// that records no issues — and its caller starts such an agent another way.
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
// while the fresh session has started the agent before it gets here and answers success with
// the refusal named in it (freshSession). A restart that leaves the issue held relaunched
// nothing: the workflow retries no phase of an issue out of Legion's work (workflow's
// retryOrEscalate), whose todo brings it back and releases the hold, so it is refused naming the issue
// rather than reported as a restart. So is one that moved the issue to awaiting_children: a
// Dispatch child of it is open, so the workflow started no phase of it and holds the start
// instead (workflow's awaitChildren, dispatch://INFRA-3320 R9), and the answer says that the issue
// waits on its children, naming them through the reason the held start keeps, and where the
// return takes it: a held testing, reviewing or accepting (record.ReturnsPastImplementing) to the
// first step of its walk after implementing where that step stands at or before that start,
// testing on today's line, and to that start where the walk holds no such step or its pull
// request merged during the wait (record.ReturnStep, R12); a held planning or implementing to
// that start. The answer says this without reading the walk.
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
		return http.StatusConflict, fmt.Sprintf("%s is still held after the retry of its phase %s, so nothing relaunched %s: the issue is out of Legion's work, where a retry starts nothing, and a todo on %s brings it back and releases its hold", c.Issue, working, token, c.Issue), true
	}
	if after != nil && after.Phase == phase.AwaitingChildren && after.ChildrenWait != nil {
		back := "the daemon goes back to that start by itself"
		if record.ReturnsPastImplementing(after.ChildrenWait.Start) {
			back = "the daemon takes it back by itself to the first step of its walk after implementing, where that step stands at or before that start (testing on today's line), or to that start where the walk holds none or its pull request merged during the wait,"
		}
		return http.StatusConflict, fmt.Sprintf("%s waits on its open children after the retry of its phase %s, so no phase of it was started for %s: the retry moved it to awaiting_children, holding the start of %s (%s), and %s once no Dispatch child of %s is open",
			c.Issue, working, token, after.ChildrenWait.Start, after.ChildrenWait.Reason, back, c.Issue), true
	}
	s.log.Info("api: a retry restarted a phase", "claim", token, "issue", c.Issue, "phase", working)
	return http.StatusAccepted, "", true
}

// machineRetry is the retry an architect or a phase worker that runs nothing gets from its own
// machine: fresh budgets, and the session the claim records relaunched, or a new one once
// supervise's Machine.forgetSession has dropped it. The machine refuses it for a claim in any
// other state, naming that state, and for the daemon's controller, naming who retries it
// (supervise's askedRetry). A caller that asked for a new session has forgotten the recorded one
// before it gets here (freshSession), so the relaunch spawns rather than resumes. A relaunch the
// machine holds for the agent's turn answers 202, as every operator request on an agent answers one
// (claimRequest). The retry names by, its caller, and the wake pacing lets the operator's alone
// through unpaced (supervise.RequestAskedRetry.By).
func (s *server) machineRetry(w http.ResponseWriter, r *http.Request, m *supervise.Machine, token claim.Token, fresh bool,
	by supervise.SuspendSource) (int, bool) {
	held, ok := s.claimEvent(w, r, m, token, by, "retry", supervise.RequestAskedRetry{Claim: token, FreshSession: fresh, By: by})
	if !ok {
		return 0, false
	}
	if held {
		return http.StatusAccepted, true
	}
	return http.StatusOK, true
}

// forgetRecordedSession has the claim drop the session it recorded, for a caller that is
// starting the agent in a new one (freshSession), and hands back the machine's refusal, if any, for
// that caller to answer: an agent running the session, which no fresh session reaches, one whose
// claim recorded another session since the route read it, one a suspension above by — the caller,
// whose resume would start the agent — took over since the route read it
// (supervise.RequestForgetSession.By), or a forget the store would not write. What such a
// refusal means depends on what the caller did before it — a fresh session that had already
// stopped the agent gives it as a sentence of its answer (notStartedAgain) — so this writes no
// answer of its own.
func (s *server) forgetRecordedSession(r *http.Request, m *supervise.Machine, token claim.Token, session string,
	by supervise.SuspendSource) error {
	if s.beforeForgetSessionHandle != nil {
		s.beforeForgetSessionHandle()
	}
	ctx, decided := s.decision(r, token)
	defer decided()
	if err := m.Handle(ctx, supervise.RequestForgetSession{Claim: token, Session: session, By: by}); err != nil {
		return err
	}
	s.log.Info("api: a fresh session left the claim's recorded session behind", "claim", token, "session", session)
	return nil
}
