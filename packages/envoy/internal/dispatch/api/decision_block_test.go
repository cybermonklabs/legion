package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// decisionWriteRoutes and decisionWritePaths are what the table below must drive: the eight
// routes a decision block can be written on, under every actor each of them takes - both a
// person and an agent, but for the two replies only the session, which is the one writer those
// routes have. The test counts what it drove against these numbers, so a table left empty, or a
// row dropped from it, fails rather than passes in silence.
const (
	decisionWriteRoutes = 8
	decisionWritePaths  = 14
)

// decisionSession is the live session the fixture's mention and targeted message are delivered
// to, which replies to both, and the one a broadcast and a direct send name.
const decisionSession = "s1"

// decisionSessions is the listener's registry: the one session every targeted write here goes
// to, live and taking every mode.
const decisionSessions = `[{"session_id":"s1","title":"Planner","capabilities":["btw","aside","steer"],"last_seen":1}]`

// theDecisionBlock is a decision block as a writer writes one into a comment or a message: a
// question and its options inside an `ask` directive, which the reader draws a decision card for
// and nobody can answer where it stands.
const theDecisionBlock = ":::ask{urgency=\"med\"}\n" +
	"Which transport should we expose?\n\n- REST: the platform's own\n- gRPC: streaming\n:::"

// decisionPlacements are the places a decision block stands in a body - at the top, in a quote,
// in a list item and inside a callout - each with the whole refusal every route answers it with.
// The wording is spelled out here rather than formatted from the same string the server formats
// it from, so the text a writer reads is read back against a text this test holds, and the
// position and block type each refusal names are the top-level block that holds the decision
// block, which is the decision block's own type only where it stands at the top.
var decisionPlacements = []struct {
	name string
	body string
	want string
}{
	{
		name: "at the top of the body",
		body: theDecisionBlock + "\n",
		want: "body block 1 (ask) holds a decision block, which nobody can answer where it stands: " +
			"ask the question in the issue's own document, as a decision block there, or open an ask with " +
			"`dispatch ask`; to show a decision block as an example, put it in a fenced code block",
	},
	{
		name: "in a quote",
		body: "Lead text.\n\n> :::ask{urgency=\"med\"}\n> Which transport should we expose?\n> :::\n",
		want: "body block 2 (blockquote) holds a decision block, which nobody can answer where it stands: " +
			"ask the question in the issue's own document, as a decision block there, or open an ask with " +
			"`dispatch ask`; to show a decision block as an example, put it in a fenced code block",
	},
	{
		name: "in a list item",
		body: "Lead text.\n\n- Context for the question\n\n  :::ask{urgency=\"med\"}\n" +
			"  Which transport should we expose?\n  :::\n",
		want: "body block 2 (bullet_list) holds a decision block, which nobody can answer where it stands: " +
			"ask the question in the issue's own document, as a decision block there, or open an ask with " +
			"`dispatch ask`; to show a decision block as an example, put it in a fenced code block",
	},
	{
		name: "inside a callout",
		body: "Lead text.\n\nMore lead text.\n\n:::callout{kind=\"note\" title=\"Decide\"}\nA note.\n\n" +
			theDecisionBlock + "\n:::\n",
		want: "body block 3 (callout) holds a decision block, which nobody can answer where it stands: " +
			"ask the question in the issue's own document, as a decision block there, or open an ask with " +
			"`dispatch ask`; to show a decision block as an example, put it in a fenced code block",
	},
}

// decisionFixture is one of everything the eight write paths write to or through: the issue, the
// project document a comment on a document stands on - an issue's own spec is commented on
// through the issue's routes, not the document's (ARTIFACT_LINKED) - the comment the edit route
// rewrites, the mention and the targeted message the live session replies to, and alice's own
// agent token, which is how an agent writes on the three routes that require the person scope.
type decisionFixture struct {
	handler  http.Handler
	database *store.Store
	issue    string
	document string
	edited   string
	mention  string
	direct   string
	token    string
}

