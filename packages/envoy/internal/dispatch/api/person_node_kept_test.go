package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/decisionread"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The words of the issue every fixture of TestOnlyAPersonChangesAPersonsDecision starts from, each
// alice's, and the words an agent's refused writes would put in their place.
const (
	personKeptProse             = "Alice writes this paragraph herself."
	personKeptQuestion          = "Which store do the writers take?"
	personKeptRewordedQuestion  = "Which store do the readers take?"
	personKeptSectionWords      = "We write through the ledger."
	personKeptWords             = "We keep one store."
	personKeptRewordedWords     = "We keep two stores."
	personKeptAgentWords        = "We cache every read for a minute."
	personKeptProjectWords      = "We release on Tuesdays."
	personKeptRewordedProject   = "We release on Thursdays."
	personKeptProjectDocName    = "notes.md"
	personKeptSpecDocumentName  = "spec.md"
	personKeptAgentDecisionCode = ":::decision{#ad1}\n" + personKeptAgentWords + "\n:::\n"
)

// personKeptSpec is the spec alice writes whole on each issue fixture: her prose, a requirement,
// her ask q1, which she answers, the section s1 holding words of its own and her decision pd1, the
// section s2, and her decision pd2. Every decision of it is hers: the version she writes decides
// each, so each one's decided_by names her.
const personKeptSpec = personKeptProse + "\n\n" +
	":::requirement{#r1}\nEvery write reaches the store once.\n:::\n\n" +
	":::ask{#q1 urgency=\"med\" multiple=\"false\"}\n" + personKeptQuestion + "\n\n- Ledger: The ledger we run.\n- Log: A log of our own.\n:::\n\n" +
	"::::callout{#s1 kind=\"note\" title=\"The store\"}\nThe section's own words.\n\n:::decision{#pd1}\n" + personKeptSectionWords + "\n:::\n::::\n\n" +
	":::callout{#s2 kind=\"note\" title=\"Elsewhere\"}\nAnother section.\n:::\n\n" +
	":::decision{#pd2}\n" + personKeptWords + "\n:::\n"

// personKeptProjectDocument is the project document alice writes whole: three decisions of hers.
const personKeptProjectDocument = "# Notes\n\n:::decision{#jd1}\n" + personKeptProjectWords + "\n:::\n\n" +
	":::decision{#jd2}\nWe tag every release.\n:::\n\n:::decision{#jd3}\nWe keep one branch.\n:::\n"

// personKeptTables are the tables a write of a person's decision changes: its tuples and nodes,
// its document's versions and blocks, the events, the asks and the Keeps.
var personKeptTables = []string{"graph_tuples", "graph_nodes", "artifact_versions", "document_blocks", "events", "asks", "decision_keeps"}

// personKeptCaller is one credential TestOnlyAPersonChangesAPersonsDecision writes with: what the
// test calls it, the actor its body names - a bearer that proves no session names one, and a
// browser's cookie and a broker token, which prove their writer, name none - and the request that
// carries it.
type personKeptCaller struct {
	name  string
	actor map[string]any
	sign  func(t *testing.T, method, path string, body any) *http.Request
}

// personKeptBrowser is alice at her browser.
func personKeptBrowser() personKeptCaller {
	return personKeptCaller{name: "alice's browser", sign: func(t *testing.T, method, path string, body any) *http.Request {
		return withPerson(brokerRequest(t, method, path, body, "", ""), "alice")
	}}
}

// personKeptBearer is a caller presenting token as a bearer, naming the session sessionActor
// names: the deployment's shared token, or a personal token.
func personKeptBearer(name, token string) personKeptCaller {
	return personKeptCaller{name: name, actor: sessionActor(), sign: func(t *testing.T, method, path string, body any) *http.Request {
		return brokerRequest(t, method, path, body, "Bearer "+token, "")
	}}
}

// personKeptBroker is caller presenting token, an access token the secrets broker issued, under
// the DPoP scheme, each request with a fresh proof signed for its method and its target, which
// leaves the query out (RFC 9449 Section 4.3).
func personKeptBroker(name string, caller *brokerCaller, token string) personKeptCaller {
	return personKeptCaller{name: name, sign: func(t *testing.T, method, path string, body any) *http.Request {
		target, _, _ := strings.Cut(path, "?")
		signed := caller.caller.Signed(t, method, testServerURL+target, token)
		return brokerRequest(t, method, path, body, signed.Header.Get("Authorization"), signed.Header.Get("DPoP"))
	}}
}

// personKeptRig is the server TestOnlyAPersonChangesAPersonsDecision drives, broker tokens among
// its credentials, and every document the test has written, which each snapshot waits on.
type personKeptRig struct {
	*brokerTokenRig
	documents []string
}

// send carries one request of caller through the server: body with the caller's actor beside it,
// and tag as its If-Match where tag is not "".
func (k *personKeptRig) send(t *testing.T, caller personKeptCaller, method, path string, body map[string]any, tag string) *httptest.ResponseRecorder {
	t.Helper()
	var payload any
	if body != nil || caller.actor != nil {
		merged := map[string]any{}
		for name, value := range body {
			merged[name] = value
		}
		if caller.actor != nil {
			merged["actor"] = caller.actor
		}
		payload = merged
	}
	request := caller.sign(t, method, path, payload)
	if tag != "" {
		request.Header.Set("If-Match", tag)
	}
	return k.serve(request)
}

// snapshot is what a refused write must leave as it is, once every write the test made before has
// settled and the queries those settlements moved hold their versions: a fingerprint of each table
// of personKeptTables, and the text of each document.
func (k *personKeptRig) snapshot(t *testing.T) map[string]string {
	t.Helper()
	awaitSettled(t, k.database, k.documents...)
	k.awaitQueriesDrained(t)
	awaitBackgroundWrites(t, k.served)
	held := make(map[string]string, len(personKeptTables)+len(k.documents))
	for _, table := range personKeptTables {
		var digest string
		if err := k.database.Pool.QueryRow(context.Background(), fmt.Sprintf(
			`select count(*) || ':' || coalesce(md5(string_agg(t::text, ',' order by t::text)), '') from %s t`,
			pgx.Identifier{table}.Sanitize(),
		)).Scan(&digest); err != nil {
			t.Fatalf("fingerprint %s: %v", table, err)
		}
		held["the table "+table] = digest
	}
	for _, document := range k.documents {
		held["the text of "+document] = documentMarkdown(t, k.handler, document)
	}
	return held
}

// requireUnchanged fails for each part of before a snapshot taken now no longer holds as it was.
func (k *personKeptRig) requireUnchanged(t *testing.T, what string, before map[string]string) {
	t.Helper()
	after := k.snapshot(t)
	for _, part := range slices.Sorted(maps.Keys(before)) {
		if after[part] != before[part] {
			t.Errorf("%s stored something: %s changed", what, part)
		}
	}
}

