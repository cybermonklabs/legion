package daemon

import (
	"fmt"
	"slices"
	"testing"
)

// keptLongRegressionSeeds are the seeds of the kept-decision property model whose 300-step runs
// found a fault: eleven (11 through 475) reached a hand-over at which the old root architect's
// held start task was the only carrier of a decision made on the handed-over child, which the
// child's tree must be told and the held task must stop naming (supervise's Deps.RootTask, the
// daemon's rootTask); four (363 through 479) reached one where the old architect's held task had
// already been prompted before the hand-over, and the row that carried it was long deleted by
// then, which the oracle's own reading of "a row composed before the hand-over" mistook for a
// telling to another tree; 48 adopted a queued root into a tree whose architect already ran, and
// nothing told that architect what the root had kept while it waited (workflow's Adopt now writes
// it a catch-up); 69 told one decision twice to its architect in the same sweep; 427 dropped a
// re-held copy of a catch-up while the row it copies was publishing, so the decisions the copy
// claimed were named again (DropCatchUps now keeps every row of a telling one of whose rows holds
// a claim); 262 named one decision in two catch-ups of one launch, so a composer named what another
// telling owned (UnclaimedKept now locks the rows before it reads them, and ClaimKeptDecisions
// claims everything a row names or fails), and later told one decision twice on a schedule the
// model itself made: its runTogether leased a catch-up only once another row of the step had
// started, so the lease committed inside that row's drop of the root's catch-ups, while the drop's
// DELETE was already reading the row, and the drop deleted the row it had read unleased — the
// delete's lease check is a subquery over other rows, which Postgres does not recheck against a
// concurrently committed lease — so the dropped row published, its re-held copy named the decision
// again beside the fresh catch-up, and the architect was told it twice. That late lease stood for
// a lane's claim until #2743 put the issue's lock in ClaimRow, which the drop takes too, so no lane
// makes that schedule while its claim stands (record's
// TestDroppingCatchUpsLeavesTheRowALaneIsLeasing guards the lock); the model now claims every row
// of a step through ClaimRow before any of them runs (runTogether), so it no longer makes that
// schedule either (dispatch://INFRA-2836); 701 spent a catch-up's re-holds while
// its architect held its launch, and the row's deletion left the decision with nothing to name it
// (reholdNotice now hands it to a fresh catch-up, spentKept); 41 published a catch-up (row 112)
// whose own telling write then failed, so the decision stood kept and a later catch-up (row 115)
// named it again — the second telling the architect ruled acceptable
// (https://github.com/sjawhar/legion/pull/2163#issuecomment-6542200410), which the oracle now
// accepts once and only after such a failed write (keptRun.tell, afterFailedWrite). They run at
// 300 steps because that is where each reaches it; 262 reaches its first only on some schedules of
// its rows. 41, 427 and 701 reached their faults on some schedules only, so each runs under a seek
// that steers its run to the state of its fault (keptFaultSeeds), in this test and in
// TestEachKeptFaultSeedReachesTheStateOfItsFault: 41 under seekToldAgainAfterAFailedWrite, whose
// failed write is the told mark of a catch-up telling (toldKept), the one telling write whose
// failure leaves the row untold and unclaimed for a ready's drop, so the fresh catch-up names the
// decision again; 427 under seekDropBesideADeliveringTelling, which crashes a catch-up after its
// receipt timeout and drops its tree's catch-ups while that row runs again and delivers beside its
// re-held copy; and 701 under seekSpentRehold, which races a catch-up and its copies with receipt
// timeouts, while their architect holds its launch, until the last copy's re-holds are spent, and
// then holds that state open for the rest of the run: the drive leaves out the listener's switch
// (togglePublisher) and switches a listener that refuses or denies to delivering, so quiesce does
// not relaunch the held architect after a refusal (relaunchAfterRefusal), and for the held tree it
// leaves out the death of the process in a task's turn (deathInTheTasksTurn), the root's park
// where that tree is the root, and the adoption of the queued root into it (adoptQueued); so only
// the spent branch's own hand-over (spentKept) can name what the spent copy claimed. The hold
// changes no other seed's drive. Each seek steers the catch-ups of every tree the run drives, the
// root's and those of the trees its members leave it for.
var keptLongRegressionSeeds = []int64{11, 41, 48, 69, 73, 96, 125, 262, 311, 363, 365, 366, 367, 388, 414, 427, 432, 459, 475, 479, 701}

