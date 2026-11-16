package session

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// withoutReassert is the registry's cache with the write-back left out: a read of a recreated
// bucket begins exactly as the registry's own does (beginRead) and writes nothing back. It is the
// control of the tests below: the same schedule, run through it, must lose the record.
type withoutReassert struct{ sessionCache }

func (c withoutReassert) BeginRead(read kvwatch.Read) { c.r.beginRead(read) }

// deafWatch is a bucket's jetstream handle whose watches leave out every record of key while the
// test has them deaf to it: the delivery of a write that lags its ack, so the copy learns of that
// write only from the write's own result. Every other record, and each scan's end, is forwarded.
type deafWatch struct {
	jetstream.KeyValue
	key  string
	deaf atomic.Bool
}

func (k *deafWatch) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	watcher, err := k.KeyValue.Watch(ctx, keys, opts...)
	if err != nil {
		return watcher, err
	}
	updates := make(chan jetstream.KeyValueEntry)
	go func() {
		defer close(updates)
		for entry := range watcher.Updates() {
			if entry != nil && entry.Key() == k.key && k.deaf.Load() {
				continue
			}
			updates <- entry
		}
	}()
	return &forwardedKeyWatcher{KeyWatcher: watcher, updates: updates}, nil
}

// reassertRig is a session registry whose watcher the test drives: it finds a recreated bucket
// only when the test rewatches it (no stream probe), its writes go through a handle whose answers
// the test can hold (delayingKV), its first watcher can be made deaf to ses_target (deafWatch),
// and its watcher's lines are kept (logs). A Rewatch opens a handle of its own, which is neither.
type reassertRig struct {
	client  *bus.Client
	r       *SessionRegistry
	delayed *delayingKV
	watch   *deafWatch
	logs    *logtest.Buffer
	now     *clock
}

func newReassertRig(t *testing.T, cache func(*SessionRegistry) kvwatch.Cache) *reassertRig {
	t.Helper()
	client := setupNATS(t)
	now := &clock{}
	r, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(time.Minute), WithSessionClock(now.Now))
	if err != nil {
		t.Fatalf("open session registry: %v", err)
	}
	waitFor(t, 5*time.Second, r.watcher.Following)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r.watcher.End(ctx)
	base := r.watcher.KV()
	rig := &reassertRig{
		client:  client,
		r:       r,
		delayed: &delayingKV{KeyValue: base.Legacy()},
		watch:   &deafWatch{KeyValue: base.WatchHandle(), key: "ses_target"},
		logs:    &logtest.Buffer{},
		now:     now,
	}
	rebuilt := kvwatch.New("session registry", bus.NewKeyValue(rig.delayed, rig.watch, base.Streams()), cache(r),
		kvwatch.WithStreamProbeInterval(0), kvwatch.WithLogger(slog.New(slog.NewJSONHandler(rig.logs, nil))))
	r.mu.Lock()
	r.watcher = rebuilt
	r.mu.Unlock()
	t.Cleanup(r.StopWatch)
	rebuilt.Start()
	waitFor(t, 5*time.Second, rebuilt.Following)
	return rig
}

// recreate deletes the session bucket and creates it again, empty, as an operator's delete and a
// listener's start do, and returns a handle on the new bucket.
func (rig *reassertRig) recreate(t *testing.T) natsgo.KeyValue {
	t.Helper()
	js, err := rig.client.Conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := js.DeleteKeyValue(bus.SessionBucket); err != nil {
		t.Fatalf("delete the session bucket: %v", err)
	}
	return testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: bus.SessionBucket, TTL: time.Minute})
}

// readNewBucket rewatches the registry onto the recreated bucket, which installs the first read of
// a new epoch, and waits until that read has completed.
func (rig *reassertRig) readNewBucket(t *testing.T) {
	t.Helper()
	if err := rig.r.Rewatch(rig.client.Conn); err != nil {
		t.Fatalf("Rewatch: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return rig.r.watcher.Epoch() == 1 && rig.r.watcher.Following() })
}

// stored is the title the bucket kv reaches holds for sessionID, and false when it holds no record.
func stored(t *testing.T, kv natsgo.KeyValue, sessionID string) (string, bool) {
	t.Helper()
	entry, err := kv.Get(sessionID)
	if errors.Is(err, natsgo.ErrKeyNotFound) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read %s from the bucket: %v", sessionID, err)
	}
	var row SessionEntry
	if err := json.Unmarshal(entry.Value(), &row); err != nil {
		t.Fatalf("decode %s: %v", sessionID, err)
	}
	return row.Title, true
}

// reassertLines is every line the watcher logged for a re-assert, decoded.
func reassertLines(t *testing.T, logs *logtest.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(logs.String()))
	for scanner.Scan() {
		var line map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("decode log line %q: %v", scanner.Text(), err)
		}
		if msg, _ := line["msg"].(string); strings.Contains(msg, "re-asserted what it held") {
			lines = append(lines, line)
		}
	}
	return lines
}

