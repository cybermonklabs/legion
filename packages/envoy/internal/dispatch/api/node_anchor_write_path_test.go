package api

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// requireWrittenNodeAnchor fails t unless comment, a comment written on a node's text, states as its
// own write left it its anchored_in and its part_of to the node its anchor names, and the tree read
// answers it under that node (R31). c-node-anchor reads it before any pass of the backfill runs: a
// pass levels a node-anchored comment's pair from comments.node_anchor
// (graphstore.CommentAnchoredInSQL), so a read after one would hold the pass and not the write. It
// prints the comment, its part_of, its anchored_in and the parent the read answered.
func requireWrittenNodeAnchor(t *testing.T, database *store.Store, tree map[string]nodeAnchorTreeNode, comment nodeAnchored) {
	t.Helper()
	stated := map[string][]string{}
	held := true
	for _, relation := range []string{graphstore.TuplePartOf, graphstore.TupleAnchoredIn} {
		tuples, err := graphstore.CurrentOut(context.Background(), database.Pool, graphstore.KindComment, comment.id, []string{relation})
		if err != nil {
			t.Fatalf("read the %s of %s %s: %v", relation, comment.name, comment.id, err)
		}
		ends := make([]string, 0, len(tuples))
		for _, tuple := range tuples {
			ends = append(ends, tuple.ToKind+":"+tuple.ToID)
		}
		stated[relation] = ends
		if !slices.Contains(ends, comment.kind+":"+comment.node) {
			held = false
		}
	}
	parent := "no node: the read lists no such comment"
	node, listed := tree[graphstore.KindComment+":"+comment.id]
	switch {
	case listed && node.Parent == nil:
		parent = "no parent"
	case listed:
		parent = node.Parent.Kind + ":" + node.Parent.ID
	}
	if !held || parent != comment.kind+":"+comment.node {
		t.Errorf("%s %s, as its write left it before any backfill, states part_of [%s] and anchored_in [%s], and the tree read answers it under %s: want each to the node %s:%s",
			comment.name, comment.id, strings.Join(stated[graphstore.TuplePartOf], " "), strings.Join(stated[graphstore.TupleAnchoredIn], " "),
			parent, comment.kind, comment.node)
	}
}
