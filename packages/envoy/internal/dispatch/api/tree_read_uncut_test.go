package api

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// The un-cut walk: the tree read with R8's two cuts undone, the reference read
// TestTheTwoCutsAnswerAsTheUncutWalk holds every read of the route to, byte for byte, at the same
// cut (c-tree-read-bound). The route's read (readIssueTree) reads a marked issue's head rows, reads
// the facts of only the nodes its answer takes - no block of the issue's other documents and, of
// the comments off its comment page, only the facts that choose the page - each loader once for all
// of them (the first cut), and the members of its page's nodes alone (the second cut). This read
// walks the tuples, the caller having taken the issue's mark away for its length, reads the facts
// and the members of every node the walk of the whole tree reaches and of every node the read of
// the others and the context add to it - the blocks of the issue's other documents and the
// comments off the comment page among them - and only then applies the skeleton, the comment page
// and the page window, through the route's own steps, reading nothing it read already. So a cut
// that answers otherwise than the read it cuts is a difference between the two answers. The seam
// it answers through is serveIssueTree's reader, which only a test hands it.

// readUncutIssueTree is the un-cut walk (above): an issueTreeReader serveIssueTree answers a
// request through, as the route answers one through readIssueTree.
func (s *server) readUncutIssueTree(ctx context.Context, who model.Actor, key, view string, kinds []string, version int, cursor string,
	before *refs.CommentPosition, trees *displayTreeReader) (model.IssueTreeRead, treePagePlan, error) {
	tx, err := s.deps.Store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return model.IssueTreeRead{}, treePagePlan{}, fmt.Errorf("open the un-cut tree read of %s: %w", key, err)
	}
	defer tx.Rollback(ctx)
	if err := refs.TurnJITOff(ctx, tx); err != nil {
		return model.IssueTreeRead{}, treePagePlan{}, err
	}
	// Each builder reads as the route's does (readIssueTree, build): under the deployment the read
	// is served from, whose dashboard URLs each excerpt is cut around, and, at a version, with the
	// blocks a later write deleted from the cut (readHistoricalBlocks).
	pageSize := cmp.Or(s.deps.IssueTreePageSize, issueTreePageSize)
	whole := &issueTreeBuilder{ctx: ctx, q: tx, who: who, key: key, view: view, pageSize: pageSize, serverURL: s.deps.ServerURL, atVersion: version != 0}
	if err := whole.readWholeTree(version); err != nil {
		return model.IssueTreeRead{}, treePagePlan{}, err
	}
	builder := &issueTreeBuilder{ctx: ctx, q: tx, who: who, key: key, view: view, kinds: kinds, before: before, pageSize: pageSize,
		serverURL: s.deps.ServerURL, atVersion: version != 0}
	builder.takeReadsOf(whole)
	if err := builder.applySkeleton(version, cursor); err != nil {
		return model.IssueTreeRead{}, treePagePlan{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.IssueTreeRead{}, treePagePlan{}, fmt.Errorf("close the un-cut tree read of %s: %w", key, err)
	}
	page := builder.page
	builder.page = sortedTargets(builder.inAnswer)
	if err := builder.documentPositions(s); err != nil {
		return model.IssueTreeRead{}, treePagePlan{}, err
	}
	if err := builder.countDocuments(s); err != nil {
		return model.IssueTreeRead{}, treePagePlan{}, err
	}
	if err := builder.anchorOccurrences(s); err != nil {
		return model.IssueTreeRead{}, treePagePlan{}, err
	}
	builder.page = page
	answer, plan := builder.answer(version, trees)
	return answer, plan, nil
}

