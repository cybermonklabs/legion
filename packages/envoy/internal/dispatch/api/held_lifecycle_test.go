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
	"github.com/jackc/pgx/v5"

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
	return waitHeldDecided(rig.t, rig.database, rig.served, id)
}

// holdFixture holds a text of kind under id in project, written by s-writer for alice and
// awaiting its writer for an hour, with no request a post could run: a text a write may name.
func holdFixture(t *testing.T, database *store.Store, id, kind, project string) {
	t.Helper()
	// A held issue's request names its project and title, as every issue create's does: a write
	// held beside it reads them (heldBeside).
	if _, err := database.Pool.Exec(context.Background(), `
		insert into held_texts (id, kind, route, request, original, actor, writer, reader, project, state, deadline)
		values ($1, $2, 'POST /fixture',
			case when $2 = 'issue' then jsonb_build_object('project', $3::text, 'title', 'A held fixture') else '{}'::jsonb end,
			'{}', '{"kind":"session","id":"s-writer"}', 's-writer', 'alice', $3,
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

// The writer's answer to the findings frame of a held message is kept with the held text, 202,
// after the message posted as while it was held: the writer got no delivery of its own message, so
// its reply under that id answers the frame. The recipient's reply to the same posted message is a
// reply like any other, which the reply route takes.
func TestTheWritersAnswerToAPostedMessagesFindingsIsKept(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Messages: ReviewHold, WriterWindow: time.Minute}, nil)
	const recipient = "s-recipient"
	rig.listener.start(recipient)
	held := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/messages", map[string]any{
		"body": "Status\nWe spin up the node. " + vacuousPhrase, "target": "session:" + recipient, "delivery": "btw",
	}), "the agent's message to a session")
	rig.waitHeld(held.ID, heldAwaitingWriter)
	rig.postHeldAsWritten(held.ID)
	rig.waitHeld(held.ID, heldPosted)
	const answer = "The message is right for its reader as written."
	kept := rig.asWriter(http.MethodPost, "/api/v1/messages/"+held.ID+"/reply", map[string]any{"attempt": 1, "body": answer})
	if kept.Code != http.StatusAccepted {
		t.Fatalf("the writer's answer to the posted message's findings: %d %s, want 202", kept.Code, kept.Body.String())
	}
	if got := rig.heldRow(held.ID).answer; got != answer {
		t.Fatalf("the held message keeps the answer %q, want %q", got, answer)
	}
	if count := countRows(t, rig.database, `select count(*) from messages where in_reply_to::text = $1`, held.ID); count != 0 {
		t.Fatalf("the writer's answer wrote %d replies to the posted message, want none", count)
	}
	pollUntil(t, 10*time.Second, "the posted message's delivery to "+recipient, func() bool {
		return countRows(t, rig.database, `select count(*) from message_deliveries where message_id::text = $1 and session_id = $2`, held.ID, recipient) == 1
	})
	reply := rig.asSession(recipient, http.MethodPost, "/api/v1/messages/"+held.ID+"/reply", map[string]any{
		"attempt": 1, "body": "The node answers on its port now.",
	})
	if reply.Code != http.StatusCreated {
		t.Fatalf("the recipient's reply to the posted message: %d %s, want 201", reply.Code, reply.Body.String())
	}
	if got := rig.heldRow(held.ID).answer; got != answer {
		t.Fatalf("after the recipient's reply the held message keeps the answer %q, want the writer's %q", got, answer)
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
	rig.served.detached.wait()
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

// One writer's two held texts at one place post in the order written: two comments on an issue,
// two messages to a session, a reply to a mention on an issue then a comment on that issue, and
// that comment then that reply. The second's review runs at once and passes while the first waits
// on its writer, and the second still posts only once the first has, as its answer says
// (`waits_on`).
func TestOneWritersHeldTextsAtOnePlacePostInTheOrderWritten(t *testing.T) {
	const recipient = "s-recipient"
	for name, place := range map[string]struct {
		cfg           ReviewerConfig
		table         string
		first, second string
	}{
		"comments on an issue":                 {holdComments(), "comments", "comment", "comment"},
		"messages to a session":                {ReviewerConfig{Messages: ReviewHold, WriterWindow: time.Minute}, "messages", "message", "message"},
		"a reply to a mention, then a comment": {holdComments(), "comments", "reply", "comment"},
		"a comment, then a reply to a mention": {holdComments(), "comments", "comment", "reply"},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newReviewRig(t, place.cfg, nil)
			rig.listener.start(recipient)
			writes := map[string]func(body string) *httptest.ResponseRecorder{
				"comment": func(body string) *httptest.ResponseRecorder {
					return rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/comments", map[string]any{"body": body})
				},
				"message": func(body string) *httptest.ResponseRecorder {
					return rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/messages", map[string]any{
						"body": body, "target": "session:" + recipient, "delivery": "btw",
					})
				},
				// The writer's reply to alice's mention of it on the rig's issue, a comment there.
				"reply": func(body string) *httptest.ResponseRecorder {
					return rig.asWriter(http.MethodPost, "/api/v1/comments/"+rig.mentionOfWriter("Is it done?")+"/reply", map[string]any{
						"attempt": 1, "body": body,
					})
				},
			}
			first := rig.held(writes[place.first]("Status\nWe spin up the node. "+vacuousPhrase), "the first, a "+place.first)
			rig.waitHeld(first.ID, heldAwaitingWriter)
			second := rig.held(writes[place.second]("The node answers on its port now."), "the second, a "+place.second)
			pollUntil(t, 20*time.Second, "the second's review to end", func() bool {
				return rig.heldRow(second.ID).outcome != ""
			})
			rig.served.detached.wait()
			rig.served.postReady()
			if row := rig.heldRow(second.ID); row.state != heldPosting || row.outcome != reviewPassed {
				t.Fatalf("the second is %s %s (%s) while the first waits on its writer, want it passed and waiting to post", row.state, row.outcome, row.detail)
			}
			if second.Held.WaitsOn != first.ID {
				t.Fatalf("the second's answer waits on %q, want the first, %s", second.Held.WaitsOn, first.ID)
			}
			rig.postHeldAsWritten(first.ID)
			rig.waitHeld(second.ID, heldPosted)
			rows, err := rig.database.Pool.Query(context.Background(),
				`select id::text from `+place.table+` where id::text = any($1) order by created_at`, []string{second.ID, first.ID})
			if err != nil {
				t.Fatal(err)
			}
			order, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				t.Fatal(err)
			}
			if want := []string{first.ID, second.ID}; !slices.Equal(order, want) {
				t.Fatalf("the %s posted in the order %v, want the order written %v", place.table, order, want)
			}
		})
	}
}

// One writer's two held new issues in one project, neither under a held parent, post in the order
// written, the order their keys were reserved in: the second's review runs at once and passes
// while the first waits on its writer, and the second posts only once the first has, as its answer
// says (`waits_on`).
func TestOneWritersHeldIssuesInOneProjectPostInTheOrderWritten(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Issues: ReviewHold, WriterWindow: time.Minute}, nil)
	rig.settleTermStats("RVW")
	first := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": "Astrolabe gnomon reseat offsets",
		"spec": "The astrolabe reads a constant offset after each gnomon reseat.\n\n" + vacuousPhrase,
	}), "the first held issue")
	rig.waitHeld(first.ID, heldAwaitingWriter)
	second := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": "Bellows valve chatter at idle", "spec": "The bellows valve chatters while the pump idles.\n",
	}), "the second held issue")
	if second.Held.WaitsOn != first.ID {
		t.Fatalf("the second issue's answer waits on %q, want the first, %s", second.Held.WaitsOn, first.ID)
	}
	pollUntil(t, 20*time.Second, "the second's review to end", func() bool {
		return rig.heldRow(second.ID).outcome != ""
	})
	rig.served.detached.wait()
	rig.served.postReady()
	if row := rig.heldRow(second.ID); row.state != heldPosting || row.outcome != reviewPassed {
		t.Fatalf("the second issue is %s %s (%s) while the first waits on its writer, want it passed and waiting to post", row.state, row.outcome, row.detail)
	}
	rig.postHeldAsWritten(first.ID)
	secondPosted := rig.waitHeld(second.ID, heldPosted)
	if firstPosted := rig.heldRow(first.ID); firstPosted.postedAt == nil || secondPosted.postedAt.Before(*firstPosted.postedAt) {
		t.Fatalf("the second issue posted at %v, the first at %v, want the order written", secondPosted.postedAt, firstPosted.postedAt)
	}
}

// A held text that waits on its writer's earlier held text at its place only for its turn posts
// when that text's post is refused: the writer's held message on an issue waits on its held ask
// there, the issue closes, the ask's post is refused, and the message, which a closed issue still
// takes, posts, and its writer is told only of the ask.
func TestATextWaitingForItsTurnPostsWhenTheTextBeforeItFails(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Asks: ReviewHold, Messages: ReviewHold, WriterWindow: time.Minute}, nil)
	ask := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/asks", map[string]any{
		"question": "Ship the reviewer?\n" + vacuousPhrase, "options": writtenOptions,
	}), "the held ask")
	rig.waitHeld(ask.ID, heldAwaitingWriter)
	message := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/messages", map[string]any{
		"body": "The node answers on its port now.",
	}), "the held message")
	if message.Held.WaitsOn != ask.ID {
		t.Fatalf("the message waits on %q, want the ask, %s", message.Held.WaitsOn, ask.ID)
	}
	rig.closeRigIssue()
	rig.postHeldAsWritten(ask.ID)
	if row := rig.waitDecided(ask.ID); row.state != heldFailed || !strings.Contains(row.detail, "ISSUE_CLOSED") {
		t.Fatalf("the ask is %s (%s) on its closed issue, want its post refused", row.state, row.detail)
	}
	if row := rig.waitDecided(message.ID); row.state != heldPosted {
		t.Fatalf("the message is %s (%s) once the ask before it failed, want it posted", row.state, row.detail)
	}
	if posted := countRows(t, rig.database, `select count(*) from messages where id::text = $1`, message.ID); posted != 1 {
		t.Fatalf("%d messages under the posted message's id, want one", posted)
	}
	rig.served.detached.wait()
	if bodies := rig.framesAbout(message.ID, "did not post"); len(bodies) != 0 {
		t.Fatalf("the writer was told the posted message did not post: %q", bodies)
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
	rig.served.detached.wait()
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

// heldIssue holds a new issue of project titled title, which the reviewer flags, as the writer,
// and waits for it to wait on its writer.
func (rig *reviewRig) heldIssue(project, title string) heldWriteAnswer {
	rig.t.Helper()
	held := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": project, "title": title, "spec": vacuousPhrase,
	}), "the held issue "+title)
	rig.waitHeld(held.ID, heldAwaitingWriter)
	return held
}

// A new issue whose blocked_by names a held issue is held beside it, as a child is beside a held
// parent, and posts after it with the link, as it posts with the reviewer off. One with a held
// parent and a held blocker outside that parent's chain is held beside its parent, its dry run
// leaving the held blocker out, and its post waits for the blocker too (heldNamedOpenSQL): it is
// decided once its parent posts, and posts only once its blocker has. The blocker is of another
// project, so neither held issue waits on the other for its turn at its project (heldBeforeOf).
func TestANewIssueBlockedByAHeldIssuePostsAfterIt(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Issues: ReviewHold, WriterWindow: time.Minute}, nil)
	createTestProject(t, rig.handler, "OTH")
	blocker := rig.heldIssue("OTH", "Held blocker")
	parent := rig.heldIssue("RVW", "Held parent")
	blocked := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": "Blocked by a held issue", "blocked_by": []string{blocker.ID},
	}), "the issue blocked by a held issue")
	if blocked.Held.WaitsOn != blocker.ID {
		t.Fatalf("the blocked issue waits on %q, want its held blocker %s", blocked.Held.WaitsOn, blocker.ID)
	}
	child := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": "Child blocked beside its held parent", "parent": parent.ID, "blocked_by": []string{blocker.ID},
	}), "the child blocked by a held issue outside its parent's chain")
	if child.Held.WaitsOn != parent.ID {
		t.Fatalf("the child waits on %q, want its held parent %s", child.Held.WaitsOn, parent.ID)
	}
	if count := countRows(t, rig.database, `select count(*) from issues where key = any($1)`, []string{blocked.ID, child.ID}); count != 0 {
		t.Fatalf("%d of the issues waiting on held ones exist before those post", count)
	}
	rig.postHeldAsWritten(parent.ID)
	rig.waitHeld(child.ID, heldPosting)
	rig.served.detached.wait()
	if row := rig.heldRow(child.ID); row.state != heldPosting {
		t.Fatalf("the child is %s (%s) while its blocker is held, want it waiting to post", row.state, row.detail)
	}
	rig.postHeldAsWritten(blocker.ID)
	blockerPosted := rig.waitHeld(blocker.ID, heldPosted)
	for _, waiting := range []heldWriteAnswer{blocked, child} {
		posted := rig.waitHeld(waiting.ID, heldPosted)
		if posted.postedAt.Before(*blockerPosted.postedAt) {
			t.Fatalf("%s posted at %v, before its blocker at %v", waiting.ID, posted.postedAt, blockerPosted.postedAt)
		}
		if blockers := issueBlockers(t, rig.database, waiting.ID); !slices.Equal(blockers, []string{blocker.ID}) {
			t.Fatalf("%s is blocked by %v, want %s", waiting.ID, blockers, blocker.ID)
		}
	}
	var childParent string
	if err := rig.database.Pool.QueryRow(context.Background(), `select parent_key from issues where key = $1`, child.ID).Scan(&childParent); err != nil || childParent != parent.ID {
		t.Fatalf("the child's parent is %q (%v), want %s", childParent, err, parent.ID)
	}
}

// An update whose blocked_by, parent or rank names a held issue is kept beside it, from a client
// that reads a kept write's answer, and runs once it posts, writing the link, the parent and the
// rank as it does with the reviewer off. An update of a held issue that names another held issue
// as a blocker is kept beside the issue its path names, and runs only once its blocker has posted
// too (heldNamedOpenSQL). The blocker is of another project, so neither held issue waits on the
// other for its turn at its project (heldBeforeOf).
func TestAnUpdateAddingAHeldBlockerIsKeptAndPostsAfterIt(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Issues: ReviewHold, WriterWindow: time.Minute}, nil)
	createTestProject(t, rig.handler, "OTH")
	blocker := rig.heldIssue("OTH", "Held blocker")
	updated := rig.heldIssue("RVW", "Held issue updated beside its blocker")
	reparented := createOutcomeIssue(t, rig.handler, "RVW", "Reparented under a held issue")
	ranked := createOutcomeIssue(t, rig.handler, "RVW", "Ranked after a held issue")
	keep := func(issue string, body map[string]any, waitsOn string) heldWriteAnswer {
		t.Helper()
		kept := rig.held(rig.asKeptWriter(http.MethodPatch, "/api/v1/issues/"+issue, body), "the update of "+issue)
		if kept.Held.WaitsOn != waitsOn {
			t.Fatalf("the update of %s waits on %q, want %s", issue, kept.Held.WaitsOn, waitsOn)
		}
		return kept
	}
	// The writes kept beside one text post in the order they came, so the update that waits on a
	// second held text comes last.
	posted := keep(rig.issue, map[string]any{"blocked_by": []string{blocker.ID}}, blocker.ID)
	parent := keep(reparented, map[string]any{"parent": updated.ID}, updated.ID)
	rank := keep(ranked, map[string]any{"rank": map[string]any{"after": updated.ID}}, updated.ID)
	held := keep(updated.ID, map[string]any{"blocked_by": []string{blocker.ID}}, updated.ID)
	if blockers := issueBlockers(t, rig.database, rig.issue); len(blockers) != 0 {
		t.Fatalf("%s is blocked by %v before its blocker posts", rig.issue, blockers)
	}
	requirePosted := func(id, issue string) {
		t.Helper()
		if row := rig.waitHeld(id, heldPosted); row.outcome != heldNotReviewed {
			t.Fatalf("the update of %s posted %s (%s)", issue, row.outcome, row.detail)
		}
	}
	rig.postHeldAsWritten(updated.ID)
	rig.waitHeld(updated.ID, heldPosted)
	requirePosted(parent.ID, reparented)
	requirePosted(rank.ID, ranked)
	rig.served.detached.wait()
	if row := rig.heldRow(held.ID); row.state != heldPosting {
		t.Fatalf("the update of %s is %s (%s) while its blocker is held, want it waiting to post", updated.ID, row.state, row.detail)
	}
	rig.postHeldAsWritten(blocker.ID)
	requirePosted(posted.ID, rig.issue)
	requirePosted(held.ID, updated.ID)
	for _, issue := range []string{rig.issue, updated.ID} {
		if blockers := issueBlockers(t, rig.database, issue); !slices.Equal(blockers, []string{blocker.ID}) {
			t.Fatalf("%s is blocked by %v, want %s", issue, blockers, blocker.ID)
		}
	}
	var parentKey string
	if err := rig.database.Pool.QueryRow(context.Background(), `select coalesce(parent_key, '') from issues where key = $1`, reparented).Scan(&parentKey); err != nil || parentKey != updated.ID {
		t.Fatalf("%s's parent is %q (%v), want %s", reparented, parentKey, err, updated.ID)
	}
	var after bool
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select r.rank > b.rank from issues r, issues b where r.key = $1 and b.key = $2`, ranked, updated.ID).Scan(&after); err != nil || !after {
		t.Fatalf("%s ranks after %s: %v (%v), want true", ranked, updated.ID, after, err)
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

// A held reply's answer states what its post writes, read off the dry run of its route: a comment
// reply in the thread of a project document's comment names that document, as the posted reply
// does, with the comment it replies to, its ask and turn, the block it replies to and the text it
// answers; a message reply names its issue, its target, the message and the block it replies to;
// and a follow-up on the attempt that reply answered names the reply it is filed under.
func TestAHeldRepliesAnswerStatesWhereItsPostWritesIt(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Comments: ReviewHold, Messages: ReviewHold, WriterWindow: time.Minute}, nil)
	uploaded := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/projects/RVW/artifacts", map[string]any{"name": "guide", "content": "Seed paragraph here.\n"}, "alice")
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("alice's project document: %d %s", uploaded.Code, uploaded.Body.String())
	}
	guide := decodeBody[artifactUploadResponse](t, uploaded).Artifact.ID
	asked := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/artifacts/"+guide+"/comments", map[string]any{
		"body": "Is the guide right?", "mentions": []map[string]any{{"target": "session:" + heldWriter}},
	}, "alice")
	if asked.Code != http.StatusCreated {
		t.Fatalf("alice's comment on the guide: %d %s", asked.Code, asked.Body.String())
	}
	mention := decodeBody[model.Comment](t, asked).ID
	settledCommentDeliveries(t, rig.database, mention)
	response := rig.asWriter(http.MethodPost, "/api/v1/comments/"+mention+"/reply", map[string]any{"body": "Status\nWe spin up the node. " + vacuousPhrase, "attempt": 1})
	id := rig.held(response, "the held comment reply").ID
	answer := decodeBody[model.Comment](t, response)
	rig.postedAsWritten(id)
	posted := postedItem[model.Comment](t, rig.handler, "/api/v1/comments/"+id, id)
	comment := func(c model.Comment) string {
		return asJSON(t, map[string]any{
			"issue_key": c.IssueKey, "artifact_id": c.ArtifactID, "reply_to": c.ReplyTo, "ask_id": c.AskID, "turn": c.Turn,
			"answers": c.Answers, "reply_block": c.ReplyBlock,
		})
	}
	if answer.ArtifactID == nil || comment(answer) != comment(posted) {
		t.Errorf("the held comment reply states %s; it posted %s", comment(answer), comment(posted))
	}
	sent := rig.messageToWriter("Is it done?")
	response = rig.asWriter(http.MethodPost, "/api/v1/messages/"+sent+"/reply", map[string]any{"body": "Status\nWe spin up the node. " + vacuousPhrase, "attempt": 1})
	id = rig.held(response, "the held message reply").ID
	reply := decodeBody[model.Message](t, response)
	rig.postedAsWritten(id)
	postedReply := postedItem[model.Message](t, rig.handler, "/api/v1/messages/"+id, id)
	message := func(m model.Message) string {
		return asJSON(t, map[string]any{"issue_key": m.IssueKey, "target": m.Target, "in_reply_to": m.InReplyTo, "reply_block": m.ReplyBlock})
	}
	if message(reply) != message(postedReply) {
		t.Errorf("the held message reply states %s; it posted %s", message(reply), message(postedReply))
	}
	response = rig.asWriter(http.MethodPost, "/api/v1/messages/"+sent+"/reply?follow_up=true", map[string]any{"body": "Status\nWe spin up the second node. " + vacuousPhrase, "attempt": 1})
	id = rig.held(response, "the held follow-up").ID
	followUp := decodeBody[model.Message](t, response)
	rig.postedAsWritten(id)
	postedFollowUp := postedItem[model.Message](t, rig.handler, "/api/v1/messages/"+id, id)
	if deref(followUp.InReplyTo) != postedReply.ID || message(followUp) != message(postedFollowUp) {
		t.Errorf("the held follow-up states %s; it posted %s, under the reply %s", message(followUp), message(postedFollowUp), postedReply.ID)
	}
}

