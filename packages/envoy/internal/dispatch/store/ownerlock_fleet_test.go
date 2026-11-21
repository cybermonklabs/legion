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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The owner lock beside an earlier build (OwnerLockProtocol). A build before this one serialises
// an owner's writers on the owner's row, which it takes first and holds for its whole transaction,
// and never takes the advisory lock; so while one is in the fleet, every read-phase owner lock of
// this build takes the row too, right after the advisory lock and before the read it guards
// (lockOwnerRows), and while none is, it tries the owner lock gate shared before its first owner
// lock (OwnerLockGateNamespace), which an earlier build's arrival takes exclusively, and takes the
// row as beside an earlier build when the try fails. These tests write that earlier build's
// process as the row it leaves in server_processes, owner_lock_protocol at its default 0, its
// boot's dashboard publish as the dashboard_builds row it leaves, the same, and the earlier build's
// writer as the statements it runs: the owner's row `for no key update` first, then the read, then
// the write.

// fleetOwners is the owners these tests lock, one of each kind and a second issue, as rows of a
// database of their own.
type fleetOwners struct {
	project, issueA, issueB, document Owner
}

// all is every owner of f.
func (f fleetOwners) all() []Owner { return []Owner{f.project, f.issueA, f.issueB, f.document} }

// openFleetStore opens a database of its own, migrated, and seeds a project, two issues and a
// project document: the owners of fleetOwners.
func openFleetStore(t *testing.T) (*Store, fleetOwners) {
	t.Helper()
	s := openEmptyTestStore(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := s.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('FLT', 'Fleet', 'alice@example.com');
		insert into issues (key, number, project_key, rank, title, created_by) values
			('FLT-1', 1, 'FLT', 'a', 'One', '{}'), ('FLT-2', 2, 'FLT', 'b', 'Two', '{}')`); err != nil {
		t.Fatalf("seed the owners: %v", err)
	}
	var document string
	if err := s.Pool.QueryRow(ctx, `
		insert into artifacts (created_by, kind, name, slug, project_key)
		values ('{}', 'doc', 'Notes', 'notes', 'FLT') returning id::text`).Scan(&document); err != nil {
		t.Fatalf("seed the project document: %v", err)
	}
	return s, fleetOwners{
		project:  ProjectOwner("FLT"),
		issueA:   IssueOwner("FLT-1"),
		issueB:   IssueOwner("FLT-2"),
		document: ArtifactOwner(document),
	}
}

// registerFleetProcess writes a server_processes row as a process of the given owner lock
// protocol leaves it, with a heartbeat age old, and returns its instance. A protocol of 0 is
// written by leaving the column out, as a build before the column writes its row, so its insert
// takes the owner lock gate and stamps the earlier build's mark (owner_lock_legacy_registration).
func registerFleetProcess(t *testing.T, s *Store, protocol int, age time.Duration) string {
	t.Helper()
	var instance string
	var err error
	if protocol == 0 {
		err = s.Pool.QueryRow(context.Background(), `
			insert into server_processes (instance, signing_keys, heartbeat_at)
			values (gen_random_uuid(), '{fingerprint}', now() - make_interval(secs => $1))
			returning instance::text`, age.Seconds()).Scan(&instance)
	} else {
		err = s.Pool.QueryRow(context.Background(), `
			insert into server_processes (instance, signing_keys, heartbeat_at, owner_lock_protocol)
			values (gen_random_uuid(), '{fingerprint}', now() - make_interval(secs => $1), $2)
			returning instance::text`, age.Seconds(), protocol).Scan(&instance)
	}
	if err != nil {
		t.Fatalf("register a process of owner lock protocol %d: %v", protocol, err)
	}
	t.Cleanup(func() {
		_, _ = s.Pool.Exec(context.Background(), `delete from server_processes where instance = $1::uuid`, instance)
	})
	return instance
}

// ownerRowTable is the table and key column of owner's row.
func ownerRowTable(owner Owner) (table, column string) {
	switch owner.Kind {
	case OwnerArtifact:
		return "artifacts", "id"
	case OwnerIssue:
		return "issues", "key"
	}
	return "projects", "key"
}

// ownerRowHeld reports whether another transaction holds owner's row at a level `for no key
// update` conflicts with (`for share` or stronger), by trying it with nowait from the pool.
func ownerRowHeld(t *testing.T, s *Store, owner Owner) bool {
	t.Helper()
	table, column := ownerRowTable(owner)
	_, err := s.Pool.Exec(context.Background(),
		`select 1 from `+table+` where `+column+`::text = $1 for no key update nowait`, owner.Key)
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return false
	case errors.As(err, &pgErr) && pgErr.Code == "55P03":
		return true
	}
	t.Fatalf("probe the row of %s: %v", owner.LockKey(), err)
	return false
}

// publishAsAnEarlierBuild writes a dashboard_builds row as an earlier build's boot publishes it,
// with no owner_lock_protocol, so its insert takes the owner lock gate and stamps the earlier
// build's mark (owner_lock_legacy_arrival).
func publishAsAnEarlierBuild(t *testing.T, s *Store) {
	t.Helper()
	build := fmt.Sprintf("earlier-%d", time.Now().UnixNano())
	if _, err := s.Pool.Exec(context.Background(), `insert into dashboard_builds (build, entry) values ($1, '')`, build); err != nil {
		t.Fatalf("publish as an earlier build: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.Pool.Exec(context.Background(), `delete from dashboard_builds where build = $1`, build)
	})
}

// ageLegacyMark moves the earlier build's mark age into the past, as that much time passing after
// the arrival that stamped it would.
func ageLegacyMark(t *testing.T, s *Store, age time.Duration) {
	t.Helper()
	if _, err := s.Pool.Exec(context.Background(),
		`update owner_lock_legacy_mark set seen_at = now() - make_interval(secs => $1)`, age.Seconds()); err != nil {
		t.Fatalf("age the earlier build's mark: %v", err)
	}
}

// forgetLegacyMark deletes the earlier build's mark when the test ends, so the next one starts
// from a fleet no earlier build has arrived in.
func forgetLegacyMark(t *testing.T, s *Store) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = s.Pool.Exec(context.Background(), `delete from owner_lock_legacy_mark`)
	})
}

