package natscallout

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A replica that closes logs what it counted since its last line. The reporter writes a kind's
// line at most once a logInterval, and Close "waits for its last report" (reportWhileRunning); at
// 2da040012c that last report kept to the interval as well (tally.take and the drops' own report
// then), so everything a replica refused or dropped in the minute before it stopped was never
// logged: on a rolling restart after a storm, the minute an operator reads the log for. On the
// real stack, a broker at 2da040012c refused 3000 sessions in one burst ten seconds after its first
// refusal line, then got SIGTERM: its log holds the first line (count=6) and none of the 3000.
// Each row logs one line, counts more within the minute, closes the replica, and requires a second
// line naming them.
func TestAClosingReplicaLogsWhatItCountedSinceItsLastLine(t *testing.T) {
	for name, row := range map[string]struct {
		waitingBound, refusingBound int
		line, count                 string
	}{
		// Connections holding no credential, each refused in a decision slot.
		"refusals": {maxWaiting, maxRefusing, "refused NATS connections", "no_credential=2"},
		// Requests past both bounds, each dropped unanswered.
		"drops": {0, 0, "dropped authorization requests", "past_bounds=2"},
	} {
		t.Run(name, func(t *testing.T) {
			withWaitingBound(t, row.waitingBound)
			withRefusingBound(t, row.refusingBound)
			withLiveness(t, 20*time.Millisecond, 10*time.Second)
			keys := testnats.NewCalloutKeys(t)
			url := testnats.StartWithCallout(t, keys)
			log := &logtest.Buffer{}
			service := startService(t, url, keys, Deps{Log: slog.New(slog.NewTextHandler(log, nil))}, Decide)
			connect := func() {
				nc, err := nats.Connect(url, nats.NoReconnect(), nats.Timeout(5*time.Second))
				if err == nil {
					nc.Close()
				}
				require.ErrorIs(t, err, nats.ErrAuthorization)
			}

			connect()
			testwait.WithinPolling(t, 5*time.Second, 20*time.Millisecond, "the first line", func() bool {
				return strings.Contains(log.String(), row.line)
			})
			connect()
			connect()
			service.Close()
			lines := log.Lines(row.line)
			require.Equal(t, 2, strings.Count(lines, row.line),
				"the replica closed without logging the two it counted after its first line:\n%s", lines)
			require.Contains(t, lines, row.count)
		})
	}
}
