// as_of_logs_test.go holds the three event logs at a cut to the cases the equality harness
// (as_of_equality_test.go) does not write: the events of posts deleted after the cut in each log,
// beside a post deleted before it and an edited message; the page of the whole log that reaches
// the cut; and the validator of an issue's log at a cut.
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// loggedPayloads is the payload of each event of an event log's answer whose payload names the
// post id, in the order of the answer.
func loggedPayloads(t *testing.T, body []byte, id string) []map[string]any {
	t.Helper()
	var events []model.Event
	if err := json.Unmarshal(body, &events); err != nil {
		t.Fatalf("decode an event log: %v: %s", err, abbreviateAnswer(body))
	}
	var payloads []map[string]any
	for _, event := range events {
		if payload, _ := event.Payload.(map[string]any); payload != nil && payload["id"] == id {
			payloads = append(payloads, payload)
		}
	}
	return payloads
}

// loggedEventIDs is the id of each event of an event log's answer, in the order of the answer.
func loggedEventIDs(t *testing.T, body []byte) []int64 {
	t.Helper()
	var events []model.Event
	if err := json.Unmarshal(body, &events); err != nil {
		t.Fatalf("decode an event log: %v: %s", err, abbreviateAnswer(body))
	}
	ids := make([]int64, len(events))
	for index := range events {
		ids[index] = events[index].ID
	}
	return ids
}

