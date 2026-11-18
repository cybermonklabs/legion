package refs

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// backlinkFixture is one graph that fills every arm of graph_edges: each of the view's 27 arms
// emits at least one edge over it, and every node kind nodeRegistry resolves has a node with an
// inbound edge. Every item kind holds a block that is no longer current, carrying a link of its
// own and pointed at by a mention, an ask's part and a message's block by a reply as well, so
// each place the view filters a removed block out is a place a count that kept it answers high.
// The three tests below read it, so a count, its plan and the view's own shape are all held to
// the same rows.
type backlinkFixture struct {
	database *store.Store
	// manyArms is the issue several arms of the view point at - a child issue, a blocking
	// issue, an attached document and a comment's mention - so that a count that lost an arm
	// differs from the view's rather than agreeing with it by having nothing to lose.
	manyArms string
	// heldOnce is the issue a comment cites from one current block and, redundantly, from its
	// own body: the block's edge is counted and the comment's own is not (heldByABlock), so the
	// issue has exactly one inbound edge from that comment.
	heldOnce string
	// heldTwice is the issue the same comment cites from two current blocks and from its own
	// body: each block's edge is counted, so the issue has two.
	heldTwice string
	// removedBlock is the issue the same comment cites only from a block a later revision
	// removed, and from its own body. The view's comment_block_refs arm drops the removed block
	// (position is not null) while heldByABlock's probe carries no position filter and still
	// suppresses the comment's own edge, so the link is in no count at all: it is the one row
	// that tells a count copying that asymmetry from one correcting it.
	removedBlock string
	// askPart is a current part of an ask and commentBlock a current block of a comment. A
	// reply names either in one column (comments.replies_to_block) and only the node id's
	// prefix tells the two kinds apart, so each is asked for under the other's kind: a count
	// of comment_block backlinks over a part, or of ask_block backlinks over a block, answers
	// one where the view answers none the moment an arm loses that prefix test.
	askPart      string
	commentBlock string
}

