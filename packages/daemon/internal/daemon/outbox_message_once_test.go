package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// A row's effect runs between its claim and the finish that deletes it, and those are two
// transactions: a daemon that dies after the post and before the finish leaves the row for the
// next claim, which the lease's lapse or the next boot makes. Dispatch's message route takes no
// idempotency key, so nothing on the far side refuses the second post and a person reads the same
// notice twice (dispatch://INFRA-49/comment/561afcc3-419f-4ce2-9e02-e9c1a51ac8eb). What stops it
// is the daemon's own key: the post names the row that made it, and a claim that may be a repeat
// reads the issue's messages for that marker first.
func TestAMessageRowClaimedAgainAfterACrashPostsNothingTwice(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	client := echoingDispatch{postingDispatch: &postingDispatch{}}
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, dispatch: client, log: quietLogger()}
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208",
		record.MessagePost{Body: "the merge closed this issue"}, now))

	// The claim, the post, and then the crash: nothing finishes the row.
	first := claimTheOnlyRow(t, pool, records, now)
	if first.Claims != 1 {
		t.Fatalf("the first claim recorded %d claims, want 1", first.Claims)
	}
	runMessage(t, runner, first)

	// The lease lapses and the row is claimed again, by this daemon's next sweep or by another.
	second := claimTheOnlyRow(t, pool, records, now.Add(2*outboxLease))
	if second.Claims != 2 {
		t.Fatalf("the second claim recorded %d claims, want 2: the claim counts itself in its own transaction", second.Claims)
	}
	runMessage(t, runner, second)

	if posts := client.postsTo("LEGION-208"); posts != 1 {
		t.Fatalf("%d messages posted on LEGION-208, want the one: %v", posts, client.bodiesTo("LEGION-208"))
	}
}

// A row this daemon claims for the first time has posted nothing, so the executor posts without
// reading the issue's messages at all: the read is for a run that may be a repeat, and a Dispatch
// round trip for every notice the daemon sends is what it buys nothing.
func TestAMessageRowOnItsFirstClaimReadsNoMessages(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	client := echoingDispatch{postingDispatch: &postingDispatch{}}
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, dispatch: client, log: quietLogger()}
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208",
		record.MessagePost{Body: "the merge closed this issue"}, now))

	runMessage(t, runner, claimTheOnlyRow(t, pool, records, now))

	if posts, reads := client.postsTo("LEGION-208"), client.reads(); posts != 1 || reads != 0 {
		t.Fatalf("%d messages posted and %d message reads, want one post and no read", posts, reads)
	}
}

// claimTheOnlyRow claims the one row the outbox holds, as a lane claims a row it listed.
func claimTheOnlyRow(t *testing.T, pool *pgxpool.Pool, records record.Store, now time.Time) record.OutboxRow {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), "select id from outbox").Scan(&id); err != nil {
		t.Fatalf("read the queued row: %v", err)
	}
	var row record.OutboxRow
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		claimed, ok, err := records.ClaimRow(context.Background(), tx, id, now, outboxLease)
		if err != nil {
			return err
		}
		if !ok {
			t.Fatalf("row %d was not claimable at %s", id, now)
		}
		row = claimed
		return nil
	}); err != nil {
		t.Fatalf("claim row %d: %v", id, err)
	}
	return row
}

// runMessage runs the message executor over row, as the lane runs it after the claim commits and
// before the finish that deletes the row.
func runMessage(t *testing.T, runner *outbox, row record.OutboxRow) {
	t.Helper()
	var payload record.MessagePost
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		t.Fatalf("decode the message payload of row %d: %v", row.ID, err)
	}
	if err := runner.message(context.Background(), row, payload); err != nil {
		t.Fatalf("run the message of row %d: %v", row.ID, err)
	}
}

// echoingDispatch is postingDispatch whose message read answers what was posted, which is what
// Dispatch itself answers once a post has landed, and which counts the reads it was asked for.
type echoingDispatch struct {
	*postingDispatch
}

func (d echoingDispatch) MessageBodiesSince(_ context.Context, key string, _ time.Time) ([]string, error) {
	d.countRead()
	return d.bodiesTo(key), nil
}
