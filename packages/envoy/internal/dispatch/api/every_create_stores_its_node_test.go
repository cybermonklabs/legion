package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestEveryCreateStoresItsNode holds every write route to leaving no row of a project-owned kind
// without its node in the project, or between the parties, the row belongs to
// (the node a read limited to a project depends on). It calls every POST row of writeClasses -
// read from the table, so the set is the server's own - on a server seeded with one object of
// each kind, settles every document the calls left owing a settlement, then asks
// graphcensus.ProjectComplete, the census's completeness read, which names any row left without
// a node, with no project, or with another than its row's.
//
// A POST row needs a request the route takes. postCalls holds one for every row. A route that can
// create an issue, a document, an ask, a message or a project gets a body that makes one, which
// the test holds to a 2xx: a create gets the body that makes its object, and a write of a
// document's text - a document edit, a suggestion's accept, the rebuild of a document whose
// stored history does not load - writes a new ask block, whose ask the document's settlement
// opens (docs' createAskBlock). The rig settles a document an hour on, so the test settles every
// document the calls left owing a settlement at once, and requires each new block's ask before it
// reads the census. A named version writes no text and opens no ask, but it gets the body the
// route takes too, and is held to a 2xx. Every other row gets the empty body, whose answer the
// test does not read, since it can create nothing of these kinds. A POST row of the table
// postCalls does not name fails the test by name: a create route added later is caught there and
// called once its request is written down.
//
// A project's create seeds its `evals-md` document (evals.Store.EnsureTx), and an eval write adds
// its section to that document, or creates the document where the project holds none. So the
// project create is called with a new project's key (seedingProject); the register write is
// called on a project made by SQL, as one from before the eval registry is, whose first eval
// write creates the document (unseededProject); and the readings write is called on the
// fixture's project, whose document it edits. Once the calls are made the test requires both new
// projects to hold their `evals-md`, so a call that answered 2xx and made none leaves the census
// nothing to check.
//
// The controls are the ask create's node write skipped (api/asks.go) and the settled ask block's
// node write skipped (docs/ask_blocks.go createAskBlock): either way the test fails, naming the
// ask the create left without a node.
func TestEveryCreateStoresItsNode(t *testing.T) {
	rig := newStoreWriterRig(t)
	ctx := context.Background()
	fixture := seedEveryKind(t, rig)

	var posts []string
	for name := range writeClasses {
		if strings.HasPrefix(name, http.MethodPost+" ") {
			posts = append(posts, name)
		}
	}
	slices.Sort(posts)
	if len(posts) == 0 {
		t.Fatal("writeClasses holds no POST row, so the test calls nothing")
	}
	var blocks []string
	for _, name := range posts {
		call, listed := postCalls[name]
		if !listed {
			t.Errorf("%s is a POST row of writeClasses that postCalls holds no request for: write down how to call it", name)
			continue
		}
		pattern := strings.TrimPrefix(name, http.MethodPost+" ")
		target := fixture
		if call.project != "" {
			target.project = call.project
		}
		response := call.send(t, rig.handler, target.path(pattern))
		if call.succeeds && (response.Code < 200 || response.Code > 299) {
			t.Errorf("%s answered %d %s, want a 2xx", name, response.Code, response.Body.String())
			continue
		}
		if call.askBlock != "" {
			blocks = append(blocks, call.askBlock)
		}
	}
	asks := settledAskBlocks(t, rig, blocks)
	for _, project := range []string{seedingProject, unseededProject} {
		var held int
		if err := rig.database.Pool.QueryRow(ctx, `
			select count(*) from artifacts where project_key = $1 and issue_key is null and slug = $2
		`, project, evals.DocumentSlug).Scan(&held); err != nil {
			t.Fatalf("count the %s documents of project %s: %v", evals.DocumentSlug, project, err)
		}
		if held != 1 {
			t.Errorf("project %s holds %d %s documents after the calls, want the one its create, or the first eval write that named it, made",
				project, held, evals.DocumentSlug)
		}
	}

	incomplete, err := graphcensus.ProjectComplete(ctx, rig.database.Pool)
	if err != nil {
		t.Fatalf("read the rows whose node names no project: %v", err)
	}
	if len(incomplete) != 0 {
		t.Errorf("after every POST route, these rows have no node in their project: %v", incomplete)
	}
	t.Logf("called %d POST routes of writeClasses; the settlements opened the asks %v", len(posts), asks)
}

