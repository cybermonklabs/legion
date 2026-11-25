package api

//ci:reads packages/dispatch/web/src/features/issue-page/graph-edit/__fixtures__/graph-edit.json TestGraphEditWebFixture

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// graphEditFixturePath is the file TestGraphEditWebFixture writes, from this package's directory:
// the answers the unit tests of the issue page's graph editing (graph-edit/) read.
var graphEditFixturePath = filepath.Join("..", "..", "..", "..", "dispatch", "web", "src", "features", "issue-page",
	"graph-edit", "__fixtures__", "graph-edit.json")

// The projects of the fixture's issues: the seed's, and the one of the issue whose tree artifact
// owes its block index, which stands on a server of its own (graphEditOwed).
const (
	graphEditProject     = "GEDIT"
	graphEditOwedProject = "GOWED"
)

// The words of the seed, each unique in its store, so a block is found by them (blockWithText) and
// a comment's anchor by its quote.
const (
	graphEditMemoWords        = "The memo the graph keeps first."
	graphEditQuestionWords    = "Which edits does the graph take?"
	graphEditAnswerWords      = "Indent, outdent, the two moves and the delete."
	graphEditCriterionWords   = "The reader edits the graph by keyboard alone."
	graphEditStatement1Words  = "An indent is one write of the node's place."
	graphEditStatement2Words  = "A move names the placement it read."
	graphEditStatement3Words  = "A refused edit stores nothing at all."
	graphEditStatement3Quote  = "stores nothing"
	graphEditDecisionWords    = "The graph keeps one write for each move."
	graphEditReadingWords     = "The agent reads each move as one write."
	graphEditProseWords       = "A paragraph of the issue's own words."
	graphEditPlacedWords      = "The prose block the seed places under the criterion."
	graphEditAgentProseWords  = "The agent's prose block under the third statement."
	graphEditChildWords       = "The child's design paragraph."
	graphEditSecondChildWords = "The second child's paragraph."
	graphEditListWords        = "The first item of the list."
	graphEditOwedWords        = "A paragraph the page writes into the tree artifact."
)

// graphEditCaller is one caller of the fixture's requests: the name the fixture gives it, and the
// person's login it signs in with, or the session it names as its actor on the deployment's shared
// agent token; a caller with neither is that token naming no session, as an agent's read is.
type graphEditCaller struct {
	name, login, session string
}

// The callers: alice, the person; the architect, the session that holds the seed issue's
// architect role; the worker, a session that holds its implementer role and no architect's; and an
// agent reading on the shared token.
var (
	graphEditPerson    = graphEditCaller{name: "<person>", login: "alice"}
	graphEditArchitect = graphEditCaller{name: "<architect>", session: "ses-architect-gedit"}
	graphEditWorker    = graphEditCaller{name: "<worker>", session: "ses-implementer-gedit"}
	graphEditAgent     = graphEditCaller{name: "<agent>"}
)

// graphEditExchange is one request the fixture holds and the answer the handlers gave it: the
// method, the path with its query (its {id} segment shown as the address it names, where the
// request sent it percent-encoded), the If-Match it sent, the caller, the body it sent, and the
// status and the body of the answer. node is the seed's name of the node the request moves, which
// names a placement_seq the answer gives with no node beside it.
type graphEditExchange struct {
	method, path string
	ifMatch      *string
	as           graphEditCaller
	body         any
	response     *httptest.ResponseRecorder
	node         string
}

// graphEditSend sends method to sent on handler as as, with body and the If-Match tag where tag is
// not "", and answers the exchange, whose path is shown.
func graphEditSend(t *testing.T, handler http.Handler, as graphEditCaller, method, shown, sent, tag string, body map[string]any) *graphEditExchange {
	t.Helper()
	header := http.Header{}
	var payload any
	switch {
	case as.login != "":
		header.Set("X-Dispatch-User", as.login)
		if body != nil {
			payload = body
		}
	case as.session != "":
		header.Set("Authorization", "Bearer agent-token")
		named := maps.Clone(body)
		if named == nil {
			named = map[string]any{}
		}
		named["actor"] = map[string]any{"kind": "session", "id": as.session}
		payload = named
	default:
		header.Set("Authorization", "Bearer agent-token")
		if body != nil {
			payload = body
		}
	}
	exchange := &graphEditExchange{method: method, path: shown, as: as, body: payload}
	if tag != "" {
		header.Set("If-Match", tag)
		exchange.ifMatch = &tag
	}
	exchange.response = serveNodeRequest(t, handler, method, sent, payload, header)
	return exchange
}

// graphEditRead is the tree read of the issue key with query, as as, on handler.
func graphEditRead(t *testing.T, handler http.Handler, as graphEditCaller, key, query string) *graphEditExchange {
	t.Helper()
	path := "/api/v1/issues/" + key + "/tree" + query
	return graphEditSend(t, handler, as, http.MethodGet, path, path, "", nil).taken(t, as.name+"'s tree read of "+key+query)
}

// taken fails unless the exchange answered 200, naming what, and answers it.
func (e *graphEditExchange) taken(t *testing.T, what string) *graphEditExchange {
	t.Helper()
	if e.response.Code != http.StatusOK {
		t.Fatalf("%s answered %d %s, want 200", what, e.response.Code, e.response.Body.String())
	}
	return e
}

// refused fails unless the exchange answered status with code, naming what (refusedWith), and
// answers it.
func (e *graphEditExchange) refused(t *testing.T, what string, status int, code string) *graphEditExchange {
	t.Helper()
	refusedWith(t, what, e.response, status, code)
	return e
}

// count is the count a delete, its dry run or its 409 DELETE_COUNT_MOVED answered.
func (e *graphEditExchange) count(t *testing.T) int {
	t.Helper()
	return decodeBody[nodeDeleted](t, e.response).Count
}

// graphEditReads is a tree read as the person and the same read as an agent, which the fixture
// holds in full only where its bytes, once named, differ from the person's.
type graphEditReads struct {
	person, agent *graphEditExchange
}

// graphEditSeed is the seed of the Verification document of INFRA-5705 (its first paragraph),
// made through the handlers on a server of the move's tests (moveRig), with the addresses of its
// nodes as the node routes take them.
type graphEditSeed struct {
	rig   *moveRig
	names *graphEditNames
	key   string
	child string
	done  string
	// unconverted is the issue the conversion has not reached.
	unconverted string
	tree        decisionDocument
	// The nodes of the converted issue, each by the address the node routes take.
	questionBlock, criterion, statement1, statement2, statement3, checkShared, checkAgent string
	prose, comment, constraint, listParagraph                                             string
	// doneStatement is a statement of the done copy, which no node route finds under key.
	doneStatement string
	// childParagraph is the paragraph of the child's room.
	childParagraph string
	// staleTag is the placement_seq the third statement held before its first move, as an If-Match.
	staleTag string
}

