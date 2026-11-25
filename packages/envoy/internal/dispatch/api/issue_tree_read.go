package api

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/decisionread"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// The tree read (GET /api/v1/issues/{key}/tree) answers one issue's tree as one reader may see
// it, in the tree's own order: the walk the record of every query is built from
// (docs.WalkIssueTree), with the reader's visibility applied to every node it reaches, the nodes
// no query of the tree returns left out - a document, a document version, and the prose of a role
// document (R2, R16) - and the members each node answers read from the nodes and the rows that
// hold them. A view answers one query's nodes from the same walk, with the fixed context that
// query carries.

// issueTreePageSize is the most nodes one page of the tree read answers where Deps.IssueTreePageSize
// is zero, as it is in every deployment. It stands above the largest issue the implementer
// measured through Dispatch's own API (the pull request body's R8 table: 10,910 blocks, comments,
// asks and messages), so one request answers that issue whole; a larger tree answers a cursor at
// the next node of the walk.
const issueTreePageSize = 12000

// issueTreeCommentPageSize is the most comments a page of the tree read answers, the newest of
// those it carries, and each comments_before page the next older (R8): 99 of 100 issues hold
// fewer, so they read every comment in one answer.
const issueTreeCommentPageSize = 200

// issueTreeViews are the views the tree read answers, each the query of that name.
var issueTreeViews = []string{graphstore.QuerySpec, graphstore.QueryVerification, graphstore.QueryPlan, graphstore.QueryDecisions}

// The roles a node plays in a tree read (model.IssueTreeNode.Role).
const (
	treeRoleTree    = "tree"
	treeRoleQuery   = "query"
	treeRoleContext = "context"
)

// issueTreeQuery is GET /api/v1/issues/{key}/tree's query: the view, the kinds of node of the read
// with no view, the version of the view's query node to read it at, the cursor of a page past the
// first, and the position of a page of the comments past the first.
type issueTreeQuery struct {
	// View is the query whose nodes the read answers: spec, verification, plan or decisions;
	// absent for the whole tree.
	View string `query:"view" enum:"spec,verification,plan,decisions"`
	// Kind filters the read with no view to the nodes of the kinds it names, one kind in each
	// kind=, with every ancestor of each as context: a typed block's type (callout, ask,
	// criterion, requirement, check, plan_task, decision, reading), comment, message, issue (a
	// child issue), lens, risk, constraint, result, finding or prose. callout takes the memo
	// alone, ask the ask's node and never an ask block, and comment a comment tied to no text with
	// the replies under it; a comment on text comes only with the node it is on. block, an empty
	// value and any other value are refused, and so is kind beside view or beside v.
	Kind []string `query:"kind"`
	// V is a version of the view's query node: the read answers the query as of the store
	// revision that version's record read. It needs view beside it. A version the query node
	// does not have is 404 QUERY_VERSION_NOT_FOUND, and one it has that holds no record of the
	// revision it read is 404 QUERY_VERSION_NOT_RECORDED (issueTreeCut).
	V int `query:"v"`
	// Cursor is where a page past the first starts: the cursor the page before it answered.
	Cursor string `query:"cursor"`
	// CommentsBefore is `<made_at>,<id>`, the comments_before a read answered: the read with the
	// same view or kinds answers the 200 comments it carries older than that position, with the
	// nodes under them, and no other node. It is refused beside cursor, and where no older comment
	// of the issue follows it.
	CommentsBefore string `query:"comments_before"`
}

// getIssueTree is GET /api/v1/issues/{key}/tree.
func (s *server) getIssueTree(w http.ResponseWriter, r *http.Request) {
	s.serveIssueTree(w, r, s.readIssueTree)
}

// issueTreeReader is what answers the tree read once its query is read (readIssueTree): the
// route's own reader, and in a test the read with R8's two cuts undone that a test holds the
// route's answers to.
type issueTreeReader func(ctx context.Context, who model.Actor, key, view string, kinds []string, version int, cursor string,
	before *refs.CommentPosition, trees *displayTreeReader) (model.IssueTreeRead, treePagePlan, error)

// serveIssueTree reads GET /api/v1/issues/{key}/tree's query, refusing one it does not take, and
// answers the tree read through read.
func (s *server) serveIssueTree(w http.ResponseWriter, r *http.Request, read issueTreeReader) {
	who, ok := s.actingReader(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	view := query.Get("view")
	if query.Has("view") && !slices.Contains(issueTreeViews, view) {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, "view must be one of spec, verification, plan and decisions, or absent for the whole tree")
		return
	}
	kinds, err := parseTreeKinds(query)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	version := 0
	if query.Has("v") {
		if view == "" {
			writeError(w, "INVALID_QUERY", http.StatusBadRequest, "v names a version of a view's query node: give view beside it")
			return
		}
		number, err := strconv.Atoi(query.Get("v"))
		if err != nil || number < 1 {
			writeError(w, "INVALID_QUERY", http.StatusBadRequest, "v must be a version number, 1 or above")
			return
		}
		version = number
	}
	key := r.PathValue("key")
	var before *refs.CommentPosition
	if query.Has("comments_before") {
		if query.Has("cursor") {
			writeError(w, "INVALID_QUERY", http.StatusBadRequest, fmt.Sprintf("comments_before, %s, pages the comments of the answer that named it, beside its view or its kinds: give it with no cursor",
				refs.CommentPositionShape))
			return
		}
		position, ok := refs.ParseCommentPosition(query.Get("comments_before"))
		if !ok {
			writeError(w, "INVALID_QUERY", http.StatusBadRequest, commentsBeforeRefusal(key))
			return
		}
		before = &position
	}
	if err := s.requireIssueExists(r.Context(), key); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	trees := s.readDisplayTrees(r.Context(), wantsDisplayTrees(r))
	answer, plan, err := read(r.Context(), who, key, view, kinds, version, query.Get("cursor"), before, trees)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	trees.resolve()
	writeIssueTreePage(w, &answer, &plan)
}

// commentsBeforeRefusal is the refusal of a comments_before of another shape, or one that no older
// comment of the issue key follows.
func commentsBeforeRefusal(key string) string {
	return fmt.Sprintf("comments_before must be %s, the made_at (RFC 3339, in UTC, with microseconds) and the id of a comment "+
		"of %s that older comments follow, as the read that named it answers it", refs.CommentPositionShape, key)
}

// readIssueTree is the tree read of the issue key for who: the view's query and its context, or
// the skeleton of the whole tree where view is "", filtered to the kinds of node kinds names where
// it names any, at version of the view's query node or at the head where it is 0, from cursor on,
// its comments the page before names where it is set, with the plan its page is cut by where its
// JSON holds more than issueTreePageBytes (treePagePlan). Every read of the store is made in one
// read-only transaction at repeatable read, with JIT off (refs.TurnJITOff), so the answer is one
// state of the store; the documents' block lists and the occurrences of the comments' document
// anchors in their blocks, which the live rooms answer, are read after it
// (issueTreeBuilder.documentPositions, issueTreeBuilder.countDocuments,
// issueTreeBuilder.anchorOccurrences). trees is the request's reader of the display trees, nil for
// a caller that asked for none. A page holds at most Deps.IssueTreePageSize nodes,
// issueTreePageSize where it is zero.
func (s *server) readIssueTree(ctx context.Context, who model.Actor, key, view string, kinds []string, version int, cursor string, before *refs.CommentPosition,
	trees *displayTreeReader) (model.IssueTreeRead, treePagePlan, error) {
	tx, err := s.deps.Store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return model.IssueTreeRead{}, treePagePlan{}, fmt.Errorf("open the tree read of %s: %w", key, err)
	}
	defer tx.Rollback(ctx)
	if err := refs.TurnJITOff(ctx, tx); err != nil {
		return model.IssueTreeRead{}, treePagePlan{}, err
	}
	builder := &issueTreeBuilder{ctx: ctx, q: tx, who: who, key: key, view: view, kinds: kinds, before: before,
		pageSize: cmp.Or(s.deps.IssueTreePageSize, issueTreePageSize)}
	if err := builder.build(version, cursor); err != nil {
		return model.IssueTreeRead{}, treePagePlan{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.IssueTreeRead{}, treePagePlan{}, fmt.Errorf("close the tree read of %s: %w", key, err)
	}
	if err := builder.documentPositions(s); err != nil {
		return model.IssueTreeRead{}, treePagePlan{}, err
	}
	if err := builder.countDocuments(s); err != nil {
		return model.IssueTreeRead{}, treePagePlan{}, err
	}
	if err := builder.anchorOccurrences(s); err != nil {
		return model.IssueTreeRead{}, treePagePlan{}, err
	}
	answer, plan := builder.answer(version, trees)
	return answer, plan, nil
}

// treeEntry is what the tree read holds of one node while it builds its answer.
type treeEntry struct {
	// target is the node as the store keys it.
	target refs.Target
	// walk is the node's index in the walk, -1 for a node the walk does not take.
	walk int
	// order is where a node the walk does not take was added among the others.
	order int
	// parent is the node it stands under in the store: the walk's parent, or the node a part_of
	// of its own names for a node the walk does not take.
	parent refs.Target
	typ    string
	rank   string
	ranked bool
	madeAt time.Time
	// above is a node of the walk's second part, whose parent is a node of an issue above.
	above bool
	// issue is the issue the node belongs to where it is not the issue read.
	issue string
	// role is what the node is to the answer (treeRoleTree, treeRoleQuery, treeRoleContext).
	role string
	// onText marks a comment of a thread on text no person wrote in, and each of its blocks, which
	// the read adds to the walk's and answers as context (placeTextComments).
	onText bool
	// served is, for a node of an issue under the issue read reached through serves, the first
	// requirement of the walk it serves (readServing).
	served refs.Target
}

// issueTreeBuilder builds one tree read.
type issueTreeBuilder struct {
	ctx  context.Context
	q    pgx.Tx
	who  model.Actor
	key  string
	view string
	// kinds is the kinds of node the read with kind takes (parseTreeKinds), nil for every other
	// read; kindContext is each context node a tuple of a node of its answer names (kindOrder).
	kinds       []string
	kindContext map[refs.Target]bool
	// pageSize is the most nodes one page answers (window).
	pageSize int
	cut      refs.TreeCut
	tree     docs.IssueTree
	// heads is the issue's rows of the head projection a head read of a marked issue reads in
	// place of the walk of the tuples (refs.HeadRows), nil for a read at a version and for an issue
	// no mark names.
	heads *refs.HeadTree

	entries   map[refs.Target]*treeEntry
	walked    []refs.Target
	extras    []refs.Target
	context   []refs.Target
	inContext map[refs.Target]bool
	// named is, for each comment the answer hangs under the issue because it does not hold the
	// comment's parent, that parent: the walk's (docs' IssueTree.Named) and the read's own
	// (placeTextComments).
	named map[refs.Target]refs.Target

	// textComments, textThreads and textOrder are each comment of a thread on text, each such
	// thread, and those comments in the order each was written (queueTextComments); textPage is,
	// of the read with no view and the read with kind, each comment of a thread no person wrote
	// in that its comment page holds (mergeCommentPage); viewPage is a view's comment page, and
	// viewNext the position of its oldest comment where older ones remain (pageViewComments).
	textComments map[refs.Target]treeTextComment
	textThreads  map[string]*treeTextThread
	textOrder    []refs.Target
	textPage     map[string]bool
	viewPage     map[refs.Target]bool
	viewNext     *refs.CommentPosition

	// facts, loaded, rows and blocks are what readFacts read of each node of read, every node it
	// has read.
	facts  map[refs.Target]refs.TreeFacts
	loaded map[refs.Target]refs.ExcerptedNode
	rows   map[refs.Target]refs.TreeNodeRow
	blocks map[string]refs.BlockRow
	read   map[refs.Target]bool

	kept      map[refs.Target]bool
	effective map[refs.Target]refs.Target
	order     []refs.Target
	descend   []string
	ancestors []string

	// taken is each node of the walk the view's query takes (queryTakes), requirements each
	// requirement the verification view carries (verificationRequirements), and tied each node
	// of the walk or the others the spec and the verification views carry as context for a tie of
	// theirs (readTies), each read once by readContext for a view.
	taken        map[refs.Target]bool
	requirements map[refs.Target]bool
	tied         map[refs.Target]bool

	issues    map[string]*model.IssueTreeIssue
	texts     map[refs.Target]string
	asks      map[string]treeAsk
	results   map[string]treeResult
	decisions map[refs.Target]model.Decision
	plans     map[refs.Target]*model.IssueTreePlan
	positions map[refs.Target]int
	// placements is the placement_seq of each node of the page that holds one at the cut
	// (readPlacementSeqs).
	placements map[refs.Target]int64
	// owed is each document of the issue that owes its block index (queueOwedDocuments).
	owed []model.OwedDocument
	// checks is the number of checks that serve each requirement of the page (queueRequirementChecks),
	// titles the newest title each session of the page stamped (delivery.ResolveSessionTitles), and
	// anchors the anchor of each comment of the page that holds one (queueCommentAnchors), by id.
	checks  map[string]int
	titles  map[string]string
	anchors map[string]*model.IssueTreeAnchor

	// page is the nodes of the answer from the read's cursor on: a slice of its walk's order, its
	// first sliced nodes, with the context nodes that slice places, at most pageSize nodes in all
	// but for a slice of one node, and on the last page the context no node of the walk names; next
	// is the node the next page starts at, where the page is not the last; after is how many nodes
	// the read holds after the page; placed is, for each context node of the page, the index in
	// the slice of the first node whose tuple names it, sliced for one the last page places; and
	// inAnswer is every node of the whole answer, which a tuple's far end may name (window). Only
	// the page's nodes, and the parents they stand under, read their members (readMembers,
	// documentPositions).
	page     []refs.Target
	next     *refs.Target
	after    int
	sliced   int
	placed   []int
	inAnswer map[refs.Target]bool

	// before is the position a comments_before read's page of comments starts after, nil for every
	// other read; skeleton is what the read with no view walks (readSkeleton), nil for a view;
	// listed is each document of kind doc the issue read lists, which the read with no view answers
	// as its document_nodes, each with its count of blocks (countDocuments); and counted is the
	// count of blocks of each document documentPositions read the blocks of.
	before   *refs.CommentPosition
	skeleton *docs.Skeleton
	listed   []model.IssueTreeDocumentNode
	counted  map[string]int
}