// TestAPostDeletedAfterTheCutAnswersItsOwnEventsWithNoBody: each event log at a cut gives the
// events of a post that its author deleted after the cut as the present log gives them: with the
// post's delete and no body. The answer names /*/payload/body absent. A reply's event keeps the
// preview that it froze of the deleted message. The events of a message edited before and after
// the cut read as written, with the edit mark at the cut. A comment deleted before the cut was
// deleted at the cut, so a page that holds only it and the edited message names nothing absent.
// The logs are an issue's (a comment, a message and the reply), a project document's (a comment
// on the document) and the whole log (all of them). Each answer at the cut is held to the answer
// read at the cut by the harness's comparison, which takes each deleted post's body and marks out
// of both answers and compares the rest.
func TestAPostDeletedAfterTheCutAnswersItsOwnEventsWithNoBody(t *testing.T) {
	world, takeCut := newAsOfThreadWorld(t, "ASDE")
	handler := world.handler
	handbook := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, asOfWrite(t, handler, http.MethodPost, "/api/v1/projects/ASDE/artifacts", map[string]string{
		"name": "handbook.md", "content": "# Handbook\n\nThe handbook.\n",
	}, "alice")).Artifact

	early := world.comment(t, map[string]any{"body": "A note its author withdrew before the cut: heron."})
	asOfWrite(t, handler, http.MethodDelete, "/api/v1/comments/"+early, nil, "alice")
	comment := world.comment(t, map[string]any{"body": "A note its author withdraws later: zebrafinch."})
	remark := decodeBody[model.Comment](t, asOfWrite(t, handler, http.MethodPost, "/api/v1/artifacts/"+handbook.ID+"/comments",
		map[string]any{"body": "A remark its author withdraws later: plover."}, "alice")).ID
	message := world.message(t, map[string]any{"body": "A message its author withdraws later: kingfisher."}, "alice").ID
	reply := world.message(t, map[string]any{"body": "Bob quotes it before it goes.", "in_reply_to": message}, "bob").ID
	edited := world.message(t, map[string]any{"body": "The draft as first written."}, "alice").ID
	asOfWrite(t, handler, http.MethodPatch, "/api/v1/messages/"+edited, map[string]any{"body": "The draft as edited before the cut."}, "alice")
	world.settle(t)

	logs := []struct{ route, target string }{
		{"GET /api/v1/issues/{key}/events", "/api/v1/issues/" + world.issue + "/events"},
		{"GET /api/v1/artifacts/{id}/events", "/api/v1/artifacts/" + handbook.ID + "/events"},
		{"GET /api/v1/events/log", "/api/v1/events/log?after=0"},
	}
	then := make([]asOfAnswer, len(logs))
	for index, log := range logs {
		if then[index] = documentRead(t, handler, log.target); then[index].status != http.StatusOK {
			t.Fatalf("GET %s at the cut: status=%d body=%s", log.target, then[index].status, then[index].body)
		}
	}
	cut := takeCut()
	time.Sleep(5 * time.Millisecond)

	for _, deleted := range []string{"/api/v1/comments/" + comment, "/api/v1/comments/" + remark, "/api/v1/messages/" + message} {
		asOfWrite(t, handler, http.MethodDelete, deleted, nil, "alice")
	}
	asOfWrite(t, handler, http.MethodPatch, "/api/v1/messages/"+edited, map[string]any{"body": "The draft as edited after the cut."}, "alice")
	world.settle(t)
	posts := &asOfWorld{deleted: map[string]string{comment: "zebrafinch", remark: "plover", message: "kingfisher"}}

	for index, log := range logs {
		t.Run(log.route, func(t *testing.T) {
			if present := documentRead(t, handler, log.target); bytes.Equal(present.body, then[index].body) {
				t.Fatalf("GET %s reads at the present what it read at the cut: the deletes after the cut moved nothing it reads", log.target)
			}
			asOf := documentRead(t, handler, withAsOf(t, log.target, cut))
			comparison := asOfComparison{route: log.route, cut: cut, world: posts}
			for _, problem := range comparison.problems(then[index], asOf) {
				t.Error(problem)
			}
			if named := asOf.header.Get(contracts.DispatchAsOfAbsentHeader); named != "/*/payload/body" {
				t.Errorf("%s = %q, want /*/payload/body alone: the page holds the events of a post deleted after the cut and no ask",
					contracts.DispatchAsOfAbsentHeader, named)
			}
			for id, word := range posts.deleted {
				for _, payload := range loggedPayloads(t, asOf.body, id) {
					deletedAt, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(payload["deleted_at"]))
					if _, holds := payload["body"]; holds || !deletedAt.After(cut.Time()) {
						t.Errorf("an event of post %s (%s), deleted after the cut, reads %v as of the cut, want no body and the delete after the cut",
							id, word, payload)
					}
				}
			}
		})
	}

	issueLog := then[0].body
	asOf := documentRead(t, handler, withAsOf(t, logs[0].target, cut))
	quoted := loggedPayloads(t, asOf.body, reply)
	if len(quoted) != 1 || !strings.Contains(fmt.Sprint(quoted[0]["reply_body"]), "kingfisher") {
		t.Errorf("the reply's events as of the cut are %v, want one that keeps its preview of the message it quoted", quoted)
	}
	present := documentRead(t, handler, logs[0].target)
	for index, payload := range loggedPayloads(t, asOf.body, edited) {
		atTheCut, now := loggedPayloads(t, issueLog, edited)[index], loggedPayloads(t, present.body, edited)[index]
		if payload["body"] != atTheCut["body"] || payload["edited_at"] != atTheCut["edited_at"] {
			t.Errorf("event %d of the edited message reads body %v edited at %v as of the cut, want %v edited at %v, as written then",
				index, payload["body"], payload["edited_at"], atTheCut["body"], atTheCut["edited_at"])
		}
		if now["edited_at"] == atTheCut["edited_at"] {
			t.Errorf("event %d of the edited message carries the edit mark %v at the present too: the edit after the cut moved nothing", index, now["edited_at"])
		}
	}

	// A page that holds the events of the comment deleted before the cut and of the edited message,
	// and of no post deleted after the cut, names nothing absent.
	var held []string
	for _, id := range []string{early, edited} {
		var events []model.Event
		if err := json.Unmarshal(issueLog, &events); err != nil {
			t.Fatalf("decode the issue's log: %v", err)
		}
		for _, event := range events {
			if payload, _ := event.Payload.(map[string]any); payload != nil && payload["id"] == id {
				held = append(held, strconv.FormatInt(event.ID, 10))
			}
		}
	}
	page := documentRead(t, handler, withAsOf(t, logs[0].target+"?ids="+strings.Join(held, ","), cut))
	if page.status != http.StatusOK || page.header.Get(contracts.DispatchAsOfHeader) != cut.String() {
		t.Fatalf("the page of events %v as of the cut: status=%d %s=%q body=%s", held, page.status,
			contracts.DispatchAsOfHeader, page.header.Get(contracts.DispatchAsOfHeader), abbreviateAnswer(page.body))
	}
	if named := page.header.Get(contracts.DispatchAsOfAbsentHeader); named != "" {
		t.Errorf("the page of events %v names %s %q, want nothing: no post of it was deleted after the cut", held, contracts.DispatchAsOfAbsentHeader, named)
	}
	// The comment's own events give no body; its comment.deleted, as written, an empty one.
	for _, payload := range loggedPayloads(t, page.body, early) {
		deletedAt, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(payload["deleted_at"]))
		if body, holds := payload["body"]; (holds && body != "") || deletedAt.IsZero() || deletedAt.After(cut.Time()) {
			t.Errorf("an event of the comment deleted before the cut reads %v as of the cut, want no body and its delete before the cut", payload)
		}
	}
}

