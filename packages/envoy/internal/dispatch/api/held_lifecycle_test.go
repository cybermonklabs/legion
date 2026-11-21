package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/logtest"
)

// The life of a held text past its hold (INFRA-3231's R5, Decision 4): a reply to a held message
// once it posts, a post its document refuses for good and one it refuses for now, the replies of
// one writer to one delivery (two held ones, a generated or typed one beside a held one, one sent
// again while the first is held, a role holder's), a text that waits on a held parent, a kept
// write's one run and its repeat under its key, the issue a held issue's answer names, a scoped
// token's write beside a held text, a person's write beside one, the writer's late answer to a
// findings frame, a reply sent again on an answered delivery, and the counts of one write's rows.

// asKeptWriter writes as the rig's writer session through a client that reads the answer of a
// write Dispatch keeps beside a held text (contracts.AcceptsKeptWriteHeader), as the agents'
// DispatchClient does on every write.
func (rig *reviewRig) asKeptWriter(method, target string, body map[string]any) *httptest.ResponseRecorder {
	rig.t.Helper()
	return rig.asWriterMarked(contracts.AcceptsKeptWriteHeader, method, target, body)
}

// postHeldAsWritten is the writer's post of the held text id as written.
func (rig *reviewRig) postHeldAsWritten(id string) {
	rig.t.Helper()
	if posted := rig.asWriter(http.MethodPut, "/api/v1/held/"+id+"/state", map[string]any{"state": "posted", "reason": "As written."}); posted.Code != http.StatusOK {
		rig.t.Fatalf("the writer's post of %s: %d %s", id, posted.Code, posted.Body.String())
	}
}

// waitDecided waits for the held text id to post or fail, and answers it.
func (rig *reviewRig) waitDecided(id string) heldRow {
	rig.t.Helper()
	var row heldRow
	pollUntil(rig.t, 20*time.Second, "held text "+id+" posted or failed", func() bool {
		row = rig.heldRow(id)
		return row.state == heldPosted || row.state == heldFailed
	})
	return row
}

