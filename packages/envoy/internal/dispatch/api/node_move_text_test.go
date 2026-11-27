package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// textBlockOf is the node address and the block id of the block of type typ in the text of the
// document artifactID whose excerpt holds words, failing the test where none does.
func textBlockOf(t *testing.T, database *store.Store, artifactID, typ, words string) (string, string) {
	t.Helper()
	var address, id string
	if err := database.Pool.QueryRow(context.Background(), `
		select d.node_address, d.block_id from document_blocks d
		where `+refs.BlockDocumentSQL("d")+` = $1::uuid and d.type = $2 and strpos(d.excerpt, $3) > 0 and d.node_address is not null
		order by d.block_id limit 1`, artifactID, typ, words).Scan(&address, &id); err != nil {
		t.Fatalf("read the %s of %s whose text holds %q: %v", typ, artifactID, words, err)
	}
	return address, id
}

// versionRender is the text GET /api/v1/artifacts/{id}/versions/{number}?download=true answers of
// version number of the document artifactID: what the one renderer answers for that version.
func versionRender(t *testing.T, handler http.Handler, artifactID string, number int) string {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/versions/"+strconv.Itoa(number)+"?download=true", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read version %d of %s: status=%d body=%s", number, artifactID, response.Code, response.Body.String())
	}
	return response.Body.String()
}

// blockToken is the token GET /api/v1/artifacts/{id}/blocks answers for the block blockID of the
// document artifactID.
func blockToken(t *testing.T, handler http.Handler, artifactID, blockID string) string {
	t.Helper()
	for _, block := range listedBlocks(t, handler, artifactID) {
		if block.ID == blockID {
			return block.Token
		}
	}
	t.Fatalf("the blocks of %s hold no block %s", artifactID, blockID)
	return ""
}

// decisionOf is the entry GET /api/v1/issues/{key}/decisions answers for the decision whose node
// is address.
func decisionOf(t *testing.T, handler http.Handler, key, address string) decisionEntry {
	t.Helper()
	for _, entry := range readIssueDecisions(t, handler, key).Entries {
		if entry.Node.ID == address {
			return entry
		}
	}
	t.Fatalf("the decisions read of %s holds no entry of %s", key, address)
	return decisionEntry{}
}

// textParentOf is the far end of the current text_parent tuple of the block node address,
// `<kind>/<id>`, "" where it holds none.
func textParentOf(t *testing.T, database *store.Store, address string) string {
	t.Helper()
	return strings.Join(farEnds(t, database, refs.BlockNodeKind, address, refs.TupleTextParent), ",")
}

// lineWith is the first line of markdown that holds words, "" for none.
func lineWith(markdown, words string) string {
	for line := range strings.SplitSeq(markdown, "\n") {
		if strings.Contains(line, words) {
			return line
		}
	}
	return ""
}

// containerText is the text of the container block of markdown whose opening line, after its
// fence of colons, begins with opener (`callout{#tx`): its lines from that one to its closing
// fence, a line of as many colons as its opening fence, whatever its indent; "" where markdown holds
// no such line.
func containerText(markdown, opener string) string {
	lines := strings.Split(markdown, "\n")
	for start, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		fence := len(trimmed) - len(strings.TrimLeft(trimmed, ":"))
		if fence < 3 || !strings.HasPrefix(trimmed[fence:], opener) {
			continue
		}
		for end := start + 1; end < len(lines); end++ {
			if strings.TrimSpace(lines[end]) == strings.Repeat(":", fence) {
				return strings.Join(lines[start:end+1], "\n")
			}
		}
		return strings.Join(lines[start:], "\n")
	}
	return ""
}