// A held issue's answer states what its post writes beside the row's other columns, each as a read
// of the posted issue answers it: the repository it names, the priorities it advances and the
// components it chose. An issue stating its own, and one under a parent whose repository, priority
// and components it inherits, which its dry run read back, answer as they post, field for field. A
// child of a held issue runs its dry run with no parent, so the issue that dry run files inherits
// nothing of the held issue, and its answer keeps none of it (issueCreate.beside): it answers its
// own, no repository, the priority it names and the components it chose, which is what the posted
// child holds of its own.
func TestAHeldIssuesAnswerStatesTheRepoAdvancesAndComponentsItPosts(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Issues: ReviewHold, WriterWindow: time.Minute}, nil)
	for _, id := range []string{"ship-the-daemon", "cut-the-queue"} {
		createTestPriority(t, rig.handler, map[string]any{"id": id, "title": id, "owner": "alice"}, "alice")
	}
	for _, statement := range []string{
		`insert into architecture_sources (project_key, id, repo, branch, installation_id, created_by)
			values ('RVW', 1, 'acme/arch', 'main', 1, '{"kind":"system","id":"test"}')`,
		`insert into architecture_snapshots (id, project_key, commit, files) values (903, 'RVW', 'c1', '{}')`,
		`insert into components (project_key, id, title, prose, parent, snapshot_id, source_id) values
			('RVW', 'platform', 'The platform', '', null, 903, 1), ('RVW', 'api', 'Public API', '', 'platform', 903, 1)`,
	} {
		if _, err := rig.database.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("seed RVW's component model: %v", err)
		}
	}
	recorded := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": "The rollout of record", "force": true, "repo": "acme/gadgets",
		"advances": []string{"cut-the-queue"}, "components": map[string]any{"mode": "explicit", "ids": []string{"platform"}},
	}, "alice")
	if recorded.Code != http.StatusCreated {
		t.Fatalf("alice's issue of record: %d %s", recorded.Code, recorded.Body.String())
	}
	parent := decodeBody[model.Issue](t, recorded).Key
	// writes is what the test compares of an issue, as its JSON reads.
	writes := func(issue model.Issue) string {
		return asJSON(t, map[string]any{
			"repo": issue.Repo, "resolved_repo": issue.ResolvedRepo, "advances": issue.Advances, "components": issue.Components,
		})
	}
	hold := func(body map[string]any) (string, model.Issue) {
		t.Helper()
		response := rig.asWriter(http.MethodPost, "/api/v1/issues", body)
		return rig.held(response, "the held issue "+body["title"].(string)).ID, decodeBody[model.Issue](t, response)
	}
	read := func(key string) model.Issue {
		t.Helper()
		return decodeBody[model.Issue](t, dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice"))
	}
	own, ownAnswer := hold(map[string]any{
		"project": "RVW", "title": "Held with its own relations", "spec": vacuousPhrase, "repo": "acme/widgets",
		"advances": []string{"ship-the-daemon"}, "components": map[string]any{"mode": "explicit", "ids": []string{"api"}},
	})
	under, underAnswer := hold(map[string]any{"project": "RVW", "title": "Held under the rollout", "spec": vacuousPhrase, "parent": parent})
	rig.waitHeld(own, heldAwaitingWriter)
	rig.waitHeld(under, heldAwaitingWriter)
	child, childAnswer := hold(map[string]any{
		"project": "RVW", "title": "Held child of a held issue", "parent": own,
		"advances": []string{"cut-the-queue"}, "components": map[string]any{"mode": "none", "reason": "It changes no component."},
	})
	rig.postHeldAsWritten(own)
	rig.postHeldAsWritten(under)
	rig.waitHeld(child, heldPosted)
	for key, answer := range map[string]model.Issue{own: ownAnswer, under: underAnswer} {
		if posted := read(key); writes(answer) != writes(posted) {
			t.Errorf("%s's held answer states %s; it posted %s", key, writes(answer), writes(posted))
		}
	}
	posted := read(child)
	if childAnswer.Repo != nil || !slices.Equal(ownAdvances(childAnswer.Advances), ownAdvances(posted.Advances)) ||
		writes(model.Issue{Components: childAnswer.Components}) != writes(model.Issue{Components: posted.Components}) {
		t.Errorf("%s's held answer states %s; it posted %s, want the same repository, priorities and components of its own", child, writes(childAnswer), writes(posted))
	}
}