// treeAsk is what an ask's row says that its node does not: its question, its state, and the rest
// of its wording, its options, urgency and multiple.
type treeAsk struct {
	question, state string
	wording         model.IssueTreeAsk
}

// treeResult is what a result's check_results row and its retraction hold for the tree read: who
// recorded it and when, and its retraction where one stands.
type treeResult struct {
	actor     model.Actor
	createdAt time.Time
	retracted *model.CheckResultRetracted
}

// root is the issue read's own node.
func (b *issueTreeBuilder) root() refs.Target {
	return refs.Target{Kind: graphstore.KindIssue, ID: b.key}
}

// build reads everything of the answer the store holds, in the read's transaction: what places
// each node of the whole answer and says whether the reader may see it, then the page of the
// answer from cursor on (window), then the members of the page's nodes alone (readMembers). The
// read with no view, and the read with kind, walks the skeleton of the tree (readSkeleton,
// docs.WalkIssueSkeleton), and a view the whole tree. A read of the head (version 0) of an issue
// the head projection holds whole (issue_head_marks) reads the issue's rows of it in ranges of its
// key (refs.HeadRows), the skeleton's alone for the read with no view, and walks them
// (docs.WalkIssueHead); a read at a version, and a read of an issue no mark names, walks the
// tuples (docs.WalkIssueTree). Every read reads the comments of the issue's threads on text
// (queueTextComments), and adds to the walk's those no person wrote in (placeTextComments).
func (b *issueTreeBuilder) build(version int, cursor string) error {
	cut, err := issueTreeCut(b.ctx, b.q, b.key, b.view, version)
	if err != nil {
		return err
	}
	b.cut = cut
	var skeleton *refs.HeadSkeleton
	if b.view == "" {
		documents, err := b.readSkeleton()
		if err != nil {
			return err
		}
		skeleton = &refs.HeadSkeleton{Documents: documents, Comments: b.skeleton.Comments.IDs}
	} else if err := b.readViewTextComments(); err != nil {
		return err
	}
	if version == 0 {
		if b.heads, _, err = refs.HeadRows(b.ctx, b.q, b.who, b.key, skeleton); err != nil {
			return err
		}
	}
	switch {
	case b.skeleton != nil:
		b.tree, err = docs.WalkIssueSkeleton(b.ctx, b.q, b.key, cut, b.heads, *b.skeleton)
	case b.heads != nil:
		b.tree, err = docs.WalkIssueHead(b.ctx, b.q, b.key, cut, b.heads)
	default:
		b.tree, err = docs.WalkIssueTree(b.ctx, b.q, b.key, cut)
	}
	if err != nil {
		return err
	}
	b.enterWalk()
	if b.descend, err = refs.IssueDescendants(b.ctx, b.q, b.who, b.key); err != nil {
		return err
	}
	if err := b.readOthers(); err != nil {
		return err
	}
	if err := b.placeTextComments(); err != nil {
		return err
	}
	if err := b.readFacts(b.candidates()); err != nil {
		return err
	}
	if err := b.placeOthers(); err != nil {
		return err
	}
	b.keep()
	if err := b.readContext(); err != nil {
		return err
	}
	if err := b.window(cursor); err != nil {
		return err
	}
	return b.readMembers()
}

// enterWalk enters each node the walk took (b.tree) as a candidate of the answer, in the walk's
// order, with its place, its type, its rank and when it was made, and keeps what the walk named
// of the comments it hung under the issue and the issues above.
func (b *issueTreeBuilder) enterWalk() {
	b.entries = map[refs.Target]*treeEntry{}
	for index, node := range b.tree.Nodes {
		target := refs.Target{Kind: node.Kind, ID: node.ID}
		b.entries[target] = &treeEntry{
			target: target, walk: index, parent: node.Parent, typ: node.Type,
			rank: node.Rank, ranked: node.Ranked, madeAt: node.MadeAt, above: node.Above,
		}
		b.walked = append(b.walked, target)
	}
	b.named = make(map[refs.Target]refs.Target, len(b.tree.Named))
	maps.Copy(b.named, b.tree.Named)
	b.ancestors = b.tree.Ancestors
}

// placementCutSQL bounds the tuples placementSeqsStatement reads to the cut: at a seq up to $3 and,
// where $4 is not null, at a time up to $4 (refs.TreeCut's bounds).
const placementCutSQL = `t.seq <= $3 and ($4::timestamptz is null or t.at <= $4)`

// placementSeqsStatement reads, for each node the arrays $1 and $2 name by its kind and its id, its
// placement_seq at the cut (placementCutSQL, R8): the seq of its current part_of, read as a current
// relation is read (graphstore.CurrentTupleRead: the newest tuple to each far end, kept while it
// asserts), the newest of those, else, for a child issue that holds none, the seq of its current
// child_of (childOfPlacementSQL). A node that holds neither answers no row, and neither does the
// read's own issue node, the node of the issue $5. A node's kind is read as the store keys it
// (refs.StoredKindSQL), a finding of a lens as the store's block.
var placementSeqsStatement = `
	select placed.kind, placed.id, placed.seq from (
		select p.kind, p.id, coalesce((
			select held.seq from (
				` + graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_kind, t.to_id, t.seq, t.asserted`,
	`t.from_kind = `+refs.StoredKindSQL("p.kind")+` and t.from_id = p.id and t.kind = '`+graphstore.TuplePartOf+`'
				  and t.to_kind <> '`+graphstore.KindValue+`' and `+placementCutSQL) + `
			) held
			where held.asserted
			order by held.seq desc limit 1
		), ` + childOfPlacementSQL("p.kind", "p.id", placementCutSQL) + `) as seq
		from unnest($1::text[], $2::text[]) as p (kind, id)
		where not (p.kind = '` + graphstore.KindIssue + `' and p.id = $5::text)
	) placed
	where placed.seq is not null`

// readPlacementSeqs reads the placement_seq at cut of each of targets but the node of the issue
// key, the read's own (placementSeqsStatement), in one statement: the If-Match a move of the node
// names.
func readPlacementSeqs(ctx context.Context, q pgx.Tx, targets []refs.Target, cut refs.TreeCut, key string) (map[refs.Target]int64, error) {
	placements := make(map[refs.Target]int64, len(targets))
	if len(targets) == 0 {
		return placements, nil
	}
	kinds, ids := make([]string, len(targets)), make([]string, len(targets))
	for index, target := range targets {
		kinds[index], ids[index] = target.Kind, target.ID
	}
	seq, at := treeCutBounds(cut)
	rows, err := q.Query(ctx, placementSeqsStatement, kinds, ids, seq, at, key)
	if err != nil {
		return nil, fmt.Errorf("read the placement of %d nodes: %w", len(targets), err)
	}
	defer rows.Close()
	for rows.Next() {
		var target refs.Target
		var placed int64
		if err := rows.Scan(&target.Kind, &target.ID, &placed); err != nil {
			return nil, fmt.Errorf("read the placement of a node: %w", err)
		}
		placements[target] = placed
	}
	return placements, rows.Err()
}

// treeCutBounds are the highest seq and the latest time a statement of the tree read's own takes a
// tuple at, as refs.TreeCut's bounds are: a version's record reads up to its seq, the head up to
// the newest, and a dated cut up to its time whatever the seq, the time nil for a cut that is not
// dated.
func treeCutBounds(cut refs.TreeCut) (int64, *time.Time) {
	seq := cut.ReadSeq
	if cut.Dated || seq == 0 {
		seq = math.MaxInt64
	}
	if cut.Dated {
		return seq, &cut.At
	}
	return seq, nil
}

// issueTreeDocumentsStatement is each artifact of the issue $1, in the order the issue read lists
// them: its id, its ref_key and its slug; whether the issue read lists it as a document of kind
// doc (issueArtifactsListed); and whether the skeleton holds its blocks, which it holds for each
// band but the last (refs.DocumentBandSQL): the spec, the tree's own fragment, the Verification
// document and the plan.
var issueTreeDocumentsStatement = `
	select a.id::text, a.ref_key, a.slug, a.kind = 'doc' and ` + issueArtifactsListed("a") + `,
	       ` + refs.DocumentBandSQL("a") + ` <> '` + string(refs.RankBandOther) + `'
	from artifacts a
	where a.issue_key = $1
	order by a.created_at, a.id`

// readSkeleton reads what the skeleton of the read with no view holds (docs.Skeleton), and
// answers the ref_key of each document whose blocks it holds: those documents, and each document
// the read lists in document_nodes (listed), in one batch with the comments of the issue's threads
// on text (issueTreeDocumentsStatement, queueTextComments); and the comment page from the read's
// position on, of the conversation's comments (refs.ReadCommentPage) and the comments on text no
// person wrote in (mergeCommentPage), where a position that no older comment follows is 400
// INVALID_QUERY.
func (b *issueTreeBuilder) readSkeleton() ([]string, error) {
	skeleton := &docs.Skeleton{Documents: map[string]bool{}}
	var documents []string
	b.listed = []model.IssueTreeDocumentNode{}
	batch := &pgx.Batch{}
	batch.Queue(issueTreeDocumentsStatement, b.key).Query(func(rows pgx.Rows) error {
		for rows.Next() {
			var id, slug string
			var refKey *string
			var listed, held bool
			if err := rows.Scan(&id, &refKey, &slug, &listed, &held); err != nil {
				return fmt.Errorf("read a document of %s: %w", b.key, err)
			}
			if held && refKey != nil {
				documents = append(documents, *refKey)
				skeleton.Documents[*refKey] = true
			}
			if listed {
				b.listed = append(b.listed, model.IssueTreeDocumentNode{ArtifactID: id, Slug: slug})
			}
		}
		return rows.Err()
	})
	b.queueTextComments(batch)
	if err := b.q.SendBatch(b.ctx, batch).Close(); err != nil {
		return nil, fmt.Errorf("read the documents of %s: %w", b.key, err)
	}
	page, err := refs.ReadCommentPage(b.ctx, b.q, b.key, b.cut.At, b.before, issueTreeCommentPageSize)
	pastEnd := errors.Is(err, refs.ErrCommentPositionPastEnd)
	if err != nil && !pastEnd {
		return nil, err
	}
	if skeleton.Comments, err = b.mergeCommentPage(page, pastEnd, skeleton.Documents); err != nil {
		return nil, err
	}
	b.skeleton = skeleton
	return documents, nil
}

// issueTreeCut is the cut the tree read reads the store at: the head's where version is 0, and
// where it names a version, the cut the record of that version of the view's query node was read
// at (version_renders: its read_seq and its time). A version the query node does not have
// (artifact_versions) is 404 QUERY_VERSION_NOT_FOUND. A version it has that holds no read_seq,
// because its version_renders row holds none or it holds no such row - one written before the
// cutover, a deferred version before its pass, one whose row holds tree_mismatch or whose mark the
// repair closed - is 404 QUERY_VERSION_NOT_RECORDED, naming the version.
func issueTreeCut(ctx context.Context, q pgx.Tx, key, view string, version int) (refs.TreeCut, error) {
	if version == 0 {
		return refs.HeadCut(ctx, q)
	}
	var held bool
	var readSeq *int64
	var at *time.Time
	err := q.QueryRow(ctx, `
		select v.artifact_id is not null, r.read_seq, r.at
		from artifacts a
		join graph_nodes n on n.kind = $4 and n.id = a.ref_key and n.supertag = $5 and n.attributes->>$6 = $2
		left join artifact_versions v on v.artifact_id = a.id and v.number = $3
		left join version_renders r on r.artifact_id = a.id and r.number = $3
		where a.issue_key = $1`,
		key, view, version, graphstore.KindArtifact, graphstore.SupertagQuery, graphstore.AttributeQuery).Scan(&held, &readSeq, &at)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !held {
		return refs.TreeCut{}, errorf(http.StatusNotFound, "QUERY_VERSION_NOT_FOUND", "the %s query of %s holds no version %d", view, key, version)
	}
	if err != nil {
		return refs.TreeCut{}, fmt.Errorf("read version %d of the %s query of %s: %w", version, view, key, err)
	}
	if readSeq == nil {
		return refs.TreeCut{}, errorf(http.StatusNotFound, "QUERY_VERSION_NOT_RECORDED",
			"version %d of the %s query of %s was written before the record of the store revision it reads: no read of it at that revision is recorded",
			version, view, key)
	}
	cut := refs.TreeCut{ReadSeq: *readSeq}
	if at != nil {
		cut.At = *at
	}
	return cut, nil
}

// add adds a node the walk does not take to the answer's candidates, once.
func (b *issueTreeBuilder) add(target refs.Target, parent refs.Target, issue, role string) *treeEntry {
	if entry, held := b.entries[target]; held {
		return entry
	}
	entry := &treeEntry{target: target, walk: -1, order: len(b.extras), parent: parent, issue: issue, role: role}
	b.entries[target] = entry
	b.extras = append(b.extras, target)
	return entry
}

// treeIssueOf is the issue a node's address names - a document block's, `<KEY>/<slug>#<id>`, and an
// issue's own key - and "" for a node whose address names none.
func treeIssueOf(target refs.Target) string {
	switch target.Kind {
	case graphstore.KindIssue:
		return target.ID
	case refs.BlockNodeKind:
		if _, _, finding := graphstore.ParseFindingID(target.ID); finding {
			return ""
		}
		owner, _, cut := strings.Cut(target.ID, "/")
		if cut {
			return owner
		}
	}
	return ""
}