// awaitQueriesDrained waits until no issue of the documents the test has written owes its queries
// a version (issue_query_marks). The settlement of a document of an issue room writes no version
// of a query it moved: its batch marks the issue's queries owed, and the drain writes their
// versions once the settlement has committed (docs.Service.DrainOwedQueries), so a document that
// owes no settlement (awaitSettled) may still have them to come.
func (k *personKeptRig) awaitQueriesDrained(t *testing.T) {
	t.Helper()
	testwait.UntilPolling(t, 20*time.Millisecond, fmt.Sprintf("the issues of documents %v to owe no query drain (issue_query_marks)", k.documents), func() (int, bool) {
		var owed int
		if err := k.database.Pool.QueryRow(context.Background(), `
			select count(*) from issue_query_marks m
			where m.issue_key in (select a.issue_key from artifacts a where a.id = any($1::uuid[]))
		`, k.documents).Scan(&owed); err != nil {
			t.Fatalf("read the query marks: %v", err)
		}
		return owed, owed == 0
	})
}

// requireRefusal fails t unless response is status with code, its message holding each of names,
// and the write stored nothing since before.
func (k *personKeptRig) requireRefusal(t *testing.T, what string, response *httptest.ResponseRecorder, before map[string]string, status int, code string, names ...string) {
	t.Helper()
	var refusal refusalAnswer
	_ = json.Unmarshal(response.Body.Bytes(), &refusal)
	if response.Code != status || refusal.Code != code {
		got := strconv.Itoa(response.Code)
		if refusal.Code != "" {
			got += " " + refusal.Code
		}
		t.Fatalf("%s answered %s, want %d %s: %s", what, got, status, code, response.Body.String())
	}
	for _, name := range names {
		if !strings.Contains(refusal.Error, name) {
			t.Errorf("%s is refused %q, which does not hold %s", what, refusal.Error, name)
		}
	}
	k.requireUnchanged(t, what, before)
}

// requireKept is requireRefusal of R32's 409 ANSWERED_ASK_KEPT.
func (k *personKeptRig) requireKept(t *testing.T, what string, response *httptest.ResponseRecorder, before map[string]string, names ...string) {
	t.Helper()
	k.requireRefusal(t, what, response, before, http.StatusConflict, codeAnsweredAskKept, names...)
}

// requirePersonKeptTaken fails t unless response is want.
func requirePersonKeptTaken(t *testing.T, what string, response *httptest.ResponseRecorder, want int) {
	t.Helper()
	if response.Code != want {
		t.Fatalf("%s answered %d, want %d: %s", what, response.Code, want, response.Body.String())
	}
}

// personKeptIssue is one issue alice wrote whole (personKeptSpec) on an issue Legion does not run,
// so R28 masks nothing: its key, its spec, and the id of the ask q1, which alice answered.
type personKeptIssue struct {
	key  string
	spec decisionDocument
	ask  string
}

// node is the store address of the block id of the issue's spec.
func (f personKeptIssue) node(id string) string { return f.spec.address(id) }

// block is the address a refusal names the block id of the issue's spec by.
func (f personKeptIssue) block(id string) string { return "dispatch://" + f.key + "/spec#b-" + id }

// askNode is the address of q1's own node, `ask/<uuid>`.
func (f personKeptIssue) askNode() string { return graphstore.KindAsk + "/" + f.ask }

// edits, slugEdits and uploads are the issue spec's edit routes, by its id and by its slug, and
// its issue's upload.
func (f personKeptIssue) edits() string { return "/api/v1/artifacts/" + f.spec.id + "/edits" }
func (f personKeptIssue) slugEdits() string {
	return "/api/v1/issues/" + f.key + "/artifacts/spec/edits"
}
func (f personKeptIssue) uploads() string { return "/api/v1/issues/" + f.key + "/artifacts" }

// nodePath is the path of the node route of address under the issue, with query where it is not "".
func (f personKeptIssue) nodePath(address, query string) string {
	path := "/api/v1/issues/" + f.key + "/nodes/" + url.PathEscape(address)
	if query != "" {
		path += "?" + query
	}
	return path
}

// issue writes one issue fixture in a project of its own: alice writes the project and the issue
// with personKeptSpec, and answers q1 once settlement has indexed it. It fails unless each of her
// decisions is decided by her.
func (k *personKeptRig) issue(t *testing.T, project string) personKeptIssue {
	t.Helper()
	createReferenceAPIProject(t, k.handler, project)
	issue := createSpecIssue(t, k.handler, project, personKeptSpec)
	spec := readDecisionDocument(t, k.database, issue.Key, "spec")
	k.documents = append(k.documents, spec.id)
	ask := indexedDecisionBlocks(t, k.handler, issue.Key, spec.id, map[string]string{"q1": personKeptQuestion})["q1"]
	answerDecisionBlock(t, k.handler, ask, "alice", []string{"Ledger"}, "", nil)
	awaitSettled(t, k.database, spec.id)
	fixture := personKeptIssue{key: issue.Key, spec: spec, ask: ask}
	for _, decision := range []string{"pd1", "pd2"} {
		requireDecidedBy(t, k.database, "alice's decision "+decision, fixture.node(decision), "alice")
	}
	return fixture
}

// personKeptProject is one project document alice wrote whole (personKeptProjectDocument): its
// project, the document, and the address a refusal names each of its blocks by.
type personKeptProject struct {
	project  string
	document model.Artifact
}

func (p personKeptProject) block(id string) string {
	return "dispatch://" + p.project + "/artifact/" + p.document.Slug + "#b-" + id
}
func (p personKeptProject) edits() string { return "/api/v1/artifacts/" + p.document.ID + "/edits" }
func (p personKeptProject) slugEdits() string {
	return "/api/v1/projects/" + p.project + "/artifacts/" + p.document.Slug + "/edits"
}
func (p personKeptProject) uploads() string { return "/api/v1/projects/" + p.project + "/artifacts" }

// projectDocument writes alice's project document in project, which the test has written, and
// fails unless each of its decisions is decided by her.
func (k *personKeptRig) projectDocument(t *testing.T, project string) personKeptProject {
	t.Helper()
	document := createProjectDocument(t, k.handler, project, personKeptProjectDocName, personKeptProjectDocument)
	k.documents = append(k.documents, document.ID)
	awaitSettled(t, k.database, document.ID)
	for _, decision := range []string{"jd1", "jd2", "jd3"} {
		requireDecidedBy(t, k.database, "alice's project decision "+decision, personKeptBlockNode(t, k.database, document.ID, decision), "alice")
	}
	return personKeptProject{project: project, document: document}
}

// personKeptBlockNode is the store address of the block blockID of the document artifactID, read
// where the block index keys it.
func personKeptBlockNode(t *testing.T, database *store.Store, artifactID, blockID string) string {
	t.Helper()
	var address string
	if err := database.Pool.QueryRow(context.Background(), `
		select b.node_address from document_blocks b
		where `+refs.DocumentRoomBlocksSQL("b", "$1::uuid")+` and b.block_id = $2 and b.node_address is not null`,
		artifactID, blockID).Scan(&address); err != nil {
		t.Fatalf("read the node of block %s of %s: %v", blockID, artifactID, err)
	}
	return address
}

// personKeptPath is one text path of a document: an edit route, or the upload that replaces the
// document under upload, "" for an edit route.
type personKeptPath struct {
	name, path, upload string
}

// personKeptChange is one write of a block an agent may not make on a person's decision (R32):
// what it does, its edit batch, and what it makes of the document's markdown as an upload.
type personKeptChange struct {
	what   string
	ops    []map[string]any
	upload func(t *testing.T, markdown string) string
}

