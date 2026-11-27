package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The order proofs of the retro row of TestPendingBasisWaitsOnTheAncestorRequest
// (dispatch://INFRA-3904, the 29-minute silence of CI job 140311446907 at f1db1daf). P stands at
// accepting with its open child R, whose spec step waits on P's approval request; P's sign-off is
// refused while R waits; the person moves P to retro on Dispatch's board. The settle of that move
// queues two outbox rows in one transaction, and the outbox runs them in no fixed order:
//
//   - R's pending-basis seed, which reads P on Dispatch and ends R's wait when P shows retro
//     (outbox.go:4590, seedPendingBasis, at 1341b5f8);
//   - P's status write: admission moves P from accepting to awaiting_children, since it holds its
//     open child, and writes that phase's status, in_progress, to Dispatch, carrying as its
//     observed status the person's own retro (awaiting_children.go:143-144 and effects.go:273, at
//     1341b5f8), so the outbox's check that Dispatch still shows what the row observed
//     (statusMoved, outbox.go:1317, called at :1136, at 1341b5f8) lets it through.
//
// A seed that runs first reads retro and ends the wait. A seed that runs after the status write
// reads in_progress, which is what the registration's own seed read, so its fact is a duplicate
// (the seed's fact id is built from what its read found, outbox.go:4601 at 1341b5f8) and nothing
// further cues R: R waits on P's request, P waits on its open child R, and P's sign-off is refused
// while R waits. Each proof holds one of the two rows through the rig's own outbox hold
// (overrides.holdOutbox), so each order runs on purpose rather than as the machine's load falls:
//
//   - TestAPersonsRetroEndsTheWaitWhenTheSeedRunsAfterPsStatusWrite holds the seed until P's
//     status rows are done;
//   - TestAPersonsRetroEndsTheWaitWhenTheSeedRunsBeforePsStatusWrite holds P's status write until
//     the wait ended;
//   - TestAPersonsRetroWhileTheDaemonIsDownEndsTheWaitAfterTheBoot moves P to retro while no daemon
//     runs, and holds the seed through the boot until P's status rows are done. The boot queues
//     every waiting step's pending-basis seed as its runtime is built (queueRecordedGatesPublish,
//     workflow.go:947 at 1341b5f8), runs its listing's admission pass before it serves
//     (daemon.go:758 at 1341b5f8) and the status repair in its boot fact (resume.go:77 at
//     1341b5f8). At 1341b5f8 none of the three writes P's status over the person's retro, so this
//     proof passes there; it keeps the boot's order of those rows from growing the live race.
//
// Each proof's verdict is read once the held row has run, not by a wait on the state: the seed's
// fact and the wait it ends are one transaction, so once the outbox holds none of R's seeds the
// outcome is decided, and a wait that did not end is said at once rather than at the test's
// deadline.

// refuseSignOff writes P into accepting by hand and has its architect's sign-off refused while R
// waits on P's approval request (SIGNOFF_RESTING_ROOT_IN_WORKFLOW), so P stands at accepting with
// R still waiting: where the retro row of TestPendingBasisWaitsOnTheAncestorRequest and the order
// proofs here start the person's move.
func (r *personHandoverRig) refuseSignOff(run pendingRun) {
	t := r.t
	t.Helper()
	r.moveRecord(run.parent.Key, phase.Accepting)
	// The sign-off names P's architect, P's generation and the run of the step P stands in, as the
	// route reads them (api's signOff).
	var entry record.StepEntry
	if err := store.Transact(t.Context(), r.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		entry, err = record.NewStore().OpenStep(ctx, tx, run.parent.Key)
		return err
	}); err != nil {
		t.Fatalf("read the run of the step %s stands in: %v", run.parent.Key, err)
	}
	refused := r.applyAsTheDaemon("api", "sign-off-"+run.parent.Key, intake.SignOff{Issue: run.parent.Key,
		By: r.architectToken(run.parent.Key), Generation: r.record(run.parent.Key).Generation, Entry: entry})
	if refused.Refusal == nil || refused.Refusal.Code != "SIGNOFF_RESTING_ROOT_IN_WORKFLOW" || !strings.Contains(refused.Refusal.Message, run.root.Key) {
		t.Fatalf("the sign-off of %s while %s waits on its request = %+v, want SIGNOFF_RESTING_ROOT_IN_WORKFLOW naming %s", run.parent.Key, run.root.Key, refused.Refusal, run.root.Key)
	}
	if standing := r.record(run.parent.Key).Phase; standing != phase.Accepting {
		t.Fatalf("%s stands at %s after its refused sign-off, want accepting: the daemon does not record it in retro", run.parent.Key, standing)
	}
	r.pendingRegistered(run.basisRun, run.ask, true, phase.Admitted)
	t.Logf("the sign-off of %s was refused while %s waits on its request: %s", run.parent.Key, run.root.Key, refused.Refusal.Message)
}

