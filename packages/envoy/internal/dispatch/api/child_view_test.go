package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A child issue is a view on its ancestor's spec (R23): it is one where its row of issue_views
// says it was born converted, its spec is not its own, and the nearest ancestor that holds
// requirements and is no view is not done (docs.ViewOf). The tests below hold the own-spec mark,
// which ends a child's chance of being a view, to its two writers - every write through the API
// that holds a requirement on an issue that is no view, and every pass of the block index that
// states one there - and the search for a view's ancestor to passing over an ancestor that is
// itself a view. TestARequirementPastedIntoAViewIsAGapOfItsOwnKind, in coverage_tuples_test.go,
// holds a requirement a client brings into a view through the room to the gap it reads as.

// requirementMarkdown is a requirement block of id holding words.
func requirementMarkdown(id, words string) string {
	return ":::requirement{#" + id + "}\n" + words + "\n:::\n"
}

// pasteRequirement writes a requirement block of id holding words at the end of the spec of issue
// through its room, as a client that skips the editor's creation gate leaves it, so no route's
// check sees it (connectBrowserPeer), and settles the issue's documents, whose settlement versions
// the spec and runs its block index (SettleIssueDocuments).
func pasteRequirement(t *testing.T, handler http.Handler, documents *docs.Service, issue model.Issue, id, words string) {
	t.Helper()
	pasted, err := pmdoc.Parse(requirementMarkdown(id, words))
	if err != nil {
		t.Fatalf("parse the requirement %s: %v", id, err)
	}
	peer := connectBrowserPeer(t, handler, documents, issue.PrimaryArtifactID)
	t.Cleanup(peer.close)
	peer.edit(t, func(tree *pmdoc.Node) error {
		tree.Children = append(tree.Children, pasted.Children...)
		return nil
	})
	peer.barrier(t)
	settleIssue(t, documents, issue.Key)
}

// insertRequirement writes a requirement block of id holding words at the end of the document
// artifactID through POST /api/v1/artifacts/{id}/edits, as alice, and answers the response.
func insertRequirement(t *testing.T, handler http.Handler, artifactID, id, words string) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": requirementMarkdown(id, words)}},
	}, "alice")
}

// giveRequirement writes the requirement id into the spec of issue through the edit route and
// holds the spec indexed at the version that edit wrote (requireIndexedAt), so the issue holds a
// requirement node.
func giveRequirement(t *testing.T, handler http.Handler, database *store.Store, issue model.Issue, id string) {
	t.Helper()
	succeeded(t, "write the requirement "+id+" into the spec of "+issue.Key,
		insertRequirement(t, handler, issue.PrimaryArtifactID, id, "The work "+issue.Key+" states."))
	requireIndexedAt(t, database, issue.PrimaryArtifactID, latestVersion(t, database, issue.PrimaryArtifactID).Number)
	if !requirementIndexed(t, database, issue.Key, id) {
		t.Fatalf("after its edit %s holds no requirement node %s", issue.Key, id)
	}
}

// requirementIndexed says whether the block index holds a requirement node id of a document of
// the issue issueKey: its document_blocks row of type requirement, which is what an ancestor's
// requirement is read by (docs.ViewOf).
func requirementIndexed(t *testing.T, database *store.Store, issueKey, id string) bool {
	t.Helper()
	var held bool
	if err := database.Pool.QueryRow(context.Background(), `
		select exists (select 1 from document_blocks d join artifacts a on a.id = d.artifact_id
		               where a.issue_key = $1 and d.type = 'requirement' and d.block_id = $2)`, issueKey, id).Scan(&held); err != nil {
		t.Fatalf("read whether %s holds the requirement node %s: %v", issueKey, id, err)
	}
	return held
}

// blockNodeOf is the block node read answers whose block id is block, and whether it answers one.
func blockNodeOf(read treeAnswer, block string) (model.IssueTreeNode, bool) {
	at := slices.IndexFunc(read.Nodes, func(node model.IssueTreeNode) bool {
		return node.Kind == refs.BlockNodeKind && node.BlockID == block
	})
	if at < 0 {
		return model.IssueTreeNode{}, false
	}
	return read.Nodes[at], true
}

// viewOfValue is view_of as a failure line names it: the issue it names, or null.
func viewOfValue(viewOf *string) string {
	if viewOf == nil {
		return "null"
	}
	return *viewOf
}

