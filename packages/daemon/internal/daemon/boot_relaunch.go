package daemon

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The boot's relaunch of the launches the previous daemon did not finish: the claims its restore
// put back to launch_uncertain with no process (supervisor.restore), each released to queued once
// the boot's orphan reconciliation proves no predecessor runs, and launched again in the order its
// work was owed — and, in the same order, of the deaths that daemon recorded, whose relaunches it
// held in the same pacing (supervise.RecordedDeath).

// bootRelaunch is one claim launchUnfinished relaunches: its machine, the turn reserved for it in
// the wake pacing (the zero time for none yet), and how its release ended.
//
// death is whether the relaunch is of the death the claim's record charges rather than of an
// unfinished launch: such a claim records its process, so it has no uncertain launch to release
// and reads released from the start, and generation is the claim's generation its replay names.
type bootRelaunch struct {
	token      claim.Token
	m          *supervise.Machine
	turn       time.Time
	release    releaseOutcome
	death      bool
	generation uint64
}

// releaseOutcome is how the release of one unfinished launch ended (releaseUnfinishedClaim).
type releaseOutcome int

const (
	// releaseSettled is a claim that left launch_uncertain by another way, its old process's
	// hello or a stop, and is not relaunched; and a release the daemon's stop cut short
	// (supervisor.decisionFailed answering false), whose claim is still launch_uncertain for the
	// next boot to take up.
	releaseSettled releaseOutcome = iota
	// releaseQueued is a claim released to queued with no process, relaunched now.
	releaseQueued
	// releaseRefused is a release the store did not take, retried at a later sweep.
	releaseRefused
)

// waitingLaunches is tokens as launchUnfinished takes them: each with no turn reserved yet.
func waitingLaunches(tokens []claim.Token) map[claim.Token]time.Time {
	waiting := make(map[claim.Token]time.Time, len(tokens))
	for _, token := range tokens {
		waiting[token] = time.Time{}
	}
	return waiting
}