// holdFixture holds a text of kind under id in project, written by s-writer for alice and
// awaiting its writer for an hour, with no request a post could run: a text a write may name.
func holdFixture(t *testing.T, database *store.Store, id, kind, project string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into held_texts (id, kind, route, request, original, actor, writer, reader, project, state, deadline)
		values ($1, $2, 'POST /fixture', '{}', '{}', '{"kind":"session","id":"s-writer"}', 's-writer', 'alice', $3,
			'awaiting_writer', now() + interval '1 hour')`, id, kind, project); err != nil {
		t.Fatalf("hold the fixture %s: %v", id, err)
	}
}

// A held message to a session posts under the id its hold reserved, and the recipient's reply to
// that delivery is a reply like any other: the held row under the same id, posted, is no frame
// for the writer to answer any more.
func TestARecipientRepliesToAHeldMessageOnceItPosts(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Messages: ReviewHold, WriterWindow: time.Minute}, nil)
	const recipient = "s-recipient"
	rig.listener.start(recipient)
	held := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/messages", map[string]any{
		"body": "Status\nWe spin up the node. " + vacuousPhrase, "target": "session:" + recipient, "delivery": "btw",
	}), "the agent's message to a session")
	rig.waitHeld(held.ID, heldAwaitingWriter)
	rig.postHeldAsWritten(held.ID)
	rig.waitHeld(held.ID, heldPosted)
	pollUntil(t, 10*time.Second, "the posted message's delivery to "+recipient, func() bool {
		return countRows(t, rig.database, `select count(*) from message_deliveries where message_id::text = $1 and session_id = $2`, held.ID, recipient) == 1
	})
	reply := rig.asSession(recipient, http.MethodPost, "/api/v1/messages/"+held.ID+"/reply", map[string]any{
		"attempt": 1, "body": "The node answers on its port now.",
	})
	if reply.Code != http.StatusCreated {
		t.Fatalf("the recipient's reply to the posted message: %d %s, want 201", reply.Code, reply.Body.String())
	}
	if id := decodeBody[heldWriteAnswer](t, reply).ID; id == "" || id == held.ID {
		t.Fatalf("the recipient's reply answered id %q, want one of its own", id)
	}
	if answer := rig.heldRow(held.ID).answer; answer != "" {
		t.Fatalf("the recipient's reply was kept as the writer's answer %q", answer)
	}
}

// The writer's answer to the findings frame of a held comment is kept with the held text, 202,
// after the comment posted as while it was held: a comment's id names no message, so the reply
// route has no delivery to take the answer as a reply to.
func TestTheWritersAnswerToAPostedCommentsFindingsIsKept(t *testing.T) {
	rig := newReviewRig(t, holdComments(), nil)
	held := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/comments", map[string]any{
		"body": "Status\nWe spin up the node. " + vacuousPhrase,
	}), "the held comment")
	rig.waitHeld(held.ID, heldAwaitingWriter)
	rig.postHeldAsWritten(held.ID)
	rig.waitHeld(held.ID, heldPosted)
	const answer = "The comment is right for its reader as written."
	reply := rig.asWriter(http.MethodPost, "/api/v1/messages/"+held.ID+"/reply", map[string]any{"attempt": 1, "body": answer})
	if reply.Code != http.StatusAccepted {
		t.Fatalf("the writer's answer to the posted comment's findings: %d %s, want 202", reply.Code, reply.Body.String())
	}
	if kept := rig.heldRow(held.ID).answer; kept != answer {
		t.Fatalf("the held comment keeps the answer %q, want %q", kept, answer)
	}
}

// A person signed in at a browser learns nothing of a held text: their write on a held issue's or
// a held comment's path, and their create of a comment on a held issue or of a child of one, with
// the kept-write header or without, is the route's own, answered as the same write naming an id
// nobody holds is answered, naming no held text, and nothing is kept or held.
func TestAPersonsWriteOnAHeldTextIsAnsweredAsIfNoTextWereThere(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Issues: ReviewHold, Comments: ReviewHold, WriterWindow: time.Hour}, nil)
	heldComment, nobodysComment := "0f6a4c1e-6d2b-4b8e-9c55-2a1d7e3f9b10", "5b9e2d7c-3a41-4f6e-8d02-7c1b9e4a6f23"
	holdFixture(t, rig.database, "RVW-900", "issue", "RVW")
	holdFixture(t, rig.database, heldComment, "comment", "RVW")
	send := func(method, target string, body map[string]any, readsKept bool) *httptest.ResponseRecorder {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(method, target, bytes.NewReader(data))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Dispatch-User", "alice")
		if readsKept {
			request.Header.Set(contracts.AcceptsKeptWriteHeader, "1")
		}
		response := httptest.NewRecorder()
		rig.handler.ServeHTTP(response, request)
		return response
	}
	retitle, resolve, comment := map[string]any{"title": "Retitled by alice"}, map[string]any{}, map[string]any{"body": "A comment by alice."}
	child := func(parent string) map[string]any {
		return map[string]any{"project": "RVW", "title": "A child alice files", "parent": parent}
	}
	for _, write := range []struct {
		method, held, nobodys string
		heldBody, nobodysBody map[string]any
	}{
		{http.MethodPatch, "/api/v1/issues/RVW-900", "/api/v1/issues/RVW-901", retitle, retitle},
		{http.MethodPost, "/api/v1/comments/" + heldComment + "/resolve", "/api/v1/comments/" + nobodysComment + "/resolve", resolve, resolve},
		{http.MethodPost, "/api/v1/issues/RVW-900/comments", "/api/v1/issues/RVW-901/comments", comment, comment},
		{http.MethodPost, "/api/v1/issues", "/api/v1/issues", child("RVW-900"), child("RVW-901")},
	} {
		want := send(write.method, write.nobodys, write.nobodysBody, false)
		for _, readsKept := range []bool{false, true} {
			got := send(write.method, write.held, write.heldBody, readsKept)
			if got.Code != want.Code || strings.Contains(got.Body.String(), "HELD_TEXT") || strings.Contains(got.Body.String(), `"held"`) {
				t.Fatalf("alice's %s %s (kept-write header %v): %d %s, want %d as for an id nobody holds",
					write.method, write.held, readsKept, got.Code, got.Body.String(), want.Code)
			}
		}
	}
	if held := countRows(t, rig.database, `select count(*) from held_texts`); held != 2 {
		t.Fatalf("%d held texts beside the two fixtures, want none of alice's writes kept or held", held-2)
	}
}

// A held comment anchored by a quote its issue's spec loses while the comment is held is refused
// at its post, as the comment itself would be refused now: the held text fails at once, saying
// why, and its writer is told.
func TestAHeldPostItsDocumentRefusesFailsAtOnce(t *testing.T) {
	rig := newReviewRig(t, holdComments(), nil)
	held := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/comments", map[string]any{
		"body":   "Status\nWe spin up the node. " + vacuousPhrase,
		"anchor": map[string]any{"artifact": "spec", "quote": "Seed paragraph"},
	}), "the anchored comment")
	rig.waitHeld(held.ID, heldAwaitingWriter)
	edited := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+rig.issue+"/artifacts/spec/edits", map[string]any{
		"ops":     []map[string]string{{"op": "replace", "find": "Seed paragraph here.", "with": "Another line entirely."}},
		"summary": "Reword the spec",
	}, "alice")
	if edited.Code != http.StatusOK {
		t.Fatalf("alice's edit of the spec: %d %s", edited.Code, edited.Body.String())
	}
	rig.postHeldAsWritten(held.ID)
	if failed := rig.waitHeld(held.ID, heldFailed); !strings.Contains(failed.detail, "the post was refused: ") ||
		!strings.HasSuffix(failed.detail, "; its writer was told") {
		t.Fatalf("the failed comment's detail reads %q", failed.detail)
	}
	if body := rig.waitOneFrameAbout(held.ID, "did not post"); !strings.Contains(body, "your held comment "+held.ID) {
		t.Fatalf("the failed comment's frame reads %q", body)
	}
	if slices.Contains(rig.commentIDs(), held.ID) {
		t.Fatal("alice reads the comment its document refused")
	}
}

// A held post refused for what a later post can clear is left posting for the sweep to post
// again, neither failed nor told to its writer as not posted: a comment anchored in a document
// that a newer build wrote an element into (409 DOC_NEWER_CONTENT), which the post goes through
// on once a build that reads the element serves it.
func TestAHeldPostANewerDocumentRefusesWaitsForTheNextPost(t *testing.T) {
	rig := newReviewRig(t, holdComments(), nil)
	held := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/comments", map[string]any{
		"body":   "Status\nWe spin up the node. " + vacuousPhrase,
		"anchor": map[string]any{"artifact": "spec", "quote": "Seed paragraph"},
	}), "the anchored comment")
	rig.waitHeld(held.ID, heldAwaitingWriter)
	var spec string
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select id::text from artifacts where issue_key = $1 and slug = 'spec'`, rig.issue).Scan(&spec); err != nil {
		t.Fatalf("read the issue's spec: %v", err)
	}
	if err := rig.served.deps.Docs.(*docs.Service).InjectUnknownElementForTest(context.Background(), spec); err != nil {
		t.Fatalf("write an element this build does not know: %v", err)
	}
	rig.postHeldAsWritten(held.ID)
	if row := rig.heldRow(held.ID); row.state != heldPosting || row.outcome != reviewPostedAsWritten || strings.Contains(row.detail, "refused") {
		t.Fatalf("the comment is %s %s (%s) after its newer document refused its post, want it left posting", row.state, row.outcome, row.detail)
	}
	if bodies := rig.framesAbout(held.ID, "did not post"); len(bodies) != 0 {
		t.Fatalf("the writer was told the comment did not post: %q", bodies)
	}
}

