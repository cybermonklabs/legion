package supervise

// What the daemon charges for a process that ended with its pod, by what the runtime read about
// the cause of that end (runtime.Observation.EndReading). An end something outside the pod made is
// the uncharged continue a displacement gets: the role's own session is resumed at once, neither a
// death nor a launch failure is charged, and the task the role owed is sent again — every time,
// however many such ends come, whatever the claim's agent was doing and whether it was ready or
// still starting (endedFromOutside). An end the daemon itself made, and one the runtime read
// neither way, are charged exactly as they were before the reading existed — which is what keeps
// these cases from being an amnesty.

import (
	"errors"
	"log/slog"
	"strconv"
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
		Locator: h.locator(), Kind: kind, At: h.clock.Now(), EndReading: reading,
	}})
}

// owedTask is the task every case here hands the claim before its pod ends: unconfirmed, so the
// claim holds work outstanding when the end arrives, which is the one state chargeDeath charges a
// death in. Without it "no death charged" would say nothing at all.
const owedTask = "implement the plan"

// farPastTheLimit is how many ends from outside the pod each never-fails walk below makes: five
// more than launch_failure_limit, so a charge at any count up to the limit, or a little past it,
// shows as a death, a launch failure or a failed claim.
func farPastTheLimit() int { return testLimits().LaunchFailures + 5 }

// A role whose pod something outside it ended — the cluster evicted, preempted or shut that pod
// down, or something deleted it that no act of the daemon's accounts for — is continued in its own
// session: its process is replaced at once, nothing is charged, and the task it owed is sent
// again. The claim holds that task unconfirmed when the end arrives, so the same walk under an end
// the runtime read neither way charges the death and the launch failure it always did: that is
// the control, and it is what makes the uncharged half mean anything. The continue is of the cause
// of the end and not of the kind of pod, so both halves run over both kinds of claim
// (outsideEndClaims): the project controller's claim, which is no role of an issue and shares its
// pod with nobody, is continued under an end from outside its pod as an issue's role is, and
// charged the death it always was under an end read neither way.
func TestAnEvictedPodsRoleIsContinuedChargingNoDeathAndSentItsTaskAgain(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reading runtime.PodEndReading
		want    Budgets
	}{
		{name: "something outside the pod ended it", reading: runtime.PodEndNotOurs},
		{name: "the end was read neither way", want: Budgets{LaunchFailures: 1, Deaths: 1}},
	} {
		for _, kind := range outsideEndClaims {
			for _, observed := range []runtime.ObservationKind{runtime.Gone, runtime.NotRecordedProcess} {
				t.Run(tc.name+"/"+kind.name+"/"+string(observed), func(t *testing.T) {
					h := owingTask(t, kind.claim(), kind.task)
					ended := h.locator()

					h.observePodEnd(observed, tc.reading)

					h.wantState(StateLaunching)
					h.wantBudgets(tc.want)
					resume := h.wantCalls("Resume", 1)[0]
					if resume.Previous == nil || *resume.Previous != ended || resume.Spec.ResumeSessionFile != sessionFile {
						t.Fatalf("resumed %+v from %+v, want the claim's own recorded session after the ended incarnation",
							resume.Spec, resume.Previous)
					}
					h.relaunched()
					if resent := h.wantPrompts(2)[1]; !strings.HasSuffix(resent.Message, kind.task) {
						t.Fatalf("re-sent %+v, want the task %q the claim owed", resent, kind.task)
					}
				})
			}
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
			h := owingTask(t, queuedClaim(), owedTask)

			h.must(RuntimeObservation{Observation: runtime.Observation{
				Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now(),
				EndReading: runtime.PodEndOurs, Displaced: tc.displaced,
			}})

			h.wantState(StateLaunching)
			h.wantBudgets(tc.want)
		})
	}
}

