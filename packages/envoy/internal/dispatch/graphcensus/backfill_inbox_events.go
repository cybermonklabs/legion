package graphcensus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/inboxwrite"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// --- the writes of an older build that change no row this release marks ---------------------------

// Two kinds of write a build before this one makes change what the store owes a row this release
// has already stated, and leave that row's marker as it stood, so no pass over the row's marker
// reads them again:
//
//   - a write that moves who an open ask waits on (the spec's R2): a change of its issue's
//     assignee (patchIssue's issue.updated naming assignee among the fields it changed), a hand-on
//     (putAskAskedOf's ask.asked_of), a move of its project's default assignee
//     (putProjectDefaultAssignee's settings.project_default_assignee.updated), and the restore of
//     an ask that was not open - an issue's reopen restoring the asks its close withdrew, or a
//     settlement restoring an ask it retracted - each appending the ask.opened that records it.
//     This release's own writer moves the ask's delivered_to and participates_in as it writes
//     (inboxwrite.QueueMoveWaiting); that build writes the issue's, the ask's or the project's
//     row and nothing of the store, and the ask's marker (asks.tuples_stored_at) is the moment of
//     the writes the store holds of its create and its answer, which none of these touches;
//   - an edit of a comment (comment.edited) or of an issue message (message.edited), whose new
//     text can name a person (R5): this release's edit states the mentions tuple to each person
//     its text newly names and that person's participates_in in the item's thread (api's
//     editComment and editMessage), and that build stores no row for such a mention
//     (refs.TargetOf); a comment's marker (comments.tuples_stored) marks its pointers and a
//     message's (messages.node_kind) its node and pointers, which no edit moves.
//
// The trigger of migration 20261122100123 queues the event of each such write in
// events_inbox_owed as that build appends it, gated on the store's marker writer setting as the
// marker triggers are, so this release's writers, which state these tuples themselves, queue
// nothing. The backfill and the block-index tick's catch-up both take the queue (inboxEventsPass,
// rollPasses), one batch at a time, and each event is taken once: the batch that states it deletes
// its row.

// inboxEventsKind is the kind of the pass over the queue of an older build's events
// (inboxEventsPass).
const inboxEventsKind = "inbox_events"

// The queue the trigger of migration 20261122100123 fills (events_inbox_owed), read through its key.
//
// kept for one release: events-inbox-owed
var (
	inboxEventsQueued      = `from events_inbox_owed q`
	inboxEventsQueuedBatch = `select q.event_id::text ` + inboxEventsQueued + `
		where q.event_id > $1::text::bigint order by q.event_id limit $2`
)

// inboxEventsPass gives each event of the queue what this release's writer of the same write
// states (writeQueuedInboxEvents). The backfill runs it after the asks pass, which states an ask
// that build created as it stands when it runs, and after the messages pass and the comment
// pointer pass, which state a message and a comment that build wrote from the text it holds when
// they run.
func inboxEventsPass() inboxPass {
	return inboxPass{
		kind:      inboxEventsKind,
		batch:     inboxEventsQueuedBatch,
		start:     []string{"0"},
		write:     writeQueuedInboxEvents,
		remaining: inboxEventsQueued,
	}
}

// queuedEvent is one event of the queue as a batch reads it: its id, type, owners and actor, its
// moment, the id its payload names (an ask's, a comment's or a message's), and for an edit of a
// comment or a message the text the edit wrote and whether the event carried all of it
// (model.EventBody).
type queuedEvent struct {
	id                           int64
	kind, issue, project, target string
	actor                        model.Actor
	at                           time.Time
	body                         string
	truncated                    bool
}

// queuedEventsRead reads the events of the queued ids $1 still queued, in id order. A payload id
// that is no uuid names no ask, comment or message and is read as empty, which the batch states
// nothing for.
//
// kept for one release: events-inbox-owed
const queuedEventsRead = `
	select e.id, e.type, coalesce(e.issue_key, ''), coalesce(e.project_key, ''),
	       case when pg_input_is_valid(coalesce(e.payload->>'id', ''), 'uuid') then e.payload->>'id' else '' end,
	       e.actor, e.created_at,
	       case when e.type in ('comment.edited', 'message.edited') then coalesce(e.payload->>'body', '') else '' end,
	       e.type in ('comment.edited', 'message.edited') and coalesce(e.payload->>'body_truncated', 'false') = 'true'
	from events_inbox_owed q join events e on e.id = q.event_id
	where q.event_id = any($1::text[]::bigint[])
	order by e.id`

