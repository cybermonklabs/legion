package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck"
)

// --- the registry ----------------------------------------------------------------------------

// unmarkedProseMembers is every member of request that holds text and that the registry marks
// neither prose nor not prose: the members TestTheProseRegistryMarksEveryRequestStringField fails
// on.
func unmarkedProseMembers(request reflect.Type) []string {
	var unmarked []string
	walkProseMembers(request, func(member proseMember) {
		if member.leaf && !member.marked {
			unmarked = append(unmarked, member.path)
		}
	})
	return unmarked
}

// TestTheProseRegistryMarksEveryRequestStringField holds the registry (writing_check_fields.go)
// to every request type a route of routeShapes takes: each member that holds text is marked prose
// or not prose with a reason, and no mark names a member its type does not have. The marks are
// logged route by route.
func TestTheProseRegistryMarksEveryRequestStringField(t *testing.T) {
	var unmarked, unexplained []string
	reached := map[reflect.Type]map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(routeShapes)) {
		request := routeShapes[name].Request
		if request == nil {
			continue
		}
		var prose []string
		walkProseMembers(reflect.TypeOf(request), func(member proseMember) {
			if reached[member.owner] == nil {
				reached[member.owner] = map[string]bool{}
			}
			reached[member.owner][member.name] = true
			switch {
			case !member.leaf:
			case !member.marked:
				unmarked = append(unmarked, name+": "+member.path)
			case member.mark.prose:
				prose = append(prose, member.path)
			case member.mark.why == "":
				unexplained = append(unexplained, name+": "+member.path)
			}
		})
		if len(prose) > 0 {
			t.Logf("%s: prose %s", name, strings.Join(prose, ", "))
		}
	}
	if len(unmarked) > 0 {
		t.Errorf("a request member holds text the writing check's registry does not mark:\n\t%s\n"+
			"Mark it in proseRegistry (writing_check_fields.go): prose, or not prose with the reason.",
			strings.Join(unmarked, "\n\t"))
	}
	if len(unexplained) > 0 {
		t.Errorf("a member is marked not prose with no reason:\n\t%s", strings.Join(unexplained, "\n\t"))
	}
	for owner, marks := range proseRegistry {
		for member := range marks {
			if !reached[owner][member] {
				t.Errorf("proseRegistry marks %s.%s, which no request of routeShapes carries: delete the line", owner, member)
			}
		}
	}
	// The multipart upload's form values are marked as the JSON upload's members are.
	jsonMarks := proseRegistry[reflect.TypeFor[jsonArtifactUpload]()]
	for member, mark := range jsonMarks {
		if form, named := artifactUploadFormValues[member]; !named || form.prose != mark.prose {
			t.Errorf("the upload's form value %q is marked %+v, its JSON member %+v", member, form, mark)
		}
	}
	for _, value := range []string{"content", "summary"} {
		if !artifactUploadFormValues[value].prose {
			t.Errorf("the upload's %s form value is not prose", value)
		}
	}
	for _, value := range []string{"actor", "name", "file"} {
		if mark, named := artifactUploadFormValues[value]; !named || mark.prose || mark.why == "" {
			t.Errorf("the upload's %s form value is %+v, want not prose with a reason", value, mark)
		}
	}
}

// TestTheProseRegistryFindsAnUnmarkedField is the negative control of the registry test: a
// request type with a string member the registry does not mark, nested and plain, is found.
func TestTheProseRegistryFindsAnUnmarkedField(t *testing.T) {
	type nested struct {
		Words string `json:"words"`
	}
	type probe struct {
		Plain  string            `json:"plain"`
		Nested []nested          `json:"nested"`
		Map    map[string]string `json:"map"`
		Raw    json.RawMessage   `json:"raw"`
		Any    any               `json:"any"`
		Count  int               `json:"count"`
		hidden string
	}
	got := unmarkedProseMembers(reflect.TypeFor[probe]())
	want := []string{"plain", "nested[].words", "map", "raw", "any"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("unmarked members = %v, want %v", got, want)
	}
	if unmarked := unmarkedProseMembers(reflect.TypeFor[commentInput]()); len(unmarked) != 0 {
		t.Errorf("commentInput has unmarked members %v", unmarked)
	}
}

