package api

import (
	"fmt"
	"go/ast"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// One expression decides whose list an ask is in: askedOfPersonExpression, the person somebody
// handed the ask to and the assignee of its issue until somebody does. This file is the Go half
// of dispatch://INFRA-1576's c-one-expression-decides, whose `run` names
// TestOneExpressionDecidesWhoWaits, so the test and the file carry that name rather than the
// package's sentence-style one.

// inboxFor reads one person's Inbox rows through the person filter `dispatch inbox` sends
// (?assignee=me), or through filter where the test names another.
func inboxFor(t *testing.T, handler http.Handler, login, filter string) []inboxAsk {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox?assignee="+filter, nil, login)
	if response.Code != http.StatusOK {
		t.Fatalf("read %s's inbox (?assignee=%s): status=%d body=%s", login, filter, response.Code, response.Body.String())
	}
	return decodeBody[[]inboxAsk](t, response)
}

// inboxRowsOf counts how many of the read's rows are the ask, so a row listed twice is told from
// one listed once.
func inboxRowsOf(rows []inboxAsk, askID string) int {
	listed := 0
	for _, row := range rows {
		if row.ID == askID {
			listed++
		}
	}
	return listed
}

// authorOpenAsks is the author's read keyed by ask: the rows `dispatch open-asks` renders, which
// carry waiting_on_person. listOpenAsks, the driver beside it, reads the same route for
// everything else a row holds.
func authorOpenAsks(t *testing.T, handler http.Handler, sessionID string) map[string]openAsk {
	t.Helper()
	response := sessionRequest(t, handler, http.MethodGet, "/api/v1/asks/open?author_session="+sessionID, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("read the author's open asks: status=%d body=%s", response.Code, response.Body.String())
	}
	rows := map[string]openAsk{}
	for _, ask := range decodeBody[openAsksResponse](t, response).Asks {
		rows[ask.ID] = ask
	}
	return rows
}

// specIssue creates an issue nobody holds, with a spec of its own. The create sends assignee
// null, which parseIssueAssignee reads as a value the caller gave (api/issue_assignee.go), so
// no step of the create's own chain fills the field in and the question the spec opens stands
// on an issue that holds nobody.
func specIssue(t *testing.T, handler http.Handler, project, title, spec string) model.Issue {
	t.Helper()
	response := agentRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": project, "title": title, "spec": spec,
		"actor": sessionActor(), "assignee": nil,
	}, "agent-token")
	if response.Code != http.StatusCreated {
		t.Fatalf("create %s: status=%d body=%s", title, response.Code, response.Body.String())
	}
	return decodeBody[model.Issue](t, response)
}

// blockAskOf is the id of the ask the document settler wrote for the `:::ask` block blockID of
// the document artifactID (docs/ask_blocks.go), read off the Inbox, which lists every open
// question whatever owns it.
func blockAskOf(t *testing.T, handler http.Handler, artifactID, blockID string) string {
	t.Helper()
	for _, row := range inboxFor(t, handler, "alice", "") {
		if row.BlockID != nil && *row.BlockID == blockID &&
			row.BlockArtifact != nil && row.BlockArtifact.ID == artifactID {
			return row.ID
		}
	}
	t.Fatalf("no ask stands for the %q block of %s", blockID, artifactID)
	return ""
}

// personTheInboxNames is the person the unfiltered Inbox read says the question waits on.
func personTheInboxNames(t *testing.T, handler http.Handler, askID string) string {
	t.Helper()
	for _, row := range inboxFor(t, handler, "alice", "") {
		if row.ID == askID {
			return row.WaitingOnPerson
		}
	}
	t.Fatalf("the inbox does not list %s", askID)
	return ""
}

// personOneAskReadNames is the person GET /api/v1/asks/{id} says the question waits on: the
// single-ask read of the column list every ask read shares (askReadColumns).
func personOneAskReadNames(t *testing.T, handler http.Handler, askID string) string {
	t.Helper()
	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read ask %s: status=%d body=%s", askID, read.Code, read.Body.String())
	}
	return decodeBody[struct {
		Ask model.Ask `json:"ask"`
	}](t, read).Ask.WaitingOnPerson
}