// newDecisionFixture builds that fixture and waits out the deliveries its mention and its
// targeted message left running, so what the table reads before and after each refusal is a
// database nothing else is still writing to.
func newDecisionFixture(t *testing.T) *decisionFixture {
	t.Helper()
	listener, _ := newBroadcastListener(t, decisionSessions)
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL})
	issue := createInteractionIssue(t, handler, "DECIDE", "Decision blocks", "before")
	fixture := &decisionFixture{handler: handler, database: database, issue: issue.Key}

	fixture.document = decisionProjectDocument(t, handler, "DECIDE")
	fixture.edited = createThreadComment(t, handler, issue.Key, map[string]any{"body": "before the edit"}, "alice").ID
	fixture.mention = decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Can this ship?", "mentions": []map[string]any{{"target": "session:" + decisionSession}},
	})).ID
	fixture.direct = createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "Targeted message.", "target": "session:" + decisionSession, "delivery": "btw",
	}, "alice").ID
	fixture.token = mintPersonalToken(t, handler, "alice", actingTokenName, nil).Token

	settledCommentDeliveries(t, database, fixture.mention)
	awaitBackgroundWrites(t, served)
	return fixture
}

// decisionProjectDocument uploads the project document a comment on a document stands on and
// answers its id.
func decisionProjectDocument(t *testing.T, handler http.Handler, project string) string {
	t.Helper()
	uploaded := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/"+project+"/artifacts", map[string]any{
		"name": "plan.md", "content": "# Plan\n\nbefore\n",
	}, "alice")
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload the project document: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	return decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact.ID
}

// decisionSessionActor is the session an agent names when it writes with the deployment's shared
// agent token.
func decisionSessionActor() map[string]any {
	return map[string]any{"kind": "session", "id": decisionSession}
}

// storedByTheWritePaths is everything the eight write paths store, read back as a reader reads
// it and counted as rows of the store as well: the issue's comments and messages, the spec's
// comments, the session's messages and the broadcasts - a broadcast writes one message per
// recipient, so a row written by either is counted twice over - with the edited comment's body
// byte for byte beside them.
type storedByTheWritePaths struct {
	IssueComments    int
	DocumentComments int
	IssueMessages    int
	SessionMessages  int
	Broadcasts       int
	CommentRows      int
	MessageRows      int
	BroadcastRows    int
	EditedBody       string
}

func (f *decisionFixture) stored(t *testing.T) storedByTheWritePaths {
	t.Helper()
	count := func(target string) int {
		response := dispatchRequest(t, f.handler, http.MethodGet, target, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s: status=%d body=%s", target, response.Code, response.Body.String())
		}
		return len(decodeBody[[]json.RawMessage](t, response))
	}
	stored := storedByTheWritePaths{
		IssueComments:    count("/api/v1/issues/" + f.issue + "/comments"),
		DocumentComments: count("/api/v1/artifacts/" + f.document + "/comments"),
		IssueMessages:    count("/api/v1/issues/" + f.issue + "/messages"),
		SessionMessages:  count("/api/v1/agents/" + decisionSession + "/messages"),
		Broadcasts:       count("/api/v1/broadcasts"),
	}
	if err := f.database.Pool.QueryRow(context.Background(), `
		select (select count(*) from comments), (select count(*) from messages), (select count(*) from broadcasts)
	`).Scan(&stored.CommentRows, &stored.MessageRows, &stored.BroadcastRows); err != nil {
		t.Fatalf("count the stored rows: %v", err)
	}
	stored.EditedBody = decodeBody[commentRead](t, dispatchRequest(t, f.handler,
		http.MethodGet, "/api/v1/comments/"+f.edited, nil, "alice")).Comment.Body
	return stored
}

// decisionWrite is one write path under one actor: send writes body the way that actor writes on
// that route, and answers what the route said.
type decisionWrite struct {
	route string
	actor string
	send  func(t *testing.T, f *decisionFixture, body string) *httptest.ResponseRecorder
}