// refreshUntilAcked starts a registration of sessionID titled title and returns once the bucket
// has stored it, its answer held at the handle (delayingKV): the moment Prove4238's case (b) holds
// role-set's session refresh at, its publish stored by the stream and its ack not yet delivered.
func (rig *reassertRig) refreshUntilAcked(t *testing.T, sessionID, title string) (*gate, <-chan error) {
	t.Helper()
	held := rig.delayed.delay(t, &rig.delayed.puts, sessionID)
	done := make(chan error, 1)
	go func() { done <- rig.r.Put(sessionID, SessionEntry{MachineID: "m1", Title: title}) }()
	waitClosed(t, held.reached, "the bucket to store the refresh")
	return held, done
}

// Prove4238's case (b), in a forced order: a registration the bucket stored and acknowledged, and
// then the bucket deleted and created again before any read of it saw the record. The write was
// answered as landed, and the deleted bucket was the only one that held it. The read that finds the
// new bucket writes back what the copy held, the registration among it, so the new bucket holds it
// once that read has completed: the session is in the bucket, not only in this listener's copy.
//
// A row the copy holds for a gone session is left out (beginRead): the reaper would delete it. The
// re-assert logs one line, with what it held and wrote.
//
// At main the new bucket holds no record of the session, and the completed read removes it from
// the copy too. The control below runs the same schedule with the write-back left out.
func TestARegistrationOnlyTheDeletedBucketAckedIsInTheNewBucketAfterTheReassert(t *testing.T) {
	rig := newReassertRig(t, func(r *SessionRegistry) kvwatch.Cache { return sessionCache{r} })
	if err := rig.r.Put("ses_target", SessionEntry{MachineID: "m1", Title: "registered"}); err != nil {
		t.Fatalf("register ses_target: %v", err)
	}
	putRaw(t, rig.delayed.KeyValue, "ses_gone", SessionEntry{MachineID: "m1", Title: "gone", UpdatedAt: rig.now.Now().Add(-2 * GoneHorizon).UnixMilli()})
	waitFor(t, 5*time.Second, func() bool { _, state := rig.r.Lookup("ses_gone"); return state == Gone && rig.r.CacheSize() == 2 })
	rig.now.advance(time.Second)

	held, done := rig.refreshUntilAcked(t, "ses_target", "refreshed")
	recreated := rig.recreate(t)
	held.open()
	if err := <-done; err != nil {
		t.Fatalf("the refresh the deleted bucket acknowledged: %v", err)
	}
	if _, ok := stored(t, recreated, "ses_target"); ok {
		t.Fatal("the new bucket holds ses_target before any read of it: the schedule did not lose the record")
	}

	rig.readNewBucket(t)
	if title, ok := stored(t, recreated, "ses_target"); !ok || title != "refreshed" {
		t.Fatalf("the new bucket holds ses_target = %q (present %v) after the read that found it, want the acknowledged refresh", title, ok)
	}
	if got := titleOf(t, rig.r, "ses_target"); got != "refreshed" {
		t.Fatalf("the copy holds ses_target = %q, want the refresh", got)
	}
	if _, ok := stored(t, recreated, "ses_gone"); ok {
		t.Fatal("the new bucket holds ses_gone, a gone session's row the reaper would delete")
	}
	lines := reassertLines(t, rig.logs)
	if len(lines) != 1 {
		t.Fatalf("logged %d re-assert lines, want one per recreate:\n%s", len(lines), rig.logs.String())
	}
	if lines[0]["held"] != float64(1) || lines[0]["written"] != float64(1) || lines[0]["failed"] != float64(0) {
		t.Fatalf("the re-assert line = %v, want held 1, written 1, failed 0", lines[0])
	}
}

