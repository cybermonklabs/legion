package session

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/kvchunk"
	"github.com/sjawhar/envoy/internal/testnats"
)

// storedPrincipal is the principal the bucket itself holds on sessionID's record, read past the
// store's copy, so a row holds what was written and not only what the copy kept.
func storedPrincipal(t *testing.T, known *KnownSessions, sessionID string) string {
	t.Helper()
	entry, err := known.watch().KV().Get(sessionID)
	if err != nil {
		t.Fatalf("read %s's record from the bucket: %v", sessionID, err)
	}
	var record KnownSession
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		t.Fatalf("decode %s's record %s: %v", sessionID, entry.Value(), err)
	}
	return record.Principal
}

// A record keeps the principal its session last registered as through every write that carries
// none - an interest change, a rewrite of unchanged content past the refresh age, a clean stop -
// and a write that carries another principal replaces it. A registration's own write records the
// registration's principal, none included, since it is the principal of the credential that
// session last registered on (RecordRegistrationWithin).
func TestKnownSessionsKeepThePrincipalAWriteDoesNotCarry(t *testing.T) {
	const (
		sessionID = "ses_principal"
		person    = "ada@example.com"
		service   = "example-fleet"
	)
	clock := time.Now()
	known := openKnown(t, func() time.Time { return clock })
	ctx := context.Background()
	requirePrincipal := func(t *testing.T, after, want string) {
		t.Helper()
		record, ok := known.Get(sessionID)
		if !ok {
			t.Fatalf("after %s the store holds no record of %s", after, sessionID)
		}
		if record.Principal != want {
			t.Errorf("after %s the record's principal is %q, want %q", after, record.Principal, want)
		}
		if stored := storedPrincipal(t, known, sessionID); stored != want {
			t.Errorf("after %s the bucket's record holds principal %q, want %q", after, stored, want)
		}
	}

	if err := known.RecordRegistrationWithin(ctx, KnownSession{SessionID: sessionID, Topics: []string{"notifications.team.first"}, Principal: person}); err != nil {
		t.Fatalf("record the registration: %v", err)
	}
	requirePrincipal(t, "a registration on a person's credential", person)

	t.Run("an interest change, which carries no principal, keeps it", func(t *testing.T) {
		changed := []string{"notifications.team.second"}
		if err := known.RecordWithin(ctx, KnownSession{SessionID: sessionID, Topics: changed}); err != nil {
			t.Fatalf("record the interest change: %v", err)
		}
		requirePrincipal(t, "an interest change", person)
		if record, _ := known.Get(sessionID); !slices.Equal(record.Topics, changed) {
			t.Errorf("the record's topics are %v, want the interest change's %v", record.Topics, changed)
		}
	})

	t.Run("a rewrite of unchanged content past the refresh age keeps it", func(t *testing.T) {
		before, _ := known.Get(sessionID)
		clock = clock.Add(KnownSessionRefreshAge + time.Minute)
		if err := known.RecordWithin(ctx, KnownSession{SessionID: sessionID, Topics: before.Topics}); err != nil {
			t.Fatalf("rewrite the record: %v", err)
		}
		if record, _ := known.Get(sessionID); record.LastSeen != clock.UnixMilli() {
			t.Fatalf("the rewrite left last_seen at %d, want the clock's %d: it was not written", record.LastSeen, clock.UnixMilli())
		}
		requirePrincipal(t, "a rewrite past the refresh age", person)
	})

	t.Run("a clean stop keeps it", func(t *testing.T) {
		clock = clock.Add(time.Minute)
		if err := known.TouchWithin(ctx, sessionID); err != nil {
			t.Fatalf("touch: %v", err)
		}
		requirePrincipal(t, "a clean stop", person)
	})

	t.Run("a write carrying another principal replaces it, inside the refresh age too", func(t *testing.T) {
		before, _ := known.Get(sessionID)
		if err := known.RecordWithin(ctx, KnownSession{SessionID: sessionID, Topics: before.Topics, Principal: service}); err != nil {
			t.Fatalf("record another principal: %v", err)
		}
		requirePrincipal(t, "a write carrying another principal", service)
	})

	t.Run("a registration on a credential that proves no principal records none", func(t *testing.T) {
		before, _ := known.Get(sessionID)
		if err := known.RecordRegistrationWithin(ctx, KnownSession{SessionID: sessionID, Topics: before.Topics}); err != nil {
			t.Fatalf("record the registration: %v", err)
		}
		requirePrincipal(t, "a registration that proves no principal", "")
	})
}

