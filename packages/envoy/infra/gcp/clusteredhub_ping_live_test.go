//go:build natslive

package gcp_test

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/testnats"
)

const (
	// routeBurst is what the test publishes on one server to a subscriber on the other: the 40 MB
	// the review of cybermonklabs/legion#4167 measured a 3 s route ping lose most of.
	routeBurst = 40 << 20
	// routeRate is the proxy's rate, in bytes a second in each direction of each route connection:
	// the burst takes about 100 s to drain, longer than either ping closes a busy route in.
	routeRate = 400_000
	// routeStaleBound separates the two configurations. A route is pinged on every tick, traffic or
	// not, and its PING queues behind the burst, so the route is closed as stale on the tick after
	// two PINGs go unanswered: 2 to 3 intervals after the burst. That is 40 to 60 s with the
	// top-level 20 s, and 60 to 90 s with the routes' own 30 s.
	routeStaleBound = 60 * time.Second
)

// staleRoute matches nats-server's line for a route it closed as stale, with its local timestamp
// (server/client.go:2128-2130; logger.go's flags hold microseconds).
var staleRoute = regexp.MustCompile(`(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}\.\d{6}) \[INF\] .*Router connection closed: Stale Connection`)

// routePinEntry matches one entry of the routes' ping pin in the rendered nats.conf's cluster block.
var routePinEntry = regexp.MustCompile(`"ping_(interval|max)":\s*("30s"|2),?\s*`)

// startRateLimitedProxy forwards each connection it accepts to upstream, a host:port, at about rate
// bytes a second in each direction, so a sender's socket backs up as a slow link backs it up. Every
// connection closes when the test ends.
func startRateLimitedProxy(t *testing.T, upstream string, rate int) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	})
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", upstream)
			if err != nil {
				client.Close()
				continue
			}
			mu.Lock()
			conns = append(conns, client, server)
			mu.Unlock()
			go forwardAt(server, client, rate)
			go forwardAt(client, server, rate)
		}
	}()
	return listener.Addr().String()
}

// forwardAt copies src to dst at about rate bytes a second until src ends.
func forwardAt(dst, src net.Conn, rate int) {
	buf := make([]byte, 8<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
			time.Sleep(time.Duration(n) * time.Second / time.Duration(rate))
		}
		if err != nil {
			return
		}
	}
}

// firstStaleRoute is the earliest time either output logs a route closed as stale, and whether one
// did.
func firstStaleRoute(t *testing.T, outputs ...string) (time.Time, bool) {
	t.Helper()
	var first time.Time
	for _, out := range outputs {
		for _, m := range staleRoute.FindAllStringSubmatch(out, -1) {
			at, err := time.ParseInLocation("2006/01/02 15:04:05.000000", m[1], time.Local)
			require.NoError(t, err)
			if first.IsZero() || at.Before(first) {
				first = at
			}
		}
	}
	return first, !first.IsZero()
}

// drained is what a route drain left: when the burst began, when A had queued all of it for the
// route (the publisher's flush returned), and when a server first logged the route closed as
// stale, if one did.
type drained struct {
	burst, queued, stale time.Time
}

// closedBy reports whether a server closed the route as stale no later than d after from.
func (d drained) closedBy(from time.Time, bound time.Duration) bool {
	return !d.stale.IsZero() && !d.stale.After(from.Add(bound))
}