// TestGraphEditWebFixture writes, and with no flag holds to the committed bytes, the answers the
// graph-edit unit tests of the issue page read: graph-edit/__fixtures__/graph-edit.json under
// packages/dispatch/web/src/features/issue-page. Each answer is the handlers' own, made through
// the routes on the seed of INFRA-5705's Verification document (its first paragraph), with the
// names below in place of each value a run makes afresh. `go test ./internal/dispatch/api/ -run
// '^TestGraphEditWebFixture$' -args -update` rewrites the file; with no flag the test fails where
// the file differs from what the handlers answer, naming the stale member and the first path in it
// that differs.
//
// The seed: a converted issue Legion runs (label legion, with an architect and an implementer
// published as its role holders), whose spec opens with the memo callout and holds a person's
// question (an `:::ask` block she answers), a criterion under the question, and three statements
// serving the criterion, the second written by the architect; a check of the Verification document
// serving all three and standing under the first, with one recorded result; a second check, the
// architect's, under the second statement; a plan task; a person's comment on the text of the third
// statement; a person's message with the architect's reply; the architect's decision with its
// reading; a pre-mortem lens on the third statement with one finding and one risk; a constraint;
// and, in the issue's own fragment, a paragraph placed under the criterion after the three
// statements, and under the issue a paragraph, a heading, a list, a table and a code block. Beside
// it: a child, a view of its spec, whose check serves the first statement; a done copy holding a
// criterion and a statement; and an issue the conversion has not reached.
//
// The members, each answer with the request that made it ({method, path, if_match, login, body},
// the {id} of a path shown unescaped, as the address the request percent-encoded):
//
//   - tree: the tree read with no view of the converted issue, the child, the done copy and the
//     unconverted issue, each as the person and as an agent; an agent's read the person's bytes
//     equal is held as its request and same_as_person.
//
//   - paged: the converted issue's tree read on a server whose Deps.IssueTreePageSize is half its
//     nodes, page_size beside its pages, the first carrying a cursor.
//
//   - owed: the tree read after a deferred write to an issue's tree fragment, and after the index
//     runs. The deferred typed-node pass exposes the unranked requirement; the new prose is
//     absent until the index runs.
//
//   - moved: the PATCH answers of an indent, an outdent, a move up and a move down of the third
//     statement, as the person.
//
//   - refused: each PATCH refusal the seed gives, keyed by its code, a second refusal of a code
//     keyed by the code and a word.
//
//   - deleted: the DELETE's dry run of the third statement (dry_run), the delete taken with the
//     count of its 409 DELETE_COUNT_MOVED (taken), and each DELETE refusal (refused), keyed as
//     refused is.
//
// The names: each issue key, artifact id, node id, block id a run mints, placement_seq, login and
// time is replaced, in every string and every object key, before the bytes are compared or
// written, so two runs on fresh stores write the same bytes:
//
//   - issues: <issue> (the converted issue), <child>, <done>, <unconverted>, <child-2> (the
//     second child the PARENT_CONTENDED refusals take), <owed-issue>; a document of each by its
//     issue's prefix and its slug, <spec>, <tree>, <verification>, <plan>, <child-spec>,
//     <notes-u>, and so on. A document's ref_key reads `<issue>/<slug>`.
//   - nodes: <memo>, <question> (the ask) and <question-block> (its `:::ask` block),
//     <criterion>, <statement-1>, <statement-2>, <statement-3>, <check-shared>, <check-agent>,
//     <plan-task>, <decision>, <reading>, <result>, <comment-on-statement-3> (also its anchor's
//     mark id), <message>, <agent-reply>, <lens>, <finding>, <risk>, <constraint>,
//     <prose-under-criterion>, <agent-prose>, <child-check>, <child-paragraph>, <list-paragraph>,
//     <child-2-paragraph>, <done-criterion>, <done-statement>, <unconverted-criterion>,
//     <unconverted-statement>, <owed-criterion>, <owed-requirement>, <owed-paragraph>.
//     Other minted block ids use their parent's name (or their document at the top) and their
//     type, as in <tree/bullet_list/list_item-2/paragraph>. Same-type siblings are numbered by
//     their text.
//   - callers: <person>, <architect>, <worker>, and <agent> for the shared token naming no session.
//   - placement_seq: <seq:<node>> after the node it places, <seq:<node>@2> for the second the node
//     held, and so on; in a body, an If-Match and a refusal's words alike.
//   - rank: <rank:N>, N the rank's order among the ranks of its answer, zero-padded, so the names
//     sort as the ranks do; a version number: its order among the version numbers of its answer.
//   - time: <time>; a cursor: <cursor:<kind>:<id>>, the node it starts at.
func TestGraphEditWebFixture(t *testing.T) {
	t.Parallel()
	names := newGraphEditNames(t)
	seed := seedGraphEdit(t, names)
	trees := seed.trees(t)
	fixture := map[string]any{"tree": trees, "paged": seed.paged(t, trees["converted"].(*graphEditReads).person)}
	fixture["moved"] = seed.moved(t)
	fixture["refused"], fixture["deleted"] = seed.refusals(t)
	names.blocks(t, seed.rig.database)
	fixture["owed"] = graphEditOwed(t, seed.rig.database, names)
	got := names.encode(fixture)
	if t.Failed() {
		return
	}
	if *updateFixtures {
		if err := os.MkdirAll(filepath.Dir(graphEditFixturePath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(graphEditFixturePath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(graphEditFixturePath)
	if err != nil {
		t.Fatalf("read %s (run with -args -update to write it): %v", graphEditFixturePath, err)
	}
	if !bytes.Equal(got, want) {
		for _, stale := range graphEditStale(got, want) {
			t.Errorf("%s is stale (run with -args -update): %s", graphEditFixturePath, stale)
		}
	}
}

// seedGraphEdit makes the seed (TestGraphEditWebFixture) and names its nodes.
func seedGraphEdit(t *testing.T, names *graphEditNames) *graphEditSeed {
	t.Helper()
	rig := newMoveRig(t, graphEditProject)
	handler, database := rig.handler, rig.database
	s := &graphEditSeed{rig: rig, names: names}

	issue := created201[model.Issue](t, "a person files the converted issue", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": graphEditProject, "title": "Graph editing's seed", "labels": []string{"legion"}, "force": true,
		"spec": ":::callout{#memo kind=\"note\" title=\"Memo\"}\n" + graphEditMemoWords + "\n:::\n\n:::ask{#q1}\n" + graphEditQuestionWords + "\n:::\n",
	}, "alice"))
	s.key = issue.Key
	if tree, err := issueTreeOf(context.Background(), database.Pool, s.key); err != nil || tree == "" {
		t.Fatalf("%s holds the tree %q (%v), want it born converted", s.key, tree, err)
	}
	ask := askOfBlock(t, database, s.key, "q1")
	succeeded(t, "alice answers her question", dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask+"/answer",
		map[string]any{"text": graphEditAnswerWords}, "alice"))
	served := "dispatch://" + s.key + "/spec#b-k1"
	editSpec(t, handler, s.key, []map[string]any{{"op": "insert", "after": "end", "markdown": ":::criterion{#k1 parent=\"dispatch://" + s.key + "/ask/" + ask + "\"}\n" +
		graphEditCriterionWords + "\n:::\n\n" +
		":::requirement{#s1 serves=\"" + served + "\"}\n" + graphEditStatement1Words + "\n:::\n\n" +
		":::requirement{#s3 serves=\"" + served + "\"}\n" + graphEditStatement3Words + "\n:::\n"}})
	if published := putRoleHoldersAs(t, handler, s.key, map[string]any{
		"holders": []issueRoleHolder{{Role: "architect", Session: graphEditArchitect.session}, {Role: "implementer", Session: graphEditWorker.session}},
		"actor":   daemonSessionOf(graphEditProject),
	}); published.Code != http.StatusOK {
		t.Fatalf("the daemon publishes its role holders on %s: status=%d body=%s", s.key, published.Code, published.Body.String())
	}
	s.agentEdit(t, graphEditArchitect, issue.PrimaryArtifactID, "block:s1", ":::requirement{#s2 serves=\""+served+"\"}\n"+graphEditStatement2Words+"\n:::\n")
	s.agentEdit(t, graphEditArchitect, issue.PrimaryArtifactID, "end", "::::decision{#d1}\n"+graphEditDecisionWords+"\n\n:::reading{#g1}\n"+graphEditReadingWords+"\n:::\n::::\n")

	statement := func(block string) string { return "dispatch://" + s.key + "/spec#b-" + block }
	uploadDocument(t, handler, "/api/v1/issues/"+s.key, "verification.md",
		graphEditCheck("c1", "Every edit of the graph is one write.", statement("s1"), statement("s2"), statement("s3")))
	verification := readDecisionDocument(t, database, s.key, model.ArtifactSlugVerification)
	s.agentEdit(t, graphEditArchitect, verification.id, "end", graphEditCheck("c2", "A move names the placement it read, or stores nothing.", statement("s2")))
	result := created201[recordedResult](t, "the shared check's result", recordResult(t, handler, s.key, resultBody("c1", "pass", headCommit, nil))).Result
	uploadDocument(t, handler, "/api/v1/issues/"+s.key, "plan.md", ":::plan_task{#t1 serves=\""+statement("s1")+"\"}\nBuild the node menu of Graph.\n:::\n")
	comment := created201[model.Comment](t, "alice's comment on the third statement's text", dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/issues/"+s.key+"/comments", map[string]any{"body": "Does a refused move store nothing either?",
			"anchor": map[string]any{"artifact": "spec", "quote": graphEditStatement3Quote}}, "alice"))
	message := created201[model.Comment](t, "alice's message", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+s.key+"/messages",
		map[string]any{"body": "The menu ships with the moves first."}, "alice"))
	reply := created201[model.Comment](t, "the architect's reply", sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+s.key+"/messages",
		map[string]any{"body": "The moves are planned first.", "in_reply_to": message.ID, "actor": map[string]any{"kind": "session", "id": graphEditArchitect.session}}))

	s.tree = readDecisionDocument(t, database, s.key, issueTreeName)
	edit := func(markdown string) {
		t.Helper()
		succeeded(t, "alice writes into the issue's own fragment", dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+s.tree.id+"/edits",
			map[string]any{"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": markdown}}}, "alice"))
	}
	edit(graphEditProseWords + "\n\n## A heading of the issue's own\n\n- " + graphEditListWords + "\n- The second item of the list.\n\n" +
		"| Act | Key |\n| --- | --- |\n| Indent | Tab |\n\n```go\nfmt.Println(\"graph\")\n```\n")
	edit(graphEditPlacedWords + "\n")
	s.prose = blockWithText(t, database, graphEditPlacedWords)
	s.criterion, s.statement3 = s.tree.address("k1"), s.tree.address("s3")
	// The tree fragment's band sorts after the spec's. An explicit after must name a
	// sibling in the same band; naming the criterion alone places the prose after the statements.
	rig.move(t, s.key, s.prose, s.criterion, nil)
	lens := created201[lensInvoked](t, "a pre-mortem on the third statement", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+s.key+"/lenses",
		map[string]any{"on": statement("s3"), "kind": graphstore.LensKindPreMortem, "findings": []string{"A move read from a stale page lands nowhere."}}, "alice"))
	if len(lens.Findings) != 1 {
		t.Fatalf("the pre-mortem holds %d findings, want one", len(lens.Findings))
	}
	risk := created201[struct {
		ID string `json:"id"`
	}](t, "a risk under the pre-mortem", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+s.key+"/risks", map[string]any{
		"lens": lens.Address, "heading": "A move lands on a parent read before.", "early_warning": "The If-Match fails twice.",
		"prevention": "The move names its placement_seq.",
	}, "alice"))
	constraint := created201[constraintRead](t, "alice's constraint", dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/issues/"+s.key+"/constraints", map[string]any{"standard": "RFC 9110", "reference": "section 13.1.1"}, "alice"))

	child := createDigestChild(t, handler, database, graphEditProject, "The child, a view of the spec", graphEditChildWords+"\n", s.key, false)
	s.child = child.Key
	uploadDocument(t, handler, "/api/v1/issues/"+s.child, "verification.md", graphEditCheck("cc1", "The child checks the first statement.", statement("s1")))
	done := createDigestIssue(t, handler, database, graphEditProject, "The done copy", ":::criterion{#dk1}\nThe closed issue keeps its graph.\n:::\n", false)
	s.done = done.Key
	editSpec(t, handler, s.done, []map[string]any{{"op": "insert", "after": "end",
		"markdown": ":::requirement{#ds1 serves=\"dispatch://" + s.done + "/spec#b-dk1\"}\nThe closed issue's statement.\n:::\n"}})
	succeeded(t, "close the done copy", dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+s.done, map[string]any{"status": "done"}, "alice"))
	unconverted := createDigestIssue(t, handler, database, graphEditProject, "The unconverted issue",
		":::criterion{#uk1}\nThe unconverted issue's criterion.\n:::\n\n:::requirement{#us1}\nThe unconverted issue's statement.\n:::\n", true)
	s.unconverted = unconverted.Key
	rig.quiet(t)

	for raw, name := range map[string]string{s.key: "<issue>", s.child: "<child>", s.done: "<done>", s.unconverted: "<unconverted>"} {
		names.name(raw, name)
	}
	names.issue(t, database, s.key, "")
	names.issue(t, database, s.child, "child-")
	names.issue(t, database, s.done, "done-")
	names.issue(t, database, s.unconverted, "unconverted-")
	s.questionBlock, s.statement1, s.statement2 = s.tree.address("q1"), s.tree.address("s1"), s.tree.address("s2")
	s.checkShared, s.checkAgent = s.tree.address("c1"), s.tree.address("c2")
	s.comment = graphstore.KindComment + "/" + comment.ID
	s.constraint = graphstore.KindConstraint + "/" + constraint.ID
	s.childParagraph = blockWithText(t, database, graphEditChildWords)
	doneSpec := readDecisionDocument(t, database, s.done, "spec")
	s.doneStatement = doneSpec.address("ds1")
	unconvertedSpec := readDecisionDocument(t, database, s.unconverted, "spec")
	for raw, name := range map[string]string{
		s.tree.address("memo"): "<memo>", ask: "<question>", s.questionBlock: "<question-block>", s.criterion: "<criterion>",
		s.statement1: "<statement-1>", s.statement2: "<statement-2>", s.statement3: "<statement-3>",
		s.checkShared: "<check-shared>", s.checkAgent: "<check-agent>", s.tree.address("t1"): "<plan-task>",
		s.tree.address("d1"): "<decision>", s.tree.address("g1"): "<reading>", result.ID: "<result>",
		comment.ID: "<comment-on-statement-3>", message.ID: "<message>", reply.ID: "<agent-reply>",
		lens.ID: "<lens>", lens.Findings[0].ID: "<finding>", lens.Findings[0].BlockID: "<finding>", risk.ID: "<risk>",
		constraint.ID: "<constraint>", s.prose: "<prose-under-criterion>",
		readDecisionDocument(t, database, s.child, model.ArtifactSlugVerification).address("cc1"): "<child-check>",
		s.childParagraph:        "<child-paragraph>",
		doneSpec.address("dk1"): "<done-criterion>", s.doneStatement: "<done-statement>",
		unconvertedSpec.address("uk1"): "<unconverted-criterion>", unconvertedSpec.address("us1"): "<unconverted-statement>",
	} {
		names.name(raw, name)
	}
	if comment.Anchor != nil && comment.Anchor.MarkID != "" && comment.Anchor.MarkID != comment.ID {
		names.name(comment.Anchor.MarkID, "<comment-on-statement-3-mark>")
	}
	_, anchorBlock := textBlockOf(t, database, issue.PrimaryArtifactID, "paragraph", graphEditStatement3Words)
	names.name(anchorBlock, "<statement-3/paragraph>")
	listParagraph, listBlock := textBlockOf(t, database, s.tree.id, "paragraph", graphEditListWords)
	s.listParagraph = listParagraph
	names.name(listParagraph, "<list-paragraph>")
	names.name(listBlock, "<list-paragraph>")
	return s
}