// readOthers adds the nodes of issues under the issue read that its answer holds: each child
// issue, placed where its part_of names a node of the issue and under the issue otherwise (R2);
// each node of another issue whose part_of names a node of the walk, a child's criterion under a
// comment of the issue among them, or a document the skeleton does not open or a block of one
// (docs' IssueTree.Hidden); each check and plan node of an issue under the issue whose serves
// names one of the issue's requirements, with the results under each such check, reached through
// serves (R3, R23), which the spec and the decisions views carry none of; and in the plan view
// every plan node of the whole tree's ordered read (refs.PlanOrder). Each is read-only, marked
// with its issue. The children of the walk's nodes are the rows the walk read them in (docs'
// IssueTree.Children), taken in the walk's order and each parent's in the order read.
func (b *issueTreeBuilder) readOthers() error {
	rows, err := b.q.Query(b.ctx, `select key from issues where parent_key = $1 order by key`, b.key)
	if err != nil {
		return fmt.Errorf("read the children of %s: %w", b.key, err)
	}
	children, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("read the children of %s: %w", b.key, err)
	}
	for _, child := range children {
		if slices.Contains(b.descend, child) {
			b.add(refs.Target{Kind: graphstore.KindIssue, ID: child}, b.root(), child, treeRoleTree)
		}
	}
	for _, parent := range slices.Concat(b.walked, b.tree.Hidden) {
		for _, child := range b.tree.Children[parent] {
			if _, walked := b.entries[child.Node]; walked && b.entries[child.Node].walk >= 0 {
				continue
			}
			owner := treeIssueOf(child.Node)
			if owner == "" || owner == b.key || !slices.Contains(b.descend, owner) {
				continue
			}
			entry := b.add(child.Node, child.Parent, owner, treeRoleTree)
			entry.parent, entry.typ, entry.rank, entry.ranked, entry.madeAt = child.Parent, child.Type, child.Rank, child.Ranked, child.MadeAt
		}
	}
	if b.view != graphstore.QuerySpec && b.view != graphstore.QueryDecisions {
		if err := b.readServing(); err != nil {
			return err
		}
	}
	if b.view == graphstore.QueryPlan {
		return b.readPlanOrder()
	}
	return nil
}

// readServing adds each check and plan node of an issue under the issue read whose current serves
// names one of the issue's requirements, read into each requirement over graph_tuples_as_of_in
// (servesInto), and each result under such a check, each marked with its issue. A check stands
// under the first requirement of the walk it serves (placeOthers), as R30 answers it, a plan node
// under the node its part_of names.
func (b *issueTreeBuilder) readServing() error {
	var requirements []string
	for _, target := range b.walked {
		if entry := b.entries[target]; entry.typ == requirementBlockType && target.Kind == refs.BlockNodeKind {
			requirements = append(requirements, target.ID)
		}
	}
	serving, err := servesInto(b.ctx, b.q, requirements, b.cut)
	if err != nil {
		return err
	}
	var checks []refs.Target
	for _, requirement := range requirements {
		for _, from := range serving[requirement] {
			owner := treeIssueOf(from)
			if owner == "" || owner == b.key || !slices.Contains(b.descend, owner) {
				continue
			}
			if _, held := b.entries[from]; held {
				continue
			}
			b.add(from, refs.Target{}, owner, treeRoleTree).served = refs.Target{Kind: refs.BlockNodeKind, ID: requirement}
			checks = append(checks, from)
		}
	}
	if len(checks) == 0 {
		return nil
	}
	results, err := refs.TreeChildren(b.ctx, b.q, checks, b.cut)
	if err != nil {
		return err
	}
	for _, child := range results {
		if child.Node.Kind != graphstore.KindResult {
			continue
		}
		entry := b.add(child.Node, child.Parent, b.entries[child.Parent].issue, treeRoleTree)
		entry.typ, entry.madeAt = graphstore.SupertagResult, child.MadeAt
	}
	return nil
}

// tuplesIntoStatement is, at the cut whose seq bound $2 is, each current tuple of kind $3 into the
// block nodes $1 names: of each relation the newest tuple at or below the bound, read into its far
// end through graph_tuples_as_of_in, kept while it asserts.
const tuplesIntoStatement = `
	select newest.to_id, newest.from_kind, newest.from_id
	from (
		select distinct on (t.to_id, t.from_kind, t.from_id) t.to_id, t.from_kind, t.from_id, t.asserted
		from graph_tuples t
		where t.to_kind = '` + refs.BlockNodeKind + `' and t.to_id = any($1) and t.kind = $3
		  and t.to_kind <> '` + graphstore.KindValue + `' and t.seq <= $2
		order by t.to_id, t.from_kind, t.from_id, t.seq desc
	) newest
	where newest.asserted
	order by newest.to_id, newest.from_kind, newest.from_id collate "C"`

// servesInto is each node whose current serves names one of the requirement blocks requirements,
// keyed by the requirement (tuplesInto): the coverage read walks serves from the requirement, so
// one check serving three requirements is one node under each of the three.
func servesInto(ctx context.Context, q queryer, requirements []string, cut refs.TreeCut) (map[string][]refs.Target, error) {
	return tuplesInto(ctx, q, servesTupleKind, requirements, cut)
}

// tuplesInto is each node whose current tuple of kind names one of the block nodes blocks, keyed
// by the block, read at cut's seq, the head's where it is a dated cut.
func tuplesInto(ctx context.Context, q queryer, kind string, blocks []string, cut refs.TreeCut) (map[string][]refs.Target, error) {
	into := map[string][]refs.Target{}
	if len(blocks) == 0 {
		return into, nil
	}
	seq, _ := treeCutBounds(cut)
	rows, err := q.Query(ctx, tuplesIntoStatement, blocks, seq, kind)
	if err != nil {
		return nil, fmt.Errorf("read the %s into %d blocks: %w", kind, len(blocks), err)
	}
	defer rows.Close()
	for rows.Next() {
		var block string
		var from refs.Target
		if err := rows.Scan(&block, &from.Kind, &from.ID); err != nil {
			return nil, fmt.Errorf("read the %s into a block: %w", kind, err)
		}
		into[block] = append(into[block], from)
	}
	return into, rows.Err()
}

// readPlanOrder reads the ordered read of the issue's whole tree (refs.PlanOrder) for the plan
// view: each node's position, needs and size, a plan node's skills and docs, every plan node of an
// issue under the issue added beside the issue's own, and each issue of the tree answered as its
// two halves.
func (b *issueTreeBuilder) readPlanOrder() error {
	order, err := refs.PlanOrder(b.ctx, b.q, b.who, b.key)
	if err != nil {
		return err
	}
	b.plans = map[refs.Target]*model.IssueTreePlan{}
	cycles := map[refs.OrderNode]bool{}
	for _, cycle := range order.Cycles {
		for _, node := range cycle {
			cycles[node.OrderNode] = true
		}
	}
	place := func(node refs.OrderedNode, position *int) {
		plan := &model.IssueTreePlan{Position: position, Needs: []model.IssueTreeHalfEnd{}, Cycle: cycles[node.OrderNode]}
		for _, need := range node.Needs {
			plan.Needs = append(plan.Needs, model.IssueTreeHalfEnd{Kind: need.Kind, ID: need.ID, Half: need.Half})
		}
		if node.Size != "" {
			size := node.Size
			plan.Size = &size
		}
		if node.Kind == graphstore.KindIssue {
			target := refs.Target{Kind: graphstore.KindIssue, ID: node.ID}
			halves := b.plans[target]
			if halves == nil {
				halves = &model.IssueTreePlan{Needs: []model.IssueTreeHalfEnd{}}
				b.plans[target] = halves
			}
			if node.Half == refs.HalfDone {
				halves.Done = plan
			} else {
				halves.Start = plan
			}
			if node.ID != b.key {
				b.add(target, refs.Target{}, node.ID, treeRoleTree)
			}
			return
		}
		target := refs.Target{Kind: node.Kind, ID: node.ID}
		plan.Skills, plan.Docs = node.Skills, node.Docs
		b.plans[target] = plan
		if node.Issue != b.key {
			b.add(target, refs.Target{}, node.Issue, treeRoleTree)
		}
	}
	for index, node := range order.Order {
		position := index
		place(node, &position)
	}
	for _, cycle := range order.Cycles {
		for _, node := range cycle {
			place(node, nil)
		}
	}
	for _, node := range order.Overtaken {
		place(node, nil)
	}
	return nil
}

// candidates is every node the answer may hold before the context: the walk's and the others'.
func (b *issueTreeBuilder) candidates() []refs.Target {
	return slices.Concat(b.walked, b.extras)
}