// newBacklinkFixture writes the fixture into a fresh database. The rows are written as SQL
// because the arms they fill are the view's reading of the schema, not of any writer this
// package has: a row written through a service would say what that service writes today, and the
// count is held to the view.
func newBacklinkFixture(t *testing.T) backlinkFixture {
	t.Helper()
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin fixture transaction: %v", err)
	}
	defer tx.Rollback(ctx)

	createReferenceProject(t, tx, "CORE")
	for number, issue := range []struct {
		key    string
		parent *string
	}{
		{key: "CORE-1"},
		{key: "CORE-2", parent: new("CORE-1")},
		{key: "CORE-3"},
		{key: "CORE-4"},
	} {
		execFixture(t, tx, "create "+issue.key, `
			insert into issues (key, project_key, number, title, parent_key, created_by, rank)
			values ($1, 'CORE', $2, 'Backlink fixture', $3, '{"kind":"user","id":"alice"}', 'U')
		`, issue.key, number+1, issue.parent)
	}
	// issues.parent_key: child_of. issue_links: blocked_by.
	execFixture(t, tx, "block CORE-1", `
		insert into issue_links (issue_key, kind, target_key) values ('CORE-3', 'blocked_by', 'CORE-1')
	`)

	// artifacts.issue_key: attached_to.
	specID, specRef := createReferenceArtifact(t, tx, "CORE-1", "CORE", "spec")
	_, notesRef := createReferenceArtifact(t, tx, "", "CORE", "notes")
	var notesID string
	if err := tx.QueryRow(ctx, `select id::text from artifacts where ref_key = $1`, notesRef).Scan(&notesID); err != nil {
		t.Fatalf("read the notes document's id: %v", err)
	}

	// asks and comments, anchored on the notes document and owned by it: anchored_to, owned_by.
	anchoredAsk := scanFixture(t, tx, "create the anchored ask", `
		insert into asks (issue_key, author, question, anchor)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'anchored?', jsonb_build_object('artifact_id', $1::text))
		returning id::text
	`, notesID)
	execFixture(t, tx, "create the owned ask", `
		insert into asks (artifact_id, author, question)
		values ($1::uuid, '{"kind":"user","id":"alice"}', 'owned?')
	`, notesID)
	execFixture(t, tx, "create the anchored comment", `
		insert into comments (issue_key, author, body, anchor)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'anchored', jsonb_build_object('artifact_id', $1::text))
	`, notesID)
	ownedComment := scanFixture(t, tx, "create the owned comment", `
		insert into comments (artifact_id, author, body)
		values ($1::uuid, '{"kind":"user","id":"alice"}', 'owned')
		returning id::text
	`, notesID)

	// comments.reply_to, comments.ask_id, messages.in_reply_to: replies_to.
	citingComment := scanFixture(t, tx, "create the reply comment", `
		insert into comments (issue_key, author, body, reply_to)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'a reply that cites', $1::uuid)
		returning id::text
	`, ownedComment)
	execFixture(t, tx, "create the nested reply", `
		insert into comments (issue_key, author, body, reply_to, answers)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'a nested reply', $1::uuid, $2::uuid)
	`, ownedComment, citingComment)
	execFixture(t, tx, "create the ask reply", `
		insert into comments (issue_key, author, body, ask_id)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'an answer', $1::uuid)
	`, anchoredAsk)
	rootMessage := scanFixture(t, tx, "create the root message", `
		insert into messages (issue_key, author, body)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'root')
		returning id::text
	`)
	execFixture(t, tx, "create the message reply", `
		insert into messages (issue_key, author, body, in_reply_to)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'reply', $1::uuid)
	`, rootMessage)

	// ask_followers: followed_by.
	execFixture(t, tx, "follow the anchored ask", `
		insert into ask_followers (ask_id, session_id) values ($1::uuid, 'session-backlink')
	`, anchoredAsk)

	// The same three relations as the store holds them for an ask whose row names its node
	// (dispatch://INFRA-1150): an ask on the notes document, anchored on it and owned by it, and a
	// session that follows it, each stated on the ask's node, the rows marked. graph_edges' old
	// arms leave these rows to the tuple arm, and the count reads the store's tuples for them.
	// The answered ask's tie to a block, its shaped, is the store's alone: a block_refs row of the
	// spec's block marked with the ask's node, as the writer of an ask block's links marks it.
	storedAsk := scanFixture(t, tx, "create the ask the store holds", `
		insert into asks (artifact_id, author, question, anchor)
		values ($1::uuid, '{"kind":"user","id":"alice"}', 'held by the store?', jsonb_build_object('artifact_id', $1::text))
		returning id::text
	`, notesID)
	alice := model.Actor{Kind: "user", ID: "alice"}
	stated := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	nodes, tuples, _ := graphstore.AskFacts(storedAsk, "CORE", nil, alice, stated, graphstore.NoEvent,
		graphstore.AskPlaces{Anchor: notesRef, Owner: notesRef})
	nodes = append(nodes, graphstore.SessionNode("session-stored", alice, stated, graphstore.NoEvent))
	tuples = append(tuples,
		graphstore.Tuple{FromKind: graphstore.KindAsk, FromID: storedAsk, Kind: graphstore.TupleFollowedBy,
			ToKind: graphstore.KindSession, ToID: "session-stored", At: stated, Who: alice, Asserted: true},
		graphstore.Tuple{FromKind: graphstore.KindAsk, FromID: storedAsk, Kind: "shaped",
			ToKind: BlockNodeKind, ToID: notesRef + "#b1", At: stated, Who: alice, Asserted: true})
	if err := graphstore.Write(ctx, tx, nodes, tuples); err != nil {
		t.Fatalf("state the ask the store holds: %v", err)
	}
	execFixture(t, tx, "mark the ask the store holds", `
		update asks set node_kind = 'ask', node_address = id::text where id = $1::uuid
	`, storedAsk)
	execFixture(t, tx, "follow the ask the store holds", `
		insert into ask_followers (ask_id, session_id, node_kind, node_address) values ($1::uuid, 'session-stored', 'ask', $1)
	`, storedAsk)

	// components: part_of, depends_on; issue_component_members: affects.
	sourceID := scanFixture(t, tx, "create the architecture source", `
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
		values ('CORE', 'sjawhar/core', 'main', 1, '{"kind":"user","id":"alice"}')
		returning id::text
	`)
	snapshotID := scanFixture(t, tx, "create the architecture snapshot", `
		insert into architecture_snapshots (project_key, commit, files) values ('CORE', 'abc123', '{}'::jsonb)
		returning id::text
	`)
	for _, component := range []struct{ id, parent string }{{id: "core"}, {id: "api", parent: "core"}} {
		execFixture(t, tx, "create component "+component.id, `
			insert into components (project_key, id, title, prose, parent, snapshot_id, source_id)
			values ('CORE', $1, $1, '', nullif($2, ''), $3::bigint, $4::bigint)
		`, component.id, component.parent, snapshotID, sourceID)
	}
	execFixture(t, tx, "make api depend on core", `
		insert into component_depends (project_key, from_id, to_id) values ('CORE', 'api', 'core')
	`)
	execFixture(t, tx, "give CORE-1 components", `
		insert into issue_components (issue_key, mode) values ('CORE-1', 'explicit')
	`)
	execFixture(t, tx, "make CORE-1 affect core", `
		insert into issue_component_members (issue_key, project_key, component_id) values ('CORE-1', 'CORE', 'core')
	`)

	// document_blocks: a document block's part_of. block_refs: a document block's mention.
	specBlock := specRef + "#b1"
	notesBlock := notesRef + "#b1"
	for _, block := range []struct{ artifact, node string }{{artifact: specID, node: specBlock}, {artifact: notesID, node: notesBlock}} {
		execFixture(t, tx, "index a block of "+block.node, `
			insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
			values ($1::uuid, 'b1', $2, 'paragraph', 'sha', 'excerpt', 1, now())
		`, block.artifact, block.node)
	}
	execFixture(t, tx, "cite the notes block from the spec block", `
		insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, created_version)
		values ($1::uuid, 'b1', $2, 'block', $3, 'mentions', 1)
	`, specID, specBlock, notesBlock)
	execFixture(t, tx, "tie the ask the store holds to the notes block", `
		insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, created_version, node_kind, node_address)
		values ($1::uuid, 'b1', $2, 'block', $3, 'shaped', 1, 'ask', $4)
	`, specID, specBlock, notesBlock, storedAsk)

	// comment_blocks: a comment block's part_of; comment_block_refs: its mentions. b3 is the
	// block a later revision removed, which keeps its row with no position and its reference.
	blockComment := scanFixture(t, tx, "create the comment that holds blocks", `
		insert into comments (issue_key, author, body)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'three paragraphs')
		returning id::text
	`)
	for _, block := range []struct {
		id       string
		position *int
	}{{id: "b1", position: new(0)}, {id: "b2", position: new(1)}, {id: "b3"}} {
		execFixture(t, tx, "index comment block "+block.id, `
			insert into comment_blocks (comment_id, block_id, node_id, position, type, text_sha256, excerpt,
			                            made_at, changed_revision, changed_at, removed_at)
			values ($1::uuid, $2, 'comment/' || $1 || '#' || $2, $3, 'paragraph', 'sha', 'excerpt',
			        now(), 1, now(), case when $3::int is null then now() end)
		`, blockComment, block.id, block.position)
	}
	for _, reference := range []struct{ block, issue string }{
		{block: "b1", issue: "CORE-2"},
		{block: "b1", issue: "CORE-3"},
		{block: "b2", issue: "CORE-3"},
		{block: "b3", issue: "CORE-4"},
	} {
		execFixture(t, tx, "cite "+reference.issue+" from comment block "+reference.block, `
			insert into comment_block_refs (comment_id, block_id, from_node, to_kind, to_id, kind)
			values ($1::uuid, $2, 'comment/' || $1 || '#' || $2, 'issue', $3, 'mentions')
		`, blockComment, reference.block, reference.issue)
	}
	// The comment's own mention of each issue its blocks cite: the view holds both edges and the
	// count keeps the block's alone, except for the removed block, whose edge the view drops
	// while the probe suppressing the comment's own still finds its reference.
	for _, issue := range []string{"CORE-2", "CORE-3", "CORE-4"} {
		insertReference(t, tx, "comment", blockComment, "issue", issue)
	}

	// message_blocks: a message block's part_of; message_block_refs: its mentions. b2 is the
	// block that is no longer current, which keeps its row with no position and its reference,
	// as the comment's b3 does.
	messageBlock := "message/" + rootMessage + "#b1"
	removedMessageBlock := "message/" + rootMessage + "#b2"
	for _, block := range []struct {
		id       string
		position *int
	}{{id: "b1", position: new(0)}, {id: "b2"}} {
		execFixture(t, tx, "index message block "+block.id, `
			insert into message_blocks (message_id, block_id, node_id, position, type, text_sha256, excerpt,
			                            made_at, changed_at, removed_at)
			values ($1::uuid, $2, 'message/' || $1 || '#' || $2, $3, 'paragraph', 'sha', 'excerpt',
			        now(), now(), case when $3::int is null then now() end)
		`, rootMessage, block.id, block.position)
	}
	// Both blocks cite the notes document, the removed one included: the view drops the removed
	// block's citation, so a count that kept it answers one too many.
	for _, block := range []string{"b1", "b2"} {
		execFixture(t, tx, "cite the notes document from message block "+block, `
			insert into message_block_refs (message_id, block_id, from_node, to_kind, to_id, kind)
			values ($1::uuid, $2, 'message/' || $1 || '#' || $2, 'artifact', $3, 'mentions')
		`, rootMessage, block, notesRef)
	}

	// comments.replies_to_block and messages.replies_to_block: a reply that names a block. A
	// removed block is answered too, and the edge is the view's: it is counted under the block
	// itself and never under the item the block has left.
	commentBlock := "comment/" + blockComment + "#b1"
	removedCommentBlock := "comment/" + blockComment + "#b3"
	execFixture(t, tx, "reply to a comment block", `
		insert into comments (issue_key, author, body, replies_to_block)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'about that paragraph', $1)
	`, commentBlock)
	for _, block := range []string{messageBlock, removedMessageBlock} {
		execFixture(t, tx, "reply to message block "+block, `
			insert into messages (issue_key, author, body, replies_to_block)
			values ('CORE-1', '{"kind":"user","id":"alice"}', 'about that paragraph', $1)
		`, block)
	}

	// ask_blocks: a part's part_of its ask; ask_block_refs: a link a part holds; a reply naming
	// a part in comments.replies_to_block, which the view tells from a reply to a comment's
	// block by the node id's prefix.
	partedAsk := scanFixture(t, tx, "create the ask that holds parts", `
		insert into asks (issue_key, author, question)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'a question in parts?')
		returning id::text
	`)
	askPart := "ask/" + partedAsk + "#b1"
	removedAskPart := "ask/" + partedAsk + "#b3"
	for _, part := range []struct {
		id       string
		position *int
	}{{id: "b1", position: new(0)}, {id: "b2", position: new(1)}, {id: "b3"}} {
		execFixture(t, tx, "index ask part "+part.id, `
			insert into ask_blocks (ask_id, block_id, node_id, position, type, text_sha256, excerpt,
			                        made_at, changed_revision, changed_at, removed_at)
			values ($1::uuid, $2, 'ask/' || $1 || '#' || $2, $3, 'paragraph', 'sha', 'excerpt',
			        now(), 1, now(), case when $3::int is null then now() end)
		`, partedAsk, part.id, part.position)
	}
	// The removed part cites what the current one cites, so the link is in the view once: a
	// count that read the removed part's citation too would answer twice.
	for _, part := range []string{"b1", "b3"} {
		execFixture(t, tx, "cite CORE-1 from ask part "+part, `
			insert into ask_block_refs (ask_id, block_id, from_node, to_kind, to_id, kind)
			values ($1::uuid, $2, 'ask/' || $1 || '#' || $2, 'issue', 'CORE-1', 'mentions')
		`, partedAsk, part)
	}
	// The ask's own mention of the issue its part cites: the part's edge is counted and the
	// ask's own is not, as a comment's blocks hold the comment's own.
	insertReference(t, tx, "ask", partedAsk, "issue", "CORE-1")
	for _, part := range []string{askPart, removedAskPart} {
		execFixture(t, tx, "reply to ask part "+part, `
			insert into comments (issue_key, author, body, ask_id, replies_to_block)
			values ('CORE-1', '{"kind":"user","id":"alice"}', 'about that part', $1::uuid, $2)
		`, partedAsk, part)
	}

	// refs: the mention arm. The reply comment holds no blocks, so each of its mentions is
	// counted where it is written; the document's and the message's are each held by a block of
	// theirs and counted there instead. It mentions a removed block of each item kind as well
	// as a current one: that edge is the view's, counted under the block, and the item the
	// block has left must not pick it up.
	for _, target := range [][2]string{
		{"issue", "CORE-1"},
		{"artifact", notesRef},
		{"ask", anchoredAsk},
		{"comment", ownedComment},
		{"message", rootMessage},
		{"component", "CORE/core"},
		{"session", "session-backlink"},
		// The three kinds the store holds that no structural arm points at, as the view's
		// mention arm reaches them: a person by their canonical login, a role by its topic,
		// and the page of that role.
		{"person", "person-backlink@example.com"},
		{"role", "role-backlink"},
		{"page", "role/role-backlink"},
		{"block", notesBlock},
		{"comment_block", commentBlock},
		{"comment_block", removedCommentBlock},
		{"message_block", messageBlock},
		{"message_block", removedMessageBlock},
		{"ask_block", askPart},
		{"ask_block", removedAskPart},
		// The two kinds of the lens catalogue, which the seed holds (migration 20261116123354):
		// a kind and the constraint that is its source, each named by a mention as any node is.
		{"lens_kind", "pre-mortem"},
		{"constraint", "5eed0000-1e45-4000-8000-000000000001"},
		// A lens, a finding of it and a risk under it (writeBacklinkReasoning), and a run of the
		// command bar (its node is written below), each named by a mention as any node is.
		{"lens", backlinkLens},
		{"lens_block", backlinkFinding},
		{"risk", backlinkRisk},
		{"interaction", "22222222-2222-4222-8222-222222222222"},
	} {
		insertReference(t, tx, "comment", citingComment, target[0], target[1])
	}
	insertReference(t, tx, "artifact", specID, "block", notesBlock)
	insertReference(t, tx, "message", rootMessage, "artifact", notesRef)
	// A source of a node kind this build cannot address, written straight into refs as a newer
	// build would leave it: the mention arm's from_kind filter is what keeps it out of a count.
	insertReference(t, tx, "future", "x#y", "issue", "CORE-1")
	// graph_tuples, the view's store class: an authored_by out of a lens to the person and out of
	// an interaction to the session, which the view answers from the tuples alone. Both are kinds
	// this build resolves, so each edge counts on its author (authoredByReasoning). The interaction
	// is written project-visible, as no run is, because the oracle reads the view with no
	// predicate: a run reserved for its author would be in the view's count and in no reader's.
	for _, authored := range []struct{ fromKind, fromID, toKind, toID string }{
		{"lens", backlinkLens, "person", "person-backlink@example.com"},
		{"interaction", "22222222-2222-4222-8222-222222222222", "session", "session-backlink"},
	} {
		execFixture(t, tx, "write the "+authored.fromKind+" node", `
			insert into graph_nodes (kind, id, supertag, visibility, state, made_at, made_by, source_event)
			values ($1, $2, $1, 'project', 'live', now(), '{"kind":"user","id":"alice"}', 0)
		`, authored.fromKind, authored.fromID)
		execFixture(t, tx, "write the "+authored.fromKind+"'s authored_by", `
			insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event)
			values ($1, $2, 'authored_by', $3, $4, now(), '{"kind":"user","id":"alice"}', 0)
		`, authored.fromKind, authored.fromID, authored.toKind, authored.toID)
	}
	// A has_kind out of the same lens to a kind of the catalogue, which the view answers from the
	// tuples alone, as it answers the seed's constrained_by from each kind to its source: a lens
	// kind's count holds the lens, and a constraint's count holds the lens kind whose source it is.
	execFixture(t, tx, "write the lens's has_kind", `
		insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event)
		values ('lens', $1, 'has_kind', 'lens_kind', 'pre-mortem', now(), '{"kind":"user","id":"alice"}', 0)
	`, backlinkLens)
	writeBacklinkReasoning(t, tx, specRef, specBlock, commentBlock)
	// The changed_by_run of what a write made for that run changed: a lens, a finding of it and a
	// risk under it, each answered from the tuples alone and counted on the run's node
	// (changedByRun), the finding as the lens_block its own arm writes it as; and one out of the
	// lens the store holds gone, which neither reading counts.
	for _, changed := range []struct{ fromKind, fromID string }{
		{"lens", backlinkLens}, {"block", backlinkFinding}, {"risk", backlinkRisk}, {"lens", backlinkGoneLens},
	} {
		execFixture(t, tx, "write the changed_by_run of "+changed.fromKind+" "+changed.fromID, `
			insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event)
			values ($1, $2, 'changed_by_run', 'interaction', '22222222-2222-4222-8222-222222222222', now(), '{"kind":"user","id":"alice"}', 0)
		`, changed.fromKind, changed.fromID)
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit fixture: %v", err)
	}
	return backlinkFixture{
		database:     database,
		manyArms:     "CORE-1",
		heldOnce:     "CORE-2",
		heldTwice:    "CORE-3",
		removedBlock: "CORE-4",
		askPart:      askPart,
		commentBlock: commentBlock,
	}
}

