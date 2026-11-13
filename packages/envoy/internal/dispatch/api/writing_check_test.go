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

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/candidates"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/reader"
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
// or not prose with a reason, no mark names a member its type does not have, and every request
// type that carries prose declares override_writing_check. The marks are logged route by route.
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
			if _, declares := request.(writingCheckOverrideCarrier); !declares {
				t.Errorf("%s carries prose (%s) and does not declare %s: embed writingCheckOverride in %T",
					name, strings.Join(prose, ", "), writingCheckOverrideField, request)
			}
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

// --- the actor rule and the refusal ----------------------------------------------------------

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

func TestTheWritingCheckRefusalWording(t *testing.T) {
	refusal := writingCheckRefusalOf([]writingcheck.Finding{{
		Field: "body", Rule: "phrasal-verb", Text: "We spin up the `worker` now.",
		Rewrite: `Replace "spin up" with one plain verb, for example "start".`,
	}})
	if refusal.Code != codeWritingCheckRefused || refusal.OverrideField != writingCheckOverrideField || len(refusal.Findings) != 1 {
		t.Fatalf("refusal = %+v", refusal)
	}
	for _, part := range []string{
		"1. Replace `spin up` with one plain verb, for example `start`. Rule `phrasal-verb`, field `body`: ``We spin up the `worker` now.``.",
		"`--override-writing-check`", "`override_writing_check: \"agent\"`",
	} {
		if !strings.Contains(refusal.Error, part) {
			t.Errorf("refusal text %q does not hold %q", refusal.Error, part)
		}
	}
	if strings.Index(refusal.Error, "Replace") > strings.Index(refusal.Error, "Rule `phrasal-verb`") ||
		strings.Index(refusal.Error, "Rule `phrasal-verb`") > strings.Index(refusal.Error, "--override-writing-check") {
		t.Errorf("refusal text %q is not rewrite, rule and sentence, then the way through", refusal.Error)
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

// writingCheckOn is the check as the tests run it: on, with the bundle's structure rules, and
// with the bundle's detector where the bundle ships one and a test gives a reader.
func writingCheckOn(r writingcheck.Reader) *WritingCheck {
	return NewWritingCheck(WritingCheckConfig{Enabled: true, Detector: r != nil, Reader: r})
}

type writingCheckRig struct {
	t        *testing.T
	handler  http.Handler
	database *store.Store
	token    agentTokenResponse
	fixture  writingCheckFixture
}

type writingCheckFixture struct {
	issue, spec, notesID, notesSlug, editOneID, editTwoSlug, projectDocID, projectDocSlug string
	askEdit, askAnswer, askResolve, askDecline                                            string
	message, comment, result                                                              string
}

func newWritingCheckRig(t *testing.T, check *WritingCheck) *writingCheckRig {
	t.Helper()
	handler, database, _ := newTestServer(t, testServerOptions{writingCheck: check, writingCheckOff: check == nil})
	rig := &writingCheckRig{t: t, handler: handler, database: database}
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
	comment := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{"body": "The cache is warm."}, "alice")
	if comment.Code != http.StatusCreated {
		t.Fatalf("create comment: %d %s", comment.Code, comment.Body.String())
	}
	rig.fixture.comment = decodeBody[model.Comment](t, comment).ID
	// A result recorded on the issue, so its retraction is text on that issue.
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	result := recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil))
	if result.Code != http.StatusCreated {
		t.Fatalf("record a result: %d %s", result.Code, result.Body.String())
	}
	rig.fixture.result = decodeBody[recordedResult](t, result).Result.ID
	// alice's direct conversation with the agent session checkWriter, which makes her the person
	// who reads what that session puts into its conversation.
	rig.openConversation("user", "alice", checkWriter)
	rig.clearEvents()
	return rig
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
	Route, Method, ActorKind, ActorID, ActSession, TokenID, Outcome, Override, Detail, Reader string
	Rules, Fields                                                                             []string
	Candidates                                                                                int
}

