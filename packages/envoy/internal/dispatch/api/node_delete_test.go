package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// nodeCaller is one caller of the node routes: the request it sends of method to path, with body
// and with header beside the credential it presents.
type nodeCaller func(method, path string, body any, header http.Header) *httptest.ResponseRecorder

// serveNodeRequest sends method to path on handler with body as JSON and every value of header.
func serveNodeRequest(t testing.TB, handler http.Handler, method, path string, body any, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode the body of %s %s: %v", method, path, err)
		}
		reader = bytes.NewReader(data)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Content-Type", "application/json")
	for name, values := range header {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// personCaller is the person login, signed in with the test identity's header, as dispatchRequest
// sends one: a caller whose credential holds the person scope.
func personCaller(t testing.TB, handler http.Handler, login string) nodeCaller {
	return func(method, path string, body any, header http.Header) *httptest.ResponseRecorder {
		header = header.Clone()
		if header == nil {
			header = http.Header{}
		}
		header.Set("X-Dispatch-User", login)
		return serveNodeRequest(t, handler, method, path, body, header)
	}
}

// brokerAgent is a broker-issued token bound to session, which acts for person, as settleRig.agent
// mints one, sending requests that may carry headers: it proves an author and holds no person
// scope. Its proof names the target without its query, as RFC 9449 has a DPoP proof name it.
func brokerAgent(t *testing.T, rig *brokerTokenRig, session, person, token string) nodeCaller {
	t.Helper()
	caller := newBrokerCaller(t)
	answer := liveNamedToken(caller.jkt)
	answer.Session, answer.Person = session, person
	rig.broker.Says(token, answer)
	return func(method, path string, body any, header http.Header) *httptest.ResponseRecorder {
		target, _, _ := strings.Cut(path, "?")
		signed := caller.caller.Signed(t, method, testServerURL+target, token)
		request := brokerRequest(t, method, path, body, signed.Header.Get("Authorization"), signed.Header.Get("DPoP"))
		for name, values := range header {
			for _, value := range values {
				request.Header.Add(name, value)
			}
		}
		return rig.serve(request)
	}
}

// deleteNodePath is the path of DELETE /api/v1/issues/{key}/nodes/{id} for the node id names, with
// dry_run=true where dry is set and expected_count where expected is at least 0.
func deleteNodePath(key, id string, dry bool, expected int) string {
	query := url.Values{}
	if dry {
		query.Set("dry_run", "true")
	}
	if expected >= 0 {
		query.Set("expected_count", strconv.Itoa(expected))
	}
	path := "/api/v1/issues/" + key + "/nodes/" + url.PathEscape(id)
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	return path
}

// deleteNodeAs sends the delete of id under key as as: the dry run where dry is set, the delete
// with expected_count where expected is at least 0, and If-Match tag where tag is not "".
func deleteNodeAs(as nodeCaller, key, id string, dry bool, expected int, tag string) *httptest.ResponseRecorder {
	header := http.Header{}
	if tag != "" {
		header.Set("If-Match", tag)
	}
	return as(http.MethodDelete, deleteNodePath(key, id, dry, expected), nil, header)
}

// patchNodeAs sends the move of id under key to body as as, with If-Match tag.
func patchNodeAs(as nodeCaller, key, id, tag string, body map[string]any) *httptest.ResponseRecorder {
	return as(http.MethodPatch, "/api/v1/issues/"+key+"/nodes/"+url.PathEscape(id), body, http.Header{"If-Match": {tag}})
}

// deletedCount is the count a delete or its dry run answered, failing the test on any status but
// 200, naming what.
func deletedCount(t *testing.T, what string, response *httptest.ResponseRecorder) int {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("%s answered %d %s, want 200", what, response.Code, response.Body.String())
	}
	return decodeBody[nodeDeleted](t, response).Count
}

// deleteCounts is what a refused delete and a dry run leave as they found them: the store's
// graph_tuples rows, every artifact_versions row, every events row and every doc_updates row.
type deleteCounts struct {
	tuples, versions, events, updates int
}

func readDeleteCounts(t *testing.T, database *store.Store) deleteCounts {
	t.Helper()
	var counts deleteCounts
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from graph_tuples), (select count(*) from artifact_versions),
		       (select count(*) from events), (select count(*) from doc_updates)`).Scan(
		&counts.tuples, &counts.versions, &counts.events, &counts.updates); err != nil {
		t.Fatalf("count the store's tuples, versions, events and room updates: %v", err)
	}
	return counts
}

// requireUnchanged fails where what changed the store's counts since before.
func requireUnchanged(t *testing.T, database *store.Store, what string, before deleteCounts) {
	t.Helper()
	if after := readDeleteCounts(t, database); after != before {
		t.Errorf("%s changed the store: tuples, versions, events and room updates %+v, want %+v", what, after, before)
	}
}

// refusedWith fails unless response is status with code, naming what; it answers the refusal's
// message.
func refusedWith(t *testing.T, what string, response *httptest.ResponseRecorder, status int, code string) string {
	t.Helper()
	refusal := decodeBody[refusalAnswer](t, response)
	if response.Code != status || refusal.Code != code {
		t.Errorf("%s answered %d %s, want %d %s", what, response.Code, response.Body.String(), status, code)
	}
	if strings.Contains(response.Body.String(), `"count"`) && code != codeDeleteCountMoved {
		t.Errorf("%s answered a count beside its refusal: %s", what, response.Body.String())
	}
	return refusal.Error
}

// blockWithText is the node of the block whose current text value begins with prefix, failing the
// test where none does.
func blockWithText(t *testing.T, database *store.Store, prefix string) string {
	t.Helper()
	var address string
	if err := database.Pool.QueryRow(context.Background(), `
		select t.from_id from graph_tuples t
		where t.from_kind = 'block' and t.kind = $1 and t.to_kind = $2 and t.asserted and starts_with(t.to_id, $3)
		order by t.seq desc limit 1`, graphstore.AttributeText, graphstore.KindValue, prefix).Scan(&address); err != nil {
		t.Fatalf("read the block whose text begins %q: %v", prefix, err)
	}
	return address
}

// nodeStateOf is the state the store holds of the node kind and id key, "" where it holds none.
func nodeStateOf(t *testing.T, database *store.Store, kind, id string) string {
	t.Helper()
	var state string
	if err := database.Pool.QueryRow(context.Background(),
		`select coalesce((select state from graph_nodes where kind = $1 and id = $2), '')`, kind, id).Scan(&state); err != nil {
		t.Fatalf("read the state of %s %s: %v", kind, id, err)
	}
	return state
}

// relationsOutOf is every current relation out of the node kind and id key, of every kind.
func relationsOutOf(t *testing.T, database *store.Store, kind, id string) []graphstore.Tuple {
	t.Helper()
	held, err := graphstore.CurrentOut(context.Background(), database.Pool, kind, id, nil)
	if err != nil {
		t.Fatalf("read the tuples out of %s %s: %v", kind, id, err)
	}
	var relations []graphstore.Tuple
	for _, tuple := range held {
		if tuple.ToKind != graphstore.KindValue {
			relations = append(relations, tuple)
		}
	}
	return relations
}

// holdsRelation reports whether the node kind and id key holds a current relation of tupleKind to
// the node toID.
func holdsRelation(t *testing.T, database *store.Store, kind, id, tupleKind, toID string) bool {
	t.Helper()
	return slices.ContainsFunc(relationsOutOf(t, database, kind, id), func(tuple graphstore.Tuple) bool {
		return tuple.Kind == tupleKind && tuple.ToID == toID
	})
}

// versionCount is the number of versions of the document artifactID.
func versionCount(t *testing.T, database *store.Store, artifactID string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from artifact_versions where artifact_id = $1`, artifactID).Scan(&count); err != nil {
		t.Fatalf("count the versions of %s: %v", artifactID, err)
	}
	return count
}

