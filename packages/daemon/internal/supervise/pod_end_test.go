package supervise

// What the daemon charges for a process that ended with its pod, by what the runtime read about
// the cause of that end (runtime.Observation.PodEnd). An end something outside the pod made is the
// uncharged continue a displacement gets: the role's own session is resumed at once, neither a
// death nor a launch failure is charged, and the task the role owed is sent again. Such ends are
// counted (Budgets.PodEndsNotOurs), and at launch_failure_limit of them in a row, with no turn
// completed between, the end is charged as the claim's own death. An end the daemon itself made,
// and one the runtime read neither way, are charged exactly as they were before the reading
// existed — which is what keeps these cases from being an amnesty.

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// observePodEnd is the runtime's verdict that the claim's process ended with its pod, carrying
// what the runtime read about the cause of that end.
func (h *harness) observePodEnd(kind runtime.ObservationKind, reading runtime.PodEndReading) {
	h.t.Helper()
	h.must(RuntimeObservation{Observation: runtime.Observation{
		Locator: h.locator(), Kind: kind, At: h.clock.Now(), PodEnd: reading,
	}})
}

// owedTask is the task every case here hands the claim before its pod ends: unconfirmed, so the
// claim holds work outstanding when the end arrives, which is the one state chargeDeath charges a
// death in. Without it "no death charged" would say nothing at all.
const owedTask = "implement the plan"

// A role whose pod something outside it ended — the cluster evicted, preempted or shut that pod
// down, or something deleted it that no act of the daemon's accounts for — is continued in its own
// session: its process is replaced at once, nothing is charged, and the task it owed is sent
// again. The end is counted, the first of a run (Budgets.PodEndsNotOurs), which costs nothing
// until the run reaches the limit. The claim holds that task unconfirmed when the end arrives, so
// the same walk under an end the runtime read neither way charges the death and the launch failure
// it always did: that is the control, and it is what makes the uncharged half mean anything.
func TestAnEvictedPodsRoleIsContinuedChargingNoDeathAndSentItsTaskAgain(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reading runtime.PodEndReading
		want    Budgets
	}{
		{name: "something outside the pod ended it", reading: runtime.PodEndNotOurs, want: Budgets{PodEndsNotOurs: 1}},
		{name: "the end was read neither way", want: Budgets{LaunchFailures: 1, Deaths: 1}},
	} {
		for _, kind := range []runtime.ObservationKind{runtime.Gone, runtime.NotRecordedProcess} {
			t.Run(tc.name+"/"+string(kind), func(t *testing.T) {
				h := newHarness(t)
				h.reach(StateReady)
				h.must(RequestDeliver{Claim: h.token, Task: owedTask})
				h.wantPrompts(1)
				if p := h.pending(); p.Task != owedTask || !p.ConfirmedAt.IsZero() {
					t.Fatalf("pending %+v, want the task outstanding and unconfirmed when the pod ends", p)
				}
				ended := h.locator()

				h.observePodEnd(kind, tc.reading)

				h.wantState(StateLaunching)
				h.wantBudgets(tc.want)
				resume := h.wantCalls("Resume", 1)[0]
				if resume.Previous == nil || *resume.Previous != ended || resume.Spec.ResumeSessionFile != sessionFile {
					t.Fatalf("resumed %+v from %+v, want the role's own recorded session after the ended incarnation",
						resume.Spec, resume.Previous)
				}
				h.relaunched()
				if resent := h.wantPrompts(2)[1]; !strings.HasSuffix(resent.Message, owedTask) {
					t.Fatalf("re-sent %+v, want the task the role owed", resent)
				}
			})
		}
	}
}

