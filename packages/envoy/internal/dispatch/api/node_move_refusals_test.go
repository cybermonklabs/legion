package api

// node_move_refusals_test.go holds the move of a node (R7 of the issue-tree spec: PATCH
// /api/v1/issues/{key}/nodes/{id}) to its refusals, each in the order R7 reads it, each storing
// nothing: an ask, a comment and a message, which the route moves none of, before every other rule;
// a constraint moved under any node but an issue's before the body, the If-Match and the writer; a
// body that names no move, a move that states no placement and one whose placement moved; R28's
// writer check and R32's refusal of an agent's move of a person's decision; a parent inside the
// moving subtree, outside the issue's tree and under an issue below for a node that reads its issue
// from its own row; an after that is no ranked sibling of the node's band; a parent that is, or
// stands under, the node of a document a removal could take; and a second read that meets an issue
// the attempt did not lock at each attempt.

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// moveRig is a server of the move's tests on a store of its own: its handler, its store, and the
// server, whose background writes a refusal's count waits out (quiet).
type moveRig struct {
	handler  http.Handler
	database *store.Store
	served   *server
}

// newMoveRig is a moveRig whose store holds the project project.
func newMoveRig(t *testing.T, project string) *moveRig {
	t.Helper()
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	createReferenceAPIProject(t, handler, project)
	return &moveRig{handler: handler, database: database, served: served}
}

// quiet waits until no document owes a settlement and no write the server left running past its
// answer still runs, so the counts a refusal is held to move for nothing else.
func (r *moveRig) quiet(t *testing.T) {
	t.Helper()
	awaitNoSettlementOwed(t, r.database)
	awaitBackgroundWrites(t, r.served)
}

// partOf is the far ends of the current part_of tuples of the node at the store address address,
// each `<kind>/<id>` as the store keys it, joined by commas: "" for a node that stands under none.
func (r *moveRig) partOf(t *testing.T, address string) string {
	t.Helper()
	return strings.Join(farEnds(t, r.database, refsKindOf(address), storeIDOf(address), graphstore.TuplePartOf), ",")
}

// tag is the If-Match a move of the node at the store address address names: its placement_seq as
// a quoted entity tag.
func (r *moveRig) tag(t *testing.T, address string) string {
	t.Helper()
	_, tag := placementOf(t, r.database, refsKindOf(address), storeIDOf(address))
	return tag
}

// move moves the node at address of the issue key under parent as alice, with the members of more
// beside parent and an If-Match naming its placement_seq, and fails unless the move is taken; it
// answers the move's answer.
func (r *moveRig) move(t *testing.T, key, address, parent string, more map[string]any) nodeMoved {
	t.Helper()
	body := map[string]any{"parent": parent}
	maps.Copy(body, more)
	response := patchNode(t, r.handler, key, address, r.tag(t, address), body, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("the move of %s under %s answered %d %s, want 200", address, parent, response.Code, response.Body.String())
	}
	return decodeBody[nodeMoved](t, response)
}

// tradeOffOn is alice's trade-off lens on the block of the spec of the issue key, holding findings,
// failing unless the lens route creates it; what names it in a failure.
func (r *moveRig) tradeOffOn(t *testing.T, key, block, what string, findings ...string) lensInvoked {
	t.Helper()
	body := map[string]any{"on": "dispatch://" + key + "/spec#b-" + block, "kind": "trade-off"}
	if len(findings) > 0 {
		body["findings"] = findings
	}
	return decodeBody[lensInvoked](t, created(t, what, dispatchRequest(t, r.handler, http.MethodPost, "/api/v1/issues/"+key+"/lenses", body, "alice")))
}

// refused sends the move of the node at address of the issue key with body and the If-Match tag
// (none where tag is ""), as the person login, or the shared agent bearer where login is "", and
// fails unless it answers status with code and names each of names, stores nothing - the store's
// tuples, versions, events and room updates counted the same after it (requireUnchanged) - and
// leaves the node's part_of where it stood. what names the move in a failure. It answers the
// refusal's message.
func (r *moveRig) refused(t *testing.T, what, key, address, tag string, body map[string]any, login string, status int, code string, names ...string) string {
	t.Helper()
	r.quiet(t)
	before := r.partOf(t, address)
	counts := readDeleteCounts(t, r.database)
	message := refusedWith(t, what, patchNode(t, r.handler, key, address, tag, body, login), status, code)
	for _, name := range names {
		if !strings.Contains(message, name) {
			t.Errorf("the refusal of %s names no %s: %s", what, name, message)
		}
	}
	r.quiet(t)
	requireUnchanged(t, r.database, what, counts)
	if after := r.partOf(t, address); after != before {
		t.Errorf("%s changed the part_of of %s from %s to %s", what, address, before, after)
	}
	return message
}

