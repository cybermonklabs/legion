// The block-index pass's lease and the reference forms drain's, which a test takes to hold both
// off: the memory harness (memory_harness_test.go, under the `memory` tag) for each window it
// measures, and the cost measurements and the stale-lens roll (graph_cost_test.go and
// stale_lens_test.go, under the `basebuild` tag) for theirs. A tag adds its own files to the
// package, so the one helper both take is in this file, which carries no tag: every build of the
// package's tests compiles it, the untagged one with no caller. A constraint naming both tags would
// keep it out of that build, but .github/scripts/go-tagged-tests.sh, which lists the tests each tag
// adds, takes a tag only in its bare form and refuses a file whose constraint names it any other
// way.

package main

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
)

// blockIndexLeaseNamespace is the first key every loop lease is taken under - duty's
// leaseNamespace, which stays unexported - the four bytes of "LOOP". The second key is the
// lease's own name, which Postgres hashes with hashtext.
const blockIndexLeaseNamespace int32 = 0x4C4F4F50

// heldBlockIndexLeases are the leases holdBlockIndexLease takes, in the order it takes them: the
// block-index pass's own (docs.BlockIndexLease) and the reference forms drain's
// (docs.RefsFormsLease). The drain indexes a document through the block index's own index
// (docs.Service.drainFormsDocument), so a hold of the pass alone would see a document the test
// left owed indexed by the drain all the same.
var heldBlockIndexLeases = []string{docs.BlockIndexLease, docs.RefsFormsLease}

// blockIndexHold is the hold holdBlockIndexLease answers: the one connection that holds the
// leases, and the leases it still holds. giveBack gives back every lease it still holds, with the
// connection; giveBackIndex gives back the block-index pass's alone and keeps the forms drain's,
// and holdIndexAgain takes the block-index pass's again on the same connection. A call gives back
// only what the hold still holds, so giveBack after giveBack, or after a failure gave the
// connection up, gives back nothing.
type blockIndexHold struct {
	t      *testing.T
	name   string
	wait   time.Duration
	output func() string
	conn   *pgxpool.Conn
	held   []string
}

// holdBlockIndexLease takes the block-index pass's own lease and the reference forms drain's
// (heldBlockIndexLeases) on a connection of leases, one of the test's own pools of connections to
// the database the server serves, and answers the hold that gives them back. duty.Exclusive takes
// each lease before every pass of its loop and skips the pass where another session holds it, so
// neither a block-index pass nor a drain pass runs while the test holds them: what the test writes
// meanwhile is indexed by a pass after the leases are given back.
//
// The locks belong to the session that took them, so one connection takes both, holds them until
// they are given back and gives the last of them back with itself (duty.FreeSessionLock, which
// destroys rather than returns a connection whose unlock failed, since what such a session holds
// is unknown). A hold that fails after it took a lease gives back what it took the same way.
//
// name is what every failure is about. wait bounds the wait for each take of the leases, a hang
// guard on a wait nothing healthy reaches; past it the test fails with the server's output
// (output).
func holdBlockIndexLease(t *testing.T, leases duty.Leases, name string, wait time.Duration, output func() string) *blockIndexHold {
	t.Helper()
	conn, err := leases.Acquire(context.Background())
	if err != nil {
		t.Fatalf("%s: take a connection for the block-index leases: %v", name, err)
	}
	hold := &blockIndexHold{t: t, name: name, wait: wait, output: output, conn: conn}
	deadline := time.Now().Add(wait)
	for _, lease := range heldBlockIndexLeases {
		hold.take(lease, deadline)
	}
	return hold
}

// take takes lease on the hold's connection, waiting out a pass that holds it until deadline.
// pg_try_advisory_lock answers false rather than queueing behind the holder, so a pass running at
// this moment is waited out here: it gives the lease back when it ends.
func (h *blockIndexHold) take(lease string, deadline time.Time) {
	h.t.Helper()
	for {
		var held bool
		if err := h.conn.QueryRow(context.Background(), `select pg_try_advisory_lock($1, hashtext($2))`,
			blockIndexLeaseNamespace, lease).Scan(&held); err != nil {
			h.giveBack()
			h.t.Fatalf("%s: take the %q lease: %v", h.name, lease, err)
		}
		if held {
			h.held = append(h.held, lease)
			return
		}
		if time.Now().After(deadline) {
			h.giveBack()
			h.t.Fatalf("a pass held the %q lease for more than %s, so %s could not go on with the block index and the forms drain held off:\n%s",
				lease, h.wait, h.name, h.output())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// giveBackIndex gives back the block-index pass's lease alone and keeps the reference forms
// drain's, for a test that then measures or awaits that pass's own work - the owed count it
// settles, its cursor, the rows it writes: with the drain's lease given back too, the drain could
// index the same document first, through the block index's own index, and the test would read a
// pass with no work. The drain's lease goes back with giveBack, once that reading ends. An unlock
// that fails leaves what the session holds unknown, so the hold then gives its connection up and
// fails the test.
func (h *blockIndexHold) giveBackIndex() {
	h.t.Helper()
	if !slices.Contains(h.held, docs.BlockIndexLease) {
		return
	}
	var freed bool
	if err := h.conn.QueryRow(context.Background(), `select pg_advisory_unlock($1, hashtext($2))`,
		blockIndexLeaseNamespace, docs.BlockIndexLease).Scan(&freed); err != nil || !freed {
		h.giveBack()
		h.t.Fatalf("%s: give the %q lease back alone: freed %t, %v", h.name, docs.BlockIndexLease, freed, err)
	}
	h.held = slices.DeleteFunc(h.held, func(lease string) bool { return lease == docs.BlockIndexLease })
}

// holdIndexAgain takes the block-index pass's lease again on the hold's connection, after
// giveBackIndex gave it back, while the hold keeps the forms drain's, for a test that writes what
// the block index is to owe again within one reading of the pass's work.
func (h *blockIndexHold) holdIndexAgain() {
	h.t.Helper()
	if h.conn == nil {
		h.t.Fatalf("%s: take the %q lease again on a hold that gave its connection up", h.name, docs.BlockIndexLease)
	}
	if !slices.Contains(h.held, docs.BlockIndexLease) {
		h.take(docs.BlockIndexLease, time.Now().Add(h.wait))
	}
}

// giveBack gives back every lease the hold still holds, in one statement, and its connection: an
// aggregate over every lease, so each is unlocked whatever the one before it answered.
func (h *blockIndexHold) giveBack() {
	h.t.Helper()
	if h.conn == nil {
		return
	}
	conn, held := h.conn, h.held
	h.conn, h.held = nil, nil
	if len(held) == 0 {
		conn.Release()
		return
	}
	if err := duty.FreeSessionLock(conn,
		`select bool_and(pg_advisory_unlock($1, hashtext(held.lease))) from unnest($2::text[]) as held(lease)`,
		blockIndexLeaseNamespace, held); err != nil {
		h.t.Errorf("%s: give the %v leases back: %v", h.name, held, err)
	}
}