// Two replies of one writer to one delivery, each held, post in the order the writer wrote them:
// the second waits on the first and is reviewed once the first posted, so the first posts as the
// delivery's reply under its own id, and the second as the follow-up it asked to be, threaded
// under the first.
func TestTwoHeldRepliesOfOneWriterPostInTheOrderWritten(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Messages: ReviewHold, WriterWindow: time.Minute}, nil)
	message := rig.messageToWriter("Is it done?")
	first := rig.held(rig.asWriter(http.MethodPost, "/api/v1/messages/"+message+"/reply", map[string]any{
		"attempt": 1, "body": "Status\nWe spin up the node. " + vacuousPhrase,
	}), "the first reply")
	rig.waitHeld(first.ID, heldAwaitingWriter)
	second := rig.held(rig.asWriter(http.MethodPost, "/api/v1/messages/"+message+"/reply?follow_up=true", map[string]any{
		"attempt": 1, "body": "The node answers on its port now.",
	}), "the second reply")
	if second.Held.WaitsOn != first.ID {
		t.Fatalf("the second reply waits on %q, want the first, %s", second.Held.WaitsOn, first.ID)
	}
	time.Sleep(300 * time.Millisecond)
	if row := rig.heldRow(second.ID); row.state != heldReviewing {
		t.Fatalf("the second reply is %s (%s) before the first posted, want it still in review", row.state, row.detail)
	}
	rig.postHeldAsWritten(first.ID)
	if posted := rig.waitHeld(second.ID, heldPosted); posted.outcome != reviewPassed {
		t.Fatalf("the second reply posted %s (%s), want passed", posted.outcome, posted.detail)
	}
	rig.requireAnsweredBy(message, first.ID)
	rig.requireThreadedUnder(second.ID, first.ID)
}