// outsideEndClaims are the two kinds of claim an end from outside the pod is decided for, each
// with the task it is handed: an issue's role and the project controller. The rule is of the
// cause of the end and never of the kind of pod, so every walk below runs over both. record is
// whether the kind's relaunch that finds it holding nothing is given the daemon's own relaunch
// record at its ready (queueRelaunch): the controller is given none.
var outsideEndClaims = []struct {
	name   string
	claim  func() Claim
	task   string
	record bool
}{
	{name: "an issue's role", claim: queuedClaim, task: owedTask, record: true},
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

// wantNothingCharged fails unless the claim, in memory and in the store, carries no budget spent:
// no death, no launch failure, no prompt failure.
func (h *harness) wantNothingCharged(when string) {
	h.t.Helper()
	if got, stored := h.budgets(), h.store.load(h.token).Budgets; got != (Budgets{}) || stored != (Budgets{}) {
		h.t.Fatalf("%s: the claim carries budgets %+v (%+v stored), want nothing charged", when, got, stored)
	}
}

// The kubelet evicts a pod with no DisruptionTarget condition when it goes over its own
// ephemeral-storage limit, a container's or an emptyDir's sizeLimit, and the runtime reads that
// end as neither reading (sandbox's TestAnEvictedPodsEndIsObservedFromItsConditionOrItsReason):
// it is charged a death, and at the third the claim fails, rather than a task that fills its own
// disk being relaunched into the same eviction for ever. A terminal pod whose reason is none of
// the cluster's own reaches the supervisor exactly this way, so a pod a node refuses again and
// again ends the claim rather than relaunching it for ever. The control is the eviction a node
// under pressure makes of a pod whose message names no container over its request, which carries
// the condition and is read as an end from outside the pod: the same first end costs nothing
// there. An eviction under pressure that names a container over its request is the pod's own use,
// read neither way and charged as this one is (sandbox's overRequest).
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
			h.wantNothingCharged("the end")
			h.relaunched()
			h.wantOwed(kind.task)
		})
	}
}

// A claim whose pod something outside it ends again and again while its ready agent holds its
// task is continued uncharged every time, far past launch_failure_limit, and never fails
// (endedFromOutside): the role in the pod cannot fix a cause outside it, and a charge at some
// count would only turn a loop of restarts into a claim that runs nothing. Each end finds the task
// unconfirmed, the state a death is charged in, and the relaunch sends it again. A daemon restart
// in the middle of the run changes nothing, since nothing is counted for it to keep.
func TestEndsFromOutsideFarPastTheLimitNeverFailAReadyAgentHoldingItsTask(t *testing.T) {
	ends := farPastTheLimit()
	for _, kind := range outsideEndClaims {
		t.Run(kind.name, func(t *testing.T) {
			h := owingTask(t, kind.claim(), kind.task)
			for end := 1; end <= ends; end++ {
				if end == ends/2 {
					h.restart()
					h.wantState(StateReady)
				}
				h.observePodEnd(runtime.Gone, runtime.PodEndNotOurs)

				h.wantState(StateLaunching)
				h.wantNothingCharged("end " + strconv.Itoa(end))
				h.relaunched()
				h.wantOwed(kind.task)
			}
			h.wantNoFailure()
		})
	}
}

// An end from outside the pod that finds the claim's agent ready with no work outstanding — idle
// between assignments, holding no task, in no turn — is continued uncharged as any other is,
// however many come: a pod the cluster repacks now and then costs the claim nothing, and it is
// never failed for it.
func TestEndsFromOutsideFarPastTheLimitNeverFailAnIdleClaim(t *testing.T) {
	ends := farPastTheLimit()
	for _, kind := range outsideEndClaims {
		t.Run(kind.name, func(t *testing.T) {
			h := newHarnessOf(t, kind.claim())
			h.reach(StateReady)
			if p := h.claim().Pending; p != nil {
				t.Fatalf("the claim holds %+v, want no task: this case is about a claim with no work outstanding", p)
			}

			for end := 1; end <= ends; end++ {
				h.observePodEnd(runtime.Gone, runtime.PodEndNotOurs)

				h.wantState(StateLaunching)
				h.wantNothingCharged("end " + strconv.Itoa(end))
				h.relaunched()
			}
			h.wantNoFailure()
			h.wantPrompts(0)
		})
	}
}