// launchUnfinished starts only claims that remain queued with no locator, each on a goroutine of
// its own, and returns once every one has: one relaunch never waits out another's launch, which
// under a Sandbox can take minutes. A still-live old pane can reconnect while reconciliation was
// uncertain; its hello changes the state before a later retry, and it must never be joined by a
// second pane. waiting is the claims to relaunch, each with the turn a sweep before reserved for
// it, the zero time for none. It returns the claims whose release the store did not take, outside
// the daemon's stop, each with its turn: each is still launch_uncertain, in memory as in the
// store, and nothing else in this boot releases it, so the caller retries it with what it returned
// (relaunchUnfinished). ctx is the relaunches' context; each relaunch derives a context of its own
// from it.
//
// It runs in three steps. Every claim's uncertain launch is released first, each on a goroutine of
// its own. Then the waiting claims are handed turns in the wake pacing in the order their work was
// owed (reserveUnfinishedTurns), and only then is every released launch posted at once, each
// carrying its turn (supervise.RequestSpawn.Turn). Most of these claims were
// waiting in the previous daemon's pacing when it stopped, and a wake is memory only, so this is
// the queue that daemon was draining, rebuilt: reserved instead as each goroutine reached the
// gate, its order was chance, and a claim that had waited most of an hour went back to a random
// place behind hundreds at each restart (dispatch://INFRA-5482).
//
// deaths is the claims whose record says their process died (recordedDeaths), whose relaunches
// were the rest of that queue: each still standing over its dead process (recordedDeath) joins
// the turns' one order, and its relaunch is posted with its turn as the death's replay
// (supervise.RecordedDeath.Turn). Reserved apart, ahead of the pass, each took the first turns of
// the restart's queue, ahead of claims whose work had waited longer (dispatch://INFRA-5472). The
// boot passes them to its first pass alone; none is ever refused, so none is retried.
func (s *supervision) launchUnfinished(ctx context.Context, waiting map[claim.Token]time.Time, deaths ...claim.Token) map[claim.Token]time.Time {
	launches := make([]*bootRelaunch, 0, len(waiting)+len(deaths))
	for token, turn := range waiting {
		if m, ok := s.supervisor.Machine(token); ok {
			launches = append(launches, &bootRelaunch{token: token, m: m, turn: turn})
		}
	}
	var releasing sync.WaitGroup
	for _, l := range launches {
		releasing.Add(1)
		go func() {
			defer releasing.Done()
			// Each relaunch runs on a goroutine of its own beside the others, so each takes a
			// hold mark of its own rather than ctx's.
			l.release = s.releaseUnfinishedClaim(store.ForConcurrentWorker(ctx), l.token, l.m)
		}()
	}
	releasing.Wait()
	launches = slices.DeleteFunc(launches, func(l *bootRelaunch) bool { return l.release == releaseSettled })
	for _, token := range deaths {
		if l, ok := s.recordedDeath(token); ok {
			launches = append(launches, l)
		}
	}
	s.reserveUnfinishedTurns(launches)
	var launching sync.WaitGroup
	refused := map[claim.Token]time.Time{}
	for _, l := range launches {
		if l.release == releaseRefused {
			refused[l.token] = l.turn
			continue
		}
		launching.Add(1)
		go func() {
			defer launching.Done()
			var ev supervise.Event = supervise.RequestSpawn{Claim: l.token, Turn: l.turn}
			if l.death {
				ev = supervise.RecordedDeath{Claim: l.token, Generation: l.generation, Turn: l.turn}
			} else {
				s.log.Warn("supervise: launching again a launch the previous daemon did not finish", "claim", l.token)
			}
			if err := l.m.Handle(store.ForConcurrentWorker(ctx), ev); err != nil {
				s.supervisor.decisionFailed(l.token, fmt.Sprintf("%T", ev), err, "supervise: launch again a launch the previous daemon held")
			}
		}()
	}
	launching.Wait()
	return refused
}

// releaseUnfinishedClaim releases one unfinished launch on ctx, and reports how the release ended
// (launchUnfinished).
func (s *supervision) releaseUnfinishedClaim(ctx context.Context, token claim.Token, m *supervise.Machine) releaseOutcome {
	released, err := m.ReleaseUncertainLaunch(ctx)
	if err != nil {
		if s.supervisor.decisionFailed(token, "release of an uncertain launch", err,
			"supervise: release an uncertain launch; retrying at the next orphan sweep") {
			return releaseRefused
		}
		return releaseSettled
	}
	c := m.Claim()
	if !released || c.State != supervise.StateQueued || c.Locator != nil {
		s.log.Info("supervise: unrecorded launch settled without relaunch", "claim", token, "state", c.State)
		return releaseSettled
	}
	return releaseQueued
}

// recordedDeaths is the claims of claims, as the boot restored them, whose record says the process
// they record died: a live claim with its death charged (supervise.Claim.DeathCharged) and a
// locator. The daemon before this one judged each process gone and charged its death, and the
// relaunch it raised for it lived in its memory alone, a wake the pacing parked, which the restart
// lost (dispatch://INFRA-5472). The boot relaunches each beside its unfinished launches
// (relaunchUnfinished), rather than when the runtime next reports the death: in a pod that lives
// on — its roles container restarted — that report waits for the pod's launcher to reconnect, a
// probe interval after boot, which put every such relaunch behind the whole of the boot's wave.
func recordedDeaths(claims []supervise.Claim) []claim.Token {
	live := supervise.LiveStates()
	var deaths []claim.Token
	for _, c := range claims {
		if c.DeathCharged && c.Locator != nil && slices.Contains(live, c.State) {
			deaths = append(deaths, c.Token)
		}
	}
	return deaths
}

