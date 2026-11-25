package startup

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

var (
	_ = registerRetirement(AsksArmsRetirement)
	_ = registerRetirement(IssueTreeArmsRetirement)
)

// The old arms of graph_edges. A release that moved a relation into the node and tuple store left
// the view's arm over the relation's own table, answering only the rows that name no node, which a
// build of the release before writes during a roll and after a rollback, and joined the tuple arm
// to that table, answering a tuple only while the row that names it carries its node, which a
// build of the release before deletes with no retraction. Both test the row's marker, its node
// columns (store.GraphEdgesView). Once the build a rollback reaches writes everything a child's
// arms answer into the store, no row is written without its marker and none deleted without its
// retraction any more, and the step deletes that child's arms and probes, each child apart from the
// others, each under its own guard: dispatch://INFRA-1150's (AsksArmsRetirement) once that build
// writes the relation's kinds as tuples, and dispatch://INFRA-1253's (IssueTreeArmsRetirement) once
// it also writes a document's row values on its node and no reference past the store's bound.
// dispatch://INFRA-1152's arms and the arms over the kinds dispatch://INFRA-1149 moved are not
// among them.
//
// Each restates the view from its one source (store.GraphEdgesViewRetired), without its own arms
// and without those of every child whose arms the view no longer holds, so the two entries delete
// in either order and at either boot, and neither brings back what the other deleted. Which
// children's arms stand is one persisted fact, the rows of graph_edges_retirements
// (store.GraphEdgesChildren): each entry writes its child's row in the transaction that restates
// the view, reads the rows to know what the view still holds, and refs.BacklinkCounts reads the
// same rows to know whether to consult each child's markers, so the count agrees with the view
// whatever the step kept. Both run under retireLock, so two processes booting at once restate it
// once.
//
// Before it proves anything, each runs the graph store's backfill (graphStoreBackfill), which
// states on its node every row a build of the release before wrote with no marker, and marks it.
// Then it proves, on the shared pool and again in its transaction, that the view's answer stays as
// it is: every row the child's arms read carries its marker, so the arms answer nothing, and every
// current tuple of the child's kinds is named by such a row, so the probes keep every tuple the
// tuple arm reads. A row that names no node and a tuple whose row a build of the release before
// deleted - which no backfill brings in, since only the row could say whether the edge stands -
// each hold the arms, and the step logs each by its row or its tuple's sequence.

// AsksArmsRetirement deletes dispatch://INFRA-1150's arms of graph_edges: the two over asks, an
// ask's anchored_to and owned_by, and the one over ask_followers, its followed_by, with the tuple
// arm's probes of those two tables, whose classes then consult no projection row
// (store.GraphEdgesAsksRetired). Its kinds are that release's (AsksKinds).
var AsksArmsRetirement = Retirement{
	Name:     "the asks' old arms of graph_edges",
	Kinds:    AsksKinds,
	Stands:   graphEdgesChildStands(store.GraphEdgesAsksRetired),
	Backfill: graphStoreBackfill,
	Holds:    graphEdgesHolders(asksArmsHolders),
	Delete:   graphEdgesChildDelete(store.GraphEdgesAsksRetired),
}

// IssueTreeArmsRetirement deletes dispatch://INFRA-1253's arms of graph_edges: the arms over
// issues (child_of), issue_links (blocked_by), the artifacts that name no node (attached_to),
// components (part_of), component_depends (depends_on) and issue_component_members (affects), the
// tuple arm's probes of those five tables, whose classes then consult no projection row, and the
// artifact half of the arm over refs, a document's references while its row names no node and a
// far end past the store's bound, with the tuple arm's reads of the artifacts row and the refs row
// behind a document's mentions and its attached_to (store.GraphEdgesIssueTreeRetired).
//
// Its guard is StoresMovedKinds, the list of this release (dispatch://INFRA-1098), and not that
// release's own (IssueTreeKinds): the release before this one (dispatch://INFRA-1253's, at
// 810aa47bac) lists those kinds, but its upload writes a document's node with no row values
// (graphstore.ArtifactRow first appears in this release), and its refs writer still writes a far end
// past the store's bound, which only the deleted branch of the arm over refs answers. Deleted under
// that release's list, the view would answer no mentions and no attached_to of a document that
// release writes after a rollback. A build of this release or a later one writes both, and only
// such a build announces stores_moved, so the first boot of a build after this one deletes them.
// store.GraphEdgesIssueTreeRetired is one bit, so the whole entry waits.
//
// Once it has run, the tuple arm reads a document's uuid and its row's created_at off the
// document's node (graphstore.ArtifactRow) and a mention's date off its tuple, so beside the rows
// the arms read it holds the arms for each current mentions or part_of tuple out of a document
// whose node does not hold its row's id and created_at, whose row is gone or names no node, whose
// mention's refs row is gone or dates it otherwise, or whose part_of reaches an issue its row does
// not name; the backfill it runs first states each that a backfill can (backfillArtifactRows,
// backfillReferences). And no writer of refs writes a far end past the store's bound any more
// (refs.ReferenceTarget), but a build of the release before may have, and live production is no
// database a test reads: each refs row whose far end is past the bound holds the arms, named by
// its source and its length, so no edge is lost to the deleted branch in silence. The arm over
// refs for every source but a document stays, which is dispatch://INFRA-1152's half of that arm.
var IssueTreeArmsRetirement = Retirement{
	Name:     "the issue tree's old arms of graph_edges",
	Kinds:    StoresMovedKinds,
	Stands:   graphEdgesChildStands(store.GraphEdgesIssueTreeRetired),
	Backfill: graphStoreBackfill,
	Holds:    issueTreeHolds,
	Delete:   graphEdgesChildDelete(store.GraphEdgesIssueTreeRetired),
}

