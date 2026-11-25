package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// treeAnswer is one tree read: its answer, and its body as the server wrote it, for a search of
// what the answer holds anywhere.
type treeAnswer struct {
	model.IssueTreeRead
	body string
}

// treeRead reads GET /api/v1/issues/{key}/tree with query as login, as the dashboard reads it
// (`Dispatch-Trees: 1`) where trees is true, and fails on anything but 200.
func treeRead(t *testing.T, handler http.Handler, key, query, login string, trees bool) treeAnswer {
	t.Helper()
	response := treeResponse(t, handler, key, query, login, trees)
	if response.Code != http.StatusOK {
		t.Fatalf("%s reads the tree of %s%s: status=%d body=%s", login, key, query, response.Code, response.Body.String())
	}
	return treeAnswer{IssueTreeRead: decodeBody[model.IssueTreeRead](t, response), body: response.Body.String()}
}

// treeResponse is GET /api/v1/issues/{key}/tree with query as login, `Dispatch-Trees: 1` where trees.
func treeResponse(t *testing.T, handler http.Handler, key, query, login string, trees bool) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/issues/"+key+"/tree"+query, nil)
	request.Header.Set("X-Dispatch-User", login)
	if trees {
		request.Header.Set(displayTreesHeader, "1")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// nodesNamed is every node of read whose kind and id are those given, in its order.
func (read treeAnswer) nodesNamed(kind, id string) []model.IssueTreeNode {
	var nodes []model.IssueTreeNode
	for _, node := range read.Nodes {
		if node.Kind == kind && node.ID == id {
			nodes = append(nodes, node)
		}
	}
	return nodes
}

// node is the one node of read of kind and id, and its index; it fails unless read holds exactly
// one.
func (read treeAnswer) node(t *testing.T, kind, id string) (model.IssueTreeNode, int) {
	t.Helper()
	held := read.nodesNamed(kind, id)
	if len(held) != 1 {
		t.Fatalf("the tree read of %s holds %d nodes %s:%s, want exactly one; it holds %v", read.Issue, len(held), kind, id, read.addresses())
	}
	return held[0], read.index(kind, id)
}

// index is where the node of kind and id stands in read, -1 where it holds none.
func (read treeAnswer) index(kind, id string) int {
	return slices.IndexFunc(read.Nodes, func(node model.IssueTreeNode) bool { return node.Kind == kind && node.ID == id })
}

// addresses is each node's `<kind>:<id>`, in order.
func (read treeAnswer) addresses() []string {
	addresses := make([]string, len(read.Nodes))
	for index, node := range read.Nodes {
		addresses[index] = node.Kind + ":" + node.ID
	}
	return addresses
}

// holdsTuple reports whether read answers the tuple of kind from one node to the other.
func (read treeAnswer) holdsTuple(kind string, from, to model.IssueTreeEnd) bool {
	return slices.ContainsFunc(read.Tuples, func(tuple model.IssueTreeTuple) bool {
		return tuple.Kind == kind && tuple.From == from && tuple.To == to
	})
}

// supertagged is every node of read whose supertag is supertag.
func (read treeAnswer) supertagged(supertag string) []model.IssueTreeNode {
	var nodes []model.IssueTreeNode
	for _, node := range read.Nodes {
		if node.Supertag == supertag {
			nodes = append(nodes, node)
		}
	}
	return nodes
}

// firstBlockID is the id of the first current block of comment id.
func firstBlockID(t *testing.T, database *store.Store, id string) string {
	t.Helper()
	var block string
	if err := database.Pool.QueryRow(context.Background(),
		`select block_id from comment_blocks where comment_id = $1::uuid and position is not null order by position limit 1`, id).Scan(&block); err != nil {
		t.Fatalf("read the first block of comment %s: %v", id, err)
	}
	return block
}

// treeEnd names a node as a tuple's end names it.
func treeEnd(kind, id string) model.IssueTreeEnd { return model.IssueTreeEnd{Kind: kind, ID: id} }

// TestTheTreeReadAnswersACriterionWithItsAuthorAndItsChain is c-criterion-page. An issue born
// converted holds a person's comment of two blocks, a message and an open ask; a person writes a
// criterion in the issue's room under the comment's first block. The tree read answers one
// criterion node whose text is the text the room holds for it and whose author is that person;
// its authored_by tuple names the person, and no attribute of the node holds the person, which a
// planted copy of the attributes shows the search finds; following the parents the answer gives
// from the criterion reaches the comment's block, the comment and its words, and the issue; and
// ?view=spec answers the criterion as the query's, with its parent's id, supertag and first line
// naming the comment's first block.
func TestTheTreeReadAnswersACriterionWithItsAuthorAndItsChain(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "CORE")
	issue := createDigestIssue(t, handler, database, "CORE", "A criterion's chain", ":::requirement{#r1}\nAn urgent ask comes first.\n:::\n", false)
	const commentBody = "We want the urgent asks first.\n\nAnd we want them quickly."
	comment, blockRef := personComment(t, handler, database, issue.Key, commentBody)
	block := refs.ItemComment.BlockKind()
	blockNode := string(refs.ItemComment) + "/" + comment + "#" + firstBlockID(t, database, comment)
	created(t, "a person's message", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages",
		map[string]any{"body": "The list must hold every urgent ask."}, "alice"))
	createIssueAsk(t, handler, issue.Key, "Does the list hold answered asks?")
	const words = "I want the urgent asks first."
	editSpec(t, handler, issue.Key, []map[string]any{{"op": "insert", "after": "end",
		"markdown": ":::criterion{#k1 parent=\"" + blockRef + "\"}\n" + words + "\n:::\n"}})
	spec := readDecisionDocument(t, database, issue.Key, "spec")
	criterionNode := spec.address("k1")

	read := treeRead(t, handler, issue.Key, "", "alice", false)
	criteria := read.supertagged("criterion")
	if len(criteria) != 1 || criteria[0].ID != criterionNode {
		t.Fatalf("the tree read answers the criteria %+v, want the one node %s", criteria, criterionNode)
	}
	criterion := criteria[0]
	markdown := artifactTextOf(t, handler, spec.id)
	var held string
	for text, id := range blockIDsByText(t, handler, spec.id) {
		if id == "k1" {
			held = text
		}
	}
	if held == "" || criterion.Text != held || !strings.Contains(criterion.Text, words) {
		t.Errorf("the criterion's text is %q, want the text its room holds for it, %q, holding the person's words (the spec reads %q)", criterion.Text, held, markdown)
	}
	if len(criterion.Author) != 1 || criterion.Author[0].Kind != "user" || criterion.Author[0].ID != "alice" {
		t.Errorf("the criterion's author is %+v, want alice, who wrote it", criterion.Author)
	}
	authored, err := graphstore.CurrentOut(ctx, database.Pool, refs.BlockNodeKind, criterionNode, []string{graphstore.TupleAuthoredBy})
	if err != nil {
		t.Fatalf("read the authored_by of %s: %v", criterionNode, err)
	}
	if len(authored) != 1 || authored[0].ToKind != graphstore.KindPerson || authored[0].ToID != "alice" {
		t.Errorf("the criterion %s holds the authored_by tuples %+v, want one to the person alice", criterionNode, authored)
	}
	personValue := func(attributes map[string]string) []string {
		var found []string
		for name, value := range attributes {
			if value == "alice" || value == "user:alice" || value == "person:alice" {
				found = append(found, name)
			}
		}
		return found
	}
	if found := personValue(criterion.Attributes); len(found) > 0 {
		t.Errorf("the criterion %s holds the person in the attributes %v, want the author in its authored_by tuple alone", criterionNode, found)
	}
	planted := map[string]string{"author": "alice"}
	for name, value := range criterion.Attributes {
		planted[name] = value
	}
	if found := personValue(planted); len(found) != 1 {
		t.Fatalf("the search finds %v in a copy of the attributes holding the planted author, want it found, so an empty answer is the node's", found)
	}

	if criterion.Parent == nil || criterion.Parent.Kind != block || criterion.Parent.ID != blockNode {
		t.Fatalf("the criterion stands under %+v, want the comment's first block %s:%s", criterion.Parent, block, blockNode)
	}
	var chain []string
	at := criterion
	for steps := 0; at.Parent != nil && steps < len(read.Nodes); steps++ {
		parent, index := read.node(t, at.Parent.Kind, at.Parent.ID)
		if index >= read.index(at.Kind, at.ID) {
			t.Errorf("the tree read answers %s:%s at %d, after its child %s:%s, want each parent first", parent.Kind, parent.ID, index, at.Kind, at.ID)
		}
		chain = append(chain, parent.Kind+":"+parent.ID)
		at = parent
	}
	want := []string{block + ":" + blockNode, graphstore.KindComment + ":" + comment, graphstore.KindIssue + ":" + issue.Key}
	if !slices.Equal(chain, want) {
		t.Fatalf("the criterion's chain of parents is %v, want %v", chain, want)
	}
	if said, _ := read.node(t, graphstore.KindComment, comment); said.Text != commentBody {
		t.Errorf("the comment the chain reaches says %q, want its words %q", said.Text, commentBody)
	}

	viewed := treeRead(t, handler, issue.Key, "?view=spec", "alice", false)
	inSpec, _ := viewed.node(t, refs.BlockNodeKind, criterionNode)
	if inSpec.Role != treeRoleQuery {
		t.Errorf("?view=spec answers the criterion as %q, want the query's", inSpec.Role)
	}
	if inSpec.Parent == nil || inSpec.Parent.Kind != block || inSpec.Parent.ID != blockNode ||
		inSpec.Parent.Supertag != block || inSpec.Parent.FirstLine != "We want the urgent asks first." {
		t.Errorf("?view=spec names the criterion's parent %+v, want the comment's first block %s with its supertag and first line", inSpec.Parent, blockNode)
	}
}

