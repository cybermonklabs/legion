package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const (
	alice   = "alice@example.com"
	bob     = "bob@example.com"
	carol   = "carol@example.com"
	dave    = "dave@example.com"
	erin    = "erin@example.com"
	zed     = "zed@example.com"
	agentA  = `{"kind": "session", "id": "session-a"}`
	agentB  = `{"kind": "session", "id": "session-b"}`
	daveAct = `{"kind": "user", "id": "Dave@Example.com"}`
)

// person is the actor of a person writing for themselves.
func person(login string) string { return fmt.Sprintf(`{"kind": "user", "id": %q}`, login) }

func issuePayload(key, project, assignee string) string {
	return fmt.Sprintf(`{"key": %q, "project": %q, "parent": null, "assignee": %s, "title": "A title"}`, key, project, jsonOrNull(assignee))
}

func commentPayload(id, author, issue, artifact, answers, ask string) string {
	return fmt.Sprintf(`{"id": %q, "author": %s, "issue_key": %s, "artifact_id": %s, "reply_to": %s, "answers": %s, "ask_id": %s}`,
		id, author, jsonOrNull(issue), jsonOrNull(artifact), jsonOrNull(answers), jsonOrNull(answers), jsonOrNull(ask))
}

func messagePayload(id, author, target, issue, inReplyTo, broadcast string) string {
	return fmt.Sprintf(`{"id": %q, "author": %s, "target": %s, "issue_key": %s, "in_reply_to": %s, "broadcast_id": %s}`,
		id, author, jsonOrNull(target), jsonOrNull(issue), jsonOrNull(inReplyTo), jsonOrNull(broadcast))
}

func askEventPayload(id, author, issue, artifact, askedOf string) string {
	return fmt.Sprintf(`{"id": %q, "kind": "question", "author": %s, "issue_key": %s, "artifact_id": %s, "asked_of": %s}`,
		id, author, jsonOrNull(issue), jsonOrNull(artifact), jsonOrNull(askedOf))
}

func artifactPayload(id, issue, project, session string) string {
	return fmt.Sprintf(`{"artifact": {"id": %q, "kind": "doc", "issue_key": %s, "project": %s, "session_id": %s}}`,
		id, jsonOrNull(issue), jsonOrNull(project), jsonOrNull(session))
}

