package specstore

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// specLockStatement is the statement Lock takes the lock of an approved spec with, lower-cased as
// lockstep.RequireStoppedAt reads the statement a writer waits at.
const specLockStatement = "select pg_advisory_xact_lock(hashtext('dispatch-spec-source'), hashtext($1))"

// nodeWriteStatement is the head of graphstore's node write (queueNodes), which inserts every node
// the store does not hold and locks every node it does, in ascending (kind, id).
const nodeWriteStatement = "insert into graph_nodes ("

// address is the node id of the approved spec of acme/app in CORE, the repository the tests write.
func address(id string) string { return Address("CORE", "acme/app", id) }

// holdTheSpecLock holds the lock of the approved spec of acme/app in CORE as every writer of that
// spec takes it, first (Lock). A writer waiting behind it has read nothing of the store yet, so its
// transaction holds no lock on graph_nodes or graph_tuples.
func holdTheSpecLock(t *testing.T, database *store.Store) *lockstep.Hold {
	t.Helper()
	hold := &lockstep.Hold{
		Name: "the lock of the approved spec of acme/app", Step: lockstep.StepOwner, Taken: true,
		Kind: "spec_source", ID: address(""), Statement: specLockStatement,
		Unlocked: []string{"graph_nodes", "graph_tuples"},
	}
	lockstep.Begin(t, database, hold)
	if err := Lock(context.Background(), hold.Tx, "CORE", "acme/app"); err != nil {
		t.Fatalf("hold %s: %v", hold.Name, err)
	}
	return hold
}

// holdNodeRow holds the row of a node the store holds `for update`, which the node write's insert
// waits on, since it locks every row already there at the level an update takes.
func holdNodeRow(t *testing.T, database *store.Store, kind, id string) *lockstep.Hold {
	t.Helper()
	return lockstep.HoldRow(t, database, &lockstep.Hold{
		Name: kind + " " + id + "'s row", Step: lockstep.StepNode, Taken: true, Kind: kind, ID: id,
		Statement: nodeWriteStatement,
	}, `select 1 from graph_nodes where kind = $1 and id = $2 for update`, kind, id)
}

// holdNewNodeRow holds the row of a node the store does not hold yet: its insert through the
// store's own writer, left uncommitted, so the node write's insert of the same key waits to learn
// whether it commits, and on the rollback that lets it go inserts the row itself.
func holdNewNodeRow(t *testing.T, database *store.Store, kind, id string) *lockstep.Hold {
	t.Helper()
	hold := &lockstep.Hold{
		Name: kind + " " + id + "'s new row", Step: lockstep.StepNode, Taken: true, Kind: kind, ID: id,
		Statement: nodeWriteStatement,
	}
	lockstep.Begin(t, database, hold)
	if err := graphstore.Write(context.Background(), hold.Tx, []graphstore.Node{specNode(kind, id, "CORE", person, time.Now().UTC())}, nil); err != nil {
		t.Fatalf("hold %s: %v", hold.Name, err)
	}
	return hold
}

// importLevel runs Import of level for issue in a transaction of its own over database and commits
// it. It answers every failure, so a goroutine of a test may call it.
func importLevel(database *store.Store, issue string, level Level) (Imported, error) {
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return Imported{}, err
	}
	defer tx.Rollback(ctx)
	imported, err := Import(ctx, tx, ImportInput{Project: "CORE", Repo: "acme/app", Issue: issue, Level: level,
		Provenance: Provenance{Act: ActImport, Issue: issue, Person: person}, Who: person})
	if err != nil {
		return Imported{}, err
	}
	return imported, tx.Commit(ctx)
}

// heldIDs is the id of each open hold of acme/app, sorted.
func heldIDs(t *testing.T, database *store.Store) []string {
	t.Helper()
	holds, err := Holds(context.Background(), database.Pool, "CORE", "acme/app")
	if err != nil {
		t.Fatalf("read the holds: %v", err)
	}
	ids := []string{}
	for _, hold := range holds {
		ids = append(ids, hold.ID)
	}
	return ids
}

