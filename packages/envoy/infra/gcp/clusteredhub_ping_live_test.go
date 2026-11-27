//go:build natslive

package gcp_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testport"
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
// 1 to 1.2 s after the route is made (setFirstPingTimer). A route's write deadline closes it too,
// whatever its write_timeout policy, when the write it bounds wrote nothing at all
// (client.go:2058-2061, "Slow Consumer (Write Deadline)"): with the chart's lines that is nats-server's
// default of 10 s, which a proxy the machine does not run for 10 s meets. This row's forcing met it
// once in three runs (the whole test stopped 45 s once each burst was queued). So A runs a
// write_deadline no test reaches (routePingLines), and nothing but a ping closes its route. The
// proxy holds no timer of its own.
//
// Without the pin, A's route takes A's short top-level lines (routePingLines). A closes it as
// stale 5 to 6.2 s after the burst began, a little later on a loaded machine (5.99 to 6.39 s read
// in the runs routeBurst names). The burst needs at least (routeBurst-proxyRunAhead)/routeRate,
// 15.5 s, to drain: the proxy forwards each read before it sleeps for it, so it runs up to one read
// ahead of its rate (testnats.ProxyRate), and never further. That margin of more than twice holds
// by construction on any machine. With the pin, the routes keep their own 30 s and ping_max 2, and
// a route whose burst has not drained is closed no sooner than 60 s after the burst began.
//
// The one race the row keeps: the pinned row races that 60 s against the time the burst takes to
// cross the proxy. The 60 s is a server timer that the chart's pin sets. The crossing runs at a
// wall-clock rate that a loaded machine slows. No fact can stand in for this race, because the
// ping timer is the behaviour under test. So routeBurst is sized from measurement: 60 s is at
// least twice the slowest drain read (routeBurst has the readings).
const (
	// routeBurst is what the test publishes on A to a subscriber on B: 384 messages of 16 KiB,
	// 6,291,456 bytes, which take at least 15.5 s to cross the proxy at routeRate. The pinned
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
	// proxyRunAhead is the most one read of the proxy takes, 64 KiB (testnats' proxyReadBuffer),
	// and so the most it forwards ahead of its rate.
	proxyRunAhead = 64 << 10
)

// routePingLines replace the callout's top-level ping lines (calloutPingLines: 20 s, ping_max 2)
// in the auth.conf of A, the server whose route the burst waits on. With ping_interval 1 s and
// ping_max 5, A closes a connection 5 s after the first PING it left unanswered. Without the pin,
// A's route takes these lines, so A closes it as stale 5 to 6.2 s after the burst began (6.39 s at
// the most on a loaded machine), well inside the 15.5 s the burst needs to drain. Their
// write_deadline, which a route takes where the cluster block sets none (as the chart's sets none:
// client.go initClient), is an hour, longer than any -timeout this row runs under, so no write of
// A's to a proxy the machine stalls closes the route. A's only client is the publisher, and the row
// is done with it once the burst is flushed. B, whose client is the subscriber, keeps the chart's
// lines. The cluster block's pin, which this row is about, stays as the chart renders it.
const routePingLines = "ping_interval: \"1s\"\nping_max: 5\nwrite_deadline: \"1h\"\n"

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

// routeClose is one close of a route connection between A and B that a server logged: its reason,
// and when the drain's wait first read it, on the test's monotonic clock.
type routeClose struct {
	reason string
	seen   time.Time
}

// closeLog is the closes one server has logged of its route connections to remote since the burst
// began, each stamped when the drain's wait first read it (read).
type closeLog struct {
	before int
	closes []routeClose
}

// read adds to l each close out logs past those l holds, stamped now.
func (l *closeLog) read(out, remote string, now time.Time) {
	reasons := closeReasons(out, remote)
	for _, reason := range reasons[l.before+len(l.closes):] {
		l.closes = append(l.closes, routeClose{reason: reason, seen: now})
	}
}

// fate is how a route's burst ended:
//   - entered: whether A queued all of the burst for its routes;
//   - closed: the reasons the servers logged for each connection between A and B that they closed
//     once the burst began and before it drained (none when the burst drained first);
//   - crossed: how many of the burst's bytes B's subscriber held when the drain's wait ended;
//   - took: how long after the burst began that wait ended, on the test's clock. took is logged
//     for routeBurst's readings and judges nothing.
type fate struct {
	entered bool
	closed  []string
	crossed int64
	took    time.Duration
}