// ownRequirementGap is the first line of coverage's gaps that reads a requirement as one on a
// view, "" where none does.
func ownRequirementGap(coverage model.IssueCoverage) string {
	for _, gap := range coverage.Gaps {
		if strings.Contains(gap, viewRequirementReason) {
			return gap
		}
	}
	return ""
}

// countedAsItsOwn says whether coverage lists the requirement block as its issue's own, counted as
// every requirement is: with no check, its one gap is that it has none.
func countedAsItsOwn(coverage model.IssueCoverage, block string) bool {
	at := slices.IndexFunc(coverage.Requirements, func(requirement model.RequirementCoverage) bool { return requirement.BlockID == block })
	return at >= 0 && slices.Equal(coverage.Requirements[at].Gaps, []string{noCheckReason})
}

// TestAChildWhoseSpecIsItsOwnNeverBecomesAView is c-child-view's second Go test. A parent whose
// spec holds no requirement takes a child, born converted, whose spec holds one: the create is
// taken, since no ancestor of the child holds a requirement, and it marks the child's spec its own
// in its own transaction. The parent then takes a requirement through the edit route. The child
// is no view: its tree read answers view_of null, its coverage lists its requirement as its own,
// counted as every requirement is, with no line reading it as a requirement on a view, and a
// second requirement written on it through the same route is taken. Its row of issue_views says
// it was born converted.
func TestAChildWhoseSpecIsItsOwnNeverBecomesAView(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createReferenceAPIProject(t, handler, "OWN")
	parent := createDigestIssue(t, handler, database, "OWN", "The parent", "The parent's words.\n", false)
	const block = "cr1"
	child := createDigestChild(t, handler, database, "OWN", "The child", requirementMarkdown(block, "The child's own work."), parent.Key, false)
	giveRequirement(t, handler, database, parent, "r1")

	if read := treeRead(t, handler, child.Key, "", "alice", false); read.ViewOf != nil {
		t.Errorf("the tree read of %s answers view_of %s after %s took a requirement, want null", child.Key, *read.ViewOf, parent.Key)
	}
	coverage := readCoverage(t, handler, child.Key)
	if gap := ownRequirementGap(coverage); gap != "" {
		t.Errorf("the coverage of %s reads its own requirement %s as a gap: %s", child.Key, block, gap)
	}
	if !countedAsItsOwn(coverage, block) {
		t.Errorf("the coverage of %s lists no requirement %s of its own: %+v", child.Key, block, coverage.Requirements)
	}
	if second := insertRequirement(t, handler, child.PrimaryArtifactID, "cr2", "The child's second work."); second.Code != http.StatusOK {
		t.Errorf("a second requirement write on %s answered %d: %s", child.Key, second.Code, second.Body.String())
	}
	var converted *string
	if err := database.Pool.QueryRow(context.Background(), `select converted from issue_views where issue_key = $1`, child.Key).Scan(&converted); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("read the row of %s in issue_views: %v", child.Key, err)
	}
	if converted == nil || *converted != "born" {
		t.Errorf("the create of %s left its row of issue_views saying it was converted %v, want born", child.Key, viewOfValue(converted))
	}
}

// TestAChildsRequirementWrittenThroughTheRoomMakesItsSpecItsOwn is c-child-view's third Go test. A
// parent whose spec holds no requirement takes a child, born converted, whose spec holds one
// paragraph, so no write through the API marks the child's spec its own. A client writes a
// requirement block into the child's spec through its room and the room's settlement versions it,
// whose block index states the requirement while no ancestor of the child holds one: the index
// pass finds the child no view and marks its spec its own. The parent then takes a requirement,
// and the child is still no view: its tree read answers view_of null and its coverage lists the
// requirement as its own, with no line reading it as a requirement on a view.
func TestAChildsRequirementWrittenThroughTheRoomMakesItsSpecItsOwn(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour, browserPeers: true})
	documents := deps.Docs.(*docs.Service)
	createReferenceAPIProject(t, handler, "ROOM")
	parent := createDigestIssue(t, handler, database, "ROOM", "The parent", "The parent's words.\n", false)
	child := createDigestChild(t, handler, database, "ROOM", "The child", "The child's words.\n", parent.Key, false)
	const block = "cr1"
	pasteRequirement(t, handler, documents, child, block, "The child's own work.")
	if !requirementIndexed(t, database, child.Key, block) {
		t.Fatalf("the block index did not state the requirement %s of %s after its settlement", block, child.Key)
	}
	giveRequirement(t, handler, database, parent, "r1")

	if read := treeRead(t, handler, child.Key, "", "alice", false); read.ViewOf != nil {
		t.Errorf("the tree read of %s, whose requirement came through the room, answers view_of %s after %s took a requirement, want null",
			child.Key, *read.ViewOf, parent.Key)
	}
	coverage := readCoverage(t, handler, child.Key)
	if gap := ownRequirementGap(coverage); gap != "" {
		t.Errorf("the coverage of %s reads its requirement %s, which came through the room, as a gap: %s", child.Key, block, gap)
	}
	if !countedAsItsOwn(coverage, block) {
		t.Errorf("the coverage of %s lists no requirement %s of its own: %+v", child.Key, block, coverage.Requirements)
	}
}

