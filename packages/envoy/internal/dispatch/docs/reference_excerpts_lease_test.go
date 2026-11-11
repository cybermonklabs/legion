package docs

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// excerptLeases is the pool a reconciliation pass takes its lease on, the one cmd/dispatch gives
// the loop.
func excerptLeases(t *testing.T, database *store.Store) *pgxpool.Pool {
	t.Helper()
	leases, err := database.Pool.Leases()
	if err != nil {
		t.Fatalf("open the leases pool: %v", err)
	}
	return leases
}

// countExcerptLeaseLines counts the lease lines carrying message for lease.
func countExcerptLeaseLines(logs *storetest.LockedLog, message, lease string) int {
	count := 0
	for line := range strings.Lines(logs.String()) {
		if strings.Contains(line, `msg="`+message+`"`) && strings.Contains(line, "lease="+lease) {
			count++
		}
	}
	return count
}

// Two envoy-dispatch processes reconcile one database's reference excerpts, each through
// RunReferenceExcerptReconciliation itself and each on a pool of its own (storetest.SecondProcess,
// as a second process has): one takes the reference-excerpts lease and runs its pass, and the
// other finds it held and does not run the same pass beside it. The first pass is held inside its
// own first statement by a lock this test takes on refs, which is the moment the lease is about -
// both processes' loops woken at once. The second keeps trying, and once the first pass is done
// it runs a pass of its own: the first may have read before whatever woke the second, so a woken
// loop never gives its pass up to another process's. Each wait is the package's hang guard: how
// long two processes on a loaded runner take to reach a lock or to log a pass is the machine's
// to say, and what the row reads is which lines were logged, not when.
//
// Serial: it swaps process-wide state, the default slog logger, which is how it counts the two
// loops' own lease lines back (captureLogs).
func TestTwoExcerptReconciliationRunnersNeverPassAtOnceAndBothPass(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	firstLeases, secondLeases := excerptLeases(t, database), excerptLeases(t, second)
	document := createProjectDocument(t, database, "# Document\n\nSee dispatch://CORE-1 here.\n")
	if _, err := database.Pool.Exec(ctx,
		`insert into refs (from_kind, from_id, to_kind, to_id) values ('artifact', $1, 'issue', 'CORE-1')`, document); err != nil {
		t.Fatalf("index the document's mention: %v", err)
	}
	logs := captureLogs(t)
	first, other := newExcerptService(t, database, nil), newExcerptService(t, second, nil)

	gate, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("open the transaction holding refs: %v", err)
	}
	defer func() { _ = gate.Rollback(context.Background()) }()
	if _, err := gate.Exec(ctx, `lock table refs in exclusive mode`); err != nil {
		t.Fatalf("hold refs: %v", err)
	}

	runExcerpts(t, func(ctx context.Context) { first.RunReferenceExcerptReconciliation(ctx, firstLeases, nil) })
	if waiting := storetest.WaitForLockWaiters(t, gate, 1, roomWait(t)); waiting < 1 {
		t.Fatal("the first runner's pass never reached the refs lock: it never took the lease")
	}
	runExcerpts(t, func(ctx context.Context) { other.RunReferenceExcerptReconciliation(ctx, secondLeases, nil) })
	// The second runner's boot attempt and at least one retry find the lease held.
	waitFor(t, roomWait(t), "the second runner to find the lease held twice", func() bool {
		return countExcerptLeaseLines(logs, "dispatch duty: leased pass skipped", referenceExcerptLease) >= 2
	})
	if ran := countExcerptLeaseLines(logs, "dispatch duty: leased pass ran", referenceExcerptLease); ran != 0 {
		t.Fatalf("%d passes ran while the first one was held inside its pass; want none:\n%s", ran, logs.Lines("lease="))
	}

	if err := gate.Rollback(context.Background()); err != nil {
		t.Fatalf("release refs: %v", err)
	}
	waitFor(t, roomWait(t), "both runners to run a pass of their own", func() bool {
		return countExcerptLeaseLines(logs, "dispatch duty: leased pass ran", referenceExcerptLease) == 2
	})
	if isOwed(t, database, document) {
		t.Fatal("the document's mention is still owed after both passes")
	}
}
