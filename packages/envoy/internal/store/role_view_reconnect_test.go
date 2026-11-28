package store

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testport"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A role claim view read across a reconnect is not missing a claim and keeps none that the server
// it now reads does not hold, and nothing in the registry's open or follow path fails over it. The
// row ports the first case of main's TestARoleClaimScanThatSpansAReconnectRunsAgain (#4973), the
// morning of 2026-11-22, when the Service in front of the hub moved the listener's connection to
// another server: the forwarder in front of the client points at server A and then at server B,
// whose role buckets differ in three claims (twoRoleViewServers), and what the registry keeps must
// be B's alone. The registry is opened on the listener's own client (bus.Connect) with the
// listener's hooks on it (cmd/listener hookListenerKVWatchers): the disconnect stops the copy
// following its bucket (Registry.Disconnected), and the reconnect hook moves both watchers onto the
// reconnected connection (Registry.Rewatch), whose read of B is the one the view follows. Once the
// view holds A's claims, the forwarder switches to B; the row waits for the connection to reconnect,
// for the hook's Rewatch to return, and for the copy to follow its bucket again, and the view the
// listener reads (RolesBySession) must then be B's claims exactly. Control: with the entry sweep of
// kvwatch's Copy.CompleteRead disabled, the read of B keeps what the read of A held, and the row
// fails on a-only; with the revision fence kept across the read of B's stream (Copy.BeginRead), the
// row fails on role-00001, still held by A's session.
func TestARoleClaimViewThatSpansAReconnectToAnotherServerHoldsOnlyThatServersClaims(t *testing.T) {
	const bound = 30 * time.Second
	a, b := twoRoleViewServers(t)
	forwarder := startRoleViewForwarder(t, a.address)
	client, err := bus.Connect([]string{forwarder.url()})
	if err != nil {
		t.Fatalf("connect through the forwarder to A: %v", err)
	}
	t.Cleanup(client.Close)
	reg, err := Open(t.Context(), client.Conn, WithReplicas(1))
	if err != nil {
		t.Fatalf("Open through the forwarder to A: %v", err)
	}
	t.Cleanup(reg.StopWatch)
	rewatches := &rewatchRuns{}
	client.AddDisconnectHook(reg.Disconnected)
	client.AddReconnectHook(func(ctx context.Context, conn *natsgo.Conn) error {
		err := reg.Rewatch(ctx, conn)
		rewatches.record(err)
		return err
	})

	testwait.WithinPolling(t, bound, testwait.Poll, "the role claim view to hold A's claims", func() bool {
		holders, err := viewHolders(reg)
		return err == nil && maps.Equal(holders, a.holders)
	})
	before := client.Conn.Stats().Reconnects
	forwarder.switchTo(b.address)
	testwait.WithinPolling(t, bound, testwait.Poll, "the connection to reconnect through the forwarder and the reconnect hook's Rewatch to return", func() bool {
		return client.Conn.Stats().Reconnects > before && rewatches.count() > 0
	})
	testwait.WithinPolling(t, bound, testwait.Poll, "the role claim view to follow its bucket again", reg.roleWatcher.Following)

	if errs := rewatches.failures(); len(errs) > 0 {
		t.Fatalf("the reconnect hook's Rewatch failed %d of %d runs: %v", len(errs), rewatches.count(), errs)
	}
	if err := reg.WatchErr(); err != nil {
		t.Fatalf("a watcher of the registry ended after the reconnect to B: %v", err)
	}
	holders, err := viewHolders(reg)
	if err != nil {
		t.Fatalf("the role claim view refuses a read once it follows B's bucket: %v", err)
	}
	if holder, held := holders["a-only"]; held {
		t.Fatalf("the role claim view keeps a-only, held by %s, a claim only A holds: the read of B that followed the reconnect kept what the read of A held", holder)
	}
	requireRoleViewHolders(t, holders, b.holders, "B's")
}

// roleViewEntries is how many writes each server's role claim bucket holds.
const roleViewEntries = 2000

// rewatchRuns records what each run of the reconnect hook's Rewatch returned.
type rewatchRuns struct {
	mu   sync.Mutex
	runs int
	errs []error
}

func (r *rewatchRuns) record(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs++
	if err != nil {
		r.errs = append(r.errs, err)
	}
}

func (r *rewatchRuns) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runs
}

func (r *rewatchRuns) failures() []error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.errs)
}

