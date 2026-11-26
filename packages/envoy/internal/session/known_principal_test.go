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
	"github.com/sjawhar/envoy/internal/kvchunk/kvchunktest"
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

// register records record as a registration of its session as the registration route records it:
// admitted first (AdmitRegistration), then written at the admission's revision
// (RecordRegistrationWithin). An admission that fails ends the test; the answer is the write's.
func register(t *testing.T, known *KnownSessions, record KnownSession) error {
	t.Helper()
	admission, err := known.AdmitRegistration(t.Context(), record.SessionID, record.Principal)
	if err != nil {
		t.Fatalf("admit %s's registration as %q: %v", record.SessionID, record.Principal, err)
	}
	return known.RecordRegistrationWithin(t.Context(), admission, record)
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

	if err := register(t, known, KnownSession{SessionID: sessionID, Topics: []string{"notifications.team.first"}, Principal: person}); err != nil {
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
		// As the registration route does: the admission clears the principal of the record naming
		// another, and the registration's write at the admission's revision records its own.
		if err := register(t, known, KnownSession{SessionID: sessionID, Topics: before.Topics}); err != nil {
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
		if err := register(t, known, KnownSession{SessionID: sessionID, Topics: []string{"notifications.team.read"}}); err != nil {
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
		if err := register(t, known, KnownSession{SessionID: sessionID, Topics: []string{"notifications.team.stale"}, Principal: person}); err != nil {
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

// writesRefused is a bucket handle that refuses every write - a put, a create and an update at a
// revision - as a bucket that takes no write does; its reads are the embedded handle's.
type writesRefused struct{ nats.KeyValue }

func (writesRefused) Put(string, []byte) (uint64, error) {
	return 0, errors.New("the bucket took no write")
}

func (writesRefused) Create(string, []byte) (uint64, error) {
	return 0, errors.New("the bucket took no write")
}

func (writesRefused) Update(string, []byte, uint64) (uint64, error) {
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
// holds unchanged is still written where its admission cleared the leader's record of another
// principal that the copy is behind, since a skip on the copy alone would leave no principal
// recorded; a registration whose leader does not answer is refused at its admission
// (ErrPrincipalUnrecorded) and writes nothing; a write that keeps the stored principal writes
// nothing where the leader does not answer, rather than the principal the copy names; and a
// registration whose write failed says so (ErrPrincipalUnrecorded), for the registration route's
// 503, unless the leader confirms it names the registration's own principal, or none. Each case
// fails where the store decides on its copy alone; each control is the same write where the copy
// is not behind, or the leader answers, or confirms the registration's own principal or none.
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

	t.Run("case: a registration the copy holds unchanged is written where the admission cleared the leader's record of another principal", func(t *testing.T) {
		known := openKnown(t, now)
		const sessionID = "ses_confirm_registration_behind"
		topics := []string{"notifications.team.confirm"}
		behindCopy(t, known, sessionID, topics, clock.UnixMilli(), service, person)
		if err := register(t, known, KnownSession{SessionID: sessionID, Topics: topics, Principal: person}); err != nil {
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
		if err := register(t, known, KnownSession{SessionID: sessionID, Topics: topics, Principal: person}); err != nil {
			t.Fatalf("record the registration: %v", err)
		}
		if got := revisionOf(t, known, sessionID); got != revision {
			t.Errorf("the record is at revision %d after a registration of unchanged content; want it left at %d", got, revision)
		}
	})
	t.Run("case: a registration the copy holds unchanged is refused where the leader does not answer", func(t *testing.T) {
		known := openKnown(t, now)
		useKnownKV(t, known, leaderSilent{known.watch().KV().Legacy()})
		const sessionID = "ses_confirm_registration_silent"
		topics := []string{"notifications.team.confirm"}
		behindCopy(t, known, sessionID, topics, clock.UnixMilli(), service, person)
		_, err := known.AdmitRegistration(t.Context(), sessionID, person)
		if !errors.Is(err, ErrPrincipalUnrecorded) {
			t.Errorf("the registration's admission answered %v while the leader was silent; want ErrPrincipalUnrecorded", err)
		}
		if stored := storedPrincipal(t, known, sessionID); stored != service {
			t.Errorf("the bucket's record names %q after a registration as %q the leader did not answer for; want the later registration's %q, untouched", stored, person, service)
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
		// admits is the principal the second registration is admitted and written as; between is
		// one a registration through another listener records after that admission, none for "".
		admits, between string
		handle          func(nats.KeyValue) nats.KeyValue
		unrecorded      bool
	}{
		{"case: a registration's write fails while the leader names another principal, recorded since the admission", "ses_confirm_unrecorded_leader",
			person, service, func(kv nats.KeyValue) nats.KeyValue { return writesRefused{kv} }, true},
		{"case: a registration's write fails while the leader is silent, its admission having cleared another principal", "ses_confirm_unrecorded_copy",
			person, "", func(kv nats.KeyValue) nats.KeyValue { return leaderSilent{writesRefused{kv}} }, true},
		{"case: a registration's write fails while the leader is silent and the record names the registration's own principal", "ses_confirm_unrecorded_copy_same",
			service, "", func(kv nats.KeyValue) nats.KeyValue { return leaderSilent{writesRefused{kv}} }, true},
		{"control: a registration's write fails while the leader names the registration's own principal", "ses_confirm_unrecorded_same",
			service, "", func(kv nats.KeyValue) nats.KeyValue { return writesRefused{kv} }, false},
		{"control: a registration's write fails while the leader names none, its admission having cleared another principal", "ses_confirm_unrecorded_cleared",
			person, "", func(kv nats.KeyValue) nats.KeyValue { return writesRefused{kv} }, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			known := openKnown(t, now)
			sessionID := row.sessionID
			if err := register(t, known, KnownSession{SessionID: sessionID, Topics: []string{"notifications.team.first"}, Principal: service}); err != nil {
				t.Fatalf("record the first registration: %v", err)
			}
			admission, err := known.AdmitRegistration(t.Context(), sessionID, row.admits)
			if err != nil {
				t.Fatalf("admit the registration as %q: %v", row.admits, err)
			}
			if row.between != "" {
				behindCopy(t, known, sessionID, []string{"notifications.team.between"}, clock.UnixMilli(), row.between, row.between)
			}
			useKnownKV(t, known, row.handle(known.watch().KV().Legacy()))
			err = known.RecordRegistrationWithin(t.Context(), admission, KnownSession{SessionID: sessionID, Topics: []string{"notifications.team.second"}, Principal: row.admits})
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
	if err := register(t, open, KnownSession{SessionID: sessionID, Principal: service}); err != nil {
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
		err := notOpen.RecordRegistrationWithin(t.Context(), Admission{}, KnownSession{SessionID: sessionID, Principal: person})
		if !errors.Is(err, ErrPrincipalUnrecorded) || !errors.Is(err, ErrKnownSessionsUnavailable) {
			t.Errorf("the registration answered %v; want %v wrapping %v", err, ErrPrincipalUnrecorded, ErrKnownSessionsUnavailable)
		}
		if stored := storedPrincipal(t, open, sessionID); stored != service {
			t.Fatalf("the bucket's record names %q; the case needs it still naming %q", stored, service)
		}
	})
	t.Run("case: the admission of a registration through the store whose bucket is not open", func(t *testing.T) {
		_, err := notOpen.AdmitRegistration(t.Context(), sessionID, person)
		if !errors.Is(err, ErrPrincipalUnrecorded) || !errors.Is(err, ErrKnownSessionsUnavailable) {
			t.Errorf("the admission answered %v; want %v wrapping %v", err, ErrPrincipalUnrecorded, ErrKnownSessionsUnavailable)
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
		admission, err := none.AdmitRegistration(t.Context(), sessionID, person)
		if err != nil {
			t.Errorf("the admission answered %v; want nil", err)
		}
		if err := none.RecordRegistrationWithin(t.Context(), admission, KnownSession{SessionID: sessionID, Principal: person}); err != nil {
			t.Errorf("the registration answered %v; want nil", err)
		}
	})
	t.Run("control: the same registration through the open store", func(t *testing.T) {
		admission, err := open.AdmitRegistration(t.Context(), sessionID, person)
		if err != nil {
			t.Errorf("the admission answered %v; want nil", err)
		}
		if err := open.RecordRegistrationWithin(t.Context(), admission, KnownSession{SessionID: sessionID, Principal: person}); err != nil {
			t.Errorf("the registration answered %v; want nil", err)
		}
		if stored := storedPrincipal(t, open, sessionID); stored != person {
			t.Errorf("the bucket's record names %q; want %q", stored, person)
		}
	})
}

// A registration is admitted only once no record names another principal for its session
// (AdmitRegistration), read from the bucket's leader: a record naming another principal, recorded,
// is written again with no principal recorded before the admission, so the handover gate refuses
// every handover of the session until the registration's own write records the new principal, and
// a leader that did not answer refuses the admission. The admission answers the revision it read
// or wrote, which the registration's write is fenced on. The case rows fail where the record still
// names the other principal, or where an unanswered read admits the registration. The controls - a
// record naming the registration's own principal, one whose principal no registration of this
// build recorded, and none at all - are admitted with the record as it was, at its revision.
func TestKnownSessionsAdmitARegistrationOnlyOnceNoRecordNamesAnotherPrincipal(t *testing.T) {
	const (
		person  = "ada@example.com"
		service = "example-fleet"
	)
	clock := time.Now()
	now := func() time.Time { return clock }
	put := func(t *testing.T, known *KnownSessions, sessionID string, record any) uint64 {
		t.Helper()
		value, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		revision, err := known.watch().KV().Put(sessionID, value)
		if err != nil {
			t.Fatalf("put %s's record: %v", sessionID, err)
		}
		return revision
	}
	for _, row := range []struct {
		name    string
		stored  any
		silent  bool
		refused bool
		// cleared says the admission writes the record again with no principal recorded; every
		// other row leaves the record as it was.
		cleared bool
	}{
		{name: "case: a record naming another principal has that principal cleared before the admission", cleared: true,
			stored: KnownSession{LastSeen: clock.UnixMilli(), Principal: service, PrincipalRecorded: true}},
		{name: "case: a leader that does not answer refuses the admission", silent: true, refused: true,
			stored: KnownSession{LastSeen: clock.UnixMilli(), Principal: service, PrincipalRecorded: true}},
		{name: "control: a record naming the registration's own principal is kept",
			stored: KnownSession{LastSeen: clock.UnixMilli(), Principal: person, PrincipalRecorded: true}},
		{name: "control: a record an earlier build wrote, naming no principal recorded, is kept",
			stored: map[string]any{"session_id": "ses_admit", "topics": []string{}, "last_seen": clock.UnixMilli()}},
		{name: "control: no record at all"},
	} {
		t.Run(row.name, func(t *testing.T) {
			known := openKnown(t, now)
			const sessionID = "ses_admit"
			var before uint64
			if row.stored != nil {
				before = put(t, known, sessionID, row.stored)
			}
			if row.silent {
				useKnownKV(t, known, leaderSilent{known.watch().KV().Legacy()})
			}
			admission, err := known.AdmitRegistration(t.Context(), sessionID, person)
			if got := errors.Is(err, ErrPrincipalUnrecorded); got != row.refused || (!row.refused && err != nil) {
				t.Fatalf("the admission answered %v; want ErrPrincipalUnrecorded %t", err, row.refused)
			}
			entry, getErr := known.watch().KV().Get(sessionID)
			switch {
			case row.stored == nil:
				if getErr == nil {
					t.Errorf("the admission wrote the record %s where none was", entry.Value())
				}
				if admission.revision != 0 {
					t.Errorf("the admission of a session with no record answers revision %d; want 0", admission.revision)
				}
			case getErr != nil:
				t.Errorf("the record is gone (%v); want it kept", getErr)
			case row.cleared:
				var record KnownSession
				if err := json.Unmarshal(entry.Value(), &record); err != nil {
					t.Fatalf("decode the record %s: %v", entry.Value(), err)
				}
				if entry.Revision() == before || record.Principal != "" || record.PrincipalRecorded {
					t.Errorf("the record at revision %d (written at %d) is %s; want it written again naming no principal recorded", entry.Revision(), before, entry.Value())
				}
				if admission.revision != entry.Revision() {
					t.Errorf("the admission answers revision %d; want the clear's %d", admission.revision, entry.Revision())
				}
			default:
				if entry.Revision() != before {
					t.Errorf("the record is at revision %d (%s); want it left at %d", entry.Revision(), entry.Value(), before)
				}
				if !row.refused && admission.revision != before {
					t.Errorf("the admission answers revision %d; want the record's %d", admission.revision, before)
				}
			}
		})
	}
}

// The admission of a registration as another principal clears only the principal of the record
// naming the earlier one (AdmitRegistration): the record of a session that is stopped - which a
// registration the route refuses after its admission leaves as the only record of the session -
// keeps its title, capabilities, topics and last_seen, so a send to the session and a publish's
// count still find it, in this listener's copy and in another listener's that reads the bucket
// afresh. The rows are a record stored whole; one stored in chunks, which is written again with
// every topic it held; and one whose chunks are gone, which no reader can join, written whole
// from its manifest's view. Each row fails where the admission deletes the record instead.
func TestKnownSessionsAnAdmissionKeepsAStoppedSessionsRecord(t *testing.T) {
	const (
		person  = "ada@example.com"
		service = "example-fleet"
	)
	for _, row := range []struct {
		name      string
		sessionID string
		topics    int
		damaged   bool
	}{
		{name: "a record stored whole", sessionID: "ses_stopped_whole", topics: 10},
		{name: "a record stored in chunks", sessionID: "ses_stopped_chunked", topics: 600},
		{name: "a record whose chunks are gone", sessionID: "ses_stopped_damaged", topics: 400, damaged: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			conn := kvchunktest.SmallPayloadNATS(t)
			stoppedAt := time.Now()
			clock := stoppedAt
			now := func() time.Time { return clock }
			known := openKnownOn(t, conn, now)
			kv := rawKnown(t, conn)
			topics := kvchunktest.Topics("stopped", row.topics)
			if err := register(t, known, KnownSession{SessionID: row.sessionID, Title: "stopped session", Capabilities: []string{"steer"}, Topics: topics, Principal: service}); err != nil {
				t.Fatalf("record the registration as %s: %v", service, err)
			}
			if row.topics > 10 {
				head, err := kv.Get(row.sessionID)
				if err != nil || !kvchunk.IsManifest(head.Value()) {
					t.Fatalf("the session's key holds %.60q (%v); the row needs a manifest", head.Value(), err)
				}
			}
			if row.damaged {
				head, err := kv.Get(row.sessionID)
				if err != nil {
					t.Fatalf("read the manifest: %v", err)
				}
				var view KnownSession
				if err := json.Unmarshal(head.Value(), &view); err != nil {
					t.Fatalf("decode the manifest's view: %v", err)
				}
				topics = view.Topics
				chunks := kvchunktest.LiveChunkKeys(t, kv)
				if err := kv.Delete(chunks[len(chunks)/2]); err != nil {
					t.Fatalf("delete a chunk: %v", err)
				}
			}
			// The session stopped: nothing writes its record for an hour, and then a registration
			// of it as another principal is admitted, and refused after its admission.
			clock = clock.Add(time.Hour)
			admission, err := known.AdmitRegistration(t.Context(), row.sessionID, person)
			if err != nil {
				t.Fatalf("admit the registration as %s: %v", person, err)
			}
			head, err := kv.Get(row.sessionID)
			if err != nil {
				t.Fatalf("the bucket holds no record of the stopped session after the admission (%v); want it kept with no principal", err)
			}
			if admission.revision != head.Revision() {
				t.Errorf("the admission answers revision %d; want the record's %d", admission.revision, head.Revision())
			}
			record, found, err := known.Read(t.Context(), row.sessionID)
			if err != nil || !found {
				t.Fatalf("Read after the admission = %t, %v; want the record", found, err)
			}
			if record.Principal != "" || record.PrincipalRecorded {
				t.Errorf("the leader's record names %q, recorded %t; want no principal recorded", record.Principal, record.PrincipalRecorded)
			}
			for what, store := range map[string]*KnownSessions{"the writer's store": known, "a store that read the bucket afresh": openKnownOn(t, conn, now)} {
				held, ok := store.Get(row.sessionID)
				if !ok {
					t.Fatalf("%s answers no record of the stopped session; a send to it would answer 404", what)
				}
				if held.Title != "stopped session" || !slices.Equal(held.Capabilities, []string{"steer"}) || held.LastSeen != stoppedAt.UnixMilli() || held.PrincipalRecorded {
					t.Errorf("%s answers %q, %v, last_seen %d, principal recorded %t; want the stopped session's title, capability and last_seen %d, no principal", what, held.Title, held.Capabilities, held.LastSeen, held.PrincipalRecorded, stoppedAt.UnixMilli())
				}
				requireKnownTopics(t, what, store, row.sessionID, topics)
				if got := matchedIDs(store, topics[len(topics)-1]); !slices.Contains(got, row.sessionID) {
					t.Errorf("%s matches %v for the record's last topic; want %s counted", what, got, row.sessionID)
				}
			}
		})
	}
}

// Two registrations of one session can each be admitted before either writes its record: the
// admission reads the record before the registration changes anything, and the registration
// writes its record last. So a registration's own write is a compare-and-swap at exactly the
// revision its admission read or wrote (RecordRegistrationWithin): a record written since - by
// another registration's admission, which cleared this registration's principal, or by its write
// - is left as it is, and the delayed write writes nothing and is ErrPrincipalUnrecorded, for the
// registration route's 503. The session first registers as a service, and the service's next
// registration is admitted with the record naming it; then, before that registration's delayed
// write, a person's registration is admitted, which clears the service's principal; or a person
// registers, admitted and written; or the service registers again. The first case fails where
// the delayed write is fenced on a fresh read of the leader, which finds the cleared record
// naming no principal and writes the service back over the person's admission; every case fails
// where it is a plain put. The controls are nothing between the admission and the write, which
// writes the service's record, and the person's own write after the refused one, which records
// the person at its admission's revision.
func TestKnownSessionsARegistrationWritesNothingOverARecordWrittenSinceItsAdmission(t *testing.T) {
	const (
		sessionID = "ses_fenced_registration"
		person    = "ada@example.com"
		service   = "example-fleet"
	)
	for _, row := range []struct {
		name string
		// between is what lands between the service's admission and its write: nothing, a
		// person's registration "admitted", a person's registration "registered", admitted and
		// written, or the service's registration "again", admitted and written.
		between string
		refused bool
	}{
		{name: "case: a person's registration is admitted between the service's admission and its write", between: "admitted", refused: true},
		{name: "case: a person registers the session between the service's admission and its write", between: "registered", refused: true},
		{name: "case: the service registers the session again between its admission and its write", between: "again", refused: true},
		{name: "control: nothing registers the session between the admission and the write"},
	} {
		t.Run(row.name, func(t *testing.T) {
			known := openKnown(t, time.Now)
			if err := register(t, known, KnownSession{SessionID: sessionID, Title: "the service's first registration", Principal: service}); err != nil {
				t.Fatalf("the service's first registration answered %v; want nil", err)
			}
			delayed, err := known.AdmitRegistration(t.Context(), sessionID, service)
			if err != nil {
				t.Fatalf("the service's admission answered %v; want nil", err)
			}
			var personal Admission
			switch row.between {
			case "admitted", "registered":
				if personal, err = known.AdmitRegistration(t.Context(), sessionID, person); err != nil {
					t.Fatalf("the person's admission answered %v; want nil", err)
				}
				if row.between == "registered" {
					if err := known.RecordRegistrationWithin(t.Context(), personal, KnownSession{SessionID: sessionID, Title: "the person's registration", Principal: person}); err != nil {
						t.Fatalf("the person's registration answered %v; want nil", err)
					}
				}
			case "again":
				if err := register(t, known, KnownSession{SessionID: sessionID, Title: "the service's registration between", Principal: service}); err != nil {
					t.Fatalf("the service's registration between answered %v; want nil", err)
				}
			}
			before, err := known.watch().KV().Get(sessionID)
			if err != nil {
				t.Fatalf("read the record before the delayed write: %v", err)
			}
			// The title differs from every record written before it, so the write is no skip of an
			// unchanged record.
			err = known.RecordRegistrationWithin(t.Context(), delayed, KnownSession{SessionID: sessionID, Title: "the service's delayed registration", Principal: service})
			if !row.refused {
				if err != nil {
					t.Fatalf("the service's registration answered %v; want nil", err)
				}
				if stored := storedPrincipal(t, known, sessionID); stored != service {
					t.Errorf("the bucket's record names %q; want %q", stored, service)
				}
				return
			}
			if !errors.Is(err, ErrPrincipalUnrecorded) {
				t.Errorf("the service's delayed registration answered %v; want ErrPrincipalUnrecorded", err)
			}
			after, err := known.watch().KV().Get(sessionID)
			if err != nil {
				t.Fatalf("read the record after the delayed write: %v", err)
			}
			if after.Revision() != before.Revision() {
				t.Errorf("the delayed write moved the record from revision %d to %d (%s); want nothing written", before.Revision(), after.Revision(), after.Value())
			}
			if row.between == "admitted" {
				if err := known.RecordRegistrationWithin(t.Context(), personal, KnownSession{SessionID: sessionID, Title: "the person's registration", Principal: person}); err != nil {
					t.Fatalf("the person's registration after the refused one answered %v; want nil", err)
				}
				if stored := storedPrincipal(t, known, sessionID); stored != person {
					t.Errorf("the bucket's record names %q after the person's registration; want %q", stored, person)
				}
			}
		})
	}
}
