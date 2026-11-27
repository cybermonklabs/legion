package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// FindNodeUnderIssue is the node the `{id}` of an `/api/v1/issues/{key}/nodes/{id}/…` route names,
// read for who, which must stand under the issue key names. Every route of that shape decodes its
// `{id}` here and nowhere else: `/state`, `/settle`, `/findings` and `/pointers`. Such a route's
// ID resolves no prefix, because the segment is a store address rather than an id a
// prefix shortens.
//
// id is the one path segment, which the client percent-encodes whole and the router decodes, so
// it may hold a `/` and a `#`. It is the node's store address, in one of three spellings:
//
//   - `<kind>/<id>`, the kind the read registry resolves and the id the store keys the node by:
//     `issue/CORE-1`, `artifact/CORE-1/spec` (a document by its ref_key), `comment/<uuid>`,
//     `ask/<uuid>`, `message/<uuid>`, `lens/<uuid>`, `risk/<uuid>`, `constraint/<uuid>`;
//   - a block's own node id, which names its owner before its `#`: `<ref_key>#<block id>` for a
//     block of a document (`CORE-1/spec#r1`), and `<kind>/<uuid>#<block id>` for a block of an ask,
//     a comment or a message, or a finding of a lens (`comment/<uuid>#b2`, `lens/<uuid>#<block
//     id>`), read under that kind's block kind (`comment_block`, `lens_block`);
//   - the node's whole `dispatch://` address (`dispatch://CORE-1/spec#b-r1`,
//     `dispatch://lens/<uuid>`), read as GET /api/v1/references reads one (text.ParseRef,
//     refs.TargetOf). A pin or a version it carries names the same node.
//
// A document block's address is the node it resolves to (resolvedNodeAddress), so the spec's own
// address of a block of an issue born converted names the room's node.
//
// A node stands under key when the issue it is located by is key: the issue itself, a document
// of the issue and each of its blocks, an ask, a comment or a message of the issue and each of
// their blocks, and a lens, a finding, a risk or a constraint whose part_of walk ends at one of
// those, as the registry's loaders answer IssueKey. An issue node also stands in the tree of an
// ancestor that reaches it through visible children (issueNodeBelow). An address that decodes to
// nothing, a node that does not stand under key, and a node who may not see all answer the same
// 404 NOT_FOUND (nodeNotFound), so the answer says nothing of whether such a node exists.
func FindNodeUnderIssue(ctx context.Context, q refs.Queryer, who model.Actor, key, id string) (model.GraphNode, error) {
	kind, address, ok, err := resolvedNodeAddress(ctx, q, id)
	if err != nil {
		return model.GraphNode{}, err
	}
	if !ok {
		return model.GraphNode{}, nodeNotFound(key, id)
	}
	node, found, err := refs.Node(ctx, q, who, kind, address, true)
	if err != nil {
		return model.GraphNode{}, err
	}
	if !found || node.IssueKey == nil {
		return model.GraphNode{}, nodeNotFound(key, id)
	}
	if *node.IssueKey != key {
		if kind != graphstore.KindIssue {
			return model.GraphNode{}, nodeNotFound(key, id)
		}
		below, err := issueNodeBelow(ctx, q, who, node.ID, key)
		if err != nil {
			return model.GraphNode{}, err
		}
		if !below {
			return model.GraphNode{}, nodeNotFound(key, id)
		}
	}
	return node, nil
}

// issueNodeBelow tests one branch of the tree read's IssueDescendants walk, from its named issue
// up, without reading its siblings. A hidden issue cannot make a descendant reachable.
func issueNodeBelow(ctx context.Context, q refs.Queryer, who model.Actor, child, key string) (bool, error) {
	var below bool
	err := q.QueryRow(ctx, `
		with recursive chain (key, parent_key, depth) as (
			select i.key, i.parent_key, 0 from issues i where i.key = $1
			union all
			select p.key, p.parent_key, c.depth + 1 from chain c
			join issues p on p.key = c.parent_key
			where c.depth < $3 and c.key <> $2 and `+refs.VisibleKind(graphstore.KindIssue, "p.key", "$4")+`
		)
		select exists (select 1 from chain where key = $2 and depth > 0)
	`, child, key, refs.IssueDepthCap, refs.VisibilityArg(who)).Scan(&below)
	if err != nil {
		return false, fmt.Errorf("read whether issue %s stands under %s: %w", child, key, err)
	}
	return below, nil
}

// nodeNotFound is the one 404 NOT_FOUND of an address id that names no node the caller may see
// under the issue key.
func nodeNotFound(key, id string) error {
	return errorf(http.StatusNotFound, "NOT_FOUND", "no node %q under %s", id, key)
}

// resolvedNodeAddress is the registry kind and the store id the `{id}` segment names
// (decodeNodeAddress), with a document block's address taken as the node it resolves to
// (graphstore.MovedTo: its moved_to tuples first, then its document's room, then itself), as every
// reader of a reference resolves one: a block, or the ask a chain of moves ends at, named by its
// id. An address that resolves to neither is answered as decoded, which names no node.
func resolvedNodeAddress(ctx context.Context, q refs.Queryer, id string) (kind, address string, ok bool, err error) {
	kind, address, ok = decodeNodeAddress(id)
	if !ok || kind != refs.BlockNodeKind {
		return kind, address, ok, nil
	}
	reached, resolved, err := graphstore.MovedTo(ctx, q, address)
	if err != nil {
		return "", "", false, err
	}
	switch {
	case !resolved:
	case reached.Ask != "":
		kind, address = graphstore.KindAsk, reached.Ask
	default:
		address = reached.Address
	}
	return kind, address, true, nil
}

// decodeNodeAddress is the registry kind and the store id the `{id}` segment names, as
// FindNodeUnderIssue reads it, and false for a segment that names no node of a kind the registry
// resolves. The store id is the one refs.Node takes with byRefKey: a document's ref_key.
func decodeNodeAddress(id string) (kind, address string, ok bool) {
	if strings.HasPrefix(id, "dispatch://") {
		ref, parsed := text.ParseRef(id, "")
		if !parsed {
			return "", "", false
		}
		target, targeted := refs.TargetOf(ref)
		return target.Kind, target.ID, targeted && refs.ResolvesNodeKind(target.Kind)
	}
	if strings.Contains(id, "#") {
		// An item's block names its item's kind and uuid before the `#`, and a finding its lens's;
		// a document's block names the document's ref_key, which begins with an issue key, a
		// project or `agent`, none of which is such a kind.
		if item, _, _, isItemBlock := refs.ParseItemBlockNode(id); isItemBlock {
			return item.BlockKind(), id, true
		}
		return graphstore.AddressKind(refs.BlockNodeKind, id), id, true
	}
	kind, address, named := strings.Cut(id, "/")
	if !named || address == "" || !refs.ResolvesNodeKind(kind) {
		return "", "", false
	}
	return kind, address, true
}