// relaunchRecordRuns is what a claim relaunched holding nothing is given at its ready and
// does with it, for a kind whose relaunch is given the daemon's own relaunch record (record): the
// record's turn runs to completion — its start confirms the record, its end completes it, and the
// prompt's result behind that end retires it — so the claim holds nothing again, and the next
// turn a message starts is a message's. A kind given no record, the controller, holds nothing at
// all, though what its last let-go ended is still to tell.
func (h *harness) relaunchRecordRuns(record bool) {
	h.t.Helper()
	h.wantState(StateReady)
	p := h.claim().Pending
	if !record {
		if ended := h.claim().EndedWork; p != nil || !ended.Tells() {
			h.t.Fatalf("the relaunched controller holds %+v with the record %+v, want nothing held and the record untold: "+
				"the daemon queues the controller no relaunch record", p, ended)
		}
		return
	}
	if p == nil || !p.Relaunch() {
		h.t.Fatalf("the relaunched claim holds %+v, want the relaunch's record alone", p)
	}
	// The agent takes the record's prompt at once, as it takes any prompt sent at its ready.
	h.must(StreamTurnStart{Claim: h.token})
	if c := h.claim().Pending; c == nil || c.ID != p.ID || c.ConfirmedAt.IsZero() {
		h.t.Fatalf("the turn's start left %+v pending, want the record %s confirmed: the turn is the record's", c, p.ID)
	}
	h.advance(time.Minute)
	h.mustQuiet(StreamTurnEnd{Claim: h.token})
	h.wantState(StateIdle)
	h.must(StreamPromptResult{Claim: h.token, DeliveryID: p.ID, AgentInvoked: true})
	if c := h.claim().Pending; c != nil {
		h.t.Fatalf("the record's turn ended with %+v pending, want the record retired", c)
	}
}