// personAnAskListNames is the person the ask list at target says the question waits on: an
// issue's list or a document's, both over the shared column list.
func personAnAskListNames(t *testing.T, handler http.Handler, target, askID string) string {
	t.Helper()
	listed := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("read %s: status=%d body=%s", target, listed.Code, listed.Body.String())
	}
	for _, ask := range decodeBody[[]model.Ask](t, listed) {
		if ask.ID == askID {
			return ask.WaitingOnPerson
		}
	}
	t.Fatalf("%s does not list %s", target, askID)
	return ""
}

// personTheDecisionsReadNames is the person the issue's decisions read says its spec's open
// question waits on; its open_questions carry the shared column list too (api/decisions.go).
func personTheDecisionsReadNames(t *testing.T, handler http.Handler, issueKey, askID string) string {
	t.Helper()
	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issueKey+"/decisions", nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read %s's decisions: status=%d body=%s", issueKey, read.Code, read.Body.String())
	}
	for _, question := range decodeBody[struct {
		OpenQuestions []model.Ask `json:"open_questions"`
	}](t, read).OpenQuestions {
		if question.ID == askID {
			return question.WaitingOnPerson
		}
	}
	t.Fatalf("the decisions read of %s does not hold %s", issueKey, askID)
	return ""
}

// askedOfPersonExpressionName is the identifier every read must name instead of spelling the
// rule again.
const askedOfPersonExpressionName = "askedOfPersonExpression"

// namesTheExpression reports whether a declaration or a function body mentions that identifier.
func namesTheExpression(node ast.Node) bool {
	named := false
	ast.Inspect(node, func(inner ast.Node) bool {
		if identifier, ok := inner.(*ast.Ident); ok && identifier.Name == askedOfPersonExpressionName {
			named = true
		}
		return !named
	})
	return named
}

