package daemon

import (
	"context"
	"fmt"
	"strings"
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

// stallBackstopped is the machine's report of what it did to a stalled turn
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
// machine held it when it acted on the stall, and what it did (supervise.StallBackstopStop).
type stallNoticeOwed struct {
	claim supervise.Claim
	stop  supervise.StallBackstopStop
}

// tellStallBackstopped tells the owner of the stalled worker's issue — the architect the issue's
// notices reach — what the daemon's stall backstop did to this turn, with the record the machine
// acted on in the notice: how long the agent was silent, the bound it passed, when the turn
// began and the call the record names, if any. The call's arguments are in no part of it: their
// text is in no column either, a shell command routinely holding a credential
// (supervise.StepRecord).
//
// What the lines below say of the end is the record itself, which carries it: four of the five
// ends end the turn and the parked one takes the task back and leaves the stalled process
// running (supervise.EndRelaunchParked), so a line of its own saying the turn was ended would be
// false for that one as the notice's old shared opening was (stallBackstopEnds).
//
// It reports whether the owner was told: a row the store refuses is owed and told again at the
// next sweep, as a stalled update round's is (tellUpdateStalled). The project controller's own
// claim has no issue and so no owner to tell; the line it logs is the whole of its telling, and
// the record is in it.
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
	if err := s.enqueueStallNotice(ctx, c, found); err != nil {
		if ctx.Err() == nil {
			s.log.Error("supervise: tell the owner what the daemon's stall backstop did to a stalled turn; telling again at the next sweep",
				"claim", c.Token, "issue", c.Issue, "error", err)
		}
		return false
	}
	s.log.Warn("supervise: the daemon's stall backstop acted on a stalled turn; the owner is told",
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
// Every other end reads the same way — a replacement the pacing parked has taken the task back
// and not replaced the process yet, one the store would not write has stopped the worker and
// not brought it back, and one whose every try the runtime refused has failed the claim — so
// each says what is running for the claim now and what the owner is waiting on
// (stallBackstopEnds).
//
// Its opening is the end's too, and not one sentence shared by all of them, because the ends
// are not one thing done: the daemon ended the turn of four of them and took the task back
// without ending the turn of the fifth, whose process runs on. Every clause of what follows is
// something the daemon has already done, and what is not done is named as what the claim waits
// on and never as what will happen, since nothing tells this owner a second time if it does not
// happen (supervise's backstopStalledTurn).
//
// What became of the stalled process is a sentence of its own (stallStopClause), written from
// the machine's own record of the stop rather than from the end: the end says the process was
// let go, and only the machine knows whether the runtime stopped it
// (supervise.StallBackstopStop.StopUnconfirmed). A pod the runtime would not stop goes on
// running and goes on holding what it runs on, which is the half of this news its owner acts on.
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
	words := stallBackstopEnd(stop.End)
	fill := stallFill(c, stop)
	return fmt.Sprintf("%s: its agent last stepped at %s, %s ago, past the %s this deployment gives the role (worker_stall_backstop_hours). The turn began at %s, and %s. The task it was serving is still owed to it and %s; %s %sThe arguments of a call are never recorded, so they are in no part of this notice.",
		fill.Replace(words.opening), stallInstant(stop.LastStep), silent.Round(time.Minute), stop.Bound,
		stallInstant(stop.Step.TurnStartedAt), call, fill.Replace(words.end), fill.Replace(words.charged),
		stallStopClause(words, stop))
}

// stallStopClause is what the notice says of the stalled process itself, for an end that let it
// go: that it was stopped, or that the runtime would not stop it and it may be running still. An
// end that let no process go — the relaunch the pacing parked, whose claim keeps and runs its
// process (supervise.EndRelaunchParked) — has no such sentence, and neither has an end this
// daemon has no row for, which says nothing of a process it cannot name (stallBackstopEnd).
//
// The sentence ends with a space, since it stands between the charge clause and the one about a
// call's arguments, and is empty where there is none.
func stallStopClause(words stallEndWords, stop supervise.StallBackstopStop) string {
	if words.process != stallProcessLetGo {
		return ""
	}
	if stop.StopUnconfirmed {
		return stallStopRefused + " "
	}
	return stallProcessStopped + " "
}

// stallProcessStopped and stallStopRefused are those two sentences. The second says what the
// daemon knows and no more: the runtime did not answer the stop, so the process is one nobody
// has seen end, and a pod of it goes on holding its node's room and its disk until an operator
// or the tree's close ends it (supervise's endHeld, fail, stopPrevious).
const (
	stallProcessStopped = "Its stalled process was stopped."
	stallStopRefused    = "The runtime did not confirm the stop of its stalled process, so that process may be running still and holding what it runs on."
)

// stallFill fills the two values a clause of the notice names where it names them: the claim's
// role, and the moment the launch a park queued is due to run
// (supervise.StallBackstopStop.WakeDue). They are written into the clauses as words rather than
// handed to a format string so that each row of stallBackstopEnds reads as the sentence the
// owner gets, and so that a clause naming neither needs nothing of its caller.
func stallFill(c supervise.Claim, stop supervise.StallBackstopStop) *strings.Replacer {
	return strings.NewReplacer(stallRoleWord, string(c.Role), stallDueWord, stallInstant(stop.WakeDue))
}

// stallRoleWord and stallDueWord are those two values as the clauses of stallBackstopEnds write
// them (stallFill).
const (
	stallRoleWord = "{role}"
	stallDueWord  = "{due}"
)

// stallEndWords is what the notice says of one end: the opening that says what the daemon did
// to this claim, the clause that says what is running for the claim now and what it waits on,
// the one that says what the claim was charged for it, and whether the end let the claim's
// process go (stallStopClause).
type stallEndWords struct {
	opening, end, charged string
	process               stallProcessFate
}

// stallProcessFate is what an end did with the claim's process, which no clause of the table
// says because the sentence about it is written from the machine's record of the stop rather
// than from the end (stallStopClause). Its zero value says neither, so a row added without it is
// a row the walk fails (TestTheNoticeHasWordsForEveryEndThatTellsItsOwner): an end that lets a
// process go must not reach its owner with nothing said about that process.
type stallProcessFate int

const (
	stallFateUnsaid stallProcessFate = iota
	// stallProcessKept is the claim still recording and running the process it had.
	stallProcessKept
	// stallProcessLetGo is the claim no longer recording it: stopped, or left running by a
	// runtime that would not stop it (supervise.StallBackstopStop.StopUnconfirmed).
	stallProcessLetGo
)

// stallNothingCharged is the charge clause of every end but the one that failed the claim. The
// stall itself charges nothing, which is the whole reason this recovery is not the death path
// (supervise's backstopStalledTurn).
const stallNothingCharged = "nothing was charged to the claim."

// stallBackstopEnds is what the owner's notice says for each end the machine can reach
// (supervise.ProcessEnd): the opening the notice begins with, the clause that says what is
// running for the claim now and what it waits on, the charge clause, and what the end did with
// the claim's process. It is a table keyed by the end, and not a switch with a
// default, because a default is how this notice came to tell an owner that a session had been
// brought back for ends nobody had worded: the wording is total over the type, and
// TestTheNoticeHasWordsForEveryEndThatTellsItsOwner walks supervise.ProcessEnds() and fails
// until a new end has every word of a row here. An end that tells nobody
// (supervise.ProcessEnd.TellsOwner) has no row, because no notice is ever written from it.
//
// Each row is held to one rule: it states what the daemon has already done, and names what the
// claim waits on for anything it has not. Only the relaunched row ends in something running,
// because only there has a process been started (supervise's launchNow records the locator the
// runtime answered with). Where the daemon has queued work — the parked launch, the retry of a
// launch the store refused — the row says what is queued and what can end the claim before that
// work runs, since nothing writes this owner a second notice when it does: a suspension already
// waiting ends the wake's turn in a stop (supervise's wakeNow, endHeld), a lifecycle recheck
// refuses the launch and leaves the process as it stands (runWake, relaunchRefused), a tree's
// close drops the queued launch with the process (letGo), and a retry of a launch can run the
// claim's budget out and fail it (launchNow, fail). Where nothing is queued — suspended, failed
// — the row says so and names what a person has to do, rather than promising a resume or a
// retry that may never come.
//
// No row says the stalled process was stopped, however plainly its end let that process go: the
// runtime can refuse a stop and the machine goes on anyway (supervise's endHeld, fail,
// stopPrevious), so what became of that process is one sentence written from the machine's own
// record of it (stallStopClause, stallProcessFate).
//
// The launch failures of a replacement that never started are charged, as every launch failure
// is, and the one end that carries them says so rather than leaving the owner a sentence its
// budgets contradict.
var stallBackstopEnds = map[supervise.ProcessEnd]stallEndWords{
	supervise.EndRelaunched: {
		opening: "the daemon ended the " + stallRoleWord + "'s turn and started a new process in its place",
		end:     "its session was brought back on a new process",
		charged: stallNothingCharged,
		process: stallProcessLetGo},
	supervise.EndRelaunchParked: {
		opening: "the daemon took back the " + stallRoleWord + "'s task and queued the relaunch that replaces its stalled process",
		end: "the launch that replaces its stalled process is queued in the daemon's wake pacing and is due at " + stallDueWord +
			": the stalled process runs on until that launch's turn, and that turn starts nothing at all if a suspension waiting for this claim ends it there, if the tree lifecycle recheck every launch makes first refuses this one, or if the tree's close has dropped the queued launch",
		charged: stallNothingCharged,
		process: stallProcessKept},
	supervise.EndRelaunchPending: {
		opening: "the daemon ended the " + stallRoleWord + "'s turn and took its stalled process off the claim",
		end: "the launch that replaces that process could not be written, so nothing runs for the claim: the daemon tries that launch again at its probe interval, " +
			"or at its next start where its own stop cut the launch short, and a suspension waiting for this claim, a tree cleanup already reserved or the claim's launch budget running out leaves it suspended or failed with nothing in its place instead",
		charged: stallNothingCharged,
		process: stallProcessLetGo},
	supervise.EndSuspended: {
		opening: "the daemon ended the " + stallRoleWord + "'s turn in the suspension that was already waiting for it",
		end: "a suspension that was already waiting for this turn ran instead of a replacement, and the claim is suspended with nothing queued for it: " +
			"what it waits on is a resume, or the close of its tree, which retires it",
		charged: stallNothingCharged,
		process: stallProcessLetGo},
	supervise.EndFailed: {
		opening: "the daemon ended the " + stallRoleWord + "'s turn and could not replace its stalled process, which failed the claim",
		end: "every try at the replacement was refused, which ran the claim's launch budget out, so the claim is failed and the daemon starts nothing for it: " +
			"what it waits on is a retry, or the close of its tree, which retires it",
		charged: "the stall itself charged the claim nothing; the launch failures of the replacement that could not be started are what it was charged.",
		process: stallProcessLetGo},
}

// stallBackstopEnd is what the notice says of one end: its opening, the clause that says what is
// running for the claim now and what it waits on, the charge clause, and what the end did with
// the claim's process (stallBackstopEnds).
//
// An end the table has no row for is named rather than described, and claims nothing of what the
// daemon did: a daemon whose machines reach an end its notices have no words for still tells the
// owner that its stall backstop acted on this turn and what the record said, which is what R5
// asks of it, and a reader who meets this sentence has the value to look up. It says nothing of
// the process either (stallFateUnsaid), since an end this daemon cannot word is one it cannot
// say let a process go. The walk above keeps all of it unreachable for every end this daemon
// declares.
func stallBackstopEnd(made supervise.ProcessEnd) stallEndWords {
	if words, ok := stallBackstopEnds[made]; ok {
		return words
	}
	return stallEndWords{
		opening: "the daemon's stall backstop acted on the " + stallRoleWord + "'s stalled turn",
		end:     fmt.Sprintf("what became of it is recorded as %q, which this daemon has no sentence for", made),
		charged: stallNothingCharged}
}

// stallInstant is a moment of the record as the notice and the log line write it, and "no time
// the daemon recorded" for a field the record does not hold.
func stallInstant(at time.Time) string {
	if at.IsZero() {
		return "no time the daemon recorded"
	}
	return at.UTC().Format(time.RFC3339)
}
