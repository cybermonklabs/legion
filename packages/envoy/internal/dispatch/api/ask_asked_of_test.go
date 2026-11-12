package api

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The field an ask waits on a person by, and who may write it: PUT /api/v1/asks/{id}/asked-of.
// This file is the Go half of dispatch://INFRA-1576's c-no-agent-picks-a-person, whose `run`
// names TestAskAskedOf, so the test and the file carry that name rather than the package's
// sentence-style one.

// handAskTo is the hand-on route as a person drives it, with the address they name.
func handAskTo(t *testing.T, handler http.Handler, askID, askedOf, login string) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodPut, "/api/v1/asks/"+askID+"/asked-of",
		map[string]any{"asked_of": askedOf}, login)
}

// handAskOn hands the ask on and fails unless the route took it.
func handAskOn(t *testing.T, handler http.Handler, askID, askedOf, login string) model.Ask {
	t.Helper()
	moved := handAskTo(t, handler, askID, askedOf, login)
	if moved.Code != http.StatusOK {
		t.Fatalf("hand ask %s to %q: status=%d body=%s", askID, askedOf, moved.Code, moved.Body.String())
	}
	return decodeBody[model.Ask](t, moved)
}

// askedOfColumns are the three stored columns of one ask, read from the row itself: what the
// route wrote, rather than what a read of it derived.
func askedOfColumns(t *testing.T, database *store.Store, askID string) (askedOf, askedOfBy *string, askedOfAt *time.Time) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `
		select asked_of, asked_of_by, asked_of_at from asks where id = $1
	`, askID).Scan(&askedOf, &askedOfBy, &askedOfAt); err != nil {
		t.Fatalf("read the asked_of columns of %s: %v", askID, err)
	}
	return askedOf, askedOfBy, askedOfAt
}

// askedOfPerson is the person a read of the ask says it waits on (waiting_on_person), and the
// stored asked_of beside it.
func askedOfPerson(t *testing.T, handler http.Handler, askID string) (waitingOn string, askedOf *string) {
	t.Helper()
	ask := readBlockAsk(t, handler, askID)
	return ask.WaitingOnPerson, ask.AskedOf
}