// messageTurnEndedWithItsPod is a message's turn on the agent, which holds nothing, so the turn
// confirms nothing, and the end of its pod a minute in, read as reading: from outside the pod
// (runtime.PodEndNotOurs), or neither way (""), which is the process's own death.
func (h *harness) messageTurnEndedWithItsPod(reading runtime.PodEndReading) {
	h.t.Helper()
	if p := h.claim().Pending; p != nil {
		h.t.Fatalf("the claim holds %+v, want nothing: the turn is a message's", p)
	}
	h.advance(time.Minute)
	h.must(StreamTurnStart{Claim: h.token})
	h.wantState(StateWorking)
	if p := h.claim().Pending; p != nil {
		h.t.Fatalf("the message's turn left %+v pending, want nothing confirmed", p)
	}
	h.advance(time.Minute)
	h.observePodEnd(runtime.Gone, reading)
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

// A claim whose pod something outside it ends in a turn a message started, again after each
// relaunch and again while each relaunch starts, is continued uncharged every time, far past
// launch_failure_limit, and never fails (endedFromOutside); messages are how an architect's turns
// and the controller's start. The open turn is work outstanding with no task behind it, the state
// a death is charged in. An issue's role relaunched so is given the daemon's own relaunch record
// at its ready (queueRelaunch), whose turn completes before the next message's turn; the
// controller is given none.
func TestAMessageStartedTurnEndedFromOutsideFarPastTheLimitNeverFailsTheClaim(t *testing.T) {
	ends := farPastTheLimit()
	for _, kind := range outsideEndClaims {
		t.Run(kind.name, func(t *testing.T) {
			h := newHarnessOf(t, kind.claim())
			h.reach(StateReady)

			for end := 1; end <= ends; end++ {
				if end > 1 {
					h.relaunchRecordRuns(kind.record)
				}
				h.messageTurnEndedWithItsPod(runtime.PodEndNotOurs)

				h.wantState(StateLaunching)
				h.wantNothingCharged("the end in message turn " + strconv.Itoa(end))

				h.startEndedFromOutside(true)

				h.wantState(StateLaunching)
				h.wantNothingCharged("the end of relaunch " + strconv.Itoa(end) + "'s start")

				h.relaunched()
				h.wantState(StateReady)
			}
			h.wantNoFailure()
		})
	}
}

// startEndsNeverCharged ends the claim's start from outside its pod again and again, far past
// launch_failure_limit, finding its agent launching and with its shim connected by turns and never
// letting it come up, and fails unless every end is the uncharged continue: no launch failure, no
// death, and the claim never failed. Then the agent comes up.
func (h *harness) startEndsNeverCharged() {
	h.t.Helper()
	ends := farPastTheLimit()
	for end := 1; end <= ends; end++ {
		h.startEndedFromOutside(end%2 == 0)

		h.wantState(StateLaunching)
		h.wantNothingCharged("end " + strconv.Itoa(end) + " of the start")
	}
	h.wantNoFailure()
	h.relaunched()
	h.wantState(StateReady)
}

// A claim holding its task whose pod something outside it ends again and again while its agent
// starts is continued uncharged every time, far past launch_failure_limit, and never fails
// (endedFromOutside): an end that finds the agent still starting charges no launch failure, as it
// charges a ready agent no death. The claim holds its task from before its first end, and once its
// agent comes up the task is sent.
func TestAStartEndedFromOutsideFarPastTheLimitNeverFailsAClaimHoldingItsTask(t *testing.T) {
	for _, kind := range outsideEndClaims {
		t.Run(kind.name, func(t *testing.T) {
			h := newHarnessOf(t, kind.claim())
			h.launch()
			h.must(RequestDeliver{Claim: h.token, Task: kind.task})
			if p := h.claim().Pending; p == nil || p.Task != kind.task {
				t.Fatalf("pending %+v, want the task %q held while the agent starts", p, kind.task)
			}

			h.startEndsNeverCharged()
			h.wantOwed(kind.task)
		})
	}
}

// A claim with no task whose pod something outside it ends again and again while its agent
// starts is continued uncharged exactly as a claim holding its task is (endedFromOutside). A claim
// relaunched with no task is an ordinary start — an architect a message woke, the controller, a
// phase worker between assignments.
func TestAStartWithNoTaskEndedFromOutsideFarPastTheLimitNeverFailsTheClaim(t *testing.T) {
	for _, kind := range outsideEndClaims {
		t.Run(kind.name, func(t *testing.T) {
			h := newHarnessOf(t, kind.claim())
			h.launch()
			if p := h.claim().Pending; p != nil {
				t.Fatalf("the claim holds %+v, want no task: this case is about a start with no work behind it", p)
			}

			h.startEndsNeverCharged()
			h.wantPrompts(0)
		})
	}
}

// The line an end from outside a claim's pod ends in, as an operator and a production check read
// it: whole, and not as the constant that carries it, so a reword turns this test red rather than
// leaving a search for what each claim got counting nothing.
const continuedLine = "supervise: the claim's pod ended from outside it; continued in its own session and charged nothing"

// Each claim of a pod that ended from outside it says, in a line of its own, what it got for that
// end (endedFromOutside). The runtime's line for the pod's end is written before any claim is
// decided and names every role the end ended, so it cannot say what each got. Every end writes the
// continued line once, at Info, carrying whether the claim's agent was still starting and the
// observation's detail, however many ends come: for a ready agent holding a task, for an idle one,
// and for an agent still starting.
func TestEachClaimLogsWhatItGotForAnEndFromOutsideItsPod(t *testing.T) {
	ends := farPastTheLimit()
	const detail = "the cluster marked the pod a disruption target (TerminationByKubelet)"
	podEndedFromOutside := func(h *harness) {
		h.t.Helper()
		h.must(RuntimeObservation{Observation: runtime.Observation{
			Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now(), EndReading: runtime.PodEndNotOurs, Detail: detail,
		}})
	}
	// wantLines fails unless the continued line was written lines times, the last of them at Info
	// carrying starting and the observation's detail.
	wantLines := func(h *harness, lines int, starting bool) {
		h.t.Helper()
		written := h.records.of(continuedLine)
		if len(written) != lines {
			h.t.Fatalf("%q was written %d times, want %d: one for each end", continuedLine, len(written), lines)
		}
		r := written[lines-1]
		if r.level != slog.LevelInfo {
			h.t.Fatalf("%q is %s, want %s", continuedLine, r.level, slog.LevelInfo)
		}
		for _, want := range []struct{ key, value string }{
			{"starting", strconv.FormatBool(starting)}, {"detail", detail},
		} {
			if got, ok := r.attr(want.key); !ok || got.String() != want.value {
				h.t.Errorf("%q carries %s=%v (present %v), want %q", continuedLine, want.key, got, ok, want.value)
			}
		}
	}

	for _, tc := range []struct {
		name     string
		reach    func(h *harness)
		starting bool
	}{
		{name: "an agent ready with work outstanding", reach: func(h *harness) {
			h.reach(StateReady)
			h.must(RequestDeliver{Claim: h.token, Task: owedTask})
		}},
		{name: "an idle agent", reach: func(h *harness) { h.reach(StateIdle) }},
		{name: "an agent still starting", reach: func(h *harness) { h.launch() }, starting: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarnessOf(t, queuedClaim())
			tc.reach(h)
			for end := 1; end <= ends; end++ {
				podEndedFromOutside(h)

				h.wantState(StateLaunching)
				h.wantNothingCharged("end " + strconv.Itoa(end))
				wantLines(h, end, tc.starting)
				if !tc.starting {
					h.relaunched()
				}
			}
		})
	}
}

