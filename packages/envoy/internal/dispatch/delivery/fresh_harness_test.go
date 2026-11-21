package delivery

// The rig of the fresh consumer names' rows (fresh_names_test.go, fresh_names_forced_order_test.go):
// a hub whose connections the auth service admits, as a deployed hub's are, so every take-over's
// calls go through the grant its connection's name earns (natsgrant.ParseDispatchConnection); and
// envoy-dispatch processes of one durable each, in either naming mode, over one Postgres. The rows
// reach the code under test only through the helpers here and the durable kinds, so the same rows
// run against the build before the fresh mode with a copy of this file whose helpers start that
// build's take-over: a race regression must fail there.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/cipher/natscallout"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// namingMode is a process's ENVOY_CONSUMER_NAMES, as the rows name it.
type namingMode string

const (
	fixedNames namingMode = "fixed"
	freshNames namingMode = "fresh"
)

// freshMachineID is the machine id the auth service grants Dispatch's connections for.
const freshMachineID = "example-dispatch"

// dispatchAccount is the platform account the rig's Dispatch connections present.
const dispatchAccount = "system:serviceaccount:dispatch:dispatch"

// freshHub is the NATS a row runs on: a lone nats-server whose connections the auth service
// admits, with an admin user beside them that may publish and subscribe anything and the files
// Dispatch's processes connect with; or a cluster of three, whose clients need no credential and
// whose stream holds replicas copies. Only the lone hub grants by connection name, so only its rows
// show the grants; the cluster's show the protocol on a clustered stream.
type freshHub struct {
	server    *testnats.Server
	cluster   *testnats.Cluster
	replicas  int
	url       string
	keys      testnats.CalloutKeys
	adminSeed string
	dir       string
	tokenFile string

	mu      sync.Mutex
	service *natscallout.Service
}

// startFreshHub runs a lone hub and its auth service until the test ends.
func startFreshHub(t *testing.T) *freshHub {
	t.Helper()
	keys := testnats.NewCalloutKeys(t)
	adminSeed, adminKey := testnats.User(t)
	server, url := testnats.StartWithCalloutServer(t, keys, testnats.StaticUser{NKey: adminKey, Publish: []string{">"}, Subscribe: []string{">"}})
	hub := &freshHub{server: server, replicas: 1, url: url, keys: keys, adminSeed: adminSeed, dir: t.TempDir()}
	hub.tokenFile = hub.write(t, "dispatch.token", "dispatch-token")
	hub.startService(t)
	return hub
}

// startClusterHub runs a cluster of three until the test ends, its stream at replicas copies.
func startClusterHub(t *testing.T, replicas int) *freshHub {
	t.Helper()
	cluster := testnats.StartCluster(t, 3)
	return &freshHub{cluster: cluster, replicas: replicas, url: cluster.URL()}
}

// hubTopology is one of the three a §7 row runs on.
type hubTopology struct {
	name  string
	start func(t *testing.T) *freshHub
}

// hubTopologies are one copy on a lone hub through the auth service, an R1 cluster and R3.
var hubTopologies = []hubTopology{
	{"one copy", startFreshHub},
	{"R1 cluster", func(t *testing.T) *freshHub { return startClusterHub(t, 1) }},
	{"R3 cluster", func(t *testing.T) *freshHub { return startClusterHub(t, 3) }},
}