// requireAnsweredBy fails the test unless attempt 1 of message is answered by the reply id.
func (rig *reviewRig) requireAnsweredBy(message, id string) {
	rig.t.Helper()
	var answeredBy string
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select coalesce(reply_id::text, '') from message_deliveries where message_id::text = $1 and attempt = 1`, message).Scan(&answeredBy); err != nil {
		rig.t.Fatal(err)
	}
	if answeredBy != id {
		rig.t.Fatalf("the delivery is answered by %q, want %s", answeredBy, id)
	}
}

// requireThreadedUnder fails the test unless the message id answers the message parent.
func (rig *reviewRig) requireThreadedUnder(id, parent string) {
	rig.t.Helper()
	var threadedUnder string
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select coalesce(in_reply_to::text, '') from messages where id::text = $1`, id).Scan(&threadedUnder); err != nil {
		rig.t.Fatalf("read the message %s: %v", id, err)
	}
	if threadedUnder != parent {
		rig.t.Fatalf("the message %s answers %q, want %s", id, threadedUnder, parent)
	}
}

// A reply the reviewer does not read - a plugin's generated text, a line a person typed in the
// writer's session - sent while the same writer's reply to the same delivery is held, is held
// beside it unreviewed: the held reply posts as the delivery's reply once its writer posts it,
// and the later one as the follow-up it asked to be, threaded under the first, no review reading
// it.
func TestAGeneratedOrTypedReplyWaitsOnTheWritersHeldReply(t *testing.T) {
	for name, marker := range map[string]string{"generated": contracts.GeneratedTextHeader, "typed": contracts.PersonTypedHeader} {
		t.Run(name, func(t *testing.T) {
			rig := newReviewRig(t, ReviewerConfig{Messages: ReviewHold, WriterWindow: time.Minute}, nil)
			message := rig.messageToWriter("Is it done?")
			first := rig.held(rig.asWriter(http.MethodPost, "/api/v1/messages/"+message+"/reply", map[string]any{
				"attempt": 1, "body": "Status\nWe spin up the node. " + vacuousPhrase,
			}), "the writer's reply")
			rig.waitHeld(first.ID, heldAwaitingWriter)
			const later = "The node answers on its port now."
			second := rig.held(rig.asWriterMarked(marker, http.MethodPost, "/api/v1/messages/"+message+"/reply?follow_up=true", map[string]any{
				"attempt": 1, "body": later,
			}), "the "+name+" reply")
			if second.Held.WaitsOn != first.ID {
				t.Fatalf("the %s reply waits on %q, want the writer's held reply, %s", name, second.Held.WaitsOn, first.ID)
			}
			if row := rig.heldRow(second.ID); row.state != heldPosting || row.reviewed || row.outcome != heldNotReviewed {
				t.Fatalf("the %s reply is %s, reviewed %v, outcome %q, want posting unreviewed", name, row.state, row.reviewed, row.outcome)
			}
			rig.postHeldAsWritten(first.ID)
			rig.waitHeld(second.ID, heldPosted)
			rig.requireAnsweredBy(message, first.ID)
			rig.requireThreadedUnder(second.ID, first.ID)
			if calls := rig.reviewCalls(later); calls != 0 {
				t.Fatalf("the reviewer read the %s reply %d times, want never", name, calls)
			}
		})
	}
}

// A reply sent again on a delivery while the writer's first reply to it is held writes nothing
// and is answered the first's held answer, 200, under the first's id with its `held` object, on
// the reply route of a delivered message, marked duplicate as that route marks a repeat, and of a
// delivered mention; nothing more is held, and the first then posts as the delivery's reply.
func TestAReplySentAgainWhileTheFirstIsHeldAnswersTheFirst(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Comments: ReviewHold, Messages: ReviewHold, WriterWindow: time.Minute}, nil)
	for _, route := range []struct {
		pattern   string
		delivered func() string
		duplicate bool
	}{
		{"/api/v1/messages/{id}/reply", func() string { return "/api/v1/messages/" + rig.messageToWriter("Is it done?") + "/reply" }, true},
		{"/api/v1/comments/{id}/reply", func() string { return "/api/v1/comments/" + rig.mentionOfWriter("Is it done?") + "/reply" }, false},
	} {
		target := route.delivered()
		body := map[string]any{"attempt": 1, "body": "Status\nWe spin up the node. " + vacuousPhrase}
		first := rig.held(rig.asWriter(http.MethodPost, target, body), "the first reply on "+route.pattern)
		rig.waitHeld(first.ID, heldAwaitingWriter)
		before := countRows(t, rig.database, `select count(*) from held_texts`)
		again := rig.asWriter(http.MethodPost, target, body)
		if again.Code != http.StatusOK {
			t.Fatalf("the reply sent again on %s: %d %s, want 200 and the first's held answer", route.pattern, again.Code, again.Body.String())
		}
		answer := decodeBody[struct {
			heldWriteAnswer
			Duplicate bool `json:"duplicate"`
		}](t, again)
		if answer.ID != first.ID || answer.Held == nil || answer.Held.State != heldAwaitingWriter || answer.Duplicate != route.duplicate {
			t.Fatalf("the reply sent again on %s answered %s, want the first's id %s awaiting its writer, duplicate %v",
				route.pattern, again.Body.String(), first.ID, route.duplicate)
		}
		if after := countRows(t, rig.database, `select count(*) from held_texts`); after != before {
			t.Fatalf("the reply sent again on %s held %d texts, want none", route.pattern, after-before)
		}
		rig.postHeldAsWritten(first.ID)
		rig.waitHeld(first.ID, heldPosted)
	}
}

