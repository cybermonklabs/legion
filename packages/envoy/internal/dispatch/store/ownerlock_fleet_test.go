package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