// write sends caller's change of document through path: its batch to an edit route, or its
// rewrite of the document's markdown, naming the document's token, to the upload.
func (k *personKeptRig) write(t *testing.T, caller personKeptCaller, path personKeptPath, document string, change personKeptChange) *httptest.ResponseRecorder {
	t.Helper()
	if path.upload == "" {
		return k.send(t, caller, http.MethodPost, path.path, map[string]any{"ops": change.ops}, "")
	}
	return k.send(t, caller, http.MethodPost, path.path, map[string]any{
		"name": path.upload, "content": change.upload(t, documentMarkdown(t, k.handler, document)),
		"precondition": uploadBase(t, k.handler, document),
	}, "")
}

// personKeptReword is markdown with its one occurrence of words written as with.
func personKeptReword(words, with string) func(t *testing.T, markdown string) string {
	return func(t *testing.T, markdown string) string {
		t.Helper()
		if held := strings.Count(markdown, words); held != 1 {
			t.Fatalf("the document holds %q %d times, want once:\n%s", words, held, markdown)
		}
		return strings.Replace(markdown, words, with, 1)
	}
}

// personKeptOpener is markdown with the opening line of the typed block it opens with opener -
// its fence, its type and its id - written as line.
func personKeptOpener(opener, line string) func(t *testing.T, markdown string) string {
	return func(t *testing.T, markdown string) string {
		t.Helper()
		lines := strings.Split(markdown, "\n")
		at := slices.IndexFunc(lines, func(held string) bool { return strings.HasPrefix(held, opener) })
		if at < 0 {
			t.Fatalf("the document holds no line opening %s:\n%s", opener, markdown)
		}
		lines[at] = line
		return strings.Join(lines, "\n")
	}
}

// personKeptCut is markdown without the typed block it opens with opener: its lines to the fence
// that closes it, and the blank line after.
func personKeptCut(opener string) func(t *testing.T, markdown string) string {
	return func(t *testing.T, markdown string) string {
		t.Helper()
		lines := strings.Split(markdown, "\n")
		start := slices.IndexFunc(lines, func(held string) bool { return strings.HasPrefix(held, opener) })
		if start < 0 {
			t.Fatalf("the document holds no line opening %s:\n%s", opener, markdown)
		}
		fence := lines[start][:strings.IndexFunc(lines[start], func(r rune) bool { return r != ':' })]
		end := start + 1
		for end < len(lines) && lines[end] != fence {
			end++
		}
		if end == len(lines) {
			t.Fatalf("the block opening %s holds no closing fence %s:\n%s", opener, fence, markdown)
		}
		if end+1 < len(lines) && lines[end+1] == "" {
			end++
		}
		return strings.Join(append(slices.Clone(lines[:start]), lines[end+1:]...), "\n")
	}
}

// personKeptDecisionChanges are the writes of the decision id, saying words, an agent is refused:
// a change of its words, a retype that keeps its type and changes its words by an attribute, a
// retype to another type, its removal, and its transfer to another document, which on a text path
// is its removal from this one once the other holds its words.
func personKeptDecisionChanges(id, words, reworded string) []personKeptChange {
	opener := ":::decision{#" + id
	return []personKeptChange{
		{
			what:   "change of the words",
			ops:    []map[string]any{{"op": "replace", "find": words, "with": reworded}},
			upload: personKeptReword(words, reworded),
		},
		{
			what: `retype writing uncertainty="high", which changes the words ` + strconv.Quote(words) + ` to the same words with that attribute,`,
			ops: []map[string]any{{
				"op": "retype", "block": id, "type": "decision", "attributes": map[string]any{"uncertainty": "high"},
			}},
			upload: personKeptOpener(opener, opener+` uncertainty="high"}`),
		},
		{
			what: "retype to a callout",
			ops: []map[string]any{{
				"op": "retype", "block": id, "type": "callout", "attributes": map[string]any{"kind": "note", "title": "Was decided"},
			}},
			upload: personKeptOpener(opener, ":::callout{#"+id+` kind="note" title="Was decided"}`),
		},
		{what: "removal", ops: []map[string]any{{"op": "delete", "block": id}}, upload: personKeptCut(opener)},
		{what: "transfer to the plan", ops: []map[string]any{{"op": "delete", "block": id}}, upload: personKeptCut(opener)},
	}
}

// personKeptAskChanges are the writes of q1, an answered ask's block, an agent is refused: a change
// of its words, a retype, its removal and its transfer to another document.
func personKeptAskChanges() []personKeptChange {
	return []personKeptChange{
		{
			what:   "change of the words",
			ops:    []map[string]any{{"op": "replace", "find": personKeptQuestion, "with": personKeptRewordedQuestion}},
			upload: personKeptReword(personKeptQuestion, personKeptRewordedQuestion),
		},
		{
			what: "retype to a callout",
			ops: []map[string]any{{
				"op": "retype", "block": "q1", "type": "callout", "attributes": map[string]any{"kind": "note", "title": "Was asked"},
			}},
			upload: personKeptOpener(":::ask{#q1", `:::callout{#q1 kind="note" title="Was asked"}`),
		},
		{what: "removal", ops: []map[string]any{{"op": "delete", "block": "q1"}}, upload: personKeptCut(":::ask{#q1")},
		{what: "transfer to the plan", ops: []map[string]any{{"op": "delete", "block": "q1"}}, upload: personKeptCut(":::ask{#q1")},
	}
}

// personKeptEntry is the Decisions read's entry of the block id of the issue's spec, as alice
// reads it.
func (k *personKeptRig) entry(t *testing.T, f personKeptIssue, id string) decisionEntry {
	t.Helper()
	return requireEntry(t, readEntries(t, k.handler, f.key), id, refs.BlockNodeKind+":"+f.node(id))
}

// stateOf is the node state the store's row holds of the node it keys as kind and id.
func (k *personKeptRig) stateOf(t *testing.T, kind, id string) string {
	t.Helper()
	node, held, err := graphstore.ReadNode(context.Background(), k.database.Pool, kind, id)
	if err != nil || !held {
		t.Fatalf("read the node %s %s: held %t, %v", kind, id, held, err)
	}
	return node.State
}

// dryRun is the count caller's dry run of the delete of address under f answers.
func (k *personKeptRig) dryRun(t *testing.T, caller personKeptCaller, f personKeptIssue, address string) int {
	t.Helper()
	response := k.send(t, caller, http.MethodDelete, f.nodePath(address, "dry_run=true"), nil, "")
	requirePersonKeptTaken(t, caller.name+"'s dry run of the delete of "+address, response, http.StatusOK)
	return decodeBody[struct {
		Count int `json:"count"`
	}](t, response).Count
}

// remove is caller's delete of address under f, naming its placement and count as a delete must
// (count, alice's dry run's).
func (k *personKeptRig) remove(t *testing.T, caller personKeptCaller, f personKeptIssue, address string, count int) *httptest.ResponseRecorder {
	t.Helper()
	_, tag := placementOf(t, k.database, refs.BlockNodeKind, address)
	return k.send(t, caller, http.MethodDelete, f.nodePath(address, "expected_count="+strconv.Itoa(count)), nil, tag)
}

// move is caller's PATCH of address under f to stand under parent, naming its placement.
func (k *personKeptRig) move(t *testing.T, caller personKeptCaller, f personKeptIssue, address, parent string) *httptest.ResponseRecorder {
	t.Helper()
	_, tag := placementOf(t, k.database, refs.BlockNodeKind, address)
	return k.send(t, caller, http.MethodPatch, f.nodePath(address, ""), map[string]any{"parent": parent}, tag)
}

