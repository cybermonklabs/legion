package api

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// --- the reader rule table -------------------------------------------------------------------

// proseDryRunRoutes names every route whose request carries prose that the writing check does
// not read, with the reason it does not. The route guard
// (TestEveryRouteThatTakesProseReachesTheWritingCheck), the reader rule guard
// (TestEveryProseRouteNamesItsReaderRule) and the write path's cases
// (TestTheWritingCheckRecordsAndWritesOnEveryProseRoute) read this one list, so a route it names
// is exempt from each of them.
var proseDryRunRoutes = map[string]string{
	"POST /api/v1/issues/{key}/results/dry-run": "a result's dry run takes the record's body and writes nothing, so it has no write for the check to read and no row to keep; the record (POST /api/v1/issues/{key}/results) reads the same words when it is made",
}

// proseRoutesWithNoReaderRule is every route of shapes whose request carries prose, that the
// check reads (proseDryRunRoutes) and that rules gives no reader rule: the routes
// TestEveryProseRouteNamesItsReaderRule fails on.
func proseRoutesWithNoReaderRule(shapes map[string]routeShape, rules map[string]readerRule) []string {
	var missing []string
	for _, name := range slices.Sorted(maps.Keys(shapes)) {
		_, ruled := rules[name]
		_, unread := proseDryRunRoutes[name]
		if requestCarriesProse(shapes[name].Request) && !ruled && !unread {
			missing = append(missing, name)
		}
	}
	return missing
}

// TestEveryProseRouteNamesItsReaderRule holds writingCheckReaderRules to the registry, as the
// route guards hold the check to it: every route whose request carries prose names the rule
// that decides its reader, but for a route the check does not read (proseDryRunRoutes), which
// names none; the table names no route that carries none; and the send to an agent and the
// broadcast, and those alone, are read by no person.
func TestEveryProseRouteNamesItsReaderRule(t *testing.T) {
	for _, name := range proseRoutesWithNoReaderRule(routeShapes, writingCheckReaderRules) {
		t.Errorf("%s carries prose and names no reader rule: give it one in writingCheckReaderRules", name)
	}
	for _, name := range slices.Sorted(maps.Keys(proseDryRunRoutes)) {
		shape, shaped := routeShapes[name]
		_, ruled := writingCheckReaderRules[name]
		switch {
		case !shaped:
			t.Errorf("proseDryRunRoutes names %s, which is no route", name)
		case !requestCarriesProse(shape.Request):
			t.Errorf("proseDryRunRoutes names %s, whose request carries no prose", name)
		case ruled:
			t.Errorf("writingCheckReaderRules names %s, which the check does not read (%s)", name, proseDryRunRoutes[name])
		default:
			t.Logf("%s: not read, %s", name, proseDryRunRoutes[name])
		}
	}
	for _, name := range slices.Sorted(maps.Keys(writingCheckReaderRules)) {
		shape, shaped := routeShapes[name]
		switch {
		case !shaped:
			t.Errorf("writingCheckReaderRules names %s, which is no route", name)
		case !requestCarriesProse(shape.Request):
			t.Errorf("writingCheckReaderRules names %s, whose request carries no prose", name)
		default:
			t.Logf("%s: %s", name, writingCheckReaderRules[name])
		}
	}
	noReader := []string{"POST /api/v1/agents/{session_id}/messages", "POST /api/v1/broadcasts"}
	for name, rule := range writingCheckReaderRules {
		if want := slices.Contains(noReader, name); (rule == readerNone) != want {
			t.Errorf("%s has reader rule %s; only the send to an agent and the broadcast are read by no person", name, rule)
		}
	}
}

// TestTheReaderRuleGuardFindsAProseRouteWithNoRule is the guard's negative control: a prose route
// the table does not name is found, and neither a route that carries no prose nor one the check
// does not read (proseDryRunRoutes) is.
func TestTheReaderRuleGuardFindsAProseRouteWithNoRule(t *testing.T) {
	const dryRun = "POST /api/v1/issues/{key}/results/dry-run"
	shapes := map[string]routeShape{
		"POST /api/v1/issues/{key}/comments": routeShapes["POST /api/v1/issues/{key}/comments"],
		dryRun:                               routeShapes[dryRun],
		"POST /api/v1/unruled/comments":      {Request: commentInput{}},
		"POST /api/v1/unruled/handoffs":      {Request: createHandoffInput{}},
	}
	if got := proseRoutesWithNoReaderRule(shapes, writingCheckReaderRules); !slices.Equal(got, []string{"POST /api/v1/unruled/comments"}) {
		t.Errorf("routes with no reader rule = %q, want the unruled prose route alone", got)
	}
}