// writeQueuedInboxEvents states what each queued event of keys owes the store, under that event,
// at its moment and made by its actor, as this release's writer of the same write states it, in
// the lock order written on api/inbox_read.go's markInboxReadThreads:
//
//  1. the owner of every ask, comment and message the events name and of every issue whose
//     assignee they moved (events.LockOwnerRows), each once, in ascending owner key: what this
//     release's writer of each write takes first;
//  2. shared, the owner of each project whose default the batch's asks fall through to, and of
//     each project whose default an event moved, in ascending key (inboxwrite.HoldProjectDefault),
//     as a write that reads the default to decide a delivery takes it; a move of the default this
//     release makes takes it exclusive, and so waits for the batch or the batch for it;
//  3. the queue rows of the batch still queued, `for no key update` in ascending id, so a second
//     taker of the queue - the backfill beside the tick - waits and then finds them gone;
//  4. each move in event order (inboxwrite.QueueMoveWaiting), which locks the open asks it may
//     move `for no key update` in ascending id and then appends, in its own statement, the tuples
//     that move them to whoever each waits on now. Two moves of one batch lock their asks one
//     after the other, each in ascending id, and every move but a default's names its asks by an
//     owner the batch holds (step 1): a writer that would lock asks of two of them takes the owner
//     of one first, or the project's owner exclusive for a default's (step 2), and waits there
//     holding none of them;
//  5. each edit's mentions and their participation (graphstore.ToPerson, through writeRelations);
//  6. the delete of the queue rows it took.
//
// A move is held to the asks whose create the store holds the tuples of (tuples_stored_at set): an
// ask that build created is the asks pass's, which states its create to the person it waits on
// when that pass runs, so a move of it before then has nothing to move, and one after it finds
// that pass's arm where it stands. And a move is held to the asks no later queued event moves
// again (laterMoveOf): the move reads who each ask waits on now, after every move that build made,
// so it is the last of them that states the ask's arm, under its own event and by its own actor,
// as this release's writer would have stated that last move - a first move read now would state
// the last one's person under the first one's event and actor, and could take that person for the
// mover of an ask they did not move onto themselves (inboxwrite.OwnMove). Each move appends
// nothing where the arm stands already to the person the ask waits on, so a queued event taken a
// second time states nothing.
//
// An edit is stated only for an item whose node the store holds and whose marker says the store
// holds its pointers: a comment whose tuples_stored is set, and a message whose row names its node
// (olderEditedItems). The comment pointer pass and the messages pass state an item that build
// wrote from the text it holds when that pass runs, the edits before it among it. Its text is the
// one the edit's event carries, and the item's own where the event carried only the first part of
// it.
//
// It answers the rows it took off the queue and, of the tuples it handed the store, the edits'
// alone: a move's tuples go in through a statement whose count nothing reads back
// (inboxwrite.QueueMoveWaiting) and are not counted, so the upper bound the pass reports
// (inboxWrote) leaves the moves' tuples out.
func writeQueuedInboxEvents(ctx context.Context, tx pgx.Tx, keys [][]string) (inboxWrote, error) {
	ids := column(keys, 0)
	rows, err := tx.Query(ctx, queuedEventsRead, ids)
	if err != nil {
		return inboxWrote{}, fmt.Errorf("read a batch of the events an older build queued: %w", err)
	}
	var read []queuedEvent
	var held queuedEvent
	var actor []byte
	if _, err := pgx.ForEachRow(rows, []any{&held.id, &held.kind, &held.issue, &held.project, &held.target, &actor, &held.at,
		&held.body, &held.truncated}, func() error {
		// Each row's actor is decoded into an Actor of its own: held serves every row, and a
		// decode into held.actor would keep a member an earlier event's actor carried and this
		// one lacks, and would write through the pointers of that earlier event's copy in read.
		var made model.Actor
		if err := json.Unmarshal(actor, &made); err != nil {
			return fmt.Errorf("read the actor of event %d: %w", held.id, err)
		}
		held.actor = made
		read = append(read, held)
		return nil
	}); err != nil {
		return inboxWrote{}, fmt.Errorf("read a batch of the events an older build queued: %w", err)
	}
	if len(read) == 0 {
		return inboxWrote{}, nil
	}
	var askIDs, commentIDs, messageIDs, issues, projects []string
	owners := events.OwnerRows{}
	for _, event := range read {
		switch event.kind {
		case "ask.asked_of", "ask.opened":
			if event.target != "" {
				askIDs = append(askIDs, event.target)
			}
		case "comment.edited":
			if event.target != "" {
				commentIDs = append(commentIDs, event.target)
			}
		case "message.edited":
			if event.target != "" {
				messageIDs = append(messageIDs, event.target)
			}
		case "issue.updated":
			if event.issue != "" {
				issues = append(issues, event.issue)
				owners.Add(event.issue, "")
			}
		case "settings.project_default_assignee.updated":
			if event.project != "" {
				projects = append(projects, event.project)
			}
		}
	}
	if err := readOwnersInto(ctx, tx, owners, askOwners, askIDs); err != nil {
		return inboxWrote{}, err
	}
	if err := readOwnersInto(ctx, tx, owners, commentOwners, commentIDs); err != nil {
		return inboxWrote{}, err
	}
	if err := readOwnersInto(ctx, tx, owners, messageOwners, messageIDs); err != nil {
		return inboxWrote{}, err
	}
	if err := events.LockOwnerRows(ctx, tx, owners); err != nil {
		return inboxWrote{}, err
	}
	defaults, err := readKeys(ctx, tx, 1, `
		select distinct fallen.project from (
			select coalesce(i.project_key, ar.project_key) as project
			from asks k
			left join issues i on i.key = k.issue_key and i.assignee is null
			left join artifacts ar on ar.id = k.artifact_id and ar.issue_key is null
			where k.id = any($1::uuid[])
			union all
			select i.project_key from issues i where i.key = any($2::text[]) and i.assignee is null
			union all
			select unnest($3::text[])
		) fallen
		where fallen.project is not null
		order by 1
	`, askIDs, issues, projects)
	if err != nil {
		return inboxWrote{}, fmt.Errorf("read the projects whose default a batch of queued events reads: %w", err)
	}
	for _, project := range column(defaults, 0) {
		if err := inboxwrite.HoldProjectDefault(ctx, tx, project); err != nil {
			return inboxWrote{}, err
		}
	}
	// kept for one release: events-inbox-owed
	claimed, err := readKeys(ctx, tx, 1, `
		select event_id::text from events_inbox_owed where event_id = any($1::text[]::bigint[]) order by event_id for no key update
	`, ids)
	if err != nil {
		return inboxWrote{}, fmt.Errorf("take a batch of the events an older build queued: %w", err)
	}
	taken := make(map[int64]bool, len(claimed))
	for _, key := range column(claimed, 0) {
		id, err := strconv.ParseInt(key, 10, 64)
		if err != nil {
			return inboxWrote{}, fmt.Errorf("read a queued event's id %q: %w", key, err)
		}
		taken[id] = true
	}
	var wrote inboxWrote
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		for _, event := range read {
			if !taken[event.id] {
				continue
			}
			move, moves := movedBy(event)
			if !moves {
				continue
			}
			if err := inboxwrite.QueueMoveWaiting(b, move.Narrowed(laterMoveOf(event.id)), event.id); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return inboxWrote{}, fmt.Errorf("move who the asks a batch of queued events names wait on: %w", err)
	}
	for _, event := range read {
		edited, edits := olderEditedItems[event.kind]
		if !taken[event.id] || !edits || event.target == "" {
			continue
		}
		stated, err := writeOlderEdit(ctx, tx, edited, event)
		if err != nil {
			return inboxWrote{}, err
		}
		wrote.tuples += stated
	}
	// kept for one release: events-inbox-owed
	if _, err := tx.Exec(ctx, `delete from events_inbox_owed where event_id = any($1::text[]::bigint[])`, column(claimed, 0)); err != nil {
		return inboxWrote{}, fmt.Errorf("take a batch of events off the queue of an older build's events: %w", err)
	}
	wrote.rows = int64(len(claimed))
	return wrote, nil
}