// retroWorld boots a rig whose outbox holds what held holds, and builds one pending run in it: R's
// spec step registered on P's open approval request, P at accepting after its refused sign-off,
// and none of either issue's Dispatch rows left in the outbox.
func retroWorld(t *testing.T, held *outboxHold, names []string) (*personHandoverRig, pendingRun) {
	t.Helper()
	r := newPersonHandoverRig(t, 16)
	r.tune = func(o *overrides) { o.holdOutbox = held.holds }
	r.boot()
	// The record of what the daemon writes to the outbox (writtenNotices, which pendingEnded reads)
	// starts before any issue of the proof exists, and stays on the database across a boot.
	recordOutboxWrites(t, r.pool)
	run := r.pendingWorld(names, pendingOptions{place: ancestorLive})
	r.registerOn(run.basisRun)
	r.pendingRegistered(run.basisRun, run.ask, true, phase.Admitted)
	r.dispatchRowsDone(run.root.Key)
	r.refuseSignOff(run)
	r.dispatchRowsDone(run.root.Key)
	r.dispatchRowsDone(run.parent.Key)
	return r, run
}

// retroAccepted is what the end of R's wait names when the seed reads P in retro.
func retroAccepted(run pendingRun) string {
	return fmt.Sprintf("Dispatch shows %s retro, so %s is accepted.", run.parent.Key, run.parent.Key)
}

// seedRanAfterStatusWrite releases R's held pending-basis seeds once P's status rows are done, and
// judges the wait once the seeds have run.
func (r *personHandoverRig) seedRanAfterStatusWrite(held *outboxHold, run pendingRun) {
	t := r.t
	t.Helper()
	testwait.Eventually(t, "the outbox to finish the status rows of "+run.parent.Key, func() bool {
		return r.outboxCount(run.parent.Key, record.OutboxKindDispatchStatus) == 0
	})
	shown := r.issue(run.parent.Key).Status
	t.Logf("with the pending-basis seed of %s held, the status rows of %s are done; Dispatch shows %s %s", run.root.Key, run.parent.Key, run.parent.Key, shown)
	held.release(run.root.Key)
	testwait.Eventually(t, "the held pending-basis seed of "+run.root.Key+" to run", func() bool {
		return r.outboxCount(run.root.Key, record.OutboxKindPendingBasisSeed) == 0
	})
	r.waitEndedAfterTheSeed(run, shown)
	r.pendingEnded(run.basisRun, run.ask, retroAccepted(run), true, phase.Admitted)
}

// waitEndedAfterTheSeed reads the state once, after R's seeds have run, and fails naming the order
// if R still waits: the seed's fact and the end of the wait are one transaction, so nothing later
// would end it.
func (r *personHandoverRig) waitEndedAfterTheSeed(run pendingRun, shown string) {
	t := r.t
	t.Helper()
	state, body := settled(t, r.d, "answer its state", func(_ api.State) bool { return true })
	if step := stepOf(state, run.root.Key); step.Status == api.GatedStepPendingOnBasis || step.Status == api.GatedStepUnregistered {
		t.Fatalf("the pending-basis seed of %s ran after the status rows of %s, which left Dispatch showing %s %s, and %s still waits on %s (step %s): "+
			"the person moved %s to retro, and the daemon's own status write put it back before the seed read it, so the seed read what the registration's seed "+
			"had read, its fact was a duplicate, and nothing will cue %s again (dispatch://INFRA-3904)\nstate %s",
			run.root.Key, run.parent.Key, run.parent.Key, shown, run.root.Key, run.ask, step.Status, run.parent.Key, run.root.Key, body)
	}
}