// hasLegacyMark reports whether owner_lock_legacy_mark holds its row.
func hasLegacyMark(t *testing.T, s *Store) bool {
	t.Helper()
	var marked bool
	if err := s.Pool.QueryRow(context.Background(), `select exists (select from owner_lock_legacy_mark)`).Scan(&marked); err != nil {
		t.Fatalf("read the earlier build's mark: %v", err)
	}
	return marked
}

// gateLock is the owner lock gate as backend pid holds or waits for it, by pg_locks: its mode
// ("ShareLock", "ExclusiveLock"; "" when pid neither holds nor waits for it) and whether it is
// granted.
func gateLock(t *testing.T, s *Store, pid uint32) (mode string, granted bool) {
	t.Helper()
	err := s.Pool.QueryRow(context.Background(), `
		select mode, granted from pg_locks
		where locktype = 'advisory' and database = (select oid from pg_database where datname = current_database())
		  and classid::bigint = $1 and objid::bigint = $2 and objsubid = 2 and pid = $3`,
		OwnerLockGateNamespace, OwnerLockGateKey, pid).Scan(&mode, &granted)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read the owner lock gate of backend %d: %v", pid, err)
	}
	return mode, granted
}

// fleetState is one state of the fleet a lock is taken beside.
type fleetState struct {
	name   string
	seed   func(t *testing.T, s *Store)
	legacy bool
}

