package graphcensus

import (
	"context"
	"fmt"
	"strconv"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
)

// EventsOweObjectPass answers whether an event with an id in (after, through] leaves the store
// what the object pass cures: the create of an issue, a document, an ask or a message whose row
// the backfill's marker does not mark (unmarked, unmarkedDocument), which WriteOwedObjectNodes
// writes the node of, or the removal of a document whose node stands orphaned behind no row
// (orphanedDocumentNode), which StateOrphanedGone states gone. Docs' block-index tick reads it over
// the events it moves its cursor past, on every tick, and owes the pass where it answers true.
//
// A build before this one may serve a whole roll while no tick of this build runs, since the two
// share one block-index lease and a process whose ticks come first in each period takes every
// period, so none of the roll's triggers holds on this build's next tick. What that build did
// meanwhile is in the event log: each create of these kinds appends its kind's create event in
// the transaction that writes the row - issue.created for an issue and its spec, which names the
// issue and no document, artifact.created for a document, whose payload carries the artifact,
// ask.opened for an ask, whose payload is the ask, and message.created for a message, or
// message.answered for a reply to one, a session's reply included, whose payload is the message -
// and its removal of a document appends artifact.removed, whose payload carries the removed
// artifact and its address. A conversation's file is created with no event, so only a pass started
// for another reason reaches it: the pass reads the four tables whole.
//
// The read collects the ids the events name, kind by kind, and reads only those rows and their
// nodes, through each row's unique key and the node's, so a tick whose events name only rows that
// hold their nodes, and removals this build's own removal stated gone, reads no table whole and
// writes nothing.
func EventsOweObjectPass(ctx context.Context, q graphstore.Queryer, after, through int64) (bool, error) {
	var owed bool
	if err := q.QueryRow(ctx, eventsOweObjectPass, after, through).Scan(&owed); err != nil {
		return false, fmt.Errorf("read whether an event after %d leaves a row with no node or a node with no row: %w", after, err)
	}
	return owed, nil
}

// eventsOweObjectPass is EventsOweObjectPass's read: the ids each kind's events in ($1, $2] name,
// as one row of arrays, so each kind's rows are read by `= any` on their key, and of those the
// rows the marker does not mark, a document past the store's bound left out as the backfill leaves
// it (objectKinds), and the removed documents' nodes that stand orphaned. Read per event instead,
// the planner hashes each kind's rows and nodes whole.
var eventsOweObjectPass = `
	with named as (
		select coalesce(array_agg(e.issue_key) filter (where e.type = 'issue.created'), '{}') as issues,
		       coalesce(array_agg(e.payload->'artifact'->>'id') filter (where e.type = 'artifact.created'), '{}') as documents,
		       coalesce(array_agg(e.payload->>'id') filter (where e.type = 'ask.opened'), '{}') as asks,
		       coalesce(array_agg(e.payload->>'id') filter (where e.type in ('message.created', 'message.answered')), '{}') as messages,
		       coalesce(array_agg(e.payload->'artifact'->>'ref_key') filter (where e.type = 'artifact.removed'), '{}') as removed
		from events e
		where e.id > $1 and e.id <= $2
		  and e.type in ('issue.created', 'artifact.created', 'ask.opened', 'message.created', 'message.answered', 'artifact.removed')
	)
	select exists (select 1 from named c join issues i on i.key = any(c.issues) where ` + unmarked(graphstore.KindIssue, "i.key") + `)
	    or exists (select 1 from named c join artifacts a on a.issue_key = any(c.issues)
		where octet_length(a.ref_key) <= ` + strconv.Itoa(graphstore.MaxAddressBytes) + ` and ` + unmarkedDocument("a") + `)
	    or exists (select 1 from named c join artifacts a on a.id::text = any(c.documents)
		where octet_length(a.ref_key) <= ` + strconv.Itoa(graphstore.MaxAddressBytes) + ` and ` + unmarkedDocument("a") + `)
	    or exists (select 1 from named c join asks k on k.id::text = any(c.asks) where ` + unmarked(graphstore.KindAsk, "k.id::text") + `)
	    or exists (select 1 from named c join messages m on m.id::text = any(c.messages) where ` + unmarked(graphstore.KindMessage, "m.id::text") + `)
	    or exists (select 1 from named c join graph_nodes n on n.kind = '` + graphstore.KindArtifact + `' and n.id = any(c.removed)
		where ` + orphanedDocumentNode("n") + `)`
