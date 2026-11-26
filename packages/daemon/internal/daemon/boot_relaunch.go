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
	"github.com/sjawhar/legion/daemon/internal/wakepace"
)

// The boot's relaunch of the launches the previous daemon did not finish: the claims its restore
// put back to launch_uncertain with no process (supervisor.restore), each released to queued once
// the boot's orphan reconciliation proves no predecessor runs, and launched again in the order its
// work was owed — and, ahead of them in the pacing's first queue, of the deaths that daemon
// recorded, whose relaunches it held in the same pacing (supervise.RecordedDeath).

// bootRelaunch is one claim launchUnfinished relaunches: its machine, the place taken for it in
// the wake pacing (nil for none yet), and how its release ended.
//
// death is whether the relaunch is of the end the claim's record decided — a death charged, or an
// end from outside the pod charged nothing — rather than of an unfinished launch: such a claim
// records its process, so it has no uncertain launch to release and reads released from the
// start, and generation is the claim's generation its replay names.
type bootRelaunch struct {
	token      claim.Token
	m          *supervise.Machine
	place      *wakepace.Place
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

// waitingLaunches is tokens as launchUnfinished takes them: each with no place taken yet.
func waitingLaunches(tokens []claim.Token) map[claim.Token]*wakepace.Place {
	waiting := make(map[claim.Token]*wakepace.Place, len(tokens))
	for _, token := range tokens {
		waiting[token] = nil
	}
	return waiting
}

// launchUnfinished starts only claims that remain queued with no locator, each on a goroutine of
// its own, and returns once every one has: one relaunch never waits out another's launch, which
// under a Sandbox can take minutes. A still-live old pane can reconnect while reconciliation was
// uncertain; its hello changes the state before a later retry, and it must never be joined by a
// second pane. waiting is the claims to relaunch, each with the place a sweep before took for it,
// nil for none. It returns the claims whose release the store did not take, outside the daemon's
// stop, each with its place: each is still launch_uncertain, in memory as in the store, and
// nothing else in this boot releases it, so the caller retries it with what it returned
// (relaunchUnfinished). ctx is the relaunches' context; each relaunch derives a context of its own
// from it.
//
// It runs in three steps. Every claim's uncertain launch is released first, each on a goroutine of
// its own. Then the waiting claims are placed in the wake pacing's queues, each queue in the order
// their work was owed (placeRelaunches), and only then is every released launch posted at once,
// each carrying its place (supervise.RequestSpawn.Place). Most of these claims were waiting in the
// previous daemon's pacing when it stopped, and a wake is memory only, so this is the queue that
// daemon was draining, rebuilt: placed instead as each goroutine reached the gate, its order was
// chance, and a claim that had waited most of an hour went back to a random place behind hundreds
// at each restart (dispatch://INFRA-5482).
//
// deaths is the claims whose record says their process died (recordedDeaths), whose relaunches
// were the rest of that queue: each still standing over its dead process (recordedDeath) is placed
// in the pacing's first queue, ahead of the restart's backlog (supervise.WakeQueue,
// dispatch://INFRA-5682 R2), and its relaunch is posted with its place as the death's replay
// (supervise.RecordedDeath.Place). The boot passes them to its first pass alone; none is ever
// refused, so none is retried.
func (s *supervision) launchUnfinished(ctx context.Context, waiting map[claim.Token]*wakepace.Place, deaths ...claim.Token) map[claim.Token]*wakepace.Place {
	launches := make([]*bootRelaunch, 0, len(waiting)+len(deaths))
	for token, place := range waiting {
		if m, ok := s.supervisor.Machine(token); ok {
			launches = append(launches, &bootRelaunch{token: token, m: m, place: place})
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
	launches = slices.DeleteFunc(launches, func(l *bootRelaunch) bool {
		if l.release != releaseSettled {
			return false
		}
		// A claim that settled by another way launches nothing here, so the place a sweep before
		// took for it is given up, and the turn it would have taken goes to the wake behind it.
		if l.place != nil {
			l.place.Leave()
		}
		return true
	})
	for _, token := range deaths {
		if l, ok := s.recordedDeath(token); ok {
			launches = append(launches, l)
		}
	}
	s.placeRelaunches(launches)
	var launching sync.WaitGroup
	refused := map[claim.Token]*wakepace.Place{}
	for _, l := range launches {
		if l.release == releaseRefused {
			refused[l.token] = l.place
			continue
		}
		launching.Add(1)
		go func() {
			defer launching.Done()
			var ev supervise.Event = supervise.RequestSpawn{Claim: l.token, Place: l.place}
			if l.death {
				ev = supervise.RecordedDeath{Claim: l.token, Generation: l.generation, Place: l.place}
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
// they record died: a live claim with its end decided (supervise.Claim.DeathCharged) and a
// locator. The daemon before this one judged each process gone and decided its end — charged its
// death, or found its pod ended from outside it and charged nothing, which the replay tells apart
// by the claim's record of that end (supervise's relaunchRecordedDeath) — and the relaunch it
// raised for it lived in its memory alone, a wake the pacing parked, which the restart lost
// (dispatch://INFRA-5472). The boot relaunches each beside its unfinished launches
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

// recordedDeath is token's relaunch for the end its record decided, as the boot's pass takes it
// (launchUnfinished), and false for a claim that no longer stands over that death as the machine
// holds it now: a report of the death that came first and raised its relaunch already (a wake
// waiting), a launch since, a report that found the process running, or a suspension held for the
// claim, whose end the death's own report decides. The replay decides each of those nothing
// (supervise's relaunchRecordedDeath), so the pass takes such a claim no place, which the replay
// would only give up again. A report of the death decided before this read keeps the place it
// joined itself, in the first queue the pass would have placed it in. One decided after this read
// and before the pass takes its places joins a place ahead of the pass's, so the replay moves
// nothing and gives up the place the pass took for it. One decided after the pass parks its
// relaunch behind every place the pass took in the first queue, and the replay moves that wake to
// the place the pass took for it (supervise's takeEarlierPlace), giving the report's own place up.
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

// placeRelaunches places each of launches whose launch the gate would hold (supervise.PacedLaunch)
// in the wake pacing's queue its claim's class names (supervise.WakeQueue): the first queue for a
// recorded death (dispatch://INFRA-5682 R2) and for a claim whose message mark stands (R1), the
// second for every other. Each queue is handed out in the order its claims' work was owed
// (owedSince, compareOwed): the claim owed longest takes the earliest place. The first queue is
// placed before the second, and every place is joined (wakepace.Pacer.Join) in the order it is
// handed out, here, before any launch is posted, so no place depends on which goroutine reaches
// its gate first. The pass hands out no moment: a place's turn is the pacer's to give. A launch
// the gate would not hold — the pacing off, a project pause holding it (the pause's gate takes no
// place), the project controller's — is handed none and launches as its gate decides, and a place
// a sweep before took for it is given up.
//
// A claim whose release the store refused keeps the place it was handed for the sweep that retries
// it, so a refusal that clears costs the claim no place: its place goes back into its queue's pool,
// and each claim of the retry takes its place in owed order. The pool is what keeps that order when
// places are let out: a kept place the pacer let out while its claim was refused — a turn spent,
// since nothing was bound to it — is replaced by a place joined afresh at its queue's back, but the
// pool sorts the places still waiting ahead of the fresh ones and hands the earliest to the claim
// owed longest, so the claims owed longest do not launch last, and a place let out long ago is never
// run at once, which would let every claim a store outage held launch together outside the pace. A
// refused claim whose place the pacer let out is handed none and joins none while it stays refused,
// so a refusal that never clears does not take a turn of the pacing at every sweep; its release,
// when it comes, joins it one.
func (s *supervision) placeRelaunches(launches []*bootRelaunch) {
	deps := &s.supervisor.deps
	type owed struct {
		launch *bootRelaunch
		since  time.Time
	}
	// Each queue's claims to place, and the places they kept that still wait in it, indexed by
	// wakepace.Queue.
	var queues [2]struct {
		owed []owed
		kept []*wakepace.Place
	}
	for _, l := range launches {
		c := l.m.View().Claim
		if !supervise.PacedLaunch(deps, &c) {
			if l.place != nil {
				l.place.Leave()
			}
			l.place = nil
			continue
		}
		queue := supervise.WakeQueue(&c)
		if l.place != nil {
			if _, out := l.place.Let(); out {
				if l.release == releaseRefused {
					continue
				}
			} else {
				// A claim marked since its place was taken waits in the first queue, as the gate
				// would move its place there (supervise's placeFor).
				if queue == wakepace.First {
					l.place.Mark()
				}
				queue = l.place.Queue()
				queues[queue].kept = append(queues[queue].kept, l.place)
			}
			l.place = nil
		}
		queues[queue].owed = append(queues[queue].owed, owed{launch: l, since: owedSince(c)})
	}
	for _, queue := range []wakepace.Queue{wakepace.First, wakepace.Second} {
		pool, waiting := queues[queue].kept, queues[queue].owed
		slices.SortStableFunc(pool, func(a, b *wakepace.Place) int {
			switch {
			case a.Before(b):
				return -1
			case b.Before(a):
				return 1
			}
			return 0
		})
		slices.SortStableFunc(waiting, func(a, b owed) int {
			return cmp.Or(compareOwed(a.since, b.since), cmp.Compare(a.launch.token, b.launch.token))
		})
		for i, w := range waiting {
			if i < len(pool) {
				w.launch.place = pool[i]
				continue
			}
			w.launch.place = deps.WakePace.Pacer.Join(queue)
		}
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
// sweep retries only the releases the store did not take, with the places they were handed
// (launchUnfinished), since the reconciliation's proof that no predecessor runs still holds.
//
// deaths is the claims whose record says their process died (recordedDeaths). They join the first
// pass, in the pacing's first queue, ahead of the unfinished launches (placeRelaunches). A recorded
// death needs no reconciliation, since its claim records the process it replaces, so where the
// boot's reconciliation failed their pass runs at once all the same, alone, in their own owed
// order, and the unfinished launches join the second queue at the sweep that reconciles: either
// way no relaunch that needs no reconciliation waits behind one that may never come.
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
