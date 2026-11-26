package api

// node_route_rules_test.go holds the move and the delete of a node (R7 of the issue-tree spec) to
// rules a case of their own shows: the subtree of a moved node read whole whatever its kinds sort
// by, a cycle through a finding, the far end a part_of under a finding names, the parent attribute
// of a move that keeps its parent, R28's writer check read under the owner locks, a body the
// decoder took read again the same way, the dry run's query, and the delete's own words for a
// container it would empty.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// nodeRouteRulesSpec is the spec of the lens cases: a criterion the lenses stand on, and a
// paragraph that holds no block under it.
const nodeRouteRulesSpec = ":::criterion{#nk1}\nThe store answers every reader.\n:::\n\n" +
	"The paragraph that moves under the lens.\n"

// nodeRouteRulesRig is an issue born converted (nodeRouteRulesSpec) of a moveRig of its own, with
// alice's trade-off lens on nk1 holding one finding.
type nodeRouteRulesRig struct {
	*moveRig
	key     string
	lens    lensInvoked
	finding lensFinding
}

func newNodeRouteRulesRig(t *testing.T, project string) nodeRouteRulesRig {
	t.Helper()
	rig := newMoveRig(t, project)
	key := createDigestIssue(t, rig.handler, rig.database, project, "The node routes' rules", nodeRouteRulesSpec, false).Key
	lens := rig.lensOnNk1(t, key, "a trade-off on nk1 holding a finding", "The lens finds the store slow.")
	if len(lens.Findings) != 1 {
		t.Fatalf("the lens on nk1 holds %d findings, want one", len(lens.Findings))
	}
	return nodeRouteRulesRig{moveRig: rig, key: key, lens: lens, finding: lens.Findings[0]}
}

// lensOnNk1 is alice's trade-off lens on nk1 of the issue key, holding findings.
func (r *moveRig) lensOnNk1(t *testing.T, key, what string, findings ...string) lensInvoked {
	t.Helper()
	body := map[string]any{"on": "dispatch://" + key + "/spec#b-nk1", "kind": "trade-off"}
	if len(findings) > 0 {
		body["findings"] = findings
	}
	return decodeBody[lensInvoked](t, created(t, what, dispatchRequest(t, r.handler, http.MethodPost, "/api/v1/issues/"+key+"/lenses", body, "alice")))
}

// TestAMoveReadsTheWholeSubtreeWhateverItsKindsSortBy holds the move to R7's rule of a document a
// removal could take on a lens whose subtree holds a node of a kind that sorts before a lens's: the
// move's read answers its subtree ordered by kind, so the moved lens is not its first node. alice
// moves the paragraph P under a lens L that holds no finding; L onto the node of U, a document made
// by a title alone, is then 409 PARENT_DOCUMENT_REMOVABLE naming L, its kind and U and saying its
// subtree holds a node of a kind that may not stand there, and stores nothing.
func TestAMoveReadsTheWholeSubtreeWhateverItsKindsSortBy(t *testing.T) {
	t.Parallel()
	rig := newMoveRig(t, "MVSORT")
	key := createDigestIssue(t, rig.handler, rig.database, "MVSORT", "A lens over a paragraph", nodeRouteRulesSpec, false).Key
	u := emptyDocument(t, rig.handler, key, "Notes U.md")
	lens := rig.lensOnNk1(t, key, "a trade-off on nk1 holding no finding")
	lensNode := graphstore.KindLens + "/" + lens.ID
	paragraph := blockWithText(t, rig.database, "The paragraph that moves under the lens")
	rig.quiet(t)
	rig.move(t, key, paragraph, lensNode, nil)
	if parent := rig.partOf(t, paragraph); parent != lensNode {
		t.Fatalf("the move of %s under %s left it under %s", paragraph, lensNode, parent)
	}
	rig.refused(t, "the move of "+lensNode+", holding the paragraph "+paragraph+", onto the node of U", key, lensNode, rig.tag(t, lensNode),
		map[string]any{"parent": graphstore.KindArtifact + "/" + u.RefKey}, "alice", http.StatusConflict, codeParentDocumentRemovable,
		lens.ID, "lens", u.RefKey, "its subtree holds a node of a kind that may not stand there")
}