// TestTheProseRegistryReadsEveryProseFieldOfARequest pins the reading: nested members, lists by
// index, a raw member through its shape, and nothing the registry marks not prose.
func TestTheProseRegistryReadsEveryProseFieldOfARequest(t *testing.T) {
	reason := "This issue has no part in the architecture."
	question := "Which port should the server use?"
	var ask createAskInput
	ask.Question = question
	ask.Options = []model.AskOption{{Label: "Port one", Description: "The old port."}, {Label: "Port two"}}
	quote := "a quote of the document"
	ask.Anchor = &model.AnchorInput{Artifact: "doc", Quote: &quote}
	got := proseFieldsOf(ask)
	want := []writingcheck.Field{
		{Name: "question", Text: question},
		{Name: "options[0].label", Text: "Port one"},
		{Name: "options[0].description", Text: "The old port."},
		{Name: "options[1].label", Text: "Port two"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ask fields = %+v, want %+v", got, want)
	}
	issue := createIssueInput{Title: "A title", Components: json.RawMessage(`{"mode":"none","reason":"` + reason + `"}`)}
	if got := proseFieldsOf(issue); !reflect.DeepEqual(got, []writingcheck.Field{
		{Name: "title", Text: "A title"}, {Name: "components.reason", Text: reason},
	}) {
		t.Errorf("issue fields = %+v", got)
	}
	edit := editArtifactInput{Ops: []model.EditOp{{Op: "replace", Find: "old words", With: "New words."}}}
	if got := proseFieldsOf(edit); !reflect.DeepEqual(got, []writingcheck.Field{{Name: "ops[0].with", Text: "New words."}}) {
		t.Errorf("edit fields = %+v, want the text the edit adds alone", got)
	}
}

// --- the actor rule --------------------------------------------------------------------------

func TestTheWritingCheckActorRule(t *testing.T) {
	for _, probe := range []struct {
		name      string
		actor     model.Actor
		checked   bool
		exemption string
	}{
		{"an agent session", model.Actor{Kind: "session", ID: "s-writer"}, true, ""},
		{"a project's daemon", model.Actor{Kind: "session", ID: "legion-daemon:WCK"}, false, writingcheck.ExemptDaemon},
		{"a person for themselves", model.Actor{Kind: "user", ID: "alice"}, false, writingcheck.ExemptPerson},
		{"a person's token with no session", model.Actor{Kind: "user", ID: "alice", Act: &model.ActorAct{TokenID: "t"}}, false, writingcheck.ExemptPerson},
		{"a person's token acting for a session", model.Actor{Kind: "user", ID: "alice", Act: &model.ActorAct{Session: "s-writer"}}, true, ""},
		{"a person's token acting for the daemon", model.Actor{Kind: "user", ID: "alice", Act: &model.ActorAct{Session: "legion-daemon:WCK"}}, false, writingcheck.ExemptDaemon},
		{"Dispatch itself", model.Actor{Kind: "system", ID: "dispatch"}, false, writingcheck.ExemptSystem},
	} {
		checked, exemption := writingCheckReads(probe.actor)
		if checked != probe.checked || exemption != probe.exemption {
			t.Errorf("%s: checked %t (%q), want %t (%q)", probe.name, checked, exemption, probe.checked, probe.exemption)
		}
	}
}

func TestTheWritingCheckReadsTheBlocksANewVersionAdds(t *testing.T) {
	previous := "# Plan\n\nThe first paragraph.\n\n- one item\n- two item\n\n| a | b |\n| - | - |\n| c | d |\n\n```\ncode; here\n\nstill code\n```\n"
	next := "# Plan\n\nThe first paragraph.\n\nA new paragraph.\n\n- one item\n- three item\n  - nested item\n\n| a | b |\n| - | - |\n| c | e |\n\n```\ncode; here\n\nstill code\n```\n"
	got := addedMarkdownBlocks(previous, next)
	want := "A new paragraph.\n\n- three item\n\n- nested item\n\nc\n\ne"
	if got != want {
		t.Errorf("added blocks = %q, want %q", got, want)
	}
	if got := addedMarkdownBlocks("", "One.\n\nTwo.\n"); got != "One.\n\nTwo." {
		t.Errorf("a first version = %q, want every block", got)
	}
}

// --- the write path --------------------------------------------------------------------------

// The text every case writes: badProse breaks the phrasal-verb rule (badRule), one of the rules
// the bundle enables, and cleanProse breaks none of them.
const (
	badProse    = "We will spin up the server."
	badRule     = "phrasal-verb"
	cleanProse  = "The build failed because the cache was empty."
	checkWriter = "s-writer"
	daemonID    = "legion-daemon:WCK"
)

// writingCheckOn is the check as cmd/dispatch runs it: on, with the bundle's structure rules.
func writingCheckOn() *WritingCheck {
	return NewWritingCheck()
}

type writingCheckRig struct {
	t        *testing.T
	handler  http.Handler
	database *store.Store
	// served is the rig's server, whose reader rule a test reads directly (wantReader).
	served *server
	// broken is the rig's document store, which hands the load breakNextLoad names a history
	// that does not decode.
	broken  *invalidLoadOnceStore
	token   agentTokenResponse
	fixture writingCheckFixture
}

type writingCheckFixture struct {
	issue, spec, notesID, notesSlug, editOneID, editTwoSlug, projectDocID, projectDocSlug string
	rebuildID                                                                             string
	askEdit, askAnswer, askResolve, askDecline                                            string
	message, comment, result                                                              string
}

// newWritingCheckRig builds the rig every write of the check's tests takes place on. A write the
// route refuses records nothing (recordTakenWrites), so the rig builds the state each write needs:
// a listener holding the session a send to an agent and a broadcast reach, a document store that
// can break a document for its rebuild, a message and a comment delivered to checkWriter for its
// replies, and a result checkWriter recorded for its retraction.
func newWritingCheckRig(t *testing.T, check *WritingCheck) *writingCheckRig {
	t.Helper()
	listenerURL, _ := newFakeReviewListener(t, "s-reader")
	var broken *invalidLoadOnceStore
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{
		writingCheck: check, writingCheckOff: check == nil, envoyURL: listenerURL,
		persistence: func(database *store.Store) docs.VersionedStore {
			broken = &invalidLoadOnceStore{VersionedStore: docs.NewPgVersioned(database), invalid: make(map[string]bool)}
			return broken
		},
	})
	rig := &writingCheckRig{t: t, handler: handler, database: database, served: served, broken: broken}
	rig.token = mintPersonalToken(t, handler, "alice", "writer", []string{model.ScopePerson, model.ScopeApprove})
	issue := createInteractionIssue(t, handler, "WCK", "Writing check issue", "Seed paragraph here.\n")
	rig.fixture.issue, rig.fixture.spec = issue.Key, issue.PrimaryArtifactID
	upload := func(target, name string) (string, string) {
		response := dispatchRequest(t, handler, http.MethodPost, target, map[string]any{"name": name, "content": "Seed paragraph here.\n"}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("upload %s: %d %s", name, response.Code, response.Body.String())
		}
		uploaded := decodeBody[artifactUploadResponse](t, response)
		return uploaded.Artifact.ID, uploaded.Artifact.Slug
	}
	rig.fixture.notesID, rig.fixture.notesSlug = upload("/api/v1/issues/"+issue.Key+"/artifacts", "notes")
	rig.fixture.editOneID, _ = upload("/api/v1/issues/"+issue.Key+"/artifacts", "edit one")
	_, rig.fixture.editTwoSlug = upload("/api/v1/issues/"+issue.Key+"/artifacts", "edit two")
	rig.fixture.rebuildID, _ = upload("/api/v1/issues/"+issue.Key+"/artifacts", "rebuilt")
	rig.fixture.projectDocID, rig.fixture.projectDocSlug = upload("/api/v1/projects/WCK/artifacts", "guide")
	for _, ask := range []*string{&rig.fixture.askEdit, &rig.fixture.askAnswer, &rig.fixture.askResolve, &rig.fixture.askDecline} {
		response := rig.asSession(http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{"question": "Which port should the server use?"})
		if response.Code != http.StatusCreated {
			t.Fatalf("create ask: %d %s", response.Code, response.Body.String())
		}
		*ask = decodeBody[model.Ask](t, response).ID
	}
	message := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{"body": "The queue is empty."}, "alice")
	if message.Code != http.StatusCreated {
		t.Fatalf("create message: %d %s", message.Code, message.Body.String())
	}
	rig.fixture.message = decodeBody[model.Message](t, message).ID
	rig.deliverMessage(rig.fixture.message, checkWriter, 1)
	comment := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{"body": "The cache is warm."}, "alice")
	if comment.Code != http.StatusCreated {
		t.Fatalf("create comment: %d %s", comment.Code, comment.Body.String())
	}
	rig.fixture.comment = decodeBody[model.Comment](t, comment).ID
	rig.deliverComment(rig.fixture.comment, checkWriter, 1)
	// A result checkWriter recorded on the issue, so its retraction is text on that issue and the
	// writer's to take back.
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	rig.fixture.result = recordResultAsSession(t, handler, issue.Key, checkWriter, resultBody("k1", "pass", headCommit, nil)).ID
	// alice's direct conversation with the agent session checkWriter, which makes her the person
	// who reads what that session puts into its conversation.
	rig.openConversation("user", "alice", checkWriter)
	rig.clearEvents()
	return rig
}

// deliverMessage records the message's attempt as sent to session, as the send leaves it, so
// session's reply to that attempt takes place.
func (rig *writingCheckRig) deliverMessage(messageID, session string, attempt int) {
	rig.t.Helper()
	rig.exec(`
		insert into message_deliveries (message_id, attempt, delivery, session_id, state)
		values ($1::uuid, $3, 'btw', $2, 'sent')`, messageID, session, attempt)
}