// The reasoning nodes of the backlink fixture (writeBacklinkReasoning): a pre-mortem lens on the
// issue, its finding and its risk, a lens on the spec it triggered, a lens on that finding, a lens
// on a comment's block, a constraint under the issue, and a lens on the issue the store holds gone.
const (
	backlinkLens         = "11111111-1111-4111-8111-111111111111"
	backlinkDocumentLens = "11111111-1111-4111-8111-111111111112"
	backlinkFindingLens  = "11111111-1111-4111-8111-111111111113"
	backlinkBlockLens    = "11111111-1111-4111-8111-111111111114"
	backlinkGoneLens     = "11111111-1111-4111-8111-111111111115"
	backlinkFinding      = "lens/" + backlinkLens + "#f1"
	backlinkRisk         = "33333333-3333-4333-8333-333333333331"
	backlinkConstraint   = "44444444-4444-4444-8444-444444444441"
)

// writeBacklinkReasoning writes what stands on the fixture's nodes and under its lens, as rows of
// the store, which the view's tuple arms answer from the tuples alone: the lens backlinkLens
// (written already, with its authored_by and its has_kind) on CORE-1; its finding, with its own
// authored_by, and its risk, which a check of the spec - its first block, here - prevents; a lens
// on the spec that the first lens triggered; a lens on the finding; a lens on a current block of a
// comment, which counts on the block and on the comment that holds it; and a constraint under
// CORE-1. Beside them are two rows no reading may count: a lens on CORE-1 the store holds gone, and
// a part_of of the constraint to CORE-2 that a later tuple retracts.
func writeBacklinkReasoning(t *testing.T, tx pgx.Tx, spec, specBlock, commentBlock string) {
	t.Helper()
	for _, node := range []struct{ kind, id, supertag, state string }{
		{"lens", backlinkDocumentLens, "lens", "live"},
		{"lens", backlinkFindingLens, "lens", "live"},
		{"lens", backlinkBlockLens, "lens", "live"},
		{"lens", backlinkGoneLens, "lens", "gone"},
		{"block", backlinkFinding, "block", "live"},
		{"risk", backlinkRisk, "risk", "live"},
		{"constraint", backlinkConstraint, "constraint", "live"},
	} {
		execFixture(t, tx, "write the "+node.kind+" "+node.id, `
			insert into graph_nodes (kind, id, supertag, visibility, state, made_at, made_by, source_event)
			values ($1, $2, $3, 'project', $4, now(), '{"kind":"user","id":"alice"}', 0)
		`, node.kind, node.id, node.supertag, node.state)
	}
	for _, tuple := range []struct {
		fromKind, fromID, kind, toKind, toID string
		asserted                             bool
	}{
		{"lens", backlinkLens, "part_of", "issue", "CORE-1", true},
		{"block", backlinkFinding, "part_of", "lens", backlinkLens, true},
		{"block", backlinkFinding, "authored_by", "person", "person-backlink@example.com", true},
		{"risk", backlinkRisk, "part_of", "lens", backlinkLens, true},
		{"risk", backlinkRisk, "prevented_by", "block", specBlock, true},
		{"lens", backlinkDocumentLens, "part_of", "artifact", spec, true},
		{"lens", backlinkDocumentLens, "triggered_by", "lens", backlinkLens, true},
		{"lens", backlinkFindingLens, "part_of", "lens_block", backlinkFinding, true},
		{"lens", backlinkBlockLens, "part_of", "comment_block", commentBlock, true},
		{"constraint", backlinkConstraint, "part_of", "issue", "CORE-1", true},
		{"lens", backlinkGoneLens, "part_of", "issue", "CORE-1", true},
		{"constraint", backlinkConstraint, "part_of", "issue", "CORE-2", true},
		{"constraint", backlinkConstraint, "part_of", "issue", "CORE-2", false},
	} {
		execFixture(t, tx, "write "+tuple.fromKind+" "+tuple.fromID+" -"+tuple.kind+"-> "+tuple.toKind+" "+tuple.toID, `
			insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
			values ($1, $2, $3, $4, $5, now(), '{"kind":"user","id":"alice"}', 0, $6)
		`, tuple.fromKind, tuple.fromID, tuple.kind, tuple.toKind, tuple.toID, tuple.asserted)
	}
}

