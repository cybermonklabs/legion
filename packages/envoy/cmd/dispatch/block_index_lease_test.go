// The block-index pass's lease, which a test takes to hold the pass off: the memory harness
// (memory_harness_test.go, under the `memory` tag) for each window it measures, and the cost
// measurements and the stale-lens roll (graph_cost_test.go and stale_lens_test.go, under the
// `basebuild` tag) for theirs. A tag adds its own files to the package, so the one helper both
// take is in this file, which carries no tag: every build of the package's tests compiles it, the
// untagged one with no caller. A constraint naming both tags would keep it out of that build, but
// .github/scripts/go-tagged-tests.sh, which lists the tests each tag adds, takes a tag only in its
// bare form and refuses a file whose constraint names it any other way.

package main

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
)

// blockIndexLeaseNamespace is the first key every loop lease is taken under - duty's
// leaseNamespace, which stays unexported - the four bytes of "LOOP". The second key is the
// lease's own name, which Postgres hashes with hashtext.
const blockIndexLeaseNamespace int32 = 0x4C4F4F50

// holdBlockIndexLease takes the block-index pass's own lease (docs.BlockIndexLease) on a
// connection of leases, one of the test's own pools of connections to the database the server
// serves, and answers the call that gives it back. duty.Exclusive takes that lease before every
// pass and skips the pass where another session holds it, so no block-index pass runs while the
// test holds it: what the test writes meanwhile is indexed by a tick after the lease is given
// back.
//
// The lock belongs to the session that took it, so one connection takes it, holds it until the
// call and gives it back (duty.FreeSessionLock, which destroys rather than returns a connection
// whose unlock failed, since what such a session holds is unknown).
//
// name is what every failure is about. wait bounds the wait for the lease, a hang guard on a wait
// nothing healthy reaches; past it the test fails with the server's output (output).
func holdBlockIndexLease(t *testing.T, leases duty.Leases, name string, wait time.Duration, output func() string) func() {
	t.Helper()
	ctx := context.Background()
	conn, err := leases.Acquire(ctx)
	if err != nil {
		t.Fatalf("%s: take a connection for the block-index lease: %v", name, err)
	}
	deadline := time.Now().Add(wait)
	for {
		var held bool
		if err := conn.QueryRow(ctx, `select pg_try_advisory_lock($1, hashtext($2))`,
			blockIndexLeaseNamespace, docs.BlockIndexLease).Scan(&held); err != nil {
			conn.Release()
			t.Fatalf("%s: take the block-index lease: %v", name, err)
		}
		if held {
			break
		}
		if time.Now().After(deadline) {
			conn.Release()
			t.Fatalf("the block-index pass held the %q lease for more than %s, so %s could not go on with the pass held off:\n%s",
				docs.BlockIndexLease, wait, name, output())
		}
		// pg_try_advisory_lock answers false rather than queueing behind the holder, so a pass
		// running at this moment is waited out here: it gives the lease back when it ends.
		time.Sleep(50 * time.Millisecond)
	}
	return func() {
		if err := duty.FreeSessionLock(conn, `select pg_advisory_unlock($1, hashtext($2))`,
			blockIndexLeaseNamespace, docs.BlockIndexLease); err != nil {
			t.Errorf("%s: give the block-index lease back: %v", name, err)
		}
	}
}