// decisionWrites is the table: the eight routes of the requirement, each under the actors it
// takes. A person writes as alice does from a browser. An agent writes as the route lets it: as
// a session, with the deployment's shared token and its own id in actor, on the five routes that
// take a session actor, and under its person's own agent token, which carries the person scope,
// on the three that require it - the comment edit, the issue-less send to a session and the
// broadcast. The two replies have no person's form at all: both refuse a caller that is not a
// session (403 REPLY_FORBIDDEN) before any body is read, since a reply answers a delivery
// attempt made to a session.
func decisionWrites() []decisionWrite {
	return []decisionWrite{
		{"POST /api/v1/issues/{key}/comments", "a person", func(t *testing.T, f *decisionFixture, body string) *httptest.ResponseRecorder {
			return dispatchRequest(t, f.handler, http.MethodPost, "/api/v1/issues/"+f.issue+"/comments",
				map[string]any{"body": body}, "alice")
		}},
		{"POST /api/v1/issues/{key}/comments", "an agent", func(t *testing.T, f *decisionFixture, body string) *httptest.ResponseRecorder {
			return sessionRequest(t, f.handler, http.MethodPost, "/api/v1/issues/"+f.issue+"/comments",
				map[string]any{"body": body, "actor": decisionSessionActor()})
		}},
		{"POST /api/v1/artifacts/{id}/comments", "a person", func(t *testing.T, f *decisionFixture, body string) *httptest.ResponseRecorder {
			return dispatchRequest(t, f.handler, http.MethodPost, "/api/v1/artifacts/"+f.document+"/comments",
				map[string]any{"body": body}, "alice")
		}},
		{"POST /api/v1/artifacts/{id}/comments", "an agent", func(t *testing.T, f *decisionFixture, body string) *httptest.ResponseRecorder {
			return sessionRequest(t, f.handler, http.MethodPost, "/api/v1/artifacts/"+f.document+"/comments",
				map[string]any{"body": body, "actor": decisionSessionActor()})
		}},
		{"PATCH /api/v1/comments/{id}", "a person", func(t *testing.T, f *decisionFixture, body string) *httptest.ResponseRecorder {
			return dispatchRequest(t, f.handler, http.MethodPatch, "/api/v1/comments/"+f.edited,
				map[string]any{"body": body}, "alice")
		}},
		{"PATCH /api/v1/comments/{id}", "an agent", func(t *testing.T, f *decisionFixture, body string) *httptest.ResponseRecorder {
			return agentRequest(t, f.handler, http.MethodPatch, "/api/v1/comments/"+f.edited+"?session="+actingSession,
				map[string]any{"body": body}, f.token)
		}},
		{"POST /api/v1/comments/{id}/reply", "an agent", func(t *testing.T, f *decisionFixture, body string) *httptest.ResponseRecorder {
			return sessionRequest(t, f.handler, http.MethodPost, "/api/v1/comments/"+f.mention+"/reply",
				map[string]any{"actor": decisionSessionActor(), "attempt": 1, "body": body})
		}},
		{"POST /api/v1/issues/{key}/messages", "a person", func(t *testing.T, f *decisionFixture, body string) *httptest.ResponseRecorder {
			return dispatchRequest(t, f.handler, http.MethodPost, "/api/v1/issues/"+f.issue+"/messages",
				map[string]any{"body": body}, "alice")
		}},
		{"POST /api/v1/issues/{key}/messages", "an agent", func(t *testing.T, f *decisionFixture, body string) *httptest.ResponseRecorder {
			return sessionRequest(t, f.handler, http.MethodPost, "/api/v1/issues/"+f.issue+"/messages",
				map[string]any{"body": body, "actor": decisionSessionActor()})
		}},
		{"POST /api/v1/agents/{session_id}/messages", "a person", func(t *testing.T, f *decisionFixture, body string) *httptest.ResponseRecorder {
			return dispatchRequest(t, f.handler, http.MethodPost, "/api/v1/agents/"+decisionSession+"/messages",
				map[string]any{"body": body, "delivery": "btw"}, "alice")
		}},
		{"POST /api/v1/agents/{session_id}/messages", "an agent", func(t *testing.T, f *decisionFixture, body string) *httptest.ResponseRecorder {
			return agentRequest(t, f.handler, http.MethodPost,
				"/api/v1/agents/"+decisionSession+"/messages?session="+actingSession,
				map[string]any{"body": body, "delivery": "btw"}, f.token)
		}},
		{"POST /api/v1/messages/{id}/reply", "an agent", func(t *testing.T, f *decisionFixture, body string) *httptest.ResponseRecorder {
			return sessionRequest(t, f.handler, http.MethodPost, "/api/v1/messages/"+f.direct+"/reply",
				map[string]any{"actor": decisionSessionActor(), "attempt": 1, "body": body})
		}},
		{"POST /api/v1/broadcasts", "a person", func(t *testing.T, f *decisionFixture, body string) *httptest.ResponseRecorder {
			return postBroadcast(t, f.handler, map[string]any{
				"body": body, "delivery": "btw", "session_ids": []string{decisionSession},
			}, "alice")
		}},
		{"POST /api/v1/broadcasts", "an agent", func(t *testing.T, f *decisionFixture, body string) *httptest.ResponseRecorder {
			return agentRequest(t, f.handler, http.MethodPost, "/api/v1/broadcasts?session="+actingSession, map[string]any{
				"body": body, "delivery": "btw", "session_ids": []string{decisionSession},
			}, f.token)
		}},
	}
}

