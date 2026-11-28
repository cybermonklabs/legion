package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A message from a registered session names the machine and directory of the registration this
// listener holds for it now, whether or not the stamp's read of its interest record answers
// (senderStamp).
//
// Where that read answers, the record's machine and directory are the stamp's. Each registration
// moves the record to the listener and directory it came from (internal/store's mergeForUpsert),
// so a session that registered through listener A and then through listener B is stamped with B:
// that control shows the rig moves a registration and the stamp reads it. A second control holds
// a record and an entry that disagree, and the record's values are the stamp's, so a stamp that
// preferred the entry over a record it could read would fail here.
//
// Where the read finds no record, answers a record this build cannot decode, or meets a bus that
// does not answer it, the stamp takes the two fields from the session's entry instead: the same
// call reads that entry for the title, and every registration rewrites it. The stamp still names
// the session and its title, and on a bus that does not answer it still returns within the one
// read window the route allots it (senderStampReadTimeout). The bus is frozen by the relay, which
// discards every byte the client writes once armed and keeps the connection open, so it is the
// last row.
func TestSenderStampNamesTheCurrentRegistration(t *testing.T) {
	// Not parallel: it requires the stamp on a frozen bus within 1.5 s, half a second past its
	// 1 s read window (senderStampReadTimeout plus margin).
	const (
		moved     = "ses_stamp_registered_through_a_then_b"
		disagrees = "ses_stamp_whose_record_and_entry_disagree"
		noRow     = "ses_stamp_with_no_interest_record"
		malformed = "ses_stamp_whose_interest_record_this_build_cannot_decode"
		frozen    = "ses_stamp_whose_interest_read_does_not_answer"
		title     = "the sender's title"
		// margin is what the frozen row allows past the read window: the stamp's other reads are
		// in-process (the session cache and the role claim view), so it is scheduling alone.
		margin = 500 * time.Millisecond
	)
	relay := startListenerRelay(t, testnats.URL(t))
	client, err := bus.ConnectOwningStream([]string{relay.relayed}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener's bus through the relay: %v", err)
	}
	t.Cleanup(client.Close)
	// Each stamp below reads the role claim view (senderStamp), so the listener's own startup gate
	// is taken before the freeze (openStampRig).
	rig := openStampRig(t, client)
	registry, sessions := rig.registry, rig.sessions

	first := session.SessionEntry{SelfSubscribed: true, MachineID: "listener-a", Dir: "/work/a", Title: title}
	current := session.SessionEntry{SelfSubscribed: true, MachineID: "listener-b", Dir: "/work/b", Title: title}
	register := rig.register
	// recordThrough writes sessionID's interest record through each registration in turn and
	// waits until the interest cache holds the last, so the stamp reads it from the cache.
	recordThrough := func(t *testing.T, sessionID string, registrations ...session.SessionEntry) {
		t.Helper()
		for _, through := range registrations {
			if _, err := registry.Upsert(store.Interest{SessionID: sessionID, MachineID: through.MachineID, Dir: through.Dir}, []string{contracts.LegacyAgentSubject(sessionID)}); err != nil {
				t.Fatalf("register %s through %s: %v", sessionID, through.MachineID, err)
			}
		}
		last := registrations[len(registrations)-1]
		waitFor(t, "the interest cache to hold "+sessionID+"'s record through "+last.MachineID, func() bool {
			row, held := registry.Cached(sessionID)
			return held && row.MachineID == last.MachineID && row.Dir == last.Dir
		})
	}
	requireStamp := func(t *testing.T, sessionID string, stamp *contracts.EnvelopeSender, want session.SessionEntry, names string) {
		t.Helper()
		if stamp == nil {
			t.Fatalf("stamp of %s is nil, want the session itself", sessionID)
		}
		t.Logf("stamp of %s: session %q title %q machine %q cwd %q", sessionID, stamp.SessionID, stamp.Title, stamp.Machine, stamp.Cwd)
		if stamp.SessionID != sessionID || stamp.Title != title {
			t.Errorf("stamp names session %q with title %q, want %s with %q", stamp.SessionID, stamp.Title, sessionID, title)
		}
		if stamp.Machine != want.MachineID || stamp.Cwd != want.Dir {
			t.Errorf("stamp names machine %q cwd %q, want %s %s: %s", stamp.Machine, stamp.Cwd, want.MachineID, want.Dir, names)
		}
	}

	t.Run("control: a session registered through listener A, then B", func(t *testing.T) {
		register(t, moved, current)
		recordThrough(t, moved, first, current)
		requireStamp(t, moved, senderStamp(context.Background(), registry, sessions, moved), current, "the current registration")
	})

	t.Run("control: the interest record answers and the session entry names another registration", func(t *testing.T) {
		register(t, disagrees, current)
		recordThrough(t, disagrees, first)
		requireStamp(t, disagrees, senderStamp(context.Background(), registry, sessions, disagrees), first, "the interest record's, which decides wherever its read answers")
	})

	t.Run("case: the interest read finds no record for a registered session", func(t *testing.T) {
		register(t, noRow, current)
		if _, err := registry.Get(context.Background(), noRow); !errors.Is(err, nats.ErrKeyNotFound) {
			t.Fatalf("the interest read of %s answers %v, want %v: this row is a read that finds no record", noRow, err, nats.ErrKeyNotFound)
		}
		requireStamp(t, noRow, senderStamp(context.Background(), registry, sessions, noRow), current, "the session entry's, the current registration")
	})

	t.Run("case: the interest read answers a record this build cannot decode", func(t *testing.T) {
		register(t, malformed, current)
		rig.undecodable(t, malformed)
		requireStamp(t, malformed, senderStamp(context.Background(), registry, sessions, malformed), current, "the session entry's, the current registration")
	})

	t.Run("case: the bus does not answer the interest read", func(t *testing.T) {
		register(t, frozen, current)
		if _, held := registry.Cached(frozen); held {
			t.Fatalf("the interest cache holds %s: its stamp would not read the bus", frozen)
		}
		// The next protocol line the client writes arms the swallow; every byte after it is
		// discarded and the connection stays open.
		relay.arm("\r\n", 1)
		started := time.Now()
		stamp := senderStamp(context.Background(), registry, sessions, frozen)
		elapsed := time.Since(started)
		t.Logf("stamp of %s returned after %s", frozen, elapsed.Round(time.Millisecond))
		if elapsed < senderStampReadTimeout {
			t.Fatalf("the stamp of %s returned after %s, inside its read window (%s): it waited on no interest read of the frozen bus, so this row does not test a read the bus does not answer", frozen, elapsed, senderStampReadTimeout)
		}
		if elapsed > senderStampReadTimeout+margin {
			t.Errorf("the stamp of %s took %s, want at most %s: one read window and the margin", frozen, elapsed, senderStampReadTimeout+margin)
		}
		requireStamp(t, frozen, stamp, current, "the session entry's, the current registration")
	})
}

