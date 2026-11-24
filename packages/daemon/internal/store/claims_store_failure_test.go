package store

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// readClaim is token's claim as the store reads it back (Claims), which is what the daemon's
// sweep and the state read.
func readClaim(t *testing.T, store *Store, token claim.Token) supervise.Claim {
	t.Helper()
	claims, err := store.Claims(context.Background())
	if err != nil {
		t.Fatalf("read the claims: %v", err)
	}
	for _, c := range claims {
		if c.Token == token {
			return c
		}
	}
	t.Fatalf("the store holds no claim %s", token)
	return supervise.Claim{}
}

// The session chooses every field of what it reports, so the store bounds what it keeps of a
// store failure: the store's error to its first claim.StoreFailureErrorRunes runes, counted in
// runes so the cut never ends inside a character, and the failure's start to the daemon's clock,
// as the report's own reading time is. A start dated ahead would read as a failure that has not
// stood its minute for as long as the session's clock runs ahead.
func TestAStoreFailureIsKeptToItsFirstFiveHundredRunesAndNoLaterThanTheDaemonsClock(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-213-implementer")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}

	// 600 two-byte runes: a cut by bytes at 500 would keep 250 runes, and one by runes 500.
	long := strings.Repeat("é", claim.StoreFailureErrorRunes+100)
	before := time.Now().UTC()
	ahead := before.Add(time.Hour)
	if known, err := store.PutClaimActivity(ctx, c.Token, c.Generation, claim.SessionActivity{
		At: before.Add(-time.Second), StoreFailure: &claim.StoreFailure{Since: ahead, Error: long},
	}); err != nil || !known {
		t.Fatalf("report the store failure = %t, %v", known, err)
	}
	after := time.Now().UTC()

	stored := readClaim(t, store, c.Token)
	if want := strings.Repeat("é", claim.StoreFailureErrorRunes); stored.StoreFailedError != want {
		t.Errorf("the store keeps an error of %d runes, %d bytes, want its first %d runes",
			utf8.RuneCountInString(stored.StoreFailedError), len(stored.StoreFailedError), claim.StoreFailureErrorRunes)
	}
	if stored.StoreFailedSince.IsZero() || stored.StoreFailedSince.After(after) || stored.StoreFailedSince.Before(before.Truncate(time.Microsecond)) {
		t.Errorf("the store keeps a failure since %s for one dated %s, want the daemon's clock at receipt, between %s and %s",
			stored.StoreFailedSince, ahead, before, after)
	}

	// The control: an error within the bound and a start in the past are kept as sent.
	short, since := "Session file changed before rewrite: /home/legion/.omp/agent/sessions/01a1.jsonl", at(30)
	if known, err := store.PutClaimActivity(ctx, c.Token, c.Generation, claim.SessionActivity{
		At: time.Now().UTC(), StoreFailure: &claim.StoreFailure{Since: since, Error: short},
	}); err != nil || !known {
		t.Fatalf("report the second store failure = %t, %v", known, err)
	}
	if stored := readClaim(t, store, c.Token); stored.StoreFailedError != short || !stored.StoreFailedSince.Equal(since) {
		t.Errorf("the store keeps {%s %q}, want the failure as sent, {%s %q}", stored.StoreFailedSince, stored.StoreFailedError, since, short)
	}
}

// A store failure describes the process whose session reported it, so the launch that replaces
// that process clears it with the rest of the report (putClaim), and a write that keeps the
// launch keeps it. The mark of who was told of it is the workflow's alone and outlives the
// launch: it is what holds the claim's next notice to its hour across a relaunch.
func TestALaunchClearsAStoreFailureAndKeepsWhoWasToldOfIt(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-214-implementer")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	since, toldAt := at(10), at(11)
	if known, err := store.PutClaimActivity(ctx, c.Token, c.Generation, claim.SessionActivity{
		At: at(12), StoreFailure: &claim.StoreFailure{Since: since, Error: "Session file changed before rewrite"},
	}); err != nil || !known {
		t.Fatalf("report the store failure = %t, %v", known, err)
	}
	if err := Transact(ctx, store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().TellSessionStoreFailures(ctx, tx, []claim.Token{c.Token}, supervise.StoreFailureToldArchitect, toldAt)
	}); err != nil {
		t.Fatalf("mark the claim told: %v", err)
	}

	// A write of the same launch keeps the failure and the mark.
	c.State = supervise.StateIdle
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("record the claim gone idle: %v", err)
	}
	if stored := readClaim(t, store, c.Token); !stored.StoreFailedSince.Equal(since) || stored.StoreFailedError == "" ||
		!stored.StoreFailureToldAt.Equal(toldAt) || stored.StoreFailureToldTo != supervise.StoreFailureToldArchitect {
		t.Fatalf("after a write that kept the launch the claim holds {%s %q told %s to %q}, want the failure and the mark standing",
			stored.StoreFailedSince, stored.StoreFailedError, stored.StoreFailureToldAt, stored.StoreFailureToldTo)
	}

	// The launch that replaces the process clears the failure and keeps the mark.
	c.Generation++
	c.State = supervise.StateLaunching
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("record the launch that replaced the process: %v", err)
	}
	stored := readClaim(t, store, c.Token)
	if !stored.StoreFailedSince.IsZero() || stored.StoreFailedError != "" {
		t.Errorf("after the launch the claim holds the store failure {%s %q}, want none: it was the replaced process's",
			stored.StoreFailedSince, stored.StoreFailedError)
	}
	if !stored.StoreFailureToldAt.Equal(toldAt) || stored.StoreFailureToldTo != supervise.StoreFailureToldArchitect {
		t.Errorf("after the launch the claim was told %s to %q, want %s to %q: the mark outlives a relaunch",
			stored.StoreFailureToldAt, stored.StoreFailureToldTo, toldAt, supervise.StoreFailureToldArchitect)
	}
}
