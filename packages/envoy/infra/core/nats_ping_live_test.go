//go:build natslive

package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/testnats"
)

// The tests in this file run the callout's ping lines (calloutPingLines: ping_interval 20 s,
// ping_max 2) on the hub's image (testnats.HubImage) against a connection behind a testnats.Proxy
// the test freezes: the server sees a connection that answers nothing, not one that closed.
//
// Two of them hang a replica of a test auth service. Two replicas answer in one queue group, as the
// auth service runs its replicas (dispatch://INFRA-840), and replica A is behind the proxy.
// nats-server sends each authorization request to one member of the group, so while A keeps its
// place, each client the server routes to A waits out the 2 s authorization timeout and is
// refused.
//
// The other two freeze a keyed client for 15 s, as a client whose event loop blocks, and then let
// it go on.

const (
	// calloutQueue is the queue group both replicas answer authorization requests in.
	calloutQueue = "example-auth-service"
	// replicaA is the name of the replica that hangs, as the server's /connz lists it.
	replicaA = "example-replica-a"
	// hungReplicaDeadline is how long after the hang the test waits for the server to close A.
	// With ping_interval 20 s and ping_max 2 the server closes a replica that hangs 40 to 80 s
	// later: 2 to 3 intervals if it sent nothing in the interval before, 3 to 4 if it sent data up
	// to the hang (core/nats_callout.go calloutPingLines). With the defaults it is 4 to 8 minutes.
	hungReplicaDeadline = 81 * time.Second
	// clientsAfterTheHang is how many clients connect once A is closed or the deadline has passed.
	// With A still in the group, each goes to A with even odds, so all of them reaching B by chance
	// is a one-in-a-million event.
	clientsAfterTheHang = 20
	// clientStall is how long the stalled client answers nothing: longer than the 6 to 9 s in which
	// 3 s pings close an idle client, and shorter than the 40 s 20 s pings tolerate.
	clientStall = 15 * time.Second
	// stalledClient is the name of the stalled client, as the server's /connz lists it.
	stalledClient = "example-stalled-client"
	// staleConnection is the reason nats-server's /connz gives a connection its ping timer closed
	// (server/client.go:5987-5991 closes it as StaleConnection, which monitor.go:2659-2660 names).
	staleConnection = "Stale Connection"
)

// monitorHTTP reads the server's monitoring: a /connz that hangs fails its row in 2 s, naming it,
// where http.Get would block the row until the package's -timeout.
var monitorHTTP = &http.Client{Timeout: 2 * time.Second}

// startHubOn runs the hub's image on config with its monitoring port open, on config itself
// rather than on a configuration a probe could read and then again on config (calloutRig.startOn
// says why). It returns the client URL and the monitoring endpoint.
func startHubOn(t *testing.T, config string) (uri, monitor string) {
	t.Helper()
	ctr, uri := testnats.StartImageReady(t, testnats.HubImage, config, []string{"-js", "-m", "8222"})
	monitor, err := ctr.PortEndpoint(context.Background(), "8222/tcp", "http")
	require.NoError(t, err)
	return uri, monitor
}

// hangOutcome is what a hung replica left: whether the server still listed A when the test stopped
// waiting, and the errors of the clients that connected afterwards that the server refused.
type hangOutcome struct {
	stillConnected bool
	refused        []error
}