// The stamp reads the sender's interest record before its session entry, because the two reads
// share one window (senderStampReadTimeout) and the record decides the machine and directory
// wherever its read answers: a session-entry read that went to the bucket first could spend that
// window and leave an interest read that would have answered nothing to start with (senderStamp).
// Neither cache holds the sender here. Its interest record is in the bucket, so that read answers;
// every read of the session bucket stalls, as a session bucket whose direct get has stopped
// answering does. The stamp must name the record's machine and directory, and it returns only once
// the stalled entry read has spent the window, which shows the entry read went to the bucket.
func TestTheSenderStampReadsTheInterestRecordBeforeTheSessionEntry(t *testing.T) {
	t.Parallel()
	const (
		sender  = "ses_stamp_read_order_sender"
		machine = "interest-machine"
		dir     = "/work/interest"
	)
	registry, sessions := setupSessionsTest(t, nil, nil)
	endCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	registry.EndWatch(endCtx)
	sessions.EndWatch(endCtx)
	cancel()
	writer, err := store.Open(t.Context(), setupPublishTestClient(t).Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the writing listener's registry: %v", err)
	}
	if _, err := writer.Upsert(store.Interest{SessionID: sender, MachineID: machine, Dir: dir}, []string{contracts.LegacyAgentSubject(sender)}); err != nil {
		t.Fatalf("write %s's interest record: %v", sender, err)
	}
	if _, held := registry.Cached(sender); held {
		t.Fatalf("the listener's interest cache holds %s: its interest read would not go to the bucket", sender)
	}
	if _, err := sessions.Get(sender); !errors.Is(err, nats.ErrKeyNotFound) {
		t.Fatalf("the listener's session cache answers %s with %v: its entry read would not go to the bucket", sender, err)
	}
	stallSessionReads(t, sessions)

	started := time.Now()
	stamp := senderStamp(context.Background(), registry, sessions, sender)
	elapsed := time.Since(started)
	if stamp == nil {
		t.Fatalf("stamp of %s is nil, want the sender itself", sender)
	}
	t.Logf("stamp of %s after %s: session %q machine %q cwd %q", sender, elapsed.Round(time.Millisecond), stamp.SessionID, stamp.Machine, stamp.Cwd)
	if elapsed < senderStampReadTimeout {
		t.Fatalf("the stamp of %s returned after %s, inside its read window (%s): no entry read stalled on the bucket, so the order of the reads is not under test", sender, elapsed, senderStampReadTimeout)
	}
	if stamp.SessionID != sender || stamp.Machine != machine || stamp.Cwd != dir {
		t.Errorf("stamp names session %q machine %q cwd %q, want %s on %s in %s: the interest record, read before the stalled entry read", stamp.SessionID, stamp.Machine, stamp.Cwd, sender, machine, dir)
	}
}

