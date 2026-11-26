package supervise

// The requests that do not wait on a wake the gate parked (wake.go's wake unit): a request that
// reaches a claim a wake parked over no process takes that wake off the machine first and gives it
// back if it launched nothing (dropWakeFor, reparkWake), an operator's retry no row of the state
// the gate found decides runs that wake's launch there and then (runDroppedLaunchForRetry), and an
// operator's task over a launch parked above a process the claim still records runs that launch
// there and then, unpaced (runParkedLaunchNow). Which requests those are is read here as well
// (dropsWaitingWake, operatorDelivery, exemptWake).

import (
	"context"
	"fmt"
)

// dropWakeFor is an operator's wake never waiting over a claim the gate parked with no process
// of its own: that park wrote the claim launching (waitOnMachine), so the row the request reaches
// would decide a claim that is only waiting its turn. An operator's resume is answered "the
// claim is not suspended" and an operator's forget-session "the claim has a live process or
// launch", neither of which is true of such a claim, and an operator's own task is queued behind
// a turn that is tens of minutes away in a wave, where the claim the gate found would have
// launched for it at once (dropsWaitingWake, deliverResuming). So the wake is taken off the
// machine and the state the gate found is put back in memory, and the decision that follows
// writes the claim — a resume, a retry, a spawn and an operator's delivery launch, and a
// forget-session persists.
//
// A claim parked over a process it still records is dropped for nothing, whatever the request.
// That park changed neither the claim nor the process, so every row already decides it as it
// stands, and the wake still holds the only thing that retires that process: a launch whose turn
// lets it go and arms the next process's timers (wakeNow, waitOnMachine). The one request that may
// not wait for the turn — an operator's task, whose take is refused while a launch waits
// (claimHold's launchWaits) — runs that launch where it arrives rather than dropping it
// (runParkedLaunchNow).
//
// The dropped wake keeps its place in the pacing's queue: its TimerWake is taken off the place
// rather than disarmed (detachWake), so the place waits on with no release to call, and the wakes
// around it keep their order. A let-out that comes while the wake is off the machine is read when
// the wake is taken up again (reparkWake), and a wake the decision does not take up gives the
// place up there.
//
// The wake is returned so that a decision which launched nothing can take it up again
// (reparkWake): a wake that waits must lose nothing, and a refusal, a named ignore or a
// forget-session leaves the claim exactly where the gate found it, with the launch this wake
// holds still owed to it.
//
// A paced request drops nothing: a second wake request takes no second place, and the claim's
// own wake is the launch it is asking for.
//
// Nor does a launch that waits at its issue pod's gate (wake.park) drop for any request. It is no
// turn of the pacing's: the launch has started, letting its process go and writing its claim
// launching, and waits only for another role's session load, which an operator's start waits for
// too (dispatch://INFRA-5297). So every request decides that claim as the launch under way it is,
// and the launch keeps its place in the pod's order.
func (m *Machine) dropWakeFor(ev Event) *wake {
	if m.wake == nil || m.wake.park != nil || m.claim.Locator != nil || !dropsWaitingWake(ev) {
		return nil
	}
	dropped := m.wake
	request, _ := requestName(ev)
	m.log.Info("supervise: a request that does not wait reached a claim the wake pacing holds over no process; dropping the wake",
		"request", request, "kind", dropped.kind, "due", dropped.due(), "state", dropped.state)
	m.claim.State = dropped.state
	m.detachWake()
	m.wake = nil
	return dropped
}

