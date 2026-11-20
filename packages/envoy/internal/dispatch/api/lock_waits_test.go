package api

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The tables two kinds of this package's tests hold locked, each until a call under test waits
// on it: search_term_stats, which the decision-block and related-item checks read
// (TestDecisionBlockCheckDoesNotHoldTheConditionalEditGate,
// TestRelatedCheckBoundsARequestThatEndsFirstIsRecordedCancelled), and going_requests, which a
// going stream's state read reads (TestGoingStreamCarriesAnEndingStoredBetweenItsTwoReads).
var (
	searchTermStatsLock = storetest.RelationLock("search_term_stats")
	goingRequestsLock   = storetest.RelationLock("going_requests")
)

// searchTermStatsWaiters counts the backends of this test's database that wait on
// search_term_stats, which tells a test its check has reached the table it holds locked. Its own
// database alone (storetest.Waiting): every test's database is a clone of one template, so the
// table has the same oid in each, and a check waiting in another test's database would count.
func searchTermStatsWaiters(t *testing.T, database *store.Store) int {
	t.Helper()
	return storetest.WaitersOn(t, database, searchTermStatsLock)
}

// goingRequestsWaiters counts the backends of this test's database that wait on going_requests,
// as searchTermStatsWaiters counts search_term_stats's.
func goingRequestsWaiters(t *testing.T, database *store.Store) int {
	t.Helper()
	return storetest.WaitersOn(t, database, goingRequestsLock)
}

// Each table lock-wait probe of this package counts its own database's waiters alone: the three
// probes that count the backends of their test's database waiting on one table's lock, those on
// search_term_stats (searchTermStatsWaiters), those on going_requests (goingRequestsWaiters), and
// a create's transaction that waits on near_copy_checks while it holds the lock its issue insert
// took on issues (createWaitsForNearCopyChecks). A waiter on the same table in another database
// of the server, as another test's check would be, counts for nothing, and one in its own database
// counts once: the second is the control that shows the probe sees a waiter at all, so that the
// first's zero is not a probe that sees nothing.
//
// The package's other lock-wait reads count every backend of their database that waits on any
// lock (pg_stat_activity's datname and wait_event_type), or read one backend by its process id,
// and name no table; internal/textcheck's lock read check refuses one that names no database or
// backend of its own.
func TestEveryTableLockWaitProbeCountsItsOwnDatabasesWaitersAlone(t *testing.T) {
	nearCopyChecksWaiters := func(t *testing.T, database *store.Store) int {
		t.Helper()
		if createWaitsForNearCopyChecks(t, database) {
			return 1
		}
		return 0
	}
	for _, probe := range []struct {
		name string
		lock storetest.Lock
		// holding is what a waiter the probe counts holds while it waits.
		holding []storetest.Lock
		count   func(*testing.T, *store.Store) int
	}{
		{"search_term_stats", searchTermStatsLock, nil, searchTermStatsWaiters},
		{"going_requests", goingRequestsLock, nil, goingRequestsWaiters},
		{"near_copy_checks", storetest.RelationLock("near_copy_checks"), []storetest.Lock{storetest.RelationLock("issues")}, nearCopyChecksWaiters},
	} {
		t.Run(probe.name, func(t *testing.T) {
			database := storetest.Open(t)
			elsewhere := storetest.Queue(t, storetest.Open(t), probe.lock, probe.holding...)
			if waiting := probe.count(t, database); waiting != 0 {
				t.Fatalf("the probe counted %d waiters on %s with none in its own database and backend %d waiting in another, want 0", waiting, probe.lock, elsewhere)
			}
			here := storetest.Queue(t, database, probe.lock, probe.holding...)
			if waiting := probe.count(t, database); waiting != 1 {
				t.Fatalf("the probe counted %d waiters on %s with backend %d waiting in its own database, want 1", waiting, probe.lock, here)
			}
		})
	}
}