// The control: the same schedule through a cache that writes nothing back loses the registration.
// It shows the check above can fail, and fails for the reason the fix is for: nothing put the
// record into the new bucket, and the completed read takes it out of the copy.
func TestControlARegistrationOnlyTheDeletedBucketAckedIsLostWithoutTheReassert(t *testing.T) {
	rig := newReassertRig(t, func(r *SessionRegistry) kvwatch.Cache { return withoutReassert{sessionCache{r}} })
	if err := rig.r.Put("ses_target", SessionEntry{MachineID: "m1", Title: "registered"}); err != nil {
		t.Fatalf("register ses_target: %v", err)
	}
	rig.now.advance(time.Second)
	held, done := rig.refreshUntilAcked(t, "ses_target", "refreshed")
	recreated := rig.recreate(t)
	held.open()
	if err := <-done; err != nil {
		t.Fatalf("the refresh the deleted bucket acknowledged: %v", err)
	}

	rig.readNewBucket(t)
	if title, ok := stored(t, recreated, "ses_target"); ok {
		t.Fatalf("the new bucket holds ses_target = %q with the write-back left out, want it missing", title)
	}
	if _, err := rig.r.Get("ses_target"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("Get(ses_target) with the write-back left out = %v, want not found", err)
	}
	if lines := reassertLines(t, rig.logs); len(lines) != 0 {
		t.Fatalf("logged %d re-assert lines with the write-back left out, want none", len(lines))
	}
}

// The same lost write with its ack held until after the read that found the new bucket began, and
// its delivery to this listener's watcher lagging past the delete: the copy drops the write's
// result, since the epoch it was sent in is gone (kvwatch.Dropped), so it is not among the rows
// that read wrote back, which hold the registration before it. The write is sent again into the
// new bucket over that older row (putAgainWithin), so the new bucket ends with the write its
// caller was told landed. The state before the late ack is the control: the new bucket holds the
// older row, which is what it would keep without the write sent again.
func TestARegistrationAckedAfterTheNewReadBeganIsWrittenAgainOverTheOlderRow(t *testing.T) {
	rig := newReassertRig(t, func(r *SessionRegistry) kvwatch.Cache { return sessionCache{r} })
	if err := rig.r.Put("ses_target", SessionEntry{MachineID: "m1", Title: "registered"}); err != nil {
		t.Fatalf("register ses_target: %v", err)
	}
	rig.now.advance(time.Second)
	rig.watch.deaf.Store(true)
	held, done := rig.refreshUntilAcked(t, "ses_target", "refreshed")
	recreated := rig.recreate(t)
	rig.readNewBucket(t)
	if title, ok := stored(t, recreated, "ses_target"); !ok || title != "registered" {
		t.Fatalf("before the late ack the new bucket holds ses_target = %q (present %v), want the row the copy held, written back", title, ok)
	}

	held.open()
	if err := <-done; err != nil {
		t.Fatalf("the refresh acknowledged after the new read began: %v", err)
	}
	if title, ok := stored(t, recreated, "ses_target"); !ok || title != "refreshed" {
		t.Fatalf("the new bucket holds ses_target = %q (present %v) after the late ack, want the refresh written again", title, ok)
	}
	waitFor(t, 5*time.Second, func() bool {
		entry, err := rig.r.Get("ses_target")
		return err == nil && entry.Title == "refreshed"
	})
}

// A write sent again never goes over a newer one: a registration made in the new bucket after the
// recreate, stamped later than the late-acked write, stands.
func TestAWriteSentAgainLeavesANewerRegistrationStanding(t *testing.T) {
	rig := newReassertRig(t, func(r *SessionRegistry) kvwatch.Cache { return sessionCache{r} })
	if err := rig.r.Put("ses_target", SessionEntry{MachineID: "m1", Title: "registered"}); err != nil {
		t.Fatalf("register ses_target: %v", err)
	}
	rig.now.advance(time.Second)
	rig.watch.deaf.Store(true)
	held, done := rig.refreshUntilAcked(t, "ses_target", "refreshed")
	recreated := rig.recreate(t)
	rig.readNewBucket(t)
	putRaw(t, recreated, "ses_target", SessionEntry{MachineID: "m2", Title: "newer", UpdatedAt: rig.now.Now().Add(time.Minute).UnixMilli()})

	held.open()
	if err := <-done; err != nil {
		t.Fatalf("the refresh acknowledged after the new read began: %v", err)
	}
	if title, _ := stored(t, recreated, "ses_target"); title != "newer" {
		t.Fatalf("the new bucket holds ses_target = %q, want the newer registration left standing", title)
	}
}

