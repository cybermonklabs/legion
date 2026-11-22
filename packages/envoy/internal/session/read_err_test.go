package session

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

// The registry's ReadErr is its watcher's: once every read of the session bucket for longer than
// the copy's stale bound has failed to settle past its barrier, ReadErr says so, which /healthz
// answers unhealthy. Ping reads only the bucket's stream and the watcher's terminal error, and both
// stay healthy throughout.
func TestReadErrReportsSessionReadsThatCannotReadPastABarrier(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(t.Context(), client.Conn, WithSessionReplicas(1), WithSessionTTL(10*time.Second))
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	t.Cleanup(reg.StopWatch)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := reg.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("wait for the session cache: %v", err)
	}
	// A row the bucket does not hold, as a delete the copy missed leaves one.
	value, err := json.Marshal(SessionEntry{MachineID: "test-machine", UpdatedAt: time.Now().UnixMilli()})
	if err != nil {
		t.Fatalf("encode the row: %v", err)
	}
	sessionCache{reg}.Apply(kvwatch.Entry{Key: "ses_held_by_the_copy", Value: value, Revision: 1, Created: time.Now(), Op: kvwatch.OpPut}, kvwatch.Read{})

	// The registry's watcher is replaced by one whose read barriers fail, on a clock the test
	// moves, which counts the re-reads its failed reads arm.
	kv := reg.watcher.KV()
	reg.watcher.Stop()
	var (
		clockMu sync.Mutex
		now     = time.Now()
		rereads atomic.Int32
	)
	clock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	handle := bus.NewKeyValue(kv.Legacy(), kv.WatchHandle(), failedBarriers{JetStream: kv.Streams()})
	replacement := kvwatch.New("session registry", handle, sessionCache{reg}, kvwatch.WithNow(clock), kvwatch.WithStreamProbeInterval(0),
		kvwatch.WithAfterFunc(func(d time.Duration, _ func()) kvwatch.Timer {
			if d == 5*time.Second {
				rereads.Add(1)
			}
			return heldTimer{}
		}))
	reg.mu.Lock()
	reg.watcher = replacement
	reg.mu.Unlock()
	replacement.Start()
	waitFor(t, 10*time.Second, func() bool { return rereads.Load() > 0 })

	if err := reg.ReadErr(); err != nil {
		t.Fatalf("ReadErr after the first failed read = %v, want nil within the stale bound", err)
	}
	if err := reg.Ping(); err != nil {
		t.Fatalf("Ping after the first failed read = %v, want nil: the bucket's stream answers", err)
	}
	clockMu.Lock()
	now = now.Add(2*time.Minute + time.Second)
	clockMu.Unlock()
	if err := reg.ReadErr(); !errors.Is(err, kvwatch.ErrUnverified) || !errors.Is(err, natsgo.ErrConnectionClosed) {
		t.Fatalf("ReadErr past the 2m stale bound = %v, want %v wrapping %v", err, kvwatch.ErrUnverified, natsgo.ErrConnectionClosed)
	}
	if err := reg.Ping(); err != nil {
		t.Fatalf("Ping past the stale bound = %v, want nil: only ReadErr reports the failed reads", err)
	}
}

// The known-session store's reads that fail to settle past their barrier are reported too, by
// its own ReadErr, which names the store's watcher and its bucket: it is none of the listener's
// caches, so /healthz reads this one to report them (degraded, cmd/listener's healthz handler).
func TestKnownSessionsReadErrReportsReadsThatCannotReadPastABarrier(t *testing.T) {
	client := setupNATS(t)
	known, err := OpenKnownSessions(t.Context(), client.Conn, WithKnownSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the known-session store: %v", err)
	}
	t.Cleanup(known.StopWatch)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := known.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("wait for the known-session cache: %v", err)
	}
	if err := known.ReadErr(); err != nil {
		t.Fatalf("ReadErr of an open store = %v, want nil", err)
	}

	// The store's watcher is replaced by one whose read barriers fail, on a clock the test moves,
	// which counts the re-reads its failed reads arm.
	kv := known.watch().KV()
	known.watch().Stop()
	var (
		clockMu sync.Mutex
		now     = time.Now()
		rereads atomic.Int32
	)
	clock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	handle := bus.NewKeyValue(kv.Legacy(), kv.WatchHandle(), failedBarriers{JetStream: kv.Streams()})
	replacement := kvwatch.New("known session store", handle, knownCache{known}, kvwatch.WithNow(clock), kvwatch.WithStreamProbeInterval(0),
		kvwatch.WithAfterFunc(func(d time.Duration, _ func()) kvwatch.Timer {
			if d == 5*time.Second {
				rereads.Add(1)
			}
			return heldTimer{}
		}))
	known.openMu.Lock()
	known.watcher = replacement
	known.openMu.Unlock()
	replacement.Start()
	waitFor(t, 10*time.Second, func() bool { return rereads.Load() > 0 })

	if err := known.ReadErr(); err != nil {
		t.Fatalf("ReadErr after the first failed read = %v, want nil within the stale bound", err)
	}
	clockMu.Lock()
	now = now.Add(2*time.Minute + time.Second)
	clockMu.Unlock()
	err = known.ReadErr()
	want := "known session store (KV bucket " + bus.KnownSessionBucket + "): "
	if !errors.Is(err, kvwatch.ErrUnverified) || !errors.Is(err, natsgo.ErrConnectionClosed) || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("ReadErr past the 2m stale bound = %v, want %v under %q wrapping %v", err, kvwatch.ErrUnverified, want, natsgo.ErrConnectionClosed)
	}
	var nothing *KnownSessions
	if err := nothing.ReadErr(); err != nil {
		t.Fatalf("ReadErr of a nil store = %v, want nil", err)
	}
}

// failedBarriers is a bucket's jetstream context whose publishes fail as a closed connection's do:
// a read's barrier (bus.KeyValue.WriteBarrier) is the one publish through it, so every read fails,
// while every stream read and watch goes on as usual.
type failedBarriers struct {
	jetstream.JetStream
}

func (failedBarriers) PublishMsg(context.Context, *natsgo.Msg, ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	return nil, natsgo.ErrConnectionClosed
}

// heldTimer is a wait that never fires.
type heldTimer struct{}

func (heldTimer) Stop() bool { return true }
