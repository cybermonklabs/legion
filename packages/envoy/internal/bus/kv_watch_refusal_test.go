package bus

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/testnats"
)

// watchRefusalBucket is the bucket the rows below watch.
const watchRefusalBucket = "envoy_watch_refusal"

// watchRefusalCreate is the subject of a consumer create on watchRefusalBucket's stream for the
// consumer name, followed by filter when there is one: the subject nats.go sends a watch's create
// to, with the name nats.go would pick in name's place.
func watchRefusalCreate(name string, filter ...string) string {
	return jsAPI("CONSUMER.CREATE", append([]string{"KV_" + watchRefusalBucket, name}, filter...)...)
}

// watchBuilt is what a watch answered: its watcher, or its error.
type watchBuilt struct {
	watcher jetstream.KeyWatcher
	err     error
}

// heldWatch is a jetstream handle whose watches wait for release before they make the real watch,
// and hand what the real watch returned to built as well as to their caller. A row keeps the
// watch's own consumer create back while the connection earns another request's refusal, and reads
// what the construction returned even when the call under test stopped waiting for it.
type heldWatch struct {
	jetstream.KeyValue
	started chan struct{}
	release chan struct{}
	built   chan watchBuilt
}

func (h *heldWatch) hold(watch func() (jetstream.KeyWatcher, error)) (jetstream.KeyWatcher, error) {
	close(h.started)
	<-h.release
	watcher, err := watch()
	h.built <- watchBuilt{watcher: watcher, err: err}
	return watcher, err
}

func (h *heldWatch) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	return h.hold(func() (jetstream.KeyWatcher, error) { return h.KeyValue.Watch(ctx, keys, opts...) })
}

func (h *heldWatch) WatchFiltered(ctx context.Context, keys []string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	return h.hold(func() (jetstream.KeyWatcher, error) { return h.KeyValue.WatchFiltered(ctx, keys, opts...) })
}

// startHeldWatch starts a server whose user may publish to anything but deny, opens
// watchRefusalBucket through a client of that user, and starts watch on a handle of the bucket whose
// watches are held (heldWatch). It returns once the watch is held, with the client, whose record the
// watch reads, the held handle and the channel the watch answers on.
func startHeldWatch(t *testing.T, deny string, watch func(context.Context, KeyValue) (jetstream.KeyWatcher, error)) (*Client, *heldWatch, <-chan watchBuilt) {
	t.Helper()
	seed, public := testnats.User(t)
	uri := testnats.StartNkeyDenied(t, public, deny)
	t.Setenv("NATS_NKEY_SEED", seed)
	client, err := Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect the client: %v", err)
	}
	t.Cleanup(client.Close)
	kv, err := EnsureKeyValue(t.Context(), client.Conn, client.JS(), &nats.KeyValueConfig{Bucket: watchRefusalBucket, Storage: nats.MemoryStorage})
	if err != nil {
		t.Fatalf("create the bucket: %v", err)
	}
	held := &heldWatch{KeyValue: kv.WatchHandle(), started: make(chan struct{}), release: make(chan struct{}), built: make(chan watchBuilt, 1)}
	handle := NewKeyValue(kv.Legacy(), held, kv.Streams())
	answered := make(chan watchBuilt, 1)
	go func() {
		watcher, err := watch(t.Context(), handle)
		answered <- watchBuilt{watcher: watcher, err: err}
	}()
	<-held.started
	return client, held, answered
}

// refuse has client's connection publish to subject, which its grant refuses, and returns the
// time it sent it, so the record can be read for that refusal.
func refuse(t *testing.T, client *Client, subject string) time.Time {
	t.Helper()
	sent := time.Now()
	if err := client.Conn.Publish(subject, nil); err != nil {
		t.Fatalf("publish to %s: %v", subject, err)
	}
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush the publish to %s: %v", subject, err)
	}
	return sent
}

// stopWatch stops watcher and reads it until it closes.
func stopWatch(watcher jetstream.KeyWatcher) {
	_ = watcher.Stop()
	go DrainWatcher(watcher)
}

// watcherEnds reports whether watcher's updates close within bound, which they do only once its
// subscription has ended: a watcher nobody stopped keeps them open for as long as its context lasts.
func watcherEnds(watcher jetstream.KeyWatcher, bound time.Duration) bool {
	timeout := time.After(bound)
	for {
		select {
		case _, open := <-watcher.Updates():
			if !open {
				return true
			}
		case <-timeout:
			return false
		}
	}
}

// watchAnswerSettle is how long a row gives a held watch to answer on a refusal recorded while it
// is held, once the record shows the refusal: a watch the refusal answers takes it within
// microseconds of the record's signal, so a second tells that apart from a watch still waiting for
// its own create on a loaded machine.
const watchAnswerSettle = time.Second

// watcherStopBound is how long a watcher the bus stopped may take to close its updates: the stop
// is one unsubscribe, so five seconds tells a stopped watcher from one left running.
const watcherStopBound = 5 * time.Second