// readFacts reads what each of targets states at the read's cut (refs.TreeFactsOf), each node's
// row, each block's document_blocks row, and each node as the registry's loader of its kind
// answers it for the reader (refs.ExcerptedNodes), which is the one test of whether the reader may
// see it, but a document or a version of the walk's or the others', which no answer holds
// (unloaded), for each target it has not read yet (read). A node of the walk whose row the walk read
// (docs' TreeNode.Row) takes its row from there, and every other node's is read
// (refs.TreeNodeRows): the issue's own, its conversation's, the others' and the context's. A node
// of the walk's conversation takes what its loader answered from the walk (docs' IssueTree's
// Conversation): the walk loaded it as the empty actor, who sees only what every reader sees, and
// a loader's answer differs by reader only in which nodes it leaves out, so the reader's loader
// answers it the same. A head read takes each of these from the node's head row where the issue's
// rows hold one (refs.HeadRow): its facts, its row, and for a document block what the block loader
// answers of it for the reader and its document_blocks row; any other kind is still read through
// its loader.
func (b *issueTreeBuilder) readFacts(targets []refs.Target) error {
	if b.facts == nil {
		size := len(targets)
		b.facts, b.loaded, b.rows, b.blocks = make(map[refs.Target]refs.TreeFacts, size), make(map[refs.Target]refs.ExcerptedNode, size), make(map[refs.Target]refs.TreeNodeRow, size), make(map[string]refs.BlockRow, size)
		b.read = make(map[refs.Target]bool, size)
	}
	var unread, fromStore []refs.Target
	for _, target := range targets {
		if b.read[target] {
			continue
		}
		b.read[target] = true
		unread = append(unread, target)
		if _, held := b.headRow(target); !held {
			fromStore = append(fromStore, target)
		}
	}
	if len(unread) == 0 {
		return nil
	}
	facts, err := refs.TreeFactsOf(b.ctx, b.q, fromStore, b.cut)
	if err != nil {
		return err
	}
	for target, held := range facts {
		b.facts[target] = held
	}
	var unrowed []refs.Target
	for _, target := range unread {
		if entry, held := b.entries[target]; held && entry.walk >= 0 && b.tree.Nodes[entry.walk].Row {
			node := b.tree.Nodes[entry.walk]
			b.rows[target] = refs.TreeNodeRow{Supertag: node.Supertag, MadeAt: node.MadeAt}
			continue
		}
		if head, held := b.headRow(target); held {
			b.rows[target] = head.Row()
			continue
		}
		unrowed = append(unrowed, target)
	}
	rows, err := refs.TreeNodeRows(b.ctx, b.q, unrowed)
	if err != nil {
		return err
	}
	for _, target := range unrowed {
		b.rows[target] = rows[target]
	}
	// A block's node and its document_blocks row are read in one statement
	// (refs.BlockNodesAndRows), the node where the reader may see it and the row whether or not;
	// every other node through its kind's loader, but a document or a version the walk or the
	// others took (unloaded), which no answer holds.
	loaderTargets := make([]refs.Target, 0, len(unread))
	var blocks []string
	for _, target := range unread {
		kind := loaderKind(target)
		if head, held := b.headRow(target); held {
			if head.Facts.Values != nil || head.Facts.Relations != nil {
				b.facts[target] = head.Facts
			}
			if kind == refs.BlockNodeKind {
				node, seen, documented, err := head.BlockNode()
				if err != nil {
					return err
				}
				if documented {
					b.blocks[target.ID] = *head.Block
				}
				if seen && documented {
					b.loaded[target] = node
				}
				continue
			}
		} else if target.Kind == refs.BlockNodeKind {
			blocks = append(blocks, target.ID)
		}
		if node, conversed := b.tree.Conversation[target]; conversed {
			b.loaded[target] = node
			continue
		}
		if kind != refs.BlockNodeKind && refs.ResolvesNodeKind(kind) && !b.unloaded(target) {
			loaderTargets = append(loaderTargets, refs.Target{Kind: kind, ID: target.ID})
		}
	}
	loaded, err := refs.ExcerptedNodes(b.ctx, b.q, b.who, loaderTargets)
	if err != nil {
		return err
	}
	blockNodes, blockRows, err := refs.BlockNodesAndRows(b.ctx, b.q, b.who, blocks)
	if err != nil {
		return fmt.Errorf("read the blocks of the tree of %s: %w", b.key, err)
	}
	for _, target := range unread {
		if _, held := b.headRow(target); held && loaderKind(target) == refs.BlockNodeKind {
			continue
		}
		node, held := loaded[refs.Target{Kind: loaderKind(target), ID: target.ID}]
		if loaderKind(target) == refs.BlockNodeKind {
			node, held = blockNodes[target.ID]
		}
		if held {
			b.loaded[target] = node
		}
	}
	maps.Copy(b.blocks, blockRows)
	return nil
}

// headRow is target's head row where the read is a head read of a marked issue whose rows hold
// one (refs.HeadRows), and false otherwise.
func (b *issueTreeBuilder) headRow(target refs.Target) (*refs.HeadRow, bool) {
	if b.heads == nil {
		return nil, false
	}
	head, held := b.heads.Rows[target]
	return head, held
}

// unloaded is whether target is a node of the walk or of the others that no tree read answers and
// that is no parent the answer names by its loaded node: a document or a version (excluded), which
// keep never keeps and keep's effective parents pass over, and which parentOf names from its entry.
// Its loader's answer would be read for nothing: for an issue of the most comments, the one
// statement of its documents, 0.07 ms of the read's 5.35.
func (b *issueTreeBuilder) unloaded(target refs.Target) bool {
	entry, held := b.entries[target]
	return held && entry.role != treeRoleContext &&
		(target.Kind == graphstore.KindArtifact || target.Kind == graphstore.KindVersion)
}

// loaderKind is the kind the registry resolves the node target names by: a finding of a lens,
// which the store keys as a block, is a lens_block (graphstore.AddressKind).
func loaderKind(target refs.Target) string {
	return graphstore.AddressKind(target.Kind, target.ID)
}

// placeOthers gives each node the walk does not take what its facts and its row say: a child
// issue its placement, where its current part_of names a node of the walk or a block of a
// document the skeleton does not open (hiddenBlock), a check reached through serves the
// requirement it serves (served), and every such node its type, its rank and when it was made,
// where nothing read so far gave them.
func (b *issueTreeBuilder) placeOthers() error {
	for _, target := range b.extras {
		entry := b.entries[target]
		held := b.facts[target]
		if target.Kind == graphstore.KindIssue && entry.role == treeRoleTree {
			for _, relation := range held.Relations {
				if relation.Kind != graphstore.TuplePartOf {
					continue
				}
				if parent, walked := b.entries[relation.To]; walked && parent.walk >= 0 || b.hiddenBlock(relation.To) {
					entry.parent = relation.To
				}
			}
		}
		if entry.typ == "" {
			opening, _, _ := strings.Cut(held.Values[graphstore.AttributeText], "\n")
			entry.typ = refs.TreeNodeType(target.Kind, b.rows[target].Supertag, opening)
		}
		if rank, ranked := held.Values[refs.AttributeRank]; ranked && !entry.ranked {
			entry.rank, entry.ranked = rank, true
		}
		if entry.madeAt.IsZero() {
			entry.madeAt = b.rows[target].MadeAt
		}
		if entry.parent == (refs.Target{}) && entry.typ == graphstore.BlockTypeCheck && entry.served != (refs.Target{}) {
			entry.parent = entry.served
		}
		if entry.parent == (refs.Target{}) && entry.role != treeRoleContext {
			for _, relation := range held.Relations {
				if relation.Kind == graphstore.TuplePartOf {
					entry.parent = relation.To
				}
			}
			if entry.parent == (refs.Target{}) && entry.issue != "" && target.Kind != graphstore.KindIssue {
				entry.parent = refs.Target{Kind: graphstore.KindIssue, ID: entry.issue}
			}
		}
	}
	return nil
}

// visible reports whether the reader may see the node: the registry's loader of its kind answered
// it (refs.ExcerptedNodes binds the kind's predicate), or its kind is one no read of the graph
// resolves - a result, a document version - whose nodes are project-visible.
func (b *issueTreeBuilder) visible(target refs.Target) bool {
	if !refs.ResolvesNodeKind(loaderKind(target)) {
		return true
	}
	_, held := b.loaded[target]
	return held
}

// excluded reports whether the node is one no query of the tree returns (R2, R16): a document,
// the issue's tree among them, and a document version, which no view draws; a block of a role
// document - the spec, the Verification document, the plan - that is no typed block, its prose,
// which stays a node of the issue that its document's versions render; and in the skeleton of the
// read with no view, a block of one of the issue's other documents (R8).
func (b *issueTreeBuilder) excluded(target refs.Target) bool {
	switch target.Kind {
	case graphstore.KindArtifact, graphstore.KindVersion:
		return true
	case refs.BlockNodeKind:
		row, held := b.blocks[target.ID]
		if !held {
			return false
		}
		switch row.Band {
		case refs.RankBandSpec, refs.RankBandVerification, refs.RankBandPlan:
			return !pmdoc.IsTypedBlock(b.entries[target].typ)
		case refs.RankBandOther:
			return b.skeleton != nil && treeIssueOf(target) == b.key
		}
	}
	return false
}

// hiddenBlock is whether target is a block of a document of the issue that the skeleton of the
// read with no view does not open, by its address: a block of one of the issue's documents, whose
// ref_key and `#` begin its address (refs.BlockAddress), other than those the skeleton holds the
// blocks of (docs.Skeleton's Documents). The read takes no such block, and a node that stands
// under one stands under the issue (keep), as under any node it leaves out (R8).
func (b *issueTreeBuilder) hiddenBlock(target refs.Target) bool {
	document, _, cut := strings.Cut(target.ID, "#")
	return b.skeleton != nil && cut && target.Kind == refs.BlockNodeKind && strings.HasPrefix(document, b.key+"/") &&
		!b.skeleton.Documents[document]
}

// keep decides which candidates the answer holds - each the reader may see and that no query of
// the tree leaves out - and under which node each stands: its parent where the answer holds it,
// else the nearest node above it the answer holds, the issue at worst - the issue too for a node
// under a block of a document the skeleton does not open (hiddenBlock) - or for a node whose
// parent is a node of an issue above, that node. The read answers nothing of a node it leaves
// out, so the answer is the one it would be if that node did not exist. A comment on text no
// person wrote in stands so under the nearest node the answer holds at or above the node it is
// on, as a child of that node's own. A comments_before read with no view and no kind then keeps
// the comments of its page and the nodes under them alone (keepCommentsAlone); one with a view or
// kinds keeps them once its filter has run (ordered).
func (b *issueTreeBuilder) keep() {
	b.kept = make(map[refs.Target]bool, len(b.entries))
	for target, entry := range b.entries {
		if entry.role == treeRoleContext {
			continue
		}
		b.kept[target] = target == b.root() || b.visible(target) && !b.excluded(target)
	}
	b.effective = make(map[refs.Target]refs.Target, len(b.entries))
	for target, kept := range b.kept {
		if !kept || target == b.root() {
			continue
		}
		parent := b.entries[target].parent
		for steps := 0; steps <= len(b.entries); steps++ {
			if parent == (refs.Target{}) || parent == b.root() || b.kept[parent] {
				break
			}
			above, held := b.entries[parent]
			if !held {
				if b.hiddenBlock(parent) {
					parent = b.root()
				}
				break
			}
			if above.parent == (refs.Target{}) {
				parent = b.root()
				break
			}
			parent = above.parent
		}
		if parent == (refs.Target{}) {
			parent = b.root()
		}
		b.effective[target] = parent
	}
	for target, entry := range b.entries {
		if parent, standing := b.effective[target]; standing && entry.onText && target.Kind == graphstore.KindComment {
			entry.parent = parent
		}
	}
	if b.before != nil && b.view == "" && len(b.kinds) == 0 {
		b.keepCommentsAlone()
	}
}

// keepCommentsAlone leaves out of a comments_before read every node but the comments of its page
// (the walk's, docs.WalkIssueSkeleton) and the nodes under them, the issue's own node among those
// it leaves out (R8). Each node it keeps stands under the node keep found for it, so a comment whose
// parent it leaves out names that parent (node) and stands at the top of the answer (ordered).
func (b *issueTreeBuilder) keepCommentsAlone() {
	for target, kept := range b.kept {
		if !kept {
			continue
		}
		at := target
		for steps := 0; at.Kind != graphstore.KindComment && steps <= len(b.entries); steps++ {
			entry, held := b.entries[at]
			if !held || at == b.root() {
				at = refs.Target{}
				break
			}
			at = entry.parent
		}
		if at.Kind != graphstore.KindComment {
			b.kept[target] = false
		}
	}
}

// readContext adds the context nodes the answer carries, and reads what they state and whether the
// reader may see them. Every read carries each decision's decided_by person or session. The read
// with no view carries each catalogue node a has_kind or constrained_by of a node it holds names, a
// lens_kind or a constraint the tree does not hold, one level out, and each interaction a
// changed_by_run of a node it holds names, through the reader's predicate, and so does the read
// with kind, which answers those of the nodes it returns (kindOrder). A view carries its fixed
// context (viewContext), and the spec and the verification views the ties of their nodes
// (readTies). It reads, once, what the view's query takes and the requirements the verification
// view carries, which inView answers the view's nodes by, and so the comments on their text, a
// view's comments_before read among them. A comments_before read with no view carries none.
func (b *issueTreeBuilder) readContext() error {
	if b.before != nil && b.view == "" {
		return nil
	}
	var wanted []refs.Target
	for _, target := range b.candidates() {
		if !b.kept[target] {
			continue
		}
		for _, relation := range b.facts[target].Relations {
			if _, held := b.entries[relation.To]; held {
				continue
			}
			switch {
			case relation.Kind == graphstore.TupleDecidedBy:
			case b.view != "":
				continue
			case relation.Kind == graphstore.TupleHasKind && relation.To.Kind == graphstore.KindLensKind,
				relation.Kind == graphstore.TupleConstrainedBy && relation.To.Kind == graphstore.KindConstraint,
				relation.Kind == graphstore.TupleChangedByRun && relation.To.Kind == graphstore.KindInteraction:
			default:
				continue
			}
			if !slices.Contains(wanted, relation.To) {
				wanted = append(wanted, relation.To)
			}
		}
	}
	if b.view != "" {
		b.taken = b.queryTakes(b.view)
		if b.view == graphstore.QueryVerification {
			b.requirements = b.verificationRequirements()
		}
		wanted = append(wanted, b.viewContext()...)
	}
	if err := b.addContext(wanted); err != nil {
		return err
	}
	if b.view == graphstore.QuerySpec || b.view == graphstore.QueryVerification {
		tied, err := b.readTies()
		if err != nil {
			return err
		}
		if err := b.addContext(tied); err != nil {
			return err
		}
	}
	return b.placeContext()
}