// movedBy is the move of who asks wait on that event records, as this release's writer of the
// same write names it, and false for an event that moves nothing.
func movedBy(event queuedEvent) (inboxwrite.Move, bool) {
	switch {
	case event.kind == "issue.updated" && event.issue != "":
		return inboxwrite.Reassigned(event.issue, event.at, event.actor), true
	case event.kind == "ask.asked_of" && event.target != "":
		return inboxwrite.HandedOn(event.target, event.at, event.actor), true
	case event.kind == "ask.opened" && event.target != "":
		return inboxwrite.Restored(event.target, event.at, event.actor), true
	case event.kind == "settings.project_default_assignee.updated" && event.project != "":
		return inboxwrite.DefaultMoved(event.project, event.at, event.actor), true
	}
	return inboxwrite.Move{}, false
}

// laterMoveOf is the test a queued move of event holds each of its asks to (inboxwrite.Move
// .Narrowed): the store holds the tuples of the ask's create (tuples_stored_at set), and no event
// queued after event moves the ask again - a hand-on or a restore naming it, a change of its
// issue's assignee where nobody has it handed on, or a move of the default of the project it
// falls through to where nobody has it handed on, read as the asks, issues and documents stand,
// as the move itself reads them.
func laterMoveOf(event int64) string {
	// kept for one release: events-inbox-owed
	return `narrowed.tuples_stored_at is not null and not exists (
		select 1 from events_inbox_owed later
		join events moved on moved.id = later.event_id
		where later.event_id > ` + strconv.FormatInt(event, 10) + `
		  and case moved.type
		      when 'ask.asked_of' then moved.payload->>'id' = narrowed.id::text
		      when 'ask.opened' then moved.payload->>'id' = narrowed.id::text
		      when 'issue.updated' then moved.issue_key = narrowed.issue_key and narrowed.asked_of is null
		      when 'settings.project_default_assignee.updated' then narrowed.asked_of is null and (
		          exists (select 1 from issues fallen
		                  where fallen.key = narrowed.issue_key and fallen.assignee is null and fallen.project_key = moved.project_key)
		          or exists (select 1 from artifacts fallen
		                     where fallen.id = narrowed.artifact_id and narrowed.issue_key is null and fallen.project_key = moved.project_key))
		      else false end)`
}

