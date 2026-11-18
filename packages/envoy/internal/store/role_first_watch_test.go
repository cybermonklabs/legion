package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testwait"
)

// unansweredFirstWatchKV is a role bucket's watch handle whose first fails watches run out of
// their wait unanswered (context.DeadlineExceeded), as a loaded listener's watcher create did
// (INFRA-4240's CPU-starvation control: "role claims watch failed ... context deadline
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

// heldWaits is a kvwatch AfterFunc that keeps every wait but a scan's idle wait for the test to
// fire, as the real timer would once its delay passed.
type heldWaits struct {
	mu    sync.Mutex
	fires []func()
	delay []time.Duration
}

func (h *heldWaits) after(d time.Duration, f func()) kvwatch.Timer {
	if d == kvwatch.ScanIdleTimeout {
		return time.AfterFunc(d, f)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fires = append(h.fires, f)
	h.delay = append(h.delay, d)
	return heldTimer{}
}

// armed is how many waits the watcher has armed, and the delay of the last.
func (h *heldWaits) armed() (int, time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.fires) == 0 {
		return 0, 0
	}
	return len(h.fires), h.delay[len(h.delay)-1]
}

// fire runs the i-th wait's callback.
func (h *heldWaits) fire(i int) {
	h.mu.Lock()
	f := h.fires[i]
	h.mu.Unlock()
	f()
}

type heldTimer struct{}

func (heldTimer) Stop() bool { return true }

