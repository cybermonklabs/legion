// Package storesmove moves the priority store and the workflow store into the graph store
// (dispatch://INFRA-1098's R32, the spec's Design part "How the two stores move"). A priority was a
// node of priority_nodes with its facts in priority_tuples, and a run of a step and the plan it ran
// from were nodes of workflow_nodes with their facts in workflow_tuples. This build's
// prioritystore and workflowstore write and read graph_nodes and graph_tuples alone, so the boot
// moves every row of the four tables there, after the migrations and before the process listens
// (Move, which cmd/dispatch's main runs), so /livez and /healthz answer only once it ends.
//
// The move is no migration: the pre-deploy census refuses a migration that writes rows of a table
// above pgmigrate.CensusTableLimit unless each write is bounded, and a copy of the four tables is
// not. Migration 20261126000300_stores_moved writes what the move needs that is bounded: its
// record, stores_moved_seqs, and the supertag nodes priority, step and plan.
//
// It runs in four steps, each on the move's own connection (store.Connect), which carries the
// writer mark every connection of this build carries (store.StoresMovedWriterSetting):
//
//   - One runner. It takes the session advisory lock stores-moved and holds it until the move
//     ends, so a second process that boots meanwhile waits for it, and then finds the four names
//     as views and moves nothing.
//   - The copy (copy.go). Batches of at most DefaultBatch tuples, each one transaction, in each
//     table's own seq order: each batch writes its tuples through graphstore under new seqs, the
//     node rows they name as projections alone, and the record of their old seqs.
//   - The swap (swap.go). One transaction under the old tables' locks copies what the record lacks,
//     runs the census before the drop (census.go), and, where it reads no difference, drops the
//     record and the four tables, and creates four views of the same names and columns and the two
//     triggers that complete what the release before writes through them
//     (graphstore.StoresMovedTriggers). An attempt that waits on a lock longer than half a second
//     gives up and the boot tries it again, for up to SwapWindow.
//   - The census before the drop: both directions, counted and digested, for each store apart, under
//     the swap's locks. On a difference the swap rolls back before any drop, and Move answers a
//     *DifferenceError naming the store and the first row that differs.
//
// The pre-deploy census rehearses the copy and the census before the drop in one transaction that
// it rolls back (Rehearse, rehearse.go), so a difference stops a deploy before the old pod stops.
package storesmove

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/prioritystore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/workflowstore"
	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// oldStore is one of the two stores the move takes into the graph store: the name its record
// (stores_moved_seqs.store) keeps a tuple's old seq under, its two tables, and the kinds of the
// nodes its node table holds.
type oldStore struct {
	name          string
	nodes, tuples string
	kinds         []string
}

var (
	priorityStore = oldStore{name: "priority", nodes: "priority_nodes", tuples: "priority_tuples",
		kinds: []string{prioritystore.KindPriority}}
	workflowStore = oldStore{name: "workflow", nodes: "workflow_nodes", tuples: "workflow_tuples",
		kinds: []string{workflowstore.KindStep, workflowstore.KindPlan}}
	// oldStores is both, in the order the move copies them.
	oldStores = []oldStore{priorityStore, workflowStore}
)

// lockName is the session advisory lock one runner of the move holds,
// pg_advisory_lock(hashtext(lockName)), for the whole move.
const lockName = "stores-moved"

// applicationName is how the move's connection appears in pg_stat_activity.
const applicationName = "envoy-dispatch stores move"

// DefaultBatch is how many tuples one batch of the copy moves: the batch of
// graphcensus.backfillBatch, so a batch holds its row locks about as long as one request does.
const DefaultBatch = 200

// The swap's bounds. Each attempt waits at most swapLockTimeout for each lock, below the default
// deadlock_timeout of one second, so an attempt gives up before a deadlock check fails the other
// transaction; it then pauses SwapPause, holding nothing, as the migration runner pauses between
// its attempts, and tries again, for up to SwapWindow, SwapAttempts attempts in all. Production
// runs one replica under Recreate, so a lock the swap waits on is held by a session of no build of
// Dispatch's - the nightly dump, an operator's psql - and five minutes outlasts the dump's minute.
const (
	swapLockTimeout = 500 * time.Millisecond
	SwapPause       = pgmigrate.LockTimeoutRetryPause
	SwapWindow      = 5 * time.Minute
	SwapAttempts    = int(SwapWindow / (swapLockTimeout + SwapPause))
)

// Options are the move's test settings, each zero in a deployment (cmd/dispatch's
// DISPATCH_TEST_STORES_MOVE_* settings, read only under DISPATCH_TEST_HOOKS=1).
type Options struct {
	// Batch is how many tuples one batch copies; 0 is DefaultBatch.
	Batch int
	// StopAfter ends the move with ErrStopped right after the commit of that many batches; 0 runs
	// the move to its end.
	StopAfter int
	// SwapAttempts is how many attempts the swap makes; 0 is SwapAttempts.
	SwapAttempts int
	// HoldFile, where set, is a file the move waits for once it holds stores-moved: it moves
	// nothing until the file exists, so a test can read a second process wait on the lock.
	HoldFile string
}

// ErrStopped is the move a test setting stopped after its batches (Options.StopAfter): the
// batches it copied stand, with their record, and a later boot resumes from that record.
var ErrStopped = errors.New("the stores' move stopped after the batches its test setting allows")