// queryArtifactOf is the artifact id of the query query of the issue key.
func queryArtifactOf(t *testing.T, database *store.Store, key, query string) string {
	t.Helper()
	var id string
	if err := database.Pool.QueryRow(context.Background(), `
		select a.id::text from artifacts a
		join graph_nodes n on n.kind = $2 and n.id = a.ref_key and n.supertag = $3 and n.attributes->>$4 = $5
		where a.issue_key = $1`,
		key, graphstore.KindArtifact, graphstore.SupertagQuery, graphstore.AttributeQuery, query).Scan(&id); err != nil {
		t.Fatalf("read the %s query of %s: %v", query, key, err)
	}
	return id
}

// askOfBlock is the id of the ask whose `:::ask` block is block of the issue key, waiting for the
// settlement that opens it.
func askOfBlock(t *testing.T, database *store.Store, key, block string) string {
	t.Helper()
	var id string
	testwait.Eventually(t, "the ask of block "+block+" of "+key, func() bool {
		err := database.Pool.QueryRow(context.Background(),
			`select id::text from asks where issue_key = $1 and block_id = $2`, key, block).Scan(&id)
		return err == nil
	})
	return id
}

// anchoredComment is one comment as the comments read answers it with ?node_anchor=true: its id,
// its document anchor and its node anchor (model.CommentNodeAnchor).
type anchoredComment struct {
	ID         string                   `json:"id"`
	Anchor     *model.Anchor            `json:"anchor"`
	NodeAnchor *model.CommentNodeAnchor `json:"node_anchor"`
}

// commentsWithNodeAnchors is GET /api/v1/issues/{key}/comments?node_anchor=true as alice reads it,
// by comment id.
func commentsWithNodeAnchors(t *testing.T, handler http.Handler, key string) map[string]anchoredComment {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/comments?node_anchor=true", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the comments of %s with their node anchors: status=%d body=%s", key, response.Code, response.Body.String())
	}
	read := map[string]anchoredComment{}
	for _, comment := range decodeBody[[]anchoredComment](t, response) {
		read[comment.ID] = comment
	}
	return read
}

// treeNodes is GET /api/v1/issues/{key}/tree with query as alice reads it, by node id.
func treeNodes(t *testing.T, handler http.Handler, key, query string) map[string]model.IssueTreeNode {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/tree"+query, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the tree of %s%s: status=%d body=%s", key, query, response.Code, response.Body.String())
	}
	nodes := map[string]model.IssueTreeNode{}
	for _, node := range decodeBody[model.IssueTreeRead](t, response).Nodes {
		nodes[node.ID] = node
	}
	return nodes
}

// The words of the delete test's fixture, each unique in the store, so a block is found by them
// (blockWithText).
const (
	deleteR1Words = "Every read answers in a second."
	deleteR2Words = "A refused write stores nothing."
	deleteC1Words = "The read answers within the second."
	deleteC4Words = "Nothing is stored for a refused write."
	deletePWords  = "P stands in the room's own fragment."
	deleteQWords  = "Q stands in the child's room and moves under R1."
	deleteYWords  = "The only paragraph of the kept callout."
	deleteD6Words = "Keep one ledger for every write."
)

// deleteSpec is the spec of the delete test's issue: a criterion K first, so no callout is its
// memo, the requirements R1 and R2, R6 with the decision D6 alice decides, a callout Y holding one
// paragraph, and R7, under which alice moves Y's paragraph.
const deleteSpec = ":::criterion{#k}\nThe store answers every reader.\n:::\n\n" +
	":::requirement{#r1}\n" + deleteR1Words + "\n:::\n\n" +
	":::requirement{#r2}\n" + deleteR2Words + "\n:::\n\n" +
	":::requirement{#r6}\nThe ledger keeps every write.\n:::\n\n" +
	":::decision{#d6}\n" + deleteD6Words + "\n:::\n\n" +
	":::callout{#y kind=\"note\" title=\"\"}\n" + deleteYWords + "\n:::\n\n" +
	":::requirement{#r7}\nA requirement that holds the callout's paragraph.\n:::\n"

// deleteVerification is the Verification document of the issue key: C1 hanging under R1 and
// serving R1 and R2; C2 serving R1 and R2, which alice moves under R2; C3, serving nothing, which
// holds a result; and the callout X1 holding C4, which serves R2 alone.
func deleteVerification(key string) string {
	r1, r2 := "dispatch://"+key+"/spec#b-r1", "dispatch://"+key+"/spec#b-r2"
	return fmt.Sprintf(":::check{#c1 serves=\"%[1]s %[2]s\"}\n%[3]s\n:::\n\n"+
		":::check{#c2 serves=\"%[1]s %[2]s\"}\nA refused write leaves the store as it was.\n:::\n\n"+
		":::check{#c3}\nThe result of this check stands.\n:::\n\n"+
		"::::callout{#x1 kind=\"note\" title=\"\"}\n:::check{#c4 serves=\"%[2]s\"}\n%[4]s\n:::\n::::\n",
		r1, r2, deleteC1Words, deleteC4Words)
}

// deleteAgentMarkdown is what the first agent writes into the spec: R4, the callout X4 holding two
// `:::ask` blocks, and R5.
const deleteAgentMarkdown = ":::requirement{#r4}\nEach write names its writer.\n:::\n\n" +
	"::::callout{#x4 kind=\"note\" title=\"\"}\n" +
	":::ask{#a4 urgency=\"high\" multiple=\"false\"}\nWhich writer signs a delete?\n:::\n\n" +
	":::ask{#a5 urgency=\"high\" multiple=\"false\"}\nWhich store holds the ledger?\n:::\n::::\n\n" +
	":::requirement{#r5}\nThe agent's own requirement.\n:::\n"

// The two agents of the delete test, each a broker session acting for a person of its own.
const (
	deleteFirstSession  = "ses_delete_agent_0001"
	deleteSecondSession = "ses_delete_agent_0002"
)

