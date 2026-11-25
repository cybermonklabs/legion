package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
	"github.com/sjawhar/envoy/internal/testwait"
)

// patchNode sends PATCH /api/v1/issues/{key}/nodes/{id} with body, as the person login names, or,
// with no login, under the shared agent bearer, and with If-Match tag where tag is not "".
func patchNode(t testing.TB, handler http.Handler, key, id, tag string, body map[string]any, login string) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode the move of %s: %v", id, err)
	}
	request := httptest.NewRequest(http.MethodPatch, "/api/v1/issues/"+key+"/nodes/"+url.PathEscape(id), bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	if login != "" {
		request.Header.Set("X-Dispatch-User", login)
	} else {
		request.Header.Set("Authorization", "Bearer agent-token")
	}
	if tag != "" {
		request.Header.Set("If-Match", tag)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// placementOf is the current part_of of the node the store keys as kind and id, and its
// placement_seq as the If-Match tag a move of it names, failing the test where it holds no one
// part_of.
func placementOf(t testing.TB, database *store.Store, kind, id string) (graphstore.Tuple, string) {
	t.Helper()
	held, err := graphstore.CurrentOut(context.Background(), database.Pool, kind, id, []string{graphstore.TuplePartOf})
	if err != nil || len(held) != 1 {
		t.Fatalf("the node %s %s stands on %v (%v), want one parent", kind, id, held, err)
	}
	return held[0], `"` + strconv.FormatInt(held[0].Seq, 10) + `"`
}

// blockAddressOf is the node address of the block blockID of a document of the issue key, read off its
// document_blocks row, failing the test where none holds it.
func blockAddressOf(t testing.TB, database *store.Store, key, blockID string) string {
	t.Helper()
	var address string
	if err := database.Pool.QueryRow(context.Background(), `
		select d.node_address from document_blocks d join artifacts a on a.id = d.artifact_id
		where a.issue_key = $1 and d.block_id = $2 and d.node_address is not null`, key, blockID).Scan(&address); err != nil {
		t.Fatalf("read the node of block %s of %s: %v", blockID, key, err)
	}
	return address
}

// --- TestNodePlacementRoutesMatchTheGeneratedContract ---------------------------------------------

// nodePlacementOperation is one operation at /api/v1/issues/{key}/nodes/{id} the contract test
// reads (TestNodePlacementRoutesMatchTheGeneratedContract): its method, the body members it takes
// and the answer members it returns, by schema name, whether its If-Match header is required, the
// query parameters it declares and those it must not, the codes its description names, and, by
// status, the members a refusal's body declares beside its code and its message.
type nodePlacementOperation struct {
	method, operationID   string
	request, response     string
	takes, answers        []string
	ifMatchRequired       bool
	query, notQuery       []string
	codes                 []string
	refusals              map[string][]string
	generatedHeaderMember string
}

// nodePlacementOperations are the operations the node's own resource serves (R7): the move, whose
// If-Match every request sends, and the delete with its dry run, whose If-Match the delete itself
// sends and the dry run does not, and whose 409 DELETE_COUNT_MOVED carries the count now.
var nodePlacementOperations = []nodePlacementOperation{{
	method: "patch", operationID: "patchApiV1IssuesByKeyNodesById",
	request: "NodeMoveInput", response: "NodeMoved",
	takes: []string{"parent", "after"}, answers: []string{"parent", "after", "placement_seq"},
	ifMatchRequired: true,
	notQuery:        []string{"dry_run", "expected_count"},
	codes: []string{
		"400 INVALID_POINTER", "400 NODE_NOT_MOVABLE", "400 MOVE_INPUT", "403 DOCUMENT_WRITER_NOT_ARCHITECT",
		"409 ANSWERED_ASK_KEPT", "409 PARENT_NOT_REACHED", "409 PARENT_DOCUMENT_REMOVABLE", "409 PARENT_CONTENDED",
		"409 CONTAINER_WOULD_EMPTY", "412 PRECONDITION_FAILED", "428 PRECONDITION_REQUIRED",
	},
	generatedHeaderMember: `"If-Match": string;`,
}, {
	method: "delete", operationID: "deleteApiV1IssuesByKeyNodesById",
	request: "ActorInput", response: "NodeDeleted",
	takes: []string{"actor"}, answers: []string{"count"},
	query: []string{"dry_run", "expected_count"},
	codes: []string{
		"400 NODE_NOT_DELETABLE", "400 DELETE_INPUT", "403 DOCUMENT_WRITER_NOT_ARCHITECT", "403 STATE_NOT_PERMITTED",
		"409 NODE_HOLDS_RESULT", "409 ANSWERED_ASK_KEPT", "409 DELETE_COUNT_MOVED", "409 PARENT_CONTENDED",
		"409 CONTAINER_WOULD_EMPTY", "412 PRECONDITION_FAILED", "428 PRECONDITION_REQUIRED",
	},
	refusals:              map[string][]string{"409": {"code", "error", "count"}},
	generatedHeaderMember: `"If-Match"?: string;`,
}}

// TestNodePlacementRoutesMatchTheGeneratedContract reads the committed contract of the node's own
// resource, /api/v1/issues/{key}/nodes/{id} (R7): packages/contracts/schemas/dispatch-openapi.json
// and packages/contracts/src/dispatch-api.generated.ts. Each operation of nodePlacementOperations
// declares its body's members and its answer's, an If-Match header (required on the move, which
// every request sends, and not on the delete, whose dry run sends none), the query parameters it
// takes and none of those it must not, each refusal body richer than a code and a message (the
// delete's 409 DELETE_COUNT_MOVED and its count), and names each code of its refusals in its
// description;
// the generated TypeScript holds the operation, its If-Match member and each member of its body
// and its answer; and every node of the tree read declares placement_seq, the tag a move's If-Match
// names. A failure names the operation and the member or the code it cannot find.
func TestNodePlacementRoutesMatchTheGeneratedContract(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..", "..", "..", "..")
	encoded, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(OpenAPIDocumentPath)))
	if err != nil {
		t.Fatalf("read %s: %v", OpenAPIDocumentPath, err)
	}
	generated, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(contractsGenerated)))
	if err != nil {
		t.Fatalf("read %s: %v", contractsGenerated, err)
	}
	type parameter struct {
		Name     string `json:"name"`
		In       string `json:"in"`
		Required bool   `json:"required"`
	}
	type operation struct {
		OperationID string      `json:"operationId"`
		Description string      `json:"description"`
		Parameters  []parameter `json:"parameters"`
		RequestBody struct {
			Content map[string]struct {
				Schema map[string]any `json:"schema"`
			} `json:"content"`
		} `json:"requestBody"`
		Responses map[string]struct {
			Content map[string]struct {
				Schema map[string]any `json:"schema"`
			} `json:"content"`
		} `json:"responses"`
	}
	var document struct {
		Paths      map[string]map[string]operation `json:"paths"`
		Components struct {
			Schemas map[string]struct {
				Properties map[string]any `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatalf("decode %s: %v", OpenAPIDocumentPath, err)
	}
	const path = "/api/v1/issues/{key}/nodes/{id}"
	refOf := func(schema map[string]any) string {
		ref, _ := schema["$ref"].(string)
		return strings.TrimPrefix(ref, openAPISchemaPrefix)
	}
	members := func(what, schema string, want []string) {
		properties := document.Components.Schemas[schema].Properties
		for _, member := range want {
			if _, held := properties[member]; !held {
				t.Errorf("%s: schema %s declares no member %s", what, schema, member)
			}
		}
	}
	for _, want := range nodePlacementOperations {
		name := strings.ToUpper(want.method) + " " + path
		declared, held := document.Paths[path][want.method]
		if !held {
			t.Errorf("%s declares no operation %s", OpenAPIDocumentPath, name)
			continue
		}
		if declared.OperationID != want.operationID {
			t.Errorf("%s is operation %q, want %q", name, declared.OperationID, want.operationID)
		}
		if body := refOf(declared.RequestBody.Content["application/json"].Schema); body != want.request {
			t.Errorf("%s takes the body %q, want %s", name, body, want.request)
		}
		members(name+"'s body", want.request, want.takes)
		if answer := refOf(declared.Responses["200"].Content["application/json"].Schema); answer != want.response {
			t.Errorf("%s answers 200 with %q, want %s", name, answer, want.response)
		}
		members(name+"'s 200 answer", want.response, want.answers)
		ifMatch := slices.IndexFunc(declared.Parameters, func(p parameter) bool { return p.In == "header" && p.Name == "If-Match" })
		if ifMatch < 0 || declared.Parameters[ifMatch].Required != want.ifMatchRequired {
			t.Errorf("%s declares no If-Match header required %t: %+v", name, want.ifMatchRequired, declared.Parameters)
		}
		for _, query := range want.query {
			if !slices.ContainsFunc(declared.Parameters, func(p parameter) bool { return p.In == "query" && p.Name == query }) {
				t.Errorf("%s lacks %s", name, query)
			}
		}
		for _, query := range want.notQuery {
			if slices.ContainsFunc(declared.Parameters, func(p parameter) bool { return p.In == "query" && p.Name == query }) {
				t.Errorf("%s declares %s, which only the delete takes", name, query)
			}
		}
		for _, code := range want.codes {
			if !strings.Contains(declared.Description, code) {
				t.Errorf("%s's description names no %s", name, code)
			}
		}
		for status, declares := range want.refusals {
			schema := declared.Responses[status].Content["application/json"].Schema
			shapes := []string{refOf(schema)}
			for _, union := range []string{"oneOf", "anyOf"} {
				arms, _ := schema[union].([]any)
				for _, arm := range arms {
					if held, ok := arm.(map[string]any); ok {
						shapes = append(shapes, refOf(held))
					}
				}
			}
			if !slices.ContainsFunc(shapes, func(shape string) bool {
				properties := document.Components.Schemas[shape].Properties
				return shape != "" && !slices.ContainsFunc(declares, func(member string) bool { _, held := properties[member]; return !held })
			}) {
				t.Errorf("%s's %s answer declares no body with %v among %v", name, status, declares, shapes)
			}
		}
		operationAt := strings.Index(string(generated), "\n  "+want.operationID+": {")
		if operationAt < 0 {
			t.Errorf("%s holds no operation %s for %s", contractsGenerated, want.operationID, name)
			continue
		}
		rest := string(generated[operationAt+1:])
		if end := strings.Index(rest, "\n  }"); end >= 0 {
			rest = rest[:end]
		}
		if !strings.Contains(rest, want.generatedHeaderMember) {
			t.Errorf("%s's operation %s holds no %s", contractsGenerated, want.operationID, want.generatedHeaderMember)
		}
		for _, schema := range []string{want.request, want.response} {
			if !strings.Contains(string(generated), "export type "+schema+" = ") {
				t.Errorf("%s exports no type %s for %s", contractsGenerated, schema, name)
			}
		}
	}
	members("the tree read's nodes", "IssueTreeNode", []string{"placement_seq"})
}

// --- TestThePatchRefusesAParentTheWalkDoesNotReach --------------------------------------------

// walkReachSpec is the spec TestThePatchRefusesAParentTheWalkDoesNotReach writes: a paragraph the
// test removes, an ask, and two decisions.
const walkReachSpec = "A paragraph the next version removes.\n\n:::ask{#q1}\nWhich store do the writers take?\n:::\n\n" +
	":::decision{#d1}\nKeep one store.\n:::\n\n:::decision{#d2}\nWrite through the ledger.\n:::\n"

// decisionsQueryOf is the artifact id of the decisions query of the issue key.
func decisionsQueryOf(t *testing.T, database *store.Store, key string) string {
	t.Helper()
	var id string
	if err := database.Pool.QueryRow(context.Background(), `
		select a.id::text from artifacts a
		join graph_nodes n on n.kind = $2 and n.id = a.ref_key and n.supertag = $3 and n.attributes->>$4 = $5
		where a.issue_key = $1`,
		key, graphstore.KindArtifact, graphstore.SupertagQuery, graphstore.AttributeQuery, graphstore.QueryDecisions).Scan(&id); err != nil {
		t.Fatalf("read the decisions query of %s: %v", key, err)
	}
	return id
}

// TestThePatchRefusesAParentTheWalkDoesNotReach holds the move to R7's reach rule: it refuses,
// with 409 PARENT_NOT_REACHED, a parent the walk of the issue it stands under does not reach,
// naming the parent and why, and stores nothing, so the decisions query's newest version stays the
// one read before; and a move it takes changes no node the decisions query takes, so the version
// the drain writes of that query holds the bytes of the version before it.
//
// On an issue born converted whose spec holds a paragraph, an ask and two decisions, an agent's
// session writes a status note on the issue and a comment on the second decision's text, which no
// person answers, and alice removes the paragraph through the edit route, so its node is gone once
// the block index states that version. Alice then moves the first decision under each of four
// parents: the agent's comment on the decision's text, the status note, the status note's first
// block, whose parent the walk does not reach, and the paragraph's gone node. Each answers 409
// PARENT_NOT_REACHED naming the parent and the reason, the decision keeps its part_of, and the
// decisions query stands at the version read before, byte for byte. Last she moves the first
// decision under the second, which the walk reaches: the move is taken, and the version of the
// decisions query the drain writes for it holds the bytes of the version before it.
func TestThePatchRefusesAParentTheWalkDoesNotReach(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{})
	createReferenceAPIProject(t, handler, "WALK")
	issue := createDigestIssue(t, handler, database, "WALK", "The walk's reach", walkReachSpec, false)
	key := issue.Key
	var paragraphID string
	if err := database.Pool.QueryRow(context.Background(), `
		select d.block_id from document_blocks d join artifacts a on a.id = d.artifact_id
		where a.issue_key = $1 and d.type = 'paragraph' and d.parent_block_id is null`, key).Scan(&paragraphID); err != nil {
		t.Fatalf("read the paragraph of %s: %v", key, err)
	}
	paragraph := blockAddressOf(t, database, key, paragraphID)
	note := decodeBody[model.Comment](t, created(t, "write the agent's status note", sessionRequest(t, handler, http.MethodPost,
		"/api/v1/issues/"+key+"/comments", map[string]any{"body": "Working through the store's writers.", "actor": sessionActor()})))
	removed := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits",
		map[string]any{"ops": []map[string]any{{"op": "delete", "block": paragraphID}}}, "alice")
	if removed.Code != http.StatusOK {
		t.Fatalf("remove the paragraph %s: status=%d body=%s", paragraph, removed.Code, removed.Body.String())
	}
	onText := decodeBody[model.Comment](t, created(t, "write the agent's comment on the second decision's text", sessionRequest(t, handler,
		http.MethodPost, "/api/v1/issues/"+key+"/comments", map[string]any{
			"body": "Does the ledger hold every write?", "anchor": map[string]any{"artifact": "spec", "quote": "Write through the ledger"},
			"actor": sessionActor(),
		})))
	decision := blockAddressOf(t, database, key, "d1")
	decisions := decisionsQueryOf(t, database, key)
	before := newestQueryVersion(t, database, decisions)
	for _, parent := range []struct{ address, names, why string }{
		{graphstore.KindComment + "/" + onText.ID, onText.ID, "conversation test"},
		{graphstore.KindComment + "/" + note.ID, note.ID, "conversation test"},
		{graphstore.KindComment + "/" + note.ID + "#b1", note.ID, "its own parent is not reached"},
		{paragraph, paragraphID, "it is gone"},
	} {
		held, tag := placementOf(t, database, "block", decision)
		response := patchNode(t, handler, key, decision, tag, map[string]any{"parent": parent.address}, "alice")
		refusal := decodeBody[refusalAnswer](t, response)
		if response.Code != http.StatusConflict || refusal.Code != codeParentNotReached {
			t.Errorf("the move of %s under %s answered %d %s, want 409 PARENT_NOT_REACHED", decision, parent.address, response.Code, refusal.Code)
		} else if !strings.Contains(refusal.Error, parent.names) || !strings.Contains(refusal.Error, parent.why) {
			t.Errorf("the refusal of the move of %s under %s does not name the parent and its reason: %s", decision, parent.address, response.Body.String())
		}
		if now, _ := placementOf(t, database, "block", decision); now.Seq != held.Seq {
			t.Errorf("the refused move of %s under %s changed its part_of to %s %s", decision, parent.address, now.ToKind, now.ToID)
		}
		if after := newestQueryVersion(t, database, decisions); after.number != before.number || after.markdown != before.markdown {
			t.Errorf("the decisions query of %s stands at version %d after a refused move, want %d", key, after.number, before.number)
		}
	}
	_, tag := placementOf(t, database, "block", decision)
	second := blockAddressOf(t, database, key, "d2")
	if response := patchNode(t, handler, key, decision, tag, map[string]any{"parent": second}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("the move of %s under %s answered %d %s, want 200", decision, second, response.Code, response.Body.String())
	}
	if moved, _ := placementOf(t, database, "block", decision); moved.ToID != second {
		t.Fatalf("the move of %s under %s left it under %s %s", decision, second, moved.ToKind, moved.ToID)
	}
	// The move changes no node the decisions query takes, and the query orders its nodes by when each
	// was made: a version the drain writes of it holds the bytes of the version before it, and where
	// the move moved nothing of its record the drain writes none, and the newest stays as it was.
	// Either way the mark the move left is drained.
	if mark, held := readQueryMarkRow(t, database, key); held {
		t.Errorf("the move of %s under %s left %s's queries owed: greatest_seq %d", decision, second, key, mark.seq)
	}
	after := newestQueryVersion(t, database, decisions)
	previous := before.markdown
	if after.number > before.number {
		previous = queryVersionMarkdown(t, database, decisions, after.number-1)
	}
	t.Logf("the decisions query %s stands at version %d after the move, %d before it", decisions, after.number, before.number)
	if after.markdown != previous {
		at := 0
		for at < len(after.markdown) && at < len(previous) && after.markdown[at] == previous[at] {
			at++
		}
		t.Errorf("the version of %s written for the move of %s under %s differs from the version before it from byte %d", decisions, decision, second, at)
	}
}

// queryVersionMarkdown is the stored markdown of version number of the document artifactID.
func queryVersionMarkdown(t *testing.T, database *store.Store, artifactID string, number int) string {
	t.Helper()
	var markdown string
	if err := database.Pool.QueryRow(context.Background(),
		`select coalesce(markdown, '') from artifact_versions where artifact_id = $1 and number = $2`, artifactID, number).Scan(&markdown); err != nil {
		t.Fatalf("read version %d of %s: %v", number, artifactID, err)
	}
	return markdown
}

// --- TestAMoveUnderALensOfAParentIssueWaitsForThatIssuesLock ----------------------------------

// TestAMoveUnderALensOfAParentIssueWaitsForThatIssuesLock holds the move to R7's owner locks: a
// move takes the owner lock of every issue a node of the parent's part_of chain stands under, so a
// move under a lens of a parent issue waits for that issue's lock, and once the move that held it
// has committed, decides on what that move wrote.
//
// In its subtest criterion, an issue K1 holds a criterion, a lens L of kind trade-off stands on that
// criterion, U is a document of K1 made by a title alone, and Q is a requirement of K2, a child of
// K1. The test holds K1's owner lock in a transaction A of its own (lockstep.HoldOwner) and in A
// moves L onto U through the store's write: one part_of to U's node and the retraction of the one
// to the criterion. It then sends move B, Q under L, through PATCH /api/v1/issues/{K2}/nodes/{Q}
// on a handler whose store holds one connection of its own (oneConnectionHandler), and waits for
// that backend to queue behind A (storetest.WaitForQueuedBehind), at the statement that takes an
// owner's lock (lockstep.RequireStoppedAt). It then commits A. B answers 409
// PARENT_DOCUMENT_REMOVABLE naming Q, L and U, and Q's part_of is the one it had.
//
// In its subtest comment, the parent's chain holds a comment, which stands under the issue its row
// names wherever its part_of places it. Alice writes, in K2, a child of K1, a comment on the text of
// a lens of K2, and moves the lens under K1's node through PATCH: the comment keeps its issue, K2,
// and its part_of to the lens. The test holds K2's owner lock in A, then sends move B of a paragraph
// of K3, a child of K2, under the comment, through PATCH /api/v1/issues/{K3}/nodes/{id}: B waits at
// the statement that takes an owner's lock, since the comment's row names K2, and once A commits B
// is taken, the paragraph stands under the comment, and the comment's part_of still names the lens:
// no request moves the comment itself.
func TestAMoveUnderALensOfAParentIssueWaitsForThatIssuesLock(t *testing.T) {
	t.Parallel()
	t.Run("criterion", func(t *testing.T) {
		handler, database, deps := newTestServer(t, testServerOptions{})
		ctx := context.Background()
		createReferenceAPIProject(t, handler, "LOCKMV")
		k1 := createDigestIssue(t, handler, database, "LOCKMV", "The parent", ":::criterion{#c1}\nThe store answers in time.\n:::\n", false)
		k2 := createDigestChild(t, handler, database, "LOCKMV", "The child",
			":::requirement{#q1}\nEvery read answers in a second.\n:::\n", k1.Key, false)
		u := emptyDocument(t, handler, k1.Key, "Notes U.md")
		lens := decodeBody[lensInvoked](t, created(t, "invoke the lens L on the criterion", dispatchRequest(t, handler, http.MethodPost,
			"/api/v1/issues/"+k1.Key+"/lenses", map[string]any{"on": "dispatch://" + k1.Key + "/spec#b-c1", "kind": "trade-off"}, "alice")))
		q := blockAddressOf(t, database, k2.Key, "q1")
		held, tag := placementOf(t, database, "block", q)
		place, _ := placementOf(t, database, graphstore.KindLens, lens.ID)

		writeHandler, writePID := oneConnectionHandler(t, deps)
		owner := lockstep.HoldOwner(t, database, &lockstep.Hold{
			Name: "issue " + k1.Key + "'s owner lock", Step: lockstep.StepOwner, Taken: true, Statement: lockstep.OwnerLockStatement,
		}, store.IssueOwner(k1.Key))
		moved := place
		moved.Asserted, moved.At, moved.Who, moved.SourceEvent = false, time.Now().UTC(), model.Actor{Kind: "user", ID: "alice"}, graphstore.NoEvent
		placed := moved
		placed.ToKind, placed.ToID, placed.Asserted = graphstore.KindArtifact, u.RefKey, true
		if err := graphstore.Write(ctx, owner.Tx, nil, []graphstore.Tuple{moved, placed}); err != nil {
			t.Fatalf("move L %s onto U %s in A: %v", lens.ID, u.RefKey, err)
		}
		answered := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			answered <- patchNode(t, writeHandler, k2.Key, q, tag, map[string]any{"parent": graphstore.KindLens + "/" + lens.ID}, "alice")
		}()
		if early, answeredEarly := storetest.WaitForQueuedBehind(t, owner.Tx, writePID, answered); answeredEarly {
			_ = owner.Tx.Commit(ctx)
			now, _ := placementOf(t, database, "block", q)
			t.Fatalf("the move of %s under %s did not wait for the owner lock of %s, which the move of %s onto %s held: it answered %d, and %s stands under %s",
				q, lens.ID, k1.Key, lens.ID, u.RefKey, early.Code, q, now.ToID)
		}
		lockstep.RequireStoppedAt(t, ctx, database, writePID, "the move of "+q, owner)
		if err := owner.Tx.Commit(ctx); err != nil {
			t.Fatalf("commit A: %v", err)
		}
		response := testwait.Receive(t, "the move of "+q+" under "+lens.ID, answered)
		refusal := decodeBody[refusalAnswer](t, response)
		if response.Code != http.StatusConflict || refusal.Code != codeParentDocumentRemovable {
			t.Errorf("the move of %s under %s answered %d %s after the move of %s onto %s committed, want 409 PARENT_DOCUMENT_REMOVABLE",
				q, lens.ID, response.Code, response.Body.String(), lens.ID, u.RefKey)
		} else {
			for _, named := range []string{"q1", lens.ID, u.RefKey, "requirement"} {
				if !strings.Contains(refusal.Error, named) {
					t.Errorf("the refusal of the move of %s under %s names no %s: %s", q, lens.ID, named, refusal.Error)
				}
			}
		}
		if now, _ := placementOf(t, database, "block", q); now.Seq != held.Seq {
			t.Errorf("the refused move of %s changed its part_of to %s %s", q, now.ToKind, now.ToID)
		}
	})
	t.Run("comment", func(t *testing.T) {
		handler, database, deps := newTestServer(t, testServerOptions{})
		ctx := context.Background()
		createReferenceAPIProject(t, handler, "LOCKCM")
		k1 := createDigestIssue(t, handler, database, "LOCKCM", "The parent", "The parent's paragraph.\n", false)
		k2 := createDigestChild(t, handler, database, "LOCKCM", "The child", ":::criterion{#c2}\nThe child's store answers in time.\n:::\n", k1.Key, false)
		k3 := createDigestChild(t, handler, database, "LOCKCM", "The grandchild", "The grandchild's paragraph moves under the comment.\n", k2.Key, false)
		lens := decodeBody[lensInvoked](t, created(t, "invoke a lens on K2's criterion", dispatchRequest(t, handler, http.MethodPost,
			"/api/v1/issues/"+k2.Key+"/lenses", map[string]any{"on": "dispatch://" + k2.Key + "/spec#b-c2", "kind": "trade-off", "verdict": "The read path holds one cache."}, "alice")))
		lensNode := graphstore.KindLens + "/" + lens.ID
		comment := decodeBody[model.Comment](t, created(t, "alice's comment on the lens's text", dispatchRequest(t, handler, http.MethodPost,
			"/api/v1/issues/"+k2.Key+"/comments", map[string]any{"body": "Which cache is it?", "anchor": map[string]any{"node": lensNode, "quote": "one cache"}}, "alice")))
		_, lensTag := placementOf(t, database, graphstore.KindLens, lens.ID)
		if response := patchNode(t, handler, k2.Key, lensNode, lensTag, map[string]any{"parent": graphstore.KindIssue + "/" + k1.Key}, "alice"); response.Code != http.StatusOK {
			t.Fatalf("the move of the lens %s under %s answered %d %s, want 200", lens.ID, k1.Key, response.Code, response.Body.String())
		}
		var commentIssue string
		if err := database.Pool.QueryRow(ctx, `select coalesce(issue_key, '') from comments where id = $1::uuid`, comment.ID).Scan(&commentIssue); err != nil {
			t.Fatalf("read the issue of the comment %s: %v", comment.ID, err)
		}
		onLens, _ := placementOf(t, database, graphstore.KindComment, comment.ID)
		if commentIssue != k2.Key || onLens.ToKind != graphstore.KindLens || onLens.ToID != lens.ID {
			t.Fatalf("the comment %s stands in %s under %s %s after the move of its lens, want in %s under the lens %s", comment.ID, commentIssue, onLens.ToKind, onLens.ToID, k2.Key, lens.ID)
		}
		paragraph := blockWithText(t, database, "The grandchild's paragraph")
		held, tag := placementOf(t, database, "block", paragraph)

		writeHandler, writePID := oneConnectionHandler(t, deps)
		owner := lockstep.HoldOwner(t, database, &lockstep.Hold{
			Name: "issue " + k2.Key + "'s owner lock", Step: lockstep.StepOwner, Taken: true, Statement: lockstep.OwnerLockStatement,
		}, store.IssueOwner(k2.Key))
		answered := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			answered <- patchNode(t, writeHandler, k3.Key, paragraph, tag, map[string]any{"parent": graphstore.KindComment + "/" + comment.ID}, "alice")
		}()
		if early, answeredEarly := storetest.WaitForQueuedBehind(t, owner.Tx, writePID, answered); answeredEarly {
			_ = owner.Tx.Commit(ctx)
			t.Fatalf("the move of %s under the comment %s did not wait for the owner lock of %s, the issue the comment's row names: it answered %d %s",
				paragraph, comment.ID, k2.Key, early.Code, early.Body.String())
		}
		lockstep.RequireStoppedAt(t, ctx, database, writePID, "the move of "+paragraph, owner)
		if err := owner.Tx.Commit(ctx); err != nil {
			t.Fatalf("commit A: %v", err)
		}
		response := testwait.Receive(t, "the move of "+paragraph+" under the comment "+comment.ID, answered)
		if response.Code != http.StatusOK {
			t.Fatalf("the move of %s under the comment %s answered %d %s once %s's owner lock was released, want 200", paragraph, comment.ID, response.Code, response.Body.String(), k2.Key)
		}
		if now, _ := placementOf(t, database, "block", paragraph); now.ToKind != graphstore.KindComment || now.ToID != comment.ID || now.Seq == held.Seq {
			t.Errorf("the move of %s left it under %s %s, want the comment %s", paragraph, now.ToKind, now.ToID, comment.ID)
		}
		if still, _ := placementOf(t, database, graphstore.KindComment, comment.ID); still.ToKind != graphstore.KindLens || still.ToID != lens.ID {
			t.Errorf("the comment %s stands under %s %s after the move under it, want the lens %s: no request moves the comment itself", comment.ID, still.ToKind, still.ToID, lens.ID)
		}
	})
}

// --- the move's lock steps (TestEveryApiStoreWriterTakesTheStoreLockOrder) --------------------

// nodeMove is a flat move of a node (moveNodeRoute, queueNodeMove), through its route: the owner
// lock of the rig's issue, the one issue every node of the move stands under (lockIssueOwners),
// then, in its write phase, the move's tuples alone at the sentinel - the retraction of the block's
// part_of, the new one and its rank - with no node and no event, then the block's
// document_blocks.parent_block_id, the projection of its part_of, and the mark of the issue's
// queries owed a version (queryMarkStep). Each run moves the spec's paragraph between the issue and
// the heading above it, places Markdown shows no differently, so the move writes no version and
// takes no room's lock. The tuples, the row and the mark are held as the tables they are written to.
func (r *storeWriterRig) nodeMove() storeWriter {
	return storeWriter{
		name: "moveNodeRoute",
		steps: []storeStep{
			ownerLockStep(store.IssueOwner(r.issue)),
			{
				name: "the move's tuples", order: tupleStep,
				hold: holdTableWrites("graph_tuples"), at: tupleOnlyAppendStatement,
				takes: []tableLock{{"graph_tuples", rowExclusiveMode}},
			},
			{
				name: "the moved block's row", order: projectionStep,
				hold: holdTableWrites("document_blocks"), at: "update document_blocks set parent_block_id",
				takes: []tableLock{{"document_blocks", rowExclusiveMode}},
			},
			queryMarkStep(),
		},
		ready: func(t *testing.T, ctx context.Context) storeWriterRun {
			var paragraph, heading string
			if err := r.database.Pool.QueryRow(ctx, `
				select p.node_address, h.node_address
				from document_blocks p join document_blocks h on h.artifact_id = p.artifact_id and h.type = 'heading'
				where `+refs.BlockDocumentSQL("p")+` = $1::uuid and `+refs.BlockDocumentSQL("h")+` = $1::uuid
				  and p.type = 'paragraph'`, r.spec).Scan(&paragraph, &heading); err != nil {
				t.Fatalf("read the spec's paragraph and heading of %s: %v", r.issue, err)
			}
			place, tag := placementOf(t, r.database, "block", paragraph)
			parent := heading
			if place.ToID == heading {
				parent = graphstore.KindIssue + "/" + r.issue
			}
			return storeWriterRun{
				write: func() error {
					return statusOrError(patchNode(t, r.call, r.issue, paragraph, tag, map[string]any{"parent": parent}, "alice"), http.StatusOK)
				},
			}
		},
	}
}
