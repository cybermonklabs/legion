package consumernames

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/testnats"
)

func TestMain(m *testing.M) { os.Exit(testnats.Main(m)) }

const testStream = "NAMES"

// jsFamily is a family of this file's own on a real server: consumers of testStream filtering on
// names.>, created by start sequence or by a native policy, and its Server.
type jsFamily struct {
	js jetstream.JetStream
}

func (f jsFamily) Info(ctx context.Context, name string) (*jetstream.ConsumerInfo, error) {
	consumer, err := f.js.Consumer(ctx, testStream, name)
	if errors.Is(err, jetstream.ErrConsumerNotFound) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if err != nil {
		return nil, err
	}
	return consumer.CachedInfo(), nil
}

func (f jsFamily) Delete(ctx context.Context, name string) error {
	err := f.js.DeleteConsumer(ctx, testStream, name)
	if errors.Is(err, jetstream.ErrConsumerNotFound) {
		return nil
	}
	return err
}

func (f jsFamily) create(logical string) CreateFunc {
	return func(ctx context.Context, name string, start Start) (*jetstream.ConsumerInfo, error) {
		config := jetstream.ConsumerConfig{
			Durable:       name,
			AckPolicy:     jetstream.AckExplicitPolicy,
			FilterSubject: "names.>",
			Metadata:      map[string]string{LogicalMetadataKey: logical},
		}
		switch start.Native {
		case NativeNew:
			config.DeliverPolicy = jetstream.DeliverNewPolicy
		case NativeAll:
			config.DeliverPolicy = jetstream.DeliverAllPolicy
		default:
			config.DeliverPolicy, config.OptStartSeq = jetstream.DeliverByStartSequencePolicy, start.Seq
		}
		consumer, err := f.js.CreateConsumer(ctx, testStream, config)
		if err != nil {
			var api *jetstream.APIError
			if errors.As(err, &api) && api.ErrorCode == jetstream.JSErrCodeMaximumConsumersLimit {
				return nil, fmt.Errorf("%w: %w", ErrConsumerLimit, err)
			}
			return nil, err
		}
		return consumer.CachedInfo(), nil
	}
}

// liveStore is a real server with testStream, the selection bucket and its sentinel, and a Names
// over them.
type liveStore struct {
	conn   *natsgo.Conn
	js     jetstream.JetStream
	store  *KVStore
	family jsFamily
}

func newLiveStore(t *testing.T, uri string, state *StoreState) *liveStore {
	t.Helper()
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: testStream, Subjects: []string{"names.>"}}); err != nil {
		t.Fatalf("create %s: %v", testStream, err)
	}
	if _, err := js.CreateKeyValue(ctx, SessionBucketConfig(1)); err != nil {
		t.Fatalf("create %s: %v", SessionBucket, err)
	}
	live := &liveStore{conn: conn, js: js, store: NewKVStore(SessionBucket, ConnRequester{conn}), family: jsFamily{js}}
	if state != nil {
		if _, err := live.store.WriteSentinel(ctx, *state, 0); err != nil {
			t.Fatalf("write the sentinel: %v", err)
		}
	}
	return live
}

func (l *liveStore) names(owner string) *Names {
	return New(Config{
		Store:    l.store,
		Server:   l.family,
		Prefix:   func(string) string { return "session" },
		Floor:    func(info *jetstream.ConsumerInfo) uint64 { return info.AckFloor.Stream },
		StreamID: func(context.Context) (string, error) { return "names-stream", nil },
		Owner:    owner,
	})
}

func TestTheKVStoreRefusesAMissingBucketAndAMissingSentinel(t *testing.T) {
	uri := testnats.URL(t)
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	store := NewKVStore(SessionBucket, ConnRequester{conn})
	if _, err := store.State(context.Background()); !errors.Is(err, ErrNoStore) {
		t.Fatalf("State with no bucket = %v, want ErrNoStore", err)
	}
	if _, err := store.Write(context.Background(), "ses-1", Record{Start: 1}, 0); !errors.Is(err, ErrNoStore) {
		t.Fatalf("Write with no bucket = %v, want ErrNoStore", err)
	}
	live := newLiveStore(t, uri, nil)
	if _, err := live.store.State(context.Background()); !errors.Is(err, ErrNoSentinel) {
		t.Fatalf("State with no sentinel = %v, want ErrNoSentinel", err)
	}
	if _, _, err := live.names("p1").Ensure(context.Background(), "ses-1", CreateRequest{Create: live.family.create("ses-1"), First: first(1)}); !IsSelectionStateLost(err) {
		t.Fatalf("Ensure with no sentinel = %v, want a refusal", err)
	}
}

