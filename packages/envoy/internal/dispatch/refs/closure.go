package refs

import (
	"context"
	"sort"
	"sync"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Member is one member of an issue's reference closure: a document a citation reaches, or the
// lens a cited document's address moved to.
type Member struct {
	// Kind is the member's kind: graphstore.KindArtifact for a document, or graphstore.KindLens for
	// the lens a citation of a document reaches where the document's address moved to it, as the
	// pre-mortem conversion moves the address of the document it retires (api.ConvertPremortem).
	Kind string
	// Address is the member's dispatch:// address: the document's (ArtifactRef) or the lens's.
	Address string
	// RefKey is the ref_key of the document the citation reaches: the member itself, or, for a
	// lens, the retired document whose address moved to it.
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

// documentMovedTo is the join that answers, for a refs row r whose target is a document, the lens
// the document's address moved to (block_moved_to, which reads from a document's address the
// moved_to the pre-mortem conversion appends to the lens it retires the document into), where
// the acting person, bound at param, may see that lens: moved_document.lens_id, null for every
// other row. A document whose lens the reader may not see stays the document, as it reads to a
// reader who sees no move, so the walk says nothing of that lens. Like documentBlockHeld's, its
// to_kind test is a one-time filter, so the function runs for a document target alone.
func documentMovedTo(param string) string {
	return `left join lateral (
				select moved.lens_id from block_moved_to(r.to_id) as moved (lens_id)
				where r.to_kind = 'artifact' and moved.lens_id is not null
				  and ` + VisibleKind(graphstore.KindLens, `moved.lens_id`, param) + `
			) moved_document on true`
}

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
// for every link of the issue. A refs row's source is of no standing kind (refsSources), so that
// predicate binds no staleness test, which the planner would charge to every link of the issue.
// Its target is the document the walk reaches, and the block of it the row's target resolves to
// where the row names one, or the ask a move of that block ends at, so a document hidden from them
// is neither listed nor walked on from.
//
// A row's lens_id is the lens a cited document's address moved to (documentMovedTo), and its
// ref_key that document's still: the lens is the member, in the document's place, and the walk
// goes on from no lens, which cites nothing. A citation of a block of that document still reaches
// the document itself, since the move is of the document's own address, so one document can
// stand for two members, the lens and itself, and the members are told apart by both columns.
// Beside each member ride the columns of its document's address (ArtifactRef).
var closureQuery = sync.OnceValue(func() string {
	visibleSource := VisibleEnd(refsSources, `r.from_kind`,
		`case when r.from_kind = 'artifact' then (select s.ref_key from artifacts s where s.id = r.from_id::uuid) else r.from_id end`,
		true, `$2`)
	visibleTarget := VisibleKind("artifact", documentOfTarget, `$2`) +
		` and (r.to_kind <> 'block' or ` + VisibleKind("block", `block_target.node_id`, `$2`) +
		` and ` + VisibleKind(string(ItemAsk), `block_target.ask_id`, `$2`) + `)`
	movedTo := documentMovedTo(`$2`)
	return `
		with recursive closure as (
			select ` + documentOfTarget + ` as ref_key, 1 as depth, r.from_kind as via_kind, r.from_id as via_id,
			       moved_document.lens_id
			from refs r
			` + documentBlockHeld + `
			` + movedTo + `
			where ` + documentTarget + ` and (
				  (r.from_kind = 'ask'      and r.from_id in (select id::text from asks      where issue_key = $1))
				or (r.from_kind = 'comment'  and r.from_id in (select id::text from comments  where issue_key = $1))
				or (r.from_kind = 'message'  and r.from_id in (select id::text from messages  where issue_key = $1))
				or (r.from_kind = 'artifact' and r.from_id in (select id::text from artifacts where issue_key = $1))
			)
			  and ` + visibleSource + `
			  and ` + visibleTarget + `
			union
			select ` + documentOfTarget + `, 1, 'ask', r.ask_id::text, moved_document.lens_id` + askPartLinksFrom + `
			` + documentBlockHeld + `
			` + movedTo + askPartLinksCurrent + `
			  and r.ask_id in (select id from asks where issue_key = $1)
			  and ` + documentTarget + `
			  and ` + VisibleKind(string(ItemAsk), `r.ask_id::text`, `$2`) + `
			  and ` + visibleTarget + `
			union
			select ` + documentOfTarget + `, c.depth + 1, 'artifact', a.id::text, moved_document.lens_id
			from closure c
			join artifacts a on a.ref_key = c.ref_key
			join refs r on r.from_kind = 'artifact' and r.from_id = a.id::text
			` + documentBlockHeld + `
			` + movedTo + `
			where c.depth < 9 and c.lens_id is null and ` + documentTarget + `
			  and ` + visibleTarget + `
		)
		select distinct on (c.ref_key, c.lens_id) c.ref_key, c.depth, c.via_kind, c.via_id, c.lens_id,
		       a.issue_key, coalesce(a.project_key, ''), a.session_id, a.slug, a.kind, a.is_primary
		from closure c
		join artifacts a on a.ref_key = c.ref_key
		where a.issue_key is distinct from $1
		order by c.ref_key, c.lens_id, c.depth, c.via_kind, c.via_id
	`
})

// Closure follows artifact references from an issue's own content to eight hops. A reference to
// a block reaches the block's document, while a current version of the document holds that
// block, and a reference to a block that moved reaches the document of the block its moved_to
// tuples reach (graphstore.MovedTo): a deleted block's links drop out of the closure until a
// version holds its id again, unless a move records where it went. A move that ends at an ask
// reaches the document the ask's row names, the document it was decided in. A reference to a
// document whose own address moved to a lens - the pre-mortem conversion retires a document into
// one and moves its address there (api.ConvertPremortem) - reaches that lens, a member of kind
// lens at the lens's address, and the walk goes no further from it. A
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
		var lens, issue, session *string
		var project, slug, kind string
		var primary bool
		if err := rows.Scan(&member.RefKey, &member.Depth, &member.Via.Kind, &member.Via.ID, &lens,
			&issue, &project, &session, &slug, &kind, &primary); err != nil {
			return nil, false, err
		}
		if member.Depth == 9 {
			truncated = true
			continue
		}
		member.Kind, member.Address = graphstore.KindArtifact, ArtifactRef(issue, project, session, slug, kind, primary)
		if lens != nil {
			member.Kind, member.Address = graphstore.KindLens, StoreRef(graphstore.KindLens, *lens)
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
		if members[left].RefKey != members[right].RefKey {
			return members[left].RefKey < members[right].RefKey
		}
		return members[left].Kind < members[right].Kind
	})
	return members, truncated, nil
}
