package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// heldTaskTurnBound is the rig's wait for the turn of an acknowledged prompt
// (supervise.Timeouts.RPC): the five seconds of production, and a wait none of the rig's other
// timers is armed for — boot, probe and stop are a second each (newOutboxSupervisor) — so firing
// it fires the turn's wait and nothing else.
const heldTaskTurnBound = 5 * time.Second

// heldTaskClock gives the rig's machines clock, whose timers run only when the test fires them,
// and heldTaskTurnBound for the wait on an acknowledged prompt's turn.
func heldTaskClock(clock *heldClock) heldTaskOption {
	return func(r *heldTaskRig) {
		r.sup.deps.Clock = clock
		r.sup.deps.Timeouts.RPC = heldTaskTurnBound
	}
}

// A root architect's start task names a decision a person made while nobody in the root's tree
// could hear it, and the turn that serves the task starts late: Oh My Pi acknowledges the prompt,
// spends longer than the five-second wait on its pre-turn work, so the daemon takes the task back
// under a new id (supervise's noTurn), and the next send of it is answered busy, because Oh My Pi
// is preparing that very turn. When the turn starts it is the first prompt's, still outstanding
// (supervise.Machine.outstanding), so it confirms the task, and the write that records the
// confirmation tells the decision the task's delivery holds (store's tellKeptDecisions): its row
// goes. The prompt's result settles the task, and nothing names the decision again — neither the
// turn's end, nor the catch-up of the architect's next launch, nor the next start sent to it.
//
// Before, the late start confirmed nothing, the row stayed held by the delivery, and the task was
// sent again at the turn's end, naming the decision once more: dispatch://INFRA-3135 measured the
// same `decided` notice four times from 05:52Z, and again in a catch-up at 05:55:02Z
// (dispatch://INFRA-3276, spec version 26).
func TestAKeptDecisionToldInALateTurnIsToldOnce(t *testing.T) {
	clock := &heldClock{}
	r := newHeldTaskRig(t, heldTaskClock(clock))
	ref := "dispatch://" + heldTaskRoot + "/ask/ask-late-turn"
	keepDecision(t, r.pool, r.records, heldTaskRoot, ref, time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	r.runRow(r.enqueueStart())
	machine, _ := r.sup.Machine(r.token)

	prompts := r.conn.Prompts()
	if len(prompts) != 1 || !strings.Contains(prompts[0].Message, ref) {
		t.Fatalf("the architect was prompted %#v, want the start's task naming %s", prompts, ref)
	}
	first := prompts[0].DeliveryID
	acknowledged := r.pending()
	if acknowledged == nil || acknowledged.ID != first || acknowledged.DeliveredAt.IsZero() || !acknowledged.ConfirmedAt.IsZero() {
		t.Fatalf("the architect holds %#v, want the task acknowledged as %s and not yet confirmed", acknowledged, first)
	}
	if named := r.named(*acknowledged); len(named.Decisions) != 1 || named.Decisions[0].Ref != ref {
		t.Fatalf("the delivery names %#v, want %s", named.Decisions, ref)
	}
	r.wantHeldBy(ref, first)

	// Oh My Pi's pre-turn work outlasts the wait for the turn: the task goes back under a new id,
	// and the first prompt stays outstanding.
	if live, _ := clock.armed(heldTaskTurnBound); live != 1 {
		t.Fatalf("%d timers are armed for %s, want the one wait for the acknowledged prompt's turn", live, heldTaskTurnBound)
	}
	if fired := clock.fire(heldTaskTurnBound); fired != 1 {
		t.Fatalf("firing the wait for the turn ran %d timers, want 1", fired)
	}
	machine.Wait()
	taken := r.pending()
	if taken == nil || taken.ID == first || !taken.ConfirmedAt.IsZero() {
		t.Fatalf("the architect holds %#v after the wait ran out, want the task back under an id of its own, unconfirmed", taken)
	}
	r.wantHeldBy(ref, taken.ID)

	// The sweep sends the task again, and Oh My Pi, preparing the first prompt's turn, answers busy.
	r.conn.RefusePrompt(busyAnswer)
	if err := machine.Handle(r.ctx, aliveObservation(machine)); err != nil {
		t.Fatalf("post the sweep's observation: %v", err)
	}
	machine.Wait()
	r.conn.FailPrompt(nil)
	prompts = r.conn.Prompts()
	if len(prompts) != 2 || prompts[1].DeliveryID == first || !strings.Contains(prompts[1].Message, ref) {
		t.Fatalf("the architect was prompted %#v, want the task sent once more under an id of its own", prompts)
	}

	// The first prompt's turn starts, late.
	if err := machine.Handle(r.ctx, supervise.StreamTurnStart{Claim: r.token}); err != nil {
		t.Fatalf("start the late turn: %v", err)
	}
	machine.Wait()
	if confirmed := r.pending(); confirmed == nil || confirmed.ConfirmedAt.IsZero() {
		t.Fatalf("the architect holds %#v in the late turn, want its task confirmed by the turn of its outstanding prompt %s",
			confirmed, first)
	}
	if kept := keptDecisions(t, r.pool, r.records, heldTaskRoot); len(kept) != 0 {
		t.Fatalf("%s keeps %s after the late turn confirmed the task, want %s told by the confirmation's write and its row gone",
			heldTaskRoot, keptRefs(kept), ref)
	}

	// The turn ends, and the first prompt's result settles the task.
	for _, ev := range []supervise.Event{
		supervise.StreamTurnEnd{Claim: r.token},
		supervise.StreamPromptResult{Claim: r.token, DeliveryID: first, AgentInvoked: true},
	} {
		if err := machine.Handle(r.ctx, ev); err != nil {
			t.Fatalf("handle %T: %v", ev, err)
		}
		machine.Wait()
	}
	if pending := r.pending(); pending != nil {
		t.Fatalf("the architect still holds %#v, want the task settled by its prompt's result", pending)
	}
	if stored := r.storedDelivery(); stored != nil {
		t.Fatalf("the store still holds %#v, want the settled task gone", stored)
	}
	if prompts := r.conn.Prompts(); len(prompts) != 2 {
		t.Fatalf("the architect was prompted %d times, want the two sends before the turn and none after it: %#v", len(prompts), prompts)
	}

	// The next start sent to the claim names what was decided since, and not what was told. Its
	// turn takes it in time and its result settles it, so the claim holds nothing again.
	later := "dispatch://" + heldTaskRoot + "/ask/ask-after-the-turn"
	keepDecision(t, r.pool, r.records, heldTaskRoot, later, time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC))
	r.readmit()
	r.runRow(r.enqueueStart())
	prompts = r.conn.Prompts()
	if len(prompts) != 3 || !strings.Contains(prompts[2].Message, later) || strings.Contains(prompts[2].Message, ref) {
		t.Fatalf("the architect was prompted %#v, want the next start's task naming %s and not %s", prompts, later, ref)
	}
	r.turnOn(machine, prompts[2].DeliveryID)
	if kept := keptDecisions(t, r.pool, r.records, heldTaskRoot); len(kept) != 0 {
		t.Fatalf("%s keeps %s after the next start's turn, want nothing", heldTaskRoot, keptRefs(kept))
	}

	// The architect's next launch: its catch-up names what nobody holds, which is nothing now.
	before := machine.Claim().Generation
	r.relaunchArchitect(machine)
	launch := machine.Claim().Generation
	if launch <= before || !supervise.SaidReady(machine.Claim().State) {
		t.Fatalf("the architect after its relaunch is %s at launch %d, want it ready at a launch after %d",
			machine.Claim().State, launch, before)
	}
	ready := &workflowRuntime{pool: r.pool, records: r.records, log: quietLogger(), handlers: []intake.Handler{r.engine, r.admission}}
	if err := ready.applyTerminal(r.ctx, machine.Claim(), supervise.StateReady); err != nil {
		t.Fatalf("apply the ready of launch %d: %v", launch, err)
	}
	if !r.catchUpOf(launch) {
		t.Fatalf("no catch-up of %s was written for launch %d", heldTaskRoot, launch)
	}
	if naming := namingRef(catchUpsNaming(t, r.pool, heldTaskRoot, 0, ref)); naming != 0 {
		t.Fatalf("%d catch-ups of %s name %s, want none: the late turn told it", naming, heldTaskRoot, ref)
	}
	// Nothing sent to the claim since the late turn — the next start's task, and whatever the
	// relaunch sent — names it either.
	for _, prompt := range r.conn.Prompts()[2:] {
		if strings.Contains(prompt.Message, ref) {
			t.Fatalf("the prompt %q names %s, which the late turn told", prompt.Message, ref)
		}
	}
}

