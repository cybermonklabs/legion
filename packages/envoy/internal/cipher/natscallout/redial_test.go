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
// replica's connects, and redialsInWindow the most its backoff can make in that time, or in any
// time short of 7.875 s: the first connect, the first redial at once, and redials after waits of
// at least 125 ms, 250 ms, 500 ms, 1 s and 2 s, 3.875 s in all, with the next at least 4 s after
// the last of them (bus.WithPromptReconnect). Without the backoff the replica redialled every
// 25 ms on average, 35 connects a second (1,055 in 30 s, measured before it), so about 175 here.
const (
	redialWindow    = 5 * time.Second
	redialsInWindow = 7
)

// A replica whose hub refuses its own user, as a hub does whose auth_callout does not list that
// user yet, redials on a backoff (bus.WithPromptReconnect) and never gives up on the hub,
// and its /livez stays 200 throughout: a hub that refuses the replica is not a replica that
// stalled, and a probe that killed it would only start the next replica refused the same way.
//
// The hub counts each connect it accepts, the refused ones included (hubConnects). The test first
// makes one refused connect of its own and reads the count move by one, so the count cannot read
// low for a hub that does not count a refused connect; and the replica must be counted at least
// three times, so it cannot pass with a replica that stopped dialling.
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
	service := startRefused(t, url, keys)
	refusing := time.Now()
	for time.Since(refusing) < redialWindow {
		require.Equal(t, http.StatusOK, livenessStatus(service), "/livez failed while the hub refused the replica")
		require.False(t, service.conn.IsClosed(), "the replica gave up on the hub")
		time.Sleep(50 * time.Millisecond)
	}
	connects := hubConnects(t, server) - start
	elapsed := time.Since(refusing)
	require.False(t, service.conn.IsConnected(), "the hub admitted the replica, so nothing here was refused")
	t.Logf("the replica connected %d times in %s", connects, elapsed.Round(time.Millisecond))
	require.LessOrEqual(t, connects, int64(redialsInWindow),
		"the replica connected %d times in %s to a hub that refuses it", connects, elapsed.Round(time.Millisecond))
	require.GreaterOrEqual(t, connects, int64(3),
		"the replica connected only %d times in %s: it stopped redialling the hub", connects, elapsed.Round(time.Millisecond))
}

// The backoff starts over at each connect. A replica the hub refused until its waits reached
// seconds, and then admitted, redials at once when that connection drops, as it must when a hub
// restart drops it, since every other client of the hub waits on it (bus.WithPromptReconnect).
// Here the hub refuses the replica for six connects, after which its next wait is at least 2 s,
// and is then restarted with the auth_callout that lists it. The replica's link to the hub is then
// cut and restored at once: it is back within two seconds, where a replica that kept its place in
// the backoff waits at least 4 s.
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
	require.Less(t, back, 2*time.Second, "the replica took %s to redial: it kept its place in the backoff", back.Round(time.Millisecond))
}