func (h *freshHub) write(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(h.dir, name)
	if err := os.WriteFile(path, []byte(content+"\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// startService starts the auth service, which admits Dispatch's account and decides each
// connection's grant by its name, and waits until it is subscribed.
func (h *freshHub) startService(t *testing.T) {
	t.Helper()
	settings := natscallout.Settings{
		URLs:           []string{h.url},
		SeedFile:       h.write(t, "service.seed", h.keys.ServiceSeed),
		IssuerSeedFile: h.write(t, "issuer.seed", h.keys.IssuerSeed),
		XKeySeedFile:   h.write(t, "xkey.seed", h.keys.XKeySeed),
		Services:       []natscallout.ServiceEntry{{Kind: natscallout.KindDispatch, MachineID: freshMachineID, Account: dispatchAccount, AllProjects: true}},
	}
	deps := natscallout.Deps{ServiceToken: func(_ context.Context, raw string) (string, bool, time.Time, time.Time, error) {
		if raw != "dispatch-token" {
			return "", false, time.Time{}, time.Time{}, errors.New("the token did not verify")
		}
		now := time.Now()
		return dispatchAccount, false, now, now.Add(time.Hour), nil
	}}
	service, err := natscallout.Start(context.Background(), settings, deps)
	if err != nil {
		t.Fatalf("start the auth service: %v", err)
	}
	t.Cleanup(service.Close)
	ready, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := service.Ready(ready); err != nil {
		t.Fatalf("the auth service's subscription never reached the hub: %v", err)
	}
	h.mu.Lock()
	h.service = service
	h.mu.Unlock()
}

// stopService stops the auth service: from then on the hub admits no new connection, as in an
// outage of the service, and those it admitted stay.
func (h *freshHub) stopService() {
	h.mu.Lock()
	service := h.service
	h.mu.Unlock()
	service.Close()
}

// connect opens a connection of Dispatch's as envoy-dispatch does (cmd/dispatch serverBus): on the
// lone hub its service account's token, its name, connecting in the background; on a cluster, no
// credential.
func (h *freshHub) connect(name string) (*bus.Client, error) {
	if h.cluster != nil {
		return bus.Connect(h.cluster.URLs(), bus.WithName(name), bus.WithConnectInBackground())
	}
	token := h.tokenFile
	return bus.Connect([]string{h.url},
		bus.WithEnvironment(func(key string) (string, bool) {
			if key == "NATS_SERVICE_TOKEN_FILE" {
				return token, true
			}
			return "", false
		}),
		bus.WithName(name), bus.WithConnectInBackground())
}

// admin is a connection of the admin user, closed when the test ends.
func (h *freshHub) admin(t *testing.T) (*natsgo.Conn, natsgo.JetStreamContext) {
	t.Helper()
	var conn *natsgo.Conn
	if h.cluster != nil {
		conn = testnats.Connect(t, h.url)
	} else {
		var err error
		if conn, err = testnats.ConnectSeed(t, h.url, h.adminSeed); err != nil {
			t.Fatalf("connect the admin: %v", err)
		}
	}
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(natsgo.MaxWait(10 * time.Second))
	if err != nil {
		t.Fatalf("open the admin's JetStream: %v", err)
	}
	return conn, js
}

// createStream creates ENVOY_NOTIFICATIONS as a deployment of the hub's copies holds it, with at
// most maxConsumers consumers (0: the server's default).
func (h *freshHub) createStream(t *testing.T, maxConsumers int) {
	t.Helper()
	config := bustest.DeployedThreeCopyStream(func(c *natsgo.StreamConfig) {
		c.Replicas, c.MaxBytes, c.MaxConsumers = h.replicas, 0, maxConsumers
	})
	if h.cluster != nil {
		h.cluster.CreateStream(t, config)
		testnats.AwaitStreamCopies(t, h.cluster.JetStream(t), bus.Stream, h.replicas)
		return
	}
	_, js := h.admin(t)
	if _, err := js.AddStream(&config); err != nil {
		t.Fatalf("create the stream: %v", err)
	}
}

// publish publishes n messages on subjects kind's durable captures and answers their stream
// sequences.
func (h *freshHub) publish(t *testing.T, kind durableKind, n int) []uint64 {
	t.Helper()
	_, js := h.admin(t)
	sequences := make([]uint64, 0, n)
	for i := range n {
		ack, err := js.Publish(kind.subject(i), []byte(`{}`))
		if err != nil {
			t.Fatalf("publish to %s: %v", kind.subject(i), err)
		}
		sequences = append(sequences, ack.Sequence)
	}
	return sequences
}

// durableKind is one of the intakes' two durables, as the rows drive it: its name, this build's
// config of it, and a subject it captures.
type durableKind struct {
	logical string
	config  func() natsgo.ConsumerConfig
	subject func(n int) string
}

// durableKinds are the delivery-events and delivery-jobs durables.
var durableKinds = []durableKind{
	{bus.DispatchDeliveryDurable, deliveryConsumerConfig, func(n int) string {
		return fmt.Sprintf("notifications.github.acme.widgets.issue.%d", n)
	}},
	{bus.DispatchJobDurable, jobConsumerConfig, func(n int) string {
		return fmt.Sprintf("notifications.github.acme.widgets.job.%d", n)
	}},
}

// durableProcess is one envoy-dispatch process's take-over and delivery of one durable: a
// heldDurable over its own lease pool, its own database pool and its own connections, whose
// handler records the stream sequence of every message it is handed and acknowledges it.
type durableProcess struct {
	held   *heldDurable
	leases *cuttableLeases
	store  *cuttableStore
	reader *bus.Client
	stop   func()
	// step, when set, runs at each point of the fresh take-over (heldDurable.freshHook).
	step atomic.Pointer[func(p *durableProcess, point, name string) error]
	// blocked holds every handler until unblock.
	blocked chan struct{}

	mu      sync.Mutex
	handled map[uint64]int
}

// processOption sets up a durableProcess before it runs.
type processOption func(*durableProcess)

// blockingHandlers holds every handler of the process until its unblock.
func blockingHandlers() processOption {
	return func(p *durableProcess) { p.blocked = make(chan struct{}) }
}

// atStep runs step at each point of the process's fresh take-over.
func atStep(step func(p *durableProcess, point, name string) error) processOption {
	return func(p *durableProcess) { p.step.Store(&step) }
}

// startDurableProcess runs kind's durable as a process of its own in mode, over pool's database
// and the hub, until the test ends or its stop is called.
func startDurableProcess(t *testing.T, hub *freshHub, pool *store.Pool, kind durableKind, mode namingMode, options ...processOption) *durableProcess {
	t.Helper()
	second := storetest.SecondProcess(t, &store.Store{Pool: pool})
	process := &durableProcess{
		leases:  &cuttableLeases{pool: storetest.Leases(t, second.Pool)},
		handled: map[uint64]int{},
	}
	for _, option := range options {
		option(process)
	}
	reader, err := hub.connect(intakeConnectionName)
	if err != nil {
		t.Fatalf("connect the process's intake: %v", err)
	}
	process.reader = reader
	key := func(*natsgo.Msg) (string, *contracts.Envelope, bool) { return "all", nil, true }
	held := newHeldDurable(reader, process.leases, kind.config, key, process.handle)
	process.held = &held
	if mode == freshNames {
		process.store = &cuttableStore{inner: consumernames.NewPGStore(second.Pool)}
		held.nameFreshly(process.store, hub.connect, reader, nil)
		held.nats = nil
		held.freshHook = func(point, name string) error {
			if step := process.step.Load(); step != nil {
				return (*step)(process, point, name)
			}
			return nil
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		held.run(ctx, nil, func() bool { return true })
	}()
	process.stop = sync.OnceFunc(func() {
		cancel()
		<-done
		process.unblock()
		if held.nats != nil {
			held.nats.Close()
		}
		reader.Close()
		held.waitFinalizers()
	})
	t.Cleanup(process.stop)
	return process
}

// deliverySubject is the deliver subject of the subscription the process delivers with, "" with
// none.
func (p *durableProcess) deliverySubject() string {
	p.held.mu.Lock()
	defer p.held.mu.Unlock()
	if p.held.delivering == nil {
		return ""
	}
	return p.held.delivering.subject
}

// seedPreviousHolder makes what a holder that is gone leaves: kind's current consumer, created by
// the admin with this build's config of the durable, delivering from the stream's first message at
// a deliver subject the returned subscription holds, its name the record's current; and the
// durable's hold, lapsed, its token naming that subject, so the next holder takes the consumer
// over as a dead holder's. It answers the consumer's name and the subscription, the gone holder's
// connection, which the row acknowledges on.
func seedPreviousHolder(t *testing.T, hub *freshHub, pool *store.Pool, kind durableKind) (string, *natsgo.Subscription) {
	t.Helper()
	conn, js := hub.admin(t)
	name := consumernames.FreshName(kind.logical)
	config := kind.config()
	config.Durable, config.DeliverPolicy, config.OptStartSeq = name, natsgo.DeliverAllPolicy, 0
	config.Metadata[consumernames.LogicalMetadataKey] = kind.logical
	sub, err := conn.SubscribeSync(config.DeliverSubject)
	if err != nil {
		t.Fatalf("subscribe the gone holder's deliver subject: %v", err)
	}
	if _, err := js.AddConsumer(bus.Stream, &config); err != nil {
		t.Fatalf("create the gone holder's consumer %s: %v", name, err)
	}
	records := consumernames.NewPGStore(pool)
	record, revision, err := records.Read(t.Context(), kind.logical)
	if err != nil {
		t.Fatalf("read %s's record: %v", kind.logical, err)
	}
	record.Current, record.Start = name, 1
	if _, err := records.Write(t.Context(), kind.logical, record, revision); err != nil {
		t.Fatalf("select %s: %v", name, err)
	}
	lapsedHoldToken(t, pool, kind.logical, config.DeliverSubject)
	return name, sub
}

// lapsedHoldToken leaves logical's intake hold lapsed, its token naming subject, as a holder whose
// process died leaves it.
func lapsedHoldToken(t *testing.T, pool *store.Pool, logical, subject string) {
	t.Helper()
	leases := storetest.Leases(t, storetest.SecondProcess(t, &store.Store{Pool: pool}).Pool)
	hold, err := duty.TryHold(t.Context(), leases, intakeHoldName(logical), uuid.New(),
		duty.HoldOptions{TTL: intakeHoldTTL, Renew: intakeHoldRenew, Margin: intakeHoldMargin, Token: subject})
	if err != nil || hold == nil {
		t.Fatalf("take %s's hold: %v", logical, err)
	}
	if err := hold.Lapse(t.Context()); err != nil {
		t.Fatalf("lapse %s's hold: %v", logical, err)
	}
}

// successorOf is the consumer a take-over of old made: the record's current once it is another
// name, "" before.
func successorOf(t *testing.T, pool *store.Pool, logical, old string) string {
	t.Helper()
	if current := currentName(t, pool, logical); current != old {
		return current
	}
	return ""
}

// selectionRows is how many rows the selection table holds.
func selectionRows(t *testing.T, pool *store.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), `select count(*) from intake_consumers`).Scan(&n); err != nil {
		t.Fatalf("count the selection rows: %v", err)
	}
	return n
}

// endLeaseSession ends the database session that holds the lease named name (duty.Exclusive's
// session-level advisory lock, keyed by the name's hashtext), as the server ends a session whose
// client it lost: its lease is free from then on, whatever its process does next.
func endLeaseSession(t *testing.T, pool *store.Pool, name string) {
	t.Helper()
	var ended int
	if err := pool.QueryRow(context.Background(), `
		select count(pg_terminate_backend(pid)) from pg_locks
		where locktype = 'advisory' and granted and objsubid = 2 and objid = hashtext($1)::oid
		  and database = (select oid from pg_database where datname = current_database())`, name).Scan(&ended); err != nil || ended != 1 {
		t.Errorf("end the session holding lease %s: ended %d, %v", name, ended, err)
	}
}

func (p *durableProcess) handle(delivery *intakeDelivery, message intakeMessage) {
	meta, err := message.msg.Metadata()
	if err == nil {
		p.mu.Lock()
		p.handled[meta.Sequence.Stream]++
		p.mu.Unlock()
	}
	if p.blocked != nil {
		select {
		case <-p.blocked:
		case <-delivery.handling.Done():
			delivery.handBack(message.msg)
			return
		}
	}
	delivery.acknowledge(message.msg)
}

// unblock lets every handler of the process go on.
func (p *durableProcess) unblock() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.blocked != nil {
		select {
		case <-p.blocked:
		default:
			close(p.blocked)
		}
	}
}

// times is how often the process was handed the message at stream sequence seq.
func (p *durableProcess) times(seq uint64) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.handled[seq]
}

