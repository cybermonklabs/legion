package outbox

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/session"
)

// agedUnnamedAttempts puts every attempt of commentID at age with its claim written just now,
// which is the sender that is alive and holding its lane: the attempt was created age ago, so
// the outbox reads it past the lease it waits a send out for (model.MentionResumedSQL), and the
// claim on it is this very moment's, so it is neither abandoned nor the delivery sweep's
// (model.MentionAbandonedSQL, model.OwedMentionAttemptSQL). A sender writes that claim again
// every mentionClaimRefresh for as long as its process lives, so an attempt staged this way
// stays pending however old it grows (mentionClaimRefresh in internal/dispatch/api).
func agedUnnamedAttempts(t *testing.T, database *store.Store, commentID string, age time.Duration) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		update comment_deliveries set created_at = now() - $2::interval, claimed_at = now()
		where comment_id = $1
	`, commentID, age); err != nil {
		t.Fatalf("age the comment's attempts to %s with their claims renewed: %v", age, err)
	}
}

// heldDestinations lists the destinations of commentID that comment_fallback_copies rows own.
func heldDestinations(t *testing.T, database *store.Store, commentID string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select destination from comment_fallback_copies where comment_id = $1 order by destination
	`, commentID)
	if err != nil {
		t.Fatalf("read the comment's fallback copies: %v", err)
	}
	held, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("decode the comment's fallback copies: %v", err)
	}
	return held
}

