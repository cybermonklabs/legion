package supervise

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// The pods these tests start processes in and refuse launches for: the pod a refusal names, and
// two others, the one that replaced it and one more.
const (
	refusedPod  = "0d4e7a1c-5b38-4f62-9c07-3e1a8b5d2f94"
	replacedPod = "8f2c6b0e-1d47-4a93-b5e8-7c0a2d9f3e61"
	anotherPod  = "5a9e3c1d-7f02-4b68-8d4c-2e6f0b1a9c37"
)

// refusedFor is the runtime's refusal of a launch into pod for want of its update round
// (runtime.AwaitingPodRound), naming that pod.
func refusedFor(pod string) func(context.Context) error {
	return func(context.Context) error {
		return fmt.Errorf("launch %s: %w", testToken, &runtime.AwaitingPodRound{Pod: podProcess(pod, 1),
			Reason: "pod legion-legion-209 carries no exchange identity and dispatch_token_transition_file is not set; its update round replaces it"})
	}
}

// wantRegistered holds the set of pods an agent of the claim registered a session in
// (Claim.RegisteredPods) to want, as Claim, View and the accessor the daemon's sweep reads
// (Machine.RegisteredPods) each report it, and holds the store to none of it: the set is memory
// only.
func (h *harness) wantRegistered(want RegisteredPods, when string) {
	h.t.Helper()
	for name, got := range map[string]RegisteredPods{
		"Claim": h.claim().RegisteredPods, "View": h.m.View().RegisteredPods, "the accessor": h.m.RegisteredPods(),
	} {
		if !slices.Equal(got.Pods, want.Pods) || got.Unknown != want.Unknown {
			h.t.Fatalf("%s %s reports the claim registered in %+v, want %+v", when, name, got, want)
		}
	}
	if stored := h.store.load(h.token).RegisteredPods; len(stored.Pods) != 0 || stored.Unknown {
		h.t.Fatalf("%s the store holds the set %+v, want none: it is memory only", when, stored)
	}
}

// A registration adds the pod of the process that registered to the claim's set
// (Claim.RegisteredPods), and nothing else adds to it: not a refusal for a pod, and not a process
// started in a pod before its agent registers. Nothing takes a pod out of it either: a relaunch
// into another pod adds that pod beside the first once its agent registers, and a claim that lost
// its session and started a fresh agent in a third pod still holds both, since an agent of it
// registered in each, and what that agent's worker did not push was on that pod's disk.
func TestARegistrationAddsItsPodToTheSetAndNothingTakesItOut(t *testing.T) {
	h := newHarness(t)
	h.wantRegistered(RegisteredPods{}, "before any launch,")
	h.rt.OnSpawn = refusedFor(refusedPod)
	if err := h.handle(RequestSpawn{Claim: h.token}); !errors.Is(err, runtime.ErrAwaitingPodRound) {
		t.Fatalf("the launch answered %v, want the runtime's wait for the pod's round", err)
	}
	h.wantRegistered(RegisteredPods{}, "at a refusal for a pod,")

	h.rt.OnSpawn = nil
	h.rt.ScriptSpawn(fake.SpawnResult{Locator: podProcess(refusedPod, 2)})
	h.advance(podRoundRetry)
	if c := h.claim(); locatorPod(c.Locator) != refusedPod {
		t.Fatalf("the retry that went through left the claim at %+v, want a process in pod %s", c.Locator, refusedPod)
	}
	h.wantRegistered(RegisteredPods{}, "once a retry started a process in the refused pod, before its agent registered,")
	h.connect()
	h.register()
	h.wantRegistered(RegisteredPods{Pods: []string{refusedPod}}, "once its agent registered in that pod,")
	h.ready()

	h.diesIntoPod(replacedPod)
	h.wantRegistered(RegisteredPods{Pods: []string{refusedPod, replacedPod}}, "once a relaunch into another pod registered there,")

	h.rt.ScriptResume(fake.SpawnResult{Err: fmt.Errorf("resume %s: %w: no store holds it", testToken, runtime.ErrSessionFileMissing)})
	h.rt.ScriptSpawn(fake.SpawnResult{Locator: podProcess(anotherPod, 1)})
	h.observe(runtime.Gone)
	if c := h.claim(); c.Session != "" || c.Abandoned.Session != session || locatorPod(c.Locator) != anotherPod {
		t.Fatalf("the claim records session %q, abandoned %q, at %+v; want its session abandoned and a fresh agent in pod %s",
			c.Session, c.Abandoned.Session, c.Locator, anotherPod)
	}
	h.wantRegistered(RegisteredPods{Pods: []string{refusedPod, replacedPod}}, "once the claim lost its session and started fresh in a third pod,")
}

