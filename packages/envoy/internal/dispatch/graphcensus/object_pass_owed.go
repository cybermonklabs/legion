package graphcensus

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
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
// The tick's pass also catches up the rows of the inbox's caches that build leaves unmarked
// (CatchUpOwedRows), so once a run of the backfill of this build's marking rule or a later one has
// recorded its horizon (backfillRan) the read answers true too for an event that leaves such a row
// owed (rollOwedByEvents): a person's
// user_thread_state.updated, user_state.updated or user_agent_state.updated, which that build's
// read marks, dismissals, clears and snoozes append naming the person, where a user_threads row or
// a row of the five other read-state tables of that person is owed; an ask's ask.opened, ask.edited
// or ask.answered, a comment's comment.created, comment.answered or comment.edited and a message's
// message.created or message.answered, where that item's blocks, their links or its citations are
// owed, an ask's or a message's row, a comment's row where the comment names its node (the pointer
// pass and the pass past the store's bound), a message's attempts, or a comment's delivery routes
// (comment_mentions), or, for an answer, a user_threads row of the ask's thread; and
// a comment.delivery, a message.delivery or a message.accepted, the receipt of an attempt its
// sender settled or its session accepted, where an attempt row of that item is owed. Its claims
// append no event, so only a pass started for another reason reaches an attempt one moved. And
// whatever the events are, it answers true while the queue of the events that build's outbox
// recorded (events_published_owed) holds one: that record appends no event. So it does while the
// queue of that build's moves of who an ask waits on and its edits of a comment or an issue
// message (events_inbox_owed) holds one: the event it queues names no row the catch-up's other
// passes owe;
// and while a document that build stated the blocks of without their identity mentions waits for
// the block identity pass (blockIdentitiesOwed): its fence write appends no event.
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
// it (objectKinds), the removed documents' nodes that stand orphaned, and the inbox cache rows the
// catch-up owes (rollOwedByEvents). Read per event instead, the planner hashes each kind's rows and
// nodes whole.
var eventsOweObjectPass = `
	with named as (
		select coalesce(array_agg(e.issue_key) filter (where e.type = 'issue.created'), '{}') as issues,
		       coalesce(array_agg(e.payload->'artifact'->>'id') filter (where e.type = 'artifact.created'), '{}') as documents,
		       coalesce(array_agg(e.payload->>'id') filter (where e.type = 'ask.opened'), '{}') as asks,
		       coalesce(array_agg(e.payload->>'id') filter (where e.type in ('message.created', 'message.answered')), '{}') as messages,
		       coalesce(array_agg(e.payload->'artifact'->>'ref_key') filter (where e.type = 'artifact.removed'), '{}') as removed,
		       coalesce(array_agg(e.payload->>'login') filter (where e.type in ('user_thread_state.updated', 'user_state.updated',
		                                                                         'user_agent_state.updated')), '{}') as logins,
		       coalesce(array_agg(` + store.CastableUUID(`e.payload->>'id'`) + `) filter (where e.type = 'ask.answered'), '{}') as answered,
		       coalesce(array_agg(` + store.CastableUUID(`e.payload->>'id'`) + `) filter (where e.type in ('ask.opened', 'ask.edited', 'ask.answered')), '{}') as asked,
		       coalesce(array_agg(` + store.CastableUUID(`e.payload->>'id'`) + `) filter (where e.type in ('comment.created', 'comment.answered',
		                                                                                          'comment.edited')), '{}') as commented,
		       coalesce(array_agg(` + store.CastableUUID(`e.payload->>'id'`) + `) filter (where e.type in ('message.created', 'message.answered')), '{}') as messaged,
		       coalesce(array_agg(` + store.CastableUUID(`e.payload->>'comment_id'`) + `) filter (where e.type = 'comment.delivery'), '{}') as attempted,
		       coalesce(array_agg(` + store.CastableUUID(`e.payload->>'message_id'`) + `) filter (where e.type in ('message.delivery', 'message.accepted')), '{}') as delivered
		from events e
		where e.id > $1 and e.id <= $2
		  and e.type in ('issue.created', 'artifact.created', 'ask.opened', 'message.created', 'message.answered', 'artifact.removed',
		                 'user_thread_state.updated', 'user_state.updated', 'user_agent_state.updated', 'ask.edited', 'ask.answered',
		                 'comment.created', 'comment.answered', 'comment.edited', 'comment.delivery', 'message.delivery', 'message.accepted')
	)
	select exists (select 1 from named c join issues i on i.key = any(c.issues) where ` + unmarked(graphstore.KindIssue, "i.key") + `)
	    or exists (select 1 from named c join artifacts a on a.issue_key = any(c.issues)
		where octet_length(a.ref_key) <= ` + strconv.Itoa(graphstore.MaxAddressBytes) + ` and ` + unmarkedDocument("a") + `)
	    or exists (select 1 from named c join artifacts a on a.id::text = any(c.documents)
		where octet_length(a.ref_key) <= ` + strconv.Itoa(graphstore.MaxAddressBytes) + ` and ` + unmarkedDocument("a") + `)
	    or exists (select 1 from named c join asks k on k.id::text = any(c.asks) where ` + unmarked(graphstore.KindAsk, "k.id::text") + `)
	    or exists (select 1 from named c join messages m on m.id::text = any(c.messages) where ` + unmarked(graphstore.KindMessage, "m.id::text") + `)
	    or exists (select 1 from named c join graph_nodes n on n.kind = '` + graphstore.KindArtifact + `' and n.id = any(c.removed)
		where ` + orphanedDocumentNode("n") + `)
	    or (` + backfillRan + ` and (
		` + rollOwedByEvents() + `))`

