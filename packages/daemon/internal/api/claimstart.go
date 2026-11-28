package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// retryStart starts an agent that is running nothing: one that failed or retired, whatever retired
// it. It answers every refusal itself and reports the status its caller's success answer takes,
// with the sentence that answer carries (OperatorClaim.PhaseNotRestarted) and whether it started
// the agent. Whether the retry goes through the issue's phase is restartWorkedPhase's to decide:
// where it starts that phase again, a relaunched phase worker is handed the phase's task, since a
// worker relaunched outside its own phase is handed none and does nothing. Every agent it does not
// reach, an architect among them, is the machine's to answer (machineRetry), relaunched on its own
// with whatever task it still holds. It is how an operator relaunches the agents a runtime failed
// to launch — every failed agent in turn, with `legion claims retry --failed` — once the cause is
// gone.
//
// The session is the claim's own either way: this start relaunches the recorded one, or a new one
// once supervise's Machine.forgetSession has dropped it. A caller that wants a new session forgets
// it before calling here (freshSession), which is also what reaches an agent this start refuses.
// forgot is the session that caller left behind, and "" for the plain retry and for a claim that
// recorded none. Once a session is forgotten the claim has changed, so a start refused after it is
// answered as what the request did and not as a refusal of it (refusedAfterTheForget). by is who
// asks — the operator or the controller — which the machine's retry names (machineRetry).
//
// A restart whose start waits for the issue's admission slot started nothing: started is false,
// and the sentence names the wait (slotWait.stays), for a caller at claimStartContract or above
// (waitNamed). Every other start that ran reports started.
func (s *server) retryStart(w http.ResponseWriter, r *http.Request, m *supervise.Machine, token claim.Token, fresh bool,
	forgot string, by supervise.SuspendSource) (status int, notRestarted string, started, ok bool) {
	c := m.Claim()
	if supervise.FailedOrRetired(c.State) {
		if status, refusal, waits, restarted := s.restartWorkedPhase(r, token, c, fresh); restarted {
			switch {
			case refusal != "" && status == http.StatusInternalServerError:
				// The daemon failed to read or apply the restart: no refusal, so it stays the
				// failure it is, saying the session was left behind where the forget ran first.
				s.refuse(w, r, status, refusal+leftBehind(token, forgot))
				return 0, "", false, false
			case refusal != "":
				return s.refusedAfterTheForget(w, r, m, token, forgot, refusal, func() { s.refuse(w, r, status, refusal) })
			case waits != nil:
				return status, s.waitNamed(r, token, waits.stays(token, c.State, forgot)), false, true
			}
			return status, "", true, true
		}
	}
	return s.machineRetry(w, r, m, token, fresh, forgot, by)
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
//
// A restart that ran can still start nothing yet: the workflow writes the worker's start marked
// for the issue's admission slot wherever the issue's phase keeps one (workflow.StartOrOwe), and
// the outbox runs no such row while the issue holds no slot (record's outboxInOrder). So the slot
// is read after the restart is applied, with the issue's record (restartedIssue), and a start
// that waits for it is reported as waits, for the caller to name in its answer: before
// dispatch://INFRA-5649 the answer said nothing of it, and the controller read a restart that had
// not happened. The line this logs says the start waits and for what.
func (s *server) restartWorkedPhase(r *http.Request, token claim.Token, c supervise.Claim, fresh bool) (status int, refusal string,
	waits *slotWait, restarted bool) {
	if !isPhaseWorker(c.Role) || s.pool == nil || s.roleFor == nil {
		return 0, "", nil, false
	}
	if stopping := s.stopRefusal("retry"); stopping != "" {
		return http.StatusServiceUnavailable, stopping, nil, true
	}
	recorded, err := s.recordedIssue(r.Context(), c.Issue)
	if err != nil {
		s.logRequestFailure(r, err, "api: read the issue of a phase worker to retry", "claim", token)
		return http.StatusInternalServerError, fmt.Sprintf("could not read %s's record: %v", c.Issue, err), nil, true
	}
	if recorded == nil || s.roleFor(recorded.WorkingPhase()) != c.Role {
		return 0, "", nil, false
	}
	working := recorded.WorkingPhase()
	fact := intake.Fact(intake.RestartPhase{Issue: c.Issue, Role: c.Role, FreshSession: fresh})
	if recorded.Phase == phase.Held {
		fact = intake.RetryOrEscalate{Issue: c.Issue, Decision: intake.RetryDecision}
	}
	result, err := s.apply(r.Context(), "api", requestFactID("operator/retry"), fact)
	if err != nil {
		s.logRequestFailure(r, err, "api: apply the operator's retry of a phase", "claim", token, "issue", c.Issue)
		return http.StatusInternalServerError, fmt.Sprintf("could not retry %s's phase %s: %v", c.Issue, working, err), nil, true
	}
	if result.Refusal != nil {
		return result.Refusal.Status, result.Refusal.Message, nil, true
	}
	after, waits, err := s.restartedIssue(r.Context(), c.Issue, working)
	if err != nil {
		s.logRequestFailure(r, err, "api: read the issue after the operator's retry of its phase", "claim", token, "issue", c.Issue)
		return http.StatusInternalServerError, fmt.Sprintf("could not read %s's record after the retry: %v", c.Issue, err), nil, true
	}
	if after != nil && after.Phase == phase.Held {
		return http.StatusConflict, fmt.Sprintf("%s is still held after the retry of its phase %s, so nothing relaunched %s: the issue is out of Legion's work, where a retry starts nothing, and a todo on %s brings it back and releases its hold", c.Issue, working, token, c.Issue), nil, true
	}
	if after != nil && after.Phase == phase.AwaitingChildren && after.ChildrenWait != nil {
		back := "the daemon goes back to that start by itself"
		if record.ReturnsPastImplementing(after.ChildrenWait.Start) {
			back = "the daemon takes it back by itself to the first step of its walk after implementing, where that step stands at or before that start (testing on today's line), or to that start where the walk holds none or its pull request merged during the wait,"
		}
		return http.StatusConflict, fmt.Sprintf("%s waits on its open children after the retry of its phase %s, so no phase of it was started for %s: the retry moved it to awaiting_children, holding the start of %s (%s), and %s once no Dispatch child of %s is open",
			c.Issue, working, token, after.ChildrenWait.Start, after.ChildrenWait.Reason, back, c.Issue), nil, true
	}
	if waits != nil {
		s.log.Info("api: a retry restarted a phase, and its start was not run: it waits for the issue's admission slot",
			"claim", token, "issue", c.Issue, "phase", working, "waitsOn", waits.waitsOn)
		return http.StatusAccepted, "", waits, true
	}
	s.log.Info("api: a retry restarted a phase", "claim", token, "issue", c.Issue, "phase", working)
	return http.StatusAccepted, "", nil, true
}

// restartedIssue reads issue's record after the restart of its phase working, and, in the same
// read-only transaction, what the start that restart wrote waits for: the issue's admission slot,
// where the issue's phase keeps one (record.KeepsSlot) and no slot row names the issue
// (record.Store.Slots), which is the rule the outbox holds such a start by; and then the issues the
// mirror of Dispatch says it waits on that are not done (blockers.Load, blockers.Unfinished),
// which is what keeps a started issue from getting its slot back. waits is nil where the start
// waits for no slot, and for an issue the daemon does not record.
func (s *server) restartedIssue(ctx context.Context, issue string, working phase.Phase) (recorded *record.Issue, waits *slotWait, err error) {
	err = store.TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		waits = nil
		var err error
		if recorded, err = s.records.Issue(ctx, tx, issue); err != nil || recorded == nil || !record.KeepsSlot(*recorded) {
			return err
		}
		slots, err := s.records.Slots(ctx, tx)
		if err != nil {
			return err
		}
		for _, slot := range slots {
			if slot.Issue == issue {
				return nil
			}
		}
		rows, err := blockers.Load(ctx, tx, s.records, s.dispatchProject, []string{issue})
		if err != nil {
			return err
		}
		waits = &slotWait{issue: issue, phase: working, waitsOn: blockers.Unfinished(rows, issue)}
		return nil
	})
	return recorded, waits, err
}