// TestAChildWhoseCreateDeferredItsIndexKeepsItsSpecItsOwn is c-child-view's fourth Go test. On a
// server whose settlement waits an hour and whose document service starts no block-index
// reconciliation, a parent whose spec holds no requirement takes a child, born converted, whose
// spec holds a list of more items than a write indexes in its own request and one requirement, so
// the create leaves the room's blocks to the reconciliation (R15), the spec owed its block index,
// and the create's own mark and the pass that writes its requirement in the create's own
// transaction (docs' planTypedPasses) hold the child's spec its own. The parent then takes a
// requirement while the child's index is still deferred, and the child answers view_of null; one
// tick of the reconciliation indexes the child's spec, after which it answers view_of null again
// and its coverage lists the requirement as its own, counted as every requirement is, with no line
// reading it as a requirement on a view.
func TestAChildWhoseCreateDeferredItsIndexKeepsItsSpecItsOwn(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "DEFER")
	parent := createDigestIssue(t, handler, database, "DEFER", "The parent", "The parent's words.\n", false)
	const block = "cr1"
	spec := deferralList(itemsOverTheBlockBound, "end") + "\n" + requirementMarkdown(block, "The child's own work.")
	child := createDigestChild(t, handler, database, "DEFER", "The child", spec, parent.Key, false)
	if indexed := blockIndexVersion(t, database, child.PrimaryArtifactID); indexed != nil {
		t.Fatalf("the create of %s indexed its blocks in its own request at version %d, so no index was deferred", child.Key, *indexed)
	}
	giveRequirement(t, handler, database, parent, "r1")

	if read := treeRead(t, handler, child.Key, "", "alice", false); read.ViewOf != nil {
		t.Errorf("the tree read of %s, whose create deferred its index, answers view_of %s before the reconciliation, want null", child.Key, *read.ViewOf)
	}
	if _, err := deps.Docs.(*docs.Service).ReconcileBlockIndex(ctx, docs.BlockIndexCursors{}, nil); err != nil {
		t.Fatalf("one pass of the block-index reconciliation: %v", err)
	}
	if indexed := blockIndexVersion(t, database, child.PrimaryArtifactID); indexed == nil || !requirementIndexed(t, database, child.Key, block) {
		t.Fatalf("the reconciliation left the spec of %s owed its index", child.Key)
	}
	if read := treeRead(t, handler, child.Key, "", "alice", false); read.ViewOf != nil {
		t.Errorf("the tree read of %s, whose create deferred its index, answers view_of %s after the reconciliation, want null", child.Key, *read.ViewOf)
	}
	coverage := readCoverage(t, handler, child.Key)
	if gap := ownRequirementGap(coverage); gap != "" || !countedAsItsOwn(coverage, block) {
		t.Errorf("the coverage of %s reads its requirement %s, whose index the create deferred, as a gap: %s (it lists %+v)", child.Key, block, gap, coverage.Requirements)
	}
}