// A machine built over a stored claim does not know where the claim registered before, so it reads
// the set off the stored claim (bootRegisteredPods), and the set it reads holds no pod. Where the
// stored claim shows a session it held, now or abandoned (Claim.Abandoned), the set is Unknown;
// where it shows none, the set is known and empty. A registered agent, whose process runs in a pod
// or in no pod (a tmux pane), a session whose agent has not registered since its launch, a session
// with no process and an abandoned one all read Unknown: a stored claim whose state holds a
// registered agent always shows the session that agent registered, so the pod its process runs in
// would decide nothing, and it is not read.
//
// The cases after the table drive it on. A registered agent's claim reads Unknown through the
// restart, and a relaunch's registration in another pod after the restart adds that pod and
// leaves the set Unknown: where an earlier agent registered stays unknown. And a registration
// through a process in no pod adds nothing.
func TestAMachineBuiltOverAStoredClaimReadsTheSetOffIt(t *testing.T) {
	inPod := podProcess(refusedPod, 4)
	pane := runtime.Locator{Runtime: runtime.RuntimeTmux, Claim: testToken, Incarnation: "4242:1",
		Tmux: &runtime.TmuxLocator{Window: "@1", Pane: "%1"}}
	for name, tc := range map[string]struct {
		claim Claim
		want  RegisteredPods
	}{
		"no session and none abandoned": {claim: queuedClaim(), want: RegisteredPods{}},
		"a registered agent whose process is in a pod": {claim: func() Claim {
			c := queuedClaim()
			c.State, c.Session, c.SessionFile, c.Locator = StateReady, session, sessionFile, &inPod
			return c
		}(), want: RegisteredPods{Unknown: true}},
		"a registered agent whose process is in no pod": {claim: func() Claim {
			c := queuedClaim()
			c.State, c.Session, c.SessionFile, c.Locator = StateReady, session, sessionFile, &pane
			return c
		}(), want: RegisteredPods{Unknown: true}},
		"a session whose agent has not registered since its launch into a pod": {claim: func() Claim {
			c := queuedClaim()
			c.State, c.Session, c.SessionFile, c.Locator = StateLaunching, session, sessionFile, &inPod
			return c
		}(), want: RegisteredPods{Unknown: true}},
		"a session and no process": {claim: func() Claim {
			c := queuedClaim()
			c.State, c.SuspendedBy, c.Session, c.SessionFile = StateSuspended, SuspendedByOperator, session, sessionFile
			return c
		}(), want: RegisteredPods{Unknown: true}},
		"an abandoned session and none now": {claim: func() Claim {
			c := queuedClaim()
			c.Abandoned = AbandonedSession{Session: "ses_before", Why: "no store holds it", At: epoch}
			return c
		}(), want: RegisteredPods{Unknown: true}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newBareHarness(t)
			h.store.PutClaim(h.ctx, tc.claim)
			h.start(h.store.load(testToken))
			h.wantRegistered(tc.want, "built over the stored claim,")
		})
	}

	t.Run("a registered agent's claim through a restart, and a registration after it", func(t *testing.T) {
		h := newHarness(t)
		h.inPod(refusedPod)
		h.wantRegistered(RegisteredPods{Pods: []string{refusedPod}}, "once ready in the pod,")

		h.restart()
		h.wantRegistered(RegisteredPods{Unknown: true}, "after the restart,")
		h.diesIntoPod(replacedPod)
		h.wantRegistered(RegisteredPods{Pods: []string{replacedPod}, Unknown: true}, "once a relaunch registered in another pod after the restart,")
	})

	t.Run("a registration through a process in no pod", func(t *testing.T) {
		c := queuedClaim()
		c.Abandoned = AbandonedSession{Session: "ses_before", Why: "no store holds it", At: epoch}
		h := newHarnessOf(t, c)
		h.wantRegistered(RegisteredPods{Unknown: true}, "built over a claim that abandoned a session,")
		h.reach(StateRegistered)
		if loc := h.claim().Locator; loc == nil || loc.Sandbox != nil {
			t.Fatalf("the claim registered through %+v, want a process in no pod", loc)
		}
		h.wantRegistered(RegisteredPods{Unknown: true}, "once a session registered through a process in no pod,")
	})
}

// StoredClaim clears every field the machine overlays on the claim it reports (current), each of
// which only the machine holds, in memory: with each of the machine's memory set, the claim it
// reports carries each overlay, and that claim in its stored form equals the claim the machine
// holds in its stored form, which carries none. A field current comes to overlay that StoredClaim
// leaves set fails here once this test sets its memory.
func TestStoredClaimClearsEveryFieldTheMachineOverlays(t *testing.T) {
	h := newHarness(t)
	h.inPod(refusedPod)
	awaited := podProcess(replacedPod, 2)
	h.m.mu.Lock()
	h.m.held = &RequestSuspend{Claim: h.token, By: SuspendedByOperator}
	h.m.compacting = &compacting{}
	h.m.processGone = true
	h.m.awaitsPod = &awaited
	reported, held := h.m.current(), copyClaim(h.m.claim)
	h.m.held, h.m.compacting, h.m.processGone, h.m.awaitsPod = nil, nil, false, nil
	h.m.mu.Unlock()

	if !reported.SuspensionHeld || reported.SuspensionHeldBy != SuspendedByOperator || !reported.Compacting || !reported.ProcessGone ||
		reported.AwaitsPod == nil || len(reported.RegisteredPods.Pods) == 0 {
		t.Fatalf("the claim the machine reports carries %+v, want every overlay set", reported)
	}
	if stored, want := StoredClaim(reported), StoredClaim(held); !reflect.DeepEqual(stored, want) {
		t.Fatalf("the reported claim's stored form is\n%+v\nwant the held claim's\n%+v", stored, want)
	}
}