// everyKindFixture is the object of each kind the routes address: the rig's issue and project
// document, an ask on the issue, a comment a session's mention answered, a suggestion on the
// issue's spec whose replacement is a new ask block, a direct message a session was sent, and a
// document of the issue whose stored history does not load, the one document a rebuild takes. The
// issue also has an open child, which a status change's include_children moves.
type everyKindFixture struct {
	issue, project, document, documentSlug string
	ask, comment, suggestion, message      string
	unloadable                             string
}

// acceptedAskBlock is the ask block the fixture's suggestion puts into the issue's spec once it is
// accepted.
const acceptedAskBlock = "every-create-accepted"

// rebuiltAskBlock is the ask block the rebuild of the fixture's unloadable document writes.
const rebuiltAskBlock = "every-create-rebuilt"

// seedingProject is the key of the project the project create makes, whose seed creates its
// evals-md.
const seedingProject = "SEEDED"

// unseededProject is a project made by SQL, as one from before the eval registry is: it holds no
// evals-md, so the first eval write that names it creates the document (seedEveryKind).
const unseededProject = "BARE"

// everyCreateEval is the slug of the eval the register write registers, and the one the readings
// write's path names.
const everyCreateEval = "every-create-eval"

// seedEveryKind writes the objects postCalls' paths name, through the API.
func seedEveryKind(t *testing.T, rig *storeWriterRig) everyKindFixture {
	t.Helper()
	fixture := everyKindFixture{issue: rig.issue, project: "LOCK", document: rig.document}
	if err := rig.database.Pool.QueryRow(context.Background(),
		`select slug from artifacts where id = $1::uuid`, rig.document).Scan(&fixture.documentSlug); err != nil {
		t.Fatalf("read the slug of the project document: %v", err)
	}
	fixture.ask = createIssueAsk(t, rig.handler, rig.issue, "Which region goes first?")
	mentioned := decodeMentionedComment(t, postMentionedComment(t, rig.handler, rig.issue, map[string]any{
		"body": "Does this ship, @s1?", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	settledCommentDeliveries(t, rig.database, mentioned.ID)
	fixture.comment = mentioned.ID
	suggested := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+rig.issue+"/comments", map[string]any{
		"body": "Ask it instead.", "anchor": map[string]any{"artifact": "spec", "quote": "What the writers write."},
		"suggestion": map[string]string{"replace_with": askBlockMarkdown(acceptedAskBlock, "Does an accepted ask name its project?")},
	}, "alice")
	if suggested.Code != http.StatusCreated {
		t.Fatalf("suggest an ask block: status=%d body=%s", suggested.Code, suggested.Body.String())
	}
	fixture.suggestion = decodeBody[model.Comment](t, suggested).ID
	direct := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/agents/s1/messages", map[string]any{
		"body": "Status, please.", "delivery": "btw",
	}, "alice")
	if direct.Code != http.StatusCreated {
		t.Fatalf("send the direct message: status=%d body=%s", direct.Code, direct.Body.String())
	}
	fixture.message = decodeBody[model.Message](t, direct).ID
	// The rebuild route takes only a document whose history does not load, so one is uploaded and
	// its stored history corrupted, as the close's tests corrupt one (makeDocumentUnloadable).
	documents, ok := rig.direct.deps.Docs.(*docs.Service)
	if !ok {
		t.Fatalf("the rig's document service is a %T, want *docs.Service", rig.direct.deps.Docs)
	}
	fixture.unloadable = uploadDocument(t, rig.handler, "/api/v1/issues/"+rig.issue, "unloadable.md",
		"# Unloadable\n\nWhat the rebuild replaces.\n").Artifact.ID
	corrupted := makeDocumentUnloadable(t, repairFixture{handler: rig.handler, database: rig.database, documents: documents}, fixture.unloadable)
	if corrupted == 0 {
		t.Fatal("the unloadable document holds no stored update to corrupt")
	}
	// A status change leaves its comments only when an option moves an issue (status_change.go), so
	// the issue gets an open child for the call's include_children to move.
	child := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "LOCK", "title": "The child a status change moves", "parent": rig.issue,
	}, "alice")
	if child.Code != http.StatusCreated {
		t.Fatalf("file the issue's child: status=%d body=%s", child.Code, child.Body.String())
	}
	if _, err := rig.database.Pool.Exec(context.Background(),
		`insert into projects (key, name, default_assignee) values ($1, 'Made by SQL', 'alice')`, unseededProject); err != nil {
		t.Fatalf("make project %s, which holds no %s, by SQL: %v", unseededProject, evals.DocumentSlug, err)
	}
	return fixture
}