// --- the reader of each kind of text -----------------------------------------------------------

// exec runs one statement on the rig's database: the reader tests set the record a rule reads.
func (rig *writingCheckRig) exec(statement string, args ...any) {
	rig.t.Helper()
	if _, err := rig.database.Pool.Exec(context.Background(), statement, args...); err != nil {
		rig.t.Fatalf("%s: %v", statement, err)
	}
}

// unassign leaves the rig's issue with no assignee.
func (rig *writingCheckRig) unassign() {
	rig.exec(`update issues set assignee = null where key = $1`, rig.fixture.issue)
}

// noProjectDefault leaves the rig's project with no default assignee. Every project names one
// (20261111210103), so the test takes the column's not-null off in its own database to build
// the record where nobody holds the text: the rule then names no person.
func (rig *writingCheckRig) noProjectDefault() {
	rig.exec(`alter table projects alter column default_assignee drop not null`)
	rig.exec(`update projects set default_assignee = null where key = 'WCK'`)
}

// wantRead writes badProse through call and holds it to a write the check read, recorded findings
// with reader. The check refuses no write, and a write the route refuses records nothing
// (recordTakenWrites), so the write must take place: the rig builds the state each write needs.
func (rig *writingCheckRig) wantRead(what, reader string, call func() int) {
	rig.t.Helper()
	if !rig.wantTaken(what, call) {
		return
	}
	if event := rig.lastEvent(what); event.Outcome != writingCheckFindings || event.Reader != reader {
		rig.t.Errorf("%s: event = %+v, want %s with reader %s", what, event, writingCheckFindings, reader)
	}
}

// wantUnread writes badProse through call and holds the write to one the check did not read,
// recorded exempt_no_reader with no reader.
func (rig *writingCheckRig) wantUnread(what string, call func() int) {
	rig.t.Helper()
	if !rig.wantTaken(what, call) {
		return
	}
	if event := rig.lastEvent(what); event.Outcome != writingCheckNoReader || event.Reader != "" {
		rig.t.Errorf("%s: event = %+v, want %s with no reader", what, event, writingCheckNoReader)
	}
}

// wantTaken runs call on a cleared record and reports whether the route took the write, the one
// kind of write the check's decision is recorded for.
func (rig *writingCheckRig) wantTaken(what string, call func() int) bool {
	rig.t.Helper()
	rig.clearEvents()
	if code := call(); code < 200 || code >= 300 {
		rig.t.Errorf("%s: %d, want the write taken, whose decision alone is recorded", what, code)
		return false
	}
	return true
}

// session writes body as the agent session checkWriter and answers the status.
func (rig *writingCheckRig) session(method, target string, body map[string]any) func() int {
	return func() int { return rig.asSession(method, target, body).Code }
}

// TestTheWritingCheckReaderOfAnAsk: an ask's question, its options and an approval request's
// summary are read by the person the ask waits on - the person it was handed to, else the issue's
// assignee, else the project's default assignee.
func TestTheWritingCheckReaderOfAnAsk(t *testing.T) {
	rig := newWritingCheckRig(t, writingCheckOn())
	rig.exec(`update projects set default_assignee = 'dana' where key = 'WCK'`)
	rig.exec(`update asks set asked_of = 'bob' where id = $1`, rig.fixture.askEdit)
	asks := "/api/v1/issues/" + rig.fixture.issue + "/asks"
	approval := "/api/v1/artifacts/" + rig.fixture.notesID + "/approval-requests"
	question := map[string]any{"question": badProse}
	options := map[string]any{"question": "Which port should the server use?", "options": []map[string]any{{"label": badProse}}}

	rig.wantRead("an ask handed to bob", "bob", rig.session(http.MethodPatch, "/api/v1/asks/"+rig.fixture.askEdit, question))
	rig.wantRead("an ask with no chosen person", "alice", rig.session(http.MethodPatch, "/api/v1/asks/"+rig.fixture.askAnswer, question))
	rig.wantRead("a new ask on an assigned issue", "alice", rig.session(http.MethodPost, asks, question))
	rig.wantRead("a new ask's options", "alice", rig.session(http.MethodPost, asks, options))
	rig.wantRead("an approval request on an assigned issue's document", "alice", rig.session(http.MethodPost, approval, map[string]any{"summary": badProse}))

	rig.unassign()
	rig.wantRead("an ask handed to bob on an unassigned issue", "bob", rig.session(http.MethodPatch, "/api/v1/asks/"+rig.fixture.askEdit, question))
	rig.wantRead("an ask on an unassigned issue", "dana", rig.session(http.MethodPatch, "/api/v1/asks/"+rig.fixture.askAnswer, question))
	rig.wantRead("a new ask on an unassigned issue", "dana", rig.session(http.MethodPost, asks, question))
	rig.wantRead("an approval request on an unassigned issue's document", "dana", rig.session(http.MethodPost, approval, map[string]any{"summary": badProse}))
}

