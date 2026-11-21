package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/sessionanswers"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/textversions"
	"github.com/sjawhar/envoy/internal/dispatch/textversions/textversionstest"
)

// TestTheKeywordTableHoldsEachVersionOfEachItem writes, through the API, every kind of item the
// keyword table holds and every change that starts a version of one: an issue with a primary
// document, a second version of the document and a title change; a comment, a reply through the
// delivery callback and the comment's edit; an issue message and a reply to it; an ask, its edit,
// its answer and its answer restored after its block left the document; a comment and an ask on a
// project document; a comment longer than an event carries whole, then edited, and a message that
// long. Three changes start none: an issue.updated that changes only the status, the comment's
// resolve and a message with no issue, which the table leaves out. Between each two changes the
// test keeps a database time at which nothing else is written, and every item's search column and
// text as they stand then (keywordHistory.keep).
//
// One more comment carries the reversed pair: transaction A begins, B edits the comment through
// the API and commits, then A writes the comment's body and an event of its own and commits, so
// A's event has the later seq and the earlier created_at. One kept time falls inside A's window,
// after B's commit, and one after A's commit.
//
// Then the fill writes the table (textversions.FillToHorizon), and at every kept time the relation
// store.TextVersionsAt hands out gives each item that existed one version: its vector is the kept
// column, its body the kept text and none of the text written after it, an issue's names the title
// and the primary document version of that time, and its issue, project, event and seq are its
// event's. Inside A's window the reversed pair's comment has A's version, and after A's commit too.
// No version is the message with no issue's, the long comment's first version and the long
// message's hold their whole bodies, read from the comment's first blocks and from the message's
// row, and at the end every item's version is its last, equal to its present column.
func TestTheKeywordTableHoldsEachVersionOfEachItem(t *testing.T) {
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	defer listener.Close()
	handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.URL})
	ctx := t.Context()
	history := &keywordHistory{t: t, database: database}

	// An issue with its primary document, then the changes of the issue's text: a second version
	// of the document and a title, and an issue.updated that changes only the status.
	issue := createInteractionIssue(t, handler, "TEST", "Rotate the signing keys",
		"# Rotation\n\nThe verifiers read the old key until Monday.\n")
	history.keep("the issue is created with its primary document")
	agentEdit(t, handler, issue.PrimaryArtifactID, 2, replaceOp("until Monday.", "until Tuesday, when the auditors sign."))
	history.keep("a second version of the issue's primary document")
	wantStatus(t, "retitle the issue", dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key,
		map[string]any{"title": "Rotate the signing keys before the audit"}, "alice"), http.StatusOK)
	history.keep("the issue's title changes")
	wantStatus(t, "move the issue's status", dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key,
		map[string]any{"status": "in_progress"}, "alice"), http.StatusOK)
	unchanging := []int64{keywordLatestEvent(t, database, "issue.updated", issue.Key)}
	history.keep("an issue.updated changes only the issue's status")

	// A comment; a comment that mentions a session, and the session's reply through the delivery
	// callback (comment.answered); the first comment's edit; and its resolve, which changes no text,
	// between two kept times.
	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "The verifiers read the old key until Monday."}, "alice")
	history.keep("a comment")
	mentioned := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Can the rotation ship tonight?", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	settledCommentDeliveries(t, database, mentioned.ID)
	history.keep("a comment that mentions a session")
	callbackReply := decodeBody[model.Comment](t, wantStatus(t, "reply through the delivery callback",
		bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+mentioned.ID+"/reply", map[string]any{
			"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "It ships at nine, the canary first.",
		}), http.StatusCreated))
	history.keep("the session's reply through the delivery callback")
	requireEdit(t, "edit the comment", editCommentAs(t, handler, comment.ID, "The verifiers read the old key until Wednesday.", nil, nil))
	history.keep("the comment's edit")
	wantStatus(t, "resolve the comment", dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/resolve",
		map[string]any{}, "alice"), http.StatusOK)
	unchanging = append(unchanging, keywordLatestEvent(t, database, "comment.resolved", issue.Key))
	history.keep("the comment is resolved")

	// An issue message and a reply to it (message.answered).
	message := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "Can the rotation wait for the freeze to end?"}, "alice")
	history.keep("an issue message")
	messageReply := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "It can; the freeze ends Friday.", "in_reply_to": message.ID,
	}, "bob")
	history.keep("a reply to the issue message")

	// An ask, a decision block of the primary document; its edit; its answer; and its answer
	// restored: the answered block leaves the document, the ask's row holds settlement's retraction
	// as a build before the present one wrote it (TestListMyAnswersCollapsesRestoredAnswerEvents),
	// and the block comes back, which writes the answer back with an ask.answered marked restored.
	wantStatus(t, "write an ask block into the primary document", sessionRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
			"actor": sessionActor(), "ops": []map[string]string{{"op": "insert", "after": "end", "markdown": transportAsk}},
		}), http.StatusOK)
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "decision", "Which transport?")
	askID := blockAskID(t, handler, issue.Key, "decision")
	history.keep("an ask")
	wantStatus(t, "edit the ask", sessionRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+askID, map[string]any{
		"question": "Which transport ships first?", "actor": sessionActor(),
	}), http.StatusOK)
	history.keep("the ask's edit")
	wantStatus(t, "answer the ask", dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
		"selected": []string{"REST"}, "text": "REST first; gRPC after the audit.", "expected_edited_at": readBlockAsk(t, handler, askID).EditedAt,
	}, "alice"), http.StatusOK)
	history.keep("the ask's answer")
	wantStatus(t, "delete the answered ask's block", dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
			"ops": []map[string]string{{"op": "delete", "block": "decision"}},
		}, "bob"), http.StatusOK)
	settleDocument(t, handler, issue.PrimaryArtifactID, issue.Key, "after-delete")
	history.keep("the answered ask's block leaves the document")
	// The retraction and the restore are one step: the retraction an older build wrote changed
	// the row's answer with an event this test does not write, so no time is kept between them.
	if _, err := database.Pool.Exec(ctx, `
		update asks
		   set state = 'resolved', answer = null,
		       resolution = jsonb_build_object(
		         'kind', 'retracted',
		         'reason', 'removed from the document in version 9',
		         'actor', jsonb_build_object('kind', 'system', 'id', 'document-settlement'),
		         'at', now())
		 where id = $1`, askID); err != nil {
		t.Fatalf("write the retraction an older build's settlement wrote: %v", err)
	}
	wantStatus(t, "write the answered ask's block back", dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
			"ops": []map[string]string{{"op": "insert", "after": "end", "markdown": strings.Replace(transportAsk,
				"Which transport?", "Which transport ships first?", 1)}},
		}, "bob"), http.StatusOK)
	settleDocument(t, handler, issue.PrimaryArtifactID, issue.Key, "after-restore")
	var restored int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from events
		 where type = 'ask.answered' and payload->>'id' = $1 and payload->>'restored' = 'true'`, askID).Scan(&restored); err != nil {
		t.Fatalf("count the ask's restored answers: %v", err)
	}
	if restored != 1 {
		t.Fatalf("the ask's block came back with %d restored ask.answered events, want one", restored)
	}
	history.keep("the ask's answer is restored")

	// A comment and an ask on a project document, which belong to no issue.
	document := createProjectDocument(t, handler, "TEST", "Runbook", "# Runbook\n\nPage the on-call engineer first.\n")
	documentComment := decodeBody[model.Comment](t, wantStatus(t, "comment on the project document",
		dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/comments",
			map[string]any{"body": "Name the pager rotation here."}, "alice"), http.StatusCreated))
	history.keep("a comment on a project document")
	documentAsk := decodeBody[model.Ask](t, wantStatus(t, "ask on the project document",
		sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/asks",
			map[string]any{"question": "Who owns the pager rotation?", "actor": sessionActor()}), http.StatusCreated))
	history.keep("an ask on a project document")
	if documentComment.IssueKey != nil || documentAsk.IssueKey != nil {
		t.Fatalf("the project document's comment and ask name issues %v and %v, want none", documentComment.IssueKey, documentAsk.IssueKey)
	}

	// A message with no issue, a session's direct message, which the table leaves out.
	direct := decodeBody[model.Message](t, wantStatus(t, "message the session with no issue",
		dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages",
			map[string]any{"body": "The pager rotation moves to Thursday.", "delivery": "btw"}, "alice"), http.StatusCreated))
	if direct.IssueKey != nil {
		t.Fatalf("the direct message names issue %s, want none", *direct.IssueKey)
	}
	history.keep("a message with no issue")

	// A comment longer than an event carries whole, then edited, and an issue message that long.
	longBody := keywordLongBody("verifiers")
	longComment := createThreadComment(t, handler, issue.Key, map[string]any{"body": longBody}, "alice")
	history.keep("a comment longer than an event carries whole")
	requireEdit(t, "edit the long comment", editCommentAs(t, handler, longComment.ID, longBody+" The kestrel signs it off.", nil, nil))
	history.keep("the long comment's edit")
	longMessage := createIssueMessage(t, handler, issue.Key, map[string]any{"body": keywordLongBody("auditors")}, "alice")
	history.keep("an issue message longer than an event carries whole")

	// The reversed pair.
	pair := createThreadComment(t, handler, issue.Key, map[string]any{"body": "The canary takes the first region."}, "alice")
	history.keep("the comment the reversed pair edits")
	a, aStarted := textversionstest.BeginTransaction(t, database)
	defer func() { _ = a.Rollback(context.Background()) }()
	var aBackend int32
	if err := a.QueryRow(ctx, `select pg_backend_pid()`).Scan(&aBackend); err != nil {
		t.Fatalf("read transaction A's backend: %v", err)
	}
	edited := decodeBody[model.Comment](t, wantStatus(t, "B edits the comment",
		editCommentAs(t, handler, pair.ID, "The canary takes the first two regions.", nil, nil), http.StatusOK))
	history.keepInside("inside A's window, after B's commit", aBackend)
	const aBody = "The canary takes every region at once."
	if _, err := a.Exec(ctx, `update comments set body = $2, edited_at = now() where id = $1::uuid`, pair.ID, aBody); err != nil {
		t.Fatalf("A writes the comment's body: %v", err)
	}
	payload := model.CommentEventPayload{Comment: edited}
	payload.Body = aBody
	editedAt := aStarted.UTC().Format(time.RFC3339Nano)
	payload.EditedAt = &editedAt
	issueKey := issue.Key
	aEvent, err := deps.Events.Append(ctx, a, model.Event{
		IssueKey: &issueKey, Type: "comment.edited", Actor: model.Actor{Kind: "user", ID: "alice"}, Payload: payload,
	})
	if err != nil {
		t.Fatalf("A appends its comment.edited: %v", err)
	}
	if err := a.Commit(ctx); err != nil {
		t.Fatalf("commit transaction A: %v", err)
	}
	history.keep("after A's commit")
	requireReversedPair(t, database, pair.ID, aEvent.ID)

	if err := textversions.FillToHorizon(ctx, database, events.NewHorizon(database.Pool)); err != nil {
		t.Fatalf("fill the keyword table: %v", err)
	}
	history.keep("the end, after the fill")
	history.reversed = keywordItem{"comment", pair.ID}
	history.reversedEvent = aEvent.ID
	history.verify()
	if t.Failed() {
		return
	}

	// The replies have versions of their own, started by their own events.
	history.requireVersion("the session's reply through the delivery callback", keywordItem{"comment", callbackReply.ID},
		func(v keywordVersion) bool { return v.eventType == "comment.answered" }, "started by its comment.answered")
	history.requireVersion("a reply to the issue message", keywordItem{"message", messageReply.ID},
		func(v keywordVersion) bool { return v.eventType == "message.answered" }, "started by its message.answered")
	// The long comment's first version and the long message's hold their whole bodies, which
	// their events cut: the comment's read from the blocks of its first revision, since its row
	// has been edited since, and the message's from its row. The edit's version reads the row.
	history.requireVersion("a comment longer than an event carries whole", keywordItem{"comment", longComment.ID},
		func(v keywordVersion) bool { return v.truncated && v.bodyFrom == "blocks" }, "whole from its first blocks, its event's body cut")
	history.requireVersion("the long comment's edit", keywordItem{"comment", longComment.ID},
		func(v keywordVersion) bool { return v.truncated && v.bodyFrom == "row" }, "whole from its row, its event's body cut")
	history.requireVersion("an issue message longer than an event carries whole", keywordItem{"message", longMessage.ID},
		func(v keywordVersion) bool { return v.truncated && v.bodyFrom == "row" }, "whole from its row, its event's body cut")
	// The changes that change no text start no version.
	if rows := textversionstest.RowsOf(t, database, unchanging...); rows != 0 {
		t.Errorf("the status-only issue.updated and the comment.resolved (events %v) have %d rows, want none", unchanging, rows)
	}
}

// TestTheKeywordFillWritesWhileTheServerAnswers runs the fill as a serving process runs it
// (textversions.Run, its process serving) beside the handler of a server whose log holds 2,000
// earlier events, and once the fill has begun catching them up, runs rounds of requests that write
// issues, comments, asks and messages and read them, until the fill has caught up. A second server,
// with the same log and no fill, answers the same rounds: each request answers alike, status and
// body, but for the ids and times each server draws. At the end every event has its rows: each
// earlier event the rows it starts, and the table is the one a fill run on the quiet database from
// no row writes.
func TestTheKeywordFillWritesWhileTheServerAnswers(t *testing.T) {
	filled, quiet := newKeywordServer(t), newKeywordServer(t)
	backlog := seedKeywordBacklog(t, filled.database, filled.deps.Events, 2000)
	seedKeywordBacklog(t, quiet.database, quiet.deps.Events, 2000)
	last := backlog[len(backlog)-1].ID

	fill := startKeywordFill(t, filled)
	fill.AwaitLine(t, keywordBatchLine, time.Minute)
	if cursor := textversionstest.Cursor(t, filled.database); cursor >= last {
		t.Fatalf("the fill's first batch took the cursor to %d, the backlog's last event %d: nothing is left to catch up beside the requests", cursor, last)
	}
	before := len(fill.Lines(t, keywordBatchLine))
	var rounds [][]keywordAnswer
	for round := 1; textversionstest.Cursor(t, filled.database) < last; round++ {
		if round > keywordMaxRounds {
			t.Fatalf("the fill had not caught up the backlog after %d rounds of requests: its cursor is %d of %d",
				keywordMaxRounds, textversionstest.Cursor(t, filled.database), last)
		}
		rounds = append(rounds, keywordRound(t, filled, round, fill.Wake))
	}
	if during := len(fill.Lines(t, keywordBatchLine)) - before; during < 2 {
		t.Fatalf("the fill wrote %d batches while %d rounds of requests ran, want at least two", during, len(rounds))
	}

	for index, filledAnswers := range rounds {
		quietAnswers := keywordRound(t, quiet, index+1, func() {})
		for n := range max(len(filledAnswers), len(quietAnswers)) {
			if n >= len(filledAnswers) || n >= len(quietAnswers) {
				t.Errorf("round %d: the server with the fill answered %d requests, the one without %d", index+1, len(filledAnswers), len(quietAnswers))
				break
			}
			got, want := filledAnswers[n], quietAnswers[n]
			if got != want {
				at := keywordFirstDifference(got.body, want.body)
				t.Errorf("round %d, %s: with the fill it answered %d, from byte %d of its body %.300s\nwith no fill %s answered %d, from byte %d %.300s",
					index+1, got.request, got.status, at, got.body[at:], want.request, want.status, at, want.body[at:])
			}
		}
	}

	// Every event has its rows.
	newest := lastEventID(t, filled.database)
	keywordAwaitCursor(t, filled.database, newest, time.Minute, fill.Wake)
	fill.Stop(t)
	textversionstest.RequireRowsOf(t, filled.database, backlog)
	written := keywordTableRows(t, filled.database, newest)
	if _, err := filled.database.Pool.Exec(t.Context(), `delete from search_text_versions`); err != nil {
		t.Fatalf("empty the keyword table: %v", err)
	}
	if _, err := filled.database.Pool.Exec(t.Context(), `delete from search_text_versions_fill`); err != nil {
		t.Fatalf("empty the keyword fill's cursor: %v", err)
	}
	if err := textversions.FillToHorizon(t.Context(), filled.database, events.NewHorizon(filled.database.Pool)); err != nil {
		t.Fatalf("fill the keyword table again on the quiet database: %v", err)
	}
	again := keywordTableRows(t, filled.database, newest)
	if len(written) != len(again) {
		t.Errorf("the fill beside the requests wrote %d rows for events up to %d; a fill from no row writes %d", len(written), newest, len(again))
	}
	for index := range min(len(written), len(again)) {
		if written[index] != again[index] {
			t.Fatalf("row %d: the fill beside the requests wrote\n%.600s\na fill from no row writes\n%.600s", index, written[index], again[index])
		}
	}
}

// TestTheKeywordFillPausesWhileTheAnswersReadIsSlow runs the fill as a serving process runs it,
// on the server's own sessionanswers.Latency, while it catches up a backlog of 4,000 events. A
// transaction locks message_deliveries, which the answers read reads, so an answers read through
// the handler runs past 800 ms: within one batch the cursor stops, and the fill says it paused.
// For the 20 s the lock then holds, while events are written, the cursor does not move. Once the
// lock is released, reads answer within 800 ms, and for the 10 s after the slow read ended the
// cursor still stands; then the fill says it continues and the cursor moves, and every event,
// those written under the lock included, gets its rows.
func TestTheKeywordFillPausesWhileTheAnswersReadIsSlow(t *testing.T) {
	server := newKeywordServer(t)
	backlog := seedKeywordBacklog(t, server.database, server.deps.Events, 4000)
	last := backlog[len(backlog)-1].ID
	issue := createInteractionIssue(t, server.handler, "TEST", "Rotate the signing keys", "# Rotation\n")
	fill := startKeywordFill(t, server)
	fill.AwaitLine(t, keywordBatchLine, time.Minute)
	if cursor := textversionstest.Cursor(t, server.database); cursor >= last {
		t.Fatalf("the fill's first batch took the cursor to %d, the backlog's last event %d: it is not catching up as the lock starts", cursor, last)
	}

	holder, _ := textversionstest.BeginTransaction(t, server.database)
	defer func() { _ = holder.Rollback(context.Background()) }()
	if _, err := holder.Exec(t.Context(), `lock table message_deliveries in access exclusive mode`); err != nil {
		t.Fatalf("lock message_deliveries: %v", err)
	}
	const answers = "/api/v1/agents/" + answersSession + "/answers"
	held := make(chan *httptest.ResponseRecorder, 1)
	go func() { held <- dispatchRequest(t, server.handler, http.MethodGet, answers, nil, "alice") }()
	waitForDatabaseLocks(t, holder, 1)
	slowDeadline := time.Now().Add(5 * time.Second)
	for !server.latency.Slow(time.Now()) {
		if time.Now().After(slowDeadline) {
			t.Fatal("the answers read held by the lock was no slow read 5 s after it began")
		}
		time.Sleep(10 * time.Millisecond)
	}
	batchesWhenSlow := len(fill.Lines(t, keywordBatchLine))
	fill.AwaitLine(t, keywordPausedLine, 30*time.Second)
	var batchesBeforePause int
	for _, msg := range fill.Lines(t, "") {
		if strings.HasPrefix(msg, keywordPausedLine) {
			break
		}
		if strings.HasPrefix(msg, keywordBatchLine) {
			batchesBeforePause++
		}
	}
	if after := batchesBeforePause - batchesWhenSlow; after > 1 {
		t.Fatalf("the fill wrote %d batches after the answers read turned slow and before it paused, want at most the one in flight", after)
	}
	paused := textversionstest.Cursor(t, server.database)
	if paused >= last {
		t.Fatalf("the fill paused with its cursor at %d, the backlog's last event %d: it was not catching up", paused, last)
	}

	// The lock holds 20 s while events are written, and the cursor stands.
	writes := 0
	releaseAt := time.Now().Add(20 * time.Second)
	for time.Now().Before(releaseAt) {
		writes++
		keywordWriteUnderLock(t, server, issue.Key, writes)
		fill.Wake()
		for sampled := time.Now(); time.Since(sampled) < 2*time.Second && time.Now().Before(releaseAt); {
			if cursor := textversionstest.Cursor(t, server.database); cursor != paused {
				t.Fatalf("the cursor moved from %d to %d while the answers read was held by the lock", paused, cursor)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	if batches := len(fill.Lines(t, keywordBatchLine)); batches != batchesBeforePause {
		t.Fatalf("the fill logged %d batches while paused, want none", batches-batchesBeforePause)
	}
	if err := holder.Rollback(t.Context()); err != nil {
		t.Fatalf("release message_deliveries: %v", err)
	}
	released := time.Now()
	select {
	case response := <-held:
		if response.Code != http.StatusOK {
			t.Fatalf("the held answers read answered %d: %s", response.Code, response.Body.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the held answers read did not answer within 10 s of the lock's release")
	}

	// For the 10 s after the slow read ended, reads answer within 800 ms, and the cursor stands:
	// the slow read ended after the release, so nothing may move before released + 10 s.
	for time.Since(released) < sessionanswers.SlowWithin+time.Second {
		began := time.Now()
		if response := dispatchRequest(t, server.handler, http.MethodGet, answers, nil, "alice"); response.Code != http.StatusOK {
			t.Fatalf("an answers read after the release answered %d: %s", response.Code, response.Body.String())
		}
		if took := time.Since(began); took > sessionanswers.SlowRead {
			t.Fatalf("an answers read after the release took %v, over %v", took, sessionanswers.SlowRead)
		}
		if sampled := time.Now(); sampled.Before(released.Add(sessionanswers.SlowWithin)) {
			if cursor := textversionstest.Cursor(t, server.database); cursor != paused {
				t.Fatalf("the cursor moved from %d to %d %v after the release, inside the %v after the slow read ended",
					paused, cursor, sampled.Sub(released), sessionanswers.SlowWithin)
			}
		}
		time.Sleep(time.Until(began.Add(time.Second)))
	}

	// Then the fill continues: the cursor moves, and every event gets its rows.
	fill.AwaitLine(t, keywordContinuesLine, 20*time.Second)
	moveDeadline := time.Now().Add(20 * time.Second)
	for textversionstest.Cursor(t, server.database) == paused {
		if time.Now().After(moveDeadline) {
			t.Fatalf("the cursor stood at %d 20 s after the fill said it continues", paused)
		}
		time.Sleep(100 * time.Millisecond)
	}
	keywordAwaitCursor(t, server.database, lastEventID(t, server.database), 2*time.Minute, fill.Wake)
	textversionstest.RequireRowsOf(t, server.database, backlog)
	var underLock int
	if err := server.database.Pool.QueryRow(t.Context(), `
		select count(distinct e.id) from events e join search_text_versions v on v.event_id = e.id
		 where e.type = 'comment.created' and e.issue_key = $1`, issue.Key).Scan(&underLock); err != nil {
		t.Fatalf("count the rows of the comments written under the lock: %v", err)
	}
	if underLock != writes {
		t.Fatalf("%d of the %d comments written under the lock have rows", underLock, writes)
	}
}

// requireReversedPair fails unless the comment's two edits are the reversed pair: transaction A's
// event, aEvent, has the later seq and the earlier created_at than B's.
func requireReversedPair(t *testing.T, database *store.Store, comment string, aEvent int64) {
	t.Helper()
	var bEvent int64
	var aSeq, bSeq int32
	var aAt, bAt time.Time
	if err := database.Pool.QueryRow(t.Context(), `
		select b.id, b.seq, b.created_at, a.seq, a.created_at
		  from events a, events b
		 where a.id = $2 and b.type = 'comment.edited' and b.payload->>'id' = $1 and b.id <> a.id`,
		comment, aEvent).Scan(&bEvent, &bSeq, &bAt, &aSeq, &aAt); err != nil {
		t.Fatalf("read the reversed pair's two edits: %v", err)
	}
	if aSeq <= bSeq || !aAt.Before(bAt) {
		t.Fatalf("A's event %d has seq %d and created_at %s, B's event %d seq %d and created_at %s: not a reversed pair",
			aEvent, aSeq, aAt.Format(time.RFC3339Nano), bEvent, bSeq, bAt.Format(time.RFC3339Nano))
	}
}

// keywordLongBody is a body of three paragraphs of about 11,000 UTF-16 units each, each naming
// word: longer than the 32,000 units an event carries whole (model.EventBody), each paragraph one
// block inside the block bound.
func keywordLongBody(word string) string {
	paragraphs := make([]string, 3)
	for index := range paragraphs {
		sentence := fmt.Sprintf("The %s read paragraph %d of the rotation plan before the audit. ", word, index+1)
		paragraphs[index] = strings.TrimSpace(strings.Repeat(sentence, 11000/len(sentence)+1))
	}
	return strings.Join(paragraphs, "\n\n")
}

// keywordLatestEvent is the id of issue's latest event of eventType.
func keywordLatestEvent(t *testing.T, database *store.Store, eventType, issue string) int64 {
	t.Helper()
	var id int64
	if err := database.Pool.QueryRow(t.Context(),
		`select id from events where type = $1 and issue_key = $2 order by id desc limit 1`, eventType, issue).Scan(&id); err != nil {
		t.Fatalf("read %s's latest %s: %v", issue, eventType, err)
	}
	return id
}

// keywordServer is the package's test server with its answers reads timed into latency
// (Deps.AnswersLatency), as a serving process's are, for a fill that pauses on them.
type keywordServer struct {
	handler  http.Handler
	database *store.Store
	deps     Deps
	served   *server
	latency  *sessionanswers.Latency
}

// newKeywordServer stands up a keywordServer on a database of its own.
func newKeywordServer(t *testing.T) keywordServer {
	t.Helper()
	_, database, deps := newTestServer(t, testServerOptions{})
	latency := sessionanswers.NewLatency(time.Now)
	deps.AnswersLatency = latency
	mux := http.NewServeMux()
	served := register(mux, t.Context(), deps)
	t.Cleanup(func() { waitBackgroundOf(t, served) })
	return keywordServer{handler: mux, database: database, deps: deps, served: served, latency: latency}
}

// startKeywordFill runs the fill as a serving process runs it (textversions.Run, through
// textversionstest.StartFill) beside server, until the test stops it or ends: its lease pool
// reserved as the server reserves it, its process serving and its answers reads the server's.
func startKeywordFill(t *testing.T, server keywordServer) *textversionstest.Fill {
	t.Helper()
	callers, err := server.database.Pool.Leases()
	if err != nil {
		t.Fatalf("open the lease pool: %v", err)
	}
	leases, err := textversions.ReserveLeases(callers)
	if err != nil {
		t.Fatalf("reserve the keyword fill's leases: %v", err)
	}
	serving := make(chan struct{})
	close(serving)
	config := textversions.Config{
		Leases:  leases,
		Horizon: events.NewHorizon(server.database.Pool),
		Latency: server.latency,
		Serving: serving,
	}
	return textversionstest.StartFill(t, func(ctx context.Context, wake <-chan struct{}, logger *slog.Logger) error {
		config.Events, config.Logger = wake, logger
		return textversions.Run(ctx, config)
	})
}

// The lines of the fill the tests read: a batch's (filler.record), and the pause's two
// (textversions' pausedMessage and continuesMessage), by the start each one's message has.
const (
	keywordBatchLine     = "dispatch: the keyword fill wrote a batch"
	keywordPausedLine    = "dispatch: the keyword fill paused:"
	keywordContinuesLine = "dispatch: the keyword fill continues:"
)

// keywordAwaitCursor waits until the fill's cursor reaches id, waking the fill each second.
func keywordAwaitCursor(t *testing.T, database *store.Store, id int64, within time.Duration, wake func()) {
	t.Helper()
	deadline := time.Now().Add(within)
	for textversionstest.Cursor(t, database) < id {
		if time.Now().After(deadline) {
			t.Fatalf("the fill's cursor is %d %v on; want event %d", textversionstest.Cursor(t, database), within, id)
		}
		wake()
		time.Sleep(time.Second)
	}
}

// seedKeywordBacklog writes project ACME (textversionstest.SeedProject) and its issues ACME-1
// onwards (textversionstest.WriteIssue) until the log holds n events or more, and answers each
// event with the versions it starts.
func seedKeywordBacklog(t *testing.T, database *store.Store, broker *events.Broker, n int) []textversionstest.Written {
	t.Helper()
	textversionstest.SeedProject(t, database)
	var seeded []textversionstest.Written
	for number := 1; len(seeded) < n; number++ {
		seeded = append(seeded, textversionstest.WriteIssue(t, database, broker, number)...)
	}
	return seeded
}

// keywordTableRows is every row of the keyword table of an event at or below upTo, each as one
// line, in key order.
func keywordTableRows(t *testing.T, database *store.Store, upTo int64) []string {
	t.Helper()
	rows, err := database.Pool.Query(t.Context(), `
		select concat_ws(' | ', kind, id, issue_key, project_key, event_id, event_seq, started_at, body_from,
		                 spec_artifact_id, spec_version, md5(body), search::text)
		  from search_text_versions
		 where event_id <= $1
		 order by kind, id, event_seq`, upTo)
	if err != nil {
		t.Fatalf("read the keyword table: %v", err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the keyword table: %v", err)
	}
	return lines
}

// keywordMaxRounds bounds the rounds of requests that run while the fill catches up.
const keywordMaxRounds = 40

// keywordAnswer is one request of a round and how it was answered: its status, and its body with
// the ids and times a server draws masked (keywordMasked).
type keywordAnswer struct {
	request string
	status  int
	body    string
}

// keywordRound is one round of requests a client makes, in a project of its own so the near-copy
// gate compares its issue with nothing: it writes the project, an issue, a comment and its edit, an
// ask and its answer and a message, reads each, reads an issue of the backlog, and searches for the
// word only the comment's edit wrote, whose one result ranks in no tie the servers' ids would break.
// After each write it wakes the fill and waits out the write's background work
// (awaitBackgroundWrites), so the next request reads what any server would.
func keywordRound(t *testing.T, server keywordServer, round int, wake func()) []keywordAnswer {
	t.Helper()
	var answers []keywordAnswer
	project := fmt.Sprintf("LOAD%d", round)
	answer := func(request string, response *httptest.ResponseRecorder, wrote bool) *httptest.ResponseRecorder {
		t.Helper()
		answers = append(answers, keywordAnswer{request: request, status: response.Code, body: keywordMasked(response.Body.String())})
		if wrote {
			wake()
			awaitBackgroundWrites(t, server.served)
		}
		return response
	}
	h := server.handler
	answer("POST /api/v1/projects", dispatchRequest(t, h, http.MethodPost, "/api/v1/projects",
		map[string]string{"key": project, "name": "Load " + project}, "alice"), true)
	created := wantStatus(t, "create the round's issue", answer("POST /api/v1/issues", dispatchRequest(t, h, http.MethodPost,
		"/api/v1/issues", map[string]string{
			"project": project, "title": fmt.Sprintf("Rotate the signing keys of round %d", round),
			"spec": fmt.Sprintf("# Round %d\n\nThe verifiers read the old key until the audit.\n", round),
		}, "alice"), true), http.StatusCreated)
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, created).Key
	answer("GET /api/v1/issues/{key}", dispatchRequest(t, h, http.MethodGet, "/api/v1/issues/"+issue, nil, "alice"), false)

	comment := decodeBody[model.Comment](t, wantStatus(t, "comment on the round's issue", answer("POST /api/v1/issues/{key}/comments",
		dispatchRequest(t, h, http.MethodPost, "/api/v1/issues/"+issue+"/comments",
			map[string]any{"body": fmt.Sprintf("Round %d: the verifiers read the old key until Monday.", round)}, "alice"), true), http.StatusCreated))
	answer("PATCH /api/v1/comments/{id}", dispatchRequest(t, h, http.MethodPatch, "/api/v1/comments/"+comment.ID,
		map[string]any{"body": fmt.Sprintf("Round %d: the verifiers read the old key until Tuesday.", round)}, "alice"), true)
	answer("GET /api/v1/comments/{id}", dispatchRequest(t, h, http.MethodGet, "/api/v1/comments/"+comment.ID, nil, "alice"), false)

	ask := decodeBody[model.Ask](t, wantStatus(t, "ask on the round's issue", answer("POST /api/v1/issues/{key}/asks",
		sessionRequest(t, h, http.MethodPost, "/api/v1/issues/"+issue+"/asks", map[string]any{
			"question": fmt.Sprintf("Rotate round %d's keys now?", round),
			"options":  []map[string]string{{"label": "Rotate now"}, {"label": "Wait a week"}},
			"actor":    sessionActor(),
		}), true), http.StatusCreated))
	answer("POST /api/v1/asks/{id}/answer", dispatchRequest(t, h, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer",
		map[string]any{"selected": []string{"Rotate now"}, "text": "Before the audit."}, "alice"), true)
	answer("GET /api/v1/asks/{id}", dispatchRequest(t, h, http.MethodGet, "/api/v1/asks/"+ask.ID, nil, "alice"), false)

	answer("POST /api/v1/issues/{key}/messages", dispatchRequest(t, h, http.MethodPost, "/api/v1/issues/"+issue+"/messages",
		map[string]any{"body": fmt.Sprintf("Can round %d wait for the freeze to end?", round)}, "alice"), true)
	answer("GET /api/v1/issues/{key}/messages", dispatchRequest(t, h, http.MethodGet, "/api/v1/issues/"+issue+"/messages", nil, "alice"), false)

	answer("GET /api/v1/issues/{backlog key}", dispatchRequest(t, h, http.MethodGet, fmt.Sprintf("/api/v1/issues/ACME-%d", round), nil, "alice"), false)
	answer("GET /api/v1/search", dispatchRequest(t, h, http.MethodGet, "/api/v1/search?q=Tuesday&project="+project, nil, "alice"), false)
	return answers
}

// keywordUUID, keywordTime and keywordTook match the ids and the times a server draws, and the
// milliseconds a request took that it reports (a search's took_ms), which two servers answering the
// same requests draw differently.
var (
	keywordUUID = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	keywordTime = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`)
	keywordTook = regexp.MustCompile(`("[a-z_]+_ms"):\d+`)
)