// TestADeleteTakesTheNodeAndItsSubtreeOutInOneWrite is c-node-delete: DELETE
// /api/v1/issues/{key}/nodes/{id} takes the node and its part_of subtree out in one transaction,
// less each comment, message and issue node and what stands under each, in one direct write of each
// document's tree under the room locks, and its dry run answers the count a read of the tree returns
// to the caller, applying each refusal the delete applies, in the same order (R7, R28, R32).
//
// On the server of c-settle's tests (settleRig), alice, in the browser, creates an issue born
// converted and approves its spec query. The spec holds the criterion K and the requirements R1,
// R2, R6 (with alice's decision D6 moved under it), R7 and a callout Y; alice moves R1 under K. The
// Verification document holds C1 under R1 serving R1 and R2, C2 serving both, which alice moves
// under R2, C3 with a result, and the callout X1 holding C4, which serves R2 alone; alice moves X1
// under R1, writes P into the room's own fragment and moves it under R1, invokes a lens L with a
// finding F on R1 and comments on L's text by node anchor, moves a child issue's node, which holds
// no part_of yet, under K with the tag of its child_of, which the repeat of that move then names
// stale, and moves that child's paragraph Q under R1 and Y's paragraph under R7. The first agent writes R4,
// the callout X4 holding two asks, one of which alice answers, moves X4 under R4, writes R5 and
// comments on R1's and R2's text; the second agent reserves a lens on R5 for itself.
func TestADeleteTakesTheNodeAndItsSubtreeOutInOneWrite(t *testing.T) {
	t.Parallel()
	rig := newSettleRig(t)
	handler, database := rig.handler, rig.database
	ctx := context.Background()
	alice := personCaller(t, handler, "alice")
	first := brokerAgent(t, rig.brokerTokenRig, deleteFirstSession, settleFirstPerson, "the-first-deleting-agent's-token")
	second := brokerAgent(t, rig.brokerTokenRig, deleteSecondSession, settleSecondPerson, "the-second-deleting-agent's-token")

	issue := createDigestIssue(t, handler, database, brokerTestProject, "What a delete takes", deleteSpec, false)
	key := issue.Key
	spec := readDecisionDocument(t, database, key, "spec")
	specQuery := queryArtifactOf(t, database, key, graphstore.QuerySpec)
	awaitFirstRender(t, database, specQuery)
	approved := reviewDocument(t, handler, specQuery, "alice", "approved", "")
	move := func(as nodeCaller, owner, node, parent string) {
		t.Helper()
		_, tag := placementOf(t, database, refsKindOf(node), storeIDOf(node))
		if response := patchNodeAs(as, owner, node, tag, map[string]any{"parent": parent}); response.Code != http.StatusOK {
			t.Fatalf("the move of %s under %s answered %d %s, want 200", node, parent, response.Code, response.Body.String())
		}
	}
	approvedView := "?view=spec&v=" + strconv.Itoa(approved.Version)
	if _, held := treeNodes(t, handler, key, approvedView)[spec.address("r1")]; !held {
		t.Fatalf("the approved spec version %d does not answer R1 before the delete", approved.Version)
	}
	r1, r2, r6, r7, k := spec.address("r1"), spec.address("r2"), spec.address("r6"), spec.address("r7"), spec.address("k")
	move(alice, key, r1, k)
	move(alice, key, spec.address("d6"), r6)
	uploadDocument(t, handler, "/api/v1/issues/"+key, "verification.md", deleteVerification(key))
	verification := readDecisionDocument(t, database, key, model.ArtifactSlugVerification)
	c1, c2, c3, c4, x1 := verification.address("c1"), verification.address("c2"), verification.address("c3"), verification.address("c4"), verification.address("x1")
	move(alice, key, c2, r2)
	move(alice, key, x1, r1)
	tree := readDecisionDocument(t, database, key, issueTreeName)
	if edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+tree.id+"/edits", map[string]any{"ops": []map[string]any{{
		"op": "insert", "after": "end", "markdown": deletePWords + "\n",
	}}}, "alice"); edited.Code != http.StatusOK {
		t.Fatalf("write P into the room's fragment: status=%d body=%s", edited.Code, edited.Body.String())
	}
	p := blockWithText(t, database, deletePWords)
	move(alice, key, p, r1)
	lens := decodeBody[lensInvoked](t, created(t, "invoke L on R1", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/lenses",
		map[string]any{"on": r1, "kind": "trade-off", "verdict": "The read path holds one cache.", "findings": []string{"The cache is warm at boot."}}, "alice")))
	finding := lens.Findings[0].ID
	onLens := decodeBody[model.Comment](t, created(t, "alice's comment on L's text", dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/issues/"+key+"/comments", map[string]any{"body": "Which cache is it?", "anchor": map[string]any{"node": "lens/" + lens.ID, "quote": "one cache"}}, "alice")))
	child := createDigestChild(t, handler, database, brokerTestProject, "The child", deleteQWords+"\n", key, false)
	// The child's node holds no part_of until a move places it, so the tree read answers its
	// placement_seq as the seq of its current child_of; the first PATCH names that tag and is taken,
	// and the same PATCH again is 412, since the move wrote the child's first part_of (R7, R8).
	childNode := graphstore.KindIssue + "/" + child.Key
	childOf := currentRelations(t, database, child.Key, graphstore.TupleChildOf)
	unplaced, listed := treeNodes(t, handler, key, "")[child.Key]
	if len(childOf) != 1 || !listed || unplaced.PlacementSeq == nil || *unplaced.PlacementSeq != childOf[0].Seq {
		t.Fatalf("the tree read of %s answers the unplaced child %s (listed %t) at placement_seq %s, want the seq of its one child_of %+v",
			key, child.Key, listed, placementWords(unplaced.PlacementSeq), childOf)
	}
	childTag := `"` + strconv.FormatInt(childOf[0].Seq, 10) + `"`
	placed := patchNodeAs(alice, key, childNode, childTag, map[string]any{"parent": k})
	if placed.Code != http.StatusOK {
		t.Fatalf("the move of %s under %s with the tag of its child_of answered %d %s, want 200", childNode, k, placed.Code, placed.Body.String())
	}
	childPlacement, _ := placementOf(t, database, graphstore.KindIssue, child.Key)
	if childPlacement.ToID != k || decodeBody[nodeMoved](t, placed).PlacementSeq != childPlacement.Seq {
		t.Errorf("the move of %s answered %s and left it under %s %s at seq %d, want it under %s at the placement_seq it answered",
			childNode, placed.Body.String(), childPlacement.ToKind, childPlacement.ToID, childPlacement.Seq, k)
	}
	refusedWith(t, "the repeat of the move of "+childNode+" with the tag of its child_of", patchNodeAs(alice, key, childNode, childTag, map[string]any{"parent": k}),
		http.StatusPreconditionFailed, codePreconditionFailed)
	if after, _ := placementOf(t, database, graphstore.KindIssue, child.Key); after.Seq != childPlacement.Seq {
		t.Errorf("the refused repeat of the move of %s left it at placement_seq %d, want %d", childNode, after.Seq, childPlacement.Seq)
	}
	q := blockWithText(t, database, deleteQWords)
	move(alice, child.Key, q, r1)
	result := decodeBody[recordedResult](t, created(t, "C3's result", recordResult(t, handler, key, resultBody("c3", "pass", headCommit, nil)))).Result
	move(alice, key, blockWithText(t, database, deleteYWords), r7)
	premortem := decodeBody[lensInvoked](t, created(t, "a pre-mortem on R2", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/lenses",
		map[string]any{"on": r2, "kind": graphstore.LensKindPreMortem}, "alice")))
	risk := decodeBody[struct {
		ID string `json:"id"`
	}](t, created(t, "a risk C1 prevents", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/risks", map[string]any{
		"lens": premortem.Address, "heading": "A read runs past its second.", "early_warning": "The p90 climbs.",
		"prevention": "The check of the second fails the build.", "prevented_by": c1,
	}, "alice")))

	// The first agent's requirements and asks, and its comments on R1's and R2's text.
	if edited := first(http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{"ops": []map[string]any{{
		"op": "insert", "after": "end", "markdown": deleteAgentMarkdown,
	}}}, nil); edited.Code != http.StatusOK {
		t.Fatalf("the first agent writes R4, X4 and R5: status=%d body=%s", edited.Code, edited.Body.String())
	}
	r4, r5, x4 := spec.address("r4"), spec.address("r5"), spec.address("x4")
	openAsk, answeredAsk := askOfBlock(t, database, key, "a4"), askOfBlock(t, database, key, "a5")
	if answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+answeredAsk+"/answer", map[string]any{"text": "The ledger's own store."}, "alice"); answered.Code != http.StatusOK {
		t.Fatalf("alice answers the ask of a5: status=%d body=%s", answered.Code, answered.Body.String())
	}
	move(first, key, x4, r4)
	onR1 := decodeBody[model.Comment](t, created(t, "the agent's comment on R1's text", first(http.MethodPost, "/api/v1/issues/"+key+"/comments",
		map[string]any{"body": "Is one second the bound?", "anchor": map[string]any{"artifact": "spec", "quote": "answers in a second"}}, nil)))
	onR2 := decodeBody[model.Comment](t, created(t, "the agent's comment on R2's text", first(http.MethodPost, "/api/v1/issues/"+key+"/comments",
		map[string]any{"body": "Nothing at all?", "anchor": map[string]any{"artifact": "spec", "quote": "stores nothing"}}, nil)))
	hidden := decodeBody[lensInvoked](t, created(t, "the second agent's author-only lens on R5", second(http.MethodPost, "/api/v1/issues/"+key+"/lenses",
		map[string]any{"on": r5, "kind": "trade-off", "visibility": graphstore.VisibilityAuthor}, nil)))
	message := decodeBody[model.Comment](t, created(t, "alice's message on the issue", dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/issues/"+key+"/messages", map[string]any{"body": "The delete runs next."}, "alice")))
	awaitNoSettlementOwed(t, database)
	awaitBackgroundWrites(t, rig.served)

	r1Paragraph, c1Paragraph := blockWithText(t, database, deleteR1Words), blockWithText(t, database, deleteC1Words)
	r2Paragraph := blockWithText(t, database, deleteR2Words)
	documents := map[string]string{
		"the spec": issue.PrimaryArtifactID, "the Verification document": verification.id, "the room's own fragment": tree.id,
		"the child's room": child.PrimaryArtifactID,
	}

	// The dry run of R1 counts R1, C1, P, L, F and X1: no typed block's own paragraph, which no read
	// returns, and not Q, a node of the child issue the walk of the issue passes over.
	before := readDeleteCounts(t, database)
	if count := deletedCount(t, "the dry run of "+r1, deleteNodeAs(alice, key, r1, true, -1, "")); count != 6 {
		t.Errorf("the dry run of %s answered the count %d, want 6: R1, C1, P, L, F and X1", r1, count)
	}
	requireUnchanged(t, database, "the dry run of "+r1, before)
	_, tag := placementOf(t, database, "block", r1)
	refusedWith(t, "a DELETE of "+r1+" with no If-Match", deleteNodeAs(alice, key, r1, false, 6, ""), http.StatusPreconditionRequired, codePreconditionRequired)
	refusedWith(t, "a DELETE of "+r1+" with a stale tag", deleteNodeAs(alice, key, r1, false, 6, `"1"`), http.StatusPreconditionFailed, codePreconditionFailed)
	refusedWith(t, "a DELETE of "+r1+" with no expected_count", deleteNodeAs(alice, key, r1, false, -1, tag), http.StatusBadRequest, codeDeleteInput)
	requireUnchanged(t, database, "the refused DELETEs of "+r1, before)

	// The delete itself: one version of each document whose text it changes, each with its
	// artifact.version, and the open request on the Verification document moved to its new version.
	request := requestApproval(t, handler, verification.id)
	versions := map[string]int{}
	for name, id := range documents {
		versions[name] = versionCount(t, database, id)
	}
	deleted := deleteNodeAs(alice, key, r1, false, 6, tag)
	if deleted.Code != http.StatusOK {
		t.Fatalf("DELETE of %s answered %d %s, want 200", r1, deleted.Code, deleted.Body.String())
	}
	if count := decodeBody[nodeDeleted](t, deleted).Count; count != 6 {
		t.Errorf("DELETE of %s answered the count %d, want 6", r1, count)
	}
	awaitNoSettlementOwed(t, database)
	for name, id := range documents {
		if now := versionCount(t, database, id); now != versions[name]+1 {
			t.Errorf("DELETE of %s left %s at %d versions, want %d: one version of each document whose text it changes", r1, name, now, versions[name]+1)
		}
		var events int
		if err := database.Pool.QueryRow(ctx, `
			select count(*) from events where type = 'artifact.version' and payload->>'artifact_id' = $1
			  and (payload->'version'->>'number')::int = $2`, id, versions[name]+1).Scan(&events); err != nil {
			t.Fatalf("read the artifact.version of %s: %v", name, err)
		}
		if events != 1 {
			t.Errorf("version %d of %s appended %d artifact.version events, want 1", versions[name]+1, name, events)
		}
	}
	if want, requested := versions["the Verification document"]+1, requestedVersionOf(t, database, request.ID, verification.id); requested != want {
		t.Errorf("the request on the Verification document names version %d after the delete, want %d", requested, want)
	}
	for name, node := range map[string][2]string{
		"R1": {"block", r1}, "R1's own paragraph": {"block", r1Paragraph}, "C1": {"block", c1}, "C1's own paragraph": {"block", c1Paragraph},
		"P": {"block", p}, "Q": {"block", q}, "L": {graphstore.KindLens, lens.ID}, "F": {"block", finding}, "X1": {"block", x1},
	} {
		if state := nodeStateOf(t, database, node[0], node[1]); state != graphstore.StateGone {
			t.Errorf("after DELETE of %s, %s %s is %s, want gone", r1, name, node[1], state)
		}
		if held := relationsOutOf(t, database, node[0], node[1]); len(held) > 0 {
			t.Errorf("after DELETE of %s, %s %s still holds %d tuples out of it: %+v", r1, name, node[1], len(held), held)
		}
	}
	if holdsRelation(t, database, "block", c1, "serves", r2) || holdsRelation(t, database, "block", c1, "serves", r1) {
		t.Errorf("C1's serves tuple to R2 still standing after C1 goes gone")
	}
	if !holdsRelation(t, database, "block", c2, "serves", r1) {
		t.Errorf("C2's serves to R1, a tuple into a gone node from a node that stays, was retracted")
	}
	if !holdsRelation(t, database, graphstore.KindRisk, risk.ID, graphstore.TuplePreventedBy, c1) {
		t.Errorf("the risk %s's prevented_by into C1, a tuple a node that stays holds into a node the delete takes, was retracted", risk.ID)
	}
	if state := nodeStateOf(t, database, "block", c4); state != graphstore.StateLive {
		t.Errorf("C4 is %s after the delete of X1's text, want live: its text lifted out to where X1's stood", state)
	}
	if !holdsRelation(t, database, "block", c4, graphstore.TuplePartOf, r2) {
		t.Errorf("C4 no longer stands under R2 after the delete")
	}
	newest := func(id string) string {
		t.Helper()
		return newestQueryVersion(t, database, id).markdown
	}
	if text := newest(issue.PrimaryArtifactID); strings.Contains(text, deleteR1Words) {
		t.Errorf("the newest spec version still holds R1:\n%s", text)
	}
	if text := newest(verification.id); strings.Contains(text, deleteC1Words) || strings.Contains(text, "callout{#x1") || !strings.Contains(text, deleteC4Words) {
		t.Errorf("the newest Verification version holds C1 or X1, or lost C4: X1, emptied by the lift-out, goes in the same write:\n%s", text)
	}
	if text := newest(tree.id); strings.Contains(text, deletePWords) {
		t.Errorf("the newest version of the room's fragment still holds P:\n%s", text)
	}
	if text := newest(child.PrimaryArtifactID); strings.Contains(text, deleteQWords) {
		t.Errorf("the newest version of the child's room still holds Q:\n%s", text)
	}
	for _, requirement := range readCoverage(t, handler, key).Requirements {
		if requirement.BlockID == "r2" && requirement.Checks != 2 {
			t.Errorf("R2's coverage counts %d checks after the delete, want 2: C2 and C4, and no C1", requirement.Checks)
		}
	}

	// The comments on what the delete took stay live and are orphaned.
	if state := nodeStateOf(t, database, graphstore.KindComment, onR1.ID); state != graphstore.StateLive {
		t.Errorf("the agent's comment on R1's text is %s after the delete, want live", state)
	}
	if holdsRelation(t, database, graphstore.KindComment, onR1.ID, graphstore.TupleAnchoredIn, r1Paragraph) || holdsRelation(t, database, graphstore.KindComment, onR1.ID, graphstore.TuplePartOf, r1Paragraph) {
		t.Errorf("the agent's comment on R1's text keeps its anchored_in or its part_of to R1's paragraph %s", r1Paragraph)
	}
	if holdsRelation(t, database, graphstore.KindComment, onLens.ID, graphstore.TupleAnchoredIn, lens.ID) || holdsRelation(t, database, graphstore.KindComment, onLens.ID, graphstore.TuplePartOf, lens.ID) {
		t.Errorf("alice's comment on L's text keeps its anchored_in or its part_of to L")
	}
	read := commentsWithNodeAnchors(t, handler, key)
	if comment := read[onR1.ID]; comment.Anchor == nil || !comment.Anchor.Orphaned || comment.NodeAnchor == nil ||
		comment.NodeAnchor.Node.ID != r1Paragraph || comment.NodeAnchor.Node.State != graphstore.StateGone || comment.NodeAnchor.Quote != "answers in a second" {
		t.Errorf("the agent's comment on R1's text reads %+v, want orphaned with its quote and a node_anchor naming %s gone", comment, r1Paragraph)
	}
	if comment := read[onLens.ID]; comment.NodeAnchor == nil || !comment.NodeAnchor.Orphaned || comment.NodeAnchor.Quote != "one cache" ||
		comment.NodeAnchor.Node.State != graphstore.StateGone {
		t.Errorf("alice's comment on L's text reads %+v, want its node anchor on L orphaned, its quote kept, and L gone", comment)
	}
	if anchor := treeNodes(t, handler, key, "")[onLens.ID].Anchor; anchor == nil || anchor.Node == nil || anchor.Node.State != graphstore.StateGone {
		t.Errorf("the tree read answers alice's comment on L with the anchor %+v, want L in the state gone", anchor)
	}

	// An edit that drops the quote of the agent's comment on R2 orphans it with R2 live, so
	// orphaned alone tells no delete; the same edit's index pass leaves R1 gone.
	if edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{"ops": []map[string]any{{
		"op": "delete", "find": "stores nothing",
	}}}, "alice"); edited.Code != http.StatusOK {
		t.Fatalf("alice edits R2's words: status=%d body=%s", edited.Code, edited.Body.String())
	}
	awaitNoSettlementOwed(t, database)
	if comment := commentsWithNodeAnchors(t, handler, key)[onR2.ID]; comment.Anchor == nil || !comment.Anchor.Orphaned ||
		comment.NodeAnchor == nil || comment.NodeAnchor.Node.ID != r2Paragraph || comment.NodeAnchor.Node.State != graphstore.StateLive {
		t.Errorf("the agent's comment on R2's text reads %+v after R2's edit, want orphaned with its paragraph %s live", comment, r2Paragraph)
	}
	if state := nodeStateOf(t, database, "block", r1); state != graphstore.StateGone {
		t.Errorf("R1 reads %s again after the next index pass of the spec, want gone", state)
	}
	if edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+child.PrimaryArtifactID+"/edits", map[string]any{"ops": []map[string]any{{
		"op": "insert", "after": "end", "markdown": "The child's second paragraph.\n",
	}}}, "alice"); edited.Code != http.StatusOK {
		t.Fatalf("alice edits the child's room: status=%d body=%s", edited.Code, edited.Body.String())
	}
	awaitNoSettlementOwed(t, database)
	if state := nodeStateOf(t, database, "block", q); state != graphstore.StateGone {
		t.Errorf("Q reads %s again after the child's room's next index pass, want gone", state)
	}
	census, err := graphcensus.StoreCensus(ctx, database.Pool.Config().ConnString(), false, docs.ReturnDeclined)
	if err != nil {
		t.Fatalf("take the store census: %v", err)
	}
	for _, kind := range census.Kinds {
		if kind.Kind == "comment_anchor_block" && kind.RowDifferences+kind.NodeDifferences != 0 {
			t.Errorf("the store census reads %d differences of comment_anchor_block after the delete, want 0", kind.RowDifferences+kind.NodeDifferences)
		}
	}
	if _, held := treeNodes(t, handler, key, approvedView)[r1]; !held {
		t.Errorf("the spec query at the approved version %d no longer answers R1", approved.Version)
	}
	if newestQuery := newestQueryVersion(t, database, specQuery); newestQuery.number <= approved.Version {
		t.Errorf("the spec query stands at version %d after the delete, want a version after the approved %d", newestQuery.number, approved.Version)
	}

	// A count that moved since the dry run is refused with the count now, while R2's If-Match holds.
	counted := deletedCount(t, "the dry run of "+r2, deleteNodeAs(alice, key, r2, true, -1, ""))
	_, r2Tag := placementOf(t, database, "block", r2)
	if edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+tree.id+"/edits", map[string]any{"ops": []map[string]any{{
		"op": "insert", "after": "end", "markdown": "A prose block that comes to stand under R2.\n",
	}}}, "alice"); edited.Code != http.StatusOK {
		t.Fatalf("write a prose block into the room's fragment: status=%d body=%s", edited.Code, edited.Body.String())
	}
	move(alice, key, blockWithText(t, database, "A prose block that comes to stand under R2."), r2)
	before = readDeleteCounts(t, database)
	stale := deleteNodeAs(alice, key, r2, false, counted, r2Tag)
	if stale.Code != http.StatusConflict {
		t.Errorf("DELETE of %s with a stale expected_count answered %d, not 409: %s", r2, stale.Code, stale.Body.String())
	} else if moved := decodeBody[deleteCountMoved](t, stale); moved.Code != codeDeleteCountMoved ||
		moved.Count != deletedCount(t, "a fresh dry run of "+r2, deleteNodeAs(alice, key, r2, true, -1, "")) || moved.Count == counted {
		t.Errorf("DELETE of %s with a stale expected_count answered %+v, want DELETE_COUNT_MOVED with the count a fresh dry run answers", r2, moved)
	}
	requireUnchanged(t, database, "the DELETE of "+r2+" with a stale expected_count", before)

	// K goes, and the child issue keeps its node, its child_of and its part_of to K, hung from its
	// parent issue by the walk.
	_, kTag := placementOf(t, database, "block", k)
	kCount := deletedCount(t, "the dry run of "+k, deleteNodeAs(alice, key, k, true, -1, ""))
	deletedCount(t, "DELETE of "+k, deleteNodeAs(alice, key, k, false, kCount, kTag))
	if !holdsRelation(t, database, graphstore.KindIssue, child.Key, graphstore.TuplePartOf, k) || !holdsRelation(t, database, graphstore.KindIssue, child.Key, graphstore.TupleChildOf, key) {
		t.Errorf("the child issue %s lost its part_of to %s or its child_of to %s after the delete of %s", child.Key, k, key, k)
	}
	if node, held := treeNodes(t, handler, key, "")[child.Key]; !held || node.Parent == nil || node.Parent.ID != key {
		t.Errorf("the tree read of %s answers the child %s at %+v, want under the issue", key, child.Key, node.Parent)
	}

	// C3 holds a result, retracted and never deleted: the dry run and the delete refuse it alike.
	before = readDeleteCounts(t, database)
	_, c3Tag := placementOf(t, database, "block", c3)
	for _, dry := range []bool{true, false} {
		what := "DELETE of " + c3
		if dry {
			what = "dry run of " + c3
		}
		response := deleteNodeAs(alice, key, c3, dry, 1, c3Tag)
		if response.Code == http.StatusOK {
			t.Errorf("%s answered 200, not 409: the result %s goes with it", what, result.ID)
			continue
		}
		if message := refusedWith(t, what, response, http.StatusConflict, codeNodeHoldsResult); !strings.Contains(message, "c3") || !strings.Contains(message, result.ID) {
			t.Errorf("%s does not name C3 and its result: %s", what, message)
		}
	}
	requireUnchanged(t, database, "the refused deletes of "+c3, before)

	// The agent deletes R4 and X4 and leaves both asks' blocks in the spec, where X4's text stood.
	specVersions := versionCount(t, database, issue.PrimaryArtifactID)
	r4Count := deletedCount(t, "the agent's dry run of "+r4, deleteNodeAs(first, key, r4, true, -1, ""))
	if r4Count != 2 {
		t.Errorf("the agent's dry run of %s answered %d, want 2: R4 and X4, and neither ask's block", r4, r4Count)
	}
	_, r4Tag := placementOf(t, database, "block", r4)
	if response := deleteNodeAs(first, key, r4, false, r4Count, r4Tag); response.Code != http.StatusOK {
		t.Errorf("the agent's DELETE of %s answered %d %s, want 200: the ask blocks lift out of X4", r4, response.Code, response.Body.String())
	} else if count := decodeBody[nodeDeleted](t, response).Count; count != r4Count {
		t.Errorf("the agent's DELETE of %s answered the count %d, want %d", r4, count, r4Count)
	}
	awaitNoSettlementOwed(t, database)
	if now := versionCount(t, database, issue.PrimaryArtifactID); now != specVersions+1 {
		t.Errorf("the DELETE of %s left the spec at %d versions, want %d", r4, now, specVersions+1)
	}
	if text := newest(issue.PrimaryArtifactID); strings.Contains(text, "callout{#x4") || !strings.Contains(text, "Which writer signs a delete?") || !strings.Contains(text, "Which store holds the ledger?") {
		t.Errorf("the spec after the delete of %s holds X4 or lost an ask's block:\n%s", r4, text)
	}
	for _, ask := range []struct{ id, state string }{{openAsk, "open"}, {answeredAsk, "answered"}} {
		var state string
		var answer *string
		if err := database.Pool.QueryRow(ctx, `select state, answer->>'text' from asks where id = $1::uuid`, ask.id).Scan(&state, &answer); err != nil {
			t.Fatalf("read the ask %s: %v", ask.id, err)
		}
		if state != ask.state || ask.state == "answered" && (answer == nil || *answer != "The ledger's own store.") {
			t.Errorf("the ask %s reads %s with the answer %v after the delete, want %s with its answer", ask.id, state, answer, ask.state)
		}
	}

	// R6 holds alice's decision: the agent's dry run and its delete refuse it, quoting the decision;
	// a lens alice puts on R6 does not move that refusal behind the state route's.
	created(t, "alice's lens on R6", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/lenses",
		map[string]any{"on": r6, "kind": "trade-off"}, "alice"))
	before = readDeleteCounts(t, database)
	_, r6Tag := placementOf(t, database, "block", r6)
	for _, dry := range []bool{true, false} {
		response := deleteNodeAs(first, key, r6, dry, 2, r6Tag)
		if response.Code == http.StatusOK {
			t.Errorf("agent DELETE of %s answered 200, not 409: it took the decision %s", r6, spec.address("d6"))
			continue
		}
		if message := refusedWith(t, "the agent's delete of "+r6, response, http.StatusConflict, codeAnsweredAskKept); !strings.Contains(message, deleteD6Words) || !strings.Contains(message, "d6") {
			t.Errorf("the refusal of the agent's delete of %s does not quote and name the decision: %s", r6, message)
		}
	}
	requireUnchanged(t, database, "the agent's refused deletes of "+r6, before)

	// A node the delete never takes: an ask, a comment, a message, a result, the issue node and a
	// document, refused on the dry run and on the delete alike.
	before = readDeleteCounts(t, database)
	for _, address := range []string{
		graphstore.KindAsk + "/" + openAsk, graphstore.KindComment + "/" + onR2.ID, graphstore.KindMessage + "/" + message.ID,
		graphstore.KindResult + "/" + result.ID, graphstore.KindIssue + "/" + key, graphstore.KindArtifact + "/" + spec.refKey,
	} {
		for _, dry := range []bool{true, false} {
			refusedWith(t, fmt.Sprintf("the delete (dry run %t) of %s", dry, address), deleteNodeAs(alice, key, address, dry, 1, `"1"`), http.StatusBadRequest, codeNodeNotDeletable)
		}
	}
	requireUnchanged(t, database, "the deletes of nodes the delete never takes", before)

	// A text the delete would leave holding Y with no block of its own: refused on the dry run, read
	// off the document's tree, and on the delete alike.
	before = readDeleteCounts(t, database)
	_, r7Tag := placementOf(t, database, "block", r7)
	for _, dry := range []bool{true, false} {
		if message := refusedWith(t, fmt.Sprintf("the delete (dry run %t) of %s", dry, r7), deleteNodeAs(alice, key, r7, dry, 1, r7Tag),
			http.StatusConflict, codeContainerWouldEmpty); !strings.Contains(message, "callout") || !strings.Contains(message, `"y"`) {
			t.Errorf("the refusal of the delete of %s does not name the callout Y: %s", r7, message)
		}
	}
	requireUnchanged(t, database, "the refused deletes of "+r7, before)

	// R5 holds the second agent's author-only lens: alice counts R5 alone, and the first agent is
	// refused the state route's way, naming no hidden node.
	before = readDeleteCounts(t, database)
	if count := deletedCount(t, "alice's dry run of "+r5, deleteNodeAs(alice, key, r5, true, -1, "")); count != 1 {
		t.Errorf("alice's dry run of %s answered %d, want 1: R5, and not the lens she may not see", r5, count)
	}
	_, r5Tag := placementOf(t, database, "block", r5)
	for _, dry := range []bool{true, false} {
		response := deleteNodeAs(first, key, r5, dry, 1, r5Tag)
		refusedWith(t, fmt.Sprintf("the first agent's delete (dry run %t) of %s", dry, r5), response, http.StatusForbidden, codeStateNotPermitted)
		if strings.Contains(response.Body.String(), hidden.ID) {
			t.Errorf("the first agent's delete of %s names the hidden lens %s: %s", r5, hidden.ID, response.Body.String())
		}
	}

	// The node routes find the hidden lens for its author alone.
	hiddenAddress := graphstore.KindLens + "/" + hidden.ID
	_, hiddenTag := placementOf(t, database, graphstore.KindLens, hidden.ID)
	notFound := fmt.Sprintf("no node %q under %s", hiddenAddress, key)
	for what, response := range map[string]*httptest.ResponseRecorder{
		"agent dry run":  deleteNodeAs(first, key, hiddenAddress, true, -1, ""),
		"agent DELETE":   deleteNodeAs(first, key, hiddenAddress, false, 1, hiddenTag),
		"agent PATCH of": patchNodeAs(first, key, hiddenAddress, hiddenTag, map[string]any{"parent": r2}),
	} {
		if refusal := decodeBody[refusalAnswer](t, response); response.Code != http.StatusNotFound || refusal.Code != "NOT_FOUND" || !strings.Contains(refusal.Error, notFound) {
			t.Errorf("%s %s answered %d %s, want 404 NOT_FOUND with the words %q: %s", what, hiddenAddress, response.Code, refusal.Code, notFound, refusal.Error)
		}
	}
	refusedWith(t, "the hidden lens's author's dry run", deleteNodeAs(second, key, hiddenAddress, true, -1, ""), http.StatusForbidden, codeStateNotPermitted)
	requireUnchanged(t, database, "the refused deletes of "+r5+" and of the hidden lens", before)

	// An issue the first agent may not see, its node reserved for alice, who wrote it: the agent's
	// delete of C3's result is 404 NOT_FOUND, as its delete of the issue's node is and as any node
	// it may not see under the issue is, and alice's of either is still the 400 of a node the
	// delete never takes.
	reserveIssue := func(visibility string) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, `update graph_nodes set visibility = $3 where kind = $1 and id = $2`,
			graphstore.KindIssue, key, visibility); err != nil {
			t.Fatalf("set the visibility of %s's node to %s: %v", key, visibility, err)
		}
	}
	reserveIssue(graphstore.VisibilityAuthor)
	before = readDeleteCounts(t, database)
	for _, address := range []string{graphstore.KindResult + "/" + result.ID, graphstore.KindIssue + "/" + key} {
		notFound := fmt.Sprintf("no node %q under %s", address, key)
		for _, dry := range []bool{true, false} {
			what := fmt.Sprintf("the first agent's delete (dry run %t) of %s under the issue it may not see", dry, address)
			if message := refusedWith(t, what, deleteNodeAs(first, key, address, dry, 1, `"1"`), http.StatusNotFound, "NOT_FOUND"); !strings.Contains(message, notFound) {
				t.Errorf("%s does not answer the words %q: %s", what, notFound, message)
			}
			refusedWith(t, fmt.Sprintf("alice's delete (dry run %t) of %s under the issue reserved for her", dry, address),
				deleteNodeAs(alice, key, address, dry, 1, `"1"`), http.StatusBadRequest, codeNodeNotDeletable)
		}
	}
	requireUnchanged(t, database, "the deletes under the issue the first agent may not see", before)
	reserveIssue(graphstore.VisibilityProject)

	t.Run("writer", func(t *testing.T) {
		t.Parallel()
		writers := newNodeWriterRig(t)
		requirement := writers.spec.address("r1")
		_, writerTag := placementOf(t, writers.database, "block", requirement)
		worker := writers.caller(t, writers.worker)
		counts := readDeleteCounts(t, writers.database)
		for _, dry := range []bool{true, false} {
			refusedWith(t, fmt.Sprintf("the worker's delete (dry run %t) of %s", dry, requirement),
				deleteNodeAs(worker, writers.issue, requirement, dry, 1, writerTag), http.StatusForbidden, specWriterRefusalCode)
		}
		requireUnchanged(t, writers.database, "the worker's refused deletes", counts)
	})

	t.Run("contended", func(t *testing.T) {
		t.Parallel()
		requireTheDeleteRetriesOnceAndIsContended(t)
	})
}