// runParkedLaunchNow is an operator's task meeting a launch the pacing parked over the process
// the claim still records: that launch runs here, unpaced, before the row that queues the task
// (Handle). Both halves of what the operator is owed hold only this way. A person is waiting on
// the task they have just given the claim, so it may not wait out a paced turn that is tens of
// minutes away in a wave; and the task may not reach the process that launch is about to let go,
// since the launch would cut off the turn it had just begun (claimHold's launchWaits). Running
// the launch here leaves the claim exactly where an unpaced daemon's would have left it —
// launching, over a process let go, with the next one's timers armed — so the row the task then
// reaches queues it for the process this launch starts (deliver), and that process's worker
// takes it once it is ready.
//
// Dropping the wake instead would leave the claim recording a process nothing retires: the park
// disarmed that process's boot interval, registration deadline and probe interval (waitOnMachine),
// the launch that was to replace it would be gone, and every report of it decides nothing while
// no launch waits only because one does (judge). The next report of that process would charge
// another launch failure, the death it already charged excepted (died).
//
// Five things it does not touch: the four here, and a launch the pause's gate holds (below). A
// request the table refuses or ignores decides nothing, so it starts no launch either: the row is
// read here, before anything runs, exactly as Handle reads it after. A task the row acts on but
// queue refuses decides nothing either, so it starts no launch: that launch would replace a live
// process for a request the claim answers with nothing.
// The deliver route sends an operator's task with no id of its own and no outbox row
// (operatorDelivery, RequestDeliver.ID, Claim.DeliveredRows), so neither of the two repeats queue
// accepts over a pending delivery — the repeat of that very delivery under its id, and a row
// whose task the claim has already taken — can be what arrived, and queue answers it "a delivery
// is already pending" (queue, ErrDeliveryPending) behind exactly two pending deliveries: one a
// take holds whose turn has not started, and an untaken one that the record still owes and that
// no newer task replaces. The record is asked here whether it owes that untaken one, and the
// row's queue asks it again before its refusal. A claim holding a suspension is left to its
// hold: the turn of such a claim ends the suspension rather than launching (wakeNow, endHeld),
// which is not what a task asks for, and the task is queued and kept until the hold lifts
// whichever process the claim is running (claimHold). And a take over a claim that is working is
// left to the turn that runs, which the launch would cut, though queue takes the operator's task
// over such a take whose turn has started (its last case), and that task waits for the launch's
// turn.
//
// The pending deliveries the launch runs over are the ones the operator's task takes the place
// of, of three kinds, decided in the order queue decides them. The first is an untaken task the
// record no longer owes (dropUnowed, the test a take makes before it drops): its issue has left
// the phase it was queued for, it is an ask of an update round an older daemon queued, or it is
// the daemon's own delivery of a relaunch's record that no longer tells. queue drops such a task
// and queues the newer one in its place, so the claim takes an operator's task over it, a person
// is waiting on a task this claim will work, and the launch runs for it. The drop is made here,
// before that launch, as queue makes it — the value deleted, under the take's drop line — so the
// row's queue finds no delivery, and the operator's task carries the record's sentence in front
// of itself where the record still tells (Delivery.message).
//
// The second is one a newer task replaces (replaceable): the task a provider error took back
// while the ladder's wait stands for it and nothing works it, or the daemon's own delivery of a
// relaunch's record while no take of it is accepted. A newer task takes its place rather than
// waiting out a wait the next error only makes longer (queue, dispatch://INFRA-956 R2), or behind
// a sentence it carries itself (Delivery.message). The claim takes an operator's task over such a
// one, so a person is waiting on a task this claim will work, and the launch runs for it. The
// replacement is made here, before that launch, and not left to the row (replaceOwedTask, the one
// path both places a task replaces an owed one take). For the task a provider error took back it
// must be: the launch takes the ladder's wait with the claim's timers (letGo), and that wait is
// the whole of what makes that task replaceable, so queue — reading the claim the launch leaves —
// would refuse the very task the launch had just replaced a live process for. The relaunch's
// delivery would stay replaceable, since no take of it is accepted; it is replaced here with the
// other, on the one path.
//
// The third is a task a take holds whose turn has run, over a claim in no turn (ready or idle):
// the take's turn is over and no report has finished the task, or a report came and the store
// refused its finish, which the retirement makes (takesTurnRan, retirePending). queue retires such
// a task and queues the newer one in its place (its last case), so the claim takes an operator's
// task over it, and the launch runs for it. The retirement is made here, before that launch, as
// queue makes it, under a line of its own, so the row's queue finds no delivery. Left to the row
// it could not be: where only a report finishes the task (TakesNoTask false), the launch would take
// it back as a cut turn (settle, interrupted), and queue, reading the claim the launch leaves,
// would refuse the operator's task behind the task taken back, which the next process would then
// run again.
//
// Each of the three is made only once the launch's lifecycle recheck has passed, and that recheck
// is made here, first, with the wake still waiting, rather than at the launch's own first step
// (runWake): the launch then runs from past its recheck (launchWake). A recheck that refuses —
// the tree's cleanup reserved, or a store that did not answer — starts no launch, and a refusal
// over a claim that records its process ends the decision before the row that queues the task
// (relaunchRefused, Handle): a drop, a retirement or a replacement made before it, with the wake
// taken off the machine, would leave the claim holding neither the task it had, nor the
// operator's, nor the launch. So a refusal changes nothing: the pending task, its value, the
// timers armed and the wake with its place in the pacing are left as they were, and the operator's
// task is refused with the recheck's reason (RefusedError, which the operator's deliver route
// answers 409), so the operator asks again, and that delivery is decided afresh. The recheck comes
// before the record is asked whether it owes an untaken task, so over a task it still owes and no
// newer one replaces, a refusal names the lifecycle rather than the pending delivery; both leave
// the claim as it was.
//
// A plugin-refresh wake whose fact is gone — the process registered on this daemon's contract
// inside the wait, or the claim started a turn of its own — relaunches nothing
// (refreshOverturned), so it has no launch to recheck, and a refusal would answer the operator 409
// over a launch that would not have run. So the task spends such a wake before the recheck, as
// its turn would (runWake's re-read), makes none of the three here, and goes to the row that
// follows, which queues it over the claim as it stands, the pending task giving way there as
// queue decides. Every launch that will run keeps the recheck above, and a refusal of it still
// changes nothing.
//
// The launch takes no turn of the pacing: disarming its timer gives its place up (placeTimer), and
// the turn that place would have taken goes to the wake behind it. The daemon is told the launch is
// let out now (wakeLetOut), and the line it writes says the pacing did not decide when it ran
// (wakeRan, launchWake).
//
// A launch the project pause's gate holds (wake.pause) is not run for the operator's task either:
// nothing but the controller launches while the project is paused (pauseGate), so the task is
// queued and kept, and goes where every task held for a launch goes once the claim runs again.
func (m *Machine) runParkedLaunchNow(ctx context.Context, ev Event) error {
	if !operatorDelivery(ev) || m.wake == nil || !m.launchWaits() || m.wake.pause != 0 || m.claim.Locator == nil || m.held != nil {
		return nil
	}
	if r, ok := table[key{m.claim.State, kindOf(ev)}]; !ok || r.act == nil {
		return nil
	}
	pending := m.claim.Pending
	if pending != nil && (m.takeAwaitsTurn() || (pending.taken() && m.busy())) {
		return nil
	}
	// A plugin-refresh wake whose fact is gone launches nothing, so no recheck is made for it: the
	// task spends it (runWake's re-read), and the row queues the task.
	if m.wake.kind == WakePluginRefresh && m.refreshOverturned() {
		waiting := m.wake
		m.wake = nil
		m.disarm(TimerWake)
		return m.runWake(ctx, waiting, false)
	}
	if err := m.checkLaunch(ctx); err != nil {
		m.log.Warn("supervise: an operator's task reached a claim whose paced launch waits over the process it replaces, and that launch's lifecycle recheck refused it; the task is refused, and the pending task and the launch are left as they were",
			"kind", m.wake.kind, "due", m.wake.due(), "state", m.claim.State, "error", err)
		return &RefusedError{State: m.claim.State, Request: "deliver", Err: fmt.Errorf(
			"the launch parked over %s's process runs before this task is queued, and its tree's lifecycle check refused that launch, so nothing of the claim changed; ask again once the check passes: %w",
			m.claim.Token, err)}
	}
	if pending != nil {
		switch {
		case pending.taken():
			m.log.Info("supervise: the pending task's turn has run, so the operator's task takes its place, and the launch parked over the claim's process runs for it",
				"taken", pending.Number, "delivery", pending.ID)
			if err := m.retirePending(ctx); err != nil {
				return err
			}
		default:
			dropped, err := m.dropUnowed(ctx)
			if err != nil {
				return err
			}
			if dropped {
				break
			}
			owed, attempt := pending.ID, pending.ProviderErrorRetries
			replaced, err := m.replaceOwedTask(ctx)
			if err != nil || !replaced {
				return err
			}
			if relaunchTask(owed) {
				m.log.Info("supervise: the operator's task replaces the daemon's delivery of the relaunch's record, and the launch parked over the claim's process runs for it",
					"task", pending.Number, "delivery", pending.ID)
			} else {
				m.log.Info("supervise: the operator's task replaces the one a provider error took back, and the launch parked over the claim's process runs for it",
					"owed", owed, "attempt", attempt)
			}
		}
	}
	waiting := m.wake
	// The estimate is read before the disarm gives the place up (placeTimer): a place that left
	// estimates the zero time.
	due := waiting.due()
	m.wake = nil
	m.disarm(TimerWake)
	m.log.Info("supervise: an operator's task reached a claim whose paced launch waits over the process it replaces; that launch runs now and the task goes to the process it starts",
		"kind", waiting.kind, "due", due, "state", m.claim.State)
	m.wakeLetOut(m.deps.Clock.Now())
	return m.launchWake(ctx, waiting, false)
}