// addContext reads what each of wanted states and whether the reader may see it, and adds each
// one the reader may see, and that the answer holds as no node of its walk or of the others, as a
// context node, once.
func (b *issueTreeBuilder) addContext(wanted []refs.Target) error {
	if err := b.readFacts(wanted); err != nil {
		return err
	}
	for _, target := range wanted {
		if !b.visible(target) {
			continue
		}
		if entry, held := b.entries[target]; held {
			if entry.role != treeRoleContext {
				continue
			}
		} else {
			entry := &treeEntry{target: target, walk: -1, issue: treeIssueOf(target), role: treeRoleContext}
			if entry.issue == b.key {
				entry.issue = ""
			}
			b.entries[target] = entry
		}
		b.kept[target] = true
		if !b.inContext[target] {
			if b.inContext == nil {
				b.inContext = map[refs.Target]bool{}
			}
			b.inContext[target] = true
			b.context = append(b.context, target)
		}
	}
	return nil
}

// readTies is what the spec and the verification views carry as context for the ties of the nodes
// they answer - each node their query takes that the reader may see, and their fixed context: each
// constraint a constrained_by of one of them names, and each decision, and each ask a person
// answered, whose current shaped tuple names one of them, read into it at the read's cut
// (tuplesInto). A tie's node the walk or the read of the others took - a decision or an ask of the
// issue, a constraint written on it - is marked for inView to answer as context (tied), and the
// rest are answered for addContext to add. A node a tie brings in brings in no tie of its own.
func (b *issueTreeBuilder) readTies() ([]refs.Target, error) {
	var viewed []refs.Target
	for _, target := range b.candidates() {
		if _, in := b.inView(target); in && b.kept[target] {
			viewed = append(viewed, target)
		}
	}
	viewed = append(viewed, b.context...)
	var blocks []string
	var ties []refs.Target
	for _, target := range viewed {
		if target.Kind == refs.BlockNodeKind {
			blocks = append(blocks, target.ID)
		}
		for _, relation := range b.facts[target].Relations {
			if relation.Kind == graphstore.TupleConstrainedBy && relation.To.Kind == graphstore.KindConstraint && !slices.Contains(ties, relation.To) {
				ties = append(ties, relation.To)
			}
		}
	}
	shaping, err := tuplesInto(b.ctx, b.q, refs.TupleShaped, blocks, b.cut)
	if err != nil {
		return nil, err
	}
	var shapers []refs.Target
	for _, block := range blocks {
		for _, from := range shaping[block] {
			if !slices.Contains(shapers, from) {
				shapers = append(shapers, from)
			}
		}
	}
	if err := b.readFacts(shapers); err != nil {
		return nil, err
	}
	for _, shaper := range shapers {
		decision := shaper.Kind == refs.BlockNodeKind && b.rows[shaper].Supertag == refs.DecisionSupertag
		answered := shaper.Kind == graphstore.KindAsk && b.facts[shaper].Values[graphstore.TupleAnswerState] == asks.StateAnswered
		if decision || answered {
			ties = append(ties, shaper)
		}
	}
	b.tied = map[refs.Target]bool{}
	var wanted []refs.Target
	for _, tie := range ties {
		if entry, held := b.entries[tie]; held && entry.role != treeRoleContext {
			b.tied[tie] = true
			continue
		}
		wanted = append(wanted, tie)
	}
	return wanted, nil
}

// viewContext is the fixed context of the view beyond what its query takes: on a child issue's
// spec view, each requirement of an issue above that one of the child's checks serves and the
// child's own design blocks, the blocks of its tree's own fragment that are no typed block; on
// the verification view, every requirement node of the issue, served or not, so a requirement no
// check serves is in the answer, and each other requirement a check of the answer serves - one of
// an issue above on a child, one of an issue under the issue read on a parent. The walk holds the
// issue's own requirements and the design blocks, which ordered marks as context (inView); this
// adds the requirements no walk holds, each once.
func (b *issueTreeBuilder) viewContext() []refs.Target {
	var wanted []refs.Target
	switch b.view {
	case graphstore.QuerySpec:
		if len(b.ancestors) == 0 {
			return nil
		}
		for _, target := range b.walked {
			entry := b.entries[target]
			if entry.typ != graphstore.BlockTypeCheck {
				continue
			}
			for _, relation := range b.facts[target].Relations {
				if relation.Kind == servesTupleKind && slices.Contains(b.ancestors, treeIssueOf(relation.To)) && !slices.Contains(wanted, relation.To) {
					wanted = append(wanted, relation.To)
				}
			}
		}
	case graphstore.QueryVerification:
		for _, target := range b.candidates() {
			entry := b.entries[target]
			if entry.typ != graphstore.BlockTypeCheck || !b.kept[target] {
				continue
			}
			for _, relation := range b.facts[target].Relations {
				if relation.Kind == servesTupleKind && !slices.Contains(wanted, relation.To) {
					if held, walked := b.entries[relation.To]; walked && held.walk >= 0 {
						continue
					}
					wanted = append(wanted, relation.To)
				}
			}
		}
	}
	return wanted
}

// placeContext gives each context node its type, its rank and its time, and a context node of a
// view its own parent, which every node a view answers carries; a catalogue node, an interaction
// and a person stand under none.
func (b *issueTreeBuilder) placeContext() error {
	for _, target := range b.context {
		entry := b.entries[target]
		held := b.facts[target]
		if entry.typ == "" {
			opening, _, _ := strings.Cut(held.Values[graphstore.AttributeText], "\n")
			entry.typ = refs.TreeNodeType(target.Kind, b.rows[target].Supertag, opening)
		}
		if rank, ranked := held.Values[refs.AttributeRank]; ranked {
			entry.rank, entry.ranked = rank, true
		}
		entry.madeAt = b.rows[target].MadeAt
		if b.view != "" && entry.parent == (refs.Target{}) && target.Kind == refs.BlockNodeKind {
			for _, relation := range held.Relations {
				if relation.Kind == graphstore.TuplePartOf {
					entry.parent = relation.To
				}
			}
		}
		if entry.parent != (refs.Target{}) {
			b.effective[target] = entry.parent
		}
	}
	return nil
}

// servesTupleKind is the relation a check and a plan node serve a requirement by, and a
// requirement a criterion.
const servesTupleKind = "serves"

// requirementBlockType is the typed block a requirement is.
const requirementBlockType = "requirement"

// readMembers reads, for every node of the page (window), what its own members need from the rows
// that hold them: each issue's own members, the texts of the items and their blocks, the asks'
// questions, states and wording, the results' rows and retractions, the anchors of the comments,
// the checks that serve each requirement, the titles of the sessions, and the decisions' members
// through their one builder (decisionread.Decisions); the texts, too, of each parent a node of the
// page stands under that the answer holds, whose first line the node names (parentOf), and what
// the store and the loaders answer of each such parent the answer does not hold; and, in the batch
// of the texts, the documents of the issue that owe their block index (queueOwedDocuments). A node
// of the answer off the page reads none of these, since no member of a node off the page reaches
// the page.
func (b *issueTreeBuilder) readMembers() error {
	b.issues = map[string]*model.IssueTreeIssue{}
	root, err := readIssueTreeIssue(b.ctx, b.q, b.key, &b.tree.ViewOf)
	if err != nil {
		return err
	}
	b.issues[b.key] = root
	if b.placements, err = readPlacementSeqs(b.ctx, b.q, b.page, b.cut, b.key); err != nil {
		return err
	}
	for _, target := range b.page {
		if target.Kind != graphstore.KindIssue || target.ID == b.key {
			continue
		}
		state, err := readIssueTreeIssue(b.ctx, b.q, target.ID, nil)
		if err != nil {
			return err
		}
		b.issues[target.ID] = state
	}
	// A parent the answer does not hold - a node of an issue above that a node of the walk's
	// second part stands under, a node of an issue under the one read whose node the answer does
	// not hold, a parent the read names for a comment it hangs under the issue (named) - is named
	// by what the store and the registry's loader answer of it, and the last also by its own texts;
	// a parent the answer holds by its own texts, read beside the page's, and so is each node of the
	// read's tree a comments_before read leaves out that a comment of its page stands under.
	var outside []refs.Target
	named := slices.Clone(b.page)
	isNamed := make(map[refs.Target]bool, len(b.page))
	for _, target := range b.page {
		isNamed[target] = true
	}
	for _, target := range b.page {
		parent, standing := b.effective[target]
		if !standing || parent == (refs.Target{}) {
			continue
		}
		if skeleton, held := b.named[target]; held && !b.inAnswer[skeleton] {
			if !slices.Contains(outside, skeleton) {
				outside = append(outside, skeleton)
			}
			if !isNamed[skeleton] {
				isNamed[skeleton] = true
				named = append(named, skeleton)
			}
		}
		if _, held := b.entries[parent]; !held {
			if !slices.Contains(outside, parent) {
				outside = append(outside, parent)
			}
			continue
		}
		if (b.inAnswer[parent] || b.before != nil) && !isNamed[parent] {
			isNamed[parent] = true
			named = append(named, parent)
		}
	}
	if err := b.readFacts(outside); err != nil {
		return err
	}
	b.texts = map[refs.Target]string{}
	b.asks = map[string]treeAsk{}
	b.results = map[string]treeResult{}
	var comments, messages, asks, results []string
	var itemBlocks []refs.Target
	for _, target := range named {
		// A comment's body and a comment block's text at its newest revision, which BlockTexts
		// leaves out where that revision holds none, are its head row's where the read holds one.
		if head, held := b.headRow(target); held && head.Item != nil &&
			(target.Kind == graphstore.KindComment || target.Kind == refs.ItemComment.BlockKind()) {
			if head.Item.Text != nil {
				b.texts[target] = *head.Item.Text
			}
			continue
		}
		switch target.Kind {
		case graphstore.KindComment:
			comments = append(comments, target.ID)
		case graphstore.KindMessage:
			messages = append(messages, target.ID)
		case graphstore.KindAsk:
			asks = append(asks, target.ID)
		case graphstore.KindResult:
			results = append(results, target.ID)
		case refs.ItemAsk.BlockKind(), refs.ItemComment.BlockKind(), refs.ItemMessage.BlockKind():
			itemBlocks = append(itemBlocks, target)
		}
	}
	batch := &pgx.Batch{}
	queueTexts := func(kind, statement string, ids []string) {
		if len(ids) == 0 {
			return
		}
		batch.Queue(statement, ids).Query(func(rows pgx.Rows) error {
			for rows.Next() {
				var id, body string
				if err := rows.Scan(&id, &body); err != nil {
					return err
				}
				b.texts[refs.Target{Kind: kind, ID: id}] = body
			}
			return rows.Err()
		})
	}
	queueTexts(graphstore.KindComment, `select id::text, body from comments where id = any($1::uuid[])`, comments)
	queueTexts(graphstore.KindMessage, `select id::text, body from messages where id = any($1::uuid[])`, messages)
	if len(asks) > 0 {
		batch.Queue(`select id::text, question, state, options, urgency, multiple from asks where id = any($1::uuid[])`, asks).Query(func(rows pgx.Rows) error {
			for rows.Next() {
				var id string
				var ask treeAsk
				var options []byte
				if err := rows.Scan(&id, &ask.question, &ask.state, &options, &ask.wording.Urgency, &ask.wording.Multiple); err != nil {
					return err
				}
				if err := json.Unmarshal(options, &ask.wording.Options); err != nil {
					return fmt.Errorf("decode the options of ask %s: %w", id, err)
				}
				b.asks[id] = ask
				b.texts[refs.Target{Kind: graphstore.KindAsk, ID: id}] = ask.question
			}
			return rows.Err()
		})
	}
	if len(results) > 0 {
		batch.Queue(`
			select r.id::text, r.actor, r.created_at, x.reason, x.actor, x.created_at
			from check_results r
			left join check_result_retractions x on x.result_id = r.id
			where r.id = any($1::uuid[])`, results).Query(func(rows pgx.Rows) error {
			for rows.Next() {
				var id string
				var actor, retractedBy []byte
				var result treeResult
				var reason *string
				var retractedAt *time.Time
				if err := rows.Scan(&id, &actor, &result.createdAt, &reason, &retractedBy, &retractedAt); err != nil {
					return err
				}
				if err := json.Unmarshal(actor, &result.actor); err != nil {
					return fmt.Errorf("decode the actor of result %s: %w", id, err)
				}
				if reason != nil {
					retraction := model.CheckResultRetracted{Reason: *reason, CreatedAt: *retractedAt}
					if err := json.Unmarshal(retractedBy, &retraction.Actor); err != nil {
						return fmt.Errorf("decode the actor of result %s's retraction: %w", id, err)
					}
					result.retracted = &retraction
				}
				b.results[id] = result
			}
			return rows.Err()
		})
	}
	queueOwedDocuments(batch, b.key, &b.owed)
	var paged, requirements, sessions []string
	for _, target := range b.page {
		switch {
		case target.Kind == graphstore.KindComment:
			paged = append(paged, target.ID)
		case target.Kind == graphstore.KindSession:
			sessions = append(sessions, target.ID)
		case target.Kind == refs.BlockNodeKind && b.entries[target].typ == requirementBlockType:
			requirements = append(requirements, target.ID)
		}
	}
	b.queueCommentAnchors(batch, paged)
	b.queueRequirementChecks(batch, requirements)
	if err := b.q.SendBatch(b.ctx, batch).Close(); err != nil {
		return fmt.Errorf("read the texts of the tree of %s: %w", b.key, err)
	}
	if len(itemBlocks) > 0 {
		texts, err := refs.BlockTexts(b.ctx, b.q, b.who, itemBlocks)
		if err != nil {
			return err
		}
		for target, text := range texts {
			b.texts[target] = text
		}
	}
	// A session's title is the newest it stamped on anything it wrote, read without the reader's
	// predicate, as the collaborators read and the Decisions read's decided_by read it, in one
	// statement for every session of the page.
	if b.titles, err = delivery.ResolveSessionTitles(b.ctx, b.q, sessions); err != nil {
		return fmt.Errorf("read the titles of the sessions of the tree of %s: %w", b.key, err)
	}
	return b.readDecisions(b.page)
}

