package api

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// The words of c-comments-on-text's issues: the requirement and the decision of the issue born
// converted, and those of the issue that is not converted.
const (
	commentsOnTextRequirement      = "The list orders each urgent ask first, then by its age."
	commentsOnTextDecision         = "We sort the asks on the server, in one query."
	commentsOnTextOtherRequirement = "The board shows each open ask."
	commentsOnTextOtherDecision    = "We read the board from one table."
)

// commentsOnTextNode is what c-comments-on-text reads of a node of a tree read: its kind, id,
// supertag and role, the node it stands under, and the anchor a comment on text answers.
type commentsOnTextNode struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Supertag string `json:"supertag"`
	Role     string `json:"role"`
	Parent   *struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	} `json:"parent"`
	Anchor *struct {
		Quote    string `json:"quote"`
		Orphaned bool   `json:"orphaned"`
	} `json:"anchor"`
}

// commentsOnTextRead is one tree read as c-comments-on-text reads it, with the words its failures
// name it by.
type commentsOnTextRead struct {
	label          string
	Converted      bool                 `json:"converted"`
	Nodes          []commentsOnTextNode `json:"nodes"`
	CommentsBefore json.RawMessage      `json:"comments_before"`
}

// commentOnText is one comment c-comments-on-text writes on the text of a node: what the test
// names it, its id, the store address of the node whose text its thread is on, the comment it
// answers ("" for the first comment of its thread), the quote its anchor holds, and whether a
// person wrote in its thread.
type commentOnText struct {
	name    string
	id      string
	on      string
	answers string
	quote   string
	person  bool
}

// commentsOnTextGet reads the tree of key with query as alice, and answers whether it answered
// 200; a read that answers anything else fails t, naming it label.
func commentsOnTextGet(t *testing.T, handler http.Handler, key, query, label string) (commentsOnTextRead, bool) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/tree"+query, nil, "alice")
	if response.Code != http.StatusOK {
		t.Errorf("%s (GET /api/v1/issues/%s/tree%s): status=%d body=%s", label, key, query, response.Code, response.Body.String())
		return commentsOnTextRead{label: label}, false
	}
	read := decodeBody[commentsOnTextRead](t, response)
	read.label = label
	return read, true
}

// commentsOnTextPages is every page of the tree read of key with query: the read, then the read
// from each comments_before the page before answered, until a page answers none.
func commentsOnTextPages(t *testing.T, handler http.Handler, key, query, label string) []commentsOnTextRead {
	t.Helper()
	var pages []commentsOnTextRead
	target := query
	for page := 1; page <= 10; page++ {
		read, ok := commentsOnTextGet(t, handler, key, target, fmt.Sprintf("page %d of %s", page, label))
		if !ok {
			return pages
		}
		pages = append(pages, read)
		position, _ := read.position()
		if position == "" {
			return pages
		}
		separator := "?"
		if query != "" {
			separator = "&"
		}
		target = query + separator + "comments_before=" + url.QueryEscape(position)
	}
	t.Errorf("%s still answers a comments_before after 10 pages", label)
	return pages
}

// commentsOnTextListed is GET /api/v1/issues/{key}/comments as alice, by id.
func commentsOnTextListed(t *testing.T, handler http.Handler, key string) map[string]model.Comment {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/comments", nil, "alice")
	succeeded(t, "list the comments of "+key, response)
	listed := map[string]model.Comment{}
	for _, comment := range decodeBody[[]model.Comment](t, response) {
		listed[comment.ID] = comment
	}
	return listed
}

// commentsOnTextAgentEdits inserts markdown at the end of the spec of key through the spec's edit
// route, written by the agent's session.
func commentsOnTextAgentEdits(t *testing.T, handler http.Handler, key, markdown string) {
	t.Helper()
	response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/artifacts/spec/edits", map[string]any{
		"ops":     []map[string]any{{"op": "insert", "after": "end", "markdown": markdown}},
		"summary": "the agent's decision",
		"actor":   sessionActor(),
	})
	if response.Code != http.StatusOK {
		t.Fatalf("the agent writes into the spec of %s: status=%d body=%s", key, response.Code, response.Body.String())
	}
}