func execFixture(t *testing.T, tx pgx.Tx, what, statement string, args ...any) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), statement, args...); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func scanFixture(t *testing.T, tx pgx.Tx, what, statement string, args ...any) string {
	t.Helper()
	var id string
	if err := tx.QueryRow(context.Background(), statement, args...).Scan(&id); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	return id
}

// viewBacklinkTargets is every node of toKind the view points at, read from the view itself so
// that each test covers whatever arms the fixture filled without naming an id, plus one address
// nothing points at, which must be absent from every answer.
func viewBacklinkTargets(t *testing.T, q Queryer, toKind string) []string {
	t.Helper()
	rows, err := q.Query(context.Background(), `select distinct to_id from graph_edges where to_kind = $1 order by to_id`, toKind)
	if err != nil {
		t.Fatalf("read the %s nodes the view points at: %v", toKind, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan a %s node the view points at: %v", toKind, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the %s nodes the view points at: %v", toKind, err)
	}
	if len(ids) == 0 {
		t.Fatalf("no arm of the view points at a %s node: the fixture leaves that kind's arms empty", toKind)
	}
	return append(ids, "nothing-points-at-this")
}

// backlinkExclusionCases is every exclusion a node kind is counted under: none; mentions, which
// every kind's arms carry; each edge kind this kind's own structural arms emit, so that at least
// one case drops an arm the kind really has; and, for an ask, the exclusion every ask surface
// passes.
func backlinkExclusionCases(toKind string) [][]string {
	cases := [][]string{nil, {"mentions"}}
	for _, arm := range backlinkArms[toKind] {
		if !slices.ContainsFunc(cases, func(kinds []string) bool { return slices.Equal(kinds, []string{arm.kind}) }) {
			cases = append(cases, []string{arm.kind})
		}
	}
	if toKind == "ask" {
		cases = append(cases, AskBacklinkExclusions)
	}
	return cases
}

