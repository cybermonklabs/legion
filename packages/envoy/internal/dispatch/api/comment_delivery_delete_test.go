package api

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// endedByThePostsDelete is the words a delete settles a pending send of its post with
// (deletedPostDeliveryError), spelled out here so a change to them is a change this file sees.
const endedByThePostsDelete = "ended by the post's delete"

// mentionRetryBehindADelete is a mention of s1 whose first attempt was sent, and a Retry of it
// readied on the stand-in listener: the comment, the server and store it is on, and the
// listener's state.
type mentionRetryBehindADelete struct {
	handler  http.Handler
	served   *server
	database *store.Store
	issueKey string
	comment  mentionedCommentRead
	listener *mentionListener
}

// newMentionRetryBehindADelete posts a comment of alice's mentioning session:s1 and waits for its
// first attempt to settle sent, so a Retry of it opens attempt 2.
func newMentionRetryBehindADelete(t *testing.T, title string) mentionRetryBehindADelete {
	t.Helper()
	listener, state := newMentionListener(t)
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL})
	issue := createInteractionIssue(t, handler, "TEST", title, "before")
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body":     "The mention a delete withdraws while its Retry is under way.",
		"mentions": []map[string]any{{"target": "session:s1"}},
	}))
	if settled := settledCommentDeliveries(t, database, comment.ID); len(settled) != 1 || settled[0].State != "sent" {
		t.Fatalf("the mention's first attempt = %#v, want one attempt sent to s1", settled)
	}
	return mentionRetryBehindADelete{
		handler: handler, served: served, database: database, issueKey: issue.Key, comment: comment, listener: state,
	}
}

// retry sends the person's Retry of the mention on another goroutine and answers on the channel.
func (m mentionRetryBehindADelete) retry(t *testing.T) <-chan *httptest.ResponseRecorder {
	t.Helper()
	retried := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		retried <- dispatchRequest(t, m.handler, http.MethodPost, "/api/v1/comments/"+m.comment.ID+"/deliveries",
			map[string]any{"delivery": "steer"}, "alice")
	}()
	return retried
}

// framesSent counts the sends of the comment the listener took for s1.
func (m mentionRetryBehindADelete) framesSent() int {
	sends, _, _ := framesOf(m.listener, "s1", m.comment.Body)
	return sends
}

// endedByTheDelete fails t unless the Retry answered its own attempt 2, failed with the delete's
// words, as the delete settled it.
func endedByTheDelete(t *testing.T, answer *httptest.ResponseRecorder) {
	t.Helper()
	if answer.Code != http.StatusCreated {
		t.Fatalf("the Retry: status=%d body=%s, want 201 with the attempt the delete settled", answer.Code, answer.Body.String())
	}
	if retried := decodeBody[commentDeliveryRead](t, answer); retried.Attempt != 2 || retried.State != model.DeliveryStateFailed ||
		deref(retried.Error) != endedByThePostsDelete {
		t.Fatalf("the Retry answered attempt %d %s %q, want attempt 2 failed %q",
			retried.Attempt, retried.State, deref(retried.Error), endedByThePostsDelete)
	}
}

// The settle beats the sender. A sender claims its attempt and then resolves the mention with
// nothing held; the comment's delete commits in between and settles that attempt, moving its
// claim. The record of the resolution is scoped to the claim, so it finds the attempt no longer
// the sender's, and the sender sends nothing: the listener takes no frame of the Retry, and the
// attempt reads as the delete left it. A sender that sent on a claim it no longer held put the
// withdrawn text on the wire after the delete had committed.
func TestAMentionSendWhoseClaimADeleteSettledSendsNothing(t *testing.T) {
	t.Parallel()
	mention := newMentionRetryBehindADelete(t, "A delete inside a send's resolution")
	// The Retry's resolution of s1 waits, between its claim and the record of that resolution on
	// the claim; the delete is written while it does.
	letResolve := mention.listener.holdLookups("/v1/registry/s1")
	t.Cleanup(letResolve)
	retried := mention.retry(t)
	waitForCommentDeliveries(t, mention.database, mention.comment.ID, "the Retry's claim of attempt 2",
		func(attempts []model.CommentDelivery) bool { return len(attempts) == 2 })
	deleteThreadComment(t, mention.handler, mention.comment.ID, "alice")
	letResolve()

	endedByTheDelete(t, testwait.Receive(t, "the Retry's answer", retried))
	if sends := mention.framesSent(); sends != 1 {
		t.Fatalf("the listener took %d frames of the comment, want its first attempt's alone: the Retry sent on a "+
			"claim the delete had settled", sends)
	}
}