// TestAPastTimeEventLogPageStopsAtTheCut: a page of the whole log at a cut gives the events at or
// before the cut and none after it, and ends its id range at the newest event id at the cut
// (decision 13). Paged two events at a time from the start, the log at the cut is the events whose
// transaction began at or before the cut, in id order, and the page after the newest of them is
// empty, though the log holds later events. Each page's read stops at that id: without the bound
// the page that reaches the newest event at the cut reads each event written after it (1,367 ms
// against 3 ms on a copy of production's log). An issue's log, paged newest first, starts at the
// issue's newest event at the cut.
func TestAPastTimeEventLogPageStopsAtTheCut(t *testing.T) {
	statements := &statementCounter{}
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{tracer: statements})
	asOfWrite(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "ASDL", "name": "The log at a cut"}, "alice")
	issue := asOfIssue(t, handler, map[string]any{"project": "ASDL", "title": "A log read at a cut"}).Key
	comment := func(body string) {
		asOfWrite(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/comments", map[string]any{"body": body}, "alice")
	}
	for index := range 3 {
		comment(fmt.Sprintf("Written before the cut, %d.", index))
	}
	settleServed(t, served)
	cut := databaseCut(t, database)
	time.Sleep(5 * time.Millisecond)
	for index := range 3 {
		comment(fmt.Sprintf("Written after the cut, %d.", index))
	}
	settleServed(t, served)

	ctx := t.Context()
	var atTheCut []int64
	rows, err := database.Pool.Query(ctx, `select id from events where created_at <= $1 order by id`, cut.Time())
	if err != nil {
		t.Fatalf("read the events at the cut: %v", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("read the events at the cut: %v", err)
		}
		atTheCut = append(atTheCut, id)
	}
	if rows.Close(); rows.Err() != nil {
		t.Fatalf("read the events at the cut: %v", rows.Err())
	}
	var later int
	if err := database.Pool.QueryRow(ctx, `select count(*) from events where created_at > $1`, cut.Time()).Scan(&later); err != nil || later == 0 || len(atTheCut) < 3 {
		t.Fatalf("the log holds %d events at the cut and %d after it (%v), want both", len(atTheCut), later, err)
	}
	newest := slices.Max(atTheCut)

	var paged []int64
	after := int64(0)
	for range len(atTheCut) + 2 {
		since := statements.mark()
		target := "/api/v1/events/log?after=" + strconv.FormatInt(after, 10) + "&limit=2"
		page := documentRead(t, handler, withAsOf(t, target, cut))
		if page.status != http.StatusOK || page.header.Get(contracts.DispatchAsOfHeader) != cut.String() {
			t.Fatalf("GET %s as of the cut: status=%d %s=%q body=%s", target, page.status,
				contracts.DispatchAsOfHeader, page.header.Get(contracts.DispatchAsOfHeader), abbreviateAnswer(page.body))
		}
		if bounds := pageBounds(statements, since); !slices.Equal(bounds, []string{strconv.FormatInt(newest, 10)}) {
			t.Errorf("GET %s as of the cut read its page up to the ids %v, want the newest event id at the cut, %d", target, bounds, newest)
		}
		ids := loggedEventIDs(t, page.body)
		if len(ids) == 0 {
			break
		}
		paged = append(paged, ids...)
		after = ids[len(ids)-1]
	}
	if !slices.Equal(paged, atTheCut) {
		t.Errorf("the pages of the log as of the cut hold the events %v, want the events at the cut, %v", paged, atTheCut)
	}
	beyond := "/api/v1/events/log?after=" + strconv.FormatInt(newest, 10)
	if ids := loggedEventIDs(t, documentRead(t, handler, beyond).body); len(ids) == 0 {
		t.Errorf("GET %s at the present holds no event, want the events written after the cut", beyond)
	}
	if ids := loggedEventIDs(t, documentRead(t, handler, withAsOf(t, beyond, cut)).body); len(ids) != 0 {
		t.Errorf("GET %s as of the cut holds the events %v, want none: none is at or before the cut", beyond, ids)
	}

	var issueAtTheCut []int64
	if err := database.Pool.QueryRow(ctx, `select array_agg(id order by seq desc) from events where issue_key = $1 and created_at <= $2`,
		issue, cut.Time()).Scan(&issueAtTheCut); err != nil {
		t.Fatalf("read the issue's events at the cut: %v", err)
	}
	newestFirst := "/api/v1/issues/" + issue + "/events?order=desc&limit=2"
	if ids := loggedEventIDs(t, documentRead(t, handler, withAsOf(t, newestFirst, cut)).body); !slices.Equal(ids, issueAtTheCut[:2]) {
		t.Errorf("GET %s as of the cut holds the events %v, want the issue's two newest at the cut, %v", newestFirst, ids, issueAtTheCut[:2])
	}
}

