package session

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/kvchunk"
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