// graphEditCheck is a check block id of words serving each of serves.
func graphEditCheck(id, words string, serves ...string) string {
	return ":::check{#" + id + " serves=\"" + strings.Join(serves, " ") + "\" surface=\"api\" run=\"go test ./graph\" " +
		"negative_control=\"Drop the rule: the test fails.\"}\n" + words + "\n:::\n"
}

// agentEdit inserts markdown after the block after ("end" for the document's end) of the document
// artifactID as the agent as, and fails unless the edit is taken.
func (s *graphEditSeed) agentEdit(t *testing.T, as graphEditCaller, artifactID, after, markdown string) {
	t.Helper()
	succeeded(t, as.name+"'s edit of "+artifactID, sessionRequest(t, s.rig.handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
		"ops":   []map[string]any{{"op": "insert", "after": after, "markdown": markdown}},
		"actor": map[string]any{"kind": "session", "id": as.session},
	}))
}

// tag is the If-Match a write of the node at address names, its placement_seq as a quoted entity
// tag, which the fixture names after name.
func (s *graphEditSeed) tag(t *testing.T, name, address string) string {
	t.Helper()
	held, tag := placementOf(t, s.rig.database, refsKindOf(address), storeIDOf(address))
	s.names.seq(held.Seq, name)
	return tag
}