// TestAPastTimeEventLogPageUnderTheFallbackHorizonPool: where the event horizon's own pool does
// not open, the horizon observes through the shared request pool (server.horizon), which binds
// each statement it sends under a cut. The page's wait for the horizon reads no table as it stood
// at the cut, so it waits under no cut: the page at a cut is answered, and the observation that it
// shares with the readers at the present is not refused.
func TestAPastTimeEventLogPageUnderTheFallbackHorizonPool(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	served.horizonOnce.Do(func() { served.eventHorizon = events.NewHorizon(database.Pool) })
	asOfWrite(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "ASDH", "name": "The log on the shared pool"}, "alice")
	settleServed(t, served)
	cut := databaseCut(t, database)
	asOfWrite(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "ASDI", "name": "Written after the cut"}, "alice")

	page := documentRead(t, handler, withAsOf(t, "/api/v1/events/log?after=0", cut))
	if page.status != http.StatusOK || page.header.Get(contracts.DispatchAsOfHeader) != cut.String() {
		t.Fatalf("GET /api/v1/events/log as of the cut on the shared pool: status=%d %s=%q body=%s", page.status,
			contracts.DispatchAsOfHeader, page.header.Get(contracts.DispatchAsOfHeader), abbreviateAnswer(page.body))
	}
	if ids := loggedEventIDs(t, page.body); len(ids) == 0 {
		t.Errorf("GET /api/v1/events/log as of the cut on the shared pool holds no event, want the events at the cut")
	}
}

// pageBounds is the upper bound of the id range of each read of a page of the whole log
// (readEventsAfterID) that statements recorded since mark. The store sends a statement that it
// bound to a cut with pgx.QueryExecModeCacheDescribe in front of its arguments, which the tracer
// records with them.
func pageBounds(statements *statementCounter, since int) []string {
	statements.mu.Lock()
	defer statements.mu.Unlock()
	var bounds []string
	for _, statement := range statements.sent[since:] {
		args := statement.args
		if len(args) > 0 {
			if _, mode := args[0].(pgx.QueryExecMode); mode {
				args = args[1:]
			}
		}
		if strings.Contains(statement.sql, "e.id > $1 and e.id <= $2") && len(args) > 1 {
			bounds = append(bounds, fmt.Sprint(args[1]))
		}
	}
	return bounds
}