// A session that ended in the new bucket before this listener found it stays ended. Its
// deregistration went through another listener that found no row in the new bucket, so it marked
// the key (bus.KeyValue.MarkDeletedIfNone); this listener's copy still held the row, and its
// write-back is create-only, so the marker refuses it. The control half shows the write-back would
// otherwise bring the session back.
func TestASessionThatEndedInTheNewBucketIsNotWrittenBack(t *testing.T) {
	for _, ended := range []bool{true, false} {
		name := "deregistered through another listener"
		if !ended {
			name = "control: not deregistered"
		}
		t.Run(name, func(t *testing.T) {
			rig := newReassertRig(t, func(r *SessionRegistry) kvwatch.Cache { return sessionCache{r} })
			other, err := OpenSessionRegistry(rig.client.Conn, WithSessionReplicas(1), WithSessionTTL(time.Minute))
			if err != nil {
				t.Fatalf("open the other listener's registry: %v", err)
			}
			t.Cleanup(other.StopWatch)
			if err := rig.r.Put("ses_ended", SessionEntry{MachineID: "m1", Title: "registered"}); err != nil {
				t.Fatalf("register ses_ended: %v", err)
			}
			waitFor(t, 5*time.Second, func() bool { _, err := other.Get("ses_ended"); return err == nil })
			recreated := rig.recreate(t)
			if ended {
				if err := other.Delete(context.Background(), "ses_ended"); err != nil {
					t.Fatalf("deregister ses_ended through the other listener: %v", err)
				}
			}

			rig.readNewBucket(t)
			_, present := stored(t, recreated, "ses_ended")
			if present == ended {
				t.Fatalf("the new bucket holds ses_ended: %v, want %v", present, !ended)
			}
			if _, err := rig.r.Get("ses_ended"); (err == nil) == ended {
				t.Fatalf("Get(ses_ended) = %v after the read, want present %v", err, !ended)
			}
		})
	}
}

// The known-session store's records come back the same way: a record its copy held when the
// bucket was deleted and created again empty is in the new bucket once the read that finds it
// completes, and a record aged out of KnownSessionRetention, which the bucket's TTL would already
// have dropped, is left out.
func TestARecreatedKnownSessionBucketGetsBackTheRecordsTheCopyHeld(t *testing.T) {
	client := setupNATS(t)
	now := &clock{}
	known, err := OpenKnownSessions(client.Conn, WithKnownSessionReplicas(1), WithKnownSessionClock(now.Now),
		withKnownSessionWatch(kvwatch.WithStreamProbeInterval(0)))
	if err != nil {
		t.Fatalf("open the known-session store: %v", err)
	}
	t.Cleanup(known.StopWatch)
	waitFor(t, 10*time.Second, func() bool { w := known.watch(); return w != nil && w.Following() })
	now.advance(-KnownSessionRetention - time.Hour)
	if err := known.RecordWithin(context.Background(), KnownSession{SessionID: "ses_aged", Topics: []string{"notifications.team.a"}}); err != nil {
		t.Fatalf("record ses_aged: %v", err)
	}
	now.advance(KnownSessionRetention + time.Hour)
	if err := known.RecordWithin(context.Background(), KnownSession{SessionID: "ses_known", Topics: []string{"notifications.team.a"}}); err != nil {
		t.Fatalf("record ses_known: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { _, ok := known.Get("ses_known"); return ok })

	js, err := client.Conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := js.DeleteKeyValue(bus.KnownSessionBucket); err != nil {
		t.Fatalf("delete the known-session bucket: %v", err)
	}
	recreated := testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: bus.KnownSessionBucket, TTL: KnownSessionRetention})
	if err := known.Rewatch(client.Conn); err != nil {
		t.Fatalf("Rewatch: %v", err)
	}
	waitFor(t, 10*time.Second, func() bool { w := known.watch(); return w.Epoch() == 1 && w.Following() })
	if _, err := recreated.Get("ses_known"); err != nil {
		t.Fatalf("the new bucket's record of ses_known: %v, want it written back", err)
	}
	if _, err := recreated.Get("ses_aged"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("the new bucket's record of ses_aged: %v, want none: it aged out before the recreate", err)
	}
	if _, ok := known.Get("ses_known"); !ok {
		t.Fatal("the store answers no record of ses_known after the read of the new bucket")
	}
}
