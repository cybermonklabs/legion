package api

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"testing"
)

// --- the reader rule table -------------------------------------------------------------------

// proseRoutesWithNoReaderRule is every route of shapes whose request carries prose and that rules
// gives no reader rule: the routes TestEveryProseRouteNamesItsReaderRule fails on.
func proseRoutesWithNoReaderRule(shapes map[string]routeShape, rules map[string]readerRule) []string {
	var missing []string
	for _, name := range slices.Sorted(maps.Keys(shapes)) {
		if _, ruled := rules[name]; requestCarriesProse(shapes[name].Request) && !ruled {
			missing = append(missing, name)
		}
	}
	return missing
}

// TestEveryProseRouteNamesItsReaderRule holds writingCheckReaderRules to the registry, as the
// route guards hold the check to it: every route whose request carries prose names the rule
// that decides its reader, the table names no route that carries none, and the send to an agent
// and the broadcast, and those alone, are read by no person.
func TestEveryProseRouteNamesItsReaderRule(t *testing.T) {
	for _, name := range proseRoutesWithNoReaderRule(routeShapes, writingCheckReaderRules) {
		t.Errorf("%s carries prose and names no reader rule: give it one in writingCheckReaderRules", name)
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
// the table does not name is found, and a route that carries no prose is not.
func TestTheReaderRuleGuardFindsAProseRouteWithNoRule(t *testing.T) {
	shapes := map[string]routeShape{
		"POST /api/v1/issues/{key}/comments": routeShapes["POST /api/v1/issues/{key}/comments"],
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

// wantRead writes badProse through call and holds the write to a refusal recorded with reader.
func (rig *writingCheckRig) wantRead(what, reader string, call func() int) {
	rig.t.Helper()
	rig.clearEvents()
	if code := call(); code != http.StatusUnprocessableEntity {
		rig.t.Errorf("%s: %d, want 422: %s reads it", what, code, reader)
		return
	}
	if event := rig.lastEvent(what); event.Outcome != "refused" || event.Reader != reader {
		rig.t.Errorf("%s: event = %+v, want refused with reader %s", what, event, reader)
	}
}

// wantUnread writes badProse through call and holds the write to one the check did not read,
// recorded exempt_no_reader with no reader.
func (rig *writingCheckRig) wantUnread(what string, call func() int) {
	rig.t.Helper()
	rig.clearEvents()
	if code := call(); code == http.StatusUnprocessableEntity {
		rig.t.Errorf("%s: 422, want it written: no person reads it", what)
		return
	}
	if event := rig.lastEvent(what); event.Outcome != writingCheckNoReader || event.Reader != "" {
		rig.t.Errorf("%s: event = %+v, want %s with no reader", what, event, writingCheckNoReader)
	}
}

// session writes body as the agent session checkWriter and answers the status.
func (rig *writingCheckRig) session(method, target string, body map[string]any) func() int {
	return func() int { return rig.asSession(method, target, body).Code }
}

// TestTheWritingCheckReaderOfAnAsk: an ask's question, its options and an approval request's
// summary are read by the person the ask waits on - the person it was handed to, else the issue's
// assignee, else the project's default assignee.
func TestTheWritingCheckReaderOfAnAsk(t *testing.T) {
	rig := newWritingCheckRig(t, writingCheckOn(nil))
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
	rig := newWritingCheckRig(t, writingCheckOn(nil))
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

// TestTheWritingCheckReaderOfADirectConversation: a session's reply in a person's direct
// conversation, and a file it puts there, are read by that person; a reply in a conversation a
// session opened to another session, and a file put where no person has a conversation, by
// nobody. The send to an agent and the broadcast are read by nobody.
func TestTheWritingCheckReaderOfADirectConversation(t *testing.T) {
	rig := newWritingCheckRig(t, writingCheckOn(nil))
	reply := map[string]any{"body": badProse, "attempt": 1}
	fromBob := rig.openConversation("user", "bob", checkWriter)
	fromSession := rig.openConversation("session", "s-other", checkWriter)

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
	rig := newWritingCheckRig(t, writingCheckOn(nil))
	rig.exec(`update projects set default_assignee = 'dana' where key = 'WCK'`)
	edit := map[string]any{"ops": []map[string]any{{"op": "replace", "find": "Seed paragraph here.", "with": badProse}}}
	edits := "/api/v1/projects/WCK/artifacts/" + rig.fixture.projectDocSlug + "/edits"
	comments := "/api/v1/artifacts/" + rig.fixture.projectDocID + "/comments"
	upload := map[string]any{"name": "runbook", "content": badProse}

	rig.wantRead("an edit of a project document", "dana", rig.session(http.MethodPost, edits, edit))
	rig.wantRead("a comment on a project document", "dana", rig.session(http.MethodPost, comments, map[string]any{"body": badProse}))
	rig.wantRead("a new project document", "dana", rig.session(http.MethodPost, "/api/v1/projects/WCK/artifacts", upload))

	rig.noProjectDefault()
	rig.wantUnread("an edit of a project document with no default assignee", rig.session(http.MethodPost, edits, edit))
	rig.wantUnread("a comment on a project document with no default assignee", rig.session(http.MethodPost, comments, map[string]any{"body": badProse}))
	rig.wantUnread("a new project document with no default assignee", rig.session(http.MethodPost, "/api/v1/projects/WCK/artifacts", upload))
}

// TestTheWritingCheckReaderOfANewIssue: a new issue's title and spec are read by the person it
// goes to: the assignee the create names, else, for a session that acts for nobody, the
// project's default assignee.
func TestTheWritingCheckReaderOfANewIssue(t *testing.T) {
	rig := newWritingCheckRig(t, writingCheckOn(nil))
	rig.exec(`update projects set default_assignee = 'dana' where key = 'WCK'`)
	rig.wantRead("a new issue naming bob", "bob", rig.session(http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "WCK", "title": "A worker for the queue", "spec": badProse, "assignee": "Bob"}))
	rig.wantRead("a new issue naming nobody", "dana", rig.session(http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "WCK", "title": badProse}))
}