// The role claim watcher makes a first watch that went unanswered again, on its own clock, and the
// view it then reads holds every claim a first watch that succeeded reads: the one a loaded
// listener's start lost to a watcher create that ran out of its wait is a WARN, and the listener's
// role claim view fills a moment later rather than wait for the self-health rebuild. The row's
// watcher is the store's own (newRoleWatcher), on a handle whose first watch times out, put in
// place of the one Open started once that one's view is read as the first try's.
func TestARoleClaimWatchWhoseFirstWatchGoesUnansweredReadsEveryClaimOnItsNextAttempt(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	logs := &lockedBuffer{}
	registry, err := Open(conn, WithReplicas(1), WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	for i := range 6 {
		putRoleClaim(t, registry.roles().Legacy(), fmt.Sprintf("role-%d", i), fmt.Sprintf("ses_%d", i%3))
	}
	// The first try that succeeded: Open's own watcher, once it has read all six claims.
	firstTry := testwait.Until(t, "Open's role claim view to hold all six claims", func() (map[string][]string, bool) {
		held, err := registry.RolesBySession()
		count := 0
		for _, roles := range held {
			count += len(roles)
		}
		return held, err == nil && count == 6
	})

	registry.roleWatcher.Stop()
	registry.holdersMu.Lock()
	registry.roleHolders = newRoleHolderCopy(registry)
	registry.holdersMu.Unlock()
	roles := registry.roles()
	seam := &unansweredFirstWatchKV{KeyValue: roles.WatchHandle(), fails: 1}
	waits := &heldWaits{}
	registry.roleWatcher = registry.newRoleWatcher(bus.NewKeyValue(roles.Legacy(), seam, roles.Streams()),
		kvwatch.WithAfterFunc(waits.after), kvwatch.WithStreamProbeInterval(0))
	registry.roleWatcher.Start()

	attempt := testwait.Until(t, "the first attempt's WARN", func() (map[string]any, bool) {
		for _, record := range jsonRecords(logs.String()) {
			if record["level"] == "WARN" && record["msg"] == "role claims first watch attempt failed; it is made again" {
				return record, true
			}
		}
		return nil, false
	})
	if attempt["attempt"] != float64(1) || !strings.Contains(fmt.Sprint(attempt["error"]), "context deadline exceeded") {
		t.Fatalf("the first attempt's WARN = %v, want attempt 1 naming the deadline", attempt)
	}
	count, delay := waits.armed()
	if count != 1 || delay != 5*time.Second {
		t.Fatalf("after the first attempt the watcher armed %d waits, the last of %s; want one, of 5s (the re-read's wait)", count, delay)
	}
	waits.fire(0)
	testwait.Eventually(t, "the second attempt's view to answer", func() bool { return registry.RoleViewErr() == nil })
	retried, err := registry.RolesBySession()
	if err != nil || !maps.EqualFunc(retried, firstTry, slices.Equal) {
		t.Fatalf("the view the second attempt read = %v, %v; want the first try's %v", retried, err, firstTry)
	}
	for _, record := range jsonRecords(logs.String()) {
		if record["level"] == "ERROR" {
			t.Fatalf("a first watch that recovered on its second attempt logged an ERROR: %v", record)
		}
	}
}

// The interest registry's watcher makes a first watch that went unanswered again on its own clock,
// as the role claim watcher does, and the copy it then reads lists every interest a first watch
// that succeeded lists. The row's watcher is the store's own (newInterestWatcher), on a handle
// whose first watch times out, put in place of the one Open started once that one's list is read
// as the first try's.
func TestAnInterestWatchWhoseFirstWatchGoesUnansweredReadsEveryInterestOnItsNextAttempt(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	logs := &lockedBuffer{}
	registry, err := Open(conn, WithReplicas(1), WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	interests := registry.watcher.KV()
	for i := range 4 {
		sessionID := fmt.Sprintf("ses_interest_%d", i)
		putInterest(t, interests.Legacy(), Interest{SessionID: sessionID, MachineID: "m1", Topics: []string{"notifications.agent." + sessionID}})
	}
	firstTry := testwait.Until(t, "Open's interest copy to list all four interests", func() ([]Interest, bool) {
		listed, err := registry.List()
		return listed, err == nil && len(listed) == 4
	})

	registry.watcher.Stop()
	registry.mu.Lock()
	registry.cache = newInterestCopy(registry)
	registry.mu.Unlock()
	seam := &unansweredFirstWatchKV{KeyValue: interests.WatchHandle(), fails: 1}
	waits := &heldWaits{}
	registry.watcher = registry.newInterestWatcher(bus.NewKeyValue(interests.Legacy(), seam, interests.Streams()),
		kvwatch.WithAfterFunc(waits.after), kvwatch.WithStreamProbeInterval(0))
	registry.watcher.Start()

	attempt := testwait.Until(t, "the first attempt's WARN", func() (map[string]any, bool) {
		for _, record := range jsonRecords(logs.String()) {
			if record["level"] == "WARN" && record["msg"] == "interest registry first watch attempt failed; it is made again" {
				return record, true
			}
		}
		return nil, false
	})
	if attempt["attempt"] != float64(1) {
		t.Fatalf("the first attempt's WARN = %v, want attempt 1", attempt)
	}
	if count, delay := waits.armed(); count != 1 || delay != 5*time.Second {
		t.Fatalf("after the first attempt the watcher armed %d waits, the last of %s; want one, of 5s (the re-read's wait)", count, delay)
	}
	waits.fire(0)
	retried := testwait.Until(t, "the second attempt's copy to list again", func() ([]Interest, bool) {
		listed, err := registry.List()
		return listed, err == nil
	})
	if !reflect.DeepEqual(retried, firstTry) {
		t.Fatalf("the copy the second attempt read lists %v, want the first try's %v", retried, firstTry)
	}
	for _, record := range jsonRecords(logs.String()) {
		if record["level"] == "ERROR" {
			t.Fatalf("a first watch that recovered on its second attempt logged an ERROR: %v", record)
		}
	}
}

// The role-revision scan's consumer create is one of the open's requests: on a context with no
// deadline, as a production open's, nats.go holds one create to 5 s, and a create that drew no
// answer is sent again, a WARN for the one that went unanswered, no sooner than 5 s after it
// began; the create answered next reads every claim. A create the server refuses fails the scan
// at once, naming the refusal and no wait, since nothing waited. A context with a deadline of the
// caller's (WithRequestWait) still bounds every send together, and a create never answered within
// it fails naming the request and the wait, so the start that ends on it says why. The creates are
// a seam's, which answers each at once: with the deadline, or with the refusal.
func TestTheRoleRevisionScansConsumerCreateWaitsTheOpensRequestWait(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	registry, err := Open(conn, WithReplicas(1))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	roles := registry.roles()
	for i := range 3 {
		putRoleClaim(t, roles.Legacy(), fmt.Sprintf("role-%d", i), fmt.Sprintf("ses_%d", i))
	}
	const openWait = 10 * time.Second

	t.Run("a create unanswered once is sent again", func(t *testing.T) {
		seam := &unansweredFirstWatchKV{KeyValue: roles.WatchHandle(), fails: 1}
		logs := &lockedBuffer{}
		// The pause before the second send is armed on the scan's clock, which the row moves.
		fake := clocktest.New(time.Now())
		type scan struct {
			revisions map[string]uint64
			err       error
		}
		done := make(chan scan, 1)
		go func() {
			revisions, err := roleRevisions(context.Background(), bus.NewKeyValue(roles.Legacy(), seam, roles.Streams()), fake, kvwatch.ScanIdleTimeout, slog.New(slog.NewJSONHandler(logs, nil)), openWait)
			done <- scan{revisions, err}
		}()
		got := testwait.Until(t, "the scan whose first create went unanswered to send it again and read every claim", func() (scan, bool) {
			select {
			case s := <-done:
				return s, true
			default:
			}
			if seam.watched.Load() == 1 && len(fake.Armed()) > 0 {
				fake.Advance(scanCreateWait)
			}
			return scan{}, false
		})
		if got.err != nil || len(got.revisions) != 3 {
			t.Fatalf("the scan whose first create went unanswered = %v, %v; want all 3 claims' revisions", got.revisions, got.err)
		}
		if sent := seam.watched.Load(); sent != 2 {
			t.Fatalf("the scan sent its create %d times, want 2", sent)
		}
		if attempts := unansweredCreateWarnings(logs.String()); !slices.Equal(attempts, []float64{1}) {
			t.Fatalf("the scan logged the unanswered create's WARN for attempts %v, want [1]:\n%s", attempts, logs.String())
		}
	})

	t.Run("a create the server refuses fails at once naming the refusal", func(t *testing.T) {
		seam := &refusedWatchKV{KeyValue: roles.WatchHandle(), refusal: jetstream.ErrMaximumConsumersLimit}
		logs := &lockedBuffer{}
		_, err := roleRevisions(context.Background(), bus.NewKeyValue(roles.Legacy(), seam, roles.Streams()), registry.clock, kvwatch.ScanIdleTimeout, slog.New(slog.NewJSONHandler(logs, nil)), openWait)
		if err == nil || !errors.Is(err, kvwatch.ErrScanNotCreated) || !errors.Is(err, jetstream.ErrMaximumConsumersLimit) ||
			!strings.HasPrefix(err.Error(), "role revisions: create the scan's consumer: ") || strings.Contains(err.Error(), "waited") {
			t.Fatalf("the scan whose create the server refused = %v, want the refusal named after the request, with no wait", err)
		}
		if sent := seam.watched.Load(); sent != 1 {
			t.Fatalf("the scan sent the create the server refused %d times, want once", sent)
		}
		if attempts := unansweredCreateWarnings(logs.String()); len(attempts) != 0 {
			t.Fatalf("a refused create logged the unanswered create's WARN for attempts %v, want none:\n%s", attempts, logs.String())
		}
	})

	t.Run("a create never answered within a deadline of the caller's fails naming it", func(t *testing.T) {
		seam := &unansweredFirstWatchKV{KeyValue: roles.WatchHandle(), fails: 1 << 20}
		ended, cancel := context.WithDeadline(context.Background(), time.Now())
		defer cancel()
		_, err := roleRevisions(ended, bus.NewKeyValue(roles.Legacy(), seam, roles.Streams()), registry.clock, kvwatch.ScanIdleTimeout, slog.Default(), openWait)
		want := "role revisions: create the scan's consumer: create the watcher: context deadline exceeded (waited 10s)"
		if err == nil || err.Error() != want || !errors.Is(err, kvwatch.ErrScanNotCreated) {
			t.Fatalf("the scan whose create never answered within its caller's deadline = %v, want %q", err, want)
		}
		if sent := seam.watched.Load(); sent != 1 {
			t.Fatalf("the scan sent its create %d times within a deadline that had ended, want once", sent)
		}
	})
}

// A server that leaves the role-revision scan's consumer create unanswered past the 10 s an open
// used to give it, as a server on a loaded machine answers late, does not fail the scan: each
// create it leaves unanswered is a WARN naming the attempt, the scan is made again, and once the
// server answers, the scan reads every claim's revision, exactly what a first create the server
// answered would have read. A scan that gave up at the 10 s failed the open, and with it the
// listener's start: "role revisions: create the scan's consumer: context deadline exceeded (waited
// 10s)" ended INFRA-4240's starvation check. The creates are a seam's (heldScanCreates), which
// holds each one, as a server that has not answered it does, until the scan's clock passes
// nats.go's 5 s wait for it or the row answers it; the row moves that clock 15 s, three creates,
// before it answers.
func TestARoleRevisionScanWhoseConsumerCreateTheServerHoldsPastTenSecondsReadsEveryClaimOnceAnswered(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	registry, err := Open(conn, WithReplicas(1))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	roles := registry.roles()
	want := map[string]uint64{}
	for i := range 3 {
		role := fmt.Sprintf("role-%d", i)
		want[role] = putRoleClaim(t, roles.Legacy(), role, fmt.Sprintf("ses_%d", i))
	}
	fake := clocktest.New(time.Now())
	seam := &heldScanCreates{KeyValue: roles.WatchHandle(), clock: fake, answered: make(chan struct{})}
	logs := &lockedBuffer{}
	type scan struct {
		revisions map[string]uint64
		err       error
	}
	done := make(chan scan, 1)
	go func() {
		revisions, err := roleRevisions(context.Background(), bus.NewKeyValue(roles.Legacy(), seam, roles.Streams()), fake, kvwatch.ScanIdleTimeout, slog.New(slog.NewJSONHandler(logs, nil)), 10*time.Second)
		done <- scan{revisions, err}
	}()
	const unanswered = 3
	for held := 1; held <= unanswered; held++ {
		testwait.Until(t, fmt.Sprintf("the scan to send create %d, which the server holds", held), func() (struct{}, bool) {
			select {
			case got := <-done:
				t.Fatalf("the scan returned %v, %v after the server held %d of its creates for %s, before the server answered any",
					got.revisions, got.err, held-1, time.Duration(held-1)*scanCreateWait)
			default:
			}
			return struct{}{}, seam.held.Load() == int32(held) && len(fake.Armed()) > 0
		})
		// nats.go's wait for this create runs out.
		fake.Advance(scanCreateWait)
	}
	testwait.Until(t, "the scan to send the create the server answers", func() (int32, bool) {
		select {
		case got := <-done:
			t.Fatalf("the scan returned %v, %v after the server held %d of its creates for %s, before the server answered any",
				got.revisions, got.err, unanswered, unanswered*scanCreateWait)
		default:
		}
		held := seam.held.Load()
		return held, held == unanswered+1
	})
	close(seam.answered)
	got := testwait.Receive(t, "the scan the server answered to return", done)
	if got.err != nil {
		t.Fatalf("the scan whose create the server answered after holding %d for %s failed: %v", unanswered, unanswered*scanCreateWait, got.err)
	}
	if !maps.Equal(got.revisions, want) {
		t.Fatalf("the scan the server answered late read revisions %v, want every claim's stored revision %v", got.revisions, want)
	}
	if attempts := unansweredCreateWarnings(logs.String()); !slices.Equal(attempts, []float64{1, 2, 3}) {
		t.Fatalf("the scan logged the unanswered create's WARN for attempts %v, want [1 2 3]:\n%s", attempts, logs.String())
	}
	for _, record := range jsonRecords(logs.String()) {
		if record["level"] == "ERROR" {
			t.Fatalf("a scan the server answered late logged an ERROR: %v", record)
		}
	}
}

// heldScanCreates is a role bucket's watch handle whose watches the server holds unanswered, as a
// loaded server holds a consumer create it has not answered yet: each waits until clock passes
// scanCreateWait, nats.go's own wait for it, and fails with the deadline as nats.go's does, or
// until answered is closed, from when each watch is the bucket's own. held counts the watches made.
type heldScanCreates struct {
	jetstream.KeyValue
	clock    *clocktest.Fake
	answered chan struct{}
	held     atomic.Int32
}

func (k *heldScanCreates) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	k.held.Add(1)
	select {
	case <-k.answered:
		return k.KeyValue.Watch(ctx, keys, opts...)
	default:
	}
	wait := k.clock.NewTimer(scanCreateWait)
	defer wait.Stop()
	select {
	case <-wait.C():
		return nil, fmt.Errorf("create the watcher: %w", context.DeadlineExceeded)
	case <-k.answered:
		return k.KeyValue.Watch(ctx, keys, opts...)
	}
}

// refusedWatchKV is a role bucket's watch handle whose every watch the server refuses with
// refusal, its answer to the consumer create.
type refusedWatchKV struct {
	jetstream.KeyValue
	refusal error
	watched atomic.Int32
}

func (k *refusedWatchKV) Watch(context.Context, string, ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	k.watched.Add(1)
	return nil, fmt.Errorf("create the watcher: %w", k.refusal)
}

// unansweredCreateWarnings is the attempt each WARN in logs names that says the role-revision
// scan's consumer create drew no answer, in the order they were logged.
func unansweredCreateWarnings(logs string) []float64 {
	var attempts []float64
	for _, record := range jsonRecords(logs) {
		if record["level"] != "WARN" || !strings.Contains(fmt.Sprint(record["msg"]), "the scan's consumer create drew no answer") {
			continue
		}
		attempt, _ := record["attempt"].(float64)
		attempts = append(attempts, attempt)
	}
	return attempts
}

// jsonRecords is every JSON record in logs, one a line.
func jsonRecords(logs string) []map[string]any {
	var records []map[string]any
	for line := range strings.SplitSeq(logs, "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) == nil {
			records = append(records, record)
		}
	}
	return records
}