// move is PATCH /api/v1/issues/{key}/nodes/{address} of the converted issue as as, with the
// If-Match tag where it is not "" and body; name is the node's name in the fixture.
func (s *graphEditSeed) move(t *testing.T, as graphEditCaller, name, address, tag string, body map[string]any) *graphEditExchange {
	t.Helper()
	exchange := graphEditSend(t, s.rig.handler, as, http.MethodPatch, "/api/v1/issues/"+s.key+"/nodes/"+address,
		"/api/v1/issues/"+s.key+"/nodes/"+url.PathEscape(address), tag, body)
	exchange.node = name
	return exchange
}

// remove is DELETE /api/v1/issues/{key}/nodes/{address} of the converted issue as as: the dry run
// where dry is set, else the delete with expected_count where expected is at least 0, with the
// If-Match tag where it is not "".
func (s *graphEditSeed) remove(t *testing.T, as graphEditCaller, address string, dry bool, tag string, expected int) *graphEditExchange {
	t.Helper()
	sent := deleteNodePath(s.key, address, dry, expected)
	shown := "/api/v1/issues/" + s.key + "/nodes/" + address
	if _, query, queried := strings.Cut(sent, "?"); queried {
		shown += "?" + query
	}
	return graphEditSend(t, s.rig.handler, as, http.MethodDelete, shown, sent, tag, nil)
}

// trees is the member tree: the tree read of each issue of the seed, as the person and as an
// agent.
func (s *graphEditSeed) trees(t *testing.T) map[string]any {
	t.Helper()
	trees := map[string]any{}
	for name, key := range map[string]string{"converted": s.key, "child": s.child, "done": s.done, "unconverted": s.unconverted} {
		trees[name] = &graphEditReads{
			person: graphEditRead(t, s.rig.handler, graphEditPerson, key, ""),
			agent:  graphEditRead(t, s.rig.handler, graphEditAgent, key, ""),
		}
	}
	return trees
}

// paged is the member paged: the converted issue's tree read, read whole as whole, on a second
// server over the seed's store whose Deps.IssueTreePageSize is half the nodes of whole, page after
// page, as tree_read_two_cuts_test.go reads one.
func (s *graphEditSeed) paged(t *testing.T, whole *graphEditExchange) map[string]any {
	t.Helper()
	size := (len(decodeBody[model.IssueTreeRead](t, whole.response).Nodes) + 1) / 2
	small, _, _, _ := newRegisteredTestServer(t, testServerOptions{store: s.rig.database, settle: time.Hour, issueTreePageSize: size})
	var pages []any
	cursor := ""
	for page := 1; ; page++ {
		if page > 4 {
			t.Fatalf("the tree read of %s at a page size of %d still answers a cursor after %d pages", s.key, size, page-1)
		}
		query := ""
		if cursor != "" {
			query = "?cursor=" + url.QueryEscape(cursor)
		}
		read := graphEditRead(t, small, graphEditPerson, s.key, query)
		pages = append(pages, read)
		next := decodeBody[model.IssueTreeRead](t, read.response).Cursor
		if next == nil {
			break
		}
		cursor = *next
	}
	if len(pages) < 2 {
		t.Fatalf("the tree read of %s at a page size of %d answers one page, want a cursor and a second page", s.key, size)
	}
	return map[string]any{"page_size": size, "pages": pages}
}

// moved is the member moved: the third statement indented under the second, outdented back under
// the criterion after the second with the placement_seq the indent answered, moved up before the
// second and down after it again, each as R4 of INFRA-5705's spec writes its body.
func (s *graphEditSeed) moved(t *testing.T) map[string]any {
	t.Helper()
	s.staleTag = s.tag(t, "<statement-3>", s.statement3)
	tag := s.staleTag
	moved := map[string]any{}
	for _, step := range []struct {
		name string
		body map[string]any
	}{
		{"indent", map[string]any{"parent": s.statement2, "after": nil}},
		{"outdent", map[string]any{"parent": s.criterion, "after": s.statement2}},
		{"move_up", map[string]any{"parent": s.criterion, "after": s.statement1}},
		{"move_down", map[string]any{"parent": s.criterion, "after": s.statement2}},
	} {
		exchange := s.move(t, graphEditPerson, "<statement-3>", s.statement3, tag, step.body).taken(t, "the "+step.name+" of "+s.statement3)
		answer := decodeBody[nodeMoved](t, exchange.response)
		s.names.seq(answer.PlacementSeq, "<statement-3>")
		tag = `"` + strconv.FormatInt(answer.PlacementSeq, 10) + `"`
		moved[step.name] = exchange
	}
	if held, _ := placementOf(t, s.rig.database, refsKindOf(s.statement3), storeIDOf(s.statement3)); held.ToID != s.criterion {
		t.Fatalf("the moves left %s under %s %s, want it under %s", s.statement3, held.ToKind, held.ToID, s.criterion)
	}
	return moved
}

