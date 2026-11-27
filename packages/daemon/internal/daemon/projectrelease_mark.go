package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// projectRelease is the daemon-wide mark of a project release (dispatch://INFRA-3583 R7): while
// it is held, the daemon starts nothing of its project until its process ends. Every layer that
// could make a pod or start or prompt a role reads it:
//
//   - the launch check every machine launch makes, the controller's included (launchGatedStore);
//   - the Sandbox runtime itself (sandbox.Options.ProjectReleaseHolds), which creates no Sandbox,
//     patches none to Running, makes no repository cache or probe and replaces no pod;
//   - the machines, which hand no claim its task (supervise.Deps.ProjectReleaseHolds);
//   - the daemon's periodic work: the orphan sweep and its cache sweep, the pod update sweep, the
//     stale-plugin refresh and the unfinished-launch relaunch (supervision.sweepOnce,
//     relaunchUnfinished), the idle and unowned ticks (workflowRuntime.idlePass), the controller
//     keeper (controllerKeeper.keep) and the wakes (workflowRuntime.wakePass);
//   - the outbox, which answers a wait for every row that would start or prompt a role
//     (record.HeldByProjectRelease);
//   - the operator's routes that start or prompt a role (api.Options.ProjectReleaseHolds).
//
// The release sets it before its record commits (store.StartProjectRelease) and before that
// transaction reads the claims it stops, and every role start reads it once more after its
// claim's `launching` write, just before the launcher starts the role (the Sandbox runtime's
// launch), so a launch either sees the mark or is among the claims the release stops. It clears
// the mark only when its start answers an error before that commit: its request ended while it
// waited for a pass of the sweep (awaitSweep), or the commit failed. Otherwise only the process's
// end clears it, and the next boot, which ends the record, starts the claims the release stopped.
// The zero value holds nothing, and so does a nil mark: a test that builds a part of the daemon by
// hand reads every start as free.
//
// A pass of the sweep reads the mark, and runs the acts it holds, holding the sweep's turn
// (sweep), and the release takes that turn once it has set the mark (awaitSweep): a pass that read
// the mark clear before it was set finishes those acts before the release records itself, reads,
// asks or deletes anything, and every pass after reads the mark set and skips them. So no pod
// update round writes the record of a replacement (recordRoundReplacement) after the release's
// settle has read the records of its pod (projectReleaser.settleReplacements), and none ends as a
// replacement made over a pod the release deleted. A release whose request ends while it waits
// for that turn clears the mark and records nothing (projectReleaser.Start).
type projectRelease struct {
	marked atomic.Bool
	// hook is the release once its record has committed (recorded), nil before and once unmark
	// cleared the mark: its record id, which names the release's own ask round (ownRound), and its
	// side of those asks, which the outbox reads as it publishes one (runningHook). One pointer
	// holds all of it, and the outbox reads it once per ask, so the round it matched is the hook it
	// uses.
	hook atomic.Pointer[releaseAskHook]
	// finishedActs is set once the release finished its acts (finish, finished): its round is then
	// no round the outbox publishes an ask under without the claim-round read (askRound), while
	// the mark itself stays held.
	finishedActs atomic.Bool
	// unmarked runs once unmark has cleared the mark: the machines' send of every task the mark
	// held back (supervisor.sendHeldTasks). openSupervision sets it once, at boot, before anything
	// can set the mark; nil runs nothing.
	unmarked func()
	// sweeping is the sweep's turn, a one-slot semaphore that sweepingOnce makes at its first use
	// (sweepTurn), so the zero value needs none of its own. A pass of the supervision's sweep holds
	// it across its read of the mark and the acts the mark holds (sweep), and the release takes it
	// and gives it back once it has set the mark (awaitSweep). It is a semaphore rather than a
	// mutex so the release waits for it only while its request lives; a pass waits for it as long
	// as it takes.
	sweepingOnce sync.Once
	sweeping     chan struct{}
}

// mark sets the mark and reports whether this call set it: a process runs one release, so a mark
// already held is a release already running or finished here, and the caller starts no second
// one.
func (r *projectRelease) mark() bool {
	return r.marked.CompareAndSwap(false, true)
}

