package outbox

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// commitAtStatementStart runs the commit it was armed with, once, as the first statement its
// matches answers true for starts on the pool it traces (storetest.Traced), before that
// statement is sent: that statement's snapshot holds the commit, and every statement the code
// sent before it does not. The commit runs on a pool it does not trace, since a statement of its
// own on the traced pool would arm nothing and could wait for this one.
type commitAtStatementStart struct {
	matches func(sql string) bool
	mu      sync.Mutex
	commit  func()
	fired   bool
}

// commitAtStatement fires at the first statement whose SQL is sql, byte for byte.
func commitAtStatement(sql string) *commitAtStatementStart {
	return &commitAtStatementStart{matches: func(sent string) bool { return sent == sql }}
}

// commitAtStatementContaining fires at the first statement whose SQL holds text.
func commitAtStatementContaining(text string) *commitAtStatementStart {
	return &commitAtStatementStart{matches: func(sent string) bool { return strings.Contains(sent, text) }}
}

func (c *commitAtStatementStart) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !c.matches(data.SQL) {
		return ctx
	}
	c.mu.Lock()
	commit := c.commit
	c.commit = nil
	c.mu.Unlock()
	if commit != nil {
		commit()
		c.mu.Lock()
		c.fired = true
		c.mu.Unlock()
	}
	return ctx
}

func (c *commitAtStatementStart) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// Arm sets the commit the next statement this one matches starts with.
func (c *commitAtStatementStart) Arm(commit func()) {
	c.mu.Lock()
	c.commit, c.fired = commit, false
	c.mu.Unlock()
}

// Fired is whether the armed commit ran.
func (c *commitAtStatementStart) Fired() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fired
}

// deleteOfTheComment is the delete's own writes on comment, as one commit of another process:
// its settle of the post's pending attempts (queueDeletedPostSettlement in internal/dispatch/api),
// its clear of the comment (clearDeletedPostBody) and its settle of the comment's unsettled held
// copies (deleteComment), which the delete sends in that one transaction. deletes false is the
// control: the mention fails for a reason of its own and nothing is deleted or settled. It
// answers how many held rows the settle found, -1 for a control. It runs on database, a pool the
// test does not trace, since it can run inside a statement of the pass, whose connection the
// pass's tracked context holds.
func deleteOfTheComment(t *testing.T, database *store.Store, commentID string, deletes bool) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Errorf("begin the delete's commit: %v", err)
		return -1
	}
	defer tx.Rollback(ctx)
	failure := "the listener refused the send"
	if deletes {
		failure = contracts.DeletedPostDeliveryError
	}
	if tag, err := tx.Exec(ctx, `
		update comment_deliveries set state = 'failed', error = $2, claimed_at = now()
		where comment_id = $1 and state = 'pending'
	`, commentID, failure); err != nil || tag.RowsAffected() != 1 {
		t.Errorf("fail the pending attempt: %d rows (%v), want 1", tag.RowsAffected(), err)
		return -1
	}
	if !deletes {
		if err := tx.Commit(ctx); err != nil {
			t.Errorf("commit the failed mention: %v", err)
		}
		return -1
	}
	if tag, err := tx.Exec(ctx, `
		update comments set body = '', deleted_at = now(), deleted_by = '{"kind":"user","id":"alice"}'
		where id = $1
	`, commentID); err != nil || tag.RowsAffected() != 1 {
		t.Errorf("mark the comment deleted: %d rows (%v), want 1", tag.RowsAffected(), err)
		return -1
	}
	settled, err := tx.Exec(ctx, `
		update comment_fallback_copies set settled_at = now(), published = false
		where comment_id = $1 and settled_at is null
	`, commentID)
	if err != nil {
		t.Errorf("settle the held copies: %v", err)
		return -1
	}
	if err := tx.Commit(ctx); err != nil {
		t.Errorf("commit the delete: %v", err)
		return -1
	}
	return settled.RowsAffected()
}