// TestTheTreeReadAnswersARunThatChangedANodeOnceAsItsAuthorsContext: a comment alice writes under
// her run of the command bar holds a changed_by_run tuple to the run's interaction. Her tree read
// answers that interaction once, as context, with the comment's changed_by_run tuple, and the
// issue's node holds no changed_by_run of its own for that run; bob's read answers the comment and
// neither the interaction nor any tuple naming it.
func TestTheTreeReadAnswersARunThatChangedANodeOnceAsItsAuthorsContext(t *testing.T) {
	handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{})
	ctx := context.Background()
	issue := createTestIssue(t, handler, "TRUN", "The issue a run comments on")
	run, err := directServer(deps).recordGoingRequest(ctx, model.Actor{Kind: "user", ID: "alice"}, "", "Which plan holds the rollback step?")
	if err != nil {
		t.Fatalf("record alice's run: %v", err)
	}
	runWindowAhead(t, database, run.ID)
	written := requestForRun(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/comments",
		map[string]any{"body": "The rollback step is in the plan."}, "alice", run.ID)
	if written.Code != http.StatusCreated {
		t.Fatalf("write the comment under the run: status=%d body=%s", written.Code, written.Body.String())
	}
	comment := decodeBody[writtenRecord](t, written).ID
	if count := changedByRunCount(t, database, graphstore.KindComment, comment, run.ID); count != 1 {
		t.Fatalf("the comment holds %d changed_by_run tuples to the run, want the one its write appended", count)
	}

	alice := treeRead(t, handler, issue, "", "alice", false)
	interaction, at := alice.node(t, graphstore.KindInteraction, run.ID)
	if interaction.Role != treeRoleContext {
		t.Errorf("alice's read answers the run as %q, want context", interaction.Role)
	}
	if _, commented := alice.node(t, graphstore.KindComment, comment); commented > at {
		t.Errorf("alice's read answers the run at %d before the comment at %d, want the context after the tree", at, commented)
	}
	runEnd := treeEnd(graphstore.KindInteraction, run.ID)
	if !alice.holdsTuple(graphstore.TupleChangedByRun, treeEnd(graphstore.KindComment, comment), runEnd) {
		t.Errorf("alice's read holds the tuples %+v, want the comment's changed_by_run to the run", alice.Tuples)
	}
	if alice.holdsTuple(graphstore.TupleChangedByRun, treeEnd(graphstore.KindIssue, issue), runEnd) {
		t.Errorf("alice's read answers a changed_by_run from the issue's node to the run")
	}
	if count := changedByRunCount(t, database, graphstore.KindIssue, issue, run.ID); count != 0 {
		t.Errorf("the issue's node holds %d changed_by_run tuples to the run, want none: the read derives it from the comment's own", count)
	}

	bob := treeRead(t, handler, issue, "", "bob", false)
	bob.node(t, graphstore.KindComment, comment)
	if held := bob.nodesNamed(graphstore.KindInteraction, run.ID); len(held) > 0 || strings.Contains(bob.body, run.ID) {
		t.Errorf("bob's read answers alice's run: nodes %+v, or its id in the body %s", held, bob.body)
	}
}

// TestTheTreeReadAnswersTheCatalogueNodesItsTuplesNameAsContext: a pre-mortem lens on a requirement
// and the requirement's constrained_by to a constraint of the catalogue. The tree read with no view
// answers the lens as a node of the tree and, as context, the pre-mortem lens_kind and that
// constraint, each with its has_kind or constrained_by tuple; the pre-mortem kind's own source
// constraint, which only that context node names, is answered nowhere, and neither is a constraint
// nothing the tree holds names.
func TestTheTreeReadAnswersTheCatalogueNodesItsTuplesNameAsContext(t *testing.T) {
	rig := newReasoningRig(t)
	ctx := context.Background()
	lens := rig.invoke(rig.requirement, graphstore.LensKindPreMortem, "alice", nil)
	addKind := func(id string) string {
		tx, err := rig.database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		source, err := graphstore.AddLensKind(ctx, tx, graphstore.LensKind{
			ID: id, Words: "The kind " + id + ".", Source: "The standard of " + id, Reference: "section 1",
		}, time.Now().UTC())
		if err != nil {
			t.Fatalf("add the lens kind %s: %v", id, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit the lens kind %s: %v", id, err)
		}
		return source
	}
	named, unnamed := addKind("tree-read-named"), addKind("tree-read-unnamed")
	succeeded(t, "the requirement's constrained_by to a constraint of the catalogue", rig.post(nodePath(rig.requirementNode, "pointers"),
		map[string]any{"kind": graphstore.TupleConstrainedBy, "action": pointerAppend, "to": "dispatch://constraint/" + named}, "alice"))
	catalogue, err := refs.LensCatalogue(ctx, rig.database.Pool, model.Actor{})
	if err != nil {
		t.Fatalf("read the lens catalogue: %v", err)
	}
	var source string
	for _, kind := range catalogue {
		if kind.ID == graphstore.LensKindPreMortem && len(kind.Sources) == 1 {
			source = kind.Sources[0].ID
		}
	}
	if source == "" {
		t.Fatalf("the catalogue names no source constraint of %s: %+v", graphstore.LensKindPreMortem, catalogue)
	}

	read := treeRead(t, rig.handler, rig.issue, "", "alice", false)
	if node, _ := read.node(t, graphstore.KindLens, lens.ID); node.Role != treeRoleTree {
		t.Errorf("the lens is answered as %q, want a node of the tree", node.Role)
	}
	kind, _ := read.node(t, graphstore.KindLensKind, graphstore.LensKindPreMortem)
	constraint, _ := read.node(t, graphstore.KindConstraint, named)
	for _, context := range []model.IssueTreeNode{kind, constraint} {
		if context.Role != treeRoleContext || context.Parent != nil {
			t.Errorf("%s:%s is answered as %q under %+v, want context under none", context.Kind, context.ID, context.Role, context.Parent)
		}
	}
	if !read.holdsTuple(graphstore.TupleHasKind, treeEnd(graphstore.KindLens, lens.ID), treeEnd(graphstore.KindLensKind, graphstore.LensKindPreMortem)) {
		t.Errorf("the read holds no has_kind from the lens to its kind: %+v", read.Tuples)
	}
	if !read.holdsTuple(graphstore.TupleConstrainedBy, treeEnd(refs.BlockNodeKind, rig.requirementNode), treeEnd(graphstore.KindConstraint, named)) {
		t.Errorf("the read holds no constrained_by from the requirement to its constraint: %+v", read.Tuples)
	}
	for name, id := range map[string]string{"the pre-mortem kind's source": source, "a constraint nothing names": unnamed} {
		if held := read.nodesNamed(graphstore.KindConstraint, id); len(held) > 0 || strings.Contains(read.body, id) {
			t.Errorf("the read answers %s, %s, which no node of the tree names: %+v", name, id, held)
		}
	}
	for _, node := range read.Nodes {
		if node.Kind == graphstore.KindLensKind && node.ID != graphstore.LensKindPreMortem {
			t.Errorf("the read answers the lens_kind %s, which no node of the tree names", node.ID)
		}
	}
}

// TestTheTreeReadAnswersEachTypedBlockOnceWithItsDocument: on an issue that has not converted, a
// requirement of its spec set proposed and a check of its Verification document set overtaken are
// each answered once, under the parent the walk gives it, with its state, its visibility, the time
// it was made, the text_tree the blocks read answers for it, and a document member naming its
// document and its position in the blocks read's list; no prose block of either document is
// answered, and the issue answers converted false, no tree and its documents' ids.
func TestTheTreeReadAnswersEachTypedBlockOnceWithItsDocument(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "CORE")
	const specProse, checksProse = "A paragraph of the spec's prose.", "A paragraph of the checks' prose."
	issue := createDigestIssue(t, handler, database, "CORE", "Unconverted", specProse+"\n\n:::requirement{#r1}\nThe list comes first.\n:::\n", true)
	verification := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md",
		checksProse+"\n\n"+checkServing("c1", "dispatch://"+issue.Key+"/spec#b-r1"))
	spec := readDecisionDocument(t, database, issue.Key, "spec")
	checks := readDecisionDocument(t, database, issue.Key, verification.Artifact.Slug)
	for address, state := range map[string]string{spec.address("r1"): graphstore.StateProposed, checks.address("c1"): graphstore.StateOvertaken} {
		succeeded(t, "set "+address+" "+state, dispatchRequest(t, handler, http.MethodPost,
			"/api/v1/issues/"+issue.Key+"/nodes/"+url.PathEscape(address)+"/state", map[string]any{"state": state}, "alice"))
	}
	walk, err := docs.WalkIssueTree(ctx, database.Pool, issue.Key, refs.TreeCut{})
	if err != nil {
		t.Fatalf("walk the tree of %s: %v", issue.Key, err)
	}
	walkParent := map[string]refs.Target{}
	for _, node := range walk.Nodes {
		walkParent[node.ID] = node.Parent
	}

	read := treeRead(t, handler, issue.Key, "", "alice", true)
	if read.Converted || read.Tree != nil || read.Documents == nil || read.Documents.Spec == nil || *read.Documents.Spec != spec.id ||
		read.Documents.Verification == nil || *read.Documents.Verification != verification.Artifact.ID {
		t.Errorf("the unconverted issue answers converted %t, tree %v and documents %+v, want false, none, and its spec %s and Verification %s",
			read.Converted, read.Tree, read.Documents, spec.id, verification.Artifact.ID)
	}
	for _, typed := range []struct {
		document decisionDocument
		slug     string
		block    string
		state    string
	}{
		{spec, "spec", "r1", graphstore.StateProposed},
		{checks, verification.Artifact.Slug, "c1", graphstore.StateOvertaken},
	} {
		address := typed.document.address(typed.block)
		node, _ := read.node(t, refs.BlockNodeKind, address)
		parent := walkParent[address]
		if node.Parent == nil || node.Parent.Kind != parent.Kind || node.Parent.ID != parent.ID {
			t.Errorf("%s stands under %+v, want the walk's parent %+v", address, node.Parent, parent)
		}
		if node.State != typed.state || node.Visibility != graphstore.VisibilityProject || node.MadeAt == nil || node.BlockID != typed.block {
			t.Errorf("%s answers state %q, visibility %q, made_at %v and block_id %q, want %s, project, a time and %s",
				address, node.State, node.Visibility, node.MadeAt, node.BlockID, typed.state, typed.block)
		}
		blocks := decodeBody[[]model.ArtifactBlock](t, treesRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts/"+typed.slug+"/blocks", true))
		position := slices.IndexFunc(blocks, func(block model.ArtifactBlock) bool { return block.ID == typed.block })
		if position < 0 {
			t.Fatalf("the blocks read of %s lists no %s", typed.slug, typed.block)
		}
		if len(node.TextTree) == 0 || !bytes.Equal(node.TextTree, blocks[position].TextTree) {
			t.Errorf("%s answers the text_tree %s, want the blocks read's %s", address, node.TextTree, blocks[position].TextTree)
		}
		if node.Document == nil || node.Document.ArtifactID != typed.document.id || node.Document.Slug != typed.slug || node.Document.Position != position {
			t.Errorf("%s answers the document %+v, want %s (%s) at position %d", address, node.Document, typed.document.id, typed.slug, position)
		}
	}
	for _, node := range read.Nodes {
		if strings.Contains(node.Text, specProse) || strings.Contains(node.Text, checksProse) {
			t.Errorf("the read answers %s:%s, a prose block of a role document: %q", node.Kind, node.ID, node.Text)
		}
		if node.Kind == graphstore.KindArtifact || node.Kind == graphstore.KindVersion {
			t.Errorf("the read answers %s:%s, a document or a document version", node.Kind, node.ID)
		}
	}
}