// keptShortRegressionSeeds are the seeds whose runs at the default length found faults earlier in
// this work — a decision told twice, one told to an architect owed nothing, one left kept at
// quiesce. They run at the default bounds, which is where each was found.
var keptShortRegressionSeeds = []int64{1, 4, 7, 8, 14, 19, 21, 22, 25, 27, 29, 30, 56, 66, 69, 110, 199, 260}

// Every seed of the property model that has ever failed is run again here, each at the length that
// found it, so a regression in the kept-decision machinery is caught by a named test rather than
// by whichever sweep happens to draw that seed again. The invariant and the oracle are the
// property test's own (TestKeptDecisionsReachTheCurrentArchitectOnce): a person's decision reaches
// the architect of the tree its issue stands in exactly once, at most one telling more goes to the
// tree it left, carried by a row composed before the hand-over, and nothing is left kept.
//
// The default property test keeps the bounds it has, so it stays a minute's work; these seeds are
// the ones worth paying for every time.
func TestKeptDecisionsRegressionSeeds(t *testing.T) {
	t.Parallel()
	// Each seed's run has a database, runtime and state directory of its own (newKeptRun), so the
	// seeds run in parallel, with each other and with the other kept-decision seed tests.
	for _, seed := range keptLongRegressionSeeds {
		t.Run(fmt.Sprintf("seed-%d-at-300-steps", seed), func(t *testing.T) {
			t.Parallel()
			run := newKeptRun(t, seed, 300)
			if seek := keptFaultSeek(seed); seek != nil {
				seek(run)
			}
			run.drive()
			run.quiesce()
			run.held()
			run.report()
		})
	}
	for _, seed := range keptShortRegressionSeeds {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			t.Parallel()
			run := newKeptRun(t, seed, keptPropertySteps)
			run.drive()
			run.quiesce()
			run.held()
			run.report()
		})
	}
}

// keptFaultSeed pairs one regression seed of keptLongRegressionSeeds with the fault state it is
// kept for, the seek that steers the run to that state (kept_decision_faults_test.go), and the
// marker that counts how many times the run reached it.
type keptFaultSeed struct {
	seed   int64
	fault  string
	seek   func(*keptRun)
	marker func(*keptRun) int64
}

var keptFaultSeeds = []keptFaultSeed{
	{seed: 41, fault: keptFaultToldAgainAfterAFailedWrite,
		seek:   func(r *keptRun) { r.seekToldAgainAfterAFailedWrite = true },
		marker: func(r *keptRun) int64 { return r.faults.toldAgainAfterAFailedWrite.Load() }},
	{seed: 427, fault: keptFaultDropBesideADeliveringTelling,
		seek:   func(r *keptRun) { r.seekDropBesideADeliveringTelling = true },
		marker: func(r *keptRun) int64 { return r.faults.dropBesideADeliveringTelling.Load() }},
	{seed: 701, fault: keptFaultSpentRehold,
		seek:   func(r *keptRun) { r.seekSpentRehold = true },
		marker: func(r *keptRun) int64 { return r.faults.spentRehold.Load() }},
}

// keptFaultSeek is the seek the regression seed runs under, nil for a seed kept for no fault
// state of keptFaultSeeds: each seek is set for its own seed alone, in both tests that run it.
func keptFaultSeek(seed int64) func(*keptRun) {
	for _, fault := range keptFaultSeeds {
		if fault.seed == seed {
			return fault.seek
		}
	}
	return nil
}

// Each of seeds 41, 427 and 701 of keptLongRegressionSeeds is kept for a named fault of the
// kept-decision machinery; a seed that passes without ever reaching that state no longer guards it.
// This test runs each at 300 steps and fails any whose marker does not fire.
func TestEachKeptFaultSeedReachesTheStateOfItsFault(t *testing.T) {
	t.Parallel()
	for _, tc := range keptFaultSeeds {
		t.Run(fmt.Sprintf("seed-%d", tc.seed), func(t *testing.T) {
			t.Parallel()
			if !slices.Contains(keptLongRegressionSeeds, tc.seed) {
				t.Fatalf("seed %d is not in keptLongRegressionSeeds", tc.seed)
			}
			run := newKeptRun(t, tc.seed, 300)
			if tc.seek != nil {
				tc.seek(run)
			}
			run.drive()
			run.quiesce()
			run.held()
			run.report()
			reached := tc.marker(run)
			t.Logf("seed %d: %s reached %d times", tc.seed, tc.fault, reached)
			if reached == 0 {
				t.Fatalf("seed %d: %s reached 0 times; the seed does not reach the state of its fault", tc.seed, tc.fault)
			}
		})
	}
}