// A held reply of the session that holds the role its delivery was sent for posts on what the
// listener said when the reply was written: the session took the role from the one the attempt
// names, wrote its reply, and lost the role while the reply was held, and the reply posts as the
// attempt's reply all the same, its post asking the listener nothing.
func TestAHeldReplyOfARolesHolderPostsOnceTheRoleMovesOn(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Messages: ReviewHold, WriterWindow: time.Minute}, nil)
	const role, first, next = "reply-holder", "s-first-holder", "s-next-holder"
	rig.listener.start(first)
	rig.listener.start(next)
	rig.listener.claim(role, first)
	sent := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+rig.issue+"/messages", map[string]any{
		"body": "Is it done?", "target": "role:" + role, "delivery": "steer",
	}, "alice")
	if sent.Code != http.StatusCreated {
		t.Fatalf("alice's message to role:%s: %d %s", role, sent.Code, sent.Body.String())
	}
	message := decodeBody[model.Message](t, sent)
	if len(message.Deliveries) != 1 || message.Deliveries[0].SessionID != first {
		t.Fatalf("the message to role:%s = %#v, want one attempt sent to %s", role, message.Deliveries, first)
	}
	rig.listener.claim(role, heldWriter)
	held := rig.held(rig.asWriter(http.MethodPost, "/api/v1/messages/"+message.ID+"/reply", map[string]any{
		"attempt": 1, "body": "Status\nWe spin up the node. " + vacuousPhrase,
	}), "the role holder's reply")
	rig.waitHeld(held.ID, heldAwaitingWriter)
	rig.listener.claim(role, next)
	rig.postHeldAsWritten(held.ID)
	if row := rig.waitDecided(held.ID); row.state != heldPosted {
		t.Fatalf("the role holder's reply is %s (%s) once the role moved on, want it posted", row.state, row.detail)
	}
	rig.requireAnsweredBy(message.ID, held.ID)
}

// A child issue held beside its held parent is not reviewed while the parent is held, since its
// record would miss the parent, and is reviewed once the parent posts, posting passed.
func TestAHeldTextWaitingOnAHeldParentIsReviewedOnceTheParentPosts(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Issues: ReviewHold, WriterWindow: time.Minute}, nil)
	const childTitle = "Held child of a held parent"
	parent := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": "Held parent", "spec": vacuousPhrase,
	}), "the held parent")
	rig.waitHeld(parent.ID, heldAwaitingWriter)
	child := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": childTitle, "parent": parent.ID,
	}), "the child")
	time.Sleep(500 * time.Millisecond)
	if row := rig.heldRow(child.ID); row.state != heldReviewing || row.outcome != "" {
		t.Fatalf("the child is %s %q (%s) while its parent is held, want it in review, undecided", row.state, row.outcome, row.detail)
	}
	if calls := rig.reviewCalls(childTitle); calls != 0 {
		t.Fatalf("the reviewer read the child %d times while its parent was held, want none", calls)
	}
	rig.postHeldAsWritten(parent.ID)
	if posted := rig.waitHeld(child.ID, heldPosted); posted.outcome != reviewPassed {
		t.Fatalf("the child posted %s (%s), want passed", posted.outcome, posted.detail)
	}
	if calls := rig.reviewCalls(childTitle); calls != 1 {
		t.Fatalf("the reviewer read the child %d times, want once", calls)
	}
}

