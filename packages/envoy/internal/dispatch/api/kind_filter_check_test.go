package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// kindFilterConversation is the kinds of the conversation c-kind-filter reads the tree with.
var kindFilterConversation = []string{
	"callout", "ask", "comment", "reading", "lens", "finding", "risk", "criterion", "requirement", "check", "decision", "issue", "prose",
}

// kindFilterQuery is the tree read's query naming each of kinds in a kind= of its own.
func kindFilterQuery(kinds ...string) string {
	values := url.Values{}
	for _, kind := range kinds {
		values.Add("kind", kind)
	}
	return "?" + values.Encode()
}

// kindFilterAddress is a node's `<kind>:<id>`.
func kindFilterAddress(kind, id string) string { return kind + ":" + id }

// pinnedSpecQueryVersion is the spec query of the issue key and its newest version, failing unless
// that version records the store revision it read (treeReadViewsRecorded), as a read at it needs.
func pinnedSpecQueryVersion(t *testing.T, database *store.Store, key string) (string, int) {
	t.Helper()
	query := queryArtifactOf(t, database, key, graphstore.QuerySpec)
	pinned := renderAtVersionNewest(t, database, query)
	if treeReadViewsRecorded(t, database, query, pinned) != 1 {
		t.Fatalf("version %d of the spec query of %s holds no record of the store revision it read: this case proves nothing", pinned, key)
	}
	return query, pinned
}

// kindFilterPages is the tree read of key with query as alice, and each read from the
// comments_before the page before answered beside the same query, until one answers none.
func kindFilterPages(t *testing.T, handler http.Handler, key, query string) []treeAnswer {
	t.Helper()
	var pages []treeAnswer
	target := query
	for page := 1; page <= 10; page++ {
		read := treeRead(t, handler, key, target, "alice", false)
		pages = append(pages, read)
		var position *string
		if len(read.CommentsBefore) > 0 {
			if err := json.Unmarshal(read.CommentsBefore, &position); err != nil {
				t.Fatalf("page %d of the tree of %s%s answers comments_before %s: %v", page, key, query, read.CommentsBefore, err)
			}
		}
		if position == nil {
			return pages
		}
		separator := "?"
		if query != "" {
			separator = "&"
		}
		target = query + separator + "comments_before=" + url.QueryEscape(*position)
	}
	t.Fatalf("the tree of %s%s still answers a comments_before after 10 pages", key, query)
	return nil
}

// kindFilterUnion is every node of pages by its address, the first page's where two hold it, and
// the page each first stands on.
func kindFilterUnion(pages []treeAnswer) (map[string]model.IssueTreeNode, map[string]int) {
	nodes, on := map[string]model.IssueTreeNode{}, map[string]int{}
	for index, page := range pages {
		for _, node := range page.Nodes {
			address := kindFilterAddress(node.Kind, node.ID)
			if _, held := nodes[address]; !held {
				nodes[address], on[address] = node, index+1
			}
		}
	}
	return nodes, on
}

// kindFilterTupleHeld reports whether a page of pages holds the tuple of kind from from to to.
func kindFilterTupleHeld(pages []treeAnswer, kind string, from, to model.IssueTreeEnd) bool {
	return slices.ContainsFunc(pages, func(page treeAnswer) bool { return page.holdsTuple(kind, from, to) })
}

// kindFilterKindOf is the kind `kind` takes a node of an answer by, as the test reads it off the
// node: a block's typed block, finding for a finding of a lens, prose for a block that opens none,
// none for an ask's block, and every other node's own kind; the issue read is of no kind.
func kindFilterKindOf(node model.IssueTreeNode, key string) string {
	switch node.Kind {
	case refs.BlockNodeKind:
		switch {
		case strings.HasPrefix(node.ID, graphstore.KindLens+"/"):
			return treeKindFinding
		case node.Supertag == graphstore.SupertagAsk:
			return ""
		case pmdoc.IsTypedBlock(node.Supertag):
			return node.Supertag
		}
		return treeKindProse
	case graphstore.KindIssue:
		if node.ID == key {
			return ""
		}
	}
	return node.Kind
}

// kindFilterRefused fails t unless the tree read of key with query answers 400 INVALID_QUERY,
// naming kind.
func kindFilterRefused(t *testing.T, handler http.Handler, key, query, what string) {
	t.Helper()
	response := treeResponse(t, handler, key, query, "alice", false)
	var refusal refusalAnswer
	_ = json.Unmarshal(response.Body.Bytes(), &refusal)
	if response.Code != http.StatusBadRequest || refusal.Code != "INVALID_QUERY" || !strings.Contains(refusal.Error, "kind") {
		t.Errorf("%s on %s (%s) answered %d, not 400: %s, want 400 INVALID_QUERY naming kind", what, key, query, response.Code, response.Body.String())
	}
}