// askEventPayloads are the stored payloads of one issue's `ask.*` events, by type, oldest first.
// The stored row is what every consumer of an event reads, so it is the oracle for what an event
// carries.
func askEventPayloads(t *testing.T, database *store.Store, issueKey string) map[string][]map[string]json.RawMessage {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select type, payload from events where issue_key = $1 and type like 'ask.%' order by seq
	`, issueKey)
	if err != nil {
		t.Fatalf("read the ask events of %s: %v", issueKey, err)
	}
	defer rows.Close()
	payloads := map[string][]map[string]json.RawMessage{}
	for rows.Next() {
		var eventType string
		var raw []byte
		if err := rows.Scan(&eventType, &raw); err != nil {
			t.Fatalf("scan an ask event of %s: %v", issueKey, err)
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("decode the %s payload of %s: %v", eventType, issueKey, err)
		}
		payloads[eventType] = append(payloads[eventType], payload)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the ask events of %s: %v", issueKey, err)
	}
	return payloads
}

// askEventCensus is the census of ask events the check names: every `ask.` event type
// api/events.go hydrates as an ask payload, which is what api/events_test.go's askEventTypes
// mirrors. It is read out of that source rather than copied here, so an ask event type added
// there is one this test demands the field of rather than one it quietly skips.
func askEventCensus(t *testing.T) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "events.go", nil, 0)
	if err != nil {
		t.Fatalf("parse api/events.go: %v", err)
	}
	census := []string{}
	ast.Inspect(file, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		text, err := strconv.Unquote(literal.Value)
		if err != nil || !strings.HasPrefix(text, "ask.") || slices.Contains(census, text) {
			return true
		}
		census = append(census, text)
		return true
	})
	slices.Sort(census)
	if len(census) == 0 {
		t.Fatal("api/events.go names no ask event type; the census this test reads is gone")
	}
	return census
}

// sqlWithoutComments is one migration file's SQL with its `--` comments taken off, so the
// statements can be counted and read.
func sqlWithoutComments(text string) string {
	lines := []string{}
	for _, line := range strings.Split(text, "\n") {
		if comment := strings.Index(line, "--"); comment >= 0 {
			line = line[:comment]
		}
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// sqlStatementsOf splits comment-free SQL into its statements, the trailing semicolon dropped.
func sqlStatementsOf(text string) []string {
	statements := []string{}
	for _, statement := range strings.Split(sqlWithoutComments(text), ";") {
		if trimmed := strings.TrimSpace(statement); trimmed != "" {
			statements = append(statements, trimmed)
		}
	}
	return statements
}

// rowWritingStatement matches a statement that writes a row, which the migration must not hold.
var rowWritingStatement = regexp.MustCompile(`(?i)\b(insert|update|delete|truncate|copy)\b`)

func TestAskAskedOf(t *testing.T) {
	// Handing a question to a person is one person asking another, so the route is the person's
	// alone: the deployment's shared bearer holds no person to hand one from.
	t.Run("the shared agent bearer and an anonymous caller are refused", func(t *testing.T) {
		handler, database := newTestHandlerWithStore(t)
		issue := createInteractionIssue(t, handler, "SCOPE", "Who may hand an ask on", "A spec")
		askID := createIssueAsk(t, handler, issue.Key, "Which transport?")
		target := "/api/v1/asks/" + askID + "/asked-of"

		bearer := sessionRequest(t, handler, http.MethodPut, target, map[string]any{"asked_of": "bob"})
		if bearer.Code != http.StatusForbidden || !strings.Contains(bearer.Body.String(), `"code":"`+codeScopeRequired+`"`) {
			t.Fatalf("the deployment's shared bearer: status=%d body=%s, want 403 %s",
				bearer.Code, bearer.Body.String(), codeScopeRequired)
		}
		if anonymous := handAskTo(t, handler, askID, "bob", ""); anonymous.Code != http.StatusUnauthorized {
			t.Fatalf("an anonymous caller: status=%d body=%s, want 401", anonymous.Code, anonymous.Body.String())
		}
		askedOf, by, at := askedOfColumns(t, database, askID)
		if askedOf != nil || by != nil || at != nil {
			t.Fatalf("the refused ask holds asked_of=%v asked_of_by=%v asked_of_at=%v, want all three empty", askedOf, by, at)
		}
	})

	// A person's own credential moves it, and the address they wrote is stored as the canonical
	// login both sides of askedOfPersonExpression compare with.
	t.Run("a person-scoped caller moves the ask and the login is canonical", func(t *testing.T) {
		handler, database := newTestHandlerWithStore(t)
		seedPeople(t, database, "bob@example.com")
		issue := createInteractionIssue(t, handler, "MOVE", "A question for somebody else", "A spec")
		askID := createIssueAsk(t, handler, issue.Key, "Which transport?")
		token := personalTokenHolding(t, database, model.ScopePerson)

		moved := agentRequest(t, handler, http.MethodPut, "/api/v1/asks/"+askID+"/asked-of",
			map[string]any{"asked_of": "  BoB@Example.COM  "}, token)
		if moved.Code != http.StatusOK {
			t.Fatalf("hand the ask on with a person's token: status=%d body=%s", moved.Code, moved.Body.String())
		}
		answered := decodeBody[model.Ask](t, moved)
		if answered.AskedOf == nil || *answered.AskedOf != "bob@example.com" ||
			answered.AskedOfBy == nil || *answered.AskedOfBy != "alice" || answered.AskedOfAt == nil {
			t.Fatalf("the hand-on answered asked_of=%v asked_of_by=%v asked_of_at=%v, want bob@example.com handed on by alice",
				answered.AskedOf, answered.AskedOfBy, answered.AskedOfAt)
		}
		askedOf, by, at := askedOfColumns(t, database, askID)
		if askedOf == nil || *askedOf != "bob@example.com" || by == nil || *by != "alice" || at == nil {
			t.Fatalf("the row holds asked_of=%v asked_of_by=%v asked_of_at=%v, want the canonical login handed on by alice", askedOf, by, at)
		}
		if waits, stored := askedOfPerson(t, handler, askID); waits != "bob@example.com" || stored == nil || *stored != "bob@example.com" {
			t.Fatalf("the ask read waits on %q with asked_of %v, want bob@example.com both", waits, stored)
		}
	})

	// The answer is the ask as a read of it answers: whom it waits on is on the 200 itself, so a
	// caller needs no second request to learn what its own write decided - a hand-back to the
	// assignee, which stores nobody, included.
	t.Run("the answer says whom the ask waits on, after a hand-on and after a hand-back", func(t *testing.T) {
		handler, _ := newTestHandlerWithStore(t)
		issue := createInteractionIssue(t, handler, "ANSWER", "A question handed to and fro", "A spec")
		askID := createIssueAsk(t, handler, issue.Key, "Which transport?")

		handedOn := handAskOn(t, handler, askID, "bob", "alice")
		if handedOn.WaitingOnPerson != "bob" || handedOn.WaitingOn != "human" {
			t.Fatalf("the hand-on answered waiting_on_person=%q waiting_on=%q, want bob and human",
				handedOn.WaitingOnPerson, handedOn.WaitingOn)
		}
		// Handed back to the issue's own assignee the stored field is empty, and the answer
		// still names the person: the expression every read shares resolves it.
		handedBack := handAskOn(t, handler, askID, "alice", "alice")
		if handedBack.AskedOf != nil || handedBack.WaitingOnPerson != "alice" || handedBack.WaitingOn != "human" {
			t.Fatalf("the hand-back answered asked_of=%v waiting_on_person=%q waiting_on=%q, want nothing stored and alice",
				handedBack.AskedOf, handedBack.WaitingOnPerson, handedBack.WaitingOn)
		}
	})

	// asked_of is a required string, never null: a body that omits it and one that sends null
	// both name nobody, and the route says so before it reads the ask.
	t.Run("a body naming nobody is refused", func(t *testing.T) {
		handler, database := newTestHandlerWithStore(t)
		issue := createInteractionIssue(t, handler, "EMPTY", "A question handed to nobody", "A spec")
		askID := createIssueAsk(t, handler, issue.Key, "Which transport?")
		target := "/api/v1/asks/" + askID + "/asked-of"

		for _, body := range []map[string]any{{}, {"asked_of": nil}, {"asked_of": ""}} {
			refused := dispatchRequest(t, handler, http.MethodPut, target, body, "alice")
			if refused.Code != http.StatusBadRequest || !strings.Contains(refused.Body.String(), `"code":"INVALID_ASK"`) {
				t.Fatalf("hand the ask on with %v: status=%d body=%s, want 400 INVALID_ASK",
					body, refused.Code, refused.Body.String())
			}
		}
		if askedOf, by, at := askedOfColumns(t, database, askID); askedOf != nil || by != nil || at != nil {
			t.Fatalf("the refused ask holds asked_of=%v asked_of_by=%v asked_of_at=%v, want all three empty", askedOf, by, at)
		}
	})

	// The person must be somebody who has signed in, held by the check the assignee is held by.
	t.Run("a login nobody has signed in with is refused", func(t *testing.T) {
		handler, database := newTestHandlerWithStore(t)
		issue := createInteractionIssue(t, handler, "UNKNOWN", "A question for a stranger", "A spec")
		askID := createIssueAsk(t, handler, issue.Key, "Which transport?")

		refused := handAskTo(t, handler, askID, "mallory@example.com", "alice")
		if refused.Code != http.StatusBadRequest || !strings.Contains(refused.Body.String(), `"code":"ASSIGNEE_NOT_ALLOWED"`) {
			t.Fatalf("hand the ask to a stranger: status=%d body=%s, want 400 ASSIGNEE_NOT_ALLOWED",
				refused.Code, refused.Body.String())
		}
		if askedOf, _, _ := askedOfColumns(t, database, askID); askedOf != nil {
			t.Fatalf("the refused ask holds asked_of %q, want none", *askedOf)
		}
	})

	// Nothing but a person's own hand-on writes the field: the create routes do not take it, and
	// an ask opens empty whatever its author sent.
	t.Run("the create route accepts no asked_of and an ask opens with it empty", func(t *testing.T) {
		handler, _ := newTestHandlerWithStore(t)
		issue := createInteractionIssue(t, handler, "CREATE", "An ask opens on nobody", "A spec")
		document := createProjectDocument(t, handler, "CREATE", "Design notes", "# Design notes\n")

		for _, target := range []string{
			"/api/v1/issues/" + issue.Key + "/asks",
			"/api/v1/artifacts/" + document.ID + "/asks",
		} {
			refused := sessionRequest(t, handler, http.MethodPost, target, map[string]any{
				"question": "Who decides?", "asked_of": "bob", "actor": sessionActor(),
			})
			if refused.Code != http.StatusBadRequest || !strings.Contains(refused.Body.String(), `"code":"INVALID_JSON"`) {
				t.Fatalf("create an ask on %s naming asked_of: status=%d body=%s, want 400 INVALID_JSON",
					target, refused.Code, refused.Body.String())
			}
		}
		opened := createIssueAsk(t, handler, issue.Key, "Which transport?")
		ask := readBlockAsk(t, handler, opened)
		if ask.AskedOf != nil || ask.AskedOfBy != nil || ask.AskedOfAt != nil {
			t.Fatalf("the opened ask holds asked_of=%v asked_of_by=%v asked_of_at=%v, want all three empty",
				ask.AskedOf, ask.AskedOfBy, ask.AskedOfAt)
		}
	})

	// An empty field means the ask follows its issue; a filled one means it does not, and handing
	// an ask back to the issue's own assignee empties the field rather than repeating them in it.
	t.Run("an empty field follows the issue assignee and a filled one does not", func(t *testing.T) {
		handler, database := newTestHandlerWithStore(t)
		seedPeople(t, database, "carol")
		issue := createInteractionIssue(t, handler, "FOLLOW", "A question that follows its issue", "A spec")
		askID := createIssueAsk(t, handler, issue.Key, "Which transport?")

		if waits, askedOf := askedOfPerson(t, handler, askID); waits != "alice" || askedOf != nil {
			t.Fatalf("the new ask waits on %q with asked_of %v, want its issue's assignee and no stored person", waits, askedOf)
		}
		reassign := func(login string) {
			t.Helper()
			moved := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key,
				map[string]any{"assignee": login}, "alice")
			if moved.Code != http.StatusOK {
				t.Fatalf("assign %s to %q: status=%d body=%s", issue.Key, login, moved.Code, moved.Body.String())
			}
		}
		reassign("bob")
		if waits, askedOf := askedOfPerson(t, handler, askID); waits != "bob" || askedOf != nil {
			t.Fatalf("after the reassignment the ask waits on %q with asked_of %v, want bob and no stored person", waits, askedOf)
		}

		handAskOn(t, handler, askID, "alice", "bob")
		reassign("carol")
		if waits, askedOf := askedOfPerson(t, handler, askID); waits != "alice" || askedOf == nil || *askedOf != "alice" {
			t.Fatalf("the handed-on ask waits on %q with asked_of %v after its issue moved, want alice both", waits, askedOf)
		}

		// Handed to the assignee of its own issue, it follows the issue again.
		handAskOn(t, handler, askID, "carol", "alice")
		if waits, askedOf := askedOfPerson(t, handler, askID); waits != "carol" || askedOf != nil {
			t.Fatalf("handed to the issue's own assignee the ask waits on %q with asked_of %v, want carol and no stored person", waits, askedOf)
		}
		if _, by, at := askedOfColumns(t, database, askID); by == nil || *by != "alice" || at == nil {
			t.Fatalf("the emptied field records asked_of_by=%v asked_of_at=%v, want the person who handed it back", by, at)
		}
	})

	// A project document is always open, so the close check a document ask passes refuses it
	// nothing, and it has no assignee to empty the field against.
	t.Run("a document ask is handed on and refused nothing", func(t *testing.T) {
		handler, _ := newTestHandlerWithStore(t)
		if created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
			"key": "DOCS", "name": "Docs",
		}, "alice"); created.Code != http.StatusCreated {
			t.Fatalf("create the DOCS project: status=%d body=%s", created.Code, created.Body.String())
		}
		document := createProjectDocument(t, handler, "DOCS", "Design notes", "# Design notes\n")
		askID := createSnoozeAsk(t, handler, "/api/v1/artifacts/"+document.ID+"/asks", "Document question")

		moved := handAskOn(t, handler, askID, "bob", "alice")
		if moved.AskedOf == nil || *moved.AskedOf != "bob" || moved.IssueKey != nil {
			t.Fatalf("the handed-on document ask = %#v, want bob on an ask with no issue", moved)
		}
		if waits, askedOf := askedOfPerson(t, handler, askID); waits != "bob" || askedOf == nil || *askedOf != "bob" {
			t.Fatalf("the document ask waits on %q with asked_of %v, want bob both", waits, askedOf)
		}
	})

	// Only an open ask moves: an answered one is fixed, as the hidden control on its card says.
	t.Run("an answered ask is refused", func(t *testing.T) {
		handler, _ := newTestHandlerWithStore(t)
		issue := createInteractionIssue(t, handler, "SETTLED", "An answered question", "A spec")
		askID := createIssueAsk(t, handler, issue.Key, "Which transport?")
		if answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer",
			map[string]any{"text": "REST."}, "alice"); answered.Code != http.StatusOK {
			t.Fatalf("answer the ask: status=%d body=%s", answered.Code, answered.Body.String())
		}

		refused := handAskTo(t, handler, askID, "bob", "alice")
		if refused.Code != http.StatusConflict || !strings.Contains(refused.Body.String(), `"code":"ASK_NOT_OPEN"`) {
			t.Fatalf("hand an answered ask on: status=%d body=%s, want 409 ASK_NOT_OPEN", refused.Code, refused.Body.String())
		}
	})

	// A closed issue refuses this write as it refuses every other write to its asks.
	t.Run("an issue ask on a closed issue is refused", func(t *testing.T) {
		handler, _ := newTestHandlerWithStore(t)
		issue := createInteractionIssue(t, handler, "CLOSED", "A question on a closed issue", "A spec")
		askID := createIssueAsk(t, handler, issue.Key, "Which transport?")
		closeIssue(t, handler, issue.Key)

		assertRefusedOnClosedIssue(t, "a hand-on", handAskTo(t, handler, askID, "bob", "alice"))
	})

	// The two restores write state back and leave the person where they found them.
	t.Run("the person survives the reopen of a closed issue", func(t *testing.T) {
		handler, _ := newTestHandlerWithStore(t)
		issue := createInteractionIssue(t, handler, "REOPEN", "A question reopened with its issue", "A spec")
		askID := createIssueAsk(t, handler, issue.Key, "Which transport?")
		handAskOn(t, handler, askID, "bob", "alice")

		closeIssue(t, handler, issue.Key)
		if withdrawn := readBlockAsk(t, handler, askID); withdrawn.State != "resolved" {
			t.Fatalf("the ask after the close is %q, want the close to have withdrawn it", withdrawn.State)
		}
		reopened := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key,
			map[string]any{"status": "todo"}, "alice")
		if reopened.Code != http.StatusOK {
			t.Fatalf("reopen the issue: status=%d body=%s", reopened.Code, reopened.Body.String())
		}
		restored := readBlockAsk(t, handler, askID)
		if restored.State != "open" || restored.AskedOf == nil || *restored.AskedOf != "bob" || restored.WaitingOnPerson != "bob" {
			t.Fatalf("the restored ask = %q waiting on %q with asked_of %v, want an open ask still waiting on bob",
				restored.State, restored.WaitingOnPerson, restored.AskedOf)
		}
	})

	// This case came from main's pull request #3429 ("a person hands a question to the person it
	// is for", merged 2026-11-11T00:53Z), where the block left the document through a delete on
	// the edits route. This branch's R5 (dispatch://INFRA-1520) refuses such a batch, so the
	// removal now runs through a person's own editor, the seam R5 exempts. What the case proves
	// is unchanged: asked_of survives the block's removal, the settlement's retraction and the
	// restore.
	t.Run("the person survives a block settlement's restore", func(t *testing.T) {
		handler, _, documentService := blockAskHandlerWithDocuments(t)
		issue, askID := seedBlockAsk(t, handler, "A block ask handed on", "decision", transportAsk, "Which transport?")
		handAskOn(t, handler, askID, "bob", "alice")

		removeBlockAsABrowserDoes(t, documentService, issue.PrimaryArtifactID, "decision")
		settleDocument(t, handler, issue.PrimaryArtifactID, issue.Key, "after-delete")
		if retracted := readBlockAsk(t, handler, askID); retracted.State != "resolved" {
			t.Fatalf("the ask whose block was deleted is %q, want the settlement to have retracted it", retracted.State)
		}
		if restoring := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits",
			map[string]any{"ops": []map[string]string{{"op": "insert", "after": "end", "markdown": transportAsk}}},
			"alice"); restoring.Code != http.StatusOK {
			t.Fatalf("write the ask block back: status=%d body=%s", restoring.Code, restoring.Body.String())
		}
		settleDocument(t, handler, issue.PrimaryArtifactID, issue.Key, "after-restore")

		restored := readBlockAsk(t, handler, askID)
		if restored.State != "open" || restored.AskedOf == nil || *restored.AskedOf != "bob" || restored.WaitingOnPerson != "bob" {
			t.Fatalf("the restored block ask = %q waiting on %q with asked_of %v, want an open ask still waiting on bob",
				restored.State, restored.WaitingOnPerson, restored.AskedOf)
		}
	})

	// The schema half: one timestamped pair, a census that counts nobody and an `up` that writes
	// no row, so the deploy takes the catalog write and nothing else.
	t.Run("the migration is a timestamped pair that writes no row", func(t *testing.T) {
		_, database := newTestHandlerWithStore(t)
		const directory = "../store/migrations"
		found, err := filepath.Glob(filepath.Join(directory, "*_asks_asked_of.*.sql"))
		if err != nil {
			t.Fatalf("read %s: %v", directory, err)
		}
		slices.Sort(found)
		if len(found) != 2 {
			t.Fatalf("the asks_asked_of migration is %v, want a census and an up", found)
		}
		censusPath, upPath := found[0], found[1]
		if filepath.Base(censusPath) != strings.TrimSuffix(filepath.Base(upPath), ".up.sql")+".census.sql" {
			t.Fatalf("the migration pair is %v, want <id>_asks_asked_of.census.sql beside <id>_asks_asked_of.up.sql", found)
		}
		identifier, _, _ := strings.Cut(filepath.Base(upPath), "_")
		if _, err := time.Parse("20060102150405", identifier); err != nil {
			t.Fatalf("the migration's identifier is %q, want the UTC moment it was written: %v", identifier, err)
		}

		censusText, err := os.ReadFile(censusPath)
		if err != nil {
			t.Fatalf("read %s: %v", censusPath, err)
		}
		statements := sqlStatementsOf(string(censusText))
		if len(statements) != 1 {
			t.Fatalf("the census holds %d statements (%v), want one", len(statements), statements)
		}
		var counted int
		if err := database.Pool.QueryRow(context.Background(), statements[0]).Scan(&counted); err != nil {
			t.Fatalf("run the census %q: %v", statements[0], err)
		}
		if counted != 0 {
			t.Fatalf("the census answers %d, want 0: this migration refuses no row", counted)
		}

		upText, err := os.ReadFile(upPath)
		if err != nil {
			t.Fatalf("read %s: %v", upPath, err)
		}
		up := sqlStatementsOf(string(upText))
		if len(up) != 1 {
			t.Fatalf("the up holds %d statements (%v), want the one alter", len(up), up)
		}
		if written := rowWritingStatement.FindString(up[0]); written != "" {
			t.Fatalf("the up statement writes rows (%q): %s", written, up[0])
		}
		lowered := strings.ToLower(sqlWhitespace.ReplaceAllString(up[0], " "))
		if !strings.HasPrefix(lowered, "alter table asks") {
			t.Fatalf("the up statement is %q, want one alter of asks", up[0])
		}
		for _, column := range []string{"asked_of text", "asked_of_by text", "asked_of_at timestamptz"} {
			if !strings.Contains(lowered, "add column "+column) {
				t.Fatalf("the up statement does not add %q: %s", column, up[0])
			}
		}
	})

	// The route is the person's in the table the gate reads, not only in the handler.
	t.Run("routeScopes lists the route as person-scoped", func(t *testing.T) {
		scopes, listed := routeScopes["PUT /api/v1/asks/{id}/asked-of"]
		if !listed || !slices.Equal(scopes, routeScopeSet{model.ScopePerson}) {
			t.Fatalf("the hand-on route's scopes are %v (listed=%t), want exactly %v",
				scopes, listed, routeScopeSet{model.ScopePerson})
		}
	})

	// The three columns ride docs.AskColumns, so the ask read and every ask event the census
	// lists carry them. Each event type below is produced by the flow its name describes.
	t.Run("the ask read and every ask event the census lists carry the field", func(t *testing.T) {
		var documentService *docs.Service
		handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
			documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour, MarkWait: 50 * time.Millisecond})
			t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
			return documentService
		})
		issue := createInteractionIssue(t, handler, "EVENTS", "Every ask event", "The quick brown fox")

		// ask.opened, on an anchored ask an accepted suggestion then orphans (ask.anchor_refreshed).
		anchored := decodeBody[model.Ask](t, sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
			"question": "What about fox?", "anchor": map[string]any{"artifact": "spec", "quote": "fox"},
			"actor": sessionActor(),
		}))
		suggestion := decodeBody[model.Comment](t, sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
			"body": "Later suggestion.", "anchor": map[string]any{"artifact": "spec", "quote": "brown fox"},
			"suggestion": map[string]string{"replace_with": "brown dog"}, "actor": sessionActor(),
		}))
		if accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+suggestion.ID+"/accept",
			map[string]any{}, "alice"); accepted.Code != http.StatusOK {
			t.Fatalf("accept the suggestion: status=%d body=%s", accepted.Code, accepted.Body.String())
		}

		// ask.asked_of, then ask.edited and ask.answered on the same ask.
		handAskOn(t, handler, anchored.ID, "bob", "alice")
		if edited := sessionRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+anchored.ID, map[string]any{
			"question": "What about the fox?", "actor": sessionActor(),
		}); edited.Code != http.StatusOK {
			t.Fatalf("edit the ask: status=%d body=%s", edited.Code, edited.Body.String())
		}
		if answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+anchored.ID+"/answer", map[string]any{
			"text": "The fox.", "expected_edited_at": readBlockAsk(t, handler, anchored.ID).EditedAt,
		}, "alice"); answered.Code != http.StatusOK {
			t.Fatalf("answer the ask: status=%d body=%s", answered.Code, answered.Body.String())
		}

		// ask.resolved, on an ask its author retracts.
		retracted := createIssueAsk(t, handler, issue.Key, "Which transport?")
		if resolved := sessionRequest(t, handler, http.MethodPost, "/api/v1/asks/"+retracted+"/resolve", map[string]any{
			"kind": "retracted", "reason": "No longer needed", "actor": sessionActor(),
		}); resolved.Code != http.StatusOK {
			t.Fatalf("retract the ask: status=%d body=%s", resolved.Code, resolved.Body.String())
		}

		// ask.handed_back, from an approval request a version move paused and its agent returned.
		requestApproval := func(summary string) *httptest.ResponseRecorder {
			return sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests",
				map[string]any{"actor": sessionActor(), "summary": summary})
		}
		if requested := requestApproval("Caps retries at three attempts."); requested.Code != http.StatusCreated {
			t.Fatalf("request approval: status=%d body=%s", requested.Code, requested.Body.String())
		}
		if _, err := replaceDocumentText(database, documentService, issue.PrimaryArtifactID, "A revised spec",
			model.Actor{Kind: "session", ID: "session-0123456789abcdef"}); err != nil {
			t.Fatalf("revise the document: %v", err)
		}
		if named := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/versions",
			map[string]string{"summary": "revised"}, "alice"); named.Code != http.StatusCreated {
			t.Fatalf("name the revised version: status=%d body=%s", named.Code, named.Body.String())
		}
		if handedBack := requestApproval("Adds the rollback budget."); handedBack.Code != http.StatusCreated {
			t.Fatalf("hand the approval request back: status=%d body=%s", handedBack.Code, handedBack.Body.String())
		}

		// GET /api/v1/asks/{id} carries the three fields, with the person the hand-on wrote.
		read := readBlockAsk(t, handler, anchored.ID)
		if read.AskedOf == nil || *read.AskedOf != "bob" || read.AskedOfBy == nil || *read.AskedOfBy != "alice" || read.AskedOfAt == nil {
			t.Fatalf("the ask read carries asked_of=%v asked_of_by=%v asked_of_at=%v, want bob handed on by alice",
				read.AskedOf, read.AskedOfBy, read.AskedOfAt)
		}

		payloads := askEventPayloads(t, database, issue.Key)
		for _, eventType := range askEventCensus(t) {
			events := payloads[eventType]
			if len(events) == 0 {
				t.Fatalf("no %s event was written; the census names it, so this test must produce one", eventType)
			}
			for index, payload := range events {
				for _, field := range []string{"asked_of", "asked_of_by", "asked_of_at"} {
					if _, carried := payload[field]; !carried {
						t.Fatalf("%s event %d carries no %s: %v", eventType, index, field, payload)
					}
				}
			}
		}
		// The hand-on's own payload names the person it moved the ask to, not a null the read
		// filled in later.
		handOn := payloads["ask.asked_of"]
		if len(handOn) != 1 || string(handOn[0]["asked_of"]) != `"bob"` {
			t.Fatalf("the ask.asked_of payloads = %v, want one naming bob", handOn)
		}
	})

	// The hand-on moves whose list the ask is in and nothing else: no rewording, so the ask's
	// wording history and its edit list are as they were.
	t.Run("the hand-on appends one ask.asked_of and no ask.edited", func(t *testing.T) {
		handler, database := newTestHandlerWithStore(t)
		issue := createInteractionIssue(t, handler, "THREAD", "A question handed to bob", "A spec")
		askID := createIssueAsk(t, handler, issue.Key, "Which transport?")
		before := readBlockAsk(t, handler, askID)

		handAskOn(t, handler, askID, "bob", "alice")

		counts := askEventCounts(t, handler, issue.Key, askID)
		if counts["ask.asked_of"] != 1 || counts["ask.edited"] != 0 {
			t.Fatalf("the ask's events after the hand-on = %#v, want one ask.asked_of and no ask.edited", counts)
		}
		// The two people the hand-on moved, as an assignee move names them: the issue's assignee,
		// who waited before, and the person it waits on now.
		payload := askEventPayloads(t, database, issue.Key)["ask.asked_of"]
		if len(payload) != 1 {
			t.Fatalf("the ask.asked_of events = %v, want one", payload)
		}
		var threadLogins []string
		if err := json.Unmarshal(payload[0]["thread_logins"], &threadLogins); err != nil {
			t.Fatalf("decode the ask.asked_of thread_logins: %v", err)
		}
		if !slices.Equal(threadLogins, []string{"alice", "bob"}) {
			t.Fatalf("the ask.asked_of thread_logins = %v, want the person it moved from and the one it moved to", threadLogins)
		}

		after := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID, nil, "alice")
		if after.Code != http.StatusOK {
			t.Fatalf("read the handed-on ask: status=%d body=%s", after.Code, after.Body.String())
		}
		read := decodeBody[struct {
			Ask   model.Ask       `json:"ask"`
			Edits []model.AskEdit `json:"edits"`
		}](t, after)
		if read.Ask.Question != before.Question || read.Ask.EditedAt != nil || len(read.Edits) != 0 {
			t.Fatalf("after the hand-on the ask reads %q edited at %v with %d edits, want the wording it opened with and no edit",
				read.Ask.Question, read.Ask.EditedAt, len(read.Edits))
		}
	})
}