// olderEdited is how writeOlderEdit reads the item an older build's edit names: the item kind its
// text is indexed as, which is also its node's kind, the table that holds its row, and the test
// that row holds to its marker beside naming its node.
type olderEdited struct {
	item          refs.ItemKind
	table, marked string
}

// olderEditedItems is olderEdited by the type of the edit's event: a comment's edit
// (comment.edited), of a comment whose pointers the store holds (comments.tuples_stored), and an
// issue message's (message.edited), of a message whose row names its node (messages.node_kind),
// which the messages pass names once it has stated the message's pointers. The read joins the
// node the row names, so an item the store holds no node of is read as no row.
//
// kept for one release: events-inbox-owed
var olderEditedItems = map[string]olderEdited{
	"comment.edited": {item: refs.ItemComment, table: "comments", marked: "x.tuples_stored"},
	"message.edited": {item: refs.ItemMessage, table: "messages", marked: "x.node_kind is not null"},
}

// writeOlderEdit states what an older build's edit of a comment or of an issue message owes the
// store, as this release's edit states it (api's editComment, editMessage): the mentions tuple out
// of the item's node to each identity the edit's text names - a person, an agent's session or a
// role - whom the store holds no mention of out of that node yet (refs.ItemIdentityMentions), made
// by the node's maker at the edit's moment, and each such person's participates_in in the item's
// thread (inboxwrite.MentionParticipation), made by the editor at that moment, all under the edit's
// event, and no delivery: an edit delivers nothing. A person the text names whose mention the store
// holds already is owed the same where they take no part in the thread: a pass that no event
// records can state the mention before this one runs - a re-index of the item's text at the
// sentinel (refs.ReindexScopeSource, refs.ReindexOwed), which states no participation, since a
// participation stands under the event of a mention that explains it (the census's
// mentioned_alone) - so the participation R4 owes is decided on the participation itself
// (takingPart), and its mention is stated again under the edit's event, beside it. An item whose
// marker does not mark it yet (olderEditedItems) is the comment pointer pass's or the messages
// pass's, which reads the text the item holds then. It answers how many tuples it handed the store.
func writeOlderEdit(ctx context.Context, tx pgx.Tx, edited olderEdited, event queuedEvent) (int64, error) {
	var body, visibility string
	var madeBy []byte
	var standingKinds, standingIDs []string
	err := tx.QueryRow(ctx, `
		select coalesce(x.body, ''), n.visibility, n.made_by, coalesce(held.kinds, '{}'), coalesce(held.ids, '{}')
		from `+edited.table+` x
		join graph_nodes n on n.kind = x.node_kind and n.id = x.node_address
		left join lateral (
			select array_agg(mentioned.to_kind order by mentioned.to_kind, mentioned.to_id) as kinds,
			       array_agg(mentioned.to_id order by mentioned.to_kind, mentioned.to_id) as ids
			from (`+refs.IdentityMentionsRead(`n.kind`, `n.id`, `$2::text[]`)+`) mentioned
		) held on true
		where x.id = $1::uuid and `+edited.marked+`
	`, event.target, refs.IdentityKinds).Scan(&body, &visibility, &madeBy, &standingKinds, &standingIDs)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read %s %s an older build edited under event %d: %w", edited.item, event.target, event.id, err)
	}
	if !event.truncated {
		body = event.body
	}
	var maker model.Actor
	if err := json.Unmarshal(madeBy, &maker); err != nil {
		return 0, fmt.Errorf("read the maker of %s %s: %w", edited.item, event.target, err)
	}
	// A person's standing mention is held back from the plan, so the plan answers every person
	// the text names, and those of them whom a mention names already are kept below only where
	// they take no part in the thread.
	standing := make([]refs.Target, 0, len(standingIDs))
	var mentionedBefore []string
	for index := range standingIDs {
		if standingKinds[index] == graphstore.KindPerson {
			mentionedBefore = append(mentionedBefore, standingIDs[index])
			continue
		}
		standing = append(standing, refs.Target{Kind: standingKinds[index], ID: standingIDs[index]})
	}
	identities, people := refs.ItemIdentityMentions(edited.item, event.target, visibility, maker, event.at, event.id, body, standing)
	if len(identities)+len(people) == 0 {
		return 0, nil
	}
	rootKind, rootID, err := inboxwrite.ThreadRoot(ctx, tx, string(edited.item), event.target)
	if err != nil {
		return 0, err
	}
	var named []string
	for _, mention := range people {
		if slices.Contains(mentionedBefore, mention.ToID) {
			named = append(named, mention.ToID)
		}
	}
	if len(named) > 0 {
		taking, err := takingPart(ctx, tx, rootKind, rootID, named)
		if err != nil {
			return 0, err
		}
		people = slices.DeleteFunc(people, func(mention graphstore.Tuple) bool {
			return slices.Contains(taking, mention.ToID)
		})
	}
	if len(identities)+len(people) == 0 {
		return 0, nil
	}
	tuples := append(slices.Concat(identities, people),
		inboxwrite.MentionParticipation(rootKind, rootID, graphstore.Uncaused{People: people}, event.actor, event.at, event.id)...)
	if err := writeRelations(ctx, tx, tuples); err != nil {
		return 0, fmt.Errorf("state what an older build's edit of %s %s names: %w", edited.item, event.target, err)
	}
	return int64(len(tuples)), nil
}