// TestTheWritingCheckReaderOfACommentOnAnIssue: a comment is read by the issue's assignee, else
// the project's default assignee, and by nobody where the record names neither; a reply to a
// person's comment is read by that person whatever the issue names.
func TestTheWritingCheckReaderOfACommentOnAnIssue(t *testing.T) {
	rig := newWritingCheckRig(t, writingCheckOn())
	rig.exec(`update projects set default_assignee = 'dana' where key = 'WCK'`)
	comments := "/api/v1/issues/" + rig.fixture.issue + "/comments"
	comment := map[string]any{"body": badProse}
	replyToAlice := map[string]any{"body": badProse, "reply_to": rig.fixture.comment}
	rig.clearEvents()
	posted := rig.asSessionNamed("s-other", http.MethodPost, comments, map[string]any{"body": cleanProse})
	if posted.Code != http.StatusCreated {
		t.Fatalf("a session's comment: %d %s", posted.Code, posted.Body.String())
	}
	replyToSession := map[string]any{"body": badProse, "reply_to": decodeBody[struct {
		ID string `json:"id"`
	}](t, posted).ID}

	rig.wantRead("a comment on an assigned issue", "alice", rig.session(http.MethodPost, comments, comment))
	rig.wantRead("a reply to another session's comment on an assigned issue", "alice", rig.session(http.MethodPost, comments, replyToSession))

	rig.unassign()
	rig.wantRead("a comment on an unassigned issue", "dana", rig.session(http.MethodPost, comments, comment))

	rig.noProjectDefault()
	rig.wantUnread("a comment on an issue whose record names nobody", rig.session(http.MethodPost, comments, comment))
	rig.wantUnread("a reply to another session's comment where nobody is named", rig.session(http.MethodPost, comments, replyToSession))
	rig.wantRead("a reply to alice's comment where nobody is named", "alice", rig.session(http.MethodPost, comments, replyToAlice))
	rig.wantRead("a session's reply to the delivery of alice's comment", "alice",
		rig.session(http.MethodPost, "/api/v1/comments/"+rig.fixture.comment+"/reply", map[string]any{"body": badProse, "attempt": 1}))
	rig.wantRead("a message answering alice's message where nobody is named", "alice",
		rig.session(http.MethodPost, "/api/v1/issues/"+rig.fixture.issue+"/messages", map[string]any{"body": badProse, "in_reply_to": rig.fixture.message}))
	rig.wantUnread("a message on an issue whose record names nobody",
		rig.session(http.MethodPost, "/api/v1/issues/"+rig.fixture.issue+"/messages", map[string]any{"body": badProse}))
}

// insertID runs one insert that returns an id on the rig's database and answers the id: the
// reader tests write a text on an issue's document as the store holds it, with the document's
// id and no issue_key.
func (rig *writingCheckRig) insertID(statement string, args ...any) string {
	rig.t.Helper()
	var id string
	if err := rig.database.Pool.QueryRow(context.Background(), statement, args...).Scan(&id); err != nil {
		rig.t.Fatalf("%s: %v", statement, err)
	}
	return id
}

