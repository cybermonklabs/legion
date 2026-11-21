package routes_test

// A regression lock from the independent test of the route check at 7c7131eece: it passes at that
// head, and fails when the log rule's window opens at a later "Reloaded: cluster" line than the
// first, which no other test of the change notices.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/routes"
)

// The window opens at the first "Reloaded: cluster" line of the log, the reload that loaded
// authorization, and a later one cannot move it: a route peer that held a route through that
// reload, which the hub dialed again with the password, can write a "Reloaded: cluster" line and a
// "Reloaded server configuration" line through its server name when the route closes, with the
// server's process id and the time now; a window opened there would leave out the peer's own route
// connections after the real reload, and the check would be Clean. Control: without the written
// lines the connection is PasswordRead too, so the row tests where the window opens and nothing else.
func TestCheckOpensTheWindowAtTheFirstAuthorizationReload(t *testing.T) {
	connection := logLine("INF", "10.9.9.8:6222 - rid:40 - Route connection created") + "\n" +
		logLine("INF", "10.9.9.8:6222 - rid:40 - Router connection closed: Client Closed - Remote: x") + "\n"
	later := authorizationAt.Add(time.Minute)
	written := logLineAt(later, "INF", "Reloaded: cluster") + "\n" + logLineAt(later, "INF", "Reloaded server configuration (feed)") + "\n"
	for _, row := range []struct {
		name string
		tail string
	}{
		{"control: the connection alone", connection},
		{"the connection, then a later Reloaded: cluster line", connection + written},
	} {
		t.Run(row.name, func(t *testing.T) {
			h := cleanHub()
			rec := recordOf(t, h)
			h.logs["envoy-nats-1"] += row.tail
			report := routes.Check(context.Background(), h, rec)
			require.Equal(t, routes.PasswordRead, report.Verdict(), "findings: %v", report.Findings)
		})
	}
}
