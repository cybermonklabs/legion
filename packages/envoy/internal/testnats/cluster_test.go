package testnats

import (
	"errors"
	"fmt"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testwait"
)

// StartClusterConfig gives every member the configuration it is passed: each admits a user its
// authorization holds and refuses one it does not, and its no_auth_user lets the cluster start.
func TestEveryClusterMemberTakesTheConfigItIsGiven(t *testing.T) {
	seed, public := User(t)
	stranger, _ := User(t)
	_, fallback := User(t)
	cluster := StartClusterConfig(t, 3, fmt.Sprintf("authorization {\n  users = [ {nkey: %q}, {nkey: %q} ]\n}\nno_auth_user: %q\n", public, fallback, fallback))
	for i, uri := range cluster.URLs() {
		conn, err := AwaitConnectSeed(t, uri, seed)
		if err != nil {
			t.Fatalf("member %s refused a user its authorization holds: %v", cluster.Name(i), err)
		}
		conn.Close()
		if _, err := AwaitConnectSeed(t, uri, stranger); !errors.Is(err, natsgo.ErrAuthorization) {
			t.Fatalf("member %s answered a user its authorization does not hold with %v, want an authorization violation", cluster.Name(i), err)
		}
	}
}

// A three-copy stream on the cluster keeps a leader and takes a publish with the member that led
// it stopped, and that member, started again on its own store and URLs, rejoins and catches up.
func TestAThreeCopyStreamKeepsALeaderAndTakesAPublishWithOneMemberStopped(t *testing.T) {
	cluster := StartCluster(t, 3)
	conn := connectCluster(t, cluster)
	js, err := conn.JetStream(MaxWait(t))
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	cluster.CreateStream(t, natsgo.StreamConfig{Name: "THREE", Subjects: []string{"three.>"}, Replicas: 3, Storage: natsgo.FileStorage})
	if _, err := js.Publish("three.before", []byte("before")); err != nil {
		t.Fatalf("publish with every member running: %v", err)
	}
	info := AwaitStreamLeader(t, js, "THREE", "")
	if got := Copies(info.Cluster); got != 3 {
		t.Fatalf("stream has %d copies, want 3: %+v", got, info.Cluster)
	}
	leader := cluster.Member(info.Cluster.Leader)
	if leader < 0 {
		t.Fatalf("stream leader %q is no member of the cluster", info.Cluster.Leader)
	}

	cluster.Servers[leader].Stop(t)
	cluster.AwaitReady(t)
	AwaitStreamLeader(t, js, "THREE", cluster.Name(leader))
	ack, err := js.Publish("three.after", []byte("after"))
	if err != nil {
		t.Fatalf("publish with member %s stopped: %v", cluster.Name(leader), err)
	}
	if ack.Sequence != 2 {
		t.Fatalf("publish with a member stopped stored sequence %d, want 2", ack.Sequence)
	}

	cluster.Servers[leader].Start(t)
	cluster.AwaitReady(t)
	// The restarted member has caught up once the stream again holds three settled copies.
	info = AwaitStreamCopies(t, js, "THREE", 3)
	if info.State.Msgs != 2 || info.State.LastSeq != 2 {
		t.Fatalf("stream state = %+v, want both messages", info.State)
	}
}

// connectCluster connects to every member of cluster in one server list, reconnecting to another
// member when the one it holds stops.
func connectCluster(t *testing.T, cluster *Cluster) *natsgo.Conn {
	t.Helper()
	Connect(t, cluster.URL()).Close()
	return dialCluster(t, cluster, "a connection to the cluster", func(*natsgo.Conn) bool { return true },
		natsgo.MaxReconnects(-1), natsgo.ReconnectWait(reconnectWait))
}

// dialCluster dials cluster's server list with options until a dial connects and connected holds
// of its connection, closing each other connection, and returns that one, closed at the test's
// end. A dial that fails or lands elsewhere is made again by the shared wait, as Connect's is, so
// a dial that nats.go's own two seconds ran out on costs the test time and not its result.
func dialCluster(t *testing.T, cluster *Cluster, what string, connected func(*natsgo.Conn) bool, options ...natsgo.Option) *natsgo.Conn {
	t.Helper()
	dial := testwait.Until(t, what, func() (dialAttempt, bool) {
		conn, err := natsgo.Connect(cluster.URL(), options...)
		if err != nil {
			return dialAttempt{err: err}, false
		}
		if !connected(conn) {
			name := conn.ConnectedServerName()
			conn.Close()
			return dialAttempt{err: fmt.Errorf("connected to %q", name)}, false
		}
		return dialAttempt{conn: conn}, true
	})
	t.Cleanup(dial.conn.Close)
	return dial.conn
}

// reconnectWait is nats.go's own wait between a client's reconnect attempts, shortened from its
// two seconds so a test that waits for a client to move to another member is not held by it; no
// verdict here rests on an attempt not being made within it.
const reconnectWait = 100 * time.Millisecond

// clientPlace is where a client is, its status and the member it is connected to: the reading a
// wait for its move prints when the move never comes.
type clientPlace struct {
	Status natsgo.Status
	Server string
}

// A member in lame duck closes its clients' connections over its lame-duck window, and a client
// that knows the cluster's other URLs reconnects to one of them: the move a hub's server change
// forces on every session. The client knows only the URLs it was given, since the members
// advertise none of their own.
func TestALameDuckMemberSendsItsClientsToAnotherMember(t *testing.T) {
	cluster := StartCluster(t, 3)
	// The client is on the first URL's member, as DontRandomize dials the list in order; a dial that
	// ran out on it before landing on another is made again.
	conn := dialCluster(t, cluster, fmt.Sprintf("a connection to the first URL's member %s", cluster.Name(0)),
		func(conn *natsgo.Conn) bool { return conn.ConnectedServerName() == cluster.Name(0) },
		natsgo.DontRandomize(),
		natsgo.ReconnectWait(reconnectWait),
		natsgo.MaxReconnects(-1),
	)
	if discovered := conn.DiscoveredServers(); len(discovered) != 0 {
		t.Fatalf("the members advertised %v to the client, want no URL beyond the ones it was given", discovered)
	}

	// The lame duck runs on a goroutine of its own, since the client's move is awaited meanwhile.
	down := make(chan struct{})
	go func() {
		defer close(down)
		cluster.Servers[0].LameDuck()
	}()
	testwait.Until(t, fmt.Sprintf("the client to move off %s to another member of the cluster", cluster.Name(0)), func() (clientPlace, bool) {
		place := clientPlace{Status: conn.Status(), Server: conn.ConnectedServerName()}
		return place, place.Status == natsgo.CONNECTED && cluster.Member(place.Server) > 0
	})
	testwait.Receive(t, "the lame duck of the member the client was on to finish", down)
	cluster.Servers[0].Start(t)
	cluster.AwaitReady(t)
}
