package natscallout

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// hubConnects is how many client connections server has accepted since it last started, the ones
// it then refused included: nats-server counts a connection as it accepts it, before it reads the
// connection's CONNECT (nats-server v2.15.1 server.go createClientEx), and reports the count as
// varz's total_connections.
func hubConnects(t *testing.T, server *testnats.Server) int64 {
	t.Helper()
	resp, err := http.Get(server.MonitorURL() + "/varz")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "the hub's /varz")
	var varz struct {
		TotalConnections *int64 `json:"total_connections"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&varz))
	require.NotNil(t, varz.TotalConnections, "the hub's /varz names no total_connections")
	return *varz.TotalConnections
}

// startRefused starts the auth service against the server at url with keys' seeds, stopped when
// the test ends, without waiting for it to connect: here the hub refuses it.
func startRefused(t *testing.T, url string, keys testnats.CalloutKeys) *Service {
	t.Helper()
	service, err := Start(t.Context(), serviceSettings(t, url, keys), Deps{Log: slog.New(slog.DiscardHandler)})
	require.NoError(t, err)
	t.Cleanup(service.Close)
	return service
}

// redialWindow is how long TestAReplicaTheHubRefusesRedialsOnABackoffAndStaysLive counts the
// replica's connects, and redialsInWindow and redialsAtLeast the most and the least its backoff can
// make in that time (bus.WithPromptReconnect). The most holds for any window short of
// redialWindowBound: the first connect, the first redial at once, and redials after waits of at
// least 125 ms, 250 ms, 500 ms, then 1 s each at the 2 s cap, so the seventh wait ends at 4.875 s
// at the earliest and the eighth at 5.875 s, nine connects. The least sums the waits at their
// longest, under 50 ms, 250 ms, 500 ms and 1 s: the fourth redial starts by 1.8 s, so five connects
// are made with more than 3 s of the window left for their dials. A window that overran its bound,
// as a stalled runner can make it, fails as that rather than as a backoff that redialled too
// often. Without the backoff the replica redialled every 25 ms on average, 35 connects a second
// (1,055 in 30 s, measured before it), so about 175 here.
const (
	redialWindow      = 5 * time.Second
	redialWindowBound = 5875 * time.Millisecond
	redialsInWindow   = 9
	redialsAtLeast    = 5
)

// A replica whose hub refuses its own user, as a hub does whose auth_callout does not list that
// user yet, redials on a backoff (bus.WithPromptReconnect) and never gives up on the hub,
// and its /livez stays 200 throughout: a hub that refuses the replica is not a replica that
// stalled, and a probe that killed it would only start the next replica refused the same way.
//
// The hub counts each connect it accepts, the refused ones included (hubConnects). The test first
// makes one refused connect of its own and reads the count move by one, so the count cannot read
// low for a hub that does not count a refused connect; and the replica must be counted at least
// redialsAtLeast times, so it cannot pass with a replica that stopped dialling.
func TestAReplicaTheHubRefusesRedialsOnABackoffAndStaysLive(t *testing.T) {
	withLiveness(t, 20*time.Millisecond, 500*time.Millisecond)
	keys := testnats.NewCalloutKeys(t)
	_, other := testnats.User(t)
	server, url := testnats.StartNkeyAuthorizedServer(t, other)

	before := hubConnects(t, server)
	_, err := testnats.ConnectSeed(t, url, keys.ServiceSeed)
	require.ErrorIs(t, err, nats.ErrAuthorization, "the hub admitted the service's user, so nothing here is refused")
	require.Equal(t, before+1, hubConnects(t, server), "the hub's count missed a refused connect")

	start := hubConnects(t, server)
	refusing := time.Now()
	service := startRefused(t, url, keys)
	for time.Since(refusing) < redialWindow {
		require.Equal(t, http.StatusOK, livenessStatus(service), "/livez failed while the hub refused the replica")
		require.False(t, service.conn.IsClosed(), "the replica gave up on the hub")
		time.Sleep(50 * time.Millisecond)
	}
	connects := hubConnects(t, server) - start
	elapsed := time.Since(refusing)
	require.False(t, service.conn.IsConnected(), "the hub admitted the replica, so nothing here was refused")
	t.Logf("the replica connected %d times in %s", connects, elapsed.Round(time.Millisecond))
	require.Less(t, elapsed, redialWindowBound,
		"the counted window ran %s, past the %s the schedule's bound holds for", elapsed.Round(time.Millisecond), redialWindowBound)
	require.LessOrEqual(t, connects, int64(redialsInWindow),
		"the replica connected %d times in %s to a hub that refuses it", connects, elapsed.Round(time.Millisecond))
	require.GreaterOrEqual(t, connects, int64(redialsAtLeast),
		"the replica connected only %d times in %s: it stopped redialling the hub", connects, elapsed.Round(time.Millisecond))
}

// The backoff starts over at each connect. A replica the hub refused until its waits reached
// seconds, and then admitted, redials at once when that connection drops, as it must when a hub
// restart drops it, since every other client of the hub waits on it (bus.WithPromptReconnect).
// Here the hub refuses the replica for six connects, after which each of its waits is at least
// 1 s, and is then restarted with the auth_callout that lists it. The replica's link to the hub is
// then cut and restored at once: it is back within half a second (tens of milliseconds in
// practice), where a replica that kept its place in the backoff waits about 1 s or more before it
// even dials, so the bound stays clear of both.
func TestAReplicaRedialsAtOnceWhenItsAdmittedConnectionDrops(t *testing.T) {
	keys := testnats.NewCalloutKeys(t)
	_, other := testnats.User(t)
	server, url := testnats.StartNkeyAuthorizedServer(t, other)
	link := testnats.StartProxy(t, url)

	start := hubConnects(t, server)
	service := startRefused(t, link.URL(), keys)
	testwait.WithinPolling(t, 30*time.Second, 10*time.Millisecond, "six refused connects", func() bool {
		return hubConnects(t, server)-start >= 6
	})
	require.False(t, service.conn.IsConnected(), "the hub admitted the replica, so nothing here was refused")
	server.Restart(t, testnats.CalloutConfig(keys))
	ready, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	require.NoError(t, service.Ready(ready), "the hub that lists the replica never admitted it")

	link.Cut()
	testwait.WithinPolling(t, 10*time.Second, time.Millisecond, "the replica's connection dropping", func() bool {
		return !service.conn.IsConnected()
	})
	link.Restore()
	restored := time.Now()
	testwait.WithinPolling(t, 30*time.Second, 5*time.Millisecond, "the replica back", service.conn.IsConnected)
	back := time.Since(restored)
	t.Logf("the replica was back %s after its link was restored", back.Round(time.Millisecond))
	require.Less(t, back, 500*time.Millisecond, "the replica took %s to redial: it kept its place in the backoff", back.Round(time.Millisecond))
}