// Result is what one boot's move did.
type Result struct {
	// Moved is false where the four names were views already, or gone, and the boot moved
	// nothing.
	Moved bool
	// Copied and Batches count the tuples the copy moved and its batches, and SwapCopied the
	// tuples the swap moved under its locks.
	Copied, Batches, SwapCopied int
	// SwapAttempts counts the swap's attempts, the one that swapped included.
	SwapAttempts int
	// Census is the census before the drop the swap took, nil where nothing moved.
	Census *Census
}

// Move moves the priority store and the workflow store into the graph store, or nothing where the
// database's four names are views already or gone, on a connection of its own to the database
// databaseURL names. It answers ErrStopped where Options.StopAfter stopped it, and a
// *DifferenceError where the census before the drop read a difference, with the four tables and
// the record standing as they were; every error it answers ends the boot.
func Move(ctx context.Context, databaseURL string, options Options) (*Result, error) {
	conn, err := store.Connect(ctx, databaseURL, applicationName)
	if err != nil {
		return nil, fmt.Errorf("the stores' move: %w", err)
	}
	// The session's end releases stores-moved, on every path.
	defer closeBounded(ctx, conn)
	if err := takeLock(ctx, conn); err != nil {
		return nil, err
	}
	if options.HoldFile != "" {
		if err := waitForFile(ctx, options.HoldFile); err != nil {
			return nil, err
		}
	}
	relkind, err := priorityNodesKind(ctx, conn)
	if err != nil {
		return nil, err
	}
	if relkind != tableKind {
		slog.Info("dispatch: the priority store and the workflow store are in the graph store already; the boot moves nothing",
			"priority_nodes", relkindWords(relkind))
		return &Result{}, nil
	}
	mover := newMover(options)
	started := time.Now()
	slog.Info("dispatch: the move of the priority store and the workflow store into the graph store begins", "batch", mover.batch)
	result := &Result{Moved: true}
	result.Copied, result.Batches, err = mover.copy(ctx, conn)
	if err != nil {
		return nil, err
	}
	if err := mover.swapWithRetries(ctx, conn, result); err != nil {
		return nil, err
	}
	if result.Census == nil {
		// Another runner's swap stood by the time this one held the old tables, which
		// stores-moved rules out but for a runner that took no such lock.
		slog.Info("dispatch: the priority store and the workflow store were moved by another runner during this move")
		return result, nil
	}
	for _, line := range result.Census.Lines() {
		slog.Info("dispatch: the census before the drop: " + line)
	}
	slog.Info("dispatch: the move of the priority store and the workflow store into the graph store ended",
		"copied", result.Copied, "batches", result.Batches, "swap_copied", result.SwapCopied,
		"swap_attempts", result.SwapAttempts, "took", time.Since(started).String())
	return result, nil
}

// takeLock takes stores-moved on conn, session-level, waiting for it with no bound where another
// process holds it: that process is moving the stores, and once it ends this one finds them moved.
// It says so in the log before it waits, since a silent unbounded wait tells an operator nothing.
// The wait is pg_advisory_lock's own, so pg_locks shows it.
func takeLock(ctx context.Context, conn *pgx.Conn) error {
	var granted bool
	if err := conn.QueryRow(ctx, `select pg_try_advisory_lock(hashtext($1))`, lockName).Scan(&granted); err != nil {
		return fmt.Errorf("the stores' move: take the advisory lock %s: %w", lockName, err)
	}
	if granted {
		return nil
	}
	slog.Info("dispatch: another process holds the advisory lock " + lockName + "; the boot waits for it")
	started := time.Now()
	if _, err := conn.Exec(ctx, `select pg_advisory_lock(hashtext($1))`, lockName); err != nil {
		return fmt.Errorf("the stores' move: wait for the advisory lock %s: %w", lockName, err)
	}
	slog.Info("dispatch: the advisory lock "+lockName+" was granted after a wait", "waited", time.Since(started).String())
	return nil
}

// waitForFile waits until a file stands at path, for a test that holds the move (Options.HoldFile).
func waitForFile(ctx context.Context, path string) error {
	slog.Info("dispatch: the stores' move holds "+lockName+" until its hold file exists", "path", path)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// The relkinds priorityNodesKind answers: a table, which the move moves, a view, which a move
// made, and none, where no relation of the name stands, as after the drop step of the release
// after.
const (
	tableKind = "r"
	viewKind  = "v"
	noneKind  = ""
)

// priorityNodesKind reads the pg_class kind of the relation the name priority_nodes resolves to,
// or noneKind where none does. The move reads it under stores-moved before it copies, the swap again
// under its locks, and the rehearsal before it runs.
func priorityNodesKind(ctx context.Context, q queryer) (string, error) {
	var relkind string
	err := q.QueryRow(ctx, `select c.relkind::text from pg_class c where c.oid = to_regclass('priority_nodes')`).Scan(&relkind)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return noneKind, nil
	case err != nil:
		return "", fmt.Errorf("the stores' move: read what priority_nodes is: %w", err)
	}
	return relkind, nil
}

// relkindWords says what a relkind of priorityNodesKind is, for the log.
func relkindWords(relkind string) string {
	switch relkind {
	case viewKind:
		return "a view"
	case noneKind:
		return "gone"
	}
	return "relkind " + relkind
}

// closeBounded closes conn on a context its caller's cancellation does not end, bounded as the
// migration runner bounds its own closes.
func closeBounded(ctx context.Context, conn *pgx.Conn) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), pgmigrate.LockTimeout)
	defer cancel()
	_ = conn.Close(cleanup)
}