func TestTheKVStoreSeesADeleteMarkerAsADeletedRecord(t *testing.T) {
	live := newLiveStore(t, testnats.URL(t), &StoreState{Cutover: CutoverDone})
	ctx := context.Background()
	if _, err := live.store.Write(ctx, "ses-1", Record{Start: 5}, 0); err != nil {
		t.Fatal(err)
	}
	kv, err := live.js.KeyValue(ctx, SessionBucket)
	if err != nil {
		t.Fatal(err)
	}
	// Control: before the delete the record reads.
	if record, _, err := live.store.Read(ctx, "ses-1"); err != nil || record.Start != 5 {
		t.Fatalf("Read = %+v, %v; want start 5", record, err)
	}
	if err := kv.Delete(ctx, "ses-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := live.store.Read(ctx, "ses-1"); !errors.Is(err, ErrRecordDeleted) {
		t.Fatalf("Read after a KV delete = %v, want ErrRecordDeleted", err)
	}
	if _, _, err := live.names("p1").Ensure(ctx, "ses-1", CreateRequest{Create: live.family.create("ses-1"), First: first(1)}); !errors.Is(err, ErrRecordDeleted) {
		t.Fatalf("Ensure after a KV delete = %v, want ErrRecordDeleted", err)
	}
	if err := kv.Purge(ctx, "ses-2"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := live.store.Read(ctx, "ses-2"); !errors.Is(err, ErrRecordDeleted) {
		t.Fatalf("Read after a KV purge = %v, want ErrRecordDeleted", err)
	}
}

func TestTheKVStoreWritesOnlyOverTheRevisionItRead(t *testing.T) {
	live := newLiveStore(t, testnats.URL(t), &StoreState{Cutover: CutoverDone})
	ctx := context.Background()
	if _, _, err := live.store.Read(ctx, "ses-1"); !errors.Is(err, ErrNoRecord) {
		t.Fatalf("Read of a key never written = %v, want ErrNoRecord", err)
	}
	first, err := live.store.Write(ctx, "ses-1", Record{Start: 1}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := live.store.Write(ctx, "ses-1", Record{Start: 2}, 0); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second write at revision 0 = %v, want ErrConflict", err)
	}
	second, err := live.store.Write(ctx, "ses-1", Record{Start: 3}, first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := live.store.Write(ctx, "ses-1", Record{Start: 4}, first); !errors.Is(err, ErrConflict) {
		t.Fatalf("a write at the old revision = %v, want ErrConflict", err)
	}
	if record, revision, err := live.store.Read(ctx, "ses-1"); err != nil || record.Start != 3 || revision != second {
		t.Fatalf("Read = %+v at %d, %v; want start 3 at %d", record, revision, err, second)
	}
}

func TestReplayModeEndsOnlyWhenNoListedOrphanIsHeld(t *testing.T) {
	live := newLiveStore(t, testnats.URL(t), &StoreState{Cutover: CutoverDone, ReplayFrom: 4, Orphans: []string{"s-a", "session~b"}})
	ctx := context.Background()
	held := map[string]bool{"session~b": true, "session~selected": true}
	if over, err := live.store.EndReplay(ctx, func(name string) bool { return held[name] }); err != nil || over {
		t.Fatalf("EndReplay with an orphan held = %t, %v; want replay mode kept", over, err)
	}
	if state, _ := live.store.State(ctx); state.ReplayFrom != 4 {
		t.Fatalf("replay_from %d, want 4 kept", state.ReplayFrom)
	}
	delete(held, "session~b")
	if over, err := live.store.EndReplay(ctx, func(name string) bool { return held[name] }); err != nil || !over {
		t.Fatalf("EndReplay with no orphan held = %t, %v; want replay mode over", over, err)
	}
	if state, _ := live.store.State(ctx); state.ReplayFrom != 0 || len(state.Orphans) != 0 || state.Cutover != CutoverDone {
		t.Fatalf("sentinel %+v, want replay_from and the orphans cleared, the cutover kept", state)
	}
}

// consumerCreateSpy records the consumer names every create request on the server names: a
// subscription on $JS.API.CONSUMER.>, which sees each request a client of the account sends.
type consumerCreateSpy struct {
	mu      sync.Mutex
	creates []string
	deletes []string
}

func spyOnConsumerCalls(t *testing.T, conn *natsgo.Conn) *consumerCreateSpy {
	t.Helper()
	spy := &consumerCreateSpy{}
	sub, err := conn.Subscribe("$JS.API.CONSUMER.>", func(msg *natsgo.Msg) {
		tokens := strings.Split(msg.Subject, ".")
		spy.mu.Lock()
		defer spy.mu.Unlock()
		switch {
		case len(tokens) >= 6 && tokens[3] == "CREATE":
			spy.creates = append(spy.creates, tokens[5])
		case len(tokens) >= 7 && tokens[3] == "DURABLE" && tokens[4] == "CREATE":
			spy.creates = append(spy.creates, tokens[6])
		case len(tokens) >= 6 && tokens[3] == "DELETE":
			spy.deletes = append(spy.deletes, tokens[5])
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := conn.Flush(); err != nil {
		t.Fatal(err)
	}
	return spy
}

// twice is the first name the spy saw sent to create more than once, and the fixed names (no
// '~') it saw created.
func (s *consumerCreateSpy) twice() (string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	var repeated string
	var fixed []string
	for _, name := range s.creates {
		if seen[name] && repeated == "" {
			repeated = name
		}
		seen[name] = true
		if !strings.Contains(name, "~") {
			fixed = append(fixed, name)
		}
	}
	return repeated, fixed
}

func (s *consumerCreateSpy) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.creates)
}

// awaitCreates waits until the spy has seen n creates: its subscription hears a request on its own
// connection, after the request's sender has its answer.
func (s *consumerCreateSpy) awaitCreates(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for s.count() < n {
		if time.Now().After(deadline) {
			t.Fatalf("the spy saw %d creates, want %d", s.count(), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNoConsumerNameIsCreatedTwice(t *testing.T) {
	live := newLiveStore(t, testnats.URL(t), &StoreState{Cutover: CutoverDone, Boundary: 1})
	ctx := context.Background()
	spyConn := testnats.Connect(t, testnats.URL(t))
	t.Cleanup(spyConn.Close)
	spy := spyOnConsumerCalls(t, spyConn)

	// Control: today's restart, a delete and a create of one fixed name, is seen as that name
	// created twice, and as a fixed name.
	control := live.family.create("ses-control")
	for range 2 {
		if _, err := control(ctx, "s-ses-control", Start{Seq: 1}); err != nil {
			t.Fatal(err)
		}
		if err := live.family.Delete(ctx, "s-ses-control"); err != nil {
			t.Fatal(err)
		}
	}
	spy.awaitCreates(t, 2)
	if repeated, fixed := spy.twice(); repeated != "s-ses-control" || len(fixed) != 2 {
		t.Fatalf("the control's same-name re-creation was seen as repeated %q, fixed %v; the spy is blind", repeated, fixed)
	}
	spy.mu.Lock()
	spy.creates = nil
	spy.mu.Unlock()

	// Treatment: every creation site of the protocol, its interruptions included.
	names := live.names("p1")
	create := live.family.create("ses-1")
	sel, _, err := names.Ensure(ctx, "ses-1", CreateRequest{Create: create, First: first(1)})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if sel, err = names.Replace(ctx, sel, 1, create); err != nil {
			t.Fatal(err)
		}
	}
	killed := errors.New("killed")
	sel, err = names.Select(ctx, "ses-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := names.Replace(ctx, sel, 1, func(context.Context, string, Start) (*jetstream.ConsumerInfo, error) { return nil, killed }); !errors.Is(err, killed) {
		t.Fatalf("the killed replace = %v", err)
	}
	recovery := New(Config{Store: live.store, Server: live.family, Prefix: func(string) string { return "session" },
		Floor: func(info *jetstream.ConsumerInfo) uint64 { return info.AckFloor.Stream }, StreamID: func(context.Context) (string, error) { return "names-stream", nil },
		Owner: "p2", SettleAge: time.Millisecond})
	if _, err := recovery.Select(ctx, "ses-1", create); err != nil {
		t.Fatal(err)
	}
	if _, err := names.Delete(ctx, "ses-1", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := names.Ensure(ctx, "ses-1", CreateRequest{Create: create, First: first(1)}); err != nil {
		t.Fatal(err)
	}
	spy.awaitCreates(t, 6)
	if repeated, fixed := spy.twice(); repeated != "" || len(fixed) != 0 {
		t.Fatalf("the protocol sent %q to create twice, and created the fixed names %v", repeated, fixed)
	}
}
