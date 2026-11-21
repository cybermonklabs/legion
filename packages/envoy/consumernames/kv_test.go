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
	return newLiveStoreAt(t, uri, state, 1)
}

// newLiveStoreAt is newLiveStore with testStream and the bucket at replicas copies. A rig with a
// sentinel makes the bucket through CreateSessionBucket, which writes the sentinel only once the
// new bucket's stream answers: on a cluster a write right after the create can find no responder
// (run 44901606033's 2.10.29 step failed the protocol row's R1 subtest so, before any hold).
func newLiveStoreAt(t *testing.T, uri string, state *StoreState, replicas int) *liveStore {
	t.Helper()
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: testStream, Subjects: []string{"names.>"}, Replicas: replicas}); err != nil {
		t.Fatalf("create %s: %v", testStream, err)
	}
	live := &liveStore{conn: conn, js: js, store: NewKVStore(SessionBucket, ConnRequester{conn}), family: jsFamily{js}}
	if state == nil {
		if _, err := js.CreateKeyValue(ctx, SessionBucketConfig(replicas)); err != nil {
			t.Fatalf("create %s: %v", SessionBucket, err)
		}
		return live
	}
	if _, err := CreateSessionBucket(ctx, conn, replicas, *state); err != nil {
		t.Fatalf("create %s and its sentinel: %v", SessionBucket, err)
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

// The sentinel round-trips through the bucket: what the operator writes (the cutover, replay_from
// and its orphans, the stream's identity) reads back, and EndReplay, which rewrites it, clears only
// replay_from and the orphans.
func TestReplayModeEndsOnlyWhenNoListedOrphanIsHeld(t *testing.T) {
	live := newLiveStore(t, testnats.URL(t), &StoreState{Cutover: CutoverDone, ReplayFrom: 4, Orphans: []string{"s-a", "session~b"}, StreamID: "stream-1"})
	ctx := context.Background()
	held := map[string]bool{"session~b": true, "session~selected": true}
	if over, err := live.store.EndReplay(ctx, func(name string) bool { return held[name] }); err != nil || over {
		t.Fatalf("EndReplay with an orphan held = %t, %v; want replay mode kept", over, err)
	}
	if state, _ := live.store.State(ctx); state.ReplayFrom != 4 || state.StreamID != "stream-1" {
		t.Fatalf("sentinel %+v, want replay_from 4 and stream_id stream-1 kept", state)
	}
	delete(held, "session~b")
	if over, err := live.store.EndReplay(ctx, func(name string) bool { return held[name] }); err != nil || !over {
		t.Fatalf("EndReplay with no orphan held = %t, %v; want replay mode over", over, err)
	}
	if state, _ := live.store.State(ctx); state.ReplayFrom != 0 || len(state.Orphans) != 0 || state.Cutover != CutoverDone || state.StreamID != "stream-1" {
		t.Fatalf("sentinel %+v, want replay_from and the orphans cleared, the cutover and stream_id kept", state)
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

// On a grant with no direct get of the bucket (the listener beside Dispatch holds none), KVStore
// still reads and writes: it reads only from the stream's leader (STREAM.MSG.GET) and writes only
// on the key's expected last sequence, never nats.go's kvs.Get or kvs.Create. Two writers racing on
// one key at one revision end with one write and one ErrConflict. The control is nats.go's
// kvs.Create on the same grant: over a key whose last message is a delete, its refused first write
// falls back to a read of that marker (kv.go's Create) by direct get, which the grant refuses, so it
// fails where the same call on a full grant recreates the key.
func TestTheKVStoreRacesOnAGrantWithNoDirectGet(t *testing.T) {
	seed, public := testnats.User(t)
	stream := "KV_" + SessionBucket
	uri := testnats.StartNkeyDenied(t, public, "$JS.API.DIRECT.GET."+stream, "$JS.API.DIRECT.GET."+stream+".>")
	admin := testnats.Connect(t, uri)
	t.Cleanup(admin.Close)
	ctx := context.Background()
	if _, err := CreateSessionBucket(ctx, admin, 1, StoreState{Cutover: CutoverDone}); err != nil {
		t.Fatal(err)
	}
	writer := func() *KVStore {
		conn, err := testnats.ConnectSeed(t, uri, seed)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(conn.Close)
		return NewKVStore(SessionBucket, ConnRequester{conn})
	}
	first, second := writer(), writer()
	if _, err := first.Write(ctx, "ses-race", Record{Start: 1}, 0); err != nil {
		t.Fatalf("the first write on the reduced grant: %v", err)
	}
	for round := range 20 {
		_, revision, err := first.Read(ctx, "ses-race")
		if err != nil {
			t.Fatalf("round %d: the leader read on the reduced grant: %v", round, err)
		}
		results := make(chan error, 2)
		for _, store := range []*KVStore{first, second} {
			go func() {
				_, err := store.Write(ctx, "ses-race", Record{Start: uint64(round + 2)}, revision)
				results <- err
			}()
		}
		var wrote, conflicts int
		for range 2 {
			switch err := <-results; {
			case err == nil:
				wrote++
			case errors.Is(err, ErrConflict):
				conflicts++
			default:
				t.Fatalf("round %d: a racing write answered %v, want a write or ErrConflict", round, err)
			}
		}
		if wrote != 1 || conflicts != 1 {
			t.Fatalf("round %d: %d writes and %d conflicts, want one of each", round, wrote, conflicts)
		}
	}
	// Control: nats.go's Create over a deleted key reads the delete marker by direct get after its
	// refused write. On a full grant it recreates the key; on this one the read is refused and the
	// create fails.
	adminJS, err := jetstream.New(admin)
	if err != nil {
		t.Fatal(err)
	}
	adminKV, err := adminJS.KeyValue(ctx, SessionBucket)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ses-deleted-full", "ses-deleted-reduced"} {
		if _, err := adminKV.Put(ctx, key, []byte("{}")); err != nil {
			t.Fatal(err)
		}
		if err := adminKV.Delete(ctx, key); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := adminKV.Create(ctx, "ses-deleted-full", []byte("{}")); err != nil {
		t.Fatalf("control: kvs.Create over a deleted key on a full grant = %v, want the key recreated", err)
	}
	conn, err := testnats.ConnectSeed(t, uri, seed)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	kv, err := js.KeyValue(ctx, SessionBucket)
	if err != nil {
		t.Fatalf("bind the bucket on the reduced grant: %v", err)
	}
	if _, err := kv.Create(ctx, "ses-deleted-reduced", []byte("{}")); err == nil {
		t.Fatal("kvs.Create over a deleted key on a grant with no direct get recreated it: its fallback read was not refused")
	}
	// KVStore reads the same marker from the stream's leader, and refuses it as lost selection state.
	if _, _, err := first.Read(ctx, "ses-deleted-reduced"); !errors.Is(err, ErrRecordDeleted) {
		t.Fatalf("KVStore's read of the deleted key on the reduced grant = %v, want ErrRecordDeleted", err)
	}
}

// On a bucket of three copies a stale conditional write is a conflict as on one copy. nats.go
// reads a wrong last sequence as either 10071 or 10164 (jetstream's
// JSErrCodeStreamWrongLastSequenceConstant, which it says a replicated stream can answer);
// nats-server v2.15.1 answered 10071 to both subtests' expected-last-subject-sequence write.
func TestTheKVStoreReadsAReplicatedBucketsWrongLastSequenceAsAConflict(t *testing.T) {
	for _, replicas := range []int{1, 3} {
		t.Run(fmt.Sprintf("%d copies", replicas), func(t *testing.T) {
			cluster := testnats.StartCluster(t, 3)
			conn := testnats.Connect(t, cluster.URL())
			t.Cleanup(conn.Close)
			ctx := context.Background()
			js, err := jetstream.New(conn)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := js.CreateKeyValue(ctx, SessionBucketConfig(replicas)); err != nil {
				t.Fatal(err)
			}
			store := NewKVStore(SessionBucket, ConnRequester{conn})
			first, err := store.Write(ctx, "ses-1", Record{Start: 1}, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Write(ctx, "ses-1", Record{Start: 2}, first); err != nil {
				t.Fatal(err)
			}
			_, err = store.Write(ctx, "ses-1", Record{Start: 3}, first)
			var api *jetstream.APIError
			if !errors.As(err, &api) {
				t.Fatalf("a stale write on %d copies = %v, want the server's answer wrapped", replicas, err)
			}
			t.Logf("a stale write on %d copies: the server answered %d", replicas, api.ErrorCode)
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("a stale write on %d copies = %v (code %d), want ErrConflict", replicas, err, api.ErrorCode)
			}
		})
	}
}

// answeringRequester answers every request with reply, as a server would.
type answeringRequester struct{ reply string }

func (r answeringRequester) Request(context.Context, *natsgo.Msg) (*natsgo.Msg, error) {
	return &natsgo.Msg{Data: []byte(r.reply)}, nil
}

// A replicated stream answers a conditional write that meets another in flight on the same key with
// 10164, "wrong last sequence" (nats-server's checkMsgHeadersPreClusteredProposal), where one copy
// answers 10071: both mean the record changed under the write, so both are ErrConflict, and Names
// reads the record again. The control is another refusal, which stays an error of its own.
func TestTheKVStoreReadsEitherWrongLastSequenceAnswerAsAConflict(t *testing.T) {
	for code, description := range map[int]string{10071: "wrong last sequence: 5", 10164: "wrong last sequence"} {
		store := NewKVStore(SessionBucket, answeringRequester{fmt.Sprintf(`{"error":{"code":400,"err_code":%d,"description":%q}}`, code, description)})
		if _, err := store.Write(context.Background(), "ses-1", Record{Start: 1}, 4); !errors.Is(err, ErrConflict) {
			t.Errorf("a write answered %d = %v, want ErrConflict", code, err)
		}
	}
	// Control: a refusal that is no wrong last sequence is no conflict.
	store := NewKVStore(SessionBucket, answeringRequester{`{"error":{"code":503,"err_code":10008,"description":"jetstream temporarily unavailable"}}`})
	if _, err := store.Write(context.Background(), "ses-1", Record{Start: 1}, 4); err == nil || errors.Is(err, ErrConflict) {
		t.Fatalf("a write answered 10008 = %v, want an error that is no conflict", err)
	}
}

// Two writers racing on one key of a three-copy bucket, at the revision both read, end each round
// with one write and one ErrConflict, whichever of the server's two answers the loser gets.
func TestTwoWritersRacingOnAThreeCopyBucketEndWithOneWriteAndOneConflict(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	ctx := context.Background()
	admin := testnats.Connect(t, cluster.URL())
	t.Cleanup(admin.Close)
	if _, err := CreateSessionBucket(ctx, admin, 3, StoreState{Cutover: CutoverDone}); err != nil {
		t.Fatal(err)
	}
	writer := func() *KVStore {
		conn := testnats.Connect(t, cluster.URL())
		t.Cleanup(conn.Close)
		return NewKVStore(SessionBucket, ConnRequester{conn})
	}
	first, second := writer(), writer()
	if _, err := first.Write(ctx, "ses-race", Record{Start: 1}, 0); err != nil {
		t.Fatal(err)
	}
	for round := range 50 {
		_, revision, err := first.Read(ctx, "ses-race")
		if err != nil {
			t.Fatalf("round %d: read: %v", round, err)
		}
		start := make(chan struct{})
		results := make(chan error, 2)
		for _, store := range []*KVStore{first, second} {
			go func() {
				<-start
				_, err := store.Write(ctx, "ses-race", Record{Start: uint64(round + 2)}, revision)
				results <- err
			}()
		}
		close(start)
		var wrote, conflicts int
		for range 2 {
			switch err := <-results; {
			case err == nil:
				wrote++
			case errors.Is(err, ErrConflict):
				conflicts++
			default:
				t.Fatalf("round %d: a racing write answered %v, want a write or ErrConflict", round, err)
			}
		}
		if wrote != 1 || conflicts != 1 {
			t.Fatalf("round %d: %d writes and %d conflicts, want one of each", round, wrote, conflicts)
		}
	}
}

// On a real nats-server, a consumer created at deliver policy new reports a delivered stream
// sequence one before the first message it delivers, so the start NativeFirstCreate's publish saves
// is the first message published after the create. The boundary is set to 1 and the stream holds
// earlier messages, none of which the consumer delivers.
func TestTheNativeOptionSavesTheStartTheServerReports(t *testing.T) {
	live := newLiveStore(t, testnats.URL(t), &StoreState{Cutover: CutoverDone, Boundary: 1})
	ctx := context.Background()
	for i := range 3 {
		if _, err := live.js.Publish(ctx, "names.native", fmt.Appendf(nil, "before %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	names := live.names("p1")
	names.cfg.NativeFirstCreate = true
	sel, _, err := names.Ensure(ctx, "legion-go-p-github", CreateRequest{Create: live.family.create("legion-go-p-github"), First: nativeNew})
	if err != nil {
		t.Fatal(err)
	}
	ack, err := live.js.Publish(ctx, "names.native", []byte("after"))
	if err != nil {
		t.Fatal(err)
	}
	if sel.Record.Start != ack.Sequence {
		t.Fatalf("the saved start is %d, want %d, the first message published after the create", sel.Record.Start, ack.Sequence)
	}
	consumer, err := live.js.Consumer(ctx, testStream, sel.Record.Current)
	if err != nil {
		t.Fatal(err)
	}
	if policy := consumer.CachedInfo().Config.DeliverPolicy; policy != jetstream.DeliverNewPolicy {
		t.Fatalf("the consumer was created at %v, want deliver policy new", policy)
	}
	msg, err := consumer.Next(jetstream.FetchMaxWait(10 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if meta, err := msg.Metadata(); err != nil || meta.Sequence.Stream != sel.Record.Start {
		t.Fatalf("the consumer delivered %+v (%v) first, want stream sequence %d", meta, err, sel.Record.Start)
	}
}

// On a cluster a bucket's stream answers nobody for a moment after its create (no leader yet, or
// its interest not yet on the member the client is on), and a write then is answered "no
// responders". CreateSessionBucket writes the sentinel only once the stream answers, so on a
// three-server cluster, at one copy and at three, every create writes its sentinel. Each round
// creates a bucket of its own (createBucket, the session bucket's create under another name)
// through the next member, since one round wins the race most of the time; a bucket deleted and
// created again under one name races the delete's own clean-up instead (10049).
func TestCreatingTheSessionBucketOnAClusterWritesItsSentinel(t *testing.T) {
	for _, replicas := range []int{1, 3} {
		t.Run(fmt.Sprintf("%d copies", replicas), func(t *testing.T) {
			cluster := testnats.StartCluster(t, 3)
			for round := range 60 {
				conn := testnats.Connect(t, cluster.Servers[round%len(cluster.Servers)].URL())
				config := SessionBucketConfig(replicas)
				config.Bucket = fmt.Sprintf("%s_round_%d", SessionBucket, round)
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				store, err := createBucket(ctx, conn, config, StoreState{Family: SessionFamily, Cutover: CutoverDone, Boundary: 7})
				if err != nil {
					cancel()
					t.Fatalf("round %d: the create on a three-server cluster at %d copies = %v", round, replicas, err)
				}
				state, err := store.State(ctx)
				cancel()
				if err != nil || state.Cutover != CutoverDone || state.Boundary != 7 {
					t.Fatalf("round %d: the sentinel read back as %+v, %v", round, state, err)
				}
				conn.Close()
			}
		})
	}
}