// slotWait is a phase start the workflow queued behind its issue's admission slot
// (restartWorkedPhase): the issue, the phase the start is for, and the issues the issue waits on
// that are not done, in the order Dispatch lists them.
type slotWait struct {
	issue   string
	phase   phase.Phase
	waitsOn []string
}

// showsItsState ends a sentence of what became of a claim: the list its state is published on,
// in the forms both callers have, and no moment of what comes next (askAgainLater says why).
const showsItsState = "`read_state` (`legion claims list`) shows its state"

// slot is the wait in words: the slot the issue does not hold, and the issues it waits on.
func (w slotWait) slot() string {
	said := fmt.Sprintf("waits for an admission slot of %s, which holds none", w.issue)
	if len(w.waitsOn) > 0 {
		said += " and waits on " + strings.Join(w.waitsOn, ", ")
	}
	return said
}

// stays is the sentence of a claim that runs nothing and is left so, its start waiting for the
// slot: one failed or retired, with the session a fresh session left behind named where there is
// one (forgot).
func (w slotWait) stays(token claim.Token, state supervise.ClaimState, forgot string) string {
	left := ""
	if forgot != "" {
		left = " with its recorded session " + forgot + " left behind"
	}
	return fmt.Sprintf("%s stays %s%s, and the start of its phase %s %s; %s", token, state, left, w.phase, w.slot(), showsItsState)
}

// runs is the sentence of a claim that runs again at once, in a new session, while the task of
// its phase waits for the slot: a suspended claim's fresh session (freshSession).
func (w slotWait) runs(token claim.Token) string {
	return fmt.Sprintf("%s is back in a new session, and the task of its phase %s %s; %s", token, w.phase, w.slot(), showsItsState)
}

