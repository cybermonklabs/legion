package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/schemanodes"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// conversationKinds are the item kinds of an issue's conversation (R2 of the issue-tree spec),
// which only a node's parent may name.
var conversationKinds = []pmdoc.BlockItemKind{pmdoc.BlockItemAsk, pmdoc.BlockItemComment, pmdoc.BlockItemMessage}

// personComment writes a comment of issueKey as alice, a person, through the comment route, and
// returns its id and the reference of its first block.
func personComment(t *testing.T, handler http.Handler, database *store.Store, issueKey, body string) (string, string) {
	t.Helper()
	id := decodeBody[struct {
		ID string `json:"id"`
	}](t, created(t, "a person's comment on "+issueKey, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/comments", map[string]any{"body": body}, "alice"))).ID
	return id, firstItemBlock(t, database, refs.ItemComment, issueKey, id)
}

// agentItem writes a comment or an issue message of issueKey as author, a session, as the write
// routes store one, indexing its blocks and its references as they plan and queue them
// (planItemWrite), and returns its id and the reference of its first block.
func agentItem(t *testing.T, database *store.Store, kind refs.ItemKind, issueKey, author, body string) (string, string) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	table := map[refs.ItemKind]string{refs.ItemComment: "comments", refs.ItemMessage: "messages"}[kind]
	var id string
	var at time.Time
	if err := tx.QueryRow(ctx, `insert into `+table+` (issue_key, author, body) values ($1, $2, $3) returning id::text, created_at`, issueKey, author, body).Scan(&id, &at); err != nil {
		t.Fatalf("insert the %s: %v", kind, err)
	}
	indexItemAsARouteDoes(t, tx, kind, id, body, at)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the %s: %v", kind, err)
	}
	return id, firstItemBlock(t, database, kind, issueKey, id)
}

// indexItemAsARouteDoes indexes item id of kind, whose row tx holds with body written at at, as a
// write route does: the blocks and the references it cites planned (planItemWrite) and written as
// one batch in tx. The writer resolves the body's short ids as the item's own author would, and a
// zero actor resolves them as nobody, which these bodies, holding none, do not depend on.
func indexItemAsARouteDoes(t *testing.T, tx pgx.Tx, kind refs.ItemKind, id, body string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	_, write, err := (&server{}).planItemWrite(ctx, tx, model.Actor{}, kind, id, body, nil, at, true, nil, nil)
	if err != nil {
		t.Fatalf("plan the index of the %s: %v", kind, err)
	}
	if err := store.RunWrite(ctx, tx, write); err != nil {
		t.Fatalf("index the %s: %v", kind, err)
	}
}

// firstItemBlock is the reference of the first current block of item id of kind on issueKey.
func firstItemBlock(t *testing.T, database *store.Store, kind refs.ItemKind, issueKey, id string) string {
	t.Helper()
	table := map[refs.ItemKind]string{refs.ItemComment: "comment_blocks where comment_id", refs.ItemMessage: "message_blocks where message_id"}[kind]
	var block string
	if err := database.Pool.QueryRow(context.Background(), `select block_id from `+table+` = $1::uuid and position is not null order by position limit 1`, id).Scan(&block); err != nil {
		t.Fatalf("read the first block of %s %s: %v", kind, id, err)
	}
	written, ok := text.FormatItemBlockRef(text.Ref{Kind: "block", Item: string(kind), IssueKey: issueKey, ID: id, Block: block})
	if !ok {
		t.Fatalf("no item block reference for block %s of %s %s", block, kind, id)
	}
	return written
}

// criterionUnder is a document holding one criterion whose parent is parent.
func criterionUnder(parent string) string {
	return ":::criterion{#k1 parent=\"" + parent + "\"}\nI want the urgent asks first.\n:::\n"
}