// A kept write marks its held row posted in the transaction of the write itself. A post of the
// write whose row another post has marked already - one that took it past staleClaim while the
// first ran - writes nothing: its route's commit is refused, the write rolls back, and the post
// answers that the text no longer waits to post (errHeldNotPosting), logging no error. A post of
// the row still waiting runs the write and leaves the row posted.
func TestAKeptWriteWhoseHeldRowPostedRunsNoSecondTime(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Issues: ReviewHold, WriterWindow: time.Hour}, nil)
	ctx := context.Background()
	// The rig's issue stands in for a held issue, so a write naming it is kept, and its replay
	// writes a row that exists.
	holdFixture(t, rig.database, rig.issue, "issue", "RVW")
	kept := rig.held(rig.asKeptWriter(http.MethodPatch, "/api/v1/issues/"+rig.issue, map[string]any{"labels": []string{"kept"}}), "the kept update")
	labels := func() []string {
		var labels []string
		if err := rig.database.Pool.QueryRow(ctx, `select labels from issues where key = $1`, rig.issue).Scan(&labels); err != nil {
			t.Fatal(err)
		}
		return labels
	}
	if _, err := rig.database.Pool.Exec(ctx, `
		update held_texts set state = 'posted', posted_at = now() where id = $1`, kept.ID); err != nil {
		t.Fatal(err)
	}
	logs := &logtest.Recorder{}
	logtest.SetDefault(t, logs)
	if err := rig.served.runHeldPost(rig.served.postContext(), rig.heldRow(kept.ID)); !errors.Is(err, errHeldNotPosting) {
		t.Fatalf("the post of a kept write another post posted answered %v, want errHeldNotPosting", err)
	}
	for _, record := range logs.Records() {
		if record.Level >= slog.LevelError {
			t.Fatalf("the post of a kept write another post posted logged an error: %s", record.Message)
		}
	}
	if slices.Contains(labels(), "kept") {
		t.Fatalf("the kept write ran again, though its row was posted: labels %v", labels())
	}
	if _, err := rig.database.Pool.Exec(ctx, `
		update held_texts set state = 'posting', posted_at = null where id = $1`, kept.ID); err != nil {
		t.Fatal(err)
	}
	if err := rig.served.runHeldPost(rig.served.postContext(), rig.heldRow(kept.ID)); err != nil {
		t.Fatalf("the post of the kept write: %v", err)
	}
	if !slices.Contains(labels(), "kept") {
		t.Fatalf("the kept write did not run: labels %v", labels())
	}
	if row := rig.heldRow(kept.ID); row.state != heldPosted {
		t.Fatalf("the kept write's row is %s after its post, want posted", row.state)
	}
}

// A session's upload to a held issue under an Idempotency-Key is kept beside the issue inside the
// keyed layer, as every other kept write is: sent again under the same key, it is answered the
// same 202, and one write is kept.
func TestAKeptUploadSentAgainUnderItsKeyIsKeptOnce(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Issues: ReviewHold, WriterWindow: time.Hour}, nil)
	holdFixture(t, rig.database, "RVW-900", "issue", "RVW")
	data, err := json.Marshal(map[string]any{
		"name": "notes.md", "content": "The node answers on its port now.\n",
		"actor": map[string]any{"kind": "session", "id": heldWriter},
	})
	if err != nil {
		t.Fatal(err)
	}
	upload := func() heldDeferred {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/api/v1/issues/RVW-900/artifacts", bytes.NewReader(data))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer agent-token")
		request.Header.Set(contracts.AcceptsKeptWriteHeader, "1")
		request.Header.Set(contracts.IdempotencyKeyHeader, "kept-upload")
		response := httptest.NewRecorder()
		rig.handler.ServeHTTP(response, request)
		answer := decodeBody[heldDeferred](t, response)
		if response.Code != http.StatusAccepted || answer.Held.WaitsOn != "RVW-900" {
			t.Fatalf("the upload to the held issue: %d %s, want 202 waiting on RVW-900", response.Code, response.Body.String())
		}
		return answer
	}
	if first, again := upload(), upload(); again.ID != first.ID {
		t.Fatalf("the upload sent again under its key is kept as %s, the first as %s: want the first's answer", again.ID, first.ID)
	}
	if kept := countRows(t, rig.database, `select count(*) from held_texts where kind = 'request'`); kept != 1 {
		t.Fatalf("%d uploads kept beside the held issue, want one", kept)
	}
}