// issueTreeHolds is IssueTreeArmsRetirement's Holds: what its arms and reads still answer
// (issueTreeArmsHolders), and each refs row whose far end is past the store's bound
// (pastBoundReferences).
func issueTreeHolds(ctx context.Context, q queryer) ([]Holder, error) {
	holders, err := graphEdgesHolders(issueTreeArmsHolders)(ctx, q)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, pastBoundReferences)
	if err != nil {
		return nil, fmt.Errorf("count the references past the store's bound: %w", err)
	}
	past, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Holder, error) {
		var source, toKind string
		var length int64
		if err := row.Scan(&source, &toKind, &length); err != nil {
			return nil, err
		}
		return Holder{slog.String("past_bound_reference", source), slog.String("to_kind", toKind), slog.Int64("length", length)}, nil
	})
	if err != nil {
		return nil, fmt.Errorf("count the references past the store's bound: %w", err)
	}
	return append(holders, past...), nil
}

// pastBoundReferences is every refs row whose far end graphstore.AddressFits refuses, as its
// source, the kind of its far end and the far end's length in bytes: the far end itself, over a
// kilobyte, is not logged.
var pastBoundReferences = `
	select r.from_kind || ':' || r.from_id, r.to_kind, octet_length(r.to_id)::bigint
	from refs r
	where octet_length(r.to_id) > ` + strconv.Itoa(graphstore.MaxAddressBytes) + `
	order by r.from_kind, r.from_id, r.to_kind, r.to_id`

// graphEdgesRetired reads which children's arms graph_edges no longer holds: the children
// graph_edges_retirements names (store.GraphEdgesChildren).
func graphEdgesRetired(ctx context.Context, q queryer) (store.GraphEdgesRetired, error) {
	rows, err := q.Query(ctx, `select child from graph_edges_retirements`)
	if err != nil {
		return 0, fmt.Errorf("read which children's arms of graph_edges the step deleted: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, fmt.Errorf("read which children's arms of graph_edges the step deleted: %w", err)
	}
	var retired store.GraphEdgesRetired
	for _, child := range store.GraphEdgesChildren {
		if slices.Contains(names, child.Name) {
			retired |= child.Retired
		}
	}
	return retired, nil
}

// graphEdgesChildStands is a Stands that reports whether graph_edges still holds child's arms.
func graphEdgesChildStands(child store.GraphEdgesRetired) func(context.Context, queryer) (bool, error) {
	return func(ctx context.Context, q queryer) (bool, error) {
		retired, err := graphEdgesRetired(ctx, q)
		return retired&child == 0, err
	}
}

// graphEdgesChildDelete is a Delete that restates graph_edges without child's arms and without
// those of every child whose arms it no longer holds, read in the deleting transaction, under
// retireLock, and records child's row of graph_edges_retirements in the same transaction, so the
// count stops consulting child's markers at the commit that deletes its arms, and not before.
func graphEdgesChildDelete(child store.GraphEdgesRetired) func(context.Context, pgx.Tx) error {
	return func(ctx context.Context, tx pgx.Tx) error {
		retired, err := graphEdgesRetired(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, store.GraphEdgesViewRetired(retired|child)); err != nil {
			return fmt.Errorf("restate graph_edges without the arms: %w", err)
		}
		for _, known := range store.GraphEdgesChildren {
			if known.Retired != child {
				continue
			}
			if _, err := tx.Exec(ctx, `insert into graph_edges_retirements (child) values ($1) on conflict (child) do nothing`, known.Name); err != nil {
				return fmt.Errorf("record that graph_edges holds %s's arms no more: %w", known.Name, err)
			}
		}
		return nil
	}
}

// graphStoreBackfill runs the graph store's backfill as a boot of the server runs it
// (graphcensus.StartBackfill, BackfillRun.Passes): its record of the event log's horizon, bounded
// as a boot bounds it, then every pass, each over the rows its marker does not mark. A row it
// leaves because the store holds its node in another project, which no later run writes, fails
// nothing here: if the arms read that row, the proof names it.
func graphStoreBackfill(ctx context.Context, database *store.Store) error {
	run, err := graphcensus.StartBackfill(ctx, database, io.Discard, graphcensus.BackfillHorizonWaitBound)
	if err != nil {
		return fmt.Errorf("record the graph store backfill's horizon: %w", err)
	}
	if _, err := run.Passes(ctx, io.Discard); err != nil {
		if _, only := graphcensus.OnlyProjectMoved(err); !only {
			return fmt.Errorf("run the graph store backfill: %w", err)
		}
	}
	return nil
}

// graphEdgesHolders is a Holds that reads statement, whose rows are each a row an arm still
// answers or a tuple a probe still keeps out, as the text that names it and, for a tuple, its
// sequence, null for a row.
func graphEdgesHolders(statement string) func(context.Context, queryer) ([]Holder, error) {
	return func(ctx context.Context, q queryer) ([]Holder, error) {
		rows, err := q.Query(ctx, statement)
		if err != nil {
			return nil, fmt.Errorf("prove the store answers every edge of the arms: %w", err)
		}
		return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Holder, error) {
			var what string
			var seq *int64
			if err := row.Scan(&what, &seq); err != nil {
				return nil, err
			}
			if seq == nil {
				return Holder{slog.String("unmarked_row", what)}, nil
			}
			return Holder{slog.String("unprojected_tuple", what), slog.Int64("tuple_seq", *seq)}, nil
		})
	}
}

