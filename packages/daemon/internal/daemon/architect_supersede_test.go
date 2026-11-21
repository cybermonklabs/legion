package daemon

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The resume of a root a person answered raises the architect's entry in the same run
// (workflow.Engine.ResumeAwaitedRoot), and its task is what the architect owes now: which ask was
// answered and where to read it (dispatch://INFRA-2614 R3, R4). Here the architect's claim still
// holds the root's first task, entry 1, unconfirmed: it was sent and acknowledged, and the
// architect's process ended before any turn took it, which ends the wait for that prompt's result
// with the process (supervise's letGo). The root's record is put in its wait directly, as
// answeredResumePath puts it. The resume's start reaches the relaunching claim, and the first
// prompt the architect is sent once it comes up is the resume's task, not entry 1's.
func TestTheResumeOfAnAnsweredRootIsSentBeforeAnUnconfirmedTaskOfALowerEntry(t *testing.T) {
	r := newMainPathRig(t, true)
	r.put(r.waitingRoot())
	r.admitPass()
	r.runStarts()
	r.bringUp()
	machine := r.machine()
	held := machine.Claim().Pending
	if held == nil || held.Generation != 1 || held.Entry != 1 || !held.Prompted || !held.ConfirmedAt.IsZero() {
		t.Fatalf("the architect's first task = %#v, want entry 1 of generation 1, sent and unconfirmed", held)
	}
	if err := machine.Handle(r.ctx, supervise.RuntimeObservation{Observation: runtime.Observation{
		Locator: *machine.Claim().Locator, Kind: runtime.Gone, At: time.Now()}}); err != nil {
		t.Fatalf("end the architect's process: %v", err)
	}
	r.waitMachine()

	root := r.issue()
	root.Phase = phase.AwaitingPerson
	root.Awaiting = &record.Awaiting{Ask: mainPathAsk, Since: record.EventMark{Seq: r.nextSeq()}}
	r.put(root)
	r.recordClaim()
	r.apply("ask-answered", intake.DispatchAskClosed{Key: r.key, Ask: mainPathAsk, Kind: record.AskQuestion, State: "answered",
		Actor: alice, Mark: record.EventMark{Seq: r.nextSeq()}})
	if got := r.issue(); got.Phase != phase.Admitted || got.Awaiting != nil || got.Generation != 1 {
		t.Fatalf("the answered root = %+v, want it admitted again in generation 1 on the resume's handout", got)
	}
	r.runStarts()
	sent := len(r.prompts())
	r.bringUp()

	resume := fmt.Sprintf("A person answered your ask %s on %s", mainPathAsk, r.key)
	prompts := r.prompts()[sent:]
	if len(prompts) == 0 {
		t.Fatalf("the relaunched architect was sent nothing; it holds %#v", r.machine().Claim().Pending)
	}
	if first := prompts[0]; !strings.Contains(first.Message, resume) || first.DeliveryID == held.ID {
		t.Fatalf("the relaunched architect's first prompt is %s: %q, want the resume's task naming %q", first.DeliveryID, first.Message, resume)
	}
	if pending := r.machine().Claim().Pending; pending == nil || pending.Entry != 2 {
		t.Fatalf("the architect holds %#v, want the resume's task of entry 2", pending)
	}
}