// refusals is the members refused and deleted: each PATCH and DELETE refusal the seed gives, the
// DELETE's dry run of the third statement and the delete taken once its count moved.
func (s *graphEditSeed) refusals(t *testing.T) (map[string]any, map[string]any) {
	t.Helper()
	person, architect, worker := graphEditPerson, graphEditArchitect, graphEditWorker
	issueNode := graphstore.KindIssue + "/" + s.key
	indent := map[string]any{"parent": s.statement2, "after": nil}
	refused := map[string]any{}
	moveRefused := func(code, what string, exchange *graphEditExchange, status int) {
		t.Helper()
		refused[code] = exchange.refused(t, what, status, strings.Fields(code)[0])
	}
	moveRefused(codeNodeNotMovable, "the move of the comment "+s.comment, s.move(t, person, "<comment-on-statement-3>", s.comment,
		s.tag(t, "<comment-on-statement-3>", s.comment), map[string]any{"parent": s.criterion}), http.StatusBadRequest)
	tag := s.tag(t, "<statement-3>", s.statement3)
	moveRefused(codeMoveInput, "the move of "+s.statement3+" with an empty body", s.move(t, person, "<statement-3>", s.statement3, tag, map[string]any{}), http.StatusBadRequest)
	moveRefused(codePreconditionRequired, "the indent of "+s.statement3+" with no If-Match", s.move(t, person, "<statement-3>", s.statement3, "", indent), http.StatusPreconditionRequired)
	moveRefused(codePreconditionFailed, "the indent of "+s.statement3+" with the placement_seq it held before its moves",
		s.move(t, person, "<statement-3>", s.statement3, s.staleTag, indent), http.StatusPreconditionFailed)
	moveRefused(specWriterRefusalCode, "the worker's indent of "+s.statement3, s.move(t, worker, "<statement-3>", s.statement3, tag, indent), http.StatusForbidden)
	moveRefused(codeAnsweredAskKept, "the architect's move of the answered question's block "+s.questionBlock, s.move(t, architect, "<question-block>", s.questionBlock,
		s.tag(t, "<question-block>", s.questionBlock), map[string]any{"parent": issueNode}), http.StatusConflict)
	moveRefused(codeInvalidPointer, "the move of "+s.statement1+" under the check that stands under it", s.move(t, person, "<statement-1>", s.statement1,
		s.tag(t, "<statement-1>", s.statement1), map[string]any{"parent": s.checkShared}), http.StatusBadRequest)
	moveRefused(codeInvalidPointer+" after", "the move of "+s.statement3+" after the second statement's check", s.move(t, person, "<statement-3>", s.statement3, tag,
		map[string]any{"parent": s.criterion, "after": s.checkAgent}), http.StatusBadRequest)
	moveRefused(codeInvalidPointer+" constraint", "the move of the constraint under "+s.statement3, s.move(t, person, "<constraint>", s.constraint,
		s.tag(t, "<constraint>", s.constraint), map[string]any{"parent": s.statement3}), http.StatusBadRequest)
	moveRefused(codeContainerWouldEmpty, "the move of the list's first paragraph under the issue", s.move(t, person, "<list-paragraph>", s.listParagraph,
		s.tag(t, "<list-paragraph>", s.listParagraph), map[string]any{"parent": issueNode}), http.StatusConflict)
	// The container error names the list item even though the tree does not expose its
	// block id. The paragraph's id is already named, so only the container is new here.
	containerError := decodeBody[refusalAnswer](t, refused[codeContainerWouldEmpty].(*graphEditExchange).response).Error
	for _, id := range graphEditUUID.FindAllString(containerError, -1) {
		if _, named := s.names.within[id]; !named {
			s.names.name(id, "<list-item>")
		}
	}
	moveRefused("NOT_FOUND", "the move of the done copy's statement under "+s.key, s.move(t, person, "<done-statement>", s.doneStatement,
		s.tag(t, "<done-statement>", s.doneStatement), map[string]any{"parent": s.criterion}), http.StatusNotFound)

	refusedDeletes := map[string]any{}
	deleteRefused := func(code, what string, exchange *graphEditExchange, status int) *graphEditExchange {
		t.Helper()
		refusedDeletes[code] = exchange.refused(t, what, status, strings.Fields(code)[0])
		return exchange
	}
	dryRun := s.remove(t, person, s.statement3, true, "", -1).taken(t, "the dry run of "+s.statement3)
	counted := dryRun.count(t)
	deleteRefused(codeStateNotPermitted, "the architect's dry run of "+s.statement3+", which holds a lens", s.remove(t, architect, s.statement3, true, "", -1), http.StatusForbidden)
	deleteRefused(specWriterRefusalCode, "the worker's dry run of "+s.statement2, s.remove(t, worker, s.statement2, true, "", -1), http.StatusForbidden)
	deleteRefused(codeAnsweredAskKept, "the architect's dry run of the answered question's block", s.remove(t, architect, s.questionBlock, true, "", -1), http.StatusConflict)
	deleteRefused(codeNodeNotDeletable, "the dry run of the comment", s.remove(t, person, s.comment, true, "", -1), http.StatusBadRequest)
	deleteRefused(codeNodeNotDeletable+" question", "the dry run of the question's block", s.remove(t, person, s.questionBlock, true, "", -1), http.StatusBadRequest)
	deleteRefused(codeNodeHoldsResult+" check", "the dry run of the shared check", s.remove(t, person, s.checkShared, true, "", -1), http.StatusConflict)
	deleteRefused(codeNodeHoldsResult+" statement", "the dry run of the first statement", s.remove(t, person, s.statement1, true, "", -1), http.StatusConflict)
	deleteRefused(codeContainerWouldEmpty, "the dry run of the list's first paragraph", s.remove(t, person, s.listParagraph, true, "", -1), http.StatusConflict)
	tag = s.tag(t, "<statement-3>", s.statement3)
	deleteRefused(codeDeleteInput, "the delete of "+s.statement3+" with no expected_count", s.remove(t, person, s.statement3, false, tag, -1), http.StatusBadRequest)
	deleteRefused(codePreconditionRequired, "the delete of "+s.statement3+" with no If-Match", s.remove(t, person, s.statement3, false, "", counted), http.StatusPreconditionRequired)
	deleteRefused(codePreconditionFailed, "the delete of "+s.statement3+" with the placement_seq it held before its moves",
		s.remove(t, person, s.statement3, false, s.staleTag, counted), http.StatusPreconditionFailed)

	// The architect writes a prose block under the third statement while the person's confirm is
	// open, so the count she confirms moved.
	s.agentEdit(t, architect, s.tree.id, "end", graphEditAgentProseWords+"\n")
	agentProse := blockWithText(t, s.rig.database, graphEditAgentProseWords)
	s.names.name(agentProse, "<agent-prose>")
	s.move(t, architect, "<agent-prose>", agentProse, s.tag(t, "<agent-prose>", agentProse), map[string]any{"parent": s.statement3}).
		taken(t, "the architect's move of its prose block under "+s.statement3)
	moved := deleteRefused(codeDeleteCountMoved, "the delete of "+s.statement3+" with the count read before the architect's write",
		s.remove(t, person, s.statement3, false, tag, counted), http.StatusConflict)
	if now := moved.count(t); now != counted+1 {
		t.Fatalf("the delete of %s answered the count %d, want %d: the dry run's and the architect's prose block", s.statement3, now, counted+1)
	}
	taken := s.remove(t, person, s.statement3, false, tag, counted+1).taken(t, "the delete of "+s.statement3)
	if count := taken.count(t); count != counted+1 {
		t.Errorf("the delete of %s answered the count %d, want %d", s.statement3, count, counted+1)
	}
	s.rig.quiet(t)
	deleteRefused(codeNodeGone, "the dry run of the deleted "+s.statement3, s.remove(t, person, s.statement3, true, "", -1), http.StatusConflict)
	deleteRefused("NOT_FOUND", "the dry run of the done copy's statement under "+s.key, s.remove(t, person, s.doneStatement, true, "", -1), http.StatusNotFound)
	moveRefused(codeNodeGone, "the move of the deleted "+s.statement3, s.move(t, person, "<statement-3>", s.statement3, tag, map[string]any{"parent": s.criterion}), http.StatusConflict)
	moveRefused(codeParentNotReached, "the move of the prose block under the deleted "+s.statement3, s.move(t, person, "<prose-under-criterion>", s.prose,
		s.tag(t, "<prose-under-criterion>", s.prose), map[string]any{"parent": s.statement3}), http.StatusConflict)

	// A document made by a title alone, which a removal could take: no node but a lens, a finding
	// or a risk stands under its node.
	notes := emptyDocument(t, s.rig.handler, s.key, "Notes U.md")
	moveRefused(codeParentDocumentRemovable, "the move of the prose block onto a document a removal could take", s.move(t, person, "<prose-under-criterion>", s.prose,
		s.tag(t, "<prose-under-criterion>", s.prose), map[string]any{"parent": graphstore.KindArtifact + "/" + notes.RefKey}), http.StatusConflict)
	s.contended(t, refused, refusedDeletes)
	s.names.issue(t, s.rig.database, s.key, "")
	return refused, map[string]any{"dry_run": dryRun, "taken": taken, "refused": refusedDeletes}
}