// wantReader holds the reader the check decides for a write of route, as checkWriter, on the
// record the path values name, with input the write's decoded body, to reader. It reads the rule
// itself (writingCheckReader) rather than a record: it is for a text every route refuses a write
// on, whose decision is recorded nowhere (recordTakenWrites).
func (rig *writingCheckRig) wantReader(what, reader, route string, values map[string]string, input any) {
	rig.t.Helper()
	method, _, _ := strings.Cut(route, " ")
	request := jsonRequest(rig.t, method, "/", nil, values)
	got, err := rig.served.writingCheckReader(context.Background(), request, route, model.Actor{Kind: "session", ID: checkWriter}, input)
	if err != nil || got != reader {
		rig.t.Errorf("%s: the reader is %q (%v), want %s", what, got, err, reader)
	}
}

// TestTheWritingCheckReaderOfTextOnAnIssuesDocument: a comment or an ask on an issue's document
// names the document and no issue, and is read by that issue's assignee, not by the project's
// default; a reply to a person's comment on the document is read by that person. Every route
// refuses a write on such a text, since the owner check takes a document only where no issue
// holds it (unlinkedArtifactRead), so the rule is read for each route directly (wantReader).
func TestTheWritingCheckReaderOfTextOnAnIssuesDocument(t *testing.T) {
	rig := newWritingCheckRig(t, writingCheckOn())
	rig.exec(`update projects set default_assignee = 'dana' where key = 'WCK'`)
	onDocument := func(author string) string {
		return rig.insertID(`insert into comments (artifact_id, author, body) values ($1::uuid, $2::jsonb, 'The cache is warm.') returning id::text`,
			rig.fixture.notesID, author)
	}
	agentComment := onDocument(`{"kind":"session","id":"s-other"}`)
	bobComment := onDocument(`{"kind":"user","id":"bob"}`)
	agentReplyToBob := rig.insertID(`insert into comments (artifact_id, author, body, answers, reply_to) values ($1::uuid, $2::jsonb, 'The queue is empty.', $3::uuid, $3::uuid) returning id::text`,
		rig.fixture.notesID, `{"kind":"session","id":"s-other"}`, bobComment)
	ask := rig.insertID(`insert into asks (artifact_id, author, question) values ($1::uuid, $2::jsonb, 'Which port should the server use?') returning id::text`,
		rig.fixture.notesID, `{"kind":"session","id":"s-other"}`)
	handed := rig.insertID(`insert into asks (artifact_id, author, question, asked_of) values ($1::uuid, $2::jsonb, 'Which disk should the server use?', 'bob') returning id::text`,
		rig.fixture.notesID, `{"kind":"session","id":"s-other"}`)
	on := func(id string) map[string]string { return map[string]string{"id": id} }
	answering := func(id string) commentInput { return commentInput{Body: badProse, ReplyTo: &id} }
	issue := map[string]string{"key": rig.fixture.issue}

	rig.wantReader("a reply to a session's comment on the document", "alice", "POST /api/v1/comments/{id}/reply", on(agentComment), nil)
	rig.wantReader("an edit of a session's comment on the document", "alice", "PATCH /api/v1/comments/{id}", on(agentComment), nil)
	rig.wantReader("a new comment answering a session's comment on the document", "alice",
		"POST /api/v1/issues/{key}/comments", issue, answering(agentComment))
	rig.wantReader("an edit of an ask on the document", "alice", "PATCH /api/v1/asks/{id}", on(ask), nil)
	rig.wantReader("an answer to a session's ask on the document", "alice", "POST /api/v1/asks/{id}/answer", on(ask), nil)
	rig.wantReader("an edit of an ask on the document handed to bob", "bob", "PATCH /api/v1/asks/{id}", on(handed), nil)

	rig.wantReader("a reply to bob's comment on the document", "bob", "POST /api/v1/comments/{id}/reply", on(bobComment), nil)
	rig.wantReader("a new comment answering bob's comment on the document", "bob",
		"POST /api/v1/issues/{key}/comments", issue, answering(bobComment))
	rig.wantReader("an edit of a session's reply to bob on the document", "bob", "PATCH /api/v1/comments/{id}", on(agentReplyToBob), nil)
}