// TestAFallbackPassPublishesNoCopyOfACommentItsAuthorDeleted: the author's delete of a comment
// marks it deleted, fails the mention its held copy waits on and settles that copy with nothing
// published, in one commit (deleteComment in internal/dispatch/api), so the copy's text is
// withdrawn. A failed mention is the one state in which a fallback pass publishes a held copy,
// and the pass reads it in statements of its own after its claim, so a delete can commit in
// between: the pass reads whether the comment is deleted after every read of its decision, and
// publishes nothing of a deleted comment. The delete lands, with no timing, at the end of the
// pass's claim (storetest.CommitBetweenReads) and at the start of its read of the comment's
// attempts (commitAtStatementStart), where a check read before the attempts would miss it. The
// control is the mention failing for a reason of its own, nothing deleted: the pass publishes the
// copy, so the rig shows a publish when one is owed. A row held after the delete, which the
// delete never settled, is the other window
// (TestAFallbackPassPublishesNoCopyHeldAfterItsCommentWasDeleted).
func TestAFallbackPassPublishesNoCopyOfACommentItsAuthorDeleted(t *testing.T) {
	const (
		issueTopic   = "notifications.dispatch.issue.T-1.comment.created"
		sessionTopic = "notifications.agent.s1"
	)
	for _, tc := range []struct {
		name string
		// atAttempts is whether the commit lands at the start of the pass's read of the comment's
		// attempts, rather than at the end of its claim.
		atAttempts bool
		// deletes is whether the commit is the author's delete; otherwise the mention fails for a
		// reason of its own and nothing is deleted.
		deletes bool
	}{
		{name: "the delete commits as the pass's claim ends", deletes: true},
		{name: "the delete commits as the pass's read of the comment's attempts starts", atAttempts: true, deletes: true},
		{name: "control: the mention fails and nothing deletes the comment", deletes: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := store.WithTransactionTracking(context.Background())
			database := storetest.Open(t)
			atClaim := storetest.NewCommitBetweenReads("comment_fallback_copies")
			atAttempts := commitAtStatement(mentionDeliveryQuery)
			traced := storetest.TracedStore(t, database, atClaim)
			if tc.atAttempts {
				traced = storetest.TracedStore(t, database, atAttempts)
			}
			publisher := &recordingPublisher{}
			deps := Deps{Store: traced, Publisher: publisher, Broker: events.NewBroker()}
			commentID := seedHeldFallbackCopy(ctx, t, deps)
			// The mention is being sent again: a sender claimed its attempt just now, a lease
			// after the attempt was made, so a pass that read it before the commit would leave
			// the row to a later pass (TestAFallbackCopyWhoseMentionIsStillBeingSentIsClaimedAgainOnlyOnceItsWaitHasPassed).
			if _, err := database.Pool.Exec(ctx, `
				update comment_deliveries set state = 'pending', error = null, claimed_at = now() where comment_id = $1
			`, commentID); err != nil {
				t.Fatalf("put the mention back in flight: %v", err)
			}
			var settledByTheDelete int64
			commit := func() { settledByTheDelete = deleteOfTheComment(t, database, commentID, tc.deletes) }
			if tc.atAttempts {
				atAttempts.Arm(commit)
			} else {
				atClaim.Arm(commit)
			}
			var logged lockedBuffer
			logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))

			_, failed := publishFallbackCopies(ctx, deps, unfenced{}, holdingProcess(t, database))
			if failed {
				t.Fatalf("the pass's claim failed; it logged:\n%s", logged.String())
			}
			if logs := logged.String(); strings.Contains(logs, "level=ERROR") {
				t.Fatalf("the pass logged at Error, so it decided nothing the test reads: %s", logs)
			}
			switch {
			case tc.atAttempts && !atAttempts.Fired():
				t.Fatal("the pass never read the comment's attempts, so the commit never landed at that read")
			case !tc.atAttempts && !atClaim.Fired():
				t.Fatal("the pass sent no statement on comment_fallback_copies, so the commit never landed at its claim")
			}
			row := readHeldCopy(t, database, commentID, sessionTopic)
			published := topicsOf(publisher.all())
			if tc.deletes {
				if settledByTheDelete != 1 {
					t.Fatalf("the delete's settle found %d held rows, want the one the scan held", settledByTheDelete)
				}
				if !row.settled || row.published {
					t.Fatalf("the held copy is settled=%v published=%v, want it settled with nothing published: the author deleted the comment", row.settled, row.published)
				}
				if !slices.Equal(published, []string{issueTopic}) {
					t.Fatalf("the scan and the pass published %v, want the scan's issue topic alone: the pass published a copy of a deleted comment", published)
				}
				return
			}
			if !row.settled || !row.published {
				t.Fatalf("the held copy is settled=%v published=%v, want it settled and published: its mention failed and nothing withdrew it", row.settled, row.published)
			}
			if !slices.Equal(published, []string{issueTopic, sessionTopic}) {
				t.Fatalf("the scan and the pass published %v, want the issue topic and then s1's copy", published)
			}
		})
	}
}