// postedItem is the object of id in alice's read of target, wherever the read nests it, decoded
// as T: a single read wraps its item beside what the read carries with it.
func postedItem[T any](t *testing.T, handler http.Handler, target, id string) T {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("alice's read of %s: %d %s", target, response.Code, response.Body.String())
	}
	var tree any
	if err := json.Unmarshal(response.Body.Bytes(), &tree); err != nil {
		t.Fatal(err)
	}
	var found any
	var walk func(any)
	walk = func(node any) {
		switch value := node.(type) {
		case map[string]any:
			if value["id"] == id && found == nil {
				found = value
				return
			}
			for _, child := range value {
				walk(child)
			}
		case []any:
			for _, child := range value {
				walk(child)
			}
		}
	}
	walk(tree)
	if found == nil {
		t.Fatalf("alice's read of %s holds no %s: %s", target, id, response.Body.String())
	}
	var item T
	if err := json.Unmarshal([]byte(asJSON(t, found)), &item); err != nil {
		t.Fatalf("decode %s in %s: %v", id, target, err)
	}
	return item
}

// specOf is the id of issue's own document.
func specOf(t *testing.T, database *store.Store, issue string) string {
	t.Helper()
	var id string
	if err := database.Pool.QueryRow(context.Background(), `select id::text from artifacts where issue_key = $1 and is_primary`, issue).Scan(&id); err != nil {
		t.Fatalf("read %s's document: %v", issue, err)
	}
	return id
}

