package api

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
)

// deliveryReadFault ends a reply route's read of the attempt it answers, the row it locks inside
// its transaction (message_deliveries or comment_deliveries ... for update), while armed, as a
// statement timeout or a reset pooled connection ends it: that one statement is sent with a
// context already ended, and every other statement runs.
type deliveryReadFault struct {
	armed atomic.Bool
	// table is the deliveries table whose locked read the fault ends.
	table string
}

func (f *deliveryReadFault) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	read := strings.Contains(data.SQL, "from "+f.table+" where") && strings.Contains(data.SQL, "and attempt = ") &&
		strings.Contains(data.SQL, "for update")
	if !f.armed.Load() || !read {
		return ctx
	}
	ended, cancel := context.WithCancel(ctx)
	cancel()
	return ended
}

func (*deliveryReadFault) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// A reply to a role route's delivery reads the attempt it answers under lock. A read that failed
// - a statement timeout, a reset connection - says nothing about whether the delivery exists, so
// it is a 500 the holder can reply again after, and not the 404 the route gives an attempt no
// message has, which tells the holder the delivery is gone (role_route_reply.go draws the same
// line for its own read). The reply is the role's live holder's, B, to an attempt sent to A. The
// controls: an attempt number the message has no row for is still the 404 MESSAGE_NOT_FOUND, and
// the same reply once the read answers is accepted.
func TestAMessageReplyWhoseLockedDeliveryReadFailsIsAServerError(t *testing.T) {
	t.Parallel()
	fault := &deliveryReadFault{table: "message_deliveries"}
	rig := newRoleRouteRepliesTraced(t, fault)
	const role = "reply-delivery-read-fails"
	rig.listener.claim(role, "ses_role_a", true)
	message := rig.message(t, role, "ses_role_a", "Is the index rebuilt?")
	rig.listener.claim(role, "ses_role_b", true)

	fault.armed.Store(true)
	failed := rig.replyMessage(t, message, "ses_role_b", map[string]any{"body": "Rebuilt."}, false)
	fault.armed.Store(false)
	if failed.Code != http.StatusInternalServerError || strings.Contains(failed.Body.String(), "MESSAGE_NOT_FOUND") {
		t.Fatalf("the role's live holder's reply while the locked read of its attempt fails: status=%d body=%s, want 500 and no MESSAGE_NOT_FOUND", failed.Code, failed.Body.String())
	}
	if _, _, replyID := rig.messageAttempt(t, message); replyID != nil {
		t.Fatalf("the message's attempt has reply %s after a reply answered 500; want none", *replyID)
	}

	missing := bearerRequest(t, rig.handler, http.MethodPost, "/api/v1/messages/"+message.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "ses_role_b"}, "attempt": 7, "body": "Rebuilt.",
	})
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), `"code":"MESSAGE_NOT_FOUND"`) {
		t.Fatalf("control: a reply to an attempt the message has no row for: status=%d body=%s, want 404 MESSAGE_NOT_FOUND", missing.Code, missing.Body.String())
	}
	if answered := rig.replyMessage(t, message, "ses_role_b", map[string]any{"body": "Rebuilt."}, false); answered.Code != http.StatusCreated {
		t.Fatalf("control: the same reply once the read answers: status=%d body=%s, want 201", answered.Code, answered.Body.String())
	}
}

// The comment reply route's case of TestAMessageReplyWhoseLockedDeliveryReadFailsIsAServerError:
// the locked read of the mention's attempt failing is a 500, not the 404 COMMENT_NOT_FOUND an
// attempt the mention has no row for still is.
func TestACommentReplyWhoseLockedDeliveryReadFailsIsAServerError(t *testing.T) {
	t.Parallel()
	fault := &deliveryReadFault{table: "comment_deliveries"}
	rig := newRoleRouteRepliesTraced(t, fault)
	const role = "mention-delivery-read-fails"
	rig.listener.claim(role, "ses_role_a", true)
	comment := rig.mention(t, role, "ses_role_a", "Is the backfill done?")
	rig.listener.claim(role, "ses_role_b", true)

	fault.armed.Store(true)
	failed := rig.replyComment(t, comment, "ses_role_b", map[string]any{"body": "Done."})
	fault.armed.Store(false)
	if failed.Code != http.StatusInternalServerError || strings.Contains(failed.Body.String(), "COMMENT_NOT_FOUND") {
		t.Fatalf("the role's live holder's reply while the locked read of its attempt fails: status=%d body=%s, want 500 and no COMMENT_NOT_FOUND", failed.Code, failed.Body.String())
	}
	if _, _, replyID := rig.commentAttempt(t, comment); replyID != nil {
		t.Fatalf("the mention's attempt has reply %s after a reply answered 500; want none", *replyID)
	}

	missing := bearerRequest(t, rig.handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "ses_role_b"}, "attempt": 7, "body": "Done.",
	})
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), `"code":"COMMENT_NOT_FOUND"`) {
		t.Fatalf("control: a reply to an attempt the mention has no row for: status=%d body=%s, want 404 COMMENT_NOT_FOUND", missing.Code, missing.Body.String())
	}
	if answered := rig.replyComment(t, comment, "ses_role_b", map[string]any{"body": "Done."}); answered.Code != http.StatusCreated {
		t.Fatalf("control: the same reply once the read answers: status=%d body=%s, want 201", answered.Code, answered.Body.String())
	}
}