// fleetStates is every fleet state the owner lock tells apart: an earlier build is in the fleet
// while a process of it has heartbeated within OwnerLockLegacyWindow, or its arrival - its
// dashboard publish or its registration - stamped the mark within OwnerLockMarkWindow. A process
// of it whose heartbeat stopped within its window still counts, where the registry's own live
// window has long dropped it: a row counts until it is deregistered or pruned. A mark older than a
// row's window and younger than its own still counts.
var fleetStates = []fleetState{
	{name: "no process registered", seed: func(*testing.T, *Store) {}},
	{name: "a live process of this build", seed: func(t *testing.T, s *Store) { registerFleetProcess(t, s, OwnerLockProtocol, 0) }},
	{name: "a process of an earlier build whose heartbeat stopped within the legacy window", legacy: true, seed: func(t *testing.T, s *Store) {
		registerFleetProcess(t, s, 0, registryLiveWindow+time.Minute)
		ageLegacyMark(t, s, OwnerLockMarkWindow+time.Minute)
	}},
	{name: "a process of an earlier build whose heartbeat stopped before the legacy window", seed: func(t *testing.T, s *Store) {
		registerFleetProcess(t, s, 0, OwnerLockLegacyWindow+time.Minute)
		ageLegacyMark(t, s, OwnerLockMarkWindow+time.Minute)
	}},
	{name: "a live process of an earlier build", legacy: true, seed: func(t *testing.T, s *Store) { registerFleetProcess(t, s, 0, 0) }},
	{name: "an earlier build's dashboard publish just now", legacy: true, seed: publishAsAnEarlierBuild},
	{name: "an earlier build's dashboard publish older than the legacy window, within the mark's", legacy: true, seed: func(t *testing.T, s *Store) {
		publishAsAnEarlierBuild(t, s)
		ageLegacyMark(t, s, OwnerLockLegacyWindow+time.Minute)
	}},
	{name: "an earlier build's dashboard publish before the mark's window", seed: func(t *testing.T, s *Store) {
		publishAsAnEarlierBuild(t, s)
		ageLegacyMark(t, s, OwnerLockMarkWindow+time.Minute)
	}},
}

// fleetStateNamed is the state of fleetStates named name.
func fleetStateNamed(t *testing.T, name string) fleetState {
	t.Helper()
	for _, state := range fleetStates {
		if state.name == name {
			return state
		}
	}
	t.Fatalf("no fleet state is named %q", name)
	return fleetState{}
}

// registryLockKey is the registry's lock (processes.registryLock), which a registration takes
// before it reads the live rows and writes its own, and every heartbeat takes too;
// registryLiveWindow the registry's live window (processes.LiveWindow), within which a heartbeat
// counts its process as live; and dashboardAssetsLock the advisory lock every dashboard publish
// takes shared first (dashboard.lockName). Each spelled here because this package's tests cannot
// import those.
const (
	registryLockKey     int64 = 0x4450524F43524547
	registryLiveWindow        = 15 * time.Second
	dashboardAssetsLock       = "dispatch-dashboard-assets"
)

// earlierBuildArrival is one way an earlier build arrives in the fleet, its statements as its boot
// sends them (legion-envoy-v30.24.0's dashboard.Publish and processes.admit).
type earlierBuildArrival struct {
	name string
	// arrive runs the arrival in tx, which the caller commits, for key: the build's id, or the
	// process's instance.
	arrive func(ctx context.Context, tx pgx.Tx, key string) error
}