// postedAsWritten waits for the held text id to await its writer, posts it as written and waits
// for it to post.
func (rig *reviewRig) postedAsWritten(id string) {
	rig.t.Helper()
	rig.waitHeld(id, heldAwaitingWriter)
	rig.postHeldAsWritten(id)
	rig.waitHeld(id, heldPosted)
}

// A held ask's answer states the anchor its post writes, as its dry run resolved it: the document,
// the block and the quote the posted ask holds, and no mark id, which only the post's own mark
// makes.
func TestAHeldAsksAnswerStatesTheAnchorItPosts(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Asks: ReviewHold, WriterWindow: time.Minute}, nil)
	response := rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/asks", map[string]any{
		"question": "Ship the reviewer?\n" + vacuousPhrase,
		"options":  []map[string]string{{"label": "Ship it", "description": "Release it today."}, {"label": "Do not file", "description": "Wait."}},
		"anchor":   map[string]any{"artifact": specOf(t, rig.database, rig.issue), "quote": "Seed paragraph"},
	})
	id := rig.held(response, "the held ask").ID
	answer := decodeBody[model.Ask](t, response)
	rig.postedAsWritten(id)
	posted := postedItem[model.Ask](t, rig.handler, "/api/v1/asks/"+id, id)
	if answer.Anchor == nil || posted.Anchor == nil || answer.Anchor.MarkID != "" || posted.Anchor.MarkID == "" {
		t.Fatalf("the held ask's anchor %s, the posted one %s: want both, the held one with no mark id", asJSON(t, answer.Anchor), asJSON(t, posted.Anchor))
	}
	held, wrote := *answer.Anchor, *posted.Anchor
	held.MarkID, wrote.MarkID = "", ""
	if asJSON(t, held) != asJSON(t, wrote) {
		t.Errorf("the held ask's anchor is %s; it posted %s", asJSON(t, held), asJSON(t, wrote))
	}
}

