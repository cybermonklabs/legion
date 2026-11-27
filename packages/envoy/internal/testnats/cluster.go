package testnats

import (
	"errors"
	"fmt"
	"math"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"

	"github.com/sjawhar/envoy/internal/testport"
)

// Cluster is n nats-servers in this test process, routed to each other with JetStream clustered
// across them, as a replicated hub runs. Each member is a Server start made, so Stop and Start work
// on one member as on a lone server: a restart keeps the member's store, its client URL and its
// route URL.
type Cluster struct {
	Servers []*Server
	// routes are the relays of the members' route ports, member i's at i, which FreezeRoutes
	// freezes to hold every message between the members.
	routes []*relay
	// readySeed is the seed of the nkey user AwaitReady asks each member's JetStream as, "" for a
	// cluster whose members admit a client with no credential (StartClusterConfig). A member of a
	// StartClusterWithCallout cluster reloaded with ReloadMember must keep that user in its
	// configuration, or AwaitReady waits on it until the test's deadline.
	readySeed string
}

// StartCluster runs a cluster of n servers with JetStream, each with a store of its own, shut down
// when the test ends, and returns it once JetStream answers on every member with a meta leader that
// knows all n (AwaitReady).
//
// A member's routes, like its clients, go through a relay this process owns: the member listens
// for routes on a port it picks at each start, and advertises the relay's port, which it holds for
// the test's life, as its route URL. Every member's configuration names every relay's port as a
// route, its own included (nats-server ignores a route to itself, and counts the routes to size
// the JetStream meta group when it bootstraps). So no port is chosen by binding and releasing it,
// which another process could take in between, and a member restarted on a new route port is still
// found where its peers' configurations say. Members do not advertise their own client ports, so a
// client reaches each one only through its relay, and a stopped member's URL refuses as a stopped
// lone server's does.
func StartCluster(t testing.TB, n int) *Cluster {
	t.Helper()
	return StartClusterConfig(t, n, "")
}

// StartClusterConfig is StartCluster with config added to the end of every member's configuration,
// as an authorization block that every member enforces alike. AwaitReady connects to each member
// with no credential and asks JetStream for its account info, so a config that declares users names
// a no_auth_user whose account has JetStream.
func StartClusterConfig(t testing.TB, n int, config string) *Cluster {
	t.Helper()
	cluster := startCluster(t, n, config)
	cluster.AwaitReady(t)
	return cluster
}

// startCluster starts the n members StartClusterConfig describes, each member's relay listening on
// a loopback address of its own (testport.ListenLoopback), and returns without waiting for
// JetStream.
func startCluster(t testing.TB, n int, config string) *Cluster {
	t.Helper()
	if n < 2 {
		t.Fatalf("a cluster needs at least 2 servers, not %d", n)
	}
	members := make([]atomic.Pointer[Server], n)
	routes := make([]string, n)
	listeners := make([]net.Listener, n)
	relays := make([]*relay, n)
	for i := range n {
		listener, err := testport.ListenLoopback()
		if err != nil {
			t.Fatalf("hold member %d's route port: %v", i, err)
		}
		listeners[i] = listener
		routes[i] = listener.Addr().String()
		member := &members[i]
		routeRelay := newRelay(listener, func() (string, error) {
			server := member.Load()
			if server == nil {
				return "", errors.New("the NATS server has not started")
			}
			return server.clusterAddress()
		}, false)
		t.Cleanup(routeRelay.Close)
		relays[i] = routeRelay
	}
	cluster := &Cluster{Servers: make([]*Server, n), routes: relays}
	for i := range n {
		server := start(t, clusterConfig(i, routes, listeners[i].Addr().(*net.TCPAddr).IP.String())+config, []string{"-js"})
		server.name = memberName(i)
		members[i].Store(server)
		cluster.Servers[i] = server
	}
	return cluster
}