// backend is the one backend of single, the store a writer under test runs on.
func backend(t *testing.T, single *store.Store) uint32 {
	t.Helper()
	var pid uint32
	if err := single.Pool.QueryRow(context.Background(), `select pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read the writer's backend: %v", err)
	}
	return pid
}

// TestAnApprovalOfAChangeRequestTakesTheStoreLockOrder holds the write of an approval of a change
// request into the approved spec (Apply) to the store's lock order, written on api/inbox_read.go's
// markInboxReadThreads. Its owner step is the lock of the repository's approved spec (Lock), which
// every writer of that spec takes before it reads what it writes against; then its one node write
// takes the rows in ascending (kind, id): the hold the approval clears, of kind spec_import_hold,
// before the spec_node rows it writes, a version of a node already there, the first version of a
// node the store does not hold yet and a removal, in id order. Its facts are values at the
// sentinel, which no repeat key holds, so no hold stands on them, and it appends no event. The
// approval runs on one connection of its own, whose backend the test watches, in a transaction of
// its own that runs its write at once (RunNow), as an approval's write phase runs what it queued.
// After it the approved spec holds the request's texts, and the hold is cleared.
func TestAnApprovalOfAChangeRequestTakesTheStoreLockOrder(t *testing.T) {
	database := storetest.Open(t)
	mustApply(t, database, issue(1), statement("a-bill-is-kept", "A bill is kept.", ""), statement("a-bill-is-sent", "A bill is sent.", ""))
	if _, err := importLevel(database, issue(2), Level{Statements: []LevelStatement{{ID: "a-bill-is-owned", Text: "A bill is owned."}}}); err != nil {
		t.Fatalf("import the level that holds a-bill-is-owned: %v", err)
	}
	if held := heldIDs(t, database); !reflect.DeepEqual(held, []string{"a-bill-is-owned"}) {
		t.Fatalf("the import holds %v, want a-bill-is-owned, the hold the approval clears", held)
	}
	single := storetest.SingleConnection(t, database)
	pid := backend(t, single)

	holds := []*lockstep.Hold{
		holdTheSpecLock(t, database),
		holdNodeRow(t, database, KindHold, address("a-bill-is-owned")),
		holdNodeRow(t, database, KindNode, address("a-bill-is-kept")),
		holdNewNodeRow(t, database, KindNode, address("a-bill-is-owned")),
		holdNodeRow(t, database, KindNode, address("a-bill-is-sent")),
	}
	done := make(chan error, 1)
	go func() {
		_, err := apply(single, issue(3),
			statement("a-bill-is-kept", "A bill is kept a year.", ` base="1"`),
			statement("a-bill-is-owned", "A bill is owned.", ""),
			statement("a-bill-is-sent", "A bill is sent.", ` base="1" removes="true"`))
		done <- err
	}()
	lockstep.Follow(t, database, "Apply", pid, done, holds)

	texts := current(t, database)
	if !strings.Contains(texts["a-bill-is-kept"], "A bill is kept a year.") || !strings.Contains(texts["a-bill-is-owned"], "A bill is owned.") {
		t.Errorf("after the approval the approved spec reads %q, want a-bill-is-kept's new text and a-bill-is-owned", texts)
	}
	if text, held := texts["a-bill-is-sent"]; held {
		t.Errorf("after the approval a-bill-is-sent reads %q, want it removed", text)
	}
	if held := heldIDs(t, database); len(held) != 0 {
		t.Errorf("after the approval the holds %v are open, want the approval to clear a-bill-is-owned's", held)
	}
}

// TestTheLevelImportTakesTheStoreLockOrder holds the import of an approved level (Import) to the
// store's lock order, written on api/inbox_read.go's markInboxReadThreads: the lock of the
// repository's approved spec first (Lock), then its one node write's rows in ascending (kind, id).
// The import runs again over a level an import before it read: the statement it held then has its
// kind now, so this import writes its node, which the store does not hold yet, and clears its hold,
// whose row is there; another statement has no kind, so this import holds it, a hold row the store
// does not hold yet; and the component the import before wrote it leaves as it stands. Its facts are
// values at the sentinel, so no hold stands on them, and it appends no event. The import runs on
// one connection of its own, whose backend the test watches. After it the approved spec holds the
// statement, and the one open hold is the new one.
func TestTheLevelImportTakesTheStoreLockOrder(t *testing.T) {
	database := storetest.Open(t)
	billing := specgraph.Component{ID: "billing", Name: "Billing", Purpose: "It bills each order."}
	if _, err := importLevel(database, issue(1), Level{
		Statements: []LevelStatement{{ID: "a-bill-is-owned", Text: "A bill is owned."}},
		Components: []specgraph.Component{billing},
	}); err != nil {
		t.Fatalf("import the level that holds a-bill-is-owned: %v", err)
	}
	single := storetest.SingleConnection(t, database)
	pid := backend(t, single)

	holds := []*lockstep.Hold{
		holdTheSpecLock(t, database),
		holdNewNodeRow(t, database, KindHold, address("a-bill-is-flagged")),
		holdNodeRow(t, database, KindHold, address("a-bill-is-owned")),
		holdNewNodeRow(t, database, KindNode, address("a-bill-is-owned")),
	}
	done := make(chan error, 1)
	go func() {
		_, err := importLevel(single, issue(1), Level{
			Statements: []LevelStatement{
				{ID: "a-bill-is-owned", Text: "A bill is owned.", Kind: "behavior", About: []string{"billing"}, Noted: true},
				{ID: "a-bill-is-flagged", Text: "A bill is flagged."},
			},
			Components: []specgraph.Component{billing},
		})
		done <- err
	}()
	lockstep.Follow(t, database, "Import", pid, done, holds)

	texts := current(t, database)
	if !strings.Contains(texts["a-bill-is-owned"], "A bill is owned.") || texts["billing"] == "" {
		t.Errorf("after the import the approved spec reads %q, want a-bill-is-owned beside billing", texts)
	}
	if held := heldIDs(t, database); !reflect.DeepEqual(held, []string{"a-bill-is-flagged"}) {
		t.Errorf("after the import the open holds are %v, want a-bill-is-flagged alone", held)
	}
}