// readerLog is a log that exercises every reader rule. Project PROJ defaults to zed until
// event 40 moves it to carol; the log records no default for project NONE.
func readerLog() []event {
	return []event{
		ev(3, "issue.created", agentA, "PROJ-1", issuePayload("PROJ-1", "PROJ", alice)),
		ev(4, "issue.created", agentA, "PROJ-2", issuePayload("PROJ-2", "PROJ", "")),
		ev(5, "issue.created", agentA, "NONE-1", issuePayload("NONE-1", "NONE", "")),
		// A person's comment on the unassigned PROJ-2, and an agent's reply to it.
		ev(10, "comment.created", daveAct, "PROJ-2", commentPayload("c-dave", daveAct, "PROJ-2", "", "", "")),
		ev(11, "comment.created", agentA, "PROJ-2", commentPayload("c-reply", agentA, "PROJ-2", "", "c-dave", "")),
		// An agent's comment on PROJ-1 while alice holds it, a retitle that also hands it to bob
		// (read by alice, who held it when the request came), and a comment once bob holds it.
		ev(12, "comment.created", agentA, "PROJ-1", commentPayload("c-alice", agentA, "PROJ-1", "", "", "")),
		ev(13, "issue.updated", agentA, "PROJ-1", issuePayload("PROJ-1", "PROJ", bob)),
		ev(14, "comment.created", agentA, "PROJ-1", commentPayload("c-bob", agentA, "PROJ-1", "", "", "")),
		// A comment on the issue nobody holds in the project that names nobody.
		ev(15, "comment.created", agentA, "NONE-1", commentPayload("c-none", agentA, "NONE-1", "", "", "")),
		// A session sends to another, which replies: a conversation between two sessions.
		ev(20, "message.created", agentA, "", messagePayload("m-send", agentA, "session:session-b", "", "", "")),
		ev(21, "message.answered", agentB, "", messagePayload("m-ab", agentB, "session:session-b", "", "m-send", "")),
		// A person opens a conversation with session-b, which replies and puts a file there.
		ev(22, "message.created", daveAct, "", messagePayload("m-dave", daveAct, "session:session-b", "", "", "")),
		ev(23, "message.answered", agentB, "", messagePayload("m-bd", agentB, "session:session-b", "", "m-dave", "")),
		ev(24, "artifact.created", agentB, "", artifactPayload("d-conv", "", "", "session-b")),
		// A broadcast, and a message on an issue.
		ev(25, "message.created", agentA, "", messagePayload("m-cast", agentA, "session:session-b", "", "", "cast-1")),
		ev(26, "message.created", agentA, "PROJ-1", messagePayload("m-issue", agentA, "", "PROJ-1", "", "")),
		// An ask on PROJ-2, handed on to erin, then edited and answered.
		ev(30, "ask.opened", agentA, "PROJ-2", askEventPayload("a-1", agentA, "PROJ-2", "", "")),
		ev(31, "ask.asked_of", person(alice), "PROJ-2", askEventPayload("a-1", agentA, "PROJ-2", "", erin)),
		ev(32, "ask.edited", agentA, "PROJ-2", askEventPayload("a-1", agentA, "PROJ-2", "", erin)),
		ev(33, "ask.opened", daveAct, "PROJ-1", askEventPayload("a-dave", daveAct, "PROJ-1", "", "")),
		ev(34, "ask.answered", agentA, "PROJ-1", askEventPayload("a-dave", daveAct, "PROJ-1", "", "")),
		// Documents: a project's, before and after its default moves from zed to carol.
		ev(35, "artifact.created", agentA, "", artifactPayload("d-proj", "", "PROJ", "")),
		ev(40, "settings.project_default_assignee.updated", person(alice), "",
			fmt.Sprintf(`{"project": "PROJ", "assignee": %q, "thread_logins": [%q, %q]}`, carol, carol, zed)),
		ev(41, "artifact.version", agentA, "", `{"artifact_id": "d-proj", "version": {"number": 2}}`),
		ev(42, "result.recorded", agentA, "PROJ-2", `{"result_id": "r-1", "check_issue": "PROJ-2"}`),
		ev(43, "issue.created", agentA, "PROJ-3", issuePayload("PROJ-3", "PROJ", "")),
	}
}

func replayed(t *testing.T, events []event) *resolver {
	t.Helper()
	s := newReplayState(0)
	for _, e := range events {
		if err := s.apply(e); err != nil {
			t.Fatalf("apply %d: %v", e.ID, err)
		}
	}
	return &resolver{s: s, missing: map[string]int{}}
}

