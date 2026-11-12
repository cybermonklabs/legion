package supervise

// What the daemon charges for a process that ended with its pod, by what the runtime read about
// the cause of that end (runtime.Observation.PodEnd). An end something outside the pod made is the
// uncharged continue a displacement gets: the role's own session is resumed at once, neither a
// death nor a launch failure is charged, and the task the role owed is sent again. An end the
// daemon itself made, and one the runtime read neither way, are charged exactly as they were
// before the reading existed — which is what keeps these cases from being an amnesty.

import (
	"strings"
	"testing"

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
// again. The claim holds that task unconfirmed when the end arrives, so the same walk under an end
// the runtime read neither way charges the death and the launch failure it always did: that is the
// control, and it is what makes the uncharged half mean anything.
func TestAnEvictedPodsRoleIsContinuedChargingNoDeathAndSentItsTaskAgain(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reading runtime.PodEndReading
		want    Budgets
	}{
		{name: "something outside the pod ended it", reading: runtime.PodEndNotOurs},
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
		{name: "something outside its pod ended it", reading: runtime.PodEndNotOurs},
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
