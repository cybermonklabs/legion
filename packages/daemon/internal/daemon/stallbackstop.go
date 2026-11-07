package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// backstopStalledTurns ends the turn of every claim the sweep finds in one whose recorded last
// step is older than its role's backstop bound (`worker_stall_backstop_hours`,
// config.Config.StallBackstopFor), over the claims the orphan sweep already read this tick. It is
// the daemon's half of INFRA-1274: the worker's own plugin watches its tool calls and recovers in
// steps, and this acts where that plugin is itself the thing that is broken, since the daemon is
// the only part that outlives it.
//
// It reads the record and nothing else (supervise.StepRecord.LastStep): the last step the daemon
// received, or the turn's own start for a claim whose agent has sent nothing since that turn
// began. A claim whose record holds neither is judged from nothing and left alone, because this
// pass never guesses. It does not ask whether the worker's own ladder is running — the daemon
// cannot see it, since no step of that ladder crosses the wire — and nothing here needs to: the
// configuration refuses a bound below that whole ladder (config's checkStallBackstops), so a
// working plugin always finishes first.
//
// Three claims are passed over. A role whose bound is 0 is switched off by the deployment and is
// never read again. A claim in no turn has no turn to end: a claim's record is never cleared, so
// the step of a turn that ended hours ago still stands on an idle claim, and reading it would end
// a turn nobody is in. A claim in an update round is the round's to carry: the round replaces
// that process once every worker of its pod has answered, and tells the controller when it does
// not finish (reconcilePodUpdates, tellUpdateStalled), so a backstop relaunch under it would
// replace a process the round is waiting on and start its clock again.
//
// The stop itself is the machine's (supervise's backstopStalledTurn), which re-reads the claim's
// own record before it acts and charges nothing. The post is made at every tick that finds the
// claim still stalled, as StalePlugin's is, so a stop the runtime refuses is tried again at the
// next sweep with no timer of its own, and the record it read goes with it: the claim a machine
// holds carries the record as it stood when that machine was built, and the owner's notice must
// name the call the agent is actually in (supervise.StallBackstop.Step).
//
// Nobody is told from here. What this pass asks for is not what happens — the machine's own
// fence can leave the turn alone, a wake already parked over the claim takes it, and a
// suspension held for that turn ends the process rather than replacing it — so the owner is told
// by the machine's own report of the end it made (stallBackstopped), and a sweep that posts over
// a claim already told nothing tells nothing. The one thing left here is the notice a store
// refused, told again at this tick (tellStallsOwed).
func (s *supervision) backstopStalledTurns(ctx context.Context, claims []supervise.Claim) {
	s.tellStallsOwed(ctx)
	now := s.plan.clock.Now()
	for _, c := range claims {
		if ctx.Err() != nil {
			return
		}
		bound := s.cfg.StallBackstopFor(c.Role)
		if bound <= 0 || c.State != supervise.StateWorking || c.UpdateRound != "" {
			continue
		}
		last := c.Step.LastStep()
		if last.IsZero() || !last.Add(bound).Before(now) {
			continue
		}
		s.supervisor.post(c.Token, supervise.StallBackstop{Claim: c.Token, Bound: bound, Deadline: last.Add(bound), Step: c.Step})
	}
}

// stallNoticeBudget bounds the write of one owner's notice, which is made outside the sweep on a
// goroutine of its own (stallBackstopped): a store that never answers holds nothing open longer
// than this, and what it refuses is written again at the next sweep.
const stallNoticeBudget = 30 * time.Second

// stallBackstopped is the machine's report that it has ended a stalled turn
// (supervise.Deps.StallBackstopped), and the only thing that tells the owner: a request the
// machine answered by leaving the turn alone is told to nobody, and a turn a held suspension
// ended is told as that. It is called inside the machine's decision, so the row is written on a
// goroutine of its own, bounded like a terminal fact; the sweep is not waited for, because a
// notice whose whole purpose is that somebody hears it should not wait a tick for a tick it does
// not need.
//
// Nothing of what it writes is in the store, as the pod-update sweep's own pace is not
// (podUpdateOpenings). A row the store refuses is kept in memory and written again at the next
// sweep (oweStallNotice, tellStallsOwed), which is the same retry a stalled update round's
// notice gets (tellUpdateStalled).
func (s *supervision) stallBackstopped(c supervise.Claim, stop supervise.StallBackstopStop) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), stallNoticeBudget)
		defer cancel()
		if !s.tellStallBackstopped(ctx, c, stop) {
			s.oweStallNotice(c, stop)
		}
	}()
}

