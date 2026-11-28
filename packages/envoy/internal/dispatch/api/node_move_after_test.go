package api

// node_move_after_test.go holds the move of a node (R7 of the issue-tree spec: PATCH
// /api/v1/issues/{key}/nodes/{id}) to the sibling its answer names as after where a text edit
// removed a block of its parent: the removed block keeps its part_of and its rank, but the issue's
// head holds it as no child of the parent, so it is no sibling (INFRA-1098's architect, 2026-11-28,
// dispatch://INFRA-1253/comment/f0e8c7f5-945b-4122-b605-071fbd3d0c2e). The answer's after names
// the nearest sibling before the node that a tree read at the same head holds, or null; no after
// names the removed block; and the memo, which after null puts a node of the spec's band right
// after, is the first sibling of that band the head holds.

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/testwait"
)

// rankedChildrenRead is the id of each node GET /api/v1/issues/{key}/tree, read as alice with no
// view, answers under the node kind and id that holds a rank, in the read's order: the siblings a
// reader of the tree finds there. It reads until no document of the issue owes its block index,
// so the read answers every block the store holds, and fails where the read takes more than one
// page.
func rankedChildrenRead(t *testing.T, handler http.Handler, key, kind, id string) []string {
	t.Helper()
	var read model.IssueTreeRead
	testwait.Eventually(t, "the tree read of "+key+" to owe no document its block index", func() bool {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/tree", nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("read the tree of %s: status=%d body=%s", key, response.Code, response.Body.String())
		}
		read = decodeBody[model.IssueTreeRead](t, response)
		return len(read.OwedDocuments) == 0
	})
	if read.Cursor != nil {
		t.Fatalf("the tree read of %s answered more than one page", key)
	}
	var children []string
	for _, node := range read.Nodes {
		if node.Parent != nil && node.Parent.Kind == kind && node.Parent.ID == id && node.Rank != nil {
			children = append(children, node.ID)
		}
	}
	return children
}

// awaitGone waits until the store holds the block node address gone, as the block index states a
// block the newest version of its document dropped.
func awaitGone(t *testing.T, rig *moveRig, address string) {
	t.Helper()
	testwait.Eventually(t, "the node of "+address+" to be gone", func() bool {
		node, held, err := graphstore.ReadNode(context.Background(), rig.database.Pool, refs.BlockNodeKind, address)
		if err != nil {
			t.Fatalf("read the node of %s: %v", address, err)
		}
		return held && node.State == graphstore.StateGone
	})
}

// requireAfterRead fails unless answer, the after a move of moved answered, names the sibling
// children, a tree read's ranked children of the node's parent, answer right before moved, and is
// null where moved stands first among them; what names the move in a failure.
func requireAfterRead(t *testing.T, what string, answer *string, moved string, children []string) {
	t.Helper()
	at := slices.Index(children, moved)
	if at < 0 {
		t.Fatalf("%s: the tree read answers %v under the parent, which holds no %s", what, children, moved)
	}
	var want *string
	if at > 0 {
		want = &children[at-1]
	}
	if (answer == nil) != (want == nil) || answer != nil && *answer != *want {
		t.Errorf("%s answers after %s, want %s, the sibling a tree read at the same head answers right before %s (%v)",
			what, afterWords(answer), afterWords(want), moved, children)
	}
}

