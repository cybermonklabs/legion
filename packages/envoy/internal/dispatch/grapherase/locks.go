package grapherase

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
)

// eraseOwnerAttempts is how many tries lockInStoreOrder makes at reading which owner rows the
// erase writes under and taking them and the node's row before it refuses. A try fails only when
// a write lands between its read of those rows and its locks and moves the node under another
// owner; three in a row is a stream of such writes, which the operator stops before running the
// erase again, rather than one the erase can outrun.
const eraseOwnerAttempts = 3

// lockInStoreOrder opens a confirmed erase's transaction and takes its locks in the store's lock
// order (api/inbox_read.go's markInboxReadThreads), whose first invariant is that a store write
// takes the owner's lock first. The lock of every owner the erase writes under comes first
// (ownerRows says which they are), then the node's row `for update`, then the node's own
// projection rows (lockProjectionRows), and nothing the erase does after them takes an owner lock
// it does not already hold: docs.ReadErasedLiveText takes a document's owner lock again and finds
// this transaction holding it. A node no row projects - a person, a session, a role, a page -
// stands under no owner, and the erase then starts at its node's row, which the order allows: a
// writer may leave a step out.
//
// Those owners are the node's own and its own document's, never another node's: a row that
// holds the erased bytes and derives from another node is listed and left as it is, so the run
// writes nothing under that node's owner and takes no lock on it.
//
// The owner locks are the advisory locks every writer of an issue or a document takes
// (store.LockOwners), each taken once, in the order of the owner keys (events.LockOwnerRows), as
// events.Broker.LockOwners takes the owners of several events: "artifact:<id>" for a document no
// issue owns, before "issue:<key>". That is the one order over every owner, and the backfill's
// batches take their documents' owners in it too (graphcensus's lockBlockDocuments).
//
// Which owners those are is read before they are locked, so it is read again under them with the
// rest of the listing (readDerived), and the run goes on with that second read. A write that,
// between the two reads, moved the node under an owner the run did not take could go on writing
// under that owner while the run does. The run then rolls back and starts again rather than take
// that owner after the node's row, and after eraseOwnerAttempts it refuses, naming each owner it
// found untaken, having removed nothing.
//
// Before it takes any lock, each try searches the database for the rows the run leaves
// (searchLeft). That search sends one statement per text column of every base table, each a scan
// of the whole table, and every writer of an issue or a document takes its owner's lock first and
// waits there, holding a pool connection, while another transaction holds it: under the run's
// locks, the search would hold every comment create, reply, version write and ask transition of
// the node's issue or document for as long as it scans. The run changes none of the rows the
// search lists, so listing them needs no lock. What that costs is that a row written between the
// search and the locks that holds the erased bytes is not listed; the run leaves it as it leaves
// every row the search lists. Once a try holds its locks, the run holds the writers of those
// owners for the rest of its work: the read of the rest of the listing under them, the erase and
// the commit.
func lockInStoreOrder(ctx context.Context, conn *pgx.Conn, kind, id string) (pgx.Tx, *Erasure, erasedRead, error) {
	var untaken []string
	for range eraseOwnerAttempts {
		left, err := searchLeft(ctx, conn, kind, id)
		if err != nil {
			return nil, nil, erasedRead{}, err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return nil, nil, erasedRead{}, fmt.Errorf("begin: %w", err)
		}
		erasure := &Erasure{Kind: kind, ID: id, RanAt: time.Now().UTC()}
		var read erasedRead
		read, untaken, err = erasure.lockOwnersThenNode(ctx, tx, left)
		if err == nil && len(untaken) == 0 {
			return tx, erasure, read, nil
		}
		_ = tx.Rollback(context.Background())
		if err != nil {
			return nil, nil, erasedRead{}, err
		}
	}
	return nil, nil, erasedRead{}, fmt.Errorf("the erase refuses %s:%s: on each of %d tries a write put the node under an owner row the erase had not taken before it took the node's row (%s); nothing was removed, and the erase can be run again",
		kind, id, eraseOwnerAttempts, strings.Join(untaken, "; "))
}