// pathParameter matches one parameter of a route pattern.
var pathParameter = regexp.MustCompile(`\{([a-z_]+)\}`)

// path fills a route pattern's parameters from the fixture: a key is the project's under
// /projects/ and the issue's elsewhere, an id is the object of the collection it follows - the
// suggestion's for an accept, which only a suggestion takes, and the unloadable document's for a
// rebuild, which only such a document takes - a slug under an eval is the eval's, and a parameter
// no object answers takes a value that addresses nothing.
func (f everyKindFixture) path(pattern string) string {
	return pathParameter.ReplaceAllStringFunc(pattern, func(match string) string {
		at := strings.Index(pattern, match)
		before := pattern[:at]
		switch match {
		case "{key}":
			if strings.HasSuffix(before, "/projects/") {
				return f.project
			}
			return f.issue
		case "{slug}":
			if strings.Contains(pattern, "/evals/{slug}") {
				return everyCreateEval
			}
			if strings.HasPrefix(pattern, "/api/v1/projects/") {
				return f.documentSlug
			}
			return "spec"
		case "{session_id}":
			return "s1"
		case "{attempt}":
			return "1"
		case "{phase}":
			return "spec"
		case "{block_id}":
			return "b1"
		case "{owner}", "{repo}":
			return "nobody"
		case "{id}":
			switch {
			case strings.HasSuffix(before, "/artifacts/") && strings.HasSuffix(pattern, "/rebuild"):
				return f.unloadable
			case strings.HasSuffix(before, "/artifacts/"):
				return f.document
			case strings.HasSuffix(before, "/asks/"):
				return f.ask
			case strings.HasSuffix(before, "/comments/") && strings.HasSuffix(pattern, "/accept"):
				return f.suggestion
			case strings.HasSuffix(before, "/comments/"):
				return f.comment
			case strings.HasSuffix(before, "/messages/"):
				return f.message
			}
		}
		return "00000000-0000-4000-8000-000000000000"
	})
}

// postCall is how the test calls one POST row: its body, who sends it, whether it is a file upload
// rather than a JSON body, whether the route takes the request, which the test then holds to a
// 2xx, the ask block the call writes into a document, whose ask that document's settlement
// opens, the project the route is called on where it is not the fixture's, and the
// Idempotency-Key the route requires, "" for none.
type postCall struct {
	body     any
	session  bool
	upload   bool
	succeeds bool
	askBlock string
	project  string
	key      string
}

func (c postCall) send(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	if c.upload {
		return multipartRequest(t, handler, path, map[string]string{"name": "shot.png"}, "shot.png", "image/png", pngBytes("every create"), "alice")
	}
	body := c.body
	if body == nil {
		body = map[string]any{}
	}
	if c.session {
		return bearerRequest(t, handler, http.MethodPost, path, body)
	}
	if c.key != "" {
		return keyedRequest(t, handler, http.MethodPost, path, body, "alice", c.key)
	}
	return dispatchRequest(t, handler, http.MethodPost, path, body, "alice")
}

// noObject is a POST row that can create no row of a project-owned kind: it is called with the
// empty body, and its answer is not read.
var noObject = postCall{}

// askBlockMarkdown is a free-text ask block, block, asking question.
func askBlockMarkdown(block, question string) string {
	return ":::ask{#" + block + ` urgency="high" multiple="false"}` + "\n" + question + "\n:::\n"
}

// askBlockEdit is a document edit that appends a new ask block, block, asking question.
func askBlockEdit(block, question string) postCall {
	return postCall{succeeds: true, askBlock: block, body: map[string]any{
		"ops": []map[string]string{{"op": "insert", "after": "end", "markdown": askBlockMarkdown(block, question)}},
	}}
}

// settledAskBlocks settles at once every document a write left owing its settlement, which the
// rig's settle window would leave for an hour, and answers the ask each of blocks opened, by block
// id, once every one of them has opened. A block whose ask never opens fails the test by name.
func settledAskBlocks(t *testing.T, rig *storeWriterRig, blocks []string) map[string]string {
	t.Helper()
	ctx := context.Background()
	documents, ok := rig.direct.deps.Docs.(*docs.Service)
	if !ok {
		t.Fatalf("the rig's document service is a %T, want *docs.Service", rig.direct.deps.Docs)
	}
	rows, err := rig.database.Pool.Query(ctx, `select artifact_id::text from doc_settlements_pending`)
	if err != nil {
		t.Fatalf("read the documents owing a settlement: %v", err)
	}
	owed, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the documents owing a settlement: %v", err)
	}
	for _, document := range owed {
		documents.SettleNow(document)
	}
	opened := make(map[string]string, len(blocks))
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		rows, err := rig.database.Pool.Query(ctx, `select block_id, id::text from asks where block_id = any($1)`, blocks)
		if err != nil {
			t.Fatalf("read the asks of the new ask blocks: %v", err)
		}
		var block, id string
		if _, err := pgx.ForEachRow(rows, []any{&block, &id}, func() error {
			opened[block] = id
			return nil
		}); err != nil {
			t.Fatalf("read the asks of the new ask blocks: %v", err)
		}
		if len(opened) == len(blocks) {
			return opened
		}
		if time.Now().After(deadline) {
			var missing []string
			for _, block := range blocks {
				if _, open := opened[block]; !open {
					missing = append(missing, block)
				}
			}
			t.Fatalf("settling the %d documents owing a settlement opened no ask for the blocks %v", len(owed), missing)
		}
	}
}