// A held comment's answer states what its post writes beyond its body: a comment on a quote its
// anchor, less the mark id only the post makes, and a reply on an ask the turn it hands on, each as
// the posted comment holds it, with its mentions, the block it replies to and the agent text it
// answers.
func TestAHeldCommentsAnswerStatesTheAnchorAndTurnItPosts(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Comments: ReviewHold, WriterWindow: time.Minute}, nil)
	asked := rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/asks", map[string]any{"question": "Which port should the node use?"})
	if asked.Code != http.StatusCreated {
		t.Fatalf("the writer's ask: %d %s", asked.Code, asked.Body.String())
	}
	ask := decodeBody[model.Ask](t, asked).ID
	comments := "/api/v1/issues/" + rig.issue + "/comments"
	for name, body := range map[string]map[string]any{
		"on a quote": {"body": "Status\nWe spin up the node. " + vacuousPhrase, "anchor": map[string]any{"artifact": specOf(t, rig.database, rig.issue), "quote": "Seed paragraph"}},
		"on an ask":  {"body": "Status\nThe node answers on 8080. " + vacuousPhrase, "ask_id": ask},
	} {
		response := rig.asWriter(http.MethodPost, comments, body)
		id := rig.held(response, "the held comment "+name).ID
		answer := decodeBody[model.Comment](t, response)
		rig.postedAsWritten(id)
		posted := postedItem[model.Comment](t, rig.handler, "/api/v1/comments/"+id, id)
		if (answer.Anchor == nil) != (body["anchor"] == nil) || (answer.Turn == nil) != (body["ask_id"] == nil) || (answer.Anchor != nil && answer.Anchor.MarkID != "") {
			t.Errorf("%s: the held comment states anchor %s turn %v, want the anchor with no mark id or the turn its request makes", name, asJSON(t, answer.Anchor), answer.Turn)
		}
		if posted.Anchor != nil {
			posted.Anchor.MarkID = ""
		}
		writes := func(comment model.Comment) string {
			return asJSON(t, map[string]any{
				"anchor": comment.Anchor, "turn": comment.Turn, "answers": comment.Answers, "reply_block": comment.ReplyBlock,
				"mentions": comment.Mentions, "reply_to": comment.ReplyTo, "ask_id": comment.AskID,
			})
		}
		if writes(answer) != writes(posted) {
			t.Errorf("%s: the held comment states %s; it posted %s", name, writes(answer), writes(posted))
		}
	}
}