// currentTuplesWithoutRow is the current tuples, out of a node that is not gone, that the tuple
// arm reads under classes, a test over a graph_tuples row aliased t, and that probe, a test over
// the tuple aliased edge and its node aliased node, does not find a row for: the tuples the arm's
// probe keeps out. Each is named as its relation and its sequence, which graphEdgesHolders reads.
func currentTuplesWithoutRow(classes, probe string) string {
	return `
	select edge.from_kind || ':' || edge.from_id || ' -' || edge.kind || '-> ' || edge.to_kind || ':' || edge.to_id, edge.seq
	from (
		select distinct on (t.to_kind, t.to_id, t.kind, t.from_kind, t.from_id)
		       t.seq, t.from_kind, t.from_id, t.kind, t.to_kind, t.to_id, t.at, t.legacy_source_seq, t.asserted
		from graph_tuples t
		where t.to_kind <> 'value' and (` + classes + `)
		order by t.to_kind, t.to_id, t.kind, t.from_kind, t.from_id, t.seq desc
	) edge
	join graph_nodes node on node.kind = edge.from_kind and node.id = edge.from_id
	where edge.asserted and node.state <> 'gone' and not (` + probe + `)`
}

// asksArmsHolders is what holds dispatch://INFRA-1150's arms: each asks row an arm over asks reads
// that names no node, an anchored or an owned ask whose document stands; each ask_followers row
// that names none; and each current anchored_to, owned_by or followed_by tuple out of an ask whose
// row, or whose follower row of that session, does not carry the ask's node.
var asksArmsHolders = `
	select 'asks ' || k.id::text, null::bigint
	from asks k
	where k.node_kind is null
	  and (exists (select 1 from artifacts a where a.id = coalesce(k.block_artifact_id, (k.anchor->>'artifact_id')::uuid))
	       or exists (select 1 from artifacts a where a.id = k.artifact_id))
	union all
	select 'ask_followers ' || f.ask_id::text || ' ' || f.session_id, null
	from ask_followers f
	where f.node_kind is null
	union all` + currentTuplesWithoutRow(
	`t.from_kind = 'ask' and t.kind in ('anchored_to', 'owned_by', 'followed_by')`,
	`case edge.kind
	  when 'followed_by' then exists (
	    select 1 from ask_followers projected
	    where projected.node_kind = edge.from_kind and projected.node_address = edge.from_id
	      and projected.session_id = edge.to_id)
	  else exists (
	    select 1 from asks projected
	    where projected.node_kind = edge.from_kind and projected.node_address = edge.from_id)
	end`)