// requirementChecksStatement is, for each requirement block $1 names, how many checks serve it at
// the cut whose seq bound $2 is, as the coverage read counts them (readServingChecks): each node
// whose current serves names it - of each relation the newest tuple at or below the bound, read
// into the requirement through graph_tuples_as_of_in, kept while it asserts - that is a block node
// of one of the issues $4, whose document_blocks row types it a check, at any state but gone,
// proposed and overtaken, that the reader whose credential $3 binds may see (the block loader's
// predicate). It is built at each read, as the block loader's is: the registry the predicate
// reads is filled by refs' init.
func requirementChecksStatement() string {
	return `
	select served.to_id, count(*)
	from (
		select distinct on (t.to_id, t.from_kind, t.from_id) t.to_id, t.from_kind, t.from_id, t.asserted
		from graph_tuples t
		where t.to_kind = '` + refs.BlockNodeKind + `' and t.to_id = any($1) and t.kind = '` + servesTupleKind + `' and t.seq <= $2
		order by t.to_id, t.from_kind, t.from_id, t.seq desc
	) served
	join graph_nodes g on g.kind = served.from_kind and g.id = served.from_id
	where served.asserted and served.from_kind = '` + refs.BlockNodeKind + `' and split_part(served.from_id, '/', 1) = any($4::text[])
	  and g.state not in ('` + graphstore.StateGone + `', '` + graphstore.StateProposed + `', '` + graphstore.StateOvertaken + `')
	  and ` + refs.VisibleKindRow(refs.BlockNodeKind, "g", "$3") + `
	  and exists (select 1 from document_blocks d where d.node_id = served.from_id and d.type = '` + graphstore.BlockTypeCheck + `')
	group by served.to_id`
}

// queueRequirementChecks queues into batch the count of the checks that serve each of
// requirements at the read's cut, of the issue read and of each issue under it the reader may see
// (requirementChecksStatement), into checks: the number GET /api/v1/issues/{key}/coverage lists
// under each, which a requirement that no check serves answers as 0.
func (b *issueTreeBuilder) queueRequirementChecks(batch *pgx.Batch, requirements []string) {
	b.checks = map[string]int{}
	if len(requirements) == 0 {
		return
	}
	seq, _ := treeCutBounds(b.cut)
	issues := append([]string{b.key}, b.descend...)
	batch.Queue(requirementChecksStatement(), requirements, seq, refs.VisibilityArg(b.who), issues).Query(func(rows pgx.Rows) error {
		for rows.Next() {
			var requirement string
			var served int
			if err := rows.Scan(&requirement, &served); err != nil {
				return err
			}
			b.checks[requirement] = served
		}
		return rows.Err()
	})
}

// requirementChecks is the requirement node target's checks member: the number of checks that
// serve it (queueRequirementChecks), and 0 for one that is closed, the requirement or a block above
// it in the tree overtaken for the reader, which the coverage read lists nothing under.
func (b *issueTreeBuilder) requirementChecks(target refs.Target) *int {
	served := b.checks[target.ID]
	for at, steps := target, 0; at != (refs.Target{}) && steps <= len(b.entries); steps++ {
		if at.Kind == refs.BlockNodeKind && b.loaded[at].Node.State == graphstore.StateOvertaken {
			served = 0
			break
		}
		entry, held := b.entries[at]
		if !held {
			break
		}
		at = entry.parent
	}
	return &served
}

// readDecisions reads, through the one builder of a decision's members (decisionread.Decisions),
// each decision node the answer holds and each ask a person answered, which the decisions read
// answers as a person's decision.
func (b *issueTreeBuilder) readDecisions(answer []refs.Target) error {
	var nodes []decisionread.Node
	for _, target := range answer {
		switch {
		case target.Kind == refs.BlockNodeKind && b.entries[target].typ == refs.DecisionSupertag:
			nodes = append(nodes, decisionread.Node{Kind: refs.BlockNodeKind, ID: target.ID})
		case target.Kind == graphstore.KindAsk && b.asks[target.ID].state == "answered":
			nodes = append(nodes, decisionread.Node{Kind: graphstore.KindAsk, ID: target.ID})
		}
	}
	b.decisions = map[refs.Target]model.Decision{}
	if len(nodes) == 0 {
		return nil
	}
	decided, err := decisionread.Decisions(b.ctx, b.q, b.who, nodes)
	if err != nil {
		return err
	}
	for _, decision := range decided {
		b.decisions[refs.Target{Kind: decision.NodeKind, ID: decision.NodeID}] = decision
	}
	return nil
}

// readIssueTreeIssue is what the issue key answers of itself in a tree read (model.IssueTreeIssue):
// whether it is converted and its tree, whose view it is (docs.ViewOf: view where the caller read
// it already, as the walk of the issue read reads it for its own key, and read here where view is
// nil), and while it has not converted the ids of its role documents, as the role predicates find
// them. Its tree (issue_trees, as issueTreeOf reads it) and its role documents are read in one
// statement (issueTreeIssueStatement).
func readIssueTreeIssue(ctx context.Context, q pgx.Tx, key string, view *string) (*model.IssueTreeIssue, error) {
	state := &model.IssueTreeIssue{}
	var tree *string
	documents := &model.IssueTreeDocuments{}
	if err := q.QueryRow(ctx, issueTreeIssueStatement, key).Scan(&tree, &documents.Spec, &documents.Verification, &documents.Plan); err != nil {
		return nil, fmt.Errorf("read the tree and the role documents of %s: %w", key, err)
	}
	if tree != nil {
		state.Converted, state.Tree = true, tree
	} else {
		state.Documents = documents
	}
	ancestor := ""
	if view != nil {
		ancestor = *view
	} else {
		var err error
		if ancestor, err = docs.ViewOf(ctx, q, key); err != nil {
			return nil, err
		}
	}
	if ancestor != "" {
		state.ViewOf = &ancestor
	}
	return state, nil
}

// issueTreeIssueStatement is the tree artifact of the issue $1 (issue_trees, as issueTreeOf reads
// it), null where it holds none, beside the ids of its role documents, as the role predicates find
// them, each read only where the issue holds no tree: a converted issue's answer names none, and
// Postgres runs a subquery of a case's arm only where the case takes it.
var issueTreeIssueStatement = `
	with tree as (select t.artifact_id::text as id from issue_trees t where t.issue_key = $1)
	select (select tree.id from tree),
	       case when not exists (select 1 from tree) then (select a.id::text from artifacts a where a.issue_key = $1 and a.is_primary) end,
	       case when not exists (select 1 from tree) then (select a.id::text from artifacts a where a.issue_key = $1 and ` + aliasedVerificationRole + `) end,
	       case when not exists (select 1 from tree) then (select a.id::text from artifacts a where a.issue_key = $1 and ` + aliasedPlanRole + `) end`

// answered is every node the answer holds, in its order, the context last (ordered); a
// comments_before read answers no context.
func (b *issueTreeBuilder) answered() []refs.Target {
	b.order = b.ordered()
	if b.before != nil {
		return b.order
	}
	return slices.Concat(b.order, b.contextOrder())
}

// queryTakes is each node of the walk the query takes, by its definition (docs'
// IssueTree.Takes).
func (b *issueTreeBuilder) queryTakes(query string) map[refs.Target]bool {
	taken := map[refs.Target]bool{}
	indexes, _ := b.tree.Takes(query)
	for _, index := range indexes {
		node := b.tree.Nodes[index]
		taken[refs.Target{Kind: node.Kind, ID: node.ID}] = true
	}
	return taken
}

// inView reports whether the view answers the node a walk or a read of the others took: a node of
// its query (taken), or of the view's fixed context, each of an issue under the issue read included
// where the view carries one (readServing, readPlanOrder), on a child issue's spec view its design
// blocks, and on the verification view the requirements it carries (requirements); and on the
// spec and the verification views each node a tie of their nodes brings in (tied, readTies).
func (b *issueTreeBuilder) inView(target refs.Target) (string, bool) {
	entry := b.entries[target]
	if b.taken[target] {
		return treeRoleQuery, true
	}
	if b.tied[target] {
		return treeRoleContext, true
	}
	switch b.view {
	case graphstore.QuerySpec:
		if entry.walk >= 0 && target.Kind == refs.BlockNodeKind && !pmdoc.IsTypedBlock(entry.typ) &&
			b.blocks[target.ID].Band == refs.RankBandTree && len(b.ancestors) > 0 {
			return treeRoleContext, true
		}
	case graphstore.QueryVerification:
		if entry.walk < 0 && (entry.typ == graphstore.BlockTypeCheck || target.Kind == graphstore.KindResult) || b.requirements[target] {
			return treeRoleContext, true
		}
	case graphstore.QueryPlan:
		if _, planned := b.plans[target]; planned && (entry.walk < 0 || target.Kind == graphstore.KindIssue) {
			return treeRoleContext, true
		}
	}
	return "", false
}

// verificationRequirements is each requirement the verification view carries as context among the
// nodes the walk and the read of the others took: every requirement node of the issue, each one
// its spec query takes, found as the spec view finds them (queryTakes), and each requirement a
// check the answer holds serves.
func (b *issueTreeBuilder) verificationRequirements() map[refs.Target]bool {
	carried := map[refs.Target]bool{}
	for target := range b.queryTakes(graphstore.QuerySpec) {
		if b.entries[target].typ == requirementBlockType {
			carried[target] = true
		}
	}
	for _, target := range b.candidates() {
		if b.entries[target].typ != graphstore.BlockTypeCheck || !b.kept[target] {
			continue
		}
		for _, relation := range b.facts[target].Relations {
			if relation.Kind == servesTupleKind {
				carried[relation.To] = true
			}
		}
	}
	return carried
}

// siblingClass is where a child stands among its siblings in the walk's order: a conversation
// item's own blocks first, then the children that hold a rank, then the rest.
func (b *issueTreeBuilder) siblingClass(target refs.Target) int {
	entry := b.entries[target]
	switch {
	case isItemBlockKind(target.Kind):
		return 0
	case entry.ranked:
		return 1
	}
	return 2
}

// compareSiblings orders two children of one parent as the walk does (docs' treeWalk.ordered): a
// ranked child by rank, an unranked one by when it was made, each tie by the lower address, every
// comparison byte for byte. Two blocks of one item keep the order the walk gave them.
func (b *issueTreeBuilder) compareSiblings(left, right refs.Target) int {
	leftClass, rightClass := b.siblingClass(left), b.siblingClass(right)
	if leftClass != rightClass {
		return leftClass - rightClass
	}
	l, r := b.entries[left], b.entries[right]
	switch leftClass {
	case 0:
		return l.walk - r.walk
	case 1:
		return cmp.Or(strings.Compare(l.rank, r.rank), refs.CompareAddresses(left, right))
	}
	return cmp.Or(l.madeAt.Compare(r.madeAt), refs.CompareAddresses(left, right))
}