// runDroppedLaunchForRetry is the operator's retry (RequestAskedRetry naming the operator, the line
// exemptWake draws: the controller's retries, which it asks of as many agents at once as it decides
// to, keep the turn the pacing gave them and launch at it) of a claim whose launch the pacing or
// the pause's gate parked over no process, where the state the gate found is one no retry row acts
// on: queued, which every spawn and the boot's relaunch of an unfinished launch park from;
// suspended, which a rule's resume parks from; and launching, which a try the pause met after its
// write (pauseHeldTry) and a start whose pod grant the pause met (podStartDue) are held in, their
// wake already past the pacing (wake.ran), whose run is a fresh try. The claim reads launching for
// the whole wait, so a person who sees it stuck reaches for `legion claims retry`; refusing that
// retry as "a queued claim is spawned, not retried" left the claim behind a restart's whole queue,
// an hour of it on 2026-11-23 (dispatch://INFRA-5482). The person is asking for the claim to run,
// and the launch the dropped wake holds is the one that runs it, so that launch runs here, unpaced,
// exactly as its turn would have run it: the claim is put back launching, where the park wrote it
// (waitOnMachine), and the wake goes through runWake, lifecycle recheck and all. ran is false, and
// nothing is done, for every other request, for a claim whose gate-found state a retry row decides
// (failed, retired), and for any launch while a project pause holds the claim (pauseHolds): the API
// refuses the operator's retry while the project is paused (refusedWhilePaused), and a pause that
// opened after that refusal was read holds this launch as it holds every other, so the retry is
// refused and the wake parked again, for its turn, or the pause's gate, to take it (wakeNow,
// pauseGate). A launch the pause's gate held whose pause has ended, before the gate's own stop or
// the resume's event let it out (pauseStopDue, resumePause), runs here too, out of that gate, but
// unpaced and with no place taken, where their let-out takes a place for a launch that has not paid
// (letOutHeldLaunch).
//
// The launch takes no turn of the pacing, as an operator's task over a parked launch takes none
// (runParkedLaunchNow): its place is given up, the turn it would have taken going to the wake
// behind it, and the daemon is told the launch is let out now (wakeLetOut).
func (m *Machine) runDroppedLaunchForRetry(ctx context.Context, ev Event, dropped *wake) (ran bool, err error) {
	if asked, retry := ev.(RequestAskedRetry); !retry || asked.By != SuspendedByOperator || dropped == nil || m.held != nil {
		return false, nil
	}
	if _, paused := m.pauseHolds(); paused {
		return false, nil
	}
	if dropped.pause != 0 {
		dropped.pause = 0
		m.disarm(TimerPause)
	}
	m.log.Info("supervise: an operator's retry reached a claim whose paced launch waits over no process; that launch runs now",
		"kind", dropped.kind, "due", dropped.due(), "state", dropped.state)
	dropped.leave()
	m.claim.State = StateLaunching
	m.wakeLetOut(m.deps.Clock.Now())
	return true, m.runWake(ctx, dropped, false)
}