// c-conversation-parent, its first test: a conversation node may be a node's parent, through the
// one attribute that names one. text.FormatItemBlockRef and text.CanonicalItemBlockRef read back
// exactly what they write for an ask, a comment and a message and for a block of each, and refuse
// a project document's item, a uuid prefix, a non-canonical uuid and a revision pin.
// pmdoc.ParseBlockPointerValue and the tree's validation (validateBlockPointerValues) take those
// references where a parent is written, and refuse them under an attribute that points at blocks
// of documents: serves points at a block of a document alone, and needs at a plan task or an issue
// and never at a conversation. A write of a criterion under a person's comment of the issue above
// is taken, and pmdoc.CheckBlockPointers, through the resolver the document service calls, refuses
// a conversation of an issue neither the writing issue nor one above it, and one no row holds,
// each 400 INVALID_MARKDOWN naming the attribute.
func TestAConversationNodeMayBeABlockParent(t *testing.T) {
	const id = "4f1c2d3e-5a6b-4c7d-8e9f-0a1b2c3d4e5f"
	for _, kind := range []string{"ask", "comment", "message"} {
		for _, ref := range []text.Ref{
			{Kind: kind, IssueKey: "CORE-1", ID: id},
			{Kind: "block", Item: kind, IssueKey: "CORE-1", ID: id, Block: "q:1"},
		} {
			written, ok := text.FormatItemBlockRef(ref)
			read, canonical := text.CanonicalItemBlockRef(written)
			if !ok || !canonical || read != ref {
				t.Errorf("%#v writes %q (%t) and reads back %#v (%t), want itself", ref, written, ok, read, canonical)
			}
		}
	}
	for _, refused := range []string{
		"dispatch://CORE/artifact/handbook-md/comment/" + id,
		"dispatch://CORE/artifact/handbook-md/comment/" + id + "#b-b1",
		"dispatch://CORE-1/comment/4f1c2d3e",
		"dispatch://CORE-1/comment/4f1c2d3e#b-b1",
		"dispatch://CORE-1/comment/" + strings.ToUpper(id),
		"dispatch://CORE-1/comment/" + strings.ToUpper(id) + "#b-b1",
		"dispatch://CORE-1/comment/" + id + "#b-b1@v2",
		"dispatch://CORE-1/comment/" + id + "@v2",
	} {
		if ref, ok := text.CanonicalItemBlockRef(refused); ok {
			t.Errorf("CanonicalItemBlockRef(%q) = %#v, want it refused", refused, ref)
		}
		if _, err := pmdoc.ParseBlockPointerValue(refused, conversationKinds); err == nil {
			t.Errorf("a parent %q is taken, want it refused", refused)
		}
	}
	criterion, _ := typedBlockSchema(t, "criterion")
	declared := criterion.Attributes.Get(pmdoc.ParentAttribute)
	if !slices.Equal(declared.Items, conversationKinds) {
		t.Fatalf("criterion's parent declares the items %v, want %v", declared.Items, conversationKinds)
	}
	for _, value := range []string{
		"dispatch://CORE-1/ask/" + id, "dispatch://CORE-1/comment/" + id, "dispatch://CORE-1/message/" + id,
		"dispatch://CORE-1/ask/" + id + "#b-b1", "dispatch://CORE-1/comment/" + id + "#b-b1", "dispatch://CORE-1/message/" + id + "#b-b1",
	} {
		if _, err := pmdoc.ParseBlockPointerValue(value, declared.Items); err != nil {
			t.Errorf("a parent %q is refused: %v", value, err)
		}
		if tree, err := pmdoc.Parse(criterionUnder(value)); err != nil || tree.Validate() != nil {
			t.Errorf("a criterion under %q = %v, want it taken", value, err)
		}
		// Under an attribute that points at blocks of documents the same reference is refused,
		// read from markdown and held in a tree alike.
		for _, attribute := range []struct{ typ, name string }{{"check", "serves"}, {"requirement", "serves"}, {"plan_task", "serves"}, {"plan_task", "needs"}} {
			typ, _ := typedBlockSchema(t, attribute.typ)
			if _, err := pmdoc.ParseBlockPointerValue(value, typ.Attributes.Get(attribute.name).Items); err == nil {
				t.Errorf("%s's %s takes %q, want it refused", attribute.typ, attribute.name, value)
			}
			tree, err := pmdoc.Parse(":::" + attribute.typ + "{#x1}\nThe words.\n:::\n")
			if err != nil {
				t.Fatalf("parse a %s: %v", attribute.typ, err)
			}
			tree.Children[0].Attrs[attribute.name] = []any{value}
			if err := tree.Validate(); err == nil {
				t.Errorf("a %s tree whose %s holds %q validates, want it refused", attribute.typ, attribute.name, value)
			}
		}
	}

	handler, database := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	root := createSpecIssue(t, handler, "CORE", "The root's spec.\n")
	child := createChildIssue(t, handler, "CORE", root.Key, "The child's spec.\n")
	other := createSpecIssue(t, handler, "CORE", "Another tree's spec.\n")
	rootComment, rootBlock := personComment(t, handler, database, root.Key, "I want the urgent asks first.\n\nThen the rest.")
	otherComment, otherBlock := personComment(t, handler, database, other.Key, "Somebody else's words.")

	for _, parent := range []string{"dispatch://" + root.Key + "/comment/" + rootComment, rootBlock} {
		if response := uploadChecks(t, handler, child.Key, criterionUnder(parent)); response.Code != http.StatusCreated {
			t.Fatalf("a criterion of %s under %s, a person's comment of the issue above: status=%d body=%s, want 201", child.Key, parent, response.Code, response.Body.String())
		}
	}
	for _, parent := range []string{"dispatch://" + other.Key + "/comment/" + otherComment, otherBlock} {
		expectRefusal(t, "a criterion under a comment of another tree", uploadChecks(t, handler, child.Key, criterionUnder(parent)),
			http.StatusBadRequest, "INVALID_MARKDOWN", `"parent"`, parent, other.Key, child.Key+" or of an issue above it")
	}
	missing := "dispatch://" + root.Key + "/comment/77777777-7777-4777-8777-777777777777"
	expectRefusal(t, "a criterion under a comment no row holds", uploadChecks(t, handler, child.Key, criterionUnder(missing)),
		http.StatusBadRequest, "INVALID_MARKDOWN", `"parent"`, missing, "which is no comment of "+root.Key)
	expectRefusal(t, "a criterion under a block no comment holds", uploadChecks(t, handler, child.Key, criterionUnder(missing+"#b-b1")),
		http.StatusBadRequest, "INVALID_MARKDOWN", `"parent"`, missing+"#b-b1", "which no comment of "+root.Key+" holds")
	// A comment of the root named under the child's key is no comment of the child.
	moved := "dispatch://" + child.Key + "/comment/" + rootComment
	expectRefusal(t, "a criterion under a comment its reference places in the wrong issue", uploadChecks(t, handler, child.Key, criterionUnder(moved)),
		http.StatusBadRequest, "INVALID_MARKDOWN", `"parent"`, moved, "which is no comment of "+child.Key)
}