// The settle beats the sender after the record too. The record of the sender's resolution finds
// the claim its own, and the sender then reads, with nothing held, whether s1 must take an earlier
// event of the comment's owner first (mentionCopyHolder); the comment's delete commits while it
// reads and settles the attempt, moving its claim. The claim is checked again after that read, in
// a statement scoped to it, so the sender sends nothing: the listener takes no frame of the Retry,
// and the attempt reads as the delete left it. A sender whose last word on its claim was the
// record put the withdrawn text on the wire after the delete had committed.
func TestAMentionSendWhoseClaimADeleteSettledDuringItsCopyRuleSendsNothing(t *testing.T) {
	t.Parallel()
	mention := newMentionRetryBehindADelete(t, "A delete inside a send's copy rule")
	// The Retry's send waits once its copy rule has answered, between the record of its
	// resolution on the claim and the re-check of that claim; the delete is written while it does.
	inCopyRule, release := make(chan struct{}), make(chan struct{})
	reached, letSend := sync.OnceFunc(func() { close(inCopyRule) }), sync.OnceFunc(func() { close(release) })
	t.Cleanup(letSend)
	mention.served.afterMentionCopyRule = func() {
		reached()
		<-release
	}
	retried := mention.retry(t)
	testwait.Receive(t, "the Retry's send reaching the end of its copy rule", inCopyRule)
	deleteThreadComment(t, mention.handler, mention.comment.ID, "alice")
	letSend()

	endedByTheDelete(t, testwait.Receive(t, "the Retry's answer", retried))
	if sends := mention.framesSent(); sends != 1 {
		t.Fatalf("the listener took %d frames of the comment, want its first attempt's alone: the Retry sent on a "+
			"claim the delete settled while its copy rule was read", sends)
	}
}

// The send first, then the delete: the Retry's frame reached s1 and only its answer was
// outstanding when the delete settled its attempt. The attempt and its receipt read the delete's
// words, and they stay true of that attempt, frame and all: the words say the delete ended it and
// nothing of whether a frame had gone out, which here it had.
func TestAMentionWhoseFrameLeftBeforeTheDeleteReadsEndedByThePostsDelete(t *testing.T) {
	t.Parallel()
	mention := newMentionRetryBehindADelete(t, "A delete after a send left")
	landed, answer := mention.listener.holdAnswer("s1")
	t.Cleanup(answer)
	retried := mention.retry(t)
	testwait.Receive(t, "the Retry's frame at the listener", landed)
	deleteThreadComment(t, mention.handler, mention.comment.ID, "alice")
	answer()

	endedByTheDelete(t, testwait.Receive(t, "the Retry's answer", retried))
	if sends := mention.framesSent(); sends != 2 {
		t.Fatalf("the listener took %d frames of the comment, want two: the Retry's left before the delete", sends)
	}
	if receipts, want := commentDeliveryReceipts(t, mention.database, mention.comment.ID, "session:s1", 2),
		"failed/"+endedByThePostsDelete+"//s1"; receipts != want {
		t.Fatalf("attempt 2's receipts = %q, want the one the delete's settle appended, %q", receipts, want)
	}
}