// refsKindOf and storeIDOf are the store's kind and id of the node a store address names, as a move
// of it names its placement (placementOf).
func refsKindOf(address string) string {
	kind, _, _ := decodeNodeAddress(address)
	return storedEnd{kind: kind}.stored().kind
}

func storeIDOf(address string) string {
	_, id, _ := decodeNodeAddress(address)
	return id
}

// requireTheDeleteRetriesOnceAndIsContended holds the delete to the two-attempt owner-lock rule (R7):
// on an issue whose requirement T is to be deleted, and two child issues each with a paragraph of
// its room, the test hook (server.betweenNodeRouteReads) pauses the delete after each read it makes
// with no lock held, and alice moves one child's paragraph under T there, through PATCH. Each read
// under the locks then finds a node under an issue the attempt did not lock: the first attempt gives
// its locks back and the second reads again, and the delete answers 409 PARENT_CONTENDED, not 409
// DELETE_COUNT_MOVED, changing nothing the two moves left.
func requireTheDeleteRetriesOnceAndIsContended(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	createReferenceAPIProject(t, handler, "DELRACE")
	issue := createDigestIssue(t, handler, database, "DELRACE", "Contended", ":::requirement{#t1}\nThe target of the contended delete.\n:::\n", false)
	firstChild := createDigestChild(t, handler, database, "DELRACE", "First child", "The first child's paragraph moves under the target.\n", issue.Key, false)
	secondChild := createDigestChild(t, handler, database, "DELRACE", "Second child", "The second child's paragraph moves under the target.\n", issue.Key, false)
	target := blockAddressOf(t, database, issue.Key, "t1")
	moves := []struct{ key, block string }{
		{firstChild.Key, blockWithText(t, database, "The first child's paragraph")},
		{secondChild.Key, blockWithText(t, database, "The second child's paragraph")},
	}
	awaitNoSettlementOwed(t, database)
	alice := personCaller(t, handler, "alice")
	expected := deletedCount(t, "the dry run of "+target, deleteNodeAs(alice, issue.Key, target, true, -1, ""))
	_, tag := placementOf(t, database, "block", target)
	attempts := 0
	moving := false
	var afterMoves deleteCounts
	served.betweenNodeRouteReads = func(_ context.Context, attempt int) {
		if moving || attempt > len(moves) {
			return
		}
		attempts, moving = attempt, true
		defer func() { moving = false }()
		moved := moves[attempt-1]
		_, blockTag := placementOf(t, database, "block", moved.block)
		if response := patchNode(t, handler, moved.key, moved.block, blockTag, map[string]any{"parent": target}, "alice"); response.Code != http.StatusOK {
			t.Errorf("alice's move of %s under %s answered %d %s, want 200", moved.block, target, response.Code, response.Body.String())
		}
		if attempt == len(moves) {
			awaitNoSettlementOwed(t, database)
			afterMoves = readDeleteCounts(t, database)
		}
	}
	response := deleteNodeAs(alice, issue.Key, target, false, expected, tag)
	served.betweenNodeRouteReads = nil
	if attempts != 2 {
		t.Errorf("DELETE stopped after attempt %d, want 2", attempts)
	}
	refusedWith(t, "the contended DELETE of "+target, response, http.StatusConflict, parentContended)
	awaitNoSettlementOwed(t, database)
	if now := readDeleteCounts(t, database); now != afterMoves {
		t.Errorf("the contended DELETE of %s changed the store after the two moves: %+v, want %+v", target, now, afterMoves)
	}
	if state := nodeStateOf(t, database, "block", target); state != graphstore.StateLive {
		t.Errorf("the contended DELETE of %s left it %s, want live", target, state)
	}
}

