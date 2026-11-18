package routes

import (
	"testing"

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/horizontest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Two waits here stand behind the event horizon, whose proof is instance-wide: the event feed's
// events_start (waitEventsStart) and a horizon proven and persisted above the log's newest event
// (horizontest.AwaitPersisted). Neither has a bound of its own: each goes through the shared wait,
// fails at once only on a hold of the test's own database, and prints the horizon's own account of
// itself if it gives up (internal/dispatch/horizontest), which the router's horizon logs in this
// process at horizontest.StallWarning (storeRouter). The 10 s and 30 s these tests had failed
// whenever another client of the instance held a transaction for longer.

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