// A count read arm by arm is the view's count. For every node kind this build resolves, under no
// exclusion and under each exclusion the kind's arms carry, BacklinkCounts answers exactly what
// the whole view answers over the same addresses (docstest.ViewBacklinkCountSQL over graph_edges).
//
// The fixture's rows make the comparison able to fail rather than merely agree: one node is
// pointed at through several arms, so a dropped arm shows as a lower count; the issue cited
// only from a block a revision removed is counted zero by both, so the asymmetry between the
// view's position filter and the probe that suppresses the citing comment's own edge is pinned,
// not reproduced by accident; and every edge at a removed block of an item is an edge the item
// must not count, so each position filter the count carries is one the view disagrees with the
// moment it goes.
func TestBacklinkCountsReadArmByArmAgreeWithTheView(t *testing.T) {
	fixture := newBacklinkFixture(t)
	ctx := context.Background()
	pool := fixture.database.Pool

	var arms int
	if err := pool.QueryRow(ctx, `
		select count(*) from (
			select distinct g.from_kind, g.kind from graph_edges g where g.to_kind = 'issue' and g.to_id = $1
		) pair
	`, fixture.manyArms).Scan(&arms); err != nil {
		t.Fatalf("count the arms pointing at %s: %v", fixture.manyArms, err)
	}
	// Each distinct (from_kind, kind) pair of an inbound edge is a separate arm of the view, so
	// more than one pair is more than one arm.
	if arms < 3 {
		t.Fatalf("%s is pointed at through %d (from_kind, kind) pairs; want at least 3, so a dropped arm changes its count", fixture.manyArms, arms)
	}

	for _, toKind := range nodeKinds {
		ids := viewBacklinkTargets(t, pool, toKind)
		switch toKind {
		case "issue":
			// The removed block's issue is in no arm of the view, so the view's own list does
			// not name it; it is asked for by name.
			ids = append(ids, fixture.removedBlock)
		case "comment_block":
			// A reply names a comment's block and an ask's part in one column, so each kind is
			// asked for over the other's address as well as its own: that is where an arm
			// which lost the prefix test telling the two apart answers one and the view none.
			ids = append(ids, fixture.askPart)
		case "ask_block":
			ids = append(ids, fixture.commentBlock)
		}
		for _, exclusions := range backlinkExclusionCases(toKind) {
			counts, err := BacklinkCounts(ctx, pool, model.Actor{}, toKind, ids, exclusions)
			if err != nil {
				t.Fatalf("count %s backlinks excluding %v: %v", toKind, exclusions, err)
			}
			want := docstest.ViewBacklinkCounts(t, pool, "graph_edges", toKind, ids, nodeKinds, exclusions)
			if !maps.Equal(counts, want) {
				t.Errorf("%s backlink counts excluding %v = %v; the view says %v", toKind, exclusions, counts, want)
			}
			if _, ok := counts[fixture.removedBlock]; ok {
				t.Errorf("%s backlink counts excluding %v count %s, whose only citation is held by a removed block: %v",
					toKind, exclusions, fixture.removedBlock, counts)
			}
		}
	}

	counts, err := BacklinkCounts(ctx, pool, model.Actor{}, "issue", []string{fixture.heldOnce, fixture.heldTwice, fixture.removedBlock}, nil)
	if err != nil {
		t.Fatalf("count the block-held issues' backlinks: %v", err)
	}
	want := map[string]int{fixture.heldOnce: 1, fixture.heldTwice: 2}
	if !maps.Equal(counts, want) {
		t.Errorf("block-held issue backlink counts = %v; want %v: each link counted once where a current block holds it, and none for the removed block", counts, want)
	}
}