// A held ask's answer states who the ask waits on and the document its anchor quotes, as the
// write's own answer does (writeAsk: waiting_on_person, anchor_artifact), and a held reply on an
// ask states whom that ask waits on once the reply posts (ask_waiting_on, as comment_create.go's
// answer gives it): the dispatch command prints each, so a held write that leaves them out tells
// its writer less than the same write with the reviewer off.
func TestAHeldAskAndAHeldReplyAnswerWhomTheirPostLeavesTheAskWaitingOn(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Asks: ReviewHold, WriterWindow: time.Minute}, nil)
	response := rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/asks", map[string]any{
		"question": "Ship the reviewer?\n" + vacuousPhrase,
		"options":  []map[string]string{{"label": "Ship it", "description": "Release it today."}, {"label": "Do not file", "description": "Wait."}},
		"anchor":   map[string]any{"artifact": specOf(t, rig.database, rig.issue), "quote": "Seed paragraph"},
	})
	id := rig.held(response, "the held ask").ID
	answer := decodeBody[model.Ask](t, response)
	rig.postedAsWritten(id)
	posted := postedItem[model.Ask](t, rig.handler, "/api/v1/asks/"+id, id)
	if posted.WaitingOnPerson == "" || answer.WaitingOnPerson != posted.WaitingOnPerson {
		t.Errorf("the held ask states waiting_on_person %q; it posted waiting on %q", answer.WaitingOnPerson, posted.WaitingOnPerson)
	}
	if posted.AnchorArtifact == nil || asJSON(t, answer.AnchorArtifact) != asJSON(t, posted.AnchorArtifact) {
		t.Errorf("the held ask states anchor_artifact %s; it posted %s", asJSON(t, answer.AnchorArtifact), asJSON(t, posted.AnchorArtifact))
	}

	replies := newReviewRig(t, ReviewerConfig{Comments: ReviewHold, WriterWindow: time.Minute}, nil)
	asked := replies.asWriter(http.MethodPost, "/api/v1/issues/"+replies.issue+"/asks", map[string]any{"question": "Which port should the node use?"})
	if asked.Code != http.StatusCreated {
		t.Fatalf("the writer's ask: %d %s", asked.Code, asked.Body.String())
	}
	reply := replies.asWriter(http.MethodPost, "/api/v1/issues/"+replies.issue+"/comments", map[string]any{
		"body": "Status\nThe node answers on 8080. " + vacuousPhrase, "ask_id": decodeBody[model.Ask](t, asked).ID,
	})
	replies.held(reply, "the held reply on the ask")
	var stated struct {
		AskWaitingOn string `json:"ask_waiting_on"`
	}
	if err := json.Unmarshal(reply.Body.Bytes(), &stated); err != nil {
		t.Fatal(err)
	}
	if stated.AskWaitingOn == "" {
		t.Errorf("the held reply on an open ask states no ask_waiting_on: %s", reply.Body.String())
	}
}

// A held comment that replies inside a thread answers the thread its post writes it in: the post
// climbs reply_to to the thread's head (threadHeadOf) and keeps the comment the request named as
// answers. A reply to a reply answers reply_to the thread's root comment, and a reply to a comment
// in an ask's thread answers no reply_to and ask_id that ask, each as the posted comment holds it.
func TestAHeldCommentReplyingInsideAThreadAnswersTheThreadItPostsIn(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Comments: ReviewHold, WriterWindow: time.Minute}, nil)
	comments := "/api/v1/issues/" + rig.issue + "/comments"
	alice := func(body map[string]any) string {
		t.Helper()
		response := dispatchRequest(t, rig.handler, http.MethodPost, comments, body, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("alice's comment %v: %d %s", body, response.Code, response.Body.String())
		}
		return decodeBody[model.Comment](t, response).ID
	}
	asked := rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/asks", map[string]any{"question": "Which port should the node use?"})
	if asked.Code != http.StatusCreated {
		t.Fatalf("the writer's ask: %d %s", asked.Code, asked.Body.String())
	}
	ask := decodeBody[model.Ask](t, asked).ID
	root := alice(map[string]any{"body": "Is the node up?"})
	reply := alice(map[string]any{"body": "And on which port?", "reply_to": root})
	onAsk := alice(map[string]any{"body": "Port 8080, I think.", "ask_id": ask})
	for _, test := range []struct {
		name, repliesTo string
		replyTo, askID  *string
	}{
		{name: "to a reply", repliesTo: reply, replyTo: &root},
		{name: "to a comment in an ask's thread", repliesTo: onAsk, askID: &ask},
	} {
		response := rig.asWriter(http.MethodPost, comments, map[string]any{"body": "Status\nThe node answers on 8080. " + vacuousPhrase, "reply_to": test.repliesTo})
		id := rig.held(response, "the held reply "+test.name).ID
		answer := decodeBody[model.Comment](t, response)
		rig.postedAsWritten(id)
		posted := postedItem[model.Comment](t, rig.handler, "/api/v1/comments/"+id, id)
		thread := func(comment model.Comment) string {
			return asJSON(t, map[string]any{"reply_to": comment.ReplyTo, "ask_id": comment.AskID, "answers": comment.Answers, "turn": comment.Turn})
		}
		want := asJSON(t, map[string]any{"reply_to": test.replyTo, "ask_id": test.askID, "answers": &test.repliesTo, "turn": posted.Turn})
		if thread(answer) != want || thread(posted) != want {
			t.Errorf("%s: the held reply states %s and posted %s, want %s", test.name, thread(answer), thread(posted), want)
		}
	}
}