// arrange is the nodes members holds but the context in the tree's order: the issue's node, each
// node before its children, a node's own children in the walk's order and after them each node
// lifted to it from under a node the answer leaves out, by its rank and then its id; then each
// node whose parent is a node members does not hold - the walk's second part, under a node of an
// issue above, and a node reached through serves whose issue's node the answer does not hold -
// each with its subtree, in the walk's order and then the order the others were read in. Where
// members holds no issue's node, as a comments_before read, each node whose parent it does not
// hold stands at the top, by when each was made and then by its address, each with its subtree.
func (b *issueTreeBuilder) arrange(members map[refs.Target]bool) []refs.Target {
	direct := map[refs.Target][]refs.Target{}
	lifted := map[refs.Target][]refs.Target{}
	var outside []refs.Target
	for _, target := range b.candidates() {
		if !members[target] || target == b.root() || b.inContext[target] {
			continue
		}
		parent := b.effective[target]
		switch {
		case !members[parent]:
			outside = append(outside, target)
		case parent == b.entries[target].parent:
			direct[parent] = append(direct[parent], target)
		default:
			lifted[parent] = append(lifted[parent], target)
		}
	}
	for parent := range direct {
		slices.SortStableFunc(direct[parent], func(left, right refs.Target) int {
			l, r := b.entries[left], b.entries[right]
			if l.walk >= 0 && r.walk >= 0 {
				return l.walk - r.walk
			}
			return b.compareSiblings(left, right)
		})
	}
	for parent := range lifted {
		slices.SortStableFunc(lifted[parent], func(left, right refs.Target) int {
			l, r := b.entries[left], b.entries[right]
			if l.ranked != r.ranked {
				if l.ranked {
					return -1
				}
				return 1
			}
			return cmp.Or(strings.Compare(l.rank, r.rank), refs.CompareAddresses(left, right))
		})
	}
	var order []refs.Target
	taken := map[refs.Target]bool{}
	var take func(target refs.Target)
	take = func(target refs.Target) {
		if taken[target] {
			return
		}
		taken[target] = true
		order = append(order, target)
		for _, child := range direct[target] {
			take(child)
		}
		for _, child := range lifted[target] {
			take(child)
		}
	}
	if members[b.root()] {
		take(b.root())
	} else {
		slices.SortStableFunc(outside, func(left, right refs.Target) int {
			return cmp.Or(b.entries[left].madeAt.Compare(b.entries[right].madeAt), refs.CompareAddresses(left, right))
		})
	}
	for _, target := range outside {
		take(target)
	}
	return order
}

// ordered is the answer's nodes but the context in its order (arrange): of the read with no view,
// the skeleton's; of the read with kind, the nodes of the skeleton it takes (kindOrder); of a view,
// the nodes it answers (viewOrder). A comments_before read with a view or kinds then holds the
// comments of its page and the nodes under them alone (underPage), which a read with neither has
// kept alone already (keepCommentsAlone), arranged as such a read stands them.
func (b *issueTreeBuilder) ordered() []refs.Target {
	order := b.arrange(b.kept)
	switch {
	case b.view == "" && len(b.kinds) == 0:
		return order
	case b.view == "":
		order = b.kindOrder(order)
	default:
		order = b.viewOrder(order)
	}
	if b.before != nil {
		return b.arrange(b.underPage(order))
	}
	return order
}

// viewOrder is the view's answer out of order, the whole tree's in its order: each node the view
// answers (inView), in its role, and each comment of a thread on the text of a node it answers
// (carriedText), with its blocks, as context, of the view's comment page alone (pageViewComments).
// The plan view orders them by their positions in the one order of the whole tree, the issue's
// node first where that order holds it.
func (b *issueTreeBuilder) viewOrder(order []refs.Target) []refs.Target {
	in := make(map[refs.Target]bool, len(order))
	var viewed []refs.Target
	for _, target := range order {
		role, answered := b.inView(target)
		if !answered {
			if _, carried := b.carriedText(target, in); carried {
				role, answered = treeRoleContext, true
			}
		}
		if answered {
			b.entries[target].role = role
			in[target] = true
			viewed = append(viewed, target)
		}
	}
	viewed = b.pageViewComments(viewed)
	if b.view == graphstore.QueryPlan {
		if _, held := b.plans[b.root()]; held {
			b.entries[b.root()].role = treeRoleContext
			viewed = append([]refs.Target{b.root()}, slices.DeleteFunc(viewed, func(target refs.Target) bool { return target == b.root() })...)
		}
		slices.SortStableFunc(viewed, func(left, right refs.Target) int {
			return cmp.Compare(b.planPosition(left), b.planPosition(right))
		})
	}
	return viewed
}

// planPosition is where a node of the plan view stands in the one order of the whole tree: its
// position, an issue's its start half's, and past every placed node for one the order gives none.
func (b *issueTreeBuilder) planPosition(target refs.Target) int {
	plan := b.plans[target]
	switch {
	case plan == nil:
	case plan.Position != nil:
		return *plan.Position
	case plan.Start != nil && plan.Start.Position != nil:
		return *plan.Start.Position
	}
	return 1 << 30
}

// contextOrder is the context nodes in kind order and then id order, each once, byte for byte: of
// the read with kind, those a tuple of a node of its answer names (kindContext) alone; of a view,
// its fixed context and the person or session a decided_by of a node it answers names, and no
// decider of a decision the view does not answer (R8: a view's context is a fixed set).
func (b *issueTreeBuilder) contextOrder() []refs.Target {
	var deciders map[refs.Target]bool
	if b.view != "" {
		deciders = map[refs.Target]bool{}
		for _, target := range b.order {
			for _, relation := range b.facts[target].Relations {
				if relation.Kind == graphstore.TupleDecidedBy {
					deciders[relation.To] = true
				}
			}
		}
	}
	var context []refs.Target
	for _, target := range b.context {
		if len(b.kinds) > 0 && !b.kindContext[target] {
			continue
		}
		if deciders != nil && (target.Kind == graphstore.KindPerson || target.Kind == graphstore.KindSession) && !deciders[target] {
			continue
		}
		if b.kept[target] && b.entries[target].role == treeRoleContext && !slices.Contains(b.order, target) {
			context = append(context, target)
		}
	}
	slices.SortFunc(context, func(left, right refs.Target) int {
		return cmp.Or(strings.Compare(left.Kind, right.Kind), strings.Compare(left.ID, right.ID))
	})
	return context
}

// documentPositions reads, for each typed block of a role document the page holds (window), its
// position in its document as GET /api/v1/issues/{key}/artifacts/{slug}/blocks lists it, from that
// read's own call on each document once (docs.Service.Blocks), and keeps how many blocks each such
// document lists (counted), which countDocuments reads no second time. It runs after the read's
// transaction has closed, since a document's blocks are its live room's.
func (b *issueTreeBuilder) documentPositions(s *server) error {
	b.positions = map[refs.Target]int{}
	b.counted = map[string]int{}
	listed := map[string]map[string]int{}
	for _, target := range b.page {
		row, documented := b.roleBlock(target)
		if !documented {
			continue
		}
		positions, read := listed[row.Document]
		if !read {
			blocks, err := s.deps.Docs.Blocks(b.ctx, row.Document)
			if err != nil {
				return err
			}
			positions = make(map[string]int, len(blocks))
			for index, block := range blocks {
				if _, held := positions[block.ID]; !held {
					positions[block.ID] = index
				}
			}
			listed[row.Document] = positions
			b.counted[row.Document] = len(blocks)
		}
		if position, held := positions[row.BlockID]; held {
			b.positions[target] = position
		}
	}
	return nil
}

// roleBlock is the document_blocks row of a typed block of a role document, and whether target is
// one.
func (b *issueTreeBuilder) roleBlock(target refs.Target) (refs.BlockRow, bool) {
	if target.Kind != refs.BlockNodeKind {
		return refs.BlockRow{}, false
	}
	row, held := b.blocks[target.ID]
	if !held || !pmdoc.IsTypedBlock(b.entries[target].typ) {
		return refs.BlockRow{}, false
	}
	switch row.Band {
	case refs.RankBandSpec, refs.RankBandVerification, refs.RankBandPlan:
		return row, true
	}
	return refs.BlockRow{}, false
}

// window cuts the page of the answer that starts at cursor (page, next, after) out of the whole
// answer's order, and holds every node of that answer (inAnswer). The page is a slice of the
// walk's order from the cursor on with each context node a node of that slice places
// (contextPlacement), at most pageSize nodes in all and one node of the walk at least; the last
// page holds too each context node no node of the walk places. So each context node stands on the
// first page whose slice holds a node a tuple of which names it, and on no other. A cursor that
// names no node of the order is 400 INVALID_QUERY.
func (b *issueTreeBuilder) window(cursor string) error {
	order := b.answered()
	context := order[len(b.order):]
	walk := order[:len(b.order)]
	start := 0
	if cursor != "" {
		address, err := base64.RawURLEncoding.DecodeString(cursor)
		kind, id, named := strings.Cut(string(address), ":")
		start = slices.Index(walk, refs.Target{Kind: kind, ID: id})
		if err != nil || !named || start < 0 {
			return errorf(http.StatusBadRequest, "INVALID_QUERY", "cursor names no node of the tree of %s: read the first page again", b.key)
		}
	}
	placement := b.contextPlacement(walk, context)
	brought := make(map[int]int, len(context))
	for _, at := range placement {
		brought[at]++
	}
	end, count := start, 0
	for end < len(walk) {
		add := 1 + brought[end]
		if end > start && count+add > b.pageSize {
			break
		}
		count += add
		end++
	}
	b.sliced = end - start
	b.page = slices.Clone(walk[start:end])
	for index, target := range context {
		switch at := placement[index]; {
		case at >= start && at < end:
			b.page, b.placed = append(b.page, target), append(b.placed, at-start)
		case at == len(walk) && end == len(walk):
			b.page, b.placed = append(b.page, target), append(b.placed, b.sliced)
		case at >= end:
			b.after++
		}
	}
	if end < len(walk) {
		b.next = &walk[end]
		b.after += len(walk) - end
	}
	b.inAnswer = make(map[refs.Target]bool, len(order))
	for _, target := range order {
		b.inAnswer[target] = true
	}
	return nil
}

// contextPlacement is, for each node of context, the index in walk, the answer's order but the
// context, of the first node a tuple between the two names, out of either: a run's interaction
// stands with the first node its changed_by_run names it from, and a person with the first
// decision decided_by them. A context node a tuple names together with another context node is
// placed with the earlier of the two, and so on through each such tie. A context node no node of
// walk names is placed at len(walk), past every node of it.
func (b *issueTreeBuilder) contextPlacement(walk, context []refs.Target) []int {
	placement := make([]int, len(context))
	if len(context) == 0 {
		return placement
	}
	walked := make(map[refs.Target]int, len(walk))
	for index, target := range walk {
		walked[target] = index
	}
	contexts := make(map[refs.Target]int, len(context))
	for index, target := range context {
		contexts[target], placement[index] = index, len(walk)
	}
	for index, target := range walk {
		for _, relation := range b.facts[target].Relations {
			if at, held := contexts[relation.To]; held {
				placement[at] = min(placement[at], index)
			}
		}
	}
	var ties [][2]int
	for at, target := range context {
		for _, relation := range b.facts[target].Relations {
			if index, held := walked[relation.To]; held {
				placement[at] = min(placement[at], index)
			}
			if other, held := contexts[relation.To]; held && other != at {
				ties = append(ties, [2]int{at, other})
			}
		}
	}
	for changed := len(ties) > 0; changed; {
		changed = false
		for _, tie := range ties {
			if low := min(placement[tie[0]], placement[tie[1]]); placement[tie[0]] != low || placement[tie[1]] != low {
				placement[tie[0]], placement[tie[1]], changed = low, low, true
			}
		}
	}
	return placement
}

// treeCursor is the cursor of a page that starts at the node of kind and id: its address,
// `<kind>:<id>`, in URL base64 with no padding.
func treeCursor(kind, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(kind + ":" + id))
}

// answer is the read's answer: the page window cut, and the tuples whose near end the page holds
// and whose far end the whole answer does, each node's beside it; and the plan the page is cut by
// where its JSON holds more than issueTreePageBytes (treePagePlan). A page that answers a cursor
// answers remaining, the nodes after it; the read with no view, the read with kind and a view
// answer comments_before.
func (b *issueTreeBuilder) answer(version int, trees *displayTreeReader) (model.IssueTreeRead, treePagePlan) {
	issue := b.issues[b.key]
	read := model.IssueTreeRead{
		Issue: b.key, View: b.view, Converted: issue.Converted, Tree: issue.Tree, ViewOf: issue.ViewOf, Documents: issue.Documents,
		Nodes: []model.IssueTreeNode{}, Tuples: []model.IssueTreeTuple{}, OwedDocuments: b.owed,
	}
	if version > 0 {
		read.Version = &version
	}
	if b.next != nil {
		next, remaining := treeCursor(b.next.Kind, b.next.ID), b.after
		read.Cursor, read.Remaining = &next, &remaining
	}
	position := b.viewNext
	if b.skeleton != nil {
		read.DocumentNodes = b.listed
		position = b.skeleton.Comments.Next
	}
	read.CommentsBefore = json.RawMessage("null")
	if position != nil {
		// A position is digits, letters and `-:.,`, which JSON quotes as Go does.
		read.CommentsBefore = strconv.AppendQuote(nil, position.String())
	}
	plan := treePagePlan{walk: b.sliced, placed: b.placed, tuples: make([]int, len(b.page)), after: b.after}
	read.Nodes = make([]model.IssueTreeNode, len(b.page))
	for index, target := range b.page {
		read.Nodes[index] = b.node(target)
		for _, relation := range b.facts[target].Relations {
			if b.inAnswer[relation.To] {
				read.Tuples = append(read.Tuples, model.IssueTreeTuple{
					Kind: relation.Kind,
					From: model.IssueTreeEnd{Kind: target.Kind, ID: target.ID},
					To:   model.IssueTreeEnd{Kind: relation.To.Kind, ID: relation.To.ID},
				})
			}
		}
		plan.tuples[index] = len(read.Tuples)
	}
	// The trees are registered once the answer's nodes stand where they will be encoded from.
	for index := range read.Nodes {
		node := &read.Nodes[index]
		if node.Decision != nil {
			trees.text(&node.Decision.TextTree, node.Decision.Text)
		}
		trees.text(&node.TextTree, node.Text)
	}
	return read, plan
}

