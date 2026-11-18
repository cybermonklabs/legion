//go:build natslive

package gcp_test

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The two configurations are told apart by the order of two facts, not by a time: a server
// closes the route as stale, or the burst drains across it. nats-server v2.15.1 pings a route on
// every tick, whether data flows or not (server/client.go processPingTimer always sends for
// ROUTER). It closes the route as stale on the tick that finds ping_max PINGs unanswered (the
// c.ping.out+1 > maxPingsOut check), so ping_max intervals after the first PING the route left
// unanswered. A PONG resets the count (processPong). A PING, or the PONG that answers it, that
// waits behind the burst on A's side of the route is answered only once the burst has drained.
// The first such PING goes out no sooner than the burst begins, less the moment a PING sent just
// before it takes to reach A. It goes out no later than the route's next tick: at most one
// interval after the burst begins, and for a route made just before the burst, its first tick,
// 1 to 1.2 s after the route is made (setFirstPingTimer). Nothing else closes the route. A logs a
// slow consumer each time its 10 s write_deadline passes, but keeps the route, because a route's
// write_timeout policy is retry (client.go:742-743, 2059). The proxy holds no timer of its own.
//
// Without the pin, A's route takes A's short top-level lines (routePingLines). A closes it as
// stale 5 to 6.2 s after the burst began, a little later on a loaded machine (5.99 to 6.39 s read
// in the runs routeBurst names). The burst needs at least routeBurst/routeRate, 15.7 s, to drain,
// since the proxy never moves it faster than its rate. That margin of more than twice holds by
// construction on any machine. With the pin, the routes keep their own 30 s and ping_max 2, and a
// route whose burst has not drained is closed no sooner than 60 s after the burst began.
//
// R2 exception: the pinned row races that 60 s against the time the burst takes to cross the
// proxy. The 60 s is a server timer that the chart's pin sets. The crossing runs at a wall-clock
// rate that a loaded machine slows. No fact can stand in for this race, because the ping timer is
// the behaviour under test. So routeBurst is sized from measurement: 60 s is at least twice the
// slowest drain read (routeBurst has the readings).
const (
	// routeBurst is what the test publishes on A to a subscriber on B: 384 messages of 16 KiB,
	// 6,291,456 bytes, which take at least 15.7 s to cross the proxy at routeRate. The pinned
	// route lives at least 60 s with the burst undrained. That is at least twice the slowest drain
	// read, from the burst's start to B's subscriber holding all of it. The readings, slowest
	// first, are from builds Jobs with this file (`go test -tags natslive -run
	// '^TestClusteredNatsHubRoutesKeepTheirPingBesideACallout$' ./gcp/`), at 604fd8f4a4 unless
	// marked, with the number of busy loops beside the Job's 2 CPUs:
	//   19.895 (32), 19.703 (16), 19.599 (32), 19.508 (32), 19.469 (16), 19.409 (16),
	//   19.407 (32), 19.309 (32), 19.299 (32), 19.209 (32), 16.043 (0, at 40c961b68b),
	//   15.799 (0), 15.798 (0), 15.796 (0, at 40c961b68b) and 15.790 s (0).
	// The slowest is 19.9 s, under half of 60 s. With the servers stopped (SIGSTOP) for 30 s
	// during the drain, the burst drained in 41.362 and 40.028 s and no server closed the route.
	// The stop holds the servers' ping timers too, so it moves their limit by the same 30 s.
	routeBurst = 384 << 14
	// routeRate is the proxy's rate (testnats.ProxyRate), in bytes a second in each direction of
	// the route connection.
	routeRate = 400_000
)

// routePingLines replace the callout's top-level ping lines (calloutPingLines: 20 s, ping_max 2)
// in the auth.conf of A, the server whose route the burst waits on. With ping_interval 1 s and
// ping_max 5, A closes a connection 5 s after the first PING it left unanswered. Without the pin,
// A's route takes these lines, so A closes it as stale 5 to 6.2 s after the burst began (6.39 s at
// the most on a loaded machine), well inside the 15.7 s the burst needs to drain. A's only client
// is the publisher, and the row is done with it once the burst is flushed. B, whose client is the
// subscriber, keeps the chart's lines. The cluster block's pin, which this row is about, stays as
// the chart renders it.
const routePingLines = "ping_interval: \"1s\"\nping_max: 5\n"

// routeClosed matches a line nats-server logs when it closes a route connection to another server,
// with its reason and that server's name (server/client.go markConnAsClosed: "Router connection
// closed: <reason> - Remote: <name>"). A close of a server's route to itself names no remote.
var routeClosed = regexp.MustCompile(`(?m)\[INF\] .* - rid:\d+ - Router connection closed: (.+) - Remote: (\S+)$`)

// staleReason is the reason a server logs for a route it closed as stale (server/client.go
// StaleConnection).
const staleReason = "Stale Connection"

// routePinEntry matches one entry of the routes' ping pin in the rendered nats.conf's cluster block.
var routePinEntry = regexp.MustCompile(`"ping_(interval|max)":\s*("30s"|2),?\s*`)