// A node kind with no arms declared is refused rather than counted from its mention arms alone,
// and every kind this build resolves is declared, so no node a list shows is counted short.
func TestBacklinkCountsRefuseANodeKindWithNoDeclaredArms(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	if _, err := BacklinkCounts(ctx, database.Pool, model.Actor{}, "pull_request", []string{"x"}, nil); err == nil {
		t.Fatal("counting the backlinks of a node kind with no declared arms answered without an error")
	}
	for _, kind := range nodeKinds {
		if _, ok := backlinkArms[kind]; !ok {
			t.Errorf("nodeRegistry resolves %q but backlinkArms declares no arms for it, so every count of that kind is refused", kind)
		}
	}
}

// planSettings is what every plan below is read under. Sequential scans off alone is not enough:
// the planner can still hash or merge a whole relation into a join, reading it under a node that
// carries no index condition, which is the unkeyed read this test exists to catch. With the two
// join strategies that can do that off as well, what is left is the nested loop over an index
// each arm is written for.
var planSettings = []string{
	"set local enable_seqscan = off",
	"set local enable_hashjoin = off",
	"set local enable_mergejoin = off",
}

// Every arm of the count's statement reaches its table through an index, and through an index
// that leads with the column the arm is keyed on, under a custom plan (built knowing the bound
// values) and under a generic plan alike. The generic plan is the one at risk: it cannot assume
// what an address parameter holds, so an arm whose index depends on the bound value loses it
// there, and a fixture this small never reaches a generic plan on its own
// (api.TestListIssueAsksOpenQueryUsesAsksOpenIndex says why), so the mode is forced.
func TestBacklinkCountStatementReadsEveryArmThroughAnIndex(t *testing.T) {
	fixture := newBacklinkFixture(t)
	ctx := context.Background()
	connection, err := fixture.database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the plans: %v", err)
	}
	defer connection.Release()

	firstKeys := explaintest.NewIndexKeys(fixture.database.Pool)
	prepared := 0
	for _, toKind := range nodeKinds {
		ids := viewBacklinkTargets(t, fixture.database.Pool, toKind)
		for _, exclusions := range backlinkExclusionCases(toKind) {
			// Both shapes the count binds its addresses in: the array of every address, and
			// the one address a read of a single node's page counts, which is a statement of
			// its own (backlinkCountStatement). An arm that lost its index in one of them
			// would be unkeyed for half the counts Dispatch runs.
			for _, shape := range backlinkAddressShapes(ids) {
				statement, ok := backlinkCountSQL(toKind, exclusions, shape.one)
				if !ok {
					t.Fatalf("no arms declared for %s, the kind nodeRegistry resolves", toKind)
				}
				// The acting credential rides as $3, as backlinkCountStatement binds it: an
				// author who proves none, so every arm's predicate is the shape a reader of
				// the project runs, and the plan is the one every count pays for.
				seen := VisibilityArg(model.Actor{})
				args := []any{shape.addresses, nodeKinds, seen}
				types := []string{shape.sqlType, "text[]", "jsonb"}
				literals := []string{shape.literal, textArrayLiteral(nodeKinds), "null"}
				if len(exclusions) > 0 {
					args = append(args, exclusions)
					types = append(types, "text[]")
					literals = append(literals, textArrayLiteral(exclusions))
				}
				prepared++
				name := fmt.Sprintf("backlink_count_%d", prepared)
				for mode, plan := range map[string]json.RawMessage{
					"custom": customPlan(t, connection, statement, args),
					"generic": explaintest.GenericPlan(t, ctx, connection, name, statement,
						types, literals, planSettings...),
				} {
					if unkeyed := explaintest.ScansWithoutIndexCond(t, plan); len(unkeyed) > 0 {
						t.Errorf("the %s backlink count over %s excluding %v reads %s under its %s plan, which no index serves:\n%s",
							toKind, shape.name, exclusions, strings.Join(unkeyed, ", "), mode, plan)
					}
					// Carrying an index condition is not the same as being served by the index: a
					// scan whose qual names a column the index does not lead with reads the whole
					// index and filters it. Take the index off ask_followers.session_id and the
					// check above still passes, because the planner then reads the table through
					// ask_followers_pkey, (ask_id, session_id), as a full index scan with the
					// session_id qual in its Index Cond. So every scan is held to its index's own
					// first key column, the one column an index narrows on; a scan carrying no
					// condition at all is the check above's to name.
					firstKeys.RequireIndexKeyed(t, plan, explaintest.AnyIndex,
						fmt.Sprintf("the %s backlink count over %s excluding %v under its %s plan", toKind, shape.name, exclusions, mode),
						explaintest.CondOptional)
				}
			}
		}
	}
}

// backlinkAddressShape is one way the count binds the addresses it counts for, as
// backlinkCountStatement binds them: the argument, the type a prepared statement declares for
// it and the literal a generic plan is read under.
type backlinkAddressShape struct {
	name      string
	one       bool
	addresses any
	sqlType   string
	literal   string
}

// backlinkAddressShapes are both of them, over the addresses a fixture holds.
func backlinkAddressShapes(ids []string) []backlinkAddressShape {
	return []backlinkAddressShape{
		{name: "every address", addresses: ids, sqlType: "text[]", literal: textArrayLiteral(ids)},
		{name: "one address", one: true, addresses: ids[0], sqlType: "text",
			literal: `'` + strings.ReplaceAll(ids[0], `'`, `''`) + `'::text`},
	}
}

// customPlan is the plan Postgres builds for statement knowing the values bound to it.
func customPlan(t *testing.T, connection *pgxpool.Conn, statement string, args []any) json.RawMessage {
	t.Helper()
	ctx := context.Background()
	tx, err := connection.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the custom plan's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	for _, setting := range planSettings {
		if _, err := tx.Exec(ctx, setting); err != nil {
			t.Fatalf("%s: %v", setting, err)
		}
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "explain (format json) "+statement, args...).Scan(&raw); err != nil {
		t.Fatalf("explain the backlink count: %v", err)
	}
	return explaintest.OnePlan(t, raw)
}