// TestAnIssueLogETagAtACutIsNotThePresentsETag: an issue's log at a cut goes out under an ETag
// that names its cut (decision 13). With no write after the cut, the log at the cut holds the
// bytes of the present log, and still its ETag is not the present's: a reader that holds the
// present's validator is answered the log at the cut whole, with its cut named, and never 304.
// The ETag at the cut answers 304 at that cut, at no other cut and never at the present. Once a
// comment is written after the cut, the present log holds it and the log at the cut does not:
// neither is answered from what the other kept.
func TestAnIssueLogETagAtACutIsNotThePresentsETag(t *testing.T) {
	world, takeCut := newAsOfThreadWorld(t, "ASDT")
	world.comment(t, map[string]any{"body": "Written before the cut."})
	target := "/api/v1/issues/" + world.issue + "/events"
	world.settle(t)
	thenRead := listRead(t, world.handler, target, listReadOptions{})
	present := listETagOf(t, thenRead, "the issue's log at the cut")
	then := thenRead.Body.Bytes()
	cut := takeCut()
	time.Sleep(5 * time.Millisecond)

	read := func(what, target, etag string) (int, string, []byte) {
		t.Helper()
		response := listRead(t, world.handler, target, listReadOptions{etag: etag})
		if strings.Contains(target, "as_of=") && response.Header().Get(contracts.DispatchAsOfHeader) == "" {
			t.Errorf("%s answered %d with no %s: the route answers the present", what, response.Code, contracts.DispatchAsOfHeader)
		}
		return response.Code, response.Header().Get("ETag"), response.Body.Bytes()
	}
	status, atTheCut, body := read("the log as of the cut, holding the present's ETag", withAsOf(t, target, cut), present)
	if status != http.StatusOK || !bytes.Equal(body, then) {
		t.Fatalf("the log as of the cut, holding the present's ETag: status=%d body=%s, want 200 and the log read at the cut", status, abbreviateAnswer(body))
	}
	if atTheCut == "" || atTheCut == present {
		t.Fatalf("the log as of the cut goes out under the ETag %q, and the present log under %q: want an ETag of its own", atTheCut, present)
	}
	if status, _, _ := read("the log as of the cut, holding its ETag", withAsOf(t, target, cut), atTheCut); status != http.StatusNotModified {
		t.Errorf("the log as of the cut, holding its own ETag: status=%d, want 304", status)
	}
	later := takeCut()
	if status, etag, _ := read("the log as of a later cut, holding the first cut's ETag", withAsOf(t, target, later), atTheCut); status != http.StatusOK || etag == atTheCut {
		t.Errorf("the log as of a later cut, holding the first cut's ETag: status=%d ETag=%q, want 200 under an ETag of its own", status, etag)
	}

	world.comment(t, map[string]any{"body": "Written after the cut."})
	world.settle(t)
	if status, etag, body := read("the present log, holding the ETag at the cut", target, atTheCut); status != http.StatusOK || etag == atTheCut || bytes.Equal(body, then) {
		t.Errorf("the present log, holding the ETag at the cut: status=%d ETag=%q, want 200 holding the comment written after the cut under the present's ETag", status, etag)
	}
	if status, _, _ := read("the present log, holding the present's ETag at the cut", target, present); status != http.StatusOK {
		t.Errorf("the present log, holding the ETag the present had at the cut: status=%d, want 200: a comment moved it", status)
	}
	if status, etag, body := read("the log as of the cut after a later write", withAsOf(t, target, cut), ""); status != http.StatusOK || etag != atTheCut || !bytes.Equal(body, then) {
		t.Errorf("the log as of the cut after a later write: status=%d ETag=%q body=%s, want 200 under %q holding the log read at the cut",
			status, etag, abbreviateAnswer(body), atTheCut)
	}
}