// assertDecisionBlockRefused requires response to be the 400 DECISION_BLOCK_IN_BODY a body
// holding a decision block is refused with, reading want whole.
func assertDecisionBlockRefused(t *testing.T, request string, response *httptest.ResponseRecorder, want string) {
	t.Helper()
	refusal := decodeBody[apiErrorBody](t, response)
	if response.Code != http.StatusBadRequest || refusal.Code != "DECISION_BLOCK_IN_BODY" || refusal.Error != want {
		t.Fatalf("%s = %d %s %q, want 400 DECISION_BLOCK_IN_BODY %q",
			request, response.Code, refusal.Code, refusal.Error, want)
	}
}

// Every route that stores a comment, a message or a broadcast body refuses a decision block
// written into it, wherever it stands in that body and whoever writes it: 400
// DECISION_BLOCK_IN_BODY, worded once by itemblocks.DecisionBlock, the same text on every route
// (INFRA-1201). Nothing is stored by a refusal: the issue's comments and messages, the spec's
// comments, the session's messages and the broadcasts are counted before and after each row, as
// a reader reads them and as rows of the store, and the edited comment's body is read back byte
// for byte.
func TestDecisionBlockIsRefusedOnEveryWriteRoute(t *testing.T) {
	f := newDecisionFixture(t)
	// Every write below is refused, so the store never moves: one snapshot before the first of
	// them is what each route's own read is held to, and a route that stored something fails
	// against it as it would against a snapshot of its own.
	before := f.stored(t)
	drove, routes := 0, map[string]struct{}{}
	for _, write := range decisionWrites() {
		t.Run(write.route+" as "+write.actor, func(t *testing.T) {
			for _, placement := range decisionPlacements {
				t.Run(placement.name, func(t *testing.T) {
					assertDecisionBlockRefused(t, write.route+" as "+write.actor,
						write.send(t, f, placement.body), placement.want)
				})
			}
			if after := f.stored(t); after != before {
				t.Fatalf("%s as %s stored something: %+v, want the %+v it found",
					write.route, write.actor, after, before)
			}
			drove++
			routes[write.route] = struct{}{}
		})
	}
	if drove != decisionWritePaths || len(routes) != decisionWriteRoutes {
		t.Fatalf("the table drove %d writes over %d routes, want %d over the requirement's %d",
			drove, len(routes), decisionWritePaths, decisionWriteRoutes)
	}
}

