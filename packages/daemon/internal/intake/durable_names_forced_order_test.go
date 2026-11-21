//go:build odatrace

package intake

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/testnatsinprocess"

	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The daemon's race regressions on the hooked nats-server (packages/envoy/scripts/forced-order.sh
// -C packages/daemon ./internal/intake/), on the in-process server testnats.StartInProcess runs.
// Each holds the server at one point and so forces an order the server's scheduling only sometimes
// produces, then checks what the daemon's next boot makes of it. Each runs its control first: the
// same order on fixed names, where it loses messages or fails the boot, so the hold is shown to
// reach what it is meant to. On main's code the treatment is that control, and fails.

// collisionOrder is one order of family 1 at the daemon's creation site, a missing current made
// again: the predecessor's state write is held at predecessor, the predecessor is deleted, and the
// daemon's next boot creates the replacement; when replacement is set, the replacement's own store
// is held there too until the predecessor's write has been released and has finished. control is
// what the order does to the fixed name made again on one copy and on an R1 cluster, and controlR3
// on R3, where the copy the write was held on is a follower made leader after the restart.
type collisionOrder struct {
	name        string
	predecessor testnatsinprocess.Point
	replacement testnatsinprocess.Point
	control     controlOutcome
	controlR3   controlOutcome
}

// controlOutcome is what an order does to the fixed name made again.
type controlOutcome int

const (
	// loses: the predecessor's floor 21 lands in the replacement's state, and after the restart
	// it delivers 22 to 30 of the 20 to 30 it was created at.
	loses controlOutcome = iota
	// refuses: the late write takes the replacement's o.dat.tmp, its create fails with 10012,
	// and the boot with it.
	refuses
	// delivers: the late write lands nothing the replacement serves from, and it delivers 20 to
	// 30. The order is a compatibility row where its control delivers.
	delivers
	// losesOrDelivers: either of the two, as the server's scheduling falls; any other delivery,
	// or a failed boot, fails the control.
	losesOrDelivers
)

var collisionOrders = []collisionOrder{
	// A compatibility row on R3: the follower's late write finishes with no error, and the
	// follower, made leader after the restart, has delivered 20 to 30 in one run and 22 to 30 in
	// another (hooked v2.15.1, fo4 and fo5 of the build-t5 report), so it is no race regression
	// there, and its treatment must still deliver 21 to 30.
	{name: "the predecessor's write held before its IO, released after the replacement's create", predecessor: testnatsinprocess.WritePreIO, control: loses, controlR3: losesOrDelivers},
	// Held before its rename, the predecessor's write has its o.dat.tmp in the directory the delete
	// removes, so its late rename fails and lands nothing, whichever name the replacement has: the
	// order is a compatibility row everywhere, both names delivering from their own start.
	{name: "the predecessor's write held before its rename, released after the replacement's create", predecessor: testnatsinprocess.WritePreRename, control: delivers, controlR3: delivers},
	{name: "the predecessor's write released before the replacement loads its state", predecessor: testnatsinprocess.WritePreIO, replacement: testnatsinprocess.StoreCreate, control: loses, controlR3: loses},
	// On R3 the boot opens and the consumer delivers from 20 (seen on the hooked v2.15.1): a
	// compatibility row there.
	{name: "the create-error order: the predecessor's write released while the replacement's waits to rename", predecessor: testnatsinprocess.WritePreIO, replacement: testnatsinprocess.WritePreRename, control: refuses, controlR3: delivers},
}

// collision is what one run of an order ended with: the third boot's error, and, when it opened,
// the stream sequences the wake consumer it selected delivers after a restart of the server.
type collision struct {
	boot      error
	delivered []uint64
}

// writeDone is the hooked server's report of a consumer state write that has finished.
const writeDone testnatsinprocess.Point = "write.done"

// ackFloorOf takes the events carrying a consumer state whose ack floor is floor.
func ackFloorOf(floor uint64) testnatsinprocess.Match {
	return testnatsinprocess.Where(fmt.Sprintf("ack floor %d", floor), func(e testnatsinprocess.Event) bool {
		return e.State != nil && e.State.AckFloor.Stream == floor
	})
}