// --- the delete's lock steps (TestEveryApiStoreWriterTakesTheStoreLockOrder) ------------------

// nodeDelete is a delete of a lens with one finding that alice invoked on the rig's spec
// (deleteNodeRoute, queueNodeDelete), through its route: the owner lock of the rig's issue, the one
// issue every node of the delete stands under (lockIssueOwners); then, in its write phase, the node
// rows of the finding, a block node, and of the lens, in ascending (kind, id), each written gone as
// the state route writes it (graphstore.PlanState); the state tuple of each, at the sentinel, which
// the node move appends, with the retraction of every tuple out of each after it; and the mark of
// the issue's queries owed a version (queryMarkStep). The lens belongs to no document, so the delete
// writes no version and takes no room's lock. Each run deletes a lens of its own, invoked just
// before it, with the count its dry run answers. The rows stand before the run, so each is held
// `for update`, which the delete's node write waits on; the tuples, at the sentinel, are held as the
// writes of graph_tuples, and the mark as its table.
func (r *storeWriterRig) nodeDelete() storeWriter {
	var lens, finding string
	return storeWriter{
		name: "deleteNodeRoute",
		steps: []storeStep{
			ownerLockStep(store.IssueOwner(r.issue)),
			{
				name: "the finding's node row", order: nodeStep,
				hold: holdNodeRow(refs.BlockNodeKind, &finding), at: nodeWriteStatement,
				takes: []tableLock{{"graph_nodes", rowExclusiveMode}},
			},
			{
				name: "the lens's node row", order: nodeStep,
				hold: holdNodeRow(graphstore.KindLens, &lens), at: nodeWriteStatement,
				takes: []tableLock{{"graph_nodes", rowExclusiveMode}},
			},
			{
				name: "the state tuples and the retractions", order: tupleStep,
				hold: holdTableWrites("graph_tuples"), at: keptCauseMoveStatement,
				takes: []tableLock{{"graph_tuples", rowExclusiveMode}},
			},
			queryMarkStep(),
		},
		ready: func(t *testing.T, ctx context.Context) storeWriterRun {
			invoked := decodeBody[lensInvoked](t, created(t, "invoke a lens with a finding on the spec of "+r.issue,
				dispatchRequest(t, r.handler, http.MethodPost, "/api/v1/issues/"+r.issue+"/lenses", map[string]any{
					"on": "dispatch://" + r.issue + "/spec", "kind": "trade-off",
					"findings": []string{r.fresh("A finding the delete takes gone with its lens")},
				}, "alice")))
			if len(invoked.Findings) != 1 {
				t.Fatalf("the lens on the spec of %s holds %d findings, want the one it was invoked with", r.issue, len(invoked.Findings))
			}
			lens, finding = invoked.ID, invoked.Findings[0].ID
			address := graphstore.KindLens + "/" + lens
			alice := personCaller(t, r.handler, "alice")
			count := deletedCount(t, "the dry run of "+address, deleteNodeAs(alice, r.issue, address, true, -1, ""))
			_, tag := placementOf(t, r.database, graphstore.KindLens, lens)
			call := personCaller(t, r.call, "alice")
			return storeWriterRun{write: func() error {
				return statusOrError(deleteNodeAs(call, r.issue, address, false, count, tag), http.StatusOK)
			}}
		},
	}
}