// recordedDeath is token's relaunch for the death its record charges, as the boot's pass takes it
// (launchUnfinished), and false for a claim that no longer stands over that death as the machine
// holds it now: a report of the death that came first and raised its relaunch already (a wake
// waiting), a launch since, a report that found the process running, or a suspension held for the
// claim, whose end the death's own report decides. The replay decides each of those nothing
// (supervise's relaunchRecordedDeath), so the pass reserves such a claim no turn, which would be
// left spent and put every wake behind it an interval late. A report of the death decided before
// this read keeps the turn it reserved itself, which can run ahead of the owed order. One decided
// after this read and before the pass reserves its turns takes a turn ahead of the pass's, so the
// replay moves nothing and the turn the pass handed it is left spent. One decided after the pass
// reserved parks its relaunch behind every turn the pass handed out, and the replay moves that
// wake to the turn the pass handed it (supervise's takeEarlierTurn), leaving the report's later
// turn spent.
func (s *supervision) recordedDeath(token claim.Token) (*bootRelaunch, bool) {
	m, ok := s.supervisor.Machine(token)
	if !ok {
		return nil, false
	}
	v := m.View()
	c := v.Claim
	if !c.DeathCharged || c.Locator == nil || !slices.Contains(supervise.LiveStates(), c.State) || c.SuspensionHeld || v.WakeWaiting != nil {
		return nil, false
	}
	return &bootRelaunch{token: token, m: m, release: releaseQueued, death: true, generation: c.Generation}, true
}

// reserveUnfinishedTurns hands a turn in the wake pacing to each of launches whose launch the gate
// would hold (supervise.PacedLaunch), in the order their work was owed (owedSince): the claim owed
// longest takes the earliest. The turns handed out are the pool of the turns these launches still
// hold that are to come, and one turn reserved afresh for each launch that holds none, or holds one
// that has passed. A launch the gate would not hold — the pacing off, a project pause holding it
// (the pause's gate takes no turn), the project controller's — is handed none, and launches as its
// gate decides.
//
// A claim whose release the store refused keeps the turn it was handed for the sweep that retries
// it, so a refusal that clears costs the claim no place: its turn goes back into the pool, and each
// claim of the retry takes its place in owed order. The pool is what keeps that order when turns
// pass: a kept turn that has passed is replaced by a fresh one at the pacing's tail, but the pool
// sorts the fresh with the kept and hands the earliest to the claim owed longest, so the claims
// owed longest do not launch last, and a passed turn is never run at once, which would let every
// claim a store outage held launch together outside the pace. A refused claim whose turn has
// passed is handed none and reserved none while it stays refused, so a refusal that never clears
// does not take a turn of the pacing at every sweep; its release, when it comes, reserves it one.
// The order of the reservations does not matter, since the pool is sorted before it is handed out.
// A turn reserved while the model gateway's hold stands is reserved from the hold's end
// (supervise.WakePace.Turn), and a kept turn the hold has come over since is reserved again at
// the gate (reserveWake).
func (s *supervision) reserveUnfinishedTurns(launches []*bootRelaunch) {
	deps := &s.supervisor.deps
	now := deps.Clock.Now()
	type owed struct {
		launch *bootRelaunch
		since  time.Time
	}
	var (
		queue []owed
		pool  []time.Time
	)
	for _, l := range launches {
		c := l.m.View().Claim
		if !supervise.PacedLaunch(deps, &c) {
			l.turn = time.Time{}
			continue
		}
		turn := l.turn
		if !turn.After(now) {
			if l.release == releaseRefused && !turn.IsZero() {
				continue
			}
			turn = deps.WakePace.Turn(l.token, deps.Clock.Now())
		}
		pool = append(pool, turn)
		queue = append(queue, owed{launch: l, since: owedSince(c)})
	}
	slices.SortFunc(pool, time.Time.Compare)
	slices.SortStableFunc(queue, func(a, b owed) int {
		return cmp.Or(compareOwed(a.since, b.since), cmp.Compare(a.launch.token, b.launch.token))
	})
	for i, waiting := range queue {
		waiting.launch.turn = pool[i]
	}
}