// deliverComment records the comment's mention of session, once, and the mention's attempt, sent
// to session, as the comment's create and its send leave them, so session's reply to that attempt
// takes place.
func (rig *writingCheckRig) deliverComment(commentID, session string, attempt int) {
	rig.t.Helper()
	rig.exec(`
		insert into comment_mentions (comment_id, target, delivery, resolved_session_id)
		values ($1::uuid, 'session:' || $2::text, 'steer', $2::text)
		on conflict (comment_id, target) do nothing`, commentID, session)
	rig.exec(`
		insert into comment_deliveries (comment_id, target, attempt, delivery, session_id, state, claimed_at)
		values ($1::uuid, 'session:' || $2::text, $3, 'steer', $2::text, 'sent', now())`, commentID, session, attempt)
}

// breakNextLoad leaves the document id out of every room and hands its next load a history that
// does not decode, the one state a rebuild takes place on.
func (rig *writingCheckRig) breakNextLoad(id string) {
	rig.t.Helper()
	if err := rig.served.deps.Docs.(*docs.Service).Evict(context.Background(), id); err != nil {
		rig.t.Fatalf("evict document %s: %v", id, err)
	}
	rig.broken.failNextLoad(id)
}

// openConversation opens a direct conversation to session, as the author kind and id write it,
// and answers its root message's id.
func (rig *writingCheckRig) openConversation(kind, id, session string) string {
	rig.t.Helper()
	var root string
	if err := rig.database.Pool.QueryRow(context.Background(), `
		insert into messages (author, body, target) values (jsonb_build_object('kind', $1::text, 'id', $2::text), 'Is the queue empty?', $3)
		returning id::text`, kind, id, "session:"+session).Scan(&root); err != nil {
		rig.t.Fatalf("open a direct conversation to %s: %v", session, err)
	}
	return root
}

// asSession writes as the agent session checkWriter, with the deployment's shared token.
func (rig *writingCheckRig) asSession(method, target string, body map[string]any) *httptest.ResponseRecorder {
	return rig.asSessionNamed(checkWriter, method, target, body)
}

func (rig *writingCheckRig) asSessionNamed(session, method, target string, body map[string]any) *httptest.ResponseRecorder {
	rig.t.Helper()
	withActor := maps.Clone(body)
	withActor["actor"] = map[string]any{"kind": "session", "id": session}
	return agentRequest(rig.t, rig.handler, method, target, withActor, "agent-token")
}

// asTokenFor writes with alice's personal token, acting for session through ?session=.
func (rig *writingCheckRig) asTokenFor(session, method, target string, body map[string]any) *httptest.ResponseRecorder {
	rig.t.Helper()
	separator := "?"
	if strings.Contains(target, "?") {
		separator = "&"
	}
	return agentRequest(rig.t, rig.handler, method, target+separator+"session="+session, body, rig.token.Token)
}

type writingCheckEventRow struct {
	Route, Method, ActorKind, ActorID, ActSession, TokenID, Outcome, Detail, Reader string
	Rules, Fields                                                                   []string
}

func (rig *writingCheckRig) events() []writingCheckEventRow {
	rig.t.Helper()
	rows, err := rig.database.Pool.Query(context.Background(), `
		select route, method, actor_kind, actor_id, coalesce(act_session, ''), coalesce(token_id, ''), outcome,
			coalesce(detail, ''), coalesce(reader, ''), rules, fields
		from writing_check_events order by id`)
	if err != nil {
		rig.t.Fatalf("read writing check events: %v", err)
	}
	defer rows.Close()
	var events []writingCheckEventRow
	for rows.Next() {
		var row writingCheckEventRow
		if err := rows.Scan(&row.Route, &row.Method, &row.ActorKind, &row.ActorID, &row.ActSession, &row.TokenID,
			&row.Outcome, &row.Detail, &row.Reader, &row.Rules, &row.Fields); err != nil {
			rig.t.Fatalf("scan a writing check event: %v", err)
		}
		events = append(events, row)
	}
	return events
}

func (rig *writingCheckRig) clearEvents() {
	rig.t.Helper()
	if _, err := rig.database.Pool.Exec(context.Background(), `delete from writing_check_events`); err != nil {
		rig.t.Fatalf("clear writing check events: %v", err)
	}
}

// lastEvent is the one event the write just made: the rig clears the table before each write.
func (rig *writingCheckRig) lastEvent(what string) writingCheckEventRow {
	rig.t.Helper()
	events := rig.events()
	if len(events) != 1 {
		rig.t.Fatalf("%s: %d writing check events %+v, want one", what, len(events), events)
	}
	return events[0]
}

// proseRouteCase is one route the check reads, with how a writer calls it and the field the
// text goes in.
type proseRouteCase struct {
	route string
	field string
	// call writes text through the route; wantStatus is the status a write answers, 0 for any
	// status (a second call that writes what the first wrote, such as a second answer to one
	// ask, is refused, and records nothing).
	call       func(rig *writingCheckRig, text string) *httptest.ResponseRecorder
	wantStatus int
}

