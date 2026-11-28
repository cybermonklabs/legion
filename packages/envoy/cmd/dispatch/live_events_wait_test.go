package main

import (
	"testing"

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/horizontest"
)

// waitEventsStart waits for subscription sub's events_start frame on conn, which the caller has
// just subscribed, and returns once it has arrived. events_start is the last thing a new
// subscription sends: the relay awaits the event horizon and writes the frame only once Await has
// returned, and every event write of the process's database holds the horizon back until it ends,
// so no fixed deadline bounds the wait. It is on the server's own account of itself instead
// (internal/dispatch/horizontest, which carries the
// bounds and their measurements): while the process keeps logging events.StallWarning the wait
// goes on, and it fails - with the process's whole output, naming the newest warning - when the
// process never said it was held, when it stopped saying so, or when the hold is one in the
// process's own database. Every process these tests start warns at horizontest.StallWarning
// (startDispatchProcessWith).
//
// Nothing here touches the connection after this returns, because the caller reads it next
// (socketEnd and watchAgent in shutdown_test.go, openEventFeed in multiprocess_test.go): the frame
// is read on a goroutine that sets no read deadline (horizontest.ReadFirstFrame).
func (p *dispatchProcess) waitEventsStart(t *testing.T, conn *gws.Conn, sub string) {
	t.Helper()
	result := horizontest.Wait(t, "the event feed's events_start", horizontest.ReadFirstFrame(conn), p.stallWarnings(), p.database,
		horizontest.Dialed(p.databaseURL), p.Output.String)
	switch {
	case result.Err != nil:
		t.Fatalf("read the event feed's events_start: %v\n%s", result.Err, p.Output.String())
	case result.Type != "events_start" || result.Sub != sub:
		t.Fatalf("the live socket answered the event feed's subscribe with a %s frame of %q, want %s's events_start", result.Type, result.Sub, sub)
	}
}

// stallWarnings answers the stall warnings the process has written so far, oldest first: what a
// wait on work behind its event horizon keys on (horizontest.Wait, horizontest.NewWatch).
func (p *dispatchProcess) stallWarnings() func() []horizontest.Warning {
	return horizontest.Lines(func() []string { return p.logLines(events.StallWarning) })
}