// A held issue's answer reads as the issue its post files: in triage, as every create files it,
// and under the assignee its post writes, for an issue that names its assignee and for a child of
// that held issue, whose assignee is its parent's and not the project's default.
func TestAHeldIssuesAnswerReadsTriageAndTheAssigneeItPostsUnder(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Issues: ReviewHold, WriterWindow: time.Minute}, nil)
	seedPeople(t, rig.database, "bob")
	hold := func(body map[string]any) (heldWriteAnswer, model.Issue) {
		t.Helper()
		response := rig.asWriter(http.MethodPost, "/api/v1/issues", body)
		held := rig.held(response, "the held issue "+body["title"].(string))
		answer := decodeBody[model.Issue](t, response)
		if answer.Status != model.IssueStatusTriage || deref(answer.Assignee) != "bob" {
			t.Fatalf("the held issue answered status %q assignee %q, want triage and bob", answer.Status, deref(answer.Assignee))
		}
		return held, answer
	}
	parent, parentAnswer := hold(map[string]any{"project": "RVW", "title": "Held issue for its assignee", "spec": vacuousPhrase, "assignee": "bob"})
	rig.waitHeld(parent.ID, heldAwaitingWriter)
	child, childAnswer := hold(map[string]any{"project": "RVW", "title": "Held child for its assignee", "parent": parent.ID})
	rig.postHeldAsWritten(parent.ID)
	rig.waitHeld(child.ID, heldPosted)
	for key, answer := range map[string]model.Issue{parent.ID: parentAnswer, child.ID: childAnswer} {
		posted := decodeBody[model.Issue](t, dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice"))
		if posted.Status != answer.Status || posted.Assignee == nil || *posted.Assignee != *answer.Assignee {
			t.Fatalf("%s posted with status %q assignee %v; its held answer read %q %q", key, posted.Status, posted.Assignee, answer.Status, *answer.Assignee)
		}
	}
}

// A project-scoped token's write on a held issue, ask or comment of its own project reaches the
// hold, though no row of its own table holds the id yet: it is kept beside the held text for a
// client that reads that answer, and refused 409 HELD_TEXT for one that does not. A held message
// of its own project passes the check to the route's own answer, since no write on a message's
// path is kept. Its write on a held text of another project is refused PROJECT_SCOPE, naming that
// project, as a write on any text of another project is.
func TestAProjectScopedTokenWritesBesideAHeldTextOfItsOwnProject(t *testing.T) {
	rig := newBrokerTokenRigOn(t, testServerOptions{reviewer: testReviewer(ReviewerConfig{Issues: ReviewHold})})
	caller := newBrokerCaller(t)
	const token = "a-token-writing-beside-a-held-issue"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	for _, kind := range []struct {
		kind, own, other, method, path string
		body                           map[string]any
		kept                           bool
	}{
		{"issue", brokerTestProject + "-900", "OTH-900", http.MethodPatch, "/api/v1/issues/%s", map[string]any{"title": "Retitled beside the hold"}, true},
		{"ask", uuid.NewString(), uuid.NewString(), http.MethodPatch, "/api/v1/asks/%s", map[string]any{"question": "Which port should the node use?"}, true},
		{"comment", uuid.NewString(), uuid.NewString(), http.MethodPost, "/api/v1/comments/%s/resolve", map[string]any{}, true},
		{"message", uuid.NewString(), uuid.NewString(), http.MethodPost, "/api/v1/messages/%s/deliveries", map[string]any{}, false},
	} {
		t.Run(kind.kind, func(t *testing.T) {
			holdFixture(t, rig.database, kind.own, kind.kind, brokerTestProject)
			holdFixture(t, rig.database, kind.other, kind.kind, "OTH")
			write := func(id string, readsKept bool) *httptest.ResponseRecorder {
				t.Helper()
				request := caller.presenting(t, kind.method, strings.Replace(kind.path, "%s", id, 1), token, kind.body)
				if readsKept {
					request.Header.Set(contracts.AcceptsKeptWriteHeader, "1")
				}
				return rig.serve(request)
			}
			if kind.kept {
				if kept := write(kind.own, true); kept.Code != http.StatusAccepted || decodeBody[heldDeferred](t, kept).Held.WaitsOn != kind.own {
					t.Fatalf("the scoped token's write on its own project's held %s: %d %s, want 202 waiting on %s", kind.kind, kept.Code, kept.Body.String(), kind.own)
				}
				if refused := write(kind.own, false); refused.Code != http.StatusConflict || decodeBody[heldTextError](t, refused).HeldID != kind.own {
					t.Fatalf("the same write from a client that does not read a kept write: %d %s, want 409 HELD_TEXT", refused.Code, refused.Body.String())
				}
			} else if passed := write(kind.own, true); decodeBody[refusalAnswer](t, passed).Code == codeProjectScope {
				t.Fatalf("the scoped token's write on its own project's held %s: %d %s, want it past the project check", kind.kind, passed.Code, passed.Body.String())
			}
			refused := write(kind.other, true)
			if refused.Code != http.StatusForbidden {
				t.Fatalf("the scoped token's write on another project's held %s: %d %s, want 403", kind.kind, refused.Code, refused.Body.String())
			}
			if refusal := decodeBody[refusalAnswer](t, refused); refusal.Code != codeProjectScope || !strings.Contains(refusal.Error, "OTH") {
				t.Fatalf("the refusal = %+v, want %s naming OTH", refusal, codeProjectScope)
			}
		})
	}
}