// kindFilterSpec is the spec c-kind-filter's issue is born with: the memo callout first, a
// Summary paragraph, a second callout, and the ask block a declined ask is asked by.
const kindFilterSpec = ":::callout{#m1}\nThe board lists the urgent asks first.\n:::\n\n" +
	"Summary: the board orders the asks a person must answer.\n\n" +
	":::callout{#m2}\nA second callout, which is no memo.\n:::\n\n" +
	":::ask{#a9 urgency=\"low\" multiple=\"false\"}\nShall the board page?\n\n- Yes: Page it.\n- No: Keep one list.\n:::\n"

// TestTheTreeReadFiltersByNodeKind is c-kind-filter (R30, R8). alice creates an issue born
// converted whose spec opens with its memo callout and holds a Summary paragraph, a second callout
// and an ask block she declines. She writes a message and the criterion k1 under it, with the
// requirement r1 under k1, and a comment C and the criterion k2 under it, with r2 under k2; an
// agent session asks a1 in the spec, alice answers it, and the session writes the reading g1 under
// it and the decision d1 with uncertainty medium and a reason. alice writes a constraint r1's
// constrained_by names, the check h1 serving r1 and r2 with one result, a plan task, a paragraph of
// the room's own fragment she places under k1 through PATCH, a child issue she places under C
// through PATCH whose check h2 serves r1 and holds one result, a pre-mortem lens on r1 with one
// finding and one risk, a post to which the session replies, and a comment on the text of h1's
// result through a node anchor (R31). The session comments on r1's text, alice writes 205 posts
// more, and an interaction is seeded with a changed_by_run from r1.
//
// The read with the conversation's kinds, page by page through comments_before, answers exactly
// the nodes of those kinds the read with no view answers, with each ancestor of each, but each
// callout but the memo, beside the comments it carries, the items' blocks and the context; the
// memo first after the issue's node; the message as context with k1 under it; h2 under r1, marked
// with the child's key, and neither result, nor the plan task, nor the Summary paragraph; the
// agent's comment on r1's text under r1 as context; the constraint, the lens's kind and the
// interaction as context, each with its tuple; 200 comments and a comments_before on its first page,
// and on each page after it comments and the nodes under them alone. r1 and r2 answer checks 2 and
// 1 there, in the read with no view and in ?view=spec, each the coverage read's, and r3, written
// with no check, 0. d1 and a1 answer their decision and answer members as the Decisions read's
// entries do, there and in ?view=decisions, where the declined ask answers its answer members
// present and null and d1 its keeper and its decided_by. kind=check&kind=result answers h1, h2,
// both results and the comment on h1's result's text under it; kind=callout the memo alone; and
// kind=ask each ask's own node, an ask block never but as an ancestor. kind=block, an empty kind,
// kind=spec, kind beside view=spec and kind beside v=1 are each refused. A version of the spec
// query written while h1 and h2 serve r1 answers r1's checks 2, and the head 1 once h2 serves r2
// alone.
func TestTheTreeReadFiltersByNodeKind(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "KINDS")
	issue := createDigestIssue(t, handler, database, "KINDS", "The conversation's kinds", kindFilterSpec, false)
	key := issue.Key
	spec := readDecisionDocument(t, database, key, "spec")
	specRef := "dispatch://" + key + "/spec"
	tree, err := issueTreeOf(ctx, database.Pool, key)
	if err != nil || tree == "" {
		t.Fatalf("read the tree artifact of %s: %q, %v: the issue is not born converted, so this case proves nothing", key, tree, err)
	}

	message := decodeBody[writtenRecord](t, created(t, "a person's message", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/messages",
		map[string]any{"body": "The board must hold every urgent ask."}, "alice"))).ID
	commentC, _ := personComment(t, handler, database, key, "The board should show the age too.")
	// A criterion holds paragraphs alone (pmdoc/schema/blocks.json, `"content": "paragraph+"`), so
	// each requirement stands under its criterion through its parent attribute.
	k1Ref, k2Ref := refs.BlockRef(specRef, "k1"), refs.BlockRef(specRef, "k2")
	editSpec(t, handler, key, []map[string]any{{"op": "insert", "after": "end", "markdown": ":::criterion{#k1 parent=\"dispatch://" + key + "/message/" + message + "\"}\n" +
		"I want the urgent asks first.\n:::\n\n:::requirement{#r1 parent=\"" + k1Ref + "\"}\nThe board lists each urgent ask first.\n:::\n\n" +
		":::criterion{#k2 parent=\"dispatch://" + key + "/comment/" + commentC + "\"}\nI want each ask's age shown.\n:::\n\n" +
		":::requirement{#r2 parent=\"" + k2Ref + "\"}\nThe board shows each ask's age.\n:::\n"}})
	r1, r2 := spec.address("r1"), spec.address("r2")
	r1Ref, r2Ref := refs.BlockRef(specRef, "r1"), refs.BlockRef(specRef, "r2")

	// The session asks a1 in the spec and decides d1; alice answers a1, and the session reads it.
	agentEdit := func(markdown, summary string) {
		t.Helper()
		response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/artifacts/spec/edits", map[string]any{
			"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": markdown}}, "summary": summary, "actor": sessionActor(),
		})
		if response.Code != http.StatusOK {
			t.Fatalf("the session writes %s into the spec of %s: status=%d body=%s", summary, key, response.Code, response.Body.String())
		}
	}
	agentEdit(":::ask{#a1 urgency=\"med\" multiple=\"false\"}\nWhich order first?\n\n- Urgency: The most urgent first\n- Age: The oldest first\n:::\n", "the ask a1")
	agentEdit(":::decision{#d1 uncertainty=\"medium\" uncertainty_reason=\"No load was measured yet.\"}\nWe sort the asks on the server.\n:::\n", "the decision d1")
	// The edit versions the spec in its own transaction, and the spec's settlement opens a1's ask
	// after it: the test reads the ask once settlement has indexed it.
	awaitIndexedAskBlock(t, handler, spec.id, "a1", "Which order first?")
	asked := blockAskID(t, handler, key, "a1")
	answerDecisionBlock(t, handler, asked, "alice", []string{"Urgency"}, "Urgency, then age.", nil)
	agentUploadAs(t, handler, key, "spec.md", withAskReading(t, documentMarkdown(t, handler, spec.id), "a1", "g1", "The session reads the answer as urgency before age."), nil)
	declined := blockAskID(t, handler, key, "a9")
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+declined+"/decline", declineBody(nil), "alice"); response.Code != http.StatusOK {
		t.Fatalf("alice declines the ask a9: status=%d body=%s", response.Code, response.Body.String())
	}
	decision, reading := spec.address("d1"), spec.address("g1")

	// The constraint r1 names, the check h1 of r1 and r2 with its result, a plan task, and a
	// paragraph of the room's own fragment.
	rig := reasoningRig{t: t, handler: handler, database: database, issue: key}
	constraint := decodeBody[constraintRead](t, created(t, "the constraint", rig.post("/constraints", map[string]any{"standard": "RFC 9110", "reference": "section 9.2.2"}, "alice")))
	if pointed := rig.post(nodePath(r1, "pointers"), map[string]any{"kind": graphstore.TupleConstrainedBy, "action": pointerAppend, "to": constraint.Address}, "alice"); pointed.Code != http.StatusOK {
		t.Fatalf("point r1 at the constraint: status=%d body=%s", pointed.Code, pointed.Body.String())
	}
	uploadDocument(t, handler, "/api/v1/issues/"+key, "verification.md", checkServing("h1", r1Ref+" "+r2Ref))
	h1 := readDecisionDocument(t, database, key, "verification-md").address("h1")
	recorded := recordResult(t, handler, key, resultBody("h1", "pass", headCommit, map[string]any{"observed": "The board listed the urgent asks in three seconds."}))
	if recorded.Code != http.StatusCreated {
		t.Fatalf("record a result of h1: status=%d body=%s", recorded.Code, recorded.Body.String())
	}
	h1Result := decodeBody[recordedResult](t, recorded).Result.ID
	uploadDocument(t, handler, "/api/v1/issues/"+key, "plan.md", planTaskServing("t1", r1Ref))
	planTask := readDecisionDocument(t, database, key, "plan-md").address("t1")
	if written := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+tree+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": "A paragraph of the room's own fragment.\n"}}, "summary": "a paragraph",
	}, "alice"); written.Code != http.StatusOK {
		t.Fatalf("write a paragraph into the room's own fragment of %s: status=%d body=%s", key, written.Code, written.Body.String())
	}

	// The child issue under C, its check h2 of r1 with a result.
	child := createDigestChild(t, handler, database, "KINDS", "A child of the board", "The child's design.\n", key, false)
	uploadDocument(t, handler, "/api/v1/issues/"+child.Key, "verification.md", checkServing("h2", r1Ref))
	h2 := readDecisionDocument(t, database, child.Key, "verification-md").address("h2")
	recordedChild := recordResult(t, handler, child.Key, resultBody("h2", "pass", headCommit, nil))
	if recordedChild.Code != http.StatusCreated {
		t.Fatalf("record a result of h2: status=%d body=%s", recordedChild.Code, recordedChild.Body.String())
	}
	h2Result := decodeBody[recordedResult](t, recordedChild).Result.ID
	// alice places the child under C, and the paragraph P of the room's own fragment under k1, each
	// through PATCH naming the placement_seq the tree read answers: Markdown shows no paragraph under
	// a criterion, so the tree alone holds P's place (R7).
	before := treeRead(t, handler, key, "", "alice", false)
	placeUnder := func(what string, node model.IssueTreeNode, id, parent string) {
		t.Helper()
		if node.PlacementSeq == nil {
			t.Fatalf("the tree read of %s answers %s with no placement_seq, which a move names", key, what)
		}
		if moved := patchNode(t, handler, key, id, strconv.Quote(strconv.FormatInt(*node.PlacementSeq, 10)), map[string]any{"parent": parent}, "alice"); moved.Code != http.StatusOK {
			t.Fatalf("alice places %s under %s through PATCH: status=%d body=%s", what, parent, moved.Code, moved.Body.String())
		}
	}
	placed, _ := before.node(t, graphstore.KindIssue, child.Key)
	placeUnder("the child "+child.Key, placed, graphstore.KindIssue+"/"+child.Key, graphstore.KindComment+"/"+commentC)
	proseAt := slices.IndexFunc(before.Nodes, func(node model.IssueTreeNode) bool {
		return node.Kind == refs.BlockNodeKind && strings.TrimSpace(node.Text) == "A paragraph of the room's own fragment."
	})
	if proseAt < 0 {
		t.Fatalf("the tree read of %s answers no paragraph of the room's own fragment: %v", key, before.addresses())
	}
	prose := before.Nodes[proseAt]
	placeUnder("the paragraph "+prose.ID, prose, prose.ID, spec.address("k1"))

	// A pre-mortem lens on r1 with one finding and one risk.
	lens := rig.invoke(r1Ref, graphstore.LensKindPreMortem, "alice", nil)
	finding := decodeBody[lensFinding](t, created(t, "the finding", rig.post(nodePath("lens/"+lens.ID, "findings"), map[string]any{"text": "The board misses an ask asked twice."}, "alice")))
	risk := decodeBody[riskWritten](t, created(t, "the risk", rig.post("/risks", map[string]any{
		"lens": lens.Address, "heading": "The board hides a stale ask.", "early_warning": "Asks age unseen.", "prevention": "Sort by age too.", "prevented_by": refs.BlockRef("dispatch://"+key+"/artifact/verification-md", "h1"),
	}, "alice")))

	// A post the session replies to, and a comment on the text of h1's result (R31's node anchor).
	post, _ := personComment(t, handler, database, key, "Do we page the board?")
	reply := createThreadComment(t, handler, key, map[string]any{"body": "Not yet.", "reply_to": post, "actor": sessionActor()}, "").ID
	onResult := ""
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/comments", map[string]any{
		"body": "Three seconds is slow.", "anchor": map[string]any{"node": graphstore.KindResult + "/" + h1Result, "quote": "three seconds"},
	}, "alice"); response.Code == http.StatusCreated {
		onResult = decodeBody[writtenRecord](t, response).ID
	} else {
		t.Errorf("alice comments on the text of h1's result through a node anchor (R31): status=%d body=%s, want 201", response.Code, response.Body.String())
	}
	agentOnR1 := createThreadComment(t, handler, key, map[string]any{
		"body": "The order holds for asks of one age.", "anchor": map[string]any{"artifact": spec.id, "block_id": "r1", "quote": "urgent ask"}, "actor": sessionActor(),
	}, "").ID
	for index := range 205 {
		created(t, fmt.Sprintf("post %d", index+1), dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/comments",
			map[string]any{"body": fmt.Sprintf("Post %d on the board.", index+1)}, "alice"))
	}

	// An interaction that changed r1, seeded through the store's own writer.
	run, err := directServer(deps).recordGoingRequest(ctx, model.Actor{Kind: "user", ID: "alice"}, "", "Which ask comes first?")
	if err != nil {
		t.Fatalf("record alice's run: %v", err)
	}
	seedTupleWithHead(t, database, graphstore.Tuple{
		FromKind: refs.BlockNodeKind, FromID: r1, Kind: graphstore.TupleChangedByRun, ToKind: graphstore.KindInteraction, ToID: run.ID,
		At: time.Now(), Who: model.Actor{Kind: "user", ID: "alice"}, Asserted: true,
	})

	// The read with no view, page by page, and the read with the conversation's kinds.
	whole := kindFilterPages(t, handler, key, "")
	wholeNodes, _ := kindFilterUnion(whole)
	kinds := kindFilterPages(t, handler, key, kindFilterQuery(kindFilterConversation...))
	kindNodes, kindPage := kindFilterUnion(kinds)
	label := "the read with the conversation's kinds of " + key
	memo := kindFilterAddress(refs.BlockNodeKind, spec.address("m1"))

	// Each node of those kinds the read with no view answers, and each ancestor of each.
	parents := map[string]string{}
	for address, node := range wholeNodes {
		if node.Parent != nil {
			parents[address] = kindFilterAddress(node.Parent.Kind, node.Parent.ID)
		}
	}
	selected, expected := map[string]bool{}, map[string]bool{}
	for address, node := range wholeNodes {
		kind := kindFilterKindOf(node, key)
		if node.Role == treeRoleContext || kind == graphstore.KindComment || !slices.Contains(kindFilterConversation, kind) ||
			kind == treeKindMemo && address != memo {
			continue
		}
		selected[address], expected[address] = true, true
		for at, steps := parents[address], 0; at != "" && steps < len(wholeNodes); steps++ {
			expected[at] = true
			at = parents[at]
		}
	}
	for address := range expected {
		node, held := kindNodes[address]
		switch {
		case !held:
			t.Errorf("%s answers no %s, which the read with no view answers (parent %+v) and it should answer, of a kind it names or an ancestor of one",
				label, address, wholeNodes[address].Parent)
		case selected[address] && node.Role != treeRoleTree:
			t.Errorf("%s answers %s, of a kind it names, as %q, want a node of the tree", label, address, node.Role)
		case !selected[address] && node.Kind != graphstore.KindComment && node.Role != treeRoleContext:
			t.Errorf("%s answers %s, an ancestor of a kind it does not name, as %q, want context", label, address, node.Role)
		}
	}
	for address, node := range kindNodes {
		switch {
		case expected[address], node.Kind == graphstore.KindComment, isItemBlockKind(node.Kind):
		case node.Role == treeRoleContext && slices.Contains([]string{graphstore.KindLensKind, graphstore.KindConstraint, graphstore.KindInteraction, graphstore.KindPerson, graphstore.KindSession}, node.Kind):
		default:
			t.Errorf("%s answers %s (%s, parent %+v), which is of no kind it names and no ancestor of one", label, address, node.Supertag, node.Parent)
		}
	}

	// The memo first and no other callout, the message with k1, h2 under r1, and what the kinds leave out.
	if first := kinds[0].Nodes; len(first) < 2 || kindFilterAddress(first[0].Kind, first[0].ID) != kindFilterAddress(graphstore.KindIssue, key) ||
		kindFilterAddress(first[1].Kind, first[1].ID) != memo {
		t.Errorf("%s does not answer the memo %s first, after the issue's node: it answers %v", label, memo, kinds[0].addresses())
	}
	if _, held := kindNodes[kindFilterAddress(refs.BlockNodeKind, spec.address("m2"))]; held {
		t.Errorf("%s answers the second callout, which is no memo", label)
	}
	if node, held := kindNodes[kindFilterAddress(graphstore.KindMessage, message)]; !held || node.Role != treeRoleContext {
		t.Errorf("%s answers the message %s as %+v, want it as context, the parent of k1", label, message, node)
	}
	if node, held := kindNodes[kindFilterAddress(refs.BlockNodeKind, spec.address("k1"))]; !held || node.Parent == nil || node.Parent.ID != message {
		t.Errorf("%s answers the criterion k1 written under the message %s under %+v, want the message", label, message, node.Parent)
	}
	if node, held := kindNodes[kindFilterAddress(refs.BlockNodeKind, h2)]; !held || node.Parent == nil || node.Parent.ID != r1 || node.Issue != child.Key {
		t.Errorf("%s answers the child's check h2 as %+v, want it under r1 %s, marked with %s", label, node, r1, child.Key)
	}
	for what, address := range map[string]string{
		"h2's result": kindFilterAddress(graphstore.KindResult, h2Result), "h1's result": kindFilterAddress(graphstore.KindResult, h1Result),
		"the plan task": kindFilterAddress(refs.BlockNodeKind, planTask), "the comment on h1's result's text": kindFilterAddress(graphstore.KindComment, onResult),
	} {
		if _, held := kindNodes[address]; held {
			t.Errorf("%s answers %s %s, which the conversation's kinds leave out", label, what, address)
		}
	}
	for address, node := range kindNodes {
		if strings.Contains(node.Text, "Summary: the board orders") {
			t.Errorf("%s answers %s, the Summary paragraph of the spec", label, address)
		}
	}
	for what, comment := range map[string]string{"the post": post, "the agent's reply": reply, "the comment C": commentC} {
		if _, held := kindNodes[kindFilterAddress(graphstore.KindComment, comment)]; !held {
			t.Errorf("%s answers no %s %s", label, what, comment)
		}
	}
	if node, held := kindNodes[kindFilterAddress(graphstore.KindComment, agentOnR1)]; !held || node.Role != treeRoleContext || node.Parent == nil || node.Parent.ID != r1 {
		t.Errorf("%s answers the agent's comment on r1's text as %+v, want it under r1 as context", label, node)
	}
	for _, carried := range []struct {
		what     string
		node     model.IssueTreeEnd
		tuple    string
		from, to model.IssueTreeEnd
	}{
		{"the constraint", treeEnd(graphstore.KindConstraint, constraint.ID), graphstore.TupleConstrainedBy, treeEnd(refs.BlockNodeKind, r1), treeEnd(graphstore.KindConstraint, constraint.ID)},
		{"the lens's kind", treeEnd(graphstore.KindLensKind, graphstore.LensKindPreMortem), graphstore.TupleHasKind, treeEnd(graphstore.KindLens, lens.ID), treeEnd(graphstore.KindLensKind, graphstore.LensKindPreMortem)},
		{"the interaction", treeEnd(graphstore.KindInteraction, run.ID), graphstore.TupleChangedByRun, treeEnd(refs.BlockNodeKind, r1), treeEnd(graphstore.KindInteraction, run.ID)},
	} {
		node, held := kindNodes[kindFilterAddress(carried.node.Kind, carried.node.ID)]
		if !held || node.Role != treeRoleContext || !kindFilterTupleHeld(kinds, carried.tuple, carried.from, carried.to) {
			t.Errorf("%s answers %s as %+v, with its %s tuple %t, want it as context with the tuple", label, carried.what, node, carried.tuple,
				kindFilterTupleHeld(kinds, carried.tuple, carried.from, carried.to))
		}
	}
	for what, address := range map[string]string{
		"the finding": kindFilterAddress(refs.BlockNodeKind, finding.ID), "the risk": kindFilterAddress(graphstore.KindRisk, risk.ID),
		"the reading": kindFilterAddress(refs.BlockNodeKind, reading), "the child issue": kindFilterAddress(graphstore.KindIssue, child.Key),
		"the lens": kindFilterAddress(graphstore.KindLens, lens.ID),
	} {
		if node, held := kindNodes[address]; !held || node.Role != treeRoleTree {
			t.Errorf("%s answers %s %s as %+v, want it a node of the tree", label, what, address, node)
		}
	}

	// The comment page: 200 comments and a comments_before on the first page, and on each page
	// after it comments and the nodes under them alone.
	firstComments := 0
	for _, node := range kinds[0].Nodes {
		if node.Kind == graphstore.KindComment {
			firstComments++
		}
	}
	if firstComments != issueTreeCommentPageSize || len(kinds) < 2 {
		t.Errorf("%s answers %d comments on its first page and %d pages, want %d and a comments_before", label, firstComments, len(kinds), issueTreeCommentPageSize)
	}
	for index, page := range kinds[1:] {
		within := map[string]model.IssueTreeNode{}
		for _, node := range page.Nodes {
			within[kindFilterAddress(node.Kind, node.ID)] = node
		}
		for _, node := range page.Nodes {
			at := node
			for steps := 0; at.Kind != graphstore.KindComment && at.Parent != nil && steps < len(page.Nodes); steps++ {
				above, held := within[kindFilterAddress(at.Parent.Kind, at.Parent.ID)]
				if !held {
					break
				}
				at = above
			}
			if at.Kind != graphstore.KindComment {
				t.Errorf("page %d of %s answers %s:%s, which stands under no comment of that page", index+2, label, node.Kind, node.ID)
			}
		}
	}
	if page := kindPage[kindFilterAddress(graphstore.KindComment, commentC)]; page < 2 {
		t.Errorf("%s answers the comment C on page %d, want a comments_before page: 205 newer posts come before it", label, page)
	}

	// checks: r1 2, r2 1, each the coverage read's, in the kind read, the read with no view and ?view=spec.
	coverage := readCoverage(t, handler, key)
	specView := treeRead(t, handler, key, "?view=spec", "alice", false)
	specNodes, _ := kindFilterUnion([]treeAnswer{specView})
	for _, requirement := range []struct {
		block, node string
		want        int
	}{{"r1", r1, 2}, {"r2", r2, 1}} {
		listed := requirementOf(t, coverage, requirement.block).Checks
		if listed != requirement.want {
			t.Errorf("the coverage read of %s lists %d checks under %s, want %d: this case proves nothing", key, listed, requirement.block, requirement.want)
		}
		for read, nodes := range map[string]map[string]model.IssueTreeNode{label: kindNodes, "the read with no view of " + key: wholeNodes, "?view=spec of " + key: specNodes} {
			node, held := nodes[kindFilterAddress(refs.BlockNodeKind, requirement.node)]
			if !held || node.Checks == nil || *node.Checks != listed {
				t.Errorf("%s answers %s with checks %v, want %d, the number the coverage read lists under it", read, requirement.block, node.Checks, listed)
			}
		}
	}

	// d1 and a1 answer their members as the Decisions read's entries do.
	entries := readEntries(t, handler, key)
	decided := requireEntry(t, entries, "the decision d1", kindFilterAddress(refs.BlockNodeKind, decision))
	answered := requireEntry(t, entries, "the ask a1", kindFilterAddress(graphstore.KindAsk, asked))
	requireDecisionMembers := func(read string, nodes map[string]model.IssueTreeNode) {
		t.Helper()
		node := nodes[kindFilterAddress(refs.BlockNodeKind, decision)]
		if node.Decision == nil || node.Decision.Uncertainty == nil || *node.Decision.Uncertainty != "medium" ||
			entryJSON(t, node.Decision.Uncertainty) != entryJSON(t, decided.Uncertainty) || entryJSON(t, node.Decision.UncertaintyReason) != entryJSON(t, decided.UncertaintyReason) {
			t.Errorf("%s answers d1's decision %s, want uncertainty medium and the reason %s, as the entries answer them", read, entryJSON(t, node.Decision), entryJSON(t, decided.UncertaintyReason))
		}
		ask := nodes[kindFilterAddress(graphstore.KindAsk, asked)]
		if ask.Ask == nil || entryJSON(t, ask.Ask.Answer) != entryJSON(t, answered.Answer) || entryJSON(t, ask.Ask.Selected) != entryJSON(t, answered.Selected) ||
			entryJSON(t, ask.Ask.AnsweredBy) != entryJSON(t, answered.AnsweredBy) || entryJSON(t, ask.Ask.AnsweredAt) != entryJSON(t, answered.AnsweredAt) ||
			ask.Ask.AnsweredBy == nil || *ask.Ask.AnsweredBy != "alice" {
			t.Errorf("%s answers a1's answer %s, want alice's, as the entries answer it: answer %s, selected %s, answered_by %s, answered_at %s", read,
				entryJSON(t, ask.Ask), entryJSON(t, answered.Answer), entryJSON(t, answered.Selected), entryJSON(t, answered.AnsweredBy), entryJSON(t, answered.AnsweredAt))
		}
	}
	requireDecisionMembers(label, kindNodes)

	// kind=check&kind=result, kind=callout and kind=ask.
	checked, _ := kindFilterUnion(kindFilterPages(t, handler, key, kindFilterQuery("check", "result")))
	for what, address := range map[string]string{
		"h1": kindFilterAddress(refs.BlockNodeKind, h1), "h2": kindFilterAddress(refs.BlockNodeKind, h2),
		"h1's result": kindFilterAddress(graphstore.KindResult, h1Result), "h2's result": kindFilterAddress(graphstore.KindResult, h2Result),
	} {
		if node, held := checked[address]; !held || node.Role != treeRoleTree {
			t.Errorf("kind=check&kind=result of %s answers %s %s as %+v, want it a node of the tree", key, what, address, node)
		}
	}
	if onResult != "" {
		if node, held := checked[kindFilterAddress(graphstore.KindComment, onResult)]; !held || node.Parent == nil || node.Parent.ID != h1Result {
			t.Errorf("kind=check&kind=result of %s answers the comment on h1's result's text as %+v, want it under that result", key, node)
		}
	}
	callouts := treeRead(t, handler, key, kindFilterQuery("callout"), "alice", false)
	for _, node := range callouts.Nodes {
		address := kindFilterAddress(node.Kind, node.ID)
		if address != memo && address != kindFilterAddress(graphstore.KindIssue, key) {
			t.Errorf("kind=callout of %s answers %s (%s, %s), want the memo alone", key, address, node.Supertag, node.Role)
		}
	}
	if len(callouts.nodesNamed(refs.BlockNodeKind, spec.address("m1"))) != 1 {
		t.Errorf("kind=callout of %s answers no memo: %v", key, callouts.addresses())
	}
	askRead, _ := kindFilterUnion(kindFilterPages(t, handler, key, kindFilterQuery("ask")))
	var questions []string
	for address, node := range askRead {
		switch {
		case node.Kind == graphstore.KindAsk && node.Role == treeRoleTree:
			questions = append(questions, node.ID)
		case node.Kind == refs.BlockNodeKind && node.Supertag == graphstore.SupertagAsk && node.Role == treeRoleTree:
			t.Errorf("kind=ask of %s answers the ask block %s as a question of its own, want it only as an ancestor, as context", key, address)
		}
	}
	want := []string{asked, declined}
	slices.Sort(questions)
	slices.Sort(want)
	if !slices.Equal(questions, want) {
		t.Errorf("kind=ask of %s answers the questions %v, want each ask's own node once, %v", key, questions, want)
	}

	// The refusals.
	for what, query := range map[string]string{
		"kind=block": "?kind=block", "an empty kind": "?kind=", "kind=spec": "?kind=spec",
		"kind beside view=spec": "?view=spec&kind=requirement", "kind beside v=1": "?kind=requirement&v=1",
	} {
		kindFilterRefused(t, handler, key, query, what)
	}

	// ?view=decisions: d1 kept, its keeper and its decided_by, a1's answer, the declined ask's answer present and null.
	keepBlock(t, handler, spec.id, "d1", decided.TextSHA256)
	entries = readEntries(t, handler, key)
	decided = requireEntry(t, entries, "the decision d1", kindFilterAddress(refs.BlockNodeKind, decision))
	answered = requireEntry(t, entries, "the ask a1", kindFilterAddress(graphstore.KindAsk, asked))
	viewResponse := treeResponse(t, handler, key, "?view=decisions", "alice", false)
	if viewResponse.Code != http.StatusOK {
		t.Fatalf("?view=decisions of %s: status=%d body=%s", key, viewResponse.Code, viewResponse.Body.String())
	}
	view := treeAnswer{IssueTreeRead: decodeBody[model.IssueTreeRead](t, viewResponse), body: viewResponse.Body.String()}
	viewNodes, _ := kindFilterUnion([]treeAnswer{view})
	requireDecisionMembers("?view=decisions of "+key, viewNodes)
	if node := viewNodes[kindFilterAddress(refs.BlockNodeKind, decision)]; node.Decision == nil || node.Decision.Kept == nil || node.Decision.Kept.KeptBy != "alice" {
		t.Errorf("?view=decisions of %s answers d1's decision %s, want it kept by alice", key, entryJSON(t, node.Decision))
	}
	decider, err := graphstore.CurrentOut(ctx, database.Pool, refs.BlockNodeKind, decision, []string{graphstore.TupleDecidedBy})
	if err != nil || len(decider) != 1 {
		t.Fatalf("read the decided_by of d1: %+v, %v", decider, err)
	}
	if _, held := viewNodes[kindFilterAddress(decider[0].ToKind, decider[0].ToID)]; !held ||
		!view.holdsTuple(graphstore.TupleDecidedBy, treeEnd(refs.BlockNodeKind, decision), treeEnd(decider[0].ToKind, decider[0].ToID)) {
		t.Errorf("?view=decisions of %s answers d1's decider %s:%s and its decided_by tuple %t, want both", key, decider[0].ToKind, decider[0].ToID,
			view.holdsTuple(graphstore.TupleDecidedBy, treeEnd(refs.BlockNodeKind, decision), treeEnd(decider[0].ToKind, decider[0].ToID)))
	}
	raw := decodeBody[struct {
		Nodes []struct {
			Kind string                     `json:"kind"`
			ID   string                     `json:"id"`
			Ask  map[string]json.RawMessage `json:"ask"`
		} `json:"nodes"`
	}](t, viewResponse)
	declinedAnswered := false
	for _, node := range raw.Nodes {
		if node.Kind != graphstore.KindAsk || node.ID != declined {
			continue
		}
		declinedAnswered = true
		for _, member := range []string{"answer", "selected", "answered_by", "answered_at"} {
			if value, present := node.Ask[member]; !present || string(value) != "null" {
				t.Errorf("?view=decisions of %s answers the declined ask's %s as %s (present %t), want it present and null", key, member, value, present)
			}
		}
	}
	if !declinedAnswered {
		t.Errorf("?view=decisions of %s answers no declined ask %s", key, declined)
	}

	// r3, written with no check, answers 0; a version of the spec query written while h1 and h2
	// serve r1 answers r1's checks 2, and the head 1 once h2 serves r2 alone.
	editSpec(t, handler, key, []map[string]any{{"op": "insert", "after": "end", "markdown": ":::requirement{#r3}\nThe board names its reader.\n:::\n"}})
	_, pinned := pinnedSpecQueryVersion(t, database, key)
	head := treeRead(t, handler, key, "?view=spec", "alice", false)
	if node := head.nodesNamed(refs.BlockNodeKind, spec.address("r3")); len(node) != 1 || node[0].Checks == nil || *node[0].Checks != 0 {
		t.Errorf("?view=spec of %s answers r3, which no check serves, as %+v, want checks 0", key, node)
	}
	uploadAs(t, handler, child.Key, "verification.md", checkServing("h2", r2Ref), "alice")
	head = treeRead(t, handler, key, "?view=spec", "alice", false)
	saved := treeRead(t, handler, key, fmt.Sprintf("?view=spec&v=%d", pinned), "alice", false)
	headR1, savedR1 := head.nodesNamed(refs.BlockNodeKind, r1), saved.nodesNamed(refs.BlockNodeKind, r1)
	if len(headR1) != 1 || len(savedR1) != 1 || headR1[0].Checks == nil || savedR1[0].Checks == nil || *headR1[0].Checks != 1 || *savedR1[0].Checks != 2 {
		var atHead, atSaved any
		if len(headR1) == 1 {
			atHead = headR1[0].Checks
		}
		if len(savedR1) == 1 {
			atSaved = savedR1[0].Checks
		}
		t.Errorf("r1 of %s answers checks %s at the head and %s at version %d of the spec query, want 1, h1 alone once h2 serves r2, and 2, h1 and h2 at that version",
			key, entryJSON(t, atHead), entryJSON(t, atSaved), pinned)
	}
}