// state is caller's write of value on the node address names under f.
func (k *personKeptRig) state(t *testing.T, caller personKeptCaller, key, address, value string) *httptest.ResponseRecorder {
	t.Helper()
	return k.send(t, caller, http.MethodPost, statePath(key, address, "state"), map[string]any{"state": value}, "")
}

// TestOnlyAPersonChangesAPersonsDecision is c-person-node-kept of the issue tree's Verification
// document (R32 of its spec): on every path that writes a person's decision an agent cannot change,
// move or remove it, and it can move, replace or delete any other node a person wrote.
//
// A person's decision is an answered ask - its own node and its `:::ask` block - and a decision
// whose decided_by names a person. Every issue here is one Legion does not run, so R28 masks
// nothing. The credentials are the deployment's shared token, an agent's, and the four kinds of
// token that hold the person scope, each obtained through the server's own paths: a token alice
// mints in the browser naming no scopes, which takes hers; a token minted with person among its
// scopes, and one minted without it and then changed to hold it; a broker token scoped to alice;
// and a token a personal token of alice's holding person and approve mints naming no scopes, which
// takes those two. No broker token mints a token: its mint is 403 OAUTH_SCOPE_REQUIRED naming
// approve, and mints nothing.
//
// On the text paths - an issue document's edit by its id and by its slug, a project document's
// edit by its id and by its project's slug, and both uploads - the shared token's change of the
// words, retype and removal of alice's answered ask and of her decision, and its transfer of each
// to another document, are each 409 ANSWERED_ASK_KEPT, quoting the question or the words and naming
// the block, and store nothing. So is a retype that keeps the decision's type and changes its
// words. The same token may move her decision inside its spec, among its siblings and into another
// section, while its PATCH of that decision is 409; may insert its own reading into her decision
// and write the decision's `shaped`, which change none of its words, so the reading and the link
// stand and the words and the placement do not move; may write `shaped` on its own decision alice
// kept, which leaves the decision's sum and her Keep standing, and may change that decision's
// words, a Keep making no decision a person's; and may change alice's prose and replace a document
// of hers that holds no decision of hers.
//
// On the node routes the shared token's PATCH moves a section that holds alice's decision, which
// stays in its spec with its words, and is 409 on the decision itself and on the `:::ask` block of
// her ask, while the ask's own node is no move target (400 NODE_NOT_MOVABLE). Its DELETE of her
// decision, of the section that holds one and of her ask's block is 409; alice's DELETE of that
// block is R7's 400 NODE_NOT_DELETABLE. On the state route its overtaken on her decision is 409, and
// so is its live once she marks the decision overtaken herself; its proposed is the settle's 403
// STATE_NOT_PERMITTED and its settled and gone 409 NODE_STATE_NOT_WRITABLE, today's refusals, which
// come first; every value on her ask's block is 409 ASK_HOLDS_STATE. On the own node of an ask an
// agent session asked and she answered, the asking session's proposed, overtaken and live, and a
// second session's, are each 409 ANSWERED_ASK_KEPT, the second's proposed included, which the
// settle's author rule would refuse 403 had it come first. No refused write stores anything.
//
// Each token that holds the person scope then makes every one of those writes on fixtures of its
// own, and each is taken: R32 refuses none of them.
//
// Last the Keep pass: two decisions of the agent's, e1 and e2, each with `shaped` written, kept by
// alice, e2's words then changed by her; their Keeps' sums are written back as the release before
// this one summed them, over their words with `shaped`. The census kind keep_shaped_sum counts 1,
// e1's; after graphcensus.Backfill e1's Keep stores the sum the Decisions read answers and waits for
// no review, e2's keeps the sum it held and still waits, and the kind counts 0.
func TestOnlyAPersonChangesAPersonsDecision(t *testing.T) {
	t.Parallel()
	k := &personKeptRig{brokerTokenRig: newBrokerTokenRig(t)}
	seedPeople(t, k.database, "carol")
	browser := personKeptBrowser()
	shared := personKeptBearer("the shared token", "agent-token")

	mint := func(t *testing.T, minter personKeptCaller, name string, scopes []string) agentTokenResponse {
		t.Helper()
		body := map[string]any{"name": name}
		if scopes != nil {
			body["scopes"] = scopes
		}
		response := k.send(t, minter, http.MethodPost, "/api/v1/me/agent-tokens", body, "")
		requirePersonKeptTaken(t, minter.name+"'s mint of "+name, response, http.StatusCreated)
		return decodeBody[agentTokenResponse](t, response)
	}
	holds := func(t *testing.T, what string, token agentTokenResponse, want ...string) {
		t.Helper()
		for _, scope := range want {
			if !slices.Contains(token.Scopes, scope) {
				t.Fatalf("%s holds the scopes %v, want %v among them", what, token.Scopes, want)
			}
		}
	}
	browserDefault := mint(t, browser, "minted in the browser naming no scopes", nil)
	holds(t, "the token alice minted in the browser naming no scopes", browserDefault, model.ScopePerson, model.ScopeApprove)
	namedPerson := mint(t, browser, "minted with person", []string{model.ScopePerson})
	holds(t, "the token minted with person", namedPerson, model.ScopePerson)
	later := mint(t, browser, "minted without person", []string{})
	if slices.Contains(later.Scopes, model.ScopePerson) {
		t.Fatalf("the token minted naming no scope holds %v", later.Scopes)
	}
	changed := k.send(t, browser, http.MethodPatch, "/api/v1/me/agent-tokens/"+later.ID, map[string]any{"scopes": []string{model.ScopePerson}}, "")
	requirePersonKeptTaken(t, "alice's change of the token minted without person to hold it", changed, http.StatusOK)
	holds(t, "the token changed to hold person", decodeBody[agentTokenResponse](t, changed), model.ScopePerson)
	minter := mint(t, browser, "holding person and approve", []string{model.ScopePerson, model.ScopeApprove})
	tokenMinted := mint(t, personKeptBearer("alice's personal token holding person and approve", minter.Token), "minted by a personal token", nil)
	holds(t, "the token a personal token minted naming no scopes", tokenMinted, model.ScopePerson, model.ScopeApprove)
	aliceKey := newBrokerCaller(t)
	const aliceBrokerToken = "a token the broker scoped to alice"
	k.broker.Says(aliceBrokerToken, personScopedTokenAnswer(aliceKey.jkt))
	aliceBroker := personKeptBroker("a broker token scoped to alice", aliceKey, aliceBrokerToken)

	people := []personKeptCaller{
		personKeptBearer("a token alice minted in the browser naming no scopes", browserDefault.Token),
		personKeptBearer("a token minted with person among its scopes", namedPerson.Token),
		personKeptBearer("a token minted without person and changed to hold it", later.Token),
		aliceBroker,
		personKeptBearer("a token a personal token holding person and approve minted naming no scopes", tokenMinted.Token),
	}

	t.Run("no broker token mints a token", func(t *testing.T) {
		tokens := countRows(t, k.database, `select (select count(*) from agent_tokens) + (select count(*) from agent_read_tokens)`)
		response := k.send(t, aliceBroker, http.MethodPost, "/api/v1/me/agent-tokens", map[string]any{"name": "from the broker"}, "")
		var refusal refusalAnswer
		_ = json.Unmarshal(response.Body.Bytes(), &refusal)
		if response.Code != http.StatusForbidden || refusal.Code != codeScopeRequired || !strings.Contains(refusal.Error, model.ScopeApprove) {
			t.Fatalf("broker token mint answered %d %s, want 403 %s naming %s: %s",
				response.Code, refusal.Code, codeScopeRequired, model.ScopeApprove, response.Body.String())
		}
		if after := countRows(t, k.database, `select (select count(*) from agent_tokens) + (select count(*) from agent_read_tokens)`); after != tokens {
			t.Fatalf("the broker token's refused mint moved the tokens from %d to %d", tokens, after)
		}
	})

	t.Run("the text paths", func(t *testing.T) {
		f := k.issue(t, "PKA")
		requirePersonKeptTaken(t, "the shared token's decision ad1", k.send(t, shared, http.MethodPost, f.edits(),
			map[string]any{"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": personKeptAgentDecisionCode}}}, ""), http.StatusOK)
		plan := k.send(t, shared, http.MethodPost, f.uploads(), map[string]any{
			"name": "plan.md", "content": "# Plan\n\n" + personKeptQuestion + " The ledger.\n\n:::decision\n" + personKeptWords + "\n:::\n",
		}, "")
		requirePersonKeptTaken(t, "the shared token's plan holding alice's words", plan, http.StatusCreated)
		k.documents = append(k.documents, decodeBody[struct {
			Artifact model.Artifact `json:"artifact"`
		}](t, plan).Artifact.ID)
		awaitSettled(t, k.database, f.spec.id)
		requireDecidedByNode(t, k.database, "the agent's decision ad1", f.node("ad1"), graphstore.KindSession, decidingSession)

		createReferenceAPIProject(t, k.handler, "PKD")
		p := k.projectDocument(t, "PKD")

		paths := []personKeptPath{
			{name: "the spec's edit by its id", path: f.edits()},
			{name: "the spec's edit by its issue's slug", path: f.slugEdits()},
			{name: "the issue's upload", path: f.uploads(), upload: personKeptSpecDocumentName},
		}
		projectPaths := []personKeptPath{
			{name: "the project document's edit by its id", path: p.edits()},
			{name: "the project document's edit by its project's slug", path: p.slugEdits()},
			{name: "the project's upload", path: p.uploads(), upload: personKeptProjectDocName},
		}
		before := k.snapshot(t)
		askNames := []string{refs.AskRef(textRefOf(f.key, f.ask)), strconv.Quote(personKeptQuestion), f.block("q1")}
		for _, path := range paths {
			for _, change := range personKeptAskChanges() {
				what := "agent " + change.what + " of the answered ask's block " + f.block("q1") + " through " + path.name
				k.requireKept(t, what, k.write(t, shared, path, f.spec.id, change), before, askNames...)
			}
			for _, change := range personKeptDecisionChanges("pd2", personKeptWords, personKeptRewordedWords) {
				what := "agent " + change.what + " of alice's decision " + f.block("pd2") + " through " + path.name
				k.requireKept(t, what, k.write(t, shared, path, f.spec.id, change), before, strconv.Quote(personKeptWords), f.block("pd2"))
			}
		}
		for _, path := range projectPaths {
			for _, change := range personKeptDecisionChanges("jd1", personKeptProjectWords, personKeptRewordedProject) {
				what := "agent " + change.what + " of alice's project decision " + p.block("jd1") + " through " + path.name
				k.requireKept(t, what, k.write(t, shared, path, p.document.ID, change), before, strconv.Quote(personKeptProjectWords), p.block("jd1"))
			}
		}
		if ask := readBlockAsk(t, k.handler, f.ask); ask.State != "answered" || ask.Answer == nil || ask.Answer.User != "alice" {
			t.Fatalf("q1 after the agent's refused writes = state %q answer %#v, want alice's answer", ask.State, ask.Answer)
		}

		// The agent moves alice's decision inside its spec: among its siblings, and into another
		// section. Each is taken, the decision keeps its words, and its PATCH of the decision is
		// still refused.
		words := k.entry(t, f, "pd2")
		for _, move := range []struct {
			what string
			ops  []map[string]any
		}{
			{"agent move of alice's decision past its siblings", []map[string]any{
				{"op": "move", "block": "pd2", "after": "end"},
			}},
			// A move by quote uses the top-level boundary. To nest the decision, relocate
			// the same block id and words in one batch with an explicit container.
			{"agent move of alice's decision into the section s2", []map[string]any{
				{"op": "delete", "block": "pd2"},
				{"op": "insert", "into": "block:s2", "markdown": ":::decision{#pd2}\n" + personKeptWords + "\n:::\n"},
			}},
		} {
			requirePersonKeptTaken(t, move.what, k.send(t, shared, http.MethodPost, f.edits(), map[string]any{"ops": move.ops}, ""), http.StatusOK)
			awaitSettled(t, k.database, f.spec.id)
			if moved := k.entry(t, f, "pd2"); moved.Document == nil || moved.Document.ArtifactID != f.spec.id || moved.Text != words.Text {
				t.Fatalf("after the %s it reads %s, want it in the spec %s with its words %q", move.what, entryJSON(t, moved), f.spec.id, words.Text)
			}
		}
		if placed, _ := placementOf(t, k.database, refs.BlockNodeKind, f.node("pd2")); placed.ToID != f.node("s2") {
			t.Fatalf("alice's decision stands under %s %s after the agent moved it into s2, want %s", placed.ToKind, placed.ToID, f.node("s2"))
		}
		before = k.snapshot(t)
		k.requireKept(t, "agent PATCH of "+f.node("pd2"), k.move(t, shared, f, f.node("pd2"), f.node("s1")), before, strconv.Quote(personKeptWords))

		// The agent's reading inserted into alice's decision, and its retype writing the
		// decision's shaped, change none of its words.
		placement, _ := placementOf(t, k.database, refs.BlockNodeKind, f.node("pd2"))
		pointer := "dispatch://" + f.key + "/spec#b-r1"
		for _, write := range []struct {
			what string
			op   map[string]any
		}{
			{"agent reading inserted into alice's decision", map[string]any{
				"op": "insert", "into": "block:pd2", "markdown": ":::reading\nThe agent reads it as one ledger for every writer.\n:::\n",
			}},
			{"agent retype writing the shaped of alice's decision", map[string]any{
				"op": "retype", "block": "pd2", "type": "decision", "attributes": map[string]any{"shaped": []string{pointer}},
			}},
		} {
			requirePersonKeptTaken(t, write.what, k.send(t, shared, http.MethodPost, f.edits(), map[string]any{"ops": []map[string]any{write.op}}, ""), http.StatusOK)
		}
		awaitSettled(t, k.database, f.spec.id)
		read := k.entry(t, f, "pd2")
		linked := slices.ContainsFunc(read.Shaped, func(link model.DecisionLink) bool { return link.To.ID == f.spec.pointed("r1") })
		if len(read.Readings) != 1 || !linked {
			t.Fatalf("alice's decision after the agent's reading and shaped reads the readings %s and the links %s, want the reading and r1",
				entryJSON(t, read.Readings), entryJSON(t, read.Shaped))
		}
		if read.Text != words.Text || read.TextSHA256 != words.TextSHA256 {
			t.Fatalf("the agent's reading and shaped changed alice's decision from %q (%s) to %q (%s)", words.Text, words.TextSHA256, read.Text, read.TextSHA256)
		}
		if placed, _ := placementOf(t, k.database, refs.BlockNodeKind, f.node("pd2")); placed.ToKind != placement.ToKind || placed.ToID != placement.ToID {
			t.Fatalf("the agent's reading and shaped moved alice's decision from %s %s to %s %s", placement.ToKind, placement.ToID, placed.ToKind, placed.ToID)
		}

		// alice's Keep of the agent's decision makes it no person's decision: its shaped written
		// by the agent leaves its sum and her Keep standing, and the agent may change its words.
		agents := k.entry(t, f, "ad1")
		keepBlock(t, k.handler, f.spec.id, "ad1", agents.TextSHA256)
		requirePersonKeptTaken(t, "agent retype writing the shaped of its decision alice kept", k.send(t, shared, http.MethodPost, f.edits(), map[string]any{
			"ops": []map[string]any{{"op": "retype", "block": "ad1", "type": "decision", "attributes": map[string]any{"shaped": []string{pointer}}}},
		}, ""), http.StatusOK)
		awaitSettled(t, k.database, f.spec.id)
		if kept := k.entry(t, f, "ad1"); kept.TextSHA256 != agents.TextSHA256 || kept.Kept == nil || kept.Kept.KeptBy != "alice" || kept.WaitingReview {
			t.Fatalf("the agent's shaped of %s moved its text_sha256 from %s to %s, and its Keep reads %s with waiting_review %t, want alice's Keep standing",
				f.node("ad1"), agents.TextSHA256, kept.TextSHA256, entryJSON(t, kept.Kept), kept.WaitingReview)
		}
		requireDecidedByNode(t, k.database, "the agent's decision ad1 alice kept", f.node("ad1"), graphstore.KindSession, decidingSession)
		requirePersonKeptTaken(t, "agent change of the words of its decision alice kept", k.send(t, shared, http.MethodPost, f.slugEdits(), map[string]any{
			"ops": []map[string]any{{"op": "replace", "find": personKeptAgentWords, "with": "We cache every read for an hour."}},
		}, ""), http.StatusOK)
		requirePersonKeptTaken(t, "agent change of alice's prose", k.send(t, shared, http.MethodPost, f.edits(), map[string]any{
			"ops": []map[string]any{{"op": "replace", "find": personKeptProse, "with": "The agent rewrote alice's paragraph."}},
		}, ""), http.StatusOK)
		notes := k.send(t, browser, http.MethodPost, f.uploads(), map[string]any{"name": "notes.md", "content": "# Notes\n\nAlice's notes, no decision among them.\n"}, "")
		requirePersonKeptTaken(t, "alice's notes", notes, http.StatusCreated)
		notesID := decodeBody[struct {
			Artifact model.Artifact `json:"artifact"`
		}](t, notes).Artifact.ID
		k.documents = append(k.documents, notesID)
		requirePersonKeptTaken(t, "agent replacement of alice's notes, which hold no decision of hers", k.send(t, shared, http.MethodPost, f.uploads(), map[string]any{
			"name": "notes.md", "content": "# Notes\n\nThe agent's notes.\n", "precondition": uploadBase(t, k.handler, notesID),
		}, ""), http.StatusCreated)
	})

	t.Run("the node routes and the state route", func(t *testing.T) {
		f := k.issue(t, "PKN")
		pd1, pd2, q1 := f.node("pd1"), f.node("pd2"), f.node("q1")

		// The state route on alice's decision.
		before := k.snapshot(t)
		k.requireKept(t, "agent state overtaken of "+pd2, k.state(t, shared, f.key, pd2, graphstore.StateOvertaken), before, strconv.Quote(personKeptWords))
		if held := k.stateOf(t, refs.BlockNodeKind, pd2); held != graphstore.StateLive {
			t.Fatalf("agent state overtaken of %s answered 409 and left it %s, want live", pd2, held)
		}
		requirePersonKeptTaken(t, "alice's overtaken of her decision", k.state(t, browser, f.key, pd2, graphstore.StateOvertaken), http.StatusOK)
		before = k.snapshot(t)
		k.requireKept(t, "agent state live of "+pd2, k.state(t, shared, f.key, pd2, graphstore.StateLive), before, strconv.Quote(personKeptWords))
		k.requireRefusal(t, "agent state proposed of "+pd2, k.state(t, shared, f.key, pd2, graphstore.StateProposed), before, http.StatusForbidden, codeStateNotPermitted, f.block("pd2"))
		for _, value := range []string{graphstore.StateSettled, graphstore.StateGone} {
			k.requireRefusal(t, "agent state "+value+" of "+pd2, k.state(t, shared, f.key, pd2, value), before, http.StatusConflict, codeStateNotWritable, f.block("pd2"))
		}
		if held := k.stateOf(t, refs.BlockNodeKind, pd2); held != graphstore.StateOvertaken {
			t.Fatalf("the agent's refused state writes left alice's decision %s, want the overtaken she wrote", held)
		}
		requirePersonKeptTaken(t, "alice's live of her decision", k.state(t, browser, f.key, pd2, graphstore.StateLive), http.StatusOK)
		for _, person := range people {
			for _, value := range []string{graphstore.StateOvertaken, graphstore.StateLive} {
				requirePersonKeptTaken(t, person.name+"'s state "+value+" of "+pd2, k.state(t, person, f.key, pd2, value), http.StatusOK)
				if held := k.stateOf(t, refs.BlockNodeKind, pd2); held != value {
					t.Fatalf("%s's state %s of %s left it %s", person.name, value, pd2, held)
				}
			}
		}
		before = k.snapshot(t)
		for _, value := range []string{graphstore.StateOvertaken, graphstore.StateLive, graphstore.StateProposed, graphstore.StateGone} {
			k.requireRefusal(t, "agent state "+value+" of the :::ask block "+q1, k.state(t, shared, f.key, q1, value), before, http.StatusConflict, codeAskHoldsState, f.ask)
		}

		// The DELETE: alice's decision, the section holding her other one, and her ask's block.
		k.requireKept(t, "agent DELETE of "+pd2, k.remove(t, shared, f, pd2, k.dryRun(t, browser, f, pd2)), before, strconv.Quote(personKeptWords))
		k.requireKept(t, "agent DELETE of the section "+f.node("s1")+", which holds "+pd1,
			k.remove(t, shared, f, f.node("s1"), k.dryRun(t, browser, f, f.node("s1"))), before, strconv.Quote(personKeptSectionWords))
		k.requireKept(t, "agent DELETE of the :::ask block "+q1, k.send(t, shared, http.MethodDelete, f.nodePath(q1, "expected_count=1"), nil, ""),
			before, strconv.Quote(personKeptQuestion))
		k.requireRefusal(t, "alice's DELETE of the :::ask block "+q1, k.send(t, browser, http.MethodDelete, f.nodePath(q1, "expected_count=1"), nil, ""),
			before, http.StatusBadRequest, codeNodeNotDeletable)

		// The PATCH: alice's decision and her ask's block are no agent's to move, and the ask's own
		// node is no move target at all.
		k.requireKept(t, "agent PATCH of "+pd1, k.move(t, shared, f, pd1, f.node("s2")), before, strconv.Quote(personKeptSectionWords))
		k.requireKept(t, "agent PATCH of the :::ask block "+q1, k.move(t, shared, f, q1, f.node("s2")), before, strconv.Quote(personKeptQuestion))
		k.requireRefusal(t, "agent PATCH of the ask "+f.askNode(),
			k.send(t, shared, http.MethodPatch, f.nodePath(f.askNode(), ""), map[string]any{"parent": f.node("s2")}, ""),
			before, http.StatusBadRequest, codeNodeNotMovable)

		// The agent moves the section that holds alice's decision: the decision stays in its spec,
		// with its words.
		held := k.entry(t, f, "pd1")
		requirePersonKeptTaken(t, "agent PATCH of the section "+f.node("s1"), k.move(t, shared, f, f.node("s1"), f.node("s2")), http.StatusOK)
		awaitSettled(t, k.database, f.spec.id)
		if moved := k.entry(t, f, "pd1"); moved.Document == nil || moved.Document.ArtifactID != f.spec.id || moved.Text != held.Text {
			t.Fatalf("alice's decision after the agent moved its section reads %s, want it in the spec %s with its words %q",
				entryJSON(t, moved), f.spec.id, held.Text)
		}
	})

	t.Run("the own node of an ask an agent asked and alice answered", func(t *testing.T) {
		requirePersonKeptTaken(t, "alice's project "+brokerTestProject, k.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{
			"key": brokerTestProject, "name": "Broker",
		}), http.StatusCreated)
		issue := createSpecIssue(t, k.handler, brokerTestProject, "The sessions ask here.\n")
		k.documents = append(k.documents, issue.PrimaryArtifactID)
		session := func(name, id, owner, token string) personKeptCaller {
			caller := newBrokerCaller(t)
			answer := liveNamedToken(caller.jkt)
			answer.Session, answer.Person = id, owner
			k.broker.Says(token, answer)
			return personKeptBroker(name, caller, token)
		}
		asker := session("the asking session", "ses_a5a5a5a5a5a5", "bob", "the asking session's token")
		second := session("a second session", "ses_c3c3c3c3c3c3", "carol", "the second session's token")
		asked := k.send(t, asker, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
			"question": personKeptQuestion, "options": []map[string]string{{"label": "Ledger"}, {"label": "Log"}},
		}, "")
		requirePersonKeptTaken(t, "the asking session's ask", asked, http.StatusCreated)
		ask := decodeBody[model.Ask](t, asked)
		answerAskForChange(t, k.handler, ask.ID, "alice", "Ledger", nil)
		node := graphstore.KindAsk + "/" + ask.ID
		state := k.stateOf(t, graphstore.KindAsk, ask.ID)
		before := k.snapshot(t)
		for _, caller := range []personKeptCaller{asker, second} {
			for _, value := range []string{graphstore.StateProposed, graphstore.StateOvertaken, graphstore.StateLive} {
				what := "agent state " + value + " of " + node + " by " + caller.name
				k.requireKept(t, what, k.state(t, caller, issue.Key, node, value), before, ask.ID, strconv.Quote(personKeptQuestion))
				if held := k.stateOf(t, graphstore.KindAsk, ask.ID); held != state {
					t.Fatalf("%s answered 409 and left the ask %s, want %s", what, held, state)
				}
			}
		}
		if read := readBlockAsk(t, k.handler, ask.ID); read.State != "answered" || read.Answer == nil || read.Answer.User != "alice" {
			t.Fatalf("the ask after the sessions' refused writes = state %q answer %#v, want alice's answer", read.State, read.Answer)
		}
	})

	for index, person := range people {
		t.Run(person.name, func(t *testing.T) {
			project := "PKT" + string(rune('A'+index))
			f := k.issue(t, project)
			for _, value := range []string{graphstore.StateOvertaken, graphstore.StateLive} {
				requirePersonKeptTaken(t, person.name+"'s state "+value+" of alice's decision", k.state(t, person, f.key, f.node("pd1"), value), http.StatusOK)
			}
			requirePersonKeptTaken(t, person.name+"'s change of the words of alice's decision through the spec's edit by its id",
				k.send(t, person, http.MethodPost, f.edits(), map[string]any{
					"ops": []map[string]any{{"op": "replace", "find": personKeptWords, "with": personKeptRewordedWords}},
				}, ""), http.StatusOK)
			requirePersonKeptTaken(t, person.name+"'s removal of the answered ask's block through the spec's edit by its issue's slug",
				k.send(t, person, http.MethodPost, f.slugEdits(), map[string]any{"ops": []map[string]any{{"op": "delete", "block": "q1"}}}, ""), http.StatusOK)
			awaitSettled(t, k.database, f.spec.id)
			requirePersonKeptTaken(t, person.name+"'s PATCH of alice's decision", k.move(t, person, f, f.node("pd1"), f.node("s2")), http.StatusOK)
			awaitSettled(t, k.database, f.spec.id)
			requirePersonKeptTaken(t, person.name+"'s DELETE of alice's decision",
				k.remove(t, person, f, f.node("pd2"), k.dryRun(t, person, f, f.node("pd2"))), http.StatusOK)
			awaitSettled(t, k.database, f.spec.id)
			requirePersonKeptTaken(t, person.name+"'s upload changing the words of alice's decision",
				k.write(t, person, personKeptPath{name: "the issue's upload", path: f.uploads(), upload: personKeptSpecDocumentName}, f.spec.id, personKeptChange{
					upload: personKeptReword(personKeptSectionWords, "We write through two ledgers."),
				}), http.StatusCreated)

			p := k.projectDocument(t, project)
			requirePersonKeptTaken(t, person.name+"'s change of the words of alice's project decision through its project's slug",
				k.send(t, person, http.MethodPost, p.slugEdits(), map[string]any{
					"ops": []map[string]any{{"op": "replace", "find": personKeptProjectWords, "with": personKeptRewordedProject}},
				}, ""), http.StatusOK)
			requirePersonKeptTaken(t, person.name+"'s retype of alice's project decision through its id",
				k.send(t, person, http.MethodPost, p.edits(), map[string]any{"ops": []map[string]any{{
					"op": "retype", "block": "jd2", "type": "callout", "attributes": map[string]any{"kind": "note", "title": "Was decided"},
				}}}, ""), http.StatusOK)
			awaitSettled(t, k.database, p.document.ID)
			requirePersonKeptTaken(t, person.name+"'s upload without alice's project decision",
				k.write(t, person, personKeptPath{name: "the project's upload", path: p.uploads(), upload: personKeptProjectDocName}, p.document.ID, personKeptChange{
					upload: personKeptCut(":::decision{#jd3"),
				}), http.StatusCreated)
		})
	}

	t.Run("the Keep pass", func(t *testing.T) {
		ctx := context.Background()
		createReferenceAPIProject(t, k.handler, "PKK")
		issue := createSpecIssue(t, k.handler, "PKK", ":::requirement{#r1}\nEvery write reaches the store once.\n:::\n")
		spec := readDecisionDocument(t, k.database, issue.Key, "spec")
		k.documents = append(k.documents, spec.id)
		edits := "/api/v1/artifacts/" + spec.id + "/edits"
		requirePersonKeptTaken(t, "the agent's decisions e1 and e2", k.send(t, shared, http.MethodPost, edits, map[string]any{"ops": []map[string]any{{
			"op": "insert", "after": "end", "markdown": ":::decision{#e1}\nWe page every read.\n:::\n\n:::decision{#e2}\nWe batch the writes.\n:::\n",
		}}}, ""), http.StatusOK)
		pointer := "dispatch://" + issue.Key + "/spec#b-r1"
		for _, id := range []string{"e1", "e2"} {
			requirePersonKeptTaken(t, "the agent's shaped of "+id, k.send(t, shared, http.MethodPost, edits, map[string]any{"ops": []map[string]any{{
				"op": "retype", "block": id, "type": "decision", "attributes": map[string]any{"shaped": []string{pointer}},
			}}}, ""), http.StatusOK)
		}
		awaitSettled(t, k.database, spec.id)
		entries := readEntries(t, k.handler, issue.Key)
		for _, id := range []string{"e1", "e2"} {
			keepBlock(t, k.handler, spec.id, id, requireEntry(t, entries, id, refs.BlockNodeKind+":"+spec.address(id)).TextSHA256)
		}
		unchanged := documentMarkdown(t, k.handler, spec.id)
		requirePersonKeptTaken(t, "alice's change of e2's words", dispatchRequest(t, k.handler, http.MethodPost, edits, map[string]any{
			"ops": []map[string]any{{"op": "replace", "find": "We batch the writes.", "with": "We batch every write."}},
		}, "alice"), http.StatusOK)
		awaitSettled(t, k.database, spec.id)
		e1Sum := personKeptShapedSum(t, documentMarkdown(t, k.handler, spec.id), "e1")
		e2Sum := personKeptShapedSum(t, unchanged, "e2")
		personKeptKeepAsBefore(t, k.database, spec, "e1", e1Sum)
		personKeptKeepAsBefore(t, k.database, spec, "e2", e2Sum)

		census := func(t *testing.T) int64 {
			t.Helper()
			report, err := graphcensus.StoreCensus(ctx, k.database.Pool.Config().ConnString(), false, nil)
			if err != nil {
				t.Fatalf("take the store census: %v", err)
			}
			for _, kind := range report.Kinds {
				if kind.Kind == "keep_shaped_sum" {
					return kind.Owed
				}
			}
			t.Fatalf("the store census reported no kind keep_shaped_sum")
			return 0
		}
		if owed := census(t); owed != 1 {
			t.Errorf("census kind keep_shaped_sum reads %d, want %d", owed, 1)
		}
		if _, err := graphcensus.Backfill(ctx, k.database, io.Discard); err != nil {
			t.Fatalf("run the backfill: %v", err)
		}
		after := readEntries(t, k.handler, issue.Key)
		e1 := requireEntry(t, after, "e1", refs.BlockNodeKind+":"+spec.address("e1"))
		if row, value := personKeptStoredKeep(t, k.database, spec, "e1"); e1.WaitingReview || row != e1.TextSHA256 || value != e1.TextSHA256 ||
			e1.Kept == nil || e1.Kept.KeptBy != "alice" {
			t.Errorf("the Keep of %s reads waiting_review %t after the backfill, stored %s (its kept_text_sum %s, the sum written %s), want %s kept by alice",
				spec.address("e1"), e1.WaitingReview, row, value, e1Sum, e1.TextSHA256)
		}
		e2 := requireEntry(t, after, "e2", refs.BlockNodeKind+":"+spec.address("e2"))
		if row, value := personKeptStoredKeep(t, k.database, spec, "e2"); row != e2Sum || value != e2Sum {
			t.Errorf("the pass rewrote the Keep of %s, which did not match: %s to %s (its kept_text_sum %s)", spec.address("e2"), e2Sum, row, value)
		}
		if !e2.WaitingReview {
			t.Errorf("the Keep of %s reads waiting_review false after the backfill, its sum %s: its Keep did not match, so it waits", spec.address("e2"), e2.TextSHA256)
		}
		if owed := census(t); owed != 0 {
			t.Errorf("census kind keep_shaped_sum reads %d, want %d", owed, 0)
		}
	})
}