// clusterConfig is the configuration of member i of a cluster whose route relays listen at routes.
// The member's own route listener takes host, the address of its route relay (testport.LoopbackIP).
func clusterConfig(i int, routes []string, host string) string {
	urls := make([]string, len(routes))
	for j, route := range routes {
		urls[j] = fmt.Sprintf("%q", "nats-route://"+route)
	}
	return fmt.Sprintf(`server_name: %q
jetstream {}
cluster {
  name: "envoy-test"
  listen: %q
  advertise: %q
  no_advertise: true
  routes: [ %s ]
}
`, memberName(i), net.JoinHostPort(host, "-1"), routes[i], strings.Join(urls, ", "))
}

// memberName is member i's server_name.
func memberName(i int) string { return fmt.Sprintf("n%d", i+1) }

// Name is member i's server_name (Server.Name).
func (c *Cluster) Name(i int) string { return c.Servers[i].Name() }

// FreezeRoutes stops the members' routes moving bytes, in either direction, and returns the func
// that thaws them, which runs once however often it is called. Every route keeps its connection,
// as Server.Freeze keeps a client's: no member hears another until the thaw, so a replicated
// stream's leader takes a publish its own clients send it and cannot store it, since no follower
// hears the proposal, and a second publish under the same message id meanwhile is refused as
// still in process (nats-server's JSStreamDuplicateMessageConflict).
func (c *Cluster) FreezeRoutes(t testing.TB) func() {
	t.Helper()
	thaws := make([]func(), len(c.routes))
	for i, route := range c.routes {
		thaws[i] = route.freeze()
	}
	return func() {
		for _, thaw := range thaws {
			thaw()
		}
	}
}

// Member is the index of the member whose server_name is name, or -1 when none has it.
func (c *Cluster) Member(name string) int {
	for i := range c.Servers {
		if c.Name(i) == name {
			return i
		}
	}
	return -1
}

// URLs are the members' client URLs, each the same for the test's life.
func (c *Cluster) URLs() []string {
	urls := make([]string, len(c.Servers))
	for i, server := range c.Servers {
		urls[i] = server.URL()
	}
	return urls
}

// URL is every member's client URL in one, comma-separated, as nats.go takes a server list: a
// client connects to whichever member answers and reconnects to another.
func (c *Cluster) URL() string { return strings.Join(c.URLs(), ",") }

// AwaitReady waits until one running member leads the JetStream meta group, that leader counts
// every running member, and no stopped one, as an online peer, every running member is current
// with it, JetStream answers an account info request on every running member, and every running
// member's routes hold the reply wildcard that request made on every other running member
// (replyRoutesReady), for as long as the test may run (testDeadline): a cluster's JetStream start
// on a loaded machine can take longer than any fixed wait allows. A test calls it after it starts
// a stopped member again, or after it stops one, before it asks JetStream for anything that needs
// the meta group.
//
// A member that no other running member's meta group hears from within muteAfter is restarted, at
// most muteRestarts times in one call. Each restart starts the listening over for every member,
// since a restarted member has heard no one yet.
//
// nats-server 2.10.29, the server these tests linked before v2.15.1, could start a member whose
// every Raft message is dropped: its setSystemAccount started the Raft send queue's goroutine
// (server/server.go:1710), which registered its client with s.SystemAccount() (server/sendq.go:48,
// newSendQ() taking no account) before setSystemAccount stored the account (server/server.go:1717),
// and a client with no account delivers nothing (server/client.go:3882). Such a member followed the
// leader but never answered it or voted, so the leader never counted it, for as long as that start
// ran; a restart is a fresh start. Under -race on two CPUs with four test processes, 29 of 1,920
// fresh clusters had such a member. nats-server v2.15.1, the server these tests link now, hands the
// queue its account instead: newSendQ(acc) stores it and the loop registers sq.a
// (server/sendq.go:35-48), and both callers pass it (server/server.go:1898,
// server/raft_transport.go:74). The restarts stay as a guard against a member that goes mute for
// another reason.
func (c *Cluster) AwaitReady(t testing.TB) {
	t.Helper()
	c.awaitMeta(t)
	for _, server := range c.Servers {
		if server.running() {
			connectAs(t, server.URL(), c.readySeed).Close()
		}
	}
	c.awaitReplyRoutes(t)
}

