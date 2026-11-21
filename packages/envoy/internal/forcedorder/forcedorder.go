//go:build odatrace

// Package forcedorder holds the forced-order rows' servers and holds across the plan's three
// topologies, for the session consumers' rows (internal/bus), the listener durable's (cmd/listener)
// and the protocol's (consumernames), on the hooked nats-server scripts/forced-order.sh builds. It
// is built only under the odatrace tag those rows carry, which the hooked server's measurement
// hooks need (internal/testnats/hooks.go).
//
// On a cluster a consumer's state write is held on every member that stores the consumer, and every
// member restarts together, so whichever replica leads after the restart holds the late write: no
// leader needs pinning.
package forcedorder

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// Topology is where a row's stream lives: Members servers, the stream at Replicas copies.
type Topology struct {
	Name     string
	Members  int
	Replicas int
}

// Topologies are the plan's three: one copy, an R1 cluster and R3.
var Topologies = []Topology{
	{Name: "one copy", Members: 1, Replicas: 1},
	{Name: "an R1 cluster", Members: 3, Replicas: 1},
	{Name: "R3", Members: 3, Replicas: 3},
}

// Servers are a row's hooked servers in its topology.
type Servers struct {
	Topology Topology
	Servers  []*testnats.Server
	// Cluster is nil for one copy.
	Cluster *testnats.Cluster
	// URL reaches every member.
	URL   string
	Hooks *testnats.Hooks
}

// Start starts topo's servers, with the hooks armed.
func Start(t *testing.T, topo Topology) *Servers {
	t.Helper()
	s := &Servers{Topology: topo}
	if topo.Members == 1 {
		srv, uri := testnats.Start(t)
		s.Servers, s.URL = []*testnats.Server{srv}, uri
	} else {
		s.Cluster = testnats.StartCluster(t, topo.Members)
		s.Servers, s.URL = s.Cluster.Servers, s.Cluster.URL()
	}
	s.Hooks = testnats.Hook(t)
	return s
}

