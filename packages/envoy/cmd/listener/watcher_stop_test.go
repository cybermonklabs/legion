package main

import (
	"reflect"
	"testing"
	"time"
	"unsafe"

	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// A helper that opens a store stops the store's cache watchers when the test that opened it ends.
// A watcher left running outlives its test on a connection that test closed: it reads its bucket
// again every rereadDelay (kvwatch, 5 s), each read failing on the closed connection, and logs an
// ERROR that it was not repaired once a minute, for the rest of the binary. Before this check the
// package's tests left about 1,750 of them running, each one load that every later test of the
// binary ran beside, and an ERROR line in whichever test's output its minute fell.
//
// Each case runs its helper in a subtest of its own and reads the watchers once that subtest has
// returned, after its cleanups: every watcher of every store the helper opened must be stopped by
// then (kvwatch.Watcher.Stop). A watcher a helper swapped in is read from the store's own field, so
// the swap's replacement is the one checked (replaceWatcher).
func TestTheRigsStopTheWatchersOfTheStoresTheyOpen(t *testing.T) {
	for _, rig := range []struct {
		name string
		open func(t *testing.T) []any
	}{
		{"newListenerDeliveryHarness", func(t *testing.T) []any {
			harness := newListenerDeliveryHarness(t)
			return []any{harness.registry, harness.sessions}
		}},
		{"setupSessionsTest", func(t *testing.T) []any {
			registry, sessions := setupSessionsTest(t, map[string][]string{"ses_watched": nil}, []string{"ses_watched"})
			return []any{registry, sessions}
		}},
		{"trailingListeners", func(t *testing.T) []any {
			claimant, observer := trailingListeners(t)
			return []any{claimant.registry, claimant.sessions, observer.registry, observer.sessions}
		}},
		{"newRecipientRig and watchCopiesOn", func(t *testing.T) []any {
			rig := newRecipientRig(t)
			rig.watchCopiesOn(t, &listenerClock{now: time.Now()})
			return []any{rig.harness.registry, rig.harness.sessions, rig.known}
		}},
		{"newListRig", func(t *testing.T) []any {
			rig := newListRig(t, listRigOptions{})
			return []any{rig.registry, rig.sessions}
		}},
	} {
		var stores []any
		t.Run(rig.name, func(t *testing.T) { stores = rig.open(t) })
		if len(stores) == 0 {
			t.Fatalf("%s opened no store this check could read", rig.name)
		}
		for _, owner := range stores {
			for field, watcher := range storeWatchers(t, owner) {
				if !watcherStopped(watcher) {
					t.Errorf("%s left the watcher in the %s field of its %T running after its test ended", rig.name, field, owner)
				}
			}
		}
	}
}

// storeWatchers is each cache watcher owner keeps, by the name of the field that holds it: a
// *store.Registry's interest and role claim watchers, a *session.SessionRegistry's and a
// *session.KnownSessions's one. The fields are unexported, so they are read as replaceWatcher reads
// them.
func storeWatchers(t *testing.T, owner any) map[string]*kvwatch.Watcher {
	t.Helper()
	var fields []string
	switch owner.(type) {
	case *store.Registry:
		fields = []string{"watcher", "roleWatcher"}
	case *session.SessionRegistry, *session.KnownSessions:
		fields = []string{"watcher"}
	default:
		t.Fatalf("storeWatchers does not know the watchers of a %T", owner)
	}
	watchers := map[string]*kvwatch.Watcher{}
	for _, name := range fields {
		field := reflect.ValueOf(owner).Elem().FieldByName(name)
		watcher := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Interface().(*kvwatch.Watcher)
		if watcher == nil {
			t.Fatalf("the %T holds no %s", owner, name)
		}
		watchers[name] = watcher
	}
	return watchers
}

// watcherStopped reports whether w was stopped (kvwatch.Watcher.Stop), reading its unexported
// field. The caller reads it after the test that stopped it has returned, so nothing writes it then.
func watcherStopped(w *kvwatch.Watcher) bool {
	field := reflect.ValueOf(w).Elem().FieldByName("stopped")
	return reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Bool()
}