// viewHolders is the role claim view the listener reads (RolesBySession) as each role's holder, or
// the view's refusal.
func viewHolders(reg *Registry) (map[string]string, error) {
	held, err := reg.RolesBySession()
	if err != nil {
		return nil, err
	}
	holders := map[string]string{}
	for session, roles := range held {
		for _, role := range roles {
			holders[role] = session
		}
	}
	return holders, nil
}

// requireRoleViewHolders fails the test unless got is want, role for role and holder for holder;
// whose names want in the failure.
func requireRoleViewHolders(t *testing.T, got, want map[string]string, whose string) {
	t.Helper()
	if maps.Equal(got, want) {
		return
	}
	var wrong []string
	for role, holder := range got {
		if wanted, held := want[role]; !held {
			wrong = append(wrong, fmt.Sprintf("%s held by %s, want no claim", role, holder))
		} else if holder != wanted {
			wrong = append(wrong, fmt.Sprintf("%s held by %s, want %s", role, holder, wanted))
		}
	}
	for role, holder := range want {
		if _, held := got[role]; !held {
			wrong = append(wrong, fmt.Sprintf("%s missing, want held by %s", role, holder))
		}
	}
	slices.Sort(wrong)
	if len(wrong) > 10 {
		wrong = append(wrong[:10], fmt.Sprintf("and %d more", len(wrong)-10))
	}
	t.Fatalf("the role claim view holds %d claims, not %s %d:\n%s", len(got), whose, len(want), strings.Join(wrong, "\n"))
}

// roleViewEntry is one write a test makes to the role claim bucket: a claim of key by holder
// (ses_<key> when holder is empty), or a delete marker of key.
type roleViewEntry struct {
	key    string
	holder string
	marker bool
}

// roleViewBucket is n writes to the role claim bucket, of keys role-00001 on, a claim and a delete
// marker by turns, so a read streams past markers among the claims as it does in the deployment.
func roleViewBucket(n int) []roleViewEntry {
	entries := make([]roleViewEntry, n)
	for i := range entries {
		entries[i] = roleViewEntry{key: fmt.Sprintf("role-%05d", i+1), marker: i%2 == 1}
	}
	return entries
}

// roleViewServer is a NATS server whose role claim bucket the row reads: its address for a
// forwarder, and the holder of each claim its bucket holds, which is what a view of it must hold.
type roleViewServer struct {
	address string
	holders map[string]string
}

// startRoleViewServer starts a NATS server with the interest and role claim buckets and writes
// entries to the role claim bucket in order. The writes are asynchronous publishes to the keys'
// subjects, a delete marker one with the KV-Operation header nats.go's Delete sends.
func startRoleViewServer(t *testing.T, entries []roleViewEntry) roleViewServer {
	t.Helper()
	_, uri := testnats.Start(t)
	conn := testnats.Connect(t, uri)
	defer conn.Close()
	js, err := conn.JetStream(natsgo.PublishAsyncMaxPending(1024))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	for _, bucket := range []string{bus.InterestBucket, bus.RoleClaimBucket} {
		if _, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bucket, Storage: natsgo.FileStorage}); err != nil {
			t.Fatalf("create the %s bucket: %v", bucket, err)
		}
	}
	futures := make([]natsgo.PubAckFuture, len(entries))
	holders := map[string]string{}
	for i, entry := range entries {
		msg := natsgo.NewMsg("$KV." + bus.RoleClaimBucket + "." + entry.key)
		if entry.marker {
			msg.Header.Set("KV-Operation", "DEL")
			delete(holders, entry.key)
		} else {
			holder := entry.holder
			if holder == "" {
				holder = "ses_" + entry.key
			}
			msg.Data = []byte(`{"holder_session_id":"` + holder + `"}`)
			holders[entry.key] = holder
		}
		if futures[i], err = js.PublishMsgAsync(msg, natsgo.StallWait(time.Minute)); err != nil {
			t.Fatalf("write entry %d of the role claim bucket: %v", i+1, err)
		}
	}
	for i, future := range futures {
		select {
		case <-future.Ok():
		case err := <-future.Err():
			t.Fatalf("write entry %d of the role claim bucket: %v", i+1, err)
		}
	}
	server, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("read the server's URL: %v", err)
	}
	return roleViewServer{address: server.Host, holders: holders}
}