func proseRouteCases() []proseRouteCase {
	session := func(method, path func(f writingCheckFixture) string, body func(text string) map[string]any) func(*writingCheckRig, string) *httptest.ResponseRecorder {
		return func(rig *writingCheckRig, text string) *httptest.ResponseRecorder {
			return rig.asSession(method(rig.fixture), path(rig.fixture), body(text))
		}
	}
	token := func(method string, path func(f writingCheckFixture) string, body func(text string) map[string]any) func(*writingCheckRig, string) *httptest.ResponseRecorder {
		return func(rig *writingCheckRig, text string) *httptest.ResponseRecorder {
			return rig.asTokenFor(checkWriter, method, path(rig.fixture), body(text))
		}
	}
	post := func(writingCheckFixture) string { return http.MethodPost }
	patch := func(writingCheckFixture) string { return http.MethodPatch }
	field := func(name string, extra ...any) func(string) map[string]any {
		return func(text string) map[string]any {
			body := map[string]any{name: text}
			for index := 0; index+1 < len(extra); index += 2 {
				body[extra[index].(string)] = extra[index+1]
			}
			return body
		}
	}
	// replace edits a document's text in place: the first call replaces the seed, and each call
	// after it the text the call before it wrote, so every call finds what it replaces.
	replace := func() func(text string) map[string]any {
		found := "Seed paragraph here."
		return func(text string) map[string]any {
			body := map[string]any{"ops": []map[string]any{{"op": "replace", "find": found, "with": text}}}
			found = text
			return body
		}
	}
	upload := func(path func(f writingCheckFixture) string) func(*writingCheckRig, string) *httptest.ResponseRecorder {
		names := 0
		return func(rig *writingCheckRig, text string) *httptest.ResponseRecorder {
			names++
			return rig.asSession(http.MethodPost, path(rig.fixture), map[string]any{"name": fmt.Sprintf("upload %d", names), "content": text})
		}
	}
	// reply answers a delivered attempt of the text id names on kind's reply route: the first call
	// answers the attempt the rig delivered, and each call after it a new attempt it delivers, so
	// every call is a reply that posts. A second reply to one attempt is its replay, which posts
	// nothing and records no row (TestTheWritingCheckRecordsNoRowForAReplyThatWritesNothing).
	reply := func(kind string, id func(f writingCheckFixture) string, deliver func(rig *writingCheckRig, id, session string, attempt int)) func(*writingCheckRig, string) *httptest.ResponseRecorder {
		attempt := 0
		return func(rig *writingCheckRig, text string) *httptest.ResponseRecorder {
			attempt++
			if attempt > 1 {
				deliver(rig, id(rig.fixture), checkWriter, attempt)
			}
			return rig.asSession(http.MethodPost, "/api/v1/"+kind+"/"+id(rig.fixture)+"/reply", map[string]any{"body": text, "attempt": attempt})
		}
	}
	return []proseRouteCase{
		{"POST /api/v1/issues", "title", session(post, func(writingCheckFixture) string { return "/api/v1/issues" }, field("title", "project", "WCK")), http.StatusCreated},
		{"PATCH /api/v1/issues/{key}", "title", session(patch, func(f writingCheckFixture) string { return "/api/v1/issues/" + f.issue }, field("title")), http.StatusOK},
		{"POST /api/v1/issues/{key}/artifacts", "content", upload(func(f writingCheckFixture) string { return "/api/v1/issues/" + f.issue + "/artifacts" }), http.StatusCreated},
		{"POST /api/v1/projects/{key}/artifacts", "content", upload(func(writingCheckFixture) string { return "/api/v1/projects/WCK/artifacts" }), http.StatusCreated},
		{"POST /api/v1/agents/{session_id}/artifacts", "summary", func(rig *writingCheckRig, text string) *httptest.ResponseRecorder {
			return rig.multipartUpload("/api/v1/agents/"+checkWriter+"/artifacts", "trace.txt", "text/plain", "plain bytes", text, checkWriter)
		}, http.StatusCreated},
		{"POST /api/v1/issues/{key}/results", "run", session(post, func(f writingCheckFixture) string { return "/api/v1/issues/" + f.issue + "/results" },
			field("run", "check", "k1", "role", "tester", "commit", strings.Repeat("a", 40), "verdict", "pass")), 0},
		{"POST /api/v1/issues/{key}/results/dry-run", "run", session(post, func(f writingCheckFixture) string { return "/api/v1/issues/" + f.issue + "/results/dry-run" },
			field("run", "check", "k1", "role", "tester", "commit", strings.Repeat("a", 40), "verdict", "pass")), http.StatusOK},
		{"POST /api/v1/results/{id}/retract", "reason", session(post, func(f writingCheckFixture) string {
			return "/api/v1/results/" + f.result + "/retract"
		}, field("reason")), 0},
		{"POST /api/v1/issues/{key}/messages", "body", session(post, func(f writingCheckFixture) string { return "/api/v1/issues/" + f.issue + "/messages" }, field("body")), http.StatusCreated},
		{"POST /api/v1/messages/{id}/reply", "body", reply("messages", func(f writingCheckFixture) string { return f.message }, (*writingCheckRig).deliverMessage), http.StatusCreated},
		{"POST /api/v1/agents/{session_id}/messages", "body", token(http.MethodPost, func(writingCheckFixture) string { return "/api/v1/agents/s-reader/messages" }, field("body", "delivery", "btw")), 0},
		{"POST /api/v1/broadcasts", "body", token(http.MethodPost, func(writingCheckFixture) string { return "/api/v1/broadcasts" }, field("body", "delivery", "btw", "session_ids", []string{"s-reader"})), 0},
		{"POST /api/v1/issues/{key}/asks", "question", session(post, func(f writingCheckFixture) string { return "/api/v1/issues/" + f.issue + "/asks" }, field("question")), http.StatusCreated},
		{"POST /api/v1/artifacts/{id}/asks", "question", session(post, func(f writingCheckFixture) string { return "/api/v1/artifacts/" + f.projectDocID + "/asks" }, field("question")), http.StatusCreated},
		{"PATCH /api/v1/asks/{id}", "question", session(patch, func(f writingCheckFixture) string { return "/api/v1/asks/" + f.askEdit }, field("question")), http.StatusOK},
		{"POST /api/v1/asks/{id}/answer", "text", token(http.MethodPost, func(f writingCheckFixture) string { return "/api/v1/asks/" + f.askAnswer + "/answer" }, field("text", "expected_edited_at", nil)), 0},
		{"POST /api/v1/asks/{id}/resolve", "reason", session(post, func(f writingCheckFixture) string { return "/api/v1/asks/" + f.askResolve + "/resolve" }, field("reason", "kind", "resolved")), 0},
		{"POST /api/v1/asks/{id}/decline", "note", token(http.MethodPost, func(f writingCheckFixture) string { return "/api/v1/asks/" + f.askDecline + "/decline" }, field("note")), 0},
		{"POST /api/v1/issues/{key}/comments", "body", session(post, func(f writingCheckFixture) string { return "/api/v1/issues/" + f.issue + "/comments" }, field("body")), http.StatusCreated},
		{"POST /api/v1/artifacts/{id}/comments", "body", session(post, func(f writingCheckFixture) string { return "/api/v1/artifacts/" + f.projectDocID + "/comments" }, field("body")), http.StatusCreated},
		{"POST /api/v1/comments/{id}/reply", "body", reply("comments", func(f writingCheckFixture) string { return f.comment }, (*writingCheckRig).deliverComment), http.StatusCreated},
		{"PATCH /api/v1/comments/{id}", "body", token(http.MethodPatch, func(f writingCheckFixture) string { return "/api/v1/comments/" + f.comment }, field("body")), 0},
		{"POST /api/v1/artifacts/{id}/rebuild", "markdown", func(rig *writingCheckRig, text string) *httptest.ResponseRecorder {
			rig.breakNextLoad(rig.fixture.rebuildID)
			return rig.asTokenFor(checkWriter, http.MethodPost, "/api/v1/artifacts/"+rig.fixture.rebuildID+"/rebuild", map[string]any{"markdown": text})
		}, 0},
		{"POST /api/v1/artifacts/{id}/reviews", "reason", token(http.MethodPost, func(f writingCheckFixture) string { return "/api/v1/artifacts/" + f.notesID + "/reviews" }, field("reason", "state", "changes_requested")), 0},
		// The versioned review names the version its writer read, which must be the version the
		// review records: the document's latest settled version, as no approval request is open on
		// it until the approval-requests case below opens one. A review writes no version, so both
		// calls name the same one.
		{"POST /api/v1/artifacts/{id}/versions/{number}/reviews", "reason", func(rig *writingCheckRig, text string) *httptest.ResponseRecorder {
			version, err := settledVersionNumber(context.Background(), rig.database.Pool, rig.fixture.notesID)
			if err != nil {
				rig.t.Fatalf("read the settled version of the notes document: %v", err)
			}
			return rig.asTokenFor(checkWriter, http.MethodPost, fmt.Sprintf("/api/v1/artifacts/%s/versions/%d/reviews", rig.fixture.notesID, version),
				map[string]any{"reason": text, "state": "changes_requested"})
		}, http.StatusCreated},
		{"POST /api/v1/artifacts/{id}/approval-requests", "summary", session(post, func(f writingCheckFixture) string { return "/api/v1/artifacts/" + f.notesID + "/approval-requests" }, field("summary")), 0},
		{"POST /api/v1/artifacts/{id}/versions", "summary", session(post, func(f writingCheckFixture) string { return "/api/v1/artifacts/" + f.notesID + "/versions" }, field("summary")), http.StatusCreated},
		{"POST /api/v1/issues/{key}/artifacts/{slug}/versions", "summary", session(post, func(f writingCheckFixture) string {
			return "/api/v1/issues/" + f.issue + "/artifacts/" + f.notesSlug + "/versions"
		}, field("summary")), http.StatusCreated},
		{"POST /api/v1/projects/{key}/artifacts/{slug}/versions", "summary", session(post, func(f writingCheckFixture) string {
			return "/api/v1/projects/WCK/artifacts/" + f.projectDocSlug + "/versions"
		}, field("summary")), http.StatusCreated},
		{"POST /api/v1/artifacts/{id}/edits", "ops[0].with", session(post, func(f writingCheckFixture) string { return "/api/v1/artifacts/" + f.editOneID + "/edits" }, replace()), http.StatusOK},
		{"POST /api/v1/issues/{key}/artifacts/{slug}/edits", "ops[0].with", session(post, func(f writingCheckFixture) string {
			return "/api/v1/issues/" + f.issue + "/artifacts/" + f.editTwoSlug + "/edits"
		}, replace()), http.StatusOK},
		{"POST /api/v1/projects/{key}/artifacts/{slug}/edits", "ops[0].with", session(post, func(f writingCheckFixture) string {
			return "/api/v1/projects/WCK/artifacts/" + f.projectDocSlug + "/edits"
		}, replace()), http.StatusOK},
	}
}