// A stamp whose read-through did not answer within its bound still goes out with what it knows
// (senderStamp), and says nothing of why its machine, directory or title is missing: the
// registry's logger says so at WARN, naming the session, the bound and the read's error. A read
// that answered writes no line: a cache hit makes no read, a sender neither bucket holds a row of
// is answered nats.ErrKeyNotFound, and a record this build cannot decode is an answer about the
// record, which the registry logs on its own line (interestReadAnswered). The rows whose read must
// not answer end the window before the stamp starts, which bus.Bounded answers as a read the
// window ended without starting it; the last row's session bucket fails the read instead, the bus
// failing it rather than the window ending. The first row ends the window too, with both caches
// holding the sender, so an ended window alone is shown to write no line.
func TestASenderStampLogsAtWarnTheReadThroughThatDidNotAnswer(t *testing.T) {
	t.Parallel()
	const (
		stampLine          = "listener sender stamp left out what a read-through did not answer within its bound"
		cached             = "ses_stamp_warn_both_cached"
		unknown            = "ses_stamp_warn_no_row"
		malformed          = "ses_stamp_warn_undecodable_record"
		interestUnanswered = "ses_stamp_warn_interest_unanswered"
		titleUnanswered    = "ses_stamp_warn_title_unanswered"
		titleFailed        = "ses_stamp_warn_title_read_failed"
		title              = "the sender's title"
	)
	logs := &lockedBuffer{}
	rig := openStampRig(t, setupPublishTestClient(t), store.WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
	registry, sessions := rig.registry, rig.sessions

	entry := session.SessionEntry{SelfSubscribed: true, MachineID: "entry-machine", Dir: "/work/entry", Title: title}
	register := func(t *testing.T, sessionID string) {
		t.Helper()
		rig.register(t, sessionID, entry)
	}
	// record writes sessionID's interest record and waits until the interest cache holds it, so
	// the stamp reads no bucket for its machine and directory.
	record := func(t *testing.T, sessionID string) {
		t.Helper()
		if _, err := registry.Upsert(store.Interest{SessionID: sessionID, MachineID: "interest-machine", Dir: "/work/interest"},
			[]string{contracts.LegacyAgentSubject(sessionID)}); err != nil {
			t.Fatalf("write %s's interest record: %v", sessionID, err)
		}
		testwait.Eventually(t, "the interest cache to hold "+sessionID+"'s record", func() bool {
			_, held := registry.Cached(sessionID)
			return held
		})
	}
	uncachedInterest := func(t *testing.T, sessionID string) {
		t.Helper()
		if _, held := registry.Cached(sessionID); held {
			t.Fatalf("the interest cache holds %s: its stamp would read no bucket for the record", sessionID)
		}
	}
	uncachedEntry := func(t *testing.T, sessionID string) {
		t.Helper()
		if _, err := sessions.Get(sessionID); !errors.Is(err, nats.ErrKeyNotFound) {
			t.Fatalf("the session cache answers %s with %v, want %v: its stamp would read no bucket for the title", sessionID, err, nats.ErrKeyNotFound)
		}
	}
	// ended is a stamp's context whose window has already ended.
	ended, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancel()
	stampLines := func(sessionID string) []map[string]any {
		return linesIn(logs.String(), stampLine, map[string]any{"session_id": sessionID})
	}
	requireNoLine := func(t *testing.T, sessionID, why string) {
		t.Helper()
		if lines := stampLines(sessionID); len(lines) != 0 {
			t.Fatalf("the stamp of %s logged %v, want no line: %s", sessionID, lines, why)
		}
	}
	requireLine := func(t *testing.T, sessionID string, carries ...string) {
		t.Helper()
		lines := stampLines(sessionID)
		if len(lines) != 1 {
			t.Fatalf("the stamp of %s logged %d lines %q, want 1:\n%s", sessionID, len(lines), stampLine, logs.String())
		}
		assertRecordFields(t, lines[0], map[string]any{"level": "WARN", "bound": float64(senderStampReadTimeout)})
		for _, want := range carries {
			assertRecordErrorCarries(t, lines[0], want)
		}
	}

	t.Run("control: both caches hold the sender, and the window has ended", func(t *testing.T) {
		register(t, cached)
		record(t, cached)
		stamp := senderStamp(ended, registry, sessions, cached)
		if stamp == nil || stamp.Machine != "interest-machine" || stamp.Title != title {
			t.Fatalf("stamp of %s is %+v, want the cached record's machine and the cached entry's title", cached, stamp)
		}
		requireNoLine(t, cached, "a cache hit makes no read, so no read went unanswered")
	})

	t.Run("control: neither bucket holds a row of the sender", func(t *testing.T) {
		uncachedInterest(t, unknown)
		uncachedEntry(t, unknown)
		stamp := senderStamp(context.Background(), registry, sessions, unknown)
		if stamp == nil || stamp.SessionID != unknown || stamp.Machine != "" || stamp.Title != "" {
			t.Fatalf("stamp of %s is %+v, want the session alone", unknown, stamp)
		}
		requireNoLine(t, unknown, "both buckets answered that they hold no row")
	})

	t.Run("control: the interest record does not decode", func(t *testing.T) {
		register(t, malformed)
		rig.undecodable(t, malformed)
		stamp := senderStamp(context.Background(), registry, sessions, malformed)
		if stamp == nil || stamp.Machine != entry.MachineID {
			t.Fatalf("stamp of %s is %+v, want the entry's machine %s", malformed, stamp, entry.MachineID)
		}
		requireNoLine(t, malformed, "the bucket answered the record, which this build cannot decode")
	})

	t.Run("case: the interest read does not answer within the window", func(t *testing.T) {
		register(t, interestUnanswered)
		uncachedInterest(t, interestUnanswered)
		stamp := senderStamp(ended, registry, sessions, interestUnanswered)
		if stamp == nil || stamp.Machine != entry.MachineID || stamp.Title != title {
			t.Fatalf("stamp of %s is %+v, want the cached entry's machine and title", interestUnanswered, stamp)
		}
		requireLine(t, interestUnanswered, "interest record", context.DeadlineExceeded.Error())
	})

	t.Run("case: the title read does not answer within the window", func(t *testing.T) {
		record(t, titleUnanswered)
		uncachedEntry(t, titleUnanswered)
		stamp := senderStamp(ended, registry, sessions, titleUnanswered)
		if stamp == nil || stamp.Machine != "interest-machine" || stamp.Title != "" {
			t.Fatalf("stamp of %s is %+v, want the cached record's machine and no title", titleUnanswered, stamp)
		}
		requireLine(t, titleUnanswered, "read session "+titleUnanswered+" from the bucket", context.DeadlineExceeded.Error())
	})

	t.Run("case: the session bucket fails the title read", func(t *testing.T) {
		record(t, titleFailed)
		uncachedEntry(t, titleFailed)
		failure := errors.New("the session bucket's read failed")
		sessions.StopWatch()
		swapSessionBucket(t, sessions, func(kv nats.KeyValue) nats.KeyValue {
			return bucketThatFails{KeyValue: kv, err: failure}
		})
		stamp := senderStamp(context.Background(), registry, sessions, titleFailed)
		if stamp == nil || stamp.Machine != "interest-machine" || stamp.Title != "" {
			t.Fatalf("stamp of %s is %+v, want the cached record's machine and no title", titleFailed, stamp)
		}
		requireLine(t, titleFailed, failure.Error())
	})
}

// stampRig is a listener's interest and session registries over one connection, for a test of the
// stamp a send carries (senderStamp), with the interest bucket a row writes past the registry.
type stampRig struct {
	registry  *store.Registry
	sessions  *session.SessionRegistry
	interests nats.KeyValue
}

// openStampRig opens both registries over client, the interest registry with options, and waits
// for each as the listener's own start does (waitForRegistry, waitForSessions): a stamp reads the
// role claim view, which refuses until it is complete.
func openStampRig(t *testing.T, client *bus.Client, options ...store.OpenOption) stampRig {
	t.Helper()
	registry, err := store.Open(t.Context(), client.Conn, append([]store.OpenOption{store.WithReplicas(1)}, options...)...)
	if err != nil {
		t.Fatalf("open the interest registry: %v", err)
	}
	waitForRegistry(t, registry)
	sessions, err := session.OpenSessionRegistry(t.Context(), client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	waitForSessions(t, sessions)
	interests, err := client.JS().KeyValue(bus.InterestBucket)
	if err != nil {
		t.Fatalf("open the interest bucket: %v", err)
	}
	return stampRig{registry: registry, sessions: sessions, interests: interests}
}

// register writes sessionID's entry and requires this listener's session cache to hold it, so a
// stamp of it reads no bucket for its title or its fallback.
func (r stampRig) register(t *testing.T, sessionID string, entry session.SessionEntry) {
	t.Helper()
	if err := r.sessions.Put(sessionID, entry); err != nil {
		t.Fatalf("write %s's session entry: %v", sessionID, err)
	}
	if _, err := r.sessions.Get(sessionID); err != nil {
		t.Fatalf("the listener's session cache does not hold %s (%v): its stamp would read the bus for the entry", sessionID, err)
	}
}

// undecodable writes sessionID an interest record this build cannot decode, and requires the
// interest read of it to answer store.ErrMalformedInterest.
func (r stampRig) undecodable(t *testing.T, sessionID string) {
	t.Helper()
	if _, err := r.interests.Put(sessionID, []byte("not a json interest record")); err != nil {
		t.Fatalf("write %s's unreadable interest record: %v", sessionID, err)
	}
	if _, err := r.registry.Get(context.Background(), sessionID); !errors.Is(err, store.ErrMalformedInterest) {
		t.Fatalf("the interest read of %s answers %v, want %v: this row is a record this build cannot decode", sessionID, err, store.ErrMalformedInterest)
	}
}