// TestAMoveUnderAFindingOfItsOwnSubtreeIsACycle holds the move to R7's cycle rule where the parent
// is a finding, which the move's read names lens_block and the store keys as a block: the move of
// the lens L under its own finding is 400 INVALID_POINTER naming the cycle, and stores nothing.
func TestAMoveUnderAFindingOfItsOwnSubtreeIsACycle(t *testing.T) {
	t.Parallel()
	rig := newNodeRouteRulesRig(t, "MVCYCLE")
	lensNode := graphstore.KindLens + "/" + rig.lens.ID
	rig.refused(t, "the move of "+lensNode+" under its own finding "+rig.finding.ID, rig.key, lensNode, rig.tag(t, lensNode),
		map[string]any{"parent": rig.finding.ID}, "alice", http.StatusBadRequest, codeInvalidPointer, "cycle")
	if parent := rig.partOf(t, rig.finding.ID); parent != lensNode {
		t.Errorf("the finding %s stands under %s after the refused move, want %s", rig.finding.ID, parent, lensNode)
	}
}

// TestAMoveUnderAFindingNamesItAsTheReasoningRoutesDo holds the part_of a move writes under a
// finding to the far end every reader of an edge into a finding reads (farEndOf): lens_block at the
// finding's own id, as the lens route names a lens invoked on a finding. alice moves a second lens
// L2 under the finding F of L; L2's part_of names lens_block F, and the lens read of F lists L2.
func TestAMoveUnderAFindingNamesItAsTheReasoningRoutesDo(t *testing.T) {
	t.Parallel()
	rig := newNodeRouteRulesRig(t, "MVFIND")
	second := rig.lensOnNk1(t, rig.key, "a second trade-off on nk1")
	secondNode := graphstore.KindLens + "/" + second.ID
	rig.quiet(t)
	rig.move(t, rig.key, secondNode, rig.finding.ID, nil)
	if parent, want := rig.partOf(t, secondNode), graphstore.KindLensBlock+"/"+rig.finding.ID; parent != want {
		t.Errorf("the move of %s under the finding %s left its part_of at %s, want %s", secondNode, rig.finding.ID, parent, want)
	}
	response := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/lenses?on="+url.QueryEscape(rig.finding.Address), nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("the lens read of %s answered %d %s", rig.finding.Address, response.Code, response.Body.String())
	}
	var listed []string
	for _, lens := range decodeBody[nodeLenses](t, response).Lenses {
		listed = append(listed, lens.Address)
	}
	if !strings.Contains(strings.Join(listed, " "), second.Address) {
		t.Errorf("the lens read of the finding %s lists %v, want %s, which the move put under it", rig.finding.Address, listed, second.Address)
	}
}

// TestAMoveThatKeepsItsParentKeepsTheParentAttribute holds the move's text to R7 where the body
// names after alone: the node keeps its parent, so a typed block's parent attribute keeps naming
// it. On an issue that has not converted, whose spec holds the criterion pk1 and the requirements
// pr1 and pr2, each naming pk1 as its parent, alice moves pr2 first among pk1's ranked children
// with after null; pr2 still stands under pk1, and its line in the spec's head text still names pk1
// as its parent.
func TestAMoveThatKeepsItsParentKeepsTheParentAttribute(t *testing.T) {
	t.Parallel()
	rig := newMoveRig(t, "MVKEEP")
	issue := createDigestIssue(t, rig.handler, rig.database, "MVKEEP", "The parent a move keeps", "The opening paragraph.\n", true)
	key := issue.Key
	parent := "dispatch://" + key + "/spec#b-pk1"
	if written := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": ":::criterion{#pk1}\nThe criterion both requirements stand under.\n:::\n\n" +
			":::requirement{#pr1 parent=\"" + parent + "\"}\nThe first requirement under the criterion.\n:::\n\n" +
			":::requirement{#pr2 parent=\"" + parent + "\"}\nThe requirement that moves first.\n:::\n"}},
	}, "alice"); written.Code != http.StatusOK {
		t.Fatalf("alice writes the blocks into the spec of %s: status=%d body=%s", key, written.Code, written.Body.String())
	}
	spec := readDecisionDocument(t, rig.database, key, "spec")
	pk1, pr2 := spec.address("pk1"), spec.address("pr2")
	rig.quiet(t)
	if held := rig.partOf(t, pr2); held != "block/"+pk1 {
		t.Fatalf("pr2 %s stands under %s before the move, want block/%s, which its parent attribute names", pr2, held, pk1)
	}
	response := patchNode(t, rig.handler, key, pr2, rig.tag(t, pr2), map[string]any{"after": nil}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("the move of %s first among its siblings answered %d %s, want 200", pr2, response.Code, response.Body.String())
	}
	rig.quiet(t)
	if held := rig.partOf(t, pr2); held != "block/"+pk1 {
		t.Errorf("the move of %s with after alone left it under %s, want block/%s", pr2, held, pk1)
	}
	if line := lineWith(artifactTextOf(t, rig.handler, issue.PrimaryArtifactID), "requirement{#pr2"); !strings.Contains(line, `parent="`+parent+`"`) {
		t.Errorf("the move of %s with after alone took its parent attribute out: its line reads %q, want it naming %s", pr2, line, parent)
	}
}