// multipartUpload uploads one file part with its own content type, and a summary, as session.
func (rig *writingCheckRig) multipartUpload(target, name, contentType, content, summary, session string, extra ...string) *httptest.ResponseRecorder {
	rig.t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	values := map[string]string{"name": name, "summary": summary, "actor": `{"kind":"session","id":"` + session + `"}`}
	for index := 0; index+1 < len(extra); index += 2 {
		values[extra[index]] = extra[index+1]
	}
	for key, value := range values {
		if err := form.WriteField(key, value); err != nil {
			rig.t.Fatal(err)
		}
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", `form-data; name="file"; filename="`+name+`"`)
	header.Set("Content-Type", contentType)
	part, err := form.CreatePart(header)
	if err != nil {
		rig.t.Fatal(err)
	}
	_, _ = part.Write([]byte(content))
	if err := form.Close(); err != nil {
		rig.t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, target, &body)
	request.Header.Set("Content-Type", form.FormDataContentType())
	request.Header.Set("Authorization", "Bearer agent-token")
	response := httptest.NewRecorder()
	rig.handler.ServeHTTP(response, request)
	return response
}

// TestTheWritingCheckRecordsAndWritesOnEveryProseRoute is the check's behaviour on every route
// the registry marks: a body breaking an enabled rule is written at once and recorded findings
// with the rule, its field and its reader, and a clean one is recorded clean. A route whose text
// no person reads (readerNone) writes the body, recorded exempt_no_reader. A write the route
// refuses records nothing, as it writes nothing else (recordTakenWrites), and every route takes
// at least one of its writes, which records exactly one row; a dry run, which the check does not
// read, records none. The cases are held to the registry: a route that starts to carry prose
// needs a case here. Every reader in the rig is alice: she holds the issue, the project defaults
// to her, she wrote the comment and the message the replies answer, and she opened the direct
// conversation.
func TestTheWritingCheckRecordsAndWritesOnEveryProseRoute(t *testing.T) {
	rig := newWritingCheckRig(t, writingCheckOn())
	cases := proseRouteCases()
	var covered []string
	for _, c := range cases {
		covered = append(covered, c.route)
	}
	for name, shape := range routeShapes {
		if requestCarriesProse(shape.Request) && !slices.Contains(covered, name) && !strings.Contains(name, "/_test") {
			t.Errorf("%s carries prose and has no case in proseRouteCases", name)
		}
	}
	for _, c := range cases {
		t.Run(c.route, func(t *testing.T) {
			rig.t = t
			method, route, _ := strings.Cut(c.route, " ")
			if _, unread := proseDryRunRoutes[c.route]; unread {
				// The check does not read a dry run, which writes nothing, so it records nothing,
				// whatever its words break.
				for _, text := range []string{badProse, cleanProse} {
					rig.clearEvents()
					if answered := c.call(rig, text); answered.Code != c.wantStatus {
						t.Fatalf("%s: %d %s, want %d", c.route, answered.Code, answered.Body.String(), c.wantStatus)
					}
					if events := rig.events(); len(events) != 0 {
						t.Errorf("the dry run recorded %+v, want no row", events)
					}
				}
				return
			}
			taken := 0
			// write writes text through the route and answers the one row a write that took place
			// recorded, and false for a write the route refused, which must have recorded none.
			write := func(text, what string) (writingCheckEventRow, bool) {
				t.Helper()
				rig.clearEvents()
				written := c.call(rig, text)
				if written.Code == http.StatusUnprocessableEntity || (c.wantStatus != 0 && written.Code != c.wantStatus) {
					t.Fatalf("%s: %d %s, want %d", what, written.Code, written.Body.String(), c.wantStatus)
				}
				if written.Code < 200 || written.Code >= 300 {
					if events := rig.events(); len(events) != 0 {
						t.Errorf("%s was refused (%d %s) and recorded %+v, want no row", what, written.Code, written.Body.String(), events)
					}
					return writingCheckEventRow{}, false
				}
				taken++
				return rig.lastEvent(what), true
			}
			defer func() {
				if taken == 0 {
					t.Errorf("the route took none of its writes, so none shows it records one: give its case a write it takes")
				}
			}()
			if writingCheckReaderRules[c.route] == readerNone {
				if event, took := write(badProse, "bad text to no person"); took && (event.Outcome != writingCheckNoReader || event.Reader != "" ||
					event.Route != route || event.Method != method) {
					t.Errorf("event = %+v, want %s with no reader", event, writingCheckNoReader)
				}
				return
			}
			if event, took := write(badProse, "bad text"); took && (event.Outcome != writingCheckFindings || event.Route != route || event.Method != method || event.Reader != "alice" ||
				!slices.Equal(event.Rules, []string{badRule}) || !slices.Equal(event.Fields, []string{c.field})) {
				t.Errorf("bad text event = %+v, want %s with %s in %s and reader alice", event, writingCheckFindings, badRule, c.field)
			}
			want := writingcheck.OutcomeClean
			if c.route == "POST /api/v1/agents/{session_id}/artifacts" {
				want = writingCheckUnreadUpload
			}
			if event, took := write(cleanProse, "clean text"); took && (event.Outcome != want || event.Reader != "alice") {
				t.Errorf("clean event = %+v, want %s with reader alice", event, want)
			}
		})
	}
}

// TestTheWritingCheckRecordsNoRowForAReplyThatWritesNothing: a reply route answers 2xx for a call
// that writes nothing - a reply to an attempt already answered, which is its replay, an error
// report on such an attempt, and an error report repeated on an attempt already failed - and such a
// call records no row, while the reply and the report it repeats record one each.
func TestTheWritingCheckRecordsNoRowForAReplyThatWritesNothing(t *testing.T) {
	rig := newWritingCheckRig(t, writingCheckOn())
	rig.deliverComment(rig.fixture.comment, checkWriter, 2)
	rig.deliverMessage(rig.fixture.message, checkWriter, 2)
	for _, target := range []string{
		"/api/v1/comments/" + rig.fixture.comment + "/reply",
		"/api/v1/messages/" + rig.fixture.message + "/reply",
	} {
		for _, call := range []struct {
			what   string
			body   map[string]any
			status int
			rows   int
		}{
			{"a reply to attempt 1", map[string]any{"body": badProse, "attempt": 1}, http.StatusCreated, 1},
			{"the reply's replay", map[string]any{"body": badProse, "attempt": 1}, http.StatusOK, 0},
			{"an error report on the answered attempt 1", map[string]any{"error": badProse, "attempt": 1}, http.StatusOK, 0},
			{"an error report on attempt 2", map[string]any{"error": badProse, "attempt": 2}, http.StatusOK, 1},
			{"the error report repeated", map[string]any{"error": badProse, "attempt": 2}, http.StatusOK, 0},
		} {
			rig.clearEvents()
			if response := rig.asSession(http.MethodPost, target, call.body); response.Code != call.status {
				t.Fatalf("%s on %s: %d %s, want %d", call.what, target, response.Code, response.Body.String(), call.status)
			}
			if events := rig.events(); len(events) != call.rows {
				t.Errorf("%s on %s recorded %+v, want %d rows", call.what, target, events, call.rows)
			}
		}
	}
}

// TestTheWritingCheckRecordsNoRowForADocumentCallThatWritesNothing: an edit whose batch leaves the
// document as it was, on each of the three edit routes, and an approval request on a document that
// already stands approved at its version answer 2xx having written none of the words the check
// read, and such a call records no row, while the edit that changes the document and the request
// that opens an approval record one each.
func TestTheWritingCheckRecordsNoRowForADocumentCallThatWritesNothing(t *testing.T) {
	rig := newWritingCheckRig(t, writingCheckOn())
	// write posts body to target as checkWriter and holds the call to its status and to the rows
	// it recorded.
	write := func(what, target string, body map[string]any, status, rows int) {
		t.Helper()
		rig.clearEvents()
		if response := rig.asSession(http.MethodPost, target, body); response.Code != status {
			t.Fatalf("%s: %d %s, want %d", what, response.Code, response.Body.String(), status)
		}
		if events := rig.events(); len(events) != rows {
			t.Errorf("%s recorded %+v, want %d rows", what, events, rows)
		}
	}
	replace := func(find, with string) map[string]any {
		return map[string]any{"ops": []map[string]any{{"op": "replace", "find": find, "with": with}}, "summary": badProse}
	}
	for _, target := range []string{
		"/api/v1/artifacts/" + rig.fixture.editOneID + "/edits",
		"/api/v1/issues/" + rig.fixture.issue + "/artifacts/" + rig.fixture.editTwoSlug + "/edits",
		"/api/v1/projects/WCK/artifacts/" + rig.fixture.projectDocSlug + "/edits",
	} {
		write("an edit that changes the document on "+target, target, replace("Seed paragraph here.", badProse), http.StatusOK, 1)
		write("an edit that leaves it as it was on "+target, target, replace(badProse, badProse), http.StatusOK, 0)
	}
	approvals := "/api/v1/artifacts/" + rig.fixture.notesID + "/approval-requests"
	write("an approval request that opens one", approvals, map[string]any{"summary": badProse}, http.StatusCreated, 1)
	reviewDocument(t, rig.handler, rig.fixture.notesID, "alice", "approved", "")
	write("an approval request on the document approved at its version", approvals, map[string]any{"summary": badProse}, http.StatusOK, 0)
}

// asSessionGenerated writes as checkWriter with the generated-text header, as a plugin posts the
// text it generates.
func (rig *writingCheckRig) asSessionGenerated(method, target string, body map[string]any) *httptest.ResponseRecorder {
	rig.t.Helper()
	withActor := maps.Clone(body)
	withActor["actor"] = map[string]any{"kind": "session", "id": checkWriter}
	data, err := json.Marshal(withActor)
	if err != nil {
		rig.t.Fatal(err)
	}
	request := httptest.NewRequest(method, target, bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer agent-token")
	request.Header.Set(contracts.GeneratedTextHeader, "1")
	response := httptest.NewRecorder()
	rig.handler.ServeHTTP(response, request)
	return response
}

// TestTheWritingCheckRecordsAGeneratedReplyExemptGenerated: a reply a plugin marks with the
// generated-text header, on either reply route, is not read and is recorded exempt_generated with
// its reader. The same text with no header is read, and the header on any other route is not
// read (the negative controls).
func TestTheWritingCheckRecordsAGeneratedReplyExemptGenerated(t *testing.T) {
	rig := newWritingCheckRig(t, writingCheckOn())
	// The reply with no header answers attempt 2: a second reply to attempt 1 would be its replay,
	// which posts nothing and records no row.
	rig.deliverComment(rig.fixture.comment, checkWriter, 2)
	rig.deliverMessage(rig.fixture.message, checkWriter, 2)
	for _, target := range []string{
		"/api/v1/comments/" + rig.fixture.comment + "/reply",
		"/api/v1/messages/" + rig.fixture.message + "/reply",
	} {
		rig.clearEvents()
		rig.asSessionGenerated(http.MethodPost, target, map[string]any{"body": badProse, "attempt": 1})
		if event := rig.lastEvent("a generated reply to " + target); event.Outcome != writingCheckGenerated ||
			event.Reader != "alice" || len(event.Rules) != 0 {
			t.Errorf("a generated reply to %s: event = %+v, want %s with reader alice and nothing read", target, event, writingCheckGenerated)
		}
		rig.clearEvents()
		rig.asSession(http.MethodPost, target, map[string]any{"body": badProse, "attempt": 2})
		if event := rig.lastEvent("an agent's reply to " + target); event.Outcome != writingCheckFindings {
			t.Errorf("an agent's reply to %s: event = %+v, want %s", target, event, writingCheckFindings)
		}
	}
	rig.clearEvents()
	comments := "/api/v1/issues/" + rig.fixture.issue + "/comments"
	if response := rig.asSessionGenerated(http.MethodPost, comments, map[string]any{"body": badProse}); response.Code != http.StatusCreated {
		t.Fatalf("a comment with the header: %d %s, want 201", response.Code, response.Body.String())
	}
	if event := rig.lastEvent("a comment with the header"); event.Outcome != writingCheckFindings {
		t.Errorf("a comment with the header: event = %+v, want %s: the header is read on the reply routes alone", event, writingCheckFindings)
	}
}

// TestTheWritingCheckActorCases is R3 through the server: who is read and who is exempt, with the
// personal token recorded on a daemon-named write made with it.
func TestTheWritingCheckActorCases(t *testing.T) {
	rig := newWritingCheckRig(t, writingCheckOn())
	comments := "/api/v1/issues/" + rig.fixture.issue + "/comments"
	ask := func() string {
		response := rig.asSession(http.MethodPost, "/api/v1/issues/"+rig.fixture.issue+"/asks", map[string]any{"question": "Which port should the server use?"})
		if response.Code != http.StatusCreated {
			t.Fatalf("create ask: %d %s", response.Code, response.Body.String())
		}
		rig.clearEvents()
		return decodeBody[model.Ask](t, response).ID
	}
	answer := map[string]any{"text": badProse, "expected_edited_at": nil}

	rig.clearEvents()
	if response := rig.asSession(http.MethodPost, comments, map[string]any{"body": badProse}); response.Code != http.StatusCreated {
		t.Errorf("a session: %d %s, want 201", response.Code, response.Body.String())
	}
	if event := rig.lastEvent("a session"); event.ActorKind != "session" || event.ActorID != checkWriter || event.Outcome != writingCheckFindings {
		t.Errorf("a session's event = %+v", event)
	}

	rig.clearEvents()
	if response := rig.asTokenFor(checkWriter, http.MethodPost, "/api/v1/asks/"+ask()+"/answer", maps.Clone(answer)); response.Code != http.StatusOK {
		t.Errorf("a person's token acting for a session: %d %s, want 200", response.Code, response.Body.String())
	}
	if event := rig.lastEvent("token for a session"); event.ActorKind != "user" || event.ActSession != checkWriter || event.TokenID != rig.token.ID || event.Outcome != writingCheckFindings {
		t.Errorf("a person's token acting for a session: event = %+v", event)
	}

	askID := ask()
	if response := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", maps.Clone(answer), "alice"); response.Code != http.StatusOK {
		t.Errorf("a person for themselves: %d %s, want 200", response.Code, response.Body.String())
	}
	if event := rig.lastEvent("a person"); event.Outcome != writingcheck.ExemptPerson || event.ActorID != "alice" || event.Reader != "alice" {
		t.Errorf("a person's event = %+v, want exempt_person with its reader recorded", event)
	}

	rig.clearEvents()
	if response := rig.asSessionNamed(daemonID, http.MethodPost, comments, map[string]any{"body": badProse}); response.Code != http.StatusCreated {
		t.Errorf("the daemon session: %d %s, want 201", response.Code, response.Body.String())
	}
	if event := rig.lastEvent("the daemon"); event.Outcome != writingcheck.ExemptDaemon || event.ActorID != daemonID || event.TokenID != "" || event.Reader != "alice" {
		t.Errorf("the daemon's event = %+v", event)
	}

	rig.clearEvents()
	named := map[string]any{"body": badProse, "actor": map[string]any{"kind": "session", "id": daemonID}}
	if response := agentRequest(t, rig.handler, http.MethodPost, comments, named, rig.token.Token); response.Code != http.StatusCreated {
		t.Errorf("a person's token naming the daemon in the body: %d %s, want 201", response.Code, response.Body.String())
	}
	if event := rig.lastEvent("token naming the daemon in the body"); event.Outcome != writingcheck.ExemptDaemon || event.TokenID != rig.token.ID {
		t.Errorf("a person's token naming the daemon in the body: event = %+v, want exempt_daemon with token %s", event, rig.token.ID)
	}

	askID = ask()
	if response := rig.asTokenFor(daemonID, http.MethodPost, "/api/v1/asks/"+askID+"/answer", maps.Clone(answer)); response.Code != http.StatusOK {
		t.Errorf("a person's token naming the daemon in ?session=: %d %s, want 200", response.Code, response.Body.String())
	}
	if event := rig.lastEvent("token naming the daemon in ?session="); event.Outcome != writingcheck.ExemptDaemon || event.ActSession != daemonID || event.TokenID != rig.token.ID {
		t.Errorf("a person's token naming the daemon in ?session=: event = %+v", event)
	}
}

// TestTheWritingCheckReadsAMarkdownUploadAndNotAFile: an upload stored as a Markdown document is
// read as inline content is, and any other file is stored unread.
func TestTheWritingCheckReadsAMarkdownUploadAndNotAFile(t *testing.T) {
	rig := newWritingCheckRig(t, writingCheckOn())
	target := "/api/v1/issues/" + rig.fixture.issue + "/artifacts"
	rig.clearEvents()
	if response := rig.multipartUpload(target, "plan.md", "text/markdown", badProse+"\n", "", checkWriter); response.Code != http.StatusCreated {
		t.Errorf("a text/markdown upload: %d %s, want 201", response.Code, response.Body.String())
	}
	if event := rig.lastEvent("markdown upload"); event.Outcome != writingCheckFindings || !slices.Equal(event.Fields, []string{"content"}) {
		t.Errorf("markdown upload event = %+v", event)
	}
	rig.clearEvents()
	if response := rig.multipartUpload(target, "trace.txt", "text/plain", badProse+"\n", "", checkWriter); response.Code != http.StatusCreated {
		t.Errorf("a text/plain upload: %d %s, want 201", response.Code, response.Body.String())
	}
	if event := rig.lastEvent("plain upload"); event.Outcome != writingCheckUnreadUpload || !strings.Contains(event.Detail, "text/plain") {
		t.Errorf("plain upload event = %+v", event)
	}
	// A new version uploaded whole is read for the blocks it adds alone: the seed's own blocks are
	// not read again, and a bad block it adds is. Each names the token of the version it replaces
	// (`precondition`), which every replacement of a document must.
	base := uploadBaseNamed(t, rig.handler, target, "notes")
	rig.clearEvents()
	whole := "Seed paragraph here.\n\n" + cleanProse + "\n"
	if response := rig.asSession(http.MethodPost, target, map[string]any{"name": "notes", "content": whole, "precondition": base}); response.Code != http.StatusCreated {
		t.Fatalf("a clean new version: %d %s", response.Code, response.Body.String())
	}
	if event := rig.lastEvent("a clean new version"); event.Outcome != writingcheck.OutcomeClean {
		t.Errorf("a clean new version: event = %+v, want clean", event)
	}
	base = uploadBaseNamed(t, rig.handler, target, "notes")
	rig.clearEvents()
	if response := rig.asSession(http.MethodPost, target, map[string]any{"name": "notes", "content": whole + "\n" + badProse + "\n", "precondition": base}); response.Code != http.StatusCreated {
		t.Errorf("a new version adding a bad block: %d %s, want 201", response.Code, response.Body.String())
	}
	if event := rig.lastEvent("a new version adding a bad block"); event.Outcome != writingCheckFindings || !slices.Equal(event.Rules, []string{badRule}) {
		t.Errorf("a new version adding a bad block: event = %+v, want %s", event, writingCheckFindings)
	}
}

// TestTheWritingCheckCountsAndStateRoutes reads the two read routes. The counts list every
// outcome each day of the range recorded, the agent's broadcast's exempt_no_reader and a plugin's
// generated reply's exempt_generated among them, and give the write that posted over a finding to
// its reader.
func TestTheWritingCheckCountsAndStateRoutes(t *testing.T) {
	rig := newWritingCheckRig(t, writingCheckOn())
	comments := "/api/v1/issues/" + rig.fixture.issue + "/comments"
	rig.asSession(http.MethodPost, comments, map[string]any{"body": badProse})
	rig.asSession(http.MethodPost, comments, map[string]any{"body": cleanProse})
	rig.asSessionGenerated(http.MethodPost, "/api/v1/messages/"+rig.fixture.message+"/reply", map[string]any{"body": badProse, "attempt": 1})
	rig.asTokenFor(checkWriter, http.MethodPost, "/api/v1/broadcasts", map[string]any{"body": badProse, "delivery": "btw", "session_ids": []string{"s-reader"}})
	now := time.Now().UTC()
	today, yesterday := now.Format(time.DateOnly), now.AddDate(0, 0, -1).Format(time.DateOnly)
	response := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/writing-check/counts?first="+yesterday+"&last="+today, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("counts: %d %s", response.Code, response.Body.String())
	}
	counts := decodeBody[store.WritingCheckCounts](t, response)
	if counts.First != yesterday || counts.Last != today || len(counts.Days) != 2 || counts.Days[0].Day != yesterday || counts.Days[0].Total != 0 {
		t.Fatalf("counts = %+v, want yesterday with nothing and today", counts)
	}
	if day := counts.Days[1]; day.Day != today || day.Total != 4 || day.Outcomes[writingCheckFindings] != 1 || day.Outcomes[writingCheckGenerated] != 1 ||
		day.Outcomes["clean"] != 1 || day.Outcomes[writingCheckNoReader] != 1 ||
		day.Routes["POST /api/v1/issues/{key}/comments"] != 2 || day.Routes["POST /api/v1/messages/{id}/reply"] != 1 ||
		day.Routes["POST /api/v1/broadcasts"] != 1 {
		t.Errorf("today = %+v", day)
	}
	overFinding := 0
	for _, row := range counts.Items {
		if row.Day != today {
			t.Errorf("a row of another day: %+v", row)
		}
		overFinding += row.PostedOverFinding
	}
	if overFinding != 1 {
		t.Errorf("rows = %+v, want the one write with findings counted as posted over a finding", counts.Items)
	}
	if response := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/writing-check/counts", nil, "alice"); response.Code != http.StatusOK {
		t.Errorf("no query: %d %s", response.Code, response.Body.String())
	} else if counts := decodeBody[store.WritingCheckCounts](t, response); counts.Last != today || len(counts.Days) != 14 || counts.Days[13].Total != 4 {
		t.Errorf("no query = %+v, want the 14 days to today", counts)
	}
	if response := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/writing-check/counts?first=2026-01-01&last=2026-01-01", nil, "alice"); response.Code != http.StatusOK ||
		decodeBody[store.WritingCheckCounts](t, response).Days[0].Total != 0 {
		t.Errorf("an empty day: %d %s", response.Code, response.Body.String())
	}
	for _, query := range []string{"first=today", "last=2026-1-1", "first=2026-01-02&last=2026-01-01", "first=2025-01-01&last=2026-01-02"} {
		if response := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/writing-check/counts?"+query, nil, "alice"); response.Code != http.StatusBadRequest {
			t.Errorf("?%s: %d %s, want 400", query, response.Code, response.Body.String())
		}
	}
	state := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/writing-check", nil, "alice")
	if state.Code != http.StatusOK {
		t.Fatalf("state: %d %s", state.Code, state.Body.String())
	}
	got := decodeBody[writingCheckState](t, state)
	if !got.Enabled || !slices.Equal(got.Rules, writingcheck.EnabledRules) || got.Detector.On || got.Detector.Reason != writingCheckDetectorElsewhere {
		t.Errorf("state = %+v", got)
	}
	for _, measured := range writingcheck.MeasuredOff {
		if !slices.Contains(got.RulesLeftOut, writingCheckRuleLeftOut{measured.ID, measured.Reason}) {
			t.Errorf("state names no measured reason for %s: %+v", measured.ID, got.RulesLeftOut)
		}
	}
	if off := (*WritingCheck)(nil).state(); off.Enabled || off.Detector.On || off.Detector.Reason != "the check is off" {
		t.Errorf("the state of a check that is off = %+v", off)
	}
}

