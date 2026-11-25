package session

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"
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