// A task a newer entry supersedes is retired, and its retirement releases what it named of a
// person's decisions (the claim store's releaseKeptDecisions): the newer task was composed while
// the older one held them, so it names none of them, and they are the tree's to tell again. The
// catch-up the relaunched architect's ready writes (workflow's claimReady, applied as the daemon
// applies a claim's ready, workflowRuntime.applyTerminal) names what nobody holds or claims, so it
// names that decision, and its publish tells the architect.
//
// The root here is the red test's, with a person's decision on it while it waited for its slot:
// the entry-1 start names that decision, its task is sent and never confirmed, a person takes the
// root out of Legion's work and its architect is suspended, and the person's todo returns it to
// work in the same run at a new entry (workflow.Engine.ReturnToWork, R20 of
// dispatch://INFRA-3320), whose task supersedes entry 1's.
func TestADecisionASupersededTaskNamedIsToldToTheReturnedArchitect(t *testing.T) {
	r := newMainPathRig(t, true)
	r.put(r.waitingRoot())
	ref := r.decide(mainPathAsk)
	r.admitPass()
	r.runStarts()
	r.bringUp()
	held := r.machine().Claim().Pending
	if held == nil || held.Generation != 1 || held.Entry != 1 || !strings.Contains(held.Task, ref) || !held.Prompted || !held.ConfirmedAt.IsZero() {
		t.Fatalf("the architect's task before the park = %#v, want its entry-1 task naming %s, sent and unconfirmed", held, ref)
	}
	if kept := keptDecisions(t, r.pool, r.records, r.key); len(kept) != 1 || kept[0].HeldBy.Delivery != held.ID {
		t.Fatalf("%s keeps %#v, want %s held by %s", r.key, kept, ref, held.ID)
	}

	r.park()
	r.runSuspend()
	r.handBack()
	r.runStarts()

	pending := r.machine().Claim().Pending
	if pending == nil || pending.Generation != 1 || pending.Entry <= held.Entry || strings.Contains(pending.Task, ref) {
		t.Fatalf("the returned architect holds %#v, want a task of a later entry than %d in generation 1, which names nothing %s held", pending, held.Entry, held.ID)
	}
	kept := keptDecisions(t, r.pool, r.records, r.key)
	if len(kept) != 1 || kept[0].Ref != ref || kept[0].HeldBy.Claim != "" || kept[0].ClaimedBy != 0 {
		t.Fatalf("%s keeps %#v, want %s standing, neither held nor claimed, once the task that held it was retired", r.key, kept, ref)
	}

	sent := len(r.prompts())
	r.bringUp()
	if prompts := r.prompts()[sent:]; len(prompts) == 0 || prompts[0].DeliveryID != pending.ID {
		t.Fatalf("the returned architect was prompted %+v, want the return's task %s first", prompts, pending.ID)
	}
	ready := r.machine().Claim()
	w := &workflowRuntime{pool: r.pool, records: r.records, handlers: r.runner.handlers}
	if err := w.applyTerminal(r.ctx, ready, supervise.StateReady); err != nil {
		t.Fatalf("apply the ready of launch %d: %v", ready.Generation, err)
	}

	catchUps := catchUpRows(t, r.pool, r.key)
	if len(catchUps) == 0 {
		t.Fatalf("the ready of launch %d wrote no catch-up of %s", ready.Generation, r.key)
	}
	catchUp := catchUps[len(catchUps)-1]
	payload, err := record.DecodeOutboxPayload(catchUp)
	if err != nil {
		t.Fatalf("decode the catch-up %d: %v", catchUp.ID, err)
	}
	if notice := payload.(record.Notice); notice.CatchUp == nil || !strings.Contains(notice.CatchUp.Decided, ref) {
		t.Fatalf("the catch-up of launch %d is %#v, want it naming %s", ready.Generation, payload, ref)
	}
	if kept := keptDecisions(t, r.pool, r.records, r.key); len(kept) != 1 || kept[0].ClaimedBy != catchUp.ID {
		t.Fatalf("%s keeps %#v, want %s claimed by the catch-up %d", r.key, kept, ref, catchUp.ID)
	}

	catchUp.LeaseToken = leaseOutboxRow(t, r.pool, catchUp.ID)
	if err := r.runner.execute(r.ctx, catchUp); err != nil {
		t.Fatalf("run the catch-up %d: %v", catchUp.ID, err)
	}
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		return r.records.FinishOutbox(r.ctx, tx, catchUp.ID, catchUp.LeaseToken)
	}); err != nil {
		t.Fatalf("finish the catch-up %d: %v", catchUp.ID, err)
	}
	told := false
	publisher := r.runner.notices.(*outboxPublisher)
	for _, published := range publisher.publishes() {
		notice, ok := published.payload.(record.Notice)
		told = told || (ok && notice.CatchUp != nil && strings.Contains(notice.CatchUp.Decided, ref) &&
			strings.HasSuffix(published.topic, strings.ToLower(string(r.token))))
	}
	if !told {
		t.Fatalf("the architect was published %+v, want a catch-up to %s naming %s", publisher.publishes(), r.token, ref)
	}
	if kept := keptDecisions(t, r.pool, r.records, r.key); len(kept) != 0 {
		t.Fatalf("%s keeps %#v after the catch-up told it, want nothing left to tell", r.key, kept)
	}
}