// TestEachAPIWriteSetsTheOwnSpecMark holds each write through the API other than the create to
// setting the own-spec mark itself, in its own transaction: the upload of a new document and the
// upload of a new version, each writing a child's first requirement with more blocks than a write
// indexes in its own request, so the block index leaves the child's blocks to the reconciliation;
// and the edit route, which defers no index by its size, writing it while another fragment of the
// child's room holds a browser's edit that no settlement has versioned, so the write leaves the
// room owed its index. Each child is born converted under a root that holds no requirement, so
// only the write's own mark, and the pass that writes the requirement in the write's own
// transaction while its document stays owed its block index (docs' planTypedPasses), can hold
// it. The root then takes a requirement before the reconciliation runs, and each child still
// answers view_of null and takes a requirement unrefused.
func TestEachAPIWriteSetsTheOwnSpecMark(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour, browserPeers: true})
	documents := deps.Docs.(*docs.Service)
	createReferenceAPIProject(t, handler, "MARK")
	large := func(id string) string {
		return deferralList(itemsOverTheBlockBound, "end") + "\n" + requirementMarkdown(id, "The child's own work.")
	}
	for _, write := range []struct {
		name  string
		write func(t *testing.T, child model.Issue, id string) string
	}{
		{"the upload of a new document", func(t *testing.T, child model.Issue, id string) string {
			return uploadDocument(t, handler, "/api/v1/issues/"+child.Key, "notes.md", large(id)).Artifact.ID
		}},
		{"the upload of a new version", func(t *testing.T, child model.Issue, id string) string {
			owner := "/api/v1/issues/" + child.Key + "/artifacts"
			response := dispatchRequest(t, handler, http.MethodPost, owner,
				map[string]any{"name": "spec", "content": large(id), "precondition": uploadBaseNamed(t, handler, owner, "spec")}, "alice")
			if response.Code != http.StatusCreated {
				t.Fatalf("upload a new version of the spec of %s: status=%d body=%s", child.Key, response.Code, response.Body.String())
			}
			return child.PrimaryArtifactID
		}},
		{"the edit route", func(t *testing.T, child model.Issue, id string) string {
			verification := uploadDocument(t, handler, "/api/v1/issues/"+child.Key, "verification.md", "The checks to come.\n").Artifact
			peer := connectBrowserPeer(t, handler, documents, verification.ID)
			t.Cleanup(peer.close)
			peer.edit(t, func(tree *pmdoc.Node) error {
				typed, err := pmdoc.Parse("A browser's words no settlement has versioned.\n")
				if err != nil {
					return err
				}
				tree.Children = append(tree.Children, typed.Children...)
				return nil
			})
			peer.barrier(t)
			succeeded(t, "write the requirement "+id+" into the spec of "+child.Key, insertRequirement(t, handler, child.PrimaryArtifactID, id, "The child's own work."))
			return child.PrimaryArtifactID
		}},
	} {
		t.Run(write.name, func(t *testing.T) {
			root := createDigestIssue(t, handler, database, "MARK", "The root of "+write.name, "The root's words.\n", false)
			child := createDigestChild(t, handler, database, "MARK", "The child of "+write.name, "The child's words.\n", root.Key, false)
			const block = "cr1"
			written := write.write(t, child, block)
			if indexed := blockIndexVersion(t, database, written); indexed != nil {
				t.Fatalf("%s indexed the document it wrote of %s at version %d in its own request, so no block index was left to the reconciliation", write.name, child.Key, *indexed)
			}
			giveRequirement(t, handler, database, root, "r1")
			if read := treeRead(t, handler, child.Key, "", "alice", false); read.ViewOf != nil {
				t.Errorf("the tree read of %s, whose requirement %s wrote, answers view_of %s after %s took a requirement, want null",
					child.Key, write.name, *read.ViewOf, root.Key)
			}
			if again := insertRequirement(t, handler, child.PrimaryArtifactID, "cr2", "The child's second work."); again.Code != http.StatusOK {
				t.Errorf("a requirement write on %s after %s answered %d: %s", child.Key, write.name, again.Code, again.Body.String())
			}
		})
	}
}

