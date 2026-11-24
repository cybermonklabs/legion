package consumernames

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// The store and server the protocol's rows (names_test.go) run on: memStore keeps the records as
// a store with conditional writes does, and fakeServer holds consumers by name, refuses a second
// create of a name the way nats-server does, and records every create it is sent, so a row can
// say which names were created and how often. Each row interrupts the protocol at one point, as a
// process killed there leaves it, and runs the recovery in a second Names, as another process.

// memStore is a Store in memory.
type memStore struct {
	mu      sync.Mutex
	state   StoreState
	missing error
	records map[string]memRecord
	// deleted holds the keys whose last write is a delete marker.
	deleted map[string]bool
	seq     uint64
	// beforeWrite, when set, runs before each write and can fail it.
	beforeWrite func(logical string, record Record) error
}

type memRecord struct {
	record   Record
	revision uint64
}

func newMemStore() *memStore {
	return &memStore{state: StoreState{Cutover: CutoverDone}, records: map[string]memRecord{}, deleted: map[string]bool{}}
}

func (m *memStore) State(context.Context) (StoreState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.missing != nil {
		return StoreState{}, m.missing
	}
	return m.state, nil
}

func (m *memStore) Read(_ context.Context, logical string) (Record, uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.missing != nil {
		return Record{}, 0, m.missing
	}
	if m.deleted[logical] {
		return Record{}, 0, fmt.Errorf("%w: %s", ErrRecordDeleted, logical)
	}
	stored, ok := m.records[logical]
	if !ok {
		return Record{}, 0, ErrNoRecord
	}
	return stored.record.clone(), stored.revision, nil
}