// storeCommentWithADecisionBlock writes a comment holding a decision block the way one written
// before the rule stood is held: the row and its block index, the two writes the create route
// makes, made here around the check that route now makes.
func storeCommentWithADecisionBlock(t *testing.T, database *store.Store, issueKey, body string) string {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	var id string
	var createdAt time.Time
	if err := tx.QueryRow(ctx, `
		insert into comments (issue_key, author, body)
		values ($1, '{"kind":"user","id":"alice"}', $2) returning id::text, created_at
	`, issueKey, body).Scan(&id, &createdAt); err != nil {
		t.Fatalf("insert the comment: %v", err)
	}
	if _, err := refs.IndexItem(ctx, tx, refs.ItemComment, id, body, createdAt, nil, nil); err != nil {
		t.Fatalf("index the comment's blocks: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return id
}

// The rule refuses a decision block as it is written and changes nothing about one already
// stored, which is the issue's non-goal: a comment holding a decision block that stands in the
// database is read back whole and listed with its block indexed under the type `ask`, because
// the indexer calls neither bound nor the new rule (refs.IndexItem).
func TestACommentStoredWithADecisionBlockIsStillReadWholeAndIndexedAsAnAsk(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "STORED", "A stored decision block", "before")
	body := "Lead text.\n\n" + theDecisionBlock + "\n"
	id := storeCommentWithADecisionBlock(t, database, issue.Key, body)

	read := decodeBody[commentRead](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+id, nil, "alice"))
	if read.Comment.Body != body {
		t.Fatalf("the stored comment reads back %q, want the %q it holds", read.Comment.Body, body)
	}
	listed := decodeBody[model.ItemBlocks](t, dispatchRequest(t, handler,
		http.MethodGet, "/api/v1/comments/"+id+"/blocks", nil, "alice"))
	asks := []model.ItemBlock{}
	for _, block := range listed.Blocks {
		if block.Type == "ask" {
			asks = append(asks, block)
		}
	}
	if !listed.Indexed || len(asks) != 1 || asks[0].Text != theDecisionBlock {
		t.Fatalf("the stored comment lists %+v (indexed=%v), want its decision block indexed as one `ask` block",
			listed.Blocks, listed.Indexed)
	}
}

// decisionRefusedParse are bodies the server's parser refuses - for a block the schema holds
// nothing like, for a typed block it does not declare, for a line its reader and the browser's
// read differently, and for nesting past a bound - each holding the same decision block, which
// the browser reader draws its card for all the same: the reader draws a card for every `ask` its
// own parser makes, whatever else in the body the server's parse refused (INFRA-1201). Each is
// named by what makes the parse refuse it, so a reader of a failure reads the class.
var decisionRefusedParse = []struct{ name, body string }{
	{name: "a link reference definition", body: theDecisionBlock + "\n\n[a]: http://x\n"},
	{name: "a block of HTML", body: theDecisionBlock + "\n\n<!-- legion-outbox:1 -->\n"},
	{name: "a typed block of a type the schema does not declare", body: theDecisionBlock + "\n\n:::unknown{#b1}\nBody.\n:::\n"},
	{name: "a quoted line the browser's parser reads as a block of HTML", body: theDecisionBlock + "\n\n> </a\t>\n> text\n"},
	{name: "quotes nested past the block bound", body: theDecisionBlock + "\n\n" + strings.Repeat("> ", 101) + "deep\n"},
	{
		name: "a table whose short rows would take more padding than one write may add",
		body: theDecisionBlock + "\n\n" + strings.Repeat("| a ", 110) + "|\n" +
			strings.Repeat("| - ", 110) + "|\n" + strings.Repeat("| x |\n", 95),
	},
}

// decisionAtTheTop is the whole refusal a body holding its decision block as its first block is
// answered with, spelled out here as the placements above spell theirs out.
const decisionAtTheTop = "body block 1 (ask) holds a decision block, which nobody can answer where it stands: " +
	"ask the question in the issue's own document, as a decision block there, or open an ask with " +
	"`dispatch ask`; to show a decision block as an example, put it in a fenced code block"