// hangAReplica runs the hub's image on config, rig's authorization rendered, starts replicas A
// (through a testnats.Proxy) and B in the callout's queue group as rig's service user, freezes A
// for good, waits up to hungReplicaDeadline for the server to close A, and then connects
// clientsAfterTheHang clients presenting the admitted token, one at a time.
func hangAReplica(t *testing.T, rig *calloutRig, config string) hangOutcome {
	t.Helper()
	uri, monitor := startHubOn(t, config)
	replica := func(server, name string) {
		conn := connectWhenUp(t, server, rig.serviceSeed, natsgo.Name(name))
		_, err := conn.QueueSubscribe("$SYS.REQ.USER.AUTH", calloutQueue, func(m *natsgo.Msg) { rig.answer(t, m) })
		require.NoError(t, err)
		require.NoError(t, conn.Flush())
	}
	replica(uri, "example-replica-b")
	proxy := testnats.StartProxy(t, uri)
	replica(proxy.URL(), replicaA)
	_, listed := connection(t, monitor, "open", replicaA)
	require.True(t, listed, "the server does not list replica A before the hang")

	proxy.Freeze()
	hung := time.Now()
	outcome := hangOutcome{stillConnected: true}
	for outcome.stillConnected && time.Since(hung) < hungReplicaDeadline {
		time.Sleep(250 * time.Millisecond)
		_, outcome.stillConnected = connection(t, monitor, "open", replicaA)
	}
	if outcome.stillConnected {
		t.Logf("the server still listed %s %s after the hang", replicaA, time.Since(hung).Round(100*time.Millisecond))
	} else {
		reason, _ := connection(t, monitor, "closed", replicaA)
		t.Logf("the server had closed %s %s after the hang, as %q", replicaA, time.Since(hung).Round(100*time.Millisecond), reason)
	}
	for i := range clientsAfterTheHang {
		start := time.Now()
		conn, err := testnats.ConnectSeed(t, uri, "", natsgo.Token(sessionToken), natsgo.Timeout(5*time.Second))
		if err != nil {
			t.Logf("client %d refused after %s: %v", i, time.Since(start).Round(100*time.Millisecond), err)
			outcome.refused = append(outcome.refused, err)
			continue
		}
		conn.Close()
	}
	return outcome
}

// stallOutcome is what a 15 s stall left a client: whether its connection is still the one it
// opened (a round trip on it completes, and the server lists it), and the reason the server gave
// for closing it, "" when it did not.
type stallOutcome struct {
	kept   bool
	closed string
}

// stallAClient runs the hub's image on config and connects rig's static user through a
// testnats.Proxy, which it freezes for clientStall and then restores.
func stallAClient(t *testing.T, rig *calloutRig, config string) stallOutcome {
	t.Helper()
	uri, monitor := startHubOn(t, config)
	connectWhenUp(t, uri, rig.staticSeed).Close()
	proxy := testnats.StartProxy(t, uri)
	conn, err := testnats.ConnectSeed(t, proxy.URL(), rig.staticSeed, natsgo.Name(stalledClient))
	require.NoError(t, err)
	t.Cleanup(conn.Close)
	_, listed := connection(t, monitor, "open", stalledClient)
	require.True(t, listed, "the server does not list the client before the stall")

	proxy.Freeze()
	time.Sleep(clientStall)
	proxy.Restore()
	err = conn.FlushTimeout(5 * time.Second)
	_, listed = connection(t, monitor, "open", stalledClient)
	var outcome stallOutcome
	outcome.kept = err == nil && !conn.IsClosed() && listed
	outcome.closed, _ = connection(t, monitor, "closed", stalledClient)
	t.Logf("after a %s stall: round trip %v, connection closed by the client %v, listed by the server %v, closed by the server as %q",
		clientStall, err, conn.IsClosed(), listed, outcome.closed)
	return outcome
}

// connection finds the connection named name among the server's /connz in state, open or closed:
// the reason the server gives for it ("" for an open one) and whether it lists it.
func connection(t *testing.T, monitor, state, name string) (reason string, listed bool) {
	t.Helper()
	response, err := monitorHTTP.Get(monitor + "/connz?limit=1024&state=" + state)
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	var page struct {
		Connections []struct {
			Name   string `json:"name"`
			Reason string `json:"reason"`
		} `json:"connections"`
	}
	require.NoError(t, json.Unmarshal(body, &page), "%s", body)
	for _, c := range page.Connections {
		if c.Name == name {
			return c.Reason, true
		}
	}
	return "", false
}