// routePoll is how often drainARoute asks a server's monitoring port, dials a client, or sends a
// probe, while it waits for the route between A and B, for its clients and for B's subscription to
// reach A.
const routePoll = 100 * time.Millisecond

// flushSpare is how long before the test binary's deadline the row's flushes give up, as the shared
// wait gives up (testwait.Until), so a flush a server never answers fails the row by name.
const flushSpare = 30 * time.Second

// dialSeed connects to url as the user of seed, and dials again until a connect succeeds:
// ConnectSeed gives each dial's handshake one second, which a server the machine starves of CPU can
// pass, and only the test binary's deadline bounds the wait (testwait.UntilPolling).
func dialSeed(t *testing.T, url, seed string) *natsgo.Conn {
	t.Helper()
	var conn *natsgo.Conn
	testwait.UntilPolling(t, routePoll, "a client connection to "+url, func() (string, bool) {
		var err error
		if conn, err = testnats.ConnectSeed(t, url, seed); err != nil {
			return err.Error(), false
		}
		return "", true
	})
	return conn
}

// stale reports whether closes holds a close as stale.
func stale(closes []routeClose) bool {
	return slices.ContainsFunc(closes, func(c routeClose) bool { return c.reason == staleReason })
}

// drainARoute runs two servers of the pods r renders on natsConf. A routes to B through a
// testnats.Proxy at routeRate, so the one route is A's, and A runs routePingLines. It publishes
// routeBurst on A to a subscriber on subject, which is the burst's subject on B unless a control
// moves it. Then it waits for whichever comes first: B's subscriber holding the whole burst, a
// server logging the route between A and B closed as stale, or both servers logging it closed for
// another reason. Only the test binary's deadline bounds that wait. When the wait finds both a
// drained burst and a close, the two are ordered on the test's monotonic clock: the moment B's
// subscriber took the burst's last byte, against the moment the wait first read the close's line.
// The wait reads the servers' output every testwait.Poll, so a close logged less than that, and the
// pipe's own delay, before the drain reads as after it; the configurations this row tells apart
// differ by seconds (5 to 6.4 s against 15.5 s, 60 s against 20 s). A burst that A did not queue
// for the route ends the drain at once, since nothing of it can then cross. B is its own only peer,
// so it solicits a route to its own route port, which nats-server ignores (server/route.go
// routesToSelf), since a JetStream cluster refuses to start with no configured route ("JetStream
// cluster requires configured routes").
func drainARoute(t *testing.T, r *clusteredHubRender, binary, natsConf, subject string) fate {
	t.Helper()
	// nats.go flushes only under a context with a deadline: the row's is the test binary's own less
	// flushSpare, not a span of its own.
	deadline, cancel := context.WithTimeout(t.Context(), testwait.Remaining(t, flushSpare))
	defer cancel()
	// Both servers listen at one loopback address of the test's own (hubServer.host).
	host := testport.LoopbackIP().String()
	b := r.newHubServer(t, r.release+"-1", binary, natsConf, host, reservePortsOn(t, host, 3))
	b.peers = []*hubServer{b}
	b.start(t)
	proxy := testnats.StartProxy(t, "nats://"+net.JoinHostPort(b.host, strconv.Itoa(b.route)), testnats.ProxyRate(routeRate))
	quick := *r
	quick.authConf = strings.TrimSuffix(r.authConf, calloutPingLines) + routePingLines
	a := quick.newHubServer(t, r.release+"-0", binary, natsConf, host, reservePortsOn(t, host, 3))
	a.routeURLs = []string{proxy.URL()}
	a.start(t)
	testwait.UntilPolling(t, routePoll, "A routes to B", func() (string, bool) {
		remotes, err := a.routeRemotes()
		if err != nil {
			return err.Error(), false
		}
		if !remotes[b.name] {
			return "A has no route to B yet", false
		}
		return "", true
	})

	sub := dialSeed(t, b.url(), r.keys.operatorSeed)
	t.Cleanup(sub.Close)
	// received is the burst's bytes B's subscriber took; drainedAt, the moment, on the test's
	// monotonic clock, it took the last of them, stored before the wait can read the count whole
	// (the wait reads a whole count with no moment yet as a drain still under way).
	var received atomic.Int64
	var drainedAt atomic.Pointer[time.Time]
	var probed atomic.Bool
	_, err := sub.Subscribe("load.probe", func(*natsgo.Msg) { probed.Store(true) })
	require.NoError(t, err)
	_, err = sub.Subscribe(subject, func(m *natsgo.Msg) {
		size := int64(len(m.Data))
		if received.Load()+size >= routeBurst {
			now := time.Now()
			drainedAt.CompareAndSwap(nil, &now)
		}
		received.Add(size)
	})
	require.NoError(t, err)
	require.NoError(t, sub.FlushWithContext(deadline))
	pub := dialSeed(t, a.url(), r.keys.operatorSeed)
	t.Cleanup(pub.Close)
	testwait.UntilPolling(t, routePoll, "B's subscription reaches A", func() (string, bool) {
		require.NoError(t, pub.Publish("load.probe", nil))
		require.NoError(t, pub.FlushWithContext(deadline))
		if !probed.Load() {
			return "no probe through yet", false
		}
		return "", true
	})

	// Only closes of the route between A and B count, and only those logged once the burst began.
	closedA := closeLog{before: len(closeReasons(a.proc.Output.String(), b.name))}
	closedB := closeLog{before: len(closeReasons(b.proc.Output.String(), a.name))}
	queuedBefore := routeQueued(t, a)
	payload := make([]byte, 16<<10)
	began := time.Now()
	for range routeBurst / len(payload) {
		require.NoError(t, pub.Publish("load.burst", payload))
	}
	// The flush returns once A has read every message before its PING, and A queues each message
	// for a route as it reads it. From here on, A's routes hold all of the burst they will ever
	// carry, and routeQueued counts it whether or not the route has closed since.
	require.NoError(t, pub.FlushWithContext(deadline))
	var f fate
	if queued := routeQueued(t, a) - queuedBefore; queued < routeBurst {
		f.crossed, f.took = received.Load(), time.Since(began)
		t.Logf("A queued %d of the burst's %d bytes for its routes, so the burst never entered the route; %d bytes reached B's subscriber", queued, routeBurst, f.crossed)
		return f
	}
	f.entered = true
	var after []string
	testwait.Until(t, "the burst to drain, a server to log the route closed as stale, or both servers to log a route closed", func() (string, bool) {
		now := time.Now()
		closedA.read(a.proc.Output.String(), b.name, now)
		closedB.read(b.proc.Output.String(), a.name, now)
		crossed, drained := received.Load(), drainedAt.Load()
		closed := append(slices.Clone(closedA.closes), closedB.closes...)
		if (crossed < routeBurst || drained == nil) && !stale(closed) && (len(closedA.closes) == 0 || len(closedB.closes) == 0) {
			return fmt.Sprintf("%d of the burst's %d bytes across; routes closed since it began: %d", crossed, routeBurst, len(closed)), false
		}
		f.crossed, f.took = crossed, time.Since(began)
		for _, c := range closed {
			if drained == nil || c.seen.Before(*drained) {
				f.closed = append(f.closed, c.reason)
			} else {
				after = append(after, c.reason)
			}
		}
		return "", true
	})
	switch {
	case len(f.closed) > 0:
		t.Logf("a route was closed (%s) %s after the burst began, with %d of its %d bytes across",
			strings.Join(f.closed, ", "), f.took.Round(time.Millisecond), f.crossed, routeBurst)
	case len(after) > 0:
		t.Logf("the burst drained, all %d bytes, by %s after it began, and a route was closed (%s) after B's subscriber took its last byte",
			f.crossed, f.took.Round(time.Millisecond), strings.Join(after, ", "))
	default:
		t.Logf("the burst drained, all %d bytes, %s after it began, with no route closed", f.crossed, f.took.Round(time.Millisecond))
	}
	return f
}

// routeQueued is every payload byte s has queued for its routes since it started: /varz out_bytes
// less out_client_bytes. nats-server adds a message's payload to the first when it queues the
// message for any connection, and to the second as well when that connection is a client's
// (server/client.go processMsgResults); s has no leaf node or gateway. Unlike /routez, whose
// counters go with a route that closes, these sums keep the bytes of a route the burst's stale
// close has already taken down. A server the machine starves of CPU can answer its monitoring port
// late, or not within monitorClient's 2 s; such a read counts as "not yet", not as a failure.
func routeQueued(t *testing.T, s *hubServer) int64 {
	t.Helper()
	return testwait.Until(t, fmt.Sprintf("%s's /varz to answer", s.name), func() (int64, bool) {
		var varz struct {
			OutBytes       int64 `json:"out_bytes"`
			OutClientBytes int64 `json:"out_client_bytes"`
		}
		if err := s.monitorJSON("/varz", &varz); err != nil {
			return 0, false
		}
		return varz.OutBytes - varz.OutClientBytes, true
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
