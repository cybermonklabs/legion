package store

import (
	"context"
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

// The registry answers ReadErr for two watchers, and the listener's /healthz reports it under the
// interest cache. So the role claim watcher's reads that fail to settle past their barrier
// are reported under that watcher's own name and bucket, not as the interest registry's: here the
// interest reads complete, and only the role claim reads fail.
func TestReadErrNamesTheRoleClaimWatcherAndItsBucket(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	reg := openRegistry(t, conn)

	// The role claim watcher is replaced by one whose read barriers fail, on a clock the test
	// moves, which counts the re-reads its failed reads arm.
	roles := reg.roles()
	reg.roleWatcher.Stop()
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
	handle := bus.NewKeyValue(roles.Legacy(), roles.WatchHandle(), failingBarriers{JetStream: roles.Streams()})
	reg.roleWatcher = kvwatch.New("role claims", handle, roleCache{reg}, kvwatch.WithNow(clock), kvwatch.WithStreamProbeInterval(0),
		kvwatch.WithAfterFunc(func(d time.Duration, _ func()) kvwatch.Timer {
			if d == 5*time.Second {
				rereads.Add(1)
			}
			return neverFires{}
		}))
	reg.roleWatcher.Start()
	waitFor(t, 10*time.Second, func() bool { return rereads.Load() > 0 })

	if err := reg.ReadErr(); err != nil {
		t.Fatalf("ReadErr after the first failed role claim read = %v, want nil within the stale bound", err)
	}
	clockMu.Lock()
	now = now.Add(2*time.Minute + time.Second)
	clockMu.Unlock()
	err := reg.ReadErr()
	want := "role claims (KV bucket " + bus.RoleClaimBucket + "): "
	if !errors.Is(err, kvwatch.ErrUnverified) || !strings.HasPrefix(err.Error(), want) || strings.Contains(err.Error(), "interest") {
		t.Fatalf("ReadErr past the role claim watcher's stale bound = %v, want %v under %q, and no interest name", err, kvwatch.ErrUnverified, want)
	}
	if !errors.Is(err, natsgo.ErrConnectionClosed) {
		t.Fatalf("ReadErr past the stale bound = %v, want it to wrap the reads' own %v", err, natsgo.ErrConnectionClosed)
	}
}

// failingBarriers is a bucket's jetstream context whose publishes fail as a closed connection's
// do: a read's barrier (bus.KeyValue.WriteBarrier) is the one publish through it, so every read
// fails, while every stream read and watch goes on as usual.
type failingBarriers struct {
	jetstream.JetStream
}

func (failingBarriers) PublishMsg(context.Context, *natsgo.Msg, ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	return nil, natsgo.ErrConnectionClosed
}

// neverFires is a wait that never fires.
type neverFires struct{}

func (neverFires) Stop() bool { return true }