// textArrayLiteral writes values as a SQL text[] literal, which is how an EXECUTE carries them:
// EXECUTE takes no bind parameters, so each value is written into the statement, with the
// backslashes and double quotes an array literal escapes and the single quotes a string literal
// doubles.
func textArrayLiteral(values []string) string {
	elements := make([]string, 0, len(values))
	for _, value := range values {
		escaped := strings.ReplaceAll(value, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `"`, `\"`)
		elements = append(elements, `"`+escaped+`"`)
	}
	return `'` + strings.ReplaceAll(`{`+strings.Join(elements, ",")+`}`, `'`, `''`) + `'::text[]`
}

// graphViewArm is one arm of graph_edges as the view's own text declares it: the first relation
// its FROM names, and the literal value of each of its from_kind, kind and to_kind select items,
// empty where the arm selects a column instead.
type graphViewArm struct {
	table    string
	fromKind string
	kind     string
	toKind   string
}

func (a graphViewArm) String() string {
	return fmt.Sprintf("from %s: from_kind=%q kind=%q to_kind=%q", a.table, a.fromKind, a.kind, a.toKind)
}

// graphViewArms is every arm of graph_edges, as this package's count knows them. The count unions
// the arms that can point at the kind it is asked for, so an arm the view gains and the count
// does not know is a count silently low: this list is what makes that a failing test rather than
// a wrong number.
var graphViewArms = []graphViewArm{
	{table: "refs"},
	{table: "issues", fromKind: "issue", kind: "child_of", toKind: "issue"},
	{table: "issue_links", fromKind: "issue", kind: "blocked_by", toKind: "issue"},
	{table: "artifacts", fromKind: "artifact", kind: "attached_to", toKind: "issue"},
	// The arm over the artifacts that carry their node, answering each one's attached_to from its
	// part_of tuple (migration 20261117140000).
	{table: "artifacts", fromKind: "artifact", kind: "attached_to", toKind: "issue"},
	{table: "asks", fromKind: "ask", kind: "anchored_to", toKind: "artifact"},
	{table: "comments", fromKind: "comment", kind: "anchored_to", toKind: "artifact"},
	{table: "asks", fromKind: "ask", kind: "owned_by", toKind: "artifact"},
	{table: "comments", fromKind: "comment", kind: "owned_by", toKind: "artifact"},
	{table: "comments", fromKind: "comment", kind: "replies_to", toKind: "comment"},
	{table: "comments", fromKind: "comment", kind: "answers", toKind: "comment"},
	{table: "comments", fromKind: "comment", kind: "replies_to", toKind: "ask"},
	{table: "messages", fromKind: "message", kind: "replies_to", toKind: "message"},
	{table: "ask_followers", fromKind: "ask", kind: "followed_by", toKind: "session"},
	{table: "components", fromKind: "component", kind: "part_of", toKind: "component"},
	{table: "component_depends", fromKind: "component", kind: "depends_on", toKind: "component"},
	{table: "issue_component_members", fromKind: "issue", kind: "affects", toKind: "component"},
	{table: "block_refs", fromKind: "block"},
	// The store's own arm, which answers each kind it carries once that kind's cutover row
	// reads true: every select item of it is a column, so it is a mention arm to this test, and
	// mentionBacklinkArms reads graph_tuples for exactly that reason.
	{table: "graph_tuples"},
	// The store's arm over the relations out of a finding of a lens, which writes its from_kind,
	// lens_block, as a literal and every other item as a column (migration 20261117020856): its
	// edges are counted by the arms of the store alone (storedFrom) that name a lens_block.
	{table: "graph_tuples", fromKind: "lens_block"},
	{table: "document_blocks", fromKind: "block", kind: "part_of", toKind: "artifact"},
	{table: "comment_blocks", fromKind: "comment_block", kind: "part_of", toKind: "comment"},
	{table: "message_blocks", fromKind: "message_block", kind: "part_of", toKind: "message"},
	{table: "ask_blocks", fromKind: "ask_block", kind: "part_of", toKind: "ask"},
	{table: "comment_block_refs", fromKind: "comment_block"},
	{table: "message_block_refs", fromKind: "message_block"},
	{table: "ask_block_refs", fromKind: "ask_block"},
	{table: "comments", fromKind: "comment", kind: "replies_to", toKind: "comment_block"},
	{table: "comments", fromKind: "comment", kind: "replies_to", toKind: "ask_block"},
	{table: "messages", fromKind: "message", kind: "replies_to", toKind: "message_block"},
}

// The view cannot gain an arm the count does not know. graph_edges' own text is parsed back into
// one descriptor per arm and held to graphViewArms, and graphViewArms is held to the count's
// declarations: every literal-kind arm to an entry of backlinkArms, every column-kind arm to one
// of the five tables mentionBacklinkArms reads, and every literal source kind to a kind
// nodeRegistry resolves, which is what makes BacklinkCounts' source filter on the refs arm alone
// exact.
func TestTheGraphViewGainsNoArmTheBacklinkCountDoesNotKnow(t *testing.T) {
	fixture := newBacklinkFixture(t)
	var definition string
	if err := fixture.database.Pool.QueryRow(context.Background(),
		`select pg_get_viewdef('graph_edges'::regclass, true)`).Scan(&definition); err != nil {
		t.Fatalf("read the graph_edges definition: %v", err)
	}

	got := parseGraphViewArms(t, definition)
	gotText, wantText := armTexts(got), armTexts(graphViewArms)
	if !slices.Equal(gotText, wantText) {
		t.Fatalf("graph_edges' arms are not the arms this test knows.\nthe view:\n  %s\nthis test:\n  %s\n"+
			"Declare each new arm in backlinkArms (or, when its to_kind is a column, in mentionBacklinkArms), "+
			"add it to graphViewArms in this test, and fill it in newBacklinkFixture.",
			strings.Join(gotText, "\n  "), strings.Join(wantText, "\n  "))
	}

	// An arm whose to_kind is a literal is a structural arm, and backlinkArms must declare it
	// under that kind with that edge kind. They are compared as multisets because one kind can
	// hold the same edge kind twice: an artifact is anchored_to and owned_by from both asks and
	// comments. An arm of the store alone (storedFrom) counts rows of the view's tuple arm, whose
	// to_kind is a column and which graphViewArms holds as graph_tuples, so it is none of them.
	var declared, viewed []string
	for kind, arms := range backlinkArms {
		for _, arm := range arms {
			if len(arm.storedFrom) > 0 {
				continue
			}
			declared = append(declared, kind+"/"+arm.kind)
		}
	}
	mentionTables := map[string]bool{}
	for _, arm := range mentionBacklinkArms(backlinkTarget{kind: "issue"}, nil) {
		match := regexp.MustCompile(`\bfrom (\w+)`).FindStringSubmatch(arm)
		if match == nil {
			t.Fatalf("a mention arm names no table: %s", arm)
		}
		mentionTables[match[1]] = false
	}
	for _, arm := range got {
		if arm.toKind != "" {
			viewed = append(viewed, arm.toKind+"/"+arm.kind)
		} else if _, ok := mentionTables[arm.table]; !ok {
			t.Errorf("the view's arm over %s selects its to_kind as a column, but mentionBacklinkArms does not read %s", arm.table, arm.table)
		} else {
			mentionTables[arm.table] = true
		}
		if arm.fromKind != "" && !slices.Contains(nodeKinds, arm.fromKind) {
			t.Errorf("the view's arm over %s writes from_kind %q, which nodeRegistry does not resolve: "+
				"BacklinkCounts filters the source kind on the refs arm alone, which is exact only while every other arm's source kind is one this build reads",
				arm.table, arm.fromKind)
		}
	}
	for table, seen := range mentionTables {
		if !seen {
			t.Errorf("mentionBacklinkArms reads %s, but no arm of the view selects its to_kind as a column from it", table)
		}
	}
	slices.Sort(declared)
	slices.Sort(viewed)
	if !slices.Equal(declared, viewed) {
		t.Errorf("backlinkArms declares %v; the view's literal-kind arms are %v", declared, viewed)
	}
}