// moveTextSpec is what alice writes into the spec of the text cases' issue key: the blocks each
// case moves. The rewritten case moves tr1, whose parent names tk1, under tk2; the dropped case
// moves tr2, which names and serves tk1, under a lens; the list case moves the second item of the
// first list under the first; the order case moves the first paragraph of its pair after the
// second; the callout case moves a paragraph under the callout tx; the decision case a paragraph
// under the decision td; the list paragraph case a paragraph under the list item that holds one;
// the table case a paragraph under each of trq, tkq, taq, the reading tgr of the decision tdr and
// the callout tc inside the decision td2; and the emptied case the paragraph of the callout ty, the
// paragraph of the callout ty2 inside a list item, and the first paragraph of the list item M.
func moveTextSpec(key string) string {
	ref := func(block string) string { return "dispatch://" + key + "/spec#b-" + block }
	return ":::criterion{#tk1}\nThe first criterion of the text cases.\n:::\n\n" +
		":::criterion{#tk2}\nThe second criterion of the text cases.\n:::\n\n" +
		":::requirement{#tr1 parent=\"" + ref("tk1") + "\"}\nThe requirement whose parent the move rewrites.\n:::\n\n" +
		":::requirement{#tr2 serves=\"" + ref("tk1") + "\" parent=\"" + ref("tk1") + "\"}\nThe requirement whose parent the move drops.\n:::\n\n" +
		":::requirement{#tr3}\nThe requirement the checks serve.\n:::\n\n" +
		"- The first item of the moving list.\n- The second item of the moving list, which moves under the first.\n\n" +
		"The first paragraph of the order case.\n\n" +
		"The second paragraph of the order case.\n\n" +
		":::callout{#tx kind=\"note\" title=\"\"}\nThe callout's own paragraph.\n:::\n\n" +
		"The paragraph that moves into the callout.\n\n" +
		":::decision{#td}\nThe decision alice keeps.\n:::\n\n" +
		"The paragraph the agent moves under the decision.\n\n" +
		"- The list item that holds one paragraph.\n\n" +
		"The paragraph that moves into the list item.\n\n" +
		":::requirement{#trq}\nThe requirement of the table's row.\n:::\n\n" +
		"A paragraph that moves under the requirement of the table.\n\n" +
		":::criterion{#tkq}\nThe criterion of the table's row.\n:::\n\n" +
		"A paragraph that moves under the criterion of the table.\n\n" +
		":::ask{#taq}\nWhich store do the table's writers take?\n:::\n\n" +
		"A paragraph that moves under the ask of the table.\n\n" +
		"::::decision{#tdr}\nThe decision whose reading the table moves under.\n\n:::reading{#tgr}\nThe agent reads the decision as one store.\n:::\n::::\n\n" +
		"A paragraph that moves under the reading of the table.\n\n" +
		"::::decision{#td2}\nThe decision that holds the callout of the table.\n\n:::callout{#tc kind=\"note\" title=\"\"}\nThe callout inside the decision.\n:::\n::::\n\n" +
		"A paragraph that moves under the callout inside the decision.\n\n" +
		"A top-level paragraph before the emptied containers.\n\n" +
		":::callout{#ty kind=\"note\" title=\"\"}\nY holds this one paragraph.\n:::\n\n" +
		"- The first paragraph of the item that holds Y2.\n\n  :::callout{#ty2 kind=\"note\" title=\"\"}\n  Y2 holds this one paragraph.\n  :::\n" +
		"- M's first paragraph.\n\n  M's second paragraph.\n"
}

// moveTextVerification is the Verification document of the text cases' issue key: the check tc1,
// which names no parent and serves tr3, which the gained case moves under tr1; the check tc2 of the
// table's row; and the paragraph the table's row moves under tc2.
func moveTextVerification(key string) string {
	check := func(id, words string) string {
		return ":::check{#" + id + " serves=\"dispatch://" + key + "/spec#b-tr3\" surface=\"api\" run=\"go test ./...\" negative_control=\"Undo the move.\"}\n" +
			words + "\n:::\n\n"
	}
	return "How " + key + " is checked.\n\n" + check("tc1", "The check that gains a parent.") + check("tc2", "The check of the table's row.") +
		"A paragraph of the Verification document that moves under its check.\n"
}