func (rig *writingCheckRig) events() []writingCheckEventRow {
	rig.t.Helper()
	rows, err := rig.database.Pool.Query(context.Background(), `
		select route, method, actor_kind, actor_id, coalesce(act_session, ''), coalesce(token_id, ''), outcome,
			coalesce(override_value, ''), coalesce(detail, ''), coalesce(reader, ''), rules, fields, candidates
		from writing_check_events order by id`)
	if err != nil {
		rig.t.Fatalf("read writing check events: %v", err)
	}
	defer rows.Close()
	var events []writingCheckEventRow
	for rows.Next() {
		var row writingCheckEventRow
		if err := rows.Scan(&row.Route, &row.Method, &row.ActorKind, &row.ActorID, &row.ActSession, &row.TokenID,
			&row.Outcome, &row.Override, &row.Detail, &row.Reader, &row.Rules, &row.Fields, &row.Candidates); err != nil {
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
	// call writes text through the route; wantClean is the status a clean write answers, 0 for
	// any status but 422 (a route whose write needs a state the rig does not build, such as a
	// delivery attempt to reply to, still reaches the check before it refuses for that).
	call      func(rig *writingCheckRig, text string) *httptest.ResponseRecorder
	wantClean int
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
	replace := func(text string) map[string]any {
		return map[string]any{"ops": []map[string]any{{"op": "replace", "find": "Seed paragraph here.", "with": text}}}
	}
	upload := func(path func(f writingCheckFixture) string) func(*writingCheckRig, string) *httptest.ResponseRecorder {
		names := 0
		return func(rig *writingCheckRig, text string) *httptest.ResponseRecorder {
			names++
			return rig.asSession(http.MethodPost, path(rig.fixture), map[string]any{"name": fmt.Sprintf("upload %d", names), "content": text})
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
			field("run", "check", "no-such-check", "role", "tester", "commit", strings.Repeat("a", 40), "verdict", "pass")), 0},
		{"POST /api/v1/results/{id}/retract", "reason", session(post, func(f writingCheckFixture) string {
			return "/api/v1/results/" + f.result + "/retract"
		}, field("reason")), 0},
		{"POST /api/v1/issues/{key}/messages", "body", session(post, func(f writingCheckFixture) string { return "/api/v1/issues/" + f.issue + "/messages" }, field("body")), http.StatusCreated},
		{"POST /api/v1/messages/{id}/reply", "body", session(post, func(f writingCheckFixture) string { return "/api/v1/messages/" + f.message + "/reply" }, field("body", "attempt", 1)), 0},
		{"POST /api/v1/agents/{session_id}/messages", "body", token(http.MethodPost, func(writingCheckFixture) string { return "/api/v1/agents/s-reader/messages" }, field("body", "delivery", "btw")), 0},
		{"POST /api/v1/broadcasts", "body", token(http.MethodPost, func(writingCheckFixture) string { return "/api/v1/broadcasts" }, field("body", "delivery", "btw", "session_ids", []string{"s-reader"})), 0},
		{"POST /api/v1/issues/{key}/asks", "question", session(post, func(f writingCheckFixture) string { return "/api/v1/issues/" + f.issue + "/asks" }, field("question")), http.StatusCreated},
		{"POST /api/v1/artifacts/{id}/asks", "question", session(post, func(f writingCheckFixture) string { return "/api/v1/artifacts/" + f.projectDocID + "/asks" }, field("question")), http.StatusCreated},
		{"PATCH /api/v1/asks/{id}", "question", session(patch, func(f writingCheckFixture) string { return "/api/v1/asks/" + f.askEdit }, field("question")), http.StatusOK},
		{"POST /api/v1/asks/{id}/answer", "text", token(http.MethodPost, func(f writingCheckFixture) string { return "/api/v1/asks/" + f.askAnswer + "/answer" }, field("text", "expected_edited_at", nil)), http.StatusOK},
		{"POST /api/v1/asks/{id}/resolve", "reason", session(post, func(f writingCheckFixture) string { return "/api/v1/asks/" + f.askResolve + "/resolve" }, field("reason", "kind", "resolved")), http.StatusOK},
		{"POST /api/v1/asks/{id}/decline", "note", token(http.MethodPost, func(f writingCheckFixture) string { return "/api/v1/asks/" + f.askDecline + "/decline" }, field("note")), 0},
		{"POST /api/v1/issues/{key}/comments", "body", session(post, func(f writingCheckFixture) string { return "/api/v1/issues/" + f.issue + "/comments" }, field("body")), http.StatusCreated},
		{"POST /api/v1/artifacts/{id}/comments", "body", session(post, func(f writingCheckFixture) string { return "/api/v1/artifacts/" + f.projectDocID + "/comments" }, field("body")), http.StatusCreated},
		{"POST /api/v1/comments/{id}/reply", "body", session(post, func(f writingCheckFixture) string { return "/api/v1/comments/" + f.comment + "/reply" }, field("body", "attempt", 1)), 0},
		{"PATCH /api/v1/comments/{id}", "body", token(http.MethodPatch, func(f writingCheckFixture) string { return "/api/v1/comments/" + f.comment }, field("body")), 0},
		{"POST /api/v1/artifacts/{id}/rebuild", "markdown", token(http.MethodPost, func(f writingCheckFixture) string { return "/api/v1/artifacts/" + f.spec + "/rebuild" }, field("markdown")), 0},
		{"POST /api/v1/artifacts/{id}/reviews", "reason", token(http.MethodPost, func(f writingCheckFixture) string { return "/api/v1/artifacts/" + f.notesID + "/reviews" }, field("reason", "state", "changes_requested")), 0},
		{"POST /api/v1/artifacts/{id}/approval-requests", "summary", session(post, func(f writingCheckFixture) string { return "/api/v1/artifacts/" + f.notesID + "/approval-requests" }, field("summary")), 0},
		{"POST /api/v1/artifacts/{id}/versions", "summary", session(post, func(f writingCheckFixture) string { return "/api/v1/artifacts/" + f.notesID + "/versions" }, field("summary")), http.StatusCreated},
		{"POST /api/v1/issues/{key}/artifacts/{slug}/versions", "summary", session(post, func(f writingCheckFixture) string {
			return "/api/v1/issues/" + f.issue + "/artifacts/" + f.notesSlug + "/versions"
		}, field("summary")), http.StatusCreated},
		{"POST /api/v1/projects/{key}/artifacts/{slug}/versions", "summary", session(post, func(f writingCheckFixture) string {
			return "/api/v1/projects/WCK/artifacts/" + f.projectDocSlug + "/versions"
		}, field("summary")), http.StatusCreated},
		{"POST /api/v1/artifacts/{id}/edits", "ops[0].with", session(post, func(f writingCheckFixture) string { return "/api/v1/artifacts/" + f.editOneID + "/edits" }, replace), http.StatusOK},
		{"POST /api/v1/issues/{key}/artifacts/{slug}/edits", "ops[0].with", session(post, func(f writingCheckFixture) string {
			return "/api/v1/issues/" + f.issue + "/artifacts/" + f.editTwoSlug + "/edits"
		}, replace), http.StatusOK},
		{"POST /api/v1/projects/{key}/artifacts/{slug}/edits", "ops[0].with", session(post, func(f writingCheckFixture) string {
			return "/api/v1/projects/WCK/artifacts/" + f.projectDocSlug + "/edits"
		}, replace), http.StatusOK},
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

// TestTheWritingCheckRefusesAndWritesOnEveryProseRoute is R1's behaviour on every route the
// registry marks: a body breaking an enabled rule is refused 422 with the finding in its field
// and recorded refused with its reader, and a clean one is written and recorded clean. A route
// whose text no person reads (readerNone) writes both, recorded exempt_no_reader. The cases are
// held to the registry: a route that starts to carry prose needs a case here. Every reader in
// the rig is alice: she holds the issue, the project defaults to her, she wrote the comment and
// the message the replies answer, and she opened the direct conversation.
func TestTheWritingCheckRefusesAndWritesOnEveryProseRoute(t *testing.T) {
	rig := newWritingCheckRig(t, writingCheckOn(nil))
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
			rig.clearEvents()
			if writingCheckReaderRules[c.route] == readerNone {
				written := c.call(rig, badProse)
				if written.Code == http.StatusUnprocessableEntity {
					t.Fatalf("bad text to no person: %d %s, want it written", written.Code, written.Body.String())
				}
				if event := rig.lastEvent("bad text to no person"); event.Outcome != writingCheckNoReader || event.Reader != "" ||
					event.Route != route || event.Method != method {
					t.Errorf("event = %+v, want %s with no reader", event, writingCheckNoReader)
				}
				return
			}
			refused := c.call(rig, badProse)
			if refused.Code != http.StatusUnprocessableEntity {
				t.Fatalf("bad text: %d %s, want 422", refused.Code, refused.Body.String())
			}
			body := decodeBody[writingCheckRefusal](t, refused)
			if body.Code != codeWritingCheckRefused || len(body.Findings) != 1 || body.Findings[0].Field != c.field ||
				body.Findings[0].Rule != badRule || body.OverrideField != writingCheckOverrideField {
				t.Errorf("refusal = %+v, want one %s finding in %s", body, badRule, c.field)
			}
			event := rig.lastEvent("bad text")
			if event.Outcome != "refused" || event.Route != route || event.Method != method || event.Reader != "alice" ||
				!slices.Equal(event.Rules, []string{badRule}) || !slices.Equal(event.Fields, []string{c.field}) {
				t.Errorf("refused event = %+v, want refused with reader alice", event)
			}
			rig.clearEvents()
			written := c.call(rig, cleanProse)
			if written.Code == http.StatusUnprocessableEntity || (c.wantClean != 0 && written.Code != c.wantClean) {
				t.Fatalf("clean text: %d %s, want %d", written.Code, written.Body.String(), c.wantClean)
			}
			want := "clean"
			if c.route == "POST /api/v1/agents/{session_id}/artifacts" {
				want = writingCheckUnreadUpload
			}
			if event := rig.lastEvent("clean text"); event.Outcome != want || event.Reader != "alice" {
				t.Errorf("clean event = %+v, want %s with reader alice", event, want)
			}
		})
	}
}

