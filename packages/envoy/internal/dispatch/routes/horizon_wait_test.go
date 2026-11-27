package routes

import (
	"testing"

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/horizontest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Two waits here stand behind the event horizon, which every event write of the router's database
// holds back until it ends: the event feed's events_start (waitEventsStart) and a horizon proven
// and persisted above the log's newest event (horizontest.AwaitPersisted). No fixed deadline
// bounds either; each reads the database's oldest event writer itself, and only a hold of the
// test's own database past horizontest.OwnHoldBound or the drain before the run's deadline ends it
// (internal/dispatch/horizontest). The 10 s and 30 s these tests had failed whenever a write held
// the horizon for longer.

// waitEventsStart returns the first frame but a heartbeat that conn receives once the caller has
// subscribed it to the event feed: the events_start, which the caller checks. own is the store
// the router reads. The caller reads conn again only once this has returned.
func waitEventsStart(t *testing.T, conn *gws.Conn, own *store.Store) []byte {
	t.Helper()
	result := horizontest.WaitInProcess(t, "the event feed's events_start", horizontest.ReadFirstFrame(conn), own)
	if result.Err != nil {
		t.Fatalf("read the event feed's events_start: %v", result.Err)
	}
	return result.Data
}
