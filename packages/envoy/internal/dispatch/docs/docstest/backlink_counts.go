package docstest

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Queryer is the query surface ViewBacklinkCounts reads through: a pool's or a transaction's.
type Queryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// viewBacklinkCount is the first arm of the count a release before refs.BacklinkCounts read
// graph_edges arm by arm ran over the whole view, the reading the arms of that count must add up
// to: the view, filtered and grouped. It is held as a literal, copied from that release rather
// than built from refs' helpers, on purpose - built from the helpers, a change to the count's own
// builders would move this statement and the count together and the comparison would pass
// whatever either said. The view it reads is its one argument, graph_edges where that release
// read it (ViewBacklinkCountSQL). $1 is the target kind, $2 the ids, $3 the node kinds the
// counting build reads; $4, when the statement carries viewBacklinkCountExcluded, is the excluded
// edge kinds.
func viewBacklinkCount(view string) string {
	return `select g.to_id from ` + view + ` g
where g.to_kind = $1 and g.to_id = any($2)`
}

// viewBacklinkCountAtItsBlocks is the statement's second arm for each item kind over view: an
// edge at one of the item's current blocks, counted as the item's own, since the item's own read
// lists it. The block kind and the item's tables are written out per kind for the reason the
// first arm is.
var viewBacklinkCountAtItsBlocks = map[string]func(view string) string{
	"comment": func(view string) string {
		return `select part.comment_id::text from ` + view + ` g
	join comment_blocks part on part.node_id = g.to_id and part.position is not null
	where g.to_kind = 'comment_block' and part.comment_id::text = any($2)`
	},
	"message": func(view string) string {
		return `select part.message_id::text from ` + view + ` g
	join message_blocks part on part.node_id = g.to_id and part.position is not null
	where g.to_kind = 'message_block' and part.message_id::text = any($2)`
	},
	"ask": func(view string) string {
		return `select part.ask_id::text from ` + view + ` g
	join ask_blocks part on part.node_id = g.to_id and part.position is not null
	where g.to_kind = 'ask_block' and part.ask_id::text = any($2)`
	},
}

// viewBacklinkCountNarrow is what each arm of the statement carries: the source-kind filter and
// the probe that keeps an item's own mention out of the count where one of its blocks holds a
// link of any kind to the same target, written out as the rest of the statement is.
const viewBacklinkCountNarrow = `
  and g.from_kind = any($3)
  and not (g.kind = 'mentions' and (
    (g.from_kind = 'artifact' and exists (select 1 from block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id and b.artifact_id::text = g.from_id))
    or (g.from_kind = 'comment' and exists (select 1 from comment_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id and b.comment_id::text = g.from_id))
    or (g.from_kind = 'message' and exists (select 1 from message_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id and b.message_id::text = g.from_id))
    or (g.from_kind = 'ask' and exists (select 1 from ask_block_refs b where b.to_kind = g.to_kind and b.to_id = g.to_id and b.ask_id::text = g.from_id))))`

// viewBacklinkCountExcluded is the statement's exclusion predicate, the one part of it a caller's
// exclusions add.
const viewBacklinkCountExcluded = `
  and not (g.kind = any($4))`

// ViewBacklinkCountSQL is that release's whole statement for toKind, read over view: the view's
// own arm, the arm over the item's current blocks where the kind has one, and the group. Over
// graph_edges it is what refs.BacklinkCounts must answer arm for arm
// (refs.TestBacklinkCountsReadArmByArmAgreeWithTheView). The release before this one held its
// count to it over its own graph_edges in the same test, so over that release's view it is what
// that release's count answers over the same rows (docs.TestGraphEdgesLosesEveryArmOverAMovedKind).
// excluded says whether the caller binds excluded edge kinds as $4. refs' timing harness measures
// this text as the before side of the backlink pair, so it is built in one place rather than
// assembled for each caller.
func ViewBacklinkCountSQL(view, toKind string, excluded bool) string {
	narrow := viewBacklinkCountNarrow
	if excluded {
		narrow += viewBacklinkCountExcluded
	}
	arms := viewBacklinkCount(view) + narrow
	if atBlocks, ok := viewBacklinkCountAtItsBlocks[toKind]; ok {
		arms += "\nunion all\n" + atBlocks(view) + narrow
	}
	return `select id, count(*)::int from (` + arms + `) e (id) group by id`
}

// ViewBacklinkCounts counts the inbound edges of ids of toKind over view with
// ViewBacklinkCountSQL, binding nodeKinds as the node kinds an edge may come from and, when there
// are any, excludeKinds as the edge kinds left out. A node with no inbound edge is absent from the
// answer, as it is from refs.BacklinkCounts'.
func ViewBacklinkCounts(t testing.TB, q Queryer, view, toKind string, ids, nodeKinds, excludeKinds []string) map[string]int {
	t.Helper()
	args := []any{toKind, ids, nodeKinds}
	if len(excludeKinds) > 0 {
		args = append(args, excludeKinds)
	}
	rows, err := q.Query(context.Background(), ViewBacklinkCountSQL(view, toKind, len(excludeKinds) > 0), args...)
	if err != nil {
		t.Fatalf("count %s backlinks over %s: %v", toKind, view, err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var id string
		var count int
		if err := rows.Scan(&id, &count); err != nil {
			t.Fatalf("scan a %s backlink count over %s: %v", toKind, view, err)
		}
		counts[id] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the %s backlink counts over %s: %v", toKind, view, err)
	}
	return counts
}
