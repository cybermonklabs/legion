package store

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// The owner lock beside an earlier build, raced (ownerlock_fleet_test.go): the writers of either
// build against each other, and the earlier build arriving, writing and leaving while writers of
// this build run.

// ───── deadlocks and lost updates across the writers ───────────────────────

// raceTally is what one pair's rounds came to.
type raceTally struct {
	rounds, deadlocks, otherFailures, lost int
}

// raceShapes runs rounds of shape a and shape b at once over random subsets of owners
// (randomOwners), and counts the rounds Postgres broke with `deadlock detected` (40P01), the
// transactions that failed any other way, and the increments lost (lostIncrements).
func raceShapes(t *testing.T, s *Store, owners fleetOwners, a, b writerShape, rounds int) raceTally {
	t.Helper()
	ctx := context.Background()
	all := owners.all()
	start := ownerSeqs(t, s, all)
	committed := map[Owner]int{}
	tally := raceTally{rounds: rounds}
	var mu sync.Mutex
	for range rounds {
		var wg sync.WaitGroup
		for _, shape := range []writerShape{a, b} {
			subset, touch := randomOwners(all)
			wg.Go(func() {
				_, err := raceWriter(ctx, t, s, shape, subset, touch)
				mu.Lock()
				defer mu.Unlock()
				var pgErr *pgconn.PgError
				switch {
				case err == nil:
					for _, owner := range touch {
						committed[owner]++
					}
				case errors.As(err, &pgErr) && pgErr.Code == "40P01":
					tally.deadlocks++
				default:
					tally.otherFailures++
					t.Logf("%s: %v", shape, err)
				}
			})
		}
		wg.Wait()
	}
	tally.lost = lostIncrements(t, s, all, start, committed)
	return tally
}

// raceRounds is how many rounds each pair runs: each round is one transaction of each shape at
// once over random subsets of the four owners.
const raceRounds = 300

// This build's writer in the mode beside an earlier build deadlocks with neither this build's
// advisory writer nor the earlier build's row-first writer, and loses no update to either: it
// takes the advisory locks sorted by key, then the rows in the earlier build's own order, before
// it reads. The advisory writer beside the row-first writer is the pairing the mode exists to
// prevent - its read phase is not excluded by a writer that holds the row - and it is the
// control: it loses updates, and it deadlocks where its write phase takes two rows in an order the
// row-first writer does not, so its rounds must lose an update or deadlock at least once. A race
// that broke neither would hold the safe pairs to nothing.
func TestTheOwnerLockModesRaceEachOtherAndTheEarlierBuildsWriter(t *testing.T) {
	t.Parallel()
	s, owners := openFleetStore(t)
	registerFleetProcess(t, s, 0, 0)
	pairs := []struct {
		a, b writerShape
		safe bool
	}{
		{legacyModeWriter, advisoryWriter, true},
		{legacyModeWriter, rowFirstWriter, true},
		{legacyModeWriter, legacyModeWriter, true},
		{advisoryWriter, rowFirstWriter, false},
	}
	for _, pair := range pairs {
		t.Run(fmt.Sprintf("%s against %s", pair.a, pair.b), func(t *testing.T) {
			tally := raceShapes(t, s, owners, pair.a, pair.b, raceRounds)
			t.Logf("%d rounds of %s against %s: %d deadlocks (40P01), %d other failures, %d increments lost",
				tally.rounds, pair.a, pair.b, tally.deadlocks, tally.otherFailures, tally.lost)
			switch {
			case pair.safe && (tally.deadlocks > 0 || tally.otherFailures > 0 || tally.lost != 0):
				t.Errorf("%s against %s: %d deadlocks, %d other failures, %d increments lost; want none of each",
					pair.a, pair.b, tally.deadlocks, tally.otherFailures, tally.lost)
			case !pair.safe && tally.deadlocks == 0 && tally.lost == 0:
				t.Errorf("%s against %s, the control: no deadlock and no increment lost in %d rounds; want at least one of either, or the race tells the safe pairs from this one by nothing",
					pair.a, pair.b, tally.rounds)
			}
		})
	}
}

// ───── the gate raced by an earlier build arriving, writing and leaving ────