// awaitReplyRoutes waits, until testDeadline, for replyRoutesReady.
func (c *Cluster) awaitReplyRoutes(t testing.TB) {
	t.Helper()
	bound := boundOf(t)
	for {
		err := c.replyRoutesReady()
		if err == nil {
			return
		}
		if bound.over() {
			t.Fatalf("the members' reply subjects were not routed by %s, short of the test's deadline: %v", bound, err)
		}
		bound.pause()
	}
}

// replyRoutesReady reports why a running member could still drop the answer to the first
// JetStream request a client sends through another running member, or nil when none can.
//
// A client's JetStream request crosses its account's import of the system account's $JS.API.>
// service, and the member it entered through gives it a reply subject under the system account's
// reply wildcard, _R_.<prefix>.>, and the answer comes back on that subject through the member's
// routes. nats-server makes the wildcard, and sends it to the member's routes, only at the first
// such request through the member (newServiceReply, server/accounts.go:2504-2522, the same in
// v2.15.3; the routes get it from updateRouteSubscriptionMap, server/route.go:2553-2723), while
// the request goes on at once. A member that hears the request from the entry member hears the
// wildcard before it on the same route, so its answer comes back: the meta leader's answer to an
// account info (server/jetstream_api.go:1461-1475) did, for the first request through each member
// of 1,000 fresh clusters. The answer to a consumer update comes from the consumer's leader once
// it applies the meta leader's Raft commit (server/jetstream_cluster.go:7840-7856 and 9045-9145
// for a one-copy consumer, 7891-7928 for a replicated one). When that leader is a third member,
// the commit reaches it on another route than the wildcard, can arrive first, and the answer is
// dropped.
// TestThePrepareOperatorRaisesThresholdsAndWritesTheCohort lost that update so in 6 of 48,000
// plain runs on two CPUs in the build cluster, and in envoy-go job 140166810178.
func (c *Cluster) replyRoutesReady() error {
	for i, server := range c.Servers {
		srv := server.natsServer()
		if srv == nil {
			continue
		}
		wildcards, err := replyWildcards(srv)
		if err != nil {
			return fmt.Errorf("member %s: %w", c.Name(i), err)
		}
		if len(wildcards) == 0 {
			return fmt.Errorf("member %s has no reply wildcard: no request has entered it yet", c.Name(i))
		}
		for j, other := range c.Servers {
			otherSrv := other.natsServer()
			if j == i || otherSrv == nil {
				continue
			}
			routed, err := routedSubjects(otherSrv)
			if err != nil {
				return fmt.Errorf("member %s: %w", c.Name(j), err)
			}
			for _, wildcard := range wildcards {
				if !routed[wildcard] {
					return fmt.Errorf("member %s has not heard member %s's reply wildcard %s", c.Name(j), c.Name(i), wildcard)
				}
			}
		}
	}
	return nil
}

// replyWildcards are the reply wildcards, _R_.<prefix>.>, of srv's system account: its own
// subscriptions, which its routes carry to the other members.
func replyWildcards(srv *natsserver.Server) ([]string, error) {
	system := srv.SystemAccount()
	if system == nil {
		return nil, errors.New("no system account")
	}
	subs, err := srv.Subsz(&natsserver.SubszOptions{Subscriptions: true, Account: system.GetName(), Limit: math.MaxInt32})
	if err != nil {
		return nil, err
	}
	var wildcards []string
	for _, sub := range subs.Subs {
		if tokens := strings.Split(sub.Subject, "."); len(tokens) == 3 && tokens[0] == "_R_" && tokens[2] == ">" {
			wildcards = append(wildcards, sub.Subject)
		}
	}
	return wildcards, nil
}

// routedSubjects are the subjects of every subscription srv's routes hold for the other members.
func routedSubjects(srv *natsserver.Server) (map[string]bool, error) {
	routes, err := srv.Routez(&natsserver.RoutezOptions{Subscriptions: true})
	if err != nil {
		return nil, err
	}
	routed := map[string]bool{}
	for _, route := range routes.Routes {
		for _, subject := range route.Subs {
			routed[subject] = true
		}
	}
	return routed, nil
}