// An end the daemon itself made is charged exactly as it is without the reading: a teardown or a
// close, which the daemon makes after it has suspended or released the claim, is a death with work
// outstanding wherever it still finds one, and a replacement of a live pod is the uncharged
// displacement it always was. The reading says who ended the pod and changes neither.
func TestAnEndTheDaemonItselfMadeIsChargedTheDeathItAlwaysWas(t *testing.T) {
	for _, tc := range []struct {
		name      string
		displaced bool
		want      Budgets
	}{
		{name: "an issue close or idle release teardown", want: Budgets{LaunchFailures: 1, Deaths: 1}},
		{name: "a relaunch's replacement of the pod", displaced: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateReady)
			h.must(RequestDeliver{Claim: h.token, Task: owedTask})
			h.wantPrompts(1)

			h.must(RuntimeObservation{Observation: runtime.Observation{
				Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now(),
				PodEnd: runtime.PodEndOurs, Displaced: tc.displaced,
			}})

			h.wantState(StateLaunching)
			h.wantBudgets(tc.want)
		})
	}
}

// An end the runtime read neither way is the claim's own death: it is charged one, relaunched, and
// at the third it has no budget left and fails. A terminal pod whose reason is none of the
// cluster's own reaches the supervisor exactly this way
// (sandbox's TestAnEvictedPodsEndIsObservedFromItsConditionOrItsReason), so a pod a node refuses
// again and again ends the claim rather than relaunching it for ever.
func TestAnEndOutsideTheClustersOwnReasonsIsChargedADeathAndFailsTheClaimAtTheThird(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: h.token, Task: owedTask})
	h.wantPrompts(1)

	for death := 1; death < testLimits().LaunchFailures; death++ {
		h.observePodEnd(runtime.Gone, "")

		h.wantState(StateLaunching)
		// One launch failure for the relaunch each death makes, which the launch that reaches
		// ready clears; the deaths themselves are what accumulate to the limit.
		h.wantBudgets(Budgets{LaunchFailures: 1, Deaths: death})
		h.relaunched()
	}

	h.observePodEnd(runtime.Gone, "")

	h.wantState(StateFailed)
	if deaths := h.budgets().Deaths; deaths != testLimits().LaunchFailures {
		t.Fatalf("the failed claim was charged %d deaths, want the limit %d", deaths, testLimits().LaunchFailures)
	}
}

// The uncharged continue is of the cause of the end and not of the kind of pod, so the project
// controller's claim — which is no role of an issue and shares its pod with nobody — is continued
// under an end from outside its pod as any role is. The second half is the condition for the
// first: under an end the runtime read neither way that same claim is charged the death it always
// was, so the rule waives what the cluster did and nothing else.
func TestTheControllersEvictedPodIsContinuedUnchargedAndItsOwnDeathIsStillCharged(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reading runtime.PodEndReading
		want    Budgets
	}{
		{name: "something outside its pod ended it", reading: runtime.PodEndNotOurs, want: Budgets{PodEndsNotOurs: 1}},
		{name: "the end was read neither way", want: Budgets{LaunchFailures: 1, Deaths: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarnessOf(t, controllerClaim())
			h.reach(StateReady)
			h.must(RequestDeliver{Claim: h.token, Task: "Legion controller start"})
			h.wantPrompts(1)
			ended := h.locator()

			h.observePodEnd(runtime.Gone, tc.reading)

			h.wantState(StateLaunching)
			h.wantBudgets(tc.want)
			if resume := h.wantCalls("Resume", 1)[0]; resume.Previous == nil || *resume.Previous != ended {
				t.Fatalf("resumed from %+v, want the controller's own ended incarnation", resume.Previous)
			}
			h.relaunched()
			if resent := h.wantPrompts(2)[1]; !strings.HasSuffix(resent.Message, "Legion controller start") {
				t.Fatalf("re-sent %+v, want the task the controller owed", resent)
			}
		})
	}
}

// outsideEndClaims are the two kinds of claim an end from outside the pod is decided for, each
// with the task it is handed: an issue's role and the project controller. The rule is of the
// cause of the end and never of the kind of pod, so every walk below runs over both.
var outsideEndClaims = []struct {
	name  string
	claim func() Claim
	task  string
}{
	{name: "an issue's role", claim: queuedClaim, task: owedTask},
	{name: "the project controller", claim: controllerClaim, task: "Legion controller start"},
}