// Which of the claim's reports decides an end from outside the pod: the first report, which a
// refused relaunch leaves the claim standing on, records the ended process as decided
// (Claim.DeathCharged) in the store, the daemon restarts, and its orphan sweep reports that same
// process again. Read again as an end from outside the pod, the second report is the same
// uncharged continue. Read neither way, as it is once the runtime's reading of the end has
// expired, it reaches died, which finds the process decided and charges it no death: it
// relaunches it as it relaunches any death already decided, a launch failure the agent's ready
// clears. Left undecided, the one eviction would be charged a death against the task the claim
// holds.
func TestAnEvictionReportedAgainOverTheKeptProcessIsChargedNoDeath(t *testing.T) {
	for _, tc := range []struct {
		name    string
		again   runtime.PodEndReading
		charged Budgets
	}{
		{name: "read again as an end from outside the pod", again: runtime.PodEndNotOurs},
		{name: "read neither way once the reading expired", charged: Budgets{LaunchFailures: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := owingTask(t, queuedClaim(), owedTask)
			dead := h.locator()
			store := &unansweredCheckLaunchStore{memStore: h.store, failing: true}
			h.m.deps.Store = store

			first := RuntimeObservation{Observation: runtime.Observation{
				Locator: dead, Kind: runtime.Gone, At: h.clock.Now(), EndReading: runtime.PodEndNotOurs,
			}}
			if err := h.handle(first); err == nil {
				t.Fatal("the relaunch the end raised was taken, want the lifecycle recheck's refusal of it")
			}
			h.wantNothingCharged("the first report")

			// The daemon restarts in the window the refusal left, so the second report is decided
			// from what the first one wrote and nothing else.
			h.restart()
			h.must(RuntimeObservation{Observation: runtime.Observation{
				Locator: dead, Kind: runtime.Gone, At: h.clock.Now(), EndReading: tc.again,
			}})

			h.wantState(StateLaunching)
			h.wantBudgets(tc.charged)
			h.relaunched()
			h.wantNothingCharged("the relaunch's ready")
			h.wantOwed(owedTask)
		})
	}
}

// An end from outside the pod whose relaunch the wake pacing parked, then a daemon restart: the
// claim is restored standing over its process with that process decided (Claim.DeathCharged), and
// the boot raises again the relaunch the restart lost (RecordedDeath). The end found the agent
// ready in no turn, holding its task unconfirmed or holding nothing, so it ended nothing worth
// telling and recordGoneEnd records nothing of it; the record the decision leaves of it tells
// nothing and says only that the end of that launch was from outside the pod (markFromOutside),
// and the decision's write carries it. The replay reads that record (relaunchRecordedDeath): it
// charges nothing, neither a death nor a launch failure, and resumes the claim's own session,
// which is sent again the task the claim held, and nothing where it held none, as it is where no
// restart comes.
//
// The negative controls are the replay's charged branch alone (relaunchRecordedDeath with no
// branch for an end from outside the pod) and the claim written before the end is recorded
// (endedFromOutside, as it was): either way the replay charges the end a launch failure.
func TestAnEndFromOutsideWhoseRelaunchARestartLostIsRelaunchedUncharged(t *testing.T) {
	for _, row := range []struct {
		name string
		task string
	}{
		{name: "holding its task", task: owedTask},
		{name: "holding nothing"},
	} {
		t.Run(row.name, func(t *testing.T) {
			h := pacedHarness(t, wakeRate)
			h.reach(StateReady)
			if row.task != "" {
				h.must(RequestDeliver{Claim: h.token, Task: row.task})
				h.wantOwed(row.task)
			}
			sent := len(h.prompts())
			queueAhead(h, 1)
			ended := h.locator()
			h.observePodEnd(runtime.Gone, runtime.PodEndNotOurs)
			h.wantWaitingWake(WakeLaunch)

			h.restart()
			h.must(RecordedDeath{Claim: h.token, Generation: h.claim().Generation})

			h.wantNothingCharged("the boot's replay of the end")
			if got := h.claim().EndedWork; got.Cause != endFromOutside || got.Tells() {
				t.Fatalf("the claim records %+v of the end, want a record of an end from outside the pod that tells nothing", got)
			}
			waiting := h.wantWaitingWake(WakeLaunch)
			h.advance(waiting.At.Sub(h.clock.Now()))
			resume := h.wantCalls("Resume", 1)[0]
			if resume.Previous == nil || *resume.Previous != ended || resume.Spec.ResumeSessionFile != sessionFile {
				t.Fatalf("resumed %+v from %+v, want the claim's own recorded session after the ended incarnation",
					resume.Spec, resume.Previous)
			}
			h.relaunched()
			h.wantNothingCharged("the relaunch's ready")
			if row.task == "" {
				h.wantPrompts(sent)
				return
			}
			if resent := h.wantPrompts(sent + 1)[sent]; !strings.HasSuffix(resent.Message, row.task) {
				t.Fatalf("re-sent %+v, want the task %q the claim held", resent, row.task)
			}
		})
	}
}