// TestTheMoveChecksItsWriterUnderTheOwnerLocks holds R28's writer check of a move to the issue's
// owner lock, as every writer check reads it (refuseSpecWriterOfNode): on an issue Legion runs
// (newNodeWriterRig), the daemon republishes the issue's role holders without the architect while
// the architect's move of the check c1 under r2 waits for its locks (server.betweenNodeRouteReads,
// after the move's first read); the move is then 403 DOCUMENT_WRITER_NOT_ARCHITECT, and c1 stays
// under r1.
func TestTheMoveChecksItsWriterUnderTheOwnerLocks(t *testing.T) {
	t.Parallel()
	rig := newNodeWriterRig(t)
	r1, r2, c1 := rig.spec.address("r1"), rig.spec.address("r2"), rig.verification.address("c1")
	held, tag := placementOf(t, rig.database, "block", c1)
	if held.ToID != r1 {
		t.Fatalf("c1 %s stands under %s %s, want r1 %s", c1, held.ToKind, held.ToID, r1)
	}
	republished := false
	rig.served.betweenNodeRouteReads = func(_ context.Context, _ int) {
		if republished {
			return
		}
		republished = true
		if response := putRoleHoldersAs(t, rig.handler, rig.issue, map[string]any{
			"holders": []issueRoleHolder{{Role: "implementer", Session: rig.worker.session}},
			"actor":   daemonSessionOf("CORE"),
		}); response.Code != http.StatusOK {
			t.Errorf("the daemon republishes the role holders of %s without the architect: status=%d body=%s", rig.issue, response.Code, response.Body.String())
		}
	}
	response := serveNodeRequest(t, rig.handler, http.MethodPatch, "/api/v1/issues/"+rig.issue+"/nodes/"+url.PathEscape(c1),
		map[string]any{"parent": r2, "actor": map[string]any{"kind": "session", "id": rig.architect.session}},
		http.Header{"Authorization": {"Bearer agent-token"}, "If-Match": {tag}})
	rig.served.betweenNodeRouteReads = nil
	if !republished {
		t.Fatalf("the move of %s never paused for its locks: status=%d body=%s", c1, response.Code, response.Body.String())
	}
	if response.Code != http.StatusForbidden || decodeBody[refusalAnswer](t, response).Code != specWriterRefusalCode {
		t.Errorf("the move of %s by %s, no architect once its locks are held, answered %d %s, want 403 %s",
			c1, rig.architect.session, response.Code, response.Body.String(), specWriterRefusalCode)
	}
	rig.quiet(t)
	if after, _ := placementOf(t, rig.database, "block", c1); after.ToID != r1 {
		t.Errorf("the refused move left %s under %s %s, want r1 %s", c1, after.ToKind, after.ToID, r1)
	}
}

// TestAMoveBodyFollowedByAClosingBraceIsReadAsTheValueBeforeIt holds the move's second read of its
// body's members to the first read's (readJSONBody, which takes a value followed by a closing
// bracket or brace): a body that names no move followed by a stray `}` is 400 MOVE_INPUT, as the
// same body without it is, and a body naming the parent mk1 followed by one moves mr1 there.
func TestAMoveBodyFollowedByAClosingBraceIsReadAsTheValueBeforeIt(t *testing.T) {
	t.Parallel()
	rig := newMoveRig(t, "MVBRACE")
	key := createDigestIssue(t, rig.handler, rig.database, "MVBRACE", "A body with a stray brace", moveRefusalSpec, false).Key
	spec := readDecisionDocument(t, rig.database, key, "spec")
	mk1, mr1 := spec.address("mk1"), spec.address("mr1")
	rig.quiet(t)
	send := func(body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPatch, "/api/v1/issues/"+key+"/nodes/"+url.PathEscape(mr1), bytes.NewReader([]byte(body)))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Dispatch-User", "alice")
		request.Header.Set("If-Match", rig.tag(t, mr1))
		response := httptest.NewRecorder()
		rig.handler.ServeHTTP(response, request)
		return response
	}
	refusedWith(t, "the move of "+mr1+" with the body {}}", send(`{}}`), http.StatusBadRequest, codeMoveInput)
	if moved := send(`{"parent":` + strconv.Quote(mk1) + `}}`); moved.Code != http.StatusOK {
		t.Errorf("the move of %s under %s with a stray closing brace answered %d %s, want 200", mr1, mk1, moved.Code, moved.Body.String())
	}
	rig.quiet(t)
	if parent := rig.partOf(t, mr1); parent != "block/"+mk1 {
		t.Errorf("the move of %s under %s left it under %s", mr1, mk1, parent)
	}
}

