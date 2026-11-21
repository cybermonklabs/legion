package daemon

import (
	"context"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/texts"
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
// that process with its pod once every worker of the pod has answered, no turn runs and no
// claim's activity report holds it (roundEnd.holding), or at the round's bound whatever the
// workers answered and whatever runs, and tells the controller when something holds it past that
// bound (reconcilePodUpdates, tellUpdateStalled), so a backstop relaunch under it would replace a
// process the round is about to replace and start its clock again.
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

// stallBackstopped is the machine's report of what it did to a stalled turn
// (supervise.Deps.StallBackstopped), and the only thing that tells the owner: a request the
// machine answered by leaving the turn alone is told to nobody, and a turn a held suspension
// ended is told as that. It is called inside the machine's decision, so the row is written on a
// goroutine of its own, bounded by stallNoticeBudget; the sweep is not waited for, because a
// notice whose whole purpose is that somebody hears it should not wait a tick for a tick it does
// not need.
//
// Nothing of what it writes is in the store, as the pod-update sweep's own pace is not
// (podUpdateOpenings). A row the store refuses is kept in memory and written again at the next
// sweep (oweStallNotice, tellStallsOwed), which is the same retry a stalled update round's
// notice gets (tellUpdateStalled).
func (s *supervision) stallBackstopped(c supervise.Claim, stop supervise.StallBackstopStop) {
	go func() {
		// This goroutine runs beside the machine that started it and every other, so it takes a
		// hold mark of its own (store.ForConcurrentWorker).
		ctx, cancel := context.WithTimeout(store.ForConcurrentWorker(context.Background()), stallNoticeBudget)
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

// oweStallNoticeAgain puts back a notice the sweep took out of the owed set and could not write,
// unless the claim owes another one by now. Whatever is owed for it since was owed by a
// machine's own report while this one was in the sweep's hand (stallBackstopped), which is a
// later stall of the same worker, and the newer record is the one its owner wants
// (oweStallNotice): putting the older one back over it would tell the owner a stall the
// daemon has since acted on again, and never tell the one it acted on last.
func (s *supervision) oweStallNoticeAgain(c supervise.Claim, stop supervise.StallBackstopStop) {
	s.backstopMu.Lock()
	defer s.backstopMu.Unlock()
	if _, newer := s.backstopOwed[c.Token]; newer {
		return
	}
	if s.backstopOwed == nil {
		s.backstopOwed = map[claim.Token]stallNoticeOwed{}
	}
	s.backstopOwed[c.Token] = stallNoticeOwed{claim: c, stop: stop}
}

// tellStallsOwed writes the notices a store refused since the last sweep. Each is taken out of
// the owed set before it is tried, and put back where it fails unless a newer one was owed
// meanwhile (oweStallNoticeAgain), so the sweep and the machine's own goroutine never write one
// row twice: a notice is in the set or in a hand, never both.
func (s *supervision) tellStallsOwed(ctx context.Context) {
	s.backstopMu.Lock()
	owed := s.backstopOwed
	s.backstopOwed = nil
	s.backstopMu.Unlock()
	for _, notice := range owed {
		if ctx.Err() != nil || !s.tellStallBackstopped(ctx, notice.claim, notice.stop) {
			s.oweStallNoticeAgain(notice.claim, notice.stop)
		}
	}
}

// stallNoticeOwed is one owner's notice the daemon has not managed to write: the claim as the
// machine held it when it acted on the stall, and what it did (supervise.StallBackstopStop).
type stallNoticeOwed struct {
	claim supervise.Claim
	stop  supervise.StallBackstopStop
}

// tellStallBackstopped tells the architect that owns the stall's notice — for a phase worker the
// architect its issue's notices reach, for a child's own architect the architect above that child,
// and for a tree's architect that architect itself, with the controller — what the daemon's stall
// backstop did to this turn, with the record the machine acted on in the notice: how long the
// agent was silent, the bound it passed, when the turn began and the call the record names, if
// any. The call's arguments are in no part of it: their text is in no column either, a shell
// command routinely holding a credential (supervise.StepRecord).
//
// It reports whether the owner was told: a row the store refuses is owed and told again at the
// next sweep, as a stalled update round's is (tellUpdateStalled). The project controller's own
// claim has no issue and so no owner to tell; the line it logs is the whole of its telling, and
// the record is in it. So is the line of a claim of another project's issue, which the operator's
// spawn makes: its owner's notice would be a row keyed by that issue, which only that project's
// daemon runs, so none is written (supervision.enqueue), and the line says so.
func (s *supervision) tellStallBackstopped(ctx context.Context, c supervise.Claim, stop supervise.StallBackstopStop) bool {
	found := stallBackstopRecord(c, stop, s.plan.clock.Now().Sub(stop.LastStep))
	if c.Issue == "" {
		s.log.Warn("supervise: the daemon's stall backstop acted on a stalled turn of a claim on no issue; there is no owner to tell",
			"claim", c.Token, "role", c.Role, "record", found)
		return true
	}
	if s.enqueue == nil {
		s.log.Warn("supervise: the daemon's stall backstop acted on a stalled turn, and this deployment has no workflow to tell the owner through",
			"claim", c.Token, "issue", c.Issue, "record", found)
		return true
	}
	written, err := s.enqueueStallNotice(ctx, c, found)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("supervise: tell the owner what the daemon's stall backstop did to a stalled turn; telling again at the next sweep",
				"claim", c.Token, "issue", c.Issue, "error", err)
		}
		return false
	}
	if !written {
		s.log.Warn("supervise: the daemon's stall backstop acted on a stalled turn of a claim of another project's issue, whose rows this daemon writes none of; no owner is told",
			"claim", c.Token, "issue", c.Issue, "role", c.Role, "record", found)
		return true
	}
	s.log.Warn("supervise: the daemon's stall backstop acted on a stalled turn; the owner is told",
		"claim", c.Token, "issue", c.Issue, "role", c.Role, "record", found)
	return true
}

// enqueueStallNotice writes the owner's notice rows, both in one call so a stall told to one
// side and not the other is never told again at the next sweep (tellUpdateStalled's own reason
// for one call), and answers whether it wrote them (supervision.enqueue). A phase worker's stall
// is read by the architect that owns its issue, and a child's own architect's stall by the
// architect above that child, as each one's death is. A tree architect's own stall is told to the
// controller as well, in a row of its own: every notice of the tree that no child's own architect
// takes reaches that architect, and nobody inside the tree is left to act on it (workflow's
// claimFailed, which tells a failed tree architect's death the same way).
func (s *supervision) enqueueStallNotice(ctx context.Context, c supervise.Claim, found texts.WorkerStalledRecord) (bool, error) {
	notice, err := record.NoticeOf(found)
	if err != nil {
		return false, err
	}
	rows := []record.OutboxPayload{notice}
	if claim.IsTreeArchitect(c.Role, c.Issue, c.Tree) {
		rows = append(rows, record.ControllerNotice(notice))
	}
	return s.enqueue(ctx, c.Issue, rows...)
}

// stallBackstopRecord is what the owner is told and what the log line carries: the
// worker-stalled notice's record (texts.WorkerStalledRecord), the facts the daemon read and what
// it did about them. It is the same record either way, so a deployment with no workflow leaves
// the same account in its log as the notice would have carried.
//
// What it says happened is the end the machine reported (supervise.StallBackstopStop.End),
// never what the sweep asked for and never what the machine set out to do: a turn a suspension
// was already waiting for ends in that suspension, with nothing launched in its place, and the
// end says so. What became of the stalled process is the stop word (stallStop), read from the
// end and from the machine's own record of the stop, since only the machine knows whether the
// runtime stopped it (supervise.StallBackstopStop.StopUnconfirmed). What the claim was charged
// is the launch failures the replacement charged on its way to this end
// (supervise.StallBackstopStop.LaunchFailures): the stall itself charges nothing. The moment a
// launch the pacing parked is due is wake_due. A call is named only where the record names one,
// and its end only where the call has ended.
func stallBackstopRecord(c supervise.Claim, stop supervise.StallBackstopStop, silent time.Duration) texts.WorkerStalledRecord {
	found := texts.WorkerStalledRecord{
		NoticeHead:     texts.NoticeHead{Kind: texts.WorkerStalled, Issue: c.Issue},
		Role:           c.Role,
		LastStep:       stop.LastStep.UTC(),
		SilentFor:      silent.Round(time.Minute).String(),
		Bound:          stop.Bound.String(),
		TurnStarted:    stop.Step.TurnStartedAt.UTC(),
		End:            string(stop.End),
		WakeDue:        stop.WakeDue.UTC(),
		Stop:           stallStop(stop),
		LaunchFailures: stop.LaunchFailures,
	}
	if stop.Step.CallID != "" {
		found.CallTool, found.CallID, found.CallStarted = stop.Step.Tool, stop.Step.CallID, stop.Step.StartedAt.UTC()
		if !stop.Step.EndedAt.IsZero() && !stop.Step.EndedAt.Before(stop.Step.StartedAt) {
			found.CallEnded = stop.Step.EndedAt.UTC()
		}
	}
	return found
}

// stallStop is the stop word of the notice (texts.StopStopped and the others): what became of
// the claim's stalled process. An end that let the process go (stallBackstopEnds) stopped it, or
// the runtime did not confirm the stop (supervise.StallBackstopStop.StopUnconfirmed), so the
// process may be running still. The one end that kept it made no stop of it. An end this daemon
// has no row for gets no stop word, since it cannot say what such an end did with the process.
func stallStop(stop supervise.StallBackstopStop) string {
	letsGo, known := stallBackstopEnds[stop.End]
	switch {
	case !known:
		return ""
	case !letsGo:
		return texts.StopNotMade
	case stop.StopUnconfirmed:
		return texts.StopUnconfirmed
	}
	return texts.StopStopped
}

// stallBackstopEnds is, for each end the machine can reach that tells its owner
// (supervise.ProcessEnd.TellsOwner), whether that end let the claim's process go. It is a table
// keyed by the end, and not a switch with a default, because the stop word must be total over
// the type: TestTheNoticeHasWordsForEveryEndThatTellsItsOwner walks supervise.ProcessEnds() and
// fails until a new end has a row here. Only the relaunch the pacing parked keeps the process:
// the claim records it and runs it until the queued launch comes due (supervise's reserveWake,
// parkWake).
var stallBackstopEnds = map[supervise.ProcessEnd]bool{
	supervise.EndRelaunched:      true,
	supervise.EndRelaunchParked:  false,
	supervise.EndRelaunchPending: true,
	supervise.EndSuspended:       true,
	supervise.EndFailed:          true,
}