// owingTask brings a claim of kind c to ready and hands it its task, which stays unconfirmed: the
// claim holds work outstanding when its pod ends, the one state chargeDeath charges a death in,
// so a walk that finds nothing charged has said something.
func owingTask(t *testing.T, c Claim, task string) *harness {
	t.Helper()
	h := newHarnessOf(t, c)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: h.token, Task: task})
	h.wantPrompts(1)
	h.wantOwed(task)
	return h
}

// wantOwed fails unless the claim holds task unconfirmed, so that the next end of its pod finds
// work outstanding.
func (h *harness) wantOwed(task string) {
	h.t.Helper()
	if p := h.pending(); p.Task != task || !p.ConfirmedAt.IsZero() {
		h.t.Fatalf("pending %+v, want the task %q outstanding and unconfirmed when the pod ends", p, task)
	}
}

// A claim whose pod something outside it ends launch_failure_limit times in a row, with no turn
// of the claim completed between, is charged a death at that end, and at every such end after it,
// until its deaths run out and it fails (endedFromOutside). The reading cannot tell a neighbour's
// pressure from the pod's own: a pod that uses more memory than it requested drives its own node
// into pressure, and that eviction carries the same DisruptionTarget condition. The ends before
// the limit are the uncharged continue each always was, which is the half that shows the count
// and not the reading decides.
//
// The run is the claim's and durable (claims.pod_ends_not_ours): a daemon restart in the middle
// of it, which this walk makes just before the end that reaches the limit, starts nothing again.
// The claim holds its task unconfirmed at every end, so "no death charged" before the limit says
// something.
func TestEvictionsInARowWithNoCompletedTurnAreChargedADeathAtTheLimitAndFailTheClaim(t *testing.T) {
	limit := testLimits().LaunchFailures
	for _, kind := range outsideEndClaims {
		t.Run(kind.name, func(t *testing.T) {
			h := owingTask(t, kind.claim(), kind.task)
			for end := 1; end < limit; end++ {
				h.observePodEnd(runtime.Gone, runtime.PodEndNotOurs)

				h.wantState(StateLaunching)
				h.wantBudgets(Budgets{PodEndsNotOurs: end})
				h.relaunched()
				h.wantOwed(kind.task)
			}
			if stored := h.store.load(h.token).Budgets; stored.PodEndsNotOurs != limit-1 {
				t.Fatalf("the stored claim carries a run of %d ends from outside its pod, want %d: a restart reads this and nothing else",
					stored.PodEndsNotOurs, limit-1)
			}
			h.restart()

			for death := 1; death < limit; death++ {
				h.observePodEnd(runtime.Gone, runtime.PodEndNotOurs)

				h.wantState(StateLaunching)
				// One launch failure for the relaunch each charged end makes, which the launch
				// that reaches ready clears; the deaths are what accumulate to the limit.
				h.wantBudgets(Budgets{LaunchFailures: 1, Deaths: death, PodEndsNotOurs: limit - 1 + death})
				h.relaunched()
				h.wantOwed(kind.task)
			}

			h.observePodEnd(runtime.Gone, runtime.PodEndNotOurs)

			h.wantState(StateFailed)
			if got := h.claim(); got.Budgets.Deaths != limit || got.FailureCause != CauseDied {
				t.Fatalf("the failed claim carries %d deaths and cause %q, want the limit %d and %q",
					got.Budgets.Deaths, got.FailureCause, limit, CauseDied)
			}
		})
	}
}