// readWholeTree reads, at the cut of the view's version, everything the un-cut walk reads before it
// applies anything: the walk of the whole tree over the tuples (docs.WalkIssueTree), every comment
// of a thread on text, the others, the facts of every candidate, every node the answer of the whole
// tree keeps and its context, and the members of all of them (readMembers over every kept node). It
// applies no skeleton, no comment page, no kind, no comments_before and no page window.
func (b *issueTreeBuilder) readWholeTree(version int) error {
	cut, err := issueTreeCut(b.ctx, b.q, b.key, b.view, version)
	if err != nil {
		return err
	}
	b.cut = cut
	if err := b.readViewTextComments(); err != nil {
		return err
	}
	if b.tree, err = docs.WalkIssueTree(b.ctx, b.q, b.key, cut); err != nil {
		return err
	}
	b.enterWalk()
	if err := b.readDescendants(); err != nil {
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
	b.placeOthers()
	b.keep()
	if err := b.readContext(); err != nil {
		return err
	}
	b.inAnswer = map[refs.Target]bool{}
	for target, kept := range b.kept {
		if kept {
			b.inAnswer[target] = true
		}
	}
	b.page = sortedTargets(b.inAnswer)
	return b.readMembers()
}

// takeReadsOf takes everything whole read (readWholeTree) as read already: the facts, the rows, the
// loaded nodes and the blocks of every node it read, and the members of every node it kept.
func (b *issueTreeBuilder) takeReadsOf(whole *issueTreeBuilder) {
	b.facts, b.loaded, b.rows, b.blocks, b.read = whole.facts, whole.loaded, whole.rows, whole.blocks, whole.read
	b.issues, b.texts, b.asks, b.results, b.decisions = whole.issues, whole.texts, whole.asks, whole.results, whole.decisions
	b.placements, b.owed, b.checks, b.titles, b.anchors = whole.placements, whole.owed, whole.checks, whole.titles, whole.anchors
}

// applySkeleton is build less its reads of the members: the skeleton and the comment page of the
// read with no view (readSkeleton) or a view's comments on text, the walk of the tuples that takes
// them (docs.WalkIssueSkeleton with no head rows, docs.WalkIssueTree), the others, the nodes the
// answer keeps and its context, and the page window from cursor on, each step reading only what
// the reads taken from the whole tree (takeReadsOf) do not hold.
func (b *issueTreeBuilder) applySkeleton(version int, cursor string) error {
	cut, err := issueTreeCut(b.ctx, b.q, b.key, b.view, version)
	if err != nil {
		return err
	}
	b.cut = cut
	if b.view == "" {
		if _, err := b.readSkeleton(); err != nil {
			return err
		}
	} else if err := b.readViewTextComments(); err != nil {
		return err
	}
	if b.skeleton != nil {
		b.tree, err = docs.WalkIssueSkeleton(b.ctx, b.q, b.key, cut, nil, *b.skeleton)
	} else {
		b.tree, err = docs.WalkIssueTree(b.ctx, b.q, b.key, cut)
	}
	if err != nil {
		return err
	}
	b.enterWalk()
	if err := b.readDescendants(); err != nil {
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
	b.placeOthers()
	b.keep()
	if err := b.readContext(); err != nil {
		return err
	}
	return b.window(cursor)
}

// sortedTargets is each node of held, by kind and then id, byte for byte.
func sortedTargets(held map[refs.Target]bool) []refs.Target {
	targets := slices.Collect(maps.Keys(held))
	slices.SortFunc(targets, func(left, right refs.Target) int {
		return cmp.Or(strings.Compare(left.Kind, right.Kind), strings.Compare(left.ID, right.ID))
	})
	return targets
}

// uncutTreeResponse answers request, a GET /api/v1/issues/{key}/tree of the issue key, through
// served's own handling of the route's query (serveIssueTree) and the un-cut walk
// (readUncutIssueTree): its status and its body.
func uncutTreeResponse(served *server, request *http.Request, key string) (int, []byte) {
	request.SetPathValue("key", key)
	response := httptest.NewRecorder()
	served.serveIssueTree(response, request, served.readUncutIssueTree)
	return response.Code, response.Body.Bytes()
}
