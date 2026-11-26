package graphcensus

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// roleRow is one role the store census holds to a node (addressedRoles) whose node or page the
// store does not hold: its name, the first event that named it and that event's time, or the
// sentinel and the run's own time for a role only a row names, and which of its two nodes the
// store lacks.
type roleRow struct {
	name                 string
	event                int64
	at                   time.Time
	needsRole, needsPage bool
}

// backfillRoles writes the node of every role the store census holds to one and that the store
// holds no node of, its page and the page's part_of the role, as the publisher writes them for a
// role an event it publishes names (outbox.deliveredGraph). Its input is addressedRoles, the
// census's own role row side, so the roles this gives a node are exactly the roles the census
// holds to one: every route, message target and comment mention a database written before the
// publisher wrote them holds, in a row now or in an event.
//
// A role, its page and that part_of have no projection row, so there is no row to name the node
// on and no fence to take. A role the store already holds both nodes of is left alone, and the
// write is safe beside a running publisher and on a second run: a node another write made is
// written `do nothing`, and the part_of is a claim, appended only where the page is not part of
// the role already (graphstore.WriteArms). The writer is the run (backfillActor), and the cause
// and the time are the first event that named the role, or the sentinel and the run's own time
// for a role only a row names, which no event dates.
//
// No arm of graph_edges answers a role, so its Complete switches nothing off: it reads whether
// every role the census names holds its node and its page.
func backfillRoles(ctx context.Context, database *store.Store) (KindBackfill, error) {
	done := KindBackfill{Kind: graphstore.KindRole}
	missing, err := readRoleRows(ctx, database)
	if err != nil {
		return done, err
	}
	for start := 0; start < len(missing); start += backfillBatch {
		batch := missing[start:min(start+backfillBatch, len(missing))]
		nodes, tuples, err := writeRoleBatch(ctx, database, batch)
		if err != nil {
			return done, err
		}
		done.Rows += int64(len(batch))
		done.Nodes += nodes
		done.Tuples += tuples
	}
	left, err := readRoleRows(ctx, database)
	if err != nil {
		return done, err
	}
	done.Remaining = int64(len(left))
	done.Complete = done.Remaining == 0
	return done, nil
}

// readRoleRows reads every role addressedRoles names whose node or page the store does not hold
// and whose address fits the store's bound, in name order. A role name past the bound is no node
// of the store, and the page of a role whose name leaves no room for the `role/` prefix is past
// the bound too: each is owed only where its own address fits, so a role the writers leave out on
// purpose is neither written here nor counted as remaining (outbox.deliveredGraph).
func readRoleRows(ctx context.Context, database *store.Store) ([]roleRow, error) {
	rows, err := database.Pool.Query(ctx, `
		select addressed.name, coalesce(addressed.event, $1), coalesce(e.created_at, now()),
		       role.id is null and octet_length(addressed.name) <= $4,
		       page.id is null and octet_length($2::text || '/' || addressed.name) <= $4
		from (`+addressedRoles+`) addressed
		left join events e on e.id = addressed.event
		left join graph_nodes role on role.kind = $2::text and role.id = addressed.name
		left join graph_nodes page on page.kind = $3::text and page.id = $2::text || '/' || addressed.name
		where (role.id is null and octet_length(addressed.name) <= $4)
		   or (page.id is null and octet_length($2::text || '/' || addressed.name) <= $4)
		order by addressed.name
	`, graphstore.NoEvent, graphstore.KindRole, graphstore.KindPage, graphstore.MaxAddressBytes)
	if err != nil {
		return nil, fmt.Errorf("read the roles the store holds no node of: %w", err)
	}
	defer rows.Close()
	var missing []roleRow
	for rows.Next() {
		var row roleRow
		if err := rows.Scan(&row.name, &row.event, &row.at, &row.needsRole, &row.needsPage); err != nil {
			return nil, fmt.Errorf("read a role the store holds no node of: %w", err)
		}
		missing = append(missing, row)
	}
	return missing, rows.Err()
}

// writeRoleBatch writes one batch's role and page nodes and each page's part_of its role, in one
// statement. It answers the nodes it wrote and, as the upper bound the report states, the tuples:
// the two base attributes of each node and one part_of per page, each of which the write appends
// only where the store does not hold it already.
func writeRoleBatch(ctx context.Context, database *store.Store, batch []roleRow) (int64, int64, error) {
	var nodes []graphstore.Node
	claims := make([]graphstore.Claim, 0, len(batch))
	for _, row := range batch {
		// A role's facts as the publisher states them (graphstore.RoleFacts), each address held to
		// the store's bound on its own: a role whose page is past the bound has no page node and no
		// part_of, a claim that would key an address the store cannot hold. Of the two nodes the
		// batch writes the ones the store lacks (readRoleRows), and leaves the other to the write
		// that made it; the part_of is a claim, which appends nothing where the page is part of
		// the role already.
		stated := len(nodes)
		nodes, claims = graphstore.RoleFacts(nodes, claims, row.name, backfillActor, row.at, row.event)
		owed := slices.DeleteFunc(nodes[stated:], func(node graphstore.Node) bool {
			return (node.Kind == graphstore.KindRole && !row.needsRole) || (node.Kind == graphstore.KindPage && !row.needsPage)
		})
		nodes = nodes[:stated+len(owed)]
	}
	arms, params, err := graphstore.WriteArms(1, nodes, nil, claims)
	if err != nil {
		return 0, 0, err
	}
	if _, err := database.Pool.Exec(ctx, "with "+arms+"\nselect 1", params...); err != nil {
		return 0, 0, fmt.Errorf("write the nodes of a role batch: %w", err)
	}
	return int64(len(nodes)), 2*int64(len(nodes)) + int64(len(claims)), nil
}