// contended adds the PATCH's and the DELETE's 409 PARENT_CONTENDED to refused and refusedDeletes,
// as TestAMoveWhoseSecondReadMeetsAnUnlockedIssueAtEachAttemptIsContended and
// requireTheDeleteRetriesOnceAndIsContended make each: the test hook (server.betweenNodeRouteReads)
// pauses the write after each read it makes with no lock held, and there the person moves the
// paragraph of a child's room, the child's and then a second child's, under the node the write
// takes, so each read under the locks meets a node of an issue the attempt did not lock. The move
// is of the second statement to stand first, the delete of the prose block under the criterion.
func (s *graphEditSeed) contended(t *testing.T, refused, refusedDeletes map[string]any) {
	t.Helper()
	second := createDigestChild(t, s.rig.handler, s.rig.database, graphEditProject, "The second child", graphEditSecondChildWords+"\n", s.key, false)
	s.names.name(second.Key, "<child-2>")
	s.names.issue(t, s.rig.database, second.Key, "child-2-")
	secondParagraph := blockWithText(t, s.rig.database, graphEditSecondChildWords)
	s.names.name(secondParagraph, "<child-2-paragraph>")
	moves := []struct{ key, name, block string }{{s.child, "<child-paragraph>", s.childParagraph}, {second.Key, "<child-2-paragraph>", secondParagraph}}
	hold := func(parent string) {
		moving := false
		s.rig.served.betweenNodeRouteReads = func(_ context.Context, attempt int) {
			if moving || attempt > len(moves) {
				return
			}
			moving = true
			defer func() { moving = false }()
			moved := moves[attempt-1]
			if response := patchNode(t, s.rig.handler, moved.key, moved.block, s.tag(t, moved.name, moved.block), map[string]any{"parent": parent}, "alice"); response.Code != http.StatusOK {
				t.Errorf("the move of %s under %s answered %d %s, want 200", moved.block, parent, response.Code, response.Body.String())
			}
		}
	}
	s.rig.quiet(t)
	hold(s.statement2)
	contended := s.move(t, graphEditPerson, "<statement-2>", s.statement2, s.tag(t, "<statement-2>", s.statement2), map[string]any{"parent": s.criterion, "after": nil})
	s.rig.served.betweenNodeRouteReads = nil
	refused[parentContended] = contended.refused(t, "the contended move of "+s.statement2, http.StatusConflict, parentContended)

	s.rig.quiet(t)
	counted := s.remove(t, graphEditPerson, s.prose, true, "", -1).taken(t, "the dry run of "+s.prose).count(t)
	tag := s.tag(t, "<prose-under-criterion>", s.prose)
	hold(s.prose)
	contended = s.remove(t, graphEditPerson, s.prose, false, tag, counted)
	s.rig.served.betweenNodeRouteReads = nil
	refusedDeletes[parentContended] = contended.refused(t, "the contended delete of "+s.prose, http.StatusConflict, parentContended)
	s.rig.quiet(t)
}

// graphEditOwed is the member owed: on a server of its own, whose document service settles after
// an hour and admits browser peers (writeAcceptedVersions' room left owed), a converted issue whose
// tree artifact owes its block index - a browser's unversioned edit of its spec holds the room, so
// a write of the tree artifact defers its index (acceptedFixture.holdOwedBy) - read right after
// the write of a paragraph and a requirement into the tree artifact, and again once the
// reconciliation has indexed the room.
func graphEditOwed(t *testing.T, store *store.Store, names *graphEditNames) map[string]any {
	t.Helper()
	handler, database, deps := newTestServer(t, testServerOptions{store: store, settle: time.Hour, browserPeers: true})
	createReferenceAPIProject(t, handler, graphEditOwedProject)
	issue := createDigestIssue(t, handler, database, graphEditOwedProject, "The tree artifact owes its index",
		":::criterion{#ok1}\nThe owed issue's criterion.\n:::\n", false)
	tree := readDecisionDocument(t, database, issue.Key, issueTreeName)
	fixture := &acceptedFixture{handler: handler, database: database, documents: deps.Docs.(*docs.Service), issue: issue, converted: true}
	fixture.holdOwedBy(t, issue.PrimaryArtifactID, "")
	written := acceptedRequirement("or2")
	fixture.accept(t, "the write of a paragraph and a requirement into the tree artifact", tree.id, []acceptedBlock{written}, func() string {
		succeeded(t, "alice writes into the tree artifact", dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+tree.id+"/edits",
			map[string]any{"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": graphEditOwedWords + "\n\n" + typedBlockMarkdown(issue.Key, written)}}}, "alice"))
		return tree.id
	})
	owed := graphEditRead(t, handler, graphEditPerson, issue.Key, "")
	fixture.settleOwed(t)
	reconcileUntilNothingOwed(t, handler, deps, issue.Key)
	indexed := graphEditRead(t, handler, graphEditPerson, issue.Key, "")
	names.name(issue.Key, "<owed-issue>")
	names.issue(t, database, issue.Key, "owed-")
	names.name(tree.address("ok1"), "<owed-criterion>")
	names.name(tree.address(written.id), "<owed-requirement>")
	names.name(blockWithText(t, database, graphEditOwedWords), "<owed-paragraph>")
	names.blocks(t, database)
	return map[string]any{"owed": owed, "indexed": indexed}
}

// --- the names ---------------------------------------------------------------------------------

// graphEditNames is what TestGraphEditWebFixture replaces before it compares or writes: each value
// a run makes afresh, by the seed's name for it (the test's head lists the scheme). within names a
// raw value wherever a string holds it, the longest first; seqs names a placement_seq, by its
// digits; seqsOf counts the placement_seqs named after each node; issues holds, for each issue the
// test named, the prefix of its blocks' names.
type graphEditNames struct {
	t        *testing.T
	within   map[string]string
	seqs     map[string]string
	seqsOf   map[string]int
	issues   map[string]string
	replacer *strings.Replacer
}

func newGraphEditNames(t *testing.T) *graphEditNames {
	names := &graphEditNames{t: t, within: map[string]string{}, seqs: map[string]string{}, seqsOf: map[string]int{}, issues: map[string]string{}}
	for _, caller := range []graphEditCaller{graphEditArchitect, graphEditWorker} {
		names.name(caller.session, caller.name)
	}
	names.name("legion-daemon:"+graphEditProject, "<daemon>")
	return names
}

