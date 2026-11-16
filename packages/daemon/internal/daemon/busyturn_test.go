package daemon

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// aliveObservation is the runtime's sweep finding the claim's process running, as the daemon
// posts it.
func aliveObservation(m *supervise.Machine) supervise.RuntimeObservation {
	return supervise.RuntimeObservation{Observation: runtime.Observation{Locator: *m.Claim().Locator, Kind: runtime.Alive, At: time.Now()}}
}

// A claim a busy answer recorded working holds the issue's working copy for its turn as any working
// claim does (claimInTurn), so the tester's phase task waits behind it. That turn's end is posted to
// the issue's other claims whichever way it comes: the agent's own agent_end, or the sweep's ask
// finding the agent in no prompt (supervise's askTurnOver), which reaches the machine on an
// observation rather than a turn end and is told by the count of turns the machine has ended
// (supervise.Machine.TurnsEnded, wakeIssue). The tester's task is sent then, and not a sweep later.
func TestABusyRecordedTurnEndReleasesTheIssuesHeldTask(t *testing.T) {
	for _, tc := range []struct {
		name string
		// end is what ends the implementer's turn once its agent stops streaming, posted through
		// the supervisor's own feed, which is where the worker stream and the runtime's sweep post
		// their events.
		end func(implementer claim.Token, writing *supervise.Machine) supervise.Event
	}{
		{name: "by the agent's own end of it", end: func(implementer claim.Token, _ *supervise.Machine) supervise.Event {
			return supervise.StreamTurnEnd{Claim: implementer}
		}},
		{name: "by the sweep's ask", end: func(_ claim.Token, writing *supervise.Machine) supervise.Event {
			return aliveObservation(writing)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newIssueRig(t)
			// The issue is in the implementing phase until the implementer's own turn finishes it.
			var implementing atomic.Bool
			implementing.Store(true)
			rig.sup.deps.PhaseHolds = func(_ context.Context, _ string, p phase.Phase, _ uint64) (bool, error) {
				return p != phase.Implementing || implementing.Load(), nil
			}
			implementerConn := fake.NewConn()
			implementer, writing := rig.ready(claim.RoleImplementer, implementerConn)
			testerConn := fake.NewConn()
			tester, waiting := rig.ready(claim.RoleTester, testerConn)

			// The implementer's agent is in a turn of its own, whose start the daemon never read,
			// and its phase task is answered busy: the claim is recorded working for that turn.
			implementerConn.SetStreaming(true)
			implementerConn.RefusePrompt(keptBusyRefusal)
			if err := writing.Handle(rig.ctx, supervise.RequestDeliver{Claim: implementer, Task: "implement the plan", ID: "outbox:1",
				Phase: phase.Implementing, Generation: 1}); err != nil {
				t.Fatalf("deliver the implementer's task: %v", err)
			}
			writing.Wait()
			if view := writing.View(); view.State != supervise.StateWorking {
				t.Fatalf("the implementer is %s after its busy answer, want it recorded working", view.State)
			}
			// In that turn the implementer finishes its phase, and the issue moves on to testing.
			implementing.Store(false)

			if err := waiting.Handle(rig.ctx, supervise.RequestDeliver{Claim: tester, Task: "test the branch", ID: "outbox:2",
				Phase: phase.Testing, Generation: 1}); err != nil {
				t.Fatalf("deliver the tester's task: %v", err)
			}
			waiting.Wait()
			if prompts := testerConn.Prompts(); len(prompts) != 0 {
				t.Fatalf("the tester was prompted %+v while the implementer's busy turn ran; want its task held", prompts)
			}
			// A sweep that finds the implementer's agent still streaming ends nothing.
			if err := writing.Handle(rig.ctx, aliveObservation(writing)); err != nil {
				t.Fatalf("the sweep over the streaming implementer: %v", err)
			}
			if view := writing.View(); view.State != supervise.StateWorking {
				t.Fatalf("the implementer is %s after a sweep of its streaming agent, want it working", view.State)
			}
			if prompts := testerConn.Prompts(); len(prompts) != 0 {
				t.Fatalf("the tester was prompted %+v while the implementer's agent was still streaming; want its task held", prompts)
			}

			implementerConn.SetStreaming(false)
			rig.sup.post(implementer, tc.end(implementer, writing))

			testwait.Eventually(t, "the tester's task to be sent once the implementer's turn ended", func() bool {
				return len(testerConn.Prompts()) == 1
			})
			if sent := testerConn.Prompts()[0]; sent.Message != "test the branch" {
				t.Fatalf("the tester was sent %+v, want its own task", sent)
			}
			if view := writing.View(); view.State != supervise.StateIdle {
				t.Fatalf("the implementer is %s after its turn ended, want it idle", view.State)
			}
			// A send the turn's end made would run on the machine's send goroutine: wait for it.
			writing.Wait()
			if sent := implementerConn.Prompts(); len(sent) != 1 {
				t.Fatalf("the implementer was sent %+v, want only the send its busy answer refused: its turn finished the phase that task was for", sent)
			}
		})
	}
}