// A held message's answer states the block it replies to, as the posted message holds it.
func TestAHeldMessagesAnswerStatesTheBlockItRepliesTo(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Messages: ReviewHold, WriterWindow: time.Minute}, nil)
	messages := "/api/v1/issues/" + rig.issue + "/messages"
	root := dispatchRequest(t, rig.handler, http.MethodPost, messages, map[string]any{"body": "One.\n\nTwo."}, "alice")
	if root.Code != http.StatusCreated {
		t.Fatalf("alice's message: %d %s", root.Code, root.Body.String())
	}
	response := rig.asWriter(http.MethodPost, messages, map[string]any{
		"body": "Status\nWe spin up the node. " + vacuousPhrase, "in_reply_to": decodeBody[model.Message](t, root).ID, "in_reply_to_block": "b2",
	})
	id := rig.held(response, "the held message").ID
	answer := decodeBody[model.Message](t, response)
	rig.postedAsWritten(id)
	posted := postedItem[model.Message](t, rig.handler, "/api/v1/messages/"+id, id)
	if answer.ReplyBlock == nil || asJSON(t, answer.ReplyBlock) != asJSON(t, posted.ReplyBlock) {
		t.Errorf("the held message replies to %s; it posted replying to %s", asJSON(t, answer.ReplyBlock), asJSON(t, posted.ReplyBlock))
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

// postHeldIssueFixture posts the held issue id that holdFixture held, as a clean review decides
// one: its post files the issue under the number its key names, and each write kept or held
// beside it posts after it.
func postHeldIssueFixture(t *testing.T, database *store.Store, served *server, id string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		update held_texts set request = request || '{"labels": []}'::jsonb, state = 'posting', outcome = 'passed',
			deadline = null
		where id = $1`, id); err != nil {
		t.Fatalf("decide the held issue %s: %v", id, err)
	}
	served.postReady()
}

// waitHeldDecided waits for the held text id of served to post or fail, and answers it.
func waitHeldDecided(t *testing.T, database *store.Store, served *server, id string) heldRow {
	t.Helper()
	var row heldRow
	pollUntil(t, 20*time.Second, "held text "+id+" posted or failed", func() bool {
		var err error
		if row, err = served.loadHeld(context.Background(), database.Pool, id, false); err != nil {
			t.Fatalf("read held text %s: %v", id, err)
		}
		return row.state == heldPosted || row.state == heldFailed
	})
	return row
}

// issueBlockers is the blocked_by of the issue key, as its links hold them, in key order.
func issueBlockers(t *testing.T, database *store.Store, key string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select target_key from issue_links where issue_key = $1 and kind = 'blocked_by' order by target_key`, key)
	if err != nil {
		t.Fatalf("read the blockers of %s: %v", key, err)
	}
	blockers, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the blockers of %s: %v", key, err)
	}
	return blockers
}

// A write kept beside a held text runs at its post as the caller its hold stored, held to the one
// project that caller's credential may write, as it is held to it with the reviewer off: a
// project-scoped token's update of its own project's held issue naming a posted blocker of another
// project is kept, since its hold judges only a held blocker (refuseHeldForeignBlockers), and its
// post refuses it, 403 PROJECT_SCOPE naming both projects, as the same update of a posted issue is
// refused at once. The kept row fails with that refusal and no link is written. The same token's
// kept update naming a blocker of its own project posts.
func TestAKeptWritePostsWithinTheProjectItsCallerMayWrite(t *testing.T) {
	rig := newBrokerTokenRigOn(t, testServerOptions{reviewer: testReviewer(ReviewerConfig{Issues: ReviewHold})})
	caller := newBrokerCaller(t)
	const token = "a-token-keeping-updates-beside-a-held-issue"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	createTestProject(t, rig.handler, brokerTestProject, "OTH")
	own := createOutcomeIssue(t, rig.handler, brokerTestProject, "Its own project's issue")
	outside := createOutcomeIssue(t, rig.handler, "OTH", "An issue of another project")
	held := brokerTestProject + "-900"
	holdFixture(t, rig.database, held, "issue", brokerTestProject)
	keep := func(blocker string) string {
		t.Helper()
		request := caller.presenting(t, http.MethodPatch, "/api/v1/issues/"+held, token, map[string]any{"blocked_by": []string{blocker}})
		request.Header.Set(contracts.AcceptsKeptWriteHeader, "1")
		response := rig.serve(request)
		if response.Code != http.StatusAccepted || decodeBody[heldDeferred](t, response).Held.WaitsOn != held {
			t.Fatalf("the scoped token's update of its own project's held issue naming %s: %d %s, want 202 waiting on %s",
				blocker, response.Code, response.Body.String(), held)
		}
		return decodeBody[heldDeferred](t, response).ID
	}
	// The kept writes post in the order they came; the one its post refuses comes last, so no
	// write waits behind a refused one for the sweep.
	within, across := keep(own), keep(outside)
	postHeldIssueFixture(t, rig.database, rig.served, held)
	if row := waitHeldDecided(t, rig.database, rig.served, across); row.state != heldFailed ||
		!strings.Contains(row.detail, codeProjectScope) || !strings.Contains(row.detail, brokerTestProject) || !strings.Contains(row.detail, "OTH") {
		t.Fatalf("the kept update naming %s is %s (%s), want it failed at its post with %s naming %s and OTH",
			outside, row.state, row.detail, codeProjectScope, brokerTestProject)
	}
	if row := waitHeldDecided(t, rig.database, rig.served, within); row.state != heldPosted {
		t.Fatalf("the kept update naming %s is %s (%s), want it posted", own, row.state, row.detail)
	}
	if blockers := issueBlockers(t, rig.database, held); !slices.Equal(blockers, []string{own}) {
		t.Fatalf("the posted issue %s is blocked by %v, want only %s of its own project", held, blockers, own)
	}
}

