package session

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

// The order CI ran cmd/listener's TestSessionsHandler_IncludesTitle in when it failed
// ("expected 1 session, got 0", main_test.go:1909, run 44283375746, job 134752350774), held here
// so it runs the same way every time. That test opens the session registry, whose watcher starts
// its first read at once, and registers its session (Put) while that read is starting: the read's
// consumer is made on a bucket with nothing pending, the session's record is stored after it, and
// then the read begins. GET /v1/sessions answers List from the copy once the copy can be served.
//
// Here the watch is held once its consumer is made, the session is registered, and the watch is
// released. The consumer then hands over its scan's end at once, since nothing was pending when it
// was made, and every later delivery, the session's record among them, is held. A read that
// completes at the scan's end sweeps the session's row, since the read has no evidence of it, and
// the copy follows its bucket without it until the record is delivered. So the list is read the
// moment the copy follows its bucket. A read that waits for its own consumer to pass a barrier
// does not complete while the deliveries are held: once its barrier is stored they are let
// through, and the read completes holding the session.
func TestASessionRegisteredAsTheFirstReadStartsIsListedOnceTheReadCompletes(t *testing.T) {
	client := setupNATS(t)
	r, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open session registry: %v", err)
	}
	waitFor(t, 5*time.Second, r.watcher.Following)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r.watcher.End(ctx)
	kv := r.watcher.KV()
	window := &windowWatch{KeyValue: kv.WatchHandle(), made: make(chan struct{}), start: make(chan struct{}), afterEnd: make(chan struct{})}
	r.watcher = kvwatch.New("session registry", bus.NewKeyValue(kv.Legacy(), window, kv.Streams()), sessionCache{r})
	t.Cleanup(r.StopWatch)
	t.Cleanup(window.letThrough)
	r.watcher.Start()

	waitClosed(t, window.made, "the first read's consumer to be made")
	if err := r.Put("ses_titled", SessionEntry{SelfSubscribed: true, MachineID: "test-machine", Dir: "/test/ses_titled", Title: "My Session Title"}); err != nil {
		t.Fatalf("register the session: %v", err)
	}
	lastSeq := func() uint64 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		state, err := kv.ReadStreamState(ctx)
		if err != nil {
			t.Fatalf("read the session bucket's stream: %v", err)
		}
		return state.LastSeq
	}
	registered := lastSeq()
	close(window.start)

	// The read either follows its bucket at its scan's end, or stores its barrier after the
	// session's record and waits for its consumer to deliver both.
	waitFor(t, 10*time.Second, func() bool { return r.watcher.Following() || lastSeq() > registered })
	if !r.watcher.Following() {
		window.letThrough()
		waitFor(t, 10*time.Second, r.watcher.Following)
	}
	if err := r.Servable(); err != nil {
		t.Fatalf("the copy cannot be served once its first read completed: %v", err)
	}
	if got := listed(t, r); !slices.Equal(got, []string{"ses_titled"}) {
		t.Fatalf("listed once the first read completed = %v, want ses_titled: GET /v1/sessions answers this list", got)
	}
}

// windowWatch is a session bucket's jetstream handle whose watches are made, so each ordered
// consumer exists on the server with what was pending then, and then held until start closes.
// Each watch hands over its updates up to its scan's end, and holds every later one until
// afterEnd closes (letThrough).
type windowWatch struct {
	jetstream.KeyValue
	made, start, afterEnd chan struct{}
	madeOnce, letOnce     sync.Once
}

func (k *windowWatch) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	watcher, err := k.KeyValue.Watch(ctx, keys, opts...)
	if err != nil {
		return nil, err
	}
	k.madeOnce.Do(func() { close(k.made) })
	<-k.start
	updates := make(chan jetstream.KeyValueEntry)
	go func() {
		defer close(updates)
		ended := false
		for entry := range watcher.Updates() {
			if ended {
				select {
				case <-k.afterEnd:
				case <-ctx.Done():
					return
				}
			}
			select {
			case updates <- entry:
			case <-ctx.Done():
				return
			}
			ended = ended || entry == nil
		}
	}()
	return &forwardedKeyWatcher{KeyWatcher: watcher, updates: updates}, nil
}

// letThrough hands over every update held after a scan's end, and every later one.
func (k *windowWatch) letThrough() { k.letOnce.Do(func() { close(k.afterEnd) }) }
