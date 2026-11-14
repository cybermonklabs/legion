package daemon

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/projection"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The state carries the handshake between the daemon and a root's architect: beside the two entry
// numbers, the time the task of that entry was first queued and the time the architect confirmed
// it (dispatch://INFRA-2614, R6). The absence of both times is what marks a root of the release
// before this one, whose migration gives the claim the two numbers and no times (R8), and the live
// reading of R9 reads both times once the architect has acted on its task.
//
// An architect acts in the turn that confirmed its task, and that turn ends. So the two times
// outlast the turn: a root whose architect took its first task and ended its turn still stands at
// admitted with its two entries level, and its state still says when that task was queued and
// when the architect took it. Without them it reads exactly as a root of the release before this
// one, and as a root that was queued nothing.
//
// The path is main's own: admission queues the start, the outbox runner launches the architect on
// it, the agent comes up and is prompted the task, and the turn that task starts runs and ends.
func TestTheArchitectHandshakeOutlastsTheTurnThatConfirmedIt(t *testing.T) {
	r := newMainPathRig(t, true)
	r.put(r.waitingRoot())
	r.admitPass()
	r.runStarts()
	r.bringUp()
	if prompts := r.prompts(); len(prompts) != 1 {
		t.Fatalf("the architect was prompted %#v, want its one start task", prompts)
	}
	machine := r.machine()
	pending := machine.Claim().Pending
	if pending == nil {
		t.Fatal("the architect holds no task after its start, so this test proves nothing")
	}

	// The turn the task's prompt starts confirms it, and the state shows both times while it runs.
	if err := machine.Handle(r.ctx, supervise.StreamTurnStart{Claim: r.token, DeliveryID: pending.ID}); err != nil {
		t.Fatalf("start the architect's turn: %v", err)
	}
	r.waitMachine()
	during := r.rootView()
	if during.EntryQueuedAt == nil || during.EntryConfirmedAt == nil {
		t.Fatalf("the state during the architect's turn carries entryQueuedAt %v and entryConfirmedAt %v, want both: "+
			"this is the handshake the test holds past the turn", during.EntryQueuedAt, during.EntryConfirmedAt)
	}

	// The turn ends, and the prompt's result retires the task the claim held.
	for _, ev := range []supervise.Event{
		supervise.StreamTurnEnd{Claim: r.token},
		supervise.StreamPromptResult{Claim: r.token, DeliveryID: pending.ID, AgentInvoked: true},
	} {
		if err := machine.Handle(r.ctx, ev); err != nil {
			t.Fatalf("handle %T for the architect: %v", ev, err)
		}
	}
	r.waitMachine()
	if held := machine.Claim().Pending; held != nil {
		t.Fatalf("the architect holds %#v after the turn its task ran, want the task retired", held)
	}

	after := r.rootView()
	if after.Phase != phase.Admitted || after.PhaseEntry != 1 || after.GivenEntry != 1 {
		t.Fatalf("the root after its architect's turn = phase %s, entry %d, given %d; want admitted with both entries at 1",
			after.Phase, after.PhaseEntry, after.GivenEntry)
	}
	if after.EntryQueuedAt == nil || after.EntryConfirmedAt == nil {
		t.Fatalf("the state after the architect's turn ended carries entryQueuedAt %v and entryConfirmedAt %v, want the %s and %s "+
			"the turn left: with neither, a root this release served reads as a root of the release before it (R8), "+
			"and nothing is left for the live reading of R9", after.EntryQueuedAt, after.EntryConfirmedAt,
			during.EntryQueuedAt, during.EntryConfirmedAt)
	}
	if !after.EntryQueuedAt.Equal(*during.EntryQueuedAt) || !after.EntryConfirmedAt.Equal(*during.EntryConfirmedAt) {
		t.Fatalf("the state after the turn carries queued %s and confirmed %s, want the %s and %s the turn confirmed",
			after.EntryQueuedAt, after.EntryConfirmedAt, during.EntryQueuedAt, during.EntryConfirmedAt)
	}
}

// rootView is the rig's root as the state projects it now, from the record and the architect's
// claim as the supervisor holds it.
func (r *mainPathRig) rootView() api.Issue {
	r.t.Helper()
	var state api.State
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		state, err = projection.Project(r.ctx, tx, r.records, mainPathProject, config.DesignGateRootIssues.Steps(),
			[]supervise.Claim{r.machine().Claim()}, nil, nil, time.Now().UTC())
		return err
	}); err != nil {
		r.t.Fatalf("project the state: %v", err)
	}
	view, known := state.Issues[r.key]
	if !known {
		r.t.Fatalf("%s is in no issue of the projected state", r.key)
	}
	return view
}