// TestTheDryRunReadsNoExpectedCount holds the delete's dry run to its query (nodeDeleteQuery):
// expected_count is the delete's alone, so a dry run that names one it could not take, -1, answers
// the count a dry run naming none answers.
func TestTheDryRunReadsNoExpectedCount(t *testing.T) {
	t.Parallel()
	rig := newMoveRig(t, "DRYCOUNT")
	key := createDigestIssue(t, rig.handler, rig.database, "DRYCOUNT", "A dry run's query", moveRefusalSpec, false).Key
	mr1 := readDecisionDocument(t, rig.database, key, "spec").address("mr1")
	rig.quiet(t)
	alice := personCaller(t, rig.handler, "alice")
	want := deletedCount(t, "alice's dry run of the delete of "+mr1, deleteNodeAs(alice, key, mr1, true, -1, ""))
	named := alice(http.MethodDelete, deleteNodePath(key, mr1, true, -1)+"&expected_count=-1", nil, http.Header{})
	if count := deletedCount(t, "alice's dry run of the delete of "+mr1+" naming expected_count -1", named); count != want {
		t.Errorf("the dry run of the delete of %s naming expected_count -1 answered %d, want %d", mr1, count, want)
	}
}

// TestTheDeleteNamesAContainerItWouldEmptyInItsOwnWords holds the delete's 409
// CONTAINER_WOULD_EMPTY to the delete's own words: it names the container, quoted, and its kind,
// says what the delete takes out of it, and says nothing of a move, for a callout and for a list
// item alike; where the error names the document whose text holds the container, the refusal
// names that document by the address of a block of it the delete takes, and where it names none,
// no document.
func TestTheDeleteNamesAContainerItWouldEmptyInItsOwnWords(t *testing.T) {
	t.Parallel()
	root := moveNode{named: "dispatch://DEL-1/spec#b-r7"}
	takes := []deleteReached{{id: "DEL-1/notes#p1", document: "document-of-notes"}, {id: "DEL-1/spec#p2", document: "document-of-the-spec"}}
	for _, emptied := range []pmdoc.ContainerEmptiedError{
		{Container: "y", Type: "callout"}, {Container: "m", Type: "list_item"}, {Container: "y2", Type: "callout", Document: "document-of-the-spec"},
	} {
		var refusal *apiError
		if err := containerWouldEmpty(root, takes, fmt.Errorf("delete the blocks of %s: %w", root.named, &emptied)); !errors.As(err, &refusal) ||
			refusal.status != http.StatusConflict || refusal.code != codeContainerWouldEmpty {
			t.Fatalf("the delete's answer to %+v is %v, want 409 %s", emptied, err, codeContainerWouldEmpty)
		}
		message := refusal.message
		kind := strings.ReplaceAll(emptied.Type, "_", " ")
		if strings.Contains(message, "move") || !strings.Contains(message, strconv.Quote(emptied.Container)) || !strings.Contains(message, kind) ||
			!strings.Contains(message, root.named) {
			t.Errorf("the delete's refusal for the %s %q reads %q, want the delete's words naming %s, the %s and its id, and no move",
				kind, emptied.Container, message, root.named, kind)
		}
		names := strings.Contains(message, "of DEL-1/spec ") || strings.Contains(message, "of DEL-1/notes ")
		if named := emptied.Document != ""; names != named || named && !strings.Contains(message, "of DEL-1/spec ") {
			t.Errorf("the delete's refusal for the %s %q of the document %q reads %q, want it naming DEL-1/spec exactly where the error names that document",
				kind, emptied.Container, emptied.Document, message)
		}
	}
}

// TestTheTreeReadRefusesAVersionPastTheColumn holds the tree read's v to the version numbers a
// version's column holds (parseVersionNumber): a v past the largest integer that column holds is
// 400 INVALID_QUERY, as a v of 0 is, and no read is made with it.
func TestTheTreeReadRefusesAVersionPastTheColumn(t *testing.T) {
	t.Parallel()
	rig := newMoveRig(t, "TREEV")
	key := createDigestIssue(t, rig.handler, rig.database, "TREEV", "A version past the column", moveRefusalSpec, false).Key
	for _, version := range []string{"0", strconv.Itoa(maxIntegerColumn + 1)} {
		response := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/issues/"+key+"/tree?view=spec&v="+version, nil, "alice")
		if refusal := decodeBody[refusalAnswer](t, response); response.Code != http.StatusBadRequest || refusal.Code != "INVALID_QUERY" {
			t.Errorf("the tree read of %s at v=%s answered %d %s, want 400 INVALID_QUERY", key, version, response.Code, response.Body.String())
		}
	}
}
