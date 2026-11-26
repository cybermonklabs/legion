package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
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
	registry, err := store.Open(t.Context(), client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the interest registry: %v", err)
	}
	// Each stamp below reads the role claim view (senderStamp), so the listener's own startup
	// gate is taken here, before the freeze.
	waitForRegistry(t, registry)
	sessions, err := session.OpenSessionRegistry(t.Context(), client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	waitForSessions(t, sessions)
	interestBucket, err := client.JS().KeyValue(bus.InterestBucket)
	if err != nil {
		t.Fatalf("open the interest bucket: %v", err)
	}

	first := session.SessionEntry{SelfSubscribed: true, MachineID: "listener-a", Dir: "/work/a", Title: title}
	current := session.SessionEntry{SelfSubscribed: true, MachineID: "listener-b", Dir: "/work/b", Title: title}
	// register writes sessionID's entry and requires this listener's session cache to hold it, so
	// no stamp below reads the bus for its title or its fallback.
	register := func(t *testing.T, sessionID string, entry session.SessionEntry) {
		t.Helper()
		if err := sessions.Put(sessionID, entry); err != nil {
			t.Fatalf("write %s's session entry: %v", sessionID, err)
		}
		if _, err := sessions.Get(sessionID); err != nil {
			t.Fatalf("the listener's session cache does not hold %s (%v): its stamp would read the bus for the entry", sessionID, err)
		}
	}
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
		if _, err := interestBucket.Put(malformed, []byte("not a json interest record")); err != nil {
			t.Fatalf("write %s's unreadable interest record: %v", malformed, err)
		}
		if _, err := registry.Get(context.Background(), malformed); !errors.Is(err, store.ErrMalformedInterest) {
			t.Fatalf("the interest read of %s answers %v, want %v: this row is a record this build cannot decode", malformed, err, store.ErrMalformedInterest)
		}
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