// keywordMasked is body with its ids, times and timings masked.
func keywordMasked(body string) string {
	masked := keywordTime.ReplaceAllString(keywordUUID.ReplaceAllString(body, "<uuid>"), "<time>")
	return keywordTook.ReplaceAllString(masked, "$1:<ms>")
}

// keywordFirstDifference is where a failure message shows two differing bodies from: 100 bytes
// before the first byte at which they differ, so the message shows the difference in its place.
func keywordFirstDifference(a, b string) int {
	at := 0
	for at < min(len(a), len(b)) && a[at] == b[at] {
		at++
	}
	return max(0, at-100)
}

// keywordWriteUnderLock writes a comment on issue through the API while message_deliveries is
// locked, which a comment's write does not read, and fails when it does not answer within 5 s.
func keywordWriteUnderLock(t *testing.T, server keywordServer, issue string, n int) {
	t.Helper()
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- dispatchRequest(t, server.handler, http.MethodPost, "/api/v1/issues/"+issue+"/comments",
			map[string]any{"body": fmt.Sprintf("Note %d, written while the answers read waits.", n)}, "alice")
	}()
	select {
	case response := <-answered:
		if response.Code != http.StatusCreated {
			t.Fatalf("write comment %d under the lock: status=%d body=%s", n, response.Code, response.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("comment %d, written under the lock on message_deliveries, did not answer within 5 s", n)
	}
}