// gateRace is what the race below sizes: how many times the earlier build arrives, writes and
// leaves, and how many row-first writes it makes each time on each of its two writers. How many
// writers of this build run all the while is gateRaceWriters (ownerlock_fleet_test.go).
const (
	gateRaceCycles        = 100
	gateRaceEarlierWrites = 6
)

// An earlier build arrives, writes and leaves, over and over, while writers of this build run
// throughout, each in the mode its own fleet read answers, and this build's process heartbeats.
// Each time, the earlier build publishes its dashboard, starts two row-first writers as its boot's
// repairs and passes do, registers while they run (every other time), and leaves once they end:
// its row deregistered and its mark aged past the window, as that much time passing does. No
// update is lost, nothing deadlocks and nothing else fails: a writer of this build that read no
// earlier build holds the gate until it ends, so the arrival waits for it, and every writer that
// starts while the arrival waits, or after it, takes the owners' rows. Writers run in both modes,
// and the arrivals that waited for the gate, and how long, are logged. With the gate taken out of
// the two arrival triggers, a writer that read no earlier build writes beside that build's
// writers, and updates are lost. The registration's own 5 s bound on the gate is not this row's
// subject, so the row takes it off the function (waitRegistrationsOut): each registration waits as
// its own transaction allows, until shortly before the test binary's deadline, and one that waits
// longer behind a slow writer is no failure here.
func TestTheOwnerLockGateRacesAnEarlierBuildArrivingWritingAndLeaving(t *testing.T) {
	t.Parallel()
	s, owners := openFleetStore(t)
	ctx := context.Background()
	waitRegistrationsOut(ctx, t, s)
	mine := registerFleetProcess(t, s, OwnerLockProtocol, 0)
	all := owners.all()
	start := ownerSeqs(t, s, all)
	ledger := newRaceLedger()

	stop := make(chan struct{})
	var thisBuild sync.WaitGroup
	for range gateRaceWriters {
		thisBuild.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				ledger.write(ctx, t, s, fleetModeWriter, all)
			}
		})
	}
	thisBuild.Go(func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			ledger.record(t, "this build's heartbeat", heartbeatAsThisBuild(ctx, t, s, mine), nil)
		}
	})

	var waits []time.Duration
	timedArrival := func(arrival earlierBuildArrival, key string) {
		began := time.Now()
		err := arrive(ctx, t, s, arrival, key)
		waits = append(waits, time.Since(began))
		ledger.record(t, "earlier build, "+arrival.name, err, nil)
	}
	for cycle := range gateRaceCycles {
		timedArrival(earlierBuildArrivals[0], uuid.NewString())
		var earlier sync.WaitGroup
		for range 2 {
			earlier.Go(func() {
				for range gateRaceEarlierWrites {
					ledger.write(ctx, t, s, rowFirstWriter, all)
				}
			})
		}
		instance := uuid.NewString()
		if cycle%2 == 0 {
			timedArrival(earlierBuildArrivals[1], instance)
		}
		earlier.Wait()
		if _, err := s.Pool.Exec(ctx, `delete from server_processes where instance = $1::uuid`, instance); err != nil {
			t.Fatal(err)
		}
		ageLegacyMark(t, s, OwnerLockMarkWindow+time.Minute)
		time.Sleep(time.Duration(10+rand.IntN(40)) * time.Millisecond)
	}
	close(stop)
	thisBuild.Wait()

	lost, deadlocks, failures := ledger.settle(t, s, all, start)
	slices.Sort(waits)
	t.Logf("%d cycles: this build's writers committed %d in the mode beside an earlier build and %d in the advisory mode; %d arrivals took %s at the median, %s at the 90th percentile and %s at most; %d increments lost",
		gateRaceCycles, ledger.modes[true], ledger.modes[false], len(waits), waits[len(waits)/2], waits[len(waits)*9/10], waits[len(waits)-1], lost)
	if lost != 0 || deadlocks != 0 || failures != 0 {
		t.Errorf("%d increments lost, %d deadlocks, %d other failures; want none of each", lost, deadlocks, failures)
	}
	if ledger.modes[true] == 0 || ledger.modes[false] == 0 {
		t.Errorf("this build's writers committed %d beside an earlier build and %d in the advisory mode; want both modes raced", ledger.modes[true], ledger.modes[false])
	}
}