// moveTextPlan is the plan of the text cases' issue key: the plan task tt1 of the table's row and
// the paragraph the row moves under it.
func moveTextPlan(key string) string {
	return "The plan of " + key + ".\n\n:::plan_task{#tt1}\nThe plan task of the table's row.\n:::\n\n" +
		"A paragraph of the plan that moves under its plan task.\n"
}

// TestTheMoveWritesItsTextWhereMarkdownCanShowIt holds the move's text to R7, on an issue that has
// not converted, whose spec, Verification document and plan alice writes the blocks of each case
// into (moveTextSpec, moveTextVerification, moveTextPlan), with an approval request open on the spec
// and on the Verification document. The issue holds no document_rooms row before the first block
// and after the last move.
//
// A case that writes a version writes one version of the document that holds the moved block, whose
// artifact.version the log holds and which the open request on the document names; the version
// before still renders the bytes it rendered before; and the new version's stored markdown and the
// document's head render each read the move:
//
//   - rewritten: tr1, whose parent names tk1, under tk2: tr1's parent= names tk2 and not tk1; its
//     text_parent stays.
//   - gained: tc1, which names no parent, under tr1: tc1's parent= names tr1, in a version of the
//     Verification document, and the spec takes no version.
//   - dropped: tr2, which names and serves tk1, under a trade-off lens on tk2: tr2's line names no
//     parent and still serves, and the tree read answers tr2 under the lens.
//   - list: the second item of the first list under the first: it stands one level deeper, and its
//     text_parent moves.
//   - order: the first paragraph of the order pair after the second: the text holds the second
//     first, its text_rank stands below the moved one's, and the answer names the second as after.
//   - callout: a paragraph under the callout tx: the text holds it inside tx as its last block, its
//     stored part_of names tx, and its text_parent is tx.
//   - list paragraph: a paragraph under the list item that holds one paragraph: the text holds it in
//     the item after its first paragraph, its stored part_of names the item, and its text_parent is
//     the item.
//
// A moved paragraph is prose of a role document, which no read of the issue's tree returns (R2, R8,
// R16: issueTreeBuilder.excluded): the tree alone holds its place, so each case reads that place
// off the newest part_of the store holds, and reads no node of the paragraph in the tree read
// (storedUnder).
//
// The decision case, the table case and the emptied case write no version:
//
//   - decision: alice keeps td, and the agent moves a paragraph under it: the move is taken, the
//     text holds the paragraph outside td's content, which holds what it held, its stored part_of
//     names td, and td reads its text_sha256 still, kept by alice, waiting on no review.
//   - table: a paragraph under each of a requirement, a criterion, an ask block, a reading, a check,
//     a plan task and a callout inside a decision, each by its own document: each move is taken, the
//     document's head render is the one read before, the paragraph's stored part_of names the
//     block, the block's token is the one read before, and for the callout the decision's
//     text_sha256 is too.
//   - emptied: the only paragraph of the callout ty, the only paragraph of the callout ty2 inside a
//     list item, and the first paragraph of the list item M, each moved under the issue after a
//     top-level paragraph before them, are each 409 CONTAINER_WOULD_EMPTY naming the container and
//     its kind, storing nothing.
//
// Last, an edit of the spec that changes another block's words alone runs a later index pass, after
// which every block a case moved reads the placement stored and the part_of its move wrote.
func TestTheMoveWritesItsTextWhereMarkdownCanShowIt(t *testing.T) {
	t.Parallel()
	rig := newMoveRig(t, "MVTEXT")
	issue := createDigestIssue(t, rig.handler, rig.database, "MVTEXT", "The move's text", "The opening paragraph of the text cases.\n", true)
	key := issue.Key
	specID := issue.PrimaryArtifactID
	rooms := func(t *testing.T, when string) {
		t.Helper()
		var held int
		if err := rig.database.Pool.QueryRow(context.Background(), `
			select count(*) from document_rooms r join artifacts a on a.id = r.artifact_id where a.issue_key = $1`, key).Scan(&held); err != nil {
			t.Fatalf("count the document_rooms rows of %s: %v", key, err)
		}
		if held != 0 {
			t.Fatalf("the issue %s of the text cases holds a document_rooms row %s, want none", key, when)
		}
	}
	rooms(t, "before the first block")
	if written := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/artifacts/"+specID+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": moveTextSpec(key)}},
	}, "alice"); written.Code != http.StatusOK {
		t.Fatalf("alice writes the blocks of the text cases into the spec of %s: status=%d body=%s", key, written.Code, written.Body.String())
	}
	verificationID := uploadDocument(t, rig.handler, "/api/v1/issues/"+key, "verification.md", moveTextVerification(key)).Artifact.ID
	planID := uploadDocument(t, rig.handler, "/api/v1/issues/"+key, "plan.md", moveTextPlan(key)).Artifact.ID
	spec := readDecisionDocument(t, rig.database, key, "spec")
	verification := readDecisionDocument(t, rig.database, key, model.ArtifactSlugVerification)
	plan := readDecisionDocument(t, rig.database, key, model.ArtifactSlugPlan)
	specRequest := requestApproval(t, rig.handler, specID).ID
	verificationRequest := requestApproval(t, rig.handler, verificationID).ID
	rig.quiet(t)

	// stored is each block a case moved and the part_of its move wrote, `<kind>/<id>`.
	type storedPlace struct{ address, parent string }
	var stored []storedPlace
	moved := func(t *testing.T, address, parent string, more map[string]any) nodeMoved {
		t.Helper()
		answer := rig.move(t, key, address, parent, more)
		kind, id, _ := decodeNodeAddress(parent)
		stored = append(stored, storedPlace{address, storedEnd{kind: kind, id: id}.stored().kind + "/" + id})
		return answer
	}
	// versioned holds a move that writes the text of the document artifactID, which send makes, to
	// R7's version: one version more, its artifact.version, the open request named request naming
	// it, the version before rendering as it did, and holds, which answers what it finds wrong with
	// a text or "", reading the move in the new version's stored markdown and in the head render.
	versioned := func(t *testing.T, what, artifactID, request string, send func(), holds func(markdown string) string) {
		t.Helper()
		rig.quiet(t)
		before := newestQueryVersion(t, rig.database, artifactID)
		rendered := versionRender(t, rig.handler, artifactID, before.number)
		send()
		rig.quiet(t)
		after := newestQueryVersion(t, rig.database, artifactID)
		if after.number != before.number+1 {
			t.Errorf("%s left %s at version %d, want %d: one version of the document whose text it moved", what, artifactID, after.number, before.number+1)
			return
		}
		if _, _, held := documentEventOf(t, rig.database, "artifact.version", artifactID, after.number); !held {
			t.Errorf("%s wrote version %d of %s with no artifact.version", what, after.number, artifactID)
		}
		if named := requestedVersionOf(t, rig.database, request, artifactID); named != after.number {
			t.Errorf("the request on %s names version %d after %s, want %d", artifactID, named, what, after.number)
		}
		if again := versionRender(t, rig.handler, artifactID, before.number); again != rendered {
			t.Errorf("version %d of %s renders %q after %s, want the bytes it rendered before, %q", before.number, artifactID, again, what, rendered)
		}
		if problem := holds(after.markdown); problem != "" {
			t.Errorf("%s: the new version of %s %s", what, key, problem)
		}
		if problem := holds(documentMarkdown(t, rig.handler, artifactID)); problem != "" {
			t.Errorf("%s: the head render of %s %s", what, key, problem)
		}
	}
	underIn := func(t *testing.T, address, parent string) {
		t.Helper()
		if node, listed := treeNodes(t, rig.handler, key, "")[address]; !listed || node.Parent == nil || node.Parent.ID != parent {
			t.Errorf("%s stands under %+v in the tree read, want %s", address, node.Parent, parent)
		}
	}
	// storedUnder fails unless the block at address, a prose paragraph of a role document, stands
	// under the block parent through the newest part_of the store holds, and the tree read answers no
	// node of it, what naming the case: no read of the issue's tree returns such a paragraph (R2, R8,
	// R16), so the tree alone holds its place.
	storedUnder := func(t *testing.T, what, address, parent string) {
		t.Helper()
		if held := rig.partOf(t, address); held != refs.BlockNodeKind+"/"+parent {
			t.Errorf("%s%s holds a stored part_of naming %s, want %s", what, address, held, parent)
		}
		if node, listed := treeNodes(t, rig.handler, key, "")[address]; listed {
			t.Errorf("%sthe tree read answers %s, want no node: it answers it under %+v, and it is prose of a role document", what, address, node.Parent)
		}
	}
	textParentIs := func(t *testing.T, address, parent string) {
		t.Helper()
		testwait.Eventually(t, "the text_parent of "+address+" to name "+parent, func() bool {
			return textParentOf(t, rig.database, address) == parent
		})
	}

	t.Run("rewritten", func(t *testing.T) {
		tr1, tk1, tk2 := spec.address("tr1"), spec.address("tk1"), spec.address("tk2")
		textBefore := textParentOf(t, rig.database, tr1)
		versioned(t, "the move of "+tr1+" under "+tk2, specID, specRequest, func() { moved(t, tr1, tk2, nil) }, func(markdown string) string {
			line := lineWith(markdown, "requirement{#tr1")
			switch {
			case strings.Contains(line, "#b-tk1\""):
				return "still names the old parent " + tk1 + ": " + line
			case !strings.Contains(line, "#b-tk2\""):
				return "holds no parent= naming " + tk2 + " for " + tr1 + ": " + line
			}
			return ""
		})
		if now := textParentOf(t, rig.database, tr1); now != textBefore {
			t.Errorf("the move of %s moved its text_parent from %s to %s, want it kept: the text holds it where it stood", tr1, textBefore, now)
		}
		if rank := moveValueOf(t, rig.database, refs.BlockNodeKind, tr1, refs.AttributeTextRank); rank == "" {
			t.Errorf("%s holds no text_rank after its move", tr1)
		}
	})

	t.Run("gained", func(t *testing.T) {
		tc1, tr1 := verification.address("tc1"), spec.address("tr1")
		specVersion := newestQueryVersion(t, rig.database, specID).number
		versioned(t, "the move of "+tc1+" under "+tr1, verificationID, verificationRequest, func() { moved(t, tc1, tr1, nil) }, func(markdown string) string {
			if line := lineWith(markdown, "check{#tc1"); !strings.Contains(line, "parent=\"dispatch://"+key+"/spec#b-tr1\"") {
				return "holds no parent= for " + tc1 + ", want " + tr1 + ": " + line
			}
			return ""
		})
		if now := newestQueryVersion(t, rig.database, specID).number; now != specVersion {
			t.Errorf("the move of %s under %s wrote version %d of the spec, want none: the check's text is the Verification document's", tc1, tr1, now)
		}
	})

	t.Run("dropped", func(t *testing.T) {
		tr2 := spec.address("tr2")
		lens := decodeBody[lensInvoked](t, created(t, "a trade-off on tk2", dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+key+"/lenses",
			map[string]any{"on": "dispatch://" + key + "/spec#b-tk2", "kind": "trade-off"}, "alice")))
		versioned(t, "the move of "+tr2+" under the lens "+lens.ID, specID, specRequest, func() { moved(t, tr2, graphstore.KindLens+"/"+lens.ID, nil) }, func(markdown string) string {
			line := lineWith(markdown, "requirement{#tr2")
			switch {
			case strings.Contains(line, "parent="):
				return "still names a parent for " + tr2 + ", which a lens no attribute names: " + line
			case !strings.Contains(line, "serves="):
				return "lost the serves of " + tr2 + ": " + line
			}
			return ""
		})
		underIn(t, tr2, lens.ID)
	})

	t.Run("list", func(t *testing.T) {
		first, _ := textBlockOf(t, rig.database, specID, "list_item", "The first item of the moving list")
		second, _ := textBlockOf(t, rig.database, specID, "list_item", "The second item of the moving list")
		depth := func(markdown string) int {
			line := lineWith(markdown, "The second item of the moving list")
			return len(line) - len(strings.TrimLeft(line, " "))
		}
		from := depth(documentMarkdown(t, rig.handler, specID))
		textBefore := textParentOf(t, rig.database, second)
		versioned(t, "the move of the list item "+second+" under "+first, specID, specRequest, func() { moved(t, second, first, nil) }, func(markdown string) string {
			if at := depth(markdown); at <= from {
				return fmt.Sprintf("holds list item %s at depth %d, want deeper than %d", second, at, from)
			}
			return ""
		})
		testwait.Eventually(t, "the text_parent of "+second+" to move from "+textBefore, func() bool {
			return textParentOf(t, rig.database, second) != textBefore
		})
	})

	t.Run("order", func(t *testing.T) {
		first, _ := textBlockOf(t, rig.database, specID, "paragraph", "The first paragraph of the order case")
		second, _ := textBlockOf(t, rig.database, specID, "paragraph", "The second paragraph of the order case")
		var answer nodeMoved
		versioned(t, "the move of "+first+" after "+second, specID, specRequest, func() {
			answer = moved(t, first, graphstore.KindIssue+"/"+key, map[string]any{"after": second})
		}, func(markdown string) string {
			if at, after := strings.Index(markdown, "The first paragraph of the order case"), strings.Index(markdown, "The second paragraph of the order case"); at < 0 || after < 0 || at < after {
				return fmt.Sprintf("holds the first paragraph of the order case at byte %d and the second at byte %d, want the first after the second", at, after)
			}
			return ""
		})
		if answer.After == nil || *answer.After != second {
			t.Errorf("the move of %s after %s answers after %v, want %s", first, second, answer.After, second)
		}
		testwait.Eventually(t, "the text_rank of "+first+" to stand above that of "+second, func() bool {
			return moveValueOf(t, rig.database, refs.BlockNodeKind, first, refs.AttributeTextRank) > moveValueOf(t, rig.database, refs.BlockNodeKind, second, refs.AttributeTextRank)
		})
	})

	t.Run("callout", func(t *testing.T) {
		paragraph, _ := textBlockOf(t, rig.database, specID, "paragraph", "The paragraph that moves into the callout")
		tx := spec.address("tx")
		versioned(t, "the move of "+paragraph+" under the callout "+tx, specID, specRequest, func() { moved(t, paragraph, tx, nil) }, func(markdown string) string {
			lines := strings.Split(containerText(markdown, "callout{#tx"), "\n")
			if len(lines) < 2 || !strings.Contains(lines[len(lines)-2], "The paragraph that moves into the callout") {
				return "holds " + paragraph + " outside " + tx + ", want inside it as its last block"
			}
			return ""
		})
		storedUnder(t, "", paragraph, tx)
		textParentIs(t, paragraph, refs.BlockNodeKind+"/"+tx)
	})

	t.Run("decision", func(t *testing.T) {
		td := spec.address("td")
		paragraph, _ := textBlockOf(t, rig.database, specID, "paragraph", "The paragraph the agent moves under the decision")
		decision := decisionOf(t, rig.handler, key, td)
		if kept := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/artifacts/"+specID+"/blocks/td/keep",
			map[string]any{"text_sha256": decision.TextSHA256}, "alice"); kept.Code != http.StatusOK {
			t.Fatalf("alice keeps the decision %s: status=%d body=%s", td, kept.Code, kept.Body.String())
		}
		rig.quiet(t)
		version := newestQueryVersion(t, rig.database, specID)
		content := containerText(version.markdown, "decision{#td")
		response := patchNode(t, rig.handler, key, paragraph, rig.tag(t, paragraph), map[string]any{"parent": td, "actor": sessionActor()}, "")
		if response.Code != http.StatusOK {
			refusal := decodeBody[refusalAnswer](t, response)
			t.Fatalf("the move of %s under %s answered %d %s, want 200: %s", paragraph, td, response.Code, refusal.Code, refusal.Error)
		}
		stored = append(stored, storedPlace{paragraph, refs.BlockNodeKind + "/" + td})
		rig.quiet(t)
		now := newestQueryVersion(t, rig.database, specID)
		if now.number != version.number {
			t.Errorf("the move of %s under the decision %s wrote version %d of the spec, want none", paragraph, td, now.number)
		}
		for name, text := range map[string]string{"newest version": now.markdown, "head render": documentMarkdown(t, rig.handler, specID)} {
			held := containerText(text, "decision{#td")
			if strings.Contains(held, "The paragraph the agent moves under the decision") {
				t.Errorf("the %s of %s holds %s inside %s, want outside it", name, key, paragraph, td)
			} else if held != content {
				t.Errorf("the %s of %s holds the decision %s as %q, want %q, the blocks it held before", name, key, td, held, content)
			}
		}
		storedUnder(t, "", paragraph, td)
		after := decisionOf(t, rig.handler, key, td)
		if after.TextSHA256 != decision.TextSHA256 || after.Kept == nil || after.Kept.KeptBy != "alice" || after.WaitingReview {
			t.Errorf("the decision %s reads text_sha256 %s after the move, want %s, kept %+v, waiting_review %t", td, after.TextSHA256, decision.TextSHA256, after.Kept, after.WaitingReview)
		}
	})

	t.Run("list paragraph", func(t *testing.T) {
		item, _ := textBlockOf(t, rig.database, specID, "list_item", "The list item that holds one paragraph")
		paragraph, _ := textBlockOf(t, rig.database, specID, "paragraph", "The paragraph that moves into the list item")
		versioned(t, "the move of "+paragraph+" under the list item "+item, specID, specRequest, func() { moved(t, paragraph, item, nil) }, func(markdown string) string {
			if !strings.Contains(markdown, "- The list item that holds one paragraph.\n\n  The paragraph that moves into the list item.") {
				return "holds " + paragraph + " outside " + item + ", want inside it after its first paragraph"
			}
			return ""
		})
		storedUnder(t, "", paragraph, item)
		textParentIs(t, paragraph, refs.BlockNodeKind+"/"+item)
	})

	t.Run("table", func(t *testing.T) {
		documents := map[string]decisionDocument{specID: spec, verificationID: verification, planID: plan}
		for _, row := range []struct {
			typ, document, block, words string
		}{
			{"requirement", specID, "trq", "A paragraph that moves under the requirement of the table"},
			{"criterion", specID, "tkq", "A paragraph that moves under the criterion of the table"},
			{"ask", specID, "taq", "A paragraph that moves under the ask of the table"},
			{"reading", specID, "tgr", "A paragraph that moves under the reading of the table"},
			{"check", verificationID, "tc2", "A paragraph of the Verification document that moves under its check"},
			{"plan_task", planID, "tt1", "A paragraph of the plan that moves under its plan task"},
			{"callout", specID, "tc", "A paragraph that moves under the callout inside the decision"},
		} {
			block := documents[row.document].address(row.block)
			paragraph, _ := textBlockOf(t, rig.database, row.document, "paragraph", row.words)
			rig.quiet(t)
			version := newestQueryVersion(t, rig.database, row.document).number
			head := documentMarkdown(t, rig.handler, row.document)
			token := blockToken(t, rig.handler, row.document, row.block)
			var sum string
			if row.block == "tc" {
				sum = decisionOf(t, rig.handler, key, spec.address("td2")).TextSHA256
			}
			moved(t, paragraph, block, nil)
			rig.quiet(t)
			if now := newestQueryVersion(t, rig.database, row.document).number; now != version {
				t.Errorf("row %s: the move of %s wrote version %d of %s, want none", row.typ, paragraph, now, row.document)
			}
			if now := documentMarkdown(t, rig.handler, row.document); now != head {
				t.Errorf("row %s: the move of %s changed the head render of %s to %q, want %q", row.typ, paragraph, row.document, now, head)
			}
			storedUnder(t, "row "+row.typ+": ", paragraph, block)
			if now := blockToken(t, rig.handler, row.document, row.block); now != token {
				t.Errorf("row %s: %s reads token %s, want %s", row.typ, block, now, token)
			}
			if sum != "" {
				if now := decisionOf(t, rig.handler, key, spec.address("td2")).TextSHA256; now != sum {
					t.Errorf("row %s: the decision %s reads text_sha256 %s, want %s", row.typ, spec.address("td2"), now, sum)
				}
			}
		}
	})

	t.Run("emptied", func(t *testing.T) {
		before, _ := textBlockOf(t, rig.database, specID, "paragraph", "A top-level paragraph before the emptied containers")
		item, itemID := textBlockOf(t, rig.database, specID, "list_item", "M's first paragraph")
		ofY, _ := textBlockOf(t, rig.database, specID, "paragraph", "Y holds this one paragraph")
		ofY2, _ := textBlockOf(t, rig.database, specID, "paragraph", "Y2 holds this one paragraph")
		ofM, _ := textBlockOf(t, rig.database, specID, "paragraph", "M's first paragraph")
		for _, out := range []struct {
			paragraph, container, id, kind string
		}{
			{ofY, spec.address("ty"), "ty", "callout"},
			{ofY2, spec.address("ty2"), "ty2", "callout"},
			{ofM, item, itemID, "list item"},
		} {
			if parent := rig.partOf(t, out.paragraph); parent != refs.BlockNodeKind+"/"+out.container {
				t.Errorf("%s stands under %s before its move, want its container %s", out.paragraph, parent, out.container)
			}
			rig.refused(t, "the move of "+out.paragraph+" out of "+out.container, key, out.paragraph, rig.tag(t, out.paragraph),
				map[string]any{"parent": graphstore.KindIssue + "/" + key, "after": before}, "alice",
				http.StatusConflict, codeContainerWouldEmpty, out.kind, "#"+out.id)
		}
	})

	// A later pass: an edit that changes another block's words alone.
	if edited := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/artifacts/"+specID+"/edits", map[string]any{"ops": []map[string]any{{
		"op": "replace", "find": "The opening paragraph of the text cases.", "with": "The opening paragraph of the text cases, reworded.",
	}}}, "alice"); edited.Code != http.StatusOK {
		t.Fatalf("alice edits the opening paragraph of %s: status=%d body=%s", key, edited.Code, edited.Body.String())
	}
	rig.quiet(t)
	for _, place := range stored {
		parent := rig.partOf(t, place.address)
		if placement := moveValueOf(t, rig.database, refs.BlockNodeKind, place.address, refs.AttributePlacement); placement != refs.PlacementStored || parent != place.parent {
			t.Errorf("after the next index pass %s holds the placement %q under %s, want %q under %s, where its move put it",
				place.address, placement, parent, refs.PlacementStored, place.parent)
		}
	}
	rooms(t, "after the last move")
}