// reparkWake takes a dropped wake up again when the decision it was dropped for launched nothing
// (Handle, after both the named ignore's refusal and the row that acted): the claim is in the
// state the gate found, with no process and no wake of its own, which is where a refusal, an
// ignore and a forget-session each leave it — and the launch the wake holds is still owed. Its
// TimerWake is bound to the same place again (bindWake) and the wake is parked by the park the
// gate itself makes (waitOnMachine): the place kept its spot in its queue while the wake was off
// the machine (dropWakeFor), so this costs nothing and moves no other wake. A place the pacer let
// out meanwhile arms TimerWake at its let-out moment, which fires at once, so the wake runs
// through its own timer as its let-out would have run it.
//
// A claim that records a process takes no wake back, which is what the locator in the guard
// below says: every wake dropped here was parked over no process, so a claim recording one is a
// claim the decision launched (dropWakeFor). A dropped wake that is not taken back — that launch,
// a claim the decision moved on, or a wake the decision raised in its place — gives its place up,
// the turn it would have taken going to the wake behind it.
//
// A write that fails is logged and changes no verdict: the request is answered as the table
// answered it, and the claim stands as that answer left it, which is what a refusal leaves today.
// The wake still waits in memory, and a restart loses it as it loses any other.
//
// A launch the project pause's gate held goes back to that gate, as it was held, rather than to a
// place in the pacing it never had (pauseGate): the gate's own stop is posted again, and it reads
// then whether that pause still holds the claim (pauseStopDue).
func (m *Machine) reparkWake(ctx context.Context, dropped *wake) {
	if dropped == nil {
		return
	}
	if m.wake != nil || m.claim.Locator != nil || m.claim.State != dropped.state {
		dropped.leave()
		return
	}
	m.log.Info("supervise: the request launched nothing; the wake the pacing holds keeps its place",
		"kind", dropped.kind, "due", dropped.due(), "state", dropped.state, "pause", dropped.pause)
	var err error
	if dropped.pause != 0 {
		err = m.pauseGate(ctx, dropped, dropped.pause)
	} else {
		m.rebindWake(dropped.place)
		err = m.waitOnMachine(ctx, dropped)
	}
	if err != nil {
		m.log.Warn("supervise: the wake that keeps its place could not be written; it waits in memory alone",
			"kind", dropped.kind, "due", dropped.due(), "error", err)
	}
}