func (m *memStore) Write(_ context.Context, logical string, record Record, revision uint64) (uint64, error) {
	m.mu.Lock()
	hook := m.beforeWrite
	m.mu.Unlock()
	if hook != nil {
		if err := hook(logical, record); err != nil {
			return 0, err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.missing != nil {
		return 0, m.missing
	}
	if stored := m.records[logical]; stored.revision != revision {
		return 0, fmt.Errorf("%w: %s at revision %d, not %d", ErrConflict, logical, stored.revision, revision)
	}
	m.seq++
	m.records[logical] = memRecord{record: record.clone(), revision: m.seq}
	return m.seq, nil
}

func (m *memStore) record(t *testing.T, logical string) Record {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, ok := m.records[logical]
	if !ok {
		t.Fatalf("no record for %s", logical)
	}
	return stored.record.clone()
}

func (m *memStore) put(logical string, record Record) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	m.records[logical] = memRecord{record: record.clone(), revision: m.seq}
}

// fakeConsumer is one consumer fakeServer holds.
type fakeConsumer struct {
	start  Start
	floor  uint64
	logic  string
	native Native
}

// fakeServer holds consumers by name.
type fakeServer struct {
	mu        sync.Mutex
	consumers map[string]*fakeConsumer
	creates   []string
	deletes   []string
	// capacity, when above 0, is the most consumers the server holds.
	capacity int
	// createFault, when set, decides what a create does: applied says whether the server holds
	// the consumer afterwards, and err is what the caller is answered.
	createFault func(name string) (applied bool, err error)
	// lastSeq is the stream's last sequence, which a native DeliverNew create starts after.
	lastSeq uint64
	// deleteFault, when set, decides what a delete answers: a non-nil error leaves the consumer
	// held and is the caller's answer.
	deleteFault func(name string) error
}

func newFakeServer() *fakeServer {
	return &fakeServer{consumers: map[string]*fakeConsumer{}, lastSeq: 100}
}

var errAnswerLost = errors.New("the create's answer was lost")

func (f *fakeServer) Info(_ context.Context, name string) (*jetstream.ConsumerInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.consumers[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return f.infoLocked(name, c), nil
}

func (f *fakeServer) infoLocked(name string, c *fakeConsumer) *jetstream.ConsumerInfo {
	info := &jetstream.ConsumerInfo{Name: name}
	info.Config.Durable = name
	info.Config.Metadata = map[string]string{LogicalMetadataKey: c.logic}
	switch c.native {
	case NativeNew:
		info.Config.DeliverPolicy = jetstream.DeliverNewPolicy
	case NativeAll:
		info.Config.DeliverPolicy = jetstream.DeliverAllPolicy
	default:
		info.Config.DeliverPolicy = jetstream.DeliverByStartSequencePolicy
		info.Config.OptStartSeq = c.start.Seq
	}
	info.AckFloor.Stream = c.floor
	// A consumer that delivered nothing yet reports its delivered stream sequence at its floor, as
	// nats-server does for a consumer created at deliver policy new (TestTheNativeOptionSavesTheStartTheServerReports).
	info.Delivered.Stream = c.floor
	return info
}

func (f *fakeServer) Delete(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, name)
	if f.deleteFault != nil {
		if err := f.deleteFault(name); err != nil {
			return err
		}
	}
	delete(f.consumers, name)
	return nil
}

// create is the CreateFunc of logical, which the rows hand the protocol.
func (f *fakeServer) create(logical string) CreateFunc {
	return func(_ context.Context, name string, start Start) (*jetstream.ConsumerInfo, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.creates = append(f.creates, name)
		if _, ok := f.consumers[name]; ok {
			return nil, &jetstream.APIError{Code: 400, ErrorCode: jetstream.JSErrCodeConsumerExists, Description: "consumer already exists"}
		}
		if f.capacity > 0 && len(f.consumers) >= f.capacity {
			return nil, fmt.Errorf("%w: %s", ErrConsumerLimit, name)
		}
		applied, err := true, error(nil)
		if f.createFault != nil {
			applied, err = f.createFault(name)
		}
		if !applied {
			return nil, err
		}
		c := &fakeConsumer{start: start, logic: logical, native: start.Native}
		if start.Native == "" && start.Seq > 0 {
			c.floor = start.Seq - 1
		}
		if start.Native == NativeNew {
			c.floor = f.lastSeq
		}
		f.consumers[name] = c
		if err != nil {
			return nil, err
		}
		return f.infoLocked(name, c), nil
	}
}

func (f *fakeServer) setFloor(name string, floor uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.consumers[name].floor = floor
}

func (f *fakeServer) holds(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.consumers[name]
	return ok
}

func (f *fakeServer) startOf(name string) Start {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.consumers[name].start
}

// createdTwice fails t when any name was sent to the server's create more than once.
func (f *fakeServer) createdTwice(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := map[string]bool{}
	for _, name := range f.creates {
		if seen[name] {
			t.Fatalf("%s was sent to the server's create twice: %v", name, f.creates)
		}
		seen[name] = true
	}
}

// harness is one row's store and server, and a Names over them, which is one process.
type harness struct {
	store  *memStore
	server *fakeServer
	clock  *fakeClock
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newHarness() *harness {
	return &harness{store: newMemStore(), server: newFakeServer(), clock: &fakeClock{now: time.Unix(1_700_000_000, 0)}}
}

// testCreateBound is the create bound of the test families' Configs (Config.CreateBound).
const testCreateBound = 10 * time.Second

// testSettleAge is the settle age of the test families' Configs (Config.SettleAge): the age an open
// operation's candidate is retired at, and the clock skew a dropper allows beside its until.
const testSettleAge = 2 * time.Second

// names is a process's Names over the row's store and server.
func (h *harness) names(owner string) *Names {
	return New(Config{
		Store:  h.store,
		Server: h.server,
		Prefix: func(string) string { return "session" },
		// The fixed name as a Config with no Fixed named it before Fixed was required: the prefix.
		Fixed: func(string) string { return "session" },
		Floor: func(info *jetstream.ConsumerInfo) uint64 { return info.AckFloor.Stream },
		StreamID: func(context.Context) (string, error) {
			return "stream-1", nil
		},
		Owner:       owner,
		Now:         h.clock.Now,
		SettleAge:   testSettleAge,
		Poll:        time.Millisecond,
		CreateBound: testCreateBound,
	})
}

// first is a first-create rule that starts at seq.
func first(seq uint64) func(context.Context) (Start, error) {
	return func(context.Context) (Start, error) { return Start{Seq: seq}, nil }
}