func TestTheReaderOfEachWriteIsTheServersRuleAtTheTimeOfTheWrite(t *testing.T) {
	r := replayed(t, readerLog())
	tests := []struct {
		name         string
		c            Case
		reader, rule string
	}{
		{"a reply to a person's comment on an unassigned issue is read by that person",
			Case{EventID: 11, EventType: "comment.created", Surface: surfaceComment, SourceID: "c-reply", Issue: "PROJ-2"}, dave, ruleComment},
		{"a comment is read by the assignee when it is written",
			Case{EventID: 12, EventType: "comment.created", Surface: surfaceComment, SourceID: "c-alice", Issue: "PROJ-1"}, alice, ruleComment},
		{"an update that reassigns the issue is read by the assignee before it",
			Case{EventID: 13, EventType: "issue.updated", Surface: surfaceIssueTitle, SourceID: "PROJ-1", Issue: "PROJ-1"}, alice, ruleIssue},
		{"a comment after the reassignment is read by the new assignee",
			Case{EventID: 14, EventType: "comment.created", Surface: surfaceComment, SourceID: "c-bob", Issue: "PROJ-1"}, bob, ruleComment},
		{"a comment where neither the issue nor the project names anybody is read by nobody",
			Case{EventID: 15, EventType: "comment.created", Surface: surfaceComment, SourceID: "c-none", Issue: "NONE-1"}, "", ruleComment},
		{"a send to an agent is read by nobody",
			Case{EventID: 20, EventType: "message.created", Surface: surfaceSendToAgent, SourceID: "m-send"}, "", ruleNone},
		{"a reply between two sessions is read by nobody",
			Case{EventID: 21, EventType: "message.answered", Surface: surfaceDeliveryReply, SourceID: "m-ab"}, "", ruleMessageReply},
		{"a session's reply to a person is read by that person",
			Case{EventID: 23, EventType: "message.answered", Surface: surfaceDeliveryReply, SourceID: "m-bd"}, dave, ruleMessageReply},
		{"a file in a person's conversation is read by that person",
			Case{EventID: 24, EventType: "artifact.created", Surface: surfaceDocument, SourceID: "d-conv@1", Document: "d-conv"}, dave, ruleConversation},
		{"a broadcast is read by nobody",
			Case{EventID: 25, EventType: "message.created", Surface: surfaceBroadcast, SourceID: "cast-1"}, "", ruleNone},
		{"a message on an issue is read by its assignee",
			Case{EventID: 26, EventType: "message.created", Surface: surfaceMessage, SourceID: "m-issue", Issue: "PROJ-1"}, bob, ruleMessage},
		{"a new ask on an unassigned issue is read by the project's default",
			Case{EventID: 30, EventType: "ask.opened", Surface: surfaceAskQuestion, SourceID: "a-1", Issue: "PROJ-2"}, zed, ruleIssue},
		{"an edit of an ask handed on is read by the person it waits on",
			Case{EventID: 32, EventType: "ask.edited", Surface: surfaceAskQuestion, SourceID: "a-1", Issue: "PROJ-2"}, erin, ruleAsk},
		{"an agent's answer to a person's ask is read by that person",
			Case{EventID: 34, EventType: "ask.answered", Surface: surfaceAskAnswer, SourceID: "a-dave", Issue: "PROJ-1"}, dave, ruleAskReply},
		{"a project document before the default moved is read by the default it replaced",
			Case{EventID: 35, EventType: "artifact.created", Surface: surfaceDocument, SourceID: "d-proj@1", Document: "d-proj"}, zed, ruleProject},
		{"a project document after the default moved is read by the new default",
			Case{EventID: 41, EventType: "artifact.version", Surface: surfaceDocument, SourceID: "d-proj@2", Document: "d-proj"}, carol, ruleDocument},
		{"a result on an unassigned issue is read by the project's default",
			Case{EventID: 42, EventType: "result.recorded", Surface: surfaceResultRun, SourceID: "r-1", Issue: "PROJ-2"}, carol, ruleIssue},
		{"a new issue created with nobody is read by the project's default",
			Case{EventID: 43, EventType: "issue.created", Surface: surfaceIssueTitle, SourceID: "PROJ-3", Issue: "PROJ-3"}, carol, ruleNewIssue},
		{"a new issue is read by the person it was created for",
			Case{EventID: 3, EventType: "issue.created", Surface: surfaceIssueTitle, SourceID: "PROJ-1", Issue: "PROJ-1"}, alice, ruleNewIssue},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader, rule, err := r.readerOf(tt.c)
			if err != nil || reader != tt.reader || rule != tt.rule {
				t.Errorf("reader = %q by %q (%v), want %q by %q", reader, rule, err, tt.reader, tt.rule)
			}
		})
	}
	if len(r.missing) > 0 {
		t.Errorf("records missing from the replay: %v", r.missing)
	}
}