// closeReasons is the reason of each route connection to remote that a server's output logs as
// closed, in the order logged.
func closeReasons(out, remote string) []string {
	var reasons []string
	for _, m := range routeClosed.FindAllStringSubmatch(out, -1) {
		if m[2] == remote {
			reasons = append(reasons, m[1])
		}
	}
	return reasons
}

// fate is how a route's burst ended:
//   - entered: whether A queued all of the burst for its routes;
//   - closed: the reasons the servers logged for each connection between A and B that they closed
//     once the burst began (none when the burst drained first);
//   - crossed: how many of the burst's bytes B's subscriber held when the drain's wait ended;
//   - took: how long after the burst began that wait ended, on the test's clock. took is logged
//     for routeBurst's readings and judges nothing.
type fate struct {
	entered bool
	closed  []string
	crossed int64
	took    time.Duration
}

// drainARoute runs two servers of the pods r renders on natsConf. A routes to B through a
// testnats.Proxy at routeRate, so the one route is A's, and A runs routePingLines. It publishes
// routeBurst on A to a subscriber on subject, which is the burst's subject on B unless a control
// moves it. Then it waits for whichever comes first: B's subscriber holding the whole burst, a
// server logging the route between A and B closed as stale, or both servers logging it closed for
// another reason. Only the test binary's deadline bounds that wait. A burst that A did not queue
// for the route ends the drain at once, since nothing of it can then cross. B is its own only
// peer, so it solicits a route to its own route port, which nats-server ignores (server/route.go
// routesToSelf), since a JetStream cluster refuses to start with no configured route ("JetStream
// cluster requires configured routes").
func drainARoute(t *testing.T, r *clusteredHubRender, binary, natsConf, subject string) fate {
	t.Helper()
	b := r.newHubServer(t, r.release+"-1", binary, natsConf, reservePorts(t, 3))
	b.peers = []*hubServer{b}
	b.start(t)
	proxy := testnats.StartProxy(t, fmt.Sprintf("nats://127.0.0.1:%d", b.route), testnats.ProxyRate(routeRate))
	quick := *r
	quick.authConf = strings.TrimSuffix(r.authConf, calloutPingLines) + routePingLines
	a := quick.newHubServer(t, r.release+"-0", binary, natsConf, reservePorts(t, 3))
	a.routeURLs = []string{proxy.URL()}
	a.start(t)
	within(t, "A routes to B", func() error {
		remotes, err := a.routeRemotes()
		if err != nil {
			return err
		}
		if !remotes[b.name] {
			return errors.New("A has no route to B yet")
		}
		return nil
	})

	sub, err := testnats.ConnectSeed(t, b.url(), r.keys.operatorSeed)
	require.NoError(t, err)
	t.Cleanup(sub.Close)
	var received atomic.Int64
	var probed atomic.Bool
	_, err = sub.Subscribe("load.probe", func(*natsgo.Msg) { probed.Store(true) })
	require.NoError(t, err)
	_, err = sub.Subscribe(subject, func(m *natsgo.Msg) { received.Add(int64(len(m.Data))) })
	require.NoError(t, err)
	require.NoError(t, sub.Flush())
	pub, err := testnats.ConnectSeed(t, a.url(), r.keys.operatorSeed)
	require.NoError(t, err)
	t.Cleanup(pub.Close)
	within(t, "B's subscription reaches A", func() error {
		require.NoError(t, pub.Publish("load.probe", nil))
		require.NoError(t, pub.Flush())
		if !probed.Load() {
			return errors.New("no probe through yet")
		}
		return nil
	})

	// Only closes of the route between A and B count, and only those logged once the burst began.
	closedBefore := []int{len(closeReasons(a.proc.Output.String(), b.name)), len(closeReasons(b.proc.Output.String(), a.name))}
	queuedBefore := routeQueued(t, a)
	payload := make([]byte, 16<<10)
	began := time.Now()
	for range routeBurst / len(payload) {
		require.NoError(t, pub.Publish("load.burst", payload))
	}
	// The flush returns once A has read every message before its PING, and A queues each message
	// for a route as it reads it. From here on, A's routes hold all of the burst they will ever
	// carry.
	require.NoError(t, pub.FlushWithContext(testwait.Context(t)))
	var f fate
	if queued := routeQueued(t, a) - queuedBefore; queued < routeBurst {
		f.crossed, f.took = received.Load(), time.Since(began)
		t.Logf("A queued %d of the burst's %d bytes for its routes, so the burst never entered the route; %d bytes reached B's subscriber", queued, routeBurst, f.crossed)
		return f
	}
	f.entered = true
	testwait.Until(t, "the burst to drain, a server to log the route closed as stale, or both servers to log a route closed", func() (string, bool) {
		crossed := received.Load()
		closedA := closeReasons(a.proc.Output.String(), b.name)[closedBefore[0]:]
		closedB := closeReasons(b.proc.Output.String(), a.name)[closedBefore[1]:]
		closed := append(slices.Clone(closedA), closedB...)
		if crossed >= routeBurst || slices.Contains(closed, staleReason) || (len(closedA) > 0 && len(closedB) > 0) {
			f.closed, f.crossed, f.took = closed, crossed, time.Since(began)
			return "", true
		}
		return fmt.Sprintf("%d of the burst's %d bytes across; routes closed since it began: %q", crossed, routeBurst, closed), false
	})
	if len(f.closed) == 0 {
		t.Logf("the burst drained, all %d bytes, %s after it began, with no route closed", f.crossed, f.took.Round(time.Millisecond))
	} else {
		t.Logf("a route was closed (%s) %s after the burst began, with %d of its %d bytes across",
			strings.Join(f.closed, ", "), f.took.Round(time.Millisecond), f.crossed, routeBurst)
	}
	return f
}

