//go:build natslive

package gcp_test

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testport"
)

const (
	// routeBurst is what the test publishes on one server to a subscriber on the other: the 40 MB
	// the review of cybermonklabs/legion#4167 measured a 3 s route ping lose most of.
	routeBurst = 40 << 20
	// routeRate is the proxy's rate (testnats.ProxyRate), in bytes a second in each direction of
	// the route connection: the burst takes about 100 s to drain, longer than either ping closes a
	// busy route in.
	routeRate = 400_000
	// routeStaleBound separates the two configurations. A route is pinged on every tick, traffic or
	// not, and its PING queues behind the burst, so the route is closed as stale on the tick after
	// two PINGs go unanswered: 2 to 3 intervals after the burst. That is 40 to 60 s with the
	// top-level 20 s, and 60 to 90 s with the routes' own 30 s (gcp clusteredHubCluster says why).
	routeStaleBound = 60 * time.Second
	// routeCrossedFloor is the least of the burst that must have crossed the route by
	// routeStaleBound after it began: half of what routeRate moves in that time. A route that
	// closed early for another reason, or a burst that never crossed it, falls short.
	routeCrossedFloor = routeRate * int64(routeStaleBound/time.Second) / 2
)

// staleRoute matches nats-server's line for a route it closed as stale, with its local timestamp
// (server/client.go:2128-2130; logger.go's flags hold microseconds).
var staleRoute = regexp.MustCompile(`(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}\.\d{6}) \[INF\] .*Router connection closed: Stale Connection`)

// routePinEntry matches one entry of the routes' ping pin in the rendered nats.conf's cluster block.
var routePinEntry = regexp.MustCompile(`"ping_(interval|max)":\s*("30s"|2),?\s*`)

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
// route (the publisher's flush returned), when a server first logged the route closed as stale, if
// one did, and how many of the burst's bytes had reached B's subscriber routeStaleBound after the
// burst began.
type drained struct {
	burst, queued, stale time.Time
	crossed              int64
}

// closedBy reports whether a server closed the route as stale no later than d after from.
func (d drained) closedBy(from time.Time, bound time.Duration) bool {
	return !d.stale.IsZero() && !d.stale.After(from.Add(bound))
}

// drainARoute runs two servers of the pods r renders on natsConf, A routing to B through a
// testnats.Proxy at routeRate, so the one route is A's; publishes routeBurst on A to a subscriber
// on subject, which is the burst's subject on B unless a control moves it; and waits until
// routeStaleBound and a second have passed since A queued it. B is its own only peer, so it
// solicits a route to its own route port, which nats-server ignores (server/route.go
// routesToSelf), since a JetStream cluster refuses to start with no configured route ("JetStream
// cluster requires configured routes").
func drainARoute(t *testing.T, r *clusteredHubRender, binary, natsConf, subject string) drained {
	t.Helper()
	// Both servers listen at one loopback address of the test's own (hubServer.host).
	host := testport.LoopbackIP().String()
	b := r.newHubServer(t, r.release+"-1", binary, natsConf, host, reservePortsOn(t, host, 3))
	b.peers = []*hubServer{b}
	b.start(t)
	proxy := testnats.StartProxy(t, "nats://"+net.JoinHostPort(b.host, strconv.Itoa(b.route)), testnats.ProxyRate(routeRate))
	a := r.newHubServer(t, r.release+"-0", binary, natsConf, host, reservePortsOn(t, host, 3))
	a.routeURLs = []string{proxy.URL()}
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
	_, err = sub.Subscribe("load.probe", func(*natsgo.Msg) { probed.Store(true) })
	require.NoError(t, err)
	_, err = sub.Subscribe(subject, func(m *natsgo.Msg) { received.Add(int64(len(m.Data))) })
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
	time.Sleep(time.Until(d.burst.Add(routeStaleBound)))
	d.crossed = received.Load()
	time.Sleep(time.Until(d.queued.Add(routeStaleBound + time.Second)))
	d.stale, _ = firstStaleRoute(t, a.proc.Output.String(), b.proc.Output.String())
	if d.stale.IsZero() {
		t.Logf("no route was closed as stale within %s of the burst (queued in %s); %d of %d bytes had crossed %s after the burst began",
			time.Since(d.burst).Round(time.Second), d.queued.Sub(d.burst).Round(time.Millisecond), d.crossed, routeBurst, routeStaleBound)
	} else {
		t.Logf("a route was closed as stale %s after the burst began and %s after A had queued it; %d of %d bytes had crossed %s after the burst began",
			d.stale.Sub(d.burst).Round(time.Millisecond), d.stale.Sub(d.queued).Round(time.Millisecond), d.crossed, routeBurst, routeStaleBound)
	}
	return d
}

// pinnedRouteKept is the pinned row's judgement of d: nil when the route was not closed as stale
// within routeStaleBound of the burst's start and at least routeCrossedFloor of the burst had
// crossed it by then, so the pin kept a live, busy route open; else what failed.
func pinnedRouteKept(d drained) error {
	if d.closedBy(d.burst, routeStaleBound) {
		return fmt.Errorf("the route was closed as stale %s after the burst began; with the routes' own 30 s ping it is closed no sooner than %s",
			d.stale.Sub(d.burst), routeStaleBound)
	}
	if d.crossed < routeCrossedFloor {
		return fmt.Errorf("%d bytes of the burst had crossed the route %s after it began, under the %d a live route at %d B/s moves",
			d.crossed, routeStaleBound, int64(routeCrossedFloor), routeRate)
	}
	return nil
}

// TestClusteredNatsHubRoutesKeepTheirPingBesideACallout: beside a callout, the hub's chart renders
// nats.conf's cluster block with the routes' own ping_interval "30s" and ping_max 2, and auth.conf
// with the top-level 20 s ping lines. On that configuration a route whose queue takes about 100 s
// to drain is not closed as stale within 60 s of the burst, and carries the burst meanwhile: the
// routes keep today's 30 s ping, so only client connections get the 20 s one.
//
// Two controls show the row can fail. Without the pin, the top-level 20 s governs the route too,
// and it is closed as stale within 60 s of A queuing the burst. With the burst sent where no
// subscriber listens, it never crosses the route: no stale close comes either, and the row fails
// on what crossed.
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
		require.NoError(t, pinnedRouteKept(drainARoute(t, r, natsServer, r.natsConf, "load.burst")))
	})
	t.Run("control: without the pin", func(t *testing.T) {
		t.Parallel()
		d := drainARoute(t, r, natsServer, withoutPin, "load.burst")
		require.True(t, d.closedBy(d.queued, routeStaleBound+500*time.Millisecond),
			"without the pin the route was not closed as stale within %s of A queuing the burst", routeStaleBound)
	})
	t.Run("control: a burst that never crosses", func(t *testing.T) {
		t.Parallel()
		d := drainARoute(t, r, natsServer, r.natsConf, "load.elsewhere")
		require.False(t, d.closedBy(d.burst, routeStaleBound), "an idle route was closed as stale")
		require.ErrorContains(t, pinnedRouteKept(d), "bytes of the burst had crossed the route",
			"the pinned row passed on a route the burst never crossed")
	})
}
