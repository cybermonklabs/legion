package testnats

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

// StartClusterConfig gives every member the configuration it is passed: each admits a user its
// authorization holds and refuses one it does not, and its no_auth_user lets the cluster start.
func TestEveryClusterMemberTakesTheConfigItIsGiven(t *testing.T) {
	seed, public := User(t)
	stranger, _ := User(t)
	_, fallback := User(t)
	cluster := StartClusterConfig(t, 3, fmt.Sprintf("authorization {\n  users = [ {nkey: %q}, {nkey: %q} ]\n}\nno_auth_user: %q\n", public, fallback, fallback))
	for i, uri := range cluster.URLs() {
		conn, err := ConnectSeed(t, uri, seed)
		if err != nil {
			t.Fatalf("member %s refused a user its authorization holds: %v", cluster.Name(i), err)
		}
		conn.Close()
		if _, err := ConnectSeed(t, uri, stranger); !errors.Is(err, natsgo.ErrAuthorization) {
			t.Fatalf("member %s answered a user its authorization does not hold with %v, want an authorization violation", cluster.Name(i), err)
		}
	}
}

// A three-copy stream on the cluster keeps a leader and takes a publish with the member that led
// it stopped, and that member, started again on its own store and URLs, rejoins and catches up.
func TestAThreeCopyStreamKeepsALeaderAndTakesAPublishWithOneMemberStopped(t *testing.T) {
	cluster := StartCluster(t, 3)
	conn := connectCluster(t, cluster)
	js, err := conn.JetStream(natsgo.MaxWait(jetStreamWait))
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
	conn, err := natsgo.Connect(cluster.URL(), natsgo.Timeout(dialTimeout), natsgo.MaxReconnects(-1), natsgo.ReconnectWait(retryInterval))
	if err != nil {
		t.Fatalf("connect to the cluster: %v", err)
	}
	t.Cleanup(conn.Close)
	return conn
}

// A member in lame duck closes its clients' connections over its lame-duck window, and a client
// that knows the cluster's other URLs reconnects to one of them: the move a hub's server change
// forces on every session. The client knows only the URLs it was given, since the members
// advertise none of their own.
func TestALameDuckMemberSendsItsClientsToAnotherMember(t *testing.T) {
	cluster := StartCluster(t, 3)
	conn, err := natsgo.Connect(cluster.URL(),
		natsgo.DontRandomize(),
		natsgo.Timeout(dialTimeout),
		natsgo.ReconnectWait(retryInterval),
		natsgo.MaxReconnects(-1),
	)
	if err != nil {
		t.Fatalf("connect to the cluster: %v", err)
	}
	t.Cleanup(conn.Close)
	if got := conn.ConnectedServerName(); got != cluster.Name(0) {
		t.Fatalf("the client is connected to %q, want the first URL's member %q", got, cluster.Name(0))
	}
	if discovered := conn.DiscoveredServers(); len(discovered) != 0 {
		t.Fatalf("the members advertised %v to the client, want no URL beyond the ones it was given", discovered)
	}

	if err := cluster.Servers[0].LameDuck(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(connectTimeout)
	for !conn.IsConnected() || cluster.Member(conn.ConnectedServerName()) <= 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the client did not move to another member within %s of the lame duck: status %s, connected to %q", connectTimeout, conn.Status(), conn.ConnectedServerName())
		}
		time.Sleep(retryInterval)
	}
	cluster.Servers[0].Start(t)
	cluster.AwaitReady(t)
}

// The cluster is not ready while a member has made no reply wildcard, nor while another member's
// routes do not hold one it made, and is ready once AwaitReady has run: replyRoutesReady reads the
// members' own subscriptions and their routes', so a request through any member can be answered
// by any other. The routes are frozen while the first request through member n1 makes its
// wildcard, so the other members cannot have heard it.
func TestTheClusterIsNotReadyUntilEveryMembersReplyWildcardIsRouted(t *testing.T) {
	cluster := startCluster(t, 3, "")
	cluster.awaitMeta(t)
	if err := cluster.replyRoutesReady(); err == nil || !strings.Contains(err.Error(), "member n1 has no reply wildcard") {
		t.Fatalf("before any request entered a member, replyRoutesReady = %v, want member n1 to have no reply wildcard", err)
	}

	thaw := cluster.FreezeRoutes(t)
	defer thaw()
	conn, err := ConnectSeed(t, cluster.Servers[0].URL(), "")
	if err != nil {
		t.Fatalf("connect to member n1: %v", err)
	}
	defer conn.Close()
	// The answer is beside the point (the meta leader may be another member, which the frozen
	// routes keep the request from): the request's crossing of the JetStream import makes n1's
	// wildcard, and the flush returns once n1 has read it.
	if err := conn.PublishRequest("$JS.API.INFO", natsgo.NewInbox(), nil); err != nil {
		t.Fatalf("send the request through member n1: %v", err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the request through member n1: %v", err)
	}
	if err := cluster.replyRoutesReady(); err == nil || !strings.Contains(err.Error(), "member n2 has not heard member n1's reply wildcard _R_.") {
		t.Fatalf("with the routes frozen after n1 made its wildcard, replyRoutesReady = %v, want member n2 not to have heard it", err)
	}

	thaw()
	cluster.AwaitReady(t)
	if err := cluster.replyRoutesReady(); err != nil {
		t.Fatalf("after AwaitReady, replyRoutesReady = %v, want nil", err)
	}
}