// The handover gate reads a session's principal from the bucket's leader (Read), and the record
// says whether a registration of this build recorded it (PrincipalRecorded): a registration on a
// credential that proves none records none, which is not the same as a record an earlier build
// wrote again without the field, whose principal nobody knows. A write that carries no principal
// keeps the one the bucket's leader holds, not the one this listener's copy holds, so a copy that
// has not applied another listener's registration yet does not write the principal that
// registration replaced back over it.
func TestKnownSessionsReadThePrincipalTheBucketHolds(t *testing.T) {
	const (
		person  = "ada@example.com"
		service = "example-fleet"
	)
	clock := time.Now()
	known := openKnown(t, func() time.Time { return clock })
	ctx := context.Background()
	read := func(t *testing.T, sessionID string) (KnownSession, bool) {
		t.Helper()
		record, found, err := known.Read(ctx, sessionID)
		if err != nil {
			t.Fatalf("read %s from the bucket's leader: %v", sessionID, err)
		}
		return record, found
	}
	// putRaw writes record to the bucket past the store, as another listener - or one of an
	// earlier build - writes it, and answers the revision it stored at.
	putRaw := func(t *testing.T, sessionID string, record any) uint64 {
		t.Helper()
		value, err := json.Marshal(record)
		if err != nil {
			t.Fatalf("encode %s's record: %v", sessionID, err)
		}
		revision, err := known.watch().KV().Put(sessionID, value)
		if err != nil {
			t.Fatalf("put %s's record: %v", sessionID, err)
		}
		return revision
	}

	t.Run("a registration records its principal, none included", func(t *testing.T) {
		const sessionID = "ses_read_none"
		if err := known.RecordRegistrationWithin(ctx, KnownSession{SessionID: sessionID, Topics: []string{"notifications.team.read"}}); err != nil {
			t.Fatalf("record the registration: %v", err)
		}
		record, found := read(t, sessionID)
		if !found || record.Principal != "" || !record.PrincipalRecorded {
			t.Errorf("Read = %+v, %t; want a record naming no principal, recorded", record, found)
		}
		if held, _ := known.Get(sessionID); !held.PrincipalRecorded {
			t.Errorf("the copy holds %+v; want the principal recorded", held)
		}
	})

	t.Run("a record an earlier build wrote names no recorded principal, and a keep leaves it so", func(t *testing.T) {
		const sessionID = "ses_read_earlier_build"
		putRaw(t, sessionID, map[string]any{"session_id": sessionID, "topics": []string{"notifications.team.earlier"}, "last_seen": clock.UnixMilli()})
		record, found := read(t, sessionID)
		if !found || record.PrincipalRecorded {
			t.Errorf("Read = %+v, %t; want a record whose principal is not recorded", record, found)
		}
		if err := known.RecordWithin(ctx, KnownSession{SessionID: sessionID, Topics: []string{"notifications.team.earlier-changed"}}); err != nil {
			t.Fatalf("record the interest change: %v", err)
		}
		if record, _ := read(t, sessionID); record.PrincipalRecorded || record.Principal != "" {
			t.Errorf("after an interest change Read = %+v; want the principal still unrecorded", record)
		}
	})

	t.Run("a keep writes the principal the leader holds over a copy that is behind", func(t *testing.T) {
		const sessionID = "ses_read_stale_copy"
		if err := known.RecordRegistrationWithin(ctx, KnownSession{SessionID: sessionID, Topics: []string{"notifications.team.stale"}, Principal: person}); err != nil {
			t.Fatalf("record the first registration: %v", err)
		}
		// Another listener registers the session as a service; this listener's copy has not
		// applied it, and keeps the person's record behind a fence above that write's revision.
		revision := putRaw(t, sessionID, KnownSession{SessionID: sessionID, Topics: []string{"notifications.team.stale"}, LastSeen: clock.UnixMilli(), Principal: service, PrincipalRecorded: true})
		watcher := known.watch()
		_, epoch := watcher.Handle()
		known.mu.Lock()
		known.records.Put(watcher, epoch, sessionID, newKnownRow(KnownSession{SessionID: sessionID, Topics: []string{"notifications.team.stale"}, LastSeen: clock.UnixMilli(), Principal: person, PrincipalRecorded: true}, kvchunk.Layout{}), revision+1000)
		known.mu.Unlock()
		if held, _ := known.Get(sessionID); held.Principal != person {
			t.Fatalf("the copy holds %q; the test needs it behind, at %q", held.Principal, person)
		}
		if err := known.RecordWithin(ctx, KnownSession{SessionID: sessionID, Topics: []string{"notifications.team.stale-changed"}}); err != nil {
			t.Fatalf("record the interest change: %v", err)
		}
		if stored := storedPrincipal(t, known, sessionID); stored != service {
			t.Errorf("the bucket's record names %q after the interest change; want the later registration's %q", stored, service)
		}
	})

	t.Run("a record past the retention, and a key that holds none, are no record", func(t *testing.T) {
		const sessionID = "ses_read_aged"
		putRaw(t, sessionID, KnownSession{SessionID: sessionID, LastSeen: clock.Add(-KnownSessionRetention - time.Minute).UnixMilli(), Principal: person, PrincipalRecorded: true})
		if record, found := read(t, sessionID); found {
			t.Errorf("Read of a record past the retention = %+v; want none", record)
		}
		if record, found := read(t, "ses_read_never"); found {
			t.Errorf("Read of a session that never registered = %+v; want none", record)
		}
	})

	t.Run("a store whose bucket is not open does not answer", func(t *testing.T) {
		var none *KnownSessions
		if _, _, err := none.Read(ctx, "ses_read_nil"); !errors.Is(err, ErrKnownSessionsUnavailable) {
			t.Errorf("Read on a nil store = %v; want %v", err, ErrKnownSessionsUnavailable)
		}
	})
}

