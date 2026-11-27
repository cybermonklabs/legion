package api

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/listenerwire"
)

// keptAttempt is attempt 1 of a post's delivery to a role as the sender of its send leaves it
// before its settle: pending, claimed, with no envelope and no session.
type keptAttempt struct {
	post listenerwire.DispatchPost
	// settle is the sender's settle transaction (completeCommentDelivery,
	// completeMessageDelivery), called with what its send reported, and answers the state it
	// settled the attempt in.
	settle func(outcome sendOutcome) (string, error)
	// read is the attempt as the settle left it - its state, its error and its envelope - and the
	// receipts it has, as commentDeliveryReceipts and messageDeliveryReceipts render them.
	read func() (state string, failure, envelope *string, receipts string)
}

// seedKeptAttempt writes a post of kind on issue, a person's comment that mentions target or a
// message to it, and its attempt 1 as a sender that claimed it just now leaves it.
func seedKeptAttempt(t *testing.T, s *server, database *store.Store, issue, kind, target string) keptAttempt {
	t.Helper()
	ctx := context.Background()
	actor := model.Actor{Kind: "user", ID: "alice"}
	var id string
	var claimed time.Time
	if kind == listenerwire.DispatchPostComment {
		if err := database.Pool.QueryRow(ctx, `
			insert into comments (issue_key, author, body) values ($1, '{"kind":"user","id":"alice"}', 'For the role.') returning id::text
		`, issue).Scan(&id); err != nil {
			t.Fatalf("create the comment: %v", err)
		}
		if _, err := database.Pool.Exec(ctx, `
			insert into comment_mentions (comment_id, target, delivery) values ($1, $2, 'steer')
		`, id, target); err != nil {
			t.Fatalf("mention %s in comment %s: %v", target, id, err)
		}
		if err := database.Pool.QueryRow(ctx, `
			insert into comment_deliveries (comment_id, target, attempt, delivery, state, claimed_at)
			values ($1, $2, 1, 'steer', 'pending', now()) returning claimed_at
		`, id, target).Scan(&claimed); err != nil {
			t.Fatalf("claim attempt 1 of comment %s: %v", id, err)
		}
		comment, err := s.loadComment(ctx, database.Pool, id)
		if err != nil {
			t.Fatalf("load comment %s: %v", id, err)
		}
		return keptAttempt{
			post: listenerwire.DispatchPost{Kind: kind, ID: id},
			settle: func(outcome sendOutcome) (string, error) {
				settled, err := s.completeCommentDelivery(ctx, comment,
					model.CommentDelivery{CommentID: id, Target: target, Attempt: 1, Delivery: "steer"},
					actor, attemptClaim{attempt: 1, claimedAt: claimed}, outcome)
				return settled.State, err
			},
			read: func() (string, *string, *string, string) {
				var state string
				var failure, envelope *string
				if err := database.Pool.QueryRow(ctx, `
					select state, error, envelope_id from comment_deliveries where comment_id = $1 and target = $2 and attempt = 1
				`, id, target).Scan(&state, &failure, &envelope); err != nil {
					t.Fatalf("read comment %s's attempt: %v", id, err)
				}
				return state, failure, envelope, commentDeliveryReceipts(t, database, id, target, 1)
			},
		}
	}
	if err := database.Pool.QueryRow(ctx, `
		insert into messages (issue_key, author, body, target) values ($1, '{"kind":"user","id":"alice"}', 'For the role.', $2)
		returning id::text
	`, issue, target).Scan(&id); err != nil {
		t.Fatalf("create the message: %v", err)
	}
	if err := database.Pool.QueryRow(ctx, `
		insert into message_deliveries (message_id, attempt, delivery, session_id, state, claimed_at, requested_by)
		values ($1, 1, 'steer', '', 'pending', now(), '{"kind":"user","id":"alice"}') returning claimed_at
	`, id).Scan(&claimed); err != nil {
		t.Fatalf("claim attempt 1 of message %s: %v", id, err)
	}
	message, err := s.loadMessage(ctx, database.Pool, "", id)
	if err != nil {
		t.Fatalf("load message %s: %v", id, err)
	}
	return keptAttempt{
		post: listenerwire.DispatchPost{Kind: kind, ID: id},
		settle: func(outcome sendOutcome) (string, error) {
			settled, err := s.completeMessageDelivery(ctx, message, actor, pendingMessageDelivery{
				attemptClaim: attemptClaim{attempt: 1, claimedAt: claimed},
				resolved:     ResolvedMention{Target: target, Delivery: "steer"},
			}, outcome)
			return settled.State, err
		},
		read: func() (string, *string, *string, string) {
			var state string
			var failure, envelope *string
			if err := database.Pool.QueryRow(ctx, `
				select state, error, envelope_id from message_deliveries where message_id = $1 and attempt = 1
			`, id).Scan(&state, &failure, &envelope); err != nil {
				t.Fatalf("read message %s's attempt: %v", id, err)
			}
			return state, failure, envelope, messageDeliveryReceipts(t, database, id, 1)
		},
	}
}