// turnOn is the architect's turn on the task it was sent as delivery: the turn starts, ends, and
// Oh My Pi reports that prompt's result behind its end, which settles the task.
func (r *heldTaskRig) turnOn(machine *supervise.Machine, delivery string) {
	r.t.Helper()
	for _, ev := range []supervise.Event{
		supervise.StreamTurnStart{Claim: r.token},
		supervise.StreamTurnEnd{Claim: r.token},
		supervise.StreamPromptResult{Claim: r.token, DeliveryID: delivery, AgentInvoked: true},
	} {
		if err := machine.Handle(r.ctx, ev); err != nil {
			r.t.Fatalf("handle %T: %v", ev, err)
		}
		machine.Wait()
	}
	if pending := r.pending(); pending != nil {
		r.t.Fatalf("the architect still holds %#v after the turn on %s, want it settled", pending, delivery)
	}
}

// wantHeldBy fails the test unless the root keeps ref as its one decision, held by the architect's
// claim under delivery: the row the delivery's confirmation tells (store's holdKeptDecisions).
func (r *heldTaskRig) wantHeldBy(ref, delivery string) {
	r.t.Helper()
	kept := keptDecisions(r.t, r.pool, r.records, heldTaskRoot)
	if len(kept) != 1 || kept[0].Ref != ref || kept[0].HeldBy.Claim != string(r.token) || kept[0].HeldBy.Delivery != delivery {
		r.t.Fatalf("%s keeps %#v, want %s alone, held by %s's delivery %s", heldTaskRoot, kept, ref, r.token, delivery)
	}
}