// awaitMeta is AwaitReady's wait for the meta group, restarting a mute member as AwaitReady says.
func (c *Cluster) awaitMeta(t testing.TB) {
	t.Helper()
	bound := boundOf(t)
	// listening is when the wait began listening for the members: its call, or its last restart.
	listening := time.Now()
	restarts := 0
	for {
		err := c.metaReady()
		if err == nil {
			return
		}
		if restarts < muteRestarts {
			if i := c.mute(time.Since(listening)); i >= 0 {
				t.Logf("restart cluster member %s: no other member's JetStream meta group heard from it within %s (%v)", c.Name(i), muteAfter, err)
				c.Servers[i].stop()
				c.Servers[i].launch(t)
				restarts++
				listening = time.Now()
				continue
			}
		}
		if bound.over() {
			t.Fatalf("JetStream was not ready by %s, short of the test's deadline (%d members restarted): %v", bound, restarts, err)
		}
		bound.pause()
	}
}

const (
	// muteAfter is how long AwaitReady listens for a running member before it restarts it. A
	// working member is heard sooner whether or not the meta group has a leader: a leader hears
	// each follower answer its heartbeat every second, and without a leader every member campaigns
	// within nats-server's longest election timeout, 9 seconds, so the members it asks for a vote
	// hear it and it hears their answers. Under -race on two CPUs, most beside three more such test
	// processes, the 1,925 fresh clusters without a mute member were all ready within 7.8 seconds.
	muteAfter = 15 * time.Second
	// muteRestarts caps the restarts one AwaitReady makes, so a member nothing can hear, such as
	// the last running one of a cluster that lost its quorum, fails the wait instead of being
	// restarted for ever.
	muteRestarts = 3
	// metaGroupName is the Raft group JetStream's meta group runs as.
	metaGroupName = "_meta_"
)

// mute is the index of a running member that no other running member's meta group has heard from
// in the last listened, once that is muteAfter or longer, or -1 when there is none.
func (c *Cluster) mute(listened time.Duration) int {
	if listened < muteAfter {
		return -1
	}
	groups := make([]*natsserver.RaftzGroup, len(c.Servers))
	for i, server := range c.Servers {
		if srv := server.natsServer(); srv != nil {
			groups[i] = metaGroup(srv)
		}
	}
	for i, group := range groups {
		if group != nil && !heard(groups, i, listened) {
			return i
		}
	}
	return -1
}

// heard reports whether a member other than i last heard member i's meta group within the last
// within, by each member's own Raft view of the group (groups, nil for a member without one).
func heard(groups []*natsserver.RaftzGroup, i int, within time.Duration) bool {
	for j, group := range groups {
		if j == i || group == nil {
			continue
		}
		peer, ok := group.Peers[groups[i].ID]
		if !ok || peer.LastSeen == "" {
			continue
		}
		if seen, err := time.ParseDuration(peer.LastSeen); err == nil && seen < within {
			return true
		}
	}
	return false
}

// metaGroup is srv's Raft view of the JetStream meta group, or nil while it has none.
func metaGroup(srv *natsserver.Server) *natsserver.RaftzGroup {
	status := srv.Raftz(&natsserver.RaftzOptions{GroupFilter: metaGroupName})
	if status == nil {
		return nil
	}
	for _, groups := range *status {
		if group, ok := groups[metaGroupName]; ok {
			return &group
		}
	}
	return nil
}

// metaReady reports why the meta group is not ready yet, or nil when it is.
func (c *Cluster) metaReady() error {
	leaders, running := 0, 0
	var online []string
	for i, server := range c.Servers {
		srv := server.natsServer()
		if srv == nil {
			continue
		}
		running++
		if !srv.JetStreamIsCurrent() {
			return fmt.Errorf("member %s is not current with the meta group", c.Name(i))
		}
		if srv.JetStreamIsLeader() {
			leaders++
			online = srv.JetStreamClusterPeers()
		}
	}
	if leaders != 1 {
		return fmt.Errorf("%d running members lead the meta group, want 1", leaders)
	}
	if len(online) != running {
		return fmt.Errorf("the meta leader counts %d members online (%v), want the %d running", len(online), online, running)
	}
	return nil
}