// A watch answers with the refusal of its own consumer create, or with its own watcher, and never
// with the refusal of another consumer's create on the same bucket and connection. nats.go picks a
// watch's consumer name as it sends the create, so the refusal a watch reads is one of a create of
// its own shape: the bucket's stream, one name, and the watch's own filter when it has one key, or
// no filter in the subject when it has several. Each watch, of one key and of several, takes four
// rows: no refusal at all; the refusal of another consumer's create, of another key, recorded while
// the watch's own create is held back, which must leave the watch its watcher and the refusal in
// the record for the request that earned it; its own create refused, which must still end at once
// naming that refusal; and a refusal of a create of its own shape that the grant gives one named
// consumer only, which answers the watch before its own create is sent, and must not leave the
// watcher that create then builds running with nobody holding it.
func TestAWatchIsAnsweredByItsOwnCreateAndNotByAnotherConsumersRefusal(t *testing.T) {
	bucketKeys := "$KV." + watchRefusalBucket + "."
	other := watchRefusalCreate("unrelated", bucketKeys+"denied")
	for _, watch := range []struct {
		name string
		call func(context.Context, KeyValue) (jetstream.KeyWatcher, error)
		// own is a grant pattern that refuses every create of this watch's shape, and named is one
		// create of that shape, of a consumer named "named".
		own, named string
		// refused matches the refusal of this watch's own create, whose name nats.go picked.
		refused *regexp.Regexp
	}{
		{
			name: "WatchKeys",
			call: func(ctx context.Context, kv KeyValue) (jetstream.KeyWatcher, error) {
				return kv.WatchKeys(ctx, "allowed")
			},
			own:     watchRefusalCreate("*", bucketKeys+"allowed"),
			named:   watchRefusalCreate("named", bucketKeys+"allowed"),
			refused: regexp.MustCompile(`Permissions Violation for Publish to "` + regexp.QuoteMeta(watchRefusalCreate("")) + `[^."]+` + regexp.QuoteMeta("."+bucketKeys+"allowed") + `"`),
		},
		{
			name: "WatchFilteredKeys",
			call: func(ctx context.Context, kv KeyValue) (jetstream.KeyWatcher, error) {
				return kv.WatchFilteredKeys(ctx, []string{"allowed.a", "allowed.b"})
			},
			own:     watchRefusalCreate("*"),
			named:   watchRefusalCreate("named"),
			refused: regexp.MustCompile(`Permissions Violation for Publish to "` + regexp.QuoteMeta(watchRefusalCreate("")) + `[^."]+"`),
		},
	} {
		t.Run(watch.name, func(t *testing.T) {
			t.Run("no refusal", func(t *testing.T) {
				_, held, answered := startHeldWatch(t, other, watch.call)
				close(held.release)
				got := <-answered
				if got.err != nil {
					t.Fatalf("a watch the grant allows = %v, want its watcher", got.err)
				}
				stopWatch(got.watcher)
			})

			t.Run("another consumer's create is refused", func(t *testing.T) {
				client, held, answered := startHeldWatch(t, other, watch.call)
				refusedAt := refuse(t, client, other)
				early := func(got watchBuilt) {
					close(held.release)
					if built := <-held.built; built.err == nil {
						stopWatch(built.watcher)
					}
					if got.err == nil {
						stopWatch(got.watcher)
					}
					t.Fatalf("the watch answered %v before its own create was sent, on the refusal of another consumer's create", got.err)
				}
				deadline := time.After(refusalRecordWait)
				for !client.holdsRefusal(other, refusedAt) {
					select {
					case got := <-answered:
						early(got)
					case <-deadline:
						close(held.release)
						t.Fatalf("the server's refusal of %s was not recorded within %s", other, refusalRecordWait)
					case <-time.After(refusalRecordPoll):
					}
				}
				select {
				case got := <-answered:
					early(got)
				case <-time.After(watchAnswerSettle):
				}
				close(held.release)
				got := <-answered
				if got.err != nil {
					t.Fatalf("a watch the grant allows = %v, want its watcher", got.err)
				}
				if built := <-held.built; built.watcher != got.watcher {
					t.Fatalf("the watch answered a watcher its own construction did not build")
				}
				stopWatch(got.watcher)
				if err := client.deniedPublish(other, refusedAt); !errors.Is(err, ErrPublishDenied) {
					t.Fatalf("the record's refusal of %s, after the watch = %v, want it left there for the request that earned it", other, err)
				}
			})

			t.Run("its own create is refused", func(t *testing.T) {
				_, held, answered := startHeldWatch(t, watch.own, watch.call)
				started := time.Now()
				close(held.release)
				got := <-answered
				took := time.Since(started)
				if !errors.Is(got.err, ErrPublishDenied) {
					t.Fatalf("a watch whose own create the grant refuses = %v (after %s), want ErrPublishDenied", got.err, took)
				}
				if !watch.refused.MatchString(got.err.Error()) {
					t.Fatalf("the refused watch = %v, want it to name the refusal of its own create (%s)", got.err, watch.refused)
				}
				if took > refusalAnswerBound {
					t.Fatalf("the refused watch ended after %s, want within %s", took, refusalAnswerBound)
				}
			})

			t.Run("a create of its own shape is refused", func(t *testing.T) {
				client, held, answered := startHeldWatch(t, watch.named, watch.call)
				refuse(t, client, watch.named)
				var got watchBuilt
				early := false
				select {
				case got = <-answered:
					early = true
				case <-time.After(refusalRecordWait):
				}
				close(held.release)
				if !early {
					got = <-answered
				}
				built := <-held.built
				if built.err != nil {
					t.Fatalf("the watch's own create = %v, want it allowed: the grant refuses only the consumer named %q", built.err, "named")
				}
				if got.err == nil {
					if got.watcher != built.watcher {
						t.Fatalf("the watch answered a watcher its own construction did not build")
					}
					stopWatch(got.watcher)
					return
				}
				if !watcherEnds(built.watcher, watcherStopBound) {
					stopWatch(built.watcher)
					t.Fatalf("the watch answered %v, and the watcher its own create then built was still running %s later, with nobody holding it", got.err, watcherStopBound)
				}
			})
		})
	}
}