// A parent the document service refuses because R2's test does not pass: an agent's status
// comment and a message of the Legion daemon's session, each itself and as its block, where no
// walk of the issue's tree would reach the node written under it; an agent's reply in a
// person's thread is taken.
func TestAParentIsRefusedAtANodeThatIsNoConversation(t *testing.T) {
	handler, database := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "The spec.\n")
	status, statusBlock := agentItem(t, database, refs.ItemComment, issue.Key, `{"kind":"session","id":"architect-1"}`, "Round 2 is done.")
	lifecycle, lifecycleBlock := agentItem(t, database, refs.ItemMessage, issue.Key, `{"kind":"session","id":"legion-daemon:CORE"}`, "READY: claimed.")
	for _, parent := range []string{"dispatch://" + issue.Key + "/comment/" + status, statusBlock, "dispatch://" + issue.Key + "/message/" + lifecycle, lifecycleBlock} {
		expectRefusal(t, "a criterion under a node of no conversation", uploadChecks(t, handler, issue.Key, criterionUnder(parent)),
			http.StatusBadRequest, "INVALID_MARKDOWN", `"parent"`, parent, "no conversation node")
	}

	said, _ := personComment(t, handler, database, issue.Key, "I want it sized.")
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	var reply string
	var at time.Time
	if err := tx.QueryRow(ctx, `insert into comments (issue_key, author, body, reply_to) values ($1, '{"kind":"session","id":"architect-1"}', 'Sized.', $2::uuid) returning id::text, created_at`, issue.Key, said).Scan(&reply, &at); err != nil {
		t.Fatalf("insert the agent's reply: %v", err)
	}
	indexItemAsARouteDoes(t, tx, refs.ItemComment, reply, "Sized.", at)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the agent's reply: %v", err)
	}
	if response := uploadChecks(t, handler, issue.Key, criterionUnder("dispatch://"+issue.Key+"/comment/"+reply)); response.Code != http.StatusCreated {
		t.Fatalf("a criterion under an agent's reply in a person's thread: status=%d body=%s, want 201", response.Code, response.Body.String())
	}
}