// TestTheWritingCheckOverrideWritesAndRecordsForced: override_writing_check agent or generated
// lets a refused write through, recorded forced with the rules it broke and the value; any other
// value is 400.
func TestTheWritingCheckOverrideWritesAndRecordsForced(t *testing.T) {
	rig := newWritingCheckRig(t, writingCheckOn(nil))
	target := "/api/v1/issues/" + rig.fixture.issue + "/comments"
	for _, value := range []string{"agent", "generated"} {
		rig.clearEvents()
		response := rig.asSession(http.MethodPost, target, map[string]any{"body": badProse, "override_writing_check": value})
		if response.Code != http.StatusCreated {
			t.Fatalf("override %s: %d %s", value, response.Code, response.Body.String())
		}
		event := rig.lastEvent("override " + value)
		if event.Outcome != "forced" || event.Override != value || !slices.Equal(event.Rules, []string{badRule}) || !slices.Equal(event.Fields, []string{"body"}) {
			t.Errorf("override %s event = %+v", value, event)
		}
	}
	rig.clearEvents()
	if response := rig.asSession(http.MethodPost, target, map[string]any{"body": badProse, "override_writing_check": "person"}); response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), "INVALID_WRITING_CHECK_OVERRIDE") {
		t.Errorf("override person: %d %s, want 400 INVALID_WRITING_CHECK_OVERRIDE", response.Code, response.Body.String())
	}
	if events := rig.events(); len(events) != 0 {
		t.Errorf("an invalid override recorded %+v", events)
	}
	// A multipart upload takes the override as a form value.
	rig.clearEvents()
	if response := rig.multipartUpload("/api/v1/issues/"+rig.fixture.issue+"/artifacts", "forced.md", "text/markdown", badProse, "", checkWriter,
		"override_writing_check", "agent"); response.Code != http.StatusCreated {
		t.Fatalf("forced markdown upload: %d %s", response.Code, response.Body.String())
	}
	if event := rig.lastEvent("forced upload"); event.Outcome != "forced" || event.Override != "agent" {
		t.Errorf("forced upload event = %+v", event)
	}
}