// A turn the claim completes between two ends from outside its pod starts the run again: the
// process held a pod long enough to finish work, so the end after it is the first of a new run
// and costs nothing. The claim reaches one end short of the limit, completes a turn, and is handed
// a new task it holds unconfirmed when its pod ends again — the end that, with no turn between,
// is charged a death (TestEvictionsInARowWithNoCompletedTurnAreChargedADeathAtTheLimitAndFailTheClaim).
func TestACompletedTurnBetweenEvictionsStartsTheCountAgainAndChargesNoDeath(t *testing.T) {
	limit := testLimits().LaunchFailures
	const next = "the next task"
	for _, kind := range outsideEndClaims {
		t.Run(kind.name, func(t *testing.T) {
			h := owingTask(t, kind.claim(), kind.task)
			for end := 1; end < limit; end++ {
				h.observePodEnd(runtime.Gone, runtime.PodEndNotOurs)
				h.wantBudgets(Budgets{PodEndsNotOurs: end})
				h.relaunched()
			}

			h.must(StreamTurnStart{Claim: h.token})
			h.must(StreamTurnEnd{Claim: h.token})

			h.wantState(StateIdle)
			h.wantBudgets(Budgets{})
			if stored := h.store.load(h.token).Budgets; stored != (Budgets{}) {
				t.Fatalf("the stored budgets after the completed turn are %+v, want the reset written", stored)
			}
			h.must(RequestDeliver{Claim: h.token, Task: next})
			h.wantOwed(next)

			h.observePodEnd(runtime.Gone, runtime.PodEndNotOurs)

			h.wantState(StateLaunching)
			h.wantBudgets(Budgets{PodEndsNotOurs: 1})
			h.relaunched()
			h.wantOwed(next)
		})
	}
}

// The kubelet evicts a pod with no DisruptionTarget condition when it goes over its own
// ephemeral-storage limit, a container's or an emptyDir's sizeLimit, and the runtime reads that
// end as neither reading (sandbox's TestAnEvictedPodsEndIsObservedFromItsConditionOrItsReason):
// it is charged a death, and at the third the claim fails, rather than a task that fills its own
// disk being relaunched into the same eviction for ever. The control is the eviction a node under
// pressure makes, which carries the condition and is read as an end from outside the pod: the same
// first end costs nothing there.
func TestAnEvictionWithNoDisruptionTargetIsChargedADeathAndFailsTheClaimAtTheThird(t *testing.T) {
	limit := testLimits().LaunchFailures
	for _, kind := range outsideEndClaims {
		t.Run(kind.name+"/no disruption target", func(t *testing.T) {
			h := owingTask(t, kind.claim(), kind.task)
			for death := 1; death < limit; death++ {
				h.observePodEnd(runtime.Gone, "")

				h.wantState(StateLaunching)
				h.wantBudgets(Budgets{LaunchFailures: 1, Deaths: death})
				h.relaunched()
				h.wantOwed(kind.task)
			}

			h.observePodEnd(runtime.Gone, "")

			h.wantState(StateFailed)
			if got := h.claim(); got.Budgets.Deaths != limit || got.FailureCause != CauseDied {
				t.Fatalf("the failed claim carries %d deaths and cause %q, want the limit %d and %q",
					got.Budgets.Deaths, got.FailureCause, limit, CauseDied)
			}
		})
		t.Run(kind.name+"/a disruption target", func(t *testing.T) {
			h := owingTask(t, kind.claim(), kind.task)

			h.observePodEnd(runtime.Gone, runtime.PodEndNotOurs)

			h.wantState(StateLaunching)
			h.wantBudgets(Budgets{PodEndsNotOurs: 1})
			h.relaunched()
			h.wantOwed(kind.task)
		})
	}
}

// An end from outside the pod that the daemon's orphan sweep reports again, over the process a
// refused relaunch left the claim standing on, is the one end read twice and is counted once
// (endedFromOutside, Claim.DeathCharged). Counted twice, one eviction would bring the claim a step
// nearer the end that is charged a death, and a claim whose relaunches are refused now and then
// would be charged for ends it never had.
func TestAnEvictionReportedAgainOverTheKeptProcessIsCountedOnce(t *testing.T) {
	h := owingTask(t, queuedClaim(), owedTask)
	dead := h.locator()
	store := &unansweredCheckLaunchStore{memStore: h.store, failing: true}
	h.m.deps.Store = store

	gone := RuntimeObservation{Observation: runtime.Observation{
		Locator: dead, Kind: runtime.Gone, At: h.clock.Now(), PodEnd: runtime.PodEndNotOurs,
	}}
	if err := h.handle(gone); err == nil {
		t.Fatal("the relaunch the end raised was taken, want the lifecycle recheck's refusal of it")
	}
	h.wantBudgets(Budgets{PodEndsNotOurs: 1})
	if stored := h.store.load(h.token); stored.Budgets.PodEndsNotOurs != 1 || !stored.DeathCharged {
		t.Fatalf("the stored claim carries a run of %d with DeathCharged %v, want 1 with the end recorded as counted",
			stored.Budgets.PodEndsNotOurs, stored.DeathCharged)
	}

	store.failing = false
	h.must(gone)

	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{PodEndsNotOurs: 1})
	h.relaunched()
}