// A person's retro ends the wait when R's pending-basis seed runs after P's status write: the
// outbox holds the seed until P's status rows are done, as the outbox's lanes ran them in CI job
// 140311446907 at f1db1daf.
func TestAPersonsRetroEndsTheWaitWhenTheSeedRunsAfterPsStatusWrite(t *testing.T) {
	// Not parallel: the scratch-Dispatch rig holds its boot (1 min), Dispatch's readiness (2 min) and each reply (10 s) to fixed spans, which rigs beside it can push it past; its five callers already parallel carry that risk until the rig waits on the events instead (#5735).
	held := &outboxHold{}
	r, run := retroWorld(t, held, []string{"Compost bay thermometer", "Allotment soil stewardship"})
	held.hold(run.root.Key, record.OutboxKindPendingBasisSeed)
	r.setStatus(run.parent.Key, "retro")
	// The settle of the person's move queues the seed and P's status write in one transaction, so
	// the seed held in the outbox says the status write, if any, is queued too.
	testwait.Eventually(t, "the person's move of "+run.parent.Key+" to queue the pending-basis seed of "+run.root.Key, func() bool {
		return r.outboxCount(run.root.Key, record.OutboxKindPendingBasisSeed) > 0
	})
	r.seedRanAfterStatusWrite(held, run)
	t.Logf("the person's retro of %s ended the wait of %s with the seed run after %s's status rows", run.parent.Key, run.root.Key, run.parent.Key)
}

// A person's retro ends the wait when R's pending-basis seed runs before P's status write: the
// outbox holds P's status write until the wait ended, as the outbox's lanes ran them in every run
// that passed.
func TestAPersonsRetroEndsTheWaitWhenTheSeedRunsBeforePsStatusWrite(t *testing.T) {
	// Not parallel: the scratch-Dispatch rig holds its boot (1 min), Dispatch's readiness (2 min) and each reply (10 s) to fixed spans, which rigs beside it can push it past; its five callers already parallel carry that risk until the rig waits on the events instead (#5735).
	held := &outboxHold{}
	r, run := retroWorld(t, held, []string{"Beehive scale logger", "Rooftop pollinator gardens"})
	held.hold(run.parent.Key, record.OutboxKindDispatchStatus)
	r.setStatus(run.parent.Key, "retro")
	r.pendingEnded(run.basisRun, run.ask, retroAccepted(run), true, phase.Admitted)
	held.release(run.parent.Key)
	r.dispatchRowsDone(run.parent.Key)
	t.Logf("the person's retro of %s ended the wait of %s with %s's status write held; Dispatch shows %s %s once it ran",
		run.parent.Key, run.root.Key, run.parent.Key, run.parent.Key, r.issue(run.parent.Key).Status)
}

// bootFacts counts the boot facts the daemon has applied (workflowRuntime.booted), so a proof can
// tell the boot's status repair has run.
func (r *personHandoverRig) bootFacts() int {
	r.t.Helper()
	var count int
	if err := r.pool.QueryRow(r.t.Context(), `select count(*) from processed_events where source = 'daemon' and event_id like 'boot:%'`).Scan(&count); err != nil {
		r.t.Fatalf("count the boot facts: %v", err)
	}
	return count
}

// A person's retro given while no daemon runs ends the wait after the boot, with R's pending-basis
// seed held through the boot until P's status rows are done: at 1341b5f8 nothing of the boot
// writes P's status over the person's retro.
func TestAPersonsRetroWhileTheDaemonIsDownEndsTheWaitAfterTheBoot(t *testing.T) {
	// Not parallel: the scratch-Dispatch rig holds its boot (1 min), Dispatch's readiness (2 min) and each reply (10 s) to fixed spans, which rigs beside it can push it past; its five callers already parallel carry that risk until the rig waits on the events instead (#5735).
	held := &outboxHold{}
	r, run := retroWorld(t, held, []string{"Lighthouse lamp rotator", "Coastal navigation heritage"})
	booted := r.bootFacts()
	r.d.stop()
	r.setStatus(run.parent.Key, "retro")
	held.hold(run.root.Key, record.OutboxKindPendingBasisSeed)
	rebindHeldPorts(t, &r.cfg)
	r.boot()
	// The daemon serves only once its listing's admission pass committed, and its boot fact, which
	// runs the status repair, runs beside the API: both have queued what they queue once the boot
	// fact is recorded.
	testwait.Eventually(t, "the daemon to apply its boot fact", func() bool { return r.bootFacts() == booted+1 })
	testwait.Eventually(t, "the boot to queue the pending-basis seed of "+run.root.Key, func() bool {
		return r.outboxCount(run.root.Key, record.OutboxKindPendingBasisSeed) > 0
	})
	r.seedRanAfterStatusWrite(held, run)
	t.Logf("the person's retro of %s, given while no daemon ran, ended the wait of %s after the boot", run.parent.Key, run.root.Key)
}