// TestTheMoveAnswersAsAfterTheNearestSiblingTheHeadHolds holds the answer of the move to R7's
// after, "the ranked sibling the node now follows, or null where it follows none", where the
// sibling the node's place follows by rank is a block a text edit removed. On an issue born
// converted whose spec holds a criterion ak1 and the requirements ar1 and ar2, each naming ak1 as
// its parent, and whose Verification document holds a check ac1 serving ar1, alice removes ar2
// through the edit route: its node is gone, and it keeps its part_of to ak1 and its rank. She then
// moves ac1 under ak1 with after null, which puts it first in the Verification document's band,
// after ar2 by rank. The answer's after is ar1, the sibling a tree read at the same head answers
// right before ac1, and the read answers no ar2. Last, a move of ar1 whose after names ar2, which
// ranks in ar1's own band under ar1's own parent, is 400 INVALID_POINTER naming after, as an after
// that is no ranked child of the parent is, and stores nothing.
func TestTheMoveAnswersAsAfterTheNearestSiblingTheHeadHolds(t *testing.T) {
	t.Parallel()
	rig := newMoveRig(t, "MVAFTER")
	issue := createDigestIssue(t, rig.handler, rig.database, "MVAFTER", "The sibling a move follows",
		":::criterion{#ak1}\nThe store answers every reader.\n:::\n", false)
	key := issue.Key
	criterion := "dispatch://" + key + "/spec#b-ak1"
	if written := editDocument(t, rig.handler, issue.PrimaryArtifactID, insertAtEnd(
		":::requirement{#ar1 parent=\""+criterion+"\"}\nEvery read answers in a second.\n:::\n\n"+
			":::requirement{#ar2 parent=\""+criterion+"\"}\nThe requirement the next version removes.\n:::\n")); written.Code != http.StatusOK {
		t.Fatalf("alice writes ar1 and ar2 under ak1 into the spec of %s: status=%d body=%s", key, written.Code, written.Body.String())
	}
	uploadDocument(t, rig.handler, "/api/v1/issues/"+key, "verification.md", fmt.Sprintf(
		":::check{#ac1 serves=\"dispatch://%s/spec#b-ar1\" surface=\"api\" run=\"go test ./...\" negative_control=\"Undo the rule.\"}\nThe read answers in a second.\n:::\n", key))
	spec := readDecisionDocument(t, rig.database, key, "spec")
	ak1, ar1, ar2 := spec.address("ak1"), spec.address("ar1"), spec.address("ar2")
	ac1 := readDecisionDocument(t, rig.database, key, model.ArtifactSlugVerification).address("ac1")
	rig.quiet(t)
	for _, requirement := range []string{ar1, ar2} {
		if parent := rig.partOf(t, requirement); parent != "block/"+ak1 {
			t.Fatalf("%s stands under %s, want block/%s, which its parent attribute names", requirement, parent, ak1)
		}
	}

	if removed := editDocument(t, rig.handler, issue.PrimaryArtifactID,
		map[string]any{"ops": []map[string]any{{"op": "delete", "block": "ar2"}}}); removed.Code != http.StatusOK {
		t.Fatalf("alice removes ar2 from the spec of %s: status=%d body=%s", key, removed.Code, removed.Body.String())
	}
	awaitGone(t, rig, ar2)
	rig.quiet(t)
	// The case: the removed block keeps its part_of and its rank, the ones the move reads.
	if parent := rig.partOf(t, ar2); parent != "block/"+ak1 {
		t.Fatalf("the removed %s stands under %s, want block/%s, which it kept", ar2, parent, ak1)
	}
	removedRank := moveValueOf(t, rig.database, refs.BlockNodeKind, ar2, refs.AttributeRank)
	if removedRank == "" {
		t.Fatalf("the removed %s holds no rank, want the one it kept", ar2)
	}

	what := "the move of " + ac1 + " under " + ak1 + " with after null"
	response := patchNode(t, rig.handler, key, ac1, rig.tag(t, ac1), map[string]any{"parent": ak1, "after": nil}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("%s answered %d %s, want 200", what, response.Code, response.Body.String())
	}
	answer := decodeBody[nodeMoved](t, response)
	if rank := moveValueOf(t, rig.database, refs.BlockNodeKind, ac1, refs.AttributeRank); rank <= removedRank {
		t.Fatalf("%s gave it the rank %q, at or before the rank %q of the removed %s: the move is not to the place after it", what, rank, removedRank, ar2)
	}
	rig.quiet(t)
	children := rankedChildrenRead(t, rig.handler, key, refs.BlockNodeKind, ak1)
	if slices.Contains(children, ar2) {
		t.Errorf("the tree read answers the removed %s under %s: %v", ar2, ak1, children)
	}
	requireAfterRead(t, what, answer.After, ac1, children)
	if answer.After == nil || *answer.After != ar1 {
		t.Errorf("%s answers after %s, want %s, the nearest sibling before it that the issue's head holds", what, afterWords(answer.After), ar1)
	}

	rig.refused(t, "the move of "+ar1+" after "+ar2+", which a text edit removed", key, ar1, rig.tag(t, ar1),
		map[string]any{"after": ar2}, "alice", http.StatusBadRequest, codeInvalidPointer, "after", "no ranked child", "text edit removed")
}

