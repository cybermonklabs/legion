package natscallout

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The hub clock reads how long a hub server has waited on a request from that server's own
// clock, the issue times on its requests, advanced by this replica's monotonic clock from when each
// arrived, and never from this replica's wall clock, so a skew between the two machines decides
// nothing. Each row starts from one moment on this replica (arrived) and one second on the hub's
// clock (hub); a request's lifetime there is two seconds, the hub's authorization timeout.
func TestTheHubClockDropsOnlyRequestsTheHubHasGivenUpOn(t *testing.T) {
	const lifetime = 2
	arrived := time.Now()
	for name, row := range map[string]struct {
		// earlier is a request the same server sent before, read at its own arrival, or none.
		earlier *hubReading
		// server is the request's server; "hub" unless the row names another.
		server string
		// waited is how long the request has been on this replica since it arrived.
		waited time.Duration
		// sinceEarlier is how long after earlier's arrival the request is read.
		sinceEarlier time.Duration
		gaveUp       bool
	}{
		"a request read as it arrives":                             {gaveUp: false},
		"a request that waited here inside its lifetime":           {waited: lifetime * time.Second, gaveUp: false},
		"a request that waited here past its lifetime":             {waited: 5 * time.Second, gaveUp: true},
		"a request held before it arrived, which an earlier shows": {earlier: &hubReading{issued: 1000, at: arrived}, sinceEarlier: 6 * time.Second, gaveUp: true},
		"the same hold seen from another server's earlier request": {earlier: &hubReading{issued: 1000, at: arrived}, server: "other", sinceEarlier: 6 * time.Second, gaveUp: false},
		"an earlier request from the same second, read at once":    {earlier: &hubReading{issued: 1000, at: arrived}, sinceEarlier: 0, gaveUp: false},
		"an earlier request older than the window":                 {earlier: &hubReading{issued: 1000, at: arrived.Add(-2 * hubReadingWindow)}, sinceEarlier: 2*hubReadingWindow + 6*time.Second, gaveUp: false},
	} {
		t.Run(name, func(t *testing.T) {
			clock := newHubClock()
			server := row.server
			if server == "" {
				server = "hub"
			}
			read := arrived
			if row.earlier != nil {
				// The earlier request was answered when it arrived, from the same server ("hub").
				require.False(t, clock.gaveUp("hub", row.earlier.issued, row.earlier.issued+lifetime, row.earlier.at, row.earlier.at))
				read = row.earlier.at.Add(row.sinceEarlier)
			}
			// The request was issued in the same second as the earlier one, or as read when none:
			// a request a hold kept from arriving arrives at read, and one that waited here
			// arrived waited before read.
			issued := int64(1000)
			gaveUp := clock.gaveUp(server, issued, issued+lifetime, read.Add(-row.waited), read)
			require.Equal(t, row.gaveUp, gaveUp)
		})
	}
}