// TestTheReaderOfWritesOnAnIssuesDocumentAndOfNewIssues: an issue's primary document (which no
// artifact.created announces), comments on an issue's document, an edit of one, a block ask, and
// new issues created with nobody, each read as the server reads it. The server reads the owner
// of a document's comment through its project alone (readAnsweredComment joins the comment's
// own issue_key, which a document's comment leaves null), so an edit is read by the project's
// default where a new comment is read by the issue's assignee.
func TestTheReaderOfWritesOnAnIssuesDocumentAndOfNewIssues(t *testing.T) {
	owned := `{"kind": "session", "id": "session-f", "owner": "Frank@Example.com"}`
	r := replayed(t, []event{
		ev(1, "settings.project_default_assignee.updated", person(alice), "", fmt.Sprintf(`{"project": "P", "assignee": %q, "thread_logins": [%q]}`, carol, carol)),
		ev(2, "issue.created", agentA, "P-1", `{"key": "P-1", "project": "P", "parent": null, "assignee": "alice@example.com", "primary_artifact_id": "d-spec"}`),
		ev(3, "artifact.created", agentA, "P-1", artifactPayload("d-issue", "P-1", "P", "")),
		ev(4, "comment.created", daveAct, "P-1", commentPayload("c-dave", daveAct, "", "d-issue", "", "")),
		ev(5, "comment.created", agentA, "P-1", commentPayload("c-reply", agentA, "", "d-issue", "c-dave", "")),
		ev(6, "comment.created", agentA, "P-1", commentPayload("c-agent", agentA, "", "d-issue", "", "")),
		ev(7, "comment.edited", agentA, "P-1", commentPayload("c-agent", agentA, "", "d-issue", "", "")),
		ev(8, "ask.opened", agentA, "P-1", `{"id": "a-block", "author": {"kind": "session", "id": "session-a"}, "issue_key": "P-1", "artifact_id": null, "block_id": "b-1", "asked_of": null}`),
		ev(9, "issue.created", owned, "P-2", issuePayload("P-2", "P", "")),
		ev(10, "issue.created", agentA, "P-3", `{"key": "P-3", "project": "P", "parent": "P-1", "assignee": null}`),
		ev(11, "artifact.version", agentA, "P-1", `{"artifact_id": "d-spec", "version": {"number": 2}}`),
	})
	for _, tt := range []struct {
		c            Case
		reader, rule string
	}{
		{Case{EventID: 5, EventType: "comment.created", Surface: surfaceComment, SourceID: "c-reply", Document: "d-issue"}, dave, ruleComment},
		{Case{EventID: 6, EventType: "comment.created", Surface: surfaceComment, SourceID: "c-agent", Document: "d-issue"}, alice, ruleComment},
		{Case{EventID: 7, EventType: "comment.edited", Surface: surfaceCommentEdit, SourceID: "c-agent", Document: "d-issue"}, carol, ruleCommentEdit},
		{Case{EventID: 8, EventType: "ask.opened", Surface: surfaceAskQuestion, SourceID: "a-block", Issue: "P-1"}, alice, ruleDocument},
		{Case{EventID: 9, EventType: "issue.created", Surface: surfaceIssueTitle, SourceID: "P-2", Issue: "P-2"}, "frank@example.com", ruleNewIssue},
		{Case{EventID: 10, EventType: "issue.created", Surface: surfaceIssueTitle, SourceID: "P-3", Issue: "P-3"}, alice, ruleNewIssue},
		{Case{EventID: 11, EventType: "artifact.version", Surface: surfaceDocument, SourceID: "d-spec@2", Document: "d-spec"}, alice, ruleDocument},
	} {
		reader, rule, err := r.readerOf(tt.c)
		if err != nil || reader != tt.reader || rule != tt.rule {
			t.Errorf("event %d %s: reader = %q by %q (%v), want %q by %q", tt.c.EventID, tt.c.EventType, reader, rule, err, tt.reader, tt.rule)
		}
	}
}

// TestEveryReaderRuleNameIsTheServers holds the rule names to the server's readerRule constants,
// so reader_rule names the rule the server's table names.
func TestEveryReaderRuleNameIsTheServers(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "..", "internal", "dispatch", "api", "writing_check_reader.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	server := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok || spec.Type == nil || fmt.Sprint(spec.Type) != "readerRule" {
			return true
		}
		for _, v := range spec.Values {
			if lit, ok := v.(*ast.BasicLit); ok {
				name, _ := strconv.Unquote(lit.Value)
				server[name] = true
			}
		}
		return true
	})
	for _, rule := range []string{ruleNewIssue, ruleIssue, ruleProject, ruleDocument, ruleConversation, ruleAsk, ruleAskReply,
		ruleComment, ruleCommentReply, ruleCommentEdit, ruleMessage, ruleMessageReply, ruleNone} {
		if !server[rule] {
			t.Errorf("rule %q is not one of the server's reader rules %v", rule, sortedKeys(server))
		}
	}
}