var earlierBuildArrivals = []earlierBuildArrival{
	{"its dashboard publish", func(ctx context.Context, tx pgx.Tx, build string) error {
		if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock_shared(hashtext($1))`, dashboardAssetsLock); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `select path from dashboard_build_assets where build = $1`, build); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into dashboard_builds (build, entry, commit) values ($1, $2, $3)
			on conflict (build) do update set last_started_at = now()`, build, "/assets/index-earlier.js", "")
		return err
	}},
	{"its registration", func(ctx context.Context, tx pgx.Tx, instance string) error {
		if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, registryLockKey); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `select instance from server_processes where heartbeat_at > now() - make_interval(secs => $1)`,
			registryLiveWindow.Seconds()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into server_processes (instance, commit, dashboard_build, signing_keys,
				schema_version, schema_creatable, embedder_identity, file_store)
			values ($1, '', '', '{fingerprint}', 1, 1, '', false)
			on conflict (instance) do update set heartbeat_at = now()`, instance)
		return err
	}},
}

// arriveOn runs arrival for key in a transaction of its own on conn, bounded by a lock timeout so
// a wait that never ends fails rather than hangs, and commits it.
func arriveOn(ctx context.Context, conn *pgxpool.Conn, arrival earlierBuildArrival, key string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `set local lock_timeout = '20s'`); err != nil {
		return err
	}
	if err := arrival.arrive(ctx, tx, key); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// arrive runs arrival for key on a connection of the pool's (arriveOn).
func arrive(ctx context.Context, s *Store, arrival earlierBuildArrival, key string) error {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	return arriveOn(ctx, conn, arrival, key)
}

// awaitGateWait waits for backend pid to wait for the owner lock gate exclusively, as an earlier
// build's arrival waits for it, and fails the test when what it runs, named what, ends first
// (ended). Its one bound is the test binary's deadline less testwait's margin (testwait.Until),
// whose failure prints the gate as pid last held or waited for it.
func awaitGateWait(t *testing.T, s *Store, pid uint32, ended <-chan error, what string) {
	t.Helper()
	testwait.Until(t, what+" to wait for the owner lock gate exclusively", func() (string, bool) {
		gate, granted := gateLock(t, s, pid)
		if gate == "ExclusiveLock" && !granted {
			return "", true
		}
		select {
		case err := <-ended:
			t.Fatalf("%s ended (%v) while a transaction held the gate shared; want it to wait for the gate", what, err)
		default:
		}
		return fmt.Sprintf("backend %d holds or waits for the gate as %q (granted %t)", pid, gate, granted), false
	})
}

// writerShape is how one writer of the owner lock's races (ownerlock_fleet_race_test.go,
// ownerlock_fleet_mark_test.go) serialises: this build beside a process of an earlier build
// (advisory lock, then the rows), this build in the advisory mode (advisory lock, the rows only in
// the write phase), this build as its fleet read answers (either of those two, as LockOwners runs
// it), and the earlier build itself (the rows first, no advisory lock).
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

// gateRaceWriters is how many writers of this build, each in the mode its fleet read answers, run
// at once in the race beside an earlier build's arrivals (ownerlock_fleet_race_test.go) and in the
// one across its mark's expiry (ownerlock_fleet_mark_test.go).
const gateRaceWriters = 4

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

// settle logs each role's tally and answers the increments lost (lostIncrements), and the
// deadlocks and other failures of every role. It is called once the race's goroutines have ended.
func (l *raceLedger) settle(t *testing.T, s *Store, all []Owner, start map[Owner]int) (lost, deadlocks, failures int) {
	t.Helper()
	lost = lostIncrements(t, s, all, start, l.committed)
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

// lostIncrements is the increments a race over all lost: every committed transaction adds one to
// each owner it touched, so an owner whose last_seq, read now, grew from start by less than
// committed, its commits, lost the difference.
func lostIncrements(t *testing.T, s *Store, all []Owner, start, committed map[Owner]int) int {
	t.Helper()
	end := ownerSeqs(t, s, all)
	lost := 0
	for _, owner := range all {
		lost += start[owner] + committed[owner] - end[owner]
	}
	return lost
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

// Each read-phase owner lock takes its owners' rows, before it answers, exactly while an earlier
// build is in the fleet, and the owner lock gate, shared, exactly while none is: LockOwner,
// LockOwners over every kind, LockOwnerAndRead, LockOwnersAndRead, LockOwnersShared and
// TryLockOwners. A process of an earlier build whose heartbeat and mark are older than the window,
// or a live process of this build, makes it take no row.
func TestTheOwnerLockTakesTheOwnerRowWhileAnEarlierBuildIsInTheFleet(t *testing.T) {
	s, owners := openFleetStore(t)
	ctx := context.Background()
	locks := []struct {
		name   string
		owners []Owner
		lock   func(tx pgx.Tx) error
	}{
		{"LockOwner", []Owner{owners.issueA}, func(tx pgx.Tx) error { return LockOwner(ctx, tx, owners.issueA) }},
		{"LockOwners", owners.all(), func(tx pgx.Tx) error { return LockOwners(ctx, tx, owners.all()...) }},
		{"LockOwnerAndRead", []Owner{owners.issueA}, func(tx pgx.Tx) error {
			var title string
			return LockOwnerAndRead(ctx, tx, owners.issueA, `select title from issues where key = $1`, []any{owners.issueA.Key}, &title)
		}},
		{"LockOwnersAndRead", []Owner{owners.issueB, owners.project}, func(tx pgx.Tx) error {
			var title string
			return LockOwnersAndRead(ctx, tx, []Owner{owners.issueB, owners.project}, `select title from issues where key = $1`, []any{owners.issueB.Key}, &title)
		}},
		{"LockOwnersShared", []Owner{owners.document, owners.issueA}, func(tx pgx.Tx) error {
			return LockOwnersShared(ctx, tx, owners.document, owners.issueA)
		}},
		{"TryLockOwners", []Owner{owners.issueB, owners.document}, func(tx pgx.Tx) error {
			taken, err := TryLockOwners(ctx, tx, owners.issueB, owners.document)
			if err == nil && len(taken) != 2 {
				err = fmt.Errorf("took %v, want both owners", lockKeys(taken))
			}
			return err
		}},
	}
	for _, state := range fleetStates {
		t.Run(state.name, func(t *testing.T) {
			forgetLegacyMark(t, s)
			state.seed(t, s)
			want := ownerLockAdvisory
			if state.legacy {
				want = ownerLockAdvisoryAndRow
			}
			for _, lock := range locks {
				tx, err := s.Pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if err := lock.lock(tx); err != nil {
					_ = tx.Rollback(ctx)
					t.Fatalf("%s: %v", lock.name, err)
				}
				if mode := lastOwnerLockMode(); mode != want {
					t.Errorf("%s beside %s took the %s mode, want %s", lock.name, state.name, mode, want)
				}
				for _, owner := range owners.all() {
					held := ownerRowHeld(t, s, owner)
					named := slices.Contains(lock.owners, owner)
					if want := state.legacy && named; held != want {
						t.Errorf("%s of %v beside %s: the row of %s is held: %t, want %t",
							lock.name, lockKeys(lock.owners), state.name, owner.LockKey(), held, want)
					}
				}
				gate, granted := gateLock(t, s, tx.Conn().PgConn().PID())
				if wantGate := !state.legacy; (gate == "ShareLock" && granted) != wantGate || (!wantGate && gate != "") {
					t.Errorf("%s beside %s holds the owner lock gate as %q (granted %t), want it held shared: %t",
						lock.name, state.name, gate, granted, wantGate)
				}
				if err := tx.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// holdRowAsAnEarlierBuild holds owner's row `for no key update` in a transaction of its own, as
// a writer of an earlier build holds it from its first statement, and answers the transaction.
func holdRowAsAnEarlierBuild(t *testing.T, s *Store, owner Owner) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	holder, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	table, column := ownerRowTable(owner)
	if _, err := holder.Exec(ctx, `select 1 from `+table+` where `+column+`::text = $1 for no key update`, owner.Key); err != nil {
		t.Fatalf("hold the row of %s: %v", owner.LockKey(), err)
	}
	return holder
}

// awaitRowLockWaiter waits up to ten seconds for a backend to wait on a row lock in a statement
// of lockOwnerRows (an owner row's lock taken beside the advisory lock), or for answered.
func awaitRowLockWaiter(t *testing.T, s *Store, answered <-chan error) (error, bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-answered:
			return err, true
		default:
		}
		var waiting int
		if err := s.Pool.QueryRow(context.Background(), `
			select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock'
			  and query like '%= any($1::%order by%for %'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return nil, false
		}
		if time.Now().After(deadline) {
			t.Fatal("no owner lock waited on the owner's row within 10s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The read a lock guards sees what a writer of an earlier build committed while it held the row:
// LockOwnerAndRead, sent with its read in one batch, waits on the row that writer holds and reads
// again behind it, so the read answers the title the writer wrote rather than the one before it.
// With no process of that build registered it does not wait, and reads the title as it stood.
func TestTheReadTheOwnerLockGuardsSeesTheCommitOfAWriterOfAnEarlierBuild(t *testing.T) {
	for _, state := range []fleetState{fleetStateNamed(t, "no process registered"), fleetStateNamed(t, "a live process of an earlier build")} {
		t.Run(state.name, func(t *testing.T) {
			s, owners := openFleetStore(t)
			state.seed(t, s)
			ctx := context.Background()
			holder := holdRowAsAnEarlierBuild(t, s, owners.issueA)
			if _, err := holder.Exec(ctx, `update issues set title = 'Closed by the earlier build' where key = $1`, owners.issueA.Key); err != nil {
				t.Fatal(err)
			}
			read := make(chan string, 1)
			answered := make(chan error, 1)
			go func() {
				tx, err := s.Pool.Begin(ctx)
				if err != nil {
					answered <- err
					return
				}
				defer tx.Rollback(ctx)
				var title string
				err = LockOwnerAndRead(ctx, tx, owners.issueA, `select title from issues where key = $1`, []any{owners.issueA.Key}, &title)
				read <- title
				answered <- err
			}()
			if state.legacy {
				if err, early := awaitRowLockWaiter(t, s, answered); early {
					t.Fatalf("LockOwnerAndRead answered (%v, %q) while a writer of an earlier build held the row; want it to wait for that row", err, <-read)
				}
			} else if err := <-answered; err != nil {
				t.Fatal(err)
			}
			if err := holder.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if state.legacy {
				if err := <-answered; err != nil {
					t.Fatal(err)
				}
			}
			want := "One"
			if state.legacy {
				want = "Closed by the earlier build"
			}
			if got := <-read; got != want {
				t.Errorf("the guarded read beside %s answered %q, want %q", state.name, got, want)
			}
		})
	}
}

// TryLockOwners beside a live process of an earlier build leaves out an owner whose row a writer of
// that build holds, as `for no key update skip locked` left it out, and takes the rows of the
// others; beside none, the row is not looked at and the owner is taken.
func TestTryLockOwnersSkipsAnOwnerWhoseRowAWriterOfAnEarlierBuildHolds(t *testing.T) {
	for _, state := range []fleetState{fleetStateNamed(t, "no process registered"), fleetStateNamed(t, "a live process of an earlier build")} {
		t.Run(state.name, func(t *testing.T) {
			s, owners := openFleetStore(t)
			state.seed(t, s)
			ctx := context.Background()
			holdRowAsAnEarlierBuild(t, s, owners.issueA)
			tx, err := s.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			taken, err := TryLockOwners(ctx, tx, owners.issueA, owners.issueB, owners.document)
			if err != nil {
				t.Fatal(err)
			}
			want := []Owner{owners.issueA, owners.issueB, owners.document}
			if state.legacy {
				want = []Owner{owners.issueB, owners.document}
			}
			if !slices.Equal(taken, want) {
				t.Errorf("TryLockOwners beside %s took %v, want %v", state.name, lockKeys(taken), lockKeys(want))
			}
			for _, owner := range []Owner{owners.issueB, owners.document} {
				if held := ownerRowHeld(t, s, owner); held != state.legacy {
					t.Errorf("beside %s the row of %s is held: %t, want %t", state.name, owner.LockKey(), held, state.legacy)
				}
			}
		})
	}
}