// An end from outside the pod that finds the claim's agent ready with no work outstanding is
// neither counted nor charged, however many come (endedFromOutside, workOutstanding): the rule
// bounds a task that keeps ending its own pod, and a claim idle between assignments completes no
// turn, so a count it could raise would only grow, and a pod the cluster repacks now and then
// would cost it a launch failure and a backoff from its third repacking on. Each claim here holds
// no task, and each end is read before its relaunch reaches ready, which ends any run an agent
// with no work outstanding comes up with (ready): an end counted would show there as a run of
// one, and one charged as a launch failure or a death.
func TestAnEvictionWithNoWorkOutstandingIsDisplacedNeitherCountedNorChargedADeath(t *testing.T) {
	limit := testLimits().LaunchFailures
	for _, kind := range outsideEndClaims {
		t.Run(kind.name, func(t *testing.T) {
			h := newHarnessOf(t, kind.claim())
			h.reach(StateReady)
			if p := h.claim().Pending; p != nil {
				t.Fatalf("the claim holds %+v, want no task: this case is about a claim with no work outstanding", p)
			}

			for range limit + 1 {
				h.observePodEnd(runtime.Gone, runtime.PodEndNotOurs)

				h.wantState(StateLaunching)
				h.wantBudgets(Budgets{})
				h.relaunched()
			}
			if stored := h.store.load(h.token).Budgets; stored != (Budgets{}) {
				t.Fatalf("the stored budgets are %+v, want none: an end with no work outstanding moves nothing", stored)
			}
			h.wantPrompts(0)
		})
	}
}

// The run of ends from outside the pod is counted only while the claim holds a task, so it is
// counted against that task and ends with it, as the deaths do (retirePending): a task that ends
// some other way than a completed turn — here the daemon's suspension a phase transition sends,
// which retires the phase's task — takes the run with it. The claim reaches one end short of the
// limit holding its task, the suspension retires it, and a new task is delivered and left
// unconfirmed: the next end from outside the pod is the first of a new run, charged nothing.
// Kept, the old run would charge that end a death against a task it was never counted for.
func TestAnEvictionAfterTheTaskItsRunWasCountedAgainstEndedIsTheFirstOfANewRunAndNoDeath(t *testing.T) {
	limit := testLimits().LaunchFailures
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: h.token, Task: "round one", Phase: "implementing", Generation: 5})
	h.wantOwed("round one")
	for end := 1; end < limit; end++ {
		h.observePodEnd(runtime.Gone, runtime.PodEndNotOurs)
		h.wantBudgets(Budgets{PodEndsNotOurs: end})
		h.relaunched()
		h.wantOwed("round one")
	}

	h.must(RequestSuspend{Claim: h.token, By: SuspendedByDaemon})

	h.wantState(StateSuspended)
	if p := h.claim().Pending; p != nil {
		t.Fatalf("pending %+v after the daemon's suspension, want the finished phase's task retired", p)
	}
	h.wantBudgets(Budgets{})
	if stored := h.store.load(h.token).Budgets; stored != (Budgets{}) {
		t.Fatalf("the stored budgets after the task was retired are %+v, want the run ended in the retirement's write", stored)
	}

	h.must(RequestDeliver{Claim: h.token, Task: "round two", Phase: "implementing", Generation: 6})
	h.relaunched()
	h.wantOwed("round two")

	h.observePodEnd(runtime.Gone, runtime.PodEndNotOurs)

	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{PodEndsNotOurs: 1})
	h.relaunched()
	h.wantOwed("round two")
}

