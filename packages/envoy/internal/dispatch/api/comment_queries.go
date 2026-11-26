package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// listComments answers an issue's comment list, scoped by ?artifact= to the comments anchored in
// one of its documents. With ?node_anchor=true it answers each comment with the member
// node_anchor (answerNodeAnchoredComments).
func (s *server) listComments(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	nodeAnchors, err := wantsNodeAnchors(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	owner, artifactFilter := issueOwner(r.PathValue("key")), strings.TrimSpace(r.URL.Query().Get("artifact"))
	if nodeAnchors {
		s.answerNodeAnchoredComments(w, r, owner, artifactFilter)
		return
	}
	s.answerCommentList(w, r, owner, artifactFilter, r.PathValue("key"))
}

// wantsNodeAnchors reports whether the request asks for the member node_anchor on each comment
// whose anchor names a node or a block (this issue's spec R31): ?node_anchor=true. A deployed
// client parses a comment with the strict CommentSchema of packages/contracts, so only a caller
// that asks gets the member, and parses it with CommentWithNodeAnchorSchema. Any value but true and
// false is 400 INVALID_QUERY naming the parameter.
func wantsNodeAnchors(r *http.Request) (bool, error) {
	switch value := r.URL.Query().Get("node_anchor"); value {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, errorf(http.StatusBadRequest, "INVALID_QUERY", "node_anchor %q is neither true nor false", value)
	}
}

// nodeAnchoredComment is a comment as a read asked with ?node_anchor=true answers it: the comment
// as every read shapes it, and, where its anchor names a node or a block, node_anchor
// (model.CommentNodeAnchor).
type nodeAnchoredComment struct {
	shapedComment
	NodeAnchor *model.CommentNodeAnchor `json:"node_anchor,omitempty"`
}

// answerNodeAnchoredComments answers an issue's comment list asked with ?node_anchor=true: the
// comments answerCommentList answers, in its order, each with its node_anchor (loadNodeAnchors).
// It carries no ETag and keeps no answer: a node anchor names its node's state, which a write of
// that node moves without writing a comment row, so the list's watermark, which only a write of a
// comment raises, cannot stand behind it.
func (s *server) answerNodeAnchoredComments(w http.ResponseWriter, r *http.Request, owner owner, artifactFilter string) {
	comments, err := s.loadOwnerComments(r.Context(), s.deps.Store.Pool, everyReader, owner, artifactFilter)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	anchors, err := loadNodeAnchors(r.Context(), s.deps.Store.Pool, comments)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	reader := s.readDisplayTrees(r.Context(), wantsDisplayTrees(r))
	shaped := shapedComments(comments, reader)
	reader.resolve()
	reads := make([]nodeAnchoredComment, len(shaped))
	for index := range shaped {
		reads[index] = nodeAnchoredComment{shapedComment: shaped[index], NodeAnchor: anchors[shaped[index].ID]}
	}
	WriteJSON(w, http.StatusOK, reads)
}

// nodeAnchorsStatement is the node each comment $1 names whose anchor names one: a node anchor's
// node (comments.node_anchor), and a document anchor's block, addressed as the block index
// addresses it (graphstore.AnchorBlockAddressSQL), with the node's state, live where the store
// holds no row of it, the anchor's quote, its occurrence, null for a document anchor, which stores
// none, and its orphaned mark as stored.
var nodeAnchorsStatement = `
	select c.id::text, named.kind, named.id, coalesce(n.state, '` + graphstore.StateLive + `'),
	       coalesce(case when c.node_anchor is not null then c.node_anchor->>'quote' else c.anchor->>'quote' end, ''),
	       case when c.node_anchor is not null then (c.node_anchor->>'occurrence')::int end,
	       coalesce(case when c.node_anchor is not null then (c.node_anchor->>'orphaned')::boolean
	                     else (c.anchor->>'orphaned')::boolean end, false)
	from comments c
	cross join lateral (
		select case when c.node_anchor is not null then c.node_anchor->>'node_kind' else '` + refs.BlockNodeKind + `' end as kind,
		       case when c.node_anchor is not null then c.node_anchor->>'node_id' else ` + graphstore.AnchorBlockAddressSQL("c") + ` end as id
	) named
	left join graph_nodes n on n.kind = named.kind and n.id = named.id
	where c.id = any($1::uuid[]) and named.id is not null`

// loadNodeAnchors is the node_anchor of each of comments whose anchor names a node or a block
// (nodeAnchorsStatement), by comment id, read through q in one statement. A comment's row read
// (scanComment) carries no node anchor, so every comment is asked for, and the statement answers
// those whose anchor names a node or a block.
func loadNodeAnchors(ctx context.Context, q queryer, comments []model.Comment) (map[string]*model.CommentNodeAnchor, error) {
	anchors := map[string]*model.CommentNodeAnchor{}
	ids := make([]string, len(comments))
	for index, comment := range comments {
		ids[index] = comment.ID
	}
	if len(ids) == 0 {
		return anchors, nil
	}
	rows, err := q.Query(ctx, nodeAnchorsStatement, ids)
	if err != nil {
		return nil, fmt.Errorf("read the node anchors of %d comments: %w", len(ids), err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var anchor model.CommentNodeAnchor
		if err := rows.Scan(&id, &anchor.Node.Kind, &anchor.Node.ID, &anchor.Node.State, &anchor.Quote, &anchor.Occurrence, &anchor.Orphaned); err != nil {
			return nil, fmt.Errorf("read a comment's node anchor: %w", err)
		}
		anchors[id] = &anchor
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the node anchors of %d comments: %w", len(ids), err)
	}
	return anchors, nil
}

// answerCommentList answers one comment list - an issue's (listComments) or an unlinked
// document's (listArtifactComments) - in the order the activity feed answers a page of events
// (listIssueEvents): the validator is read first, in one statement over metadata, and the
// comments are loaded only for a reader that neither holds that validator nor can be written the
// bytes this server kept under it (list_answers.go). A warm read therefore builds nothing - no
// comment row, no mention, no delivery attempt, no display tree, no encode, no compression -
// which is what a validator over the answer's own bytes could never do, since the answer had to
// exist before its key was known.
//
// The validator stands on the list's watermark: a counter per comment-list owner that three
// deferred constraint triggers raise for every write of comments, comment_mentions and
// comment_deliveries (migration 20261106020619). It cannot stand on the issue's last_seq the way
// the feed's does, because a delivery claim and the resolution recorded before a send write a
// delivery row and append no event at all (comment_delivery.go), and a reader whose list now
// shows that attempt would be answered 304. Deferred to commit, the counter rises inside the
// writing transaction, so a reader sees the new rows and the new counter together or neither.
//
// last_seq is hashed in beside it for the one part of the answer those three tables do not
// decide: a reply that answers a block carries that block's reference, built from the owner's
// project key and the document's slug (refs.ReplyBlocks), and a rename of either moves the
// owner's event sequence without touching a comment row. The owner and the ?artifact= scope are
// in the hash too, so no two lists of one deploy can be given one validator.
//
// name is the owner as the request spells it: the key the kept answer is filed under beside its
// ETag, and the subject the display tree read is logged against.
func (s *server) answerCommentList(w http.ResponseWriter, r *http.Request, owner owner, artifactFilter, name string) {
	watermark, lastSeq, ownerHeld, err := s.commentListWatermark(r.Context(), owner)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	trees := wantsDisplayTrees(r)
	etag := s.commentListETag(owner, artifactFilter, watermark, lastSeq, trees)
	if answerNotModified(w, r, etag) {
		return
	}
	key := listAnswerKeyFor(r, commentListRoute, name, etag)
	if s.listAnswers.answer(w, key) {
		return
	}
	// The answer is kept and handed to every reader whose request names this owner and ETag
	// (listAnswers), so it is read as everyReader: one reader's view of a block reserved for its
	// author must never be the answer kept for the next.
	comments, err := s.loadOwnerComments(r.Context(), s.deps.Store.Pool, everyReader, owner, artifactFilter)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	reader := s.readDisplayTrees(r.Context(), trees)
	reads := shapedComments(comments, reader)
	reader.resolve()
	encoded, err := encodeListAnswer(reads)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.writeListAnswer(w, key, encoded, ownerHeld)
}

// commentListWatermark is the pair a comment list's validator is built on, read in one statement
// with whether the owner holding it is there at all: the list's own watermark, and the newest
// event sequence of its owner. Each counter reads 0 where no row answers - an owner nothing has
// written carries no counter, and a key no issue holds answers the empty list rather than a
// refusal, as this route has always done - so a list that has never been written and one whose
// owner does not exist are one case in the answer, as they have always been. They are two cases
// for the server's own keeping: the empty list of an owner no row holds is kept under no key,
// since that key is the path the caller spelled (writeListAnswer).
func (s *server) commentListWatermark(ctx context.Context, owner owner) (int64, int64, bool, error) {
	var query, value string
	switch {
	case owner.IssueKey != nil:
		query, value = `
			select coalesce((select seq from comment_watermarks where issue_key = $1), 0),
			       coalesce((select last_seq from issues where key = $1), 0),
			       exists(select 1 from issues where key = $1)
		`, *owner.IssueKey
	case owner.ArtifactID != nil:
		query, value = `
			select coalesce((select seq from comment_watermarks where artifact_id = $1), 0),
			       coalesce((select max(seq) from events where artifact_id = $1), 0),
			       exists(select 1 from artifacts where id = $1)
		`, *owner.ArtifactID
	default:
		return 0, 0, false, errorf(http.StatusBadRequest, "OWNER_INVALID", "owner requires exactly one issue or artifact")
	}
	var watermark, lastSeq int64
	var ownerHeld bool
	if err := s.deps.Store.Pool.QueryRow(ctx, query, value).Scan(&watermark, &lastSeq, &ownerHeld); err != nil {
		return 0, 0, false, fmt.Errorf("read the comment list's watermark: %w", err)
	}
	return watermark, lastSeq, ownerHeld, nil
}

// commentListETag is a comment list answer's validator: the list it is of, the watermark every
// write the list shows raises, and the owner's event sequence, closed with the lines both lists'
// ETags carry (listETag): one line per part, then the hash of them. Unlike the feed's, it names
// its owner, so two lists standing at one count never share a validator.
func (s *server) commentListETag(owner owner, artifactFilter string, watermark, lastSeq int64, trees bool) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "issue=%s\nartifact=%s\nfilter=%s\nwatermark=%d\nlast_seq=%d\n",
		deref(owner.IssueKey), deref(owner.ArtifactID), artifactFilter, watermark, lastSeq)
	return s.listETag(hash, trees)
}