// GET /api/v1/writing-check/counts pages the rows of each day, project and person in that order:
// limit of them from offset, total counting every row, a page past the last one empty with the
// total still counted, the day's counts whole on every page, and a limit or an offset out of its
// bounds 400 INVALID_QUERY.
func TestTheWritingCheckCountsPageTheirRows(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	ctx := context.Background()
	for _, reader := range []string{"carol", "alice", "bob"} {
		if err := database.InsertWritingCheckEvent(ctx, store.WritingCheckEvent{
			Route: "/api/v1/issues/{key}/comments", Method: "POST", ActorKind: "session", ActorID: "s-agent",
			Outcome: "held", Reader: reader, Project: "WCK",
		}); err != nil {
			t.Fatalf("record %s's held text: %v", reader, err)
		}
	}
	const day = "2026-01-05"
	if _, err := database.Pool.Exec(ctx, `update writing_check_events set created_at = $1`, time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("date the rows: %v", err)
	}
	page := func(query string) store.WritingCheckCounts {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/writing-check/counts?first="+day+"&last="+day+"&"+query, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("counts ?%s: %d %s", query, response.Code, response.Body.String())
		}
		counts := decodeBody[store.WritingCheckCounts](t, response)
		if len(counts.Days) != 1 || counts.Days[0].Total != 3 {
			t.Errorf("counts ?%s: days %+v, want the one day with its three decisions", query, counts.Days)
		}
		return counts
	}
	people := func(counts store.WritingCheckCounts) []string {
		var listed []string
		for _, row := range counts.Items {
			listed = append(listed, row.Person)
		}
		return listed
	}
	if first := page("limit=2"); !slices.Equal(people(first), []string{"alice", "bob"}) || first.Total != 3 || first.Limit != 2 || first.Offset != 0 {
		t.Errorf("the first page = %v total %d limit %d offset %d, want alice and bob of 3 at limit 2 offset 0", people(first), first.Total, first.Limit, first.Offset)
	}
	if second := page("limit=2&offset=2"); !slices.Equal(people(second), []string{"carol"}) || second.Total != 3 || second.Offset != 2 {
		t.Errorf("the second page = %v total %d offset %d, want carol of 3 at offset 2", people(second), second.Total, second.Offset)
	}
	if past := page("offset=3"); len(past.Items) != 0 || past.Total != 3 || past.Limit != listDefaultLimit || past.Offset != 3 {
		t.Errorf("a page past the last = %+v, want no row, total 3, limit %d and offset 3", past, listDefaultLimit)
	}
	for _, query := range []string{"limit=0", "limit=201", "offset=-1"} {
		if response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/writing-check/counts?first="+day+"&last="+day+"&"+query, nil, "alice"); response.Code != http.StatusBadRequest ||
			!strings.Contains(response.Body.String(), "INVALID_QUERY") {
			t.Errorf("?%s: %d %s, want 400 INVALID_QUERY", query, response.Code, response.Body.String())
		}
	}
}

// TestTheWritingCheckOffReadsNothing: a server with the check off records nothing and refuses
// nothing.
func TestTheWritingCheckOffReadsNothing(t *testing.T) {
	rig := newWritingCheckRig(t, nil)
	comments := "/api/v1/issues/" + rig.fixture.issue + "/comments"
	if response := rig.asSession(http.MethodPost, comments, map[string]any{"body": badProse}); response.Code != http.StatusCreated {
		t.Errorf("check off: %d %s, want 201", response.Code, response.Body.String())
	}
	if events := rig.events(); len(events) != 0 {
		t.Errorf("check off recorded %+v", events)
	}
}