// leaderSilent is a bucket handle whose reads from the stream's leader do not answer
// (bus.LeaderReads), as a leader being elected or cut off does; every other call is the embedded
// handle's.
type leaderSilent struct{ nats.KeyValue }

func (leaderSilent) NewestFromLeader(context.Context, string) (nats.KeyValueEntry, error) {
	return nil, errors.New("the stream's leader did not answer")
}

// writesRefused is a bucket handle that refuses every put, as a bucket that takes no write does;
// its reads are the embedded handle's.
type writesRefused struct{ nats.KeyValue }

func (writesRefused) Put(string, []byte) (uint64, error) {
	return 0, errors.New("the bucket took no write")
}

// behindCopy writes sessionID's record naming bucketPrincipal, recorded, to known's bucket past
// the store, as another listener's registration writes it, and holds known's copy of the record
// at copyPrincipal behind a fence above that write, as a copy whose watcher has not applied it
// holds it. It answers the revision the bucket stored the record at.
func behindCopy(t *testing.T, known *KnownSessions, sessionID string, topics []string, lastSeen int64, bucketPrincipal, copyPrincipal string) uint64 {
	t.Helper()
	value, err := json.Marshal(KnownSession{SessionID: sessionID, Topics: topics, LastSeen: lastSeen, Principal: bucketPrincipal, PrincipalRecorded: true})
	if err != nil {
		t.Fatal(err)
	}
	watcher := known.watch()
	revision, err := watcher.KV().Put(sessionID, value)
	if err != nil {
		t.Fatalf("put %s's record: %v", sessionID, err)
	}
	_, epoch := watcher.Handle()
	known.mu.Lock()
	known.records.Put(watcher, epoch, sessionID, newKnownRow(KnownSession{SessionID: sessionID, Topics: topics, LastSeen: lastSeen, Principal: copyPrincipal, PrincipalRecorded: true}, kvchunk.Layout{}), revision+1000)
	known.mu.Unlock()
	if held, _ := known.Get(sessionID); held.Principal != copyPrincipal {
		t.Fatalf("the copy holds %q; the test needs it at %q", held.Principal, copyPrincipal)
	}
	return revision
}