// kill stops the process as a SIGKILL does, from its own run goroutine (a step): its database
// statements fail from here on, so its hold lapses, and every connection it holds is closed.
func (p *durableProcess) kill() {
	p.leases.cut.Store(true)
	if p.store != nil {
		p.store.cut.Store(true)
	}
	if p.held.fresh != nil {
		for _, candidate := range p.held.fresh.candidates {
			candidate.client.Close()
		}
	}
	if p.held.nats != nil {
		p.held.nats.Close()
	}
	p.reader.Close()
}

// errKilled is a step a row stopped its process at.
var errKilled = errors.New("the process was killed at this step")

// killAt is a step that kills the process the first time its take-over reaches point.
func killAt(point string) processOption {
	var once sync.Once
	return atStep(func(p *durableProcess, at, _ string) error {
		if at != point {
			return nil
		}
		killed := false
		once.Do(func() { killed = true; p.kill() })
		if killed || p.leases.cut.Load() {
			return errKilled
		}
		return nil
	})
}

// cuttableStore is a process's selection store, whose every call fails once the row cuts it.
type cuttableStore struct {
	inner consumernames.Store
	cut   atomic.Bool
}

func (s *cuttableStore) State(ctx context.Context) (consumernames.StoreState, error) {
	if s.cut.Load() {
		return consumernames.StoreState{}, errCutOff
	}
	return s.inner.State(ctx)
}

