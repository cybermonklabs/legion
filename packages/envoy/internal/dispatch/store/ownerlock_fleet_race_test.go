package store

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
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

// writerShape is how one writer of the races below serialises: this build beside a process of an
// earlier build (advisory lock, then the rows), this build in the advisory mode (advisory lock,
// the rows only in the write phase), this build as its fleet read answers (either of those two,
// as LockOwners runs it), and the earlier build itself (the rows first, no advisory lock).
type writerShape string

const (
	legacyModeWriter writerShape = "this build, advisory and row"
	advisoryWriter   writerShape = "this build, advisory alone"
	fleetModeWriter  writerShape = "this build, as its fleet read answers"
	rowFirstWriter   writerShape = "earlier build, row first"
)

// raceWriter runs one transaction of shape over owners: the read phase reads each owner's
// last_seq, the write phase writes each one back plus one, in touch's order, as a read-modify-write
// whose base the read phase decided. It answers whether the transaction's fleet read found an
// earlier build in the fleet (fleetModeWriter alone; false for the others), and the transaction's
// error. This build's writers take their write phase's row locks in touch's order, an order of the
// handler's appends, which no rule fixes since the advisory locks they hold already exclude each
// other; the earlier build's take theirs sorted by `<kind>:<key>` up front.
func raceWriter(ctx context.Context, s *Store, shape writerShape, owners []Owner, touch []Owner) (legacy bool, err error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `set local lock_timeout = '20s'`); err != nil {
		return false, err
	}
	switch shape {
	case legacyModeWriter:
		if err := LockOwners(ctx, tx, owners...); err != nil {
			return false, err
		}
	case advisoryWriter:
		// The advisory statement alone, whatever its fleet read answers: the writer whose fleet
		// read found no earlier build.
		if _, err := tx.Exec(ctx, ownerLockExclusive.statement, ownerLockArgs(lockKeys(owners))...); err != nil {
			return false, err
		}
	case fleetModeWriter:
		if _, legacy, err = lockOwnersAt(ctx, tx, ownerLockExclusive, owners, nil); err != nil {
			return legacy, err
		}
	case rowFirstWriter:
		sorted := slices.Clone(owners)
		slices.SortFunc(sorted, func(a, b Owner) int { return strings.Compare(a.LockKey(), b.LockKey()) })
		for _, owner := range sorted {
			table, column := ownerRowTable(owner)
			if _, err := tx.Exec(ctx, `select 1 from `+table+` where `+column+`::text = $1 for no key update`, owner.Key); err != nil {
				return legacy, err
			}
		}
	}
	seen := map[Owner]int{}
	for _, owner := range owners {
		table, column := ownerRowTable(owner)
		var seq int
		if err := tx.QueryRow(ctx, `select last_seq from `+table+` where `+column+`::text = $1`, owner.Key).Scan(&seq); err != nil {
			return legacy, err
		}
		seen[owner] = seq
	}
	time.Sleep(time.Duration(rand.IntN(3)) * time.Millisecond)
	for _, owner := range touch {
		table, column := ownerRowTable(owner)
		if shape != rowFirstWriter {
			if _, err := tx.Exec(ctx, ownerLock, OwnerLockNamespace, owner.LockKey()); err != nil {
				return legacy, err
			}
			if _, err := tx.Exec(ctx, `select 1 from `+table+` where `+column+`::text = $1 for no key update`, owner.Key); err != nil {
				return legacy, err
			}
		}
		if _, err := tx.Exec(ctx, `update `+table+` set last_seq = $2 where `+column+`::text = $1`, owner.Key, seen[owner]+1); err != nil {
			return legacy, err
		}
	}
	return legacy, tx.Commit(ctx)
}

// raceTally is what one pair's rounds came to.
type raceTally struct {
	rounds, deadlocks, otherFailures, lost int
}