// No write puts a principal back that the bucket's leader did not confirm, and nothing the leader
// did not answer counts as its confirmation. A registration whose record this listener's copy
// holds unchanged is still written where the leader holds another principal, or does not answer,
// since the copy can be behind a registration through another listener; a write that keeps the
// stored principal writes nothing where the leader does not answer, rather than the principal the
// copy names; and a registration whose write failed says so (ErrPrincipalUnrecorded), for the
// registration route's 503, unless the leader confirms it names the registration's own
// principal. Each case fails where the store decides on its copy alone; each control is the same
// write where the copy is not behind, or the leader answers, or confirms the registration's own
// principal.
func TestKnownSessionsWriteNoPrincipalTheLeaderDidNotConfirm(t *testing.T) {
	const (
		person  = "ada@example.com"
		service = "example-fleet"
	)
	clock := time.Now()
	now := func() time.Time { return clock }
	revisionOf := func(t *testing.T, known *KnownSessions, sessionID string) uint64 {
		t.Helper()
		entry, err := known.watch().KV().Get(sessionID)
		if err != nil {
			t.Fatalf("read %s's record from the bucket: %v", sessionID, err)
		}
		return entry.Revision()
	}

	t.Run("case: a registration the copy holds unchanged is written where the leader holds another principal", func(t *testing.T) {
		known := openKnown(t, now)
		const sessionID = "ses_confirm_registration_behind"
		topics := []string{"notifications.team.confirm"}
		behindCopy(t, known, sessionID, topics, clock.UnixMilli(), service, person)
		if err := known.RecordRegistrationWithin(t.Context(), KnownSession{SessionID: sessionID, Topics: topics, Principal: person}); err != nil {
			t.Fatalf("record the registration: %v", err)
		}
		if stored := storedPrincipal(t, known, sessionID); stored != person {
			t.Errorf("the bucket's record names %q after the session registered as %q; want the registration's", stored, person)
		}
	})
	t.Run("control: a registration the copy and the leader hold unchanged is not written", func(t *testing.T) {
		known := openKnown(t, now)
		const sessionID = "ses_confirm_registration_current"
		topics := []string{"notifications.team.confirm"}
		revision := behindCopy(t, known, sessionID, topics, clock.UnixMilli(), person, person)
		if err := known.RecordRegistrationWithin(t.Context(), KnownSession{SessionID: sessionID, Topics: topics, Principal: person}); err != nil {
			t.Fatalf("record the registration: %v", err)
		}
		if got := revisionOf(t, known, sessionID); got != revision {
			t.Errorf("the record is at revision %d after a registration of unchanged content; want it left at %d", got, revision)
		}
	})
	t.Run("case: a registration the copy holds unchanged is written where the leader does not answer", func(t *testing.T) {
		known := openKnown(t, now)
		useKnownKV(t, known, leaderSilent{known.watch().KV().Legacy()})
		const sessionID = "ses_confirm_registration_silent"
		topics := []string{"notifications.team.confirm"}
		behindCopy(t, known, sessionID, topics, clock.UnixMilli(), service, person)
		if err := known.RecordRegistrationWithin(t.Context(), KnownSession{SessionID: sessionID, Topics: topics, Principal: person}); err != nil {
			t.Fatalf("record the registration: %v", err)
		}
		if stored := storedPrincipal(t, known, sessionID); stored != person {
			t.Errorf("the bucket's record names %q after the session registered as %q while the leader was silent; want the registration's", stored, person)
		}
	})

	t.Run("case: a keep whose leader read does not answer writes nothing", func(t *testing.T) {
		known := openKnown(t, now)
		useKnownKV(t, known, leaderSilent{known.watch().KV().Legacy()})
		const sessionID = "ses_confirm_keep_silent"
		behindCopy(t, known, sessionID, []string{"notifications.team.keep"}, clock.UnixMilli(), service, person)
		if err := known.RecordWithin(t.Context(), KnownSession{SessionID: sessionID, Topics: []string{"notifications.team.keep-changed"}}); err == nil {
			t.Errorf("an interest change whose leader read did not answer answered no error; want the read's")
		}
		if stored := storedPrincipal(t, known, sessionID); stored != service {
			t.Errorf("the bucket's record names %q after the interest change; want the later registration's %q, untouched", stored, service)
		}
	})
	t.Run("control: a keep whose leader read answers writes the leader's principal", func(t *testing.T) {
		known := openKnown(t, now)
		const sessionID = "ses_confirm_keep_answered"
		behindCopy(t, known, sessionID, []string{"notifications.team.keep"}, clock.UnixMilli(), service, person)
		if err := known.RecordWithin(t.Context(), KnownSession{SessionID: sessionID, Topics: []string{"notifications.team.keep-changed"}}); err != nil {
			t.Fatalf("record the interest change: %v", err)
		}
		if stored := storedPrincipal(t, known, sessionID); stored != service {
			t.Errorf("the bucket's record names %q after the interest change; want the later registration's %q", stored, service)
		}
	})

	for _, row := range []struct {
		name, sessionID string
		handle          func(nats.KeyValue) nats.KeyValue
		registers       string
		unrecorded      bool
	}{
		{"case: a registration's write fails while the leader names another principal", "ses_confirm_unrecorded_leader",
			func(kv nats.KeyValue) nats.KeyValue { return writesRefused{kv} }, person, true},
		{"case: a registration's write fails while the leader is silent and the copy names another principal", "ses_confirm_unrecorded_copy",
			func(kv nats.KeyValue) nats.KeyValue { return leaderSilent{writesRefused{kv}} }, person, true},
		{"case: a registration's write fails while the leader is silent and the copy names the registration's own principal", "ses_confirm_unrecorded_copy_same",
			func(kv nats.KeyValue) nats.KeyValue { return leaderSilent{writesRefused{kv}} }, service, true},
		{"control: a registration's write fails while the leader names the registration's own principal", "ses_confirm_unrecorded_same",
			func(kv nats.KeyValue) nats.KeyValue { return writesRefused{kv} }, service, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			known := openKnown(t, now)
			sessionID := row.sessionID
			if err := known.RecordRegistrationWithin(t.Context(), KnownSession{SessionID: sessionID, Topics: []string{"notifications.team.first"}, Principal: service}); err != nil {
				t.Fatalf("record the first registration: %v", err)
			}
			useKnownKV(t, known, row.handle(known.watch().KV().Legacy()))
			err := known.RecordRegistrationWithin(t.Context(), KnownSession{SessionID: sessionID, Topics: []string{"notifications.team.second"}, Principal: row.registers})
			if err == nil {
				t.Fatalf("a registration through a bucket that takes no write answered no error")
			}
			if got := errors.Is(err, ErrPrincipalUnrecorded); got != row.unrecorded {
				t.Errorf("the failed registration's error %q is ErrPrincipalUnrecorded: %t; want %t", err, got, row.unrecorded)
			}
		})
	}
}