// TestTheSearchForAViewsAncestorPassesOverAViewAncestor: under a root that holds a requirement, a
// child born converted is a view of the root and holds a requirement a client brought in through
// its room, which does not end the view. A grandchild born converted under that child is a view of
// the root and not of the child, whose requirement is a gap and no part of a spec: its tree read
// answers view_of the root, a requirement written on it is refused naming the root, and so is a
// create under the child whose spec holds one.
func TestTheSearchForAViewsAncestorPassesOverAViewAncestor(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour, browserPeers: true})
	documents := deps.Docs.(*docs.Service)
	createReferenceAPIProject(t, handler, "PASS")
	root := createDigestIssue(t, handler, database, "PASS", "The root", requirementMarkdown("r1", "The root states it."), false)
	view := createDigestChild(t, handler, database, "PASS", "The view", "The view's design.\n", root.Key, false)
	pasteRequirement(t, handler, documents, view, "pr1", "The pasted work.")
	if !requirementIndexed(t, database, view.Key, "pr1") {
		t.Fatalf("the block index did not state the requirement pr1 of %s after its settlement", view.Key)
	}
	if read := treeRead(t, handler, view.Key, "", "alice", false); viewOfValue(read.ViewOf) != root.Key {
		t.Fatalf("after the paste the tree read of %s answers view_of %s, want %s", view.Key, viewOfValue(read.ViewOf), root.Key)
	}
	grandchild := createDigestChild(t, handler, database, "PASS", "The grandchild", "The grandchild's design.\n", view.Key, false)

	if read := treeRead(t, handler, grandchild.Key, "", "alice", false); viewOfValue(read.ViewOf) != root.Key {
		t.Errorf("the tree read of %s, under the view %s, answers view_of %s, want %s", grandchild.Key, view.Key, viewOfValue(read.ViewOf), root.Key)
	}
	requireRefused(t, "a requirement written on "+grandchild.Key, insertRequirement(t, handler, grandchild.PrimaryArtifactID, "gr1", "The grandchild's own work."),
		http.StatusConflict, codeRequirementBelongsToAncestor, root.Key)
	requireRefused(t, "a create under "+view.Key+" whose spec holds a requirement", createIssueRequest(t, handler, map[string]any{
		"project": "PASS", "title": "Another grandchild", "spec": requirementMarkdown("gr2", "Its own work."), "parent": view.Key, "force": true,
	}), http.StatusConflict, codeRequirementBelongsToAncestor, root.Key)
}

// TestAViewsSpecQueryTakesOnlyItsMemo: on a view whose spec opens with its memo callout, a client
// brings a requirement in through the room. The view's spec query takes the memo callout alone:
// `?view=spec` answers it as the query's only node, and the record of the spec query, with its
// digest, is the record the view held before the paste, so the requirement is in neither.
func TestAViewsSpecQueryTakesOnlyItsMemo(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour, browserPeers: true})
	documents := deps.Docs.(*docs.Service)
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "MEMO")
	parent := createDigestIssue(t, handler, database, "MEMO", "The parent", requirementMarkdown("r1", "The parent states it."), false)
	child := createDigestChild(t, handler, database, "MEMO", "The view",
		":::callout{#memo}\nThe view builds the list.\n:::\n\nThe view's design.\n", parent.Key, false)
	before, err := docs.ReadQueryRecord(ctx, database.Pool, child.Key, graphstore.QuerySpec, refs.TreeCut{})
	if err != nil {
		t.Fatalf("read the spec record of %s: %v", child.Key, err)
	}
	if len(before.Nodes) != 1 {
		t.Fatalf("before the paste the spec record of %s holds %d nodes, want the memo alone", child.Key, len(before.Nodes))
	}
	pasteRequirement(t, handler, documents, child, "pr1", "The pasted work.")
	if !requirementIndexed(t, database, child.Key, "pr1") {
		t.Fatalf("the block index did not state the requirement pr1 of %s after its settlement", child.Key)
	}

	read := treeRead(t, handler, child.Key, "?view=spec", "alice", false)
	var taken []string
	for _, node := range read.Nodes {
		if node.Role == "query" {
			taken = append(taken, node.BlockID+" "+node.Supertag)
		}
	}
	if !slices.Equal(taken, []string{"memo callout"}) {
		t.Errorf("?view=spec of the view %s answers as the query's %q, want the memo callout alone", child.Key, taken)
	}
	after, err := docs.ReadQueryRecord(ctx, database.Pool, child.Key, graphstore.QuerySpec, refs.TreeCut{})
	if err != nil {
		t.Fatalf("read the spec record of %s: %v", child.Key, err)
	}
	if after.Digest != before.Digest || !bytes.Equal(after.Record, before.Record) {
		t.Errorf("after the paste the spec record of %s holds %d nodes with the digest %s, want the record before the paste, %d node with %s",
			child.Key, len(after.Nodes), after.Digest, len(before.Nodes), before.Digest)
	}
}