// The patterns of what a string holds that a run makes afresh, or that the names write over.
var (
	graphEditLogin   = regexp.MustCompile(`\balice\b`)
	graphEditTime    = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})`)
	graphEditCursor  = regexp.MustCompile(`cursor=([A-Za-z0-9_-]+)`)
	graphEditSeqWord = regexp.MustCompile(`placement_seq (\d+)`)
	graphEditTag     = regexp.MustCompile(`^(W/)?"(\d+)"$`)
	graphEditUUID    = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	graphEditLeft    = regexp.MustCompile(`\b(?:` + graphEditProject + `|` + graphEditOwedProject + `)-\d|\bses-|legion-daemon:|placement_seq \d`)
)

// name names raw name wherever a string holds it.
func (n *graphEditNames) name(raw, name string) {
	n.t.Helper()
	if raw == "" {
		n.t.Fatalf("an empty value is named %s", name)
	}
	if held, named := n.within[raw]; named && held != name {
		n.t.Fatalf("%q is named both %s and %s", raw, held, name)
	}
	n.within[raw] = name
	n.replacer = nil
	// A minted block id also appears bare in block_id and in the dispatch:// reference to it.
	if _, blockID, found := strings.Cut(raw, "#"); found && graphEditUUID.MatchString(blockID) {
		n.name(blockID, name)
	}
}

// seq names the placement_seq seq after the node named node, and answers its name.
func (n *graphEditNames) seq(seq int64, node string) string {
	key := strconv.FormatInt(seq, 10)
	if name, named := n.seqs[key]; named {
		return name
	}
	node = strings.TrimSuffix(strings.TrimPrefix(node, "<"), ">")
	n.seqsOf[node]++
	name := "<seq:" + node + ">"
	if held := n.seqsOf[node]; held > 1 {
		name = fmt.Sprintf("<seq:%s@%d>", node, held)
	}
	n.seqs[key] = name
	return name
}

// issue names each document of the issue key, by prefix and its slug, and keeps prefix as the
// prefix of the names of the issue's blocks (blocks).
func (n *graphEditNames) issue(t *testing.T, database *store.Store, key, prefix string) {
	t.Helper()
	n.issues[key] = prefix
	rows, err := database.Pool.Query(context.Background(), `select id::text, slug from artifacts where issue_key = $1`, key)
	if err != nil {
		t.Fatalf("read the documents of %s: %v", key, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, slug string
		if err := rows.Scan(&id, &slug); err != nil {
			t.Fatalf("read a document of %s: %v", key, err)
		}
		n.name(id, "<"+prefix+strings.TrimSuffix(slug, "-md")+">")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the documents of %s: %v", key, err)
	}
}

// graphEditBlock is one document_blocks row blocks reads: the block's id, its type, the block
// that holds it, its words and its node's address.
type graphEditBlock struct {
	id, typ, parent, excerpt, address, artifact string
}

// blocks names each block of the documents of each issue the names hold whose id a run minted and
// that no name names yet: by the path of names from the block that holds it, else from its issue's
// prefix and prose, and its type, each of the unnamed siblings of one type numbered in the order of
// their words. It reads the rows standing in database once its writes are done, so it is called
// once for each store.
func (n *graphEditNames) blocks(t *testing.T, database *store.Store) {
	t.Helper()
	for _, key := range slices.Sorted(maps.Keys(n.issues)) {
		rows, err := database.Pool.Query(context.Background(), `
			select d.block_id, d.type, coalesce(d.parent_block_id, ''), d.excerpt, coalesce(d.node_address, ''), d.artifact_id::text
			from document_blocks d join artifacts a on a.id = d.artifact_id where a.issue_key = $1`, key)
		if err != nil {
			t.Fatalf("read the blocks of %s: %v", key, err)
		}
		held := map[string]graphEditBlock{}
		for rows.Next() {
			var block graphEditBlock
			if err := rows.Scan(&block.id, &block.typ, &block.parent, &block.excerpt, &block.address, &block.artifact); err != nil {
				rows.Close()
				t.Fatalf("read a block of %s: %v", key, err)
			}
			held[block.id] = block
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("read the blocks of %s: %v", key, err)
		}
		named := func(block graphEditBlock) (string, bool) {
			if name, ok := n.within[block.address]; ok && block.address != "" {
				return name, true
			}
			name, ok := n.within[block.id]
			return name, ok
		}
		unnamed := map[string]bool{}
		for id, block := range held {
			if _, ok := named(block); !ok && graphEditUUID.MatchString(id) {
				unnamed[id] = true
			}
		}
		var nameOf func(id string) string
		nameOf = func(id string) string {
			block := held[id]
			if name, ok := named(block); ok {
				return strings.TrimSuffix(strings.TrimPrefix(name, "<"), ">")
			}
			if !unnamed[id] {
				return n.issues[key] + id
			}
			base := strings.TrimSuffix(strings.TrimPrefix(n.within[block.artifact], "<"), ">")
			if base == "" {
				base = n.issues[key] + "prose"
			}
			if _, ok := held[block.parent]; ok {
				base = nameOf(block.parent)
			}
			var siblings []graphEditBlock
			for other := range unnamed {
				if held[other].parent == block.parent && held[other].typ == block.typ {
					siblings = append(siblings, held[other])
				}
			}
			slices.SortFunc(siblings, func(a, b graphEditBlock) int {
				return cmp.Or(strings.Compare(a.excerpt, b.excerpt), strings.Compare(a.id, b.id))
			})
			name := base + "/" + block.typ
			if len(siblings) > 1 {
				name += "-" + strconv.Itoa(slices.IndexFunc(siblings, func(other graphEditBlock) bool { return other.id == id })+1)
			}
			if block.address != "" {
				n.name(block.address, "<"+name+">")
			}
			n.name(id, "<"+name+">")
			return name
		}
		for _, id := range slices.Sorted(maps.Keys(unnamed)) {
			nameOf(id)
		}
	}
}

// text is s with each name written over what it names.
func (n *graphEditNames) text(s string) string {
	s = graphEditCursor.ReplaceAllStringFunc(s, func(found string) string {
		return "cursor=" + n.cursor(strings.TrimPrefix(found, "cursor="))
	})
	if n.replacer == nil {
		raws := slices.Collect(maps.Keys(n.within))
		slices.SortFunc(raws, func(a, b string) int {
			if len(a) != len(b) {
				return len(b) - len(a)
			}
			return strings.Compare(a, b)
		})
		pairs := make([]string, 0, 2*len(raws))
		for _, raw := range raws {
			pairs = append(pairs, raw, n.within[raw])
		}
		n.replacer = strings.NewReplacer(pairs...)
	}
	s = n.replacer.Replace(s)
	s = graphEditLogin.ReplaceAllString(s, graphEditPerson.name)
	s = graphEditTime.ReplaceAllString(s, "<time>")
	return graphEditSeqWord.ReplaceAllStringFunc(s, func(found string) string {
		digits := strings.TrimPrefix(found, "placement_seq ")
		if name, named := n.seqs[digits]; named {
			return "placement_seq " + name
		}
		return found
	})
}

// cursor is a tree read's cursor as the fixture names it: the node it starts at.
func (n *graphEditNames) cursor(cursor string) string {
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		n.t.Fatalf("read the cursor %q: %v", cursor, err)
	}
	return "<cursor:" + n.text(string(decoded)) + ">"
}

// value is v with each string and object key named (text), each cursor named, and each
// placement_seq named after the node whose id stands beside it, else after node; objects are read
// in the order of their keys, so names are given in one order.
func (n *graphEditNames) value(v any, node string) any {
	switch v := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for _, key := range slices.Sorted(maps.Keys(v)) {
			if cursor, ok := v[key].(string); ok && key == "cursor" {
				out[key] = n.cursor(cursor)
				continue
			}
			out[n.text(key)] = n.value(v[key], node)
		}
		if seq, ok := out["placement_seq"].(json.Number); ok {
			owner := node
			if id, ok := out["id"].(string); ok {
				owner = id
			}
			number, err := seq.Int64()
			if err != nil || owner == "" {
				n.t.Fatalf("the placement_seq %s names no node (%v)", seq, err)
			}
			out["placement_seq"] = n.seq(number, owner)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for index, item := range v {
			out[index] = n.value(item, node)
		}
		return out
	case string:
		return n.text(v)
	default:
		return v
	}
}

// exchange is the exchange as the fixture holds it, each value named.
func (n *graphEditNames) exchange(e *graphEditExchange) map[string]any {
	n.t.Helper()
	request := map[string]any{"method": e.method, "path": n.text(e.path), "login": e.as.name, "if_match": nil}
	if e.ifMatch != nil {
		request["if_match"] = n.tag(*e.ifMatch)
	}
	if e.body != nil {
		request["body"] = n.value(graphEditDecode(n.t, mustJSON(n.t, e.body)), e.node)
	}
	return map[string]any{
		"request": request,
		"status":  e.response.Code,
		"body":    graphEditOrders(n.value(graphEditDecode(n.t, e.response.Body.Bytes()), e.node)),
	}
}

// tag is an If-Match as the fixture names it: each placement_seq it names by its name.
func (n *graphEditNames) tag(tag string) string {
	n.t.Helper()
	found := graphEditTag.FindStringSubmatch(tag)
	if found == nil {
		return tag
	}
	name, named := n.seqs[found[2]]
	if !named {
		n.t.Fatalf("the If-Match %s names a placement_seq no node of the fixture is named by", tag)
	}
	return found[1] + `"` + name + `"`
}

// walk is the fixture with each exchange named, in the order of its members' keys.
func (n *graphEditNames) walk(v any) any {
	switch v := v.(type) {
	case *graphEditExchange:
		return n.exchange(v)
	case *graphEditReads:
		person, agent := n.exchange(v.person), n.exchange(v.agent)
		if reflect.DeepEqual(person["body"], agent["body"]) && person["status"] == agent["status"] {
			agent = map[string]any{"request": agent["request"], "same_as_person": true}
		}
		return map[string]any{"person": person, "agent": agent}
	case map[string]any:
		out := make(map[string]any, len(v))
		for _, key := range slices.Sorted(maps.Keys(v)) {
			out[key] = n.walk(v[key])
		}
		return out
	case []any:
		out := make([]any, len(v))
		for index, item := range v {
			out[index] = n.walk(item)
		}
		return out
	default:
		return v
	}
}

// encode is the fixture's bytes: every member named, as indented JSON with no HTML escapes, so a
// name reads as it is written. It fails where a value a run makes afresh is left unnamed.
func (n *graphEditNames) encode(fixture map[string]any) []byte {
	n.t.Helper()
	named := n.walk(fixture)
	graphEditUnnamed(n.t, "", named)
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(named); err != nil {
		n.t.Fatalf("encode the fixture: %v", err)
	}
	return buffer.Bytes()
}

// graphEditUnnamed fails for each string of v that still holds a value a run makes afresh, and for
// each placement_seq left a number, naming its path.
func graphEditUnnamed(t *testing.T, path string, v any) {
	t.Helper()
	switch v := v.(type) {
	case map[string]any:
		for _, key := range slices.Sorted(maps.Keys(v)) {
			if key == "placement_seq" {
				if _, number := v[key].(json.Number); number {
					t.Errorf("%s.placement_seq is left a number: %s", path, v[key])
				}
			}
			graphEditUnnamed(t, path+"."+key, key)
			graphEditUnnamed(t, path+"."+key, v[key])
		}
	case []any:
		for index, item := range v {
			graphEditUnnamed(t, fmt.Sprintf("%s[%d]", path, index), item)
		}
	case string:
		if found := slices.Concat(graphEditUUID.FindAllString(v, -1), graphEditTime.FindAllString(v, -1), graphEditLeft.FindAllString(v, -1),
			graphEditLogin.FindAllString(v, -1)); len(found) > 0 {
			t.Errorf("%s holds %q, which a run makes afresh and the fixture names nothing: %q", path, found, v)
		}
	}
}

// graphEditOrders is the answer v with each rank written as its order among the ranks v holds,
// `<rank:N>` zero-padded, and each version number as its order among the version numbers v holds
// (a member named version or ending in _version).
func graphEditOrders(v any) any {
	ranks, versions := map[string]bool{}, map[int64]bool{}
	var collect func(any)
	collect = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for key, item := range v {
				if rank, ok := item.(string); ok && key == "rank" {
					ranks[rank] = true
				}
				if number, ok := item.(json.Number); ok && graphEditVersionKey(key) {
					if version, err := number.Int64(); err == nil {
						versions[version] = true
					}
				}
				collect(item)
			}
		case []any:
			for _, item := range v {
				collect(item)
			}
		}
	}
	collect(v)
	rankOrder := slices.Sorted(maps.Keys(ranks))
	versionOrder := slices.Sorted(maps.Keys(versions))
	width := len(strconv.Itoa(len(rankOrder)))
	var write func(any) any
	write = func(v any) any {
		switch v := v.(type) {
		case map[string]any:
			for key, item := range v {
				if rank, ok := item.(string); ok && key == "rank" {
					v[key] = fmt.Sprintf("<rank:%0*d>", width, slices.Index(rankOrder, rank)+1)
					continue
				}
				if number, ok := item.(json.Number); ok && graphEditVersionKey(key) {
					if version, err := number.Int64(); err == nil {
						v[key] = json.Number(strconv.Itoa(slices.Index(versionOrder, version) + 1))
						continue
					}
				}
				v[key] = write(item)
			}
			return v
		case []any:
			for index, item := range v {
				v[index] = write(item)
			}
			return v
		default:
			return v
		}
	}
	return write(v)
}

// graphEditVersionKey reports whether the member key holds a version number.
func graphEditVersionKey(key string) bool {
	return key == "version" || strings.HasSuffix(key, "_version")
}

// graphEditDecode is data decoded with its numbers kept as written, null for no data.
func graphEditDecode(t *testing.T, data []byte) any {
	t.Helper()
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var v any
	if err := decoder.Decode(&v); err != nil {
		t.Fatalf("decode %q: %v", data, err)
	}
	return v
}

// mustJSON is v as JSON.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode %v: %v", v, err)
	}
	return data
}

// graphEditStale is, for each member of the committed fixture want whose bytes differ from got,
// the member and the first path inside it where the two differ.
func graphEditStale(got, want []byte) []string {
	decode := func(data []byte) (map[string]any, error) {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		var v map[string]any
		return v, decoder.Decode(&v)
	}
	answered, _ := decode(got)
	committed, err := decode(want)
	if err != nil {
		return []string{"the committed file is no JSON object: " + err.Error()}
	}
	members := map[string]bool{}
	for member := range answered {
		members[member] = true
	}
	for member := range committed {
		members[member] = true
	}
	var stale []string
	for _, member := range slices.Sorted(maps.Keys(members)) {
		if !reflect.DeepEqual(answered[member], committed[member]) {
			stale = append(stale, "the member "+member+" differs at "+graphEditDifference(member, answered[member], committed[member]))
		}
	}
	if len(stale) == 0 {
		stale = append(stale, "its bytes differ from what the test writes, every member equal: its layout is not the test's")
	}
	return stale
}

// graphEditDifference is the first path under path where got, the handlers' answer, and want,
// the committed fixture's, differ, with the two values there.
func graphEditDifference(path string, got, want any) string {
	switch g := got.(type) {
	case map[string]any:
		w, ok := want.(map[string]any)
		if !ok {
			break
		}
		keys := slices.Sorted(maps.Keys(g))
		for key := range w {
			if !slices.Contains(keys, key) {
				keys = append(keys, key)
			}
		}
		slices.Sort(keys)
		for _, key := range keys {
			if !reflect.DeepEqual(g[key], w[key]) {
				return graphEditDifference(path+"."+key, g[key], w[key])
			}
		}
	case []any:
		w, ok := want.([]any)
		if !ok {
			break
		}
		for index := range min(len(g), len(w)) {
			if !reflect.DeepEqual(g[index], w[index]) {
				return graphEditDifference(fmt.Sprintf("%s[%d]", path, index), g[index], w[index])
			}
		}
		return fmt.Sprintf("%s: the handlers answer %d items, the file holds %d", path, len(g), len(w))
	}
	return fmt.Sprintf("%s: the handlers answer %s, the file holds %s", path, graphEditShown(got), graphEditShown(want))
}

// graphEditShown is v as JSON, cut to 200 bytes, or "nothing" for a member one side lacks.
func graphEditShown(v any) string {
	if v == nil {
		return "nothing"
	}
	data, _ := json.Marshal(v)
	if len(data) > 200 {
		return string(data[:200]) + "..."
	}
	return string(data)
}