// A body the parser refuses is refused for its decision block too, in the same words and under
// the same code: every write route answers one such body so, and the comment route answers one of
// every class of refusal. Before INFRA-1201 each of these was stored while the browser drew its
// decision card in full, which is the silent failure the rule exists to stop. Nothing is stored by
// these refusals either.
func TestADecisionBlockIsRefusedInABodyTheParserRefuses(t *testing.T) {
	f := newDecisionFixture(t)
	// One of the bodies stands for the class on every route - the link reference definition, the
	// line a writer leaves behind most often - and the comment route below answers every class.
	onEveryRoute := decisionRefusedParse[0]
	before := f.stored(t)
	for _, write := range decisionWrites() {
		t.Run(write.route+" as "+write.actor, func(t *testing.T) {
			assertDecisionBlockRefused(t, write.route+" as "+write.actor,
				write.send(t, f, onEveryRoute.body), decisionAtTheTop)
			if after := f.stored(t); after != before {
				t.Fatalf("%s as %s stored something: %+v, want the %+v it found",
					write.route, write.actor, after, before)
			}
		})
	}
	for _, refused := range decisionRefusedParse {
		t.Run("a comment holding "+refused.name, func(t *testing.T) {
			assertDecisionBlockRefused(t, "POST /api/v1/issues/{key}/comments", dispatchRequest(t, f.handler,
				http.MethodPost, "/api/v1/issues/"+f.issue+"/comments",
				map[string]any{"body": refused.body}, "alice"), decisionAtTheTop)
		})
	}
}

// Nothing a write path stores changed with the reading that closed the lone carriage return: the
// rule reads a body whose line endings the browser reader ends a line at and this parser ends
// none at over a copy of its own (itemblocks.Check), so a comment written in those line endings
// and holding no decision block is stored and reads back byte for byte, every carriage return
// where its writer put it.
func TestACommentOfLoneCarriageReturnsIsStoredByteForByte(t *testing.T) {
	f := newDecisionFixture(t)
	before := f.stored(t)
	body := "Which one?\r\r- REST: the platform's own\r- gRPC: streaming\r"
	created := createThreadComment(t, f.handler, f.issue, map[string]any{"body": body}, "alice")

	read := decodeBody[commentRead](t, dispatchRequest(t, f.handler,
		http.MethodGet, "/api/v1/comments/"+created.ID, nil, "alice"))
	if read.Comment.Body != body {
		t.Fatalf("the stored comment reads back %q, want the %q it was written with", read.Comment.Body, body)
	}
	if after := f.stored(t); after.IssueComments != before.IssueComments+1 || after.CommentRows != before.CommentRows+1 {
		t.Fatalf("the store holds %+v, want the one comment more than the %+v it found", after, before)
	}
}

// No document write path changed: the rule holds bodies, which carry no answer, and a document is
// where a decision block belongs. A document write takes the markdown the body routes refuse - the
// same bytes, refused as a comment and stored as a document - and a document whose markdown the
// parser refuses is still refused for that markdown, under its own code, never for the decision
// block it holds.
func TestADocumentWriteStillTakesADecisionBlockTheBodyRoutesRefuse(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "DOCASK", "Decision blocks in documents", "before")
	content := "# Plan\n\n" + theDecisionBlock + "\n"

	uploaded := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/DOCASK/artifacts",
		map[string]any{"name": "plan.md", "content": content}, "alice")
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("the document write answered %d %s, want the decision block stored",
			uploaded.Code, uploaded.Body.String())
	}
	document := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact.ID
	text := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+document+"/text", nil, "alice").Body.String()
	if !strings.Contains(text, ":::ask{") || !strings.Contains(text, "Which transport should we expose?") {
		t.Fatalf("the document reads back %q, want the decision block it was written with", text)
	}

	assertDecisionBlockRefused(t, "POST /api/v1/issues/{key}/comments", dispatchRequest(t, handler,
		http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
		map[string]any{"body": content}, "alice"),
		"body block 2 (ask) holds a decision block, which nobody can answer where it stands: "+
			"ask the question in the issue's own document, as a decision block there, or open an ask with "+
			"`dispatch ask`; to show a decision block as an example, put it in a fenced code block")

	refused := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/DOCASK/artifacts",
		map[string]any{"name": "refused.md", "content": decisionRefusedParse[0].body}, "alice")
	body := decodeBody[apiErrorBody](t, refused)
	if refused.Code != http.StatusBadRequest || body.Code == "DECISION_BLOCK_IN_BODY" {
		t.Fatalf("the document write answered %d %s %q, want the markdown's own refusal",
			refused.Code, body.Code, body.Error)
	}
}