func (s *cuttableStore) Read(ctx context.Context, logical string) (consumernames.Record, uint64, error) {
	if s.cut.Load() {
		return consumernames.Record{}, 0, errCutOff
	}
	return s.inner.Read(ctx, logical)
}

func (s *cuttableStore) Write(ctx context.Context, logical string, record consumernames.Record, revision uint64) (uint64, error) {
	if s.cut.Load() {
		return 0, errCutOff
	}
	return s.inner.Write(ctx, logical, record, revision)
}

// writeCutover writes the selection table's sentinel with the cutover marker cutover, as the
// cutover's convert writes it ("done") or the rollback's restore ("rolled_back").
func writeCutover(t *testing.T, pool *store.Pool, cutover string) {
	t.Helper()
	records := consumernames.NewPGStore(pool)
	_, revision, err := sentinelRow(t.Context(), pool)
	if err != nil {
		t.Fatalf("read the sentinel: %v", err)
	}
	if _, err := records.WriteSentinel(t.Context(), consumernames.StoreState{Cutover: cutover}, revision); err != nil {
		t.Fatalf("write the sentinel %q: %v", cutover, err)
	}
}

// sentinelRow is the sentinel row's revision, 0 when there is none.
func sentinelRow(ctx context.Context, pool *store.Pool) (string, uint64, error) {
	var record string
	var revision uint64
	err := pool.QueryRow(ctx, `select record::text, revision from intake_consumers where logical = $1`, consumernames.PGSentinelKey).Scan(&record, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, nil
	}
	return record, revision, err
}