// recorded names the release once StartProjectRelease committed its record, so the outbox lets
// that release's own asks through (ownRound, askRound), and installs hook, the release's side of
// those asks, with the round, so every ask the outbox publishes under that round finds it.
func (r *projectRelease) recorded(hook releaseAskHook) {
	r.hook.Store(&hook)
}

// releaseAskHook is the release of this process once its record committed, as the outbox needs
// it as it publishes one of its asks (outbox.publishReleaseAsk): id, the release's record id,
// which names the round of its asks (projectReleaseRound); bound, until which an ask whose role's
// topic no live session holds waits for one; and reached, which tells the release that its ask to
// a claim reached a session of the claim's role (releaseRun.reach).
type releaseAskHook struct {
	id      int64
	bound   time.Time
	reached func(claim.Token)
}

// recordedHook is the release's hook once its record committed (recorded), nil on a nil mark,
// before the commit and once unmark cleared the mark.
func (r *projectRelease) recordedHook() *releaseAskHook {
	if r == nil {
		return nil
	}
	return r.hook.Load()
}

// runningHook is the hook of the release this process runs (recorded), nil on a nil mark, before
// its record committed, once unmark cleared the mark and once the release finished (finish): the
// one read the outbox makes of the release for an ask, which gives it both the round to match
// (projectReleaseRound of id) and the bound and reached it then uses.
func (r *projectRelease) runningHook() *releaseAskHook {
	if r.finished() || !r.held() {
		return nil
	}
	return r.recordedHook()
}

// finish records that the release finished its acts: from then on an ask of its round is
// published only where the claim records that round, which no claim does (askRound), so none is
// published. The mark stays held until the process ends.
func (r *projectRelease) finish() {
	r.finishedActs.Store(true)
}

// finished is whether the release of this process has finished its acts: the one flag that says
// so, which the release itself reads for the acts it no longer does (projectReleaser.current,
// record, the authorize of a pod's delete). A nil mark has finished nothing.
func (r *projectRelease) finished() bool {
	return r != nil && r.finishedActs.Load()
}

// unmark clears the mark of a release whose start answered an error, the one way the mark is
// cleared before the process ends: nothing of that release ran, so the mark holds nothing of the
// project for it. Its record did not commit, but for one case: a commit whose outcome the store
// had still not said when the start's context ended (store.ErrCommitOutcomeUnknown). Such a record
// may have committed, and then, with no release running it, it refuses every pause until the
// operator's retry runs a release whose finish leaves the newest record finished
// (store.ProjectReleasingError), and every idle release until the next boot ends it
// (store.Store.EndProjectReleasesAtBoot); and the claims that the idle releases it took over had
// stopped get no backstop (armTakenOver). A task a machine held back while the mark stood is sent
// at once (unmarked) rather than at the claim's next event; a held outbox row runs at its next
// read, within releaseHoldSpacing, a launch the launch check refused at its probe-interval retry,
// and the periodic work at its next tick.
func (r *projectRelease) unmark() {
	r.hook.Store(nil)
	r.marked.Store(false)
	if r.unmarked != nil {
		r.unmarked()
	}
}

// held is whether the project release holds every start now. A nil mark holds nothing.
func (r *projectRelease) held() bool {
	return r != nil && r.marked.Load()
}

// sweep runs acts, the acts of a pass of the supervision's sweep that the mark holds
// (supervision.sweepOnce), unless the mark is held: it reads the mark and runs acts holding the
// sweep's turn (sweepTurn), so a release that sets the mark while acts run waits for them
// (awaitSweep). A nil mark holds nothing, and acts run.
func (r *projectRelease) sweep(acts func()) {
	if r == nil {
		acts()
		return
	}
	turn := r.sweepTurn()
	turn <- struct{}{}
	defer func() { <-turn }()
	if !r.marked.Load() {
		acts()
	}
}

