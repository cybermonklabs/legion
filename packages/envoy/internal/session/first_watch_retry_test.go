package session

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// unansweredFirstWatchKV is a session bucket's watch handle whose first fails watches run out of
// their wait unanswered (context.DeadlineExceeded), as a loaded listener's watcher create did
// (INFRA-4240's CPU-starvation control: "session registry watch failed ... context deadline
// exceeded"), and whose later watches are the bucket's own.
type unansweredFirstWatchKV struct {
	jetstream.KeyValue
	fails   int32
	watched atomic.Int32
}

func (k *unansweredFirstWatchKV) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	if k.watched.Add(1) <= k.fails {
		return nil, fmt.Errorf("create the watcher: %w", context.DeadlineExceeded)
	}
	return k.KeyValue.Watch(ctx, keys, opts...)
}

// heldRetry is a kvwatch AfterFunc that keeps every wait but a scan's idle wait for the test to
// fire, as the real timer would once its delay passed.
type heldRetry struct {
	mu    sync.Mutex
	fires []func()
	delay []time.Duration
}

func (h *heldRetry) after(d time.Duration, f func()) kvwatch.Timer {
	if d == kvwatch.ScanIdleTimeout {
		return time.AfterFunc(d, f)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fires = append(h.fires, f)
	h.delay = append(h.delay, d)
	return stoppedTimer{}
}

type stoppedTimer struct{}

func (stoppedTimer) Stop() bool { return true }

// The session registry's watcher makes a first watch that went unanswered again on its own clock,
// as the store's interest and role claim watchers do, and the copy it then reads answers every
// session a first watch that succeeded answers. The failed attempt is a WARN and nothing is an
// ERROR. The row's watcher is the registry's own (newWatcher), on a handle whose first watch times
// out, put in place of the one OpenSessionRegistry started once that one's copy is read as the
// first try's.
func TestASessionRegistryWhoseFirstWatchGoesUnansweredReadsEverySessionOnItsNextAttempt(t *testing.T) {
	client := setupNATS(t)
	logs := &logtest.Buffer{}
	r, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionLogger(slog.New(slog.NewJSONHandler(logs, nil))))
	if err != nil {
		t.Fatalf("open session registry: %v", err)
	}
	t.Cleanup(r.StopWatch)
	testwait.Eventually(t, "the first try's read to complete", r.watcher.Following)
	sessions := r.watcher.KV()
	ids := []string{"ses_first_a", "ses_first_b", "ses_first_c"}
	for i, id := range ids {
		putRaw(t, sessions.Legacy(), id, SessionEntry{Port: i + 1, MachineID: "m1", Title: id, UpdatedAt: time.Now().UnixMilli()})
	}
	firstTry := map[string]SessionEntry{}
	testwait.Eventually(t, "the first try's copy to answer every session", func() bool {
		for _, id := range ids {
			entry, err := r.Get(id)
			if err != nil {
				return false
			}
			firstTry[id] = entry
		}
		return true
	})

	r.watcher.Stop()
	r.mu.Lock()
	r.cache = newSessionCopy(r)
	r.mu.Unlock()
	seam := &unansweredFirstWatchKV{KeyValue: sessions.WatchHandle(), fails: 1}
	waits := &heldRetry{}
	r.watcher = r.newWatcher(bus.NewKeyValue(sessions.Legacy(), seam, sessions.Streams()),
		kvwatch.WithAfterFunc(waits.after), kvwatch.WithStreamProbeInterval(0))
	r.watcher.Start()

	testwait.Eventually(t, "the first attempt's WARN", func() bool {
		return strings.Contains(logs.String(), `"level":"WARN","msg":"session registry first watch attempt failed; it is made again"`)
	})
	waits.mu.Lock()
	armed, delay := len(waits.fires), waits.delay
	waits.mu.Unlock()
	if armed != 1 || delay[0] != 5*time.Second {
		t.Fatalf("after the first attempt the watcher armed %d waits (%v); want one, of 5s (the re-read's wait)", armed, delay)
	}
	waits.mu.Lock()
	retry := waits.fires[0]
	waits.mu.Unlock()
	retry()
	testwait.Eventually(t, "the second attempt's read to complete", r.watcher.Following)
	for _, id := range ids {
		entry, err := r.Get(id)
		if err != nil || entry.Title != firstTry[id].Title || entry.Port != firstTry[id].Port {
			t.Fatalf("Get(%s) after the second attempt = %+v, %v; want the first try's %+v", id, entry, err, firstTry[id])
		}
	}
	for line := range strings.SplitSeq(logs.String(), "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) == nil && record["level"] == "ERROR" {
			t.Fatalf("a first watch that recovered on its second attempt logged an ERROR: %v", record)
		}
	}
}