// A registration through a store whose bucket is not open writes nothing, while the bucket another
// listener's store opened goes on naming the principal the session registered as before, which
// the handover gate reads as the one it last registered as. So that registration is
// ErrPrincipalUnrecorded, for the registration route's 503, wrapping ErrKnownSessionsUnavailable.
// The store that is not open is the one a listener whose NATS user may not open the bucket holds
// (TestAKnownSessionStoreOpensItsDeniedBucketOnceTheGrantArrives). The case fails where such a
// store answers a registration nil, as it answers every other write. The controls are the same
// registration through the open store, which records its principal and answers nil; the writes
// through the store that is not open that are no registration, which answer nil; and a
// registration through a nil store, which answers nil as each of its writes does.
func TestKnownSessionsARegistrationThroughAStoreNotOpenIsUnrecorded(t *testing.T) {
	const (
		sessionID = "ses_registration_not_open"
		person    = "ada@example.com"
		service   = "example-fleet"
	)
	clock := time.Now()
	open := openKnown(t, func() time.Time { return clock })
	if err := open.RecordRegistrationWithin(t.Context(), KnownSession{SessionID: sessionID, Principal: service}); err != nil {
		t.Fatalf("record the registration as %s through the open store: %v", service, err)
	}
	seed, public := testnats.User(t)
	denied := testnats.StartNkeyGranted(t, public, "nothing.this.store.uses")
	conn, err := testnats.ConnectSeed(t, denied.URL, seed)
	if err != nil {
		t.Fatalf("connect as the user whose grant does not carry the bucket: %v", err)
	}
	t.Cleanup(conn.Close)
	// No grant ever arrives, and the retry's first wait is past the test's end, so no attempt the
	// store makes on its own runs while the rows do.
	notOpen, err := OpenKnownSessions(t.Context(), conn, WithKnownSessionReplicas(1), withKnownSessionRetry(time.Hour, time.Hour))
	if err != nil {
		t.Fatalf("open the store over a bucket its user may not open: %v", err)
	}
	t.Cleanup(notOpen.StopWatch)
	if notOpen.watch() != nil {
		t.Fatal("the store the case registers through has its bucket open; the case needs it not open")
	}

	t.Run("case: a registration as another principal through the store whose bucket is not open", func(t *testing.T) {
		err := notOpen.RecordRegistrationWithin(t.Context(), KnownSession{SessionID: sessionID, Principal: person})
		if !errors.Is(err, ErrPrincipalUnrecorded) || !errors.Is(err, ErrKnownSessionsUnavailable) {
			t.Errorf("the registration answered %v; want %v wrapping %v", err, ErrPrincipalUnrecorded, ErrKnownSessionsUnavailable)
		}
		if stored := storedPrincipal(t, open, sessionID); stored != service {
			t.Fatalf("the bucket's record names %q; the case needs it still naming %q", stored, service)
		}
	})
	t.Run("control: a write through the store whose bucket is not open that is no registration", func(t *testing.T) {
		if err := notOpen.RecordWithin(t.Context(), KnownSession{SessionID: sessionID}); err != nil {
			t.Errorf("RecordWithin answered %v; want nil", err)
		}
		if err := notOpen.TouchWithin(t.Context(), sessionID); err != nil {
			t.Errorf("TouchWithin answered %v; want nil", err)
		}
	})
	t.Run("control: a registration through a nil store", func(t *testing.T) {
		var none *KnownSessions
		if err := none.RecordRegistrationWithin(t.Context(), KnownSession{SessionID: sessionID, Principal: person}); err != nil {
			t.Errorf("the registration answered %v; want nil", err)
		}
	})
	t.Run("control: the same registration through the open store", func(t *testing.T) {
		if err := open.RecordRegistrationWithin(t.Context(), KnownSession{SessionID: sessionID, Principal: person}); err != nil {
			t.Errorf("the registration answered %v; want nil", err)
		}
		if stored := storedPrincipal(t, open, sessionID); stored != person {
			t.Errorf("the bucket's record names %q; want %q", stored, person)
		}
	})
}