// raceShapes runs rounds of shape a and shape b at once over random subsets of owners, and counts
// the rounds Postgres broke with `deadlock detected` (40P01), the transactions that failed any
// other way, and the increments lost: every committed transaction adds one to each owner it
// touched, so an owner whose last_seq grew by less than its commits lost the difference.
func raceShapes(t *testing.T, s *Store, owners fleetOwners, a, b writerShape, rounds int) raceTally {
	t.Helper()
	ctx := context.Background()
	all := owners.all()
	start := map[Owner]int{}
	for _, owner := range all {
		table, column := ownerRowTable(owner)
		var seq int
		if err := s.Pool.QueryRow(ctx, `select last_seq from `+table+` where `+column+`::text = $1`, owner.Key).Scan(&seq); err != nil {
			t.Fatal(err)
		}
		start[owner] = seq
	}
	committed := map[Owner]int{}
	tally := raceTally{rounds: rounds}
	var mu sync.Mutex
	for range rounds {
		var wg sync.WaitGroup
		for _, shape := range []writerShape{a, b} {
			subset := slices.Clone(all)
			rand.Shuffle(len(subset), func(i, j int) { subset[i], subset[j] = subset[j], subset[i] })
			subset = subset[:2+rand.IntN(len(subset)-1)]
			touch := slices.Clone(subset)
			rand.Shuffle(len(touch), func(i, j int) { touch[i], touch[j] = touch[j], touch[i] })
			wg.Go(func() {
				_, err := raceWriter(ctx, s, shape, subset, touch)
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
	for _, owner := range all {
		table, column := ownerRowTable(owner)
		var seq int
		if err := s.Pool.QueryRow(ctx, `select last_seq from `+table+` where `+column+`::text = $1`, owner.Key).Scan(&seq); err != nil {
			t.Fatal(err)
		}
		tally.lost += start[owner] + committed[owner] - seq
	}
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
// leaves, how many row-first writes it makes each time on each of its two writers, and how many
// writers of this build run all the while.
const (
	gateRaceCycles        = 100
	gateRaceEarlierWrites = 6
	gateRaceWriters       = 4
)

// gateRaceTally is what one role of the races came to.
type gateRaceTally struct {
	commits, deadlocks, failures int
}

// raceLedger is what the writers of a race committed: each role's tally, every increment they
// committed by owner, and how many of this build's writers committed in each mode (true: beside an
// earlier build). The race's goroutines share it.
type raceLedger struct {
	mu        sync.Mutex
	committed map[Owner]int
	tallies   map[string]*gateRaceTally
	modes     map[bool]int
}

func newRaceLedger() *raceLedger {
	return &raceLedger{committed: map[Owner]int{}, tallies: map[string]*gateRaceTally{}, modes: map[bool]int{}}
}

// record counts one transaction of role that touched touched: its commit, or its failure, a
// deadlock (40P01) or any other, logged.
func (l *raceLedger) record(t *testing.T, role string, err error, touched []Owner) {
	l.mu.Lock()
	defer l.mu.Unlock()
	tally := l.tallies[role]
	if tally == nil {
		tally = &gateRaceTally{}
		l.tallies[role] = tally
	}
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		tally.commits++
		for _, owner := range touched {
			l.committed[owner]++
		}
	case errors.As(err, &pgErr) && pgErr.Code == "40P01":
		tally.deadlocks++
		t.Logf("%s: deadlock: %s", role, pgErr.Detail)
	default:
		tally.failures++
		t.Logf("%s: %v", role, err)
	}
}

// write runs one writer of shape over a random subset of all and records it, and for this
// build's writer as its fleet read answers, the mode it committed in.
func (l *raceLedger) write(ctx context.Context, t *testing.T, s *Store, shape writerShape, all []Owner) {
	subset, touch := randomOwners(all)
	legacy, err := raceWriter(ctx, s, shape, subset, touch)
	if err == nil && shape == fleetModeWriter {
		l.mu.Lock()
		l.modes[legacy]++
		l.mu.Unlock()
	}
	l.record(t, string(shape), err, touch)
}

// settle logs each role's tally and answers the increments lost, every owner's last_seq read
// against start and the increments committed, and the deadlocks and other failures of every role.
// It is called once the race's goroutines have ended.
func (l *raceLedger) settle(t *testing.T, s *Store, all []Owner, start map[Owner]int) (lost, deadlocks, failures int) {
	t.Helper()
	end := ownerSeqs(t, s, all)
	for _, owner := range all {
		lost += start[owner] + l.committed[owner] - end[owner]
	}
	for role, tally := range l.tallies {
		t.Logf("%s: %d committed, %d deadlocks (40P01), %d other failures",
			role, tally.commits, tally.deadlocks, tally.failures)
		deadlocks += tally.deadlocks
		failures += tally.failures
	}
	return lost, deadlocks, failures
}

// ownerSeqs is every owner's last_seq.
func ownerSeqs(t *testing.T, s *Store, owners []Owner) map[Owner]int {
	t.Helper()
	seq := map[Owner]int{}
	for _, owner := range owners {
		table, column := ownerRowTable(owner)
		var value int
		if err := s.Pool.QueryRow(context.Background(), `select last_seq from `+table+` where `+column+`::text = $1`, owner.Key).Scan(&value); err != nil {
			t.Fatal(err)
		}
		seq[owner] = value
	}
	return seq
}

// randomOwners is a random subset of all, at least two, and the same owners in another random
// order: what a writer of the races reads, and the order it writes them in.
func randomOwners(all []Owner) (subset, touch []Owner) {
	subset = slices.Clone(all)
	rand.Shuffle(len(subset), func(i, j int) { subset[i], subset[j] = subset[j], subset[i] })
	subset = subset[:2+rand.IntN(len(subset)-1)]
	touch = slices.Clone(subset)
	rand.Shuffle(len(touch), func(i, j int) { touch[i], touch[j] = touch[j], touch[i] })
	return subset, touch
}

// heartbeatAsThisBuild stamps instance's heartbeat as processes' heartbeat does: under the
// registry's lock, in a transaction of its own.
func heartbeatAsThisBuild(ctx context.Context, s *Store, instance string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `set local lock_timeout = '20s'`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, registryLockKey); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `update server_processes set heartbeat_at = now() where instance = $1::uuid`, instance); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

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
// writers, and updates are lost.
func TestTheOwnerLockGateRacesAnEarlierBuildArrivingWritingAndLeaving(t *testing.T) {
	s, owners := openFleetStore(t)
	ctx := context.Background()
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
			ledger.record(t, "this build's heartbeat", heartbeatAsThisBuild(ctx, s, mine), nil)
		}
	})

	var waits []time.Duration
	timedArrival := func(arrival earlierBuildArrival, key string) {
		began := time.Now()
		err := arrive(ctx, s, arrival, key)
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