// rollOwedByEvents is eventsOweObjectPass's test of the inbox cache rows the events name that the
// block-index tick's catch-up owes (rollPasses), each kind read by its own owed test, the pass's,
// narrowed to the rows the arrays of named name, through the key or the index each kind's rows are
// read by; and of the queues of the events an older build's outbox recorded, which no event names
// (publishedDeliveriesQueued), and of the events of that build's writes that change no row's marker
// (inboxEventsQueued), each read through its key.
// kept for one release: inbox-roll-catch-up
func rollOwedByEvents() string {
	named := func(array, as string) string { return `any((select named.` + array + ` from named)::` + as + `[])` }
	terms := []string{
		`exists (select 1 ` + userThreadsOwed + ` and ` + threadItemsStored + `
		  and (u.login = ` + named("logins", "text") + ` or u.thread_id = ` + named("answered", "uuid") + `))`,
		`exists (select 1 ` + asksOwed + ` and k.id = ` + named("asked", "uuid") + `)`,
		`exists (select 1 ` + messagesOwed + ` and m.id = ` + named("messaged", "uuid") + `)`,
		`exists (select 1 ` + pastBoundMessages.owed + ` and m.id = ` + named("messaged", "uuid") + `)`,
		`exists (select 1 ` + commentPointersOwed + ` and c.id = ` + named("commented", "uuid") + `)`,
		`exists (select 1 ` + pastBoundComments.owed + ` and c.id = ` + named("commented", "uuid") + `)`,
		`exists (select 1 ` + commentAttempts.owed() + ` and d.comment_id = ` + named("attempted", "uuid") + `)`,
		`exists (select 1 ` + commentMentionsOwed + ` and m.comment_id = ` + named("commented", "uuid") + `)`,
		`exists (select 1 ` + messageAttempts.owed() + ` and (d.message_id = ` + named("delivered", "uuid") +
			` or d.message_id = ` + named("messaged", "uuid") + `))`,
		// kept for one release: events-published-owed
		`exists (select 1 ` + publishedDeliveriesQueued + `)`,
		// kept for one release: events-inbox-owed
		`exists (select 1 ` + inboxEventsQueued + `)`,
		// kept for one release: block-identities-stated
		`exists (select 1 ` + blockIdentitiesOwed + `)`,
	}
	for _, table := range readStateTables {
		terms = append(terms, `exists (select 1 `+table.owed()+` and user_threads_login(r.login) = `+named("logins", "text")+`)`)
	}
	items := map[string]string{graphstore.KindComment: "commented", graphstore.KindMessage: "messaged", graphstore.KindAsk: "asked"}
	for _, kind := range inboxItemKinds {
		terms = append(terms,
			`exists (select 1 `+kind.blocksOwed()+` and b.`+kind.column+` = `+named(items[kind.kind], "uuid")+`)`,
			`exists (select 1 `+kind.linksOwed()+` and r.`+kind.column+` = `+named(items[kind.kind], "uuid")+`)`,
			`exists (select 1 `+kind.citationsOwed()+` and r.from_id = `+named(items[kind.kind], "text")+`)`)
	}
	return strings.Join(terms, "\n\t\tor ")
}
