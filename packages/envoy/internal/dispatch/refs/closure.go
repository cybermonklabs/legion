package refs

import (
	"context"
	"sort"
	"sync"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Member is an artifact reached through an issue's reference closure.
type Member struct {
	RefKey string
	Depth  int
	Via    model.ReferenceVia
}

// documentTarget is true of a refs row r whose target is a document, or a block a current
// version of its document holds, or a block address that no longer stands whose moved_to tuples
// reach one (graphstore.MovedTo) or reach an ask whose row stands; documentOfTarget is that
// document's ref_key, which never holds a '#': the document of the block the target resolved to,
// so a citation of a block that moved reaches the document it moved into (R8), and the document
// the row of an ask a move ends at names, so a citation of the heading of a section that cites an
// answered ask a person removed reaches the document that ask was decided in. documentBlockHeld
// is the join that answers what a block target resolves to (movedTarget), block_target, and
// documentTarget reads its answer rather than an `exists` probe of its own:
// inside the recursive walk below the probe sits under an OR, which no semijoin can carry, so the
// planner plans it as a hashed subplan and reads all of document_blocks to build the hash —
// 800,000 rows and 45,915 buffers of a 122,128-buffer read, once for each place the predicate is
// spliced into. A lateral whose probe stops at its first row is a nested loop keyed through
// document_blocks_node_id, the hash index on exactly node_id, and it runs at all only for a block
// target, because its own to_kind test is a one-time filter the planner re-reads on each loop.
// Only a target that probe finds no row for is followed through moved_to_reach, the SQL function
// every resolver reads a move through, so a block that stands costs the probe alone.
//
// documentTarget states the two target kinds the scan must read in a clause of their own as
// well. The kinds are implied by its disjunction, but that disjunction now reads a column of a
// join, so the planner can no longer derive the restriction from it and would read refs whole.
var (
	documentBlockHeld = `left join lateral (` + movedTarget(`r.to_id`) + `
				where r.to_kind = 'block'
			) block_target on true`
	documentTarget   = `(r.to_kind in ('artifact', 'block') and (r.to_kind = 'artifact' or block_target.node_id is not null or block_target.ask_document is not null))`
	documentOfTarget = `case when r.to_kind = 'block' then coalesce(split_part(block_target.node_id, '#', 1), block_target.ask_document) else r.to_id end`
)

// movedTarget is the select list and FROM of what the block address toID resolves to, as
// (node_id, ask_id, ask_document): node_id is the block a document_blocks row holds at toID, else
// the block its moved_to tuples reach; where they reach an ask whose row stands instead, ask_id is
// that ask's id and ask_document the ref_key of the document its row names. Each is null where
// it resolves to none. The probe of document_blocks comes first, and moved_to_reach runs only
// where it found no row: its subquery's own test of the probe is a one-time filter, kept one by
// `offset 0`, which stops the planner pulling the subquery up into the join.
func movedTarget(toID string) string {
	return `
				select coalesce(probe.node_id, case when moved.kind = 'block' then moved.id end) as node_id,
				       case when moved.kind = 'ask' then moved.id end as ask_id,
				       case when moved.kind = 'ask' then (
				           select asked_in.ref_key from asks moved_ask
				           join artifacts asked_in on asked_in.id = moved_ask.block_artifact_id
				           where moved_ask.id = moved.id::uuid) end as ask_document
				from (select (select d.node_id from document_blocks d where d.node_id = ` + toID + ` limit 1) as node_id) probe
				left join lateral (
					select reach.kind, reach.id from moved_to_reach(` + toID + `) reach where probe.node_id is null offset 0
				) moved on true`
}

// closureQuery is the statement Closure runs, held here so that its plan can be read by name. $1
// is the issue and $2 the acting credential (VisibilityArg). It is rendered at its first use and
// not as the variable is initialized, because the predicates it binds read the registry, which
// init fills after every variable of the package (nodeRegistry): rendered before that, every
// predicate would be `false` and the closure empty.
//
// A link is in the walk only where the acting person may see both its ends. Its source - an ask,
// a comment, a message or a document of the issue - is read through VisibleEnd, with a
// document's id turned into the ref_key the store keys it by through artifacts' primary key: the
// registry's own rewrite compares the uuid as text, which no index serves, and this arm runs once
// for every link of the issue. Its target is the document the walk reaches, and the block of it
// the row's target resolves to where the row names one, or the ask a move of that block ends at,
// so a document hidden from them is neither listed nor walked on from.
var closureQuery = sync.OnceValue(func() string {
	visibleSource := VisibleEnd(`r.from_kind`,
		`case when r.from_kind = 'artifact' then (select s.ref_key from artifacts s where s.id = r.from_id::uuid) else r.from_id end`,
		true, `$2`)
	visibleTarget := VisibleKind("artifact", documentOfTarget, `$2`) +
		` and (r.to_kind <> 'block' or ` + VisibleKind("block", `block_target.node_id`, `$2`) +
		` and ` + VisibleKind(string(ItemAsk), `block_target.ask_id`, `$2`) + `)`
	return `
		with recursive closure as (
			select ` + documentOfTarget + ` as ref_key, 1 as depth, r.from_kind as via_kind, r.from_id as via_id
			from refs r
			` + documentBlockHeld + `
			where ` + documentTarget + ` and (
				  (r.from_kind = 'ask'      and r.from_id in (select id::text from asks      where issue_key = $1))
				or (r.from_kind = 'comment'  and r.from_id in (select id::text from comments  where issue_key = $1))
				or (r.from_kind = 'message'  and r.from_id in (select id::text from messages  where issue_key = $1))
				or (r.from_kind = 'artifact' and r.from_id in (select id::text from artifacts where issue_key = $1))
			)
			  and ` + visibleSource + `
			  and ` + visibleTarget + `
			union
			select ` + documentOfTarget + `, 1, 'ask', r.ask_id::text` + askPartLinksFrom + `
			` + documentBlockHeld + askPartLinksCurrent + `
			  and r.ask_id in (select id from asks where issue_key = $1)
			  and ` + documentTarget + `
			  and ` + VisibleKind(string(ItemAsk), `r.ask_id::text`, `$2`) + `
			  and ` + visibleTarget + `
			union
			select ` + documentOfTarget + `, c.depth + 1, 'artifact', a.id::text
			from closure c
			join artifacts a on a.ref_key = c.ref_key
			join refs r on r.from_kind = 'artifact' and r.from_id = a.id::text
			` + documentBlockHeld + `
			where c.depth < 9 and ` + documentTarget + `
			  and ` + visibleTarget + `
		)
		select distinct on (c.ref_key) c.ref_key, c.depth, c.via_kind, c.via_id
		from closure c
		join artifacts a on a.ref_key = c.ref_key
		where a.issue_key is distinct from $1
		order by c.ref_key, c.depth, c.via_kind, c.via_id
	`
})

// Closure follows artifact references from an issue's own content to eight hops. A reference to
// a block reaches the block's document, while a current version of the document holds that
// block, and a reference to a block that moved reaches the document of the block its moved_to
// tuples reach (graphstore.MovedTo): a deleted block's links drop out of the closure until a
// version holds its id again, unless a move records where it went. A move that ends at an ask
// reaches the document the ask's row names, the document it was decided in. A
// block of a comment or message reaches its item, as a document's block reaches its document: a
// link one holds is read from its item's own refs row, which every comment and message keeps
// beside its blocks' mentions, so the member's via is the item. An ask keeps no such row - its
// refs are its question's alone - so the links its parts hold are read from ask_block_refs, by
// the same rule: the member's via is the ask, at the depth the ask's own link has. A reference to
// an item block, as one to its item, reaches no document.
//
// It answers for who: a link whose source or target who may not see is no link of the walk, so a
// member is never reached through, or named as the via of, a node reserved for another author,
// and such a document is never a member (VisibleKind, VisibleEnd).
func Closure(ctx context.Context, q Queryer, who model.Actor, issueKey string) ([]Member, bool, error) {
	rows, err := q.Query(ctx, closureQuery(), issueKey, VisibilityArg(who))
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	members := []Member{}
	truncated := false
	for rows.Next() {
		var member Member
		if err := rows.Scan(&member.RefKey, &member.Depth, &member.Via.Kind, &member.Via.ID); err != nil {
			return nil, false, err
		}
		if member.Depth == 9 {
			truncated = true
			continue
		}
		members = append(members, member)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	sort.Slice(members, func(left, right int) bool {
		if members[left].Depth != members[right].Depth {
			return members[left].Depth < members[right].Depth
		}
		return members[left].RefKey < members[right].RefKey
	})
	return members, truncated, nil
}