// decisionByBlock is the decision of decisions whose block id is block.
func decisionByBlock(t *testing.T, decisions []model.Decision, block string) model.Decision {
	t.Helper()
	for _, decision := range decisions {
		if decision.BlockID == block {
			return decision
		}
	}
	t.Fatalf("the decisions read answers no decision %s: %+v", block, decisions)
	return model.Decision{}
}

// readIssueDecisionsWithTrees is GET /api/v1/issues/{key}/decisions as the dashboard reads it.
func readIssueDecisionsWithTrees(t *testing.T, handler http.Handler, key string) issueDecisions {
	t.Helper()
	return decodeBody[issueDecisions](t, treesRequest(t, handler, "/api/v1/issues/"+key+"/decisions", true))
}

// requireDecisionAsRead fails unless the tree read's decision node d1 answers every member of
// model.Decision as the decisions read answers it for the same decision, byte for byte, and its
// uncertainty and uncertainty_reason as that read's entry for it answers them.
func requireDecisionAsRead(t *testing.T, what string, handler http.Handler, key, node string) model.Decision {
	t.Helper()
	read := treeRead(t, handler, key, "", "alice", true)
	held, _ := read.node(t, refs.BlockNodeKind, node)
	if held.Decision == nil {
		t.Fatalf("%s: the tree read's decision %s answers no decision members", what, node)
	}
	decisions := readIssueDecisionsWithTrees(t, handler, key)
	want := decisionByBlock(t, decisions.Decisions, "d1")
	got, _ := json.Marshal(held.Decision.Decision)
	expected, _ := json.Marshal(want)
	if !bytes.Equal(got, expected) {
		t.Errorf("%s: the tree read's decision answers\n%s\nwant the decisions read's\n%s", what, got, expected)
	}
	for _, entry := range decisions.Entries {
		if entry.Node.Kind != refs.BlockNodeKind || entry.Node.ID != node {
			continue
		}
		if entryJSON(t, held.Decision.Uncertainty) != entryJSON(t, entry.Uncertainty) || entryJSON(t, held.Decision.UncertaintyReason) != entryJSON(t, entry.UncertaintyReason) {
			t.Errorf("%s: the tree read's decision %s answers the uncertainty %s and its reason %s, want the decisions read's entry's %s and %s", what, node,
				entryJSON(t, held.Decision.Uncertainty), entryJSON(t, held.Decision.UncertaintyReason), entryJSON(t, entry.Uncertainty), entryJSON(t, entry.UncertaintyReason))
		}
	}
	return held.Decision.Decision
}

// TestTheTreeReadAnswersEveryDecisionMemberAsTheDecisionsReadDoes: on a converted issue, the
// decision node answers every member of model.Decision equal to GET /api/v1/issues/{key}/decisions'
// for the same decision; after the words of the requirement its shaped tuple names change, its
// link answers changed_since_tie and not stale, and after the decision's own words change, stale,
// each as the decisions read's does; and the read answers the decision's decided_by person as
// context with the tuple.
func TestTheTreeReadAnswersEveryDecisionMemberAsTheDecisionsReadDoes(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createDigestIssue(t, handler, database, "CORE", "Decided", ":::requirement{#r1}\nAn urgent ask comes first.\n:::\n\n## Decisions\n\n"+
		":::decision{#d1 shaped=\"dispatch://CORE-1/spec#b-r1\"}\nWe order the list by urgency.\n:::\n", false)
	if issue.Key != "CORE-1" {
		t.Fatalf("the issue is %s, want CORE-1, which its decision's shaped names", issue.Key)
	}
	node := readDecisionDocument(t, database, issue.Key, "spec").address("d1")
	before := requireDecisionAsRead(t, "before the requirement's words change", handler, issue.Key, node)
	if len(before.Links) != 1 || before.Links[0].Stale || before.Links[0].ChangedSinceTie {
		t.Fatalf("the decision answers the links %+v, want one link that is neither stale nor changed since the tie", before.Links)
	}
	editSpec(t, handler, issue.Key, []map[string]any{{"op": "replace", "find": "An urgent ask comes first.", "with": "An urgent ask always comes first."}})
	after := requireDecisionAsRead(t, "after the requirement's words change", handler, issue.Key, node)
	if len(after.Links) != 1 || !after.Links[0].ChangedSinceTie || after.Links[0].Stale {
		t.Errorf("after the requirement's words changed the decision answers the links %+v, want its link changed since the tie and not stale, the decision's words unchanged since the tie", after.Links)
	}
	editSpec(t, handler, issue.Key, []map[string]any{{"op": "replace", "find": "We order the list by urgency.", "with": "We order the list by urgency, then by age."}})
	reworded := requireDecisionAsRead(t, "after the decision's own words change", handler, issue.Key, node)
	if len(reworded.Links) != 1 || !reworded.Links[0].Stale || !reworded.Links[0].ChangedSinceTie {
		t.Errorf("after the decision's own words changed the decision answers the links %+v, want its link stale, the decision changed after the tie and the requirement not since, and still changed since the tie", reworded.Links)
	}

	read := treeRead(t, handler, issue.Key, "", "alice", false)
	decided, err := graphstore.CurrentOut(context.Background(), database.Pool, refs.BlockNodeKind, node, []string{graphstore.TupleDecidedBy})
	if err != nil || len(decided) != 1 {
		t.Fatalf("read the decided_by of %s: %+v, %v", node, decided, err)
	}
	person, _ := read.node(t, decided[0].ToKind, decided[0].ToID)
	if person.Role != treeRoleContext {
		t.Errorf("the decided_by %s:%s is answered as %q, want context", person.Kind, person.ID, person.Role)
	}
	if !read.holdsTuple(graphstore.TupleDecidedBy, treeEnd(refs.BlockNodeKind, node), treeEnd(decided[0].ToKind, decided[0].ToID)) {
		t.Errorf("the read holds no decided_by from the decision to %s:%s: %+v", decided[0].ToKind, decided[0].ToID, read.Tuples)
	}
}

