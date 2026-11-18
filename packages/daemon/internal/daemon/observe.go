package daemon

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// observation is one thing the daemon tells a test that observes it (overrides.observe): that it
// is ready to serve, that an admission pass or a pass of a periodic loop ended, that an outbox row
// settled, that the boot's report of the claims nothing will wake finished, or that it holds a
// shim's hello for restoration. The set is closed: the six observed types below. A test waits on
// these rather than polling an endpoint, a row count or a log for the work they report.
type observation interface{ isObservation() }

// reportTo hands o to observe, the observer a test set (overrides.observe), and does nothing when
// observe is nil, as it is in production. No caller holds a lock across it: the observer is the
// test's code, and a lock held across it would let a slow test hold the daemon.
func reportTo(observe func(observation), o observation) {
	if observe != nil {
		observe(o)
	}
}

// observedReady is the daemon ready to serve: the goroutine that serves the API has begun on the
// listener the daemon took (serve), so a request a test sends from here is answered. It is raised
// before beforeServingLatched runs, so a test that holds the daemon inside that hook still hears
// it, while /readyz answers 503 until the latch is set.
type observedReady struct{}

// observedAdmissionPass is the end of an admission pass that committed: a fact applied through the
// workflow's handlers, reported once its transaction has committed and the fact lock is released
// (admissionObserver), or a reconcile of Dispatch's listing, the boot's or a recovery's
// (reconcileListing), for which fact is nil. A duplicate fact, and an observed fact the workflow
// does not apply (intake.ApplyFact's settleObserved), run no handler and raise none.
type observedAdmissionPass struct {
	fact intake.Fact
}

// observedPass is the end of one pass of a periodic loop of the daemon: the loop, and the pass's
// number within it, counted from 1.
type observedPass struct {
	loop passLoop
	pass uint64
}

// passEnded reports the end of pass of loop to observe (reportTo) and returns the next pass's
// number. A loop that waits first and then runs its pass calls it as its post statement, so every
// way a pass ends, a continue among them, is reported, and the daemon's stop, which returns from
// the wait, is not.
func passEnded(observe func(observation), loop passLoop, pass uint64) uint64 {
	reportTo(observe, observedPass{loop: loop, pass: pass})
	return pass + 1
}

// passLoop names a periodic loop of the daemon, as its observedPass reports it.
type passLoop string

const (
	// passIdle is tickIdle's pass: the release of the pods no claim owns, then the idle release.
	passIdle passLoop = "idle"
	// passWakes is tickWakes' pass: the backstops that came due, then the resumes that are stuck.
	passWakes passLoop = "wakes"
	// passOrphans is the orphan sweep's pass (supervision.reconcileOrphans).
	passOrphans passLoop = "orphans"
	// passUnfinished is a sweep of the boot's unfinished relaunches (supervision.relaunchUnfinished).
	passUnfinished passLoop = "unfinished"
	// passControllerKeeper is one look of the keeper at the daemon's controller (controllerKeeper.run).
	passControllerKeeper passLoop = "controller-keeper"
	// passControllerWatch is one look of the liveness sweep over the controller (watchController).
	passControllerWatch passLoop = "controller-watch"
	// passControllerTick is one controller wake tick (tickController).
	passControllerTick passLoop = "controller-tick"
	// passEntrySweep is one run of the phase-entry sweep (tickEntrySweep).
	passEntrySweep passLoop = "entry-sweep"
	// passMergeState is one read of a merge-state ladder (climbMergeState).
	passMergeState passLoop = "merge-state"
	// passMergeQueueRetry is the outbox's retry of the merge-queue reads that wait out the wait
	// between two reads of GitHub (mergeQueueRead, mergeStateWait): a test's pass makes them due
	// now (outbox.expediteMergeQueueReads), where production waits the half minute.
	passMergeQueueRetry passLoop = "merge-queue-retry"
	// passHoldRelease is one poll of a held admission's release (pollHoldRelease).
	passHoldRelease passLoop = "hold-release"
	// passReconnect is one poll of the NATS connection's reconnect watchdog (reconnectWatchdog).
	passReconnect passLoop = "reconnect"
)

// observedOutboxSettled is an outbox row's run settled (outbox.settle, and runRow's deferral
// behind a GitHub rate limit): the row, its issue and kind, and what its run came to.
type observedOutboxSettled struct {
	row     int64
	issue   string
	kind    record.OutboxKind
	outcome settlement
}

// settlement is what a row's run came to, as observedOutboxSettled reports it.
type settlement string

const (
	// settledFinished is the row finished (outbox.finish committed).
	settledFinished settlement = "finished"
	// settledRefused is the row finished refused with its follow-up, a failure no retry repairs
	// (outbox.finishRefused committed).
	settledRefused settlement = "refused"
	// settledRetried is the row due again after its backoff, or at the moment its wait names
	// (outbox.retry committed).
	settledRetried settlement = "retried"
	// settledDeferred is the row due again when a GitHub rate limit standing on its kind ends,
	// nothing of it run (runRow's deferral committed).
	settledDeferred settlement = "deferred"
	// settledNotRecorded is a finish, refusal, retry or deferral that did not commit: the row
	// keeps its lease and runs again once the lease lapses.
	settledNotRecorded settlement = "not-recorded"
)

// observedStranded is the boot's report of the claims nothing will wake finished, whether it
// logged them or not (reportStranded). The report runs beside the API, so a test that reads what
// it said waits for this rather than for observedReady.
type observedStranded struct{}

// observedHelloHeld is a shim's hello whose boot token the first resolve found real, as the
// resolver begins to hold it until restoration is over (helloResolver): the claim the token was
// minted for. The hold says so nowhere else, so a test that needs a hello held before its next
// step waits for this.
type observedHelloHeld struct {
	claim claim.Token
}

func (observedReady) isObservation()         {}
func (observedAdmissionPass) isObservation() {}
func (observedPass) isObservation()          {}
func (observedOutboxSettled) isObservation() {}
func (observedStranded) isObservation()      {}
func (observedHelloHeld) isObservation()     {}

// admissionObserver is the handler run adds after the deciding handlers when a test observes the
// daemon (overrides.observe), and only then: it reports each fact's admission pass once the fact's
// transaction has committed (intake.OnCommit), after the hooks the deciding handlers registered
// and with the fact lock released.
type admissionObserver func(observation)

func (observe admissionObserver) Apply(ctx context.Context, _ pgx.Tx, fact intake.Fact) (intake.Result, error) {
	intake.OnCommit(ctx, func() { observe(observedAdmissionPass{fact: fact}) })
	return intake.Result{}, nil
}