// TestAFallbackPassPublishesNoCopyHeldAfterItsCommentWasDeleted: the other window of the same
// defect. A batch that read a comment's mention as resumed past its lease holds that session's
// copy as a comment_fallback_copies row (holdFallbackCopy), and that insert can land after the
// author's delete committed: the delete's settle finds no row of the comment, so the row is
// inserted unsettled, with the mention already failed by the delete - the state in which a pass
// publishes. The pass reads whether the comment is deleted, not whether the row survived a
// delete, so it publishes nothing of this row either, and settles it with nothing sent.
//
// The delete's commit lands as the scan's insert starts (commitAtStatementContaining), with no
// timing, and the test reads from the delete itself that it found no row to settle. The control
// is the same commit with the mention failing for a reason of its own, nothing deleted: the pass
// publishes the copy.
func TestAFallbackPassPublishesNoCopyHeldAfterItsCommentWasDeleted(t *testing.T) {
	const (
		issueTopic   = "notifications.dispatch.issue.T-1.comment.created"
		sessionTopic = "notifications.agent.s1"
	)
	for _, tc := range []struct {
		name    string
		deletes bool
	}{
		{name: "the delete commits as the scan's insert of the held row starts", deletes: true},
		{name: "control: the mention fails as that insert starts, nothing deleted", deletes: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := store.WithTransactionTracking(context.Background())
			database := storetest.Open(t)
			atInsert := commitAtStatementContaining("insert into comment_fallback_copies")
			publisher := &recordingPublisher{}
			deps := Deps{
				Store: storetest.TracedStore(t, database, atInsert), Publisher: publisher,
				Broker: events.NewBroker(),
			}
			// The comment of seedHeldFallbackCopy, whose scan this test runs itself: its mention
			// is resumed past the lease, so the scan holds s1's copy as a row.
			route := "session:s1"
			seedIssue(t, deps.Store, "T-1", &route)
			alice := model.Actor{Kind: "user", ID: "alice"}
			commentID := seedComment(t, deps.Store, "T-1", alice, "@s1 are you there?", nil)
			appendEvent(t, deps.Store, deps.Broker, model.Event{
				IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
				Payload: model.CommentEventPayload{
					Comment: model.Comment{
						ID: commentID, IssueKey: new("T-1"), Body: "@s1 are you there?", Mentions: mentionOf("s1"),
					},
					SuppressRoute: true, SuppressedRoute: route, SuppressedRouteSessionID: new("s1"),
				},
			})
			seedMentionDelivery(t, deps.Store, commentID, "s1", "resumed")
			settledByTheDelete := int64(-1)
			atInsert.Arm(func() { settledByTheDelete = deleteOfTheComment(t, database, commentID, tc.deletes) })

			holding(t, deps).scan(ctx, unfenced{})

			if !atInsert.Fired() {
				t.Fatal("the scan inserted no held row, so the commit never landed at that insert")
			}
			if tc.deletes && settledByTheDelete != 0 {
				t.Fatalf("the delete's settle found %d held rows, want none: the scan holds its row after the delete", settledByTheDelete)
			}
			if row := readHeldCopy(t, database, commentID, sessionTopic); row.settled {
				t.Fatalf("the scan's held row is settled=%v published=%v before any pass, want it unsettled: the delete never saw it",
					row.settled, row.published)
			}
			var logged lockedBuffer
			logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))

			_, failed := publishFallbackCopies(ctx, deps, unfenced{}, holdingProcess(t, database))
			if failed {
				t.Fatalf("the pass's claim failed; it logged:\n%s", logged.String())
			}
			if logs := logged.String(); strings.Contains(logs, "level=ERROR") {
				t.Fatalf("the pass logged at Error, so it decided nothing the test reads: %s", logs)
			}
			row := readHeldCopy(t, database, commentID, sessionTopic)
			published := topicsOf(publisher.all())
			if tc.deletes {
				if !slices.Equal(published, []string{issueTopic}) {
					t.Fatalf("the scan and the pass published %v, want the scan's issue topic alone: the pass published a copy held after the comment was deleted", published)
				}
				if !row.settled || row.published {
					t.Fatalf("the held copy is settled=%v published=%v, want it settled with nothing published: the author deleted the comment", row.settled, row.published)
				}
				return
			}
			if !row.settled || !row.published {
				t.Fatalf("the held copy is settled=%v published=%v, want it settled and published: its mention failed and nothing withdrew it", row.settled, row.published)
			}
			if !slices.Equal(published, []string{issueTopic, sessionTopic}) {
				t.Fatalf("the scan and the pass published %v, want the issue topic and then s1's copy", published)
			}
		})
	}
}