func armTexts(arms []graphViewArm) []string {
	texts := make([]string, 0, len(arms))
	for _, arm := range arms {
		texts = append(texts, arm.String())
	}
	slices.Sort(texts)
	return texts
}

// armLiteralItem matches a select item that writes a literal, `'<value>'::text AS <name>`, and
// armColumnItem one that selects a column, `<alias>.<name>`. A select item of graph_edges is one
// or the other; anything else fails the parse rather than passing as a shape it did not read.
var (
	armLiteralItem = regexp.MustCompile(`^'([^']*)'::text AS (\w+)$`)
	armColumnItem  = regexp.MustCompile(`^[a-z_][a-z0-9_]*\.(\w+)$`)
)

// parseGraphViewArms reads pg_get_viewdef's pretty-printed graph_edges back into one descriptor
// per arm. That form puts each top-level UNION ALL on a line of its own with no indentation, and
// no arm of this view holds a union of its own, so splitting on such a line splits the arms; each
// arm then opens with ` SELECT <item>,` and one four-space-indented line per further select item,
// and its `   FROM ` line names the relation it reads first. An arm whose FROM opens a sub-select
// is named by the first relation inside it, which is the relation that arm reads first too: the
// store's own arm reads graph_tuples under a `distinct on`, so the list declares it as that.
func parseGraphViewArms(t *testing.T, definition string) []graphViewArm {
	t.Helper()
	body := strings.TrimSuffix(strings.TrimRight(definition, "\n"), ";")
	var arms []graphViewArm
	for index, text := range strings.Split(body, "\nUNION ALL\n") {
		where := fmt.Sprintf("arm %d of graph_edges", index+1)
		lines := strings.Split(text, "\n")
		if !strings.HasPrefix(lines[0], " SELECT ") {
			t.Fatalf("%s does not open with a SELECT: %q", where, lines[0])
		}
		items := []string{strings.TrimPrefix(lines[0], " SELECT ")}
		table := ""
		for _, line := range lines[1:] {
			if rest, found := strings.CutPrefix(line, "   FROM "); found {
				fields := strings.Fields(rest)
				if len(fields) == 0 {
					t.Fatalf("%s names no relation after its FROM: %q", where, line)
				}
				table = fields[0]
				if table == "(" {
					table = subSelectRelation(t, where, lines)
				}
				break
			}
			if !strings.HasPrefix(line, "    ") {
				t.Fatalf("%s holds a line that is neither a select item nor its FROM: %q", where, line)
			}
			items = append(items, strings.TrimPrefix(line, "    "))
		}
		if table == "" {
			t.Fatalf("%s has no FROM line:\n%s", where, text)
		}
		if len(items) != 7 {
			t.Fatalf("%s selects %d items; want the view's 7, one per column:\n%s", where, len(items), text)
		}
		for position, item := range items {
			last := position == len(items)-1
			if strings.HasSuffix(item, ",") == last {
				t.Fatalf("%s: select item %q is not one item on one line", where, item)
			}
			items[position] = strings.TrimSuffix(item, ",")
		}
		arms = append(arms, graphViewArm{
			table:    table,
			fromKind: armLiteral(t, where, items[0], "from_kind"),
			kind:     armLiteral(t, where, items[2], "kind"),
			toKind:   armLiteral(t, where, items[3], "to_kind"),
		})
	}
	return arms
}

// subSelectRelation is the first relation a sub-select arm reads: the first FROM inside it,
// which the catalogue prints indented past the arm's own. An arm whose sub-select names no
// relation at all fails the parse rather than being read as a table named "(".
func subSelectRelation(t *testing.T, where string, lines []string) string {
	t.Helper()
	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		if len(line)-len(trimmed) <= len("   ") {
			continue
		}
		if rest, found := strings.CutPrefix(trimmed, "FROM "); found {
			if fields := strings.Fields(rest); len(fields) > 0 && fields[0] != "(" {
				return fields[0]
			}
		}
	}
	t.Fatalf("%s opens a sub-select that names no relation:\n%s", where, strings.Join(lines, "\n"))
	return ""
}

// armLiteral is the value a select item writes as a literal, or "" when it selects a column. The
// item must name the column it fills, so that a reordered select list fails the parse instead of
// reading one arm's kind as another's.
func armLiteral(t *testing.T, where, item, column string) string {
	t.Helper()
	if match := armLiteralItem.FindStringSubmatch(item); match != nil {
		if match[2] != column {
			t.Fatalf("%s: select item %q fills %s, not %s", where, item, match[2], column)
		}
		return match[1]
	}
	if match := armColumnItem.FindStringSubmatch(item); match != nil {
		if match[1] != column {
			t.Fatalf("%s: select item %q fills %s, not %s", where, item, match[1], column)
		}
		return ""
	}
	t.Fatalf("%s: select item %q for %s is neither a '<value>'::text AS <name> literal nor a column reference", where, item, column)
	return ""
}