// storeGeneration takes the events of the consumer file store of generation gen.
func storeGeneration(gen uint64) testnatsinprocess.Match {
	return testnatsinprocess.Where(fmt.Sprintf("store generation %d", gen), func(e testnatsinprocess.Event) bool { return e.Gen == gen })
}

// inProcessJetStream is a client of the in-process server at url, named for t and closed when t
// ends. It is no client of the package's shared server, so the shared server's leak check does
// not hold it.
func inProcessJetStream(t *testing.T, url string) jetstream.JetStream {
	t.Helper()
	conn, err := nats.Connect(url, nats.Name(t.Name()), nats.MaxReconnects(-1), nats.ReconnectWait(50*time.Millisecond))
	if err != nil {
		t.Fatalf("connect to the in-process server: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("JetStream on the in-process server: %v", err)
	}
	return js
}

// ackThrough fetches name's next messages up to stream sequence through, acknowledging each and
// waiting for the server to take each acknowledgement.
func ackThrough(t *testing.T, stream jetstream.Stream, name string, through uint64) {
	t.Helper()
	consumer, err := stream.Consumer(context.Background(), name)
	if err != nil {
		t.Fatalf("bind %s: %v", name, err)
	}
	for {
		batch, err := consumer.Fetch(1, jetstream.FetchMaxWait(5*time.Second))
		if err != nil {
			t.Fatalf("fetch from %s: %v", name, err)
		}
		var last uint64
		for msg := range batch.Messages() {
			metadata, err := msg.Metadata()
			if err != nil {
				t.Fatalf("read a message's metadata: %v", err)
			}
			if err := msg.DoubleAck(context.Background()); err != nil {
				t.Fatalf("ack stream sequence %d: %v", metadata.Sequence.Stream, err)
			}
			last = metadata.Sequence.Stream
		}
		if err := batch.Error(); err != nil {
			t.Fatalf("fetch from %s: %v", name, err)
		}
		if last == 0 {
			t.Fatalf("%s delivered nothing before stream sequence %d", name, through)
		}
		if last >= through {
			return
		}
	}
}

// topology is where a collision runs: one in-process server (servers 0), or a cluster of servers
// whose notification stream, and so the daemon's consumers, keep replicas copies. On R3 the held
// write is a follower's, and that follower is made the replacement's leader after the restart, so
// the copy the late write reached is the one that serves.
type topology struct {
	name     string
	servers  int
	replicas int
}

var topologies = []topology{{name: "one copy"}, {name: "an R1 cluster", servers: 3, replicas: 1}, {name: "R3", servers: 3, replicas: 3}}

// collide runs order on where with the daemon on spec, fresh saying whether spec names fresh
// names: the wake consumer A of a project acknowledges 1 to 20 of 30 messages, with the daemon's
// checkpoint at 20, as the daemon leaves it after applying message 20; the daemon boots again; A's
// state write carrying floor 21 is held at order.predecessor on one copy of A; A is deleted outside
// the daemon; the daemon boots a third time, which makes the wake consumer again, A's write is
// released and finishes, the servers restart on their stores, and the copy the write was held on
// serves the consumer made again.
func collide(t *testing.T, where topology, order collisionOrder, specFor func(jetstream.JetStream) ConsumerSpec, fresh bool) collision {
	t.Helper()
	for _, point := range []testnatsinprocess.Point{order.predecessor, order.replacement} {
		if point == "" {
			continue
		}
		if ok, why := point.Exists(); !ok {
			t.Skipf("the linked nats-server has no %s: %s", point, why)
		}
	}
	var (
		url     string
		servers []*testnatsinprocess.Server
		restart func()
	)
	if where.servers == 0 {
		server, serverURL := testnats.StartInProcess(t)
		url, servers = serverURL, []*testnatsinprocess.Server{server}
		restart = func() { server.Stop(t); server.Start(t) }
	} else {
		cluster := testnatsinprocess.StartCluster(t, where.servers)
		url, servers = cluster.URL(), cluster.Servers
		restart = func() {
			for _, server := range cluster.Servers {
				server.Stop(t)
			}
			for _, server := range cluster.Servers {
				server.Start(t)
			}
			cluster.AwaitReady(t)
		}
	}
	hooks := testnatsinprocess.Hook(t)
	js := inProcessJetStream(t, url)
	stream, _ := identifiedStreamOf(t, js, where.replicas)
	pool := migratedPool(t)
	if fresh {
		cutover(t, pool, consumernames.CutoverDone)
	}
	spec := specFor(js)
	wake := wakeConsumerName(spec.Project)
	current := func(pool *pgxpool.Pool) string {
		if fresh {
			return currentName(t, pool, wake)
		}
		return wake
	}
	ctx := context.Background()

	openIntake(t, js, pool, spec)
	publishWakes(t, js, 30)
	predecessor := current(pool)
	ackThrough(t, stream, predecessor, 20)
	applied, err := stream.GetMsg(ctx, 20)
	if err != nil {
		t.Fatalf("read message 20: %v", err)
	}
	if _, err := pool.Exec(ctx, `insert into consumer_positions (consumer, stream_seq, stream_time) values ($1, 20, $2)`,
		wake, applied.Time.Truncate(time.Microsecond)); err != nil {
		t.Fatalf("keep the checkpoint of message 20: %v", err)
	}
	openIntake(t, js, pool, spec)

	target := copyOf(t, servers, heldInfo(t, stream, predecessor))
	held := hooks.Hold(order.predecessor, testnatsinprocess.OnServer(target), testnatsinprocess.Consumer(predecessor), ackFloorOf(21))
	ackThrough(t, stream, predecessor, 21)
	parked := held.Reached(t)
	t.Logf("held %s", parked)
	if err := stream.DeleteConsumer(ctx, predecessor); err != nil {
		t.Fatalf("delete %s outside the daemon: %v", predecessor, err)
	}
	var replacement *testnatsinprocess.Hold
	if order.replacement != "" {
		replacement = hooks.Hold(order.replacement, testnatsinprocess.OnServer(target),
			testnatsinprocess.Where("a later store of "+wake, func(e testnatsinprocess.Event) bool {
				return consumernames.LogicalOf(e.Consumer) == wake && e.Gen > parked.Gen
			}))
	}
	mark := hooks.Mark()
	booted := make(chan error, 1)
	go func() {
		boot, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		consumers, err := OpenConsumers(boot, js, pool, spec)
		consumers.Close()
		booted <- err
	}()
	var result collision
	if replacement != nil {
		t.Logf("held %s", replacement.Reached(t))
		held.Release()
		t.Logf("finished %s", hooks.Await(t, writeDone, testnatsinprocess.Consumer(predecessor), storeGeneration(parked.Gen), testnatsinprocess.After(mark)))
		replacement.Release()
		result.boot = <-booted
	} else {
		result.boot = <-booted
		held.Release()
		t.Logf("finished %s", hooks.Await(t, writeDone, testnatsinprocess.Consumer(predecessor), storeGeneration(parked.Gen), testnatsinprocess.After(mark)))
	}
	if result.boot != nil {
		return result
	}
	selected := current(pool)
	restart()
	restarted := inProcessJetStream(t, url)
	var after jetstream.Stream
	testwait.Eventually(t, notificationStream+" answering after the restart", func() bool {
		read, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		var err error
		after, err = restarted.Stream(read, notificationStream)
		return err == nil
	})
	testwait.Eventually(t, selected+" answering after the restart", func() bool {
		read, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_, err := after.Consumer(read, selected)
		return err == nil
	})
	if where.replicas > 1 {
		leadOn(t, restarted.Conn(), after, selected, target.Name())
	}
	result.delivered = fetchSeqs(t, after, selected)
	return result
}

// copyOf is the server holding the copy of a consumer whose write a collision holds: the one
// server, the server of an R1 consumer, or a follower of an R3 one.
func copyOf(t *testing.T, servers []*testnatsinprocess.Server, info *jetstream.ConsumerInfo) *testnatsinprocess.Server {
	t.Helper()
	if len(servers) == 1 {
		return servers[0]
	}
	if info == nil || info.Cluster == nil {
		t.Fatalf("the consumer reports no cluster: %+v", info)
	}
	name := info.Cluster.Leader
	if len(info.Cluster.Replicas) > 0 {
		name = info.Cluster.Replicas[0].Name
	}
	for _, server := range servers {
		if server.Name() == name {
			return server
		}
	}
	t.Fatalf("no server of the cluster is %s", name)
	return nil
}

// leadOn moves the leadership of consumer name to the server named server, stepping its leader
// down with that server preferred until it leads.
func leadOn(t *testing.T, conn *nats.Conn, stream jetstream.Stream, name, server string) {
	t.Helper()
	subject := "$JS.API.CONSUMER.LEADER.STEPDOWN." + notificationStream + "." + name
	body := []byte(`{"placement":{"preferred":"` + server + `"}}`)
	testwait.Eventually(t, name+" led by "+server, func() bool {
		consumer, err := stream.Consumer(context.Background(), name)
		if err != nil {
			return false
		}
		info := consumer.CachedInfo()
		if info.Cluster != nil && info.Cluster.Leader == server {
			return true
		}
		_, _ = conn.Request(subject, body, 2*time.Second)
		return false
	})
}

// Family 1 at the daemon's missing-current site, in each order, on one copy, an R1 cluster and R3:
// on fixed names the predecessor's late state write lands in the state of the consumer made again
// under its name, so after the restart it delivers from past the predecessor's floor and loses the
// messages it was created to deliver (the control, M4), or, in the create-error order, the create
// fails and the boot with it; an order whose control delivers is a compatibility row there
// (collisionOrder). On fresh names the consumer made again is another name, created at the
// predecessor's last floor + 1, and after the restart every message from there arrives.
func TestForcedOrderAMissingCurrentIsMadeAgainOutOfReachOfItsPredecessorsLateWrite(t *testing.T) {
	for _, where := range topologies {
		t.Run(where.name, func(t *testing.T) {
			for _, order := range collisionOrders {
				t.Run(order.name, func(t *testing.T) {
					t.Run("control: the fixed name made again", func(t *testing.T) {
						got := collide(t, where, order, func(jetstream.JetStream) ConsumerSpec { return consumerSpec(&lockedBuffer{}) }, false)
						want := order.control
						if where.replicas > 1 {
							want = order.controlR3
						}
						switch {
						case want == refuses:
							if got.boot == nil || !strings.Contains(got.boot.Error(), "10012") {
								t.Fatalf("the boot in the create-error order = %v, delivered %v; want the create refused with 10012", got.boot, got.delivered)
							}
						case got.boot != nil:
							t.Fatalf("the third boot on fixed names: %v", got.boot)
						case want == loses && !slices.Equal(got.delivered, seqRange(22, 30)):
							t.Fatalf("the fixed name made again at message 20 delivered %v after the restart, want 22 to 30: the predecessor's floor 21 in its state", got.delivered)
						case want == delivers && !slices.Equal(got.delivered, seqRange(20, 30)):
							t.Fatalf("the fixed name made again at message 20 delivered %v after the restart, want 20 to 30: the late write lands nothing it serves from", got.delivered)
						case want == losesOrDelivers && !slices.Equal(got.delivered, seqRange(22, 30)) && !slices.Equal(got.delivered, seqRange(20, 30)):
							t.Fatalf("the fixed name made again at message 20 delivered %v after the restart, want 22 to 30 or 20 to 30", got.delivered)
						}
					})
					t.Run("treatment: a fresh name made again", func(t *testing.T) {
						got := collide(t, where, order, func(js jetstream.JetStream) ConsumerSpec { return freshSpec(t, &lockedBuffer{}, js) }, true)
						if got.boot != nil {
							t.Fatalf("the third boot: %v, want the wake consumer made again", got.boot)
						}
						if want := seqRange(21, 30); !slices.Equal(got.delivered, want) {
							t.Fatalf("the wake consumer made again delivered %v after the restart, want %v: every message from the predecessor's last floor + 1", got.delivered, want)
						}
					})
				})
			}
		})
	}
}

// Family 2, a paused creator: a boot's create of its wake consumer's fresh name is held before the
// server serves it, so the boot never hears an answer and stops with the operation open. The next
// boot finds the candidate nowhere, retires it once its reservation is two seconds old, and creates
// another; the held create is then served and the candidate lands, late. A third boot keeps the
// second boot's consumer, the candidate is never selected, no name is created twice, and the
// operator's status lists the candidate as retired and cleanup deletes it. On main the hold is
// never reached: main creates the fixed name, which is no fresh name.
func TestForcedOrderACreateNobodyAnsweredIsRetiredAndNeverSelected(t *testing.T) {
	server, url := testnats.StartInProcess(t)
	hooks := testnatsinprocess.Hook(t)
	js := inProcessJetStream(t, url)
	stream, _ := identifiedStream(t, js)
	pool := migratedPool(t)
	cutover(t, pool, consumernames.CutoverDone)
	logs := &lockedBuffer{}
	spec := freshSpec(t, logs, js)
	wake := wakeConsumerName(spec.Project)
	calls := watchConsumerCalls(t, js.Conn())
	createPrefix := "$JS.API.CONSUMER.CREATE." + notificationStream + "." + wake + "~"

	held := hooks.Hold(testnatsinprocess.APIWorker, testnatsinprocess.OnServer(server),
		testnatsinprocess.Where("the create of a fresh name of "+wake, func(e testnatsinprocess.Event) bool { return strings.HasPrefix(e.Subject, createPrefix) }))
	paused, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() {
		_, err := OpenConsumers(paused, js, pool, spec)
		stopped <- err
	}()
	parked := held.Reached(t)
	cancel()
	if err := <-stopped; err == nil {
		t.Fatal("the boot whose create was held opened, want it stopped with no answer")
	} else {
		t.Logf("the paused boot stopped: %v", err)
	}
	candidate := strings.Split(strings.TrimPrefix(parked.Subject, "$JS.API.CONSUMER.CREATE."+notificationStream+"."), ".")[0]
	if op := selectionOf(t, pool, wake).Op; op == nil || op.Kind != consumernames.OpCreate || op.Candidate != candidate {
		t.Fatalf("the stopped boot left %+v, want the create of %s open", op, candidate)
	}
	// The create was published on the candidate's own connection, named for its generation, which a
	// grant held to the daemon's projects admits that one consumer on.
	published := hooks.Events(testnatsinprocess.ClientPublish, testnatsinprocess.Subject(parked.Subject))
	if len(published) == 0 {
		t.Fatalf("no publish of %s was reported", parked.Subject)
	}
	for _, event := range published {
		if event.Client != GenerationConnection(candidate) {
			t.Fatalf("the create of %s was published on the connection %q, want %q", candidate, event.Client, GenerationConnection(candidate))
		}
	}

	openIntake(t, js, pool, spec)
	settled := selectionOf(t, pool, wake)
	if settled.Current == candidate || !isFresh(wake, settled.Current) || !slices.Contains(settled.Retired, candidate) {
		t.Fatalf("the next boot left %+v, want %s retired and another fresh name selected", settled, candidate)
	}
	held.Release()
	testwait.Eventually(t, "the held create of "+candidate+" served", func() bool {
		_, err := stream.Consumer(context.Background(), candidate)
		return err == nil
	})

	openIntake(t, js, pool, spec)
	if again := selectionOf(t, pool, wake).Current; again != settled.Current {
		t.Fatalf("the third boot selected %s, want %s kept: the late candidate is never selected", again, settled.Current)
	}
	calls.noNameCreatedTwice(t, spec.Project)

	listed, err := consumernames.NewPGStore(pool).List(context.Background())
	if err != nil {
		t.Fatalf("status: list the daemon's records: %v", err)
	}
	if !slices.Contains(listed[wake].Retired, candidate) {
		t.Fatalf("status lists %s as %+v, want %s retired", wake, listed[wake], candidate)
	}
	cleanup := consumernames.New(consumernames.Config{
		Store: consumernames.NewPGStore(pool), Server: operatorServer{stream},
		Prefix: func(logical string) string { return logical }, Owner: "the operator's cleanup",
		Floor:    func(info *jetstream.ConsumerInfo) uint64 { return info.AckFloor.Stream },
		StreamID: func(context.Context) (string, error) { return "", nil },
	})
	if deleted, err := cleanup.Cleanup(context.Background(), wake); err != nil || deleted != 1 {
		t.Fatalf("cleanup = %d, %v; want the late candidate deleted", deleted, err)
	}
	if _, err := stream.Consumer(context.Background(), candidate); !errors.Is(err, jetstream.ErrConsumerNotFound) {
		t.Fatalf("%s after cleanup: %v, want it gone", candidate, err)
	}
}