// TestReadersReplaysTheLogOnceAndWritesEveryCaseWithItsReader runs the subcommand against a log
// served over HTTP, then again with the server gone: the second run reads the saved state.
func TestReadersReplaysTheLogOnceAndWritesEveryCaseWithItsReader(t *testing.T) {
	events := readerLog()
	var pages int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/events/log" {
			http.NotFound(w, r)
			return
		}
		pages++
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		page := []json.RawMessage{}
		for _, e := range events {
			if e.ID > after && len(page) < limit {
				b, _ := json.Marshal(e)
				page = append(page, b)
			}
		}
		json.NewEncoder(w).Encode(page)
	}))
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DISPATCH_URL", srv.URL)
	t.Setenv("DISPATCH_TOKEN_FILE", token)
	in := filepath.Join(dir, "cases.jsonl")
	cases := []Case{
		{EventID: 11, EventType: "comment.created", Project: "PROJ", Issue: "PROJ-2", Surface: surfaceComment, Field: "body", SourceID: "c-reply", Text: "One.", Group: "g1"},
		{EventID: 20, EventType: "message.created", Project: "", Surface: surfaceSendToAgent, Field: "body", SourceID: "m-send", Text: "Two."},
		{EventID: 30, EventType: "ask.opened", Project: "PROJ", Issue: "PROJ-2", Surface: surfaceAskQuestion, Field: "question", SourceID: "a-1", Text: "Three?"},
		{EventID: 30, EventType: "ask.opened", Project: "PROJ", Issue: "PROJ-2", Surface: surfaceAskOption, Field: "options[0].label", SourceID: "a-1", Text: "Yes"},
	}
	if _, err := writeJSONL(in, cases); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state.json")
	out := filepath.Join(dir, "out.jsonl")
	args := []string{"readers", "--in", in, "--log-after", "0", "--log-through", "43", "--out", out, "--state", state}
	code, stdout, stderr := runCmd(t, args...)
	if code != 0 {
		t.Fatalf("readers = %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "writes: 3, with a person reader 2 (66.7%), with none 1") ||
		!strings.Contains(stdout, "| ask | 2 | 2 | 0 | 1 | 1 | 0 | 100.0% |") ||
		!strings.Contains(stdout, "| send_to_agent | 1 | 0 | 1 | 1 | 0 | 1 | 0.0% |") ||
		!strings.Contains(stdout, "distinct readers: 2") {
		t.Errorf("summary:\n%s", stdout)
	}
	srv.Close()
	served := pages
	if code, _, stderr := runCmd(t, args...); code != 0 || pages != served {
		t.Fatalf("second run = %d after %d more pages: %s", code, pages-served, stderr)
	}
	got, err := readCases(out)
	if err != nil {
		t.Fatal(err)
	}
	var readers []string
	for i, c := range got {
		if c.Reader == nil {
			t.Fatalf("case %d has no reader", i)
		}
		readers = append(readers, *c.Reader+"/"+c.ReaderRule)
		if c.Text != cases[i].Text || c.Group != cases[i].Group {
			t.Errorf("case %d changed: %+v", i, c)
		}
	}
	if want := []string{dave + "/comment", "/none", zed + "/issue", zed + "/issue"}; !slices.Equal(readers, want) {
		t.Errorf("readers = %v, want %v", readers, want)
	}
	// A case past the replayed window is refused.
	if _, err := writeJSONL(in, []Case{{EventID: 44, EventType: "comment.created", Surface: surfaceComment, SourceID: "c-x"}}); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCmd(t, args...); code != 1 || !strings.Contains(stderr, "outside the replayed window") {
		t.Errorf("a case past the window = %d: %s", code, stderr)
	}
}