func TestOneExpressionDecidesWhoWaits(t *testing.T) {
	handler := newTestHandler(t)
	// alice and bob are the two people the test server records as having signed in
	// (server_test.go); the asks below are all opened by one author session, so the author's
	// read answers for every one of them.
	const author = "session-0123456789abcdef"
	bobsIssue := createInteractionIssue(t, handler, "WAITS", "Bob's issue", "A spec")
	if moved := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+bobsIssue.Key,
		map[string]any{"assignee": "bob"}, "alice"); moved.Code != http.StatusOK {
		t.Fatalf("assign %s to bob: status=%d body=%s", bobsIssue.Key, moved.Code, moved.Body.String())
	}
	createIssue := func(title string, body map[string]any) string {
		t.Helper()
		body["project"], body["title"] = "WAITS", title
		var response *httptest.ResponseRecorder
		if body["actor"] != nil {
			response = agentRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "agent-token")
		} else {
			response = dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "alice")
		}
		if response.Code != http.StatusCreated {
			t.Fatalf("create %s: status=%d body=%s", title, response.Code, response.Body.String())
		}
		return decodeBody[model.Issue](t, response).Key
	}
	alicesIssue := createIssue("Alice's issue", map[string]any{"assignee": "alice"})
	// An issue nobody holds: the create sends assignee null, which parseIssueAssignee reads as
	// a value the caller gave, so no step of the create's own chain fills it in.
	nobodysIssue := createIssue("Nobody's issue", map[string]any{"actor": sessionActor(), "assignee": nil})
	// The assignee the API was given in mixed case, stored as the canonical login both sides of
	// the expression compare with.
	mixedCaseIssue := createIssue("Alice's issue, written loudly", map[string]any{"assignee": "  ALICE  "})
	document := createProjectDocument(t, handler, "WAITS", "Design notes", "# Design notes\n")

	// Alice writes in a thread, then the author answers her, so the ask is a Reply row of hers
	// unless the Inbox already lists it.
	writeInThread := func(owner, askID string) {
		t.Helper()
		for _, reply := range []struct {
			body  map[string]any
			login string
		}{
			{body: map[string]any{"body": "Could you clarify?", "ask_id": askID}, login: "alice"},
			{body: map[string]any{"body": "Here is the clarification.", "ask_id": askID, "actor": sessionActor()}},
		} {
			var response *httptest.ResponseRecorder
			if reply.login == "" {
				response = sessionRequest(t, handler, http.MethodPost, owner+"/comments", reply.body)
			} else {
				response = dispatchRequest(t, handler, http.MethodPost, owner+"/comments", reply.body, reply.login)
			}
			if response.Code != http.StatusCreated {
				t.Fatalf("reply in the thread of %s: status=%d body=%s", askID, response.Code, response.Body.String())
			}
		}
	}

	handedToAlice := createIssueAsk(t, handler, bobsIssue.Key, "Handed to alice")
	handAskOn(t, handler, handedToAlice, "alice", "bob")
	nobodyHandedOn := createIssueAsk(t, handler, bobsIssue.Key, "Nobody handed it on")
	handedAway := createIssueAsk(t, handler, alicesIssue, "Handed away from alice")
	writeInThread("/api/v1/issues/"+alicesIssue, handedAway)
	handAskOn(t, handler, handedAway, "bob", "alice")
	onNobodysIssue := createIssueAsk(t, handler, nobodysIssue, "On nobody's issue")
	mixedCase := createIssueAsk(t, handler, mixedCaseIssue, "On the loudly assigned issue")
	writeInThread("/api/v1/issues/"+mixedCaseIssue, mixedCase)
	documentAsk := createSnoozeAsk(t, handler, "/api/v1/artifacts/"+document.ID+"/asks", "Handed to alice on a document")
	writeInThread("/api/v1/artifacts/"+document.ID, documentAsk)
	handAskOn(t, handler, documentAsk, "alice", "alice")
	documentUnhanded := createSnoozeAsk(t, handler, "/api/v1/artifacts/"+document.ID+"/asks", "On a document nobody was handed")
	// A question on an issue nobody holds, handed to the very person that issue's project
	// defaults to: the hand-on stores the login instead of emptying the field, since only the
	// issue's own assignee empties it (putAskAskedOf), so the question stops following the
	// default and a later default moves it nowhere.
	handedToTheDefault := createIssueAsk(t, handler, nobodysIssue, "Handed to the project's own default")
	if moved := handAskOn(t, handler, handedToTheDefault, "alice", "bob"); moved.AskedOf == nil || *moved.AskedOf != "alice" {
		t.Fatalf("handing a question to the person its project defaults to stored %v, want alice stored",
			moved.AskedOf)
	}
	// A question the spec's own `:::ask` block opened (docs/ask_blocks.go), on an issue nobody
	// holds: the decisions read answers the spec's own open questions and nothing else, so it
	// needs one of these to answer at all.
	decisionsIssue := specIssue(t, handler, "WAITS", "Nobody's decisions",
		"Context\n\n:::ask{#chain urgency=\"med\" multiple=\"false\"}\nWhich chain decides?\n:::\n")
	awaitIndexedAskBlock(t, handler, decisionsIssue.PrimaryArtifactID, "chain", "Which chain decides?")
	blockQuestion := blockAskOf(t, handler, decisionsIssue.PrimaryArtifactID, "chain")
	writeInThread("/api/v1/issues/"+decisionsIssue.Key, blockQuestion)

	// A hand-on moves the row between the two Inboxes and moves no other person's rows.
	t.Run("a handed-on ask is the person's it was handed to", func(t *testing.T) {
		alices, bobs := inboxFor(t, handler, "alice", "me"), inboxFor(t, handler, "bob", "me")
		if inboxRowsOf(alices, handedToAlice) != 1 || inboxRowsOf(bobs, handedToAlice) != 0 {
			t.Fatalf("the ask handed to alice is in alice's inbox %d times and bob's %d, want once and never",
				inboxRowsOf(alices, handedToAlice), inboxRowsOf(bobs, handedToAlice))
		}
		if inboxRowsOf(bobs, nobodyHandedOn) != 1 || inboxRowsOf(alices, nobodyHandedOn) != 0 {
			t.Fatalf("the ask nobody handed on is in bob's inbox %d times and alice's %d, want once and never",
				inboxRowsOf(bobs, nobodyHandedOn), inboxRowsOf(alices, nobodyHandedOn))
		}
		// The driver every other ask test reads the Inbox through sees the same row.
		if row := readInboxRow(t, handler, handedToAlice); row.ID != handedToAlice {
			t.Fatalf("alice's unfiltered inbox row = %#v, want the ask handed to her", row)
		}
	})

	// The exclusion reverses as well as moves: an ask handed away from the caller is no longer
	// their Inbox row, so the Replies list, which leaves out exactly what the Inbox lists, holds
	// it again.
	t.Run("an ask handed away leaves the inbox and enters the replies list", func(t *testing.T) {
		alices := inboxFor(t, handler, "alice", "me")
		if inboxRowsOf(alices, handedAway) != 0 {
			t.Fatalf("the ask alice handed away is still in her inbox %d times, want never", inboxRowsOf(alices, handedAway))
		}
		if inboxRowsOf(inboxFor(t, handler, "bob", "me"), handedAway) != 1 {
			t.Fatalf("the ask alice handed to bob is not in bob's inbox")
		}
		if !listsThread(getMyReplies(t, handler, "alice", ""), handedAway) {
			t.Fatalf("alice's replies do not hold the ask she handed away: %#v", getMyReplies(t, handler, "alice", "").Rows)
		}
	})

	// The pair that disagreed in dispatch://INFRA-1687: a document ask one person was handed is
	// their Inbox row, so it is not also a Reply row of theirs.
	t.Run("a handed-on document ask is an inbox row and not a reply row", func(t *testing.T) {
		if listed := inboxRowsOf(inboxFor(t, handler, "alice", "me"), documentAsk); listed != 1 {
			t.Fatalf("the document ask handed to alice is in her inbox %d times, want once", listed)
		}
		if page := getMyReplies(t, handler, "alice", ""); listsThread(page, documentAsk) {
			t.Fatalf("the document ask handed to alice is in her replies too: %#v", page.Rows)
		}
	})

	// The caller the assignee was written to in mixed case is the caller the stored login names,
	// so the exclusion reaches them: the expression compares canonical logins with `=`.
	t.Run("an assignee written in mixed case is still excluded from the replies list", func(t *testing.T) {
		if listed := inboxRowsOf(inboxFor(t, handler, "alice", "me"), mixedCase); listed != 1 {
			t.Fatalf("the ask on the loudly assigned issue is in alice's inbox %d times, want once", listed)
		}
		if page := getMyReplies(t, handler, "alice", ""); listsThread(page, mixedCase) {
			t.Fatalf("the ask on the loudly assigned issue is in alice's replies too: %#v", page.Rows)
		}
	})

	// Every question reaches a person: the one on an issue nobody holds and the one on a
	// project document both reach the person their project defaults to, so the filter for the
	// questions nobody waits on answers nothing at all.
	t.Run("the unassigned filter answers nothing, since every question reaches a person", func(t *testing.T) {
		if rows := inboxFor(t, handler, "alice", "unassigned"); len(rows) != 0 {
			t.Fatalf("the unassigned read holds %d rows, want none: %#v", len(rows), rows)
		}
		mine := inboxFor(t, handler, "alice", "me")
		for _, standing := range []struct {
			ask  string
			name string
		}{
			{onNobodysIssue, "the question on the issue nobody holds"},
			{documentUnhanded, "the question on a project document nobody was handed"},
		} {
			if listed := inboxRowsOf(mine, standing.ask); listed != 1 {
				t.Fatalf("%s is in alice's inbox %d times, want once: WAITS defaults to her",
					standing.name, listed)
			}
		}
	})

	// Every read resolves the person itself, and the author's read names the same person the
	// Inbox row does. No row waits on nobody: a question on an issue nobody holds and one on a
	// project document both name the person their project defaults to.
	t.Run("each row carries the person the server resolved", func(t *testing.T) {
		waiting := map[string]string{
			handedToAlice:      "alice",
			nobodyHandedOn:     "bob",
			handedAway:         "bob",
			mixedCase:          "alice",
			documentAsk:        "alice",
			onNobodysIssue:     "alice",
			documentUnhanded:   "alice",
			handedToTheDefault: "alice",
		}
		rows := map[string]inboxAsk{}
		for _, row := range inboxFor(t, handler, "alice", "") {
			rows[row.ID] = row
		}
		authors := authorOpenAsks(t, handler, author)
		for askID, person := range waiting {
			row, listed := rows[askID]
			if !listed || row.WaitingOnPerson != person {
				t.Fatalf("the inbox row of %s says it waits on %q (listed=%t), want %q",
					askID, row.WaitingOnPerson, listed, person)
			}
			if got := authors[askID]; got.WaitingOnPerson != person {
				t.Fatalf("the author's read of %s says it waits on %q, want %q", askID, got.WaitingOnPerson, person)
			}
		}
		for _, row := range rows {
			if row.WaitingOnPerson == "" {
				t.Fatalf("the inbox row of %s waits on nobody; every question reaches a person", row.ID)
			}
		}
		// The driver the other author-read tests use answers the same rows.
		listed := listOpenAsks(t, handler, author, "")
		if listed.Count != len(authors) {
			t.Fatalf("the author's read lists %d asks, want the %d the person read answered", listed.Count, len(authors))
		}
		for _, ask := range listed.Asks {
			if _, known := authors[ask.ID]; !known {
				t.Fatalf("the author's read lists %s, which the read beside it did not", ask.ID)
			}
		}
	})

	// No read holds a copy of the rule: each of the six names the one constant, and the SQL it
	// holds - the expression and the three joins it reads over - is spelled once in the module.
	// The precedent for reading the source rather than the behaviour is api/lock_level_test.go.
	t.Run("the six reads share one expression", func(t *testing.T) {
		sources := moduleSources(t, "../../..")
		declarations := map[string]ast.Node{}
		for _, source := range sources {
			if filepath.Base(filepath.Dir(source.path)) != "api" {
				continue
			}
			for _, declared := range source.file.Decls {
				switch node := declared.(type) {
				case *ast.FuncDecl:
					declarations[node.Name.Name+"'s query"] = node
				case *ast.GenDecl:
					for _, spec := range node.Specs {
						value, ok := spec.(*ast.ValueSpec)
						if !ok {
							continue
						}
						for _, name := range value.Names {
							declarations[name.Name] = value
						}
					}
				}
			}
		}
		for _, read := range []string{
			"askReadColumns", "listInbox's query", "listOpenAsks's query", "myRepliesOwner",
			"putAskAskedOf's query", "askWaitingOnPerson's query",
		} {
			node, declared := declarations[read]
			if !declared {
				t.Fatalf("%s is no longer declared in the api package; the reads this check holds have moved", read)
			}
			if !namesTheExpression(node) {
				t.Fatalf("%s does not name %s: a read with its own copy of the rule answers a different person",
					read, askedOfPersonExpressionName)
			}
		}

		// Two spellings are each counted across the module, with _test.go files skipped by
		// moduleSources, so the needles below are not found in this file's own literals.
		//
		// The rule's needle is the prefix `coalesce(a.asked_of` rather than the whole
		// expression: a read that reintroduced the two-step `coalesce(a.asked_of, i.assignee)`,
		// the spelling that sends a question standing on an issue nobody holds to nobody, is
		// what this count exists to catch, and the whole expression would count it zero times
		// and pass.
		//
		// The joins the rule needs are counted by the one that makes them its own, the project
		// reached through either owner: the other two left joins are ordinary beside any ask.
		copies := []string{}
		joins := []string{}
		for _, source := range sources {
			text, err := os.ReadFile(source.path)
			if err != nil {
				t.Fatalf("read %s: %v", source.path, err)
			}
			if spelled := strings.Count(string(text), "coalesce(a.asked_of"); spelled > 0 {
				copies = append(copies, fmt.Sprintf("%s (%d)", filepath.ToSlash(source.path), spelled))
			}
			joined := strings.Count(string(text), "left join projects p on p.key = coalesce(i.project_key, ar.project_key)")
			if joined > 0 {
				joins = append(joins, fmt.Sprintf("%s (%d)", filepath.ToSlash(source.path), joined))
			}
		}
		slices.Sort(copies)
		slices.Sort(joins)
		if want := []string{"../../../internal/dispatch/api/ask_rows.go (1)"}; !slices.Equal(copies, want) {
			t.Fatalf("the rule is spelled in %v, want it written once, in %v", copies, want)
		}
		if want := []string{"../../../internal/dispatch/api/ask_rows.go (1)"}; !slices.Equal(joins, want) {
			t.Fatalf("the joins the rule reads are spelled in %v, want them written once, in %v", joins, want)
		}
	})

	// The whole chain through every read that names a person, on one question: the Inbox read,
	// the author's open-questions read, the Replies exclusion, the hand-on route's answer, and
	// the reads over the shared column list - one ask, an issue's ask list, a document's ask
	// list and the decisions read. The question stands on an issue nobody holds and nobody was
	// handed it, so every one of them resolves the third step and names the project's default.
	t.Run("every read names the person the project defaults to", func(t *testing.T) {
		for read, named := range map[string]string{
			"the Inbox read":              personTheInboxNames(t, handler, blockQuestion),
			"the author's open questions": authorOpenAsks(t, handler, author)[blockQuestion].WaitingOnPerson,
			"the single ask read":         personOneAskReadNames(t, handler, blockQuestion),
			"the issue's ask list":        personAnAskListNames(t, handler, "/api/v1/issues/"+decisionsIssue.Key+"/asks", blockQuestion),
			"the decisions read":          personTheDecisionsReadNames(t, handler, decisionsIssue.Key, blockQuestion),
			"the document's ask list": personAnAskListNames(t, handler,
				"/api/v1/artifacts/"+document.ID+"/asks", documentUnhanded),
		} {
			if named != "alice" {
				t.Fatalf("%s says the question waits on %q, want alice, the default of its project", read, named)
			}
		}
		// The Replies exclusion is the same expression read the other way round: alice wrote in
		// the thread, and the Inbox lists the question under her, so her Replies list leaves it
		// out until somebody hands it away from her.
		if page := getMyReplies(t, handler, "alice", ""); listsThread(page, blockQuestion) {
			t.Fatalf("the question WAITS defaults to alice is in her replies too: %#v", page.Rows)
		}
		if moved := handAskOn(t, handler, blockQuestion, "bob", "alice"); moved.WaitingOnPerson != "bob" {
			t.Fatalf("the hand-on route says the question waits on %q, want bob", moved.WaitingOnPerson)
		}
		if page := getMyReplies(t, handler, "alice", ""); !listsThread(page, blockQuestion) {
			t.Fatalf("the question alice handed to bob is not back in her replies: %#v", page.Rows)
		}
	})

	// Writing an assignee onto an issue nobody held moves every question standing on it off the
	// project's default, and moves no question somebody was handed: that one names its person
	// outright.
	t.Run("an assignee written onto the issue moves the questions standing on it", func(t *testing.T) {
		if moved := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+nobodysIssue,
			map[string]any{"assignee": "bob"}, "alice"); moved.Code != http.StatusOK {
			t.Fatalf("assign %s to bob: status=%d body=%s", nobodysIssue, moved.Code, moved.Body.String())
		}
		if named := personTheInboxNames(t, handler, onNobodysIssue); named != "bob" {
			t.Fatalf("the question on the issue bob now holds waits on %q, want bob", named)
		}
		if named := personTheInboxNames(t, handler, handedToTheDefault); named != "alice" {
			t.Fatalf("the question handed to alice waits on %q once its issue moved to bob, want alice", named)
		}
	})

	// The last step moves as the other two do, and a question handed on does not follow it.
	t.Run("a question handed on keeps its person when the project's default moves", func(t *testing.T) {
		if moved := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/WAITS/default-assignee",
			map[string]any{"assignee": "bob"}, "alice"); moved.Code != http.StatusOK {
			t.Fatalf("move WAITS's default to bob: status=%d body=%s", moved.Code, moved.Body.String())
		}
		if named := personTheInboxNames(t, handler, documentUnhanded); named != "bob" {
			t.Fatalf("the document question nobody was handed waits on %q, want WAITS's new default bob", named)
		}
		if named := personTheInboxNames(t, handler, documentAsk); named != "alice" {
			t.Fatalf("the document question handed to alice waits on %q once the default moved, want alice", named)
		}
	})
}