// An end from outside the pod counted against an open turn ends that turn, as a charged death
// does (chargeDeath, TestADeathEndsTheTurnItInterruptedSoLaterIdleDeathsAreNotCharged): a turn a
// message started is work outstanding with no task behind it, so the first end is counted, and
// nothing else records the end of a turn whose agent is gone. The relaunched agent then comes up
// with no work outstanding, which ends the run (ready). Left open, that turn would be work
// outstanding at that ready and at every later end of the parked relaunched agent — idle, with
// nothing pending — so the run would be kept and grow, and the third end would charge a death for
// work that was never there.
func TestAnEndFromOutsideThePodEndsTheTurnItInterruptedSoLaterIdleEndsAreNotCounted(t *testing.T) {
	limit := testLimits().LaunchFailures
	h := newHarness(t)
	h.reach(StateIdle)
	h.advance(time.Minute)
	h.must(StreamTurnStart{Claim: testToken})
	h.advance(time.Minute)

	h.observePodEnd(runtime.Gone, runtime.PodEndNotOurs)
	h.wantBudgets(Budgets{PodEndsNotOurs: 1})
	h.relaunched()
	if p := h.claim().Pending; p != nil {
		t.Fatalf("the relaunched claim holds %+v, want nothing pending", p)
	}

	for end := 2; end <= limit+1; end++ {
		h.observePodEnd(runtime.Gone, runtime.PodEndNotOurs)
		if got := h.budgets(); got != (Budgets{}) {
			t.Fatalf("end %d of the parked agent left budgets %+v, want none: "+
				"the turn the first end interrupted is over, and the agent came up with no work outstanding", end, got)
		}
		h.wantState(StateLaunching)
		h.relaunched()
	}
	h.wantState(StateReady)
}

// startEndedFromOutside is the end of a claim's pod, read as made by something outside it, that
// finds the claim's agent still starting: its shim connected when shim is true, launching
// otherwise. Both are a start not yet over (unready).
func (h *harness) startEndedFromOutside(shim bool) {
	h.t.Helper()
	h.wantState(StateLaunching)
	if shim {
		h.connect()
		h.wantState(StateShimConnected)
	}
	h.observePodEnd(runtime.Gone, runtime.PodEndNotOurs)
}

// A claim whose pod something outside it ends again and again while its agent starts is charged
// for it (endedFromOutside): a start the claim is making is work outstanding for the run, whether
// or not a task waits on it, because a pod's own start — the workspace's restore and six agents
// starting at once in a Burstable pod — can drive its node into memory pressure, and that
// eviction carries DisruptionTarget as a neighbour's does. The ends before launch_failure_limit
// are the uncharged continue each always was; from there each end is charged as the claim's own
// death, which for an agent not yet ready is a launch failure, so the claim fails once its launch
// failures run out rather than relaunching for ever with its task never sent. Each claim holds
// its task from before its first end, and its ends find it launching and with its shim connected
// by turns. Counted only on a ready agent, every one of these ends would be the uncharged
// continue, and the claim would stay launching with nothing charged at all.
func TestAStartEndedFromOutsideAgainAndAgainIsChargedLaunchFailuresAndFailsTheClaim(t *testing.T) {
	limit := testLimits().LaunchFailures
	for _, kind := range outsideEndClaims {
		t.Run(kind.name, func(t *testing.T) {
			h := newHarnessOf(t, kind.claim())
			h.launch()
			h.must(RequestDeliver{Claim: h.token, Task: kind.task})
			if p := h.claim().Pending; p == nil || p.Task != kind.task {
				t.Fatalf("pending %+v, want the task %q held while the agent starts", p, kind.task)
			}

			for end := 1; end < limit; end++ {
				h.startEndedFromOutside(end%2 == 0)

				h.wantState(StateLaunching)
				h.wantBudgets(Budgets{PodEndsNotOurs: end})
			}
			for charge := 1; charge < limit; charge++ {
				end := limit - 1 + charge
				h.startEndedFromOutside(end%2 == 0)

				h.wantState(StateLaunching)
				h.wantBudgets(Budgets{LaunchFailures: charge, PodEndsNotOurs: end})
			}

			h.startEndedFromOutside(true)

			h.wantState(StateFailed)
			got := h.claim()
			if got.Budgets.LaunchFailures != limit || got.Budgets.Deaths != 0 || got.FailureCause != CauseDied {
				t.Fatalf("the failed claim carries budgets %+v and cause %q, want launch failures at the limit %d, no death and %q",
					got.Budgets, got.FailureCause, limit, CauseDied)
			}
		})
	}
}