// relaunchArchitect ends the architect's process as the runtime's sweep sees a process gone, which
// the supervisor answers with a launch of the same session, and brings the agent of that launch up
// as launchArchitect brings the first: its connection, the shim's hello, its registration and its
// ready.
func (r *heldTaskRig) relaunchArchitect(machine *supervise.Machine) {
	r.t.Helper()
	if err := machine.Handle(r.ctx, supervise.RuntimeObservation{Observation: runtime.Observation{
		Locator: *machine.Claim().Locator, Kind: runtime.Gone, At: time.Now()}}); err != nil {
		r.t.Fatalf("end the architect's process: %v", err)
	}
	machine.Wait()
	r.sup.deps.Conns.(*fake.Conns).Register(r.token, r.conn)
	launch := machine.Claim().Generation
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: r.token, Generation: launch},
		supervise.RequestRegister{Claim: r.token, Generation: launch, Session: "ses-architect", SessionFile: "/tmp/architect.jsonl"},
		supervise.RequestReady{Claim: r.token, Generation: launch, Session: "ses-architect"},
	} {
		if err := machine.Handle(r.ctx, ev); err != nil {
			r.t.Fatalf("handle %T for the relaunched architect: %v", ev, err)
		}
	}
	machine.Wait()
}

// catchUpOf is whether the outbox holds a catch-up of the root written for launch.
func (r *heldTaskRig) catchUpOf(launch uint64) bool {
	r.t.Helper()
	for _, row := range catchUpRows(r.t, r.pool, heldTaskRoot) {
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			r.t.Fatalf("decode the catch-up %d of %s: %v", row.ID, heldTaskRoot, err)
		}
		if notice, ok := payload.(record.Notice); ok && notice.CatchUp != nil && notice.CatchUp.Launch == launch {
			return true
		}
	}
	return false
}