// twoRoleViewServers starts servers A and B, A first, with the same role claim bucket but for three
// claims: a-only, which only A holds, written among the first thousand entries; b-only, which only
// B holds, at the same place in B's bucket; and role-00001, which A claims once more at the end of
// its bucket, naming another session. So B holds two claims A lacks, and its claim of role-00001
// is at revision 1, below A's: a view that kept A's revision fence across the switch would drop it.
func twoRoleViewServers(t *testing.T) (a, b roleViewServer) {
	t.Helper()
	entries := roleViewBucket(roleViewEntries)
	onA, onB := slices.Clone(entries), slices.Clone(entries)
	onA[500] = roleViewEntry{key: "a-only"}
	onB[500] = roleViewEntry{key: "b-only"}
	onA = append(onA, roleViewEntry{key: entries[0].key, holder: "ses_a-reclaimed"})
	a, b = startRoleViewServer(t, onA), startRoleViewServer(t, onB)
	if _, held := a.holders["a-only"]; !held {
		t.Fatal("A's bucket holds no a-only claim")
	}
	if _, held := b.holders["b-only"]; !held {
		t.Fatal("B's bucket holds no b-only claim")
	}
	if a.holders[entries[0].key] == b.holders[entries[0].key] {
		t.Fatalf("%s is held by %s on both servers; A's claim of it again did not land", entries[0].key, a.holders[entries[0].key])
	}
	return a, b
}

// roleViewForwarder is the TCP forwarder the row puts between its client and a NATS server, as the
// Service in front of the hub was on 2026-11-22. It relays each connection it accepts to the server
// it points at; switchTo points it at another server and drops every connection it relays, as the
// Service's switch reset them, so the client's reconnect reaches the other server.
type roleViewForwarder struct {
	listener net.Listener

	mu     sync.Mutex
	target string
	// generation moves on at each switch and at the end, so a connection accepted before one is
	// closed rather than joined to the server it was dialing.
	generation int
	relayed    map[net.Conn]struct{}
}

// startRoleViewForwarder starts a forwarder to target, a server's host and port, on a loopback
// address of its own (testport.ListenLoopback), until the test ends.
func startRoleViewForwarder(t *testing.T, target string) *roleViewForwarder {
	t.Helper()
	listener, err := testport.ListenLoopback()
	if err != nil {
		t.Fatalf("listen for the forwarder: %v", err)
	}
	f := &roleViewForwarder{listener: listener, target: target, relayed: map[net.Conn]struct{}{}}
	go f.serve()
	t.Cleanup(f.end)
	return f
}

// url is the NATS URL of the forwarder's address.
func (f *roleViewForwarder) url() string { return "nats://" + f.listener.Addr().String() }

// switchTo points the forwarder at target and drops every connection, so the client's reconnect
// reaches target.
func (f *roleViewForwarder) switchTo(target string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.target = target
	f.dropLocked()
}

// end stops listening and drops every connection, as the test ends.
func (f *roleViewForwarder) end() {
	_ = f.listener.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropLocked()
}

// dropLocked closes every relayed connection and moves the generation on. f.mu is held.
func (f *roleViewForwarder) dropLocked() {
	f.generation++
	for conn := range f.relayed {
		_ = conn.Close()
		delete(f.relayed, conn)
	}
}

// serve joins each connection the forwarder accepts to the server it points at.
func (f *roleViewForwarder) serve() {
	for {
		client, err := f.listener.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		target, generation := f.target, f.generation
		f.mu.Unlock()
		go f.join(client, target, generation)
	}
}

// join dials target for client and relays the two into each other, unless the forwarder has been
// switched or ended since client was accepted.
func (f *roleViewForwarder) join(client net.Conn, target string, generation int) {
	server, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		_ = client.Close()
		return
	}
	f.mu.Lock()
	if f.generation != generation {
		f.mu.Unlock()
		_ = client.Close()
		_ = server.Close()
		return
	}
	f.relayed[client] = struct{}{}
	f.relayed[server] = struct{}{}
	f.mu.Unlock()
	go f.relay(server, client)
	go f.relay(client, server)
}

// relay copies src to dst until either ends, then closes both, which ends the other direction too.
func (f *roleViewForwarder) relay(dst, src net.Conn) {
	_, _ = io.Copy(dst, src)
	_ = dst.Close()
	_ = src.Close()
	f.mu.Lock()
	delete(f.relayed, dst)
	delete(f.relayed, src)
	f.mu.Unlock()
}