func (s *server) loadOwnerComments(ctx context.Context, q queryer, who model.Actor, owner owner, artifactFilter string) ([]model.Comment, error) {
	var ownerColumn, ownerValue string
	switch {
	case owner.IssueKey != nil:
		ownerColumn, ownerValue = "issue_key", *owner.IssueKey
	case owner.ArtifactID != nil:
		ownerColumn, ownerValue = "artifact_id", *owner.ArtifactID
	default:
		return nil, errorf(http.StatusBadRequest, "OWNER_INVALID", "owner requires exactly one issue or artifact")
	}
	rows, err := q.Query(ctx, fmt.Sprintf(`
		select `+commentColumns+`
		from comments
		where %s = $1 and ($2 = '' or anchor->>'artifact_id' = $2)
		order by created_at, id
	`, ownerColumn), ownerValue, artifactFilter)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	comments := []model.Comment{}
	for rows.Next() {
		comment, err := scanComment(rows)
		if err != nil {
			return nil, err
		}
		comments = append(comments, comment)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return s.hydrateCommentSideTables(ctx, q, who, comments)
}

func commentHasOwner(comment model.Comment, owner owner) bool {
	if owner.IssueKey != nil {
		return comment.IssueKey != nil && *comment.IssueKey == *owner.IssueKey
	}
	return owner.ArtifactID != nil && comment.ArtifactID != nil && *comment.ArtifactID == *owner.ArtifactID
}

// commentRead is the body GET /api/v1/comments/{id} answers.
type commentRead struct {
	Comment shapedComment   `json:"comment"`
	Replies []shapedComment `json:"replies"`
}

// nodeAnchoredCommentRead is the body GET /api/v1/comments/{id}?node_anchor=true answers: the
// comment with its node_anchor where its anchor names a node or a block (this issue's spec R31),
// and its replies, which carry no anchor, as commentRead answers them.
type nodeAnchoredCommentRead struct {
	Comment nodeAnchoredComment `json:"comment"`
	Replies []shapedComment     `json:"replies"`
}

func (s *server) getComment(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !requireUUIDPath(w, r, "comment") {
		return
	}
	nodeAnchors, err := wantsNodeAnchors(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	who, ok := s.actingReader(w, r)
	if !ok {
		return
	}
	comment, err := s.loadComment(r.Context(), s.deps.Store.Pool, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	comments, err := s.hydrateCommentSideTables(r.Context(), s.deps.Store.Pool, who, []model.Comment{comment})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	comments[0].AnchorPosition, err = s.anchorBlock(r.Context(), comments[0].Anchor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	replies, err := s.loadReplyChain(r.Context(), s.deps.Store.Pool, who, "reply_to", comment.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var anchors map[string]*model.CommentNodeAnchor
	if nodeAnchors {
		if anchors, err = loadNodeAnchors(r.Context(), s.deps.Store.Pool, comments); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	reader := s.readDisplayTrees(r.Context(), wantsDisplayTrees(r))
	read := commentRead{Replies: shapedComments(replies, reader)}
	read.Comment.Comment = comments[0]
	read.Comment.shape(reader)
	reader.resolve()
	if nodeAnchors {
		WriteJSON(w, http.StatusOK, nodeAnchoredCommentRead{
			Comment: nodeAnchoredComment{shapedComment: read.Comment, NodeAnchor: anchors[comment.ID]},
			Replies: read.Replies,
		})
		return
	}
	WriteJSON(w, http.StatusOK, read)
}

// replyChainQuery is every comment transitively replying to the row(s) matching
// "<seedColumn> = $1", oldest first; %s is that seed column. comments.reply_to has no
// acyclicity constraint, so the walk unions on the visited row: a cycle repeats a row already
// in the set and adds nothing, rather than spinning the request. query_plan_test.go plans
// this exact text, so the plan it checks is the one the server runs.
const replyChainQuery = `
		with recursive replies as (
			select ` + commentThreadColumns + `
			from comments c where c.%s = $1
			union
			select ` + commentThreadColumns + `
			from comments c join replies r on c.reply_to = r.id
		)
		select ` + commentColumns + `
		from replies
		order by created_at, id`

// inboxAskReplyChainsQuery hydrates every Inbox card's thread in one walk: the same recursion
// as replyChainQuery, seeded from a set of asks and carrying the ask each row belongs to, so a
// comment under two asks yields one row per ask. Planned by query_plan_test.go too.
const inboxAskReplyChainsQuery = `
		with recursive replies as (
			select c.ask_id as thread_id, ` + commentThreadColumns + `
			from comments c
			where c.ask_id = any($1::uuid[])
			union
			select r.thread_id, ` + commentThreadColumns + `
			from comments c join replies r on c.reply_to = r.id
		)
		select ` + commentColumns + `, thread_id::text
		from replies
		order by thread_id, created_at, id`

// loadReplyChain returns every comment transitively replying to the row(s)
// matching "<seedColumn> = seedValue", ordered oldest first, each reply's block read for who.
// seedColumn selects the thread root: "reply_to" for a comment's own replies, "ask_id"
// for an ask's directly-replying comments (plus, either way, every comment
// that replies to one of those via reply_to). seedColumn is caller-controlled
// and never derived from request input.
func (s *server) loadReplyChain(ctx context.Context, q queryer, who model.Actor, seedColumn, seedValue string) ([]model.Comment, error) {
	rows, err := q.Query(ctx, fmt.Sprintf(replyChainQuery, seedColumn), seedValue)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	replies := []model.Comment{}
	for rows.Next() {
		reply, err := scanComment(rows)
		if err != nil {
			return nil, err
		}
		replies = append(replies, reply)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return s.hydrateCommentSideTables(ctx, q, who, replies)
}

// withReply is replies, a chain loadReplyChain read, with reply, a reply to the chain's root this
// transaction has queued the insert of and the table does not hold yet, in the place the chain's
// query would have returned it: ordered by created_at, then id (a uuid orders as its canonical
// text does).
func withReply(replies []model.Comment, reply model.Comment) []model.Comment {
	index, _ := slices.BinarySearchFunc(replies, reply, func(stored, added model.Comment) int {
		if order := stored.CreatedAt.Compare(added.CreatedAt); order != 0 {
			return order
		}
		return strings.Compare(stored.ID, added.ID)
	})
	return slices.Insert(replies, index, reply)
}

func (s *server) loadComment(ctx context.Context, q queryer, id string) (model.Comment, error) {
	return scanComment(q.QueryRow(ctx, `
		select `+commentColumns+`
		from comments where id = $1
	`, id))
}

// loadOwnedComment reads a comment and refuses one that is not on this owner, with the 400 every
// reply_to walk answers. It locks no row: the caller holds the owner's lock (requireOpenOwner),
// which every writer of a comment takes before it reads one, so the comment cannot change under
// the caller until it commits.
func (s *server) loadOwnedComment(
	ctx context.Context, tx pgx.Tx, owner owner, id string,
) (model.Comment, error) {
	comment, err := s.loadComment(ctx, tx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !commentHasOwner(comment, owner)) {
		return model.Comment{}, errorf(http.StatusBadRequest, "INVALID_COMMENT", "reply_to must identify a comment on this owner")
	}
	if err != nil {
		return model.Comment{}, err
	}
	return comment, nil
}

// commentColumns is the comments select list scanComment reads, in scan order.
const commentColumns = `id::text, issue_key, artifact_id::text, author, body, anchor, reply_to::text, ask_id::text, turn, resolved, resolved_by, resolved_at, edited_at, suggestion, created_at, replies_to_block, answers::text, deleted_at, deleted_by`

// commentThreadColumns is commentColumns without the ::text casts, qualified to the comments
// alias `c`. A recursive walk joins c.reply_to to the visited row's id, so those columns have
// to stay uuid inside the CTE; and a recursive union cannot take c.* because comments.search
// is a tsvector, which has no hash operator.
const commentThreadColumns = `c.id, c.issue_key, c.artifact_id, c.author, c.body, c.anchor, c.reply_to,
		c.ask_id, c.turn, c.resolved, c.resolved_by, c.resolved_at, c.edited_at, c.suggestion, c.created_at, c.replies_to_block,
		c.answers, c.deleted_at, c.deleted_by`

func scanComment(row pgx.Row, extra ...any) (model.Comment, error) {
	var comment model.Comment
	var author, anchor, resolvedBy, suggestion, deletedBy []byte
	var resolvedAt, editedAt, deletedAt *time.Time
	destinations := []any{
		&comment.ID, &comment.IssueKey, &comment.ArtifactID, &author, &comment.Body, &anchor, &comment.ReplyTo, &comment.AskID, &comment.Turn, &comment.Resolved,
		&resolvedBy, &resolvedAt, &editedAt, &suggestion, &comment.CreatedAt, &comment.RepliesToBlock, &comment.Answers, &deletedAt, &deletedBy,
	}
	destinations = append(destinations, extra...)
	if err := row.Scan(destinations...); err != nil {
		return model.Comment{}, err
	}
	if err := json.Unmarshal(author, &comment.Author); err != nil {
		return model.Comment{}, fmt.Errorf("decode comment author: %w", err)
	}
	if len(anchor) > 0 {
		var value model.Anchor
		if err := model.DecodeAnchor(anchor, &value); err != nil {
			return model.Comment{}, fmt.Errorf("decode comment anchor: %w", err)
		}
		comment.Anchor = &value
	}
	if len(resolvedBy) > 0 {
		var value model.Actor
		if err := json.Unmarshal(resolvedBy, &value); err != nil {
			return model.Comment{}, fmt.Errorf("decode comment resolved_by: %w", err)
		}
		comment.ResolvedBy = &value
	}
	var err error
	if comment.DeletedAt, comment.DeletedBy, err = model.DeletedPostMark(deletedAt, deletedBy); err != nil {
		return model.Comment{}, err
	}
	comment.ResolvedAt = timestampPtr(resolvedAt)
	comment.EditedAt = timestampPtr(editedAt)
	if len(suggestion) > 0 {
		var value model.Suggestion
		if err := json.Unmarshal(suggestion, &value); err != nil {
			return model.Comment{}, fmt.Errorf("decode comment suggestion: %w", err)
		}
		comment.Suggestion = &value
	}
	comment.Mentions = []model.Mention{}
	comment.Deliveries = []model.CommentDelivery{}
	return comment, nil
}

// hydrateCommentSideTables loads mentions, delivery attempts and the block each reply answers once
// per comment page, avoiding a per-comment query while keeping all reads of a Comment's visible
// delivery state consistent.
func (s *server) hydrateCommentSideTables(ctx context.Context, q queryer, who model.Actor, comments []model.Comment) ([]model.Comment, error) {
	ids := make([]string, len(comments))
	replies := make([]*model.Comment, len(comments))
	for index := range comments {
		ids[index] = comments[index].ID
		replies[index] = &comments[index]
	}
	mentions, err := s.loadCommentMentions(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	deliveries, err := s.loadCommentDeliveries(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	if err := hydrateCommentReplyBlocks(ctx, q, who, replies...); err != nil {
		return nil, err
	}
	for index := range comments {
		comments[index].Mentions = mentions[comments[index].ID]
		comments[index].Deliveries = deliveries[comments[index].ID]
	}
	return comments, nil
}

// hydrateWrittenComment is the comment a write answers, with the side tables
// GET /api/v1/comments/{id} reads it with (hydrateCommentSideTables): its mentions, its delivery
// attempts and the block it replies to, read for who, the writer the answer goes to. A write
// calls it on its own transaction, tx, in its read phase, before Commit runs its queued writes,
// so a read that fails ends the request with nothing written. The rows it reads are ones no
// write that answers this way queues a change to, but for the delete's settle of the attempts
// still pending, which the delete sets in what this returns as its settle leaves them
// (deleteComment). The anchor's position stays out: getComment computes it from the document,
// and no side table holds it.
func (s *server) hydrateWrittenComment(ctx context.Context, tx pgx.Tx, who model.Actor, comment model.Comment) (model.Comment, error) {
	comments, err := s.hydrateCommentSideTables(ctx, tx, who, []model.Comment{comment})
	if err != nil {
		return model.Comment{}, err
	}
	return comments[0], nil
}

// freshComment is one comment as GET /api/v1/comments/{id} reads it for who and a write of it
// answers it (hydrateWrittenComment): the row with its mentions, its delivery attempts and the
// block it replies to, read now from the pool.
func (s *server) freshComment(ctx context.Context, who model.Actor, id string) (model.Comment, error) {
	comment, err := s.loadComment(ctx, s.deps.Store.Pool, id)
	if err != nil {
		return model.Comment{}, err
	}
	comments, err := s.hydrateCommentSideTables(ctx, s.deps.Store.Pool, who, []model.Comment{comment})
	if err != nil {
		return model.Comment{}, err
	}
	return comments[0], nil
}

// rereadComment is the keyedReread of each route that answers one comment shaped for the
// dashboard - its edit, resolve, reopen, accept and reject: the comment as it stands now, read for
// the caller the repeat answers (freshComment), shaped as those routes shape it, an accept's lost
// kept from the first answer.
func (s *server) rereadComment(w http.ResponseWriter, r *http.Request, id string, status int, stored []byte) {
	who, ok := s.actingReader(w, r)
	if !ok {
		return
	}
	comment, err := s.freshComment(r.Context(), who, id)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.writeRefreshed(w, status, stored, newShapedComment(comment, wantsDisplayTrees(r)))
}

// rereadCommentReply is the keyedReread of POST /api/v1/comments/{id}/reply, whose answer is the
// reply, not the comment the path names: a reply the session posted (201) as rereadComment answers
// a comment, its ask_waiting_on kept, and the stored reply handed back for an attempt already
// answered (200) unshaped, as replyComment answers it.
func (s *server) rereadCommentReply(w http.ResponseWriter, r *http.Request, id string, status int, stored []byte) {
	if status == http.StatusCreated {
		s.rereadComment(w, r, id, status, stored)
		return
	}
	who, ok := s.actingReader(w, r)
	if !ok {
		return
	}
	reply, err := s.freshComment(r.Context(), who, id)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.writeRefreshed(w, status, stored, reply)
}

func (s *server) loadCommentMentions(ctx context.Context, q queryer, commentIDs []string) (map[string][]model.Mention, error) {
	mentions := make(map[string][]model.Mention, len(commentIDs))
	for _, id := range commentIDs {
		mentions[id] = []model.Mention{}
	}
	if len(commentIDs) == 0 {
		return mentions, nil
	}
	rows, err := q.Query(ctx, `
		select comment_id::text, target, delivery, resolved_session_id
		from comment_mentions
		where comment_id = any($1::uuid[])
		order by comment_id, target
	`, commentIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var commentID string
		var mention model.Mention
		if err := rows.Scan(&commentID, &mention.Target, &mention.Delivery, &mention.SessionID); err != nil {
			return nil, err
		}
		mentions[commentID] = append(mentions[commentID], mention)
	}
	return mentions, rows.Err()
}

func (s *server) loadCommentDeliveries(ctx context.Context, q queryer, commentIDs []string) (map[string][]model.CommentDelivery, error) {
	deliveries := make(map[string][]model.CommentDelivery, len(commentIDs))
	for _, id := range commentIDs {
		deliveries[id] = []model.CommentDelivery{}
	}
	if len(commentIDs) == 0 {
		return deliveries, nil
	}
	rows, err := q.Query(ctx, `
		select `+commentDeliveryColumns+`
		from comment_deliveries
		where comment_id = any($1::uuid[])
		order by comment_id, target, attempt
	`, commentIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		delivery, err := scanCommentDelivery(rows)
		if err != nil {
			return nil, err
		}
		deliveries[delivery.CommentID] = append(deliveries[delivery.CommentID], delivery)
	}
	return deliveries, rows.Err()
}