// textRefOf is the ask id of the issue key as an ask's address is written from.
func textRefOf(key, id string) text.Ref { return text.Ref{IssueKey: key, ID: id} }

// personKeptShapedSum is the sum a release before this one stored for a Keep of the decision id of
// markdown: the decision rendered alone with its shaped attribute, without the readings it closes
// with (pmdoc.Render), as refs.DecisionWords rendered it before shaped left the words.
func personKeptShapedSum(t *testing.T, markdown, id string) string {
	t.Helper()
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		t.Fatalf("read the spec back: %v", err)
	}
	var block *pmdoc.Node
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type == refs.DecisionSupertag && refs.TypedBlockID(node) == id {
			block = node
		}
		return block == nil
	})
	if block == nil {
		t.Fatalf("the spec holds no decision %s:\n%s", id, markdown)
	}
	content, _ := pmdoc.TypedContent(block)
	whole := *block
	whole.Children = content
	words, err := pmdoc.Render(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{&whole}})
	if err != nil {
		t.Fatalf("render %s with its shaped: %v", id, err)
	}
	if !strings.Contains(words, "shaped=") {
		t.Fatalf("%s renders %q, want its shaped attribute among it", id, words)
	}
	return decisionread.TextSum(words)
}

// personKeptKeepAsBefore writes the Keep of the decision id of spec as a release before this one
// stored it, by SQL: sum as its decision_keeps row's text_sha256, and appended as its node's
// kept_text_sum value.
func personKeptKeepAsBefore(t *testing.T, database *store.Store, spec decisionDocument, id, sum string) {
	t.Helper()
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}
	at := time.Now().UTC().Truncate(time.Microsecond)
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	written, err := tx.Exec(ctx, `update decision_keeps set text_sha256 = $3 where artifact_id = $1::uuid and block_id = $2`, spec.id, id, sum)
	if err != nil || written.RowsAffected() != 1 {
		t.Fatalf("write the Keep of %s as a release before stored it: %d rows, %v", id, written.RowsAffected(), err)
	}
	if err := graphstore.Write(ctx, tx, []graphstore.Node{graphstore.PersonNode(alice.ID, alice, at, graphstore.NoEvent)}, []graphstore.Tuple{{
		FromKind: refs.BlockNodeKind, FromID: spec.address(id), Kind: refs.ValueKeptTextSum,
		ToKind: graphstore.KindValue, ToID: sum, At: at, Who: alice, SourceEvent: graphstore.NoEvent, Asserted: true,
	}}); err != nil {
		t.Fatalf("append the kept_text_sum of %s: %v", id, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// personKeptStoredKeep is what the Keep of the decision id of spec stores now: its decision_keeps
// row's sum and its node's current kept_text_sum.
func personKeptStoredKeep(t *testing.T, database *store.Store, spec decisionDocument, id string) (row, value string) {
	t.Helper()
	ctx := context.Background()
	if err := database.Pool.QueryRow(ctx, `select text_sha256 from decision_keeps where artifact_id = $1::uuid and block_id = $2`, spec.id, id).Scan(&row); err != nil {
		t.Fatalf("read the Keep of %s: %v", id, err)
	}
	value, _, err := graphstore.CurrentValue(ctx, database.Pool, refs.BlockNodeKind, spec.address(id), refs.ValueKeptTextSum)
	if err != nil {
		t.Fatal(err)
	}
	return row, value
}