// A reply sent again on a delivery its first reply already answered writes nothing and answers
// the stored reply, 200, as it does with the reviewer off, on the reply route of a delivered
// message and of a delivered mention: its dry run passes that answer back, and no text is held.
func TestAHeldReplySentAgainOnAnAnsweredDeliveryAnswersTheStoredReply(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Comments: ReviewHold, Messages: ReviewHold, WriterWindow: time.Minute}, nil)
	for _, route := range []struct {
		pattern   string
		delivered func() string
	}{
		{"/api/v1/messages/{id}/reply", func() string { return "/api/v1/messages/" + rig.messageToWriter("Is it done?") + "/reply" }},
		{"/api/v1/comments/{id}/reply", func() string { return "/api/v1/comments/" + rig.mentionOfWriter("Is it done?") + "/reply" }},
	} {
		target := route.delivered()
		body := map[string]any{"attempt": 1, "body": "The node answers on its port now."}
		first := rig.held(rig.asWriter(http.MethodPost, target, body), "the first reply on "+route.pattern)
		rig.waitHeld(first.ID, heldPosted)
		before := countRows(t, rig.database, `select count(*) from held_texts`)
		again := rig.asWriter(http.MethodPost, target, body)
		if again.Code != http.StatusOK || strings.Contains(again.Body.String(), `"held"`) {
			t.Fatalf("the reply sent again on %s: %d %s, want 200 and the stored reply", route.pattern, again.Code, again.Body.String())
		}
		if id := decodeBody[heldWriteAnswer](t, again).ID; id != first.ID {
			t.Fatalf("the reply sent again on %s answered %q, want the stored reply %s", route.pattern, id, first.ID)
		}
		if after := countRows(t, rig.database, `select count(*) from held_texts`); after != before {
			t.Fatalf("the reply sent again on %s held %d texts, want none", route.pattern, after-before)
		}
	}
}

// Every row the writing check and the reviewer record of one write names that write, so the
// counts route counts the text posted over a finding once: a document version the check found a
// finding in and the reviewer reviewed after its save (the request's own write id), and a comment
// answering a held ask, which the check found a finding in, held beside the ask and reviewed after
// its post (the held text's id), each count once beside the held ask posted as written over its
// own findings.
func TestTheCountsCountAWritePostedOverAFindingOnce(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Asks: ReviewHold, Comments: ReviewAfter, Documents: ReviewAfter, WriterWindow: time.Minute}, nil)
	const finding = badProse + " " + vacuousPhrase
	if uploaded := rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/artifacts", map[string]any{
		"name": "rollout.md", "content": "# Rollout notes\n\n" + finding + "\n",
	}); uploaded.Code != http.StatusCreated {
		t.Fatalf("the document's upload: %d %s", uploaded.Code, uploaded.Body.String())
	}
	ask := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/asks", map[string]any{
		"question": "Which port should the node use? " + vacuousPhrase,
	}), "the held ask")
	rig.waitHeld(ask.ID, heldAwaitingWriter)
	comment := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/comments", map[string]any{
		"body": "Status\n" + finding, "ask_id": ask.ID,
	}), "the comment answering the held ask")
	rig.postHeldAsWritten(ask.ID)
	rig.waitHeld(comment.ID, heldPosted)
	pollUntil(t, 20*time.Second, "the reviews of the document and the comment after their posts", func() bool {
		return rig.outcomeCount(reviewReviewedAfter) == 2
	})
	if checked := rig.outcomeCount(writingCheckFindings); checked != 2 {
		t.Fatalf("the writing check found a finding in %d writes, want the document and the comment", checked)
	}
	now := time.Now().UTC()
	response := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/writing-check/counts?first="+
		now.AddDate(0, 0, -1).Format(time.DateOnly)+"&last="+now.Format(time.DateOnly), nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("the counts: %d %s", response.Code, response.Body.String())
	}
	overFinding := 0
	for _, row := range decodeBody[store.WritingCheckCounts](t, response).Items {
		overFinding += row.PostedOverFinding
	}
	if overFinding != 3 {
		t.Fatalf("%d texts posted over a finding, want 3: the document, the held ask and the comment, each once", overFinding)
	}
}