// named is every node of read of kind and id, in its order.
func (read commentsOnTextRead) named(kind, id string) []commentsOnTextNode {
	var nodes []commentsOnTextNode
	for _, node := range read.Nodes {
		if node.Kind == kind && node.ID == id {
			nodes = append(nodes, node)
		}
	}
	return nodes
}

// addresses is each node's `<kind>:<id>`, in order.
func (read commentsOnTextRead) addresses() []string {
	addresses := make([]string, len(read.Nodes))
	for index, node := range read.Nodes {
		addresses[index] = node.Kind + ":" + node.ID
	}
	return addresses
}

// comments is the id of each comment node of read, in order.
func (read commentsOnTextRead) comments() []string {
	var ids []string
	for _, node := range read.Nodes {
		if node.Kind == graphstore.KindComment {
			ids = append(ids, node.ID)
		}
	}
	return ids
}

// position is read's comments_before, "" where it is null, and whether the answer holds the
// member as a position or null.
func (read commentsOnTextRead) position() (string, bool) {
	if len(read.CommentsBefore) == 0 {
		return "", false
	}
	var position *string
	if err := json.Unmarshal(read.CommentsBefore, &position); err != nil {
		return "", false
	}
	if position == nil {
		return "", true
	}
	return *position, true
}

// requireNoStatusNote fails t where read answers the agent's status note status, a comment tied
// to no text in whose thread no person wrote (R2).
func (read commentsOnTextRead) requireNoStatusNote(t *testing.T, status string) {
	t.Helper()
	if len(read.named(graphstore.KindComment, status)) > 0 {
		t.Errorf("the status note %s, an agent's comment tied to no text, is found in %s: no read answers it", status, read.label)
	}
}

// requireComments holds read to the comments the test wrote on text (R2, R8, R31): it answers each
// node of returned, and each comment on the text of one of them once, under the node its thread is
// on or under the comment it answers, with the quote its anchor holds where it is the first of its
// thread, as context in a view and where no person wrote in its thread, and as a node of the tree
// otherwise. It answers no comment on the text of a node it does not return, and never the status
// note.
func (read commentsOnTextRead) requireComments(t *testing.T, view bool, returned []string, comments []commentOnText, status string) {
	t.Helper()
	for _, node := range returned {
		if len(read.named(refs.BlockNodeKind, node)) == 0 {
			t.Errorf("%s answers no node %s, which it returns: it answers %v", read.label, node, read.addresses())
		}
	}
	for _, comment := range comments {
		held := read.named(graphstore.KindComment, comment.id)
		if !slices.Contains(returned, comment.on) {
			if len(held) > 0 {
				t.Errorf("%s answers %s %s, on the text of %s, a node it does not return: a comment on text comes only with the node it is on",
					read.label, comment.name, comment.id, comment.on)
			}
			continue
		}
		under, want := model.IssueTreeEnd{Kind: refs.BlockNodeKind, ID: comment.on}, fmt.Sprintf("under %s with its anchor quote %q", comment.on, comment.quote)
		if comment.answers != "" {
			under, want = model.IssueTreeEnd{Kind: graphstore.KindComment, ID: comment.answers}, "under the comment it answers, "+comment.answers
		}
		if len(held) != 1 {
			if len(held) == 0 {
				t.Errorf("%s %s is missing from %s: want it %s; the read answers %v", comment.name, comment.id, read.label, want, read.addresses())
			} else {
				t.Errorf("%s answers %s %s %d times, want it once, %s", read.label, comment.name, comment.id, len(held), want)
			}
			continue
		}
		node := held[0]
		if node.Parent == nil || node.Parent.Kind != under.Kind || node.Parent.ID != under.ID {
			t.Errorf("%s answers %s %s under %+v, want it %s", read.label, comment.name, comment.id, node.Parent, want)
		}
		if comment.answers == "" && (node.Anchor == nil || node.Anchor.Quote != comment.quote) {
			t.Errorf("%s answers %s %s with the anchor %+v, want it %s", read.label, comment.name, comment.id, node.Anchor, want)
		}
		role := treeRoleTree
		if view || !comment.person {
			role = treeRoleContext
		}
		if node.Role != role {
			t.Errorf("%s answers %s %s as %q, want %q: a comment no person wrote in is context in every read, and every comment on text is context in a view",
				read.label, comment.name, comment.id, node.Role, role)
		}
	}
	read.requireNoStatusNote(t, status)
}

