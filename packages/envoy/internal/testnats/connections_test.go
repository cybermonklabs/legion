package testnats

import (
	"fmt"
	"net"
	"testing"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testport"
	"github.com/sjawhar/envoy/internal/testwait"
)

// Connections names each connection the server took once, the ones its clients have closed among
// them: five clients connect in turn, the first three closing as soon as they have connected, and
// the listing read once the fifth has connected names each of the five once. The server finishes
// those closes while the later clients connect, and a single read of its Connz then can leave a
// closed one out, or name it twice (Connections says why).
func TestConnectionsNamesEachConnectionTheServerTookOnce(t *testing.T) {
	server, uri := Start(t)
	const clients, closed = 5, 3
	for i := range clients {
		conn, err := ConnectSeed(t, uri, "", natsgo.Name(fmt.Sprintf("client-%d", i)))
		if err != nil {
			t.Fatalf("connect client-%d: %v", i, err)
		}
		if i < closed {
			conn.Close()
		} else {
			t.Cleanup(conn.Close)
		}
	}
	conns := server.Connections(t)
	named := map[string]int{}
	for _, conn := range conns {
		named[conn.Name]++
	}
	if len(conns) != clients {
		t.Errorf("the server lists %d connections, want %d: %v", len(conns), clients, named)
	}
	for i := range clients {
		if n := named[fmt.Sprintf("client-%d", i)]; n != 1 {
			t.Errorf("the server's connections name client-%d %d times, want once: %v", i, n, named)
		}
	}
}

// Connz lists a server's leaf nodes, which its count of the client connections it took
// (total_connections) leaves out, so the listing is read for client connections alone: a hub
// whose one connection is a leaf node lists no client connection, settled, where one that counted
// the leaf would never settle. The hub's leaf listener is at a loopback address of its own
// (testport.LoopbackIP), as every listener of a server this package starts is.
func TestConnectionsLeavesALeafNodeOut(t *testing.T) {
	hub := start(t, fmt.Sprintf("leafnodes { listen: %q }\n", net.JoinHostPort(testport.LoopbackIP().String(), "-1")), nil)
	srv := hub.natsServer()
	varz, err := srv.Varz(nil)
	if err != nil {
		t.Fatalf("read the hub's varz: %v", err)
	}
	start(t, fmt.Sprintf("leafnodes { remotes: [{ url: \"nats-leaf://%s:%d\" }] }\n", varz.LeafNode.Host, varz.LeafNode.Port), nil)
	testwait.Until(t, "the leaf node to connect to the hub", func() (int, bool) {
		leaves := srv.NumLeafNodes()
		return leaves, leaves > 0
	})
	if conns := hub.Connections(t); len(conns) != 0 {
		t.Fatalf("a hub whose one connection is a leaf node lists %d client connections, want none: %+v", len(conns), conns)
	}
}