// A claim with no task whose pod is ended from outside while its agent starts, and whose agent
// then comes up, ends its run there (ready): the start the run was counted against is over, and
// a claim that holds no task completes no turn, so a run it kept would grow by every start a
// repacking interrupts and in the end charge a launch failure to a claim whose starts come up.
// Each cycle here ends the start one end short of the limit, brings the agent up, and has the
// cluster repack the idle agent's pod — an end with no work outstanding, displaced uncounted —
// whose relaunch is the next cycle's start. The cycles run past the limit, and the claim is never
// charged; with the run kept through ready, the second cycle's first end would charge it.
func TestAnIdleClaimThatComesUpAfterItsStartWasEndedFromOutsideEndsTheRunAndNeverFails(t *testing.T) {
	limit := testLimits().LaunchFailures
	for _, kind := range outsideEndClaims {
		t.Run(kind.name, func(t *testing.T) {
			h := newHarnessOf(t, kind.claim())
			h.launch()

			for cycle := 1; cycle <= limit+1; cycle++ {
				for end := 1; end < limit; end++ {
					h.startEndedFromOutside(true)

					h.wantState(StateLaunching)
					h.wantBudgets(Budgets{PodEndsNotOurs: end})
				}

				h.relaunched()

				h.wantState(StateReady)
				if got := h.budgets(); got != (Budgets{}) {
					t.Fatalf("cycle %d: the agent came up with no work outstanding and budgets %+v, want none: its start is over", cycle, got)
				}
				if stored := h.store.load(h.token).Budgets; stored != (Budgets{}) {
					t.Fatalf("cycle %d: the stored budgets after ready are %+v, want the run's end written with the ready", cycle, stored)
				}

				h.observePodEnd(runtime.Gone, runtime.PodEndNotOurs)

				h.wantState(StateLaunching)
				h.wantBudgets(Budgets{})
			}
			h.wantNoFailure()
			h.wantPrompts(0)
		})
	}
}

// A claim holding its task whose pod is ended from outside while its agent starts keeps its run
// when the agent comes up (ready): the ends were counted against work it still holds, which only
// that task's completed turn or its end puts behind it. The claim reaches one end short of the
// limit during its start, comes up and is sent its task, and the next end from outside its pod
// is the run's last, charged a death against that task. Ended at that ready, the run would let a
// claim whose task keeps ending its pod at each start relaunch for ever.
func TestAClaimThatComesUpHoldingItsTaskAfterItsStartWasEndedFromOutsideKeepsTheRun(t *testing.T) {
	limit := testLimits().LaunchFailures
	for _, kind := range outsideEndClaims {
		t.Run(kind.name, func(t *testing.T) {
			h := newHarnessOf(t, kind.claim())
			h.launch()
			h.must(RequestDeliver{Claim: h.token, Task: kind.task})
			for end := 1; end < limit; end++ {
				h.startEndedFromOutside(true)
				h.wantBudgets(Budgets{PodEndsNotOurs: end})
			}

			h.relaunched()

			h.wantOwed(kind.task)
			h.wantBudgets(Budgets{PodEndsNotOurs: limit - 1})
			if stored := h.store.load(h.token).Budgets; stored != (Budgets{PodEndsNotOurs: limit - 1}) {
				t.Fatalf("the stored budgets after ready are %+v, want the run of %d kept", stored, limit-1)
			}

			h.observePodEnd(runtime.Gone, runtime.PodEndNotOurs)

			h.wantState(StateLaunching)
			h.wantBudgets(Budgets{LaunchFailures: 1, Deaths: 1, PodEndsNotOurs: limit})
			h.relaunched()
			h.wantOwed(kind.task)
		})
	}
}