// A new issue, or an update, that names a held issue as a blocker waits on it (heldNamedOpenSQL),
// so a project-scoped token is refused at once, with the refusal its post would give, a held
// blocker of another project: 403 PROJECT_SCOPE naming that project, and nothing is held or kept.
// The same writes naming a held issue of its own project are held, and kept, beside it.
func TestAScopedWriteIsRefusedAHeldBlockerOfAnotherProjectAtItsHold(t *testing.T) {
	rig := newBrokerTokenRigOn(t, testServerOptions{reviewer: testReviewer(ReviewerConfig{Issues: ReviewHold})})
	caller := newBrokerCaller(t)
	const token = "a-token-naming-held-blockers"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	createTestProject(t, rig.handler, brokerTestProject, "OTH")
	own := createOutcomeIssue(t, rig.handler, brokerTestProject, "Its own project's issue")
	ownHeld, otherHeld := brokerTestProject+"-900", "OTH-900"
	holdFixture(t, rig.database, ownHeld, "issue", brokerTestProject)
	holdFixture(t, rig.database, otherHeld, "issue", "OTH")
	create := func(blocker string) *httptest.ResponseRecorder {
		t.Helper()
		return rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues", token, map[string]any{
			"project": brokerTestProject, "title": "Blocked by " + blocker, "blocked_by": []string{blocker},
		}))
	}
	update := func(blocker string) *httptest.ResponseRecorder {
		t.Helper()
		request := caller.presenting(t, http.MethodPatch, "/api/v1/issues/"+own, token, map[string]any{"blocked_by": []string{blocker}})
		request.Header.Set(contracts.AcceptsKeptWriteHeader, "1")
		return rig.serve(request)
	}
	held := func() int64 { return countRows(t, rig.database, `select count(*) from held_texts`) }
	before := held()
	for name, refused := range map[string]*httptest.ResponseRecorder{
		"a new issue blocked by another project's held issue":   create(otherHeld),
		"an update adding another project's held issue blocker": update(otherHeld),
	} {
		if refused.Code != http.StatusForbidden {
			t.Fatalf("%s: %d %s, want 403", name, refused.Code, refused.Body.String())
		}
		if refusal := decodeBody[refusalAnswer](t, refused); refusal.Code != codeProjectScope ||
			!strings.Contains(refusal.Error, otherHeld) || !strings.Contains(refusal.Error, "project OTH") {
			t.Fatalf("%s: the refusal = %+v, want %s naming %s of project OTH", name, refusal, codeProjectScope, otherHeld)
		}
	}
	if after := held(); after != before {
		t.Fatalf("the refused writes left %d held texts, want %d", after, before)
	}
	if created := create(ownHeld); created.Code != http.StatusCreated || decodeBody[heldWriteAnswer](t, created).Held == nil ||
		decodeBody[heldWriteAnswer](t, created).Held.WaitsOn != ownHeld {
		t.Fatalf("a new issue blocked by its own project's held issue: %d %s, want 201 held waiting on %s", created.Code, created.Body.String(), ownHeld)
	}
	if kept := update(ownHeld); kept.Code != http.StatusAccepted || decodeBody[heldDeferred](t, kept).Held.WaitsOn != ownHeld {
		t.Fatalf("an update adding its own project's held issue as a blocker: %d %s, want 202 waiting on %s", kept.Code, kept.Body.String(), ownHeld)
	}
}

// A scoped write naming blockers of other projects, one posted and one held, is refused at its
// hold naming the first of them in key order, posted or held, as its post names it once the held
// one posted (writeBlockedBy reads its targets in key order): here the posted one, not the held one
// it would wait on. A new issue (heldIssueCreateError) and an update (refuseHeldForeignBlockers)
// alike, and nothing is held or kept.
func TestAScopedWriteIsRefusedItsFirstForeignBlockerInKeyOrderAtItsHold(t *testing.T) {
	rig := newBrokerTokenRigOn(t, testServerOptions{reviewer: testReviewer(ReviewerConfig{Issues: ReviewHold})})
	caller := newBrokerCaller(t)
	const token = "a-token-naming-a-posted-and-a-held-foreign-blocker"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	createTestProject(t, rig.handler, brokerTestProject, "OTH", "ZZZ")
	own := createOutcomeIssue(t, rig.handler, brokerTestProject, "Its own project's issue")
	posted := createOutcomeIssue(t, rig.handler, "OTH", "A posted issue of another project")
	held := "ZZZ-900"
	holdFixture(t, rig.database, held, "issue", "ZZZ")
	blockers := []string{posted, held}
	update := func() *httptest.ResponseRecorder {
		t.Helper()
		request := caller.presenting(t, http.MethodPatch, "/api/v1/issues/"+own, token, map[string]any{"blocked_by": blockers})
		request.Header.Set(contracts.AcceptsKeptWriteHeader, "1")
		return rig.serve(request)
	}
	before := countRows(t, rig.database, `select count(*) from held_texts`)
	for name, refused := range map[string]*httptest.ResponseRecorder{
		"a new issue": rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues", token, map[string]any{
			"project": brokerTestProject, "title": "Blocked by a posted and a held issue of other projects", "blocked_by": blockers,
		})),
		"an update": update(),
	} {
		if refused.Code != http.StatusForbidden {
			t.Fatalf("%s blocked by %v: %d %s, want 403", name, blockers, refused.Code, refused.Body.String())
		}
		if refusal := decodeBody[refusalAnswer](t, refused); refusal.Code != codeProjectScope ||
			!strings.Contains(refusal.Error, "issue "+posted+" belongs to project OTH") || strings.Contains(refusal.Error, held) {
			t.Fatalf("%s blocked by %v: the refusal = %+v, want %s naming %s of project OTH, the first in key order", name, blockers, refusal, codeProjectScope, posted)
		}
	}
	if after := countRows(t, rig.database, `select count(*) from held_texts`); after != before {
		t.Fatalf("the refused writes left %d held texts, want %d", after, before)
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
// own findings. The server's clock is fixed at lastTenthOfADay, which dates every row, so the day
// counted is the rows' whatever the wall clock reads.
func TestTheCountsCountAWritePostedOverAFindingOnce(t *testing.T) {
	rig := newClockedReviewRig(t, ReviewerConfig{Asks: ReviewHold, Comments: ReviewAfter, Documents: ReviewAfter, WriterWindow: time.Minute}, nil, fixedClock(lastTenthOfADay))
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
	day := lastTenthOfADay.Format(time.DateOnly)
	response := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/writing-check/counts?first="+day+"&last="+day, nil, "alice")
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