// An end from outside the pod in a turn a message started, whose relaunch the wake pacing parked,
// then a daemon restart. The claim holds no task, so what the relaunched role is told of the end
// is the record the end wrote of the turn it cut off (EndedWork), delivered on its own at the
// relaunch's ready (queueRelaunch). The decision writes the claim after that record
// (endedFromOutside), so the claim the restart restores carries it, and the boot's replay reads it
// (relaunchRecordedDeath): the relaunch is charged nothing and resumes the claim's own session, and
// the record says that its pod was ended from outside it, never that the process died.
//
// The negative controls are the claim written before the end is recorded (endedFromOutside, as it
// was), where the replay charges a launch failure and the let-go at its turn records the end as a
// death of no account, `the process died: ""`; and the replay's charged branch alone, which charges
// the launch failure.
func TestAnEndFromOutsideInAMessageTurnWhoseRelaunchARestartLostIsToldAsAnEndFromOutside(t *testing.T) {
	h := pacedHarness(t, wakeRate)
	h.reach(StateIdle)
	h.advance(time.Minute)
	h.must(StreamTurnStart{Claim: h.token})
	h.advance(time.Minute)
	queueAhead(h, 1)
	ended := h.locator()
	h.observePodEnd(runtime.Gone, runtime.PodEndNotOurs)
	h.wantWaitingWake(WakeLaunch)

	h.restart()
	h.must(RecordedDeath{Claim: h.token, Generation: h.claim().Generation})

	h.wantNothingCharged("the boot's replay of the end")
	waiting := h.wantWaitingWake(WakeLaunch)
	h.advance(waiting.At.Sub(h.clock.Now()))
	h.wantNothingCharged("the relaunch")
	if got := h.claim().EndedWork.Cause; got != endFromOutside {
		t.Fatalf("the record the relaunched role is told reads %q, want %q", got, endFromOutside)
	}
	resume := h.wantCalls("Resume", 1)[0]
	if resume.Previous == nil || *resume.Previous != ended || resume.Spec.ResumeSessionFile != sessionFile {
		t.Fatalf("resumed %+v from %+v, want the claim's own recorded session after the ended incarnation",
			resume.Spec, resume.Previous)
	}
	sent := len(h.prompts())
	h.relaunched()
	if told, says := h.wantPrompts(sent + 1)[sent], "ended because "+endFromOutside+"."; !strings.Contains(told.Message, says) {
		t.Fatalf("the relaunched role was sent %q, want it to say %q", told.Message, says)
	}
}