// resultsFixture is an issue whose spec holds r1 and whose Verification document holds c1 serving
// it, and the path of that document's upload.
func resultsFixture(t *testing.T, handler http.Handler, project string) (issue string, checks string) {
	t.Helper()
	created := createInteractionIssue(t, handler, project, "Results", ":::requirement{#r1}\nThe list comes first.\n:::\n")
	uploadDocument(t, handler, "/api/v1/issues/"+created.Key, "verification.md", checkServing("c1", "dispatch://"+created.Key+"/spec#b-r1"))
	return created.Key, "verification-md"
}

// TestTheTreeReadAnswersAResultsObservedTextAsItsReading: a result whose observed text holds a link
// and a reference answers, under `Dispatch-Trees: 1`, a text_tree equal byte for byte to the
// observed_tree the results read answers for it, a reading that keeps the link.
func TestTheTreeReadAnswersAResultsObservedTextAsItsReading(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue, _ := resultsFixture(t, handler, "TOBS")
	observed := "The run printed [the log](https://example.com/log) and named dispatch://" + issue + "/spec#b-r1."
	recorded := recordResult(t, handler, issue, resultBody("c1", "pass", headCommit, map[string]any{"observed": observed}))
	if recorded.Code != http.StatusCreated {
		t.Fatalf("record a result: status=%d body=%s", recorded.Code, recorded.Body.String())
	}
	result := decodeBody[recordedResult](t, recorded).Result
	read := treeRead(t, handler, issue, "", "alice", true)
	node, _ := read.node(t, graphstore.KindResult, result.ID)
	results := decodeBody[issueResults](t, treesRequest(t, handler, "/api/v1/issues/"+issue+"/results", true))
	check := checkOf(t, results, "c1")
	if len(check.Results) != 1 || len(check.Results[0].ObservedTree) == 0 {
		t.Fatalf("the results read answers %+v, want the one result with its observed_tree", check.Results)
	}
	if !bytes.Equal(node.TextTree, check.Results[0].ObservedTree) {
		t.Errorf("the result answers the text_tree\n%s\nwant the results read's observed_tree\n%s", node.TextTree, check.Results[0].ObservedTree)
	}
	if !bytes.Contains(node.TextTree, []byte(`"link"`)) || node.Text != observed {
		t.Errorf("the result answers the text %q and the text_tree %s, want its observed text read with its link kept", node.Text, node.TextTree)
	}
}

// TestTheTreeReadAnswersAResultAgainstTheWordsItWasRecordedFor: a result's verdict, retraction,
// check_version and check_sha256 equal the results read's, and the check node's text_sha256 and
// changed_version equal that read's sha256 and version of the check; after the check's words change
// the check node's move with the read's while the result's check_sha256 stays; a retraction is read
// with its reason.
func TestTheTreeReadAnswersAResultAgainstTheWordsItWasRecordedFor(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue, slug := resultsFixture(t, handler, "TWRD")
	recorded := recordResult(t, handler, issue, resultBody("c1", "pass", headCommit, nil))
	if recorded.Code != http.StatusCreated {
		t.Fatalf("record a result: status=%d body=%s", recorded.Code, recorded.Body.String())
	}
	result := decodeBody[recordedResult](t, recorded).Result
	checkNode := readDecisionDocument(t, database, issue, slug).address("c1")
	compare := func(what string) (model.IssueTreeNode, model.IssueTreeNode, model.CheckBlock) {
		t.Helper()
		read := treeRead(t, handler, issue, "", "alice", false)
		check := checkOf(t, readResults(t, handler, issue), "c1")
		resultNode, _ := read.node(t, graphstore.KindResult, result.ID)
		checked, _ := read.node(t, refs.BlockNodeKind, checkNode)
		if len(check.Results) != 1 || resultNode.Result == nil || checked.Check == nil {
			t.Fatalf("%s: the results read answers %+v, the result node %+v and the check node %+v", what, check.Results, resultNode.Result, checked.Check)
		}
		want := check.Results[0]
		got := resultNode.Result
		if got.Verdict != want.Verdict || got.CheckVersion != want.CheckVersion || got.CheckSHA256 != want.CheckSHA256 ||
			(got.Retracted == nil) != (want.Retracted == nil) || got.Retracted != nil && got.Retracted.Reason != want.Retracted.Reason {
			t.Errorf("%s: the result node answers %+v, want the results read's verdict %q, retraction %+v, check_version %d and check_sha256 %s",
				what, got, want.Verdict, want.Retracted, want.CheckVersion, want.CheckSHA256)
		}
		if checked.Check.TextSHA256 != check.SHA256 || checked.Check.ChangedVersion != check.Version {
			t.Errorf("%s: the check node answers %+v, want the results read's sha256 %s and version %d", what, checked.Check, check.SHA256, check.Version)
		}
		return resultNode, checked, check
	}
	firstResult, firstCheck, _ := compare("as recorded")
	uploadDocument(t, handler, "/api/v1/issues/"+issue, "verification.md",
		":::check{#c1 serves=\"dispatch://"+issue+"/spec#b-r1\" surface=\"cli\" run=\"go test ./...\"}\nThe check that proves it, reworded.\n:::\n")
	movedResult, movedCheck, _ := compare("after the check's words changed")
	if movedCheck.Check.TextSHA256 == firstCheck.Check.TextSHA256 || movedCheck.Check.ChangedVersion == firstCheck.Check.ChangedVersion {
		t.Errorf("the check's words changed and its node answers %+v, as before, %+v", movedCheck.Check, firstCheck.Check)
	}
	if movedResult.Result.CheckSHA256 != firstResult.Result.CheckSHA256 || movedResult.Result.CheckSHA256 == movedCheck.Check.TextSHA256 {
		t.Errorf("the result answers check_sha256 %s after the words changed, want the %s it was recorded against, not the check's %s",
			movedResult.Result.CheckSHA256, firstResult.Result.CheckSHA256, movedCheck.Check.TextSHA256)
	}
	const reason = "The run named the wrong commit."
	succeededCreated := retractAsPerson(t, handler, result.ID, reason)
	if succeededCreated.Code != http.StatusCreated {
		t.Fatalf("retract the result: status=%d body=%s", succeededCreated.Code, succeededCreated.Body.String())
	}
	retracted, _, _ := compare("after the retraction")
	if retracted.Result.Retracted == nil || retracted.Result.Retracted.Reason != reason {
		t.Errorf("the retracted result answers %+v, want the retraction with its reason %q", retracted.Result.Retracted, reason)
	}
}

