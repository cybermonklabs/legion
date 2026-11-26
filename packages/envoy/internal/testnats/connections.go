package testnats

import (
	"fmt"
	"math"
	"testing"

	natsserver "github.com/nats-io/nats-server/v2/server"

	"github.com/sjawhar/envoy/internal/testwait"
)

// clientKind is the kind nats-server's connection listings give a client connection (ConnInfo.Kind,
// kindStringMap in nats-server v2.15.1 client.go), as against a leaf node's, which they list too.
const clientKind = "Client"

// Connections is every client connection the server has taken since it last started, open and
// closed, each once, in the order it took them: the server's own record of its connections, which
// its monitoring endpoint lists at /connz?state=all, read once that record has settled.
//
// One read of that listing is no such record just after a client closed a connection. nats-server
// v2.15.1 takes a closing connection off its list of open ones (closeConnection, client.go 6335)
// while a goroutine of its own puts it on its list of closed ones (the saveClosedClient that
// markConnAsClosed starts, client.go 2144, server.go 3660), and nothing orders the two: a read
// between them finds the connection on neither list, or, the other way round, on both. The count
// the server keeps of the connections it took is never out of step so: it counts a client
// connection as it accepts it, before it reads the connection's CONNECT (createClientEx, server.go
// 3344), and reports the count as varz's total_connections. So Connections reads the two lists
// until they hold each connection of that count exactly once, the count the same before the read
// and after it. Its one bound is the test binary's deadline less testwait's margin
// (testwait.Until), a guard against a record that never settles; its failure names how many
// connections the server took and how many it listed. The list of closed connections is a ring of
// the server's last 10,000 (MaxClosedClients), so a test that makes more than that cannot read its
// whole record here, and a server whose connections keep being made and closed has no settled
// record at all: read it where the test's own connections are at rest. Each entry is the server's
// own; a caller reads it and changes nothing.
func (s *Server) Connections(t *testing.T) []*natsserver.ConnInfo {
	t.Helper()
	srv := s.natsServer()
	if srv == nil {
		t.Fatalf("testnats: the server at %s is stopped, and keeps no record of its connections until it starts again", s.url)
	}
	what := fmt.Sprintf("the server at %s to list each client connection it took exactly once, open or closed", s.url)
	return testwait.Until(t, what, func() (connectionRecord, bool) {
		taken := takenConnections(t, srv)
		connz, err := srv.Connz(&natsserver.ConnzOptions{State: natsserver.ConnAll, Limit: math.MaxInt})
		if err != nil {
			t.Fatalf("testnats: list the connections of the server at %s: %v", s.url, err)
		}
		record := connectionRecord{taken: taken, listed: map[uint64]int{}}
		for _, connection := range connz.Conns {
			if connection.Kind != clientKind {
				continue
			}
			record.listed[connection.Cid]++
			if record.listed[connection.Cid] == 1 {
				record.connections = append(record.connections, connection)
			}
		}
		settled := uint64(len(record.connections)) == taken && len(record.twice()) == 0 &&
			takenConnections(t, srv) == taken
		return record, settled
	}).connections
}

// takenConnections is how many client connections srv has accepted since it started (varz's
// total_connections): each connection counts once, from the moment the server accepted it.
func takenConnections(t *testing.T, srv *natsserver.Server) uint64 {
	t.Helper()
	varz, err := srv.Varz(nil)
	if err != nil {
		t.Fatalf("testnats: read the server's varz: %v", err)
	}
	return varz.TotalConnections
}

// connectionRecord is one read of a server's connection listings: the client connections it
// listed, each once in the order it took them, how many times it listed each, and how many it had
// taken when the read began.
type connectionRecord struct {
	taken       uint64
	listed      map[uint64]int
	connections []*natsserver.ConnInfo
}

// twice is the client id of each connection the read listed more than once.
func (r connectionRecord) twice() []uint64 {
	var ids []uint64
	for _, connection := range r.connections {
		if r.listed[connection.Cid] > 1 {
			ids = append(ids, connection.Cid)
		}
	}
	return ids
}

// String is what a wait that gave up prints of its last read.
func (r connectionRecord) String() string {
	return fmt.Sprintf("the server had taken %d client connections and listed %d, %v of them twice", r.taken, len(r.connections), r.twice())
}