// An end from outside the pod ends the turn it interrupted (Claim.LastTurnEnd), as a charged death
// does (chargeDeath, TestADeathEndsTheTurnItInterruptedSoLaterIdleDeathsAreNotCharged): nothing
// else records the end of a turn whose agent is gone. Here a message's turn is cut by an end from
// outside the pod, and the parked agent relaunched after it — holding only the daemon's own
// relaunch record, which is no task — then dies again and again, which the runtime reads neither
// way. Left open, the cut turn would make each of those deaths a death with work outstanding, and
// the claim would fail at the third for work that was never there.
func TestAnEndFromOutsideThePodEndsTheTurnItInterruptedSoLaterDeathsAreNotCharged(t *testing.T) {
	limit := testLimits().LaunchFailures
	h := newHarness(t)
	h.reach(StateIdle)
	h.advance(time.Minute)
	h.must(StreamTurnStart{Claim: testToken})
	h.advance(time.Minute)

	h.observePodEnd(runtime.Gone, runtime.PodEndNotOurs)
	h.wantNothingCharged("the end in the message's turn")
	h.relaunched()
	if p := h.claim().Pending; p == nil || !p.Relaunch() {
		t.Fatalf("the relaunched claim holds %+v, want the relaunch's record alone", p)
	}

	for death := 1; death <= limit+1; death++ {
		h.observePodEnd(runtime.Gone, "")
		if got := h.budgets(); got.Deaths != 0 {
			t.Fatalf("death %d of the parked agent left budgets %+v, want no death charged: "+
				"the turn the end from outside the pod interrupted is over", death, got)
		}
		h.wantState(StateLaunching)
		h.relaunched()
	}
	h.wantState(StateReady)
	h.wantNoFailure()
}

// What a relaunched process is told ended its last one says who ended it (displaced): an end
// something outside the pod made is recorded as an end from outside the pod (endFromOutside),
// whether it is continued in a turn or finds no work outstanding, and only the daemon's own
// replacement of the pod is recorded as a replacement (endDisplaced). Each is recorded first,
// before its take-back (recordGoneEnd), so where a suspension is held for the turn the end cut
// off, the stop that suspension then makes finds the record standing (endHeld): the resumed
// process is told what ended its pod, never that it was suspended. The next process reads the
// cause in the prompt it is sent — the task taken back from the turn, or the record alone for a
// claim that held nothing.
func TestAnEndFromOutsideThePodIsToldAsSuchAndOnlyTheDaemonsReplacementAsAReplacement(t *testing.T) {
	const detail = "the cluster marked the pod a disruption target (TerminationByKubelet)"
	for _, tc := range []struct {
		name      string
		idle      bool
		held      bool
		reading   runtime.PodEndReading
		displaced bool
		cause     string
	}{
		{name: "an end from outside the pod in a turn", reading: runtime.PodEndNotOurs, cause: endFromOutside},
		{name: "an end from outside the pod in a turn a suspension is held for", held: true, reading: runtime.PodEndNotOurs, cause: endFromOutside},
		{name: "an end from outside the pod with no work outstanding", idle: true, reading: runtime.PodEndNotOurs, cause: endFromOutside},
		{name: "the daemon's replacement of the pod in a turn", displaced: true, cause: endDisplaced},
		{name: "the daemon's replacement of the pod in a turn a suspension is held for", held: true, displaced: true, cause: endDisplaced},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			want := EndedWork{Cause: tc.cause, Background: true, Kinds: []string{"subagent"}, Reported: true}
			if tc.idle {
				h.reach(StateIdle)
			} else {
				h.reach(StateWorking)
				want.Turn, want.TaskTurn = true, true
			}
			h.reportActivity("subagent")
			if tc.held {
				held := RequestSuspend{Claim: h.token, By: SuspendedByOperator, Reason: "the operator is draining the daemon"}
				if err := h.handle(held); !errors.Is(err, ErrSuspendHeld) {
					t.Fatalf("the suspension answered %v, want it held for the turn's end", err)
				}
			}
			want.Generation, want.At = h.generation(), h.clock.Now()

			h.must(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now(),
				EndReading: tc.reading, Displaced: tc.displaced, Detail: detail}})

			h.wantEnded(want)
			h.wantEndedLine(tc.cause)
			sent := len(h.prompts())
			if tc.held {
				h.wantState(StateSuspended)
				h.must(RequestResume{Claim: h.token})
				h.reach(StateReady)
			} else {
				h.relaunched()
			}
			if told, says := h.wantPrompts(sent + 1)[sent], "ended because "+tc.cause+"."; !strings.Contains(told.Message, says) {
				t.Errorf("the next process was sent %q, want it to say %q", told.Message, says)
			}
		})
	}
}