// searchLeft is a confirmed run's search for the rows it leaves (readLeft, with the margin records
// readKeptMargins lists), which a try of lockInStoreOrder runs before it takes any lock. It runs in
// a read-only transaction of its own, as a run without --confirm reads its whole listing, over the
// erased texts and the rows that derive from the node as that transaction reads them
// (readDerived), and that transaction ends before the try's own begins, so the try holds none of
// the table locks the search took. It answers the lines of the listing's section of rows left.
func searchLeft(ctx context.Context, conn *pgx.Conn, kind, id string) ([]string, error) {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin the search for the rows the erase leaves: %w", err)
	}
	defer tx.Rollback(context.Background())
	searched := &Erasure{Kind: kind, ID: id}
	read, err := searched.readDerived(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := searched.readLeft(ctx, tx, &read); err != nil {
		return nil, err
	}
	return read.listing.left, nil
}

// lockOwnersThenNode is one try of lockInStoreOrder: it reads which owner rows the erase writes
// under (ownerRows, which reads no node, tuple or event), takes them (events.LockOwnerRows), the
// node's row and the node's projection rows (lockProjectionRows), reads the rest of the listing
// under those locks (readDerived), lists beside it the rows left that the try's search found
// (searchLeft), and answers it with every owner it names that the try did not take.
func (e *Erasure) lockOwnersThenNode(ctx context.Context, tx pgx.Tx, left []string) (erasedRead, []string, error) {
	before, err := e.ownerRows(ctx, tx)
	if err != nil {
		return erasedRead{}, nil, err
	}
	if err := events.LockOwnerRows(ctx, tx, before); err != nil {
		return erasedRead{}, nil, err
	}
	if err := tx.QueryRow(ctx,
		`select supertag from graph_nodes where kind = $1 and id = $2 for update`,
		e.Kind, e.ID).Scan(&e.Supertag); err != nil {
		if err == pgx.ErrNoRows {
			return erasedRead{}, nil, fmt.Errorf("the store holds no node %s:%s", e.Kind, e.ID)
		}
		return erasedRead{}, nil, fmt.Errorf("read node %s:%s: %w", e.Kind, e.ID, err)
	}
	if err := lockProjectionRows(ctx, tx, e.Kind, e.ID); err != nil {
		return erasedRead{}, nil, err
	}
	locked, err := e.readDerived(ctx, tx)
	if err != nil {
		return erasedRead{}, nil, err
	}
	locked.listing.left = left
	e.list(&locked)
	var untaken []string
	for _, key := range slices.Sorted(maps.Keys(locked.owners)) {
		if _, taken := before[key]; !taken {
			untaken = append(untaken, key+", the owner of "+locked.owners[key])
		}
	}
	return locked, untaken, nil
}

// lockProjectionRows takes every projection row of the node `for update`, the level the delete of
// each in apply takes, right after the node's row and before the erase reads under its locks or
// writes anything: table by table in graphstore.ProjectedTables' order, and by key within each.
// R17 has a node's row taken before any projection row whose foreign key names it, and every
// writer of these rows that takes an owner row takes it before them, which the erase already
// holds.
//
// Taking them here, and not when apply deletes them, is what orders the erase against a writer
// that holds one of them and no owner row: the block index tick's window arm takes a comment's
// row `for no key update` and then writes that comment's windows and their window state
// (refs.WriteItemWindows). An erase that took the row only at its delete would have deleted those
// windows and that state first (forgetEmbeddings), and the arm, writing the same keys, would wait
// for the erase while the erase waited for it, until Postgres ended one of them. Here the erase
// waits at the row holding no row the arm writes, and the arm commits first.
func lockProjectionRows(ctx context.Context, tx pgx.Tx, kind, id string) error {
	for _, held := range graphstore.ProjectedTables {
		if _, err := tx.Exec(ctx, `select 1 from `+held.Table+` where node_kind = $1 and node_address = $2 order by `+
			held.Key+` for update`, kind, id); err != nil {
			return fmt.Errorf("lock the %s rows of %s:%s: %w", held.Table, kind, id, err)
		}
	}
	return nil
}