// TestTheTreeReadNamesTheAncestorAnIssueIsAViewOf: view_of is null on an issue that holds
// requirements; on a converted child that holds none it names the parent, and a requirement
// written on that child is refused 409 REQUIREMENT_BELONGS_TO_ANCESTOR naming the same issue, by an
// edit, an upload replacing a document and an upload of a new one, leaving the child unchanged; a
// child that holds a requirement of its own is no view; and once the parent is done, the child is
// no view and takes a requirement.
//
// The child holding a requirement of its own is created unconverted, as the release before writes
// a child, with no issue_trees row: a child born converted under a parent whose requirements stand
// is a view whatever it holds, and its create with a requirement is refused
// (TestAnIssueCreateRefusesARequirementOnAView), so no converted child holding one can be created
// at this head.
func TestTheTreeReadNamesTheAncestorAnIssueIsAViewOf(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createReferenceAPIProject(t, handler, "VIEW")
	parent := createDigestIssue(t, handler, database, "VIEW", "The parent", ":::requirement{#r1}\nThe parent states it.\n:::\n", false)
	child := createChildIssue(t, handler, "VIEW", parent.Key, "The child's design.\n")
	owning := createDigestChild(t, handler, database, "VIEW", "The owning child", ":::requirement{#cr1}\nThe child's own requirement.\n:::\n", parent.Key, true)

	read := treeRead(t, handler, parent.Key, "", "alice", false)
	if read.ViewOf != nil {
		t.Errorf("the parent, which holds requirements, answers view_of %v, want null", *read.ViewOf)
	}
	if node, _ := read.node(t, graphstore.KindIssue, child.Key); node.IssueTreeIssue == nil || node.ViewOf == nil || *node.ViewOf != parent.Key {
		t.Errorf("the parent's read answers the child %s with %+v, want view_of %s", child.Key, node.IssueTreeIssue, parent.Key)
	}
	childRead := treeRead(t, handler, child.Key, "", "alice", false)
	if !childRead.Converted || childRead.ViewOf == nil || *childRead.ViewOf != parent.Key {
		t.Fatalf("the child answers converted %t and view_of %v, want a converted view of %s", childRead.Converted, childRead.ViewOf, parent.Key)
	}
	if owned := treeRead(t, handler, owning.Key, "", "alice", false); owned.ViewOf != nil {
		t.Errorf("the child holding a requirement of its own answers view_of %s, want null", *owned.ViewOf)
	}

	childSpec := artifactTextOf(t, handler, child.PrimaryArtifactID)
	requirement := ":::requirement{#cr9}\nThe child's own work.\n:::\n"
	refusedWith := func(what string, response *httptest.ResponseRecorder) {
		t.Helper()
		refusal := requireRefused(t, what, response, http.StatusConflict, codeRequirementBelongsToAncestor, parent.Key)
		if !strings.Contains(refusal.Error, "cr9") {
			t.Errorf("%s is refused %q, want the requirement it adds named", what, refusal.Error)
		}
	}
	refusedWith("an edit adding a requirement to the child", dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/issues/"+child.Key+"/artifacts/spec/edits", map[string]any{"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": requirement}}}, "alice"))
	owner := "/api/v1/issues/" + child.Key + "/artifacts"
	refusedWith("an upload replacing the child's spec with a requirement", dispatchRequest(t, handler, http.MethodPost, owner,
		map[string]any{"name": "spec", "content": childSpec + "\n" + requirement, "precondition": uploadBaseNamed(t, handler, owner, "spec")}, "alice"))
	refusedWith("an upload of a new document holding a requirement", dispatchRequest(t, handler, http.MethodPost, owner,
		map[string]any{"name": "notes.md", "content": requirement}, "alice"))
	if after := artifactTextOf(t, handler, child.PrimaryArtifactID); after != childSpec {
		t.Errorf("the refused writes left the child's spec %q, want %q", after, childSpec)
	}
	if again := treeRead(t, handler, child.Key, "", "alice", false); again.ViewOf == nil || len(again.supertagged("requirement")) > 0 {
		t.Errorf("after the refusals the child answers view_of %v and the requirements %+v, want still a view holding none", again.ViewOf, again.supertagged("requirement"))
	}

	succeeded(t, "close the parent", dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+parent.Key, map[string]any{"status": "done"}, "alice"))
	if closed := treeRead(t, handler, child.Key, "", "alice", false); closed.ViewOf != nil {
		t.Errorf("the child of a done parent answers view_of %s, want null", *closed.ViewOf)
	}
	succeeded(t, "add a requirement to the child of a done parent", dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/issues/"+child.Key+"/artifacts/spec/edits", map[string]any{"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": requirement}}}, "alice"))
}

// TestAKeepReachesADecisionOfAnIssueRoom: a decision of an issue born converted, in its spec, and
// one of a document that joined the issue's room later, its Verification document, are each kept
// through the Keep route with that document's id, the decision's block id and the text_sha256 the
// tree read answered; the tree read and the decisions read then answer each kept by the person who
// kept it, and a sum the decision no longer holds is refused 409 DECISION_CHANGED.
func TestAKeepReachesADecisionOfAnIssueRoom(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createReferenceAPIProject(t, handler, "KEEP")
	issue := createDigestIssue(t, handler, database, "KEEP", "Kept", "## Decisions\n\n:::decision{#d1}\nWe keep the list short.\n:::\n", false)
	verification := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md",
		"## Decisions\n\n:::decision{#d2}\nWe check the list twice.\n:::\n")
	for _, kept := range []struct {
		name     string
		document string
		slug     string
		block    string
	}{
		{"the spec's decision", issue.PrimaryArtifactID, "spec", "d1"},
		{"the Verification document's decision", verification.Artifact.ID, verification.Artifact.Slug, "d2"},
	} {
		room := readDecisionDocument(t, database, issue.Key, kept.slug)
		if room.room == kept.document {
			t.Fatalf("%s: the document %s is its own room, want it a fragment of the issue's room", kept.name, kept.document)
		}
		node := room.address(kept.block)
		decision, _ := treeRead(t, handler, issue.Key, "", "alice", false).node(t, refs.BlockNodeKind, node)
		if decision.Decision == nil || decision.Decision.TextSHA256 == "" || decision.Decision.Kept != nil {
			t.Fatalf("%s: the tree read answers %+v, want an unkept decision with its text_sha256", kept.name, decision.Decision)
		}
		keep := "/api/v1/artifacts/" + kept.document + "/blocks/" + kept.block + "/keep"
		requireRefused(t, kept.name+" kept with a sum it does not hold", dispatchRequest(t, handler, http.MethodPost, keep,
			map[string]any{"text_sha256": strings.Repeat("0", 64)}, "alice"), http.StatusConflict, "DECISION_CHANGED", "")
		succeeded(t, "keep "+kept.name, dispatchRequest(t, handler, http.MethodPost, keep, map[string]any{"text_sha256": decision.Decision.TextSHA256}, "alice"))
		after, _ := treeRead(t, handler, issue.Key, "", "alice", false).node(t, refs.BlockNodeKind, node)
		if after.Decision == nil || after.Decision.Kept == nil || after.Decision.Kept.KeptBy != "alice" || after.Decision.WaitingReview {
			t.Errorf("%s: after the Keep the tree read answers %+v, want it kept by alice", kept.name, after.Decision)
		}
		if read := decisionByBlock(t, readIssueDecisionsWithTrees(t, handler, issue.Key).Decisions, kept.block); read.Kept == nil || read.Kept.KeptBy != "alice" {
			t.Errorf("%s: after the Keep the decisions read answers %+v, want it kept by alice", kept.name, read.Kept)
		}
	}
}

// TestTheTreeReadLiftsAVisibleNodeAboveAParentItsReaderMayNotSee: a project risk under alice's
// author-only pre-mortem lens on a requirement stands under the lens in alice's read; in bob's it
// stands under the requirement, after the requirement's own children, and the lens's id and its
// verdict are nowhere in his answer, no node, parent or tuple naming it.
//
// The store refuses to write a project node under an author-only one (graphstore's
// ErrVisibilityWiderThanParent), so the test writes the lens and the risk project-visible through
// their routes and then reserves the lens for alice by SQL, the shape the spec names.
func TestTheTreeReadLiftsAVisibleNodeAboveAParentItsReaderMayNotSee(t *testing.T) {
	rig := newReasoningRig(t)
	ctx := context.Background()
	const verdict = "The cutover holds only if the index builds first, alice thinks."
	hidden := rig.invoke(rig.requirement, graphstore.LensKindPreMortem, "alice", map[string]any{"verdict": verdict})
	written := rig.post("/risks", map[string]any{
		"lens": hidden.Address, "heading": "The migration locks the table.", "early_warning": "Writes queue.", "prevention": "Build the index concurrently.",
	}, "alice")
	if written.Code != http.StatusCreated {
		t.Fatalf("write a project risk under alice's lens: status=%d body=%s", written.Code, written.Body.String())
	}
	risk := decodeBody[riskWritten](t, written)
	if _, err := rig.database.Pool.Exec(ctx, `update graph_nodes set visibility = $3 where kind = $1 and id = $2`,
		graphstore.KindLens, hidden.ID, graphstore.VisibilityAuthor); err != nil {
		t.Fatalf("reserve alice's lens for her: %v", err)
	}
	placed, err := graphstore.CurrentOut(ctx, rig.database.Pool, graphstore.KindLens, hidden.ID, []string{graphstore.TuplePartOf})
	if err != nil || len(placed) != 1 || placed[0].ToKind != refs.BlockNodeKind || placed[0].ToID != rig.requirementNode {
		t.Fatalf("alice's lens %s stands under %+v (%v), want the requirement's node %s, which the tree walks", hidden.ID, placed, err, rig.requirementNode)
	}
	lensTarget, riskTarget := refs.Target{Kind: graphstore.KindLens, ID: hidden.ID}, refs.Target{Kind: graphstore.KindRisk, ID: risk.ID}
	for _, reader := range []struct {
		login string
		sees  []refs.Target
	}{{"alice", []refs.Target{lensTarget, riskTarget}}, {"bob", []refs.Target{riskTarget}}} {
		loaded, err := refs.Nodes(ctx, rig.database.Pool, model.Actor{Kind: "user", ID: reader.login}, []refs.Target{lensTarget, riskTarget})
		if err != nil {
			t.Fatalf("load the lens and the risk as %s: %v", reader.login, err)
		}
		if len(loaded) != len(reader.sees) {
			t.Fatalf("the registry's loaders answer %s %v, want %v", reader.login, loaded, reader.sees)
		}
	}
	// A lens alice reserves for herself through the route, with a risk under it that takes its
	// visibility: hers to read, and nowhere in bob's answer.
	reserved := rig.invoke(rig.requirement, graphstore.LensKindPreMortem, "alice", map[string]any{"visibility": graphstore.VisibilityAuthor})
	reservedRisk := decodeBody[riskWritten](t, created(t, "a risk under alice's reserved lens", rig.post("/risks", map[string]any{
		"lens": reserved.Address, "heading": "Her own worry.", "early_warning": "She sees it.", "prevention": "She acts.",
	}, "alice")))

	alice := treeRead(t, rig.handler, rig.issue, "", "alice", false)
	if node, _ := alice.node(t, graphstore.KindRisk, risk.ID); node.Parent == nil || node.Parent.Kind != graphstore.KindLens || node.Parent.ID != hidden.ID {
		t.Errorf("alice reads the risk under %+v, want her lens %s", node.Parent, hidden.ID)
	}
	if node, _ := alice.node(t, graphstore.KindRisk, reservedRisk.ID); node.Parent == nil || node.Parent.ID != reserved.ID || node.Visibility != graphstore.VisibilityAuthor {
		t.Errorf("alice reads the risk under her reserved lens as %+v under %+v, want hers under %s", node.Visibility, node.Parent, reserved.ID)
	}
	bob := treeRead(t, rig.handler, rig.issue, "", "bob", false)
	node, at := bob.node(t, graphstore.KindRisk, risk.ID)
	if node.Parent == nil || node.Parent.Kind != refs.BlockNodeKind || node.Parent.ID != rig.requirementNode {
		t.Fatalf("bob reads the risk under %+v, want the requirement %s, the lens's own parent", node.Parent, rig.requirementNode)
	}
	under := map[string]bool{refs.BlockNodeKind + ":" + rig.requirementNode: true}
	for index, held := range bob.Nodes {
		if held.Parent == nil || !under[held.Parent.Kind+":"+held.Parent.ID] || held.Kind == graphstore.KindRisk && held.ID == risk.ID {
			continue
		}
		under[held.Kind+":"+held.ID] = true
		if index > at {
			t.Errorf("bob reads %s:%s, under the requirement, at %d after the lifted risk at %d, want the requirement's own children first", held.Kind, held.ID, index, at)
		}
	}
	if strings.Contains(bob.body, hidden.ID) || strings.Contains(bob.body, verdict) {
		t.Errorf("bob's answer names alice's lens %s or its verdict: %s", hidden.ID, bob.body)
	}
	if strings.Contains(bob.body, reserved.ID) || strings.Contains(bob.body, reservedRisk.ID) {
		t.Errorf("bob's answer names alice's reserved lens %s or its risk %s: %s", reserved.ID, reservedRisk.ID, bob.body)
	}
}