// typedBlockSchema is the installed schema's typed block name.
func typedBlockSchema(t *testing.T, name string) (pmdoc.BlockTypeSchema, pmdoc.BlockSchema) {
	t.Helper()
	schema := installedBlockSchema(t)
	index := slices.IndexFunc(schema.Types, func(typ pmdoc.BlockTypeSchema) bool { return typ.Name == name })
	if index < 0 {
		t.Fatalf("the installed schema declares no %s", name)
	}
	return schema.Types[index], schema
}

// installedBlockSchema is the block schema the server installs, as it serves it.
func installedBlockSchema(t *testing.T) pmdoc.BlockSchema {
	t.Helper()
	var schema pmdoc.BlockSchema
	if err := json.Unmarshal(pmdoc.SchemaJSON(), &schema); err != nil {
		t.Fatalf("decode the installed schema: %v", err)
	}
	return schema
}

// conversationAttributes is every attribute of schema that declares one of the conversation's
// kinds among its items, as `<type>.<attribute>`, and every attribute called from, in the schema's
// order: the search the no-second-tie rule reads the schema by.
func conversationAttributes(schema pmdoc.BlockSchema) (declaring, from []string) {
	for _, typ := range schema.Types {
		for _, attribute := range typ.Attributes {
			if slices.ContainsFunc(attribute.Items, func(item pmdoc.BlockItemKind) bool { return slices.Contains(conversationKinds, item) }) {
				declaring = append(declaring, typ.Name+"."+attribute.Name)
			}
			if attribute.Name == "from" {
				from = append(from, typ.Name+"."+attribute.Name)
			}
		}
	}
	return declaring, from
}