// takingPart is each of logins whose participates_in in the thread whose root is rootKind/rootID
// stands now: the newest tuple of each one's relation, kept while it asserts
// (graphstore.CurrentTupleRead), as the record reads a thread's participants (inboxwrite's
// readMembers), here for the few people one edit names rather than for every member. It reads
// graph_tuples_as_of_in, led by the root, whose predicate the far end's `<> 'value'` proves.
func takingPart(ctx context.Context, tx pgx.Tx, rootKind, rootID string, logins []string) ([]string, error) {
	rows, err := tx.Query(ctx, `
		select stated.from_id from (`+graphstore.CurrentTupleRead(`t.from_id`, `t.from_id, t.asserted`,
		`t.to_kind = $1 and t.to_id = $2 and t.kind = '`+graphstore.TupleParticipatesIn+`'
			  and t.from_kind = '`+graphstore.KindPerson+`' and t.from_id = any($3::text[])
			  and t.to_kind <> '`+graphstore.KindValue+`'`)+`) stated
		where stated.asserted
	`, rootKind, rootID, logins)
	if err != nil {
		return nil, fmt.Errorf("read who of %v takes part in %s/%s: %w", logins, rootKind, rootID, err)
	}
	taking, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read who of %v takes part in %s/%s: %w", logins, rootKind, rootID, err)
	}
	return taking, nil
}