// TestAFileNamedLikeARoleDocumentPlaysNoRole is c-role-kind. On an issue that has not converted,
// whose role-less upload of kind file with the slug plan-md is written by SQL as the release
// before stores one, the tree read answers converted false and no plan document; a second such
// issue whose role-less row of kind doc has that slug answers that row as its plan. On an issue
// born converted, an upload named plan.md, and one named verification.md, whose bytes are not
// markdown is a file node under the issue, ?view=plan and ?view=verification answer no node of it,
// and the issue's plan and verification query nodes and their versions are as they were.
func TestAFileNamedLikeARoleDocumentPlaysNoRole(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "ROLE")
	insertRoleless := func(issue, kind string) string {
		t.Helper()
		var id string
		if err := database.Pool.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
			values ($1, 'ROLE', 'plan-md', 'plan.md', $2, '{"kind":"user","id":"alice"}') returning id::text`, issue, kind).Scan(&id); err != nil {
			t.Fatalf("store a role-less %s plan-md on %s: %v", kind, issue, err)
		}
		return id
	}
	upload := createDigestIssue(t, handler, database, "ROLE", "A file named plan.md", "The spec.\n", true)
	file := insertRoleless(upload.Key, "file")
	read := treeRead(t, handler, upload.Key, "", "alice", false)
	if read.Converted || read.Documents == nil || read.Documents.Plan != nil {
		t.Errorf("the unconverted issue answers converted %t and documents %+v, want no plan document: the file %s (plan-md) plays none",
			read.Converted, read.Documents, file)
	}
	rolling := createDigestIssue(t, handler, database, "ROLE", "A document a roll left with no role", "The spec.\n", true)
	plan := insertRoleless(rolling.Key, "doc")
	if read := treeRead(t, handler, rolling.Key, "", "alice", false); read.Documents == nil || read.Documents.Plan == nil || *read.Documents.Plan != plan {
		t.Errorf("the issue whose role-less doc row is plan-md answers documents %+v, want %s as its plan", read.Documents, plan)
	}

	converted := createDigestIssue(t, handler, database, "ROLE", "A converted issue", "The spec.\n", false)
	queryVersions := func(query string) []string {
		t.Helper()
		rows, err := database.Pool.Query(ctx, `
			select a.id::text || '@' || (select count(*) from artifact_versions v where v.artifact_id = a.id)
			from artifacts a join graph_nodes n on n.kind = 'artifact' and n.id = a.ref_key and n.supertag = $2 and n.attributes->>$3 = $4
			where a.issue_key = $1 order by 1`, converted.Key, graphstore.SupertagQuery, graphstore.AttributeQuery, query)
		if err != nil {
			t.Fatalf("read the %s query nodes of %s: %v", query, converted.Key, err)
		}
		defer rows.Close()
		var held []string
		for rows.Next() {
			var row string
			if err := rows.Scan(&row); err != nil {
				t.Fatalf("read a %s query node: %v", query, err)
			}
			held = append(held, row)
		}
		return held
	}
	for _, named := range []struct{ name, view string }{{"plan.md", graphstore.QueryPlan}, {"verification.md", graphstore.QueryVerification}} {
		before := queryVersions(named.view)
		response := multipartRequest(t, handler, "/api/v1/issues/"+converted.Key+"/artifacts", map[string]string{"name": named.name}, named.name, "application/octet-stream", []byte{0, 1, 2, 3}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("upload %s as bytes: status=%d body=%s", named.name, response.Code, response.Body.String())
		}
		uploaded := decodeBody[uploadedArtifact](t, response).Artifact
		if uploaded.Kind != "file" {
			t.Fatalf("the upload %s is of kind %s, want file", named.name, uploaded.Kind)
		}
		viewed := treeRead(t, handler, converted.Key, "?view="+named.view, "alice", false)
		for _, node := range viewed.Nodes {
			if node.ID == uploaded.RefKey || strings.HasPrefix(node.ID, uploaded.RefKey+"#") || strings.HasPrefix(node.ID, uploaded.RefKey+"@") {
				t.Errorf("?view=%s answers %s:%s, a node of the file %s", named.view, node.Kind, node.ID, named.name)
			}
		}
		if after := queryVersions(named.view); !slices.Equal(after, before) {
			t.Errorf("the upload of the file %s moved the %s query nodes from %v to %v", named.name, named.view, before, after)
		}
	}
}

// TestTheVerificationViewAnswersEveryRequirementOfTheIssue: an issue born converted holds two
// requirements, r1 served by its check c1, which holds one result, and r2 served by no check. Its
// ?view=verification answers c1 and the result as the query's and both requirements as context,
// each once, with its supertag and its parent named, so the requirement no check serves is in that
// one read. A parent holding the same two requirements and no check of its own takes a child, a
// view of it, whose check serves the parent's r1: the parent's ?view=verification answers both of
// its requirements as context and the child's check, marked with the child, and the child's own
// ?view=verification answers the parent's r1, which its check serves, and not r2.
func TestTheVerificationViewAnswersEveryRequirementOfTheIssue(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createReferenceAPIProject(t, handler, "GAPS")
	answered := func(read treeAnswer, kind, id string) (model.IssueTreeNode, bool) {
		t.Helper()
		held := read.nodesNamed(kind, id)
		if len(held) != 1 {
			t.Errorf("?view=verification of %s answers %s:%s %d times, want it once", read.Issue, kind, id, len(held))
			return model.IssueTreeNode{}, false
		}
		return held[0], true
	}
	requireRequirements := func(read treeAnswer, owner string, blocks ...string) {
		t.Helper()
		spec := readDecisionDocument(t, database, owner, "spec")
		for _, block := range blocks {
			held := read.nodesNamed(refs.BlockNodeKind, spec.address(block))
			if len(held) != 1 {
				t.Errorf("?view=verification of %s answers the requirement %s of %s (%s) %d times, want it once, as context: %v",
					read.Issue, block, owner, spec.address(block), len(held), read.addresses())
				continue
			}
			node := held[0]
			if node.Role != treeRoleContext || node.Supertag != requirementBlockType {
				t.Errorf("?view=verification of %s answers the requirement %s as role %q and supertag %q, want context and %s",
					read.Issue, block, node.Role, node.Supertag, requirementBlockType)
			}
			if node.Parent == nil || node.Parent.Kind == "" || node.Parent.ID == "" || node.Parent.Supertag == "" || node.Parent.FirstLine == "" {
				t.Errorf("?view=verification of %s names the parent of the requirement %s as %+v, want its kind, id, supertag and first line",
					read.Issue, block, node.Parent)
			}
		}
	}

	issue := createDigestIssue(t, handler, database, "GAPS", "Two requirements", twoRequirements, false)
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", checkServing("c1", "dispatch://"+issue.Key+"/spec#b-r1"))
	recorded := recordResult(t, handler, issue.Key, resultBody("c1", "pass", headCommit, nil))
	if recorded.Code != http.StatusCreated {
		t.Fatalf("record a result of c1: status=%d body=%s", recorded.Code, recorded.Body.String())
	}
	result := decodeBody[recordedResult](t, recorded).Result
	read := treeRead(t, handler, issue.Key, "?view=verification", "alice", false)
	check := readDecisionDocument(t, database, issue.Key, "verification-md").address("c1")
	if node, held := answered(read, refs.BlockNodeKind, check); held && node.Role != treeRoleQuery {
		t.Errorf("?view=verification of %s answers the check c1 as %q, want the query's", issue.Key, node.Role)
	}
	if node, held := answered(read, graphstore.KindResult, result.ID); held && node.Role != treeRoleQuery {
		t.Errorf("?view=verification of %s answers c1's result as %q, want the query's", issue.Key, node.Role)
	}
	requireRequirements(read, issue.Key, "r1", "r2")

	parent := createDigestIssue(t, handler, database, "GAPS", "The parent", twoRequirements, false)
	child := createDigestChild(t, handler, database, "GAPS", "The child", "The child's design.\n", parent.Key, false)
	uploadDocument(t, handler, "/api/v1/issues/"+child.Key, "verification.md", checkServing("cc1", "dispatch://"+parent.Key+"/spec#b-r1"))
	childCheck := readDecisionDocument(t, database, child.Key, "verification-md").address("cc1")
	parentRead := treeRead(t, handler, parent.Key, "?view=verification", "alice", false)
	if node, held := answered(parentRead, refs.BlockNodeKind, childCheck); held && (node.Role != treeRoleContext || node.Issue != child.Key) {
		t.Errorf("?view=verification of %s answers the child's check cc1 as %q of %q, want context of %s", parent.Key, node.Role, node.Issue, child.Key)
	}
	requireRequirements(parentRead, parent.Key, "r1", "r2")

	childRead := treeRead(t, handler, child.Key, "?view=verification", "alice", false)
	requireRequirements(childRead, parent.Key, "r1")
	if unserved := readDecisionDocument(t, database, parent.Key, "spec").address("r2"); childRead.index(refs.BlockNodeKind, unserved) >= 0 {
		t.Errorf("?view=verification of the child %s answers the parent's r2 (%s), which no check of the child serves", child.Key, unserved)
	}
}

// decisionsViewSpec is the spec of the decisions view's issue key: two requirements; an ask that
// shaped r1, which bob answers; an ask that shaped r2, which nobody answers; a decision that shaped
// r1, with a reading under it, which alice keeps; and a decision that shaped nothing, which waits
// for review.
func decisionsViewSpec(key string) string {
	pointer := func(block string) string { return "dispatch://" + key + "/spec#b-" + block }
	return ":::requirement{#r1}\nThe list orders the asks by urgency.\n:::\n\n" +
		":::requirement{#r2}\nThe list shows each ask once.\n:::\n\n" +
		":::ask{#a1 urgency=\"med\" multiple=\"false\" shaped=\"" + pointer("r1") + "\"}\nWhich order?\n\n" +
		"- Urgency: The most urgent first\n- Age: The oldest first\n:::\n\n" +
		":::ask{#a2 urgency=\"high\" multiple=\"true\" shaped=\"" + pointer("r2") + "\"}\nWhich lists show it?\n\n" +
		"- Inbox: The person's inbox\n- Issue: The issue's own list\n:::\n\n" +
		"::::decision{#d1 shaped=\"" + pointer("r1") + "\"}\nWe order the list by urgency.\n\n" +
		":::reading{#g1}\nThe agent reads it as urgency first and age second.\n:::\n::::\n\n" +
		":::decision{#d2}\nWe show each ask once.\n:::\n"
}

// requireDecisionsViewAsRead fails unless view, ?view=decisions of an issue, answers every entry
// of read, the decisions read of the same issue, as the query's, keyed as read keys it and member
// by member: an ask by its ask id with its question, options, urgency and multiple; a decision and
// an answered ask with every member of model.Decision the read's `decisions` answers for it, and
// the block id, text, sum, kind, replaced, kept and waiting_review of its entry; and each reading
// the read lists under an entry, a decision's or an answered ask's, under that entry's node. It
// answers each entry's node by the entry's key.
func requireDecisionsViewAsRead(t *testing.T, what string, view treeAnswer, read issueDecisions) map[string]model.IssueTreeNode {
	t.Helper()
	decided := make(map[string]model.Decision, len(read.Decisions))
	for _, decision := range read.Decisions {
		decided[decision.Ref] = decision
	}
	nodes := map[string]model.IssueTreeNode{}
	for _, entry := range read.Entries {
		held := view.nodesNamed(entry.Node.Kind, entry.Node.ID)
		if len(held) != 1 || held[0].Role != treeRoleQuery {
			t.Errorf("%s: ?view=decisions answers the entry %s %d times as %s, want it once as the query's; it answers %v",
				what, entryKey(entry), len(held), entryJSON(t, held), view.addresses())
			continue
		}
		node := held[0]
		nodes[entryKey(entry)] = node
		for _, reading := range entry.Readings {
			held := view.nodesNamed(reading.Node.Kind, reading.Node.ID)
			if len(held) != 1 || held[0].Role != treeRoleQuery || held[0].Parent == nil ||
				held[0].Parent.Kind != entry.Node.Kind || held[0].Parent.ID != entry.Node.ID {
				t.Errorf("%s: ?view=decisions answers the reading %s:%s of the entry %s as %s, want it once, the query's, under that entry's node",
					what, reading.Node.Kind, reading.Node.ID, entryKey(entry), entryJSON(t, held))
			}
		}
		if entry.Node.Kind == graphstore.KindAsk {
			if entry.Question == nil || node.Text != *entry.Question || node.Ask == nil ||
				entryJSON(t, node.Ask.Options) != entryJSON(t, entry.Options) || entry.Urgency == nil || node.Ask.Urgency != *entry.Urgency ||
				entry.Multiple == nil || node.Ask.Multiple != *entry.Multiple {
				t.Errorf("%s: ?view=decisions answers the ask %s with the text %q and the wording %s, want the decisions read's question %v, options %s, urgency %v and multiple %v",
					what, entryKey(entry), node.Text, entryJSON(t, node.Ask), entryJSON(t, entry.Question), entryJSON(t, entry.Options),
					entryJSON(t, entry.Urgency), entryJSON(t, entry.Multiple))
			}
		}
		want, isDecision := decided[entry.Ref]
		if !isDecision {
			if node.Decision != nil {
				t.Errorf("%s: ?view=decisions answers the entry %s, which the decisions read answers as no decision, with the decision %s",
					what, entryKey(entry), entryJSON(t, node.Decision))
			}
			continue
		}
		if node.Decision == nil {
			t.Errorf("%s: ?view=decisions answers the entry %s with no decision, want the decisions read's %s", what, entryKey(entry), entryJSON(t, want))
			continue
		}
		if got, expected := entryJSON(t, node.Decision.Decision), entryJSON(t, want); got != expected {
			t.Errorf("%s: ?view=decisions answers the entry %s with the decision\n%s\nwant the decisions read's\n%s", what, entryKey(entry), got, expected)
		}
		if entryJSON(t, node.Decision.Uncertainty) != entryJSON(t, entry.Uncertainty) || entryJSON(t, node.Decision.UncertaintyReason) != entryJSON(t, entry.UncertaintyReason) {
			t.Errorf("%s: ?view=decisions answers the entry %s with the uncertainty %s and its reason %s, want the entry's %s and %s", what, entryKey(entry),
				entryJSON(t, node.Decision.Uncertainty), entryJSON(t, node.Decision.UncertaintyReason), entryJSON(t, entry.Uncertainty), entryJSON(t, entry.UncertaintyReason))
		}
		got := node.Decision
		blockID := ""
		if entry.Document != nil {
			blockID = entry.Document.BlockID
		}
		if entry.Node.Kind == refs.BlockNodeKind && (got.BlockID != blockID || got.Kind != entry.Kind) ||
			got.Text != entry.Text || got.TextSHA256 != entry.TextSHA256 || got.Replaced != entry.Replaced ||
			entryJSON(t, got.Kept) != entryJSON(t, entry.Kept) || got.WaitingReview != entry.WaitingReview {
			t.Errorf("%s: ?view=decisions answers the entry %s with block_id %q, kind %q, replaced %t, kept %s and waiting_review %t, want the entry's %q, %q, %t, %s and %t",
				what, entryKey(entry), got.BlockID, got.Kind, got.Replaced, entryJSON(t, got.Kept), got.WaitingReview,
				blockID, entry.Kind, entry.Replaced, entryJSON(t, entry.Kept), entry.WaitingReview)
		}
	}
	return nodes
}

// TestTheDecisionsViewAnswersTheDecisionsQuery: an issue born converted and one that has not
// converted each hold decisionsViewSpec, with its ask a1 answered and then closed by the reading
// g2, a2 open, its decision d1, which holds the reading g1, kept by alice, d2 waiting for review,
// and a constraint r1's constrained_by names. Each one's ?view=decisions answers every entry of the
// decisions read as the query's, keyed and shaped as that read answers it
// (requireDecisionsViewAsRead), the open question with its options, the kept decision with its
// keeper, the waiting one waiting, the reading under its decision and the reading under the
// answered ask's node, and no node of its query but an ask, a decision or a reading of a decision
// or of an answered ask. Its ?view=spec answers, as context, d1 and the answered a1, each with its
// shaped tuple to r1, and the constraint with r1's constrained_by, and neither the open a2, which
// shaped r2, nor d2, which shaped nothing. A version of the decisions query node is answered as a
// version no record holds.
func TestTheDecisionsViewAnswersTheDecisionsQuery(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	documents := deps.Docs.(*docs.Service)
	createReferenceAPIProject(t, handler, "CORE")
	for _, issueCase := range []struct {
		what        string
		key         string
		unconverted bool
	}{
		{"the issue born converted", "CORE-1", false},
		{"the issue that has not converted", "CORE-2", true},
	} {
		what := issueCase.what
		issue := createDigestIssue(t, handler, database, "CORE", "Decided", decisionsViewSpec(issueCase.key), issueCase.unconverted)
		if issue.Key != issueCase.key {
			t.Fatalf("%s is %s, want %s, which its shaped pointers name", what, issue.Key, issueCase.key)
		}
		settleIssue(t, documents, issue.Key)
		spec := readDecisionDocument(t, database, issue.Key, "spec")
		answered, open := blockAskID(t, handler, issue.Key, "a1"), blockAskID(t, handler, issue.Key, "a2")
		answerDecisionBlock(t, handler, answered, "bob", []string{"Urgency"}, "Urgency, then age.", nil)
		// The reading g2 closes the answered a1, written as a person rewrites the spec around it.
		uploadAs(t, handler, issue.Key, "spec.md",
			withAskReading(t, documentMarkdown(t, handler, spec.id), "a1", "g2", "The agent reads the answer as urgency before age."), "alice")
		settleIssue(t, documents, issue.Key)
		kept := requireEntry(t, readEntries(t, handler, issue.Key), "the decision d1", refs.BlockNodeKind+":"+spec.address("d1"))
		keepBlock(t, handler, spec.id, "d1", kept.TextSHA256)
		written := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/constraints",
			map[string]any{"standard": "RFC 9110", "reference": "section 9.2.2"}, "alice")
		if written.Code != http.StatusCreated {
			t.Fatalf("%s: write a constraint: status=%d body=%s", what, written.Code, written.Body.String())
		}
		constraint := decodeBody[constraintRead](t, written)
		if pointed := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+nodePath(spec.address("r1"), "pointers"),
			map[string]any{"kind": graphstore.TupleConstrainedBy, "action": pointerAppend, "to": constraint.Address}, "alice"); pointed.Code != http.StatusOK {
			t.Fatalf("%s: point r1 at the constraint: status=%d body=%s", what, pointed.Code, pointed.Body.String())
		}

		read := readIssueDecisions(t, handler, issue.Key)
		view := treeRead(t, handler, issue.Key, "?view=decisions", "alice", false)
		if view.View != graphstore.QueryDecisions || view.Converted == issueCase.unconverted {
			t.Errorf("%s: ?view=decisions answers the view %q and converted %t", what, view.View, view.Converted)
		}
		nodes := requireDecisionsViewAsRead(t, what, view, read)

		// The fixture's entries, each as the case needs it.
		askNode, decisionNode := func(id string) string { return graphstore.KindAsk + ":" + id }, func(block string) string {
			return refs.BlockNodeKind + ":" + spec.address(block)
		}
		if node, held := nodes[askNode(open)]; !held || node.Ask == nil || len(node.Ask.Options) != 2 || node.Ask.Options[0].Label != "Inbox" ||
			node.Ask.Urgency != "high" || !node.Ask.Multiple || node.Decision != nil {
			t.Errorf("%s: ?view=decisions answers the open question a2 (held %t) as %s, want its two options, urgency high, multiple and no decision",
				what, held, entryJSON(t, node))
		}
		if node, held := nodes[askNode(answered)]; !held || node.Decision == nil || node.Decision.Kind != "person" {
			t.Errorf("%s: ?view=decisions answers the answered ask a1 (held %t) as %s, want a person's decision", what, held, entryJSON(t, node))
		}
		closing := spec.address("g2")
		askEntry := requireEntry(t, read.Entries, "the answered ask a1", askNode(answered))
		if !slices.ContainsFunc(askEntry.Readings, func(reading decisionReading) bool { return reading.Node.ID == closing }) {
			t.Fatalf("%s: the decisions read lists the readings %s under the answered ask a1, want the reading g2 (%s) that closes it",
				what, entryJSON(t, askEntry.Readings), closing)
		}
		if held := view.nodesNamed(refs.BlockNodeKind, closing); len(held) != 1 || held[0].Role != treeRoleQuery || held[0].Supertag != refs.ReadingSupertag ||
			held[0].Parent == nil || held[0].Parent.Kind != graphstore.KindAsk || held[0].Parent.ID != answered {
			t.Errorf("%s: ?view=decisions answers the reading g2 that closes the answered ask a1 as %s, want it once, the query's reading, under the ask's node %s",
				what, entryJSON(t, held), answered)
		}
		if node, held := nodes[decisionNode("d1")]; !held || node.Decision == nil || node.Decision.Kept == nil || node.Decision.Kept.KeptBy != "alice" ||
			node.Decision.Kept.KeptAt.IsZero() || node.Decision.WaitingReview {
			t.Errorf("%s: ?view=decisions answers the kept decision d1 (held %t) as %s, want it kept by alice, with when, and not waiting",
				what, held, entryJSON(t, node.Decision))
		}
		if node, held := nodes[decisionNode("d2")]; !held || node.Decision == nil || node.Decision.Kept != nil || !node.Decision.WaitingReview {
			t.Errorf("%s: ?view=decisions answers the decision d2 (held %t) as %s, want it waiting for review", what, held, entryJSON(t, node.Decision))
		}
		if held := view.nodesNamed(refs.BlockNodeKind, spec.address("g1")); len(held) != 1 || held[0].Supertag != refs.ReadingSupertag {
			t.Errorf("%s: ?view=decisions answers the reading g1 as %s, want it once, a reading", what, entryJSON(t, held))
		}
		for _, node := range view.Nodes {
			if node.Role != treeRoleQuery {
				continue
			}
			underDecision := node.Parent != nil && (node.Parent.Supertag == refs.DecisionSupertag || node.Parent.Kind == graphstore.KindAsk)
			if node.Kind != graphstore.KindAsk && node.Supertag != refs.DecisionSupertag && (node.Supertag != refs.ReadingSupertag || !underDecision) {
				t.Errorf("%s: ?view=decisions answers %s:%s (%s) as the query's, which is no ask, no decision and no reading of a decision or an answered ask",
					what, node.Kind, node.ID, node.Supertag)
			}
		}

		// The spec view carries the decision and the answered ask that shaped r1, and r1's
		// constraint, each with its tuple, and no ask nobody answered.
		specView := treeRead(t, handler, issue.Key, "?view=spec", "alice", false)
		requirement := treeEnd(refs.BlockNodeKind, spec.address("r1"))
		for _, tie := range []struct {
			name, kind string
			node       model.IssueTreeEnd
		}{
			{"the decision d1", refs.TupleShaped, treeEnd(refs.BlockNodeKind, spec.address("d1"))},
			{"the answered ask a1", refs.TupleShaped, treeEnd(graphstore.KindAsk, answered)},
		} {
			held := specView.nodesNamed(tie.node.Kind, tie.node.ID)
			if len(held) != 1 || held[0].Role != treeRoleContext || !specView.holdsTuple(tie.kind, tie.node, requirement) {
				t.Errorf("%s: ?view=spec answers %s as %s and its shaped tuple to r1 %t, want it once as context, with the tuple; it answers %v",
					what, tie.name, entryJSON(t, held), specView.holdsTuple(tie.kind, tie.node, requirement), specView.addresses())
			}
		}
		constrained := treeEnd(graphstore.KindConstraint, constraint.ID)
		if held := specView.nodesNamed(constrained.Kind, constrained.ID); len(held) != 1 || held[0].Role != treeRoleContext ||
			!specView.holdsTuple(graphstore.TupleConstrainedBy, requirement, constrained) {
			t.Errorf("%s: ?view=spec answers r1's constraint as %s and r1's constrained_by %t, want it once as context, with the tuple",
				what, entryJSON(t, held), specView.holdsTuple(graphstore.TupleConstrainedBy, requirement, constrained))
		}
		for _, node := range specView.Nodes {
			if node.Kind == graphstore.KindAsk && node.ID != answered || node.ID == spec.address("d2") {
				t.Errorf("%s: ?view=spec answers %s:%s, which is no answered ask and no decision that shaped one of its nodes", what, node.Kind, node.ID)
			}
		}

		// The decisions query node records no version of its own yet: a version of it is answered
		// as a version no record holds.
		requireRefused(t, what+": a version of the decisions query", treeResponse(t, handler, issue.Key, "?view=decisions&v=1", "alice", false),
			http.StatusNotFound, "QUERY_VERSION_NOT_FOUND", "decisions query of "+issue.Key+" holds no version 1")
	}
}