// c-conversation-parent, its second test (R2 of the issue-tree spec: "The tie is the parent
// tuple and nothing else"). The schema half: the whole schema the store's type nodes build holds
// no attribute but parent that declares the conversation's kinds, and none called from. A planted
// second attribute declaring them, and a planted from, in a copy of that schema, are found by the
// same search that finds none in the real one. The walk-up half (T7 of the issue-tree
// plan): from a block whose parent is a comment's block, the walk up the chain the block index
// climbs (refs.PartOfChains) reaches the comment and the words in it; with the block's stored
// parent value cleared in the store and its part_of left, the same walk reaches the same comment,
// so that production read climbs the tuple and no attribute.
func TestNoAttributeTiesABlockToItsConversation(t *testing.T) {
	handler, database := blockIndexHandler(t)
	schema, err := schemanodes.Read(context.Background(), database.Pool)
	if err != nil {
		t.Fatalf("build the schema from the store's type nodes: %v", err)
	}
	declaring, from := conversationAttributes(schema)
	if want := []string{"requirement.parent", "plan_task.parent", "check.parent", "criterion.parent"}; !slices.Equal(declaring, want) {
		t.Errorf("the attributes declaring the conversation's kinds are %v, want %v alone", declaring, want)
	}
	if len(from) > 0 {
		t.Errorf("the schema declares %v, want no attribute called from", from)
	}

	planted := schema
	planted.Types = slices.Clone(schema.Types)
	index := slices.IndexFunc(planted.Types, func(typ pmdoc.BlockTypeSchema) bool { return typ.Name == "check" })
	planted.Types[index].Attributes = append(slices.Clone(planted.Types[index].Attributes),
		pmdoc.BlockAttribute{Name: "from", BlockAttributeSchema: pmdoc.BlockAttributeSchema{Kind: pmdoc.BlockAttributeBlockList, Items: []pmdoc.BlockItemKind{pmdoc.BlockItemComment}}})
	declaring, from = conversationAttributes(planted)
	if !slices.Contains(declaring, "check.from") || !slices.Equal(from, []string{"check.from"}) {
		t.Errorf("a planted check.from declaring comments is found as %v and %v, want the search to find it both ways", declaring, from)
	}

	ctx := context.Background()
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "The spec.\n")
	const words = "I want the urgent asks first."
	comment, block := personComment(t, handler, database, issue.Key, words+"\n\nThen the rest.")
	created(t, "a criterion under the comment's first block", uploadChecks(t, handler, issue.Key, criterionUnder(block)))
	// The criterion's node is keyed by checks.md's room: checks.md plays no role, so it is a room
	// of its own (documentJoinsIssueRoom).
	criterion := readDecisionDocument(t, database, issue.Key, "checks-md").address("k1")
	walk := func(when string) {
		t.Helper()
		parent := walkUp(t, database, criterion)
		if parent.Parent == nil || parent.Parent.Kind != "comment" || parent.Parent.ID != comment {
			t.Fatalf("%s, the walk up from the criterion reaches %+v, whose item is %+v, want a block of comment %s", when, parent, parent.Parent, comment)
		}
		said, err := refs.CurrentItemBlockText(ctx, database.Pool, refs.ItemComment, parent.ID)
		if err != nil {
			t.Fatalf("read the words of %s: %v", parent.ID, err)
		}
		if strings.TrimSpace(said) != words {
			t.Errorf("%s, the walk up reaches the words %q, want %q", when, said, words)
		}
		if item, found, err := refs.Node(ctx, database.Pool, model.Actor{Kind: "user", ID: "alice"}, parent.Parent.Kind, parent.Parent.ID, false); err != nil || !found || item.ID != comment {
			t.Errorf("%s, the walk up reaches no comment %s past its block: %+v, found %t, %v", when, comment, item, found, err)
		}
	}
	walk("with the parent attribute stored")

	// The block's stored parent value goes, by a retraction appended straight into the store and
	// its block_refs row deleted beside it; its part_of tuple is left as it is.
	held := currentTies(t, database, criterion, pmdoc.ParentAttribute)
	if len(held) != 1 {
		t.Fatalf("the criterion states the parent values %+v, want the one its attribute holds", held)
	}
	if _, err := database.Pool.Exec(ctx, `
		with retracted as (
			insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
			values ($1, $2, $3, $4, $5, clock_timestamp(), '{}', 0, false)
		)
		delete from block_refs where from_node = $2 and kind = $3
	`, refs.BlockNodeKind, criterion, pmdoc.ParentAttribute, held[0].ToKind, held[0].ToID); err != nil {
		t.Fatalf("clear the criterion's stored parent value: %v", err)
	}
	if cleared := currentTies(t, database, criterion, pmdoc.ParentAttribute); len(cleared) != 0 {
		t.Fatalf("the criterion still states the parent values %+v after the retraction", cleared)
	}
	walk("with the parent attribute cleared from the store")
}

// walkUp is the node the block node address hangs under, the first far end of the chain the block
// index climbs from it to keep a part_of cycle out (refs.PartOfChains), a production read that
// picks the relation it follows itself, and read as the read registry loads that node: a walk up
// the tree that reads no attribute of any node.
func walkUp(t *testing.T, database *store.Store, address string) model.GraphNode {
	t.Helper()
	ctx := context.Background()
	chains, err := refs.PartOfChains(ctx, database.Pool, []string{address})
	if err != nil {
		t.Fatalf("read the chain up from %s: %v", address, err)
	}
	chain := chains[address]
	if len(chain) == 0 {
		t.Fatalf("the chain up from %s climbs to no node", address)
	}
	node, found, err := refs.Node(ctx, database.Pool, model.Actor{Kind: "user", ID: "alice"}, chain[0].Kind, chain[0].ID, false)
	if err != nil || !found {
		t.Fatalf("read %s/%s, which %s hangs under: found %t, %v", chain[0].Kind, chain[0].ID, address, found, err)
	}
	return node
}
