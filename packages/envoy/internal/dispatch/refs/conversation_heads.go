package refs

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// conversationHeadPageItems lists the items of the issue $1 written by $2 as conversationPageItems
// lists them, its comments those of the page given as their ids $3 and the times each was written
// $4, read off their head rows rather than probed in comments by key.
var conversationHeadPageItems = conversationItemsStatement(`unnest($3::text[], $4::timestamptz[]) c (id, created_at)`)

// headItemOwner is what a conversation item's node reads of an issue or a document its row names, as
// headDocumentsStatement reads it: an issue's project, or a document's slug, issue, project,
// session, kind and whether it is primary (documentColumns).
type headItemOwner struct {
	project                       *string
	issueKey, session, slug, kind *string
	primary                       *bool
}

// ConversationPageNodesOfHeads is ConversationPageNodes read off heads, the rows of a marked issue's
// skeleton read (HeadRows with a HeadSkeleton naming page's comments), and false where it cannot
// be. The asks and the messages of the issue, and their blocks, are read as conversationNodes reads
// them; each comment of page and each of its current blocks is built from its head row (HeadItem)
// as loadCommentNodes and loadCommentBlockNodes build it from the store, with the project and the
// documents its row names read with the head rows (HeadTree's owners), and every node is
// answered in conversationNodes' order. It is false, and reads nothing, where a comment of page
// holds no head row of its conversation columns, or it or one of its blocks is a node the store
// holds reserved for its author, whom the loaders' predicate compares with who and the rows do not:
// the caller reads ConversationPageNodes then.
func ConversationPageNodesOfHeads(ctx context.Context, q Queryer, who model.Actor, issueKey string, at time.Time, page CommentPage, heads *HeadTree) ([]ExcerptedNode, bool, error) {
	blocksOf := headCommentBlocks(heads)
	ids := make([]string, 0, len(page.IDs))
	written := make([]time.Time, 0, len(page.IDs))
	for _, id := range page.IDs {
		head, held := heads.Rows[Target{Kind: graphstore.KindComment, ID: id}]
		if !held || head.Item == nil || head.Item.MadeAt == nil || headReserved(head) {
			return nil, false, nil
		}
		for _, block := range blocksOf[id] {
			if headReserved(block) {
				return nil, false, nil
			}
		}
		// The comments arm of the items read keeps a comment of the issue written by `at` alone.
		if head.Item.IssueKey == nil || *head.Item.IssueKey != issueKey || head.Item.MadeAt.After(at) {
			continue
		}
		ids, written = append(ids, id), append(written, *head.Item.MadeAt)
	}
	rows, err := q.Query(ctx, conversationHeadPageItems, issueKey, at, ids, written)
	if err != nil {
		return nil, false, fmt.Errorf("list the items of %s: %w", issueKey, err)
	}
	var listed []edgeRow
	others := map[ItemKind][]string{}
	askKinds := map[string]string{}
	for rows.Next() {
		var item edgeRow
		var askKind string
		if err := rows.Scan(&item.otherKind, &item.otherID, &askKind); err != nil {
			rows.Close()
			return nil, false, fmt.Errorf("scan an item of %s: %w", issueKey, err)
		}
		listed = append(listed, item)
		if ItemKind(item.otherKind) != ItemComment {
			others[ItemKind(item.otherKind)] = append(others[ItemKind(item.otherKind)], item.otherID)
		}
		if ItemKind(item.otherKind) == ItemAsk {
			askKinds[item.otherID] = askKind
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("list the items of %s: %w", issueKey, err)
	}
	passing := map[ItemKind]map[string]bool{ItemComment: {}}
	for _, id := range ids {
		passing[ItemComment][id] = true
	}
	for kind, of := range others {
		if passing[kind], err = conversationItemIDs(ctx, q, kind, of, at); err != nil {
			return nil, false, err
		}
	}
	kept := slices.DeleteFunc(listed, func(item edgeRow) bool { return !passing[ItemKind(item.otherKind)][item.otherID] })
	if len(kept) == 0 {
		return []ExcerptedNode{}, true, nil
	}
	var loading []edgeRow
	for _, item := range kept {
		if ItemKind(item.otherKind) != ItemComment {
			loading = append(loading, item)
		}
	}
	blocks := map[[2]string][]edgeRow{}
	if len(loading) > 0 {
		if blocks, err = conversationBlocks(ctx, q, loading); err != nil {
			return nil, false, err
		}
		for _, item := range slices.Clone(loading) {
			loading = append(loading, blocks[[2]string{item.otherKind, item.otherID}]...)
		}
	}
	resolved := map[[2]string]resolvedNode{}
	if len(loading) > 0 {
		if resolved, err = resolveNodes(ctx, q, who, false, loading); err != nil {
			return nil, false, err
		}
	}
	if err := resolveHeadComments(kept, heads, blocksOf, blocks, resolved); err != nil {
		return nil, false, err
	}
	nodes := make([]ExcerptedNode, 0, len(kept))
	for _, item := range kept {
		node, found := resolved[[2]string{item.otherKind, item.otherID}]
		if !found {
			continue
		}
		nodes = append(nodes, ExcerptedNode{Node: node.GraphNode, Excerpt: excerptOf(node), AskKind: askKindOf(item, askKinds)})
		for _, block := range blocks[[2]string{item.otherKind, item.otherID}] {
			if node, found := resolved[[2]string{block.otherKind, block.otherID}]; found {
				nodes = append(nodes, ExcerptedNode{Node: node.GraphNode, Excerpt: excerptOf(node)})
			}
		}
	}
	return nodes, true, nil
}

// headReserved is whether the store holds the node of head reserved for its author.
func headReserved(head *HeadRow) bool {
	return head.stored.Visibility != nil && *head.stored.Visibility == graphstore.VisibilityAuthor
}

// headCommentBlocks is the current blocks heads holds of each comment, by the comment's id, each in
// its position's order, as conversationBlocks lists a comment's blocks.
func headCommentBlocks(heads *HeadTree) map[string][]*HeadRow {
	blocks := map[string][]*HeadRow{}
	for target, head := range heads.Rows {
		if target.Kind != graphstore.KindCommentBlock || head.Item == nil || head.Item.Position == nil {
			continue
		}
		if kind, itemID, _, ok := ParseItemBlockNode(target.ID); ok && kind == ItemComment {
			blocks[itemID] = append(blocks[itemID], head)
		}
	}
	for _, held := range blocks {
		slices.SortFunc(held, func(left, right *HeadRow) int { return *left.Item.Position - *right.Item.Position })
	}
	return blocks
}

// resolveHeadComments builds, into resolved, the node of each comment kept names and of each of its
// current blocks, from their head rows and the owners heads read with them, and lists each
// comment's blocks into blocks, as conversationBlocks and resolveNodes would.
func resolveHeadComments(kept []edgeRow, heads *HeadTree, blocksOf map[string][]*HeadRow,
	blocks map[[2]string][]edgeRow, resolved map[[2]string]resolvedNode) error {
	for _, item := range kept {
		if ItemKind(item.otherKind) != ItemComment {
			continue
		}
		id := item.otherID
		head := heads.Rows[Target{Kind: graphstore.KindComment, ID: id}]
		node, err := headCommentNode(id, head, heads.owners)
		if err != nil {
			return err
		}
		resolved[[2]string{string(ItemComment), id}] = node
		key := [2]string{string(ItemComment), id}
		for _, block := range blocksOf[id] {
			node, err := headCommentBlockNode(id, block, heads.owners)
			if err != nil {
				return err
			}
			resolved[[2]string{ItemComment.BlockKind(), block.Node.ID}] = node
			blocks[key] = append(blocks[key], edgeRow{otherKind: ItemComment.BlockKind(), otherID: block.Node.ID})
		}
	}
	return nil
}

// headItemProject is the project and the slug both loaders select of a comment's row: the project
// of the issue issueKey names, else that of the document artifactID names, else empty; and that
// document's slug, else empty.
func headItemProject(owners map[[2]string]headItemOwner, issueKey, artifactID *string) (project, slug string) {
	var document headItemOwner
	if artifactID != nil {
		document = owners[[2]string{"artifact", *artifactID}]
	}
	var issue headItemOwner
	if issueKey != nil {
		issue = owners[[2]string{"issue", *issueKey}]
	}
	switch {
	case issue.project != nil:
		project = *issue.project
	case document.project != nil:
		project = *document.project
	}
	if document.slug != nil {
		slug = *document.slug
	}
	return project, slug
}

// headCommentNode is the comment id as loadCommentNodes resolves it, from its head row: its parent
// the comment it answers, else its thread's head, else its ask (commentReplyParent), and a comment
// that answers none of them the document it is written on, else its issue.
func headCommentNode(id string, head *HeadRow, owners map[[2]string]headItemOwner) (resolvedNode, error) {
	item := head.Item
	project, slug := headItemProject(owners, item.IssueKey, item.ArtifactID)
	node := model.GraphNode{Kind: string(ItemComment), ID: id, IssueKey: item.IssueKey, Project: project,
		Ref: itemRef(string(ItemComment), item.IssueKey, project, slug, id), EditedAt: item.EditedAt}
	if err := setMadeAndChanged(&node, item.Author, *item.MadeAt, item.Author, item.ChangedAt, storedActor); err != nil {
		return resolvedNode{}, err
	}
	setStored(&node, head.stored)
	var err error
	if node.DeletedAt, node.DeletedBy, err = model.DeletedPostMark(item.DeletedAt, item.DeletedBy); err != nil {
		return resolvedNode{}, fmt.Errorf("comment node %s: %w", id, err)
	}
	var document documentParent
	if item.DocumentID != nil {
		if held, found := owners[[2]string{"artifact", *item.DocumentID}]; found {
			documentID := *item.DocumentID
			document = documentParent{id: &documentID, issueKey: held.issueKey, session: held.session, slug: held.slug,
				kind: held.kind, primary: held.primary}
			if held.project != nil {
				document.project = *held.project
			}
		}
	}
	switch reply, parent := commentReplyParent(item.IssueKey, project, slug, item.Answers, item.ReplyTo, item.AskID), document.parent(); {
	case reply != nil:
		node.Parent = reply
	case parent != nil:
		node.Parent, node.ParentVersion = parent, item.Version
	default:
		node.Parent = issueParent(item.IssueKey)
	}
	body := ""
	if item.Text != nil {
		body = *item.Text
	}
	return resolvedNode{GraphNode: node, text: body}, nil
}

// headCommentBlockNode is the block of the comment commentID that head is the row of, as
// itemBlockNodes resolves a current block of a comment, from its head row.
func headCommentBlockNode(commentID string, head *HeadRow, owners map[[2]string]headItemOwner) (resolvedNode, error) {
	item := head.Item
	if item.BlockID == nil || item.Version == nil || item.ChangedAt == nil || item.Excerpt == nil {
		return resolvedNode{}, fmt.Errorf("the head row of block %s holds no comment_blocks row", head.Node.ID)
	}
	authors, err := storedActor(item.Author)
	if err != nil {
		return resolvedNode{}, fmt.Errorf("decode the author of block %s: %w", head.Node.ID, err)
	}
	project, slug := headItemProject(owners, item.IssueKey, item.ArtifactID)
	changedRevision, changedAt := *item.Version, *item.ChangedAt
	node := model.GraphNode{
		Kind:           "block",
		ID:             head.Node.ID,
		BlockID:        *item.BlockID,
		IssueKey:       item.IssueKey,
		Project:        project,
		Ref:            BlockRef(itemRef(string(ItemComment), item.IssueKey, project, slug, commentID), *item.BlockID),
		ChangedVersion: changedRevision,
		MadeBy:         authors,
		MadeAt:         item.MadeAt,
		ChangedBy:      authors,
		ChangedAt:      &changedAt,
		Parent:         itemParent(string(ItemComment), item.IssueKey, project, slug, commentID),
		ParentVersion:  &changedRevision,
		EditedAt:       item.EditedAt,
	}
	if node.DeletedAt, node.DeletedBy, err = model.DeletedPostMark(item.DeletedAt, item.DeletedBy); err != nil {
		return resolvedNode{}, fmt.Errorf("block node %s: %w", head.Node.ID, err)
	}
	setStored(&node, head.stored)
	return resolvedNode{GraphNode: node, text: *item.Excerpt}, nil
}