// drainARoute runs two servers of the pods r renders on natsConf, A routing to B through a proxy at
// routeRate, so the one route is A's; publishes routeBurst on A to a subscriber on B; and waits
// until routeStaleBound and a second have passed since A queued it. B is its own only peer, so it
// solicits a route to its own route port, which nats-server ignores (server/route.go
// routesToSelf), since a JetStream cluster refuses to start with no configured route ("JetStream
// cluster requires configured routes"); start builds that route from the port B binds, a re-drawn
// one included.
func drainARoute(t *testing.T, r *clusteredHubRender, binary, natsConf string) drained {
	t.Helper()
	ports := newLoopbackPorts()
	b := r.newHubServer(t, r.release+"-1", binary, natsConf, ports, ports.draw(t, 3))
	b.peers = []*hubServer{b}
	b.start(t)
	proxy := startRateLimitedProxy(t, fmt.Sprintf("127.0.0.1:%d", b.route), routeRate)
	a := r.newHubServer(t, r.release+"-0", binary, natsConf, ports, ports.draw(t, 3))
	a.routeURLs = []string{"nats://" + proxy}
	a.start(t)
	within(t, 30*time.Second, "A routes to B", func() error {
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
	_, err = sub.Subscribe("load.>", func(m *natsgo.Msg) {
		if m.Subject == "load.probe" {
			probed.Store(true)
			return
		}
		received.Add(int64(len(m.Data)))
	})
	require.NoError(t, err)
	require.NoError(t, sub.Flush())
	pub, err := testnats.ConnectSeed(t, a.url(), r.keys.operatorSeed)
	require.NoError(t, err)
	t.Cleanup(pub.Close)
	within(t, 30*time.Second, "B's subscription reaches A", func() error {
		require.NoError(t, pub.Publish("load.probe", nil))
		require.NoError(t, pub.Flush())
		if !probed.Load() {
			return errors.New("no probe through yet")
		}
		return nil
	})

	payload := make([]byte, 16<<10)
	var d drained
	d.burst = time.Now()
	for range routeBurst / len(payload) {
		require.NoError(t, pub.Publish("load.burst", payload))
	}
	require.NoError(t, pub.FlushTimeout(30*time.Second))
	d.queued = time.Now()
	time.Sleep(time.Until(d.queued.Add(routeStaleBound + time.Second)))
	d.stale, _ = firstStaleRoute(t, a.proc.Output.String(), b.proc.Output.String())
	if d.stale.IsZero() {
		t.Logf("no route was closed as stale within %s of the burst (queued in %s); %d of %d bytes had arrived",
			time.Since(d.burst).Round(time.Second), d.queued.Sub(d.burst).Round(time.Millisecond), received.Load(), routeBurst)
	} else {
		t.Logf("a route was closed as stale %s after the burst began and %s after A had queued it; %d of %d bytes had arrived %s after the burst",
			d.stale.Sub(d.burst).Round(time.Millisecond), d.stale.Sub(d.queued).Round(time.Millisecond), received.Load(), routeBurst,
			time.Since(d.burst).Round(time.Second))
	}
	return d
}

// TestClusteredNatsHubRoutesKeepTheirPingBesideACallout: beside a callout, the hub's chart renders
// nats.conf's cluster block with the routes' own ping_interval "30s" and ping_max 2, and auth.conf
// with the top-level 20 s ping lines. On that configuration a route whose queue takes about 100 s
// to drain is not closed as stale within 60 s of the burst: the routes keep today's 30 s ping, so
// only client connections get the 20 s one.
//
// The control, the same nats.conf without the pin, shows the row can fail: the top-level 20 s
// then governs the route too, and it is closed as stale within 60 s, losing what was in transit.
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

	// Each bound is measured from the clock that makes it exact. The first tick that queues a PING
	// behind the burst comes after the burst began, so the routes' own 30 s close the route later
	// than 60 s after the burst began; and it comes no later than one interval after A has queued
	// the burst, so the top-level 20 s close it within 60 s of that.
	t.Run("pinned", func(t *testing.T) {
		t.Parallel()
		d := drainARoute(t, r, natsServer, r.natsConf)
		require.False(t, d.closedBy(d.burst, routeStaleBound),
			"the route was closed as stale %s after the burst began; with the routes' own 30 s ping it is closed no sooner than %s",
			d.stale.Sub(d.burst), routeStaleBound)
	})
	t.Run("control: without the pin", func(t *testing.T) {
		t.Parallel()
		d := drainARoute(t, r, natsServer, withoutPin)
		require.True(t, d.closedBy(d.queued, routeStaleBound+500*time.Millisecond),
			"without the pin the route was not closed as stale within %s of A queuing the burst", routeStaleBound)
	})
}