// countDocuments counts, for each document the read with no view lists in document_nodes
// (listed), the blocks GET /api/v1/artifacts/{id}/blocks lists for it, read as that read reads
// them, off the document's live room (docs' BlockCount), or kept where documentPositions read
// them (counted). It runs after the read's transaction has closed, as documentPositions does.
func (b *issueTreeBuilder) countDocuments(s *server) error {
	for index := range b.listed {
		document := &b.listed[index]
		if count, held := b.counted[document.ArtifactID]; held {
			document.BlockCount = count
			continue
		}
		count, err := s.deps.Docs.BlockCount(b.ctx, document.ArtifactID)
		if err != nil {
			return fmt.Errorf("count the blocks of document %s of %s: %w", document.ArtifactID, b.key, err)
		}
		document.BlockCount = count
	}
	return nil
}

// node is target as the answer carries it (model.IssueTreeNode). A comment of a thread on text no
// person wrote in, and each block of one, is context in every read (onText).
func (b *issueTreeBuilder) node(target refs.Target) model.IssueTreeNode {
	entry := b.entries[target]
	held := b.facts[target]
	loaded := b.loaded[target]
	node := model.IssueTreeNode{
		Kind: target.Kind, ID: target.ID, Supertag: entry.typ, Issue: entry.issue, ReadOnly: entry.issue != "",
		Role: cmp.Or(entry.role, treeRoleTree), Author: []model.Actor{}, Attributes: make(map[string]string, len(held.Values)),
		State: graphstore.StateLive, Visibility: graphstore.VisibilityProject,
	}
	if entry.onText {
		node.Role = treeRoleContext
	}
	if entry.ranked {
		rank := entry.rank
		node.Rank = &rank
	}
	if placed, held := b.placements[target]; held {
		node.PlacementSeq = &placed
	}
	for name, value := range held.Values {
		if name != refs.AttributeRank && name != graphstore.AttributeText {
			node.Attributes[name] = value
		}
	}
	if loaded.Node.Kind != "" {
		if loaded.Node.MadeBy != nil {
			node.Author = loaded.Node.MadeBy
		}
		node.MadeAt = loaded.Node.MadeAt
		node.State, node.Visibility = cmp.Or(loaded.Node.State, node.State), cmp.Or(loaded.Node.Visibility, node.Visibility)
	} else {
		node.State = cmp.Or(held.Values[graphstore.AttributeState], node.State)
		node.Visibility = cmp.Or(held.Values[graphstore.AttributeVisibility], node.Visibility)
		if !entry.madeAt.IsZero() {
			made := entry.madeAt
			node.MadeAt = &made
		}
	}
	if _, blockID, cut := strings.Cut(target.ID, "#"); cut && (target.Kind == refs.BlockNodeKind || strings.HasSuffix(target.Kind, "_block")) {
		node.BlockID = blockID
	}
	node.Text = b.text(target)
	if parent, standing := b.effective[target]; standing && target != b.root() {
		if skeleton, held := b.named[target]; held && !b.inAnswer[skeleton] && b.visible(skeleton) {
			parent = skeleton
		}
		node.Parent = b.parentOf(parent)
	}
	if b.noConversation(target) {
		node.NoConversation = true
	}
	if row, documented := b.roleBlock(target); documented {
		if position, held := b.positions[target]; held {
			node.Document = &model.IssueTreeDocument{ArtifactID: row.Document, Slug: row.Slug, Position: position}
		}
	}
	if ask, held := b.asks[target.ID]; held && target.Kind == graphstore.KindAsk {
		wording := ask.wording
		b.treeAskAnswer(&wording, target)
		node.Ask = &wording
	}
	if decision, decided := b.decisions[target]; decided {
		node.Decision = &model.IssueTreeDecision{Decision: decision}
		if target.Kind == refs.BlockNodeKind {
			node.Decision.Uncertainty, node.Decision.UncertaintyReason = b.treeUncertainty(target)
		}
		node.Text = decision.Text
	}
	switch {
	case target.Kind == graphstore.KindResult:
		node.Result = b.result(target)
		if recorded, held := b.results[target.ID]; held {
			node.Author = []model.Actor{recorded.actor}
			made := recorded.createdAt
			node.MadeAt = &made
		}
	case entry.typ == graphstore.BlockTypeCheck:
		if row, held := b.blocks[target.ID]; held {
			node.Check = &model.IssueTreeCheck{TextSHA256: row.TextSHA256, ChangedVersion: row.ChangedVersion}
		}
	}
	if plan, planned := b.plans[target]; planned && b.view == graphstore.QueryPlan {
		node.Plan = plan
	}
	switch {
	case target.Kind == refs.BlockNodeKind && entry.typ == requirementBlockType:
		node.Checks = b.requirementChecks(target)
	case target.Kind == graphstore.KindSession:
		if title := b.titles[target.ID]; title != "" {
			node.Title = &title
		}
	case target.Kind == graphstore.KindComment:
		node.Anchor = b.anchors[target.ID]
	}
	if target.Kind == graphstore.KindIssue {
		node.IssueTreeIssue = b.issues[target.ID]
		if target.ID == b.key {
			node.Issue = ""
		}
	}
	return node
}

// text is the node's words: a block's and a finding's text value at the read's cut, a comment's or
// a message's body, an ask's question, the text of an item's block, a result's observed text, an
// issue's title, and for any other node the words an edge at it shows.
func (b *issueTreeBuilder) text(target refs.Target) string {
	if text, held := b.texts[target]; held {
		return text
	}
	held := b.facts[target]
	switch target.Kind {
	case refs.BlockNodeKind:
		return held.Values[graphstore.AttributeText]
	case graphstore.KindResult:
		return held.Values[graphstore.AttributeObserved]
	}
	if excerpt := b.loaded[target].Excerpt; excerpt != nil {
		return excerpt.Text
	}
	return ""
}

// parentOf names the node parent as a node of the answer names its parent: its store kind and id,
// its supertag, the first line of its words, and its issue where it is not the issue read.
func (b *issueTreeBuilder) parentOf(parent refs.Target) *model.IssueTreeParent {
	named := &model.IssueTreeParent{Kind: parent.Kind, ID: parent.ID}
	if entry, held := b.entries[parent]; held {
		named.Supertag, named.FirstLine, named.Issue = entry.typ, treeFirstLine(b.text(parent)), entry.issue
		if parent.Kind == graphstore.KindIssue && parent.ID != b.key {
			named.Issue = parent.ID
		}
		return named
	}
	held := b.facts[parent]
	opening, _, _ := strings.Cut(held.Values[graphstore.AttributeText], "\n")
	named.Supertag = refs.TreeNodeType(parent.Kind, b.rows[parent].Supertag, opening)
	named.FirstLine = treeFirstLine(b.text(parent))
	switch loaded := b.loaded[parent].Node; {
	case loaded.IssueKey != nil && *loaded.IssueKey != b.key:
		named.Issue = *loaded.IssueKey
	case treeIssueOf(parent) != b.key:
		named.Issue = treeIssueOf(parent)
	}
	return named
}

// treeFirstLine is the first line of a node's words that says something: past a typed block's opening
// line, its first line that is not blank, its leading space and markdown marks trimmed. It reads
// the words a line at a time and stops at that line, since a list's words are the whole list and
// every item of it names the list as its parent.
func treeFirstLine(text string) string {
	for opening := true; ; opening = false {
		line, rest, more := strings.Cut(text, "\n")
		text = rest
		line = strings.TrimSpace(line)
		switch {
		case opening && strings.HasPrefix(line, ":::"), line == "", line == ":::":
		default:
			return strings.TrimSpace(strings.TrimLeft(line, "#>-*+ "))
		}
		if !more {
			return ""
		}
	}
}

// noConversation reports whether target is a node of the issue read that no conversation node -
// an ask, a comment or a message, or a block of one - stands above in the walk. A conversation
// node itself, a node of another issue, a node of the walk's second part, the issue's own node and
// a context node carry no such mark. A block of a document the skeleton does not open
// (hiddenBlock) stands under its document, under the issue, so no conversation node stands above
// a node under one.
func (b *issueTreeBuilder) noConversation(target refs.Target) bool {
	entry := b.entries[target]
	if entry.walk < 0 || entry.above || target == b.root() || isConversationKind(target.Kind) {
		return false
	}
	at := entry.parent
	for steps := 0; steps <= len(b.entries); steps++ {
		if at == (refs.Target{}) || at == b.root() {
			return true
		}
		if isConversationKind(at.Kind) {
			return false
		}
		above, held := b.entries[at]
		if !held {
			return b.hiddenBlock(at)
		}
		at = above.parent
	}
	return true
}

// isConversationKind is whether a node of kind is an ask, a comment or a message, or a block of one.
func isConversationKind(kind string) bool {
	switch kind {
	case graphstore.KindAsk, graphstore.KindComment, graphstore.KindMessage:
		return true
	}
	return isItemBlockKind(kind)
}

// treeAskAnswer fills the answer members of the ask node target's wording from its facts, as the
// decisions read's entries read them (askNodeAnswer): the answer's words, the options it selected
// and when it was answered, each a value of the node, and who answered, the far end of its
// answered_by. A declined ask answers none of them, as its entry answers none. A stored value that
// does not parse, for which the entries read leaves the whole entry out, answers null and is
// logged.
func (b *issueTreeBuilder) treeAskAnswer(ask *model.IssueTreeAsk, target refs.Target) {
	held := b.facts[target]
	var answer askNodeAnswer
	read := func(kind, value string) {
		if _, err := answer.read(kind, value); err != nil {
			slog.Warn("dispatch: an ask's stored answer does not parse, and the tree read answers it null",
				"issue", b.key, "ask", target.ID, "err", err)
		}
	}
	for _, kind := range []string{graphstore.TupleAnswer, graphstore.TupleSelected, graphstore.TupleAnsweredAt, graphstore.TupleResolution} {
		if value, stated := held.Values[kind]; stated {
			read(kind, value)
		}
	}
	for _, relation := range held.Relations {
		if relation.Kind == graphstore.TupleAnsweredBy {
			read(relation.Kind, relation.To.ID)
		}
	}
	if answer.Resolution != nil && answer.Resolution.Kind == model.AskResolutionDeclined {
		return
	}
	ask.Answer, ask.Selected, ask.AnsweredBy, ask.AnsweredAt = answer.Answer, answer.Selected, answer.AnsweredBy, answer.AnsweredAt
}

// treeUncertainty is the decision node target's uncertainty, low, medium or high, and the reason
// for it, each a value of the node, as the decisions read's entries read them, each nil where the
// node holds none. An uncertainty of another value, for which the entries read leaves the whole
// entry out, answers nil and is logged.
func (b *issueTreeBuilder) treeUncertainty(target refs.Target) (*string, *string) {
	held := b.facts[target]
	var uncertainty, reason *string
	if value, stated := held.Values[refs.ValueUncertainty]; stated {
		switch value {
		case "low", "medium", "high":
			uncertainty = &value
		default:
			slog.Warn("dispatch: a decision's stored uncertainty is none of low, medium and high, and the tree read answers it null",
				"issue", b.key, "decision", target.ID, "uncertainty", value)
		}
	}
	if value, stated := held.Values[refs.ValueUncertaintyReason]; stated {
		reason = &value
	}
	return uncertainty, reason
}

// result is what a result node answers as the results read answers it: its verdict, its check's
// version and text sum when it was recorded, each a value of the node, and its retraction from the
// check_result_retractions row that projects its retracted value.
func (b *issueTreeBuilder) result(target refs.Target) *model.IssueTreeResult {
	values := b.facts[target].Values
	version, _ := strconv.Atoi(values[graphstore.AttributeCheckVersion])
	return &model.IssueTreeResult{
		Verdict: values[graphstore.AttributeVerdict], CheckVersion: version, CheckSHA256: values[graphstore.AttributeCheckSHA256],
		Retracted: b.results[target.ID].retracted,
	}
}
