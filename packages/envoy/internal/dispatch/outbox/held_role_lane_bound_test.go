package outbox

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/session"
)

// heldRoleLaneBound is the longest a role route's forward waits for an unnamed mention, measured
// from that mention's attempt's creation: two claim leases. It is written out rather than read
// from unnamedForwardBound, so that a change which widens that bound fails the test below instead
// of moving the test's own clock with it.
const heldRoleLaneBound = 2 * model.DeliveryClaimLapse

// A comment's role route forwards to the role's holder, and the comment names two roles whose
// listener lookups went unanswered, so no one key stands for its mentions and the forward waits for
// them (mentionHolds.waitsForAnUnnamedMention). Their sender is alive and its lanes are held: it
// writes each attempt's claim again and again, so neither attempt is abandoned and neither settles
// (agedUnnamedAttempts). While the forward waits, the holder receives nothing further of the issue:
// the owner's later event is held behind the holder's key (planChains). That starvation ends at
// heldRoleLaneBound after the attempts' creation and no later: just past it the holder receives the
// later event, and once the mentions' sends settle the comment's copy reaches it, once.
//
// An earlier event of the issue, already published to the holder, stands before the comment, so
// the comment's copy has an earlier event to keep after; it owes nothing more, and holds nothing.
func TestAHeldRoleLaneStarvesItsHolderNoLongerThanTheUnnamedForwardBound(t *testing.T) {
	t.Logf("start %s", time.Now().UTC().Format(time.RFC3339Nano))
	const routed = "routed-role-of-the-issue"
	const first = "first-unanswered-role"
	const second = "second-unanswered-role"
	const holder = "ses_holds_the_routed_role"
	ctx := store.WithTransactionTracking(t.Context())
	route := "role:" + routed
	roleTopic := contracts.RoleTopicPrefix + routed

	rig := newRoleRig(t)
	rig.claim(t, routed, holder, session.SessionEntry{SelfSubscribed: true})
	rig.silent(t, roleTopic)
	recv := rig.receiver(t, holder)
	alice := model.Actor{Kind: "user", ID: "alice"}
	seedIssue(t, rig.database, "T-1", &route)
	comment := func(body string, mentions ...string) (string, model.Event) {
		t.Helper()
		id := seedComment(t, rig.database, "T-1", alice, body, nil)
		var named []model.Mention
		for _, target := range mentions {
			named = append(named, model.Mention{Target: target, Delivery: "steer"})
		}
		return id, appendEvent(t, rig.database, rig.broker, model.Event{
			IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
			Payload: model.CommentEventPayload{Comment: model.Comment{ID: id, IssueKey: new("T-1"), Body: body, Mentions: named}},
		})
	}

	_, earlier := comment("an earlier comment")
	holding(t, rig.deps()).scan(ctx, unfenced{})
	waitFor(t, 10*time.Second, "the earlier comment at the holder", func() bool { return len(recv.received()) == 1 })
	if publishedAt(t, rig.database, earlier.ID) == nil {
		t.Fatal("the earlier comment is not published")
	}

	commentID, held := comment("@role:"+first+" @role:"+second+" look", "role:"+first, "role:"+second)
	seedDelivery(t, rig.database, commentID, "role:"+first, "", "resumed")
	seedDelivery(t, rig.database, commentID, "role:"+second, "", "resumed")
	_, later := comment("and one more thing")

	// Within the bound: the attempts are ten seconds short of it, their claims this moment's.
	agedUnnamedAttempts(t, rig.database, commentID, heldRoleLaneBound-10*time.Second)
	holding(t, rig.deps()).scan(ctx, unfenced{})
	time.Sleep(time.Second)
	if got := recv.received(); len(got) != 1 {
		t.Fatalf("%s was sent %v with the attempts %s short of the bound, want the earlier comment alone: the forward waits for the unnamed mentions, and the later event waits behind it",
			holder, keysOf(got), 10*time.Second)
	}
	if publishedAt(t, rig.database, held.ID) != nil || publishedAt(t, rig.database, later.ID) != nil {
		t.Fatal("the comment or the later event is published within the bound, want both waiting")
	}

	// Just past the bound, the sender still alive and its claims written again just now.
	agedUnnamedAttempts(t, rig.database, commentID, heldRoleLaneBound+time.Second)
	dueNow(t, rig.database, held.ID)
	dueNow(t, rig.database, later.ID)
	passed := time.Now()
	holding(t, rig.deps()).scan(ctx, unfenced{})
	waitFor(t, 10*time.Second, "the later event at the holder past the bound", func() bool { return len(recv.received()) == 2 })
	t.Logf("the later event reached %s %v after the scan past the bound", holder, time.Since(passed))
	if got := recv.received(); got[1].SourceEventID != fmt.Sprint(later.ID) {
		t.Fatalf("%s was sent %+v past the bound, want the earlier comment and then the later event (%d)", holder, got, later.ID)
	}
	if publishedAt(t, rig.database, held.ID) == nil {
		attempts, failure, reached := eventState(t, rig.database, held.ID)
		t.Fatalf("the comment is not published past the bound: attempt_count %d, publish_error %s, reached %v", attempts, publishFailure(failure), reached)
	}
	if rows := heldDestinations(t, rig.database, commentID); !slices.Contains(rows, roleTopic) {
		t.Fatalf("the comment holds fallback rows %v past the bound, want its forward held as a row of %s", rows, roleTopic)
	}

	// The lanes reach the sends at last; both resolve to no session and fail, so nothing of the
	// mentions is owed and the row's copy goes.
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
	waitFor(t, 10*time.Second, "the comment's copy at the holder", func() bool { return len(recv.received()) == 3 })
	time.Sleep(time.Second)
	got := recv.received()
	var ids []string
	for _, item := range got {
		ids = append(ids, item.SourceEventID)
	}
	t.Logf("%s received events %v (want %d, %d, %d)", holder, ids, earlier.ID, later.ID, held.ID)
	t.Logf("end %s", time.Now().UTC().Format(time.RFC3339Nano))
	if !slices.Equal(ids, []string{fmt.Sprint(earlier.ID), fmt.Sprint(later.ID), fmt.Sprint(held.ID)}) {
		t.Fatalf("%s received events %v, want the earlier comment, the later event and the comment, each once", holder, ids)
	}
}