// moveValueOf is the current value of attribute the node kind and id holds, "" where it holds none.
func moveValueOf(t *testing.T, database *store.Store, kind, id, attribute string) string {
	t.Helper()
	var value string
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce((select case when t.asserted then t.to_id end from graph_tuples t
		                 where t.from_kind = $1 and t.from_id = $2 and t.kind = $3 and t.to_kind = $4
		                 order by t.seq desc limit 1), '')`,
		kind, id, attribute, graphstore.KindValue).Scan(&value); err != nil {
		t.Fatalf("read the %s of %s %s: %v", attribute, kind, id, err)
	}
	return value
}

// moveRefusalSpec is the spec of the refusal tests' issue: two criteria, a requirement, a decision
// alice decides, an ask and a paragraph, each block id unique to it.
const moveRefusalSpec = ":::criterion{#mk1}\nThe store answers every reader.\n:::\n\n" +
	":::criterion{#mk2}\nThe store answers every writer.\n:::\n\n" +
	":::requirement{#mr1}\nEvery read answers in a second.\n:::\n\n" +
	":::decision{#md1}\nKeep one store for every writer.\n:::\n\n" +
	":::ask{#mq1}\nWhich store do the writers take?\n:::\n\n" +
	"A paragraph the refusals leave where it stands.\n"

// TestTheMoveReadsItsFirstRulesInR7sOrder holds the move to the rules R7 reads before it takes a
// lock, in their order, each storing nothing. On an issue born converted (moveRefusalSpec):
//
//   - An ask, a comment and a message, each by its store address, are 400 NODE_NOT_MOVABLE naming
//     the node and its kind, with an empty body and no If-Match: R7 answers this once {id} is found
//     under {key}, before every other rule.
//   - A constraint under the requirement mr1 with no If-Match is 400 INVALID_POINTER naming the
//     constraint, mr1 and its kind, and under the node of U, a document a removal could take, it is
//     400 INVALID_POINTER and not 409 PARENT_DOCUMENT_REMOVABLE: R7 reads the constraint rule before
//     every other rule.
//   - A body with neither parent nor after, one whose parent names nothing and one whose after names
//     nothing are each 400 MOVE_INPUT; a move of mr1 with no If-Match is 428 PRECONDITION_REQUIRED
//     naming mr1; one whose If-Match is a placement_seq mr1 no longer holds, its own placement_seq as
//     a weak tag, or `*`, is 412 PRECONDITION_FAILED naming the placement_seq mr1 holds.
//   - The agent's move of md1, alice's decision, is 409 ANSWERED_ASK_KEPT quoting the decision and
//     naming it (R32); alice's own move of it is taken.
func TestTheMoveReadsItsFirstRulesInR7sOrder(t *testing.T) {
	t.Parallel()
	rig := newMoveRig(t, "MVREF")
	issue := createDigestIssue(t, rig.handler, rig.database, "MVREF", "The move's first refusals", moveRefusalSpec, false)
	key := issue.Key
	spec := readDecisionDocument(t, rig.database, key, "spec")
	mk1, mr1, md1 := spec.address("mk1"), spec.address("mr1"), spec.address("md1")
	ask := askOfBlock(t, rig.database, key, "mq1")
	comment := createThreadComment(t, rig.handler, key, map[string]any{"body": "A remark the move leaves on the issue."}, "alice")
	message := decodeBody[model.Comment](t, created(t, "alice's message on the issue", dispatchRequest(t, rig.handler, http.MethodPost,
		"/api/v1/issues/"+key+"/messages", map[string]any{"body": "A message the move leaves on the issue."}, "alice")))
	constraint := created201[constraintRead](t, "alice's constraint", dispatchRequest(t, rig.handler, http.MethodPost,
		"/api/v1/issues/"+key+"/constraints", map[string]any{"standard": "RFC 9110", "reference": "section 13.1.1"}, "alice"))
	u := emptyDocument(t, rig.handler, key, "Notes U.md")
	rig.quiet(t)

	for _, node := range []struct{ address, id, kind string }{
		{graphstore.KindAsk + "/" + ask, ask, graphstore.KindAsk},
		{graphstore.KindComment + "/" + comment.ID, comment.ID, graphstore.KindComment},
		{graphstore.KindMessage + "/" + message.ID, message.ID, graphstore.KindMessage},
	} {
		rig.refused(t, "the move of the "+node.kind+" "+node.address+" with an empty body and no If-Match", key, node.address, "",
			map[string]any{}, "alice", http.StatusBadRequest, codeNodeNotMovable, node.id, node.kind)
	}

	constraintNode := graphstore.KindConstraint + "/" + constraint.ID
	rig.refused(t, "the move of the constraint "+constraintNode+" under "+mr1+" with no If-Match", key, constraintNode, "",
		map[string]any{"parent": mr1}, "alice", http.StatusBadRequest, codeInvalidPointer, constraint.ID, "mr1", "requirement")
	rig.refused(t, "the move of the constraint "+constraintNode+" under the node of U", key, constraintNode, rig.tag(t, constraintNode),
		map[string]any{"parent": graphstore.KindArtifact + "/" + u.RefKey}, "alice", http.StatusBadRequest, codeInvalidPointer, constraint.ID)

	tag := rig.tag(t, mr1)
	for _, body := range []map[string]any{{}, {"parent": ""}, {"after": ""}} {
		rig.refused(t, fmt.Sprintf("the move of %s with the body %v", mr1, body), key, mr1, tag, body, "alice",
			http.StatusBadRequest, codeMoveInput)
	}
	rig.refused(t, "the move of "+mr1+" with no If-Match", key, mr1, "", map[string]any{"parent": mk1}, "alice",
		http.StatusPreconditionRequired, codePreconditionRequired, "mr1")
	held, _ := placementOf(t, rig.database, "block", mr1)
	seq := strconv.FormatInt(held.Seq, 10)
	for _, stale := range []string{`"` + strconv.FormatInt(held.Seq-1, 10) + `"`, `W/"` + seq + `"`, "*"} {
		rig.refused(t, "the move of "+mr1+" with If-Match "+stale, key, mr1, stale, map[string]any{"parent": mk1}, "alice",
			http.StatusPreconditionFailed, codePreconditionFailed, seq)
	}

	rig.refused(t, "the agent's move of alice's decision "+md1, key, md1, rig.tag(t, md1),
		map[string]any{"parent": mk1, "actor": sessionActor()}, "", http.StatusConflict, codeAnsweredAskKept, "md1", "Keep one store for every writer")
	rig.move(t, key, md1, mk1, nil)
	if parent := rig.partOf(t, md1); parent != "block/"+mk1 {
		t.Errorf("alice's move of her decision %s under %s left it under %s", md1, mk1, parent)
	}
}

// TestTheMoveRefusesAParentOutsideItsPlaceAndAnAfterOfNoSiblingOfItsBand holds the move to the
// rules R7 reads of the new parent and of after under the owner locks, each 400 INVALID_POINTER
// storing nothing. On an issue born converted (moveRefusalSpec) whose Verification document holds
// a check mc1 serving mr1, with alice's comment on mr1's text, a trade-off lens on mk1, a child
// issue and another root issue:
//
//   - mr1 under itself, and under mc1, which stands in its subtree, each refused naming the cycle.
//   - mr1 under a criterion of the other root issue, refused naming that issue and the tree of the
//     issue it leaves.
//   - mr1, a block that reads its issue from its own row, under the child issue's node, refused
//     naming mr1 and the child; the lens, whose issue follows its part_of, moves there, and the
//     child's tree read lists it.
//   - mc1 under mr1 after mk1, which is no child of mr1; after mc1 itself; after the comment, which
//     holds no rank; under the issue after mk2, a criterion of the spec's band where mc1 stands in
//     the Verification document's; and the lens with after null, which holds no rank: each names
//     after and why.
func TestTheMoveRefusesAParentOutsideItsPlaceAndAnAfterOfNoSiblingOfItsBand(t *testing.T) {
	t.Parallel()
	rig := newMoveRig(t, "MVTREE")
	issue := createDigestIssue(t, rig.handler, rig.database, "MVTREE", "The move's tree", moveRefusalSpec, false)
	key := issue.Key
	spec := readDecisionDocument(t, rig.database, key, "spec")
	mk1, mk2, mr1 := spec.address("mk1"), spec.address("mk2"), spec.address("mr1")
	uploadDocument(t, rig.handler, "/api/v1/issues/"+key, "verification.md", fmt.Sprintf(
		":::check{#mc1 serves=\"dispatch://%s/spec#b-mr1\" surface=\"api\" run=\"go test ./...\" negative_control=\"Undo the rule.\"}\nThe read answers in a second.\n:::\n", key))
	mc1 := readDecisionDocument(t, rig.database, key, model.ArtifactSlugVerification).address("mc1")
	other := createDigestIssue(t, rig.handler, rig.database, "MVTREE", "Another tree", ":::criterion{#mx1}\nA criterion of another tree.\n:::\n", false)
	child := createDigestChild(t, rig.handler, rig.database, "MVTREE", "The child", "The child's paragraph.\n", key, false)
	onR1 := decodeBody[model.Comment](t, created(t, "alice's comment on mr1's text", dispatchRequest(t, rig.handler, http.MethodPost,
		"/api/v1/issues/"+key+"/comments", map[string]any{"body": "Is a second the bound?", "anchor": map[string]any{"artifact": "spec", "quote": "answers in a second"}}, "alice")))
	lens := rig.tradeOffOn(t, key, "mk1", "a trade-off on mk1")
	lensNode := graphstore.KindLens + "/" + lens.ID
	onR1Node := graphstore.KindComment + "/" + onR1.ID
	rig.quiet(t)
	if parent := rig.partOf(t, mc1); parent != "block/"+mr1 {
		t.Fatalf("the check %s stands under %s, want %s, which it serves", mc1, parent, mr1)
	}
	paragraph, _ := textBlockOf(t, rig.database, spec.id, "paragraph", "Every read answers in a second.")
	if parent := rig.partOf(t, onR1Node); parent != "block/"+paragraph {
		t.Fatalf("alice's comment on the text of %s stands under %s, want %s, the paragraph its anchor is on", mr1, parent, paragraph)
	}
	if node := treeNodes(t, rig.handler, key, "")[onR1.ID]; node.Parent == nil || node.Parent.ID != mr1 {
		t.Fatalf("the tree read puts the comment %s under %+v, want %s, the paragraph's nearest returned ancestor", onR1.ID, node.Parent, mr1)
	}

	for _, parent := range []string{mr1, mc1} {
		rig.refused(t, "the move of "+mr1+" under "+parent, key, mr1, rig.tag(t, mr1), map[string]any{"parent": parent}, "alice",
			http.StatusBadRequest, codeInvalidPointer, "cycle")
	}
	otherCriterion := readDecisionDocument(t, rig.database, other.Key, "spec").address("mx1")
	rig.refused(t, "the move of "+mr1+" under "+otherCriterion+", a criterion of another tree", key, mr1, rig.tag(t, mr1),
		map[string]any{"parent": otherCriterion}, "alice", http.StatusBadRequest, codeInvalidPointer, other.Key, "outside the tree of "+key)
	childNode := graphstore.KindIssue + "/" + child.Key
	rig.refused(t, "the move of "+mr1+" under the node of the child "+child.Key, key, mr1, rig.tag(t, mr1),
		map[string]any{"parent": childNode}, "alice", http.StatusBadRequest, codeInvalidPointer, "mr1", child.Key)
	rig.move(t, key, lensNode, childNode, nil)
	if _, listed := treeNodes(t, rig.handler, child.Key, "")[lens.ID]; !listed {
		t.Errorf("the lens %s is not listed by the tree of %s after its move under %s", lens.ID, child.Key, childNode)
	}

	mcTag := rig.tag(t, mc1)
	for _, after := range []struct {
		what, parent string
		after        any
		why          string
	}{
		{"a node that is no child of the parent", mr1, mk1, "no ranked child"},
		{"the moved node itself", mr1, mc1, "moved node itself"},
		{"the comment on mr1's text, which holds no rank", mr1, onR1Node, "no ranked child"},
		{"mk2, a criterion of the spec's band", graphstore.KindIssue + "/" + key, mk2, "band"},
	} {
		rig.refused(t, "the move of "+mc1+" under "+after.parent+" after "+after.what, key, mc1, mcTag,
			map[string]any{"parent": after.parent, "after": after.after}, "alice", http.StatusBadRequest, codeInvalidPointer, "after", after.why)
	}
	rig.refused(t, "the move of the lens "+lensNode+" with after null", child.Key, lensNode, rig.tag(t, lensNode),
		map[string]any{"parent": childNode, "after": nil}, "alice", http.StatusBadRequest, codeInvalidPointer, "after", "holds no rank")
}

// moveRemovableSpec is the spec of TestTheMoveKeepsEveryNodeButALensOffADocumentARemovalCouldTake:
// a criterion the lenses stand on and a requirement with nothing under it.
const moveRemovableSpec = ":::criterion{#rk1}\nThe store answers every reader.\n:::\n\n" +
	":::requirement{#rr9}\nA requirement with nothing under it.\n:::\n"

// TestTheMoveKeepsEveryNodeButALensOffADocumentARemovalCouldTake holds the move to R7's rule of a
// document a removal could take: no move puts a node where a removal would leave it live behind no
// row. On an issue born converted (moveRemovableSpec) with U, a document made by a title alone, W, a
// document holding text, and the trade-off lenses L1 and L4 on rk1, each holding a finding:
//
//   - L4 onto the node of U is taken, and records U's row as its artifact_id.
//   - L1's finding directly onto the node of U is 409 PARENT_DOCUMENT_REMOVABLE naming the finding,
//     its kind and U; under L4 it is taken.
//   - rr9 under L4 is refused naming rr9 and its kind; under W it is taken; after a comment cites U,
//     which leaves U holding no writing, under L4 it is refused again.
//   - Two levels down: L1's finding under L4's finding is taken, and rr9 under L1's finding is
//     refused.
//   - The whole subtree: rr9 moves under L5, a third trade-off lens; L5 onto the node of U and under
//     L4 are each refused naming L5, its kind and U, saying its subtree holds a node of a kind that
//     may not stand there, and naming no node of that subtree.
//
// Each refusal stores nothing.
func TestTheMoveKeepsEveryNodeButALensOffADocumentARemovalCouldTake(t *testing.T) {
	t.Parallel()
	rig := newMoveRig(t, "MVDOC")
	issue := createDigestIssue(t, rig.handler, rig.database, "MVDOC", "The move's removable documents", moveRemovableSpec, false)
	key := issue.Key
	rr9 := readDecisionDocument(t, rig.database, key, "spec").address("rr9")
	u := emptyDocument(t, rig.handler, key, "Notes U.md")
	uNode := graphstore.KindArtifact + "/" + u.RefKey
	w := uploadDocument(t, rig.handler, "/api/v1/issues/"+key, "Notes W.md", "W holds writing.\n").Artifact
	wNode := graphstore.KindArtifact + "/" + w.RefKey
	first := rig.tradeOffOn(t, key, "rk1", "the first lens, holding a finding", "The first lens finds the disk full.")
	fourth := rig.tradeOffOn(t, key, "rk1", "the fourth lens, holding a finding", "The fourth lens finds the store slow.")
	if len(first.Findings) != 1 || len(fourth.Findings) != 1 {
		t.Fatalf("the first and the fourth lens hold %d and %d findings, want one each", len(first.Findings), len(fourth.Findings))
	}
	f1, f4 := first.Findings[0], fourth.Findings[0]
	l4 := graphstore.KindLens + "/" + fourth.ID
	rig.quiet(t)

	rig.move(t, key, l4, uNode, nil)
	if recorded := moveValueOf(t, rig.database, graphstore.KindLens, fourth.ID, graphstore.AttributeArtifactID); recorded != u.ID {
		t.Errorf("L4 %s records the row %q as its artifact_id after its move onto U, want U's row %s", fourth.ID, recorded, u.ID)
	}
	rig.refused(t, "the move of L1's finding "+f1.ID+" onto the node of U", key, f1.ID, rig.tag(t, f1.ID),
		map[string]any{"parent": uNode}, "alice", http.StatusConflict, codeParentDocumentRemovable, first.ID, f1.BlockID, "finding", u.RefKey)
	rig.move(t, key, f1.ID, l4, nil)
	rig.refused(t, "the move of "+rr9+" under L4", key, rr9, rig.tag(t, rr9), map[string]any{"parent": l4}, "alice",
		http.StatusConflict, codeParentDocumentRemovable, "rr9", "requirement", u.RefKey)
	rig.move(t, key, rr9, wNode, nil)
	created(t, "alice's comment citing U", dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+key+"/comments",
		map[string]any{"body": "This cites dispatch://" + key + "/artifact/" + u.Slug + " and leaves it empty."}, "alice"))
	rig.refused(t, "the move of "+rr9+" under L4 once a comment cites U", key, rr9, rig.tag(t, rr9), map[string]any{"parent": l4}, "alice",
		http.StatusConflict, codeParentDocumentRemovable, "rr9", "requirement", u.RefKey)

	rig.move(t, key, f1.ID, f4.ID, nil)
	rig.refused(t, "the move of "+rr9+" under L1's finding, two levels under L4", key, rr9, rig.tag(t, rr9), map[string]any{"parent": f1.ID}, "alice",
		http.StatusConflict, codeParentDocumentRemovable, "rr9", u.RefKey)

	fifth := rig.tradeOffOn(t, key, "rk1", "the fifth lens")
	l5 := graphstore.KindLens + "/" + fifth.ID
	rig.move(t, key, rr9, l5, nil)
	for _, parent := range []string{uNode, l4} {
		message := rig.refused(t, "the move of L5 "+l5+" under "+parent, key, l5, rig.tag(t, l5), map[string]any{"parent": parent}, "alice",
			http.StatusConflict, codeParentDocumentRemovable, fifth.ID, "lens", u.RefKey, "its subtree holds a node of a kind that may not stand there")
		if strings.Contains(message, "rr9") {
			t.Errorf("the refusal of the move of %s under %s reads %s, want the subtree's node of a kind that may not stand there, unnamed", l5, parent, message)
		}
	}
}

// TestAMoveWhoseSecondReadMeetsAnUnlockedIssueAtEachAttemptIsContended holds the move to R7's
// owner locks: it reads once with no lock held, locks every issue that read met, and reads again;
// where the second read meets a node under an issue it did not lock, it gives its locks back and
// starts over, at most parentLockAttempts times, and then answers 409 PARENT_CONTENDED storing
// nothing. On an issue whose requirement T is to move under its criterion, and two child issues
// each with a paragraph of its room, the test hook (server.betweenNodeRouteReads) pauses the move
// after each read it makes with no lock held, and alice moves one child's paragraph under T there,
// through PATCH: each second read finds T's subtree holding a node of an issue the attempt did not
// lock. The move answers 409 PARENT_CONTENDED after two attempts, changing nothing the two moves
// left, and T stands where it stood.
func TestAMoveWhoseSecondReadMeetsAnUnlockedIssueAtEachAttemptIsContended(t *testing.T) {
	t.Parallel()
	rig := newMoveRig(t, "MVRACE")
	issue := createDigestIssue(t, rig.handler, rig.database, "MVRACE", "Contended",
		":::criterion{#ck1}\nThe criterion the target moves under.\n:::\n\n:::requirement{#ct1}\nThe target of the contended move.\n:::\n", false)
	firstChild := createDigestChild(t, rig.handler, rig.database, "MVRACE", "First child", "The first child's paragraph moves under the target.\n", issue.Key, false)
	secondChild := createDigestChild(t, rig.handler, rig.database, "MVRACE", "Second child", "The second child's paragraph moves under the target.\n", issue.Key, false)
	spec := readDecisionDocument(t, rig.database, issue.Key, "spec")
	target, criterion := spec.address("ct1"), spec.address("ck1")
	moves := []struct{ key, block string }{
		{firstChild.Key, blockWithText(t, rig.database, "The first child's paragraph")},
		{secondChild.Key, blockWithText(t, rig.database, "The second child's paragraph")},
	}
	rig.quiet(t)
	before := rig.partOf(t, target)
	tag := rig.tag(t, target)
	attempts := 0
	moving := false
	var afterMoves deleteCounts
	rig.served.betweenNodeRouteReads = func(_ context.Context, attempt int) {
		if moving || attempt > len(moves) {
			return
		}
		attempts, moving = attempt, true
		defer func() { moving = false }()
		moved := moves[attempt-1]
		if response := patchNode(t, rig.handler, moved.key, moved.block, rig.tag(t, moved.block), map[string]any{"parent": target}, "alice"); response.Code != http.StatusOK {
			t.Errorf("alice's move of %s under %s answered %d %s, want 200", moved.block, target, response.Code, response.Body.String())
		}
		if attempt == len(moves) {
			awaitNoSettlementOwed(t, rig.database)
			afterMoves = readDeleteCounts(t, rig.database)
		}
	}
	response := patchNode(t, rig.handler, issue.Key, target, tag, map[string]any{"parent": criterion}, "alice")
	rig.served.betweenNodeRouteReads = nil
	if attempts != 2 {
		t.Errorf("the move of %s stopped after attempt %d, want 2", target, attempts)
	}
	refusedWith(t, "the contended move of "+target, response, http.StatusConflict, parentContended)
	rig.quiet(t)
	if now := readDeleteCounts(t, rig.database); now != afterMoves {
		t.Errorf("the contended move of %s changed the store after the two moves: %+v, want %+v", target, now, afterMoves)
	}
	if after := rig.partOf(t, target); after != before {
		t.Errorf("the contended move of %s left it under %s, want %s", target, after, before)
	}
}

// TestTheMoveHoldsR28sWriterCheck holds the move to R28's writer check (R7 reads it after the
// If-Match): on an issue Legion runs (newNodeWriterRig), the worker's move of the check c2 under r1
// is 403 DOCUMENT_WRITER_NOT_ARCHITECT, the store's tuples and the issue's versions and events
// counted the same before and after it, and c2 stays under r2, which it serves; the architect's
// move of c1 under r2, and the person's, on a token holding the person scope, of c2 under r1, are
// each taken, else printing `PATCH refused <writer>`.
func TestTheMoveHoldsR28sWriterCheck(t *testing.T) {
	t.Parallel()
	rig := newNodeWriterRig(t)
	r1, r2 := rig.spec.address("r1"), rig.spec.address("r2")
	c1, c2 := rig.verification.address("c1"), rig.verification.address("c2")
	move := func(as nodeWriterAs, address, parent string) *httptest.ResponseRecorder {
		t.Helper()
		_, tag := placementOf(t, rig.database, "block", address)
		return patchNodeAs(rig.caller(t, as), rig.issue, address, tag, map[string]any{"parent": parent})
	}
	before := rig.counts(t)
	refused := move(rig.worker, c2, r1)
	rig.quiet(t)
	if after := rig.counts(t); refused.Code != http.StatusForbidden || decodeBody[refusalAnswer](t, refused).Code != specWriterRefusalCode || after != before {
		t.Errorf("PATCH let worker %s move %s under %s: status=%d body=%s; tuples %d -> %d, versions %d -> %d, events %d -> %d",
			rig.worker.session, c2, r1, refused.Code, clippedBody(refused), before.tuples, after.tuples, before.versions, after.versions, before.events, after.events)
	}
	if held, _ := placementOf(t, rig.database, "block", c2); held.ToID != r2 {
		t.Errorf("the worker's refused move of %s left it under %s %s, want %s", c2, held.ToKind, held.ToID, r2)
	}
	for _, write := range []struct {
		as            nodeWriterAs
		check, parent string
	}{{rig.architect, c1, r2}, {rig.person, c2, r1}} {
		if response := move(write.as, write.check, write.parent); response.Code != http.StatusOK {
			t.Errorf("PATCH refused %s its move of %s under %s: status=%d body=%s", write.as.name, write.check, write.parent, response.Code, clippedBody(response))
			continue
		}
		rig.quiet(t)
		if held, _ := placementOf(t, rig.database, "block", write.check); held.ToID != write.parent {
			t.Errorf("%s's move of %s under %s left it under %s %s", write.as.name, write.check, write.parent, held.ToKind, held.ToID)
		}
	}
}