// Hosts are the servers that store consumer on stream: the lone server, or the members its cluster
// information names, its leader and its replicas, read on conn. A consumer created a moment ago can
// answer its info before its replica group has formed, with no cluster information or no leader,
// so Hosts reads it again until the group names a leader and every replica the topology stores,
// for up to 30 s.
func (s *Servers) Hosts(t *testing.T, conn *natsgo.Conn, stream, consumer string) []*testnats.Server {
	t.Helper()
	if s.Cluster == nil {
		return s.Servers
	}
	js, err := conn.JetStream(natsgo.MaxWait(30 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		hosts, why := s.hostsOf(js, stream, consumer)
		if why == "" {
			return hosts
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s's replica group did not form within 30 s: %s", consumer, why)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// hostsOf is one read of Hosts: the members consumer's cluster information names, or why it names
// no leader, a member the cluster does not have, or fewer members than the topology stores.
func (s *Servers) hostsOf(js natsgo.JetStreamContext, stream, consumer string) ([]*testnats.Server, string) {
	info, err := js.ConsumerInfo(stream, consumer)
	switch {
	case err != nil:
		return nil, err.Error()
	case info.Cluster == nil || info.Cluster.Leader == "":
		return nil, "its info names no leader yet"
	}
	names := []string{info.Cluster.Leader}
	for _, peer := range info.Cluster.Replicas {
		names = append(names, peer.Name)
	}
	var hosts []*testnats.Server
	for _, name := range names {
		i := s.Cluster.Member(name)
		if i < 0 {
			return nil, fmt.Sprintf("its host %q is no member of the cluster", name)
		}
		hosts = append(hosts, s.Servers[i])
	}
	if len(hosts) != s.Topology.Replicas {
		return nil, fmt.Sprintf("it names %d members, want %d", len(hosts), s.Topology.Replicas)
	}
	return hosts, ""
}

// StoredState is consumer's state on stream as srv's o.dat holds it, or nil while there is none or
// it does not decode (nats-server v2.10.29 writes o.dat in place).
func StoredState(t *testing.T, srv *testnats.Server, stream, consumer string) *natsserver.ConsumerState {
	t.Helper()
	buf, err := os.ReadFile(filepath.Join(srv.StoreDir(), "jetstream", "$G", "streams", stream, "obs", consumer, "o.dat"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read %s's o.dat: %v", consumer, err)
	}
	state, err := natsserver.MeasureDecodeConsumerState(buf)
	if err != nil {
		return nil
	}
	return state
}

// AwaitStoredFloor waits for consumer's o.dat to hold ack floor floor on every host.
func (s *Servers) AwaitStoredFloor(t *testing.T, conn *natsgo.Conn, stream, consumer string, floor uint64) {
	t.Helper()
	for _, srv := range s.Hosts(t, conn, stream, consumer) {
		deadline := time.Now().Add(30 * time.Second)
		for {
			state := StoredState(t, srv, stream, consumer)
			if state != nil && state.AckFloor.Stream == floor {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s's o.dat on %q did not reach ack floor %d: %+v", consumer, srv.Name(), floor, state)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// Store is one host's store of a consumer, by its generation there.
type Store struct {
	Server *testnats.Server
	Gen    uint64
}

// Stores are consumer's stores, one per host.
func (s *Servers) Stores(t *testing.T, conn *natsgo.Conn, stream, consumer string) []Store {
	t.Helper()
	var stores []Store
	for _, srv := range s.Hosts(t, conn, stream, consumer) {
		events := s.Hooks.Events(testnats.Point("store.loaded"), testnats.OnServer(srv), testnats.Consumer(consumer))
		if len(events) == 0 {
			t.Fatalf("the hooked server %q reported no store of %s", srv.Name(), consumer)
		}
		stores = append(stores, Store{Server: srv, Gen: events[len(events)-1].Gen})
	}
	return stores
}

// Held is one hold per store of a consumer.
type Held struct {
	Stores []Store
	holds  []*testnats.Hold
}

// HoldWrites arms, on every store of consumer, the hold at point of the state write that carries
// ack floor floor.
func (s *Servers) HoldWrites(t *testing.T, conn *natsgo.Conn, point testnats.Point, stream, consumer string, floor uint64) Held {
	t.Helper()
	held := Held{Stores: s.Stores(t, conn, stream, consumer)}
	for _, store := range held.Stores {
		held.holds = append(held.holds, s.Hooks.Hold(point, testnats.OnServer(store.Server), testnats.Consumer(consumer),
			testnats.Where(fmt.Sprintf("store %d's write of ack floor %d", store.Gen, floor), func(e testnats.Event) bool {
				return e.Gen == store.Gen && e.State != nil && e.State.AckFloor.Stream == floor
			})))
	}
	return held
}

// Reached waits for every hold, and logs each.
func (h Held) Reached(t *testing.T) {
	t.Helper()
	for _, hold := range h.holds {
		t.Logf("held %s", hold.Reached(t))
	}
}

// HoldNewer arms, on every one of stores' servers, the hold at point of a consumer store of stream
// newer than that server's store: the replacement's.
func (s *Servers) HoldNewer(point testnats.Point, stream string, stores []Store) []*testnats.Hold {
	var holds []*testnats.Hold
	for _, store := range stores {
		holds = append(holds, s.Hooks.Hold(point, testnats.OnServer(store.Server), testnats.Stream(stream),
			testnats.Where(fmt.Sprintf("a store newer than %d", store.Gen), func(e testnats.Event) bool { return e.Gen > store.Gen })))
	}
	return holds
}

// AwaitNewerWrites waits, on every one of stores' servers, for the first completed state write
// after mark of a consumer store of stream newer than that server's store: the replacement's own,
// made before the held one lands.
func (s *Servers) AwaitNewerWrites(t *testing.T, mark int, stream string, stores []Store) {
	t.Helper()
	for _, store := range stores {
		s.Hooks.Await(t, testnats.Point("write.done"), testnats.OnServer(store.Server), testnats.Stream(stream), testnats.After(mark),
			testnats.Where(fmt.Sprintf("a store newer than %d", store.Gen), func(e testnats.Event) bool { return e.Gen > store.Gen && e.Err == "" }))
	}
}

// ReleaseAndLand releases every hold of held and waits for each held write to finish.
func (s *Servers) ReleaseAndLand(t *testing.T, held Held) {
	t.Helper()
	mark := s.Hooks.Mark()
	for _, hold := range held.holds {
		hold.Release()
	}
	for _, store := range held.Stores {
		landed := s.Hooks.Await(t, testnats.Point("write.done"), testnats.OnServer(store.Server), testnats.After(mark),
			testnats.Where(fmt.Sprintf("store %d", store.Gen), func(e testnats.Event) bool { return e.Gen == store.Gen }))
		t.Logf("the held write of store %d on %q finished after its release: err %q", store.Gen, store.Server.Name(), landed.Err)
	}
}

// Restart stops every server and starts them again on their stores, waits for a cluster to be
// ready, and answers a new connection of the test's own (testnats.Connect does not reconnect) on
// which stream answers.
func (s *Servers) Restart(t *testing.T, stream string) *natsgo.Conn {
	t.Helper()
	for _, srv := range s.Servers {
		srv.Stop(t)
	}
	for _, srv := range s.Servers {
		srv.Start(t)
	}
	if s.Cluster != nil {
		s.Cluster.AwaitReady(t)
	}
	conn := testnats.Connect(t, s.URL)
	t.Cleanup(conn.Close)
	deadline := time.Now().Add(60 * time.Second)
	for {
		ready, why := s.consumersReady(conn, stream)
		if ready {
			return conn
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s's consumers did not all answer within 60 s of the restart: %s", stream, why)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// consumersReady reports whether stream answers and every one of its consumers answers its info,
// with a leader on a cluster: until then a row's read of a consumer after the restart would time
// out on a replica group still electing, and read as nothing delivered. The consumers it reads must
// number what the stream's own state counts, so a listing cut short is not read as every consumer.
// why says what is not ready.
func (s *Servers) consumersReady(conn *natsgo.Conn, stream string) (bool, string) {
	js, err := conn.JetStream(natsgo.MaxWait(5 * time.Second))
	if err != nil {
		return false, err.Error()
	}
	info, err := js.StreamInfo(stream)
	if err != nil {
		return false, "the stream: " + err.Error()
	}
	read := 0
	for name := range js.ConsumerNames(stream) {
		consumer, err := js.ConsumerInfo(stream, name)
		if err != nil {
			return false, fmt.Sprintf("consumer %s: %v", name, err)
		}
		if s.Cluster != nil && (consumer.Cluster == nil || consumer.Cluster.Leader == "") {
			return false, fmt.Sprintf("consumer %s has no leader yet", name)
		}
		read++
	}
	if read != info.State.Consumers {
		return false, fmt.Sprintf("read %d consumers of the %d the stream counts", read, info.State.Consumers)
	}
	return true, ""
}
