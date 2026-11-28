package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// occurrenceTreeNode is what TestADocumentAnchorsOccurrenceCountsOverTheTextOfTheNodeItsCommentHangsUnder
// reads of a node of the tree read: what c-node-anchor reads of it (nodeAnchorTreeNode) and its
// text, in whose rendered words a client places the quote of each comment that stands under it.
type occurrenceTreeNode struct {
	nodeAnchorTreeNode
	Text string `json:"text"`
}

// TestADocumentAnchorsOccurrenceCountsOverTheTextOfTheNodeItsCommentHangsUnder holds the tree
// read's occurrence of a document anchor to the plain text of the node the read hangs the comment
// under (R31 of the issue-tree spec, v258): its words as pmdoc.QuoteFinder.FindInBlock reads them,
// without a directive line or its attributes. So a client places the quote from that node's text
// alone: the occurrence-th match of the quote in the rendered words of the text the read answers of
// that node is the words the comment was written on. No node stands for a paragraph of a typed
// block, so a comment anchored in one stands under the typed block. Each typed block below holds
// two paragraphs, each holding limit once, written each after words of its own, which the test
// places the quote by:
//   - the requirement r1 of the spec. alice comments on the limit of its second paragraph and on
//     that of its first, each anchored by the spec and the paragraph's block_id, and an agent
//     comments on the second's, a thread no person wrote in, which the read adds beside the walk's
//     (placeTextComments);
//   - the decision d1, which an agent writes into the spec closing with the reading g1, which holds
//     limit too: the decision's text leaves the readings it closes with out (refs.DecisionWords),
//     and they stand after each of its own words (pmdoc.TypedContent), so no match before a mark
//     in its own words is one the decision's text leaves out. alice comments on the limit of its
//     second paragraph, and on the limit of g1, a node of its own, under which that comment stands;
//   - the check c1 of the issue's Verification document. alice comments on the limit of its second
//     paragraph;
//   - the check c2 of that document, whose run attribute holds limit too, so the text the read
//     answers of c2 holds limit in its directive line, before its words. alice comments on the
//     limit of its second paragraph: the attribute is no word of c2, and counts for nothing.
//
// The issue's own fragment holds a paragraph, a node of its own, that holds limit twice, and alice
// comments on its second limit: its occurrence is the count within its block_id, as before.
//
// The read with no view and each view that answers the typed block - ?view=spec for r1,
// ?view=decisions for d1, ?view=verification for c1 and c2 - each through the route, which reads
// the issue's head rows, and through the un-cut walk (readUncutIssueTree), the issue's mark taken
// away for its length, answer each comment under its typed block, its anchor's block_id the
// paragraph it was written on as stored, and its occurrence counted over that block's words: 1 for
// each comment on a second paragraph, where the count within that paragraph answered 0, and 0 for
// the one on r1's first. The read with no view answers the comment on g1 under g1 at occurrence 0,
// and the comment in the fragment under its paragraph's own block at occurrence 1. In each of them
// the occurrence-th match of limit in the rendered words of the text the read answers of the node
// the comment stands under - that text without its directive lines - follows the words written
// before the limit the comment was written on.
func TestADocumentAnchorsOccurrenceCountsOverTheTextOfTheNodeItsCommentHangsUnder(t *testing.T) {
	t.Parallel()
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	const (
		readWords    = "A read takes a limit."
		writeWords   = "A write takes a limit too."
		weighWords   = "We weigh a limit first."
		settleWords  = "We settle a limit last."
		readingWords = "The agent reads the limit as a bound."
		probeWords   = "A check probes a limit."
		proveWords   = "A check proves a limit."
		boundWords   = "A check bounds a limit."
		capWords     = "A check caps a limit."
		noteWords    = "A note takes a limit, and a list takes a limit too."
	)
	key := createInteractionIssue(t, handler, "OCC", "Occurrences over a node's text",
		"An opening paragraph.\n\n:::requirement{#r1}\n"+readWords+"\n\n"+writeWords+"\n:::\n").Key
	commentsOnTextAgentEdits(t, handler, key,
		"::::decision{#d1}\n"+weighWords+"\n\n"+settleWords+"\n\n:::reading{#g1}\n"+readingWords+"\n:::\n::::\n")
	serves := "dispatch://" + key + "/spec#b-r1"
	uploadDocument(t, handler, "/api/v1/issues/"+key, "verification.md",
		":::check{#c1 serves=\""+serves+"\" surface=\"cli\" run=\"go test ./...\"}\n"+probeWords+"\n\n"+proveWords+"\n:::\n\n"+
			":::check{#c2 serves=\""+serves+"\" surface=\"cli\" run=\"go test -run limit ./...\"}\n"+boundWords+"\n\n"+capWords+"\n:::\n")
	spec, verification := readDecisionDocument(t, database, key, "spec"), readDecisionDocument(t, database, key, model.ArtifactSlugVerification)
	requirement, decision, reading := spec.address("r1"), spec.address("d1"), spec.address("g1")
	check, capped := verification.address("c1"), verification.address("c2")
	// blockHolding is the id of the one block of blocks, a document's blocks by their Markdown, whose
	// text trimmed is words.
	blockHolding := func(blocks map[string]string, words string) string {
		t.Helper()
		var found []string
		for text, id := range blocks {
			if strings.TrimSpace(text) == words {
				found = append(found, id)
			}
		}
		if len(found) != 1 {
			t.Fatalf("the blocks %v of %s hold %q, want one", found, key, words)
		}
		return found[0]
	}
	specBlocks, verificationBlocks := blockIDsByText(t, handler, spec.id), blockIDsByText(t, handler, verification.id)

	// The issue's tree artifact's own fragment is read through its text route, its blocks with
	// blocks=true; the blocks route answers a doc alone.
	tree, err := issueTreeOf(context.Background(), database.Pool, key)
	if err != nil || tree == "" {
		t.Fatalf("issue %s holds the tree %q (%v), want the tree it was born with", key, tree, err)
	}
	if edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+tree+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": noteWords}}, "summary": "a note",
	}, "alice"); edited.Code != http.StatusOK {
		t.Fatalf("write a paragraph into the issue's own fragment: status=%d body=%s", edited.Code, edited.Body.String())
	}
	fragment := decodeBody[struct {
		Markdown string                `json:"markdown"`
		Blocks   []model.ArtifactBlock `json:"blocks"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+tree+"/text?blocks=true", nil, "alice"))
	fragmentBlocks := make(map[string]string, len(fragment.Blocks))
	for _, block := range fragment.Blocks {
		fragmentBlocks[fragment.Markdown[block.From:block.To]] = block.ID
	}
	noteBlock := blockHolding(fragmentBlocks, noteWords)

	// onText is one comment anchored by a document and a block_id on a limit: what the test names it,
	// its id, the block it was written on, the node the read hangs it under, the words that stand
	// right before the limit it was written on, the occurrence the read counts over that node's
	// words, and whether that node's text holds limit in a directive line too, before its words.
	type onText struct {
		name, id, block, under, before string
		occurrence                     int
		inAttributes                   bool
	}
	anchored := func(document, block string, occurrence int) map[string]any {
		return map[string]any{"artifact": document, "block_id": block, "quote": "limit", "occurrence": occurrence}
	}
	person := func(comment *onText, document string, occurrence int) {
		t.Helper()
		comment.id = createBlockAnchoredComment(t, handler, key, anchored(document, comment.block, occurrence)).ID
	}
	second := onText{name: "alice's comment on the limit of r1's second paragraph", block: blockHolding(specBlocks, writeWords),
		under: requirement, before: "A write takes a ", occurrence: 1}
	person(&second, spec.id, 0)
	first := onText{name: "alice's comment on the limit of r1's first paragraph", block: blockHolding(specBlocks, readWords),
		under: requirement, before: "A read takes a ", occurrence: 0}
	person(&first, spec.id, 0)
	agent := onText{name: "the agent's comment on the limit of r1's second paragraph", block: second.block,
		under: requirement, before: second.before, occurrence: 1}
	agent.id = createThreadComment(t, handler, key, map[string]any{
		"body": "Which limit binds the write?", "anchor": anchored(spec.id, agent.block, 0), "actor": sessionActor(),
	}, "").ID
	decided := onText{name: "alice's comment on the limit of d1's second paragraph", block: blockHolding(specBlocks, settleWords),
		under: decision, before: "We settle a ", occurrence: 1}
	person(&decided, spec.id, 0)
	onReading := onText{name: "alice's comment on the limit of the reading g1", block: blockHolding(specBlocks, readingWords),
		under: reading, before: "The agent reads the ", occurrence: 0}
	person(&onReading, spec.id, 0)
	checked := onText{name: "alice's comment on the limit of c1's second paragraph", block: blockHolding(verificationBlocks, proveWords),
		under: check, before: "A check proves a ", occurrence: 1}
	person(&checked, verification.id, 0)
	cappedCheck := onText{name: "alice's comment on the limit of c2's second paragraph, after the limit of its run", block: blockHolding(verificationBlocks, capWords),
		under: capped, before: "A check caps a ", occurrence: 1, inAttributes: true}
	person(&cappedCheck, verification.id, 0)
	note := onText{name: "alice's comment on the second limit of the fragment's paragraph", block: noteBlock,
		under: anchoredBlockAddress(t, database, tree, noteBlock), before: "a list takes a ", occurrence: 1}
	person(&note, tree, 1)

	// nodesOf is the tree read answered with status and body, which the test names what, by each
	// node's kind and id.
	nodesOf := func(what string, status int, body []byte) map[string]occurrenceTreeNode {
		t.Helper()
		if status != http.StatusOK {
			t.Fatalf("%s of %s: status=%d body=%s", what, key, status, body)
		}
		var read struct {
			Nodes []occurrenceTreeNode `json:"nodes"`
		}
		if err := json.Unmarshal(body, &read); err != nil {
			t.Fatalf("decode %s of %s: %v", what, key, err)
		}
		nodes := make(map[string]occurrenceTreeNode, len(read.Nodes))
		for _, node := range read.Nodes {
			nodes[node.Kind+":"+node.ID] = node
		}
		return nodes
	}
	// request is the tree read of key with query, "" for the read with no view, as alice.
	request := func(query string) *http.Request {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/issues/"+key+"/tree"+query, nil)
		request.Header.Set("X-Dispatch-User", "alice")
		return request
	}
	// routeRead is the tree read with query through the route; uncutRead is the same read through
	// the un-cut walk, with the issue's mark taken away for its length so it walks the tuples
	// (headRig.withoutMark).
	routeRead := func(what, query string) map[string]occurrenceTreeNode {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request(query))
		return nodesOf(what, response.Code, response.Body.Bytes())
	}
	uncutRead := func(what, query string) map[string]occurrenceTreeNode {
		t.Helper()
		status, body := headRig{t: t, database: database}.withoutMark(t, key, false, func() (int, []byte) {
			return uncutTreeResponse(served, request(query), key)
		})
		return nodesOf(what, status, body)
	}
	// renderedWords is text, a node's text as the read answers it, with each line that opens or
	// closes a directive left out - its opening line with its attributes, and its closing fence - as
	// a client that renders the text reads its words (spec v258). The test's texts hold no inline
	// Markdown syntax, which a rendering leaves out too.
	renderedWords := func(text string) string {
		return strings.Join(slices.DeleteFunc(strings.Split(text, "\n"), func(line string) bool {
			return strings.HasPrefix(strings.TrimSpace(line), ":::")
		}), "\n")
	}
	// limits is where each match of limit in text starts, in text's order.
	limits := func(text string) []int {
		var matches []int
		for from := 0; ; {
			at := strings.Index(text[from:], "limit")
			if at < 0 {
				return matches
			}
			matches = append(matches, from+at)
			from += at + 1
		}
	}
	// requireOccurrence fails t unless read, which the test names what, answers comment under the
	// node it hangs under, with its anchor's block_id the block it was written on, the quote limit,
	// not orphaned, and the occurrence counted over that node's words; and unless the occurrence-th
	// match of limit in the rendered words of the text read answers of that node follows the words
	// written before the limit the comment was written on, where a client places the quote.
	requireOccurrence := func(what string, read map[string]occurrenceTreeNode, comment onText) {
		t.Helper()
		anchors := make(map[string]nodeAnchorTreeNode, len(read))
		for target, node := range read {
			anchors[target] = node.nodeAnchorTreeNode
		}
		node := requireAnchoredUnder(t, anchors, comment.name+" in "+what, comment.id, refs.BlockNodeKind, comment.under)
		if _, held := read[graphstore.KindComment+":"+comment.id]; !held {
			return
		}
		anchor := node.Anchor
		switch {
		case anchor == nil:
			t.Errorf("%s answers %s %s with no anchor", what, comment.name, comment.id)
			return
		case anchor.BlockID == nil || *anchor.BlockID != comment.block || anchor.Quote != "limit" || anchor.Orphaned:
			encoded, _ := json.Marshal(anchor)
			t.Errorf("%s answers the anchor of %s %s as %s, want the block %s it was written on, the quote limit and not orphaned",
				what, comment.name, comment.id, encoded, comment.block)
		case anchor.Occurrence == nil:
			t.Errorf("%s answers the anchor of %s %s with no occurrence, want %d, counted over the words of %s",
				what, comment.name, comment.id, comment.occurrence, comment.under)
			return
		case *anchor.Occurrence != comment.occurrence:
			t.Errorf("%s answers the anchor of %s %s with occurrence %d, want %d, counted over the words of %s",
				what, comment.name, comment.id, *anchor.Occurrence, comment.occurrence, comment.under)
		}
		host, held := read[refs.BlockNodeKind+":"+comment.under]
		if !held {
			t.Errorf("%s answers no node %s, which %s %s stands under", what, comment.under, comment.name, comment.id)
			return
		}
		words := renderedWords(host.Text)
		matches := limits(words)
		if inText := len(limits(host.Text)); comment.inAttributes && inText <= len(matches) {
			t.Errorf("%s answers the text of %s holding limit %d times and its words %d times, want more in the text, whose directive line holds it too, else this case proves nothing: %q",
				what, comment.under, inText, len(matches), host.Text)
		}
		switch occurrence := *anchor.Occurrence; {
		case occurrence >= len(matches):
			t.Errorf("%s answers the anchor of %s %s with occurrence %d, and the words of the text it answers of %s hold limit %d times: %q",
				what, comment.name, comment.id, occurrence, comment.under, len(matches), words)
		case !strings.HasSuffix(words[:matches[occurrence]], comment.before):
			t.Errorf("%s answers the anchor of %s %s with occurrence %d, which places it in the words of the text it answers of %s at %q, want the limit after %q: %q",
				what, comment.name, comment.id, occurrence, comment.under, words[max(0, matches[occurrence]-len(comment.before)):matches[occurrence]+len("limit")],
				comment.before, host.Text)
		}
	}
	for _, form := range []struct {
		name, query string
		comments    []onText
	}{
		{"read with no view", "", []onText{second, first, agent, decided, onReading, checked, cappedCheck, note}},
		{"?view=spec read", "?view=" + graphstore.QuerySpec, []onText{second, first, agent}},
		{"?view=decisions read", "?view=" + graphstore.QueryDecisions, []onText{decided}},
		{"?view=verification read", "?view=" + graphstore.QueryVerification, []onText{checked, cappedCheck}},
	} {
		for what, read := range map[string]map[string]occurrenceTreeNode{
			"the route's " + form.name:       routeRead("the route's "+form.name, form.query),
			"the un-cut walk's " + form.name: uncutRead("the un-cut walk's "+form.name, form.query),
		} {
			for _, comment := range form.comments {
				requireOccurrence(what, read, comment)
			}
		}
	}
}