// TestTheWritingCheckActorCases is R3 through the server: who is read and who is exempt, with the
// personal token recorded on a daemon-named write made with it.
func TestTheWritingCheckActorCases(t *testing.T) {
	rig := newWritingCheckRig(t, writingCheckOn(nil))
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
	if response := rig.asSession(http.MethodPost, comments, map[string]any{"body": badProse}); response.Code != http.StatusUnprocessableEntity {
		t.Errorf("a session: %d %s, want 422", response.Code, response.Body.String())
	}
	if event := rig.lastEvent("a session"); event.ActorKind != "session" || event.ActorID != checkWriter || event.Outcome != "refused" {
		t.Errorf("a session's event = %+v", event)
	}

	rig.clearEvents()
	if response := rig.asTokenFor(checkWriter, http.MethodPost, "/api/v1/asks/"+ask()+"/answer", maps.Clone(answer)); response.Code != http.StatusUnprocessableEntity {
		t.Errorf("a person's token acting for a session: %d %s, want 422", response.Code, response.Body.String())
	}
	if event := rig.lastEvent("token for a session"); event.ActorKind != "user" || event.ActSession != checkWriter || event.TokenID != rig.token.ID || event.Outcome != "refused" {
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
	rig := newWritingCheckRig(t, writingCheckOn(nil))
	target := "/api/v1/issues/" + rig.fixture.issue + "/artifacts"
	rig.clearEvents()
	if response := rig.multipartUpload(target, "plan.md", "text/markdown", badProse+"\n", "", checkWriter); response.Code != http.StatusUnprocessableEntity {
		t.Errorf("a text/markdown upload: %d %s, want 422", response.Code, response.Body.String())
	}
	if event := rig.lastEvent("markdown upload"); event.Outcome != "refused" || !slices.Equal(event.Fields, []string{"content"}) {
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
	// not read again, and a bad block it adds is.
	rig.clearEvents()
	whole := "Seed paragraph here.\n\n" + cleanProse + "\n"
	if response := rig.asSession(http.MethodPost, target, map[string]any{"name": "notes", "content": whole}); response.Code != http.StatusCreated {
		t.Fatalf("a clean new version: %d %s", response.Code, response.Body.String())
	}
	rig.clearEvents()
	if response := rig.asSession(http.MethodPost, target, map[string]any{"name": "notes", "content": whole + "\n" + badProse + "\n"}); response.Code != http.StatusUnprocessableEntity {
		t.Errorf("a new version adding a bad block: %d %s, want 422", response.Code, response.Body.String())
	}
}

// TestTheWritingCheckCountsAndStateRoutes reads the two read routes. The counts list every
// outcome the day recorded, the agent's broadcast's exempt_no_reader among them.
func TestTheWritingCheckCountsAndStateRoutes(t *testing.T) {
	rig := newWritingCheckRig(t, writingCheckOn(nil))
	comments := "/api/v1/issues/" + rig.fixture.issue + "/comments"
	rig.asSession(http.MethodPost, comments, map[string]any{"body": badProse})
	rig.asSession(http.MethodPost, comments, map[string]any{"body": badProse, "override_writing_check": "generated"})
	rig.asSession(http.MethodPost, comments, map[string]any{"body": cleanProse})
	rig.asTokenFor(checkWriter, http.MethodPost, "/api/v1/broadcasts", map[string]any{"body": badProse, "delivery": "btw", "session_ids": []string{"s-reader"}})
	today := time.Now().UTC().Format(time.DateOnly)
	response := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/writing-check/counts?day="+today, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("counts: %d %s", response.Code, response.Body.String())
	}
	counts := decodeBody[store.WritingCheckCounts](t, response)
	if counts.Day != today || counts.Total != 4 || counts.Outcomes["refused"] != 1 || counts.Outcomes["forced"] != 1 ||
		counts.Outcomes["clean"] != 1 || counts.Outcomes[writingCheckNoReader] != 1 || counts.Overrides["generated"] != 1 ||
		counts.Routes["POST /api/v1/issues/{key}/comments"] != 3 || counts.Routes["POST /api/v1/broadcasts"] != 1 {
		t.Errorf("counts = %+v", counts)
	}
	if response := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/writing-check/counts?day=2026-01-01", nil, "alice"); response.Code != http.StatusOK ||
		decodeBody[store.WritingCheckCounts](t, response).Total != 0 {
		t.Errorf("an empty day: %d %s", response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/writing-check/counts?day=today", nil, "alice"); response.Code != http.StatusBadRequest {
		t.Errorf("a malformed day: %d %s, want 400", response.Code, response.Body.String())
	}
	state := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/writing-check", nil, "alice")
	if state.Code != http.StatusOK {
		t.Fatalf("state: %d %s", state.Code, state.Body.String())
	}
	got := decodeBody[writingCheckState](t, state)
	if !got.Enabled || !slices.Equal(got.Rules, writingcheck.EnabledRules) || got.Detector.On || got.Model != nil {
		t.Errorf("state = %+v", got)
	}
	for _, measured := range writingcheck.MeasuredOff {
		if !slices.Contains(got.RulesLeftOut, writingCheckRuleLeftOut{measured.ID, measured.Reason}) {
			t.Errorf("state names no measured reason for %s: %+v", measured.ID, got.RulesLeftOut)
		}
	}
	off := NewWritingCheck(WritingCheckConfig{Enabled: true, RulesOff: []string{badRule}, Detector: true}).state()
	if slices.Contains(off.Rules, badRule) || off.Detector.On != writingcheck.DetectorEnabled {
		t.Errorf("state with %s off and the detector asked for = %+v, want the bundle's detector %t", badRule, off, writingcheck.DetectorEnabled)
	}
	if !writingcheck.DetectorEnabled && !strings.Contains(off.Detector.Reason, "the bundle ships the detector off") {
		t.Errorf("state names no bundle reason for its detector: %+v", off.Detector)
	}
	if !slices.Contains(off.RulesLeftOut, writingCheckRuleLeftOut{badRule, "DISPATCH_WRITING_CHECK_RULES_OFF names it"}) {
		t.Errorf("state with %s off names no reason for it: %+v", badRule, off.RulesLeftOut)
	}
}

// timeoutReader is a reader whose every call times out.
type timeoutReader struct{ calls int64 }

func (r *timeoutReader) Read(context.Context, []candidates.Candidate) ([]reader.Verdict, string) {
	r.calls++
	return nil, reader.OutcomeTimeout
}

func (r *timeoutReader) Calls() int64 { return r.calls }

// TestTheWritingCheckLetsAWriteThroughWhenTheReaderTimesOut: a reader fault never refuses. While
// the bundle ships the detector off, a reader the server is given is never called, whatever the
// write holds; the fault itself is the checker's own test (writingcheck.TestAReaderFaultIsTheOutcome).
func TestTheWritingCheckLetsAWriteThroughWhenTheReaderTimesOut(t *testing.T) {
	stub := &timeoutReader{}
	rig := newWritingCheckRig(t, writingCheckOn(stub))
	rig.clearEvents()
	if response := rig.asSession(http.MethodPost, "/api/v1/issues/"+rig.fixture.issue+"/comments", map[string]any{"body": "This names the problem."}); response.Code != http.StatusCreated {
		t.Fatalf("a write whose reader timed out: %d %s, want 201", response.Code, response.Body.String())
	}
	event := rig.lastEvent("reader timeout")
	switch {
	case !writingcheck.DetectorEnabled && (event.Outcome != "clean" || stub.calls != 0):
		t.Errorf("detector off in the bundle: event = %+v, %d calls, want clean and no call", event, stub.calls)
	case writingcheck.DetectorEnabled && (event.Outcome != reader.OutcomeTimeout || event.Candidates == 0 || stub.calls != 1):
		t.Errorf("reader timeout event = %+v, %d calls", event, stub.calls)
	}
}

// namingReader is a reader that calls every candidate it is sent vacuous.
type namingReader struct{ calls int64 }

func (r *namingReader) Read(_ context.Context, cands []candidates.Candidate) ([]reader.Verdict, string) {
	r.calls++
	verdicts := make([]reader.Verdict, len(cands))
	for i, c := range cands {
		verdicts[i] = reader.Verdict{ID: c.ID, Vacuous: true, Rewrite: "Say what the problem is."}
	}
	return verdicts, reader.OutcomeRead
}

func (r *namingReader) Calls() int64 { return r.calls }

// TestTheWritingCheckTestHookRunsTheDetector: a test process's DetectorTestHook
// (DISPATCH_TEST_WRITING_CHECK_DETECTOR=on) runs the detector although the bundle ships it off,
// so its reader's verdict refuses the write, and the state route says what turned it on.
// DISPATCH_WRITING_CHECK_DETECTOR=off still turns it off.
func TestTheWritingCheckTestHookRunsTheDetector(t *testing.T) {
	stub := &namingReader{}
	check := NewWritingCheck(WritingCheckConfig{Enabled: true, Detector: true, DetectorTestHook: true, Reader: stub})
	rig := newWritingCheckRig(t, check)
	rig.clearEvents()
	response := rig.asSession(http.MethodPost, "/api/v1/issues/"+rig.fixture.issue+"/comments", map[string]any{"body": "This names the problem."})
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), writingcheck.DetectorRule) {
		t.Fatalf("a write the reader calls vacuous: %d %s, want 422 naming %s", response.Code, response.Body.String(), writingcheck.DetectorRule)
	}
	if event := rig.lastEvent("detector test hook"); event.Outcome != "refused" || stub.calls != 1 {
		t.Errorf("event = %+v, %d calls, want refused after one call", event, stub.calls)
	}
	state := check.state()
	if !state.Detector.On || (!writingcheck.DetectorEnabled && !strings.Contains(state.Detector.Reason, "DISPATCH_TEST_WRITING_CHECK_DETECTOR")) {
		t.Errorf("state detector = %+v, want on and the test hook named", state.Detector)
	}
	if off := NewWritingCheck(WritingCheckConfig{Enabled: true, Detector: false, DetectorTestHook: true, Reader: stub}).state(); off.Detector.On {
		t.Errorf("detector off beside the test hook: state detector = %+v, want off", off.Detector)
	}
}