// The stall backstop takes every working claim whose last step is past its role's bound
// (backstopStalledTurns). A busy answer records an idle worker working, and that worker's last
// step is the one its previous turn left, which can be older than the bound: the record writes a
// step of its own, the start of the turn it stands for (supervise's recordBusyTurn), so the next
// minute's sweep leaves the claim alone, whether it reads the claims as the store holds them now or
// read them before that step was written and leaves the machine to judge (supervise's
// backstopStalledTurn).
func TestABusyRecordedClaimIsNotBackstopped(t *testing.T) {
	rig := newBackstopRig(t)
	idle := rig.claim(claim.RoleImplementer, "LEGION-209", backstopBound+30*time.Minute, true)
	idle.State, idle.Pending = supervise.StateReady, nil
	rig.supervising(idle)
	m, ok := rig.sup.Machine(idle.Token)
	if !ok {
		t.Fatal("the idle claim is not supervised")
	}
	conn := fake.NewConn()
	conn.SetStreaming(true)
	conn.RefusePrompt(keptBusyRefusal)
	rig.sup.deps.Conns.(*fake.Conns).Register(idle.Token, conn)

	if err := m.Handle(context.Background(), supervise.RequestDeliver{Claim: idle.Token, Task: "work the phase", ID: "outbox:3"}); err != nil {
		t.Fatalf("deliver the task: %v", err)
	}
	m.Wait()
	held := m.Claim()
	if held.State != supervise.StateWorking {
		t.Fatalf("the claim is %s after the busy answer, want it recorded working", held.State)
	}
	if last := held.Step.LastStep(); !last.Add(backstopBound).Before(time.Now()) {
		t.Fatalf("the machine's own copy of the step reads %v, want the earlier turn's, past the bound: the sweep below must post", last)
	}
	written := rig.claimStore()
	written.mu.Lock()
	stored := written.claims[idle.Token]
	written.mu.Unlock()
	if last := stored.Step.LastStep(); last.Add(backstopBound).Before(time.Now()) {
		t.Fatalf("the stored step reads %v after the busy answer, want the start of the turn it recorded", last)
	}

	// The sweep reads the claims as the store holds them, and as a sweep that read them before the
	// record's step was written holds them.
	rig.sweep([]supervise.Claim{stored})
	rig.sweep([]supervise.Claim{held})
	rig.settled(idle)

	if resumes := rig.resumes(idle.Token); len(resumes) != 0 {
		t.Fatalf("the backstop relaunched the claim %d times, want it left alone: its busy turn began a moment ago", len(resumes))
	}
	if told := rig.reported(); told != 0 {
		t.Fatalf("the machines' reports = %d, want none: no turn was ended", told)
	}
}

// A claim a busy answer recorded working holds its pod as any claim in a turn does: the idle rule
// releases only a pod whose live claims are ready or idle (workflow.IdleRelease's IdleHoldInTurn),
// and it reads the state the machine wrote to the store. The pod the rig leaves is one the rule
// releases, which is the control; the same pod, once its implementer's task is answered busy, is
// held in_turn, and the daemon's own pass queues none of a release's rows.
func TestABusyRecordedClaimIsNotReleasedAsIdle(t *testing.T) {
	rig := newIdleRig(t)
	ctx := context.Background()
	decide := func() workflow.IdleDecision {
		t.Helper()
		var decision workflow.IdleDecision
		if _, err := rig.st.ReleaseIdleIssue(ctx, "legion", rig.issue.Key, func(view pods.IdleView) (store.IdleReleaseMark, error) {
			decision = workflow.IdleRelease(view, time.Now().UTC(), rig.w.idlePeriod, true, api.DaemonAPIVersion)
			return store.IdleReleaseMark{}, nil
		}); err != nil {
			t.Fatalf("read the pod's idle decision: %v", err)
		}
		return decision
	}
	if decision := decide(); !decision.Release {
		t.Fatalf("the rule holds the rig's quiet pod for %s, want it released: the hold below means nothing otherwise", decision.Hold)
	}

	// The daemon's sweep finds the idle implementer's process alive and sends it its phase's task,
	// and Oh My Pi answers the send busy.
	conn := fake.NewConn()
	conn.RefusePrompt(keptBusyRefusal)
	rig.sup.deps.Conns.(*fake.Conns).Register(rig.token, conn)
	if err := rig.machine.Handle(ctx, aliveObservation(rig.machine)); err != nil {
		t.Fatalf("the sweep over the idle implementer: %v", err)
	}
	rig.machine.Wait()
	if sent := conn.Prompts(); len(sent) != 1 {
		t.Fatalf("sends = %+v, want the one the busy answer refused", sent)
	}
	if got := rig.machine.Claim().State; got != supervise.StateWorking {
		t.Fatalf("the implementer is %s after the busy answer, want it recorded working", got)
	}

	if decision := decide(); decision.Release || decision.Hold != workflow.IdleHoldInTurn {
		t.Fatalf("the rule's decision over the busy-recorded pod = %+v, want it held %s", decision, workflow.IdleHoldInTurn)
	}
	rig.w.releaseIdlePod(ctx, rig.issue.Key)
	if got := rig.rows(t); len(got) != 0 {
		t.Fatalf("queued rows = %v, want none: the pod is held for the implementer's turn", got)
	}
	if rig.marked(t) {
		t.Fatal("the pod was marked for release while its implementer was in a turn")
	}
	if got := rig.machine.Claim(); got.State != supervise.StateWorking || got.Released() {
		t.Fatalf("the implementer after the release pass = %s by %s, want it still working", got.State, got.SuspendedBy)
	}
}