// machineRetry is the retry an architect or a phase worker that runs nothing gets from its own
// machine: fresh budgets, and the session the claim records relaunched, or a new one once
// supervise's Machine.forgetSession has dropped it. The machine refuses it for a claim in any
// other state, naming that state, and for the daemon's controller, naming who retries it
// (supervise's askedRetry). A caller that asked for a new session has forgotten the recorded one
// before it gets here (freshSession), so the relaunch spawns rather than resumes, and a refusal
// then is answered as what the request did (refusedAfterTheForget, forgot). The retry names by,
// its caller, and the wake pacing lets the operator's alone through unpaced
// (supervise.RequestAskedRetry.By).
//
// The retry is answered once its start is under way (startUnderWay), 202 with the claim launching,
// and 200 where it wrote no launch, such as one the wake pacing parked; a relaunch the machine
// holds for the agent's turn answers 202, as every operator request on an agent answers one
// (claimRequest).
func (s *server) machineRetry(w http.ResponseWriter, r *http.Request, m *supervise.Machine, token claim.Token, fresh bool,
	forgot string, by supervise.SuspendSource) (status int, notRestarted string, started, ok bool) {
	underWay, stopping, err := s.startUnderWay(r, m, token, "retry", supervise.RequestAskedRetry{Claim: token, FreshSession: fresh, By: by})
	switch {
	case stopping != "":
		return s.refusedAfterTheForget(w, r, m, token, forgot, stopping, func() { s.refuse(w, r, http.StatusServiceUnavailable, stopping) })
	case underWay, errors.Is(err, supervise.ErrSuspendHeld):
		return http.StatusAccepted, "", true, true
	case err != nil && !refusedByTheMachine(err):
		s.operationFailure(w, r, by, "retry", token, failedAfterTheForget(err, token, forgot))
		return 0, "", false, false
	case err != nil:
		return s.refusedAfterTheForget(w, r, m, token, forgot, err.Error(), func() { s.operationFailure(w, r, by, "retry", token, err) })
	}
	return http.StatusOK, "", true, true
}

// refusedByTheMachine is whether err is the machine declining a start it was asked for: a state
// that does not allow it (supervise.RefusedError), a suspension that holds the agent
// (holdRefusal), or a wait, such as a tree whose cleanup is reserved (wait.ErrWaiting). Anything
// else is the daemon failing to carry the start out, a launch write the store did not take among
// them, after which the claim may stand launching and try again. Only a refusal is answered as what
// a fresh session did after its forget (refusedAfterTheForget, dispatch://INFRA-5649 R2), which
// names the workflow's, the machine's and the hold's; a failure stays the failure it is.
func refusedByTheMachine(err error) bool {
	var refused *supervise.RefusedError
	return errors.As(err, &refused) || holdRefusal(err) || errors.Is(err, wait.ErrWaiting)
}

// failedAfterTheForget is err with leftBehind's words, for a failure the daemon answers as an
// error after a fresh session's forget ran: the claim changed, so the error says so.
func failedAfterTheForget(err error, token claim.Token, forgot string) error {
	if forgot == "" {
		return err
	}
	return fmt.Errorf("%w%s", err, leftBehind(token, forgot))
}

// leftBehind is what a failure after a fresh session's forget adds to its error: the session the
// claim recorded is gone, so the agent starts in a new one (dispatch://INFRA-5649 R2). It is ""
// where no forget ran.
func leftBehind(token claim.Token, forgot string) string {
	if forgot == "" {
		return ""
	}
	return fmt.Sprintf("; %s had already left its recorded session %s behind for a fresh session, so the agent starts in a new session once it is started", token, forgot)
}

// refusedAfterTheForget answers a start of an agent that runs nothing which was refused — the
// workflow's refusal of the phase's restart, the machine's of its retry, the machine's of the
// resume of a claim suspended before the request, or the daemon's stop — in the words refusal
// gives. Where nothing of the claim changed before it, forgot is "" and the refusal is the
// caller's answer, as before (asBefore writes it). Where a fresh session had left the claim's
// session behind first, the claim has changed, and the answer says so: 200, at both doors, with
// the refusal and the state the claim stands in, failed, retired or suspended with no session, in
// phaseNotRestarted (dispatch://INFRA-5649 R2). Its callers send it refusals alone: a failure of
// the daemon's own stays an error, which says the session was left behind (failedAfterTheForget). The operator's door already answered that way for
// every other refusal phaseNotRestarted carries, and the contract reads a changed claim as a 200.
// A caller below claimStartContract is answered as before, with the refusal (answersAsBefore).
func (s *server) refusedAfterTheForget(w http.ResponseWriter, r *http.Request, m *supervise.Machine, token claim.Token, forgot,
	refusal string, asBefore func()) (status int, notRestarted string, started, ok bool) {
	if forgot == "" {
		asBefore()
		return 0, "", false, false
	}
	if at, below := s.answersAsBefore(r); below {
		asBefore()
		s.servedAsBefore(r, at, token, servedTheRefusal)
		return 0, "", false, false
	}
	state := m.View().State
	s.log.Warn("api: a fresh session left the claim's recorded session behind, and its start was refused",
		"claim", token, "session", forgot, "state", state, "refusal", refusal)
	return http.StatusOK, fmt.Sprintf("%s left its recorded session %s behind for a fresh session, and its start was refused: %s. It is %s now, with no session recorded; %s",
		token, forgot, refusal, state, showsItsState), false, true
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