// A role route's forward waits for an attempt of the comment that names no session yet and that
// no one key stands for, and that wait has a bound of its own: unnamedForwardBound, two leases
// from the attempt's creation (mentionHolds.waitsForAnUnnamedMention). Within it the forward
// waits, as TestAForwardWaitsForTwoUnnamedMentionsPastTheirLease has it. Past it the copy is
// held as a comment_fallback_copies row of the role's own topic keyed by the holder, the event
// settles, and the holder's later events of the issue flow again (holdsTheForwardAsARow,
// destination.heldAsARow, planChains).
//
// The sender here is alive and its lane is held, which is what makes the bound the only thing
// that ends this wait: it writes the claim on each attempt again every mentionClaimRefresh, so
// neither attempt is ever abandoned and neither is the delivery sweep's to resume
// (agedUnnamedAttempts, model.MentionResumedSQL). Nothing in the attempts settles, so before
// this round the forward waited for as long as that process lived, and the holder of the role
// received nothing further of the issue meanwhile - the measured starvation, which is what the
// owner's later event here stands for.
//
// What the bound gives up is the owner's order to that one holder for that one comment: the
// later event reaches the holder first and the comment follows as the row, once the sender's
// sends have settled. What it keeps is the one delivery: the row is published only once no send
// of that mention is owed, so the holder is shown the comment once and never beside the mention
// a send may yet pin to it (decideFallbackCopy, mentionHolds.state).
func TestAForwardPastItsBoundIsHeldAsARowAndStopsHoldingTheHoldersLaterEvents(t *testing.T) {
	const routed = "routed-to-a-session-either-mention-may-name"
	const first = "mentioned-first-whose-sender-still-holds-its-lane"
	const second = "mentioned-second-whose-sender-still-holds-its-lane"
	const holder = "ses_holds_the_routed_role"
	ctx := store.WithTransactionTracking(context.Background())
	firstTarget, secondTarget := "role:"+first, "role:"+second
	route := "role:" + routed
	roleTopic := contracts.RoleTopicPrefix + routed

	rig := newRoleRig(t)
	rig.claim(t, routed, holder, session.SessionEntry{SelfSubscribed: true})
	rig.silent(t, roleTopic)
	recv := rig.receiver(t, holder)

	alice := model.Actor{Kind: "user", ID: "alice"}
	seedIssue(t, rig.database, "T-1", &route)
	body := "@" + firstTarget + " @" + secondTarget + " look"
	commentID := seedComment(t, rig.database, "T-1", alice, body, nil)
	comment := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{ID: commentID, IssueKey: new("T-1"), Body: body, Mentions: []model.Mention{
				{Target: firstTarget, Delivery: "steer"}, {Target: secondTarget, Delivery: "steer"}}},
		},
	})
	seedDelivery(t, rig.database, commentID, firstTarget, "", "resumed")
	seedDelivery(t, rig.database, commentID, secondTarget, "", "resumed")
	// Both attempts are past their lease and within the bound, their claims this moment's.
	agedUnnamedAttempts(t, rig.database, commentID, unnamedForwardBound/2)

	// The owner's next event of the same issue, which the forward's wait holds behind it: the
	// holder's key is the one both reach (destination.key, planChains).
	laterBody := "and one more thing"
	laterID := seedComment(t, rig.database, "T-1", alice, laterBody, nil)
	later := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{ID: laterID, IssueKey: new("T-1"), Body: laterBody},
		},
	})

	holding(t, rig.deps()).scan(ctx, unfenced{})
	time.Sleep(time.Second)

	if got := recv.received(); len(got) != 0 {
		t.Fatalf("%s was sent %v within the bound, want nothing: the forward waits while a send of an unnamed mention no key stands for is owed on an attempt younger than %s, and the owner's later event waits behind it",
			holder, keysOf(got), unnamedForwardBound)
	}
	if at := publishedAt(t, rig.database, comment.ID); at != nil {
		t.Fatalf("published_at = %v within the bound, want the comment still waiting on its unnamed mentions", at)
	}
	if at := publishedAt(t, rig.database, later.ID); at != nil {
		t.Fatalf("the later event has published_at = %v while the comment's forward holds the holder's key, want it held back", at)
	}
	if rows := heldDestinations(t, rig.database, commentID); len(rows) != 0 {
		t.Fatalf("the comment holds fallback rows %v within the bound, want none: the forward waits in the event's list while the bound holds", rows)
	}

	// The bound passes with the sender still alive: it wrote both claims again just now, so
	// nothing of the attempts settled and nothing else ends the wait.
	agedUnnamedAttempts(t, rig.database, commentID, unnamedForwardBound+30*time.Second)
	dueNow(t, rig.database, comment.ID)
	dueNow(t, rig.database, later.ID)

	holding(t, rig.deps()).scan(ctx, unfenced{})

	if publishedAt(t, rig.database, comment.ID) == nil {
		attempts, failure, reached := eventState(t, rig.database, comment.ID)
		t.Fatalf("the comment was not published past the forward's bound of %s: attempt_count %d, publish_error %s, reached %v. The wait has a bound of its own, the sender's claim keeping its attempts pending for as long as its process lives",
			unnamedForwardBound, attempts, publishFailure(failure), reached)
	}
	_, _, reached := eventState(t, rig.database, comment.ID)
	if slices.Contains(reached, roleTopic) || slices.Contains(reached, contracts.AgentSubject(holder)) {
		t.Fatalf("the comment reached %v past the bound, want neither %s nor %s among them: the copy is the row's, not the event's",
			reached, roleTopic, contracts.AgentSubject(holder))
	}
	if rows := heldDestinations(t, rig.database, commentID); !slices.Contains(rows, roleTopic) {
		t.Fatalf("the comment holds fallback rows %v past the bound, want %s among them: the forward is held as a row of the role's own topic, keyed by its holder, rather than published under the event's own key",
			rows, roleTopic)
	}
	// The row waits for the send the forward was waiting for, which is what keeps the double
	// closed: a copy published here would stand beside a mention a send may yet pin to this
	// holder (decideFallbackCopy, mentionHolds.state).
	publishFallbackCopies(ctx, rig.deps(), unfenced{}, holdingProcess(t, rig.database))
	if row := readHeldCopy(t, rig.database, commentID, roleTopic); row.settled || row.published || row.attempts != 0 {
		t.Fatalf("the role row is settled %v, published %v and attempts %d while a send of the comment's unnamed mentions is still owed, want it waiting with none counted: the row waits for that send as every other copy of the comment does",
			row.settled, row.published, row.attempts)
	}
	waitFor(t, 10*time.Second, "the owner's later event at the holder", func() bool { return len(recv.received()) > 0 })
	if copies := recv.received(); len(copies) != 1 || copies[0].SourceEventID != fmt.Sprint(later.ID) {
		t.Fatalf("%s was sent %+v past the bound, want the later event (%d) alone: the comment's copy is the row's now, and the order to this one holder is what the bound gives up",
			holder, copies, later.ID)
	}

	// The sender's lane reaches the sends at last, both resolve to no session and fail. Nothing
	// of the mention is owed from then on, so the pass that reads the row publishes the copy.
	if _, err := rig.database.Pool.Exec(ctx, `
		update comment_deliveries set state = 'failed', error = 'no live session' where comment_id = $1
	`, commentID); err != nil {
		t.Fatalf("fail the comment's mentions: %v", err)
	}
	if _, err := rig.database.Pool.Exec(ctx, `
		update comment_fallback_copies set next_attempt_at = now() - interval '1 second' where comment_id = $1
	`, commentID); err != nil {
		t.Fatalf("make the role row due: %v", err)
	}

	publishFallbackCopies(ctx, rig.deps(), unfenced{}, holdingProcess(t, rig.database))

	waitFor(t, 10*time.Second, "the comment's copy the row owes the holder", func() bool { return len(recv.received()) > 1 })
	time.Sleep(time.Second)
	copies := recv.received()
	if len(copies) != 2 || copies[1].SourceEventID != fmt.Sprint(comment.ID) {
		t.Fatalf("%s was sent %+v once the mentions settled, want the later event (%d) and then the comment (%d), each once: the row delivers the comment the forward gave up, and delivers it once",
			holder, copies, later.ID, comment.ID)
	}
	if row := readHeldCopy(t, rig.database, commentID, roleTopic); !row.settled || !row.published {
		t.Fatalf("the role row is settled %v and published %v once the mentions failed, want both: the pass that reads it forwards the copy to the role's holder",
			row.settled, row.published)
	}
}