// renderedWithPing is rig's authorization rendered with its callout, which ends with the ping lines
// the tests in this file measure.
func renderedWithPing(t *testing.T, rig *calloutRig) string {
	t.Helper()
	config := rig.render(t, true)
	require.True(t, strings.HasSuffix(config, calloutPingLines), "the rendered block does not end with the ping lines:\n%s", config)
	return config
}

// TestAHungReplicaLeavesTheCalloutsQueueGroup (t-c-22): on the rendered block, which sets
// ping_interval 20 s and ping_max 2 beside the callout, the server closes the hung replica within
// 81 s, and every client that connects afterwards is admitted, by the replica left.
func TestAHungReplicaLeavesTheCalloutsQueueGroup(t *testing.T) {
	t.Parallel()
	rig := newCalloutRig(t, false)
	outcome := hangAReplica(t, rig, renderedWithPing(t, rig))
	require.False(t, outcome.stillConnected, "the server still lists the hung replica %s %s after the hang", replicaA, hungReplicaDeadline)
	require.Empty(t, outcome.refused, "%d of %d clients were refused after the hung replica should have left the queue group", len(outcome.refused), clientsAfterTheHang)
}

// TestWithoutThePingLinesAHungReplicaKeepsItsPlace is the control, which shows the test above can
// fail: the same block without the ping lines, so the server's defaults (ping_interval 2 minutes,
// ping_max 2) hold. The hung replica is still listed when the deadline passes, and at least one
// client after it is refused, routed to the replica that answers nothing: at the 2 s
// authorization timeout, or by nats.go's handshake on the server's first PING, whichever comes
// first.
func TestWithoutThePingLinesAHungReplicaKeepsItsPlace(t *testing.T) {
	t.Parallel()
	rig := newCalloutRig(t, false)
	without := strings.TrimSuffix(renderedWithPing(t, rig), calloutPingLines)
	require.NotContains(t, without, "ping_interval")
	outcome := hangAReplica(t, rig, without)
	require.True(t, outcome.stillConnected, "the server closed the hung replica %s within %s with its default ping settings", replicaA, hungReplicaDeadline)
	require.NotEmpty(t, outcome.refused, "no client was routed to the hung replica")
	for _, err := range outcome.refused {
		require.True(t, errors.Is(err, natsgo.ErrAuthorization) || strings.Contains(err.Error(), "PING"), "a refusal other than the authorization timeout: %v", err)
	}
}

// TestAClientThatStallsFifteenSecondsStaysConnected: on the rendered block, a client whose event
// loop blocks for 15 s, as envoy-client's 10 s Bun.spawnSync can, keeps its connection. The server
// pings it every 20 s and closes it only after two PINGs go unanswered, so it tolerates any stall
// shorter than 40 s.
func TestAClientThatStallsFifteenSecondsStaysConnected(t *testing.T) {
	t.Parallel()
	rig := newCalloutRig(t, false)
	outcome := stallAClient(t, rig, renderedWithPing(t, rig))
	require.True(t, outcome.kept, "a client that stalled %s lost its connection (closed by the server as %q)", clientStall, outcome.closed)
}

// TestWithThreeSecondPingsAStalledClientIsClosed is the control, which shows the test above can
// fail: the same block with ping_interval 3 s, the value this change first proposed. The server
// closes the stalled client 6 to 9 s into the stall, and closes it as stale, by its ping timer, not
// for any other reason.
func TestWithThreeSecondPingsAStalledClientIsClosed(t *testing.T) {
	t.Parallel()
	rig := newCalloutRig(t, false)
	config := renderedWithPing(t, rig)
	threeSeconds := strings.Replace(config, "\nping_interval: \"20s\"\n", "\nping_interval: \"3s\"\n", 1)
	require.Contains(t, threeSeconds, "\nping_interval: \"3s\"\n")
	outcome := stallAClient(t, rig, threeSeconds)
	require.False(t, outcome.kept, "a client that stalled %s kept its connection with 3 s pings", clientStall)
	require.Equal(t, staleConnection, outcome.closed, "the server closed the stalled client, but not by its ping timer")
}