// memoAfterRemovalSpec is the spec of TestAMoveWithAfterNullStandsAfterTheMemoPastABlockATextEditRemoved:
// a paragraph the test removes, then a callout, which is the issue's memo once the paragraph is
// gone, and two criteria.
const memoAfterRemovalSpec = "A paragraph the next version removes.\n\n:::callout{#bm}\nThe memo of the issue.\n:::\n\n" +
	":::criterion{#bk1}\nThe store answers every reader.\n:::\n\n:::criterion{#bk2}\nThe store answers every writer.\n:::\n"

// TestAMoveWithAfterNullStandsAfterTheMemoPastABlockATextEditRemoved holds R7's after null for a
// node of the spec's band under the issue - "right after the memo, so the memo stays the issue's
// first child" - to the memo the issue's head holds where a text edit removed the block that ranked
// before it. On an issue born converted whose spec opens with a paragraph, then the callout bm and
// the criteria bk1 and bk2, alice removes the paragraph through the edit route: its node is gone
// and keeps its rank, before bm's, and bm opens the spec, the memo. She moves bk2 with after null:
// the answer's after is bm, the sibling a tree read at the same head answers right before bk2, and
// that read answers bm first among the issue's ranked children and bk2 right after it.
func TestAMoveWithAfterNullStandsAfterTheMemoPastABlockATextEditRemoved(t *testing.T) {
	t.Parallel()
	rig := newMoveRig(t, "MVMEMO")
	issue := createDigestIssue(t, rig.handler, rig.database, "MVMEMO", "The memo a move follows", memoAfterRemovalSpec, false)
	key := issue.Key
	spec := readDecisionDocument(t, rig.database, key, "spec")
	memo, bk2 := spec.address("bm"), spec.address("bk2")
	paragraph, paragraphID := textBlockOf(t, rig.database, spec.id, "paragraph", "A paragraph the next version removes.")
	rig.quiet(t)

	if removed := editDocument(t, rig.handler, issue.PrimaryArtifactID,
		map[string]any{"ops": []map[string]any{{"op": "delete", "block": paragraphID}}}); removed.Code != http.StatusOK {
		t.Fatalf("alice removes the paragraph %s from the spec of %s: status=%d body=%s", paragraph, key, removed.Code, removed.Body.String())
	}
	awaitGone(t, rig, paragraph)
	rig.quiet(t)
	// The case: the removed paragraph keeps its part_of and a rank of the spec's band before the
	// memo's.
	if parent := rig.partOf(t, paragraph); parent != graphstore.KindIssue+"/"+key {
		t.Fatalf("the removed paragraph %s stands under %s, want %s/%s, which it kept", paragraph, parent, graphstore.KindIssue, key)
	}
	if removedRank, memoRank := moveValueOf(t, rig.database, refs.BlockNodeKind, paragraph, refs.AttributeRank),
		moveValueOf(t, rig.database, refs.BlockNodeKind, memo, refs.AttributeRank); removedRank == "" || removedRank >= memoRank {
		t.Fatalf("the removed paragraph %s holds the rank %q and the memo %s %q, want the paragraph's first", paragraph, removedRank, memo, memoRank)
	}

	what := "the move of " + bk2 + " with after null"
	response := patchNode(t, rig.handler, key, bk2, rig.tag(t, bk2), map[string]any{"after": nil}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("%s answered %d %s, want 200", what, response.Code, response.Body.String())
	}
	answer := decodeBody[nodeMoved](t, response)
	rig.quiet(t)
	children := rankedChildrenRead(t, rig.handler, key, graphstore.KindIssue, key)
	if len(children) < 2 || children[0] != memo || children[1] != bk2 {
		t.Errorf("after %s the tree read answers the issue's ranked children %v, want the memo %s first and %s right after it", what, children, memo, bk2)
	}
	requireAfterRead(t, what, answer.After, bk2, children)
	if answer.After == nil || *answer.After != memo {
		t.Errorf("%s answers after %s, want the memo %s", what, afterWords(answer.After), memo)
	}
}