// writeSelectionRow writes logical's selection row with no current, as the cutover's convert
// leaves a durable it found no consumer of, with start saved (0 for none).
func writeSelectionRow(t *testing.T, pool *store.Pool, logical string, start uint64) {
	t.Helper()
	if _, err := consumernames.NewPGStore(pool).Write(t.Context(), logical, consumernames.Record{Start: start}, 0); err != nil {
		t.Fatalf("write %s's selection row: %v", logical, err)
	}
}

// readyForFresh writes what the cutover leaves: the sentinel done, and a row for each durable.
func readyForFresh(t *testing.T, pool *store.Pool) {
	t.Helper()
	writeCutover(t, pool, consumernames.CutoverDone)
	for _, kind := range durableKinds {
		writeSelectionRow(t, pool, kind.logical, 0)
	}
}

// currentName is the physical name logical's durable delivers from: the record's current.
func currentName(t *testing.T, pool *store.Pool, logical string) string {
	t.Helper()
	record, _, err := consumernames.NewPGStore(pool).Read(t.Context(), logical)
	if err != nil {
		return ""
	}
	return record.Current
}

// selectionRecord is logical's record, for a row's failure message and its checks.
func selectionRecord(t *testing.T, pool *store.Pool, logical string) consumernames.Record {
	t.Helper()
	record, _, err := consumernames.NewPGStore(pool).Read(t.Context(), logical)
	if err != nil {
		t.Fatalf("read %s's selection record: %v", logical, err)
	}
	return record
}

