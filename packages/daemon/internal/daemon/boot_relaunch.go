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
// work was owed.

// bootRelaunch is one claim launchUnfinished relaunches: its machine, the turn reserved for it in
// the wake pacing (the zero time for none yet), and how its release ended.
type bootRelaunch struct {
	token   claim.Token
	m       *supervise.Machine
	turn    time.Time
	release releaseOutcome
}

// releaseOutcome is how the release of one unfinished launch ended (releaseUnfinishedClaim).
type releaseOutcome int

const (
	// releaseSettled is a claim that left launch_uncertain by another way, its old process's
	// hello or a stop, and is not relaunched.
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
// its own. Then the waiting claims' turns are reserved in the wake pacing one at a time, in the
// order their work was owed (reserveUnfinishedTurns), and only then is every released launch
// posted at once, each carrying its turn (supervise.RequestSpawn.Turn). Most of these claims were
// waiting in the previous daemon's pacing when it stopped, and a wake is memory only, so this is
// the queue that daemon was draining, rebuilt: reserved instead as each goroutine reached the
// gate, its order was chance, and a claim that had waited most of an hour went back to a random
// place behind hundreds at each restart (dispatch://INFRA-5482).
func (s *supervision) launchUnfinished(ctx context.Context, waiting map[claim.Token]time.Time) map[claim.Token]time.Time {
	launches := make([]*bootRelaunch, 0, len(waiting))
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
			s.log.Warn("supervise: launching again a launch the previous daemon did not finish", "claim", l.token)
			spawn := supervise.RequestSpawn{Claim: l.token, Turn: l.turn}
			if err := l.m.Handle(store.ForConcurrentWorker(ctx), spawn); err != nil {
				s.supervisor.decisionFailed(l.token, fmt.Sprintf("%T", spawn), err, "supervise: launch an unfinished launch again")
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

// reserveUnfinishedTurns hands a turn in the wake pacing to each of launches whose launch the gate
// would hold (supervise.PacedLaunch), in the order their work was owed (owedSince): the claim owed
// longest takes the earliest. The turns handed out are the pool of the turns these launches still
// hold that are to come, and one turn reserved afresh for each launch that holds none, or holds one
// that has passed. A launch the gate would not hold — the pacing off, a project pause holding it
// (the pause's gate takes no turn), the project controller's — is handed none, and launches as its
// gate decides.
//
// A claim whose release the store refused keeps the turn it was handed for the sweep that retries
// it, so a refusal that clears costs the claim no place: its turn goes back into the pool and, its
// work owed longer, it takes the pool's earliest turn again. The pool is what keeps that order when
// turns pass: a kept turn that has passed is replaced by a fresh one at the pacing's tail, but the
// pool sorts the fresh with the kept and hands the earliest to the claim owed longest, so the
// claims owed longest do not launch last, and a passed turn is never run at once, which would let
// every claim a store outage held launch together outside the pace. A refused claim whose turn
// has passed is handed none and reserved none while it stays refused, so a refusal that never
// clears does not take a turn of the pacing at every sweep; its release, when it comes, reserves
// it one.
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
		fresh int
	)
	for _, l := range launches {
		c := l.m.View().Claim
		if !supervise.PacedLaunch(deps, &c) {
			l.turn = time.Time{}
			continue
		}
		switch {
		case l.turn.After(now):
			pool = append(pool, l.turn)
		case l.release == releaseRefused && !l.turn.IsZero():
			continue
		default:
			fresh++
		}
		queue = append(queue, owed{launch: l, since: owedSince(c)})
	}
	for range fresh {
		pool = append(pool, deps.WakePace.Reserve(deps.Clock.Now()))
	}
	slices.SortFunc(pool, time.Time.Compare)
	slices.SortStableFunc(queue, func(a, b owed) int {
		return cmp.Or(compareOwed(a.since, b.since), cmp.Compare(a.launch.token, b.launch.token))
	})
	for i, waiting := range queue {
		waiting.launch.turn = pool[i]
	}
}

// owedSince is when the work an unfinished launch is for was first owed: the moment the task the
// claim holds was queued to it, and the zero time for a claim that holds none.
func owedSince(c supervise.Claim) time.Time {
	if c.Pending == nil {
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
func (s *supervision) relaunchUnfinished(tokens []claim.Token, reconciled bool) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		// The relaunches run on a goroutine of their own beside the machines and the orphan sweep,
		// so this goroutine takes a hold mark of its own.
		relaunching := store.ForConcurrentWorker(s.supervisor.ctx)
		waiting := waitingLaunches(tokens)
		if reconciled {
			waiting = s.launchUnfinished(relaunching, waiting)
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
