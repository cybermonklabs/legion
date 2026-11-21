package architecture

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// architectureLeases is the pool an import pass takes its lease on, the one cmd/dispatch gives
// the ticker.
func architectureLeases(t *testing.T, database *store.Store) duty.Leases {
	t.Helper()
	return storetest.Leases(t, database.Pool)
}

// Every import pass re-reads each enabled source from GitHub against one installation's rate
// limit, so one process imports however many tick: with the architecture-import lease held by
// another caller - the other envoy-dispatch process, inside an import of its own - this process's
// pass reads nothing from GitHub and writes no component.
//
// Serial: the lease is its subject, and duty.Exclusive gives each statement on the lease's own
// connection duty's leaseCallTimeout (5 s, internal/dispatch/duty/exclusive.go), a constant no
// test can raise: the other caller's take must answer inside it for that caller to hold the lease
// at all, and the last pass's take and period read must answer inside it for that pass to run, so
// a parallel neighbour's load that stretches one of them past five seconds fails the row.
func TestAnImportPassWhoseLeaseIsHeldElsewhereReadsNothing(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{t: t, files: map[string]string{"core.md": "# Core\n"}}
	importer, database := newImporterFixture(t, fake)
	leases := architectureLeases(t, database)

	// 0: this caller stands for the other process's pass while it is inside it, which is all
	// this row is about, and records no pass of its own -- so what the passes below do is the
	// lease's doing alone and not the import interval's.
	taken, release := make(chan struct{}), make(chan struct{})
	held := make(chan bool, 1)
	go func() {
		held <- duty.Exclusive(ctx, leases, architectureImportLease, time.Time{}, 0, func(context.Context) {
			close(taken)
			<-release
		})
	}()
	select {
	case <-taken:
	case <-time.After(heldReadWait):
		t.Fatalf("the other caller never took the architecture-import lease within %v, the bound waitHeld gives a held read", heldReadWait)
	}

	syncAllLeased(ctx, importer, leases, time.Now())
	if trees, blobs := fake.reads(); trees != 0 || blobs != 0 {
		t.Fatalf("the pass read %d trees and %d blobs under a lease held elsewhere; want none", trees, blobs)
	}
	if rows := componentRows(t, database); len(rows) != 0 {
		t.Fatalf("the pass imported %v under a lease held elsewhere; want nothing", rows)
	}
	close(release)
	if !<-held {
		t.Fatal("the other caller never held the lease, so the pass was free to import")
	}

	// The lease is the pass's, not the process's: the next pass takes it and imports.
	syncAllLeased(ctx, importer, leases, time.Now())
	if trees, _ := fake.reads(); trees == 0 {
		t.Fatal("the pass read no tree with the lease free")
	}
	if rows := componentRows(t, database); len(rows) == 0 {
		t.Fatal("the pass imported no component with the lease free")
	}
}

// The import loop's shortest wait between two ticks is SyncInterval less a tenth of it, and a
// nanosecond more (duty.RunJittered draws each wait in (SyncInterval - SyncInterval/10,
// SyncInterval]). Two ticks of a lone process that far apart must both import: a tick refused as a
// period the process's own last pass served is an import lost, and the next comes almost two
// intervals after the last. The first tick here is due at the start of a SyncInterval-aligned
// bucket, so the second is due inside that same five-minute bucket, which a lease whose period
// were SyncInterval would find served. The gap is worked out from SyncInterval alone, not from the
// period the loop hands the lease, so this test reads the same against a build that handed it
// SyncInterval and shows the refusal there.
func TestTwoImportTicksTheShortestWaitApartBothImport(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{t: t, files: map[string]string{"core.md": "# Core\n"}}
	importer, database := newImporterFixture(t, fake)
	leases := architectureLeases(t, database)

	interval := SyncInterval.Nanoseconds()
	first := time.Unix(0, time.Now().UnixNano()/interval*interval)
	second := first.Add(SyncInterval - SyncInterval/10 + 1)

	syncAllLeased(ctx, importer, leases, first)
	if _, heads := fake.lookups(); heads != 1 {
		t.Fatalf("the first tick looked the branch head up %d times, want once: the tick imports", heads)
	}
	syncAllLeased(ctx, importer, leases, second)
	if _, heads := fake.lookups(); heads != 2 {
		t.Fatalf("two ticks %v apart looked the branch head up %d times, want twice: the second tick is due more than a lease period after the first, in a period no pass has served, and imports too", second.Sub(first), heads)
	}
}