// TestTheWritingCheckRefusalTextPassesTheCheck: the refusal of each rule the bundle enables,
// posted as an agent's own body, is written.
func TestTheWritingCheckRefusalTextPassesTheCheck(t *testing.T) {
	rig := newWritingCheckRig(t, writingCheckOn(nil))
	comments := "/api/v1/issues/" + rig.fixture.issue + "/comments"
	breaking := map[string]string{
		"semicolon":            "The build failed; the cache was empty.",
		"phrasal-verb":         "We will spin up a new worker for the queue.",
		"nominalization":       "The team will perform an analysis of the logs.",
		"long-sentence":        strings.Repeat("The worker reads one row ", 6) + "today.",
		"dangling-conjunction": "Steps:\n\n- The cache is warm and\n- The queue is empty.\n",
	}
	for _, rule := range writingcheck.EnabledRules {
		body, ok := breaking[rule]
		if !ok {
			t.Errorf("no body here breaks %s, a rule the bundle enables", rule)
			continue
		}
		refused := rig.asSession(http.MethodPost, comments, map[string]any{"body": body})
		if refused.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %s, want 422", rule, refused.Code, refused.Body.String())
			continue
		}
		refusal := decodeBody[writingCheckRefusal](t, refused)
		if len(refusal.Findings) == 0 || refusal.Findings[0].Rule != rule {
			t.Errorf("%s: findings %+v", rule, refusal.Findings)
		}
		if posted := rig.asSession(http.MethodPost, comments, map[string]any{"body": refusal.Error}); posted.Code != http.StatusCreated {
			t.Errorf("%s: the refusal text posted as a body: %d %s", rule, posted.Code, posted.Body.String())
		}
	}
}

// TestTheWritingCheckOffReadsNothing: a server with the check off records nothing and refuses
// nothing, and still refuses an override value it does not know.
func TestTheWritingCheckOffReadsNothing(t *testing.T) {
	rig := newWritingCheckRig(t, nil)
	comments := "/api/v1/issues/" + rig.fixture.issue + "/comments"
	if response := rig.asSession(http.MethodPost, comments, map[string]any{"body": badProse}); response.Code != http.StatusCreated {
		t.Errorf("check off: %d %s, want 201", response.Code, response.Body.String())
	}
	if events := rig.events(); len(events) != 0 {
		t.Errorf("check off recorded %+v", events)
	}
	if response := rig.asSession(http.MethodPost, comments, map[string]any{"body": cleanProse, "override_writing_check": "yes"}); response.Code != http.StatusBadRequest {
		t.Errorf("check off, override yes: %d %s, want 400", response.Code, response.Body.String())
	}
}