// issueTreeArmsHolders is what holds dispatch://INFRA-1253's arms: each row one of its arms reads
// that names no node - an issue with a parent, also one whose parent was moved in place
// (store.IssueParentMoved), a wait, a document on an issue, a component with a parent, also one
// whose parent was moved in place (store.ComponentParentMoved), a dependency, an issue's
// component, and a document's reference to a far end inside the store's bound whose document's row
// names no node or is gone - and each current tuple of an issue's child_of and needs, a component's
// part_of and depends_on, and an issue's affects whose row of the pair does not carry its node.
// Beside them, each current mentions and part_of tuple out of a document that the tuple arm's
// read of the store alone would answer otherwise than its read through the artifacts row and the
// refs row (store.GraphEdgesIssueTreeRetired) - the document's row gone, naming no node, or naming
// another id or created_at than its node holds (graphstore.ArtifactRow); a mention whose refs row is
// gone or dates it at another created_at or source_seq; a part_of to an issue the row does not
// name - which the backfill brings level where it can (backfillArtifactRows, backfillReferences).
var issueTreeArmsHolders = `
	select 'issues ' || i.key, null::bigint
	from issues i
	where i.parent_key is not null and (i.node_kind is null or ` + store.IssueParentMoved + `)
	union all
	select 'issue_links ' || l.issue_key || ' blocked_by ' || l.target_key, null
	from issue_links l
	where l.kind = 'blocked_by' and l.node_kind is null
	union all
	select 'artifacts ' || a.id::text, null
	from artifacts a
	where a.issue_key is not null and a.node_kind is null
	union all
	select 'components ' || c.project_key || '/' || c.id, null
	from components c
	where c.parent is not null and (c.node_kind is null or ` + store.ComponentParentMoved + `)
	union all
	select 'component_depends ' || d.project_key || '/' || d.from_id || ' depends_on ' || d.project_key || '/' || d.to_id, null
	from component_depends d
	where d.node_kind is null
	union all
	select 'issue_component_members ' || m.issue_key || ' affects ' || m.project_key || '/' || m.component_id, null
	from issue_component_members m
	where m.node_kind is null
	union all
	select 'refs ' || r.from_kind || ':' || r.from_id || ' -' || r.kind || '-> ' || r.to_kind || ':' || r.to_id, null
	from refs r
	where r.from_kind = 'artifact' and octet_length(r.to_id) <= ` + strconv.Itoa(graphstore.MaxAddressBytes) + `
	  and not exists (
	    select 1 from artifacts mentioning
	    where mentioning.id::text = r.from_id and mentioning.node_kind is not null)
	union all` + currentTuplesWithoutRow(
	`(t.from_kind = 'issue' and t.kind in ('child_of', 'needs') and t.to_kind = 'issue')
	 or (t.from_kind = 'component' and t.kind in ('part_of', 'depends_on') and t.to_kind = 'component')
	 or (t.from_kind = 'issue' and t.kind = 'affects' and t.to_kind = 'component')
	 or (t.from_kind = 'artifact' and t.kind = 'mentions')
	 or (t.from_kind = 'artifact' and t.kind = 'part_of' and t.to_kind = 'issue')`,
	`case
	  when edge.from_kind = 'issue' and edge.kind = 'child_of' then exists (
	    select 1 from issues projected
	    where projected.key = edge.from_id and projected.node_kind = edge.from_kind
	      and projected.node_address = edge.from_id and projected.parent_key = edge.to_id)
	  when edge.from_kind = 'issue' and edge.kind = 'needs' then exists (
	    select 1 from issue_links projected
	    where projected.issue_key = edge.from_id and projected.kind = 'blocked_by'
	      and projected.target_key = edge.to_id and projected.node_kind = edge.from_kind
	      and projected.node_address = edge.from_id)
	  when edge.from_kind = 'component' and edge.kind = 'part_of' then exists (
	    select 1 from components projected
	    where projected.project_key || '/' || projected.id = edge.from_id and projected.node_kind = edge.from_kind
	      and projected.node_address = edge.from_id and projected.project_key || '/' || projected.parent = edge.to_id)
	  when edge.from_kind = 'component' and edge.kind = 'depends_on' then exists (
	    select 1 from component_depends projected
	    where projected.project_key || '/' || projected.from_id = edge.from_id
	      and projected.project_key || '/' || projected.to_id = edge.to_id and projected.node_kind = edge.from_kind
	      and projected.node_address = edge.from_id)
	  when edge.from_kind = 'issue' and edge.kind = 'affects' then exists (
	    select 1 from issue_component_members projected
	    where projected.issue_key = edge.from_id and projected.project_key || '/' || projected.component_id = edge.to_id
	      and projected.node_kind = edge.from_kind and projected.node_address = edge.from_id)
	  else exists (
	    select 1 from artifacts named
	    where named.ref_key = edge.from_id and named.node_kind is not null
	      and named.id::text = node.attributes->>'`+graphstore.AttributeArtifactID+`'
	      and named.created_at = (node.attributes->>'`+graphstore.AttributeCreatedAt+`')::timestamptz
	      and case when edge.kind = 'mentions' then exists (
	            select 1 from refs stated
	            where stated.from_kind = edge.from_kind and stated.from_id = named.id::text
	              and stated.to_kind = edge.to_kind and stated.to_id = edge.to_id
	              and stated.created_at = edge.at and stated.source_seq is not distinct from edge.legacy_source_seq)
	          else named.issue_key = edge.to_id end)
	end`)
