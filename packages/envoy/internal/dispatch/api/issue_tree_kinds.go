package api

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// The tree read with `kind` (this issue's spec R30) is a filter over the one walk: out of the
// skeleton the read with no view answers at the same cut for the same reader, it takes each node
// of a kind it names and every ancestor of each such node up to the issue, in the walk's order, an
// ancestor of a kind it does not name marked as context. It chooses no root, no depth and no
// relation, and it reads nothing the read with no view does not read.

// The two kinds the tree read works out itself, because refs.TreeNodeType gives both the supertag
// block: a finding, a block node whose id names its lens (graphstore.ParseFindingID), and prose, a
// block node that is no finding and whose text opens no typed block. The memo is a callout.
const (
	treeKindFinding = "finding"
	treeKindProse   = "prose"
	treeKindMemo    = "callout"
)

// treeSupertagKinds are the kinds `kind` takes that are a node's supertag: a comment, a message, a
// child issue, a lens, a risk, a constraint and a result.
var treeSupertagKinds = []string{
	graphstore.KindComment, graphstore.KindMessage, graphstore.KindIssue, graphstore.KindLens,
	graphstore.KindRisk, graphstore.KindConstraint, graphstore.KindResult,
}

// treeKinds is every value `kind` takes: each typed block the installed schema declares
// (pmdoc.TypedBlockNames, the types of pmdoc/schema/blocks.json), then the supertags
// (treeSupertagKinds), finding and prose. block, the supertag every untyped block holds, is none.
func treeKinds() []string {
	return slices.Concat(pmdoc.TypedBlockNames(), treeSupertagKinds, []string{treeKindFinding, treeKindProse})
}

// parseTreeKinds reads query's `kind`, each value one kind, in the order given and each once: nil
// where the query holds none. A value that is none of treeKinds - block, an empty value - and kind
// beside view or beside v are each refused 400 INVALID_QUERY, naming kind and the values it takes:
// a view's kinds are its query's.
func parseTreeKinds(query url.Values) ([]string, error) {
	if !query.Has("kind") {
		return nil, nil
	}
	accepted := treeKinds()
	refusal := func(why string) error {
		return errorf(http.StatusBadRequest, "INVALID_QUERY", "kind %s: kind names a kind of node of the tree, one in each kind=, of %s",
			why, strings.Join(accepted, ", "))
	}
	if query.Has("view") || query.Has("v") {
		return nil, refusal("filters the read with no view, and stands beside no view and no v, since a view's kinds are its query's")
	}
	var kinds []string
	for _, kind := range query["kind"] {
		if !slices.Contains(accepted, kind) {
			return nil, refusal(fmt.Sprintf("%q is no kind the read tells apart", kind))
		}
		if !slices.Contains(kinds, kind) {
			kinds = append(kinds, kind)
		}
	}
	return kinds, nil
}

// nodeKind is the kind `kind` takes target by (treeKinds): a block node's typed block, read off
// its supertag and its text's opening line (refs.TreeNodeType), finding for a block that names its
// lens and prose for one whose text opens no typed block; and every other node's own kind. An ask
// block's own block node, which refs.TreeNodeType reads as ask, is of no kind: `kind=ask` takes the
// ask's node, which holds the question's state, and takes that block only as an ancestor.
func (b *issueTreeBuilder) nodeKind(target refs.Target) string {
	if target.Kind != refs.BlockNodeKind {
		return target.Kind
	}
	if _, _, finding := graphstore.ParseFindingID(target.ID); finding {
		return treeKindFinding
	}
	switch typ := b.entries[target].typ; {
	case typ == graphstore.SupertagAsk:
		return ""
	case pmdoc.IsTypedBlock(typ):
		return typ
	}
	return treeKindProse
}

// selects reports whether the read with kind takes target as a node of a kind it names: the memo
// alone for callout, the issue's first child where that child is a callout, which the spec query
// takes first (R14), so no other callout; for comment, each comment of a thread on no text, a post
// on the issue or a reply under one (textComments holds every comment of a thread on text); for
// issue, a child issue and never the issue read; and each node of any other kind it names.
func (b *issueTreeBuilder) selects(target refs.Target) bool {
	kind := b.nodeKind(target)
	if !slices.Contains(b.kinds, kind) {
		return false
	}
	switch kind {
	case treeKindMemo:
		return b.tree.Memo >= 0 && b.entries[target].walk == b.tree.Memo
	case graphstore.KindComment:
		_, onText := b.textComments[target]
		return !onText
	case graphstore.KindIssue:
		return target != b.root()
	}
	return true
}

// kindOrder is the read with kind's answer out of order, the skeleton's in the walk's order: each
// node it selects, as a node of the tree; every ancestor of each, up to the issue, as context
// where it selects no such ancestor; each comment of a thread on the text of a node it answers
// (carriedText), a comment of a thread a person wrote in as a node of the tree and any other as
// context (node), a comment on text never bringing in its node; each walk node a has_kind,
// constrained_by, changed_by_run or decided_by tuple of a node it answers names, as context; and
// an item's own blocks with their item, in its item's role. It marks in kindContext each context
// node of the read with no view such a tuple names, which contextOrder answers alone. A context
// node it adds brings in no further context, and no other node is in it.
func (b *issueTreeBuilder) kindOrder(order []refs.Target) []refs.Target {
	in := make(map[refs.Target]bool, len(order))
	roles := make(map[refs.Target]string, len(order))
	mark := func(target refs.Target, role string) {
		if !in[target] {
			in[target], roles[target] = true, role
		}
	}
	for _, target := range order {
		if b.selects(target) {
			mark(target, treeRoleTree)
		}
	}
	for _, target := range order {
		if roles[target] != treeRoleTree {
			continue
		}
		for at, steps := b.effective[target], 0; at != (refs.Target{}) && b.kept[at] && steps <= len(b.entries); steps++ {
			mark(at, treeRoleContext)
			if at == b.root() {
				break
			}
			at = b.effective[at]
		}
	}
	for _, target := range order {
		if in[target] {
			continue
		}
		if host, carried := b.carriedText(target, in); carried {
			mark(target, treeRoleTree)
			if !in[b.effective[target]] {
				b.effective[target] = host
			}
		}
	}
	answering := make([]refs.Target, 0, len(in))
	for _, target := range order {
		if in[target] {
			answering = append(answering, target)
		}
	}
	b.kindContext = map[refs.Target]bool{}
	for _, target := range answering {
		for _, relation := range b.facts[target].Relations {
			switch relation.Kind {
			case graphstore.TupleHasKind, graphstore.TupleConstrainedBy, graphstore.TupleChangedByRun, graphstore.TupleDecidedBy:
			default:
				continue
			}
			entry, held := b.entries[relation.To]
			switch {
			case !held:
			case entry.role == treeRoleContext:
				b.kindContext[relation.To] = true
			case b.kept[relation.To]:
				mark(relation.To, treeRoleContext)
			}
		}
	}
	var answered []refs.Target
	for _, target := range order {
		if !in[target] && isItemBlockKind(target.Kind) && in[b.entries[target].parent] {
			mark(target, roles[b.entries[target].parent])
		}
		if !in[target] {
			continue
		}
		if roles[target] == treeRoleContext {
			b.entries[target].role = treeRoleContext
		}
		answered = append(answered, target)
	}
	return answered
}

// isItemBlockKind is whether a node of kind is a block of an ask, a comment or a message.
func isItemBlockKind(kind string) bool {
	switch kind {
	case refs.ItemAsk.BlockKind(), refs.ItemComment.BlockKind(), refs.ItemMessage.BlockKind():
		return true
	}
	return false
}