// routeQueued is every payload byte s has queued for its routes: /routez out_bytes, summed over
// the routes. nats-server counts a message's payload there when it queues the message for the route
// (server/client.go deliverMsg), not when the message is written to the socket. A server that the
// machine is starving of CPU can answer its monitoring port late, or not within monitorClient's
// 2 s. Such a read counts as "not yet", not as a failure.
func routeQueued(t *testing.T, s *hubServer) int64 {
	t.Helper()
	return testwait.Until(t, fmt.Sprintf("%s's /routez to answer", s.name), func() (int64, bool) {
		var routez struct {
			Routes []struct {
				OutBytes int64 `json:"out_bytes"`
			} `json:"routes"`
		}
		if err := s.monitorJSON("/routez", &routez); err != nil {
			return 0, false
		}
		var queued int64
		for _, route := range routez.Routes {
			queued += route.OutBytes
		}
		return queued, true
	})
}

// drainedFirst is the pinned row's judgement of f. It is nil when the burst entered the route and
// all of it crossed with no server closing a route, so the pin kept a live, busy route open.
// Otherwise it says what failed.
func drainedFirst(f fate) error {
	switch {
	case !f.entered:
		return errors.New("the burst never entered the route, so none of it drained across")
	case slices.Contains(f.closed, staleReason):
		return fmt.Errorf("the route was closed as stale before the burst drained, with %d of its %d bytes across", f.crossed, routeBurst)
	case len(f.closed) > 0:
		return fmt.Errorf("the route was closed (%s) before the burst drained, with %d of its %d bytes across", strings.Join(f.closed, ", "), f.crossed, routeBurst)
	}
	return nil
}

// TestClusteredNatsHubRoutesKeepTheirPingBesideACallout checks the hub's configuration beside a
// callout. The chart renders nats.conf's cluster block with the routes' own ping_interval "30s"
// and ping_max 2, and renders auth.conf with the top-level 20 s ping lines. On that configuration,
// with A's top-level lines shortened to routePingLines, a route carries a burst that takes far
// longer than A's top-level ping allows, and the burst crosses in full before any server closes
// the route. The routes keep today's 30 s ping, so only client connections get the top-level one.
//
// Two controls show that the row can fail. Without the pin, A's top-level ping governs the route
// too, and A closes the route as stale before the burst has drained. With the burst sent where no
// subscriber listens, the burst never enters the route, so it cannot drain across it.
func TestClusteredNatsHubRoutesKeepTheirPingBesideACallout(t *testing.T) {
	keys := newLiveHubKeys(t)
	args := withoutTailnet(liveHubArgs(keys))
	args.Users = append(args.Users, authServiceUser())
	args.AuthCallout = authCallout()
	r := renderClusteredHubArgs(t, keys, args)
	compact := strings.Join(strings.Fields(r.natsConf), "")
	require.Contains(t, compact, `"no_advertise":true,"ping_interval":"30s","ping_max":2,"port":6222,`,
		"the rendered cluster block pins the routes' ping:\n%s", r.natsConf)
	require.True(t, strings.HasSuffix(r.authConf, calloutPingLines), "auth.conf ends with the callout's ping lines:\n%s", r.authConf)
	require.Len(t, routePinEntry.FindAllString(r.natsConf, -1), 2, "the pin's two entries:\n%s", r.natsConf)
	withoutPin := routePinEntry.ReplaceAllString(r.natsConf, "")
	require.NotContains(t, withoutPin, "ping_")
	natsServer := installTools(t, t.TempDir(), liveTool{natsServerTool, "nats-server"})[0]

	t.Run("pinned", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, drainedFirst(drainARoute(t, r, natsServer, r.natsConf, "load.burst")))
	})
	t.Run("control: without the pin", func(t *testing.T) {
		t.Parallel()
		require.ErrorContains(t, drainedFirst(drainARoute(t, r, natsServer, withoutPin, "load.burst")),
			"closed as stale before the burst drained", "without the pin the burst drained with no route closed as stale")
	})
	t.Run("control: a burst that never crosses", func(t *testing.T) {
		t.Parallel()
		require.ErrorContains(t, drainedFirst(drainARoute(t, r, natsServer, r.natsConf, "load.elsewhere")),
			"never entered the route", "the pinned row passed on a route the burst never crossed")
	})
}