// requireCommentPage fails t unless the comments read answers are exactly want, each once.
// newest, the person's newest reply, is named on its own where the page lacks it.
func (read commentsOnTextRead) requireCommentPage(t *testing.T, want []string, newest string) {
	t.Helper()
	held := map[string]int{}
	for _, id := range read.comments() {
		held[id]++
	}
	for _, id := range want {
		switch {
		case held[id] == 0 && id == newest:
			t.Errorf("%s lacks the person's newest reply %s, one of the %d comments of the 251 on the requirement's text it should answer", read.label, id, len(want))
		case held[id] == 0:
			t.Errorf("%s lacks the comment %s, one of the %d comments of the 251 on the requirement's text it should answer", read.label, id, len(want))
		case held[id] > 1:
			t.Errorf("%s answers the comment %s %d times, want it once", read.label, id, held[id])
		}
	}
	for id := range held {
		if !slices.Contains(want, id) {
			t.Errorf("%s answers the comment %s, which is none of the %d comments on the requirement's text it should answer", read.label, id, len(want))
		}
	}
}

// TestEveryTreeReadCarriesTheCommentsOnItsNodesText is c-comments-on-text (R2, R8, R31). A person
// creates an issue born converted, whose spec holds the requirement r1 and, written by the agent's
// session, the decision d1, and whose Verification document holds the check c1. The agent writes a
// comment anchored in r1's text, in whose thread no person writes, and a status note tied to no
// text; the person comments on c1's text and the agent replies in that thread; the person comments
// on d1's words. The read with no view, ?view=spec, ?view=verification, ?view=decisions and
// ?kind=requirement each answer each of those comments that is on the text of a node the read
// returns, under that node with its anchor's quote, each reply under the comment it answers: as
// context where no person wrote in its thread and in every view, as a node of the tree otherwise;
// no comment on the text of a node the read does not return, and never the status note. Before
// any person writes in the thread of the agent's comment on r1, a criterion whose parent names
// that comment is refused 400 INVALID_OP through the spec's edit route, as no conversation node,
// and no criterion is stored.
//
// A second live process whose capabilities hold no issue_room makes the next issue born not
// converted (createDigestIssue), whose spec holds a requirement and the agent's decision: the
// person comments on the decision's words and the agent on the requirement's text. It answers
// converted false; its read with no view answers both comments under their nodes, and its
// ?view=decisions, which returns no requirement, the person's alone, under the decision.
//
// The agent then writes 249 more comments on r1's text and the person one reply to the first of
// them. ?view=spec answers the 200 newest of the 251 comments on r1's text, the reply among them,
// with the comments_before of the oldest of them; the read with that comments_before beside
// view=spec answers the 51 older ones and comments_before null. The read with no view and
// ?kind=requirement answer the reply as a node of the tree under the comment it answers.
//
// The person then changes r1's words so they no longer hold the quote of the agent's first
// comment: its anchor is marked orphaned in the store, its anchored_in and its part_of to the node
// it was on are gone, and no page of the read with no view, of ?view=spec and of
// ?kind=requirement&kind=comment answers it, while GET /api/v1/issues/{key}/comments lists it
// with its quote and orphaned true, and lists every comment the test wrote on both issues.
func TestEveryTreeReadCarriesTheCommentsOnItsNodesText(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "TEXT")
	issue := createDigestIssue(t, handler, database, "TEXT", "Comments on text", requirementMarkdown("r1", commentsOnTextRequirement), false)
	key := issue.Key
	commentsOnTextAgentEdits(t, handler, key, ":::decision{#d1}\n"+commentsOnTextDecision+"\n:::\n")
	uploadDocument(t, handler, "/api/v1/issues/"+key, "verification.md", checkServing("c1", "dispatch://"+key+"/spec#b-r1"))
	spec, verification := readDecisionDocument(t, database, key, "spec"), readDecisionDocument(t, database, key, "verification-md")
	requirement, decision, check := spec.address("r1"), spec.address("d1"), verification.address("c1")
	anchor := func(document, block, quote string) map[string]any {
		return map[string]any{"artifact": document, "block_id": block, "quote": quote}
	}
	agent := func(input map[string]any) string {
		t.Helper()
		input["actor"] = sessionActor()
		return createThreadComment(t, handler, key, input, "").ID
	}
	person := func(input map[string]any) string {
		t.Helper()
		return createThreadComment(t, handler, key, input, "alice").ID
	}
	label := func(query, owner string) string {
		if query == "" {
			return "the read with no view of " + owner
		}
		return query + " of " + owner
	}
	converted := "the converted issue " + key

	first := commentOnText{name: "the agent's comment on the requirement's text", on: requirement, quote: "then by its age"}
	first.id = agent(map[string]any{"body": "The age breaks a tie only.", "anchor": anchor(spec.id, "r1", first.quote)})
	status := agent(map[string]any{"body": "Round 1 is done."})

	// No node hangs under a comment of an agent that no person answered (R2).
	refused := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/artifacts/spec/edits", map[string]any{
		"ops":     []map[string]any{{"op": "insert", "after": "end", "markdown": criterionUnder("dispatch://" + key + "/comment/" + first.id)}},
		"summary": "a criterion under the agent's comment",
	}, "alice")
	var refusal refusalAnswer
	_ = json.Unmarshal(refused.Body.Bytes(), &refusal)
	if refused.Code != http.StatusBadRequest || refusal.Code != "INVALID_OP" || !strings.Contains(refusal.Error, "no conversation node") {
		t.Errorf("a criterion whose parent names %s %s, in whose thread no person wrote, through the spec's edit route: status=%d body=%s, want 400 INVALID_OP saying it is no conversation node",
			first.name, first.id, refused.Code, refused.Body.String())
	}
	var criteria int
	if err := database.Pool.QueryRow(ctx, `select count(*) from graph_nodes where kind = $1 and supertag = 'criterion' and id like $2`,
		refs.BlockNodeKind, key+"/%").Scan(&criteria); err != nil {
		t.Fatalf("count the criteria of %s: %v", key, err)
	}
	if criteria != 0 {
		t.Errorf("the refused edit of the spec of %s stores %d criterion nodes, want none", key, criteria)
	}

	checked := commentOnText{name: "the person's comment on the check's text", on: check, quote: "proves it", person: true}
	checked.id = person(map[string]any{"body": "Which run proves it?", "anchor": anchor(verification.id, "c1", checked.quote)})
	reply := commentOnText{name: "the agent's reply in the person's thread on the check", on: check, answers: checked.id, person: true}
	reply.id = agent(map[string]any{"body": "The tester's run proves it.", "reply_to": checked.id})
	decided := commentOnText{name: "the person's comment on the decision's words", on: decision, quote: "in one query", person: true}
	decided.id = person(map[string]any{"body": "One query holds under load?", "anchor": anchor(spec.id, "d1", decided.quote)})
	comments := []commentOnText{first, checked, reply, decided}

	for _, read := range []struct {
		query    string
		view     bool
		returned []string
	}{
		{"", false, []string{requirement, check, decision}},
		{"?view=spec", true, []string{requirement}},
		{"?view=verification", true, []string{check, requirement}},
		{"?view=decisions", true, []string{decision}},
		{"?kind=requirement", false, []string{requirement}},
	} {
		answer, ok := commentsOnTextGet(t, handler, key, read.query, label(read.query, converted))
		if !ok {
			continue
		}
		answer.requireComments(t, read.view, read.returned, comments, status)
		if read.query != "?kind=requirement" {
			continue
		}
		for _, node := range answer.Nodes {
			if node.Supertag == "check" || node.Supertag == refs.DecisionSupertag {
				t.Errorf("%s answers the %s node %s:%s, which is no requirement and no ancestor of one (R30)", answer.label, node.Supertag, node.Kind, node.ID)
			}
		}
	}

	other := createDigestIssue(t, handler, database, "TEXT", "Comments on the text of an issue that is not converted",
		requirementMarkdown("r1", commentsOnTextOtherRequirement), true)
	commentsOnTextAgentEdits(t, handler, other.Key, ":::decision{#d1}\n"+commentsOnTextOtherDecision+"\n:::\n")
	otherSpec := readDecisionDocument(t, database, other.Key, "spec")
	otherDecided := commentOnText{name: "the person's comment on the decision's words", on: otherSpec.address("d1"), quote: "from one table", person: true}
	otherDecided.id = createThreadComment(t, handler, other.Key, map[string]any{
		"body": "One table for every board?", "anchor": anchor(otherSpec.id, "d1", otherDecided.quote),
	}, "alice").ID
	otherFirst := commentOnText{name: "the agent's comment on the requirement's text", on: otherSpec.address("r1"), quote: "each open ask"}
	otherFirst.id = createThreadComment(t, handler, other.Key, map[string]any{
		"body": "Closed asks stay off the board.", "anchor": anchor(otherSpec.id, "r1", otherFirst.quote), "actor": sessionActor(),
	}, "").ID
	unconverted := "the issue that is not converted, " + other.Key
	for _, read := range []struct {
		query    string
		view     bool
		returned []string
	}{
		{"", false, []string{otherSpec.address("r1"), otherSpec.address("d1")}},
		{"?view=decisions", true, []string{otherSpec.address("d1")}},
	} {
		answer, ok := commentsOnTextGet(t, handler, other.Key, read.query, label(read.query, unconverted))
		if !ok {
			continue
		}
		if answer.Converted {
			t.Errorf("%s answers converted true, want false: a live process of the release before held it unconverted at its create", answer.label)
		}
		answer.requireComments(t, read.view, read.returned, []commentOnText{otherDecided, otherFirst}, status)
	}

	// 249 more comments on the requirement's text and the person's reply to the first of them: 251
	// comments on the text of r1, which the spec view pages by each comment's own time.
	notes := make([]string, 0, 249)
	for index := range 249 {
		notes = append(notes, agent(map[string]any{
			"body": fmt.Sprintf("Note %d on the urgent asks.", index+1), "anchor": anchor(spec.id, "r1", "each urgent ask"),
		}))
	}
	newest := person(map[string]any{"body": "The urgent asks come first, yes.", "reply_to": notes[0]})
	listed := commentsOnTextListed(t, handler, key)
	onText := append([]string{first.id, newest}, notes...)
	for _, id := range onText {
		if _, held := listed[id]; !held {
			t.Fatalf("the comments read of %s lists no comment %s, which the test wrote on the requirement's text", key, id)
		}
	}
	slices.SortFunc(onText, func(left, right string) int {
		return cmp.Or(listed[right].CreatedAt.Compare(listed[left].CreatedAt), strings.Compare(right, left))
	})
	if onText[0] != newest {
		t.Fatalf("the person's reply %s is not the newest comment on the requirement's text of %s: %s is", newest, key, onText[0])
	}
	positionOf := func(id string) string {
		return listed[id].CreatedAt.UTC().Format("2006-01-02T15:04:05.000000Z") + "," + id
	}

	if page, ok := commentsOnTextGet(t, handler, key, "?view=spec", "page 1 of the spec view of "+key); ok {
		page.requireCommentPage(t, onText[:200], newest)
		if position, held := page.position(); !held || position != positionOf(onText[199]) {
			t.Errorf("%s answers comments_before %s, want %q, the position of the oldest of its 200 comments", page.label, page.CommentsBefore, positionOf(onText[199]))
		}
	}
	if page, ok := commentsOnTextGet(t, handler, key, "?view=spec&comments_before="+url.QueryEscape(positionOf(onText[199])), "page 2 of the spec view of "+key); ok {
		page.requireCommentPage(t, onText[200:], newest)
		if position, held := page.position(); !held || position != "" {
			t.Errorf("%s answers comments_before %s, want null: no older comment on the requirement's text remains", page.label, page.CommentsBefore)
		}
	}
	for _, read := range []struct {
		query string
		role  string
	}{
		{"", treeRoleTree},
		{"?kind=requirement", treeRoleTree},
		{"?view=spec", treeRoleContext},
	} {
		page, ok := commentsOnTextGet(t, handler, key, read.query, label(read.query, converted))
		if !ok {
			continue
		}
		held := page.named(graphstore.KindComment, newest)
		if len(held) != 1 {
			t.Errorf("%s answers the person's reply %s %d times, want it once, as %q, under the comment it answers, %s",
				page.label, newest, len(held), read.role, notes[0])
		} else if node := held[0]; node.Role != read.role || node.Parent == nil || node.Parent.Kind != graphstore.KindComment || node.Parent.ID != notes[0] {
			t.Errorf("%s answers the person's reply %s as %q under %+v, want it %q, under the comment it answers, %s",
				page.label, newest, node.Role, node.Parent, read.role, notes[0])
		}
		page.requireNoStatusNote(t, status)
	}

	// The person's words leave the quote of the agent's first comment: its anchor is orphaned, and
	// it is in no answer of the tree read (R2, R31).
	anchored, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindComment, first.id, []string{graphstore.TupleAnchoredIn})
	if err != nil || len(anchored) != 1 {
		t.Fatalf("%s %s states the anchored_in %+v (%v) before the person's edit, want the one to the node it quotes: this case proves nothing",
			first.name, first.id, anchored, err)
	}
	editSpec(t, handler, key, []map[string]any{{"op": "replace", "find": first.quote, "with": "then by when it was asked"}})
	if _, orphaned := storedCommentAnchor(t, database, first.id); !orphaned {
		t.Errorf("the edit of the words of r1 leaves the anchor of %s %s standing in the store, want it marked orphaned", first.name, first.id)
	}
	if held, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindComment, first.id, []string{graphstore.TupleAnchoredIn}); err != nil || len(held) != 0 {
		t.Errorf("%s %s, whose anchor is orphaned, states the anchored_in %+v (%v), want none", first.name, first.id, held, err)
	}
	placed, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindComment, first.id, []string{graphstore.TuplePartOf})
	if err != nil {
		t.Fatalf("read the part_of of %s: %v", first.id, err)
	}
	for _, tuple := range placed {
		if tuple.ToKind == anchored[0].ToKind && tuple.ToID == anchored[0].ToID {
			t.Errorf("%s %s, whose anchor is orphaned, still states a part_of to %s:%s, the node its anchor was on", first.name, first.id, tuple.ToKind, tuple.ToID)
		}
	}
	for _, query := range []string{"", "?view=spec", "?kind=requirement&kind=comment"} {
		for _, page := range commentsOnTextPages(t, handler, key, query, label(query, converted)) {
			if len(page.named(graphstore.KindComment, first.id)) > 0 {
				t.Errorf("%s answers %s %s, whose anchor is orphaned and in whose thread no person wrote: no tree answer returns it", page.label, first.name, first.id)
			}
			page.requireNoStatusNote(t, status)
		}
	}

	listed = commentsOnTextListed(t, handler, key)
	for _, id := range append([]string{first.id, status, checked.id, reply.id, decided.id, newest}, notes...) {
		if _, held := listed[id]; !held {
			t.Errorf("the comments read of %s lists no comment %s, which the test wrote", key, id)
		}
	}
	if stored, held := listed[first.id]; held && (stored.Anchor == nil || stored.Anchor.Quote != first.quote || !stored.Anchor.Orphaned) {
		t.Errorf("the comments read of %s lists %s %s with the anchor %+v, want its quote %q and orphaned true", key, first.name, first.id, stored.Anchor, first.quote)
	}
	otherListed := commentsOnTextListed(t, handler, other.Key)
	for _, id := range []string{otherDecided.id, otherFirst.id} {
		if _, held := otherListed[id]; !held {
			t.Errorf("the comments read of %s lists no comment %s, which the test wrote", other.Key, id)
		}
	}
}