// keptReceipts is the one receipt of an attempt of a post of kind that ended in state with failure,
// as commentDeliveryReceipts and messageDeliveryReceipts render it.
func keptReceipts(kind, state, failure string) string {
	if kind == listenerwire.DispatchPostComment {
		return state + "/" + failure + "//"
	}
	return state + "/" + failure
}

// The outbox settles the attempts of a kept delivery's copy that are queued when the listener's
// notice that the copy ended is read, and the notice can be read before the API records the
// attempt queued: the send's report waits on a transaction of its own, so the attempt is still
// pending and the notice settles nothing. The outbox records the end of the copy under the post's
// owner lock, and the API's settle, which takes the same lock, reads it before it writes queued
// (settleDeliveryAttempt, keptDeliveryExpired) and settles the attempt failed instead, with the
// error that says whether the copy's forward may have reached the role's holder, its envelope
// cleared and its receipt saying the same, so the attempt ends settled in either order, for a
// comment and for a message alike. A notice that names another role's copy, or another copy, is
// no record of this attempt's, and the attempt is queued as ever. With the check taken away, every
// row ends queued, and the first two fail.
func TestAKeptDeliverysAttemptSettledAfterTheNoticeOfItsCopyWasReadEndsFailed(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "TEST", "A delivery kept for a role", "before")
	s := directServer(deps)
	const (
		role     = "legion-kept-planner"
		another  = "legion-kept-other"
		envelope = "kept-copy-envelope"
	)
	for _, kind := range []string{listenerwire.DispatchPostComment, listenerwire.DispatchPostMessage} {
		for _, tc := range []struct {
			name string
			// recordRole and recordEnvelope name the end of a copy the outbox recorded before the
			// settle, none where recordRole is empty; forward is its qualifier.
			recordRole, recordEnvelope string
			forward                    bool
			// failed is whether the settle ends the attempt failed.
			failed bool
		}{
			{"the outbox has read no notice", "", "", false, false},
			{"the outbox read the notice that the copy ended", role, envelope, false, true},
			{"the outbox read the notice that the copy's forward may have reached the holder", role, envelope, true, true},
			{"the notice is another role's", another, envelope, true, false},
			{"the notice is another copy's", role, "another-envelope", true, false},
		} {
			t.Run(kind+", "+tc.name, func(t *testing.T) {
				ctx := context.Background()
				attempt := seedKeptAttempt(t, s, database, issue.Key, kind, "role:"+role)
				if tc.recordRole != "" {
					tx, err := database.Pool.Begin(ctx)
					if err != nil {
						t.Fatalf("begin the outbox's record: %v", err)
					}
					defer tx.Rollback(ctx)
					end := store.KeptDeliveryExpiry{EnvelopeID: tc.recordEnvelope, Role: tc.recordRole, PostKind: kind, PostID: attempt.post.ID}
					if err := store.RecordKeptDeliveryExpiry(ctx, tx, end, tc.forward); err != nil {
						t.Fatalf("record the end of the copy: %v", err)
					}
					if err := tx.Commit(ctx); err != nil {
						t.Fatalf("commit the outbox's record: %v", err)
					}
				}
				reported := envelope
				settledAs, err := attempt.settle(sendOutcome{envelopeID: &reported, queued: true})
				if err != nil {
					t.Fatalf("settle the attempt: %v", err)
				}
				state, failure, kept, receipts := attempt.read()
				if tc.failed {
					want := model.KeptDeliveryExpiredError(tc.forward)
					if settledAs != model.DeliveryStateFailed || state != model.DeliveryStateFailed || failure == nil || *failure != want ||
						kept != nil || receipts != keptReceipts(kind, model.DeliveryStateFailed, want) {
						t.Fatalf("the attempt settled %q, is %q with error %v and envelope %v, receipts %q; want it failed with %q, no envelope and a receipt that says so",
							settledAs, state, failure, kept, receipts, want)
					}
					return
				}
				if settledAs != model.DeliveryStateQueued || state != model.DeliveryStateQueued || failure != nil ||
					kept == nil || *kept != envelope || receipts != keptReceipts(kind, model.DeliveryStateQueued, "") {
					t.Fatalf("the attempt settled %q, is %q with error %v and envelope %v, receipts %q; want it queued under %q",
						settledAs, state, failure, kept, receipts, envelope)
				}
			})
		}
	}
}