// TestTheWritingCheckReaderOfADirectConversation: a session's reply in a person's direct
// conversation, and a file it puts there, are read by that person; a reply in a conversation a
// session opened to another session, and a file put where no person has a conversation, by
// nobody. The send to an agent and the broadcast are read by nobody.
func TestTheWritingCheckReaderOfADirectConversation(t *testing.T) {
	rig := newWritingCheckRig(t, writingCheckOn())
	reply := map[string]any{"body": badProse, "attempt": 1}
	fromBob := rig.openConversation("user", "bob", checkWriter)
	fromSession := rig.openConversation("session", "s-other", checkWriter)
	rig.deliverMessage(fromBob, checkWriter, 1)
	rig.deliverMessage(fromSession, checkWriter, 1)

	rig.wantRead("a direct message to bob", "bob", rig.session(http.MethodPost, "/api/v1/messages/"+fromBob+"/reply", reply))
	rig.wantUnread("a reply between two sessions", rig.session(http.MethodPost, "/api/v1/messages/"+fromSession+"/reply", reply))
	rig.wantRead("a file's summary in bob's conversation", "bob", func() int {
		return rig.multipartUpload("/api/v1/agents/"+checkWriter+"/artifacts", "trace.txt", "text/plain", "plain bytes", badProse, checkWriter).Code
	})
	rig.wantUnread("a file's summary where no person has a conversation", func() int {
		return rig.multipartUpload("/api/v1/agents/s-lonely/artifacts", "trace.txt", "text/plain", "plain bytes", badProse, "s-lonely").Code
	})
	rig.wantUnread("a send to an agent", func() int {
		return rig.asTokenFor(checkWriter, http.MethodPost, "/api/v1/agents/s-reader/messages", map[string]any{"body": badProse, "delivery": "btw"}).Code
	})
	rig.wantUnread("a broadcast", func() int {
		return rig.asTokenFor(checkWriter, http.MethodPost, "/api/v1/broadcasts",
			map[string]any{"body": badProse, "delivery": "btw", "session_ids": []string{"s-reader"}}).Code
	})
}

// TestTheWritingCheckReaderOfAProjectDocument: a project's document and the text on it are read
// by the project's default assignee, and by nobody where the project names none.
func TestTheWritingCheckReaderOfAProjectDocument(t *testing.T) {
	rig := newWritingCheckRig(t, writingCheckOn())
	rig.exec(`update projects set default_assignee = 'dana' where key = 'WCK'`)
	edit := map[string]any{"ops": []map[string]any{{"op": "replace", "find": "Seed paragraph here.", "with": badProse}}}
	// The second edit replaces what the first wrote, so it finds what it replaces.
	editAgain := map[string]any{"ops": []map[string]any{{"op": "replace", "find": badProse, "with": "We will spin up the queue."}}}
	edits := "/api/v1/projects/WCK/artifacts/" + rig.fixture.projectDocSlug + "/edits"
	comments := "/api/v1/artifacts/" + rig.fixture.projectDocID + "/comments"
	upload := map[string]any{"name": "runbook", "content": badProse}

	rig.wantRead("an edit of a project document", "dana", rig.session(http.MethodPost, edits, edit))
	rig.wantRead("a comment on a project document", "dana", rig.session(http.MethodPost, comments, map[string]any{"body": badProse}))
	rig.wantRead("a new project document", "dana", rig.session(http.MethodPost, "/api/v1/projects/WCK/artifacts", upload))

	rig.noProjectDefault()
	rig.wantUnread("an edit of a project document with no default assignee", rig.session(http.MethodPost, edits, editAgain))
	rig.wantUnread("a comment on a project document with no default assignee", rig.session(http.MethodPost, comments, map[string]any{"body": badProse}))
	// A new document of its own: the runbook above was written, and a version that adds no block
	// gives the check nothing to read.
	rig.wantUnread("a new project document with no default assignee", rig.session(http.MethodPost, "/api/v1/projects/WCK/artifacts",
		map[string]any{"name": "rota", "content": badProse}))
}

// TestTheWritingCheckReaderOfANewIssue: a new issue's title and spec are read by the person it
// goes to: the assignee the create names, else, for a session that acts for nobody, the
// project's default assignee.
func TestTheWritingCheckReaderOfANewIssue(t *testing.T) {
	rig := newWritingCheckRig(t, writingCheckOn())
	rig.exec(`update projects set default_assignee = 'dana' where key = 'WCK'`)
	rig.wantRead("a new issue naming bob", "bob", rig.session(http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "WCK", "title": "A worker for the queue", "spec": badProse, "assignee": "Bob"}))
	rig.wantRead("a new issue naming nobody", "dana", rig.session(http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "WCK", "title": badProse}))
}