// postCalls is how every POST row of writeClasses is called. The creates make one object each of
// the kinds whose node names its project, the writes of a document's text one ask block each, and
// the status change, whose include_children moves the fixture's child, a comment of Dispatch's
// system actor on the child and one on the issue it names; the project create and the eval writes
// make or edit the project's evals-md; every other row creates none.
var postCalls = map[string]postCall{
	"POST /api/v1/issues":                       {succeeds: true, body: map[string]any{"project": "LOCK", "title": "Every create stores its node", "spec": "A spec.\n"}},
	"POST /api/v1/issues/{key}/artifacts":       {succeeds: true, body: map[string]any{"name": "notes.md", "content": "# Notes\n"}},
	"POST /api/v1/projects/{key}/artifacts":     {succeeds: true, body: map[string]any{"name": "Playbook.md", "content": "# Playbook\n"}},
	"POST /api/v1/issues/{key}/asks":            {succeeds: true, body: map[string]any{"question": "Does the issue's ask name its project?"}},
	"POST /api/v1/artifacts/{id}/asks":          {succeeds: true, body: map[string]any{"question": "Does the document's ask name its project?"}},
	"POST /api/v1/issues/{key}/comments":        {succeeds: true, body: map[string]any{"body": "A remark on the issue."}},
	"POST /api/v1/issues/{key}/status-change":   {succeeds: true, body: map[string]any{"status": "in_progress", "include_children": true}},
	"POST /api/v1/artifacts/{id}/comments":      {succeeds: true, body: map[string]any{"body": "A remark on the document."}},
	"POST /api/v1/issues/{key}/messages":        {succeeds: true, body: map[string]any{"body": "A message on the issue."}},
	"POST /api/v1/agents/{session_id}/messages": {succeeds: true, body: map[string]any{"body": "Another direct message.", "delivery": "btw"}},
	"POST /api/v1/broadcasts":                   {succeeds: true, body: map[string]any{"body": "To everyone.", "delivery": "btw", "session_ids": []string{"s1"}}},
	"POST /api/v1/comments/{id}/reply": {succeeds: true, session: true, body: map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "Yes, it ships."}},
	"POST /api/v1/messages/{id}/reply": {succeeds: true, session: true, body: map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "All green."}},
	"POST /api/v1/artifacts/{id}/approval-requests": {succeeds: true, session: true, body: map[string]any{"actor": map[string]any{"kind": "session", "id": "s1"}}},
	"POST /api/v1/agents/{session_id}/artifacts":    {succeeds: true, upload: true},

	// The project create, whose seed creates the project's evals-md; the register write, on the
	// project made by SQL that holds none, whose first eval write creates it; and the readings
	// write, which adds its eval's section to the fixture's own.
	"POST /api/v1/projects": {succeeds: true, body: map[string]any{
		"key": seedingProject, "name": "A project whose create seeds its evals-md"}},
	"POST /api/v1/projects/{key}/evals": {succeeds: true, project: unseededProject, body: map[string]any{
		"slug": everyCreateEval, "doc_text": "What the every-create register write counts."}},
	"POST /api/v1/projects/{key}/evals/{slug}/readings": {succeeds: true, key: "every-create-readings", body: map[string]any{
		"readings": []any{reading("minutes", 10, time.Now().UTC().Add(-time.Hour))}}},

	"POST /api/v1/artifacts/{id}/edits":                  askBlockEdit("every-create-document-edit", "Does an edited document's ask name its project?"),
	"POST /api/v1/issues/{key}/artifacts/{slug}/edits":   askBlockEdit("every-create-spec-edit", "Does an edited spec's ask name its project?"),
	"POST /api/v1/projects/{key}/artifacts/{slug}/edits": askBlockEdit("every-create-slug-edit", "Does an ask edited in by slug name its project?"),
	"POST /api/v1/comments/{id}/accept":                  {succeeds: true, askBlock: acceptedAskBlock},
	"POST /api/v1/artifacts/{id}/rebuild": {succeeds: true, askBlock: rebuiltAskBlock, body: map[string]any{
		"markdown": "# Unloadable\n\n" + askBlockMarkdown(rebuiltAskBlock, "Does a rebuilt document's ask name its project?")}},
	"POST /api/v1/artifacts/{id}/versions":                  {succeeds: true, body: map[string]any{"summary": "The project document, named."}},
	"POST /api/v1/issues/{key}/artifacts/{slug}/versions":   {succeeds: true, body: map[string]any{"summary": "The spec, named."}},
	"POST /api/v1/projects/{key}/artifacts/{slug}/versions": {succeeds: true, body: map[string]any{"summary": "The project document, named by its slug."}},

	"POST /api/v1/issues/{key}/claim":                                      noObject,
	"POST /api/v1/issues/{key}/results":                                    noObject,
	"POST /api/v1/issues/{key}/results/dry-run":                            noObject,
	"POST /api/v1/issues/{key}/handoffs/{phase}":                           noObject,
	"POST /api/v1/results/{id}/retract":                                    noObject,
	"POST /api/v1/projects/{key}/architecture-sources/{owner}/{repo}/sync": noObject,
	"POST /api/v1/projects/{key}/architecture-sources/sync":                noObject,
	"POST /api/v1/projects/{key}/architecture-source/sync":                 noObject,
	"POST /api/v1/projects/{key}/related":                                  noObject,
	"POST /api/v1/artifacts/{id}/blocks/{block_id}/keep":                   noObject,
	"POST /api/v1/artifacts/{id}/reviews":                                  noObject,
	"POST /api/v1/artifacts/{id}/versions/{number}/reviews":                noObject,
	"POST /api/v1/artifacts/{id}/plan-reviews":                             noObject,
	"POST /api/v1/artifacts/{id}/_test/outside-schema":                     noObject,
	"POST /api/v1/artifacts/{id}/_test/newer-content":                      noObject,
	"POST /api/v1/artifacts/{id}/_test/hold-persistence":                   noObject,
	"POST /api/v1/asks/{id}/answer":                                        noObject,
	"POST /api/v1/asks/{id}/resolve":                                       noObject,
	"POST /api/v1/asks/{id}/decline":                                       noObject,
	"POST /api/v1/comments/{id}/resolve":                                   noObject,
	"POST /api/v1/comments/{id}/reopen":                                    noObject,
	"POST /api/v1/comments/{id}/deliveries":                                noObject,
	"POST /api/v1/comments/{id}/reject":                                    noObject,
	"POST /api/v1/messages/{id}/deliveries":                                noObject,
	"POST /api/v1/going/requests":                                          noObject,
	"POST /api/v1/messages/{id}/deliveries/{attempt}/accept":               noObject,
	"POST /api/v1/agents/{session_id}/stream/_test/publish":                noObject,
	"POST /api/v1/me/agent-tokens":                                         noObject,
	"POST /api/v1/me/inbox/read":                                           noObject,
	"POST /api/v1/me/inbox/dismiss":                                        noObject,
	"POST /api/v1/priorities":                                              noObject,
	"POST /api/v1/deploys":                                                 noObject,
	"POST /api/v1/delivery/session-groups":                                 noObject,
	"POST /api/v1/credential-requests/{id}/approve":                        noObject,
	"POST /api/v1/credential-requests/{id}/deny":                           noObject,
	"POST /api/v1/credential-requests/{id}/dismiss":                        noObject,
	"POST /api/v1/credential-requests/machine-lookup":                      noObject,
	"POST /api/v1/credential-grants/{id}/revoke":                           noObject,
	"POST /api/v1/machine-logins/{id}/revoke":                              noObject,
	"POST /api/v1/signins/{id}/revoke":                                     noObject,
	"POST /api/v1/live/_test/disconnect":                                   noObject,
	"POST /api/v1/live/_test/rotate":                                       noObject,
	"POST /api/v1/artifacts/_test/quiesce":                                 noObject,
	"POST /api/v1/settings/new-issue/_test":                                noObject,
	"POST /api/v1/writes/_test/slow":                                       noObject,
	"POST /api/v1/markdown/round-trip":                                     noObject,
	"POST /api/v1/markdown/file-round-trip":                                noObject,
}
