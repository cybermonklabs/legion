package routes

import (
	"testing"

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/horizontest"
)

// Two waits here stand behind the event horizon, whose proof is instance-wide: the event feed's
// events_start (waitEventsStart) and a horizon proven and persisted above the log's newest event
// (horizontest.PersistedThrough). No fixed deadline bounds either; each waits on the horizon's own
// account of itself (internal/dispatch/horizontest), which the router's horizon logs in this
// process at horizontest.StallWarning (storeRouter). The 10 s and 30 s these tests had failed
// whenever another client of the instance held a transaction for longer.

// waitEventsStart returns the first frame but a heartbeat that conn receives once the caller has
// subscribed it to the event feed: the events_start, which the caller checks. ownDatabase is the
// database the router's pool reads. The caller reads conn again only once this has returned.
func waitEventsStart(t *testing.T, conn *gws.Conn, ownDatabase string) []byte {
	t.Helper()
	result := horizontest.WaitInProcess(t, "the event feed's events_start", horizontest.ReadFirstFrame(conn), ownDatabase)
	if result.Err != nil {
		t.Fatalf("read the event feed's events_start: %v", result.Err)
	}
	return result.Data
}