// owedSince is when the work an unfinished launch is for was first owed: the moment the task that
// hands the claim work was queued to it (supervise.HoldsWork), and the zero time for a claim that
// holds none — no delivery, the daemon's own notice of its relaunch, or a task of news alone,
// none of which is work a person waits on.
func owedSince(c supervise.Claim) time.Time {
	if !supervise.HoldsWork(c.Pending) {
		return time.Time{}
	}
	return c.Pending.QueuedAt
}

// compareOwed orders two owedSince moments, the earlier first, and a claim owed nothing after every
// claim owed work: its launch holds up no task.
func compareOwed(a, b time.Time) int {
	switch {
	case a.IsZero() && b.IsZero():
		return 0
	case a.IsZero():
		return 1
	case b.IsZero():
		return -1
	}
	return a.Compare(b)
}

// relaunchUnfinished launches tokens' unfinished launches on a goroutine of its own, never on the
// boot: at once when the boot reconciliation succeeded (reconciled), then on the normal orphan-sweep
// cadence, until none is left or the daemon stops. A relaunched worker reaches the daemon only once
// the daemon is up — a Sandbox pod's launcher dials it through its Service, which routes only to a
// pod whose probes pass, so only once the API answers /healthz, and a shim's hello is resolved only
// once restoration has closed — so a relaunch the boot waited on could not register: it spent its
// whole launch budget, launch_failure_limit × worker_boot_timeout_seconds, with the API unanswered.
//
// The pre-restart process of such a launch, when one still runs, may now say hello before its
// relaunch writes the next generation, and be admitted: its claim, launch_uncertain or queued until
// then, ignores the hello and refuses its registration, and at the next generation the fence drops
// it. Its connection holds the claim's stream slot only while that process lives, and the relaunch
// ends it before it starts the next generation: a tmux boot's reconciliation kills every pane no
// claim records, and a Sandbox relaunch stops its launcher's earlier generation first. Once the next
// generation is written, the old process's boot token resolves stale.
//
// Until the boot reconciliation has succeeded, each sweep retries it first — each reconciliation
// has the bounded retry above, and only a success releases these claims to launch; once it has, a
// sweep retries only the releases the store did not take, with the turns they were reserved
// (launchUnfinished), since the reconciliation's proof that no predecessor runs still holds.
//
// deaths is the claims whose record says their process died (recordedDeaths). They join the first
// pass, in one order with the unfinished launches. A recorded death needs no reconciliation, since
// its claim records the process it replaces, so where the boot's reconciliation failed their pass
// runs at once all the same, alone, in their own owed order. The unfinished launches cannot launch
// before a sweep reconciles, and the sweep that does reserves their turns behind every death turn
// still to come: on that path a death goes ahead of an unfinished launch owed before it, which is
// the price of not holding a relaunch that needs no reconciliation behind one that may never come.
func (s *supervision) relaunchUnfinished(tokens, deaths []claim.Token, reconciled bool) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		// The relaunches run on a goroutine of their own beside the machines and the orphan sweep,
		// so this goroutine takes a hold mark of its own.
		relaunching := store.ForConcurrentWorker(s.supervisor.ctx)
		waiting := waitingLaunches(tokens)
		if reconciled {
			waiting = s.launchUnfinished(relaunching, waiting, deaths...)
		} else if len(deaths) > 0 {
			s.launchUnfinished(relaunching, nil, deaths...)
		}
		ticker := time.NewTicker(s.plan.orphanSweep)
		defer ticker.Stop()
		for len(waiting) > 0 {
			select {
			case <-s.supervisor.ctx.Done():
				return
			case <-ticker.C:
			}
			if !reconciled {
				if reconciled = s.reconcileBootOrphans(relaunching); !reconciled {
					continue
				}
			}
			waiting = s.launchUnfinished(relaunching, waiting)
		}
	}()
}