// awaitSweep waits for a pass of the sweep that runs the acts the mark holds to finish them
// (sweep), or for ctx, the release's request, to end, and answers ctx's error then: the release
// calls it once it has set the mark (projectReleaser.Start), so when it answers nil the pass that
// read the mark clear has ended its acts, and every pass after reads the mark set. It gives the
// sweep's turn back as soon as it has taken it: taking it is the wait.
func (r *projectRelease) awaitSweep(ctx context.Context) error {
	turn := r.sweepTurn()
	select {
	case turn <- struct{}{}:
		<-turn
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// sweepTurn is the sweep's turn (sweeping), made at its first use: a send takes the turn, and a
// receive gives it back.
func (r *projectRelease) sweepTurn() chan struct{} {
	r.sweepingOnce.Do(func() { r.sweeping = make(chan struct{}, 1) })
	return r.sweeping
}

// ownRound is the round the release of this process asks its roles under (projectReleaseRound),
// running or finished, "" while no release is marked or its record has not committed yet: the
// one round of update asks the outbox runs while the mark is held (record.HeldByProjectRelease).
func (r *projectRelease) ownRound() string {
	hook := r.recordedHook()
	if hook == nil || !r.held() {
		return ""
	}
	return projectReleaseRound(hook.id)
}

// askRound is ownRound while the release runs, and "" once it finished (finish): the round whose
// asks the outbox publishes without the claim-round read, since only a running release holds the
// answers to them (projectReleaser.AskRound, which answers the same). An ask of the release's round
// that runs after its finish meets the claim-round read, and since no claim records that round it
// finishes without a publish.
func (r *projectRelease) askRound() string {
	if hook := r.runningHook(); hook != nil {
		return projectReleaseRound(hook.id)
	}
	return ""
}

// projectReleaseRound is the round a project release's ask rides (record.UpdateAsk.Round), named
// by the release's record id so it is never the round of a pod update.
func projectReleaseRound(id int64) string {
	return "release:" + strconv.FormatInt(id, 10)
}

// errProjectReleased is the refusal of a start the project release holds: the launch check's, for
// every claim the controller's included (launchGatedStore), and the outbox's wait for a row that
// would start or prompt a role. It is a wait (wait.ErrWaiting), since the start is due again once
// the daemon's next boot has ended the release: a machine whose relaunch it refuses keeps the
// claim waiting uncharged (supervise's relaunchRefused), and a row it holds is put back unrun,
// with no attempt counted, and read again every releaseHoldSpacing (outbox.holdForRelease). The
// wait is routine (routineWaiting): the operator who released the project knows what it holds,
// so a row held for the whole bound stays at debug after its first line.
var errProjectReleased error = routineWaiting{err: wait.New(api.ProjectReleaseHeldSentence)}

// roleTakenMargin is how long past config.ClaimFirstTurnBound a session that stands ready is given
// to hold its role: the plugin arms the bound once the daemon has answered its ready, and its
// claim of the name then has to reach the Envoy listener.
const roleTakenMargin = 15 * time.Second

// releaseStoppedClaims is the claims this process's boot read that a project release stopped
// and whose sessions do not hold their roles yet (dispatch://INFRA-3583 R8). The boot starts such
// a claim again through an outbox start, which the wake pacing lets out in its turn (workflow's
// booted), and from that start until its session holds its role the claim reads as any claim
// coming up does. So the set is what tells it from a claim no release stopped: a delivery waits
// for the first (outbox.bootStartComing), and one for the second is ended when nobody holds its
// role.
//
// Every boot builds the set again from the store (readReleaseStoppedClaims), so a claim an earlier
// boot started is in it after a restart for as long as its session has not held its role. A claim
// leaves the set once its session holds its role: it reports working
// (workflowRuntime.claimDurableState), or it has stood ready or idle for
// config.ClaimFirstTurnBound and roleTakenMargin (comingBack). It leaves it too once it fails,
// retires, or is suspended by any source but the project release. A nil set holds no claim: an
// outbox or a runtime a test built by hand reads every claim as one no release stopped.
type releaseStoppedClaims struct {
	mu     sync.Mutex
	tokens map[claim.Token]struct{}
}

// readReleaseStoppedClaims builds the set at the boot from claims, the boot's read of the store,
// and the newest project release of project the store holds (store.Store.ReadProjectRelease),
// which the boot has ended by then (store.EndProjectReleasesAtBoot). A release whose result
// cannot be read is logged, and the claims stored suspended by a release are in the set
// whatever that result says (newReleaseStoppedClaims).
func readReleaseStoppedClaims(ctx context.Context, st *store.Store, project string, claims []supervise.Claim, log *slog.Logger) (*releaseStoppedClaims, error) {
	release, found, err := st.ReadProjectRelease(ctx, project)
	if err != nil {
		return nil, fmt.Errorf("read the newest project release of %s: %w", project, err)
	}
	stopSet := map[claim.Token]bool{}
	if found {
		view, err := api.ProjectReleaseResponseOf(release)
		if err != nil {
			log.Warn("legion daemon: the boot could not read which claims the newest project release stopped; it reads only the claims still stored suspended by it",
				"release", release.ID, "error", err)
		}
		for _, line := range view.Result.Claims {
			stopSet[line.Token] = true
		}
	}
	return newReleaseStoppedClaims(claims, stopSet, release.EndedAt), nil
}

// newReleaseStoppedClaims is the set over claims, the boot's read of the store. A claim is in it
// when:
//
//   - it is stored suspended by a project release (supervise.Claim.ProjectReleased), whose start
//     is still to run;
//   - or it is in stopSet, the claims the newest release stopped (api.ReleasedClaim), and reads
//     started with no turn begun since that release ended at endedAt (Claim.LastTurnStart): a
//     claim an earlier boot started after the release, whose session this boot finds still coming
//     up or standing ready (supervise.StartedWithoutTurn).
func newReleaseStoppedClaims(claims []supervise.Claim, stopSet map[claim.Token]bool, endedAt time.Time) *releaseStoppedClaims {
	stopped := &releaseStoppedClaims{tokens: map[claim.Token]struct{}{}}
	for _, c := range claims {
		if c.ProjectReleased() || stopSet[c.Token] && supervise.StartedWithoutTurn(c.State) && !c.LastTurnStart.After(endedAt) {
			stopped.tokens[c.Token] = struct{}{}
		}
	}
	return stopped
}

// comingBack is whether a delivery for held's claim waits for its session to hold its role: the
// claim is in the set and started, and its session does not hold the role yet. One coming up
// waits with no moment named (until is zero). One that stands ready or idle waits until
// config.ClaimFirstTurnBound and roleTakenMargin after its ready (Claim.LastReady), read on now:
// its session takes the role at its first turn, which the claim reports as working, or at that
// bound (supervise.SaidReady), so from that moment the claim leaves the set and waits no longer. A
// claim still stored suspended by the project release has had no start yet: it keeps its place and
// waits on no reading of this set (bootStartComing reads it). Any other claim — working, failed,
// retired, or suspended by another source — leaves the set, so a later start of it is a start
// like any other.
//
// A claim whose process ended while it stood ready, before any reading of the set found that ready
// past the bound, keeps its place, and a delivery for it waits for its next ready: neither the set
// nor the claim's record says how long that ready stood. now is read only for a claim that stands
// ready or idle.
func (s *releaseStoppedClaims) comingBack(held supervise.ClaimView, now func() time.Time) (until time.Time, waits bool) {
	if s == nil {
		return time.Time{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, stopped := s.tokens[held.Token]; !stopped || held.ProjectReleased() {
		return time.Time{}, false
	}
	switch held.State {
	case supervise.StateReady, supervise.StateIdle:
		if until := held.LastReady.Add(config.ClaimFirstTurnBound + roleTakenMargin); now().Before(until) {
			return until, true
		}
	default:
		if supervise.StartedWithoutTurn(held.State) {
			return time.Time{}, true
		}
	}
	delete(s.tokens, held.Token)
	return time.Time{}, false
}

// settled takes token's claim out of the set: its session holds its role, or the claim failed,
// and either way no delivery waits for the boot's start of it any longer.
func (s *releaseStoppedClaims) settled(token claim.Token) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tokens, token)
}