// consumerSpy records every consumer create and delete any client sends the stream, by the
// consumer name its subject names, as the server is sent it.
type consumerSpy struct {
	mu      sync.Mutex
	creates []string
	deletes []string
	updates []string
}

// spyConsumerCalls subscribes the spy on the admin's connection until the test ends.
func spyConsumerCalls(t *testing.T, hub *freshHub) *consumerSpy {
	t.Helper()
	conn, _ := hub.admin(t)
	spy := &consumerSpy{}
	record := func(into *[]string) natsgo.MsgHandler {
		return func(msg *natsgo.Msg) {
			tokens := strings.Split(msg.Subject, ".")
			if len(tokens) < 6 {
				return
			}
			spy.mu.Lock()
			*into = append(*into, tokens[5])
			spy.mu.Unlock()
		}
	}
	for subject, into := range map[string]*[]string{
		"$JS.API.CONSUMER.CREATE." + bus.Stream + ".>": &spy.creates,
		"$JS.API.CONSUMER.DELETE." + bus.Stream + ".*": &spy.deletes,
	} {
		if _, err := conn.Subscribe(subject, record(into)); err != nil {
			t.Fatalf("spy on %s: %v", subject, err)
		}
	}
	if _, err := conn.Subscribe("$JS.API.STREAM.UPDATE."+bus.Stream, func(msg *natsgo.Msg) {
		spy.mu.Lock()
		spy.updates = append(spy.updates, string(msg.Data))
		spy.mu.Unlock()
	}); err != nil {
		t.Fatalf("spy on the stream's updates: %v", err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the spy: %v", err)
	}
	return spy
}

// requireEachNameCreatedOnce fails t when any consumer name was sent to a create twice, or when a
// fixed durable name was created or deleted: the fresh names' rule, which the fixed take-over's
// delete and create of one name breaks.
func (s *consumerSpy) requireEachNameCreatedOnce(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.creates) == 0 {
		t.Fatal("UNKNOWN: the spy saw no consumer create at all, so it shows nothing")
	}
	seen := map[string]int{}
	for _, name := range s.creates {
		seen[name]++
	}
	for name, n := range seen {
		if n > 1 {
			t.Errorf("consumer %s was sent to a create %d times; creates %v", name, n, s.creates)
		}
	}
	for _, fixed := range natsFixedDurables() {
		if seen[fixed] > 0 || slices.Contains(s.deletes, fixed) {
			t.Errorf("the fixed name %s was created %d times and deleted in %v", fixed, seen[fixed], s.deletes)
		}
	}
}

// natsFixedDurables are the intakes' fixed durable names.
func natsFixedDurables() []string {
	return []string{bus.DispatchDeliveryDurable, bus.DispatchJobDurable}
}

// createdNames are the names sent to a create, in order.
func (s *consumerSpy) createdNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.creates)
}

// awaitDelivering waits for logical's current consumer to be push-bound and the record to name
// it, and answers its name.
func awaitDelivering(t *testing.T, hub *freshHub, pool *store.Pool, logical string) string {
	t.Helper()
	_, js := hub.admin(t)
	var current string
	waitFor(t, 40*time.Second, func() bool {
		current = currentName(t, pool, logical)
		if current == "" {
			return false
		}
		info, err := js.ConsumerInfo(bus.Stream, current)
		return err == nil && info.PushBound
	}, func() string {
		return fmt.Sprintf("%s's durable is not delivering: the record selects %q", logical, current)
	})
	return current
}

// awaitHandled waits for every sequence of want to have been handed to one of the processes.
func awaitHandled(t *testing.T, want []uint64, processes ...*durableProcess) {
	t.Helper()
	missing := func() []uint64 {
		var missing []uint64
		for _, seq := range want {
			handled := false
			for _, p := range processes {
				if p.times(seq) > 0 {
					handled = true
				}
			}
			if !handled {
				missing = append(missing, seq)
			}
		}
		return missing
	}
	waitFor(t, 60*time.Second, func() bool { return len(missing()) == 0 }, func() string {
		return fmt.Sprintf("messages %v never reached a process", missing())
	})
}
