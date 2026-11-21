package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The reviewer's writes that are never held (INFRA-3231's R5 and Decisions 8 and 26): a person's
// own typed command, posted at once with no review; a reply a plugin generated, a send to an agent,
// a handoff and a person's own ask, each posted at once with no review; a document version, saved
// at once and reviewed after the save; and an agent's edit of a posted ask, comment or issue title,
// saved at once and reviewed after the save, with nothing more happening when the writer does not
// act.

// asWriterTyped writes as the rig's writer session with Dispatch-Person-Typed: 1, as the dispatch
// command does for a line a person typed in that session.
func (rig *reviewRig) asWriterTyped(method, target string, body map[string]any) *httptest.ResponseRecorder {
	rig.t.Helper()
	return rig.asWriterMarked(contracts.PersonTypedHeader, method, target, body)
}

// asWriterGenerated writes as the rig's writer session with Dispatch-Generated-Text: 1, as a
// plugin posts text it generated rather than its agent wrote: a person's !command report, a slash
// command's output.
func (rig *reviewRig) asWriterGenerated(method, target string, body map[string]any) *httptest.ResponseRecorder {
	rig.t.Helper()
	return rig.asWriterMarked(contracts.GeneratedTextHeader, method, target, body)
}

// asWriterMarked writes as the rig's writer session with the header marker set to 1.
func (rig *reviewRig) asWriterMarked(marker, method, target string, body map[string]any) *httptest.ResponseRecorder {
	rig.t.Helper()
	withActor := map[string]any{"actor": map[string]any{"kind": "session", "id": heldWriter}}
	for key, value := range body {
		withActor[key] = value
	}
	data, err := json.Marshal(withActor)
	if err != nil {
		rig.t.Fatal(err)
	}
	request := httptest.NewRequest(method, target, bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer agent-token")
	request.Header.Set(marker, "1")
	response := httptest.NewRecorder()
	rig.handler.ServeHTTP(response, request)
	return response
}

// reviewCalls counts the stand-in's review calls whose text holds marker.
func (rig *reviewRig) reviewCalls(marker string) int {
	calls := 0
	for _, request := range rig.stand.Requests() {
		if strings.Contains(request.Text, reviewMarker) && strings.Contains(reviewedText(request), marker) {
			calls++
		}
	}
	return calls
}

// waitFrame waits for a findings frame naming id and answers its body.
func (rig *reviewRig) waitFrame(id string) string {
	rig.t.Helper()
	var body string
	pollUntil(rig.t, 20*time.Second, "a findings frame naming "+id, func() bool {
		for _, frame := range rig.listener.frames() {
			if frame["id"] == id {
				body, _ = frame["body"].(string)
				return true
			}
		}
		return false
	})
	return body
}

// outcomeCount counts the writing_check_events rows of outcome.
func (rig *reviewRig) outcomeCount(outcome string) int64 {
	return countRows(rig.t, rig.database, `select count(*) from writing_check_events where outcome = $1`, outcome)
}

// A comment the writer session's person typed (`!dispatch comment …` through pi-envoy's typed
// shell, which the dispatch command marks Dispatch-Person-Typed: 1) posts at once with the
// reviewer set to hold every kind: no hold, no review call, a row exempt_person_typed with its
// session, and its comment.created marked person_typed. The same comment with no header is held
// (the negative control).
func TestPersonTypedCommentPostsAtOnceUnderHold(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Asks: ReviewHold, Issues: ReviewHold, Comments: ReviewHold, Messages: ReviewHold}, nil)
	typed := rig.asWriterTyped(http.MethodPost, "/api/v1/issues/"+rig.issue+"/comments", map[string]any{
		"body": "Status\nWe spin up the node. " + vacuousPhrase,
	})
	if typed.Code != http.StatusCreated {
		t.Fatalf("the typed comment: %d %s", typed.Code, typed.Body.String())
	}
	answer := decodeBody[heldWriteAnswer](t, typed)
	if answer.Held != nil || answer.ID == "" {
		t.Fatalf("the typed comment answered %s, want it posted with no held object", typed.Body.String())
	}
	if !slices.Contains(rig.commentIDs(), answer.ID) {
		t.Fatal("alice does not read the typed comment at once")
	}
	var actorID string
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select actor_id from writing_check_events where outcome = 'exempt_person_typed'`).Scan(&actorID); err != nil {
		t.Fatalf("read the exempt_person_typed row: %v", err)
	}
	if actorID != heldWriter {
		t.Fatalf("the exempt_person_typed row names %q, want the session %s", actorID, heldWriter)
	}
	if _, reader, project := rig.eventOf(writingCheckPersonTyped); reader != "alice" || project != "RVW" {
		t.Fatalf("the exempt_person_typed row: reader %q project %q", reader, project)
	}
	if marked := countRows(t, rig.database, `
		select count(*) from events where type = 'comment.created' and payload->>'id' = $1 and (payload->>'person_typed')::boolean`,
		answer.ID); marked != 1 {
		t.Fatalf("%d comment.created events of the typed comment carry person_typed, want 1", marked)
	}
	time.Sleep(300 * time.Millisecond)
	if calls := rig.reviewCalls("We spin up the node."); calls != 0 {
		t.Fatalf("the reviewer read the typed comment %d times, want none", calls)
	}
	if held := rig.outcomeCount(reviewHeld); held != 0 {
		t.Fatalf("%d held rows for a typed comment", held)
	}
	// The negative control: the agent's own comment, with no header, is held.
	rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/comments", map[string]any{
		"body": "Status\nWe spin up the node. " + vacuousPhrase,
	}), "the agent's own comment")
}

// messageToWriter is alice's message to the writer session, sent as a btw: its id, whose attempt
// 1 the writer answers.
func (rig *reviewRig) messageToWriter(body string) string {
	rig.t.Helper()
	sent := dispatchRequest(rig.t, rig.handler, http.MethodPost, "/api/v1/agents/"+heldWriter+"/messages", map[string]any{"body": body, "delivery": "btw"}, "alice")
	if sent.Code != http.StatusCreated {
		rig.t.Fatalf("alice's message to the writer: %d %s", sent.Code, sent.Body.String())
	}
	return decodeBody[model.Message](rig.t, sent).ID
}

// mentionOfWriter is alice's comment on the rig's issue that mentions the writer session, once its
// delivery settled: its id, whose attempt 1 the writer answers.
func (rig *reviewRig) mentionOfWriter(body string) string {
	rig.t.Helper()
	comment := decodeMentionedComment(rig.t, postMentionedComment(rig.t, rig.handler, rig.issue, map[string]any{
		"body": body, "mentions": []map[string]any{{"target": "session:" + heldWriter}},
	}))
	settledCommentDeliveries(rig.t, rig.database, comment.ID)
	return comment.ID
}

// A reply a plugin marks as generated (Dispatch-Generated-Text: 1, which pi-envoy sends with a
// person's !command report) posts at once on the reply route of a delivered message and of a
// delivered mention, with the reviewer holding comments and messages: the answer carries no held
// object, alice reads the reply at once, no text is held, the reviewer is never called, and the
// writing check records the reply exempt_generated with alice as its reader. The same reply with
// no header is held on each route (the negative control).
func TestAGeneratedReplyPostsAtOnceUnderHold(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Comments: ReviewHold, Messages: ReviewHold, WriterWindow: time.Minute}, nil)
	const report = "Status\nWe spin up the node. " + vacuousPhrase
	routes := []struct {
		pattern string
		// delivered is the reply route of a new delivery to the writer.
		delivered func() string
		// read is the reply id's body as alice reads it in its thread.
		read func(id string) string
	}{
		{
			"/api/v1/messages/{id}/reply",
			func() string { return "/api/v1/messages/" + rig.messageToWriter("Is it done?") + "/reply" },
			func(id string) string {
				read := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/messages/"+id, nil, "alice")
				if read.Code != http.StatusOK {
					t.Fatalf("alice's read of the reply %s: %d %s", id, read.Code, read.Body.String())
				}
				for _, reply := range decodeBody[messageRead](t, read).Replies {
					if reply.ID == id {
						return reply.Body
					}
				}
				t.Fatalf("alice's read of the thread holds no reply %s: %s", id, read.Body.String())
				return ""
			},
		},
		{
			"/api/v1/comments/{id}/reply",
			func() string { return "/api/v1/comments/" + rig.mentionOfWriter("Is it done?") + "/reply" },
			func(id string) string { return readCommentThread(t, rig.handler, id).Comment.Body },
		},
	}
	for _, route := range routes {
		response := rig.asWriterGenerated(http.MethodPost, route.delivered(), map[string]any{"attempt": 1, "body": report})
		if response.Code != http.StatusCreated || strings.Contains(response.Body.String(), `"held"`) {
			t.Fatalf("the generated reply on %s: %d %s, want it posted at once", route.pattern, response.Code, response.Body.String())
		}
		id := decodeBody[heldWriteAnswer](t, response).ID
		if body := route.read(id); body != report {
			t.Fatalf("alice reads the generated reply on %s as %q, want %q", route.pattern, body, report)
		}
		if generated := countRows(t, rig.database, `
			select count(*) from writing_check_events
			where method = 'POST' and route = $1 and outcome = $2 and reader = 'alice'`, route.pattern, writingCheckGenerated); generated != 1 {
			t.Fatalf("%d %s rows with reader alice for the reply on %s, want 1", generated, writingCheckGenerated, route.pattern)
		}
		if rows := countRows(t, rig.database, `select count(*) from writing_check_events where method = 'POST' and route = $1`, route.pattern); rows != 1 {
			t.Fatalf("%d writing_check_events rows for the reply on %s, want its one", rows, route.pattern)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if calls := rig.reviewCalls("We spin up the node."); calls != 0 {
		t.Fatalf("the reviewer read the generated replies %d times, want none", calls)
	}
	if held := countRows(t, rig.database, `select count(*) from held_texts`); held != 0 {
		t.Fatalf("%d held texts for generated replies, want none", held)
	}
	if outcomes := rig.outcomes(); len(outcomes) != 0 {
		t.Fatalf("the reviewer recorded %v for generated replies, want nothing", outcomes)
	}
	// The negative control: the agent's own reply, with no header, is held on each route.
	for _, route := range routes {
		rig.held(rig.asWriter(http.MethodPost, route.delivered(), map[string]any{"attempt": 1, "body": report}), "the agent's own reply on "+route.pattern)
	}
}

// An agent session's send to another session (POST /api/v1/agents/{session_id}/messages, with its
// person's token acting for it) whose body is the sentence the stand-in flags answers 201 at once
// with the reviewer holding messages: no held object, no held text, no review call, and the
// message reaches the recipient session. No person reads a send to an agent, so the writing check
// records it exempt_no_reader.
func TestASendToAnAgentPostsAtOnceUnderHold(t *testing.T) {
	rig := newEditRig(t, ReviewerConfig{Messages: ReviewHold, WriterWindow: time.Minute})
	const recipient = "s-reader"
	rig.listener.start(recipient)
	sent := rig.writeAsPerson(http.MethodPost, "/api/v1/agents/"+recipient+"/messages", map[string]any{
		"body": vacuousPhrase, "delivery": "btw",
	})
	if sent.Code != http.StatusCreated || strings.Contains(sent.Body.String(), `"held"`) {
		t.Fatalf("the writer's send to %s: %d %s, want it posted at once", recipient, sent.Code, sent.Body.String())
	}
	message := decodeBody[model.Message](t, sent)
	if message.Body != vacuousPhrase || message.Author.Act == nil || message.Author.Act.Session != heldWriter {
		t.Fatalf("the send = %+v, want the sentence written for the writer session", message)
	}
	pollUntil(t, 10*time.Second, "the send's delivery to "+recipient, func() bool { return rig.listener.deliveredTo(recipient, message.ID) })
	if unread := countRows(t, rig.database, `
		select count(*) from writing_check_events
		where method = 'POST' and route = '/api/v1/agents/{session_id}/messages' and outcome = $1`, writingCheckNoReader); unread != 1 {
		t.Fatalf("%d %s rows for the send, want 1", unread, writingCheckNoReader)
	}
	time.Sleep(300 * time.Millisecond)
	if calls := rig.reviewCalls(vacuousPhrase); calls != 0 {
		t.Fatalf("the reviewer read the send %d times, want none", calls)
	}
	if held := countRows(t, rig.database, `select count(*) from held_texts`); held != 0 {
		t.Fatalf("%d held texts for a send to an agent, want none", held)
	}
}

// everyKindHeld is the reviewer holding every kind it may hold, and reviewing a document version,
// which is never held, after its save.
func everyKindHeld() ReviewerConfig {
	return ReviewerConfig{
		Asks: ReviewHold, Issues: ReviewHold, Comments: ReviewHold, Messages: ReviewHold,
		Documents: ReviewAfter, DecisionBlocks: ReviewHold, WriterWindow: time.Minute,
	}
}

// An agent's handoff (POST /api/v1/issues/{key}/handoffs/{phase}) whose body holds the sentence the
// stand-in flags answers 201 at once with the reviewer holding every kind: no held object, no held
// text, no review call, and the phase's latest handoff reads back the body exactly as written.
func TestAHandoffPostsAtOnceWithEveryKindHeld(t *testing.T) {
	rig := newReviewRig(t, everyKindHeld(), nil)
	body := `{"summary":"` + vacuousPhrase + `","next":"review"}`
	written := rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/handoffs/implement", map[string]any{"body": body})
	if written.Code != http.StatusCreated || strings.Contains(written.Body.String(), `"held"`) {
		t.Fatalf("the writer's handoff: %d %s, want it recorded at once", written.Code, written.Body.String())
	}
	summary := decodeBody[model.HandoffSummary](t, written)
	latest := agentRequest(t, rig.handler, http.MethodGet, "/api/v1/issues/"+rig.issue+"/handoffs/implement/latest", nil, "agent-token")
	if latest.Code != http.StatusOK {
		t.Fatalf("the latest implement handoff: %d %s", latest.Code, latest.Body.String())
	}
	if read := decodeBody[model.Handoff](t, latest); read.ID != summary.ID || read.Version != 1 || read.Body != body {
		t.Fatalf("the latest implement handoff = %+v, want version 1 %s with the body %q", read, summary.ID, body)
	}
	time.Sleep(300 * time.Millisecond)
	if calls := rig.reviewCalls(vacuousPhrase); calls != 0 {
		t.Fatalf("the reviewer read the handoff %d times, want none", calls)
	}
	if held := countRows(t, rig.database, `select count(*) from held_texts`); held != 0 {
		t.Fatalf("%d held texts for a handoff, want none", held)
	}
}

// A person's own ask, written in the web app (the X-Dispatch-User identity this suite signs a
// person in with, in place of the browser's cookie), posts at once with the reviewer holding asks:
// the answer carries no held object, another person reads the ask at once, no text is held and the
// reviewer is never called. The agent session's same ask is held (the negative control).
func TestAPersonsAskPostsAtOnceUnderHold(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Asks: ReviewHold, WriterWindow: time.Minute}, nil)
	question := "Ship the reviewer?\n" + vacuousPhrase
	ask := map[string]any{"question": question, "options": writtenOptions}
	asked := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+rig.issue+"/asks", ask, "alice")
	if asked.Code != http.StatusCreated || strings.Contains(asked.Body.String(), `"held"`) {
		t.Fatalf("alice's ask: %d %s, want it posted at once", asked.Code, asked.Body.String())
	}
	id := decodeBody[heldWriteAnswer](t, asked).ID
	read := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/asks/"+id, nil, "bob")
	if read.Code != http.StatusOK {
		t.Fatalf("bob's read of alice's ask: %d %s", read.Code, read.Body.String())
	}
	if got := decodeBody[askRead](t, read).Ask.Question; got != question {
		t.Fatalf("bob reads alice's ask as %q, want %q", got, question)
	}
	time.Sleep(300 * time.Millisecond)
	if calls := rig.reviewCalls(vacuousPhrase); calls != 0 {
		t.Fatalf("the reviewer read alice's ask %d times, want none", calls)
	}
	if held := countRows(t, rig.database, `select count(*) from held_texts`); held != 0 {
		t.Fatalf("%d held texts for a person's ask, want none", held)
	}
	if outcomes := rig.outcomes(); len(outcomes) != 0 {
		t.Fatalf("the reviewer recorded %v for a person's ask, want nothing", outcomes)
	}
	// The negative control: the agent session's same ask is held.
	rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/asks", ask), "the agent's same ask")
}

// An agent's document version saves at once with DISPATCH_REVIEWER_DOCUMENTS=after, and its
// findings reach the writer as a frame naming the document, recorded reviewed_after; with the
// setting off no review runs.
func TestTheReviewerReadsADocumentVersionAfterItsSave(t *testing.T) {
	content := "# Rollout notes\n\nStatus of the rollout. " + vacuousPhrase + "\n"
	t.Run("after", func(t *testing.T) {
		rig := newReviewRig(t, ReviewerConfig{Documents: ReviewAfter}, nil)
		uploaded := rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/artifacts", map[string]any{
			"name": "rollout.md", "content": content,
		})
		if uploaded.Code != http.StatusCreated {
			t.Fatalf("the upload: %d %s", uploaded.Code, uploaded.Body.String())
		}
		var answer artifactUploadResponse
		if err := json.Unmarshal(uploaded.Body.Bytes(), &answer); err != nil || answer.Artifact.ID == "" {
			t.Fatalf("the upload answered %s (%v)", uploaded.Body.String(), err)
		}
		body := rig.waitFrame(answer.Artifact.ID)
		if !strings.Contains(body, "Dispatch posted your document") || !strings.Contains(body, vacuousPhrase) {
			t.Fatalf("the document's frame reads %q", body)
		}
		pollUntil(t, 10*time.Second, "the document's reviewed_after row", func() bool { return rig.outcomeCount(reviewReviewedAfter) == 1 })
		if held := countRows(t, rig.database, `select count(*) from held_texts`); held != 0 {
			t.Fatalf("%d held texts for a document version", held)
		}
	})
	t.Run("off", func(t *testing.T) {
		rig := newReviewRig(t, ReviewerConfig{Comments: ReviewHold}, nil)
		uploaded := rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/artifacts", map[string]any{
			"name": "rollout.md", "content": content,
		})
		if uploaded.Code != http.StatusCreated {
			t.Fatalf("the upload: %d %s", uploaded.Code, uploaded.Body.String())
		}
		// A held comment's review is the marker that the reviewer ran since the upload.
		comment := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/comments", map[string]any{
			"body": "Status\nThe sweep runs. " + vacuousPhrase,
		}), "the marker comment")
		rig.waitHeld(comment.ID, heldAwaitingWriter)
		if calls := rig.reviewCalls("Status of the rollout."); calls != 0 {
			t.Fatalf("the reviewer read the document %d times with the setting off", calls)
		}
	})
}

// editRig is a reviewer rig whose writer is a personal token of bob acting for the writer session,
// the credential an agent edits a comment with (the comment route acts as a person).
type editRig struct {
	*reviewRig
	token string
}

func newEditRig(t *testing.T, cfg ReviewerConfig) editRig {
	rig := newReviewRig(t, cfg, nil)
	token := mintPersonalToken(t, rig.handler, "bob", "writer", []string{model.ScopePerson})
	return editRig{reviewRig: rig, token: token.Token}
}

// write writes as the token's person acting for the writer session, naming that session as the
// actor, as the ask and issue routes take a bearer caller's write.
func (rig editRig) write(method, target string, body map[string]any) *httptest.ResponseRecorder {
	rig.t.Helper()
	withActor := map[string]any{"actor": map[string]any{"kind": "session", "id": heldWriter}}
	for key, value := range body {
		withActor[key] = value
	}
	return rig.writeAsPerson(method, target, withActor)
}

// writeAsPerson writes as the token's person acting for the writer session, naming no actor.
func (rig editRig) writeAsPerson(method, target string, body map[string]any) *httptest.ResponseRecorder {
	rig.t.Helper()
	separator := "?"
	if strings.Contains(target, "?") {
		separator = "&"
	}
	return agentRequest(rig.t, rig.handler, method, target+separator+"session="+heldWriter, body, rig.token)
}

// posted is the id a write answers, once its text has posted: at once where it was not held, and
// once its review passed where it was.
func (rig editRig) posted(response *httptest.ResponseRecorder, what string) string {
	rig.t.Helper()
	if response.Code != http.StatusCreated {
		rig.t.Fatalf("%s: %d %s", what, response.Code, response.Body.String())
	}
	answer := decodeBody[heldWriteAnswer](rig.t, response)
	if answer.ID == "" {
		answer.ID = answer.Key
	}
	if answer.Held != nil {
		rig.waitHeld(answer.ID, heldPosted)
	}
	return answer.ID
}

// An agent's edit through each of PATCH /api/v1/asks/{id}, PATCH /api/v1/comments/{id} and PATCH
// /api/v1/issues/{key} saves at once whatever the kind's setting, and with the setting after or
// hold its findings reach the writer as a frame naming the text; a writer that does not act leaves
// the edit as it saved. With the setting off no review runs (Decision 8 of INFRA-3231's plan).
func TestTheReviewerReadsAnAgentsEditAfterTheSave(t *testing.T) {
	for _, setting := range []string{ReviewAfter, ReviewHold, ReviewOff} {
		t.Run(setting, func(t *testing.T) {
			cfg := ReviewerConfig{Asks: setting, Issues: setting, Comments: setting, WriterWindow: time.Minute}
			if setting == ReviewOff {
				cfg.Messages = ReviewHold
			}
			rig := newEditRig(t, cfg)
			askID := rig.posted(rig.write(http.MethodPost, "/api/v1/issues/"+rig.issue+"/asks", map[string]any{
				"question": "Ship the reviewer today?",
				"options":  []map[string]string{{"label": "Ship it", "description": "Release it today."}, {"label": "Do not file", "description": "Wait."}},
			}), "the ask")
			// The comment route edits as the token's person, beside the session it acts for, so
			// the posted comment is bob's acting for the writer session, as the store holds it.
			var commentID string
			if err := rig.database.Pool.QueryRow(context.Background(), `
				insert into comments (issue_key, author, body)
				values ($1, jsonb_build_object('kind', 'user', 'id', 'bob', 'act', jsonb_build_object('session', $2::text)),
					'Status'||chr(10)||'The sweep runs every ten seconds.')
				returning id::text`, rig.issue, heldWriter).Scan(&commentID); err != nil {
				t.Fatalf("seed the posted comment: %v", err)
			}
			issueKey := rig.posted(rig.write(http.MethodPost, "/api/v1/issues", map[string]any{
				"project": "RVW", "title": "Retry the sweep", "spec": "The sweep retries a failed post.",
			}), "the issue")

			edits := []struct {
				what, target, id, marker string
				body                     map[string]any
				stored                   string
			}{
				{"ask", "/api/v1/asks/" + askID, askID, "Ship the reviewer today? ",
					map[string]any{"question": "Ship the reviewer today? " + vacuousPhrase},
					`select question from asks where id = $1::uuid`},
				{"comment", "/api/v1/comments/" + commentID, commentID, "Status\nThe sweep runs. ",
					map[string]any{"body": "Status\nThe sweep runs. " + vacuousPhrase},
					`select body from comments where id = $1::uuid`},
				{"issue", "/api/v1/issues/" + issueKey, issueKey, "Retry the sweep. ",
					map[string]any{"title": "Retry the sweep. " + vacuousPhrase},
					`select title from issues where key = $1`},
			}
			for _, edit := range edits {
				write := rig.write
				if edit.what == "comment" {
					write = rig.writeAsPerson
				}
				response := write(http.MethodPatch, edit.target, edit.body)
				if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"held"`) {
					t.Fatalf("the %s edit: %d %s, want it saved at once", edit.what, response.Code, response.Body.String())
				}
			}
			if setting == ReviewOff {
				marker := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/messages", map[string]any{
					"body": "Status\nThe marker message. " + vacuousPhrase,
				}), "the marker message")
				rig.waitFrame(marker.ID)
			}
			for _, edit := range edits {
				if setting == ReviewOff {
					if calls := rig.reviewCalls(edit.marker); calls != 0 {
						t.Fatalf("the reviewer read the %s edit %d times with the setting off", edit.what, calls)
					}
					continue
				}
				body := rig.waitFrame(edit.id)
				if !strings.Contains(body, "Dispatch posted your") || !strings.Contains(body, vacuousPhrase) {
					t.Fatalf("the %s edit's frame reads %q", edit.what, body)
				}
			}
			// The writer does not act: each edit stays as it saved.
			time.Sleep(300 * time.Millisecond)
			for _, edit := range edits {
				var stored string
				if err := rig.database.Pool.QueryRow(context.Background(), edit.stored, edit.id).Scan(&stored); err != nil {
					t.Fatalf("read the edited %s: %v", edit.what, err)
				}
				if !strings.Contains(stored, vacuousPhrase) {
					t.Fatalf("the edited %s reads %q, want the edit as it saved", edit.what, stored)
				}
			}
		})
	}
}