// oweStallNotice keeps one owner's notice for the next sweep, after the store refused it. One
// claim owes at most one: a second stall of the same claim is a later end of the same worker,
// and the newer record is the one its owner wants.
func (s *supervision) oweStallNotice(c supervise.Claim, stop supervise.StallBackstopStop) {
	s.backstopMu.Lock()
	defer s.backstopMu.Unlock()
	if s.backstopOwed == nil {
		s.backstopOwed = map[claim.Token]stallNoticeOwed{}
	}
	s.backstopOwed[c.Token] = stallNoticeOwed{claim: c, stop: stop}
}

// tellStallsOwed writes the notices a store refused since the last sweep. Each is taken out of
// the owed set before it is tried, and put back where it fails, so the sweep and the machine's
// own goroutine never write one row twice: a notice is in the set or in a hand, never both.
func (s *supervision) tellStallsOwed(ctx context.Context) {
	s.backstopMu.Lock()
	owed := s.backstopOwed
	s.backstopOwed = nil
	s.backstopMu.Unlock()
	for _, notice := range owed {
		if ctx.Err() != nil || !s.tellStallBackstopped(ctx, notice.claim, notice.stop) {
			s.oweStallNotice(notice.claim, notice.stop)
		}
	}
}

// stallNoticeOwed is one owner's notice the daemon has not managed to write: the claim as the
// machine held it when it ended the turn, and what it did (supervise.StallBackstopStop).
type stallNoticeOwed struct {
	claim supervise.Claim
	stop  supervise.StallBackstopStop
}

// tellStallBackstopped tells the owner of the stalled worker's issue — the architect the issue's
// notices reach — that the daemon ended this turn, with the record the machine acted on in the
// notice: how long the agent was silent, the bound it passed, when the turn began and the call
// the record names, if any. The call's arguments are in no part of it: their text is in no column
// either, a shell command routinely holding a credential (supervise.StepRecord).
//
// It reports whether the owner was told: a row the store refuses is owed and told again at the
// next sweep, as a stalled update round's is (tellUpdateStalled). The project controller's own
// claim has no issue and so no owner to tell; the line it logs is the whole of its telling, and
// the record is in it.
func (s *supervision) tellStallBackstopped(ctx context.Context, c supervise.Claim, stop supervise.StallBackstopStop) bool {
	found := stallBackstopRecord(c, stop, s.plan.clock.Now().Sub(stop.LastStep))
	if c.Issue == "" {
		s.log.Warn("supervise: the daemon's stall backstop ended a turn of a claim on no issue; there is no owner to tell",
			"claim", c.Token, "role", c.Role, "record", found)
		return true
	}
	if s.enqueue == nil {
		s.log.Warn("supervise: the daemon's stall backstop ended a turn, and this deployment has no workflow to tell the owner through",
			"claim", c.Token, "issue", c.Issue, "record", found)
		return true
	}
	if err := s.enqueueStallNotice(ctx, c, found); err != nil {
		if ctx.Err() == nil {
			s.log.Error("supervise: tell the owner that the daemon's stall backstop ended a turn; telling again at the next sweep",
				"claim", c.Token, "issue", c.Issue, "error", err)
		}
		return false
	}
	s.log.Warn("supervise: the daemon's stall backstop ended a turn; the owner is told",
		"claim", c.Token, "issue", c.Issue, "role", c.Role, "record", found)
	return true
}

// enqueueStallNotice writes the owner's notice rows, both in one call so a stall told to one
// side and not the other is never told again at the next sweep (tellUpdateStalled's own reason
// for one call). A worker's stall is its issue's architect's to read, as its death is. A tree
// architect's own stall is told to the controller as well, in a row of its own: every other
// notice of the tree reaches that architect, and nobody inside the tree is left to act on it
// (workflow's claimFailed, which tells a failed tree architect's death the same way).
func (s *supervision) enqueueStallNotice(ctx context.Context, c supervise.Claim, found string) error {
	notice := record.Notice{Kind: stallBackstopNotice, Role: c.Role, Reason: found}
	rows := []record.OutboxPayload{notice}
	if claim.IsTreeArchitect(c.Role, c.Issue, c.Tree) {
		rows = append(rows, record.ControllerNotice(notice))
	}
	return s.enqueue(ctx, c.Issue, rows...)
}