// dropsWaitingWake is whether ev is a request the wake pacing never makes wait over a claim it
// parked with no process: every request exemptWake lets through, every forget-session whoever
// asks it (the operator's own, and the one a fresh session of the operator or the controller
// sends: RequestForgetSession.By) — which launches nothing, but which a claim parked launching
// would refuse "the claim has a live process or launch", and it has neither — and an operator's
// own delivery (operatorDelivery), which a suspended claim parked by the rule's own resume would
// queue behind that paced turn where the state the gate found launches for it at once
// (deliverResuming, revive). An operator's delivery over a launch parked above a process the
// claim still records is none of this: that launch runs where the task arrives instead
// (runParkedLaunchNow).
//
// The daemon's resume by rule is none of them: it meets a parked claim with "the claim runs:
// nothing to resume" (nothingToDo), which is the truth — the wake the claim waits for is the
// launch that rule would make. Nor is the controller's resume_agent or retry, which the pacing makes
// wait (exemptWake): dropping the wake for it would park a wake of its own behind every turn the
// queue holds, for the launch this claim already waits its turn for, so it reaches the agent while
// its claim is parked launching, is refused because that claim is neither suspended nor failed,
// and the claim keeps the turn it holds.
func dropsWaitingWake(ev Event) bool {
	if _, ok := ev.(RequestForgetSession); ok {
		return true
	}
	return operatorDelivery(ev) || exemptWake(ev)
}

// operatorDelivery is a task a person gave a claim through the daemon's API: that route sends it
// with no id of an outbox row (api.operator's deliver, RequestDeliver.ID), where every task the
// workflow queues names the row it came from. The daemon's own task of no row, the controller
// keeper's start message, says it is no operator's (RequestDeliver.ResumesNothing). The pacing
// makes no operator's task wait (dropsWaitingWake).
func operatorDelivery(ev Event) bool {
	deliver, ok := ev.(RequestDeliver)
	return ok && deliver.ID == "" && !deliver.ResumesNothing
}

// exemptWake is whether the launch a request raises is one the wake pacing lets through: the
// operator's own resume and retry (RequestResume.By, RequestAskedRetry.By naming the operator),
// which a person is waiting on, and a spawn whose caller marked it exempt, which is the
// operator's spawn route and nothing else — the project controller is exempt by identity, in the
// gate itself, which covers the launches no request raises as well (reserveWake,
// claim.IsController). The same two events from the agent door are paced: the controller's
// resume, retry and fresh session (api's resumeAgent, retryAgent, claimFreshSession) are launches
// an agent raises, as many at once as it asks for — a controller lifting its own holds after a
// deploy asks one for every agent it held — and let through, they would start them all at once,
// past `worker_wakes_per_minute`, the bound that keeps a wave under the provider's per-minute cap.
// Every other path into a launch is paced, so every other event is too: an operator's delivery
// carries no launch of its own, and revive reads the exemption from the delivery itself
// (deliverResuming).
//
// The same flag keeps the project controller's session older than its age at the launch it raises
// (replaceAgedSession, dispatch://INFRA-2045 R3): the gate lets every launch of the controller
// through by identity, so for the controller this is the flag's one effect, and a daemon request
// marked exempt here would turn R3 off for that request.
func exemptWake(ev Event) bool {
	switch ev := ev.(type) {
	case RequestSpawn:
		return ev.Exempt
	case RequestResume:
		return ev.By == SuspendedByOperator
	case RequestAskedRetry:
		return ev.By == SuspendedByOperator
	}
	return false
}
