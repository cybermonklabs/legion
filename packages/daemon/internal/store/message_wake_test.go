package store

// The mark that a message waits for a claim's role (dispatch://INFRA-5682 R1): the transaction of
// a start row or a listener report writes it on the claim, by the claim's token or its session,
// and the claim's own write keeps it until the role takes a turn after it.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// markedClaim reads the claim back with its mark, as a boot reads it.
func markedClaim(t *testing.T, store *Store, token claim.Token) supervise.Claim {
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

// A listener's report marks the claim it names, by its token or by its session, whatever the
// claim's state; the earliest mark the role has had no turn since stands, since that message is
// the one nearest the end of Envoy's keep window; and a claim of another project, or of no row at
// all, is marked not at all.
func TestAMessageWakeIsMarkedOnTheClaimItNames(t *testing.T) {
	store := migratedStore(t)
	ctx := context.Background()
	seedReleasedPod(t, store, "session-1")
	records := podRecords(t, store)
	first := time.Now().UTC().Truncate(time.Millisecond)

	inTx(t, store, func(tx pgx.Tx) error {
		token, at, marked, err := records.MarkMessageWake(ctx, tx, string(releasedToken), "", first)
		if err != nil || !marked || token != releasedToken || !at.Equal(first) {
			t.Fatalf("the mark by token = %s, %s, %t, %v; want %s marked at %s", token, at, marked, err, releasedToken, first)
		}
		// The second message of the same role, with no turn between: the first mark stands.
		token, at, marked, err = records.MarkMessageWake(ctx, tx, "", "session-1", first.Add(time.Minute))
		if err != nil || !marked || token != releasedToken || !at.Equal(first) {
			t.Fatalf("the mark by session = %s, %s, %t, %v; want %s still marked at %s: the earlier message ages first",
				token, at, marked, err, releasedToken, first)
		}
		if _, _, marked, err := records.MarkMessageWake(ctx, tx, "legion-legion-legion-999-tester", "", first); err != nil || marked {
			t.Fatalf("the mark of a claim this project does not hold = %t, %v; want no row marked", marked, err)
		}
		if _, _, _, err := records.MarkMessageWake(ctx, tx, "", "", first); err == nil {
			t.Fatal("a mark naming neither a token nor a session was taken, want a refusal")
		}
		if _, _, _, err := records.MarkMessageWake(ctx, tx, string(releasedToken), "", time.Time{}); err == nil {
			t.Fatal("a mark with no moment was taken, want a refusal")
		}
		return nil
	})

	if c := markedClaim(t, store, releasedToken); !c.MessageMark.Equal(first) {
		t.Fatalf("the claim reads back marked at %s, want %s", c.MessageMark, first)
	}
	if _, waits := markedClaim(t, store, releasedToken).MessageWaiting(); !waits {
		t.Fatal("the claim reads no message waiting, want the mark it holds: its role has taken no turn since")
	}
}

// The claim's own write never sets the mark and never clears it while the role has taken no turn
// since: a restart reads the mark the listener's report left, which is what places the boot's
// relaunch in the message queue. A write whose last turn start is later than the mark spends it:
// the role read its messages in that turn.
func TestAClaimsOwnWriteKeepsTheMarkUntilItsRoleTakesATurn(t *testing.T) {
	store := migratedStore(t)
	ctx := context.Background()
	seedReleasedPod(t, store, "session-1")
	records := podRecords(t, store)
	at := time.Now().UTC().Truncate(time.Millisecond)
	inTx(t, store, func(tx pgx.Tx) error {
		_, _, _, err := records.MarkMessageWake(ctx, tx, string(releasedToken), "", at)
		return err
	})

	// The claim's machine writes it again, carrying the claim it read before the mark: the mark
	// stands, and no write of the machine's sets one.
	c := markedClaim(t, store, releasedToken)
	unmarked := c
	unmarked.MessageMark = time.Time{}
	unmarked.State = supervise.StateLaunching
	if err := store.PutClaim(ctx, unmarked); err != nil {
		t.Fatalf("write the claim its machine holds: %v", err)
	}
	if got := markedClaim(t, store, releasedToken); !got.MessageMark.Equal(at) || got.State != supervise.StateLaunching {
		t.Fatalf("after the machine's write the claim is %s marked at %s, want %s still marked at %s",
			got.State, got.MessageMark, supervise.StateLaunching, at)
	}

	// The role takes a turn after the mark: the write that records it spends the mark.
	turned := markedClaim(t, store, releasedToken)
	turned.LastTurnStart = at.Add(time.Minute)
	if err := store.PutClaim(ctx, turned); err != nil {
		t.Fatalf("write the claim whose role took a turn: %v", err)
	}
	got := markedClaim(t, store, releasedToken)
	if !got.MessageMark.IsZero() {
		t.Fatalf("after the role's turn the claim is marked at %s, want no mark: the role read its messages in that turn", got.MessageMark)
	}
	if _, waits := got.MessageWaiting(); waits {
		t.Fatal("the claim reads a message waiting after its role's turn, want none")
	}

	// A message marked after that turn stands again.
	later := at.Add(2 * time.Minute)
	inTx(t, store, func(tx pgx.Tx) error {
		_, stamp, marked, err := records.MarkMessageWake(ctx, tx, "", "session-1", later)
		if err != nil || !marked || !stamp.Equal(later) {
			t.Fatalf("the mark after the turn = %s, %t, %v; want it marked at %s", stamp, marked, err, later)
		}
		return nil
	})
	if _, waits := markedClaim(t, store, releasedToken).MessageWaiting(); !waits {
		t.Fatal("the claim reads no message waiting after a mark later than its turn, want one")
	}
}