// stallBackstopNotice is the kind of the notice above (record's validNoticeKind), beside
// `worker-died`, which is what the architect hears of a claim that died rather than stalled.
const stallBackstopNotice = "worker-stalled"

// stallBackstopRecord is what the owner is told and what the log line carries: the record the
// daemon read, in one sentence, and what it did about it. It is the same text either way, so a
// deployment with no workflow leaves the same account in its log as the notice would have
// carried.
//
// What it says happened is the end the machine reported (supervise.StallBackstopStop.End),
// never what the sweep asked for and never what the machine set out to do: a turn a suspension
// was already waiting for ends in that suspension, with nothing launched in its place, and an
// owner told that the session had come back would go looking for a process that is not there.
// The three ends beside that one read the same way — a replacement the store would not write
// has stopped the worker and not brought it back yet, and one whose every try the runtime
// refused has failed the claim — so each says what is running for the claim now and what the
// owner is waiting on.
func stallBackstopRecord(c supervise.Claim, stop supervise.StallBackstopStop, silent time.Duration) string {
	call := "the daemon saw no call of this turn"
	if stop.Step.CallID != "" {
		call = fmt.Sprintf("the last call the daemon saw was %s (%s), which started at %s and has not ended",
			stop.Step.Tool, stop.Step.CallID, stallInstant(stop.Step.StartedAt))
		if !stop.Step.EndedAt.IsZero() && !stop.Step.EndedAt.Before(stop.Step.StartedAt) {
			call = fmt.Sprintf("the last call the daemon saw was %s (%s), which started at %s and ended at %s",
				stop.Step.Tool, stop.Step.CallID, stallInstant(stop.Step.StartedAt), stallInstant(stop.Step.EndedAt))
		}
	}
	end, charged := stallBackstopEnd(stop.End)
	return fmt.Sprintf("the daemon ended the %s's turn: its agent last stepped at %s, %s ago, past the %s this deployment gives the role (worker_stall_backstop_hours). The turn began at %s, and %s. The task it was serving is still owed to it and %s; %s The arguments of a call are never recorded, so they are in no part of this notice.",
		c.Role, stallInstant(stop.LastStep), silent.Round(time.Minute), stop.Bound, stallInstant(stop.Step.TurnStartedAt), call, end, charged)
}

// stallBackstopEnd is the clause of the notice that says what became of the worker, and the one
// that says what the claim was charged for it. The stall itself charges nothing, which is the
// whole reason this recovery is not the death path (supervise's backstopStalledTurn); the launch
// failures of a replacement that never started are charged, as every launch failure is, and the
// one end that carries them says so rather than leaving the owner a sentence its budgets
// contradict.
func stallBackstopEnd(made supervise.ProcessEnd) (end, charged string) {
	charged = "nothing was charged to the claim."
	switch made {
	case supervise.EndSuspended:
		return "a suspension that was already waiting for this turn ended the process rather than replacing it, so nothing runs for the claim until it is resumed", charged
	case supervise.EndRelaunchPending:
		return "its process was stopped, and the daemon could not write the launch that replaces it: nothing runs for the claim until one of the daemon's retries of that launch lands", charged
	case supervise.EndFailed:
		return "its process was stopped and every try at the replacement was refused, which ran the claim's launch budget out, so the claim has failed and nothing runs for it until it is retried",
			"the stall itself charged the claim nothing; the launch failures of the replacement that could not be started are what it was charged."
	}
	return "its session was brought back on a new process", charged
}

// stallInstant is a moment of the record as the notice and the log line write it, and "no time
// the daemon recorded" for a field the record does not hold.
func stallInstant(at time.Time) string {
	if at.IsZero() {
		return "no time the daemon recorded"
	}
	return at.UTC().Format(time.RFC3339)
}
